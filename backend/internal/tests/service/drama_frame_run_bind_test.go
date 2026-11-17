package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/provider"
	"video-canvas/internal/service"
	"video-canvas/internal/service/dramasegmentfake"
)

func TestFrameRunSubmittingKeyConflictsPreTx(t *testing.T) {
	for _, oldRev := range []int64{0, 1} {
		t.Run(string(rune('a'+oldRev)), func(t *testing.T) {
			e := newTaskSvcEnv(defaultTaskLimits())
			req := validCreateReq()
			task, err := createSingle(context.Background(), e.svc, 1, "occupied", req)
			if err != nil {
				t.Fatal(err)
			}
			req.NodeID = "seg:" + idcodec.Encode(1)
			e.repo.tasks[task.ID].NodeID = req.NodeID
			e.repo.tasks[task.ID].RequestIntentSig = ""
			if oldRev != 0 {
				run := uint64(31)
				e.repo.tasks[task.ID].FrameRunID = &run
				e.repo.tasks[task.ID].FrameRunConfirmRev = &oldRev
			}
			_, err = e.svc.CreateWithOptions(context.Background(), 1, "occupied", req, service.CreateTaskOptions{DramaRunBind: &service.DramaFrameRunBind{RunID: 31, ConfirmRev: 2}})
			assertTaskCode(t, err, errcode.ErrIdempotencyConflict.Code)
		})
	}
}

func TestFrameRunBind_RejectPublicInternalKey(t *testing.T) {
	e := newTaskSvcEnv(defaultTaskLimits())
	ctx := context.WithValue(context.Background(), service.TestRunKeyForTest(), service.InternalDramaFrameRunKeyPrefix+"x")
	_, err := e.svc.SubmitTest(ctx, 1, e.registry.snap, map[string]any{"prompt": "video"})
	assertTaskCode(t, err, errcode.ErrInvalidParams.Code)
	_, err = segmentService(dramasegmentfake.New()).Generate(context.Background(), 1, 1, service.InternalDramaFrameRunKeyPrefix+"x", &model.GenerateDramaSegmentReq{ModelID: "m1", Input: map[string]any{}})
	assertTaskCode(t, err, errcode.ErrInvalidParams.Code)
	_, err = e.svc.Create(context.Background(), 1, service.InternalDramaFrameRunKeyPrefix+"x", validCreateReq())
	assertTaskCode(t, err, errcode.ErrInvalidParams.Code)
	_, err = e.svc.CreateWithOptions(context.Background(), 1, "internal", &model.CreateGenerationTaskReq{AgentRunID: 1}, service.CreateTaskOptions{DramaRunBind: &service.DramaFrameRunBind{RunID: 1, ConfirmRev: 1}})
	assertTaskCode(t, err, errcode.ErrInvalidParams.Code)
}

type frameBindEnv struct {
	registry *fakeTaskRegistry
	assets   *segEditAssets
	db       *gorm.DB
	user     uint64
	tasks    *service.GenerationTaskService
	segments *service.DramaSegmentService
	p        *model.DramaProject
	ep       *model.DramaEpisode
	seg      *model.DramaSegment
	run      *model.DramaSegmentFrameRun
	req      *model.CreateGenerationTaskReq
	opts     service.CreateTaskOptions
}

func newFrameBindPG(t *testing.T) *frameBindEnv {
	t.Helper()
	now := time.Now()
	db, user, registry, tasks, segs, assets := segEditPGEnv(t, &now)
	assertSlice2PG16(t, db)
	p, ep, seg, _, _ := seedDramaEpisodeWithTakes(t, db, assets, user, 0, true)
	mustTakeSQL(t, db.Model(seg).Updates(map[string]any{"chosen_task_id": nil, "chosen_cleared": true}))
	seg.ChosenTaskID, seg.ChosenCleared = nil, true
	req := &model.CreateGenerationTaskReq{Kind: model.KindVideo, ModelID: "m1", CanvasID: idcodec.ID(p.CanvasID), NodeID: "seg:" + idcodec.Encode(seg.ID), Input: map[string]any{"prompt": "首帧视频", "duration": 5}}
	quote, err := tasks.Quote(context.Background(), user, &model.QuoteGenerationTaskReq{Kind: req.Kind, ModelID: req.ModelID, CanvasID: req.CanvasID, NodeID: req.NodeID, Input: req.Input}, service.QuoteTaskOptions{SegmentID: seg.ID, SegmentRevision: seg.Revision})
	if err != nil {
		t.Fatal(err)
	}
	req.QuoteToken = quote.QuoteToken
	hash := strings.Repeat("a", 64)
	run := &model.DramaSegmentFrameRun{UserID: user, SegmentID: seg.ID, RequestKey: "bind-run", RequestHash: hash, FrameSource: model.FrameSourceExisting, State: model.FrameRunStateSubmitting, SegmentRevision: seg.Revision, Revision: 1, ConfirmedVersion: 2, VideoIdempotencyKey: service.InternalDramaFrameRunKeyPrefix + "bind-run", ModelKey: req.ModelID, ConfirmedInputHash: &hash}
	mustTakeSQL(t, db.Create(run))
	return &frameBindEnv{registry: registry, assets: assets, db: db, user: user, tasks: tasks, segments: segs, p: p, ep: ep, seg: seg, run: run, req: req, opts: service.CreateTaskOptions{DramaRunBind: &service.DramaFrameRunBind{RunID: run.ID, ConfirmRev: run.ConfirmedVersion}}}
}

func assertSlice2PG16(t *testing.T, db *gorm.DB) {
	t.Helper()
	var version int
	mustTakeSQL(t, db.Raw("SHOW server_version_num").Scan(&version))
	if version < 160000 || version >= 170000 {
		t.Fatalf("要求 PostgreSQL16，实际 %d", version)
	}
}

func mustTakeSQL(t *testing.T, result *gorm.DB) {
	t.Helper()
	if result.Error != nil {
		t.Fatal(result.Error)
	}
}

func (e *frameBindEnv) create(ctx context.Context) (*model.GenerationTaskView, error) {
	resp, err := e.tasks.CreateWithOptions(ctx, e.user, e.run.VideoIdempotencyKey, e.req, e.opts)
	if err != nil {
		return nil, err
	}
	if len(resp.Items) != 1 {
		return nil, fmt.Errorf("期望单镜: %+v", resp)
	}
	item := resp.Items[0]
	if item.Error != nil {
		return nil, errcode.New(item.Error.Code, item.Error.Message, item.Error.Status)
	}
	return item.Task, nil
}

func (e *frameBindEnv) reloadRun(t *testing.T) model.DramaSegmentFrameRun {
	t.Helper()
	var run model.DramaSegmentFrameRun
	mustTakeSQL(t, e.db.First(&run, e.run.ID))
	return run
}

type bindAccountSnapshot struct {
	Credit model.UserCredit
	Ledger []model.CreditLedger
	Tasks  []model.GenerationTask
}

func (e *frameBindEnv) account(t *testing.T) bindAccountSnapshot {
	t.Helper()
	var result bindAccountSnapshot
	mustTakeSQL(t, e.db.Where("user_id = ?", e.user).First(&result.Credit))
	mustTakeSQL(t, e.db.Where("user_id = ?", e.user).Order("id").Find(&result.Ledger))
	mustTakeSQL(t, e.db.Where("user_id = ?", e.user).Order("id").Find(&result.Tasks))
	return result
}

func TestFrameRunBind_HappyAndIdempotentReplay(t *testing.T) {
	e := newFrameBindPG(t)
	first, err := e.create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run := e.reloadRun(t)
	var task model.GenerationTask
	mustTakeSQL(t, e.db.First(&task, first.ID))
	if task.FrameRunID == nil || *task.FrameRunID != run.ID || task.FrameRunConfirmRev == nil || *task.FrameRunConfirmRev != 2 || run.VideoTaskID == nil || *run.VideoTaskID != task.ID || run.State != model.FrameRunStateVideoPending {
		t.Fatalf("双向绑定未原子写入: %+v %+v", task, run)
	}
	before := e.account(t)
	// 真并发重放只返回同一任务，完整账本与账户不变。
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	ids := make([]uint64, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := e.create(context.Background())
			errs[i] = err
			if v != nil {
				ids[i] = v.ID
			}
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil || ids[i] != first.ID {
			t.Fatalf("重放[%d]=%d/%v", i, ids[i], err)
		}
	}
	if after := e.account(t); !reflect.DeepEqual(before, after) {
		t.Fatal("重放改变账户、流水或任务")
	}
}

func (e *frameBindEnv) occupyKey(t *testing.T, bindRev int64, status string) *model.GenerationTask {
	t.Helper()
	input, _ := json.Marshal(e.req.Input)
	snap, _ := json.Marshal(fakeTaskSnapshot(model.KindVideo, 10))
	task := &model.GenerationTask{UserID: e.user, Kind: model.KindVideo, ModelKey: "m1", Provider: "demo", CanvasProjectID: &e.p.CanvasID, NodeID: e.req.NodeID, IdempotencyKey: e.run.VideoIdempotencyKey, Status: status, InputJSON: input, ConfigSnapshot: snap, Credits: 10, Version: 1}
	if bindRev != 0 {
		task.FrameRunID = &e.run.ID
		task.FrameRunConfirmRev = &bindRev
	}
	mustTakeSQL(t, e.db.Create(task))
	return task
}

func TestFrameRunSubmittingKeyConflicts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rev       int64
		different bool
	}{{"ordinary same input", 0, false}, {"earlier confirmation", 1, false}, {"ordinary different input", 0, true}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFrameBindPG(t)
			task := e.occupyKey(t, tc.rev, model.TaskPending)
			if tc.different {
				mustTakeSQL(t, e.db.Model(task).Update("input_json", []byte(`{"prompt":"other"}`)))
			}
			before := e.account(t)
			_, err := e.create(context.Background())
			assertTaskCode(t, err, errcode.ErrIdempotencyConflict.Code)
			if after := e.account(t); !reflect.DeepEqual(before, after) {
				t.Fatal("冲突改变任务、账户或完整流水")
			}
			if run := e.reloadRun(t); run.VideoTaskID != nil || run.State != model.FrameRunStateSubmitting {
				t.Fatalf("冲突绑定了任务: %+v", run)
			}
		})
	}
}

func TestFrameRunCancelOrdinaryTaskUntouched(t *testing.T) {
	e := newFrameBindPG(t)
	e.occupyKey(t, 0, model.TaskPending)
	mustTakeSQL(t, e.db.Model(e.run).Update("state", model.FrameRunStateCancelled))
	before := e.account(t)
	_, err := e.create(context.Background())
	assertTakeHTTP(t, err, 409)
	if after := e.account(t); !reflect.DeepEqual(before, after) {
		t.Fatal("取消 run 冒领并修改了普通任务或账本")
	}
	if run := e.reloadRun(t); run.State != model.FrameRunStateCancelled || run.VideoTaskID != nil {
		t.Fatalf("终态逆转: %+v", run)
	}
}

func assertTakeHTTP(t *testing.T, err error, status int) {
	t.Helper()
	var business *errcode.Error
	if !errors.As(err, &business) || business.HTTPStatus() != status {
		t.Fatalf("期望 HTTP %d: %v", status, err)
	}
}

func TestFrameRunCancelThenLateTaskNoBound(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		forward      bool
	}{{"cancelled bound task", model.TaskPending, true}, {"orphan pending refund", model.TaskPending, false}, {"finalizing never cancel", model.TaskFinalizing, false}, {"running charges frozen", model.TaskRunning, false}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFrameBindPG(t)
			task, err := e.create(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			mustTakeSQL(t, e.db.Model(&model.GenerationTask{}).Where("id = ?", task.ID).Update("status", tc.status))
			fields := map[string]any{"state": model.FrameRunStateCancelled}
			if !tc.forward {
				fields["video_task_id"] = nil
			}
			mustTakeSQL(t, e.db.Model(e.run).Updates(fields))
			before := e.account(t)
			_, err = e.create(context.Background())
			assertTaskCode(t, err, errcode.ErrDramaFrameRunConflict.Code)
			run := e.reloadRun(t)
			if run.State != model.FrameRunStateCancelled {
				t.Fatalf("已取消 run 被逆转: %+v", run)
			}
			after := e.account(t)
			var stored model.GenerationTask
			mustTakeSQL(t, e.db.First(&stored, task.ID))
			if tc.status == model.TaskFinalizing {
				if stored.Status != model.TaskFinalizing || !reflect.DeepEqual(before, after) {
					t.Fatal("finalizing 不得退冻/取消")
				}
				_, err = e.tasks.Cancel(context.Background(), e.user, task.ID)
				assertTaskCode(t, err, errcode.ErrTaskNotCancelable.Code)
			} else {
				balance := 500
				if tc.status == model.TaskRunning {
					balance = 490
				}
				if stored.Status != model.TaskCanceled || after.Credit.Frozen != 0 || after.Credit.Balance != balance {
					t.Fatalf("取消积分语义错误: %+v %+v", stored, after.Credit)
				}
			}
			links := []model.DramaSegmentTakeLink{}
			mustTakeSQL(t, e.db.Find(&links))
			if len(links) != 0 {
				t.Fatal("取消产生候选")
			}
		})
	}
}

func TestFrameRunQuoteAssertionCrossSegmentBlocked(t *testing.T) {
	e := newFrameBindPG(t)
	other := &model.DramaSegment{UserID: e.user, EpisodeID: e.ep.ID, Seq: 1, Revision: 1, Seconds: 5}
	mustTakeSQL(t, e.db.Create(other))
	quote, err := e.tasks.Quote(context.Background(), e.user, &model.QuoteGenerationTaskReq{Kind: e.req.Kind, ModelID: e.req.ModelID, CanvasID: e.req.CanvasID, NodeID: e.req.NodeID, Input: e.req.Input}, service.QuoteTaskOptions{SegmentID: other.ID, SegmentRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	e.req.QuoteToken = quote.QuoteToken
	before := e.account(t)
	_, err = e.create(context.Background())
	assertTaskCode(t, err, 91002)
	if after := e.account(t); !reflect.DeepEqual(before, after) {
		t.Fatal("跨段报价改变账本")
	}
	if run := e.reloadRun(t); run.VideoTaskID != nil || run.State != model.FrameRunStateSubmitting {
		t.Fatal("跨段报价写入绑定")
	}
}

func TestFrameRunBind_SubmittingCrashReplayAndTerminalTable(t *testing.T) {
	for _, state := range []model.DramaFrameRunState{model.FrameRunStateSubmitting, model.FrameRunStateVideoPending, model.FrameRunStateBound, model.FrameRunStateFailed} {
		t.Run(string(state), func(t *testing.T) {
			e := newFrameBindPG(t)
			first, err := e.create(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			fields := map[string]any{"state": state}
			if state == model.FrameRunStateSubmitting {
				fields["video_task_id"] = nil
			}
			mustTakeSQL(t, e.db.Model(e.run).Updates(fields))
			before := e.account(t)
			v, err := e.create(context.Background())
			if state == model.FrameRunStateFailed {
				assertTaskCode(t, err, errcode.ErrDramaFrameRunConflict.Code)
			} else if err != nil || v.ID != first.ID {
				t.Fatalf("绑定重入失败: %+v %v", v, err)
			}
			if after := e.account(t); !reflect.DeepEqual(before, after) {
				t.Fatal("重入产生新冻结或修改任务")
			}
			run := e.reloadRun(t)
			if state == model.FrameRunStateSubmitting && (run.State != model.FrameRunStateVideoPending || run.VideoTaskID == nil || *run.VideoTaskID != first.ID) {
				t.Fatalf("未补齐原子正向键: %+v", run)
			}
		})
	}
}

func TestFrameRunBind_CancelStatePrecedesLiveModel(t *testing.T) {
	e := newFrameBindPG(t)
	task, err := e.create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mustTakeSQL(t, e.db.Model(e.run).Update("state", model.FrameRunStateCancelled))
	// 同键未命中时仍须按正向任务 ID 取消，不能先被已下架模型或过期报价阻断。
	mustTakeSQL(t, e.db.Model(&model.GenerationTask{}).Where("id = ?", task.ID).Update("idempotency_key", "changed-key"))
	e.registry.snapErr = provider.ErrModelUnavailable
	_, err = e.create(context.Background())
	assertTaskCode(t, err, errcode.ErrDramaFrameRunConflict.Code)
	var stored model.GenerationTask
	mustTakeSQL(t, e.db.First(&stored, task.ID))
	if stored.Status != model.TaskCanceled || e.account(t).Credit.Frozen != 0 {
		t.Fatal("终态优先表被实时模型读取绕过")
	}
}
