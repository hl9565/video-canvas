package service

import (
	"context"
	"fmt"
	"path/filepath"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
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
		if err := s.downloadCompose(ctx, user, c.AssetID, path); err != nil {
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
func (s *DramaService) registerComposeExports(ctx context.Context, tx repository.DramaTx, job *model.DramaJob, assets []*model.Asset, input *dramaComposeInput) (*model.DramaComposeResult, *model.Asset, error) {
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
		if input.Manifest == nil {
			continue
		}
		variant := input.Variants[i]
		if variant == "clean" {
			primary = asset
			result.Asset = *view
		}
		width, height := composeDimensions(input.AspectRatio, input.Size)
		result.Exports = append(result.Exports, model.DramaComposeExport{Sha256: asset.Sha256, Variant: variant, Size: input.Size, Width: width, Height: height, ManifestHash: input.ManifestHash, Asset: *view})
	}
	return result, primary, nil
}
