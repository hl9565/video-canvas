package service_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/provider"
	"video-canvas/internal/repository"

	"video-canvas/internal/model"
	"video-canvas/internal/service"
	"video-canvas/internal/service/dramasegmentfake"
)

func visualFixture() (*dramasegmentfake.Repo, *service.DramaSegmentService) {
	r := dramasegmentfake.New()
	r.Rows = []model.DramaSegment{{BaseModel: model.BaseModel{ID: 2}, UserID: 1, EpisodeID: 10, Seconds: 5, Revision: 1}}
	r.Assets = map[uint64]model.Asset{
		20: {ID: 20, UserID: 1, Kind: model.KindImage, Sha256: "first"},
		21: {ID: 21, UserID: 1, Kind: model.KindImage, Sha256: "second"},
		22: {ID: 22, UserID: 3, Kind: model.KindImage},
		23: {ID: 23, UserID: 1, Kind: model.KindVideo},
	}
	return r, service.NewDramaSegmentService(service.DramaSegmentDeps{Repo: r, Tasks: r, Assets: r, Canvases: r})
}

func visualPatch(t *testing.T, s *service.DramaSegmentService, rev int64, patch string) (*model.DramaSegmentView, error) {
	t.Helper()
	var req model.UpdateDramaSegmentReq
	if err := json.Unmarshal(fmt.Appendf(nil, `{"revision":%d,%s}`, rev, patch), &req); err != nil {
		t.Fatal(err)
	}
	return s.Update(t.Context(), 1, 2, &req)
}

func TestDramaVisualTransitions(t *testing.T) {
	r, s := visualFixture()
	r.Rows[0].ChosenTaskID, r.Rows[0].SourceOutF = new(uint64(70)), new(int32(24))
	r.Rows[0].MuteOriginal, r.Rows[0].AudioGainDB, r.Rows[0].NativeIsDialogue, r.Rows[0].AvPtsOffsetMs = true, -4, true, 40
	r.Rows[0].FXJSON = []byte(`{"native_fade_in_f":2}`)
	for i, tc := range []struct {
		name, patch, kind string
		asset             uint64
		frames            int32
		version           int64
	}{
		{"video to still", `"visual_kind":"still","still_asset_id":20,"edit":{"source_out_f":18}`, "still", 20, 18, 1},
		{"swap image with FX", `"still_asset_id":21,"fx":{"zoom_start":1.10,"fade_in_f":4}`, "still", 21, 18, 2},
		{"canonical no-op", `"fx":{"fade_in_f":4,"zoom_start":1.1}`, "still", 21, 18, 2},
		{"still to black", `"visual_kind":"black"`, "black", 0, 18, 3},
		{"black to still", `"visual_kind":"still","still_asset_id":20`, "still", 20, 18, 4},
		{"still to video", `"visual_kind":"video","fx":{}`, "video", 0, 0, 5},
		{"video to black", `"visual_kind":"black","still_asset_id":null,"edit":{"source_out_f":13}`, "black", 0, 13, 6},
		{"black duration change", `"edit":{"source_out_f":22}`, "black", 0, 22, 7},
		{"black no-op", `"visual_kind":"black"`, "black", 0, 22, 7},
		{"black to video", `"visual_kind":"video"`, "video", 0, 0, 8},
	} {
		v, err := visualPatch(t, s, int64(i+1), tc.patch)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if v.VisualKind != tc.kind || v.SegmentVersion != tc.version || r.EditSchema != 1 || v.SourceFrameCount != nil || !v.ChosenCleared || v.ChosenTaskID != "" {
			t.Fatalf("%s: %+v schema=%d", tc.name, v, r.EditSchema)
		}
		if tc.asset == 0 && v.StillAssetID != nil || tc.asset != 0 && (v.StillAssetID == nil || *v.StillAssetID != tc.asset) {
			t.Fatalf("%s: wrong image ID: %+v", tc.name, v)
		}
		if tc.frames == 0 && v.Edit.SourceOutF != nil || tc.frames > 0 && (v.Edit.SourceOutF == nil || *v.Edit.SourceOutF != tc.frames) {
			t.Fatalf("%s: wrong source duration: %+v", tc.name, v.Edit)
		}
		if v.Edit.SourceInF != 0 || v.Edit.MuteOriginal || v.Edit.AudioGainDB != 0 || v.Edit.NativeIsDialogue || v.Edit.AvPtsOffsetMs != 0 {
			t.Fatalf("%s: stale video edits: %+v", tc.name, v.Edit)
		}
		if v.FX != nil && ((tc.kind != "still" && v.FX.HasStillMotion()) || v.FX.HasNativeFade()) {
			t.Fatalf("%s: incompatible FX: %+v", tc.name, v.FX)
		}
	}
	_, err := visualPatch(t, s, 1, `"visual_kind":"black","edit":{"source_out_f":18}`)
	dramaCheckCode(t, err, 30002)
}

func TestDramaVisualRejectsShortenedKeptFX(t *testing.T) {
	r, s := visualFixture()
	_, err := visualPatch(t, s, 1, `"visual_kind":"still","still_asset_id":20,"edit":{"source_out_f":18},"fx":{"fade_in_f":6,"fade_out_f":6}`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = visualPatch(t, s, 2, `"edit":{"source_out_f":11}`)
	dramaCheckCode(t, err, 91011)
	v, err := s.Get(context.Background(), 1, 2)
	if err != nil || v.Revision != 2 || r.SegmentVersion != 1 || *v.Edit.SourceOutF != 18 {
		t.Fatalf("failed patch changed timeline: %+v %v", v, err)
	}
}

func TestDramaVisualPatchValidation(t *testing.T) {
	for _, tc := range []struct {
		name, patch string
		code        int
	}{
		{"missing still ID", `"visual_kind":"still","edit":{"source_out_f":10}`, 91011},
		{"null still ID", `"visual_kind":"still","still_asset_id":null,"edit":{"source_out_f":10}`, 91011},
		{"missing image", `"visual_kind":"still","still_asset_id":99,"edit":{"source_out_f":10}`, 91012},
		{"foreign image", `"visual_kind":"still","still_asset_id":22,"edit":{"source_out_f":10}`, 91012},
		{"wrong kind", `"visual_kind":"still","still_asset_id":23,"edit":{"source_out_f":10}`, 91012},
		{"black cannot use image", `"visual_kind":"black","still_asset_id":20,"edit":{"source_out_f":10}`, 91011},
		{"missing explicit duration", `"visual_kind":"black"`, 91011},
		{"null duration", `"visual_kind":"black","edit":{"source_out_f":null}`, 91011},
		{"too long", `"visual_kind":"black","edit":{"source_out_f":14401}`, 91011},
		{"nonzero start", `"visual_kind":"black","edit":{"source_in_f":1,"source_out_f":10}`, 91011},
		{"synthetic native audio", `"visual_kind":"black","edit":{"source_out_f":10,"mute_original":false}`, 91011},
		{"synthetic native fade", `"visual_kind":"black","edit":{"source_out_f":10},"fx":{"native_fade_in_f":1}`, 91011},
		{"black motion", `"visual_kind":"black","edit":{"source_out_f":10},"fx":{"seed":0}`, 91011},
		{"video motion", `"fx":{"zoom_start":1}`, 91011},
		{"synthetic with take", `"visual_kind":"black","edit":{"source_out_f":10},"chosen_task_id":"` + idcodec.Encode(70) + `"`, 91011},
		{"stale expected task", `"visual_kind":"black","edit":{"source_out_f":10},"expected_task_id":"` + idcodec.Encode(70) + `"`, 30002},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, s := visualFixture()
			_, err := visualPatch(t, s, 1, tc.patch)
			dramaCheckCode(t, err, tc.code)
			if r.EditSchema != 0 || r.Rows[0].Revision != 1 || r.SegmentVersion != 0 {
				t.Fatalf("invalid PATCH committed an update: %+v", r.Rows[0])
			}
		})
	}
}

func TestDramaVisualFrameRunBlocksUntilTerminal(t *testing.T) {
	for _, kind := range []string{"still", "black"} {
		t.Run(kind, func(t *testing.T) {
			r, s := visualFixture()
			r.FrameRuns = []model.DramaSegmentFrameRun{{ID: 90, UserID: 1, SegmentID: 2, Revision: 1, State: model.FrameRunStateConfirmed}}
			patch := `"visual_kind":"` + kind + `","edit":{"source_out_f":13}`
			if kind == "still" {
				patch += `,"still_asset_id":20`
			}
			_, err := visualPatch(t, s, 1, patch)
			dramaCheckCode(t, err, 91005)
			r.FrameRuns[0].State = model.FrameRunStateCancelled
			v, err := visualPatch(t, s, 1, patch)
			if err != nil || v.FrameRun == nil || uint64(v.FrameRun.ID) != 90 || v.FrameRun.State != model.FrameRunStateCancelled {
				t.Fatalf("terminal run history: %+v %v", v, err)
			}
			_, err = visualPatch(t, s, 2, `"visual_kind":"video","reset_edit_on_task_change":false`)
			dramaCheckCode(t, err, 91011)
			_, err = visualPatch(t, s, 2, `"visual_kind":"video","edit":{"source_out_f":10}`)
			dramaCheckCode(t, err, 91003)
			_, err = visualPatch(t, s, 2, `"visual_kind":"video","still_asset_id":20`)
			dramaCheckCode(t, err, 91011)
		})
	}
}

func TestDramaVisualSwitchBackWithExplicitTake(t *testing.T) {
	for _, kind := range []string{"still", "black"} {
		t.Run(kind, func(t *testing.T) {
			r, _ := visualFixture()
			r.Rows[0].VisualKind, r.Rows[0].SourceOutF, r.Rows[0].ChosenCleared = kind, new(int32(27)), true
			if kind == "still" {
				r.Rows[0].StillAssetID = new(uint64(20))
				r.Rows[0].FXJSON = []byte(`{"seed":1}`)
			}
			r.Tasks = []model.GenerationTask{{ID: 70, UserID: 1, CanvasProjectID: new(uint64(9)), NodeID: "seg:" + idcodec.Encode(2),
				Kind: model.KindVideo, Status: model.TaskSucceeded, OutputJSON: []byte(`[{"asset_id":23,"media_type":"video"}]`)}}
			assets := takeTrimAssets{rows: map[uint64]*model.Asset{23: {ID: 23, UserID: 1, Kind: model.KindVideo, Sha256: "video"}}}
			s := service.NewDramaSegmentService(service.DramaSegmentDeps{Repo: r, Assets: assets,
				Probes: &fakeTakeProbeStore{frames: map[uint64]int32{23: 48}}})
			v, err := visualPatch(t, s, 1, `"visual_kind":"video","chosen_task_id":"`+idcodec.Encode(70)+`","edit":{"source_in_f":4,"source_out_f":24}`)
			if err != nil || v.VisualKind != "video" || v.StillAssetID != nil || v.FX != nil || v.ChosenCleared || v.ChosenTaskID != idcodec.Encode(70) ||
				v.SourceFrameCount == nil || *v.SourceFrameCount != 48 || v.Edit.SourceInF != 4 || *v.Edit.SourceOutF != 24 || v.SegmentVersion != 1 {
				t.Fatalf("adopt on return to video: %+v %v", v, err)
			}
			_, err = visualPatch(t, s, 2, `"fx":{"native_fade_in_f":11,"native_fade_out_f":10}`)
			dramaCheckCode(t, err, 91011)
		})
	}
}

func TestDramaVisualAspectRatioGates(t *testing.T) {
	r, seg := visualFixture()
	r.Ratio = "9:16"
	for _, patch := range []string{
		`"visual_kind":"still","still_asset_id":20,"edit":{"source_out_f":10}`,
		`"visual_kind":"black","edit":{"source_out_f":10}`,
		`"still_asset_id":20`,
		`"fx":{"flash_in_f":1}`,
	} {
		_, err := visualPatch(t, seg, 1, patch)
		dramaCheckCode(t, err, 91011)
	}
	if v, err := visualPatch(t, seg, 1, `"visual_kind":"video","fx":{}`); err != nil || v.FX != nil || r.EditSchema != 1 {
		t.Fatalf("empty FX and video flag must be allowed in portrait: %+v %v", v, err)
	}

	for _, schema := range []int16{0, 1} {
		base := newComposeMemory()
		base.project.Ratio, base.episodes[0].EditSchema = "9:16", schema
		svc, _, _ := manifestService(base)
		v, err := svc.ResolveEpisodeManifest(t.Context(), 7, 1, 1)
		if err != nil || v.ManifestHash == "" {
			t.Fatalf("Stage A schema %d portrait manifest: %+v %v", schema, v, err)
		}
		view, err := svc.Update(t.Context(), 7, 1, &model.UpdateDramaReq{Revision: base.project.Revision, Ratio: new("4:3")})
		if err != nil || view.Ratio != "4:3" {
			t.Fatalf("Stage A schema %d ratio change: %+v %v", schema, view, err)
		}
	}
}

func TestDramaVisualProgramRatioGates(t *testing.T) {
	r, svc := newProgramFixture(t, nil)
	r.project.Ratio = "9:16"
	_, err := svc.PutProgram(t.Context(), 7, 1, 1, anchoredProgram(0, 5, 10))
	dramaCheckCode(t, err, 91009)
	empty := &service.PutDramaProgramReq{ExpectedEpisodeID: 3, ExpectedSegmentVersion: new(int64), Program: service.DramaProgramInput{Version: 1}}
	if _, err := svc.PutProgram(t.Context(), 7, 1, 1, empty); err != nil {
		t.Fatalf("empty cleanup in portrait: %v", err)
	}

	r.project.Ratio = "16:9"
	if _, err := svc.PutProgram(t.Context(), 7, 1, 1, anchoredProgram(0, 5, 10)); err != nil {
		t.Fatal(err)
	}
	change := &model.UpdateDramaReq{Revision: r.project.Revision, Ratio: new("9:16")}
	_, err = svc.Update(t.Context(), 7, 1, change)
	dramaCheckCode(t, err, 91011)

	r.episodes[0].ProgramJSON = nil
	for _, row := range []model.DramaSegment{
		{VisualKind: "black", SourceOutF: new(int32(13))},
		{VisualKind: "still", StillAssetID: new(uint64(32)), SourceOutF: new(int32(47))},
		{VisualKind: "video", FXJSON: []byte(`{"flash_in_f":1}`)},
	} {
		row.UserID, row.EpisodeID = 7, 3
		r.segments = []model.DramaSegment{row}
		_, err = svc.Update(t.Context(), 7, 1, change)
		dramaCheckCode(t, err, 91011)
	}
}

func TestDramaVisualManifestReadyWithoutVideo(t *testing.T) {
	for _, kind := range []string{"black", "still"} {
		t.Run(kind, func(t *testing.T) {
			r, svc := newProgramFixture(t, nil)
			r.project.Ratio = "16:9"
			r.episodes[0].EditSchema = 1
			row := &r.segments[0]
			row.VisualKind, row.ChosenTaskID, row.ChosenCleared, row.SourceOutF = kind, nil, true, new(int32(13))
			if kind == "still" {
				row.StillAssetID, row.SourceOutF = new(uint64(32)), new(int32(47))
			}
			r.segments = r.segments[:1]
			v, prepare, err := svc.PrepareOrResolveEpisodeManifestV2(t.Context(), 7, 1, 1)
			if err != nil || prepare != nil || v.ManifestHash == "" || v.Status != "ready" ||
				v.Manifest.TotalFrames != *row.SourceOutF || v.Manifest.Clips[0].VisualKind != kind || v.Manifest.Clips[0].TaskID != 0 {
				t.Fatalf("synthetic-only manifest: %+v %+v %v", v, prepare, err)
			}
		})
	}
}

type visualRaceRepo struct{ *dramasegmentfake.Repo }

func (r visualRaceRepo) WithTx(ctx context.Context, fn func(repository.DramaTx) error) error {
	return r.Repo.WithTx(ctx, func(repository.DramaTx) error { return fn(r) })
}

func (r visualRaceRepo) LockProgramAssets(_ context.Context, _ uint64, ids []uint64) error {
	asset := r.Assets[ids[0]]
	asset.Sha256 = "changed after planning"
	r.Assets[ids[0]] = asset
	return nil
}

func TestDramaVisualRejectsChangedImageHash(t *testing.T) {
	r, _ := visualFixture()
	s := service.NewDramaSegmentService(service.DramaSegmentDeps{Repo: visualRaceRepo{r}, Assets: r})
	_, err := visualPatch(t, s, 1, `"visual_kind":"still","still_asset_id":20,"edit":{"source_out_f":13}`)
	dramaCheckCode(t, err, 91012)
	if r.Rows[0].Revision != 1 || r.SegmentVersion != 0 || r.EditSchema != 0 {
		t.Fatal("stale hash caused a partial visual update")
	}
}

type visualStillStore struct {
	*dramasegmentfake.Repo
	t         *testing.T
	hashCalls int
	failOpen  bool
}

func (a *visualStillStore) EnsureSha256(ctx context.Context, user, id uint64) (string, *model.Asset, error) {
	if a.InTx {
		a.t.Fatal("image hash computation ran under a write lock")
	}
	a.hashCalls++
	row, err := a.Get(ctx, user, id)
	if err != nil {
		return "", nil, err
	}
	row.Sha256 = "computed-image-sha"
	a.Assets[id] = *row
	return row.Sha256, row, nil
}

func (a *visualStillStore) Open(ctx context.Context, user, id uint64) (*provider.AssetFile, error) {
	if a.InTx {
		a.t.Fatal("image open ran under a write lock")
	}
	if a.failOpen {
		return nil, provider.ErrAssetNotFound
	}
	row, err := a.Get(ctx, user, id)
	if err != nil {
		return nil, err
	}
	return &provider.AssetFile{Asset: row, Body: io.NopCloser(strings.NewReader("still"))}, nil
}

func TestDramaVisualStillHashAndReadability(t *testing.T) {
	r, _ := visualFixture()
	r.Assets[20] = model.Asset{ID: 20, UserID: 1, Kind: model.KindImage}
	assets := &visualStillStore{Repo: r, t: t}
	s := service.NewDramaSegmentService(service.DramaSegmentDeps{Repo: r, Assets: assets})
	_, err := visualPatch(t, s, 1, `"visual_kind":"still","still_asset_id":20,"edit":{"source_out_f":13}`)
	if err != nil || assets.hashCalls != 1 || r.Assets[20].Sha256 == "" {
		t.Fatalf("image hash was not ensured: %v", err)
	}
	assets.failOpen = true
	_, err = visualPatch(t, s, 2, `"edit":{"source_out_f":20}`)
	dramaCheckCode(t, err, 91012)
	if r.SegmentVersion != 1 || r.Rows[0].Revision != 2 || assets.hashCalls != 1 {
		t.Fatal("unreadable existing still caused a partial update")
	}
}
