#!/bin/bash

TS_HOSTNAME="koyeb-server"

# 自动检测 Koyeb 公网域名
if [ -n "$KOYEB_PUBLIC_DOMAIN" ]; then
    export HEADSCALE_SERVER_URL="https://${KOYEB_PUBLIC_DOMAIN}"
    log "检测到 Koyeb 域名: $HEADSCALE_SERVER_URL"
elif [ -n "$HEADSCALE_SERVER_URL" ]; then
    log "使用环境变量 server_url: $HEADSCALE_SERVER_URL"
else
    export HEADSCALE_SERVER_URL="http://localhost:8081"
    warn "未检测到公网域名，使用本地地址 (仅测试用)"
fi

if [ -n "$LOCAL_LOGIN_URL" ]; then
    log "使用环境变量 login_url: $LOCAL_LOGIN_URL"
else
    export LOCAL_LOGIN_URL="http://localhost:8081"
    warn "未检测到公网域名，使用本地地址 (仅测试用)"
fi

# 开启错误追踪，便于调试
set -e

# === 生成全封装还原代理 (proxy.js) ===
cat <<EOF > /proxy.js
const http = require('http');
const net = require('net');

const TARGET_PORT = 8081;
const TARGET_HOST = '127.0.0.1';

function log(msg) {
    console.log(\`[\${new Date().toISOString()}] \${msg}\`);
}

const server = http.createServer((clientReq, clientRes) => {
    const reqId = Math.random().toString(36).substring(7);

    // 1. 解包元数据 (还原原始 Header)
    let realHeaders = {};
    try {
        if (clientReq.headers['x-hidden-meta']) {
            realHeaders = JSON.parse(clientReq.headers['x-hidden-meta']);
        } else {
            // 如果没有封装头，说明是直连或异常，尝试使用现有头
            realHeaders = { ...clientReq.headers };
        }
    } catch (e) {
        log(\`[req:\${reqId}] Failed to parse hidden meta\`);
        realHeaders = { ...clientReq.headers };
    }

    const isWebSocket = realHeaders['upgrade'] === 'websocket';
    const isBase64Body = clientReq.headers['x-body-encoding'] === 'base64';

    // === 场景 A: 还原 WebSocket 握手 (/ts2021) ===
    if (isWebSocket) {
        log(\`[req:\${reqId}] TYPE: WebSocket (Restored from Meta)\`);
        
        // 使用 Net 模块直接建立 TCP 连接，绕过 HTTP 协议限制
        const proxySocket = net.connect(TARGET_PORT, TARGET_HOST, () => {
            // 手动构建 HTTP 报文 (因为 Node http 模块不支持在非 Upgrade 请求里发起 Upgrade)
            let rawReq = \`\${clientReq.method} \${clientReq.url} HTTP/1.1\r\n\`;
            
            // 写入还原后的 Header
            Object.keys(realHeaders).forEach(key => {
                // 排除掉可能冲突的头，Host 强制修正
                if (key !== 'host') {
                    rawReq += \`\${key}: \${realHeaders[key]}\r\n\`;
                }
            });
            rawReq += \`Host: 127.0.0.1:\${TARGET_PORT}\r\n\`;
            rawReq += '\r\n'; // 头部结束

            proxySocket.write(rawReq);
        });

        // 管道对接：Headscale <-> Koyeb/Worker
        proxySocket.pipe(clientRes.socket || clientRes);
        clientReq.pipe(proxySocket);

        proxySocket.on('error', (e) => {
            log(\`[req:\${reqId}] WS Socket Error: \${e.message}\`);
            clientRes.end();
        });
        
        // 这里的 return 非常关键，接管 socket 后退出 http 处理流程
        return; 
    }

    // === 场景 B: 还原普通 HTTP 请求 (Register / Key) ===
    log(\`[req:\${reqId}] TYPE: Standard HTTP\`);
    
    const options = {
        hostname: TARGET_HOST,
        port: TARGET_PORT,
        path: clientReq.url,
        method: clientReq.method,
        headers: realHeaders // 使用还原后的 Header
    };

    // 清理掉 node 自动加的 connection 头，防止 conflict
    delete options.headers['connection'];
    delete options.headers['host'];
    // 重新计算 content-length (因为 Base64 解码后长度变了)
    delete options.headers['content-length'];

    const proxyReq = http.request(options, (proxyRes) => {
        clientRes.writeHead(proxyRes.statusCode, proxyRes.headers);
        proxyRes.pipe(clientRes, { end: true });
    });

    proxyReq.on('error', (e) => {
        log(\`[req:\${reqId}] Proxy Error: \${e.message}\`);
        clientRes.writeHead(502);
        clientRes.end();
    });

    // === Body 处理：解码 Base64 并转发 ===
    if (isBase64Body) {
        let bodyData = '';
        clientReq.setEncoding('utf8');
        clientReq.on('data', chunk => bodyData += chunk);
        clientReq.on('end', () => {
            try {
                if (bodyData.length > 0) {
                    const decodedBuffer = Buffer.from(bodyData, 'base64');
                    proxyReq.write(decodedBuffer);
                    log(\`[req:\${reqId}] Decoded Body: \${decodedBuffer.length} bytes\`);
                }
                proxyReq.end();
            } catch (e) {
                log(\`[req:\${reqId}] Decode Error\`);
                clientRes.writeHead(400);
                clientRes.end();
            }
        });
    } else {
        // 如果 Worker 没有 Base64 编码（比如 GET 请求），直接透传
        clientReq.pipe(proxyReq, { end: true });
    }
});

server.listen(8080, () => {
    log('>>> Universal Unpacker Proxy ready on 8080');
});
EOF

# 初始化数据库（如果是第一次运行）
touch /var/lib/headscale/db.sqlite

# 启动 Headscale (放入后台运行)
echo "启动 Headscale 服务..."
headscale serve &
HEADSCALE_PID=$!

# --- 第二步：等待 Headscale 就绪 ---
echo "等待 Headscale 启动..."
until curl -s http://127.0.0.1:8081/health > /dev/null; do
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
echo "Starting Node.js Proxy..."
node /proxy.js
#wait $HEADSCALE_PID
