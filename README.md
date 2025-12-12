# OpenIDCS 客户端 (Go版本)

[![Go Version](https://img.shields.io/badge/Go-1.21+-blue.svg)](https://golang.org)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

OpenIDCS (开源IDC受控端) 的Go语言实现版本，用于管理虚拟化平台上的虚拟机资源。

## 🚀 功能特性

### 核心功能
- ✅ 主机管理：支持多种虚拟化平台
- ✅ 虚拟机管理：完整的VM生命周期管理
- ✅ 网络管理：IP地址、端口分配和代理服务
- ✅ VNC控制台：远程桌面访问
- ✅ REST API：完整的HTTP接口
- ✅ 实时监控：系统状态监控和告警
- ✅ 认证安全：Token认证和中间件

### 支持的虚拟化平台
- VMware Workstation (已实现)
- Hyper-V (计划中)
- Proxmox (计划中)
- VirtualBox (计划中)

## 📦 安装部署

### 环境要求
- Go 1.21+
- SQLite3
- Windows/Linux/macOS

### 编译安装
```bash
# 克隆项目
git clone https://github.com/OpenIDCSTeam/OpenIDCS-Client-Go.git
cd OpenIDCS-Client-Go

# 下载依赖
go mod download

# 编译
go build -o openidcs-client ./cmd

# 运行
./openidcs-client
```

### Docker部署
```bash
# 构建镜像
docker build -t openidcs-client .

# 运行容器
docker run -d -p 1880:1880 -v ./data:/app/data openidcs-client
```

## ⚙️ 配置说明

配置文件位于 `config/config.yaml`：

```yaml
server:
  host: "0.0.0.0"      # 监听地址
  port: 1880           # 监听端口
  mode: "debug"        # 运行模式

database:
  type: "sqlite3"
  path: "./data/hostmanage.db"

security:
  jwt_secret: "your-secret-key"
  token_expire: 24     # Token过期时间(小时)

vnc:
  enable: true
  port_range: "5900-5999"
  max_sessions: 100
```

## 📚 API文档

### 认证
所有API请求都需要在Header中包含Token：
```
Authorization: Bearer your-token
```

### 系统管理
```bash
# 获取当前Token
GET /api/v1/system/token/current

# 重置Token
POST /api/v1/system/token/reset

# 获取系统统计
GET /api/v1/system/statis
```

### 主机管理
```bash
# 获取所有主机
GET /api/v1/hosts

# 创建主机
POST /api/v1/hosts
{
  "hs_name": "host1",
  "hs_type": "VMWareSetup",
  "hs_config": { ... }
}

# 删除主机
DELETE /api/v1/hosts/{host_name}
```

### 虚拟机管理
```bash
# 获取虚拟机列表
GET /api/v1/vms/{host_name}

# 创建虚拟机
POST /api/v1/vms/{host_name}
{
  "vm_uuid": "vm-001",
  "vm_name": "test-vm",
  "cpu_num": 2,
  "ram_num": 4096,
  "hdd_num": 50
}

# 电源控制
POST /api/v1/vms/{host_name}/{vm_uuid}/power
{
  "action": "start"
}

# VNC控制台
GET /api/v1/vms/{host_name}/{vm_uuid}/console
```

## 🏗️ 项目结构

```
OpenIDCS-Client-Go/
├── cmd/                    # 主程序入口
│   └── main.go
├── internal/               # 内部包
│   ├── api/               # API处理器
│   ├── config/            # 配置管理
│   ├── database/          # 数据库操作
│   ├── host/              # 主机管理
│   ├── middleware/        # 中间件
│   ├── models/            # 数据模型
│   ├── monitoring/        # 监控模块
│   ├── network/           # 网络管理
│   ├── vm/                # 虚拟机管理
│   └── vnc/               # VNC控制台
├── config/                # 配置文件
├── data/                  # 数据目录
├── web/                   # Web界面
├── go.mod
├── go.sum
└── README.md
```

## 🔧 开发指南

### 添加新的虚拟化平台支持

1. 在 `internal/host/` 目录下实现 `BaseServer` 接口
2. 在 `internal/api/host_api.go` 中注册新的引擎类型
3. 更新配置文件中的引擎选项

### 扩展API接口

1. 在相应的API文件中添加处理函数
2. 在 `internal/api/router.go` 中注册路由
3. 更新API文档

### 自定义中间件

在 `internal/middleware/` 目录下创建新的中间件：
```go
func CustomMiddleware() gin.HandlerFunc {
    return func(c *gin.Context) {
        // 自定义逻辑
        c.Next()
    }
}
```

## 📊 监控指标

系统提供以下监控指标：
- 主机数量和状态
- 虚拟机数量和状态
- CPU、内存、磁盘使用率
- 网络流量统计
- 端口分配情况

### 告警阈值
- CPU使用率 > 80%
- 内存使用率 > 80%
- 磁盘使用率 > 80%
- 系统负载 > 2.0

## 🔒 安全特性

- Token认证机制
- CORS跨域保护
- 安全HTTP头
- 请求限流
- 输入验证

## 🐛 故障排除

### 常见问题

1. **数据库连接失败**
   - 检查数据库文件路径权限
   - 确认SQLite驱动已正确安装

2. **端口被占用**
   - 修改配置文件中的端口号
   - 检查防火墙设置

3. **VNC连接失败**
   - 确认虚拟机VNC服务已启用
   - 检查端口范围配置

### 日志查看
```bash
# 查看应用日志
tail -f ./data/logs/openidcs.log

# 查看错误日志
grep "ERROR" ./data/logs/openidcs.log
```

## 🤝 贡献指南

1. Fork 项目
2. 创建特性分支 (`git checkout -b feature/AmazingFeature`)
3. 提交更改 (`git commit -m 'Add some AmazingFeature'`)
4. 推送到分支 (`git push origin feature/AmazingFeature`)
5. 开启 Pull Request

## 📄 许可证

本项目采用 MIT 许可证 - 查看 [LICENSE](LICENSE) 文件了解详情

## 📞 联系方式

- 项目主页：https://github.com/OpenIDCSTeam/OpenIDCS-Client-Go
- 问题反馈：https://github.com/OpenIDCSTeam/OpenIDCS-Client-Go/issues
- 邮箱：openidcs@example.com

## 🙏 致谢

感谢所有为本项目做出贡献的开发者和社区成员。

---

**OpenIDCS** - 开源IDC受控端，让虚拟化管理更简单！