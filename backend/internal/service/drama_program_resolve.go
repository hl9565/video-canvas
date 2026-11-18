package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/jsonhash"
	"video-canvas/internal/pkg/videoframe"
)

type dramaManifestV2Source struct {
	project         model.DramaProject
	episode         model.DramaEpisode
	rows            []model.DramaSegment
	program         model.DramaProgram
	composeSnapshot string
	videos          map[uint64]uint64 // 分镜主键 -> 可采用的视频素材主键。
}

type dramaV2ClipState struct {
	clip   model.DramaManifestV2Clip
	cold   bool
	failed bool
}

type dramaV2Timeline struct {
	bySegment   map[uint64]dramaV2ClipState
	skipped     []int
	coldAssets  []uint64
	coldStills  []uint64
	videoAssets []model.DramaProgramBoundSource
	diagnostics []DramaProgramDiagnostic
}

// dramaV2Resolution 保留公开视图与同一次解析中用于哈希和渲染的内部清单。
type dramaV2Resolution struct {
	*DramaManifestV2ResponseView
	frozen *model.DramaEpisodeManifestV2
}

// ResolveEpisodeManifestV2 在锁外将集级节目单解析到规范化时间轴。
func (s *DramaService) ResolveEpisodeManifestV2(ctx context.Context, user, id uint64, no int) (*DramaManifestV2ResponseView, error) {
	// 1. 读取采用及节目单快照，再在锁外完成探测与诊断。
	source, err := s.loadDramaManifestV2Source(ctx, user, id, no)
	if err != nil {
		return nil, err
	}
	resolved, err := s.resolveDramaManifestV2Source(ctx, source)
	if err != nil {
		return nil, err
	}
	return resolved.DramaManifestV2ResponseView, nil
}

// PrepareOrResolveEpisodeManifestV2 只在缺冷探针且没有已知诊断时返回 202。
func (s *DramaService) PrepareOrResolveEpisodeManifestV2(ctx context.Context, user, id uint64, no int) (*DramaManifestV2ResponseView, *DramaManifestV2PrepareView, error) {
	// 1. 同时检查节目单与当前素材，避免已知错误被冷探针遮住。
	source, err := s.loadDramaManifestV2Source(ctx, user, id, no)
	if err != nil {
		return nil, nil, err
	}
	audio := s.readDramaV2Audio(ctx, source, false)
	timeline := s.readDramaV2Timeline(ctx, source, false)
	ready, err := assembleDramaV2Manifest(source, timeline, audio)
	if err != nil {
		return nil, nil, err
	}
	key, err := dramaManifestV2WarmupKey(source, timeline, audio)
	if err != nil {
		return nil, nil, err
	}
	if ready != nil {
		s.clearManifestWarmupKey(key)
		return ready.DramaManifestV2ResponseView, nil, nil
	}
	// 2. 只异步补齐探针；下次请求使用最新的节目单和媒体快照。
	if err := s.ensureDramaV2Warmup(key, user, timeline, audio); err != nil {
		var failure *dramaV2ProbeFailure
		if !errors.As(err, &failure) {
			return nil, nil, err
		}
		if failure.audio {
			audio.media[failure.assetID] = dramaV2AudioMedia{failed: true}
		} else {
			timeline.markProbeFailed(source.rows, failure.assetID, failure.still)
		}
		view, err := assembleDramaV2Manifest(source, timeline, audio)
		if view == nil {
			return nil, nil, err
		}
		return view.DramaManifestV2ResponseView, nil, err
	}
	return nil, &DramaManifestV2PrepareView{
		Status: "preparing", SegmentVersion: source.episode.SegmentVersion, SkippedSeqs: timeline.skipped,
		Diagnostics: []string{}, ProgramDiagnostics: []DramaProgramDiagnostic{},
	}, nil
}

// loadDramaManifestV2Source 读取归属、节目单及所有分镜，包括没有采用的分镜。
func (s *DramaService) loadDramaManifestV2Source(ctx context.Context, user, id uint64, no int) (*dramaManifestV2Source, error) {
	p, ep, err := s.readProgramEpisode(ctx, user, id, no)
	if err != nil {
		return nil, err
	}
	reader, ok := s.repo.(dramaManifestReader)
	if !ok {
		return nil, errcode.ErrInternal
	}
	rows, err := reader.Segments(ctx, user, ep.ID)
	if err != nil {
		return nil, dramaError(err)
	}
	program, err := model.DecodeStoredDramaProgram(json.RawMessage(ep.ProgramJSON))
	if err != nil {
		return nil, err
	}
	source, err := readDramaManifestV2Source(ctx, reader, p, *ep, rows, program)
	return source, dramaError(err)
}

// readDramaManifestV2Source 只读取数据库中的采用来源；失效的采用留给 V2 诊断。
func readDramaManifestV2Source(ctx context.Context, reader dramaManifestReader, p *model.DramaProject, ep model.DramaEpisode, rows []model.DramaSegment, program model.DramaProgram) (*dramaManifestV2Source, error) {
	sortDramaManifestRows(rows)
	src := &dramaManifestV2Source{project: *p, episode: ep, rows: rows, program: program, videos: make(map[uint64]uint64)}
	for _, row := range rows {
		if dramaVisualKind(row) != model.DramaVisualVideo || row.ChosenTaskID == nil {
			continue
		}
		asset, err := programChosenVideo(ctx, reader, p, &row)
		if isUnreadyProgramVideo(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		src.videos[row.ID] = asset
	}
	return src, nil
}

// isUnreadyProgramVideo 区分不可用采用和应原样返回的仓储错误。
func isUnreadyProgramVideo(err error) bool {
	var business *errcode.Error
	return errors.As(err, &business) && (business.Code == errcode.ErrDramaTakeInvalid.Code || business.Code == errcode.ErrDramaTakeQCRejected.Code)
}

// resolveDramaManifestV2Source 先发现无需探测的错误，再同步补齐剩余探针。
func (s *DramaService) resolveDramaManifestV2Source(ctx context.Context, source *dramaManifestV2Source) (*dramaV2Resolution, error) {
	resolved, err := s.resolveCachedDramaManifestV2Source(ctx, source)
	if resolved != nil || err != nil {
		return resolved, err
	}
	return assembleDramaV2Manifest(source, s.readDramaV2Timeline(ctx, source, true), s.readDramaV2Audio(ctx, source, true))
}

// resolveCachedDramaManifestV2Source 返回已有诊断或热清单，冷探针留给调用方调度。
func (s *DramaService) resolveCachedDramaManifestV2Source(ctx context.Context, source *dramaManifestV2Source) (*dramaV2Resolution, error) {
	return assembleDramaV2Manifest(source, s.readDramaV2Timeline(ctx, source, false), s.readDramaV2Audio(ctx, source, false))
}

// checkDramaV2Ratio 限制 Stage C 的渲染画幅，旧版合成和预览仍支持其它比例。
func checkDramaV2Ratio(ratio string) error {
	if ratio == dramaStageCRatio {
		return nil
	}
	return programDiagnosticError(errcode.ErrDramaVisualInvalid.WithMsg("节目单与新型视觉仅支持 16:9"), []DramaProgramDiagnostic{{
		Field: "ratio", Reason: "unsupported_ratio", Severity: "error",
	}})
}

// dramaManifestV2WarmupKey 在补写音频哈希期间保持稳定，避免启动重复预热。
func dramaManifestV2WarmupKey(src *dramaManifestV2Source, timeline dramaV2Timeline, audio dramaV2AudioState) (string, error) {
	key, err := jsonhash.CanonicalSHA256(struct {
		User, Episode uint64
		Version       int64
		Videos        []model.DramaProgramBoundSource
		Audio, Stills []uint64
	}{src.project.UserID, src.episode.ID, src.episode.SegmentVersion, timeline.videoAssets, audio.assets, timeline.coldStills})
	return "v2:" + key, err
}

// dramaV2ProbeFailure 记录失败素材，使后续轮询能返回片段及锚点诊断。
type dramaV2ProbeFailure struct {
	assetID uint64
	sha256  string
	audio   bool
	still   bool
}

func (*dramaV2ProbeFailure) Error() string { return "节目单素材不可读取或探测" }

// markProbeFailed 保留失败片段的身份，供节目单返回明确的锚点诊断。
func (t *dramaV2Timeline) markProbeFailed(rows []model.DramaSegment, assetID uint64, still bool) {
	t.skipped = t.skipped[:0]
	t.coldAssets = t.coldAssets[:0]
	t.coldStills = t.coldStills[:0]
	for _, row := range rows {
		state, exists := t.bySegment[row.ID]
		if state.cold && isColdDramaV2Asset(state.clip, assetID, still) {
			state.cold, state.failed = false, true
			t.bySegment[row.ID] = state
			t.diagnostics = append(t.diagnostics, DramaProgramDiagnostic{
				SegmentID: idcodec.ID(row.ID), Field: "visual", Reason: "media_unavailable", Severity: "error",
			})
		}
		if !exists || state.failed {
			t.skipped = append(t.skipped, row.Seq)
		}
		t.addColdClip(state)
	}
}

// isColdDramaV2Asset 区分冷视频探针和待补算指纹的静帧。
func isColdDramaV2Asset(clip model.DramaManifestV2Clip, id uint64, still bool) bool {
	if still {
		return clip.StillAssetID != nil && *clip.StillAssetID == id
	}
	return clip.AssetID == id
}

// addColdClip 收集需要异步探测的视频和需要补算指纹的图片。
func (t *dramaV2Timeline) addColdClip(state dramaV2ClipState) {
	if !state.cold {
		return
	}
	if state.clip.VisualKind == model.DramaVisualStill {
		t.coldStills = append(t.coldStills, *state.clip.StillAssetID)
	} else {
		t.coldAssets = append(t.coldAssets, state.clip.AssetID)
	}
}

// ensureDramaV2Warmup 复用剧集预热；失败指纹过时才重试当前素材。
func (s *DramaService) ensureDramaV2Warmup(key string, user uint64, timeline dramaV2Timeline, audio dramaV2AudioState) error {
	work := func(ctx context.Context) error {
		return s.warmDramaV2Probes(ctx, user, timeline.coldAssets, timeline.coldStills, audio.cold)
	}
	err := s.ensureManifestProbeWork(key, false, work)
	var failure *dramaV2ProbeFailure
	if errors.As(err, &failure) && failure.audio {
		media := audio.media[failure.assetID]
		if media.probe != nil || (media.sha256 != "" && media.sha256 != failure.sha256) {
			s.clearManifestWarmupKey(key)
			return s.ensureManifestProbeWork(key, false, work)
		}
	}
	return err
}

// warmDramaV2Probes 填充缓存，失败只通知当前轮询，不让 202 一直重复。
func (s *DramaService) warmDramaV2Probes(ctx context.Context, user uint64, videos, stills, audio []uint64) error {
	for _, id := range videos {
		if _, _, _, err := s.readDramaV2VideoProbe(ctx, user, id, true); err != nil {
			return &dramaV2ProbeFailure{assetID: id}
		}
	}
	for _, id := range stills {
		clip := model.DramaManifestV2Clip{DramaManifestV2ClipParams: model.DramaManifestV2ClipParams{StillAssetID: &id}}
		if _, reason := s.readDramaV2Still(ctx, user, &clip, true); reason != "" {
			return &dramaV2ProbeFailure{assetID: id, still: true}
		}
	}
	for _, id := range audio {
		if hash, _, err := s.dramaManifestAudioProbe(ctx, user, id); err != nil {
			return &dramaV2ProbeFailure{assetID: id, sha256: hash, audio: true}
		}
	}
	return nil
}

// readDramaV2Timeline 收集片段元数据和探针；冷片段仍可校验采用身份和显式裁剪点。
func (s *DramaService) readDramaV2Timeline(ctx context.Context, src *dramaManifestV2Source, probe bool) dramaV2Timeline {
	timeline := dramaV2Timeline{bySegment: make(map[uint64]dramaV2ClipState, len(src.rows)), skipped: []int{}}
	for _, row := range src.rows {
		if dramaVisualKind(row) == model.DramaVisualVideo && row.ChosenTaskID == nil {
			timeline.skipped = append(timeline.skipped, row.Seq)
			continue
		}
		state, reason := s.readDramaV2Clip(ctx, src, row, probe)
		timeline.bySegment[row.ID] = state
		if reason != "" {
			timeline.skipped = append(timeline.skipped, row.Seq)
			timeline.diagnostics = append(timeline.diagnostics, DramaProgramDiagnostic{
				SegmentID: idcodec.ID(row.ID), Field: "visual", Reason: reason, Severity: "error",
			})
		}
		if state.clip.AssetID != 0 {
			timeline.videoAssets = append(timeline.videoAssets, clipSource(state.clip))
		}
		timeline.addColdClip(state)
	}
	return timeline
}

// readDramaV2Clip 区分视频、静帧和黑场，后两者使用显式持续帧数、无需视频探针。
func (s *DramaService) readDramaV2Clip(ctx context.Context, src *dramaManifestV2Source, row model.DramaSegment, probe bool) (dramaV2ClipState, string) {
	clip := dramaV2BaseClip(row)
	reason := ""
	cold := false
	if clip.VisualKind == model.DramaVisualVideo {
		return s.readDramaV2Video(ctx, src, row, clip, probe)
	}
	if row.SourceOutF == nil || row.SourceInF != 0 || *row.SourceOutF <= 0 {
		reason = "visual_invalid"
	} else if clip.VisualKind == model.DramaVisualStill {
		cold, reason = s.readDramaV2Still(ctx, src.project.UserID, &clip, probe)
	} else if clip.VisualKind != model.DramaVisualBlack {
		reason = "visual_invalid"
	}
	clip.NormalizedFrames24 = clip.SourceOutF
	if reason == "" && !populateDramaV2FX(row, &clip) {
		reason = "visual_invalid"
	}
	return dramaV2ClipState{clip: clip, cold: cold && reason == "", failed: reason != ""}, reason
}

// dramaV2BaseClip 复制所有公开的裁剪与原声参数。
func dramaV2BaseClip(row model.DramaSegment) model.DramaManifestV2Clip {
	clip := model.DramaManifestV2Clip{SegmentID: row.ID, DramaManifestV2ClipParams: model.DramaManifestV2ClipParams{
		Seq: row.Seq, VisualKind: dramaVisualKind(row), StillAssetID: row.StillAssetID, SourceInF: row.SourceInF,
		MuteOriginal: row.MuteOriginal, AudioGainDB: row.AudioGainDB, NativeIsDialogue: row.NativeIsDialogue, AvPtsOffsetMs: row.AvPtsOffsetMs,
	}}
	if row.SourceOutF != nil {
		clip.SourceOutF = *row.SourceOutF
	}
	return clip
}

// readDramaV2Video 读取视频身份与探针；缺探针时保留身份供锚点错误的早期判断。
func (s *DramaService) readDramaV2Video(ctx context.Context, src *dramaManifestV2Source, row model.DramaSegment, clip model.DramaManifestV2Clip, probe bool) (dramaV2ClipState, string) {
	id := src.videos[row.ID]
	if id == 0 {
		return dramaV2ClipState{clip: clip, failed: true}, "media_unavailable"
	}
	asset, media, hit, err := s.readDramaV2VideoProbe(ctx, src.project.UserID, id, probe)
	if err != nil {
		return dramaV2ClipState{clip: clip, failed: true}, "media_unavailable"
	}
	clip.TaskID, clip.AssetID, clip.Sha256 = *row.ChosenTaskID, asset.ID, asset.Sha256
	if !populateDramaV2FX(row, &clip) {
		return dramaV2ClipState{clip: clip, failed: true}, "visual_invalid"
	}
	if !hit {
		return dramaV2ClipState{clip: clip, cold: true}, ""
	}
	if !applyDramaV2VideoProbe(row, media, &clip) {
		return dramaV2ClipState{clip: clip, failed: true}, "visual_invalid"
	}
	return dramaV2ClipState{clip: clip}, ""
}

// applyDramaV2VideoProbe 解析 EOF，沿用视频原声与裁剪边界的 v1 校验。
func applyDramaV2VideoProbe(row model.DramaSegment, probe *model.AssetMediaProbe, clip *model.DramaManifestV2Clip) bool {
	video := manifestVideoClip(row, probe)
	// 单片过长由 V2 时间轴上限诊断；此处只复用剪辑及原声参数规则。
	in, out, valid := programTrim(row, probe.NormalizedFrames24)
	if !valid {
		return false
	}
	video.SourceOutF = new(in + min(model.DramaProgramMaxFrames, out-in))
	if _, _, err := videoframe.NormalizeManifestClips([]videoframe.ManifestClip{video}); err != nil {
		return false
	}
	clip.SourceOutF, clip.NormalizedFrames24 = out, probe.NormalizedFrames24
	clip.HasAudio, clip.DecodedAudioSamples = probe.HasAudio, probe.DecodedAudioSamples
	clip.VStartUs, clip.AStartUs = probe.VStartUs, probe.AStartUs
	return true
}

// populateDramaV2FX 投影已存储的特效，空对象保持缺省。
func populateDramaV2FX(row model.DramaSegment, clip *model.DramaManifestV2Clip) bool {
	if !dramaSegmentHasFX(row) {
		return true
	}
	var fx model.DramaSegmentFX
	if err := json.Unmarshal(row.FXJSON, &fx); err != nil {
		return false
	}
	clip.FX = &fx
	return true
}

// dramaVisualKind 为历史行和旧夹具补上默认视频模式。
func dramaVisualKind(row model.DramaSegment) string {
	if row.VisualKind == "" {
		return model.DramaVisualVideo
	}
	return row.VisualKind
}

// dramaSegmentHasFX 区分无特效的空对象和已编排的特效。
func dramaSegmentHasFX(row model.DramaSegment) bool {
	raw := bytes.TrimSpace(row.FXJSON)
	return len(raw) != 0 && !bytes.Equal(raw, []byte("{}"))
}

// dramaSegmentUsesStageC 标识旧工作流不能覆盖或渲染的视觉编辑。
func dramaSegmentUsesStageC(row model.DramaSegment) bool {
	return dramaVisualKind(row) != model.DramaVisualVideo || row.StillAssetID != nil || dramaSegmentHasFX(row)
}

// clipSource 从公开片段内部值提取锚点的源指纹。
func clipSource(clip model.DramaManifestV2Clip) model.DramaProgramBoundSource {
	return model.DramaProgramBoundSource{TaskID: clip.TaskID, AssetID: clip.AssetID, Sha256: clip.Sha256}
}

// assembleDramaV2Manifest 保留原有条目，只有完整且无错误的时间轴才生成哈希。
func assembleDramaV2Manifest(src *dramaManifestV2Source, timeline dramaV2Timeline, audio dramaV2AudioState) (*dramaV2Resolution, error) {
	diagnostics := slices.Clone(timeline.diagnostics)
	clips, total, complete, diagnostics := freezeDramaV2Clips(src, timeline, diagnostics)
	program, diagnostics := resolveProgramAgainstTimeline(src, timeline, clips, total, complete, diagnostics)
	audioDiagnostics, missing := freezeDramaV2Audio(src.program, &program, audio)
	diagnostics = append(audioDiagnostics, diagnostics...)
	if (timeline.hasColdClips() || len(audio.cold) > 0) && len(diagnostics) == 0 {
		return nil, nil
	}
	if len(clips) == 0 && len(diagnostics) == 0 {
		return nil, errcode.ErrDramaNothingToCompose
	}
	manifest := model.DramaEpisodeManifestV2{
		Version: model.CurrentDramaManifestV2SchemaVersion, RenderVersion: model.CurrentDramaManifestV2RenderVersion,
		EpisodeID: src.episode.ID, SegmentVersion: src.episode.SegmentVersion, FPS: model.DramaProgramFPS, SampleRate: model.DramaProgramSampleRate,
		TotalFrames: total, SkippedSeqs: timeline.skipped, Clips: clips, Program: program,
	}
	view := DramaManifestV2ViewFromDomain(manifest)
	response := &DramaManifestV2ResponseView{Status: "ready", Manifest: &view, SegmentVersion: manifest.SegmentVersion,
		SkippedSeqs: manifest.SkippedSeqs, Diagnostics: dramaProgramDiagnosticMessages(src, diagnostics, missing), ProgramDiagnostics: diagnostics}
	if len(diagnostics) == 0 {
		hash, err := jsonhash.CanonicalSHA256(manifest)
		if err != nil {
			return nil, err
		}
		response.ManifestHash = hash
	}
	return &dramaV2Resolution{DramaManifestV2ResponseView: response, frozen: &manifest}, nil
}

// hasColdClips 判断画面时间轴是否还缺素材指纹或视频探针。
func (t dramaV2Timeline) hasColdClips() bool {
	return len(t.coldAssets) != 0 || len(t.coldStills) != 0
}

// freezeDramaV2Clips 不给部分预热的片段推测母版时间；上限诊断也不依赖其他冷探针。
func freezeDramaV2Clips(src *dramaManifestV2Source, timeline dramaV2Timeline, diagnostics []DramaProgramDiagnostic) ([]model.DramaManifestV2Clip, int32, bool, []DramaProgramDiagnostic) {
	clips := []model.DramaManifestV2Clip{}
	var total int64
	count := 0
	for _, row := range src.rows {
		state, ok := timeline.bySegment[row.ID]
		if !ok || state.failed {
			continue
		}
		count++
		if state.cold {
			continue
		}
		c := state.clip
		c.MasterInF = int32(total)
		total += int64(c.SourceOutF - c.SourceInF)
		if total > model.DramaProgramMaxFrames {
			break // 总长已超限，无需为后续分镜计算母版起止点。
		}
		c.MasterOutF = int32(total)
		clips = append(clips, c)
	}
	if err := model.ValidateDramaProgramTimeline(count, int32(min(total, model.DramaProgramMaxFrames+1))); err != nil {
		diagnostics = append(diagnostics, DramaProgramDiagnostic{Field: "timeline", Reason: "timeline_limit", Severity: "error"})
		return []model.DramaManifestV2Clip{}, 0, false, diagnostics
	}
	if timeline.hasColdClips() {
		return []model.DramaManifestV2Clip{}, 0, false, diagnostics
	}
	return clips, int32(total), true, diagnostics
}

// resolveProgramAgainstTimeline 通过分镜主键定位条目，诊断无效条目但不丢弃它们。
func resolveProgramAgainstTimeline(src *dramaManifestV2Source, timeline dramaV2Timeline, clips []model.DramaManifestV2Clip, total int32, complete bool, diagnostics []DramaProgramDiagnostic) (model.DramaManifestV2Program, []DramaProgramDiagnostic) {
	if diagnostics == nil {
		diagnostics = []DramaProgramDiagnostic{}
	}
	program := src.program.ManifestProgram()
	program.Version = model.CurrentDramaProgramVersion
	if err := src.program.Validate(); err != nil {
		diagnostics = append(diagnostics, DramaProgramDiagnostic{Field: "program", Reason: "invalid_program", Severity: "error"})
	}
	segments := programSegmentsByID(src.rows)
	frozen := make(map[uint64]model.DramaManifestV2Clip, len(clips))
	for _, clip := range clips {
		frozen[clip.SegmentID] = clip
	}
	for i, cue := range src.program.Audio {
		if cue.Anchor == nil {
			if complete && cue.EndF != nil && *cue.EndF > total {
				diagnostics = append(diagnostics, cueDiagnostic(cue, "end_f", "cue_out_of_episode"))
			}
			continue
		}
		id := cue.Anchor.SegmentID
		resolved, reason := resolveProgramAnchor(cue, segments[id], timeline.bySegment[id], frozen[id], total, complete)
		program.Audio[i].DramaProgramAudioSource = resolved
		if reason != "" {
			diagnostics = append(diagnostics, cueDiagnostic(cue, "anchor", reason))
		}
	}
	return program, diagnostics
}

// resolveProgramAnchor 检查源指纹和排他剪辑出点，再将源帧映射到母版。
func resolveProgramAnchor(cue model.DramaProgramAudioCue, row model.DramaSegment, state dramaV2ClipState, clip model.DramaManifestV2Clip, total int32, complete bool) (model.DramaProgramAudioSource, string) {
	audio := cue.DramaProgramAudioSource
	if reason := programAnchorIssue(cue, row, state); reason != "" {
		return audio, reason
	}
	if !complete || clip.SegmentID == 0 || cue.LengthF == nil {
		return audio, ""
	}
	start := clip.MasterInF + (cue.Anchor.SourceF - clip.SourceInF)
	end := int64(start) + int64(*cue.LengthF)
	if start < 0 || end > int64(total) {
		return audio, "cue_out_of_episode"
	}
	audio.StartF, audio.EndF = &start, new(int32(end))
	return audio, ""
}

// programAnchorIssue 先判断已知的缺失、换源及裁剪错误，冷素材不遮挡这些错误。
func programAnchorIssue(cue model.DramaProgramAudioCue, row model.DramaSegment, state dramaV2ClipState) string {
	switch {
	case row.ID == 0:
		return "anchor_segment_missing"
	case dramaVisualKind(row) != model.DramaVisualVideo:
		return "anchor_non_video"
	case row.ChosenTaskID == nil || state.clip.AssetID == 0:
		return "anchor_segment_unready"
	case cue.BoundSource == nil || *cue.BoundSource != clipSource(state.clip):
		return "anchor_source_replaced"
	}
	frame := cue.Anchor.SourceF
	if frame < row.SourceInF || row.SourceOutF != nil && frame >= *row.SourceOutF {
		return "anchor_frame_trimmed"
	}
	if state.failed {
		return "anchor_segment_unready"
	}
	if !state.cold && frame >= state.clip.SourceOutF {
		return "anchor_frame_trimmed"
	}
	return ""
}
