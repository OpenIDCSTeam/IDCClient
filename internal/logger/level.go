package logger

import "strings"

// Level 日志级别。
//
// 级别决定一条日志「输出到哪些通道」：
//
//	DEBUG   → 仅文件（细节排查用，量大）
//	INFO    → 控制台 + 文件 + 数据库（关键业务动作）
//	WARNING → 控制台 + 文件 + 数据库（异常但可继续）
//	ERROR   → 控制台(stderr) + 文件 + 数据库
//	FATAL   → 控制台(stderr) + 文件 + 数据库
//
// 也就是「重要的多输出、不重要的少输出」：级别越高，落地通道越多。
type Level int8

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
	LevelFatal
)

// String 返回写入数据库/文件的规范化级别名。
//
// 历史数据里同时存在 "WARN" 与 "WARNING" 两种写法，统一收敛为 "WARNING"。
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARNING"
	case LevelError:
		return "ERROR"
	case LevelFatal:
		return "FATAL"
	default:
		return "INFO"
	}
}

// AtLeast 判断当前级别是否达到输出门槛。
func (l Level) AtLeast(min Level) bool { return l >= min }

// ParseLevel 解析级别名（大小写不敏感，兼容 warn/warning/none/off 等写法）。
// 无法识别时返回 fallback。
func ParseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug", "trace", "verbose":
		return LevelDebug
	case "info", "information", "":
		return LevelInfo
	case "warn", "warning":
		return LevelWarn
	case "error", "err":
		return LevelError
	case "fatal", "panic", "critical":
		return LevelFatal
	default:
		return LevelInfo
	}
}

// CanonicalLevel 把任意来源的级别名规范化为数据库中的取值。
func CanonicalLevel(s string) string {
	return ParseLevel(s).String()
}

// colorCode 返回控制台着色用的 ANSI 序列（仅终端启用时使用）。
func (l Level) colorCode() string {
	switch l {
	case LevelDebug:
		return "\x1b[90m" // 灰
	case LevelInfo:
		return "\x1b[36m" // 青
	case LevelWarn:
		return "\x1b[33m" // 黄
	case LevelError:
		return "\x1b[31m" // 红
	case LevelFatal:
		return "\x1b[35m" // 紫
	default:
		return ""
	}
}

// padRight 右侧补空格到指定宽度（超长则截断），保证终端列对齐。
func padRight(s string, width int) string {
	n := len(s)
	if n == width {
		return s
	}
	if n > width {
		return s[:width]
	}
	return s + strings.Repeat(" ", width-n)
}
