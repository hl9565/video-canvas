package videoframe

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

// NormalizedAudioProbeVersion 将独立音频与视频的规范化探针隔离。
const NormalizedAudioProbeVersion int16 = 2

const (
	programStereoFrameBytes = 8
	programAudioFormat      = "aresample=48000,aformat=sample_fmts=flt:channel_layouts=stereo"
)

// AudioProbe 记录第一条音轨的起点与解码后 48kHz 双声道帧数。
type AudioProbe struct {
	AStartUs            int64
	DecodedAudioSamples int64
}

// ProbeProgramAudio 按节目单混音的格式完整解码第一条音轨，拒绝缺失或损坏的音轨。
func (e *Extractor) ProbeProgramAudio(ctx context.Context, source string) (AudioProbe, error) {
	out, err := e.run(ctx, e.ffprobe, "-v", "error", "-protocol_whitelist", "file",
		"-select_streams", "a:0", "-show_entries", "stream=index,start_time", "-of", "json", source)
	if err != nil {
		return AudioProbe{}, err
	}
	start, err := programAudioStart(out)
	if err != nil {
		return AudioProbe{}, err
	}
	var written byteCountWriter
	if err := e.runStreamStdout(ctx, &written, e.ffmpeg, decodeProgramAudioArgs(source, "")...); err != nil {
		return AudioProbe{}, err
	}
	if written <= 0 || int64(written)%programStereoFrameBytes != 0 {
		return AudioProbe{}, fmt.Errorf("%w: 没有完整的音频采样帧", ErrDecode)
	}
	return AudioProbe{AStartUs: start, DecodedAudioSamples: int64(written) / programStereoFrameBytes}, nil
}

// programAudioStart 对没有显式起点的格式（例如 WAV）使用零时间基。
func programAudioStart(data []byte) (int64, error) {
	var metadata struct {
		Streams []struct {
			Start string `json:"start_time"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil || len(metadata.Streams) != 1 {
		return 0, fmt.Errorf("%w: 找不到第一条音轨", ErrDecode)
	}
	raw := metadata.Streams[0].Start
	if raw == "" || raw == "N/A" {
		return 0, nil
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	micros := math.Round(seconds * 1e6)
	if err != nil || math.IsNaN(micros) || math.Abs(micros) >= math.MaxInt64 {
		return 0, fmt.Errorf("%w: 音轨起点无效", ErrDecode)
	}
	return int64(micros), nil
}

func decodeProgramAudioArgs(source, trim string) []string {
	return []string{"-nostdin", "-v", "error", "-xerror", "-protocol_whitelist", "file",
		"-i", source, "-map", "0:a:0", "-vn", "-af", "asetpts=PTS-STARTPTS," + programAudioFormat + trim,
		"-f", "f32le", "pipe:1"}
}
