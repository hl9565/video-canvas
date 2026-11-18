package videoframe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"video-canvas/internal/model"
)

// buildProgramVisualConcat 渲染各片段，逐一校验规范帧数后生成无重叠拼接清单。
func (e *Extractor) buildProgramVisualConcat(ctx context.Context, dir string, spec ComposeProgramVisualInput, parts []programVisualPart) (string, error) {
	var list strings.Builder
	for i, p := range parts {
		name := fmt.Sprintf("visual-%06d.mp4", i)
		path := filepath.Join(dir, name)
		if err := e.renderProgramVisualPart(ctx, spec, p, path); err != nil {
			return "", fmt.Errorf("渲染画面片段 %d：%w", i+1, err)
		}
		if err := e.checkProgramVisualFrames(ctx, path, p.frames); err != nil {
			return "", fmt.Errorf("校验画面片段 %d：%w", i+1, err)
		}
		fmt.Fprintf(&list, "file '%s'\n", name)
	}
	path := filepath.Join(dir, "visual-concat.txt")
	if err := os.WriteFile(path, []byte(list.String()), 0600); err != nil {
		return "", fmt.Errorf("写入 v2 画面拼接清单失败：%w", err)
	}
	return path, nil
}

// checkProgramVisualFrames 防止源文件不完整或拼接后帧数漂移时发布成片。
func (e *Extractor) checkProgramVisualFrames(ctx context.Context, path string, expected int32) error {
	frames, err := e.countVideoFrames(ctx, path)
	if err != nil {
		return err
	}
	if frames != int64(expected) {
		return fmt.Errorf("%w: 输出 %d 帧，预期 %d 帧", ErrDecode, frames, expected)
	}
	return nil
}

// renderProgramVisualPart 用无损 H.264 保存画面母版，使整片和独立预览的解码像素完全一致。
func (e *Extractor) renderProgramVisualPart(ctx context.Context, spec ComposeProgramVisualInput, p programVisualPart, dest string) error {
	input, vf, err := e.programVisualSource(ctx, spec, p)
	if err != nil {
		return err
	}
	graph := "[0:v:0]" + vf + ",setsar=1,format=yuv420p,setparams=range=limited" + programVisualEffects(spec, p)
	args := append([]string{"-nostdin", "-v", "error", "-filter_complex_threads", "2"}, input...)
	args = append(args, "-filter_complex", graph, "-map", "[picture]", "-an",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "0", "-bf", "0", "-pix_fmt", "yuv420p", "-color_range", "tv",
		"-video_track_timescale", "24000", "-movie_timescale", "24000", "-y", dest)
	_, err = e.run(ctx, e.ffmpeg, args...)
	return err
}

// programVisualSource 建立三种画面源的 24fps 半开区间；静帧由解码器读取一张图像。
func (e *Extractor) programVisualSource(ctx context.Context, spec ComposeProgramVisualInput, p programVisualPart) ([]string, string, error) {
	w, h := spec.Width, spec.Height
	switch p.clip.VisualKind {
	case model.DramaVisualBlack:
		// lavfi 以微秒解析时长，取多一帧再用 trim 保证最后一帧不会因舍入丢失。
		source := fmt.Sprintf("color=c=black:s=%dx%d:r=24/1:d=%.6f", w, h, float64(p.frames+1)/24)
		return []string{"-f", "lavfi", "-i", source}, fmt.Sprintf("trim=end_frame=%d,setpts=PTS-STARTPTS", p.frames), nil
	case model.DramaVisualStill:
		return []string{"-protocol_whitelist", "file", "-i", p.clip.StillPath}, programStillFilter(w, h, p), nil
	default:
		info, err := e.composeProbe(ctx, p.clip.SourcePath)
		if err != nil {
			return nil, "", err
		}
		return []string{"-protocol_whitelist", "file", "-i", p.clip.SourcePath}, programVideoFilter(w, h, p, info), nil
	}
}

// programVideoFilter 使用显示宽高比计算 cover 尺寸；FFmpeg 默认先应用源旋转。
func programVideoFilter(w, h int, p programVisualPart, info composeInfo) string {
	in := p.clip.SourceInF + p.offset
	target := composeTarget{width: w, height: h, baseAR: float64(w) / float64(h)}
	sw, sh := planScaledDimensions(target, info.dar, true)
	return fmt.Sprintf("setpts=PTS-STARTPTS,fps=24/1:start_time=0,trim=start_frame=%d:end_frame=%d,setpts=PTS-STARTPTS,scale=%d:%d:flags=lanczos:out_range=tv,setsar=1,crop=%d:%d",
		in, in+p.frames, sw, sh, w, h)
}

// programStillFilter 对中性静帧直接做 cover；有运动时在高分辨率画布上计算取景窗口。
func programStillFilter(w, h int, p programVisualPart) string {
	z0, z1, shake, cropY, seed := programStillMotion(p.clip.FX, h)
	scale := 1
	if z0 != 1 || z1 != 1 || shake != 0 {
		scale = 2
	}
	cw, ch := w*scale, h*scale
	cover := fmt.Sprintf("trim=end_frame=1,scale=%d:%d:force_original_aspect_ratio=increase:force_divisible_by=2:flags=lanczos:out_range=tv,format=yuv444p,setsar=1,crop=%d:%d:(iw-ow)/2:(ih-oh)*%.8f",
		cw, ch, cw, ch, cropY)
	z, x, y := "1", "0", "0"
	if scale > 1 {
		z, x, y = programStillWindow(p, z0, z1, shake, cropY, seed, w, h)
	}
	return fmt.Sprintf("%s,zoompan=z='%s':x='%s':y='%s':d=%d:s=%dx%d:fps=24/1", cover, z, x, y, p.frames, w, h)
}

// programStillMotion 读取静帧取景默认值，抖动按 720p 等效像素缩放。
func programStillMotion(fx *model.DramaSegmentFX, h int) (z0, z1, shake, cropY float64, seed int32) {
	z0, z1, cropY = 1, 1, 0.5
	if fx == nil {
		return
	}
	if fx.ZoomStart != nil {
		z0 = *fx.ZoomStart
	}
	if fx.ZoomEnd != nil {
		z1 = *fx.ZoomEnd
	}
	if fx.ShakePx != nil {
		shake = *fx.ShakePx * float64(h) / 720
	}
	if fx.CropY != nil {
		cropY = *fx.CropY
	}
	if fx.Seed != nil {
		seed = *fx.Seed
	}
	return
}

// programStillWindow 预留抖动空间，并直接用分镜内帧号求取景，预览无需重放前缀帧。
func programStillWindow(p programVisualPart, z0, z1, shake, cropY float64, seed int32, w, h int) (z, x, y string) {
	frame := fmt.Sprintf("(on+%d)", p.offset)
	full := programVisualClipFrames(p.clip)
	z = fmt.Sprintf("(1+%.10f)*(%.8f+(%.8f)*%s/%d)", 2*shake/float64(h), z0, z1-z0, frame, max(1, full-1))
	// 两轴使用独立的确定性相位，不依赖 FFmpeg 表达式的随机状态。
	phaseX := float64((uint64(seed)*48271+1)%65521) / 65521 * 6.283185307
	phaseY := float64((uint64(seed)*69621+7)%65519) / 65519 * 6.283185307
	x = fmt.Sprintf("(iw-iw/zoom)/2+(iw/zoom/%d)*%.10f*sin(%s*1.71+%.10f)", w, shake, frame, phaseX)
	y = fmt.Sprintf("(ih-ih/zoom)*%.8f+(ih/zoom/%d)*%.10f*(1-2*%.8f+sin(%s*2.39+%.10f))",
		cropY, h, shake, cropY, frame, phaseY)
	return z, x, y
}

// programVisualEffects 独立处理每段的白闪和黑淡变，白闪最多混入 78%。
func programVisualEffects(spec ComposeProgramVisualInput, p programVisualPart) string {
	fx := p.clip.FX
	if fx == nil || fx.FadeInF == 0 && fx.FadeOutF == 0 && fx.FlashInF == 0 && fx.FlashOutF == 0 {
		return "[picture]"
	}
	frame := fmt.Sprintf("(N+%d)", p.offset)
	last := programVisualClipFrames(p.clip) - 1
	var graph strings.Builder
	graph.WriteString("[base];")
	current := "base"
	if fx.FlashInF > 0 || fx.FlashOutF > 0 {
		flash := programVisualRamp(frame, last, fx.FlashInF, fx.FlashOutF)
		graph.WriteString(programVisualBlend(spec, flash, 255*0.78, "white", current, 235))
		current = "whiteMix"
	}
	if fx.FadeInF > 0 || fx.FadeOutF > 0 {
		fade := programVisualRamp(frame, last, fx.FadeInF, fx.FadeOutF)
		graph.WriteString(programVisualBlend(spec, fade, 255, "black", current, 16))
		current = "blackMix"
	}
	fmt.Fprintf(&graph, "[%s]setparams=range=limited[picture]", current)
	return graph.String()
}

// programVisualBlend 在 2×2 遮罩上求每帧的权重，再用 FFmpeg 的逐像素混合扩展到画幅。
func programVisualBlend(spec ComposeProgramVisualInput, ramp string, maxAlpha float64, label, input string, luma int) string {
	// 向下取整保证闪白即使经过 8 位权重离散化也不超过 78%。
	alpha := fmt.Sprintf("floor(%.4f*(%s))", maxAlpha, ramp)
	size := fmt.Sprintf("%dx%d", spec.Width, spec.Height)
	// 遮罩和纯色都从基底继承时间戳和 EOF，不能重复末帧来掩盖源视频截断。
	return fmt.Sprintf(
		"[%[1]s]split[%[2]sBase][%[2]sSmall];[%[2]sSmall]scale=2:2:flags=neighbor,split[%[2]sAlpha][%[2]sFill];"+
			"[%[2]sAlpha]geq=lum='%[3]s':cb='%[3]s':cr='%[3]s',scale=%[4]s:flags=neighbor:in_range=full:out_range=full,setparams=range=limited[%[2]sMask];"+
			"[%[2]sFill]lutyuv=y=%[5]d:u=128:v=128,scale=%[4]s:flags=neighbor[%[2]sColor];"+
			"[%[2]sBase][%[2]sColor][%[2]sMask]maskedmerge[%[2]sMix];",
		input, label, alpha, size, luma)
}

// programVisualRamp 返回相对分镜首尾的线性包络，区间外权重为 0。
func programVisualRamp(frame string, last, in, out int32) string {
	a, b := "0", "0"
	if in > 0 {
		a = fmt.Sprintf("max(0,1-%s/%d)", frame, in)
	}
	if out > 0 {
		b = fmt.Sprintf("max(0,1-(%d-%s)/%d)", last, frame, out)
	}
	return fmt.Sprintf("max(%s,%s)", a, b)
}
