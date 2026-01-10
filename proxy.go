package main

import (
	"encoding/base64"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/gorilla/websocket"
)

const headscaleTarget = "127.0.0.1:8080"

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func handleTunnel(w http.ResponseWriter, r *http.Request) {
	wsConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WS Upgrade Failed: %v", err)
		return
	}
	defer wsConn.Close()

	tcpConn, err := net.Dial("tcp", headscaleTarget)
	if err != nil {
		log.Printf("Dial Target Failed: %v", err)
		return
	}
	defer tcpConn.Close()

	log.Printf("[Tunnel] Connected. Proxying...")

	errChan := make(chan error, 2)

	// --- 协程 A: WS (Base64) -> TCP (Raw) ---
	go func() {
		defer func() {
			// 关闭 TCP 写端，通知 Headscale 数据发完了
			if c, ok := tcpConn.(*net.TCPConn); ok {
				c.CloseWrite()
			}
		}()

		for {
			_, message, err := wsConn.ReadMessage()
			if err != nil {
				// WS 断开 (通常是 Worker 发完数据后等待响应时)
				if err != io.EOF && !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
					log.Printf("[Rx] WS Read Error: %v", err)
				}
				errChan <- err
				return
			}

			// 容错处理：去除可能存在的换行符
			cleanMsg := strings.TrimSpace(string(message))
			if len(cleanMsg) == 0 {
				continue
			}

			// 使用 RawStdEncoding 尝试解码 (更宽容)
			rawBytes, err := base64.StdEncoding.DecodeString(cleanMsg)
			if err != nil {
				// 尝试 Raw 解码 (无 Padding)
				rawBytes, err = base64.RawStdEncoding.DecodeString(cleanMsg)
				if err != nil {
					log.Printf("[Rx] Base64 Decode Fail. Len=%d, Err=%v", len(cleanMsg), err)
					continue
				}
			}

			n, err := tcpConn.Write(rawBytes)
			log.Printf("[Rx] Wrote %d bytes to Headscale", n) // 关键日志：看是否有数据写入
			if err != nil {
				log.Printf("[Rx] Write TCP Fail: %v", err)
				errChan <- err
				return
			}
		}
	}()

	// --- 协程 B: TCP (Raw) -> WS (Base64) ---
	go func() {
		buffer := make([]byte, 32*1024)
		for {
			n, err := tcpConn.Read(buffer)
			if err != nil {
				if err != io.EOF {
					log.Printf("[Tx] Read TCP Fail: %v", err)
				}
				errChan <- err
				return
			}
			
			log.Printf("[Tx] Read %d bytes from Headscale", n) // 关键日志：看 Headscale 是否回复
			
			encodedMsg := base64.StdEncoding.EncodeToString(buffer[:n])
			if err := wsConn.WriteMessage(websocket.TextMessage, []byte(encodedMsg)); err != nil {
				log.Printf("[Tx] WS Write Fail: %v", err)
				errChan <- err
				return
			}
		}
	}()

	err := <-errChan
	log.Printf("[Tunnel] Closed. Reason: %v", err)
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}
	http.HandleFunc("/tunnel", handleTunnel)
	log.Printf("Verbose-Tunnel listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}