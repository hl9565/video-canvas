package videoframe

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Compose 按给定顺序把多段视频合成为同一分辨率、统一恒定帧率、H.264/AAC 的完整成片。
// 保留原生 24/25fps，消除 AAC 拼接首尾静音与音画漂移；失败清理半成片，所有路径清理中间文件。
func (e *Extractor) Compose(ctx context.Context, sources []string, destination string) (err error) {
	if len(sources) == 0 {
		return fmt.Errorf("%w: 没有待合成视频", ErrDecode)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	dir, err := os.MkdirTemp(filepath.Dir(destination), "vc-concat-*")
	if err != nil {
		return fmt.Errorf("创建合成临时目录失败：%w", err)
	}
	defer func() {
		_ = os.RemoveAll(dir)
		if err != nil {
			_ = os.Remove(destination)
		}
	}()
	infos, err := e.probeComposeInputs(ctx, sources)
	if err != nil {
		return err
	}
	target := chooseComposeTarget(infos)
	vListPath, aListPath, err := e.normalizeComposeParts(ctx, dir, sources, infos, target)
	if err != nil {
		return err
	}
	return e.muxComposeParts(ctx, vListPath, aListPath, target.fps, destination)
}

func (e *Extractor) probeComposeInputs(ctx context.Context, sources []string) ([]composeInfo, error) {
	infos := make([]composeInfo, 0, len(sources))
	for _, source := range sources {
		info, err := e.composeProbe(ctx, source)
		if err != nil {
			return nil, err
		}
		infos = append(infos, info)
	}
	return infos, nil
}

func (e *Extractor) normalizeComposeParts(ctx context.Context, dir string, sources []string, infos []composeInfo, target composeTarget) (string, string, error) {
	var vList, aList strings.Builder
	var cumFrames, prevSamples int64
	for i, source := range sources {
		vName := fmt.Sprintf("v-%06d.mp4", i)
		aName := fmt.Sprintf("a-%06d.wav", i)
		vPath := filepath.Join(dir, vName)
		aPath := filepath.Join(dir, aName)
		if err := e.composeVideoPart(ctx, source, vPath, target, infos[i]); err != nil {
			return "", "", err
		}
		frames, err := e.countVideoFrames(ctx, vPath)
		if err != nil {
			return "", "", err
		}
		cumFrames += frames
		needSamples := cumulativeSamplesDelta(cumFrames, target.fps, &prevSamples)
		if err := e.composeAudioPart(ctx, source, aPath, needSamples, infos[i]); err != nil {
			return "", "", err
		}
		fmt.Fprintf(&vList, "file '%s'\n", vName)
		fmt.Fprintf(&aList, "file '%s'\n", aName)
	}
	vListPath := filepath.Join(dir, "vconcat.txt")
	if err := os.WriteFile(vListPath, []byte(vList.String()), 0600); err != nil {
		return "", "", fmt.Errorf("写入视频清单失败：%w", err)
	}
	aListPath := filepath.Join(dir, "aconcat.txt")
	if err := os.WriteFile(aListPath, []byte(aList.String()), 0600); err != nil {
		return "", "", fmt.Errorf("写入音频清单失败：%w", err)
	}
	return vListPath, aListPath, nil
}

// composeProbe 提取包含显示尺寸、SAR、旋转、帧率与音视频起始时间的标准元数据。
func (e *Extractor) composeProbe(ctx context.Context, source string) (composeInfo, error) {
	out, err := e.run(ctx, e.ffprobe, "-v", "error", "-protocol_whitelist", "file", "-show_format", "-show_streams", "-of", "json", source)
	if err != nil {
		return composeInfo{}, err
	}
	var probe probeOutput
	if err := json.Unmarshal(out, &probe); err != nil {
		return composeInfo{}, fmt.Errorf("%w: 视频信息无法解析", ErrDecode)
	}
	return parseComposeInfo(probe)
}

func (e *Extractor) composeVideoPart(ctx context.Context, source, destination string, target composeTarget, info composeInfo) error {
	vf := buildVideoFilter(target, info)
	args := []string{
		"-nostdin", "-v", "error", "-protocol_whitelist", "file", "-i", source,
		"-map", "0:v:0", "-an", "-vf", vf,
		"-c:v", "libx264", "-preset", "fast", "-crf", "18", "-pix_fmt", "yuv420p",
		"-video_track_timescale", composeTimescale(target.fps), "-y", destination,
	}
	_, err := e.run(ctx, e.ffmpeg, args...)
	return err
}

func (e *Extractor) countVideoFrames(ctx context.Context, videoPath string) (int64, error) {
	out, err := e.run(ctx, e.ffprobe, "-v", "error", "-protocol_whitelist", "file",
		"-select_streams", "v:0", "-count_frames", "-show_entries", "stream=nb_frames,nb_read_frames", "-of", "json", videoPath)
	if err != nil {
		return 0, err
	}
	var probe probeOutput
	if err := json.Unmarshal(out, &probe); err != nil || len(probe.Streams) == 0 {
		return 0, fmt.Errorf("%w: 无法统计归一化视频帧数", ErrDecode)
	}
	st := probe.Streams[0]
	for _, raw := range []string{st.NBReadFrames, st.NBFrames} {
		if n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil && n > 0 {
			return n, nil
		}
	}
	return 0, fmt.Errorf("%w: 归一化视频没有有效帧", ErrDecode)
}

func (e *Extractor) composeAudioPart(ctx context.Context, source, destination string, needSamples int64, info composeInfo) error {
	af, useSilence := buildAudioFilter(info, needSamples)
	var args []string
	if useSilence {
		args = []string{
			"-nostdin", "-v", "error", "-f", "lavfi", "-i",
			fmt.Sprintf("anullsrc=channel_layout=stereo:sample_rate=%d", composeAudioRate),
			"-af", af, "-c:a", "pcm_s16le", "-y", destination,
		}
	} else {
		args = []string{
			"-nostdin", "-v", "error", "-protocol_whitelist", "file", "-i", source,
			"-map", "0:a:0", "-vn", "-af", af, "-c:a", "pcm_s16le", "-y", destination,
		}
	}
	_, err := e.run(ctx, e.ffmpeg, args...)
	return err
}

func (e *Extractor) muxComposeParts(ctx context.Context, vListPath, aListPath string, fps rational, destination string) error {
	_, err := e.run(ctx, e.ffmpeg,
		"-nostdin", "-v", "error",
		"-protocol_whitelist", "file", "-f", "concat", "-safe", "1", "-i", vListPath,
		"-protocol_whitelist", "file", "-f", "concat", "-safe", "1", "-i", aListPath,
		"-map", "0:v:0", "-map", "1:a:0",
		"-c:v", "copy", "-video_track_timescale", composeTimescale(fps),
		"-c:a", "aac", "-b:a", "192k", "-ar", strconv.Itoa(composeAudioRate), "-ac", "2",
		"-movflags", "+faststart", "-y", destination)
	return err
}

func composeTimescale(fps rational) string {
	if fps.den > 1 && fps.num > 0 {
		return strconv.Itoa(fps.num)
	}
	return "90000"
}
