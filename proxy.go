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

	// --- 1. 请求头伪装 (Worker -> Headscale) ---
	r.Header.Set("Connection", "Upgrade")
	if r.Header.Get("Upgrade") == "websocket" {
		r.Header.Set("Upgrade", "tailscale-control-protocol")
	}
	r.Host = u.Host
	r.Header.Set("Host", u.Host)

	// --- 2. 连接 Headscale ---
	destConn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		http.Error(w, "Backend Unavailable", http.StatusBadGateway)
		return
	}
	defer destConn.Close()

	// --- 3. 劫持客户端连接 ---
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "No hijack", http.StatusInternalServerError)
		return
	}
	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// --- 4. 发送请求给 Headscale ---
	if err := r.Write(destConn); err != nil {
		return
	}

	// 补发缓冲区数据 (修复上一轮的丢包问题)
	if clientBuf.Reader.Buffered() > 0 {
		io.Copy(destConn, clientBuf)
	}

	// --- 5. 响应头伪装 (Headscale -> Worker) [新增核心修复] ---
	// 我们不能直接 io.Copy 响应，因为必须修改 Upgrade 头
	
	errChan := make(chan error, 2)

	go func() {
		// 使用 bufio 读取 Headscale 的响应头，逐行处理
		remoteReader := bufio.NewReader(destConn)
		for {
			line, err := remoteReader.ReadString('\n')
			if err != nil {
				errChan <- err
				return
			}

			// 检查并替换 Upgrade 头
			// Headscale 发送: Upgrade: tailscale-control-protocol
			// 我们需要改为:   Upgrade: websocket
			if strings.HasPrefix(line, "Upgrade: tailscale-control-protocol") {
				line = "Upgrade: websocket\r\n"
			}

			// 写入修改后的行给客户端 (Worker)
			if _, err := clientConn.Write([]byte(line)); err != nil {
				errChan <- err
				return
			}

			// 空行表示 Header 结束，Body (数据流) 开始
			if line == "\r\n" {
				break
			}
		}

		// Header 处理完后，直接透传剩余的数据流 (Noise 协议数据)
		_, err := io.Copy(clientConn, remoteReader)
		errChan <- err
	}()

	// --- 6. 客户端数据透传 (Client -> Headscale) ---
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
	// 增加超时设置，避免连接卡死
	server := &http.Server{
		Addr:        ":" + port,
		Handler:     http.HandlerFunc(handleRequest),
		IdleTimeout: 120 * time.Second,
	}
	log.Printf("Proxy listening on :%s", port)
	log.Fatal(server.ListenAndServe())
}