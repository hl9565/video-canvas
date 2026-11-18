package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"video-canvas/internal/handler"
	"video-canvas/internal/middleware"
	"video-canvas/internal/model"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/jsonhash"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/provider"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
	"video-canvas/internal/service/dramasegmentfake"
)

type programHTTPAssets struct {
	previewHTTPAssets
	repo *dramasegmentfake.Repo
}

func (a programHTTPAssets) Get(ctx context.Context, user, id uint64) (*model.Asset, error) {
	return a.repo.ComposeAsset(ctx, user, id)
}

func (a programHTTPAssets) Open(ctx context.Context, user, id uint64) (*provider.AssetFile, error) {
	if id != 40 {
		return a.previewHTTPAssets.Open(ctx, user, id)
	}
	asset, err := a.Get(ctx, user, id)
	if err != nil {
		return nil, err
	}
	return &provider.AssetFile{Asset: asset, Body: io.NopCloser(strings.NewReader("audio"))}, nil
}

type programHTTPProbes struct{ video, audio atomic.Bool }

func (p *programHTTPProbes) Get(_ context.Context, id uint64, sha string, version int16) (*model.AssetMediaProbe, error) {
	switch {
	case id == 40 && version == videoframe.NormalizedAudioProbeVersion && p.audio.Load():
		return &model.AssetMediaProbe{AssetID: id, Sha256: sha, ProbeVersion: version, HasAudio: true, DecodedAudioSamples: 8000}, nil
	case id == 30 && version == videoframe.NormalizedProbeVersion && p.video.Load():
		return &model.AssetMediaProbe{NormalizedFrames24: 24}, nil
	default:
		return nil, repository.ErrNotFound
	}
}

func (p *programHTTPProbes) Upsert(_ context.Context, row *model.AssetMediaProbe) error {
	if row.ProbeVersion == videoframe.NormalizedAudioProbeVersion {
		p.audio.Store(true)
	} else {
		p.video.Store(true)
	}
	return nil
}

func programHTTPRouter(t *testing.T, cached bool, probeErr error, configure ...func(*dramasegmentfake.Repo)) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repo := dramasegmentfake.New()
	repo.Rows = []model.DramaSegment{{BaseModel: model.BaseModel{ID: 5}, EpisodeID: 10, UserID: 1, ChosenTaskID: new(uint64(20))}}
	repo.Tasks = []model.GenerationTask{{ID: 20, UserID: 1, CanvasProjectID: new(uint64(9)), NodeID: "seg:" + idcodec.Encode(5),
		Kind: model.KindVideo, Status: model.TaskSucceeded, OutputJSON: []byte(`[{"asset_id":30,"media_type":"video"}]`)}}
	repo.Assets = map[uint64]model.Asset{
		30: {ID: 30, UserID: 1, Kind: model.KindVideo, Sha256: jsonhash.SHA256Hex([]byte("video"))},
		40: {ID: 40, UserID: 1, Kind: model.KindAudio, Sha256: jsonhash.SHA256Hex([]byte("audio"))},
	}
	for _, setup := range configure {
		setup(repo)
	}
	probes := &programHTTPProbes{}
	probes.video.Store(cached)
	probes.audio.Store(cached)
	svc := service.NewDramaService(service.DramaDeps{
		Repo: repo, Assets: programHTTPAssets{repo: repo}, Probes: probes,
		ProbeVideo: func(context.Context, string) (videoframe.NormalizedProbe, error) {
			return videoframe.NormalizedProbe{NormalizedFrames24: 24}, probeErr
		},
		ProbeAudio: func(context.Context, string) (videoframe.AudioProbe, error) {
			return videoframe.AudioProbe{DecodedAudioSamples: 8000}, nil
		},
	})
	t.Cleanup(svc.ClosePreviews)
	h := handler.NewDramaHandler(svc)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(middleware.CtxUserIDKey, uint(1)) })
	router.GET("/dramas/:id/episodes/:no/program", h.GetProgram)
	router.PUT("/dramas/:id/episodes/:no/program", h.PutProgram)
	router.GET("/dramas/:id/episodes/:no/manifest", h.Manifest)
	return router
}

func programHTTPCall(r *gin.Engine, method, suffix, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, "/dramas/"+idcodec.Encode(7)+"/episodes/1/"+suffix, strings.NewReader(body)))
	return w
}

func programHTTPBody(episode uint64, v int, segment uint64, frame int, asset, extra string) string {
	return fmt.Sprintf(`{"expected_episode_id":%q,"expected_segment_version":%d,"program":{"v":1,"audio":[{"id":"hit","role":"sfx","asset_id":%s,"anchor":{"segment_id":%q,"source_f":%d},"length_f":2}],"overlays":[]}%s}`,
		idcodec.Encode(episode), v, asset, idcodec.Encode(segment), frame, extra)
}

func TestDramaProgramHTTPReadsAndWritesWithoutPrivateBinding(t *testing.T) {
	r := programHTTPRouter(t, true, nil)
	w := programHTTPCall(r, http.MethodGet, "program", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"audio":[],"overlays":[]`) {
		t.Fatalf("empty GET: %d %s", w.Code, w.Body.String())
	}
	req := programHTTPBody(10, 0, 5, 2, `"40"`, "")
	w = programHTTPCall(r, http.MethodPut, "program", req)
	assertProgramHTTPView(t, w)
	w = programHTTPCall(r, http.MethodGet, "program", "")
	assertProgramHTTPView(t, w)
	w = programHTTPCall(r, http.MethodPut, "program", req)
	assertProgramHTTPError(t, w, http.StatusConflict, 91001, "")
	w = programHTTPCall(r, http.MethodPut, "program", programHTTPBody(10, 1, 5, 2, "40", ""))
	assertProgramHTTPView(t, w)
}

func assertProgramHTTPView(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, `"segment_version":1`) || !strings.Contains(body, `"asset_id":40`) || !strings.Contains(body, `"overlays":[]`) ||
		!strings.Contains(body, idcodec.Encode(5)) || strings.Contains(body, "bound_source") {
		t.Fatalf("unexpected program view: %d %s", w.Code, body)
	}
}

func TestDramaProgramHTTPErrorsKeepCueDiagnostics(t *testing.T) {
	r := programHTTPRouter(t, true, nil)
	for _, c := range []struct {
		name, body, reason string
		status, code       int
	}{
		{"duplicate rebind", programHTTPBody(10, 0, 5, 2, "40", `,"rebind_anchor_cue_ids":["hit","hit"]`), "rebind_cue_duplicate", 400, 91009},
		{"unknown rebind", programHTTPBody(10, 0, 5, 2, "40", `,"rebind_anchor_cue_ids":["gone"]`), "rebind_cue_not_anchored", 400, 91009},
		{"overlays", strings.Replace(programHTTPBody(10, 0, 5, 2, "40", ""), `"overlays":[]`, `"overlays":[{}]`, 1), "overlays_unsupported", 400, 91009},
		{"private binding input", strings.Replace(programHTTPBody(10, 0, 5, 2, "40", ""), `"length_f":2`, `"length_f":2,"bound_source":{}`, 1), "invalid_json", 400, 91009},
		{"episode identity", programHTTPBody(11, 0, 5, 2, "40", ""), "", 409, 91001},
		{"missing segment", programHTTPBody(10, 0, 6, 2, "40", ""), "anchor_segment_missing", 409, 91010},
		{"trim boundary", programHTTPBody(10, 0, 5, 24, "40", ""), "anchor_frame_trimmed", 409, 91010},
		{"asset kind", programHTTPBody(10, 0, 5, 2, "30", ""), "media_unavailable", 409, 91012},
	} {
		t.Run(c.name, func(t *testing.T) {
			assertProgramHTTPError(t, programHTTPCall(r, http.MethodPut, "program", c.body), c.status, c.code, c.reason)
		})
	}
}

func assertProgramHTTPError(t *testing.T, w *httptest.ResponseRecorder, status, code int, reason string) {
	t.Helper()
	var response struct {
		Code int                                 `json:"code"`
		Data service.DramaProgramDiagnosticsData `json:"data"`
	}
	err := json.Unmarshal(w.Body.Bytes(), &response)
	if err != nil || w.Code != status || response.Code != code || strings.Contains(w.Body.String(), "bound_source") {
		t.Fatalf("unexpected error: %d %s %v", w.Code, w.Body.String(), err)
	}
	if reason != "" && (len(response.Data.Diagnostics) == 0 || response.Data.Diagnostics[0].Reason != reason) {
		t.Fatalf("missing diagnostic %q: %s", reason, w.Body.String())
	}
}

func TestDramaManifestHTTPV2PreparesThenServesReadyWhileV1IsStable(t *testing.T) {
	r := programHTTPRouter(t, false, nil)
	w := programHTTPCall(r, http.MethodGet, "manifest?v=2", "")
	if w.Code != http.StatusAccepted || w.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(w.Body.String(), `"status":"preparing"`) || !strings.Contains(w.Body.String(), `"program_diagnostics":[]`) {
		t.Fatalf("v2 prepare: %d %s", w.Code, w.Body.String())
	}
	for deadline := time.Now().Add(2 * time.Second); w.Code == http.StatusAccepted && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
		w = programHTTPCall(r, http.MethodGet, "manifest?v=2", "")
	}
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"v":2,"render_version":2`) || !strings.Contains(w.Body.String(), `"visual_kind":"video","asset_id":30`) {
		t.Fatalf("v2 ready: %d %s", w.Code, w.Body.String())
	}
	v1 := programHTTPCall(r, http.MethodGet, "manifest", "")
	explicit := programHTTPCall(r, http.MethodGet, "manifest?v=1", "")
	if v1.Code != http.StatusOK || v1.Body.String() != explicit.Body.String() || strings.Contains(v1.Body.String(), "program_diagnostics") {
		t.Fatalf("v1 behavior changed: %d %s / %s", v1.Code, v1.Body.String(), explicit.Body.String())
	}
	assertProgramHTTPError(t, programHTTPCall(r, http.MethodGet, "manifest?v=3", ""), http.StatusBadRequest, 10001, "")
}

func TestDramaManifestHTTPV2FailedProbeDoesNotResumePolling(t *testing.T) {
	r := programHTTPRouter(t, false, errors.New("bad video"))
	w := programHTTPCall(r, http.MethodGet, "manifest?v=2", "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("v2 prepare: %d %s", w.Code, w.Body.String())
	}
	for deadline := time.Now().Add(2 * time.Second); w.Code == http.StatusAccepted && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
		w = programHTTPCall(r, http.MethodGet, "manifest?v=2", "")
	}
	next := programHTTPCall(r, http.MethodGet, "manifest?v=2", "")
	for _, got := range []*httptest.ResponseRecorder{w, next} {
		if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"manifest_hash":""`) || !strings.Contains(got.Body.String(), `"reason":"media_unavailable"`) {
			t.Fatalf("failed probe must stay terminal: %d %s", got.Code, got.Body.String())
		}
	}
}

func TestDramaManifestHTTPV2WarmsAudioAndKeepsNumericAssetIDs(t *testing.T) {
	r := programHTTPRouter(t, false, nil)
	w := programHTTPCall(r, http.MethodPut, "program", programHTTPBody(10, 0, 5, 2, `"40"`, ""))
	assertProgramHTTPView(t, w)
	w = programHTTPCall(r, http.MethodGet, "manifest?v=2", "")
	if w.Code != http.StatusAccepted || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cold audio manifest: %d %s", w.Code, w.Body.String())
	}
	for deadline := time.Now().Add(2 * time.Second); w.Code == http.StatusAccepted && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
		w = programHTTPCall(r, http.MethodGet, "manifest?v=2", "")
	}
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"asset_id":40`) || !strings.Contains(w.Body.String(), `"overlays":[]`) ||
		strings.Contains(w.Body.String(), `"manifest_hash":""`) || strings.Contains(w.Body.String(), "bound_source") {
		t.Fatalf("warmed audio manifest: %d %s", w.Code, w.Body.String())
	}
}

func TestDramaProgramHTTPGoldenManifestDecimalIDs(t *testing.T) {
	const stillA = uint64(9007199254740993)
	const stillB = uint64(9007199254740995)
	router := programHTTPRouter(t, true, nil, func(r *dramasegmentfake.Repo) {
		for i, id := range []uint64{stillA, stillB} {
			r.Rows = append(r.Rows, model.DramaSegment{BaseModel: model.BaseModel{ID: uint64(6 + i)}, UserID: 1, EpisodeID: 10,
				Seq: i + 1, VisualKind: model.DramaVisualStill, StillAssetID: new(id), SourceOutF: new(int32(47)),
			})
			r.Assets[id] = model.Asset{ID: id, UserID: 1, Kind: model.KindImage, Sha256: strings.Repeat("b", 64)}
		}
	})
	assertProgramHTTPView(t, programHTTPCall(router, http.MethodPut, "program", programHTTPBody(10, 0, 5, 2, `"40"`, "")))
	w := programHTTPCall(router, http.MethodGet, "manifest?v=2", "")
	var envelope struct {
		Data struct {
			Hash     string `json:"manifest_hash"`
			Manifest struct {
				Clips []struct {
					SegmentID string          `json:"segment_id"`
					AssetID   json.RawMessage `json:"asset_id"`
					StillID   json.RawMessage `json:"still_asset_id"`
				} `json:"clips"`
				Program struct {
					Audio []struct {
						AssetID json.RawMessage `json:"asset_id"`
					} `json:"audio"`
					Overlays json.RawMessage `json:"overlays"`
				} `json:"program"`
			} `json:"manifest"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || w.Code != http.StatusOK {
		t.Fatalf("mixed v2 response: %d %s (%v)", w.Code, w.Body.String(), err)
	}
	m := envelope.Data.Manifest
	if len(envelope.Data.Hash) != 64 || len(m.Clips) != 3 || len(m.Program.Audio) != 1 ||
		string(m.Program.Audio[0].AssetID) != "40" || string(m.Clips[0].AssetID) != "30" ||
		string(m.Program.Overlays) != "[]" || strings.Contains(w.Body.String(), "bound_source") {
		t.Fatalf("incorrect mixed manifest: %s", w.Body.String())
	}
	for i, id := range []uint64{stillA, stillB} {
		if c := m.Clips[i+1]; c.SegmentID != idcodec.Encode(uint64(6+i)) || string(c.StillID) != fmt.Sprint(id) {
			t.Fatalf("still clip %d lost its distinct segment or decimal asset ID: %s", i, w.Body.String())
		}
	}
}
