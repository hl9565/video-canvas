package service

import (
	"context"
	"errors"
	"strings"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/repository"
)

// Generate 固定视频种类并在归属检查后优先核验重放，实时业务守卫由任务事务再次执行。
func (s *DramaSegmentService) Generate(ctx context.Context, userID, sid uint64, key string, req *model.GenerateDramaSegmentReq) (*model.GenerationTaskView, error) {
	// 1. 保留键在读取目标之前拒绝；归属读取不检查锁定、繁忙或时长。
	if req == nil || req.ModelID == "" || req.Input == nil || strings.HasPrefix(key, InternalDramaFrameRunKeyPrefix) {
		return nil, errcode.ErrInvalidParams
	}
	p, ep, row, err := s.segmentSource(ctx, userID, sid)
	if err != nil {
		return nil, err
	}
	createReq := segmentCreateRequest(p, sid, req)
	replay, err := s.tasks.CheckIdempotentReplay(ctx, userID, key, createReq)
	if err != nil || replay != nil {
		return replay, err
	}
	// 2. 未命中才验证当前集状态，防止后续状态漂移阻断合法重放。
	err = s.repo.WithTx(ctx, func(tx repository.DramaTx) error {
		lockedP, lockedEp, lockedRow, err := lockSegment(ctx, tx, userID, sid)
		if err != nil {
			return err
		}
		p, ep, row = lockedP, lockedEp, lockedRow
		if ep.LockedAt == nil {
			return errcode.ErrDramaStepNotReady.WithMsg("媒体任务只允许已锁定的集")
		}
		jobs, err := tx.Jobs(ctx, userID, p.ID, true)
		if err != nil {
			return err
		}
		if dramaJobsConflict(jobs, &model.DramaJobReq{Step: "segment", Episodes: []int{ep.No}}) {
			return errcode.ErrDramaJobBusy
		}
		return nil
	})
	if err != nil {
		return nil, segmentError(err)
	}
	if err := checkSegmentTimeline(req.Input, row.Seconds); err != nil {
		return nil, err
	}
	result, err := s.tasks.Create(ctx, userID, key, segmentCreateRequest(p, sid, req))
	if err != nil {
		return nil, err
	}
	return segmentCreatedTask(result, p, sid)
}

// Quote 无锁读取片段归属与版本，报价令牌绑定该版本和精裁单镜约束。
func (s *DramaSegmentService) Quote(ctx context.Context, userID, sid uint64, req *model.GenerateDramaSegmentReq) (*model.TaskQuoteView, error) {
	// 1. 请求结构与生成入口一致；报价不要求集已锁定，也不冻结积分。
	if req == nil || req.ModelID == "" || req.Input == nil {
		return nil, errcode.ErrInvalidParams
	}
	p, ep, row, err := s.segmentSource(ctx, userID, sid)
	if err != nil {
		return nil, err
	}
	return s.tasks.Quote(ctx, userID, &model.QuoteGenerationTaskReq{Kind: model.KindVideo, ModelID: req.ModelID, CanvasID: idcodec.ID(p.CanvasID), NodeID: segmentNode(sid), Input: req.Input}, QuoteTaskOptions{SegmentID: sid, SegmentRevision: row.Revision, SingleShotOnly: ep.EditSchema == 1})
}

// segmentSource 无锁读取存活的段、集和项目，统一隐藏越权资源。
func (s *DramaSegmentService) segmentSource(ctx context.Context, userID, sid uint64) (*model.DramaProject, *model.DramaEpisode, *model.DramaSegment, error) {
	row, err := s.repo.Segment(ctx, userID, sid, false)
	if err != nil {
		return nil, nil, nil, segmentError(err)
	}
	ep, err := s.repo.SegmentEpisode(ctx, userID, row.EpisodeID)
	if err != nil {
		return nil, nil, nil, segmentError(err)
	}
	p, err := s.repo.Project(ctx, userID, ep.DramaID, false)
	if err != nil {
		return nil, nil, nil, segmentError(err)
	}
	return p, ep, row, nil
}

// segmentCreateRequest 将服务端认可的归属与客户端报价一起传给任务服务。
func segmentCreateRequest(p *model.DramaProject, sid uint64, req *model.GenerateDramaSegmentReq) *model.CreateGenerationTaskReq {
	return &model.CreateGenerationTaskReq{Kind: model.KindVideo, ModelID: req.ModelID, CanvasID: idcodec.ID(p.CanvasID), NodeID: segmentNode(sid), Input: req.Input, QuoteToken: req.QuoteToken}
}

// segmentCreatedTask 将单项错误恢复为标准业务错误，并检查返回任务的归属。
func segmentCreatedTask(result *model.CreateGenerationTaskResp, project *model.DramaProject, sid uint64) (*model.GenerationTaskView, error) {
	if result == nil || len(result.Items) != 1 {
		return nil, errors.New("片段任务创建未返回唯一结果")
	}
	item := result.Items[0]
	if item.Error != nil {
		return nil, errcode.New(item.Error.Code, item.Error.Message, item.Error.Status)
	}
	if item.Task == nil {
		return nil, errors.New("片段任务创建结果缺少任务")
	}
	if item.Task.NodeID != segmentNode(sid) || item.Task.CanvasProjectID == nil || uint64(*item.Task.CanvasProjectID) != project.CanvasID || item.Task.Kind != model.KindVideo {
		return nil, errcode.ErrIdempotencyConflict
	}
	return item.Task, nil
}
