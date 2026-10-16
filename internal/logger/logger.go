// Package logger 分层分级日志系统。
//
// 一条日志会按级别同时投递到三个通道：
//
//	通道        默认门槛   用途
//	控制台        INFO      运维在终端实时看到「谁在什么主机上做了什么」
//	日志文件      DEBUG     完整留痕（按主机分文件 + 全量 hostagent.log），超 10MB 自动压缩轮转
//	数据库        INFO      前端「系统日志」页面展示与审计，字段结构化、可过滤
//
// 调用方式（推荐链式，字段越全日志越有意义）：
//
//	app.Logger.Event("vm", "创建虚拟机").
//	    User(uc.Username).Host(hsName).VM(uuid, cfg.OsName).
//	    Detail("cpu", cfg.CpuNum).Detail("mem", cfg.MemNum).
//	    Info("已提交创建任务 %s", taskID)
//
// 兼容历史调用：Log / Info / Error / Warning 仍然可用。
package logger

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
)

const (
	maxLogFileSize = 10 * 1024 * 1024 // 10MB
	// unifiedLogName 全量日志文件名：所有模块按时间顺序落入同一文件，
	// 便于排查「一次操作跨模块」的完整链路。
	unifiedLogName = "hostagent.log"
)

// knownModules 内部功能域名。
//
// 历史代码习惯把功能域名当作 hs_name 传进来（如 "cmd"、"accesskey"），
// 这里据此推断模块名，避免日志里出现「未知」的操作来源。
var knownModules = map[string]string{
	"system":    "system",
	"auth":      "auth",
	"user":      "user",
	"cmd":       "cmd",
	"accesskey": "accesskey",
	"temptoken": "temptoken",
	"sync":      "sync",
	"task":      "task",
	"scheduler": "scheduler",
	"server":    "server",
	"vm":        "vm",
	"host":      "host",
	"logger":    "logger",
	"quota":     "quota",
	"nat":       "nat",
	"proxy":     "proxy",
	"backup":    "backup",
}

// Entry 一条结构化日志。
type Entry struct {
	Time     time.Time              // 发生时间
	Level    Level                  // 级别
	Module   string                 // 模块：vm / task / host / auth ...
	Action   string                 // 操作：创建虚拟机 / 任务完成 ...
	Message  string                 // 人类可读的描述
	Operator string                 // 操作人（用户名；系统动作为空）
	HsName   string                 // 主机名
	VMUUID   string                 // 虚拟机 UUID
	VMName   string                 // 虚拟机显示名
	ClientIP string                 // 客户端 IP
	Success  bool                   // 是否成功
	Duration time.Duration          // 耗时
	Err      error                  // 错误对象
	Detail   map[string]interface{} // 附加结构化字段
}

// Logger 日志管理器
type Logger struct {
	db      *data.DB
	dataDir string
	files   map[string]*os.File // 文件键 -> 日志文件（"" 表示全量日志）
	mu      sync.Mutex

	consoleLevel Level
	fileLevel    Level
	dbLevel      Level
	console      bool
	color        bool
}

// NewLogger 创建日志管理器。
//
// 默认门槛：控制台 INFO、文件 DEBUG、数据库 INFO（可通过 Configure 调整）。
func NewLogger(db *data.DB, dataDir string) *Logger {
	return &Logger{
		db:           db,
		dataDir:      dataDir,
		files:        make(map[string]*os.File),
		consoleLevel: LevelInfo,
		fileLevel:    LevelDebug,
		dbLevel:      LevelInfo,
		console:      true,
		color:        detectColor(),
	}
}

// Configure 设置三个通道的级别门槛（main 中由 --log-level 等参数注入）。
func (l *Logger) Configure(console, file, db Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.consoleLevel = console
	l.fileLevel = file
	l.dbLevel = db
}

// SetConsole 打开/关闭控制台输出（测试或纯服务部署时可关闭）。
func (l *Logger) SetConsole(enabled bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.console = enabled
}

// Levels 返回当前三个通道的门槛（用于启动横幅展示）。
func (l *Logger) Levels() (console, file, db Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.consoleLevel, l.fileLevel, l.dbLevel
}

// ============================================================
// 链式构建器
// ============================================================

// Event 日志事件构建器：先累积上下文，最后调用 Info/Warn/Errorf 等落地。
type Event struct {
	l *Logger
	e Entry
}

// Event 创建一个日志事件（默认级别 INFO、结果成功）。
func (l *Logger) Event(module, action string) *Event {
	return &Event{
		l: l,
		e: Entry{
			Time:    time.Now(),
			Level:   LevelInfo,
			Module:  module,
			Action:  action,
			Success: true,
			Detail:  map[string]interface{}{},
		},
	}
}

// EventFor 以主机名创建事件，自动推断模块（伪主机名 → 功能模块）。
func (l *Logger) EventFor(hsName, action string) *Event {
	return l.Event(moduleOf(hsName), action).Host(hsName)
}

// User 设置操作人。
func (e *Event) User(username string) *Event {
	e.e.Operator = strings.TrimSpace(username)
	return e
}

// Host 设置主机名。
func (e *Event) Host(hsName string) *Event {
	e.e.HsName = strings.TrimSpace(hsName)
	return e
}

// VM 设置虚拟机标识。
func (e *Event) VM(uuid, name string) *Event {
	e.e.VMUUID = strings.TrimSpace(uuid)
	e.e.VMName = strings.TrimSpace(name)
	return e
}

// IP 设置客户端 IP。
func (e *Event) IP(ip string) *Event {
	e.e.ClientIP = strings.TrimSpace(ip)
	return e
}

// Err 绑定错误：自动置为失败结果，并写入 detail.error。
func (e *Event) Err(err error) *Event {
	if err != nil {
		e.e.Err = err
		e.e.Success = false
	}
	return e
}

// Duration 设置耗时。
func (e *Event) Duration(d time.Duration) *Event {
	e.e.Duration = d
	return e
}

// Detail 追加一个结构化字段（key 为空或 value 为 nil 时忽略）。
func (e *Event) Detail(key string, value interface{}) *Event {
	if strings.TrimSpace(key) == "" || value == nil {
		return e
	}
	if e.e.Detail == nil {
		e.e.Detail = map[string]interface{}{}
	}
	e.e.Detail[key] = value
	return e
}

// Details 批量追加结构化字段。
func (e *Event) Details(fields map[string]interface{}) *Event {
	for k, v := range fields {
		e.Detail(k, v)
	}
	return e
}

// Fail 标记本次操作失败（不绑定具体 error 时使用）。
func (e *Event) Fail() *Event {
	e.e.Success = false
	return e
}

// emit 落地（format 支持 fmt 风格占位符）。
func (e *Event) emit(level Level, format string, args ...interface{}) {
	if e == nil || e.l == nil {
		return
	}
	e.e.Level = level
	if e.e.Action == "" {
		e.e.Action = "未命名操作"
	}
	if e.e.Module == "" {
		e.e.Module = "system"
	}
	e.e.Message = formatMessage(format, args...)
	if e.e.Time.IsZero() {
		e.e.Time = time.Now()
	}
	e.l.emit(e.e)
}

// Emit 按指定级别落地，且不修改「成功/失败」语义。
//
// 用于「操作成功但影响面大、需要重点留痕」的场景，
// 例如删除主机、重装系统：级别用 WARNING 让它在终端和数据库都显眼，
// 但结果仍然是成功的。
func (e *Event) Emit(level Level, format string, args ...interface{}) {
	e.emit(level, format, args...)
}

// Debug 记录 DEBUG 日志（默认只落文件）。
func (e *Event) Debug(format string, args ...interface{}) { e.emit(LevelDebug, format, args...) }

// Info 记录 INFO 日志（成功结果）。
func (e *Event) Info(format string, args ...interface{}) { e.emit(LevelInfo, format, args...) }

// Warn 记录 WARNING 日志（失败/降级但流程可继续）。
func (e *Event) Warn(format string, args ...interface{}) {
	e.e.Success = false
	e.emit(LevelWarn, format, args...)
}

// Errorf 记录 ERROR 日志。
func (e *Event) Errorf(format string, args ...interface{}) {
	e.e.Success = false
	e.emit(LevelError, format, args...)
}

// Fatalf 记录 FATAL 日志（调用方负责终止进程）。
func (e *Event) Fatalf(format string, args ...interface{}) {
	e.e.Success = false
	e.emit(LevelFatal, format, args...)
}

// ============================================================
// 兼容接口
// ============================================================

// Log 记录日志（同时写入数据库和文件）。
//
// 兼容历史签名：level 支持 "INFO"/"WARN"/"WARNING"/"ERROR"/"DEBUG"。
func (l *Logger) Log(hsName, level, action, message string, success bool) {
	ev := l.Event(moduleOf(hsName), action).Host(hsName)
	if !success {
		ev.Fail()
	}
	ev.emit(ParseLevel(level), "%s", message)
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

// Debug 记录DEBUG日志（仅文件）
func (l *Logger) Debug(hsName, action, message string) {
	l.Log(hsName, "DEBUG", action, message, true)
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
func (l *Logger) LogJSON(hsName, level string, payload interface{}) {
	ev := l.Event(moduleOf(hsName), "JSON").Host(hsName)
	if fields, ok := payload.(map[string]interface{}); ok {
		ev.Details(fields)
		if action, ok := fields["action"].(string); ok && action != "" {
			ev.e.Action = action
		}
		if message, ok := fields["message"].(string); ok {
			ev.emit(ParseLevel(level), "%s", message)
			return
		}
	}
	raw, _ := json.Marshal(payload)
	ev.emit(ParseLevel(level), "%s", string(raw))
}

// ============================================================
// 输出通道
// ============================================================

// emit 按级别门槛把日志投递到控制台 / 文件 / 数据库。
func (l *Logger) emit(e Entry) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Module == "" {
		e.Module = "system"
	}
	if e.Err != nil {
		if e.Detail == nil {
			e.Detail = map[string]interface{}{}
		}
		e.Detail["error"] = e.Err.Error()
		e.Success = false
	}

	l.mu.Lock()
	consoleOn := l.console && e.Level.AtLeast(l.consoleLevel)
	fileOn := e.Level.AtLeast(l.fileLevel)
	dbOn := e.Level.AtLeast(l.dbLevel)
	color := l.color
	l.mu.Unlock()

	line := formatEntry(e)
	if consoleOn {
		l.writeConsole(e, line, color)
	}
	if fileOn {
		l.writeFiles(e, line)
	}
	if dbOn {
		l.writeDB(e)
	}
}

// writeConsole 输出到终端（WARNING 及以上走 stderr）。
func (l *Logger) writeConsole(e Entry, line string, color bool) {
	out := os.Stdout
	if e.Level >= LevelWarn {
		out = os.Stderr
	}
	if color {
		line = e.Level.colorCode() + line + "\x1b[0m"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := fmt.Fprintln(out, line); err != nil {
		// 控制台写失败无需再记录，避免递归
		_ = err
	}
}

// writeFiles 写入「按主机/模块分文件」与「全量日志文件」。
func (l *Logger) writeFiles(e Entry, line string) {
	line += "\n"
	// 全量文件：所有级别按时间顺序，排查跨模块链路
	l.appendToFile("", unifiedLogName, line)
	// 分域文件：主机用主机名，功能域用模块名
	if key := fileKeyOf(e); key != "" {
		l.appendToFile(key, fmt.Sprintf("log-%s.log", sanitizeFileName(key)), line)
	}
}

// appendToFile 追加一行到指定日志文件，超限时轮转。
func (l *Logger) appendToFile(key, fileName, line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writeLocked(key, fileName, line)
}

// writeLocked 在已持锁状态下写入文件。
func (l *Logger) writeLocked(key, fileName, line string) {
	f, err := l.getOrOpenFile(key, fileName)
	if err != nil {
		log.Printf("[logger] 打开日志文件失败(%s): %v", fileName, err)
		return
	}
	if _, err := f.WriteString(line); err != nil {
		log.Printf("[logger] 写入日志文件失败(%s): %v", fileName, err)
	}
	if info, err := f.Stat(); err == nil && info.Size() >= maxLogFileSize {
		l.rotateFile(key, fileName)
	}
}

// getOrOpenFile 获取或打开日志文件（需持有 l.mu）。
func (l *Logger) getOrOpenFile(key, fileName string) (*os.File, error) {
	if f, ok := l.files[key]; ok {
		return f, nil
	}
	if l.dataDir == "" {
		return nil, fmt.Errorf("数据目录为空")
	}
	if err := os.MkdirAll(l.dataDir, 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(l.dataDir, fileName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	l.files[key] = f
	return f, nil
}

// rotateFile 轮转日志文件（压缩为zip）。
//
// 关键顺序：先把旧文件重命名，再异步压缩。
// 原实现是「关闭句柄 → 异步压缩原路径 → 删除原路径」，
// 期间新的写入会以同一路径重新创建文件，
// 异步任务随后执行的 os.Remove(logPath) 会删掉正在写入的活跃日志。
func (l *Logger) rotateFile(key, fileName string) {
	// 调用方已持有 l.mu，这里只操作内存映射与文件系统
	f, ok := l.files[key]
	if !ok {
		return
	}
	logPath := filepath.Join(l.dataDir, fileName)
	// 使用纳秒后缀避免同秒轮转时归档名冲突
	rotated := fmt.Sprintf("%s.%d", logPath, time.Now().UnixNano())

	_ = f.Close()
	delete(l.files, key)

	if err := os.Rename(logPath, rotated); err != nil {
		log.Printf("[logger] 轮转重命名失败(%s): %v", fileName, err)
		return
	}

	// 异步只处理已改名的旧文件，活跃路径始终空闲
	go func() {
		zipPath := rotated + ".zip"
		if err := compressFile(rotated, zipPath); err != nil {
			log.Printf("[logger] 压缩日志文件失败: %v", err)
			return
		}
		if err := os.Remove(rotated); err != nil {
			log.Printf("[logger] 删除已归档日志失败: %v", err)
		}
	}()
}

// writeDB 写入数据库（结构化字段 + 兼容前端的 log_data JSON）。
func (l *Logger) writeDB(e Entry) {
	if l.db == nil {
		return
	}

	payload := map[string]interface{}{
		"action":  e.Action,
		"message": e.Message,
		"success": e.Success,
		"time":    e.Time.Format("2006-01-02 15:04:05"),
		"level":   e.Level.String(),
		"module":  e.Module,
	}
	if e.Operator != "" {
		payload["operator"] = e.Operator
	}
	if e.HsName != "" {
		payload["hs_name"] = e.HsName
	}
	if e.VMUUID != "" {
		payload["vm_uuid"] = e.VMUUID
	}
	if e.VMName != "" {
		payload["vm_name"] = e.VMName
	}
	if e.ClientIP != "" {
		payload["client_ip"] = e.ClientIP
	}
	if e.Duration > 0 {
		payload["duration_ms"] = e.Duration.Milliseconds()
	}
	if len(e.Detail) > 0 {
		payload["detail"] = e.Detail
	}

	dataJSON, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[logger] 序列化日志失败: %v", err)
		return
	}
	detailJSON, _ := json.Marshal(e.Detail)

	rec := data.LogRecord{
		HsName:     e.HsName,
		Level:      e.Level.String(),
		Action:     e.Action,
		Message:    e.Message,
		Module:     e.Module,
		Operator:   e.Operator,
		VMUUID:     e.VMUUID,
		VMName:     e.VMName,
		ClientIP:   e.ClientIP,
		Success:    e.Success,
		DurationMS: e.Duration.Milliseconds(),
		Detail:     string(detailJSON),
		LogData:    string(dataJSON),
	}

	if err := l.db.AddLogRecord(rec); err != nil {
		// 旧库可能仍带 hs_name 外键（伪功能域名如 cmd/accesskey 不在 hs_config 中）：
		// 退化为不带主机名的记录，确保审计日志不会静默丢失。
		if strings.Contains(err.Error(), "FOREIGN KEY") && rec.HsName != "" {
			retry := rec
			retry.HsName = ""
			if retryErr := l.db.AddLogRecord(retry); retryErr == nil {
				return
			}
		}
		log.Printf("[logger] 写入数据库失败: %v", err)
	}
}

// ============================================================
// 查询与维护
// ============================================================

// GetLogs 获取日志列表（兼容旧签名：单级别精确匹配）。
func (l *Logger) GetLogs(hsName, level string, limit int) ([]map[string]interface{}, error) {
	return l.db.QueryLoggers(data.LogQuery{
		HsName: hsName,
		Levels: splitLevels(level),
		Limit:  limit,
	})
}

// QueryLogs 按多条件查询日志。
func (l *Logger) QueryLogs(q data.LogQuery) ([]map[string]interface{}, error) {
	return l.db.QueryLoggers(q)
}

// Stats 返回日志分级/分模块统计。
func (l *Logger) Stats(days int) (map[string]interface{}, error) {
	return l.db.LogStats(days)
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

// ============================================================
// 辅助函数
// ============================================================

// formatEntry 生成终端与文件共用的单行日志。
func formatEntry(e Entry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s [%s] [%s] %s",
		e.Time.Format("2006-01-02 15:04:05.000"),
		padRight(e.Level.String(), 7),
		padRight(e.Module, 10),
		e.Action)
	if e.Message != "" {
		b.WriteString(" | ")
		b.WriteString(e.Message)
	}

	kvs := make([]string, 0, 6)
	if e.Operator != "" {
		kvs = append(kvs, "操作人="+e.Operator)
	}
	if e.HsName != "" {
		kvs = append(kvs, "主机="+e.HsName)
	}
	if e.VMUUID != "" {
		vm := e.VMUUID
		if e.VMName != "" && e.VMName != e.VMUUID {
			vm = e.VMUUID + "(" + e.VMName + ")"
		}
		kvs = append(kvs, "虚拟机="+vm)
	} else if e.VMName != "" {
		kvs = append(kvs, "虚拟机="+e.VMName)
	}
	if e.ClientIP != "" {
		kvs = append(kvs, "来源="+e.ClientIP)
	}
	if !e.Success {
		kvs = append(kvs, "结果=失败")
	}
	if e.Duration > 0 {
		kvs = append(kvs, "耗时="+formatDuration(e.Duration))
	}
	if len(kvs) > 0 {
		b.WriteString(" (")
		b.WriteString(strings.Join(kvs, " "))
		b.WriteString(")")
	}

	if len(e.Detail) > 0 {
		if raw, err := json.Marshal(e.Detail); err == nil {
			b.WriteString(" ")
			b.Write(raw)
		}
	}
	return b.String()
}

// formatDuration 输出人类可读耗时。
func formatDuration(d time.Duration) string {
	switch {
	case d >= time.Minute:
		return fmt.Sprintf("%.1fm", d.Minutes())
	case d >= time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
}

// moduleOf 由 hs_name 推断模块名。
func moduleOf(hsName string) string {
	name := strings.TrimSpace(hsName)
	if name == "" {
		return "system"
	}
	if m, ok := knownModules[strings.ToLower(name)]; ok {
		return m
	}
	return "host"
}

// fileKeyOf 返回该条日志的分域文件键（空表示只落全量日志）。
func fileKeyOf(e Entry) string {
	if e.HsName != "" {
		return e.HsName
	}
	if e.Module == "" || e.Module == "system" {
		return ""
	}
	return e.Module
}

// sanitizeFileName 过滤文件名中的路径分隔符等危险字符。
func sanitizeFileName(name string) string {
	replaced := strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		return r
	}, name)
	if replaced == "" || replaced == "." || replaced == ".." {
		return "unknown"
	}
	return replaced
}

// splitLevels 解析逗号分隔的级别列表（空表示不过滤）。
func splitLevels(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// formatMessage 渲染 fmt 风格消息（无参数时直接返回）。
func formatMessage(format string, args ...interface{}) string {
	if len(args) == 0 {
		return format
	}
	return fmt.Sprintf(format, args...)
}

// detectColor 判断终端是否支持 ANSI 着色。
func detectColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if v := strings.ToLower(os.Getenv("LOG_COLOR")); v == "0" || v == "false" || v == "off" {
		return false
	}
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
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
