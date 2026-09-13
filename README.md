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
- `cmd/shell`：进程入口，已经接上 `be-ops` 产出 4（`shell-config.json`）/
  产出 7（`shell-env.json`）——同一份镜像按 `SHELL_NAME` 环境变量的值
  （`go-core`/`go-backoffice`/`go-infra`）从两份数据文件里挑出自己要装的
  外壳，`moduleRegistry` 是本仓库唯一"componentId 字符串 → 真实 Go 源码
  import"的静态映射（数据驱动配置，代码驱动装配，`ModuleSpec.New` 不能
  从数据文件动态加载，见 `internal/shell.ModuleSpec` 的既有注释）。
  `cmd/shell/main_test.go` 用手写夹具覆盖了"按外壳挑模块"/"外壳不存在时
  报错"/"外壳还没原子式切换完时报错"/"11 个真实组件都在 moduleRegistry
  里"四类断言，不需要真实基础设施。
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

## 待办（阶段四后续任务）

⚠️ **当前真机跑着的 3 个外壳容器是手动 `docker run` 起的，不是 `shell-compose.yml`
（那是 Task 8 的产出）**——没有健康检查探针、没有重启策略、没有跟
`docker-compose.infra.yml`/`brickkit up` 接成一条启停链，`SHELL_CONFIG_JSON`/
`SHELL_ENV_JSON` 也是手动挂载的宿主机文件，不是部署流程自动生成/分发的。
这是刻意的：Task 6 只要求证明"原子式切换到 local: true 之后，真机能起
3 个外壳容器、11 个模块都在里面正常跑"，编排层面的规范化是 Task 8 自己
的范围，不要在这里提前把两件事混在一起判断"做完了"。

- Task 7：`infra-print` 迁进 `be-shell-python`（Python 外壳，与本仓库无关）。
- Task 8：三份 compose 编排（`docker-compose.infra.yml` + `brickkit up` 生成的
  compose + `be-ops` 产出 8 的 `shell-compose.yml`）+ 启停脚本——把上面那 3 个
  手动容器换成真正的 `shell-compose.yml`，接上健康检查、`be-net`、
  `SHELL_CONFIG_JSON`/`SHELL_ENV_JSON` 的自动分发。
- Task 9：合并态业务闭环真机验证（附录 E 全链路）——包括驱动一次真实
  authenticated 的 `CreateOpportunity`，验证跨外壳依赖在**带真实权限判定**
  的完整链路下也能正常工作，不只是网络层面可达。
- Task 10：§13.7 拆回门禁 + 铁律六 import 扫描扩展到本仓库（本仓库自身语言层面
  不可能违反铁律六——它只能 import 各组件的公开 `backend/module` 包，Go 的
  `internal/` 可见性规则物理上不允许它碰到任何组件的内部实现；真正的风险点是
  组件之间互相 `import`，那条边完全不会出现在本仓库自己的依赖图里，根
  `make gates` 的扫描器才是这条铁律的守卫）。

完整任务清单见 `docs/plans/04-阶段四-做外壳验拆回.md`；本地开发时可以自己建一份
`go.work`（`use ( . ../../tools/be-sdk-go )`）联调 `be-sdk-go` 未发布的改动，
不要提交它——`Dockerfile` 的最终构建只依赖 `go.mod`/`go.sum` 锁定的已发布版本。
