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

	// --- 1. 请求头还原 (Worker -> Headscale) ---
	r.Header.Set("Connection", "Upgrade")
	
	if r.Header.Get("Upgrade") == "websocket" {
		r.Header.Set("Upgrade", "tailscale-control-protocol")
		
		// 【关键修复】从 Sec-WebSocket-Protocol 提取 handshake 数据
		// Worker 把 X-Tailscale-Handshake 藏在了这里
		handshake := r.Header.Get("Sec-WebSocket-Protocol")
		if handshake != "" {
			// 还原回 Headscale 需要的 Header
			r.Header.Set("X-Tailscale-Handshake", handshake)
			// 清理掉伪装的 Protocol 头，以免 Headscale 误判为 WebSocket 模式
			r.Header.Del("Sec-WebSocket-Protocol")
		}
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

	// --- 4. 写入修改后的请求头 ---
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

			// 强制确保返回给 Worker 的是 websocket，骗过 Worker 的 fetch 检查
			if strings.HasPrefix(line, "Upgrade:") {
				line = "Upgrade: websocket\r\n"
			}
            
            // 为了防止 Worker 或 Koyeb 对响应头进行严格校验
            // 我们可以在这里也伪造一个 Sec-WebSocket-Protocol 响应
            // 但通常只要 Upgrade 对了就行，这里保持简单

			if _, err := clientConn.Write([]byte(line)); err != nil {
				errChan <- err
				return
			}

			if line == "\r\n" {
				break
			}
		}

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
	log.Printf("Proxy (Header Smuggling Fixed) listening on :%s", port)
	log.Fatal(server.ListenAndServe())
}