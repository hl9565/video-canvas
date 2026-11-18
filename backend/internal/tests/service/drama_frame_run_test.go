package service_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"video-canvas/internal/config"
	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/provider"
	"video-canvas/internal/provider/modelcfg"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
	"video-canvas/internal/service/dramasegmentfake"
)

func framePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewNRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func frameI2VSnapshot() *provider.Snapshot {
	snap := fakeTaskSnapshot(model.KindVideo, 10)
	snap.Model.Key = service.DefaultDramaI2VModelKey
	snap.Model.Capabilities.Params = modelcfg.ParamSet{
		{Name: "duration", ParamField: modelcfg.ParamField{Type: modelcfg.ParamEnum, Open: true, Options: []any{5.0, 10.0}, Default: 5.0}},
		{Name: "resolution", ParamField: modelcfg.ParamField{Type: modelcfg.ParamEnum, Open: true, Options: []any{"720p", "1080p"}, Default: "720p"}},
		{Name: "aspect_ratio", ParamField: modelcfg.ParamField{Type: modelcfg.ParamEnum, Open: true, Options: []any{"16:9", "9:16"}, Default: "16:9"}},
		{Name: "count", ParamField: modelcfg.ParamField{Type: modelcfg.ParamEnum, Open: true, Options: []any{1.0, 2.0}, Default: 1.0, Fanout: true}},
	}
	return snap
}

func TestFrameRunConfirmAndReplace(t *testing.T) {
	ctx := context.Background()
	assets, _, _ := newAssetTestService(config.Storage{})
	repo := dramasegmentfake.New()
	repo.Rows = []model.DramaSegment{{BaseModel: model.BaseModel{ID: 1}, UserID: 1, EpisodeID: 10, Seconds: 5, Revision: 1}}
	registry := &fakeTaskRegistry{snap: frameI2VSnapshot()}
	tasks := service.NewGenerationTaskService(service.GenerationTaskDeps{Repo: newFakeTaskRepo(), Registry: registry, Assets: assets, QuoteSecret: "frame-test"})
	segments := service.NewDramaSegmentService(service.DramaSegmentDeps{Repo: repo, Assets: assets, Broadcaster: repo})
	svc := service.NewDramaFrameRunService(service.DramaFrameRunDeps{Repo: repo, Assets: assets, Registry: registry, Tasks: tasks, Segments: segments, Broadcaster: repo})
	a, err := assets.SaveDerivedImage(ctx, 1, "source.png", "image/png", framePNG(t, 160, 160))
	if err != nil {
		t.Fatal(err)
	}
	id := model.AssetID(a.ID)
	v, _, err := svc.Create(ctx, 1, 1, &model.CreateFrameRunReq{RequestKey: "confirm", SegmentRevision: 1, FrameSource: model.FrameSourceExisting, FrameAssetID: &id})
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"prompt": "I2V", "op": "i2v", "duration": 5, "resolution": "720p", "aspect_ratio": "16:9", "count": 1, "images": []uint64{uint64(*v.FrameAssetID)}}
	quote, err := tasks.Quote(ctx, 1, &model.QuoteGenerationTaskReq{Kind: model.KindVideo, ModelID: registry.snap.Model.Key, CanvasID: 9, NodeID: "seg:" + idcodec.Encode(1), Input: input}, service.QuoteTaskOptions{SegmentID: 1, SegmentRevision: 1, SingleShotOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	req := &model.ConfirmFrameRunReq{Revision: v.Revision, FrameSHA256: v.FrameSHA256, Input: input, QuoteToken: quote.QuoteToken}
	v, err = svc.Confirm(ctx, 1, 1, uint64(v.ID), req)
	if err != nil || v.State != model.FrameRunStateConfirmed || v.ConfirmedVersion != 1 || v.ModelKey != service.DefaultDramaI2VModelKey {
		t.Fatalf("确认: %+v %v", v, err)
	}
	req.Revision, req.FrameSHA256 = v.Revision, strings.Repeat("0", 64)
	_, err = svc.Confirm(ctx, 1, 1, uint64(v.ID), req)
	assertTaskCode(t, err, errcode.ErrDramaFrameInvalid.Code)
	v, err = svc.SetFrame(ctx, 1, 1, uint64(v.ID), &model.SetFrameRunFrameReq{Revision: v.Revision, FrameAssetID: id})
	if err != nil || v.State != model.FrameRunStateFrameReady || v.ConfirmedVersion != 1 || v.ModelKey != "" || len(v.ConfirmedInput) != 0 {
		t.Fatalf("换图未作废确认或重置了单调版本: %+v %v", v, err)
	}
	stored := repo.FrameRuns[0]
	if stored.QuoteToken != nil || stored.ConfirmedInputHash != nil || stored.VideoIdempotencyKey != "" || stored.SubmittingAt != nil {
		t.Fatalf("旧确认仍可提交: %+v", stored)
	}
}

type frameRunTasks struct {
	*service.GenerationTaskService
	repo                               *dramasegmentfake.Repo
	createErr, submitErr, cancelErr    error
	createHook, submitHook, cancelHook func()
	createCalls, submitCalls           int
	cancelled                          []uint64
}

func frameTaskView(task *model.GenerationTask) *model.GenerationTaskView {
	return &model.GenerationTaskView{ID: task.ID, CanvasProjectID: frameID(task.CanvasProjectID), NodeID: task.NodeID, Kind: task.Kind, Status: task.Status}
}

func frameID(value *uint64) *idcodec.ID {
	if value == nil {
		return nil
	}
	id := idcodec.ID(*value)
	return &id
}

func (f *frameRunTasks) outsideTx() {
	if f.repo.InTx {
		panic("task I/O while holding drama locks")
	}
}

func (f *frameRunTasks) Create(ctx context.Context, userID uint64, key string, req *model.CreateGenerationTaskReq) (*model.CreateGenerationTaskResp, error) {
	f.outsideTx()
	f.createCalls++
	if f.createHook != nil {
		f.createHook()
	}
	if f.createErr != nil {
		return nil, f.createErr
	}
	if task, err := f.FindByIdempotencyKey(ctx, userID, key); err == nil {
		return &model.CreateGenerationTaskResp{Items: []model.CreateTaskItem{{Task: frameTaskView(task)}}}, nil
	}
	id := uint64(200 + len(f.repo.Tasks))
	canvas := uint64(req.CanvasID)
	raw, _ := json.Marshal(req.Input)
	task := model.GenerationTask{ID: id, UserID: userID, CanvasProjectID: &canvas, NodeID: req.NodeID, Kind: req.Kind,
		ModelKey: req.ModelID, IdempotencyKey: key, InputJSON: raw, Status: model.TaskPending}
	f.repo.Tasks = append(f.repo.Tasks, task)
	return &model.CreateGenerationTaskResp{Items: []model.CreateTaskItem{{Task: frameTaskView(&task)}}}, nil
}

func (f *frameRunTasks) CreateWithOptions(ctx context.Context, userID uint64, key string, req *model.CreateGenerationTaskReq, opt service.CreateTaskOptions) (*model.CreateGenerationTaskResp, error) {
	f.outsideTx()
	f.submitCalls++
	if f.submitHook != nil {
		f.submitHook()
	}
	run, err := f.repo.FrameRun(ctx, userID, opt.DramaRunBind.RunID, false)
	if err != nil {
		return nil, err
	}
	if run.State == model.FrameRunStateCancelled || run.State == model.FrameRunStateFailed {
		return nil, errcode.ErrDramaFrameRunConflict
	}
	if task, err := f.FindByIdempotencyKey(ctx, userID, key); err == nil {
		if task.FrameRunID == nil || *task.FrameRunID != run.ID || task.FrameRunConfirmRev == nil || *task.FrameRunConfirmRev != opt.DramaRunBind.ConfirmRev {
			return nil, errcode.ErrIdempotencyConflict
		}
		return &model.CreateGenerationTaskResp{Items: []model.CreateTaskItem{{Task: frameTaskView(task)}}}, nil
	}
	if f.submitErr != nil {
		return nil, f.submitErr
	}
	resp, err := f.Create(ctx, userID, key, req)
	if err != nil {
		return nil, err
	}
	task := &f.repo.Tasks[len(f.repo.Tasks)-1]
	task.FrameRunID, task.FrameRunConfirmRev = &run.ID, &run.ConfirmedVersion
	run.VideoTaskID, run.State, run.SubmittingAt = &task.ID, model.FrameRunStateVideoPending, nil
	if err := f.repo.SaveFrameRun(ctx, run, run.Revision); err != nil {
		return nil, err
	}
	return resp, nil
}

func (f *frameRunTasks) FindByIdempotencyKey(_ context.Context, userID uint64, key string) (*model.GenerationTask, error) {
	f.outsideTx()
	for _, task := range f.repo.Tasks {
		if task.UserID == userID && task.IdempotencyKey == key {
			return &task, nil
		}
	}
	return nil, repository.ErrNotFound
}

func (f *frameRunTasks) Cancel(_ context.Context, _ uint64, taskID uint64) (*model.GenerationTaskView, error) {
	f.outsideTx()
	f.cancelled = append(f.cancelled, taskID)
	if f.cancelHook != nil {
		f.cancelHook()
	}
	if f.cancelErr != nil {
		return nil, f.cancelErr
	}
	for i := range f.repo.Tasks {
		if f.repo.Tasks[i].ID == taskID {
			f.repo.Tasks[i].Status = model.TaskCanceled
			return frameTaskView(&f.repo.Tasks[i]), nil
		}
	}
	return nil, errcode.ErrTaskNotFound
}

type frameGuardAssets struct {
	*service.AssetService
	repo     *dramasegmentfake.Repo
	openHook func()
}

func (a *frameGuardAssets) Get(ctx context.Context, userID, id uint64) (*model.Asset, error) {
	if a.repo.InTx {
		panic("asset I/O while holding drama locks")
	}
	return a.AssetService.Get(ctx, userID, id)
}

func (a *frameGuardAssets) Open(ctx context.Context, userID, id uint64) (*provider.AssetFile, error) {
	if a.repo.InTx {
		panic("storage I/O while holding drama locks")
	}
	if a.openHook != nil {
		a.openHook()
	}
	return a.AssetService.Open(ctx, userID, id)
}

func (a *frameGuardAssets) SaveDerivedImage(ctx context.Context, userID uint64, name, mime string, data []byte) (*model.Asset, error) {
	if a.repo.InTx {
		panic("derived upload while holding drama locks")
	}
	return a.AssetService.SaveDerivedImage(ctx, userID, name, mime, data)
}

// frameRunSegmentRepo 让片段快照和素材服务读取同一份素材数据。
type frameRunSegmentRepo struct {
	*dramasegmentfake.Repo
	assets *fakeAssetRepo
}

// WithSnapshotTx 将当前素材行复制到快照，模拟生产环境在同一事务内读多张表。
func (r frameRunSegmentRepo) WithSnapshotTx(ctx context.Context, fn func(repository.DramaTx) error) error {
	snapshot := *r.Repo
	snapshot.Assets = make(map[uint64]model.Asset, len(r.assets.rows))
	for id, asset := range r.assets.rows {
		snapshot.Assets[id] = *asset
	}
	return snapshot.WithSnapshotTx(ctx, fn)
}

type frameRunEnv struct {
	repo      *dramasegmentfake.Repo
	assets    *frameGuardAssets
	assetRepo *fakeAssetRepo
	store     *fakeAssetStore
	registry  *fakeTaskRegistry
	tasks     *frameRunTasks
	segments  *service.DramaSegmentService
	svc       *service.DramaFrameRunService
	now       time.Time
}

func newFrameRunEnv(t *testing.T) *frameRunEnv {
	t.Helper()
	assets, assetRepo, store := newAssetTestService(config.Storage{})
	repo := dramasegmentfake.New()
	repo.Rows = []model.DramaSegment{{BaseModel: model.BaseModel{ID: 1}, UserID: 1, EpisodeID: 10, Seconds: 5, Revision: 1}}
	e := &frameRunEnv{repo: repo, assets: &frameGuardAssets{AssetService: assets, repo: repo}, assetRepo: assetRepo, store: store, registry: &fakeTaskRegistry{snap: frameI2VSnapshot()}, now: time.Now()}
	e.tasks = &frameRunTasks{repo: repo, GenerationTaskService: service.NewGenerationTaskService(service.GenerationTaskDeps{
		Repo: newFakeTaskRepo(), Registry: e.registry, Assets: e.assets, QuoteSecret: "frame-tests", FrameRunGuard: repo}, service.WithTaskClock(func() time.Time { return e.now }))}
	e.segments = service.NewDramaSegmentService(service.DramaSegmentDeps{Repo: frameRunSegmentRepo{Repo: repo, assets: assetRepo}, Tasks: e.tasks, Assets: e.assets, Canvases: repo, Broadcaster: repo})
	e.svc = service.NewDramaFrameRunService(service.DramaFrameRunDeps{Repo: repo, Assets: e.assets, Tasks: e.tasks, Registry: e.registry,
		Canvases: repo, Segments: e.segments, Broadcaster: repo, TicketSecret: "ticket-test"}, service.WithDramaFrameRunClock(func() time.Time { return e.now }))
	e.segments.SetFrameRunEnricher(e.svc)
	return e
}

func frameAssetID(value *uint64) *model.AssetID {
	if value == nil {
		return nil
	}
	id := model.AssetID(*value)
	return &id
}

func (e *frameRunEnv) image(t *testing.T, w, h int) model.AssetID {
	t.Helper()
	a, err := e.assets.SaveDerivedImage(context.Background(), 1, "image.png", "image/png", framePNG(t, w, h))
	if err != nil {
		t.Fatal(err)
	}
	return model.AssetID(a.ID)
}

func (e *frameRunEnv) create(t *testing.T, source model.DramaFrameSource) *model.DramaSegmentFrameRunView {
	t.Helper()
	req := &model.CreateFrameRunReq{RequestKey: "run", SegmentRevision: 1, FrameSource: source}
	if source == model.FrameSourceExisting {
		id := e.image(t, 160, 90)
		req.FrameAssetID = &id
	}
	if source == model.FrameSourceGenerate {
		req.ModelID, req.Input = "image-model", map[string]any{"prompt": "frame"}
	}
	v, _, err := e.svc.Create(context.Background(), 1, 1, req)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (e *frameRunEnv) confirmReq(t *testing.T, v *model.DramaSegmentFrameRunView) *model.ConfirmFrameRunReq {
	t.Helper()
	input := map[string]any{"prompt": "video", "duration": 5, "resolution": "720p", "op": "i2v", "aspect_ratio": "16:9", "count": 1, "images": []uint64{uint64(*v.FrameAssetID)}}
	q, err := e.tasks.Quote(context.Background(), 1, &model.QuoteGenerationTaskReq{Kind: model.KindVideo, ModelID: service.DefaultDramaI2VModelKey,
		CanvasID: 9, NodeID: "seg:" + idcodec.Encode(1), Input: input}, service.QuoteTaskOptions{SegmentID: 1, SegmentRevision: 1, SingleShotOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	return &model.ConfirmFrameRunReq{Revision: v.Revision, FrameSHA256: v.FrameSHA256, Input: input, QuoteToken: q.QuoteToken}
}

func (e *frameRunEnv) confirmed(t *testing.T) *model.DramaSegmentFrameRunView {
	t.Helper()
	v := e.create(t, model.FrameSourceExisting)
	v, err := e.svc.Confirm(context.Background(), 1, 1, uint64(v.ID), e.confirmReq(t, v))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestFrameRunSubmitAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
		state   model.DramaFrameRunState
	}{
		{"happy", nil, model.FrameRunStateVideoPending}, {"expired", errcode.ErrDramaQuoteExpired, model.FrameRunStateFrameReady},
		{"quota", errcode.ErrInsufficientCredits, model.FrameRunStateFailed}, {"disconnect", errors.New("connection reset"), model.FrameRunStateSubmitting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFrameRunEnv(t)
			v := e.confirmed(t)
			e.tasks.submitErr = tc.failure
			req := &model.SubmitFrameRunReq{Revision: v.Revision, ConfirmedVersion: v.ConfirmedVersion}
			view, task, err := e.svc.Submit(context.Background(), 1, 1, uint64(v.ID), req)
			if (err != nil) != (tc.failure != nil) {
				t.Fatalf("submit: %v", err)
			}
			if e.repo.FrameRuns[0].State != tc.state {
				t.Fatalf("state=%s", e.repo.FrameRuns[0].State)
			}
			if tc.failure != nil {
				if len(e.repo.Tasks) != 0 {
					t.Fatal("failed submit left partial task")
				}
				if tc.state == model.FrameRunStateFrameReady && (e.repo.FrameRuns[0].SubmittingAt != nil || e.repo.FrameRuns[0].QuoteToken != nil) {
					t.Fatal("quote failure not reset")
				}
				return
			}
			if task == nil || view.VideoTaskID == nil || uint64(*view.VideoTaskID) != task.ID {
				t.Fatalf("missing binding: %+v %+v", view, task)
			}
			e.now = e.now.Add(time.Hour)
			replay, again, err := e.svc.Submit(context.Background(), 1, 1, uint64(v.ID), req)
			if err != nil || replay.ID != view.ID || again.ID != task.ID || len(e.repo.Tasks) != 1 {
				t.Fatalf("expired replay: %+v %v", replay, err)
			}
		})
	}
}

func TestFrameRunCanvasTicketAndEnrichedViews(t *testing.T) {
	e := newFrameRunEnv(t)
	ctx := context.Background()
	v := e.create(t, model.FrameSourceCanvas)
	if v.State != model.FrameRunStateDraft || v.OriginCanvasID == nil || *v.OriginCanvasID != 9 || v.CanvasTicket == "" {
		t.Fatalf("canvas draft: %+v", v)
	}
	assertTicket := func(v *model.DramaSegmentView) {
		t.Helper()
		if v.FrameRun == nil || v.FrameRun.CanvasTicket == "" {
			t.Fatalf("ticket omitted: %+v", v)
		}
	}
	seg, err := e.segments.Get(ctx, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	assertTicket(seg)
	list, err := e.segments.List(ctx, 1, 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	assertTicket(&list[0])
	raw, _ := json.Marshal(e.repo.Messages[len(e.repo.Messages)-1].Data)
	if !bytes.Contains(raw, []byte(`"canvas_ticket"`)) {
		t.Fatalf("WS ticket omitted: %s", raw)
	}
	id := e.image(t, 160, 160)
	req := &model.SetFrameRunFrameReq{Revision: v.Revision, FrameAssetID: id, CanvasTicket: v.CanvasTicket + "x"}
	_, err = e.svc.SetFrame(ctx, 1, 1, uint64(v.ID), req)
	assertTaskCode(t, err, 91008)
	req.CanvasTicket = v.CanvasTicket
	origin := idcodec.ID(999)
	req.OriginCanvasID = &origin
	_, err = e.svc.SetFrame(ctx, 1, 1, uint64(v.ID), req)
	assertTaskCode(t, err, 91008)
	req.OriginCanvasID = nil
	e.now = e.now.Add(31 * time.Minute)
	_, err = e.svc.SetFrame(ctx, 1, 1, uint64(v.ID), req)
	assertTaskCode(t, err, 91008)
	e.now = e.now.Add(-31 * time.Minute)
	e.repo.CanvasDeleted = true
	_, err = e.svc.SetFrame(ctx, 1, 1, uint64(v.ID), req)
	assertTaskCode(t, err, 91008)
	e.repo.CanvasDeleted = false
	v, err = e.svc.SetFrame(ctx, 1, 1, uint64(v.ID), req)
	if err != nil || v.FrameSHA256 == "" || *v.FrameAssetID == uint64(id) {
		t.Fatalf("canvas crop: %+v %v", v, err)
	}
	seg, err = e.segments.Get(ctx, 1, 1)
	if err != nil || seg.FrameRun.FrameSHA256 != v.FrameSHA256 {
		t.Fatalf("segment SHA: %+v %v", seg, err)
	}
	raw, _ = json.Marshal(e.repo.Messages[len(e.repo.Messages)-1].Data)
	if !bytes.Contains(raw, []byte(v.FrameSHA256)) {
		t.Fatalf("WS SHA missing: %s", raw)
	}
	oldTicket := v.CanvasTicket
	v, err = e.svc.Cancel(ctx, 1, 1, uint64(v.ID), &model.CancelFrameRunReq{Revision: v.Revision})
	if err != nil || v.CanvasTicket != "" {
		t.Fatalf("terminal ticket: %+v %v", v, err)
	}
	v, _, err = e.svc.Create(ctx, 1, 1, &model.CreateFrameRunReq{RequestKey: "next", SegmentRevision: 1, FrameSource: model.FrameSourceCanvas})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.SetFrame(ctx, 1, 1, uint64(v.ID), &model.SetFrameRunFrameReq{Revision: v.Revision, FrameAssetID: id, CanvasTicket: oldTicket})
	assertTaskCode(t, err, 91008)
}

func TestFrameRunSetFrameStateAndCAS(t *testing.T) {
	for _, state := range []model.DramaFrameRunState{model.FrameRunStateDraft, model.FrameRunStateFramePending, model.FrameRunStateFrameReady, model.FrameRunStateConfirmed,
		model.FrameRunStateSubmitting, model.FrameRunStateVideoPending, model.FrameRunStateBound, model.FrameRunStateFailed, model.FrameRunStateCancelled} {
		t.Run(string(state), func(t *testing.T) {
			e := newFrameRunEnv(t)
			v := e.create(t, model.FrameSourceExisting)
			id := e.image(t, 160, 90)
			e.repo.FrameRuns[0].State = state
			e.repo.FrameRuns[0].ConfirmedVersion = 4
			got, err := e.svc.SetFrame(context.Background(), 1, 1, uint64(v.ID), &model.SetFrameRunFrameReq{Revision: v.Revision, FrameAssetID: id})
			allowed := state == model.FrameRunStateDraft || state == model.FrameRunStateFramePending || state == model.FrameRunStateFrameReady || state == model.FrameRunStateConfirmed
			if !allowed {
				assertTaskCode(t, err, 91005)
				return
			}
			if err != nil || got.State != model.FrameRunStateFrameReady || got.ConfirmedVersion != 4 {
				t.Fatalf("set state: %+v %v", got, err)
			}
			_, err = e.svc.SetFrame(context.Background(), 1, 1, uint64(v.ID), &model.SetFrameRunFrameReq{Revision: v.Revision, FrameAssetID: id})
			assertTaskCode(t, err, errcode.ErrCanvasConflict.Code)
		})
	}
}

func TestFrameRunConfirmGuards(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*frameRunEnv, *model.ConfirmFrameRunReq)
		code   int
	}{
		{"model override", func(_ *frameRunEnv, r *model.ConfirmFrameRunReq) { r.ModelID = "other" }, 10001},
		{"model unavailable", func(e *frameRunEnv, _ *model.ConfirmFrameRunReq) { e.registry.snapErr = provider.ErrModelUnavailable }, 91002},
		{"model kind", func(e *frameRunEnv, _ *model.ConfirmFrameRunReq) { e.registry.snap.Model.Kind = model.KindImage }, 10001},
		{"no I2V", func(e *frameRunEnv, _ *model.ConfirmFrameRunReq) {
			e.registry.snap.Model.Capabilities.Ops = []string{"t2v"}
		}, 10001},
		{"refs disabled", func(e *frameRunEnv, _ *model.ConfirmFrameRunReq) {
			e.registry.snap.Model.Capabilities.Refs.Image.On = false
		}, 10001},
		{"refs zero", func(e *frameRunEnv, _ *model.ConfirmFrameRunReq) {
			e.registry.snap.Model.Capabilities.Refs.Image.Max = 0
		}, 10001},
		{"frame mismatch", func(_ *frameRunEnv, r *model.ConfirmFrameRunReq) { id := model.AssetID(999); r.FrameAssetID = &id }, 91008},
		{"SHA mismatch", func(_ *frameRunEnv, r *model.ConfirmFrameRunReq) { r.FrameSHA256 = strings.Repeat("a", 64) }, 91008},
		{"op override", func(_ *frameRunEnv, r *model.ConfirmFrameRunReq) { r.Input["op"] = "t2v" }, 10001},
		{"missing prompt", func(_ *frameRunEnv, r *model.ConfirmFrameRunReq) { r.Input["prompt"] = " " }, 40006},
		{"missing duration", func(_ *frameRunEnv, r *model.ConfirmFrameRunReq) { delete(r.Input, "duration") }, 40006},
		{"fractional duration", func(_ *frameRunEnv, r *model.ConfirmFrameRunReq) { r.Input["duration"] = 5.1 }, 40006},
		{"zero duration", func(_ *frameRunEnv, r *model.ConfirmFrameRunReq) { r.Input["duration"] = 0 }, 40006},
		{"invalid resolution", func(_ *frameRunEnv, r *model.ConfirmFrameRunReq) { r.Input["resolution"] = "4k" }, 40006},
		{"missing resolution", func(_ *frameRunEnv, r *model.ConfirmFrameRunReq) { delete(r.Input, "resolution") }, 40006},
		{"quote mismatch", func(_ *frameRunEnv, r *model.ConfirmFrameRunReq) { r.Input["duration"] = 10 }, 91002},
		{"quote tamper", func(_ *frameRunEnv, r *model.ConfirmFrameRunReq) { r.QuoteToken += "x" }, 91002},
		{"quote expiry", func(e *frameRunEnv, _ *model.ConfirmFrameRunReq) { e.now = e.now.Add(time.Hour) }, 91002},
		{"timeline", func(_ *frameRunEnv, r *model.ConfirmFrameRunReq) { r.Input["prompt"] = "0-9秒：奔跑" }, 40006},
		{"stale segment", func(_ *frameRunEnv, r *model.ConfirmFrameRunReq) { rev := int64(2); r.SegmentRevision = &rev }, 30002},
		{"stale run", func(_ *frameRunEnv, r *model.ConfirmFrameRunReq) { r.Revision++ }, 91005},
		{"corrupt image", func(e *frameRunEnv, _ *model.ConfirmFrameRunReq) {
			a := e.assetRepo.rows[*e.repo.FrameRuns[0].FrameAssetID]
			e.store.objects[a.StorageKey] = []byte("corrupt")
		}, 91008},
		{"wrong aspect bytes", func(e *frameRunEnv, _ *model.ConfirmFrameRunReq) {
			a := e.assetRepo.rows[*e.repo.FrameRuns[0].FrameAssetID]
			e.store.objects[a.StorageKey] = framePNG(t, 160, 160)
		}, 91008},
		{"state changed", func(e *frameRunEnv, _ *model.ConfirmFrameRunReq) {
			e.repo.FrameRuns[0].State = model.FrameRunStateSubmitting
		}, 91005},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFrameRunEnv(t)
			v := e.create(t, model.FrameSourceExisting)
			req := e.confirmReq(t, v)
			tc.change(e, req)
			_, err := e.svc.Confirm(context.Background(), 1, 1, uint64(v.ID), req)
			assertTaskCode(t, err, tc.code)
			if e.repo.FrameRuns[0].ConfirmedVersion != 0 {
				t.Fatal("failed confirmation persisted")
			}
		})
	}
}

func TestFrameRunMonotonicConfirmAndDrift(t *testing.T) {
	e := newFrameRunEnv(t)
	ctx := context.Background()
	v := e.confirmed(t)
	firstKey := e.repo.FrameRuns[0].VideoIdempotencyKey
	v, err := e.svc.Confirm(ctx, 1, 1, uint64(v.ID), e.confirmReq(t, v))
	if err != nil {
		t.Fatal(err)
	}
	id := e.image(t, 320, 180)
	v, err = e.svc.SetFrame(ctx, 1, 1, uint64(v.ID), &model.SetFrameRunFrameReq{Revision: v.Revision, FrameAssetID: id})
	if err != nil {
		t.Fatal(err)
	}
	v, err = e.svc.Confirm(ctx, 1, 1, uint64(v.ID), e.confirmReq(t, v))
	if err != nil || v.ConfirmedVersion != 3 || e.repo.FrameRuns[0].VideoIdempotencyKey == firstKey || !strings.HasSuffix(e.repo.FrameRuns[0].VideoIdempotencyKey, ":v3") {
		t.Fatalf("monotonic: %+v %v", v, err)
	}
	req := e.confirmReq(t, v)
	e.assets.openHook = func() {
		e.assets.openHook = nil
		e.repo.FrameRuns[0].FrameAssetID = new(uint64(id))
		e.repo.FrameRuns[0].Revision++
	}
	_, err = e.svc.Confirm(ctx, 1, 1, uint64(v.ID), req)
	assertTaskCode(t, err, 91005)
}

func TestFrameRunCancelOrdinaryTaskUntouched_Service(t *testing.T) {
	for _, rev := range []int64{0, 7} {
		t.Run(fmt.Sprint(rev), func(t *testing.T) {
			e := newFrameRunEnv(t)
			v := e.confirmed(t)
			run := &e.repo.FrameRuns[0]
			run.State = model.FrameRunStateSubmitting
			canvas := uint64(9)
			task := model.GenerationTask{ID: 999, UserID: 1, Kind: model.KindVideo, CanvasProjectID: &canvas, NodeID: "seg:" + idcodec.Encode(1), IdempotencyKey: run.VideoIdempotencyKey, Status: model.TaskPending}
			if rev > 0 {
				task.FrameRunID = &run.ID
				task.FrameRunConfirmRev = &rev
			}
			e.repo.Tasks = append(e.repo.Tasks, task)
			before := e.repo.Tasks[0]
			cancelled, err := e.svc.Cancel(context.Background(), 1, 1, uint64(v.ID), &model.CancelFrameRunReq{Revision: v.Revision})
			if err != nil || cancelled.State != model.FrameRunStateCancelled || len(e.tasks.cancelled) != 0 || !reflect.DeepEqual(e.repo.Tasks[0], before) {
				t.Fatalf("ordinary task touched: %+v %v", cancelled, err)
			}
		})
	}
}

func TestFrameRunCancelThenLateTaskNoBound_Service(t *testing.T) {
	e := newFrameRunEnv(t)
	ctx := context.Background()
	v := e.confirmed(t)
	v, task, err := e.svc.Submit(ctx, 1, 1, uint64(v.ID), &model.SubmitFrameRunReq{Revision: v.Revision, ConfirmedVersion: v.ConfirmedVersion})
	if err != nil {
		t.Fatal(err)
	}
	e.repo.Tasks[0].Status = model.TaskFinalizing
	e.tasks.cancelErr = errcode.ErrTaskNotCancelable
	version := e.repo.SegmentVersion
	v, err = e.svc.Cancel(ctx, 1, 1, uint64(v.ID), &model.CancelFrameRunReq{Revision: v.Revision})
	if err != nil || v.State != model.FrameRunStateCancelled || len(e.tasks.cancelled) != 1 {
		t.Fatalf("cancel finalizing: %+v %v", v, err)
	}
	a := &model.Asset{ID: 999, UserID: 1, Kind: model.KindVideo, Sha256: strings.Repeat("b", 64)}
	e.assetRepo.rows[a.ID] = a
	out, _ := json.Marshal([]model.TaskOutput{{AssetID: a.ID, MediaType: model.KindVideo}})
	e.repo.Tasks[0].Status, e.repo.Tasks[0].OutputJSON = model.TaskSucceeded, out
	if err := e.svc.OnTaskChanged(ctx, &e.repo.Tasks[0]); err != nil {
		t.Fatal(err)
	}
	if err := e.segments.OnTaskChanged(ctx, &e.repo.Tasks[0]); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RecoverUnsettledDramaFrameRuns(ctx, repository.FrameRunRecoveryFilter{}, 20); err != nil {
		t.Fatal(err)
	}
	seg, err := e.segments.Get(ctx, 1, 1)
	if err != nil || len(seg.Takes) != 0 || seg.ChosenTaskID != "" || seg.FrameRun.State != model.FrameRunStateCancelled || len(e.repo.TakeLinks) != 0 || e.repo.SegmentVersion != version {
		t.Fatalf("late task %d adopted: %+v %v", task.ID, seg, err)
	}
	_, _, err = e.svc.Submit(ctx, 1, 1, uint64(v.ID), &model.SubmitFrameRunReq{Revision: v.Revision, ConfirmedVersion: v.ConfirmedVersion})
	assertTaskCode(t, err, 91005)
}

func TestFrameRunCancelCASAndTaskSafety(t *testing.T) {
	for _, state := range []model.DramaFrameRunState{model.FrameRunStateFramePending, model.FrameRunStateSubmitting, model.FrameRunStateVideoPending, model.FrameRunStateBound, model.FrameRunStateFailed} {
		t.Run(string(state), func(t *testing.T) {
			e := newFrameRunEnv(t)
			ctx := context.Background()
			v := e.confirmed(t)
			run := &e.repo.FrameRuns[0]
			run.State = state
			canvas := uint64(9)
			task := model.GenerationTask{ID: 999, UserID: 1, Kind: model.KindVideo, Status: model.TaskPending, CanvasProjectID: &canvas,
				IdempotencyKey: run.VideoIdempotencyKey, FrameRunID: &run.ID, FrameRunConfirmRev: &run.ConfirmedVersion}
			if state == model.FrameRunStateFramePending {
				task.Kind = model.KindImage
				run.FrameTaskID = &task.ID
			}
			if state == model.FrameRunStateVideoPending {
				run.VideoTaskID = &task.ID
			}
			e.repo.Tasks = append(e.repo.Tasks, task)
			e.tasks.cancelHook = func() {
				if e.repo.FrameRuns[0].State != model.FrameRunStateCancelled {
					t.Fatal("task cancelled before run commit")
				}
			}
			got, err := e.svc.Cancel(ctx, 1, 1, uint64(v.ID), &model.CancelFrameRunReq{Revision: v.Revision})
			if state == model.FrameRunStateBound || state == model.FrameRunStateFailed {
				assertTaskCode(t, err, 91005)
				return
			}
			if err != nil || len(e.tasks.cancelled) != 1 {
				t.Fatalf("missing cancellation: %+v %v", got, err)
			}
			again, err := e.svc.Cancel(ctx, 1, 1, uint64(v.ID), &model.CancelFrameRunReq{Revision: got.Revision})
			if err != nil || again.Revision != got.Revision {
				t.Fatalf("cancel not idempotent: %+v %v", again, err)
			}
			_, err = e.svc.Cancel(ctx, 1, 1, uint64(v.ID), &model.CancelFrameRunReq{Revision: v.Revision})
			assertTaskCode(t, err, 30002)
		})
	}
}

func TestFrameRunImageObservationAndRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		valid        bool
		state        model.DramaFrameRunState
	}{
		{"pending", model.TaskPending, true, model.FrameRunStateFramePending},
		{"succeeded", model.TaskSucceeded, true, model.FrameRunStateFrameReady},
		{"failed", model.TaskFailed, true, model.FrameRunStateFailed},
		{"cancelled", model.TaskCanceled, true, model.FrameRunStateFailed},
		{"invalid", model.TaskSucceeded, false, model.FrameRunStateFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFrameRunEnv(t)
			v := e.create(t, model.FrameSourceGenerate)
			if v.FrameTaskID == nil || e.repo.Tasks[0].NodeID != "seg-frame:"+idcodec.Encode(1) || e.repo.Tasks[0].Kind != model.KindImage {
				t.Fatalf("image node contract: %+v", e.repo.Tasks)
			}
			id := e.image(t, 160, 160)
			task := &e.repo.Tasks[0]
			task.Status = tc.status
			if tc.valid {
				task.OutputJSON, _ = json.Marshal([]model.TaskOutput{{AssetID: uint64(id), MediaType: model.KindImage}})
			}
			if err := e.svc.OnTaskChanged(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			got, err := e.svc.Get(context.Background(), 1, 1, uint64(v.ID))
			if err != nil || got.State != tc.state {
				t.Fatalf("observation: %+v %v", got, err)
			}
			if tc.state == model.FrameRunStateFrameReady && (*got.FrameAssetID == uint64(id) || got.FrameSHA256 == "") {
				t.Fatalf("generated image not cropped: %+v", got)
			}
			before := got.Revision
			if err := e.svc.RecoverUnsettledDramaFrameRuns(context.Background(), repository.FrameRunRecoveryFilter{}, 10); err != nil {
				t.Fatal(err)
			}
			if e.repo.FrameRuns[0].Revision != before {
				t.Fatal("repeated recovery changed version")
			}
		})
	}
}

func TestFrameRunImageReplayAndMissingTasks(t *testing.T) {
	e := newFrameRunEnv(t)
	ctx := context.Background()
	v := e.create(t, model.FrameSourceGenerate)
	e.repo.FrameRuns[0].State = model.FrameRunStateDraft
	e.repo.FrameRuns[0].FrameTaskID = nil
	req := &model.CreateFrameRunReq{RequestKey: "run", SegmentRevision: 1, FrameSource: model.FrameSourceGenerate, ModelID: "image-model", Input: map[string]any{"prompt": "frame"}}
	replay, created, err := e.svc.Create(ctx, 1, 1, req)
	if err != nil || created || replay.ID != v.ID || replay.State != model.FrameRunStateFramePending || len(e.repo.Tasks) != 1 {
		t.Fatalf("draft replay: %+v %v", replay, err)
	}
	e.repo.FrameRuns[0].FrameTaskID = nil
	if err := e.svc.RecoverUnsettledDramaFrameRuns(ctx, repository.FrameRunRecoveryFilter{}, 10); err != nil {
		t.Fatal(err)
	}
	if e.repo.FrameRuns[0].State != model.FrameRunStateFailed {
		t.Fatal("missing image task not failed")
	}
	e = newFrameRunEnv(t)
	e.tasks.createErr = errcode.ErrModelUnavailable
	_, _, err = e.svc.Create(ctx, 1, 1, req)
	assertTaskCode(t, err, errcode.ErrModelUnavailable.Code)
	if e.repo.FrameRuns[0].State != model.FrameRunStateFailed || e.repo.FrameRuns[0].ErrorCode == "" {
		t.Fatal("create error not persisted")
	}
}

func TestFrameRunSubmittingRecovery(t *testing.T) {
	for _, tc := range []struct {
		name           string
		matched, stale bool
		want           model.DramaFrameRunState
	}{
		{"live no task", false, false, model.FrameRunStateSubmitting}, {"stale no task", false, true, model.FrameRunStateFrameReady},
		{"matching task", true, false, model.FrameRunStateVideoPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFrameRunEnv(t)
			v := e.confirmed(t)
			run := &e.repo.FrameRuns[0]
			run.State = model.FrameRunStateSubmitting
			at := e.now
			if tc.stale {
				at = at.Add(-3 * time.Minute)
			}
			run.SubmittingAt = &at
			if tc.matched {
				canvas := uint64(9)
				e.repo.Tasks = append(e.repo.Tasks, model.GenerationTask{ID: 999, UserID: 1, Kind: model.KindVideo, Status: model.TaskPending, CanvasProjectID: &canvas,
					NodeID: "seg:" + idcodec.Encode(1), IdempotencyKey: run.VideoIdempotencyKey, FrameRunID: &run.ID, FrameRunConfirmRev: &run.ConfirmedVersion})
			}
			if err := e.svc.RecoverUnsettledDramaFrameRuns(context.Background(), repository.FrameRunRecoveryFilter{UserID: 1}, 10); err != nil {
				t.Fatal(err)
			}
			got, err := e.svc.Get(context.Background(), 1, 1, uint64(v.ID))
			if err != nil || got.State != tc.want {
				t.Fatalf("recover: %+v %v", got, err)
			}
			if tc.want == model.FrameRunStateFrameReady && (e.repo.FrameRuns[0].QuoteToken != nil || e.repo.FrameRuns[0].SubmittingAt != nil || e.repo.FrameRuns[0].FrameAssetID == nil) {
				t.Fatal("stale recovery lost frame or quote not cleared")
			}
		})
	}
}

func TestFrameRunVideoFailureRecovery(t *testing.T) {
	for _, status := range []string{model.TaskFailed, model.TaskCanceled, model.TaskExpired} {
		t.Run(status, func(t *testing.T) {
			e := newFrameRunEnv(t)
			v := e.confirmed(t)
			ctx := context.Background()
			_, _, err := e.svc.Submit(ctx, 1, 1, uint64(v.ID), &model.SubmitFrameRunReq{Revision: v.Revision, ConfirmedVersion: v.ConfirmedVersion})
			if err != nil {
				t.Fatal(err)
			}
			e.repo.Tasks[0].Status, e.repo.Tasks[0].ErrorCode, e.repo.Tasks[0].ErrorMessage = status, "provider_failed", "生成失败"
			if err := e.svc.OnTaskChanged(ctx, &e.repo.Tasks[0]); err != nil {
				t.Fatal(err)
			}
			got, err := e.svc.Get(ctx, 1, 1, uint64(v.ID))
			if err != nil || got.State != model.FrameRunStateFailed || got.ErrorCode != "provider_failed" {
				t.Fatalf("video failure: %+v %v", got, err)
			}
		})
	}
}

func TestFrameRunVideoSuccessRecovery(t *testing.T) {
	for _, adopt := range []bool{false, true} {
		t.Run(fmt.Sprint(adopt), func(t *testing.T) {
			e := newFrameRunEnv(t)
			ctx := context.Background()
			v := e.confirmed(t)
			e.repo.FrameRuns[0].AdoptIfEmpty = adopt
			_, _, err := e.svc.Submit(ctx, 1, 1, uint64(v.ID), &model.SubmitFrameRunReq{Revision: v.Revision, ConfirmedVersion: v.ConfirmedVersion})
			if err != nil {
				t.Fatal(err)
			}
			a := &model.Asset{ID: 999, UserID: 1, Kind: model.KindVideo, Sha256: strings.Repeat("b", 64)}
			e.assetRepo.rows[a.ID] = a
			e.repo.Assets = map[uint64]model.Asset{a.ID: *a}
			e.repo.Tasks[0].Status = model.TaskSucceeded
			e.repo.Tasks[0].OutputJSON, _ = json.Marshal([]model.TaskOutput{{AssetID: a.ID, MediaType: model.KindVideo}})
			if err := e.svc.RecoverUnsettledDramaFrameRuns(ctx, repository.FrameRunRecoveryFilter{}, 10); err != nil {
				t.Fatal(err)
			}
			view, err := e.segments.Get(ctx, 1, 1)
			if err != nil || view.FrameRun.State != model.FrameRunStateBound || len(view.Takes) != 1 || len(e.repo.TakeLinks) != 1 || (view.ChosenTaskID != "") != adopt {
				t.Fatalf("success: %+v %v", view, err)
			}
			before := view.Revision
			if err := e.svc.RecoverUnsettledDramaFrameRuns(ctx, repository.FrameRunRecoveryFilter{}, 10); err != nil {
				t.Fatal(err)
			}
			view, err = e.segments.Get(ctx, 1, 1)
			if err != nil || view.Revision != before || len(e.repo.TakeLinks) != 1 {
				t.Fatalf("duplicate success side effects: %+v %v", view, err)
			}
		})
	}
}

func TestFrameRunCreateGuards(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*frameRunEnv, *model.CreateFrameRunReq)
		code   int
	}{
		{"blank key", func(_ *frameRunEnv, r *model.CreateFrameRunReq) { r.RequestKey = " " }, 10001},
		{"long key", func(_ *frameRunEnv, r *model.CreateFrameRunReq) { r.RequestKey = strings.Repeat("x", 97) }, 10001},
		{"source", func(_ *frameRunEnv, r *model.CreateFrameRunReq) { r.FrameSource = "unknown" }, 10001},
		{"missing image", func(_ *frameRunEnv, r *model.CreateFrameRunReq) { r.FrameAssetID = nil }, 91008},
		{"zero image", func(_ *frameRunEnv, r *model.CreateFrameRunReq) { id := model.AssetID(0); r.FrameAssetID = &id }, 91008},
		{"missing asset", func(_ *frameRunEnv, r *model.CreateFrameRunReq) { id := model.AssetID(999); r.FrameAssetID = &id }, 91008},
		{"asset ownership", func(e *frameRunEnv, r *model.CreateFrameRunReq) { e.assetRepo.rows[uint64(*r.FrameAssetID)].UserID = 2 }, 91008},
		{"asset kind", func(e *frameRunEnv, r *model.CreateFrameRunReq) {
			e.assetRepo.rows[uint64(*r.FrameAssetID)].Kind = model.KindVideo
		}, 91008},
		{"corrupt", func(e *frameRunEnv, r *model.CreateFrameRunReq) {
			a := e.assetRepo.rows[uint64(*r.FrameAssetID)]
			e.store.objects[a.StorageKey] = []byte("corrupt")
		}, 91008},
		{"zero pixels", func(e *frameRunEnv, r *model.CreateFrameRunReq) {
			a := e.assetRepo.rows[uint64(*r.FrameAssetID)]
			e.store.objects[a.StorageKey] = nil
		}, 91008},
		{"image prompt", func(_ *frameRunEnv, r *model.CreateFrameRunReq) {
			r.FrameSource = model.FrameSourceGenerate
			r.ModelID = "m"
			r.Input = map[string]any{"prompt": " "}
		}, 10001},
		{"image model", func(_ *frameRunEnv, r *model.CreateFrameRunReq) {
			r.FrameSource = model.FrameSourceGenerate
			r.Input = map[string]any{"prompt": "x"}
		}, 10001},
		{"canvas missing", func(_ *frameRunEnv, r *model.CreateFrameRunReq) {
			r.FrameSource = model.FrameSourceCanvas
			id := idcodec.ID(99)
			r.OriginCanvasID = &id
		}, 91008},
		{"stale segment", func(_ *frameRunEnv, r *model.CreateFrameRunReq) { r.SegmentRevision = 2 }, 30002},
		{"unlocked episode", func(e *frameRunEnv, _ *model.CreateFrameRunReq) { e.repo.Unlocked = true }, errcode.ErrDramaStepNotReady.Code},
		{"active run", func(e *frameRunEnv, _ *model.CreateFrameRunReq) {
			e.repo.FrameRuns = append(e.repo.FrameRuns, model.DramaSegmentFrameRun{ID: 99, UserID: 1, SegmentID: 1, State: model.FrameRunStateDraft})
		}, 91005},
		{"active video", func(e *frameRunEnv, _ *model.CreateFrameRunReq) {
			canvas := uint64(9)
			e.repo.Tasks = append(e.repo.Tasks, model.GenerationTask{ID: 99, UserID: 1, CanvasProjectID: &canvas, NodeID: "seg:" + idcodec.Encode(1), Kind: model.KindVideo, Status: model.TaskPending})
		}, errcode.ErrDramaJobBusy.Code},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFrameRunEnv(t)
			id := e.image(t, 160, 90)
			r := &model.CreateFrameRunReq{RequestKey: "run", SegmentRevision: 1, FrameSource: model.FrameSourceExisting, FrameAssetID: &id}
			tc.change(e, r)
			_, _, err := e.svc.Create(context.Background(), 1, 1, r)
			assertTaskCode(t, err, tc.code)
		})
	}
}

func TestFrameRunOwnershipAndObserverGuards(t *testing.T) {
	e := newFrameRunEnv(t)
	ctx := context.Background()
	v := e.create(t, model.FrameSourceGenerate)
	for _, task := range []*model.GenerationTask{nil, {IsTest: true}, {Kind: model.KindImage, UserID: 1, NodeID: "seg-frame:" + idcodec.Encode(1)}} {
		if err := e.svc.OnTaskChanged(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	_, err := e.svc.Get(ctx, 2, 1, uint64(v.ID))
	assertTaskCode(t, err, errcode.ErrDramaSegmentNotFound.Code)
	_, err = e.svc.Get(ctx, 1, 1, 999)
	assertTaskCode(t, err, 91004)
	e.repo.Rows = append(e.repo.Rows, model.DramaSegment{BaseModel: model.BaseModel{ID: 2}, UserID: 1, EpisodeID: 10, Revision: 1})
	_, err = e.svc.Get(ctx, 1, 2, uint64(v.ID))
	assertTaskCode(t, err, 91004)
	e.repo.ProjectDeleted = true
	if err := e.svc.OnTaskChanged(ctx, &e.repo.Tasks[0]); err != nil {
		t.Fatalf("deleted parent callback: %v", err)
	}
	if err := e.svc.RecoverUnsettledDramaFrameRuns(ctx, repository.FrameRunRecoveryFilter{}, 10); err != nil {
		t.Fatalf("deleted parent recovery: %v", err)
	}
}

func TestFrameRunCropFormatsAndCenter(t *testing.T) {
	for _, format := range []string{"png", "jpeg", "gif", "webp", "bmp"} {
		t.Run(format, func(t *testing.T) {
			e := newFrameRunEnv(t)
			ctx := context.Background()
			img := image.NewNRGBA(image.Rect(0, 0, 160, 160))
			for y := 0; y < 160; y++ {
				for x := 0; x < 160; x++ {
					if y >= 35 && y < 125 {
						img.SetNRGBA(x, y, color.NRGBA{G: 255, A: 255})
					} else {
						img.SetNRGBA(x, y, color.NRGBA{R: 255, A: 255})
					}
				}
			}
			var out bytes.Buffer
			var err error
			switch format {
			case "png":
				err = png.Encode(&out, img)
			case "jpeg":
				err = jpeg.Encode(&out, img, nil)
			case "gif":
				err = gif.Encode(&out, img, nil)
			case "webp":
				if _, err := exec.LookPath("ffmpeg"); err != nil {
					t.Skip("ffmpeg unavailable")
				}
				// 固定的 160×160 无损样本：上下红色、中央绿色；不依赖机器装有 WebP 编码器。
				data, decodeErr := base64.StdEncoding.DecodeString("UklGRiwAAABXRUJQVlA4TB8AAAAvn8AnAA+w//Mf8x8VFKRtwNS/6XrYgYj+p9HlBo0uAA==")
				if decodeErr != nil {
					t.Fatal(decodeErr)
				}
				out.Write(data)
			default:
				if _, err := exec.LookPath("ffmpeg"); err != nil {
					t.Skip("ffmpeg unavailable")
				}
				var input bytes.Buffer
				if err := png.Encode(&input, img); err != nil {
					t.Fatal(err)
				}
				codec := format
				cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-i", "pipe:0", "-frames:v", "1", "-f", "image2pipe", "-c:v", codec, "pipe:1")
				cmd.Stdin = &input
				data, convErr := cmd.Output()
				if convErr != nil {
					t.Fatal(convErr)
				}
				out.Write(data)
			}
			if err != nil {
				t.Fatal(err)
			}
			id := uint64(900)
			key := "fixture." + format
			e.assetRepo.rows[id] = &model.Asset{ID: id, UserID: 1, Kind: model.KindImage, StorageID: 1, StorageKey: key, Sha256: strings.Repeat("a", 64), Width: 160, Height: 90}
			e.store.objects[key] = out.Bytes()
			view, _, err := e.svc.Create(ctx, 1, 1, &model.CreateFrameRunReq{RequestKey: format, SegmentRevision: 1, FrameSource: model.FrameSourceExisting, FrameAssetID: frameAssetID(&id)})
			if err != nil {
				t.Fatal(err)
			}
			file, err := e.assets.Open(ctx, 1, uint64(*view.FrameAssetID))
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(file.Body)
			_ = file.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			cropped, err := png.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if cropped.Bounds().Dx() != 160 || cropped.Bounds().Dy() != 90 {
				t.Fatalf("dimensions: %v", cropped.Bounds())
			}
			red, green, _, _ := cropped.At(80, 45).RGBA()
			if green <= red {
				t.Fatal("center crop removed subject")
			}
		})
	}
}

type framePGEnv struct {
	db       *gorm.DB
	repo     *repository.DramaRepository
	assets   *service.AssetService
	registry *fakeTaskRegistry
	tasks    *service.GenerationTaskService
	segments *service.DramaSegmentService
	svc      *service.DramaFrameRunService
	p        *model.DramaProject
	ep       *model.DramaEpisode
	seg      *model.DramaSegment
	now      time.Time
}

func newFramePGEnv(t *testing.T) *framePGEnv {
	t.Helper()
	db := slice2cPG16(t)
	migrateCurrentSchemaPG16(t, db)
	e := &framePGEnv{db: db, repo: repository.NewDramaRepository(db), registry: &fakeTaskRegistry{snap: frameI2VSnapshot()}, now: time.Now()}
	store := &framePGStore{fakeAssetStore: newFakeAssetStore()}
	e.assets = service.NewAssetService(repository.NewAssetRepository(db), newFakeRegistry(1, objectHandle(1, store, time.Hour)), config.Storage{})
	e.tasks = service.NewGenerationTaskService(service.GenerationTaskDeps{Repo: repository.NewGenerationTaskRepository(db), Registry: e.registry, Assets: e.assets,
		Executor: &fakeTaskExecutor{}, Limits: defaultTaskLimits(), QuoteSecret: "pg-frame-tests"}, service.WithTaskClock(func() time.Time { return e.now }))
	e.segments = service.NewDramaSegmentService(service.DramaSegmentDeps{Repo: e.repo, Assets: e.assets, Tasks: e.tasks, Canvases: repository.NewCanvasProjectRepository(db), Probes: repository.NewAssetMediaProbeRepository(db)})
	e.svc = service.NewDramaFrameRunService(service.DramaFrameRunDeps{Repo: e.repo, Assets: e.assets, Registry: e.registry, Tasks: e.tasks, Segments: e.segments,
		Canvases: repository.NewCanvasProjectRepository(db), TicketSecret: "pg-ticket"}, service.WithDramaFrameRunClock(func() time.Time { return e.now }))
	e.segments.SetFrameRunEnricher(e.svc)
	e.tasks.SetFrameRunCancelGuard(e.repo)
	e.tasks.SetTaskObserver(service.TaskObservers{e.svc, e.segments})
	canvas := &model.CanvasProject{UserID: 7, PayloadJSON: []byte(`{}`), Revision: 1}
	mustTakeSQL(t, db.Create(canvas))
	e.p = &model.DramaProject{UserID: 7, CanvasID: canvas.ID, Title: "首帧测试", SourceText: "src", StyleKey: "custom", Ratio: "16:9"}
	mustTakeSQL(t, db.Create(e.p))
	e.ep = &model.DramaEpisode{UserID: 7, DramaID: e.p.ID, No: 1, LockedAt: &e.now, EditSchema: 1, Revision: 1}
	mustTakeSQL(t, db.Create(e.ep))
	e.seg = &model.DramaSegment{UserID: 7, EpisodeID: e.ep.ID, Seq: 0, Seconds: 5, Revision: 1}
	mustTakeSQL(t, db.Create(e.seg))
	mustTakeSQL(t, db.Create(&model.UserCredit{UserID: 7, Balance: 500}))
	return e
}

// framePGStore 让真库并发测试的存储计数器也满足 race 检查。
type framePGStore struct {
	*fakeAssetStore
	mu sync.Mutex
}

func (s *framePGStore) Put(ctx context.Context, key string, body io.Reader, n int64, mime string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeAssetStore.Put(ctx, key, body, n, mime)
}
func (s *framePGStore) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeAssetStore.Open(ctx, key)
}
func (s *framePGStore) URL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeAssetStore.URL(ctx, key, ttl)
}
func (s *framePGStore) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeAssetStore.Delete(ctx, key)
}

func (e *framePGEnv) create(t *testing.T, key string) *model.DramaSegmentFrameRunView {
	t.Helper()
	a, err := e.assets.SaveDerivedImage(context.Background(), 7, "frame.png", "image/png", framePNG(t, 160, 160))
	if err != nil {
		t.Fatal(err)
	}
	id := model.AssetID(a.ID)
	v, _, err := e.svc.Create(context.Background(), 7, e.seg.ID, &model.CreateFrameRunReq{RequestKey: key, SegmentRevision: e.seg.Revision, FrameSource: model.FrameSourceExisting, FrameAssetID: &id, AdoptIfEmpty: true})
	if err != nil {
		t.Fatal(err)
	}
	if uint64(*v.FrameAssetID) == a.ID || v.FrameSHA256 == "" {
		t.Fatal("real persistence did not crop frame")
	}
	return v
}

func (e *framePGEnv) confirm(t *testing.T, v *model.DramaSegmentFrameRunView) *model.DramaSegmentFrameRunView {
	t.Helper()
	input := map[string]any{"prompt": "video", "op": "i2v", "aspect_ratio": "16:9", "duration": 5, "resolution": "720p", "count": 1, "images": []uint64{uint64(*v.FrameAssetID)}}
	q, err := e.tasks.Quote(context.Background(), 7, &model.QuoteGenerationTaskReq{Kind: model.KindVideo, ModelID: service.DefaultDramaI2VModelKey, CanvasID: idcodec.ID(e.p.CanvasID),
		NodeID: "seg:" + idcodec.Encode(e.seg.ID), Input: input}, service.QuoteTaskOptions{SegmentID: e.seg.ID, SegmentRevision: e.seg.Revision, SingleShotOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	v, err = e.svc.Confirm(context.Background(), 7, e.seg.ID, uint64(v.ID), &model.ConfirmFrameRunReq{Revision: v.Revision, FrameSHA256: v.FrameSHA256, Input: input, QuoteToken: q.QuoteToken})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestFrameRunPG16EndToEndAndExpiredReplay(t *testing.T) {
	e := newFramePGEnv(t)
	ctx := context.Background()
	v := e.confirm(t, e.create(t, "lifecycle"))
	req := &model.SubmitFrameRunReq{Revision: v.Revision, ConfirmedVersion: v.ConfirmedVersion}
	v, task, err := e.svc.Submit(ctx, 7, e.seg.ID, uint64(v.ID), req)
	if err != nil {
		t.Fatal(err)
	}
	before := e.account(t)
	e.now = e.now.Add(time.Hour)
	_, again, err := e.svc.Submit(ctx, 7, e.seg.ID, uint64(v.ID), req)
	if err != nil || again.ID != task.ID || !reflect.DeepEqual(before, e.account(t)) {
		t.Fatalf("replay changed accounting: %+v %v", again, err)
	}
	a := &model.Asset{UserID: 7, Kind: model.KindVideo, StorageKey: "video.mp4", Source: model.AssetSourceGenerated, Sha256: strings.Repeat("b", 64)}
	mustTakeSQL(t, e.db.Create(a))
	mustTakeSQL(t, e.db.Create(&model.AssetMediaProbe{AssetID: a.ID, Sha256: a.Sha256, ProbeVersion: videoframe.NormalizedProbeVersion, NormalizedFrames24: 120}))
	out, _ := json.Marshal([]model.TaskOutput{{AssetID: a.ID, MediaType: model.KindVideo}})
	mustTakeSQL(t, e.db.Model(&model.GenerationTask{}).Where("id = ?", task.ID).Updates(map[string]any{"status": model.TaskSucceeded, "output_json": out}))
	if err := e.svc.RecoverUnsettledDramaFrameRuns(ctx, repository.FrameRunRecoveryFilter{}, 50); err != nil {
		t.Fatal(err)
	}
	view, err := e.segments.Get(ctx, 7, e.seg.ID)
	if err != nil || view.FrameRun.State != model.FrameRunStateBound || view.ChosenTaskID != idcodec.Encode(task.ID) || len(view.Takes) != 1 {
		t.Fatalf("final adoption: %+v %v", view, err)
	}
	var links int64
	mustTakeSQL(t, e.db.Model(&model.DramaSegmentTakeLink{}).Count(&links))
	if links != 1 {
		t.Fatal("duplicate candidate")
	}
}

func (e *framePGEnv) account(t *testing.T) bindAccountSnapshot {
	t.Helper()
	var out bindAccountSnapshot
	mustTakeSQL(t, e.db.Where("user_id = ?", 7).First(&out.Credit))
	mustTakeSQL(t, e.db.Where("user_id = ?", 7).Order("id").Find(&out.Ledger))
	mustTakeSQL(t, e.db.Where("user_id = ?", 7).Order("id").Find(&out.Tasks))
	return out
}

func TestFrameRunPG16ConcurrentCreateAndSubmit(t *testing.T) {
	e := newFramePGEnv(t)
	ctx := context.Background()
	a, err := e.assets.SaveDerivedImage(ctx, 7, "frame.png", "image/png", framePNG(t, 160, 90))
	if err != nil {
		t.Fatal(err)
	}
	id := model.AssetID(a.ID)
	req := &model.CreateFrameRunReq{RequestKey: "same-key", SegmentRevision: 1, FrameSource: model.FrameSourceExisting, FrameAssetID: &id}
	const count = 4
	views := make([]*model.DramaSegmentFrameRunView, count)
	errs := make([]error, count)
	created := make([]bool, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() { defer wg.Done(); views[i], created[i], errs[i] = e.svc.Create(ctx, 7, e.seg.ID, req) }()
	}
	wg.Wait()
	newRuns := 0
	for i, err := range errs {
		if err != nil || views[i].ID != views[0].ID {
			t.Fatalf("create race: %+v %v", views[i], err)
		}
		if created[i] {
			newRuns++
		}
	}
	if newRuns != 1 {
		t.Fatalf("created %d runs", newRuns)
	}
	v := e.confirm(t, views[0])
	submit := &model.SubmitFrameRunReq{Revision: v.Revision, ConfirmedVersion: v.ConfirmedVersion}
	tasks := make([]*model.GenerationTaskView, count)
	for i := range count {
		wg.Add(1)
		go func() { defer wg.Done(); _, tasks[i], errs[i] = e.svc.Submit(ctx, 7, e.seg.ID, uint64(v.ID), submit) }()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil || tasks[i].ID != tasks[0].ID {
			t.Fatalf("submit race: %+v %v", tasks[i], err)
		}
	}
	account := e.account(t)
	if len(account.Tasks) != 1 || account.Credit.Frozen != 10 || account.Credit.Balance != 500 || len(account.Ledger) != 1 {
		t.Fatalf("duplicate freeze: %+v", account)
	}
}

func TestFrameRunPG16CancelFinalizingLateSuccess(t *testing.T) {
	e := newFramePGEnv(t)
	ctx := context.Background()
	v := e.confirm(t, e.create(t, "cancel"))
	v, task, err := e.svc.Submit(ctx, 7, e.seg.ID, uint64(v.ID), &model.SubmitFrameRunReq{Revision: v.Revision, ConfirmedVersion: v.ConfirmedVersion})
	if err != nil {
		t.Fatal(err)
	}
	mustTakeSQL(t, e.db.Model(&model.GenerationTask{}).Where("id = ?", task.ID).Update("status", model.TaskFinalizing))
	before := e.account(t)
	v, err = e.svc.Cancel(ctx, 7, e.seg.ID, uint64(v.ID), &model.CancelFrameRunReq{Revision: v.Revision})
	if err != nil || v.State != model.FrameRunStateCancelled {
		t.Fatalf("cancel: %+v %v", v, err)
	}
	if !reflect.DeepEqual(before, e.account(t)) {
		t.Fatal("non-cancelable task accounting changed")
	}
	a := &model.Asset{UserID: 7, Kind: model.KindVideo, StorageKey: "late.mp4", Source: model.AssetSourceGenerated, Sha256: strings.Repeat("c", 64)}
	mustTakeSQL(t, e.db.Create(a))
	out, _ := json.Marshal([]model.TaskOutput{{AssetID: a.ID, MediaType: model.KindVideo}})
	mustTakeSQL(t, e.db.Model(&model.GenerationTask{}).Where("id = ?", task.ID).Updates(map[string]any{"status": model.TaskSucceeded, "output_json": out}))
	var stored model.GenerationTask
	mustTakeSQL(t, e.db.First(&stored, task.ID))
	if err := e.segments.OnTaskChanged(ctx, &stored); err != nil {
		t.Fatal(err)
	}
	view, err := e.segments.Get(ctx, 7, e.seg.ID)
	if err != nil || len(view.Takes) != 0 || view.ChosenTaskID != "" || view.FrameRun.State != model.FrameRunStateCancelled {
		t.Fatalf("late success adopted: %+v %v", view, err)
	}
}

type frameSubmitRaceRepo struct {
	*dramasegmentfake.Repo
	beforeTx func()
}

func (r *frameSubmitRaceRepo) WithTx(ctx context.Context, fn func(repository.DramaTx) error) error {
	if r.beforeTx != nil {
		hook := r.beforeTx
		r.beforeTx = nil
		hook()
	}
	return r.Repo.WithTx(ctx, fn)
}

func TestFrameRunConcurrentSubmitReplaysWinner(t *testing.T) {
	e := newFrameRunEnv(t)
	v := e.confirmed(t)
	repo := &frameSubmitRaceRepo{Repo: e.repo}
	repo.beforeTx = func() {
		run := &e.repo.FrameRuns[0]
		canvas := uint64(9)
		id := uint64(999)
		e.repo.Tasks = append(e.repo.Tasks, model.GenerationTask{ID: id, UserID: 1, Kind: model.KindVideo, CanvasProjectID: &canvas, NodeID: "seg:" + idcodec.Encode(1), Status: model.TaskPending,
			IdempotencyKey: run.VideoIdempotencyKey, FrameRunID: &run.ID, FrameRunConfirmRev: &run.ConfirmedVersion})
		run.State, run.VideoTaskID, run.Revision = model.FrameRunStateVideoPending, &id, run.Revision+2
	}
	svc := service.NewDramaFrameRunService(service.DramaFrameRunDeps{Repo: repo, Assets: e.assets, Tasks: e.tasks, Registry: e.registry, Segments: e.segments})
	got, task, err := svc.Submit(context.Background(), 1, 1, uint64(v.ID), &model.SubmitFrameRunReq{Revision: v.Revision, ConfirmedVersion: v.ConfirmedVersion})
	if err != nil || got.State != model.FrameRunStateVideoPending || task.ID != 999 || len(e.repo.Tasks) != 1 {
		t.Fatalf("concurrent winner not replayed: %+v %+v %v", got, task, err)
	}
}

func TestFrameRunPG16ImageAndSubmittingRecovery(t *testing.T) {
	t.Run("frame_pending", func(t *testing.T) {
		e := newFramePGEnv(t)
		ctx := context.Background()
		imageSnap := fakeTaskSnapshot(model.KindImage, 10)
		imageSnap.Model.Key = "image-model"
		imageSnap.Model.Capabilities.Ops = []string{modelcfg.OpT2I}
		e.registry.snap = imageSnap
		v, _, err := e.svc.Create(ctx, 7, e.seg.ID, &model.CreateFrameRunReq{RequestKey: "generate", SegmentRevision: 1, FrameSource: model.FrameSourceGenerate, ModelID: "image-model", Input: map[string]any{"prompt": "first frame"}})
		if err != nil {
			t.Fatal(err)
		}
		if v.FrameTaskID == nil || v.State != model.FrameRunStateFramePending {
			t.Fatalf("image pending: %+v", v)
		}
		a, err := e.assets.SaveDerivedImage(ctx, 7, "source.png", "image/png", framePNG(t, 160, 160))
		if err != nil {
			t.Fatal(err)
		}
		output, _ := json.Marshal([]model.TaskOutput{{AssetID: a.ID, MediaType: model.KindImage}})
		mustTakeSQL(t, e.db.Model(&model.GenerationTask{}).Where("id = ?", uint64(*v.FrameTaskID)).Updates(map[string]any{"status": model.TaskSucceeded, "output_json": output}))
		if err := e.svc.RecoverUnsettledDramaFrameRuns(ctx, repository.FrameRunRecoveryFilter{UserID: 7}, 10); err != nil {
			t.Fatal(err)
		}
		v, err = e.svc.Get(ctx, 7, e.seg.ID, uint64(v.ID))
		if err != nil || v.State != model.FrameRunStateFrameReady || uint64(*v.FrameAssetID) == a.ID || v.FrameSHA256 == "" {
			t.Fatalf("image recovered: %+v %v", v, err)
		}
	})
	for _, matched := range []bool{false, true} {
		t.Run(fmt.Sprintf("submitting/matched=%v", matched), func(t *testing.T) {
			e := newFramePGEnv(t)
			ctx := context.Background()
			v := e.confirm(t, e.create(t, "recover"))
			var task *model.GenerationTaskView
			var err error
			if matched {
				v, task, err = e.svc.Submit(ctx, 7, e.seg.ID, uint64(v.ID), &model.SubmitFrameRunReq{Revision: v.Revision, ConfirmedVersion: v.ConfirmedVersion})
				if err != nil {
					t.Fatal(err)
				}
			}
			before := e.account(t)
			at := e.now.Add(-3 * time.Minute)
			mustTakeSQL(t, e.db.Model(&model.DramaSegmentFrameRun{}).Where("id = ?", uint64(v.ID)).Updates(map[string]any{"state": model.FrameRunStateSubmitting, "video_task_id": nil, "submitting_at": at}))
			if err := e.svc.RecoverUnsettledDramaFrameRuns(ctx, repository.FrameRunRecoveryFilter{}, 10); err != nil {
				t.Fatal(err)
			}
			v, err = e.svc.Get(ctx, 7, e.seg.ID, uint64(v.ID))
			if err != nil {
				t.Fatal(err)
			}
			want := model.FrameRunStateFrameReady
			if matched {
				want = model.FrameRunStateVideoPending
			}
			if v.State != want || v.FrameAssetID == nil || !reflect.DeepEqual(before, e.account(t)) {
				t.Fatalf("submitting recovery: %+v", v)
			}
			if matched && (v.VideoTaskID == nil || uint64(*v.VideoTaskID) != task.ID) {
				t.Fatal("did not repair forward task ID")
			}
		})
	}
}

func TestFrameRunPG16SubmitFailuresNoPartialFreeze(t *testing.T) {
	for _, quota := range []bool{false, true} {
		t.Run(fmt.Sprintf("quota=%v", quota), func(t *testing.T) {
			e := newFramePGEnv(t)
			ctx := context.Background()
			v := e.confirm(t, e.create(t, "failure"))
			wantCode := 91002
			wantState := model.FrameRunStateFrameReady
			if quota {
				mustTakeSQL(t, e.db.Model(&model.UserCredit{}).Where("user_id = ?", 7).Update("balance", 0))
				wantCode = errcode.ErrInsufficientCredits.Code
				wantState = model.FrameRunStateFailed
			} else {
				e.now = e.now.Add(time.Hour)
			}
			before := e.account(t)
			_, _, err := e.svc.Submit(ctx, 7, e.seg.ID, uint64(v.ID), &model.SubmitFrameRunReq{Revision: v.Revision, ConfirmedVersion: v.ConfirmedVersion})
			assertTaskCode(t, err, wantCode)
			v, err = e.svc.Get(ctx, 7, e.seg.ID, uint64(v.ID))
			if err != nil || v.State != wantState || !reflect.DeepEqual(before, e.account(t)) {
				t.Fatalf("failure changed accounting: %+v %v", v, err)
			}
		})
	}
}

func TestFrameRunQuoteLiveSnapshotAndCount(t *testing.T) {
	for _, change := range []string{"price", "model revision", "plugin hash", "multi count", "model unavailable"} {
		t.Run(change, func(t *testing.T) {
			e := newFrameRunEnv(t)
			v := e.create(t, model.FrameSourceExisting)
			req := e.confirmReq(t, v)
			switch change {
			case "price":
				e.registry.snap.Model.Pricing.Unit++
			case "model revision":
				e.registry.snap.ModelRevisionID++
			case "plugin hash":
				e.registry.snap.Plugin.SHA256 = "different"
			case "model unavailable":
				e.registry.snapErr = provider.ErrModelUnavailable
			case "multi count":
				input := cloneTestInput(req.Input)
				input["count"] = 2
				q, err := e.tasks.Quote(context.Background(), 1, &model.QuoteGenerationTaskReq{Kind: model.KindVideo, ModelID: service.DefaultDramaI2VModelKey, NodeIDs: []string{"one", "two"}, Input: input}, service.QuoteTaskOptions{SegmentID: 1, SegmentRevision: 1})
				if err != nil {
					t.Fatal(err)
				}
				req.QuoteToken = q.QuoteToken
			}
			quoteReq := &model.QuoteGenerationTaskReq{Kind: model.KindVideo, ModelID: service.DefaultDramaI2VModelKey, NodeID: "seg:" + idcodec.Encode(1), Input: req.Input}
			_, err := e.tasks.VerifyQuoteToken(context.Background(), 1, quoteReq, req.QuoteToken, service.QuoteTaskOptions{SegmentID: 1, SegmentRevision: 1, SingleShotOnly: true})
			assertTaskCode(t, err, 91002)
		})
	}
}

func cloneTestInput(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for k, v := range input {
		out[k] = v
	}
	return out
}

func TestFrameRunExistingCropAndIdempotency(t *testing.T) {
	ctx := context.Background()
	assets, _, _ := newAssetTestService(config.Storage{})
	repo := dramasegmentfake.New()
	repo.Rows = []model.DramaSegment{{BaseModel: model.BaseModel{ID: 1}, UserID: 1, EpisodeID: 10, Seconds: 5, Revision: 1}}
	segments := service.NewDramaSegmentService(service.DramaSegmentDeps{Repo: repo, Assets: assets, Broadcaster: repo})
	svc := service.NewDramaFrameRunService(service.DramaFrameRunDeps{Repo: repo, Assets: assets, Segments: segments, Broadcaster: repo})
	segments.SetFrameRunEnricher(svc)
	for _, tc := range []struct {
		name string
		w, h int
		crop bool
	}{
		{"square", 160, 160, true}, {"wide", 320, 100, true}, {"native", 160, 90, false}, {"tolerant", 159, 90, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := assets.SaveDerivedImage(ctx, 1, "source.png", "image/png", framePNG(t, tc.w, tc.h))
			if err != nil {
				t.Fatal(err)
			}
			id := model.AssetID(a.ID)
			req := &model.CreateFrameRunReq{RequestKey: tc.name, SegmentRevision: 1, FrameSource: model.FrameSourceExisting, FrameAssetID: &id}
			v, created, err := svc.Create(ctx, 1, 1, req)
			if err != nil || !created || v.State != model.FrameRunStateFrameReady || v.FrameSHA256 == "" {
				t.Fatalf("创建失败: %+v %v", v, err)
			}
			out, err := assets.Get(ctx, 1, uint64(*v.FrameAssetID))
			if err != nil || (out.ID != a.ID) != tc.crop {
				t.Fatalf("不必要的上传或未裁图: %+v %v", out, err)
			}
			if tc.crop && (out.Width*9 != out.Height*16 || out.Width%2 != 0 || out.Height%2 != 0) {
				t.Fatalf("不是精确偶数 16:9: %+v", out)
			}
			replay, created, err := svc.Create(ctx, 1, 1, req)
			if err != nil || created || replay.ID != v.ID {
				t.Fatalf("幂等重放: %+v %v", replay, err)
			}
			req.AdoptIfEmpty = true
			_, _, err = svc.Create(ctx, 1, 1, req)
			assertTaskCode(t, err, errcode.ErrIdempotencyConflict.Code)
			if _, err := svc.Cancel(ctx, 1, 1, uint64(v.ID), &model.CancelFrameRunReq{Revision: v.Revision}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFrameRunSaveDerivedImage(t *testing.T) {
	svc, _, _ := newAssetTestService(config.Storage{})
	a, err := svc.SaveDerivedImage(context.Background(), 1, "frame.png", "image/png", framePNG(t, 160, 90))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == 0 || len(a.Sha256) != 64 || a.Width != 160 || a.Height != 90 || a.Kind != model.KindImage || a.Source != model.AssetSourceUpload {
		t.Fatalf("派生图未经过完整入库: %+v", a)
	}
}

func TestFrameRunVerifyQuoteToken(t *testing.T) {
	e := newTaskSvcEnv(defaultTaskLimits())
	ctx := context.Background()
	req := &model.QuoteGenerationTaskReq{Kind: model.KindVideo, ModelID: "m1", NodeID: "seg:one", Input: map[string]any{"prompt": "首帧视频"}}
	opt := service.QuoteTaskOptions{SegmentID: 1, SegmentRevision: 3, SingleShotOnly: true}
	quote, err := e.svc.Quote(ctx, 1, req, opt)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := e.svc.VerifyQuoteToken(ctx, 1, req, quote.QuoteToken, opt)
	if err != nil || verified.QuoteToken != quote.QuoteToken || verified.Credits != quote.Credits || verified.PriceFingerprint != quote.PriceFingerprint {
		t.Fatalf("报价未能无冻结验证: %+v %v", verified, err)
	}
	for _, tc := range []struct {
		name, token string
		user        uint64
		opt         service.QuoteTaskOptions
	}{
		{"bad signature", quote.QuoteToken + "x", 1, opt},
		{"wrong user", quote.QuoteToken, 2, opt},
		{"wrong segment", quote.QuoteToken, 1, service.QuoteTaskOptions{SegmentID: 2, SegmentRevision: 3}},
		{"wrong revision", quote.QuoteToken, 1, service.QuoteTaskOptions{SegmentID: 1, SegmentRevision: 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.svc.VerifyQuoteToken(ctx, tc.user, req, tc.token, tc.opt)
			assertTaskCode(t, err, errcode.ErrDramaQuoteExpired.Code)
		})
	}
	req.Input["prompt"] = "changed"
	_, err = e.svc.VerifyQuoteToken(ctx, 1, req, quote.QuoteToken, opt)
	assertTaskCode(t, err, errcode.ErrDramaQuoteExpired.Code)
}

func TestFrameRunOrdinaryCancelBlockedAndExpiredHandledPG16(t *testing.T) {
	e := newFramePGEnv(t)
	ctx := context.Background()
	v := e.confirm(t, e.create(t, "guard-cancel"))
	v, task, err := e.svc.Submit(ctx, 7, e.seg.ID, uint64(v.ID), &model.SubmitFrameRunReq{Revision: v.Revision, ConfirmedVersion: v.ConfirmedVersion})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.tasks.Cancel(ctx, 7, task.ID); err == nil {
		t.Fatal("普通取消接口不应绕过活跃首帧流程")
	} else {
		assertTaskCode(t, err, errcode.ErrDramaFrameRunConflict.Code)
	}
	cancelled, err := e.svc.Cancel(ctx, 7, e.seg.ID, uint64(v.ID), &model.CancelFrameRunReq{Revision: v.Revision})
	if err != nil || cancelled.State != model.FrameRunStateCancelled {
		t.Fatalf("合法取消失败: %+v %v", cancelled, err)
	}

	// 普通画布任务不受首帧取消守卫影响，仍可正常取消。
	canvasTask := &model.GenerationTask{UserID: 7, CanvasProjectID: &e.p.CanvasID, NodeID: "plain-node", Kind: model.KindVideo,
		ModelKey: service.DefaultDramaI2VModelKey, Provider: "fake", Status: model.TaskPending, InputJSON: []byte(`{}`), ConfigSnapshot: []byte(`{}`),
		IdempotencyKey: "plain-cancel", Credits: 5, NextPollAt: e.now, DeadlineAt: e.now.Add(time.Minute)}
	mustTakeSQL(t, e.db.Create(canvasTask))
	mustTakeSQL(t, e.db.Model(&model.UserCredit{}).Where("user_id = ?", 7).Update("frozen", 5))
	out, err := e.tasks.Cancel(ctx, 7, canvasTask.ID)
	if err != nil || out.Status != model.TaskCanceled {
		t.Fatalf("普通任务取消受影响: %+v %v", out, err)
	}

	// TaskExpired 触发回调与恢复，释放片段活跃槽位。
	v2 := e.confirm(t, e.create(t, "expire-video"))
	v2, task2, err := e.svc.Submit(ctx, 7, e.seg.ID, uint64(v2.ID), &model.SubmitFrameRunReq{Revision: v2.Revision, ConfirmedVersion: v2.ConfirmedVersion})
	if err != nil {
		t.Fatal(err)
	}
	var dbTask2 model.GenerationTask
	mustTakeSQL(t, e.db.Where("id = ?", task2.ID).First(&dbTask2))
	if applied, err := e.tasks.Expire(ctx, &dbTask2); err != nil || !applied {
		t.Fatalf("Expire 失败: %v %v", applied, err)
	}
	got2, err := e.svc.Get(ctx, 7, e.seg.ID, uint64(v2.ID))
	if err != nil || got2.State != model.FrameRunStateFailed {
		t.Fatalf("超时视频流程未释放为 failed: %+v %v", got2, err)
	}
}

func TestFrameRunGeneratedDraftAndImageExpireAndFairSweep(t *testing.T) {
	e := newFrameRunEnv(t)
	ctx := context.Background()
	canvas := uint64(9)

	// 1. 生成首帧任务在 frame_pending 下超时进入 failed。
	v := e.create(t, model.FrameSourceGenerate)
	e.repo.Tasks[0].Status = model.TaskExpired
	if err := e.svc.OnTaskChanged(ctx, &e.repo.Tasks[0]); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.Get(ctx, 1, 1, uint64(v.ID))
	if err != nil || got.State != model.FrameRunStateFailed {
		t.Fatalf("超时生图未失败收口: %+v %v", got, err)
	}

	// 2. 生成首帧 draft 崩溃窗口：若幂等键已建图则自动绑回并推进 frame_ready；否则超时释放。
	a := e.image(t, 160, 90)
	outJSON, _ := json.Marshal([]model.TaskOutput{{AssetID: uint64(a), MediaType: model.KindImage}})
	crashRun := model.DramaSegmentFrameRun{
		ID: 501, UserID: 1, SegmentID: 1, RequestKey: "crash-draft", RequestHash: strings.Repeat("c", 64),
		FrameSource: model.FrameSourceGenerate, State: model.FrameRunStateDraft, SegmentRevision: 1, Revision: 1,
		ConfirmedInput: []byte(`{}`), UpdatedAt: e.now.Add(-5 * time.Minute),
	}
	e.repo.FrameRuns = []model.DramaSegmentFrameRun{crashRun}
	e.repo.Tasks = []model.GenerationTask{{
		ID: 777, UserID: 1, CanvasProjectID: &canvas, NodeID: "seg-frame:" + idcodec.Encode(1), Kind: model.KindImage,
		Status: model.TaskSucceeded, OutputJSON: outJSON, IdempotencyKey: "drama-frame-image:" + idcodec.Encode(501),
	}}
	if err := e.svc.RecoverUnsettledDramaFrameRuns(ctx, repository.FrameRunRecoveryFilter{UserID: 1}, 10); err != nil {
		t.Fatal(err)
	}
	if e.repo.FrameRuns[0].State != model.FrameRunStateFrameReady || e.repo.FrameRuns[0].FrameTaskID == nil || *e.repo.FrameRuns[0].FrameTaskID != 777 {
		t.Fatalf("生成 draft 崩溃窗口未恢复: %+v", e.repo.FrameRuns[0])
	}

	staleDraft := model.DramaSegmentFrameRun{
		ID: 502, UserID: 1, SegmentID: 1, RequestKey: "stale-draft", RequestHash: strings.Repeat("d", 64),
		FrameSource: model.FrameSourceGenerate, State: model.FrameRunStateDraft, SegmentRevision: 1, Revision: 1,
		ConfirmedInput: []byte(`{}`), UpdatedAt: e.now.Add(-5 * time.Minute),
	}
	e.repo.FrameRuns = []model.DramaSegmentFrameRun{staleDraft}
	e.repo.Tasks = nil
	if err := e.svc.RecoverUnsettledDramaFrameRuns(ctx, repository.FrameRunRecoveryFilter{UserID: 1}, 10); err != nil {
		t.Fatal(err)
	}
	if e.repo.FrameRuns[0].State != model.FrameRunStateFailed || e.repo.FrameRuns[0].ErrorCode != "frame_create_timeout" {
		t.Fatalf("超时无任务 draft 未失败收口: %+v", e.repo.FrameRuns[0])
	}

	// 3. 游标分页公平扫描：前缀 2 条持续进行中时，第 3 条仍可在下一页被恢复，不会饿死。
	e.repo.Rows = append(e.repo.Rows,
		model.DramaSegment{BaseModel: model.BaseModel{ID: 2}, UserID: 1, EpisodeID: 10, Seconds: 5, Revision: 1},
		model.DramaSegment{BaseModel: model.BaseModel{ID: 3}, UserID: 1, EpisodeID: 10, Seconds: 5, Revision: 1},
	)
	e.repo.FrameRuns = []model.DramaSegmentFrameRun{
		{ID: 601, UserID: 1, SegmentID: 1, RequestKey: "p1", FrameSource: model.FrameSourceGenerate, State: model.FrameRunStateFramePending, FrameTaskID: ptrUint(801), Revision: 1, ConfirmedInput: []byte(`{}`), UpdatedAt: e.now.Add(-3 * time.Minute)},
		{ID: 602, UserID: 1, SegmentID: 2, RequestKey: "p2", FrameSource: model.FrameSourceGenerate, State: model.FrameRunStateFramePending, FrameTaskID: ptrUint(802), Revision: 1, ConfirmedInput: []byte(`{}`), UpdatedAt: e.now.Add(-2 * time.Minute)},
		{ID: 603, UserID: 1, SegmentID: 3, RequestKey: "p3", FrameSource: model.FrameSourceGenerate, State: model.FrameRunStateFramePending, FrameTaskID: ptrUint(803), Revision: 1, ConfirmedInput: []byte(`{}`), UpdatedAt: e.now.Add(-1 * time.Minute)},
	}
	e.repo.Tasks = []model.GenerationTask{
		{ID: 801, UserID: 1, CanvasProjectID: &canvas, NodeID: "seg-frame:" + idcodec.Encode(1), Kind: model.KindImage, Status: model.TaskRunning},
		{ID: 802, UserID: 1, CanvasProjectID: &canvas, NodeID: "seg-frame:" + idcodec.Encode(2), Kind: model.KindImage, Status: model.TaskRunning},
		{ID: 803, UserID: 1, CanvasProjectID: &canvas, NodeID: "seg-frame:" + idcodec.Encode(3), Kind: model.KindImage, Status: model.TaskSucceeded, OutputJSON: outJSON},
	}
	cursor, hasMore, err := e.svc.RecoverUnsettledDramaFrameRunsPage(ctx, repository.FrameRunRecoveryFilter{UserID: 1}, 2)
	if err != nil || !hasMore {
		t.Fatalf("首页游标异常: %v hasMore=%v", err, hasMore)
	}
	if _, hasMore2, err := e.svc.RecoverUnsettledDramaFrameRunsPage(ctx, cursor, 2); err != nil || hasMore2 {
		t.Fatalf("次页恢复失败: %v hasMore=%v", err, hasMore2)
	}
	if e.repo.FrameRuns[2].State != model.FrameRunStateFrameReady {
		t.Fatalf("分页游标未能扫到第 3 条待恢复流程: %+v", e.repo.FrameRuns[2])
	}
}

func ptrUint(v uint64) *uint64 { return &v }

type storyboardTestRegistry struct {
	*fakeTaskRegistry
}

func (r *storyboardTestRegistry) ListModels(_ context.Context, kind string) ([]provider.ModelInfo, error) {
	if kind == modelcfg.KindAgent {
		return []provider.ModelInfo{{Key: service.DefaultDramaI2VModelKey, Kind: modelcfg.KindAgent}}, nil
	}
	return []provider.ModelInfo{{Key: service.DefaultDramaI2VModelKey, Kind: modelcfg.KindVideo}}, nil
}

func (r *storyboardTestRegistry) Snapshot(_ context.Context, _ string) (*provider.Snapshot, error) {
	return &provider.Snapshot{Model: provider.ModelSnapshot{Key: service.DefaultDramaI2VModelKey, Kind: modelcfg.KindAgent, Capabilities: r.snap.Model.Capabilities}}, nil
}

func TestFrameRunPG16StoryboardResolverGuard(t *testing.T) {
	e := newFramePGEnv(t)
	ctx := context.Background()
	mustTakeSQL(t, e.db.Model(e.p).Updates(map[string]any{"text_model": service.DefaultDramaI2VModelKey, "video_model": service.DefaultDramaI2VModelKey}))
	mustTakeSQL(t, e.db.Model(e.ep).Updates(map[string]any{"script_status": "ready", "script_md": "第一集剧本"}))
	dramaSvc := service.NewDramaService(service.DramaDeps{Repo: e.repo, Registry: &storyboardTestRegistry{e.registry}, Assets: e.assets, Segments: e.segments})
	req := &model.DramaJobReq{Step: "storyboard", Episodes: []int{1}}

	// 1. 悬空导入 link（任务缺失）以及已失败流程孤儿任务均不阻塞分镜估算。
	mustTakeSQL(t, e.db.Create(&model.DramaSegmentTakeLink{UserID: 7, SegmentID: e.seg.ID, TaskID: 8888, SourceCanvasID: e.p.CanvasID, AssetID: 9999, Origin: model.TakeOriginCanvasImport}))
	failedRun := model.DramaSegmentFrameRun{UserID: 7, SegmentID: e.seg.ID, RequestKey: "sb-fail", RequestHash: strings.Repeat("e", 64),
		FrameSource: model.FrameSourceExisting, State: model.FrameRunStateFailed, SegmentRevision: 1, Revision: 1, ConfirmedInput: []byte(`{}`)}
	mustTakeSQL(t, e.db.Create(&failedRun))
	orphanTask := model.GenerationTask{UserID: 7, CanvasProjectID: &e.p.CanvasID, NodeID: "seg:" + idcodec.Encode(e.seg.ID),
		Kind: model.KindVideo, ModelKey: service.DefaultDramaI2VModelKey, Provider: "fake", Status: model.TaskFailed,
		FrameRunID: &failedRun.ID, InputJSON: []byte(`{}`), ConfigSnapshot: []byte(`{}`), NextPollAt: e.now, DeadlineAt: e.now}
	mustTakeSQL(t, e.db.Create(&orphanTask))
	if _, err := dramaSvc.Estimate(ctx, 7, e.p.ID, req); err != nil {
		t.Fatalf("悬空链接或失败孤儿不应阻断分镜: %v", err)
	}

	// 2. 活跃首帧流程占位必须阻断分镜替换。
	active := e.create(t, "sb-active")
	if _, err := dramaSvc.Estimate(ctx, 7, e.p.ID, req); err == nil {
		t.Fatal("活跃首帧流程必须阻断分镜")
	} else {
		assertTaskCode(t, err, errcode.ErrDramaStoryboardHasTakes.Code)
	}
	if _, err := e.svc.Cancel(ctx, 7, e.seg.ID, uint64(active.ID), &model.CancelFrameRunReq{Revision: active.Revision}); err != nil {
		t.Fatal(err)
	}

	// 3. 真实有效的画布导入候选经 ResolveSegmentTakes 核验后阻断分镜。
	validImport := model.GenerationTask{UserID: 7, CanvasProjectID: &e.p.CanvasID, NodeID: "canvas-video",
		Kind: model.KindVideo, ModelKey: service.DefaultDramaI2VModelKey, Provider: "fake", Status: model.TaskSucceeded,
		OutputJSON: []byte(`[{"asset_id":1,"media_type":"video"}]`), InputJSON: []byte(`{}`), ConfigSnapshot: []byte(`{}`),
		NextPollAt: e.now, DeadlineAt: e.now}
	mustTakeSQL(t, e.db.Create(&validImport))
	mustTakeSQL(t, e.db.Create(&model.DramaSegmentTakeLink{UserID: 7, SegmentID: e.seg.ID, TaskID: validImport.ID, SourceCanvasID: e.p.CanvasID, AssetID: 1, Origin: model.TakeOriginCanvasImport}))
	if _, err := dramaSvc.Estimate(ctx, 7, e.p.ID, req); err == nil {
		t.Fatal("有效导入候选必须阻断分镜")
	} else {
		assertTaskCode(t, err, errcode.ErrDramaStoryboardHasTakes.Code)
	}
}
