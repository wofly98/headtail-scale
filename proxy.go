package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"time"
)

const (
	targetHost = "127.0.0.1:8080"
	targetURL  = "http://127.0.0.1:8080"
)

var standardProxy *httputil.ReverseProxy

func init() {
	u, _ := url.Parse(targetURL)
	standardProxy = httputil.NewSingleHostReverseProxy(u)
	d := standardProxy.Director
	standardProxy.Director = func(req *http.Request) {
		d(req)
		req.Host = "127.0.0.1:8080"
	}
	standardProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("[PROXY_ERR] %v", err)
		http.Error(w, "Proxy Error", http.StatusBadGateway)
	}
}

// 处理 HTTP POST 隧道 (还原为 Upgrade 请求)
func handlePostTunnel(w http.ResponseWriter, r *http.Request) {
	// 从 URL 参数还原 Handshake 数据
	tsHandshake := r.URL.Query().Get("ts_handshake")
	
	log.Printf("[TUNNEL] Starting Tunnel via POST. HandshakeLen=%d", len(tsHandshake))
	
	if tsHandshake == "" {
		http.Error(w, "Missing Handshake", http.StatusBadRequest)
		return
	}

	// 1. 建立到 Headscale 的 TCP 连接
	destConn, err := net.DialTimeout("tcp", targetHost, 5*time.Second)
	if err != nil {
		log.Printf("[TUNNEL_FAIL] Dial Backend failed: %v", err)
		http.Error(w, "Backend Unavailable", http.StatusBadGateway)
		return
	}
	defer destConn.Close()

	// 2. 【关键】重构原始请求报文
	// 即使 Worker 发来的是普通 POST，我们也必须向 Headscale 发送 Upgrade 请求
	// 同时将 X-Tailscale-Handshake 头还原
	reqToBackend := fmt.Sprintf(
		"POST /ts2021 HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Upgrade: tailscale-control-protocol\r\n"+
		"Connection: Upgrade\r\n"+
		"X-Tailscale-Handshake: %s\r\n"+
		"\r\n", 
		targetHost, tsHandshake)

	// 发送 Header
	if _, err := destConn.Write([]byte(reqToBackend)); err != nil {
		log.Printf("[TUNNEL_FAIL] Write Header failed: %v", err)
		return
	}

	// 3. 【关键】透传 Body (Noise 握手数据)
	// Cloudflare 发来的 Body 包含了客户端生成的 Noise 协议数据包
	// 我们必须把它写进 TCP 连接，否则 Headscale 无法解密
	go func() {
		if _, err := io.Copy(destConn, r.Body); err != nil {
			log.Printf("[TUNNEL] Copy Body to Backend error: %v", err)
		}
	}()

	// 4. 处理 Headscale 的响应
	// Headscale 会返回 HTTP 101，我们需要读取它但不要发给 Worker
	// 因为 Worker 期望的是 200 OK 的数据流
	br := bufio.NewReader(destConn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			log.Printf("[TUNNEL_FAIL] Read Backend Header failed: %v", err)
			return 
		}
		// 遇到空行表示 Header 结束
		if line == "\r\n" {
			break
		}
	}
	
	log.Printf("[TUNNEL] Headscale handshake accepted. Streaming data...")

	// 5. 设置响应头并开启流式传输
	// 返回 200 OK 给 Worker，并禁用缓存
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Accel-Buffering", "no") // 禁用 Nginx 缓冲(如果由的话)
	w.WriteHeader(http.StatusOK)
	
	// 立即刷新 Header，确保 Worker 收到 200
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// 6. 将 Headscale 的 TCP 数据流写回给 HTTP Response
	// 这里使用的是 br.WriteTo，因为它包含缓冲中已读取的部分 + 后续数据
	_, err = br.WriteTo(w)
	if err != nil {
		log.Printf("[TUNNEL_END] Stream closed: %v", err)
	}
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	// 识别 Worker 发来的自定义头
	isTunnelMode := r.Header.Get("X-Tunnel-Mode") == "true"
	
	log.Printf("[REQ] Method=%s URL=%s Tunnel=%v", r.Method, r.URL.Path, isTunnelMode)

	if isTunnelMode {
		handlePostTunnel(w, r)
	} else {
		standardProxy.ServeHTTP(w, r)
	}
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}
	server := &http.Server{
		Addr:    ":" + port,
		Handler: http.HandlerFunc(handleRequest),
		// 增加超时，保证长连接不中断
		IdleTimeout: 300 * time.Second,
	}
	log.Printf("Strict-Proxy listening on :%s", port)
	log.Fatal(server.ListenAndServe())
}