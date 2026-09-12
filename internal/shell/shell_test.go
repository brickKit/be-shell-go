package shell

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/nats-io/nats.go"
)

// natsURLForTest 同 be-sdk-go 的既有约定（outbox_test.go）：优先读
// TEST_NATS_URL，本地开发环境没设就退化到 nats.DefaultURL——本项目的
// 惯例是"真实基础设施优先"，不是 mock，这条测试因此需要一个真的在跑的
// NATS（本机 make up 起的 be-nats 或任何可达的 NATS 都行）。
func natsURLForTest() string {
	if u := os.Getenv("TEST_NATS_URL"); u != "" {
		return u
	}
	return nats.DefaultURL
}

// fakeModule 是本包全部测试共用的最小假模块——阶段四计划 Task 2 明确要求
// "空转验证：先不装任何真实模块"，这里用一个什么都不做的 module.New
// 实现验证外壳骨架本身（能起、能优雅关闭、单模块 panic 不崩溃整个进程），
// 不依赖任何真实组件仓库、真实 Postgres/NATS。
type fakeModule struct {
	startFn func(context.Context) error
	stopped chan struct{}
}

func (f *fakeModule) new(_ context.Context, rt *besdk.Runtime) (*besdk.Module, error) {
	return &besdk.Module{
		HTTPHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
		Start: f.startFn,
		Stop: func(context.Context) error {
			if f.stopped != nil {
				close(f.stopped)
			}
			return nil
		},
	}, nil
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func waitHealthy(t *testing.T, port int) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("端口 %d 在 2 秒内没有开始正常响应", port)
}

// TestRun_两个假模块共享一个进程且都能真的响应HTTP 是"空转验证"的第一条：
// NATS 接真实的（本机跑的 be-nats 或 TEST_NATS_URL 指向的任意可达
// 实例——sql.Open 是懒的，Postgres 不需要真的能连上，但 nats.Connect
// 会真的拨号，两个假模块都没有 Migrations、也不发布任何事件，不需要
// 真实的 schema/subject），只验证外壳骨架本身能把两个模块的 HTTP
// handler 都真的 Listen 起来、ctx 取消后都优雅退出、Stop 都被调用到。
func TestRun_两个假模块共享一个进程且都能真的响应HTTP(t *testing.T) {
	port1, port2 := freePort(t), freePort(t)
	stopped1, stopped2 := make(chan struct{}), make(chan struct{})
	m1 := &fakeModule{stopped: stopped1}
	m2 := &fakeModule{stopped: stopped2}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Config{
			ShellName: "be-shell-go-test",
			PGDSN:     "postgres://user:pass@localhost:1/doesnotmatter?sslmode=disable",
			NATSURL:   natsURLForTest(),
			Modules: []ModuleSpec{
				{ComponentID: "fake/one", Schema: "fake_one", HTTPPort: port1, New: m1.new},
				{ComponentID: "fake/two", Schema: "fake_two", HTTPPort: port2, New: m2.new},
			},
		}, besdk.NewLogger("be-shell-go-test"))
	}()

	waitHealthy(t, port1)
	waitHealthy(t, port2)

	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("期望优雅关闭无错误，实际 %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run 在 ctx 取消后没有及时退出")
	}

	select {
	case <-stopped1:
	default:
		t.Error("期望模块一的 Stop 被调用")
	}
	select {
	case <-stopped2:
	default:
		t.Error("期望模块二的 Stop 被调用")
	}
}

// TestRun_HealthPort独立于任何模块自己响应 验证外壳自己的健康检查端口
// （§13.6"一个容器只有一个 probe"，但这个 probe 不属于任何一个模块）
// 即使配了 0 个真实模块也能正常响应——shell-compose.yml 的健康检查
// 探的是这个端口，不是任意挑一个模块的端口。
func TestRun_HealthPort独立于任何模块自己响应(t *testing.T) {
	healthPort := freePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Config{
			ShellName:  "be-shell-go-test",
			PGDSN:      "postgres://user:pass@localhost:1/doesnotmatter?sslmode=disable",
			NATSURL:    natsURLForTest(),
			HealthPort: healthPort,
		}, besdk.NewLogger("be-shell-go-test"))
	}()

	waitHealthy(t, healthPort)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("期望优雅关闭无错误，实际 %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run 在 ctx 取消后没有及时退出")
	}
}

// TestRun_单模块Start里panic不崩溃整个进程只是优雅退出并清楚指出是哪个模块
// 是"空转验证"的第二条，也是写这份骨架过程中真正发现的一处缺口
// （RunStandalone 的等价调用没有这层 recover，单模块场景下不需要；
// 外壳把 N 个模块塞进一个进程后，同一个未捕获 panic 会把其余模块一起
// 带崩，Go 的 panic 会直接终止整个进程，不会被 errgroup 或其他 goroutine
// 拦住）——这里验证 Run 加的 recover 确实生效：Run 干净返回一个包含
// componentID 的错误，测试进程本身没有崩溃（如果没有 recover，这条测试
// 会直接让整个 go test 进程崩溃退出，而不是"测试失败"）。
func TestRun_单模块Start里panic不崩溃整个进程只是优雅退出并清楚指出是哪个模块(t *testing.T) {
	port := freePort(t)
	m := &fakeModule{
		startFn: func(ctx context.Context) error {
			panic("模拟模块自己代码里的一个真实 bug")
		},
	}

	err := Run(context.Background(), Config{
		ShellName: "be-shell-go-test",
		PGDSN:     "postgres://user:pass@localhost:1/doesnotmatter?sslmode=disable",
		NATSURL:   natsURLForTest(),
		Modules: []ModuleSpec{
			{ComponentID: "fake/panics", Schema: "fake_panics", HTTPPort: port, New: m.new},
		},
	}, besdk.NewLogger("be-shell-go-test"))

	if err == nil {
		t.Fatal("期望 Run 返回错误（panic 被 recover 转成了 error），实际 nil")
	}
	if got := err.Error(); !strings.Contains(got, "fake/panics") {
		t.Fatalf("期望错误信息里包含出问题的 componentID，实际 %q", got)
	}
}
