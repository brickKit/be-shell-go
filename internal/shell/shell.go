// Package shell 是 be-shell-go 的全部装配逻辑——RunStandalone
// （be-sdk-go/standalone.go）的"1 模块"形状推广到"N 模块"，字段构造与
// Listen 方式直接复用 be-sdk-go 已经验证过的
// NewShellRuntime/InitShellAuthz/ServeHTTP/ServeExtraPort，不重新实现
// 一遍（阶段三踩坑记录 A4g 的教训：一条只有测试/只有小范围调用方走过的
// 构造路径，跟生产真正走的那条路径不是同一条，就是真实 bug 藏身的地方；
// 外壳如果自己重写这段逻辑，等于在 11 倍的爆炸半径上重演同一类风险）。
//
// 迁移执行用 golang-migrate 的 iofs 源驱动，直接吃每个模块
// *besdk.Module 已经嵌好的 Migrations fs.FS——全拆态迁移容器
// （各组件自己的 backend/cmd/migrate）读的是磁盘上的 migrations/ 目录，
// 合并态这里读的是同一份 .sql 文件编译进二进制的另一条路径，两条路径
// 共用同一份 .sql 文件，不会出现"哪份是权威"的疑问（同 mdm-customer
// migrations 包自己的注释）。
package shell

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib" // 注册 "pgx" 驱动，同 be-sdk-go §12.4：不用 lib/pq
	"github.com/nats-io/nats.go"
	"golang.org/x/sync/errgroup"
)

// identRe 同 be-sdk-go 的 tx.go：role/schema 要拼进 DSN 字符串，不接受
// 占位符，白名单校验防注入——这两个值来自 be-ops 产出 4，不是用户输入，
// 但仍然值得同一条防线（防的是"生成器自己写错"，不是外部攻击）。
var identRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ModuleSpec 描述外壳要装的一个模块。`be-ops` 产出 4/7（合并清单 + 每
// 外壳环境变量表）落地前，Task 2 先用一个普通 Go 结构体表达这个形状，
// 供 Task 4 的生成器与 Task 5/6 的真实迁移共同收敛；`New` 字段必须是
// Go 源码里的静态 import（`.../backend/module".New`），不能从数据文件
// 动态加载——combined 清单只负责 ComponentID/Env/端口/schema 这些数据，
// 具体要跑哪些模块是 cmd/shell/main.go 自己的 Go 代码决定的。
type ModuleSpec struct {
	ComponentID      string
	ComponentVersion string
	Env              map[string]string
	HTTPPort         int
	ExtraPorts       map[string]int
	// Schema 是这个模块的 PG schema 名（registry/schemas.tsv 的值，例如
	// "mdm_customer"）——用来给迁移拼 search_path/x-migrations-table，
	// 也是未来 WithTx 调用时模块自己代码里已经硬编码的那个值（这里不
	// 重复模块内部逻辑，只是迁移阶段需要单独知道一次）。
	Schema string
	New    func(context.Context, *besdk.Runtime) (*besdk.Module, error)
}

// Config 是外壳进程级的全部输入。
type Config struct {
	// ShellName 是 Bootstrap 的 serviceName（例如 "be-shell-go"）——外壳
	// 自己的进程身份，不是任何一个模块的 componentID。
	ShellName   string
	OTelBaseURL string
	// PGDSN 是外壳唯一共享登录角色的连接串（设计书 §13.3："brickkit.yaml
	// 里只有 5 个外壳登录角色"），不含 search_path/x-migrations-table——
	// runMigrations 会按模块各自追加。运行时查询走 besdk.WithTx 的
	// SET LOCAL ROLE 切身份，不需要这两个参数出现在共享池的 DSN 里。
	PGDSN   string
	NATSURL string
	// IamJwksURL/AuthzBundleURL 本项目当前全部模块共用同一份（见
	// docs/design/_调研记录/04-阶段四.md），外壳只需要配一次。
	IamJwksURL     string
	AuthzBundleURL string
	// Modules 的顺序即迁移与启动顺序——调用方（cmd/shell/main.go）负责
	// 传入拓扑序，Run 本身不做依赖排序（同设计书 §13.8.3："外壳内部的
	// 模块顺序由你的框架负责"）。
	Modules []ModuleSpec
	// HealthPort 是外壳自己（不是任何一个模块）对外暴露的健康检查端口。
	// 设计书 §13.6："健康检查……一个容器只有一个 probe"——但外壳里的
	// 11 个模块各有各的端口和各自的 /healthz，谁的健康检查代表整个
	// 容器，平台不知道也不会替这个新问题给出答案（这正是"你要接手的
	// 六件事"之一）。这里的判据同每个模块自己的 /healthz 一致：只答
	// "外壳进程本身活着"，不查任何模块、不查任何依赖——0 表示不开
	// （目前 cmd/shell/main.go 骨架阶段还没有真实 shell-compose 健康
	// 检查配置去消费它，Task 4/8 会把这个端口接进 be-ops 产出的
	// shell-compose.yml）。
	HealthPort int
}

type builtModule struct {
	spec ModuleSpec
	rt   *besdk.Runtime
	mod  *besdk.Module
}

// Run 装配整个外壳：Bootstrap 一次 → 开一个共享 DB/NATS → 权限判定装配
// 一次 → 逐个模块调 New 拿 *besdk.Module → 按 cfg.Modules 的顺序逐个跑
// 迁移 → 用 errgroup 一起 Listen → 任一失败或 ctx 取消，全部优雅退出。
//
// 阻塞到 ctx 被取消或任一模块失败为止，返回值是 errgroup.Wait() 的结果
// （nil 代表正常收到取消信号退出，不是"什么都没做"）。
func Run(ctx context.Context, cfg Config, logger *slog.Logger) error {
	shutdownOTel, err := besdk.Bootstrap(ctx, cfg.ShellName, cfg.OTelBaseURL)
	if err != nil {
		return fmt.Errorf("Bootstrap: %w", err)
	}
	defer func() { _ = shutdownOTel(context.Background()) }()

	db, err := sql.Open("pgx", cfg.PGDSN)
	if err != nil {
		return fmt.Errorf("打开外壳共享连接池失败: %w", err)
	}
	defer func() { _ = db.Close() }()

	nc, err := nats.Connect(cfg.NATSURL)
	if err != nil {
		return fmt.Errorf("连接 NATS 失败: %w", err)
	}
	defer nc.Close()

	besdk.InitShellAuthz(ctx, besdk.NewConfig(map[string]string{
		"IAM_JWKS_URL":     cfg.IamJwksURL,
		"AUTHZ_BUNDLE_URL": cfg.AuthzBundleURL,
	}), logger)

	built := make([]builtModule, 0, len(cfg.Modules))
	for _, m := range cfg.Modules {
		rt := besdk.NewShellRuntime(besdk.ShellModuleConfig{
			ComponentID:      m.ComponentID,
			ComponentVersion: m.ComponentVersion,
			Env:              m.Env,
			HTTPPort:         m.HTTPPort,
			ExtraPorts:       m.ExtraPorts,
		}, db, nc)
		mod, err := m.New(ctx, rt)
		if err != nil {
			return fmt.Errorf("模块 %s 初始化失败: %w", m.ComponentID, err)
		}
		built = append(built, builtModule{spec: m, rt: rt, mod: mod})
	}

	// 铁律五：合并后平台不再管迁移，外壳自己按 cfg.Modules 给定的顺序
	// 跑，失败即中止启动（不是跳过这一个模块继续往下）。
	for _, b := range built {
		if b.mod.Migrations == nil {
			continue
		}
		if err := runMigrations(cfg.PGDSN, b.spec.Schema, b.mod.Migrations); err != nil {
			return fmt.Errorf("模块 %s 迁移失败: %w", b.spec.ComponentID, err)
		}
	}

	g, gctx := errgroup.WithContext(ctx)
	if cfg.HealthPort != 0 {
		g.Go(func() error {
			err := besdk.ServeHTTP(gctx, cfg.HealthPort, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			if err != nil {
				logger.Error("外壳自己的健康检查端口退出", "error", err)
			}
			return err
		})
	}
	for _, b := range built {
		b := b
		g.Go(func() error {
			err := besdk.ServeHTTP(gctx, b.spec.HTTPPort, b.mod.HTTPHandler)
			if err != nil {
				logger.Error("模块 HTTP 服务退出", "module_component_id", b.spec.ComponentID, "error", err)
			}
			return err
		})
		for name, port := range b.spec.ExtraPorts {
			name, port := name, port
			g.Go(func() error {
				err := besdk.ServeExtraPort(gctx, name, port, b.mod.RegisterGRPC, b.rt.Logger)
				if err != nil {
					logger.Error("模块额外端口服务退出", "module_component_id", b.spec.ComponentID, "port_name", name, "error", err)
				}
				return err
			})
		}
		if b.mod.Start != nil {
			g.Go(func() (err error) {
				// ⚠️ 阶段四 Task 11 真机复现过的一处真实缺口：这里如果把
				// recover 到的 panic 转成的 error 原样 return 给 errgroup，
				// errgroup.WithContext 的既有语义会立刻取消 gctx——而 gctx
				// 正是本函数里全部模块的 HTTP/额外端口/Start 共用的同一个
				// ctx，取消它等于把其余 10 个健康模块的服务也一起带下线。
				// RunStandalone 的等价调用（standalone.go）不需要考虑这个：
				// 单模块进程里，一次 panic 让整个进程退出就是正确行为
				// （Docker 重启策略负责，爆炸半径=1 个组件）；外壳把 N 个
				// 模块塞进同一个进程后，"1 个模块的后台循环 panic 了"不该
				// 等价于"这 N 个模块一起坏了"——那样合并部署就白白放弃了
				// 独立部署本来就有的故障隔离，merged 之后反而比 unmerged
				// 更脆弱。所以 panic 这条分支 recover 之后只记日志、不
				// 把 error 流回 g（同 HTTP 侧
				// recoveryAndErrorMappingMiddleware、gRPC 侧
				// grpcRecoveryInterceptor 已经在各自路径上守住的"一个
				// 请求 panic 不该拖累其它请求"同一类判据，只是这里的隔离
				// 单位是"一个模块"而不是"一次请求"）。代价：这个模块自己的
				// Start 循环从此不会再被重启，直到整个外壳下一次重启——
				// 这是有意接受的降级，不是被忽略的错误，日志会带上
				// component_id 清楚指出是谁。
				//
				// ⚠️ 这条隔离只针对 panic——Start 自己正常 return 的非
				// panic error 不受影响，仍然按原样流回 g、取消 gctx、
				// 拖垮整个外壳。两者是不同性质的失败：panic 是这一个
				// 模块自己代码里的 bug（同一个进程的另外 N-1 个模块的
				// 代码没有任何理由被牵连）；Start 主动返回错误通常意味着
				// 它依赖的外部资源坏了（比如共享的 DB/NATS 连接整个不可用
				// ——这种情况下其它模块大概率也在用同一份资源，让外壳整体
				// 退出交给 Docker 重启，仍然是目前认为更安全的默认值，
				// Task 11 没有要求改变这一半）。
				defer func() {
					if r := recover(); r != nil {
						logger.Error("模块后台循环 panic（已隔离，不影响外壳内其余模块）", "module_component_id", b.spec.ComponentID, "recovered", r)
						err = nil
					}
				}()
				err = b.mod.Start(gctx)
				if err != nil {
					logger.Error("模块后台循环退出", "module_component_id", b.spec.ComponentID, "error", err)
				}
				return err
			})
		}
	}

	runErr := g.Wait()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, b := range built {
		if b.mod.Stop != nil {
			_ = b.mod.Stop(shutdownCtx)
		}
	}

	return runErr
}

// runMigrations 对一个模块的迁移 FS 跑一次 Up——DSN 沿用外壳共享的
// 连接串，只按这个模块的 schema 追加 search_path/x-migrations-table
// （同各组件自己 backend/cmd/migrate/main.go 的既有约定：迁移状态表
// 必须落在各自的 schema 里，主键含组件标识，§11.2.3）。ErrNoChange
// 不是错误——迁移必须能连跑两次都成功，合并态由外壳跑、全拆态由平台跑，
// 两条路径都要能重跑（§13.3 铁律五）。
//
// 用 golang-migrate 的 iofs 源驱动直接吃 *besdk.Module.Migrations 这个
// fs.FS——不是 file:// 驱动（那是全拆态迁移容器读磁盘目录用的），两条
// 路径共用同一份内嵌 .sql 文件，不会出现"哪份是权威"的疑问。
func runMigrations(sharedDSN, schema string, fsys fs.FS) error {
	if !identRe.MatchString(schema) {
		return fmt.Errorf("非法 schema 名 %q（不是 be-ops 产出 4 生成的合法值）", schema)
	}
	src, err := iofs.New(fsys, ".")
	if err != nil {
		return fmt.Errorf("读迁移文件失败: %w", err)
	}
	dsn := appendMigrationParams(sharedDSN, schema)
	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		return fmt.Errorf("迁移初始化失败: %w", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("迁移执行失败: %w", err)
	}
	return nil
}

// appendMigrationParams 把 search_path/x-migrations-table 追加到共享
// DSN 后面——shared DSN 本身已经带了至少一个 query 参数
// （BuildPGDSN 固定拼 ?sslmode=disable），但这里仍然按"有没有问号"
// 显式判断，不假设调用方一定用 BuildPGDSN 拼出来的那份。
func appendMigrationParams(dsn, schema string) string {
	sep := "&"
	if !strings.Contains(dsn, "?") {
		sep = "?"
	}
	return fmt.Sprintf("%s%ssearch_path=%s&x-migrations-table=schema_migrations_%s", dsn, sep, schema, schema)
}
