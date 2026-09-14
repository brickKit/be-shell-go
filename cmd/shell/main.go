// be-shell-go 的进程入口——阶段四 Task 6：11 个真实组件的合并进程集成
// 测试已经全部通过（internal/shell/real_modules_test.go），这里接的是
// 真机部署这一步：同一份镜像按 SHELL_NAME 环境变量的值，装配出
// go-core/go-backoffice/go-infra 三个外壳实例中的一个（阶段四调研记录
// 04 §12 的既定结论："部署几份实例纯粹是运行时配置问题"，internal/shell.Run
// 本来就接受任意 Modules 子集，不需要为每个外壳单独建一份镜像/仓库）。
//
// 哪些模块属于这个外壳、端口/schema/configSchema 值，全部来自 be-ops
// 产出 4（`be-ops shell-config --shell <name>` 打印出的这一个外壳自己
// 的 modules 数组）——这里负责两件事：①按 BRICKKIT_SERVED_MEMBERS（平台
// 原生注入，servedBy 场景下"这次真的被收编、活着"的成员清单）筛一遍，
// 只装真的被收编的那些——SHELL_CONFIG_JSON 里列出的是"这个外壳理论上
// 有哪些成员"，不等于"这次部署真的都被收编了"（一个成员可能还没切成
// servedBy、或者被临时摘掉）；②把数据里的 componentId 字符串映到真实的
// Go 源码 import（这一步不能数据驱动，ModuleSpec.New 必须是静态 import，
// 见 internal/shell.ModuleSpec 的字段注释）。除此之外不做任何装配决策——
// 哪个组件该不该合并、合并成几个外壳，那是 assembly.yaml/registry/
// schemas.tsv 的既定数据，不是这个文件该决定的。
//
// ⚠️ 阶段四附加 Task 0.2/0.3：原来还要读产出 7（shell-env.json）做依赖
// 地址改写 + 导出到进程环境（internal/shell.exportDependencyEndpoints）
// ——servedBy 落地后这两步都不需要了：brickKit 自己在生成阶段就把
// *_ENDPOINT 类变量直接合并进外壳容器自己的 os.Environ()，这个进程一
// 启动就已经看得见，不需要本文件/internal/shell 再做任何搬运。完整
// 调研过程见装配仓库 docs/plans/04b-验证记录.md Task 0.2。
//
// ⚠️ 阶段四附加 Task 0.4：SHELL_CONFIG_JSON 从"文件路径"改成了"内容本身"
// ——servedBy 外壳没有自己的 component.yaml 之外的任何东西可以挂载
// （brickKit 的 manifest 模型没有 volumes 字段），"文件路径 + 挂载卷"这
// 条路在没有 volumes 的世界里走不通。现在 SHELL_CONFIG_JSON 的值直接是
// `be-ops shell-config --shell <name>` 打印出的、这一个外壳自己的
// modules 数组（compact JSON），跟 infra/authz 的 permissionCatalog 是
// 同一种模式——一段生成的字符串，写死在 brickkit.yaml 该外壳组件的
// config.shellConfigJson 里，源头数据变了就重新跑一次那条命令、手动贴
// 回去。也因此不再需要"按 SHELL_NAME 从多个外壳里挑一个"这一步——
// 这个环境变量的内容从生成的那一刻起就已经只属于这一个外壳。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
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

// shellConfigModule 是 be-ops 产出 4（`shell-config --shell <name>` 打印
// 出的这一个外壳自己的 modules 数组）里一条的形状——字段名与
// tools/be-ops/internal/shellconfig.Module 逐一对应。两个仓库是两个
// 独立的 Go module，`internal/` 的可见性规则不允许直接 import 那边的
// 类型，这里按 JSON 字段名重新声明一份，耦合点是数据形状，不是 Go 类型
// （同 be-ops genyaml 包文档："自己另算的那份，早晚和平台的算法分叉"这
// 条判据反过来也成立：约定数据格式，而不是共享代码，才不会跨仓库耦合
// 出一条隐藏的编译期依赖）。
type shellConfigModule struct {
	ComponentID string         `json:"componentId"`
	Version     string         `json:"version"`
	Schema      string         `json:"schema"`
	HTTPPort    int            `json:"httpPort"`
	ExtraPorts  map[string]int `json:"extraPorts,omitempty"`
	// Config 是这个模块自己的 configSchema 解析结果（component.yaml 的
	// default 与 brickkit.yaml 的 config: 字面量已经合并好），阶段四
	// 附加 Task 0.2 起取代了原来单独一份 shell-env.json 的 Env 字段。
	Config map[string]string `json:"config,omitempty"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	shellName := os.Getenv("SHELL_NAME")
	if shellName == "" {
		log.Fatal("SHELL_NAME 未设置（go-core|go-backoffice|go-infra 之一，configSchema 的 shellName 项）")
	}

	modules, err := buildModules()
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

// buildModules 解析 SHELL_CONFIG_JSON（产出 4，`be-ops shell-config
// --shell <name>` 打印出的内容，这一个外壳自己的 modules 数组），再按
// BRICKKIT_SERVED_MEMBERS 筛出这次真的被收编、活着的成员，拼出真实的
// []shell.ModuleSpec。
func buildModules() ([]shell.ModuleSpec, error) {
	raw, ok := os.LookupEnv("SHELL_CONFIG_JSON")
	if !ok || raw == "" {
		return nil, fmt.Errorf("SHELL_CONFIG_JSON 未设置（be-ops shell-config --shell <name> 的产出，应该是这个外壳自己的 modules 数组）")
	}
	served, err := servedMemberSet()
	if err != nil {
		return nil, err
	}

	var modules []shellConfigModule
	if err := json.Unmarshal([]byte(raw), &modules); err != nil {
		return nil, fmt.Errorf("解析 SHELL_CONFIG_JSON 失败: %w", err)
	}

	// SHELL_CONFIG_JSON 里的模块顺序已经是 be-ops 按依赖关系拓扑排序过的
	// 结果（tools/be-ops/internal/shellconfig 的既有职责），这里原样保留
	// 顺序传给 shell.Run——迁移与启动顺序由这个顺序决定，本文件不重新排序。
	matched := make(map[string]bool, len(served))
	specs := make([]shell.ModuleSpec, 0, len(modules))
	for _, m := range modules {
		name := versionedServiceName(m.ComponentID, m.Version)
		if !served[name] {
			// shell-config.json 列的是"这个外壳理论上有哪些成员"，不是
			// "这次都被收编了"——没在 BRICKKIT_SERVED_MEMBERS 里的成员
			// 这次没被平台收编（还没切成 servedBy，或者被临时摘掉），
			// 正常跳过，不是错误。
			continue
		}
		matched[name] = true
		ctor, ok := moduleRegistry[m.ComponentID]
		if !ok {
			return nil, fmt.Errorf("组件 %s 在 SHELL_CONFIG_JSON 里，但 moduleRegistry 没有登记它的真实 New 函数——是不是漏了给它加 import", m.ComponentID)
		}
		specs = append(specs, shell.ModuleSpec{
			ComponentID:      m.ComponentID,
			ComponentVersion: m.Version,
			Env:              m.Config,
			HTTPPort:         m.HTTPPort,
			ExtraPorts:       m.ExtraPorts,
			Schema:           m.Schema,
			New:              ctor,
		})
	}

	if len(matched) != len(served) {
		var missing []string
		for name := range served {
			if !matched[name] {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		return nil, fmt.Errorf("BRICKKIT_SERVED_MEMBERS 里有 SHELL_CONFIG_JSON 找不到的成员：%v（是不是 brickkit.yaml 改完之后忘了重新跑 be-ops shell-config --shell 把新的字符串贴回 config.shellConfigJson）", missing)
	}
	return specs, nil
}

// servedMemberSet 读 BRICKKIT_SERVED_MEMBERS（平台原生注入，servedBy
// 外壳"这次真的被收编、活着"的成员清单，逗号分隔的版本化服务名）。
//
// ⚠️ 必须用 os.LookupEnv 而不是 os.Getenv：这个变量"不存在"和"存在但是
// 空字符串"是两种不同的状态，语义完全不同——不存在意味着这个容器可能
// 根本不是被 servedBy 正常收编启动的（平台总会至少注入一个空字符串，
// 真的读不到通常说明是手动 docker run 忘了传，必须报错，不能悄悄退化成
// "全部实例化"这类旧行为，那会掩盖真实的配置错误）；空字符串是合法状态，
// 意味着这次没有任何成员被收编（都还没切换、或者都被临时摘掉了），
// 应该装出 0 个模块，而不是报错。
func servedMemberSet() (map[string]bool, error) {
	raw, ok := os.LookupEnv("BRICKKIT_SERVED_MEMBERS")
	if !ok {
		return nil, fmt.Errorf("BRICKKIT_SERVED_MEMBERS 未设置——这个容器看起来不是被 servedBy 正常收编启动的（平台总会至少注入一个空字符串），检查是不是手动 docker run 漏传了这个变量")
	}
	set := map[string]bool{}
	if raw == "" {
		return set, nil
	}
	for _, name := range strings.Split(raw, ",") {
		set[strings.TrimSpace(name)] = true
	}
	return set, nil
}

// versionedServiceName 与 brickKit 自己推导服务名的算法逐字对应
// （总纲 §2.1："/ → -、. → -、全部小写，再接精确版本号"）——
// "mdm/customer"@"1.0.7" → "mdm-customer-1-0-7"，跟
// BRICKKIT_SERVED_MEMBERS 里的写法逐字一致。
func versionedServiceName(id, version string) string {
	s := strings.NewReplacer("/", "-", ".", "-").Replace(id + "-" + version)
	return strings.ToLower(s)
}

