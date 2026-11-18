package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/jsonhash"
	"video-canvas/internal/repository"
)

// estimateCompose 执行同一 tx1 和锁外清单校验，完全不访问模型或积分。
func (s *DramaService) estimateCompose(ctx context.Context, user, id uint64, req *model.DramaJobReq) (int, error) {
	intent, err := newDramaJobIntent(id, nil, req)
	if err != nil {
		return 0, err
	}
	source, hit, err := s.prepareComposeTx(ctx, user, id, intent)
	if err != nil || hit != nil {
		return 0, dramaError(err)
	}
	_, err = s.freezeComposeManifest(ctx, source, intent.req.Compose)
	return 0, dramaError(err)
}

// createComposeJob 分离短事务和媒体探测，tx2 再校验全部冻结条件后只入队一次。
func (s *DramaService) createComposeJob(ctx context.Context, user, id uint64, intent *dramaJobIntent) (*model.DramaJobView, error) {
	// 1. 项目锁内联合查键，再校验集状态和请求参数。
	source, hit, err := s.prepareComposeTx(ctx, user, id, intent)
	if err != nil {
		return nil, dramaError(err)
	}
	if hit != nil {
		v := jobView(*hit)
		return &v, nil
	}
	// 2. 释放全部写锁后下载、探测并生成冻结清单。
	input, err := s.freezeComposeManifest(ctx, source, intent.req.Compose)
	if err != nil {
		return nil, err
	}
	var job *model.DramaJob
	created := false
	err = s.repo.WithTx(ctx, func(tx repository.DramaTx) error {
		p, err := tx.Project(ctx, user, id, true)
		if err != nil {
			return err
		}
		job, err = resolveDramaJobIntent(ctx, tx, user, id, intent)
		if err != nil || job != nil {
			return err
		}
		if err := s.prepareAndRecheckCompose(ctx, tx, p, &intent.req, source, input); err != nil {
			return err
		}
		job, err = enqueueCompose(ctx, tx, p, intent, input)
		created = err == nil
		return err
	})
	if errors.Is(err, repository.ErrDuplicate) {
		return s.recoverDramaJob(ctx, user, id, intent)
	}
	if err != nil {
		return nil, dramaError(err)
	}
	if created {
		s.publish(ctx, job)
		logDramaCreated(ctx, job, nil)
	}
	v := jobView(*job)
	return &v, nil
}

// prepareAndRecheckCompose 在业务前置检查之前识别已删除或换号的 V2 集。
func (s *DramaService) prepareAndRecheckCompose(ctx context.Context, tx repository.DramaTx, p *model.DramaProject, req *model.DramaJobReq, source *dramaManifestSource, input *dramaComposeInput) error {
	if input.ManifestV2 != nil {
		if err := s.recheckCompose(ctx, tx, p, source.episode, source, input); err != nil {
			return err
		}
	}
	eps, err := s.prepareDramaJob(ctx, tx, p, req)
	if err != nil {
		return err
	}
	if input.ManifestV2 == nil {
		return s.recheckCompose(ctx, tx, p, eps[0], source, input)
	}
	if eps[0].ID != input.EpisodeID || eps[0].SegmentVersion != input.SegmentVersion {
		return errcode.ErrDramaManifestStale
	}
	return nil
}

// prepareComposeTx 是 tx1：所有重放先返回旧任务，避免探测或可变配置影响同意图重试。
func (s *DramaService) prepareComposeTx(ctx context.Context, user, id uint64, intent *dramaJobIntent) (*dramaManifestSource, *model.DramaJob, error) {
	var source *dramaManifestSource
	var hit *model.DramaJob
	err := s.repo.WithTx(ctx, func(tx repository.DramaTx) error {
		p, err := tx.Project(ctx, user, id, true)
		if err != nil {
			return err
		}
		hit, err = resolveDramaJobIntent(ctx, tx, user, id, intent)
		if err != nil || hit != nil {
			return err
		}
		eps, err := s.prepareDramaJob(ctx, tx, p, &intent.req)
		if err != nil {
			return err
		}
		if err = checkComposeVersion(eps[0], intent.req.Compose); err != nil {
			return err
		}
		source, err = readDramaRenderSource(ctx, tx, p, eps[0])
		if err != nil || source.stageC == nil {
			return err
		}
		source.stageC.composeSnapshot, err = composeV2Snapshot(eps[0], source.stageC.rows)
		return err
	})
	return source, hit, err
}

// readDramaRenderSource 保留旧合成的严格采用规则；Stage C 走独立的诊断清单。
func readDramaRenderSource(ctx context.Context, reader dramaManifestReader, p *model.DramaProject, ep model.DramaEpisode) (*dramaManifestSource, error) {
	rows, err := reader.Segments(ctx, p.UserID, ep.ID)
	if err != nil {
		return nil, err
	}
	program, err := model.DecodeStoredDramaProgram(json.RawMessage(ep.ProgramJSON))
	if err != nil {
		return nil, err
	}
	if !program.HasAuthoredContent() && !slices.ContainsFunc(rows, dramaSegmentUsesStageC) {
		return readDramaManifestSourceRows(ctx, reader, p, ep, rows)
	}
	stageC, err := readDramaManifestV2Source(ctx, reader, p, ep, rows, program)
	if err != nil {
		return nil, err
	}
	return &dramaManifestSource{project: *p, episode: ep, stageC: stageC}, nil
}

// checkComposeVersion 校验显式版本；旧 schema=0 无新参数仍使用旧整段输入。
func checkComposeVersion(ep model.DramaEpisode, c *model.DramaComposeReq) error {
	if c != nil && c.SegmentVersion != nil && *c.SegmentVersion != ep.SegmentVersion {
		return errcode.ErrDramaManifestStale
	}
	return nil
}

// freezeComposeManifest 只在新版输入上冻结清单；schema=0 的旧输入保留原 worker 行为。
func (s *DramaService) freezeComposeManifest(ctx context.Context, source *dramaManifestSource, c *model.DramaComposeReq) (*dramaComposeInput, error) {
	if source.stageC != nil {
		return s.freezeComposeManifestV2(ctx, source.stageC, c)
	}
	input := source.input
	if source.episode.EditSchema == 0 && emptyDramaCompose(c) {
		return &input, nil
	}
	view, err := s.resolveDramaManifestSource(ctx, source)
	if err != nil {
		return nil, err
	}
	if c != nil && c.ManifestHash != "" && c.ManifestHash != view.ManifestHash {
		return nil, errcode.ErrDramaManifestStale
	}
	input.AspectRatio = source.project.Ratio
	input.Manifest = &view.Manifest
	input.ManifestHash = view.ManifestHash
	setComposeOutputOptions(&input, c)
	return &input, nil
}

// setComposeOutputOptions 保持两版冻结导出的档位和变体默认值一致。
func setComposeOutputOptions(input *dramaComposeInput, c *model.DramaComposeReq) {
	input.Size = "720p"
	input.Variants = []string{"clean"}
	if c == nil {
		return
	}
	if c.Size != "" {
		input.Size = c.Size
	}
	if len(c.Variants) > 0 {
		input.Variants = canonicalDramaVariants(c.Variants)
	}
}

// freezeComposeManifestV2 在无写锁时解析节目单，冻结同一份用于哈希的内部清单。
func (s *DramaService) freezeComposeManifestV2(ctx context.Context, source *dramaManifestV2Source, c *model.DramaComposeReq) (*dramaComposeInput, error) {
	if err := checkDramaV2Ratio(source.project.Ratio); err != nil {
		return nil, err
	}
	view, err := s.resolveDramaManifestV2Source(ctx, source)
	if err != nil {
		return nil, err
	}
	if err := dramaV2ComposeDiagnostic(view.ProgramDiagnostics); err != nil {
		return nil, err
	}
	if c != nil && c.ManifestHash != "" && c.ManifestHash != view.ManifestHash {
		return nil, errcode.ErrDramaManifestStale
	}
	input := &dramaComposeInput{
		EpisodeID: source.episode.ID, SegmentVersion: source.episode.SegmentVersion,
		ManifestV2: view.frozen, RenderVersion: model.CurrentDramaManifestV2RenderVersion,
		ManifestHash: view.ManifestHash, AspectRatio: source.project.Ratio, SkippedSeqs: view.SkippedSeqs,
		AssetIDs: []uint64{},
	}
	for _, clip := range view.frozen.Clips {
		if clip.VisualKind == model.DramaVisualVideo {
			input.AssetIDs = append(input.AssetIDs, clip.AssetID)
		}
	}
	setComposeOutputOptions(input, c)
	return input, nil
}

// recheckCompose 是 tx2：项目→集→片段锁序下重验画幅、版本、采用和内容指纹，不进行媒体 I/O。
func (s *DramaService) recheckCompose(ctx context.Context, tx repository.DramaTx, p *model.DramaProject, ep model.DramaEpisode, source *dramaManifestSource, input *dramaComposeInput) error {
	if input.ManifestV2 != nil {
		return recheckComposeV2(ctx, tx, p, ep, source.stageC, input)
	}
	if input.Manifest != nil {
		if p.Ratio != input.AspectRatio {
			return errcode.ErrDramaManifestStale
		}
		locked, err := tx.LockEpisodes(ctx, p.UserID, []uint64{ep.ID})
		if err != nil {
			return err
		}
		if len(locked) != 1 {
			return repository.ErrNotFound
		}
		ep = locked[0]
		ids := make([]uint64, 0, len(source.rows))
		for _, row := range source.rows {
			ids = append(ids, row.ID)
		}
		if _, err = tx.LockSegments(ctx, p.UserID, ids); err != nil {
			return err
		}
	}
	if ep.ID != input.EpisodeID || ep.SegmentVersion != input.SegmentVersion {
		return errcode.ErrDramaManifestStale
	}
	current, err := readDramaRenderSource(ctx, tx, p, ep)
	if err != nil {
		return err
	}
	if current.stageC != nil || !slices.Equal(current.input.AssetIDs, input.AssetIDs) || !slices.Equal(current.input.SkippedSeqs, input.SkippedSeqs) {
		return errcode.ErrDramaManifestStale
	}
	before, err := composeMediaSignature(source.rows)
	if err != nil {
		return err
	}
	after, err := composeMediaSignature(current.rows)
	if err != nil {
		return err
	}
	if before != after {
		return errcode.ErrDramaManifestStale
	}
	if input.Manifest != nil {
		return recheckComposeHashes(ctx, tx, p.UserID, input)
	}
	return nil
}

// recheckComposeV2 在项目锁下再锁集及全部片段，防止发布锁外解析的旧快照。
func recheckComposeV2(ctx context.Context, tx repository.DramaTx, p *model.DramaProject, ep model.DramaEpisode, source *dramaManifestV2Source, input *dramaComposeInput) error {
	if p.Ratio != input.AspectRatio || ep.ID != input.EpisodeID || ep.SegmentVersion != input.SegmentVersion {
		return errcode.ErrDramaManifestStale
	}
	locked, err := tx.LockEpisodes(ctx, p.UserID, []uint64{ep.ID})
	if err != nil {
		return err
	}
	if len(locked) != 1 || locked[0].ID != input.EpisodeID || locked[0].SegmentVersion != input.SegmentVersion ||
		locked[0].DramaID != p.ID || locked[0].No != source.episode.No {
		return errcode.ErrDramaManifestStale
	}
	ep = locked[0]
	rows, err := lockComposeV2Segments(ctx, tx, ep)
	if err != nil {
		return err
	}
	snapshot, err := composeV2Snapshot(ep, rows)
	if err != nil || snapshot != source.composeSnapshot {
		return errcode.ErrDramaManifestStale
	}
	current, err := readDramaManifestV2Source(ctx, tx, p, ep, rows, source.program)
	if err != nil {
		return err
	}
	if !maps.Equal(current.videos, source.videos) {
		return errcode.ErrDramaManifestStale
	}
	return recheckComposeV2Hashes(ctx, tx, p.UserID, input)
}

// lockComposeV2Segments 读取当前成员后按主键加锁，集合变化也会使快照过期。
func lockComposeV2Segments(ctx context.Context, tx repository.DramaTx, ep model.DramaEpisode) ([]model.DramaSegment, error) {
	rows, err := tx.Segments(ctx, ep.UserID, ep.ID)
	if err != nil {
		return nil, err
	}
	ids := make([]uint64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	locked, err := tx.LockSegments(ctx, ep.UserID, ids)
	if err != nil {
		return nil, err
	}
	if len(locked) != len(ids) || slices.ContainsFunc(locked, func(row model.DramaSegment) bool { return row.EpisodeID != ep.ID }) {
		return nil, errcode.ErrDramaManifestStale
	}
	sortDramaManifestRows(locked)
	return locked, nil
}

// composeV2Snapshot 包含整个节目单（含私有绑定）及所有影响渲染的分镜字段。
func composeV2Snapshot(ep model.DramaEpisode, rows []model.DramaSegment) (string, error) {
	media, err := composeMediaSignature(rows)
	if err != nil {
		return "", err
	}
	type visual struct {
		Kind  string
		Still *uint64
		FX    json.RawMessage
	}
	visuals := make([]visual, 0, len(rows))
	for _, row := range rows {
		visuals = append(visuals, visual{row.VisualKind, row.StillAssetID, json.RawMessage(row.FXJSON)})
	}
	return jsonhash.CanonicalSHA256(struct {
		Program json.RawMessage
		Media   string
		Visuals []visual
	}{json.RawMessage(ep.ProgramJSON), media, visuals})
}

// recheckComposeHashes 事务内只读素材元数据，并核验冻结清单自身没有被改写。
func recheckComposeHashes(ctx context.Context, tx repository.DramaTx, user uint64, input *dramaComposeInput) error {
	for _, clip := range input.Manifest.Clips {
		asset, err := tx.ComposeAsset(ctx, user, clip.AssetID)
		if err != nil {
			return errcode.ErrDramaManifestStale
		}
		if asset.Kind != model.KindVideo || asset.Sha256 != clip.Sha256 {
			return errcode.ErrDramaManifestStale
		}
	}
	hash, err := jsonhash.CanonicalSHA256(input.Manifest)
	if err != nil {
		return err
	}
	if hash != input.ManifestHash {
		return errcode.ErrDramaManifestStale
	}
	return nil
}

// enqueueCompose 在 tx2 同事务写入请求签名、完整输入和唯一的零费用队列任务。
func enqueueCompose(ctx context.Context, tx repository.DramaTx, p *model.DramaProject, intent *dramaJobIntent, input *dramaComposeInput) (*model.DramaJob, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	job := &model.DramaJob{UserID: p.UserID, DramaID: p.ID, Lane: "user", Step: "compose", Episodes: intent.req.Episodes, Status: "queued", ConfigSnapshot: []byte(`{}`), InputJSON: raw, Output: []byte(`{}`), ApprovalID: intent.approvalID, RequestIntentSig: intent.signature}
	if intent.req.ComposeRequestKey != "" {
		key := intent.req.ComposeRequestKey
		job.ComposeRequestKey = &key
	}
	if err := tx.InsertJob(ctx, job); err != nil {
		return nil, err
	}
	return job, nil
}

// composeMediaSignature 只复核影响既有采用视频的字段，提示词或转写稿修订不能使媒体快照误报过期。
func composeMediaSignature(rows []model.DramaSegment) (string, error) {
	type mediaSegment struct {
		SegmentID    uint64
		Seq          int
		ChosenTaskID *uint64
		Edit         model.DramaSegmentEditView
	}
	media := make([]mediaSegment, 0, len(rows))
	for _, row := range rows {
		media = append(media, mediaSegment{SegmentID: row.ID, Seq: row.Seq, ChosenTaskID: row.ChosenTaskID, Edit: model.DramaSegmentEditView{
			SourceInF: row.SourceInF, SourceOutF: row.SourceOutF, MuteOriginal: row.MuteOriginal, AudioGainDB: row.AudioGainDB, NativeIsDialogue: row.NativeIsDialogue, AvPtsOffsetMs: row.AvPtsOffsetMs,
		}})
	}
	return jsonhash.CanonicalSHA256(media)
}

// freezeDramaV2Audio 校验排他源区间，并把探针结果加入清单指纹。
func freezeDramaV2Audio(src model.DramaProgram, program *model.DramaManifestV2Program, audio dramaV2AudioState) ([]DramaProgramDiagnostic, map[string]int64) {
	diagnostics := []DramaProgramDiagnostic{}
	missing := make(map[string]int64)
	for i, cue := range src.Audio {
		media := audio.media[cue.AssetID]
		if media.failed {
			diagnostics = append(diagnostics, cueDiagnostic(cue, "asset_id", "media_unavailable"))
			continue
		}
		if media.probe == nil {
			continue
		}
		p := media.probe
		if !validDramaManifestHash(media.sha256) || p.Sha256 != media.sha256 {
			diagnostics = append(diagnostics, cueDiagnostic(cue, "asset_id", "media_unavailable"))
			continue
		}
		frozen := &program.Audio[i]
		frozen.Sha256, frozen.DecodedAudioSamples, frozen.AStartUs = p.Sha256, p.DecodedAudioSamples, p.AStartUs
		if field := dramaAudioBoundsField(cue, p.DecodedAudioSamples); field != "" {
			diagnostics = append(diagnostics, cueDiagnostic(cue, field, "audio_range_out_of_bounds"))
			continue
		}
		if n := dramaNarrationOverflow(cue, p.DecodedAudioSamples); n > 0 {
			missing[cue.ID] = n
			diagnostics = append(diagnostics, cueDiagnostic(cue, "end_f", "narration_overflow"))
		}
	}
	return diagnostics, missing
}

// dramaAudioBoundsField 报告第一个超出已解码素材的裁剪端点。
func dramaAudioBoundsField(c model.DramaProgramAudioCue, decoded int64) string {
	if c.SrcInSample >= decoded {
		return "src_in_sample"
	}
	if c.SrcOutSample != nil && *c.SrcOutSample > decoded {
		return "src_out_sample"
	}
	return ""
}

// dramaNarrationOverflow 返回旁白需要的额外采样，负偏移会先消耗被裁掉的头部。
func dramaNarrationOverflow(c model.DramaProgramAudioCue, decoded int64) int64 {
	if c.Role != model.DramaAudioNarration || c.StartF == nil || c.EndF == nil || c.SrcInSample < 0 {
		return 0
	}
	out := decoded
	if c.SrcOutSample != nil {
		out = *c.SrcOutSample
	}
	window := (int64(*c.EndF) - int64(*c.StartF)) * model.DramaProgramSamplesPerFrame
	return (out - c.SrcInSample) - (window - int64(c.AvPtsOffsetMs)*48)
}

// dramaProgramDiagnosticMessages 为只显示文本的清单客户端提供定位及修复信息。
func dramaProgramDiagnosticMessages(src *dramaManifestV2Source, diagnostics []DramaProgramDiagnostic, missing map[string]int64) []string {
	messages := make([]string, 0, len(diagnostics))
	segments := programSegmentsByID(src.rows)
	for _, d := range diagnostics {
		label := "节目单"
		if d.CueID != "" {
			label = fmt.Sprintf("音频条目 %q", d.CueID)
		} else if row, ok := segments[uint64(d.SegmentID)]; ok {
			label = fmt.Sprintf("片段 %d", row.Seq)
		}
		message := dramaProgramDiagnosticMessage(d.Reason)
		if d.Reason == "narration_overflow" {
			message = fmt.Sprintf("旁白窗口不足，差 %d 个采样，请延长窗口或显式裁剪 src_out_sample", missing[d.CueID])
		}
		messages = append(messages, label+"："+message)
	}
	return messages
}

// dramaProgramDiagnosticMessage 提供不包含内部素材元数据的诊断文案。
func dramaProgramDiagnosticMessage(reason string) string {
	switch reason {
	case "media_unavailable":
		return "引用素材不可用或类型不符"
	case "anchor_segment_missing":
		return "锚定分镜已不存在"
	case "anchor_segment_unready":
		return "锚定分镜没有可用的采用视频"
	case "anchor_non_video":
		return "只能锚定视频分镜"
	case "anchor_source_replaced":
		return "锚定视频已变更，请明确重新绑定"
	case "anchor_frame_trimmed":
		return "锚定源帧已被裁掉"
	case "cue_out_of_episode":
		return "音频区间超出剧集时间轴"
	case "audio_range_out_of_bounds":
		return "源音频采样区间超出已解码范围"
	case "timeline_limit":
		return "最多支持 200 个分镜、14400 帧"
	case "visual_invalid":
		return "分镜视觉参数或裁剪区间无效"
	default:
		return "保存的节目单结构不合法"
	}
}

// dramaV2ComposeDiagnostic 将解析诊断转换为估算和提交使用的业务错误。
func dramaV2ComposeDiagnostic(diagnostics []DramaProgramDiagnostic) error {
	if len(diagnostics) == 0 {
		return nil
	}
	code := errcode.ErrDramaProgramInvalid
	switch {
	case slices.ContainsFunc(diagnostics, func(d DramaProgramDiagnostic) bool {
		return strings.HasPrefix(d.Reason, "anchor_") || d.Field == "anchor"
	}):
		code = errcode.ErrDramaAnchorInvalid
	case slices.ContainsFunc(diagnostics, func(d DramaProgramDiagnostic) bool { return d.Reason == "media_unavailable" }):
		code = errcode.ErrDramaMediaUnavailable
	case slices.ContainsFunc(diagnostics, func(d DramaProgramDiagnostic) bool { return d.Reason == "visual_invalid" }):
		code = errcode.ErrDramaVisualInvalid
	}
	return programDiagnosticError(code, diagnostics)
}
