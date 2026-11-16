package service

import (
	"context"
	"errors"
	"time"

	"video-canvas/internal/pkg/errcode"

	"video-canvas/internal/marketing"
	"video-canvas/internal/model"
	"video-canvas/internal/repository"
)

// RecoverUnsettledMarketingRuns 扫描所有queued/running运行，在真实父锁事务中按执行代与租约回收失活运行。
// 默认保护0035旧NULL租约；存活worker只响应取消，保留真实用量结算权，不重调上游。
func (s *MarketingService) RecoverUnsettledMarketingRuns(ctx context.Context) error {
	// 1. 扫描不限定轮次状态，包含取消/软删后的孤儿；不设截断，避免活跃小ID饿死后面的孤儿。
	runs, err := s.repo.ListUnsettledRuns(ctx, 0)
	if err != nil {
		return err
	}
	seen := map[uint64]bool{}
	for _, run := range runs {
		if seen[run.RoundID] {
			continue
		}
		seen[run.RoundID] = true
		round, err := s.repo.RoundByID(ctx, run.RoundID, false)
		if mktNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if err := s.recoverMarketingRound(ctx, round); err != nil {
			return err
		}
		if err := s.resumeMarketingRound(ctx, round); err != nil {
			return err
		}
	}
	return nil
}

// DrainLegacyNullLeaseOrphans 在确认旧消费者停领、在途结算且进程已全部退出并开启 legacyDrainCompleted 后，
// 仅扫描包含 0035 旧空令牌/NULL租约运行孤儿的轮次并回收其运行与冻结积分；未满足前置条件时拒绝执行。
func (s *MarketingService) DrainLegacyNullLeaseOrphans(ctx context.Context) (int, error) {
	if !s.legacyDrainCompleted {
		return 0, errcode.ErrInvalidParams.WithMsg("必须先确认旧消费者停领、在途调用已结算并全部退出后再开启旧NULL租约孤儿回收")
	}
	runs, err := s.repo.ListUnsettledRuns(ctx, 0)
	if err != nil {
		return 0, err
	}
	seen := map[uint64]bool{}
	reclaimedRounds := 0
	for _, run := range runs {
		if run.Status != model.MarketingRunRunning || run.ExecToken != "" || run.LeaseUntil != nil || seen[run.RoundID] {
			continue
		}
		seen[run.RoundID] = true
		round, err := s.repo.RoundByID(ctx, run.RoundID, false)
		if mktNotFound(err) {
			continue
		}
		if err != nil {
			return reclaimedRounds, err
		}
		if err := s.recoverMarketingRound(ctx, round); err != nil {
			return reclaimedRounds, err
		}
		reclaimedRounds++
	}
	return reclaimedRounds, nil
}

// recoverMarketingRound 只在可信双键父锁内关闭陈旧queued或CAS回收过期running；事务外再收口轮次。
func (s *MarketingService) recoverMarketingRound(ctx context.Context, probe *model.MarketingRound) error {
	err := s.repo.WithTx(ctx, func(tx repository.MarketingTx) error {
		p, r, err := lockMarketingBackground(ctx, tx, probe)
		if err != nil {
			return err
		}
		runs, err := tx.RoundRuns(ctx, r.ID)
		if err != nil {
			return err
		}
		// 1. 先完成脚本写入决策，再取得UserCredit；后续循环不再加脚本锁。
		return s.recoverMarketingRunsLocked(ctx, tx, p, r, runs)
	})
	if err != nil {
		return err
	}
	_, err = s.finalizeRound(ctx, probe.ID)
	return err
}

// recoverMarketingRunsLocked 在完整父链锁内先释放当前代失活Follow，再逐个结算，确保积分锁最后取得。
func (s *MarketingService) recoverMarketingRunsLocked(ctx context.Context, tx repository.MarketingTx, p *model.MarketingProject, r *model.MarketingRound, runs []model.MarketingStepRun) error {
	var reclaimed []model.MarketingStepRun
	for i := range runs {
		run := &runs[i]
		stale := staleMarketingRun(p, r, run)
		if run.Status == model.MarketingRunQueued && stale {
			reclaimed = append(reclaimed, *run)
			continue
		}
		if !s.marketingLeaseExpired(run, s.now()) {
			continue
		}
		ok, err := s.reclaimMarketingRunLocked(ctx, tx, r, run, stale)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		reclaimed = append(reclaimed, *run)
	}
	// 2. CAS成功的running已关闭；queued仍需状态CAS；所有退款与状态提交在同一事务。
	for i := range reclaimed {
		run := &reclaimed[i]
		if run.Status == model.MarketingRunQueued {
			if err := s.closeRun(ctx, tx, run, model.MarketingRunCanceled, ""); err != nil {
				return err
			}
		} else if err := s.settleRun(ctx, tx, run, 0); err != nil {
			return err
		}
	}
	return nil
}

// marketingLeaseExpired 仅非空令牌的过期租约可即时判死；旧NULL租约须显式确认排空。
func (s *MarketingService) marketingLeaseExpired(run *model.MarketingStepRun, now time.Time) bool {
	if run.Status != model.MarketingRunRunning {
		return false
	}
	if run.ExecToken != "" && run.LeaseUntil != nil {
		return !run.LeaseUntil.After(now)
	}
	return run.ExecToken == "" && run.LeaseUntil == nil && s.legacyDrainCompleted
}

// hasQueuedMarketingRun 判断依赖层是否有可认领运行；只有存活running时退出等待后续投递。
func hasQueuedMarketingRun(runs []model.MarketingStepRun) bool {
	for _, run := range runs {
		if run.Status == model.MarketingRunQueued {
			return true
		}
	}
	return false
}

// reclaimMarketingRunLocked CAS回收已失活运行；只释放活跃当前代的Follow脚本。
func (s *MarketingService) reclaimMarketingRunLocked(ctx context.Context, tx repository.MarketingTx, r *model.MarketingRound, run *model.MarketingStepRun, stale bool) (bool, error) {
	ok, err := tx.ReclaimExpiredStepRun(ctx, run.ID, run.ExecToken, s.now(), s.legacyDrainCompleted)
	if err != nil || !ok {
		return ok, err
	}
	if !stale && r.Status == model.MarketingRoundScripting && run.StepKey == marketing.StepFollow {
		if err := s.releaseRewriting(ctx, tx, r.ID); err != nil {
			return false, err
		}
	}
	return true, nil
}

// currentMarketingRuns 保留各步的历史结果，但排除无执行权的陈旧在途运行，避免阻塞当前代依赖层。
func currentMarketingRuns(runs []model.MarketingStepRun, gen int64) []model.MarketingStepRun {
	out := make([]model.MarketingStepRun, 0, len(runs))
	for _, run := range latestRuns(runs) {
		active := run.Status == model.MarketingRunQueued || run.Status == model.MarketingRunRunning
		if active && (run.RoundGen != gen || run.CancelRequested) {
			continue
		}
		out = append(out, run)
	}
	return out
}

// resumeMarketingRound 为恢复后仍有queued运行的存活轮次补投队列，覆盖依赖worker崩溃导致的后续步骤停滞。
func (s *MarketingService) resumeMarketingRound(ctx context.Context, probe *model.MarketingRound) error {
	return s.repo.WithTx(ctx, func(tx repository.MarketingTx) error {
		r, err := lockMarketingRound(ctx, tx, probe.UserID, probe)
		if mktNotFound(err) || errors.Is(err, errcode.ErrMarketingNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if r.Status != model.MarketingRoundScripting {
			return nil
		}
		runs, err := tx.RoundRuns(ctx, r.ID)
		if err != nil {
			return err
		}
		if !hasQueuedMarketingRun(currentMarketingRuns(runs, r.ExecutionGen)) {
			return nil
		}
		return tx.EnqueueRound(ctx, r.ID)
	})
}
