# be-shell-go

Go 外壳的启动器：把 N 个组件模块（各自的 `.../backend/module".New`）装进同一个进程，
共享一个数据库连接池 + 一个 NATS 连接 + 一套 OTel/权限判定，逐个 `Listen` 各自的
HTTP/gRPC 端口。设计动机、七条铁律、代价见父仓库《BrickEnterprise 设计书.md》
第 13 章；本仓库只负责"怎么实现"，不重复"为什么这样做"。

**它不是 brickKit 组件**——不进 `brickkit.yaml`，不受签名覆盖，`brickKit` 平台对它
"不挡路也不帮忙"（`组件合并部署.md` 的原话）。

## 现状（阶段四 Task 2，骨架）

- `internal/shell`：全部装配逻辑（`Run`），复用 `be-sdk-go` 已验证过的
  `NewShellRuntime`/`InitShellAuthz`/`ServeHTTP`/`ServeExtraPort`，不重新实现。
- `cmd/shell`：进程入口，目前 `Modules` 是空的——真实的 11 个 Go 组件要等
  `be-ops` 产出 4/7（合并清单 + 每外壳环境变量表，阶段四 Task 4）落地、且各组件
  仓库被正式 `go get` 进 `go.mod` 之后（Task 5/6）才会填。
- 已用假模块验证过骨架本身：多模块共享 DB/NATS 但各自独立字段、`ctx` 取消后
  优雅关闭且 `Stop` 都被调用、外壳自己的 `HealthPort` 独立于任何模块响应、
  单模块 `Start()` 里的 panic 被 `recover` 住不会让整个进程崩溃（细节见
  `internal/shell/shell_test.go`，这是写这份骨架过程中才发现的一处真实缺口，
  不是从设计书推演出来的——`RunStandalone` 的等价调用不需要这层防护，单模块
  进程整个崩溃退出正是正确行为；11 个模块共享一个进程后，同一个疏漏会把另外
  10 个一起带走）。

## 待办（阶段四后续任务）

- Task 4：`be-ops` 产出 4（合并清单：哪些组件进哪个外壳、端口分配、迁移顺序）、
  产出 7（每外壳环境变量表）、产出 8（`shell-compose.yml` 的 `depends_on` +
  健康检查，消费本仓库的 `HealthPort`）。
- Task 5/6：把 11 个真实 Go 组件（`mdm-customer`/`mdm-product`/`erp-inventory`/
  `erp-finance`/`erp-sales`/`infra-authz`/`infra-iam-casdoor`/`infra-workflow`/
  `infra-notification`/`integration-im-dingtalk`/`crm-opportunity`）正式
  `go get` 进 `go.mod`，`cmd/shell/main.go` 按拓扑序静态 `import` 它们的
  `backend/module` 包，填进 `Config.Modules`。
- Task 9：合并态业务闭环真机验证（附录 E 全链路）。
- Task 10：§13.7 拆回门禁 + 铁律六 import 扫描扩展到本仓库（本仓库自身语言层面
  不可能违反铁律六——它只能 import 各组件的公开 `backend/module` 包，Go 的
  `internal/` 可见性规则物理上不允许它碰到任何组件的内部实现；真正的风险点是
  组件之间互相 `import`，那条边完全不会出现在本仓库自己的依赖图里，根
  `make gates` 的扫描器才是这条铁律的守卫）。

完整任务清单见 `docs/plans/04-阶段四-做外壳验拆回.md`；本地开发时可以自己建一份
`go.work`（`use ( . ../../tools/be-sdk-go )`）联调 `be-sdk-go` 未发布的改动，
不要提交它——`Dockerfile` 的最终构建只依赖 `go.mod`/`go.sum` 锁定的已发布版本。
