package main

import (
	"encoding/base64"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time" // 引入 time

	"github.com/gorilla/websocket"
)

const headscaleTarget = "127.0.0.1:8080"
const timeoutDuration = 60 * time.Second // 60秒无数据则断开

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
		log.Printf("Dial Headscale Failed: %v", err)
		return
	}
	defer tcpConn.Close()

	log.Printf("[Server] Tunnel Connected: %s", r.RemoteAddr)

	// 【关键】设置 Pong 处理函数：收到客户端的 Ping/Pong 自动延长超时时间
	wsConn.SetReadDeadline(time.Now().Add(timeoutDuration))
	wsConn.SetPongHandler(func(string) error {
		wsConn.SetReadDeadline(time.Now().Add(timeoutDuration))
		return nil
	})
	// 收到 Ping 也会自动回复 Pong，Gorilla 库底层已处理

	errChan := make(chan error, 2)

	// --- 接收方向 ---
	go func() {
		for {
			_, msg, err := wsConn.ReadMessage()
			if err != nil {
				errChan <- err
				return
			}

			// 【关键】每收到一次数据，就重置超时时间
			wsConn.SetReadDeadline(time.Now().Add(timeoutDuration))

			cleanMsg := strings.TrimSpace(string(msg))
			if len(cleanMsg) == 0 {
				continue
			}

			rawBytes, err := base64.StdEncoding.DecodeString(cleanMsg)
			if err != nil {
				rawBytes, err = base64.RawStdEncoding.DecodeString(cleanMsg)
				if err != nil {
					continue
				}
			}

			if _, err := tcpConn.Write(rawBytes); err != nil {
				errChan <- err
				return
			}
		}
	}()

	// --- 发送方向 ---
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := tcpConn.Read(buf)
			if err != nil {
				errChan <- err
				return
			}
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

func handleGetKey(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile("/authkey")
	if err != nil {
		http.Error(w, "404 Not Found", http.StatusNotFound)
		return
	}
	w.Write(data)
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}
	http.HandleFunc("/tunnel", handleTunnel)
	http.HandleFunc("/getkey", handleGetKey)
	log.Printf("Heartbeat-Server listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
