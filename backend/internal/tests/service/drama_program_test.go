package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/jsonhash"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/provider"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
	"video-canvas/internal/service/dramasegmentfake"
)

func TestDramaProgramEmptyRoundTrip(t *testing.T) {
	ctx := context.Background()
	r := dramasegmentfake.New()
	r.ProgramJSON = []byte(`{}`)
	s := service.NewDramaService(service.DramaDeps{Repo: r})
	view, err := s.GetProgram(ctx, 1, 7, 1)
	if err != nil || view.Program.Version != 1 || view.Program.Audio == nil || view.SegmentVersion != 0 {
		t.Fatalf("empty program: %+v %v", view, err)
	}
	req := &service.PutDramaProgramReq{ExpectedEpisodeID: view.EpisodeID, ExpectedSegmentVersion: new(int64), Program: service.DramaProgramInput{Version: 1}}
	view, err = s.PutProgram(ctx, 1, 7, 1, req)
	if err != nil || view.SegmentVersion != 0 || view.EditSchema != 0 || string(r.ProgramJSON) != `{"v":1,"audio":[],"overlays":[]}` {
		t.Fatalf("empty normalization: %+v %s %v", view, r.ProgramJSON, err)
	}
	req.ExpectedEpisodeID++
	_, err = s.PutProgram(ctx, 1, 7, 1, req)
	dramaCheckCode(t, err, 91001)
}

// programMemory wraps the compose fixture so the same API can be tested before and after Program edits.
type programMemory struct {
	*composeMemory
	media map[uint64]model.Asset
	inTx  bool
}

func (r *programMemory) WithTx(ctx context.Context, fn func(repository.DramaTx) error) error {
	r.inTx = true
	defer func() { r.inTx = false }()
	return r.composeMemory.WithTx(ctx, func(repository.DramaTx) error { return fn(r) })
}

func (r *programMemory) ComposeAsset(_ context.Context, user, id uint64) (*model.Asset, error) {
	asset, ok := r.media[id]
	if !ok || asset.UserID != user {
		return nil, repository.ErrNotFound
	}
	return &asset, nil
}

func (r *programMemory) SaveEpisodeProgram(_ context.Context, user, drama, epID uint64, version int64, schema int16, raw json.RawMessage, material bool) error {
	ep := &r.episodes[0]
	if !r.inTx || ep.UserID != user || ep.DramaID != drama || ep.ID != epID || ep.SegmentVersion != version {
		return repository.ErrStateConflict
	}
	ep.ProgramJSON, ep.EditSchema = []byte(raw), schema
	ep.Revision++
	if material {
		ep.SegmentVersion++
	}
	return nil
}

type programFiles struct {
	*asyncWarmupFiles
	*programMemory
}

func (a programFiles) Get(ctx context.Context, user, id uint64) (*model.Asset, error) {
	return a.ComposeAsset(ctx, user, id)
}

func (a programFiles) Open(ctx context.Context, user, id uint64) (*provider.AssetFile, error) {
	var body string
	switch id {
	case 31:
		body = "video2"
	case 40:
		body = "audio"
	default:
		return a.asyncWarmupFiles.Open(ctx, user, id)
	}
	asset, err := a.Get(ctx, user, id)
	if err != nil {
		return nil, err
	}
	return &provider.AssetFile{Asset: asset, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func newProgramFixture(t *testing.T, duringProbe func(*programMemory), configure ...func(*service.DramaDeps)) (*programMemory, *service.DramaService) {
	t.Helper()
	memory := newComposeMemory()
	memory.project.Ratio = "16:9"
	r := &programMemory{composeMemory: memory, media: map[uint64]model.Asset{
		30: {ID: 30, UserID: 7, Kind: model.KindVideo, Sha256: jsonhash.SHA256Hex([]byte("video"))},
		31: {ID: 31, UserID: 7, Kind: model.KindVideo, Sha256: jsonhash.SHA256Hex([]byte("video2"))},
		32: {ID: 32, UserID: 7, Kind: model.KindImage, Sha256: strings.Repeat("c", 64)},
		40: {ID: 40, UserID: 7, Kind: model.KindAudio, Sha256: jsonhash.SHA256Hex([]byte("audio"))},
	}}
	files := programFiles{&asyncWarmupFiles{&composeFiles{r: memory}}, r}
	deps := service.DramaDeps{
		Repo: r, Assets: files, Probes: &manifestProbes{rows: map[string]*model.AssetMediaProbe{}},
		ProbeVideo: func(context.Context, string) (videoframe.NormalizedProbe, error) {
			if r.inTx {
				t.Error("normalized video probe executed inside write transaction")
			}
			if duringProbe != nil {
				duringProbe(r)
			}
			return videoframe.NormalizedProbe{NormalizedFrames24: 120}, nil
		},
		ProbeAudio: func(context.Context, string) (videoframe.AudioProbe, error) {
			if r.inTx {
				t.Error("normalized audio probe executed inside write transaction")
			}
			return videoframe.AudioProbe{DecodedAudioSamples: 12000}, nil
		},
	}
	for _, f := range configure {
		f(&deps)
	}
	s := service.NewDramaService(deps)
	t.Cleanup(s.ClosePreviews)
	return r, s
}

func anchoredProgram(version int64, segment uint64, sourceF int32) *service.PutDramaProgramReq {
	return &service.PutDramaProgramReq{
		ExpectedEpisodeID: 3, ExpectedSegmentVersion: &version,
		Program: service.DramaProgramInput{Version: 1, Audio: []service.DramaProgramAudioInput{
			{DramaProgramAudioParams: model.DramaProgramAudioParams{ID: "hit", Role: model.DramaAudioSFX, LengthF: new(int32(10))}, AssetID: 40,
				Anchor: &service.DramaProgramAnchorInput{SegmentID: idcodec.ID(segment), SourceF: &sourceF}},
			absoluteProgramCue("voice", model.DramaAudioNarration, 0, 12),
		}},
	}
}

func absoluteProgramCue(id string, role model.DramaProgramAudioRole, start, end int32) service.DramaProgramAudioInput {
	return service.DramaProgramAudioInput{AssetID: 40, DramaProgramAudioParams: model.DramaProgramAudioParams{ID: id, Role: role, StartF: &start, EndF: &end}}
}

func requireProgramError(t *testing.T, err error, code int, reason string) {
	t.Helper()
	dramaCheckCode(t, err, code)
	var business *errcode.Error
	if !errors.As(err, &business) {
		t.Fatalf("missing business error: %v", err)
	}
	data, ok := business.Data.(service.DramaProgramDiagnosticsData)
	if !ok || len(data.Diagnostics) == 0 || data.Diagnostics[0].Reason != reason {
		t.Fatalf("missing %q diagnostic: %+v", reason, business.Data)
	}
}

func TestDramaProgramRejectsRacesDuringSourceProbe(t *testing.T) {
	for name, mutate := range map[string]func(*programMemory){
		"version": func(r *programMemory) { r.episodes[0].SegmentVersion++ },
		"video hash": func(r *programMemory) {
			asset := r.media[30]
			asset.Sha256 = strings.Repeat("e", 64)
			r.media[30] = asset
		},
		"trim without version change": func(r *programMemory) { r.segments[0].SourceInF = 5 },
		"audio hash": func(r *programMemory) {
			asset := r.media[40]
			asset.Sha256 = strings.Repeat("e", 64)
			r.media[40] = asset
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, s := newProgramFixture(t, mutate)
			_, err := s.PutProgram(context.Background(), 7, 1, 1, anchoredProgram(0, 5, 10))
			dramaCheckCode(t, err, 91001)
			if len(r.episodes[0].ProgramJSON) != 0 {
				t.Fatal("stale request wrote private binding")
			}
		})
	}
}

func replaceProgramTake(r *programMemory) {
	replacement := r.tasks[0]
	replacement.ID = 21
	replacement.OutputJSON = []byte(`[{"asset_id":31,"media_type":"video"}]`)
	r.tasks = append(r.tasks, replacement)
	r.segments[0].ChosenTaskID = new(replacement.ID)
	r.episodes[0].SegmentVersion++
}

func TestDramaProgramSourceReplacementRequiresExplicitRebind(t *testing.T) {
	ctx := context.Background()
	r, s := newProgramFixture(t, nil)
	req := anchoredProgram(0, 5, 10)
	view, err := s.PutProgram(ctx, 7, 1, 1, req)
	if err != nil || view.SegmentVersion != 1 || view.EditSchema != 1 {
		t.Fatalf("initial bind: %+v %v", view, err)
	}
	replaceProgramTake(r)
	req.ExpectedSegmentVersion = new(int64(2))
	req.Program.Audio[1].GainDB = -2
	view, err = s.PutProgram(ctx, 7, 1, 1, req)
	if err != nil || view.SegmentVersion != 3 || !strings.Contains(string(r.episodes[0].ProgramJSON), `"bound_source":{"task_id":20,"asset_id":30`) {
		t.Fatalf("unrelated edit refreshed source: %+v %s %v", view, r.episodes[0].ProgramJSON, err)
	}
	manifest, prep, err := s.PrepareOrResolveEpisodeManifestV2(ctx, 7, 1, 1)
	if err != nil || prep != nil || manifest.ManifestHash != "" || len(manifest.ProgramDiagnostics) != 1 || manifest.ProgramDiagnostics[0].Reason != "anchor_source_replaced" || len(manifest.Manifest.Program.Audio) != 2 {
		t.Fatalf("replacement should stop polling before probing a cold take: %+v %+v %v", manifest, prep, err)
	}
	assertProgramComposeBlocked(t, s, 91010, "anchor_source_replaced")
	r.segments[0].ChosenTaskID = new(uint64(20))
	restored, err := s.ResolveEpisodeManifestV2(ctx, 7, 1, 1)
	if err != nil || len(restored.ManifestHash) != 64 {
		t.Fatalf("original source did not restore anchor: %+v %v", restored, err)
	}
	r.segments[0].ChosenTaskID = new(uint64(21))
	req.ExpectedSegmentVersion = new(int64(3))
	req.Program.Audio[0].Anchor.SourceF = new(int32(11))
	before := string(r.episodes[0].ProgramJSON)
	_, err = s.PutProgram(ctx, 7, 1, 1, req)
	requireProgramError(t, err, 91010, "anchor_source_replaced")
	if string(r.episodes[0].ProgramJSON) != before {
		t.Fatal("moving the anchor after replacement changed the stored binding")
	}
	req.RebindAnchorCueIDs = []string{"hit"}
	view, err = s.PutProgram(ctx, 7, 1, 1, req)
	if err != nil || view.SegmentVersion != 4 {
		t.Fatalf("rebind: %+v %v", view, err)
	}
	manifest, err = s.ResolveEpisodeManifestV2(ctx, 7, 1, 1)
	if err != nil || len(manifest.ProgramDiagnostics) != 0 || len(manifest.ManifestHash) != 64 || *manifest.Manifest.Program.Audio[0].StartF != 11 {
		t.Fatalf("repaired manifest: %+v %v", manifest, err)
	}
	compose := composeReq(t, `,"compose_request_key":"stage-c"`)
	if credits, err := s.Estimate(ctx, 7, 1, compose); err != nil || credits != 0 {
		t.Fatalf("repaired estimate failed: credits=%d err=%v", credits, err)
	}
	if _, err := s.CreateJob(ctx, 7, 1, compose); err != nil {
		t.Fatalf("repaired compose enqueue failed: %v", err)
	}
	if len(r.jobs) != 1 || !strings.Contains(string(r.jobs[0].InputJSON), `"render_version":2`) {
		t.Fatalf("repaired compose did not freeze V2 manifest: %+v", r.jobs)
	}
}

func TestDramaProgramColdVideoRejectsMismatchedBytesBeforeProbe(t *testing.T) {
	probes := &manifestProbes{rows: map[string]*model.AssetMediaProbe{}}
	r, svc, files := newSlice5ProgramFixture(t, nil, func(*programMemory) {
		t.Error("probe ran on bytes that did not match the asset hash")
	}, func(d *service.DramaDeps) { d.Probes = probes })
	files.bodies[30] = "tampered video"
	r.episodes[0].EditSchema = 1

	view, err := svc.ResolveEpisodeManifestV2(t.Context(), 7, 1, 1)
	if err != nil || view.ManifestHash != "" || len(view.ProgramDiagnostics) != 1 ||
		view.ProgramDiagnostics[0].Reason != "media_unavailable" {
		t.Fatalf("mismatched video should block the manifest: %+v %v", view, err)
	}
	if probes.calls != 0 || len(probes.rows) != 0 {
		t.Fatalf("mismatched video was cached: %d writes, %d rows", probes.calls, len(probes.rows))
	}
}

func TestDramaProgramAnchorCanMoveOnSameSourceOrBindAnotherSegment(t *testing.T) {
	r, svc := newProgramFixture(t, nil)
	otherTask := r.tasks[0]
	otherTask.ID, otherTask.NodeID = 21, "seg:"+idcodec.Encode(6)
	otherTask.OutputJSON = []byte(`[{"asset_id":31,"media_type":"video"}]`)
	r.tasks = append(r.tasks, otherTask)
	r.segments[1].ChosenTaskID = new(otherTask.ID)
	for i, tc := range []struct {
		segment uint64
		frame   int32
		task    uint64
		asset   uint64
	}{{5, 10, 20, 30}, {5, 12, 20, 30}, {6, 14, 21, 31}} {
		view, err := svc.PutProgram(t.Context(), 7, 1, 1, anchoredProgram(int64(i), tc.segment, tc.frame))
		if err != nil || view.SegmentVersion != int64(i+1) {
			t.Fatalf("anchor at %d:%d: %+v %v", tc.segment, tc.frame, view, err)
		}
		stored, err := model.DecodeStoredDramaProgram(json.RawMessage(r.episodes[0].ProgramJSON))
		if err != nil {
			t.Fatal(err)
		}
		cue := stored.Audio[0]
		want := model.DramaProgramBoundSource{TaskID: tc.task, AssetID: tc.asset, Sha256: r.media[tc.asset].Sha256}
		if cue.Anchor == nil || cue.Anchor.SourceF != tc.frame || cue.Anchor.SegmentID != tc.segment ||
			cue.BoundSource == nil || *cue.BoundSource != want {
			t.Fatalf("anchor at %d:%d: %+v, want source %+v", tc.segment, tc.frame, cue, want)
		}
	}
}

func assertProgramComposeBlocked(t *testing.T, s *service.DramaService, code int, reason string) {
	t.Helper()
	ctx := context.Background()
	req := composeReq(t, `,"compose_request_key":"stage-c"`)
	_, estimate := s.Estimate(ctx, 7, 1, req)
	_, submit := s.CreateJob(ctx, 7, 1, req)
	for _, err := range []error{estimate, submit} {
		if reason == "" {
			dramaCheckCode(t, err, code)
		} else {
			requireProgramError(t, err, code, reason)
		}
	}
}

func TestDramaProgramComposeReplaysOriginalV1AfterEdit(t *testing.T) {
	ctx := context.Background()
	r, s := newProgramFixture(t, nil)
	r.episodes[0].EditSchema = 1
	req := composeReq(t, `,"compose_request_key":"frozen-v1"`)
	first, err := s.CreateJob(ctx, 7, 1, req)
	if err != nil {
		t.Fatal(err)
	}
	frozen := string(r.jobs[0].InputJSON)
	if _, err := s.PutProgram(ctx, 7, 1, 1, anchoredProgram(0, 5, 10)); err != nil {
		t.Fatal(err)
	}
	replayed, err := s.CreateJob(ctx, 7, 1, req)
	if err != nil || replayed.ID != first.ID || len(r.jobs) != 1 || string(r.jobs[0].InputJSON) != frozen || !strings.Contains(frozen, `"render_version":1`) {
		t.Fatalf("v1 replay: %+v %v", replayed, err)
	}
}

func TestDramaProgramBrokenAssetsRemainReadableAndStopPolling(t *testing.T) {
	ctx := context.Background()
	r, s := newProgramFixture(t, nil)
	req := anchoredProgram(0, 5, 10)
	if _, err := s.PutProgram(ctx, 7, 1, 1, req); err != nil {
		t.Fatal(err)
	}
	replaceProgramTake(r) // New take is cold; audio diagnostics must already be terminal.
	delete(r.media, 40)
	view, err := s.GetProgram(ctx, 7, 1, 1)
	if err != nil || len(view.Program.Audio) != 2 || view.Program.Audio[0].AssetID != 40 {
		t.Fatalf("broken program was not readable: %+v %v", view, err)
	}
	m, p, err := s.PrepareOrResolveEpisodeManifestV2(ctx, 7, 1, 1)
	if err != nil || p != nil || m.ManifestHash != "" || len(m.ProgramDiagnostics) < 2 || m.ProgramDiagnostics[0].Reason != "media_unavailable" {
		t.Fatalf("broken program polled or lost diagnostics: %+v %+v %v", m, p, err)
	}
	req.ExpectedSegmentVersion = new(int64(2))
	_, err = s.PutProgram(ctx, 7, 1, 1, req)
	requireProgramError(t, err, 91012, "media_unavailable")
	req.Program.Audio = nil
	view, err = s.PutProgram(ctx, 7, 1, 1, req)
	if err != nil || len(view.Program.Audio) != 0 || view.SegmentVersion != 3 {
		t.Fatalf("unable to clear invalid reference: %+v %v", view, err)
	}
}

func TestDramaProgramStoredCanonicalJSON(t *testing.T) {
	for _, raw := range []string{"", "  ", "null", "{}", `{"overlays":null,"v":1,"audio":null}`} {
		p, err := model.DecodeStoredDramaProgram(json.RawMessage(raw))
		canonical, marshalErr := json.Marshal(p)
		if err != nil || marshalErr != nil || p.HasAuthoredContent() || string(canonical) != `{"v":1,"audio":[],"overlays":[]}` {
			t.Errorf("%q: %s %v %v", raw, canonical, err, marshalErr)
		}
	}
	if _, err := model.DecodeStoredDramaProgram(json.RawMessage(`{"v":`)); err == nil {
		t.Fatal("truncated stored data accepted")
	}
}

func TestDramaProgramMissingAudioBlocksEstimateAndCompose(t *testing.T) {
	ctx := context.Background()
	r, s := newProgramFixture(t, nil)
	req := anchoredProgram(0, 5, 10)
	req.Program.Audio = req.Program.Audio[1:]
	if _, err := s.PutProgram(ctx, 7, 1, 1, req); err != nil {
		t.Fatal(err)
	}
	delete(r.media, 40)
	assertProgramComposeBlocked(t, s, 91012, "media_unavailable")
}

func TestDramaContractErrorAndVerifiedProbeVersion(t *testing.T) {
	programErr := service.DramaContractError(&model.DramaProgramError{
		Msg:         "bad program",
		Diagnostics: []model.DramaProgramDiagnostic{{SegmentID: 42, Field: "overlays", Reason: "overlays_unsupported", Severity: "error"}},
	})
	requireProgramError(t, programErr, 91009, "overlays_unsupported")
	var biz *errcode.Error
	if !errors.As(programErr, &biz) || biz.Data.(service.DramaProgramDiagnosticsData).Diagnostics[0].SegmentID != idcodec.ID(42) {
		t.Fatalf("unexpected diagnostic encoding: %+v", programErr)
	}
	visualErr := service.DramaContractError(&model.DramaVisualError{Msg: "bad visual"})
	dramaCheckCode(t, visualErr, 91011)

	r, _ := newProgramFixture(t, nil)
	sha := r.media[30].Sha256
	probes := &versionedProbeCache{rows: map[int16]*model.AssetMediaProbe{
		1: {AssetID: 30, Sha256: sha, ProbeVersion: 1, NormalizedFrames24: 999},
	}}
	vSvc := service.NewDramaService(service.DramaDeps{
		Repo: r, Assets: programFiles{&asyncWarmupFiles{&composeFiles{r: r.composeMemory}}, r}, Probes: probes,
		ProbeVideo: func(context.Context, string) (videoframe.NormalizedProbe, error) {
			return videoframe.NormalizedProbe{FpsNum: 24, FpsDen: 1, NormalizedFrames24: 120}, nil
		},
	})
	view, err := vSvc.ResolveEpisodeManifestV2(context.Background(), 7, 1, 1)
	if err != nil || view.Manifest.TotalFrames != 120 || probes.rows[videoframe.NormalizedProbeVersion] == nil {
		t.Fatalf("legacy unverified v1 probe must be ignored: %+v %v %+v", view, err, probes.rows)
	}
}

type versionedProbeCache struct {
	rows map[int16]*model.AssetMediaProbe
}

func (p *versionedProbeCache) Get(_ context.Context, id uint64, sha string, version int16) (*model.AssetMediaProbe, error) {
	if row := p.rows[version]; row != nil && row.AssetID == id && row.Sha256 == sha {
		return row, nil
	}
	return nil, repository.ErrNotFound
}

func (p *versionedProbeCache) Upsert(_ context.Context, row *model.AssetMediaProbe) error {
	p.rows[row.ProbeVersion] = row
	return nil
}
