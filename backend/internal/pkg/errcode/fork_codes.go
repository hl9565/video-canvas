package errcode

import "net/http"

// Fork 专属业务错误码保留 90000–99999，避免与上游业务码冲突。
// 90xxx 用于通用/跨模块错误，91xxx 用于短剧，92xxx 用于营销；新增 Fork 错误码统一在此定义。
var (
	ErrIdempotencyConflict = New(90001, "幂等键已用于其他请求", http.StatusConflict)
	ErrDramaManifestStale  = New(91001, "剧本分镜版本已变更，请刷新清单", http.StatusConflict)
	ErrDramaQuoteExpired   = New(91002, "生成报价已失效，请重新获取报价", http.StatusConflict)
	ErrDramaEditInvalid    = New(91003, "分镜剪辑参数不合法", http.StatusBadRequest)
)
