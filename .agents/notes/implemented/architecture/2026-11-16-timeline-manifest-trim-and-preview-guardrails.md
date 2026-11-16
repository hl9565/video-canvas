# Agent Note: 精裁整数帧清单、短链预览守卫与上游基线边界

Status: implemented

## Problem

短剧成片合成与片段精裁原先缺少标准化媒体探针与整帧/毫秒边界校验，容易因浮点秒误差、未探测源文件或并发重放导致音画撕裂；同时浏览器预览与生成网关接口若缺少短期签名边界与等待限时，容易产生无界轮询阻塞或跨资源泄露。另一方面，fork 当前已完成迁移 `0036_drama_edit.sql`（Stage A PR1），而上游 `zaylora/video-canvas` `v0.6.12` 亦占用了 `0036_marketing_adaptations.sql` / `0037_marketing_prompt_edits.sql` 并重构了服务器日志及营销提示词转写链路。

## Decision

Stage A 在媒体精裁、前端等待/版本号保护以及上下游基线边界上固定以下实现契约：

- **规范探针与整帧范围清单（`must`）**：`backend/internal/pkg/videoframe/compose_manifest.go` 中的 `ComposeManifest`（L67）基于已落库探针规范化信息校验 `FrameRange`（`resolveManifestRange`，L172）并驱动服务端混音/拼接渲染；`backend/internal/service/drama_manifest.go` 中的 `ResolveEpisodeManifest`（L40）与 `PrepareOrResolveEpisodeManifest`（L50）通过 `backend/internal/service/drama_preview_auth.go` 中的 HMAC 签名票据 `dramaPreviewStore`（L49）与并发渲染信号量（`maxConcurrentPreviewRenders = 4`、`maxManifestErrEntries = 256`）控制预览资源。
- **前端清单轮询与版本单调守卫**：`web/src/utils/drama/manifest.ts` 中的 `waitForEpisodeManifest`（L133）使用 `raceWithinDeadline` + 可取消 `sleep` 严格封顶等待窗口；`web/src/pages/drama/workspace/use-workspace.ts` 中的 `mergeEpisodeListWithMonotonicVersions`（L37）防止陈旧异步响应与缺 ID patch 覆盖高版本分集状态；[锁序与保存点笔记](./2026-11-16-drama-lock-hierarchy-and-savepoint-avoidance.md) 约定删除/保存竞态由 `expected_episode_id` 闭环。
- **策略 A 上游基线冻结**：Phase 2 期间保持 fork `v0.6.11` + Stage A PR1（迁移至 `backend/migrations/0036_drama_edit.sql`）为唯一实施基线，暂不并入上游 `zaylora/video-canvas` `v0.6.12` 的 `23` 个提交（其迁移 `0036/0037` 与本地 `0036_drama_edit.sql` 及规划中的 Stage B `0037` 撞号，且重叠 `25` 个文件含 `14` 处直接文本冲突）。

被否决策见 [拒绝批量复用 InsertTask 与 Phase 2 中途合并上游](../../rejected/architecture/2026-11-16-reuse-inserttask-in-batch-and-early-upstream-merge.md)。

## Alternatives considered

- **直接用浮点秒数拼接 ffmpeg 命令并在前端使用默认永久轮询或宽松版本号覆盖** — 省去探针缓存表 `asset_media_probes`、`ComposeManifest` 与 `raceWithinDeadline` 逻辑；但浮点裁剪在不同编码器下易出现边界黑帧/少帧，且预览失败或无超时轮询会挂死前端组件，因此被否。
- **在 Stage A 与 Stage B 之间先强行合并上游 `zaylora/video-canvas` `v0.6.12`** — 可提前获得上游分批并行拆解（`01064493`）与营销转写特性；但 `01064493` 强依赖上游 4 期日志系统重写，合并会产生 `0036` 迁移号直接冲突、营销状态机双改与 `14` 个冲突文件重审成本，因此继续按策略 A 延后至 Phase 2 收口后再在独立分支顺延迁移号合入。

## Consequences

- **收益**：端到端精裁与合成基于可哈希规范化清单，预览票据受 TTL 与并发上限保护；Phase 2 的 Stage B（`0037`）与 Stage C（`0038`）无需在不稳定多端重合基础上返工。
- **代价与已知上限**：与上游 `zaylora/video-canvas` 继续保持 `23` 个提交差异；当 Phase 2 全部落地或用户明确决定提前同步上游时，必须在独立分支将上游 `0036_marketing_adaptations.sql` / `0037_marketing_prompt_edits.sql` 顺延重编号并逐一按锁定不变量核对 `25` 个重叠文件（尤其是 `marketing_job.go`、`generation_task_create.go`、`drama_compose.go`、`drama_job.go`）。

## Verification

在 `/Users/huanglin/code/github/video-canvas` 验证（commit `c1f7c937` 前全套门禁）：

- `REQUIRE_NGINX=1 bash scripts/check.sh web`（`4751 pass, 0 fail`，含真实 `nginx` 与 `vite build`）
- `backend` 真实 PostgreSQL + Redis 全量单测/集成测试（46 包全绿）与 `golangci-lint`（`0 issues.`）
