// Package main HostAgent-Go 程序入口
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/api"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/driver"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/logger"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/scheduler"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/security"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/service"
)

var (
	port        int
	dataDir     string
	logLevel    string
	logFileLevel string
	logDBLevel  string
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "hostagent",
		Short: "OpenIDCS HostAgent - 虚拟化统一管理平台后端",
		RunE:  run,
	}

	rootCmd.Flags().IntVar(&port, "port", 1881, "HTTP监听端口")
	rootCmd.Flags().StringVar(&dataDir, "data-dir", "./DataSaving", "数据目录路径")
	// 分层分级：级别越高落地通道越多。
	// 终端只输出 info 及以上（debug 需要显式开启），文件默认保留 debug 细节，
	// 数据库只落 info 及以上，避免无意义的调试噪声污染审计日志。
	rootCmd.Flags().StringVar(&logLevel, "log-level", "info", "终端日志级别 (debug/info/warn/error)")
	rootCmd.Flags().StringVar(&logFileLevel, "log-file-level", "debug", "日志文件级别 (debug/info/warn/error)")
	rootCmd.Flags().StringVar(&logDBLevel, "log-db-level", "info", "写入数据库的日志级别 (debug/info/warn/error/fatal)")

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

	// 装配日志分层门槛：终端 / 文件 / 数据库
	app.Logger.Configure(
		logger.ParseLevel(logLevel),
		logger.ParseLevel(logFileLevel),
		logger.ParseLevel(logDBLevel),
	)
	app.Logger.Event("server", "服务启动").
		Detail("port", port).
		Detail("data_dir", dataDir).
		Detail("console_level", logger.ParseLevel(logLevel).String()).
		Detail("file_level", logger.ParseLevel(logFileLevel).String()).
		Detail("db_level", logger.ParseLevel(logDBLevel).String()).
		Info("HostAgent 启动，监听端口 %d", port)

	// 输出初始Token；首次启动时先生成并持久化，
	// 避免用空口令创建管理员（空口令无法登录，会造成系统不可用）
	token := app.SystemSvc.GetToken()
	if strings.TrimSpace(token) == "" {
		generated, genErr := app.SystemSvc.ResetToken()
		if genErr != nil {
			return fmt.Errorf("生成初始 Bearer Token 失败: %w", genErr)
		}
		token = generated
	}

	// 初始化默认管理员账号（首次启动时创建，初始密码为 Bearer Token）
	if err := db.InitDefaultAdmin(token); err != nil {
		app.Logger.Event("server", "初始化管理员").Err(err).Warn("初始化默认管理员失败: %v", err)
	}

	// 加载所有已启用主机的驱动（对应 Python 启动时构建 host engine 表），
	// 并从中恢复各主机的虚拟机配置：否则重启后必须等待调度器扫描或手动调用
	// /api/system/loader 才能管理主机与虚拟机。
	if err := app.HostSvc.LoadAllDrivers(); err != nil {
		app.Logger.Event("server", "加载主机驱动").Err(err).Warn("加载主机驱动失败: %v", err)
	} else {
		app.Logger.Event("server", "加载主机驱动").Info("已加载全部启用主机的驱动并恢复虚拟机配置")
	}

	consoleLevel, fileLevel, dbLevel := app.Logger.Levels()
	fmt.Printf("\n========================================\n")
	fmt.Printf("  OpenIDCS HostAgent-Go 已启动\n")
	fmt.Printf("  监听端口: %d\n", port)
	fmt.Printf("  数据目录: %s\n", dataDir)
	fmt.Printf("  访问地址: http://localhost:%d\n", port)
	fmt.Printf("  日志级别: 终端=%s 文件=%s 数据库=%s\n", consoleLevel, fileLevel, dbLevel)
	fmt.Printf("  Bearer Token: %s\n", token)
	fmt.Printf("========================================\n\n")

	// 启动VNC代理服务（6080端口）
	vncProxy := api.NewVNCProxyServer(6080)
	vncProxy.Start()

	// 启动SSH终端服务（7681端口）
	sshTerminal := api.NewSSHTerminalServer(7681)
	// 已知主机密钥持久化在数据库全局配置中，默认 TOFU 策略
	hostKeyStore := security.NewHostKeyStore(
		func() (string, error) { return db.GetGlobal("ssh_known_hosts") },
		func(v string) error { return db.SetGlobal("ssh_known_hosts", v) },
	)
	sshTerminal.SetHostKeyStore(hostKeyStore)
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
			if d := app.HostSvc.GetOrCreateDriver(h.HsName); d != nil {
				drivers[h.HsName] = d
			}
		}
		return drivers
	})
	// 注入业务日志：维护与状态采集的异常会按级别落地（失败 → WARNING，细节 → 文件）
	sched.SetLogger(app.Logger)
	// 装配周期性维护：孤立资源清理 + 用户配额全量重算 + 主机间自动同步
	cleanupSvc := service.NewVMCleanupService(db)
	cleanupSvc.SetNATService(app.NATService)
	cleanupSvc.SetProxyService(app.ProxyService)
	sched.SetCleanupService(cleanupSvc)
	// 与 HTTP 接口共用同一配额服务实例，避免两套锁并行导致统计互相覆盖
	sched.SetQuotaService(app.QuotaSvc)
	sched.SetSyncService(service.NewSyncService(db, app.HostSvc))
	// 外部资源补偿 + 未完成删除的断点续做
	sched.SetReconcileService(app.ReconcileSvc)
	sched.SetDeleteWorkflow(app.DeleteWorkflow)
	sched.Start()
	// 注意：这里不使用 defer，改为在关闭流程中按顺序显式调用。
	// defer 会在函数返回时执行，无法保证「先停服务再停调度器」的顺序，
	// 且 Stop 内部会 close 通道，重复调用会 panic。

	// 启动HTTP服务器
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           app.Router,
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
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
	app.Logger.Event("server", "服务关闭").Info("收到退出信号，开始优雅关闭")

	// 统一限时关闭：先停止接收新请求，再停止代理与后台任务，
	// 最后释放数据库与日志句柄，避免依赖阻塞导致进程无法退出。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[server] HTTP 服务关闭超时: %v", err)
	}
	vncProxy.Stop(shutdownCtx)
	sshTerminal.Stop(shutdownCtx)

	if err := app.TaskManager.Stop(); err != nil {
		log.Printf("[server] 任务管理器关闭失败: %v", err)
	}
	sched.Stop()
	app.Logger.Close()

	log.Println("[server] 已安全退出")
	return nil
}
