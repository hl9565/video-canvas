// 本文件是营销脚本出片：提示词预览、组装任务输入、单份与全部出片。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"

	"video-canvas/internal/marketing"
	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/provider"
	"video-canvas/internal/provider/modelcfg"
)

// assetIDString 把素材 id 转成任务输入里的十进制字符串。
func assetIDString(id uint64) string { return strconv.FormatUint(id, 10) }

// renderInput 组装出片任务的输入：提示词由格式化器拼，op=omni，图片 = 商品图 + 模特图，视频 = 参考视频，generate_audio=true。
func (s *MarketingService) renderInput(c *scriptCtx, snap *provider.Snapshot, params map[string]any, prompt string) (map[string]any, error) {
	var ids []uint64
	if err := json.Unmarshal(c.project.ProductAssetIDs, &ids); err != nil {
		return nil, err
	}
	images := make([]any, 0, len(ids)+1)
	for _, id := range ids {
		images = append(images, assetIDString(id))
	}
	if c.round.ModelAssetID != nil {
		images = append(images, assetIDString(*c.round.ModelAssetID))
	}
	input := map[string]any{"prompt": prompt, "op": modelcfg.OpOmni, "images": images, "generate_audio": true}
	if c.round.RefVideoAssetID != nil {
		input["videos"] = []any{assetIDString(*c.round.RefVideoAssetID)}
	}
	applyMarketingVideoParams(input, snap.Model.Capabilities, params)
	return input, nil
}

// applyMarketingVideoParams 只拷贝模型能力允许且非宿主锁定的视频参数（保留 prompt/op/images/videos/generate_audio）。
func applyMarketingVideoParams(dst map[string]any, caps modelcfg.Capabilities, params map[string]any) {
	for k, v := range params {
		switch k {
		case "prompt", "op", "images", "videos", "generate_audio":
			continue
		}
		if _, ok := caps.Params.Get(k); ok {
			dst[k] = v
		}
	}
}

// validateAndQuoteMarketingVideo 用真实 renderInput 规则校验参数与单生成语义并计算视频积分预留。
func (s *MarketingService) validateAndQuoteMarketingVideo(c *scriptCtx, video *provider.Snapshot, params map[string]any) (int, error) {
	previewInput, err := s.renderInput(c, video, params, "预估")
	if err != nil {
		return 0, err
	}
	caps := video.Model.Capabilities
	norm, errs := modelcfg.ValidateInput(modelcfg.KindVideo, caps, previewInput)
	if len(errs) > 0 {
		return 0, errcode.ErrTaskInput.WithMsg(taskUserVisibleErrPrefix + modelcfg.JoinFieldErrors(errs))
	}
	if modelcfg.FanoutCount(caps, norm) != 1 {
		return 0, errcode.ErrInvalidParams
	}
	return max(modelcfg.Quote(video.Model.Pricing, caps, modelcfg.SpecFromInput(norm, caps.System)), 0), nil
}

// mergeParams 合并出片参数：本轮设置为底，请求里的覆盖。
func mergeParams(base, over map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// Render 在任何LLM调用前校验原始请求意图；候选转写与出片由同一任务保存点提交。
func (s *MarketingService) Render(ctx context.Context, userID, id uint64, req *model.RenderMarketingScriptReq) (*model.MarketingScriptView, error) {
	if req == nil {
		return nil, errcode.ErrInvalidParams
	}
	_, err := s.loadScript(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	tasks, ok := s.tasks.(marketingTaskSubmitter)
	if !ok {
		return nil, errcode.ErrInternal
	}
	intent := MarketingRenderIntent{Operation: "marketing_render", ResourceID: id, VideoModel: req.VideoModel, Params: req.Params, Targets: []uint64{id}}
	hit, err := tasks.CheckMarketingRenderReplay(ctx, userID, req.IdempotencyKey, req.IdempotencyKey, intent, id)
	if err != nil {
		return nil, err
	}
	if hit == nil {
		prepared, err := s.prepareMarketingRender(ctx, userID, id, req, intent, req.IdempotencyKey)
		if err != nil {
			return nil, err
		}
		if err := s.submitPreparedRender(ctx, tasks, userID, req.IdempotencyKey, prepared); err != nil {
			return nil, err
		}
	}
	sc, err := s.repo.Script(ctx, userID, id, false)
	if err != nil {
		return nil, marketingPublicError(err)
	}
	return s.scriptViewByRow(ctx, userID, sc)
}

// prepareMarketingRender 取得一致输入后释放锁，只准备候选稿和报价，不存稿、不收费。
func (s *MarketingService) prepareMarketingRender(ctx context.Context, user, id uint64, req *model.RenderMarketingScriptReq, intent MarketingRenderIntent, rootKey string) (*MarketingRenderCommit, error) {
	c, err := s.snapshotRenderScript(ctx, user, id)
	if err != nil {
		return nil, err
	}
	if flags := flagList(c.sc); len(flags) > 0 {
		return nil, errcode.ErrMarketingScriptBusy.WithMsg("旁白有问题（" + flagSummary(flags) + "），请先修改后再出片")
	}
	st := c.st
	if req.VideoModel != "" && req.VideoModel != st.VideoModel {
		st.VideoModel = req.VideoModel
		if req.Params == nil {
			st.Params = map[string]any{}
		}
	}
	st.Params = mergeParams(st.Params, req.Params)
	video, err := s.videoSnapshot(ctx, st.VideoModel)
	if err != nil {
		return nil, err
	}
	caps := video.Model.Capabilities
	if !caps.Refs.Image.On || !hasOp(caps, modelcfg.OpOmni) {
		return nil, errcode.ErrMarketingVideoModel.WithMsg("所选视频模型不支持全能参考，不能用商品图出片")
	}
	prepared := &MarketingRenderCommit{Intent: intent, RootKey: rootKey, source: c, settings: st, video: video}
	if marketing.NeedsAdapt(caps.PromptFormat) {
		reserve, qerr := s.validateAndQuoteMarketingVideo(c, video, st.Params)
		if qerr != nil {
			return nil, qerr
		}
		prepared.candidate, err = s.prepareAdaptation(ctx, c, st, video, reserve, false, false)
		if err == nil {
			prepared.prompt = prepared.candidate.adaptation.Prompt
		}
	} else {
		var src sourcePrompt
		_, src, err = s.resolveSource(c, st, caps)
		prepared.prompt = src.Text
	}
	if err != nil {
		return nil, err
	}
	if err := validateMarketingAdaptLength(video, prepared.prompt); err != nil {
		return nil, err
	}
	return prepared, nil
}

// submitPreparedRender 只走受信的原子入口；节点错误保持原业务码，失败不会单独留下转写费用。
func (s *MarketingService) submitPreparedRender(ctx context.Context, tasks marketingTaskSubmitter, user uint64, key string, c *MarketingRenderCommit) error {
	input, err := s.renderInput(c.source, c.video, c.settings.Params, c.prompt)
	if err != nil {
		return err
	}
	resp, err := tasks.CreateWithOptions(ctx, user, key, &model.CreateGenerationTaskReq{Kind: model.KindVideo, ModelID: c.video.Model.Key, NodeID: marketingNode(c.source.sc.ID), Input: input}, CreateTaskOptions{MarketingRender: c})
	if err != nil {
		return err
	}
	if resp == nil || len(resp.Items) != 1 {
		return errors.New("出片任务创建未返回唯一结果")
	}
	item := resp.Items[0]
	if item.Error != nil {
		return errcode.New(item.Error.Code, item.Error.Message, item.Error.Status)
	}
	if item.Task == nil {
		return errors.New("出片任务创建结果缺少任务")
	}
	if item.Task.NodeID != marketingNode(c.source.sc.ID) || item.Task.Kind != model.KindVideo {
		return errcode.ErrIdempotencyConflict
	}
	return nil
}

// hasOp 判断模型是否支持某生成方式。
func hasOp(c modelcfg.Capabilities, op string) bool {
	for _, o := range c.Ops {
		if o == op {
			return true
		}
	}
	return false
}

// flagList 解码脚本的软问题。
func flagList(sc *model.MarketingScript) []model.MarketingFlagView {
	var flags []model.MarketingFlagView
	if len(sc.Flags) > 0 {
		_ = json.Unmarshal(sc.Flags, &flags)
	}
	return flags
}

// flagSummary 把软问题概括成给用户看的一句话。
func flagSummary(flags []model.MarketingFlagView) string {
	names := map[string]string{marketing.FlagForbidden: "禁用说法", marketing.FlagClaim: "背书无依据", marketing.FlagBrand: "第三方品牌"}
	seen := map[string]bool{}
	var parts []string
	for _, f := range flags {
		n := names[f.Type]
		if n != "" && !seen[n] {
			seen[n] = true
			parts = append(parts, n)
		}
	}
	return strings.Join(parts, "、")
}

// RenderRound 先校验全部原始目标子键，再最多四路准备候选；每项转写、冻结与入队同保存点。
func (s *MarketingService) RenderRound(ctx context.Context, user, roundID uint64, req *model.RenderMarketingRoundReq) (*model.MarketingRenderAll, error) {
	if req == nil {
		return nil, errcode.ErrInvalidParams
	}
	r, err := s.activeMarketingRound(ctx, user, roundID)
	if err != nil {
		return nil, err
	}
	if _, err := s.repo.Project(ctx, user, r.ProjectID, false); err != nil {
		return nil, marketingPublicError(err)
	}
	scripts, byNode, err := s.roundTasks(ctx, user, roundID)
	if err != nil {
		return nil, err
	}
	targets, err := marketingRoundTargets(scripts, req)
	if err != nil {
		return nil, err
	}
	tasks, ok := s.tasks.(marketingTaskSubmitter)
	if !ok {
		return nil, errcode.ErrInternal
	}
	intent := MarketingRenderIntent{Operation: "marketing_render_round", ResourceID: roundID, Targets: targets}
	hits, err := preflightMarketingRound(ctx, tasks, user, req.IdempotencyKey, intent)
	if err != nil {
		return nil, err
	}
	prepared, failures := s.prepareMarketingRound(ctx, user, scripts, byNode, req.IdempotencyKey, intent, hits)
	out, err := s.commitMarketingRound(ctx, tasks, user, scripts, byNode, req.IdempotencyKey, intent, hits, prepared, failures)
	if err != nil {
		return nil, err
	}
	out.Round, err = s.roundViewByID(ctx, user, roundID)
	return out, err
}

// marketingRoundTargets 绑定客户端保留的原始目标集合；后来新增脚本不能被旧点击带跑。
func marketingRoundTargets(scripts []model.MarketingScript, req *model.RenderMarketingRoundReq) ([]uint64, error) {
	// 零任务不留操作回执，缺少原目标时无法安全证明旧键的目标集合。
	if req.ScriptIDs == nil {
		return nil, errcode.ErrInvalidParams.WithMsg("请刷新并携带本次出片的原始目标清单")
	}
	targets := make([]uint64, 0, len(req.ScriptIDs))
	for _, id := range req.ScriptIDs {
		targets = append(targets, uint64(id))
	}
	slices.Sort(targets)
	known := make(map[uint64]bool, len(scripts))
	for _, sc := range scripts {
		known[sc.ID] = true
	}
	for i, id := range targets {
		if !known[id] || i > 0 && targets[i-1] == id {
			return nil, errcode.ErrInvalidParams
		}
	}
	return targets, nil
}

// marketingRoundKey 使用完整脚本身份派生稳定子键，不能按当前索引重排。
func marketingRoundKey(root string, id uint64) string { return root + ":" + idcodec.Encode(id) }

// preflightMarketingRound 全量预检先于任何并发准备，异载、不交子集或旧空签名立即失败。
func preflightMarketingRound(ctx context.Context, tasks marketingTaskSubmitter, user uint64, key string, intent MarketingRenderIntent) (map[uint64]bool, error) {
	if err := tasks.CheckMarketingRenderRoot(ctx, user, key, intent); err != nil {
		return nil, err
	}
	hits := map[uint64]bool{}
	for _, id := range intent.Targets {
		hit, err := tasks.CheckMarketingRenderReplay(ctx, user, key, marketingRoundKey(key, id), intent, id)
		if err != nil {
			return nil, err
		}
		hits[id] = hit != nil
	}
	return hits, nil
}

// prepareMarketingRound 最多四路调用模型，只为原目标集合中未建成且仍可出的脚本准备。
func (s *MarketingService) prepareMarketingRound(ctx context.Context, user uint64, scripts []model.MarketingScript, byNode map[string][]model.GenerationTask, key string, intent MarketingRenderIntent, hits map[uint64]bool) (map[uint64]*MarketingRenderCommit, map[uint64]error) {
	prepared := map[uint64]*MarketingRenderCommit{}
	failures := map[uint64]error{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, sc := range scripts {
		if !slices.Contains(intent.Targets, sc.ID) || hits[sc.ID] {
			continue
		}
		status, _ := deriveScriptStatus(sc.Status, byNode[marketingNode(sc.ID)])
		if batchSkipReason(&sc, status, false) != "" {
			continue
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			p, err := s.prepareMarketingRender(ctx, user, sc.ID, &model.RenderMarketingScriptReq{IdempotencyKey: marketingRoundKey(key, sc.ID)}, intent, key)
			mu.Lock()
			defer mu.Unlock()
			prepared[sc.ID], failures[sc.ID] = p, err
		})
	}
	wg.Wait()
	return prepared, failures
}

// commitMarketingRound 逐项原子提交；部分命中只补原始缺失项，完整命中不调模型也不重复扣费。
func (s *MarketingService) commitMarketingRound(ctx context.Context, tasks marketingTaskSubmitter, user uint64, scripts []model.MarketingScript, byNode map[string][]model.GenerationTask, key string, intent MarketingRenderIntent, hits map[uint64]bool, prepared map[uint64]*MarketingRenderCommit, failures map[uint64]error) (*model.MarketingRenderAll, error) {
	out := &model.MarketingRenderAll{Skipped: []model.MarketingSkipped{}}
	created, noCredits := 0, false
	for _, sc := range scripts {
		if hits[sc.ID] {
			created++
			continue
		}
		reason, short, err := s.commitMarketingRoundItem(ctx, tasks, user, &sc, byNode, key, intent, prepared, failures, noCredits)
		if err != nil {
			return nil, err
		}
		noCredits = noCredits || short
		if reason == "" {
			created++
		}
		if reason != "" {
			out.Skipped = append(out.Skipped, model.MarketingSkipped{ScriptID: hexID(sc.ID), Reason: reason})
		}
	}
	if noCredits && created == 0 {
		return nil, errcode.ErrInsufficientCredits
	}
	return out, nil
}

// commitMarketingRoundItem 将一项候选提交或业务错误转换为跳过原因，系统错误中止整轮。
func (s *MarketingService) commitMarketingRoundItem(ctx context.Context, tasks marketingTaskSubmitter, user uint64, sc *model.MarketingScript, byNode map[string][]model.GenerationTask, key string, intent MarketingRenderIntent, prepared map[uint64]*MarketingRenderCommit, failures map[uint64]error, noCredits bool) (string, bool, error) {
	status, _ := deriveScriptStatus(sc.Status, byNode[marketingNode(sc.ID)])
	if reason := batchSkipReason(sc, status, noCredits); reason != "" {
		return reason, false, nil
	}
	if !slices.Contains(intent.Targets, sc.ID) {
		return "不在本次出片目标中", false, nil
	}
	err := failures[sc.ID]
	if err == nil {
		if prepared[sc.ID] == nil {
			return "", false, errcode.ErrMarketingConflict
		}
		err = s.submitPreparedRender(ctx, tasks, user, marketingRoundKey(key, sc.ID), prepared[sc.ID])
	}
	if errors.Is(err, errcode.ErrIdempotencyConflict) {
		return "", false, err
	}
	if err != nil {
		return skipOf(err)
	}
	return "", false, nil
}

// roundTasks 返回轮次的脚本和它们的出片任务（按节点 id 分组，时间正序）。
func (s *MarketingService) roundTasks(ctx context.Context, userID, roundID uint64) ([]model.MarketingScript, map[string][]model.GenerationTask, error) {
	scripts, err := s.repo.RoundScripts(ctx, roundID)
	if err != nil {
		return nil, nil, err
	}
	nodes := make([]string, 0, len(scripts))
	for i := range scripts {
		nodes = append(nodes, marketingNode(scripts[i].ID))
	}
	tasks, err := s.repo.ScriptTasks(ctx, userID, nodes)
	if err != nil {
		return nil, nil, err
	}
	byNode := map[string][]model.GenerationTask{}
	for _, t := range tasks {
		byNode[t.NodeID] = append(byNode[t.NodeID], t)
	}
	return scripts, byNode, nil
}

// batchSkipReason 返回全部出片时这份脚本被跳过的原因；能出片返回空串。
func batchSkipReason(sc *model.MarketingScript, status string, noCredits bool) string {
	switch {
	case noCredits:
		return "积分不足"
	case status == model.MarketingScriptRewriting:
		return "正在重写"
	case status == model.MarketingScriptRendering:
		return "正在出片"
	case status == model.MarketingScriptDone:
		return "已出片"
	case len(flagList(sc)) > 0:
		return "旁白有问题，请先修改"
	}
	return ""
}

// skipOf 把批量出片里一份的错误转成跳过原因：业务错误作为原因（积分不足时 noCredits 为真），其他错误原样返回。
func skipOf(err error) (reason string, noCredits bool, _ error) {
	var biz *errcode.Error
	if errors.As(err, &biz) {
		return biz.Msg, biz.Code == errcode.ErrInsufficientCredits.Code, nil
	}
	return "", false, err
}
