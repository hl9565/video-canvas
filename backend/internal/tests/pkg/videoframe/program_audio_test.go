package videoframe_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/videoframe"
)

func TestProgramAudioProbeCountsDecodedFramesAndRejectsMissingAudio(t *testing.T) {
	e := requireProgramFFmpeg(t)
	path := filepath.Join(t.TempDir(), "sample.mkv")
	runProgramFFmpeg(t, "-f", "lavfi", "-i", "sine=r=48000", "-af", "atrim=end_sample=12345,asetpts=PTS+0.125/TB",
		"-c:a", "pcm_f32le", path)
	probe, err := e.ProbeProgramAudio(t.Context(), path)
	if err != nil || probe.DecodedAudioSamples != 12345 || probe.AStartUs != 125000 {
		t.Fatalf("probe: %+v, %v", probe, err)
	}
	video := filepath.Join(t.TempDir(), "silent.mp4")
	runProgramFFmpeg(t, "-f", "lavfi", "-i", "color=s=32x32", "-frames:v", "1", video)
	corrupt := filepath.Join(t.TempDir(), "corrupt.wav")
	if err := os.WriteFile(corrupt, []byte("not audio"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{video, corrupt} {
		if _, err := e.ProbeProgramAudio(t.Context(), bad); !errors.Is(err, videoframe.ErrDecode) {
			t.Fatalf("invalid source %s: %v", bad, err)
		}
	}
}

func requireProgramFFmpeg(t *testing.T) *videoframe.Extractor {
	t.Helper()
	e := videoframe.New(videoframe.Options{})
	if !e.Available() {
		t.Skip("FFmpeg and ffprobe required")
	}
	return e
}

func runProgramFFmpeg(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "ffmpeg", append([]string{"-nostdin", "-v", "error", "-y"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
}

func TestProgramAudioCueCrossesCutsAndPadsItsTail(t *testing.T) {
	e := requireProgramFFmpeg(t)
	source := programAudioSource(t, "0.25", 10001)
	cue := videoframe.ProgramAudioCueSpec{
		ID: "vo_3", Role: model.DramaAudioNarration, SourcePath: source, DecodedAudioSamples: 10001, EndF: 7,
	}
	in := videoframe.ComposeProgramInput{Clips: programBlackClips(2, 2, 3), Audio: []videoframe.ProgramAudioCueSpec{cue}}
	pcm := mixProgramPCM(t, e, in)
	if len(pcm) != 14000*8 {
		t.Fatalf("float bytes: %d", len(pcm))
	}
	for pos := range 14000 {
		want := float32(0)
		if pos < 10001 {
			want = 0.25
		}
		if left, right := programSample(pcm, pos, 0), programSample(pcm, pos, 1); left != want || right != want {
			t.Fatalf("frame %d: (%g,%g), want %g", pos, left, right, want)
		}
	}
	in.Audio[0].EndF = 4
	if _, err := e.MixProgramAudio(t.Context(), in, filepath.Join(t.TempDir(), "overflow.f32")); !errors.Is(err, videoframe.ErrInvalidManifest) || !strings.Contains(err.Error(), "差 2001 个采样") {
		t.Fatalf("expected exact missing sample count: %v", err)
	}
	in.Audio[0].SrcOutSample = new(int64(8000))
	pcm = mixProgramPCM(t, e, in)
	if programSample(pcm, 7999, 0) != 0.25 || programSample(pcm, 8000, 0) != 0 {
		t.Fatal("explicit source trim did not fit exactly at sample 8000")
	}
}

func programAudioSource(t *testing.T, expr string, samples int64) string {
	t.Helper()
	return programStereoAudioSource(t, expr+"|"+expr, samples)
}

func programStereoAudioSource(t *testing.T, expr string, samples int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.wav")
	runProgramFFmpeg(t, "-f", "lavfi", "-i", "aevalsrc="+expr+":s=48000",
		"-af", fmt.Sprintf("atrim=end_sample=%d", samples), "-c:a", "pcm_f32le", path)
	return path
}

func programBlackClips(lengths ...int32) []videoframe.ProgramClip {
	var clips []videoframe.ProgramClip
	for _, n := range lengths {
		clips = append(clips, videoframe.ProgramClip{ProgramVisualClip: videoframe.ProgramVisualClip{VisualKind: model.DramaVisualBlack, SourceOutF: n}})
	}
	return clips
}

func mixProgramPCM(t *testing.T, e *videoframe.Extractor, in videoframe.ComposeProgramInput) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "program-raw.f32")
	if _, err := e.MixProgramAudio(t.Context(), in, path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func programSample(pcm []byte, frame, channel int) float32 {
	return math.Float32frombits(binary.LittleEndian.Uint32(pcm[frame*8+channel*4:]))
}

func TestProgramAudioPlacesOffsetsLoopPhaseAndFadesAtSamples(t *testing.T) {
	e := requireProgramFFmpeg(t)
	path := programAudioSource(t, "n/16384", 4000)
	in := videoframe.ComposeProgramInput{Clips: programBlackClips(4), Audio: []videoframe.ProgramAudioCueSpec{
		{ID: "early", Role: model.DramaAudioSFX, SourcePath: path, StartF: 0, EndF: 2, DecodedAudioSamples: 4000,
			SrcInSample: 100, SrcOutSample: new(int64(257)), AvPtsOffsetMs: -2, FadeInF: 1, FadeOutF: 2},
		{ID: "late", Role: model.DramaAudioSFX, SourcePath: path, StartF: 2, EndF: 3, DecodedAudioSamples: 4000,
			SrcInSample: 101, SrcOutSample: new(int64(106)), AvPtsOffsetMs: 1},
		{ID: "bed", Role: model.DramaAudioRoomTone, SourcePath: path, StartF: 3, EndF: 4, DecodedAudioSamples: 4000,
			SrcInSample: 101, SrcOutSample: new(int64(108)), AvPtsOffsetMs: -1, Loop: true, FadeOutF: 1},
	}}
	pcm := mixProgramPCM(t, e, in)
	for sample, want := range map[int]float64{
		0: 0, 1: (197.0 / 16384) / 2000 * 3998 / 4000, 60: (256.0 / 16384) * 60 / 2000 * 3939 / 4000, 61: 0,
		4047: 0, 4048: 101.0 / 16384, 4052: 105.0 / 16384, 4053: 0,
		6000: (107.0 / 16384) * 1999 / 2000, 6001: (101.0 / 16384) * 1998 / 2000,
		6007: (107.0 / 16384) * 1992 / 2000, 7999: 0,
	} {
		checkProgramPCM(t, pcm, sample, want)
	}
}

func TestProgramAudioNativeOffsetGainFadeAndMuteAreLocal(t *testing.T) {
	e := requireProgramFFmpeg(t)
	path := programNativeMedia(t, 36000)
	p, err := e.ProbeNormalized24FPS(t.Context(), path)
	if err != nil || p.AStartUs != 25000 || p.VStartUs != 0 {
		t.Fatalf("native probe: %+v %v", p, err)
	}
	native := videoframe.ProgramClip{ProgramVisualClip: videoframe.ProgramVisualClip{
		VisualKind: model.DramaVisualVideo, SourcePath: path, SourceOutF: 24, NormalizedFrames24: p.NormalizedFrames24,
		FX: &model.DramaSegmentFX{NativeFadeInF: 1, NativeFadeOutF: 1},
	}, HasAudio: true, DecodedAudioSamples: p.DecodedAudioSamples, VStartUs: p.VStartUs, AStartUs: p.AStartUs,
		AudioGainDB: 20 * math.Log10(2), AvPtsOffsetMs: -5}
	independent := programAudioSource(t, "0.125", 48000)
	in := videoframe.ComposeProgramInput{Clips: []videoframe.ProgramClip{native}, Audio: []videoframe.ProgramAudioCueSpec{
		{ID: "independent", Role: model.DramaAudioSFX, SourcePath: independent, DecodedAudioSamples: 48000, EndF: 24},
	}}
	pcm := mixProgramPCM(t, e, in)
	for sample, want := range map[int]float64{
		959: 0.125, 960: 0.125 + 0.25*960/2000, 2000: 0.375, 36959: 0.375, 36960: 0.125, 47999: 0.125,
	} {
		checkProgramPCM(t, pcm, sample, want)
	}
	in.Clips[0].MuteOriginal = true
	muted := mixProgramPCM(t, e, in)
	for _, sample := range []int{0, 960, 2000, 36959, 47999} {
		checkProgramPCM(t, muted, sample, 0.125)
	}

	// 裁去一帧视频后，已在进行的音轨从对应源采样接上，淡入从新剪辑起点算。
	in.Clips[0].MuteOriginal = false
	in.Clips[0].SourceInF = 1
	in.Audio[0].EndF = 23
	pcm = mixProgramPCM(t, e, in)
	checkProgramPCM(t, pcm, 1000, 0.25)
	checkProgramPCM(t, pcm, 34960, 0.125)

	// 同期声音走制作总线，淡出相对于完整画面片段，而不是解码区间。
	in.Clips[0].SourceInF, in.Clips[0].DecodedAudioSamples = 0, 48000
	in.Clips[0].SourcePath = programNativeMedia(t, 48000)
	in.Audio[0].EndF = 24
	pcm = mixProgramPCM(t, e, in)
	checkProgramPCM(t, pcm, 47000, 0.125+0.25*999/2000)
	checkProgramPCM(t, pcm, 47999, 0.125)
}

func TestProgramAudioDucksSingleVoiceUnionAndPreservesReleaseInPreview(t *testing.T) {
	e := requireProgramFFmpeg(t)
	source := programAudioSource(t, "0.125", 48000)
	bed := programAudioSource(t, "0.25", 24000)
	native := videoframe.ProgramClip{ProgramVisualClip: videoframe.ProgramVisualClip{
		VisualKind: model.DramaVisualVideo, SourcePath: programNativeMedia(t, 48000), SourceOutF: 24, NormalizedFrames24: 30,
	}, HasAudio: true, DecodedAudioSamples: 48000, NativeIsDialogue: true, AStartUs: 25000, AvPtsOffsetMs: -5}
	clips := programBlackClips(24, 36)
	clips = append(clips[:1:1], native, clips[1])
	in := videoframe.ComposeProgramInput{Clips: clips, Audio: []videoframe.ProgramAudioCueSpec{
		{ID: "bed", Role: model.DramaAudioBGM, SourcePath: bed, DecodedAudioSamples: 24000, EndF: 84, Loop: true, DuckDB: new(-8.0)},
		{ID: "voice-a", Role: model.DramaAudioNarration, SourcePath: source, DecodedAudioSamples: 48000,
			StartF: 12, EndF: 40, SrcOutSample: new(int64(40000)), AvPtsOffsetMs: 1},
		{ID: "voice-b", Role: model.DramaAudioNarration, SourcePath: source, DecodedAudioSamples: 48000,
			StartF: 48, EndF: 58, SrcOutSample: new(int64(10000))},
		{ID: "hit", Role: model.DramaAudioSFX, SourcePath: source, DecodedAudioSamples: 48000,
			StartF: 54, EndF: 63, SrcOutSample: new(int64(12000))},
	}}
	raw := mixProgramPCM(t, e, in)
	checkProgramPCM(t, raw, 24047, 0.25) // 旁白首部补零不闪避。
	checkProgramDuckedBed(t, raw, 60000, 0.25)
	checkProgramDuckedBed(t, raw, 95999, 0.125)
	checkProgramDuckedBed(t, raw, 96000, 0.125)
	checkProgramPCM(t, raw, 106000+13439, 0.125+0.25*(1-(1-math.Pow(10, -0.4))/math.E))
	// 预览从释放段启动时，整集原始母带及其对应区间逐位一致。
	in.RangeF = &videoframe.FrameRange{InF: 55, OutF: 65}
	preview := mixProgramPCM(t, e, in)
	if !bytes.Equal(raw, preview) {
		t.Fatal("preview reset full-program ducking/envelopes")
	}

	// 单独保留同期非对白和 SFX；即使电平大于 1，混音也不提前削波。
	in.Audio = []videoframe.ProgramAudioCueSpec{in.Audio[0], in.Audio[3]}
	in.Audio[1].GainDB = 12
	in.Clips[1].NativeIsDialogue = false
	in.Clips[1].AudioGainDB = 12
	raw = mixProgramPCM(t, e, in)
	checkProgramPCM(t, raw, 60000, 0.25+0.125*math.Pow(10, 0.6))
	checkProgramPCM(t, raw, 112000, 0.25+0.125*math.Pow(10, 0.6))
	in.Clips[1].NativeIsDialogue, in.Clips[1].MuteOriginal = true, true
	raw = mixProgramPCM(t, e, in)
	checkProgramPCM(t, raw, 60000, 0.25)
	checkProgramPCM(t, raw, 112000, 0.25+0.125*math.Pow(10, 0.6))

	in.Audio[0].GainDB = 12
	raw = mixProgramPCM(t, e, in)
	if got := programSample(raw, 112000, 0); got <= 1 {
		t.Fatalf("float bus prematurely clamped: %g", got)
	}
}

func TestProgramAudioDucksOnlyOptedInBeds(t *testing.T) {
	e := requireProgramFFmpeg(t)
	in := videoframe.ComposeProgramInput{Clips: programBlackClips(24), Audio: []videoframe.ProgramAudioCueSpec{
		{ID: "bgm", Role: model.DramaAudioBGM, SourcePath: programStereoAudioSource(t, "0.25|0", 12000),
			DecodedAudioSamples: 12000, EndF: 24, Loop: true, DuckDB: new(-8.0)},
		{ID: "room", Role: model.DramaAudioRoomTone, SourcePath: programStereoAudioSource(t, "0|0.25", 12000),
			DecodedAudioSamples: 12000, EndF: 24, Loop: true, GainDB: -6},
		{ID: "voice", Role: model.DramaAudioNarration, SourcePath: programAudioSource(t, "0.125", 48000),
			DecodedAudioSamples: 48000, EndF: 24},
	}}
	pcm := mixProgramPCM(t, e, in)
	checkProgramDuckedBed(t, pcm, 24000, 0.125)
	right := float64(programSample(pcm, 24000, 1)) - 0.125
	db := 20 * math.Log10(right/0.25)
	if math.IsNaN(db) || math.Abs(db+6) > 0.1 {
		t.Fatalf("opted-out bed level %.4f dB, want its nominal gain of -6 dB", db)
	}

	in.Audio[0].DuckDB = new(-7.0)
	_, err := e.MixProgramAudio(t.Context(), in, filepath.Join(t.TempDir(), "invalid.f32"))
	if !errors.Is(err, videoframe.ErrInvalidManifest) {
		t.Fatalf("unsupported duck amount: %v", err)
	}
	in.Audio[0].DuckDB, in.Audio[2].DuckDB = nil, new(-8.0)
	_, err = e.MixProgramAudio(t.Context(), in, filepath.Join(t.TempDir(), "invalid.f32"))
	if !errors.Is(err, videoframe.ErrInvalidManifest) {
		t.Fatalf("duck setting on narration: %v", err)
	}
}

func programNativeMedia(t *testing.T, samples int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "native.mkv")
	runProgramFFmpeg(t, "-f", "lavfi", "-i", "color=s=32x32:r=24:d=1.25", "-f", "lavfi", "-i", "aevalsrc=0.125|0.125:s=48000",
		"-af", fmt.Sprintf("atrim=end_sample=%d,asetpts=PTS+0.025/TB", samples), "-c:a", "pcm_f32le", "-c:v", "libx264", path)
	return path
}

func checkProgramDuckedBed(t *testing.T, pcm []byte, at int, voice float64) {
	t.Helper()
	bed := float64(programSample(pcm, at, 0)) - voice
	db := 20 * math.Log10(bed/0.25)
	if math.IsNaN(db) || math.Abs(db+8) > 0.3 {
		t.Fatalf("sample %d: bed ducking %.4f dB", at, db)
	}
}

func checkProgramPCM(t *testing.T, pcm []byte, at int, want float64) {
	t.Helper()
	for ch := range 2 {
		if got := float64(programSample(pcm, at, ch)); math.IsNaN(got) || math.Abs(got-want) > 2e-6 {
			t.Fatalf("sample %d, channel %d: %g, want %g", at, ch, got, want)
		}
	}
}
