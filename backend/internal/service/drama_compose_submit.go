package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/jsonhash"
	"video-canvas/internal/repository"
)

// estimateCompose 执行同一 tx1 和锁外清单校验，完全不访问模型或积分。
func (s *DramaService) estimateCompose(ctx context.Context, user, id uint64, req *model.DramaJobReq) (int, error) {
	intent, err := newDramaJobIntent(id, nil, req)
	if err != nil {
		return 0, err
	}
	source, hit, err := s.prepareComposeTx(ctx, user, id, intent)
	if err != nil || hit != nil {
		return 0, dramaError(err)
	}
	_, err = s.freezeComposeManifest(ctx, source, intent.req.Compose)
	return 0, dramaError(err)
}

// createComposeJob 分离短事务和媒体探测，tx2 再校验全部冻结条件后只入队一次。
func (s *DramaService) createComposeJob(ctx context.Context, user, id uint64, intent *dramaJobIntent) (*model.DramaJobView, error) {
	// 1. 项目锁内联合查键，再校验集状态和请求参数。
	source, hit, err := s.prepareComposeTx(ctx, user, id, intent)
	if err != nil {
		return nil, dramaError(err)
	}
	if hit != nil {
		v := jobView(*hit)
		return &v, nil
	}
	// 2. 释放全部写锁后下载、探测并生成冻结清单。
	input, err := s.freezeComposeManifest(ctx, source, intent.req.Compose)
	if err != nil {
		return nil, err
	}
	var job *model.DramaJob
	created := false
	err = s.repo.WithTx(ctx, func(tx repository.DramaTx) error {
		p, err := tx.Project(ctx, user, id, true)
		if err != nil {
			return err
		}
		job, err = resolveDramaJobIntent(ctx, tx, user, id, intent)
		if err != nil || job != nil {
			return err
		}
		eps, err := s.prepareDramaJob(ctx, tx, p, &intent.req)
		if err != nil {
			return err
		}
		if err = s.recheckCompose(ctx, tx, p, eps[0], source, input); err != nil {
			return err
		}
		job, err = enqueueCompose(ctx, tx, p, intent, input)
		created = err == nil
		return err
	})
	if errors.Is(err, repository.ErrDuplicate) {
		return s.recoverDramaJob(ctx, user, id, intent)
	}
	if err != nil {
		return nil, dramaError(err)
	}
	if created {
		s.publish(ctx, job)
	}
	v := jobView(*job)
	return &v, nil
}

// prepareComposeTx 是 tx1：所有重放先返回旧任务，避免探测或可变配置影响同意图重试。
func (s *DramaService) prepareComposeTx(ctx context.Context, user, id uint64, intent *dramaJobIntent) (*dramaManifestSource, *model.DramaJob, error) {
	var source *dramaManifestSource
	var hit *model.DramaJob
	err := s.repo.WithTx(ctx, func(tx repository.DramaTx) error {
		p, err := tx.Project(ctx, user, id, true)
		if err != nil {
			return err
		}
		hit, err = resolveDramaJobIntent(ctx, tx, user, id, intent)
		if err != nil || hit != nil {
			return err
		}
		eps, err := s.prepareDramaJob(ctx, tx, p, &intent.req)
		if err != nil {
			return err
		}
		if err = checkComposeVersion(eps[0], intent.req.Compose); err != nil {
			return err
		}
		source, err = readDramaManifestSource(ctx, tx, p, eps[0])
		return err
	})
	return source, hit, err
}

// checkComposeVersion 校验显式版本；旧 schema=0 无新参数仍使用旧整段输入。
func checkComposeVersion(ep model.DramaEpisode, c *model.DramaComposeReq) error {
	if c != nil && c.SegmentVersion != nil && *c.SegmentVersion != ep.SegmentVersion {
		return errcode.ErrDramaManifestStale
	}
	return nil
}

// freezeComposeManifest 只在新版输入上冻结清单；schema=0 的旧输入保留原 worker 行为。
func (s *DramaService) freezeComposeManifest(ctx context.Context, source *dramaManifestSource, c *model.DramaComposeReq) (*dramaComposeInput, error) {
	input := source.input
	if source.episode.EditSchema == 0 && emptyDramaCompose(c) {
		return &input, nil
	}
	view, err := s.resolveDramaManifestSource(ctx, source)
	if err != nil {
		return nil, err
	}
	if c != nil && c.ManifestHash != "" && c.ManifestHash != view.ManifestHash {
		return nil, errcode.ErrDramaManifestStale
	}
	input.AspectRatio = source.project.Ratio
	input.Manifest = &view.Manifest
	input.ManifestHash = view.ManifestHash
	input.Size = "720p"
	input.Variants = []string{"clean"}
	if c != nil {
		if c.Size != "" {
			input.Size = c.Size
		}
		if len(c.Variants) > 0 {
			input.Variants = canonicalDramaVariants(c.Variants)
		}
	}
	return &input, nil
}

// recheckCompose 是 tx2：项目→集→片段锁序下重验画幅、版本、采用和内容指纹，不进行媒体 I/O。
func (s *DramaService) recheckCompose(ctx context.Context, tx repository.DramaTx, p *model.DramaProject, ep model.DramaEpisode, source *dramaManifestSource, input *dramaComposeInput) error {
	if input.Manifest != nil {
		if p.Ratio != input.AspectRatio {
			return errcode.ErrDramaManifestStale
		}
		locked, err := tx.LockEpisodes(ctx, p.UserID, []uint64{ep.ID})
		if err != nil {
			return err
		}
		if len(locked) != 1 {
			return repository.ErrNotFound
		}
		ep = locked[0]
		ids := make([]uint64, 0, len(source.rows))
		for _, row := range source.rows {
			ids = append(ids, row.ID)
		}
		if _, err = tx.LockSegments(ctx, p.UserID, ids); err != nil {
			return err
		}
	}
	if ep.ID != input.EpisodeID || ep.SegmentVersion != input.SegmentVersion {
		return errcode.ErrDramaManifestStale
	}
	current, err := readDramaManifestSource(ctx, tx, p, ep)
	if err != nil {
		return err
	}
	if !slices.Equal(current.input.AssetIDs, input.AssetIDs) || !slices.Equal(current.input.SkippedSeqs, input.SkippedSeqs) {
		return errcode.ErrDramaManifestStale
	}
	before, err := composeMediaSignature(source.rows)
	if err != nil {
		return err
	}
	after, err := composeMediaSignature(current.rows)
	if err != nil {
		return err
	}
	if before != after {
		return errcode.ErrDramaManifestStale
	}
	if input.Manifest != nil {
		return recheckComposeHashes(ctx, tx, p.UserID, input)
	}
	return nil
}

// recheckComposeHashes 事务内只读素材元数据，并核验冻结清单自身没有被改写。
func recheckComposeHashes(ctx context.Context, tx repository.DramaTx, user uint64, input *dramaComposeInput) error {
	for _, clip := range input.Manifest.Clips {
		asset, err := tx.ComposeAsset(ctx, user, clip.AssetID)
		if err != nil {
			return errcode.ErrDramaManifestStale
		}
		if asset.Kind != model.KindVideo || asset.Sha256 != clip.Sha256 {
			return errcode.ErrDramaManifestStale
		}
	}
	hash, err := jsonhash.CanonicalSHA256(input.Manifest)
	if err != nil {
		return err
	}
	if hash != input.ManifestHash {
		return errcode.ErrDramaManifestStale
	}
	return nil
}

// enqueueCompose 在 tx2 同事务写入请求签名、完整输入和唯一的零费用队列任务。
func enqueueCompose(ctx context.Context, tx repository.DramaTx, p *model.DramaProject, intent *dramaJobIntent, input *dramaComposeInput) (*model.DramaJob, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	job := &model.DramaJob{UserID: p.UserID, DramaID: p.ID, Lane: "user", Step: "compose", Episodes: intent.req.Episodes, Status: "queued", ConfigSnapshot: []byte(`{}`), InputJSON: raw, Output: []byte(`{}`), ApprovalID: intent.approvalID, RequestIntentSig: intent.signature}
	if intent.req.ComposeRequestKey != "" {
		key := intent.req.ComposeRequestKey
		job.ComposeRequestKey = &key
	}
	if err := tx.InsertJob(ctx, job); err != nil {
		return nil, err
	}
	return job, nil
}

// composeMediaSignature 只复核影响既有采用视频的字段，提示词或转写稿修订不能使媒体快照误报过期。
func composeMediaSignature(rows []model.DramaSegment) (string, error) {
	type mediaSegment struct {
		SegmentID    uint64
		Seq          int
		ChosenTaskID *uint64
		Edit         model.DramaSegmentEditView
	}
	media := make([]mediaSegment, 0, len(rows))
	for _, row := range rows {
		media = append(media, mediaSegment{SegmentID: row.ID, Seq: row.Seq, ChosenTaskID: row.ChosenTaskID, Edit: model.DramaSegmentEditView{
			SourceInF: row.SourceInF, SourceOutF: row.SourceOutF, MuteOriginal: row.MuteOriginal, AudioGainDB: row.AudioGainDB, NativeIsDialogue: row.NativeIsDialogue, AvPtsOffsetMs: row.AvPtsOffsetMs,
		}})
	}
	return jsonhash.CanonicalSHA256(media)
}
