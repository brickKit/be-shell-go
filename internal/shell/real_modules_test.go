package shell

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	besdk "github.com/brickKit/be-sdk-go"

	crmopportunity "github.com/brickKit/crm-opportunity/backend/module"
	erpfinance "github.com/brickKit/erp-finance/backend/module"
	erpinventory "github.com/brickKit/erp-inventory/backend/module"
	erpsales "github.com/brickKit/erp-sales/backend/module"
	infraauthz "github.com/brickKit/infra-authz/backend/module"
	infranotification "github.com/brickKit/infra-notification/backend/module"
	infraworkflow "github.com/brickKit/infra-workflow/backend/module"
	mdmcustomer "github.com/brickKit/mdm-customer/backend/module"
	mdmproduct "github.com/brickKit/mdm-product/backend/module"
)

// realModule 是一次真机验证要装的一个真实组件——把 Task 5 两个模块时
// 手写的字面量收敛成一个小结构体，供 Task 6 扩到 11 个模块时复用，不
// 是从头重写一遍装配逻辑。
type realModule struct {
	id      string
	version string
	schema  string
	extra   map[string]string // 该模块除 PG_SCHEMA 之外还需要的 env（比如 erp-sales 的 defaultWarehouseId）
	new     func(context.Context, *besdk.Runtime) (*besdk.Module, error)
}

func realGoDSN(t *testing.T) string {
	t.Helper()
	pgDSN := os.Getenv("TEST_PG_DSN")
	if pgDSN == "" {
		t.Skip("未设置 TEST_PG_DSN，跳过（CI 里必须设）")
	}
	if !strings.Contains(pgDSN, "sslmode=") {
		// besdk.BuildPGDSN 生成的真实共享连接串总是带 sslmode=disable
		// （本地/内网部署的默认值）；TEST_PG_DSN 是测试环境自己给的，
		// 不一定带这个参数——golang-migrate 的 postgres 驱动默认要求
		// SSL，没有这个参数会在迁移这一步真实报错（Task 5 真机测试时
		// 踩到的，不是凭空加的防御代码）。
		sep := "&"
		if !strings.Contains(pgDSN, "?") {
			sep = "?"
		}
		pgDSN += sep + "sslmode=disable"
	}
	return pgDSN
}

// runRealShell 把一组真实模块装进一次 shell.Run，等全部健康检查通过后
// 逐个断言业务路由 fail-closed 403，最后优雅关闭。是 Task 5/6 反复用到
// 的同一套判据的通用版本，不按外壳分组各写一遍。
func runRealShell(t *testing.T, shellName string, modules []realModule, routeChecks map[string]string) {
	t.Helper()
	pgDSN := realGoDSN(t)
	natsURL := natsURLForTest()

	ports := make(map[string]int, len(modules))
	specs := make([]ModuleSpec, 0, len(modules))
	for _, m := range modules {
		port := freePort(t)
		ports[m.id] = port
		env := map[string]string{"PG_SCHEMA": m.schema}
		for k, v := range m.extra {
			env[k] = v
		}
		specs = append(specs, ModuleSpec{
			ComponentID:      m.id,
			ComponentVersion: m.version,
			Env:              env,
			HTTPPort:         port,
			Schema:           m.schema,
			New:              m.new,
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Config{
			ShellName: shellName,
			PGDSN:     pgDSN,
			NATSURL:   natsURL,
			Modules:   specs,
		}, besdk.NewLogger(shellName))
	}()

	// /healthz 是 besdk.NewGinEngine 统一挂的，零依赖恒 200——每个真实
	// 模块都要能在合并态下正常响应，这是"外壳骨架扛得住真实组件"的
	// 第一道断言。同时监听 errCh：Run 如果在健康检查通过之前就已经
	// 失败退出（迁移失败、module.New 报错……），直接把真实错误打出来，
	// 不要傻等超时才报一个"没响应"、看不出真正原因。
	for _, m := range modules {
		waitHealthyOrFail(t, errCh, ports[m.id], "/healthz")
	}

	// 业务路由要真的挂载、真的经过权限中间件——没配 iamJwksUrl 时
	// RequirePermission 是 fail-closed stub，非 Public 路由一律 403，
	// 不是 404（说明路由确实注册了）、也不是 500/连接失败（说明真实
	// service/repo 层的构造、真实共享连接池的 SET LOCAL ROLE 切换都
	// 没有在启动阶段就崩掉）。
	for id, path := range routeChecks {
		assertStatusReal(t, ports[id], path, http.StatusForbidden)
	}

	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("期望优雅关闭无错误，实际 %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run 在 ctx 取消后没有及时退出")
	}
}

// TestRun_go_core外壳全部5个真实模块合并进一个进程 是阶段四计划
// Task 5+6 的核心验证：go-core 外壳（设计书 §13.2 外壳一）真实的 5 个
// 组件——mdm-customer/mdm-product 零依赖，erp-inventory/erp-finance
// 也零依赖，erp-sales 强依赖前四个——全部装进同一个 internal/shell.Run
// 进程，用真实、未经任何修改的组件代码验证"外壳骨架扛得住真实组件，
// 且同外壳内部的依赖关系（erp-sales 要等前四个都构造完）不会导致启动
// 顺序错乱"。
//
// 需要真实可达的 TEST_PG_DSN/TEST_NATS_URL（同 be-sdk-go/be-sdk-python
// 既有判据，未设置就跳过）——这条测试会真的对 TEST_PG_DSN 指向的库跑
// 一次这 5 个组件的迁移，5 个 schema 必须已经在这个库里存在对应的
// ROLE（`make test-db-init` 已经建好）。
func TestRun_go_core外壳全部5个真实模块合并进一个进程(t *testing.T) {
	modules := []realModule{
		{id: "mdm/customer", version: "1.0.6", schema: "mdm_customer", new: mdmcustomer.New},
		{id: "mdm/product", version: "1.0.7", schema: "mdm_product", new: mdmproduct.New},
		{id: "erp/inventory", version: "1.0.14", schema: "erp_inventory", new: erpinventory.New},
		{id: "erp/finance", version: "1.0.10", schema: "erp_finance", new: erpfinance.New},
		{
			id: "erp/sales", version: "1.0.22", schema: "erp_sales",
			// defaultWarehouseId 是 erp-sales 唯一"required 且无 default"
			// 的配置项（component.yaml 自己的 required 段）——不供这个值
			// module.New 会直接构造失败，这不是本测试要验的东西，随便给
			// 一个语法合法的占位值即可（ConfirmOrder 真的用到它是业务
			// 逻辑测试的范围，本测试只验"能不能启动、健康检查/权限中间件
			// 是否正常"）。
			extra: map[string]string{"DEFAULT_WAREHOUSE_ID": "wh-test-placeholder"},
			new:   erpsales.New,
		},
	}
	routeChecks := map[string]string{
		"mdm/customer":  "/mdm/customer/customers",
		"mdm/product":   "/mdm/product/products",
		"erp/inventory": "/erp/inventory/balances",
		"erp/finance":   "/erp/finance/entries",
		"erp/sales":     "/erp/sales/orders",
	}
	runRealShell(t, "be-shell-go-core-test", modules, routeChecks)
}

// TestRun_go_backoffice外壳的真实模块能启动 验证 crm-opportunity（设计书
// §13.2 外壳二，本阶段唯一成员）能在合并态下正常启动——它对
// mdm-customer/mdm-product 是**跨外壳**强依赖（go-core 里的两个零依赖
// 组件），这条测试只验证它自己这一侧"能不能启动、健康检查/权限中间件
// 是否正常"，不需要真的把 go-core 也跑起来去满足这条依赖——跨外壳
// 依赖真正被调用只发生在请求处理阶段（比如 CreateOpportunity 真的去
// gRPC 查一次客户是否存在），/healthz 与"未鉴权 403"这两条断言都不会
// 触发这条调用链，先验证这一半（同 mdm-customer/mdm-product 各自独立
// 启动一样，不依赖对方）。跨外壳依赖的真实网络连通性验证见
// TestRun_跨外壳依赖真实网络可达。
func TestRun_go_backoffice外壳的真实模块能启动(t *testing.T) {
	modules := []realModule{
		{id: "crm/opportunity", version: "1.0.9", schema: "crm_opportunity", new: crmopportunity.New},
	}
	routeChecks := map[string]string{"crm/opportunity": "/crm/opportunity/opportunities"}
	runRealShell(t, "be-shell-go-backoffice-test", modules, routeChecks)
}

// TestRun_go_infra外壳的三个真实模块能一起启动 验证 go-infra 外壳
// （设计书 §13.2 外壳三）里配置最简单的三个成员——infra-authz/
// infra-workflow/infra-notification（全部字段都有默认值，不需要任何
// 额外 env）。**故意不含 infra-iam-casdoor/integration-im-dingtalk**：
// 前者的 configSchema 有六七个"必填无默认值"的敏感配置项
// （casdoorAdminPassword/webhookSharedSecret/appTokenSigningKeyPem…）
// 且 Start() 会真的去调一个真实 Casdoor 实例的管理 API 做自举；后者
// 需要真实钉钉凭据——这两个要么需要伪造一整套看起来合法的密钥/PEM
// （伪造出来的值本身没有验证意义，只是让 MustString 不 panic），要么
// 需要真的连一个可用的 Casdoor/钉钉沙盒，复杂度与本条测试想验证的
// "外壳骨架能不能扛住真实组件"不是同一件事，留给单独一轮任务处理，
// 不在阶段四这一遍里勉强凑数。
func TestRun_go_infra外壳的三个真实模块能一起启动(t *testing.T) {
	modules := []realModule{
		{id: "infra/authz", version: "1.0.4", schema: "infra_authz", new: infraauthz.New},
		{id: "infra/workflow", version: "1.0.3", schema: "infra_workflow", new: infraworkflow.New},
		{id: "infra/notification", version: "1.0.3", schema: "infra_notification", new: infranotification.New},
	}
	routeChecks := map[string]string{
		"infra/authz":        "/api/admin/roles",
		"infra/workflow":     "/infra/workflow/tasks",
		"infra/notification": "/infra/notification/notifications",
	}
	runRealShell(t, "be-shell-go-infra-test", modules, routeChecks)
}

func waitHealthyOrFail(t *testing.T, errCh <-chan error, port int, path string) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-errCh:
			t.Fatalf("Run 在 %s 变健康之前就退出了：%v", url, err)
		default:
		}
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("端口 %d 的 %s 在 5 秒内没有开始正常响应", port, path)
}

func assertStatusReal(t *testing.T, port int, path string, want int) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("请求 %s 失败：%v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("请求 %s 期望状态码 %d，实际 %d", url, want, resp.StatusCode)
	}
}
