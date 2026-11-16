# Agent Note: 上游双段迁移兼容、Fork 错误码隔离与出片原子保存点

Status: implemented

## Problem

在 Stage A 之后、Stage B（`PR2`）之前，将上游 `zaylora/video-canvas` `v0.6.12`（`90b950f1`，23 个提交）合入分支时出现四类结构性冲突，必须一次解决且保证后续可持续拉取上游更新：

1. **迁移编号撞车**：本地 Stage A 已占用了 `0036_drama_edit.sql`，而上游 `v0.6.12` 同时占用 `0036_marketing_adaptations.sql` 与 `0037_marketing_prompt_edits.sql`。直接覆盖或重编号上游 SQL 会导致每次上游合入都发生持续冲突。
2. **错误码区间冲突**：上游在 `67028` / `67029` / `68018` 扩展错误码并统一使用 `502`，与 Fork 原在 `67xxx` 注册的精裁/报价/幂等错误码发生碰撞与语义混淆。
3. **发布与配置耦合**：直接覆写仓根版本或配置所有者（`owner/repo`）会让 Upstream Check 与 Docker 发布校验失配，而引入额外 compose 覆盖文件又会增加运维配置分裂。
4. **H3 自动转写与批量出片并发竞态**：上游 `v0.6.12` 新增原稿手改、H3 自动转写与出片后置扣费；若无请求意图防抖、完整输入指纹、内容版本 CAS 以及同事务原生保存点，双实例并发出片、整轮失败重放或队列入队失败会导致重复转写扣费、陈旧候选覆盖用户手改稿，以及无任务孤立流水。

## Decision

在独立同步分支 `sync/upstream-0.6.12` 完成上游 `90b950f1` 的 `--no-ff` 合入，并确立以下长期边界（部分取代 [精裁清单与上游基线边界](./2026-11-16-timeline-manifest-trim-and-preview-guardrails.md) 中的「策略 A 上游基线冻结」，其精裁与预览守卫继续有效；并沿用 [短剧/营销并发锁序与原生保存点笔记](./2026-11-16-drama-lock-hierarchy-and-savepoint-avoidance.md) 的事务不变量）：

- **双段连续 SQL 迁移号段（`must`）**：`backend/internal/tests/migration/schema_test.go` 与 `backend/internal/tests/migration/drama_edit_test.go` 强制校验两段各自连续递增区间：`0001–8999` 专归上游（原样保留 `0036_marketing_adaptations.sql` 与 `0037_marketing_prompt_edits.sql`），`9001–9999` 专归 Fork（将 Stage A 尚未落库的 `0036_drama_edit.sql` 按字节原样重命名为 `backend/migrations/9001_drama_edit.sql`，预留 `9002+` 给 Stage B/C，禁用 `0000` 与 `9000`）。`migration.Run` 允许已执行 `9001` 的库在后续升级时补跑新合入的 `0036` / `0037`。
- **Fork 独立错误码号段**：保留上游 `67028` / `67029` / `68018`（`502`）；Fork 专有错误码迁至 `90000–99999`（`backend/internal/pkg/errcode/fork_codes.go` 与 `web/src/api/drama/fork-errors.ts`：`ErrIdempotencyConflict=90001/409`、`ErrDramaManifestStale=91001/409`、`ErrDramaQuoteExpired=91002/409`、`ErrDramaEditInvalid=91003/400`），前端仅对 `30002 + HTTP 409` 触发 revision 刷新恢复。
- **精简发布与仓库归属解耦**：保留仓根 `VERSION` 与 `CHANGELOG.md` 契约及 `.github/workflows/release-docker.yml` 发布校验；将默认更新仓库恢复为 `zaylora/video-canvas`（`backend/internal/config/config.go` 等），在 `docker-compose.yml` 透传可选空串 `APP_UPDATE_REPO: ${APP_UPDATE_REPO:-}`（依赖 Viper 非 `AllowEmptyEnv` 保持安全回退）；在 `scripts/deploy.sh` 加执行前文件存在检查，并在 `.github/workflows/release-docker.yml` `quality` 作业补入 `nginx` 安装以覆盖 `scripts/check.sh web`。
- **营销转写与出片零 `9002` 新表反重放闭环（`must`）**：
  - `MarketingRenderIntent` 与 `marketingRenderIntentSig`（`backend/internal/service/generation_task_batch_marketing.go`）在任何 LLM 调用前通过 `CheckMarketingRenderRoot` + `CheckMarketingRenderReplay` 校验完整根键意图（区分 `nil` 与 `{}` 参数，绑定原始 `script_ids` 目标清单；签名以 `"mkt:v1:"` 为前缀并截断至 `varchar(64)` 上限 `64` 字符）；`ListMarketingRootTasks` 与事务咨询锁 `LockMarketingRoot`（`pg_advisory_xact_lock`）保证顺序或并发下同根键切分不交目标集均以 `90001` 拒绝。
  - `AdaptInputHash`（`backend/internal/marketing/adapt.go`）与 `adaptationInputHash`（`backend/internal/service/marketing_adapt.go`）绑定格式、原稿、时长、参考数量/角色/参考视频/镜头、长度上限及素材 ID 顺序与模型参数；`EditPrompt`、`RestorePrompt`、`Adapt`、`EditAdaptation` 强制通过 `If-Match` / `expected_version`（`marketingExpectedVersion`）执行内容版本 CAS，缺失或过期一律 `409 ErrMarketingConflict`（`68009`）。
  - `snapshotRenderScript`（`backend/internal/service/marketing_lock.go` / `marketing_render.go`）与 `resolveLockedMarketingTarget`（`backend/internal/service/generation_task_batch_marketing.go`）统一先检 `MarketingScriptRewriting` 与活跃任务并返回 `ErrMarketingScriptBusy`（`68006`）；未命中缓存转写稿时，`prepareMarketingRender` 经 `validateAndQuoteMarketingVideo` 复用 `applyMarketingVideoParams`（锁定 `prompt`/`op`/`images`/`videos`/`generate_audio=true`）并向上传播校验错误，再由 `checkAdaptBalance(..., ensureAccount=false)` 做只读余额预检（不足直接 `402` 且不开户、不调 LLM）；LLM 调用期间持无锁候选，落库时由 `validateAdaptationCandidate` 在父链锁（`Project -> Round -> Scripts -> Credit`）内复核 `sameMarketingContent` 与有效赢家（采用库内赢家时 `write=false`，零重复写库、零收费）。
  - `createBatchItem`（`backend/internal/service/generation_task_batch.go`）在原生 `SAVEPOINT sp_batch_<idx>` 内原子执行 `commitBatchMarketingAdapt` + `TxInsertTask` + 积分冻结 + 入队；任一步业务/系统失败统一回滚，不留孤立转写稿或扣费流水。
- **并发与数据竞争对齐**：`drama_breakdown.go` 使用 `mu.Lock()` 与前置 `completedBreakdownBatches` 消除非前缀检查点并发计数竞争；`UpdateRoundSettings`、`checkProductSwap`、`RefreshFacts`、`DeleteProject` 统一纳入 `Project -> Round -> Scripts -> Credit` 锁序并保留 Stage A 的 `requeue` 执行代推进。

相关历史决策见 [拒绝批量复用 InsertTask 与 Phase 2 中途合上游](../../rejected/architecture/2026-11-16-reuse-inserttask-in-batch-and-early-upstream-merge.md)（其中上游合入的重提条件已由本独立双段合并分支满足）。

## Alternatives considered

- **把上游 `0036_marketing_adaptations.sql` / `0037_marketing_prompt_edits.sql` 重编号为 Fork 自己的顺序号**：直观且单段连续，不用修改迁移校验器；但此后每次合入上游新迁移都要人工改名、改 SHA-256 记录并重审全部迁移顺序，长期维护成本远高于一次性确立 `0001–8999` vs `9001–9999` 双段约定。
- **新增 `9002_marketing_render_ops.sql` 持久化营销出片操作记录表**：能在进程崩溃期间跨请求持久化记录“转写已完成待建视频任务”的中间态，并在表内做行级租约；但会提前消耗 Fork 迁移号段并引入额外的表清理/垃圾回收状态机，而基于“LLM 调用零落库零扣费 + 父链锁赢家采用 + 生成任务原保存点原子提交”已完全消除双扣费与孤立状态，复杂度更低。
- **将 Fork 版本与更新脚本拆入 `docs/fork/` 和 `docker-compose.override.yml`**：表面减少对根文件的文本冲突；但会破坏根 `VERSION` / `CHANGELOG.md` 的发布流水线既有契约并要求额外运维覆盖文件，因此保留精简的根文件与环境变量透传方案。

## Consequences

- **收益**：彻底解除上游 `v0.6.12` 与 Fork Stage A 在迁移、错误码和工作流上的冲突；为后续 Stage B（`9002`）与 Stage C（`9003`）建立清晰的号段与错误码隔离边界；自动转写与批量出片在双实例竞争、异载重放和队列故障下均保持积分与任务零漂移。
- **代价与已知上限**：Fork 未保留独立 `marketing_render_ops` 中间态表，意味着若进程在 H3 LLM 返回前崩溃，该次未落库的转写算力无法跨进程复用（可重试，且绝不重复扣用户积分）；后续任何合入上游的 PR 都必须遵守 `0001–8999` 仅上游、`9001–9999` 仅 Fork、`90000–99999` 仅 Fork 错误码的契约。

## Verification

在 `/Users/huanglin/code/github/video-canvas` 运行以下全量门禁验证（最终编辑后均需全绿）：

- `backend`: `go build ./... && go vet ./... && golangci-lint run --new-from-merge-base=archive/pre-sync-34705df8 ./...`
- `backend` PostgreSQL 16 + Redis 7 `-race`: `TEST_DATABASE_DSN="host=127.0.0.1 port=5432 user=postgres dbname=video_canvas_test sslmode=disable" TEST_REDIS_ADDR="127.0.0.1:6379" go test -race -count=1 ./internal/tests/... ./cmd/server`（含双段迁移 `9001` + `0036/0037`、`arch_test` 及营销/短剧并发测试）
- `web`: `REQUIRE_NGINX=1 bash scripts/check.sh web`
- `agent`: `npm run format:check && npm run lint && npm run typecheck && npm test && npm run build`
