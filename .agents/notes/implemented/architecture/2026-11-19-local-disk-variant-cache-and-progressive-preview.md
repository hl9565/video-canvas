# Agent Note: 本地磁盘变体缓存与画布渐进顺序预览

Status: implemented

## Problem

在默认使用 `local`（本地磁盘）存储且未配置云厂商图片处理服务（`image_processors`）的部署中，画布与灯箱媒体加载存在四个叠加瓶颈：

1. **原图直接下发**：`AssetService.ResolveVariant` 在无云端处理服务时，`?v=thumb` 回退到 1–3+ MB 原图（PNG），`?v=poster` 直接返回 `404`（视频节点长期只剩暗色占位）。
2. **本地缓存策略失配**：`handler/files.go` 对 `target.Local` 无条件调用 `storage.FileServer`，忽略了 `ResolveVariant` 设置的 `variantFallbackMaxAge`（60 秒），硬编码输出 `Cache-Control: public, max-age=31536000, immutable`——浏览器和 CDN edge 会将“回退后的几 MB 原图”按 `?v=thumb` 锁定缓存一年。
3. **前端并发双倍下载与视频探测泄漏**：在缩放 `>= 0.65`、选中态或 `mode="full"` 下，`MediaPreview` 与灯箱 `ImageStage` 同时挂载 `?v=thumb` 与原图 `<img>`；当无缩略图可用（或本地未缓存旧响应）时，浏览器并发发起两次内容相同的数 MB 下载。此外 `probeMediaAspect` 在视频 `?v=poster` 失败后仍创建 `<video preload="metadata">` 读取原视频元数据。
4. **旧设计前提变化**：[画布素材加载设计](../../../docs/design/画布素材加载设计/画布素材加载设计.md) §9 曾因早期后端镜像未安装 `ffmpeg`、担心跨网络回读与 SQL 迁移队列而搁置后端生图；当前后端镜像（`backend/Dockerfile`）已内置含 `libwebp`/`mjpeg` 的 `ffmpeg`，且 `local` 存储原文件本就在本机磁盘，无需远程回读与数据库表改动。

## Decision

在**零 SQL 迁移、零新增 Go 第三方依赖、零修改云处理适配器与环境变量配置**的前提下，确立以下本地变体生成与渐进预览契约：

- **本地文件与私有缓存目录隔离（`must`）**：
  - `storage.ValidateKey` 继续拒绝所有以 `.` 开头的路径段（`backend/internal/storage/key.go`）。本地派生文件统一存于 `<local.dir>/.variants/<sha256(key)[:2]>/<sha256(key)>/p2/<size>-<mtimeNano>/<variant>.<ext>`（`backend/internal/storage/local_variant.go`），只经由内部方法解析，绝不通过 `/files/` 静态路径直接访问。
  - `storage.FileTarget.Local` 收窄为具体的 `*storage.LocalFile` 描述符（按请求独立 `Open()`，不共享 `*os.File` 偏移量），由 `storage.ServeLocalFile` 显式应用 `storage.CachePolicy`（原图 `public, max-age=31536000, immutable`；本地成功变体 `public, max-age=86400`；非严格回退原图 `private, max-age=60`；变体错误由 `files.go` 显式写 `Cache-Control: no-store`），并以 `localFileETag` 区分变体与原图表示、私有回退省略 `Last-Modified` 避免旧兼容 URL 双向切换时误回 `304`。
  - `LocalStorage.Delete`、`Put` 与变体发布 `PublishVariantContext` 共用包级按「目录 + key」分片的互斥锁 `localKeyLocks[64]`（跨同目录多 `LocalStorage` 实例生效）：仅在“原文件存在性与指纹复核 + `os.Rename`”短临界区加锁，耗时的 `ffmpeg` 编码绝不持锁；删除时同步 `os.RemoveAll` 清理 `.variants/<prefix>/<hash>`。
- **单机有界按需编码与同键共享等待（`must`）**：
  - `videoframe.PreviewEncoder`（`backend/internal/pkg/videoframe/preview.go`）独立于 Agent `Extractor`，仅接受本地常规文件，强制 `-protocol_whitelist file`、媒体解复用白名单 `-format_whitelist`、`-threads 1`、`-filter_threads 1`、`ffprobe` 4000 万像素上限（`40_000_000`）及 1 MiB 流式输出上限；优先使用保留透明通道的 `libwebp`（长边 ≤ 512px 不放大，按显示宽高比缩放并 `setsar=1`、`quality=75`），仅在宿主 `ffmpeg` 缺少 `libwebp`（如本机 macOS）时降级为 `mjpeg`。
  - `service.LocalVariantService`（`backend/internal/service/asset_variant_local.go`）按 `<rootDir>\0<key>\0<size>\0<mtimeNano>\0<variant>` 在 `tasks` 映射中合并同指纹请求，所有等待者监听共享只读关闭信号 `task.done`（不随热点并发取消累积 channel）；限制进程内同时运行 2 个生成任务、最多 64 个不同变体排队（在创建 goroutine 前计数），单任务从入队起算 15 秒总超时。
  - 请求退出只停止单个等待者，共享生成任务生命周期归属应用根上下文；收到停机信号时 `initialize/app.go` 先于 `server.Shutdown` 调用 `localVariants.Close()` 取消并等待全部任务退出（`App.close()` 再做幂等兜底），避免慢任务拖满 HTTP 关停宽限期导致 `.tmp-*` 残留；确定性失败（不存在、超像素、解码失败、缺编码器）进入 30 秒有界冷却（上限 2048 项），队列满、超时与取消绝不进入冷却。
- **局部严格模式 `?pv=2` 与顺序渐进加载（`must`）**：
  - `variantUrl(src, variant, opts?)`（`web/src/utils/canvas/media-lod.ts`）默认保留 `?v=thumb|poster` 兼容契约（确保 `message-attachments.tsx` 及后端 `WithCoverURL` 列表封面在无处理器时仍正常显示）。
  - 仅支持降级的预览入口（`MediaPreview`、`VideoPoster`、`MediaLightbox` 的 `ImageStage`/`VideoStage`、`probeMediaAspect`）开启 `{ strict: true }` 追加 `pv=2`：同时绕开旧 `?v=thumb` 浏览器/CDN `immutable` 缓存，并让 `AssetService.ResolveVariantWithOptions` 在无云端且本地无可用变体时**禁止回退原图**（永久不可用返回 `404`，本地忙碌/超时返回 `503 ErrVariantUnavailable = 90002`）。
  - `ImageLayersInner` 与 `ImageStage` 共用纯判定 `resolveImageLayers`：先只挂载 `?v=thumb&pv=2`，待缩略图 `load` 或 `error` 结算后再按 `preferThumb` 决定是否挂载原图 `src`；一旦原图请求发起即保留挂载，避免缩放跨 0.55/0.65 滞回带反复卸载重下；原图失败但缩略图已就绪时继续保留缩略图。
  - `probeMediaAspect` 对站内 `/files/` 视频若 `?v=poster&pv=2` 失败直接返回 `null`，不再创建 `<video preload="metadata">` 探针；外链与 `blob:` 视频保留原逻辑。

## Alternatives considered

- **在 `assets` 表新增 `thumb_key`/`thumb_status` 列并建立后台异步回填队列**：能持久化变体状态并入库预生成；但增加 SQL 迁移号段消耗、后台任务状态机与上游合并冲突，而基于源指纹的 `.variants/` 惰性按需生成与磁盘原子发布可实现零迁移、老素材首次访问自动加速与删改自动失效。
- **新建 `internal/pkg/localvariant` 独立包与 `configs/config.yaml` 开关，并在云处理器强制关闭 Cloudflare `onerror=redirect`**：抽象更细且可配置；但违反项目直接在 `service` 编排的惯例并增加配置与云适配器合并面。已配置云处理器的环境极少触发回退，保持零配置、零改动云适配器即可获得更低耦合。
- **全局把所有 `variantUrl` 改为 `pv=2` 严格 404**：无需区分调用方；但会导致无 `onError` 原图回退逻辑的聊天附件（`message-attachments.tsx`）和后端列表封面（`WithCoverURL`）在纯 S3 或无 `ffmpeg` 环境下直接显示失效占位，因此严格模式只局部启用。

## Consequences

- **收益**：`local` 存储下的画布图片缩略图与视频封面降至约 10–50 KB 级别且支持透明通道；高缩放 / `full` / 灯箱与回退链路消除同源并发双下载；视频节点获得真实首帧封面且封面缺失时不拉取原视频元数据；现有云处理器、数据库结构与兼容入口零破坏。
- **代价与已知上限**：队列（2 运行 + 64 等待、15 秒预算）在数十个冷素材同时进入队列溢出时仍会返回 `503` 触发前端退回原图一次下载；旧 `?v=thumb` 访问者依赖前端新上线的 `pv=2` 才能绕开先前已写入本地缓存的一年 `immutable` 响应；若 `backend` 启动环境缺少 `libwebp`（如本机 macOS Homebrew `ffmpeg`），本地变体会退回 JPEG 无法在本地复现 WebP 透明压缩，需在含 `libwebp` 的 Linux/容器环境回归验证。

## Verification

在 `/Users/huanglin/code/github/video-canvas` 已执行并全部通过：

- `web`: `REQUIRE_NGINX=1 bash scripts/check.sh web` → `oxfmt --check`（1527 files）、`tsc -b --noEmit`、`oxlint`、`bun test`（**4999 pass, 0 fail**，423 files）、`vite build` 全部通过。
- `backend` 全量校验与真实 PostgreSQL 16 + Redis 7 `-race` 集成测试：`PATH="$HOME/go/bin:$PATH" golangci-lint run --new-from-merge-base=master ./...`（`0 issues.`）、`go build ./...`、`TEST_DATABASE_DSN="host=127.0.0.1 port=5432 user=huanglin dbname=video_canvas_test sslmode=disable" TEST_REDIS_ADDR="127.0.0.1:6379" go test -race -count=1 ./...` 全部通过，且针对 `pkg/videoframe`、`storage`、`service`、`handler` 变体相关定向用例无一跳过。
- 生产 Alpine 容器实测（`23` 上 `video-canvas-backend-1` 实际 `ffmpeg`/`ffprobe` 参数验证，未修改容器状态）：`1536×1024` PNG → `32,952` 字节 WebP，`480×864` MP4 → `7,472` 字节 WebP，`640×480` JPG → `436` 字节 WebP，`640×480` 透明 WebP → `594` 字节 WebP。
