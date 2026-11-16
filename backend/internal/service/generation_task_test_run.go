package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/provider"
	"video-canvas/internal/repository"
)

func validateTestRunKey(key string) error {
	if strings.HasPrefix(key, InternalDramaFrameRunKeyPrefix) || len(key) > maxIdempotencyKeyLen {
		return errcode.ErrInvalidParams
	}
	return nil
}

// ReplayTestRun 按 (userID, key, is_test, modelKey, rawInput) 直接重放已落库试跑任务，
// 不重建或依赖当前模型配置/密钥。未命中返回 nil, nil。
func (s *GenerationTaskService) ReplayTestRun(ctx context.Context, userID uint64, key, modelKey string, rawInput map[string]any) (*model.GenerationTaskView, error) {
	if err := validateTestRunKey(key); err != nil {
		return nil, err
	}
	if key == "" {
		return nil, nil
	}
	existing, err := s.findIdempotent(ctx, s.repo, userID, key)
	if err != nil || existing == nil {
		return nil, err
	}
	if !existing.IsTest || existing.AgentRunID != 0 || existing.ModelKey != modelKey {
		return nil, errcode.ErrIdempotencyConflict
	}
	kind := existing.Kind
	if kind == "" && len(existing.ConfigSnapshot) > 0 {
		var legacySnap provider.Snapshot
		if json.Unmarshal(existing.ConfigSnapshot, &legacySnap) == nil {
			kind = legacySnap.Model.Kind
		}
	}
	sig, err := computeTestRunIntentSig(userID, key, kind, modelKey, rawInput)
	if err != nil {
		return nil, err
	}
	exp := taskReplayExpectation{RawIntentSig: sig, BatchSize: 1, IsTest: true, Kind: kind, ModelKey: modelKey, RawInput: nonNilMap(rawInput)}
	return s.testReplayView(exp, existing)
}

// SubmitTest 创建运营试跑任务，不冻结积分、不检查并发、不向普通用户推送。
func (s *GenerationTaskService) SubmitTest(ctx context.Context, userID uint64, snap *provider.Snapshot, input map[string]any) (*model.GenerationTaskView, error) {
	// 1. 使用规范化之前的原始输入核验意图，避免后台默认值漂移造成误判。
	key, _ := ctx.Value(testRunKey{}).(string)
	if err := validateTestRunKey(key); err != nil {
		return nil, err
	}
	if snap == nil {
		return nil, errcode.ErrInvalidParams.WithMsg("缺少试跑配置")
	}
	raw, ok := ctx.Value(testRunRawInput{}).(map[string]any)
	if !ok {
		raw = input
	}
	sig, err := computeTestRunIntentSig(userID, key, snap.Model.Kind, snap.Model.Key, raw)
	if err != nil {
		return nil, err
	}
	exp := taskReplayExpectation{RawIntentSig: sig, BatchSize: 1, IsTest: true, Kind: snap.Model.Kind, ModelKey: snap.Model.Key, RawInput: nonNilMap(raw)}
	existing, err := s.findIdempotent(ctx, s.repo, userID, key)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return s.testReplayView(exp, existing)
	}
	// 2. 新建使用实时校验后的输入，保留试跑的 fanout 字段，与历史落库管线一致。
	normalized, err := s.prepareInput(ctx, userID, snap, input)
	if err != nil {
		return nil, err
	}
	queueWait, err := s.queueMaxWait(ctx)
	if err != nil {
		return nil, err
	}
	task := &model.GenerationTask{UserID: userID, IsTest: true, IdempotencyKey: key, RequestIntentSig: sig, RequestBatchIndex: 0}
	if err := s.fillTask(task, snap, normalized, queueWait); err != nil {
		return nil, err
	}
	err = s.repo.WithTx(ctx, func(tx repository.GenerationTaskTx) error {
		inserted, err := tx.TxInsertTask(ctx, task)
		if err != nil {
			return err
		}
		if !inserted {
			return errIdempotentConflict
		}
		return enqueueTaskAndNotify(ctx, tx, task)
	})
	// 3. 并发唯一冲突的回捞同样强制核验来源与意图。
	if errors.Is(err, errIdempotentConflict) {
		existing, findErr := s.findIdempotent(ctx, s.repo, userID, key)
		if findErr != nil {
			return nil, findErr
		}
		if existing == nil {
			return nil, errcode.ErrIdempotencyConflict
		}
		return s.testReplayView(exp, existing)
	}
	if err != nil {
		return nil, err
	}
	logGenCreated(ctx, task)
	return taskView(task), nil
}

// testReplayView 将试跑快返和冲突回捞收敛到同一个校验器。
func (s *GenerationTaskService) testReplayView(exp taskReplayExpectation, hit *model.GenerationTask) (*model.GenerationTaskView, error) {
	if err := s.validateTaskReplayHit(exp, hit); err != nil {
		return nil, err
	}
	return taskView(hit), nil
}
