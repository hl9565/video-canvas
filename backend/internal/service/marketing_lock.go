package service

import (
	"context"
	"slices"

	"video-canvas/internal/pkg/jsonhash"

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

// lockedScriptContext 只从持锁事务重建父链、设置和内容快照，锁外探查不能授予写权限。
func lockedScriptContext(ctx context.Context, tx repository.MarketingTx, userID uint64, probe *model.MarketingScript) (*scriptCtx, error) {
	sc, err := lockMarketingScript(ctx, tx, userID, probe)
	if err != nil {
		return nil, err
	}
	r, err := tx.Round(ctx, userID, sc.RoundID, false)
	if err != nil {
		return nil, marketingPublicError(err)
	}
	if sc.Status == model.MarketingScriptRewriting {
		return nil, errcode.ErrMarketingScriptBusy
	}
	if r.Status == model.MarketingRoundScripting || r.Status == model.MarketingRoundCanceled {
		return nil, errcode.ErrMarketingConflict
	}
	p, err := tx.Project(ctx, userID, sc.ProjectID, false)
	if err != nil {
		return nil, marketingPublicError(err)
	}
	st, err := settingsOf(r)
	if err != nil {
		return nil, err
	}
	return &scriptCtx{sc: sc, round: r, project: p, st: st}, nil
}

// snapshotScript 在短事务中取得一致输入，模型调用在事务结束后进行。
func (s *MarketingService) snapshotScript(ctx context.Context, userID, id uint64) (*scriptCtx, error) {
	return s.snapshotScriptChecked(ctx, userID, id, false)
}

// snapshotRenderScript 在短事务中确认脚本既未重写也无在途出片，避免无效的模型转写。
func (s *MarketingService) snapshotRenderScript(ctx context.Context, userID, id uint64) (*scriptCtx, error) {
	return s.snapshotScriptChecked(ctx, userID, id, true)
}

func (s *MarketingService) snapshotScriptChecked(ctx context.Context, userID, id uint64, requireIdle bool) (*scriptCtx, error) {
	probe, err := s.repo.Script(ctx, userID, id, false)
	if err != nil {
		return nil, marketingPublicError(err)
	}
	var c *scriptCtx
	err = s.repo.WithTx(ctx, func(tx repository.MarketingTx) error {
		locked, e := lockedScriptContext(ctx, tx, userID, probe)
		if e != nil {
			return e
		}
		if requireIdle {
			if e := s.scriptIdle(ctx, tx, locked.sc); e != nil {
				return e
			}
		}
		c = locked
		return nil
	})
	return c, err
}

// marketingContentSignature 保护原稿、旁白、父项目、执行代与实际参考顺序；转写稿独立版本允许自动请求采用赢家。
func marketingContentSignature(c *scriptCtx) (string, error) {
	return jsonhash.CanonicalSHA256(struct {
		ScriptID                                   uint64
		Version                                    int64
		Title, Hook, Music, Voice                  string
		Shots, PromptEdits, Product, ProductAssets []byte
		ProjectVersion, ExecutionGen               int64
		Settings                                   []byte
		Model, Reference                           *uint64
		Reverse, FollowBasis                       []byte
	}{c.sc.ID, c.sc.Revision, c.sc.Title, c.sc.Hook, c.sc.Music, c.sc.Voice, c.sc.Shots, c.sc.PromptEdits,
		c.project.Product, c.project.ProductAssetIDs, c.project.Revision, c.round.ExecutionGen, c.round.Settings,
		c.round.ModelAssetID, c.round.RefVideoAssetID, c.round.Reverse, c.round.FollowBasis})
}

// sameMarketingContent 对在途请求重新验证全部可变输入，版本相同仍不能代替素材和设置校验。
func sameMarketingContent(before, now *scriptCtx) error {
	old, err := marketingContentSignature(before)
	if err != nil {
		return err
	}
	current, err := marketingContentSignature(now)
	if err != nil {
		return err
	}
	if old != current {
		return errcode.ErrMarketingConflict
	}
	return nil
}

// requireMarketingVersion 缺少预览凭据的收费或内容写请求必须失败关闭。
func requireMarketingVersion(expected *int64) error {
	if expected == nil || *expected < 0 {
		return errcode.ErrMarketingConflict
	}
	return nil
}
