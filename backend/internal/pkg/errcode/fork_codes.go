package errcode

import "net/http"

// Fork 专属业务错误码保留 90000–99999，避免与上游业务码冲突。
// 90xxx 用于通用/跨模块错误，91xxx 用于短剧，92xxx 用于营销；新增 Fork 错误码统一在此定义。
var (
	ErrIdempotencyConflict       = New(90001, "幂等键已用于其他请求", http.StatusConflict)
	ErrDramaManifestStale        = New(91001, "剧本分镜版本已变更，请刷新清单", http.StatusConflict)
	ErrDramaQuoteExpired         = New(91002, "生成报价已失效，请重新获取报价", http.StatusConflict)
	ErrDramaEditInvalid          = New(91003, "分镜剪辑参数不合法", http.StatusBadRequest)
	ErrDramaFrameRunNotFound     = New(91004, "首帧控镜流程不存在", http.StatusNotFound)
	ErrDramaFrameRunConflict     = New(91005, "首帧控镜状态已变更或存在进行中流程", http.StatusConflict)
	ErrDramaFrameRunInconsistent = New(91006, "首帧控镜任务归属或确认快照不一致", http.StatusConflict)
	ErrDramaTakeQCRejected       = New(91007, "该候选已被驳回，无法直接采用", http.StatusConflict)
	ErrDramaFrameInvalid         = New(91008, "首帧素材无效或尚未就绪", http.StatusBadRequest)
	ErrDramaProgramInvalid       = New(91009, "节目单参数或结构不合法", http.StatusBadRequest)
	ErrDramaAnchorInvalid        = New(91010, "节目锚点已失效", http.StatusConflict)
	ErrDramaVisualInvalid        = New(91011, "分镜视觉模式或特效参数不合法", http.StatusBadRequest)
	ErrDramaMediaUnavailable     = New(91012, "引用素材不可用或已变更", http.StatusConflict)
	ErrDramaLoudnessInvalid      = New(91013, "成片混音未达到响度或真峰交付标准", http.StatusUnprocessableEntity)
	ErrDramaAssetInUse           = New(91014, "素材正被剧集引用，无法执行此操作", http.StatusConflict)
)
