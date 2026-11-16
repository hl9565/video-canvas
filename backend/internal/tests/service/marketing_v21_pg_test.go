package service_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/datatypes"

	"video-canvas/internal/llmgateway"
	"video-canvas/internal/marketing"
	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/ws"
	"video-canvas/internal/provider"
	"video-canvas/internal/provider/modelcfg"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
	"video-canvas/internal/service/stepfake"
	"video-canvas/internal/stepkit"
)

// v21Env 仅使用 PG16 真实 SQL 迁移（上游截至 0037 + Fork 9001），独立 schema 隔离每个用例。
func v21Env(t *testing.T) *mktEnv {
	t.Helper()
	db := slice2cPG16(t)
	migrateCurrentSchemaPG16(t, db)
	e := &mktEnv{t: t, db: db, repo: repository.NewMarketingRepository(db), taskRepo: repository.NewGenerationTaskRepository(db),
		models: stepfake.NewModels(), caller: newMktCaller(), assets: &mktAssets{items: map[uint64]*mktAsset{}}, canvases: &mktCanvases{},
		queue: &mktQueue{}, bc: &fakeTaskBroadcaster{}, user: 7, initial: 1000, reg: stepkit.NewRegistry(), resolver: stepkit.NewResolver(emptyBindings{}, stepfake.NewLibrary())}
	marketing.RegisterSteps(e.reg)
	e.repo.SetEnqueuer(e.queue)
	e.models.Snapshots["text-a"] = stepfake.TextModel("text-a", true, true, 3)
	e.models.Snapshots[mktVideoKey] = mktVideoSnapshot()
	e.taskSvc = service.NewGenerationTaskService(service.GenerationTaskDeps{Repo: e.taskRepo, Registry: e.models, Executor: &fakeTaskExecutor{}, Assets: e.assets, Broadcaster: e.bc, Limits: &fakeLimits{initial: 1000, defMax: 20}})
	e.rebuild(nil)
	for id, kind := range map[uint64]string{1: "image", 2: "image", 3: "image", 4: "video"} {
		mime := "image/jpeg"
		if kind == "video" {
			mime = "video/mp4"
		}
		e.assets.put(7, id, kind, mime, 10000)
		v21Must(t, db.Create(e.assets.items[id].asset).Error)
	}
	e.replyAll()
	return e
}

func v21Must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func v21Round(t *testing.T, e *mktEnv, id uint64) *model.MarketingRound {
	t.Helper()
	r, err := e.repo.Round(context.Background(), e.user, id, false)
	v21Must(t, err)
	return r
}
func v21Run(t *testing.T, e *mktEnv, id uint64) *model.MarketingStepRun {
	t.Helper()
	r, err := e.repo.Run(context.Background(), e.user, id, false)
	v21Must(t, err)
	return r
}
func v21Script(t *testing.T, e *mktEnv, id uint64) *model.MarketingScript {
	t.Helper()
	r, err := e.repo.Script(context.Background(), e.user, id, false)
	v21Must(t, err)
	return r
}

// v21Running 将已有冻结的queued运行模拟成持租约worker，不调用上游。
func v21Running(t *testing.T, e *mktEnv, id uint64, token string, lease *time.Time) *model.MarketingStepRun {
	t.Helper()
	v21Must(t, e.db.Model(&model.MarketingStepRun{}).Where("id = ?", id).Updates(map[string]any{"status": "running", "exec_token": token, "lease_until": lease, "started_at": time.Now()}).Error)
	return v21Run(t, e, id)
}

// v21Finish 真实用量按3积分结算；apply用于验证陈旧产出绝不落库。
func v21Finish(e *mktEnv, r *model.MarketingRound, run *model.MarketingStepRun, apply func(context.Context, repository.MarketingTx, *model.MarketingRound) error) error {
	return e.svc.FinishMarketingRunForTest(context.Background(), r, run, &stepkit.Result{Usage: llmgateway.Usage{InputTokens: 100, OutputTokens: 50}, Attempts: 1}, stepfake.TextModel("text-a", true, true, 3), apply)
}

func v21Accounting(t *testing.T, e *mktEnv, id uint64, settle, refund int) {
	t.Helper()
	var rows []model.CreditLedger
	v21Must(t, e.db.Where("marketing_step_run_id = ? AND type IN ?", id, []string{model.LedgerSettle, model.LedgerRefund}).Find(&rows).Error)
	got := map[string]int{}
	counts := map[string]int{}
	for _, r := range rows {
		got[r.Type] += r.Amount
		counts[r.Type]++
	}
	if got[model.LedgerSettle] != settle || got[model.LedgerRefund] != refund || counts[model.LedgerSettle] != 1 || (refund > 0 && counts[model.LedgerRefund] != 1) {
		t.Fatalf("结算/退款应为%d/%d且各1笔: %+v", settle, refund, rows)
	}
}

// v21GateRepo 在真实事务获取父锁后屏障阻塞，第二事务通过pg_blocking_pids证实已进入竞争。
type v21GateRepo struct {
	service.MarketingRepo
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}
type v21GateTx struct {
	repository.MarketingTx
	gate *v21GateRepo
}

func (g *v21GateRepo) WithTx(ctx context.Context, fn func(repository.MarketingTx) error) error {
	return g.MarketingRepo.WithTx(ctx, func(tx repository.MarketingTx) error { return fn(&v21GateTx{tx, g}) })
}
func (tx *v21GateTx) Project(ctx context.Context, u, id uint64, lock bool) (*model.MarketingProject, error) {
	p, err := tx.MarketingTx.Project(ctx, u, id, lock)
	if err == nil && lock {
		tx.gate.once.Do(func() {
			close(tx.gate.entered)
			select {
			case <-tx.gate.release:
			case <-ctx.Done():
			}
		})
	}
	return p, err
}

func v21Barrier(t *testing.T, e *mktEnv, first, second func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	g := &v21GateRepo{MarketingRepo: e.repo, entered: make(chan struct{}), release: make(chan struct{})}
	original := e.svc
	e.svc = service.NewMarketingService(service.MarketingDeps{Repo: g, Steps: e.reg, Resolver: e.resolver, Caller: e.caller, Models: e.models, Assets: e.assets, Tasks: e.taskSvc, Broadcaster: e.bc})
	defer func() { e.svc = original }()
	done := make(chan error, 2)
	go func() { done <- first(ctx) }()
	select {
	case <-g.entered:
	case <-ctx.Done():
		t.Fatal("首事务未优先取得项目锁")
	}
	go func() { done <- second(ctx) }()
	blocked := false
	for !blocked && ctx.Err() == nil {
		var n int64
		v21Must(t, e.db.Raw("SELECT count(*) FROM pg_stat_activity a WHERE cardinality(pg_blocking_pids(a.pid)) > 0 AND EXISTS (SELECT 1 FROM pg_locks l WHERE l.pid=a.pid AND l.relation='marketing_projects'::regclass)").Scan(&n).Error)
		blocked = n > 0
		if !blocked {
			time.Sleep(5 * time.Millisecond)
		}
	}
	close(g.release)
	if !blocked {
		t.Fatal("第二事务未在真实PG锁屏障等待")
	}
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				var b *errcode.Error
				if !errors.As(err, &b) {
					t.Fatal(err)
				}
			}
		case <-ctx.Done():
			t.Fatal("事务未能结束/死锁")
		}
	}
}

func TestMarketingV21_PG_G_LockOrderAndDeletedWrites(t *testing.T) {
	for _, restore := range []bool{false, true} {
		for _, reverseOrder := range []bool{false, true} {
			t.Run(fmt.Sprintf("restore=%v/reverse=%v", restore, reverseOrder), func(t *testing.T) {
				e := v21Env(t)
				_, r := e.refRound()
				follow := func(ctx context.Context) error { _, err := e.svc.Refollow(ctx, e.user, uint64(r.ID)); return err }
				edit := func(ctx context.Context) error {
					if restore {
						_, err := e.svc.RestoreReverse(ctx, e.user, uint64(r.ID))
						return err
					}
					_, err := e.svc.UpdateReverse(ctx, e.user, uint64(r.ID), &model.UpdateMarketingReverseReq{Reverse: *r.Reverse, ReverseRevision: r.ReverseRevision})
					return err
				}
				if reverseOrder {
					v21Barrier(t, e, edit, follow)
				} else {
					v21Barrier(t, e, follow, edit)
				}
			})
		}
	}
	e := v21Env(t)
	p, r := e.refRound()
	ctx := context.Background()
	v21Must(t, e.svc.DeleteProject(ctx, e.user, uint64(p.ID)))
	before := v21Script(t, e, uint64(r.Scripts[0].ID))
	beforeR := v21Round(t, e, uint64(r.ID))
	ops := []func() error{
		func() error {
			_, err := e.svc.UpdateReverse(ctx, e.user, uint64(r.ID), &model.UpdateMarketingReverseReq{})
			return err
		},
		func() error { _, err := e.svc.RestoreReverse(ctx, e.user, uint64(r.ID)); return err },
		func() error {
			_, err := e.svc.UpdateScript(ctx, e.user, before.ID, &model.UpdateMarketingScriptReq{Revision: before.Revision})
			return err
		},
		func() error { _, err := e.svc.RestoreScript(ctx, e.user, before.ID); return err },
		func() error { _, err := e.svc.CancelRound(ctx, e.user, uint64(r.ID)); return err },
		func() error { _, err := e.svc.Refollow(ctx, e.user, uint64(r.ID)); return err },
	}
	for _, op := range ops {
		wantMktCode(t, op(), errcode.ErrMarketingNotFound.Code)
	}
	after := v21Script(t, e, before.ID)
	afterR := v21Round(t, e, beforeR.ID)
	if mustJSONText(before) != mustJSONText(after) || mustJSONText(beforeR) != mustJSONText(afterR) {
		t.Fatal("删除后子表发生变化")
	}
}

func TestMarketingV21_PG_H_ReconcileAndHealVersusRefollow(t *testing.T) {
	for _, get := range []bool{false, true} {
		for _, reverseOrder := range []bool{false, true} {
			t.Run(fmt.Sprintf("get=%v/reverse=%v", get, reverseOrder), func(t *testing.T) {
				e := v21Env(t)
				p, r := e.refRound()
				sc := r.Scripts[0]
				cb := func(ctx context.Context) error {
					if get {
						_, err := e.svc.GetProject(ctx, e.user, uint64(p.ID))
						return err
					}
					return e.svc.OnTaskChanged(ctx, &model.GenerationTask{UserID: e.user, Kind: "video", NodeID: "mkt:" + idcodec.Encode(uint64(sc.ID))})
				}
				follow := func(ctx context.Context) error { _, err := e.svc.Refollow(ctx, e.user, uint64(r.ID)); return err }
				if reverseOrder {
					v21Barrier(t, e, cb, follow)
				} else {
					v21Barrier(t, e, follow, cb)
				}
				if v21Script(t, e, uint64(sc.ID)).Status != model.MarketingScriptRewriting {
					t.Fatal("回调覆盖了rewriting")
				}
			})
		}
	}
	e := v21Env(t)
	p, r := e.refRound()
	sc := v21Script(t, e, uint64(r.Scripts[0].ID))
	v21Must(t, e.svc.DeleteProject(context.Background(), e.user, uint64(p.ID)))
	v21Must(t, e.svc.OnTaskChanged(context.Background(), &model.GenerationTask{UserID: e.user, Kind: "video", NodeID: "mkt:" + idcodec.Encode(uint64(r.Scripts[0].ID))}))
	if mustJSONText(sc) != mustJSONText(v21Script(t, e, sc.ID)) {
		t.Fatal("删除后回调写入脚本")
	}
}

func TestMarketingV21_PG_I_RetryVersusRefollow(t *testing.T) {
	e := v21Env(t)
	p, r := e.refRound()
	ctx := context.Background()
	rid := uint64(r.ID)
	// 已成功运行沿用既有400契约；重写成功轮次仍正常。
	_, err := e.svc.RetryStep(ctx, e.user, uint64(stepOf(r, "follow").ID))
	wantMktCode(t, err, errcode.ErrInvalidParams.Code)
	r2, err := e.svc.NewRound(ctx, e.user, uint64(p.ID), &model.CreateMarketingRoundReq{Settings: mktSettings()})
	v21Must(t, err)
	_, err = e.svc.Refollow(ctx, e.user, rid)
	wantMktCode(t, err, 68007)
	failed := v21Run(t, e, uint64(stepOf(r, "follow").ID))
	v21Must(t, e.db.Model(failed).Update("status", "failed").Error)
	_, err = e.svc.RetryStep(ctx, e.user, failed.ID)
	wantMktCode(t, err, 68007)
	_, err = e.svc.CancelRound(ctx, e.user, uint64(r2.ID))
	v21Must(t, err)
	re, err := e.svc.Refollow(ctx, e.user, rid)
	v21Must(t, err)
	before := e.credit()
	queued := e.queue.count()
	_, err = e.svc.RetryStep(ctx, e.user, failed.ID)
	if !errors.Is(err, repository.ErrStateConflict) {
		t.Fatalf("旧失败ID应冲突: %v", err)
	}
	if e.credit() != before || e.queue.count() != queued {
		t.Fatal("陈旧重试产生冻结/入队")
	}
	nr := v21Round(t, e, rid)
	run := v21Run(t, e, uint64(stepOf(re, "follow").ID))
	if nr.ExecutionGen != 2 || run.RoundGen != 2 {
		t.Fatalf("执行代未同步: %+v %+v", nr, run)
	}
}

func TestMarketingV21_PG_E_SoftDeleteActualUsageAndTrustedKeys(t *testing.T) {
	e := v21Env(t)
	p := e.createProject(mktSettings())
	r := v21Round(t, e, uint64(p.Rounds[0].ID))
	ctx := context.Background()
	id := uint64(stepOf(&p.Rounds[0], "facts").ID)
	run := v21Run(t, e, id)
	delta := 10 - run.Frozen
	v21Must(t, e.db.Model(run).Update("frozen", 10).Error)
	v21Must(t, e.repo.AddCredit(ctx, e.user, 0, delta))
	lease := time.Now().Add(time.Minute)
	run = v21Running(t, e, id, "worker-e", &lease)
	_, err := e.svc.CancelRound(ctx, e.user, r.ID)
	v21Must(t, err)
	v21Must(t, e.svc.DeleteProject(ctx, e.user, uint64(p.ID)))
	var before model.MarketingProject
	v21Must(t, e.db.Unscoped().First(&before, uint64(p.ID)).Error)
	v21Must(t, e.repo.WithTx(ctx, func(tx repository.MarketingTx) error {
		_, err := tx.ProjectUnscopedLock(ctx, e.user+1, uint64(p.ID))
		if !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("错误user锁中: %v", err)
		}
		return nil
	}))
	var applied atomic.Int32
	for range 2 {
		v21Must(t, v21Finish(e, r, run, func(context.Context, repository.MarketingTx, *model.MarketingRound) error { applied.Add(1); return nil }))
	}
	v21Accounting(t, e, id, 3, 7)
	if e.credit().Frozen != 0 || v21Run(t, e, id).Status != "canceled" || applied.Load() != 0 {
		t.Fatal("陈旧收尾污染/未全结算")
	}
	var after model.MarketingProject
	v21Must(t, e.db.Unscoped().First(&after, uint64(p.ID)).Error)
	if mustJSONText(before) != mustJSONText(after) {
		t.Fatal("软删项目被改写")
	}
}

func TestMarketingV21_PG_D_CancelVersusFactsFinish(t *testing.T) {
	for _, cancelFirst := range []bool{true, false} {
		t.Run(fmt.Sprint(cancelFirst), func(t *testing.T) {
			e := v21Env(t)
			p := e.createProject(mktSettings())
			r := v21Round(t, e, uint64(p.Rounds[0].ID))
			lease := time.Now().Add(time.Minute)
			run := v21Running(t, e, uint64(stepOf(&p.Rounds[0], "facts").ID), "d", &lease)
			cancel := func(ctx context.Context) error { _, err := e.svc.CancelRound(ctx, e.user, r.ID); return err }
			finish := func(ctx context.Context) error {
				return e.svc.FinishMarketingRunForTest(ctx, r, run, &stepkit.Result{Usage: llmgateway.Usage{InputTokens: 100}}, stepfake.TextModel("text-a", true, true, 3), func(ctx context.Context, tx repository.MarketingTx, _ *model.MarketingRound) error {
					return tx.UpdateProject(ctx, r.ProjectID, map[string]any{"title": "已完成"})
				})
			}
			// finish同样需取得可信双键父锁，通过独立gate适配检测。
			if cancelFirst {
				v21Barrier(t, e, cancel, finish)
			} else {
				v21BackgroundBarrier(t, e, finish, cancel)
			}
			v21Accounting(t, e, run.ID, 3, run.Frozen-3)
			if e.credit().Frozen != 0 {
				t.Fatal("取消/收尾后有冻结残留")
			}
			if cancelFirst && v21Run(t, e, run.ID).Status != "canceled" {
				t.Fatal("取消先提交后成功产出未隔离")
			}
		})
	}
}

func TestMarketingV21_PG_C_CreateVersusCancel(t *testing.T) {
	for _, cancelFirst := range []bool{true, false} {
		t.Run(fmt.Sprint(cancelFirst), func(t *testing.T) {
			e := v21Env(t)
			_, r := e.refRound()
			_, err := e.svc.Refollow(context.Background(), e.user, uint64(r.ID))
			v21Must(t, err)
			cancel := func(ctx context.Context) error { _, err := e.svc.CancelRound(ctx, e.user, uint64(r.ID)); return err }
			create := func(ctx context.Context) error {
				resp, err := e.taskSvc.Create(ctx, e.user, "concurrent-render", &model.CreateGenerationTaskReq{Kind: "video", ModelID: mktVideoKey, NodeID: "mkt:" + idcodec.Encode(uint64(r.Scripts[0].ID)), Input: map[string]any{"prompt": "x", "op": "t2v"}})
				if err == nil && resp.Items[0].Error != nil {
					return errcode.New(resp.Items[0].Error.Code, resp.Items[0].Error.Message, resp.Items[0].Error.Status)
				}
				return err
			}
			// 生成任务批锁也获取相同父锁；直接持真实父锁作为统一起跑屏障。
			v21ConcurrentAtProject(t, e, uint64(r.ID), cancelFirst, cancel, create)
			var n int64
			v21Must(t, e.db.Model(&model.GenerationTask{}).Where("node_id = ?", "mkt:"+idcodec.Encode(uint64(r.Scripts[0].ID))).Count(&n).Error)
			// 本轮仍在重写或已取消，两种锁顺序都不能创建媒体任务。
			want := int64(0)
			if n != want {
				t.Fatalf("创建/取消屏障后任务数=%d want%d", n, want)
			}
		})
	}
}

func (tx *v21GateTx) ProjectUnscopedLock(ctx context.Context, u, id uint64) (*model.MarketingProject, error) {
	p, err := tx.MarketingTx.ProjectUnscopedLock(ctx, u, id)
	if err == nil {
		tx.gate.once.Do(func() {
			close(tx.gate.entered)
			select {
			case <-tx.gate.release:
			case <-ctx.Done():
			}
		})
	}
	return p, err
}
func v21BackgroundBarrier(t *testing.T, e *mktEnv, a, b func(context.Context) error) {
	t.Helper()
	v21Barrier(t, e, a, b)
}

func v21ConcurrentAtProject(t *testing.T, e *mktEnv, rid uint64, firstCancel bool, a, b func(context.Context) error) {
	t.Helper()
	if !firstCancel {
		a, b = b, a
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan error, 2)
	err := e.repo.WithTx(ctx, func(tx repository.MarketingTx) error {
		_, err := tx.Project(ctx, e.user, v21Round(t, e, rid).ProjectID, true)
		if err != nil {
			return err
		}
		go func() { done <- a(ctx) }()
		v21WaitBlocked(ctx, t, e, 1)
		go func() { done <- b(ctx) }()
		v21WaitBlocked(ctx, t, e, 2)
		return nil
	})
	v21Must(t, err)
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				var biz *errcode.Error
				if !errors.As(err, &biz) && err.Error() != "脚本正在出片或重写，请稍后再试" {
					t.Fatal(err)
				}
			}
		case <-ctx.Done():
			t.Fatal("创建/取消死锁")
		}
	}
}
func v21WaitBlocked(ctx context.Context, t *testing.T, e *mktEnv, want int64) {
	t.Helper()
	for ctx.Err() == nil {
		var n int64
		v21Must(t, e.db.Raw("SELECT count(*) FROM pg_stat_activity a WHERE cardinality(pg_blocking_pids(a.pid)) > 0 AND EXISTS (SELECT 1 FROM pg_locks l WHERE l.pid=a.pid AND l.relation='marketing_projects'::regclass)").Scan(&n).Error)
		if n >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("PG锁等待未出现")
}

func TestMarketingV21_PG_J_CancelABA_AllSteps(t *testing.T) {
	for _, key := range []string{marketing.StepFacts, marketing.StepReverse, marketing.StepFollow, marketing.StepCreate} {
		t.Run(key, func(t *testing.T) {
			e := v21Env(t)
			p, view := e.refRound()
			r := v21Round(t, e, uint64(view.ID))
			ctx := context.Background()
			run := &model.MarketingStepRun{UserID: e.user, RoundID: r.ID, RoundGen: r.ExecutionGen, StepKey: key, Status: "running", Frozen: 10, ExecToken: "old-j", Method: []byte(`{}`)}
			lease := time.Now().Add(time.Minute)
			run.LeaseUntil = &lease
			v21Must(t, e.db.Create(run).Error)
			v21Must(t, e.repo.AddCredit(ctx, e.user, 0, 10))
			v21Must(t, e.db.Model(r).Update("status", "scripting").Error)
			_, err := e.svc.CancelRound(ctx, e.user, r.ID)
			v21Must(t, err)
			old := v21Run(t, e, run.ID)
			if !old.CancelRequested {
				t.Fatal("取消未持久化标志")
			}
			_, err = e.svc.Refollow(ctx, e.user, r.ID)
			v21Must(t, err)
			before := v21Script(t, e, uint64(view.Scripts[0].ID))
			beforeR := v21Round(t, e, r.ID)
			var applied atomic.Int32
			for range 2 {
				v21Must(t, v21Finish(e, r, run, func(context.Context, repository.MarketingTx, *model.MarketingRound) error { applied.Add(1); return nil }))
			}
			if applied.Load() != 0 || v21Run(t, e, run.ID).Status != "canceled" {
				t.Fatal("ABA旧产出未隔离")
			}
			if mustJSONText(before) != mustJSONText(v21Script(t, e, before.ID)) || mustJSONText(beforeR.Warnings) != mustJSONText(v21Round(t, e, r.ID).Warnings) || v21Round(t, e, r.ID).Status != "scripting" {
				t.Fatal("旧结算覆盖了新代脚本/轮次")
			}
			v21Accounting(t, e, run.ID, 3, 7)
			_ = p
		})
	}
}

// v21BlockingCaller 模拟取消后仍返回真实已用token的上游，支持检查存活租约不抢收。
type v21BlockingCaller struct {
	calls    atomic.Int32
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
	reply    string
}

func (c *v21BlockingCaller) Complete(ctx context.Context, _ *provider.Snapshot, _ []llmgateway.Message) (*llmgateway.Result, error) {
	if c.calls.Add(1) == 1 {
		close(c.started)
	}
	select {
	case <-ctx.Done():
		close(c.canceled)
		<-c.release
	case <-c.release:
	}
	return &llmgateway.Result{Text: c.reply, Usage: llmgateway.Usage{InputTokens: 100, OutputTokens: 50}}, nil
}
func v21CallerService(e *mktEnv, c stepkit.Caller) *service.MarketingService {
	return service.NewMarketingService(service.MarketingDeps{Repo: e.repo, Steps: e.reg, Resolver: e.resolver, Caller: c, Models: e.models, Assets: e.assets, Broadcaster: e.bc})
}
func v21Wait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(6 * time.Second):
		t.Fatal("等待worker屏障超时")
	}
}

func TestMarketingV21_PG_K_ClaimAndReentryCallsLLMOnce(t *testing.T) {
	e := v21Env(t)
	p := e.createProject(mktSettings())
	r := v21Round(t, e, uint64(p.Rounds[0].ID))
	run := v21Run(t, e, uint64(stepOf(&p.Rounds[0], "facts").ID))
	c := &v21BlockingCaller{started: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{}), reply: factsReply()}
	e.svc = v21CallerService(e, c)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			copyRun := *run
			e.svc.ExecMarketingStepForTest(context.Background(), r, &copyRun)
		}()
	}
	v21Wait(t, c.started)
	current := v21Run(t, e, run.ID)
	if current.ExecToken == "" || current.LeaseUntil == nil {
		t.Fatal("认领未写租约")
	}
	e.svc.ExecMarketingStepForTest(context.Background(), r, current)
	close(c.release)
	wg.Wait()
	if c.calls.Load() != 1 {
		t.Fatalf("调用LLM次数=%d", c.calls.Load())
	}
}

func TestMarketingV21_PG_J_WatchCancel_AllSteps(t *testing.T) {
	keys := []string{marketing.StepFacts, marketing.StepReverse, marketing.StepFollow, marketing.StepCreate}
	for _, key := range keys {
		for _, reason := range []string{"flag", "gen", "round", "deleted"} {
			t.Run(key+"/"+reason, func(t *testing.T) {
				e := v21Env(t)
				p := e.createRefProject()
				r := v21Round(t, e, uint64(p.Rounds[0].ID))
				ctx := context.Background()
				v21Must(t, e.db.Model(&model.MarketingProject{}).Where("id = ?", r.ProjectID).Update("product", datatypes.JSON(factsReply())).Error)
				v21Must(t, e.db.Model(r).Update("reverse", datatypes.JSON(reverseReply())).Error)
				r = v21Round(t, e, r.ID)
				var run *model.MarketingStepRun
				for _, v := range p.Rounds[0].Steps {
					if "marketing."+v.Step == key {
						run = v21Run(t, e, uint64(v.ID))
					}
				}
				reply := map[string]string{marketing.StepFacts: factsReply(), marketing.StepReverse: reverseReply(), marketing.StepFollow: followReply(10, "一抹就润"), marketing.StepCreate: createReply(10, "pain")}[key]
				c := &v21BlockingCaller{started: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{}), reply: reply}
				e.svc = v21CallerService(e, c)
				done := make(chan struct{})
				go func() { defer close(done); e.svc.ExecMarketingStepForTest(ctx, r, run) }()
				v21Wait(t, c.started)
				switch reason {
				case "flag":
					v21Must(t, e.db.Model(run).Update("cancel_requested", true).Error)
				case "gen":
					v21Must(t, e.db.Model(r).Update("execution_gen", r.ExecutionGen+1).Error)
				case "round":
					v21Must(t, e.db.Model(r).Update("status", "canceled").Error)
				case "deleted":
					v21Must(t, e.db.Delete(&model.MarketingProject{}, r.ProjectID).Error)
				}
				v21Wait(t, c.canceled)
				close(c.release)
				v21Wait(t, done)
				if v21Run(t, e, run.ID).Status != "canceled" {
					t.Fatal("watchCancel未隔离旧结局")
				}
			})
		}
	}
}

func TestMarketingV21_PG_L_ActiveLeaseNeverReclaimed(t *testing.T) {
	e := v21Env(t)
	p := e.createProject(mktSettings())
	r := v21Round(t, e, uint64(p.Rounds[0].ID))
	run := v21Run(t, e, uint64(stepOf(&p.Rounds[0], "facts").ID))
	ctx := context.Background()
	c := &v21BlockingCaller{started: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{}), reply: factsReply()}
	e.svc = v21CallerService(e, c)
	done := make(chan struct{})
	go func() { defer close(done); e.svc.ExecMarketingStepForTest(ctx, r, run) }()
	v21Wait(t, c.started)
	_, err := e.svc.CancelRound(ctx, e.user, r.ID)
	v21Must(t, err)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.svc.RecoverUnsettledMarketingRuns(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if v21Run(t, e, run.ID).Status != "running" || e.credit().Frozen != run.Frozen {
		t.Fatal("活跃worker被抢退")
	}
	v21Wait(t, c.canceled)
	close(c.release)
	v21Wait(t, done)
	v21Accounting(t, e, run.ID, 3, run.Frozen-3)
	if e.credit().Frozen != 0 {
		t.Fatal("结算后仍有冻结")
	}
}

func TestMarketingV21_PG_M_CrashRecovery(t *testing.T) {
	for _, mode := range []string{"active_empty", "active_mixed", "deleted", "canceled", "old_generation"} {
		t.Run(mode, func(t *testing.T) {
			e := v21Env(t)
			p, r := e.refRound()
			ctx := context.Background()
			sid := uint64(r.Scripts[0].ID)
			if mode == "active_mixed" {
				for i, st := range []string{model.TaskSucceeded, model.TaskFailed} {
					task := &model.GenerationTask{ID: uint64(100 + i), UserID: e.user, Kind: "video", NodeID: "mkt:" + idcodec.Encode(sid), Status: st, ErrorMessage: "历史视频失败", CreatedAt: time.Now().Add(time.Duration(i) * time.Second)}
					seedComposePGTask(task)
					task.IdempotencyKey = fmt.Sprint(i)
					task.TaskRef = fmt.Sprint(i)
					v21Must(t, e.db.Create(task).Error)
				}
			}
			re, err := e.svc.Refollow(ctx, e.user, uint64(r.ID))
			v21Must(t, err)
			expired := time.Now().Add(-time.Minute)
			run := v21Running(t, e, uint64(stepOf(re, "follow").ID), "dead-m", &expired)
			if mode == "deleted" || mode == "canceled" {
				_, err = e.svc.CancelRound(ctx, e.user, uint64(r.ID))
				v21Must(t, err)
				if mode == "deleted" {
					v21Must(t, e.svc.DeleteProject(ctx, e.user, uint64(p.ID)))
				}
			}
			if mode == "old_generation" {
				v21Must(t, e.db.Model(&model.MarketingRound{}).Where("id = ?", uint64(r.ID)).Update("execution_gen", run.RoundGen+1).Error)
			}
			before := v21Script(t, e, sid)
			var wg sync.WaitGroup
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := e.svc.RecoverUnsettledMarketingRuns(ctx); err != nil {
						t.Error(err)
					}
				}()
			}
			wg.Wait()
			v21Must(t, e.svc.HandleMarketingRound(ctx, uint64(r.ID)))
			after := v21Script(t, e, sid)
			rr := v21Round(t, e, uint64(r.ID))
			if string(before.Shots) != string(after.Shots) {
				t.Fatal("恢复破坏上一版shots")
			}
			if mode == "active_empty" || mode == "active_mixed" {
				want, errMsg := model.MarketingScriptDraft, ""
				if mode == "active_mixed" {
					want, errMsg = model.MarketingScriptFailed, "历史视频失败"
				}
				if after.Status != want || after.Error != errMsg || rr.Status != model.MarketingRoundReady {
					t.Fatalf("未释放重写/写物理error/收口: %+v %+v", after, rr)
				}
				_, err = e.svc.RestoreScript(ctx, e.user, sid)
				v21Must(t, err)
				_, err = e.svc.Refollow(ctx, e.user, rr.ID)
				v21Must(t, err)
			} else if mustJSONText(before) != mustJSONText(after) {
				t.Fatal("陈旧恢复触碰脚本")
			}
			v21Accounting(t, e, run.ID, 0, run.Frozen)
			if e.caller.count(marketing.StepFollow) != 1 {
				t.Fatal("恢复重调了LLM")
			}
		})
	}
}

func TestMarketingV21_PG_N_Legacy0035DrainGate(t *testing.T) {
	db := slice2cPG16(t)
	slice2cMigrate(t, db, slice2cUpstream35Only)
	for _, sql := range []string{
		`INSERT INTO marketing_projects(id,user_id,title) VALUES(1,7,'旧项目')`,
		`INSERT INTO marketing_rounds(id,user_id,project_id,no,settings) VALUES(1,7,1,1,'{}')`,
		`INSERT INTO marketing_step_runs(id,user_id,round_id,step_key,status,frozen) VALUES(1,7,1,'marketing.facts','running',10),(2,7,1,'marketing.reverse','running',10)`,
		`INSERT INTO user_credits(user_id,balance,frozen) VALUES(7,100,20)`,
		`INSERT INTO credit_ledger(user_id,marketing_step_run_id,type,amount) VALUES(7,1,'freeze',10),(7,2,'freeze',10)`,
	} {
		v21Must(t, db.Exec(sql).Error)
	}
	migrateCurrentSchemaPG16(t, db)
	e := &mktEnv{t: t, db: db, repo: repository.NewMarketingRepository(db), user: 7}
	e.svc = service.NewMarketingService(service.MarketingDeps{Repo: e.repo, Models: stepfake.NewModels()})
	a, b := v21Run(t, e, 1), v21Run(t, e, 2)
	for _, run := range []*model.MarketingStepRun{a, b} {
		if run.ExecToken != "" || run.LeaseUntil != nil || run.RoundGen != 1 {
			t.Fatalf("迁后不是旧NULL租约: %+v", run)
		}
	}
	ctx := context.Background()
	v21Must(t, e.svc.RecoverUnsettledMarketingRuns(ctx))
	if v21Run(t, e, 1).Status != "running" || v21Run(t, e, 2).Status != "running" || e.credit().Frozen != 20 {
		t.Fatal("排空前误退款")
	}
	r := v21Round(t, e, 1)
	for range 2 {
		v21Must(t, v21Finish(e, r, a, nil))
	}
	v21Accounting(t, e, 1, 3, 7)
	e.svc = service.NewMarketingService(service.MarketingDeps{Repo: e.repo, Models: stepfake.NewModels()}, service.WithMarketingLegacyNullLeaseDrainCompleted(true))
	for range 2 {
		v21Must(t, e.svc.RecoverUnsettledMarketingRuns(ctx))
	}
	v21Accounting(t, e, 2, 0, 10)
	if e.credit().Frozen != 0 || v21Run(t, e, 2).Status != "canceled" {
		t.Fatal("排空后未回收B")
	}
	v21Accounting(t, e, 1, 3, 7)
}

func TestMarketingV21_PG_K_HeartbeatAndReclaimFencing(t *testing.T) {
	e := v21Env(t)
	p := e.createProject(mktSettings())
	r := v21Round(t, e, uint64(p.Rounds[0].ID))
	ctx := context.Background()
	id := uint64(stepOf(&p.Rounds[0], "facts").ID)
	active := time.Now().Add(time.Minute)
	worker := v21Running(t, e, id, "lease-owner", &active)
	ok, err := e.repo.HeartbeatStepRun(ctx, id, "foreign", time.Now().Add(time.Minute))
	v21Must(t, err)
	if ok {
		t.Fatal("错误令牌续约成功")
	}
	ok, err = e.repo.HeartbeatStepRun(ctx, id, "lease-owner", time.Now().Add(2*time.Minute))
	v21Must(t, err)
	if !ok {
		t.Fatal("持有者无法续约")
	}
	ok, err = e.repo.ReclaimExpiredStepRun(ctx, id, "lease-owner", time.Now(), false)
	v21Must(t, err)
	if ok {
		t.Fatal("续约后仍被回收")
	}
	expired := time.Now().Add(-time.Minute)
	v21Must(t, e.db.Model(&model.MarketingStepRun{}).Where("id = ?", id).Update("lease_until", expired).Error)
	// 租约过期后、尚未被 reclaim 清理前，原持有者迟到心跳不能借旧令牌复活租约。
	resurrectOK, err := e.repo.HeartbeatStepRun(ctx, id, "lease-owner", time.Now().Add(5*time.Minute))
	v21Must(t, err)
	if resurrectOK {
		t.Fatal("租约已过期的迟到心跳不应复活 lease_until")
	}
	stillExpired := v21Run(t, e, id)
	if stillExpired.LeaseUntil == nil || !stillExpired.LeaseUntil.Before(time.Now()) {
		t.Fatalf("迟到心跳改写了已过期租约: %+v", stillExpired.LeaseUntil)
	}
	ok, err = e.repo.ReclaimExpiredStepRun(ctx, id, "foreign", time.Now(), false)
	v21Must(t, err)
	if ok {
		t.Fatal("错误令牌取得回收权")
	}
	v21Must(t, e.svc.RecoverUnsettledMarketingRuns(ctx))
	closed := v21Run(t, e, id)
	if closed.ExecToken != "" || closed.LeaseUntil != nil || closed.Status != "canceled" {
		t.Fatalf("回收未清执行权:%+v", closed)
	}
	v21Must(t, v21Finish(e, r, worker, func(context.Context, repository.MarketingTx, *model.MarketingRound) error {
		t.Error("已回收worker仍应用结果")
		return nil
	}))
	v21Accounting(t, e, id, 0, worker.Frozen)
}

// v21FailFinishRepo 在真实事务关闭运行时注入两次瞬时错误，验证不重调上游且沿用同一执行令牌。
type v21FailFinishRepo struct {
	service.MarketingRepo
	failures atomic.Int32
	mu       sync.Mutex
	tokens   []string
}
type v21FailFinishTx struct {
	repository.MarketingTx
	parent *v21FailFinishRepo
}

func (r *v21FailFinishRepo) WithTx(ctx context.Context, fn func(repository.MarketingTx) error) error {
	return r.MarketingRepo.WithTx(ctx, func(tx repository.MarketingTx) error { return fn(&v21FailFinishTx{tx, r}) })
}
func (tx *v21FailFinishTx) UpdateRunIf(ctx context.Context, id uint64, from []string, fields map[string]any) error {
	run, err := tx.Run(ctx, 7, id, false)
	if err != nil {
		return err
	}
	tx.parent.mu.Lock()
	tx.parent.tokens = append(tx.parent.tokens, run.ExecToken)
	tx.parent.mu.Unlock()
	if tx.parent.failures.Add(1) <= 2 {
		return errors.New("注入瞬时DB错误")
	}
	return tx.MarketingTx.UpdateRunIf(ctx, id, from, fields)
}

func TestMarketingV21_PG_K_FinishRetryKeepsUsageAndToken(t *testing.T) {
	e := v21Env(t)
	p := e.createProject(mktSettings())
	r := v21Round(t, e, uint64(p.Rounds[0].ID))
	run := v21Run(t, e, uint64(stepOf(&p.Rounds[0], "facts").ID))
	repo := &v21FailFinishRepo{MarketingRepo: e.repo}
	e.svc = service.NewMarketingService(service.MarketingDeps{Repo: repo, Steps: e.reg, Resolver: e.resolver, Caller: e.caller, Models: e.models, Assets: e.assets, Broadcaster: e.bc})
	e.svc.ExecMarketingStepForTest(context.Background(), r, run)
	if e.caller.count(marketing.StepFacts) != 1 || v21Run(t, e, run.ID).Status != "succeeded" {
		t.Fatal("提交重试重调LLM或丢失真实结局")
	}
	if len(repo.tokens) != 3 || repo.tokens[0] == "" || repo.tokens[0] != repo.tokens[1] || repo.tokens[1] != repo.tokens[2] {
		t.Fatalf("重试未沿用执行令牌:%v", repo.tokens)
	}
	v21Accounting(t, e, run.ID, 3, run.Frozen-3)
}

func TestMarketingV21_PG_HeartbeatDoesNotShrinkLease(t *testing.T) {
	e := v21Env(t)
	p := e.createProject(mktSettings())
	ctx := context.Background()
	id := uint64(stepOf(&p.Rounds[0], "facts").ID)
	base := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
	later := base.Add(2 * time.Minute)
	earlier := base.Add(10 * time.Second)
	_ = v21Running(t, e, id, "tok-mono", &base)

	ok, err := e.repo.HeartbeatStepRun(ctx, id, "tok-mono", later)
	v21Must(t, err)
	if !ok {
		t.Fatal("新租约续约失败")
	}
	ok, err = e.repo.HeartbeatStepRun(ctx, id, "tok-mono", earlier)
	v21Must(t, err)
	if !ok {
		t.Fatal("匹配令牌乱序心跳不应出错或失去更新")
	}
	got := v21Run(t, e, id)
	if got.LeaseUntil == nil || got.LeaseUntil.Before(later.Add(-time.Millisecond)) {
		t.Fatalf("旧到期时间后写不得缩短有效租约：got=%v later=%v", got.LeaseUntil, later)
	}
}

func TestMarketingV21_PG_DrainLegacyNullLeaseOrphansRequiresOptIn(t *testing.T) {
	e := v21Env(t)
	p := e.createProject(mktSettings())
	ctx := context.Background()
	id := uint64(stepOf(&p.Rounds[0], "facts").ID)
	orphan := v21Running(t, e, id, "", nil)

	// 默认未开启 legacyDrainCompleted：拒绝执行且不触动旧 NULL 租约。
	if _, err := e.svc.DrainLegacyNullLeaseOrphans(ctx); err == nil {
		t.Fatal("未满足停领排空前置确认时必须拒绝回收")
	}
	if still := v21Run(t, e, id); still.Status != model.MarketingRunRunning {
		t.Fatalf("拒绝执行时不应变更旧孤儿状态：%+v", still)
	}

	// 开启后仅回收旧 NULL 租约孤儿并解冻积分。
	drainSvc := service.NewMarketingService(
		service.MarketingDeps{Repo: e.repo},
		service.WithMarketingLegacyNullLeaseDrainCompleted(true),
	)
	rounds, err := drainSvc.DrainLegacyNullLeaseOrphans(ctx)
	v21Must(t, err)
	if rounds != 1 {
		t.Fatalf("应回收 1 轮旧 NULL 租约孤儿，实际=%d", rounds)
	}
	if closed := v21Run(t, e, id); closed.Status != model.MarketingRunCanceled {
		t.Fatalf("开启后应回收孤儿为 canceled：%+v", closed)
	}
	v21Accounting(t, e, id, 0, orphan.Frozen)
}

func TestMarketingV21_PG_HeartbeatDoesNotResurrectAcrossLockWait(t *testing.T) {
	e := v21Env(t)
	p := e.createProject(mktSettings())
	ctx := context.Background()
	id := uint64(stepOf(&p.Rounds[0], "facts").ID)
	const token = "hb-lock-wait-tok"

	v21Must(t, e.db.Exec(
		"UPDATE marketing_step_runs SET status = ?, exec_token = ?, started_at = clock_timestamp(), lease_until = clock_timestamp() + interval '250 milliseconds' WHERE id = ?",
		"running", token, id,
	).Error)
	var origLease time.Time
	v21Must(t, e.db.Raw("SELECT lease_until FROM marketing_step_runs WHERE id = ?", id).Scan(&origLease).Error)

	// TX1 预先持有行锁；TX2 在租约尚未过期时开启事务（冻结 NOW() < origLease）。
	tx1 := e.db.Begin()
	v21Must(t, tx1.Error)
	tx2 := e.db.Begin()
	v21Must(t, tx2.Error)
	var tx1Closed, tx2Closed bool
	defer func() {
		if !tx1Closed {
			_ = tx1.Rollback().Error
		}
		if !tx2Closed {
			_ = tx2.Rollback().Error
		}
	}()
	var lockedID uint64
	v21Must(t, tx1.Raw("SELECT id FROM marketing_step_runs WHERE id = ? FOR UPDATE", id).Scan(&lockedID).Error)
	var tx2Now time.Time
	v21Must(t, tx2.Raw("SELECT now()").Scan(&tx2Now).Error)
	if !tx2Now.Before(origLease) {
		t.Fatalf("TX2 事务开始时间应早于租约到期时间: tx2Now=%v origLease=%v", tx2Now, origLease)
	}

	type hbRes struct {
		ok  bool
		err error
	}
	resCh := make(chan hbRes, 1)
	nextLease := origLease.Add(time.Minute)
	hbCtx, hbCancel := context.WithTimeout(ctx, 5*time.Second)
	defer hbCancel()
	go func() {
		ok, err := repository.NewMarketingRepository(tx2).HeartbeatStepRun(hbCtx, id, token, nextLease)
		resCh <- hbRes{ok: ok, err: err}
	}()

	// 通过 pg_blocking_pids 确认 TX2 确实阻塞在 marketing_step_runs 行锁上。
	blocked := false
	for range 200 {
		var n int
		v21Must(t, e.db.Raw("SELECT count(*) FROM pg_stat_activity a WHERE cardinality(pg_blocking_pids(a.pid)) > 0 AND EXISTS (SELECT 1 FROM pg_locks l WHERE l.pid=a.pid AND l.relation='marketing_step_runs'::regclass)").Scan(&n).Error)
		if n > 0 {
			blocked = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("TX2 未在真实 PG marketing_step_runs 行锁上阻塞")
	}

	// 等到 PostgreSQL clock_timestamp() 越过 origLease 后再释放 TX1。
	expired := false
	for range 200 {
		v21Must(t, e.db.Raw("SELECT clock_timestamp() > lease_until FROM marketing_step_runs WHERE id = ?", id).Scan(&expired).Error)
		if expired {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !expired {
		t.Fatal("等待租约过期超时")
	}

	v21Must(t, tx1.Commit().Error)
	tx1Closed = true

	var res hbRes
	select {
	case res = <-resCh:
	case <-time.After(3 * time.Second):
		t.Fatal("释放行锁后 HeartbeatStepRun 未返回")
	}
	v21Must(t, tx2.Commit().Error)
	tx2Closed = true

	if res.err != nil || res.ok {
		t.Fatalf("等锁跨过过期时间的心跳不应复活过期租约: ok=%v err=%v", res.ok, res.err)
	}
	var afterLease time.Time
	v21Must(t, e.db.Raw("SELECT lease_until FROM marketing_step_runs WHERE id = ?", id).Scan(&afterLease).Error)
	if !afterLease.Equal(origLease) {
		t.Fatalf("lease_until 不应被修改: orig=%v after=%v", origLease, afterLease)
	}
}

// m2SQLQueue 是仅用于独立测试schema的事务队列替身，写完再报错也必须随任务保存点回滚。
type m2SQLQueue struct{ fail error }

func (q *m2SQLQueue) EnqueueTx(ctx context.Context, tx *sql.Tx, id uint64, _ string, _ time.Time) error {
	if _, err := tx.ExecContext(ctx, "INSERT INTO m2_test_queue(task_id) VALUES ($1)", id); err != nil {
		return err
	}
	return q.fail
}
func (*m2SQLQueue) EnqueueAgentNotifyTx(context.Context, *sql.Tx, uint64, uint64) error {
	return errors.New("营销出片不应创建Agent通知")
}
func m2PGH3(t *testing.T) (*mktEnv, []model.MarketingScriptView, *m2SQLQueue) {
	t.Helper()
	e := v21Env(t)
	e.useH3()
	_, scripts := e.h3Scripts()
	q := &m2SQLQueue{}
	v21Must(t, e.db.Exec("CREATE TABLE m2_test_queue (task_id bigint PRIMARY KEY)").Error)
	e.taskRepo.SetEnqueuer(q)
	return e, scripts, q
}
func m2ServiceInstance(e *mktEnv, caller stepkit.Caller) *service.MarketingService {
	tasks := service.NewGenerationTaskService(service.GenerationTaskDeps{Repo: e.taskRepo, Registry: e.models, Assets: e.assets, Broadcaster: e.bc, Limits: &fakeLimits{initial: e.initial, defMax: 20}})
	svc := service.NewMarketingService(service.MarketingDeps{Repo: e.repo, Models: e.models, Caller: caller, Tasks: tasks, Assets: e.assets, Credits: stepfake.Policy{Initial: e.initial}, Broadcaster: e.bc})
	tasks.SetTaskObserver(svc)
	return svc
}
func m2Wait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(8 * time.Second):
		t.Fatal("模型屏障超时")
	}
}
func m2Result(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(8 * time.Second):
		t.Fatal("请求未收尾，可能死锁")
		return nil
	}
}
func m2GatedCaller(entered, release chan struct{}, marker string) *mktCaller {
	c := newMktCaller()
	c.byText[mktAdaptStep] = func(prompt string) (string, error) {
		close(entered)
		select {
		case <-release:
		case <-time.After(8 * time.Second):
			return "", errors.New("模型屏障超时")
		}
		return strings.ReplaceAll(h3Reply(prompt), "Quiet room tone.", marker), nil
	}
	return c
}
func m2TaskBroadcasts(e *mktEnv) int {
	e.bc.mu.Lock()
	defer e.bc.mu.Unlock()
	n := 0
	for _, msg := range e.bc.msgs {
		if msg.Type == ws.TypeTaskUpdated {
			n++
		}
	}
	return n
}
func m2SetAvailable(t *testing.T, e *mktEnv, n int) {
	t.Helper()
	c := e.credit()
	delta := c.Frozen + n - c.Balance
	v21Must(t, e.repo.AddCredit(context.Background(), e.user, delta, 0))
	_, err := e.repo.InsertLedger(context.Background(), &model.CreditLedger{UserID: e.user, Type: model.LedgerAdminAdjust, Amount: delta})
	v21Must(t, err)
}
func m2CheckPGAccounting(t *testing.T, e *mktEnv, balance, frozen, tasks int) {
	t.Helper()
	c, err := e.taskRepo.Reconcile(context.Background(), e.user)
	v21Must(t, err)
	if c.Balance != balance || c.Frozen != frozen || c.BalanceDiff() != 0 || c.FrozenDiff() != 0 || c.ActiveDiff() != 0 {
		t.Fatalf("账户/流水/任务不守恒: %+v want %d/%d", c, balance, frozen)
	}
	var count, queued int64
	v21Must(t, e.db.Model(&model.GenerationTask{}).Count(&count).Error)
	v21Must(t, e.db.Table("m2_test_queue").Count(&queued).Error)
	if count != int64(tasks) || queued != count || m2TaskBroadcasts(e) != tasks {
		t.Fatalf("任务/队列/提交后通知: %d/%d/%d want %d", count, queued, m2TaskBroadcasts(e), tasks)
	}
}
func m2TaskPrompt(t *testing.T, task model.GenerationTask) string {
	t.Helper()
	var input map[string]any
	v21Must(t, json.Unmarshal(task.InputJSON, &input))
	prompt, _ := input["prompt"].(string)
	return prompt
}

func TestMarketingV21_PG_M2_RenderReplayRejectsDifferentIntentBeforeLLM(t *testing.T) {
	e, scripts, _ := m2PGH3(t)
	ctx := context.Background()
	id := uint64(scripts[0].ID)
	before := e.credit().Balance
	got, err := e.svc.Render(ctx, e.user, id, &model.RenderMarketingScriptReq{IdempotencyKey: "render"})
	v21Must(t, err)
	if got.RenderCredits != 20 {
		t.Fatalf("返回视图不得继续报价已提交的转写: %d", got.RenderCredits)
	}
	if _, err := e.svc.Render(ctx, e.user, id, &model.RenderMarketingScriptReq{IdempotencyKey: "render"}); err != nil {
		t.Fatal(err)
	}
	for _, req := range []model.RenderMarketingScriptReq{
		{IdempotencyKey: "render", Params: map[string]any{}},
		{IdempotencyKey: "render", VideoModel: mktH3Key},
		{IdempotencyKey: "render", Params: map[string]any{"duration": 10}},
	} {
		_, err := e.svc.Render(ctx, e.user, id, &req)
		wantMktCode(t, err, errcode.ErrIdempotencyConflict.Code)
	}
	_, err = e.svc.Render(ctx, e.user, uint64(scripts[1].ID), &model.RenderMarketingScriptReq{IdempotencyKey: "render"})
	wantMktCode(t, err, errcode.ErrIdempotencyConflict.Code)
	if e.caller.count(mktAdaptStep) != 1 || len(e.adaptLedger(scripts[0].ID)) != 2 || len(e.adaptLedger(scripts[1].ID)) != 0 {
		t.Fatal("重放/异载进行了模型调用或扣费")
	}
	m2CheckPGAccounting(t, e, before-3, 20, 1)
}

func TestMarketingV21_PG_M2_TwoInstancesBothMissRenderKey(t *testing.T) {
	for _, differentScript := range []bool{false, true} {
		t.Run(fmt.Sprint(differentScript), func(t *testing.T) {
			e, scripts, _ := m2PGH3(t)
			before := e.credit().Balance
			entered, release := []chan struct{}{make(chan struct{}), make(chan struct{})}, []chan struct{}{make(chan struct{}), make(chan struct{})}
			done := make(chan error, 2)
			for i := range 2 {
				svc := m2ServiceInstance(e, m2GatedCaller(entered[i], release[i], fmt.Sprintf("instance-%d", i)))
				id := scripts[0].ID
				if differentScript {
					id = scripts[i].ID
				}
				go func() {
					_, err := svc.Render(context.Background(), e.user, uint64(id), &model.RenderMarketingScriptReq{IdempotencyKey: "same"})
					done <- err
				}()
			}
			for i := range 2 {
				m2Wait(t, entered[i])
			}
			close(release[0])
			close(release[1])
			conflicts := 0
			for range 2 {
				err := m2Result(t, done)
				if differentScript && errors.Is(err, errcode.ErrIdempotencyConflict) {
					conflicts++
					continue
				}
				v21Must(t, err)
			}
			if differentScript && conflicts != 1 {
				t.Fatalf("同键异脚本必须恰有一个409: %d", conflicts)
			}
			var all []model.GenerationTask
			v21Must(t, e.db.Find(&all).Error)
			if len(all) != 1 {
				t.Fatalf("只能创建一个赢家: %+v", all)
			}
			sid, ok := idcodec.Decode(strings.TrimPrefix(all[0].NodeID, model.MarketingNodePrefix))
			if !ok {
				t.Fatal("节点身份错误")
			}
			pv, err := e.svc.Prompt(context.Background(), e.user, sid, "")
			v21Must(t, err)
			if pv.Adaptation == nil || pv.Adaptation.Version != 1 || pv.Adaptation.Prompt != m2TaskPrompt(t, all[0]) {
				t.Fatal("任务必须使用库内赢家实际稿与版本")
			}
			m2CheckPGAccounting(t, e, before-3, 20, 1)
			if len(e.adaptLedger(idcodec.ID(sid))) != 2 {
				t.Fatal("转写只能扣一次")
			}
		})
	}
}

func TestMarketingV21_PG_M2_RenderAdoptsConcurrentSavedWinner(t *testing.T) {
	e, scripts, _ := m2PGH3(t)
	id := uint64(scripts[0].ID)
	before := e.credit().Balance
	entered, release := make(chan struct{}), make(chan struct{})
	svc := m2ServiceInstance(e, m2GatedCaller(entered, release, "loser-output"))
	done := make(chan error, 1)
	go func() {
		_, err := svc.Render(context.Background(), e.user, id, &model.RenderMarketingScriptReq{IdempotencyKey: "auto"})
		done <- err
	}()
	m2Wait(t, entered)
	// 第二个独立服务先成功保存同输入的稿，自动候选不得继续使用自己的输出。
	winner := m2ServiceInstance(e, e.caller)
	pv, err := winner.Adapt(context.Background(), e.user, id, &model.AdaptMarketingScriptReq{ExpectedVersion: e.promptVersion(scripts[0].ID, true)})
	close(release)
	v21Must(t, err)
	v21Must(t, m2Result(t, done))
	tasks := e.scriptTasks(scripts[0].ID)
	if len(tasks) != 1 || pv.Adaptation == nil || m2TaskPrompt(t, tasks[0]) != pv.Adaptation.Prompt || strings.Contains(m2TaskPrompt(t, tasks[0]), "loser-output") {
		t.Fatal("输家输出污染出片")
	}
	current, err := e.svc.Prompt(context.Background(), e.user, id, "")
	v21Must(t, err)
	if current.Adaptation.Version != pv.Adaptation.Version || len(e.adaptLedger(scripts[0].ID)) != 2 {
		t.Fatal("采用赢家时重复写稿、推进版本或收费")
	}
	m2CheckPGAccounting(t, e, before-3, 20, 1)
}

func TestMarketingV21_PG_M2_RenderFailureRollsBackAdaptAndAllMoney(t *testing.T) {
	for _, failure := range []string{"balance", "queue", "task_limit"} {
		t.Run(failure, func(t *testing.T) {
			e, scripts, q := m2PGH3(t)
			if failure == "balance" {
				m2SetAvailable(t, e, 3)
			}
			if failure == "queue" {
				q.fail = errors.New("queue unavailable after insert")
			}
			if failure == "task_limit" {
				e.taskSvc = service.NewGenerationTaskService(service.GenerationTaskDeps{Repo: e.taskRepo, Registry: e.models, Assets: e.assets, Broadcaster: e.bc, Limits: &fakeLimits{initial: e.initial, defMax: 0}})
				e.rebuild(nil)
			}
			before := e.credit()
			var ledgerBefore int64
			v21Must(t, e.db.Model(&model.CreditLedger{}).Count(&ledgerBefore).Error)
			_, err := e.render(scripts[0].ID, "failure")
			switch failure {
			case "balance":
				wantMktCode(t, err, errcode.ErrInsufficientCredits.Code)
			case "task_limit":
				wantMktCode(t, err, errcode.ErrTooManyTasks.Code)
			default:
				if !errors.Is(err, q.fail) {
					t.Fatalf("队列错误: %v", err)
				}
			}
			var ledgerAfter int64
			v21Must(t, e.db.Model(&model.CreditLedger{}).Count(&ledgerAfter).Error)
			if ledgerAfter != ledgerBefore || len(e.adaptLedger(scripts[0].ID)) != 0 || string(v21Script(t, e, uint64(scripts[0].ID)).Adaptations) != "{}" {
				t.Fatal("零任务残留了转写稿或费用")
			}
			m2CheckPGAccounting(t, e, before.Balance, before.Frozen, 0)
		})
	}
}

func TestMarketingV21_PG_M2_RenderRoundPartialReplayAndTargetBinding(t *testing.T) {
	e, scripts, _ := m2PGH3(t)
	m2SetAvailable(t, e, 23)
	ctx := context.Background()
	req := m2RoundReq("round", scripts)
	first, err := e.svc.RenderRound(ctx, e.user, uint64(scripts[0].RoundID), req)
	v21Must(t, err)
	if len(first.Skipped) != 1 || len(e.scriptTasks(scripts[0].ID)) != 1 || len(e.scriptTasks(scripts[1].ID)) != 0 {
		t.Fatalf("部分成功: %+v", first)
	}
	if len(e.adaptLedger(scripts[1].ID)) != 0 {
		t.Fatal("失败项不应单独收转写费")
	}
	m2CheckPGAccounting(t, e, 20, 20, 1)
	m2SetAvailable(t, e, 23)
	_, err = e.svc.RenderRound(ctx, e.user, uint64(scripts[0].RoundID), req)
	v21Must(t, err)
	calls, before := e.caller.count(mktAdaptStep), e.credit()
	m2CheckPGAccounting(t, e, 40, 40, 2)
	_, err = e.svc.RenderRound(ctx, e.user, uint64(scripts[0].RoundID), req)
	v21Must(t, err)
	if e.caller.count(mktAdaptStep) != calls || e.credit() != before {
		t.Fatal("整轮全命中重复调用/扣费")
	}
	added := *v21Script(t, e, uint64(scripts[0].ID))
	added.ID, added.Seq, added.Status, added.TaskIDs, added.Adaptations = 0, 99, model.MarketingScriptDraft, []byte(`[]`), []byte(`{}`)
	v21Must(t, e.db.Create(&added).Error)
	req.ScriptIDs = append(req.ScriptIDs, idcodec.ID(added.ID))
	_, err = e.svc.RenderRound(ctx, e.user, uint64(scripts[0].RoundID), req)
	wantMktCode(t, err, errcode.ErrIdempotencyConflict.Code)
	if e.caller.count(mktAdaptStep) != calls || len(e.scriptTasks(idcodec.ID(added.ID))) != 0 || e.credit() != before {
		t.Fatal("旧键为新脚本偷跑")
	}
	// 显式携带旧目标清单可安全重放，新增脚本仍被跳过。
	req.ScriptIDs = []idcodec.ID{scripts[0].ID, scripts[1].ID}
	out, err := e.svc.RenderRound(ctx, e.user, uint64(scripts[0].RoundID), req)
	v21Must(t, err)
	if len(out.Skipped) != 1 || e.caller.count(mktAdaptStep) != calls {
		t.Fatal("显式原目标应只读重放")
	}
	m2CheckPGAccounting(t, e, before.Balance, before.Frozen, 2)
}

// 模型真正运行在父链锁外，公开写抢先提交后，旧HTTP候选必须零覆盖、零新增费用。
func TestMarketingV21_PG_M2_InFlightCandidateRejectsPublicEdits(t *testing.T) {
	for _, auto := range []bool{false, true} {
		for _, change := range []string{"original", "narration", "images", "settings", "generation", "cancel", "deleted"} {
			t.Run(fmt.Sprintf("auto=%v/%s", auto, change), func(t *testing.T) {
				e, scripts, _ := m2PGH3(t)
				e.addVideoModel("video-b")
				entered, release := make(chan struct{}), make(chan struct{})
				svc := m2ServiceInstance(e, m2GatedCaller(entered, release, "obsolete candidate"))
				id := uint64(scripts[0].ID)
				expected := e.promptVersion(scripts[0].ID, true)
				done := make(chan error, 1)
				go func() {
					var err error
					if auto {
						_, err = svc.Render(context.Background(), e.user, id, &model.RenderMarketingScriptReq{IdempotencyKey: "inflight"})
					} else {
						_, err = svc.Adapt(context.Background(), e.user, id, &model.AdaptMarketingScriptReq{ExpectedVersion: expected})
					}
					done <- err
				}()
				m2Wait(t, entered)
				err := m2ChangePublicInput(e, &scripts[0], change)
				credit := e.credit()
				saved := *v21Script(t, e, id)
				var ledgers int64
				v21Must(t, e.db.Model(&model.CreditLedger{}).Count(&ledgers).Error)
				close(release)
				v21Must(t, err)
				code := errcode.ErrMarketingConflict.Code
				if change == "deleted" {
					code = errcode.ErrMarketingNotFound.Code
				}
				wantMktCode(t, m2Result(t, done), code)
				after := v21Script(t, e, id)
				var actual int64
				v21Must(t, e.db.Model(&model.CreditLedger{}).Count(&actual).Error)
				if mustJSONText(saved) != mustJSONText(after) || e.credit() != credit || actual != ledgers || len(e.scriptTasks(scripts[0].ID)) != 0 || m2TaskBroadcasts(e) != 0 {
					t.Fatal("陈旧候选覆盖了当前稿或留下收费、任务、通知")
				}
			})
		}
	}
}

func m2ChangePublicInput(e *mktEnv, sc *model.MarketingScriptView, change string) error {
	ctx := context.Background()
	switch change {
	case "original":
		_, err := e.svc.EditPrompt(ctx, e.user, uint64(sc.ID), &model.EditMarketingPromptReq{Format: "h3", Prompt: "用户刚保存的原稿", ExpectedVersion: e.promptVersion(sc.ID, false)})
		return err
	case "narration":
		narr := make([]string, len(sc.Shots))
		for i, shot := range sc.Shots {
			narr[i] = shot.Narration
		}
		narr[0] = "用户刚修改的旁白"
		_, err := e.svc.UpdateScript(ctx, e.user, uint64(sc.ID), &model.UpdateMarketingScriptReq{Narrations: narr, Revision: sc.Revision})
		return err
	case "images":
		p, err := e.repo.Project(ctx, e.user, v21Script(e.t, e, uint64(sc.ID)).ProjectID, false)
		if err != nil {
			return err
		}
		_, err = e.svc.UpdateProject(ctx, e.user, p.ID, &model.UpdateMarketingProjectReq{ProductAssetIDs: []uint64{2, 1}, Revision: p.Revision})
		return err
	case "settings":
		r, err := e.repo.Round(ctx, e.user, uint64(sc.RoundID), false)
		if err != nil {
			return err
		}
		var st model.MarketingSettings
		if err := json.Unmarshal(r.Settings, &st); err != nil {
			return err
		}
		_, err = e.svc.UpdateRoundSettings(ctx, e.user, r.ID, &model.UpdateMarketingRoundSettingsReq{VideoModel: "video-b", Params: st.Params})
		return err
	case "generation", "cancel":
		p := v21Script(e.t, e, uint64(sc.ID)).ProjectID
		r, err := e.svc.RefreshFacts(ctx, e.user, p)
		if err == nil && change == "cancel" {
			_, err = e.svc.CancelRound(ctx, e.user, uint64(r.ID))
		}
		return err
	case "deleted":
		return e.svc.DeleteProject(ctx, e.user, v21Script(e.t, e, uint64(sc.ID)).ProjectID)
	}
	return errors.New("未知输入变化")
}

func TestMarketingV21_PG_M2_AllFailedRoundRetainsOriginalTargets(t *testing.T) {
	e, scripts, _ := m2PGH3(t)
	ctx := context.Background()
	req := m2RoundReq("all-failed", scripts)
	m2SetAvailable(t, e, 3)
	_, err := e.svc.RenderRound(ctx, e.user, uint64(scripts[0].RoundID), req)
	wantMktCode(t, err, errcode.ErrInsufficientCredits.Code)
	m2CheckPGAccounting(t, e, 3, 0, 0)
	added := *v21Script(t, e, uint64(scripts[0].ID))
	added.ID, added.Seq = 0, 99
	v21Must(t, e.db.Create(&added).Error)
	calls := e.caller.count(mktAdaptStep)
	_, err = e.svc.RenderRound(ctx, e.user, uint64(scripts[0].RoundID), &model.RenderMarketingRoundReq{IdempotencyKey: req.IdempotencyKey})
	wantMktCode(t, err, errcode.ErrInvalidParams.Code)
	if e.caller.count(mktAdaptStep) != calls {
		t.Fatal("丢失目标清单不得重新调用模型")
	}
	m2SetAvailable(t, e, 23)
	_, err = e.svc.RenderRound(ctx, e.user, uint64(scripts[0].RoundID), req)
	v21Must(t, err)
	if len(e.scriptTasks(idcodec.ID(added.ID))) != 0 {
		t.Fatal("旧点击带出了后来新增脚本")
	}
	m2CheckPGAccounting(t, e, 20, 20, 1)
}

func TestMarketingV21_PG_M2_RenderAutoAdaptAffordabilityPrecheck(t *testing.T) {
	e, scripts, _ := m2PGH3(t)
	ctx := context.Background()
	m2SetAvailable(t, e, 3)
	calls := e.caller.count(mktAdaptStep)
	_, err := e.render(scripts[0].ID, "poor-single")
	wantMktCode(t, err, errcode.ErrInsufficientCredits.Code)
	// 注入 params.prompt="" 或非法参数均不得绕过预检或悄然回落 0 积分而调用转写 LLM。
	_, err = e.svc.Render(ctx, e.user, uint64(scripts[0].ID), &model.RenderMarketingScriptReq{
		IdempotencyKey: "poor-prompt-override",
		Params:         map[string]any{"prompt": ""},
	})
	wantMktCode(t, err, errcode.ErrInsufficientCredits.Code)
	_, err = e.svc.Render(ctx, e.user, uint64(scripts[0].ID), &model.RenderMarketingScriptReq{
		IdempotencyKey: "bad-duration-param",
		Params:         map[string]any{"duration": 999},
	})
	wantMktCode(t, err, errcode.ErrTaskInput.Code)
	_, err = e.svc.RenderRound(ctx, e.user, uint64(scripts[0].RoundID), m2RoundReq("poor-round", scripts))
	wantMktCode(t, err, errcode.ErrInsufficientCredits.Code)
	if e.caller.count(mktAdaptStep) != calls || string(v21Script(t, e, uint64(scripts[0].ID)).Adaptations) != "{}" {
		t.Fatal("余额不足或非法参数时自动出片不得调用转写 LLM 或写稿")
	}

	// 有声定价高于无声定价时，即使模型默认或请求伪造 generate_audio=false，预检仍按强制 generate_audio=true 核验余额。
	audioSnap := mktVideoSnapshot()
	audioSnap.Model.Key = "video-h3-audio"
	audioSnap.Model.Capabilities.PromptFormat = "h3"
	paramsCopy := append(modelcfg.ParamSet(nil), audioSnap.Model.Capabilities.Params...)
	for i := range paramsCopy {
		if paramsCopy[i].Name == "generate_audio" {
			paramsCopy[i].Default = false
		}
	}
	audioSnap.Model.Capabilities.Params = paramsCopy
	audioSnap.Model.Pricing.Tiers = []modelcfg.Tier{
		{On: true, Unit: 10},
		{On: true, Unit: 25, When: map[string]any{"generate_audio": true}},
	}
	e.models.Snapshots[audioSnap.Model.Key] = audioSnap
	m2SetAvailable(t, e, 22) // 足付无声(10+3=13)及旧基础视频(20)，但不足支付有声全额(25+3=28)
	_, err = e.svc.Render(ctx, e.user, uint64(scripts[0].ID), &model.RenderMarketingScriptReq{
		IdempotencyKey: "poor-audio-override",
		VideoModel:     audioSnap.Model.Key,
		Params:         map[string]any{"generate_audio": false},
	})
	wantMktCode(t, err, errcode.ErrInsufficientCredits.Code)
	if e.caller.count(mktAdaptStep) != calls {
		t.Fatalf("伪造 generate_audio=false 绕过了有声报价预检，仍调用 LLM: calls=%d", e.caller.count(mktAdaptStep))
	}

	// 无账户行的新用户在初始积分不足时同样前置拒绝且不得惰性开户。
	v21Must(t, e.db.Where("user_id = ?", e.user).Delete(&model.UserCredit{}).Error)
	e.taskSvc = service.NewGenerationTaskService(service.GenerationTaskDeps{Repo: e.taskRepo, Registry: e.models, Assets: e.assets, Broadcaster: e.bc, Limits: &fakeLimits{initial: 3, defMax: 20}})
	e.svc = service.NewMarketingService(service.MarketingDeps{Repo: e.repo, Models: e.models, Caller: e.caller, Tasks: e.taskSvc, Assets: e.assets, Credits: stepfake.Policy{Initial: 3}, Broadcaster: e.bc})
	e.taskSvc.SetTaskObserver(e.svc)
	_, err = e.render(scripts[0].ID, "poor-new-user")
	wantMktCode(t, err, errcode.ErrInsufficientCredits.Code)
	var rows int64
	v21Must(t, e.db.Model(&model.UserCredit{}).Where("user_id = ?", e.user).Count(&rows).Error)
	if rows != 0 || e.caller.count(mktAdaptStep) != calls {
		t.Fatalf("余额不足前置拒绝不得开户或调用模型: rows=%d calls=%d", rows, e.caller.count(mktAdaptStep))
	}
}

func TestMarketingV21_PG_M2_RenderRoundRejectsDisjointTargetsSequentialAndConcurrent(t *testing.T) {
	e, scripts, _ := m2PGH3(t)
	ctx := context.Background()
	roundID := uint64(scripts[0].RoundID)
	reqA := &model.RenderMarketingRoundReq{IdempotencyKey: "disjoint-seq", ScriptIDs: []idcodec.ID{scripts[0].ID}}
	_, err := e.svc.RenderRound(ctx, e.user, roundID, reqA)
	v21Must(t, err)
	calls, credit := e.caller.count(mktAdaptStep), e.credit()

	// 同根键切换成完全不交的目标集合 [B]，及同根键混用单条 Render，必须在 LLM 前 90001 拒绝。
	reqB := &model.RenderMarketingRoundReq{IdempotencyKey: "disjoint-seq", ScriptIDs: []idcodec.ID{scripts[1].ID}}
	_, err = e.svc.RenderRound(ctx, e.user, roundID, reqB)
	wantMktCode(t, err, errcode.ErrIdempotencyConflict.Code)
	_, err = e.render(scripts[1].ID, "disjoint-seq")
	wantMktCode(t, err, errcode.ErrIdempotencyConflict.Code)
	if e.caller.count(mktAdaptStep) != calls || len(e.scriptTasks(scripts[1].ID)) != 0 || e.credit() != credit {
		t.Fatal("同根键不交集合/混用单条调用触发了 LLM 或创建任务")
	}

	// 单条先提交后，再用同根键走 RenderRound（不交脚本）也必须 90001 拒绝。
	_, err = e.render(scripts[0].ID, "single-first")
	wantMktCode(t, err, errcode.ErrMarketingScriptBusy.Code) // scripts[0] 已在出片中
	if err := e.db.Model(&model.GenerationTask{}).Where("node_id = ?", "mkt:"+idcodec.Encode(uint64(scripts[0].ID))).Update("status", model.TaskSucceeded).Error; err != nil {
		t.Fatal(err)
	}
	v21Must(t, e.svc.OnTaskChanged(ctx, &model.GenerationTask{UserID: e.user, Kind: model.KindVideo, NodeID: "mkt:" + idcodec.Encode(uint64(scripts[0].ID))}))
	_, err = e.render(scripts[0].ID, "single-first")
	v21Must(t, err)
	calls2 := e.caller.count(mktAdaptStep)
	_, err = e.svc.RenderRound(ctx, e.user, roundID, &model.RenderMarketingRoundReq{IdempotencyKey: "single-first", ScriptIDs: []idcodec.ID{scripts[1].ID}})
	wantMktCode(t, err, errcode.ErrIdempotencyConflict.Code)
	if e.caller.count(mktAdaptStep) != calls2 || len(e.scriptTasks(scripts[1].ID)) != 0 {
		t.Fatal("单条占用根键后整轮不交集合仍调用了模型或创建了任务")
	}

	// 并发竞态：两实例同时携带同根键和不交目标集合 [A] vs [B]，双过预检后 PG 根键锁只允许一个提交，另一个必须 90001 回滚。
	e2, scripts2, _ := m2PGH3(t)
	round2 := uint64(scripts2[0].RoundID)
	before2 := e2.credit().Balance
	entered := []chan struct{}{make(chan struct{}), make(chan struct{})}
	release := []chan struct{}{make(chan struct{}), make(chan struct{})}
	done := make(chan error, 2)
	for i := range 2 {
		svc := m2ServiceInstance(e2, m2GatedCaller(entered[i], release[i], fmt.Sprintf("race-%d", i)))
		target := scripts2[i].ID
		go func() {
			_, err := svc.RenderRound(context.Background(), e2.user, round2, &model.RenderMarketingRoundReq{
				IdempotencyKey: "concurrent-disjoint",
				ScriptIDs:      []idcodec.ID{target},
			})
			done <- err
		}()
	}
	for i := range 2 {
		m2Wait(t, entered[i])
	}
	close(release[0])
	close(release[1])
	var okCnt, conflictCnt int
	for range 2 {
		err := m2Result(t, done)
		if err == nil {
			okCnt++
		} else if errors.Is(err, errcode.ErrIdempotencyConflict) {
			conflictCnt++
		} else {
			t.Fatalf("非预期错误: %v", err)
		}
	}
	if okCnt != 1 || conflictCnt != 1 {
		t.Fatalf("并发不交根键应恰有一胜一负(90001): ok=%d conflict=%d", okCnt, conflictCnt)
	}
	var totalTasks int64
	v21Must(t, e2.db.Model(&model.GenerationTask{}).Count(&totalTasks).Error)
	if totalTasks != 1 || e2.credit().Frozen != 20 || e2.credit().Balance != before2-3 {
		t.Fatalf("并发不交根键只能建1个任务并扣1次转写: tasks=%d credit=%+v", totalTasks, e2.credit())
	}
}
