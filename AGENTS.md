# be-shell-go · AI 助手导读

## 身份证

| 项 | 值 |
|---|---|
| 仓库名 | `be-shell-go` |
| 目录 | `shells/go/`（不进 `brickkit.yaml`，不是 brickKit 组件） |
| 语言 / 框架 | Go；只依赖 `be-sdk-go`、`golang-migrate`（`iofs` 源驱动）、`golang.org/x/sync/errgroup` |
| 装的模块 | 阶段四：11 个 Go 组件（`mdm-customer`/`mdm-product`/`erp-inventory`/`erp-finance`/`erp-sales`/`infra-authz`/`infra-iam-casdoor`/`infra-workflow`/`infra-notification`/`integration-im-dingtalk`/`crm-opportunity`）——9/11 个已在 `internal/shell/real_modules_test.go` 里真机验证过合并进程，`infra-iam-casdoor`/`integration-im-dingtalk` 留给单独一轮任务；`cmd/shell/main.go` 的 `Config.Modules` 仍是空的，真机 `brickkit.yaml` 切换是 Task 6 剩余部分，见 `README.md` 待办 |
| 设计真相源 | 《BrickEnterprise 设计书.md》第 13 章（为什么、七条铁律、代价）+ `docs/plans/04-阶段四-做外壳验拆回.md`（本仓库具体要做什么）+ `docs/design/_调研记录/04-阶段四.md`（技术判断的推演过程）——本文件与它们冲突时，以那三份为准 |

## 这个仓库存在的唯一理由

把 N 个组件模块（各自导出的 `.../backend/module".New`）塞进一个进程——除此之外
不做任何事。**外壳不许做的事**（`shells/README.md`、设计书 §13.3 铁律六）：

- ❌ 不许让两个组件模块直接互相 `import`（这个仓库唯一被允许 `import` 的组件代码
  是各自的 `backend/module` 公开包——Go 的 `internal/` 可见性规则物理上不允许碰到
  更深的实现细节，不需要额外的纪律去守这条）
- ❌ 不许把两个组件的表放进同一个 schema、不许跨 schema JOIN
- ❌ 不许把 N 个模块的 API 合并成一个端口（`ModuleSpec.HTTPPort` 必须逐个不同）
- ❌ 不许在外壳里写任何业务逻辑——`internal/shell.Run` 的每一行都应该是"怎么把
  模块组装起来"，不是"这个模块该怎么处理某个字段"

## 核心判断：复用 `be-sdk-go`，不重新实现

`internal/shell.Run` 的字段构造、`Listen`/优雅关闭、gRPC panic 恢复，全部调用
`be-sdk-go` 已经在 `RunStandalone`（单模块场景）里验证过的
`NewShellRuntime`/`InitShellAuthz`/`ServeHTTP`/`ServeExtraPort`——**新增任何"外壳
要多做一点什么"的需求，第一反应应该是"这段逻辑要不要提到 `be-sdk-go`里、变成一个
可以被 `RunStandalone` 和外壳共同复用的构件"，而不是直接在这个仓库里另起一份实现**。
理由是阶段三踩坑记录 A4g：一条只有小范围调用方走过的构造路径，跟生产真正走的那条
路径不是同一条，就是真实 bug 藏身的地方——外壳如果自己重写 `be-sdk-go` 已经踩过坑
修好的逻辑，等于在 11 倍的爆炸半径上重演同一类风险。

`WithTx`/`NewConfig`/`Module.DB`/`Module.NATS` 这些字段的既有设计从项目建仓库起
就已经支持"外壳共享一个池"这个场景（见各自的既有注释），不需要为了适配外壳再改。

## 已知的、故意留到后面任务的缺口（不是遗漏，是任务顺序）

- `Config.Modules` 目前是空的——Task 5/6 才会填真实模块，之前跑这个二进制只会
  一直挂着响应 `HealthPort`，不做任何真实事情。
- `Config.PGDSN`/`NATSURL`/`IamJwksURL`/`AuthzBundleURL`/每个 `ModuleSpec.Env` 目前
  只能靠 `cmd/shell/main.go` 手写或读裸环境变量——Task 4 的 `be-ops` 产出 4/7 落地
  后，这里要换成读它们生成的合并清单/环境变量表文件，不是继续手写。
- `HealthPort` 目前默认写死 `18888`——Task 4/8 的 `be-ops` 产出 8（`shell-compose.yml`）
  落地后，这个端口该是多少、健康检查探针的具体命令，由那一步决定，这里只是先给
  骨架一个能跑起来验证的默认值。
- 产出 7 的"跨外壳"环境变量改写分支，在本阶段的真实依赖图里大概率不会被真实
  触发（`docs/design/_调研记录/04-阶段四.md` §4 有完整推演）——如果以后（阶段
  五/六）这个仓库真的需要处理"依赖另一个外壳里的模块"的场景，先去读那份调研记录，
  不要凭直觉重新论证一遍。

## 测试

`internal/shell` 的测试全部用假模块（`shell_test.go` 的 `fakeModule`），不依赖任何
真实组件仓库——这是刻意的：外壳骨架测的是"装配机制本身对不对"，不是"某个具体
组件对不对"，混在一起会让骨架的测试跟着组件的迭代节奏一起变。`TEST_NATS_URL`
需要一个真实可达的 NATS（本机 `make up` 起的 `be-nats` 或任何其它实例）——`sql.Open`
是懒的，`Config.PGDSN` 不需要真的能连上；`nats.Connect` 会真的拨号，用假地址会让
测试直接失败在连接这一步，不是失败在真正想测的断言上。
