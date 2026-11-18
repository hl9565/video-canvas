package repository

import (
	"context"
	"encoding/json"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"video-canvas/internal/model"
)

// SaveEpisodeProgram 只更新指定用户、项目及集主键的节目单；片段版本不符返回 ErrStateConflict。
func (r *DramaRepository) SaveEpisodeProgram(ctx context.Context, userID, dramaID, epID uint64, expectedSegmentVersion int64, editSchema int16, programJSON json.RawMessage, materialChange bool) error {
	updates := map[string]any{
		"program_json":    datatypes.JSON(programJSON),
		"segment_version": gorm.Expr("segment_version + CASE WHEN ? THEN 1 ELSE 0 END", materialChange),
		"revision":        gorm.Expr("revision + 1"),
		"updated_at":      gorm.Expr("NOW()"),
	}
	if editSchema > 0 {
		updates["edit_schema"] = editSchema
	}
	result := r.db.WithContext(ctx).Model(&model.DramaEpisode{}).
		Where("user_id = ? AND drama_id = ? AND id = ? AND segment_version = ?", userID, dramaID, epID, expectedSegmentVersion).
		Updates(updates)
	if result.Error != nil {
		return translate(result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrStateConflict
	}
	return nil
}

// LockProgramAssets 锁定当前用户的已有素材行，防止校验后到提交前更换内容或删除；缺行由调用者诊断。
func (r *DramaRepository) LockProgramAssets(ctx context.Context, userID uint64, ids []uint64) error {
	if len(ids) == 0 {
		return nil
	}
	var assets []model.Asset
	return r.db.WithContext(ctx).Select("id").Where("user_id = ? AND id IN ?", userID, ids).
		Order("id ASC").Clauses(clause.Locking{Strength: "SHARE"}).Find(&assets).Error
}
