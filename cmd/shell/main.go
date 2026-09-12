// be-shell-go 的进程入口——目前只是骨架（阶段四计划 Task 2）：
// Modules 列表故意留空，Task 5/6 才会把 11 个真实 Go 组件的
// `.../backend/module".New` 静态 import 进来并按拓扑序填进这里。
//
// 端口/env/schema 这些数据本该来自 `be-ops` 产出 4/7（合并清单 + 每
// 外壳环境变量表，Task 4），产出 4/7 落地前这里先留一句提醒，不假装
// 已经有一个真实的生成器可以调用。
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	besdk "github.com/brickKit/be-sdk-go"
	"github.com/brickKit/be-shell-go/internal/shell"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	pgDSN, err := besdk.BuildPGDSN()
	if err != nil {
		log.Fatalf("拼外壳共享数据库连接串失败：%v", err)
	}

	// ⚠️ SHELL_HEALTH_PORT 不是平台注入的（外壳根本不是 brickKit
	// 组件）——be-ops 产出 8（shell-compose.yml）生成时会把这个端口
	// 写进 healthcheck 配置，同时把同一个值以这个变量名注入外壳容器，
	// 这里只负责读、不负责决定这个端口该是多少。Task 4/8 之前先给一个
	// 明显不常用的默认值，方便本地手动验证骨架。
	healthPort, _ := strconv.Atoi(os.Getenv("SHELL_HEALTH_PORT"))
	if healthPort == 0 {
		healthPort = 18888
	}

	cfg := shell.Config{
		ShellName:   "be-shell-go",
		OTelBaseURL: os.Getenv("OTEL_BASE_URL"),
		PGDSN:       pgDSN,
		NATSURL:     besdk.BuildNATSURL(),
		HealthPort:  healthPort,
		// ⚠️ 阶段四 Task 5/6 之前，这里故意是空的——真实的 11 个模块要
		// 等 be-ops 产出 4/7（合并清单+每外壳环境变量表）落地、且各组件
		// 的 go.mod 依赖被正式 require 进来之后才能填。在那之前跑这个
		// 二进制只会一直挂着响应健康检查（HealthPort），errgroup 里
		// 没有任何模块 goroutine，g.Wait() 只等 ctx 取消。
		Modules: nil,
	}

	if err := shell.Run(ctx, cfg, besdk.NewLogger("be-shell-go")); err != nil {
		log.Fatalf("外壳异常退出：%v", err)
	}
}
