package main

import (
	"encoding/base64"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const headscaleTarget = "127.0.0.1:8080"

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func handleTunnel(w http.ResponseWriter, r *http.Request) {
	wsConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Upgrade Error: %v", err)
		return
	}
	defer wsConn.Close()

	tcpConn, err := net.Dial("tcp", headscaleTarget)
	if err != nil {
		log.Printf("Dial Target Error: %v", err)
		return
	}
	defer tcpConn.Close()

	log.Printf("[Tunnel] Connected.")

	errChan := make(chan error, 3)

	// --- 心跳保活协程 ---
	// 每 10 秒发送一个 KeepAlive 帧，防止中间路由(Cloudflare/Koyeb)断开空闲连接
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				// 发送一个特殊的 Base64 包 "KEEP_ALIVE"
				// Worker 尝试解码时会失败或得到无意义数据，这都没关系，关键是有流量通过
				// "KEEP_ALIVE" 不是有效的 Base64，Worker 会抛错并忽略，完美。
				if err := wsConn.WriteMessage(websocket.TextMessage, []byte("KEEP_ALIVE")); err != nil {
					return
				}
			}
		}
	}()

	// --- A: WS -> TCP ---
	go func() {
		for {
			_, message, err := wsConn.ReadMessage()
			if err != nil {
				if err != io.EOF && !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					log.Printf("[Rx] WS Error: %v", err)
				}
				errChan <- err
				return
			}

			cleanMsg := strings.TrimSpace(string(message))
			if len(cleanMsg) == 0 { continue }

			rawBytes, err := base64.StdEncoding.DecodeString(cleanMsg)
			if err != nil {
				// 尝试 Raw 解码
				rawBytes, err = base64.RawStdEncoding.DecodeString(cleanMsg)
				if err != nil {
					// 忽略解码错误 (可能是心跳包或其他干扰)
					continue
				}
			}

			if _, err := tcpConn.Write(rawBytes); err != nil {
				errChan <- err
				return
			}
		}
	}()

	// --- B: TCP -> WS ---
	go func() {
		buffer := make([]byte, 32*1024)
		for {
			n, err := tcpConn.Read(buffer)
			if err != nil {
				errChan <- err
				return
			}
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
	log.Printf("KeepAlive-Tunnel listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}