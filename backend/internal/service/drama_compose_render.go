package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/jsonhash"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/repository"
)

// DramaManifestComposer 渲染按帧冻结的输入，预览和成片使用同一 CPU 合成器。
type DramaManifestComposer interface {
	// ComposeManifest 按 24fps 与 48kHz 规范渲染全部输出。
	ComposeManifest(context.Context, videoframe.ComposeManifestInput) error
}

// renderFrozenManifest 为清单绑定临时源路径；冻结与公开 JSON 中始终不出现本地路径。
func (s *DramaService) renderFrozenManifest(ctx context.Context, user uint64, input *dramaComposeInput, dir string, destinations []string, rf *videoframe.FrameRange) error {
	renderer, ok := s.compose.(DramaManifestComposer)
	if !ok || input.Manifest == nil || len(destinations) == 0 {
		return errcode.ErrInternal
	}
	if input.Manifest.Version != model.CurrentDramaManifestSchemaVersion ||
		input.Manifest.RenderVersion != model.CurrentDramaManifestRenderVersion ||
		input.Manifest.FPS != 24 ||
		input.Manifest.SampleRate != model.CurrentDramaManifestSampleRate {
		return errcode.ErrDramaEditInvalid.WithMsg("不支持的清单渲染版本")
	}
	render := videoframe.ComposeManifestInput{RangeF: rf}
	for i, c := range input.Manifest.Clips {
		path := filepath.Join(dir, fmt.Sprintf("source-%d.mp4", i))
		if err := s.downloadComposeVideo(ctx, user, c.AssetID, path, c.Sha256); err != nil {
			return err
		}
		end := c.SourceOutF
		render.Clips = append(render.Clips, videoframe.ManifestClip{SegmentID: c.SegmentID, Seq: c.Seq, SourcePath: path, SourceInF: c.SourceInF, SourceOutF: &end, NormalizedFrames24: c.NormalizedFrames24,
			HasAudio: c.HasAudio, DecodedAudioSamples: c.DecodedAudioSamples, VStartUs: c.VStartUs, AStartUs: c.AStartUs, MuteOriginal: c.MuteOriginal, AudioGainDB: c.AudioGainDB, NativeIsDialogue: c.NativeIsDialogue, AvPtsOffsetMs: c.AvPtsOffsetMs})
	}
	width, height := composeDimensions(input.AspectRatio, input.Size)
	maxEdge := max(width, height)
	for i, dst := range destinations {
		render.Outputs = append(render.Outputs, videoframe.ManifestRenderSpec{Destination: dst, WithOverlays: input.Variants[i] == "packaged", MaxEdge: maxEdge, Width: width, Height: height})
	}
	return renderer.ComposeManifest(ctx, render)
}

// dramaAspectBase 将项目画幅映射为 720p 基础像素（宽, 高）；1080p 等比例放大 1.5 倍且保持偶数像素。
var dramaAspectBase = map[string][2]int{
	"9:16": {720, 1280},
	"4:3":  {960, 720},
	"3:4":  {720, 960},
	"1:1":  {720, 720},
	"21:9": {1680, 720},
}

// composeDimensions 按项目画幅与导出档位映射为固定像素，不沿用输入素材分辨率。
func composeDimensions(aspectRatio, size string) (int, int) {
	base, ok := dramaAspectBase[aspectRatio]
	if !ok {
		base = [2]int{1280, 720}
	}
	if size == "1080p" {
		return base[0] * 3 / 2, base[1] * 3 / 2
	}
	return base[0], base[1]
}

// registerComposeExports 在同一业务事务登记全部变体；主成片优先指向 clean。
func (s *DramaService) registerComposeExports(ctx context.Context, tx repository.DramaTx, job *model.DramaJob, assets []*model.Asset, input *dramaComposeInput, report *model.DramaComposeExportReport) (*model.DramaComposeResult, *model.Asset, error) {
	if len(assets) == 0 {
		return nil, nil, errcode.ErrInternal
	}
	result := &model.DramaComposeResult{SkippedSeqs: input.SkippedSeqs}
	primary := assets[0]
	for i, asset := range assets {
		if asset.UserID != job.UserID {
			return nil, nil, repository.ErrNotFound
		}
		if err := tx.CreateAsset(ctx, asset); err != nil {
			return nil, nil, err
		}
		view, err := s.assets.ViewOf(ctx, asset)
		if err != nil {
			return nil, nil, err
		}
		if i == 0 {
			result.Asset = *view
		}
		if input.Manifest == nil && input.ManifestV2 == nil {
			continue
		}
		variant := input.Variants[i]
		if variant == "clean" {
			primary = asset
			result.Asset = *view
		}
		width, height := composeDimensions(input.AspectRatio, input.Size)
		result.Exports = append(result.Exports, model.DramaComposeExport{DramaComposeExportReport: report, Sha256: asset.Sha256, Variant: variant, Size: input.Size, Width: width, Height: height, ManifestHash: input.ManifestHash, Asset: *view})
	}
	return result, primary, nil
}

// recheckComposeV2Hashes 只读素材元数据；真实下载字节由 worker 独立验签。
func recheckComposeV2Hashes(ctx context.Context, tx repository.DramaTx, user uint64, input *dramaComposeInput) error {
	media, err := frozenComposeV2Media(input.ManifestV2)
	if err != nil {
		return err
	}
	if err := lockComposeV2Assets(ctx, tx, user, media); err != nil {
		return err
	}
	for _, frozen := range media {
		asset, err := tx.ComposeAsset(ctx, user, frozen.ID)
		if err != nil || !validProgramAsset(asset, user, frozen.ID, frozen.Kind) ||
			!validDramaManifestHash(frozen.Sha256) || asset.Sha256 != frozen.Sha256 {
			return errcode.ErrDramaManifestStale
		}
	}
	hash, err := jsonhash.CanonicalSHA256(input.ManifestV2)
	if err != nil || hash != input.ManifestHash {
		return errcode.ErrDramaManifestStale
	}
	return nil
}

// frozenComposeV2Media 提取所有需要按内容指纹校验的画面和音频。
func frozenComposeV2Media(manifest *model.DramaEpisodeManifestV2) ([]model.Asset, error) {
	media := make([]model.Asset, 0, len(manifest.Clips)+len(manifest.Program.Audio))
	for _, clip := range manifest.Clips {
		switch clip.VisualKind {
		case model.DramaVisualVideo:
			media = append(media, model.Asset{ID: clip.AssetID, Kind: model.KindVideo, Sha256: clip.Sha256})
		case model.DramaVisualStill:
			if clip.StillAssetID == nil {
				return nil, errcode.ErrDramaManifestStale
			}
			media = append(media, model.Asset{ID: *clip.StillAssetID, Kind: model.KindImage, Sha256: clip.Sha256})
		case model.DramaVisualBlack:
		default:
			return nil, errcode.ErrDramaManifestStale
		}
	}
	for _, cue := range manifest.Program.Audio {
		media = append(media, model.Asset{ID: cue.AssetID, Kind: model.KindAudio, Sha256: cue.Sha256})
	}
	return media, nil
}

// lockComposeV2Assets 延续项目 → 集 → 分镜 → 素材锁序，直到入队提交才释放指纹保护。
func lockComposeV2Assets(ctx context.Context, tx repository.DramaTx, user uint64, media []model.Asset) error {
	locker, ok := tx.(dramaProgramAssetLocker)
	if !ok {
		return nil // 内存仓储没有独立素材事务。
	}
	ids := make([]uint64, 0, len(media))
	for _, asset := range media {
		ids = append(ids, asset.ID)
	}
	slices.Sort(ids)
	return locker.LockProgramAssets(ctx, user, slices.Compact(ids))
}

// downloadProgramMedia 在锁外读取被冻结的素材，并用实际下载字节抵御对象同址替换。
func (s *DramaService) downloadProgramMedia(ctx context.Context, user uint64, path string, asset model.Asset) error {
	if s.assets == nil {
		return errcode.ErrDramaMediaUnavailable
	}
	file, err := s.assets.Open(ctx, user, asset.ID)
	if err != nil || file == nil || file.Body == nil {
		return errcode.ErrDramaMediaUnavailable
	}
	defer file.Body.Close()
	if !validProgramAsset(file.Asset, user, asset.ID, asset.Kind) {
		return errcode.ErrDramaMediaUnavailable
	}
	if err := copyComposeMedia(ctx, file.Body, path, asset.Sha256); err != nil {
		return errcode.ErrDramaMediaUnavailable
	}
	return nil
}

// copyComposeMedia 限制下载大小，并直接对写入临时文件的字节计算指纹。
func copyComposeMedia(ctx context.Context, body io.Reader, path, expected string) error {
	dst, err := os.Create(path) //nolint:gosec // 目标由私有目录和固定素材编号生成。
	if err != nil {
		return err
	}
	var out io.Writer = dst
	hash := sha256.New()
	if expected != "" {
		out = io.MultiWriter(dst, hash)
	}
	n, copyErr := io.Copy(out, io.LimitReader(&assetSrcReader{ctx: ctx, r: body}, defaultMaxResult+1))
	closeErr := dst.Close()
	if copyErr != nil {
		return copyErr
	}
	if n <= 0 || n > defaultMaxResult {
		return errors.New("采用素材为空或超过大小限制")
	}
	if expected != "" && !strings.EqualFold(expected, hex.EncodeToString(hash.Sum(nil))) {
		return errors.New("下载素材与冻结指纹不符")
	}
	return closeErr
}

// DramaProgramComposer 是 Stage C 渲染边界，返回对成片音轨的验收报告。
type DramaProgramComposer interface {
	// ComposeProgramManifest 渲染冻结节目单并验证共享音频母带。
	ComposeProgramManifest(context.Context, videoframe.ComposeProgramInput) (videoframe.ProgramComposeReport, error)
}

// RenderProgramManifest 渲染已冻结的 v2 清单，供预览与成片 worker 共用。
func (s *DramaService) RenderProgramManifest(ctx context.Context, user uint64, manifest *model.DramaEpisodeManifestV2, dir string, outputs []videoframe.ComposeProgramOutputSpec, rf *videoframe.FrameRange) (model.DramaComposeExportReport, error) {
	// 1. 校验冻结版本，再在私有临时目录绑定素材。
	if manifest == nil || manifest.Version != model.CurrentDramaManifestV2SchemaVersion ||
		manifest.RenderVersion != model.CurrentDramaManifestV2RenderVersion ||
		manifest.FPS != model.DramaProgramFPS || manifest.SampleRate != model.DramaProgramSampleRate {
		return model.DramaComposeExportReport{}, errcode.ErrDramaProgramInvalid.WithMsg("不支持的节目单渲染版本")
	}
	// 2. 将响度验收错误映射为可定位的业务错误；报告只记录实际封装的母带。
	out, err := s.renderProgramManifest(ctx, user, manifest, dir, videoframe.ComposeProgramInput{Outputs: outputs, RangeF: rf})
	switch {
	case errors.Is(err, videoframe.ErrLoudnessAcceptance):
		return model.DramaComposeExportReport{}, errcode.ErrDramaLoudnessInvalid
	case errors.Is(err, videoframe.ErrInvalidManifest):
		return model.DramaComposeExportReport{}, errcode.ErrDramaProgramInvalid
	case err != nil:
		return model.DramaComposeExportReport{}, err
	}
	return model.DramaComposeExportReport{AudioSHA256: out.AudioSHA256, LUFS: out.LUFS, TruePeak: out.TruePeak, LoudnessStatus: out.LoudnessStatus}, nil
}

// renderProgramManifest 只读冻结媒体，不回查现行节目单或重新探测素材。
func (s *DramaService) renderProgramManifest(ctx context.Context, user uint64, manifest *model.DramaEpisodeManifestV2, dir string, input videoframe.ComposeProgramInput) (videoframe.ProgramComposeReport, error) {
	renderer, ok := s.compose.(DramaProgramComposer)
	if !ok {
		return videoframe.ProgramComposeReport{}, errcode.ErrInternal
	}
	dir, err := os.MkdirTemp(dir, "vc-drama-program-sources-*")
	if err != nil {
		return videoframe.ProgramComposeReport{}, err
	}
	defer os.RemoveAll(dir)
	sources := make(map[uint64]model.Asset)
	load := func(id uint64, kind, hash string) (string, error) {
		return s.programRenderSource(ctx, user, dir, sources, model.Asset{ID: id, Kind: kind, Sha256: hash})
	}
	if err := bindProgramRenderSources(manifest, &input, load); err != nil {
		return videoframe.ProgramComposeReport{}, err
	}
	return renderer.ComposeProgramManifest(ctx, input)
}

// programRenderSource 每份冻结素材只下载一次，同时拒绝同一 ID 的不同指纹。
func (s *DramaService) programRenderSource(ctx context.Context, user uint64, dir string, sources map[uint64]model.Asset, asset model.Asset) (string, error) {
	if asset.ID == 0 || !validDramaManifestHash(asset.Sha256) {
		return "", errcode.ErrDramaMediaUnavailable
	}
	path := filepath.Join(dir, fmt.Sprintf("source-%d", asset.ID))
	if prev, ok := sources[asset.ID]; ok {
		if prev.Kind != asset.Kind || prev.Sha256 != asset.Sha256 {
			return "", errcode.ErrDramaMediaUnavailable
		}
		return path, nil
	}
	if err := s.downloadProgramMedia(ctx, user, path, asset); err != nil {
		return "", err
	}
	sources[asset.ID] = asset
	return path, nil
}

type programRenderSourceLoader func(id uint64, kind, hash string) (string, error)

// bindProgramRenderSources 将冻结坐标和经校验的本地路径传给渲染器。
func bindProgramRenderSources(manifest *model.DramaEpisodeManifestV2, input *videoframe.ComposeProgramInput, load programRenderSourceLoader) error {
	var cursor int32
	for _, c := range manifest.Clips {
		if c.MasterInF != cursor || c.MasterOutF <= cursor ||
			int64(c.MasterOutF)-int64(cursor) != int64(c.SourceOutF)-int64(c.SourceInF) {
			return errcode.ErrDramaProgramInvalid
		}
		cursor = c.MasterOutF
		clip, err := programRenderClip(c, load)
		if err != nil {
			field := "asset_id"
			if c.VisualKind == model.DramaVisualStill {
				field = "still_asset_id"
			}
			return programRenderMediaError(err, model.DramaProgramDiagnostic{SegmentID: c.SegmentID, Field: field})
		}
		input.Clips = append(input.Clips, clip)
	}
	if cursor != manifest.TotalFrames {
		return errcode.ErrDramaProgramInvalid
	}
	for _, cue := range manifest.Program.Audio {
		if cue.StartF == nil || cue.EndF == nil {
			return errcode.ErrDramaProgramInvalid
		}
		path, err := load(cue.AssetID, model.KindAudio, cue.Sha256)
		if err != nil {
			diagnostic := model.DramaProgramDiagnostic{CueID: cue.ID, Field: "asset_id"}
			if cue.Anchor != nil {
				diagnostic.SegmentID = cue.Anchor.SegmentID
			}
			return programRenderMediaError(err, diagnostic)
		}
		input.Audio = append(input.Audio, videoframe.ProgramAudioCueSpec{
			ID: cue.ID, Role: cue.Role, SourcePath: path, DecodedAudioSamples: cue.DecodedAudioSamples, StartF: *cue.StartF, EndF: *cue.EndF,
			SrcInSample: cue.SrcInSample, SrcOutSample: cue.SrcOutSample, GainDB: cue.GainDB, AvPtsOffsetMs: cue.AvPtsOffsetMs,
			FadeInF: cue.FadeInF, FadeOutF: cue.FadeOutF, Loop: cue.Loop, DuckDB: cue.DuckDB,
		})
	}
	return nil
}

// programRenderMediaError 为冻结素材下载失败补上已知条目身份，不公开源地址或本地路径。
func programRenderMediaError(err error, diagnostic model.DramaProgramDiagnostic) error {
	var business *errcode.Error
	if !errors.As(err, &business) || business.Code != errcode.ErrDramaMediaUnavailable.Code {
		return err
	}
	diagnostic.Reason, diagnostic.Severity = "media_unavailable", "error"
	return business.WithData(DramaProgramDiagnosticsData{Diagnostics: dramaProgramDiagnosticsView([]model.DramaProgramDiagnostic{diagnostic})})
}

// programRenderClip 保留视频 PTS 和原声参数，静帧与黑场不查视频素材。
func programRenderClip(c model.DramaManifestV2Clip, load programRenderSourceLoader) (videoframe.ProgramClip, error) {
	clip := videoframe.ProgramClip{
		ProgramVisualClip: videoframe.ProgramVisualClip{
			VisualKind: c.VisualKind, SourceInF: c.SourceInF, SourceOutF: c.SourceOutF, NormalizedFrames24: c.NormalizedFrames24, FX: c.FX,
		},
		HasAudio: c.HasAudio, DecodedAudioSamples: c.DecodedAudioSamples, VStartUs: c.VStartUs, AStartUs: c.AStartUs,
		MuteOriginal: c.MuteOriginal, AudioGainDB: c.AudioGainDB, NativeIsDialogue: c.NativeIsDialogue, AvPtsOffsetMs: c.AvPtsOffsetMs,
	}
	var err error
	switch c.VisualKind {
	case model.DramaVisualVideo:
		clip.SourcePath, err = load(c.AssetID, model.KindVideo, c.Sha256)
	case model.DramaVisualStill:
		if c.StillAssetID == nil {
			return videoframe.ProgramClip{}, errcode.ErrDramaProgramInvalid
		}
		clip.StillPath, err = load(*c.StillAssetID, model.KindImage, c.Sha256)
	}
	return clip, err
}

// readDramaV2Still 校验图片指纹；缓存路径把历史空指纹留给锁外预热。
func (s *DramaService) readDramaV2Still(ctx context.Context, user uint64, clip *model.DramaManifestV2Clip, warm bool) (bool, string) {
	if clip.StillAssetID == nil {
		return false, "visual_invalid"
	}
	id := *clip.StillAssetID
	asset, err := s.programAsset(ctx, user, id)
	if err != nil || !validProgramAsset(asset, user, id, model.KindImage) {
		return false, "media_unavailable"
	}
	if asset.Sha256 == "" && !warm {
		if _, ok := s.assets.(segmentMediaHasher); !ok {
			return false, "media_unavailable"
		}
		return true, ""
	}
	asset, err = s.ensureDramaV2AssetHash(ctx, asset)
	if err != nil {
		return false, "media_unavailable"
	}
	clip.Sha256 = asset.Sha256
	return false, ""
}

// ensureDramaV2AssetHash 只在事务外补齐历史媒体的空指纹。
func (s *DramaService) ensureDramaV2AssetHash(ctx context.Context, asset *model.Asset) (*model.Asset, error) {
	if validDramaManifestHash(asset.Sha256) {
		return asset, nil
	}
	hasher, ok := s.assets.(segmentMediaHasher)
	if !ok || asset.Sha256 != "" {
		return nil, errcode.ErrDramaMediaUnavailable
	}
	hash, updated, err := hasher.EnsureSha256(ctx, asset.UserID, asset.ID)
	if err != nil || !validProgramAsset(updated, asset.UserID, asset.ID, asset.Kind) ||
		updated.Sha256 != hash || !validDramaManifestHash(hash) {
		return nil, errcode.ErrDramaMediaUnavailable
	}
	return updated, nil
}

// readDramaV2VideoProbe 为旧视频补齐 V2 指纹，继续复用版本化视频探针。
func (s *DramaService) readDramaV2VideoProbe(ctx context.Context, user, id uint64, warm bool) (*model.Asset, *model.AssetMediaProbe, bool, error) {
	asset, media, hit, err := s.lookupCachedDramaManifestProbe(ctx, user, id)
	if errors.Is(err, errcode.ErrDramaTakeInvalid) {
		legacy, readErr := s.programAsset(ctx, user, id)
		_, canHash := s.assets.(segmentMediaHasher)
		if readErr == nil && canHash && validProgramAsset(legacy, user, id, model.KindVideo) && legacy.Sha256 == "" {
			if !warm {
				return legacy, nil, false, nil
			}
			_, err = s.ensureDramaV2AssetHash(ctx, legacy)
		}
	}
	if err == nil && !hit && warm {
		asset, media, err = s.dramaManifestProbe(ctx, user, id)
		hit = err == nil
	}
	return asset, media, hit, err
}

// dramaV2AudioState 保存本次解析的探针快照和预热所用的素材身份。
type dramaV2AudioState struct {
	media  map[uint64]dramaV2AudioMedia
	cold   []uint64
	assets []uint64
}

type dramaV2AudioMedia struct {
	probe  *model.AssetMediaProbe
	sha256 string
	failed bool
}

// readDramaV2Audio 按素材去重；缓存路径绝不下载或补算哈希。
func (s *DramaService) readDramaV2Audio(ctx context.Context, src *dramaManifestV2Source, warm bool) dramaV2AudioState {
	state := dramaV2AudioState{media: make(map[uint64]dramaV2AudioMedia)}
	user := src.project.UserID
	for _, cue := range src.program.Audio {
		id := cue.AssetID
		if _, seen := state.media[id]; seen {
			continue
		}
		asset, err := s.programAsset(ctx, user, id)
		if err != nil || !validProgramAsset(asset, user, id, model.KindAudio) {
			state.media[id] = dramaV2AudioMedia{failed: true}
			continue
		}
		state.assets = append(state.assets, id)
		hash := asset.Sha256
		probe, err := s.lookupCachedDramaManifestAudioProbe(ctx, asset)
		if probe == nil && err == nil && warm {
			hash, probe, err = s.dramaManifestAudioProbe(ctx, user, id)
		}
		_, canHash := s.assets.(segmentMediaHasher)
		if probe == nil && hash == "" && !canHash {
			err = errcode.ErrDramaMediaUnavailable
		}
		if probe == nil && err == nil {
			state.cold = append(state.cold, id)
		}
		state.media[id] = dramaV2AudioMedia{probe: probe, sha256: hash, failed: err != nil}
	}
	return state
}
