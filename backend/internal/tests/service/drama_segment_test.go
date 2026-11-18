package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
	"video-canvas/internal/service/dramasegmentfake"
)

func segmentService(r *dramasegmentfake.Repo) *service.DramaSegmentService {
	return service.NewDramaSegmentService(service.DramaSegmentDeps{Repo: r, Tasks: r, Assets: r, Broadcaster: r})
}

func segmentCode(t *testing.T, err error, code int) {
	t.Helper()
	var business *errcode.Error
	if !errors.As(err, &business) || business.Code != code {
		t.Fatalf("期望错误码%d，实际%v", code, err)
	}
}

func TestDramaSegmentService_CRUDAndOrder(t *testing.T) {
	ctx := context.Background()
	r := dramasegmentfake.New()
	s := segmentService(r)
	first, err := s.Create(ctx, 1, 7, 1, &model.CreateDramaSegmentReq{})
	if err != nil || first.Seq != 0 || first.Revision != 1 {
		t.Fatalf("新增默认片段：%+v %v", first, err)
	}
	after := 0
	second, err := s.Create(ctx, 1, 7, 1, &model.CreateDramaSegmentReq{AfterSeq: &after})
	if err != nil || second.Seq != 1 {
		t.Fatalf("插入片段：%+v %v", second, err)
	}
	_, err = s.Order(ctx, 1, 7, 1, &model.OrderDramaSegmentsReq{IDs: []idcodec.ID{first.ID, first.ID}})
	segmentCode(t, err, 67012)
	views, err := s.Order(ctx, 1, 7, 1, &model.OrderDramaSegmentsReq{IDs: []idcodec.ID{second.ID, first.ID}})
	if err != nil || views[0].ID != second.ID || views[1].Seq != 1 {
		t.Fatalf("全量重排：%+v %v", views, err)
	}
	prompt := "修改"
	_, err = s.Update(ctx, 1, uint64(first.ID), &model.UpdateDramaSegmentReq{Revision: 1, Prompt: &prompt})
	segmentCode(t, err, errcode.ErrCanvasConflict.Code)
	view, err := s.Update(ctx, 1, uint64(first.ID), &model.UpdateDramaSegmentReq{Revision: views[1].Revision, Prompt: &prompt})
	if err != nil || view.Prompt != prompt {
		t.Fatalf("更新片段：%+v %v", view, err)
	}
	_, err = s.Update(ctx, 2, uint64(first.ID), &model.UpdateDramaSegmentReq{Revision: view.Revision})
	segmentCode(t, err, 67009)
	if err := s.Delete(ctx, 1, uint64(first.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Generate(ctx, 1, uint64(first.ID), "", &model.GenerateDramaSegmentReq{ModelID: "video", Input: map[string]any{}}); err == nil {
		t.Fatal("删除后不可生成")
	}
}

func TestDramaSegmentService_TakesReconcileAndStatus(t *testing.T) {
	ctx := context.Background()
	r := dramasegmentfake.New()
	s := segmentService(r)
	v, err := s.Create(ctx, 1, 7, 1, &model.CreateDramaSegmentReq{})
	if err != nil {
		t.Fatal(err)
	}
	node := "seg:" + idcodec.Encode(uint64(v.ID))
	canvas := uint64(9)
	output, _ := json.Marshal([]model.TaskOutput{{AssetID: 41, MediaType: "video"}})
	r.Assets = map[uint64]model.Asset{41: {ID: 41, UserID: 1, Kind: model.KindVideo}}
	r.Tasks = []model.GenerationTask{
		{ID: 21, UserID: 1, CanvasProjectID: &canvas, NodeID: node, Kind: "video", Status: "succeeded", OutputJSON: output, CreatedAt: time.Unix(1, 0)},
		{ID: 22, UserID: 1, CanvasProjectID: &canvas, NodeID: node, Kind: "video", Status: "running", CreatedAt: time.Unix(2, 0)},
		{ID: 23, UserID: 2, CanvasProjectID: &canvas, NodeID: node, Kind: "video", Status: "succeeded"},
	}
	items, err := s.List(ctx, 1, 7, 1)
	if err != nil || len(items[0].Takes) != 2 || items[0].ChosenTaskID != idcodec.Encode(21) || items[0].Status != "running" || items[0].Takes[0].Asset.ID != 41 {
		t.Fatalf("从真实任务恢复采用并优先最新状态：%+v %v", items, err)
	}
	bad := idcodec.ID(22)
	_, err = s.Update(ctx, 1, uint64(v.ID), &model.UpdateDramaSegmentReq{Revision: items[0].Revision, ChosenTaskID: &bad})
	segmentCode(t, err, 67011)
	r.Tasks[1].Status = "failed"
	if err := s.OnTaskChanged(ctx, &r.Tasks[1]); err != nil {
		t.Fatal(err)
	}
	if r.Messages[len(r.Messages)-1].Type != "drama.segment" {
		t.Fatalf("未推送片段状态：%+v", r.Messages)
	}
	items, err = s.List(ctx, 1, 7, 1)
	if err != nil || items[0].Status != "error" {
		t.Fatalf("失败take优先：%+v %v", items, err)
	}
	// 历史遗留的首帧任务（segkf: 节点）不再触发片段更新，也不出现在片段视图里。
	kf, _ := json.Marshal([]model.TaskOutput{{AssetID: 52, MediaType: "image"}})
	r.Tasks = append(r.Tasks, model.GenerationTask{ID: 24, UserID: 1, CanvasProjectID: &canvas, NodeID: "segkf:" + idcodec.Encode(uint64(v.ID)), Kind: "image", Status: "succeeded", OutputJSON: kf, CreatedAt: time.Unix(3, 0)})
	messages := len(r.Messages)
	if err := s.OnTaskChanged(ctx, &r.Tasks[len(r.Tasks)-1]); err != nil {
		t.Fatal(err)
	}
	if len(r.Messages) != messages {
		t.Fatalf("首帧任务不该再推送片段事件：%+v", r.Messages[messages:])
	}
	if err := s.Delete(ctx, 1, uint64(v.ID)); err != nil || len(r.Tasks) != 4 {
		t.Fatalf("删除保留生成任务：%v %+v", err, r.Tasks)
	}
}

func TestDramaSegmentService_GenerationDelegatesAndErrors(t *testing.T) {
	ctx := context.Background()
	r := dramasegmentfake.New()
	s := segmentService(r)
	v, err := s.Create(ctx, 1, 7, 1, &model.CreateDramaSegmentReq{})
	if err != nil {
		t.Fatal(err)
	}
	req := &model.GenerateDramaSegmentReq{ModelID: "model", Input: map[string]any{"prompt": "内容"}}
	if _, err := s.Generate(ctx, 1, uint64(v.ID), "same-request", req); err != nil {
		t.Fatal(err)
	}
	if r.Created.Kind != "video" || r.Created.CanvasID != 9 || r.Created.NodeID != "seg:"+idcodec.Encode(uint64(v.ID)) || r.IdempotencyKey != "same-request" {
		t.Fatalf("生成参数错误：%+v key=%s", r.Created, r.IdempotencyKey)
	}
	r.CreateError = &model.TaskItemError{Status: 402, Code: 42001, Message: "积分不足"}
	_, err = s.Generate(ctx, 1, uint64(v.ID), "new-request", req)
	segmentCode(t, err, 42001)
	r.ProjectDeleted = true
	_, err = s.Generate(ctx, 1, uint64(v.ID), "gone", req)
	segmentCode(t, err, 67009)
}

func TestDramaSegmentService_OrderRejectsMissingAndForeignIDs(t *testing.T) {
	ctx := context.Background()
	for _, ids := range [][]idcodec.ID{{}, {999}, {1, 999}} {
		r := dramasegmentfake.New()
		s := segmentService(r)
		if _, err := s.Create(ctx, 1, 7, 1, &model.CreateDramaSegmentReq{}); err != nil {
			t.Fatal(err)
		}
		_, err := s.Order(ctx, 1, 7, 1, &model.OrderDramaSegmentsReq{IDs: ids})
		segmentCode(t, err, 67012)
		rows, err := s.List(ctx, 1, 7, 1)
		if err != nil || len(rows) != 1 || rows[0].Seq != 0 || rows[0].Revision != 1 {
			t.Fatalf("非法全量集合不可部分写入：%+v %v", rows, err)
		}
	}
}

func TestDramaSegmentService_ManualChoiceSurvivesLaterSuccess(t *testing.T) {
	ctx := context.Background()
	r := dramasegmentfake.New()
	s := segmentService(r)
	v, err := s.Create(ctx, 1, 7, 1, &model.CreateDramaSegmentReq{})
	if err != nil {
		t.Fatal(err)
	}
	canvas := uint64(9)
	node := "seg:" + idcodec.Encode(uint64(v.ID))
	output, _ := json.Marshal([]model.TaskOutput{{AssetID: 41, MediaType: "video"}})
	r.Tasks = []model.GenerationTask{
		{ID: 21, UserID: 1, CanvasProjectID: &canvas, NodeID: node, Kind: "video", Status: "succeeded", OutputJSON: output, CreatedAt: time.Unix(1, 0)},
		{ID: 22, UserID: 1, CanvasProjectID: &canvas, NodeID: node, Kind: "video", Status: "succeeded", OutputJSON: output, CreatedAt: time.Unix(2, 0)},
	}
	views, err := s.List(ctx, 1, 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	chosen := idcodec.ID(22)
	selected, err := s.Update(ctx, 1, uint64(v.ID), &model.UpdateDramaSegmentReq{Revision: views[0].Revision, ChosenTaskID: &chosen})
	if err != nil || selected.ChosenTaskID != idcodec.Encode(22) {
		t.Fatalf("手动采用成功结果：%+v %v", selected, err)
	}
	r.Tasks = append(r.Tasks, model.GenerationTask{ID: 23, UserID: 1, CanvasProjectID: &canvas, NodeID: node, Kind: "video", Status: "succeeded", OutputJSON: output, CreatedAt: time.Unix(3, 0)})
	if err := s.OnTaskChanged(ctx, &r.Tasks[2]); err != nil {
		t.Fatal(err)
	}
	// 之后又有新的成功结果（23），已经手选的 22 不被覆盖。
	views, err = s.List(ctx, 1, 7, 1)
	if err != nil || views[0].ChosenTaskID != idcodec.Encode(22) {
		t.Fatalf("手选保留：%+v %v", views, err)
	}
	count, seconds, done, err := s.Summary(ctx, 1, 10)
	if err != nil || count != 1 || seconds != 5 || done != 1 {
		t.Fatalf("分集摘要：%d/%d/%d %v", count, seconds, done, err)
	}
}

func TestDramaSegmentService_ObserverIsolationAndUnknownErrors(t *testing.T) {
	ctx := context.Background()
	r := dramasegmentfake.New()
	s := segmentService(r)
	v, err := s.Create(ctx, 1, 7, 1, &model.CreateDramaSegmentReq{})
	if err != nil {
		t.Fatal(err)
	}
	canvas, other := uint64(9), uint64(99)
	node := "seg:" + idcodec.Encode(uint64(v.ID))
	r.Tasks = []model.GenerationTask{
		{ID: 1, UserID: 1, CanvasProjectID: &canvas, NodeID: node, Kind: "video", Status: "succeeded", IsTest: true},
		{ID: 2, UserID: 1, CanvasProjectID: &other, NodeID: node, Kind: "video", Status: "succeeded"},
		{ID: 3, UserID: 2, CanvasProjectID: &canvas, NodeID: node, Kind: "video", Status: "succeeded"},
	}
	before := len(r.Messages)
	for i := range r.Tasks {
		if err := s.OnTaskChanged(ctx, &r.Tasks[i]); err != nil {
			t.Fatal(err)
		}
	}
	views, err := s.List(ctx, 1, 7, 1)
	if err != nil || len(views[0].Takes) != 0 || views[0].ChosenTaskID != "" || len(r.Messages) != before {
		t.Fatalf("试跑、他人、错误画布不得污染片段：%+v %v", views, err)
	}
	want := errors.New("存储不可用")
	r.Err = want
	if _, err := s.List(ctx, 1, 7, 1); !errors.Is(err, want) {
		t.Fatalf("未知错误必须保留：%v", err)
	}
}

type changingSegmentSnapshotRepo struct {
	*dramasegmentfake.Repo
	writes  int
	reads   int
	advance func()
}

func (r *changingSegmentSnapshotRepo) WithTx(ctx context.Context, fn func(repository.DramaTx) error) error {
	r.writes++
	return r.Repo.WithTx(ctx, fn)
}

func (r *changingSegmentSnapshotRepo) WithSnapshotTx(ctx context.Context, fn func(repository.DramaTx) error) error {
	r.reads++
	snapshot := *r.Repo
	return fn(&changingSegmentSnapshotTx{DramaTx: &snapshot, advance: r.advance})
}

type changingSegmentSnapshotTx struct {
	repository.DramaTx
	advance func()
}

func (tx *changingSegmentSnapshotTx) Segments(ctx context.Context, user, episode uint64) ([]model.DramaSegment, error) {
	rows, err := tx.DramaTx.Segments(ctx, user, episode)
	if tx.advance != nil {
		tx.advance()
		tx.advance = nil
	}
	return rows, err
}

type segmentSnapshotAssetViews struct{}

func (segmentSnapshotAssetViews) View(context.Context, uint64, uint64) (*model.AssetView, error) {
	return nil, errors.New("asset metadata must be read through the snapshot")
}

func (segmentSnapshotAssetViews) ViewOf(_ context.Context, row *model.Asset) (*model.AssetView, error) {
	return &model.AssetView{ID: row.ID, Kind: row.Kind, DurationMs: row.DurationMs}, nil
}

func TestDramaSegmentService_ListEpisodeReadsTasksLinksAndRunsFromOneSnapshot(t *testing.T) {
	fake := dramasegmentfake.New()
	fake.SegmentVersion = 4
	chosen, canvas := uint64(21), uint64(9)
	fake.Rows = []model.DramaSegment{
		{BaseModel: model.BaseModel{ID: 101}, UserID: 1, EpisodeID: 10, Revision: 1, ChosenTaskID: &chosen},
		{BaseModel: model.BaseModel{ID: 102}, UserID: 1, EpisodeID: 10, Revision: 1, Seq: 1},
	}
	fake.Tasks = []model.GenerationTask{{
		ID: chosen, UserID: 1, Kind: model.KindVideo, CanvasProjectID: &canvas,
		NodeID: "seg:" + idcodec.Encode(101), Status: model.TaskSucceeded,
		OutputJSON: []byte(`[{"asset_id":41,"media_type":"video"}]`),
	}}
	fake.TakeLinks = []model.DramaSegmentTakeLink{{
		UserID: 1, SegmentID: 101, TaskID: chosen, Origin: model.TakeOriginNative, QC: model.TakeQCAccepted,
	}}
	fake.FrameRuns = []model.DramaSegmentFrameRun{
		{ID: 31, UserID: 1, SegmentID: 101, State: model.FrameRunStateCancelled, Revision: 1},
		{ID: 32, UserID: 1, SegmentID: 102, State: model.FrameRunStateCancelled, Revision: 1},
	}
	fake.Assets = map[uint64]model.Asset{
		41: {ID: 41, UserID: 1, Kind: model.KindVideo, DurationMs: 5000},
	}
	repo := &changingSegmentSnapshotRepo{Repo: fake}
	repo.advance = func() {
		task := fake.Tasks[0]
		task.Status, task.OutputJSON = model.TaskFailed, nil
		fake.Tasks = []model.GenerationTask{task}
		fake.TakeLinks = nil
		fake.Assets = map[uint64]model.Asset{
			41: {ID: 41, UserID: 1, Kind: model.KindVideo, DurationMs: 2500},
		}
		fake.FrameRuns = []model.DramaSegmentFrameRun{
			{ID: 33, UserID: 1, SegmentID: 101, State: model.FrameRunStateFailed, Revision: 2},
		}
	}
	svc := service.NewDramaSegmentService(service.DramaSegmentDeps{Repo: repo, Assets: segmentSnapshotAssetViews{}})
	ep, views, err := svc.ListEpisode(t.Context(), 1, 7, 1)
	if err != nil || ep.SegmentVersion != 4 || len(views) != 2 || repo.reads != 1 || repo.writes != 0 {
		t.Fatalf("expected one read-only snapshot, got %d reads and %d writes: %+v %v", repo.reads, repo.writes, views, err)
	}
	if take := views[0].Takes; len(take) != 1 || take[0].Status != model.TaskSucceeded ||
		take[0].QC != string(model.TakeQCAccepted) || take[0].Asset == nil || take[0].Asset.ID != 41 || take[0].Asset.DurationMs != 5000 {
		t.Fatalf("take details crossed snapshot boundary: %+v", take)
	}
	if views[0].FrameRun == nil || views[0].FrameRun.ID != 31 || views[0].FrameRun.Revision != 1 ||
		views[1].FrameRun == nil || views[1].FrameRun.ID != 32 {
		t.Fatalf("frame runs crossed snapshot boundary: %+v", views)
	}
}

type contendedAfterCommitRepo struct {
	*dramasegmentfake.Repo
	writes      int
	snapshotErr error
}

func (r *contendedAfterCommitRepo) WithTx(ctx context.Context, fn func(repository.DramaTx) error) error {
	r.writes++
	if r.writes > 1 {
		return errors.New("unexpected post-commit write locks")
	}
	return r.Repo.WithTx(ctx, fn)
}

func (r *contendedAfterCommitRepo) WithSnapshotTx(ctx context.Context, fn func(repository.DramaTx) error) error {
	if r.snapshotErr != nil {
		return r.snapshotErr
	}
	return r.Repo.WithSnapshotTx(ctx, fn)
}

func TestDramaSegmentService_CommittedDeleteAndOrderSurviveFollowUpListContention(t *testing.T) {
	for _, snapshotErr := range []error{nil, context.DeadlineExceeded} {
		name := "read-only projection"
		if snapshotErr != nil {
			name = "snapshot timed out"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			fake := dramasegmentfake.New()
			fake.SegmentVersion = 1
			fake.Rows = []model.DramaSegment{
				{BaseModel: model.BaseModel{ID: 101}, UserID: 1, EpisodeID: 10, Seq: 0, Seconds: 5, Prompt: "A", Revision: 1},
				{BaseModel: model.BaseModel{ID: 102}, UserID: 1, EpisodeID: 10, Seq: 1, Seconds: 4, Prompt: "B", Revision: 1},
				{BaseModel: model.BaseModel{ID: 103}, UserID: 1, EpisodeID: 10, Seq: 2, Seconds: 6, Prompt: "C", Revision: 1},
			}
			chosen, canvas := uint64(21), uint64(9)
			fake.Rows[1].ChosenTaskID = &chosen
			fake.Tasks = []model.GenerationTask{{
				ID: chosen, UserID: 1, Kind: model.KindVideo, CanvasProjectID: &canvas,
				NodeID: "seg:" + idcodec.Encode(102), Status: model.TaskSucceeded,
				OutputJSON: []byte(`[{"asset_id":41,"media_type":"video"}]`),
			}}
			fake.Assets = map[uint64]model.Asset{
				41: {ID: 41, UserID: 1, Kind: model.KindVideo, Sha256: strings.Repeat("a", 64)},
			}
			fake.FrameRuns = []model.DramaSegmentFrameRun{
				{ID: 31, UserID: 1, SegmentID: 102, State: model.FrameRunStateBound, Revision: 3},
			}
			repo := &contendedAfterCommitRepo{Repo: fake, snapshotErr: snapshotErr}
			probe := model.AssetMediaProbe{
				AssetID: 41, Sha256: fake.Assets[41].Sha256, ProbeVersion: videoframe.NormalizedProbeVersion, NormalizedFrames24: 96,
			}
			fake.MediaProbes = map[uint64]model.AssetMediaProbe{41: probe}
			probes := &segmentProbeCache{row: &probe}
			svc := service.NewDramaSegmentService(service.DramaSegmentDeps{
				Repo: repo, Assets: segmentSnapshotAssetViews{}, Broadcaster: fake, Probes: probes,
			})

			views, err := svc.Order(ctx, 1, 7, 1, &model.OrderDramaSegmentsReq{IDs: []idcodec.ID{103, 101, 102}})
			if err != nil || repo.writes != 1 {
				t.Fatalf("Order must succeed with one write transaction; got %d: %v", repo.writes, err)
			}
			if len(views) != 3 || views[0].ID != 103 || views[1].ID != 101 || views[2].ID != 102 {
				t.Fatalf("Order must return the committed membership and order: %+v", views)
			}
			for i, view := range views {
				if view.Seq != i || view.Revision != 2 || view.EpisodeID != idcodec.Encode(10) || view.SegmentVersion != 2 {
					t.Fatalf("Order returned incorrect committed row %d: %+v", i, view)
				}
			}

			assertCommittedClip(t, views[2], chosen, 31, 96)
			assertPublishedClip(t, fake, 102, 2, chosen, 31, 96)

			repo.writes = 0
			fake.Messages = nil
			if err := svc.Delete(ctx, 1, 101); err != nil || repo.writes != 1 {
				t.Fatalf("Delete must succeed with one write transaction; got %d: %v", repo.writes, err)
			}
			assertPublishedClip(t, fake, 102, 3, chosen, 31, 96)
			repo.snapshotErr = nil
			ep, after, err := svc.ListEpisode(ctx, 1, 7, 1)
			if err != nil || len(after) != 2 || after[0].ID != 103 || after[1].ID != 102 || ep.SegmentVersion != 3 {
				t.Fatalf("Delete did not preserve the committed state: ep=%+v views=%+v err=%v", ep, after, err)
			}
		})
	}
}

func assertCommittedClip(t *testing.T, v model.DramaSegmentView, chosen, run uint64, frames int32) {
	t.Helper()
	if v.ChosenTaskID != idcodec.Encode(chosen) || len(v.Takes) != 1 ||
		v.Takes[0].Asset == nil || v.Takes[0].Status != model.TaskSucceeded ||
		v.FrameRun == nil || uint64(v.FrameRun.ID) != run ||
		v.SourceFrameCount == nil || *v.SourceFrameCount != frames {
		t.Fatalf("committed clip lost its adopted take or frames: %+v", v)
	}
}

func assertPublishedClip(t *testing.T, feed *dramasegmentfake.Repo, segment uint64, version int64, chosen, run uint64, frames int32) {
	t.Helper()
	for _, msg := range feed.Messages {
		raw, err := json.Marshal(msg.Data)
		if err != nil {
			t.Fatal(err)
		}
		var event struct {
			Segment model.DramaSegmentView `json:"segment"`
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatal(err)
		}
		if uint64(event.Segment.ID) == segment && event.Segment.SegmentVersion == version {
			assertCommittedClip(t, event.Segment, chosen, run, frames)
			return
		}
	}
	t.Fatalf("no event for clip %d at version %d", segment, version)
}

type segmentTakeReadFailureRepo struct {
	*dramasegmentfake.Repo
	err error
}

func (r segmentTakeReadFailureRepo) WithTx(ctx context.Context, fn func(repository.DramaTx) error) error {
	return r.Repo.WithTx(ctx, func(tx repository.DramaTx) error {
		return fn(failingSegmentTakeReadTx{DramaTx: tx, err: r.err})
	})
}

type failingSegmentTakeReadTx struct {
	repository.DramaTx
	err error
}

func (tx failingSegmentTakeReadTx) SegmentTasks(context.Context, uint64, uint64, string) ([]model.GenerationTask, error) {
	return nil, tx.err
}

func TestDramaSegmentService_ProjectionFailureRollsBackWithoutBroadcast(t *testing.T) {
	for _, method := range []string{"order", "delete"} {
		t.Run(method, func(t *testing.T) {
			ctx := t.Context()
			fake := matrixTakeRepo(takeMatrixCase{chosen: true})
			fake.Rows = append(fake.Rows, model.DramaSegment{
				BaseModel: model.BaseModel{ID: 2}, UserID: 1, EpisodeID: 10, Seq: 1, Revision: 1,
			})
			failure := errors.New("take query unavailable")
			svc := service.NewDramaSegmentService(service.DramaSegmentDeps{
				Repo: segmentTakeReadFailureRepo{Repo: fake, err: failure}, Assets: fake, Broadcaster: fake,
			})
			var err error
			if method == "order" {
				_, err = svc.Order(ctx, 1, 7, 1, &model.OrderDramaSegmentsReq{IDs: []idcodec.ID{2, 1}})
			} else {
				err = svc.Delete(ctx, 1, 2)
			}
			if !errors.Is(err, failure) || len(fake.Messages) != 0 || len(fake.Rows) != 2 ||
				fake.Rows[0].ID != 1 || fake.Rows[0].Seq != 0 || fake.SegmentVersion != 17 {
				t.Fatalf("incomplete projection must not commit or publish: rows=%+v events=%v err=%v", fake.Rows, fake.Messages, err)
			}
		})
	}
}

func TestDramaSegmentService_RecoveryNeverUsesOuterAssetReader(t *testing.T) {
	fake := matrixTakeRepo(takeMatrixCase{adopt: true})
	svc := service.NewDramaSegmentService(service.DramaSegmentDeps{
		Repo: fake, Assets: segmentSnapshotAssetViews{},
	})
	ep, clips, err := svc.ListEpisode(t.Context(), 1, 7, 1)
	if err != nil || len(clips) != 1 || clips[0].ChosenTaskID != idcodec.Encode(21) ||
		clips[0].FrameRun == nil || clips[0].FrameRun.State != model.FrameRunStateBound ||
		ep.SegmentVersion != 18 {
		t.Fatalf("recovery must adopt the take and bind the run using the transaction's asset reader: %+v %v", clips, err)
	}
}
