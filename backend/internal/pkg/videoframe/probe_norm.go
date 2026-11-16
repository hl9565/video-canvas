package videoframe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
)

// NormalizedProbeVersion 是当前 24fps/48kHz 规范化媒体探测缓存的结构版本号。
const NormalizedProbeVersion int16 = 1

// 规范化 24fps/48kHz 探测与清单校验常量。
const (
	NormalizedFPSNum  = 24
	NormalizedFPSDen  = 1
	SamplesPerFrame24 = 2000
	minClipGainDB     = -60.0
	maxClipGainDB     = 12.0
)

// ErrInvalidManifest 表示清单帧区间、范围、增益或输出配置不合法。
var ErrInvalidManifest = errors.New("合成清单不合法")

// NormalizedProbe 记录素材归一化到 24fps 与 48kHz 立体声后的精确帧数与音视频起点。
type NormalizedProbe struct {
	FpsNum              int32
	FpsDen              int32
	NormalizedFrames24  int32
	HasAudio            bool
	AudioSampleRate     int32
	DecodedAudioSamples int64
	VStartUs            int64
	AStartUs            int64
}

// NativeAudioInput 描述计算片段原生音频裁切与对齐窗口的输入参数。
type NativeAudioInput struct {
	HasAudio            bool
	VStartUs            int64
	AStartUs            int64
	DecodedAudioSamples int64
	AvPtsOffsetMs       int32
	SourceInF           int32
	SourceOutF          int32
	MuteOriginal        bool
}

// NativeAudioWindow 给出按 48kHz 立体声采样精确对齐后的截取范围与首尾静音填充长度。
type NativeAudioWindow struct {
	UseSilence    bool
	OffsetSamples int64
	K0            int64
	K1            int64
	LeadPad       int64
	TailPad       int64
	TargetSamples int64
}

type byteCountWriter int64

func (w *byteCountWriter) Write(p []byte) (int, error) {
	*w += byteCountWriter(len(p))
	return len(p), nil
}

// ProbeNormalized24FPS 将本地视频按规范 24/1 CFR 与未补偿 48kHz 立体声解码，返回精确帧数、采样数与起点微秒。
func (e *Extractor) ProbeNormalized24FPS(ctx context.Context, source string) (NormalizedProbe, error) {
	if err := ctx.Err(); err != nil {
		return NormalizedProbe{}, err
	}
	info, err := e.composeProbe(ctx, source)
	if err != nil {
		return NormalizedProbe{}, err
	}
	frames24, err := e.countNormalized24Frames(ctx, source)
	if err != nil {
		return NormalizedProbe{}, err
	}
	out := NormalizedProbe{
		FpsNum:             int32(info.fps.num),
		FpsDen:             int32(info.fps.den),
		NormalizedFrames24: frames24,
		HasAudio:           info.audio,
		VStartUs:           int64(math.Round(info.vStart * 1e6)),
	}
	if !info.audio {
		return out, nil
	}
	out.AStartUs = int64(math.Round(info.aStart * 1e6))
	samples, err := e.countDecodedAudioSamples48k(ctx, source)
	if err != nil {
		return NormalizedProbe{}, err
	}
	out.DecodedAudioSamples = samples
	out.AudioSampleRate = composeAudioRate
	return out, nil
}

// ComputeNativeAudioWindow 计算单段源视频原生音轨在 48kHz 时间轴上的对齐裁切与首尾补零长度。
func ComputeNativeAudioWindow(in NativeAudioInput) NativeAudioWindow {
	targetSamples := max(int64(in.SourceOutF-in.SourceInF)*SamplesPerFrame24, 0)
	offsetSamples := int64(math.Round(float64(in.AStartUs-in.VStartUs)*float64(composeAudioRate)/1e6)) +
		int64(math.Round(float64(in.AvPtsOffsetMs)*48.0))
	if targetSamples == 0 || in.MuteOriginal || !in.HasAudio || in.DecodedAudioSamples <= 0 {
		return NativeAudioWindow{
			UseSilence:    true,
			OffsetSamples: offsetSamples,
			LeadPad:       targetSamples,
			TargetSamples: targetSamples,
		}
	}
	w0 := int64(in.SourceInF) * SamplesPerFrame24
	w1 := int64(in.SourceOutF) * SamplesPerFrame24
	k0 := max(0, w0-offsetSamples)
	k1 := min(in.DecodedAudioSamples, w1-offsetSamples)
	if k1 <= k0 {
		return NativeAudioWindow{
			UseSilence:    true,
			OffsetSamples: offsetSamples,
			LeadPad:       targetSamples,
			TargetSamples: targetSamples,
		}
	}
	leadPad := max(0, offsetSamples-w0)
	tailPad := max(targetSamples-leadPad-(k1-k0), 0)
	return NativeAudioWindow{
		OffsetSamples: offsetSamples,
		K0:            k0,
		K1:            k1,
		LeadPad:       leadPad,
		TailPad:       tailPad,
		TargetSamples: targetSamples,
	}
}

type frameCRCLineState uint8

const (
	frameCRCLead frameCRCLineState = iota
	frameCRCComment
	frameCRCPacket
)

// frameCRCCountWriter 流式统计 ffmpeg framecrc 输出的非空且非 '#' 注释行，
// 仅保留常数级行状态内存，避免长视频或畸形流导致整段标准输出/错误输出常驻内存。
type frameCRCCountWriter struct {
	count    int32
	state    frameCRCLineState
	overflow bool
}

func (w *frameCRCCountWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		if b == '\n' {
			w.state = frameCRCLead
			continue
		}
		switch w.state {
		case frameCRCLead:
			if b == ' ' || b == '\t' || b == '\r' || b == '\v' || b == '\f' {
				continue
			}
			if b == '#' {
				w.state = frameCRCComment
				continue
			}
			if w.count == math.MaxInt32 {
				w.overflow = true
				return 0, fmt.Errorf("%w: 视频帧数超出上限", ErrDecode)
			}
			w.count++
			w.state = frameCRCPacket
		case frameCRCComment, frameCRCPacket:
		}
	}
	return len(p), nil
}

func (w *frameCRCCountWriter) finish(err error) (int32, error) {
	if err != nil {
		if w.overflow && ctxActiveErr(err) {
			return 0, fmt.Errorf("%w: 视频帧数超出上限", ErrDecode)
		}
		return 0, err
	}
	if w.count <= 0 {
		return 0, fmt.Errorf("%w: 归一化 24fps 后没有有效视频帧", ErrDecode)
	}
	return w.count, nil
}

func ctxActiveErr(err error) bool {
	return !errors.Is(err, ErrTimeout) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func (e *Extractor) countNormalized24Frames(ctx context.Context, source string) (int32, error) {
	var counter frameCRCCountWriter
	err := e.runStreamStdout(ctx, &counter, e.ffmpeg,
		"-nostdin", "-v", "error", "-protocol_whitelist", "file", "-i", source,
		"-map", "0:v:0", "-an",
		"-vf", "setpts=PTS-STARTPTS,fps=24/1:start_time=0",
		"-f", "framecrc", "pipe:1",
	)
	return counter.finish(err)
}

func (e *Extractor) countDecodedAudioSamples48k(ctx context.Context, source string) (int64, error) {
	var written byteCountWriter
	err := e.runStreamStdout(ctx, &written, e.ffmpeg,
		"-nostdin", "-v", "error", "-protocol_whitelist", "file", "-i", source,
		"-map", "0:a:0", "-vn",
		"-af", fmt.Sprintf("asetpts=PTS-STARTPTS,aresample=%d,aformat=sample_fmts=s16:channel_layouts=stereo", composeAudioRate),
		"-f", "s16le", "pipe:1",
	)
	if err != nil {
		return 0, err
	}
	return int64(written) / 4, nil
}

func (e *Extractor) runStreamStdout(ctx context.Context, stdout io.Writer, bin string, args ...string) error {
	stderr := newBoundedStderr()
	return e.runCmd(ctx, stdout, stderr, stderr.decodeErr, bin, args...)
}
