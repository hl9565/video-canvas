package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/jsonhash"
	"video-canvas/internal/provider"
	"video-canvas/internal/provider/modelcfg"
	"video-canvas/internal/repository"
)

const (
	// InternalDramaFrameRunKeyPrefix 是短剧首帧工作流专用的内部幂等键前缀，禁止外部直接传入。
	InternalDramaFrameRunKeyPrefix = "__internal:drama-frame-run:"
	defaultQuoteTTL                = 5 * time.Minute
)

// DramaFrameRunBind 是内部 FrameRun 提交绑定，放行内部前缀并在事务内提供报价断言。
type DramaFrameRunBind struct {
	RunID          uint64
	ConfirmRev     int64
	QuoteAssertion *model.TaskQuoteAssertion
}

// CreateTaskOptions 携带服务端内部任务提交选项（仅同进程受信服务构造）。
type CreateTaskOptions struct {
	DramaRunBind *DramaFrameRunBind
}

// QuoteTaskOptions 配置报价令牌绑定的短剧片段上下界及单镜约束。
type QuoteTaskOptions struct {
	SegmentID       uint64
	SegmentRevision int64
	SingleShotOnly  bool
}

type taskReplayExpectation struct {
	RawIntentSig string
	BatchSize    int
	BatchIndex   int
	IsTest       bool
	AgentRunID   uint64
	Kind         string
	ModelKey     string
	CanvasID     *uint64
	NodeID       string
	RawInput     map[string]any
}

var sizePattern = regexp.MustCompile(`^(\d{2,5})[x*](\d{2,5})$`)

func resolveQuoteSecret(explicit, aiSecret string) []byte {
	if s := strings.TrimSpace(explicit); s != "" {
		sum := sha256.Sum256([]byte("vc-quote:" + s))
		return sum[:]
	}
	if s := strings.TrimSpace(aiSecret); s != "" {
		sum := sha256.Sum256([]byte("vc-quote:" + s))
		return sum[:]
	}
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return buf
}

// Quote 计算规范化有效参数、已知插件请求像素尺寸、积分、价格指纹与短期报价令牌。
func (s *GenerationTaskService) Quote(ctx context.Context, userID uint64, req *model.QuoteGenerationTaskReq, opts ...QuoteTaskOptions) (*model.TaskQuoteView, error) {
	if req == nil || req.ModelID == "" || req.Input == nil {
		return nil, errcode.ErrInvalidParams
	}
	var opt QuoteTaskOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	nodes := reqNodes(req.NodeID, req.NodeIDs)
	if opt.SingleShotOnly && len(nodes) != 1 {
		return nil, errcode.ErrDramaEditInvalid.WithMsg("精裁模式仅允许单镜生成")
	}
	createReq := &model.CreateGenerationTaskReq{
		Kind: req.Kind, ModelID: req.ModelID, CanvasID: req.CanvasID, NodeID: req.NodeID, NodeIDs: req.NodeIDs, Input: req.Input,
	}
	snap, err := s.snapshotFor(ctx, createReq)
	if err != nil {
		return nil, err
	}
	normalized, err := s.prepareInput(ctx, userID, snap, req.Input)
	if err != nil {
		return nil, err
	}
	caps := snap.Model.Capabilities
	count := modelcfg.FanoutCount(caps, normalized)
	if opt.SingleShotOnly && count != 1 {
		return nil, errcode.ErrDramaEditInvalid.WithMsg("精裁模式生成数量必须为 1")
	}
	if count != len(nodes) {
		return nil, errcode.ErrInvalidParams.WithMsg("node_ids 的数量必须等于生成数量")
	}
	effective := cloneMap(normalized)
	perTaskInput := cloneMap(normalized)
	if name := modelcfg.FanoutParam(caps); name != "" {
		delete(perTaskInput, name)
	}
	storedHash, err := jsonhash.CanonicalSHA256(s.storedInput(perTaskInput))
	if err != nil {
		return nil, err
	}
	unit := max(modelcfg.Quote(snap.Model.Pricing, caps, modelcfg.SpecFromInput(normalized, caps.System)), 0)
	priceFP, err := priceFingerprint(snap)
	if err != nil {
		return nil, err
	}
	exp := s.now().Add(s.quoteTTL)
	assertion := &model.TaskQuoteAssertion{
		UserID: userID, SegmentID: opt.SegmentID, SegmentRevision: opt.SegmentRevision,
		ModelID: snap.Model.Key, ModelRevisionID: snap.ModelRevisionID,
		PluginKey: snap.Plugin.Key, PluginVersion: snap.Plugin.Version, PluginSHA256: snap.Plugin.SHA256,
		UnitCredits: unit, Count: count, TotalCredits: unit * len(nodes),
		InputHash: storedHash, PriceFingerprint: priceFP, ExpiresAt: exp,
	}
	token, err := s.signQuoteAssertion(assertion)
	if err != nil {
		return nil, err
	}
	return &model.TaskQuoteView{
		EffectiveInput:   effective,
		RawSize:          resolvePluginRawSize(snap, effective),
		Credits:          assertion.TotalCredits,
		PriceFingerprint: priceFP,
		QuoteToken:       token,
		ExpiresAt:        exp,
	}, nil
}

// CheckIdempotentReplay 供外层单镜生成在检查锁定/繁忙/片长前预判幂等命中或冲突。
func (s *GenerationTaskService) CheckIdempotentReplay(ctx context.Context, userID uint64, idempotencyKey string, req *model.CreateGenerationTaskReq) (*model.GenerationTaskView, error) {
	if idempotencyKey == "" || req == nil {
		return nil, nil
	}
	if strings.HasPrefix(idempotencyKey, InternalDramaFrameRunKeyPrefix) {
		return nil, errcode.ErrInvalidParams.WithMsg("保留的内部幂等键前缀")
	}
	nodes := reqNodes(req.NodeID, req.NodeIDs)
	if len(idempotencyKey) > maxIdempotencyKeyLen-len("#9") {
		return nil, errcode.ErrInvalidParams.WithMsg("Idempotency-Key 过长")
	}
	rawSig, err := computeCreateIntentSig(userID, idempotencyKey, req, nodes)
	if err != nil {
		return nil, err
	}
	existing, err := s.findIdempotentAll(ctx, s.repo, userID, idempotencyKey, len(nodes))
	if err != nil {
		return nil, err
	}
	allHit, err := s.validateBatchReplayHits(userID, rawSig, nodes, req, existing)
	if err != nil || !allHit {
		return nil, err
	}
	return taskView(existing[0]), nil
}

func computeCreateIntentSig(userID uint64, rootKey string, req *model.CreateGenerationTaskReq, nodes []string) (string, error) {
	return jsonhash.CanonicalSHA256(struct {
		UserID    uint64         `json:"u"`
		RootKey   string         `json:"k"`
		IsTest    bool           `json:"t"`
		AgentRun  uint64         `json:"a"`
		Kind      string         `json:"kind"`
		ModelKey  string         `json:"m"`
		CanvasID  uint64         `json:"c"`
		BatchSize int            `json:"bs"`
		Nodes     []string       `json:"n"`
		RawInput  map[string]any `json:"in"`
		QuoteTok  string         `json:"qt"`
	}{
		UserID: userID, RootKey: rootKey, IsTest: false, AgentRun: req.AgentRunID,
		Kind: req.Kind, ModelKey: req.ModelID, CanvasID: uint64(req.CanvasID),
		BatchSize: len(nodes), Nodes: nodes, RawInput: nonNilMap(req.Input), QuoteTok: req.QuoteToken,
	})
}

func computeTestRunIntentSig(userID uint64, rootKey string, kind, modelKey string, rawInput map[string]any) (string, error) {
	return jsonhash.CanonicalSHA256(struct {
		UserID   uint64         `json:"u"`
		RootKey  string         `json:"k"`
		IsTest   bool           `json:"t"`
		AgentRun uint64         `json:"a"`
		Kind     string         `json:"kind"`
		ModelKey string         `json:"m"`
		RawInput map[string]any `json:"in"`
	}{
		UserID: userID, RootKey: rootKey, IsTest: true, AgentRun: 0,
		Kind: kind, ModelKey: modelKey, RawInput: nonNilMap(rawInput),
	})
}

func (s *GenerationTaskService) validateBatchReplayHits(userID uint64, rawSig string, nodes []string, req *model.CreateGenerationTaskReq, existing []*model.GenerationTask) (bool, error) {
	all := true
	var cid *uint64
	if req.CanvasID != 0 {
		v := uint64(req.CanvasID)
		cid = &v
	}
	for i, hit := range existing {
		if hit == nil {
			all = false
			continue
		}
		exp := taskReplayExpectation{
			RawIntentSig: rawSig, BatchSize: len(nodes), BatchIndex: i,
			IsTest: false, AgentRunID: req.AgentRunID, Kind: req.Kind, ModelKey: req.ModelID,
			CanvasID: cid, NodeID: nodes[i], RawInput: nonNilMap(req.Input),
		}
		if err := s.validateTaskReplayHit(exp, hit); err != nil {
			return false, err
		}
	}
	return all && len(existing) == len(nodes), nil
}

func (s *GenerationTaskService) validateTaskReplayHit(exp taskReplayExpectation, hit *model.GenerationTask) error {
	if hit == nil {
		return nil
	}
	if hit.IsTest != exp.IsTest || hit.AgentRunID != exp.AgentRunID {
		return errcode.ErrIdempotencyConflict
	}
	if hit.RequestIntentSig != "" {
		if hit.RequestIntentSig != exp.RawIntentSig || hit.RequestBatchIndex != int16(exp.BatchIndex) || hit.NodeID != exp.NodeID {
			return errcode.ErrIdempotencyConflict
		}
		return nil
	}
	return s.validateLegacyEmptySigHit(exp, hit)
}

func (s *GenerationTaskService) validateLegacyEmptySigHit(exp taskReplayExpectation, hit *model.GenerationTask) error {
	if exp.BatchSize != 1 || exp.BatchIndex != 0 || hit.RequestBatchIndex != 0 {
		return errcode.ErrIdempotencyConflict
	}
	if hit.Kind != exp.Kind || hit.ModelKey != exp.ModelKey || hit.NodeID != exp.NodeID || !sameUint64Ptr(hit.CanvasProjectID, exp.CanvasID) {
		return errcode.ErrIdempotencyConflict
	}
	if len(hit.ConfigSnapshot) == 0 || len(hit.InputJSON) == 0 {
		return errcode.ErrIdempotencyConflict
	}
	var legacySnap provider.Snapshot
	if err := json.Unmarshal(hit.ConfigSnapshot, &legacySnap); err != nil || legacySnap.Model.Key == "" {
		return errcode.ErrIdempotencyConflict
	}
	if legacySnap.Model.Kind != exp.Kind || legacySnap.Model.Key != exp.ModelKey {
		return errcode.ErrIdempotencyConflict
	}
	norm, err := s.normalizeLegacyReplayInput(exp, &legacySnap)
	if err != nil {
		return err
	}
	recomputed := s.storedInput(norm)
	var storedDB map[string]any
	if err := json.Unmarshal(hit.InputJSON, &storedDB); err != nil {
		return errcode.ErrIdempotencyConflict
	}
	hNew, err1 := jsonhash.CanonicalSHA256(recomputed)
	hOld, err2 := jsonhash.CanonicalSHA256(storedDB)
	if err1 != nil || err2 != nil || hNew != hOld {
		return errcode.ErrIdempotencyConflict
	}
	return nil
}

// normalizeLegacyReplayInput 还原历史任务所属来源的完整规范化与 fanout 持久化规则。
func (s *GenerationTaskService) normalizeLegacyReplayInput(exp taskReplayExpectation, legacySnap *provider.Snapshot) (map[string]any, error) {
	caps := legacySnap.Model.Capabilities
	norm, fieldErrs := s.validateInput(legacySnap.Model.Kind, caps, exp.RawInput)
	if len(fieldErrs) > 0 {
		return nil, errcode.ErrIdempotencyConflict
	}
	if legacySnap.Model.Kind == modelcfg.KindText {
		if caps.System != "" {
			norm["system"] = caps.System
		}
		if caps.Context != nil {
			norm["max_tokens"] = caps.Context.Output
		}
	}
	if modelcfg.FanoutCount(caps, norm) != 1 {
		return nil, errcode.ErrIdempotencyConflict
	}
	if !exp.IsTest {
		if fp := modelcfg.FanoutParam(caps); fp != "" {
			delete(norm, fp)
		}
	}
	return norm, nil
}

func (s *GenerationTaskService) signQuoteAssertion(q *model.TaskQuoteAssertion) (string, error) {
	payload, err := json.Marshal(q)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, s.quoteSecret)
	mac.Write(payload)
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (s *GenerationTaskService) parseAndVerifyQuoteToken(token string) (*model.TaskQuoteAssertion, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, errcode.ErrDramaQuoteExpired
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errcode.ErrDramaQuoteExpired
	}
	gotSig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errcode.ErrDramaQuoteExpired
	}
	mac := hmac.New(sha256.New, s.quoteSecret)
	mac.Write(payload)
	if subtle.ConstantTimeCompare(gotSig, mac.Sum(nil)) != 1 {
		return nil, errcode.ErrDramaQuoteExpired
	}
	var q model.TaskQuoteAssertion
	if err := json.Unmarshal(payload, &q); err != nil {
		return nil, errcode.ErrDramaQuoteExpired
	}
	return &q, nil
}

func (s *GenerationTaskService) verifyQuoteAssertionLocked(ctx context.Context, tx repository.GenerationTaskTx, assets provider.AssetStore, userID uint64, req *model.CreateGenerationTaskReq, editSchema int16, segID uint64, segRev int64, count int, fallbackSnap *provider.Snapshot, fallbackUnit int, fallbackInput map[string]any, bind *DramaFrameRunBind) (*provider.Snapshot, map[string]any, int, error) {
	if editSchema != 1 && req.QuoteToken == "" && bind == nil {
		return fallbackSnap, fallbackInput, fallbackUnit, nil
	}
	var q *model.TaskQuoteAssertion
	if bind != nil && bind.QuoteAssertion != nil {
		q = bind.QuoteAssertion
	} else {
		if req.QuoteToken == "" {
			return nil, nil, 0, errcode.ErrDramaQuoteExpired
		}
		parsed, err := s.parseAndVerifyQuoteToken(req.QuoteToken)
		if err != nil {
			return nil, nil, 0, err
		}
		q = parsed
	}
	if q.UserID != userID || !q.ExpiresAt.After(s.now()) || count != 1 || q.Count != 1 {
		return nil, nil, 0, errcode.ErrDramaQuoteExpired
	}
	if editSchema == 1 && bind == nil && (q.SegmentID != segID || q.SegmentRevision != segRev) {
		return nil, nil, 0, errcode.ErrDramaQuoteExpired
	}
	freshSnap, err := s.snapshotForTx(ctx, tx, req)
	if err != nil {
		if errors.Is(err, errcode.ErrModelUnavailable) {
			return nil, nil, 0, errcode.ErrDramaQuoteExpired
		}
		return nil, nil, 0, err
	}
	freshInput, err := assertQuoteMatchesLiveSnapshot(ctx, s, assets, userID, freshSnap, req.Input, fallbackInput, q)
	if err != nil {
		return nil, nil, 0, err
	}
	return freshSnap, freshInput, q.UnitCredits, nil
}

func assertQuoteMatchesLiveSnapshot(ctx context.Context, s *GenerationTaskService, assets provider.AssetStore, userID uint64, snap *provider.Snapshot, rawReqInput, fallbackStored map[string]any, q *model.TaskQuoteAssertion) (map[string]any, error) {
	if snap.Model.Key != q.ModelID || snap.ModelRevisionID != q.ModelRevisionID ||
		snap.Plugin.Key != q.PluginKey || snap.Plugin.Version != q.PluginVersion || snap.Plugin.SHA256 != q.PluginSHA256 {
		return nil, errcode.ErrDramaQuoteExpired
	}
	freshFP, err := priceFingerprint(snap)
	if err != nil || (q.PriceFingerprint != "" && freshFP != q.PriceFingerprint) {
		return nil, errcode.ErrDramaQuoteExpired
	}
	if assets == nil {
		assets = s.assets
	}
	freshNorm, err := s.prepareInputWithAssets(ctx, assets, userID, snap, cloneMap(rawReqInput))
	if err != nil {
		return nil, errcode.ErrDramaQuoteExpired
	}
	freshUnit := max(modelcfg.Quote(snap.Model.Pricing, snap.Model.Capabilities, modelcfg.SpecFromInput(freshNorm, snap.Model.Capabilities.System)), 0)
	if freshUnit != q.UnitCredits {
		return nil, errcode.ErrDramaQuoteExpired
	}
	perTask := cloneMap(freshNorm)
	if fp := modelcfg.FanoutParam(snap.Model.Capabilities); fp != "" {
		delete(perTask, fp)
	}
	freshHash, err := jsonhash.CanonicalSHA256(s.storedInput(perTask))
	fbHash, err2 := jsonhash.CanonicalSHA256(s.storedInput(fallbackStored))
	if err != nil || err2 != nil || freshHash != fbHash || (q.InputHash != "" && freshHash != q.InputHash) {
		return nil, errcode.ErrDramaQuoteExpired
	}
	return perTask, nil
}

func priceFingerprint(snap *provider.Snapshot) (string, error) {
	return jsonhash.CanonicalSHA256(struct {
		Key       string                `json:"k"`
		RevID     uint64                `json:"rev"`
		PluginKey string                `json:"pk"`
		PluginVer string                `json:"pv"`
		PluginSHA string                `json:"ps"`
		Caps      modelcfg.Capabilities `json:"c"`
		Pricing   modelcfg.Pricing      `json:"p"`
	}{
		Key: snap.Model.Key, RevID: snap.ModelRevisionID,
		PluginKey: snap.Plugin.Key, PluginVer: snap.Plugin.Version, PluginSHA: snap.Plugin.SHA256,
		Caps: snap.Model.Capabilities, Pricing: snap.Model.Pricing,
	})
}

func reqNodes(single string, many []string) []string {
	if len(many) > 0 {
		return many
	}
	return []string{single}
}

func sameUint64Ptr(a, b *uint64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func cloneMap(src map[string]any) map[string]any {
	if src == nil {
		return map[string]any{}
	}
	b, err := json.Marshal(src)
	if err != nil {
		out := make(map[string]any, len(src))
		for k, v := range src {
			out[k] = v
		}
		return out
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

func nonNilMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
