package repository_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/repository"
	"video-canvas/internal/service/dramasegmentfake"
)

func newFrameRunFixture(t *testing.T) (*gorm.DB, *repository.DramaRepository, model.DramaProject, model.DramaEpisode, model.DramaSegment) {
	t.Helper()
	db := isolatedDB(t, &model.CanvasProject{}, &model.DramaProject{}, &model.DramaEpisode{},
		&model.DramaSegment{}, &model.GenerationTask{}, &model.DramaSegmentFrameRun{}, &model.DramaSegmentTakeLink{})
	r := repository.NewDramaRepository(db)
	p := model.DramaProject{UserID: 1, Title: "控镜测试", SourceText: "文", StyleKey: "real"}
	var ep model.DramaEpisode
	var seg model.DramaSegment
	ctx := context.Background()
	if err := r.WithTx(ctx, func(tx repository.DramaTx) error {
		if err := tx.InsertProject(ctx, &p); err != nil {
			return err
		}
		ep = model.DramaEpisode{UserID: 1, DramaID: p.ID, No: 1}
		if err := tx.SaveEpisode(ctx, &ep, 0); err != nil {
			return err
		}
		seg = model.DramaSegment{UserID: 1, EpisodeID: ep.ID, Prompt: "原提示词"}
		return tx.SaveSegment(ctx, &seg, 0)
	}); err != nil {
		t.Fatal(err)
	}
	return db, r, p, ep, seg
}

func TestFrameRunRepositoryConcurrentCAS(t *testing.T) {
	_, r, _, _, seg := newFrameRunFixture(t)
	ctx := context.Background()
	run := frameRunRow(seg.ID, "concurrent")
	if err := r.CreateFrameRun(ctx, &run); err != nil {
		t.Fatal(err)
	}
	start, results := make(chan struct{}), make(chan error, 2)
	for range 2 {
		go func() {
			row := run
			row.State = model.FrameRunStateFrameReady
			<-start
			results <- r.SaveFrameRun(ctx, &row, 1)
		}()
	}
	close(start)
	succeeded, conflicted := 0, 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, repository.ErrStateConflict):
			conflicted++
		default:
			t.Fatalf("非预期 CAS 错误：%v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("CAS 应只有一个胜者：成功=%d 冲突=%d", succeeded, conflicted)
	}
}

func TestFrameRunRepositoryHasSegmentTakes(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		state                  model.DramaFrameRunState
		task, native, dangling bool
		foreign                string
		imported               bool
		want                   bool
	}{
		{name: "空片段"},
		{name: "导入候选", imported: true, want: true},
		{name: "悬空导入孤儿", imported: true, dangling: true},
		{name: "活跃流程", state: model.FrameRunStateDraft, want: true},
		{name: "已绑定候选", state: model.FrameRunStateBound, want: true},
		{name: "已取消孤儿", state: model.FrameRunStateCancelled, task: true},
		{name: "已失败孤儿", state: model.FrameRunStateFailed, task: true},
		{name: "缺失流程孤儿", task: true},
		{name: "跨用户孤儿", state: model.FrameRunStateVideoPending, task: true, foreign: "user"},
		{name: "跨片段孤儿", state: model.FrameRunStateVideoPending, task: true, foreign: "segment"},
		{name: "原生失败任务仍守卫", task: true, native: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, r, p, ep, seg := newFrameRunFixture(t)
			ctx := context.Background()
			run := frameRunRow(seg.ID, "guard")
			run.ID, run.State = 900, tc.state
			if tc.foreign == "user" {
				run.UserID = 2
			}
			if tc.foreign == "segment" {
				run.SegmentID += 100
			}
			var videoID uint64 = 101
			if tc.state == model.FrameRunStateBound {
				run.VideoTaskID, run.ConfirmedVersion = &videoID, 1
				runID, rev := run.ID, run.ConfirmedVersion
				boundTask := model.GenerationTask{ID: videoID, UserID: 1, CanvasProjectID: &p.CanvasID, NodeID: "seg:" + idcodec.Encode(seg.ID),
					Kind: model.KindVideo, ModelKey: "video", Provider: "fake", Status: model.TaskSucceeded, FrameRunID: &runID, FrameRunConfirmRev: &rev,
					InputJSON: []byte(`{}`), ConfigSnapshot: []byte(`{}`), NextPollAt: time.Now(), DeadlineAt: time.Now()}
				if err := db.Create(&boundTask).Error; err != nil {
					t.Fatal(err)
				}
			}
			if tc.state != "" {
				if err := r.CreateFrameRun(ctx, &run); err != nil {
					t.Fatal(err)
				}
			}
			if tc.task {
				task := model.GenerationTask{UserID: 1, CanvasProjectID: &p.CanvasID, NodeID: "seg:" + idcodec.Encode(seg.ID),
					Kind: model.KindVideo, ModelKey: "video", Provider: "fake", Status: model.TaskFailed,
					InputJSON: []byte(`{}`), ConfigSnapshot: []byte(`{}`), NextPollAt: time.Now(), DeadlineAt: time.Now()}
				if !tc.native {
					task.FrameRunID = &run.ID
				}
				if err := db.Create(&task).Error; err != nil {
					t.Fatal(err)
				}
			}
			if tc.imported {
				if !tc.dangling {
					canvas19 := uint64(19)
					importTask := model.GenerationTask{ID: 101, UserID: 1, CanvasProjectID: &canvas19, NodeID: "canvas-video",
						Kind: model.KindVideo, ModelKey: "video", Provider: "fake", Status: model.TaskSucceeded,
						InputJSON: []byte(`{}`), ConfigSnapshot: []byte(`{}`), NextPollAt: time.Now(), DeadlineAt: time.Now()}
					if err := db.Create(&importTask).Error; err != nil {
						t.Fatal(err)
					}
				}
				link := model.DramaSegmentTakeLink{UserID: 1, SegmentID: seg.ID, TaskID: 101, SourceCanvasID: 19, AssetID: 201, Origin: model.TakeOriginCanvasImport}
				if _, err := r.CreateTakeLinkIfAbsent(ctx, &link); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := r.HasSegmentTakes(ctx, 1, ep.ID); err != nil || got != tc.want {
				t.Fatalf("HasSegmentTakes=%v / %v，期望 %v", got, err, tc.want)
			}
		})
	}
}

func TestFrameRunRepositoryDeletionCancelsActive(t *testing.T) {
	for _, operation := range []string{"segment", "replace", "project"} {
		t.Run(operation, func(t *testing.T) {
			_, r, p, ep, seg := newFrameRunFixture(t)
			ctx := context.Background()
			run, terminal, other := frameRunRow(seg.ID, "delete-active"), frameRunRow(seg.ID, "delete-bound"), frameRunRow(seg.ID+100, "other")
			run.State, terminal.State, other.UserID = model.FrameRunStateVideoPending, model.FrameRunStateBound, 2
			for _, row := range []*model.DramaSegmentFrameRun{&run, &terminal, &other} {
				if err := r.CreateFrameRun(ctx, row); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			switch operation {
			case "segment":
				_, err = r.DeleteSegmentNoBump(ctx, 1, seg.ID)
			case "replace":
				err = r.ReplaceSegments(ctx, 1, ep.ID, []model.DramaSegment{{Prompt: "新片段"}})
			case "project":
				err = r.DeleteProject(ctx, 1, p.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := r.FrameRun(ctx, 1, run.ID, false)
			if err != nil || got.State != model.FrameRunStateCancelled || got.Revision != 2 || got.UpdatedAt.Before(run.UpdatedAt) {
				t.Fatalf("删除应停用活跃流程并 bump：%+v / %v", got, err)
			}
			for _, unchanged := range []*model.DramaSegmentFrameRun{&terminal, &other} {
				got, err := r.FrameRun(ctx, unchanged.UserID, unchanged.ID, false)
				if err != nil || got.State != unchanged.State || got.Revision != unchanged.Revision {
					t.Fatalf("终态或他人流程不应改动：%+v / %v", got, err)
				}
			}
			rows, err := r.ListRecoverableFrameRuns(ctx, repository.FrameRunRecoveryFilter{UserID: 1}, 10)
			if err != nil || len(rows) != 0 {
				t.Fatalf("删除后不应继续恢复：%+v / %v", rows, err)
			}
		})
	}
}

func TestFrameRunRepositoryReplacementRollback(t *testing.T) {
	_, r, _, ep, seg := newFrameRunFixture(t)
	ctx := context.Background()
	run := frameRunRow(seg.ID, "replace-rollback")
	if err := r.CreateFrameRun(ctx, &run); err != nil {
		t.Fatal(err)
	}
	// 替换插入失败必须同时回滚旧片段软删和 run 取消，独立调用也应原子执行。
	if err := r.ReplaceSegments(ctx, 1, ep.ID, []model.DramaSegment{{Adaptations: []byte(`invalid-json`)}}); err == nil {
		t.Fatal("非法 JSON 应使替换失败")
	}
	if _, err := r.Segment(ctx, 1, seg.ID, false); err != nil {
		t.Fatalf("失败替换不应软删旧片段：%v", err)
	}
	got, err := r.FrameRun(ctx, 1, run.ID, false)
	if err != nil || got.State != model.FrameRunStateDraft || got.Revision != 1 {
		t.Fatalf("失败替换不应取消旧流程：%+v / %v", got, err)
	}
}

func TestFrameRunRepositorySaveChosenCleared(t *testing.T) {
	_, r, _, ep, seg := newFrameRunFixture(t)
	ctx := context.Background()
	before, err := r.SegmentEpisode(ctx, 1, ep.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []bool{true, false} {
		seg.ChosenCleared = value
		if changed, err := r.SaveSegmentNoBump(ctx, &seg, seg.Revision); err != nil || !changed {
			t.Fatalf("显式写入 %v：changed=%v / %v", value, changed, err)
		}
		got, err := r.Segment(ctx, 1, seg.ID, false)
		if err != nil || got.ChosenCleared != value {
			t.Fatalf("chosen_cleared 未持久化：%+v / %v", got, err)
		}
	}
	after, err := r.SegmentEpisode(ctx, 1, ep.ID)
	if err != nil || before.SegmentVersion != after.SegmentVersion {
		t.Fatalf("NoBump 不应递增集版本：%+v / %v", after, err)
	}
}

func TestFrameRunFakeDeepRollback(t *testing.T) {
	r := dramasegmentfake.New()
	ctx := context.Background()
	run := frameRunRow(4, "fake-rollback")
	run.FrameAssetID, run.ConfirmedInput = ptrUint64(201), []byte(`{"value":1}`)
	if err := r.CreateFrameRun(ctx, &run); err != nil {
		t.Fatal(err)
	}
	link := model.DramaSegmentTakeLink{UserID: 1, SegmentID: 4, TaskID: 101, SourceCanvasID: 9, AssetID: 201, Origin: model.TakeOriginI2V, RunID: &run.ID}
	if _, err := r.CreateTakeLinkIfAbsent(ctx, &link); err != nil {
		t.Fatal(err)
	}
	abort := errors.New("rollback")
	if err := r.WithTx(ctx, func(tx repository.DramaTx) error {
		*r.FrameRuns[0].FrameAssetID, *r.TakeLinks[0].RunID = 999, 999
		r.FrameRuns[0].ConfirmedInput[9] = '9'
		if err := tx.CancelSegmentFrameRuns(ctx, 1, 4); err != nil {
			return err
		}
		if err := tx.UpdateTakeLinkQC(ctx, 1, 4, 101, model.TakeQCRejected); err != nil {
			return err
		}
		newRun := frameRunRow(5, "rolled-back-create")
		if err := tx.CreateFrameRun(ctx, &newRun); err != nil {
			return err
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatal(err)
	}
	got, err := r.FrameRunByRequestKey(ctx, 1, "fake-rollback")
	if err != nil || len(r.FrameRuns) != 1 || got.State != model.FrameRunStateDraft || got.Revision != 1 || *got.FrameAssetID != 201 || string(got.ConfirmedInput) != `{"value":1}` {
		t.Fatalf("假仓储未深拷贝恢复流程：%+v / %v", got, err)
	}
	take, err := r.TakeLinkByRun(ctx, 1, run.ID)
	if err != nil || take.QC != model.TakeQCUnreviewed || *take.RunID != run.ID {
		t.Fatalf("假仓储未恢复候选：%+v / %v", take, err)
	}
}

func frameRunRow(segmentID uint64, key string) model.DramaSegmentFrameRun {
	return model.DramaSegmentFrameRun{UserID: 1, SegmentID: segmentID, RequestKey: key,
		RequestHash: strings.Repeat("a", 64), FrameSource: model.FrameSourceExisting,
		State: model.FrameRunStateDraft, SegmentRevision: 1}
}

func TestFrameRunRepositoryCASAndUniqueness(t *testing.T) {
	_, r, _, _, seg := newFrameRunFixture(t)
	ctx := context.Background()
	run := frameRunRow(seg.ID, "req-1")
	if err := r.WithTx(ctx, func(tx repository.DramaTx) error { return tx.CreateFrameRun(ctx, &run) }); err != nil {
		t.Fatal(err)
	}
	stale := run
	run.State, run.FrameAssetID = model.FrameRunStateFrameReady, ptrUint64(201)
	if err := r.SaveFrameRun(ctx, &run, 1); err != nil || run.Revision != 2 {
		t.Fatalf("CAS 保存失败：%+v / %v", run, err)
	}
	stale.State = model.FrameRunStateFailed
	if err := r.SaveFrameRun(ctx, &stale, 1); !errors.Is(err, repository.ErrStateConflict) {
		t.Fatalf("旧版本应冲突：%v", err)
	}
	duplicate := frameRunRow(seg.ID, "req-2")
	if err := r.CreateFrameRun(ctx, &duplicate); !errors.Is(err, repository.ErrDuplicate) {
		t.Fatalf("同段第二个活跃 run 应冲突：%v", err)
	}
	run.State = model.FrameRunStateCancelled
	if err := r.SaveFrameRun(ctx, &run, 2); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateFrameRun(ctx, &duplicate); err != nil {
		t.Fatalf("取消后应释放槽位：%v", err)
	}
	sameKey := frameRunRow(seg.ID+100, "req-1")
	if err := r.CreateFrameRun(ctx, &sameKey); !errors.Is(err, repository.ErrDuplicate) {
		t.Fatalf("用户请求键应全局唯一：%v", err)
	}
	if _, err := r.FrameRun(ctx, 2, run.ID, false); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("跨用户读取应不存在：%v", err)
	}
	if active, err := r.ActiveFrameRun(ctx, 1, seg.ID, false); err != nil || active.ID != duplicate.ID {
		t.Fatalf("活跃流程查询错误：%+v / %v", active, err)
	}
}

func ptrUint64(v uint64) *uint64 { return &v }

func TestTakeLinkRepositoryPreservesOriginAndQC(t *testing.T) {
	_, r, _, _, seg := newFrameRunFixture(t)
	ctx := context.Background()
	link := model.DramaSegmentTakeLink{UserID: 1, SegmentID: seg.ID, TaskID: 101,
		SourceCanvasID: 9, AssetID: 201, Origin: model.TakeOriginI2V, QC: model.TakeQCRejected, RunID: ptrUint64(31)}
	if inserted, err := r.CreateTakeLinkIfAbsent(ctx, &link); err != nil || !inserted {
		t.Fatalf("首次补缺写入：%v / %v", inserted, err)
	}
	imported := link
	imported.Origin, imported.QC, imported.AssetID, imported.RunID = model.TakeOriginCanvasImport, model.TakeQCUnreviewed, 999, nil
	if inserted, err := r.CreateTakeLinkIfAbsent(ctx, &imported); err != nil || inserted {
		t.Fatalf("重导应保留原行：%v / %v", inserted, err)
	}
	got, err := r.TakeLink(ctx, 1, seg.ID, 101)
	if err != nil || got.Origin != model.TakeOriginI2V || got.QC != model.TakeQCRejected || got.AssetID != 201 || got.RunID == nil {
		t.Fatalf("重导覆盖了既有候选：%+v / %v", got, err)
	}
	if err := r.UpdateTakeLinkQC(ctx, 1, seg.ID, 101, model.TakeQCAccepted); err != nil {
		t.Fatal(err)
	}
	got, err = r.TakeLinkByRun(ctx, 1, 31)
	if err != nil || got.QC != model.TakeQCAccepted || got.Origin != model.TakeOriginI2V {
		t.Fatalf("质检更新丢失来源：%+v / %v", got, err)
	}
	if err := r.UpdateTakeLinkQC(ctx, 2, seg.ID, 101, model.TakeQCUnreviewed); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("质检必须限定用户：%v", err)
	}
	other := link
	other.TaskID = 102
	if _, err := r.CreateTakeLinkIfAbsent(ctx, &other); !errors.Is(err, repository.ErrDuplicate) {
		t.Fatalf("其他候选重复 run_id 不应被 DO NOTHING 吞掉：%v", err)
	}
	if rows, err := r.ListTakeLinks(ctx, 1, seg.ID); err != nil || len(rows) != 1 {
		t.Fatalf("候选列表：%+v / %v", rows, err)
	}
}

func TestFrameRunRepositoryRollback(t *testing.T) {
	_, r, _, _, seg := newFrameRunFixture(t)
	ctx := context.Background()
	abort := errors.New("回滚")
	run := frameRunRow(seg.ID, "rollback")
	if err := r.WithTx(ctx, func(tx repository.DramaTx) error {
		if err := tx.CreateFrameRun(ctx, &run); err != nil {
			return err
		}
		link := model.DramaSegmentTakeLink{UserID: 1, SegmentID: seg.ID, TaskID: 101, SourceCanvasID: 9, AssetID: 201, Origin: model.TakeOriginI2V, RunID: &run.ID}
		if _, err := tx.CreateTakeLinkIfAbsent(ctx, &link); err != nil {
			return err
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatalf("事务应返回回滚原因：%v", err)
	}
	if _, err := r.FrameRunByRequestKey(ctx, 1, "rollback"); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("回滚不应留下 run：%v", err)
	}
	if rows, err := r.ListTakeLinks(ctx, 1, seg.ID); err != nil || len(rows) != 0 {
		t.Fatalf("回滚不应留下候选：%+v / %v", rows, err)
	}
}

func TestFrameRunRepositoryLookupsAndRecovery(t *testing.T) {
	_, r, _, _, seg := newFrameRunFixture(t)
	ctx := context.Background()
	run := frameRunRow(seg.ID, "lookup")
	run.State, run.FrameTaskID, run.VideoTaskID = model.FrameRunStateVideoPending, ptrUint64(11), ptrUint64(12)
	run.VideoIdempotencyKey = "video-idem"
	if err := r.CreateFrameRun(ctx, &run); err != nil {
		t.Fatal(err)
	}
	for name, lookup := range map[string]func(uint64) (*model.DramaSegmentFrameRun, error){
		"id": func(user uint64) (*model.DramaSegmentFrameRun, error) { return r.FrameRun(ctx, user, run.ID, false) },
		"request": func(user uint64) (*model.DramaSegmentFrameRun, error) {
			return r.FrameRunByRequestKey(ctx, user, "lookup")
		},
		"frame": func(user uint64) (*model.DramaSegmentFrameRun, error) { return r.FrameRunByFrameTask(ctx, user, 11) },
		"video": func(user uint64) (*model.DramaSegmentFrameRun, error) { return r.FrameRunByVideoTask(ctx, user, 12) },
		"idem": func(user uint64) (*model.DramaSegmentFrameRun, error) {
			return r.FrameRunByVideoIdempotencyKey(ctx, user, "video-idem")
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := lookup(1); err != nil || got.ID != run.ID {
				t.Fatalf("查询错误：%+v / %v", got, err)
			}
			if _, err := lookup(2); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("跨用户查询应不存在：%v", err)
			}
		})
	}
	for i, state := range []model.DramaFrameRunState{model.FrameRunStateFramePending, model.FrameRunStateSubmitting, model.FrameRunStateDraft, model.FrameRunStateCancelled} {
		row := frameRunRow(seg.ID+uint64(i)+1, "recover-"+string(rune('a'+i)))
		row.State = state
		if err := r.CreateFrameRun(ctx, &row); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := r.ListRecoverableFrameRuns(ctx, repository.FrameRunRecoveryFilter{UserID: 1}, 1)
	if err != nil || len(rows) != 1 || rows[0].ID != run.ID {
		t.Fatalf("恢复扫描应限量且按时间排序：%+v / %v", rows, err)
	}
	rows, err = r.ListRecoverableFrameRuns(ctx, repository.FrameRunRecoveryFilter{UserID: 1}, 0)
	if err != nil || len(rows) != 3 {
		t.Fatalf("仅扫描三个可恢复状态：%+v / %v", rows, err)
	}
	rows, err = r.ListRecoverableFrameRuns(ctx, repository.FrameRunRecoveryFilter{UserID: 2}, 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("恢复扫描用户过滤：%+v / %v", rows, err)
	}
	rows, err = r.ListRecoverableFrameRuns(ctx, repository.FrameRunRecoveryFilter{UpdatedBefore: time.Unix(1, 0)}, 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("恢复扫描时间过滤：%+v / %v", rows, err)
	}
}
