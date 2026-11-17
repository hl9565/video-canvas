package repository

import (
	"context"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/idcodec"
)

// DramaFrameRunTx 将首帧流程与候选关联原语绑定到短剧业务事务。
type DramaFrameRunTx interface {
	// FrameRunsBySegment 按用户及片段读取全部历史流程，按 ID 升序。
	FrameRunsBySegment(context.Context, uint64, uint64) ([]model.DramaSegmentFrameRun, error)
	// ActiveFrameRunsBySegments 只读取指定用户片段的媒体进行态。
	ActiveFrameRunsBySegments(context.Context, uint64, []uint64) ([]model.DramaSegmentFrameRun, error)
	// FrameRun 按用户和 ID 查询，可在父资源锁之后锁定流程；缺失返回 ErrNotFound。
	FrameRun(context.Context, uint64, uint64, bool) (*model.DramaSegmentFrameRun, error)
	// ActiveFrameRun 按用户和片段查询唯一活跃流程，可加锁；缺失返回 ErrNotFound。
	ActiveFrameRun(context.Context, uint64, uint64, bool) (*model.DramaSegmentFrameRun, error)
	// FrameRunByRequestKey 按用户恢复幂等请求；缺失返回 ErrNotFound。
	FrameRunByRequestKey(context.Context, uint64, string) (*model.DramaSegmentFrameRun, error)
	// FrameRunByFrameTask 按用户及首帧任务查流程；缺失返回 ErrNotFound。
	FrameRunByFrameTask(context.Context, uint64, uint64) (*model.DramaSegmentFrameRun, error)
	// FrameRunByVideoTask 按用户及视频任务查流程；缺失返回 ErrNotFound。
	FrameRunByVideoTask(context.Context, uint64, uint64) (*model.DramaSegmentFrameRun, error)
	// FrameRunByVideoIdempotencyKey 按用户恢复提交中的视频流程；缺失返回 ErrNotFound。
	FrameRunByVideoIdempotencyKey(context.Context, uint64, string) (*model.DramaSegmentFrameRun, error)
	// CreateFrameRun 插入流程；请求键、活跃槽位或视频任务冲突返回 ErrDuplicate。
	CreateFrameRun(context.Context, *model.DramaSegmentFrameRun) error
	// SaveFrameRun 按用户、片段、ID、revision 保存可变列；旧版本返回 ErrStateConflict，唯一冲突返回 ErrDuplicate。
	SaveFrameRun(context.Context, *model.DramaSegmentFrameRun, int64) error
	// CancelSegmentFrameRuns 取消指定用户片段下的活跃流程并递增 revision。
	CancelSegmentFrameRuns(context.Context, uint64, uint64) error
	// CancelEpisodeFrameRuns 取消指定用户分集下（包括已软删片段）的活跃流程。
	CancelEpisodeFrameRuns(context.Context, uint64, uint64) error
	// CancelProjectFrameRuns 取消指定用户项目下（包括已软删父资源）的活跃流程。
	CancelProjectFrameRuns(context.Context, uint64, uint64) error
	// ListRecoverableFrameRuns 有界扫描 frame_pending/submitting/video_pending，支持用户及更新时间过滤。
	ListRecoverableFrameRuns(context.Context, FrameRunRecoveryFilter, int) ([]model.DramaSegmentFrameRun, error)
	// ListTakeLinks 按用户和片段列出候选，按 task_id 排序。
	ListTakeLinks(context.Context, uint64, uint64) ([]model.DramaSegmentTakeLink, error)
	// TakeLink 按用户和复合主键读取候选；缺失返回 ErrNotFound。
	TakeLink(context.Context, uint64, uint64, uint64) (*model.DramaSegmentTakeLink, error)
	// TakeLinkByRun 按用户和 run_id 读取候选；缺失返回 ErrNotFound。
	TakeLinkByRun(context.Context, uint64, uint64) (*model.DramaSegmentTakeLink, error)
	// CreateTakeLinkIfAbsent 仅补缺复合主键，保留已有 origin/qc；其他唯一冲突返回 ErrDuplicate。
	CreateTakeLinkIfAbsent(context.Context, *model.DramaSegmentTakeLink) (bool, error)
	// UpdateTakeLinkQC 按用户和复合主键只更新质检结果；缺失返回 ErrNotFound。
	UpdateTakeLinkQC(context.Context, uint64, uint64, uint64, model.DramaSegmentTakeQC) error
}

// FrameRunRecoveryFilter 限定后台恢复扫描；UserID=0 仅供跨用户后台扫描，After* 支持游标分页推进。
type FrameRunRecoveryFilter struct {
	UserID         uint64
	UpdatedBefore  time.Time
	AfterUpdatedAt time.Time
	AfterID        uint64
}

func activeFrameRunStates() []model.DramaFrameRunState {
	return []model.DramaFrameRunState{model.FrameRunStateDraft, model.FrameRunStateFramePending,
		model.FrameRunStateFrameReady, model.FrameRunStateConfirmed, model.FrameRunStateSubmitting, model.FrameRunStateVideoPending}
}

// FrameRun 按用户与 ID 查询，可加行锁；缺失或他人流程返回 ErrNotFound。
func (r *DramaRepository) FrameRun(ctx context.Context, userID, id uint64, lock bool) (*model.DramaSegmentFrameRun, error) {
	return readFrameRun(r.db.WithContext(ctx).Where("user_id = ? AND id = ?", userID, id), lock)
}

// ActiveFrameRun 按用户与片段查询唯一活跃流程，可加行锁；缺失返回 ErrNotFound。
func (r *DramaRepository) ActiveFrameRun(ctx context.Context, userID, segmentID uint64, lock bool) (*model.DramaSegmentFrameRun, error) {
	return readFrameRun(r.db.WithContext(ctx).Where("user_id = ? AND segment_id = ? AND state IN ?", userID, segmentID, activeFrameRunStates()), lock)
}

// FrameRunByRequestKey 按用户与非空请求键查询；缺失返回 ErrNotFound。
func (r *DramaRepository) FrameRunByRequestKey(ctx context.Context, userID uint64, key string) (*model.DramaSegmentFrameRun, error) {
	if key == "" {
		return nil, ErrNotFound
	}
	return readFrameRun(r.db.WithContext(ctx).Where("user_id = ? AND request_key = ?", userID, key), false)
}

// FrameRunByFrameTask 按用户与非零首帧任务查询，多个引用时按 ID 取最早流程；缺失返回 ErrNotFound。
func (r *DramaRepository) FrameRunByFrameTask(ctx context.Context, userID, taskID uint64) (*model.DramaSegmentFrameRun, error) {
	if taskID == 0 {
		return nil, ErrNotFound
	}
	return readFrameRun(r.db.WithContext(ctx).Where("user_id = ? AND frame_task_id = ?", userID, taskID), false)
}

// FrameRunByVideoTask 按用户与非零视频任务查询；缺失返回 ErrNotFound。
func (r *DramaRepository) FrameRunByVideoTask(ctx context.Context, userID, taskID uint64) (*model.DramaSegmentFrameRun, error) {
	if taskID == 0 {
		return nil, ErrNotFound
	}
	return readFrameRun(r.db.WithContext(ctx).Where("user_id = ? AND video_task_id = ?", userID, taskID), false)
}

// FrameRunByVideoIdempotencyKey 按用户与非空视频幂等键查询；缺失返回 ErrNotFound。
func (r *DramaRepository) FrameRunByVideoIdempotencyKey(ctx context.Context, userID uint64, key string) (*model.DramaSegmentFrameRun, error) {
	if key == "" {
		return nil, ErrNotFound
	}
	return readFrameRun(r.db.WithContext(ctx).Where("user_id = ? AND video_idempotency_key = ?", userID, key), false)
}

func readFrameRun(q *gorm.DB, lock bool) (*model.DramaSegmentFrameRun, error) {
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var row model.DramaSegmentFrameRun
	err := q.First(&row).Error
	return &row, translate(err)
}

// CreateFrameRun 创建流程，唯一索引冲突返回 ErrDuplicate。
func (r *DramaRepository) CreateFrameRun(ctx context.Context, row *model.DramaSegmentFrameRun) error {
	return frameRunWriteError(r.db.WithContext(ctx).Create(row).Error)
}

// SaveFrameRun 按用户、片段及版本 CAS 保存可变列，未命中返回 ErrStateConflict，唯一冲突返回 ErrDuplicate。
func (r *DramaRepository) SaveFrameRun(ctx context.Context, row *model.DramaSegmentFrameRun, revision int64) error {
	now := time.Now()
	res := r.db.WithContext(ctx).Model(&model.DramaSegmentFrameRun{}).
		Where("user_id = ? AND segment_id = ? AND id = ? AND revision = ?", row.UserID, row.SegmentID, row.ID, revision).
		Updates(map[string]any{
			"frame_source": row.FrameSource, "state": row.State, "segment_revision": row.SegmentRevision,
			"confirmed_version": row.ConfirmedVersion, "video_idempotency_key": row.VideoIdempotencyKey,
			"model_key": row.ModelKey, "origin_canvas_id": row.OriginCanvasID,
			"frame_task_id": row.FrameTaskID, "frame_asset_id": row.FrameAssetID, "video_task_id": row.VideoTaskID,
			"adopt_if_empty": row.AdoptIfEmpty, "confirmed_input": segmentAdaptations(row.ConfirmedInput),
			"confirmed_input_hash": row.ConfirmedInputHash, "quote_token": row.QuoteToken, "submitting_at": row.SubmittingAt,
			"error_code": row.ErrorCode, "error_message": row.ErrorMessage,
			"revision": gorm.Expr("revision + 1"), "updated_at": now,
		})
	if res.Error != nil {
		return frameRunWriteError(res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrStateConflict
	}
	row.Revision, row.UpdatedAt = revision+1, now
	return nil
}

func frameRunWriteError(err error) error {
	if isUniqueViolation(err) {
		return ErrDuplicate
	}
	return err
}

// CancelSegmentFrameRuns 按用户和片段取消活跃流程；不存在也成功，不修改终态流程。
func (r *DramaRepository) CancelSegmentFrameRuns(ctx context.Context, userID, segmentID uint64) error {
	return cancelFrameRuns(r.db.WithContext(ctx).Where("user_id = ? AND segment_id = ?", userID, segmentID))
}

// CancelEpisodeFrameRuns 按用户和分集取消活跃流程，包含已软删片段；不存在也成功。
func (r *DramaRepository) CancelEpisodeFrameRuns(ctx context.Context, userID, episodeID uint64) error {
	segments := r.db.WithContext(ctx).Unscoped().Model(&model.DramaSegment{}).Select("id").Where("user_id = ? AND episode_id = ?", userID, episodeID)
	return cancelFrameRuns(r.db.WithContext(ctx).Where("user_id = ? AND segment_id IN (?)", userID, segments))
}

// CancelProjectFrameRuns 按用户和项目取消活跃流程，包含已软删的集与片段；无分集时不查询子表。
func (r *DramaRepository) CancelProjectFrameRuns(ctx context.Context, userID, dramaID uint64) error {
	var episodes []uint64
	if err := r.db.WithContext(ctx).Unscoped().Model(&model.DramaEpisode{}).
		Where("user_id = ? AND drama_id = ?", userID, dramaID).Pluck("id", &episodes).Error; err != nil {
		return err
	}
	if len(episodes) == 0 {
		return nil
	}
	segments := r.db.WithContext(ctx).Unscoped().Model(&model.DramaSegment{}).Select("id").Where("user_id = ? AND episode_id IN ?", userID, episodes)
	return cancelFrameRuns(r.db.WithContext(ctx).Where("user_id = ? AND segment_id IN (?)", userID, segments))
}

func cancelFrameRuns(q *gorm.DB) error {
	return q.Model(&model.DramaSegmentFrameRun{}).Where("state IN ?", activeFrameRunStates()).
		Updates(map[string]any{"state": model.FrameRunStateCancelled, "revision": gorm.Expr("revision + 1"), "updated_at": gorm.Expr("now()")}).Error
}

// ListRecoverableFrameRuns 有界扫描进行中流程及生成首帧 draft；支持游标分页，limit 默认 50、最多 500。
func (r *DramaRepository) ListRecoverableFrameRuns(ctx context.Context, filter FrameRunRecoveryFilter, limit int) ([]model.DramaSegmentFrameRun, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	limit = min(limit, 500)
	q := r.db.WithContext(ctx).Where(
		"(state IN ? OR (frame_source = ? AND state = ?))",
		[]model.DramaFrameRunState{model.FrameRunStateFramePending, model.FrameRunStateSubmitting, model.FrameRunStateVideoPending},
		model.FrameSourceGenerate,
		model.FrameRunStateDraft,
	)
	if filter.UserID != 0 {
		q = q.Where("user_id = ?", filter.UserID)
	}
	if !filter.UpdatedBefore.IsZero() {
		q = q.Where("updated_at <= ?", filter.UpdatedBefore)
	}
	if !filter.AfterUpdatedAt.IsZero() && filter.AfterID > 0 {
		q = q.Where("(updated_at > ? OR (updated_at = ? AND id > ?))", filter.AfterUpdatedAt, filter.AfterUpdatedAt, filter.AfterID)
	}
	rows := []model.DramaSegmentFrameRun{}
	err := q.Order("updated_at ASC, id ASC").Limit(limit).Find(&rows).Error
	return rows, err
}

// HasActiveFrameRunForTask 判断普通任务是否仍被非终态首帧流程持有，避免直接取消绕过流程状态机。
func (r *DramaRepository) HasActiveFrameRunForTask(ctx context.Context, userID uint64, task *model.GenerationTask) (bool, error) {
	if task == nil || task.ID == 0 || userID == 0 {
		return false, nil
	}
	conds := r.db.WithContext(ctx).Where("frame_task_id = ? OR video_task_id = ?", task.ID, task.ID)
	if task.FrameRunID != nil {
		conds = conds.Or("id = ?", *task.FrameRunID)
	}
	if task.Kind == model.KindImage && strings.HasPrefix(task.NodeID, "seg-frame:") {
		if sid, ok := idcodec.Decode(strings.TrimPrefix(task.NodeID, "seg-frame:")); ok {
			conds = conds.Or("(segment_id = ? AND frame_source = ? AND state = ? AND frame_task_id IS NULL)", sid, model.FrameSourceGenerate, model.FrameRunStateDraft)
		}
	}
	var count int64
	err := r.db.WithContext(ctx).Model(&model.DramaSegmentFrameRun{}).
		Where("user_id = ? AND state IN ?", userID, activeFrameRunStates()).
		Where(conds).
		Limit(1).
		Count(&count).Error
	return count > 0, err
}

// ListTakeLinks 按用户和片段列出候选，按 task_id 升序；没有候选返回空列表。
func (r *DramaRepository) ListTakeLinks(ctx context.Context, userID, segmentID uint64) ([]model.DramaSegmentTakeLink, error) {
	rows := []model.DramaSegmentTakeLink{}
	err := r.db.WithContext(ctx).Where("user_id = ? AND segment_id = ?", userID, segmentID).Order("task_id ASC").Find(&rows).Error
	return rows, err
}

// TakeLink 按用户和复合主键读取候选，缺失或他人候选返回 ErrNotFound。
func (r *DramaRepository) TakeLink(ctx context.Context, userID, segmentID, taskID uint64) (*model.DramaSegmentTakeLink, error) {
	var row model.DramaSegmentTakeLink
	err := r.db.WithContext(ctx).Where("user_id = ? AND segment_id = ? AND task_id = ?", userID, segmentID, taskID).First(&row).Error
	return &row, translate(err)
}

// TakeLinkByRun 按用户和非零 run_id 读取候选；缺失返回 ErrNotFound。
func (r *DramaRepository) TakeLinkByRun(ctx context.Context, userID, runID uint64) (*model.DramaSegmentTakeLink, error) {
	if runID == 0 {
		return nil, ErrNotFound
	}
	var row model.DramaSegmentTakeLink
	err := r.db.WithContext(ctx).Where("user_id = ? AND run_id = ?", userID, runID).First(&row).Error
	return &row, translate(err)
}

// CreateTakeLinkIfAbsent 仅复合主键冲突时忽略插入，不覆盖 origin/qc；run_id 唯一冲突返回 ErrDuplicate。
func (r *DramaRepository) CreateTakeLinkIfAbsent(ctx context.Context, row *model.DramaSegmentTakeLink) (bool, error) {
	res := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "segment_id"}, {Name: "task_id"}}, DoNothing: true,
	}).Create(row)
	return res.RowsAffected > 0, frameRunWriteError(res.Error)
}

// UpdateTakeLinkQC 按用户和复合主键只更新 QC；缺失返回 ErrNotFound，非法值由数据库 CHECK 拒绝。
func (r *DramaRepository) UpdateTakeLinkQC(ctx context.Context, userID, segmentID, taskID uint64, qc model.DramaSegmentTakeQC) error {
	res := r.db.WithContext(ctx).Model(&model.DramaSegmentTakeLink{}).
		Where("user_id = ? AND segment_id = ? AND task_id = ?", userID, segmentID, taskID).Update("qc", qc)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// withDramaWriteTx 让独立调用也原子提交多表删除；已在业务事务中时直接复用，绝不建立嵌套保存点。
func (r *DramaRepository) withDramaWriteTx(ctx context.Context, fn func(*DramaRepository) error) error {
	if _, ok := r.db.Statement.ConnPool.(gorm.TxCommitter); ok {
		return fn(r)
	}
	return r.WithTx(ctx, func(tx DramaTx) error { return fn(tx.(*DramaRepository)) })
}

// TasksByIDs 按用户与 ID 集合读取正式任务，空集合不查询，排除试跑。
func (r *DramaRepository) TasksByIDs(ctx context.Context, userID uint64, ids []uint64) ([]model.GenerationTask, error) {
	rows := []model.GenerationTask{}
	if len(ids) == 0 {
		return rows, nil
	}
	err := r.db.WithContext(ctx).Where("user_id = ? AND is_test = false AND id IN ?", userID, ids).Order("id ASC").Find(&rows).Error
	return rows, err
}

// FrameRunsBySegment 按用户和片段读取当前及历史流程，按 ID 升序，无行返回空切片。
func (r *DramaRepository) FrameRunsBySegment(ctx context.Context, userID, segmentID uint64) ([]model.DramaSegmentFrameRun, error) {
	rows := []model.DramaSegmentFrameRun{}
	err := r.db.WithContext(ctx).Where("user_id = ? AND segment_id = ?", userID, segmentID).Order("id ASC").Find(&rows).Error
	return rows, err
}

// ActiveFrameRunsBySegments 按用户和片段集合读取三种媒体进行态，空集合不查询。
func (r *DramaRepository) ActiveFrameRunsBySegments(ctx context.Context, userID uint64, ids []uint64) ([]model.DramaSegmentFrameRun, error) {
	rows := []model.DramaSegmentFrameRun{}
	if len(ids) == 0 {
		return rows, nil
	}
	err := r.db.WithContext(ctx).Where("user_id = ? AND segment_id IN ? AND state IN ?", userID, ids,
		[]model.DramaFrameRunState{model.FrameRunStateFramePending, model.FrameRunStateSubmitting, model.FrameRunStateVideoPending}).Order("id ASC").Find(&rows).Error
	return rows, err
}

// TakeLinksByTask 按用户和任务反查候选关联，供跨画布回调定位片段，不依赖源画布存活。
func (r *DramaRepository) TakeLinksByTask(ctx context.Context, userID, taskID uint64) ([]model.DramaSegmentTakeLink, error) {
	rows := []model.DramaSegmentTakeLink{}
	err := r.db.WithContext(ctx).Where("user_id = ? AND task_id = ?", userID, taskID).Order("segment_id ASC").Find(&rows).Error
	return rows, err
}
