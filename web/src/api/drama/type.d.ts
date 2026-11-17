import type { AssetDto, BackendAssetDto } from "@/api/asset/type";
import type { TaskStatus, TaskView } from "@/api/generation-task/type";

/**
 * 短剧工作室接口类型（契约 docs/design/短剧工作室/接口契约.md）。
 * Backend* 是后端原样的 snake_case 响应，其余是前端 camelCase 结构。
 */

/** 短剧整体状态 */
export type DramaStatus = "analyzing" | "outlined" | "scripting" | "ready" | "failed";

/** 流水线步骤 */
export type DramaStep =
  | "analyze"
  /** 分集规划（pipeline 自动发起，一批单元出一批分集目录） */
  | "plan"
  | "outline"
  | "scripts"
  | "breakdown"
  | "asset_prompts"
  | "storyboard"
  /** 合成整集（契约 §14）：服务端 ffmpeg，不扣积分 */
  | "compose";

/** 任务所在的线：pipeline 是系统自动研读 / 规划，user 是用户发起的全部任务 */
export type DramaLane = "pipeline" | "user";

/** 自动规划阶段：running 研读 / 规划中，paused 已暂停或失败，done 规划到范围终点（老项目也是 done） */
export type DramaPhase = "running" | "paused" | "done";

/** 规划暂停的原因：用户暂停 / 积分不足 / 规划失败 / 模型上下文不足 / 自定义提示词导致输出不合格 */
export type PauseCode = "user" | "credits" | "failed" | "context" | "custom_prompt";

/** 原文切出来的单元种类：集 / 场次 / 章 / 段 */
export type UnitKind = "episode" | "scene" | "chapter" | "chunk";

/** 提示词的四个环节：剧本改编 / 资产拆解 / 资产图提示词 / 视频提示词 */
export type PromptStep = "adapt" | "assets" | "asset_prompts" | "video";

/** 任务状态 */
export type DramaJobStatus = "queued" | "running" | "succeeded" | "failed" | "canceled";

/** 一集剧本的状态 */
export type ScriptStatus = "empty" | "generating" | "ready" | "failed";

/** 一集分镜脚本的状态（契约 §9.1） */
export type StoryboardStatus = "empty" | "generating" | "ready" | "failed";

/** 成片比例，空串表示未定 */
export type DramaRatio = "9:16" | "16:9" | "21:9" | "3:4" | "4:3" | "1:1" | "";

/** 原文类型，研读前为空串 */
export type DramaSourceKind = "script" | "novel" | "";

/** 开拍设置可选的画幅：只有三档 */
export type StartRatio = "9:16" | "16:9" | "4:3";

/** 提示词来源：内置 / 我的提示词 / 后台技能 */
export type PromptSource = "builtin" | "mine" | "skill";

/** 一个环节的提示词选择（提交给后端的结构）：mine 带 id，skill 带技能 name */
export interface PromptChoice {
  /** 来源 */
  source: PromptSource;
  /** source = mine 时的「我的提示词」id */
  id?: string;
  /** source = skill 时的技能 name */
  skill?: string;
}

/** 四个环节的提示词选择；省略的环节等于内置 */
export type PromptChoices = Partial<Record<PromptStep, PromptChoice>>;

/** 已建剧的一个环节提示词设置（快照来源与展示名） */
export interface PromptSetting extends PromptChoice {
  /** 展示名，如「内置」「我的 · 古风资产」「技能 · 跨集连续性」 */
  label: string;
  /** true 表示快照来源（我的提示词 / 技能）已被删或停用：快照照常可用，界面标红提示 */
  missing?: boolean;
}

/** 已建剧的四个环节提示词设置 */
export type PromptSettings = Record<PromptStep, PromptSetting>;

export interface BackendDramaSummary {
  id: string;
  title: string;
  status: DramaStatus;
  /** 自动规划阶段；老后端没有，按 done 处理 */
  phase?: DramaPhase;
  episode_count: number;
  locked_count: number;
  cover_url: string;
  updated_at: string;
}

export interface BackendEpisodeSummary {
  /** 分集稳定实体 ID（老后端 / 旧夹具可能缺失） */
  id?: string;
  no: number;
  title: string;
  synopsis: string;
  script_status: ScriptStatus;
  locked: boolean;
  revision: number;
  updated_at: string;
  /** 二期字段（契约 §9.1）；后端还没带时按空处理 */
  storyboard_status?: StoryboardStatus;
  segment_count?: number;
  segment_seconds?: number;
  done_segment_count?: number;
  /** 三期字段（契约 §14）：成片与是否过期；后端还没带时按「没有成片」处理 */
  final?: BackendAssetDto | null;
  final_stale?: boolean;
  /** 开场钩子；规划前建的老集为空 */
  hook?: string;
  /** 结尾悬念；规划前建的老集为空 */
  cliffhanger?: string;
  /** 对应原文起始单元号，0 = 未关联单元 */
  unit_from?: number;
  /** 对应原文结束单元号，0 = 未关联单元 */
  unit_to?: number;
  /** 片段编辑版本号，每次改镜头 / 排序 / 增删 / 剪辑递增 */
  segment_version?: number;
  /** 剪辑协议版本（0 = 旧整段拼接，1 = PR1 冻结清单） */
  edit_schema?: number;
  /** 最近一次写入当前成片的 compose 任务 id */
  final_compose_job_id?: string | number | null;
}

export interface BackendEpisodeView extends BackendEpisodeSummary {
  script_md: string;
}

/** 合成导出档位 */
export type DramaComposeSize = "720p" | "1080p";

/** 合成导出变体（PR1 仅允许 clean / packaged，sample 只是 UI 显示别名） */
export type DramaComposeVariant = "clean" | "packaged";

export interface BackendComposeExport {
  sha256: string;
  variant: DramaComposeVariant;
  size: DramaComposeSize;
  width: number;
  height: number;
  manifest_hash: string;
  asset: BackendAssetDto;
}

export interface BackendJobView {
  id: string;
  step: DramaStep;
  /** analyze / outline 为空数组 */
  episodes: number[] | null;
  status: DramaJobStatus;
  progress: number;
  credits: number;
  error_code: number;
  error_message: string;
  created_at: string;
  finished_at: string | null;
  /** compose 成功时 `{asset, skipped_seqs, exports?}`；其他步骤为 null（契约 §14） */
  result?: {
    asset: BackendAssetDto;
    skipped_seqs: number[] | null;
    exports?: BackendComposeExport[] | null;
  } | null;
  /** 所在的线；老后端没有这个字段 */
  lane?: DramaLane;
  /** plan 任务对应的起始单元号 */
  batch_from?: number;
  /** plan 任务对应的结束单元号 */
  batch_to?: number;
}

/** 三类资产各自的图片模型 key，空串 = 不指定 */
export interface DramaImageModels {
  character: string;
  scene: string;
  prop: string;
}

export interface BackendDramaView {
  id: string;
  title: string;
  canvas_id: string;
  status: DramaStatus;
  source_kind: DramaSourceKind;
  genre: string;
  style_key: string;
  style_custom: string;
  ratio: DramaRatio;
  target_episodes: number;
  episode_seconds: number;
  text_model: string;
  video_model?: string;
  image_models?: Partial<DramaImageModels> | null;
  outline_md: string;
  characters_md: string;
  revision: number;
  episodes: BackendEpisodeSummary[] | null;
  active_jobs: BackendJobView[] | null;
  created_at: string;
  updated_at: string;
  /** 原文字数 */
  source_chars?: number;
  /** 原文单元总数 */
  units_total?: number;
  /** 单元种类 */
  unit_kind?: UnitKind;
  /** 自动规划阶段；老后端没有，按 done 处理 */
  phase?: DramaPhase;
  /** 已规划到第几个单元 */
  plan_cursor?: number;
  /** 规划范围终点（已归一为 units_total） */
  range_end?: number;
  /** 暂停原因文案 */
  pause_reason?: string;
  /** 暂停原因分类 */
  pause_code?: PauseCode | "";
  /** 四个环节当前的提示词设置 */
  prompt_settings?: PromptSettings;
}

export interface BackendDramaStyles {
  genres: { key: string; name: string }[] | null;
  styles: { key: string; name: string; prompt: string }[] | null;
}

/** 列表项 */
export interface DramaSummary {
  id: string;
  title: string;
  status: DramaStatus;
  /** 自动规划阶段：running / paused 时首页显示「规划中」「规划已暂停」 */
  phase: DramaPhase;
  episodeCount: number;
  /** 已拆解（锁定）的集数 */
  lockedCount: number;
  /** 一期可为空，前端用渐变占位 */
  coverUrl: string | null;
  updatedAt: string;
}

export interface EpisodeSummary {
  /** 分集稳定实体 ID（后端带回时填充） */
  id?: string;
  no: number;
  title: string;
  synopsis: string;
  scriptStatus: ScriptStatus;
  /** 已拆解资产，剧本只读 */
  locked: boolean;
  revision: number;
  updatedAt: string;
  /** 分镜脚本状态 */
  storyboardStatus: StoryboardStatus;
  /** 片段数 */
  segmentCount: number;
  /** 各片段时长之和（秒） */
  segmentSeconds: number;
  /** 有「采用」视频的片段数 */
  doneSegmentCount: number;
  /** 合成好的成片；没有为 null */
  final: AssetDto | null;
  /** 成片之后有片段改了采用 / 顺序 / 增删：提示「成片已过期，可重新合成」 */
  finalStale: boolean;
  /** 开场钩子，没有为空串 */
  hook: string;
  /** 结尾悬念，没有为空串 */
  cliffhanger: string;
  /** 对应原文起始单元号，null = 未关联单元 */
  unitFrom: number | null;
  /** 对应原文结束单元号，null = 未关联单元 */
  unitTo: number | null;
  /** 片段编辑版本号（可选，后端带回时填充） */
  segmentVersion?: number;
  /** 剪辑协议版本（0 / 1） */
  editSchema?: number;
  /** 写入当前成片的 compose 任务 id */
  finalComposeJobId?: string | null;
}

export interface Episode extends EpisodeSummary {
  scriptMd: string;
}

export interface DramaJob {
  id: string;
  step: DramaStep;
  /** 本次范围；analyze / outline 为空 */
  episodes: number[];
  status: DramaJobStatus;
  /** 0–100 */
  progress: number;
  /** 实际扣费，结束后才有值 */
  credits: number;
  errorCode: number;
  errorMessage: string;
  createdAt: string;
  finishedAt: string | null;
  /** compose 成功的结果；其他步骤为 null */
  result: DramaJobResult | null;
  /** 所在的线：pipeline 系统自动，user 用户发起 */
  lane: DramaLane;
  /** plan 任务对应的起始单元号；其他任务为 null */
  batchFrom: number | null;
  /** plan 任务对应的结束单元号；其他任务为 null */
  batchTo: number | null;
}

export interface DramaComposeExport {
  sha256: string;
  variant: DramaComposeVariant;
  size: DramaComposeSize;
  width: number;
  height: number;
  manifestHash: string;
  asset: AssetDto;
}

/** compose 任务的结果：成片素材、被跳过片段序号，以及按变体导出的资产列表 */
export interface DramaJobResult {
  asset: AssetDto;
  skippedSeqs: number[];
  exports?: DramaComposeExport[];
}

export interface Drama {
  id: string;
  title: string;
  /** 短剧专属画布 ID，用现有画布接口读写 */
  canvasId: string;
  status: DramaStatus;
  sourceKind: DramaSourceKind;
  genre: string;
  styleKey: string;
  styleCustom: string;
  ratio: DramaRatio;
  /** 0 = 自动 */
  targetEpisodes: number;
  episodeSeconds: number;
  /** 空串 = 跟随后台「文本」类别第一个启用模型 */
  textModel: string;
  /** 建剧时选的视频模型；空串 = 没指定 */
  videoModel: string;
  /** 建剧时选的人物 / 场景 / 物品图片模型；空串 = 没指定 */
  imageModels: DramaImageModels;
  outlineMd: string;
  charactersMd: string;
  revision: number;
  episodes: EpisodeSummary[];
  activeJobs: DramaJob[];
  createdAt: string;
  updatedAt: string;
  /** 原文字数，老数据为 0 */
  sourceChars: number;
  /** 原文单元总数，老数据为 0 */
  unitsTotal: number;
  /** 单元种类；老数据按「集」 */
  unitKind: UnitKind;
  /** 自动规划阶段；老项目为 done */
  phase: DramaPhase;
  /** 已规划到第几个单元 */
  planCursor: number;
  /** 规划范围终点；老数据为 0 */
  rangeEnd: number;
  /** 暂停原因文案，没有为空串 */
  pauseReason: string;
  /** 暂停原因分类，没有为空串 */
  pauseCode: PauseCode | "";
  /** 四个环节当前的提示词设置；老数据全是内置 */
  promptSettings: PromptSettings;
}

export interface DramaStyles {
  genres: { key: string; name: string }[];
  styles: { key: string; name: string; prompt: string }[];
}

/** 分页列表（沿用 OKPage） */
export interface DramaPage {
  items: DramaSummary[];
  total: number;
  page: number;
  pageSize: number;
}

/** 新建短剧的请求（契约 §2）：原文已在预检时上传，这里只带草稿 token 与开拍设置 */
export interface CreateDramaInput {
  /** 预检返回的草稿 id */
  sourceToken: string;
  /** 用户确认的原文类型 */
  sourceKind: "script" | "novel";
  /** 0 = 全部；否则规划到第 N 个单元 */
  rangeEnd: number;
  /** 空串用文件名 */
  title?: string;
  /** 成片画幅 */
  ratio: StartRatio;
  /** 题材 */
  genre?: string;
  /** 画风 key */
  styleKey: string;
  /** 自定义画风描述 */
  styleCustom?: string;
  /** 单集时长（秒） */
  episodeSeconds: number;
  /** 文本模型 key；空串 = 默认模型 */
  textModel?: string;
  /** 视频模型 key；省略 = 不指定 */
  videoModel?: string;
  /** 人物 / 场景 / 物品资产用的图片模型 key；省略的项 = 不指定 */
  imageModels?: { character?: string; scene?: string; prop?: string };
  /** 四个环节的提示词选择；省略的环节 = 内置 */
  prompts?: PromptChoices;
}

/** PATCH /dramas/:id 可改字段 */
export interface UpdateDramaInput {
  revision: number;
  title?: string;
  genre?: string;
  styleKey?: string;
  styleCustom?: string;
  ratio?: DramaRatio;
  textModel?: string;
  outlineMd?: string;
  charactersMd?: string;
  /** 重新选择各环节的提示词：保存时重新快照正文，只影响之后提交的任务 */
  prompts?: PromptChoices;
}

/** PATCH /dramas/:id/episodes/:no 可改字段 */
export interface UpdateEpisodeInput {
  revision: number;
  /** 期望当前集号对应的稳定分集 ID；不匹配时后端返回 409 */
  expectedEpisodeId?: string;
  title?: string;
  synopsis?: string;
  scriptMd?: string;
  /** 开场钩子 */
  hook?: string;
  /** 结尾悬念 */
  cliffhanger?: string;
}

/** compose 请求附加参数 */
export interface DramaComposeInput {
  segmentVersion?: number;
  manifestHash?: string;
  size?: DramaComposeSize;
  variants?: DramaComposeVariant[];
}

/** 提交 / 估价任务的请求 */
export interface DramaJobInput {
  step: DramaStep;
  episodes: number[];
  /** 仅老项目没定比例时由 breakdown 带上；新项目比例建剧时已定，后端忽略 */
  ratio?: DramaRatio;
  /** storyboard：所选视频模型的单段时长上限（秒），缺省 15，范围 4–60 */
  maxSeconds?: number;
  /** compose：冻结版本、清单哈希与导出档位 / 变体 */
  compose?: DramaComposeInput;
  /** compose：客户端幂等键（最长 96 字符） */
  composeRequestKey?: string;
}

/** WebSocket drama.episode 事件载荷：该集剧本状态已保存，需重新读取短剧详情。 */
export interface DramaEpisodeEvent {
  /** 短剧 ID。 */
  drama_id: string;
  /** 已保存状态的集号，从 1 开始。 */
  episode_no: number;
}

/** WebSocket drama.job 事件载荷 */
export interface DramaJobEvent {
  dramaId: string;
  job: DramaJob;
}

/** 片段状态（由最近一条 take 推出） */
export type SegmentStatus = "idle" | "running" | "done" | "error";

export interface BackendSegmentTake {
  task_id: string;
  status: TaskStatus;
  asset: BackendAssetDto | null;
  error_message: string;
  created_at: string;
  /** 候选来源；旧响应缺省为 native */
  origin?: TakeOrigin;
  /** 人工质检；旧响应缺省为 unreviewed */
  qc?: TakeQC;
  /** 所属首帧流程 */
  run_id?: string;
  /** 来源画布 */
  source_canvas_id?: string;
  /** 实际首帧素材 */
  frame_asset_id?: number | string;
}

/** 候选来源：首帧 I2V、画布导入、直接生成。 */
export type TakeOrigin = "i2v" | "canvas_import" | "native";
/** 人工质检结果。 */
export type TakeQC = "unreviewed" | "accepted" | "rejected";
/** 首帧来源。 */
export type FrameSource = "existing" | "generate" | "canvas";
/** 首帧确认、提交与绑定流程的持久状态。 */
export type FrameRunState =
  | "draft"
  | "frame_pending"
  | "frame_ready"
  | "confirmed"
  | "submitting"
  | "video_pending"
  | "bound"
  | "failed"
  | "cancelled";

/** 后端首帧控镜快照；不含报价令牌和内部任务幂等键。 */
export interface BackendSegmentFrameRun {
  /** 流程 ID */
  id: string;
  /** 片段 ID */
  segment_id: string;
  /** 客户端幂等键 */
  request_key: string;
  /** 首帧来源 */
  frame_source: FrameSource;
  /** 流程状态 */
  state: FrameRunState;
  /** 确认基线的片段版本 */
  segment_revision: number;
  /** 流程 CAS 版本 */
  revision: number;
  /** 确认快照版本，0 为未确认 */
  confirmed_version: number;
  /** 视频模型 key */
  model_key: string;
  /** 来源画布 */
  origin_canvas_id?: string;
  /** 首帧生成任务 */
  frame_task_id?: string;
  /** 实际送入模型的首帧素材 */
  frame_asset_id?: number | string;
  /** 视频生成任务 */
  video_task_id?: string;
  /** 当前无采用时自动采用 */
  adopt_if_empty: boolean;
  /** 实际首帧内容指纹 */
  frame_sha256?: string;
  /** 画布回传授权票据 */
  canvas_ticket?: string;
  /** 已冻结的生成输入 */
  confirmed_input?: Record<string, unknown>;
  /** 提交开始时间 */
  submitting_at?: string | null;
  /** 失败码 */
  error_code: string;
  /** 失败原因 */
  error_message: string;
  /** 创建时间 */
  created_at: string;
  /** 更新时间 */
  updated_at: string;
}

/** 前端首帧控镜快照。 */
export interface SegmentFrameRun {
  /** 流程 ID */
  id: string;
  /** 片段 ID */
  segmentId: string;
  /** 客户端幂等键 */
  requestKey: string;
  /** 首帧来源 */
  frameSource: FrameSource;
  /** 流程状态 */
  state: FrameRunState;
  /** 片段版本基线 */
  segmentRevision: number;
  /** 流程 CAS 版本 */
  revision: number;
  /** 确认版本，0 为未确认 */
  confirmedVersion: number;
  /** 视频模型 key */
  modelKey: string;
  /** 来源画布 */
  originCanvasId?: string;
  /** 首帧生成任务 */
  frameTaskId?: string;
  /** 实际 16:9 首帧素材 */
  frameAssetId?: string;
  /** 视频生成任务 */
  videoTaskId?: string;
  /** 无采用时自动采用 */
  adoptIfEmpty: boolean;
  /** 首帧 SHA-256 */
  frameSha256?: string;
  /** 画布回传票据 */
  canvasTicket?: string;
  /** 已确认的生成输入 */
  confirmedInput?: Record<string, unknown>;
  /** 提交开始时间 */
  submittingAt?: string | null;
  /** 失败码 */
  errorCode: string;
  /** 失败原因 */
  errorMessage: string;
  /** 创建时间 */
  createdAt: string;
  /** 更新时间 */
  updatedAt: string;
}

/** 创建流程；网络重试必须复用 requestKey 和同一请求体。 */
export interface CreateFrameRunInput {
  /** 幂等键，最长 96 字符 */
  requestKey: string;
  /** 片段版本 */
  segmentRevision: number;
  /** 首帧来源 */
  frameSource: FrameSource;
  /** 已有素材 */
  frameAssetId?: string;
  /** 来源画布 */
  originCanvasId?: string;
  /** 生成首帧图片的模型 */
  modelId?: string;
  /** 图片生成参数 */
  input?: Record<string, unknown>;
  /** 无采用时自动采用 */
  adoptIfEmpty?: boolean;
}

/** 设置首帧，服务端裁为 16:9 并作废旧确认。 */
export interface SetFrameRunFrameInput {
  /** 流程版本 */
  revision: number;
  /** 新素材 ID */
  frameAssetId: string;
  /** 来源画布 */
  originCanvasId?: string;
  /** 画布回传票据 */
  canvasTicket?: string;
}

/** 确认首帧及报价，冻结 I2V 输入。 */
export interface ConfirmFrameRunInput {
  /** 流程版本 */
  revision: number;
  /** 当前片段版本 */
  segmentRevision?: number;
  /** 当前实际首帧素材 */
  frameAssetId?: string;
  /** 首帧指纹 */
  frameSha256: string;
  /** 无采用时自动采用 */
  adoptIfEmpty?: boolean;
  /** 固定视频模型 */
  modelId?: string;
  /** I2V 输入 */
  input: Record<string, unknown>;
  /** 服务端报价令牌 */
  quoteToken: string;
}

/** 提交只接受已确认版本，禁止携带新输入。 */
export interface SubmitFrameRunInput {
  /** 流程版本 */
  revision: number;
  /** 确认版本 */
  confirmedVersion: number;
}

/** 取消流程的 CAS 基线。 */
export interface CancelFrameRunInput {
  /** 流程版本 */
  revision: number;
}

/** 视频提交结果。 */
export interface SubmitFrameRunResult {
  /** 更新后的流程 */
  run: SegmentFrameRun;
  /** 任务快照，交给 gateway 入库 */
  task: TaskView;
}

/** 从用户画布补入成功视频候选。 */
export interface ImportSegmentTakesInput {
  /** 片段版本 */
  revision: number;
  /** 来源画布 */
  sourceCanvasId: string;
  /** 单个任务 ID */
  taskId?: string;
  /** 多个任务 ID */
  taskIds?: string[];
  /** 导入并采用，仅限单条 */
  adopt?: boolean;
  /** 换镜时是否重置剪辑 */
  resetEditOnTaskChange?: boolean;
}

/** 候选质检与当前采用项处置。 */
export interface PatchTakeQCInput {
  /** 片段版本 */
  revision: number;
  /** 质检结果 */
  qc: TakeQC;
  /** 集级内容版本 */
  expectedSegmentVersion?: number;
  /** 显式替换采用 */
  replacementTaskId?: string;
  /** 显式清空采用 */
  clearChosen?: boolean;
  /** 换镜时是否重置剪辑 */
  resetEditOnTaskChange?: boolean;
}

export interface BackendSegmentEdit {
  source_in_f: number;
  source_out_f: number | null;
  mute_original: boolean;
  audio_gain_db: number;
  native_is_dialogue: boolean;
  av_pts_offset_ms: number;
}

/** 逐镜精裁与原声参数（24fps 整数帧） */
export interface SegmentEdit {
  sourceInF: number;
  /** null 表示裁到源视频片尾 EOF */
  sourceOutF: number | null;
  muteOriginal: boolean;
  /** 原声增益 dB，范围 -60..+12 */
  audioGainDb: number;
  nativeIsDialogue: boolean;
  /** 音画同步偏移 ms，范围 -10000..10000 */
  avPtsOffsetMs: number;
}

export type SegmentEditPatch = Partial<SegmentEdit>;

export interface BackendSegmentView {
  id: string;
  /** 所属分集稳定实体 ID（可选） */
  episode_id?: string;
  episode_no: number;
  seq: number;
  seconds: number;
  prompt: string;
  takes: BackendSegmentTake[] | null;
  chosen_task_id: string;
  /** 用户显式清空了采用 */
  chosen_cleared?: boolean;
  /** 活跃或最近一轮首帧流程 */
  frame_run?: BackendSegmentFrameRun | null;
  /** 规范化 24fps 源视频帧数；尚未探测或无视频时为 null */
  source_frame_count?: number | null;
  /** 采用视频是否含音轨 */
  has_audio?: boolean;
  /** 逐镜精裁与原声参数 */
  edit?: Partial<BackendSegmentEdit> | null;
  status: SegmentStatus;
  revision: number;
  /** 后端随段视图回传的集级内容版本号（可选） */
  segment_version?: number;
  updated_at: string;
  /** 按格式存的转写稿；老后端可能没有 */
  adaptations?: Partial<Record<AdaptFormat, BackendSegmentAdaptation>> | null;
}

/** 需要转写的提示词格式（Seedance 是原稿本身，不存转写稿） */
export type AdaptFormat = "h3" | "generic";

export interface BackendSegmentAdaptation {
  prompt: string;
  source_hash: string;
  /** 转写时对应的片段时长（秒）；存量旧数据未记录时省略 */
  seconds?: number;
  edited: boolean;
  updated_at: string;
}

/** 一份转写稿 */
export interface SegmentAdaptation {
  /** `@[名](节点id)` 写法的转写结果 */
  prompt: string;
  /** 转写时原稿全文的 SHA-256（小写 hex）；与当前原稿不同 = 已过期 */
  sourceHash: string;
  /** 转写时对应的片段时长（秒）；存量未知时长为 undefined */
  seconds?: number;
  /** 用户手改过；重新转写前要确认覆盖 */
  edited: boolean;
  updatedAt: string;
}

/** 片段的一次视频生成结果 */
export interface SegmentTake {
  taskId: string;
  status: TaskStatus;
  asset: AssetDto | null;
  errorMessage: string;
  createdAt: string;
  /** 来源，省略视为直接生成 */
  origin?: TakeOrigin;
  /** 质检，省略视为待质检 */
  qc?: TakeQC;
  /** 所属首帧流程 */
  runId?: string;
  /** 来源画布 */
  sourceCanvasId?: string;
  /** 实际首帧素材 */
  frameAssetId?: string;
}

export interface Segment {
  id: string;
  /** 所属分集稳定实体 ID（可选） */
  episodeId?: string;
  episodeNo: number;
  /** 从 0 开始，按它排序 */
  seq: number;
  seconds: number;
  /** `@[名](短剧画布节点 id)` 形式的提示词 */
  prompt: string;
  /** 按创建时间正序 */
  takes: SegmentTake[];
  /** 采用的那一条；空串 = 没有 */
  chosenTaskId: string;
  /** 用户显式清空了采用 */
  chosenCleared?: boolean;
  /** 活跃或最近一轮首帧流程 */
  frameRun?: SegmentFrameRun | null;
  /** 规范化 24fps 源视频总帧数（未探测 / 无视频为 null；旧测试夹具可省略） */
  sourceFrameCount?: number | null;
  /** 源视频是否含音轨；未知为 undefined，明确 false 时提示无原声 */
  hasAudio?: boolean;
  /** 逐镜精裁与原声参数（旧测试夹具可省略，读取请用 segmentEditOf） */
  edit?: SegmentEdit;
  status: SegmentStatus;
  revision: number;
  /** 后端回传的集级内容版本号（用于即时同步，避免仅依赖异步详情刷新） */
  segmentVersion?: number;
  updatedAt: string;
  /** 按格式存的转写稿（原稿始终是 prompt，Seedance 格式） */
  adaptations: Partial<Record<AdaptFormat, SegmentAdaptation>>;
}

/** PATCH /drama-segments/:sid 可改字段 */
export interface UpdateSegmentInput {
  revision: number;
  prompt?: string;
  seconds?: number;
  chosenTaskId?: string | null;
  /** 显式清空当前采用 */
  clearChosen?: boolean;
  /** 期望当前采用的任务 id（"" 或 null 表示尚未采用）；不匹配时后端返回 409 */
  expectedTaskId?: string | null;
  /** 切换采用任务时是否重置剪辑字段（默认 true，显式传 false 可保留当前剪辑） */
  resetEditOnTaskChange?: boolean;
  /** 逐镜精裁与原声局部更新；sourceOutF 显式 null 表示恢复片尾 EOF */
  edit?: SegmentEditPatch;
  /** 手改某个格式的转写稿 */
  adaptation?: { format: AdaptFormat; prompt: string };
}

/** POST /drama-segments/:sid/adapt 请求体 */
export interface AdaptSegmentInput {
  format: AdaptFormat;
  revision: number;
}

/** POST /dramas/:id/episodes/:no/segments 请求体 */
export interface AddSegmentInput {
  prompt?: string;
  seconds?: number;
  afterSeq?: number;
}

/** POST /drama-segments/:sid/quote 请求体 */
export interface SegmentQuoteInput {
  modelId: string;
  input: Record<string, unknown>;
}

/** 后端 TaskQuoteView 原始结构 */
export interface BackendTaskQuote {
  effective_input: Record<string, unknown>;
  raw_size: string;
  credits: number;
  price_fingerprint: string;
  quote_token: string;
  expires_at: string;
}

/** 报价结果（提交生成时原样传回 quoteToken） */
export interface TaskQuote {
  effectiveInput: Record<string, unknown>;
  rawSize: string;
  credits: number;
  priceFingerprint: string;
  quoteToken: string;
  expiresAt: string;
}

/** generate 后端 JSON 请求体 */
export interface BackendSegmentGenerateBody {
  model_id: string;
  input: Record<string, unknown>;
  quote_token?: string;
}

/** generate 请求入参：兼容 snake_case 与 camelCase（quoteToken） */
export type SegmentGenerateBody =
  | { model_id: string; input: Record<string, unknown>; quote_token?: string }
  | { modelId: string; input: Record<string, unknown>; quoteToken?: string };

export type SegmentGenerateResult = { task: TaskView };

export interface BackendDramaManifestClip {
  segment_id: number | string;
  task_id: number | string;
  asset_id: number | string;
  sha256: string;
  seq: number;
  source_in_f: number;
  source_out_f: number;
  master_in_f: number;
  master_out_f: number;
  normalized_frames_24: number;
  has_audio: boolean;
  decoded_audio_samples: number;
  v_start_us: number;
  a_start_us: number;
  mute_original: boolean;
  audio_gain_db: number;
  native_is_dialogue: boolean;
  av_pts_offset_ms: number;
}

export interface DramaManifestClip {
  segmentId: string;
  taskId: string;
  assetId: string;
  sha256: string;
  seq: number;
  sourceInF: number;
  sourceOutF: number;
  masterInF: number;
  masterOutF: number;
  normalizedFrames24: number;
  hasAudio: boolean;
  decodedAudioSamples: number;
  vStartUs: number;
  aStartUs: number;
  muteOriginal: boolean;
  audioGainDb: number;
  nativeIsDialogue: boolean;
  avPtsOffsetMs: number;
}

export interface DramaEpisodeProgram {
  v: number;
  audio: Array<{ id: string }>;
  overlays: Array<{ id: string }>;
}

export type DramaManifestStatus = "ready" | "preparing";

export interface BackendDramaManifestView {
  status?: DramaManifestStatus;
  manifest?: {
    v: number;
    fps: number;
    total_frames: number;
    clips: BackendDramaManifestClip[] | null;
    program?: DramaEpisodeProgram | null;
  } | null;
  manifest_hash?: string;
  segment_version: number;
  skipped_seqs: number[] | null;
  diagnostics: string[] | null;
}

export interface DramaEpisodeManifest {
  v: number;
  fps: number;
  totalFrames: number;
  clips: DramaManifestClip[];
  program: DramaEpisodeProgram;
}

export interface DramaManifestView {
  status?: DramaManifestStatus;
  manifest: DramaEpisodeManifest;
  manifestHash: string;
  segmentVersion: number;
  skippedSeqs: number[];
  diagnostics: string[];
}

export interface DramaPreviewRange {
  inF: number;
  outF: number;
}

export interface CreateEpisodePreviewInput {
  segmentVersion: number;
  manifestHash: string;
  variant: DramaComposeVariant;
  rangeF?: DramaPreviewRange;
}

export type EpisodePreviewStatus = "pending" | "ready" | "failed";

export interface BackendEpisodePreviewView {
  ticket: string;
  status: EpisodePreviewStatus;
  stream_url?: string;
  expires_at: number;
}

export interface EpisodePreviewView {
  ticket: string;
  status: EpisodePreviewStatus;
  streamUrl: string | null;
  expiresAt: number;
}

/** WebSocket drama.segment 事件载荷（原样 snake_case） */
export interface DramaSegmentEvent {
  drama_id: string;
  episode_no: number;
  segment: BackendSegmentView;
}

/** 预检里的一条处理记录 */
export interface SourceIssue {
  /** 种类：noise_removed / split_long / episode_too_long / empty / duplicate 等 */
  kind: string;
  /** 涉及的行数 / 单元数 */
  count: number;
  /** 给用户看的一句话 */
  message: string;
}

/** 预检切出来的一个单元（只有目录，没有正文） */
export interface SourceUnit {
  /** 单元号，从 1 起 */
  no: number;
  /** 标题 */
  title: string;
  /** 字数 */
  chars: number;
}

/** 自动部分（研读 + 规划）的预估 */
export interface SourceEstimate {
  /** 预计集数下限 */
  episodesMin: number;
  /** 预计集数上限 */
  episodesMax: number;
  /** 集数是否精确（剧本型输入） */
  episodesExact: boolean;
  /** 预计积分 */
  credits: number;
  /** 预计耗时（分钟） */
  minutes: number;
}

/** 预检结果：弹窗「开拍设置」的来源行与初始预估 */
export interface SourcePreflight {
  /** 草稿 id，24 小时有效 */
  sourceToken: string;
  /** 文件名；粘贴文本时由后端给默认名 */
  filename: string;
  /** 去首尾空白后的 Unicode 字符数 */
  chars: number;
  /** 识别结果；unknown 表示认不出，用户必须手选 */
  kindGuess: "script" | "novel" | "unknown";
  /** 一句话依据，界面直接显示 */
  kindSignals: string;
  /** 单元种类 */
  unitKind: UnitKind;
  /** 单元目录 */
  units: SourceUnit[];
  /** 单元总数 */
  unitsTotal: number;
  /** 处理记录 */
  issues: SourceIssue[];
  /** 范围 = 全部时的预估 */
  estimate: SourceEstimate;
}

/** 预检请求：file 与 text 二选一 */
export interface PreflightInput {
  /** 上传的文件 */
  file?: File;
  /** 粘贴的正文 */
  text?: string;
  /** 用户改了类型后带上，后端按新类型重新切分 */
  sourceKind?: "script" | "novel";
  /** 估价用的文本模型，空串 = 默认模型 */
  textModel?: string;
  /** 估价用的单集时长（秒） */
  episodeSeconds?: number;
}

/** 草稿的预估请求 */
export interface SourceEstimateInput {
  /** 用户选的类型 */
  sourceKind: "script" | "novel";
  /** 0 = 全部 */
  rangeEnd: number;
  /** 单集时长（秒） */
  episodeSeconds: number;
  /** 文本模型 key，空串 = 默认模型 */
  textModel: string;
}

/** 内置提示词的一页（剧本改编有「分集规划 / 分集剧本」两页，其他环节一页） */
export interface PromptPage {
  /** 页 key：plan / scripts / breakdown / asset_prompts / storyboard */
  key: string;
  /** 页签名 */
  label: string;
  /** 内置正文 */
  body: string;
}

/** 一个环节的内置提示词 */
export interface PromptTemplate {
  /** 环节 */
  step: PromptStep;
  /** 可替换的正文页 */
  pages: PromptPage[];
}

/** 「我的提示词」一项 */
export interface MyPrompt {
  /** id */
  id: string;
  /** 环节 */
  step: PromptStep;
  /** 名称，同环节内不重复 */
  name: string;
  /** 各页正文，键是该环节 pages[].key 的子集，缺的页用内置 */
  bodies: Record<string, string>;
  /** 乐观锁版本 */
  revision: number;
  /** 最近更新时间 */
  updatedAt: string;
}

/** 新建 / 修改「我的提示词」的内容 */
export interface MyPromptInput {
  /** 环节 */
  step: PromptStep;
  /** 名称，≤ 40 字 */
  name: string;
  /** 各页正文 */
  bodies: Record<string, string>;
  /** 修改时带上的乐观锁版本 */
  revision?: number;
}

/** 后台已启用且标了该环节的技能 */
export interface DramaSkill {
  /** 技能 name */
  name: string;
  /** 显示名 */
  title: string;
  /** 一句话说明 */
  description: string;
}

/** 一条原文单元（对应原文查看） */
export interface DramaUnit extends SourceUnit {
  /** 正文，仅读单个单元时有 */
  text?: string;
}

/** 估价结果：积分，以及 scripts 时选中集里上一集既没剧本也不在选择里的集号 */
export interface JobEstimate {
  /** 预计积分 */
  credits: number;
  /** 衔接缺口：这些集的上一集没有剧本 */
  gapEpisodes: number[];
}

/** 预检接口的后端原样响应（snake_case） */
export interface BackendSourcePreflight {
  /** 草稿 id */
  source_token: string;
  /** 文件名 */
  filename: string;
  /** 字数 */
  chars: number;
  /** 识别结果 */
  kind_guess: "script" | "novel" | "unknown";
  /** 一句话依据 */
  kind_signals: string;
  /** 单元种类 */
  unit_kind: UnitKind;
  /** 单元目录，可能为 null */
  units: SourceUnit[] | null;
  /** 单元总数 */
  units_total: number;
  /** 处理记录，可能为 null */
  issues: SourceIssue[] | null;
  /** 预估 */
  estimate: {
    episodes_min: number;
    episodes_max: number;
    episodes_exact: boolean;
    credits: number;
    minutes: number;
  };
}
