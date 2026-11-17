package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/logger"
	"video-canvas/internal/repository"
)

// segmentNode 与对外片段ID共用编码，避免将数据库主键直接暴露给节点。
func segmentNode(sid uint64) string {
	return "seg:" + idcodec.Encode(sid)
}

// readView 读入口与回调、恢复共用锁内对账，避免绕过准入或显式清空。
func (s *DramaSegmentService) readView(ctx context.Context, userID, sid uint64) (*model.DramaSegmentView, *model.DramaProject, *model.DramaEpisode, bool, error) {
	return s.readSegmentView(ctx, userID, sid, true)
}

// readSegmentView 可选择只读快照，QC 和非采用导入的响应不能隐式递增采用版本。
func (s *DramaSegmentService) readSegmentView(ctx context.Context, userID, sid uint64, reconcile bool) (*model.DramaSegmentView, *model.DramaProject, *model.DramaEpisode, bool, error) {
	var p *model.DramaProject
	var ep *model.DramaEpisode
	var row *model.DramaSegment
	var takes []resolvedSegmentTake
	var runs []model.DramaSegmentFrameRun
	var changed bool
	err := s.repo.WithTx(ctx, func(tx repository.DramaTx) error {
		var err error
		p, ep, row, err = lockSegment(ctx, tx, userID, sid)
		if err != nil {
			return err
		}
		takes, err = ResolveSegmentTakes(ctx, tx, s.assets, p, row)
		if err != nil {
			return err
		}
		if reconcile {
			changed, err = s.reconcileSegment(ctx, tx, p, row, takes)
			if err != nil {
				return err
			}
			ep, err = tx.SegmentEpisode(ctx, userID, row.EpisodeID)
			if err != nil {
				return err
			}
		}
		runs, err = tx.FrameRunsBySegment(ctx, userID, sid)
		return err
	})
	if err != nil {
		return nil, nil, nil, false, segmentError(err)
	}
	v, err := s.segmentView(ctx, row, ep.No, ep.SegmentVersion, takes)
	if v != nil {
		v.FrameRun = latestSegmentFrameRun(runs)
		if err == nil && v.FrameRun != nil && s.frameRuns != nil {
			index := slices.IndexFunc(runs, func(run model.DramaSegmentFrameRun) bool { return run.ID == uint64(v.FrameRun.ID) })
			err = s.frameRuns.EnrichFrameRunView(ctx, userID, &runs[index], v.FrameRun)
		}
	}
	return v, p, ep, changed, err
}

// reconcileSegment 先推进可用视频的关系，再独立执行自动采用；只推进流程不改变段/集版本。
func (s *DramaSegmentService) reconcileSegment(ctx context.Context, tx repository.DramaTx, p *model.DramaProject, row *model.DramaSegment, takes []resolvedSegmentTake) (bool, error) {
	changed := false
	for i := range takes {
		promoted, err := PromoteSegmentVideoPendingRun(ctx, tx, p, row, &takes[i])
		if err != nil {
			return false, err
		}
		changed = changed || promoted
	}
	if row.ChosenTaskID != nil || row.ChosenCleared {
		return changed, nil
	}
	for _, take := range takes {
		if !take.Adoptable {
			continue
		}
		id := take.Task.ID
		row.ChosenTaskID, row.ChosenCleared = &id, false
		return true, tx.SaveSegment(ctx, row, row.Revision)
	}
	return changed, nil
}

// Get 返回单段最新视图，并复用统一候选对账。
func (s *DramaSegmentService) Get(ctx context.Context, userID, sid uint64) (*model.DramaSegmentView, error) {
	// 1. 所有读入口都执行同一个父链锁和流程推进路径。
	v, p, ep, changed, err := s.readView(ctx, userID, sid)
	if err == nil && changed {
		s.publishSegment(ctx, p, ep, v)
	}
	return v, err
}

// ReconcileSegmentAndFrameRuns 供 bound 重入和恢复扫描复用，与 Get、List、回调具有相同准入和采用规则。
func (s *DramaSegmentService) ReconcileSegmentAndFrameRuns(ctx context.Context, userID, sid uint64) (*model.DramaSegmentView, error) {
	// 1. 共用读入口，终态流程永不重新绑定或产生候选。
	return s.Get(ctx, userID, sid)
}

// RecoverFrameRuns 扫描持久化恢复项并共用片段对账；本切片只推进已就绪的视频候选。
func (s *DramaSegmentService) RecoverFrameRuns(ctx context.Context, filter repository.FrameRunRecoveryFilter, limit int) error {
	// 1. 扫描有界且按仓储顺序，已删除父链忽略而真实故障上报。
	reader, ok := s.repo.(interface {
		ListRecoverableFrameRuns(context.Context, repository.FrameRunRecoveryFilter, int) ([]model.DramaSegmentFrameRun, error)
	})
	if !ok {
		return errcode.ErrInternal
	}
	runs, err := reader.ListRecoverableFrameRuns(ctx, filter, limit)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if _, err := s.ReconcileSegmentAndFrameRuns(ctx, run.UserID, run.SegmentID); err != nil && !isSegmentMissing(err) {
			return err
		}
	}
	return nil
}

// isSegmentMissing 只忽略软删父链；数据库故障仍返回调用方。
func isSegmentMissing(err error) bool {
	var business *errcode.Error
	return errors.Is(err, repository.ErrNotFound) || errors.As(err, &business) && business.Code == errcode.ErrDramaSegmentNotFound.Code
}

// frameRunView 只投影已有流程字段，不从任务反推已取消流程的状态。
func frameRunView(run *model.DramaSegmentFrameRun) *model.DramaSegmentFrameRunView {
	var input map[string]any
	// 持久化输入仅用于展示；非法内容不改变候选准入。
	_ = json.Unmarshal(run.ConfirmedInput, &input)
	return &model.DramaSegmentFrameRunView{ID: idcodec.ID(run.ID), SegmentID: idcodec.ID(run.SegmentID), RequestKey: run.RequestKey, FrameSource: run.FrameSource, State: run.State,
		SegmentRevision: run.SegmentRevision, Revision: run.Revision, ConfirmedVersion: run.ConfirmedVersion, ModelKey: run.ModelKey, OriginCanvasID: takeViewID(run.OriginCanvasID),
		FrameTaskID: takeViewID(run.FrameTaskID), FrameAssetID: cloneUint64Ptr(run.FrameAssetID), VideoTaskID: takeViewID(run.VideoTaskID), AdoptIfEmpty: run.AdoptIfEmpty, ConfirmedInput: input, SubmittingAt: run.SubmittingAt, ErrorCode: run.ErrorCode, ErrorMessage: run.ErrorMessage, CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt}
}

// takeViewID 保留对外 ID 编码和 nil 语义。
func takeViewID(id *uint64) *idcodec.ID {
	if id == nil {
		return nil
	}
	v := idcodec.ID(*id)
	return &v
}

// cloneUint64Ptr 复制十进制素材主键指针，与 /assets 接口格式保持一致。
func cloneUint64Ptr(id *uint64) *uint64 {
	if id == nil {
		return nil
	}
	v := *id
	return &v
}

// segmentOutputAsset 从已转存产物读取指定媒体类型的第一个素材；非法持久化JSON按内部错误处理。
func segmentOutputAsset(task *model.GenerationTask, kind string) (uint64, error) {
	if len(task.OutputJSON) == 0 {
		return 0, nil
	}
	var outputs []model.TaskOutput
	if err := json.Unmarshal(task.OutputJSON, &outputs); err != nil {
		return 0, fmt.Errorf("解析片段任务产物失败：%w", err)
	}
	for _, output := range outputs {
		if output.MediaType == kind && output.AssetID > 0 {
			return output.AssetID, nil
		}
	}
	return 0, nil
}

// segmentView 只展示合格候选；有效进行态可显示 running，失格 I2V 的迟到状态不得污染片段。
func (s *DramaSegmentService) segmentView(ctx context.Context, row *model.DramaSegment, no int, segVersion int64, takes []resolvedSegmentTake) (*model.DramaSegmentView, error) {
	adaptations, err := segmentAdaptations(row)
	if err != nil {
		return nil, err
	}
	v := &model.DramaSegmentView{ID: idcodec.ID(row.ID), EpisodeNo: no, Seq: row.Seq, Seconds: row.Seconds, Prompt: row.Prompt, Adaptations: adaptations, Takes: []model.DramaSegmentTakeView{}, Status: "idle", Revision: row.Revision, SegmentVersion: segVersion, UpdatedAt: row.UpdatedAt, ChosenCleared: row.ChosenCleared}
	v.Edit = model.DramaSegmentEditView{SourceInF: row.SourceInF, SourceOutF: row.SourceOutF, MuteOriginal: row.MuteOriginal, AudioGainDB: row.AudioGainDB, NativeIsDialogue: row.NativeIsDialogue, AvPtsOffsetMs: row.AvPtsOffsetMs}
	tasks := make([]model.GenerationTask, 0, len(takes))
	for _, take := range takes {
		if take.Usable {
			tasks = append(tasks, take.Task)
		}
	}
	frames, probeErr := s.resolveSegmentSourceFrames(ctx, row.UserID, row.ChosenTaskID, tasks)
	if probeErr != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		logger.Warn("片段读取探测源帧数失败，降级为未知帧数", zap.Uint64("segment_id", row.ID), zap.Error(probeErr))
	} else {
		v.SourceFrameCount = frames
	}
	if row.ChosenTaskID != nil {
		v.ChosenTaskID = idcodec.Encode(*row.ChosenTaskID)
	}
	for _, take := range takes {
		if !visibleSegmentTake(row, take) {
			continue
		}
		if take.Usable {
			v.Status = "done"
		}
		switch take.Task.Status {
		case model.TaskPending, model.TaskQueued, model.TaskRunning, model.TaskFinalizing:
			v.Status = "running"
		case model.TaskFailed:
			v.Status = "error"
		}
		// 流程进行态只投影片段状态，不能作为可选视频候选。
		if take.IsFrameRun && !take.Usable {
			continue
		}
		v.Takes = append(v.Takes, resolvedTakeView(take))
	}
	return v, nil
}

// visibleSegmentTake 原生进行态保留旧片场兼容，流程必须双向匹配且处于 video_pending。
func visibleSegmentTake(row *model.DramaSegment, take resolvedSegmentTake) bool {
	if take.Usable {
		return true
	}
	if !take.SourceEligible {
		return false
	}
	if take.IsFrameRun {
		return trustedSegmentRunTask(row.UserID, row.ID, take.Run, &take.Task) && take.Run.State == model.FrameRunStateVideoPending && slices.Contains(model.ActiveTaskStatuses, take.Task.Status)
	}
	return take.Origin == model.TakeOriginNative && take.Task.UserID == row.UserID && !take.Task.IsTest && take.Task.Kind == model.KindVideo && take.Task.Status != model.TaskSucceeded
}

// resolvedTakeView 把真实来源、QC 与流程首帧快照投影为候选视图。
func resolvedTakeView(take resolvedSegmentTake) model.DramaSegmentTakeView {
	v := model.DramaSegmentTakeView{TaskID: idcodec.ID(take.Task.ID), Status: take.Task.Status, Asset: take.Asset, ErrorMessage: take.Task.ErrorMessage, CreatedAt: take.Task.CreatedAt, Origin: string(take.Origin), QC: string(take.QC), SourceCanvasID: takeViewID(take.Task.CanvasProjectID)}
	if take.Run != nil {
		id := take.Run.ID
		v.RunID = takeViewID(&id)
		v.FrameAssetID = cloneUint64Ptr(take.Run.FrameAssetID)
	}
	if take.Link != nil {
		v.SourceCanvasID = takeViewID(&take.Link.SourceCanvasID)
	}
	return v
}

// segmentAsset 已删除素材显示为空；数据库等未知故障仍上报，不能伪装为没有素材。
func (s *DramaSegmentService) segmentAsset(ctx context.Context, userID, id uint64) (*model.AssetView, error) {
	v, err := s.assets.View(ctx, userID, id)
	var business *errcode.Error
	if errors.Is(err, repository.ErrNotFound) || (errors.As(err, &business) && business.Code == errcode.ErrAssetNotFound.Code) {
		return nil, nil
	}
	return v, err
}

// OnTaskChanged 从真实候选关系定位片段；失格、取消流程的迟到回调不改变采用、版本或候选。
func (s *DramaSegmentService) OnTaskChanged(ctx context.Context, task *model.GenerationTask) error {
	// 1. 不信任回调状态，先按归属证据找到段，再从任务表及资产重新准入。
	if task == nil || task.IsTest || task.Kind != model.KindVideo || task.CanvasProjectID == nil {
		return nil
	}
	ids, err := s.callbackSegmentIDs(ctx, task)
	if err != nil {
		return err
	}
	for _, sid := range batchLockIDs(ids) {
		valid, err := s.callbackTakeVisible(ctx, task, sid)
		if isSegmentMissing(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !valid {
			continue
		}
		if _, err := s.changedView(ctx, task.UserID, sid); err != nil && !isSegmentMissing(err) {
			return err
		}
	}
	return nil
}

// callbackSegmentIDs 同时支持原生节点、受信流程和跨画布导入，关联读取不依赖源画布。
func (s *DramaSegmentService) callbackSegmentIDs(ctx context.Context, task *model.GenerationTask) ([]uint64, error) {
	var ids []uint64
	if strings.HasPrefix(task.NodeID, "seg:") {
		if sid, ok := idcodec.Decode(strings.TrimPrefix(task.NodeID, "seg:")); ok {
			ids = append(ids, sid)
		}
	}
	if task.FrameRunID != nil {
		reader, ok := s.repo.(interface {
			FrameRun(context.Context, uint64, uint64, bool) (*model.DramaSegmentFrameRun, error)
		})
		if ok {
			run, err := reader.FrameRun(ctx, task.UserID, *task.FrameRunID, false)
			if err != nil && !errors.Is(err, repository.ErrNotFound) {
				return nil, err
			}
			if err == nil {
				ids = append(ids, run.SegmentID)
			}
		}
	}
	reader, ok := s.repo.(interface {
		TakeLinksByTask(context.Context, uint64, uint64) ([]model.DramaSegmentTakeLink, error)
	})
	if ok {
		links, err := reader.TakeLinksByTask(ctx, task.UserID, task.ID)
		if err != nil {
			return nil, err
		}
		for _, link := range links {
			ids = append(ids, link.SegmentID)
		}
	}
	return ids, nil
}

// callbackTakeVisible 只有有效候选或合法进行态才可触发片段对账，历史取消任务没有副作用。
func (s *DramaSegmentService) callbackTakeVisible(ctx context.Context, task *model.GenerationTask, sid uint64) (bool, error) {
	p, _, seg, err := s.segmentSource(ctx, task.UserID, sid)
	if err != nil {
		return false, err
	}
	takes, err := ResolveSegmentTakes(ctx, s.repo, s.assets, p, seg)
	if err != nil {
		return false, err
	}
	for _, take := range takes {
		if take.Task.ID == task.ID {
			return visibleSegmentTake(seg, take), nil
		}
	}
	return false, nil
}

// latestSegmentFrameRun 优先展示当前活跃流程，没有活跃流程时展示最近一次历史流程。
func latestSegmentFrameRun(runs []model.DramaSegmentFrameRun) *model.DramaSegmentFrameRunView {
	if len(runs) == 0 {
		return nil
	}
	for i := len(runs) - 1; i >= 0; i-- {
		if slices.Contains([]model.DramaFrameRunState{model.FrameRunStateDraft, model.FrameRunStateFramePending, model.FrameRunStateFrameReady, model.FrameRunStateConfirmed, model.FrameRunStateSubmitting, model.FrameRunStateVideoPending}, runs[i].State) {
			return frameRunView(&runs[i])
		}
	}
	return frameRunView(&runs[len(runs)-1])
}

// replayFrameRun 要求创建意图和目标片段均一致，允许图片提交中断后继续原任务。
func (s *DramaFrameRunService) replayFrameRun(ctx context.Context, userID, sid uint64, run *model.DramaSegmentFrameRun, req *model.CreateFrameRunReq, hash string) (*model.DramaSegmentFrameRunView, bool, error) {
	if run.RequestHash != hash || run.SegmentID != sid {
		return nil, false, errcode.ErrIdempotencyConflict
	}
	if _, err := s.loadFrameRun(ctx, userID, sid, run.ID); err != nil {
		return nil, false, err
	}
	if err := s.finishFrameCreation(ctx, run, req); err != nil {
		return nil, false, err
	}
	if run.State == model.FrameRunStateFramePending {
		if err := s.reconcileFrameRun(ctx, run); err != nil {
			return nil, false, err
		}
	}
	v, err := s.reloadFrameRunView(ctx, userID, sid, run.ID)
	return v, false, err
}

// frameImageKey 与视频流程保留前缀分离，图片任务不能进入 seg: 视频准入。
func frameImageKey(rid uint64) string { return "drama-frame-image:" + idcodec.Encode(rid) }

// frameImageNode 将图片任务与 seg: 视频节点隔离，防止图片占用片段视频候选。
func frameImageNode(sid uint64) string { return "seg-frame:" + idcodec.Encode(sid) }

// finishFrameCreation 在事务外创建图片任务，再按原 draft 版本绑定；取消或换图优先。
func (s *DramaFrameRunService) finishFrameCreation(ctx context.Context, run *model.DramaSegmentFrameRun, req *model.CreateFrameRunReq) error {
	if run.FrameSource != model.FrameSourceGenerate || run.State != model.FrameRunStateDraft || run.FrameTaskID != nil {
		return nil
	}
	p, _, _, err := s.segments.segmentSource(ctx, run.UserID, run.SegmentID)
	if err != nil {
		return err
	}
	createReq := &model.CreateGenerationTaskReq{Kind: model.KindImage, ModelID: req.ModelID, CanvasID: idcodec.ID(p.CanvasID),
		NodeID: frameImageNode(run.SegmentID), Input: req.Input}
	resp, err := s.tasks.Create(ctx, run.UserID, frameImageKey(run.ID), createReq)
	var task *model.GenerationTaskView
	if err == nil {
		task, err = frameCreatedTask(resp, createReq)
	}
	if err != nil {
		return s.frameCreationFailed(ctx, run, err)
	}
	updated, err := s.updateFrameRun(ctx, run.UserID, run.SegmentID, run.ID, func(row *model.DramaSegmentFrameRun) error {
		if row.State != model.FrameRunStateDraft || row.Revision != run.Revision {
			return errFrameRunUnchanged
		}
		id := task.ID
		row.FrameTaskID, row.State = &id, model.FrameRunStateFramePending
		return nil
	})
	if err != nil {
		return err
	}
	if updated.State != model.FrameRunStateFramePending || updated.FrameTaskID == nil || *updated.FrameTaskID != task.ID {
		// 窗口期取消/换图胜出，图片任务不再被使用；取消失败不能逆转用户状态。
		s.cancelFrameTask(ctx, run.UserID, task.ID)
	}
	*run = *updated
	return s.publishFrameRun(ctx, run)
}

// frameCreatedTask 统一转换逐项错误，并复核任务归属，避免同键普通任务被冒领。
func frameCreatedTask(resp *model.CreateGenerationTaskResp, req *model.CreateGenerationTaskReq) (*model.GenerationTaskView, error) {
	if resp == nil || len(resp.Items) != 1 {
		return nil, errcode.ErrInternal
	}
	item := resp.Items[0]
	if item.Error != nil {
		return nil, errcode.New(item.Error.Code, item.Error.Message, item.Error.Status)
	}
	if item.Task == nil || item.Task.ID == 0 {
		return nil, errcode.ErrInternal
	}
	task := item.Task
	if task.Kind != req.Kind || task.NodeID != req.NodeID || task.CanvasProjectID == nil || *task.CanvasProjectID != req.CanvasID {
		return nil, errcode.ErrIdempotencyConflict
	}
	return task, nil
}

// frameCreationFailed 仅将仍处于原 draft 的流程标记失败，错误消息不携带底层敏感信息。
func (s *DramaFrameRunService) frameCreationFailed(ctx context.Context, run *model.DramaSegmentFrameRun, cause error) error {
	updated, err := s.updateFrameRun(ctx, run.UserID, run.SegmentID, run.ID, func(row *model.DramaSegmentFrameRun) error {
		if row.State != model.FrameRunStateDraft || row.Revision != run.Revision {
			return errFrameRunUnchanged
		}
		row.State = model.FrameRunStateFailed
		row.ErrorCode, row.ErrorMessage = frameRunFailure(cause)
		return nil
	})
	if err != nil {
		return err
	}
	if err := s.publishFrameRun(ctx, updated); err != nil {
		return err
	}
	return cause
}

// frameRunFailure 对业务错误保留公开错误码，基础设施错误只保存通用提示。
func frameRunFailure(err error) (string, string) {
	var business *errcode.Error
	if errors.As(err, &business) {
		return strconv.Itoa(business.Code), business.Msg
	}
	return "internal", "首帧控镜处理失败，请重试"
}
