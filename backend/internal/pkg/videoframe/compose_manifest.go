package videoframe

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// ManifestClip 描述 24fps 规范时间轴上的单个有序分镜片段及其原生音频参数。
type ManifestClip struct {
	SegmentID           uint64
	Seq                 int
	SourcePath          string
	SourceInF           int32
	SourceOutF          *int32
	NormalizedFrames24  int32
	HasAudio            bool
	VStartUs            int64
	AStartUs            int64
	DecodedAudioSamples int64
	MuteOriginal        bool
	AudioGainDB         float64
	NativeIsDialogue    bool
	AvPtsOffsetMs       int32
	FadeInF             int32
	FadeOutF            int32
}

// FrameRange 表示 24fps 母版时间轴上的半开区间 [InF, OutF)。
type FrameRange struct {
	InF  int32
	OutF int32
}

// ManifestRenderSpec 描述一次合成任务的一个输出文件及画幅/叠层选项。
type ManifestRenderSpec struct {
	Destination  string
	WithOverlays bool
	MaxEdge      int
	Width        int
	Height       int
}

// ComposeManifestInput 是 24fps/48kHz 帧精合成与片段预览的完整输入清单。
type ComposeManifestInput struct {
	Clips       []ManifestClip
	RangeF      *FrameRange
	AudioGainDB float64
	FadeInF     int32
	FadeOutF    int32
	Outputs     []ManifestRenderSpec
}

type resolvedClip struct {
	ManifestClip
	info       composeInfo
	outF       int32
	clipFrames int32
	masterInF  int32
	masterOutF int32
}

// ComposeManifest 按 24/1 CFR 与 48kHz 立体声契约执行逐镜精裁合成，支持 range_f 预览且与 edit_schema=0 路径完全隔离。
func (e *Extractor) ComposeManifest(ctx context.Context, input ComposeManifestInput) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateManifestBasic(input); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	dir, err := os.MkdirTemp(filepath.Dir(input.Outputs[0].Destination), "vc-manifest-*")
	if err != nil {
		return fmt.Errorf("创建清单合成临时目录失败：%w", err)
	}
	defer func() {
		_ = os.RemoveAll(dir)
		if err != nil {
			for _, out := range input.Outputs {
				_ = os.Remove(out.Destination)
			}
		}
	}()
	resolved, infos, totalFrames, err := e.resolveManifestClips(ctx, input.Clips)
	if err != nil {
		return err
	}
	rangeIn, rangeOut, err := resolveManifestRange(input.RangeF, totalFrames)
	if err != nil {
		return err
	}
	m4aPath := filepath.Join(dir, "a-master.m4a")
	if err := e.renderManifestAudioM4A(ctx, dir, resolved, totalFrames, rangeIn, rangeOut, input, m4aPath); err != nil {
		return err
	}
	baseTarget := chooseComposeTarget(infos)
	baseTarget.fps = rational{num: NormalizedFPSNum, den: NormalizedFPSDen}
	return e.renderAndMuxOutputs(ctx, dir, resolved, baseTarget, rangeIn, rangeOut, m4aPath, input.Outputs)
}

func validateManifestBasic(input ComposeManifestInput) error {
	if len(input.Clips) == 0 || len(input.Outputs) == 0 {
		return fmt.Errorf("%w: 清单片段与输出不能为空", ErrInvalidManifest)
	}
	if !validGainDB(input.AudioGainDB) || input.FadeInF < 0 || input.FadeOutF < 0 {
		return fmt.Errorf("%w: 集级音频增益或淡入淡出参数非法", ErrInvalidManifest)
	}
	for _, c := range input.Clips {
		if c.SourcePath == "" || !validGainDB(c.AudioGainDB) || c.FadeInF < 0 || c.FadeOutF < 0 {
			return fmt.Errorf("%w: 片段 %d 参数非法", ErrInvalidManifest, c.Seq)
		}
	}
	for _, out := range input.Outputs {
		if strings.TrimSpace(out.Destination) == "" || out.MaxEdge < 0 {
			return fmt.Errorf("%w: 输出目标或缩放上限非法", ErrInvalidManifest)
		}
	}
	return nil
}

func validGainDB(g float64) bool {
	return !math.IsNaN(g) && !math.IsInf(g, 0) && g >= minClipGainDB && g <= maxClipGainDB
}

func (e *Extractor) resolveManifestClips(ctx context.Context, clips []ManifestClip) ([]resolvedClip, []composeInfo, int32, error) {
	resolved := make([]resolvedClip, 0, len(clips))
	infos := make([]composeInfo, 0, len(clips))
	var cursor int32
	for _, clip := range clips {
		info, err := e.composeProbe(ctx, clip.SourcePath)
		if err != nil {
			return nil, nil, 0, err
		}
		if clip.NormalizedFrames24 <= 0 {
			probed, err := e.ProbeNormalized24FPS(ctx, clip.SourcePath)
			if err != nil {
				return nil, nil, 0, err
			}
			clip.NormalizedFrames24 = probed.NormalizedFrames24
			clip.HasAudio = probed.HasAudio
			clip.VStartUs = probed.VStartUs
			clip.AStartUs = probed.AStartUs
			clip.DecodedAudioSamples = probed.DecodedAudioSamples
		}
		outF := clip.NormalizedFrames24
		if clip.SourceOutF != nil {
			outF = *clip.SourceOutF
		}
		if clip.SourceInF < 0 || outF <= clip.SourceInF || outF > clip.NormalizedFrames24 {
			return nil, nil, 0, fmt.Errorf("%w: 片段 %d 帧区间 [%d,%d) 超出源帧数 %d",
				ErrInvalidManifest, clip.Seq, clip.SourceInF, outF, clip.NormalizedFrames24)
		}
		clipFrames := outF - clip.SourceInF
		resolved = append(resolved, resolvedClip{
			ManifestClip: clip,
			info:         info,
			outF:         outF,
			clipFrames:   clipFrames,
			masterInF:    cursor,
			masterOutF:   cursor + clipFrames,
		})
		infos = append(infos, info)
		cursor += clipFrames
	}
	return resolved, infos, cursor, nil
}

func resolveManifestRange(rf *FrameRange, totalFrames int32) (int32, int32, error) {
	if totalFrames <= 0 {
		return 0, 0, fmt.Errorf("%w: 有效帧数为 0", ErrInvalidManifest)
	}
	if rf == nil {
		return 0, totalFrames, nil
	}
	if rf.InF < 0 || rf.OutF <= rf.InF || rf.OutF > totalFrames {
		return 0, 0, fmt.Errorf("%w: 预览范围 [%d,%d) 超出总帧数 %d", ErrInvalidManifest, rf.InF, rf.OutF, totalFrames)
	}
	return rf.InF, rf.OutF, nil
}

func (e *Extractor) renderAndMuxOutputs(
	ctx context.Context,
	dir string,
	clips []resolvedClip,
	baseTarget composeTarget,
	rangeIn, rangeOut int32,
	m4aPath string,
	outputs []ManifestRenderSpec,
) error {
	vLists := make(map[int]string)
	for _, spec := range outputs {
		vListPath, ok := vLists[spec.MaxEdge]
		if !ok {
			var err error
			target := targetWithMaxEdge(baseTarget, spec.MaxEdge)
			if spec.Width > 0 && spec.Height > 0 {
				target.width = spec.Width
				target.height = spec.Height
				target.baseAR = float64(spec.Width) / float64(spec.Height)
			}
			vListPath, err = e.buildManifestVideoConcat(ctx, dir, spec.MaxEdge, clips, target, rangeIn, rangeOut)
			if err != nil {
				return err
			}
			vLists[spec.MaxEdge] = vListPath
		}
		if err := e.muxManifestOutput(ctx, vListPath, m4aPath, baseTarget.fps, spec.Destination); err != nil {
			return err
		}
	}
	return nil
}

func targetWithMaxEdge(base composeTarget, maxEdge int) composeTarget {
	if maxEdge <= 0 {
		return base
	}
	longest := max(base.width, base.height)
	if longest <= maxEdge {
		return base
	}
	scale := float64(maxEdge) / float64(longest)
	base.width = roundEven(int(math.Round(float64(base.width) * scale)))
	base.height = roundEven(int(math.Round(float64(base.height) * scale)))
	return base
}

func (e *Extractor) buildManifestVideoConcat(
	ctx context.Context,
	dir string,
	maxEdge int,
	clips []resolvedClip,
	target composeTarget,
	rangeIn, rangeOut int32,
) (string, error) {
	var vList strings.Builder
	for i, clip := range clips {
		if clip.masterOutF <= rangeIn || clip.masterInF >= rangeOut {
			continue
		}
		m0 := max(clip.masterInF, rangeIn)
		m1 := min(clip.masterOutF, rangeOut)
		subIn := clip.SourceInF + (m0 - clip.masterInF)
		subOut := clip.SourceInF + (m1 - clip.masterInF)
		vName := fmt.Sprintf("v-%d-%06d.mp4", maxEdge, i)
		vPath := filepath.Join(dir, vName)
		vf := buildManifestVideoFilter(target, clip.info, subIn, subOut)
		_, err := e.run(ctx, e.ffmpeg,
			"-nostdin", "-v", "error", "-protocol_whitelist", "file", "-i", clip.SourcePath,
			"-map", "0:v:0", "-an", "-vf", vf,
			"-c:v", "libx264", "-preset", "fast", "-crf", "18", "-pix_fmt", "yuv420p",
			"-video_track_timescale", composeTimescale(target.fps), "-y", vPath,
		)
		if err != nil {
			return "", err
		}
		gotFrames, err := e.countVideoFrames(ctx, vPath)
		if err != nil {
			return "", err
		}
		if want := int64(subOut - subIn); gotFrames != want {
			return "", fmt.Errorf("%w: 片段 %d 规范化输出 %d 帧 != 预期 %d 帧", ErrDecode, clip.Seq, gotFrames, want)
		}
		fmt.Fprintf(&vList, "file '%s'\n", vName)
	}
	listPath := filepath.Join(dir, fmt.Sprintf("vconcat-%d.txt", maxEdge))
	if err := os.WriteFile(listPath, []byte(vList.String()), 0600); err != nil {
		return "", fmt.Errorf("写入清单视频拼接列表失败：%w", err)
	}
	return listPath, nil
}

func buildManifestVideoFilter(target composeTarget, info composeInfo, trimIn, trimOut int32) string {
	cover := (info.height > info.width) == (target.height > target.width) && nearAspectRatio(info.dar, target.baseAR)
	scaledW, scaledH := planScaledDimensions(target, info.dar, cover)
	steps := []string{
		"setpts=PTS-STARTPTS",
		fmt.Sprintf("fps=%d/%d:start_time=0", NormalizedFPSNum, NormalizedFPSDen),
		fmt.Sprintf("trim=start_frame=%d:end_frame=%d", trimIn, trimOut),
		"setpts=PTS-STARTPTS",
		fmt.Sprintf("scale=%d:%d:flags=lanczos", scaledW, scaledH),
		"setsar=1",
	}
	if scaledW != target.width || scaledH != target.height {
		if cover {
			steps = append(steps, fmt.Sprintf("crop=%d:%d", target.width, target.height))
		} else {
			steps = append(steps, fmt.Sprintf("pad=%d:%d:(ow-iw)/2:(oh-ih)/2", target.width, target.height))
		}
	}
	steps = append(steps, "format=yuv420p")
	return strings.Join(steps, ",")
}

func (e *Extractor) muxManifestOutput(ctx context.Context, vListPath, m4aPath string, fps rational, destination string) error {
	_, err := e.run(ctx, e.ffmpeg,
		"-nostdin", "-v", "error",
		"-protocol_whitelist", "file", "-f", "concat", "-safe", "1", "-i", vListPath,
		"-protocol_whitelist", "file", "-i", m4aPath,
		"-map", "0:v:0", "-map", "1:a:0",
		"-c:v", "copy", "-video_track_timescale", composeTimescale(fps),
		"-c:a", "copy",
		"-movflags", "+faststart", "-y", destination,
	)
	return err
}
