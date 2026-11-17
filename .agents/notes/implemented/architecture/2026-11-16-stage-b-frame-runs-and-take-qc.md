# Agent Note: Stage B 首帧控镜（9002）、全消费点候选核验与画布/QC 契约

Status: implemented

## Problem

Stage B 需要在不修改上游 `0036`/`0037` 迁移编号、不引入 Stage C（PR3）音字画中画字段的前提下，为短剧分集提供首帧控镜 `I2V` 工作流、画布跨页取帧/视频候选导入与候选人工质检（QC）。如果只靠前端串行拼装步骤，图片异步生成、非 `16:9` 自动裁切、报价确认与提交竞态、生成首帧初始化崩溃窗口、进程重启丢任务以及画布外部视频与分集合成/重拆口各自查任务导致的候选漂移与已驳回候选误采用都无法在数据库层封口。

## Decision

- **迁移与隔离边界（`9002` + `91004–91008`）**：
  - Schema 演进严格落在 `backend/migrations/9002_drama_frame_runs.sql`，包含 `drama_segments.chosen_cleared`、`generation_tasks.frame_run_id` / `frame_run_confirm_rev`、`drama_segment_frame_runs`、`drama_segment_take_links` 及局部唯一索引 `uk_dfr_segment_active`、`uk_dfr_video`；`TestMigrationsMatchModels` 零新增 `autoMigrateOnly` 豁免。
  - Fork 业务码收口于 `backend/internal/pkg/errcode/fork_codes.go`（`91004`–`91008`），并在 `web/src/api/drama/fork-errors.ts` 与 `web/src/api/drama/errors.ts` 一比一同步。
  - **跨端 ID 线协议统一**：`AssetID`（`backend/internal/model/drama_frame_run.go`）在 `frame_asset_id` 上与 `/assets/:id`、`AssetView` 及 `modelcfg.ValidateInput` 对齐十进制主键（请求兼容十进制数字/字符串并拒绝十六进制编码串，视图序列化为 `*uint64`）；任务/片段/画布 ID 继续使用 `idcodec.ID`，画布导入统一以 `GenerationTaskView.task_ref`（`idcodec.ID`）匹配与提交。
- **持久状态机与后台自愈**：
  - `backend/internal/service/drama_frame_run.go`、`backend/internal/service/drama_segment_generate.go` 与 `backend/internal/service/drama_frame_crop.go` 实现 `draft` → `frame_pending` → `frame_ready` → `confirmed` → `submitting` → `video_pending` → `bound` / `failed` / `cancelled` 状态机：
    - `SetFrame` 对非 `16:9` 图片居中裁切后经 `SaveDerivedImage` 落为衍生 PNG 资产并作废旧确认输入与报价快照，同时保持 `confirmed_version` 单调不减以防内部视频幂等键与历史确认版本复用碰撞；画布来源流程校验同源画布与 HMAC `canvas_ticket`。
    - `Confirm` 强制锁定 `m-1c89decdf8fd`、`op="i2v"`、`aspect_ratio="16:9"`、`count=1`、`images=[frame_asset_id]` 与实时 `SHA-256`，校验 `quote_token` 并冻结 `confirmed_input_hash`。
    - `Submit` 以两阶段提交（`beginFrameSubmit` CAS 进入 `submitting` + 事务外 `GenerationTaskService.CreateWithOptions` 带 `DramaRunBind` + 回写 `video_pending`）避免长锁；`GenerationTaskService.cancel` 经 `FrameRunCancelGuard`（`DramaRepository.HasActiveFrameRunForTask`）禁止普通任务取消接口绕过活跃流程，必须由 `DramaFrameRunService.Cancel` 先提交 `cancelled` 再取消任务。
    - `backend/internal/service/drama_frame_recovery.go` 与 `15s` ticker `maintainDramaFrameRuns` 对 `generate` `draft` 崩溃窗口、`submitting`、`frame_pending` 与 `video_pending` 进行游标分页公平扫描恢复（`RecoverUnsettledDramaFrameRunsPage` + `AfterUpdatedAt`/`AfterID`），完整处理 `failed`/`canceled`/`expired` 终态并防止前缀慢任务饿死后续队列。
- **全消费点候选统一核验、剪辑边界与显式清空语义**：
  - `backend/internal/service/drama_takes.go` 的 `ResolveSegmentTakes` 作为单一事实源统一服务片段读取、分集列表、合成、预览以及分镜重置守卫（`DramaService.checkStoryboardTakes`，连同 `DramaRepository.HasSegmentTakes` 拒绝悬空 `take_links` 孤儿），要求归属、视频类型与有效产出全部合法且尊重 `origin`/`qc`；`ImportTakes` 与 `PatchTakeQC` 阻止导入失败/跨片段首帧任务、阻止未显式提供 `replacement_task_id` 或 `clear_chosen: true` 时驳回当前采用，并在 `reset_edit_on_task_change=false` 时校验替换视频帧数界限。
  - `chosen_cleared` 区分「尚未自动采用」与「用户显式清空」（`backend/internal/service/drama_segment_edit_patch.go`、`backend/internal/service/drama_segment_task.go`）。
- **Web 片场接入与 Canvas 跨页桥接**：
  - `web/src/pages/drama/studio/use-studio.ts` 经 `web/src/utils/tasks/gateway.ts` 收口所有首帧创建/提交/取消入口（受 `web/src/tests/utils/tasks/gateway-guard.test.ts` 静态守卫），结合 `use-nav.ts`、`preserveNewerFrameRun`（`web/src/utils/drama/segments.ts`）与 `runFloors`（`web/src/store/drama-segments.ts`）防止迟到刷新/列表/WS 快照回退流程状态。
  - `web/src/components/drama/frame-run-drawer.tsx`、`import-takes-dialog.tsx`、`reject-take-dialog.tsx` 与 `web/src/pages/canvas/chrome/drama-frame-bridge.tsx` 实现十进制首帧预览、确认输入标准化比对、报价过期后强制重新确认、画布票据刷新/透传、`BroadcastChannel` + `focus`/`visibilitychange` 同步及 `task_ref` 候选导入。

## Alternatives considered

- **继续使用顺序编号 `0037_*.sql` 或复用 `idcodec.ID` 传递 `frame_asset_id`** — `0037_*` 与上游编号冲突；而 `idcodec.ID` 会与 `/assets/:id`、`AssetView` 及 `modelcfg` 的十进制资产主键契约断裂，因此分别采用 Fork `9002_*` 号段与 `model.AssetID`。
- **由前端缓存报价后直接调用通用任务创建接口创建 I2V 任务** — 无法保证送入模型的图片与用户确认的裁切 `16:9` 资产 `SHA-256` 一致，也不能在刷新/断网/重启后幂等恢复单片段唯一活跃流程，因此改为数据库持久状态机加 `DramaRunBind`、`FrameRunCancelGuard` 与后台游标恢复。
- **仅在读片场时过滤候选，不统一分镜重置、合成与预览核验** — 会导致悬空或归属失效候选阻塞分镜或进入预览合成，因此五处消费点统一接入 `ResolveSegmentTakes`。

## Consequences

- **收益**：首帧流程从选图/生图/画布构图到裁切、报价冻结、两阶段提交、超时收敛、取消守卫和崩溃恢复具备完整 DB/HTTP/UI 闭环；所有 Stage B 新逻辑严格隔离在 `9002` 与 `91004–91008` 内，未修改上游迁移文件，仅在 `9002` 增量扩展所需表和列且未引入 PR3 字段。
- **代价与后续约束**：后续 Stage C（PR3）需继续沿用 `9003_*` 迁移号段，并遵守「凡创建或取消生成任务的接口一律经过 `web/src/utils/tasks/gateway.ts`」与「新增片段消费者必须走 `ResolveSegmentTakes`」的约束。

## Verification

- 后端代码检查与构建：`SKIP_TESTS=1 bash scripts/check.sh backend` 通过（`golangci-lint` 相对 `master` 增量含 `gofmt`/`goimports` `0 issues.`，后端构建通过）。
- 后端全量竞态与真实依赖测试：`TEST_DATABASE_DSN="host=127.0.0.1 port=5432 user=postgres dbname=video_canvas_test sslmode=disable" TEST_REDIS_ADDR="127.0.0.1:6379" go test -race -count=1 ./internal/tests/... ./cmd/server` 全部 50 个包通过（含 `arch`、`migration`、`repository`、`handler`、`service`、`drama` 及真实 PostgreSQL 16 + Redis 7 集成用例）。
- 前端全量门禁：`REQUIRE_NGINX=1 bash scripts/check.sh web` 通过（`oxfmt --check` 1506 个文件、`tsc`、`oxlint`、`bun test` `4809 pass / 0 fail` 共 413 个文件含真实 `nginx -t` 校验、`vite build` 5890 modules in `1.17s`）。
- Agent 全量门禁：`bash scripts/check.sh agent` 通过（`oxfmt --check` 88 个文件、`oxlint`、`tsc -p tsconfig.build.json`、`355 pass / 0 fail`）。
- 架构笔记校验：`.agents/notes/` 通过 `verify-agent-note-tree.ts`、`verify-agent-note-format.ts`、`verify-archived-agent-notes.ts` 与 `check-note-anchors.ts` 四项脚本校验。
