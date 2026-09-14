# be-shell-go

Go 外壳的启动器：把 N 个组件模块（各自的 `.../backend/module".New`）装进同一个进程，
共享一个数据库连接池 + 一个 NATS 连接 + 一套 OTel/权限判定，逐个 `Listen` 各自的
HTTP/gRPC 端口。设计动机、七条铁律、代价见父仓库《BrickEnterprise 设计书.md》
第 13 章；本仓库只负责"怎么实现"，不重复"为什么这样做"。

**它不是 brickKit 组件**——不进 `brickkit.yaml`，不受签名覆盖，`brickKit` 平台对它
"不挡路也不帮忙"（`组件合并部署.md` 的原话）。

## 现状（阶段四 Task 5/6 全部完成，`brickkit.yaml` 已真机原子式切换）

- `internal/shell`：全部装配逻辑（`Run`），复用 `be-sdk-go` 已验证过的
  `NewShellRuntime`/`InitShellAuthz`/`ServeHTTP`/`ServeExtraPort`，不重新实现。
- `internal/shell/real_modules_test.go`：go-core（`mdm-customer`/`mdm-product`/
  `erp-inventory`/`erp-finance`/`erp-sales`）、go-backoffice（`crm-opportunity`）、
  go-infra（`infra-authz`/`infra-workflow`/`infra-notification`/
  `infra-iam-casdoor`/`integration-im-dingtalk`）三组外壳、**全部 11 个真实、
  未经任何修改的组件模块**，真的合并进同一个 `internal/shell.Run` 进程跑通
  （真实迁移、真实健康检查、真实权限中间件 403）。`infra-iam-casdoor` 真的连
  本机 `be-casdoor`（`make up` 默认起的基础资源）自举，`APP_TOKEN_SIGNING_KEY_PEM`
  用测试现生成的真实 RSA 密钥；`integration-im-dingtalk` 用 `.env` 里真实的
  钉钉凭据（New/Start 全程零网络调用，不会真的联系钉钉服务器，见测试文件顶部
  注释）。
- `cmd/shell`：进程入口，已经接上 `be-ops` 产出 4（`shell-config.json`）——
  同一份镜像按 `SHELL_NAME` 环境变量的值（`go-core`/`go-backoffice`/
  `go-infra`）从这份数据文件里挑出自己要装的外壳，再按平台原生注入的
  `BRICKKIT_SERVED_MEMBERS` 筛出这次真的被 `servedBy` 收编、活着的成员
  （阶段四附加 Task 0.2/0.3 起——原来还需要单独一份 `shell-env.json`
  才能拿到每个模块自己的 `Env`，已并入 `shell-config.json` 的 `Config`
  字段，见下方"现状补充"）；`moduleRegistry` 是本仓库唯一"componentId
  字符串 → 真实 Go 源码 import"的静态映射（数据驱动配置，代码驱动装配，
  `ModuleSpec.New` 不能从数据文件动态加载，见 `internal/shell.ModuleSpec`
  的既有注释）。`cmd/shell/main_test.go` 用手写夹具覆盖了"按外壳挑模块"/
  "外壳不存在时报错"/"按 `BRICKKIT_SERVED_MEMBERS` 筛成员的四种状态
  （未设置报错/空字符串零模块/部分列出只装那几个/列出了 shell-config
  里找不到的成员报错）"/"11 个真实组件都在 moduleRegistry 里"五类断言，
  不需要真实基础设施。
- **真机部署验证（阶段四 Task 6 最后一步，2026-09-13 完成）**：`docker build`
  出的镜像，用同一份镜像分别起了 `shell-go-core`/`shell-go-backoffice`/
  `shell-go-infra` 三个真实容器，`brickkit.yaml` 也真的原子式切换成
  `local: true`——`brickkit up` 之后 `docker ps` 显示外壳态最终成品：
  5 个基础资源 + 3 个外壳容器（合起来跑着全部 11 个真实模块）+
  `infra-print`/`infra-bff-mobile`/`frontend-standard` 3 个仍独立的容器，
  不再是 11 个独立的 Go 组件容器。真实验证过的内容：11 个模块的迁移全部
  在各自真实 schema 里跑通；`infra-iam-casdoor` 真的连本机 `be-casdoor`
  自举、真的收到 Casdoor 回调的 webhook（200）；`go-core`/`go-backoffice`
  两个外壳跨容器请求 `go-infra` 暴露的 `authzBundleUrl`/`iamJwksUrl`
  成功（同外壳/跨外壳两种地址改写规则都真机验证过）；3 个外壳容器合计
  内存占用约 29MiB（`docker stats`），远低于 11 个独立容器的量级。
  ⚠️ **过程中真的踩到两个新坑**（都已在 `be-ops` 修复，非本仓库范围但
  记在这里方便理解为什么真机验证分了好几轮）：① `be-ops shell-env` 原来
  要求全部 4 个外壳同时就绪才产出，与"Task 6 先切 3 个 Go 外壳、Task 7
  才轮到 py-render"的分批节奏冲突，改成外壳级别静默跳过（`be-ops@v0.1.7`）；
  ② brickKit 生成 `local-debug.*.env` 时，`appTokenSigningKeyPem` 这类
  多行 PEM 值原样把换行符写进文件、不加引号也不转义，`be-ops` 原来的单行
  dotenv 解析器会把 PEM 续行悄悄丢弃，且第一版修复又被"Base64 结尾的 `=`
  补齐符"这个真实存在的边界情况击穿（真机拿到的 RSA 私钥被截断到倒数
  第二行）——最终判据改成"`=` 前面那段像不像合法的 dotenv key"，不是
  "这行有没有 `=`"（`be-ops@v0.1.7` 同一个 commit）。
- ⚠️ **踩到的真实坑，复发了两次（阶段四调研记录 04 §13）**：erp-sales/
  crm-opportunity/infra-iam-casdoor 原本各自 vendor 了一份被依赖组件的契约
  生成代码（vendored-contract 模式）——一旦调用方和被调方的真实模块被编译
  进同一个 Go 二进制（哪怕只是这份测试文件把多个外壳的模块放进同一个测试
  包），两份 import path 不同、内容相同的生成代码会在 protobuf **进程级
  全局**注册表里重复注册同一个文件/类型全名，直接 panic。**先试过"嵌套
  go module + 外壳 replace 去重"，真机验证证明技术上不可行**——Go 的
  module system 无法把两个不同 import path 的包合并成一份编译实例，即使
  replace 也不行。真正的解法是把"生成物契约包"本身提升为跟 `be-sdk-*`
  同类的白名单：erp-finance/erp-inventory/infra-authz/infra-workflow/
  mdm-customer/mdm-product 各自把 `gen/<domain>/<name>` 发布成独立嵌套 go
  module，erp-sales/crm-opportunity/infra-iam-casdoor 改成直接 import 真身，
  不再各自 vendor 一份镜像（设计书 §13.3 铁律六新增说明）。第二次复发
  （infra-authz/infra-iam-casdoor 这一对）是把 go-infra 五个模块凑齐时
  才发现的，此前只覆盖了 go-core 外壳内部的边。
- 已用假模块验证过骨架本身：多模块共享 DB/NATS 但各自独立字段、`ctx` 取消后
  优雅关闭且 `Stop` 都被调用、外壳自己的 `HealthPort` 独立于任何模块响应、
  单模块 `Start()` 里的 panic 被 `recover` 住不会让整个进程崩溃（细节见
  `internal/shell/shell_test.go`，这是写这份骨架过程中才发现的一处真实缺口，
  不是从设计书推演出来的——`RunStandalone` 的等价调用不需要这层防护，单模块
  进程整个崩溃退出正是正确行为；11 个模块共享一个进程后，同一个疏漏会把另外
  10 个一起带走）。
- `TestRun_跨外壳依赖真实网络可达`：go-core（`mdm-customer`/`mdm-product`）
  与 go-backoffice（`crm-opportunity`）两个外壳真的**同时**跑起来（前面几条
  测试都是先后顺序跑完一个再跑下一个，从没验证过"两个外壳同时存在、互不
  干扰"这件事），用真身 gRPC 客户端桩代码直接拨号 go-core 的真实端口发起
  一次 `BatchGet`，验证跨外壳地址真的可达、对方的 gRPC handler → service
  层 → repo 层 → 共享连接池 `WithTx` 这一整条路径真的能跑通。⚠️ **没有**
  通过 crm-opportunity 自己的 REST 层驱动这次调用——那需要真实
  `iamJwksUrl`/`authzBundleUrl`（`infra-iam-casdoor`+`infra-authz`真实跑
  起来），复杂度不是这条测试要验的东西，留给 Task 9；crm-opportunity 的
  `backend/internal/client` 又是 Go 的 `internal` 包，本仓库物理上 import
  不到。gRPC 侧本项目目前没有任何鉴权（鉴权只在 REST 层），所以直接拨号
  验证网络可达性是自洽的，不构成绕过鉴权。

## 现状补充（阶段四 Task 9 完成，2026-09-13）——真机撞到一个此前未知的重要 bug

`internal/shell.Run` 新增 `exportDependencyEndpoints`：把每个模块 `Env` 里 `_ENDPOINT` 结尾的 key
真的 `os.Setenv` 进外壳进程环境，不再只喂进 `rt.Config`。

**根因**：`besdk.Endpoint()`（`SystemClient`/`UserClient` 内部都靠它）读的是 `os.LookupEnv`，不是
`rt.Config`——这是平台自己的既有设计（依赖地址按依赖方身份命名，同一个进程里所有消费者看到的值本来就
该一样）。合并部署之前这个变量由 brickKit 平台注入进每个独立容器**自己的** `os.Environ`；合并之后那些
容器不存在了，"把它设进进程环境"这件事从来没有人做过——不补上这一步，任何调用
`besdk.SystemClient`/`UserClient` 的代码路径在合并态下都会报"地址未注入"，即使 `rt.Config` 里其实有
这份数据。**Task 6/7/8 都没有撞到**，因为它们的验证范围止于健康检查/路由 fail-closed 状态码，从没有
真的走到一条会调用 `besdk.SystemClient`/`UserClient` 的代码路径——真机驱动一次真实认证的
`infra-iam-casdoor` 换 JWT（内部会拨号 `infra-authz`）才第一次暴露。

同一个 key 在同一个外壳的不同模块 `Env` 里理应完全一致（决定它的只有"我的外壳、对方的外壳"这对关系），
`exportDependencyEndpoints` 真的发现不一致就报错，不悄悄用后一个值覆盖前一个。

**真机验证（阶段四 Task 9，附录 E 全链路）**：修复后真实驱动一次 CRM 赢单（真实 REST + 真实 Casdoor
JWT）→ `erp-sales` 真实建单确认（自己的定价引擎重算单价）→ `erp-inventory` 真实锁库存 →
`erp-finance` 真实生成 AR 凭证，金额与订单完全对上；Saga 补偿路径（信用超限）验证订单停留
`DRAFT`、真实建出 `infra-workflow` 异常待办、`infra-notification` 正确路由；最后用真实订单数据调
`infra-print` 渲染出送货单 PDF。全程跨 `go-backoffice`/`go-core`/`py-render` 三个外壳。完整细节见父
仓库 `docs/plans/04-阶段四-做外壳验拆回.md` Task 9。

## 现状补充（阶段四附加 Task 0.2/0.3 完成，2026-09-14）——servedBy 落地，上面这个 bug 的修复代码本身退休了

brickKit 新增了 `servedBy` 机制（组件声明"我的工作负载由外壳组件 X 提供"，platform 原生支持一份
`brickkit.yaml` 里部分组件独立、部分组件被外壳收编）之后，上面 Task 9 那条 `exportDependencyEndpoints`
——连同它要读的 `be-ops` 产出 7（`shell-env.json`）——整个退休了：`servedBy` 落地后，brickKit 自己在
生成阶段就把 `*_ENDPOINT` 类变量直接合并进外壳容器**自己的** `os.Environ()`，这个进程一启动就已经看
得见，不再需要 `internal/shell.Run` 自己再 `os.Setenv` 一遍。函数本体与它的两条回归测试
（`TestExportDependencyEndpoints_*`）已从 `internal/shell/shell.go`/`shell_test.go` 删除。

⚠️ **这不代表上面 Task 9 记的那个 bug/根因分析是错的**——`besdk.Endpoint()` 读 `os.LookupEnv` 不是
`rt.Config` 这条平台既有设计依然成立，只是"谁负责把值放进 `os.Environ()`"这件事的责任方换了：以前是
`be-shell-go` 自己（因为平台完全不知道"外壳"这个概念），现在是 brickKit 自己（因为 `servedBy` 让平台
第一次知道了"这些组件的工作负载合并到了这个容器里"）。这段历史继续留着，是因为它是这条平台设计
（`os.LookupEnv` not `rt.Config`）第一次被真机验证出来的地方，具有独立的参考价值。

`cmd/shell/main.go` 的 `buildModules` 同时换了第二件事：不再无条件把 `shell-config.json` 里列出的
模块全部实例化，改成额外按平台原生注入的 `BRICKKIT_SERVED_MEMBERS`（这次真的被收编、活着的成员，
逗号分隔的版本化服务名）筛一遍——`shell-config.json` 现在表达的是"这个外壳理论上有哪些成员"，不再
等价于"这次都被收编了"。每个模块自己的 `configSchema` 解析结果（原来 `shell-env.json` 的 `Env` 字段）
也一并挪进了 `shell-config.json` 新增的 `Config` 字段（`be-ops` 侧的完整调研过程见装配仓库
`docs/plans/04b-验证记录.md` Task 0.2）。

## 现状补充（阶段四 Task 8 完成，2026-09-13）

Task 6 验证阶段那 3 个手动 `docker run` 起的容器，已经换成真正的
`infra/shell-compose.yml`（父仓库根目录）——接上了健康检查（`wget` 打
`SHELL_HEALTH_PORT`，不是 `--spider`：真机撞到过 FastAPI 的 `GET /` 不
支持 HEAD、`--spider` 发 HEAD 请求会被 405 误判成不健康，Go 侧的裸
`http.HandlerFunc` 因为不区分方法所以没暴露这个问题，Python 侧才踩到）、
`be-net`（直连 `be-postgres`/`be-nats` 容器 DNS，不再靠 `host.docker.internal`
间接寻址）、`SHELL_CONFIG_JSON`/`SHELL_ENV_JSON` 的自动生成与挂载
（`make shell-gen`）。根 `Makefile` 的 `make shell-up`/`make shell-down`
把设计书 §13.9 的互斥规矩写进了命令本身：`shell-up` 第一步无条件
`brickkit down`，`teardown-up` 第一步无条件停外壳——两个方向都真机验证过
`docker ps` 零残留。完整细节见父仓库 `docs/plans/04-阶段四-做外壳验拆回.md`
Task 8。

## 现状补充（阶段四 Task 11 完成，2026-09-13）——单个模块 panic 不再拖垮整个外壳

`internal/shell/shell_test.go` 第 163-194 行原来的既有测试
（Task 2 写的，只用假模块、只有 1 个模块）只验证过"`Run` 不会被一次 panic
崩掉整个测试进程，能干净返回一个包含 componentID 的错误"——从没验证过
"返回一个错误"这个结果本身对不对。混进真实模块之后才发现问题：
`shell.go` 里 `Start()` 的 panic 分支把 recover 到的内容转成 error 原样
`return` 给 errgroup，而 `errgroup.WithContext` 的既有语义是"任一
goroutine 返回非 nil error，立刻取消整个组共用的 `gctx`"——`gctx` 正是
全部模块的 HTTP/额外端口/`Start` 共用的同一个 ctx，取消它会把其它健康的
模块也一起带下线。这与"单个模块 panic 不该拖垮外壳其余模块"直接矛盾。

**真机复现**：新增 `internal/shell/real_modules_test.go` 的
`TestRun_一个模块panic不影响其它真实模块继续服务`——把 `mdm/customer`/
`mdm/product` 两个真实、零依赖组件模块，跟一个故意在 `Start()` 里 panic
的假模块混进同一次 `shell.Run`。改 `shell.go` 之前这条测试是真的红的：
panic 发生后两个真实模块的 `/healthz` 全部跟着停止响应。

**修复**：`Start()` 这条 goroutine 的 panic 分支 recover 之后只记日志，
不再把 error 流回 `g`（普通、非 panic 的 `Start()` 返回错误不受影响，
仍然按原样拖垮整个外壳——两者是不同性质的失败：panic 是这一个模块自己
代码里的 bug，其它模块的代码没有任何理由被牵连；`Start` 主动返回错误
通常意味着它依赖的外部资源整个不可用，其它模块大概率也在用同一份资源，
让外壳整体退出交给 Docker 重启仍是更安全的默认值）。代价：这个模块自己
的 `Start` 循环从此不会再被重启，直到整个外壳下一次重启——这是有意接受
的降级，日志会带上 `component_id` 清楚指出是谁。

`shell_test.go` 的原有测试相应更名为
`TestRun_单模块Start里panic不崩溃整个进程只隔离在这一个模块自己身上`，
判据从"断言 `Run` 返回错误"改成"断言 panic 发生之后这个模块自己的 HTTP
还在正常响应、`Run` 一直阻塞到 ctx 被取消才干净返回 `nil`"——这才是新
行为下真正想要的结果。

配套的"共享连接池下 `SET LOCAL` 越权测试"（漏加 schema 限定仍只读到自己
schema 的数据）物理上落在父仓库 `tools/be-acceptance/tier2/`（只需要
`be-sdk-go` + 真实 `TEST_PG_DSN`，不需要 import 任何组件仓库）。两条合起来
是父仓库 `make tier2` 的完整内容，完整细节见父仓库
`docs/plans/04-阶段四-做外壳验拆回.md` Task 11 与
`tools/be-acceptance/platform/README.md` 新增的"tier2"一节（用例 24/25）。

## 现状补充（阶段四 Task 10 完成）

§13.7 拆回门禁与铁律六 import 扫描的扩展全部落在父仓库
`tools/be-acceptance`/根 `brickkit.yaml`/`infra/scripts/weekly-teardown-gate.sh`
——本仓库自身语言层面不可能违反铁律六（它只能 import 各组件的公开
`backend/module` 包，Go 的 `internal/` 可见性规则物理上不允许它碰到任何
组件的内部实现），所以这条任务没有在本仓库留下任何代码改动，完整细节见
父仓库 `docs/plans/04-阶段四-做外壳验拆回.md` Task 10。

完整任务清单见 `docs/plans/04-阶段四-做外壳验拆回.md`；本地开发时可以自己建一份
`go.work`（`use ( . ../../tools/be-sdk-go )`）联调 `be-sdk-go` 未发布的改动，
不要提交它——`Dockerfile` 的最终构建只依赖 `go.mod`/`go.sum` 锁定的已发布版本。
