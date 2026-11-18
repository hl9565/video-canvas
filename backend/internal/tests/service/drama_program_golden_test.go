package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"video-canvas/internal/config"
	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/jsonhash"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/provider"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
	"video-canvas/internal/service/dramasegmentfake"
	"video-canvas/internal/tests/testutil"
)

// goldenProgramFiles serves real input bytes; the worker must verify them against the frozen hashes.
type goldenProgramFiles struct {
	*composeFiles
	media       map[uint64]testutil.GoldenMedia
	lookup      func(context.Context, uint64, uint64) (*model.Asset, error)
	capturePath string
}

func (a *goldenProgramFiles) Get(ctx context.Context, user, id uint64) (*model.Asset, error) {
	return a.lookup(ctx, user, id)
}

func (a *goldenProgramFiles) Open(ctx context.Context, user, id uint64) (*provider.AssetFile, error) {
	asset, err := a.Get(ctx, user, id)
	if err != nil {
		return nil, err
	}
	media, ok := a.media[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	body, err := os.Open(media.Path)
	if err != nil {
		return nil, err
	}
	return &provider.AssetFile{Asset: asset, Body: body}, nil
}

func (a *goldenProgramFiles) PrepareDramaVideo(ctx context.Context, user uint64, path string) (*model.Asset, error) {
	asset, err := a.composeFiles.PrepareDramaVideo(ctx, user, path)
	if err != nil || a.capturePath == "" {
		return asset, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(a.capturePath, data, 0600); err != nil {
		return nil, err
	}
	asset.Sha256 = jsonhash.SHA256Hex(data)
	return asset, nil
}

func goldenDramaDeps(repo service.DramaRepo, files service.DramaComposeAssets, probes service.DramaSegmentProbeStore, e *videoframe.Extractor) service.DramaDeps {
	return service.DramaDeps{
		Repo: repo, Assets: files, Probes: probes, Compose: e, PreviewSecret: "golden-program-preview",
		ProbeVideo: e.ProbeNormalized24FPS, ProbeAudio: e.ProbeProgramAudio,
	}
}

func goldenMemoryService(t *testing.T, f *testutil.GoldenProgram, e *videoframe.Extractor) (*programMemory, *service.DramaService, *goldenProgramFiles) {
	t.Helper()
	r := &programMemory{composeMemory: newComposeMemory(), media: make(map[uint64]model.Asset)}
	r.project.Ratio, r.episodes[0].EditSchema = "16:9", 1
	r.segments, r.tasks = nil, nil
	for _, shot := range f.Shots {
		r.segments = append(r.segments, shot.Segment)
		if shot.Task != nil {
			r.tasks = append(r.tasks, *shot.Task)
		}
	}
	for id, media := range f.Media {
		r.media[id] = media.Asset
	}
	files := &goldenProgramFiles{composeFiles: &composeFiles{r: r.composeMemory}, media: f.Media, lookup: r.ComposeAsset}
	probes := &manifestProbes{rows: make(map[string]*model.AssetMediaProbe)}
	s := service.NewDramaService(goldenDramaDeps(r, files, probes, e))
	t.Cleanup(s.ClosePreviews)
	return r, s, files
}

func TestDramaProgramGolden(t *testing.T) {
	e := testutil.GoldenExtractor()
	if !e.Available() {
		t.Skip("FFmpeg and ffprobe required")
	}
	f := testutil.NewGoldenProgram(t, 7, 3, 2)
	t.Run("demo_trim_19f_preview_compose", func(t *testing.T) { goldenTrimParity(t, f, e) })
	t.Run("vo_3_and_contact_edits", func(t *testing.T) { goldenTimelineEdits(t, f, e) })
	t.Run("contact_diagnostics", func(t *testing.T) { goldenContactDiagnostics(t, f, e) })
	t.Run("stage_b_i2v", func(t *testing.T) { goldenStageB(t, f, e) })
}

type goldenFrozenInput struct {
	ManifestHash string                        `json:"manifest_hash"`
	ManifestV2   *model.DramaEpisodeManifestV2 `json:"manifest_v2"`
}

func readGoldenFrozen(t *testing.T, job model.DramaJob) goldenFrozenInput {
	t.Helper()
	var frozen goldenFrozenInput
	if err := json.Unmarshal(job.InputJSON, &frozen); err != nil || frozen.ManifestV2 == nil {
		t.Fatalf("missing frozen v2 manifest: %s (%v)", job.InputJSON, err)
	}
	return frozen
}

func goldenTrimParity(t *testing.T, f *testutil.GoldenProgram, e *videoframe.Extractor) {
	t.Helper()
	r, s, files := goldenMemoryService(t, f, e)
	row := r.segments[0]
	row.SourceInF, row.SourceOutF, row.FXJSON = 24, new(int32(43)), []byte(`{"fade_in_f":1}`)
	task := testutil.GoldenVideoTask(120, 7, 2, row.ID, 140)
	row.ChosenTaskID = &task.ID
	r.segments, r.tasks = []model.DramaSegment{row}, []model.GenerationTask{*task}
	view := goldenManifest(t, s)
	sha := f.Media[140].Asset.Sha256
	if len(view.Manifest.Clips) != 1 || view.Manifest.TotalFrames != 19 || view.Manifest.Clips[0].Sha256 != sha {
		t.Fatalf("trim manifest: %+v", view.Manifest)
	}
	goldenPreviewDuration(t, s, view)
	req := goldenComposeRequest(view, "golden-trim", "clean")
	files.capturePath = filepath.Join(t.TempDir(), "compose.mp4")
	job := runSlice5Compose(t, s, req)
	assertGoldenTrimMedia(t.Context(), t, files.capturePath)
	frozen := readGoldenFrozen(t, r.jobs[0])
	if job.Status != "succeeded" || job.Result == nil || len(job.Result.Exports) != 1 || job.Result.Exports[0].ManifestHash != view.ManifestHash {
		t.Fatalf("compose did not publish the preview manifest: %+v", job)
	}
	if frozen.ManifestHash != view.ManifestHash || frozen.ManifestV2.TotalFrames != 19 ||
		len(frozen.ManifestV2.Clips) != 1 || frozen.ManifestV2.Clips[0].Sha256 != sha || testutil.FileSHA256(t, f.Media[140].Path) != sha {
		t.Fatalf("compose changed the trim snapshot or source hash: %+v", frozen)
	}
	goldenRejectChangedDownload(t, e, files, frozen.ManifestV2, f.Media[123])
}

func goldenRejectChangedDownload(t *testing.T, e *videoframe.Extractor, files *goldenProgramFiles, manifest *model.DramaEpisodeManifestV2, changed testutil.GoldenMedia) {
	t.Helper()
	clip := manifest.Clips[0]
	if changed.Asset.Sha256 == clip.Sha256 {
		t.Fatal("tamper fixture needs different source bytes")
	}
	// Keep the original asset row and change only the downloaded, still decodable bytes.
	other := *files
	other.media = map[uint64]testutil.GoldenMedia{clip.AssetID: changed}
	s := service.NewDramaService(service.DramaDeps{Assets: &other, Compose: e})
	t.Cleanup(s.ClosePreviews)
	dir := t.TempDir()
	dst := filepath.Join(dir, "tampered.mp4")
	_, err := s.RenderProgramManifest(t.Context(), 7, manifest, dir, []videoframe.ComposeProgramOutputSpec{
		{Destination: dst, Width: 1280, Height: 720, Variant: "clean"},
	}, nil)
	dramaCheckCode(t, err, 91012)
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatched source published an output: %v", err)
	}
}

func goldenPreviewDuration(t *testing.T, s *service.DramaService, view *service.DramaManifestV2ResponseView) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	req := &model.DramaPreviewReq{ManifestHash: view.ManifestHash, SegmentVersion: view.SegmentVersion, Variant: "clean"}
	stale := *req
	stale.ManifestHash = strings.Repeat("0", 64)
	_, _, err := s.CreateEpisodePreview(ctx, 7, 1, 1, &stale)
	dramaCheckCode(t, err, 91001)
	ticket, cookie, err := s.CreateEpisodePreview(ctx, 7, 1, 1, req)
	if err != nil {
		t.Fatal(err)
	}
	ready := goldenWaitPreview(ctx, t, s, ticket.Ticket)
	stream, err := url.Parse(ready.StreamURL)
	if err != nil {
		t.Fatal(err)
	}
	file, err := s.OpenEpisodePreview(ctx, 1, 1, ready.Ticket, cookie, stream.Query().Get("token"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	assertGoldenTrimMedia(ctx, t, file.Name())
}

func assertGoldenTrimMedia(ctx context.Context, t *testing.T, path string) {
	t.Helper()
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0", "-count_frames",
		"-show_entries", "stream=duration,nb_read_frames", "-of", "csv=p=0", path).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "0.791667,19" {
		t.Fatalf("19-frame render duration and frames: %s (%v)", out, err)
	}
}

func goldenWaitPreview(ctx context.Context, t *testing.T, svc *service.DramaService, ticket string) *model.DramaPreviewView {
	t.Helper()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		view, err := svc.GetEpisodePreview(ctx, 7, 1, 1, ticket)
		if err != nil || view.Status == "failed" {
			t.Fatalf("preview did not render: %+v %v", view, err)
		}
		if view.Status == "ready" {
			return view
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-tick.C:
		}
	}
}

func goldenComposeRequest(view *service.DramaManifestV2ResponseView, key string, variants ...string) *model.DramaJobReq {
	return &model.DramaJobReq{Step: "compose", Episodes: []int{1}, ComposeRequestKey: key, Compose: &model.DramaComposeReq{
		Size: "720p", Variants: variants, SegmentVersion: new(view.SegmentVersion), ManifestHash: view.ManifestHash,
	}}
}

func goldenManifest(t *testing.T, s *service.DramaService) *service.DramaManifestV2ResponseView {
	t.Helper()
	view, err := s.ResolveEpisodeManifestV2(t.Context(), 7, 1, 1)
	if err != nil || view.Manifest == nil || len(view.ManifestHash) != 64 || len(view.ProgramDiagnostics) != 0 {
		t.Fatalf("manifest unavailable: %+v %v", view, err)
	}
	return view
}

func putGoldenProgram(t *testing.T, s *service.DramaService, f *testutil.GoldenProgram, version int64, rebind ...string) *service.DramaEpisodeProgramView {
	t.Helper()
	v, err := s.PutProgram(t.Context(), 7, 1, 1, &service.PutDramaProgramReq{
		ExpectedEpisodeID: 3, ExpectedSegmentVersion: &version, Program: f.Program, RebindAnchorCueIDs: rebind,
	})
	if err != nil || v.SegmentVersion != version+1 {
		t.Fatalf("put golden program at %d: %+v %v", version, v, err)
	}
	return v
}

func goldenTimelineEdits(t *testing.T, f *testutil.GoldenProgram, e *videoframe.Extractor) {
	t.Helper()
	r, s, _ := goldenMemoryService(t, f, e)
	putGoldenProgram(t, s, f, 0)
	base := goldenManifest(t, s)
	checkGoldenCueBounds(t, base, "contact", 239, 242)
	checkGoldenCueBounds(t, base, "vo_3", 372, 465)
	c := base.Manifest.Clips
	if len(c) != 10 || c[testutil.GoldenS3].MasterInF != 228 || c[testutil.GoldenS6A].SegmentID == c[testutil.GoldenS6B].SegmentID {
		t.Fatalf("golden manifest has incorrect geometry or duplicate still segments: %+v", c)
	}
	r.segments[testutil.GoldenS5].SourceOutF = new(int32(74))
	r.segments[testutil.GoldenS6A].SourceOutF = new(int32(45))
	r.episodes[0].SegmentVersion++
	moved := goldenManifest(t, s)
	if moved.Manifest.TotalFrames != 708 || moved.Manifest.Clips[testutil.GoldenS6A].MasterInF != 398 || moved.Manifest.Clips[testutil.GoldenS6B].MasterInF != 443 {
		t.Fatalf("moving the cut did not preserve the rest of the timeline: %+v", moved.Manifest)
	}
	checkGoldenCueBounds(t, moved, "contact", 239, 242)
	checkGoldenCueBounds(t, moved, "vo_3", 372, 465)
	// Keep the bed end within the timeline while moving S3 two frames later.
	r.segments[0].SourceOutF = new(int32(116))
	r.episodes[0].SegmentVersion++
	later := goldenManifest(t, s)
	checkGoldenCueBounds(t, later, "contact", 241, 244)
	checkGoldenCueBounds(t, later, "vo_3", 372, 465)
}

func checkGoldenCueBounds(t *testing.T, view *service.DramaManifestV2ResponseView, id string, start, end int32) {
	t.Helper()
	for _, cue := range view.Manifest.Program.Audio {
		if cue.ID == id {
			if cue.StartF == nil || *cue.StartF != start || cue.EndF == nil || *cue.EndF != end {
				t.Fatalf("%s: unexpected bounds, want [%d,%d): %+v", id, start, end, cue)
			}
			if id == "contact" && start == 239 && int64(*cue.StartF)*int64(view.Manifest.SampleRate/view.Manifest.FPS) != 478000 {
				t.Fatal("S3 contact does not map to sample 478000")
			}
			return
		}
	}
	t.Fatalf("cue %q missing", id)
}

func goldenContactDiagnostics(t *testing.T, f *testutil.GoldenProgram, e *videoframe.Extractor) {
	t.Helper()
	const s3 = testutil.GoldenS3
	for _, tc := range []struct {
		name, reason string
		edit         func(*programMemory)
	}{
		{"trimmed", "anchor_frame_trimmed", func(r *programMemory) { r.segments[s3].SourceInF = 14 }},
		{"replaced", "anchor_source_replaced", func(r *programMemory) {
			task := testutil.GoldenVideoTask(117, 7, 2, r.segments[s3].ID, 135)
			r.tasks = append(r.tasks, *task)
			r.segments[s3].ChosenTaskID = &task.ID
		}},
		{"deleted", "anchor_segment_missing", func(r *programMemory) { r.segments = slices.Delete(r.segments, s3, s3+1) }},
		{"still", "anchor_non_video", func(r *programMemory) {
			stageCSegment(&r.segments[s3], model.DramaVisualStill, 44)
			r.segments[s3].SourceInF = 0
			r.segments[s3].StillAssetID = new(uint64(141))
		}},
		{"black", "anchor_non_video", func(r *programMemory) {
			stageCSegment(&r.segments[s3], model.DramaVisualBlack, 44)
			r.segments[s3].SourceInF = 0
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, s, _ := goldenMemoryService(t, f, e)
			putGoldenProgram(t, s, f, 0)
			tc.edit(r)
			r.episodes[0].SegmentVersion++
			// A deletion or trim would also shorten the bed. Keep its end in range
			// so the anchor error can be asserted without unrelated diagnostics.
			if tc.name == "deleted" || tc.name == "trimmed" {
				last := &r.segments[len(r.segments)-1]
				last.SourceOutF = new(int32(25))
				if tc.name == "deleted" {
					last.SourceOutF = new(int32(57))
				}
			}
			want := service.DramaProgramDiagnostic{CueID: "contact", SegmentID: idcodec.ID(f.Shots[s3].Segment.ID), Field: "anchor", Reason: tc.reason, Severity: "error"}
			assertGoldenComposeBlocked(t, r, s, want)
			if tc.name == "replaced" {
				checkGoldenRebind(t, r, s, f)
			}
		})
	}
}

func assertGoldenComposeBlocked(t *testing.T, r *programMemory, s *service.DramaService, want service.DramaProgramDiagnostic) {
	t.Helper()
	ctx := t.Context()
	view, err := s.ResolveEpisodeManifestV2(ctx, 7, 1, 1)
	if err != nil || view.ManifestHash != "" || !slices.Equal(view.ProgramDiagnostics, []service.DramaProgramDiagnostic{want}) {
		t.Fatalf("manifest should report exactly %+v: %+v %v", want, view, err)
	}
	req := goldenComposeRequest(view, "blocked-contact", "clean")
	_, estimate := s.Estimate(ctx, 7, 1, req)
	_, submit := s.CreateJob(ctx, 7, 1, req)
	for _, err := range []error{estimate, submit} {
		var business *errcode.Error
		if !errors.As(err, &business) || business.Code != 91010 {
			t.Fatalf("expected 91010, got %v", err)
		}
		got, ok := business.Data.(service.DramaProgramDiagnosticsData)
		if !ok || !slices.Equal(got.Diagnostics, []service.DramaProgramDiagnostic{want}) {
			t.Fatalf("expected exactly %+v, got %+v", want, business.Data)
		}
	}
	if len(r.jobs) != 0 || len(r.assets) != 0 {
		t.Fatal("blocked compose produced a job or export")
	}
}

func checkGoldenRebind(t *testing.T, r *programMemory, s *service.DramaService, f *testutil.GoldenProgram) {
	t.Helper()
	before := strings.Clone(string(r.episodes[0].ProgramJSON))
	if !strings.Contains(before, `"bound_source":{"task_id":103,"asset_id":123`) {
		t.Fatalf("old binding did not survive replacement: %s", before)
	}
	putGoldenProgram(t, s, f, 2, "contact")
	if !strings.Contains(string(r.episodes[0].ProgramJSON), `"bound_source":{"task_id":117,"asset_id":135`) {
		t.Fatalf("rebind did not save the replacement fingerprint: %s", r.episodes[0].ProgramJSON)
	}
	view := goldenManifest(t, s)
	checkGoldenCueBounds(t, view, "contact", 239, 242)
	checkGoldenCueBounds(t, view, "vo_3", 372, 465)
	req := goldenComposeRequest(view, "rebound-contact", "clean")
	if credits, err := s.Estimate(t.Context(), 7, 1, req); err != nil || credits != 0 {
		t.Fatalf("repaired estimate: %d %v", credits, err)
	}
	if _, err := s.CreateJob(t.Context(), 7, 1, req); err != nil {
		t.Fatal(err)
	}
	if frozen := readGoldenFrozen(t, r.jobs[0]); frozen.ManifestV2.Clips[testutil.GoldenS3].AssetID != 135 {
		t.Fatalf("repaired compose froze the wrong S3: %+v", frozen)
	}
}

// goldenStageBRepo adapts the shared frame-run fake to drama 7, episode 9 and canvas 10.
type goldenStageBRepo struct {
	*dramasegmentfake.Repo
	tasks    *fakeTaskRepo
	canvases takeCanvases
}

func (r *goldenStageBRepo) WithTx(ctx context.Context, fn func(repository.DramaTx) error) error {
	return r.Repo.WithTx(ctx, func(repository.DramaTx) error { return fn(r) })
}

func (r *goldenStageBRepo) WithSnapshotTx(_ context.Context, fn func(repository.DramaTx) error) error {
	return fn(r)
}

func (r *goldenStageBRepo) Project(ctx context.Context, user, id uint64, lock bool) (*model.DramaProject, error) {
	p, err := r.Repo.Project(ctx, user, id, lock)
	if err == nil {
		p.CanvasID = 10
	}
	return p, err
}

func (r *goldenStageBRepo) LockProjects(ctx context.Context, user uint64, ids []uint64) ([]model.DramaProject, error) {
	p, err := r.Project(ctx, user, 7, true)
	if err != nil || !slices.Equal(ids, []uint64{7}) {
		return nil, repository.ErrNotFound
	}
	return []model.DramaProject{*p}, nil
}

func (r *goldenStageBRepo) Episodes(ctx context.Context, user, drama uint64) ([]model.DramaEpisode, error) {
	eps, err := r.Repo.Episodes(ctx, user, drama)
	if err == nil {
		eps[0].ID = 9
	}
	return eps, err
}

func (r *goldenStageBRepo) SegmentEpisode(ctx context.Context, user, episode uint64) (*model.DramaEpisode, error) {
	if episode != 9 {
		return nil, repository.ErrNotFound
	}
	eps, err := r.Episodes(ctx, user, 7)
	if err != nil {
		return nil, err
	}
	return &eps[0], nil
}

func (r *goldenStageBRepo) LockEpisodes(ctx context.Context, user uint64, ids []uint64) ([]model.DramaEpisode, error) {
	if !slices.Equal(ids, []uint64{9}) {
		return nil, repository.ErrNotFound
	}
	return r.Episodes(ctx, user, 7)
}

func (r *goldenStageBRepo) Segments(ctx context.Context, user, episode uint64) ([]model.DramaSegment, error) {
	if _, err := r.SegmentEpisode(ctx, user, episode); err != nil {
		return nil, err
	}
	return slices.Clone(r.Rows), nil
}

func (r *goldenStageBRepo) SaveSegmentNoBump(ctx context.Context, row *model.DramaSegment, rev int64) (bool, error) {
	if _, err := r.SegmentEpisode(ctx, row.UserID, row.EpisodeID); err != nil {
		return false, err
	}
	// The shared fake validates its internal episode key when carrying out the CAS.
	copyRow := *row
	copyRow.EpisodeID = 10
	changed, err := r.Repo.SaveSegmentNoBump(ctx, &copyRow, rev)
	if err == nil {
		copyRow.EpisodeID = 9
		*row = copyRow
		r.Rows[0] = copyRow
	}
	return changed, err
}

func (r *goldenStageBRepo) SaveSegment(ctx context.Context, row *model.DramaSegment, rev int64) error {
	changed, err := r.SaveSegmentNoBump(ctx, row, rev)
	if err == nil && changed {
		return r.BumpSegmentVersion(ctx, row.UserID, row.EpisodeID)
	}
	return err
}

func (r *goldenStageBRepo) BumpSegmentVersion(ctx context.Context, user, episode uint64) error {
	if _, err := r.SegmentEpisode(ctx, user, episode); err != nil {
		return err
	}
	r.SegmentVersion++
	return nil
}

func (r *goldenStageBRepo) TasksByIDs(ctx context.Context, user uint64, ids []uint64) ([]model.GenerationTask, error) {
	return r.tasks.ListByIDs(ctx, user, ids)
}

func (r *goldenStageBRepo) SegmentTasks(_ context.Context, user, canvas uint64, node string) ([]model.GenerationTask, error) {
	var rows []model.GenerationTask
	for _, task := range r.tasks.tasks {
		if task.UserID == user && task.CanvasProjectID != nil && *task.CanvasProjectID == canvas && task.NodeID == node && !task.IsTest {
			rows = append(rows, *task)
		}
	}
	return rows, nil
}

func (r *goldenStageBRepo) Canvas(ctx context.Context, user, id uint64) (*model.CanvasProject, error) {
	return r.canvases.GetByID(ctx, user, id)
}

func (r *goldenStageBRepo) JobByComposeKey(context.Context, uint64, uint64, string) (*model.DramaJob, error) {
	return nil, repository.ErrNotFound
}

func (r *goldenStageBRepo) InsertJob(_ context.Context, job *model.DramaJob) error {
	job.ID = 1
	r.JobRows = append(r.JobRows, *job)
	return nil
}

// goldenStageBTasks runs the real generation and credit logic against the shared fake transaction.
type goldenStageBTasks struct {
	*fakeTaskRepo
	drama *goldenStageBRepo
}

func (r *goldenStageBTasks) WithTx(ctx context.Context, fn func(repository.GenerationTaskTx) error) error {
	return r.drama.WithTx(ctx, func(repository.DramaTx) error {
		return r.fakeTaskRepo.WithTx(ctx, func(repository.GenerationTaskTx) error { return fn(r) })
	})
}

func (r *goldenStageBTasks) DramaTx() repository.DramaTx { return r.drama }

type goldenStageBEnv struct {
	repo       *goldenStageBRepo
	segments   *service.DramaSegmentService
	runs       *service.DramaFrameRunService
	generation *service.GenerationTaskService
	assets     *service.AssetService
	media      map[uint64]testutil.GoldenMedia
	probes     *manifestProbes
}

func newGoldenStageB(t *testing.T, f *testutil.GoldenProgram, e *videoframe.Extractor) *goldenStageBEnv {
	t.Helper()
	assets, store, _ := newAssetTestService(config.Storage{})
	row := f.Shots[testutil.GoldenS7].Segment
	row.UserID, row.EpisodeID, row.ChosenTaskID = 1, 9, new(uint64(112))
	r := &goldenStageBRepo{Repo: dramasegmentfake.New(), tasks: newFakeTaskRepo(), canvases: takeCanvases{map[uint64]*model.CanvasProject{
		10: {BaseModel: model.BaseModel{ID: 10}, UserID: 1},
		11: {BaseModel: model.BaseModel{ID: 11}, UserID: 1},
		13: {BaseModel: model.BaseModel{ID: 13}, UserID: 2},
		14: {BaseModel: model.BaseModel{ID: 14}, UserID: 1},
	}}}
	r.canvases.rows[14].DeletedAt.Valid = true
	r.Rows, r.EditSchema = []model.DramaSegment{row}, 1
	r.tasks.addCredit(1, 500, 0)
	r.tasks.addTask(*testutil.GoldenVideoTask(112, 1, 10, row.ID, 132))
	r.tasks.nextID = 114
	media := map[uint64]testutil.GoldenMedia{132: f.Media[121], 133: f.Media[133]}
	r.Assets = make(map[uint64]model.Asset)
	for id, m := range media {
		m.Asset.ID, m.Asset.UserID = id, 1
		store.rows[id], r.Assets[id], media[id] = &m.Asset, m.Asset, m
	}
	registry := &fakeTaskRegistry{snap: frameI2VSnapshot()}
	gen := service.NewGenerationTaskService(service.GenerationTaskDeps{
		Repo: &goldenStageBTasks{r.tasks, r}, Assets: assets, Registry: registry, Limits: defaultTaskLimits(), QuoteSecret: "golden-i2v-quote",
	})
	probes := &manifestProbes{rows: make(map[string]*model.AssetMediaProbe)}
	files := &goldenProgramFiles{composeFiles: &composeFiles{}, media: media, lookup: r.ComposeAsset}
	segs := service.NewDramaSegmentService(service.DramaSegmentDeps{
		Repo: r, Assets: goldenSegmentFiles{files}, Probes: probes, ProbeVideo: e.ProbeNormalized24FPS, Tasks: gen, Canvases: r.canvases,
	})
	runs := service.NewDramaFrameRunService(service.DramaFrameRunDeps{
		Repo: r, Assets: assets, Tasks: gen, Registry: registry, Segments: segs, Canvases: r.canvases, TicketSecret: "golden-i2v-ticket",
	})
	segs.SetFrameRunEnricher(runs)
	return &goldenStageBEnv{r, segs, runs, gen, assets, media, probes}
}

type goldenSegmentFiles struct{ *goldenProgramFiles }

func (f goldenSegmentFiles) View(ctx context.Context, user, id uint64) (*model.AssetView, error) {
	a, err := f.Get(ctx, user, id)
	if err != nil {
		return nil, err
	}
	return f.ViewOf(ctx, a)
}

func goldenStageB(t *testing.T, f *testutil.GoldenProgram, e *videoframe.Extractor) {
	t.Helper()
	b := newGoldenStageB(t, f, e)
	sid := b.repo.Rows[0].ID
	ready := b.frameFromCanvas(t, sid)
	confirmed := b.confirmGoldenFrame(t, sid, ready)
	b.submitAndRecoverGoldenTake(t, sid, confirmed)
	b.clearAndAdoptGoldenTake(t, sid, e)
}

func (b *goldenStageBEnv) frameFromCanvas(t *testing.T, sid uint64) *model.DramaSegmentFrameRunView {
	t.Helper()
	ctx := t.Context()
	for _, id := range []idcodec.ID{12, 13, 14} {
		_, _, err := b.runs.Create(ctx, 1, sid, &model.CreateFrameRunReq{
			RequestKey: "canvas-guard", SegmentRevision: 1, FrameSource: model.FrameSourceCanvas, OriginCanvasID: &id,
		})
		dramaCheckCode(t, err, 91008)
	}
	run, _, err := b.runs.Create(ctx, 1, sid, &model.CreateFrameRunReq{
		RequestKey: "golden-i2v", SegmentRevision: 1, FrameSource: model.FrameSourceCanvas, OriginCanvasID: new(idcodec.ID(11)),
	})
	if err != nil || run.OriginCanvasID == nil || *run.OriginCanvasID != 11 || run.CanvasTicket == "" {
		t.Fatalf("owned canvas 11: %+v %v", run, err)
	}
	image, err := b.assets.SaveDerivedImage(ctx, 1, "frame.png", "image/png", framePNG(t, 160, 90))
	if err != nil {
		t.Fatal(err)
	}
	req := &model.SetFrameRunFrameReq{Revision: run.Revision, FrameAssetID: model.AssetID(image.ID), CanvasTicket: run.CanvasTicket,
		OriginCanvasID: new(idcodec.ID(10))}
	_, err = b.runs.SetFrame(ctx, 1, sid, uint64(run.ID), req)
	dramaCheckCode(t, err, 91008)
	req.OriginCanvasID = new(idcodec.ID(11))
	ready, err := b.runs.SetFrame(ctx, 1, sid, uint64(run.ID), req)
	if err != nil || ready.State != model.FrameRunStateFrameReady {
		t.Fatalf("valid canvas frame: %+v %v", ready, err)
	}
	return ready
}

func (b *goldenStageBEnv) confirmGoldenFrame(t *testing.T, sid uint64, ready *model.DramaSegmentFrameRunView) *model.DramaSegmentFrameRunView {
	t.Helper()
	ctx := t.Context()
	_, _, err := b.runs.Submit(ctx, 1, sid, uint64(ready.ID), &model.SubmitFrameRunReq{Revision: ready.Revision, ConfirmedVersion: 1})
	dramaCheckCode(t, err, 91005)
	input := map[string]any{"prompt": "golden I2V", "duration": 5, "resolution": "720p", "op": "i2v",
		"aspect_ratio": "16:9", "count": 1, "images": []uint64{*ready.FrameAssetID}}
	node := "seg:" + idcodec.Encode(sid)
	wrong, err := b.generation.Create(ctx, 1, "wrong-drama-canvas", &model.CreateGenerationTaskReq{
		Kind: model.KindVideo, ModelID: service.DefaultDramaI2VModelKey, CanvasID: 11, NodeID: node, Input: input,
	})
	if err != nil || len(wrong.Items) != 1 || wrong.Items[0].Error == nil || wrong.Items[0].Error.Code != 10001 {
		t.Fatalf("I2V task accepted on a canvas outside drama 7: %+v %v", wrong, err)
	}
	quote, err := b.generation.Quote(ctx, 1, &model.QuoteGenerationTaskReq{
		Kind: model.KindVideo, ModelID: service.DefaultDramaI2VModelKey, CanvasID: 10, NodeID: node, Input: input,
	}, service.QuoteTaskOptions{SegmentID: sid, SegmentRevision: 1, SingleShotOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	req := &model.ConfirmFrameRunReq{Revision: ready.Revision, Input: input, QuoteToken: quote.QuoteToken,
		FrameSHA256: strings.Repeat("0", 64)}
	_, err = b.runs.Confirm(ctx, 1, sid, uint64(ready.ID), req)
	dramaCheckCode(t, err, 91008)
	if len(b.repo.tasks.tasks) != 1 || len(b.repo.tasks.ledger) != 0 {
		t.Fatal("unconfirmed or unauthorized request created a task or froze credits")
	}
	req.FrameSHA256 = ready.FrameSHA256
	confirmed, err := b.runs.Confirm(ctx, 1, sid, uint64(ready.ID), req)
	if err != nil || confirmed.State != model.FrameRunStateConfirmed {
		t.Fatalf("confirm authorized frame: %+v %v", confirmed, err)
	}
	return confirmed
}

func (b *goldenStageBEnv) submitAndRecoverGoldenTake(t *testing.T, sid uint64, v *model.DramaSegmentFrameRunView) {
	t.Helper()
	ctx := t.Context()
	req := &model.SubmitFrameRunReq{Revision: v.Revision, ConfirmedVersion: v.ConfirmedVersion}
	_, first, err := b.runs.Submit(ctx, 1, sid, uint64(v.ID), req)
	if err != nil || first.ID != 115 || first.CanvasProjectID == nil || *first.CanvasProjectID != 10 {
		t.Fatalf("confirmed run did not create T115 on canvas 10: %+v %v", first, err)
	}
	_, replay, err := b.runs.Submit(ctx, 1, sid, uint64(v.ID), req)
	if err != nil || replay.ID != 115 {
		t.Fatalf("submit replay changed task: %+v %v", replay, err)
	}
	task := b.repo.tasks.tasks[115]
	task.Status, task.OutputJSON = model.TaskSucceeded, []byte(`[{"asset_id":133,"media_type":"video"}]`)
	for range 2 {
		if err := b.runs.RecoverUnsettledDramaFrameRuns(ctx, repository.FrameRunRecoveryFilter{}, 10); err != nil {
			t.Fatal(err)
		}
	}
	_, replay, err = b.runs.Submit(ctx, 1, sid, uint64(v.ID), req)
	if err != nil || replay.ID != 115 {
		t.Fatalf("replay after recovery changed task: %+v %v", replay, err)
	}
	seg, err := b.segments.Get(ctx, 1, sid)
	if err != nil || seg.EpisodeNo != 1 || seg.ChosenTaskID != idcodec.Encode(112) || seg.FrameRun == nil ||
		seg.FrameRun.State != model.FrameRunStateBound || len(seg.Takes) != 2 || len(b.repo.TakeLinks) != 1 ||
		b.repo.TakeLinks[0].TaskID != 115 || b.repo.TakeLinks[0].AssetID != 133 {
		t.Fatalf("recovery must bind exactly one T115 candidate without replacing T112: %+v %v", seg, err)
	}
	store := b.repo.tasks
	freeze := store.ledgerOf(115, model.LedgerFreeze)
	if len(store.tasks) != 2 || len(store.ledger) != 1 || freeze == nil || freeze.Amount <= 0 || store.credits[1].Frozen != freeze.Amount {
		t.Fatal("submit and recovery must leave exactly one task and one corresponding credit freeze")
	}
}

func (b *goldenStageBEnv) clearAndAdoptGoldenTake(t *testing.T, sid uint64, e *videoframe.Extractor) {
	t.Helper()
	ctx := t.Context()
	seg, err := b.segments.Get(ctx, 1, sid)
	if err != nil {
		t.Fatal(err)
	}
	cleared, err := b.segments.PatchTakeQC(ctx, 1, sid, 112, &model.PatchDramaTakeQCReq{
		Revision: seg.Revision, QC: model.TakeQCRejected, ClearChosen: true,
	})
	if err != nil || !cleared.ChosenCleared || cleared.ChosenTaskID != "" {
		t.Fatalf("reject chosen T112: %+v %v", cleared, err)
	}
	chosen, err := b.segments.Update(ctx, 1, sid, &model.UpdateDramaSegmentReq{Revision: cleared.Revision,
		ChosenTaskID: new(idcodec.ID(115)), Edit: &model.DramaSegmentEditPatch{
			SourceOutF: model.NullableInt32{Set: true, Value: new(int32(81))}, MuteOriginal: new(true),
		}, FX: &model.DramaSegmentFX{FadeInF: 1},
	})
	if err != nil || chosen.ChosenCleared || chosen.ChosenTaskID != idcodec.Encode(115) {
		t.Fatalf("explicitly adopt T115: %+v %v", chosen, err)
	}
	files := &goldenProgramFiles{composeFiles: &composeFiles{}, media: b.media, lookup: b.repo.ComposeAsset}
	s := service.NewDramaService(goldenDramaDeps(b.repo, files, b.probes, e))
	t.Cleanup(s.ClosePreviews)
	view, err := s.ResolveEpisodeManifestV2(ctx, 1, 7, 1)
	if err != nil || len(view.ManifestHash) != 64 {
		t.Fatalf("resolve T115: %+v %v", view, err)
	}
	if _, err := s.CreateJob(ctx, 1, 7, goldenComposeRequest(view, "golden-t115-freeze", "clean")); err != nil {
		t.Fatal(err)
	}
	frozen := readGoldenFrozen(t, b.repo.JobRows[0]).ManifestV2
	if frozen.EpisodeID != 9 || frozen.TotalFrames != 81 || len(frozen.Clips) != 1 || frozen.Clips[0].TaskID != 115 ||
		frozen.Clips[0].AssetID != 133 || frozen.Clips[0].Sha256 != b.media[133].Asset.Sha256 || !frozen.Clips[0].MuteOriginal {
		t.Fatalf("T115 did not freeze A133 at [0,81): %+v", frozen)
	}
}
