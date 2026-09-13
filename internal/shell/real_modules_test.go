package shell

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	besdk "github.com/brickKit/be-sdk-go"

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
	customerv1 "github.com/brickKit/mdm-customer/gen/mdm/customer/v1"
	mdmproduct "github.com/brickKit/mdm-product/backend/module"
	productv1 "github.com/brickKit/mdm-product/gen/mdm/product/v1"
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
	// 没有在启动阶段就崩掉）。routeChecks 的 value 支持可选的方法前缀
	// （如 "POST /api/iam/logout"），不带前缀默认 GET——infra-iam-casdoor
	// 除了 Public 路由全是 POST，没有一条 GET 的非 Public 路由可选。
	for id, path := range routeChecks {
		method, p := http.MethodGet, path
		if i := strings.IndexByte(path, ' '); i >= 0 {
			method, p = path[:i], path[i+1:]
		}
		assertStatusReal(t, ports[id], method, p, http.StatusForbidden)
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

// TestRun_go_infra外壳全部5个真实模块能一起启动 验证 go-infra 外壳
// （设计书 §13.2 外壳三）全部 5 个成员：infra-authz/infra-workflow/
// infra-notification（全部字段都有默认值）+ infra-iam-casdoor（真实连一个
// 本机跑着的 be-casdoor 基础资源自举，`APP_TOKEN_SIGNING_KEY_PEM` 用测试
// 现生成的真实 RSA 密钥）+ integration-im-dingtalk（真实钉钉凭据，见
// `.env` 的 `DINGTALK_APP_KEY`/`DINGTALK_APP_SECRET`/`DINGTALK_AGENT_ID`
// ——项目 Task 9 阶段三已经真机验证过这三项是真实可用的凭据，不是占位符）。
//
// ⚠️ 用真实凭据不等于会真的调一次钉钉/深度改 Casdoor 状态——已经确认过
// 两件事，这条测试才敢这么设计：
//  1. integration-im-dingtalk 的 New/Start 零网络调用，真正会打钉钉
//     gettoken 接口的 tokenmgr.EnsureValid/Refresh 只在真的消费一条
//     infra.notification.dispatch.im.v1 事件、或者管理员手动调
//     POST /admin/token/refresh 时才触发——这条测试两者都不做，全程不会
//     真的联系钉钉服务器。
//  2. infra-iam-casdoor 的 Start() 对 Casdoor 不可达/自举失败的每一步都是
//     记日志继续，不会把错误返回给 errgroup（不会拖垮整个外壳）；自举
//     用的组织/应用名沿用 component.yaml 默认值 brickkit/brickkit-app，
//     跟阶段三真机验证时创建的是同一个，本来就存在，自举只是确认存在
//     不会重复创建，不会弄脏 Casdoor 状态；`webhookCallbackUrl` 留空
//     跳过 webhook 自举这一步，不需要真实可达的回调地址。
//
// 需要真实运行的 be-casdoor（`make up` 默认起的基础资源之一）；用宿主机
// 直连地址 http://localhost:8000，不是 host.docker.internal（那是给
// brickkit 托管容器用的，这条测试是宿主机上的 go test 进程直连）。
func TestRun_go_infra外壳全部5个真实模块能一起启动(t *testing.T) {
	requireCasdoorReachable(t)
	signingKeyPEM := generateTestRSAPrivateKeyPEM(t)

	appKey := os.Getenv("DINGTALK_APP_KEY")
	appSecret := os.Getenv("DINGTALK_APP_SECRET")
	agentID := os.Getenv("DINGTALK_AGENT_ID")
	if appKey == "" || appSecret == "" || agentID == "" {
		t.Skip("未设置 DINGTALK_APP_KEY/DINGTALK_APP_SECRET/DINGTALK_AGENT_ID，跳过（见 .env）")
	}

	modules := []realModule{
		{id: "infra/authz", version: "1.0.5", schema: "infra_authz", new: infraauthz.New},
		{id: "infra/workflow", version: "1.0.3", schema: "infra_workflow", new: infraworkflow.New},
		{id: "infra/notification", version: "1.0.3", schema: "infra_notification", new: infranotification.New},
		{
			id: "infra/iam-casdoor", version: "1.0.7", schema: "infra_iam_casdoor",
			extra: map[string]string{
				"CASDOOR_BASE_URL":          "http://localhost:8000",
				"CASDOOR_ADMIN_USERNAME":    "admin",
				"CASDOOR_ADMIN_PASSWORD":    "123",
				"WEBHOOK_SHARED_SECRET":     "test-webhook-shared-secret-placeholder",
				"APP_TOKEN_SIGNING_KEY_PEM": signingKeyPEM,
			},
			new: infraiamcasdoor.New,
		},
		{
			id: "integration/im-dingtalk", version: "1.0.4", schema: "integration_im_dingtalk",
			extra: map[string]string{
				"DINGTALK_APP_KEY":    appKey,
				"DINGTALK_APP_SECRET": appSecret,
				"DINGTALK_AGENT_ID":   agentID,
			},
			new: integrationimdingtalk.New,
		},
	}
	routeChecks := map[string]string{
		"infra/authz":             "/api/admin/roles",
		"infra/workflow":          "/infra/workflow/tasks",
		"infra/notification":      "/infra/notification/notifications",
		"infra/iam-casdoor":       "POST /api/iam/logout",
		"integration/im-dingtalk": "/integration/im/admin/deliveries",
	}
	runRealShell(t, "be-shell-go-infra-test", modules, routeChecks)
}

// TestRun_跨外壳依赖真实网络可达 验证 go-backoffice 外壳（crm-opportunity）
// 对 go-core 外壳（mdm-customer/mdm-product）的两条跨外壳强依赖，在两个
// 外壳真的同时作为两个独立进程跑起来时，真实网络可达——不是"路由挂载
// 对了"这种间接证据，是真的对着另一个进程里活着的 gRPC 服务发一次调用、
// 拿到一个正确的响应。前三条测试从没有真的让两个外壳同时活着过（都是
// 先后顺序跑完一个再跑下一个），这条测试补的正是这一半。
//
// ⚠️ 没有通过 crm-opportunity 自己的 REST 层去驱动这次调用（比如真的走
// 一次 CreateOpportunity）——那需要一整套真实 iamJwksUrl/authzBundleUrl
// （infra-iam-casdoor 真实签发 + infra-authz 真实下发权限），复杂度跟
// 本条测试想验证的"跨外壳地址真实可达"不是同一件事，留给阶段四 Task 9
// 的合并态业务闭环真机验证。crm-opportunity 自己的 backend/internal/client
// 又是 Go 的 internal 包，本仓库物理上 import 不到（铁律六天然生效），
// 所以这里改用它内部调用的同一套真身 gRPC 客户端桩代码
// （mdm-customer/mdm-product 各自的 gen/.../v1 包，铁律六第二类白名单，
// 见设计书 §13.3）直接发起调用——验证的是"produce-7 会算出来的这个跨
// 外壳地址，在另一个真实运行的进程里确实有一个正常响应的 gRPC 服务"，
// 这正是"跨外壳依赖真实网络可达"字面要验证的事，不依赖 crm-opportunity
// 自己那一侧的鉴权状态（gRPC 侧本项目目前没有任何鉴权，鉴权只在 REST
// 层，见各组件 AGENTS.md 的既有说明——跟本条测试想验证的东西是两回事）。
func TestRun_跨外壳依赖真实网络可达(t *testing.T) {
	pgDSN := realGoDSN(t)
	natsURL := natsURLForTest()

	customerHTTPPort, customerGRPCPort := freePort(t), freePort(t)
	productHTTPPort, productGRPCPort := freePort(t), freePort(t)

	coreCtx, coreCancel := context.WithCancel(context.Background())
	coreErrCh := make(chan error, 1)
	go func() {
		coreErrCh <- Run(coreCtx, Config{
			ShellName: "be-shell-go-core-crosscheck",
			PGDSN:     pgDSN,
			NATSURL:   natsURL,
			Modules: []ModuleSpec{
				{
					ComponentID:      "mdm/customer",
					ComponentVersion: "1.0.7",
					Env:              map[string]string{"PG_SCHEMA": "mdm_customer"},
					HTTPPort:         customerHTTPPort,
					ExtraPorts:       map[string]int{"grpc": customerGRPCPort},
					Schema:           "mdm_customer",
					New:              mdmcustomer.New,
				},
				{
					ComponentID:      "mdm/product",
					ComponentVersion: "1.0.8",
					Env:              map[string]string{"PG_SCHEMA": "mdm_product"},
					HTTPPort:         productHTTPPort,
					ExtraPorts:       map[string]int{"grpc": productGRPCPort},
					Schema:           "mdm_product",
					New:              mdmproduct.New,
				},
			},
		}, besdk.NewLogger("be-shell-go-core-crosscheck"))
	}()
	t.Cleanup(func() {
		coreCancel()
		select {
		case <-coreErrCh:
		case <-time.After(5 * time.Second):
			t.Error("go-core 外壳在 ctx 取消后没有及时退出")
		}
	})
	waitHealthyOrFail(t, coreErrCh, customerHTTPPort, "/healthz")
	waitHealthyOrFail(t, coreErrCh, productHTTPPort, "/healthz")

	// 模拟 be-ops 产出 7 会给跨外壳依赖算出来的那种地址——真实 Docker 部署
	// 时这里会是 host.docker.internal:<对方发布的端口>，本地测试里两个
	// "外壳"是同一个测试进程里的两个 goroutine，用 127.0.0.1 才能真的连
	// 上；验证的是同一件事："这个地址在另一个真实运行的进程里有一个正常
	// 工作的 gRPC 服务"，跟具体是哪个主机名无关。besdk.Endpoint() 读的是
	// 真实进程环境变量（§2.1 的既有设计：依赖地址不走 rt.Config，走
	// os.Getenv——因为它的取值只取决于"被依赖组件是谁"，不取决于"哪个
	// 模块在问"，多模块共享一个进程环境不会有 item 16 那类冲突），所以
	// 这里用 t.Setenv 而不是 ModuleSpec.Env。
	t.Setenv("MDM_CUSTOMER_ENDPOINT", fmt.Sprintf("http://127.0.0.1:%d", customerHTTPPort))
	t.Setenv("MDM_CUSTOMER_GRPC_ENDPOINT", fmt.Sprintf("http://127.0.0.1:%d", customerGRPCPort))
	t.Setenv("MDM_PRODUCT_ENDPOINT", fmt.Sprintf("http://127.0.0.1:%d", productHTTPPort))
	t.Setenv("MDM_PRODUCT_GRPC_ENDPOINT", fmt.Sprintf("http://127.0.0.1:%d", productGRPCPort))

	backofficePort := freePort(t)
	boCtx, boCancel := context.WithCancel(context.Background())
	boErrCh := make(chan error, 1)
	go func() {
		boErrCh <- Run(boCtx, Config{
			ShellName: "be-shell-go-backoffice-crosscheck",
			PGDSN:     pgDSN,
			NATSURL:   natsURL,
			Modules: []ModuleSpec{
				{
					ComponentID:      "crm/opportunity",
					ComponentVersion: "1.0.10",
					Env:              map[string]string{"PG_SCHEMA": "crm_opportunity"},
					HTTPPort:         backofficePort,
					Schema:           "crm_opportunity",
					New:              crmopportunity.New,
				},
			},
		}, besdk.NewLogger("be-shell-go-backoffice-crosscheck"))
	}()
	t.Cleanup(func() {
		boCancel()
		select {
		case <-boErrCh:
		case <-time.After(5 * time.Second):
			t.Error("go-backoffice 外壳在 ctx 取消后没有及时退出")
		}
	})
	waitHealthyOrFail(t, boErrCh, backofficePort, "/healthz")

	// 到这里，两个外壳是真的同时活着的两个独立进程（这条测试里是两个
	// 独立的 shell.Run 调用），互不干扰——这是前三条测试从未验证过的
	// 状态。真正验证"跨外壳依赖真实网络可达"：用真身 gRPC 客户端桩代码
	// 直接拨号 go-core 刚刚绑定的真实端口，发起一次真实调用。
	assertBatchGetReachable(t, "mdm/customer", customerGRPCPort, func(ctx context.Context, cc *grpc.ClientConn) error {
		_, err := customerv1.NewCustomerServiceClient(cc).BatchGet(ctx, &customerv1.BatchGetRequest{})
		return err
	})
	assertBatchGetReachable(t, "mdm/product", productGRPCPort, func(ctx context.Context, cc *grpc.ClientConn) error {
		_, err := productv1.NewProductServiceClient(cc).BatchGet(ctx, &productv1.BatchGetRequest{})
		return err
	})
}

// assertBatchGetReachable 拨号真实端口发起一次真实 BatchGet 调用——ids
// 传空列表（besdk.BatchGetRouted 对空列表直接短路返回，不碰真实数据，
// 见 be-sdk-go archive.go），验证的不是业务数据对不对（那是各组件自己
// L2/L3 的范围），是"这个地址背后真的有一个正常工作的 gRPC 服务在响应"
// 这件事本身：连接失败/超时/gRPC transport 错误说明网络不可达，正常
// 返回（哪怕是空列表对应的空结果）说明可达，且请求真的走完了服务端的
// gRPC handler → service 层 → repo 层 → 真实共享连接池 WithTx 这一整条
// 路径。
func assertBatchGetReachable(t *testing.T, dep string, port int, call func(context.Context, *grpc.ClientConn) error) {
	t.Helper()
	target := fmt.Sprintf("127.0.0.1:%d", port)
	cc, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("拨号 %s（%s）失败: %v", dep, target, err)
	}
	defer cc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := call(ctx, cc); err != nil {
		t.Fatalf("跨外壳调用 %s（%s）的 BatchGet 失败，网络不可达或服务未正常响应: %v", dep, target, err)
	}
}

// requireCasdoorReachable 确认本机 be-casdoor（`make up` 默认起的基础
// 资源）真的在跑——不可达就跳过，不是伪造一个"假装可达"的判断，同
// realGoDSN/natsURLForTest 判断真实依赖是否就绪的既有方式一致。
func requireCasdoorReachable(t *testing.T) {
	t.Helper()
	resp, err := http.Get("http://localhost:8000/api/health")
	if err != nil {
		t.Skipf("本机 be-casdoor 不可达（先 make up）：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("本机 be-casdoor 健康检查非 200（状态码 %d），先确认它已经完全起来", resp.StatusCode)
	}
}

// generateTestRSAPrivateKeyPEM 现生成一把真实的 RSA 私钥，PKCS1 PEM 编码
// ——infra-iam-casdoor 的 keys.ParsePrivateKeyPEM 先试 PKCS1 再退化试
// PKCS8，两种都支持，这里选最简单的那种。这把钥匙只在这条测试的生命周期
// 内存在，不落盘、不复用，纯粹是为了让 appTokenSigningKeyPem 这个必填项
// 拿到一把语法/语义都合法的真实密钥，不是伪造一个看起来像密钥的字符串
// 去"骗过" MustString。
func generateTestRSAPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成测试用 RSA 密钥失败: %v", err)
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	return string(pem.EncodeToMemory(block))
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

func assertStatusReal(t *testing.T, port int, method, path string, want int) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("构造请求 %s %s 失败：%v", method, url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求 %s %s 失败：%v", method, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("请求 %s %s 期望状态码 %d，实际 %d", method, url, want, resp.StatusCode)
	}
}
