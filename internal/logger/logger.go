// Package logger 日志系统
package logger

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
)

const (
	maxLogFileSize = 10 * 1024 * 1024 // 10MB
)

// Logger 日志管理器
type Logger struct {
	db      *data.DB
	dataDir string
	files   map[string]*os.File // hs_name -> 日志文件
	mu      sync.Mutex
}

// NewLogger 创建日志管理器
func NewLogger(db *data.DB, dataDir string) *Logger {
	return &Logger{
		db:      db,
		dataDir: dataDir,
		files:   make(map[string]*os.File),
	}
}

// Log 记录日志（同时写入数据库和文件）
func (l *Logger) Log(hsName, level, action, message string, success bool) {
	logData := map[string]interface{}{
		"action":  action,
		"message": message,
		"success": success,
		"time":    time.Now().Format("2006-01-02 15:04:05"),
	}

	// 写入数据库
	if err := l.db.AddLogger(hsName, logData, level); err != nil {
		log.Printf("[logger] 写入数据库失败: %v", err)
	}

	// 写入文件
	l.writeToFile(hsName, level, action, message)
}

// Info 记录INFO日志
func (l *Logger) Info(hsName, action, message string) {
	l.Log(hsName, "INFO", action, message, true)
}

// Error 记录ERROR日志
func (l *Logger) Error(hsName, action, message string) {
	l.Log(hsName, "ERROR", action, message, false)
}

// Warning 记录WARNING日志
func (l *Logger) Warning(hsName, action, message string) {
	l.Log(hsName, "WARNING", action, message, false)
}

// writeToFile 写入日志文件
func (l *Logger) writeToFile(hsName, level, action, message string) {
	if hsName == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := l.getOrOpenFile(hsName)
	if err != nil {
		log.Printf("[logger] 打开日志文件失败: %v", err)
		return
	}

	line := fmt.Sprintf("%s | %s | [%s] %s: %s\n",
		time.Now().Format("2006-01-02 15:04:05"), level, hsName, action, message)
	if _, err := f.WriteString(line); err != nil {
		log.Printf("[logger] 写入日志文件失败: %v", err)
	}

	// 检查文件大小，超限则轮转
	if info, err := f.Stat(); err == nil && info.Size() >= maxLogFileSize {
		l.rotateFile(hsName, f)
	}
}

// getOrOpenFile 获取或打开日志文件
func (l *Logger) getOrOpenFile(hsName string) (*os.File, error) {
	if f, ok := l.files[hsName]; ok {
		return f, nil
	}
	logPath := filepath.Join(l.dataDir, fmt.Sprintf("log-%s.log", hsName))
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	l.files[hsName] = f
	return f, nil
}

// rotateFile 轮转日志文件（压缩为zip）
func (l *Logger) rotateFile(hsName string, f *os.File) {
	logPath := filepath.Join(l.dataDir, fmt.Sprintf("log-%s.log", hsName))
	zipPath := filepath.Join(l.dataDir, fmt.Sprintf("log-%s.%s.log.zip",
		hsName, time.Now().Format("2006-01-02_15-04-05_000000")))

	// 关闭当前文件
	_ = f.Close()
	delete(l.files, hsName)

	// 压缩旧文件
	go func() {
		if err := compressFile(logPath, zipPath); err != nil {
			log.Printf("[logger] 压缩日志文件失败: %v", err)
			return
		}
		// 删除原文件
		_ = os.Remove(logPath)
	}()
}

// compressFile 将文件压缩为zip
func compressFile(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer dst.Close()

	w := zip.NewWriter(dst)
	defer w.Close()

	fw, err := w.Create(filepath.Base(srcPath))
	if err != nil {
		return err
	}
	_, err = io.Copy(fw, src)
	return err
}

// GetLogs 获取日志列表
func (l *Logger) GetLogs(hsName, level string, limit int) ([]map[string]interface{}, error) {
	return l.db.GetLoggers(hsName, level, limit)
}

// ClearLogs 清空日志
func (l *Logger) ClearLogs(hsName string) error {
	return l.db.ClearLoggers(hsName)
}

// DeleteOldLogs 删除N天前的日志
func (l *Logger) DeleteOldLogs(hsName string, days int) (int64, error) {
	return l.db.DeleteOldLoggers(hsName, days)
}

// Close 关闭所有日志文件
func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, f := range l.files {
		_ = f.Close()
	}
	l.files = make(map[string]*os.File)
}

// LogMessage ZMessage日志记录辅助函数
func (l *Logger) LogMessage(hsName string, msg map[string]interface{}) {
	success, _ := msg["success"].(bool)
	action, _ := msg["action"].(string)
	message, _ := msg["message"].(string)
	level := "INFO"
	if !success {
		level = "ERROR"
	}
	l.Log(hsName, level, action, message, success)
}

// LogJSON 记录JSON格式日志
func (l *Logger) LogJSON(hsName, level string, data interface{}) {
	jsonData, _ := json.Marshal(data)
	l.writeToFile(hsName, level, "JSON", string(jsonData))
	_ = l.db.AddLogger(hsName, data, level)
}
