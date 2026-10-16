// Package api 统一访问控制与响应脱敏
package api

import (
	"encoding/json"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/auth"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
	"github.com/OpenIDCSTeam/HostAgent-Go/pkg/utils"
)

// 主机配置中不得下发给普通用户的凭据字段
var hostSecretFields = []string{"server_pass", "i_kuai_pass"}

// HostVMAccessMiddleware 统一执行「主机访问权 + 虚拟机所有权」校验。
//
// 管理员直接放行；普通用户必须被分配到目标主机，且是目标虚拟机的所有者。
// 该中间件覆盖所有带 :hs_name / :vm_uuid 路由参数的接口，
// 以取代此前分散在各处理器、且存在遗漏与恒真实现的检查。
func (app *App) HostVMAccessMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		uc := auth.GetUserContext(c)
		if uc == nil {
			utils.Unauthorized(c)
			c.Abort()
			return
		}
		if uc.IsAdmin {
			c.Next()
			return
		}

		// 受限身份（临时凭据 / 访问密钥登录）只允许访问签发时绑定的主机与虚拟机
		if uc.TempLogin {
			hsName := c.Param("hs_name")
			vmUUID := c.Param("vm_uuid")
			if hsName == "" {
				// 非主机作用域接口（当前用户、任务等）：与普通用户分支保持一致，
				// 放行后由各自处理器按身份过滤。若在此一律拒绝，前端连
				// /api/users/current 都无法调用，受限会话会表现为「登录后立刻掉线」。
				c.Next()
				return
			}
			if hsName != uc.TempHsName || (vmUUID != "" && vmUUID != uc.TempVMUUID) {
				utils.Forbidden(c)
				c.Abort()
				return
			}
			c.Next()
			return
		}

		hsName := c.Param("hs_name")
		if hsName == "" {
			// 非主机作用域接口（用户自身、任务等）由各自处理器过滤
			c.Next()
			return
		}
		// 标识会参与目录拼接与外部命令参数，先做格式校验再进入业务逻辑
		if !vmUUIDLooksSafe(hsName) {
			utils.BadRequest(c, "主机标识格式无效")
			c.Abort()
			return
		}
		if !app.HostSvc.CheckHostAccess(uc.Username, hsName) {
			utils.Forbidden(c)
			c.Abort()
			return
		}

		vmUUID := c.Param("vm_uuid")
		if vmUUID == "" {
			c.Next()
			return
		}
		if !vmUUIDLooksSafe(vmUUID) {
			utils.BadRequest(c, "虚拟机标识格式无效")
			c.Abort()
			return
		}
		if !app.HostSvc.CheckVMOwnership(uc.Username, hsName, vmUUID) {
			utils.Fail(c, 403, "无权操作此虚拟机")
			c.Abort()
			return
		}
		c.Next()
	}
}

// canOperateVM 判断当前用户是否可操作目标虚拟机。
//
// 统一三处判定，避免处理器各自实现时遗漏管理员放行：
//   - 管理员：放行（否则管理员未出现在 own_all 中会被误拒）；
//   - 临时凭据：仅限签发时绑定的主机与虚拟机；
//   - 普通用户：必须是该虚拟机的所有者。
func (app *App) canOperateVM(uc *auth.UserContext, hsName, vmUUID string) bool {
	if uc == nil {
		return false
	}
	if uc.IsAdmin {
		return true
	}
	if uc.TempLogin {
		return uc.TempHsName == hsName && uc.TempVMUUID == vmUUID
	}
	return app.HostSvc.CheckVMOwnership(uc.Username, hsName, vmUUID)
}

// checkQuotaForUser 校验用户配额。
//
// Token 登录的管理员在数据库中没有对应用户行（配额为上限值），
// 直接查询会得到「用户不存在」并阻断创建，因此这里显式放行。
func (app *App) checkQuotaForUser(uc *auth.UserContext, cfg *model.VMConfig) error {
	if uc == nil {
		return nil
	}
	if uc.IsToken {
		return nil
	}
	if app.QuotaSvc == nil {
		return nil
	}
	return app.QuotaSvc.CheckQuota(uc.Username, cfg)
}

// sanitizeHostConfig 将主机配置转为响应 map 并清空凭据字段。
func sanitizeHostConfig(cfg interface{}) map[string]interface{} {
	result := make(map[string]interface{})
	if cfg == nil {
		return result
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return result
	}
	if err := json.Unmarshal(data, &result); err != nil || result == nil {
		return make(map[string]interface{})
	}
	for _, field := range hostSecretFields {
		if _, ok := result[field]; ok {
			result[field] = ""
		}
	}
	return result
}

// filterVMsForUser 仅保留用户具有所有者身份的虚拟机，避免同主机跨用户泄露。
func filterVMsForUser(vms map[string]*model.VMConfig, username string) map[string]*model.VMConfig {
	result := make(map[string]*model.VMConfig, len(vms))
	if username == "" {
		return result
	}
	for uuid, vm := range vms {
		if vm == nil || vm.OwnAll == nil {
			continue
		}
		if _, ok := vm.OwnAll[username]; ok {
			result[uuid] = vm
		}
	}
	return result
}

// vmUUIDLooksSafe 校验虚拟机标识可安全用于路径与字符串拼接。
// 该标识会用于宿主机目录拼接、外部命令参数和切片操作，
// 因此必须拒绝路径分隔符、上级目录与空白字符。
func vmUUIDLooksSafe(vmUUID string) bool {
	if vmUUID == "" || len(vmUUID) > 128 {
		return false
	}
	if strings.ContainsAny(vmUUID, `/\:*?"<>|`) || strings.Contains(vmUUID, "..") {
		return false
	}
	return strings.TrimSpace(vmUUID) == vmUUID
}

// shortUUID 安全截取标识前缀，避免对过短标识做切片导致 panic。
func shortUUID(vmUUID string, n int) string {
	if n <= 0 || len(vmUUID) <= n {
		return vmUUID
	}
	return vmUUID[:n]
}
