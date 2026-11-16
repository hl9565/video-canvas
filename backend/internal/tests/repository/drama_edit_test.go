package repository_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"video-canvas/internal/marketing"
	"video-canvas/internal/model"
	"video-canvas/internal/repository"
)

// sqlRecorder 是轻量级本地 database/sql 驱动，用来精确捕获 SQL 轨迹并注入特定语句失败，
// 验证外层批次事务无隐式 SAVEPOINT、且显式保存点与扁平写原语绝不吞错。
type sqlRecorder struct {
	mu          sync.Mutex
	queries     []string
	failOnMatch string
	nextSeqID   int64
}

type recordDriver struct{ rec *sqlRecorder }

func (d recordDriver) Open(string) (driver.Conn, error) {
	return &recordConn{rec: d.rec}, nil
}

type recordConn struct{ rec *sqlRecorder }

func (c *recordConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (c *recordConn) Close() error                        { return nil }
func (c *recordConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *recordConn) BeginTx(_ context.Context, _ driver.TxOptions) (driver.Tx, error) {
	c.rec.record("BEGIN")
	return &recordTx{rec: c.rec}, nil
}

type recordResult struct{}

func (recordResult) LastInsertId() (int64, error) { return 1, nil }
func (recordResult) RowsAffected() (int64, error) { return 1, nil }

func (c *recordConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.rec.record(query)
	if err := c.rec.maybeFail(query); err != nil {
		return nil, err
	}
	return recordResult{}, nil
}

func (c *recordConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.rec.record(query)
	if err := c.rec.maybeFail(query); err != nil {
		return nil, err
	}
	if strings.Contains(strings.ToLower(query), "nextval") {
		c.rec.mu.Lock()
		c.rec.nextSeqID++
		id := c.rec.nextSeqID
		c.rec.mu.Unlock()
		return &singleValRows{cols: []string{"nextval"}, vals: []driver.Value{id}}, nil
	}
	return &singleValRows{cols: []string{"ok"}, vals: nil, empty: true}, nil
}

type recordTx struct{ rec *sqlRecorder }

func (tx *recordTx) Commit() error   { tx.rec.record("COMMIT"); return nil }
func (tx *recordTx) Rollback() error { tx.rec.record("ROLLBACK"); return nil }

type singleValRows struct {
	cols  []string
	vals  []driver.Value
	empty bool
	done  bool
}

func (r *singleValRows) Columns() []string { return r.cols }
func (r *singleValRows) Close() error      { return nil }
func (r *singleValRows) Next(dest []driver.Value) error {
	if r.empty || r.done {
		return io.EOF
	}
	r.done = true
	copy(dest, r.vals)
	return nil
}

func (r *sqlRecorder) record(q string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = append(r.queries, q)
}

func (r *sqlRecorder) maybeFail(q string) error {
	r.mu.Lock()
	sub := r.failOnMatch
	r.mu.Unlock()
	if sub != "" && strings.Contains(q, sub) {
		return fmt.Errorf("injected error on %s", sub)
	}
	return nil
}

func newRecordedGormDB(t *testing.T, rec *sqlRecorder) *gorm.DB {
	t.Helper()
	connector := &recordConnector{rec: rec}
	sqlDB := sql.OpenDB(connector)
	t.Cleanup(func() { _ = sqlDB.Close() })
	db, err := gorm.Open(postgres.New(postgres.Config{
		Conn:                 sqlDB,
		WithoutReturning:     true,
		PreferSimpleProtocol: true,
	}), &gorm.Config{SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("打开录制库失败：%v", err)
	}
	return db
}

type recordConnector struct{ rec *sqlRecorder }

func (c *recordConnector) Connect(context.Context) (driver.Conn, error) {
	return &recordConn{rec: c.rec}, nil
}
func (c *recordConnector) Driver() driver.Driver { return recordDriver{rec: c.rec} }

// TestTxEnsureCreditAndTxInsertTask_ZeroNestedSavepoints 验证批次写原语全程零 GORM 隐式 SAVEPOINT，仅含可控 sp_batch_<idx>。
func TestTxEnsureCreditAndTxInsertTask_ZeroNestedSavepoints(t *testing.T) {
	rec := &sqlRecorder{}
	db := newRecordedGormDB(t, rec)
	repo := repository.NewGenerationTaskRepository(db)
	ctx := context.Background()

	err := repo.WithTx(ctx, func(tx repository.GenerationTaskTx) error {
		if err := tx.TxEnsureCredit(ctx, 1, 100); err != nil {
			return err
		}
		if err := tx.ExecSavepoint(ctx, 0); err != nil {
			return err
		}
		task := &model.GenerationTask{
			UserID:            1,
			Kind:              model.KindVideo,
			Status:            model.TaskPending,
			IdempotencyKey:    "k-0",
			RequestIntentSig:  "sig-abc",
			RequestBatchIndex: 0,
			ConfigSnapshot:    []byte(`{}`),
			InputJSON:         []byte(`{}`),
		}
		inserted, err := tx.TxInsertTask(ctx, task)
		if err != nil {
			return err
		}
		if !inserted || task.ID == 0 || task.TaskRef == "" {
			return fmt.Errorf("TxInsertTask 未分配 ID/TaskRef：inserted=%v id=%d ref=%q", inserted, task.ID, task.TaskRef)
		}
		if err := tx.ReleaseSavepoint(ctx, 0); err != nil {
			return err
		}
		if err := tx.ExecSavepoint(ctx, 1); err != nil {
			return err
		}
		return tx.RollbackSavepoint(ctx, 1)
	})
	if err != nil {
		t.Fatalf("WithTx 执行失败：%v", err)
	}

	var savepointSQL []string
	for _, q := range rec.queries {
		upper := strings.ToUpper(q)
		if strings.Contains(upper, "SAVEPOINT") {
			savepointSQL = append(savepointSQL, q)
		}
	}
	want := []string{
		"SAVEPOINT sp_batch_0",
		"RELEASE SAVEPOINT sp_batch_0",
		"SAVEPOINT sp_batch_1",
		"ROLLBACK TO SAVEPOINT sp_batch_1",
	}
	if !slices.Equal(savepointSQL, want) {
		t.Fatalf("SAVEPOINT 轨迹存在非预期或隐式嵌套保存点：\ngot:  %v\nwant: %v", savepointSQL, want)
	}
}

// TestSavepointAndFlatTx_PropagateErrors 验证 SAVEPOINT / ROLLBACK / RELEASE 及扁平写入的错误绝不被吞掉。
func TestSavepointAndFlatTx_PropagateErrors(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		fail string
		fn   func(tx repository.GenerationTaskTx) error
	}{
		{"ExecSavepoint", "SAVEPOINT sp_batch_2", func(tx repository.GenerationTaskTx) error { return tx.ExecSavepoint(ctx, 2) }},
		{"RollbackSavepoint", "ROLLBACK TO SAVEPOINT sp_batch_2", func(tx repository.GenerationTaskTx) error { return tx.RollbackSavepoint(ctx, 2) }},
		{"ReleaseSavepoint", "RELEASE SAVEPOINT sp_batch_2", func(tx repository.GenerationTaskTx) error { return tx.ReleaseSavepoint(ctx, 2) }},
		{"TxEnsureCredit", `INSERT INTO "user_credits"`, func(tx repository.GenerationTaskTx) error { return tx.TxEnsureCredit(ctx, 1, 50) }},
		{"TxInsertTask_task", `INSERT INTO "generation_tasks"`, func(tx repository.GenerationTaskTx) error {
			_, err := tx.TxInsertTask(ctx, &model.GenerationTask{UserID: 1, Status: model.TaskPending, ConfigSnapshot: []byte(`{}`), InputJSON: []byte(`{}`)})
			return err
		}},
		{"TxInsertTask_event", `INSERT INTO "generation_task_events"`, func(tx repository.GenerationTaskTx) error {
			_, err := tx.TxInsertTask(ctx, &model.GenerationTask{UserID: 1, Status: model.TaskPending, ConfigSnapshot: []byte(`{}`), InputJSON: []byte(`{}`)})
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &sqlRecorder{failOnMatch: tc.fail}
			db := newRecordedGormDB(t, rec)
			repo := repository.NewGenerationTaskRepository(db)
			err := repo.WithTx(ctx, tc.fn)
			if err == nil || !strings.Contains(err.Error(), tc.fail) {
				t.Fatalf("期望错误透传 %q，实际 %v", tc.fail, err)
			}
		})
	}

	rec := &sqlRecorder{}
	db := newRecordedGormDB(t, rec)
	repo := repository.NewGenerationTaskRepository(db)
	if err := repo.ExecSavepoint(ctx, -1); err == nil {
		t.Fatal("负数 savepoint 下标应拒绝")
	}
}

// TestMarketingProjectUnscopedLock_SQL 验证软删恢复专用的双键加锁不过滤 deleted_at 且强制 user_id + id 双条件。
func TestMarketingProjectUnscopedLock_SQL(t *testing.T) {
	rec := &sqlRecorder{}
	db := newRecordedGormDB(t, rec)
	repo := repository.NewMarketingRepository(db)
	_, _ = repo.ProjectUnscopedLock(context.Background(), 42, 99)

	found := false
	for _, q := range rec.queries {
		if !strings.Contains(q, "marketing_projects") {
			continue
		}
		if strings.Contains(q, "deleted_at") {
			t.Fatalf("ProjectUnscopedLock 不得过滤 deleted_at：%s", q)
		}
		if !strings.Contains(q, "FOR UPDATE") || !strings.Contains(q, "user_id") {
			t.Fatalf("ProjectUnscopedLock 必须带 user_id 条件和 FOR UPDATE：%s", q)
		}
		found = true
	}
	if !found {
		t.Fatalf("未捕捉到 marketing_projects 查询：%v", rec.queries)
	}
}

// TestDramaRepository_SaveSegment6ColsAndReset 验证 SaveSegment 显式写全 6 列并在换片重置时将 source_out_f 写为 SQL NULL。
func TestDramaRepository_SaveSegment6ColsAndReset(t *testing.T) {
	db := isolatedDB(t, &model.CanvasProject{}, &model.DramaProject{}, &model.DramaEpisode{}, &model.DramaSegment{}, &model.DramaJob{})
	repo := repository.NewDramaRepository(db)
	ctx := context.Background()

	p := &model.DramaProject{UserID: 1, Title: "审讯", SourceText: "x", StyleKey: "real", Revision: 1}
	if err := repo.WithTx(ctx, func(tx repository.DramaTx) error {
		if err := tx.InsertProject(ctx, p); err != nil {
			return err
		}
		ep := &model.DramaEpisode{UserID: 1, DramaID: p.ID, No: 1, Revision: 1}
		if err := tx.SaveEpisode(ctx, ep, 0); err != nil {
			return err
		}
		outF := int32(43)
		seg := &model.DramaSegment{
			UserID: 1, EpisodeID: ep.ID, Seq: 1, Seconds: 5, Revision: 1,
			SourceInF: 24, SourceOutF: &outF, MuteOriginal: true,
			AudioGainDB: -6.5, NativeIsDialogue: true, AvPtsOffsetMs: -120,
		}
		if err := tx.SaveSegment(ctx, seg, 0); err != nil {
			return err
		}
		repository.ResetSegmentEdit(seg)
		return tx.SaveSegment(ctx, seg, seg.Revision)
	}); err != nil {
		t.Fatalf("事务失败：%v", err)
	}

	var rows []model.DramaSegment
	if err := db.Find(&rows).Error; err != nil || len(rows) != 1 {
		t.Fatalf("读取片段失败：%v len=%d", err, len(rows))
	}
	got := rows[0]
	if got.SourceInF != 0 || got.SourceOutF != nil || got.MuteOriginal || got.AudioGainDB != 0 || got.NativeIsDialogue || got.AvPtsOffsetMs != 0 {
		t.Fatalf("ResetSegmentEdit + SaveSegment 未将 6 列完整清零/写 NULL：%+v", got)
	}
}

type noopDramaEnqueuer struct{}

func (noopDramaEnqueuer) EnqueueDramaTx(context.Context, *sql.Tx, uint64, uint64, uint64) error {
	return nil
}

// TestDramaRepository_SaveEpisodeAndJobByComposeKey 验证 SaveEpisode 更新新列及 JobByComposeKey 唯一查找。
func TestDramaRepository_SaveEpisodeAndJobByComposeKey(t *testing.T) {
	db := isolatedDB(t, &model.CanvasProject{}, &model.DramaProject{}, &model.DramaEpisode{}, &model.DramaJob{})
	repo := repository.NewDramaRepository(db)
	repo.SetEnqueuer(noopDramaEnqueuer{})
	ctx := context.Background()

	p := &model.DramaProject{UserID: 1, Title: "短剧", SourceText: "x", StyleKey: "real", Revision: 1}
	var epID uint64
	key := "cmp-req-1"
	err := repo.WithTx(ctx, func(tx repository.DramaTx) error {
		if err := tx.InsertProject(ctx, p); err != nil {
			return err
		}
		ep := &model.DramaEpisode{UserID: 1, DramaID: p.ID, No: 1, Revision: 1}
		if err := tx.SaveEpisode(ctx, ep, 0); err != nil {
			return err
		}
		epID = ep.ID
		jobID := uint64(777)
		ep.EditSchema = 1
		ep.FinalComposeJobID = &jobID
		if err := tx.SaveEpisode(ctx, ep, 1); err != nil {
			return err
		}
		return tx.InsertJob(ctx, &model.DramaJob{
			UserID: 1, DramaID: p.ID, Step: "compose", Status: "queued",
			ComposeRequestKey: &key, RequestIntentSig: "intent-sha",
		})
	})
	if err != nil {
		t.Fatalf("写入失败：%v", err)
	}

	var ep model.DramaEpisode
	if err := db.First(&ep, epID).Error; err != nil {
		t.Fatal(err)
	}
	if ep.EditSchema != 1 || ep.FinalComposeJobID == nil || *ep.FinalComposeJobID != 777 || ep.Revision != 2 {
		t.Fatalf("SaveEpisode 未持久新列：%+v", ep)
	}

	got, err := repo.JobByComposeKey(ctx, 1, p.ID, key)
	if err != nil || got.RequestIntentSig != "intent-sha" {
		t.Fatalf("JobByComposeKey 失败：%+v err=%v", got, err)
	}
	if _, err := repo.JobByComposeKey(ctx, 2, p.ID, key); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("跨用户不应查到：%v", err)
	}
}

// TestAssetMediaProbeRepository_UpsertAndGet 验证探测结果按复合主键幂等写入和读取。
func TestAssetMediaProbeRepository_UpsertAndGet(t *testing.T) {
	db := isolatedDB(t, &model.AssetMediaProbe{})
	repo := repository.NewAssetMediaProbeRepository(db)
	ctx := context.Background()
	sha := strings.Repeat("b", 64)
	probe := &model.AssetMediaProbe{
		AssetID: 101, Sha256: sha, ProbeVersion: 1,
		FpsNum: 24, FpsDen: 1, NormalizedFrames24: 120,
		HasAudio: true, AudioSampleRate: 48000, DecodedAudioSamples: 240000,
		ProbedAt: time.Now().UTC(),
	}
	if err := repo.Upsert(ctx, probe); err != nil {
		t.Fatalf("首次 Upsert 失败：%v", err)
	}
	if err := repo.Upsert(ctx, probe); err != nil {
		t.Fatalf("幂等重复 Upsert 失败：%v", err)
	}
	got, err := repo.Get(ctx, 101, sha, 1)
	if err != nil || got.NormalizedFrames24 != 120 || got.DecodedAudioSamples != 240000 {
		t.Fatalf("读取探测结果不对：%+v err=%v", got, err)
	}
	if _, err := repo.Get(ctx, 101, sha, 2); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("不同 probe_version 应返回 ErrNotFound：%v", err)
	}
}

// TestMarketingRepository_UnscopedLockAndStepRunLease 验证软删双键加锁与执行租约抢占/续约。
func TestMarketingRepository_UnscopedLockAndStepRunLease(t *testing.T) {
	db := isolatedDB(t, &model.MarketingProject{}, &model.MarketingRound{}, &model.MarketingStepRun{})
	repo := repository.NewMarketingRepository(db)
	ctx := context.Background()

	proj := &model.MarketingProject{UserID: 7, Title: "测试", Revision: 1, ProductAssetIDs: []byte(`[]`)}
	round := &model.MarketingRound{UserID: 7, No: 1, Status: model.MarketingRoundScripting, ExecutionGen: 2, Settings: []byte(`{}`)}
	run := &model.MarketingStepRun{UserID: 7, StepKey: marketing.StepFacts, Status: model.MarketingRunQueued, RoundGen: 2, Method: []byte(`{}`)}

	if err := repo.WithTx(ctx, func(tx repository.MarketingTx) error {
		if err := tx.InsertProject(ctx, proj); err != nil {
			return err
		}
		round.ProjectID = proj.ID
		if err := tx.InsertRound(ctx, round); err != nil {
			return err
		}
		run.RoundID = round.ID
		return tx.InsertRun(ctx, run)
	}); err != nil {
		t.Fatal(err)
	}

	wrongGen, err := repo.ClaimStepRun(ctx, run.ID, round.ID, 1, "tok-a", time.Now().Add(time.Minute))
	if err != nil || wrongGen {
		t.Fatalf("陈旧 gen 不应抢占成功：claimed=%v err=%v", wrongGen, err)
	}
	claimed, err := repo.ClaimStepRun(ctx, run.ID, round.ID, 2, "tok-a", time.Now().Add(time.Minute))
	if err != nil || !claimed {
		t.Fatalf("首个 worker 应抢占成功：claimed=%v err=%v", claimed, err)
	}
	second, err := repo.ClaimStepRun(ctx, run.ID, round.ID, 2, "tok-b", time.Now().Add(time.Minute))
	if err != nil || second {
		t.Fatalf("第二个 worker 不应重复抢占：claimed=%v err=%v", second, err)
	}
	hbOK, err := repo.HeartbeatStepRun(ctx, run.ID, "tok-a", time.Now().Add(2*time.Minute))
	if err != nil || !hbOK {
		t.Fatalf("同 exec_token 续约应成功：ok=%v err=%v", hbOK, err)
	}
	hbWrong, err := repo.HeartbeatStepRun(ctx, run.ID, "tok-b", time.Now().Add(2*time.Minute))
	if err != nil || hbWrong {
		t.Fatalf("错误 exec_token 不得续约：ok=%v err=%v", hbWrong, err)
	}

	if err := repo.DeleteProject(ctx, 7, proj.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Project(ctx, 7, proj.ID, true); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("公开 Project 必须过滤已软删项目：%v", err)
	}
	if _, err := repo.ProjectUnscopedLock(ctx, 999, proj.ID); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("错 user_id 不得锁中已软删项目：%v", err)
	}
	if locked, err := repo.ProjectUnscopedLock(ctx, 7, proj.ID); err != nil || locked.ID != proj.ID {
		t.Fatalf("同 (user_id, project_id) 应锁回已软删项目：%+v err=%v", locked, err)
	}
}
