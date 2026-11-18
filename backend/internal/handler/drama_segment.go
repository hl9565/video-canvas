package handler

import (
	"github.com/gin-gonic/gin"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/response"
	"video-canvas/internal/service"
)

// DramaSegmentHandler 提供片段编辑与生成任务接口。
type DramaSegmentHandler struct{ svc *service.DramaSegmentService }

// NewDramaSegmentHandler 创建片段HTTP入口。
func NewDramaSegmentHandler(svc *service.DramaSegmentService) *DramaSegmentHandler {
	return &DramaSegmentHandler{svc: svc}
}

// List 按seq返回用户分集的身份、版本和完整片段视图。
func (h *DramaSegmentHandler) List(c *gin.Context) {
	id, no, ok := dramaEpisodePath(c)
	if !ok {
		return
	}
	ep, items, err := h.svc.ListEpisode(c.Request.Context(), currentUserID(c), id, no)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"episode_id": idcodec.ID(ep.ID), "segment_version": ep.SegmentVersion, "items": items})
}

// Get 返回单段完整视图及最新首帧流程。
func (h *DramaSegmentHandler) Get(c *gin.Context) {
	sid, ok := dramaSegmentPath(c)
	if !ok {
		return
	}
	view, err := h.svc.Get(c.Request.Context(), currentUserID(c), sid)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// ImportTakes 将来源画布的成功视频补入片段候选。
func (h *DramaSegmentHandler) ImportTakes(c *gin.Context) {
	sid, ok := dramaSegmentPath(c)
	if !ok {
		return
	}
	var req model.ImportDramaTakesReq
	if !BindJSON(c, &req) {
		return
	}
	view, err := h.svc.ImportTakes(c.Request.Context(), currentUserID(c), sid, &req)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// PatchTakeQC 带版本更新候选人工质检结果。
func (h *DramaSegmentHandler) PatchTakeQC(c *gin.Context) {
	sid, ok := dramaSegmentPath(c)
	if !ok {
		return
	}
	tid, ok := idcodec.Decode(c.Param("tid"))
	if !ok {
		response.Fail(c, errcode.ErrInvalidParams.WithMsg("候选任务id格式错误"))
		return
	}
	var req model.PatchDramaTakeQCReq
	if !BindJSON(c, &req) {
		return
	}
	view, err := h.svc.PatchTakeQC(c.Request.Context(), currentUserID(c), sid, tid, &req)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, view)
}

// Create 在分集指定位置新增片段，返回201。
func (h *DramaSegmentHandler) Create(c *gin.Context) {
	id, no, ok := dramaEpisodePath(c)
	if !ok {
		return
	}
	var req model.CreateDramaSegmentReq
	if !BindJSON(c, &req) {
		return
	}
	v, err := h.svc.Create(c.Request.Context(), currentUserID(c), id, no, &req)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, v)
}

// Update 按revision部分更新片段，返回完整视图。
func (h *DramaSegmentHandler) Update(c *gin.Context) {
	sid, ok := dramaSegmentPath(c)
	if !ok {
		return
	}
	var req model.UpdateDramaSegmentReq
	if !BindJSON(c, &req) {
		return
	}
	v, err := h.svc.Update(c.Request.Context(), currentUserID(c), sid, &req)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, v)
}

// Adapt 把片段原稿转写成目标平台写法并保存，同步等待文本模型，成功返回最新片段。
func (h *DramaSegmentHandler) Adapt(c *gin.Context) {
	sid, ok := dramaSegmentPath(c)
	if !ok {
		return
	}
	var req model.AdaptDramaSegmentReq
	if !BindJSON(c, &req) {
		return
	}
	v, err := h.svc.Adapt(c.Request.Context(), currentUserID(c), sid, &req)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, v)
}

// Delete 软删片段，生成任务和素材保留，成功返回204。
func (h *DramaSegmentHandler) Delete(c *gin.Context) {
	sid, ok := dramaSegmentPath(c)
	if !ok {
		return
	}
	if err := h.svc.Delete(c.Request.Context(), currentUserID(c), sid); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// Order 按完整ID集合重排某集片段。
func (h *DramaSegmentHandler) Order(c *gin.Context) {
	id, no, ok := dramaEpisodePath(c)
	if !ok {
		return
	}
	var req model.OrderDramaSegmentsReq
	if !BindJSON(c, &req) {
		return
	}
	items, err := h.svc.Order(c.Request.Context(), currentUserID(c), id, no, &req)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"items": items})
}

// Generate 固定video种类提交任务并转发Idempotency-Key，任务逐项错误在service转换为标准业务响应。
func (h *DramaSegmentHandler) Generate(c *gin.Context) {
	sid, ok := dramaSegmentPath(c)
	if !ok {
		return
	}
	var req model.GenerateDramaSegmentReq
	if !BindJSON(c, &req) {
		return
	}
	task, err := h.svc.Generate(c.Request.Context(), currentUserID(c), sid, c.GetHeader("Idempotency-Key"), &req)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Accepted(c, gin.H{"task": task})
}

// Quote 返回绑定片段版本的报价，不创建任务或冻结积分。
func (h *DramaSegmentHandler) Quote(c *gin.Context) {
	sid, ok := dramaSegmentPath(c)
	if !ok {
		return
	}
	var req model.GenerateDramaSegmentReq
	if !BindJSON(c, &req) {
		return
	}
	quote, err := h.svc.Quote(c.Request.Context(), currentUserID(c), sid, &req)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, quote)
}

// dramaSegmentPath 校验并解码片段路径ID，非法编码统一为参数错误。
func dramaSegmentPath(c *gin.Context) (uint64, bool) {
	sid, ok := idcodec.Decode(c.Param("sid"))
	if !ok {
		response.Fail(c, errcode.ErrInvalidParams.WithMsg("片段id格式错误"))
	}
	return sid, ok
}
