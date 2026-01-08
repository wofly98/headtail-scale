package main

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

// Headscale 监听的本地地址
const targetURL = "http://127.0.0.1:8080"

func handleRequest(w http.ResponseWriter, r *http.Request) {
	// 解析目标地址
	u, _ := url.Parse(targetURL)

	// ---------------------------------------------------------
	// 核心逻辑：Header 还原
	// Cloudflare 发来的是 Upgrade: websocket
	// Headscale 需要的是 Upgrade: tailscale-control-protocol
	// ---------------------------------------------------------
	if r.Header.Get("Upgrade") == "websocket" {
		r.Header.Set("Upgrade", "tailscale-control-protocol")
	}

	// 建立到 Headscale 的 TCP 连接
	destConn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		http.Error(w, "Backend Unavailable", http.StatusBadGateway)
		log.Printf("Error dialing backend: %v", err)
		return
	}
	defer destConn.Close()

	// 劫持客户端连接 (Hijack) 以便进行双向透传
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Server doesn't support hijacking", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// 重写请求并发送给 Headscale
	// 注意：必须重新构造 HTTP 请求行，因为我们现在是在 TCP 层操作
	r.URL.Scheme = "http"
	r.URL.Host = u.Host
	if err := r.Write(destConn); err != nil {
		log.Printf("Error writing to backend: %v", err)
		return
	}

	// 建立双向数据管道
	errChan := make(chan error, 2)
	go func() {
		_, err := io.Copy(destConn, clientConn)
		errChan <- err
	}()
	go func() {
		_, err := io.Copy(clientConn, destConn)
		errChan <- err
	}()

	<-errChan
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8000" // Koyeb 默认端口
	}

	http.HandleFunc("/", handleRequest)
	
	log.Printf("Proxy server started. Listening on 0.0.0.0:%s", port)
	log.Printf("Forwarding targets to %s", targetURL)
	
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}