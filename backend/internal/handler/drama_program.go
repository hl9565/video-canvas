package handler

import (
	"github.com/gin-gonic/gin"

	"video-canvas/internal/pkg/response"
	"video-canvas/internal/service"
)

// GetProgram 读取该集可编辑的节目单及片段版本。
func (h *DramaHandler) GetProgram(c *gin.Context) {
	id, no, ok := dramaEpisodePath(c)
	if !ok {
		return
	}
	view, err := h.svc.GetProgram(c.Request.Context(), currentUserID(c), id, no)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// PutProgram 用集身份和片段版本整体替换节目单。
func (h *DramaHandler) PutProgram(c *gin.Context) {
	id, no, ok := dramaEpisodePath(c)
	if !ok {
		return
	}
	// 严格解码器的业务错误含可修复的节目单诊断，不能通过通用绑定器转换。
	req, err := service.DecodePutDramaProgramReq(c.Request.Body)
	if err != nil {
		response.Fail(c, service.DramaContractError(err))
		return
	}
	view, err := h.svc.PutProgram(c.Request.Context(), currentUserID(c), id, no, req)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// manifestV2 返回节目单清单；仅冷缓存且无已知错误时继续预热。
func (h *DramaHandler) manifestV2(c *gin.Context, id uint64, no int) {
	view, prep, err := h.svc.PrepareOrResolveEpisodeManifestV2(c.Request.Context(), currentUserID(c), id, no)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if prep != nil {
		c.Header("Cache-Control", "no-store")
		response.Accepted(c, prep)
		return
	}
	response.OK(c, view)
}
