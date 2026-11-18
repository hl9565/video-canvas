package service

import (
	"context"
	"errors"
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
	_, frames, err := s.readSegmentVideoFrames(ctx, store, userID, assetID)
	return frames, err
}

// readSegmentVideoFrames 将规范帧数与同一次读取的素材指纹绑定。
func (s *DramaSegmentService) readSegmentVideoFrames(ctx context.Context, store provider.AssetStore, userID, assetID uint64) (*model.Asset, *int32, error) {
	asset, err := store.Get(ctx, userID, assetID)
	if err != nil {
		return nil, nil, err
	}
	frames, err := s.segmentVideoFramesForAsset(ctx, store, asset, userID, assetID, true)
	if err != nil {
		return nil, nil, err
	}
	return asset, frames, nil
}

// segmentVideoFramesForAsset 只按快照中的素材指纹使用缓存或冷探测。
func (s *DramaSegmentService) segmentVideoFramesForAsset(ctx context.Context, store provider.AssetStore, asset *model.Asset, userID, assetID uint64, cold bool) (*int32, error) {
	if !validProgramAsset(asset, userID, assetID, model.KindVideo) {
		return nil, errcode.ErrDramaTakeInvalid
	}
	var probe *model.AssetMediaProbe
	var err error
	if s.probes == nil {
		if !cold {
			return nil, nil
		}
		probe, err = s.probeSegmentAsset(ctx, store, asset)
	} else {
		probe, err = s.probes.Get(ctx, assetID, asset.Sha256, videoframe.NormalizedProbeVersion)
		if errors.Is(err, repository.ErrNotFound) {
			if !cold {
				return nil, nil
			}
			probe, err = s.probeSegmentAsset(ctx, store, asset)
		}
	}
	if err != nil {
		return nil, err
	}
	if probe == nil || probe.NormalizedFrames24 <= 0 {
		return nil, errcode.ErrDramaEditInvalid.WithMsg("视频没有有效帧")
	}
	return &probe.NormalizedFrames24, nil
}

// probeSegmentAsset 将归属与字节指纹已校验的素材复制到临时文件后探测，网络与解码均在数据库事务之外。
func (s *DramaSegmentService) probeSegmentAsset(ctx context.Context, store provider.AssetStore, asset *model.Asset) (*model.AssetMediaProbe, error) {
	resolved, path, cleanup, err := stageVerifiedSegmentVideo(ctx, store, asset)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	result, err := s.probeVideo(ctx, path)
	if err != nil {
		return nil, err
	}
	probe := &model.AssetMediaProbe{AssetID: resolved.ID, Sha256: resolved.Sha256, ProbeVersion: videoframe.NormalizedProbeVersion,
		FpsNum: result.FpsNum, FpsDen: result.FpsDen, NormalizedFrames24: result.NormalizedFrames24, HasAudio: result.HasAudio,
		AudioSampleRate: result.AudioSampleRate, DecodedAudioSamples: result.DecodedAudioSamples, VStartUs: result.VStartUs, AStartUs: result.AStartUs}
	if s.probes != nil {
		if err := s.probes.Upsert(ctx, probe); err != nil {
			return nil, err
		}
	}
	return probe, nil
}

func stageVerifiedSegmentVideo(ctx context.Context, store provider.AssetStore, asset *model.Asset) (*model.Asset, string, func(), error) {
	if asset.Sha256 == "" {
		if hasher, ok := store.(segmentMediaHasher); ok {
			hash, updated, err := hasher.EnsureSha256(ctx, asset.UserID, asset.ID)
			if err != nil || !validProgramAsset(updated, asset.UserID, asset.ID, model.KindVideo) || !validDramaManifestHash(hash) || updated.Sha256 != hash {
				return nil, "", nil, errcode.ErrDramaMediaUnavailable
			}
			asset = updated
		}
	}
	if !validDramaManifestHash(asset.Sha256) {
		return nil, "", nil, errcode.ErrDramaMediaUnavailable
	}
	source, err := store.Open(ctx, asset.UserID, asset.ID)
	if err != nil {
		return nil, "", nil, err
	}
	if source == nil || source.Body == nil {
		return nil, "", nil, errcode.ErrDramaMediaUnavailable
	}
	defer source.Body.Close()
	if source.Asset != nil && (!validProgramAsset(source.Asset, asset.UserID, asset.ID, model.KindVideo) || source.Asset.Sha256 != asset.Sha256) {
		return nil, "", nil, errcode.ErrDramaMediaUnavailable
	}
	file, err := os.CreateTemp("", "vc-segment-probe-*")
	if err != nil {
		return nil, "", nil, err
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	if closeErr := file.Close(); closeErr != nil {
		cleanup()
		return nil, "", nil, closeErr
	}
	if err := copyComposeMedia(ctx, source.Body, path, asset.Sha256); err != nil {
		cleanup()
		return nil, "", nil, errcode.ErrDramaMediaUnavailable
	}
	return asset, path, cleanup, nil
}

// prepareSegmentVisualUpdate 先确定目标模式，再在无写锁时验证该模式使用的媒体。
func (s *DramaSegmentService) prepareSegmentVisualUpdate(ctx context.Context, userID, sid uint64, req *model.UpdateDramaSegmentReq) (segmentVisualUpdate, error) {
	p, _, row, err := s.segmentSource(ctx, userID, sid)
	if err != nil {
		return segmentVisualUpdate{}, err
	}
	if row.Revision != req.Revision || !matchesExpectedTask(row, req) {
		return segmentVisualUpdate{}, errcode.ErrCanvasConflict
	}
	if err := checkSegmentVisualRatio(p.Ratio, req); err != nil {
		return segmentVisualUpdate{}, err
	}
	u, err := planSegmentVisual(row, req)
	if err != nil {
		return u, err
	}
	switch u.kind {
	case model.DramaVisualStill:
		u.asset, err = s.readSegmentStill(ctx, userID, *u.stillID)
	case model.DramaVisualVideo:
		err = s.prepareSegmentVideoUpdate(ctx, p, row, req, &u)
	}
	return u, err
}

// segmentMediaMetadataReader 读取归属已校验的媒体记录，不要求实现内容读取。
type segmentMediaMetadataReader interface {
	// Get 按用户和媒体 ID 读取素材元数据。
	Get(context.Context, uint64, uint64) (*model.Asset, error)
}

// segmentMediaHasher 可在事务外为没有指纹的历史素材补算内容哈希。
type segmentMediaHasher interface {
	// EnsureSha256 返回更新后的素材元数据和内容指纹。
	EnsureSha256(context.Context, uint64, uint64) (string, *model.Asset, error)
}

// readSegmentStill 验证图片归属和类型，并在素材服务支持时补齐历史指纹。
func (s *DramaSegmentService) readSegmentStill(ctx context.Context, user, id uint64) (*model.Asset, error) {
	reader, ok := s.assets.(segmentMediaMetadataReader)
	if !ok {
		return nil, errcode.ErrDramaMediaUnavailable
	}
	asset, err := reader.Get(ctx, user, id)
	if err != nil || !validProgramAsset(asset, user, id, model.KindImage) {
		return nil, errcode.ErrDramaMediaUnavailable
	}
	if hasher, ok := s.assets.(segmentMediaHasher); ok && asset.Sha256 == "" {
		hash, updated, err := hasher.EnsureSha256(ctx, user, id)
		if err != nil || !validProgramAsset(updated, user, id, model.KindImage) || hash == "" || updated.Sha256 != hash {
			return nil, errcode.ErrDramaMediaUnavailable
		}
		asset = updated
	}
	snapshot := *asset
	if store, ok := s.assets.(provider.AssetStore); ok {
		if err := checkSegmentStillReadable(ctx, store, &snapshot); err != nil {
			return nil, err
		}
	}
	return &snapshot, nil
}

// checkSegmentStillReadable 验证实际存储可打开，仍在数据库事务外执行。
func checkSegmentStillReadable(ctx context.Context, store provider.AssetStore, before *model.Asset) error {
	file, err := store.Open(ctx, before.UserID, before.ID)
	if err != nil || file == nil || file.Body == nil {
		return errcode.ErrDramaMediaUnavailable
	}
	if err := file.Body.Close(); err != nil {
		return errcode.ErrDramaMediaUnavailable
	}
	if file.Asset != nil && (!validProgramAsset(file.Asset, before.UserID, before.ID, model.KindImage) || file.Asset.Sha256 != before.Sha256) {
		return errcode.ErrDramaMediaUnavailable
	}
	return nil
}

// prepareSegmentVideoUpdate 先检查目标采用资格，再探测裁剪和特效使用的规范帧数。
func (s *DramaSegmentService) prepareSegmentVideoUpdate(ctx context.Context, p *model.DramaProject, row *model.DramaSegment, req *model.UpdateDramaSegmentReq, u *segmentVisualUpdate) error {
	if req.ClearChosen {
		return nil
	}
	taskID := row.ChosenTaskID
	if req.ChosenTaskID != nil {
		taskID = new(uint64(*req.ChosenTaskID))
	}
	if needsVideoEditFrames(req) && taskID == nil {
		return errcode.ErrDramaEditInvalid.WithMsg("编辑需要采用视频镜头")
	}
	if taskID == nil || (!needsVideoEditFrames(req) && !u.fx.HasFrameEffects() && req.ChosenTaskID == nil) {
		return nil
	}
	takes, err := ResolveSegmentTakes(ctx, s.repo, s.assets, p, row)
	if err != nil {
		return err
	}
	take, err := findUsableSegmentTake(takes, *taskID, false)
	if err != nil {
		return err
	}
	u.videoTaskID, u.videoAssetID = taskID, take.Asset.ID
	if !needsVideoEditFrames(req) && !u.fx.HasFrameEffects() && dramaVisualKind(*row) == model.DramaVisualVideo {
		return s.snapshotSegmentVideo(ctx, row.UserID, u)
	}
	return s.probeSegmentUpdateVideo(ctx, row.UserID, take, u)
}

// snapshotSegmentVideo 为不用探针的普通采用也记录素材身份和指纹。
func (s *DramaSegmentService) snapshotSegmentVideo(ctx context.Context, user uint64, u *segmentVisualUpdate) error {
	store, ok := s.assets.(provider.AssetStore)
	if !ok {
		return nil // 仅用于测试的视图读取器已在候选准入中验证过素材 ID。
	}
	asset, err := store.Get(ctx, user, u.videoAssetID)
	if err != nil || !validProgramAsset(asset, user, u.videoAssetID, model.KindVideo) {
		return errcode.ErrDramaMediaUnavailable
	}
	u.asset = new(*asset)
	return nil
}

// probeSegmentUpdateVideo 把目标视频的元数据和规范帧数绑定到本次更新。
func (s *DramaSegmentService) probeSegmentUpdateVideo(ctx context.Context, user uint64, take *resolvedSegmentTake, u *segmentVisualUpdate) error {
	store, ok := s.assets.(provider.AssetStore)
	if !ok {
		return errcode.ErrDramaEditInvalid.WithMsg("视频探测依赖不可用")
	}
	asset, frames, err := s.readSegmentVideoFrames(ctx, store, user, take.Asset.ID)
	if err != nil {
		return err
	}
	u.asset, u.frames = new(*asset), frames
	return nil
}

// recheckSegmentUpdateAsset 先锁素材行，再从事务读取归属、类型和指纹，避免使用过期的探测结果。
func (s *DramaSegmentService) recheckSegmentUpdateAsset(ctx context.Context, tx repository.DramaTx, user uint64, before *model.Asset) error {
	if before == nil {
		return nil
	}
	read := tx.ComposeAsset
	if locker, ok := tx.(dramaProgramAssetLocker); ok {
		if err := locker.LockProgramAssets(ctx, user, []uint64{before.ID}); err != nil {
			return err
		}
	} else if before.Kind == model.KindVideo {
		// 内存仓储可用独立的视频素材夹具；生产仓储始终通过持锁事务读取。
		if reader, ok := s.assets.(segmentMediaMetadataReader); ok {
			read = reader.Get
		}
	}
	asset, err := read(ctx, user, before.ID)
	if err != nil && !errors.Is(err, repository.ErrNotFound) && !errors.Is(err, provider.ErrAssetNotFound) {
		return err
	}
	if !validProgramAsset(asset, user, before.ID, before.Kind) || asset.Sha256 != before.Sha256 {
		return errcode.ErrDramaMediaUnavailable
	}
	return nil
}

// recheckSegmentVideoOutput 确认锁内仍是同一条任务和视频产物。
func recheckSegmentVideoOutput(take *resolvedSegmentTake, u segmentVisualUpdate) error {
	if u.videoTaskID == nil || take.Task.ID != *u.videoTaskID || take.Asset.ID != u.videoAssetID {
		return errcode.ErrDramaMediaUnavailable
	}
	return nil
}

// recheckKeptSegmentVideo 确认保留的镜头仍然引用本次探测的视频。
func recheckKeptSegmentVideo(ctx context.Context, tx repository.DramaTx, row *model.DramaSegment, req *model.UpdateDramaSegmentReq, u segmentVisualUpdate) error {
	if u.videoTaskID == nil || req.ChosenTaskID != nil {
		return nil
	}
	if row.ChosenTaskID == nil || *row.ChosenTaskID != *u.videoTaskID {
		return errcode.ErrCanvasConflict
	}
	tasks, err := tx.TasksByIDs(ctx, row.UserID, []uint64{*u.videoTaskID})
	if err != nil {
		return err
	}
	if len(tasks) != 1 || tasks[0].Kind != model.KindVideo || tasks[0].Status != model.TaskSucceeded {
		return errcode.ErrDramaTakeInvalid
	}
	id, err := segmentOutputAsset(&tasks[0], model.KindVideo)
	if err != nil {
		return err
	}
	if u.asset == nil || u.asset.ID != id {
		return errcode.ErrDramaMediaUnavailable
	}
	return nil
}

// resolveImportAdoptFrames 在事务外拒绝合成段采用并探测目标剪辑。
func (s *DramaSegmentService) resolveImportAdoptFrames(ctx context.Context, userID, sid uint64, req *model.ImportDramaTakesReq, tid uint64) (*int32, error) {
	if !req.Adopt {
		return nil, nil
	}
	_, _, seg, err := s.segmentSource(ctx, userID, sid)
	if err != nil {
		return nil, err
	}
	if err := requireDramaVideoSegment(seg); err != nil {
		return nil, err
	}
	if !needsTakeSelectionFrames(seg, req.ResetEditOnTaskChange) {
		return nil, nil
	}
	return s.resolveSegmentSourceFrames(ctx, userID, &tid, nil)
}
