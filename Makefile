# be-shell-go 既不是 brickKit 组件也不是纯横切库——按总纲 §I 的 9 个
# 门禁目标写，保持与其它仓库一致的心智模型；对本仓库现状没意义的项
# 如实标 N/A，不硬凑。
.DEFAULT_GOAL := help
.PHONY: help check-version test image migrate-idempotent dag-check contract-check \
        import-scan smoke module-check all

help:  ## 列出所有目标
	@awk 'BEGIN{FS=":.*##"; printf "\n用法: make <目标>\n\n"} \
	     /^[a-zA-Z0-9_-]+:.*##/ {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2} \
	     /^##@/ {printf "\n\033[1m%s\033[0m\n", substr($$0,5)}' $(MAKEFILE_LIST)
	@echo ""

##@ 对本仓库现状没意义的（阶段四 Task 5/6 真的装进真实模块之前）
check-version:  ## N/A：不是 brickKit 组件，没有 component.yaml
	@echo "N/A：非组件仓库，没有 component.yaml"

migrate-idempotent:  ## N/A：迁移由 internal/shell.Run 按模块跑，不是独立的迁移二进制
	@echo "N/A：迁移逻辑在 internal/shell 里随 Run 一起测（见 test 目标），没有独立的迁移命令"

contract-check:  ## N/A：没有 contracts/，外壳不持有任何契约
	@echo "N/A：外壳只组装模块，不定义任何契约"

smoke:  ## N/A：需要真实模块 + brickkit.yaml 的 shell-compose 才有意义，阶段四 Task 5/6/8 之前不适用
	@echo "N/A：Task 5/6 装真实模块、Task 8 接 shell-compose 之前，没有可冒烟的对象"

module-check:  ## N/A：外壳是 module.New 的消费方，不是提供方，没有自己的模块入口契约
	@echo "N/A：这条门禁检查组件自己的 module.New 是否合规，外壳不提供 module.New"

##@ 真实生效的
test:  ## 跑全部单测（-race），internal/shell 的测试需要一个可达的 NATS（TEST_NATS_URL，默认 nats.DefaultURL）
	go test ./... -race

image:  ## 构建外壳镜像（本地构建，不在 brickKit 签名覆盖范围内，设计书 §13.3 铁律五下方）
	docker build -t brickenterprise/be-shell-go:$${VERSION:-dev} .

dag-check:  ## 包依赖图无环（Go 编译器本身就不允许循环 import，这条恒过）
	@go list ./... >/dev/null && echo "✓ 包依赖图无环（Go 编译器本身就不允许循环 import）"

import-scan:  ## 铁律六：外壳可以 import 各组件的 backend/module，但组件之间绝不能互相 import——本仓库自己的 import 图不会出现这条边（只有真正的跨组件违规才会），真正的守卫在根 make gates（阶段四 Task 10 扩展范围）
	@go list -deps ./... | grep -c "github.com/brickKit/" || true
	@echo "✓ 本仓库自身没有能力违反铁律六（它不 import 任何组件的 internal/ 包，Go 语言本身就不允许）——根 make gates 的扫描器是这条铁律的真正守卫"

all: test dag-check import-scan  ## 本仓库当前有意义的门禁全部跑一遍
