package service

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/repository"
)

// DramaTakeReader 是候选准入共用的只读边界，事务和无锁读取使用相同规则。
type DramaTakeReader interface {
	// SegmentTasks 返回本画布本段的正式任务。
	SegmentTasks(context.Context, uint64, uint64, string) ([]model.GenerationTask, error)
	// TasksByIDs 补齐导入及流程引用的用户正式任务。
	TasksByIDs(context.Context, uint64, []uint64) ([]model.GenerationTask, error)
	// ListTakeLinks 读取来源和人工质检，不从任务状态推测 QC。
	ListTakeLinks(context.Context, uint64, uint64) ([]model.DramaSegmentTakeLink, error)
	// FrameRunsBySegment 返回全部流程，包括取消、失败的历史证据。
	FrameRunsBySegment(context.Context, uint64, uint64) ([]model.DramaSegmentFrameRun, error)
}

type resolvedSegmentTake struct {
	Task            model.GenerationTask
	Asset           *model.AssetView
	Origin          model.DramaSegmentTakeOrigin
	QC              model.DramaSegmentTakeQC
	Run             *model.DramaSegmentFrameRun
	Link            *model.DramaSegmentTakeLink
	Usable          bool
	Adoptable       bool
	NeedsLinkCreate bool
	SourceEligible  bool
	IsFrameRun      bool
}

type segmentTakeSources struct {
	tasks []model.GenerationTask
	links []model.DramaSegmentTakeLink
	runs  []model.DramaSegmentFrameRun
}

// ResolveSegmentTakes 统一解析本段候选；准入与自动采用分离，绝不把失格流程降级成原生任务。
func ResolveSegmentTakes(ctx context.Context, reader DramaTakeReader, assets DramaSegmentAssetReader, p *model.DramaProject, seg *model.DramaSegment) ([]resolvedSegmentTake, error) {
	// 1. 汇总原生任务与可信关系引用，额外任务仍受用户及正式任务过滤。
	source, err := loadSegmentTakeSources(ctx, reader, p, seg)
	if err != nil {
		return nil, err
	}
	return resolveSegmentTakeSources(ctx, assets, p, seg, source)
}

func resolveSegmentTakeSources(ctx context.Context, assets DramaSegmentAssetReader, p *model.DramaProject, seg *model.DramaSegment, source segmentTakeSources) ([]resolvedSegmentTake, error) {
	out := make([]resolvedSegmentTake, 0, len(source.tasks))
	for _, task := range source.tasks {
		take := classifySegmentTake(task, source)
		if err := admitSegmentTake(ctx, assets, p, seg, &take); err != nil {
			return nil, err
		}
		take.Adoptable = take.Usable && take.QC != model.TakeQCRejected && take.Origin != model.TakeOriginCanvasImport
		if take.IsFrameRun {
			take.Adoptable = take.Adoptable && take.Run.AdoptIfEmpty && take.Run.SegmentRevision == seg.Revision
		}
		take.NeedsLinkCreate = take.Usable && take.Link == nil
		out = append(out, take)
	}
	return out, nil
}

// loadSegmentTakeSources 去重后按任务创建时间和 ID 固定候选顺序。
func loadSegmentTakeSources(ctx context.Context, reader DramaTakeReader, p *model.DramaProject, seg *model.DramaSegment) (segmentTakeSources, error) {
	var src segmentTakeSources
	var err error
	src.tasks, err = reader.SegmentTasks(ctx, seg.UserID, p.CanvasID, segmentNode(seg.ID))
	if err != nil {
		return src, err
	}
	src.links, err = reader.ListTakeLinks(ctx, seg.UserID, seg.ID)
	if err != nil {
		return src, err
	}
	src.runs, err = reader.FrameRunsBySegment(ctx, seg.UserID, seg.ID)
	if err != nil {
		return src, err
	}
	ids := make([]uint64, 0, len(src.links)+len(src.runs))
	for _, link := range src.links {
		ids = append(ids, link.TaskID)
	}
	for _, run := range src.runs {
		if run.VideoTaskID != nil {
			ids = append(ids, *run.VideoTaskID)
		}
	}
	extra, err := reader.TasksByIDs(ctx, seg.UserID, batchLockIDs(ids))
	if err != nil {
		return src, err
	}
	byID := make(map[uint64]model.GenerationTask, len(src.tasks)+len(extra))
	for _, task := range append(src.tasks, extra...) {
		byID[task.ID] = task
	}
	src.tasks = make([]model.GenerationTask, 0, len(byID))
	for _, task := range byID {
		src.tasks = append(src.tasks, task)
	}
	slices.SortFunc(src.tasks, func(a, b model.GenerationTask) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return src, nil
}

// classifySegmentTake 任一流程证据都阻断 native 回落；双向一致性留给统一准入。
func classifySegmentTake(task model.GenerationTask, src segmentTakeSources) resolvedSegmentTake {
	take := resolvedSegmentTake{Task: task, Origin: model.TakeOriginNative, QC: model.TakeQCUnreviewed, IsFrameRun: task.FrameRunID != nil}
	for i := range src.links {
		if src.links[i].TaskID == task.ID {
			take.Link = &src.links[i]
			take.Origin, take.QC = take.Link.Origin, take.Link.QC
			break
		}
	}
	for i := range src.runs {
		run := &src.runs[i]
		if task.FrameRunID != nil && *task.FrameRunID == run.ID || run.VideoTaskID != nil && *run.VideoTaskID == task.ID {
			take.Run = run
			take.IsFrameRun = true
			break
		}
	}
	if take.Link != nil && (take.Link.Origin == model.TakeOriginI2V || take.Link.RunID != nil) {
		take.IsFrameRun = true
	}
	if take.IsFrameRun {
		take.Origin = model.TakeOriginI2V
	}
	return take
}

// trustedSegmentRunTask 强制验证非空双向任务键与确认版本，不因终态放宽。
func trustedSegmentRunTask(userID, segmentID uint64, run *model.DramaSegmentFrameRun, task *model.GenerationTask) bool {
	return run != nil && run.UserID == userID && run.SegmentID == segmentID && task.UserID == userID &&
		run.VideoTaskID != nil && *run.VideoTaskID == task.ID && task.FrameRunID != nil && *task.FrameRunID == run.ID &&
		task.FrameRunConfirmRev != nil && *task.FrameRunConfirmRev == run.ConfirmedVersion
}

// admitSegmentTake 先验证来源和生命周期，再验证实际可读视频，真实存储故障原样返回。
func admitSegmentTake(ctx context.Context, assets DramaSegmentAssetReader, p *model.DramaProject, seg *model.DramaSegment, take *resolvedSegmentTake) error {
	task := &take.Task
	if task.UserID != seg.UserID || task.IsTest || task.Kind != model.KindVideo {
		return nil
	}
	switch {
	case take.IsFrameRun:
		if !trustedSegmentRunTask(seg.UserID, seg.ID, take.Run, task) {
			return nil
		}
		if take.Run.State != model.FrameRunStateBound && take.Run.State != model.FrameRunStateVideoPending {
			return nil
		}
	case take.Origin == model.TakeOriginCanvasImport:
		if task.CanvasProjectID == nil || *task.CanvasProjectID != take.Link.SourceCanvasID {
			return nil
		}
	default:
		if task.CanvasProjectID == nil || *task.CanvasProjectID != p.CanvasID || task.NodeID != segmentNode(seg.ID) {
			return nil
		}
	}
	take.SourceEligible = true
	if task.Status != model.TaskSucceeded {
		return nil
	}
	var err error
	take.Asset, err = resolveTakeVideoAsset(ctx, assets, seg.UserID, task)
	take.Usable = take.Asset != nil
	return err
}

// resolveTakeVideoAsset 不以产物中的非零 ID 代替实际的用户视频可读性。
func resolveTakeVideoAsset(ctx context.Context, assets DramaSegmentAssetReader, user uint64, task *model.GenerationTask) (*model.AssetView, error) {
	id, err := segmentOutputAsset(task, model.KindVideo)
	if err != nil || id == 0 {
		return nil, err
	}
	if assets == nil {
		return nil, errcode.ErrInternal
	}
	view, err := (&DramaSegmentService{assets: assets}).segmentAsset(ctx, user, id)
	if err != nil || view == nil {
		return nil, err
	}
	if view.ID != id || view.Kind != model.KindVideo {
		return nil, nil
	}
	return view, nil
}

// findUsableSegmentTake 供采用、QC、导入与合成共用，QC 驳回不影响人工重新质检。
func findUsableSegmentTake(takes []resolvedSegmentTake, id uint64, allowRejected bool) (*resolvedSegmentTake, error) {
	for i := range takes {
		if takes[i].Task.ID != id || !takes[i].Usable {
			continue
		}
		if !allowRejected && takes[i].QC == model.TakeQCRejected {
			return nil, errcode.ErrDramaTakeQCRejected
		}
		return &takes[i], nil
	}
	return nil, errcode.ErrDramaTakeInvalid
}

// takeLinkFor 从实际准入结果建立来源快照，补缺插入不能覆盖既有 QC。
func takeLinkFor(p *model.DramaProject, seg *model.DramaSegment, take *resolvedSegmentTake) *model.DramaSegmentTakeLink {
	link := &model.DramaSegmentTakeLink{UserID: seg.UserID, SegmentID: seg.ID, TaskID: take.Task.ID, AssetID: take.Asset.ID, SourceCanvasID: p.CanvasID, Origin: take.Origin, QC: model.TakeQCUnreviewed}
	if take.Run != nil {
		id := take.Run.ID
		link.RunID = &id
	}
	if take.Link != nil {
		link.SourceCanvasID = take.Link.SourceCanvasID
	}
	return link
}

// PromoteSegmentVideoPendingRun 在父链锁内补缺候选并 CAS 推进 bound，不执行自动采用。
func PromoteSegmentVideoPendingRun(ctx context.Context, tx repository.DramaTx, p *model.DramaProject, seg *model.DramaSegment, take *resolvedSegmentTake) (bool, error) {
	if !pendingSegmentPromotion(take) {
		return false, nil
	}
	run, err := tx.FrameRun(ctx, seg.UserID, take.Run.ID, true)
	if err != nil {
		return false, err
	}
	if run.State != model.FrameRunStateVideoPending || !trustedSegmentRunTask(seg.UserID, seg.ID, run, &take.Task) {
		return false, errcode.ErrDramaFrameRunInconsistent
	}
	if _, err := tx.CreateTakeLinkIfAbsent(ctx, takeLinkFor(p, seg, take)); err != nil {
		return false, err
	}
	run.State = model.FrameRunStateBound
	if err := tx.SaveFrameRun(ctx, run, run.Revision); err != nil {
		return false, err
	}
	take.Run = run
	// 已存在的人工 QC 可能由补缺冲突保留下来，采用前必须重读。
	take.Link, err = tx.TakeLink(ctx, seg.UserID, seg.ID, take.Task.ID)
	if err == nil {
		take.QC = take.Link.QC
		take.NeedsLinkCreate = false
		take.Adoptable = take.Adoptable && take.QC != model.TakeQCRejected
	}
	return true, err
}

// PatchTakeQC 在父链锁内更新人工 QC；只有改变采用时才递增集的媒体版本。
func (s *DramaSegmentService) PatchTakeQC(ctx context.Context, userID, sid, tid uint64, req *model.PatchDramaTakeQCReq) (*model.DramaSegmentView, error) {
	// 1. Service 也验证枚举，不能依赖 HTTP binding 保护内部调用。
	if req == nil || req.Revision < 1 || !slices.Contains([]model.DramaSegmentTakeQC{model.TakeQCUnreviewed, model.TakeQCAccepted, model.TakeQCRejected}, req.QC) {
		return nil, errcode.ErrInvalidParams
	}
	frames, err := s.resolveReplacementFrames(ctx, userID, sid, tid, req)
	if err != nil {
		return nil, segmentError(err)
	}
	err = s.repo.WithTx(ctx, func(tx repository.DramaTx) error {
		p, ep, seg, err := lockSegment(ctx, tx, userID, sid)
		if err != nil {
			return err
		}
		if seg.Revision != req.Revision {
			return errcode.ErrCanvasConflict
		}
		if req.ExpectedSegmentVersion != nil && ep.SegmentVersion != *req.ExpectedSegmentVersion {
			return errcode.ErrDramaManifestStale
		}
		return s.patchTakeQCLocked(ctx, tx, p, seg, tid, req, frames)
	})
	if err != nil {
		return nil, segmentError(err)
	}
	// 2. 返回只读最终视图，纯 QC 响应不能顺便执行自动采用。
	return s.takeMutationView(ctx, userID, sid, true)
}

// resolveReplacementFrames 在锁外探测驳回替换时的目标源帧数。
func (s *DramaSegmentService) resolveReplacementFrames(ctx context.Context, userID, sid, tid uint64, req *model.PatchDramaTakeQCReq) (*int32, error) {
	if req.ReplacementTaskID == nil {
		return nil, nil
	}
	_, _, seg, err := s.segmentSource(ctx, userID, sid)
	if err != nil {
		return nil, err
	}
	if seg.Revision != req.Revision {
		return nil, errcode.ErrCanvasConflict
	}
	if err := requireDramaVideoSegment(seg); err != nil {
		return nil, err
	}
	if req.QC != model.TakeQCRejected || seg.ChosenTaskID == nil || *seg.ChosenTaskID != tid {
		return nil, nil
	}
	if !needsTakeSelectionFrames(seg, req.ResetEditOnTaskChange) {
		return nil, nil
	}
	targetID := uint64(*req.ReplacementTaskID)
	return s.resolveSegmentSourceFrames(ctx, userID, &targetID, nil)
}

// patchTakeQCLocked 把 QC 行与段 CAS 放在同一事务；驳回已采用结果必须显式决定替换或清空。
func (s *DramaSegmentService) patchTakeQCLocked(ctx context.Context, tx repository.DramaTx, p *model.DramaProject, seg *model.DramaSegment, tid uint64, req *model.PatchDramaTakeQCReq, frames *int32) error {
	if req.ReplacementTaskID != nil {
		if err := requireDramaVideoSegment(seg); err != nil {
			return err
		}
	}
	takes, err := ResolveSegmentTakes(ctx, tx, s.assets, p, seg)
	if err != nil {
		return err
	}
	take, err := findUsableSegmentTake(takes, tid, true)
	if err != nil {
		return err
	}
	changing := req.QC == model.TakeQCRejected && seg.ChosenTaskID != nil && *seg.ChosenTaskID == tid
	if changing {
		if err := applyRejectedTakeDisposition(ctx, tx, p, seg, takes, tid, req, frames); err != nil {
			return err
		}
	}
	if _, err := PromoteSegmentVideoPendingRun(ctx, tx, p, seg, take); err != nil {
		return err
	}
	if take.NeedsLinkCreate {
		if _, err := tx.CreateTakeLinkIfAbsent(ctx, takeLinkFor(p, seg, take)); err != nil {
			return err
		}
	}
	if err := tx.UpdateTakeLinkQC(ctx, seg.UserID, seg.ID, tid, req.QC); err != nil {
		return err
	}
	bt, ok := tx.(segmentBatchVersionTx)
	if !ok {
		return errcode.ErrInternal
	}
	if _, err := bt.SaveSegmentNoBump(ctx, seg, req.Revision); err != nil {
		return err
	}
	if changing {
		return bt.BumpSegmentVersion(ctx, seg.UserID, seg.EpisodeID)
	}
	return nil
}

// applyRejectedTakeDisposition 处理驳回当前采用项时的显式清空或合法替换并校验保留剪辑边界。
func applyRejectedTakeDisposition(ctx context.Context, tx repository.DramaTx, p *model.DramaProject, seg *model.DramaSegment, takes []resolvedSegmentTake, tid uint64, req *model.PatchDramaTakeQCReq, frames *int32) error {
	if err := replaceRejectedTake(seg, takes, tid, req); err != nil {
		return err
	}
	if req.ReplacementTaskID == nil {
		return validateTakeSelectionEdit(seg, nil, frames)
	}
	replacement, err := findUsableSegmentTake(takes, uint64(*req.ReplacementTaskID), false)
	if err != nil {
		return err
	}
	if _, err := PromoteSegmentVideoPendingRun(ctx, tx, p, seg, replacement); err != nil {
		return err
	}
	return validateTakeSelectionEdit(seg, req.ResetEditOnTaskChange, frames)
}

// replaceRejectedTake 缺少明确处置时拒绝驳回，避免静默丢失当前采用。
func replaceRejectedTake(seg *model.DramaSegment, takes []resolvedSegmentTake, tid uint64, req *model.PatchDramaTakeQCReq) error {
	if (req.ReplacementTaskID != nil) == req.ClearChosen {
		return errcode.ErrDramaFrameRunConflict
	}
	if req.ClearChosen {
		seg.ChosenTaskID, seg.ChosenCleared = nil, true
		repository.ResetSegmentEdit(seg)
		return nil
	}
	id := uint64(*req.ReplacementTaskID)
	if id == tid {
		return errcode.ErrDramaFrameRunConflict
	}
	if _, err := findUsableSegmentTake(takes, id, false); err != nil {
		return err
	}
	adoptSegmentTake(seg, id, req.ResetEditOnTaskChange)
	return nil
}

// ImportTakes 校验源画布和正式成功视频后补缺候选，重导永不覆盖来源或绕过人工驳回。
func (s *DramaSegmentService) ImportTakes(ctx context.Context, userID, sid uint64, req *model.ImportDramaTakesReq) (*model.DramaSegmentView, error) {
	// 1. 源画布必须实时存活且归属本用户，不接受客户端提交的来源元数据。
	if req == nil || req.Revision < 1 || req.SourceCanvasID == 0 {
		return nil, errcode.ErrInvalidParams
	}
	if s.canvases == nil {
		return nil, errcode.ErrInternal
	}
	source, err := s.canvases.GetByID(ctx, userID, uint64(req.SourceCanvasID))
	if errors.Is(err, repository.ErrNotFound) {
		return nil, errcode.ErrDramaNotFound
	}
	if err != nil {
		return nil, err
	}
	if source == nil || source.ID != uint64(req.SourceCanvasID) || source.UserID != userID || source.DeletedAt.Valid {
		return nil, errcode.ErrDramaNotFound
	}
	ids, err := importTakeIDs(req)
	if err != nil {
		return nil, err
	}
	adoptFrames, err := s.resolveImportAdoptFrames(ctx, userID, sid, req, ids[0])
	if err != nil {
		return nil, segmentError(err)
	}
	changed := false
	// 2. 只关联时不触碰段/集版本；采用时段 CAS 和集媒体版本只保存一次。
	err = s.repo.WithTx(ctx, func(tx repository.DramaTx) error {
		p, _, seg, err := lockSegment(ctx, tx, userID, sid)
		if err != nil {
			return err
		}
		if seg.Revision != req.Revision {
			return errcode.ErrCanvasConflict
		}
		changed, err = s.importTakesLocked(ctx, tx, p, seg, req, ids, adoptFrames)
		return err
	})
	if err != nil {
		return nil, segmentError(err)
	}
	return s.takeMutationView(ctx, userID, sid, changed || req.Adopt)
}

// importTakeIDs 支持单条优先契约与可选批量，并在采用时严格限定一条。
func importTakeIDs(req *model.ImportDramaTakesReq) ([]uint64, error) {
	ids := make([]uint64, 0, len(req.TaskIDs)+1)
	if req.TaskID != 0 {
		ids = append(ids, uint64(req.TaskID))
	}
	for _, id := range req.TaskIDs {
		if id == 0 {
			return nil, errcode.ErrInvalidParams
		}
		ids = append(ids, uint64(id))
	}
	ids = batchLockIDs(ids)
	if len(ids) == 0 || len(ids) > 100 || req.Adopt && len(ids) != 1 {
		return nil, errcode.ErrInvalidParams
	}
	return ids, nil
}

// importTakesLocked 逐条核验当前用户可读视频，任何失败都回滚整批补缺关联。
func (s *DramaSegmentService) importTakesLocked(ctx context.Context, tx repository.DramaTx, p *model.DramaProject, seg *model.DramaSegment, req *model.ImportDramaTakesReq, ids []uint64, frames *int32) (bool, error) {
	if req.Adopt {
		if err := requireDramaVideoSegment(seg); err != nil {
			return false, err
		}
	}
	tasks, err := tx.TasksByIDs(ctx, seg.UserID, ids)
	if err != nil {
		return false, err
	}
	if len(tasks) != len(ids) {
		return false, errcode.ErrDramaTakeInvalid
	}
	changed := false
	for _, task := range tasks {
		inserted, err := s.importSingleTake(ctx, tx, seg, &task, uint64(req.SourceCanvasID))
		if err != nil {
			return false, err
		}
		changed = changed || inserted
	}
	if !req.Adopt {
		return changed, nil
	}
	link, err := tx.TakeLink(ctx, seg.UserID, seg.ID, ids[0])
	if err != nil {
		return false, err
	}
	if link.QC == model.TakeQCRejected {
		return false, errcode.ErrDramaTakeQCRejected
	}
	takes, err := ResolveSegmentTakes(ctx, tx, s.assets, p, seg)
	if err != nil {
		return false, err
	}
	take, err := findUsableSegmentTake(takes, ids[0], false)
	if err != nil {
		return false, err
	}
	if _, err := PromoteSegmentVideoPendingRun(ctx, tx, p, seg, take); err != nil {
		return false, err
	}
	adoptSegmentTake(seg, ids[0], req.ResetEditOnTaskChange)
	if err := validateTakeSelectionEdit(seg, req.ResetEditOnTaskChange, frames); err != nil {
		return false, err
	}
	return changed, tx.SaveSegment(ctx, seg, req.Revision)
}

// importSingleTake 校验单个导入任务并按补缺插入候选关联。
func (s *DramaSegmentService) importSingleTake(ctx context.Context, tx repository.DramaTx, seg *model.DramaSegment, task *model.GenerationTask, sourceCanvasID uint64) (bool, error) {
	if task.UserID != seg.UserID || task.IsTest || task.CanvasProjectID == nil || *task.CanvasProjectID != sourceCanvasID || task.Status != model.TaskSucceeded || task.Kind != model.KindVideo {
		return false, errcode.ErrDramaTakeInvalid
	}
	asset, err := resolveTakeVideoAsset(ctx, s.assets, seg.UserID, task)
	if err != nil {
		return false, err
	}
	if asset == nil {
		return false, errcode.ErrDramaTakeInvalid
	}
	link, err := buildImportTakeLink(ctx, tx, seg, task, sourceCanvasID, asset.ID)
	if err != nil {
		return false, err
	}
	return tx.CreateTakeLinkIfAbsent(ctx, link)
}

// buildImportTakeLink 禁止把已取消、失败或跨段的 I2V 流程任务冒充 canvas_import 写入。
func buildImportTakeLink(ctx context.Context, tx repository.DramaTx, seg *model.DramaSegment, task *model.GenerationTask, sourceCanvasID, assetID uint64) (*model.DramaSegmentTakeLink, error) {
	if task.FrameRunID == nil {
		return &model.DramaSegmentTakeLink{UserID: seg.UserID, SegmentID: seg.ID, TaskID: task.ID, SourceCanvasID: sourceCanvasID, AssetID: assetID, Origin: model.TakeOriginCanvasImport, QC: model.TakeQCUnreviewed}, nil
	}
	run, err := tx.FrameRun(ctx, seg.UserID, *task.FrameRunID, false)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return nil, err
	}
	if err != nil || !trustedSegmentRunTask(seg.UserID, seg.ID, run, task) || (run.State != model.FrameRunStateVideoPending && run.State != model.FrameRunStateBound) {
		return nil, errcode.ErrDramaTakeInvalid
	}
	runID := run.ID
	return &model.DramaSegmentTakeLink{UserID: seg.UserID, SegmentID: seg.ID, TaskID: task.ID, SourceCanvasID: sourceCanvasID, AssetID: assetID, RunID: &runID, Origin: model.TakeOriginI2V, QC: model.TakeQCUnreviewed}, nil
}

// takeMutationView 在事务提交后投影并按实际写入发送事件，不隐式采用候选。
func (s *DramaSegmentService) takeMutationView(ctx context.Context, userID, sid uint64, publish bool) (*model.DramaSegmentView, error) {
	v, p, ep, _, err := s.readSegmentView(ctx, userID, sid, false)
	if err == nil && publish {
		s.publishSegment(ctx, p, ep, v)
	}
	return v, err
}
