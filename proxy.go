package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	targetHost    = "127.0.0.1:8080" // TCP 连接地址
	targetHttpURL = "http://127.0.0.1:8080" // HTTP 代理地址
	wsGUID        = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

var standardProxy *httputil.ReverseProxy

func init() {
	u, _ := url.Parse(targetHttpURL)
	standardProxy = httputil.NewSingleHostReverseProxy(u)
	// 修正 Host 头，否则 Headscale 可能拒绝
	d := standardProxy.Director
	standardProxy.Director = func(req *http.Request) {
		d(req)
		req.Host = "127.0.0.1:8080"
	}
}

// 算法：计算 WebSocket 握手校验码
func computeAcceptKey(challengeKey string) string {
	h := sha1.New()
	io.WriteString(h, challengeKey)
	io.WriteString(h, wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// 核心逻辑：协议转换隧道
func handleTunnel(w http.ResponseWriter, r *http.Request) {
	// 1. 获取必要的握手参数
	wsKey := r.Header.Get("Sec-WebSocket-Key")
	tsHandshake := r.URL.Query().Get("ts_handshake")

	// 如果参数不全，说明协议链断了，拒绝连接
	if wsKey == "" || tsHandshake == "" {
		http.Error(w, "Protocol Mismatch: Missing Handshake Data", http.StatusBadRequest)
		return
	}

	// 2. 劫持 TCP 连接 (Hijack)
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Server Hijack Not Supported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// 3. 连接 Headscale (TCP Dial)
	destConn, err := net.DialTimeout("tcp", targetHost, 5*time.Second)
	if err != nil {
		log.Printf("Dial Backend Failed: %v", err)
		return
	}
	defer destConn.Close()

	// --- 阶段 A: 完成上游 (Worker) 的 WebSocket 握手 ---
	// 必须回复标准的 WebSocket 101，否则 Cloudflare 会切断连接
	acceptKey := computeAcceptKey(wsKey)
	respToWorker := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey + "\r\n" +
		"\r\n"
	
	if _, err := clientConn.Write([]byte(respToWorker)); err != nil {
		log.Printf("Write to Client Failed: %v", err)
		return
	}

	// --- 阶段 B: 发起下游 (Headscale) 的 Tailscale 握手 ---
	// 还原为 Headscale 能识别的标准 HTTP Upgrade 请求
	reqToHeadscale := "POST /ts2021 HTTP/1.1\r\n" +
		"Host: 127.0.0.1:8080\r\n" +
		"Upgrade: tailscale-control-protocol\r\n" +
		"Connection: Upgrade\r\n" +
		"X-Tailscale-Handshake: " + tsHandshake + "\r\n" +
		"\r\n"

	if _, err := destConn.Write([]byte(reqToHeadscale)); err != nil {
		log.Printf("Write to Backend Failed: %v", err)
		return
	}

	// --- 阶段 C: 响应清洗 ---
	// Headscale 会回复 HTTP 101，但我们已经给 Worker 发过 101 了
	// 所以必须把 Headscale 的 Header 读取出来丢掉
	br := bufio.NewReader(destConn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return 
		}
		if line == "\r\n" {
			break // Header 读取完毕，之后是 Body
		}
	}

	// --- 阶段 D: 全双工流转发 ---
	errChan := make(chan error, 2)
	
	// Worker -> Headscale
	go func() {
		_, err := io.Copy(destConn, clientConn)
		errChan <- err
	}()
	
	// Headscale -> Worker
	go func() {
		// 注意使用 br.WriteTo，因为 br 可能缓冲了部分 Body 数据
		_, err := br.WriteTo(clientConn)
		errChan <- err
	}()

	<-errChan
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	// 严格的分流逻辑
	
	// 检查是否是合法的隧道请求 (必须包含 Upgrade 头 和 握手参数)
	upgrade := strings.ToLower(r.Header.Get("Upgrade"))
	hasHandshake := r.URL.Query().Get("ts_handshake") != ""
	isWS := strings.Contains(upgrade, "websocket")

	if isWS && hasHandshake {
		// 走协议转换通道
		handleTunnel(w, r)
	} else {
		// 走普通 HTTP 代理 (/key, /health 等)
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
		// 增加超时防止死锁
		IdleTimeout: 120 * time.Second, 
	}
	
	log.Printf("Strict-Protocol-Proxy listening on :%s", port)
	log.Fatal(server.ListenAndServe())
}