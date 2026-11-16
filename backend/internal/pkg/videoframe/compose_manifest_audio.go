package videoframe

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

const audioMixChunkSamples = 48000

func (e *Extractor) renderManifestAudioM4A(
	ctx context.Context,
	dir string,
	clips []resolvedClip,
	totalFrames, rangeIn, rangeOut int32,
	input ComposeManifestInput,
	m4aPath string,
) (err error) {
	rangeSamples := int64(rangeOut-rangeIn) * SamplesPerFrame24
	rangeStartSample := int64(rangeIn) * SamplesPerFrame24
	rangeEndSample := int64(rangeOut) * SamplesPerFrame24
	totalEpisodeSamples := int64(totalFrames) * SamplesPerFrame24
	dataBytes := uint64(rangeSamples) * 4
	if rangeSamples < 0 || dataBytes > math.MaxUint32-36 {
		return fmt.Errorf("%w: 音频长度超出 WAV 封装上限", ErrInvalidManifest)
	}

	wavPath := filepath.Join(dir, "a-master.wav")
	defer func() { _ = os.Remove(wavPath) }()
	if err := e.writeManifestWAVStreaming(ctx, wavPath, clips, rangeStartSample, rangeEndSample, totalEpisodeSamples, uint32(dataBytes), input); err != nil {
		return err
	}
	_, err = e.run(ctx, e.ffmpeg,
		"-nostdin", "-v", "error", "-protocol_whitelist", "file", "-i", wavPath,
		"-c:a", "aac", "-b:a", "192k", "-ar", strconv.Itoa(composeAudioRate), "-ac", "2",
		"-y", m4aPath,
	)
	return err
}

func (e *Extractor) writeManifestWAVStreaming(
	ctx context.Context,
	wavPath string,
	clips []resolvedClip,
	rangeStartSample, rangeEndSample, totalEpisodeSamples int64,
	dataBytes uint32,
	input ComposeManifestInput,
) (err error) {
	f, err := os.OpenFile(wavPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600) //nolint:gosec // wavPath 由服务临时目录拼接。
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()

	bw := bufio.NewWriterSize(f, 64*1024)
	hdr := buildPCM16StereoWAVHeader(dataBytes)
	if _, err := bw.Write(hdr[:]); err != nil {
		return err
	}

	// 单次只分配 1 秒的 float64 与 PCM16 块缓存，避免长剧集整集音频常驻堆内存。
	mixChunk := make([]float64, audioMixChunkSamples*2)
	pcmChunk := make([]byte, audioMixChunkSamples*4)
	writtenSample := rangeStartSample

	for _, clip := range clips {
		clipMasterStart := int64(clip.masterInF) * SamplesPerFrame24
		clipMasterEnd := int64(clip.masterOutF) * SamplesPerFrame24
		overlapStart := max(clipMasterStart, rangeStartSample)
		overlapEnd := min(clipMasterEnd, rangeEndSample)
		if overlapEnd <= overlapStart {
			continue
		}
		if overlapStart > writtenSample {
			if err := writeSilencePCMChunks(bw, overlapStart-writtenSample, pcmChunk); err != nil {
				return err
			}
		}
		if err := e.streamClipAudioToWAV(ctx, clip, overlapStart, overlapEnd, totalEpisodeSamples, input, mixChunk, pcmChunk, bw); err != nil {
			return err
		}
		writtenSample = overlapEnd
	}
	if writtenSample < rangeEndSample {
		if err := writeSilencePCMChunks(bw, rangeEndSample-writtenSample, pcmChunk); err != nil {
			return err
		}
	}
	return bw.Flush()
}

func writeSilencePCMChunks(bw *bufio.Writer, count int64, pcmChunk []byte) error {
	clear(pcmChunk)
	capSamples := int64(len(pcmChunk) / 4)
	for count > 0 {
		n := min(capSamples, count)
		if _, err := bw.Write(pcmChunk[:n*4]); err != nil {
			return err
		}
		count -= n
	}
	return nil
}

type clipStreamMixSpec struct {
	clipMasterStart int64
	activeStartClip int64
	activeEndClip   int64
	targetSamples   int64
	clipGain        float64
	fadeInF         int32
	fadeOutF        int32
}

func (e *Extractor) streamClipAudioToWAV(
	ctx context.Context,
	clip resolvedClip,
	overlapStart, overlapEnd, totalEpisodeSamples int64,
	input ComposeManifestInput,
	mixChunk []float64,
	pcmChunk []byte,
	bw *bufio.Writer,
) (err error) {
	win := ComputeNativeAudioWindow(NativeAudioInput{
		HasAudio:            clip.HasAudio,
		VStartUs:            clip.VStartUs,
		AStartUs:            clip.AStartUs,
		DecodedAudioSamples: clip.DecodedAudioSamples,
		AvPtsOffsetMs:       clip.AvPtsOffsetMs,
		SourceInF:           clip.SourceInF,
		SourceOutF:          clip.outF,
		MuteOriginal:        clip.MuteOriginal,
	})
	clipMasterStart := int64(clip.masterInF) * SamplesPerFrame24
	activeStartClip := max(win.LeadPad, overlapStart-clipMasterStart)
	activeEndClip := min(win.LeadPad+(win.K1-win.K0), overlapEnd-clipMasterStart)
	if win.UseSilence || activeEndClip <= activeStartClip {
		return writeSilencePCMChunks(bw, overlapEnd-overlapStart, pcmChunk)
	}

	stream, err := e.openAudio48kS16LE(ctx, clip.SourcePath)
	if err != nil {
		return err
	}
	hitEOF := false
	defer func() {
		finishErr := stream.finish(ctx, hitEOF, err == nil)
		if err == nil && finishErr != nil {
			err = finishErr
		}
	}()

	hitEOF, err = discardAudioPrefixSamples(stream.reader, win.K0+(activeStartClip-win.LeadPad))
	if err != nil {
		return err
	}

	spec := clipStreamMixSpec{
		clipMasterStart: clipMasterStart,
		activeStartClip: activeStartClip,
		activeEndClip:   activeEndClip,
		targetSamples:   win.TargetSamples,
		clipGain:        math.Pow(10.0, clip.AudioGainDB/20.0),
		fadeInF:         clip.FadeInF,
		fadeOutF:        clip.FadeOutF,
	}
	for chunkStart := overlapStart; chunkStart < overlapEnd; {
		n := int(min(audioMixChunkSamples, overlapEnd-chunkStart))
		samples := mixChunk[:n*2]
		hitEOF, err = fillClipAudioChunk(stream.reader, samples, chunkStart, n, hitEOF, spec)
		if err != nil {
			return err
		}
		applyEnvelopeToRange(samples, chunkStart, totalEpisodeSamples, input.AudioGainDB, input.FadeInF, input.FadeOutF)
		encodePCM16Chunk(pcmChunk[:n*4], samples)
		if _, err := bw.Write(pcmChunk[:n*4]); err != nil {
			return err
		}
		chunkStart += int64(n)
	}
	return nil
}

func discardAudioPrefixSamples(r *bufio.Reader, sampleCount int64) (bool, error) {
	if sampleCount <= 0 {
		return false, nil
	}
	_, err := io.CopyN(io.Discard, r, sampleCount*4)
	if err == nil {
		return false, nil
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true, nil
	}
	return false, err
}

func fillClipAudioChunk(r *bufio.Reader, samples []float64, chunkStart int64, n int, hitEOF bool, spec clipStreamMixSpec) (bool, error) {
	clear(samples)
	var frame [4]byte
	for s := range n {
		clipPos := (chunkStart + int64(s)) - spec.clipMasterStart
		if hitEOF || clipPos < spec.activeStartClip || clipPos >= spec.activeEndClip {
			continue
		}
		if _, err := io.ReadFull(r, frame[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				hitEOF = true
				continue
			}
			return hitEOF, err
		}
		env := spec.clipGain * fadeEnvelopeFactor(clipPos, spec.targetSamples, spec.fadeInF, spec.fadeOutF)
		samples[s*2] = float64(int16(binary.LittleEndian.Uint16(frame[0:2]))) * env
		samples[s*2+1] = float64(int16(binary.LittleEndian.Uint16(frame[2:4]))) * env
	}
	return hitEOF, nil
}

type decodedAudioStream struct {
	cctx   context.Context
	cancel context.CancelFunc
	cmd    *exec.Cmd
	stdout io.ReadCloser
	reader *bufio.Reader
	stderr *boundedStderr
	sem    chan struct{}
}

func (e *Extractor) openAudio48kS16LE(ctx context.Context, source string) (*decodedAudioStream, error) {
	if _, err := exec.LookPath(e.ffmpeg); err != nil {
		return nil, ErrUnavailable
	}
	select {
	case e.sem <- struct{}{}:
	default:
		return nil, ErrBusy
	}
	cctx, cancel := context.WithTimeout(ctx, e.timeout)
	args := []string{
		"-nostdin", "-v", "error", "-protocol_whitelist", "file", "-i", source,
		"-map", "0:a:0", "-vn",
		"-af", fmt.Sprintf("asetpts=PTS-STARTPTS,aresample=%d,aformat=sample_fmts=s16:channel_layouts=stereo", composeAudioRate),
		"-f", "s16le", "pipe:1",
	}
	cmd := exec.CommandContext(cctx, e.ffmpeg, args...) //nolint:gosec // 二进制路径来自配置，参数全部由本包拼接，输入协议限制为 file。
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		<-e.sem
		return nil, err
	}
	stderr := newBoundedStderr()
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		cancel()
		<-e.sem
		return nil, err
	}
	return &decodedAudioStream{
		cctx:   cctx,
		cancel: cancel,
		cmd:    cmd,
		stdout: stdout,
		reader: bufio.NewReaderSize(stdout, 64*1024),
		stderr: stderr,
		sem:    e.sem,
	}, nil
}

func (s *decodedAudioStream) finish(ctx context.Context, hitEOF, wantCheck bool) error {
	defer func() {
		s.cancel()
		<-s.sem
	}()
	// 若已成功读够裁剪区间且 ffmpeg 仍在解码后续丢弃帧，直接取消子进程并由 Wait 回收管道，避免手动双重 Close fd。
	if !hitEOF && wantCheck && s.cctx.Err() == nil && ctx.Err() == nil {
		s.cancel()
		_ = s.cmd.Wait()
		return nil
	}
	if !wantCheck {
		s.cancel()
	}
	if err := s.cmd.Wait(); err != nil && wantCheck {
		if errors.Is(s.cctx.Err(), context.DeadlineExceeded) {
			return ErrTimeout
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return s.stderr.decodeErr()
	}
	if errors.Is(s.cctx.Err(), context.DeadlineExceeded) {
		return ErrTimeout
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func applyEnvelopeToRange(mix []float64, rangeStartSample, totalSamples int64, gainDB float64, fadeInF, fadeOutF int32) {
	masterGain := math.Pow(10.0, gainDB/20.0)
	n := int64(len(mix) / 2)
	for s := range n {
		masterPos := rangeStartSample + s
		env := masterGain * fadeEnvelopeFactor(masterPos, totalSamples, fadeInF, fadeOutF)
		mix[s*2] *= env
		mix[s*2+1] *= env
	}
}

func fadeEnvelopeFactor(pos, total int64, fadeInF, fadeOutF int32) float64 {
	factor := 1.0
	if fadeInF > 0 {
		inSamples := int64(fadeInF) * SamplesPerFrame24
		if pos < inSamples {
			factor *= float64(pos) / float64(inSamples)
		}
	}
	if fadeOutF > 0 {
		outSamples := int64(fadeOutF) * SamplesPerFrame24
		rem := total - pos
		if rem <= outSamples {
			factor *= float64(max(rem-1, 0)) / float64(outSamples)
		}
	}
	return factor
}

func buildPCM16StereoWAVHeader(dataSize uint32) [44]byte {
	var hdr [44]byte
	copy(hdr[0:4], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:8], 36+dataSize)
	copy(hdr[8:12], "WAVE")
	copy(hdr[12:16], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:20], 16)
	binary.LittleEndian.PutUint16(hdr[20:22], 1)
	binary.LittleEndian.PutUint16(hdr[22:24], 2)
	binary.LittleEndian.PutUint32(hdr[24:28], composeAudioRate)
	binary.LittleEndian.PutUint32(hdr[28:32], composeAudioRate*4)
	binary.LittleEndian.PutUint16(hdr[32:34], 4)
	binary.LittleEndian.PutUint16(hdr[34:36], 16)
	copy(hdr[36:40], "data")
	binary.LittleEndian.PutUint32(hdr[40:44], dataSize)
	return hdr
}

func encodePCM16Chunk(dst []byte, samples []float64) {
	for i, v := range samples {
		s := int16(max(min(math.Round(v), 32767), -32768))
		binary.LittleEndian.PutUint16(dst[i*2:i*2+2], uint16(s))
	}
}
