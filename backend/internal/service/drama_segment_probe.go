package service

import (
	"context"
	"errors"
	"io"
	"os"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/provider"
	"video-canvas/internal/repository"
)

// resolveSegmentSourceFrames 在事务外解析采用任务与素材，优先读取内容指纹对应的探测缓存。
func (s *DramaSegmentService) resolveSegmentSourceFrames(ctx context.Context, userID uint64, taskID *uint64, tasks []model.GenerationTask) (*int32, error) {
	if taskID == nil || s.probes == nil {
		return nil, nil
	}
	var task *model.GenerationTask
	for i := range tasks {
		if tasks[i].ID == *taskID {
			task = &tasks[i]
			break
		}
	}
	if task == nil {
		extra, err := s.repo.TasksByIDs(ctx, userID, []uint64{*taskID})
		if err != nil {
			return nil, err
		}
		if len(extra) == 1 {
			task = &extra[0]
		}
	}
	if task == nil || task.Kind != model.KindVideo || task.Status != model.TaskSucceeded {
		return nil, errcode.ErrDramaTakeInvalid
	}
	assetID, err := segmentOutputAsset(task, model.KindVideo)
	if err != nil {
		return nil, err
	}
	if assetID == 0 {
		return nil, errcode.ErrDramaEditInvalid.WithMsg("采用镜头缺少视频素材")
	}
	store, ok := s.assets.(provider.AssetStore)
	if !ok {
		return nil, errcode.ErrDramaEditInvalid.WithMsg("视频探测依赖不可用")
	}
	asset, err := store.Get(ctx, userID, assetID)
	if err != nil {
		return nil, err
	}
	if asset.Kind != model.KindVideo {
		return nil, errcode.ErrDramaTakeInvalid
	}
	probe, err := s.probes.Get(ctx, assetID, asset.Sha256, videoframe.NormalizedProbeVersion)
	if errors.Is(err, repository.ErrNotFound) {
		probe, err = s.probeSegmentAsset(ctx, store, asset)
	}
	if err != nil {
		return nil, err
	}
	frames := probe.NormalizedFrames24
	if frames <= 0 {
		return nil, errcode.ErrDramaEditInvalid.WithMsg("视频没有有效帧")
	}
	return &frames, nil
}

// probeSegmentAsset 将归属已校验的素材复制到临时文件后探测，网络与解码均在数据库事务之外。
func (s *DramaSegmentService) probeSegmentAsset(ctx context.Context, store provider.AssetStore, asset *model.Asset) (*model.AssetMediaProbe, error) {
	source, err := store.Open(ctx, asset.UserID, asset.ID)
	if err != nil {
		return nil, err
	}
	defer source.Body.Close()
	file, err := os.CreateTemp("", "vc-segment-probe-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(file.Name())
	_, copyErr := io.Copy(file, source.Body)
	closeErr := file.Close()
	if copyErr != nil {
		return nil, copyErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	result, err := s.probeVideo(ctx, file.Name())
	if err != nil {
		return nil, err
	}
	probe := &model.AssetMediaProbe{AssetID: asset.ID, Sha256: asset.Sha256, ProbeVersion: videoframe.NormalizedProbeVersion,
		FpsNum: result.FpsNum, FpsDen: result.FpsDen, NormalizedFrames24: result.NormalizedFrames24, HasAudio: result.HasAudio,
		AudioSampleRate: result.AudioSampleRate, DecodedAudioSamples: result.DecodedAudioSamples, VStartUs: result.VStartUs, AStartUs: result.AStartUs}
	if err := s.probes.Upsert(ctx, probe); err != nil {
		return nil, err
	}
	return probe, nil
}

// resolveUpdateFrames 将探测绑定到请求版本和拟采用任务，写锁内仍须重验版本及任务归属。
func (s *DramaSegmentService) resolveUpdateFrames(ctx context.Context, userID, sid uint64, req *model.UpdateDramaSegmentReq) (*int32, error) {
	if req.ClearChosen {
		if req.Edit != nil {
			return nil, errcode.ErrDramaEditInvalid
		}
		return nil, nil
	}
	needsBounds := req.Edit != nil || (req.ChosenTaskID != nil && req.ResetEditOnTaskChange != nil && !*req.ResetEditOnTaskChange)
	if !needsBounds {
		return nil, nil
	}
	p, _, row, err := s.segmentSource(ctx, userID, sid)
	if err != nil {
		return nil, err
	}
	if row.Revision != req.Revision || !matchesExpectedTask(row, req) {
		return nil, errcode.ErrCanvasConflict
	}
	taskID := row.ChosenTaskID
	if req.ChosenTaskID != nil {
		id := uint64(*req.ChosenTaskID)
		taskID = &id
	}
	takes, err := ResolveSegmentTakes(ctx, s.repo, s.assets, p, row)
	if err != nil {
		return nil, err
	}
	if taskID == nil {
		return nil, errcode.ErrDramaEditInvalid
	}
	take, err := findUsableSegmentTake(takes, *taskID, false)
	if err != nil {
		return nil, err
	}
	return s.resolveSegmentSourceFrames(ctx, userID, taskID, []model.GenerationTask{take.Task})
}
