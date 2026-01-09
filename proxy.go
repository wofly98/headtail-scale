package main

import (
	"encoding/base64"
	"log"
	"net"
	"net/http"
	"os"

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

	log.Printf("[Tunnel] Connected to %s", headscaleTarget)

	errChan := make(chan error, 2)

	// WS -> TCP
	go func() {
		for {
			_, message, err := wsConn.ReadMessage()
			if err != nil {
				errChan <- err
				return
			}
			rawBytes, err := base64.StdEncoding.DecodeString(string(message))
			if err != nil {
				log.Printf("Decode Error: %v", err)
				continue
			}
			if _, err := tcpConn.Write(rawBytes); err != nil {
				errChan <- err
				return
			}
		}
	}()

	// TCP -> WS
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

	// 等待任意一方报错或关闭
	exitErr := <-errChan
	// 【关键修改】打印具体错误原因，方便排查是 EOF 还是 Timeout
	log.Printf("[Tunnel] Closed. Reason: %v", exitErr)
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}
	http.HandleFunc("/tunnel", handleTunnel)
	log.Printf("Debug-Tunnel listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}