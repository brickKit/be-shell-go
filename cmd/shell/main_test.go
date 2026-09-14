package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeJSONFile 是本文件测试用的小工具——真实的 shell-config.json/
// shell-env.json 由 be-ops 产出，这里手写最小夹具即可，不需要真的跑
// be-ops 二进制（那条真实产出链路已经在 tools/be-ops 自己的测试里
// 验证过，这里只测 buildModules 这一步"怎么消费"）。
func writeJSONFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestBuildModules_真实11个组件都在moduleRegistry里 是本文件最重要的
// 一条断言：shell-config.json 能出现的 componentId 只有全项目已知的这
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

// 阶段四附加 Task 0.2/0.3：shell-env.json（产出 7）整体退休，
// BRICKKIT_SERVED_MEMBERS（平台原生注入）取代它成为"这次谁真的被收编"
// 的数据源，模块自己的 configSchema 值改由 shell-config.json 的
// Config 字段（产出 4）直接携带。下面几条用例覆盖这条新链路。

func TestBuildModules_按SHELL_NAME挑出对应外壳_按BRICKKIT_SERVED_MEMBERS再筛一遍(t *testing.T) {
	dir := t.TempDir()
	configPath := writeJSONFile(t, dir, "shell-config.json", `[
		{"name":"go-core","modules":[
			{"componentId":"mdm/customer","version":"1.0.7","schema":"mdm_customer","httpPort":8080,"extraPorts":{"grpc":9090},"config":{"pgSchema":"mdm_customer"}},
			{"componentId":"erp/sales","version":"1.0.23","schema":"erp_sales","httpPort":8084,"extraPorts":{"grpc":9094},"config":{"MDM_CUSTOMER_ENDPOINT":"http://mdm-customer-1-0-7:8080"}}
		]},
		{"name":"go-backoffice","modules":[
			{"componentId":"crm/opportunity","version":"1.0.10","schema":"crm_opportunity","httpPort":8102,"extraPorts":{"grpc":9102}}
		]}
	]`)
	t.Setenv("SHELL_CONFIG_JSON", configPath)
	// 只有 mdm/customer 和 erp/sales 这次被收编，crm/opportunity 没有
	// （即使它跟 go-core 不是同一个外壳，这里也不该被带进来——下面单独
	// 有一条用例测"BRICKKIT_SERVED_MEMBERS 里有个 shell-config 里根本
	// 找不到的名字"这种情况）。
	t.Setenv("BRICKKIT_SERVED_MEMBERS", "mdm-customer-1-0-7,erp-sales-1-0-23")

	specs, err := buildModules("go-core")
	if err != nil {
		t.Fatalf("buildModules 失败: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("期望 go-core 装 2 个模块，实际 %d 个: %+v", len(specs), specs)
	}
	// shell-config.json 的顺序必须原样保留（拓扑序，迁移/启动顺序靠它）。
	if specs[0].ComponentID != "mdm/customer" || specs[1].ComponentID != "erp/sales" {
		t.Fatalf("模块顺序被打乱: %+v", specs)
	}
	if specs[1].Env["MDM_CUSTOMER_ENDPOINT"] != "http://mdm-customer-1-0-7:8080" {
		t.Fatalf("env 没有从 shell-config.json 的 Config 字段正确带过来: %+v", specs[1].Env)
	}
	if specs[0].New == nil {
		t.Fatal("New 必须是 moduleRegistry 里真实的构造函数，不能是 nil")
	}
	if specs[0].HTTPPort != 8080 || specs[0].ExtraPorts["grpc"] != 9090 {
		t.Fatalf("端口没有从 shell-config.json 正确带过来: %+v", specs[0])
	}
}

// TestBuildModules_BRICKKIT_SERVED_MEMBERS未列出的成员被跳过不报错 是
// servedBy 场景下的正常状态（还没切过来、或者被临时摘掉的成员），不是
// 错误——同一个外壳的其余成员该怎么装还怎么装。
func TestBuildModules_BRICKKIT_SERVED_MEMBERS未列出的成员被跳过不报错(t *testing.T) {
	dir := t.TempDir()
	configPath := writeJSONFile(t, dir, "shell-config.json", `[
		{"name":"go-core","modules":[
			{"componentId":"mdm/customer","version":"1.0.7","schema":"mdm_customer","httpPort":8080},
			{"componentId":"mdm/product","version":"1.0.8","schema":"mdm_product","httpPort":8082}
		]}
	]`)
	t.Setenv("SHELL_CONFIG_JSON", configPath)
	t.Setenv("BRICKKIT_SERVED_MEMBERS", "mdm-customer-1-0-7")

	specs, err := buildModules("go-core")
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
	dir := t.TempDir()
	configPath := writeJSONFile(t, dir, "shell-config.json", `[
		{"name":"go-core","modules":[
			{"componentId":"mdm/customer","version":"1.0.7","schema":"mdm_customer","httpPort":8080}
		]}
	]`)
	t.Setenv("SHELL_CONFIG_JSON", configPath)
	t.Setenv("BRICKKIT_SERVED_MEMBERS", "")

	specs, err := buildModules("go-core")
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
	dir := t.TempDir()
	configPath := writeJSONFile(t, dir, "shell-config.json", `[{"name":"go-core","modules":[]}]`)
	t.Setenv("SHELL_CONFIG_JSON", configPath)
	os.Unsetenv("BRICKKIT_SERVED_MEMBERS")

	if _, err := buildModules("go-core"); err == nil {
		t.Fatal("BRICKKIT_SERVED_MEMBERS 未设置时应该报错，实际没有")
	}
}

// TestBuildModules_BRICKKIT_SERVED_MEMBERS里有shell_config找不到的成员时报错
// 防的是"brickkit.yaml 改完之后忘了重新跑 be-ops shell-config"这类基线
// 漂移——平台说这个成员被收编了，我们自己的合并清单却不知道它是谁，
// 不该悄悄装出一批不完整的模块。
func TestBuildModules_BRICKKIT_SERVED_MEMBERS里有shell_config找不到的成员时报错(t *testing.T) {
	dir := t.TempDir()
	configPath := writeJSONFile(t, dir, "shell-config.json", `[
		{"name":"go-core","modules":[
			{"componentId":"mdm/customer","version":"1.0.7","schema":"mdm_customer","httpPort":8080}
		]}
	]`)
	t.Setenv("SHELL_CONFIG_JSON", configPath)
	t.Setenv("BRICKKIT_SERVED_MEMBERS", "mdm-customer-1-0-7,erp-sales-1-0-23")

	if _, err := buildModules("go-core"); err == nil {
		t.Fatal("BRICKKIT_SERVED_MEMBERS 里有 shell-config.json 找不到的成员时应该报错，实际没有")
	}
}

func TestBuildModules_外壳在shell_config里不存在时报错(t *testing.T) {
	dir := t.TempDir()
	configPath := writeJSONFile(t, dir, "shell-config.json", `[{"name":"go-core","modules":[]}]`)
	t.Setenv("SHELL_CONFIG_JSON", configPath)
	t.Setenv("BRICKKIT_SERVED_MEMBERS", "")

	if _, err := buildModules("go-infra"); err == nil {
		t.Fatal("SHELL_NAME 在 shell-config.json 里找不到时应该报错，实际没有")
	}
}

func TestBuildModules_SHELL_CONFIG_JSON未设置时报错(t *testing.T) {
	// 显式置空，不依赖"测试进程环境里本来就没有这个变量"这个假设——
	// t.Setenv 会在测试结束后自动还原，不会污染其它测试。
	t.Setenv("SHELL_CONFIG_JSON", "")
	t.Setenv("BRICKKIT_SERVED_MEMBERS", "")
	if _, err := buildModules("go-core"); err == nil {
		t.Fatal("SHELL_CONFIG_JSON 未设置时应该报错，实际没有")
	}
}
