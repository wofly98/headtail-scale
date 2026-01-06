#!/bin/bash

TS_HOSTNAME="koyeb-server"

# 自动检测 Koyeb 公网域名
if [ -n "$KOYEB_PUBLIC_DOMAIN" ]; then
    export HEADSCALE_SERVER_URL="https://${KOYEB_PUBLIC_DOMAIN}"
    log "检测到 Koyeb 域名: $HEADSCALE_SERVER_URL"
elif [ -n "$HEADSCALE_SERVER_URL" ]; then
    log "使用环境变量 server_url: $HEADSCALE_SERVER_URL"
else
    export HEADSCALE_SERVER_URL="http://localhost:8080"
    warn "未检测到公网域名，使用本地地址 (仅测试用)"
fi

if [ -n "$LOCAL_LOGIN_URL" ]; then
    log "使用环境变量 login_url: $LOCAL_LOGIN_URL"
else
    export LOCAL_LOGIN_URL="http://localhost:8080"
    warn "未检测到公网域名，使用本地地址 (仅测试用)"
fi

# 开启错误追踪，便于调试
set -e

# --- 第一步：配置 Headscale ---
# 如果没有配置文件，创建一个基础的（或者你可以映射一个）
if [ ! -f /etc/headscale/config.yaml ]; then
    echo "正在生成基础 Headscale 配置..."
    mkdir -p /etc/headscale
    # 注意：这里需要根据 Koyeb 的环境变量动态修改 server_url
    # 假设 Koyeb 提供了公开域名，通常我们需要手动设置一个环境变量 HEADSCALE_SERVER_URL
    cat <<EOF > /etc/headscale/config.yaml
server_url: ${HEADSCALE_SERVER_URL:-http://127.0.0.1:8080}
listen_addr: 0.0.0.0:8080
metrics_listen_addr: 127.0.0.1:9090
grpc_listen_addr: 0.0.0.0:50443
grpc_allow_insecure: false
private_key_path: /var/lib/headscale/private.key
noise:
  private_key_path: /var/lib/headscale/noise_private.key
ip_prefixes:
  - fd7a:115c:a1e0::/48
  - 100.64.0.0/10
database:
  type: sqlite3
  sqlite:
    path: /var/lib/headscale/db.sqlite
dns_config:
  nameservers:
    - 1.1.1.1
EOF
fi

# 初始化数据库（如果是第一次运行）
touch /var/lib/headscale/db.sqlite

# 启动 Headscale (放入后台运行)
echo "启动 Headscale 服务..."
headscale serve &
HEADSCALE_PID=$!

# --- 第二步：等待 Headscale 就绪 ---
echo "等待 Headscale 启动..."
until curl -s http://127.0.0.1:8080/health > /dev/null; do
    sleep 1
    echo "..."
done
echo "Headscale 已就绪！"

# 1. 确保用户存在
headscale users create default 2>/dev/null || true

# 2. 获取用户 ID (这是最稳的方法，避免 name/id 混淆)
# headscale users list 输出通常包含 ID 列。这里用 json 提取最准确。
USER_ID=$(headscale users list -o json | jq -r '.[] | select(.name=="default") | .id')

if [ -z "$USER_ID" ] || [ "$USER_ID" = "null" ]; then
    echo "错误: 无法获取 default 用户的 ID"
    # 如果获取不到 ID，尝试直接用名字 (兼容旧版)
    USER_ID="default"
fi

echo "使用用户 ID: $USER_ID 生成密钥..."


# --- 第三步：生成预认证密钥 (使用修正后的命令) ---
echo "生成 AuthKey..."
# 注意：这里使用了之前修正后的 grep 逻辑
AUTHKEY=$(headscale preauthkeys create --user "$USER_ID" --reusable --expiration 365d -o json | jq -r .key)

if [ -z "$AUTHKEY" ]; then
    echo "错误：无法生成 AuthKey"
    exit 1
fi
echo "获取到 Key: ${AUTHKEY}" 

# --- 第四步：启动 Tailscale 并连接 ---
# 启动 tailscaled 守护进程 (需要特权模式或 TUN 设备支持)
echo "启动 Tailscaled..."
tailscaled --state=/var/lib/tailscale/tailscaled.state --tun=userspace-networking &
# 注意：Koyeb 默认可能没有 /dev/net/tun，这里使用了 --tun=userspace-networking 模式以保证兼容性
# 如果你确定平台支持 TUN，可以去掉 --tun 参数

sleep 3

echo "执行 Tailscale Up..."
# 这里我们要连接的是 localhost 的 headscale，因为它们在同一个容器里
# 但要注意：login-server 必须填 Headscale 配置文件里 server_url 的地址
# 如果 server_url 是公网域名，这里最好也填公网域名，或者通过 hosts 欺骗
tailscale up --login-server=$LOCAL_LOGIN_URL --authkey=$AUTHKEY --hostname=$TS_HOSTNAME --advertise-exit-node=true

echo "Tailscale 已连接！服务运行中..."

# === 步骤 5: 自动批准 Exit Node 路由 (后台运行) ===
(
    echo "开始监听并批准路由..."
    for i in $(seq 1 15); do
        sleep 5
        
        # [核心修正] 使用你提供的命令和 JSON 结构进行精准提取
        # 查找 name 为 "koyeb-server" 的节点 ID
        NODE_ID=$(headscale nodes list-routes -o json | jq -r ".[] | select(.name==\"$TS_HOSTNAME\") | .id")
        
        if [ -n "$NODE_ID" ] && [ "$NODE_ID" != "null" ]; then
            echo "发现节点 ID: $NODE_ID (Name: $TS_HOSTNAME)"
            
            # 批准路由 (注意：新版使用 identifier + routes)
            headscale nodes approve-routes --identifier "$NODE_ID" --routes "0.0.0.0/0,::/0" 2>/dev/null
            
            # 验证结果
            CHECK_OUTPUT=$(headscale nodes list-routes)
            if echo "$CHECK_OUTPUT" | grep -q "$TS_HOSTNAME" && echo "$CHECK_OUTPUT" | grep -q "0.0.0.0/0"; then
                 echo "SUCCESS: 路由已成功批准！"
                 echo "$CHECK_OUTPUT"
                 break
            fi
        else
            echo "等待节点注册... ($i/15)"
        fi
    done
) &


# === 步骤 6: 保持容器运行 ===
echo "系统: 所有服务已启动，进入守护模式..."
wait $HEADSCALE_PID
