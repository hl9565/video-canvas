package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"video-canvas/internal/config"
	"video-canvas/internal/initialize"
	"video-canvas/internal/pkg/logger"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
)

const drainLegacyUsage = `用法：server [-c 配置文件] marketing-drain-legacy-orphans -legacy-claim-stopped -legacy-inflight-settled -legacy-workers-exited`

// runDrainLegacyMarketing 是一次性运维子命令：必须显式确认“旧消费者停领、在途调用已结算、旧进程全部退出”三项前置条件后，
// 才回收 0035 升级前 exec_token=” 且 lease_until IS NULL 的营销孤儿及其冻结积分。
func runDrainLegacyMarketing(args []string, configPath string, out io.Writer) error {
	fs := flag.NewFlagSet("marketing-drain-legacy-orphans", flag.ContinueOnError)
	path := fs.String("c", configPath, "配置文件路径")
	claimStopped := fs.Bool("legacy-claim-stopped", false, "确认升级前旧版本消费者已停止认领新任务")
	inflightSettled := fs.Bool("legacy-inflight-settled", false, "确认所有在途营销调用与计费均已结算")
	workersExited := fs.Bool("legacy-workers-exited", false, "确认所有升级前旧版本进程均已退出")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*claimStopped || !*inflightSettled || !*workersExited {
		return fmt.Errorf("拒绝执行：必须同时传入 -legacy-claim-stopped、-legacy-inflight-settled、-legacy-workers-exited 确认三项前置条件\n%s", drainLegacyUsage)
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	if err := logger.Init(cfg.Log); err != nil {
		return fmt.Errorf("初始化日志: %w", err)
	}
	defer logger.Sync()

	db, err := initialize.OpenDB(cfg.Database)
	if err != nil {
		return err
	}
	svc := service.NewMarketingService(
		service.MarketingDeps{Repo: repository.NewMarketingRepository(db)},
		service.WithMarketingLegacyNullLeaseDrainCompleted(true),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rounds, err := svc.DrainLegacyNullLeaseOrphans(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "已完成旧NULL租约营销孤儿排空恢复：处理轮次 %d\n", rounds)
	return nil
}
