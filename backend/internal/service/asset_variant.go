package service

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"video-canvas/internal/imageproc"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/logger"
	"video-canvas/internal/storage"
)

const (
	// variantPublicMaxAge 用于本地变体及不带签名的云处理地址。
	variantPublicMaxAge = 24 * time.Hour
	// variantFallbackMaxAge 是回退到原图的缓存上限，避免后续可用的变体被旧响应遮蔽。
	variantFallbackMaxAge = 60 * time.Second
)

// AssetVariantResolver 为素材变体（缩略图 / 封面）解析处理地址，由 ImageProcessorService 实现。
type AssetVariantResolver interface {
	// Resolve 返回素材变体的处理地址：存储有已发布的处理服务且支持该变体时 ok=true；
	// 没有处理服务或不支持时 ok=false，调用方按“回退”处理；生成地址失败返回 error，调用方也应回退。
	Resolve(ctx context.Context, storageID uint64, h *storage.Handle, key, variant string) (VariantTarget, bool, error)
}

var _ AssetVariantResolver = (*ImageProcessorService)(nil)

// SetVariantResolver 设置缩略图 / 封面的云处理服务解析器。
func (s *AssetService) SetVariantResolver(r AssetVariantResolver) { s.variants = r }

// SetLocalVariantResolver 设置本地存储的按需预览调度器。
func (s *AssetService) SetLocalVariantResolver(resolver *LocalVariantService) {
	s.localVariants = resolver
}

// VariantOptions 控制没有可用变体时是否允许回退到原图。
type VariantOptions struct {
	Strict bool
}

// ResolveVariant 保持旧客户端的缩略图回退行为，封面仍不得回退到原视频。
func (s *AssetService) ResolveVariant(ctx context.Context, key, variant string) (*FileTarget, error) {
	return s.ResolveVariantWithOptions(ctx, key, variant, VariantOptions{})
}

// ResolveVariantWithOptions 优先提供云或本地预览，仅兼容缩略图请求可回退原图。
func (s *AssetService) ResolveVariantWithOptions(ctx context.Context, key, variant string, opts VariantOptions) (*FileTarget, error) {
	// 1. 变体只有两种，取值不对按不存在处理
	if variant != imageproc.VariantThumb && variant != imageproc.VariantPoster {
		return nil, errcode.ErrAssetNotFound
	}

	// 2. 反查素材与所在存储
	a, h, err := s.lookupFile(ctx, key)
	if err != nil {
		return nil, err
	}

	// 3. 变体必须与素材种类对应：缩略图只给图片，封面只给视频，音频没有变体。
	//    先于解析器判断，避免对不可能有变体的素材白白去生成地址
	if (variant == imageproc.VariantThumb && a.Kind != "image") || (variant == imageproc.VariantPoster && a.Kind != "video") {
		return nil, errcode.ErrAssetNotFound
	}

	// 4. 保留云处理服务的跳转策略，未命中时才尝试本地预览。
	if target := s.cloudVariantTarget(ctx, a.StorageID, h, key, variant); target != nil {
		return target, nil
	}
	strict := opts.Strict || variant == imageproc.VariantPoster
	if local, ok := h.Storage.(*storage.LocalStorage); ok && s.localVariants != nil {
		file, err := s.localVariants.Resolve(ctx, local, key, variant)
		if err == nil {
			return &FileTarget{Local: file, MaxAge: variantPublicMaxAge, CachePublic: true}, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if strict && errors.Is(err, errcode.ErrVariantUnavailable) {
			return nil, errcode.ErrVariantUnavailable
		}
	}

	// 5. 封面和严格缩略图返回 404，避免画布误下载原始大文件；兼容缩略图仅短期私有缓存。
	if strict {
		return nil, errcode.ErrAssetNotFound
	}
	target, err := s.targetOf(ctx, h, key, a)
	if err != nil {
		return nil, err
	}
	target.MaxAge = min(target.MaxAge, variantFallbackMaxAge)
	target.CachePublic, target.CacheImmutable = false, false
	return target, nil
}

// cloudVariantTarget 仅在已有云处理服务命中时返回跳转，错误由下一候选处理。
func (s *AssetService) cloudVariantTarget(ctx context.Context, storageID uint64, handle *storage.Handle, key, variant string) *FileTarget {
	if s.variants == nil {
		return nil
	}
	target, ok, err := s.variants.Resolve(ctx, storageID, handle, key, variant)
	if err != nil {
		logger.Warn("生成素材云变体地址失败", zap.Uint64("storage_id", storageID), zap.String("variant", variant), zap.Error(err))
		return nil
	}
	if !ok {
		return nil
	}
	return &FileTarget{RedirectURL: target.URL, MaxAge: s.variantMaxAge(handle, target)}
}

// variantMaxAge 决定处理地址的跳转缓存时长：公开读地址永久稳定，可以缓存一天；
// 带签名的地址不能超过签名有效期的 1/4，保证浏览器拿到的地址一定还没过期。
func (s *AssetService) variantMaxAge(h *storage.Handle, t VariantTarget) time.Duration {
	if !t.Signed {
		return variantPublicMaxAge
	}
	ttl := s.signedTTL(h)
	if ttl <= 0 {
		ttl = time.Hour
	}
	return ttl / 4
}
