package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/viper"
)

// Config 应用配置结构体
type Config struct {
	Server   ServerConfig   `mapstructure:"server"`
	Database DatabaseConfig `mapstructure:"database"`
	Log      LogConfig      `mapstructure:"log"`
	Security SecurityConfig `mapstructure:"security"`
	VNC      VNCConfig      `mapstructure:"vnc"`
	Network  NetworkConfig  `mapstructure:"network"`
}

// ServerConfig 服务器配置
type ServerConfig struct {
	Host         string `mapstructure:"host"`
	Port         int    `mapstructure:"port"`
	Mode         string `mapstructure:"mode"`
	ReadTimeout  int    `mapstructure:"read_timeout"`
	WriteTimeout int    `mapstructure:"write_timeout"`
	StaticPath   string `mapstructure:"static_path"`
	TemplatePath string `mapstructure:"template_path"`
}

// DatabaseConfig 数据库配置
type DatabaseConfig struct {
	Type     string `mapstructure:"type"`
	Path     string `mapstructure:"path"`
	MaxConn  int    `mapstructure:"max_conn"`
	IdleConn int    `mapstructure:"idle_conn"`
}

// LogConfig 日志配置
type LogConfig struct {
	Level      string `mapstructure:"level"`
	Format     string `mapstructure:"format"`
	Output     string `mapstructure:"output"`
	MaxSize    int    `mapstructure:"max_size"`
	MaxAge     int    `mapstructure:"max_age"`
	MaxBackups int    `mapstructure:"max_backups"`
	Compress   bool   `mapstructure:"compress"`
}

// SecurityConfig 安全配置
type SecurityConfig struct {
	JWTSecret     string `mapstructure:"jwt_secret"`
	TokenExpire   int    `mapstructure:"token_expire"`
	CookieSecret  string `mapstructure:"cookie_secret"`
	EnableHTTPS   bool   `mapstructure:"enable_https"`
	CertFile      string `mapstructure:"cert_file"`
	KeyFile       string `mapstructure:"key_file"`
	AllowOrigins  string `mapstructure:"allow_origins"`
	AllowMethods  string `mapstructure:"allow_methods"`
	AllowHeaders  string `mapstructure:"allow_headers"`
}

// VNCConfig VNC配置
type VNCConfig struct {
	Enable       bool   `mapstructure:"enable"`
	PortRange    string `mapstructure:"port_range"`
	MaxSessions  int    `mapstructure:"max_sessions"`
	SessionTimeout int  `mapstructure:"session_timeout"`
	Encryption   bool   `mapstructure:"encryption"`
	Recording    bool   `mapstructure:"recording"`
}

// NetworkConfig 网络配置
type NetworkConfig struct {
	EnableProxy bool     `mapstructure:"enable_proxy"`
	ProxyPort   int      `mapstructure:"proxy_port"`
	DNSServers  []string `mapstructure:"dns_servers"`
	Timeout     int      `mapstructure:"timeout"`
	EnableIPv6  bool     `mapstructure:"enable_ipv6"`
}

// LoadConfig 加载配置文件
func LoadConfig(configPath string) (*Config, error) {
	// 设置配置文件名和路径
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	
	if configPath != "" {
		viper.AddConfigPath(configPath)
	} else {
		// 默认配置文件路径
		viper.AddConfigPath("./config")
		viper.AddConfigPath(".")
		viper.AddConfigPath("/etc/openidcs")
	}

	// 设置环境变量前缀
	viper.SetEnvPrefix("OPENIDCS")
	viper.AutomaticEnv()

	// 设置默认值
	setDefaults()

	// 读取配置文件
	if err := viper.ReadInConfig(); err != nil {
		// 如果配置文件不存在，使用默认配置
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			fmt.Printf("配置文件未找到，使用默认配置: %v\n", err)
		} else {
			return nil, fmt.Errorf("读取配置文件失败: %w", err)
		}
	}

	// 解析配置到结构体
	var config Config
	if err := viper.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	// 验证配置
	if err := validateConfig(&config); err != nil {
		return nil, fmt.Errorf("配置验证失败: %w", err)
	}

	return &config, nil
}

// setDefaults 设置默认配置值
func setDefaults() {
	// 服务器配置默认值
	viper.SetDefault("server.host", "0.0.0.0")
	viper.SetDefault("server.port", 1880)
	viper.SetDefault("server.mode", "debug")
	viper.SetDefault("server.read_timeout", 30)
	viper.SetDefault("server.write_timeout", 30)
	viper.SetDefault("server.static_path", "./web/static")
	viper.SetDefault("server.template_path", "./web/templates")

	// 数据库配置默认值
	viper.SetDefault("database.type", "sqlite")
	viper.SetDefault("database.path", "./data/hostmanage.db")
	viper.SetDefault("database.max_conn", 10)
	viper.SetDefault("database.idle_conn", 5)

	// 日志配置默认值
	viper.SetDefault("log.level", "info")
	viper.SetDefault("log.format", "json")
	viper.SetDefault("log.output", "./data/logs")
	viper.SetDefault("log.max_size", 100)
	viper.SetDefault("log.max_age", 7)
	viper.SetDefault("log.max_backups", 10)
	viper.SetDefault("log.compress", true)

	// 安全配置默认值
	viper.SetDefault("security.jwt_secret", "openidcs-secret-key")
	viper.SetDefault("security.token_expire", 24)
	viper.SetDefault("security.cookie_secret", "openidcs-cookie-secret")
	viper.SetDefault("security.enable_https", false)
	viper.SetDefault("security.allow_origins", "*")
	viper.SetDefault("security.allow_methods", "GET,POST,PUT,DELETE,OPTIONS")
	viper.SetDefault("security.allow_headers", "Origin,Content-Type,Authorization")

	// VNC配置默认值
	viper.SetDefault("vnc.enable", true)
	viper.SetDefault("vnc.port_range", "5900-5999")
	viper.SetDefault("vnc.max_sessions", 100)
	viper.SetDefault("vnc.session_timeout", 3600)
	viper.SetDefault("vnc.encryption", true)
	viper.SetDefault("vnc.recording", false)

	// 网络配置默认值
	viper.SetDefault("network.enable_proxy", true)
	viper.SetDefault("network.proxy_port", 8080)
	viper.SetDefault("network.dns_servers", []string{"119.29.29.29", "223.5.5.5"})
	viper.SetDefault("network.timeout", 30)
	viper.SetDefault("network.enable_ipv6", false)
}

// validateConfig 验证配置
func validateConfig(config *Config) error {
	// 验证服务器配置
	if config.Server.Port <= 0 || config.Server.Port > 65535 {
		return fmt.Errorf("无效的服务器端口: %d", config.Server.Port)
	}

	// 验证数据库配置
	if config.Database.Path == "" {
		return fmt.Errorf("数据库路径不能为空")
	}

	// 确保数据库目录存在
	dbDir := filepath.Dir(config.Database.Path)
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		return fmt.Errorf("创建数据库目录失败: %w", err)
	}

	// 确保日志目录存在
	if config.Log.Output != "" && config.Log.Output != "stdout" {
		if err := os.MkdirAll(config.Log.Output, 0755); err != nil {
			return fmt.Errorf("创建日志目录失败: %w", err)
		}
	}

	// 验证安全配置
	if config.Security.JWTSecret == "" {
		return fmt.Errorf("JWT密钥不能为空")
	}

	return nil
}

// SaveConfig 保存配置到文件
func SaveConfig(config *Config, configPath string) error {
	viper.Set("server", config.Server)
	viper.Set("database", config.Database)
	viper.Set("log", config.Log)
	viper.Set("security", config.Security)
	viper.Set("vnc", config.VNC)
	viper.Set("network", config.Network)

	if err := viper.WriteConfigAs(configPath); err != nil {
		return fmt.Errorf("保存配置文件失败: %w", err)
	}

	return nil
}

// GetConfigFile 获取配置文件路径
func GetConfigFile() string {
	if configFile := viper.ConfigFileUsed(); configFile != "" {
		return configFile
	}
	return "config.yaml"
}