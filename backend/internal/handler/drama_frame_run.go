package handler

import (
	"github.com/gin-gonic/gin"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/response"
	"video-canvas/internal/service"
)

// DramaFrameRunHandler 提供首帧控镜创建、确认和可恢复视频提交接口。
type DramaFrameRunHandler struct{ svc *service.DramaFrameRunService }

// NewDramaFrameRunHandler 创建首帧流程 HTTP 入口。
func NewDramaFrameRunHandler(svc *service.DramaFrameRunService) *DramaFrameRunHandler {
	return &DramaFrameRunHandler{svc: svc}
}

// Create 创建首帧流程返回 201，幂等重放返回 200。
func (h *DramaFrameRunHandler) Create(c *gin.Context) {
	sid, ok := dramaSegmentPath(c)
	if !ok {
		return
	}
	var req model.CreateFrameRunReq
	if !BindJSON(c, &req) {
		return
	}
	view, created, err := h.svc.Create(c.Request.Context(), currentUserID(c), sid, &req)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if created {
		response.Created(c, view)
		return
	}
	response.OK(c, view)
}

// Get 读取并机会式恢复用户的首帧流程。
func (h *DramaFrameRunHandler) Get(c *gin.Context) {
	sid, rid, ok := dramaFrameRunPath(c)
	if !ok {
		return
	}
	view, err := h.svc.Get(c.Request.Context(), currentUserID(c), sid, rid)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// SetFrame 带版本及可选画布票据替换首帧。
func (h *DramaFrameRunHandler) SetFrame(c *gin.Context) {
	sid, rid, ok := dramaFrameRunPath(c)
	if !ok {
		return
	}
	var req model.SetFrameRunFrameReq
	if !BindJSON(c, &req) {
		return
	}
	view, err := h.svc.SetFrame(c.Request.Context(), currentUserID(c), sid, rid, &req)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// Confirm 验证报价与首帧内容后冻结视频输入。
func (h *DramaFrameRunHandler) Confirm(c *gin.Context) {
	sid, rid, ok := dramaFrameRunPath(c)
	if !ok {
		return
	}
	var req model.ConfirmFrameRunReq
	if !BindJSON(c, &req) {
		return
	}
	view, err := h.svc.Confirm(c.Request.Context(), currentUserID(c), sid, rid, &req)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// Submit 幂等提交确认版本，返回流程与任务快照及 202。
func (h *DramaFrameRunHandler) Submit(c *gin.Context) {
	sid, rid, ok := dramaFrameRunPath(c)
	if !ok {
		return
	}
	var req model.SubmitFrameRunReq
	if !BindJSON(c, &req) {
		return
	}
	run, task, err := h.svc.Submit(c.Request.Context(), currentUserID(c), sid, rid, &req)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Accepted(c, gin.H{"run": run, "task": task})
}

// Cancel 先取消流程再尽力取消关联生成任务。
func (h *DramaFrameRunHandler) Cancel(c *gin.Context) {
	sid, rid, ok := dramaFrameRunPath(c)
	if !ok {
		return
	}
	var req model.CancelFrameRunReq
	if !BindJSON(c, &req) {
		return
	}
	view, err := h.svc.Cancel(c.Request.Context(), currentUserID(c), sid, rid, &req)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// dramaFrameRunPath 复用片段 ID 校验，并统一拒绝无效的流程 ID。
func dramaFrameRunPath(c *gin.Context) (uint64, uint64, bool) {
	sid, ok := dramaSegmentPath(c)
	if !ok {
		return 0, 0, false
	}
	rid, ok := idcodec.Decode(c.Param("rid"))
	if !ok {
		response.Fail(c, errcode.ErrInvalidParams.WithMsg("首帧流程id格式错误"))
	}
	return sid, rid, ok
}
