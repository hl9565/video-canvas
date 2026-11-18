package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/logger"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/provider"
	"video-canvas/internal/repository"

	"go.uber.org/zap"
)

// dramaComposeTimeout 同时限制下载、全部转码和对象写入，避免整集任务无限占用 worker。
const dramaComposeTimeout = 10 * time.Minute

// DramaVideoComposer 统一片段编码并按顺序拼接，由 videoframe.Extractor 实现。
type DramaVideoComposer interface {
	// Compose 将本地 sources 拼接到 destination，调用方负责最终成片文件清理。
	Compose(context.Context, []string, string) error
}

// DramaComposeAssets 复用素材存储，未提交对象和数据库登记分离以保证取消与重试安全。
type DramaComposeAssets interface {
	// Open 只打开当前用户素材，返回值的 Body 由调用方关闭。
	Open(context.Context, uint64, uint64) (*provider.AssetFile, error)
	// PrepareDramaVideo 上传本地产物但不插入素材行，事务提交后才能公开引用。
	PrepareDramaVideo(context.Context, uint64, string) (*model.Asset, error)
	// DiscardCopies 清理尚未提交的对象，复用画布导入的失败回收路径。
	DiscardCopies(context.Context, []*model.Asset)
	// View 读取当前用户素材的稳定视图。
	View(context.Context, uint64, uint64) (*model.AssetView, error)
	// ViewOf 为已经校验归属的素材生成稳定视图，不做网络请求。
	ViewOf(context.Context, *model.Asset) (*model.AssetView, error)
}

// dramaComposeInput 固定提交时的采用、跳过列表与片段版本，重试不能改用后来选中的 take。
type dramaComposeInput struct {
	EpisodeID      uint64                        `json:"episode_id"`
	SegmentVersion int64                         `json:"segment_version"`
	AssetIDs       []uint64                      `json:"asset_ids"`
	SkippedSeqs    []int                         `json:"skipped_seqs"`
	Manifest       *model.DramaEpisodeManifest   `json:"manifest,omitempty"`
	ManifestV2     *model.DramaEpisodeManifestV2 `json:"manifest_v2,omitempty"`
	RenderVersion  int                           `json:"render_version,omitempty"`
	ManifestHash   string                        `json:"manifest_hash,omitempty"`
	AspectRatio    string                        `json:"aspect_ratio,omitempty"`
	Size           string                        `json:"size,omitempty"`
	Variants       []string                      `json:"variants,omitempty"`
}

// composeChosenAsset 复用片段任务输出解析，成功任务也必须有已转存的视频素材。
func composeChosenAsset(tasks []model.GenerationTask, chosen uint64) (uint64, error) {
	for i := range tasks {
		if tasks[i].ID != chosen || tasks[i].Kind != model.KindVideo || tasks[i].Status != model.TaskSucceeded {
			continue
		}
		id, err := segmentOutputAsset(&tasks[i], model.KindVideo)
		if err != nil {
			return 0, err
		}
		if id > 0 {
			return id, nil
		}
	}
	return 0, errcode.ErrInternal.WithMsg("采用视频不可用，请重新采用片段")
}

// handleCompose 在锁外渲染并上传全部变体，任一失败或未提交时回收所有对象。
func (s *DramaService) handleCompose(ctx context.Context, run *dramaRun) error {
	workCtx, cancel := context.WithTimeout(ctx, dramaComposeTimeout)
	defer cancel()
	assets, input, report, err := s.renderCompose(workCtx, run.job)
	committed := false
	defer func() {
		if !committed && len(assets) > 0 {
			s.assets.DiscardCopies(context.WithoutCancel(ctx), assets)
		}
	}()
	if err != nil {
		return s.failDrama(ctx, run.job, err)
	}
	committed, err = s.commitCompose(ctx, run.job, assets, input, report)
	if committed {
		logger.RunOf(ctx).End(logger.StatusSucceeded, zap.Int("segments", len(input.AssetIDs)), zap.Ints("skipped_seqs", input.SkippedSeqs))
	}
	return err
}

// renderCompose 下载并渲染冻结输入，再暂存全部变体；上传失败仍返回已暂存对象。
func (s *DramaService) renderCompose(ctx context.Context, job *model.DramaJob) ([]*model.Asset, *dramaComposeInput, *model.DramaComposeExportReport, error) {
	if s.compose == nil || s.assets == nil {
		return nil, nil, nil, errcode.ErrInternal.WithMsg("转码失败：合成服务未配置")
	}
	var input dramaComposeInput
	if err := json.Unmarshal(job.InputJSON, &input); err != nil || !input.hasComposeMedia() {
		return nil, nil, nil, errcode.ErrInternal.WithMsg("转码失败：合成输入不可用")
	}
	dir, err := os.MkdirTemp("", "vc-drama-compose-*")
	if err != nil {
		return nil, nil, nil, withCause(errcode.ErrInternal.WithMsg("转码失败：无法创建临时目录"), err, "compose_transcode")
	}
	defer func() { _ = os.RemoveAll(dir) }()
	destinations, report, err := s.renderComposeFiles(ctx, job, &input, dir)
	if err != nil {
		var business *errcode.Error
		if errors.As(err, &business) {
			return nil, nil, nil, err
		}
		return nil, nil, nil, withCause(errcode.ErrInternal.WithMsg("转码失败：无法合成视频"), err, "compose_transcode")
	}
	assets, err := s.prepareComposeExports(ctx, job.UserID, destinations)
	return assets, &input, report, err
}

// hasComposeMedia 根据冻结版本判断输入；纯黑场和静帧没有视频素材 ID。
func (input *dramaComposeInput) hasComposeMedia() bool {
	if input.ManifestV2 != nil {
		return input.RenderVersion == model.CurrentDramaManifestV2RenderVersion && len(input.ManifestV2.Clips) != 0 && len(input.Variants) != 0
	}
	return len(input.AssetIDs) != 0
}

// renderComposeFiles 由冻结清单类型选择渲染器；不从当前集推断历史任务版本。
func (s *DramaService) renderComposeFiles(ctx context.Context, job *model.DramaJob, input *dramaComposeInput, dir string) ([]string, *model.DramaComposeExportReport, error) {
	if input.Manifest == nil && input.ManifestV2 == nil {
		dst := filepath.Join(dir, "final.mp4")
		return []string{dst}, nil, s.renderLegacyCompose(ctx, job, dir, dst, input)
	}
	destinations := make([]string, len(input.Variants))
	for i := range destinations {
		destinations[i] = filepath.Join(dir, fmt.Sprintf("final-%d.mp4", i))
	}
	if input.ManifestV2 == nil {
		return destinations, nil, s.renderFrozenManifest(ctx, job.UserID, input, dir, destinations, nil)
	}
	width, height := composeDimensions(input.AspectRatio, input.Size)
	outputs := make([]videoframe.ComposeProgramOutputSpec, 0, len(destinations))
	for i, dst := range destinations {
		outputs = append(outputs, videoframe.ComposeProgramOutputSpec{Destination: dst, Variant: input.Variants[i], Width: width, Height: height})
	}
	report, err := s.RenderProgramManifest(ctx, job.UserID, input.ManifestV2, dir, outputs, nil)
	return destinations, &report, err
}

// prepareComposeExports 暂存对象，调用方负责未提交批次的清理。
func (s *DramaService) prepareComposeExports(ctx context.Context, user uint64, destinations []string) ([]*model.Asset, error) {
	assets := make([]*model.Asset, 0, len(destinations))
	for _, dst := range destinations {
		asset, err := s.assets.PrepareDramaVideo(ctx, user, dst)
		if err != nil {
			return assets, withCause(errcode.ErrInternal.WithMsg("转码失败：成片存储失败"), err, "compose_store")
		}
		assets = append(assets, asset)
	}
	return assets, nil
}

// renderLegacyCompose 保留旧 edit_schema=0 的整段合成器与下载失败错误契约。
func (s *DramaService) renderLegacyCompose(ctx context.Context, job *model.DramaJob, dir, dst string, input *dramaComposeInput) error {
	paths := make([]string, 0, len(input.AssetIDs))
	for i, id := range input.AssetIDs {
		path := filepath.Join(dir, fmt.Sprintf("input-%d.mp4", i))
		if err := s.downloadCompose(ctx, job.UserID, id, path); err != nil {
			return withCause(errcode.ErrInternal.WithMsg("下载失败：无法读取采用视频"), fmt.Errorf("素材 %d：%w", id, err), "compose_download")
		}
		paths = append(paths, path)
		if err := s.dramaProgress(ctx, job, (i+1)*40/len(input.AssetIDs)); err != nil {
			return err
		}
	}
	if err := s.compose.Compose(ctx, paths, dst); err != nil {
		return withCause(errcode.ErrInternal.WithMsg("转码失败：无法合成视频"), err, "compose_transcode")
	}
	return s.dramaProgress(ctx, job, 80)
}

// downloadCompose 保留旧整段和 v1 探针的读取契约，这些调用没有冻结指纹。
func (s *DramaService) downloadCompose(ctx context.Context, userID, assetID uint64, path string) error {
	return s.downloadComposeVideo(ctx, userID, assetID, path, "")
}

// downloadComposeVideo 下载当前用户的视频，并验证冻结任务要求的字节指纹。
func (s *DramaService) downloadComposeVideo(ctx context.Context, userID, assetID uint64, path, expected string) error {
	file, err := s.assets.Open(ctx, userID, assetID)
	if err != nil {
		return err
	}
	if file == nil || file.Body == nil {
		return errors.New("采用视频不可读取")
	}
	defer func() { _ = file.Body.Close() }()
	if !validProgramAsset(file.Asset, userID, assetID, model.KindVideo) {
		return errors.New("采用素材不是当前用户的视频")
	}
	return copyComposeMedia(ctx, file.Body, path, expected)
}

// commitCompose 重新读取当前集版本，合成期间修改过片段时成片立即显示过期。
func (s *DramaService) commitCompose(ctx context.Context, job *model.DramaJob, assets []*model.Asset, input *dramaComposeInput, report *model.DramaComposeExportReport) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	committed := false
	var completed *model.DramaJob
	err := s.repo.WithTx(ctx, func(tx repository.DramaTx) error {
		var err error
		completed, committed, err = s.commitComposeTx(ctx, tx, job, assets, input, report)
		return err
	})
	if errors.Is(err, repository.ErrNotFound) {
		return false, nil // 项目或集已删除时不发布成片；调用方仍回收临时对象。
	}
	if err != nil {
		return false, err
	}
	if committed {
		s.publish(ctx, completed)
	}
	return committed, nil
}

// commitComposeTx 在项目锁内登记素材、写集成片并结束任务；任务已被取消时什么都不写。
func (s *DramaService) commitComposeTx(ctx context.Context, tx repository.DramaTx, job *model.DramaJob, assets []*model.Asset, input *dramaComposeInput, report *model.DramaComposeExportReport) (*model.DramaJob, bool, error) {
	if _, err := tx.Project(ctx, job.UserID, job.DramaID, true); err != nil {
		return nil, false, err
	}
	current, err := tx.Job(ctx, job.UserID, job.DramaID, job.ID, true)
	if err != nil || !dramaActive(current.Status) {
		return current, false, err
	}
	ep, err := tx.SegmentEpisode(ctx, job.UserID, input.EpisodeID)
	if err != nil {
		return nil, false, err
	}
	if ep.DramaID != job.DramaID {
		return nil, false, repository.ErrNotFound
	}
	result, primary, err := s.registerComposeExports(ctx, tx, job, assets, input, report)
	if err != nil {
		return nil, false, err
	}
	// 首次导出即使期间已有编辑仍可发布旧快照；较旧完成任务不能覆盖已发布的新版本。
	if ep.FinalAssetID == nil || input.SegmentVersion >= ep.FinalSegmentVersion {
		ep.FinalAssetID, ep.FinalSegmentVersion, ep.FinalComposeJobID = &primary.ID, input.SegmentVersion, &job.ID
		if err := tx.SaveEpisode(ctx, ep, ep.Revision); err != nil {
			return nil, false, err
		}
	}
	if err := s.finishTx(ctx, tx, current, "succeeded", 0, "", 0, result); err != nil {
		return nil, false, err
	}
	return current, true, nil
}
