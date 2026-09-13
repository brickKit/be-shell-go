# be-shell-go

Go 外壳的启动器：把 N 个组件模块（各自的 `.../backend/module".New`）装进同一个进程，
共享一个数据库连接池 + 一个 NATS 连接 + 一套 OTel/权限判定，逐个 `Listen` 各自的
HTTP/gRPC 端口。设计动机、七条铁律、代价见父仓库《BrickEnterprise 设计书.md》
第 13 章；本仓库只负责"怎么实现"，不重复"为什么这样做"。

**它不是 brickKit 组件**——不进 `brickkit.yaml`，不受签名覆盖，`brickKit` 平台对它
"不挡路也不帮忙"（`组件合并部署.md` 的原话）。

## 现状（阶段四 Task 5/6，9/11 个真实模块已验证）

- `internal/shell`：全部装配逻辑（`Run`），复用 `be-sdk-go` 已验证过的
  `NewShellRuntime`/`InitShellAuthz`/`ServeHTTP`/`ServeExtraPort`，不重新实现。
- `internal/shell/real_modules_test.go`：go-core（`mdm-customer`/`mdm-product`/
  `erp-inventory`/`erp-finance`/`erp-sales`）、go-backoffice（`crm-opportunity`）、
  go-infra（`infra-authz`/`infra-workflow`/`infra-notification`）三组外壳、
  共 9 个真实、未经任何修改的组件模块，真的合并进同一个 `internal/shell.Run`
  进程跑通（真实迁移、真实健康检查、真实权限中间件 403）。**故意不含**
  `infra-iam-casdoor`/`integration-im-dingtalk`——两者都需要伪造密钥或真实
  外部服务（Casdoor/钉钉），复杂度与本轮"外壳骨架扛不扛得住真实组件"不是
  同一件事，留给单独一轮任务处理。
- `cmd/shell`：进程入口，`Modules` 仍是空的——真机 `brickkit.yaml` 原子切换
  是 Task 6 剩余部分，等 `be-ops` 产出 4/7（合并清单 + 每外壳环境变量表）
  接进 `main.go` 才做，见待办。
- ⚠️ **踩到的真实坑（阶段四调研记录 04 §13）**：erp-sales/crm-opportunity 原本
  vendor 了一份被依赖组件的契约生成代码（vendored-contract 模式）——一旦
  调用方和被调方的真实模块被编译进同一个 Go 二进制（哪怕只是这份测试文件
  把三个外壳的模块放进同一个测试包），两份 import path 不同、内容相同的
  生成代码会在 protobuf **进程级全局**注册表里重复注册同一个文件/类型全名，
  直接 panic。**先试过"嵌套 go module + 外壳 replace 去重"，真机验证证明
  技术上不可行**——Go 的 module system 无法把两个不同 import path 的包
  合并成一份编译实例，即使 replace 也不行。真正的解法是把"生成物契约包"
  本身提升为跟 `be-sdk-*` 同类的白名单：erp-finance/erp-inventory/
  infra-workflow/mdm-customer/mdm-product 各自把 `gen/<domain>/<name>`
  发布成独立嵌套 go module，erp-sales/crm-opportunity 改成直接 import 真身，
  不再各自 vendor 一份镜像（设计书 §13.3 铁律六新增说明）。
- 已用假模块验证过骨架本身：多模块共享 DB/NATS 但各自独立字段、`ctx` 取消后
  优雅关闭且 `Stop` 都被调用、外壳自己的 `HealthPort` 独立于任何模块响应、
  单模块 `Start()` 里的 panic 被 `recover` 住不会让整个进程崩溃（细节见
  `internal/shell/shell_test.go`，这是写这份骨架过程中才发现的一处真实缺口，
  不是从设计书推演出来的——`RunStandalone` 的等价调用不需要这层防护，单模块
  进程整个崩溃退出正是正确行为；11 个模块共享一个进程后，同一个疏漏会把另外
  10 个一起带走）。

## 待办（阶段四后续任务）

- Task 6 剩余部分：`brickkit.yaml` 原子式真实切换——先 `brickkit down` 停掉
  全部组装态容器（设计书 §13.9），`be-ops` 产出 4/7 接进 `cmd/shell/main.go`，
  按拓扑序静态 `import` 11 个真实组件的 `backend/module` 包填进
  `Config.Modules`，真机 `brickkit up` 验证。
- `infra-iam-casdoor`/`integration-im-dingtalk` 两个真实模块的合并验证——
  单独一轮任务，需要真实 Casdoor/钉钉沙盒或谨慎构造的伪造密钥。
- 待写：`TestRun_跨外壳依赖真实网络可达`——验证 crm-opportunity（go-backoffice）
  真的能通过 `host.docker.internal`（产出 7）跨外壳调到 mdm-customer/
  mdm-product（go-core）。
- Task 9：合并态业务闭环真机验证（附录 E 全链路）。
- Task 10：§13.7 拆回门禁 + 铁律六 import 扫描扩展到本仓库（本仓库自身语言层面
  不可能违反铁律六——它只能 import 各组件的公开 `backend/module` 包，Go 的
  `internal/` 可见性规则物理上不允许它碰到任何组件的内部实现；真正的风险点是
  组件之间互相 `import`，那条边完全不会出现在本仓库自己的依赖图里，根
  `make gates` 的扫描器才是这条铁律的守卫）。

完整任务清单见 `docs/plans/04-阶段四-做外壳验拆回.md`；本地开发时可以自己建一份
`go.work`（`use ( . ../../tools/be-sdk-go )`）联调 `be-sdk-go` 未发布的改动，
不要提交它——`Dockerfile` 的最终构建只依赖 `go.mod`/`go.sum` 锁定的已发布版本。
