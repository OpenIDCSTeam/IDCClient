// Package driver PortForward / IPTablesAPI 纯解析逻辑单元测试。
//
// 这两个模块在真实环境中依赖宿主机 sshd / iptables / socat，
// 因此把「输出解析」抽成纯函数并在此覆盖，确保：
//   - socat `ps -ef` 输出能正确还原出转发五元组
//   - iptables `-t nat -L DOCKER -n` 输出能正确提取已占用端口
//   - 非法/干扰行不会导致 panic 或误判
package driver

import (
	"testing"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

func TestParseSocatPSOutput(t *testing.T) {
	out := `root       108104       1  0 15:10 ?        00:00:00 socat TCP-LISTEN:24203,reuseaddr,fork TCP:172.19.0.100:22
root       108105       1  0 15:11 ?        00:00:00 socat UDP-LISTEN:25000,reuseaddr,fork UDP:172.17.0.5:53
root       108106       1  0 15:12 ?        00:00:00 grep --color=auto socat
someuser   1234    1  0 15:13 ?        00:00:00 /usr/bin/socat TCP-LISTEN:26000,reuseaddr,fork TCP:10.0.0.9:80
nobody     9999    1  0 15:14 ?        00:00:00 socat EXEC:/bin/sh
bad line`

	got := parseSocatPSOutput(out)
	if len(got) != 3 {
		t.Fatalf("期望解析出 3 条转发，实际 %d 条: %+v", len(got), got)
	}

	want := []PortForwardEntry{
		{WanPort: 24203, LanAddr: "172.19.0.100", LanPort: 22, Protocol: "TCP", Pid: 108104},
		{WanPort: 25000, LanAddr: "172.17.0.5", LanPort: 53, Protocol: "UDP", Pid: 108105},
		{WanPort: 26000, LanAddr: "10.0.0.9", LanPort: 80, Protocol: "TCP", Pid: 1234},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("第 %d 条不匹配:\n期望 %+v\n实际 %+v", i, w, got[i])
		}
	}
}

func TestParseSocatPSOutputEmptyAndGarbage(t *testing.T) {
	if got := parseSocatPSOutput(""); len(got) != 0 {
		t.Fatalf("空输入应返回空结果，实际 %+v", got)
	}
	garbage := "\n\n  \nno socat here\nps -ef | grep socat\n"
	if got := parseSocatPSOutput(garbage); len(got) != 0 {
		t.Fatalf("无有效转发行应返回空结果，实际 %+v", got)
	}
}

func TestParseIPTablesDPT(t *testing.T) {
	out := `Chain DOCKER (2 references)
target     prot opt source               destination
DNAT       tcp  --  0.0.0.0/0            0.0.0.0/0            tcp dpt:24203 to:172.19.0.100:22
DNAT       tcp  --  0.0.0.0/0            0.0.0.0/0            tcp dpt:26000 to:10.0.0.9:80
RETURN     all  --  0.0.0.0/0            0.0.0.0/0
ACCEPT     tcp  --  0.0.0.0/0            172.19.0.100         tcp dpt:22`

	got := parseIPTablesDPT(out)
	if len(got) != 2 {
		t.Fatalf("期望解析出 2 个已占用端口，实际 %d: %+v", len(got), got)
	}
	if !got[24203] || !got[26000] {
		t.Fatalf("端口解析结果不正确: %+v", got)
	}
	// ACCEPT 行不含 DNAT，不应被计入（端口 22 是容器内端口，不是外网端口）
	if got[22] {
		t.Fatalf("ACCEPT 行的端口不应被计入: %+v", got)
	}
}

func TestParseIPTablesDPTNoDockerChain(t *testing.T) {
	// 宿主机没有 DOCKER 链时 iptables 会报错，输出为空
	if got := parseIPTablesDPT(""); len(got) != 0 {
		t.Fatalf("空输入应返回空端口集合，实际 %+v", got)
	}
}

// TestContainerDriversImplementNatPortProvider 容器类平台必须实现端口映射通道，
// 否则 service 层会回落到爱快路由器——而容器环境通常不配爱快，端口映射将不可用。
func TestContainerDriversImplementNatPortProvider(t *testing.T) {
	cases := []struct {
		engine string
		name   string
	}{
		{"OCInterface", "Docker"},
		{"LxContainer", "LXD"},
		{"SmolVM", "SmolVM"},
	}
	for _, tc := range cases {
		d := NewDriver(testConfig(t, tc.engine))
		if d == nil {
			t.Fatalf("%s 驱动未注册", tc.engine)
		}
		if _, ok := d.(NatPortProvider); !ok {
			t.Fatalf("%s（%s）必须实现 NatPortProvider 才能下发端口映射", tc.engine, tc.name)
		}
	}

	// 反向断言：平台通道必须能识别端口占用（不返回「未实现」式的假成功）
	d := NewDriver(testConfig(t, "OCInterface"))
	nat := &model.PortData{WanPort: 0, LanPort: 22, LanAddr: "172.17.0.2"}
	msg, err := d.(NatPortProvider).PortsMap(nat, true)
	_ = err
	if msg != nil && msg.Success {
		// 测试机上没有 iptables/容器网络，此调用预期失败；
		// 若返回成功，说明下发路径没有做任何真实校验。
		if nat.WanPort == 0 {
			t.Fatalf("端口映射返回成功但未回填外网端口（假成功）")
		}
	}
}
