package vnc

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/models"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

// VNCManager VNC管理器
type VNCManager struct {
	sessions      map[string]*VNCSession
	allocatedPorts map[int]bool
	mutex         sync.RWMutex
	logger        *logrus.Logger
	config        VNCConfig
	upgrader      websocket.Upgrader
}

// VNCConfig VNC配置
type VNCConfig struct {
	PortRange    string
	MaxSessions  int
	SessionTimeout int
	Encryption   bool
	Recording    bool
}

// VNCSession VNC会话
type VNCSession struct {
	ID           string
	VMUUID       string
	HSName       string
	VNCPort      int
	WSConn       *websocket.Conn
	VNCConn      net.Conn
	Created      time.Time
	LastActivity time.Time
	Active       bool
	Password     string
	Recording    bool
}

// VNCMessage VNC消息
type VNCMessage struct {
	Type    string      `json:"type"`
	Data    interface{} `json:"data"`
	Time    time.Time   `json:"time"`
}

// NewVNCManager 创建VNC管理器
func NewVNCManager(config VNCConfig, logger *logrus.Logger) *VNCManager {
	return &VNCManager{
		sessions:       make(map[string]*VNCSession),
		allocatedPorts: make(map[int]bool),
		mutex:          sync.RWMutex{},
		logger:         logger,
		config:         config,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true // 允许所有来源，生产环境中应该更严格
			},
		},
	}
}

// CreateSession 创建VNC会话
func (v *VNCManager) CreateSession(hsName, vmUUID string, vncConfig *models.VMConfig) (*VNCSession, error) {
	v.mutex.Lock()
	defer v.mutex.Unlock()

	// 检查会话数量限制
	if len(v.sessions) >= v.config.MaxSessions {
		return nil, fmt.Errorf("达到最大会话数量限制")
	}

	// 检查是否已存在该VM的会话
	for _, session := range v.sessions {
		if session.VMUUID == vmUUID && session.Active {
			return nil, fmt.Errorf("该虚拟机已有活跃的VNC会话")
		}
	}

	// 分配端口
	port, err := v.allocatePort()
	if err != nil {
		return nil, fmt.Errorf("分配端口失败: %w", err)
	}

	// 生成会话ID
	sessionID := v.generateSessionID()

	// 创建会话
	session := &VNCSession{
		ID:           sessionID,
		VMUUID:       vmUUID,
		HSName:       hsName,
		VNCPort:      port,
		Created:      time.Now(),
		LastActivity: time.Now(),
		Active:       true,
		Password:     vncConfig.VCPass,
		Recording:    v.config.Recording && vncConfig.VCEnabled,
	}

	v.sessions[sessionID] = session
	v.logger.Infof("创建VNC会话: %s, VM: %s/%s, 端口: %d", sessionID, hsName, vmUUID, port)

	return session, nil
}

// GetSession 获取VNC会话
func (v *VNCManager) GetSession(sessionID string) (*VNCSession, error) {
	v.mutex.RLock()
	defer v.mutex.RUnlock()

	session, exists := v.sessions[sessionID]
	if !exists || !session.Active {
		return nil, fmt.Errorf("会话不存在或已关闭")
	}

	return session, nil
}

// CloseSession 关闭VNC会话
func (v *VNCManager) CloseSession(sessionID string) error {
	v.mutex.Lock()
	defer v.mutex.Unlock()

	session, exists := v.sessions[sessionID]
	if !exists {
		return fmt.Errorf("会话不存在")
	}

	// 关闭WebSocket连接
	if session.WSConn != nil {
		session.WSConn.Close()
	}

	// 关闭VNC连接
	if session.VNCConn != nil {
		session.VNCConn.Close()
	}

	// 释放端口
	if session.VNCPort > 0 {
		delete(v.allocatedPorts, session.VNCPort)
	}

	// 标记会话为非活跃状态
	session.Active = false

	// 删除会话
	delete(v.sessions, sessionID)

	v.logger.Infof("关闭VNC会话: %s", sessionID)
	return nil
}

// HandleWebSocket 处理WebSocket连接
func (v *VNCManager) HandleWebSocket(c *gin.Context) {
	sessionID := c.Query("session_id")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少session_id参数"})
		return
	}

	session, err := v.GetSession(sessionID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	// 升级HTTP连接到WebSocket
	conn, err := v.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		v.logger.Errorf("WebSocket升级失败: %v", err)
		return
	}

	// 设置WebSocket连接
	session.WSConn = conn
	session.LastActivity = time.Now()

	// 启动处理协程
	go v.handleVNCConnection(session)
	go v.handleWebSocketMessages(session)
}

// handleVNCConnection 处理VNC连接
func (v *VNCManager) handleVNCConnection(session *VNCSession) {
	defer func() {
		v.CloseSession(session.ID)
	}()

	// 连接到实际的VNC服务器
	vncAddr := fmt.Sprintf("127.0.0.1:%d", session.VNCPort)
	vncConn, err := net.Dial("tcp", vncAddr)
	if err != nil {
		v.logger.Errorf("连接VNC服务器失败: %v", err)
		return
	}

	session.VNCConn = vncConn

	// 启动数据转发
	go v.forwardData(session.VNCConn, session.WSConn, "vnc->ws")
	go v.forwardData(session.WSConn, session.VNCConn, "ws->vnc")
}

// handleWebSocketMessages 处理WebSocket消息
func (v *VNCManager) handleWebSocketMessages(session *VNCSession) {
	defer v.CloseSession(session.ID)

	for {
		var msg VNCMessage
		err := session.WSConn.ReadJSON(&msg)
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				v.logger.Errorf("WebSocket读取错误: %v", err)
			}
			break
		}

		session.LastActivity = time.Now()

		// 处理不同类型的消息
		switch msg.Type {
		case "ping":
			v.handlePing(session, msg)
		case "resize":
			v.handleResize(session, msg)
		case "clipboard":
			v.handleClipboard(session, msg)
		default:
			v.logger.Debugf("未知消息类型: %s", msg.Type)
		}
	}
}

// forwardData 转发数据
func (v *VNCManager) forwardData(src net.Conn, dst net.Conn, direction string) {
	buffer := make([]byte, 4096)

	for {
		n, err := src.Read(buffer)
		if err != nil {
			v.logger.Debugf("%s 读取错误: %v", direction, err)
			return
		}

		if wsConn, ok := dst.(*websocket.Conn); ok {
			// 如果目标是WebSocket，发送二进制数据
			err = wsConn.WriteMessage(websocket.BinaryMessage, buffer[:n])
			if err != nil {
				v.logger.Debugf("%s WebSocket写入错误: %v", direction, err)
				return
			}
		} else {
			// 如果目标是TCP连接
			_, err = dst.Write(buffer[:n])
			if err != nil {
				v.logger.Debugf("%s TCP写入错误: %v", direction, err)
				return
			}
		}
	}
}

// handlePing 处理ping消息
func (v *VNCManager) handlePing(session *VNCSession, msg VNCMessage) {
	response := VNCMessage{
		Type: "pong",
		Data: msg.Data,
		Time: time.Now(),
	}

	session.WSConn.WriteJSON(response)
}

// handleResize 处理屏幕尺寸调整
func (v *VNCManager) handleResize(session *VNCSession, msg VNCMessage) {
	// 实现屏幕尺寸调整逻辑
	v.logger.Infof("会话 %s 屏幕尺寸调整: %v", session.ID, msg.Data)
}

// handleClipboard 处理剪贴板消息
func (v *VNCManager) handleClipboard(session *VNCSession, msg VNCMessage) {
	// 实现剪贴板同步逻辑
	v.logger.Infof("会话 %s 剪贴板更新: %v", session.ID, msg.Data)
}

// allocatePort 分配端口
func (v *VNCManager) allocatePort() (int, error) {
	// 解析端口范围
	start, end, err := v.parsePortRange(v.config.PortRange)
	if err != nil {
		return 0, err
	}

	// 寻找可用端口
	for port := start; port <= end; port++ {
		if !v.allocatedPorts[port] && !v.isPortInUse(port) {
			v.allocatedPorts[port] = true
			return port, nil
		}
	}

	return 0, fmt.Errorf("没有可用端口")
}

// parsePortRange 解析端口范围
func (v *VNCManager) parsePortRange(portRange string) (int, int, error) {
	// 简单实现，假设格式为"5900-5999"
	start := 5900
	end := 5999

	// 这里应该实现更复杂的解析逻辑
	return start, end, nil
}

// isPortInUse 检查端口是否被占用
func (v *VNCManager) isPortInUse(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf(":%d", port), 100*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// generateSessionID 生成会话ID
func (v *VNCManager) generateSessionID() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	return fmt.Sprintf("%x", bytes)
}

// CleanupSessions 清理过期会话
func (v *VNCManager) CleanupSessions() {
	v.mutex.Lock()
	defer v.mutex.Unlock()

	now := time.Now()
	timeout := time.Duration(v.config.SessionTimeout) * time.Second

	for sessionID, session := range v.sessions {
		if !session.Active {
			delete(v.sessions, sessionID)
			continue
		}

		if now.Sub(session.LastActivity) > timeout {
			v.logger.Infof("清理过期会话: %s", sessionID)
			v.closeSessionUnsafe(sessionID)
		}
	}
}

// closeSessionUnsafe 非线程安全的关闭会话
func (v *VNCManager) closeSessionUnsafe(sessionID string) {
	session := v.sessions[sessionID]
	if session == nil {
		return
	}

	if session.WSConn != nil {
		session.WSConn.Close()
	}
	if session.VNCConn != nil {
		session.VNCConn.Close()
	}
	if session.VNCPort > 0 {
		delete(v.allocatedPorts, session.VNCPort)
	}

	delete(v.sessions, sessionID)
}

// GetSessions 获取所有会话
func (v *VNCManager) GetSessions() map[string]*VNCSession {
	v.mutex.RLock()
	defer v.mutex.RUnlock()

	// 返回副本
	result := make(map[string]*VNCSession)
	for id, session := range v.sessions {
		sessionCopy := *session
		result[id] = &sessionCopy
	}
	return result
}

// GetActiveSessions 获取活跃会话
func (v *VNCManager) GetActiveSessions() map[string]*VNCSession {
	v.mutex.RLock()
	defer v.mutex.RUnlock()

	result := make(map[string]*VNCSession)
	for id, session := range v.sessions {
		if session.Active {
			sessionCopy := *session
			result[id] = &sessionCopy
		}
	}
	return result
}

// GetStats 获取VNC统计信息
func (v *VNCManager) GetStats() map[string]interface{} {
	v.mutex.RLock()
	defer v.mutex.RUnlock()

	activeCount := 0
	inactiveCount := 0

	for _, session := range v.sessions {
		if session.Active {
			activeCount++
		} else {
			inactiveCount++
		}
	}

	return map[string]interface{}{
		"total_sessions":   len(v.sessions),
		"active_sessions":  activeCount,
		"inactive_sessions": inactiveCount,
		"allocated_ports":  len(v.allocatedPorts),
		"max_sessions":     v.config.MaxSessions,
	}
}

// GenerateVNCURL 生成VNC访问URL
func (v *VNCManager) GenerateVNCURL(session *VNCSession) string {
	// 生成WebSocket URL
	return fmt.Sprintf("ws://localhost:1880/api/v1/vnc/ws?session_id=%s", session.ID)
}

// StartCleanupRoutine 启动清理协程
func (v *VNCManager) StartCleanupRoutine() {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()

		for range ticker.C {
			v.CleanupSessions()
		}
	}()
}