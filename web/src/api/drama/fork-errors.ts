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
  programInvalid: 91009,
  anchorInvalid: 91010,
  visualInvalid: 91011,
  mediaUnavailable: 91012,
  loudnessInvalid: 91013,
  assetInUse: 91014,
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
  [FORK_ERR.programInvalid]: "节目单参数或结构不合法",
  [FORK_ERR.anchorInvalid]: "节目锚点已失效",
  [FORK_ERR.visualInvalid]: "分镜视觉模式或特效参数不合法",
  [FORK_ERR.mediaUnavailable]: "引用素材不可用或已变更",
  [FORK_ERR.loudnessInvalid]: "成片混音未达到响度或真峰交付标准",
  [FORK_ERR.assetInUse]: "素材正被剧集引用，无法执行此操作",
} as const;
