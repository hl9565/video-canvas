package handler

import (
	"github.com/gin-gonic/gin"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/response"
)

// bindDramaJobReq 在 HTTP 边界解码分集主键；空串保留为 0，由项目锁内的身份校验拒绝。
func bindDramaJobReq(c *gin.Context) (*model.DramaJobReq, bool) {
	var wire struct {
		model.DramaJobReq
		ExpectedEpisodeID *idcodec.ID `json:"expected_episode_id"`
		ConfirmClear      *bool       `json:"confirm_clear"`
	}
	if !BindJSON(c, &wire) {
		return nil, false
	}
	if wire.ConfirmClear != nil {
		if wire.Step != "storyboard" {
			response.Fail(c, errcode.ErrInvalidParams)
			return nil, false
		}
		wire.DramaJobReq.ConfirmClear = *wire.ConfirmClear
	}
	wire.DramaJobReq.ExpectedEpisodeID = (*uint64)(wire.ExpectedEpisodeID)
	return &wire.DramaJobReq, true
}

// Estimate 返回所选步骤的积分估算，前置和模型规则与实际提交一致。
func (h *DramaHandler) Estimate(c *gin.Context) {
	id, ok := CanvasPathID(c)
	if !ok {
		return
	}
	req, ok := bindDramaJobReq(c)
	if !ok {
		return
	}
	credits, err := h.svc.Estimate(c.Request.Context(), currentUserID(c), id, req)
	if err != nil {
		response.Fail(c, err)
		return
	}
	gaps, err := h.svc.ScriptGaps(c.Request.Context(), currentUserID(c), id, req)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"credits": credits, "gap_episodes": gaps})
}

// CreateJob 提交一个流水线步骤，返回 202 和持久任务状态。
func (h *DramaHandler) CreateJob(c *gin.Context) {
	id, ok := CanvasPathID(c)
	if !ok {
		return
	}
	req, ok := bindDramaJobReq(c)
	if !ok {
		return
	}
	v, err := h.svc.CreateJob(c.Request.Context(), currentUserID(c), id, req)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Accepted(c, v)
}

// ListJobs 返回项目任务，status=active 时只返回排队或进行中的任务。
func (h *DramaHandler) ListJobs(c *gin.Context) {
	id, ok := CanvasPathID(c)
	if !ok {
		return
	}
	var req struct {
		Status string `form:"status" binding:"omitempty,oneof=active" label:"任务状态"`
	}
	if !BindQuery(c, &req) {
		return
	}
	items, err := h.svc.ListJobs(c.Request.Context(), currentUserID(c), id, req.Status == "active")
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"items": items})
}

// CancelJob 取消当前用户项目的指定任务并返回最终状态。
func (h *DramaHandler) CancelJob(c *gin.Context) {
	id, ok := CanvasPathID(c)
	if !ok {
		return
	}
	jobID, ok := idcodec.Decode(c.Param("job_id"))
	if !ok {
		response.Fail(c, errcode.ErrInvalidParams.WithMsg("job_id 格式错误"))
		return
	}
	v, err := h.svc.CancelJob(c.Request.Context(), currentUserID(c), id, jobID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, v)
}
