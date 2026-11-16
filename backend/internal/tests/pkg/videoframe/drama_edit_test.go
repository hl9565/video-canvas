package videoframe_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"video-canvas/internal/pkg/videoframe"
)

func mustDecodeS16Stereo(t *testing.T, path string) []int16 {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "ffmpeg",
		"-v", "error", "-i", path, "-vn", "-ac", "2", "-ar", "48000", "-f", "s16le", "-").Output()
	if err != nil {
		t.Fatalf("decode stereo pcm %s: %v", path, err)
	}
	n := len(out) / 2
	samples := make([]int16, n)
	for i := range n {
		samples[i] = int16(binary.LittleEndian.Uint16(out[i*2 : i*2+2]))
	}
	return samples
}

func rmsSampleWindow(samples []int16, startFrame48, endFrame48 int) float64 {
	startIdx := startFrame48 * 2
	endIdx := endFrame48 * 2
	if startIdx < 0 {
		startIdx = 0
	}
	if endIdx > len(samples) {
		endIdx = len(samples)
	}
	if endIdx <= startIdx {
		return 0
	}
	var sumSq float64
	for i := startIdx; i < endIdx; i++ {
		v := float64(samples[i]) / 32768.0
		sumSq += v * v
	}
	return math.Sqrt(sumSq / float64(endIdx-startIdx))
}

func probeFrameCountAndFPS(t *testing.T, path string) (int, string, float64) {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "ffprobe",
		"-v", "error", "-count_frames",
		"-show_entries", "stream=codec_type,nb_read_frames,r_frame_rate,duration",
		"-show_entries", "format=duration",
		"-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var res struct {
		Streams []struct {
			CodecType    string `json:"codec_type"`
			NBReadFrames string `json:"nb_read_frames"`
			RFrameRate   string `json:"r_frame_rate"`
			Duration     string `json:"duration"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("parse ffprobe %s: %v", path, err)
	}
	for _, st := range res.Streams {
		if st.CodecType == "video" {
			var frames int
			if _, err := fmt.Sscanf(st.NBReadFrames, "%d", &frames); err != nil {
				t.Fatalf("parse nb_read_frames %q: %v", st.NBReadFrames, err)
			}
			var dur float64
			if _, err := fmt.Sscanf(st.Duration, "%f", &dur); err != nil {
				_, _ = fmt.Sscanf(res.Format.Duration, "%f", &dur)
			}
			return frames, st.RFrameRate, dur
		}
	}
	t.Fatalf("no video stream in %s", path)
	return 0, "", 0
}

func makePulseClip(t *testing.T, path string, totalSec, toneStartSec, vOffset, aOffset float64) {
	t.Helper()
	if !videoframe.New(videoframe.Options{}).Available() {
		t.Skip("本机没有 ffmpeg / ffprobe，跳过测试")
	}
	args := []string{"-v", "error"}
	if vOffset > 0 {
		args = append(args, "-itsoffset", fmt.Sprintf("%.3f", vOffset))
	}
	args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("color=c=navy:s=160x120:r=24:d=%.3f", totalSec))
	if aOffset > 0 {
		args = append(args, "-itsoffset", fmt.Sprintf("%.3f", aOffset))
	}
	audioExpr := fmt.Sprintf("sine=frequency=440:sample_rate=48000:duration=%.3f,volume='if(gte(t,%.3f),4,0)':eval=frame", totalSec, toneStartSec)
	args = append(args, "-f", "lavfi", "-i", audioExpr,
		"-map", "0:v:0", "-map", "1:a:0",
		"-c:v", "libx264", "-bf", "0", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-y", path)
	if out, err := exec.CommandContext(context.Background(), "ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("makePulseClip: %v\n%s", err, out)
	}
}

func TestProbeNormalized24FPS_RatesAndAudioSamples(t *testing.T) {
	if !videoframe.New(videoframe.Options{}).Available() {
		t.Skip("本机没有 ffmpeg / ffprobe，跳过测试")
	}
	dir := t.TempDir()
	e := videoframe.New(videoframe.Options{Timeout: 30 * time.Second})

	clip24 := filepath.Join(dir, "c24.mp4")
	makeClip(t, clip24, "red", "160x120", "24", "2", "", true)
	p24, err := e.ProbeNormalized24FPS(context.Background(), clip24)
	if err != nil {
		t.Fatalf("ProbeNormalized24FPS c24: %v", err)
	}
	if p24.FpsNum != 24 || p24.FpsDen != 1 || p24.NormalizedFrames24 != 48 {
		t.Fatalf("c24 unexpected probe: %+v", p24)
	}
	if !p24.HasAudio || p24.AudioSampleRate != 48000 || p24.DecodedAudioSamples < 90000 {
		t.Fatalf("c24 audio probe unexpected: %+v", p24)
	}

	clip30Silent := filepath.Join(dir, "c30_silent.mp4")
	makeClip(t, clip30Silent, "blue", "160x120", "30", "1", "", false)
	p30, err := e.ProbeNormalized24FPS(context.Background(), clip30Silent)
	if err != nil {
		t.Fatalf("ProbeNormalized24FPS c30: %v", err)
	}
	if p30.FpsNum != 30 || p30.FpsDen != 1 || p30.NormalizedFrames24 != 24 {
		t.Fatalf("c30 unexpected probe: %+v", p30)
	}
	if p30.HasAudio || p30.AudioSampleRate != 0 || p30.DecodedAudioSamples != 0 {
		t.Fatalf("c30 silent unexpected: %+v", p30)
	}
}

func TestComputeNativeAudioWindow_MathAndOffsets(t *testing.T) {
	// Mute always produces digital silence.
	muted := videoframe.ComputeNativeAudioWindow(videoframe.NativeAudioInput{
		HasAudio:            true,
		DecodedAudioSamples: 96000,
		SourceInF:           0,
		SourceOutF:          24,
		MuteOriginal:        true,
	})
	if !muted.UseSilence || muted.TargetSamples != 48000 || muted.LeadPad != 48000 {
		t.Fatalf("muted unexpected: %+v", muted)
	}

	// Positive offset: aStart - vStart = +100ms (4800 samples) and +50ms user offset (2400 samples) -> D = +7200.
	// Clip frames [0, 24) => W = [0, 48000): LeadPad=7200, k0=0, k1=40800, TailPad=0.
	pos := videoframe.ComputeNativeAudioWindow(videoframe.NativeAudioInput{
		HasAudio:            true,
		VStartUs:            0,
		AStartUs:            100_000,
		DecodedAudioSamples: 96000,
		AvPtsOffsetMs:       50,
		SourceInF:           0,
		SourceOutF:          24,
	})
	if pos.UseSilence || pos.OffsetSamples != 7200 || pos.K0 != 0 || pos.K1 != 40800 || pos.LeadPad != 7200 || pos.TailPad != 0 || pos.TargetSamples != 48000 {
		t.Fatalf("pos offset unexpected: %+v", pos)
	}

	// Negative offset: vStart = 100ms, aStart = 0ms -> D = -4800.
	// Short decoded stream (48000 samples): W=[0, 48000) => k0=4800, k1=48000 (43200 copied), LeadPad=0, TailPad=4800.
	neg := videoframe.ComputeNativeAudioWindow(videoframe.NativeAudioInput{
		HasAudio:            true,
		VStartUs:            100_000,
		AStartUs:            0,
		DecodedAudioSamples: 48000,
		SourceInF:           0,
		SourceOutF:          24,
	})
	if neg.UseSilence || neg.OffsetSamples != -4800 || neg.K0 != 4800 || neg.K1 != 48000 || neg.LeadPad != 0 || neg.TailPad != 4800 || neg.TargetSamples != 48000 {
		t.Fatalf("neg offset unexpected: %+v", neg)
	}
}

func TestComposeManifest_Trim19f_1f_EOF_And_Bounds(t *testing.T) {
	if !videoframe.New(videoframe.Options{}).Available() {
		t.Skip("本机没有 ffmpeg / ffprobe，跳过测试")
	}
	dir := t.TempDir()
	e := videoframe.New(videoframe.Options{Timeout: 60 * time.Second})

	src := filepath.Join(dir, "src5s.mp4")
	makeClip(t, src, "green", "320x180", "24", "5", "", true)
	p, err := e.ProbeNormalized24FPS(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if p.NormalizedFrames24 != 120 {
		t.Fatalf("expected 120 frames, got %d", p.NormalizedFrames24)
	}

	// [24, 43) => exact 19 frames, 0.791667s (matches demo_trim_19f contract).
	out19 := filepath.Join(dir, "trim19.mp4")
	end43 := int32(43)
	err = e.ComposeManifest(context.Background(), videoframe.ComposeManifestInput{
		Clips: []videoframe.ManifestClip{{
			SegmentID:  1,
			Seq:        1,
			SourcePath: src,
			SourceInF:  24,
			SourceOutF: &end43,
		}},
		Outputs: []videoframe.ManifestRenderSpec{{Destination: out19}},
	})
	if err != nil {
		t.Fatalf("ComposeManifest 19f: %v", err)
	}
	frames, rate, dur := probeFrameCountAndFPS(t, out19)
	if frames != 19 || rate != "24/1" || math.Abs(dur-19.0/24.0) > 0.01 {
		t.Fatalf("19f got frames=%d rate=%s dur=%.6f", frames, rate, dur)
	}

	// 1-frame minimum trim [10, 11).
	out1 := filepath.Join(dir, "trim1.mp4")
	end11 := int32(11)
	err = e.ComposeManifest(context.Background(), videoframe.ComposeManifestInput{
		Clips: []videoframe.ManifestClip{{
			SegmentID:  1,
			Seq:        1,
			SourcePath: src,
			SourceInF:  10,
			SourceOutF: &end11,
		}},
		Outputs: []videoframe.ManifestRenderSpec{{Destination: out1}},
	})
	if err != nil {
		t.Fatalf("ComposeManifest 1f: %v", err)
	}
	frames1, rate1, _ := probeFrameCountAndFPS(t, out1)
	if frames1 != 1 || rate1 != "24/1" {
		t.Fatalf("1f got frames=%d rate=%s", frames1, rate1)
	}

	// source_out_f: nil -> trims cleanly to normalized EOF (120 - 100 = 20 frames).
	outEOF := filepath.Join(dir, "eof.mp4")
	err = e.ComposeManifest(context.Background(), videoframe.ComposeManifestInput{
		Clips: []videoframe.ManifestClip{{
			SegmentID:  1,
			Seq:        1,
			SourcePath: src,
			SourceInF:  100,
			SourceOutF: nil,
		}},
		Outputs: []videoframe.ManifestRenderSpec{{Destination: outEOF}},
	})
	if err != nil {
		t.Fatalf("ComposeManifest EOF: %v", err)
	}
	framesEOF, _, _ := probeFrameCountAndFPS(t, outEOF)
	if framesEOF != 20 {
		t.Fatalf("EOF got frames=%d want 20", framesEOF)
	}

	// Out-of-bounds [24, 121) -> ErrInvalidManifest and no orphan destination file.
	badOut := filepath.Join(dir, "bad.mp4")
	over := int32(121)
	err = e.ComposeManifest(context.Background(), videoframe.ComposeManifestInput{
		Clips: []videoframe.ManifestClip{{
			SegmentID:  1,
			Seq:        1,
			SourcePath: src,
			SourceInF:  24,
			SourceOutF: &over,
		}},
		Outputs: []videoframe.ManifestRenderSpec{{Destination: badOut}},
	})
	if !errors.Is(err, videoframe.ErrInvalidManifest) {
		t.Fatalf("expected ErrInvalidManifest, got %v", err)
	}
	if _, statErr := os.Stat(badOut); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("badOut should not exist: %v", statErr)
	}
}

func TestComposeManifest_AVPtsOffsets_Mute_And_PreRangeEnvelope(t *testing.T) {
	if !videoframe.New(videoframe.Options{}).Available() {
		t.Skip("本机没有 ffmpeg / ffprobe，跳过测试")
	}
	dir := t.TempDir()
	e := videoframe.New(videoframe.Options{Timeout: 60 * time.Second})

	// Mute produces digital zero audio regardless of source tone.
	src := filepath.Join(dir, "tone.mp4")
	makeClip(t, src, "red", "160x120", "24", "2", "", true)
	mutedDst := filepath.Join(dir, "muted.mp4")
	if err := e.ComposeManifest(context.Background(), videoframe.ComposeManifestInput{
		Clips: []videoframe.ManifestClip{{
			SegmentID:    1,
			Seq:          1,
			SourcePath:   src,
			MuteOriginal: true,
		}},
		Outputs: []videoframe.ManifestRenderSpec{{Destination: mutedDst}},
	}); err != nil {
		t.Fatal(err)
	}
	mutedPCM := mustDecodeS16Stereo(t, mutedDst)
	if rms := rmsSampleWindow(mutedPCM, 0, len(mutedPCM)/2); rms != 0 {
		t.Fatalf("muted audio expected digital zero, got rms=%.6f", rms)
	}

	// Positive & negative container A/V offset + AvPtsOffsetMs alignment test.
	pulsePos := filepath.Join(dir, "pulse_pos.mp4")
	makePulseClip(t, pulsePos, 2.0, 0.5, 0, 0.2)
	pPos, err := e.ProbeNormalized24FPS(context.Background(), pulsePos)
	if err != nil {
		t.Fatal(err)
	}
	if pPos.AStartUs-pPos.VStartUs < 100_000 {
		t.Fatalf("expected positive aStart-vStart in fixture, got %+v", pPos)
	}
	// Tone originally at 0.5s shifted by container +0.2s -> plays around 0.7s; AvPtsOffsetMs=-200 pulls back to ~0.5s.
	alignedPos := filepath.Join(dir, "aligned_pos.mp4")
	if err := e.ComposeManifest(context.Background(), videoframe.ComposeManifestInput{
		Clips: []videoframe.ManifestClip{{
			SegmentID:     1,
			Seq:           1,
			SourcePath:    pulsePos,
			AvPtsOffsetMs: -200,
		}},
		Outputs: []videoframe.ManifestRenderSpec{{Destination: alignedPos}},
	}); err != nil {
		t.Fatal(err)
	}
	pcmPos := mustDecodeS16Stereo(t, alignedPos)
	if rmsEarly := rmsSampleWindow(pcmPos, 0, 16000); rmsEarly > 0.01 {
		t.Fatalf("expected silence in [0, 0.33s), got rms=%.4f", rmsEarly)
	}
	if rmsTone := rmsSampleWindow(pcmPos, 26000, 44000); rmsTone < 0.08 {
		t.Fatalf("expected tone after 0.54s, got rms=%.4f", rmsTone)
	}

	// Negative container offset (vStart > aStart) + positive AvPtsOffsetMs.
	pulseNeg := filepath.Join(dir, "pulse_neg.mp4")
	makePulseClip(t, pulseNeg, 2.0, 0.6, 0.2, 0)
	pNeg, err := e.ProbeNormalized24FPS(context.Background(), pulseNeg)
	if err != nil {
		t.Fatal(err)
	}
	if pNeg.AStartUs-pNeg.VStartUs > -100_000 {
		t.Fatalf("expected negative aStart-vStart in fixture, got %+v", pNeg)
	}
	alignedNeg := filepath.Join(dir, "aligned_neg.mp4")
	if err := e.ComposeManifest(context.Background(), videoframe.ComposeManifestInput{
		Clips: []videoframe.ManifestClip{{
			SegmentID:     1,
			Seq:           1,
			SourcePath:    pulseNeg,
			AvPtsOffsetMs: 200,
		}},
		Outputs: []videoframe.ManifestRenderSpec{{Destination: alignedNeg}},
	}); err != nil {
		t.Fatal(err)
	}
	pcmNeg := mustDecodeS16Stereo(t, alignedNeg)
	if rmsEarly := rmsSampleWindow(pcmNeg, 0, 16000); rmsEarly > 0.01 {
		t.Fatalf("neg fixture expected silence before 0.33s, got rms=%.4f", rmsEarly)
	}
	if rmsTone := rmsSampleWindow(pcmNeg, 30000, 48000); rmsTone < 0.08 {
		t.Fatalf("neg fixture expected tone around 0.62s+, got rms=%.4f", rmsTone)
	}

	// Pre-range audio continuity: episode has FadeInF=24 starting at frame 0 across a 48-frame clip.
	// Rendering range [24, 48) must carry full amplitude from frame 24 (NOT restart fade-in at rangeIn=24).
	fullDst := filepath.Join(dir, "full_env.mp4")
	rangeDst := filepath.Join(dir, "range_env.mp4")
	end48 := int32(48)
	baseClips := []videoframe.ManifestClip{{
		SegmentID:  1,
		Seq:        1,
		SourcePath: src,
		SourceInF:  0,
		SourceOutF: &end48,
	}}
	if err := e.ComposeManifest(context.Background(), videoframe.ComposeManifestInput{
		Clips:   baseClips,
		FadeInF: 24,
		Outputs: []videoframe.ManifestRenderSpec{{Destination: fullDst}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.ComposeManifest(context.Background(), videoframe.ComposeManifestInput{
		Clips:   baseClips,
		FadeInF: 24,
		RangeF:  &videoframe.FrameRange{InF: 24, OutF: 48},
		Outputs: []videoframe.ManifestRenderSpec{{Destination: rangeDst, MaxEdge: 320}},
	}); err != nil {
		t.Fatal(err)
	}
	rFrames, _, _ := probeFrameCountAndFPS(t, rangeDst)
	if rFrames != 24 {
		t.Fatalf("range preview expected 24 frames, got %d", rFrames)
	}
	fullPCM := mustDecodeS16Stereo(t, fullDst)
	rangePCM := mustDecodeS16Stereo(t, rangeDst)
	fullStartRMS := rmsSampleWindow(fullPCM, 0, 12*2000)
	fullSliceRMS := rmsSampleWindow(fullPCM, 24*2000, 36*2000)
	rangeHeadRMS := rmsSampleWindow(rangePCM, 0, 12*2000)
	if fullStartRMS >= fullSliceRMS*0.5 {
		t.Fatalf("expected fade-in ramp at start of full compose: fullStart=%.4f fullSlice=%.4f", fullStartRMS, fullSliceRMS)
	}
	if math.Abs(fullSliceRMS-rangeHeadRMS) > 0.005 || rangeHeadRMS < 0.05 {
		t.Fatalf("range audio envelope mismatch: full[24..36)=%.4f range[0..12)=%.4f", fullSliceRMS, rangeHeadRMS)
	}
}

func TestComposeManifest_ExplicitExportDimensions(t *testing.T) {
	e := videoframe.New(videoframe.Options{Timeout: 60 * time.Second})
	if !e.Available() {
		t.Skip("缺少 ffmpeg/ffprobe")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "tiny.mp4")
	makeClip(t, src, "green", "160x90", "24", "1", "", false)
	for _, size := range []struct{ w, h int }{{1280, 720}, {1920, 1080}} {
		dst := filepath.Join(dir, fmt.Sprintf("out-%d.mp4", size.w))
		end := int32(1)
		err := e.ComposeManifest(context.Background(), videoframe.ComposeManifestInput{Clips: []videoframe.ManifestClip{{SourcePath: src, SourceOutF: &end}}, Outputs: []videoframe.ManifestRenderSpec{{Destination: dst, Width: size.w, Height: size.h, MaxEdge: size.w}}})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := exec.CommandContext(context.Background(), "ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height,r_frame_rate,sample_aspect_ratio", "-of", "json", dst).Output()
		if err != nil {
			t.Fatal(err)
		}
		var info struct {
			Streams []struct {
				Width  int    `json:"width"`
				Height int    `json:"height"`
				FPS    string `json:"r_frame_rate"`
				SAR    string `json:"sample_aspect_ratio"`
			} `json:"streams"`
		}
		if err = json.Unmarshal(raw, &info); err != nil {
			t.Fatal(err)
		}
		if len(info.Streams) != 1 || info.Streams[0].Width != size.w || info.Streams[0].Height != size.h || info.Streams[0].FPS != "24/1" || info.Streams[0].SAR != "1:1" {
			t.Fatalf("档位未使用真实像素: %s", raw)
		}
	}
}

func TestComposeManifest_StreamingAudioMultiClipAndMaxConcurrentOne(t *testing.T) {
	// 显式以 MaxConcurrent=1 验证流式解复用/混音/WAV 写入不存在信号量自锁，且跨 1s 分块边界混音准确。
	e := videoframe.New(videoframe.Options{MaxConcurrent: 1, Timeout: 60 * time.Second})
	if !e.Available() {
		t.Fatal("本机缺少 ffmpeg / ffprobe，无法验证音频流式混音")
	}
	dir := t.TempDir()
	tone2s := filepath.Join(dir, "tone2s.mp4")
	silent2s := filepath.Join(dir, "silent2s.mp4")
	makeClip(t, tone2s, "blue", "160x120", "24", "2", "", true)
	makeClip(t, silent2s, "gray", "160x120", "24", "2", "", false)

	end24, end36 := int32(24), int32(36)
	outFull := filepath.Join(dir, "multi_full.mp4")
	clips := []videoframe.ManifestClip{
		// Clip 0 [0..24): 从第12帧起裁1秒（验证 K0 丢弃跳过 + 提前终止解码不报错）
		{SegmentID: 1, Seq: 0, SourcePath: tone2s, SourceInF: 12, SourceOutF: &end36, AudioGainDB: -6},
		// Clip 1 [24..48): 静音轨（应为数字零，避免相邻 AAC 帧叠音采样到边缘，稍后避开±2000采样检查）
		{SegmentID: 2, Seq: 1, SourcePath: tone2s, SourceInF: 0, SourceOutF: &end24, MuteOriginal: true},
		// Clip 2 [48..72): 无音频流
		{SegmentID: 3, Seq: 2, SourcePath: silent2s, SourceInF: 0, SourceOutF: &end24},
		// Clip 3 [72..108): 1.5s 跨 48000 采样块边界 + 尾部 12 帧淡出
		{SegmentID: 4, Seq: 3, SourcePath: tone2s, SourceInF: 0, SourceOutF: &end36, FadeOutF: 12},
	}
	if err := e.ComposeManifest(context.Background(), videoframe.ComposeManifestInput{
		Clips:   clips,
		Outputs: []videoframe.ManifestRenderSpec{{Destination: outFull, MaxEdge: 320}},
	}); err != nil {
		t.Fatalf("流式多段混音合成失败: %v", err)
	}
	frames, fps, _ := probeFrameCountAndFPS(t, outFull)
	if frames != 108 || fps != "24/1" {
		t.Fatalf("输出帧数或帧率不符: frames=%d fps=%s", frames, fps)
	}
	pcm := mustDecodeS16Stereo(t, outFull)
	if len(pcm) < 108*2000*2 {
		t.Fatalf("解码立体声采样数不足: got=%d want>=%d", len(pcm), 108*2000*2)
	}
	clip0RMS := rmsSampleWindow(pcm, 4000, 44000)
	if rms := rmsSampleWindow(pcm, 48000+4000, 96000-4000); rms > 0.005 {
		t.Fatalf("Clip1 静音段残留噪音: rms=%.4f", rms)
	}
	if rms := rmsSampleWindow(pcm, 96000+4000, 144000-4000); rms > 0.005 {
		t.Fatalf("Clip2 无音轨段残留噪音: rms=%.4f", rms)
	}
	clip3BodyRMS := rmsSampleWindow(pcm, 144000+4000, 144000+44000)
	clip3TailRMS := rmsSampleWindow(pcm, 216000-12000, 216000-2000)
	if clip0RMS < 0.02 || clip0RMS >= clip3BodyRMS*0.65 || clip0RMS <= clip3BodyRMS*0.35 {
		t.Fatalf("Clip0 -6dB 裁切增益比例异常: clip0=%.4f clip3=%.4f", clip0RMS, clip3BodyRMS)
	}
	if clip3BodyRMS < 0.05 || clip3TailRMS >= clip3BodyRMS*0.75 {
		t.Fatalf("Clip3 跨块音频或尾部淡出异常: body=%.4f tail=%.4f", clip3BodyRMS, clip3TailRMS)
	}

	// 验证局部 RangeF（跨段切片 [18, 90)）同样保持总时长 72 帧且无临时 WAV 泄漏。
	outRange := filepath.Join(dir, "multi_range.mp4")
	if err := e.ComposeManifest(context.Background(), videoframe.ComposeManifestInput{
		Clips:   clips,
		RangeF:  &videoframe.FrameRange{InF: 18, OutF: 90},
		Outputs: []videoframe.ManifestRenderSpec{{Destination: outRange, MaxEdge: 320}},
	}); err != nil {
		t.Fatalf("流式局部区间合成失败: %v", err)
	}
	rFrames, _, _ := probeFrameCountAndFPS(t, outRange)
	if rFrames != 72 {
		t.Fatalf("RangeF [18,90) 帧数错误: %d", rFrames)
	}
}

func setupBoundedStderrS16leFFmpeg(t *testing.T) (string, string, *videoframe.Extractor) {
	t.Helper()
	realFFmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("系统未安装 ffmpeg")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("系统未安装 ffprobe")
	}
	dir := t.TempDir()
	tone1s := filepath.Join(dir, "tone1s.mp4")
	makeClip(t, tone1s, "blue", "160x120", "24", "1", "", true)

	wrapper := filepath.Join(dir, "ffmpeg-stderr-flood.sh")
	script := fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do\n  if [ \"$a\" = \"s16le\" ]; then\n    dd if=/dev/zero bs=1024 count=512 2>/dev/null | tr '\\0' 'E' >&2\n    exit 1\n  fi\ndone\nexec %q \"$@\"\n", realFFmpeg)
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return dir, tone1s, videoframe.New(videoframe.Options{FFmpegPath: wrapper, MaxConcurrent: 2, Timeout: 10 * time.Second})
}

func TestComposeManifestAudio_BoundedStderrDuringExecution(t *testing.T) {
	dir, tone1s, e := setupBoundedStderrS16leFFmpeg(t)
	end24 := int32(24)
	err := e.ComposeManifest(context.Background(), videoframe.ComposeManifestInput{
		Clips:   []videoframe.ManifestClip{{SegmentID: 1, Seq: 0, SourcePath: tone1s, SourceInF: 0, SourceOutF: &end24}},
		Outputs: []videoframe.ManifestRenderSpec{{Destination: filepath.Join(dir, "out.mp4"), MaxEdge: 320}},
	})
	if !errors.Is(err, videoframe.ErrDecode) {
		t.Fatalf("期望返回 ErrDecode，实际: %v", err)
	}
	if msg := err.Error(); len(msg) > 560 || !strings.Contains(msg, strings.Repeat("E", 64)) {
		t.Fatalf("标准错误未在流式收集阶段有界截断或内容缺失: len=%d msg=%q", len(msg), msg)
	}
}

func TestProbeNormalized24FPS_BoundedStderrDuringExecution(t *testing.T) {
	_, tone1s, e := setupBoundedStderrS16leFFmpeg(t)
	_, err := e.ProbeNormalized24FPS(context.Background(), tone1s)
	if !errors.Is(err, videoframe.ErrDecode) {
		t.Fatalf("期望返回 ErrDecode，实际: %v", err)
	}
	if msg := err.Error(); len(msg) > 560 || !strings.Contains(msg, strings.Repeat("E", 64)) {
		t.Fatalf("探测标准错误未在流式收集阶段有界截断或内容缺失: len=%d msg=%q", len(msg), msg)
	}
}

func TestCountFrameCRCLines_StreamingParityAndBounds(t *testing.T) {
	raw := strings.Join([]string{
		"#tb 0: 1/24",
		"   # indented comment",
		"",
		"  \t\r",
		"0, 0, 0, 1, 100, 0x01",
		"  0, 1, 1, 1, 100, 0x02\r",
		"#mid-comment",
		"0, 2, 2, 1, 100, 0x03",
	}, "\n")
	// 无末尾换行符的最后一行也必须计入，且逐字节拆块结果必须一致。
	singleChunks := make([][]byte, len(raw))
	for i := range len(raw) {
		singleChunks[i] = []byte{raw[i]}
	}
	for name, chunks := range map[string][][]byte{
		"whole":      {[]byte(raw)},
		"byteByByte": singleChunks,
		"midToken":   {[]byte(raw[:19]), []byte(raw[19:47]), []byte(raw[47:])},
	} {
		got, err := videoframe.CountFrameCRCLinesForTest(0, chunks...)
		if err != nil || got != 3 {
			t.Fatalf("%s 计数不一致: got=%d err=%v", name, got, err)
		}
	}

	if _, err := videoframe.CountFrameCRCLinesForTest(0, []byte("#tb 0: 1/24\n  \r\n\t#comment\n")); !errors.Is(err, videoframe.ErrDecode) {
		t.Fatalf("零有效帧应返回 ErrDecode，实际: %v", err)
	}
	if got, err := videoframe.CountFrameCRCLinesForTest(math.MaxInt32-1, []byte("0, 1, 1, 1, 1, 0x1\n")); err != nil || got != math.MaxInt32 {
		t.Fatalf("恰好达到 MaxInt32 应成功: got=%d err=%v", got, err)
	}
	if _, err := videoframe.CountFrameCRCLinesForTest(math.MaxInt32, []byte("0, 1, 1, 1, 1, 0x1\n")); !errors.Is(err, videoframe.ErrDecode) {
		t.Fatalf("超过 MaxInt32 应返回 ErrDecode，实际: %v", err)
	}
}

func buildFakeFrameCRCFFmpeg(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fakeffmpeg\ngo 1.22\n"), 0600); err != nil {
		t.Fatal(err)
	}
	src := `package main

import (
	"bufio"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

func main() {
	if slices.Contains(os.Args[1:], "-show_streams") {
		fmt.Print(` + "`" + `{"streams":[{"codec_type":"video","width":160,"height":120,"r_frame_rate":"24/1","duration":"1000.0"}],"format":{"duration":"1000.0"}}` + "`" + `)
		return
	}
	if !slices.Contains(os.Args[1:], "framecrc") {
		os.Exit(2)
	}
	stderrChunk := strings.Repeat("W", 64*1024)
	if os.Getenv("FRAMECRC_FAIL") == "1" {
		stderrChunk = strings.Repeat("E", 64*1024)
	}
	for i := 0; i < 32; i++ {
		_, _ = os.Stderr.WriteString(stderrChunk)
	}
	if os.Getenv("FRAMECRC_FAIL") == "1" {
		os.Exit(1)
	}
	n := 1500000
	if raw := os.Getenv("FRAMECRC_LINES"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			n = parsed
		}
	}
	bw := bufio.NewWriterSize(os.Stdout, 64*1024)
	_, _ = bw.WriteString("#tb 0: 1/24\n#media_type 0: video\n#codec_id 0: rawvideo\n")
	for i := 0; i < n; i++ {
		_, _ = fmt.Fprintf(bw, "0, %10d, %10d,        1,   115200, 0xdeadbeef\n", i, i)
	}
	_ = bw.Flush()
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "fakeffmpeg")
	cmd := exec.CommandContext(context.Background(), "go", "build", "-o", bin, ".")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("编译测试 ffmpeg 桩程序失败: %v\n%s", err, out)
	}
	return bin
}

func TestProbeNormalized24FPS_HugeFrameCRCAndStderrBoundedMemory(t *testing.T) {
	bin := buildFakeFrameCRCFFmpeg(t)
	e := videoframe.New(videoframe.Options{FFmpegPath: bin, FFprobePath: bin, MaxConcurrent: 2, Timeout: 30 * time.Second})

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	probe, err := e.ProbeNormalized24FPS(context.Background(), "huge.mp4")
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("流式解析百万帧 framecrc 失败: %v", err)
	}
	if probe.NormalizedFrames24 != 1_500_000 {
		t.Fatalf("帧数不匹配: got=%d want=1500000", probe.NormalizedFrames24)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 8<<20 {
		t.Fatalf("处理 ~80MB framecrc + 2MB stderr 分配过多内存: TotalAlloc=%d bytes", alloc)
	}

	t.Setenv("FRAMECRC_FAIL", "1")
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err = e.ProbeNormalized24FPS(context.Background(), "huge-err.mp4")
	runtime.ReadMemStats(&after)
	if !errors.Is(err, videoframe.ErrDecode) {
		t.Fatalf("期望返回 ErrDecode，实际: %v", err)
	}
	if msg := err.Error(); len(msg) > 560 || !strings.Contains(msg, strings.Repeat("E", 64)) {
		t.Fatalf("framecrc 标准错误未有界截断: len=%d msg=%q", len(msg), msg)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 4<<20 {
		t.Fatalf("处理 2MB stderr 失败分支分配过多内存: TotalAlloc=%d bytes", alloc)
	}
}
