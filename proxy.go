package main

import (
	"encoding/base64"
	"log"
	"net"
	"net/http"
	"os"

	"github.com/gorilla/websocket"
)

// 这里是全链路唯一配置实际服务地址的地方
const headscaleTarget = "127.0.0.1:8080"

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func handleTunnel(w http.ResponseWriter, r *http.Request) {
	// 1. 隧道接入
	wsConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Upgrade Error: %v", err)
		return
	}
	defer wsConn.Close()

	// 2. 连接实际业务服务 (Headscale)
	tcpConn, err := net.Dial("tcp", headscaleTarget)
	if err != nil {
		log.Printf("Dial Target Error: %v", err)
		return
	}
	defer tcpConn.Close()

	log.Printf("[Tunnel] Connected to %s", headscaleTarget)

	errChan := make(chan error, 2)

	// --- 收 (WS -> Base64 Decode -> TCP) ---
	go func() {
		for {
			_, message, err := wsConn.ReadMessage()
			if err != nil {
				errChan <- err
				return
			}
			// 解码 Worker 传来的密文
			rawBytes, err := base64.StdEncoding.DecodeString(string(message))
			if err != nil {
				// 解码失败跳过，保持连接不断
				log.Printf("Decode Error: %v", err)
				continue
			}
			// 写入本地服务
			if _, err := tcpConn.Write(rawBytes); err != nil {
				errChan <- err
				return
			}
		}
	}()

	// --- 发 (TCP -> Base64 Encode -> WS) ---
	go func() {
		buffer := make([]byte, 32*1024)
		for {
			n, err := tcpConn.Read(buffer)
			if err != nil {
				errChan <- err
				return
			}
			// 编码本地服务的原始响应
			encodedMsg := base64.StdEncoding.EncodeToString(buffer[:n])
			
			if err := wsConn.WriteMessage(websocket.TextMessage, []byte(encodedMsg)); err != nil {
				errChan <- err
				return
			}
		}
	}()

	<-errChan
	log.Printf("[Tunnel] Closed")
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}
	http.HandleFunc("/tunnel", handleTunnel)
	log.Printf("Base64 Tunnel listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}