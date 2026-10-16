// Package driver 通用命令执行辅助
package driver

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"time"
)

// jsonUnmarshalLoose 宽松解析外部命令输出的 JSON。
//
// 某些平台（如 PowerShell 的 ConvertTo-Json）在输出单对象时字段大小写、
// 以及 [] 与 {} 的差异会与预期不符，这里统一收敛入口，失败时返回错误由调用方降级处理。
func jsonUnmarshalLoose(data string, v interface{}) error {
	return json.Unmarshal([]byte(data), v)
}

// runWithStdin 执行命令并通过 stdin 传入数据（用于 chpasswd 等需要标准输入的场景）
func runWithStdin(args []string, stdin string, timeout time.Duration) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
