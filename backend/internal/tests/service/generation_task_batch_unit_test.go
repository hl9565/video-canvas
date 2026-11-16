package service_test

import (
	"context"
	"errors"
	"testing"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
)

func TestGenerationTaskService_BatchSavepointFailures(t *testing.T) {
	for _, method := range []string{"TxEnsureCredit", "LockCredit", "CountActive", "ExecSavepoint", "TxInsertTask", "AddCredit", "InsertLedger", "EnqueueTask", "EnqueueAgentNotify", "ReleaseSavepoint", "RollbackSavepoint"} {
		t.Run(method, func(t *testing.T) {
			limits := &fakeLimits{initial: 15, defMax: 10}
			env := newTaskSvcEnv(limits)
			env.registry.snap = v21MultiRegistry(10).snap
			injected := errors.New("injected " + method)
			env.repo.errs[method] = injected
			req := &model.CreateGenerationTaskReq{Kind: model.KindVideo, ModelID: "m1", NodeIDs: []string{"A", "B"}, AgentRunID: 17, Input: map[string]any{"prompt": "镜头", "count": 2}}
			_, err := env.svc.Create(context.Background(), 1, "batch", req)
			if !errors.Is(err, injected) {
				t.Fatalf("应中止整批：%v", err)
			}
			if len(env.repo.tasks) != 0 || len(env.repo.credits) != 0 || len(env.repo.ledger) != 0 || len(env.repo.enqueued) != 0 || len(env.repo.agentNotified) != 0 || len(env.bc.msgs) != 0 {
				t.Fatal("整批回滚后不得残留任务、开户、流水、队列、通知或广播")
			}
		})
	}
}

func TestGenerationTaskService_BatchPartialAndZeroWrites(t *testing.T) {
	ctx := context.Background()
	env := newTaskSvcEnv(&fakeLimits{initial: 15, defMax: 10})
	env.registry.snap = v21MultiRegistry(10).snap
	req := &model.CreateGenerationTaskReq{Kind: model.KindVideo, ModelID: "m1", NodeIDs: []string{"A", "B"}, AgentRunID: 12, Input: map[string]any{"prompt": "镜头", "count": 2}}
	first, err := env.svc.Create(ctx, 1, "batch", req)
	if err != nil || first.Items[0].Task == nil || first.Items[1].Error == nil || first.Items[1].Error.Code != errcode.ErrInsufficientCredits.Code {
		t.Fatalf("部分成功：%+v %v", first, err)
	}
	if env.repo.credits[1].Frozen != 10 || len(env.repo.enqueued) != 1 || len(env.repo.agentNotified) != 1 || len(env.bc.msgs) != 1 {
		t.Fatal("仅成功项可以冻结、入队及广播")
	}
	env.repo.credits[1].Balance = 100
	second, err := env.svc.Create(ctx, 1, "batch", req)
	if err != nil || second.Items[0].Task.ID != first.Items[0].Task.ID || second.Items[1].Task == nil {
		t.Fatalf("同意图部分重放应补建缺失项：%+v %v", second, err)
	}
	if len(env.repo.tasks) != 2 || len(env.repo.agentNotified) != 2 || len(env.bc.msgs) != 2 {
		t.Fatal("已有项不得重复写入")
	}
	third, err := env.svc.Create(ctx, 1, "batch", req)
	if err != nil || third.Items[1].Task.ID != second.Items[1].Task.ID || len(env.bc.msgs) != 2 {
		t.Fatal("全命中只读返回")
	}
	zero := newTaskSvcEnv(&fakeLimits{initial: 50, defMax: 0})
	zero.registry.snap = v21MultiRegistry(10).snap
	resp, err := zero.svc.Create(ctx, 1, "zero", req)
	if err != nil || resp.Items[0].Error.Code != errcode.ErrTooManyTasks.Code || len(zero.repo.credits) != 0 {
		t.Fatalf("全业务失败不残留开户：%+v %v", resp, err)
	}
}
