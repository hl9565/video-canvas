# Agent Note: 短剧/营销并发锁序与批量建任务原生保存点规避

Status: implemented

## Problem

短剧分集、片段、营销轮次与批量任务创建共享用户积分账户行（`credit_account`），且多个并发写路径（片段保存/删除重排、批量生成任务重放、营销跟拍重试与后台恢复）会在同一事务内交叉触达父子表与账户表。如果加锁顺序不一致，PostgreSQL 下必然发生死锁；如果批量任务单项插入复用了会内嵌 `r.db.Transaction(...)` 的 `InsertTask`，GORM Dialector 会偷偷打开并吞掉隐式 `SavePoint` 错误，导致单项回滚状态损坏；若仅按集号覆盖集内容而不校对目标集 ID，删除重排并发会让编辑误落到下一集。

## Decision

后端事务与锁管理在所有短剧、营销与批量生图写路径强制遵守固定加锁顺序与显式原生保存点：

- **短剧单集与片段写锁序（`must`）**：`backend/internal/service/drama_segment.go` 中的 `lockSegment`（L163）在锁外反查后，严格按 `Project -> LockEpisodes -> LockSegments` 顺序加锁并在锁内复核 `DramaID` 与 `EpisodeID` 父链；批量集锁通过 `backend/internal/repository/drama_canvas.go` 中的 `EpisodesForUpdate`（L59）与 `backend/internal/repository/lock_helpers.go` 中的 `lockOwnedByIDs`（L23）去重升序加 `FOR UPDATE`。
- **分集删除/重排竞态守卫**：`UpdateEpisodeReq` 携带可选 `ExpectedEpisodeID`（`backend/internal/model/drama.go:173`），`backend/internal/service/drama_episode.go`（L110）与集锁同事务校对 `uint64(*req.ExpectedEpisodeID) == ep.ID`，不一致立即返回 `errcode.ErrCanvasConflict`。
- **批量创建全局预解析 + 原生保存点**：`backend/internal/service/generation_task_batch.go` 中的 `createBatchTx`（L69）在账户锁前严格按 `seg -> mkt -> credit` 顺序预锁全部目标（`backend/internal/service/generation_task_batch_targets.go` 中的 `preResolveBatchTargets`，L16），之后才锁定账户；`createBatchItem`（L146）调用 `backend/internal/repository/generation_task.go` 中的 `ExecSavepoint`（L350）/ `RollbackSavepoint` / `ReleaseSavepoint` 执行原生 `SAVEPOINT sp_batch_<idx>`，并通过平铺 `TxInsertTask`（L338）插入任务（`never` 调用嵌套事务版 `InsertTask`，L321）。
- **营销锁序与可重跑跟拍**：`backend/internal/service/marketing_lock.go` 中的 `lockMarketingRound`（L27）先锁存活项目，再锁轮次与去重升序脚本；后台收尾 `lockMarketingBackground`（L72）使用 `ProjectUnscopedLock`；`backend/internal/service/marketing_retry.go` 中的 `Refollow`（L200）仅拦在途 `Follow`/`Create` 或脚本重写，允许在拆解更新后覆盖已成功轮次。

被否方案见 [拒绝批量复用 InsertTask 与 Phase 2 中途合并上游](../../rejected/architecture/2026-11-16-reuse-inserttask-in-batch-and-early-upstream-merge.md)。

## Alternatives considered

- **在批量创建外层事务中直接调用 `InsertTask` 或使用 GORM 自带 nested `Transaction`** — 代码简洁且免手写 `SAVEPOINT` SQL；但 GORM `Dialector.SavePoint` 在特定错误链下会掩盖数据库错误并打乱回滚层级，因此改用原生 `SAVEPOINT sp_batch_<idx>` 与平铺 `TxInsertTask`。
- **分集保存继续仅凭项目 ID + 集号路由更新，不传 `ExpectedEpisodeID`** — 无需改前后端请求结构；但在一端刚删集重排、另一端自动保存尚未落库时，保存请求会覆盖错位的后继剧集正文，因此必须随锁校对 `expected_episode_id`。

## Consequences

- **收益**：消除短剧、营销与批量创建高并发下的跨表锁序死锁和错集覆盖风险，并确保批量请求在单节点业务校验失败时精准隔离回滚而不丢账户锁或损坏外层事务。
- **代价与已知上限**：任何新增长耗时/多对象并发接口（如后续 Stage B / Stage C）都必须复用 `sortedUniqueUint64`、`preResolveBatchTargets` 前置冻结模式及平铺 `Tx*` 方法；`Tx*` 函数族禁止私自嵌套 `WithTx` / `Transaction`。

## Verification

在 `/Users/huanglin/code/github/video-canvas/backend` 运行实时 PostgreSQL + Redis 测试与 lint：

- `golangci-lint run --new-from-merge-base=master ./...`（Stage A 合批提交 `c1f7c937` 前 `0 issues.`）
- `TEST_DATABASE_DSN="host=127.0.0.1 port=5432 user=postgres dbname=video_canvas_test sslmode=disable" TEST_REDIS_ADDR="127.0.0.1:6379" go test -race -count=1 ./internal/tests/... ./cmd/server`（46 个测试包全绿）
