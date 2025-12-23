package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/api"
	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/config"
	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/database"
	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/host"
	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/vm"
	"github.com/sirupsen/logrus"
)

func main() {
	app := &Application{}
	if err := app.Run(); err != nil {
		logrus.Fatalf("应用程序运行失败: %v", err)
	}
}

// Application 应用程序
type Application struct {
	config      *config.Config
	logger      *logrus.Logger
	db          *database.Database
	hostManager *host.HostManager
	vmManager   *vm.VMManager
	restManager *api.RestManager
	router      *api.Router
	server      *http.Server
	cronTicker  *time.Ticker
	cronDone    chan bool
}

// Run 运行应用程序
func (app *Application) Run() error {
	// 初始化日志
	app.logger = app.initLogger()
	
	// 加载配置
	cfg, err := config.LoadConfig("")
	if err != nil {
		return fmt.Errorf("加载配置失败: %w", err)
	}
	app.config = cfg

	app.logger.Info("启动 OpenIDCS 客户端 v1.0.0")

	// 初始化数据库
	db, err := database.NewDatabase(&cfg.Database, app.logger)
	if err != nil {
		return fmt.Errorf("初始化数据库失败: %w", err)
	}
	app.db = db

	// 初始化管理器
	app.hostManager = host.NewHostManager(db, app.logger)
	app.vmManager = vm.NewVMManager(app.hostManager, app.logger)

	// 初始化API管理器和路由
	app.restManager = api.NewRestManager(app.hostManager, app.vmManager, app.logger)
	app.router = api.NewRouter(app.restManager, app.logger)

	// 加载现有配置
	err = app.hostManager.LoadAll()
	if err != nil {
		app.logger.Warnf("加载配置失败: %v", err)
	}

	// 启动定时任务调度器（与Python版本一致，每60秒执行一次）
	app.startCronScheduler()

	app.logger.Info("应用程序启动完成")

	// 启动Web服务器
	return app.startWebServer()
}

// initLogger 初始化日志
func (app *Application) initLogger() *logrus.Logger {
	logger := logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{
		TimestampFormat: time.RFC3339,
	})
	logger.SetLevel(logrus.InfoLevel)
	logger.SetOutput(os.Stdout)
	return logger
}

// startCronScheduler 启动定时任务调度器 - 与Python版本完全一致
func (app *Application) startCronScheduler() {
	app.logger.Info("[Cron] 启动定时任务调度器...")
	
	// 创建定时器，每60秒执行一次（与Python版本一致）
	app.cronTicker = time.NewTicker(60 * time.Second)
	app.cronDone = make(chan bool)
	
	// 立即执行一次（与Python版本的initial_run一致）
	go func() {
		app.executeCronTask()
		app.logger.Info("[Cron] 初始执行完成")
		
		// 开始定时循环
		for {
			select {
			case <-app.cronTicker.C:
				app.executeCronTask()
			case <-app.cronDone:
				return
			}
		}
	}()
	
	app.logger.Info("[Cron] 定时任务已启动（后台运行），每60秒执行一次")
}

// executeCronTask 执行定时任务 - 与Python版本完全一致
func (app *Application) executeCronTask() {
	defer func() {
		if r := recover(); r != nil {
			app.logger.Errorf("[Cron] 执行定时任务出错: %v", r)
		}
	}()
	
	app.logger.Debug("[Cron] 执行定时任务")
	
	// 调用HostManager的ExecuteCron方法（对应Python版本的hs_manage.exe_cron()）
	app.hostManager.ExecuteCron()
	
	app.logger.Debug("[Cron] 执行定时任务完成")
}

// stopCronScheduler 停止定时任务调度器
func (app *Application) stopCronScheduler() {
	if app.cronTicker != nil {
		app.cronTicker.Stop()
		close(app.cronDone)
		app.logger.Info("[Cron] 定时任务调度器已停止")
	}
}

// startWebServer 启动Web服务器
func (app *Application) startWebServer() error {
	// 设置路由
	engine := app.router.SetupRoutes()
	
	// 创建HTTP服务器
	app.server = &http.Server{
		Addr:         fmt.Sprintf("%s:%d", app.config.Server.Host, app.config.Server.Port),
		Handler:      engine,
		ReadTimeout:  time.Duration(app.config.Server.ReadTimeout) * time.Second,
		WriteTimeout: time.Duration(app.config.Server.WriteTimeout) * time.Second,
	}

	// 启动服务器的goroutine
	go func() {
		app.logger.Infof("Web服务器启动在 http://%s:%d", app.config.Server.Host, app.config.Server.Port)
		if err := app.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			app.logger.Fatalf("Web服务器启动失败: %v", err)
		}
	}()

	// 等待中断信号
	app.waitForShutdown()
	return nil
}

// waitForShutdown 等待关闭信号
func (app *Application) waitForShutdown() {
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	
	app.logger.Info("正在关闭应用程序...")
	
	// 停止定时任务调度器
	app.stopCronScheduler()
	
	// 保存所有数据（与Python版本的all_save()一致）
	if err := app.hostManager.SaveAll(); err != nil {
		app.logger.Error("保存数据失败: " + err.Error())
	}
	
	app.logger.Info("正在关闭Web服务器...")
	
	// 创建一个5秒的上下文用于关闭
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	// 关闭服务器
	if err := app.server.Shutdown(ctx); err != nil {
		app.logger.Errorf("Web服务器关闭失败: %v", err)
	}
	
	app.logger.Info("应用程序已关闭")
}