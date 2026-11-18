package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/jsonhash"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/provider"
	"video-canvas/internal/repository"
)

type dramaManifestReader interface {
	DramaTakeReader
	ComposeAsset(context.Context, uint64, uint64) (*model.Asset, error)
	Segments(context.Context, uint64, uint64) ([]model.DramaSegment, error)
	SegmentTasks(context.Context, uint64, uint64, string) ([]model.GenerationTask, error)
}

type dramaManifestSource struct {
	project model.DramaProject
	episode model.DramaEpisode
	rows    []model.DramaSegment
	input   dramaComposeInput
	stageC  *dramaManifestV2Source // 仅在合成提交门控使用，不进入 v1 清单。
}

// dramaProbeVideo 让测试注入探测函数，生产默认复用规范化探测器。
func dramaProbeVideo(d DramaDeps) func(context.Context, string) (videoframe.NormalizedProbe, error) {
	if d.ProbeVideo != nil {
		return d.ProbeVideo
	}
	return videoframe.New(videoframe.Options{Timeout: dramaComposeTimeout}).ProbeNormalized24FPS
}

// ResolveEpisodeManifest 在无写锁、无媒体事务内读取采用及精裁，按内容哈希缓存探测结果。
func (s *DramaService) ResolveEpisodeManifest(ctx context.Context, user, id uint64, no int) (*model.DramaManifestView, error) {
	source, err := s.loadDramaManifestSource(ctx, user, id, no)
	if err != nil {
		return nil, err
	}
	// 2. 所有素材读取、下载及探测均在事务外，哈希仅包含冻结媒体信息与剪辑参数。
	return s.resolveDramaManifestSource(ctx, source)
}

// PrepareOrResolveEpisodeManifest 仅在全部探针已缓存时直接返回完整清单；遇到未缓存冷素材时在受控信号量池中异步预热并返回 202 准备视图。
func (s *DramaService) PrepareOrResolveEpisodeManifest(ctx context.Context, user, id uint64, no int) (*model.DramaManifestView, *model.DramaManifestPrepareView, error) {
	source, err := s.loadDramaManifestSource(ctx, user, id, no)
	if err != nil {
		return nil, nil, err
	}
	view, hit, err := s.resolveCachedDramaManifestSource(ctx, source)
	if err != nil {
		return nil, nil, err
	}
	if hit {
		s.clearManifestWarmupErr(user, id, source)
		return view, nil, nil
	}
	if err := s.ensureManifestWarmup(user, id, source); err != nil {
		return nil, nil, err
	}
	skipped := make([]int, len(source.input.SkippedSeqs))
	copy(skipped, source.input.SkippedSeqs)
	return nil, &model.DramaManifestPrepareView{
		Status:         "preparing",
		SegmentVersion: source.episode.SegmentVersion,
		SkippedSeqs:    skipped,
		Diagnostics:    []string{},
	}, nil
}

func (s *DramaService) loadDramaManifestSource(ctx context.Context, user, id uint64, no int) (*dramaManifestSource, error) {
	// 1. 校验项目与集归属，采用准入与片段视图共享来源、绑定与 QC 规则。
	p, err := s.repo.Project(ctx, user, id, false)
	if err != nil {
		return nil, dramaError(err)
	}
	eps, err := s.repo.Episodes(ctx, user, id)
	if err != nil {
		return nil, err
	}
	var ep *model.DramaEpisode
	for i := range eps {
		if eps[i].No == no {
			ep = &eps[i]
			break
		}
	}
	if ep == nil {
		return nil, errcode.ErrDramaNotFound
	}
	reader, ok := s.repo.(dramaManifestReader)
	if !ok {
		return nil, errcode.ErrInternal
	}
	source, err := readDramaManifestSource(ctx, reader, p, *ep)
	if err != nil {
		return nil, dramaError(err)
	}
	return source, nil
}

func dramaManifestWarmupKey(user, drama uint64, source *dramaManifestSource) (string, error) {
	return jsonhash.CanonicalSHA256(struct {
		User           uint64   `json:"user"`
		Drama          uint64   `json:"drama"`
		EpisodeID      uint64   `json:"episode_id"`
		SegmentVersion int64    `json:"segment_version"`
		AssetIDs       []uint64 `json:"asset_ids"`
		SkippedSeqs    []int    `json:"skipped_seqs"`
	}{
		User:           user,
		Drama:          drama,
		EpisodeID:      source.episode.ID,
		SegmentVersion: source.episode.SegmentVersion,
		AssetIDs:       source.input.AssetIDs,
		SkippedSeqs:    source.input.SkippedSeqs,
	})
}

func (s *DramaService) clearManifestWarmupErr(user, id uint64, source *dramaManifestSource) {
	key, err := dramaManifestWarmupKey(user, id, source)
	if err != nil {
		return
	}
	s.clearManifestWarmupKey(key)
}

func (s *DramaService) clearManifestWarmupKey(key string) {
	p := s.preview
	p.mu.Lock()
	delete(p.manifestErr, key)
	p.mu.Unlock()
}

func (s *DramaService) ensureManifestWarmup(user, id uint64, source *dramaManifestSource) error {
	key, err := dramaManifestWarmupKey(user, id, source)
	if err != nil {
		return err
	}
	return s.ensureManifestProbeWork(key, true, func(ctx context.Context) error {
		_, err := s.resolveDramaManifestSource(ctx, source)
		return err
	})
}

// ensureManifestProbeWork 让两版清单共用预热容量和失败缓存，解析及缓存键仍各自隔离。
func (s *DramaService) ensureManifestProbeWork(key string, consumeErr bool, work func(context.Context) error) error {
	p := s.preview
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return errcode.ErrDramaJobBusy
	}
	now := p.now()
	ttl := p.manifestErrTTL()
	if prev, ok := p.manifestErr[key]; ok {
		active := now.Sub(prev.at) <= ttl
		if consumeErr || !active {
			delete(p.manifestErr, key)
		}
		if active {
			p.mu.Unlock()
			return prev.err
		}
	}
	if _, inFlight := p.manifestInFlight[key]; inFlight {
		p.mu.Unlock()
		return nil
	}
	if len(p.manifestInFlight) >= 128 {
		p.mu.Unlock()
		return errcode.ErrDramaJobBusy
	}
	p.manifestInFlight[key] = struct{}{}
	p.wg.Add(1)
	execTimeout := p.execTimeout
	p.mu.Unlock()

	go s.runManifestWarmup(key, work, execTimeout)
	return nil
}

func (s *DramaService) runManifestWarmup(key string, work func(context.Context) error, execTimeout time.Duration) {
	p := s.preview
	defer p.wg.Done()
	workCtx, cancel := context.WithTimeout(p.ctx, execTimeout)
	defer cancel()

	var runErr error
	select {
	case p.sem <- struct{}{}:
		func() {
			defer func() { <-p.sem }()
			runErr = work(workCtx)
		}()
	case <-workCtx.Done():
		runErr = workCtx.Err()
	}

	p.mu.Lock()
	delete(p.manifestInFlight, key)
	if !p.closed && runErr != nil {
		p.recordManifestErrLocked(key, runErr)
	}
	p.mu.Unlock()
}

// recordManifestErrLocked 先清理过期失败记录，若仍达到上限则只淘汰最早一条，防止全量清空导致其他分集丢失单次报错。
func (p *dramaPreviewStore) recordManifestErrLocked(key string, err error) {
	now := p.now()
	if _, exists := p.manifestErr[key]; !exists && len(p.manifestErr) >= maxManifestErrEntries {
		p.pruneExpiredManifestErrLocked(now)
		for len(p.manifestErr) >= maxManifestErrEntries {
			if !p.evictOldestManifestErrLocked() {
				break
			}
		}
	}
	p.manifestErrSeq++
	p.manifestErr[key] = manifestWarmupErr{err: err, at: now, seq: p.manifestErrSeq}
}

func (p *dramaPreviewStore) pruneExpiredManifestErrLocked(now time.Time) {
	ttl := p.manifestErrTTL()
	for k, entry := range p.manifestErr {
		if now.Sub(entry.at) > ttl {
			delete(p.manifestErr, k)
		}
	}
}

func (p *dramaPreviewStore) evictOldestManifestErrLocked() bool {
	oldestKey := ""
	var oldest manifestWarmupErr
	for k, entry := range p.manifestErr {
		if oldestKey == "" || entry.at.Before(oldest.at) || (entry.at.Equal(oldest.at) && entry.seq < oldest.seq) {
			oldestKey = k
			oldest = entry
		}
	}
	if oldestKey == "" {
		return false
	}
	delete(p.manifestErr, oldestKey)
	return true
}

// readDramaManifestSource 仅跳过未采用片段；非法采用不能静默补黑或退回其他 take。
func readDramaManifestSource(ctx context.Context, reader dramaManifestReader, p *model.DramaProject, ep model.DramaEpisode) (*dramaManifestSource, error) {
	rows, err := reader.Segments(ctx, p.UserID, ep.ID)
	if err != nil {
		return nil, err
	}
	return readDramaManifestSourceRows(ctx, reader, p, ep, rows)
}

// sortDramaManifestRows 固定时间轴按序号和主键排序；主键也用于 V2 锚点。
func sortDramaManifestRows(rows []model.DramaSegment) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Seq == rows[j].Seq {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].Seq < rows[j].Seq
	})
}

// readDramaManifestSourceRows 按 v1 的历史采用规则读取合成输入。
func readDramaManifestSourceRows(ctx context.Context, reader dramaManifestReader, p *model.DramaProject, ep model.DramaEpisode, rows []model.DramaSegment) (*dramaManifestSource, error) {
	sortDramaManifestRows(rows)
	source := &dramaManifestSource{project: *p, episode: ep, rows: []model.DramaSegment{}, input: dramaComposeInput{EpisodeID: ep.ID, SegmentVersion: ep.SegmentVersion, AssetIDs: []uint64{}, SkippedSeqs: []int{}}}
	for _, row := range rows {
		if row.ChosenTaskID == nil {
			source.input.SkippedSeqs = append(source.input.SkippedSeqs, row.Seq)
			continue
		}
		takes, err := ResolveSegmentTakes(ctx, reader, manifestTakeAssets{reader}, p, &row)
		if err != nil {
			return nil, err
		}
		take, err := findUsableSegmentTake(takes, *row.ChosenTaskID, false)
		if err != nil {
			return nil, err
		}
		id, err := composeChosenAsset([]model.GenerationTask{take.Task}, *row.ChosenTaskID)
		if err != nil {
			return nil, err
		}
		source.rows = append(source.rows, row)
		source.input.AssetIDs = append(source.input.AssetIDs, id)
	}
	if len(source.rows) == 0 {
		return nil, errcode.ErrDramaNothingToCompose
	}
	return source, nil
}

// resolveDramaManifestSource 将采用资产与缓存的规范探测转换为不含 URL 和时间戳的清单。
func (s *DramaService) resolveDramaManifestSource(ctx context.Context, source *dramaManifestSource) (*model.DramaManifestView, error) {
	assets := make([]*model.Asset, len(source.rows))
	probes := make([]*model.AssetMediaProbe, len(source.rows))
	for i, row := range source.rows {
		asset, probe, err := s.dramaManifestProbe(ctx, source.project.UserID, source.input.AssetIDs[i])
		if err != nil {
			return nil, errcode.ErrDramaEditInvalid.WithMsg(fmt.Sprintf("片段 %d 的采用视频不可读取或探测", row.Seq))
		}
		assets[i] = asset
		probes[i] = probe
	}
	return s.assembleEpisodeManifest(source, assets, probes)
}

// resolveCachedDramaManifestSource 仅读取已命中缓存的视频探针来构建清单；任一片段未缓存时直接返回 hit=false，不下载不解码。
func (s *DramaService) resolveCachedDramaManifestSource(ctx context.Context, source *dramaManifestSource) (*model.DramaManifestView, bool, error) {
	assets := make([]*model.Asset, len(source.rows))
	probes := make([]*model.AssetMediaProbe, len(source.rows))
	for i, row := range source.rows {
		asset, probe, hit, err := s.lookupCachedDramaManifestProbe(ctx, source.project.UserID, source.input.AssetIDs[i])
		if err != nil {
			return nil, false, errcode.ErrDramaEditInvalid.WithMsg(fmt.Sprintf("片段 %d 的采用视频不可读取或探测", row.Seq))
		}
		if !hit {
			return nil, false, nil
		}
		assets[i] = asset
		probes[i] = probe
	}
	view, err := s.assembleEpisodeManifest(source, assets, probes)
	if err != nil {
		return nil, false, err
	}
	return view, true, nil
}

func (s *DramaService) assembleEpisodeManifest(source *dramaManifestSource, assets []*model.Asset, probes []*model.AssetMediaProbe) (*model.DramaManifestView, error) {
	skipped := make([]int, len(source.input.SkippedSeqs))
	copy(skipped, source.input.SkippedSeqs)
	manifest := model.DramaEpisodeManifest{
		Version:        model.CurrentDramaManifestSchemaVersion,
		RenderVersion:  model.CurrentDramaManifestRenderVersion,
		EpisodeID:      source.episode.ID,
		SegmentVersion: source.episode.SegmentVersion,
		FPS:            24,
		SampleRate:     model.CurrentDramaManifestSampleRate,
		SkippedSeqs:    skipped,
		Clips:          make([]model.DramaManifestClip, 0, len(source.rows)),
		Program:        model.DramaEpisodeProgram{Version: 1},
	}
	clips := make([]videoframe.ManifestClip, 0, len(source.rows))
	for i, row := range source.rows {
		asset, probe := assets[i], probes[i]
		clip := manifestVideoClip(row, probe)
		clips = append(clips, clip)
		manifest.Clips = append(manifest.Clips, model.DramaManifestClip{SegmentID: row.ID, TaskID: *row.ChosenTaskID, AssetID: asset.ID, Sha256: asset.Sha256, Seq: row.Seq,
			SourceInF: row.SourceInF, NormalizedFrames24: probe.NormalizedFrames24, HasAudio: probe.HasAudio, DecodedAudioSamples: probe.DecodedAudioSamples, VStartUs: probe.VStartUs, AStartUs: probe.AStartUs,
			MuteOriginal: row.MuteOriginal, AudioGainDB: row.AudioGainDB, NativeIsDialogue: row.NativeIsDialogue, AvPtsOffsetMs: row.AvPtsOffsetMs})
	}
	normalized, total, err := videoframe.NormalizeManifestClips(clips)
	if err != nil {
		return nil, errcode.ErrDramaEditInvalid.WithMsg(err.Error())
	}
	var cursor int32
	for i, clip := range normalized {
		c := &manifest.Clips[i]
		c.SourceOutF = *clip.SourceOutF
		c.MasterInF = cursor
		cursor += c.SourceOutF - c.SourceInF
		c.MasterOutF = cursor
	}
	manifest.TotalFrames = total
	hash, err := jsonhash.CanonicalSHA256(manifest)
	if err != nil {
		return nil, err
	}
	return &model.DramaManifestView{Manifest: manifest, ManifestHash: hash, SegmentVersion: source.episode.SegmentVersion, SkippedSeqs: source.input.SkippedSeqs, Diagnostics: []string{}}, nil
}

// manifestVideoClip 只转换 0036 提供的六个编辑字段以及缓存中的时间基。
func manifestVideoClip(row model.DramaSegment, p *model.AssetMediaProbe) videoframe.ManifestClip {
	return videoframe.ManifestClip{SegmentID: row.ID, Seq: row.Seq, SourceInF: row.SourceInF, SourceOutF: row.SourceOutF, NormalizedFrames24: p.NormalizedFrames24,
		HasAudio: p.HasAudio, DecodedAudioSamples: p.DecodedAudioSamples, VStartUs: p.VStartUs, AStartUs: p.AStartUs, MuteOriginal: row.MuteOriginal, AudioGainDB: row.AudioGainDB, NativeIsDialogue: row.NativeIsDialogue, AvPtsOffsetMs: row.AvPtsOffsetMs}
}

// lookupCachedDramaManifestProbe 只读素材元数据与探针缓存；探针未命中返回 hit=false，不做下载/解码。
func (s *DramaService) lookupCachedDramaManifestProbe(ctx context.Context, user, id uint64) (*model.Asset, *model.AssetMediaProbe, bool, error) {
	store, ok := s.assets.(provider.AssetStore)
	if !ok || s.probes == nil {
		return nil, nil, false, errcode.ErrInternal
	}
	asset, err := store.Get(ctx, user, id)
	if err != nil {
		return nil, nil, false, err
	}
	if asset == nil || asset.ID != id || asset.UserID != user || asset.Kind != model.KindVideo || !validDramaManifestHash(asset.Sha256) {
		return nil, nil, false, errcode.ErrDramaTakeInvalid
	}
	probe, err := s.probes.Get(ctx, id, asset.Sha256, videoframe.NormalizedProbeVersion)
	if err == nil {
		return asset, probe, true, nil
	}
	if errors.Is(err, repository.ErrNotFound) {
		return asset, nil, false, nil
	}
	return nil, nil, false, err
}

// dramaManifestProbe 先核验当前素材元数据；缓存未命中才在锁外校验下载指纹并解码。
func (s *DramaService) dramaManifestProbe(ctx context.Context, user, id uint64) (*model.Asset, *model.AssetMediaProbe, error) {
	asset, probe, hit, err := s.lookupCachedDramaManifestProbe(ctx, user, id)
	if err != nil {
		return nil, nil, err
	}
	if hit {
		return asset, probe, nil
	}
	file, err := os.CreateTemp("", "vc-drama-probe-*")
	if err != nil {
		return nil, nil, err
	}
	path := file.Name()
	defer func() { _ = os.Remove(path) }()
	if err := file.Close(); err != nil {
		return nil, nil, err
	}
	if err := s.downloadComposeVideo(ctx, user, id, path, asset.Sha256); err != nil {
		return nil, nil, err
	}
	result, err := s.probeVideo(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	probe = &model.AssetMediaProbe{AssetID: id, Sha256: asset.Sha256, ProbeVersion: videoframe.NormalizedProbeVersion, FpsNum: result.FpsNum, FpsDen: result.FpsDen, NormalizedFrames24: result.NormalizedFrames24, HasAudio: result.HasAudio, AudioSampleRate: result.AudioSampleRate, DecodedAudioSamples: result.DecodedAudioSamples, VStartUs: result.VStartUs, AStartUs: result.AStartUs}
	if err := s.probes.Upsert(ctx, probe); err != nil {
		return nil, nil, err
	}
	return asset, probe, nil
}

// manifestTakeAssets 只在当前 reader 的数据库边界内核验用户视频，不在合成事务内下载素材。
type manifestTakeAssets struct{ reader dramaManifestReader }

// View 复用合成素材元数据读取，避免采用准入依赖源画布当前是否仍存活。
func (a manifestTakeAssets) View(ctx context.Context, user, id uint64) (*model.AssetView, error) {
	asset, err := a.reader.ComposeAsset(ctx, user, id)
	if err != nil || asset == nil {
		return nil, err
	}
	if asset.UserID != user {
		return nil, nil
	}
	return &model.AssetView{ID: asset.ID, Kind: asset.Kind}, nil
}

// dramaProbeAudio 保持音频探针独立于 v1 视频探针。
func dramaProbeAudio(d DramaDeps) func(context.Context, string) (videoframe.AudioProbe, error) {
	if d.ProbeAudio != nil {
		return d.ProbeAudio
	}
	return videoframe.New(videoframe.Options{Timeout: dramaComposeTimeout}).ProbeProgramAudio
}

// lookupCachedDramaManifestAudioProbe 只读归属与版本化缓存，空哈希交给锁外预热。
func (s *DramaService) lookupCachedDramaManifestAudioProbe(ctx context.Context, asset *model.Asset) (*model.AssetMediaProbe, error) {
	if s.probes == nil || (asset.Sha256 != "" && !validDramaManifestHash(asset.Sha256)) {
		return nil, errcode.ErrDramaMediaUnavailable
	}
	if asset.Sha256 == "" {
		return nil, nil
	}
	probe, err := s.probes.Get(ctx, asset.ID, asset.Sha256, videoframe.NormalizedAudioProbeVersion)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, nil
	}
	if err == nil && (probe == nil || !probe.HasAudio || probe.DecodedAudioSamples <= 0) {
		return nil, errcode.ErrDramaMediaUnavailable
	}
	return probe, err
}

// dramaManifestAudioProbe 在事务外补写历史素材哈希，并只缓存有效的音频探针。
func (s *DramaService) dramaManifestAudioProbe(ctx context.Context, user, id uint64) (string, *model.AssetMediaProbe, error) {
	asset, err := s.programAsset(ctx, user, id)
	if err != nil || !validProgramAsset(asset, user, id, model.KindAudio) {
		return "", nil, errcode.ErrDramaMediaUnavailable
	}
	if asset.Sha256 == "" {
		hasher, ok := s.assets.(segmentMediaHasher)
		if !ok {
			return "", nil, errcode.ErrDramaMediaUnavailable
		}
		hash, updated, err := hasher.EnsureSha256(ctx, user, id)
		if err != nil || !validProgramAsset(updated, user, id, model.KindAudio) || updated.Sha256 != hash || !validDramaManifestHash(hash) {
			return "", nil, errcode.ErrDramaMediaUnavailable
		}
		asset = updated
	}
	probe, err := s.probeDramaManifestAudioAsset(ctx, user, asset)
	return asset.Sha256, probe, err
}

// probeDramaManifestAudioAsset 使用同一份元数据核验下载并填充对应探针版本。
func (s *DramaService) probeDramaManifestAudioAsset(ctx context.Context, user uint64, asset *model.Asset) (*model.AssetMediaProbe, error) {
	if probe, err := s.lookupCachedDramaManifestAudioProbe(ctx, asset); err != nil || probe != nil {
		return probe, err
	}
	dir, err := os.MkdirTemp("", "vc-drama-audio-probe-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "audio")
	if err := s.downloadProgramMedia(ctx, user, path, *asset); err != nil {
		return nil, err
	}
	result, err := s.probeAudio(ctx, path)
	if err != nil {
		return nil, err
	}
	if result.DecodedAudioSamples <= 0 {
		return nil, videoframe.ErrDecode
	}
	probe := &model.AssetMediaProbe{AssetID: asset.ID, Sha256: asset.Sha256, ProbeVersion: videoframe.NormalizedAudioProbeVersion,
		HasAudio: true, AudioSampleRate: model.DramaProgramSampleRate, DecodedAudioSamples: result.DecodedAudioSamples, AStartUs: result.AStartUs}
	if err := s.probes.Upsert(ctx, probe); err != nil {
		return nil, err
	}
	return probe, nil
}
