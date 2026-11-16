package service

import (
	"context"
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

// updateSegmentTake 在锁内校验同画布本段成功任务，默认换镜时清空六个精裁字段。
func updateSegmentTake(ctx context.Context, tx repository.DramaTx, p *model.DramaProject, row *model.DramaSegment, req *model.UpdateDramaSegmentReq) error {
	if req.ChosenTaskID == nil {
		return nil
	}
	tasks, err := tx.SegmentTasks(ctx, row.UserID, p.CanvasID, segmentNode(row.ID))
	if err != nil {
		return err
	}
	id := uint64(*req.ChosenTaskID)
	if !validSegmentTake(tasks, id) {
		return errcode.ErrDramaTakeInvalid
	}
	changed := row.ChosenTaskID == nil || *row.ChosenTaskID != id
	if changed && (req.ResetEditOnTaskChange == nil || *req.ResetEditOnTaskChange) {
		repository.ResetSegmentEdit(row)
	}
	row.ChosenTaskID = &id
	return nil
}

// applySegmentUpdate 合并部分更新；编辑边界统一使用目标素材规范化后的精确帧数。
func applySegmentUpdate(row *model.DramaSegment, req *model.UpdateDramaSegmentReq, frames *int32) error {
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
	if req.Edit != nil {
		applySegmentEdit(row, req.Edit)
	}
	if req.Edit != nil || (req.ChosenTaskID != nil && req.ResetEditOnTaskChange != nil && !*req.ResetEditOnTaskChange) {
		return validateSegmentEdit(row, frames)
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
