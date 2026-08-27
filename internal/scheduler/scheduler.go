// Package scheduler 定时任务调度器
package scheduler

import (
	"log"
	"sync"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/driver"
)

// Scheduler 定时任务调度器
type Scheduler struct {
	db          *data.DB
	getDrivers  func() map[string]driver.HostDriver
	interval    time.Duration
	stopCh      chan struct{}
	wg          sync.WaitGroup
}

// NewScheduler 创建调度器
// getDrivers: 获取所有已启用主机驱动的函数
func NewScheduler(db *data.DB, getDrivers func() map[string]driver.HostDriver) *Scheduler {
	return &Scheduler{
		db:         db,
		getDrivers: getDrivers,
		interval:   60 * time.Second,
		stopCh:     make(chan struct{}),
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

// Stop 停止定时任务
func (s *Scheduler) Stop() {
	close(s.stopCh)
	s.wg.Wait()
	log.Println("[scheduler] 定时任务已停止")
}

// runSync 执行全量状态同步
func (s *Scheduler) runSync() {
	drivers := s.getDrivers()
	if len(drivers) == 0 {
		return
	}

	var wg sync.WaitGroup
	for hsName, d := range drivers {
		wg.Add(1)
		go func(name string, drv driver.HostDriver) {
			defer wg.Done()
			s.syncHost(name, drv)
		}(hsName, d)
	}
	wg.Wait()
}

// syncHost 同步单个主机状态
func (s *Scheduler) syncHost(hsName string, d driver.HostDriver) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[scheduler] 同步主机 %s 时发生panic: %v", hsName, r)
		}
	}()

	// 1. 采集主机硬件状态
	hwStatus, err := d.GetStatus()
	if err != nil {
		log.Printf("[scheduler] 采集主机 %s 状态失败: %v", hsName, err)
	} else if hwStatus != nil && (hwStatus.CpuTotal > 0 || hwStatus.MemTotal > 0) {
		if err := s.db.AddHSStatus(hsName, hwStatus); err != nil {
			log.Printf("[scheduler] 保存主机 %s 状态失败: %v", hsName, err)
		}
	}

	// 2. 同步所有虚拟机电源状态
	vms := d.GetVMSaving()
	now := time.Now().Unix()
	for vmUUID, vmCfg := range vms {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[scheduler] 同步VM %s/%s 状态时发生panic: %v", hsName, vmUUID, r)
				}
			}()

			actualStatus := d.GetPower(vmUUID)
			if newState, ok := driver.PowerStatusMap[actualStatus]; ok {
				vmCfg.VmFlag = newState
			}

			// 保存VM状态到数据库
			statusData := map[string]interface{}{
				"vm_state":  actualStatus,
				"on_update": now,
			}
			if err := s.db.AddVMStatus(hsName, vmUUID, statusData, actualStatus, now, 0); err != nil {
				log.Printf("[scheduler] 保存VM %s/%s 状态失败: %v", hsName, vmUUID, err)
			}
		}()
	}

	// 3. 执行驱动定时任务
	d.Crontab()
}
