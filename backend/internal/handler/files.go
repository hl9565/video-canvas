package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/logger"
	"video-canvas/internal/service"
	"video-canvas/internal/storage"
)

// FileResolver 决定一个对象 key 怎么提供给浏览器，由 service.AssetService 实现。
type FileResolver interface {
	ResolveFile(ctx context.Context, key string) (*service.FileTarget, error)
	// ResolveVariantWithOptions 解析缩略图或封面，并按请求版本决定是否允许回退。
	ResolveVariantWithOptions(ctx context.Context, key, variant string, opts service.VariantOptions) (*service.FileTarget, error)
}

// NewFilesHandler 返回 /files/*filepath 的处理器（GET 与 HEAD 都挂它）。
//
// 画布 payload 里存的素材地址永远是这个稳定路径：本地存储直接由后端提供文件（支持 Range，视频可拖动），
// 对象存储在访问的这一刻现签名并 302 跳转，所以地址永不过期、换存储也不变。
// 不鉴权：<video> / <img> 要直接引用，靠 key 不可猜测保护，和原来的本地存储一致。
func NewFilesHandler(res FileResolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := strings.TrimPrefix(c.Param("filepath"), "/")
		// 带 v 参数是请求缩略图 / 封面：不带就是原文件，行为和以前完全一致
		var (
			target *service.FileTarget
			err    error
		)
		if variant := c.Query("v"); variant != "" {
			c.Writer = variantResponseWriter{c.Writer}
			target, err = res.ResolveVariantWithOptions(c.Request.Context(), key, variant, service.VariantOptions{Strict: c.Query("pv") == "2"})
		} else {
			target, err = res.ResolveFile(c.Request.Context(), key)
		}
		if err != nil {
			if c.Request.Context().Err() != nil {
				c.Abort()
				return
			}
			var e *errcode.Error
			if errors.As(err, &e) {
				c.AbortWithStatus(e.HTTPStatus())
				return
			}
			logger.Error("解析素材文件失败", zap.String("key", key), zap.Error(err))
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}

		cache := storage.CachePolicy{MaxAge: target.MaxAge, Public: target.CachePublic, Immutable: target.CacheImmutable}
		if target.Local != nil {
			storage.ServeLocalFile(c, target.Local, cache)
			return
		}

		// 跳转本身可以缓存一段时间（不超过签名有效期的 1/4，保证浏览器拿到的地址一定还没过期）；
		// no-referrer 避免把站点页面地址带给存储服务商
		h := c.Writer.Header()
		h.Set("Cache-Control", cache.HeaderValue())
		h.Set("Referrer-Policy", "no-referrer")
		c.Redirect(http.StatusFound, target.RedirectURL)
	}
}

type variantResponseWriter struct{ gin.ResponseWriter }

func (w variantResponseWriter) WriteHeader(status int) {
	if status >= http.StatusBadRequest {
		// ServeContent 会移除失败响应的缓存头，因此在发送状态时明确写回 no-store。
		w.Header().Set("Cache-Control", "no-store")
	}
	w.ResponseWriter.WriteHeader(status)
}
