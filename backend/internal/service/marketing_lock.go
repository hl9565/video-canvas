package service

import (
	"context"
	"slices"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/repository"
)

// activeMarketingRound 在锁外探查父链；锁内必须重新验证，探查结果绝不授予写权限。
func (s *MarketingService) activeMarketingRound(ctx context.Context, userID, roundID uint64) (*model.MarketingRound, error) {
	r, err := s.repo.Round(ctx, userID, roundID, false)
	return r, marketingPublicError(err)
}

// marketingPublicError 将不存在转换成营销404，避免透出资源归属。
func marketingPublicError(err error) error {
	if mktNotFound(err) {
		return errcode.ErrMarketingNotFound
	}
	return err
}

// lockMarketingRound 公开写事务先锁存活项目，再锁轮次、去重升序脚本；软删父项目立即404。
func lockMarketingRound(ctx context.Context, tx repository.MarketingTx, userID uint64, probe *model.MarketingRound) (*model.MarketingRound, error) {
	p, err := tx.Project(ctx, userID, probe.ProjectID, true)
	if err != nil {
		return nil, marketingPublicError(err)
	}
	r, err := tx.Round(ctx, userID, probe.ID, true)
	if err != nil {
		return nil, marketingPublicError(err)
	}
	if r.UserID != p.UserID || r.ProjectID != p.ID {
		return nil, errcode.ErrMarketingNotFound
	}
	if _, err = lockMarketingScripts(ctx, tx, r); err != nil {
		return nil, marketingPublicError(err)
	}
	return r, nil
}

// lockMarketingScripts 在已锁项目和轮次后按主键去重升序锁定脚本并复核完整父链。
func lockMarketingScripts(ctx context.Context, tx repository.MarketingTx, r *model.MarketingRound) ([]model.MarketingScript, error) {
	rows, err := tx.RoundScripts(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	ids := make([]uint64, 0, len(rows))
	for _, sc := range rows {
		ids = append(ids, sc.ID)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	locked := make([]model.MarketingScript, 0, len(ids))
	for _, id := range ids {
		sc, err := tx.Script(ctx, r.UserID, id, true)
		if err != nil {
			return nil, err
		}
		if sc.UserID != r.UserID || sc.ProjectID != r.ProjectID || sc.RoundID != r.ID {
			return nil, repository.ErrNotFound
		}
		locked = append(locked, *sc)
	}
	return locked, nil
}

// lockMarketingBackground 仅供后台收尾恢复使用持久化轮次的可信双键锁项目（含软删），再锁子行。
func lockMarketingBackground(ctx context.Context, tx repository.MarketingTx, probe *model.MarketingRound) (*model.MarketingProject, *model.MarketingRound, error) {
	p, err := tx.ProjectUnscopedLock(ctx, probe.UserID, probe.ProjectID)
	if err != nil {
		return nil, nil, err
	}
	r, err := tx.Round(ctx, probe.UserID, probe.ID, true)
	if err != nil {
		return nil, nil, err
	}
	if r.UserID != p.UserID || r.ProjectID != p.ID {
		return nil, nil, repository.ErrNotFound
	}
	if _, err = lockMarketingScripts(ctx, tx, r); err != nil {
		return nil, nil, err
	}
	return p, r, nil
}

// staleMarketingRun 以持锁持久化状态判断执行代、取消标志和父项目删除，不使用worker旧快照定权限。
func staleMarketingRun(p *model.MarketingProject, r *model.MarketingRound, run *model.MarketingStepRun) bool {
	return p.DeletedAt.Valid || run.CancelRequested || run.RoundGen != r.ExecutionGen || r.Status == model.MarketingRoundCanceled
}

// lockMarketingScript 先锁存活项目与轮次的完整脚本集合，再复核目标脚本的父链。
func lockMarketingScript(ctx context.Context, tx repository.MarketingTx, userID uint64, probe *model.MarketingScript) (*model.MarketingScript, error) {
	r, err := lockMarketingRound(ctx, tx, userID, &model.MarketingRound{ID: probe.RoundID, ProjectID: probe.ProjectID})
	if err != nil {
		return nil, err
	}
	sc, err := tx.Script(ctx, userID, probe.ID, false)
	if err != nil {
		return nil, marketingPublicError(err)
	}
	if sc.ProjectID != r.ProjectID || sc.RoundID != r.ID || sc.UserID != r.UserID {
		return nil, errcode.ErrMarketingNotFound
	}
	return sc, nil
}
