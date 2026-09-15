package main

import (
	"os"
	"testing"
)

// TestBuildModules_真实11个组件都在moduleRegistry里 是本文件最重要的
// 一条断言：BRICKKIT_SERVED_MEMBERS_CONFIG 能出现的 componentId 只有
// 全项目已知的这 11 个，任何一个漏注册都必须在装配时就报错，不能拖到
// 真机部署才发现"这个模块没起来"。
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

// 阶段四附加 Task 0.6（brickKit v0.4.2）：改成解析平台原生注入的
// BRICKKIT_SERVED_MEMBERS_CONFIG——不再需要单独一个 BRICKKIT_SERVED_MEMBERS
// 变量去筛"这次真的被收编了谁"（这个 JSON 数组本身就已经是筛过的结果），
// 也不再需要 be-ops shell-config 产出、手工贴进 brickkit.yaml 那一整套
// 机制。下面几条用例覆盖这条新链路，真实数据形状取自真机
// `docker exec ... env` 核对过的 BRICKKIT_SERVED_MEMBERS_CONFIG 内容。

func TestBuildModules_解析真实数据形状装配模块(t *testing.T) {
	t.Setenv("BRICKKIT_SERVED_MEMBERS_CONFIG", `[
		{"componentId":"mdm/customer","version":"1.0.9","httpPort":8080,"extraPorts":[{"name":"grpc","port":9090}],"config":{"authzBundleUrl":"http://infra-authz-1-0-7:8223/authz/bundle","pgSchema":"mdm_customer"}},
		{"componentId":"erp/sales","version":"1.0.25","httpPort":8084,"extraPorts":[{"name":"grpc","port":9094}],"config":{"defaultWarehouseId":"1","pgSchema":"erp_sales"}}
	]`)

	specs, err := buildModules()
	if err != nil {
		t.Fatalf("buildModules 失败: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("期望装 2 个模块，实际 %d 个: %+v", len(specs), specs)
	}
	// 数组顺序必须原样保留（拓扑序，迁移/启动顺序靠它）。
	if specs[0].ComponentID != "mdm/customer" || specs[1].ComponentID != "erp/sales" {
		t.Fatalf("模块顺序被打乱: %+v", specs)
	}
	// config 键必须从原始驼峰形式转成 SCREAMING_SNAKE_CASE——besdk.Config
	// 内部按这个规则查找，转换漏了模块会真机 panic（阶段四附加 Task 0.4
	// 已经真机撞过一次这类坑，见 field-tested-pitfalls-log.md）。
	if specs[0].Env["AUTHZ_BUNDLE_URL"] != "http://infra-authz-1-0-7:8223/authz/bundle" {
		t.Fatalf("env key 没有正确转成 SCREAMING_SNAKE_CASE: %+v", specs[0].Env)
	}
	if specs[1].Env["DEFAULT_WAREHOUSE_ID"] != "1" {
		t.Fatalf("env key 没有正确转成 SCREAMING_SNAKE_CASE: %+v", specs[1].Env)
	}
	// pgSchema 要单独抽成 Schema 字段（原始驼峰 key，不经过大写下划线
	// 转换那条路径）。
	if specs[0].Schema != "mdm_customer" || specs[1].Schema != "erp_sales" {
		t.Fatalf("Schema 没有从 config.pgSchema 正确带过来: %+v %+v", specs[0], specs[1])
	}
	if specs[0].New == nil {
		t.Fatal("New 必须是 moduleRegistry 里真实的构造函数，不能是 nil")
	}
	// extraPorts 是数组形状（[{name,port}]），必须正确转成 map。
	if specs[0].HTTPPort != 8080 || specs[0].ExtraPorts["grpc"] != 9090 {
		t.Fatalf("端口没有正确带过来: %+v", specs[0])
	}
}

// TestBuildModules_零成员时装出零模块 是"这次没有任何成员被收编"的合法
// 状态（都还没切换、或者都被临时摘掉），brickKit 保证这种情况下变量值
// 是 `[]`，不是变量缺失——必须是零模块，不能报错、也不能退化成
// "全部实例化"。
func TestBuildModules_零成员时装出零模块(t *testing.T) {
	t.Setenv("BRICKKIT_SERVED_MEMBERS_CONFIG", `[]`)

	specs, err := buildModules()
	if err != nil {
		t.Fatalf("buildModules 失败: %v", err)
	}
	if len(specs) != 0 {
		t.Fatalf("空数组应该装出 0 个模块，实际 %+v", specs)
	}
}

// TestBuildModules_变量未设置时报错 防的是"这个容器压根不是被 servedBy
// 正常收编启动的"（比如手动 docker run 忘了传这个变量）——brickKit 总会
// 至少注入一个 `[]`，真的读不到必须报错，不能悄悄退化成某种默认行为
// 掩盖真实的配置错误。
func TestBuildModules_变量未设置时报错(t *testing.T) {
	os.Unsetenv("BRICKKIT_SERVED_MEMBERS_CONFIG")

	if _, err := buildModules(); err == nil {
		t.Fatal("BRICKKIT_SERVED_MEMBERS_CONFIG 未设置时应该报错，实际没有")
	}
}

func TestBuildModules_不是合法JSON时报错(t *testing.T) {
	t.Setenv("BRICKKIT_SERVED_MEMBERS_CONFIG", "不是 JSON")
	if _, err := buildModules(); err == nil {
		t.Fatal("BRICKKIT_SERVED_MEMBERS_CONFIG 不是合法 JSON 时应该报错，实际没有")
	}
}

// TestBuildModules_成员在moduleRegistry里找不到时报错 防的是"这个外壳
// 镜像没有编译进这个模块的代码，但 brickkit.yaml 却把它 servedBy 到了
// 这个外壳"——这是一个真实的配置错误（servedBy 指错了外壳，或者镜像
// 忘了加这个模块的 import），必须报错，不能悄悄跳过导致这个模块的业务
// 功能整个消失且没有任何症状。
func TestBuildModules_成员在moduleRegistry里找不到时报错(t *testing.T) {
	t.Setenv("BRICKKIT_SERVED_MEMBERS_CONFIG", `[
		{"componentId":"hrm/payroll","version":"1.0.0","httpPort":8090,"config":{"pgSchema":"hrm_payroll"}}
	]`)

	if _, err := buildModules(); err == nil {
		t.Fatal("moduleRegistry 里没有这个组件时应该报错，实际没有")
	}
}

// TestBuildModules_缺pgSchema时报错 防的是数据不完整——每个组件都必须
// 有 pgSchema（registry/schemas.tsv 的既定值），缺了说明
// BRICKKIT_SERVED_MEMBERS_CONFIG 的数据本身有问题，不该假装这个模块
// 不需要 schema。
func TestBuildModules_缺pgSchema时报错(t *testing.T) {
	t.Setenv("BRICKKIT_SERVED_MEMBERS_CONFIG", `[
		{"componentId":"mdm/customer","version":"1.0.9","httpPort":8080,"config":{}}
	]`)

	if _, err := buildModules(); err == nil {
		t.Fatal("config 里没有 pgSchema 时应该报错，实际没有")
	}
}

// TestConfigEnvVarName_跟brickKit自己的EnvVarName算法逐字对应 覆盖几个
// 真实项目里出现过的 key 形状（简单驼峰、连续大写、纯小写单词）。
func TestConfigEnvVarName_跟brickKit自己的EnvVarName算法逐字对应(t *testing.T) {
	cases := map[string]string{
		"pgSchema":           "PG_SCHEMA",
		"authzBundleUrl":     "AUTHZ_BUNDLE_URL",
		"defaultWarehouseId": "DEFAULT_WAREHOUSE_ID",
		"otelBaseUrl":        "OTEL_BASE_URL",
	}
	for in, want := range cases {
		if got := configEnvVarName(in); got != want {
			t.Errorf("configEnvVarName(%q) = %q，期望 %q", in, got, want)
		}
	}
}
