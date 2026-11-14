package videoframe_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"video-canvas/internal/pkg/videoframe"
)

type streamProbe struct {
	Streams []struct {
		CodecType          string `json:"codec_type"`
		CodecName          string `json:"codec_name"`
		Width              int    `json:"width"`
		Height             int    `json:"height"`
		SampleAspectRatio  string `json:"sample_aspect_ratio"`
		DisplayAspectRatio string `json:"display_aspect_ratio"`
		RFrameRate         string `json:"r_frame_rate"`
		AvgFrameRate       string `json:"avg_frame_rate"`
		Duration           string `json:"duration"`
	} `json:"streams"`
}

func probeComposeResult(t *testing.T, path string) streamProbe {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "ffprobe",
		"-v", "error", "-show_streams", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var p streamProbe
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("parse ffprobe json: %v", err)
	}
	return p
}

func runFFmpeg(t *testing.T, args ...string) {
	t.Helper()
	if !videoframe.New(videoframe.Options{}).Available() {
		t.Skip("本机没有 ffmpeg / ffprobe，跳过合成测试")
	}
	if out, err := exec.CommandContext(context.Background(), "ffmpeg", append([]string{"-v", "error"}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}

func makeClip(t *testing.T, path, color, size, fps, dur string, vf string, withAudio bool) {
	t.Helper()
	args := []string{"-f", "lavfi", "-i", "color=c=" + color + ":s=" + size + ":r=" + fps + ":d=" + dur}
	if withAudio {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration="+dur, "-c:a", "aac")
	}
	if vf != "" {
		args = append(args, "-vf", vf)
	}
	args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-y", path)
	runFFmpeg(t, args...)
}

func decodeLumaFrame(t *testing.T, path string, atSec float64, w, h int) []byte {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "ffmpeg",
		"-v", "error", "-ss", formatSec(atSec), "-i", path,
		"-frames:v", "1", "-f", "rawvideo", "-pix_fmt", "gray", "-").Output()
	if err != nil {
		t.Fatalf("decode gray frame: %v", err)
	}
	if len(out) != w*h {
		t.Fatalf("unexpected gray buffer size %d != %d*%d", len(out), w, h)
	}
	return out
}

func decodeAudioRMS(t *testing.T, path string, startSec, durSec float64) float64 {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "ffmpeg",
		"-v", "error", "-ss", formatSec(startSec), "-t", formatSec(durSec), "-i", path,
		"-vn", "-ac", "1", "-ar", "48000", "-f", "s16le", "-").Output()
	if err != nil {
		t.Fatalf("decode pcm window: %v", err)
	}
	n := len(out) / 2
	if n == 0 {
		t.Fatalf("empty pcm window at %.2f+%.2f", startSec, durSec)
	}
	var sumSq float64
	for i := 0; i < n; i++ {
		sample := float64(int16(binary.LittleEndian.Uint16(out[i*2:i*2+2]))) / 32768.0
		sumSq += sample * sample
	}
	return math.Sqrt(sumSq / float64(n))
}

func formatSec(v float64) string {
	return strconv.FormatFloat(v, 'f', 3, 64)
}

func TestCompose_NonSquareSAR_CanonicalSquare(t *testing.T) {
	dir := t.TempDir()
	clip0 := filepath.Join(dir, "sar.mp4")
	clip1 := filepath.Join(dir, "sq.mp4")
	makeClip(t, clip0, "white", "160x120", "24", "0.5", "setsar=3/4", false)
	makeClip(t, clip1, "gray", "160x160", "24", "0.5", "", false)

	dst := filepath.Join(dir, "out.mp4")
	e := videoframe.New(videoframe.Options{Timeout: 30 * time.Second})
	if err := e.Compose(context.Background(), []string{clip0, clip1}, dst); err != nil {
		t.Fatal(err)
	}
	p := probeComposeResult(t, dst)
	for _, s := range p.Streams {
		if s.CodecType == "video" {
			if s.Width != 160 || s.Height != 160 || s.SampleAspectRatio != "1:1" || s.DisplayAspectRatio != "1:1" {
				t.Fatalf("expected canonical 160x160 SAR 1:1 DAR 1:1, got %+v", s)
			}
			return
		}
	}
	t.Fatal("missing video stream")
}

func TestCompose_Rotation90_CanonicalPortrait(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "raw.mp4")
	rot90 := filepath.Join(dir, "rot90.mp4")
	port := filepath.Join(dir, "port.mp4")
	dst := filepath.Join(dir, "out.mp4")

	makeClip(t, raw, "white", "320x180", "24", "0.5", "", false)
	runFFmpeg(t, "-display_rotation:v:0", "90", "-i", raw, "-c", "copy", "-y", rot90)
	makeClip(t, port, "gray", "180x320", "24", "0.5", "", false)

	e := videoframe.New(videoframe.Options{Timeout: 30 * time.Second})
	if err := e.Compose(context.Background(), []string{rot90, port}, dst); err != nil {
		t.Fatal(err)
	}
	p := probeComposeResult(t, dst)
	for _, s := range p.Streams {
		if s.CodecType == "video" {
			if s.Width != 180 || s.Height != 320 {
				t.Fatalf("expected rotated 180x320 portrait, got %dx%d", s.Width, s.Height)
			}
		}
	}
	gray := decodeLumaFrame(t, dst, 0.25, 180, 320)
	if gray[2*180+90] < 100 || gray[160*180+2] < 100 {
		t.Fatalf("unexpected dark border in rotated portrait frame: top=%d left=%d", gray[2*180+90], gray[160*180+2])
	}
}

func TestCompose_PortraitNearARUpgrade_ZeroBlackBars(t *testing.T) {
	dir := t.TempDir()
	c0 := filepath.Join(dir, "p720.mp4")
	c1 := filepath.Join(dir, "p1080.mp4")
	dst := filepath.Join(dir, "out.mp4")

	makeClip(t, c0, "white", "480x864", "24", "1.0", "", true)
	makeClip(t, c1, "gray", "544x960", "24", "1.0", "", true)

	e := videoframe.New(videoframe.Options{Timeout: 30 * time.Second})
	if err := e.Compose(context.Background(), []string{c0, c1}, dst); err != nil {
		t.Fatal(err)
	}
	p := probeComposeResult(t, dst)
	for _, s := range p.Streams {
		if s.CodecType == "video" {
			if s.Width != 544 || s.Height != 960 || s.RFrameRate != "24/1" || s.AvgFrameRate != "24/1" {
				t.Fatalf("expected upgraded 544x960 @ 24/1 CFR, got %+v", s)
			}
		}
	}
	for _, ts := range []float64{0.5, 1.5} {
		gray := decodeLumaFrame(t, dst, ts, 544, 960)
		for _, idx := range []int{2*544 + 272, 957*544 + 272, 480*544 + 2, 480*544 + 541} {
			if gray[idx] < 60 {
				t.Fatalf("detected black border pixel luma=%d at ts=%.1f idx=%d", gray[idx], ts, idx)
			}
		}
	}
}

func TestCompose_CrossAspectRatio_SafePad(t *testing.T) {
	dir := t.TempDir()
	wide := filepath.Join(dir, "wide.mp4")
	tall := filepath.Join(dir, "tall.mp4")
	dst := filepath.Join(dir, "out.mp4")

	makeClip(t, wide, "gray", "320x180", "24", "0.5", "", false)
	makeClip(t, tall, "white", "180x320", "24", "0.5", "", false)

	e := videoframe.New(videoframe.Options{Timeout: 30 * time.Second})
	if err := e.Compose(context.Background(), []string{wide, tall}, dst); err != nil {
		t.Fatal(err)
	}
	gray := decodeLumaFrame(t, dst, 0.75, 320, 180)
	if gray[90*320+10] > 30 || gray[90*320+160] < 180 {
		t.Fatalf("expected pillarboxed white in 16:9 canvas, left=%d center=%d", gray[90*320+10], gray[90*320+160])
	}
}

func TestCompose_CapsDimensionsAbove1920(t *testing.T) {
	dir := t.TempDir()
	uhd := filepath.Join(dir, "uhd.mp4")
	dst := filepath.Join(dir, "out.mp4")
	makeClip(t, uhd, "gray", "3840x2160", "24", "0.2", "", false)

	e := videoframe.New(videoframe.Options{Timeout: 30 * time.Second})
	if err := e.Compose(context.Background(), []string{uhd}, dst); err != nil {
		t.Fatal(err)
	}
	p := probeComposeResult(t, dst)
	for _, s := range p.Streams {
		if s.CodecType == "video" {
			if s.Width != 1920 || s.Height != 1080 {
				t.Fatalf("expected capped 1920x1080, got %dx%d", s.Width, s.Height)
			}
			return
		}
	}
	t.Fatal("missing video stream")
}

func TestCompose_FrameRateSelectionAndNTSCAudioSync(t *testing.T) {
	dir := t.TempDir()
	e := videoframe.New(videoframe.Options{Timeout: 30 * time.Second})

	p1, p2 := filepath.Join(dir, "pal1.mp4"), filepath.Join(dir, "pal2.mp4")
	makeClip(t, p1, "red", "160x120", "25", "0.4", "", false)
	makeClip(t, p2, "blue", "160x120", "25", "0.4", "", true)
	palOut := filepath.Join(dir, "pal.mp4")
	if err := e.Compose(context.Background(), []string{p1, p2}, palOut); err != nil {
		t.Fatal(err)
	}
	assertFPS(t, probeComposeResult(t, palOut), "25/1")

	m24, m30 := filepath.Join(dir, "f24.mp4"), filepath.Join(dir, "f30.mp4")
	makeClip(t, m24, "red", "160x120", "24", "0.5", "", false)
	makeClip(t, m30, "blue", "160x120", "30", "0.5", "", false)
	mixOut := filepath.Join(dir, "mix.mp4")
	if err := e.Compose(context.Background(), []string{m24, m30}, mixOut); err != nil {
		t.Fatal(err)
	}
	assertFPS(t, probeComposeResult(t, mixOut), "30/1")

	n1, n2, n3 := filepath.Join(dir, "n1.mp4"), filepath.Join(dir, "n2.mp4"), filepath.Join(dir, "n3.mp4")
	makeClip(t, n1, "red", "160x120", "24000/1001", "1.0", "", true)
	makeClip(t, n2, "green", "160x120", "24000/1001", "1.0", "", false)
	makeClip(t, n3, "blue", "160x120", "24000/1001", "1.0", "", true)
	ntscOut := filepath.Join(dir, "ntsc.mp4")
	if err := e.Compose(context.Background(), []string{n1, n2, n3}, ntscOut); err != nil {
		t.Fatal(err)
	}
	ntscProbe := probeComposeResult(t, ntscOut)
	assertFPS(t, ntscProbe, "24000/1001")
	assertAVDurationAligned(t, ntscProbe, 0.03)
}

func TestCompose_AudioStartOffsetSync_LateAndEarly(t *testing.T) {
	dir := t.TempDir()
	e := videoframe.New(videoframe.Options{Timeout: 30 * time.Second})

	lateIn := filepath.Join(dir, "late.mp4")
	runFFmpeg(t, "-f", "lavfi", "-i", "color=c=green:s=160x120:r=24:d=1",
		"-itsoffset", "0.2", "-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=48000:duration=0.8,volume=4",
		"-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-t", "1", "-y", lateIn)
	lateOut := filepath.Join(dir, "late_out.mp4")
	if err := e.Compose(context.Background(), []string{lateIn}, lateOut); err != nil {
		t.Fatal(err)
	}
	if rms := decodeAudioRMS(t, lateOut, 0.02, 0.12); rms > 0.02 {
		t.Fatalf("late audio should start silent before 200ms, got RMS=%.4f", rms)
	}
	if rms := decodeAudioRMS(t, lateOut, 0.30, 0.40); rms < 0.10 {
		t.Fatalf("late audio tone missing after 200ms, got RMS=%.4f", rms)
	}

	earlyIn := filepath.Join(dir, "early.mp4")
	runFFmpeg(t, "-f", "lavfi", "-i", "color=c=blue:s=160x120:r=24:d=0.8",
		"-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=48000:duration=1.1,volume='if(between(t,0.40,0.80),4,0)':eval=frame",
		"-vf", "setpts=PTS+0.25/TB",
		"-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-bf", "0", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-y", earlyIn)
	earlyOut := filepath.Join(dir, "early_out.mp4")
	if err := e.Compose(context.Background(), []string{earlyIn}, earlyOut); err != nil {
		t.Fatal(err)
	}
	if rms := decodeAudioRMS(t, earlyOut, 0.01, 0.08); rms > 0.02 {
		t.Fatalf("early audio should be silent in [0.01,0.09], got RMS=%.4f", rms)
	}
	if rms := decodeAudioRMS(t, earlyOut, 0.22, 0.24); rms < 0.10 {
		t.Fatalf("early audio should shift burst earlier into [0.22,0.46], got RMS=%.4f", rms)
	}
	if rms := decodeAudioRMS(t, earlyOut, 0.65, 0.10); rms > 0.02 {
		t.Fatalf("early audio burst was not shifted earlier (still loud at [0.65,0.75]), got RMS=%.4f", rms)
	}
}

func assertFPS(t *testing.T, p streamProbe, want string) {
	t.Helper()
	for _, s := range p.Streams {
		if s.CodecType == "video" {
			if s.RFrameRate != want || s.AvgFrameRate != want {
				t.Fatalf("expected fps %s, got r=%s avg=%s", want, s.RFrameRate, s.AvgFrameRate)
			}
			return
		}
	}
	t.Fatal("missing video stream")
}

func assertAVDurationAligned(t *testing.T, p streamProbe, maxDelta float64) {
	t.Helper()
	var vDur, aDur float64
	for _, s := range p.Streams {
		val, err := strconv.ParseFloat(s.Duration, 64)
		if err != nil {
			t.Fatalf("invalid stream duration %q: %v", s.Duration, err)
		}
		switch s.CodecType {
		case "video":
			vDur = val
		case "audio":
			aDur = val
		}
	}
	if math.Abs(vDur-aDur) > maxDelta {
		t.Fatalf("A/V duration drift too large: v=%.6f a=%.6f (diff=%.6f > %.3f)", vDur, aDur, math.Abs(vDur-aDur), maxDelta)
	}
}
