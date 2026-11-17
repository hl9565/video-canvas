package service

import (
	"context"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/repository"
)

// Create 在用户分集末尾或指定位置插入片段，保持seq连续。
func (s *DramaSegmentService) Create(ctx context.Context, userID, dramaID uint64, no int, req *model.CreateDramaSegmentReq) (*model.DramaSegmentView, error) {
	// 1. 复用项目归属检查；默认五秒便于用户新增空白片段。
	_, ep, err := s.episode(ctx, userID, dramaID, no)
	if err != nil {
		return nil, err
	}
	seconds := 5
	if req.Seconds != nil {
		seconds = *req.Seconds
	}
	if seconds < 1 || (req.AfterSeq != nil && *req.AfterSeq < 0) {
		return nil, errcode.ErrInvalidParams
	}
	row := &model.DramaSegment{UserID: userID, EpisodeID: ep.ID, Seconds: seconds, Prompt: req.Prompt, Revision: 1}
	// 2. 锁项目后移动后续片段；插入和所有seq变化同事务提交。
	err = s.repo.WithTx(ctx, func(tx repository.DramaTx) error {
		if _, err := tx.Project(ctx, userID, dramaID, true); err != nil {
			return err
		}
		return insertDramaSegment(ctx, tx, row, req.AfterSeq)
	})
	if err != nil {
		return nil, dramaError(err)
	}
	return s.changedView(ctx, userID, row.ID)
}

// Update 按版本更新片段，事务外探测源帧数，锁内校验采用与编辑并单次递增集版本。
func (s *DramaSegmentService) Update(ctx context.Context, userID, sid uint64, req *model.UpdateDramaSegmentReq) (*model.DramaSegmentView, error) {
	// 1. 先解析媒体缓存；探测不得占用项目、集或片段的数据库锁。
	if req == nil || req.Revision < 1 || (req.Seconds != nil && *req.Seconds < 1) || req.ClearChosen && req.ChosenTaskID != nil {
		return nil, errcode.ErrInvalidParams
	}
	frames, err := s.resolveUpdateFrames(ctx, userID, sid, req)
	if err != nil {
		return nil, err
	}
	// 2. 按项目→集→段加锁，CAS 后应用采用与编辑，版本由 SaveSegment 统一递增。
	err = s.repo.WithTx(ctx, func(tx repository.DramaTx) error {
		p, ep, row, err := lockSegment(ctx, tx, userID, sid)
		if err != nil {
			return err
		}
		if row.Revision != req.Revision || !matchesExpectedTask(row, req) {
			return errcode.ErrCanvasConflict
		}
		if err := s.updateSegmentTake(ctx, tx, p, row, req); err != nil {
			return err
		}
		if err := applySegmentUpdate(row, req, frames); err != nil {
			return err
		}
		return saveSegmentUpdate(ctx, tx, row, ep, req)
	})
	if err != nil {
		return nil, segmentError(err)
	}
	return s.changedView(ctx, userID, sid)
}

// Delete 只删除片段并收拢后续序号，保留所有生成任务和素材。
func (s *DramaSegmentService) Delete(ctx context.Context, userID, sid uint64) error {
	// 1. 先锁项目再锁片段，避免与分镜替换或并发排序交错。
	var project *model.DramaProject
	var episode *model.DramaEpisode
	err := s.repo.WithTx(ctx, func(tx repository.DramaTx) error {
		p, ep, row, err := lockSegment(ctx, tx, userID, sid)
		if err != nil {
			return err
		}
		project, episode = p, ep
		bt, hasBatch := tx.(segmentBatchVersionTx)
		if hasBatch {
			if _, err := bt.DeleteSegmentNoBump(ctx, userID, sid); err != nil {
				return err
			}
		} else if err := tx.DeleteSegment(ctx, userID, sid); err != nil {
			return err
		}
		rows, err := tx.Segments(ctx, userID, row.EpisodeID)
		if err != nil {
			return err
		}
		for i := range rows {
			if rows[i].Seq == i {
				continue
			}
			rows[i].Seq = i
			if err := saveBatchSegment(ctx, tx, bt, hasBatch, &rows[i], rows[i].Revision); err != nil {
				return err
			}
		}
		if hasBatch {
			return bt.BumpSegmentVersion(ctx, userID, row.EpisodeID)
		}
		return nil
	})
	if err != nil {
		return segmentError(err)
	}
	// 2. 删除事件不虚构SegmentView；剩余片段的seq变化通过完整列表和事件同步。
	views, err := s.List(ctx, userID, project.ID, episode.No)
	if err != nil {
		return err
	}
	for i := range views {
		s.publishSegment(ctx, project, episode, &views[i])
	}
	return nil
}

// Order 只接受该集片段的完整不重复集合，在同一事务重排并更新变更片段版本。
func (s *DramaSegmentService) Order(ctx context.Context, userID, dramaID uint64, no int, req *model.OrderDramaSegmentsReq) ([]model.DramaSegmentView, error) {
	// 1. 通过用户项目定位集，排序集合在项目锁内重新读取。
	_, ep, err := s.episode(ctx, userID, dramaID, no)
	if err != nil {
		return nil, segmentError(err)
	}
	err = s.repo.WithTx(ctx, func(tx repository.DramaTx) error {
		if _, err := tx.Project(ctx, userID, dramaID, true); err != nil {
			return err
		}
		rows, err := tx.Segments(ctx, userID, ep.ID)
		if err != nil {
			return err
		}
		return saveDramaSegmentOrder(ctx, tx, rows, req)
	})
	if err != nil {
		return nil, segmentError(err)
	}
	// 2. 提交后读取完整视图，任务或首帧的变化也反映在返回值中。
	views, err := s.List(ctx, userID, dramaID, no)
	if err != nil {
		return nil, err
	}
	p, err := s.repo.Project(ctx, userID, dramaID, false)
	if err != nil {
		return nil, dramaError(err)
	}
	for i := range views {
		s.publishSegment(ctx, p, ep, &views[i])
	}
	return views, nil
}

// segmentBatchVersionTx 允许事务内多片段操作只在提交前递增一次 episode.segment_version。
type segmentBatchVersionTx interface {
	SaveSegmentNoBump(context.Context, *model.DramaSegment, int64) (bool, error)
	DeleteSegmentNoBump(context.Context, uint64, uint64) (uint64, error)
	BumpSegmentVersion(context.Context, uint64, uint64) error
}

// saveBatchSegment 在支持批处理版本号的事务中跳过逐行递增，否则退回标准 SaveSegment。
func saveBatchSegment(ctx context.Context, tx repository.DramaTx, bt segmentBatchVersionTx, hasBatch bool, row *model.DramaSegment, revision int64) error {
	if hasBatch {
		_, err := bt.SaveSegmentNoBump(ctx, row, revision)
		return err
	}
	return tx.SaveSegment(ctx, row, revision)
}

// insertDramaSegment 在已持有项目锁的事务中校验插入位置并顺移后续片段。
func insertDramaSegment(ctx context.Context, tx repository.DramaTx, row *model.DramaSegment, afterSeq *int) error {
	rows, err := tx.Segments(ctx, row.UserID, row.EpisodeID)
	if err != nil {
		return err
	}
	row.Seq = len(rows)
	if afterSeq != nil {
		if *afterSeq >= len(rows) {
			return errcode.ErrInvalidParams.WithMsg("插入位置不存在")
		}
		row.Seq = *afterSeq + 1
	}
	bt, hasBatch := tx.(segmentBatchVersionTx)
	for i := range rows {
		if rows[i].Seq < row.Seq {
			continue
		}
		rows[i].Seq++
		if err := saveBatchSegment(ctx, tx, bt, hasBatch, &rows[i], rows[i].Revision); err != nil {
			return err
		}
	}
	if err := saveBatchSegment(ctx, tx, bt, hasBatch, row, 0); err != nil {
		return err
	}
	if hasBatch {
		return bt.BumpSegmentVersion(ctx, row.UserID, row.EpisodeID)
	}
	return nil
}

// saveDramaSegmentOrder 拒绝缺失、重复和跨集ID，只更新序号实际变化的片段并单次递增集版本。
func saveDramaSegmentOrder(ctx context.Context, tx repository.DramaTx, rows []model.DramaSegment, req *model.OrderDramaSegmentsReq) error {
	if len(rows) != len(req.IDs) {
		return errcode.ErrDramaSegmentOrderInvalid
	}
	byID := make(map[uint64]*model.DramaSegment, len(rows))
	for i := range rows {
		byID[rows[i].ID] = &rows[i]
	}
	bt, hasBatch := tx.(segmentBatchVersionTx)
	var reordered bool
	for i, id := range req.IDs {
		row, ok := byID[uint64(id)]
		if !ok {
			return errcode.ErrDramaSegmentOrderInvalid
		}
		delete(byID, uint64(id))
		if row.Seq == i {
			continue
		}
		reordered = true
		row.Seq = i
		if err := saveBatchSegment(ctx, tx, bt, hasBatch, row, row.Revision); err != nil {
			return err
		}
	}
	if hasBatch && reordered && len(rows) > 0 {
		return bt.BumpSegmentVersion(ctx, rows[0].UserID, rows[0].EpisodeID)
	}
	return nil
}

// saveSegmentUpdate 将精裁 schema 升级与段 CAS 一并保存，媒体变更时集版本只递增一次。
func saveSegmentUpdate(ctx context.Context, tx repository.DramaTx, row *model.DramaSegment, ep *model.DramaEpisode, req *model.UpdateDramaSegmentReq) error {
	flippedSchema := req.Edit != nil && ep.EditSchema == 0
	if flippedSchema {
		rev := ep.Revision
		ep.EditSchema = 1
		if err := tx.SaveEpisode(ctx, ep, rev); err != nil {
			return err
		}
	}
	bt, ok := tx.(segmentBatchVersionTx)
	if !ok {
		return tx.SaveSegment(ctx, row, req.Revision)
	}
	changed, err := bt.SaveSegmentNoBump(ctx, row, req.Revision)
	if err != nil {
		return err
	}
	if changed || flippedSchema {
		return bt.BumpSegmentVersion(ctx, row.UserID, row.EpisodeID)
	}
	return nil
}
