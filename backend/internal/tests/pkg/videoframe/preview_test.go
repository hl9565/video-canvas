package videoframe_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"video-canvas/internal/pkg/videoframe"
)

func previewTool(t *testing.T, script string) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(filename, []byte("#!/bin/sh\nset -eu\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestPreviewEncoderLimits(t *testing.T) {
	src := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(src, []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	codec := previewTool(t, `
if [ "$2" = -encoders ]; then echo ' V..... mjpeg MJPEG'; exit; fi
case " $* " in *" -protocol_whitelist file "*) ;; *) exit 9;; esac
head -c 1048577 /dev/zero
`)
	for _, tc := range []struct {
		name, dimensions string
		want             error
	}{
		{"too many pixels", `"width":100000,"height":100000`, videoframe.ErrTooLarge},
		{"invalid dimensions", `"width":0,"height":1`, videoframe.ErrDecode},
		{"oversized output", `"width":512,"height":512`, videoframe.ErrTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := previewTool(t, `
case " $* " in *" -protocol_whitelist file "*) ;; *) exit 9;; esac
echo '{"streams":[{`+tc.dimensions+`}]}'
`)
			encoder := videoframe.NewPreviewEncoder(videoframe.PreviewOptions{FFmpegPath: codec, FFprobePath: probe})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var output bytes.Buffer
			if _, err := encoder.Encode(ctx, src, &output); !errors.Is(err, tc.want) || output.Len() > 1<<20 {
				t.Fatalf("err=%v want=%v output=%d", err, tc.want, output.Len())
			}
		})
	}
	encoder := videoframe.NewPreviewEncoder(videoframe.PreviewOptions{FFmpegPath: codec, FFprobePath: codec})
	if _, err := encoder.Encode(t.Context(), "https://example.invalid/image", io.Discard); !errors.Is(err, videoframe.ErrDecode) {
		t.Fatalf("远程输入应拒绝: %v", err)
	}
	missing := videoframe.NewPreviewEncoder(videoframe.PreviewOptions{FFmpegPath: src + "-missing", FFprobePath: codec})
	if _, err := missing.Encode(t.Context(), src, io.Discard); !errors.Is(err, videoframe.ErrUnavailable) {
		t.Fatalf("缺少工具应返回 ErrUnavailable: %v", err)
	}
}

func TestPreviewEncoderRealMedia(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("缺少 ffmpeg")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("缺少 ffprobe")
	}
	dir := t.TempDir()
	still := filepath.Join(dir, "image.png")
	writeTransparentPreviewImage(t, still)
	anamorphic := filepath.Join(dir, "sar.mp4")
	runPreviewFFmpeg(t, ffmpeg, "-i", still, "-vf", "scale=640:320,setsar=2/1", "-c:v", "mpeg4", anamorphic)
	rotated := filepath.Join(dir, "rotation.mp4")
	runPreviewFFmpeg(t, ffmpeg, "-display_rotation", "90", "-i", anamorphic, "-c", "copy", rotated)
	codecs, err := exec.CommandContext(t.Context(), ffmpeg, "-hide_banner", "-encoders").Output()
	if err != nil {
		t.Fatal(err)
	}
	webp := strings.Contains(string(codecs), " libwebp ")
	encoder := videoframe.NewPreviewEncoder(videoframe.PreviewOptions{})
	for _, tc := range []struct {
		src           string
		width, height int
	}{{still, 512, 256}, {anamorphic, 512, 128}, {rotated, 128, 512}} {
		t.Run(filepath.Base(tc.src), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var output bytes.Buffer
			info, err := encoder.Encode(ctx, tc.src, &output)
			if err != nil {
				t.Fatal(err)
			}
			if webp {
				checkWebPPreview(t, ffmpeg, info, output.Bytes(), tc.width, tc.height, tc.src == still)
				return
			}
			cfg, err := jpeg.DecodeConfig(&output)
			if info.Format != "jpeg" || info.MIME != "image/jpeg" || err != nil || cfg.Width != tc.width || cfg.Height != tc.height {
				t.Fatalf("info=%+v JPEG=%+v err=%v", info, cfg, err)
			}
		})
	}
	for _, contents := range []string{"not an image", "ffconcat version 1.0\nfile 'image.png'\n"} {
		filename := filepath.Join(dir, "invalid")
		if err := os.WriteFile(filename, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := encoder.Encode(t.Context(), filename, io.Discard); !errors.Is(err, videoframe.ErrDecode) {
			t.Fatalf("损坏文件或播放列表应该拒绝: %v", err)
		}
	}
}

func writeTransparentPreviewImage(t *testing.T, path string) {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 1024, 512))
	img.SetNRGBA(512, 256, color.NRGBA{R: 255, A: 255})
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := png.Encode(file, img); err != nil {
		t.Fatal(err)
	}
}

func runPreviewFFmpeg(t *testing.T, ffmpeg string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), ffmpeg, append([]string{"-v", "error", "-filter_threads", "1"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("生成测试夹具: %v: %s", err, out)
	}
}

func checkWebPPreview(t *testing.T, ffmpeg string, info videoframe.PreviewInfo, data []byte, width, height int, transparent bool) {
	t.Helper()
	if info.Format != "webp" || info.MIME != "image/webp" || len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		t.Fatalf("不是 WebP: %+v", info)
	}
	img := decodePreviewPNG(t, ffmpeg, data)
	if img.Bounds().Size() != image.Pt(width, height) {
		t.Fatalf("WebP 尺寸错误: %v", img.Bounds())
	}
	if _, _, _, alpha := img.At(0, 0).RGBA(); transparent && alpha != 0 {
		t.Fatalf("透明像素 alpha=%d", alpha)
	}
}

func decodePreviewPNG(t *testing.T, ffmpeg string, data []byte) image.Image {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), ffmpeg, "-v", "error", "-i", "pipe:0", "-frames:v", "1", "-c:v", "png", "-f", "image2pipe", "pipe:1")
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestPreviewEncoderAnimatedWebP(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("缺少 ffmpeg")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("缺少 ffprobe")
	}
	demuxers, err := exec.CommandContext(t.Context(), ffmpeg, "-hide_banner", "-demuxers").Output()
	if err != nil || !strings.Contains(string(demuxers), " webp_anim ") {
		t.Skip("FFmpeg 不支持动画 WebP 解码")
	}
	const twoFrames = "UklGRoQAAABXRUJQVlA4WAoAAAACAAAAHwAADwAAQU5JTQYAAAD/////AABBTk1GKAAAAAAAAAAAAB8AAA8AAGQAAAJWUDhMDwAAAC8fwAMABxD1j/4HIqL/AQBBTk1GKAAAAAAAAAAAAB8AAA8AAGQAAABWUDhMDwAAAC8fwAMABxDR//4HIqL/AQA="
	data, err := base64.StdEncoding.DecodeString(twoFrames)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "red-then-blue.webp")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	encoder := videoframe.NewPreviewEncoder(videoframe.PreviewOptions{})
	if _, err := encoder.Encode(t.Context(), src, &output); err != nil {
		t.Fatal(err)
	}
	img := decodePreviewPNG(t, ffmpeg, output.Bytes())
	red, _, blue, _ := img.At(0, 0).RGBA()
	if img.Bounds().Size() != image.Pt(32, 16) || red < 0xd000 || blue > 0x2000 {
		t.Fatalf("应保留红色首帧且不放大: %v R=%d B=%d", img.Bounds(), red, blue)
	}
}
