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
	// 1. 升级 WebSocket
	// err 第一次声明
	wsConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WS Upgrade Failed: %v", err)
		return
	}
	defer wsConn.Close()

	// 2. 连接 TCP
	// err 复用 (因为 tcpConn 是新变量，所以用 := 是合法的)
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
		// 关闭 TCP 写端，通知 Headscale 数据发完了 (EOF)
		defer func() {
			if c, ok := tcpConn.(*net.TCPConn); ok {
				c.CloseWrite()
			}
		}()

		for {
			_, message, readErr := wsConn.ReadMessage()
			if readErr != nil {
				// 忽略正常的关闭错误
				if readErr != io.EOF && !websocket.IsCloseError(readErr, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					log.Printf("[Rx] WS Read Error: %v", readErr)
				}
				errChan <- readErr
				return
			}

			// 清理并解码
			cleanMsg := strings.TrimSpace(string(message))
			if len(cleanMsg) == 0 {
				continue
			}

			// 显式声明变量，避免作用域混淆
			var rawBytes []byte
			var decodeErr error

			// 尝试标准解码
			rawBytes, decodeErr = base64.StdEncoding.DecodeString(cleanMsg)
			if decodeErr != nil {
				// 尝试 Raw 解码
				rawBytes, decodeErr = base64.RawStdEncoding.DecodeString(cleanMsg)
				if decodeErr != nil {
					log.Printf("[Rx] Base64 Decode Fail: %v", decodeErr)
					continue
				}
			}

			n, writeErr := tcpConn.Write(rawBytes)
			if writeErr != nil {
				log.Printf("[Rx] TCP Write Fail: %v", writeErr)
				errChan <- writeErr
				return
			}
			log.Printf("[Rx] Forwarded %d bytes to Headscale", n)
		}
	}()

	// --- 协程 B: TCP (Raw) -> WS (Base64) ---
	go func() {
		buffer := make([]byte, 32*1024)
		for {
			n, readErr := tcpConn.Read(buffer)
			if readErr != nil {
				if readErr != io.EOF {
					log.Printf("[Tx] TCP Read Fail: %v", readErr)
				}
				errChan <- readErr
				return
			}
			
			log.Printf("[Tx] Got %d bytes from Headscale", n)

			encodedMsg := base64.StdEncoding.EncodeToString(buffer[:n])
			if writeErr := wsConn.WriteMessage(websocket.TextMessage, []byte(encodedMsg)); writeErr != nil {
				log.Printf("[Tx] WS Write Fail: %v", writeErr)
				errChan <- writeErr
				return
			}
		}
	}()

	// 【关键修复】使用新变量名 exitErr，避免与函数顶部的 err 冲突
	exitErr := <-errChan
	log.Printf("[Tunnel] Closed. Reason: %v", exitErr)
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}
	http.HandleFunc("/tunnel", handleTunnel)
	log.Printf("Fixed-Tunnel listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}