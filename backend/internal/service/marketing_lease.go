package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"video-canvas/internal/model"
	"video-canvas/internal/repository"
)

const marketingLeaseTTL = 30 * time.Second
const marketingHeartbeatInterval = marketingLeaseTTL / 3
const marketingFinishAttempts = 3

// MarketingOption 是营销运行安全选项；默认不允许回收升级前NULL租约。
type MarketingOption func(*MarketingService)

// WithMarketingLegacyNullLeaseDrainCompleted 只在旧消费者停领、在途调用结算且进程全部退出后开启旧NULL租约回收。
func WithMarketingLegacyNullLeaseDrainCompleted(completed bool) MarketingOption {
	return func(s *MarketingService) { s.legacyDrainCompleted = completed }
}

// claimMarketingStep 在父锁内原子认领queued；running重入绝不取得执行权。
func (s *MarketingService) claimMarketingStep(ctx context.Context, probe *model.MarketingRound, run *model.MarketingStepRun) (bool, error) {
	token := uuid.NewString()
	claimed := false
	err := s.repo.WithTx(ctx, func(tx repository.MarketingTx) error {
		r, err := lockMarketingRound(ctx, tx, probe.UserID, probe)
		if err != nil {
			return err
		}
		ok, err := tx.ClaimStepRun(ctx, run.ID, r.ID, r.ExecutionGen, token, s.now().Add(marketingLeaseTTL))
		claimed = ok
		if ok {
			run.ExecToken = token
			run.RoundGen = r.ExecutionGen
			run.Status = model.MarketingRunRunning
			*probe = *r
		}
		return err
	})
	return claimed, err
}

// heartbeatMarketingStep 仅在令牌仍属于本worker时续约；停止函数等待goroutine退出避免泄漏和迟到续期。
func (s *MarketingService) heartbeatMarketingStep(ctx context.Context, cancel context.CancelFunc, run *model.MarketingStepRun) func() {
	beatCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(marketingHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-beatCtx.Done():
				return
			case <-ticker.C:
				beatDeadline, cancelBeat := context.WithTimeout(beatCtx, marketingHeartbeatInterval/2)
				ok, err := s.repo.HeartbeatStepRun(beatDeadline, run.ID, run.ExecToken, s.now().Add(marketingLeaseTTL))
				cancelBeat()
				if err == nil && !ok {
					cancel()
					return
				}
			}
		}
	}()
	return func() { stop(); <-done }
}

// finishMarketingWithRetry 对已获得真实用量的结局以相同令牌有限次提交；不再次调用上游。
func (s *MarketingService) finishMarketingWithRetry(ctx context.Context, round *model.MarketingRound, run *model.MarketingStepRun, out *stepOutcome) error {
	var err error
	for attempt := 0; attempt < marketingFinishAttempts; attempt++ {
		commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		err = s.finishRun(commitCtx, round, run, out)
		cancel()
		if err == nil || errors.Is(err, repository.ErrNotFound) {
			return err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 50 * time.Millisecond)
		<-timer.C
	}
	return err
}

// marketingRunCanceled 根据持久化执行代、标志、轮次和存活项目判断上游调用是否必须停止。
func (s *MarketingService) marketingRunCanceled(ctx context.Context, run *model.MarketingStepRun) bool {
	cur, err := s.repo.Run(ctx, run.UserID, run.ID, false)
	if err != nil {
		return mktNotFound(err)
	}
	r, err := s.repo.Round(ctx, run.UserID, cur.RoundID, false)
	if err != nil {
		return mktNotFound(err)
	}
	if cur.CancelRequested || cur.RoundGen != r.ExecutionGen || r.Status == model.MarketingRoundCanceled {
		return true
	}
	_, err = s.repo.Project(ctx, r.UserID, r.ProjectID, false)
	return mktNotFound(err)
}
