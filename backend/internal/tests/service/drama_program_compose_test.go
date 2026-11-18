package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/idcodec"

	"video-canvas/internal/pkg/jsonhash"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/provider"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
)

func TestDramaProgramComposeFreezesBlackOnlyWithoutVideoAssets(t *testing.T) {
	renderer := &slice5ProgramRenderer{}
	r, svc := newProgramFixture(t, nil, func(d *service.DramaDeps) { d.Compose = renderer })
	r.segments[0].VisualKind = model.DramaVisualBlack
	r.segments[0].SourceOutF = new(int32(24))
	r.segments[0].ChosenTaskID = nil
	view, err := svc.ResolveEpisodeManifestV2(t.Context(), 7, 1, 1)
	if err != nil || view.ManifestHash == "" {
		t.Fatalf("black manifest: %+v %v", view, err)
	}
	job, err := svc.CreateJob(t.Context(), 7, 1, composeReq(t, `,"compose":{"manifest_hash":"`+view.ManifestHash+`","variants":["packaged","clean"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var input struct {
		V1            *model.DramaEpisodeManifest   `json:"manifest"`
		V2            *model.DramaEpisodeManifestV2 `json:"manifest_v2"`
		RenderVersion int                           `json:"render_version"`
		AssetIDs      []uint64                      `json:"asset_ids"`
		Hash          string                        `json:"manifest_hash"`
	}
	if err = json.Unmarshal(r.jobs[0].InputJSON, &input); err != nil || input.V1 != nil || input.V2 == nil ||
		input.RenderVersion != 2 || len(input.AssetIDs) != 0 || input.V2.TotalFrames != 24 || input.Hash != view.ManifestHash {
		t.Fatalf("incorrect frozen input: %s (%v)", r.jobs[0].InputJSON, err)
	}
	if err := svc.HandleDrama(t.Context(), 7, 1, uint64(job.ID)); err != nil {
		t.Fatal(err)
	}
	jobs, err := svc.ListJobs(t.Context(), 7, 1, false)
	if err != nil || len(jobs) != 1 || jobs[0].Status != "succeeded" {
		t.Fatalf("compose did not publish: %+v %v", jobs, err)
	}
	checkSlice5SilenceExports(t, jobs[0].Result)
}

type slice5ProgramRenderer struct {
	manifestProcessor
	during func(context.Context, videoframe.ComposeProgramInput) error
}

func (r *slice5ProgramRenderer) ComposeProgramManifest(ctx context.Context, in videoframe.ComposeProgramInput) (videoframe.ProgramComposeReport, error) {
	if r.during != nil {
		if err := r.during(ctx, in); err != nil {
			return videoframe.ProgramComposeReport{}, err
		}
	}
	for _, out := range in.Outputs {
		if err := os.WriteFile(out.Destination, []byte(out.Variant), 0600); err != nil {
			return videoframe.ProgramComposeReport{}, err
		}
	}
	report := videoframe.ProgramComposeReport{AudioSHA256: strings.Repeat("1", 64), LoudnessStatus: "silence"}
	if len(in.Audio) != 0 {
		report.LoudnessStatus, report.LUFS, report.TruePeak = "accepted", new(-16.1), new(-2.1)
	}
	return report, nil
}

func checkSlice5SilenceExports(t *testing.T, result *model.DramaComposeResult) {
	t.Helper()
	if result == nil || len(result.Exports) != 2 {
		t.Fatalf("expected two exports, got %+v", result)
	}
	for i, variant := range []string{"clean", "packaged"} {
		export := result.Exports[i]
		raw, err := json.Marshal(export)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		if export.Variant != variant || export.Width != 1280 || export.Height != 720 ||
			export.DramaComposeExportReport == nil || export.AudioSHA256 != strings.Repeat("1", 64) ||
			string(wire["lufs"]) != "null" || string(wire["true_peak"]) != "null" || string(wire["loudness_status"]) != `"silence"` {
			t.Fatalf("wrong export or flat silence report: %s", raw)
		}
	}
}

type slice5ProgramFiles struct {
	programFiles
	bodies     map[uint64]string
	beforeSave func(context.Context, int) error
	discardCtx error
	hashes     int
}

func (f *slice5ProgramFiles) Open(ctx context.Context, user, id uint64) (*provider.AssetFile, error) {
	a, err := f.Get(ctx, user, id)
	if err != nil {
		return nil, err
	}
	return &provider.AssetFile{Asset: a, Body: io.NopCloser(strings.NewReader(f.bodies[id]))}, nil
}

func (f *slice5ProgramFiles) EnsureSha256(ctx context.Context, user, id uint64) (string, *model.Asset, error) {
	if f.inTx {
		return "", nil, errors.New("hash computed inside a write transaction")
	}
	a, err := f.Get(ctx, user, id)
	if err != nil {
		return "", nil, err
	}
	f.hashes++
	a.Sha256 = jsonhash.SHA256Hex([]byte(f.bodies[id]))
	f.media[id] = *a
	return a.Sha256, a, nil
}

func (f *slice5ProgramFiles) PrepareDramaVideo(ctx context.Context, user uint64, path string) (*model.Asset, error) {
	if f.beforeSave != nil {
		if err := f.beforeSave(ctx, f.prepared+1); err != nil {
			return nil, err
		}
	}
	return f.composeFiles.PrepareDramaVideo(ctx, user, path)
}

func (f *slice5ProgramFiles) DiscardCopies(ctx context.Context, assets []*model.Asset) {
	f.discardCtx = ctx.Err()
	f.composeFiles.DiscardCopies(ctx, assets)
}

func newSlice5ProgramFixture(t *testing.T, renderer service.DramaVideoComposer, duringProbe func(*programMemory), configure ...func(*service.DramaDeps)) (*programMemory, *service.DramaService, *slice5ProgramFiles) {
	t.Helper()
	var files *slice5ProgramFiles
	r, svc := newProgramFixture(t, duringProbe, func(d *service.DramaDeps) {
		files = &slice5ProgramFiles{programFiles: d.Assets.(programFiles), bodies: map[uint64]string{30: "video", 31: "video2", 32: "still", 40: "audio"}}
		for id, a := range files.media {
			a.Sha256 = jsonhash.SHA256Hex([]byte(files.bodies[id]))
			files.media[id] = a
		}
		d.Assets, d.Compose = files, renderer
		for _, apply := range configure {
			apply(d)
		}
	})
	return r, svc, files
}

func stageCSegment(seg *model.DramaSegment, kind string, frames int32) {
	seg.VisualKind, seg.ChosenTaskID, seg.SourceOutF = kind, nil, &frames
	if kind == model.DramaVisualStill {
		seg.StillAssetID = new(uint64(32))
	}
}

func runSlice5Compose(t *testing.T, svc *service.DramaService, req *model.DramaJobReq) model.DramaJobView {
	t.Helper()
	job, err := svc.CreateJob(t.Context(), 7, 1, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.HandleDrama(t.Context(), 7, 1, uint64(job.ID)); err != nil {
		t.Fatal(err)
	}
	jobs, err := svc.ListJobs(t.Context(), 7, 1, false)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("compose result unavailable: %+v %v", jobs, err)
	}
	return jobs[0]
}

func TestDramaProgramComposeStillAndMixedFreezeVerifiedAssets(t *testing.T) {
	t.Run("still only with historical missing hash", func(t *testing.T) {
		r, svc, files := newSlice5ProgramFixture(t, &slice5ProgramRenderer{}, nil)
		stageCSegment(&r.segments[0], model.DramaVisualStill, 24)
		still := r.media[32]
		still.Sha256 = ""
		r.media[32] = still
		job := runSlice5Compose(t, svc, composeReq(t, `,"compose":{"variants":["clean","packaged"]}`))
		if job.Status != "succeeded" || files.hashes != 1 || !strings.Contains(string(r.jobs[0].InputJSON), `"asset_ids":[]`) {
			t.Fatalf("hashless still did not freeze and render: %+v, %s", job, r.jobs[0].InputJSON)
		}
		checkSlice5SilenceExports(t, job.Result)
	})
	t.Run("video+still+audio at 1080p", func(t *testing.T) {
		renderer := &slice5ProgramRenderer{during: func(_ context.Context, in videoframe.ComposeProgramInput) error {
			if len(in.Clips) != 2 || in.Clips[0].SourcePath == "" || in.Clips[1].StillPath == "" ||
				len(in.Audio) != 1 || in.Audio[0].DecodedAudioSamples != 12000 ||
				len(in.Outputs) != 2 || in.Outputs[1].Width != 1920 || in.Outputs[1].Height != 1080 || in.Outputs[1].WithOverlays {
				return errors.New("incomplete mixed render")
			}
			return nil
		}}
		r, svc, files := newSlice5ProgramFixture(t, renderer, nil)
		stageCSegment(&r.segments[1], model.DramaVisualStill, 24)
		saveProgramAudioFixture(t, r, absoluteProgramCue("voice", model.DramaAudioNarration, 0, 24))
		for _, id := range []uint64{30, 40} {
			a := r.media[id]
			a.Sha256 = ""
			r.media[id] = a
		}
		job := runSlice5Compose(t, svc, composeReq(t, `,"compose":{"size":"1080p","variants":["clean","packaged"]}`))
		if job.Status != "succeeded" || job.Result == nil || len(job.Result.Exports) != 2 || files.hashes != 2 {
			t.Fatalf("missing mixed exports: %+v", job)
		}
		for _, export := range job.Result.Exports {
			if export.DramaComposeExportReport == nil || export.AudioSHA256 != strings.Repeat("1", 64) ||
				export.LoudnessStatus != "accepted" || export.LUFS == nil || *export.LUFS != -16.1 ||
				export.TruePeak == nil || *export.TruePeak != -2.1 {
				t.Fatalf("incorrect mixed export report: %+v", export)
			}
		}
	})
}

func TestDramaProgramComposeRollsBackUploadedVariants(t *testing.T) {
	for _, failure := range []string{"second upload", "canceled job", "deleted episode", "insert failed", "canceled context"} {
		t.Run(failure, func(t *testing.T) {
			r, svc, files := newSlice5ProgramFixture(t, &slice5ProgramRenderer{}, nil, func(d *service.DramaDeps) {
				if failure == "insert failed" {
					d.Repo = &slice5FailSecondRegistration{d.Repo.(*programMemory)}
				}
			})
			stageCSegment(&r.segments[0], model.DramaVisualBlack, 24)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			job, err := svc.CreateJob(ctx, 7, 1, composeReq(t, `,"compose":{"variants":["clean","packaged"]}`))
			if err != nil {
				t.Fatal(err)
			}
			files.beforeSave = func(_ context.Context, n int) error {
				if n != 2 {
					return nil
				}
				switch failure {
				case "second upload":
					cancel()
					return errors.New("upload failed")
				case "canceled job":
					_, err := svc.CancelJob(ctx, 7, 1, uint64(job.ID))
					return err
				case "deleted episode":
					r.episodes = nil
				case "canceled context":
					cancel()
				}
				return nil
			}
			err = svc.HandleDrama(ctx, 7, 1, uint64(job.ID))
			if (failure == "insert failed" || failure == "canceled context") && err == nil {
				t.Fatal("expected worker failure")
			}
			wantDiscard := 2
			if failure == "second upload" {
				wantDiscard = 1
			}
			if files.discarded != wantDiscard || files.discardCtx != nil || len(r.assets) != 0 ||
				len(r.episodes) != 0 && r.episodes[0].FinalAssetID != nil || r.jobs[0].Status == "succeeded" {
				t.Fatalf("rollback published a result or leaked files: discarded=%d ctx=%v rows=%d status=%s",
					files.discarded, files.discardCtx, len(r.assets), r.jobs[0].Status)
			}
		})
	}
}

// slice5FailSecondRegistration fails after one row has been inserted into the transaction.
type slice5FailSecondRegistration struct{ *programMemory }

func (r *slice5FailSecondRegistration) WithTx(ctx context.Context, fn func(repository.DramaTx) error) error {
	return r.programMemory.WithTx(ctx, func(repository.DramaTx) error { return fn(r) })
}

func (r *slice5FailSecondRegistration) CreateAsset(ctx context.Context, asset *model.Asset) error {
	if len(r.assets) == 1 {
		return errors.New("second row failed")
	}
	return r.programMemory.CreateAsset(ctx, asset)
}

func TestDramaProgramComposeVerifiesFrozenDownloadBytes(t *testing.T) {
	for _, kind := range []string{model.KindVideo, model.KindImage, model.KindAudio} {
		t.Run(kind, func(t *testing.T) {
			renderer := &slice5ProgramRenderer{during: func(context.Context, videoframe.ComposeProgramInput) error {
				t.Error("tampered bytes reached renderer")
				return nil
			}}
			r, svc, files := newSlice5ProgramFixture(t, renderer, nil)
			stageCSegment(&r.segments[1], model.DramaVisualStill, 24)
			saveProgramAudioFixture(t, r, absoluteProgramCue("voice", model.DramaAudioNarration, 0, 24))
			job, err := svc.CreateJob(t.Context(), 7, 1, composeReq(t, ""))
			if err != nil {
				t.Fatal(err)
			}
			id := map[string]uint64{model.KindVideo: 30, model.KindImage: 32, model.KindAudio: 40}[kind]
			files.bodies[id] = "tampered but row hash unchanged"
			if err := svc.HandleDrama(t.Context(), 7, 1, uint64(job.ID)); err != nil {
				t.Fatal(err)
			}
			if r.jobs[0].Status != "failed" || r.jobs[0].ErrorCode != 91012 || files.prepared != 0 || len(r.assets) != 0 ||
				r.episodes[0].FinalAssetID != nil {
				t.Fatalf("tamper was published: %+v", r.jobs[0])
			}
		})
	}
}

func TestDramaProgramComposeDiagnosticsBlockEstimateAndSubmit(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		code         int
	}{
		{"anchor", "anchor_source_replaced", 91010},
		{"audio", "media_unavailable", 91012},
		{"narration", "narration_overflow", 91009},
		{"visual", "visual_invalid", 91011},
		{"ratio", "unsupported_ratio", 91011},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, svc, _ := newSlice5ProgramFixture(t, nil, nil)
			switch tc.name {
			case "anchor":
				if _, err := svc.PutProgram(t.Context(), 7, 1, 1, anchoredProgram(0, 5, 10)); err != nil {
					t.Fatal(err)
				}
				replaceProgramTake(r)
			case "audio":
				saveProgramAudioFixture(t, r, absoluteProgramCue("voice", model.DramaAudioNarration, 0, 24))
				delete(r.media, 40)
			case "narration":
				saveProgramAudioFixture(t, r, absoluteProgramCue("voice", model.DramaAudioNarration, 0, 3))
			case "visual":
				stageCSegment(&r.segments[0], model.DramaVisualBlack, 0)
			case "ratio":
				stageCSegment(&r.segments[0], model.DramaVisualBlack, 24)
				r.project.Ratio = "9:16"
			}
			assertProgramComposeBlocked(t, svc, tc.code, tc.reason)
			if len(r.jobs) != 0 || len(r.assets) != 0 || r.creditCalls != 0 {
				t.Fatal("rejected compose had side effects")
			}
		})
	}
}

func TestDramaProgramComposeRejectsClientAndTransactionStaleness(t *testing.T) {
	for _, key := range []string{"client hash", "video hash", "program", "skipped edit", "ratio", "episode version"} {
		t.Run(key, func(t *testing.T) {
			mutate := func(r *programMemory) {
				switch key {
				case "video hash":
					asset := r.media[30]
					asset.Sha256 = strings.Repeat("a", 64)
					r.media[30] = asset
				case "program":
					saveProgramAudioFixture(t, r, absoluteProgramCue("new", model.DramaAudioSFX, 0, 24))
				case "skipped edit":
					r.segments[1].SourceInF++
				case "ratio":
					r.project.Ratio = "9:16"
				case "episode version":
					r.episodes[0].SegmentVersion++
				}
			}
			r, svc, _ := newSlice5ProgramFixture(t, nil, mutate)
			saveProgramAudioFixture(t, r, absoluteProgramCue("voice", model.DramaAudioNarration, 0, 24))
			req := composeReq(t, "")
			if key == "client hash" {
				req.Compose = &model.DramaComposeReq{ManifestHash: strings.Repeat("e", 64)}
			}
			_, err := svc.CreateJob(t.Context(), 7, 1, req)
			dramaCheckCode(t, err, 91001)
			if len(r.jobs) != 0 || len(r.assets) != 0 {
				t.Fatal("stale compose created a job or asset")
			}
		})
	}
}

func TestDramaProgramComposeFFmpegPublishesSharedMaster(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("FFmpeg is required")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("FFprobe is required")
	}
	renderer := videoframe.New(videoframe.Options{})
	r, svc, files := newSlice5ProgramFixture(t, renderer, nil, func(d *service.DramaDeps) {
		d.ProbeVideo, d.ProbeAudio = renderer.ProbeNormalized24FPS, renderer.ProbeProgramAudio
	})
	t.Run("black", func(t *testing.T) {
		stageCSegment(&r.segments[0], model.DramaVisualBlack, 4)
		job := runSlice5Compose(t, svc, composeReq(t, `,"compose":{"variants":["clean","packaged"]}`))
		requireSlice5RenderedReport(t, job, "silence")
	})
	t.Run("still", func(t *testing.T) {
		r.jobs = nil
		var pixel bytes.Buffer
		if err := png.Encode(&pixel, image.NewRGBA(image.Rect(0, 0, 16, 16))); err != nil {
			t.Fatal(err)
		}
		setSlice5MediaBody(files, 32, pixel.String())
		stageCSegment(&r.segments[0], model.DramaVisualStill, 4)
		job := runSlice5Compose(t, svc, composeReq(t, `,"compose":{"variants":["clean","packaged"]}`))
		requireSlice5RenderedReport(t, job, "silence")
	})
	t.Run("video, still, black and narration", func(t *testing.T) {
		r.jobs = nil
		setSlice5MediaBody(files, 30, slice5MediaFixture(t, "video.mp4", "color=blue:s=64x64:r=24:d=1", "-c:v", "libx264", "-pix_fmt", "yuv420p"))
		setSlice5MediaBody(files, 40, slice5MediaFixture(t, "voice.wav", "sine=f=1000:r=48000:d=1"))
		r.segments[0].Seq = 1 // Keep the still, after the adopted video.
		r.segments[1].Seq = 2
		stageCSegment(&r.segments[1], model.DramaVisualBlack, 20)
		chosen := uint64(20)
		r.tasks[0].NodeID = "seg:" + idcodec.Encode(7)
		r.segments = append(r.segments, model.DramaSegment{
			BaseModel: model.BaseModel{ID: 7}, UserID: 7, EpisodeID: 3, ChosenTaskID: &chosen,
		})
		saveProgramAudioFixture(t, r, absoluteProgramCue("voice", model.DramaAudioNarration, 0, 24))
		job := runSlice5Compose(t, svc, composeReq(t, `,"compose":{"variants":["clean","packaged"]}`))
		requireSlice5RenderedReport(t, job, "accepted")
	})
}

func setSlice5MediaBody(files *slice5ProgramFiles, id uint64, body string) {
	files.bodies[id] = body
	a := files.media[id]
	a.Sha256 = jsonhash.SHA256Hex([]byte(body))
	files.media[id] = a
}

func slice5MediaFixture(t *testing.T, name, source string, encoding ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	args := append([]string{"-v", "error", "-f", "lavfi", "-i", source}, encoding...)
	if out, err := exec.CommandContext(t.Context(), "ffmpeg", append(args, path)...).CombinedOutput(); err != nil {
		t.Fatalf("create %s: %v %s", name, err, out)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func requireSlice5RenderedReport(t *testing.T, job model.DramaJobView, status string) {
	t.Helper()
	if job.Status != "succeeded" || job.Result == nil || len(job.Result.Exports) != 2 {
		t.Fatalf("FFmpeg compose failed: %+v", job)
	}
	a, b := job.Result.Exports[0].DramaComposeExportReport, job.Result.Exports[1].DramaComposeExportReport
	if a == nil || b == nil || len(a.AudioSHA256) != 64 || a.AudioSHA256 != b.AudioSHA256 ||
		a.LoudnessStatus != status || b.LoudnessStatus != status {
		t.Fatalf("variants must report the same accepted master: %+v %+v", a, b)
	}
	for _, report := range []*model.DramaComposeExportReport{a, b} {
		if status == "silence" && (report.LUFS != nil || report.TruePeak != nil) {
			t.Fatalf("silence must have no measurements: %+v", report)
		}
		if status == "accepted" && (report.LUFS == nil || math.Abs(*report.LUFS+16) > 0.5 ||
			report.TruePeak == nil || *report.TruePeak > -1.5) {
			t.Fatalf("measurements outside acceptance: %+v", report)
		}
	}
}

func TestDramaProgramComposePublishesFrozenSnapshotAfterProgramEdit(t *testing.T) {
	renderer := &slice5ProgramRenderer{}
	r, svc, _ := newSlice5ProgramFixture(t, renderer, nil)
	stageCSegment(&r.segments[0], model.DramaVisualBlack, 24)
	edit := func(end int32, version int64) *service.PutDramaProgramReq {
		return &service.PutDramaProgramReq{ExpectedEpisodeID: 3, ExpectedSegmentVersion: &version,
			Program: service.DramaProgramInput{Version: 1, Audio: []service.DramaProgramAudioInput{absoluteProgramCue("voice", model.DramaAudioNarration, 0, end)}}}
	}
	if _, err := svc.PutProgram(t.Context(), 7, 1, 1, edit(12, 0)); err != nil {
		t.Fatal(err)
	}
	renderer.during = func(ctx context.Context, input videoframe.ComposeProgramInput) error {
		if len(input.Audio) != 1 || input.Audio[0].EndF != 12 {
			return errors.New("renderer did not consume the frozen audio cue")
		}
		_, err := svc.PutProgram(ctx, 7, 1, 1, edit(24, 1))
		return err
	}
	job := runSlice5Compose(t, svc, composeReq(t, `,"compose":{"variants":["clean","packaged"]}`))
	view, err := svc.Get(t.Context(), 7, 1)
	if err != nil || job.Status != "succeeded" || job.Result == nil || len(job.Result.Exports) != 2 {
		t.Fatalf("frozen compose did not publish after edit: %+v %v", job, err)
	}
	ep := view.Episodes[0]
	if !ep.FinalStale || ep.SegmentVersion != 2 || r.episodes[0].FinalSegmentVersion != 1 || ep.FinalComposeJobID == nil || *ep.FinalComposeJobID != job.ID {
		t.Fatalf("frozen final version or job reference wrong: %+v", ep)
	}
	program, err := svc.GetProgram(t.Context(), 7, 1, 1)
	if err != nil || len(program.Program.Audio) != 1 || program.Program.Audio[0].EndF == nil || *program.Program.Audio[0].EndF != 24 {
		t.Fatalf("program edit was lost: %+v %v", program, err)
	}
}
