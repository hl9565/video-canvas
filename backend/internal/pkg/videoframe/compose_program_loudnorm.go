package videoframe

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	programLoudnormTail    = 8192
	programAACFrameSamples = 1024
	programLoudnormBase    = "loudnorm=I=-16:LRA=11"
)

type programAudioMastering struct {
	dir     string
	total   int64
	partial bool
	sampleInterval
	outputs []programStagedOutput
}

func (p programAudioMastering) rawPath() string { return filepath.Join(p.dir, "program-raw.f32") }

type programLoudness struct {
	I         string `json:"input_i"`
	TP        string `json:"input_tp"`
	LRA       string `json:"input_lra"`
	Threshold string `json:"input_thresh"`
	Offset    string `json:"target_offset"`
}

// masterProgramAudio 先用整集 AAC 验收选定母带，预览与成片因此使用同一轮归一化结果。
func (e *Extractor) masterProgramAudio(ctx context.Context, p programAudioMastering, silence bool) (ProgramComposeReport, error) {
	p.partial = p.in != 0 || p.out != p.total
	if silence {
		path, err := e.encodeProgramWindow(ctx, p, p.rawPath())
		if err != nil {
			return ProgramComposeReport{}, err
		}
		return e.finishProgramAudio(ctx, p, path, ProgramComposeReport{LoudnessStatus: "silence"})
	}
	stats, err := e.measureProgramAudio(ctx, programRawInput(p.rawPath()), "")
	if err != nil {
		return ProgramComposeReport{}, err
	}
	master := filepath.Join(p.dir, "master-full.f32")
	masterFullM4A := filepath.Join(p.dir, "a-master.m4a")
	var acceptanceErr error
	for _, ceiling := range []float64{-2.3, -3.5, -5.0} {
		if err := e.normalizeProgramAudio(ctx, p, stats, ceiling, master); err != nil {
			return ProgramComposeReport{}, err
		}
		report, err := e.verifyFullMaster(ctx, p, master, masterFullM4A)
		if err == nil {
			return e.finishAcceptedProgramAudio(ctx, p, master, masterFullM4A, report)
		}
		if !errors.Is(err, ErrLoudnessAcceptance) {
			return ProgramComposeReport{}, err
		}
		acceptanceErr = err
	}
	return ProgramComposeReport{}, fmt.Errorf("三次 AAC 响度验收均未通过：%w", acceptanceErr)
}

// verifyFullMaster 只用完整音轨决定是否降低归一化真峰上限重试。
func (e *Extractor) verifyFullMaster(ctx context.Context, p programAudioMastering, master, m4a string) (ProgramComposeReport, error) {
	if err := e.encodeProgramAAC(ctx, master, m4a, p.total); err != nil {
		return ProgramComposeReport{}, err
	}
	if err := e.verifyProgramDecodedSamples(ctx, m4a, p.total); err != nil {
		return ProgramComposeReport{}, err
	}
	stats, err := e.measureProgramAAC(ctx, m4a, p.total)
	if err != nil {
		return ProgramComposeReport{}, err
	}
	return acceptProgramLoudness(stats)
}

// finishAcceptedProgramAudio 复用验收通过的整集 AAC；选区仅从该轮 PCM 切片编码。
func (e *Extractor) finishAcceptedProgramAudio(ctx context.Context, p programAudioMastering, master, fullM4A string, report ProgramComposeReport) (ProgramComposeReport, error) {
	if !p.partial {
		return e.finishProgramAudio(ctx, p, fullM4A, report)
	}
	path, err := e.encodeProgramWindow(ctx, p, master)
	if err != nil {
		return ProgramComposeReport{}, err
	}
	stats, err := e.measureProgramAAC(ctx, path, p.out-p.in)
	if err != nil {
		return ProgramComposeReport{}, err
	}
	return e.finishProgramAudio(ctx, p, path, programLoudnessReport(stats, "measured"))
}

// programRawInput 描述已归一化到 48kHz 双声道的本地浮点 PCM。
func programRawInput(path string) []string {
	return []string{"-f", "f32le", "-ar", "48000", "-ac", "2", "-protocol_whitelist", "file", "-i", path}
}

func programMP4Input(path string) []string {
	return []string{"-protocol_whitelist", "file", "-i", path}
}

// measureProgramAudio 读取最后一个 JSON 对象，不保留 FFmpeg 的整段日志。
func (e *Extractor) measureProgramAudio(ctx context.Context, input []string, prefix string) (programLoudness, error) {
	log := &programStderrTail{}
	args := append([]string{"-nostdin", "-hide_banner", "-nostats", "-v", "info", "-xerror"}, input...)
	args = append(args, "-map", "0:a:0", "-af", prefix+programLoudnormBase+":TP=-2.3:print_format=json", "-f", "null", "-")
	if err := e.runCmd(ctx, io.Discard, log, log.decodeErr, e.ffmpeg, args...); err != nil {
		if errors.Is(err, ErrDecode) {
			return programLoudness{}, fmt.Errorf("%w: 无法测量输入响度：%w", ErrLoudnessAcceptance, err)
		}
		return programLoudness{}, err
	}
	return parseProgramLoudness(log.buf)
}

func parseProgramLoudness(log []byte) (programLoudness, error) {
	for end := bytes.LastIndexByte(log, '}'); end >= 0; end = bytes.LastIndexByte(log[:end], '}') {
		start := bytes.LastIndexByte(log[:end], '{')
		var stats programLoudness
		if start >= 0 && json.Unmarshal(log[start:end+1], &stats) == nil && stats.I != "" && stats.TP != "" {
			return stats, nil
		}
	}
	return programLoudness{}, fmt.Errorf("%w: 缺少有效的 loudnorm 测量", ErrLoudnessAcceptance)
}

type programStderrTail struct{ buf []byte }

func (w *programStderrTail) Write(p []byte) (int, error) {
	n := len(p)
	if n >= programLoudnormTail {
		w.buf = append(w.buf[:0], p[n-programLoudnormTail:]...)
		return n, nil
	}
	if excess := len(w.buf) + n - programLoudnormTail; excess > 0 {
		w.buf = w.buf[:copy(w.buf, w.buf[excess:])]
	}
	w.buf = append(w.buf, p...)
	return n, nil
}

func (w *programStderrTail) decodeErr() error {
	return fmt.Errorf("%w: %s", ErrDecode, strings.TrimSpace(string(w.buf)))
}

func parseProgramMetric(s string) (float64, bool) {
	value, err := strconv.ParseFloat(s, 64)
	return value, err == nil && !math.IsInf(value, 0) && !math.IsNaN(value)
}

// secondPassFilter 只插入测得的有限数值；无门限测量的非零输入不冒充正常母带。
func (s programLoudness) secondPassFilter(ceiling float64) (string, error) {
	i, valid := parseProgramMetric(s.I)
	if !valid {
		return "", fmt.Errorf("%w: 非零节目没有有限的整集 LUFS（%q）", ErrLoudnessAcceptance, s.I)
	}
	tp, tpOK := parseProgramMetric(s.TP)
	lra, lraOK := parseProgramMetric(s.LRA)
	thresh, threshOK := parseProgramMetric(s.Threshold)
	offset, offsetOK := parseProgramMetric(s.Offset)
	if !tpOK || !lraOK || !threshOK || !offsetOK {
		return "", fmt.Errorf("%w: 整集 loudnorm 测量不完整", ErrLoudnessAcceptance)
	}
	return fmt.Sprintf("%s:TP=%g:measured_I=%g:measured_TP=%g:measured_LRA=%g:measured_thresh=%g:offset=%g:linear=true",
		programLoudnormBase, ceiling, i, tp, lra, thresh, offset), nil
}

// normalizeProgramAudio 在整集上执行第二遍，再用采样数锁定母带边界。
func (e *Extractor) normalizeProgramAudio(ctx context.Context, p programAudioMastering, stats programLoudness, ceiling float64, destination string) error {
	filter, err := stats.secondPassFilter(ceiling)
	if err != nil {
		return err
	}
	filter += fmt.Sprintf(",%s,apad=whole_len=%d,atrim=end_sample=%d", programAudioFormat, p.total, p.total)
	args := append([]string{"-nostdin", "-v", "error", "-xerror"}, programRawInput(p.rawPath())...)
	args = append(args, "-map", "0:a:0", "-af", filter, "-f", "f32le", "-y", destination)
	if _, err := e.run(ctx, e.ffmpeg, args...); err != nil {
		if errors.Is(err, ErrDecode) {
			return fmt.Errorf("%w: loudnorm 第二遍失败：%w", ErrLoudnessAcceptance, err)
		}
		return err
	}
	return checkProgramPCMSize(destination, p.total)
}

func checkProgramPCMSize(path string, samples int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() != samples*programStereoFrameBytes {
		return fmt.Errorf("%w: PCM 文件 %d 字节，期望 %d 个立体声采样帧", ErrDecode, info.Size(), samples)
	}
	return nil
}

// finishProgramAudio 封装已选定的 AAC，并核对各个 MP4 的内容指纹和采样数。
func (e *Extractor) finishProgramAudio(ctx context.Context, p programAudioMastering, m4a string, report ProgramComposeReport) (ProgramComposeReport, error) {
	hash, err := e.programAACSHA256(ctx, m4a)
	if err != nil {
		return ProgramComposeReport{}, err
	}
	report.AudioSHA256 = hash
	report.Outputs = make([]ProgramOutputReport, 0, len(p.outputs))
	for _, out := range p.outputs {
		if err := e.muxProgramOutput(ctx, out, m4a); err != nil {
			return ProgramComposeReport{}, err
		}
		if err := e.verifyProgramOutput(ctx, out.staged, hash, p.out-p.in); err != nil {
			return ProgramComposeReport{}, err
		}
		report.Outputs = append(report.Outputs, ProgramOutputReport{ComposeProgramOutputSpec: out.spec, AudioSHA256: hash})
	}
	return report, nil
}

func (e *Extractor) encodeProgramWindow(ctx context.Context, p programAudioMastering, master string) (string, error) {
	if !p.partial {
		path := filepath.Join(p.dir, "a-master.m4a")
		return path, e.encodeProgramAAC(ctx, master, path, p.total)
	}
	raw := filepath.Join(p.dir, "range.f32")
	if err := sliceProgramPCM(ctx, master, raw, p.sampleInterval); err != nil {
		return "", err
	}
	path := filepath.Join(p.dir, "a-range.m4a")
	return path, e.encodeProgramAAC(ctx, raw, path, p.out-p.in)
}

func (e *Extractor) encodeProgramAAC(ctx context.Context, raw, path string, samples int64) error {
	if err := checkProgramPCMSize(raw, samples); err != nil {
		return err
	}
	args := append([]string{"-nostdin", "-v", "error", "-xerror"}, programRawInput(raw)...)
	args = append(args, "-map", "0:a:0", "-c:a", "aac", "-b:a", "192k", "-ar", "48000", "-ac", "2",
		"-movie_timescale", "48000", "-use_editlist", "1", "-y", path)
	_, err := e.run(ctx, e.ffmpeg, args...)
	return err
}

func sliceProgramPCM(ctx context.Context, source, destination string, window sampleInterval) error {
	in, err := os.Open(source) //nolint:gosec // 路径由私有渲染目录生成。
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(destination) //nolint:gosec // 路径由私有渲染目录生成。
	if err != nil {
		return err
	}
	count := (window.out - window.in) * programStereoFrameBytes
	src := io.NewSectionReader(in, window.in*programStereoFrameBytes, count)
	_, copyErr := io.CopyN(out, &programPCMReader{ctx: ctx, reader: src}, count)
	return errors.Join(copyErr, out.Close())
}

type programPCMReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *programPCMReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (e *Extractor) muxProgramOutput(ctx context.Context, output programStagedOutput, m4a string) error {
	_, err := e.run(ctx, e.ffmpeg, "-nostdin", "-v", "error", "-xerror",
		"-protocol_whitelist", "file", "-i", output.picture, "-protocol_whitelist", "file", "-i", m4a,
		"-map", "0:v:0", "-map", "1:a:0", "-c:v", "copy", "-c:a", "copy", "-video_track_timescale", "24000",
		"-movie_timescale", "48000", "-use_editlist", "1", "-movflags", "+faststart", "-y", output.staged)
	return err
}

func (e *Extractor) programAACSHA256(ctx context.Context, path string) (string, error) {
	args := append([]string{"-nostdin", "-v", "error", "-xerror"}, programMP4Input(path)...)
	args = append(args, "-map", "0:a:0", "-c:a", "copy", "-f", "hash", "-hash", "sha256", "pipe:1")
	out, err := e.run(ctx, e.ffmpeg, args...)
	if err != nil {
		return "", err
	}
	text := strings.ToLower(strings.TrimSpace(string(out)))
	hash, ok := strings.CutPrefix(text, "sha256=")
	if _, err := hex.DecodeString(hash); !ok || len(hash) != 64 || err != nil {
		return "", fmt.Errorf("%w: 无法读取 AAC SHA-256", ErrDecode)
	}
	return hash, nil
}

func (e *Extractor) verifyProgramOutput(ctx context.Context, path, expectedHash string, samples int64) error {
	hash, err := e.programAACSHA256(ctx, path)
	if err != nil {
		return err
	}
	if hash != expectedHash {
		return fmt.Errorf("%w: 输出 AAC 与共享母带不一致", ErrDecode)
	}
	return e.verifyProgramDecodedSamples(ctx, path, samples)
}

func (e *Extractor) measureProgramAAC(ctx context.Context, path string, samples int64) (programLoudness, error) {
	// FFmpeg 解码会处理首包 priming；尾包可能保留不足一帧的填充，测量只取 edit list 的内容。
	prefix := fmt.Sprintf("%s,atrim=end_sample=%d,", programAudioFormat, samples)
	return e.measureProgramAudio(ctx, programMP4Input(path), prefix)
}

func (e *Extractor) verifyProgramDecodedSamples(ctx context.Context, path string, expected int64) error {
	out, err := e.run(ctx, e.ffprobe, "-v", "error", "-protocol_whitelist", "file",
		"-select_streams", "a:0", "-show_entries", "stream=duration_ts,time_base,start_pts", "-of", "json", path)
	if err != nil {
		return err
	}
	if err := checkProgramAudioTrack(out, expected); err != nil {
		return err
	}
	var count byteCountWriter
	if err := e.runStreamStdout(ctx, &count, e.ffmpeg, decodeProgramAudioArgs(path, "")...); err != nil {
		return err
	}
	n := int64(count) / programStereoFrameBytes
	if int64(count)%programStereoFrameBytes != 0 || n < expected || n >= expected+programAACFrameSamples {
		return fmt.Errorf("%w: MP4 解码为 %d 个采样，期望 %d 个（尾包填充不足 1024）", ErrDecode, n, expected)
	}
	return nil
}

func checkProgramAudioTrack(out []byte, expected int64) error {
	var info struct {
		Streams []struct {
			TimeBase string `json:"time_base"`
			Start    *int64 `json:"start_pts"`
			Duration int64  `json:"duration_ts"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &info); err != nil || len(info.Streams) != 1 {
		return fmt.Errorf("%w: MP4 缺少音轨时间基", ErrDecode)
	}
	s := info.Streams[0]
	if s.Start == nil || *s.Start != 0 || s.TimeBase != "1/48000" || s.Duration != expected {
		return fmt.Errorf("%w: MP4 音轨 edit list 与 %d 个采样不符（duration_ts=%d）", ErrDecode, expected, s.Duration)
	}
	return nil
}

func acceptProgramLoudness(s programLoudness) (ProgramComposeReport, error) {
	i, iOK := parseProgramMetric(s.I)
	tp, tpOK := parseProgramMetric(s.TP)
	if !tpOK || tp > -1.5 {
		return ProgramComposeReport{}, fmt.Errorf("%w: 解码真峰 %s dBTP，要求 ≤ -1.5 dBTP", ErrLoudnessAcceptance, s.TP)
	}
	if !iOK || i < -16.5 || i > -15.5 {
		return ProgramComposeReport{}, fmt.Errorf("%w: 解码响度 %s LUFS，要求 -16 ± 0.5 LUFS", ErrLoudnessAcceptance, s.I)
	}
	return ProgramComposeReport{LoudnessStatus: "accepted", LUFS: &i, TruePeak: &tp}, nil
}

// programLoudnessReport 仅报告选区的有限测量，不让选区的响度改变整集验收结果。
func programLoudnessReport(s programLoudness, status string) ProgramComposeReport {
	report := ProgramComposeReport{LoudnessStatus: status}
	if i, ok := parseProgramMetric(s.I); ok {
		report.LUFS = &i
	}
	if tp, ok := parseProgramMetric(s.TP); ok {
		report.TruePeak = &tp
	}
	return report
}
