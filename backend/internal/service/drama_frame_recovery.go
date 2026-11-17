package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.uber.org/zap"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/logger"
	"video-canvas/internal/repository"
)

const dramaFrameRunItemTimeout = 5 * time.Second

// OnTaskChanged 观察持久化图片及失败/取消/超时视频；成功视频仍由片段服务统一准入和采用。
func (s *DramaFrameRunService) OnTaskChanged(ctx context.Context, task *model.GenerationTask) error {
	// 1. 非正式任务和无画布任务不能影响短剧状态。
	if task == nil || task.IsTest || task.CanvasProjectID == nil {
		return nil
	}
	var run *model.DramaSegmentFrameRun
	var err error
	switch {
	case task.Kind == model.KindImage && strings.HasPrefix(task.NodeID, "seg-frame:"):
		run, err = s.repo.FrameRunByFrameTask(ctx, task.UserID, task.ID)
	case task.Kind == model.KindVideo && task.FrameRunID != nil &&
		(task.Status == model.TaskFailed || task.Status == model.TaskCanceled || task.Status == model.TaskExpired):
		run, err = s.repo.FrameRun(ctx, task.UserID, *task.FrameRunID, false)
	default:
		return nil
	}
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	// 2. 回调只用于唤醒，实际状态重新读库；父链软删后的迟到通知安静忽略。
	err = s.reconcileFrameRun(ctx, run)
	if isSegmentMissing(err) {
		return nil
	}
	return err
}

// RecoverUnsettledDramaFrameRuns 有界恢复图片、断线提交和视频状态；单项故障不阻断后续用户。
func (s *DramaFrameRunService) RecoverUnsettledDramaFrameRuns(ctx context.Context, filter repository.FrameRunRecoveryFilter, limit int) error {
	_, _, err := s.RecoverUnsettledDramaFrameRunsPage(ctx, filter, limit)
	return err
}

// RecoverUnsettledDramaFrameRunsPage 按游标执行单页恢复，返回下页游标和是否仍有后续记录，避免前缀行长期占满配额。
func (s *DramaFrameRunService) RecoverUnsettledDramaFrameRunsPage(ctx context.Context, filter repository.FrameRunRecoveryFilter, limit int) (repository.FrameRunRecoveryFilter, bool, error) {
	runs, err := s.repo.ListRecoverableFrameRuns(ctx, filter, limit)
	if err != nil {
		return filter, false, err
	}
	next := filter
	var errs []error
	for i := range runs {
		next.AfterUpdatedAt, next.AfterID = runs[i].UpdatedAt, runs[i].ID
		itemCtx, cancel := context.WithTimeout(ctx, dramaFrameRunItemTimeout)
		recErr := s.reconcileFrameRun(itemCtx, &runs[i])
		cancel()
		if recErr != nil && !isSegmentMissing(recErr) {
			errs = append(errs, recErr)
		}
		if ctx.Err() != nil {
			return next, true, ctx.Err()
		}
	}
	hasMore := limit > 0 && len(runs) == limit
	return next, hasMore, errors.Join(errs...)
}

// reconcileFrameRun 只恢复媒体进行态与中断的生成首帧 draft，终态永不逆转。
func (s *DramaFrameRunService) reconcileFrameRun(ctx context.Context, run *model.DramaSegmentFrameRun) error {
	p, _, _, err := s.segments.segmentSource(ctx, run.UserID, run.SegmentID)
	if err != nil {
		return err
	}
	switch run.State {
	case model.FrameRunStateDraft:
		if run.FrameSource == model.FrameSourceGenerate {
			return s.recoverGeneratedDraft(ctx, run, p.CanvasID)
		}
		return nil
	case model.FrameRunStateFramePending:
		return s.syncFrameImage(ctx, run, p.CanvasID)
	case model.FrameRunStateSubmitting:
		return s.recoverFrameSubmitting(ctx, run)
	case model.FrameRunStateVideoPending:
		return s.syncFrameVideo(ctx, run)
	default:
		return nil
	}
}

// recoverGeneratedDraft 修复 Create 期间建图已提交但未绑定 frame_task_id 的崩溃窗口，或在超时后释放活跃槽位。
func (s *DramaFrameRunService) recoverGeneratedDraft(ctx context.Context, run *model.DramaSegmentFrameRun, canvasID uint64) error {
	task, err := s.tasks.FindByIdempotencyKey(ctx, run.UserID, frameImageKey(run.ID))
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return err
	}
	if task != nil && task.UserID == run.UserID && !task.IsTest && task.Kind == model.KindImage &&
		task.CanvasProjectID != nil && *task.CanvasProjectID == canvasID && task.NodeID == frameImageNode(run.SegmentID) {
		if err := s.commitFrameObservation(ctx, run, func(row *model.DramaSegmentFrameRun) {
			id := task.ID
			row.FrameTaskID, row.State = &id, model.FrameRunStateFramePending
		}); err != nil {
			return err
		}
		current, err := s.loadFrameRun(ctx, run.UserID, run.SegmentID, run.ID)
		if err != nil || current.State != model.FrameRunStateFramePending {
			return err
		}
		return s.syncFrameImage(ctx, current, canvasID)
	}
	if s.now().Sub(run.UpdatedAt) < s.staleTimeout {
		return nil
	}
	return s.failFrameObservation(ctx, run, "frame_create_timeout", "首帧生成初始化超时，请重试")
}

// frameRunTask 从正式任务表读取用户证据，不相信通知载荷或客户端任务 ID。
func (s *DramaFrameRunService) frameRunTask(ctx context.Context, userID uint64, id *uint64) (*model.GenerationTask, error) {
	if id == nil {
		return nil, nil
	}
	rows, err := s.repo.TasksByIDs(ctx, userID, []uint64{*id})
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || rows[0].ID != *id || rows[0].UserID != userID || rows[0].IsTest {
		return nil, nil
	}
	return &rows[0], nil
}

// syncFrameImage 下载和裁图在事务外执行，CAS 保证换图、取消或另一次恢复胜出后不覆盖。
func (s *DramaFrameRunService) syncFrameImage(ctx context.Context, run *model.DramaSegmentFrameRun, canvasID uint64) error {
	task, err := s.frameRunTask(ctx, run.UserID, run.FrameTaskID)
	if err != nil {
		return err
	}
	if task == nil || task.Kind != model.KindImage || task.CanvasProjectID == nil || *task.CanvasProjectID != canvasID || task.NodeID != frameImageNode(run.SegmentID) {
		return s.failFrameObservation(ctx, run, "frame_task_missing", "首帧生成任务不存在或归属不匹配")
	}
	if task.Status == model.TaskFailed || task.Status == model.TaskCanceled || task.Status == model.TaskExpired {
		code, msg := task.ErrorCode, task.ErrorMessage
		if task.Status == model.TaskExpired && msg == "" {
			code, msg = "task_expired", "首帧生成已超时，请重试"
		}
		return s.failFrameObservation(ctx, run, code, msg)
	}
	if task.Status != model.TaskSucceeded {
		return nil
	}
	id, err := segmentOutputAsset(task, model.KindImage)
	if err != nil || id == 0 {
		return s.failFrameObservation(ctx, run, "frame_invalid", errcode.ErrDramaFrameInvalid.Msg)
	}
	asset, err := s.prepareFrameImage(ctx, run.UserID, id)
	if isFrameInvalid(err) {
		return s.failFrameObservation(ctx, run, "frame_invalid", errcode.ErrDramaFrameInvalid.Msg)
	}
	if err != nil {
		return err
	}
	return s.commitFrameObservation(ctx, run, func(row *model.DramaSegmentFrameRun) {
		row.FrameAssetID, row.State = &asset.ID, model.FrameRunStateFrameReady
		row.ErrorCode, row.ErrorMessage = "", ""
	})
}

// commitFrameObservation 将锁外观测结果与原版本比较，重复回调不产生额外版本或事件。
func (s *DramaFrameRunService) commitFrameObservation(ctx context.Context, run *model.DramaSegmentFrameRun, mutate func(*model.DramaSegmentFrameRun)) error {
	updated, err := s.updateFrameRun(ctx, run.UserID, run.SegmentID, run.ID, func(row *model.DramaSegmentFrameRun) error {
		if row.State != run.State || row.Revision != run.Revision {
			return errFrameRunUnchanged
		}
		mutate(row)
		return nil
	})
	if err != nil || updated.Revision == run.Revision {
		return err
	}
	return s.publishFrameRun(ctx, updated)
}

// failFrameObservation 仅终结仍处于所观测版本的进行态，持久化公开的任务错误。
func (s *DramaFrameRunService) failFrameObservation(ctx context.Context, run *model.DramaSegmentFrameRun, code, message string) error {
	if code == "" {
		code = "task_failed"
	}
	if message == "" {
		message = "生成失败或已取消，请重试"
	}
	return s.commitFrameObservation(ctx, run, func(row *model.DramaSegmentFrameRun) {
		row.State, row.SubmittingAt = model.FrameRunStateFailed, nil
		row.ErrorCode, row.ErrorMessage = code, message
	})
}

// findFrameVideo 只接受匹配流程及确认版本的正式视频，普通占键任务不能被恢复或取消。
func (s *DramaFrameRunService) findFrameVideo(ctx context.Context, run *model.DramaSegmentFrameRun) (*model.GenerationTask, error) {
	task, err := s.frameRunTask(ctx, run.UserID, run.VideoTaskID)
	if err != nil {
		return nil, err
	}
	if task == nil && run.VideoIdempotencyKey != "" {
		task, err = s.tasks.FindByIdempotencyKey(ctx, run.UserID, run.VideoIdempotencyKey)
		if err != nil && !errors.Is(err, repository.ErrNotFound) {
			return nil, err
		}
	}
	if task == nil || task.UserID != run.UserID || task.IsTest || task.Kind != model.KindVideo ||
		!matchesDramaRunBind(task, &DramaFrameRunBind{RunID: run.ID, ConfirmRev: run.ConfirmedVersion}) {
		return nil, nil
	}
	return task, nil
}

// recoverFrameSubmitting 有任务时修复正向键；无任务且超时才释放提交态，用户必须重新确认报价。
func (s *DramaFrameRunService) recoverFrameSubmitting(ctx context.Context, run *model.DramaSegmentFrameRun) error {
	task, err := s.findFrameVideo(ctx, run)
	if err != nil {
		return err
	}
	if task == nil {
		at := run.UpdatedAt
		if run.SubmittingAt != nil {
			at = *run.SubmittingAt
		}
		if s.now().Sub(at) < s.staleTimeout {
			return nil
		}
		return s.commitFrameObservation(ctx, run, func(row *model.DramaSegmentFrameRun) {
			row.State, row.SubmittingAt, row.QuoteToken = model.FrameRunStateFrameReady, nil, nil
			row.ErrorCode, row.ErrorMessage = "", "提交超时，请重新提交"
		})
	}
	if err := s.commitFrameObservation(ctx, run, func(row *model.DramaSegmentFrameRun) {
		row.State, row.VideoTaskID, row.SubmittingAt = model.FrameRunStateVideoPending, &task.ID, nil
	}); err != nil {
		return err
	}
	current, err := s.loadFrameRun(ctx, run.UserID, run.SegmentID, run.ID)
	if err != nil || current.State != model.FrameRunStateVideoPending {
		return err
	}
	return s.syncFrameVideo(ctx, current)
}

// syncFrameVideo 成功视频沿用统一候选准入，失败或取消才由首帧服务结束流程。
func (s *DramaFrameRunService) syncFrameVideo(ctx context.Context, run *model.DramaSegmentFrameRun) error {
	task, err := s.findFrameVideo(ctx, run)
	if err != nil {
		return err
	}
	if task == nil || !trustedSegmentRunTask(run.UserID, run.SegmentID, run, task) {
		return s.failFrameObservation(ctx, run, "video_task_missing", "视频任务不存在或归属不匹配")
	}
	switch task.Status {
	case model.TaskFailed, model.TaskCanceled, model.TaskExpired:
		return s.failVideoRun(ctx, run, task)
	case model.TaskSucceeded:
		_, err := s.segments.ReconcileSegmentAndFrameRuns(ctx, run.UserID, run.SegmentID)
		return err
	default:
		return nil
	}
}

// failVideoRun 失败或超时只作用于双向归属一致的 video_pending，不处理已取消和已绑定流程。
func (s *DramaFrameRunService) failVideoRun(ctx context.Context, run *model.DramaSegmentFrameRun, task *model.GenerationTask) error {
	if run.State != model.FrameRunStateVideoPending || !trustedSegmentRunTask(run.UserID, run.SegmentID, run, task) {
		return nil
	}
	code, msg := task.ErrorCode, task.ErrorMessage
	if task.Status == model.TaskExpired && msg == "" {
		code, msg = "task_expired", "视频生成已超时，请重试"
	}
	return s.failFrameObservation(ctx, run, code, msg)
}

// cancelFrameTask 取消失败只记录，终态或 finalizing 的业务拒绝不能回滚已提交的流程取消。
func (s *DramaFrameRunService) cancelFrameTask(ctx context.Context, userID, taskID uint64) {
	_, err := s.tasks.Cancel(ctx, userID, taskID)
	var business *errcode.Error
	if err != nil && !errors.As(err, &business) {
		logger.Ctx(ctx).Warn("首帧流程取消关联任务失败", logger.GenRun(taskID), zap.Error(err))
	}
}

// cancelFrameRunTasks 在无父链锁时查证任务归属；同键普通任务绝不被触碰。
func (s *DramaFrameRunService) cancelFrameRunTasks(ctx context.Context, run *model.DramaSegmentFrameRun, previous model.DramaFrameRunState) {
	if previous == model.FrameRunStateFramePending && run.FrameTaskID != nil {
		s.cancelFrameTask(ctx, run.UserID, *run.FrameTaskID)
	}
	if previous != model.FrameRunStateVideoPending && previous != model.FrameRunStateSubmitting {
		return
	}
	task, err := s.findFrameVideo(ctx, run)
	if err != nil {
		logger.Ctx(ctx).Warn("首帧流程读取待取消任务失败", logger.RunField("drama_frame", idcodec.Encode(run.ID)), zap.Error(err))
		return
	}
	if task != nil {
		s.cancelFrameTask(ctx, run.UserID, task.ID)
	}
}
