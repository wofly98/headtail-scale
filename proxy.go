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
	targetHost = "127.0.0.1:8080"
	targetURL  = "http://127.0.0.1:8080"
	wsGUID     = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

// 全局普通反向代理实例
var standardProxy *httputil.ReverseProxy

func init() {
	u, _ := url.Parse(targetURL)
	standardProxy = httputil.NewSingleHostReverseProxy(u)
	
	// 自定义 Director 以确保 Host 头正确
	originalDirector := standardProxy.Director
	standardProxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = u.Host
	}
}

func computeAcceptKey(challengeKey string) string {
	h := sha1.New()
	io.WriteString(h, challengeKey)
	io.WriteString(h, wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// 处理隧道连接 (协议欺骗逻辑)
func handleTunnel(w http.ResponseWriter, r *http.Request) {
	wsKey := r.Header.Get("Sec-WebSocket-Key")
	tsHandshake := r.URL.Query().Get("ts_handshake")

	// 严格校验：必须有 WS Key 和 Handshake 参数
	if wsKey == "" || tsHandshake == "" {
		http.Error(w, "Invalid Tunnel Request", http.StatusBadRequest)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijack not supported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	destConn, err := net.DialTimeout("tcp", targetHost, 5*time.Second)
	if err != nil {
		log.Printf("Dial Headscale failed: %v", err)
		return
	}
	defer destConn.Close()

	// [A] 欺骗 Cloudflare (回写 101)
	acceptKey := computeAcceptKey(wsKey)
	respToClient := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey + "\r\n" +
		"\r\n"
	
	if _, err := clientConn.Write([]byte(respToClient)); err != nil {
		return
	}

	// [B] 欺骗 Headscale (发送 Tailscale 握手)
	reqToBackend := "POST /ts2021 HTTP/1.1\r\n" +
		"Host: 127.0.0.1:8080\r\n" +
		"Upgrade: tailscale-control-protocol\r\n" +
		"Connection: Upgrade\r\n" +
		"X-Tailscale-Handshake: " + tsHandshake + "\r\n" +
		"\r\n"

	if _, err := destConn.Write([]byte(reqToBackend)); err != nil {
		return
	}

	// [C] 丢弃 Headscale 的响应头
	br := bufio.NewReader(destConn)
	for {
		line, err := br.ReadString('\n')
		if err != nil || line == "\r\n" {
			break
		}
	}

	// [D] 管道透传
	errChan := make(chan error, 2)
	go func() {
		_, err := io.Copy(destConn, clientConn)
		errChan <- err
	}()
	go func() {
		_, err := br.WriteTo(clientConn)
		errChan <- err
	}()
	<-errChan
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	// --- 路由分发核心 ---
	
	// 判断 1: 是否是 WebSocket 升级请求 (Tailscale 隧道)
	isWS := strings.Contains(strings.ToLower(r.Header.Get("Upgrade")), "websocket")
	
	// 判断 2: 是否携带了关键的握手参数
	hasHandshake := r.URL.Query().Get("ts_handshake") != ""

	if isWS && hasHandshake {
		// 进入隧道模式 (Deep Packet Transcoding)
		handleTunnel(w, r)
	} else {
		// 进入普通代理模式 (处理 /key, /health 等)
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
	}
	log.Printf("Dual-Mode Proxy listening on :%s", port)
	log.Fatal(server.ListenAndServe())
}