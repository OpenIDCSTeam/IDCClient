// Package scheduler 定时任务调度器
package scheduler

import (
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/driver"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/logger"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/service"
)

const (
	// defaultMaintenanceInterval 维护任务（清理 / 配额重算）默认执行间隔
	defaultMaintenanceInterval = 10 * time.Minute
	// vmStatusHistoryKeep 每个虚拟机保有的状态历史条数（与 Python 版一致）
	vmStatusHistoryKeep = 43200
	// maxHostConcurrency 单轮状态采集的最大并发主机数。
	// 不限制并发时，主机数量增长会让 goroutine 与外部连接数无上限。
	maxHostConcurrency = 4
	// maintenanceTimeout 单次维护任务的最长等待时间
	maintenanceTimeout = 8 * time.Minute
	// stopTimeout 停止调度器时等待当前轮次的最长时间
	stopTimeout = 20 * time.Second
	// statusCleanupBatches 每轮维护最多执行的历史清理批次
	statusCleanupBatches = 4
	// statusCleanupBatchSize 单批删除的历史条数
	statusCleanupBatchSize = 5000
)

// Scheduler 定时任务调度器
type Scheduler struct {
	db         *data.DB
	getDrivers func() map[string]driver.HostDriver
	cleanupSvc *service.VMCleanupService
	quotaSvc   *service.QuotaService
	syncSvc    *service.SyncService

	reconcileSvc *service.ReconcileService
	deleteFlow   *service.DeleteVMWorkflow

	interval            time.Duration
	maintenanceInterval time.Duration
	lastMaintenance     time.Time

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
	// sem 限制单轮采集的主机并发数
	sem chan struct{}
	// maintenanceRunning 保证同一时间只有一轮维护在执行
	maintenanceRunning int32
	// log 业务日志（可为 nil：未注入时静默）
	log *logger.Logger
}

// SetLogger 注入业务日志器。
func (s *Scheduler) SetLogger(l *logger.Logger) {
	s.log = l
}

// NewScheduler 创建调度器
// getDrivers: 获取所有已启用主机驱动的函数
func NewScheduler(db *data.DB, getDrivers func() map[string]driver.HostDriver) *Scheduler {
	return &Scheduler{
		db:                  db,
		getDrivers:          getDrivers,
		interval:            60 * time.Second,
		maintenanceInterval: defaultMaintenanceInterval,
		stopCh:              make(chan struct{}),
		sem:                 make(chan struct{}, maxHostConcurrency),
	}
}

// SetCleanupService 注入孤立资源清理服务（可选）
func (s *Scheduler) SetCleanupService(svc *service.VMCleanupService) {
	s.cleanupSvc = svc
}

// SetQuotaService 注入配额服务（可选）
func (s *Scheduler) SetQuotaService(svc *service.QuotaService) {
	s.quotaSvc = svc
}

// SetSyncService 注入主机间自动同步服务（可选）
func (s *Scheduler) SetSyncService(svc *service.SyncService) {
	s.syncSvc = svc
}

// SetReconcileService 注入外部资源对账服务（可选）
func (s *Scheduler) SetReconcileService(svc *service.ReconcileService) {
	s.reconcileSvc = svc
}

// SetDeleteWorkflow 注入删除续做工作流（可选）
func (s *Scheduler) SetDeleteWorkflow(flow *service.DeleteVMWorkflow) {
	s.deleteFlow = flow
}

// SetMaintenanceInterval 设置维护任务执行间隔（<=0 时忽略）
func (s *Scheduler) SetMaintenanceInterval(d time.Duration) {
	if d > 0 {
		s.maintenanceInterval = d
	}
}

// Start 启动定时任务
func (s *Scheduler) Start() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()

		// 启动时立即执行一次
		s.runSync()

		for {
			select {
			case <-ticker.C:
				s.runSync()
			case <-s.stopCh:
				return
			}
		}
	}()
	log.Println("[scheduler] 定时任务已启动，间隔60秒")
}

// Stop 停止定时任务。
//
// 使用 stopOnce 保证重复调用安全，并对等待设置上限：
// 原实现里的 close(stopCh) 在重复调用时会 panic，
// 且 wg.Wait() 在某个驱动调用阻塞时会无限期挂住进程退出。
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() { close(s.stopCh) })

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(stopTimeout):
		log.Printf("[scheduler] 停止等待超时（%s），仍有采集任务未结束", stopTimeout)
	}
	log.Println("[scheduler] 定时任务已停止")
}

// runSync 执行全量状态同步
func (s *Scheduler) runSync() {
	drivers := s.getDrivers()
	if len(drivers) > 0 {
		var wg sync.WaitGroup
		for hsName, d := range drivers {
			// 通过信号量限制并发；停止时不再启动新的采集
			select {
			case s.sem <- struct{}{}:
			case <-s.stopCh:
				wg.Wait()
				return
			}
			wg.Add(1)
			go func(name string, drv driver.HostDriver) {
				defer wg.Done()
				defer func() { <-s.sem }()
				s.syncHost(name, drv)
			}(hsName, d)
		}
		wg.Wait()
	}

	// 周期性维护任务（清理 / 配额重算），按较低频率执行
	s.maybeRunMaintenance()
}

// syncHost 同步单个主机状态
func (s *Scheduler) syncHost(hsName string, d driver.HostDriver) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Event("scheduler", "状态采集").Host(hsName).
				Detail("panic", r).Errorf("同步主机时发生 panic: %v", r)
		}
	}()

	// 1. 采集主机硬件状态
	hwStatus, err := d.GetStatus()
	if err != nil {
		// 采集失败（主机离线/超时）每分钟都会发生：属于高频低价值信息，
		// Debug 级别只落文件，避免污染终端与审计日志。
		s.log.Event("scheduler", "状态采集").Host(hsName).Err(err).
			Debug("采集主机状态失败: %v", err)
	} else if hwStatus != nil && (hwStatus.CpuTotal > 0 || hwStatus.MemTotal > 0) {
		if err := s.db.AddHSStatus(hsName, hwStatus); err != nil {
			s.log.Event("scheduler", "状态采集").Host(hsName).Err(err).
				Warn("保存主机 %s 状态失败: %v", hsName, err)
		}
	}

	// 2. 同步所有虚拟机电源状态
	vms := d.GetVMSaving()
	now := time.Now().Unix()
	for vmUUID, vmCfg := range vms {
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.log.Event("scheduler", "状态采集").Host(hsName).VM(vmUUID, "").
						Detail("panic", r).Errorf("同步虚拟机状态时发生 panic: %v", r)
				}
			}()

			actualStatus := d.GetPower(vmUUID)
			// 通过驱动锁内方法写回：快照对象是复制品，
			// 直接修改不会生效，且与其它请求并发时会形成数据竞争。
			if newState, ok := driver.PowerStatusMap[actualStatus]; ok {
				d.SetVMPowerState(vmUUID, newState)
			}
			_ = vmCfg

			// 保存VM状态到数据库
			statusData := map[string]interface{}{
				"vm_state":  actualStatus,
				"on_update": now,
			}
			if err := s.db.AddVMStatus(hsName, vmUUID, statusData, actualStatus, now, 0); err != nil {
				s.log.Event("scheduler", "状态采集").Host(hsName).VM(vmUUID, "").Err(err).
					Warn("保存虚拟机 %s 状态失败: %v", vmUUID, err)
			}
		}()
	}

	// 3. 执行驱动定时任务
	d.Crontab()
}

// maybeRunMaintenance 按维护间隔触发维护任务。
//
// 维护在独立协程中执行并带超时：维护内部会做全量扫描与配额重算，
// 原实现把它放在采集轮次里同步执行，一旦维护变慢会直接拖住下一轮采集。
func (s *Scheduler) maybeRunMaintenance() {
	now := time.Now()
	if !s.lastMaintenance.IsZero() && now.Sub(s.lastMaintenance) < s.maintenanceInterval {
		return
	}
	// 同一时间只允许一轮维护
	if !atomic.CompareAndSwapInt32(&s.maintenanceRunning, 0, 1) {
		return
	}
	s.lastMaintenance = now

	go func() {
		defer atomic.StoreInt32(&s.maintenanceRunning, 0)

		done := make(chan struct{})
		go func() {
			defer close(done)
			s.runMaintenance()
		}()

		select {
		case <-done:
		case <-time.After(maintenanceTimeout):
			log.Printf("[scheduler] 维护任务执行超时（%s），本轮放弃等待", maintenanceTimeout)
		case <-s.stopCh:
			log.Println("[scheduler] 调度器停止，维护任务不再等待")
		}
	}()
}

// runMaintenance 执行周期性维护任务
// 对应 Python 版 HostManage.exe_cron 中的清理与配额重算逻辑
func (s *Scheduler) runMaintenance() {
	start := time.Now()
	// 维护细节：先收集计数，最后按「有无异常」决定日志级别——
	// 一切正常时用 DEBUG（只落文件），有清理/补偿动作时用 INFO，
	// 出错时用 WARNING/ERROR（终端与数据库都能看到）。
	detail := map[string]interface{}{}

	// 1. 清理孤立资源（NAT / Web 代理中 VM 已不存在的记录）
	if s.cleanupSvc != nil {
		if n, err := s.cleanupSvc.CleanupOrphanedResources(); err != nil {
			s.log.Event("scheduler", "清理孤立资源").Err(err).
				Warn("清理孤立资源失败: %v", err)
		} else if n > 0 {
			detail["orphaned_cleaned"] = n
		}
	}

	if s.db != nil {
		// 2. 清理已删除虚拟机的状态数据
		if n, err := s.db.CleanupDeletedVMStatus(); err != nil {
			s.log.Event("scheduler", "清理虚拟机状态").Err(err).
				Warn("清理已删除虚拟机状态失败: %v", err)
		} else if n > 0 {
			detail["deleted_vm_status_cleaned"] = n
		}

		// 3. 清理虚拟机状态历史记录（每个VM保留最近 N 条）。
		// 分批执行：单次全表窗口排名删除会长时间持有写锁，
		// 与状态上报和业务写入互相阻塞。
		var totalCleaned int64
		for i := 0; i < statusCleanupBatches; i++ {
			n, err := s.db.CleanupVMStatusHistoryBatch(vmStatusHistoryKeep, statusCleanupBatchSize)
			if err != nil {
				s.log.Event("scheduler", "清理状态历史").Err(err).
					Warn("清理虚拟机状态历史失败: %v", err)
				break
			}
			totalCleaned += n
			if n < int64(statusCleanupBatchSize) {
				break
			}
		}
		if totalCleaned > 0 {
			detail["vm_status_history_cleaned"] = totalCleaned
		}
	}

	// 4. 全量重算用户资源配额
	if s.quotaSvc != nil {
		if err := s.quotaSvc.RecalculateAll(); err != nil {
			s.log.Event("scheduler", "配额重算").Err(err).
				Warn("重算用户资源配额失败: %v", err)
		}
	}

	// 5. 主机间自动同步（pull / push / both，按系统设置与间隔自行判断）
	if s.syncSvc != nil {
		if err := s.syncSvc.CheckAndExecute(); err != nil {
			s.log.Event("scheduler", "主机自动同步").Err(err).
				Warn("自动同步失败: %v", err)
		}
	}

	// 6. 继续未完成的虚拟机删除（进程在删除中途退出后的断点续做）
	if s.deleteFlow != nil {
		resumed, failed := s.deleteFlow.SweepIncomplete(20)
		if resumed > 0 || failed > 0 {
			detail["delete_resumed"] = resumed
			detail["delete_failed"] = failed
			ev := s.log.Event("scheduler", "删除续做").Details(detail)
			if failed > 0 {
				ev.Warn("删除续做：完成 %d 台，失败 %d 台", resumed, failed)
			} else {
				ev.Info("删除续做：完成 %d 台", resumed)
			}
		}
	}

	// 7. 外部资源补偿（路由器销账、代理重新下发等）
	if s.reconcileSvc != nil {
		done, retried := s.reconcileSvc.ProcessOutbox(50)
		if done > 0 || retried > 0 {
			detail["outbox_done"] = done
			detail["outbox_retried"] = retried
		}
		if pending := s.reconcileSvc.PendingCount(); pending > 0 {
			s.log.Event("scheduler", "外部资源补偿").Detail("pending", pending).
				Warn("仍有 %d 条外部资源补偿任务待处理", pending)
		}
		s.reconcileSvc.PurgeExpired(7, 30)
	}

	ev := s.log.Event("scheduler", "周期维护").
		Details(detail).
		Duration(time.Since(start))
	if len(detail) > 0 {
		ev.Info("周期维护完成，本轮产生 %d 项清理/补偿动作", len(detail))
	} else {
		ev.Debug("周期维护完成，无需清理")
	}
}
