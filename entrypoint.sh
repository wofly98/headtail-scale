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

# === 生成“法医级”诊断代理 ===
cat <<EOF > /proxy.js
const http = require('http');
const net = require('net');

const TARGET_PORT = 8081;
const TARGET_HOST = '127.0.0.1';

// 辅助函数：安全打印日志，避免二进制乱码炸屏
function log(tag, msg) {
    console.log(\`[\${new Date().toISOString()}] [\${tag}] \${msg}\`);
}

const server = http.createServer((clientReq, clientRes) => {
    const reqId = Math.random().toString(36).substring(7);
    const mode = clientReq.headers['x-proxy-mode'];

    // === 场景 A: 协议升级 (重点监控对象) ===
    if (mode === 'upgrade') {
        const upgradeProto = clientReq.headers['x-upgrade-proto'] || 'websocket';
        log(reqId, \`MODE: Manual Upgrade (\${upgradeProto})\`);

        // 1. 连接 Headscale
        const headscaleSocket = net.connect(TARGET_PORT, TARGET_HOST, () => {
            log(reqId, 'Connected to Headscale TCP');

            // 2. 构造强制 GET 请求
            let rawReq = \`GET /ts2021 HTTP/1.1\r\n\`;
            rawReq += \`Host: 127.0.0.1:\${TARGET_PORT}\r\n\`;
            rawReq += \`Connection: Upgrade\r\n\`;
            rawReq += \`Upgrade: \${upgradeProto}\r\n\`;

            // 转发关键头
            const headersToForward = [
                'sec-websocket-key',
                'sec-websocket-protocol',
                'sec-websocket-version',
                'x-tailscale-handshake',
                'authorization',
                'user-agent'
            ];

            headersToForward.forEach(key => {
                if (clientReq.headers[key]) {
                    rawReq += \`\${key}: \${clientReq.headers[key]}\r\n\`;
                }
            });

            // 补全 Key
            if (upgradeProto === 'websocket' && !clientReq.headers['sec-websocket-key']) {
                 rawReq += \`Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\`;
            }

            rawReq += \`\r\n\`; 
            
            // 发送并记录
            headscaleSocket.write(rawReq);
            log(reqId, \`Sent Handshake Request (Headers size: \${rawReq.length} bytes)\`);
        });

        // 3. 监控 Headscale -> Client 数据流
        let firstChunk = true;
        headscaleSocket.on('data', (chunk) => {
            if (firstChunk) {
                const respStr = chunk.toString();
                const firstLine = respStr.split('\r\n')[0];
                log(reqId, \`<<< Headscale Response Header: \${firstLine}\`);
                log(reqId, \`<<< Headscale Chunk Size: \${chunk.length} bytes\`);
                
                if (!firstLine.includes('101')) {
                     log(reqId, \`!!! CRITICAL: Handshake Failed. Full Body:\n\${respStr}\`);
                }
                firstChunk = false;
            } else {
                // 后续数据只打印大小，不打印内容
                log(reqId, \`<<< Headscale sent data: \${chunk.length} bytes\`);
            }
            
            // 转发
            const flushed = clientReq.socket.write(chunk);
            if (!flushed) {
                log(reqId, 'WARN: Client socket buffer full, backpressure active');
            }
        });

        // 4. 监控 Client -> Headscale 数据流
        clientReq.socket.on('data', (chunk) => {
            log(reqId, \`>>> Client sent data: \${chunk.length} bytes\`);
            headscaleSocket.write(chunk);
        });

        // 5. 监控生死状态 (这是破案的关键)
        
        // Headscale 断开
        headscaleSocket.on('end', () => log(reqId, 'FIN: Headscale sent FIN (Connection closing)'));
        headscaleSocket.on('close', (hadError) => log(reqId, \`CLOSE: Headscale socket closed. Error: \${hadError}\`));
        headscaleSocket.on('error', (e) => log(reqId, \`ERR: Headscale socket error: \${e.message}\`));

        // Client 断开
        clientReq.socket.on('end', () => log(reqId, 'FIN: Client sent FIN (Connection closing)'));
        clientReq.socket.on('close', (hadError) => log(reqId, \`CLOSE: Client socket closed. Error: \${hadError}\`));
        clientReq.socket.on('error', (e) => log(reqId, \`ERR: Client socket error: \${e.message}\`));
        
        // 防止 Node.js HTTP Server 超时销毁 Socket
        clientReq.socket.setTimeout(0); 

        return; 
    }

    // === 场景 B: Base64 (保持简单日志) ===
    if (mode === 'base64') {
        const options = {
            hostname: TARGET_HOST, port: TARGET_PORT, path: clientReq.url, method: clientReq.method,
            headers: { ...clientReq.headers }
        };
        delete options.headers['x-proxy-mode']; delete options.headers['x-upgrade-proto'];
        delete options.headers['content-type']; delete options.headers['content-length']; delete options.headers['host'];

        const proxyReq = http.request(options, (proxyRes) => {
            log(reqId, \`Base64 Response: \${proxyRes.statusCode}\`);
            clientRes.writeHead(proxyRes.statusCode, proxyRes.headers);
            proxyRes.pipe(clientRes, { end: true });
        });
        
        let bodyData = '';
        clientReq.setEncoding('utf8');
        clientReq.on('data', chunk => bodyData += chunk);
        clientReq.on('end', () => {
            try {
                const decodedBuffer = Buffer.from(bodyData, 'base64');
                proxyReq.write(decodedBuffer);
                proxyReq.end();
            } catch (e) {
                clientRes.writeHead(400); clientRes.end();
            }
        });
        return;
    }

    // === 场景 C: 普通透传 ===
    const options = {
        hostname: TARGET_HOST, port: TARGET_PORT, path: clientReq.url, method: clientReq.method,
        headers: { ...clientReq.headers }
    };
    delete options.headers['host'];
    const proxyReq = http.request(options, (proxyRes) => {
        log(reqId, \`Standard Response: \${proxyRes.statusCode}\`);
        clientRes.writeHead(proxyRes.statusCode, proxyRes.headers);
        proxyRes.pipe(clientRes, { end: true });
    });
    clientReq.pipe(proxyReq, { end: true });
});

server.listen(8080, () => { 
    console.log('>>> Node.js FORENSIC Proxy Ready'); 
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
node /proxy.js >/tmp/proxy.log 2>&1 &
wait $HEADSCALE_PID
