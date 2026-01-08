package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

const (
	targetURL = "127.0.0.1:8080" // TCP Dial 地址
	// WebSocket 协议规定的魔术字符串
	websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

// 计算 WebSocket 握手响应 Key
func computeAcceptKey(challengeKey string) string {
	h := sha1.New()
	io.WriteString(h, challengeKey)
	io.WriteString(h, websocketGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	// --- 1. 解析入站请求 (来自 Cloudflare) ---
	// 我们期待的是 Upgrade: websocket
	// 并且 handshake 数据藏在 URL 参数 "ts_handshake" 中
	
	wsKey := r.Header.Get("Sec-WebSocket-Key")
	tsHandshake := r.URL.Query().Get("ts_handshake")

	// 如果没有 WS Key 或没有 Handshake 参数，视为普通请求或非法请求
	if wsKey == "" || tsHandshake == "" {
		http.Error(w, "Invalid Handshake", http.StatusBadRequest)
		return
	}

	// --- 2. 劫持连接 (Hijack) ---
	// 在回应任何数据前，必须先劫持 TCP 连接
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

	// --- 3. 连接 Headscale (后端) ---
	destConn, err := net.DialTimeout("tcp", targetURL, 5*time.Second)
	if err != nil {
		log.Printf("Dial Headscale failed: %v", err)
		clientConn.Close() // 必须手动关闭
		return
	}
	defer destConn.Close()

	// --- 4. 核心：协议转换 ---

	// [A] 向 Cloudflare 发送"假"的 WebSocket 握手成功响应
	// 这样 Cloudflare 认为 WebSocket 建立成功，开始透传数据
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

	// [B] 向 Headscale 发送"真"的 Tailscale 握手请求
	// 此时 Headscale 看到的是标准的 Tailscale 协议
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
	// Headscale 会回复 HTTP 101，我们需要读取它但不转发给客户端
	// 因为我们刚才已经给客户端发过 101 了 (respToClient)
	br := bufio.NewReader(destConn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			log.Printf("Read backend header failed: %v", err)
			return
		}
		// 遇到空行，说明 Header 结束，后面是二进制流
		if line == "\r\n" {
			break
		}
	}

	// --- 5. 管道对接 (Streaming) ---
	// 此时双方都认为握手完成，直接交换 Noise 协议的二进制数据
	
	errChan := make(chan error, 2)
	go func() {
		// client -> headscale
		// 注意：Headscale 不需要读取客户端发送的 HTTP Body，
		// 因为握手信息已经在 Header (X-Tailscale-Handshake) 里发过去了
		// 所以这里直接转发后续的 TCP 数据
		_, err := io.Copy(destConn, clientConn)
		errChan <- err
	}()
	
	go func() {
		// headscale -> client (Headscale 响应 Body -> Client)
		// 注意我们要用 br (bufio reader) 因为刚才预读了 Header
		_, err := br.WriteTo(clientConn)
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
		Addr:    ":" + port,
		Handler: http.HandlerFunc(handleRequest),
	}
	log.Printf("Deep-Packet-Proxy listening on :%s", port)
	log.Fatal(server.ListenAndServe())
}