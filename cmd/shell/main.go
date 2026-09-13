// be-shell-go 的进程入口——阶段四 Task 6：11 个真实组件的合并进程集成
// 测试已经全部通过（internal/shell/real_modules_test.go），这里接的是
// 真机部署这一步：同一份镜像按 SHELL_NAME 环境变量的值，装配出
// go-core/go-backoffice/go-infra 三个外壳实例中的一个（阶段四调研记录
// 04 §12 的既定结论："部署几份实例纯粹是运行时配置问题"，internal/shell.Run
// 本来就接受任意 Modules 子集，不需要为每个外壳单独建一份镜像/仓库）。
//
// 哪些模块属于哪个外壳、端口/schema、依赖地址怎么改写，全部来自
// be-ops 产出 4（shell-config.json）/产出 7（shell-env.json）——这里只
// 负责两件事：①读那两份数据文件、按 SHELL_NAME 挑出自己要装的那个外壳；
// ②把数据里的 componentId 字符串映到真实的 Go 源码 import（这一步不能
// 数据驱动，ModuleSpec.New 必须是静态 import，见 internal/shell.ModuleSpec
// 的字段注释）。除此之外不做任何装配决策——哪个组件该不该合并、合并成
// 几个外壳，那是 assembly.yaml/registry/schemas.tsv 的既定数据，不是这
// 个文件该决定的。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/be-shell-go/internal/shell"

	crmopportunity "github.com/brickKit/crm-opportunity/backend/module"
	erpfinance "github.com/brickKit/erp-finance/backend/module"
	erpinventory "github.com/brickKit/erp-inventory/backend/module"
	erpsales "github.com/brickKit/erp-sales/backend/module"
	infraauthz "github.com/brickKit/infra-authz/backend/module"
	infraiamcasdoor "github.com/brickKit/infra-iam-casdoor/backend/module"
	infranotification "github.com/brickKit/infra-notification/backend/module"
	infraworkflow "github.com/brickKit/infra-workflow/backend/module"
	integrationimdingtalk "github.com/brickKit/integration-im-dingtalk/backend/module"
	mdmcustomer "github.com/brickKit/mdm-customer/backend/module"
	mdmproduct "github.com/brickKit/mdm-product/backend/module"
)

// moduleCtor 是能装进 shell.ModuleSpec.New 的构造函数。
type moduleCtor func(context.Context, *besdk.Runtime) (*besdk.Module, error)

// moduleRegistry 是本仓库唯一"数据 componentId 字符串 → 真实 Go 代码"
// 的静态映射——be-ops 产出 4/7 只携带 componentId 这一个字符串，具体
// New 是哪个包的哪个函数，只能在源码里写死（同 internal/shell.ModuleSpec
// 的既有注释）。全部 11 个真实组件的 New 都注册在这里，不分外壳——
// 三个外壳实例共用同一份镜像/同一个二进制，实际装哪几个由 shell-config
// 数据在运行时决定，源码这一层不需要（也不应该）知道"这次是哪个外壳"。
var moduleRegistry = map[string]moduleCtor{
	"crm/opportunity":         crmopportunity.New,
	"erp/finance":             erpfinance.New,
	"erp/inventory":           erpinventory.New,
	"erp/sales":               erpsales.New,
	"infra/authz":             infraauthz.New,
	"infra/iam-casdoor":       infraiamcasdoor.New,
	"infra/notification":      infranotification.New,
	"infra/workflow":          infraworkflow.New,
	"integration/im-dingtalk": integrationimdingtalk.New,
	"mdm/customer":            mdmcustomer.New,
	"mdm/product":             mdmproduct.New,
}

// shellConfigModule/shellConfigShell 是 be-ops 产出 4（shell-config.json）
// 的形状——字段名与 tools/be-ops/internal/shellconfig.Module/Shell 逐一
// 对应。两个仓库是两个独立的 Go module，`internal/` 的可见性规则不允许
// 直接 import 那边的类型，这里按 JSON 字段名重新声明一份，耦合点是
// 数据形状，不是 Go 类型（同 be-ops shell-env 包文档："自己另算的那份，
// 早晚和平台的算法分叉"这条判据反过来也成立：约定数据格式，而不是
// 共享代码，才不会跨仓库耦合出一条隐藏的编译期依赖）。
type shellConfigModule struct {
	ComponentID string         `json:"componentId"`
	Version     string         `json:"version"`
	Schema      string         `json:"schema"`
	HTTPPort    int            `json:"httpPort"`
	ExtraPorts  map[string]int `json:"extraPorts,omitempty"`
}

type shellConfigShell struct {
	Name    string              `json:"name"`
	Modules []shellConfigModule `json:"modules"`
}

// shellEnvModule/shellEnvShell 是产出 7（shell-env.json）的形状——
// shellenv.ModuleEnv/ShellEnv 目前没有显式 json tag，Go 默认按字段名
// 编码，这里原样对应（大写字段名），不是笔误。
type shellEnvModule struct {
	ComponentID string
	Env         map[string]string
}

type shellEnvShell struct {
	Name    string
	Modules []shellEnvModule
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	shellName := os.Getenv("SHELL_NAME")
	if shellName == "" {
		log.Fatal("SHELL_NAME 未设置（go-core|go-backoffice|go-infra 之一，见 shell-compose.yml）")
	}

	modules, err := buildModules(shellName)
	if err != nil {
		log.Fatalf("装配外壳 %s 失败：%v", shellName, err)
	}

	pgDSN, err := besdk.BuildPGDSN()
	if err != nil {
		log.Fatalf("拼外壳共享数据库连接串失败：%v", err)
	}

	// ⚠️ SHELL_HEALTH_PORT 不是平台注入的（外壳根本不是 brickKit
	// 组件）——be-ops 产出 8（shell-compose.yml）生成时会把这个端口
	// 写进 healthcheck 配置，同时把同一个值以这个变量名注入外壳容器，
	// 这里只负责读、不负责决定这个端口该是多少。产出 8 落地前先给一个
	// 明显不常用的默认值，方便本地手动验证骨架。
	healthPort, _ := strconv.Atoi(os.Getenv("SHELL_HEALTH_PORT"))
	if healthPort == 0 {
		healthPort = 18888
	}

	cfg := shell.Config{
		ShellName:      "be-shell-go-" + shellName,
		OTelBaseURL:    os.Getenv("OTEL_BASE_URL"),
		PGDSN:          pgDSN,
		NATSURL:        besdk.BuildNATSURL(),
		IamJwksURL:     os.Getenv("IAM_JWKS_URL"),
		AuthzBundleURL: os.Getenv("AUTHZ_BUNDLE_URL"),
		HealthPort:     healthPort,
		Modules:        modules,
	}

	if err := shell.Run(ctx, cfg, besdk.NewLogger(cfg.ShellName)); err != nil {
		log.Fatalf("外壳异常退出：%v", err)
	}
}

// buildModules 读 SHELL_CONFIG_JSON（产出 4）+ SHELL_ENV_JSON（产出 7）
// 两份文件，挑出 shellName 对应的那个外壳，拼出真实的 []shell.ModuleSpec。
// 两份文件路径都要求显式配置（不给默认路径）——外壳到底该读哪份数据，
// 应该由部署那一层（shell-compose.yml）显式决定，不该在这里悄悄假设
// 一个约定俗成的相对路径，那类假设正是"自己另算一遍"的开端。
func buildModules(shellName string) ([]shell.ModuleSpec, error) {
	configPath := os.Getenv("SHELL_CONFIG_JSON")
	envPath := os.Getenv("SHELL_ENV_JSON")
	if configPath == "" || envPath == "" {
		return nil, fmt.Errorf("SHELL_CONFIG_JSON/SHELL_ENV_JSON 未设置（be-ops shell-config/shell-env 的产出路径）")
	}

	configShells, err := readShellConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("读 %s 失败: %w", configPath, err)
	}
	envShells, err := readShellEnv(envPath)
	if err != nil {
		return nil, fmt.Errorf("读 %s 失败: %w", envPath, err)
	}

	configShell, ok := findConfigShell(configShells, shellName)
	if !ok {
		return nil, fmt.Errorf("shell-config.json 里没有外壳 %q（是不是 brickkit.yaml 还没原子式切换，或者 SHELL_NAME 拼错了）", shellName)
	}
	envByComponent, err := envMapForShell(envShells, shellName)
	if err != nil {
		return nil, err
	}

	// shell-config.json 里的模块顺序已经是 be-ops 按依赖关系拓扑排序过的
	// 结果（tools/be-ops/internal/shellconfig 的既有职责），这里原样保留
	// 顺序传给 shell.Run——迁移与启动顺序由这个顺序决定，本文件不重新排序。
	specs := make([]shell.ModuleSpec, 0, len(configShell.Modules))
	for _, m := range configShell.Modules {
		ctor, ok := moduleRegistry[m.ComponentID]
		if !ok {
			return nil, fmt.Errorf("组件 %s 在 shell-config.json 里，但 moduleRegistry 没有登记它的真实 New 函数——是不是漏了给它加 import", m.ComponentID)
		}
		env, ok := envByComponent[m.ComponentID]
		if !ok {
			return nil, fmt.Errorf("组件 %s 在 shell-config.json 里，但 shell-env.json 的外壳 %q 下找不到它对应的环境变量", m.ComponentID, shellName)
		}
		specs = append(specs, shell.ModuleSpec{
			ComponentID:      m.ComponentID,
			ComponentVersion: m.Version,
			Env:              env,
			HTTPPort:         m.HTTPPort,
			ExtraPorts:       m.ExtraPorts,
			Schema:           m.Schema,
			New:              ctor,
		})
	}
	return specs, nil
}

func readShellConfig(path string) ([]shellConfigShell, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var shells []shellConfigShell
	if err := json.Unmarshal(data, &shells); err != nil {
		return nil, err
	}
	return shells, nil
}

func readShellEnv(path string) ([]shellEnvShell, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var shells []shellEnvShell
	if err := json.Unmarshal(data, &shells); err != nil {
		return nil, err
	}
	return shells, nil
}

func findConfigShell(shells []shellConfigShell, name string) (shellConfigShell, bool) {
	for _, s := range shells {
		if s.Name == name {
			return s, true
		}
	}
	return shellConfigShell{}, false
}

func envMapForShell(shells []shellEnvShell, name string) (map[string]map[string]string, error) {
	for _, s := range shells {
		if s.Name != name {
			continue
		}
		out := make(map[string]map[string]string, len(s.Modules))
		for _, m := range s.Modules {
			out[m.ComponentID] = m.Env
		}
		return out, nil
	}
	return nil, fmt.Errorf("shell-env.json 里没有外壳 %q（是不是 be-ops shell-env 生成时这个外壳还没原子式切换完，被跳过了）", name)
}
