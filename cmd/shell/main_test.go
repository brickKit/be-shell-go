package main

import (
	"os"
	"testing"
)

// TestBuildModules_真实11个组件都在moduleRegistry里 是本文件最重要的
// 一条断言：SHELL_CONFIG_JSON 能出现的 componentId 只有全项目已知的这
// 11 个（见 tools/be-ops/internal/genyaml 读到的 assembly.yaml shell
// 字段），任何一个漏注册都必须在装配时就报错，不能拖到真机部署才发现
// "这个模块没起来"。
func TestBuildModules_真实11个组件都在moduleRegistry里(t *testing.T) {
	want := []string{
		"crm/opportunity", "erp/finance", "erp/inventory", "erp/sales",
		"infra/authz", "infra/iam-casdoor", "infra/notification", "infra/workflow",
		"integration/im-dingtalk", "mdm/customer", "mdm/product",
	}
	if len(moduleRegistry) != len(want) {
		t.Fatalf("moduleRegistry 有 %d 个条目，期望 %d 个", len(moduleRegistry), len(want))
	}
	for _, id := range want {
		if _, ok := moduleRegistry[id]; !ok {
			t.Errorf("moduleRegistry 缺 %s", id)
		}
	}
}

// 阶段四附加 Task 0.4：SHELL_CONFIG_JSON 从"文件路径"改成"内容本身"
// ——值直接是这一个外壳自己的 modules 数组（be-ops shell-config --shell
// <name> 打印出来的那一行），不再需要按 SHELL_NAME 从多个外壳里挑一个，
// buildModules 因此不再接收 shellName 参数。下面几条用例覆盖这条新链路。

func TestBuildModules_解析SHELL_CONFIG_JSON并按BRICKKIT_SERVED_MEMBERS筛一遍(t *testing.T) {
	t.Setenv("SHELL_CONFIG_JSON", `[
		{"componentId":"mdm/customer","version":"1.0.7","schema":"mdm_customer","httpPort":8080,"extraPorts":{"grpc":9090},"config":{"pgSchema":"mdm_customer"}},
		{"componentId":"erp/sales","version":"1.0.23","schema":"erp_sales","httpPort":8084,"extraPorts":{"grpc":9094},"config":{"MDM_CUSTOMER_ENDPOINT":"http://mdm-customer-1-0-7:8080"}}
	]`)
	// 只有 mdm/customer 和 erp/sales 这次被收编。
	t.Setenv("BRICKKIT_SERVED_MEMBERS", "mdm-customer-1-0-7,erp-sales-1-0-23")

	specs, err := buildModules()
	if err != nil {
		t.Fatalf("buildModules 失败: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("期望装 2 个模块，实际 %d 个: %+v", len(specs), specs)
	}
	// SHELL_CONFIG_JSON 的顺序必须原样保留（拓扑序，迁移/启动顺序靠它）。
	if specs[0].ComponentID != "mdm/customer" || specs[1].ComponentID != "erp/sales" {
		t.Fatalf("模块顺序被打乱: %+v", specs)
	}
	if specs[1].Env["MDM_CUSTOMER_ENDPOINT"] != "http://mdm-customer-1-0-7:8080" {
		t.Fatalf("env 没有从 Config 字段正确带过来: %+v", specs[1].Env)
	}
	if specs[0].New == nil {
		t.Fatal("New 必须是 moduleRegistry 里真实的构造函数，不能是 nil")
	}
	if specs[0].HTTPPort != 8080 || specs[0].ExtraPorts["grpc"] != 9090 {
		t.Fatalf("端口没有正确带过来: %+v", specs[0])
	}
}

// TestBuildModules_BRICKKIT_SERVED_MEMBERS未列出的成员被跳过不报错 是
// servedBy 场景下的正常状态（还没切过来、或者被临时摘掉的成员），不是
// 错误——其余成员该怎么装还怎么装。
func TestBuildModules_BRICKKIT_SERVED_MEMBERS未列出的成员被跳过不报错(t *testing.T) {
	t.Setenv("SHELL_CONFIG_JSON", `[
		{"componentId":"mdm/customer","version":"1.0.7","schema":"mdm_customer","httpPort":8080},
		{"componentId":"mdm/product","version":"1.0.8","schema":"mdm_product","httpPort":8082}
	]`)
	t.Setenv("BRICKKIT_SERVED_MEMBERS", "mdm-customer-1-0-7")

	specs, err := buildModules()
	if err != nil {
		t.Fatalf("buildModules 失败: %v", err)
	}
	if len(specs) != 1 || specs[0].ComponentID != "mdm/customer" {
		t.Fatalf("应该只装 mdm/customer 一个模块，实际 %+v", specs)
	}
}

// TestBuildModules_BRICKKIT_SERVED_MEMBERS为空字符串时零模块 是"这次
// 没有任何成员被收编"的合法状态（都还没切换、或者都被临时摘掉），必须
// 是零模块，不能报错、也不能退化成"全部实例化"。
func TestBuildModules_BRICKKIT_SERVED_MEMBERS为空字符串时零模块(t *testing.T) {
	t.Setenv("SHELL_CONFIG_JSON", `[
		{"componentId":"mdm/customer","version":"1.0.7","schema":"mdm_customer","httpPort":8080}
	]`)
	t.Setenv("BRICKKIT_SERVED_MEMBERS", "")

	specs, err := buildModules()
	if err != nil {
		t.Fatalf("buildModules 失败: %v", err)
	}
	if len(specs) != 0 {
		t.Fatalf("空字符串应该装出 0 个模块，实际 %+v", specs)
	}
}

// TestBuildModules_BRICKKIT_SERVED_MEMBERS未设置时报错 区分"变量不存在"
// 与"变量是空字符串"——前者意味着这个容器可能不是被 servedBy 正常收编
// 启动的，必须报错，不能悄悄退化成旧行为掩盖真实配置错误。
func TestBuildModules_BRICKKIT_SERVED_MEMBERS未设置时报错(t *testing.T) {
	t.Setenv("SHELL_CONFIG_JSON", `[{"componentId":"mdm/customer","version":"1.0.7","schema":"mdm_customer","httpPort":8080}]`)
	os.Unsetenv("BRICKKIT_SERVED_MEMBERS")

	if _, err := buildModules(); err == nil {
		t.Fatal("BRICKKIT_SERVED_MEMBERS 未设置时应该报错，实际没有")
	}
}

// TestBuildModules_BRICKKIT_SERVED_MEMBERS里有SHELL_CONFIG_JSON找不到的成员时报错
// 防的是"brickkit.yaml 改完之后忘了重新跑 be-ops shell-config --shell
// 把新字符串贴回 config.shellConfigJson"这类基线漂移——平台说这个成员
// 被收编了，我们自己的合并清单却不知道它是谁，不该悄悄装出一批不完整
// 的模块。
func TestBuildModules_BRICKKIT_SERVED_MEMBERS里有SHELL_CONFIG_JSON找不到的成员时报错(t *testing.T) {
	t.Setenv("SHELL_CONFIG_JSON", `[
		{"componentId":"mdm/customer","version":"1.0.7","schema":"mdm_customer","httpPort":8080}
	]`)
	t.Setenv("BRICKKIT_SERVED_MEMBERS", "mdm-customer-1-0-7,erp-sales-1-0-23")

	if _, err := buildModules(); err == nil {
		t.Fatal("BRICKKIT_SERVED_MEMBERS 里有 SHELL_CONFIG_JSON 找不到的成员时应该报错，实际没有")
	}
}

func TestBuildModules_SHELL_CONFIG_JSON未设置时报错(t *testing.T) {
	// 显式置空，不依赖"测试进程环境里本来就没有这个变量"这个假设——
	// t.Setenv 会在测试结束后自动还原，不会污染其它测试。
	t.Setenv("SHELL_CONFIG_JSON", "")
	t.Setenv("BRICKKIT_SERVED_MEMBERS", "")
	if _, err := buildModules(); err == nil {
		t.Fatal("SHELL_CONFIG_JSON 未设置时应该报错，实际没有")
	}
}

func TestBuildModules_SHELL_CONFIG_JSON不是合法JSON时报错(t *testing.T) {
	t.Setenv("SHELL_CONFIG_JSON", "不是 JSON")
	t.Setenv("BRICKKIT_SERVED_MEMBERS", "")
	if _, err := buildModules(); err == nil {
		t.Fatal("SHELL_CONFIG_JSON 不是合法 JSON 时应该报错，实际没有")
	}
}
