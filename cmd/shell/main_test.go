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

func TestBuildModules_按SHELL_NAME挑出对应外壳并拼出真实ModuleSpec(t *testing.T) {
	dir := t.TempDir()
	configPath := writeJSONFile(t, dir, "shell-config.json", `[
		{"name":"go-core","modules":[
			{"componentId":"mdm/customer","version":"1.0.7","schema":"mdm_customer","httpPort":8080,"extraPorts":{"grpc":9090}},
			{"componentId":"erp/sales","version":"1.0.23","schema":"erp_sales","httpPort":8084,"extraPorts":{"grpc":9094}}
		]},
		{"name":"go-backoffice","modules":[
			{"componentId":"crm/opportunity","version":"1.0.10","schema":"crm_opportunity","httpPort":8102,"extraPorts":{"grpc":9102}}
		]}
	]`)
	envPath := writeJSONFile(t, dir, "shell-env.json", `[
		{"Name":"go-core","Modules":[
			{"ComponentID":"mdm/customer","Env":{"COMPONENT_ID":"mdm/customer"}},
			{"ComponentID":"erp/sales","Env":{"MDM_CUSTOMER_ENDPOINT":"http://127.0.0.1:8080"}}
		]},
		{"Name":"go-backoffice","Modules":[
			{"ComponentID":"crm/opportunity","Env":{"MDM_CUSTOMER_ENDPOINT":"http://host.docker.internal:8080"}}
		]}
	]`)
	t.Setenv("SHELL_CONFIG_JSON", configPath)
	t.Setenv("SHELL_ENV_JSON", envPath)

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
	if specs[1].Env["MDM_CUSTOMER_ENDPOINT"] != "http://127.0.0.1:8080" {
		t.Fatalf("env 没有从 shell-env.json 正确带过来: %+v", specs[1].Env)
	}
	if specs[0].New == nil {
		t.Fatal("New 必须是 moduleRegistry 里真实的构造函数，不能是 nil")
	}
	if specs[0].HTTPPort != 8080 || specs[0].ExtraPorts["grpc"] != 9090 {
		t.Fatalf("端口没有从 shell-config.json 正确带过来: %+v", specs[0])
	}

	// 只装了 go-core 请求的那两个模块，不该把 go-backoffice 的
	// crm/opportunity 也一起带进来。
	for _, s := range specs {
		if s.ComponentID == "crm/opportunity" {
			t.Fatal("go-core 不该装 go-backoffice 的模块")
		}
	}
}

func TestBuildModules_外壳在shell_config里不存在时报错(t *testing.T) {
	dir := t.TempDir()
	configPath := writeJSONFile(t, dir, "shell-config.json", `[{"name":"go-core","modules":[]}]`)
	envPath := writeJSONFile(t, dir, "shell-env.json", `[{"Name":"go-core","Modules":[]}]`)
	t.Setenv("SHELL_CONFIG_JSON", configPath)
	t.Setenv("SHELL_ENV_JSON", envPath)

	if _, err := buildModules("go-infra"); err == nil {
		t.Fatal("SHELL_NAME 在 shell-config.json 里找不到时应该报错，实际没有")
	}
}

// TestBuildModules_外壳还没原子式切换完时报错 覆盖 be-ops shell-env 的
// "外壳整体跳过"行为传导到这里之后该怎么反应：go-infra 在 shell-config
// 里存在（shellconfig.Gen 不看 local: true），但因为还没切换完，
// shell-env.json 里完全没有这个外壳——这里必须报错，不能装出一批完全
// 没有真实 env 的模块。
func TestBuildModules_外壳还没原子式切换完时报错(t *testing.T) {
	dir := t.TempDir()
	configPath := writeJSONFile(t, dir, "shell-config.json", `[
		{"name":"go-infra","modules":[
			{"componentId":"infra/authz","version":"1.0.5","schema":"infra_authz","httpPort":8223}
		]}
	]`)
	envPath := writeJSONFile(t, dir, "shell-env.json", `[]`)
	t.Setenv("SHELL_CONFIG_JSON", configPath)
	t.Setenv("SHELL_ENV_JSON", envPath)

	if _, err := buildModules("go-infra"); err == nil {
		t.Fatal("shell-env.json 里没有这个外壳时应该报错，实际没有")
	}
}

func TestBuildModules_未设置环境变量时报错(t *testing.T) {
	// 显式置空，不依赖"测试进程环境里本来就没有这两个变量"这个假设——
	// t.Setenv 会在测试结束后自动还原，不会污染其它测试。
	t.Setenv("SHELL_CONFIG_JSON", "")
	t.Setenv("SHELL_ENV_JSON", "")
	if _, err := buildModules("go-core"); err == nil {
		t.Fatal("SHELL_CONFIG_JSON/SHELL_ENV_JSON 未设置时应该报错，实际没有")
	}
}
