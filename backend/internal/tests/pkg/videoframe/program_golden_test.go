package videoframe_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/tests/testutil"
)

func TestProgramGolden(t *testing.T) {
	requireProgramFFmpeg(t)
	e := testutil.GoldenExtractor()
	f := testutil.NewGoldenProgram(t, 1, 9, 10)
	t.Run("demo_trim_19f", func(t *testing.T) { checkGoldenTrim(t, e, f.Media[140]) })
	in := f.RenderInput(t, e)
	t.Run("vo_3", func(t *testing.T) { checkGoldenVoice(t, e, in) })
	t.Run("contact_s2_pts_s7_mute", func(t *testing.T) {
		checkGoldenContact(t, e, in)
		checkGoldenDialogue(t, e, in)
		checkGoldenMuteS7(t, e, in)
	})
	t.Run("exports", func(t *testing.T) { checkGoldenExports(t, e, in) })
}

func checkGoldenTrim(t *testing.T, e *videoframe.Extractor, source testutil.GoldenMedia) {
	t.Helper()
	clip := testutil.GoldenVideoClip(t, e, source.Path, 24, 43)
	clip.MuteOriginal = true
	original := programLuma(t, source.Path)
	if len(original) != 144 || original[42] == original[43] || !clip.HasAudio {
		t.Fatal("trim source must have a distinguishing f43, 144 frames and native audio")
	}
	in := videoframe.ComposeProgramVisualInput{Width: 1280, Height: 720, Clips: []videoframe.ProgramVisualClip{clip.ProgramVisualClip}}
	for _, tc := range []struct {
		name   string
		out    *int32
		frames int
	}{{"19f", new(int32(43)), 19}, {"1f", new(int32(25)), 1}, {"EOF", nil, 120}} {
		t.Run(tc.name, func(t *testing.T) {
			in.Clips[0].SourceOutF = 0 // EOF in the renderer is the zero value.
			if tc.out != nil {
				in.Clips[0].SourceOutF = *tc.out
			}
			in.Destination = filepath.Join(t.TempDir(), "trim.mp4")
			composeProgramVisualTest(t, e, in, tc.frames)
			got := programLuma(t, in.Destination)
			if !bytes.Equal(got, original[24:24+tc.frames]) {
				t.Fatalf("trim pixels differ from decoded source frames [24,%d): %v", 24+tc.frames, got)
			}
			if tc.frames == 19 {
				goldenVideoDuration(t, in.Destination, 0.791667)
			}
		})
	}
	for _, out := range []int32{23, 24, 145} {
		in.Clips[0].SourceOutF = out
		if err := e.ComposeProgramVisual(t.Context(), in); !errors.Is(err, videoframe.ErrInvalidManifest) {
			t.Fatalf("invalid trim [24,%d): %v", out, err)
		}
	}
	clip.SourceInF, clip.SourceOutF = 0, 0
	pcm := mixProgramPCM(t, e, videoframe.ComposeProgramInput{Clips: []videoframe.ProgramClip{clip}})
	requireGoldenSilence(t, pcm, 144*2000)
	if got := testutil.FileSHA256(t, source.Path); got != source.Asset.Sha256 {
		t.Fatalf("render changed source SHA-256: %s", got)
	}
}

func goldenVideoDuration(t *testing.T, path string, expected float64) {
	t.Helper()
	out := runProgramVisualTestCommand(t, "ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=duration", "-of", "csv=p=0", path)
	got, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || math.Abs(got-expected) > 0.000001 {
		t.Fatalf("video duration %q, want %.6f s (%v)", out, expected, err)
	}
}

func requireGoldenSilence(t *testing.T, pcm []byte, samples int) {
	t.Helper()
	if len(pcm) != samples*8 || bytes.Count(pcm, []byte{0}) != len(pcm) {
		t.Fatalf("expected %d stereo frames of digital zero, got %d bytes (%d nonzero)",
			samples, len(pcm), len(pcm)-bytes.Count(pcm, []byte{0}))
	}
}

func goldenCueSpecs(in videoframe.ComposeProgramInput, ids ...string) []videoframe.ProgramAudioCueSpec {
	var result []videoframe.ProgramAudioCueSpec
	for _, cue := range in.Audio {
		if slices.Contains(ids, cue.ID) {
			result = append(result, cue)
		}
	}
	return result
}

func checkGoldenVoice(t *testing.T, e *videoframe.Extractor, in videoframe.ComposeProgramInput) {
	t.Helper()
	voice := in
	voice.Audio = goldenCueSpecs(in, "vo_3")
	pcm := mixProgramPCM(t, e, voice)
	part := pcm[372*2000*8 : 465*2000*8]
	source := programDecodeOutput(t, voice.Audio[0].SourcePath)
	// 到淡出之前，每个采样都应等于单次连续解码；此窗口跨过两处真实的画面切点。
	if !bytes.Equal(part[:91*2000*8], source[:91*2000*8]) {
		t.Fatal("vo_3 PCM diverges from its continuous source across cuts 396 and 443")
	}
	requireGoldenSilence(t, pcm[465*2000*8:490*2000*8], 25*2000)
	const fadeSample = 92*2000 + 17
	wantFade := float64(programSample(source, fadeSample, 0)) * (93*2000 - fadeSample - 1) / 4000
	checkProgramPCM(t, part, fadeSample, wantFade)
	checkProgramPCM(t, part, 93*2000-1, 0)

	voice.Clips = slices.Clone(in.Clips)
	voice.Clips[testutil.GoldenS5].SourceOutF += 2
	voice.Clips[testutil.GoldenS6A].SourceOutF -= 2
	shifted := mixProgramPCM(t, e, voice)
	if len(shifted) != testutil.GoldenSamples*8 || !bytes.Equal(part, shifted[372*2000*8:465*2000*8]) {
		t.Fatal("moving the S5→S6a cut by two frames changed vo_3 PCM or program duration")
	}
	checkGoldenVoiceDucking(t, e, in, pcm)
}

func checkGoldenVoiceDucking(t *testing.T, e *videoframe.Extractor, in videoframe.ComposeProgramInput, voice []byte) {
	t.Helper()
	bed := in
	bed.Audio = goldenCueSpecs(in, "bgm", "room_tone")
	base := mixProgramPCM(t, e, bed)
	vb := bed
	vb.Audio = append(slices.Clone(bed.Audio), goldenCueSpecs(in, "vo_3")...)
	ducked := mixProgramPCM(t, e, vb)
	checkGoldenLevel(t, ducked, voice, base, 420*2000, 430*2000, -8)

	// 加入 SFX 后的差值应和单独混入 SFX 时相同，且参考音效确实有信号。
	hit := in
	hit.Audio = goldenCueSpecs(in, "vo_3_sfx")
	hitPCM := mixProgramPCM(t, e, hit)
	vb.Audio = append(vb.Audio, hit.Audio...)
	all := mixProgramPCM(t, e, vb)
	checkGoldenLevel(t, all, ducked, hitPCM, 420*2000, 430*2000, 0)
}

func checkGoldenLevel(t *testing.T, mixed, subtract, reference []byte, start, end int, wantDB float64) {
	t.Helper()
	var gotPower, refPower float64
	for sample := start; sample < end; sample++ {
		for ch := range 2 {
			got := float64(programSample(mixed, sample, ch)) - float64(programSample(subtract, sample, ch))
			ref := float64(programSample(reference, sample, ch))
			gotPower, refPower = gotPower+got*got, refPower+ref*ref
		}
	}
	db := 10 * math.Log10(gotPower/refPower)
	if refPower == 0 || math.IsNaN(db) || math.Abs(db-wantDB) > 0.3 {
		t.Fatalf("[%d,%d): %.4f dB, want %.1f ± 0.3 dB", start, end, db, wantDB)
	}
}

func checkGoldenContact(t *testing.T, e *videoframe.Extractor, in videoframe.ComposeProgramInput) {
	t.Helper()
	in.Audio = goldenCueSpecs(in, "contact")
	pcm := mixProgramPCM(t, e, in)
	source := programDecodeOutput(t, in.Audio[0].SourcePath)
	const at = 478000
	requireGoldenSilence(t, pcm[456000*8:at*8], at-456000)
	if programSample(source, 0, 0) == 0 || !bytes.Equal(source, pcm[at*8:(at+6000)*8]) {
		t.Fatal("S3 contact does not start at sample 478000")
	}
}

func checkGoldenDialogue(t *testing.T, e *videoframe.Extractor, in videoframe.ComposeProgramInput) {
	t.Helper()
	s2 := in.Clips[1]
	if s2.AStartUs-s2.VStartUs != 125000 {
		t.Fatalf("S2 source PTS delta is %d µs, expected 125000", s2.AStartUs-s2.VStartUs)
	}
	source := programDecodeOutput(t, s2.SourcePath)
	if len(source) < 12100*8 || bytes.Count(source[12000*8:12100*8], []byte{0}) == 800 {
		t.Fatal("S2 source has no dialogue at the onset")
	}
	native := in
	native.Audio = nil
	native.Clips = slices.Clone(in.Clips)
	for _, ms := range []int32{-50, 0, 50} {
		native.Clips[1].AvPtsOffsetMs = ms
		pcm := mixProgramPCM(t, e, native)
		// S2 starts at 228000; the source trims 12000 samples, while PCM starts
		// at 12000 plus the probed timestamp delta (6000) and manual offset.
		onset := 234000 + int(ms)*48
		requireGoldenSilence(t, pcm[228000*8:onset*8], onset-228000)
		for i := range 64 {
			expected := float64(programSample(source, 12000+i, 0)) * math.Pow(10, 2.0/20)
			checkProgramPCM(t, pcm, onset+i, expected)
		}
		requireGoldenSilence(t, pcm[456000*8:460000*8], 4000)
	}
	dialogue := mixProgramPCM(t, e, videoframe.ComposeProgramInput{Clips: in.Clips})
	in.Audio = goldenCueSpecs(in, "bgm", "room_tone")
	mixed := mixProgramPCM(t, e, in)
	in.Clips = slices.Clone(in.Clips)
	in.Clips[1].MuteOriginal = true
	base := mixProgramPCM(t, e, in)
	checkGoldenLevel(t, mixed, dialogue, base, 150*2000, 160*2000, -8)
}

func checkGoldenMuteS7(t *testing.T, e *videoframe.Extractor, in videoframe.ComposeProgramInput) {
	t.Helper()
	native := in
	native.Audio = nil
	pcm := mixProgramPCM(t, e, native)
	requireGoldenSilence(t, pcm[490*2000*8:571*2000*8], 81*2000)
	s7 := in.Clips[testutil.GoldenS7]
	decoded := programDecodeOutput(t, s7.SourcePath)
	if !s7.HasAudio || len(decoded) < 14000*8 || !slices.ContainsFunc(decoded[12000*8:14000*8], func(b byte) bool { return b != 0 }) {
		t.Fatal("muted S7 fixture is missing native signal")
	}
	in.Audio = goldenCueSpecs(in, "vo_4", "bgm", "room_tone")
	full := mixProgramPCM(t, e, in)
	withoutNative := in
	withoutNative.Clips = slices.Clone(in.Clips)
	withoutNative.Clips[testutil.GoldenS7].HasAudio = false
	silentSource := mixProgramPCM(t, e, withoutNative)
	const start, end = 490 * 2000 * 8, 571 * 2000 * 8
	if !bytes.Equal(full[start:end], silentSource[start:end]) {
		t.Fatal("S7 leaked native audio into VO4 and the bed")
	}
	voice := in
	voice.Audio = goldenCueSpecs(in, "vo_4")
	vo4 := mixProgramPCM(t, e, voice)
	const at = 530*2000 + 17
	if programSample(vo4, at, 0) == 0 || programSample(full, at, 0) == programSample(vo4, at, 0) {
		t.Fatal("expected both VO4 and active bed during muted S7")
	}
}

func checkGoldenExports(t *testing.T, e *videoframe.Extractor, in videoframe.ComposeProgramInput) {
	t.Helper()
	dir := t.TempDir()
	for _, h := range []int{720, 1080} {
		for _, variant := range []string{"clean", "packaged"} {
			in.Outputs = append(in.Outputs, videoframe.ComposeProgramOutputSpec{
				Variant: variant, Width: h * 16 / 9, Height: h, Destination: filepath.Join(dir, fmt.Sprintf("%s-%d.mp4", variant, h)),
			})
		}
	}
	report, err := e.ComposeProgramManifest(t.Context(), in)
	if err != nil || report.TotalFrames != 708 || report.LoudnessStatus != "accepted" || len(report.Outputs) != 4 || len(report.AudioSHA256) != 64 {
		t.Fatalf("708-frame compose: %+v, %v", report, err)
	}
	s7 := programLuma(t, in.Clips[testutil.GoldenS7].SourcePath)
	for i, spec := range in.Outputs {
		t.Run(fmt.Sprintf("%s_%dp", spec.Variant, spec.Height), func(t *testing.T) {
			assertGoldenExport(t, spec, report.AudioSHA256)
			luma := programLuma(t, spec.Destination)
			if luma[570] != s7[80] || luma[570] == s7[81] {
				t.Fatalf("S7 last visible frame has Y=%d; f80=%d, f81=%d", luma[570], s7[80], s7[81])
			}
			if spec.Variant == "packaged" && !slices.Equal(programFrameHashes(t, in.Outputs[i-1].Destination), programFrameHashes(t, spec.Destination)) {
				t.Fatal("clean and packaged decoded pictures differ")
			}
		})
	}
}

func assertGoldenExport(t *testing.T, spec videoframe.ComposeProgramOutputSpec, masterHash string) {
	t.Helper()
	got := runProgramVisualTestCommand(t, "ffprobe", "-v", "error", "-select_streams", "v:0", "-count_frames",
		"-show_entries", "stream=width,height,sample_aspect_ratio,r_frame_rate,avg_frame_rate,nb_read_frames", "-of", "csv=p=0", spec.Destination)
	want := fmt.Sprintf("%d,%d,1:1,24/1,24/1,708", spec.Width, spec.Height)
	if strings.TrimSpace(string(got)) != want {
		t.Fatalf("video streams: got %s, want %s", got, want)
	}
	goldenVideoDuration(t, spec.Destination, 29.5)
	audio := runProgramVisualTestCommand(t, "ffprobe", "-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=codec_name,sample_rate,channels,duration", "-of", "csv=p=0", spec.Destination)
	if strings.TrimSpace(string(audio)) != "aac,48000,2,29.500000" {
		t.Fatalf("unexpected AAC stream: %s", audio)
	}
	samples := len(programDecodeOutput(t, spec.Destination)) / 8
	if samples < 1416000 || samples > 1416000+1023 {
		t.Fatalf("decoded %d samples/channel, expected 1416000 plus AAC padding", samples)
	}
	m4a := filepath.Join(t.TempDir(), "track.m4a")
	runProgramFFmpeg(t, "-i", spec.Destination, "-map", "0:a:0", "-c:a", "copy", m4a)
	if got := programAACHash(t, m4a); got != masterHash {
		t.Fatalf("extracted AAC payload hash %s, expected %s", got, masterHash)
	}
	checkGoldenDecodedLoudness(t, spec.Destination)
}

func checkGoldenDecodedLoudness(t *testing.T, path string) {
	t.Helper()
	log, err := exec.CommandContext(t.Context(), "ffmpeg", "-nostdin", "-v", "info", "-i", path, "-map", "0:a:0",
		"-af", "loudnorm=I=-16:TP=-1.5:print_format=json", "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("measure decoded loudness: %v: %s", err, log)
	}
	var stats struct {
		LUFS string `json:"input_i"`
		Peak string `json:"input_tp"`
	}
	b, end := bytes.LastIndexByte(log, '{'), bytes.LastIndexByte(log, '}')
	if b < 0 || end <= b || json.Unmarshal(log[b:end+1], &stats) != nil {
		t.Fatalf("no loudness measurements: %s", log)
	}
	i, ierr := strconv.ParseFloat(stats.LUFS, 64)
	tp, perr := strconv.ParseFloat(stats.Peak, 64)
	if ierr != nil || perr != nil || math.IsNaN(i) || math.IsNaN(tp) || math.IsInf(tp, 0) || math.Abs(i+16) > 0.5 || tp > -1.5 {
		t.Fatalf("decoded loudness: %s LUFS, %s dBTP", stats.LUFS, stats.Peak)
	}
}
