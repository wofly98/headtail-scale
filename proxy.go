package main

import (
	"encoding/base64"
	"log"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/gorilla/websocket"
)

// Headscale 本地地址
const headscaleTarget = "127.0.0.1:8080"

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func handleTunnel(w http.ResponseWriter, r *http.Request) {
	// 1. 建立 WebSocket 连接
	wsConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WS Upgrade Failed: %v", err)
		return
	}
	defer wsConn.Close()

	// 2. 连接本地 Headscale
	tcpConn, err := net.Dial("tcp", headscaleTarget)
	if err != nil {
		log.Printf("Dial Headscale Failed: %v", err)
		return
	}
	defer tcpConn.Close()

	log.Printf("[Server] Tunnel Connected: %s -> %s", r.RemoteAddr, headscaleTarget)

	errChan := make(chan error, 2)

	// --- 接收方向：WS(Base64) -> TCP(Raw) ---
	go func() {
		for {
			_, msg, err := wsConn.ReadMessage()
			if err != nil {
				errChan <- err
				return
			}
			
			cleanMsg := strings.TrimSpace(string(msg))
			if len(cleanMsg) == 0 { continue }

			// 解码 Base64
			rawBytes, err := base64.StdEncoding.DecodeString(cleanMsg)
			if err != nil {
				// 容错：尝试 Raw 解码
				rawBytes, err = base64.RawStdEncoding.DecodeString(cleanMsg)
				if err != nil {
					log.Printf("Decode Error: %v", err)
					continue
				}
			}

			if _, err := tcpConn.Write(rawBytes); err != nil {
				errChan <- err
				return
			}
		}
	}()

	// --- 发送方向：TCP(Raw) -> WS(Base64) ---
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := tcpConn.Read(buf)
			if err != nil {
				errChan <- err
				return
			}
			// 编码 Base64 发送
			encoded := base64.StdEncoding.EncodeToString(buf[:n])
			if err := wsConn.WriteMessage(websocket.TextMessage, []byte(encoded)); err != nil {
				errChan <- err
				return
			}
		}
	}()

	<-errChan
	log.Printf("[Server] Tunnel Closed")
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}
	http.HandleFunc("/tunnel", handleTunnel)
	log.Printf("Tunnel Server (proxy.go) listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}