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
	mdmcustomer "github.com/brickKit/mdm-customer/backend/module"
	mdmproduct "github.com/brickKit/mdm-product/backend/module"
)

// TestRun_两个真实组件模块合并进一个外壳进程 是阶段四计划 Task 5 的
// 核心验证：不再用假模块，而是把 mdm-customer/mdm-product 两个真实
// 组件仓库的 module.New 静态 import 进来，跑在同一个 internal/shell.Run
// 进程里，验证"外壳骨架能不能扛住真实组件代码"——这两个组件互相之间
// 零依赖（都是只读枢纽），选它们做第一批验证对象正是阶段四计划自己的
// 判断（"先迁两个零依赖组件验证机制本身"）。
//
// 需要真实可达的 TEST_PG_DSN/TEST_NATS_URL（同 be-sdk-go/be-sdk-python
// 既有判据，未设置就跳过）——这条测试会真的对 TEST_PG_DSN 指向的库跑
// 一次 mdm-customer/mdm-product 的迁移，两个组件的 schema
// （mdm_customer/mdm_product）必须已经在这个库里存在对应的 ROLE
// （`make test-db-init` 已经建好，同这两个组件自己 make test 依赖的
// 环境）。
func TestRun_两个真实组件模块合并进一个外壳进程(t *testing.T) {
	pgDSN := os.Getenv("TEST_PG_DSN")
	if pgDSN == "" {
		t.Skip("未设置 TEST_PG_DSN，跳过（CI 里必须设）")
	}
	if !strings.Contains(pgDSN, "sslmode=") {
		// besdk.BuildPGDSN 生成的真实共享连接串总是带 sslmode=disable
		// （本地/内网部署的默认值）；TEST_PG_DSN 是测试环境自己给的，
		// 不一定带这个参数——golang-migrate 的 postgres 驱动默认要求
		// SSL，没有这个参数会在迁移这一步真实报错（真机测试时踩到的，
		// 不是凭空加的防御代码）。
		sep := "&"
		if !strings.Contains(pgDSN, "?") {
			sep = "?"
		}
		pgDSN += sep + "sslmode=disable"
	}
	natsURL := natsURLForTest()

	portCustomer, portProduct := freePort(t), freePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Config{
			ShellName: "be-shell-go-real-modules-test",
			PGDSN:     pgDSN,
			NATSURL:   natsURL,
			Modules: []ModuleSpec{
				{
					ComponentID:      "mdm/customer",
					ComponentVersion: "1.0.5",
					Env:              map[string]string{"PG_SCHEMA": "mdm_customer"},
					HTTPPort:         portCustomer,
					Schema:           "mdm_customer",
					New:              mdmcustomer.New,
				},
				{
					ComponentID:      "mdm/product",
					ComponentVersion: "1.0.6",
					Env:              map[string]string{"PG_SCHEMA": "mdm_product"},
					HTTPPort:         portProduct,
					Schema:           "mdm_product",
					New:              mdmproduct.New,
				},
			},
		}, besdk.NewLogger("be-shell-go-real-modules-test"))
	}()

	// /healthz 是 besdk.NewGinEngine 统一挂的，零依赖恒 200——两个真实
	// 模块都要能在合并态下正常响应，这是"外壳骨架扛得住真实组件"的
	// 第一道断言。同时监听 errCh：Run 如果在健康检查通过之前就已经
	// 失败退出（迁移失败、module.New 报错……），直接把真实错误打出来，
	// 不要傻等超时才报一个"没响应"、看不出真正原因。
	waitHealthyOrFail(t, errCh, portCustomer, "/healthz")
	waitHealthyOrFail(t, errCh, portProduct, "/healthz")

	// 业务路由要真的挂载、真的经过权限中间件——没配 iamJwksUrl 时
	// RequirePermission 是 fail-closed stub，非 Public 路由一律 403，
	// 不是 404（说明路由确实注册了）、也不是 500/连接失败（说明真实
	// service/repo 层的构造、真实共享连接池的 SET LOCAL ROLE 切换都
	// 没有在启动阶段就崩掉）。
	assertStatusReal(t, portCustomer, "/mdm/customer/customers", http.StatusForbidden)
	assertStatusReal(t, portProduct, "/mdm/product/products", http.StatusForbidden)

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
