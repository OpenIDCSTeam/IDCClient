// Package api VNC/WebSocket代理服务
// 在6080端口提供WebSocket->TCP VNC代理（Websockify功能）
// 在7681端口提供WebSocket SSH终端服务（ttyd功能）
package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"
)

// VNCToken VNC访问Token
type VNCToken struct {
	HsName   string
	VmUUID   string
	Host     string
	Port     int
	ClientIP string
	ExpireAt time.Time
}

var (
	vncTokens   = make(map[string]*VNCToken)
	vncTokensMu sync.RWMutex
)

// RegisterVNCToken 注册VNC一次性Token（有效期5分钟，绑定客户端IP）
func RegisterVNCToken(token, hsName, vmUUID, host, clientIP string, port int) {
	vncTokensMu.Lock()
	defer vncTokensMu.Unlock()
	vncTokens[token] = &VNCToken{
		HsName:   hsName,
		VmUUID:   vmUUID,
		Host:     host,
		Port:     port,
		ClientIP: clientIP,
		ExpireAt: time.Now().Add(5 * time.Minute),
	}
}

// GetVNCToken 获取并验证VNC Token（一次性使用后删除）
func GetVNCToken(token string, clientIP string) *VNCToken {
	vncTokensMu.Lock()
	defer vncTokensMu.Unlock()
	t, ok := vncTokens[token]
	if !ok || time.Now().After(t.ExpireAt) {
		if ok {
			delete(vncTokens, token)
		}
		return nil
	}
	// 验证客户端IP绑定（跳过本地回环）
	if clientIP != "127.0.0.1" && clientIP != "::1" && t.ClientIP != clientIP && t.ClientIP != "" {
		return nil
	}
	// 一次性使用：验证后立即删除
	delete(vncTokens, token)
	return t
}

// VNCProxyServer VNC WebSocket代理服务器（Websockify）
type VNCProxyServer struct {
	port     int
	upgrader websocket.Upgrader
}

// allowedOrigins VNC允许的Origin白名单
var allowedOrigins = map[string]bool{
	"http://localhost:1881":  true,
	"http://localhost:5173":  true,
	"http://127.0.0.1:1881":  true,
	"http://127.0.0.1:5173":  true,
}

// NewVNCProxyServer 创建VNC代理服务器
func NewVNCProxyServer(port int) *VNCProxyServer {
	return &VNCProxyServer{
		port: port,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if origin == "" {
					return true
				}
				if allowedOrigins[origin] {
					return true
				}
				log.Printf("[vnc-proxy] 拒绝Origin: %s", origin)
				return false
			},
			ReadBufferSize:  8192,
			WriteBufferSize: 8192,
		},
	}
}

// Start 启动VNC代理服务器
func (s *VNCProxyServer) Start() {
	mux := http.NewServeMux()
	mux.HandleFunc("/websockify", s.handleWebsockify)
	mux.Handle("/", http.FileServer(http.Dir("./VNCConsole")))

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: mux,
	}
	go func() {
		log.Printf("[vnc-proxy] 启动VNC代理服务，端口: %d", s.port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[vnc-proxy] 启动失败: %v", err)
		}
	}()
}

// handleWebsockify 处理WebSocket->TCP代理
func (s *VNCProxyServer) handleWebsockify(w http.ResponseWriter, r *http.Request) {
	// 验证Token + IP绑定
	token := r.URL.Query().Get("token")
	clientIP := getRealIP(r)
	vncToken := GetVNCToken(token, clientIP)
	if vncToken == nil {
		http.Error(w, "Token无效或已过期", http.StatusUnauthorized)
		return
	}

	// 升级WebSocket连接
	wsConn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[vnc-proxy] WebSocket升级失败: %v", err)
		return
	}
	defer wsConn.Close()

	// 设置读写超时（空闲10分钟断开）
	wsConn.SetReadDeadline(time.Now().Add(10 * time.Minute))
	wsConn.SetPongHandler(func(string) error {
		wsConn.SetReadDeadline(time.Now().Add(10 * time.Minute))
		return nil
	})

	// 连接到VNC TCP服务
	tcpAddr := fmt.Sprintf("%s:%d", vncToken.Host, vncToken.Port)
	tcpConn, err := net.DialTimeout("tcp", tcpAddr, 10*time.Second)
	if err != nil {
		log.Printf("[vnc-proxy] 连接VNC服务失败 %s: %v", tcpAddr, err)
		return
	}
	defer tcpConn.Close()

	// 双向代理（使用channel等待双方都结束）
	done := make(chan struct{}, 2)

	go func() {
		defer func() { done <- struct{}{} }()
		for {
			_, msg, err := wsConn.ReadMessage()
			if err != nil {
				return
			}
			if _, err := tcpConn.Write(msg); err != nil {
				return
			}
		}
	}()

	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32768)
		for {
			n, err := tcpConn.Read(buf)
			if err != nil {
				return
			}
			if err := wsConn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				return
			}
		}
	}()

	<-done
}

// getRealIP 获取客户端真实IP（考虑反向代理）
func getRealIP(r *http.Request) string {
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		parts := strings.Split(ip, ",")
		return strings.TrimSpace(parts[0])
	}
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// SSHConnInfo SSH连接信息
type SSHConnInfo struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	UseKey   bool   `json:"use_key,omitempty"`
}

// SSHTerminalServer SSH终端WebSocket代理服务（ttyd功能）
type SSHTerminalServer struct {
	port     int
	upgrader websocket.Upgrader
	tokens   map[string]*SSHConnInfo
	tokenMu  sync.RWMutex
}

// NewSSHTerminalServer 创建SSH终端服务
func NewSSHTerminalServer(port int) *SSHTerminalServer {
	s := &SSHTerminalServer{
		port: port,
		tokens: make(map[string]*SSHConnInfo),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if origin == "" {
					return true
				}
				return allowedOrigins[origin]
			},
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
		},
	}
	return s
}

// RegisterToken 注册SSH连接Token（有效期30秒，一次性使用）
func (s *SSHTerminalServer) RegisterToken(token string, info *SSHConnInfo) {
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	s.tokens[token] = info
	// 30秒后自动过期
	go func() {
		time.Sleep(30 * time.Second)
		s.tokenMu.Lock()
		delete(s.tokens, token)
		s.tokenMu.Unlock()
	}()
}

// validateToken 验证并消费Token
func (s *SSHTerminalServer) validateToken(token string) *SSHConnInfo {
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	info, ok := s.tokens[token]
	if ok {
		delete(s.tokens, token) // 一次性使用
	}
	return info
}

// Start 启动SSH终端服务器
func (s *SSHTerminalServer) Start() {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: mux,
	}
	go func() {
		log.Printf("[ssh-terminal] 启动SSH终端服务，端口: %d", s.port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[ssh-terminal] 启动失败: %v", err)
		}
	}()
}

// handleWS 处理WebSocket连接，代理SSH终端I/O
func (s *SSHTerminalServer) handleWS(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "缺少token参数", http.StatusBadRequest)
		return
	}
	connInfo := s.validateToken(token)
	if connInfo == nil {
		http.Error(w, "Token无效或已过期", http.StatusUnauthorized)
		return
	}

	wsConn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[ssh-terminal] WebSocket升级失败: %v", err)
		return
	}
	defer wsConn.Close()

	// 配置SSH客户端
	sshConfig := &ssh.ClientConfig{
		User: connInfo.User,
		Auth: []ssh.AuthMethod{
			ssh.Password(connInfo.Password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}

	addr := fmt.Sprintf("%s:%d", connInfo.Host, connInfo.Port)
	sshClient, err := ssh.Dial("tcp", addr, sshConfig)
	if err != nil {
		log.Printf("[ssh-terminal] SSH连接失败 %s: %v", addr, err)
		wsConn.WriteMessage(websocket.TextMessage,
			[]byte(fmt.Sprintf("\r\n\x1b[31mSSH连接失败: %v\x1b[0m\r\n", err)))
		return
	}
	defer sshClient.Close()

	// 打开SSH会话
	session, err := sshClient.NewSession()
	if err != nil {
		log.Printf("[ssh-terminal] 创建SSH会话失败: %v", err)
		return
	}
	defer session.Close()

	// 请求PTY（伪终端）
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm-256color", 40, 80, modes); err != nil {
		log.Printf("[ssh-terminal] PTY请求失败: %v", err)
		return
	}

	stdinPipe, _ := session.StdinPipe()
	stdoutPipe, _ := session.StdoutPipe()
	stderrPipe, _ := session.StderrPipe()

	if err := session.Shell(); err != nil {
		log.Printf("[ssh-terminal] 启动Shell失败: %v", err)
		return
	}

	done := make(chan struct{}, 3)

	// WebSocket -> SSH stdin
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			_, msg, err := wsConn.ReadMessage()
			if err != nil {
				return
			}
			// 解析终端大小调整消息（JSON格式：{"cols":80,"rows":24}）
			var resize struct {
				Cols int `json:"cols"`
				Rows int `json:"rows"`
			}
			if json.Unmarshal(msg, &resize) == nil && resize.Cols > 0 && resize.Rows > 0 {
				session.WindowChange(resize.Rows, resize.Cols)
				continue
			}
			if _, err := stdinPipe.Write(msg); err != nil {
				return
			}
		}
	}()

	// SSH stdout -> WebSocket
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 8192)
		for {
			n, err := stdoutPipe.Read(buf)
			if err != nil {
				return
			}
			if err := wsConn.WriteMessage(websocket.TextMessage, buf[:n]); err != nil {
				return
			}
		}
	}()

	// SSH stderr -> WebSocket
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 8192)
		for {
			n, err := stderrPipe.Read(buf)
			if err != nil {
				return
			}
			if err := wsConn.WriteMessage(websocket.TextMessage, buf[:n]); err != nil {
				return
			}
		}
	}()

	// 等待任意一端断开
	<-done
	log.Printf("[ssh-terminal] 会话结束: %s@%s", connInfo.User, addr)
}