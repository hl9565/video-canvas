// 本文件是出片前的提示词转写：H3 这类模型有自己的写法（<Picture N> 标签、六段式），
// 原稿（Seedance 写法，代码拼出）交给文本模型转写，输出不合格时带校验反馈重写一次。
// 转写稿按格式存在脚本上，原稿指纹变了就过期，出片时自动重新转写；转写稿可以手改。
// 扣费按文本模型实际用量，和转写稿写回在同一个事务里：转写失败不扣。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"go.uber.org/zap"

	"video-canvas/internal/llmgateway"
	"video-canvas/internal/marketing"
	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/jsonhash"
	"video-canvas/internal/pkg/logger"
	"video-canvas/internal/provider"
	"video-canvas/internal/provider/modelcfg"
	"video-canvas/internal/repository"
)

// adaptSource 返回脚本的转写输入和原稿：原稿是不删减的 Seedance 写法（手改稿优先），上限取目标视频模型的提示词上限。
// 原稿手改过时镜头表以原稿为准，不再给脚本的镜头表。
func (s *MarketingService) adaptSource(c *scriptCtx, st model.MarketingSettings, video modelcfg.Capabilities) (marketing.AdaptInput, sourcePrompt, error) {
	in, src, err := s.resolveSource(c, st, video)
	if err != nil {
		return marketing.AdaptInput{}, src, err
	}
	var shots []marketing.AdaptShot
	seconds := float64(in.Duration)
	for i, sh := range in.Script.Shots {
		if !src.Edited {
			shot := marketing.AdaptShot{No: i + 1, Narration: strings.TrimSpace(sh.Narration)}
			if i > 0 {
				shot.Cut = marketing.CutTime(sh.Start)
			}
			shots = append(shots, shot)
		}
		if in.Duration <= 0 {
			seconds = sh.End
		}
	}
	return marketing.AdaptInput{Source: src.Text, Seconds: seconds, ProductImages: in.ProductImages, HasModel: in.HasModel,
		HasRefVideo: in.HasRefVideo, Shots: shots, MaxLen: video.Prompt.MaxLength}, src, nil
}

// adaptationsOf 解出脚本上的全部转写稿；空列当作没有，返回非 nil 的空表。
func adaptationsOf(sc *model.MarketingScript) (map[string]model.MarketingAdaptation, error) {
	all := map[string]model.MarketingAdaptation{}
	if len(sc.Adaptations) == 0 {
		return all, nil
	}
	if err := json.Unmarshal(sc.Adaptations, &all); err != nil {
		return nil, err
	}
	if all == nil {
		all = map[string]model.MarketingAdaptation{}
	}
	return all, nil
}

// freshAdaptation 只接受完整输入指纹匹配的稿，旧空指纹保留展示，显式重转后升级。
func freshAdaptation(sc *model.MarketingScript, promptFormat, hash string) (model.MarketingAdaptation, bool) {
	all, err := adaptationsOf(sc)
	if err != nil {
		return model.MarketingAdaptation{}, false
	}
	a, ok := all[promptFormat]
	return a, ok && hash != "" && a.InputHash == hash
}

// marketingAdaptCandidate 是锁外模型阶段的候选结果，不授予写入或扣费权限。
type marketingAdaptCandidate struct {
	source     *scriptCtx
	settings   model.MarketingSettings
	video      *provider.Snapshot
	input      marketing.AdaptInput
	hash       string
	adaptation model.MarketingAdaptation
	credits    int
	note       string
	cached     bool
}

// adaptationInputHash 在完整转写输入外绑定实际参考素材顺序、有效模型能力及参数。
func adaptationInputHash(c *scriptCtx, st model.MarketingSettings, caps modelcfg.Capabilities, in marketing.AdaptInput) (string, error) {
	var ids []uint64
	if err := json.Unmarshal(c.project.ProductAssetIDs, &ids); err != nil {
		return "", err
	}
	if c.round.ModelAssetID != nil {
		ids = append(ids, *c.round.ModelAssetID)
	}
	if c.round.RefVideoAssetID != nil {
		ids = append(ids, *c.round.RefVideoAssetID)
	}
	base := marketing.AdaptInputHash(caps.PromptFormat, in, ids...)
	if base == "" {
		return "", errcode.ErrMarketingConflict
	}
	return jsonhash.CanonicalSHA256(struct {
		Input, Model string
		Params       map[string]any
		Caps         modelcfg.Capabilities
	}{base, st.VideoModel, st.Params, caps})
}

// prepareAdaptation 只准备候选；自动出片不开户、不存稿、不扣费，但调 LLM 前须先做只读余额预检。
func (s *MarketingService) prepareAdaptation(ctx context.Context, c *scriptCtx, st model.MarketingSettings, video *provider.Snapshot, reserve int, force, ensureAccount bool) (*marketingAdaptCandidate, error) {
	in, _, err := s.adaptSource(c, st, video.Model.Capabilities)
	if err != nil {
		return nil, err
	}
	hash, err := adaptationInputHash(c, st, video.Model.Capabilities, in)
	if err != nil {
		return nil, err
	}
	all, err := adaptationsOf(c.sc)
	if err != nil {
		return nil, err
	}
	f := video.Model.Capabilities.PromptFormat
	a := all[f]
	candidate := &marketingAdaptCandidate{source: c, settings: st, video: video, input: in, hash: hash, adaptation: a}
	if _, ok := freshAdaptation(c.sc, f, hash); ok && !force {
		candidate.cached = true
		return candidate, nil
	}
	// 旧库手改稿无法证明素材身份，必须让用户显式确认重转，不能后台覆盖收费。
	if a.Edited && a.InputHash == "" && !force {
		return nil, errcode.ErrMarketingConflict
	}
	out, cost, err := s.callAdapt(ctx, c.sc.UserID, st.TextModel, in, reserve, ensureAccount)
	if err != nil {
		return nil, err
	}
	candidate.adaptation = model.MarketingAdaptation{Prompt: out, SourceHash: marketing.SourceHash(in.Source), InputHash: hash, Version: a.Version, UpdatedAt: s.now()}
	candidate.credits, candidate.note = cost, "营销出片转写 · H3 · "+clipRunes(c.sc.Title, 40)
	return candidate, nil
}

// validateAdaptationCandidate 持锁重验来源与稿件版本；自动请求必须回传库中赢家的实际稿和版本。
func validateAdaptationCandidate(c *marketingAdaptCandidate, current *scriptCtx, force bool) (model.MarketingAdaptation, int, bool, error) {
	if err := sameMarketingContent(c.source, current); err != nil {
		return model.MarketingAdaptation{}, 0, false, err
	}
	f := c.video.Model.Capabilities.PromptFormat
	all, err := adaptationsOf(current.sc)
	if err != nil {
		return model.MarketingAdaptation{}, 0, false, err
	}
	old := all[f]
	if winner, ok := freshAdaptation(current.sc, f, c.hash); ok && !force {
		if err := validateMarketingAdaptLength(c.video, winner.Prompt); err != nil {
			return model.MarketingAdaptation{}, 0, false, err
		}
		return winner, 0, false, nil
	}
	if old.Version != c.adaptation.Version || c.cached {
		return model.MarketingAdaptation{}, 0, false, errcode.ErrMarketingConflict
	}
	next := c.adaptation
	next.Version = old.Version + 1
	return next, c.credits, true, nil
}

// validateMarketingAdaptLength 对最终采用的稿重新检查模型长度限制，包括并发赢家和手改稿。
func validateMarketingAdaptLength(video *provider.Snapshot, prompt string) error {
	if limit := video.Model.Capabilities.Prompt.MaxLength; limit > 0 && utf8.RuneCountInString(prompt) > limit {
		return errcode.ErrMarketingPromptLong
	}
	return nil
}

// ensureAdapted 是显式转写的事务入口；本机限重仅减少供应商调用，跨实例靠父锁与版本CAS。
func (s *MarketingService) ensureAdapted(ctx context.Context, c *scriptCtx, video *provider.Snapshot, force bool) (model.MarketingAdaptation, error) {
	key := marketingNode(c.sc.ID) + ":" + video.Model.Capabilities.PromptFormat
	if _, busy := s.adapting.LoadOrStore(key, struct{}{}); busy {
		return model.MarketingAdaptation{}, errcode.ErrMarketingScriptBusy
	}
	defer s.adapting.Delete(key)
	st := c.st
	st.VideoModel = video.Model.Key
	candidate, err := s.prepareAdaptation(ctx, c, st, video, 0, force, true)
	if err != nil {
		return model.MarketingAdaptation{}, err
	}
	return s.saveAdaptation(ctx, candidate, force)
}

// callAdapt 调文本模型转写，最多两次：第一次不合格时把校验原因交给模型修正；只收通过校验那一次的钱。
func (s *MarketingService) callAdapt(ctx context.Context, userID uint64, textModel string, in marketing.AdaptInput, reserve int, ensureAccount ...bool) (string, int, error) {
	snap, err := s.pickTextModel(ctx, textModel, needAny)
	if err != nil {
		return "", 0, err
	}
	createAcc := len(ensureAccount) == 0 || ensureAccount[0]
	if err := s.checkAdaptBalance(ctx, userID, snap, in, reserve, createAcc); err != nil {
		return "", 0, err
	}
	callCtx, cancel := context.WithTimeout(ctx, adaptTimeout)
	defer cancel()
	for range 2 {
		prompt, err := marketing.AdaptPrompt(in)
		if err != nil {
			return "", 0, err
		}
		res, err := s.caller.Complete(callCtx, snap, []llmgateway.Message{{Role: "user", Text: prompt}})
		if err != nil {
			// 上游返回了错误就带出原文；没有原文（网络断开、超时）用统一文案
			logger.Ctx(ctx).Warn("营销出片转写调用失败", zap.String("model", snap.Model.Key), zap.Error(err))
			msg := "文本模型没有返回结果，请稍后重试"
			if up := upstreamText(err); up != "" {
				msg = up
			}
			return "", 0, errcode.ErrMarketingAdaptFailed.WithMsg("转写提示词失败：" + msg)
		}
		out, err := marketing.DecodeAdapt(res.Text)
		if err == nil {
			if err = marketing.ValidateAdapt(in, out); err == nil {
				return out, adaptCost(snap, utf8.RuneCountInString(prompt), res), nil
			}
		}
		in.ValidationError = err.Error()
	}
	return "", 0, errcode.ErrMarketingAdaptFailed
}

// adaptEstimate 按转写提示词字数预估一次转写的积分。
func adaptEstimate(snap *provider.Snapshot, in marketing.AdaptInput) int {
	prompt, err := marketing.AdaptPrompt(in)
	if err != nil {
		return 0
	}
	return max(modelcfg.Quote(snap.Model.Pricing, snap.Model.Capabilities, modelcfg.Spec{PromptChars: utf8.RuneCountInString(prompt), Params: map[string]any{}}), 0)
}

// checkAdaptBalance 调模型前检查可用积分（余额减冻结）够不够「转写预估 + reserve」；不够返回积分不足，不调模型。
// ensureAccount 为 true 时按新账户初始积分惰性建户；自动出片路径为只读预检，绝不开户。
func (s *MarketingService) checkAdaptBalance(ctx context.Context, userID uint64, snap *provider.Snapshot, in marketing.AdaptInput, reserve int, ensureAccount bool) error {
	need := adaptEstimate(snap, in) + reserve
	if need <= 0 {
		return nil
	}
	initial, err := s.initialCredits(ctx)
	if err != nil {
		return err
	}
	var avail int
	err = s.repo.WithTx(ctx, func(tx repository.MarketingTx) error {
		if ensureAccount {
			if e := tx.EnsureCredit(ctx, userID, initial); e != nil {
				return e
			}
		}
		credit, e := tx.LockCredit(ctx, userID)
		if errors.Is(e, repository.ErrNotFound) && !ensureAccount {
			avail = initial
			return nil
		}
		if e != nil {
			return e
		}
		avail = credit.Balance - credit.Frozen
		return nil
	})
	if err != nil {
		return err
	}
	if avail < need {
		return errcode.ErrInsufficientCredits
	}
	return nil
}

// saveAdaptation 在父链锁内复核在途输入、目标版本和模型，存稿与扣费同事务提交。
func (s *MarketingService) saveAdaptation(ctx context.Context, c *marketingAdaptCandidate, force bool) (model.MarketingAdaptation, error) {
	var winner model.MarketingAdaptation
	err := s.repo.WithTx(ctx, func(tx repository.MarketingTx) error {
		current, err := lockedScriptContext(ctx, tx, c.source.sc.UserID, c.source.sc)
		if err != nil {
			return err
		}
		if err := s.recheckMarketingVideo(ctx, c.video); err != nil {
			return err
		}
		var cost int
		var write bool
		winner, cost, write, err = validateAdaptationCandidate(c, current, force)
		if err != nil {
			return err
		}
		if !write {
			return nil
		}
		if err := writeMarketingAdaptation(ctx, tx, current.sc, c.video.Model.Capabilities.PromptFormat, winner); err != nil {
			return err
		}
		return chargeMarketingAdapt(ctx, tx, current.sc.UserID, current.sc.ID, cost, c.note)
	})
	return winner, err
}

// writeMarketingAdaptation 在已有父链与脚本锁内按格式合并JSON，保留其他格式的稿。
func writeMarketingAdaptation(ctx context.Context, tx repository.MarketingTx, sc *model.MarketingScript, f string, a model.MarketingAdaptation) error {
	all, err := adaptationsOf(sc)
	if err != nil {
		return err
	}
	all[f] = a
	raw, err := json.Marshal(all)
	if err != nil {
		return err
	}
	return tx.UpdateScript(ctx, sc.ID, map[string]any{"adaptations": raw})
}

// recheckMarketingVideo 在提交前确认有效模型配置没有在LLM期间变化。
func (s *MarketingService) recheckMarketingVideo(ctx context.Context, video *provider.Snapshot) error {
	current, err := s.videoSnapshot(ctx, video.Model.Key)
	if err != nil {
		return err
	}
	before, err := jsonhash.CanonicalSHA256(video)
	if err != nil {
		return err
	}
	after, err := jsonhash.CanonicalSHA256(current)
	if err != nil {
		return err
	}
	if before != after {
		return errcode.ErrMarketingConflict
	}
	return nil
}

// chargeMarketingAdapt 扣转写费：实扣封顶在可用积分，不扣成负数、不动进行中任务冻结的积分；
// 和短剧转写一样成对写一条冻结流水和一条结算流水，后台对账等式保持成立。应收为 0 或账户不存在时不写。
func chargeMarketingAdapt(ctx context.Context, tx repository.MarketingTx, userID, scriptID uint64, credits int, note string) error {
	if credits <= 0 {
		return nil
	}
	credit, err := tx.LockCredit(ctx, userID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	charged := min(credits, max(credit.Balance-credit.Frozen, 0))
	if charged == 0 {
		return nil
	}
	for _, typ := range []string{model.LedgerFreeze, model.LedgerSettle} {
		if _, err := tx.InsertLedger(ctx, &model.CreditLedger{UserID: userID, MarketingScriptID: &scriptID, Type: typ, Amount: charged, Note: note}); err != nil {
			return err
		}
	}
	return tx.AddCredit(ctx, userID, -charged, 0)
}

// adaptPromptView 组装需要转写的模型的提示词预览：原稿（含是否手改）、转写稿（含是否过期）、转写一次的预估积分。
func (s *MarketingService) adaptPromptView(ctx context.Context, c *scriptCtx, video *provider.Snapshot) (*model.MarketingPrompt, error) {
	f := video.Model.Capabilities.PromptFormat
	in, src, err := s.adaptSource(c, c.st, video.Model.Capabilities)
	if err != nil {
		return nil, err
	}
	st := c.st
	st.VideoModel = video.Model.Key
	hash, err := adaptationInputHash(c, st, video.Model.Capabilities, in)
	if err != nil {
		return nil, err
	}
	v := &model.MarketingPrompt{Version: c.sc.Revision, InputHash: hash, Format: f, Prompt: in.Source, Edited: src.Edited, Stale: src.Stale}
	all, err := adaptationsOf(c.sc)
	if err != nil {
		return nil, err
	}
	if a, ok := all[f]; ok {
		v.Adaptation = &model.MarketingAdaptationView{Prompt: a.Prompt, Version: a.Version, InputHash: a.InputHash, Stale: a.InputHash == "" || a.InputHash != hash, Edited: a.Edited, UpdatedAt: a.UpdatedAt}
	}
	if text, err := s.pickTextModel(ctx, c.st.TextModel, needAny); err == nil {
		v.AdaptCredits = adaptEstimate(text, in)
	}
	return v, nil
}

// Adapt 手动转写一份脚本的出片提示词（不传视频模型用本轮的），已有转写稿也重新转写。
// 模型不需要转写 10001；脚本重写中 68006；积分不足 40001；转写失败 68018。
func (s *MarketingService) Adapt(ctx context.Context, userID, id uint64, req *model.AdaptMarketingScriptReq) (*model.MarketingPrompt, error) {
	if req == nil {
		return nil, errcode.ErrMarketingConflict
	}
	if err := requireMarketingVersion(req.ExpectedVersion); err != nil {
		return nil, err
	}
	c, err := s.snapshotScript(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	video, err := s.promptVideo(ctx, c, req.VideoModel)
	if err != nil {
		return nil, err
	}
	f := video.Model.Capabilities.PromptFormat
	if !marketing.NeedsAdapt(f) {
		return nil, errcode.ErrInvalidParams.WithMsg("这个视频模型直接用原稿出片，不需要转写")
	}
	all, err := adaptationsOf(c.sc)
	if err != nil {
		return nil, err
	}
	if all[f].Version != *req.ExpectedVersion {
		return nil, errcode.ErrMarketingConflict
	}
	if _, err := s.ensureAdapted(ctx, c, video, true); err != nil {
		return nil, err
	}
	return s.Prompt(ctx, userID, id, req.VideoModel)
}

// EditAdaptation 用预览版本编辑目标格式；来源指纹不变，持锁拒绝取消、换代与陈旧覆盖。
func (s *MarketingService) EditAdaptation(ctx context.Context, userID, id uint64, req *model.EditMarketingAdaptationReq) (*model.MarketingPrompt, error) {
	if req == nil {
		return nil, errcode.ErrMarketingConflict
	}
	if err := requireMarketingVersion(req.ExpectedVersion); err != nil {
		return nil, err
	}
	probe, err := s.repo.Script(ctx, userID, id, false)
	if err != nil {
		return nil, marketingPublicError(err)
	}
	err = s.repo.WithTx(ctx, func(tx repository.MarketingTx) error {
		c, err := lockedScriptContext(ctx, tx, userID, probe)
		if err != nil {
			return err
		}
		all, err := adaptationsOf(c.sc)
		if err != nil {
			return err
		}
		a, ok := all[req.Format]
		if !ok || a.Version != *req.ExpectedVersion {
			return errcode.ErrMarketingConflict
		}
		a.Prompt, a.Edited, a.UpdatedAt = strings.TrimSpace(req.Prompt), true, s.now()
		a.Version++
		return writeMarketingAdaptation(ctx, tx, c.sc, req.Format, a)
	})
	if err != nil {
		return nil, err
	}
	return s.Prompt(ctx, userID, id, "")
}

// promptVideo 取预览 / 转写用的视频模型快照：传了就用传的，否则用本轮的。
func (s *MarketingService) promptVideo(ctx context.Context, c *scriptCtx, videoModel string) (*provider.Snapshot, error) {
	key := videoModel
	if key == "" {
		key = c.st.VideoModel
	}
	return s.videoSnapshot(ctx, key)
}

// adaptQuote 是视图里预估出片前转写费用用的：本轮视频模型需要转写时才有，带上文本模型和视频模型能力。
type adaptQuote struct {
	text  *provider.Snapshot
	video modelcfg.Capabilities
}

// adaptQuoteFor 返回本轮的转写报价依据；不需要转写、模型不可用或没有文本模型时为 nil（报价不含转写）。
func (s *MarketingService) adaptQuoteFor(ctx context.Context, st model.MarketingSettings) *adaptQuote {
	video, err := s.models.Snapshot(ctx, st.VideoModel)
	if err != nil || video == nil || !marketing.NeedsAdapt(video.Model.Capabilities.PromptFormat) {
		return nil
	}
	text, err := s.pickTextModel(ctx, st.TextModel, needAny)
	if err != nil {
		return nil
	}
	return &adaptQuote{text: text, video: video.Model.Capabilities}
}

// adaptExtra 是一份脚本出片前还要付的转写费：已有未过期的转写稿为 0。
func (s *MarketingService) adaptExtra(q *adaptQuote, c *scriptCtx) int {
	if q == nil {
		return 0
	}
	in, _, err := s.adaptSource(c, c.st, q.video)
	if err != nil {
		return 0
	}
	hash, err := adaptationInputHash(c, c.st, q.video, in)
	if err != nil {
		return 0
	}
	if _, ok := freshAdaptation(c.sc, q.video.PromptFormat, hash); ok {
		return 0
	}
	return adaptEstimate(q.text, in)
}
