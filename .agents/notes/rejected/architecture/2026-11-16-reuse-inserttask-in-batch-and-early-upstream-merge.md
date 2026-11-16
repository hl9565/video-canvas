# Agent Note: 拒绝在批量创建内复用 InsertTask 及 Phase 2 中途合并上游 v0.6.12

Status: rejected — 批量内调 InsertTask 会触发 GORM 隐式 SavePoint 吞错，Phase 2 中途合上游会引发 0036 迁移撞号与营销/日志大面积重写

## Problem

在落地 Stage A PR1 批量任务创建与评估 `zaylora/video-canvas` 上游 `v0.6.12`（`90b950f1`，较 `1ca089e2` 新增 23 个提交）时，需要抉择两件事：① 批量逐项插库能否直接复用 `backend/internal/repository/generation_task.go` 中的 `InsertTask`（L321）；② Stage A 后、Stage B（`PR2`）前是否合并上游 `v0.6.12`。

## Proposal

1. 在 `createBatchItem` 事务中直接调用现有 `GenerationTaskRepository.InsertTask` 写入任务记录，省去新增平铺 `TxInsertTask`（`backend/internal/repository/generation_task.go:338`）与原生 `SAVEPOINT sp_batch_<idx>` 方法族。
2. 在 Stage B 开始前直接合并上游 `zaylora/video-canvas:master`（`v0.6.12`），提前引入资产并行拆解、四期统一日志与营销写后改模功能。

## Alternatives considered

- **平铺 `TxInsertTask` + 原生 `SAVEPOINT sp_batch_<idx>`，以及策略 A（Phase 2 收口后再独立分支并上游）** — 最终采纳，见 [锁序与原生保存点笔记](../../implemented/architecture/2026-11-16-drama-lock-hierarchy-and-savepoint-avoidance.md) 与 [清单精裁与上游基线边界](../../implemented/architecture/2026-11-16-timeline-manifest-trim-and-preview-guardrails.md)。

## Risks

- `InsertTask` 内部调用了 `r.db.WithContext(ctx).Transaction(...)`，在外层 `WithTx` 下会经 GORM Dialector 转成嵌套保存点并可能静默吞错或破坏回滚；且批量目标锁定必须先于账户锁按 `seg -> mkt -> credit` 冻结，无法由 `InsertTask` 单项代劳。
- 上游 `v0.6.12` 改了 `210` 个文件并新增 `0036_marketing_adaptations.sql` / `0037_marketing_prompt_edits.sql`：与 Stage A 的 `0036_drama_edit.sql` 直接发生 `0036` 迁移号冲突；只挑上游 `01064493`（资产并行拆解）也不可行，因其在 `drama_job.go` 中直接依赖上游 4 期日志体系 `dramaJobRun` / `logger.StatusCanceled`；盲目合并将在Stage A 刚过终审的 `25` 个共享文件（含 `14` 处直接文本冲突）上叠加高风险二次审计。
- **重提条件**：① `GenerationTaskRepository` 全链路脱离 GORM 隐式嵌套事务机制时可重评插库抽象；② 当 Stage B + Stage C 全部落地验收，或在专门评审预算下决定提前开启上游同步分支并统一顺延迁移编号（`0036–0038` 与上游 `0036–0037` 不重叠）时，再启动上游合并。
