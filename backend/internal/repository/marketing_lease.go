package repository

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"video-canvas/internal/model"
)

// ClaimStepRun 原子从 queued 抢占为 running 并绑定 exec_token 与 lease_until，拒绝陈旧代和已请求取消的运行。
func (r *MarketingRepository) ClaimStepRun(
	ctx context.Context, runID, roundID uint64, expectedGen int64, execToken string, leaseUntil time.Time,
) (bool, error) {
	if execToken == "" {
		return false, errors.New("ClaimStepRun exec_token 不能为空")
	}
	now := time.Now()
	res := r.db.WithContext(ctx).Model(&model.MarketingStepRun{}).
		Where("id = ? AND round_id = ? AND status = ? AND cancel_requested = FALSE AND round_gen = ?",
			runID, roundID, model.MarketingRunQueued, expectedGen).
		Where("EXISTS (SELECT 1 FROM marketing_rounds mr WHERE mr.id = ? AND mr.execution_gen = ? AND mr.status = ?)",
			roundID, expectedGen, model.MarketingRoundScripting).
		Updates(map[string]any{
			"status":      model.MarketingRunRunning,
			"exec_token":  execToken,
			"lease_until": leaseUntil,
			"started_at":  now,
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// HeartbeatStepRun 仅当运行处于 running、持有匹配的 exec_token 且加行锁后按 clock_timestamp() 判定租约未过期时续约 lease_until。
func (r *MarketingRepository) HeartbeatStepRun(ctx context.Context, runID uint64, execToken string, leaseUntil time.Time) (bool, error) {
	if execToken == "" {
		return false, nil
	}
	// 先通过独立 SELECT ... FOR UPDATE 严格获取行锁再执行 UPDATE ... clock_timestamp()，
	// 防止单个 CTE/UPDATE 计划在行锁等待前预判 WHERE 条件，也避免 NOW() 冻结导致等锁跨过过期时间后复活租约。
	var ok bool
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var lockedID uint64
		if err := tx.Raw(
			`SELECT id FROM marketing_step_runs WHERE id = ? AND status = ? AND exec_token = ? FOR UPDATE`,
			runID, model.MarketingRunRunning, execToken,
		).Scan(&lockedID).Error; err != nil {
			return err
		}
		if lockedID == 0 {
			return nil
		}
		res := tx.Exec(
			`UPDATE marketing_step_runs SET lease_until = GREATEST(lease_until, ?) WHERE id = ? AND status = ? AND exec_token = ? AND lease_until IS NOT NULL AND lease_until > clock_timestamp()`,
			leaseUntil, lockedID, model.MarketingRunRunning, execToken,
		)
		if res.Error != nil {
			return res.Error
		}
		ok = res.RowsAffected == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return ok, nil
}

// MarkRoundRunsCancelRequested 在取消轮次锁内给未终态运行持久化打上 cancel_requested 标记。
func (r *MarketingRepository) MarkRoundRunsCancelRequested(ctx context.Context, roundID uint64) error {
	return r.db.WithContext(ctx).Model(&model.MarketingStepRun{}).
		Where("round_id = ? AND status IN ?", roundID, []string{model.MarketingRunQueued, model.MarketingRunRunning}).
		Update("cancel_requested", true).Error
}

// ListUnsettledRuns 按主键升序列出尚未终态的环节运行，供崩溃租约恢复扫描使用。
func (r *MarketingRepository) ListUnsettledRuns(ctx context.Context, limit int) ([]model.MarketingStepRun, error) {
	var rows []model.MarketingStepRun
	q := r.db.WithContext(ctx).
		Where("status IN ?", []string{model.MarketingRunQueued, model.MarketingRunRunning}).
		Order("id ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	err := q.Find(&rows).Error
	return rows, err
}

// ReclaimExpiredStepRun 仅回收令牌匹配且租约过期的running行；旧NULL租约需legacyDrain确认，未命中返回false。
func (r *MarketingRepository) ReclaimExpiredStepRun(ctx context.Context, runID uint64, token string, now time.Time, legacyDrain bool) (bool, error) {
	res := r.db.WithContext(ctx).Model(&model.MarketingStepRun{}).
		Where("id = ? AND status = ? AND exec_token = ?", runID, model.MarketingRunRunning, token).
		Where("(exec_token <> '' AND lease_until IS NOT NULL AND lease_until <= ?) OR (? AND exec_token = '' AND lease_until IS NULL)", now, legacyDrain).
		Updates(map[string]any{"status": model.MarketingRunCanceled, "exec_token": "", "lease_until": nil, "finished_at": now})
	return res.RowsAffected == 1, res.Error
}
