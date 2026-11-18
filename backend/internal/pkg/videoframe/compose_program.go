package videoframe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"video-canvas/internal/model"
)

// ProgramVisualClip 描述 v2 时间轴上一个视频、静帧或黑场的半开帧区间。
type ProgramVisualClip struct {
	VisualKind         string
	SourcePath         string
	StillPath          string
	SourceInF          int32
	SourceOutF         int32
	NormalizedFrames24 int32
	FX                 *model.DramaSegmentFX
}

// ComposeProgramVisualInput 描述 24fps 无声画面母版及其可选范围预览。
type ComposeProgramVisualInput struct {
	Clips       []ProgramVisualClip
	Width       int
	Height      int
	RangeF      *FrameRange
	Destination string
}

type programVisualPart struct {
	clip           ProgramVisualClip
	offset, frames int32 // 已裁剪的分镜内区间，与源视频帧号分开
}

// ComposeProgramVisual 渲染 v2 画面母版，供预览或与独立音频母带封装。
func (e *Extractor) ComposeProgramVisual(ctx context.Context, input ComposeProgramVisualInput) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	parts, frames, err := planProgramVisual(input)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	dir, err := os.MkdirTemp(filepath.Dir(input.Destination), "vc-program-visual-*")
	if err != nil {
		return fmt.Errorf("创建画面合成临时目录失败：%w", err)
	}
	defer os.RemoveAll(dir)
	listPath, err := e.buildProgramVisualConcat(ctx, dir, input, parts)
	if err != nil {
		return err
	}
	// 先在同一文件系统生成并检查帧数，失败时保留已有目标文件。
	output := filepath.Join(dir, "visual.mp4")
	_, err = e.run(ctx, e.ffmpeg, "-nostdin", "-v", "error", "-protocol_whitelist", "file",
		"-f", "concat", "-safe", "1", "-i", listPath, "-map", "0:v:0", "-c:v", "copy",
		"-video_track_timescale", "24000", "-movie_timescale", "24000", "-movflags", "+faststart", "-y", output)
	if err != nil {
		return err
	}
	if err := e.checkProgramVisualFrames(ctx, output, frames); err != nil {
		return err
	}
	return os.Rename(output, input.Destination)
}

// planProgramVisual 校验完整清单并把母版范围转换成分镜内偏移。
func planProgramVisual(input ComposeProgramVisualInput) ([]programVisualPart, int32, error) {
	if input.Width != 1280 || input.Height != 720 {
		if input.Width != 1920 || input.Height != 1080 {
			return nil, 0, fmt.Errorf("%w: v2 画面仅支持 1280x720 或 1920x1080", ErrInvalidManifest)
		}
	}
	if strings.TrimSpace(input.Destination) == "" {
		return nil, 0, fmt.Errorf("%w: 输出目标不能为空", ErrInvalidManifest)
	}
	var total int32
	for i, c := range input.Clips {
		frames, err := validateProgramVisualClip(c)
		if err != nil {
			return nil, 0, fmt.Errorf("分镜 %d：%w", i+1, err)
		}
		if frames > model.DramaProgramMaxFrames-total {
			return nil, 0, fmt.Errorf("%w: 总帧数超过 14400 帧", ErrInvalidManifest)
		}
		total += frames
	}
	start, end, err := resolveManifestRange(input.RangeF, total)
	if err != nil {
		return nil, 0, err
	}
	return sliceProgramVisual(input.Clips, start, end), end - start, nil
}

// validateProgramVisualClip 要求本地画面素材和已冻结的源视频帧数。
func validateProgramVisualClip(c ProgramVisualClip) (int32, error) {
	frames := programVisualClipFrames(c)
	if c.SourceInF < 0 || frames <= 0 {
		return 0, fmt.Errorf("%w: 画面源帧区间无效", ErrInvalidManifest)
	}
	switch c.VisualKind {
	case model.DramaVisualVideo:
		if c.SourcePath == "" || c.SourceInF+frames > c.NormalizedFrames24 {
			return 0, fmt.Errorf("%w: 视频源路径或帧区间无效", ErrInvalidManifest)
		}
	case model.DramaVisualStill:
		if c.StillPath == "" || c.SourceInF != 0 {
			return 0, fmt.Errorf("%w: 静帧源路径或起始帧无效", ErrInvalidManifest)
		}
	case model.DramaVisualBlack:
		if c.SourceInF != 0 {
			return 0, fmt.Errorf("%w: 黑场起始帧必须为 0", ErrInvalidManifest)
		}
	default:
		return 0, fmt.Errorf("%w: 不支持的视觉模式", ErrInvalidManifest)
	}
	if c.FX != nil {
		if err := c.FX.ValidateForClip(c.VisualKind, frames); err != nil {
			return 0, fmt.Errorf("%w: %w", ErrInvalidManifest, err)
		}
	}
	return frames, nil
}

// programVisualClipFrames 返回范围预览截取前的片段帧数。
func programVisualClipFrames(c ProgramVisualClip) int32 {
	out := c.SourceOutF
	if out == 0 && c.VisualKind == model.DramaVisualVideo {
		out = c.NormalizedFrames24
	}
	if out <= c.SourceInF || c.SourceInF < 0 {
		return 0
	}
	return out - c.SourceInF
}

// sliceProgramVisual 按母版范围裁切，保留每段完整长度供特效使用。
func sliceProgramVisual(clips []ProgramVisualClip, start, end int32) []programVisualPart {
	parts := make([]programVisualPart, 0, len(clips))
	var cursor int32
	for _, c := range clips {
		next := cursor + programVisualClipFrames(c)
		a, b := max(start, cursor), min(end, next)
		if b > a {
			parts = append(parts, programVisualPart{clip: c, offset: a - cursor, frames: b - a})
		}
		cursor = next
	}
	return parts
}

// ProgramClip 记录画面片段与同一片段的原声音轨信息。
type ProgramClip struct {
	ProgramVisualClip
	HasAudio            bool
	DecodedAudioSamples int64
	VStartUs            int64
	AStartUs            int64
	MuteOriginal        bool
	AudioGainDB         float64
	NativeIsDialogue    bool
	AvPtsOffsetMs       int32
}

// ComposeProgramInput 描述冻结的 v2 画面、独立音轨及输出；预览只在母带完成后裁音频。
type ComposeProgramInput struct {
	Clips   []ProgramClip
	Audio   []ProgramAudioCueSpec
	RangeF  *FrameRange
	Outputs []ComposeProgramOutputSpec
}

// ComposeProgramOutputSpec 指定一个画面尺寸及变体；当前所有变体均不绘制叠层。
type ComposeProgramOutputSpec struct {
	Destination   string
	WithOverlays  bool
	Width, Height int
	Variant       string
}

// ProgramOutputReport 记录单个已验收输出的尺寸、变体与 AAC 指纹。
type ProgramOutputReport struct {
	ComposeProgramOutputSpec
	AudioSHA256 string
}

// ProgramComposeReport 记录渲染范围的帧数与封装后音频的验收结果。
type ProgramComposeReport struct {
	TotalFrames    int32
	AudioSHA256    string
	LUFS, TruePeak *float64
	LoudnessStatus string
	Outputs        []ProgramOutputReport
}

// ComposeProgramManifest 先渲染可复用的无声画面，再以一个共享 AAC 母带封装并验证各输出。
func (e *Extractor) ComposeProgramManifest(ctx context.Context, input ComposeProgramInput) (ProgramComposeReport, error) {
	total, err := programTotalFrames(input.Clips)
	if err != nil {
		return ProgramComposeReport{}, err
	}
	in, out, err := resolveManifestRange(input.RangeF, total)
	if err != nil {
		return ProgramComposeReport{}, err
	}
	if err := validateProgramOutputs(input.Outputs); err != nil {
		return ProgramComposeReport{}, err
	}
	stems, err := planProgramStems(input, total)
	if err != nil {
		return ProgramComposeReport{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	dir, err := os.MkdirTemp(filepath.Dir(input.Outputs[0].Destination), "vc-program-*")
	if err != nil {
		return ProgramComposeReport{}, err
	}
	defer os.RemoveAll(dir)

	render := programAudioMastering{
		dir: dir, total: int64(total) * SamplesPerFrame24, partial: input.RangeF != nil,
		sampleInterval: sampleInterval{int64(in) * SamplesPerFrame24, int64(out) * SamplesPerFrame24},
	}
	render.outputs, err = e.prepareProgramPictures(ctx, dir, input)
	defer removeProgramStagedOutputs(render.outputs)
	if err != nil {
		return ProgramComposeReport{}, err
	}
	peak, err := e.renderProgramStems(ctx, stems, render.total, render.rawPath())
	if err != nil {
		return ProgramComposeReport{}, err
	}
	report, err := e.masterProgramAudio(ctx, render, peak == 0)
	if err != nil {
		return ProgramComposeReport{}, err
	}
	if err := ctx.Err(); err != nil {
		return ProgramComposeReport{}, err
	}
	report.TotalFrames = out - in
	for _, output := range render.outputs {
		if err := os.Rename(output.staged, output.spec.Destination); err != nil {
			return ProgramComposeReport{}, err
		}
	}
	return report, nil
}

func validateProgramOutputs(outputs []ComposeProgramOutputSpec) error {
	if len(outputs) == 0 {
		return fmt.Errorf("%w: 至少需要一个节目单输出", ErrInvalidManifest)
	}
	seen := make(map[string]bool, len(outputs))
	for _, output := range outputs {
		path, err := filepath.Abs(output.Destination)
		if err != nil || strings.TrimSpace(output.Destination) == "" || seen[path] {
			return fmt.Errorf("%w: 输出文件路径为空或重复", ErrInvalidManifest)
		}
		seen[path] = true
	}
	return nil
}

type programStagedOutput struct {
	spec            ComposeProgramOutputSpec
	picture, staged string
}

// prepareProgramPictures 对每组尺寸只编码一次；叠层在 B0 中始终为空。
func (e *Extractor) prepareProgramPictures(ctx context.Context, dir string, input ComposeProgramInput) ([]programStagedOutput, error) {
	clips := make([]ProgramVisualClip, 0, len(input.Clips))
	for _, c := range input.Clips {
		clips = append(clips, c.ProgramVisualClip)
	}
	pictures := make(map[[2]int]string)
	outputs := make([]programStagedOutput, 0, len(input.Outputs))
	for _, spec := range input.Outputs {
		size := [2]int{spec.Width, spec.Height}
		picture := pictures[size]
		if picture == "" {
			picture = filepath.Join(dir, fmt.Sprintf("visual-%dx%d.mp4", spec.Width, spec.Height))
			visual := ComposeProgramVisualInput{Clips: clips, Width: spec.Width, Height: spec.Height, RangeF: input.RangeF, Destination: picture}
			if err := e.ComposeProgramVisual(ctx, visual); err != nil {
				return outputs, err
			}
			pictures[size] = picture
		}
		// 暂存文件与目标同目录，验收后可直接重命名。
		tmp, err := os.CreateTemp(filepath.Dir(spec.Destination), "vc-program-output-*.mp4")
		if err != nil {
			return outputs, err
		}
		outputs = append(outputs, programStagedOutput{spec: spec, picture: picture, staged: tmp.Name()})
		if err := tmp.Close(); err != nil {
			return outputs, err
		}
	}
	return outputs, nil
}

func removeProgramStagedOutputs(outputs []programStagedOutput) {
	for _, out := range outputs {
		_ = os.Remove(out.staged)
	}
}
