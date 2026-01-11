package main

import (
	"encoding/base64"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

var (
	localAddr  = flag.String("l", ":44300", "本地监听地址")
	remoteAddr = flag.String("s", "wss://past-adelice-godsheaven-2114a06f.koyeb.app/tunnel", "远程 WebSocket 地址")
)

func handleClient(localConn net.Conn) {
	defer localConn.Close()
	log.Printf("[Client] New Connection...")

	// 1. 连接远程 WebSocket
	dialer := websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second
	wsConn, _, err := dialer.Dial(*remoteAddr, nil)
	if err != nil {
		log.Printf("[Client] Dial Failed: %v", err)
		return
	}
	defer wsConn.Close()

	// 【关键】启动心跳协程
	// 每 15 秒发送一个 Ping，告诉服务器别断开我
	stopHeartbeat := make(chan struct{})
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				// 发送 Ping 控制帧 (不需要 Base64，这是协议层的)
				if err := wsConn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(5*time.Second)); err != nil {
					log.Printf("[Client] Ping Failed: %v", err)
					return // 发送失败意味着连接断了
				}
			case <-stopHeartbeat:
				return
			}
		}
	}()
	defer close(stopHeartbeat) // 连接关闭时停止心跳

	log.Printf("[Client] Tunnel Established.")
	errChan := make(chan error, 2)

	// --- 上行：本地 -> 远程 ---
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := localConn.Read(buf)
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

	// --- 下行：远程 -> 本地 ---
	go func() {
		for {
			_, msg, err := wsConn.ReadMessage()
			if err != nil {
				errChan <- err
				return
			}
			
			cleanMsg := strings.TrimSpace(string(msg))
			if len(cleanMsg) == 0 { continue }

			rawBytes, err := base64.StdEncoding.DecodeString(cleanMsg)
			if err != nil { continue }

			if _, err := localConn.Write(rawBytes); err != nil {
				errChan <- err
				return
			}
		}
	}()

	<-errChan
	log.Printf("[Client] Connection Closed (Will Retry via Tailscale)")
}

func main() {
	flag.Parse()
	listener, err := net.Listen("tcp", *localAddr)
	if err != nil { log.Fatal(err) }

	log.Printf("== Heartbeat Client Started on %s ==", *localAddr)

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		<-c
		os.Exit(0)
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Println("Accept error:", err)
			continue
		}
		go handleClient(conn)
	}
}