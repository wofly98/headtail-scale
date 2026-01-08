package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"
	// 已移除 "net/url"，因为没有显式使用 url.Parse
)

const (
	targetURL = "127.0.0.1:8080"
	// WebSocket 协议规定的魔术字符串 (Magic GUID)
	websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

// 计算 WebSocket 握手响应 Key
// 算法：Base64(SHA1(Sec-WebSocket-Key + GUID))
func computeAcceptKey(challengeKey string) string {
	h := sha1.New()
	io.WriteString(h, challengeKey)
	io.WriteString(h, websocketGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	// --- 1. 解析入站请求 (来自 Cloudflare) ---
	// Cloudflare 发来的是标准 WebSocket 请求
	// 我们约定：真实的握手数据放在 URL 参数 "ts_handshake" 中
	
	wsKey := r.Header.Get("Sec-WebSocket-Key")
	tsHandshake := r.URL.Query().Get("ts_handshake")

	// 校验必要参数
	if wsKey == "" || tsHandshake == "" {
		http.Error(w, "Invalid Handshake Parameters", http.StatusBadRequest)
		return
	}

	// --- 2. 劫持连接 (Hijack) ---
	// 必须在发送任何响应之前接管 TCP 连接
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
	// 注意：劫持后必须负责关闭连接
	defer clientConn.Close()

	// --- 3. 连接 Headscale (后端) ---
	destConn, err := net.DialTimeout("tcp", targetURL, 5*time.Second)
	if err != nil {
		log.Printf("Dial Headscale failed: %v", err)
		return
	}
	defer destConn.Close()

	// --- 4. 协议欺骗 (核心逻辑) ---

	// [A] 欺骗 Cloudflare：发送 WebSocket 101 握手成功响应
	// 这样 Cloudflare 认为连接已建立，开始透传数据
	acceptKey := computeAcceptKey(wsKey)
	respToClient := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey + "\r\n" +
		"\r\n"
	
	if _, err := clientConn.Write([]byte(respToClient)); err != nil {
		log.Printf("Write WS Handshake to client failed: %v", err)
		return
	}

	// [B] 欺骗 Headscale：发送 Tailscale HTTP 升级请求
	// 将 URL 参数里的 ts_handshake 还原回 X-Tailscale-Handshake 头
	reqToBackend := "POST /ts2021 HTTP/1.1\r\n" +
		"Host: 127.0.0.1:8080\r\n" +
		"Upgrade: tailscale-control-protocol\r\n" +
		"Connection: Upgrade\r\n" +
		"X-Tailscale-Handshake: " + tsHandshake + "\r\n" +
		"\r\n"

	if _, err := destConn.Write([]byte(reqToBackend)); err != nil {
		log.Printf("Write Handshake to backend failed: %v", err)
		return
	}

	// [C] 消费 Headscale 的响应头
	// Headscale 会回复 HTTP 101，我们需要把它读取出来丢掉
	// 因为我们刚才已经在 [A] 步给 Cloudflare 发过 101 了，不能发两次
	br := bufio.NewReader(destConn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			log.Printf("Read backend header failed: %v", err)
			return
		}
		// HTTP 头以空行 (\r\n) 结束
		if line == "\r\n" {
			break
		}
	}

	// --- 5. 双向管道对接 (Streaming) ---
	// 握手完成，进入纯二进制流转发模式
	
	errChan := make(chan error, 2)
	
	// 协程 1: Client -> Headscale
	go func() {
		// 直接转发，因为 Headscale 已经通过 Header 拿到了握手数据
		_, err := io.Copy(destConn, clientConn)
		errChan <- err
	}()
	
	// 协程 2: Headscale -> Client
	go func() {
		// 使用 br.WriteTo 因为 br 可能预读了部分 Headscale 的 Body 数据
		_, err := br.WriteTo(clientConn)
		errChan <- err
	}()

	// 等待任意一方断开
	<-errChan
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
	log.Printf("Deep-Packet-Proxy listening on :%s", port)
	log.Fatal(server.ListenAndServe())
}