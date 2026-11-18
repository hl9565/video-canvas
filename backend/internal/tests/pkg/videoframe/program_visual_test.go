package videoframe_test

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/videoframe"
)

func TestProgramVisualFFmpeg(t *testing.T) {
	e := videoframe.New(videoframe.Options{Timeout: 2 * time.Minute})
	if !e.Available() {
		t.Skip("ffmpeg/ffprobe unavailable")
	}
	dir := t.TempDir()
	pattern, gray := filepath.Join(dir, "pattern.png"), filepath.Join(dir, "gray.png")
	writeProgramStill(t, pattern, false)
	writeProgramStill(t, gray, true)
	black := videoframe.ProgramVisualClip{VisualKind: "black", SourceOutF: 13}
	stills := []videoframe.ProgramVisualClip{
		{VisualKind: "still", StillPath: pattern, SourceOutF: 47,
			FX: &model.DramaSegmentFX{ZoomStart: new(1.0), ZoomEnd: new(1.25), ShakePx: new(6.0), CropY: new(0.8), Seed: new(int32(1234)), FlashInF: 4}},
		{VisualKind: "still", StillPath: gray, SourceOutF: 47, FX: &model.DramaSegmentFX{FlashInF: 2, FlashOutF: 3}},
	}
	for _, h := range []int{720, 1080} {
		t.Run(fmt.Sprint(h), func(t *testing.T) {
			in := videoframe.ComposeProgramVisualInput{Width: h * 16 / 9, Height: h, Destination: filepath.Join(dir, fmt.Sprintf("black-%d.mp4", h)), Clips: []videoframe.ProgramVisualClip{black}}
			composeProgramVisualTest(t, e, in, 13)
			for f, y := range programLuma(t, in.Destination) {
				if y != 16 {
					t.Fatalf("black frame %d: Y=%d", f, y)
				}
			}
			in.Clips, in.Destination = append(slices.Clone(stills), black), filepath.Join(dir, fmt.Sprintf("stills-%d.mp4", h))
			composeProgramVisualTest(t, e, in, 107)
			y := programLuma(t, in.Destination)
			baseline := float64(y[52])
			for _, f := range []int{47, 93} {
				expected := baseline + 0.78*(235-baseline)
				if float64(y[f]) < expected-1 || float64(y[f]) > expected || y[f] >= 235 {
					t.Fatalf("flash frame %d: got Y=%d, expected capped blend %f of Y=%f", f, y[f], expected, baseline)
				}
			}
			if y[48] <= y[49] || y[90] != y[52] || y[92] >= y[93] || y[94] != 16 {
				t.Fatalf("incorrect flash interval or clip spill: flash in %v, flash out %v", y[47:51], y[90:95])
			}
			checkProgramVisualSlice(t, e, in, dir)
			in.Clips = []videoframe.ProgramVisualClip{{VisualKind: "still", StillPath: gray, SourceOutF: 13, FX: &model.DramaSegmentFX{FadeInF: 4, FadeOutF: 3}}}
			in.Destination = filepath.Join(dir, fmt.Sprintf("fade-%d.mp4", h))
			composeProgramVisualTest(t, e, in, 13)
			y = programLuma(t, in.Destination)
			if y[0] != 16 || y[12] != 16 || y[4] != y[9] || y[3] >= y[4] || y[10] >= y[9] || y[11] >= y[10] {
				t.Fatalf("incorrect fade boundary: %v", y)
			}
		})
	}
}

func checkProgramVisualSlice(t *testing.T, e *videoframe.Extractor, in videoframe.ComposeProgramVisualInput, dir string) {
	t.Helper()
	full := programFrameHashes(t, in.Destination)
	in.Destination = filepath.Join(dir, fmt.Sprintf("repeat-%d.mp4", in.Height))
	composeProgramVisualTest(t, e, in, 107)
	if !slices.Equal(full, programFrameHashes(t, in.Destination)) {
		t.Fatal("same seed changed rendered pixels")
	}
	for _, r := range []videoframe.FrameRange{{InF: 10, OutF: 30}, {InF: 44, OutF: 51}, {InF: 91, OutF: 98}} {
		in.RangeF = &r
		in.Destination = filepath.Join(dir, fmt.Sprintf("slice-%d-%d.mp4", in.Height, r.InF))
		composeProgramVisualTest(t, e, in, int(r.OutF-r.InF))
		if !slices.Equal(full[r.InF:r.OutF], programFrameHashes(t, in.Destination)) {
			t.Fatalf("range [%d,%d) changed rendered pixels", r.InF, r.OutF)
		}
	}
	if in.Height == 720 {
		in.Clips = slices.Clone(in.Clips)
		fx := *in.Clips[0].FX
		fx.Seed = new(int32(4321))
		in.Clips[0].FX = &fx
		in.Destination, in.RangeF = filepath.Join(dir, "different-seed.mp4"), &videoframe.FrameRange{InF: 10, OutF: 30}
		composeProgramVisualTest(t, e, in, 20)
		if slices.Equal(full[10:30], programFrameHashes(t, in.Destination)) {
			t.Fatal("changing the seed did not change shake pixels")
		}
	}
}

func composeProgramVisualTest(t *testing.T, e *videoframe.Extractor, in videoframe.ComposeProgramVisualInput, frames int) {
	t.Helper()
	if err := e.ComposeProgramVisual(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	b := runProgramVisualTestCommand(t, "ffprobe", "-v", "error", "-count_frames",
		"-show_entries", "stream=codec_name,width,height,r_frame_rate,avg_frame_rate,sample_aspect_ratio,pix_fmt,nb_read_frames", "-of", "json", in.Destination)
	var probe struct {
		Streams []struct {
			Codec       string `json:"codec_name"`
			Width       int    `json:"width"`
			Height      int    `json:"height"`
			FPS         string `json:"r_frame_rate"`
			AverageFPS  string `json:"avg_frame_rate"`
			SAR         string `json:"sample_aspect_ratio"`
			PixelFormat string `json:"pix_fmt"`
			Frames      string `json:"nb_read_frames"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(b, &probe); err != nil || len(probe.Streams) != 1 {
		t.Fatalf("invalid probe: %s (%v)", b, err)
	}
	st := probe.Streams[0]
	if st.Width != in.Width || st.Height != in.Height || st.FPS != "24/1" || st.AverageFPS != "24/1" || st.SAR != "1:1" || st.Codec != "h264" || st.PixelFormat != "yuv420p" || st.Frames != fmt.Sprint(frames) {
		t.Fatalf("unexpected output: %+v", st)
	}
}

func runProgramVisualTestCommand(t *testing.T, bin string, args ...string) []byte {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), bin, args...).Output()
	if err != nil {
		t.Fatalf("%s: %v", bin, err)
	}
	return out
}

func programLuma(t *testing.T, path string) []byte {
	t.Helper()
	raw := runProgramVisualTestCommand(t, "ffmpeg", "-v", "error", "-i", path, "-vf", "crop=2:2", "-pix_fmt", "yuv420p", "-f", "rawvideo", "-")
	var out []byte
	for i := 0; i < len(raw); i += 6 {
		out = append(out, raw[i])
	}
	return out
}

func programFrameHashes(t *testing.T, path string) []string {
	t.Helper()
	raw := runProgramVisualTestCommand(t, "ffmpeg", "-v", "error", "-i", path, "-f", "framemd5", "-")
	var hashes []string
	for line := range strings.Lines(string(raw)) {
		if !strings.HasPrefix(line, "#") {
			hashes = append(hashes, strings.TrimSpace(line[strings.LastIndex(line, ",")+1:]))
		}
	}
	return hashes
}

func writeProgramStill(t *testing.T, path string, gray bool) {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 173, 211))
	for y := range 211 {
		for x := range 173 {
			c := color.NRGBA{R: 128, G: 128, B: 128, A: 255}
			if !gray {
				c = color.NRGBA{R: uint8(3*x + y), G: uint8(x * y), B: uint8(y*7 + x), A: 255}
			}
			img.SetNRGBA(x, y, c)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writeErr, closeErr := png.Encode(f, img), f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("write still: %v %v", writeErr, closeErr)
	}
}

func TestProgramVisualVideoNormalizesTrimRotationAndSAR(t *testing.T) {
	e := videoframe.New(videoframe.Options{})
	if !e.Available() {
		t.Skip("ffmpeg/ffprobe unavailable")
	}
	dir := t.TempDir()
	src, rotated := filepath.Join(dir, "source.mp4"), filepath.Join(dir, "rotated.mp4")
	// 旋转后上下两半成为左右两半；SAR 正确时横向绿条在 cover 裁剪范围之外。
	runProgramVisualTestCommand(t, "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "color=red:s=160x90:r=30:d=1",
		"-vf", "drawbox=x=0:y=45:w=160:h=45:color=blue:t=fill,drawbox=x=102:y=0:w=4:h=90:color=lime:t=fill,setsar=2/1",
		"-c:v", "libx264", "-y", src)
	runProgramVisualTestCommand(t, "ffmpeg", "-v", "error", "-display_rotation:v:0", "90", "-i", src, "-c", "copy", "-y", rotated)
	clip := videoframe.ProgramVisualClip{VisualKind: "video", SourcePath: rotated,
		SourceInF: 3, SourceOutF: 17, NormalizedFrames24: 24, FX: &model.DramaSegmentFX{FadeInF: 4, FadeOutF: 3}}
	in := videoframe.ComposeProgramVisualInput{Width: 1280, Height: 720, Destination: filepath.Join(dir, "trim.mp4"), Clips: []videoframe.ProgramVisualClip{clip}}
	composeProgramVisualTest(t, e, in, 14)
	pixels := runProgramVisualTestCommand(t, "ffmpeg", "-v", "error", "-i", in.Destination,
		"-vf", "select=eq(n\\,7)", "-frames:v", "1", "-pix_fmt", "rgb24", "-f", "rawvideo", "-")
	left := pixels[(360*1280+300)*3:][:3]
	right := pixels[(360*1280+980)*3:][:3]
	redLeft, redRight := left[0] > 200 && right[2] > 200, right[0] > 200 && left[2] > 200
	if !redLeft && !redRight {
		t.Fatalf("rotation did not turn horizontal split into vertical split: left=%v right=%v", left, right)
	}
	for y := range 720 {
		if p := pixels[(y*1280+300)*3:][:3]; p[1] > 100 {
			t.Fatalf("unexpected green stripe at y=%d: %v; input SAR was not preserved", y, p)
		}
	}
	full := programFrameHashes(t, in.Destination)
	in.Destination, in.RangeF = filepath.Join(dir, "preview.mp4"), &videoframe.FrameRange{InF: 1, OutF: 13}
	composeProgramVisualTest(t, e, in, 12)
	if !slices.Equal(full[1:13], programFrameHashes(t, in.Destination)) {
		t.Fatal("video preview changed trim or fade pixels")
	}

	// 源媒体比冻结的帧数短时，效果遮罩不能重复最后一帧来掩盖截断。
	in.Destination, in.RangeF = filepath.Join(dir, "short.mp4"), nil
	in.Clips[0].SourceOutF, in.Clips[0].NormalizedFrames24 = 30, 30
	if err := e.ComposeProgramVisual(t.Context(), in); err == nil {
		t.Fatal("truncated source silently produced a complete visual")
	}
	if _, err := os.Stat(in.Destination); !os.IsNotExist(err) {
		t.Fatalf("truncated visual published output: %v", err)
	}
}
