package videoframe_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/videoframe"
)

func TestProgramLoudnormAcceptsDecodedOutputsWithSharedAAC(t *testing.T) {
	requireProgramFFmpeg(t)
	e, trace, _ := tracedProgramFFmpeg(t)
	path := programAudioSource(t, "0.09*sin(2*PI*1000*t)", 96000)
	input := programLoudnormInput(t, 48, path)
	packaged := input.Outputs[0]
	packaged.Destination = filepath.Join(filepath.Dir(packaged.Destination), "packaged.mp4")
	packaged.WithOverlays, packaged.Variant = true, "packaged"
	input.Outputs = append(input.Outputs, packaged)
	report, err := e.ComposeProgramManifest(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalFrames != 48 || report.LoudnessStatus != "accepted" || report.LUFS == nil ||
		math.Abs(*report.LUFS+16) > 0.5 || report.TruePeak == nil || *report.TruePeak > -1.5 || len(report.Outputs) != 2 || len(report.AudioSHA256) != 64 {
		t.Fatalf("unexpected output report: %+v", report)
	}
	log := string(readProgramTestFile(t, trace))
	if strings.Count(log, "-c:a aac") != 1 || strings.Count(log, "-f concat") != 1 {
		t.Fatalf("same dimensions should share both encodes: %s", log)
	}
	for _, out := range report.Outputs {
		hash := programAACHash(t, out.Destination)
		if hash != report.AudioSHA256 || hash != out.AudioSHA256 {
			t.Fatalf("%s packet hash %q differs from report %+v", out.Variant, hash, report)
		}
	}
}

func TestProgramLoudnormSilenceHasNoMeasurements(t *testing.T) {
	e := requireProgramFFmpeg(t)
	in := programLoudnormInput(t, 3, "")
	in.RangeF = &videoframe.FrameRange{InF: 1, OutF: 2}
	report, err := e.ComposeProgramManifest(t.Context(), in)
	if err != nil || report.LoudnessStatus != "silence" || report.LUFS != nil || report.TruePeak != nil || report.TotalFrames != 1 {
		t.Fatalf("silence report: %+v %v", report, err)
	}
	raw, err := json.Marshal(model.DramaComposeExportReport{AudioSHA256: report.AudioSHA256, LUFS: report.LUFS, TruePeak: report.TruePeak, LoudnessStatus: report.LoudnessStatus})
	if err != nil || !strings.Contains(string(raw), `"lufs":null,"true_peak":null,"loudness_status":"silence"`) {
		t.Fatalf("silence wire values: %s %v", raw, err)
	}
	decoded := programDecodeOutput(t, in.Outputs[0].Destination)
	if len(decoded) < 2000*8 || len(decoded) > (2000+1023)*8 {
		t.Fatalf("one visual frame must carry 2000 samples (plus AAC padding): %d bytes", len(decoded))
	}
	for _, v := range decoded {
		if v != 0 {
			t.Fatal("digital silence decode contains nonzero bytes")
		}
	}
}

func programLoudnormInput(t *testing.T, frames int32, source string) videoframe.ComposeProgramInput {
	t.Helper()
	in := videoframe.ComposeProgramInput{Clips: programBlackClips(frames), Outputs: []videoframe.ComposeProgramOutputSpec{
		{Width: 1280, Height: 720, Variant: "clean", Destination: filepath.Join(t.TempDir(), "clean.mp4")},
	}}
	if source != "" {
		in.Audio = []videoframe.ProgramAudioCueSpec{{ID: "bed", Role: model.DramaAudioBGM, SourcePath: source, EndF: frames, DecodedAudioSamples: int64(frames) * 2000}}
	}
	return in
}

func programAACHash(t *testing.T, path string) string {
	t.Helper()
	data, err := exec.CommandContext(t.Context(), "ffmpeg", "-v", "error", "-i", path, "-map", "0:a:0", "-c:a", "copy", "-f", "hash", "-hash", "sha256", "pipe:1").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(string(data))), "sha256=")
}

func programDecodeOutput(t *testing.T, path string) []byte {
	t.Helper()
	file := filepath.Join(t.TempDir(), "decoded.f32")
	runProgramFFmpeg(t, "-i", path, "-map", "0:a:0", "-ar", "48000", "-ac", "2", "-f", "f32le", file)
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestProgramLoudnormRetriesAudioOnlyAndRejectsExhaustion(t *testing.T) {
	requireProgramFFmpeg(t)
	e, trace, _ := tracedProgramFFmpeg(t)
	// 孤立的 2ms 峰值使真峰限制与整集目标响度不可兼得。
	path := programAudioSource(t, `0.95*sin(2*PI*3000*t)*lt(mod(n\,24000)\,96)`, 192000)
	in := programLoudnormInput(t, 96, path)
	existing := []byte("previous export")
	if err := os.WriteFile(in.Outputs[0].Destination, existing, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := e.ComposeProgramManifest(t.Context(), in)
	if !errors.Is(err, videoframe.ErrLoudnessAcceptance) {
		t.Fatalf("expected typed acceptance error: %v", err)
	}
	log := readProgramTestFile(t, trace)
	if strings.Count(string(log), "measured_I=") != 3 || strings.Count(string(log), "-c:a aac") != 3 ||
		strings.Count(string(log), "-f concat") != 1 || !strings.Contains(string(log), "TP=-3.5:measured_I") ||
		!strings.Contains(string(log), "TP=-5:measured_I") {
		t.Fatalf("expected three audio attempts and one picture render: %s", log)
	}
	if got := readProgramTestFile(t, in.Outputs[0].Destination); !bytes.Equal(got, existing) {
		t.Fatal("failed acceptance published or replaced an output")
	}
}

func TestProgramLoudnormDoesNotCertifyAnUnmeasurableImpulse(t *testing.T) {
	e := requireProgramFFmpeg(t)
	path := programAudioSource(t, `0.5*eq(n\,96)`, 2000)
	in := programLoudnormInput(t, 1, path)
	report, err := e.ComposeProgramManifest(t.Context(), in)
	if !errors.Is(err, videoframe.ErrLoudnessAcceptance) || !strings.Contains(err.Error(), "非零节目没有有限的整集 LUFS") || report.LUFS != nil {
		t.Fatalf("nonzero impulse must not claim silence or -16 LUFS: %+v %v", report, err)
	}
}

func TestProgramLoudnormExcerptUsesExactSliceOfWholeProgramMaster(t *testing.T) {
	requireProgramFFmpeg(t)
	e, _, captured := tracedProgramFFmpeg(t)
	path := programAudioSource(t, `sin(2*PI*750*t)*(0.012+0.148*gte(n\,96000))`, 240000)
	in := programLoudnormInput(t, 120, path)
	report, err := e.ComposeProgramManifest(t.Context(), in)
	if err != nil || report.LoudnessStatus != "accepted" {
		t.Fatalf("whole program: %+v %v", report, err)
	}
	whole := readProgramTestFile(t, captured)
	if len(whole) != 240000*8 {
		t.Fatalf("whole mastered PCM size: %d", len(whole))
	}

	in.RangeF = &videoframe.FrameRange{InF: 12, OutF: 36}
	report, err = e.ComposeProgramManifest(t.Context(), in)
	if err != nil || report.LoudnessStatus != "measured" || report.LUFS == nil || *report.LUFS >= -27 ||
		report.TruePeak == nil || *report.TruePeak > -1.5 || report.TotalFrames != 24 {
		t.Fatalf("quiet excerpt independently normalized or rejected: %+v %v", report, err)
	}
	if preview := readProgramTestFile(t, captured); !bytes.Equal(preview, whole[24000*8:72000*8]) {
		t.Fatal("preview encoder input must match the corresponding whole-program master samples bit for bit")
	}
}

func TestProgramLoudnormExcerptUsesFullProgramRetry(t *testing.T) {
	requireProgramFFmpeg(t)
	e, trace, captured := tracedProgramFFmpeg(t)
	in := programLoudnormInput(t, 96, programRetryAudioSource(t))
	report, err := e.ComposeProgramManifest(t.Context(), in)
	if err != nil || report.LoudnessStatus != "accepted" {
		t.Fatalf("full program with peaks: %+v %v", report, err)
	}
	whole := readProgramTestFile(t, captured)
	fullPCM := programDecodeOutput(t, in.Outputs[0].Destination)
	fullLog := string(readProgramTestFile(t, trace))
	if !strings.Contains(fullLog, "TP=-3.5:measured_I") {
		t.Fatalf("fixture did not trigger the true peak retry: %+v", report)
	}

	// 预览包含低电平信号，三个触发重试的瞬态峰都在区间之外。
	in.RangeF = &videoframe.FrameRange{InF: 13, OutF: 23}
	report, err = e.ComposeProgramManifest(t.Context(), in)
	if err != nil || report.LoudnessStatus != "measured" || report.LUFS == nil || *report.LUFS >= -27 {
		t.Fatalf("quiet preview: %+v %v", report, err)
	}
	previewLog := string(readProgramTestFile(t, trace))[len(fullLog):]
	if !strings.Contains(previewLog, "TP=-3.5:measured_I") {
		t.Fatal("quiet preview skipped the full-program retry")
	}
	if preview := readProgramTestFile(t, captured); !bytes.Equal(preview, whole[26000*8:46000*8]) {
		t.Fatal("quiet preview was encoded from a different PCM master")
	}
	if got := programAACHash(t, in.Outputs[0].Destination); got != report.AudioSHA256 {
		t.Fatalf("preview AAC hash is %s, report has %s", got, report.AudioSHA256)
	}

	// 两端均位于连续静音内，独立 AAC 编码也可逐样本比较，无边缘编码误差。
	in.RangeF = &videoframe.FrameRange{InF: 4, OutF: 8}
	if _, err := e.ComposeProgramManifest(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	previewPCM := programDecodeOutput(t, in.Outputs[0].Destination)
	if len(previewPCM) < 8000*8 || !bytes.Equal(previewPCM[:8000*8], fullPCM[8000*8:16000*8]) {
		t.Fatal("decoded preview does not match the corresponding quiet slice of the full export")
	}
}

func programRetryAudioSource(t *testing.T) string {
	t.Helper()
	const samples = 192000
	pcm := make([]byte, samples*8)
	// 用固定的 xorshift 序列生成三个短瞬态，避免依赖 FFmpeg 的随机源。
	var seed uint32 = 4
	peaks := make(map[int]float64)
	for _, start := range []int{62001, 119722, 164173} {
		for j := range 100 {
			seed ^= seed << 13
			seed ^= seed >> 17
			seed ^= seed << 5
			peaks[start+j] = 0.85 * (float64(seed)/4294967296*2 - 1)
		}
	}
	for n := 24000; n < samples; n++ {
		amp := 0.015
		if n >= 48000 {
			amp = 0.16
		}
		v := amp*math.Sin(2*math.Pi*1000*float64(n)/48000) + peaks[n]
		for ch := range 2 {
			binary.LittleEndian.PutUint32(pcm[n*8+ch*4:], math.Float32bits(float32(v)))
		}
	}
	dir := t.TempDir()
	raw, path := filepath.Join(dir, "peaks.f32"), filepath.Join(dir, "peaks.wav")
	if err := os.WriteFile(raw, pcm, 0600); err != nil {
		t.Fatal(err)
	}
	runProgramFFmpeg(t, "-f", "f32le", "-ar", "48000", "-ac", "2", "-i", raw, "-c:a", "pcm_f32le", path)
	return path
}

// tracedProgramFFmpeg 记录实际 FFmpeg 参数，并保留传给 AAC 编码器的浮点音频作对照。
func tracedProgramFFmpeg(t *testing.T) (*videoframe.Extractor, string, string) {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	sh, log, raw := filepath.Join(dir, "ffmpeg"), filepath.Join(dir, "trace"), filepath.Join(dir, "aac-input.f32")
	t.Setenv("VC_TEST_FFMPEG_BIN", ffmpeg)
	t.Setenv("VC_TEST_FFMPEG_TRACE", log)
	t.Setenv("VC_TEST_FFMPEG_AAC_PCM", raw)
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$VC_TEST_FFMPEG_TRACE" || exit 1
case " $* " in
  *" -c:a aac "*)
    prev=
    for arg; do
      if [ "$prev" = -i ]; then
        cp "$arg" "$VC_TEST_FFMPEG_AAC_PCM" || exit 1
        break
      fi
      prev="$arg"
    done
    ;;
esac
exec "$VC_TEST_FFMPEG_BIN" "$@"
`
	if err := os.WriteFile(sh, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return videoframe.New(videoframe.Options{FFmpegPath: sh}), log, raw
}

func readProgramTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
