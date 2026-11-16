package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/response"
)

// CreatePreview 校验 JWT 身份与清单后创建 CPU 票据，并刷新仅 HttpOnly 的签名会话 Cookie。
func (h *DramaHandler) CreatePreview(c *gin.Context) {
	id, no, ok := dramaEpisodePath(c)
	if !ok {
		return
	}
	// 版本 0 合法，但字段不能缺失；用指针区分“未传”和显式 0。
	var req struct {
		model.DramaPreviewReq
		SegmentVersion *int64 `json:"segment_version" binding:"required,min=0"`
	}
	if !BindJSON(c, &req) {
		return
	}
	req.DramaPreviewReq.SegmentVersion = *req.SegmentVersion
	view, cookie, err := h.svc.CreateEpisodePreview(c.Request.Context(), currentUserID(c), id, no, &req.DramaPreviewReq)
	if err != nil {
		response.Fail(c, err)
		return
	}
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie("vc_preview_sess", cookie, 3600, "/api/v1/dramas", "", c.Request.TLS != nil, true)
	c.Header("Cache-Control", "no-store")
	response.OK(c, view)
}

// GetPreview 仅向当前 JWT 用户返回本人票据及短期签名代理 URL。
func (h *DramaHandler) GetPreview(c *gin.Context) {
	id, no, ok := dramaEpisodePath(c)
	if !ok {
		return
	}
	view, err := h.svc.GetEpisodePreview(c.Request.Context(), currentUserID(c), id, no, c.Param("ticket"))
	if err != nil {
		response.Fail(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	response.OK(c, view)
}

// StreamPreview 在公开路由强制校验 Cookie 与短期 token；拒绝时零正文，成功使用 Range 代理本地私有文件。
func (h *DramaHandler) StreamPreview(c *gin.Context) {
	id, no, ok := dramaEpisodePath(c)
	if !ok {
		return
	}
	cookie, _ := c.Cookie("vc_preview_sess")
	tokens := c.Request.URL.Query()["token"]
	if len(tokens) != 1 {
		c.Status(http.StatusUnauthorized)
		return
	}
	file, err := h.svc.OpenEpisodePreview(c.Request.Context(), id, no, c.Param("ticket"), cookie, tokens[0])
	if err != nil {
		status := http.StatusUnauthorized
		var business *errcode.Error
		if errors.As(err, &business) && business.Code == errcode.ErrForbidden.Code {
			status = http.StatusForbidden
		}
		c.Status(status)
		return
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	c.Header("Content-Type", "video/mp4")
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	http.ServeContent(c.Writer, c.Request, "preview.mp4", info.ModTime(), file)
}
