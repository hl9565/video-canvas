package repository

import (
	"context"
	"math"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/idcodec"
)

// liveSegmentEpisodes 限定同一用户且项目与分集都未软删，避免父项目删除后仍读到片段。
func (r *DramaRepository) liveSegmentEpisodes(ctx context.Context, userID uint64) *gorm.DB {
	projects := r.db.WithContext(ctx).Model(&model.DramaProject{}).Select("id").Where("user_id = ?", userID)
	return r.db.WithContext(ctx).Model(&model.DramaEpisode{}).Select("id").Where("user_id = ? AND drama_id IN (?)", userID, projects)
}

// SegmentEpisode 按用户和集ID读取存活项目下的集，缺失返回ErrNotFound。
func (r *DramaRepository) SegmentEpisode(ctx context.Context, userID, episodeID uint64) (*model.DramaEpisode, error) {
	var ep model.DramaEpisode
	err := r.db.WithContext(ctx).Where("user_id = ? AND id = ? AND id IN (?)", userID, episodeID, r.liveSegmentEpisodes(ctx, userID)).First(&ep).Error
	return &ep, translate(err)
}

// Segments 按用户和集ID读取存活项目下的片段，按seq、ID排序。
func (r *DramaRepository) Segments(ctx context.Context, userID, episodeID uint64) ([]model.DramaSegment, error) {
	rows := []model.DramaSegment{}
	err := r.db.WithContext(ctx).Where("user_id = ? AND episode_id = ? AND episode_id IN (?)", userID, episodeID, r.liveSegmentEpisodes(ctx, userID)).Order("seq ASC, id ASC").Find(&rows).Error
	return rows, err
}

// Segment 按用户和ID读取存活项目下的片段，缺失返回ErrNotFound；写入前须先锁项目。
func (r *DramaRepository) Segment(ctx context.Context, userID, id uint64, lock bool) (*model.DramaSegment, error) {
	q := r.db.WithContext(ctx).Where("user_id = ? AND id = ? AND episode_id IN (?)", userID, id, r.liveSegmentEpisodes(ctx, userID))
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var row model.DramaSegment
	err := q.First(&row).Error
	return &row, translate(err)
}

// ResetSegmentEdit 切换片段采用的视频结果时将 6 个剪辑与原声控制字段原子清零，并将 source_out_f 置空恢复到片尾 EOF。
func ResetSegmentEdit(row *model.DramaSegment) {
	if row == nil {
		return
	}
	row.SourceInF = 0
	row.SourceOutF = nil
	row.MuteOriginal = false
	row.AudioGainDB = 0
	row.NativeIsDialogue = false
	row.AvPtsOffsetMs = 0
}

func sameNullableInt32(a, b *int32) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func segmentMaterialChanged(prev, cur *model.DramaSegment) bool {
	if prev.Seq != cur.Seq || !sameChosenTask(prev.ChosenTaskID, cur.ChosenTaskID) {
		return true
	}
	return prev.SourceInF != cur.SourceInF ||
		!sameNullableInt32(prev.SourceOutF, cur.SourceOutF) ||
		prev.MuteOriginal != cur.MuteOriginal ||
		math.Round(prev.AudioGainDB*100) != math.Round(cur.AudioGainDB*100) ||
		prev.NativeIsDialogue != cur.NativeIsDialogue ||
		prev.AvPtsOffsetMs != cur.AvPtsOffsetMs
}

// SaveSegment 按用户、集与revision更新；旧版本返回ErrStateConflict，父资源不存在返回ErrNotFound。
func (r *DramaRepository) SaveSegment(ctx context.Context, row *model.DramaSegment, revision int64) error {
	changed, err := r.SaveSegmentNoBump(ctx, row, revision)
	if err != nil || !changed {
		return err
	}
	return r.BumpSegmentVersion(ctx, row.UserID, row.EpisodeID)
}

// SaveSegmentNoBump 保存片段但不递增 episode.segment_version，返回本次保存是否发生实质变更。
func (r *DramaRepository) SaveSegmentNoBump(ctx context.Context, row *model.DramaSegment, revision int64) (bool, error) {
	if _, err := r.SegmentEpisode(ctx, row.UserID, row.EpisodeID); err != nil {
		return false, err
	}
	if row.ID == 0 {
		if err := r.db.WithContext(ctx).Create(row).Error; err != nil {
			return false, err
		}
		return true, nil
	}
	previous, err := r.Segment(ctx, row.UserID, row.ID, false)
	if err != nil {
		return false, err
	}
	now := time.Now()
	var sourceOutF any
	if row.SourceOutF != nil {
		sourceOutF = *row.SourceOutF
	}
	res := r.db.WithContext(ctx).Model(&model.DramaSegment{}).Where("user_id = ? AND episode_id = ? AND id = ? AND revision = ?", row.UserID, row.EpisodeID, row.ID, revision).Updates(map[string]any{
		"seq": row.Seq, "seconds": row.Seconds, "prompt": row.Prompt,
		"chosen_task_id": row.ChosenTaskID, "adaptations": segmentAdaptations(row.Adaptations),
		"source_in_f": row.SourceInF, "source_out_f": sourceOutF,
		"mute_original": row.MuteOriginal, "audio_gain_db": row.AudioGainDB,
		"native_is_dialogue": row.NativeIsDialogue, "av_pts_offset_ms": row.AvPtsOffsetMs,
		"revision": gorm.Expr("revision + 1"), "updated_at": now,
	})
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 0 {
		return false, ErrStateConflict
	}
	row.Revision, row.UpdatedAt = revision+1, now
	return segmentMaterialChanged(previous, row), nil
}

// DeleteSegment 只软删当前用户存活项目下的片段，缺失返回ErrNotFound；任务和素材不受影响。
func (r *DramaRepository) DeleteSegment(ctx context.Context, userID, id uint64) error {
	episodeID, err := r.DeleteSegmentNoBump(ctx, userID, id)
	if err != nil {
		return err
	}
	return r.BumpSegmentVersion(ctx, userID, episodeID)
}

// DeleteSegmentNoBump 只软删当前用户存活项目下的片段，返回所属 episodeID 且不递增 segment_version。
func (r *DramaRepository) DeleteSegmentNoBump(ctx context.Context, userID, id uint64) (uint64, error) {
	row, err := r.Segment(ctx, userID, id, false)
	if err != nil {
		return 0, err
	}
	res := r.db.WithContext(ctx).Where("user_id = ? AND id = ? AND episode_id IN (?)", userID, id, r.liveSegmentEpisodes(ctx, userID)).Delete(&model.DramaSegment{})
	if res.Error != nil {
		return 0, res.Error
	}
	if res.RowsAffected == 0 {
		return 0, ErrNotFound
	}
	return row.EpisodeID, nil
}

// BumpSegmentVersion 显式递增单集片段版本号（供多片段事务结束时统一递增一次）。
func (r *DramaRepository) BumpSegmentVersion(ctx context.Context, userID, episodeID uint64) error {
	return r.bumpSegmentVersion(ctx, userID, episodeID)
}

// BumpProjectNewSchemaEpisodeVersions 在项目锁保护的事务内递增受画幅影响的新版分集版本。
// 覆盖 edit_schema = 1 的分集，以及虽为 edit_schema = 0 但存在在途冻结清单合成任务（queued/running/paused）
// 或当前已定版任务（final_compose_job_id）本身为冻结清单（input_json.manifest 对象）的分集；
// 历史失败、取消或已被旧整段覆盖淘汰的冻结任务及仅走旧整段路径的分集不受影响。
func (r *DramaRepository) BumpProjectNewSchemaEpisodeVersions(ctx context.Context, userID, dramaID uint64) error {
	return r.db.WithContext(ctx).Model(&model.DramaEpisode{}).
		Where(
			"user_id = ? AND drama_id = ? AND ("+
				"edit_schema = 1 OR EXISTS ("+
				"SELECT 1 FROM drama_jobs j "+
				"WHERE j.user_id = drama_episodes.user_id "+
				"AND j.drama_id = drama_episodes.drama_id "+
				"AND j.deleted_at IS NULL "+
				"AND j.step = 'compose' "+
				"AND jsonb_typeof(j.input_json->'manifest') = 'object' "+
				"AND (j.input_json->>'episode_id') = drama_episodes.id::text "+
				"AND (j.id = drama_episodes.final_compose_job_id OR j.status IN ('queued', 'running', 'paused'))"+
				")"+
				")",
			userID, dramaID,
		).
		Updates(map[string]any{"segment_version": gorm.Expr("segment_version + 1"), "updated_at": time.Now()}).Error
}

// ReplaceSegments 在调用方项目锁保护的事务内替换全集片段，不删除历史任务和素材。
func (r *DramaRepository) ReplaceSegments(ctx context.Context, userID, episodeID uint64, rows []model.DramaSegment) error {
	if _, err := r.SegmentEpisode(ctx, userID, episodeID); err != nil {
		return err
	}
	if err := r.db.WithContext(ctx).Where("user_id = ? AND episode_id = ?", userID, episodeID).Delete(&model.DramaSegment{}).Error; err != nil {
		return err
	}
	for i := range rows {
		rows[i].BaseModel = model.BaseModel{}
		rows[i].UserID, rows[i].EpisodeID, rows[i].Seq, rows[i].Revision = userID, episodeID, i, 1
	}
	if len(rows) > 0 {
		if err := r.db.WithContext(ctx).Create(&rows).Error; err != nil {
			return err
		}
	}
	return r.bumpSegmentVersion(ctx, userID, episodeID)
}

// SegmentTasks 严格匹配用户、画布、节点且排除试跑，按创建时间、ID正序返回。
func (r *DramaRepository) SegmentTasks(ctx context.Context, userID, canvasID uint64, nodeID string) ([]model.GenerationTask, error) {
	rows := []model.GenerationTask{}
	err := r.db.WithContext(ctx).Where("user_id = ? AND canvas_project_id = ? AND node_id = ? AND is_test = false", userID, canvasID, nodeID).Order("created_at ASC, id ASC").Find(&rows).Error
	return rows, err
}

// HasSegmentTakes 判断同用户存活项目下该集现有片段是否有任何视频任务，包括失败或进行中的任务。
func (r *DramaRepository) HasSegmentTakes(ctx context.Context, userID, episodeID uint64) (bool, error) {
	ep, err := r.SegmentEpisode(ctx, userID, episodeID)
	if err != nil {
		return false, err
	}
	project, err := r.Project(ctx, userID, ep.DramaID, false)
	if err != nil {
		return false, err
	}
	segments, err := r.Segments(ctx, userID, episodeID)
	if err != nil || len(segments) == 0 {
		return false, err
	}
	nodes := make([]string, len(segments))
	for i := range segments {
		nodes[i] = "seg:" + idcodec.Encode(segments[i].ID)
	}
	var count int64
	err = r.db.WithContext(ctx).Model(&model.GenerationTask{}).Where("user_id = ? AND canvas_project_id = ? AND node_id IN ? AND kind = ? AND is_test = false", userID, project.CanvasID, nodes, model.KindVideo).Count(&count).Error
	return count > 0, err
}

// bumpSegmentVersion 记录采用、顺序和增删发生过变化；即使之后改回也不能误报成片仍新鲜。
func (r *DramaRepository) bumpSegmentVersion(ctx context.Context, userID, episodeID uint64) error {
	res := r.db.WithContext(ctx).Model(&model.DramaEpisode{}).Where("user_id = ? AND id = ?", userID, episodeID).
		Updates(map[string]any{"segment_version": gorm.Expr("segment_version + 1"), "updated_at": time.Now()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// sameChosenTask 比较可空采用ID，提示词、首帧等编辑不影响既有成片。
func sameChosenTask(a, b *uint64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// segmentAdaptations 空值按空对象写入：列是 NOT NULL，旧代码构造的片段可能没带这一列。
func segmentAdaptations(raw []byte) []byte {
	if len(raw) == 0 {
		return []byte("{}")
	}
	return raw
}
