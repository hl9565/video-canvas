package service

import (
	"context"
	"encoding/json"
	"errors"
	"math"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/repository"
)

// matchesExpectedTask 把显式的零任务视为“尚未采用”，拒绝对过期采用关系编辑。
func matchesExpectedTask(row *model.DramaSegment, req *model.UpdateDramaSegmentReq) bool {
	if req.ExpectedTaskID == nil {
		return true
	}
	if row.ChosenTaskID == nil {
		return *req.ExpectedTaskID == 0
	}
	return *row.ChosenTaskID == uint64(*req.ExpectedTaskID)
}

// updateSegmentTake 在锁内复用统一准入；显式清空持久化阻止以后自动采用。
func (s *DramaSegmentService) updateSegmentTake(ctx context.Context, tx repository.DramaTx, p *model.DramaProject, row *model.DramaSegment, req *model.UpdateDramaSegmentReq, update segmentVisualUpdate) error {
	if req.ClearChosen {
		if row.ChosenTaskID != nil {
			repository.ResetSegmentEdit(row)
		}
		row.ChosenTaskID, row.ChosenCleared = nil, true
		return nil
	}
	if req.ChosenTaskID == nil {
		return nil
	}
	assets := s.assets
	if _, ok := tx.(dramaProgramAssetLocker); ok {
		assets = lockedSegmentTakeAssets{tx: tx}
	}
	takes, err := ResolveSegmentTakes(ctx, tx, assets, p, row)
	if err != nil {
		return err
	}
	take, err := findUsableSegmentTake(takes, uint64(*req.ChosenTaskID), false)
	if err != nil {
		return err
	}
	if err := recheckSegmentVideoOutput(take, update); err != nil {
		return err
	}
	if _, err := PromoteSegmentVideoPendingRun(ctx, tx, p, row, take); err != nil {
		return err
	}
	adoptSegmentTake(row, take.Task.ID, req.ResetEditOnTaskChange)
	return nil
}

// lockedSegmentTakeAssets 采用只需媒体身份，用当前事务读取而非另开连接。
type lockedSegmentTakeAssets struct{ tx repository.DramaTx }

// View 返回采用准入所需的素材身份。
func (a lockedSegmentTakeAssets) View(ctx context.Context, user, id uint64) (*model.AssetView, error) {
	row, err := a.tx.ComposeAsset(ctx, user, id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, errcode.ErrAssetNotFound
	}
	if err != nil {
		return nil, err
	}
	return &model.AssetView{ID: row.ID, Kind: row.Kind}, nil
}

// adoptSegmentTake 默认换镜清空六个精裁字段，人工采用不受自动采用条件限制。
func adoptSegmentTake(row *model.DramaSegment, id uint64, reset *bool) {
	changed := row.ChosenTaskID == nil || *row.ChosenTaskID != id
	if changed && (reset == nil || *reset) {
		repository.ResetSegmentEdit(row)
	}
	row.ChosenTaskID, row.ChosenCleared = &id, false
}

// applySegmentTextUpdate 合并提示词、时长和转写稿，与媒体编辑独立。
func applySegmentTextUpdate(row *model.DramaSegment, req *model.UpdateDramaSegmentReq) error {
	if req.Prompt != nil {
		row.Prompt = *req.Prompt
	}
	if req.Seconds != nil {
		row.Seconds = *req.Seconds
	}
	if req.Adaptation != nil {
		if err := editAdaptation(row, req.Adaptation); err != nil {
			return err
		}
	}

	return nil
}

// applySegmentEdit 逐字段应用补丁，显式 source_out_f:null 恢复到素材 EOF。
func applySegmentEdit(row *model.DramaSegment, patch *model.DramaSegmentEditPatch) {
	if patch.SourceInF != nil {
		row.SourceInF = *patch.SourceInF
	}
	if patch.SourceOutF.Set {
		row.SourceOutF = patch.SourceOutF.Value
	}
	if patch.MuteOriginal != nil {
		row.MuteOriginal = *patch.MuteOriginal
	}
	if patch.AudioGainDB != nil {
		row.AudioGainDB = *patch.AudioGainDB
	}
	if patch.NativeIsDialogue != nil {
		row.NativeIsDialogue = *patch.NativeIsDialogue
	}
	if patch.AvPtsOffsetMs != nil {
		row.AvPtsOffsetMs = *patch.AvPtsOffsetMs
	}
}

// validateSegmentEdit 校验右端排他的源帧区间、有限增益与音视频偏移。
func validateSegmentEdit(row *model.DramaSegment, frames *int32) error {
	if frames == nil || *frames <= 0 {
		return errcode.ErrDramaEditInvalid.WithMsg("编辑需要有效的源视频帧数")
	}
	out := *frames
	if row.SourceOutF != nil {
		out = *row.SourceOutF
	}
	if row.SourceInF < 0 || row.SourceInF >= out || out > *frames {
		return errcode.ErrDramaEditInvalid
	}
	if math.IsNaN(row.AudioGainDB) || math.IsInf(row.AudioGainDB, 0) || row.AudioGainDB < -60 || row.AudioGainDB > 12 {
		return errcode.ErrDramaEditInvalid
	}
	// 对齐数据库 numeric(5,2) 精度，避免如 0.004 在入库舍入为 0.00 后每次比较都误判变化并递增版本。
	row.AudioGainDB = normalizeAudioGainDB(row.AudioGainDB)
	if row.AvPtsOffsetMs < -10000 || row.AvPtsOffsetMs > 10000 {
		return errcode.ErrDramaEditInvalid
	}
	return nil
}

// normalizeAudioGainDB 将音频增益规整到数据库 numeric(5,2) 对应的两位小数并消除 -0.0。
func normalizeAudioGainDB(v float64) float64 {
	rounded := math.Round(v*100) / 100
	if rounded == 0 {
		return 0
	}
	return rounded
}

// segmentVisualUpdate 记录目标视觉和锁外校验过的素材快照。
type segmentVisualUpdate struct {
	kind         string
	fx           model.DramaSegmentFX
	stillID      *uint64
	duration     *int32
	asset        *model.Asset
	videoTaskID  *uint64
	videoAssetID uint64
	frames       *int32
}

// planSegmentVisual 确定目标模式，再校验该模式允许的字段和特效。
func planSegmentVisual(row *model.DramaSegment, req *model.UpdateDramaSegmentReq) (segmentVisualUpdate, error) {
	current := dramaVisualKind(*row)
	u := segmentVisualUpdate{kind: current}
	if req.VisualKind != nil {
		u.kind = *req.VisualKind
	}
	switch u.kind {
	case model.DramaVisualVideo, model.DramaVisualBlack, model.DramaVisualStill:
	default:
		return u, errcode.ErrDramaVisualInvalid.WithMsg("visual_kind 必须为 video、still 或 black")
	}
	var err error
	u.fx, err = targetSegmentFX(row, req, current, u.kind)
	if err != nil {
		return u, err
	}
	if u.kind == model.DramaVisualVideo {
		return u, validateVideoVisualPatch(current, req)
	}
	u.stillID, err = syntheticStillID(row, req, current, u.kind)
	if err != nil {
		return u, err
	}
	u.duration, err = syntheticDuration(row, req, current)
	if err != nil {
		return u, err
	}
	return u, DramaContractError(u.fx.ValidateForClip(u.kind, *u.duration))
}

// targetSegmentFX 保留兼容的旧特效，只有视觉模式切换时清除已不适用的字段。
func targetSegmentFX(row *model.DramaSegment, req *model.UpdateDramaSegmentReq, current, target string) (model.DramaSegmentFX, error) {
	if req.FX != nil {
		return *req.FX, DramaContractError(req.FX.ValidateForClip(target, 0))
	}
	fx, err := model.DecodeDramaSegmentFX(row.FXJSON)
	if err != nil {
		return fx, DramaContractError(err)
	}
	if current != target && target != model.DramaVisualStill {
		fx.ZoomStart, fx.ZoomEnd, fx.ShakePx, fx.CropY, fx.Seed = nil, nil, nil, nil, nil
	}
	if current != target && target != model.DramaVisualVideo {
		fx.NativeFadeInF, fx.NativeFadeOutF = 0, 0
	}
	return fx, DramaContractError(fx.ValidateForClip(target, 0))
}

// validateVideoVisualPatch 禁止把合成片段的持续帧数当作视频素材的裁剪设置。
func validateVideoVisualPatch(current string, req *model.UpdateDramaSegmentReq) error {
	if req.StillAssetID.Set && req.StillAssetID.Value != nil {
		return errcode.ErrDramaVisualInvalid.WithMsg("视频不能指定静帧素材")
	}
	if current != model.DramaVisualVideo && req.ResetEditOnTaskChange != nil && !*req.ResetEditOnTaskChange {
		return errcode.ErrDramaVisualInvalid.WithMsg("切回视频时必须重置合成片段的裁剪")
	}
	if req.Edit != nil && (req.ClearChosen || current != model.DramaVisualVideo && req.ChosenTaskID == nil) {
		return errcode.ErrDramaEditInvalid.WithMsg("编辑需要采用视频镜头")
	}
	return nil
}

// syntheticStillID 黑场不能引用素材；静帧必须引用有效的十进制主键。
func syntheticStillID(row *model.DramaSegment, req *model.UpdateDramaSegmentReq, current, target string) (*uint64, error) {
	if req.ChosenTaskID != nil {
		return nil, errcode.ErrDramaVisualInvalid.WithMsg("静帧和黑场不能采用视频镜头")
	}
	if target == model.DramaVisualBlack {
		if req.StillAssetID.Set && req.StillAssetID.Value != nil {
			return nil, errcode.ErrDramaVisualInvalid.WithMsg("黑场不能指定静帧素材")
		}
		return nil, nil
	}
	var id *uint64
	if req.StillAssetID.Set {
		id = req.StillAssetID.Value
	} else if current == model.DramaVisualStill {
		id = row.StillAssetID
	}
	if id == nil || *id == 0 || *id > math.MaxInt64 {
		return nil, errcode.ErrDramaVisualInvalid.WithMsg("静帧必须指定有效的 still_asset_id")
	}
	return id, nil
}

// syntheticDuration 使用显式帧数；已有合成片段未指定新时长时保留原值。
func syntheticDuration(row *model.DramaSegment, req *model.UpdateDramaSegmentReq, current string) (*int32, error) {
	var out *int32
	if current != model.DramaVisualVideo {
		out = row.SourceOutF
	}
	if p := req.Edit; p != nil {
		if p.SourceInF != nil && *p.SourceInF != 0 {
			return nil, errcode.ErrDramaVisualInvalid.WithMsg("静帧和黑场的 source_in_f 必须为 0")
		}
		if p.MuteOriginal != nil || p.AudioGainDB != nil || p.NativeIsDialogue != nil || p.AvPtsOffsetMs != nil {
			return nil, errcode.ErrDramaVisualInvalid.WithMsg("静帧和黑场没有原声")
		}
		if p.SourceOutF.Set {
			out = p.SourceOutF.Value
		}
	}
	if out == nil || *out <= 0 || *out > model.DramaProgramMaxFrames {
		return nil, errcode.ErrDramaVisualInvalid.WithMsg("静帧和黑场的 source_out_f 必须是 1 到 14400 的整数")
	}
	return out, nil
}

// checkSegmentVisualRatio 只限制 Stage C 的作者操作，允许普通视频编辑和清除特效。
func checkSegmentVisualRatio(ratio string, req *model.UpdateDramaSegmentReq) error {
	if ratio == dramaStageCRatio {
		return nil
	}
	if (req.VisualKind != nil && *req.VisualKind != model.DramaVisualVideo) ||
		(req.StillAssetID.Set && req.StillAssetID.Value != nil) ||
		(req.FX != nil && *req.FX != (model.DramaSegmentFX{})) {
		return errcode.ErrDramaVisualInvalid.WithMsg("静帧、黑场和特效仅支持 16:9 项目")
	}
	return nil
}

// applyLockedSegmentUpdate 锁内重验目标媒体，再把模式、裁剪、原声、特效和采用关系一起更改。
func (s *DramaSegmentService) applyLockedSegmentUpdate(ctx context.Context, tx repository.DramaTx, project *model.DramaProject, row *model.DramaSegment, req *model.UpdateDramaSegmentReq, u segmentVisualUpdate) error {
	if err := checkSegmentVisualRatio(project.Ratio, req); err != nil {
		return err
	}
	if u.kind != model.DramaVisualVideo {
		if err := checkSyntheticFrameRun(ctx, tx, row); err != nil {
			return err
		}
	}
	if err := s.recheckSegmentUpdateAsset(ctx, tx, row.UserID, u.asset); err != nil {
		return err
	}
	if err := applySegmentTextUpdate(row, req); err != nil {
		return err
	}
	if err := recheckKeptSegmentVideo(ctx, tx, row, req, u); err != nil {
		return err
	}
	wasVideo := dramaVisualKind(*row) == model.DramaVisualVideo
	row.VisualKind, row.StillAssetID = u.kind, u.stillID
	raw, err := json.Marshal(u.fx)
	if err != nil {
		return err
	}
	row.FXJSON = raw
	if u.kind != model.DramaVisualVideo {
		repository.ResetSegmentEdit(row)
		row.SourceOutF = u.duration
		row.ChosenTaskID, row.ChosenCleared = nil, true
		return nil
	}
	if !wasVideo {
		repository.ResetSegmentEdit(row)
		row.ChosenTaskID, row.ChosenCleared = nil, true
	}
	if err := s.updateSegmentTake(ctx, tx, project, row, req, u); err != nil {
		return err
	}
	return applyVideoVisualEdit(row, req, u, wasVideo)
}

// checkSyntheticFrameRun 锁定现有活跃流程；历史终态不参与视觉模式互斥。
func checkSyntheticFrameRun(ctx context.Context, tx repository.DramaTx, row *model.DramaSegment) error {
	_, err := tx.ActiveFrameRun(ctx, row.UserID, row.ID, true)
	if err == nil {
		return errcode.ErrDramaFrameRunConflict
	}
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	return err
}

// applyVideoVisualEdit 使用最终采用及裁剪后的长度校验视频编辑和特效。
func applyVideoVisualEdit(row *model.DramaSegment, req *model.UpdateDramaSegmentReq, u segmentVisualUpdate, wasVideo bool) error {
	if req.Edit != nil {
		applySegmentEdit(row, req.Edit)
	}
	if needsVideoEditFrames(req) || (!wasVideo && row.ChosenTaskID != nil) {
		if err := validateSegmentEdit(row, u.frames); err != nil {
			return err
		}
	}
	return validateVideoFXBounds(row, u.fx, u.frames)
}

// needsVideoEditFrames 表示用户在编辑剪辑边界或明确保留旧边界换镜。
func needsVideoEditFrames(req *model.UpdateDramaSegmentReq) bool {
	return req.Edit != nil || (req.ChosenTaskID != nil && req.ResetEditOnTaskChange != nil && !*req.ResetEditOnTaskChange)
}

// validateVideoFXBounds 使用实际剪辑长度；已清空的镜头暂以计划时长为上限。
func validateVideoFXBounds(row *model.DramaSegment, fx model.DramaSegmentFX, frames *int32) error {
	if !fx.HasFrameEffects() {
		return nil
	}
	if row.ChosenTaskID == nil {
		if !row.ChosenCleared {
			return errcode.ErrDramaVisualInvalid.WithMsg("设置视频淡入淡出或闪帧前须先采用一个镜头")
		}
		return DramaContractError(fx.ValidateForClip(model.DramaVisualVideo, int32(min(max(row.Seconds, 1), model.DramaProgramMaxFrames/24))*24))
	}
	if frames == nil || *frames <= 0 {
		return errcode.ErrDramaVisualInvalid.WithMsg("校验特效需要有效的源视频帧数")
	}
	out := *frames
	if row.SourceOutF != nil {
		out = *row.SourceOutF
	}
	if row.SourceInF < 0 || out <= row.SourceInF || out > *frames {
		return errcode.ErrDramaVisualInvalid.WithMsg("特效需要有效的视频裁剪区间")
	}
	return DramaContractError(fx.ValidateForClip(model.DramaVisualVideo, out-row.SourceInF))
}

// needsTakeSelectionFrames 保留裁剪或特效时需要目标视频的实际帧数。
func needsTakeSelectionFrames(seg *model.DramaSegment, reset *bool) bool {
	return reset != nil && !*reset || dramaSegmentHasFX(*seg)
}

// validateTakeSelectionEdit 防止导入采用及 QC 替换使裁剪或已保存的特效超过实际镜头长度。
func validateTakeSelectionEdit(seg *model.DramaSegment, reset *bool, frames *int32) error {
	if reset != nil && !*reset && frames != nil {
		if err := validateSegmentEdit(seg, frames); err != nil {
			return err
		}
	}
	fx, err := model.DecodeDramaSegmentFX(seg.FXJSON)
	if err != nil {
		return DramaContractError(err)
	}
	return validateVideoFXBounds(seg, fx, frames)
}
