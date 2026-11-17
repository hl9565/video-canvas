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

// updateSegmentTake 在锁内复用统一准入；显式清空持久化阻止以后自动采用。
func (s *DramaSegmentService) updateSegmentTake(ctx context.Context, tx repository.DramaTx, p *model.DramaProject, row *model.DramaSegment, req *model.UpdateDramaSegmentReq) error {
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
	takes, err := ResolveSegmentTakes(ctx, tx, s.assets, p, row)
	if err != nil {
		return err
	}
	take, err := findUsableSegmentTake(takes, uint64(*req.ChosenTaskID), false)
	if err != nil {
		return err
	}
	if _, err := PromoteSegmentVideoPendingRun(ctx, tx, p, row, take); err != nil {
		return err
	}
	adoptSegmentTake(row, take.Task.ID, req.ResetEditOnTaskChange)
	return nil
}

// adoptSegmentTake 默认换镜清空六个精裁字段，人工采用不受自动采用条件限制。
func adoptSegmentTake(row *model.DramaSegment, id uint64, reset *bool) {
	changed := row.ChosenTaskID == nil || *row.ChosenTaskID != id
	if changed && (reset == nil || *reset) {
		repository.ResetSegmentEdit(row)
	}
	row.ChosenTaskID, row.ChosenCleared = &id, false
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
