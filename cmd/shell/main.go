// be-shell-go 的进程入口——阶段四 Task 6：11 个真实组件的合并进程集成
// 测试已经全部通过（internal/shell/real_modules_test.go），这里接的是
// 真机部署这一步：同一份镜像按 SHELL_NAME 环境变量的值，装配出
// go-core/go-backoffice/go-infra 三个外壳实例中的一个（阶段四调研记录
// 04 §12 的既定结论："部署几份实例纯粹是运行时配置问题"，internal/shell.Run
// 本来就接受任意 Modules 子集，不需要为每个外壳单独建一份镜像/仓库）。
//
// ⚠️ 阶段四附加 Task 0.6（brickKit v0.4.2）：哪些模块属于这个外壳、
// 端口/config 值，现在全部来自平台原生注入的 BRICKKIT_SERVED_MEMBERS_CONFIG
// ——一个 JSON 数组，每个元素是当前这次部署里真的被这个外壳收编的一个
// 成员（componentId/version/httpPort/extraPorts）。这条数据 brickKit 自己
// 在算 BRICKKIT_SERVED_MEMBERS 的同一处代码里就已经算好，直接原生注入
// 外壳容器——取代了此前 be-ops shell-config 命令手工生成、手工贴进
// brickkit.yaml 该外壳组件 config.shellConfigJson 字符串配置项那一整套
// 机制（那套机制的已知缺陷：brickkit.yaml 一改版本号/config/servedBy
// 归属就会过期，平台不报错，只在外壳真机启动时才炸——这正是反馈给
// brickKit、促成这次原生支持的真实动机，见装配仓库
// docs/dev/架构复盘-servedBy落地后的自有改进空间.md）。
//
// ⚠️ 阶段四附加 Task 0.6 二次修补（brickKit v0.4.3）：`config` 字段改名
// `configEnvVars`，语义从"key → 值"变成"key → 环境变量名"——每个成员
// 自己的每个 config 值，现在各自是外壳进程环境里一条独立的、带组件 ID
// 前缀命名的标量变量（`{EnvPrefix(componentId)}_{EnvVarName(key)}`，跟
// `*_ENDPOINT` 走同一套前缀算法），不再嵌在 JSON 字符串内部——这是
// brickKit 从根上修复了我们真机复现、反馈给他们的那个"密钥类 config 值
// 会被 docker compose 自己的全文本 `${VAR}` 替换撑坏 JSON"的 bug，见
// docs/plans/04b-验证记录.md Task 0.6。本文件因此不再需要
// `sanitizeServedMembersConfig` 这层下游兜底——JSON 里永远不会再出现
// 任何可能是 `${VAR}` 的用户可控文本。
//
// 这个文件现在只负责三件事：①对每个成员的每个 config key，读
// `configEnvVars[key]` 拿到 brickKit 算好的变量名，再 `os.Getenv` 去
// 外壳自己的进程环境读真正的值；②把 key 从原始驼峰形式转成 besdk.Config
// 内部查找用的 SCREAMING_SNAKE_CASE（转换算法与 brickKit 自己的
// internal/inject.EnvVarName 逐字对应，仅供本文件自己内部组装
// ModuleSpec.Env 用，不用于重新计算 brickKit 已经算好的那条变量名）；
// ③把数据里的 componentId 字符串映到真实的 Go 源码 import（这一步不能
// 数据驱动，ModuleSpec.New 必须是静态 import，见 internal/shell.ModuleSpec
// 的字段注释）。除此之外不做任何装配决策——哪个组件该不该合并、合并成
// 几个外壳，那是 assembly.yaml/registry/schemas.tsv 的既定数据，不是这个
// 文件该决定的。
//
// ⚠️ 阶段四附加 Task 0.2/0.3：依赖地址（*_ENDPOINT）由 brickKit 自己在
// 生成阶段直接合并进外壳容器自己的 os.Environ()，这个进程一启动就已经
// 看得见，不需要本文件/internal/shell 再做任何搬运。完整调研过程见
// 装配仓库 docs/plans/04b-验证记录.md Task 0.2。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"unicode"

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
// 的静态映射——BRICKKIT_SERVED_MEMBERS_CONFIG 只携带 componentId 这一个
// 字符串，具体 New 是哪个包的哪个函数，只能在源码里写死（同
// internal/shell.ModuleSpec 的既有注释）。全部 11 个真实组件的 New 都
// 注册在这里，不分外壳——三个外壳实例共用同一份镜像/同一个二进制，实际
// 装哪几个由平台注入的数据在运行时决定，源码这一层不需要（也不应该）
// 知道"这次是哪个外壳"。
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

// servedMemberConfig 是 BRICKKIT_SERVED_MEMBERS_CONFIG 数组里一条的
// 形状——字段名与 brickKit 源码 internal/shell（`shell-implementers-guide`
// 文档里公开承诺的契约）逐一对应。零个成员时这个变量的值是 `[]`，不是
// 变量缺失（brickKit 自己的既定行为，本文件不需要对"变量不存在"这种
// 情况做特殊处理——见下面 buildModules 里唯一的错误分支）。
type servedMemberConfig struct {
	ComponentID string `json:"componentId"`
	Version     string `json:"version"`
	HTTPPort    int    `json:"httpPort"`
	ExtraPorts  []struct {
		Name string `json:"name"`
		Port int    `json:"port"`
	} `json:"extraPorts"`
	// ConfigEnvVars 把这个模块自己 configSchema 的每个 key（原始驼峰
	// 形式）映射到外壳进程环境里那条真正携带值的独立变量名——不是值
	// 本身（brickKit v0.4.3 起的既定形状，见 shell-implementers-guide
	// 原文："读 configEnvVars[key] 拿到变量名，再去自己的环境里读那条
	// 变量拿到真正的值"）。
	ConfigEnvVars map[string]string `json:"configEnvVars"`
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
	// 组件）——外壳自己的 component.yaml 声明这个端口，值写在 brickkit.yaml
	// 该外壳组件的 config 块里，这里只负责读、不负责决定这个端口该是多少。
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

// buildModules 解析 BRICKKIT_SERVED_MEMBERS_CONFIG（平台原生注入，这次
// 部署里真的被这个外壳收编的成员列表，含每个成员完整的装配数据），拼出
// 真实的 []shell.ModuleSpec。
func buildModules() ([]shell.ModuleSpec, error) {
	raw, ok := os.LookupEnv("BRICKKIT_SERVED_MEMBERS_CONFIG")
	if !ok {
		return nil, fmt.Errorf("BRICKKIT_SERVED_MEMBERS_CONFIG 未设置——这个容器看起来不是被 servedBy 正常收编启动的（平台总会至少注入一个 [] 空数组），检查是不是手动 docker run 漏传了这个变量")
	}

	var members []servedMemberConfig
	if err := json.Unmarshal([]byte(raw), &members); err != nil {
		return nil, fmt.Errorf("解析 BRICKKIT_SERVED_MEMBERS_CONFIG 失败: %w", err)
	}

	// ⚠️ 数组顺序不是 brickkit.yaml 里声明 servedBy 的顺序，也不是任何
	// 拓扑序——brickKit 自己按 componentId 字典序排过一遍
	// （internal/shell.Group.ServedMembersConfig 的 sort.Slice，真机
	// `docker exec` 核对过：go-core 的声明顺序是 mdm/customer→mdm/
	// product→erp/inventory→erp/finance→erp/sales，生成的数组顺序是
	// erp/finance→erp/inventory→erp/sales→mdm/customer→mdm/product）。
	// 这里原样保留传给 shell.Run，不重新排序——迁移按这个字典序顺序跑，
	// 依赖 shell.Run 靠 SET LOCAL search_path 天然的 schema 隔离，不靠
	// 迁移顺序本身（设计书禁止跨 schema 外键，见 §11.2.3），字典序对
	// 正确性无影响，只是"哪个模块的迁移先跑"这件事不受我们控制，见
	// 装配仓库 docs/plans/04b-部署矩阵验证.md Task 5 的真机确认。
	specs := make([]shell.ModuleSpec, 0, len(members))
	for _, m := range members {
		ctor, ok := moduleRegistry[m.ComponentID]
		if !ok {
			return nil, fmt.Errorf("组件 %s 在 BRICKKIT_SERVED_MEMBERS_CONFIG 里，但 moduleRegistry 没有登记它的真实 New 函数——是不是漏了给它加 import", m.ComponentID)
		}

		// pgSchema 要在转换成 SCREAMING_SNAKE_CASE 之前，按原始驼峰 key
		// 取——它是每个组件都有的既定配置项（registry/schemas.tsv 的值），
		// 不是可选字段。ConfigEnvVars 给的是变量名，真正的值要再
		// os.Getenv 一次。
		schemaVarName, ok := m.ConfigEnvVars["pgSchema"]
		schema := os.Getenv(schemaVarName)
		if !ok || schema == "" {
			return nil, fmt.Errorf("组件 %s 的 config 里没有 pgSchema——BRICKKIT_SERVED_MEMBERS_CONFIG 的数据看起来不完整", m.ComponentID)
		}

		env := make(map[string]string, len(m.ConfigEnvVars))
		for key, varName := range m.ConfigEnvVars {
			env[configEnvVarName(key)] = os.Getenv(varName)
		}

		extraPorts := make(map[string]int, len(m.ExtraPorts))
		for _, p := range m.ExtraPorts {
			extraPorts[p.Name] = p.Port
		}

		specs = append(specs, shell.ModuleSpec{
			ComponentID:      m.ComponentID,
			ComponentVersion: m.Version,
			Env:              env,
			HTTPPort:         m.HTTPPort,
			ExtraPorts:       extraPorts,
			Schema:           schema,
			New:              ctor,
		})
	}
	return specs, nil
}

// configEnvVarName 把一个原始 configSchema 驼峰 key 转成
// SCREAMING_SNAKE_CASE——跟 besdk.Config 内部查找配置项时用的转换规则
// （camelCase → 大写下划线）逐字对应，也是 brickKit 自己
// internal/inject.EnvVarName 的算法，这里原样复刻。⚠️ 用途仅限于本文件
// 组装 ModuleSpec.Env 的 key（besdk.Config 按这个短名查表）——不用于
// 重新计算 BRICKKIT_SERVED_MEMBERS_CONFIG 里 configEnvVars 已经给出的
// 那条带组件 ID 前缀的变量名（那条名字直接从 JSON 读，brickKit v0.4.3
// 起已经算好，本文件只管拿它去 os.Getenv，不用自己再拼一遍）。
func configEnvVarName(key string) string {
	var b strings.Builder
	runes := []rune(key)
	for i, r := range runes {
		switch {
		case r == '-' || r == '.' || r == ' ':
			b.WriteRune('_')
		case unicode.IsUpper(r):
			if i > 0 && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1])) {
				b.WriteRune('_')
			}
			b.WriteRune(r)
		default:
			b.WriteRune(unicode.ToUpper(r))
		}
	}
	return b.String()
}
