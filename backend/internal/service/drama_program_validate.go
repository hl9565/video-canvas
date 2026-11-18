package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/provider"
	"video-canvas/internal/repository"
)

// programAnchorBinding 记录本次新绑定时验证过的源、剪辑区间及规范化帧数。
type programAnchorBinding struct {
	index      int
	segmentID  uint64
	source     model.DramaProgramBoundSource
	frames     int32
	sourceInF  int32
	sourceOutF int32
}

type programAssetReader func(context.Context, uint64, uint64) (*model.Asset, error)

// programAsset 从素材服务读取归属与类型，节目单校验无需打开音频内容。
func (s *DramaService) programAsset(ctx context.Context, user, id uint64) (*model.Asset, error) {
	store, ok := s.assets.(provider.AssetStore)
	if !ok {
		return nil, errcode.ErrInternal
	}
	return store.Get(ctx, user, id)
}

// readProgramAudio 汇总所有无效的音频引用，同一素材只需读取一次。
func readProgramAudio(ctx context.Context, read programAssetReader, user uint64, p model.DramaProgram) (map[uint64]string, []DramaProgramDiagnostic) {
	hashes := make(map[uint64]string)
	unavailable := make(map[uint64]bool)
	diagnostics := []DramaProgramDiagnostic{}
	for _, cue := range p.Audio {
		if _, ok := hashes[cue.AssetID]; ok {
			continue
		}
		if !unavailable[cue.AssetID] {
			asset, err := read(ctx, user, cue.AssetID)
			if err == nil && validProgramAsset(asset, user, cue.AssetID, model.KindAudio) {
				hashes[cue.AssetID] = asset.Sha256
				continue
			}
			unavailable[cue.AssetID] = true
		}
		diagnostics = append(diagnostics, cueDiagnostic(cue, "asset_id", "media_unavailable"))
	}
	return hashes, diagnostics
}

// validProgramAsset 防止素材类型、归属或身份不匹配。
func validProgramAsset(a *model.Asset, user, id uint64, kind string) bool {
	return a != nil && a.ID == id && a.UserID == user && a.Kind == kind
}

// recheckProgramAudio 在持锁事务内重新读取素材；已变更的指纹必须重试。
func recheckProgramAudio(ctx context.Context, tx repository.DramaTx, user uint64, p model.DramaProgram, before map[uint64]string) error {
	after, diagnostics := readProgramAudio(ctx, tx.ComposeAsset, user, p)
	if len(diagnostics) != 0 {
		return programDiagnosticError(errcode.ErrDramaMediaUnavailable, diagnostics)
	}
	for id, hash := range before {
		if after[id] != hash {
			return errcode.ErrDramaManifestStale
		}
	}
	return nil
}

// cueDiagnostic 投影条目错误；分镜 ID 只暴露公开编码。
func cueDiagnostic(cue model.DramaProgramAudioCue, field, reason string) DramaProgramDiagnostic {
	d := DramaProgramDiagnostic{CueID: cue.ID, Field: field, Reason: reason, Severity: "error"}
	if cue.Anchor != nil {
		d.SegmentID = idcodec.ID(cue.Anchor.SegmentID)
	}
	return d
}

// programDiagnosticError 保留调用端可定位和修复的诊断。
func programDiagnosticError(code *errcode.Error, diagnostics []DramaProgramDiagnostic) error {
	return code.WithData(DramaProgramDiagnosticsData{Diagnostics: diagnostics})
}

// prepareProgramSave 扫描音频引用并仅为新增、改坐标或显式重绑的条目探测源视频。
func (s *DramaService) prepareProgramSave(ctx context.Context, p *model.DramaProject, ep *model.DramaEpisode, program model.DramaProgram, rebindIDs []string) ([]programAnchorBinding, map[uint64]string, error) {
	audio, diagnostics := readProgramAudio(ctx, s.programAsset, p.UserID, program)
	if len(diagnostics) != 0 {
		return nil, nil, programDiagnosticError(errcode.ErrDramaMediaUnavailable, diagnostics)
	}
	prev, err := model.DecodeStoredDramaProgram(json.RawMessage(ep.ProgramJSON))
	if err != nil {
		return nil, nil, err
	}
	rebind := make(map[string]bool, len(rebindIDs))
	for _, id := range rebindIDs {
		rebind[id] = true
	}
	prevByID := make(map[string]model.DramaProgramAudioCue, len(prev.Audio))
	for _, cue := range prev.Audio {
		prevByID[cue.ID] = cue
	}
	toBind := programAnchorChanges(program, prevByID, rebind)
	if len(toBind) == 0 {
		return nil, audio, nil
	}
	reader, ok := s.repo.(dramaManifestReader)
	if !ok {
		return nil, nil, errcode.ErrInternal
	}
	rows, err := reader.Segments(ctx, p.UserID, ep.ID)
	if err != nil {
		return nil, nil, dramaError(err)
	}
	bindings, err := s.probeProgramAnchors(ctx, reader, p, program, toBind, programSegmentsByID(rows), prevByID, rebind)
	return bindings, audio, err
}

// programAnchorChanges 保留同坐标的旧绑定，包括缺失绑定；只返回需要验证新来源的项。
func programAnchorChanges(program model.DramaProgram, prevByID map[string]model.DramaProgramAudioCue, rebind map[string]bool) []int {
	toBind := []int{}
	for i := range program.Audio {
		cue := &program.Audio[i]
		cue.BoundSource = nil
		if cue.Anchor == nil {
			continue
		}
		old := prevByID[cue.ID]
		if !rebind[cue.ID] && old.Anchor != nil && *old.Anchor == *cue.Anchor {
			cue.BoundSource = old.BoundSource
			continue
		}
		toBind = append(toBind, i)
	}
	return toBind
}

// programSegmentsByID 为锚点建立稳定的分镜主键索引。
func programSegmentsByID(rows []model.DramaSegment) map[uint64]model.DramaSegment {
	index := make(map[uint64]model.DramaSegment, len(rows))
	for _, row := range rows {
		index[row.ID] = row
	}
	return index
}

// probeProgramAnchors 对同一目标只探测一次，再校验旧来源和各条目的排他出点。
func (s *DramaService) probeProgramAnchors(ctx context.Context, reader dramaManifestReader, p *model.DramaProject, program model.DramaProgram, indices []int, rows map[uint64]model.DramaSegment, prevByID map[string]model.DramaProgramAudioCue, rebind map[string]bool) ([]programAnchorBinding, error) {
	cache := make(map[uint64]programAnchorBinding)
	errorsBySegment := make(map[uint64]string)
	bindings := make([]programAnchorBinding, 0, len(indices))
	diagnostics := []DramaProgramDiagnostic{}
	for _, index := range indices {
		cue := &program.Audio[index]
		id := cue.Anchor.SegmentID
		if _, ok := errorsBySegment[id]; !ok {
			cache[id], errorsBySegment[id] = s.probeProgramAnchor(ctx, reader, p, rows[id])
		}
		binding, reason := cache[id], errorsBySegment[id]
		if reason == "" {
			reason = validateProbedProgramAnchor(*cue, prevByID[cue.ID], binding, rebind[cue.ID])
		}
		if reason != "" {
			diagnostics = append(diagnostics, cueDiagnostic(*cue, "anchor", reason))
			continue
		}
		binding.index = index
		cue.BoundSource = &binding.source
		bindings = append(bindings, binding)
	}
	if len(diagnostics) != 0 {
		return nil, programDiagnosticError(errcode.ErrDramaAnchorInvalid, diagnostics)
	}
	return bindings, nil
}

// validateProbedProgramAnchor 在同一分镜内移动帧坐标时保留原来源，除非请求明确重绑。
func validateProbedProgramAnchor(cue, old model.DramaProgramAudioCue, binding programAnchorBinding, rebind bool) string {
	if !rebind && old.Anchor != nil && old.BoundSource != nil &&
		old.Anchor.SegmentID == cue.Anchor.SegmentID && binding.source != *old.BoundSource {
		return "anchor_source_replaced"
	}
	if !programFrameInTrim(cue.Anchor.SourceF, binding.sourceInF, binding.sourceOutF) {
		return "anchor_frame_trimmed"
	}
	return ""
}

// probeProgramAnchor 先检查采用来源，再在事务外读取规范化帧数。
func (s *DramaService) probeProgramAnchor(ctx context.Context, reader dramaManifestReader, p *model.DramaProject, row model.DramaSegment) (programAnchorBinding, string) {
	if row.ID == 0 {
		return programAnchorBinding{}, "anchor_segment_missing"
	}
	if dramaVisualKind(row) != model.DramaVisualVideo {
		return programAnchorBinding{}, "anchor_non_video"
	}
	assetID, err := programChosenVideo(ctx, reader, p, &row)
	if err != nil {
		return programAnchorBinding{}, "anchor_segment_unready"
	}
	asset, probe, err := s.dramaManifestProbe(ctx, p.UserID, assetID)
	if err != nil {
		return programAnchorBinding{}, "anchor_segment_unready"
	}
	in, out, ok := programTrim(row, probe.NormalizedFrames24)
	if !ok {
		return programAnchorBinding{}, "anchor_frame_trimmed"
	}
	return programAnchorBinding{segmentID: row.ID, frames: probe.NormalizedFrames24, sourceInF: in, sourceOutF: out,
		source: model.DramaProgramBoundSource{TaskID: *row.ChosenTaskID, AssetID: asset.ID, Sha256: asset.Sha256}}, ""
}

// programChosenVideo 复用候选准入，拒绝未采用、不可用或被 QC 驳回的 take。
func programChosenVideo(ctx context.Context, reader dramaManifestReader, p *model.DramaProject, row *model.DramaSegment) (uint64, error) {
	if row.ChosenTaskID == nil {
		return 0, errcode.ErrDramaTakeInvalid
	}
	takes, err := ResolveSegmentTakes(ctx, reader, manifestTakeAssets{reader}, p, row)
	if err != nil {
		return 0, err
	}
	take, err := findUsableSegmentTake(takes, *row.ChosenTaskID, false)
	if err != nil {
		return 0, err
	}
	return take.Asset.ID, nil
}

// programTrim 解析 EOF，并校验剪辑区间属于规范化源视频。
func programTrim(row model.DramaSegment, frames int32) (int32, int32, bool) {
	end := frames
	if row.SourceOutF != nil {
		end = *row.SourceOutF
	}
	return row.SourceInF, end, row.SourceInF >= 0 && end > row.SourceInF && end <= frames
}

// programFrameInTrim 使用左闭右开区间，排除剪辑出点所在帧。
func programFrameInTrim(frame, in, out int32) bool { return frame >= in && frame < out }

// lockProgramAnchorRows 按主键锁定本次绑定的分镜。
func lockProgramAnchorRows(ctx context.Context, tx repository.DramaTx, user uint64, bindings []programAnchorBinding) (map[uint64]model.DramaSegment, error) {
	if len(bindings) == 0 {
		return nil, nil
	}
	ids := make([]uint64, 0, len(bindings))
	for _, b := range bindings {
		ids = append(ids, b.segmentID)
	}
	slices.Sort(ids)
	rows, err := tx.LockSegments(ctx, user, slices.Compact(ids))
	if errors.Is(err, repository.ErrNotFound) {
		return nil, errcode.ErrDramaManifestStale
	}
	if err != nil {
		return nil, err
	}
	return programSegmentsByID(rows), nil
}

// recheckProgramAnchors 仅使用锁内元数据和锁外验证过的帧数，禁止重新探测。
func recheckProgramAnchors(ctx context.Context, tx repository.DramaTx, p *model.DramaProject, epID uint64, program model.DramaProgram, bindings []programAnchorBinding, byID map[uint64]model.DramaSegment) error {
	verified := make(map[uint64]bool, len(byID))
	for _, b := range bindings {
		row := byID[b.segmentID]
		if !verified[b.segmentID] && !recheckProgramAnchorSource(ctx, tx, p, epID, row, b) {
			return errcode.ErrDramaManifestStale
		}
		verified[b.segmentID] = true
		if !programFrameInTrim(program.Audio[b.index].Anchor.SourceF, b.sourceInF, b.sourceOutF) {
			return errcode.ErrDramaManifestStale
		}
	}
	return nil
}

// recheckProgramAnchorSource 比对采用任务、素材指纹及锁定后的实际剪辑区间。
func recheckProgramAnchorSource(ctx context.Context, tx repository.DramaTx, p *model.DramaProject, epID uint64, row model.DramaSegment, b programAnchorBinding) bool {
	if row.ID == 0 || row.EpisodeID != epID || dramaVisualKind(row) != model.DramaVisualVideo || row.ChosenTaskID == nil || *row.ChosenTaskID != b.source.TaskID {
		return false
	}
	in, out, ok := programTrim(row, b.frames)
	if !ok || in != b.sourceInF || out != b.sourceOutF {
		return false
	}
	id, err := programChosenVideo(ctx, tx, p, &row)
	if err != nil || id != b.source.AssetID {
		return false
	}
	asset, err := tx.ComposeAsset(ctx, p.UserID, id)
	return err == nil && validProgramAsset(asset, p.UserID, id, model.KindVideo) && asset.Sha256 == b.source.Sha256
}
