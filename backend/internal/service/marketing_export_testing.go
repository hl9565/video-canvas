package service

import (
	"context"

	"video-canvas/internal/model"
	"video-canvas/internal/provider"
	"video-canvas/internal/repository"
	"video-canvas/internal/stepkit"
)

// FinishMarketingRunForTest 仅供外部测试注入带真实用量的迟到结局，走生产结算与落库路径。
func (s *MarketingService) FinishMarketingRunForTest(ctx context.Context, round *model.MarketingRound, run *model.MarketingStepRun, res *stepkit.Result, snap *provider.Snapshot, apply func(context.Context, repository.MarketingTx, *model.MarketingRound) error) error {
	return s.finishRun(ctx, round, run, &stepOutcome{status: model.MarketingRunSucceeded, res: res, snap: snap, charge: true, apply: apply})
}

// ExecMarketingStepForTest 仅供外部测试重入单步生产执行入口。
func (s *MarketingService) ExecMarketingStepForTest(ctx context.Context, round *model.MarketingRound, run *model.MarketingStepRun) {
	_ = s.execStep(ctx, round, run)
}

// LatestMarketingRunsForTest 仅供外部测试验证执行代与主键排序。
var LatestMarketingRunsForTest = latestRuns
