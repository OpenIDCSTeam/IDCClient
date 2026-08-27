// Package main HostAgent-Go 程序入口
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/api"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/driver"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/scheduler"
)

var (
	port    int
	dataDir string
	logLevel string
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "hostagent",
		Short: "OpenIDCS HostAgent - 虚拟化统一管理平台后端",
		RunE:  run,
	}

rootCmd.Flags().IntVar(&port, "port", 1881, "HTTP监听端口")
	rootCmd.Flags().StringVar(&dataDir, "data-dir", "./DataSaving", "数据目录路径")
	rootCmd.Flags().StringVar(&logLevel, "log-level", "info", "日志级别 (debug/info/warn/error)")

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, args []string) error {
	// 初始化数据库
	db, err := data.NewDB(dataDir)
	if err != nil {
		return fmt.Errorf("初始化数据库失败: %w", err)
	}
	defer db.Close()

	// 创建应用
	app := api.NewApp(db, dataDir)

	// 输出初始Token
	token := app.SystemSvc.GetToken()

	// 初始化默认管理员账号（首次启动时创建，初始密码为 Bearer Token）
	if err := db.InitDefaultAdmin(token); err != nil {
		log.Printf("[server] 警告：初始化默认管理员失败: %v", err)
	}

	fmt.Printf("\n========================================\n")
	fmt.Printf("  OpenIDCS HostAgent-Go 已启动\n")
	fmt.Printf("  监听端口: %d\n", port)
	fmt.Printf("  数据目录: %s\n", dataDir)
	fmt.Printf("  访问地址: http://localhost:%d\n", port)
	fmt.Printf("  Bearer Token: %s\n", token)
	fmt.Printf("========================================\n\n")

	// 启动VNC代理服务（6080端口）
	vncProxy := api.NewVNCProxyServer(6080)
	vncProxy.Start()

	// 启动SSH终端服务（7681端口）
	sshTerminal := api.NewSSHTerminalServer(7681)
	sshTerminal.Start()

	// 启动定时任务
	sched := scheduler.NewScheduler(db, func() map[string]driver.HostDriver {
		drivers := make(map[string]driver.HostDriver)
		hosts, err := db.GetAllHosts()
		if err != nil {
			return drivers
		}
		for _, h := range hosts {
			if h.EnableHost == 0 {
				continue
			}
			if d := app.HostSvc.GetDriver(h.HsName); d != nil {
				drivers[h.HsName] = d
			}
		}
		return drivers
	})
	sched.Start()
	defer sched.Stop()

	// 启动HTTP服务器
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: app.Router,
	}

	// 优雅关闭
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("[server] 开始监听 :%d", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[server] 启动失败: %v", err)
		}
	}()

	<-quit
	log.Println("[server] 正在关闭服务器...")
	return nil
}
