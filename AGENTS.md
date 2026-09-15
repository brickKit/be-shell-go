# be-shell-go · AI 助手导读

## 身份证

| 项 | 值 |
|---|---|
| 仓库名 | `be-shell-go` |
| 目录 | `shells/go/`（不进 `brickkit.yaml`，不是 brickKit 组件） |
| 语言 / 框架 | Go；只依赖 `be-sdk-go`、`golang-migrate`（`iofs` 源驱动）、`golang.org/x/sync/errgroup` |
| 装的模块 | 阶段四：11 个 Go 组件（`mdm-customer`/`mdm-product`/`erp-inventory`/`erp-finance`/`erp-sales`/`infra-authz`/`infra-iam-casdoor`/`infra-workflow`/`infra-notification`/`integration-im-dingtalk`/`crm-opportunity`）——**全部 11 个**都已在 `internal/shell/real_modules_test.go` 里真机验证过合并进程；`brickkit.yaml` 已从 `local: true` 全量切到真实 `servedBy`（阶段四附加 Task 0.4，外壳本身也是真实 brickKit 组件，见 `shells/go/deploy/shell/*/component.yaml`）；`cmd/shell/main.go` 解析平台原生注入的 `BRICKKIT_SERVED_MEMBERS_CONFIG`（brickKit v0.4.2 起原生支持，取代了此前 `be-ops shell-config` 手工生成、贴进 `configSchema` 的 `SHELL_CONFIG_JSON`，阶段四附加 Task 0.6）装配 `Config.Modules`，真机起过 4 个外壳容器，见 `README.md` |
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

- `Config.Modules` 已经真实装配（`cmd/shell/main.go` 解析平台原生注入的
  `BRICKKIT_SERVED_MEMBERS_CONFIG`——一个 JSON 数组，每个元素是这次真的被
  这个外壳收编的成员，含 `componentId`/`version`/`httpPort`/`extraPorts`/
  合并后的 `config`，`config` 键是原始 configSchema 驼峰 key，需要
  `configEnvVarName` 转成 `SCREAMING_SNAKE_CASE` 才能被 `besdk.Config`
  正确查到），`Config.PGDSN`/`NATSURL`/`IamJwksURL`/`AuthzBundleURL`
  仍然是靠 `cmd/shell/main.go` 读容器自己的环境变量（`DATABASE_*`/`MQ_*`/
  `IAM_JWKS_URL`/`AUTHZ_BUNDLE_URL`）——这些是"外壳这个容器本身"的身份，不是任何
  一个模块的数据，本来就不该来自模块自己的 `Config`（见
  `internal/shell.ModuleSpec.Env` 的字段注释），现状是符合设计的终态，不是待办。
- `SHELL_NAME`/`SHELL_HEALTH_PORT` 来自外壳自己的 `component.yaml`
  （`shells/go/deploy/shell/*/`）的 `configSchema`，值写在 `brickkit.yaml`
  该外壳组件的 `config:` 块里。`BRICKKIT_SERVED_MEMBERS_CONFIG` 是平台
  原生注入（brickKit v0.4.2 起，阶段四附加 Task 0.6），不需要我们自己
  再手工生成/维护一份平行数据——此前 `SHELL_CONFIG_JSON` + `be-ops
  shell-config` 那一整套机制（阶段四附加 Task 0.4）已经整个退休，历史
  记录见父仓库 `docs/plans/04b-验证记录.md` Task 0.4/0.6。
  `infra/shell-compose.yml`/`make shell-gen`/`make shell-up` 那一套更早的
  手写编排也已经退休（父仓库 Task 0.5，`brickkit up --ignore-served-by`
  取代了它验证组件独立性的用途）。
  ⚠️ 健康检查命令是普通 `wget -q -O /dev/null`，不是 `--spider`——那个坑是
  Python 侧才踩到的（FastAPI 的 `GET /` 不支持 HEAD），但两边的 Dockerfile/
  compose 判据保持一致，不要为了"Go 这边其实没事"就改回 `--spider`。
- 跨外壳的依赖地址（`*_ENDPOINT`）**已经真机触发验证过**（`go-core`/
  `go-backoffice` 跨容器请求 `go-infra` 暴露的 `authzBundleUrl`/`iamJwksUrl`），
  不再是"大概率不会被触发"的推演状态——`docs/design/_调研记录/04-阶段四.md` §4/§12
  记录的是这个结论的推翻过程，仍然值得读，但结论本身已经是"真的被触发过而且工作
  正常"，不是待验证。

## 测试

`internal/shell` 的测试全部用假模块（`shell_test.go` 的 `fakeModule`），不依赖任何
真实组件仓库——这是刻意的：外壳骨架测的是"装配机制本身对不对"，不是"某个具体
组件对不对"，混在一起会让骨架的测试跟着组件的迭代节奏一起变。`TEST_NATS_URL`
需要一个真实可达的 NATS（本机 `make up` 起的 `be-nats` 或任何其它实例）——`sql.Open`
是懒的，`Config.PGDSN` 不需要真的能连上；`nats.Connect` 会真的拨号，用假地址会让
测试直接失败在连接这一步，不是失败在真正想测的断言上。
