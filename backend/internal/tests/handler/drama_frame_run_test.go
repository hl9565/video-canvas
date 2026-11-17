package handler_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"video-canvas/internal/handler"
	"video-canvas/internal/middleware"
	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/utils"
	"video-canvas/internal/provider"
	"video-canvas/internal/provider/modelcfg"
	"video-canvas/internal/repository"
	"video-canvas/internal/router"
	"video-canvas/internal/service"
	"video-canvas/internal/service/dramasegmentfake"
)

func frameRunValidationHTTP() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(middleware.CtxUserIDKey, uint(1)) })
	h := handler.NewDramaFrameRunHandler(nil)
	seg := handler.NewDramaSegmentHandler(nil)
	base := "/api/v1/drama-segments/:sid"
	r.POST(base+"/frame-runs", h.Create)
	r.GET(base+"/frame-runs/:rid", h.Get)
	r.POST(base+"/frame-runs/:rid/frame", h.SetFrame)
	r.POST(base+"/frame-runs/:rid/confirm", h.Confirm)
	r.POST(base+"/frame-runs/:rid/submit", h.Submit)
	r.POST(base+"/frame-runs/:rid/cancel", h.Cancel)
	r.POST(base+"/takes/import", seg.ImportTakes)
	r.PATCH(base+"/takes/:tid", seg.PatchTakeQC)
	return r
}

type httpFrameAssets struct {
	rows map[uint64]*model.Asset
	data map[uint64][]byte
}

func (a *httpFrameAssets) Get(_ context.Context, userID, id uint64) (*model.Asset, error) {
	row := a.rows[id]
	if row == nil || row.UserID != userID {
		return nil, provider.ErrAssetNotFound
	}
	return new(*row), nil
}
func (a *httpFrameAssets) Open(ctx context.Context, userID, id uint64) (*provider.AssetFile, error) {
	row, err := a.Get(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	return &provider.AssetFile{Asset: row, Body: io.NopCloser(bytes.NewReader(a.data[id]))}, nil
}
func (a *httpFrameAssets) View(ctx context.Context, userID, id uint64) (*model.AssetView, error) {
	row, err := a.Get(ctx, userID, id)
	if err != nil {
		return nil, errcode.ErrAssetNotFound
	}
	return &model.AssetView{ID: id, Kind: row.Kind, URL: "/files/test"}, nil
}
func (a *httpFrameAssets) SaveDerivedImage(_ context.Context, userID uint64, _, _ string, data []byte) (*model.Asset, error) {
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	id := uint64(41 + len(a.rows))
	row := &model.Asset{ID: id, UserID: userID, Kind: model.KindImage, Width: config.Width, Height: config.Height, Sha256: hex.EncodeToString(sum[:])}
	a.rows[id], a.data[id] = row, bytes.Clone(data)
	return new(*row), nil
}

type httpFrameRegistry struct{}

func (httpFrameRegistry) Snapshot(context.Context, string) (*provider.Snapshot, error) {
	return &provider.Snapshot{Model: provider.ModelSnapshot{Key: service.DefaultDramaI2VModelKey, Kind: model.KindVideo, Capabilities: modelcfg.Capabilities{
		Ops: []string{"i2v"}, Refs: modelcfg.Refs{Image: modelcfg.RefSpec{On: true, Max: 1}}, Prompt: modelcfg.PromptSpec{MaxLength: 1000}, Params: modelcfg.ParamSet{
			{Name: "duration", ParamField: modelcfg.ParamField{Type: modelcfg.ParamEnum, Open: true, Options: []any{5.0}, Default: 5.0}},
			{Name: "resolution", ParamField: modelcfg.ParamField{Type: modelcfg.ParamEnum, Open: true, Options: []any{"720p"}, Default: "720p"}},
			{Name: "aspect_ratio", ParamField: modelcfg.ParamField{Type: modelcfg.ParamEnum, Open: true, Options: []any{"16:9"}, Default: "16:9"}},
		}}}}, nil
}

type httpFrameTasks struct {
	service.DramaFrameTasks
	repo *dramasegmentfake.Repo
}

func (f *httpFrameTasks) VerifyQuoteToken(_ context.Context, _ uint64, r *model.QuoteGenerationTaskReq, token string, _ ...service.QuoteTaskOptions) (*model.TaskQuoteView, error) {
	if token != "quote" {
		return nil, errcode.ErrDramaQuoteExpired
	}
	return &model.TaskQuoteView{EffectiveInput: r.Input, QuoteToken: token}, nil
}
func (f *httpFrameTasks) CreateWithOptions(ctx context.Context, userID uint64, key string, r *model.CreateGenerationTaskReq, opt service.CreateTaskOptions) (*model.CreateGenerationTaskResp, error) {
	run, err := f.repo.FrameRun(ctx, userID, opt.DramaRunBind.RunID, false)
	if err != nil {
		return nil, err
	}
	canvas := uint64(r.CanvasID)
	task := model.GenerationTask{ID: 101, UserID: userID, CanvasProjectID: &canvas, Kind: r.Kind, NodeID: r.NodeID, Status: model.TaskPending,
		FrameRunID: &run.ID, FrameRunConfirmRev: &run.ConfirmedVersion, IdempotencyKey: key}
	f.repo.Tasks = append(f.repo.Tasks, task)
	run.State, run.VideoTaskID = model.FrameRunStateVideoPending, &task.ID
	if err := f.repo.SaveFrameRun(ctx, run, run.Revision); err != nil {
		return nil, err
	}
	return &model.CreateGenerationTaskResp{Items: []model.CreateTaskItem{{Task: &model.GenerationTaskView{ID: task.ID, CanvasProjectID: new(r.CanvasID), NodeID: r.NodeID, Kind: r.Kind, Status: task.Status}}}}, nil
}
func (f *httpFrameTasks) FindByIdempotencyKey(_ context.Context, userID uint64, key string) (*model.GenerationTask, error) {
	for _, task := range f.repo.Tasks {
		if task.UserID == userID && task.IdempotencyKey == key {
			return &task, nil
		}
	}
	return nil, repository.ErrNotFound
}
func (f *httpFrameTasks) Cancel(_ context.Context, _ uint64, id uint64) (*model.GenerationTaskView, error) {
	if f.repo.InTx {
		panic("cancel under lock")
	}
	for i := range f.repo.Tasks {
		if f.repo.Tasks[i].ID == id {
			f.repo.Tasks[i].Status = model.TaskCanceled
			return &model.GenerationTaskView{ID: id, Status: model.TaskCanceled}, nil
		}
	}
	return nil, errcode.ErrTaskNotFound
}

func frameRunHTTP(t *testing.T) (*gin.Engine, *dramasegmentfake.Repo, string, uint64, *httpFrameAssets) {
	t.Helper()
	repo := dramasegmentfake.New()
	repo.Rows = []model.DramaSegment{{BaseModel: model.BaseModel{ID: 1}, UserID: 1, EpisodeID: 10, Seconds: 5, Revision: 1}}
	assets := &httpFrameAssets{rows: map[uint64]*model.Asset{}, data: map[uint64][]byte{}}
	var imageData bytes.Buffer
	if err := png.Encode(&imageData, image.NewNRGBA(image.Rect(0, 0, 160, 160))); err != nil {
		t.Fatal(err)
	}
	asset, err := assets.SaveDerivedImage(context.Background(), 1, "frame.png", "image/png", imageData.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	tasks := &httpFrameTasks{repo: repo}
	segments := service.NewDramaSegmentService(service.DramaSegmentDeps{Repo: repo, Assets: assets, Canvases: repo, Broadcaster: repo})
	frames := service.NewDramaFrameRunService(service.DramaFrameRunDeps{Repo: repo, Canvases: repo, Tasks: tasks, Assets: assets, Segments: segments, Registry: httpFrameRegistry{}, Broadcaster: repo, TicketSecret: "http-frame"})
	segments.SetFrameRunEnricher(frames)
	r := router.New(gin.TestMode, "http-secret", router.Handlers{DramaFrameRun: handler.NewDramaFrameRunHandler(frames), DramaSegment: handler.NewDramaSegmentHandler(segments),
		UserState: func(context.Context, uint64) (*middleware.UserState, error) {
			return &middleware.UserState{Status: model.UserStatusActive}, nil
		}, Files: func(*gin.Context) {}})
	token, _, err := utils.GenerateToken(1, "test", 0, "http-secret", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	return r, repo, token, asset.ID, assets
}

func frameHTTPCall(t *testing.T, r *gin.Engine, token, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("invalid response %d %s: %v", w.Code, w.Body.String(), err)
	}
	return w.Code, response
}

func TestFrameRunHandlerAllRoutesAndAuth(t *testing.T) {
	r, repo, token, imageID, assets := frameRunHTTP(t)
	base := "/api/v1/drama-segments/" + idcodec.Encode(1)
	runs := base + "/frame-runs"
	create := map[string]any{"request_key": "canvas", "segment_revision": 1, "frame_source": "canvas"}
	status, body := frameHTTPCall(t, r, "", "POST", runs, create)
	if status != 401 {
		t.Fatalf("unauthenticated: %d %+v", status, body)
	}
	status, body = frameHTTPCall(t, r, token, "POST", runs, create)
	if status != 201 || responseCode(t, body) != 0 {
		t.Fatalf("create: %d %+v", status, body)
	}
	view := responseData(t, body)
	run := runs + "/" + view["id"].(string)
	ticket := view["canvas_ticket"].(string)
	status, body = frameHTTPCall(t, r, token, "POST", runs, create)
	if status != 200 {
		t.Fatalf("replay: %d %+v", status, body)
	}
	status, body = frameHTTPCall(t, r, token, "GET", run, nil)
	if status != 200 {
		t.Fatalf("get: %d %+v", status, body)
	}
	status, body = frameHTTPCall(t, r, token, "POST", run+"/frame", map[string]any{"revision": 1, "frame_asset_id": idcodec.Encode(imageID), "canvas_ticket": ticket})
	if status != 400 || responseCode(t, body) != 10001 {
		t.Fatalf("encoded asset id should fail binding: %d %+v", status, body)
	}
	status, body = frameHTTPCall(t, r, token, "POST", run+"/frame", map[string]any{"revision": 1, "frame_asset_id": "41", "canvas_ticket": ticket})
	if status != 200 {
		t.Fatalf("set decimal string: %d %+v", status, body)
	}
	view = responseData(t, body)
	status, seg := frameHTTPCall(t, r, token, "GET", base, nil)
	if status != 200 || responseData(t, seg)["frame_run"].(map[string]any)["frame_sha256"] != view["frame_sha256"] {
		t.Fatalf("segment enrichment: %+v", seg)
	}
	if _, ok := view["frame_asset_id"].(float64); !ok {
		t.Fatalf("frame_asset_id should serialize as decimal number: %+v", view["frame_asset_id"])
	}
	confirm := map[string]any{"revision": view["revision"], "frame_asset_id": view["frame_asset_id"], "frame_sha256": view["frame_sha256"], "input": map[string]any{"prompt": "video", "duration": 5, "resolution": "720p"}, "quote_token": "bad"}
	status, body = frameHTTPCall(t, r, token, "POST", run+"/confirm", confirm)
	if status != 409 || responseCode(t, body) != 91002 {
		t.Fatalf("quote error: %d %+v", status, body)
	}
	confirm["quote_token"] = "quote"
	status, body = frameHTTPCall(t, r, token, "POST", run+"/confirm", confirm)
	if status != 200 {
		t.Fatalf("confirm: %d %+v", status, body)
	}
	view = responseData(t, body)
	status, body = frameHTTPCall(t, r, token, "POST", run+"/submit", map[string]any{"revision": view["revision"], "confirmed_version": view["confirmed_version"]})
	if status != 202 {
		t.Fatalf("submit: %d %+v", status, body)
	}
	submitted := responseData(t, body)
	view = submitted["run"].(map[string]any)
	if submitted["task"].(map[string]any)["kind"] != "video" || view["state"] != "video_pending" {
		t.Fatalf("submit envelope: %+v", body)
	}
	status, body = frameHTTPCall(t, r, token, "POST", run+"/cancel", map[string]any{"revision": view["revision"]})
	if status != 200 || responseData(t, body)["state"] != "cancelled" {
		t.Fatalf("cancel: %d %+v", status, body)
	}
	canvas := uint64(9)
	repo.Tasks = append(repo.Tasks, model.GenerationTask{ID: 51, UserID: 1, Kind: model.KindVideo, CanvasProjectID: &canvas, NodeID: "canvas-video", Status: model.TaskSucceeded, OutputJSON: []byte(`[{"asset_id":900,"media_type":"video"}]`), CreatedAt: time.Now()})
	// 借用素材读取边界提供正式视频；来源与 QC 由真实片段服务校验。
	assets.rows[900] = &model.Asset{ID: 900, UserID: 1, Kind: model.KindVideo}
	status, body = frameHTTPCall(t, r, token, "POST", base+"/takes/import", map[string]any{"revision": 1, "source_canvas_id": idcodec.ID(9), "task_id": idcodec.ID(51)})
	if status != 200 {
		t.Fatalf("import: %d %+v", status, body)
	}
	status, body = frameHTTPCall(t, r, token, "PATCH", base+"/takes/"+idcodec.Encode(51), map[string]any{"revision": 1, "qc": "accepted"})
	if status != 200 || responseData(t, body)["takes"].([]any)[0].(map[string]any)["qc"] != "accepted" {
		t.Fatalf("QC: %d %+v", status, body)
	}
}

func TestFrameRunHandlerValidationAllRoutes(t *testing.T) {
	r := frameRunValidationHTTP()
	base := "/api/v1/drama-segments/" + idcodec.Encode(1)
	run := base + "/frame-runs/" + idcodec.Encode(1)
	for _, tc := range []struct{ method, path string }{
		{"POST", base + "/frame-runs"}, {"GET", base + "/frame-runs/bad-id"},
		{"POST", run + "/frame"}, {"POST", run + "/confirm"},
		{"POST", run + "/submit"}, {"POST", run + "/cancel"},
		{"POST", base + "/takes/import"}, {"PATCH", base + "/takes/" + idcodec.Encode(1)},
	} {
		t.Run(tc.path, func(t *testing.T) {
			status, body := characterCall(t, r, tc.method, tc.path, `{}`)
			if status != 400 || responseCode(t, body) != 10001 {
				t.Fatalf("status=%d body=%+v", status, body)
			}
		})
	}
}

func TestFrameRunHandlerBusinessErrorsAllRoutes(t *testing.T) {
	r, _, token, imageID, _ := frameRunHTTP(t)
	base := "/api/v1/drama-segments/" + idcodec.Encode(1)
	_, body := frameHTTPCall(t, r, token, "POST", base+"/frame-runs", map[string]any{"request_key": "errors", "segment_revision": 1, "frame_source": "canvas"})
	run := base + "/frame-runs/" + responseData(t, body)["id"].(string)
	for _, tc := range []struct {
		method, path string
		body         any
		status, code int
	}{
		{"POST", base + "/frame-runs", map[string]any{"request_key": "errors", "segment_revision": 1, "frame_source": "canvas", "adopt_if_empty": true}, 409, 90001},
		{"GET", base + "/frame-runs/" + idcodec.Encode(999), nil, 404, 91004},
		{"POST", run + "/frame", map[string]any{"revision": 1, "frame_asset_id": imageID, "canvas_ticket": "bad"}, 400, 91008},
		{"POST", run + "/confirm", map[string]any{"revision": 1, "frame_sha256": strings.Repeat("a", 64), "quote_token": "quote", "input": map[string]any{"prompt": "video", "duration": 5, "resolution": "720p"}}, 409, 91005},
		{"POST", run + "/submit", map[string]any{"revision": 1, "confirmed_version": 1}, 409, 91005},
		{"POST", run + "/cancel", map[string]any{"revision": 2}, 409, 30002},
		{"POST", base + "/takes/import", map[string]any{"revision": 2, "source_canvas_id": idcodec.ID(9), "task_id": idcodec.ID(51)}, 409, 30002},
		{"PATCH", base + "/takes/" + idcodec.Encode(51), map[string]any{"revision": 2, "qc": "accepted"}, 409, 30002},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			status, body := frameHTTPCall(t, r, "", tc.method, tc.path, tc.body)
			if status != 401 {
				t.Fatalf("route unauthenticated: %d %+v", status, body)
			}
			status, body = frameHTTPCall(t, r, token, tc.method, tc.path, tc.body)
			if status != tc.status || responseCode(t, body) != tc.code {
				t.Fatalf("business response: %d %+v", status, body)
			}
		})
	}
}
