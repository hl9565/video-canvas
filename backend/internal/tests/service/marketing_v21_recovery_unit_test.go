package service_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"gorm.io/gorm/clause"

	"video-canvas/internal/marketing"
	"video-canvas/internal/model"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
	"video-canvas/internal/service/stepfake"
)

// v21Memory 是事务边界fake；用于无网络运行安全测试，PG并发语义另由真库矩阵验证。
type v21Memory struct {
	service.MarketingRepo
	p           model.MarketingProject
	r           model.MarketingRound
	run         model.MarketingStepRun
	sc          model.MarketingScript
	tasks       []model.GenerationTask
	account     model.UserCredit
	ledgers     []model.CreditLedger
	locks       []string
	failCommits int
	enqueued    int
	writes      int
}

func newV21Memory() *v21Memory {
	return &v21Memory{
		p:       model.MarketingProject{BaseModel: model.BaseModel{ID: 1}, UserID: 7, Title: "原项目", ProductAssetIDs: []byte(`[]`)},
		r:       model.MarketingRound{ID: 2, UserID: 7, ProjectID: 1, Status: "scripting", ExecutionGen: 2, Settings: []byte(`{}`), Warnings: []byte(`[]`)},
		run:     model.MarketingStepRun{ID: 3, UserID: 7, RoundID: 2, RoundGen: 2, StepKey: marketing.StepFollow, Status: "running", Frozen: 10, ExecToken: "owner"},
		sc:      model.MarketingScript{ID: 4, UserID: 7, ProjectID: 1, RoundID: 2, Status: "rewriting", Shots: []byte(`[]`), Original: []byte(`{}`)},
		account: model.UserCredit{UserID: 7, Balance: 100, Frozen: 10},
	}
}
func (m *v21Memory) WithTx(_ context.Context, fn func(repository.MarketingTx) error) error {
	if m.failCommits > 0 {
		m.failCommits--
		return errors.New("暂时数据库错误")
	}
	return fn(m)
}
func (m *v21Memory) Project(_ context.Context, u, id uint64, lock bool) (*model.MarketingProject, error) {
	if m.p.DeletedAt.Valid || m.p.ID != id || m.p.UserID != u {
		return nil, repository.ErrNotFound
	}
	if lock {
		m.locks = append(m.locks, "project")
	}
	p := m.p
	return &p, nil
}
func (m *v21Memory) ProjectUnscopedLock(_ context.Context, u, id uint64) (*model.MarketingProject, error) {
	if m.p.ID != id || m.p.UserID != u {
		return nil, repository.ErrNotFound
	}
	m.locks = append(m.locks, "project_unscoped")
	p := m.p
	return &p, nil
}
func (m *v21Memory) Round(_ context.Context, u, id uint64, lock bool) (*model.MarketingRound, error) {
	if m.r.ID != id || m.r.UserID != u {
		return nil, repository.ErrNotFound
	}
	if lock {
		m.locks = append(m.locks, "round")
	}
	r := m.r
	return &r, nil
}
func (m *v21Memory) RoundByID(ctx context.Context, id uint64, lock bool) (*model.MarketingRound, error) {
	return m.Round(ctx, m.r.UserID, id, lock)
}
func (m *v21Memory) RoundScripts(context.Context, ...uint64) ([]model.MarketingScript, error) {
	return []model.MarketingScript{m.sc}, nil
}
func (m *v21Memory) Script(_ context.Context, u, id uint64, lock bool) (*model.MarketingScript, error) {
	if m.sc.ID != id || m.sc.UserID != u {
		return nil, repository.ErrNotFound
	}
	if lock {
		m.locks = append(m.locks, "script")
	}
	sc := m.sc
	return &sc, nil
}
func (m *v21Memory) Run(_ context.Context, u, id uint64, _ bool) (*model.MarketingStepRun, error) {
	if m.run.ID != id || m.run.UserID != u {
		return nil, repository.ErrNotFound
	}
	r := m.run
	return &r, nil
}
func (m *v21Memory) RoundRuns(context.Context, ...uint64) ([]model.MarketingStepRun, error) {
	return []model.MarketingStepRun{m.run}, nil
}
func (m *v21Memory) ListUnsettledRuns(context.Context, int) ([]model.MarketingStepRun, error) {
	if m.run.Status != "running" && m.run.Status != "queued" {
		return nil, nil
	}
	return []model.MarketingStepRun{m.run}, nil
}
func (m *v21Memory) ScriptTasks(context.Context, uint64, []string) ([]model.GenerationTask, error) {
	return m.tasks, nil
}
func (m *v21Memory) UpdateScript(_ context.Context, _ uint64, f map[string]any) error {
	m.writes++
	if x, ok := f["status"].(string); ok {
		m.sc.Status = x
	}
	if x, ok := f["error"].(string); ok {
		m.sc.Error = x
	}
	return nil
}
func v21IntUpdate(cur int, v any) int {
	if x, ok := v.(int); ok {
		return x
	}
	if x, ok := v.(clause.Expr); ok {
		return cur + x.Vars[0].(int)
	}
	return cur
}
func (m *v21Memory) UpdateRound(_ context.Context, _ uint64, f map[string]any) error {
	if x, ok := f["status"].(string); ok {
		m.r.Status = x
	}
	if x, ok := f["error"].(string); ok {
		m.r.Error = x
	}
	if x, ok := f["execution_gen"].(int64); ok {
		m.r.ExecutionGen = x
	}
	m.r.Charged = v21IntUpdate(m.r.Charged, f["charged"])
	return nil
}
func (m *v21Memory) UpdateRun(_ context.Context, _ uint64, f map[string]any) error {
	if x, ok := f["status"].(string); ok {
		m.run.Status = x
	}
	if x, ok := f["exec_token"].(string); ok {
		m.run.ExecToken = x
	}
	if _, ok := f["lease_until"]; ok {
		m.run.LeaseUntil = nil
	}
	m.run.Charged = v21IntUpdate(m.run.Charged, f["charged"])
	return nil
}
func (m *v21Memory) UpdateRunIf(ctx context.Context, id uint64, from []string, f map[string]any) error {
	if !slices.Contains(from, m.run.Status) {
		return repository.ErrStateConflict
	}
	return m.UpdateRun(ctx, id, f)
}
func (m *v21Memory) ReclaimExpiredStepRun(ctx context.Context, id uint64, token string, now time.Time, drain bool) (bool, error) {
	if m.run.Status != "running" || m.run.ExecToken != token {
		return false, nil
	}
	expired := token != "" && m.run.LeaseUntil != nil && !m.run.LeaseUntil.After(now)
	legacy := token == "" && m.run.LeaseUntil == nil && drain
	if !expired && !legacy {
		return false, nil
	}
	return true, m.UpdateRun(ctx, id, map[string]any{"status": "canceled", "exec_token": "", "lease_until": nil})
}
func (m *v21Memory) LockCredit(context.Context, uint64) (*model.UserCredit, error) {
	m.locks = append(m.locks, "credit")
	return &m.account, nil
}
func (m *v21Memory) AddCredit(_ context.Context, _ uint64, b, f int) error {
	m.account.Balance += b
	m.account.Frozen += f
	return nil
}
func (m *v21Memory) InsertLedger(_ context.Context, l *model.CreditLedger) (bool, error) {
	for _, old := range m.ledgers {
		if old.Type == l.Type {
			return false, nil
		}
	}
	m.ledgers = append(m.ledgers, *l)
	return true, nil
}
func (m *v21Memory) EnqueueRound(context.Context, uint64) error { m.enqueued++; return nil }
func v21MemoryService(m *v21Memory, opts ...service.MarketingOption) *service.MarketingService {
	return service.NewMarketingService(service.MarketingDeps{Repo: m, Models: stepfake.NewModels()}, opts...)
}

func TestMarketingV21_StaleFinishSettlesActualUsageOnce(t *testing.T) {
	for _, reason := range []string{"flag", "generation", "round", "deleted"} {
		t.Run(reason, func(t *testing.T) {
			m := newV21Memory()
			lease := time.Now().Add(time.Minute)
			m.run.LeaseUntil = &lease
			worker := m.run
			switch reason {
			case "flag":
				m.run.CancelRequested = true
			case "generation":
				m.r.ExecutionGen++
			case "round":
				m.r.Status = "canceled"
			case "deleted":
				m.p.DeletedAt.Valid = true
			}
			svc := v21MemoryService(m)
			e := &mktEnv{svc: svc}
			for range 2 {
				v21Must(t, v21Finish(e, &m.r, &worker, func(context.Context, repository.MarketingTx, *model.MarketingRound) error {
					t.Error("陈旧产出被应用")
					return nil
				}))
			}
			if m.account.Balance != 97 || m.account.Frozen != 0 || m.run.Status != "canceled" || m.run.Charged != 3 || len(m.ledgers) != 2 || m.sc.Status != "rewriting" || m.writes != 0 {
				t.Fatalf("真实3/退款7仅一次且不写脚本: %+v %+v %+v", m.account, m.run, m.ledgers)
			}
			if !slices.Equal(m.locks[:4], []string{"project_unscoped", "round", "script", "credit"}) {
				t.Fatalf("锁序错误:%v", m.locks)
			}
		})
	}
}

func TestMarketingV21_FinishRejectsForeignTokenAndParent(t *testing.T) {
	for _, wrong := range []string{"token", "round_user", "round_project", "script_parent"} {
		t.Run(wrong, func(t *testing.T) {
			m := newV21Memory()
			run := m.run
			switch wrong {
			case "token":
				run.ExecToken = "other"
			case "round_user":
				m.r.UserID = 9
			case "round_project":
				m.r.ProjectID = 99
			case "script_parent":
				m.sc.ProjectID = 99
			}
			err := v21Finish(&mktEnv{svc: v21MemoryService(m)}, &m.r, &run, nil)
			if wrong != "token" && !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("错父链应拒绝:%v", err)
			}
			if m.run.Status != "running" || m.account.Frozen != 10 || len(m.ledgers) != 0 {
				t.Fatal("错误执行权仍结算")
			}
		})
	}
}

func TestMarketingV21_LeaseRecoveryMatrix(t *testing.T) {
	for _, kind := range []string{"live", "legacy_protected", "legacy_drained", "expired_active_empty", "expired_active_mixed", "expired_stale", "expired_deleted", "expired_canceled", "queued_stale"} {
		t.Run(kind, func(t *testing.T) {
			m := newV21Memory()
			expired := time.Now().Add(-time.Minute)
			live := time.Now().Add(time.Minute)
			m.run.LeaseUntil = &expired
			switch kind {
			case "live":
				m.run.LeaseUntil = &live
			case "legacy_protected", "legacy_drained":
				m.run.ExecToken = ""
				m.run.LeaseUntil = nil
			case "expired_active_mixed":
				m.tasks = []model.GenerationTask{{Status: model.TaskSucceeded}, {Status: model.TaskFailed, ErrorMessage: "历史失败"}}
			case "expired_stale":
				m.r.ExecutionGen++
			case "expired_deleted":
				m.p.DeletedAt.Valid = true
			case "expired_canceled":
				m.r.Status = "canceled"
			case "queued_stale":
				m.run.Status = "queued"
				m.run.CancelRequested = true
			}
			svc := v21MemoryService(m, service.WithMarketingLegacyNullLeaseDrainCompleted(kind == "legacy_drained"))
			for range 2 {
				v21Must(t, svc.RecoverUnsettledMarketingRuns(context.Background()))
			}
			if kind == "live" || kind == "legacy_protected" {
				if m.run.Status != "running" || len(m.ledgers) != 0 || m.account.Frozen != 10 {
					t.Fatal("在途worker被抢退")
				}
				return
			}
			if m.run.Status != "canceled" || m.account.Frozen != 0 || m.account.Balance != 100 || len(m.ledgers) != 2 {
				t.Fatalf("回收应全退一次: %+v", m)
			}
			want := "rewriting"
			if kind == "legacy_drained" || kind == "expired_active_empty" {
				want = "draft"
			}
			if kind == "expired_active_mixed" {
				want = "failed"
				if m.sc.Error != "历史失败" {
					t.Fatal("未写物理error")
				}
			}
			if m.sc.Status != want || string(m.sc.Shots) != "[]" {
				t.Fatalf("脚本恢复错误:%s want %s", m.sc.Status, want)
			}
			if kind == "expired_active_empty" || kind == "expired_active_mixed" {
				if m.r.Status != "ready" {
					t.Fatal("未收口轮次")
				}
			}
		})
	}
}

func TestMarketingV21_RecoveryDoesNotDispatchOldGeneration(t *testing.T) {
	m := newV21Memory()
	m.run.Status = "queued"
	m.run.RoundGen = 1
	v21Must(t, v21MemoryService(m).RecoverUnsettledMarketingRuns(context.Background()))
	if m.enqueued != 0 || m.run.Status != "canceled" {
		t.Fatalf("旧代不应重新派发: %d %+v", m.enqueued, m.run)
	}
}
