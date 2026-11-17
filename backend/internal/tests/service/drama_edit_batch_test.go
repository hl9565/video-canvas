package service_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/provider"
	"video-canvas/internal/provider/modelcfg"
	"video-canvas/internal/service"
)

func v21MultiRegistry(unit int) *fakeTaskRegistry {
	snap := fakeTaskSnapshot(model.KindVideo, unit)
	snap.Model.Capabilities.Params = modelcfg.ParamSet{
		{Name: "resolution", ParamField: modelcfg.ParamField{Type: modelcfg.ParamEnum, Open: true, Options: []any{"720p", "1080p"}, Default: "720p"}},
		{Name: "count", ParamField: modelcfg.ParamField{Type: modelcfg.ParamEnum, Open: true, Options: []any{1.0, 2.0, 3.0}, Default: 1.0, Fanout: true}},
	}
	return &fakeTaskRegistry{snap: snap}
}

func TestGenerationTaskService_V21_ReservedPrefixAndSourceIsolation(t *testing.T) {
	ctx := context.Background()
	env := newTaskSvcEnv(defaultTaskLimits())
	env.repo.addCredit(1, 500, 0)

	// 1. Create / SubmitTest 无 DramaRunBind 拦截 __internal:drama-frame-run:
	_, err := env.svc.Create(ctx, 1, service.InternalDramaFrameRunKeyPrefix+"r1", validCreateReq())
	assertTaskCode(t, err, errcode.ErrInvalidParams.Code)

	testCtx := context.WithValue(ctx, service.TestRunKeyForTest(), service.InternalDramaFrameRunKeyPrefix+"r1")
	_, err = env.svc.SubmitTest(testCtx, 1, env.registry.snap, map[string]any{"prompt": "猫"})
	assertTaskCode(t, err, errcode.ErrInvalidParams.Code)

	// 缺少流程身份和片段目标的 bind 不是有效受信提交。
	now := time.Now()
	boundResp, err := env.svc.CreateWithOptions(ctx, 1, service.InternalDramaFrameRunKeyPrefix+"r1", validCreateReq(), service.CreateTaskOptions{
		DramaRunBind: &service.DramaFrameRunBind{
			QuoteAssertion: &model.TaskQuoteAssertion{
				UserID: 1, ModelID: "m1", ModelRevisionID: env.registry.snap.ModelRevisionID,
				PluginKey: env.registry.snap.Plugin.Key, PluginVersion: env.registry.snap.Plugin.Version, PluginSHA256: env.registry.snap.Plugin.SHA256,
				UnitCredits: 10, Count: 1, TotalCredits: 10, ExpiresAt: now.Add(time.Minute),
			},
		},
	})
	assertTaskCode(t, err, errcode.ErrInvalidParams.Code)
	if boundResp != nil {
		t.Fatal("不完整 bind 不得创建任务")
	}

	// 2. 三来源（normal / agent / is_test）新旧记录互斥
	// 2a. 普通先占 agent-<approval>-0 -> Agent 审批同键必返 409，普通同载重放 200，SubmitTest 必返 409
	normReq := validCreateReq()
	normReq.NodeID = "n1"
	normResp, err := env.svc.Create(ctx, 1, "agent-ap1-0", normReq)
	if err != nil || normResp.Items[0].Task == nil {
		t.Fatalf("普通请求建单失败：%v", err)
	}
	agentReq := validCreateReq()
	agentReq.NodeID = "n1"
	agentReq.AgentRunID = 88
	_, err = env.svc.Create(ctx, 1, "agent-ap1-0", agentReq)
	assertTaskCode(t, err, errcode.ErrIdempotencyConflict.Code)

	testKeyCtx := context.WithValue(ctx, service.TestRunKeyForTest(), "agent-ap1-0")
	_, err = env.svc.SubmitTest(testKeyCtx, 1, env.registry.snap, map[string]any{"prompt": "一只猫"})
	assertTaskCode(t, err, errcode.ErrIdempotencyConflict.Code)

	// 2b. SubmitTest 先占（含冲突回捞路径），Agent/普通同键均 409
	tKeyCtx2 := context.WithValue(ctx, service.TestRunKeyForTest(), "test-key-own")
	tv, err := env.svc.SubmitTest(tKeyCtx2, 1, env.registry.snap, map[string]any{"prompt": "一只猫"})
	if err != nil || tv.ID == 0 {
		t.Fatalf("SubmitTest 建单失败：%v", err)
	}
	normSameKey := validCreateReq()
	normSameKey.NodeID = "n1"
	_, err = env.svc.Create(ctx, 1, "test-key-own", normSameKey)
	assertTaskCode(t, err, errcode.ErrIdempotencyConflict.Code)

	// 3. SubmitTest 老空签名带 Fanout:true (count 默认 1) 保留在落库 InputJSON，走快返与冲突回捞均可重放
	snapFan := v21MultiRegistry(10).snap
	inFan, _ := json.Marshal(map[string]any{"op": modelcfg.OpT2V, "prompt": "一只猫", "resolution": "720p", "count": 1.0})
	snapFanJSON, _ := json.Marshal(snapFan)
	legacyTestTask := env.repo.addTask(model.GenerationTask{
		UserID: 1, IsTest: true, AgentRunID: 0, Kind: model.KindVideo, ModelKey: "m1",
		IdempotencyKey: "legacy-test-fan", InputJSON: inFan, ConfigSnapshot: snapFanJSON,
		RequestIntentSig: "", RequestBatchIndex: 0, Status: model.TaskPending,
	})
	tFanCtx := context.WithValue(ctx, service.TestRunKeyForTest(), "legacy-test-fan")
	// 3a. 快速路径命中
	gotFast, err := env.svc.SubmitTest(tFanCtx, 1, snapFan, map[string]any{"prompt": "一只猫", "resolution": "720p", "count": 1.0})
	if err != nil || gotFast.ID != legacyTestTask.ID {
		t.Fatalf("SubmitTest 老空签名带 fanout 快返应 200 返回原任务：err=%v got=%+v", err, gotFast)
	}
	// 3b. 冲突回捞路径
	env.repo.hideKeyLookups = 1
	gotRace, err := env.svc.SubmitTest(tFanCtx, 1, snapFan, map[string]any{"prompt": "一只猫", "resolution": "720p", "count": 1.0})
	if err != nil || gotRace.ID != legacyTestTask.ID {
		t.Fatalf("SubmitTest 冲突回捞应 200 返回原任务：err=%v got=%+v", err, gotRace)
	}
	// 3c. 跨来源持同一老空签名 -> 409
	_, err = env.svc.Create(ctx, 1, "legacy-test-fan", validCreateReq())
	assertTaskCode(t, err, errcode.ErrIdempotencyConflict.Code)
}

func TestGenerationTaskService_V21_IntentReplayAndLegacyParity(t *testing.T) {
	ctx := context.Background()
	reg := v21MultiRegistry(10)
	env := newTaskSvcEnv(&fakeLimits{initial: 50, defMax: 100})
	env.registry = reg
	env.svc = service.NewGenerationTaskService(service.GenerationTaskDeps{
		Repo: env.repo, Registry: reg, Executor: env.exec, Assets: env.assets, Broadcaster: env.bc, Limits: &fakeLimits{initial: 50, defMax: 100},
	})
	env.repo.addCredit(1, 500, 0)
	env.assets.items = map[uint64]*model.Asset{
		123: {ID: 123, UserID: 1, Kind: model.KindImage, ByteSize: 100},
	}

	// 1. 跨键拼单（K -> [A], K#1 -> [B], 然后 K + [A,B]）-> 409 且原各自键仍可重放
	reqA := &model.CreateGenerationTaskReq{Kind: model.KindVideo, ModelID: "m1", NodeIDs: []string{"A"}, Input: map[string]any{"prompt": "cat", "count": 1}}
	reqB := &model.CreateGenerationTaskReq{Kind: model.KindVideo, ModelID: "m1", NodeIDs: []string{"B"}, Input: map[string]any{"prompt": "cat", "count": 1}}
	respA, err := env.svc.Create(ctx, 1, "K-splice", reqA)
	if err != nil {
		t.Fatal(err)
	}
	respB, err := env.svc.Create(ctx, 1, "K-splice#1", reqB)
	if err != nil {
		t.Fatal(err)
	}
	_, err = env.svc.Create(ctx, 1, "K-splice", &model.CreateGenerationTaskReq{
		Kind: model.KindVideo, ModelID: "m1", NodeIDs: []string{"A", "B"}, Input: map[string]any{"prompt": "cat", "count": 2},
	})
	assertTaskCode(t, err, errcode.ErrIdempotencyConflict.Code)
	replayA, err := env.svc.Create(ctx, 1, "K-splice", reqA)
	if err != nil || replayA.Items[0].Task.ID != respA.Items[0].Task.ID {
		t.Fatalf("原 K 重放失败：%v", err)
	}
	replayB, err := env.svc.Create(ctx, 1, "K-splice#1", reqB)
	if err != nil || replayB.Items[0].Task.ID != respB.Items[0].Task.ID {
		t.Fatalf("原 K#1 重放失败：%v", err)
	}

	// 2. 多节点整批正向重放（K + [A,B], count=2）& 3(c) 改默认值后同载重放 200 + 2. 同内容跨下标重放 K#1 -> 409
	batchReq := &model.CreateGenerationTaskReq{
		Kind: model.KindVideo, ModelID: "m1", NodeIDs: []string{"A", "B"}, Input: map[string]any{"prompt": "多镜", "count": 2},
	}
	msgsBefore := len(env.bc.msgs)
	batch1, err := env.svc.Create(ctx, 1, "K-batch", batchReq)
	if err != nil || len(batch1.Items) != 2 {
		t.Fatalf("多节点建单失败：err=%v resp=%+v", err, batch1)
	}
	if len(env.bc.msgs) != msgsBefore+2 {
		t.Fatalf("首次多节点应推送 2 次，实际 %d", len(env.bc.msgs)-msgsBefore)
	}
	t0, _ := env.repo.GetByID(ctx, 1, uint64(batch1.Items[0].Task.ID))
	t1, _ := env.repo.GetByID(ctx, 1, uint64(batch1.Items[1].Task.ID))
	if t0.RequestIntentSig == "" || t0.RequestBatchIndex != 0 || t1.RequestBatchIndex != 1 {
		t.Fatalf("RequestIntentSig / RequestBatchIndex 落库异常：t0=%+v t1=%+v", t0, t1)
	}
	// 同内容以 K#1 提交 -> 409
	_, err = env.svc.Create(ctx, 1, "K-batch#1", batchReq)
	assertTaskCode(t, err, errcode.ErrIdempotencyConflict.Code)

	// 修改模型 Default（720p -> 1080p）并重放同一原始请求 -> 仍 200 且零重复推送、零新冻结
	reg.snap.Model.Capabilities.Params[0].Default = "1080p"
	creditBefore := env.repo.credits[1].Frozen
	msgsBefore = len(env.bc.msgs)
	batchReplay, err := env.svc.Create(ctx, 1, "K-batch", batchReq)
	if err != nil || batchReplay.Items[0].Task.ID != batch1.Items[0].Task.ID || batchReplay.Items[1].Task.ID != batch1.Items[1].Task.ID {
		t.Fatalf("改默认值后重放应 200 返回原整批：err=%v resp=%+v", err, batchReplay)
	}
	if len(env.bc.msgs) != msgsBefore || env.repo.credits[1].Frozen != creditBefore {
		t.Fatalf("重放不应二次冻结或推送：frozen=%d msgs=%d", env.repo.credits[1].Frozen, len(env.bc.msgs)-msgsBefore)
	}

	// 3(b) 模型 M1 下架后改传 M2 + 同键 K-batch -> rawIntentSig 失配必返 409
	reg.snapErr = provider.ErrModelUnavailable
	_, err = env.svc.Create(ctx, 1, "K-batch", &model.CreateGenerationTaskReq{
		Kind: model.KindVideo, ModelID: "m2", NodeIDs: []string{"A", "B"}, Input: map[string]any{"prompt": "多镜", "count": 2},
	})
	assertTaskCode(t, err, errcode.ErrIdempotencyConflict.Code)

	// 4. 部分命中签名一致但模型已下架 -> ErrModelUnavailable，A 不变，B 零新建
	snapKeep := fakeTaskSnapshot(model.KindVideo, 10)
	partSigReq := &model.CreateGenerationTaskReq{Kind: model.KindVideo, ModelID: "m1", NodeIDs: []string{"A", "B"}, Input: map[string]any{"prompt": "部分", "count": 2}}
	reg.snapErr = nil
	reg.snap = snapKeep
	// 手工预置 K-part[0]（模拟写到第 0 项后或外置状态存在第 0 项）再测第 1 项需补建但模型下架
	p0Resp, err := env.svc.Create(ctx, 1, "K-tmp-part", &model.CreateGenerationTaskReq{Kind: model.KindVideo, ModelID: "m1", NodeIDs: []string{"A"}, Input: map[string]any{"prompt": "部分", "count": 1}})
	if err != nil {
		t.Fatal(err)
	}
	// 用同用户 K-part0-only 手工写一条“意图与 partSigReq 完全一致的”第 0 项任务
	// 通过先提交 partSigReq 后删第 1 项来模拟真部分命中
	reg.snap = v21MultiRegistry(10).snap
	pFull, err := env.svc.Create(ctx, 1, "K-part", partSigReq)
	if err != nil || len(pFull.Items) != 2 {
		t.Fatal(err)
	}
	_ = p0Resp
	delete(env.repo.tasks, uint64(pFull.Items[1].Task.ID))
	reg.snapErr = provider.ErrModelUnavailable
	_, err = env.svc.Create(ctx, 1, "K-part", partSigReq)
	assertTaskCode(t, err, errcode.ErrModelUnavailable.Code)
	if len(env.repo.tasks) == 0 || env.repo.tasks[uint64(pFull.Items[0].Task.ID)] == nil {
		t.Fatal("已有第 0 项应完好保留")
	}

	// 3(a) 仅预置 K-only1#1 -> [B]，发 K-only1 + [A,B] -> 409
	reg.snapErr = nil
	_, err = env.svc.Create(ctx, 1, "K-only1#1", reqB)
	if err != nil {
		t.Fatal(err)
	}
	_, err = env.svc.Create(ctx, 1, "K-only1", &model.CreateGenerationTaskReq{
		Kind: model.KindVideo, ModelID: "m1", NodeIDs: []string{"A", "B"}, Input: map[string]any{"prompt": "cat", "count": 2},
	})
	assertTaskCode(t, err, errcode.ErrIdempotencyConflict.Code)

	// 3(d) 老空签名带 count=1 扇出剔除 + 参考图 images:[123] 落库为 "123"：模型停用/改默认值后同载重放 200，改参或 len>1 返 409
	legacySnap := v21MultiRegistry(10).snap
	legacySnapJSON, _ := json.Marshal(legacySnap)
	legacyInputJSON, _ := json.Marshal(map[string]any{"op": modelcfg.OpI2V, "prompt": "cat", "resolution": "720p", "images": []string{"123"}})
	legacyTask := env.repo.addTask(model.GenerationTask{
		UserID: 1, IsTest: false, AgentRunID: 0, Kind: model.KindVideo, ModelKey: "m1",
		CanvasProjectID: ptrUint64(9), NodeID: "seg-legacy", IdempotencyKey: "k-legacy-0035",
		InputJSON: legacyInputJSON, ConfigSnapshot: legacySnapJSON, RequestIntentSig: "", RequestBatchIndex: 0, Status: model.TaskPending,
	})
	reg.snapErr = provider.ErrModelUnavailable
	legReq := &model.CreateGenerationTaskReq{
		Kind: model.KindVideo, ModelID: "m1", CanvasID: 9, NodeID: "seg-legacy",
		Input: map[string]any{"op": modelcfg.OpI2V, "prompt": "cat", "count": 1, "images": []any{123}},
	}
	legReplay, err := env.svc.Create(ctx, 1, "k-legacy-0035", legReq)
	if err != nil || legReplay.Items[0].Task.ID != legacyTask.ID {
		t.Fatalf("老空签名同载重放失败（含 fanout 剔除与素材转字符串）：err=%v resp=%+v", err, legReplay)
	}
	// 改参数或 len(nodes)>1 -> 409
	_, err = env.svc.Create(ctx, 1, "k-legacy-0035", &model.CreateGenerationTaskReq{
		Kind: model.KindVideo, ModelID: "m1", CanvasID: 9, NodeID: "seg-legacy",
		Input: map[string]any{"op": modelcfg.OpI2V, "prompt": "dog", "count": 1, "images": []any{123}},
	})
	assertTaskCode(t, err, errcode.ErrIdempotencyConflict.Code)
	_, err = env.svc.Create(ctx, 1, "k-legacy-0035", &model.CreateGenerationTaskReq{
		Kind: model.KindVideo, ModelID: "m1", CanvasID: 9, NodeIDs: []string{"seg-legacy", "seg-legacy"},
		Input: map[string]any{"op": modelcfg.OpI2V, "prompt": "cat", "count": 2, "images": []any{123}},
	})
	assertTaskCode(t, err, errcode.ErrIdempotencyConflict.Code)

	// 5(d) AIConfigService.TestRunIdempotent 默认值漂移重放
	aic, aicRepo, _, _ := aicRunSetup(t)
	reg.snapErr = nil
	aicTaskEnv := newTaskSvcEnv(defaultTaskLimits())
	aicTaskEnv.registry = reg
	aic.SetTestTaskCreator(aicTaskEnv.svc)
	mBody720 := aicModelBody("m1", "c1", `"capabilities":{"ops":["t2v"],"prompt":{"max_length":2000},"params":{"resolution":{"type":"enum","label":"分辨率","open":true,"options":["720p","1080p"],"default":"720p"}}}`)
	if _, err := aicSave(aic, "m1", false, mBody720); err != nil {
		t.Fatal(err)
	}
	tr1, err := aic.TestRunIdempotent(ctx, 1, "m1", map[string]any{"prompt": "hi"}, "aic-key-1")
	if err != nil {
		t.Fatalf("TestRunIdempotent 首跑失败：%v", err)
	}
	mBody1080 := aicModelBody("m1", "c1", `"capabilities":{"ops":["t2v"],"prompt":{"max_length":2000},"params":{"resolution":{"type":"enum","label":"分辨率","open":true,"options":["720p","1080p"],"default":"1080p"}}}`)
	if _, err := aicSave(aic, "m1", false, mBody1080); err != nil {
		t.Fatal(err)
	}
	_ = aicRepo
	tr2, err := aic.TestRunIdempotent(ctx, 1, "m1", map[string]any{"prompt": "hi"}, "aic-key-1")
	if err != nil || tr2.ID != tr1.ID {
		t.Fatalf("TestRunIdempotent 默认值漂移后重放应返回原任务：err=%v tr1=%+v tr2=%+v", err, tr1, tr2)
	}
	_, err = aic.TestRunIdempotent(ctx, 1, "m1", map[string]any{"prompt": "changed"}, "aic-key-1")
	assertTaskCode(t, err, errcode.ErrIdempotencyConflict.Code)
}

type sqlRecorder struct {
	gormlogger.Interface
	mu   sync.Mutex
	sqls []string
}

func (l *sqlRecorder) LogMode(level gormlogger.LogLevel) gormlogger.Interface { return l }
func (l *sqlRecorder) Info(context.Context, string, ...any)                   {}
func (l *sqlRecorder) Warn(context.Context, string, ...any)                   {}
func (l *sqlRecorder) Error(context.Context, string, ...any)                  {}
func (l *sqlRecorder) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	s, _ := fc()
	l.mu.Lock()
	l.sqls = append(l.sqls, s)
	l.mu.Unlock()
}

type failingEnqueuer struct {
	calls atomic.Int32
	failN int32
}

func (f *failingEnqueuer) EnqueueTx(_ context.Context, _ *sql.Tx, _ uint64, _ string, _ time.Time) error {
	if f.calls.Add(1) == f.failN {
		return errors.New("injected enqueue boom")
	}
	return nil
}

func (f *failingEnqueuer) EnqueueAgentNotifyTx(context.Context, *sql.Tx, uint64, uint64) error {
	return nil
}

func TestGenerationTaskService_V21_PG_BatchAtomicityConcurrencyAndSQLInjections(t *testing.T) {
	ctx := context.Background()

	// 1. 7(a) 无账户用户全批业务失败（429 上限=0）-> 零 user_credits 与 zero initial 流水
	t.Run("无账户用户全批业务失败零开户", func(t *testing.T) {
		lim := taskLimitsFunc{initial: 50, maxActive: 0, mult: 1}
		env := newGTIEnv(t, lim)
		env.registry.snap = v21MultiRegistry(10).snap
		resp, err := env.svc.Create(ctx, env.user, "fail-all", &model.CreateGenerationTaskReq{
			Kind: model.KindVideo, ModelID: "m1", NodeIDs: []string{"A", "B"}, Input: map[string]any{"prompt": "x", "count": 2},
		})
		if err != nil || len(resp.Items) != 2 || resp.Items[0].Error == nil || resp.Items[1].Error == nil {
			t.Fatalf("预期逐项返回 429：err=%v resp=%+v", err, resp)
		}
		var ucCount, ledgerCount int64
		env.db.Model(&model.UserCredit{}).Where("user_id = ?", env.user).Count(&ucCount)
		env.db.Model(&model.CreditLedger{}).Where("user_id = ?", env.user).Count(&ledgerCount)
		if ucCount != 0 || ledgerCount != 0 {
			t.Fatalf("全批失败不应留下任何开户或 initial 流水：uc=%d ledger=%d", ucCount, ledgerCount)
		}
	})

	// 2. 7(b) 部分失败 A 成 B 402 + SQL 轨迹硬断言（零 GORM 隐式 SAVEPOINT，仅 sp_batch_<idx>）
	t.Run("部分成功且SQL轨迹仅含sp_batch保存点", func(t *testing.T) {
		lim := taskLimitsFunc{initial: 15, maxActive: 5, mult: 1}
		env := newGTIEnv(t, lim)
		rec := &sqlRecorder{}
		env.db.Logger = rec
		env.registry.snap = v21MultiRegistry(10).snap
		resp, err := env.svc.Create(ctx, env.user, "part-ok", &model.CreateGenerationTaskReq{
			Kind: model.KindVideo, ModelID: "m1", NodeIDs: []string{"A", "B"}, Input: map[string]any{"prompt": "cat", "count": 2},
		})
		if err != nil || len(resp.Items) != 2 || resp.Items[0].Task == nil || resp.Items[1].Error == nil || resp.Items[1].Error.Code != errcode.ErrInsufficientCredits.Code {
			t.Fatalf("期望 A 成 B 402：err=%v resp=%+v", err, resp)
		}
		if len(env.bc.msgs) != 1 {
			t.Fatalf("仅应推送成功项 A (1次)，实际 %d", len(env.bc.msgs))
		}
		env.assertReconciled(t)
		// 检查 SQL Trace
		rec.mu.Lock()
		sqls := append([]string(nil), rec.sqls...)
		rec.mu.Unlock()
		var spCount int
		for _, s := range sqls {
			upper := strings.ToUpper(s)
			if strings.Contains(upper, "SAVEPOINT") {
				if !strings.Contains(s, "sp_batch_") {
					t.Fatalf("发现未受控的隐式 GORM 保存点 SQL：%s", s)
				}
				spCount++
			}
		}
		if spCount < 4 {
			t.Fatalf("期望包含 SAVEPOINT/RELEASE/ROLLBACK sp_batch_<idx>，实际仅有 %d 条：%v", spCount, sqls)
		}
	})

	// 3. 6(o) 注入第 1 项 EnqueueTask 失败（第 0 项已放行）触发整批回滚 + 零推送
	t.Run("注入第1项EnqueueTask失败整批回滚且零推送", func(t *testing.T) {
		env := newGTIEnv(t, taskLimitsFunc{initial: 500, maxActive: 50, mult: 1})
		env.registry.snap = v21MultiRegistry(10).snap
		enq := &failingEnqueuer{failN: 2}
		env.store.SetEnqueuer(enq)
		_, err := env.svc.Create(ctx, env.user, "enq-fail", &model.CreateGenerationTaskReq{
			Kind: model.KindVideo, ModelID: "m1", NodeIDs: []string{"A", "B"}, Input: map[string]any{"prompt": "cat", "count": 2},
		})
		if err == nil {
			t.Fatal("预期系统错误中止整批")
		}
		if len(env.bc.msgs) != 0 {
			t.Fatalf("系统错误整批回滚禁止触发推送：%d", len(env.bc.msgs))
		}
		var taskCnt, ledCnt, evCnt, ucCnt int64
		env.db.Model(&model.GenerationTask{}).Where("user_id = ?", env.user).Count(&taskCnt)
		env.db.Model(&model.CreditLedger{}).Where("user_id = ?", env.user).Count(&ledCnt)
		env.db.Model(&model.GenerationTaskEvent{}).Count(&evCnt)
		env.db.Model(&model.UserCredit{}).Where("user_id = ?", env.user).Count(&ucCnt)
		if taskCnt != 0 || ledCnt != 0 || evCnt != 0 || ucCnt != 0 {
			t.Fatalf("期望全外层回滚零残留，实际 tasks=%d ledger=%d events=%d uc=%d", taskCnt, ledCnt, evCnt, ucCnt)
		}
	})

	// 4. 6(o) 六处 SQL 错误注入：TxEnsureCredit / generation_tasks / generation_task_events / ExecSavepoint / RollbackSavepoint / ReleaseSavepoint
	for _, inj := range []string{"ensure_credit", "insert_task", "insert_event", "exec_sp", "rollback_sp", "release_sp"} {
		t.Run("SQL注入_"+inj, func(t *testing.T) {
			lim := taskLimitsFunc{initial: 100, maxActive: 5, mult: 1}
			if inj == "rollback_sp" {
				lim.initial = 15 // 使第 1 项 402 触发 RollbackSavepoint
			}
			env := newGTIEnv(t, lim)
			env.registry.snap = v21MultiRegistry(10).snap
			switch inj {
			case "ensure_credit":
				_ = env.db.Callback().Create().Before("gorm:create").Register("test_fail_uc", func(tx *gorm.DB) {
					if tx.Statement.Table == "user_credits" {
						_ = tx.AddError(errors.New("boom ensure credit"))
					}
				})
			case "insert_task":
				_ = env.db.Callback().Create().Before("gorm:create").Register("test_fail_task", func(tx *gorm.DB) {
					if tx.Statement.Table == "generation_tasks" {
						_ = tx.AddError(errors.New("boom insert task"))
					}
				})
			case "insert_event":
				_ = env.db.Callback().Create().Before("gorm:create").Register("test_fail_event", func(tx *gorm.DB) {
					if tx.Statement.Table == "generation_task_events" {
						_ = tx.AddError(errors.New("boom insert event"))
					}
				})
			case "exec_sp":
				_ = env.db.Callback().Raw().Before("gorm:raw").Register("test_fail_exec_sp", func(tx *gorm.DB) {
					if strings.HasPrefix(tx.Statement.SQL.String(), "SAVEPOINT sp_batch_") {
						_ = tx.AddError(errors.New("boom exec sp"))
					}
				})
			case "rollback_sp":
				_ = env.db.Callback().Raw().Before("gorm:raw").Register("test_fail_rb_sp", func(tx *gorm.DB) {
					if strings.HasPrefix(tx.Statement.SQL.String(), "ROLLBACK TO SAVEPOINT sp_batch_") {
						_ = tx.AddError(errors.New("boom rb sp"))
					}
				})
			case "release_sp":
				_ = env.db.Callback().Raw().Before("gorm:raw").Register("test_fail_rel_sp", func(tx *gorm.DB) {
					if strings.HasPrefix(tx.Statement.SQL.String(), "RELEASE SAVEPOINT sp_batch_") {
						_ = tx.AddError(errors.New("boom rel sp"))
					}
				})
			}
			_, err := env.svc.Create(ctx, env.user, "inj-"+inj, &model.CreateGenerationTaskReq{
				Kind: model.KindVideo, ModelID: "m1", NodeIDs: []string{"A", "B"}, Input: map[string]any{"prompt": "cat", "count": 2},
			})
			if err == nil {
				t.Fatalf("注入 %s 应直接返系统错误中止整批", inj)
			}
			var taskCnt, ledCnt, evCnt, ucCnt int64
			env.db.Model(&model.GenerationTask{}).Where("user_id = ?", env.user).Count(&taskCnt)
			env.db.Model(&model.CreditLedger{}).Where("user_id = ?", env.user).Count(&ledCnt)
			env.db.Model(&model.GenerationTaskEvent{}).Count(&evCnt)
			env.db.Model(&model.UserCredit{}).Where("user_id = ?", env.user).Count(&ucCnt)
			if taskCnt != 0 || ledCnt != 0 || evCnt != 0 || ucCnt != 0 {
				t.Fatalf("注入 %s 后仍有 PG 残留：tasks=%d ledger=%d events=%d uc=%d", inj, taskCnt, ledCnt, evCnt, ucCnt)
			}
		})
	}

	// 5. 6(a) 并发 (K + [A,B], count=2) x (K#1 -> [C], count=1) + 6(f) 预锁空隙并发插 seg:B + 6(b) 并发带键/无键 mkt:S
	t.Run("并发批拼单_预锁空隙与营销带键无键竞态", func(t *testing.T) {
		env := newGTIEnv(t, taskLimitsFunc{initial: 500, maxActive: 50, mult: 1})
		env.registry.snap = v21MultiRegistry(10).snap
		_ = env.db.AutoMigrate(&model.DramaProject{}, &model.DramaEpisode{}, &model.DramaSegment{}, &model.DramaSegmentFrameRun{}, &model.DramaSegmentTakeLink{}, &model.DramaJob{}, &model.MarketingProject{}, &model.MarketingRound{}, &model.MarketingScript{})

		// 6(a) K+[A,B] vs K#1->C
		start := make(chan struct{})
		var errA, errB error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, errA = env.svc.Create(ctx, env.user, "conc-K", &model.CreateGenerationTaskReq{
				Kind: model.KindVideo, ModelID: "m1", NodeIDs: []string{"A", "B"}, Input: map[string]any{"prompt": "x", "count": 2},
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, errB = env.svc.Create(ctx, env.user, "conc-K#1", &model.CreateGenerationTaskReq{
				Kind: model.KindVideo, ModelID: "m1", NodeIDs: []string{"C"}, Input: map[string]any{"prompt": "x", "count": 1},
			})
		}()
		close(start)
		wg.Wait()
		// 若 K#1 先占则 K+[A,B] 必 409；若 K 先占 [A,B] 则 K#1 必 409（异载冲突），绝不能互相拼单或死锁
		var cA, cB *errcode.Error
		errors.As(errA, &cA)
		errors.As(errB, &cB)
		if (errA == nil) == (errB == nil) {
			t.Fatalf("并发 K+[A,B] 与 K#1->C 只能一胜一 409，实际 errA=%v errB=%v", errA, errB)
		}
		if (cA != nil && cA.Code != errcode.ErrIdempotencyConflict.Code) || (cB != nil && cB.Code != errcode.ErrIdempotencyConflict.Code) {
			t.Fatalf("并发冲突方应返回 409，实际 errA=%v errB=%v", errA, errB)
		}
		env.assertReconciled(t)

		// 6(b) 营销 mkt:S 带键与无键并发提交
		mp := model.MarketingProject{UserID: env.user, Title: "带货"}
		if err := env.db.Create(&mp).Error; err != nil {
			t.Fatal(err)
		}
		mr := model.MarketingRound{UserID: env.user, ProjectID: mp.ID, No: 1, Settings: datatypes.JSON(`{}`), Status: model.MarketingRoundReady}
		if err := env.db.Create(&mr).Error; err != nil {
			t.Fatal(err)
		}
		ms := model.MarketingScript{UserID: env.user, ProjectID: mp.ID, RoundID: mr.ID, StepRunID: 1, Kind: "create", Angle: "a", Shots: datatypes.JSON(`[]`), Original: datatypes.JSON(`{}`), Status: model.MarketingScriptDraft}
		if err := env.db.Create(&ms).Error; err != nil {
			t.Fatal(err)
		}
		mktNode := model.MarketingNodePrefix + idcodec.Encode(ms.ID)
		mktStart := make(chan struct{})
		var mErr1, mErr2 error
		var mWg sync.WaitGroup
		mWg.Add(2)
		go func() {
			defer mWg.Done()
			<-mktStart
			_, mErr1 = env.svc.Create(ctx, env.user, "mkt-key", &model.CreateGenerationTaskReq{Kind: model.KindVideo, ModelID: "m1", NodeID: mktNode, Input: map[string]any{"prompt": "p", "count": 1}})
		}()
		go func() {
			defer mWg.Done()
			<-mktStart
			_, mErr2 = env.svc.Create(ctx, env.user, "", &model.CreateGenerationTaskReq{Kind: model.KindVideo, ModelID: "m1", NodeID: mktNode, Input: map[string]any{"prompt": "p", "count": 1}})
		}()
		close(mktStart)
		mWg.Wait()
		if mErr1 != nil || mErr2 != nil {
			t.Fatalf("系统错误：%v %v", mErr1, mErr2)
		}
		var mktActiveCount int64
		env.db.Model(&model.GenerationTask{}).Where("user_id = ? AND node_id = ?", env.user, mktNode).Count(&mktActiveCount)
		if mktActiveCount != 1 {
			t.Fatalf("同一营销脚本同时仅应生成 1 个出片任务，实际 %d", mktActiveCount)
		}
		env.assertReconciled(t)

		// 6(f) 预锁空隙并发插段 seg:missing -> 持 UserCredit 期间并发插 B 不死锁，且 B 返回 NotFound
		dp := model.DramaProject{UserID: env.user, CanvasID: 99, Title: "隙间", SourceText: "x", StyleKey: "c"}
		_ = env.db.Create(&dp).Error
		now := time.Now()
		de := model.DramaEpisode{UserID: env.user, DramaID: dp.ID, No: 1, LockedAt: &now}
		_ = env.db.Create(&de).Error
		segA := model.DramaSegment{UserID: env.user, EpisodeID: de.ID, Seq: 0, Seconds: 5}
		_ = env.db.Create(&segA).Error
		missingSID := segA.ID + 9999
		var gapOnce sync.Once
		gapDone := make(chan struct{})
		_ = env.db.Callback().Query().After("gorm:query").Register("test_gap_insert_segB", func(tx *gorm.DB) {
			if tx.Statement.Table == "user_credits" && strings.Contains(tx.Statement.SQL.String(), "FOR UPDATE") {
				gapOnce.Do(func() {
					defer close(gapDone)
					segB := model.DramaSegment{BaseModel: model.BaseModel{ID: missingSID}, UserID: env.user, EpisodeID: de.ID, Seq: 1, Seconds: 5}
					_ = env.db.Session(&gorm.Session{NewDB: true}).Create(&segB).Error
				})
			}
		})
		env.registry.snap.Model.Capabilities.Params[1].Options = []any{1.0, 2.0}
		gapResp, gapErr := env.svc.Create(ctx, env.user, "k-gap", &model.CreateGenerationTaskReq{
			Kind: model.KindVideo, ModelID: "m1", CanvasID: idcodec.ID(99),
			NodeIDs: []string{"seg:" + idcodec.Encode(segA.ID), "seg:" + idcodec.Encode(missingSID)},
			Input:   map[string]any{"prompt": "cat", "count": 2},
		})
		<-gapDone
		if gapErr != nil || len(gapResp.Items) != 2 || gapResp.Items[0].Task == nil || gapResp.Items[1].Error == nil || gapResp.Items[1].Error.Code != errcode.ErrDramaSegmentNotFound.Code {
			t.Fatalf("预锁空隙缺段应局部报 67009 且不影响 A：err=%v resp=%+v", gapErr, gapResp)
		}
	})
}

type taskLimitsFunc struct {
	initial, maxActive, mult int
}

func (l taskLimitsFunc) InitialCredits(context.Context) (int, error)         { return l.initial, nil }
func (l taskLimitsFunc) MaxActiveTasks(context.Context, uint64) (int, error) { return l.maxActive, nil }
func (l taskLimitsFunc) DefaultMaxActiveTasks(context.Context) (int, error)  { return l.maxActive, nil }
func (l taskLimitsFunc) QueueMultiplier(context.Context) (int, error)        { return l.mult, nil }
func (l taskLimitsFunc) QueueMaxWait(context.Context) (time.Duration, error) {
	return time.Hour, nil
}

func ptrUint64(v uint64) *uint64 { return &v }
