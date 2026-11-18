package service

import (
	"context"
	"errors"

	"go.uber.org/zap"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/logger"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/provider"
	"video-canvas/internal/repository"
)

type segmentRead struct {
	row   model.DramaSegment
	takes []resolvedSegmentTake
	run   *model.DramaSegmentFrameRun
	video *model.Asset
	frame *model.Asset
	// Older asset readers expose only View; the production reader projects the transaction's asset rows.
	assetSnapshot bool
	frameSnapshot bool
}

// segmentAssetProjector 复用素材服务的稳定 URL 规则，避免投影时另开数据库查询。
type segmentAssetProjector interface {
	ViewOf(context.Context, *model.Asset) (*model.AssetView, error)
}

type snapshotSegmentAssets struct {
	tx        repository.DramaTx
	projector segmentAssetProjector
	rows      map[uint64]*model.Asset
}

func (s snapshotSegmentAssets) View(ctx context.Context, user, id uint64) (*model.AssetView, error) {
	row, err := s.tx.ComposeAsset(ctx, user, id)
	if err != nil {
		return nil, err
	}
	s.rows[id] = row
	if s.projector != nil {
		return s.projector.ViewOf(ctx, row)
	}
	return assetView(row, "/files/"+row.StorageKey), nil
}

func (s *DramaSegmentService) readEpisodeSegments(ctx context.Context, user, dramaID uint64, no int) (*model.DramaProject, model.DramaEpisode, []segmentRead, error) {
	var p *model.DramaProject
	var ep *model.DramaEpisode
	var reads []segmentRead
	err := s.repo.WithSnapshotTx(ctx, func(tx repository.DramaTx) error {
		var err error
		p, ep, err = locateEpisode(ctx, tx, user, dramaID, no)
		if err != nil {
			return err
		}
		rows, err := tx.Segments(ctx, user, ep.ID)
		if err != nil {
			return err
		}
		reads, err = s.loadSegmentReads(ctx, tx, p, rows, false)
		return err
	})
	if err != nil {
		return nil, model.DramaEpisode{}, nil, dramaError(err)
	}
	return p, *ep, reads, nil
}

func (s *DramaSegmentService) loadSegmentReads(ctx context.Context, tx repository.DramaTx, p *model.DramaProject, rows []model.DramaSegment, requireTxAssets bool) ([]segmentRead, error) {
	reads := make([]segmentRead, 0, len(rows))
	for i := range rows {
		read, err := s.loadSegmentRead(ctx, tx, p, rows[i], requireTxAssets)
		if err != nil {
			return nil, err
		}
		reads = append(reads, read)
	}
	return reads, nil
}

func (s *DramaSegmentService) loadSegmentRead(ctx context.Context, tx repository.DramaTx, p *model.DramaProject, row model.DramaSegment, requireTxAssets bool) (segmentRead, error) {
	src, err := loadSegmentTakeSources(ctx, tx, p, &row)
	if err != nil {
		return segmentRead{}, err
	}
	read := segmentRead{row: row, run: latestSegmentFrameRun(src.runs)}
	assets := s.assets
	var assetRows map[uint64]*model.Asset
	if projector, ok := assets.(segmentAssetProjector); ok || requireTxAssets {
		read.assetSnapshot = true
		assetRows = make(map[uint64]*model.Asset)
		assets = snapshotSegmentAssets{tx: tx, projector: projector, rows: assetRows}
	}
	read.takes, err = resolveSegmentTakeSources(ctx, assets, p, &row, src)
	if err != nil {
		return segmentRead{}, err
	}
	for _, take := range read.takes {
		if row.ChosenTaskID != nil && take.Task.ID == *row.ChosenTaskID && take.Asset != nil {
			read.video = assetRows[take.Asset.ID]
			break
		}
	}
	err = s.loadSnapshotFrameAsset(ctx, tx, &read)
	return read, err
}

func (s *DramaSegmentService) loadSnapshotFrameAsset(ctx context.Context, tx repository.DramaTx, read *segmentRead) error {
	frames, ok := s.frameRuns.(*DramaFrameRunService)
	if !ok || read.run == nil {
		return nil
	}
	if _, ok := frames.assets.(segmentAssetProjector); !ok {
		return nil
	}
	read.frameSnapshot = true
	if read.run.FrameAssetID == nil {
		return nil
	}
	asset, err := tx.ComposeAsset(ctx, read.row.UserID, *read.run.FrameAssetID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	read.frame = asset
	return err
}

func canAutoAdoptSegment(row *model.DramaSegment) bool {
	return row.ChosenTaskID == nil && !row.ChosenCleared && dramaVisualKind(*row) == model.DramaVisualVideo
}

func pendingSegmentPromotion(take *resolvedSegmentTake) bool {
	return take.IsFrameRun && take.Usable && take.Run.State == model.FrameRunStateVideoPending
}

func (r *segmentRead) needsRecovery() bool {
	for i := range r.takes {
		take := &r.takes[i]
		if pendingSegmentPromotion(take) || (canAutoAdoptSegment(&r.row) && take.Adoptable) {
			return true
		}
	}
	return false
}

// reconcileReadSegment 拿到锁后重新确认候选，防止基于过时只读快照写回。
func (s *DramaSegmentService) reconcileReadSegment(ctx context.Context, user, sid uint64) (bool, error) {
	var changed bool
	err := s.repo.WithTx(ctx, func(tx repository.DramaTx) error {
		p, _, row, err := lockSegment(ctx, tx, user, sid)
		if err != nil {
			return err
		}
		takes, err := ResolveSegmentTakes(ctx, tx, lockedSegmentTakeAssets{tx: tx}, p, row)
		if err != nil {
			return err
		}
		changed, err = s.reconcileSegment(ctx, tx, p, row, takes)
		return err
	})
	return changed, segmentError(err)
}

func (s *DramaSegmentService) readSingleSegment(ctx context.Context, user, sid uint64) (*model.DramaProject, *model.DramaEpisode, segmentRead, error) {
	var p *model.DramaProject
	var ep *model.DramaEpisode
	var read segmentRead
	err := s.repo.WithSnapshotTx(ctx, func(tx repository.DramaTx) error {
		row, err := tx.Segment(ctx, user, sid, false)
		if err != nil {
			return err
		}
		ep, err = tx.SegmentEpisode(ctx, user, row.EpisodeID)
		if err != nil {
			return err
		}
		p, err = tx.Project(ctx, user, ep.DramaID, false)
		if err != nil {
			return err
		}
		read, err = s.loadSegmentRead(ctx, tx, p, *row, false)
		return err
	})
	return p, ep, read, segmentError(err)
}

// readView 共用只读投影和漏回调恢复逻辑。
func (s *DramaSegmentService) readView(ctx context.Context, user, sid uint64) (*model.DramaSegmentView, *model.DramaProject, *model.DramaEpisode, bool, error) {
	return s.readSegmentView(ctx, user, sid, true)
}

func (s *DramaSegmentService) readSegmentView(ctx context.Context, user, sid uint64, reconcile bool) (*model.DramaSegmentView, *model.DramaProject, *model.DramaEpisode, bool, error) {
	p, ep, read, err := s.readSingleSegment(ctx, user, sid)
	if err != nil {
		return nil, nil, nil, false, err
	}
	var changed bool
	if reconcile && read.needsRecovery() {
		changed, err = s.reconcileReadSegment(ctx, user, sid)
		if err != nil {
			return nil, nil, nil, false, err
		}
		p, ep, read, err = s.readSingleSegment(ctx, user, sid)
		if err != nil {
			return nil, nil, nil, false, err
		}
	}
	v, err := s.projectSegmentRead(ctx, ep, &read)
	return v, p, ep, changed, err
}

func (s *DramaSegmentService) projectSegmentReads(ctx context.Context, ep *model.DramaEpisode, reads []segmentRead) ([]model.DramaSegmentView, error) {
	views := make([]model.DramaSegmentView, 0, len(reads))
	for i := range reads {
		v, err := s.projectSegmentRead(ctx, ep, &reads[i])
		if err != nil {
			return nil, err
		}
		views = append(views, *v)
	}
	return views, nil
}

func (s *DramaSegmentService) projectSegmentRead(ctx context.Context, ep *model.DramaEpisode, read *segmentRead) (*model.DramaSegmentView, error) {
	v, err := s.segmentReadView(ep, read)
	if err != nil {
		return nil, err
	}
	if v.ChosenTaskID != "" && s.probes != nil {
		v.SourceFrameCount, err = s.projectSegmentSourceFrames(ctx, read)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			logger.Warn("片段读取探测源帧数失败，降级为未知帧数", zap.Uint64("segment_id", read.row.ID), zap.Error(err))
		}
	}
	if read.run != nil && !read.frameSnapshot && s.frameRuns != nil {
		return v, s.frameRuns.EnrichFrameRunView(ctx, read.row.UserID, read.run, v.FrameRun)
	}
	return v, nil
}

// segmentReadView 只使用已读取的行和素材元数据，投影完整候选及首帧流程。
func (s *DramaSegmentService) segmentReadView(ep *model.DramaEpisode, read *segmentRead) (*model.DramaSegmentView, error) {
	v, err := segmentView(&read.row, ep, read.takes)
	if err != nil || read.run == nil {
		return v, err
	}
	v.FrameRun = frameRunView(read.run)
	if read.frameSnapshot {
		s.frameRuns.(*DramaFrameRunService).projectFrameRunDetails(read.run, read.frame, v.FrameRun)
	}
	return v, nil
}

// readSegmentBatch 在写事务中组装全量已采用视图；投影失败便回滚，提交后不再补读。
func (s *DramaSegmentService) readSegmentBatch(ctx context.Context, tx repository.DramaTx, p *model.DramaProject, episodeID uint64) (*model.DramaEpisode, []model.DramaSegmentView, error) {
	ep, err := tx.SegmentEpisode(ctx, p.UserID, episodeID)
	if err != nil {
		return nil, nil, err
	}
	rows, err := tx.Segments(ctx, p.UserID, ep.ID)
	if err != nil {
		return nil, nil, err
	}
	reads, err := s.loadSegmentReads(ctx, tx, p, rows, true)
	if err != nil {
		return nil, nil, err
	}
	views := make([]model.DramaSegmentView, 0, len(reads))
	for i := range reads {
		view, err := s.segmentReadView(ep, &reads[i])
		if err != nil {
			return nil, nil, err
		}
		if s.probes != nil && view.ChosenTaskID != "" {
			view.SourceFrameCount, err = readSegmentBatchFrames(ctx, tx, reads[i].video)
			if err != nil {
				return nil, nil, err
			}
		}
		views = append(views, *view)
	}
	return ep, views, nil
}

// readSegmentBatchFrames 只读取匹配采用素材指纹的已有探测结果，不在写事务中冷探测。
func readSegmentBatchFrames(ctx context.Context, tx repository.DramaTx, video *model.Asset) (*int32, error) {
	if video == nil || !validDramaManifestHash(video.Sha256) {
		return nil, nil
	}
	probe, err := tx.SegmentVideoProbe(ctx, video.ID, video.Sha256, videoframe.NormalizedProbeVersion)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if probe.NormalizedFrames24 <= 0 {
		return nil, nil
	}
	return &probe.NormalizedFrames24, nil
}

func (s *DramaSegmentService) projectSegmentSourceFrames(ctx context.Context, read *segmentRead) (*int32, error) {
	if read.video != nil {
		store, _ := s.assets.(provider.AssetStore)
		return s.segmentVideoFramesForAsset(ctx, store, read.video, read.row.UserID, read.video.ID, store != nil)
	}
	if !read.assetSnapshot {
		for _, take := range read.takes {
			if take.Task.ID == *read.row.ChosenTaskID && take.Usable {
				return s.resolveSegmentSourceFrames(ctx, read.row.UserID, read.row.ChosenTaskID, []model.GenerationTask{take.Task})
			}
		}
	}
	return nil, nil
}
