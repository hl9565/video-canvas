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
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/provider"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
	"video-canvas/internal/service/dramasegmentfake"
)

func TestFrameRunCandidatesCancelledNeverNative(t *testing.T) {
	r := dramasegmentfake.New()
	r.Rows = []model.DramaSegment{{BaseModel: model.BaseModel{ID: 1}, UserID: 1, EpisodeID: 10, Revision: 1}}
	canvas, taskID, runID, rev := uint64(9), uint64(21), uint64(31), int64(1)
	r.Tasks = []model.GenerationTask{{ID: taskID, UserID: 1, CanvasProjectID: &canvas, NodeID: "seg:" + idcodec.Encode(1), Kind: model.KindVideo, Status: model.TaskSucceeded, FrameRunID: &runID, FrameRunConfirmRev: &rev, OutputJSON: []byte(`[{"asset_id":41,"media_type":"video"}]`)}}
	r.FrameRuns = []model.DramaSegmentFrameRun{{ID: runID, UserID: 1, SegmentID: 1, State: model.FrameRunStateCancelled, VideoTaskID: &taskID, ConfirmedVersion: rev}}
	s := segmentService(r)
	if err := s.OnTaskChanged(context.Background(), &r.Tasks[0]); err != nil {
		t.Fatal(err)
	}
	views, err := s.List(context.Background(), 1, 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || len(views[0].Takes) != 0 || views[0].ChosenTaskID != "" || views[0].Revision != 1 || len(r.TakeLinks) != 0 {
		t.Fatalf("取消的流程不得回落为原生候选: %+v", views)
	}
}

// 原有纯内存短剧夹具没有流程/候选；显式空实现避免新接口通过 nil 嵌入误调用。
func (r *dramaMemory) FrameRunsBySegment(context.Context, uint64, uint64) ([]model.DramaSegmentFrameRun, error) {
	return nil, nil
}
func (r *dramaMemory) ActiveFrameRunsBySegments(context.Context, uint64, []uint64) ([]model.DramaSegmentFrameRun, error) {
	return nil, nil
}
func (r *dramaMemory) ListTakeLinks(context.Context, uint64, uint64) ([]model.DramaSegmentTakeLink, error) {
	return nil, nil
}
func (r *dramaMemory) TasksByIDs(context.Context, uint64, []uint64) ([]model.GenerationTask, error) {
	return nil, nil
}

type takeCanvases struct {
	rows map[uint64]*model.CanvasProject
}

func (c takeCanvases) GetByID(_ context.Context, user, id uint64) (*model.CanvasProject, error) {
	row := c.rows[id]
	if row == nil || row.UserID != user || row.DeletedAt.Valid {
		return nil, repository.ErrNotFound
	}
	return row, nil
}

func TestDramaTakesImportQCAndClear(t *testing.T) {
	r := dramasegmentfake.New()
	r.Rows = []model.DramaSegment{{BaseModel: model.BaseModel{ID: 1}, UserID: 1, EpisodeID: 10, Revision: 1}}
	canvas := uint64(11)
	r.Tasks = []model.GenerationTask{{ID: 21, UserID: 1, CanvasProjectID: &canvas, NodeID: "video-node", Kind: model.KindVideo, Status: model.TaskSucceeded, OutputJSON: []byte(`[{"asset_id":41,"media_type":"video"}]`)}}
	s := service.NewDramaSegmentService(service.DramaSegmentDeps{Repo: r, Assets: r, Canvases: takeCanvases{map[uint64]*model.CanvasProject{11: {BaseModel: model.BaseModel{ID: 11}, UserID: 1}}}})
	ctx := context.Background()
	req := &model.ImportDramaTakesReq{Revision: 1, SourceCanvasID: 11, TaskID: 21}
	v, err := s.ImportTakes(ctx, 1, 1, req)
	if err != nil {
		t.Fatal(err)
	}
	if v.Revision != 1 || v.SegmentVersion != 0 || v.ChosenTaskID != "" {
		t.Fatalf("非采用导入不得改版本: %+v", v)
	}
	v, err = s.PatchTakeQC(ctx, 1, 1, 21, &model.PatchDramaTakeQCReq{Revision: 1, QC: model.TakeQCRejected})
	if err != nil {
		t.Fatal(err)
	}
	if v.Revision != 2 || v.SegmentVersion != 0 {
		t.Fatalf("纯 QC 只递增段版本: %+v", v)
	}
	req.Revision, req.Adopt = 2, true
	_, err = s.ImportTakes(ctx, 1, 1, req)
	segmentCode(t, err, errcode.ErrDramaTakeQCRejected.Code)
	if r.TakeLinks[0].QC != model.TakeQCRejected || r.TakeLinks[0].Origin != model.TakeOriginCanvasImport {
		t.Fatal("重导覆盖了来源或 QC")
	}
}

type takeMatrixCase struct {
	binding, entry                int
	adopt, chosen, cleared, drift bool
}

func takeMatrixCases() []takeMatrixCase {
	cases := make([]takeMatrixCase, 256)
	for i := range 256 {
		cases[i] = takeMatrixCase{binding: i % 4, entry: i / 4 % 4, adopt: i&16 != 0, chosen: i&32 != 0, cleared: i&64 != 0, drift: i&128 != 0}
	}
	return cases
}

func (c takeMatrixCase) name() string {
	return fmt.Sprintf("binding%d/entry%d/adopt%v/chosen%v/cleared%v/drift%v", c.binding, c.entry, c.adopt, c.chosen, c.cleared, c.drift)
}

func matrixTakeRepo(c takeMatrixCase) *dramasegmentfake.Repo {
	r := dramasegmentfake.New()
	r.SegmentVersion = 17
	r.Rows = []model.DramaSegment{{BaseModel: model.BaseModel{ID: 1}, UserID: 1, EpisodeID: 10, Seconds: 5, Revision: 2, ChosenCleared: c.cleared}}
	canvas, tid, rid, rev := uint64(9), uint64(21), uint64(31), int64(2)
	task := model.GenerationTask{ID: tid, UserID: 1, CanvasProjectID: &canvas, NodeID: "seg:" + idcodec.Encode(1), Kind: model.KindVideo, ModelKey: "m1", Provider: "demo", Status: model.TaskSucceeded, FrameRunID: &rid, FrameRunConfirmRev: &rev, InputJSON: []byte(`{}`), ConfigSnapshot: []byte(`{}`), OutputJSON: []byte(`[{"asset_id":41,"media_type":"video"}]`), CreatedAt: time.Unix(2, 0)}
	run := model.DramaSegmentFrameRun{ID: rid, UserID: 1, SegmentID: 1, State: model.FrameRunStateVideoPending, VideoTaskID: &tid, ConfirmedVersion: rev, SegmentRevision: 2, Revision: 1, AdoptIfEmpty: c.adopt, RequestKey: "matrix", RequestHash: strings.Repeat("a", 64), FrameSource: model.FrameSourceExisting}
	switch c.binding {
	case 1:
		run.VideoTaskID = nil
	case 2:
		wrong := int64(1)
		task.FrameRunConfirmRev = &wrong
	case 3:
		run.State = model.FrameRunStateCancelled
	}
	if c.drift {
		run.SegmentRevision = 1
	}
	r.Tasks = []model.GenerationTask{task}
	r.FrameRuns = []model.DramaSegmentFrameRun{run}
	if c.chosen {
		id := uint64(20)
		r.Rows[0].ChosenTaskID = &id
		old := task
		old.ID, old.FrameRunID, old.FrameRunConfirmRev, old.CreatedAt = 20, nil, nil, time.Unix(1, 0)
		r.Tasks = append(r.Tasks, old)
	}
	return r
}

func triggerTakeEntry(t *testing.T, s *service.DramaSegmentService, task *model.GenerationTask, entry int) {
	t.Helper()
	var err error
	switch entry {
	case 0:
		err = s.OnTaskChanged(context.Background(), task)
	case 1:
		_, err = s.Get(context.Background(), 1, 1)
	case 2:
		_, err = s.List(context.Background(), 1, 7, 1)
	case 3:
		err = s.RecoverFrameRuns(context.Background(), repository.FrameRunRecoveryFilter{UserID: 1}, 10)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func assertTakeMatrix(t *testing.T, c takeMatrixCase, seg model.DramaSegment, epVersion int64, run model.DramaSegmentFrameRun, links int, takesUsable bool) {
	t.Helper()
	valid := c.binding == 0
	automatic := valid && c.adopt && !c.chosen && !c.cleared && !c.drift
	wantChosen := uint64(0)
	if c.chosen {
		wantChosen = 20
	}
	if automatic {
		wantChosen = 21
	}
	chosen := uint64(0)
	if seg.ChosenTaskID != nil {
		chosen = *seg.ChosenTaskID
	}
	wantRev, wantVersion := int64(2), int64(17)
	if automatic {
		wantRev++
		wantVersion++
	}
	wantLinks := 0
	wantState := model.FrameRunStateVideoPending
	if valid {
		wantLinks = 1
		wantState = model.FrameRunStateBound
	}
	if c.binding == 3 {
		wantState = model.FrameRunStateCancelled
	}
	if chosen != wantChosen || seg.ChosenCleared != c.cleared || seg.Revision != wantRev || epVersion != wantVersion || links != wantLinks || run.State != wantState || takesUsable != valid {
		t.Fatalf("自动采用=%v 有效候选=%v: seg=%+v epVersion=%d run=%+v links=%d usable=%v", automatic, valid, seg, epVersion, run, links, takesUsable)
	}
}

func TestDramaTakesCandidateMatrix256(t *testing.T) {
	for _, c := range takeMatrixCases() {
		t.Run(c.name(), func(t *testing.T) {
			r := matrixTakeRepo(c)
			s := segmentService(r)
			triggerTakeEntry(t, s, &r.Tasks[0], c.entry)
			p, _ := r.Project(context.Background(), 1, 7, false)
			takes, err := service.ResolveSegmentTakes(context.Background(), r, r, p, &r.Rows[0])
			if err != nil {
				t.Fatal(err)
			}
			usable := false
			for _, take := range takes {
				if take.Task.ID == 21 {
					if !take.IsFrameRun || take.Origin != model.TakeOriginI2V {
						t.Fatal("I2V 回落 native")
					}
					usable = take.Usable
				}
			}
			assertTakeMatrix(t, c, r.Rows[0], r.SegmentVersion, r.FrameRuns[0], len(r.TakeLinks), usable)
		})
	}
}

// TestDramaTakesPG16CandidateMatrix256 在真实 PG16 事务/行锁/唯一索引上重复完整准入矩阵。
func TestDramaTakesPG16CandidateMatrix256(t *testing.T) {
	now := time.Now()
	db, _, _, _, _, _ := segEditPGEnv(t, &now)
	assertSlice2PG16(t, db)
	for _, c := range takeMatrixCases() {
		t.Run(c.name(), func(t *testing.T) {
			r := matrixTakeRepo(c)
			tx, repo, p := seedMatrixTakePG(t, db, r)
			assets := pgTakeAssets{db: tx}
			s := service.NewDramaSegmentService(service.DramaSegmentDeps{Repo: repo, Assets: assets})
			triggerTakeEntry(t, s, &r.Tasks[0], c.entry)
			seg, err := repo.Segment(context.Background(), 1, 1, false)
			if err != nil {
				t.Fatal(err)
			}
			storedEP, err := repo.SegmentEpisode(context.Background(), 1, 10)
			if err != nil {
				t.Fatal(err)
			}
			run, err := repo.FrameRun(context.Background(), 1, 31, false)
			if err != nil {
				t.Fatal(err)
			}
			links, err := repo.ListTakeLinks(context.Background(), 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			takes, err := service.ResolveSegmentTakes(context.Background(), repo, assets, p, seg)
			if err != nil {
				t.Fatal(err)
			}
			usable := false
			for _, take := range takes {
				if take.Task.ID == 21 {
					if !take.IsFrameRun {
						t.Fatal("流程回落 native")
					}
					usable = take.Usable
				}
			}
			assertTakeMatrix(t, c, *seg, storedEP.SegmentVersion, *run, len(links), usable)
		})
	}
}

type unavailableTakeAssets struct{ err error }

func (a unavailableTakeAssets) View(context.Context, uint64, uint64) (*model.AssetView, error) {
	return nil, a.err
}

func TestDramaTakesNegativeFourEntries(t *testing.T) {
	for _, entry := range []int{0, 1, 2, 3} {
		for _, reason := range []string{"cancelled", "failed", "missing-forward-pending", "missing-forward-bound", "deleted-asset"} {
			t.Run(fmt.Sprintf("%s/entry%d", reason, entry), func(t *testing.T) {
				r := matrixTakeRepo(takeMatrixCase{adopt: true})
				switch reason {
				case "cancelled":
					r.FrameRuns[0].State = model.FrameRunStateCancelled
				case "failed":
					r.FrameRuns[0].State = model.FrameRunStateFailed
				case "missing-forward-pending":
					r.FrameRuns[0].VideoTaskID = nil
				case "missing-forward-bound":
					r.FrameRuns[0].VideoTaskID = nil
					r.FrameRuns[0].State = model.FrameRunStateBound
				}
				assets := service.DramaSegmentAssetReader(r)
				if reason == "deleted-asset" {
					assets = unavailableTakeAssets{}
				}
				s := service.NewDramaSegmentService(service.DramaSegmentDeps{Repo: r, Assets: assets})
				triggerTakeEntry(t, s, &r.Tasks[0], entry)
				p, _ := r.Project(context.Background(), 1, 7, false)
				takes, err := service.ResolveSegmentTakes(context.Background(), r, assets, p, &r.Rows[0])
				if err != nil {
					t.Fatal(err)
				}
				for _, take := range takes {
					if take.Usable || !take.IsFrameRun {
						t.Fatal("失格流程变成有效或原生候选")
					}
				}
				if len(r.TakeLinks) != 0 || r.Rows[0].ChosenTaskID != nil || r.Rows[0].Revision != 2 || r.SegmentVersion != 17 {
					t.Fatalf("失格流程产生副作用: %+v", r.Rows[0])
				}
			})
		}
	}
}

func (e *frameBindEnv) importedTask(t *testing.T, user uint64, kind string, isTest bool) (*model.CanvasProject, *model.GenerationTask) {
	t.Helper()
	canvas := &model.CanvasProject{UserID: user, Title: "source", PayloadJSON: []byte(`{}`)}
	mustTakeSQL(t, e.db.Create(canvas))
	asset := &model.Asset{UserID: user, Kind: kind, StorageKey: fmt.Sprintf("import-%d.mp4", canvas.ID), Sha256: strings.Repeat("b", 64)}
	mustTakeSQL(t, e.db.Create(asset))
	e.assets.assets[asset.ID] = asset
	output, _ := json.Marshal([]model.TaskOutput{{AssetID: asset.ID, MediaType: kind}})
	task := &model.GenerationTask{UserID: user, CanvasProjectID: &canvas.ID, NodeID: "canvas-video", Kind: kind, ModelKey: "m1", Provider: "demo", Status: model.TaskSucceeded, IsTest: isTest, InputJSON: []byte(`{}`), ConfigSnapshot: []byte(`{}`), OutputJSON: output}
	mustTakeSQL(t, e.db.Create(task))
	return canvas, task
}

func TestDramaTakesPG16ImportValidationAndReimport(t *testing.T) {
	e := newFrameBindPG(t)
	ctx := context.Background()
	// 清空标记为 false，且移开旧原生候选，验证导入本身永远不会自动采用。
	mustTakeSQL(t, e.db.Model(e.seg).Update("chosen_cleared", false))
	mustTakeSQL(t, e.db.Model(&model.GenerationTask{}).Where("user_id = ?", e.user).Update("node_id", "other-node"))
	for _, tc := range []struct {
		name                   string
		user                   uint64
		kind                   string
		test, deleted, missing bool
	}{{"other user", e.user + 1, model.KindVideo, false, false, false}, {"missing source", e.user, model.KindVideo, false, false, true}, {"deleted source", e.user, model.KindVideo, false, true, false}, {"test task", e.user, model.KindVideo, true, false, false}, {"image task", e.user, model.KindImage, false, false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			canvas, task := e.importedTask(t, tc.user, tc.kind, tc.test)
			source := canvas.ID
			if tc.deleted {
				mustTakeSQL(t, e.db.Delete(canvas))
			}
			if tc.missing {
				source = 1
			}
			before := e.account(t)
			_, err := e.segments.ImportTakes(ctx, e.user, e.seg.ID, &model.ImportDramaTakesReq{Revision: 1, SourceCanvasID: idcodec.ID(source), TaskID: idcodec.ID(task.ID)})
			if err == nil {
				t.Fatal("非法源或任务导入成功")
			}
			if after := e.account(t); !reflect.DeepEqual(before, after) {
				t.Fatal("非法导入改变任务/账户")
			}
			var count int64
			mustTakeSQL(t, e.db.Model(&model.DramaSegmentTakeLink{}).Count(&count))
			if count != 0 {
				t.Fatal("非法导入留下关系")
			}
		})
	}
	canvas, task := e.importedTask(t, e.user, model.KindVideo, false)
	req := &model.ImportDramaTakesReq{Revision: 1, SourceCanvasID: idcodec.ID(canvas.ID), TaskID: idcodec.ID(task.ID)}
	v, err := e.segments.ImportTakes(ctx, e.user, e.seg.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	if v.Revision != 1 || v.SegmentVersion != 1 || v.ChosenTaskID != "" {
		t.Fatalf("非采用导入改版本: %+v", v)
	}
	for range 3 {
		views, err := e.segments.List(ctx, e.user, e.p.ID, 1)
		if err != nil {
			t.Fatal(err)
		}
		v, err = e.segments.Get(ctx, e.user, e.seg.ID)
		if err != nil || len(views) != 1 || v.ChosenTaskID != "" || v.Revision != 1 || v.SegmentVersion != 1 {
			t.Fatalf("导入自动采用或漂移版本: %+v %v", v, err)
		}
	}
	v, err = e.segments.PatchTakeQC(ctx, e.user, e.seg.ID, task.ID, &model.PatchDramaTakeQCReq{Revision: 1, QC: model.TakeQCRejected})
	if err != nil {
		t.Fatal(err)
	}
	req.Revision, req.Adopt = v.Revision, true
	_, err = e.segments.ImportTakes(ctx, e.user, e.seg.ID, req)
	assertTaskCode(t, err, errcode.ErrDramaTakeQCRejected.Code)
	link, err := repository.NewDramaRepository(e.db).TakeLink(ctx, e.user, e.seg.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if link.Origin != model.TakeOriginCanvasImport || link.QC != model.TakeQCRejected {
		t.Fatalf("重导覆盖 QC: %+v", link)
	}
	var seg model.DramaSegment
	mustTakeSQL(t, e.db.First(&seg, e.seg.ID))
	if seg.Revision != 2 || seg.ChosenTaskID != nil || seg.ChosenCleared {
		t.Fatalf("驳回后重导改变采用: %+v", seg)
	}
	// 软删除源画布只禁止再次导入，不破坏已导入的视频、关联和人工 QC。
	if err := service.NewCanvasProjectService(repository.NewCanvasProjectRepository(e.db)).Delete(ctx, e.user, canvas.ID); err != nil {
		t.Fatal(err)
	}
	v, err = e.segments.Get(ctx, e.user, e.seg.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, take := range v.Takes {
		if uint64(take.TaskID) == task.ID && take.Asset != nil && take.Origin == "canvas_import" && take.QC == "rejected" {
			found = true
		}
	}
	if !found {
		t.Fatal("源画布软删破坏了导入候选")
	}
	_, err = e.segments.ImportTakes(ctx, e.user, e.seg.ID, req)
	assertTaskCode(t, err, errcode.ErrDramaNotFound.Code)
}

func TestDramaTakesPG16ConcurrentQCAndEpisodeCAS(t *testing.T) {
	e := newFrameBindPG(t)
	ctx := context.Background()
	repo := repository.NewDramaRepository(e.db)
	tasks, err := repo.SegmentTasks(ctx, e.user, e.p.CanvasID, e.req.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	tid := tasks[1].ID
	gate := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, qc := range []model.DramaSegmentTakeQC{model.TakeQCAccepted, model.TakeQCRejected} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			_, err := e.segments.PatchTakeQC(ctx, e.user, e.seg.ID, tid, &model.PatchDramaTakeQCReq{Revision: 1, QC: qc})
			errs <- err
		}()
	}
	close(gate)
	wg.Wait()
	close(errs)
	success, conflict := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else {
			assertTaskCode(t, err, errcode.ErrCanvasConflict.Code)
			conflict++
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("并发 QC 成功=%d 冲突=%d", success, conflict)
	}
	seg, err := repo.Segment(ctx, e.user, e.seg.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := repo.SegmentEpisode(ctx, e.user, e.ep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if seg.Revision != 2 || ep.SegmentVersion != 1 {
		t.Fatalf("纯 QC 版本错误: seg=%d ep=%d", seg.Revision, ep.SegmentVersion)
	}
	stale := int64(0)
	_, err = e.segments.PatchTakeQC(ctx, e.user, e.seg.ID, tid, &model.PatchDramaTakeQCReq{Revision: 2, ExpectedSegmentVersion: &stale, QC: model.TakeQCAccepted})
	assertTaskCode(t, err, errcode.ErrDramaManifestStale.Code)
}

func TestDramaTakesPG16RejectChosenReplacementAndClear(t *testing.T) {
	e := newFrameBindPG(t)
	ctx := context.Background()
	repo := repository.NewDramaRepository(e.db)
	tasks, err := repo.SegmentTasks(ctx, e.user, e.p.CanvasID, e.req.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	t1, t2 := tasks[0].ID, tasks[1].ID
	mustTakeSQL(t, e.db.Model(e.seg).Updates(map[string]any{"chosen_task_id": t1, "chosen_cleared": false, "source_in_f": 3, "source_out_f": 50, "mute_original": true, "audio_gain_db": -3, "native_is_dialogue": true, "av_pts_offset_ms": 100}))
	_, err = e.segments.PatchTakeQC(ctx, e.user, e.seg.ID, t1, &model.PatchDramaTakeQCReq{Revision: 1, QC: model.TakeQCRejected})
	assertTaskCode(t, err, errcode.ErrDramaFrameRunConflict.Code)
	replacement := idcodec.ID(t2)
	v, err := e.segments.PatchTakeQC(ctx, e.user, e.seg.ID, t1, &model.PatchDramaTakeQCReq{Revision: 1, QC: model.TakeQCRejected, ReplacementTaskID: &replacement})
	if err != nil {
		t.Fatal(err)
	}
	if v.ChosenTaskID != idcodec.Encode(t2) || v.Revision != 2 || v.SegmentVersion != 2 || v.Edit.SourceInF != 0 || v.Edit.SourceOutF != nil || v.Edit.MuteOriginal || v.Edit.AudioGainDB != 0 || v.Edit.NativeIsDialogue || v.Edit.AvPtsOffsetMs != 0 {
		t.Fatalf("替换未单次递增并重置六列: %+v", v)
	}
	v, err = e.segments.PatchTakeQC(ctx, e.user, e.seg.ID, t2, &model.PatchDramaTakeQCReq{Revision: 2, QC: model.TakeQCRejected, ClearChosen: true})
	if err != nil {
		t.Fatal(err)
	}
	if v.ChosenTaskID != "" || !v.ChosenCleared || v.Revision != 3 || v.SegmentVersion != 3 {
		t.Fatalf("显式清空失败: %+v", v)
	}
	for range 3 {
		if err := e.segments.OnTaskChanged(ctx, &tasks[1]); err != nil {
			t.Fatal(err)
		}
		if _, err := e.segments.List(ctx, e.user, e.p.ID, 1); err != nil {
			t.Fatal(err)
		}
		v, err = e.segments.ReconcileSegmentAndFrameRuns(ctx, e.user, e.seg.ID)
		if err != nil || v.ChosenTaskID != "" || !v.ChosenCleared || v.Revision != 3 || v.SegmentVersion != 3 {
			t.Fatalf("清空被对账逆转: %+v %v", v, err)
		}
	}
	// PATCH chosen_task_id:null 也走同一个持久化清空契约。
	mustTakeSQL(t, e.db.Model(e.seg).Updates(map[string]any{"chosen_task_id": t1, "chosen_cleared": false}))
	var patch model.UpdateDramaSegmentReq
	if err := json.Unmarshal([]byte(`{"revision":3,"chosen_task_id":null}`), &patch); err != nil {
		t.Fatal(err)
	}
	v, err = e.segments.Update(ctx, e.user, e.seg.ID, &patch)
	if err != nil {
		t.Fatal(err)
	}
	if v.ChosenTaskID != "" || !v.ChosenCleared {
		t.Fatal("PATCH null 未持久化清空")
	}
}

func TestDramaTakesPG16MediaMutexAndDeletedOrphan(t *testing.T) {
	for _, state := range []model.DramaFrameRunState{model.FrameRunStateFramePending, model.FrameRunStateSubmitting, model.FrameRunStateVideoPending} {
		t.Run(string(state), func(t *testing.T) {
			e := newFrameBindPG(t)
			ctx := context.Background()
			mustTakeSQL(t, e.db.Model(e.run).Update("state", state))
			other := &model.DramaSegment{UserID: e.user, EpisodeID: e.ep.ID, Seq: 1, Seconds: 5, Revision: 1}
			mustTakeSQL(t, e.db.Create(other))
			req := *e.req
			req.NodeID = "seg:" + idcodec.Encode(other.ID)
			req.QuoteToken = ""
			before := e.account(t)
			_, err := createSingle(ctx, e.tasks, e.user, "other-media", &req)
			assertTaskCode(t, err, errcode.ErrDramaJobBusy.Code)
			drama := service.NewDramaService(service.DramaDeps{Repo: repository.NewDramaRepository(e.db)})
			_, err = drama.CreateJob(ctx, e.user, e.p.ID, &model.DramaJobReq{Step: "compose", Episodes: []int{1}})
			assertTaskCode(t, err, errcode.ErrDramaJobBusy.Code)
			if after := e.account(t); !reflect.DeepEqual(before, after) {
				t.Fatal("互斥失败改变账本")
			}
			// 故意直接软删段以留下孤儿，验证互斥查询只覆盖存活片段。
			mustTakeSQL(t, e.db.Delete(e.seg))
			if _, err := createSingle(ctx, e.tasks, e.user, "other-media", &req); err != nil {
				t.Fatalf("孤儿流程仍阻塞媒体: %v", err)
			}
		})
	}
}

func TestDramaTakesPG16DeleteEpisodeCancelsActiveRuns(t *testing.T) {
	e := newFrameBindPG(t)
	before := e.account(t)
	err := repository.NewDramaCanvasRepository(e.db).WithTx(context.Background(), func(tx repository.DramaCanvasTx) error {
		if err := tx.LockProject(context.Background(), e.user, e.p.ID); err != nil {
			return err
		}
		return tx.DeleteEpisode(context.Background(), e.user, e.p.ID, 1)
	})
	if err != nil {
		t.Fatal(err)
	}
	run := e.reloadRun(t)
	if run.State != model.FrameRunStateCancelled || run.Revision != 2 {
		t.Fatalf("删除分集未同事务取消流程: %+v", run)
	}
	var seg model.DramaSegment
	mustTakeSQL(t, e.db.Unscoped().First(&seg, e.seg.ID))
	if !seg.DeletedAt.Valid {
		t.Fatal("段没有软删")
	}
	if after := e.account(t); !reflect.DeepEqual(before, after) {
		t.Fatal("删除分集修改了任务或积分")
	}
}

func TestDramaTakesPG16ReimportPreservesI2VQC(t *testing.T) {
	e := newFrameBindPG(t)
	ctx := context.Background()
	v, err := e.create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	native, err := repository.NewDramaRepository(e.db).SegmentTasks(ctx, e.user, e.p.CanvasID, e.req.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	assetID, err := firstVideoAsset(native[0])
	if err != nil {
		t.Fatal(err)
	}
	output, _ := json.Marshal([]model.TaskOutput{{AssetID: assetID, MediaType: model.KindVideo}})
	mustTakeSQL(t, e.db.Model(&model.GenerationTask{}).Where("id = ?", v.ID).Updates(map[string]any{"status": model.TaskSucceeded, "output_json": output}))
	view, err := e.segments.Get(ctx, e.user, e.seg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.ChosenTaskID != "" {
		t.Fatal("禁止自动采用的 run 被采用")
	}
	view, err = e.segments.PatchTakeQC(ctx, e.user, e.seg.ID, v.ID, &model.PatchDramaTakeQCReq{Revision: 1, QC: model.TakeQCRejected})
	if err != nil {
		t.Fatal(err)
	}
	mustTakeSQL(t, e.db.Create(&model.CanvasProject{BaseModel: model.BaseModel{ID: e.p.CanvasID}, UserID: e.user, PayloadJSON: []byte(`{}`)}))
	_, err = e.segments.ImportTakes(ctx, e.user, e.seg.ID, &model.ImportDramaTakesReq{Revision: view.Revision, SourceCanvasID: idcodec.ID(e.p.CanvasID), TaskID: idcodec.ID(v.ID), Adopt: true})
	assertTaskCode(t, err, errcode.ErrDramaTakeQCRejected.Code)
	link, err := repository.NewDramaRepository(e.db).TakeLink(ctx, e.user, e.seg.ID, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if link.Origin != model.TakeOriginI2V || link.QC != model.TakeQCRejected || link.RunID == nil {
		t.Fatalf("重导改写 I2V 来源/质检: %+v", link)
	}
}

func firstVideoAsset(task model.GenerationTask) (uint64, error) {
	var output []model.TaskOutput
	if err := json.Unmarshal(task.OutputJSON, &output); err != nil {
		return 0, err
	}
	for _, out := range output {
		if out.MediaType == model.KindVideo && out.AssetID > 0 {
			return out.AssetID, nil
		}
	}
	return 0, fmt.Errorf("fixture missing video")
}

// seedMatrixTakePG 为每格建立独立事务；服务内部仍使用真实的 FOR UPDATE、CAS 和唯一约束。
func seedMatrixTakePG(t *testing.T, db *gorm.DB, r *dramasegmentfake.Repo) (*gorm.DB, *repository.DramaRepository, *model.DramaProject) {
	t.Helper()
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	t.Cleanup(func() { tx.Rollback() })
	now := time.Now()
	p := &model.DramaProject{BaseModel: model.BaseModel{ID: 7}, UserID: 1, CanvasID: 9, Title: "matrix", StyleKey: "custom"}
	ep := &model.DramaEpisode{BaseModel: model.BaseModel{ID: 10}, UserID: 1, DramaID: 7, No: 1, SegmentVersion: 17, LockedAt: &now}
	mustTakeSQL(t, tx.Create(p))
	mustTakeSQL(t, tx.Create(ep))
	mustTakeSQL(t, tx.Create(&r.Rows[0]))
	mustTakeSQL(t, tx.Create(&r.Tasks))
	mustTakeSQL(t, tx.Create(&r.FrameRuns[0]))
	mustTakeSQL(t, tx.Create(&model.Asset{ID: 41, UserID: 1, Kind: model.KindVideo, StorageKey: "matrix.mp4", Sha256: strings.Repeat("a", 64)}))
	return tx, repository.NewDramaRepository(tx), p
}

type pgTakeAssets struct{ db *gorm.DB }

func (a pgTakeAssets) View(ctx context.Context, user, id uint64) (*model.AssetView, error) {
	asset, err := repository.NewAssetRepository(a.db).GetByID(ctx, user, id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &model.AssetView{ID: asset.ID, Kind: asset.Kind}, nil
}

func TestDramaTakesPG16NegativeFourEntries(t *testing.T) {
	now := time.Now()
	db, _, _, _, _, _ := segEditPGEnv(t, &now)
	assertSlice2PG16(t, db)
	for _, entry := range []int{0, 1, 2, 3} {
		for _, reason := range []string{"cancelled", "failed", "missing-forward-pending", "missing-forward-bound", "deleted-asset"} {
			t.Run(fmt.Sprintf("%s/entry%d", reason, entry), func(t *testing.T) {
				r := matrixTakeRepo(takeMatrixCase{adopt: true})
				switch reason {
				case "cancelled":
					r.FrameRuns[0].State = model.FrameRunStateCancelled
				case "failed":
					r.FrameRuns[0].State = model.FrameRunStateFailed
				case "missing-forward-pending":
					r.FrameRuns[0].VideoTaskID = nil
				case "missing-forward-bound":
					r.FrameRuns[0].VideoTaskID = nil
					r.FrameRuns[0].State = model.FrameRunStateBound
				}
				tx, repo, p := seedMatrixTakePG(t, db, r)
				if reason == "deleted-asset" {
					mustTakeSQL(t, tx.Delete(&model.Asset{ID: 41}))
				}
				assets := pgTakeAssets{tx}
				s := service.NewDramaSegmentService(service.DramaSegmentDeps{Repo: repo, Assets: assets})
				triggerTakeEntry(t, s, &r.Tasks[0], entry)
				seg, err := repo.Segment(context.Background(), 1, 1, false)
				if err != nil {
					t.Fatal(err)
				}
				takes, err := service.ResolveSegmentTakes(context.Background(), repo, assets, p, seg)
				if err != nil {
					t.Fatal(err)
				}
				for _, take := range takes {
					if take.Usable || !take.IsFrameRun {
						t.Fatal("失格流程被列为可用或回落 native")
					}
				}
				var links int64
				mustTakeSQL(t, tx.Model(&model.DramaSegmentTakeLink{}).Count(&links))
				ep, err := repo.SegmentEpisode(context.Background(), 1, 10)
				if err != nil {
					t.Fatal(err)
				}
				if links != 0 || seg.ChosenTaskID != nil || seg.Revision != 2 || ep.SegmentVersion != 17 {
					t.Fatalf("失格流程产生副作用: %+v links=%d", seg, links)
				}
			})
		}
	}
}

func TestDramaTakesStorageFailureAndRecovery(t *testing.T) {
	expected := errors.New("asset database unavailable")
	for _, entry := range []int{0, 1, 2, 3} {
		t.Run(fmt.Sprint(entry), func(t *testing.T) {
			r := matrixTakeRepo(takeMatrixCase{adopt: true})
			s := service.NewDramaSegmentService(service.DramaSegmentDeps{Repo: r, Assets: unavailableTakeAssets{err: expected}})
			var err error
			switch entry {
			case 0:
				err = s.OnTaskChanged(context.Background(), &r.Tasks[0])
			case 1:
				_, err = s.Get(context.Background(), 1, 1)
			case 2:
				_, err = s.List(context.Background(), 1, 7, 1)
			case 3:
				_, err = s.ReconcileSegmentAndFrameRuns(context.Background(), 1, 1)
			}
			if !errors.Is(err, expected) {
				t.Fatalf("真实故障被吞掉: %v", err)
			}
			if len(r.TakeLinks) != 0 || r.Rows[0].Revision != 2 {
				t.Fatal("真实故障产生半写")
			}
		})
	}
	r := matrixTakeRepo(takeMatrixCase{adopt: true})
	s := segmentService(r)
	if err := s.RecoverFrameRuns(context.Background(), repository.FrameRunRecoveryFilter{UserID: 1}, 10); err != nil {
		t.Fatal(err)
	}
	if len(r.TakeLinks) != 1 || r.FrameRuns[0].State != model.FrameRunStateBound || r.Rows[0].ChosenTaskID == nil {
		t.Fatal("持久化恢复扫描未使用共享推进")
	}
}

type takeComposeAssets struct{ *segEditAssets }

func (a takeComposeAssets) PrepareDramaVideo(context.Context, uint64, string) (*model.Asset, error) {
	return nil, errors.New("此测试只读取 manifest")
}
func (a takeComposeAssets) DiscardCopies(context.Context, []*model.Asset) {}
func (a takeComposeAssets) ViewOf(_ context.Context, asset *model.Asset) (*model.AssetView, error) {
	return &model.AssetView{ID: asset.ID, Kind: asset.Kind}, nil
}

func TestDramaTakesPG16SelectionProbeAndManifestConsumers(t *testing.T) {
	for _, origin := range []string{"native", "i2v", "canvas_import"} {
		t.Run(origin, func(t *testing.T) {
			e := newFrameBindPG(t)
			ctx := context.Background()
			repo := repository.NewDramaRepository(e.db)
			native, err := repo.SegmentTasks(ctx, e.user, e.p.CanvasID, e.req.NodeID)
			if err != nil {
				t.Fatal(err)
			}
			taskID := native[0].ID
			assetID, err := firstVideoAsset(native[0])
			if err != nil {
				t.Fatal(err)
			}
			switch origin {
			case "canvas_import":
				canvas, task := e.importedTask(t, e.user, model.KindVideo, false)
				taskID = task.ID
				assetID, err = firstVideoAsset(*task)
				if err != nil {
					t.Fatal(err)
				}
				_, err = e.segments.ImportTakes(ctx, e.user, e.seg.ID, &model.ImportDramaTakesReq{Revision: 1, SourceCanvasID: idcodec.ID(canvas.ID), TaskID: idcodec.ID(taskID)})
				if err != nil {
					t.Fatal(err)
				}
				mustTakeSQL(t, e.db.Create(&model.AssetMediaProbe{AssetID: assetID, Sha256: e.assets.assets[assetID].Sha256, ProbeVersion: videoframe.NormalizedProbeVersion, NormalizedFrames24: 100}))
			case "i2v":
				task, err := e.create(ctx)
				if err != nil {
					t.Fatal(err)
				}
				taskID = task.ID
				mustTakeSQL(t, e.db.Model(&model.GenerationTask{}).Where("id = ?", taskID).Updates(map[string]any{"status": model.TaskSucceeded, "output_json": native[0].OutputJSON}))
			}
			chosen := idcodec.ID(taskID)
			selected, err := e.segments.Update(ctx, e.user, e.seg.ID, &model.UpdateDramaSegmentReq{Revision: 1, ChosenTaskID: &chosen})
			if err != nil {
				t.Fatal(err)
			}
			if selected.ChosenCleared || selected.ChosenTaskID != idcodec.Encode(taskID) {
				t.Fatal("手动选择未共用准入")
			}
			inF, outF := int32(1), int32(20)
			selected, err = e.segments.Update(ctx, e.user, e.seg.ID, &model.UpdateDramaSegmentReq{Revision: selected.Revision, Edit: &model.DramaSegmentEditPatch{SourceInF: &inF, SourceOutF: model.NullableInt32{Set: true, Value: &outF}}})
			if err != nil {
				t.Fatal(err)
			}
			drama := service.NewDramaService(service.DramaDeps{Repo: repo, Assets: takeComposeAssets{e.assets}, Probes: repository.NewAssetMediaProbeRepository(e.db)})
			manifest, err := drama.ResolveEpisodeManifest(ctx, e.user, e.p.ID, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(manifest.Manifest.Clips) != 1 || manifest.Manifest.Clips[0].TaskID != taskID || manifest.Manifest.Clips[0].AssetID != assetID || manifest.Manifest.Clips[0].SourceInF != 1 || manifest.Manifest.Clips[0].SourceOutF != 20 {
				t.Fatalf("跨来源采用未进入探测与清单: %+v", manifest)
			}
			_, err = e.segments.PatchTakeQC(ctx, e.user, e.seg.ID, taskID, &model.PatchDramaTakeQCReq{Revision: selected.Revision, QC: model.TakeQCAccepted})
			if err != nil {
				t.Fatal(err)
			}
			// 模拟历史失格记录，验证合成源读取拒绝，不静默回退到其他成功任务。
			if err := repo.UpdateTakeLinkQC(ctx, e.user, e.seg.ID, taskID, model.TakeQCRejected); err != nil {
				t.Fatal(err)
			}
			_, err = drama.ResolveEpisodeManifest(ctx, e.user, e.p.ID, 1)
			assertTaskCode(t, err, errcode.ErrDramaTakeQCRejected.Code)
		})
	}
}

func TestDramaTakesInvalidActiveSourceCannotPolluteStatus(t *testing.T) {
	for _, kind := range []string{model.KindVideo, model.KindImage} {
		t.Run(kind, func(t *testing.T) {
			r := matrixTakeRepo(takeMatrixCase{})
			r.Tasks[0].Status = model.TaskRunning
			r.Tasks[0].Kind = kind
			if kind == model.KindVideo {
				other := uint64(99)
				r.Tasks[0].CanvasProjectID = &other
				r.Tasks[0].FrameRunID = nil
				r.Tasks[0].FrameRunConfirmRev = nil
				r.FrameRuns = nil
				r.TakeLinks = []model.DramaSegmentTakeLink{{UserID: 1, SegmentID: 1, TaskID: 21, Origin: model.TakeOriginNative, QC: model.TakeQCUnreviewed}}
			}
			v, err := segmentService(r).Get(context.Background(), 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			if v.Status != "idle" || len(v.Takes) != 0 {
				t.Fatalf("来源失格的进行态污染状态: %+v", v)
			}
		})
	}
}

func TestDramaTakesQCChosenAndExplicitClear(t *testing.T) {
	r := matrixTakeRepo(takeMatrixCase{chosen: true})
	out := int32(50)
	r.Rows[0].SourceInF, r.Rows[0].SourceOutF, r.Rows[0].MuteOriginal, r.Rows[0].AudioGainDB, r.Rows[0].NativeIsDialogue, r.Rows[0].AvPtsOffsetMs = 3, &out, true, -3, true, 100
	s := segmentService(r)
	ctx := context.Background()
	for _, req := range []*model.PatchDramaTakeQCReq{{Revision: 2, QC: model.TakeQCRejected}, {Revision: 2, QC: model.TakeQCRejected, ClearChosen: true, ReplacementTaskID: new(idcodec.ID(21))}, {Revision: 2, QC: model.TakeQCRejected, ReplacementTaskID: new(idcodec.ID(20))}} {
		_, err := s.PatchTakeQC(ctx, 1, 1, 20, req)
		segmentCode(t, err, errcode.ErrDramaFrameRunConflict.Code)
		if r.Rows[0].Revision != 2 || len(r.TakeLinks) != 0 {
			t.Fatal("失败 QC 未回滚")
		}
	}
	v, err := s.PatchTakeQC(ctx, 1, 1, 20, &model.PatchDramaTakeQCReq{Revision: 2, QC: model.TakeQCRejected, ReplacementTaskID: new(idcodec.ID(21))})
	if err != nil {
		t.Fatal(err)
	}
	if v.ChosenTaskID != idcodec.Encode(21) || v.ChosenCleared || v.Revision != 3 || v.SegmentVersion != 18 || v.Edit.SourceInF != 0 || v.Edit.SourceOutF != nil || v.Edit.MuteOriginal || v.Edit.AudioGainDB != 0 || v.Edit.NativeIsDialogue || v.Edit.AvPtsOffsetMs != 0 || r.FrameRuns[0].State != model.FrameRunStateBound {
		t.Fatalf("采用替换未原子绑定/重置六列: %+v", v)
	}
	v, err = s.PatchTakeQC(ctx, 1, 1, 21, &model.PatchDramaTakeQCReq{Revision: 3, QC: model.TakeQCRejected, ClearChosen: true})
	if err != nil {
		t.Fatal(err)
	}
	if v.ChosenTaskID != "" || !v.ChosenCleared || v.Revision != 4 || v.SegmentVersion != 19 {
		t.Fatalf("QC 清空失败: %+v", v)
	}
	for _, entry := range []int{0, 1, 2, 3} {
		triggerTakeEntry(t, s, &r.Tasks[0], entry)
		if r.Rows[0].ChosenTaskID != nil || !r.Rows[0].ChosenCleared || r.Rows[0].Revision != 4 || r.SegmentVersion != 19 {
			t.Fatal("对账逆转显式清空")
		}
	}
	_, err = s.Update(ctx, 1, 1, &model.UpdateDramaSegmentReq{Revision: 4, ClearChosen: true, Edit: &model.DramaSegmentEditPatch{}})
	segmentCode(t, err, errcode.ErrDramaEditInvalid.Code)
	_, err = s.Update(ctx, 1, 1, &model.UpdateDramaSegmentReq{Revision: 4, ClearChosen: true, ChosenTaskID: new(idcodec.ID(21))})
	segmentCode(t, err, errcode.ErrInvalidParams.Code)
}

func importedTakeFixture() (*dramasegmentfake.Repo, *service.DramaSegmentService) {
	r := dramasegmentfake.New()
	r.Rows = []model.DramaSegment{{BaseModel: model.BaseModel{ID: 1}, UserID: 1, EpisodeID: 10, Seconds: 5, Revision: 1}}
	canvas := uint64(11)
	r.Tasks = []model.GenerationTask{{ID: 21, UserID: 1, CanvasProjectID: &canvas, NodeID: "canvas-video", Kind: model.KindVideo, Status: model.TaskSucceeded, OutputJSON: []byte(`[{"asset_id":41,"media_type":"video"}]`)}}
	s := service.NewDramaSegmentService(service.DramaSegmentDeps{Repo: r, Assets: r, Canvases: takeCanvases{map[uint64]*model.CanvasProject{11: {BaseModel: model.BaseModel{ID: 11}, UserID: 1}}}})
	return r, s
}

func TestDramaTakesImportAdmissionAndAdopt(t *testing.T) {
	for _, reason := range []string{"other task user", "test task", "frame run task", "image task", "pending task", "wrong canvas", "no output", "missing task", "missing canvas"} {
		t.Run(reason, func(t *testing.T) {
			r, s := importedTakeFixture()
			req := &model.ImportDramaTakesReq{Revision: 1, SourceCanvasID: 11, TaskID: 21}
			switch reason {
			case "other task user":
				r.Tasks[0].UserID = 2
			case "test task":
				r.Tasks[0].IsTest = true
			case "frame run task":
				r.Tasks[0].FrameRunID = new(uint64(31))
			case "image task":
				r.Tasks[0].Kind = model.KindImage
			case "pending task":
				r.Tasks[0].Status = model.TaskPending
			case "wrong canvas":
				r.Tasks[0].CanvasProjectID = new(uint64(12))
			case "no output":
				r.Tasks[0].OutputJSON = nil
			case "missing task":
				req.TaskID = 42
			case "missing canvas":
				req.SourceCanvasID = 12
			}
			_, err := s.ImportTakes(context.Background(), 1, 1, req)
			expected := errcode.ErrDramaTakeInvalid.Code
			if reason == "missing canvas" {
				expected = errcode.ErrDramaNotFound.Code
			}
			segmentCode(t, err, expected)
			if len(r.TakeLinks) != 0 || r.Rows[0].Revision != 1 || r.SegmentVersion != 0 {
				t.Fatal("非法导入产生部分写入")
			}
		})
	}
	r, s := importedTakeFixture()
	r.Rows[0].SourceInF, r.Rows[0].SourceOutF, r.Rows[0].MuteOriginal, r.Rows[0].AudioGainDB, r.Rows[0].NativeIsDialogue, r.Rows[0].AvPtsOffsetMs = 2, new(int32(50)), true, -3, true, 100
	r.Rows[0].ChosenTaskID = new(uint64(20))
	r.Rows[0].ChosenCleared = true
	req := &model.ImportDramaTakesReq{Revision: 1, SourceCanvasID: 11, TaskID: 21, TaskIDs: []idcodec.ID{21}, Adopt: true}
	v, err := s.ImportTakes(context.Background(), 1, 1, req)
	if err != nil {
		t.Fatal(err)
	}
	if v.ChosenTaskID != idcodec.Encode(21) || v.ChosenCleared || v.Revision != 2 || v.SegmentVersion != 1 || v.Edit.SourceInF != 0 || v.Edit.SourceOutF != nil || v.Edit.MuteOriginal || v.Edit.AudioGainDB != 0 || v.Edit.NativeIsDialogue || v.Edit.AvPtsOffsetMs != 0 {
		t.Fatalf("导入采用未单次保存/重置: %+v", v)
	}
	r.Tasks = append(r.Tasks, r.Tasks[0])
	r.Tasks[1].ID = 22
	req.Revision, req.TaskIDs, req.TaskID = 2, nil, 22
	req.ResetEditOnTaskChange = new(false)
	r.Rows[0].SourceInF, r.Rows[0].MuteOriginal = 3, true
	v, err = s.ImportTakes(context.Background(), 1, 1, req)
	if err != nil {
		t.Fatal(err)
	}
	if v.Edit.SourceInF != 3 || !v.Edit.MuteOriginal || v.Revision != 3 || v.SegmentVersion != 2 {
		t.Fatalf("显式保留剪辑失败: %+v", v)
	}
	req.Revision, req.TaskIDs = 3, []idcodec.ID{21}
	_, err = s.ImportTakes(context.Background(), 1, 1, req)
	segmentCode(t, err, errcode.ErrInvalidParams.Code)
}

type fakeTakeProbeStore struct {
	frames map[uint64]int32
}

func (s *fakeTakeProbeStore) Get(_ context.Context, assetID uint64, _ string, _ int16) (*model.AssetMediaProbe, error) {
	frames, ok := s.frames[assetID]
	if !ok {
		return nil, repository.ErrNotFound
	}
	return &model.AssetMediaProbe{AssetID: assetID, ProbeVersion: videoframe.NormalizedProbeVersion, NormalizedFrames24: frames}, nil
}

func (s *fakeTakeProbeStore) Upsert(context.Context, *model.AssetMediaProbe) error { return nil }

type takeTrimAssets struct {
	rows map[uint64]*model.Asset
}

func (a takeTrimAssets) View(_ context.Context, userID, id uint64) (*model.AssetView, error) {
	row := a.rows[id]
	if row == nil || row.UserID != userID {
		return nil, errcode.ErrAssetNotFound
	}
	return &model.AssetView{ID: id, Kind: row.Kind, URL: "/v.mp4"}, nil
}

func (a takeTrimAssets) Get(_ context.Context, userID, id uint64) (*model.Asset, error) {
	row := a.rows[id]
	if row == nil || row.UserID != userID {
		return nil, provider.ErrAssetNotFound
	}
	return row, nil
}

func (a takeTrimAssets) Open(context.Context, uint64, uint64) (*provider.AssetFile, error) {
	return nil, provider.ErrAssetNotFound
}

func TestDramaTakesRetainedTrimBoundsValidated(t *testing.T) {
	r, _ := importedTakeFixture()
	canvas9, canvas11 := uint64(9), uint64(11)
	r.Tasks = []model.GenerationTask{
		{ID: 20, UserID: 1, CanvasProjectID: &canvas9, NodeID: "seg:" + idcodec.Encode(1), Kind: model.KindVideo, Status: model.TaskSucceeded, OutputJSON: []byte(`[{"asset_id":901,"media_type":"video"}]`)},
		{ID: 22, UserID: 1, CanvasProjectID: &canvas11, NodeID: "canvas-video", Kind: model.KindVideo, Status: model.TaskSucceeded, OutputJSON: []byte(`[{"asset_id":902,"media_type":"video"}]`)},
	}
	assets := takeTrimAssets{rows: map[uint64]*model.Asset{
		901: {ID: 901, UserID: 1, Kind: model.KindVideo},
		902: {ID: 902, UserID: 1, Kind: model.KindVideo},
	}}
	probes := &fakeTakeProbeStore{frames: map[uint64]int32{901: 240, 902: 120}}
	s := service.NewDramaSegmentService(service.DramaSegmentDeps{Repo: r, Canvases: takeCanvases{rows: map[uint64]*model.CanvasProject{
		9:  {BaseModel: model.BaseModel{ID: 9}, UserID: 1},
		11: {BaseModel: model.BaseModel{ID: 11}, UserID: 1},
	}}, Assets: assets, Probes: probes, Broadcaster: r})

	// 1. 当前剪辑 [100, 220)，保留剪辑导入采纳仅 120 帧的新视频必须拦截为 91003。
	r.Rows[0].SourceInF, r.Rows[0].SourceOutF = 100, new(int32(220))
	r.Rows[0].ChosenTaskID = new(uint64(20))
	_, err := s.ImportTakes(context.Background(), 1, 1, &model.ImportDramaTakesReq{
		Revision: 1, SourceCanvasID: 11, TaskID: 22, Adopt: true, ResetEditOnTaskChange: new(false),
	})
	segmentCode(t, err, errcode.ErrDramaEditInvalid.Code)
	if *r.Rows[0].ChosenTaskID != 20 {
		t.Fatalf("越界导入不应改动当前采用: %+v", r.Rows[0])
	}

	// 2. 驳回当前采用并保留越界剪辑切换到较短候选 22 时同样拦截，不得写坏剪辑与质检。
	r.Rows[0].Revision = 1
	r.TakeLinks = []model.DramaSegmentTakeLink{
		{UserID: 1, SegmentID: 1, TaskID: 20, SourceCanvasID: 9, AssetID: 901, Origin: model.TakeOriginNative, QC: model.TakeQCUnreviewed},
		{UserID: 1, SegmentID: 1, TaskID: 22, SourceCanvasID: 11, AssetID: 902, Origin: model.TakeOriginCanvasImport, QC: model.TakeQCUnreviewed},
	}
	_, err = s.PatchTakeQC(context.Background(), 1, 1, 20, &model.PatchDramaTakeQCReq{
		Revision: 1, QC: model.TakeQCRejected, ReplacementTaskID: new(idcodec.ID(22)), ResetEditOnTaskChange: new(false),
	})
	segmentCode(t, err, errcode.ErrDramaEditInvalid.Code)
	if r.TakeLinks[0].QC != model.TakeQCUnreviewed || *r.Rows[0].ChosenTaskID != 20 {
		t.Fatalf("越界替换不应提交 QC 或采用变更: link=%+v seg=%+v", r.TakeLinks[0], r.Rows[0])
	}

	// 3. 缩短到 [0, 120) 内则保留剪辑替换允许通过。
	r.Rows[0].SourceInF, r.Rows[0].SourceOutF = 10, new(int32(100))
	v, err := s.PatchTakeQC(context.Background(), 1, 1, 20, &model.PatchDramaTakeQCReq{
		Revision: 1, QC: model.TakeQCRejected, ReplacementTaskID: new(idcodec.ID(22)), ResetEditOnTaskChange: new(false),
	})
	if err != nil || v.ChosenTaskID != idcodec.Encode(22) || v.Edit.SourceInF != 10 {
		t.Fatalf("区间合法的保留替换应成功: %+v %v", v, err)
	}
}

func TestDramaTakesPureQCDoesNotAutoAdopt(t *testing.T) {
	r := matrixTakeRepo(takeMatrixCase{})
	r.FrameRuns = nil
	r.Tasks[0].FrameRunID, r.Tasks[0].FrameRunConfirmRev = nil, nil
	v, err := segmentService(r).PatchTakeQC(context.Background(), 1, 1, 21, &model.PatchDramaTakeQCReq{Revision: 2, QC: model.TakeQCAccepted})
	if err != nil {
		t.Fatal(err)
	}
	if v.ChosenTaskID != "" || v.Revision != 3 || v.SegmentVersion != 17 {
		t.Fatalf("纯 QC 偷改了采用或集版本: %+v", v)
	}
}
