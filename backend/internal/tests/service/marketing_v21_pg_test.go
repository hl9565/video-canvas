package service_test

import (
	"context"
	"errors"
	"fmt"
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
	"video-canvas/internal/provider"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
	"video-canvas/internal/service/stepfake"
	"video-canvas/internal/stepkit"
)

// v21Env 仅使用 PG16 真实 SQL 迁移至0036，独立 schema 隔离每个用例。
func v21Env(t *testing.T) *mktEnv {
	t.Helper()
	db := slice2cPG16(t)
	slice2cMigrate(t, db, "0036_drama_edit.sql")
	e := &mktEnv{t: t, db: db, repo: repository.NewMarketingRepository(db), taskRepo: repository.NewGenerationTaskRepository(db),
		models: stepfake.NewModels(), caller: newMktCaller(), assets: &mktAssets{items: map[uint64]*mktAsset{}}, canvases: &mktCanvases{},
		queue: &mktQueue{}, bc: &fakeTaskBroadcaster{}, user: 7, initial: 1000, reg: stepkit.NewRegistry(), resolver: stepkit.NewResolver(emptyBindings{}, stepfake.NewLibrary())}
	marketing.RegisterSteps(e.reg)
	e.repo.SetEnqueuer(e.queue)
	e.models.Snapshots["text-a"] = stepfake.TextModel("text-a", true, true, 3)
	e.models.Snapshots[mktVideoKey] = mktVideoSnapshot()
	e.taskSvc = service.NewGenerationTaskService(service.GenerationTaskDeps{Repo: e.taskRepo, Registry: &mktRegistry{snap: mktVideoSnapshot()}, Executor: &fakeTaskExecutor{}, Assets: e.assets, Broadcaster: e.bc, Limits: &fakeLimits{initial: 1000, defMax: 20}})
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
			want := int64(0)
			if cancelFirst {
				want = 1
			}
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
	slice2cMigrate(t, db, "0035_marketing.sql")
	for _, sql := range []string{
		`INSERT INTO marketing_projects(id,user_id,title) VALUES(1,7,'旧项目')`,
		`INSERT INTO marketing_rounds(id,user_id,project_id,no,settings) VALUES(1,7,1,1,'{}')`,
		`INSERT INTO marketing_step_runs(id,user_id,round_id,step_key,status,frozen) VALUES(1,7,1,'marketing.facts','running',10),(2,7,1,'marketing.reverse','running',10)`,
		`INSERT INTO user_credits(user_id,balance,frozen) VALUES(7,100,20)`,
		`INSERT INTO credit_ledger(user_id,marketing_step_run_id,type,amount) VALUES(7,1,'freeze',10),(7,2,'freeze',10)`,
	} {
		v21Must(t, db.Exec(sql).Error)
	}
	slice2cMigrate(t, db, "0036_drama_edit.sql")
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
