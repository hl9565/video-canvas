/**
 * Fork 专属业务错误码保留 90000–99999，与后端 errcode/fork_codes.go 同步。
 * 90xxx 为通用/跨模块，91xxx 为短剧，92xxx 为营销；避免占用上游 4xxxx / 67xxx。
 */
export const FORK_ERR = {
  idempotencyConflict: 90001,
  manifestStale: 91001,
  quoteExpired: 91002,
  editInvalid: 91003,
  frameRunNotFound: 91004,
  frameRunConflict: 91005,
  frameRunInconsistent: 91006,
  takeQcRejected: 91007,
  frameInvalid: 91008,
} as const;

/** Fork 错误的兜底文案；需要具体参数原因时，由 errors.ts 优先展示后端 Msg。 */
export const FORK_ERROR_FALLBACK = {
  [FORK_ERR.idempotencyConflict]: "幂等请求参数不一致，请刷新后重试",
  [FORK_ERR.manifestStale]: "剧本分镜版本已变更，请刷新清单",
  [FORK_ERR.quoteExpired]: "生成报价已失效，请重新获取报价",
  [FORK_ERR.editInvalid]: "分镜剪辑参数不合法",
  [FORK_ERR.frameRunNotFound]: "首帧控镜流程不存在",
  [FORK_ERR.frameRunConflict]: "首帧控镜状态已变更或存在进行中流程",
  [FORK_ERR.frameRunInconsistent]: "首帧控镜任务归属或确认快照不一致",
  [FORK_ERR.takeQcRejected]: "该候选已被驳回，无法直接采用",
  [FORK_ERR.frameInvalid]: "首帧素材无效或尚未就绪",
} as const;
