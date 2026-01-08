package main

import (
	"bufio"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const targetURL = "http://127.0.0.1:8080"

func handleRequest(w http.ResponseWriter, r *http.Request) {
	u, _ := url.Parse(targetURL)

	// --- 1. 请求头深度伪装 (Worker -> Headscale) ---
	// 确保 Connection 为 Upgrade
	r.Header.Set("Connection", "Upgrade")
	
	// 如果是 Worker 发来的 Websocket 伪装流量
	if r.Header.Get("Upgrade") == "websocket" {
		// 【关键修复 1】: 强制指定子协议，满足 Headscale 的 WebSocket 校验逻辑
		r.Header.Set("Sec-WebSocket-Protocol", "tailscale-control-protocol")
		
		// 保持 Upgrade 为 websocket，让 Headscale 走 WebSocket 处理流程 (路径 B)
		// 这样兼容性更好，因为 Cloudflare 和 Koyeb 对 websocket 支持最完善
		r.Header.Set("Upgrade", "websocket")
	} else {
        // 如果是其他情况，强制修正为 tailscale 协议（兜底）
        r.Header.Set("Upgrade", "tailscale-control-protocol")
    }

	r.Host = u.Host
	r.Header.Set("Host", u.Host)

	// --- 2. 连接后端 ---
	destConn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		http.Error(w, "Backend Unavailable", http.StatusBadGateway)
		log.Printf("Dial failed: %v", err)
		return
	}
	defer destConn.Close()

	// --- 3. 劫持客户端连接 ---
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "No hijack support", http.StatusInternalServerError)
		return
	}
	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// --- 4. 写入请求头给 Headscale ---
	if err := r.Write(destConn); err != nil {
		log.Printf("Write request failed: %v", err)
		return
	}

	// 补发缓冲区数据
	if clientBuf.Reader.Buffered() > 0 {
		io.Copy(destConn, clientBuf)
	}

	// --- 5. 响应头伪装 (Headscale -> Worker) ---
	errChan := make(chan error, 2)

	go func() {
		remoteReader := bufio.NewReader(destConn)
		for {
			line, err := remoteReader.ReadString('\n')
			if err != nil {
				errChan <- err
				return
			}

			// 【关键修复 2】: 响应头清洗
			// Headscale 回复: Sec-WebSocket-Protocol: tailscale-control-protocol
			// Worker 可能不认这个头，或者我们需要隐藏它
			// 但最重要的是确保 Upgrade: websocket 被正确返回
			if strings.HasPrefix(line, "Upgrade:") {
				// 强制确保返回给 Koyeb/Worker 的是 websocket
				line = "Upgrade: websocket\r\n"
			}
            
            // 可选：如果不需要把子协议暴露给外网，可以过滤掉 Sec-WebSocket-Protocol 响应头
            // 但为了保证握手严谨性，保留它通常没问题，只要 Upgrade 对了就行

			if _, err := clientConn.Write([]byte(line)); err != nil {
				errChan <- err
				return
			}

			if line == "\r\n" {
				break
			}
		}

		// 透传剩余数据流
		_, err := io.Copy(clientConn, remoteReader)
		errChan <- err
	}()

	go func() {
		_, err := io.Copy(destConn, clientConn)
		errChan <- err
	}()

	<-errChan
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}
	server := &http.Server{
		Addr:        ":" + port,
		Handler:     http.HandlerFunc(handleRequest),
		IdleTimeout: 120 * time.Second,
	}
	log.Printf("Proxy listening on :%s", port)
	log.Fatal(server.ListenAndServe())
}