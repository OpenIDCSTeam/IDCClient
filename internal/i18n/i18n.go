// Package i18n 国际化翻译模块，解析 .po 文件并提供翻译查询
package i18n

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Manager 翻译管理器
type Manager struct {
	mu           sync.RWMutex
	translations map[string]map[string]string // lang_code -> msgid -> msgstr
	translateDir string
}

// NewManager 创建翻译管理器并加载所有 .po 文件
func NewManager(translateDir string) (*Manager, error) {
	m := &Manager{
		translations: make(map[string]map[string]string),
		translateDir: translateDir,
	}
	if err := m.LoadAll(); err != nil {
		return m, err
	}
	return m, nil
}

// LoadAll 扫描并加载所有 .po 文件
func (m *Manager) LoadAll() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, err := os.Stat(m.translateDir); os.IsNotExist(err) {
		return fmt.Errorf("翻译目录不存在: %s", m.translateDir)
	}

	entries, err := os.ReadDir(m.translateDir)
	if err != nil {
		return fmt.Errorf("读取翻译目录失败: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".po") {
			continue
		}
		langCode := strings.TrimSuffix(entry.Name(), ".po")
		filePath := filepath.Join(m.translateDir, entry.Name())
		translations, err := parsePOFile(filePath)
		if err != nil {
			continue
		}
		m.translations[langCode] = translations
	}
	return nil
}

// GetLanguages 获取所有可用语言列表
func (m *Manager) GetLanguages() []Language {
	m.mu.RLock()
	defer m.mu.RUnlock()

	langs := make([]Language, 0, len(m.translations))
	for code := range m.translations {
		langs = append(langs, Language{
			Code: code,
			Name: GetLanguageName(code),
		})
	}
	return langs
}

// GetTranslations 获取指定语言的所有翻译
func (m *Manager) GetTranslations(langCode string) map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if t, ok := m.translations[langCode]; ok {
		return t
	}
	return make(map[string]string)
}

// GetText 获取指定 key 的翻译，找不到返回 key 本身
func (m *Manager) GetText(key, langCode string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if t, ok := m.translations[langCode]; ok {
		if val, ok := t[key]; ok && val != "" {
			return val
		}
	}
	return key
}

// Language 语言信息
type Language struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// parsePOFile 解析 .po 文件，返回 msgid -> msgstr 映射
func parsePOFile(filePath string) (map[string]string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("打开文件失败: %w", err)
	}
	defer file.Close()

	translations := make(map[string]string)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB buffer

	var currentMsgID strings.Builder
	var currentMsgStr strings.Builder
	var inMsgID, inMsgStr bool

	flushEntry := func() {
		msgID := currentMsgID.String()
		msgStr := currentMsgStr.String()
		if msgID != "" {
			translations[msgID] = msgStr
		}
		currentMsgID.Reset()
		currentMsgStr.Reset()
		inMsgID = false
		inMsgStr = false
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// 跳过注释和空行
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if strings.HasPrefix(line, "msgid ") {
			flushEntry()
			inMsgID = true
			inMsgStr = false
			currentMsgID.WriteString(extractQuoted(line[6:]))
		} else if strings.HasPrefix(line, "msgstr ") {
			inMsgID = false
			inMsgStr = true
			currentMsgStr.WriteString(extractQuoted(line[7:]))
		} else if strings.HasPrefix(line, "\"") && strings.HasSuffix(line, "\"") {
			if inMsgStr {
				currentMsgStr.WriteString(extractQuoted(line))
			} else if inMsgID {
				currentMsgID.WriteString(extractQuoted(line))
			}
		}
	}
	flushEntry()

	return translations, scanner.Err()
}

// extractQuoted 提取引号内的字符串内容（处理转义）
func extractQuoted(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// GetLanguageName 获取语言全称
func GetLanguageName(code string) string {
	names := map[string]string{
		"zh-cn": "简体中文",
		"zh-tw": "繁體中文",
		"en-us": "English",
		"ja-jp": "日本語",
		"ko-kr": "한국어",
		"ar-ar": "العربية",
		"de-de": "Deutsch",
		"es-es": "Español",
		"fr-fr": "Français",
		"it-it": "Italiano",
		"pt-br": "Português",
		"ru-ru": "Русский",
		"hi-in": "हिन्दी",
		"bn-bd": "বাংলা",
		"ur-pk": "اردو",
	}
	if name, ok := names[code]; ok {
		return name
	}
	return code
}

// 全局管理器
var globalManager *Manager
var once sync.Once

// InitGlobal 初始化全局翻译管理器
func InitGlobal(translateDir string) {
	once.Do(func() {
		var err error
		globalManager, err = NewManager(translateDir)
		if err != nil {
			globalManager = &Manager{
				translations: make(map[string]map[string]string),
				translateDir: translateDir,
			}
		}
	})
}

// T 快捷翻译函数 (translate)
func T(key, langCode string) string {
	if globalManager == nil {
		return key
	}
	return globalManager.GetText(key, langCode)
}

// GetLanguages 获取全局语言列表
func GetLanguages() []Language {
	if globalManager == nil {
		return []Language{{Code: "zh-cn", Name: "简体中文"}}
	}
	return globalManager.GetLanguages()
}

// GetTranslations 获取全局指定语言翻译
func GetTranslations(langCode string) map[string]string {
	if globalManager == nil {
		return make(map[string]string)
	}
	return globalManager.GetTranslations(langCode)
}
