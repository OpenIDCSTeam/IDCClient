package models

// VMPowerState 虚拟机电源状态
type VMPowerState int

const (
	// 基本状态
	PowerStopped VMPowerState = 0x0 // 停止
	PowerStarted VMPowerState = 0x1 // 启动
	PowerSuspend VMPowerState = 0x2 // 暂停
	
	// 进行状态
	PowerOnStop VMPowerState = 0x3 // 停止中
	PowerOnOpen VMPowerState = 0x4 // 打开中
	PowerOnSave VMPowerState = 0x5 // 保存中
	PowerOnWake VMPowerState = 0x6 // 唤醒中
	
	// 命令状态
	PowerOn       VMPowerState = 0x7 // 打开
	PowerReset    VMPowerState = 0x8 // 重置
	PowerShutdown VMPowerState = 0x9 // 关闭
	PowerOff      VMPowerState = 0xa // 强制关闭
	PowerPause    VMPowerState = 0xb // 暂停
	PowerUnpause  VMPowerState = 0xc // 唤醒
	
	// 其他状态
	PowerCrashed VMPowerState = 0xe // 崩溃
	PowerUnknown VMPowerState = 0xf // 未知
)

// String 返回电源状态的字符串表示
func (p VMPowerState) String() string {
	switch p {
	case PowerStopped:
		return "STOPPED"
	case PowerStarted:
		return "STARTED"
	case PowerSuspend:
		return "SUSPEND"
	case PowerOnStop:
		return "ON_STOP"
	case PowerOnOpen:
		return "ON_OPEN"
	case PowerOnSave:
		return "ON_SAVE"
	case PowerOnWake:
		return "ON_WAKE"
	case PowerOn:
		return "S_START"
	case PowerReset:
		return "S_RESET"
	case PowerShutdown:
		return "S_CLOSE"
	case PowerOff:
		return "H_CLOSE"
	case PowerPause:
		return "A_PAUSE"
	case PowerUnpause:
		return "A_WAKED"
	case PowerCrashed:
		return "CRASHED"
	case PowerUnknown:
		return "UNKNOWN"
	default:
		return "UNKNOWN"
	}
}

// MarshalJSON 实现JSON序列化
func (p VMPowerState) MarshalJSON() ([]byte, error) {
	return []byte(`"` + p.String() + `"`), nil
}

// UnmarshalJSON 实现JSON反序列化
func (p *VMPowerState) UnmarshalJSON(data []byte) error {
	str := string(data)
	str = str[1 : len(str)-1] // 去掉引号
	
	switch str {
	case "STOPPED":
		*p = PowerStopped
	case "STARTED":
		*p = PowerStarted
	case "SUSPEND":
		*p = PowerSuspend
	case "ON_STOP":
		*p = PowerOnStop
	case "ON_OPEN":
		*p = PowerOnOpen
	case "ON_SAVE":
		*p = PowerOnSave
	case "ON_WAKE":
		*p = PowerOnWake
	case "S_START":
		*p = PowerOn
	case "S_RESET", "H_RESET":
		*p = PowerReset
	case "S_CLOSE":
		*p = PowerShutdown
	case "H_CLOSE":
		*p = PowerOff
	case "A_PAUSE":
		*p = PowerPause
	case "A_WAKED":
		*p = PowerUnpause
	case "CRASHED":
		*p = PowerCrashed
	default:
		*p = PowerUnknown
	}
	
	return nil
}
