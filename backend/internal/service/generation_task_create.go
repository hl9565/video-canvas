package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/datatypes"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/logger"
	"video-canvas/internal/provider"
	"video-canvas/internal/provider/modelcfg"
	"video-canvas/internal/repository"
)

// Create 提交生成任务，复用带内部选项的整批事务入口。
func (s *GenerationTaskService) Create(ctx context.Context, userID uint64, key string, req *model.CreateGenerationTaskReq) (*model.CreateGenerationTaskResp, error) {
	return s.CreateWithOptions(ctx, userID, key, req, CreateTaskOptions{})
}

// CreateWithOptions 校验原始意图后冻结实时快照，在单个事务内隔离逐项业务失败。
func (s *GenerationTaskService) CreateWithOptions(ctx context.Context, userID uint64, key string, req *model.CreateGenerationTaskReq, opts CreateTaskOptions) (*model.CreateGenerationTaskResp, error) {
	// 1. 内部键只允许受信调用者使用；重放核验必须先于实时模型读取。
	if req == nil || (opts.DramaRunBind == nil && strings.HasPrefix(key, InternalDramaFrameRunKeyPrefix)) {
		return nil, errcode.ErrInvalidParams
	}
	nodes := reqNodes(req.NodeID, req.NodeIDs)
	if len(key) > maxIdempotencyKeyLen-len("#9") {
		return nil, errcode.ErrInvalidParams.WithMsg("Idempotency-Key 过长")
	}
	sig, err := computeCreateIntentSig(userID, key, req, nodes)
	if err != nil {
		return nil, err
	}
	existing, err := s.findIdempotentAll(ctx, s.repo, userID, key, len(nodes))
	if err != nil {
		return nil, err
	}
	all, err := s.validateBatchReplayHits(userID, sig, nodes, req, existing)
	if err != nil {
		return nil, err
	}
	if all {
		return taskItems(nodes, existing, nil), nil
	}
	// 2. 缺失项只能使用实时快照，绝不借历史快照补单。
	snap, err := s.snapshotFor(ctx, req)
	if err != nil {
		return nil, err
	}
	assetCache := newCachedAssetStore(s.assets)
	input, err := s.prepareInputWithAssets(ctx, assetCache, userID, snap, req.Input)
	if err != nil {
		return nil, err
	}
	count := modelcfg.FanoutCount(snap.Model.Capabilities, input)
	unit := max(modelcfg.Quote(snap.Model.Pricing, snap.Model.Capabilities, modelcfg.SpecFromInput(input, snap.Model.Capabilities.System)), 0)
	if name := modelcfg.FanoutParam(snap.Model.Capabilities); name != "" {
		delete(input, name)
	}
	batch := &taskBatch{userID: userID, key: key, sig: sig, nodes: nodes, req: req, opts: opts, snap: snap, input: input, assetCache: assetCache, count: count, unit: unit, existing: existing}
	// 3. 精裁模式的单镜错误由锁内目标校验返回，其余数量不符为请求级错误。
	if count != len(nodes) && !hasSegmentNode(nodes) {
		return nil, errcode.ErrInvalidParams.WithMsg(fmt.Sprintf("node_ids 的数量（%d）必须等于生成数量（%d）", len(nodes), count))
	}
	errs, err := s.createAllAtomic(ctx, batch)
	if err != nil {
		return nil, err
	}
	return taskItems(nodes, batch.existing, errs), nil
}

// taskInput 返回单个任务的输入。拆成多个任务时，每个任务带自己的随机种子（模型自己有 seed 参数时尊重用户的取值），
// 否则 N 个任务可能出一模一样的结果；插件决定怎么把 seed 交给上游。
func taskInput(input map[string]any, caps modelcfg.Capabilities, n int) map[string]any {
	out := make(map[string]any, len(input)+1)
	for k, v := range input {
		out[k] = v
	}
	if _, hasSeed := caps.Params.Get("seed"); n > 1 && !hasSeed {
		out["seed"] = float64(rand.Int32N(1<<31-1) + 1) //nolint:gosec // 生成结果的随机种子，不涉及安全
	}
	return out
}

// idemKeyAt 是第 i 个任务的幂等键：第一个沿用原 key，其余追加 #i；key 为空时都为空（不去重）。
func idemKeyAt(key string, i int) string {
	if key == "" || i == 0 {
		return key
	}
	return key + "#" + strconv.Itoa(i)
}

// findIdempotentAll 按每个节点的幂等键查已存在的任务；key 为空时全是 nil。
func (s *GenerationTaskService) findIdempotentAll(ctx context.Context, q repository.GenerationTaskTx, userID uint64, key string, n int) ([]*model.GenerationTask, error) {
	out := make([]*model.GenerationTask, n)
	for i := range out {
		t, err := s.findIdempotent(ctx, q, userID, idemKeyAt(key, i))
		if err != nil {
			return nil, err
		}
		out[i] = t
	}
	return out, nil
}

// taskItems 按节点顺序组装响应：有任务给任务，否则给这个节点的错误。
// 业务错误（errcode）原样带出 HTTP 状态与错误码，前端可以继续按状态码给文案；其它错误记日志后按内部错误返回。
func taskItems(nodes []string, tasks []*model.GenerationTask, errs []error) *model.CreateGenerationTaskResp {
	items := make([]model.CreateTaskItem, len(nodes))
	for i, nodeID := range nodes {
		items[i].NodeID = nodeID
		if tasks[i] != nil {
			items[i].Task = taskView(tasks[i])
			continue
		}
		var e *errcode.Error
		if errs == nil || !errors.As(errs[i], &e) {
			if errs != nil {
				logger.Error("创建生成任务失败", zap.String("node_id", nodeID), zap.Error(errs[i]))
			}
			e = errcode.ErrInternal
		}
		items[i].Error = &model.TaskItemError{Status: e.HTTPStatus(), Code: e.Code, Message: e.Msg}
	}
	return &model.CreateGenerationTaskResp{Items: items}
}

// findIdempotent 按幂等键查已存在的任务；key 为空或没找到返回 (nil, nil)。q 可以是仓储本身，也可以是事务。
func (s *GenerationTaskService) findIdempotent(ctx context.Context, q repository.GenerationTaskTx, userID uint64, key string) (*model.GenerationTask, error) {
	if key == "" {
		return nil, nil
	}
	t, err := q.FindByIdempotencyKey(ctx, userID, key)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// snapshotFor 取模型的当前快照并核对种类：模型不存在 / 未发布 / 已下架 / 渠道或插件不可用统一返回“模型不可用”；
// 模型种类必须与请求一致，避免拿视频模型提交图片任务。
func (s *GenerationTaskService) snapshotFor(ctx context.Context, req *model.CreateGenerationTaskReq) (*provider.Snapshot, error) {
	return checkSnapshotKind(req.Kind)(s.registry.Snapshot(ctx, req.ModelID))
}

type txSnapshotRegistry interface {
	SnapshotInTx(context.Context, string, AIConfigTxReader) (*provider.Snapshot, error)
}

func (s *GenerationTaskService) snapshotForTx(ctx context.Context, tx repository.GenerationTaskTx, req *model.CreateGenerationTaskReq) (*provider.Snapshot, error) {
	if txReg, ok := s.registry.(txSnapshotRegistry); ok {
		if reader, ok := tx.(AIConfigTxReader); ok {
			return checkSnapshotKind(req.Kind)(txReg.SnapshotInTx(ctx, req.ModelID, reader))
		}
	}
	return s.snapshotFor(ctx, req)
}

func checkSnapshotKind(kind string) func(*provider.Snapshot, error) (*provider.Snapshot, error) {
	return func(snap *provider.Snapshot, err error) (*provider.Snapshot, error) {
		if errors.Is(err, provider.ErrModelUnavailable) {
			return nil, errcode.ErrModelUnavailable
		}
		if err != nil {
			return nil, err
		}
		if snap.Model.Kind != kind {
			return nil, errcode.ErrTaskInput.WithMsg(taskUserVisibleErrPrefix + "模型的生成种类与请求不一致")
		}
		return snap, nil
	}
}

// fillTask 把快照与规范化输入填进任务行：种类 / 模型 / 渠道取快照；状态 pending、version 1；
// next_poll_at = 现在；pending 阶段的 deadline 就是排队的上限（queueWait，系统设置里的排队超时），
// 任务拿到名额之后会按模型配置的超时（缺省 30 分钟）从那一刻重新计算（见 AcquireSlot）。
func (s *GenerationTaskService) fillTask(task *model.GenerationTask, snap *provider.Snapshot, input map[string]any, queueWait time.Duration) error {
	inputJSON, err := json.Marshal(s.storedInput(input))
	if err != nil {
		return err
	}
	snapJSON, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	now := s.now()
	task.Kind = snap.Model.Kind
	task.ModelKey = snap.Model.Key
	task.Provider = providerKeyOf(snap)
	task.Status = model.TaskPending
	task.InputJSON = datatypes.JSON(inputJSON)
	task.ConfigSnapshot = datatypes.JSON(snapJSON)
	task.Version = 1
	task.NextPollAt = now
	task.DeadlineAt = now.Add(queueWait)
	return nil
}

// prepareInput 校验并规范化输入，补上宿主注入的文本参数，并确认参考素材都属于该用户、种类与大小合规。
// 字段级错误统一拼进 ErrTaskInput 的文案，返回 400。
func (s *GenerationTaskService) prepareInput(ctx context.Context, userID uint64, snap *provider.Snapshot, raw map[string]any) (map[string]any, error) {
	return s.prepareInputWithAssets(ctx, s.assets, userID, snap, raw)
}

func (s *GenerationTaskService) prepareInputWithAssets(ctx context.Context, assets provider.AssetStore, userID uint64, snap *provider.Snapshot, raw map[string]any) (map[string]any, error) {
	// 1. 按模型能力校验（提示词、生成方式、生成参数、素材数量）并补默认值
	caps := snap.Model.Capabilities
	input, fieldErrs := s.validateInput(snap.Model.Kind, caps, raw)
	if len(fieldErrs) > 0 {
		return nil, errcode.ErrTaskInput.WithMsg(taskUserVisibleErrPrefix + modelcfg.JoinFieldErrors(fieldErrs))
	}

	// 2. 文本模型：固定系统提示与最大输出由配置决定，用户的输入改不了
	if snap.Model.Kind == modelcfg.KindText {
		if caps.System != "" {
			input["system"] = caps.System
		}
		if caps.Context != nil {
			input["max_tokens"] = caps.Context.Output
		}
	}

	// 3. 逐个校验参考素材：不存在 / 不属于自己统一按“素材不存在”处理，避免暴露别人的素材；
	//    素材种类必须与数组一致（images 里不能放视频），单个大小不能超过 refs.<种类>.max_mb
	var assetErrs []modelcfg.FieldError
	for _, ref := range modelcfg.MediaRefs(input) {
		fe, err := checkAssetWith(ctx, assets, userID, caps, ref)
		if err != nil {
			return nil, err
		}
		if fe != nil {
			assetErrs = append(assetErrs, *fe)
		}
	}
	if len(assetErrs) > 0 {
		return nil, errcode.ErrTaskInput.WithMsg(taskUserVisibleErrPrefix + modelcfg.JoinFieldErrors(assetErrs))
	}
	return input, nil
}

// storedInput 返回落库用的输入：参考素材数组里的素材 id 存成十进制字符串。
// 素材 id 存成 JSON 数字，worker 读出来会变成 float64，超过 2^53 就会丢精度；字符串没有这个问题。
func (s *GenerationTaskService) storedInput(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for k, v := range input {
		out[k] = v
	}
	for _, m := range modelcfg.MediaKinds {
		if _, ok := input[m.Key]; !ok {
			continue
		}
		var ids []string
		for _, r := range modelcfg.MediaRefs(input) {
			if r.Key == m.Key {
				ids = append(ids, strconv.FormatUint(r.ID, 10))
			}
		}
		out[m.Key] = ids
	}
	return out
}

type txAssetGetter interface {
	TxAsset(ctx context.Context, userID, assetID uint64) (*model.Asset, error)
}

type cachedAssetKey struct {
	userID  uint64
	assetID uint64
}

type cachedAssetEntry struct {
	asset *model.Asset
	err   error
}

type cachedAssetStore struct {
	base  provider.AssetStore
	mu    sync.Mutex
	items map[cachedAssetKey]cachedAssetEntry
}

func newCachedAssetStore(base provider.AssetStore) *cachedAssetStore {
	return &cachedAssetStore{base: base, items: make(map[cachedAssetKey]cachedAssetEntry)}
}

func (c *cachedAssetStore) Get(ctx context.Context, userID, assetID uint64) (*model.Asset, error) {
	k := cachedAssetKey{userID: userID, assetID: assetID}
	c.mu.Lock()
	entry, ok := c.items[k]
	c.mu.Unlock()
	if ok {
		return entry.asset, entry.err
	}
	a, err := c.base.Get(ctx, userID, assetID)
	c.mu.Lock()
	c.items[k] = cachedAssetEntry{asset: a, err: err}
	c.mu.Unlock()
	return a, err
}

func (c *cachedAssetStore) Open(ctx context.Context, userID, assetID uint64) (*provider.AssetFile, error) {
	return c.base.Open(ctx, userID, assetID)
}

func (c *cachedAssetStore) txView(tx repository.GenerationTaskTx) provider.AssetStore {
	if c == nil {
		return nil
	}
	return &txCachedAssetStore{cache: c, tx: tx}
}

type txCachedAssetStore struct {
	cache *cachedAssetStore
	tx    repository.GenerationTaskTx
}

func (t *txCachedAssetStore) Get(ctx context.Context, userID, assetID uint64) (*model.Asset, error) {
	k := cachedAssetKey{userID: userID, assetID: assetID}
	t.cache.mu.Lock()
	entry, ok := t.cache.items[k]
	t.cache.mu.Unlock()
	if ok {
		return entry.asset, entry.err
	}
	if _, isSvc := t.cache.base.(*AssetService); isSvc {
		if getter, canTx := t.tx.(txAssetGetter); canTx {
			a, err := getter.TxAsset(ctx, userID, assetID)
			if errors.Is(err, repository.ErrNotFound) {
				err = provider.ErrAssetNotFound
			}
			t.cache.mu.Lock()
			t.cache.items[k] = cachedAssetEntry{asset: a, err: err}
			t.cache.mu.Unlock()
			return a, err
		}
	}
	return t.cache.Get(ctx, userID, assetID)
}

func (t *txCachedAssetStore) Open(ctx context.Context, userID, assetID uint64) (*provider.AssetFile, error) {
	return t.cache.Open(ctx, userID, assetID)
}

type txDirectAssetStore struct {
	tx       txAssetGetter
	fallback provider.AssetStore
}

func (t *txDirectAssetStore) Get(ctx context.Context, userID, assetID uint64) (*model.Asset, error) {
	a, err := t.tx.TxAsset(ctx, userID, assetID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, provider.ErrAssetNotFound
	}
	return a, err
}

func (t *txDirectAssetStore) Open(ctx context.Context, userID, assetID uint64) (*provider.AssetFile, error) {
	return t.fallback.Open(ctx, userID, assetID)
}

func checkAssetWith(ctx context.Context, assets provider.AssetStore, userID uint64, caps modelcfg.Capabilities, ref modelcfg.MediaRef) (*modelcfg.FieldError, error) {
	asset, err := assets.Get(ctx, userID, ref.ID)
	if errors.Is(err, provider.ErrAssetNotFound) {
		return &modelcfg.FieldError{Field: ref.Key, Message: "素材不存在"}, nil
	}
	if err != nil {
		return nil, err
	}
	if asset.Kind != ref.Kind {
		return &modelcfg.FieldError{Field: ref.Key, Message: "素材类型不匹配"}, nil
	}
	if mb := caps.Refs.Of(ref.Kind).MaxMB; mb > 0 && asset.ByteSize > int64(mb)<<20 {
		return &modelcfg.FieldError{Field: ref.Key, Message: fmt.Sprintf("单个素材不能超过 %d MB", mb)}, nil
	}
	return nil, nil
}
