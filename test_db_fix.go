package main

import (
	"fmt"
	"log"

	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/config"
	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/database"
	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/models"
	"github.com/sirupsen/logrus"
)

func main() {
	// 创建日志器
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)

	// 创建数据库配置
	dbConfig := &config.DatabaseConfig{
		Type:     "sqlite",
		Path:     "./test.db",
		MaxConn:  10,
		IdleConn: 5,
	}

	// 初始化数据库
	db, err := database.NewDatabase(dbConfig, logger)
	if err != nil {
		log.Fatal("数据库初始化失败:", err)
	}
	defer db.Close()

	// 创建主机配置DAO
	hostConfigDAO := database.NewHostConfigDAO(db)

	// 创建测试配置
	testConfig := &models.HSConfig{
		ServerName: "host0",
		ServerType: "VMWareSetup",
		ServerAddr: "111",
		ServerUser: "222",
		ServerPass: "333",
		FilterName: "444",
		NetworkPub: "",
		SystemMaps: map[string]interface{}{
			"test_key": "test_value",
		},
		ImagesMaps: map[string]interface{}{
			"test_image": "/path/to/image",
		},
		PublicAddr: []string{"192.168.1.100"},
		ExtendData: map[string]interface{}{
			"custom_field": "custom_value",
		},
		ServerDNSS: []string{"8.8.8.8", "8.8.4.4"},
		IPAddrMaps: map[string]interface{}{
			"192.168.1.0/24": "vm_network",
		},
		IPAddrDNSS: []string{"119.29.29.29", "223.5.5.5"},
	}

	// 测试保存配置
	fmt.Println("测试保存主机配置...")
	err = hostConfigDAO.SetHostConfig("host0", testConfig)
	if err != nil {
		log.Fatal("保存配置失败:", err)
	}
	fmt.Println("配置保存成功")

	// 测试读取配置
	fmt.Println("测试读取主机配置...")
	retrievedConfig, err := hostConfigDAO.GetHostConfig("host0")
	if err != nil {
		log.Fatal("读取配置失败:", err)
	}
	if retrievedConfig == nil {
		log.Fatal("配置不存在")
	}

	// 验证数据
	fmt.Printf("验证数据:\n")
	fmt.Printf("  ServerName: %s\n", retrievedConfig.ServerName)
	fmt.Printf("  ServerType: %s\n", retrievedConfig.ServerType)
	fmt.Printf("  ServerAddr: %s\n", retrievedConfig.ServerAddr)
	fmt.Printf("  FilterName: %s\n", retrievedConfig.FilterName)
	fmt.Printf("  NetworkPub: %s\n", retrievedConfig.NetworkPub)
	fmt.Printf("  SystemMaps: %v\n", retrievedConfig.SystemMaps)
	fmt.Printf("  ImagesMaps: %v\n", retrievedConfig.ImagesMaps)
	fmt.Printf("  PublicAddr: %v\n", retrievedConfig.PublicAddr)
	fmt.Printf("  ExtendData: %v\n", retrievedConfig.ExtendData)
	fmt.Printf("  ServerDNSS: %v\n", retrievedConfig.ServerDNSS)
	fmt.Printf("  IPAddrMaps: %v\n", retrievedConfig.IPAddrMaps)
	fmt.Printf("  IPAddrDNSS: %v\n", retrievedConfig.IPAddrDNSS)

	fmt.Println("测试完成")
}