package shell

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/nats-io/nats.go"
)

// syncLogBuf 是一个并发安全、且能在写入内容命中某个标记时对外发出信号的
// 日志缓冲区——recover 之后的那条日志写入发生在 Run 内部的另一个
// goroutine，跟测试的主 goroutine 之间如果只靠 sleep 去猜时序，-race 会
// 如实报出这是一次真正的数据竞争（写 buffer 的 goroutine 和读
// buffer.String() 的主 goroutine 之间没有任何 happens-before 关系）。
// close(logged) 建立的才是真正的同步点，不是"多等一会儿大概率来得及"。
// ⚠️ 不能在"第一次 Write"就 close(logged)——InitShellAuthz 在 panic 发生
// 之前就已经往同一个 logger 写过日志，第一次 Write 根本不是我们要等的
// 那一条，得按内容匹配。
type syncLogBuf struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	once    sync.Once
	marker  string
	matched chan struct{}
}

func newSyncLogBuf(marker string) *syncLogBuf {
	return &syncLogBuf{marker: marker, matched: make(chan struct{})}
}

func (s *syncLogBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	n, err := s.buf.Write(p)
	s.mu.Unlock()
	if strings.Contains(string(p), s.marker) {
		s.once.Do(func() { close(s.matched) })
	}
	return n, err
}

func (s *syncLogBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

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

// TestRun_单模块Start里panic不崩溃整个进程只隔离在这一个模块自己身上
// 是"空转验证"的第二条，也是写这份骨架过程中真正发现的一处缺口
// （RunStandalone 的等价调用没有这层 recover，单模块场景下不需要；
// 外壳把 N 个模块塞进一个进程后，同一个未捕获 panic 会把其余模块一起
// 带崩，Go 的 panic 会直接终止整个进程，不会被 errgroup 或其他 goroutine
// 拦住）。
//
// ⚠️ 阶段四 Task 11 把这条测试的判据往前推了一步：这条测试早先的版本
// 只验证了"Run 不会被一次 panic 崩掉整个测试进程，干净返回一个包含
// componentID 的错误"——这句话本身没错，但从没验证过"返回一个错误"这个
// 结果是不是我们真正想要的行为。用真实模块混跑（见
// real_modules_test.go 的 TestRun_一个模块panic不影响其它真实模块继续服务）
// 才发现：Run 把这个 error 原样返回给 errgroup 会取消 gctx，把外壳里
// 其它所有健康模块一起拖下线——这与"单个模块 panic 不拖垮外壳其余模块"
// 直接矛盾（shell.go 对应位置的注释记录了完整的修复过程）。修复后 Run
// 不再把 panic 转成的错误返回给 errgroup，只记日志；本测试相应地把判据
// 从"断言 Run 返回错误"改成"断言 panic 发生之后这个模块自己的 HTTP 还在
// 正常响应、Run 一直阻塞到 ctx 被取消才干净返回 nil"，用一个日志缓冲区
// 确认 componentID 确实被记下来了，替代原来靠错误信息字符串做的同一件事。
func TestRun_单模块Start里panic不崩溃整个进程只隔离在这一个模块自己身上(t *testing.T) {
	port := freePort(t)
	panicked := make(chan struct{})
	m := &fakeModule{
		startFn: func(ctx context.Context) error {
			close(panicked)
			panic("模拟模块自己代码里的一个真实 bug")
		},
	}

	// recover 之后的日志写入发生在 Run 内部另一个 goroutine 里，跟本测试
	// 的主 goroutine 没有天然的先后关系——单靠 close(panicked) + sleep
	// 只是"大概率来得及"，-race 会如实报出这是一次真正的数据竞争。
	// syncLogBuf 用一次 Write 触发 close(logged) 建立真正的 happens-before
	// 边，而不是靠时间凑巧。
	logBuf := newSyncLogBuf("recovered")
	logger := slog.New(slog.NewTextHandler(logBuf, nil))

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Config{
			ShellName: "be-shell-go-test",
			PGDSN:     "postgres://user:pass@localhost:1/doesnotmatter?sslmode=disable",
			NATSURL:   natsURLForTest(),
			Modules: []ModuleSpec{
				{ComponentID: "fake/panics", Schema: "fake_panics", HTTPPort: port, New: m.new},
			},
		}, logger)
	}()

	waitHealthy(t, port)

	select {
	case <-panicked:
	case <-time.After(2 * time.Second):
		t.Fatal("模块的 Start 在 2 秒内没有触发预期的 panic")
	}

	select {
	case <-logBuf.matched:
	case <-time.After(2 * time.Second):
		t.Fatal("panic 发生后 2 秒内没有等到 recover 那条日志落地")
	}

	// 日志已经确认落地，此时再确认这个模块自己的 HTTP 服务没有被这次
	// panic 带下线——隔离生效的直接证据。
	waitHealthy(t, port)

	if !strings.Contains(logBuf.String(), "fake/panics") {
		t.Fatalf("期望日志里记录出问题的 componentID，实际日志：%s", logBuf.String())
	}

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

// ⚠️ 原来这里有 TestExportDependencyEndpoints_真机复现/
// TestExportDependencyEndpoints_同一个外壳内不一致时报错 两条用例，测的
// 是阶段四 Task 9 真机复现的一个真实 bug：besdk.Endpoint() 读
// os.LookupEnv 不是 rt.Config，ModuleSpec.Env 必须额外导出到进程环境
// 才能被 SystemClient/UserClient 看到。servedBy 落地后（阶段四附加
// Task 0.2）这一步整个不需要了——brickKit 自己在生成阶段就把
// *_ENDPOINT 类变量直接合并进外壳容器自己的 os.Environ()，本包一启动
// 就已经看得见，exportDependencyEndpoints 函数与这两条测试一并删除。
// 完整历史（真实 bug 是怎么被真机复现出来的）留在 README.md，不随代码
// 一起消失。

// TestEnvWithProcessFallback_specific优先于进程环境 是阶段四附加
// Task 0.4 真机复现出的秘钥类 configSchema 项（appTokenSigningKeyPem
// 等）的回归测试：这类值已经被 be-ops 的 MergeConfig 整条排除出
// SHELL_CONFIG_JSON，必须靠外壳自己进程环境兜底才能到达需要它的模块。
func TestEnvWithProcessFallback_specific优先于进程环境(t *testing.T) {
	t.Setenv("FAKE_SHELL_LEVEL_SECRET", "来自外壳自己进程环境的值")
	t.Setenv("PG_SCHEMA", "不该被用到——specific 里有同名 key")

	got := envWithProcessFallback(map[string]string{
		"PG_SCHEMA": "mdm_customer",
	})

	if got["FAKE_SHELL_LEVEL_SECRET"] != "来自外壳自己进程环境的值" {
		t.Fatalf("specific 里没有的 key 应该从外壳自己的进程环境兜底拿到，实际 %+v", got)
	}
	if got["PG_SCHEMA"] != "mdm_customer" {
		t.Fatalf("specific 里已经有的 key 不该被进程环境覆盖，实际 %+v", got)
	}
}
