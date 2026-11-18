# Agent Note: Stage C（PR3）分集节目单、静态黑场转场、混音响度与原子导出

Status: implemented

## Problem

Stage A（`9001`）与 Stage B（`9002`）已落地逐镜整数帧精裁、原声设置、`ResolveEpisodeManifest` v1、CPU 预览鉴权、首帧控镜 I2V 及全消费点候选核验，但短剧工作台仍缺 Stage C（PR3）的集级声音时间轴（`program_json`）、静态帧/黑场视觉分支与帧内预设动画（`still` / `black` / `fx_json`）、人声并集统一闪避（−8 dB 包络）、两遍响度归一化（−16 LUFS / TP ≤ −1.5 dBTP）及原子导出闭环。同时在推进 Stage C 时必须避免与上游高频文件产生不必要合流冲突（不改写既有上游迁移/豁免、不修改全局资产契约与直传流程），并按当前产品决策暂缓内置硬字幕与角标渲染。

历史约束与基线关联：[锁序与保存点笔记](../../implemented/architecture/2026-11-16-drama-lock-hierarchy-and-savepoint-avoidance.md)、[精裁整数帧清单与预览守卫笔记](../../implemented/architecture/2026-11-16-timeline-manifest-trim-and-preview-guardrails.md)、[上游双段迁移与出片原子保存点笔记](../../implemented/architecture/2026-11-16-upstream-merge-dual-band-and-render-atomicity.md) 及 [Stage B 首帧控镜笔记](../../implemented/architecture/2026-11-16-stage-b-frame-runs-and-take-qc.md)。

## Decision

在不改写已发布 SQL 迁移与上游核心资产/直传/任务契约、不破坏 Stage A/B 已冻结 `v:1` 清单哈希与渲染器的前提下，按已确认取舍（**A1 + B0 + C1 + D2**）全量落地 Stage C（Slices 1–6）：

- **Fork 双号段迁移与零新增豁免**：
  - `backend/migrations/9003_drama_program.sql` 新增 `drama_episodes.program_json`（`ck_de_program`，保留空 `overlays: []` 扩展槽位）与 `drama_segments.visual_kind` / `still_asset_id` / `fx_json`（`ck_ds_visual_kind`、`ck_ds_fx_obj`、`ck_ds_visual`）。
  - 保持 `backend/internal/tests/migration/schema_test.go` 既有 4 条上游 `autoMigrateOnly` 索引豁免与 `GenerationTask` 原样不变，`9003` 本身零新增豁免；`TestDramaComposePG16_ConcurrentKeyAndSchemaStageBOnly` 保留并发测试并将 Stage C 列缺失断言固定在 pinned Stage B 迁移子集（`0001..0037 + 9001 + 9002`）。
- **暂缓内置硬字幕/角标渲染（B0）**：
  - 本阶段不在后端与前端实现烧录字幕、地点角标与中文字体渲染（零字库嵌入、零 `Dockerfile`/CI 字体改动）；DB `ck_de_program` 约束检查 `overlays` 为 JSON array 类型（`migration/drama_program_test.go` 验证 SQL 层允许数组类型扩展），应用层（`DramaProgram.Validate` / `PutDramaProgramReq.Validate`）对非空 `overlays` 返回 `91009` 明确拒绝；前端 `PUT /program` 固定发送 `overlays: []` 且片场节目面板提供 3 条可编辑音轨（`narration` 含只读原声对白指示、`bgm`/`room_tone`、`sfx`）。
- **Fork 专属错误码与严格 A1 分层**：
  - `backend/internal/pkg/errcode/fork_codes.go` 与 `web/src/api/drama/{fork-errors.ts,errors.ts}` 同步维护 `91009–91014`（其中 `91014` `ErrDramaAssetInUse` / `DRAMA_ERR.assetInUse` 已声明并纳入响应/API 映射单测，Stage C 暂未新增生产调用方）；Stage C `model` 文件（`drama_program.go`、`drama_program_wire.go`、`drama_manifest_v2.go`）不依赖任何 `internal/...` 包且零 `depguard` 旁路，仅返回纯领域校验错误（`DramaProgramError` / `DramaVisualError`）；`idcodec.ID` 线协议 DTO（`PutDramaProgramReq`、`DramaEpisodeProgramView`、`DramaManifestV2View`、`DramaProgramDiagnostic`、`DramaProgramDiagnosticsData`）与 `service.DramaContractError` 均收拢在 `service`（`drama_program_wire.go`、`drama_manifest_v2_wire.go`）及 `handler`（`bind.go`、`drama_program.go`）线协议边界。
- **v1/v2 清单隔离与局部数字线协议（D2）**：
  - 保留历史 `v:1` 清单（`DramaEpisodeManifest`）与 `ComposeManifest` 渲染器及历史哈希不变；Stage C 独立采用 `v:2` 清单（`DramaEpisodeManifestV2`、`render_version: 2`）及 `ComposeProgramManifest` 渲染管线。
  - Stage C 入站复用 `model.AssetID` / `model.OptionalAssetID` 接收十进制整数或十进制字符串（`1..MaxInt64`）并区分缺失 / `null` / 空串 / 非法值；出站 `still_asset_id`、`program.audio[].asset_id` 与 `v:2` 片段 `asset_id` 保持十进制 JSON 数字（`*uint64` / `uint64`），不改动上游 `AssetView` / `TaskOutput` / `AssetLibrary`；前端在 `web/src/utils/drama/asset-id.ts` 提供 `stageCAssetIdReviver` 与不依赖 `JSON.parse` `context.source`（兼容 Safari 16.4 等运行环境）的原始 JSON 扫描回落 `parseStageCJson`，覆盖 Axios Stage C HTTP 请求（片段读写/重排、`program`、`getEpisodeManifestV2`）与 WebSocket `drama.segment`，配合同修订 `mergeSegment` / `parseProgramAssetId` / `formatDramaAssetId` 在安全整数区间外保留原始十进制字符串精度。
  - `PUT /dramas/:id/episodes/:no/program` 使用 `service.DecodePutDramaProgramReq`（严格 `DisallowUnknownFields()` + EOF 校验），并保留 `UpdateDramaSegmentReq` 既有 `EqualFold` 兼容语义。
- **服务端私有锚点绑定与显式重绑（A1）与视觉互斥**：
  - `SaveEpisodeProgram` 仅更新 `program_json`、`revision`（`+ 1`）、`updated_at`（`NOW()`）、按需递增的 `segment_version` 与 `edit_schema`（不扩宽通用 `SaveEpisode`）；`PUT /program` 强制 `expected_episode_id` + `expected_segment_version` CAS 并在服务端私有结构保存 `bound_source = {task_id, asset_id, sha256}`。
  - 锚定源换片后若锚点坐标保持不变（`*old.Anchor == *cue.Anchor` 且未列入 `rebind_anchor_cue_ids`），`PUT /program` 保留原有 `bound_source` 以允许无关音频调整保存，`anchor_source_replaced` 诊断在 `Estimate`、`CreateJob`、`CreateEpisodePreview`（返回 `91010` + `DramaProgramDiagnosticsData`）与 `Manifest V2`（返回 `status: "ready"`、空 `manifest_hash` 与 `program_diagnostics`）暴露；仅当在已换源分镜上移动 `anchor.source_f`（或修改锚点坐标）而未指定 `rebind_anchor_cue_ids` 时，`PUT /program` 返回 `91010` 拒绝静默重绑。
  - `16:9` 门禁保护现有非 `16:9` Stage A 工程不受阻断；`video`/`still`/`black` 与 Stage B 活跃 `FrameRun` 及后台自动采用互斥，切至静帧/黑场时保留历史 `Takes` 与 `chosen_cleared`。
- **视音频渲染、母带复用与下载摘要兜底（C1）**：
  - `pkg/videoframe` 在 `24fps` 整数帧域内渲染 `video`/`still`/`black`、`2x2` `maskedmerge` 白闪（强度上限 `<= 0.78`）与区间不变量 `still_zoom_shake` / 淡入淡出。
  - 基于整集 `U_samples` 人声并集单一控制包络（`80ms` attack / `280ms` release，仅显式设置 `duck_db: -8` 的 `bgm`/`room_tone` 按 `−8 dB` 闪避，未设置 `duck_db` 时保持原电平）混合 48 kHz `f32le` PCM；先对整集主混音执行两遍 `loudnorm` 与真峰上限分级重试（`{-2.3, -3.5, -5.0}`）及整集 AAC 验收（`−16.0 ± 0.5 LUFS`，`TP ≤ −1.5 dBTP`），验收通过后再按完整或 `range_f` 选区编码 `.m4a` 并跨 `clean`/`packaged` 变体共享（静音短路输出 `lufs/true_peak = null, loudness_status = "silence"`）。视频探针缓存版本升至 `NormalizedProbeVersion = 3`（音频独立使用 `NormalizedAudioProbeVersion = 2`），探针写入点（`drama_segment_probe.go` `probeSegmentAsset`、`drama_manifest.go` `dramaManifestProbe` / `probeDramaManifestAudioAsset`）与全部冻结清单合成读取点（`drama_compose_render.go` 对冻结 `v:1`/`v:2` 合成及预览经 `copyComposeMedia` / `downloadComposeVideo`）均校验实际下载字节 SHA-256（旧 `edit_schema = 0` 整段 `renderLegacyCompose` 通路沿用 `drama_compose.go:186` 既有行为，不传预期哈希；不修改上游 `asset_direct.go`）。
- **原子提交、并发 `PUT` 与前端片场闭环**：
  - `GET /dramas/:id/episodes/:no/segments`（`handler/drama_segment.go` + `service.DramaSegmentService.ListEpisode`）在同一个 `WithSnapshotTx`（`REPEATABLE READ READ ONLY`）中读取权威 `episode_id`、`segment_version`、分镜成员及修订、候选任务与质检关系、生产素材元数据和首帧流程；仅在发现待恢复的采用或流程推进时，才用写事务的 `tx.ComposeAsset` 复核素材并恢复，再整体重读快照；恢复无需额外连接，允许 `MaxOpenConns(1)`。片段列表、写接口及 `drama.segment` 事件的每条视图均携带 `episode_id`。`Delete` 和 `Order` 在写事务内组装最终顺序、候选、采用关系、首帧流程和匹配素材指纹的已缓存规范帧数；投影失败时回滚，成功提交后直接返回或发布同一份视图。此路径不执行冷探测，也不发候选被清空的降级视图；Stage C 合成在 `tx1` 读取版本快照、锁外预热与冻结 `manifest_v2`、`tx2` 在项目→集→分镜→素材锁序内二次核验版本与资产指纹；`commitComposeTx` 原子发布多变体与导出报告，并发 `PUT /program` 不被旧合成覆盖且已完成快照标为 `final_stale = true`；`range_f` 预览 `CreateEpisodePreview` / `GetEpisodePreview` / `StreamPreview` 基于整集母带状态截取，并在冷探测发现清单失效或版本/画幅过期票据上返回 `91001` / HTTP `409`；后台失败票据（含 `91012` 缺媒体与 `91013` 响度失败等）经 `previewFailure` / `programRenderMediaError` 脱敏暴露 `error_code`、`error_message` 与已编码的公开 `program_diagnostics`，不泄露本地路径或内部原因。
  - 前端 `web/src/api/drama/index.ts`、`web/src/api/asset/index.ts`、`web/src/api/asset-library/index.ts`、`web/src/api/generation-task/index.ts`、`socket-client.ts` 与 `mergeSegment` 通过 `parseStageCJson` / `stageCAssetIdReviver` 与 `originalStageCAssetIds` 在不改动上游 `AssetView` / `TaskOutput` 协议结构的前提下无损保留超安全整数 `id` / `still_asset_id` / `asset_id` 精度（覆盖素材库挑选与 `programTtsAsset` 读取 WS/HTTP 音频产物）；`web/src/pages/drama/studio/` 提供节目音频面板（3 轨时间轴，`bgm` 与 `room_tone` 可同区间并行叠加、新增背景音默认 `duck_db: -8`、键盘/拖拽整数帧微调、`useProgram` 以同步 `savingScopeRef` 阻断在途 `PUT` 期间的 `sync` 并仅在 `useLayoutEffect` 提交真实 `serverVersion` 后记录 `syncedVersionRef`、以 `editVersion` 区分提交后真实并发修改使 clean `rebase` 后 `PUT` 成功时 `dirty` 归 `false`、记录 `rebasedSegmentVersion`，面板渲染与写入校验共用 `effectiveProgramTimeline`；节目单版本或任一实时分镜版本超过它时均停用旧的 rebase 时间轴、同版本瞬态失败后于重渲染或定时退避自动恢复、片段缓存显式启用集主键，拒绝之前一集的迟到 HTTP、WebSocket 和修改结果；完整列表受整集已知版本下限保护，且另记最近完整列表的成员版本。单条事件按片段 ID 和行修订合并：另一镜头的较新事件不会挡住已知镜头的延迟更新；低于完整列表版本的未知 ID 不能重建已删片段。并发 `rebase` 要求 `getEpisodeProgram` 与 `listEpisodeSegments` 的顶层 `episode_id` 与 `segment_version` 在含空列表场景下严格一致并在条目附带时校验条目级一致后才更新 store，clean merge 先落定合并后基线与 `savedDocument` 再发 `PUT` 使 `PUT` 失败时不滞留旧版本、同镜微调 `source_f` 保留显式锚点重绑勾选；保存时禁用面板勾选，PUT 成功只消耗该次提交的重绑 ID，仍指向同一镜头的其余待提交勾选独立于音频草稿的 `dirty` 状态保留）、独立 `SegmentVisualDrawer`、`batchTargets` 与片场批量提交在编辑器存盘、付费转写及提交前排除并复核非 `video`（`still`/`black`）分镜，按集持久化于 `useDramaProgramTtsStore` 且经 `web/src/utils/tasks/gateway.ts` 唯一入口接入的 `program-tts:${episode.id}`（切集不丢草稿、单次成功仅挂载一次、预览/合成零额外 TTS 调用）、`range_f` 预览（`runLifecycle` 对 `91001` 允许最多 2 次自动重拉清单重建、对 V1 `67010` 触发 1 次有界 `checkedV1Empty` 重探，而失败票据冷却期手动刷新在拉取清单新鲜度时仅对 V1 `67010` 执行 1 次有界 `onManifestStale` 重探以自动切至 `manifest?v=2`，并由 `failedSnapshotPatch` 透传失败码、`errorMessage` 与 `programDiagnostics`）与 `FinalArea`（按当前 `[drama.id, episodeId, no]` 与清单请求序号隔离迟到的合成 `91001`；只有仍属于当前集且请求序号有效的提交可以发起刷新并标记清单已更新，已启动的新请求及同号替补集不被旧请求写入状态。分镜与节目单重探另受当前分集版本约束。全画幅及 `schema=0` 均经 `applyProgramView` 与 `useCompose` 的 `expectedEpisodeId` 提交前复核拦截被服务端替换的分集 `episode_id` 并触发有界工作台刷新与 `stale_episode` 提示。HTTP handler 解码 `expected_episode_id`，`DramaJobReq` 仅保存解码后的 `*uint64`；null/省略兼容旧客户端，非法编码返回 `400 / 10001`，空串、旧身份或目标集号已消失返回 `409 / 91001`。后端合成在项目锁内确认集号和主键，再检查补位集的任务与资格。瞬态 `GET /program` 失败视作未知阻断状态而非按 0 条音频静默降级 V1，并在重试、`91001` 过期自动重探及手动刷新时重新读取 `getEpisodeProgram` 与 `reloadSegments` 切回 `manifest?v=2`，同时展示实测响度与诊断）。

**片段缓存与分镜再生成约束**：

- WebSocket 单条更新和 HTTP 整集快照共用候选合并规则。候选按 `taskId` 保留最高已知进度及已解析的素材、错误和 QC；片段状态跟随后端的候选创建顺序，已采用的视频不会盖过后续候选的进行或失败状态。更高行修订仍决定采用关系和显式 QC 修改。
- 片场拖拽调用 `reorderLocal`，只调整现有片段的 `seq`，不推进已知服务端版本或完整列表的成员版本。v3 的单条更新后仍可接纳晚到的 v2 新建事件，直到权威列表确认成员集合。
- 删除成功（HTTP `204`）或服务端返回片段不存在（`67009`）后，按集身份记录不可复用的片段 ID。单条事件忽略该 ID，完整列表过滤该 ID 后仍对账其余片段。客户端只看见 v5 时，删除实际发生在 v7；版本更高的 v6 也可能是删除前的快照。删除记录与缓存数组绑定，切换到替补集、丢弃项目缓存或重置数组时失效。
- 同一行修订合并候选后，如果保留了较新的首帧流程，按合并后候选的创建顺序重新判断片段状态。只保留仍属于当前 `video_pending` 流程、未列入候选的进行态；旧流程的 `running` 不能盖过已绑定候选的成功状态。
- 分镜估价、提交和 worker 替换都在项目锁内先检查节目单。已编排的音频或叠层返回 `67006` 并提示先清空节目单；已有候选和视觉编辑仍返回可确认的 `67013`。前端在弹出确认前锁定同一分集的当前 `segmentVersion` 与无节目单快照；点击确认后再次核对同版本与空节目单且每个弹窗至多提交一次，并改由单次 `POST /jobs`（携 `confirm_clear`、`expected_episode_id`、`expected_segment_version`）在服务端项目锁同一事务内先复核并原子软删全集片段再入队，仅在请求成功后于本地记录已删片段与版本下限。

## Alternatives considered

- **在 Stage C 同步交付 `Noto Sans CJK SC` 硬字幕/角标、全站改造 `AssetView` 加 `id_str` 并重构上游 `asset_direct.go`** — 当前 AI 文本/排版生成在复杂字体与容器打包下仍引入较高回归面与跨端维护成本，暂缓内置字幕（保留 `overlays` 空数组与 DB 约束以便后续无迁移扩展）以及剥离全站 `id_str` 和 `asset_direct.go` 重构可显著降低与上游合流冲突；下载阶段字节级 SHA-256 校验与安全整数外十进制保留已独立兜底正确性。
- **保存节目单时自动静默刷新全部合法坐标锚点（不要求 `rebind_anchor_cue_ids`）** — 当前镜已换同长新视频、后续仅微调旁白增益并保存时，静默刷新会掩盖动作节拍偏移风险，因此保留显式 `rebind_anchor_cue_ids` 核对名单。

- **排序和删除提交后再读取视图** — 可以缩短写锁持有时间，但后续读可能遇到新提交或连接池争用；若退回基础行，前端会将候选丢失误当作真实状态。完整视图在写事务内投影，后续没有补读依赖。
- **保存时只禁用界面上的重绑勾选** — 操作反馈清楚，但不能防止 hook 调用方在 PUT 期间排入另一个勾选。保存完成还需按本次实际提交的 ID 消耗意图。
- **用单一集版本判断所有片段变化** — 判断集中，但任务可以在行和集版本都不变时完成；单条事件也不足以确认整集成员，而删除 `204` 没有版本可供比较。候选进度、成员版本和确认删除分别处理。
- **把节目单后置到清空后提交时判定（由单事务核验替代前端逐段 DELETE）** — 节目单独立于被删片段，且拆成逐段 `DELETE` + 提交会留下并发保存节目单的竞争窗口。当前确认框先读节目单拦截明显冲突，真正确认清空时由单次 `POST /jobs` 在项目锁同一事务内先复核 `expected_episode_id`、`expected_segment_version`、节目单为空及任务互斥，再原子清空片段并入队。

## Consequences

- **收益**：短剧工作台具备完整的 Stage C 分集节目单时间轴、静帧/黑场与帧特效、人声统一闪避、两遍响度验收与双分档/双变体原子发布能力；黄金回归覆盖这些无字场景（`demo_trim_19f`、`vo_3` 跨三镜与 `-8±0.3 dB` 闪避、`S3` 拍桌 `f239 / 478,000` 与换源拦截、`S7` 静音/`S2` 原声、Stage B `T115 -> A133` 与 708 帧/`29.500s`/`1,416,000` 采样渲染）。
- **代价与约束**：`overlays` 硬字幕能力延后至后续阶段启用（DB 与协议已预留空数组字段）；上游 `asset_direct.go` 直传竞态仍需靠后续上游合并解决，当前由合成下载字节摘要校验阻断残次导出。排序和删除需要在持有写锁期间读取视图，缓存没有匹配帧数时仍由随后正常读路径按需探测。
- **缓存与再生成的边界**：每集在缓存有效期内为每个已删除片段保留一个 ID；最后一镜删除后即使没有剩余片段事件，仍可抵御在途响应。确认清空与分镜入队在同一事务内持有项目锁：服务端重验 `expected_episode_id`、`expected_segment_version`、节目单为空及任务互斥，再软删全集片段并将片段版本递增一次；若校验或入队失败则整体回滚，历史任务与素材保持不动。前端在收到成功回执后记录已删片段 ID 和版本下限，以拦截迟到的旧响应。

## Verification

- **后端代码规范、构建与全量竞态测试（宿主真实 PostgreSQL 16 `video_canvas_test` + Redis 7 + FFmpeg）**：
  - `SKIP_TESTS=1 bash scripts/check.sh backend`：`golangci-lint`（相对 `master` 增量）`0 issues.`，`go build ./...` 通过。
  - `TEST_DATABASE_DSN="host=127.0.0.1 port=5432 dbname=video_canvas_test sslmode=disable" TEST_REDIS_ADDR="127.0.0.1:6379" go test -race -count=1 ./...`：全部 51 个含测试的可执行 Go 包（含 `cmd/server`、`internal/pkg/idcodec`、`migration`、`arch`、`model`、`repository`、`service`、`handler`、`pkg/videoframe` 黄金回归及 `TestDramaProgramGoldenAtomicPG16` / `TestDramaSegmentPG16_RecoveryOrderAndDeleteWithOneConnection` / `TestDramaStoryboardConfirmedClearPG16` / `TestFrameRun*`）一次全量零失败通过（其余纯实现/工具包均 `[no test files]`）。
- **前端全量门禁**：
  - `REQUIRE_NGINX=1 bash scripts/check.sh web`：`oxfmt --check`（1,525 个文件）、`tsc -b --noEmit`、`oxlint`（0 errors / 75 warnings）、`bun test`（**4,966 pass / 0 fail**，421 个测试文件 / 18,360 次断言，含 `gateway-guard.test.ts`、实时 `nginx -t` 代理日志验证，及 R11–R16 候选到达顺序、同 `frameRun` 修订优先对账与活跃 `video_pending` 状态保持、分镜确认弹窗绑定原始版本与单事务原子清空、`still`/`black` 退出批量转写生成、异步预览失败诊断展示、同进程 spy 恢复回归）与 `vite build`（5,901 个模块，1.55s）全部通过。
- **架构笔记脚本校验**：`.agents/notes/` 通过 `verify-agent-note-tree.ts`、`verify-agent-note-format.ts`、`verify-archived-agent-notes.ts` 与 `check-note-anchors.ts` 校验，且 `git diff --check HEAD` 零空白错误。
