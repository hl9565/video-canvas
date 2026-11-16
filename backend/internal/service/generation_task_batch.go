package service

import (
	"context"
	"errors"
	"time"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/provider"
	"video-canvas/internal/repository"
)

var errBatchReadOnly = errors.New("batch has no new tasks")

type taskBatch struct {
	userID               uint64
	key, sig             string
	nodes                []string
	req                  *model.CreateGenerationTaskReq
	opts                 CreateTaskOptions
	snap                 *provider.Snapshot
	input                map[string]any
	assetCache           *cachedAssetStore
	count, unit, initial int
	activeLimit          int64
	queueWait            time.Duration
	existing             []*model.GenerationTask
	createdTasks         []*model.GenerationTask
	errs                 []error
}

// preResolvedTarget 在积分锁之前冻结目标归属、业务错误及允许查询的媒体节点集合。
type preResolvedTarget struct {
	err        error
	segment    *model.DramaSegment
	episode    *model.DramaEpisode
	project    *model.DramaProject
	script     *model.MarketingScript
	marketing  *marketingLockedRender
	mediaNodes []string
}

// createAllAtomic 全批共用一个事务，业务失败回滚单项，系统失败回滚全批；提交后才广播。
func (s *GenerationTaskService) createAllAtomic(ctx context.Context, b *taskBatch) ([]error, error) {
	if err := s.prepareBatchLimits(ctx, b); err != nil {
		return nil, err
	}
	b.errs = make([]error, len(b.nodes))
	err := s.repo.WithTx(ctx, func(tx repository.GenerationTaskTx) error {
		return s.createBatchTx(ctx, tx, b)
	})
	if errors.Is(err, errBatchReadOnly) {
		return b.errs, nil
	}
	if errors.Is(err, errIdempotentConflict) {
		return s.resolveBatchWinner(ctx, b)
	}
	if err != nil {
		return nil, err
	}
	// 3. 外层事务已提交并解锁，观察者重入不会看见半批数据。
	for _, t := range b.createdTasks {
		s.publish(ctx, t)
		logGenCreated(ctx, t)
	}
	return b.errs, nil
}

// createBatchTx 在固定锁序和账户锁内复查后逐项执行保存点。
func (s *GenerationTaskService) createBatchTx(ctx context.Context, tx repository.GenerationTaskTx, b *taskBatch) error {
	// 0. 营销出片先以事务咨询锁按 (user, rootKey) 串行化，再复核同根键是否已服务不交的目标集合。
	if err := s.lockAndCheckMarketingRoot(ctx, tx, b); err != nil {
		return err
	}
	// 1. 严格按 seg → mkt → credit 锁序解析，后续保存点禁止重新读取目标行。
	preResolvedTargets, err := preResolveBatchTargets(ctx, tx, b)
	if err != nil {
		return err
	}
	if err := tx.TxEnsureCredit(ctx, b.userID, b.initial); err != nil {
		return err
	}
	if _, err := tx.LockCredit(ctx, b.userID); err != nil {
		return err
	}
	// 2. 账户锁内全量复核，禁止并发请求跨键拼单。
	hits, err := s.findIdempotentAll(ctx, tx, b.userID, b.key, len(b.nodes))
	if err != nil {
		return err
	}
	all, err := s.validateBatchReplayHits(b.userID, b.sig, b.nodes, b.req, hits)
	if err != nil {
		return err
	}
	b.existing = hits
	if all {
		return errBatchReadOnly
	}
	for i, node := range b.nodes {
		if b.existing[i] != nil {
			continue
		}
		if err := s.createBatchItem(ctx, tx, b, preResolvedTargets[node], i); err != nil {
			return err
		}
	}
	if len(b.createdTasks) == 0 {
		return errBatchReadOnly
	}
	return nil
}

// prepareBatchLimits 在持锁前读取设置，避免账户锁中访问设置服务。
func (s *GenerationTaskService) prepareBatchLimits(ctx context.Context, b *taskBatch) error {
	var err error
	b.initial, err = s.initialCredits(ctx)
	if err != nil {
		return err
	}
	active, err := s.maxActiveTasks(ctx, b.userID)
	if err != nil {
		return err
	}
	multiplier, err := s.queueMultiplier(ctx)
	if err != nil {
		return err
	}
	b.activeLimit = int64(active) * int64(multiplier)
	b.queueWait, err = s.queueMaxWait(ctx)
	return err
}

// resolveBatchWinner 对意外唯一冲突的赢家使用同一重放校验，不返回未经核验的任务。
func (s *GenerationTaskService) resolveBatchWinner(ctx context.Context, b *taskBatch) ([]error, error) {
	hits, err := s.findIdempotentAll(ctx, s.repo, b.userID, b.key, len(b.nodes))
	if err != nil {
		return nil, err
	}
	all, err := s.validateBatchReplayHits(b.userID, b.sig, b.nodes, b.req, hits)
	if err != nil {
		return nil, err
	}
	if !all {
		return nil, errcode.ErrIdempotencyConflict
	}
	b.existing = hits
	return b.errs, nil
}

// createBatchItem 保存点内只使用冻结的目标，任何保存点操作失败均中止整个事务。
func (s *GenerationTaskService) createBatchItem(ctx context.Context, tx repository.GenerationTaskTx, b *taskBatch, target preResolvedTarget, i int) error {
	if err := tx.ExecSavepoint(ctx, i); err != nil {
		return err
	}
	task, err := s.freezeBatchItem(ctx, tx, b, target, i)
	if err != nil {
		var business *errcode.Error
		if !errors.As(err, &business) {
			return err
		}
		if rollbackErr := tx.RollbackSavepoint(ctx, i); rollbackErr != nil {
			return rollbackErr
		}
		b.errs[i] = err
	} else {
		b.existing[i] = task
		b.createdTasks = append(b.createdTasks, task)
	}
	return tx.ReleaseSavepoint(ctx, i)
}

// freezeBatchItem 校验锁内报价和任务状态，原子写任务、冻结流水、队列及 Agent 门铃。
func (s *GenerationTaskService) freezeBatchItem(ctx context.Context, tx repository.GenerationTaskTx, b *taskBatch, target preResolvedTarget, i int) (*model.GenerationTask, error) {
	if target.err != nil {
		return nil, target.err
	}
	snap, itemInput, unit, err := s.batchItemCredits(ctx, tx, b, target)
	if err != nil {
		return nil, err
	}
	if err := validateBatchMediaTasks(ctx, tx, b, target, i); err != nil {
		return nil, err
	}
	acc, err := tx.LockCredit(ctx, b.userID)
	if err != nil {
		return nil, err
	}
	if acc.Balance-acc.Frozen < unit+marketingAdaptCredits(target) {
		return nil, errcode.ErrInsufficientCredits
	}
	active, err := tx.CountActive(ctx, b.userID)
	if err != nil {
		return nil, err
	}
	if active >= b.activeLimit {
		return nil, errcode.ErrTooManyTasks
	}
	if err := commitBatchMarketingAdapt(ctx, tx, target); err != nil {
		return nil, err
	}
	task := &model.GenerationTask{UserID: b.userID, Credits: unit, NodeID: b.nodes[i], IdempotencyKey: idemKeyAt(b.key, i), AgentRunID: b.req.AgentRunID, RequestIntentSig: b.sig, RequestBatchIndex: int16(i)}
	if b.req.CanvasID > 0 {
		id := uint64(b.req.CanvasID)
		task.CanvasProjectID = &id
	}
	if err := s.fillTask(task, snap, taskInput(itemInput, snap.Model.Capabilities, b.count), b.queueWait); err != nil {
		return nil, err
	}
	inserted, err := tx.TxInsertTask(ctx, task)
	if err != nil {
		return nil, err
	}
	if !inserted {
		return nil, errIdempotentConflict
	}
	if err := tx.AddCredit(ctx, b.userID, 0, unit); err != nil {
		return nil, err
	}
	if _, err := tx.InsertLedger(ctx, &model.CreditLedger{UserID: b.userID, TaskID: model.TaskIDPtr(task.ID), Type: model.LedgerFreeze, Amount: unit}); err != nil {
		return nil, err
	}
	if err := enqueueTaskAndNotify(ctx, tx, task); err != nil {
		return nil, err
	}
	return task, nil
}

// batchItemCredits 在账户锁内复核精裁单镜限制与报价断言。
func (s *GenerationTaskService) batchItemCredits(ctx context.Context, tx repository.GenerationTaskTx, b *taskBatch, target preResolvedTarget) (*provider.Snapshot, map[string]any, int, error) {
	if b.opts.MarketingRender != nil {
		return s.marketingBatchItemCredits(ctx, tx, b, target)
	}
	var schema int16
	var sid uint64
	var rev int64
	if target.segment != nil {
		schema, sid, rev = target.episode.EditSchema, target.segment.ID, target.segment.Revision
		if schema == 1 && (len(b.nodes) != 1 || b.count != 1) {
			return nil, nil, 0, errcode.ErrDramaEditInvalid
		}
	}
	if b.count != len(b.nodes) {
		return nil, nil, 0, errcode.ErrInvalidParams.WithMsg("node_ids 的数量必须等于生成数量")
	}
	lockAssets := s.assets
	if b.assetCache != nil {
		lockAssets = b.assetCache.txView(tx)
	} else if _, isSvc := s.assets.(*AssetService); isSvc {
		if getter, ok := tx.(txAssetGetter); ok {
			lockAssets = &txDirectAssetStore{tx: getter, fallback: s.assets}
		}
	}
	return s.verifyQuoteAssertionLocked(ctx, tx, lockAssets, b.userID, b.req, schema, sid, rev, b.count, b.snap, b.unit, b.input, b.opts.DramaRunBind)
}

// enqueueTaskAndNotify 将任务与 Agent 门铃放进同一数据库事务或保存点。
func enqueueTaskAndNotify(ctx context.Context, tx repository.GenerationTaskTx, task *model.GenerationTask) error {
	if err := tx.EnqueueTask(ctx, task); err != nil {
		return err
	}
	if task.AgentRunID > 0 {
		return tx.EnqueueAgentNotify(ctx, task)
	}
	return nil
}
