package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	targetHost = "127.0.0.1:8080"
	targetURL  = "http://127.0.0.1:8080"
	wsGUID     = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

var standardProxy *httputil.ReverseProxy

func init() {
	u, _ := url.Parse(targetURL)
	standardProxy = httputil.NewSingleHostReverseProxy(u)
	originalDirector := standardProxy.Director
	standardProxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = u.Host
	}
	standardProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("[PROXY_ERR] ReverseProxy failed: %v", err)
		http.Error(w, "Proxy Error", http.StatusBadGateway)
	}
}

func computeAcceptKey(challengeKey string) string {
	h := sha1.New()
	io.WriteString(h, challengeKey)
	io.WriteString(h, wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func handleTunnel(w http.ResponseWriter, r *http.Request) {
	wsKey := r.Header.Get("Sec-WebSocket-Key")
	tsHandshake := r.URL.Query().Get("ts_handshake")

	log.Printf("[TUNNEL] Starting tunnel handshake...")

	if wsKey == "" || tsHandshake == "" {
		msg := fmt.Sprintf("Missing Headers. WSKey=%v, HandshakeLen=%d", wsKey != "", len(tsHandshake))
		log.Printf("[TUNNEL_FAIL] %s", msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		log.Printf("[TUNNEL_FAIL] Server does not support Hijack")
		http.Error(w, "Hijack not supported", http.StatusInternalServerError)
		return
	}
	
	// 【关键修复】获取 clientBuf (缓冲区)
	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		log.Printf("[TUNNEL_FAIL] Hijack error: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	destConn, err := net.DialTimeout("tcp", targetHost, 5*time.Second)
	if err != nil {
		log.Printf("[TUNNEL_FAIL] Dial Backend failed: %v", err)
		clientConn.Close()
		return
	}
	defer destConn.Close()

	// [A] 回复 Cloudflare 101
	acceptKey := computeAcceptKey(wsKey)
	respToClient := fmt.Sprintf("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", acceptKey)
	
	if _, err := clientConn.Write([]byte(respToClient)); err != nil {
		log.Printf("[TUNNEL_FAIL] Write 101 to Client failed: %v", err)
		clientConn.Close()
		return
	}

	// [B] 发送请求给 Headscale
	reqToBackend := fmt.Sprintf("POST /ts2021 HTTP/1.1\r\nHost: %s\r\nUpgrade: tailscale-control-protocol\r\nConnection: Upgrade\r\nX-Tailscale-Handshake: %s\r\n\r\n", targetHost, tsHandshake)

	if _, err := destConn.Write([]byte(reqToBackend)); err != nil {
		log.Printf("[TUNNEL_FAIL] Write Handshake to Backend failed: %v", err)
		clientConn.Close()
		return
	}

	// [C] 消费 Headscale 响应头
	br := bufio.NewReader(destConn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			log.Printf("[TUNNEL_FAIL] Read Backend Header failed: %v", err)
			clientConn.Close()
			return 
		}
		if line == "\r\n" {
			break
		}
	}

	log.Printf("[TUNNEL] Handshake success! Piping data...")

	// [D] 管道转发
	errChan := make(chan error, 2)
	
	// Client -> Headscale
	go func() {
		// 【关键修复】先清空缓冲区，再读 Socket
		// 如果不加上这一步，Headscale 将永远收不到第一个数据包，导致死锁/502
		if clientBuf.Reader.Buffered() > 0 {
			if _, err := io.Copy(destConn, clientBuf); err != nil {
				errChan <- err
				return
			}
		}
		
		// 缓冲区读完后，继续读 TCP 连接
		_, copyErr := io.Copy(destConn, clientConn)
		errChan <- copyErr
	}()
	
	// Headscale -> Client
	go func() {
		_, copyErr := br.WriteTo(clientConn)
		errChan <- copyErr
	}()

	tunnelErr := <-errChan
	log.Printf("[TUNNEL_END] Connection closed: %v", tunnelErr)
	clientConn.Close()
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	upgrade := r.Header.Get("Upgrade")
	handshakeParam := r.URL.Query().Get("ts_handshake")
	
	log.Printf("[REQ] Method=%s URL=%s Upgrade=%s HandshakeParam=%v", 
		r.Method, r.URL.Path, upgrade, handshakeParam != "")

	isWS := strings.Contains(strings.ToLower(upgrade), "websocket")
	hasHandshake := handshakeParam != ""

	if isWS && hasHandshake {
		log.Printf("[ROUTER] Matched -> Tunnel Mode")
		handleTunnel(w, r)
	} else {
		log.Printf("[ROUTER] Default -> Standard Proxy Mode")
		standardProxy.ServeHTTP(w, r)
	}
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
	log.Printf("Fixed-Proxy listening on :%s", port)
	log.Fatal(server.ListenAndServe())
}