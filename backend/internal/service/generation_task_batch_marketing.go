package service

import (
	"context"
	"strings"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/repository"
)

type marketingBatchProbe struct {
	node   string
	script *model.MarketingScript
}

// probeMarketingTargets 在加锁前反查脚本父链，缺失节点不在积分锁后重试。
func probeMarketingTargets(ctx context.Context, tx repository.MarketingTx, b *taskBatch, nodes []string, out map[string]preResolvedTarget) ([]marketingBatchProbe, error) {
	var probes []marketingBatchProbe
	for _, node := range nodes {
		if !strings.HasPrefix(node, model.MarketingNodePrefix) {
			continue
		}
		sid, ok := idcodec.Decode(strings.TrimPrefix(node, model.MarketingNodePrefix))
		if !ok {
			out[node] = preResolvedTarget{err: errcode.ErrInvalidParams}
			continue
		}
		sc, err := tx.Script(ctx, b.userID, sid, false)
		if mktNotFound(err) {
			err = errcode.ErrMarketingNotFound
		}
		if err != nil {
			if err := recordTargetError(out, node, err); err != nil {
				return nil, err
			}
			continue
		}
		probes = append(probes, marketingBatchProbe{node, sc})
	}
	return probes, nil
}

// preResolveMarketingTargets 按项目、轮次、脚本主键升序批量加锁并冻结校验结果。
func preResolveMarketingTargets(ctx context.Context, tx repository.GenerationTaskTx, b *taskBatch, nodes []string, out map[string]preResolvedTarget) error {
	hasMarketing := false
	for _, node := range nodes {
		if strings.HasPrefix(node, model.MarketingNodePrefix) {
			hasMarketing = true
			break
		}
	}
	if !hasMarketing {
		return nil
	}
	adapter, ok := tx.(interface{ MarketingTx() repository.MarketingTx })
	if !ok {
		return errcode.ErrInternal
	}
	mtx := adapter.MarketingTx()
	probes, err := probeMarketingTargets(ctx, mtx, b, nodes, out)
	if err != nil {
		return err
	}
	var pids, rids, sids []uint64
	for _, probe := range probes {
		pids = append(pids, probe.script.ProjectID)
		rids = append(rids, probe.script.RoundID)
		sids = append(sids, probe.script.ID)
	}
	ps, err := mtx.LockProjects(ctx, b.userID, batchLockIDs(pids))
	if err != nil {
		return err
	}
	rs, err := mtx.LockRounds(ctx, b.userID, batchLockIDs(rids))
	if err != nil {
		return err
	}
	ss, err := mtx.LockScripts(ctx, b.userID, batchLockIDs(sids))
	if err != nil {
		return err
	}
	for _, probe := range probes {
		out[probe.node] = resolveLockedMarketingTarget(b, probe, ps, rs, ss)
	}
	return nil
}

// resolveLockedMarketingTarget 复核实际锁定的父链、任务种类、重写状态及旁白标记。
func resolveLockedMarketingTarget(b *taskBatch, probe marketingBatchProbe, ps []model.MarketingProject, rs []model.MarketingRound, ss []model.MarketingScript) preResolvedTarget {
	var p *model.MarketingProject
	var r *model.MarketingRound
	var sc *model.MarketingScript
	for i := range ps {
		if ps[i].ID == probe.script.ProjectID {
			p = &ps[i]
		}
	}
	for i := range rs {
		if rs[i].ID == probe.script.RoundID {
			r = &rs[i]
		}
	}
	for i := range ss {
		if ss[i].ID == probe.script.ID {
			sc = &ss[i]
		}
	}
	if p == nil || r == nil || sc == nil {
		return preResolvedTarget{err: errcode.ErrMarketingNotFound}
	}
	if sc.ProjectID != p.ID || sc.RoundID != r.ID || r.ProjectID != p.ID {
		return preResolvedTarget{err: errcode.ErrMarketingNotFound}
	}
	target := preResolvedTarget{script: sc}
	if b.req.Kind != model.KindVideo || b.req.CanvasID != 0 {
		target.err = errcode.ErrInvalidParams
	} else if sc.Status == model.MarketingScriptRewriting {
		target.err = errcode.ErrMarketingScriptBusy
	} else if flags := flagList(sc); len(flags) > 0 {
		target.err = errcode.ErrMarketingScriptBusy.WithMsg("旁白有问题（" + flagSummary(flags) + "），请先修改后再出片")
	}
	return target
}
