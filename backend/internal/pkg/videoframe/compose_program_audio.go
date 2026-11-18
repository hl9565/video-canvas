package videoframe

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"

	"video-canvas/internal/model"
)

// ProgramAudioCueSpec 是解码后素材采样区间和剧集帧区间之间的映射。
type ProgramAudioCueSpec struct {
	ID                  string
	Role                model.DramaProgramAudioRole
	SourcePath          string
	DecodedAudioSamples int64
	StartF, EndF        int32
	SrcInSample         int64
	SrcOutSample        *int64
	GainDB              float64
	DuckDB              *float64
	AvPtsOffsetMs       int32
	FadeInF, FadeOutF   int32
	Loop                bool
}

type programBus int

const (
	programVoice programBus = iota
	programProduction
	programBed
	programSFX
	programBusCount
)

type sampleInterval struct{ in, out int64 }

type programStem struct {
	active, envelope sampleInterval
	source           string
	sourceIn         int64 // 非循环为起始读取位置，循环为循环区间起点
	period, phase    int64 // 循环长度与首个播放采样在循环中的相位
	bus              programBus
	gain             float64
	fadeIn, fadeOut  int32
}

type programPCM struct {
	path string
	sampleInterval
}

// MixProgramAudio 写入整集 48kHz 双声道 f32le，并返回实际写入的最大绝对振幅。
// RangeF 只在后续编码时生效，保证混音和闪避总是从剧集零点开始。
func (e *Extractor) MixProgramAudio(ctx context.Context, input ComposeProgramInput, destination string) (float64, error) {
	total, err := programTotalFrames(input.Clips)
	if err != nil {
		return 0, err
	}
	stems, err := planProgramStems(input, total)
	if err != nil {
		return 0, err
	}
	return e.renderProgramStems(ctx, stems, int64(total)*SamplesPerFrame24, destination)
}

// programTotalFrames 核验画面母版在混音与输出层共享的时长。
func programTotalFrames(clips []ProgramClip) (int32, error) {
	var total int32
	for _, clip := range clips {
		n := programVisualClipFrames(clip.ProgramVisualClip)
		if n <= 0 || n > model.DramaProgramMaxFrames-total {
			return 0, fmt.Errorf("%w: 片段帧数无效或剧集超过 14400 帧", ErrInvalidManifest)
		}
		total += n
	}
	if total == 0 || len(clips) > model.DramaProgramMaxSegments {
		return 0, fmt.Errorf("%w: 片段数量无效", ErrInvalidManifest)
	}
	return total, nil
}

// planProgramStems 先验证整集位置，任何无效条目都不产生部分混音。
func planProgramStems(input ComposeProgramInput, total int32) ([]programStem, error) {
	if len(input.Audio) > model.DramaProgramMaxAudioCues {
		return nil, fmt.Errorf("%w: 独立音频条目超过上限", ErrInvalidManifest)
	}
	var stems []programStem
	var cursor int64
	for _, clip := range input.Clips {
		frames := programVisualClipFrames(clip.ProgramVisualClip)
		stem, err := planProgramNative(clip, cursor, frames)
		if err != nil {
			return nil, err
		}
		if stem.active.out > stem.active.in {
			stems = append(stems, stem)
		}
		cursor += int64(frames) * SamplesPerFrame24
	}
	for _, cue := range input.Audio {
		stem, err := planProgramCue(cue, total)
		if err != nil {
			return nil, err
		}
		if stem.active.out > stem.active.in {
			stems = append(stems, stem)
		}
	}
	return stems, nil
}

// planProgramCue 只裁剪放置窗口；源起点随负延迟前进，正延迟保留首部空白。
func planProgramCue(c ProgramAudioCueSpec, total int32) (programStem, error) {
	bus, valid := programCueBus(c.Role)
	bedRole := bus == programBed
	if !valid || (c.Loop && !bedRole) || (c.DuckDB != nil && (!bedRole || *c.DuckDB != -8)) ||
		c.SourcePath == "" || c.StartF < 0 || c.EndF <= c.StartF || c.EndF > total ||
		c.AvPtsOffsetMs < -10000 || c.AvPtsOffsetMs > 10000 || !validGainDB(c.GainDB) || c.FadeInF < 0 || c.FadeOutF < 0 ||
		c.DecodedAudioSamples > math.MaxInt64/programStereoFrameBytes {
		return programStem{}, fmt.Errorf("%w: 音频条目 %q 参数无效", ErrInvalidManifest, c.ID)
	}
	// 背景音乐与环境声仍可循环，但只有显式设置 duck_db 才随对白闪避。
	if bedRole && c.DuckDB == nil {
		bus = programProduction
	}
	y := c.DecodedAudioSamples
	if c.SrcOutSample != nil {
		y = *c.SrcOutSample
	}
	if c.SrcInSample < 0 || y <= c.SrcInSample || y > c.DecodedAudioSamples {
		return programStem{}, fmt.Errorf("%w: 音频条目 %q 源采样区间越界", ErrInvalidManifest, c.ID)
	}
	a, b := int64(c.StartF)*SamplesPerFrame24, int64(c.EndF)*SamplesPerFrame24
	start := a + int64(c.AvPtsOffsetMs)*48
	length := y - c.SrcInSample
	end := start + length
	if bus == programVoice && end > b {
		return programStem{}, fmt.Errorf("%w: 音频条目 %q：旁白窗口不足，差 %d 个采样，请延长窗口或显式裁剪 src_out_sample", ErrInvalidManifest, c.ID, end-b)
	}
	lo, hi := max(a, start), min(b, end)
	stem := programStem{envelope: sampleInterval{a, b}, source: c.SourcePath, sourceIn: c.SrcInSample,
		bus: bus, gain: math.Pow(10, c.GainDB/20), fadeIn: c.FadeInF, fadeOut: c.FadeOutF}
	if c.Loop {
		hi = b
		stem.period, stem.phase = length, (lo-start)%length
	} else {
		stem.sourceIn += lo - start
	}
	stem.active = sampleInterval{lo, hi}
	return stem, nil
}

func programCueBus(role model.DramaProgramAudioRole) (programBus, bool) {
	switch role {
	case model.DramaAudioNarration:
		return programVoice, true
	case model.DramaAudioSFX:
		return programSFX, true
	case model.DramaAudioBGM, model.DramaAudioRoomTone:
		return programBed, true
	default:
		return 0, false
	}
}

func validProgramFades(in, out, frames int32) bool {
	return in >= 0 && out >= 0 && int64(in)+int64(out) <= int64(frames)
}

// planProgramNative 在源时间基上算一次偏移，再把有效 PCM 区间放到剧集时间轴。
func planProgramNative(c ProgramClip, start int64, frames int32) (programStem, error) {
	if c.VisualKind != model.DramaVisualVideo {
		return programStem{}, nil
	}
	win := ComputeNativeAudioWindow(NativeAudioInput{
		HasAudio: c.HasAudio, VStartUs: c.VStartUs, AStartUs: c.AStartUs, DecodedAudioSamples: c.DecodedAudioSamples,
		AvPtsOffsetMs: c.AvPtsOffsetMs, SourceInF: c.SourceInF, SourceOutF: c.SourceInF + frames, MuteOriginal: c.MuteOriginal,
	})
	if win.UseSilence {
		return programStem{}, nil
	}
	var in, out int32
	if c.FX != nil {
		in, out = c.FX.NativeFadeInF, c.FX.NativeFadeOutF
	}
	if c.SourcePath == "" || !validGainDB(c.AudioGainDB) || !validProgramFades(in, out, frames) || win.K1 > math.MaxInt64/programStereoFrameBytes {
		return programStem{}, fmt.Errorf("%w: 原声音轨参数无效", ErrInvalidManifest)
	}
	bus := programProduction
	if c.NativeIsDialogue {
		bus = programVoice
	}
	lo := start + win.LeadPad
	return programStem{
		active: sampleInterval{lo, lo + win.K1 - win.K0}, envelope: sampleInterval{start, start + win.TargetSamples},
		source: c.SourcePath, sourceIn: win.K0, bus: bus, gain: math.Pow(10, c.AudioGainDB/20), fadeIn: in, fadeOut: out,
	}, nil
}

func (e *Extractor) renderProgramStems(ctx context.Context, stems []programStem, total int64, destination string) (peak float64, err error) {
	dir, err := os.MkdirTemp(filepath.Dir(destination), "vc-program-pcm-*")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)
	inputs, err := e.decodeProgramStems(ctx, dir, stems)
	if err != nil {
		return 0, err
	}
	file, err := os.Create(destination) //nolint:gosec // 目标由调用方指定，生产路径位于私有渲染目录。
	if err != nil {
		return 0, err
	}
	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}()
	return streamProgramPCM(ctx, file, total, stems, inputs)
}

// decodeProgramStems 复用同一素材的解码，临时文件只保留混音会读取的源采样范围。
func (e *Extractor) decodeProgramStems(ctx context.Context, dir string, stems []programStem) (map[string]programPCM, error) {
	inputs := make(map[string]programPCM)
	sources := make([]string, 0, len(stems))
	for _, stem := range stems {
		span := stem.sourceBounds()
		if old, ok := inputs[stem.source]; ok {
			span = sampleInterval{min(span.in, old.in), max(span.out, old.out)}
		} else {
			sources = append(sources, stem.source)
		}
		inputs[stem.source] = programPCM{sampleInterval: span}
	}
	for i, source := range sources {
		pcm := inputs[source]
		pcm.path = filepath.Join(dir, fmt.Sprintf("source-%d.f32", i))
		if err := e.decodeProgramPCM(ctx, source, pcm); err != nil {
			return nil, err
		}
		inputs[source] = pcm
	}
	return inputs, nil
}

func (s programStem) sourceBounds() sampleInterval {
	length := s.active.out - s.active.in
	if s.period > 0 && length > s.period-s.phase {
		return sampleInterval{s.sourceIn, s.sourceIn + s.period}
	}
	start := s.sourceIn + s.phase
	return sampleInterval{start, start + length}
}

func (e *Extractor) decodeProgramPCM(ctx context.Context, source string, pcm programPCM) (err error) {
	f, err := os.Create(pcm.path)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}()
	trim := fmt.Sprintf(",atrim=start_sample=%d:end_sample=%d", pcm.in, pcm.out)
	if err := e.runStreamStdout(ctx, f, e.ffmpeg, decodeProgramAudioArgs(source, trim)...); err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() != (pcm.out-pcm.in)*programStereoFrameBytes {
		return fmt.Errorf("%w: 实际音频采样数与冻结探针不符", ErrDecode)
	}
	return nil
}

type programDucking struct {
	voice                         []sampleInterval
	index                         int
	state, attack, release, depth float64
}

// newProgramDucking 合并重叠及紧邻的人声窗口，只生成一条控制包络。
func newProgramDucking(stems []programStem) programDucking {
	var voice []sampleInterval
	for _, s := range stems {
		if s.bus == programVoice {
			voice = append(voice, s.active)
		}
	}
	slices.SortFunc(voice, func(a, b sampleInterval) int {
		switch {
		case a.in < b.in:
			return -1
		case a.in > b.in:
			return 1
		default:
			return 0
		}
	})
	union := voice[:0]
	for _, v := range voice {
		if n := len(union); n > 0 && v.in <= union[n-1].out {
			union[n-1].out = max(union[n-1].out, v.out)
		} else {
			union = append(union, v)
		}
	}
	return programDucking{voice: union, attack: math.Exp(-1.0 / 3840), release: math.Exp(-1.0 / 13440), depth: 1 - math.Pow(10, -8.0/20)}
}

func (d *programDucking) gainAt(sample int64) float64 {
	if d.index < len(d.voice) && sample >= d.voice[d.index].out {
		d.index++
	}
	if d.index < len(d.voice) && sample >= d.voice[d.index].in {
		d.state = d.attack*d.state + (1 - d.attack)
	} else {
		d.state *= d.release
	}
	return 1 - d.state*d.depth
}

type programMixChunk struct {
	bus [programBusCount][]float64
	raw []byte
}

func streamProgramPCM(ctx context.Context, w io.Writer, total int64, stems []programStem, sources map[string]programPCM) (float64, error) {
	var chunk programMixChunk
	for i := range chunk.bus {
		chunk.bus[i] = make([]float64, audioMixChunkSamples*2)
	}
	chunk.raw = make([]byte, audioMixChunkSamples*programStereoFrameBytes)
	d := newProgramDucking(stems)
	peak := 0.0
	for start := int64(0); start < total; {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		n := min(total-start, audioMixChunkSamples)
		if err := chunk.fill(stems, sources, start, n); err != nil {
			return 0, err
		}
		chunkPeak, err := chunk.write(w, &d, start, n)
		if err != nil {
			return 0, err
		}
		peak = max(peak, chunkPeak)
		start += n
	}
	return peak, nil
}

func (c *programMixChunk) fill(stems []programStem, sources map[string]programPCM, start, n int64) error {
	for _, bus := range c.bus {
		clear(bus[:n*2])
	}
	for _, stem := range stems {
		lo, hi := max(start, stem.active.in), min(start+n, stem.active.out)
		if hi <= lo {
			continue
		}
		if err := c.addStem(stem, sources[stem.source], start, lo, hi); err != nil {
			return err
		}
	}
	return nil
}

func (c *programMixChunk) addStem(s programStem, pcm programPCM, start, lo, hi int64) error {
	f, err := os.Open(pcm.path)
	if err != nil {
		return err
	}
	defer f.Close()
	raw := c.raw[:(hi-lo)*programStereoFrameBytes]
	if err := readProgramStemChunk(f, s, pcm, lo-s.active.in, raw); err != nil {
		return err
	}
	c.addSamples(s, raw, start, lo, hi-lo)
	return nil
}

// readProgramStemChunk 每块至多读取两段连续 PCM；短循环在内存中重复，避免逐采样读盘。
func readProgramStemChunk(f *os.File, s programStem, pcm programPCM, delta int64, raw []byte) error {
	base := (s.sourceIn - pcm.in) * programStereoFrameBytes
	if s.period == 0 {
		_, err := f.ReadAt(raw, base+delta*programStereoFrameBytes)
		return err
	}
	phase := (delta + s.phase) % s.period
	cycle := min(int64(len(raw)), s.period*programStereoFrameBytes)
	first := min(cycle, (s.period-phase)*programStereoFrameBytes)
	if _, err := f.ReadAt(raw[:first], base+phase*programStereoFrameBytes); err != nil {
		return err
	}
	if first < cycle {
		if _, err := f.ReadAt(raw[first:cycle], base); err != nil {
			return err
		}
	}
	for filled := int(cycle); filled < len(raw); {
		filled += copy(raw[filled:], raw[:filled])
	}
	return nil
}

func (c *programMixChunk) addSamples(s programStem, raw []byte, start, pos, n int64) {
	dst := c.bus[s.bus][(pos-start)*2:]
	for frame := range n {
		gain := s.gain * fadeEnvelopeFactor(pos+frame-s.envelope.in, s.envelope.out-s.envelope.in, s.fadeIn, s.fadeOut)
		for ch := range int64(2) {
			i := frame*2 + ch
			v := math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
			dst[i] += gain * float64(v)
		}
	}
}

func (c *programMixChunk) write(w io.Writer, d *programDucking, start, n int64) (float64, error) {
	peak := 0.0
	for frame := range n {
		duck := d.gainAt(start + frame)
		for ch := range int64(2) {
			i := frame*2 + ch
			v := float32(c.bus[programVoice][i] + c.bus[programProduction][i] + c.bus[programBed][i]*duck + c.bus[programSFX][i])
			peak = max(peak, math.Abs(float64(v)))
			binary.LittleEndian.PutUint32(c.raw[i*4:], math.Float32bits(v))
		}
	}
	if math.IsNaN(peak) || math.IsInf(peak, 0) {
		return 0, fmt.Errorf("%w: 混音含有非有限采样", ErrDecode)
	}
	_, err := w.Write(c.raw[:n*programStereoFrameBytes])
	return peak, err
}
