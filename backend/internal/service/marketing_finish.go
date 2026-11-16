package service

import (
	"context"
	"encoding/json"

	"video-canvas/internal/marketing"
	"video-canvas/internal/model"
	"video-canvas/internal/repository"
	"video-canvas/internal/stepkit"
)

// finishRun 在一个事务里落下环节结局：更新运行记录、写结果（事实卡 / 拆解 / 脚本）、结算积分。
// 轮次已被取消时成功的结果不写入，运行记为取消；已被别处结算的运行（状态迁移冲突）什么都不做。
func (s *MarketingService) finishRun(ctx context.Context, round *model.MarketingRound, run *model.MarketingStepRun, out *stepOutcome) error {
	charged := 0
	if out.charge && out.res != nil && out.snap != nil {
		charged = max(stepkit.Cost(out.snap, out.res), 0)
	}
	// 1. 只信持久化轮次的用户与项目双键，worker传入的快照不能授权跨用户锁。
	probe, err := s.repo.RoundByID(ctx, run.RoundID, false)
	if err != nil {
		return err
	}
	committed, staleResult := false, false
	finalStatus := out.status
	err = s.repo.WithTx(ctx, func(tx repository.MarketingTx) error {
		var txCommitted, txStale bool
		var txStatus string
		var txErr error
		txCommitted, txStale, txStatus, txErr = s.finishRunTx(ctx, tx, probe, run, out, charged)
		if txErr != nil {
			return txErr
		}
		if txCommitted {
			committed, staleResult, finalStatus = true, txStale, txStatus
		}
		return nil
	})
	if err == nil {
		out.committed, out.stale = committed, staleResult
		if committed {
			out.status = finalStatus
		}
	}
	return err
}

func (s *MarketingService) finishRunTx(ctx context.Context, tx repository.MarketingTx, probe *model.MarketingRound, run *model.MarketingStepRun, out *stepOutcome, charged int) (committed, staleResult bool, finalStatus string, err error) {
	p, cur, err := lockMarketingBackground(ctx, tx, probe)
	if err != nil {
		return false, false, "", err
	}
	owned, err := tx.Run(ctx, cur.UserID, run.ID, false)
	if err != nil {
		return false, false, "", err
	}
	if owned.RoundID != cur.ID || !ownsMarketingExecution(owned, run) {
		return false, false, "", nil
	}
	status, msg := out.status, out.errMsg
	stale := staleMarketingRun(p, cur, owned)
	if stale {
		status, msg = model.MarketingRunCanceled, ""
	}
	// 2. 先状态CAS，再结算一次；陈旧结果禁止落业务产出、清重写态或写警告。
	if err := s.closeOutcome(ctx, tx, owned, out, status, msg); err != nil {
		if errorsIsConflict(err) {
			return false, false, "", nil
		}
		return false, false, "", err
	}
	if err := s.applyCurrentMarketingOutcome(ctx, tx, cur, owned, out, status, stale); err != nil {
		return false, false, "", err
	}
	if err := s.settleRun(ctx, tx, owned, charged); err != nil {
		return false, false, "", err
	}
	return true, stale, status, nil
}

// applyCurrentMarketingOutcome 将陈旧结局与业务写入隔离；陈旧结局只允许会计结算。
func (s *MarketingService) applyCurrentMarketingOutcome(ctx context.Context, tx repository.MarketingTx, cur *model.MarketingRound, run *model.MarketingStepRun, out *stepOutcome, status string, stale bool) error {
	if stale {
		return nil
	}
	return s.applyOutcome(ctx, tx, cur, run, out, status)
}

// ownsMarketingExecution 新worker必须持匹配令牌；0035未持令牌且NULL租约的旧worker保留结算权。
func ownsMarketingExecution(cur, worker *model.MarketingStepRun) bool {
	if cur.Status != model.MarketingRunRunning {
		return false
	}
	if worker.ExecToken != "" {
		return cur.ExecToken == worker.ExecToken
	}
	return cur.ExecToken == "" && cur.LeaseUntil == nil && worker.LeaseUntil == nil
}

// closeOutcome 把环节运行的终态写回运行记录；运行已被别处结算时返回 repository.ErrStateConflict。
func (s *MarketingService) closeOutcome(ctx context.Context, tx repository.MarketingTx, run *model.MarketingStepRun, out *stepOutcome, status, msg string) error {
	method, err := json.Marshal(out.method)
	if err != nil {
		return err
	}
	fields := map[string]any{"status": status, "error": msg, "finished_at": s.now(), "method": method, "exec_token": "", "lease_until": nil}
	if out.model != "" {
		fields["text_model_id"] = out.model
	}
	if out.res != nil {
		fields["attempts"] = out.res.Attempts
	}
	return tx.UpdateRunIf(ctx, run.ID, []string{model.MarketingRunQueued, model.MarketingRunRunning}, fields)
}

// applyOutcome 成功时写结果（事实卡 / 拆解 / 脚本）和警告；跟拍没成功时把「重写中」的脚本放回草稿。
func (s *MarketingService) applyOutcome(ctx context.Context, tx repository.MarketingTx, cur *model.MarketingRound, run *model.MarketingStepRun, out *stepOutcome, status string) error {
	if status == model.MarketingRunSucceeded && out.apply != nil {
		if err := out.apply(ctx, tx, cur); err != nil {
			return err
		}
		return s.addWarnings(ctx, tx, cur, out.warnings)
	}
	if status != model.MarketingRunSucceeded && run.StepKey == marketing.StepFollow {
		return s.releaseRewriting(ctx, tx, cur.ID)
	}
	return nil
}

// addWarnings 把警告追加到轮次（去重）。
func (s *MarketingService) addWarnings(ctx context.Context, tx repository.MarketingTx, round *model.MarketingRound, add []string) error {
	if len(add) == 0 {
		return nil
	}
	var cur []string
	_ = json.Unmarshal(round.Warnings, &cur)
	for _, w := range add {
		found := false
		for _, c := range cur {
			found = found || c == w
		}
		if !found {
			cur = append(cur, w)
		}
	}
	raw, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	return tx.UpdateRound(ctx, round.ID, map[string]any{"warnings": raw})
}

// finalizeRound 在所有环节都结束后定轮次终态：有脚本 ready，否则 failed 并带第一个失败环节的原因。
// 还有排队中或进行中的运行（并发重试新建的）时返回 false，调用方继续执行。
func (s *MarketingService) finalizeRound(ctx context.Context, roundID uint64) (bool, error) {
	probe, err := s.repo.RoundByID(ctx, roundID, false)
	if err != nil {
		return false, err
	}
	final := false
	err = s.repo.WithTx(ctx, func(tx repository.MarketingTx) error {
		p, r, err := lockMarketingBackground(ctx, tx, probe)
		if err != nil {
			return err
		}
		if p.DeletedAt.Valid || r.Status != model.MarketingRoundScripting {
			final = true
			return nil
		}
		runs, err := tx.RoundRuns(ctx, roundID)
		if err != nil {
			return err
		}
		latest := latestRuns(runs)
		// 旧代仍在结算时不能阻止当前代收口，也不能将其警告/失败带入新代。
		if marketingGenerationPending(latest, r.ExecutionGen) {
			return nil
		}
		scripts, err := tx.RoundScripts(ctx, roundID)
		if err != nil {
			return err
		}
		final = true
		fields := map[string]any{"status": model.MarketingRoundReady, "error": ""}
		if len(scripts) == 0 {
			fields["status"], fields["error"] = model.MarketingRoundFailed, roundFailure(latest)
		}
		return tx.UpdateRound(ctx, roundID, fields)
	})
	if err != nil {
		return false, err
	}
	if final {
		if r, err := s.repo.RoundByID(ctx, roundID, false); err == nil {
			s.pushRound(ctx, r.UserID, roundID)
		}
	}
	return final, nil
}

// marketingGenerationPending 仅当前代未取消的在途运行阻止轮次定态，旧代只保留结算权。
func marketingGenerationPending(runs []model.MarketingStepRun, gen int64) bool {
	for _, run := range runs {
		if run.RoundGen == gen && !run.CancelRequested && (run.Status == model.MarketingRunQueued || run.Status == model.MarketingRunRunning) {
			return true
		}
	}
	return false
}
