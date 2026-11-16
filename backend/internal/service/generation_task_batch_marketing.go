package service

import (
	"context"
	"strings"

	"video-canvas/internal/marketing"
	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/jsonhash"
	"video-canvas/internal/provider"
	"video-canvas/internal/provider/modelcfg"
	"video-canvas/internal/repository"
)

const marketingIntentSigPrefix = "mkt:v1:"

// MarketingRenderIntent 是受信营销入口的原始请求意图，参数nil与空对象必须保持区别。
type MarketingRenderIntent struct {
	Operation  string
	ResourceID uint64
	VideoModel string
	Params     map[string]any
	Targets    []uint64
}

// MarketingRenderCommit 把锁外执行快照与候选稿交给生成任务的同事务保存点；HTTP请求不能构造此选项。
type MarketingRenderCommit struct {
	Intent    MarketingRenderIntent
	RootKey   string
	source    *scriptCtx
	settings  model.MarketingSettings
	video     *provider.Snapshot
	candidate *marketingAdaptCandidate
	prompt    string
}

// marketingTaskSubmitter 是营销服务必须使用的受信重放和原子提交能力。
type marketingTaskSubmitter interface {
	CheckMarketingRenderRoot(context.Context, uint64, string, MarketingRenderIntent) error
	CheckMarketingRenderReplay(context.Context, uint64, string, string, MarketingRenderIntent, uint64) (*model.GenerationTaskView, error)
	CreateWithOptions(context.Context, uint64, string, *model.CreateGenerationTaskReq, CreateTaskOptions) (*model.CreateGenerationTaskResp, error)
}

type marketingRootRepo interface {
	LockMarketingRoot(ctx context.Context, userID uint64, rootKey string) error
	ListMarketingRootTasks(ctx context.Context, userID uint64, rootKey string) ([]model.GenerationTask, error)
}

func validateMarketingRootKey(key string) error {
	if key == "" || strings.HasPrefix(key, InternalDramaFrameRunKeyPrefix) || len(key) > maxIdempotencyKeyLen-len("#9") {
		return errcode.ErrInvalidParams
	}
	return nil
}

// marketingRenderIntentSig 按根键与原始意图生成签名；长度固定不超过 request_intent_sig 列宽 varchar(64)。
func marketingRenderIntentSig(user uint64, rootKey string, intent MarketingRenderIntent) (string, error) {
	if intent.Operation != "marketing_render" && intent.Operation != "marketing_render_round" {
		return "", errcode.ErrInvalidParams
	}
	sig, err := jsonhash.CanonicalSHA256(struct {
		User   uint64
		Key    string
		Intent MarketingRenderIntent
	}{user, rootKey, intent})
	if err != nil {
		return "", err
	}
	return marketingIntentSigPrefix + sig[:64-len(marketingIntentSigPrefix)], nil
}

// CheckMarketingRenderRoot 在任何模型副作用前核验同根键下是否已存在不兼容目标集合的任务。
func (s *GenerationTaskService) CheckMarketingRenderRoot(ctx context.Context, user uint64, rootKey string, intent MarketingRenderIntent) error {
	if err := validateMarketingRootKey(rootKey); err != nil {
		return err
	}
	sig, err := marketingRenderIntentSig(user, rootKey, intent)
	if err != nil {
		return err
	}
	return s.validateMarketingRootTasks(ctx, s.repo, user, rootKey, sig)
}

// CheckMarketingRenderReplay 在任何模型副作用前校验完整营销子键与根键意图。
func (s *GenerationTaskService) CheckMarketingRenderReplay(ctx context.Context, user uint64, rootKey, key string, intent MarketingRenderIntent, script uint64) (*model.GenerationTaskView, error) {
	if err := validateMarketingRootKey(rootKey); err != nil {
		return nil, err
	}
	if err := validateMarketingRootKey(key); err != nil {
		return nil, err
	}
	if rootKey == key {
		if err := s.CheckMarketingRenderRoot(ctx, user, rootKey, intent); err != nil {
			return nil, err
		}
	}
	sig, err := marketingRenderIntentSig(user, rootKey, intent)
	if err != nil {
		return nil, err
	}
	hit, err := s.findIdempotent(ctx, s.repo, user, key)
	if err != nil || hit == nil {
		return nil, err
	}
	if err := validateMarketingReplay(hit, user, script, sig); err != nil {
		return nil, err
	}
	return taskView(hit), nil
}

// validateMarketingReplay 不借历史生成参数猜测营销意图，只有受信的完整签名与目标节点可以重放。
func validateMarketingReplay(hit *model.GenerationTask, user, script uint64, sig string) error {
	if hit.UserID != user || hit.RequestIntentSig != sig || hit.IsTest || hit.AgentRunID != 0 || hit.Kind != model.KindVideo || hit.CanvasProjectID != nil || hit.NodeID != marketingNode(script) || hit.RequestBatchIndex != 0 {
		return errcode.ErrIdempotencyConflict
	}
	return nil
}

func (s *GenerationTaskService) validateMarketingRootTasks(ctx context.Context, q any, user uint64, rootKey, sig string) error {
	mr, ok := q.(marketingRootRepo)
	if !ok {
		return nil
	}
	rows, err := mr.ListMarketingRootTasks(ctx, user, rootKey)
	if err != nil {
		return err
	}
	for i := range rows {
		hit := &rows[i]
		if hit.IdempotencyKey != rootKey {
			suffix, matched := strings.CutPrefix(hit.IdempotencyKey, rootKey+":")
			if !matched {
				continue
			}
			if _, ok := idcodec.Decode(suffix); !ok {
				continue
			}
		}
		if hit.UserID != user || hit.RequestIntentSig != sig || hit.IsTest || hit.AgentRunID != 0 || hit.Kind != model.KindVideo || hit.CanvasProjectID != nil || hit.RequestBatchIndex != 0 {
			return errcode.ErrIdempotencyConflict
		}
	}
	return nil
}

// marketingCommitIntent 使用根键与完整意图生成签名，使同轮多个子任务共享同一根意图签名。
func marketingCommitIntent(user uint64, key string, req *model.CreateGenerationTaskReq, nodes []string, c *MarketingRenderCommit) (string, error) {
	if c.source == nil || c.source.sc.UserID != user || len(nodes) != 1 || nodes[0] != marketingNode(c.source.sc.ID) || req.Kind != model.KindVideo || req.CanvasID != 0 || req.AgentRunID != 0 {
		return "", errcode.ErrInvalidParams
	}
	rootKey := c.RootKey
	if rootKey == "" {
		rootKey = key
	}
	return marketingRenderIntentSig(user, rootKey, c.Intent)
}

func (s *GenerationTaskService) lockAndCheckMarketingRoot(ctx context.Context, tx repository.GenerationTaskTx, b *taskBatch) error {
	if b.opts.MarketingRender == nil {
		return nil
	}
	rootKey := b.opts.MarketingRender.RootKey
	if rootKey == "" {
		rootKey = b.key
	}
	if mr, ok := tx.(marketingRootRepo); ok {
		if err := mr.LockMarketingRoot(ctx, b.userID, rootKey); err != nil {
			return err
		}
	}
	return s.validateMarketingRootTasks(ctx, tx, b.userID, rootKey, b.sig)
}

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
	p, r, sc := findLockedMarketingEntities(probe, ps, rs, ss)
	if p == nil || r == nil || sc == nil || sc.ProjectID != p.ID || sc.RoundID != r.ID || r.ProjectID != p.ID {
		return preResolvedTarget{err: errcode.ErrMarketingNotFound}
	}
	target := preResolvedTarget{script: sc}
	if sc.Status == model.MarketingScriptRewriting {
		target.err = errcode.ErrMarketingScriptBusy
		return target
	}
	if r.Status == model.MarketingRoundCanceled || r.Status == model.MarketingRoundScripting {
		target.err = errcode.ErrMarketingConflict
		return target
	}
	if b.req.Kind != model.KindVideo || b.req.CanvasID != 0 {
		target.err = errcode.ErrInvalidParams
	} else if flags := flagList(sc); len(flags) > 0 {
		target.err = errcode.ErrMarketingScriptBusy.WithMsg("旁白有问题（" + flagSummary(flags) + "），请先修改后再出片")
	}
	if target.err == nil && b.opts.MarketingRender != nil {
		target.marketing, target.err = resolveMarketingRenderLocked(b.opts.MarketingRender, p, r, sc)
	}
	return target
}

func findLockedMarketingEntities(probe marketingBatchProbe, ps []model.MarketingProject, rs []model.MarketingRound, ss []model.MarketingScript) (*model.MarketingProject, *model.MarketingRound, *model.MarketingScript) {
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
	return p, r, sc
}

// marketingLockedRender 是父链预锁阶段确定的实际稿和应收额，积分锁之后不再获取父锁。
type marketingLockedRender struct {
	script     *model.MarketingScript
	format     string
	adaptation model.MarketingAdaptation
	prompt     string
	credits    int
	note       string
	write      bool
}

// resolveMarketingRenderLocked 二验执行代、设置、素材及内容，若有有效赢家则采用其实际稿并免转写费。
func resolveMarketingRenderLocked(c *MarketingRenderCommit, p *model.MarketingProject, r *model.MarketingRound, sc *model.MarketingScript) (*marketingLockedRender, error) {
	st, err := settingsOf(r)
	if err != nil {
		return nil, err
	}
	current := &scriptCtx{sc: sc, round: r, project: p, st: st}
	if err := sameMarketingContent(c.source, current); err != nil {
		return nil, err
	}
	result := &marketingLockedRender{script: sc, prompt: c.prompt}
	if c.candidate == nil {
		return result, nil
	}
	a, cost, write, err := validateAdaptationCandidate(c.candidate, current, false)
	if err != nil {
		return nil, err
	}
	// 自动模型稿必须仍满足当前输入，手改稿按既有契约允许自由文案，但也须重验长度。
	if !a.Edited {
		if err := marketing.ValidateAdapt(c.candidate.input, a.Prompt); err != nil {
			return nil, errcode.ErrMarketingConflict
		}
	}
	if err := validateMarketingAdaptLength(c.video, a.Prompt); err != nil {
		return nil, err
	}
	f := c.video.Model.Capabilities.PromptFormat
	result.format, result.adaptation, result.prompt = f, a, a.Prompt
	result.credits, result.note, result.write = cost, c.candidate.note, write
	return result, nil
}

// marketingAdaptCredits 是保存点内需要和视频冻结额一起核算的新增转写费。
func marketingAdaptCredits(target preResolvedTarget) int {
	if target.marketing == nil {
		return 0
	}
	return target.marketing.credits
}

// commitBatchMarketingAdapt 只复用当前原生保存点，不另开事务；入队或任务插入失败会一并回滚稿和转写流水。
func commitBatchMarketingAdapt(ctx context.Context, tx repository.GenerationTaskTx, target preResolvedTarget) error {
	c := target.marketing
	if c == nil || !c.write {
		return nil
	}
	adapter, ok := tx.(interface{ MarketingTx() repository.MarketingTx })
	if !ok {
		return errcode.ErrInternal
	}
	mtx := adapter.MarketingTx()
	if err := writeMarketingAdaptation(ctx, mtx, c.script, c.format, c.adaptation); err != nil {
		return err
	}
	return chargeMarketingAdapt(ctx, mtx, c.script.UserID, c.script.ID, c.credits, c.note)
}

// marketingBatchItemCredits 在保存点内重验模型快照，用实际赢家稿重新校验输入并报价。
func (s *GenerationTaskService) marketingBatchItemCredits(ctx context.Context, tx repository.GenerationTaskTx, b *taskBatch, target preResolvedTarget) (*provider.Snapshot, map[string]any, int, error) {
	if target.marketing == nil {
		return nil, nil, 0, errcode.ErrMarketingConflict
	}
	snap, err := s.snapshotForTx(ctx, tx, b.req)
	if err != nil {
		return nil, nil, 0, err
	}
	before, err := jsonhash.CanonicalSHA256(b.opts.MarketingRender.video)
	if err != nil {
		return nil, nil, 0, err
	}
	after, err := jsonhash.CanonicalSHA256(snap)
	if err != nil {
		return nil, nil, 0, err
	}
	if before != after {
		return nil, nil, 0, errcode.ErrMarketingConflict
	}
	raw := cloneMap(b.input)
	raw["prompt"] = target.marketing.prompt
	assets := s.assets
	if b.assetCache != nil {
		assets = b.assetCache.txView(tx)
	}
	input, err := s.prepareInputWithAssets(ctx, assets, b.userID, snap, raw)
	if err != nil {
		return nil, nil, 0, err
	}
	if modelcfg.FanoutCount(snap.Model.Capabilities, input) != 1 {
		return nil, nil, 0, errcode.ErrInvalidParams
	}
	unit := max(modelcfg.Quote(snap.Model.Pricing, snap.Model.Capabilities, modelcfg.SpecFromInput(input, snap.Model.Capabilities.System)), 0)
	return snap, input, unit, nil
}
