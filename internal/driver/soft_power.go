// Package driver 软电源操作后台监控。
//
// 对应 Python 版 BasicServer.soft_pwr / _monitor_power_operation：
// 软关机与软重启是「请求」而非「结果」——平台受理后虚拟机内部还需要时间
// 走完关机流程，期间状态停留在中间态（ON_STOP）。若不做监控，
// 前端会一直显示「关闭中」，且后续操作（改配置、挂盘、备份）会因状态判断失败被拒。
//
// 本文件以 5 秒间隔、最长 5 分钟轮询平台实际状态，直到状态稳定或超时。
package driver

import (
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// 监控参数（与 Python 版一致：间隔 5 秒，最长 5 分钟）
const (
	softPowerCheckInterval = 5 * time.Second
	softPowerMaxDuration   = 300 * time.Second
)

// SoftPowerMonitor 后台监控软电源操作直到状态变化或超时。
//
// expected 为调用方设置的中间状态（软关机/软重启均为 ON_STOP）。
// 监控过程中以「平台实际状态」回写内存，保证前端看到的是真实状态。
func (b *BaseDriver) SoftPowerMonitor(vmUUID string, expected model.VMPowers) {
	self := b.SelfDriver()
	if self == nil {
		return
	}

	go func() {
		deadline := time.Now().Add(softPowerMaxDuration)
		for time.Now().Before(deadline) {
			time.Sleep(softPowerCheckInterval)

			// 虚拟机已被删除则停止监控
			b.mu.RLock()
			cfg, exists := b.VmSaving[vmUUID]
			current := model.VMPowerUnknown
			if exists {
				current = cfg.VmFlag
			}
			b.mu.RUnlock()
			if !exists {
				return
			}

			// 内存状态已脱离中间态：说明操作已完成或被其它操作改变
			if current != expected {
				return
			}

			// 以平台实际状态为准回写
			actual := self.GetPower(vmUUID)
			if state, ok := NormalizePowerState[actual]; ok {
				b.SetVMPowerState(vmUUID, state)
				if state != expected {
					return
				}
			}
		}

		// 超时仍未完成：回落到平台实际状态，避免状态永久卡在「关闭中」
		b.SetVMPowerState(vmUUID, model.VMPowerUnknown)
	}()
}
