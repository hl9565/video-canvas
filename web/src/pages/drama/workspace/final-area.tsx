import { useCallback, useEffect, useRef, useState } from "react";
import {
  Check,
  Clapperboard,
  Combine,
  Download,
  Film,
  Loader2,
  Play,
  RefreshCw,
  TriangleAlert,
} from "lucide-react";

import { listJobs } from "@/api/drama";
import { DRAMA_ERR, dramaErrorMessage } from "@/api/drama/errors";
import type {
  DramaComposeExport,
  DramaComposeSize,
  DramaComposeVariant,
  DramaJob,
  DramaManifestClip,
} from "@/api/drama/type";
import { VideoPoster } from "@/components/canvas/media-preview";
import { finalFileName } from "@/components/drama/final-file";
import { Button } from "@/components/ui/button";
import { VideoPlayer } from "@/components/video-player";
import { cn } from "@/lib/utils";
import { segmentKey, useDramaSegmentsStore } from "@/store/drama-segments";
import { downloadMedia } from "@/utils/canvas/download";
import {
  activeComposeJob,
  canUseManifestCompose,
  COMPOSE_ASPECT_BASE,
  COMPOSE_SIZES,
  COMPOSE_VARIANT_LABEL,
  COMPOSE_VARIANTS,
  composeCheck,
  composeDimensions,
  composeExportKey,
  DEFAULT_COMPOSE_SIZE,
  DEFAULT_COMPOSE_VARIANTS,
  finalState,
  latestComposeExports,
  missingVideoSeqs,
  normalizeComposeVariants,
  skippedText,
} from "@/utils/drama/compose";
import { jobConflictReason, mergeJob } from "@/utils/drama/jobs";
import { reloadSegments } from "@/utils/drama/segment-events";
import { playableTake } from "@/utils/drama/segments";
import type { WorkspaceStage } from "@/utils/drama/stages";

import { useCompose } from "../use-compose";
import { useWorkspace } from "./context";

/** 「x:y」比例转成 CSS aspect-ratio；认不出来用 9:16 */
const aspectOf = (ratio: string) => (/^\d+:\d+$/.test(ratio) ? ratio.replace(":", "/") : "9/16");

const NO_SEGMENTS: never[] = [];

const mmss = (seconds: number) =>
  `${String(Math.floor(seconds / 60)).padStart(2, "0")}:${String(Math.floor(seconds % 60)).padStart(2, "0")}`;

/** 格式化成片总时长：不足 1 秒保留两位小数（如 0.79s），1 秒及以上按 mm:ss 展示 */
const formatDuration = (seconds: number) => {
  if (!Number.isFinite(seconds) || seconds <= 0) return "00:00";
  if (seconds < 1) return `${seconds.toFixed(2)}s`;
  return mmss(Math.round(seconds));
};

/** 格式化分集清单中单个片段的显示时长 */
const formatSegmentDuration = (seconds: number) => {
  if (!Number.isFinite(seconds) || seconds <= 0) return "0s";
  if (seconds < 1 || !Number.isInteger(seconds)) {
    return `${Number(seconds.toFixed(2))}s`;
  }
  return `${seconds}s`;
};

/**
 * 成片：左边播放当前集的成片，右边是合成面板（已采用片段数、成片时长、片段清单、合成 / 重新合成）。
 * 合成在服务端用 ffmpeg 按顺序拼接各片段采用的视频，不扣积分；没有采用视频的片段会被跳过。
 */
export function FinalArea({
  ep,
  onGo,
}: {
  ep: number;
  onGo: (stage: WorkspaceStage, ep?: number) => void;
}) {
  const { drama, jobs } = useWorkspace();
  const episode = drama.episodes.find((item) => item.no === ep) ?? drama.episodes[0];
  const no = episode?.no ?? 0;
  const segments =
    useDramaSegmentsStore((state) => state.byEpisode[segmentKey(drama.id, no)]) ?? NO_SEGMENTS;
  const { compose, pending, manifests, loadManifest } = useCompose(drama.id);
  const [saving, setSaving] = useState(false);
  const [downloadingKey, setDownloadingKey] = useState<string | null>(null);
  const [size, setSize] = useState<DramaComposeSize>(DEFAULT_COMPOSE_SIZE);
  const [variants, setVariants] = useState<DramaComposeVariant[]>([...DEFAULT_COMPOSE_VARIANTS]);
  const [previewExportState, setPreviewExportState] = useState<{
    dramaId: string;
    ep: number;
    key: string | null;
  }>({ dramaId: "", ep: 0, key: null });
  const previewExportKey =
    previewExportState.dramaId === drama.id && previewExportState.ep === no
      ? previewExportState.key
      : null;
  const setPreviewExportKey = (key: string | null) =>
    setPreviewExportState({ dramaId: drama.id, ep: no, key });
  const [fetchedFinalJobsState, setFetchedFinalJobsState] = useState<{
    dramaId: string;
    jobs: DramaJob[];
  }>({ dramaId: "", jobs: [] });
  const [finalJobFetchNotice, setFinalJobFetchNotice] = useState<{
    attemptKey: string;
    kind: "error" | "missing";
    message: string;
  } | null>(null);
  const [finalJobRetrySeq, setFinalJobRetrySeq] = useState(0);
  const attemptedJobFetchRef = useRef<Set<string>>(new Set());

  const manifestEnabled = canUseManifestCompose(drama.ratio, episode);
  const ratioSupported = drama.ratio in COMPOSE_ASPECT_BASE;
  const finalComposeJobId = episode?.finalComposeJobId;
  const segmentVersion = episode?.segmentVersion ?? 0;
  const segmentsVersionKey = segments
    .map((s) => `${s.id}:${s.revision}:${s.chosenTaskId}`)
    .join("|");

  const fetchedFinalJobs =
    fetchedFinalJobsState.dramaId === drama.id ? fetchedFinalJobsState.jobs : [];
  let exportJobs = fetchedFinalJobs;
  for (const job of jobs) {
    exportJobs = mergeJob(exportJobs, job);
  }
  const hasSucceededFinalJob =
    typeof finalComposeJobId === "string" &&
    finalComposeJobId.length > 0 &&
    exportJobs.some(
      (job) =>
        job.id === finalComposeJobId &&
        job.step === "compose" &&
        job.status === "succeeded" &&
        job.episodes.includes(no),
    );
  const needsFinalJobFetch =
    typeof finalComposeJobId === "string" && finalComposeJobId.length > 0 && !hasSucceededFinalJob;
  const refreshToken = `${episode?.revision ?? 0}:${episode?.updatedAt ?? ""}:${drama.updatedAt}`;
  const attemptKey =
    needsFinalJobFetch && finalComposeJobId
      ? `${drama.id}:${no}:${finalComposeJobId}:${refreshToken}:${finalJobRetrySeq}`
      : "";
  const activeFinalJobNotice =
    finalJobFetchNotice && attemptKey && finalJobFetchNotice.attemptKey === attemptKey
      ? finalJobFetchNotice
      : null;
  const hasFinalJobNotice = activeFinalJobNotice !== null;
  const hasFinalJobError = activeFinalJobNotice?.kind === "error";

  const retryFinalJobFetch = useCallback(() => {
    if (attemptKey) {
      attemptedJobFetchRef.current.delete(attemptKey);
    }
    setFinalJobFetchNotice(null);
    setFinalJobRetrySeq((n) => n + 1);
  }, [attemptKey]);

  // 片段读自片段接口：进到成片页（或换集）时拉一次，之后靠推送保持新鲜
  useEffect(() => {
    if (no > 0) void reloadSegments(drama.id, no);
  }, [drama.id, no]);

  const adopted = segments.filter((segment) => playableTake(segment)).length;

  useEffect(() => {
    if (no <= 0 || adopted === 0 || !manifestEnabled) return;
    void loadManifest(no);
  }, [adopted, loadManifest, manifestEnabled, no, segmentVersion, segmentsVersionKey]);

  // 刷新页面后 store 仅含进行中任务：若当前分集绑定了已完成的 final_compose_job_id 且本地缺失，仅补拉目标已成功任务
  useEffect(() => {
    if (!finalComposeJobId || !needsFinalJobFetch || !attemptKey || hasFinalJobNotice) return;
    const attempted = attemptedJobFetchRef.current;
    if (attempted.has(attemptKey)) return;

    let cancelled = false;
    let settled = false;
    attempted.add(attemptKey);
    void listJobs(drama.id)
      .then((items) => {
        settled = true;
        if (cancelled) {
          attempted.delete(attemptKey);
          return;
        }
        const matched = items.find(
          (item) =>
            item.id === finalComposeJobId &&
            item.step === "compose" &&
            item.status === "succeeded" &&
            item.episodes.includes(no),
        );
        if (matched) {
          setFinalJobFetchNotice(null);
          setFetchedFinalJobsState((prev) => {
            const base = prev.dramaId === drama.id ? prev.jobs : [];
            return { dramaId: drama.id, jobs: mergeJob(base, matched) };
          });
        } else {
          // HTTP 200 但尚未查到目标成功任务：保留 attempted 禁止自动轮询循环，同时允许手动点击重试
          setFinalJobFetchNotice({
            attemptKey,
            kind: "missing",
            message: "暂未读取到本次成片的导出变体记录，可手动重试读取",
          });
        }
      })
      .catch((e) => {
        settled = true;
        attempted.delete(attemptKey);
        if (cancelled) return;
        setFinalJobFetchNotice({
          attemptKey,
          kind: "error",
          message: dramaErrorMessage(e, "读取历史成片记录失败，请重试"),
        });
      });
    return () => {
      cancelled = true;
      if (!settled) {
        attempted.delete(attemptKey);
      }
    };
  }, [attemptKey, drama.id, finalComposeJobId, hasFinalJobNotice, needsFinalJobFetch, no]);

  useEffect(() => {
    if (!hasFinalJobError || typeof window === "undefined") return;
    const onOnline = () => retryFinalJobFetch();
    window.addEventListener("online", onOnline);
    return () => window.removeEventListener("online", onOnline);
  }, [hasFinalJobError, retryFinalJobFetch]);

  if (!episode) {
    return (
      <p className="text-muted-foreground grid h-full place-items-center text-sm">还没有分集</p>
    );
  }

  const final = episode.final;
  const state = finalState(episode);
  const running = activeComposeJob(jobs, no);
  const check = composeCheck(segments, jobConflictReason(jobs, "compose", [no]));
  const manifestState = manifestEnabled ? manifests[no] : undefined;
  const manifest = manifestEnabled ? (manifestState?.data ?? null) : null;
  const localSkipped = missingVideoSeqs(segments);
  const skipped = manifest && manifest.skippedSeqs.length > 0 ? manifest.skippedSeqs : localSkipped;
  const fallbackSeconds = segments.reduce(
    (sum, segment) => sum + (playableTake(segment) ? segment.seconds : 0),
    0,
  );
  const fps = manifest?.manifest?.fps && manifest.manifest.fps > 0 ? manifest.manifest.fps : 24;
  const seconds =
    manifest?.manifest && typeof manifest.manifest.totalFrames === "number"
      ? manifest.manifest.totalFrames / fps
      : fallbackSeconds;
  const clipBySegmentId = new Map<string, DramaManifestClip>();
  const clipBySeq = new Map<number, DramaManifestClip>();
  for (const clip of manifest?.manifest?.clips ?? []) {
    clipBySegmentId.set(clip.segmentId, clip);
    clipBySeq.set(clip.seq, clip);
  }
  const [targetWidth, targetHeight] = composeDimensions(drama.ratio, size);
  const composing = !!running || pending === no;
  const withFinal = drama.episodes.filter((item) => item.final);
  const exportsList = latestComposeExports(exportJobs, no, finalComposeJobId);
  const activeExport: DramaComposeExport | null =
    exportsList.find((item) => composeExportKey(item) === previewExportKey) ?? null;
  const playerAsset = activeExport?.asset ?? final ?? exportsList[0]?.asset ?? null;

  const toggleVariant = (variant: DramaComposeVariant) => {
    setVariants((prev) => {
      if (prev.includes(variant)) {
        const next = prev.filter((item) => item !== variant);
        return next.length > 0 ? next : prev;
      }
      return normalizeComposeVariants([...prev, variant]);
    });
  };

  const download = async () => {
    if (!playerAsset) return;
    setSaving(true);
    try {
      const suffix = activeExport ? `-${activeExport.variant}-${activeExport.size}` : "";
      const baseName = finalFileName(drama.title, no).replace(/\.mp4$/i, "");
      await downloadMedia(playerAsset.url, `${baseName}${suffix}.mp4`);
    } finally {
      setSaving(false);
    }
  };

  const downloadExport = async (item: DramaComposeExport) => {
    const key = composeExportKey(item);
    setDownloadingKey(key);
    try {
      const baseName = finalFileName(drama.title, no).replace(/\.mp4$/i, "");
      await downloadMedia(item.asset.url, `${baseName}-${item.variant}-${item.size}.mp4`);
    } finally {
      setDownloadingKey(null);
    }
  };

  return (
    <div className="flex h-full min-h-0 max-lg:flex-col">
      <section className="relative flex min-w-0 flex-1 flex-col items-center gap-4 overflow-y-auto px-6 py-5">
        <div className="flex w-full items-center gap-2.5">
          <h2 className="font-display text-[17px] font-bold tracking-wide">
            第 {no} 集 · {episode.title}
          </h2>
          {final && (
            <span
              className={cn(
                "rounded-md px-2 py-0.5 text-[11px] font-medium",
                state === "stale"
                  ? "bg-primary/15 text-primary"
                  : "bg-status-success/15 text-status-success",
              )}
            >
              {state === "stale" ? "成片已过期，可重新合成" : "成片已合成"}
            </span>
          )}
          <span className="flex-1" />
          {playerAsset && (
            <Button variant="outline" size="sm" disabled={saving} onClick={() => void download()}>
              {saving ? <Loader2 className="animate-spin" /> : <Download />}
              下载 mp4
            </Button>
          )}
        </div>

        {playerAsset ? (
          // 和画布视频节点同一个播放器：先显示封面，点了才加载视频
          <div
            style={{ aspectRatio: aspectOf(drama.ratio) }}
            className="relative h-[66vh] max-w-full shrink-0 overflow-hidden rounded-2xl bg-black shadow-[0_40px_80px_-30px_rgb(0_0_0/0.9)] ring-1 ring-white/10"
          >
            <VideoPlayer
              key={`${playerAsset.id}:${playerAsset.url}`}
              src={playerAsset.url}
              durationMs={playerAsset.durationMs}
            />
          </div>
        ) : (
          <div className="flex flex-col items-center gap-3 py-14 text-center">
            <span className="bg-muted text-muted-foreground grid size-16 place-items-center rounded-[20px]">
              <Film className="size-7" aria-hidden />
            </span>
            <h3 className="text-[17px] font-semibold">第 {no} 集还没有成片</h3>
            <p className="text-muted-foreground max-w-[40ch] text-[13px]">
              {episode.segmentCount > 0
                ? `${episode.doneSegmentCount}/${episode.segmentCount} 个片段已有视频。采用每个片段的视频后，在右侧合成整集。`
                : "先生成分镜脚本、再逐个片段出视频，最后在这里合成整集。"}
            </p>
            <Button variant="outline" onClick={() => onGo("studio", no)}>
              <Clapperboard />
              去片场
            </Button>
          </div>
        )}

        {withFinal.length > 1 && (
          <div className="w-full">
            <div className="text-muted-foreground mb-2 text-xs">其他集成片</div>
            <div className="flex gap-2.5 overflow-x-auto pb-1">
              {withFinal.map((item) => (
                <button
                  key={item.no}
                  type="button"
                  aria-label={`第 ${item.no} 集成片`}
                  aria-current={item.no === no}
                  onClick={() => onGo("final", item.no)}
                  style={{ aspectRatio: aspectOf(drama.ratio) }}
                  className={cn(
                    "bg-muted focus-visible:ring-ring/50 relative w-16 shrink-0 overflow-hidden rounded-[9px] border-2 outline-none focus-visible:ring-2",
                    item.no === no ? "border-primary" : "hover:border-border border-transparent",
                  )}
                >
                  {item.final && <VideoPoster src={item.final.url} />}
                  <span className="absolute bottom-1 left-1.5 font-mono text-[11px] font-semibold text-white [text-shadow:0_1px_4px_rgb(0_0_0/0.8)]">
                    EP{String(item.no).padStart(2, "0")}
                  </span>
                </button>
              ))}
            </div>
          </div>
        )}
      </section>

      <aside
        aria-label="合成整集"
        className="border-border bg-card flex w-[360px] shrink-0 flex-col border-l max-lg:w-full max-lg:border-t max-lg:border-l-0"
      >
        <div className="px-4 pt-4 pb-2">
          <h3 className="text-[15px] font-semibold">合成整集</h3>
          <p className="text-muted-foreground mt-0.5 text-xs">
            按片段顺序拼接采用的版本，统一到项目比例 · 不扣积分
          </p>
        </div>
        <div className="grid min-h-0 flex-1 content-start gap-3.5 overflow-y-auto px-4 pb-4">
          <div className="grid grid-cols-3 gap-2">
            {[
              [`${adopted}/${segments.length}`, "已采用片段"],
              [formatDuration(seconds), "成片时长"],
              [state === "none" ? "—" : state === "stale" ? "过期" : "最新", "成片状态"],
            ].map(([value, label]) => (
              <div key={label} className="bg-muted rounded-xl px-3 py-2.5">
                <b className="block font-mono text-lg font-semibold">{value}</b>
                <small className="text-muted-foreground text-[11.5px]">{label}</small>
              </div>
            ))}
          </div>
          {skipped.length > 0 && segments.length > 0 && (
            <div className="bg-primary/10 text-muted-foreground flex gap-2.5 rounded-xl px-3 py-2.5 text-[12.5px]">
              <TriangleAlert className="text-primary mt-0.5 size-4 shrink-0" />
              <span>
                {skippedText(skipped)} 没有采用的视频，合成时会被跳过。
                <button
                  type="button"
                  className="text-primary ml-1 underline-offset-2 hover:underline"
                  onClick={() => onGo("studio", no)}
                >
                  去补齐
                </button>
              </span>
            </div>
          )}
          {manifestState?.stale && (
            <div
              role="status"
              className="border-amber-500/40 bg-amber-500/10 flex items-start justify-between gap-2 rounded-xl border px-3 py-2.5 text-xs text-amber-900 dark:text-amber-200"
            >
              <span>
                清单版本已更新（{DRAMA_ERR.manifestStale}），已加载最新清单，请确认后再次导出。
              </span>
              <button
                type="button"
                className="shrink-0 underline underline-offset-2"
                onClick={() => void loadManifest(no)}
              >
                刷新
              </button>
            </div>
          )}
          {manifestState?.error && (
            <div
              role="alert"
              className="border-destructive/40 bg-destructive/10 text-destructive flex items-start justify-between gap-2 rounded-xl border px-3 py-2.5 text-xs"
            >
              <span>{manifestState.error}</span>
              <button
                type="button"
                className="inline-flex shrink-0 items-center gap-1 underline underline-offset-2"
                onClick={() => void loadManifest(no)}
              >
                <RefreshCw className="size-3" />
                重试
              </button>
            </div>
          )}
          {activeFinalJobNotice && (
            <div
              role={activeFinalJobNotice.kind === "error" ? "alert" : "status"}
              className={cn(
                "flex items-start justify-between gap-2 rounded-xl border px-3 py-2.5 text-xs",
                activeFinalJobNotice.kind === "error"
                  ? "border-destructive/40 bg-destructive/10 text-destructive"
                  : "border-amber-500/40 bg-amber-500/10 text-amber-900 dark:text-amber-200",
              )}
            >
              <span>{activeFinalJobNotice.message}</span>
              <button
                type="button"
                aria-label="重试读取成片记录"
                className="inline-flex shrink-0 items-center gap-1 underline underline-offset-2"
                onClick={retryFinalJobFetch}
              >
                <RefreshCw className="size-3" />
                重试
              </button>
            </div>
          )}
          {manifest && manifest.diagnostics.length > 0 && (
            <div className="border-amber-500/30 bg-amber-500/10 flex flex-col gap-1 rounded-xl border px-3 py-2.5 text-xs">
              <div className="flex items-center gap-1.5 font-medium text-amber-900 dark:text-amber-200">
                <TriangleAlert className="size-3.5 shrink-0 text-amber-500" />
                <span>清单检查提示</span>
              </div>
              <ul className="text-muted-foreground list-disc space-y-0.5 pl-4">
                {manifest.diagnostics.map((msg, idx) => (
                  <li key={`${idx}:${msg}`}>{msg}</li>
                ))}
              </ul>
            </div>
          )}

          {/* 导出档位与版本选择（受支持比例的精裁集） */}
          {manifestEnabled ? (
            <div className="bg-muted/40 flex flex-col gap-2.5 rounded-xl border p-3">
              <div className="flex items-center justify-between gap-2">
                <div className="flex flex-col gap-0.5">
                  <span className="text-muted-foreground text-xs font-medium">导出分辨率</span>
                  <span className="text-muted-foreground/80 font-mono text-[11px]">
                    {drama.ratio || "16:9"} · {targetWidth}×{targetHeight}
                  </span>
                </div>
                <div className="flex items-center gap-1" role="group" aria-label="导出分辨率">
                  {COMPOSE_SIZES.map((item) => (
                    <button
                      key={item}
                      type="button"
                      aria-pressed={size === item}
                      onClick={() => setSize(item)}
                      className={cn(
                        "rounded-md border px-2.5 py-1 font-mono text-xs transition-colors",
                        size === item
                          ? "border-primary bg-primary text-primary-foreground font-medium"
                          : "border-border bg-background text-muted-foreground hover:text-foreground",
                      )}
                    >
                      {item}
                    </button>
                  ))}
                </div>
              </div>
              <div className="flex items-center justify-between gap-2">
                <span className="text-muted-foreground text-xs font-medium">导出版本</span>
                <div className="flex items-center gap-1" role="group" aria-label="导出版本">
                  {COMPOSE_VARIANTS.map((variant) => {
                    const active = variants.includes(variant);
                    return (
                      <button
                        key={variant}
                        type="button"
                        aria-pressed={active}
                        onClick={() => toggleVariant(variant)}
                        className={cn(
                          "rounded-md border px-2 py-1 text-xs transition-colors",
                          active
                            ? "border-primary bg-primary/15 text-primary font-medium"
                            : "border-border bg-background text-muted-foreground hover:text-foreground",
                        )}
                      >
                        {COMPOSE_VARIANT_LABEL[variant]}
                      </button>
                    );
                  })}
                </div>
              </div>
              {manifest?.manifest && (
                <div className="text-muted-foreground flex items-center justify-between border-t pt-2 font-mono text-[11px]">
                  <span>
                    清单 v{manifest.segmentVersion} · {manifest.manifest.totalFrames}f
                  </span>
                  <span title={manifest.manifestHash}>
                    #{(manifest.manifestHash ?? "").slice(0, 8)}
                  </span>
                </div>
              )}
            </div>
          ) : (
            <div className="bg-muted/40 text-muted-foreground rounded-xl border px-3 py-2 text-xs">
              {!ratioSupported
                ? `当前项目比例为 ${drama.ratio || "未设置"}，将按原比例整段合成。`
                : `当前集尚未保存帧精裁配置，将按原整段路径合成（在片场保存任一镜头「修剪」后启用 ${drama.ratio} 分档与版本导出）。`}
            </div>
          )}

          {exportsList.length > 0 && (
            <div className="flex flex-col gap-1.5" aria-label="本次导出结果">
              <div className="text-muted-foreground flex items-center justify-between px-0.5 text-xs font-medium">
                <span>导出文件（{exportsList.length}）</span>
                {activeExport && (
                  <button
                    type="button"
                    className="text-primary hover:underline"
                    onClick={() => setPreviewExportKey(null)}
                  >
                    切回成片
                  </button>
                )}
              </div>
              <div className="grid gap-1.5">
                {exportsList.map((item) => {
                  const exportKey = composeExportKey(item);
                  const isPreviewing = activeExport
                    ? composeExportKey(activeExport) === exportKey
                    : false;
                  const isDownloading = downloadingKey === exportKey;
                  return (
                    <div
                      key={exportKey}
                      className={cn(
                        "flex items-center justify-between gap-2 rounded-lg border px-2.5 py-2 text-xs",
                        isPreviewing ? "border-primary bg-primary/5" : "bg-background",
                      )}
                    >
                      <div className="min-w-0">
                        <div className="flex items-center gap-1.5 font-medium">
                          <span>{COMPOSE_VARIANT_LABEL[item.variant]}</span>
                          <span className="bg-muted rounded px-1 py-0.5 font-mono text-[10px]">
                            {item.size}
                          </span>
                        </div>
                        <div className="text-muted-foreground font-mono text-[11px]">
                          {item.width}×{item.height}
                        </div>
                      </div>
                      <div className="flex shrink-0 items-center gap-1">
                        <Button
                          type="button"
                          variant={isPreviewing ? "secondary" : "ghost"}
                          size="sm"
                          className="h-7 px-2 text-xs"
                          onClick={() => setPreviewExportKey(exportKey)}
                        >
                          <Play className="size-3" />
                          预览
                        </Button>
                        <Button
                          type="button"
                          variant="outline"
                          size="sm"
                          disabled={isDownloading}
                          className="h-7 px-2 text-xs"
                          onClick={() => void downloadExport(item)}
                        >
                          {isDownloading ? (
                            <Loader2 className="size-3 animate-spin" />
                          ) : (
                            <Download className="size-3" />
                          )}
                          下载
                        </Button>
                      </div>
                    </div>
                  );
                })}
              </div>
            </div>
          )}

          <ul className="grid gap-0.5">
            {segments.map((segment) => {
              const take = playableTake(segment);
              const clip = clipBySegmentId.get(segment.id) ?? clipBySeq.get(segment.seq);
              const clipFrames = clip ? clip.masterOutF - clip.masterInF : 0;
              const segDuration =
                take && clip && clipFrames > 0
                  ? formatSegmentDuration(clipFrames / fps)
                  : `${segment.seconds}s`;
              return (
                <li
                  key={segment.id}
                  className="hover:bg-muted/60 flex items-center gap-2.5 rounded-[10px] p-2 text-[13px]"
                >
                  <span
                    style={{ aspectRatio: aspectOf(drama.ratio) }}
                    className="bg-muted relative w-[30px] shrink-0 overflow-hidden rounded-[5px]"
                  >
                    {take?.asset && <VideoPoster src={take.asset.url} />}
                  </span>
                  <span className="min-w-0 flex-1">
                    片段 {String(segment.seq + 1).padStart(2, "0")}
                    <small className="text-muted-foreground block text-[11.5px]">
                      {segDuration}
                    </small>
                  </span>
                  {take ? (
                    <span className="bg-status-success/15 text-status-success inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-[11px] font-medium">
                      <Check className="size-3" aria-hidden />
                      已采用
                    </span>
                  ) : segment.status === "running" ? (
                    <span className="bg-primary/15 text-primary rounded-md px-1.5 py-0.5 text-[11px] font-medium">
                      生成中
                    </span>
                  ) : (
                    <span className="bg-muted text-muted-foreground rounded-md px-1.5 py-0.5 text-[11px]">
                      跳过
                    </span>
                  )}
                </li>
              );
            })}
            {segments.length === 0 && (
              <li className="text-muted-foreground px-2 py-6 text-center text-xs">
                这一集还没有片段
              </li>
            )}
          </ul>
        </div>
        <div className="border-border border-t p-3.5">
          {running ? (
            <div>
              <div className="mb-1.5 flex justify-between text-[12.5px]">
                <span className="text-primary font-medium">正在合成…</span>
                <span className="font-mono">{running.progress}%</span>
              </div>
              <div className="bg-muted h-1 overflow-hidden rounded-full">
                <div
                  className="bg-primary h-full rounded-full transition-[width] duration-300"
                  style={{ width: `${running.progress}%` }}
                />
              </div>
            </div>
          ) : (
            <Button
              className="w-full"
              variant={state === "fresh" ? "outline" : "default"}
              disabled={!check.enabled || composing}
              title={check.reason ?? "按顺序拼接各片段采用的视频，不扣积分"}
              onClick={() =>
                void compose(
                  no,
                  manifestEnabled
                    ? { useManifest: true, ratio: drama.ratio, size, variants, manifest }
                    : { useManifest: false, ratio: drama.ratio, segmentVersion, segments },
                )
              }
            >
              {composing ? <Loader2 className="animate-spin" /> : <Combine />}
              {state === "none" ? `合成第 ${no} 集` : "重新合成"}
            </Button>
          )}
          {check.reason && !running && (
            <p className="text-muted-foreground mt-2 text-center text-xs">{check.reason}</p>
          )}
        </div>
      </aside>
    </div>
  );
}
