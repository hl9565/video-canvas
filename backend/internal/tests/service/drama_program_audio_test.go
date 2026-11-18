package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/jsonhash"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/provider"
	"video-canvas/internal/service"
)

func TestDramaProgramAudioManifestDiagnosesExactNarrationOverflow(t *testing.T) {
	r, svc := newProgramFixture(t, nil)
	cue := absoluteProgramCue("vo_3", model.DramaAudioNarration, 2, 5)
	cue.AvPtsOffsetMs = 1
	save := func() {
		t.Helper()
		saveProgramAudioFixture(t, r, cue)
	}
	save()
	got, err := svc.ResolveEpisodeManifestV2(t.Context(), 7, 1, 1)
	if err != nil || got.ManifestHash != "" || len(got.ProgramDiagnostics) != 1 {
		t.Fatalf("overflow manifest: %+v %v", got, err)
	}
	d := got.ProgramDiagnostics[0]
	if d.CueID != "vo_3" || d.Field != "end_f" || d.Reason != "narration_overflow" || d.Severity != "error" ||
		!strings.Contains(got.Diagnostics[0], "差 6048 个采样") {
		t.Fatalf("overflow diagnostics: %+v %v", d, got.Diagnostics)
	}
	cue.SrcOutSample = new(int64(5952))
	save()
	got, err = svc.ResolveEpisodeManifestV2(t.Context(), 7, 1, 1)
	if err != nil || len(got.ManifestHash) != 64 || len(got.ProgramDiagnostics) != 0 {
		t.Fatalf("exact explicit trim: %+v %v", got, err)
	}
	cue.SrcOutSample, cue.EndF = nil, new(int32(9))
	save()
	got, err = svc.ResolveEpisodeManifestV2(t.Context(), 7, 1, 1)
	if err != nil || len(got.ManifestHash) != 64 {
		t.Fatalf("wider window: %+v %v", got, err)
	}
}

func TestDramaProgramAudioRenderReportsDecodedAcceptance(t *testing.T) {
	renderer := &programReportRenderer{}
	_, svc := newProgramFixture(t, nil, func(d *service.DramaDeps) { d.Compose = renderer })
	manifest := &model.DramaEpisodeManifestV2{
		Version: 2, RenderVersion: 2, FPS: 24, SampleRate: 48000, TotalFrames: 24,
		Clips: []model.DramaManifestV2Clip{{DramaManifestV2ClipParams: model.DramaManifestV2ClipParams{
			VisualKind: model.DramaVisualBlack, SourceOutF: 24, MasterOutF: 24,
		}}},
		Program: model.DramaManifestV2Program{Version: 1, Audio: []model.DramaManifestV2AudioCue{{
			DramaProgramAudioSource: model.DramaProgramAudioSource{AssetID: 40, DramaProgramAudioParams: model.DramaProgramAudioParams{
				ID: "bed", Role: model.DramaAudioBGM, StartF: new(int32), EndF: new(int32(24)), FadeInF: 1, FadeOutF: 2, DuckDB: new(-8.0),
			}},
			Sha256: jsonhash.SHA256Hex([]byte("audio")), DecodedAudioSamples: 12000,
		}}},
	}
	dir := t.TempDir()
	specs := []videoframe.ComposeProgramOutputSpec{{Width: 1280, Height: 720, Variant: "clean", Destination: filepath.Join(dir, "clean.mp4")}}
	got, err := svc.RenderProgramManifest(t.Context(), 7, manifest, dir, specs, nil)
	if err != nil || got.AudioSHA256 != renderer.reportHash() || got.LoudnessStatus != "accepted" ||
		got.LUFS == nil || *got.LUFS != -16.1 || got.TruePeak == nil || *got.TruePeak != -2.1 {
		t.Fatalf("export report: %+v %v", got, err)
	}
	renderer.err = fmt.Errorf("retries exhausted: %w", videoframe.ErrLoudnessAcceptance)
	_, err = svc.RenderProgramManifest(t.Context(), 7, manifest, dir, specs, nil)
	dramaCheckCode(t, err, 91013)

	manifest.Program.Audio[0].Sha256 = strings.Repeat("e", 64)
	_, err = svc.RenderProgramManifest(t.Context(), 7, manifest, dir, specs, nil)
	dramaCheckCode(t, err, 91012)
}

type programReportRenderer struct {
	composeProcessor
	err error
}

func (*programReportRenderer) reportHash() string { return strings.Repeat("1", 64) }

func (r *programReportRenderer) ComposeProgramManifest(_ context.Context, in videoframe.ComposeProgramInput) (videoframe.ProgramComposeReport, error) {
	raw, err := os.ReadFile(in.Audio[0].SourcePath)
	if err != nil || string(raw) != "audio" || in.Audio[0].DecodedAudioSamples != 12000 || in.Clips[0].SourcePath != "" ||
		in.Audio[0].FadeInF != 1 || in.Audio[0].FadeOutF != 2 || in.Audio[0].DuckDB == nil || *in.Audio[0].DuckDB != -8 {
		return videoframe.ProgramComposeReport{}, fmt.Errorf("incorrect render input: %+v, %w", in, err)
	}
	return videoframe.ProgramComposeReport{AudioSHA256: r.reportHash(), LoudnessStatus: "accepted", LUFS: new(-16.1), TruePeak: new(-2.1)}, r.err
}

func TestDramaProgramAudioWarmsHashlessAssetAndCachesByVersion(t *testing.T) {
	var calls atomic.Int32
	var files *programHashlessFiles
	entered, release := make(chan struct{}), make(chan struct{})
	r, svc := newProgramFixture(t, nil, func(d *service.DramaDeps) {
		files = &programHashlessFiles{programFiles: d.Assets.(programFiles)}
		d.Assets = files
		cached := d.Probes.(*manifestProbes)
		sha := jsonhash.SHA256Hex([]byte("audio"))
		cached.rows[sha] = &model.AssetMediaProbe{AssetID: 40, Sha256: sha, ProbeVersion: 1, NormalizedFrames24: 24}
		d.ProbeAudio = func(ctx context.Context, path string) (videoframe.AudioProbe, error) {
			if calls.Add(1) == 1 {
				close(entered)
			}
			select {
			case <-ctx.Done():
				return videoframe.AudioProbe{}, ctx.Err()
			case <-release:
			}
			b, err := os.ReadFile(path)
			if err != nil || string(b) != "audio" || files.inTx {
				t.Errorf("audio probe did not read the downloaded source outside the write lock: %q %v", b, err)
			}
			return videoframe.AudioProbe{DecodedAudioSamples: 12000, AStartUs: 123000}, nil
		}
	})
	saveProgramAudioFixture(t, r, absoluteProgramCue("music", model.DramaAudioBGM, 0, 12), absoluteProgramCue("voice", model.DramaAudioNarration, 0, 12))
	requireColdProgramAudio(t, svc)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("audio probe did not start")
	}
	// 轮询看见已回填的哈希但还没有探针，应继续使用原有预热任务。
	for range 5 {
		requireColdProgramAudio(t, svc)
	}
	close(release)
	view := pollProgramAudioManifest(t, svc)
	if len(view.ManifestHash) != 64 || len(view.Manifest.Program.Audio) != 2 || len(view.ProgramDiagnostics) != 0 ||
		files.hashCalls.Load() != 1 || calls.Load() != 1 {
		t.Fatalf("warmup did not persist and reuse the correct version: %+v, hash %d, probe %d", view, files.hashCalls.Load(), calls.Load())
	}
	again, err := svc.ResolveEpisodeManifestV2(t.Context(), 7, 1, 1)
	if err != nil || again.ManifestHash != view.ManifestHash || calls.Load() != 1 {
		t.Fatalf("synchronous resolver did not reuse warm probe: %+v %v", again, err)
	}
}

func TestDramaProgramAudioBadSourceTerminatesPolling(t *testing.T) {
	for _, tc := range []struct {
		name       string
		missing    bool
		probeCalls int32
	}{{"missing object", true, 0}, {"undecodable", false, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			r, svc := newProgramFixture(t, nil, func(d *service.DramaDeps) {
				if tc.missing {
					d.Assets = programMissingAudioFiles{d.Assets.(programFiles)}
				}
				d.ProbeAudio = func(context.Context, string) (videoframe.AudioProbe, error) {
					calls.Add(1)
					return videoframe.AudioProbe{}, videoframe.ErrDecode
				}
			})
			saveProgramAudioFixture(t, r, absoluteProgramCue("voice", model.DramaAudioNarration, 0, 12))
			requireColdProgramAudio(t, svc)
			first := pollProgramAudioManifest(t, svc)
			again, pending, err := svc.PrepareOrResolveEpisodeManifestV2(t.Context(), 7, 1, 1)
			if err != nil || pending != nil || calls.Load() != tc.probeCalls {
				t.Fatalf("failure restarted warmup: %+v %v (%d probes)", pending, err, calls.Load())
			}
			for _, v := range []*service.DramaManifestV2ResponseView{first, again} {
				if v.ManifestHash != "" || len(v.ProgramDiagnostics) != 1 || v.ProgramDiagnostics[0].Reason != "media_unavailable" {
					t.Fatalf("failed audio must remain terminal with a cue diagnostic: %+v", v)
				}
			}
		})
	}
}

func TestDramaProgramAudioDiagnosesBothOutOfBoundsEndpoints(t *testing.T) {
	r, svc := newProgramFixture(t, nil)
	in := absoluteProgramCue("start", model.DramaAudioSFX, 0, 24)
	in.SrcInSample = 12000
	out := absoluteProgramCue("end", model.DramaAudioBGM, 0, 24)
	out.SrcOutSample = new(int64(12001))
	saveProgramAudioFixture(t, r, in, out)
	view, err := svc.ResolveEpisodeManifestV2(t.Context(), 7, 1, 1)
	if err != nil || view.ManifestHash != "" || len(view.ProgramDiagnostics) != 2 {
		t.Fatalf("source bounds: %+v %v", view, err)
	}
	for i, field := range []string{"src_in_sample", "src_out_sample"} {
		if d := view.ProgramDiagnostics[i]; d.Field != field || d.Reason != "audio_range_out_of_bounds" {
			t.Fatalf("source bound %s: %+v", field, d)
		}
	}
}

type programHashlessFiles struct {
	programFiles
	hashCalls atomic.Int32
}

func (f *programHashlessFiles) Get(ctx context.Context, user, id uint64) (*model.Asset, error) {
	a, err := f.programFiles.Get(ctx, user, id)
	if id == 40 && err == nil && f.hashCalls.Load() == 0 {
		a.Sha256 = ""
	}
	return a, err
}

func (f *programHashlessFiles) EnsureSha256(ctx context.Context, user, id uint64) (string, *model.Asset, error) {
	if f.inTx {
		return "", nil, errors.New("hash backfill called inside transaction")
	}
	a, err := f.programFiles.Get(ctx, user, id)
	if err != nil {
		return "", nil, err
	}
	f.hashCalls.Add(1)
	return a.Sha256, a, nil
}

type programMissingAudioFiles struct{ programFiles }

func (f programMissingAudioFiles) Open(ctx context.Context, user, id uint64) (*provider.AssetFile, error) {
	if id == 40 {
		return nil, provider.ErrAssetNotFound
	}
	return f.programFiles.Open(ctx, user, id)
}

func requireColdProgramAudio(t *testing.T, svc *service.DramaService) {
	t.Helper()
	view, pending, err := svc.PrepareOrResolveEpisodeManifestV2(t.Context(), 7, 1, 1)
	if err != nil || view != nil || pending == nil || pending.Status != "preparing" {
		t.Fatalf("cold audio should prepare: %+v %+v %v", view, pending, err)
	}
}

func pollProgramAudioManifest(t *testing.T, svc *service.DramaService) *service.DramaManifestV2ResponseView {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		view, pending, err := svc.PrepareOrResolveEpisodeManifestV2(t.Context(), 7, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		if pending == nil && view != nil {
			return view
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("audio preparation did not reach a terminal response")
	return nil
}

func saveProgramAudioFixture(t *testing.T, r *programMemory, cues ...service.DramaProgramAudioInput) {
	t.Helper()
	raw, err := json.Marshal(service.DramaProgramInput{Version: 1, Audio: cues})
	if err != nil {
		t.Fatal(err)
	}
	r.episodes[0].ProgramJSON = raw
}
