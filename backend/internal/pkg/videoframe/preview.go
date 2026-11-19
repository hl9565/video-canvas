package videoframe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	previewPixels = 40_000_000
	previewBytes  = 1 << 20
	previewMeta   = 64 << 10
	// 只接受媒体容器和单文件图片；仅限制协议仍会允许 concat 清单读取其他本地文件。
	previewFormats = "mov,mp4,m4a,3gp,3g2,mj2,matroska,webm,avi,mpeg,mpegts,flv,asf,ogg,gif,gif_pipe,apng,png_pipe,jpeg_pipe,webp_pipe,webp_anim,bmp_pipe,tiff_pipe,avif"
	// 缩放发生在默认旋转之后，dar 此时已计入旋转和非方形像素。每一轴都不放大。
	previewScale = "scale=w='max(1,round(min(min(iw,512),min(ih,512)*dar)))':h='max(1,round(min(min(ih,512),min(iw,512)/dar)))',setsar=1"
)

// ErrTooLarge 表示预览超过输入像素或输出字节上限。
var ErrTooLarge = errors.New("预览超过大小限制")

// PreviewInfo 描述实际编码的图片格式和 MIME。
type PreviewInfo struct {
	Format string
	MIME   string
}

// PreviewOptions 指定可执行文件；空值从 PATH 查找。
type PreviewOptions struct {
	FFmpegPath  string
	FFprobePath string
}

// PreviewEncoder 从本地媒体编码单帧；由调用方限制并发和总耗时。
type PreviewEncoder struct {
	ffmpeg, ffprobe string
	formatGate      chan struct{}
	checked         bool
	format          PreviewInfo
}

// NewPreviewEncoder 创建不占用 Agent 抽帧并发配额的预览编码器。
func NewPreviewEncoder(opts PreviewOptions) *PreviewEncoder {
	return &PreviewEncoder{
		ffmpeg:     previewToolPath(opts.FFmpegPath, "ffmpeg"),
		ffprobe:    previewToolPath(opts.FFprobePath, "ffprobe"),
		formatGate: make(chan struct{}, 1),
	}
}

func previewToolPath(name, fallback string) string {
	if name == "" {
		name = fallback
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	return abs
}

// Format 返回所选格式，优先 WebP；被取消的探测不会缓存。
func (e *PreviewEncoder) Format(ctx context.Context) (PreviewInfo, error) {
	if e.ffmpeg == "" || e.ffprobe == "" {
		return PreviewInfo{}, ErrUnavailable
	}
	select {
	case e.formatGate <- struct{}{}:
		defer func() { <-e.formatGate }()
	case <-ctx.Done():
		return PreviewInfo{}, ctx.Err()
	}
	if !e.checked {
		var output bytes.Buffer
		if err := runPreview(ctx, e.ffmpeg, &output, previewMeta, "-hide_banner", "-encoders"); err != nil {
			if ctx.Err() != nil {
				return PreviewInfo{}, ctx.Err()
			}
			return PreviewInfo{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
		e.format = previewFormat(output.String())
		e.checked = true
	}
	if e.format.Format == "" {
		return PreviewInfo{}, ErrUnavailable
	}
	return e.format, nil
}

func previewFormat(list string) PreviewInfo {
	var fallback PreviewInfo
	for line := range strings.SplitSeq(list, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "V") {
			continue
		}
		switch fields[1] {
		case "libwebp":
			return PreviewInfo{Format: "webp", MIME: "image/webp"}
		case "mjpeg":
			fallback = PreviewInfo{Format: "jpeg", MIME: "image/jpeg"}
		}
	}
	return fallback
}

// Encode 探测本地源的尺寸，再流式写出不超过 1 MiB 的单帧预览。
func (e *PreviewEncoder) Encode(ctx context.Context, srcPath string, dst io.Writer) (PreviewInfo, error) {
	if !filepath.IsAbs(srcPath) {
		return PreviewInfo{}, fmt.Errorf("%w: 必须为本地绝对路径", ErrDecode)
	}
	info, err := os.Stat(srcPath)
	if err != nil {
		return PreviewInfo{}, fmt.Errorf("检查预览源: %w", err)
	}
	if !info.Mode().IsRegular() {
		return PreviewInfo{}, ErrDecode
	}
	format, err := e.Format(ctx)
	if err != nil {
		return PreviewInfo{}, err
	}
	if err := e.probePreview(ctx, srcPath); err != nil {
		return PreviewInfo{}, err
	}
	if err := runPreview(ctx, e.ffmpeg, dst, previewBytes, previewArgs(srcPath, format)...); err != nil {
		return PreviewInfo{}, err
	}
	return format, nil
}

func (e *PreviewEncoder) probePreview(ctx context.Context, srcPath string) error {
	var result struct {
		Streams []struct{ Width, Height int64 }
	}
	var output bytes.Buffer
	err := runPreview(ctx, e.ffprobe, &output, previewMeta, "-v", "error", "-protocol_whitelist", "file",
		"-format_whitelist", previewFormats, "-threads", "1", "-max_pixels", "40000000",
		"-select_streams", "V:0", "-show_entries", "stream=width,height", "-of", "json", srcPath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || len(result.Streams) != 1 {
		return ErrDecode
	}
	stream := result.Streams[0]
	if stream.Width <= 0 || stream.Height <= 0 {
		return ErrDecode
	}
	if stream.Width > previewPixels/stream.Height {
		return ErrTooLarge
	}
	return nil
}

func previewArgs(srcPath string, format PreviewInfo) []string {
	args := []string{"-v", "error", "-nostdin", "-xerror", "-protocol_whitelist", "file", "-format_whitelist", previewFormats,
		"-threads", "1", "-max_pixels", "40000000", "-filter_threads", "1",
		"-i", srcPath, "-map", "0:V:0", "-frames:v", "1", "-vf", previewScale, "-threads", "1"}
	if format.Format == "webp" {
		return append(args, "-pix_fmt", "bgra", "-c:v", "libwebp", "-quality", "75", "-compression_level", "4", "-f", "webp", "pipe:1")
	}
	return append(args, "-pix_fmt", "yuvj420p", "-strict", "unofficial", "-c:v", "mjpeg", "-q:v", "4", "-f", "image2", "-update", "1", "pipe:1")
}

func runPreview(ctx context.Context, bin string, dst io.Writer, limit int, args ...string) error {
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	output := &previewWriter{dst: dst, remaining: limit, cancel: cancel}
	// bin 是预先解析的可信绝对路径；参数由本包构造，媒体路径只作为单独的输入参数传递。
	cmd := exec.CommandContext(commandCtx, bin)
	cmd.Args = append(cmd.Args, args...)
	cmd.Stdout = output
	err := cmd.Run()
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case output.err != nil:
		return output.err
	case err != nil:
		return fmt.Errorf("%w: %w", ErrDecode, err)
	case output.remaining == limit:
		return ErrDecode
	}
	return nil
}

type previewWriter struct {
	dst       io.Writer
	remaining int
	cancel    context.CancelFunc
	err       error
}

func (w *previewWriter) Write(data []byte) (int, error) {
	if len(data) > w.remaining {
		w.err = ErrTooLarge
		w.cancel()
		return 0, w.err
	}
	written, err := w.dst.Write(data)
	w.remaining -= written
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = err
		w.cancel()
	}
	return written, err
}
