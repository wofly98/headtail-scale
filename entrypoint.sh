#!/bin/bash

TS_HOSTNAME="koyeb-server"

# 自动检测 Koyeb 公网域名
if [ -n "$KOYEB_PUBLIC_DOMAIN" ]; then
    export HEADSCALE_SERVER_URL="https://${KOYEB_PUBLIC_DOMAIN}"
    echo "Detected Koyeb domain: $HEADSCALE_SERVER_URL"
elif [ -n "$HEADSCALE_SERVER_URL" ]; then
    echo "Using environment variable server_url: $HEADSCALE_SERVER_URL"
else
    export HEADSCALE_SERVER_URL="http://localhost:8180"
    echo "Public domain not detected, using local address (test only)"
fi

if [ -n "$LOCAL_LOGIN_URL" ]; then
    echo "Using environment variable login_url: $LOCAL_LOGIN_URL"
else
    export LOCAL_LOGIN_URL="http://localhost:8180"
    echo "Public domain not detected, using local address (test only)"
fi

# 开启错误追踪，便于调试
set -e

# 配置数据库
if [ -n "$HEADSCALE_DB_TYPE" ]; then
    echo "Configuring database type to $HEADSCALE_DB_TYPE from environment variables..."
    awk -v type="$HEADSCALE_DB_TYPE" \
        -v host="$HEADSCALE_DB_HOST" \
        -v port="$HEADSCALE_DB_PORT" \
        -v name="$HEADSCALE_DB_NAME" \
        -v user="$HEADSCALE_DB_USER" \
        -v pass="$HEADSCALE_DB_PASS" \
        -v ssl="$HEADSCALE_DB_SSL_MODE" '
    /^database:/ {
        in_db = 1
        print "database:"
        if (type == "postgres") {
            print "  type: postgres"
            print "  postgres:"
            print "    host: " (host ? host : "localhost")
            print "    port: " (port ? port : "5432")
            print "    name: " (name ? name : "headscale")
            print "    user: " (user ? user : "headscale")
            print "    pass: " (pass ? pass : "headscale")
            print "    ssl_mode: " (ssl ? ssl : "disable")
        } else {
            print "  type: " type
            print "  sqlite:"
            print "    path: /var/lib/headscale/db.sqlite"
        }
        next
    }
    in_db && (/^[a-zA-Z0-9_-]+:/ || /^#/) { in_db = 0 }
    in_db { next }
    { print }
    ' /etc/headscale/config.yaml > /tmp/config.yaml && mv /tmp/config.yaml /etc/headscale/config.yaml
fi

# 初始化数据库文件（如果当前配置为 sqlite）
if grep -q "type: sqlite" /etc/headscale/config.yaml; then
    touch /var/lib/headscale/db.sqlite
fi

# 启动 Headscale (放入后台运行)
echo "Starting Headscale service..."
headscale serve &
HEADSCALE_PID=$!

# --- 第二步：等待 Headscale 就绪 ---
echo "Waiting for Headscale to start..."
until curl -s http://127.0.0.1:8180/health > /dev/null; do
    sleep 1
    echo "..."
done
echo "Headscale is ready!"

# 1. 确保用户存在
headscale users create default 2>/dev/null || true

# 2. 获取用户 ID (这是最稳的方法，避免 name/id 混淆)
# headscale users list 输出通常包含 ID 列。这里用 json 提取最准确。
USER_ID=$(headscale users list -o json | jq -r '.[] | select(.name=="default") | .id')

if [ -z "$USER_ID" ] || [ "$USER_ID" = "null" ]; then
    echo "Error: Unable to get ID for user 'default'"
    # 如果获取不到 ID，尝试直接用名字 (兼容旧版)
    USER_ID="default"
fi

echo "Generating key using user ID: $USER_ID..."


# --- 第三步：生成预认证密钥 (使用修正后的命令) ---
echo "Generating AuthKey..."
# 注意：这里使用了之前修正后的 grep 逻辑
AUTHKEY=$(headscale preauthkeys create --user "$USER_ID" --reusable --expiration 365d -o json | jq -r .key)

if [ -z "$AUTHKEY" ]; then
    echo "Error: Unable to generate AuthKey"
    exit 1
fi
echo "Obtained Key: ${AUTHKEY}" 
echo ${AUTHKEY}>/authkey

# --- 第四步：启动 Tailscale 并连接 ---
# 启动 tailscaled 守护进程 (需要特权模式或 TUN 设备支持)
echo "Starting Tailscaled..."
tailscaled --state=/var/lib/tailscale/tailscaled.state --tun=userspace-networking &
# 注意：Koyeb 默认可能没有 /dev/net/tun，这里使用了 --tun=userspace-networking 模式以保证兼容性
# 如果你确定平台支持 TUN，可以去掉 --tun 参数

sleep 3

echo "Executing Tailscale Up..."
# 这里我们要连接的是 localhost 的 headscale，因为它们在同一个容器里
# 但要注意：login-server 必须填 Headscale 配置文件里 server_url 的地址
# 如果 server_url 是公网域名，这里最好也填公网域名，或者通过 hosts 欺骗
tailscale up --login-server=$LOCAL_LOGIN_URL --authkey=$AUTHKEY --hostname=$TS_HOSTNAME --advertise-exit-node=true

echo "Tailscale connected! Service is running..."

# === 步骤 5: 自动批准 Exit Node 路由 (后台运行) ===
(
    echo "Starting to listen and approve routes..."
    for i in $(seq 1 15); do
        sleep 5
        
        # [核心修正] 使用你提供的命令和 JSON 结构进行精准提取
        # 查找 name 为 "koyeb-server" 的节点 ID
        NODE_ID=$(headscale nodes list-routes -o json | jq -r ".[] | select(.name==\"$TS_HOSTNAME\") | .id")
        
        if [ -n "$NODE_ID" ] && [ "$NODE_ID" != "null" ]; then
            echo "Found node ID: $NODE_ID (Name: $TS_HOSTNAME)"
            
            # 批准路由 (注意：新版使用 identifier + routes)
            headscale nodes approve-routes --identifier "$NODE_ID" --routes "0.0.0.0/0,::/0" 2>/dev/null
            
            # 验证结果
            CHECK_OUTPUT=$(headscale nodes list-routes)
            if echo "$CHECK_OUTPUT" | grep -q "$TS_HOSTNAME" && echo "$CHECK_OUTPUT" | grep -q "0.0.0.0/0"; then
                 echo "SUCCESS: Routes approved successfully!"
                 echo "$CHECK_OUTPUT"
                 break
            fi
        else
            echo "Waiting for node registration... ($i/15)"
        fi
    done
) &


# === 步骤 6: 保持容器运行 ===
echo "System: All services started, entering daemon mode..."
#gost -C /etc/gost/config.yaml
exec /usr/local/bin/header-proxy
wait $HEADSCALE_PID
