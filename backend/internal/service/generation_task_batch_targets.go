package service

import (
	"context"
	"errors"
	"slices"
	"strings"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/repository"
)

// preResolveBatchTargets 在账户锁前完成两类目标的全局预锁与不可变业务校验。
func preResolveBatchTargets(ctx context.Context, tx repository.GenerationTaskTx, b *taskBatch) (map[string]preResolvedTarget, error) {
	out := make(map[string]preResolvedTarget, len(b.nodes))
	nodes := slices.Clone(b.nodes)
	slices.Sort(nodes)
	nodes = slices.Compact(nodes)
	if err := preResolveDramaTargets(ctx, tx, b, nodes, out); err != nil {
		return nil, err
	}
	if err := preResolveMarketingTargets(ctx, tx, b, nodes, out); err != nil {
		return nil, err
	}
	return out, nil
}

// recordTargetError 记录可隔离的业务错误，系统错误必须中止外层事务。
func recordTargetError(out map[string]preResolvedTarget, node string, err error) error {
	var business *errcode.Error
	if !errors.As(err, &business) {
		return err
	}
	out[node] = preResolvedTarget{err: err}
	return nil
}

type dramaBatchProbe struct {
	node string
	row  *model.DramaSegment
	ep   *model.DramaEpisode
}

// probeDramaTargets 无锁反查父链，缺失节点立即冻结为错误，之后不再重新解析。
func probeDramaTargets(ctx context.Context, tx repository.DramaTx, b *taskBatch, nodes []string, out map[string]preResolvedTarget) ([]dramaBatchProbe, error) {
	var probes []dramaBatchProbe
	for _, node := range nodes {
		if !strings.HasPrefix(node, "seg:") {
			continue
		}
		sid, ok := idcodec.Decode(strings.TrimPrefix(node, "seg:"))
		if !ok {
			out[node] = preResolvedTarget{err: errcode.ErrInvalidParams}
			continue
		}
		row, err := tx.Segment(ctx, b.userID, sid, false)
		if err != nil {
			if err := recordTargetError(out, node, segmentError(err)); err != nil {
				return nil, err
			}
			continue
		}
		ep, err := tx.SegmentEpisode(ctx, b.userID, row.EpisodeID)
		if err != nil {
			if err := recordTargetError(out, node, segmentError(err)); err != nil {
				return nil, err
			}
			continue
		}
		probes = append(probes, dramaBatchProbe{node, row, ep})
	}
	return probes, nil
}

// preResolveDramaTargets 按项目、集、段的主键升序批量加锁，再校验稳定的归属和业务状态。
func preResolveDramaTargets(ctx context.Context, tx repository.GenerationTaskTx, b *taskBatch, nodes []string, out map[string]preResolvedTarget) error {
	if !hasSegmentNode(nodes) {
		return nil
	}
	adapter, ok := tx.(interface{ DramaTx() repository.DramaTx })
	if !ok {
		return errcode.ErrInternal
	}
	dtx := adapter.DramaTx()
	probes, err := probeDramaTargets(ctx, dtx, b, nodes, out)
	if err != nil {
		return err
	}
	var pids, eids, sids []uint64
	for _, probe := range probes {
		pids = append(pids, probe.ep.DramaID)
		eids = append(eids, probe.ep.ID)
		sids = append(sids, probe.row.ID)
	}
	ps, err := dtx.LockProjects(ctx, b.userID, batchLockIDs(pids))
	if err != nil {
		return err
	}
	es, err := dtx.LockEpisodes(ctx, b.userID, batchLockIDs(eids))
	if err != nil {
		return err
	}
	ss, err := dtx.LockSegments(ctx, b.userID, batchLockIDs(sids))
	if err != nil {
		return err
	}
	for _, probe := range probes {
		target := resolveLockedDramaTarget(probe, ps, es, ss)
		if target.err == nil && b.opts.DramaRunBind != nil {
			target.err = resolveDramaRunTarget(ctx, tx, dtx, b, &target)
		}
		if target.err == nil && target.existingRunTask == nil {
			target.err = validateDramaBatchTarget(ctx, dtx, b, &target)
		}
		if target.err != nil {
			if err := recordTargetError(out, probe.node, target.err); err != nil {
				return err
			}
		} else {
			out[probe.node] = target
		}
	}
	return nil
}

// resolveLockedDramaTarget 只使用实际锁到的行，复核无锁反查与持锁父链的一致性。
func resolveLockedDramaTarget(probe dramaBatchProbe, ps []model.DramaProject, es []model.DramaEpisode, ss []model.DramaSegment) preResolvedTarget {
	target := preResolvedTarget{}
	for i := range ps {
		if ps[i].ID == probe.ep.DramaID {
			target.project = &ps[i]
		}
	}
	for i := range es {
		if es[i].ID == probe.ep.ID {
			target.episode = &es[i]
		}
	}
	for i := range ss {
		if ss[i].ID == probe.row.ID {
			target.segment = &ss[i]
		}
	}
	if target.project == nil || target.episode == nil || target.segment == nil {
		target.err = errcode.ErrDramaSegmentNotFound
		return target
	}
	if target.segment.EpisodeID != target.episode.ID || target.episode.DramaID != target.project.ID {
		target.err = errcode.ErrDramaSegmentNotFound
	}
	return target
}

// validateDramaBatchTarget 在积分锁前冻结画布、锁集、短剧任务与时间轴校验结果。
func validateDramaBatchTarget(ctx context.Context, tx repository.DramaTx, b *taskBatch, target *preResolvedTarget) error {
	p, ep, row := target.project, target.episode, target.segment
	if err := requireDramaVideoSegment(row); err != nil {
		return err
	}
	if b.req.CanvasID == 0 || uint64(b.req.CanvasID) != p.CanvasID || b.req.Kind != model.KindVideo {
		return errcode.ErrInvalidParams
	}
	if ep.LockedAt == nil {
		return errcode.ErrDramaStepNotReady.WithMsg("媒体任务只允许已锁定的集")
	}
	jobs, err := tx.Jobs(ctx, b.userID, p.ID, true)
	if err != nil {
		return err
	}
	if dramaJobsConflict(jobs, &model.DramaJobReq{Step: "segment", Episodes: []int{ep.No}}) {
		return errcode.ErrDramaJobBusy
	}
	if err := checkSegmentTimeline(b.input, row.Seconds); err != nil {
		return err
	}
	rows, err := tx.Segments(ctx, b.userID, ep.ID)
	if err != nil {
		return err
	}
	var exclude uint64
	if b.opts.DramaRunBind != nil {
		exclude = b.opts.DramaRunBind.RunID
	}
	if err := checkActiveSegmentFrameRuns(ctx, tx, b.userID, rows, b.key, exclude); err != nil {
		return err
	}
	for _, segment := range rows {
		target.mediaNodes = append(target.mediaNodes, segmentNode(segment.ID))
	}
	return nil
}

// validateBatchMediaTasks 保存点内仅重读任务表，绝不重新解析或锁定业务目标。
func validateBatchMediaTasks(ctx context.Context, tx repository.GenerationTaskTx, b *taskBatch, target preResolvedTarget, i int) error {
	if target.script != nil {
		mtx := tx.(interface{ MarketingTx() repository.MarketingTx }).MarketingTx()
		tasks, err := mtx.ScriptTasks(ctx, b.userID, []string{b.nodes[i]})
		if err != nil {
			return err
		}
		for _, task := range tasks {
			if slices.Contains(model.ActiveTaskStatuses, task.Status) {
				return errcode.ErrMarketingScriptBusy
			}
		}
	}
	if target.segment == nil {
		return nil
	}
	dtx := tx.(interface{ DramaTx() repository.DramaTx }).DramaTx()
	for _, node := range target.mediaNodes {
		tasks, err := dtx.SegmentTasks(ctx, b.userID, target.project.CanvasID, node)
		if err != nil {
			return err
		}
		for _, task := range tasks {
			if slices.Contains(model.ActiveTaskStatuses, task.Status) && (b.key == "" || task.IdempotencyKey != idemKeyAt(b.key, i)) {
				return errcode.ErrDramaJobBusy
			}
		}
	}
	return nil
}

// batchLockIDs 在调用仓储前就固定升序去重，避免不同实现偏离全局锁序。
func batchLockIDs(ids []uint64) []uint64 {
	slices.Sort(ids)
	return slices.Compact(ids)
}

// resolveDramaRunTarget 按父链之后的流程锁执行终态优先表；终态绝不进入积分冻结。
func resolveDramaRunTarget(ctx context.Context, tx repository.GenerationTaskTx, dtx repository.DramaTx, b *taskBatch, target *preResolvedTarget) error {
	bind := b.opts.DramaRunBind
	run, err := dtx.FrameRun(ctx, b.userID, bind.RunID, true)
	if errors.Is(err, repository.ErrNotFound) {
		return errcode.ErrDramaFrameRunNotFound
	}
	if err != nil {
		return err
	}
	if run.SegmentID != target.segment.ID {
		return errcode.ErrDramaFrameRunInconsistent
	}
	target.run = run
	hit, err := tx.FindByIdempotencyKey(ctx, b.userID, b.key)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return err
	}
	if errors.Is(err, repository.ErrNotFound) {
		hit = nil
	}
	switch run.State {
	case model.FrameRunStateCancelled:
		return resolveCancelledRunTask(ctx, dtx, b, run, hit)
	case model.FrameRunStateFailed:
		return errcode.ErrDramaFrameRunConflict
	case model.FrameRunStateVideoPending, model.FrameRunStateBound:
		if run.VideoTaskID == nil || hit == nil || hit.ID != *run.VideoTaskID || !matchesDramaRunBind(hit, bind) || run.ConfirmedVersion != bind.ConfirmRev {
			return errcode.ErrDramaFrameRunInconsistent
		}
		target.existingRunTask = hit
		return nil
	case model.FrameRunStateSubmitting:
		return resolveSubmittingRunTask(ctx, dtx, b, target, hit)
	default:
		return errcode.ErrDramaFrameRunConflict
	}
}

// resolveSubmittingRunTask 同键只接受本次确认的受信任务，并原子修复正向键；普通任务零冻结拒绝。
func resolveSubmittingRunTask(ctx context.Context, tx repository.DramaTx, b *taskBatch, target *preResolvedTarget, hit *model.GenerationTask) error {
	run, bind := target.run, b.opts.DramaRunBind
	if run.ConfirmedVersion != bind.ConfirmRev || run.VideoIdempotencyKey != b.key || run.ModelKey != b.req.ModelID || run.ConfirmedInputHash == nil {
		return errcode.ErrDramaFrameRunInconsistent
	}
	if hit == nil {
		return nil
	}
	if !matchesDramaRunBind(hit, bind) || run.VideoTaskID != nil && *run.VideoTaskID != hit.ID {
		return errcode.ErrIdempotencyConflict
	}
	run.VideoTaskID, run.State = &hit.ID, model.FrameRunStateVideoPending
	if err := tx.SaveFrameRun(ctx, run, run.Revision); err != nil {
		return err
	}
	target.existingRunTask = hit
	return nil
}

// resolveCancelledRunTask 只有明确归属本 run 的任务可取消，普通占键任务及账本保持原样。
func resolveCancelledRunTask(ctx context.Context, tx repository.DramaTx, b *taskBatch, run *model.DramaSegmentFrameRun, hit *model.GenerationTask) error {
	if hit == nil && run.VideoTaskID != nil {
		tasks, err := tx.TasksByIDs(ctx, b.userID, []uint64{*run.VideoTaskID})
		if err != nil {
			return err
		}
		if len(tasks) == 1 {
			hit = &tasks[0]
		}
	}
	if hit == nil {
		return errcode.ErrDramaFrameRunInconsistent
	}
	if run.VideoTaskID != nil {
		if hit.ID != *run.VideoTaskID {
			return errcode.ErrDramaFrameRunInconsistent
		}
	} else if !matchesDramaRunBind(hit, &DramaFrameRunBind{RunID: run.ID, ConfirmRev: run.ConfirmedVersion}) {
		return errcode.ErrDramaFrameRunInconsistent
	}
	if !model.IsTerminalStatus(hit.Status) && hit.Status != model.TaskFinalizing {
		b.cancelTaskID = hit.ID
	}
	return errcode.ErrDramaFrameRunConflict
}
