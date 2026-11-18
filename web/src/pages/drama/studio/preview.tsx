import { useCallback, useEffect, useRef, useState } from "react";
import { AnimatePresence, motion } from "motion/react";
import { Clapperboard, Film, Loader2, RefreshCw, Scissors, TriangleAlert } from "lucide-react";

import {
  createEpisodePreview,
  getEpisodeManifest,
  getEpisodeManifestV2,
  getEpisodePreview,
} from "@/api/drama";
import {
  DRAMA_ERR,
  dramaErrorMessage,
  isDramaError,
  isEditInvalidError,
  isManifestStaleError,
  isRevisionConflict,
} from "@/api/drama/errors";
import type {
  DramaComposeVariant,
  DramaPreviewRange,
  ProgramDiagnostic,
  EpisodePreviewView,
  Segment,
  UpdateSegmentInput,
} from "@/api/drama/type";
import { VideoPoster } from "@/components/canvas/media-preview";
import { SegmentTrimDrawer } from "@/components/drama/segment-trim-drawer";
import { TaskGenerationStatus } from "@/components/generation-status";
import { Button } from "@/components/ui/button";
import { EASE_OUT } from "@/lib/motion";
import { cn } from "@/lib/utils";
import { useDramaJobsStore } from "@/store/drama";
import { segmentKey, useDramaSegmentsStore } from "@/store/drama-segments";
import {
  COMPOSE_VARIANT_LABEL,
  COMPOSE_VARIANTS,
  type ComposeManifest,
  manifestProgramDiagnostics,
  skippedText,
} from "@/utils/drama/compose";
import { waitForEpisodeManifest, waitForEpisodeManifestV2 } from "@/utils/drama/manifest";
import {
  clampPreviewRange,
  isVideoVisual,
  programDiagnosticsFrom,
  programDiagnosticText,
  usesProgramFeatures,
  VISUAL_RATIO_MESSAGE,
} from "@/utils/drama/program";
import { reloadSegments } from "@/utils/drama/segment-events";
import { hasNonDefaultSegmentEdit, segmentEditOf } from "@/utils/drama/segment-trim";
import { playableTake } from "@/utils/drama/segments";
import { ApiError } from "@/utils/requests/request";

import { TakeCarousel } from "./take-carousel";
import { usePlaybackContext } from "./use-playback";
import { useStudio } from "./use-studio";

/** 「x:y」比例转成 CSS aspect-ratio；认不出来用 9:16 */
const aspectOf = (ratio: string) => (/^\d+:\d+$/.test(ratio) ? ratio.replace(":", "/") : "9/16");

/** 「x:y」比例转成宽 / 高的数值；认不出来按 9:16 */
const aspectValue = (ratio: string) => {
  const match = /^(\d+):(\d+)$/.exec(ratio);
  return match ? Number(match[1]) / Number(match[2]) : 9 / 16;
};

/**
 * 播放器背后的氛围光：把当前画面的海报放大、虚化、压低透明度铺在底下，
 * 换片段时淡入淡出；没有画面时只有一团琥珀色的暗光。
 */
function Ambient({ src }: { src: string | null }) {
  return (
    <div aria-hidden className="pointer-events-none absolute inset-0 overflow-hidden">
      <div
        className="absolute inset-0"
        style={{
          background:
            "radial-gradient(60% 50% at 50% 45%, color-mix(in oklch, var(--primary) 14%, transparent), transparent 70%)",
        }}
      />
      <AnimatePresence>
        {src && (
          <motion.div
            key={src}
            initial={{ opacity: 0 }}
            animate={{ opacity: 0.32 }}
            exit={{ opacity: 0 }}
            transition={{ duration: 0.8, ease: EASE_OUT }}
            className="absolute -inset-[15%] blur-[80px] saturate-150"
          >
            <VideoPoster src={src} />
          </motion.div>
        )}
      </AnimatePresence>
    </div>
  );
}

const PREVIEW_POLL_MS = 400;
const PREVIEW_GET_RETRY_BASE_MS = 200;
const PREVIEW_GET_RETRY_MAX_MS = 1600;
const PREVIEW_MAX_TRANSIENT_GET_RETRIES = 4;
const PREVIEW_MAX_STALE_RETRIES = 2;
const PREVIEW_MAX_TICKET_EXPIRED_RETRIES = 1;
const PREVIEW_MAX_REBUILD_RETRIES = 1;
const PREVIEW_MAX_STREAM_ERROR_RETRIES = 3;
/** 连续播放推进至少 1.5s 且连续采样窗口跨度 >= 1.2s 后才重置流错误预算，防止 loadedmetadata/canplay 或卡顿拼接清零预算 */
const PREVIEW_STABLE_PLAYBACK_SECONDS = 1.5;
const PREVIEW_STABLE_MIN_WALL_MS = 1_200;
/** 判定播放头卡顿/低速/长盲区的短周期采样参数（timeupdate 与定时器路径共用） */
const PREVIEW_PROGRESS_EPSILON_SEC = 1e-4;
const PREVIEW_STABILITY_SAMPLE_MS = 400;
const PREVIEW_MAX_SILENT_GAP_MS = 1_100;
const PREVIEW_RATE_CHECK_MIN_WALL_MS = 200;
const PREVIEW_MIN_ADVANCE_RATE_RATIO = 0.65;
/** 后端签名 stream token 有效期为 2 分钟；每 75 秒刷新一次并支持过期 401 媒体错误恢复 */
const PREVIEW_STREAM_REFRESH_MS = 75_000;

type PreviewErrorSource = "init" | "poll" | "failed" | "stream";

type CpuPreviewSnapshot = {
  key: string;
  manifest: ComposeManifest | null;
  programDiagnostics?: ProgramDiagnostic[];
  preview: EpisodePreviewView | null;
  failedObservedAtMs: number | null;
  loading: boolean;
  error: string | null;
  errorSource: PreviewErrorSource | null;
  staleNotice: boolean;
  streamReloadNonce: number;
};

interface PreviewRunController {
  cancelled: boolean;
  signal: AbortSignal;
  lifecycleRunning: boolean;
  videoRecoveryInFlight: boolean;
  bgRenewalInFlight: boolean;
  streamErrorCount: number;
  playbackAnchorTime: number | null;
  playbackAnchorWallMs: number | null;
  lastProgressTime: number | null;
  lastProgressWallMs: number | null;
  seeking: boolean;
  clearStableTimer: () => void;
  scheduleStableSample: (onSample: () => void) => void;
  cancel: () => void;
  sleep: (ms: number) => Promise<boolean>;
}

type PreviewGetOutcome =
  | { kind: "ok"; preview: EpisodePreviewView }
  | { kind: "ticket_invalid"; error: unknown }
  | { kind: "manifest_stale"; error: unknown }
  | { kind: "fatal"; error: unknown }
  | { kind: "transient_exhausted"; error: unknown }
  | { kind: "cancelled" };

/** 后端确认预览票据已过期或不存在（67001 / HTTP 404 / 410） */
const isTicketInvalidError = (error: unknown): boolean =>
  error instanceof ApiError &&
  (error.code === DRAMA_ERR.notFound || error.status === 404 || error.status === 410);

/** 可通过保留原 ticket 并退避重试 GET 恢复的瞬态错误 */
const isTransientPreviewError = (error: unknown): boolean => {
  if (!(error instanceof ApiError)) return true;
  if (
    error.code === "NETWORK_ERROR" ||
    error.code === "TIMEOUT" ||
    error.code === "UNKNOWN_ERROR"
  ) {
    return true;
  }
  return error.status === 0 || error.status === 408 || error.status === 429 || error.status >= 500;
};

const backoffMs = (attempt: number, baseMs = PREVIEW_GET_RETRY_BASE_MS): number =>
  Math.min(PREVIEW_GET_RETRY_MAX_MS, baseMs * 2 ** attempt);

const failedCooldownDeadlineMs = (preview: EpisodePreviewView | null): number | null => {
  if (preview?.status !== "failed") return null;
  const expiresAt = preview.expiresAt;
  if (typeof expiresAt !== "number" || !Number.isFinite(expiresAt) || expiresAt <= 0) {
    return null;
  }
  return expiresAt * 1000;
};

const finitePlaybackTime = (video: HTMLVideoElement | null): number | null => {
  if (!video) return null;
  const time = video.currentTime;
  return typeof time === "number" && Number.isFinite(time) && time >= 0 ? time : null;
};

const monotonicNowMs = (): number =>
  typeof performance !== "undefined" && typeof performance.now === "function"
    ? performance.now()
    : Date.now();

const isProgressRateStalled = (
  mediaDeltaSec: number,
  wallElapsedMs: number,
  playbackRate: number,
  minWallMs: number,
  minRateRatio: number,
): boolean => {
  if (wallElapsedMs < minWallMs) return false;
  const expectedAdvanceSec = (wallElapsedMs / 1000) * playbackRate;
  return mediaDeltaSec < expectedAdvanceSec * minRateRatio;
};

const isActivelyPlaying = (video: HTMLVideoElement | null): video is HTMLVideoElement =>
  !!video &&
  !video.paused &&
  !video.ended &&
  !video.seeking &&
  typeof video.playbackRate === "number" &&
  Number.isFinite(video.playbackRate) &&
  video.playbackRate > 0;

const resetPlaybackBaseline = (
  controller: PreviewRunController,
  baselineTime: number | null,
  seeking = false,
  wallMs = monotonicNowMs(),
) => {
  controller.clearStableTimer();
  controller.playbackAnchorTime = baselineTime;
  controller.playbackAnchorWallMs = baselineTime === null ? null : wallMs;
  controller.lastProgressTime = baselineTime;
  controller.lastProgressWallMs = baselineTime === null ? null : wallMs;
  controller.seeking = seeking;
};

/** 暂停/等待/拖动等非连续推进状态下重置进度基线（保留累计错误预算） */
const resetInactiveBaseline = (
  controller: PreviewRunController,
  video: HTMLVideoElement | null,
) => {
  const seeking = Boolean(video?.seeking) || controller.seeking;
  resetPlaybackBaseline(controller, video?.seeking ? null : finitePlaybackTime(video), seeking);
};

function failedSnapshotPatch(
  preview: EpisodePreviewView,
  previous: CpuPreviewSnapshot,
): Partial<Omit<CpuPreviewSnapshot, "key">> {
  return {
    preview,
    failedObservedAtMs: Date.now(),
    loading: false,
    error: preview.errorMessage?.trim() || "CPU 预览生成失败，请检查片段视频并重试",
    errorSource: "failed",
    programDiagnostics: preview.programDiagnostics?.length
      ? preview.programDiagnostics
      : previous.programDiagnostics,
    staleNotice: preview.errorCode === DRAMA_ERR.manifestStale || previous.staleNotice,
  };
}

/** 创建绑定单次预览生命周期的控制器，卸载或版本失效时立即清理所有挂起定时器与在途请求 */
function createPreviewRunController(): PreviewRunController {
  const abortController = new AbortController();
  const waiters = new Set<{
    timer: ReturnType<typeof setTimeout>;
    resolve: (awake: boolean) => void;
  }>();
  let stableTimer: ReturnType<typeof setTimeout> | null = null;
  const controller: PreviewRunController = {
    cancelled: false,
    signal: abortController.signal,
    lifecycleRunning: false,
    videoRecoveryInFlight: false,
    bgRenewalInFlight: false,
    streamErrorCount: 0,
    playbackAnchorTime: null,
    playbackAnchorWallMs: null,
    lastProgressTime: null,
    lastProgressWallMs: null,
    seeking: false,
    clearStableTimer() {
      if (stableTimer !== null) {
        clearTimeout(stableTimer);
        stableTimer = null;
      }
    },
    scheduleStableSample(onSample: () => void) {
      controller.clearStableTimer();
      if (controller.cancelled) return;
      stableTimer = setTimeout(() => {
        stableTimer = null;
        if (!controller.cancelled) onSample();
      }, PREVIEW_STABILITY_SAMPLE_MS);
    },
    cancel() {
      if (controller.cancelled) return;
      controller.cancelled = true;
      abortController.abort();
      controller.clearStableTimer();
      for (const waiter of waiters) {
        clearTimeout(waiter.timer);
        waiter.resolve(false);
      }
      waiters.clear();
    },
    sleep(ms: number) {
      if (controller.cancelled) return Promise.resolve(false);
      return new Promise<boolean>((resolve) => {
        const entry = {
          timer: undefined as unknown as ReturnType<typeof setTimeout>,
          resolve: (awake: boolean) => {
            waiters.delete(entry);
            resolve(awake);
          },
        };
        entry.timer = setTimeout(() => entry.resolve(true), ms);
        waiters.add(entry);
      });
    },
  };
  return controller;
}

/** 查询预览票据状态；瞬态 GET 失败时保留原 ticket 并有界退避重试 */
async function getPreviewWithRetry(
  dramaId: string,
  no: number,
  ticket: string,
  controller: PreviewRunController,
  maxTransientRetries: number,
): Promise<PreviewGetOutcome> {
  let transientFailures = 0;
  for (;;) {
    if (controller.cancelled) return { kind: "cancelled" };
    try {
      const preview = await getEpisodePreview(dramaId, no, ticket);
      if (controller.cancelled) return { kind: "cancelled" };
      return { kind: "ok", preview };
    } catch (error) {
      if (controller.cancelled) return { kind: "cancelled" };
      if (isTicketInvalidError(error)) return { kind: "ticket_invalid", error };
      if (isManifestStaleError(error)) return { kind: "manifest_stale", error };
      if (!isTransientPreviewError(error)) return { kind: "fatal", error };
      transientFailures += 1;
      if (transientFailures > maxTransientRetries) {
        return { kind: "transient_exhausted", error };
      }
      const awake = await controller.sleep(backoffMs(transientFailures - 1));
      if (!awake || controller.cancelled) return { kind: "cancelled" };
    }
  }
}

/** 持续轮询同一预览票据直到 ready / failed / 确认失效或取消 */
async function pollPreviewTicket(
  dramaId: string,
  no: number,
  ticket: string,
  controller: PreviewRunController,
  onPending: (preview: EpisodePreviewView) => void,
): Promise<PreviewGetOutcome> {
  let missingStreamAttempts = 0;
  for (;;) {
    const res = await getPreviewWithRetry(
      dramaId,
      no,
      ticket,
      controller,
      PREVIEW_MAX_TRANSIENT_GET_RETRIES,
    );
    if (res.kind !== "ok") return res;
    if (res.preview.status === "ready") {
      if (res.preview.streamUrl) return res;
      missingStreamAttempts += 1;
      if (missingStreamAttempts > PREVIEW_MAX_TRANSIENT_GET_RETRIES) {
        return { kind: "fatal", error: new Error("预览流地址缺失，请重试") };
      }
    } else if (res.preview.status === "failed") {
      return res;
    } else {
      missingStreamAttempts = 0;
    }
    onPending(res.preview);
    const awake = await controller.sleep(PREVIEW_POLL_MS);
    if (!awake || controller.cancelled) return { kind: "cancelled" };
  }
}

function capturePlaybackResume(
  video: HTMLVideoElement | null,
  ref: { current: { time: number; paused: boolean } | null },
) {
  const savedTime = video && Number.isFinite(video.currentTime) ? video.currentTime : 0;
  const savedPaused = video ? video.paused : true;
  if (!ref.current || savedTime > 0 || !savedPaused) {
    ref.current = { time: savedTime, paused: savedPaused };
  }
}

function CpuEpisodePreview({
  dramaId,
  no,
  ratio,
  invalidationKey,
  useV2,
  selectedSegmentId,
  onManifestStale,
  onClose,
}: {
  dramaId: string;
  no: number;
  ratio: string;
  invalidationKey: string;
  useV2: boolean;
  selectedSegmentId?: string;
  onManifestStale?: () => Promise<void> | void;
  onClose: () => void;
}) {
  const onManifestStaleRef = useRef(onManifestStale);
  useEffect(() => {
    onManifestStaleRef.current = onManifestStale;
  }, [onManifestStale]);
  const [variant, setVariant] = useState<DramaComposeVariant>("clean");
  const [refreshTick, setRefreshTick] = useState(0);
  const [cooldownTickMs, setCooldownTickMs] = useState(() => Date.now());
  const [range, setRange] = useState<DramaPreviewRange | "segment" | null>(null);
  const [inText, setInText] = useState("0");
  const [outText, setOutText] = useState("");
  const previewRange = useV2 ? range : null;
  const rangeSegment = previewRange === "segment" ? selectedSegmentId : undefined;
  const intentKey = `${dramaId}:${no}:${invalidationKey}:${variant}:${useV2}:${JSON.stringify(previewRange)}:${rangeSegment ?? ""}`;
  const activeRunKey = `${intentKey}::${refreshTick}`;
  const [snapshot, setSnapshot] = useState<CpuPreviewSnapshot | null>(null);
  const videoRef = useRef<HTMLVideoElement | null>(null);
  const resumePlaybackRef = useRef<{ time: number; paused: boolean } | null>(null);
  const activeControllerRef = useRef<{
    runKey: string;
    controller: PreviewRunController;
  } | null>(null);
  const rebuildCountRef = useRef(0);
  const pendingStaleNoticeRef = useRef(false);
  const manualRefreshInFlightRef = useRef(false);
  const cooldownV1EmptyCheckedRef = useRef<string | null>(null);

  const current = snapshot?.key === activeRunKey ? snapshot : null;
  const manifest = current?.manifest ?? null;
  const programDiagnostics = current?.programDiagnostics ?? manifestProgramDiagnostics(manifest);
  const preview = current?.preview ?? null;
  const loading = current ? current.loading : true;
  const error = current?.error ?? null;
  const errorSource = current?.errorSource ?? null;
  const staleNotice = current?.staleNotice ?? false;
  const streamReloadNonce = current?.streamReloadNonce ?? 0;

  const cooldownDeadlineMs = errorSource === "failed" ? failedCooldownDeadlineMs(preview) : null;
  const observedNowMs = Math.max(cooldownTickMs, current?.failedObservedAtMs ?? 0);
  const remainingCooldownMs =
    cooldownDeadlineMs !== null ? Math.max(0, cooldownDeadlineMs - observedNowMs) : 0;
  const cooldownSec = Math.ceil(remainingCooldownMs / 1000);
  const inFailedCooldown = errorSource === "failed" && remainingCooldownMs > 0;

  useEffect(() => {
    if (errorSource !== "failed" || cooldownDeadlineMs === null) return;
    let timer: ReturnType<typeof setTimeout> | null = null;
    const scheduleNext = () => {
      const now = Date.now();
      const left = cooldownDeadlineMs - now;
      if (left <= 0) {
        timer = setTimeout(() => setCooldownTickMs(Date.now()), 0);
        return;
      }
      // 分段等待最多 1 秒，既即时更新倒计时，也防止超长 expiresAt 溢出 32 位定时器上限
      const stepMs = Math.min(left, 1000);
      timer = setTimeout(() => {
        const tickNow = Date.now();
        setCooldownTickMs(tickNow);
        if (tickNow < cooldownDeadlineMs) {
          scheduleNext();
        }
      }, stepMs);
    };
    scheduleNext();
    return () => {
      if (timer !== null) clearTimeout(timer);
    };
  }, [cooldownDeadlineMs, errorSource]);

  const previewUrlRef = useRef<string | null>(null);
  useEffect(() => {
    previewUrlRef.current = preview?.streamUrl ?? null;
    const activeEntry = activeControllerRef.current;
    if (activeEntry && activeEntry.runKey === activeRunKey && !activeEntry.controller.cancelled) {
      resetPlaybackBaseline(activeEntry.controller, null);
    }
  }, [activeRunKey, preview?.streamUrl]);

  useEffect(() => {
    rebuildCountRef.current = 0;
    pendingStaleNoticeRef.current = false;
  }, [intentKey]);

  const applySnapshot = useCallback(
    (
      runKey: string,
      updater:
        | Partial<Omit<CpuPreviewSnapshot, "key">>
        | ((prev: CpuPreviewSnapshot) => Partial<Omit<CpuPreviewSnapshot, "key">>),
    ) => {
      setSnapshot((prev) => {
        const base: CpuPreviewSnapshot =
          prev && prev.key === runKey
            ? prev
            : {
                key: runKey,
                manifest: null,
                preview: null,
                failedObservedAtMs: null,
                loading: true,
                error: null,
                errorSource: null,
                staleNotice: false,
                streamReloadNonce: 0,
              };
        const patch = typeof updater === "function" ? updater(base) : updater;
        return { ...base, ...patch, key: runKey };
      });
    },
    [],
  );

  const triggerRebuild = useCallback(
    (controller: PreviewRunController, runKey: string, stale: boolean) => {
      if (controller.cancelled) return;
      if (rebuildCountRef.current >= PREVIEW_MAX_REBUILD_RETRIES) {
        applySnapshot(runKey, (prev) => ({
          preview: null,
          loading: false,
          staleNotice: stale || prev.staleNotice,
          error: stale ? "分镜清单版本已变更，请刷新预览" : "预览票据已过期，请重试预览",
          errorSource: "init",
        }));
        return;
      }
      rebuildCountRef.current += 1;
      if (stale) {
        pendingStaleNoticeRef.current = true;
        onManifestStaleRef.current?.();
      }
      setRefreshTick((n) => n + 1);
    },
    [applySnapshot],
  );

  const runLifecycle = useCallback(
    async (controller: PreviewRunController, runKey: string, startTicket?: string) => {
      if (controller.cancelled || controller.lifecycleRunning) return;
      controller.lifecycleRunning = true;
      const staleCarried = pendingStaleNoticeRef.current;
      pendingStaleNoticeRef.current = false;
      try {
        if (staleCarried) {
          applySnapshot(runKey, { staleNotice: true });
        }
        let staleRetries = 0;
        let checkedV1Empty = false;
        let expiredRetries = 0;
        let ticketToPoll = startTicket ?? null;

        while (!controller.cancelled) {
          if (ticketToPoll) {
            const targetTicket = ticketToPoll;
            ticketToPoll = null;
            const polled = await pollPreviewTicket(
              dramaId,
              no,
              targetTicket,
              controller,
              (pendingView) =>
                applySnapshot(runKey, {
                  preview: pendingView,
                  loading: true,
                  error: null,
                  errorSource: null,
                }),
            );
            if (polled.kind === "cancelled") return;
            if (polled.kind === "ok") {
              if (polled.preview.status === "ready") {
                applySnapshot(runKey, {
                  preview: polled.preview,
                  failedObservedAtMs: null,
                  loading: false,
                  error: null,
                  errorSource: null,
                });
                return;
              }
              applySnapshot(runKey, (prev) => failedSnapshotPatch(polled.preview, prev));
              return;
            }
            if (polled.kind === "ticket_invalid") {
              expiredRetries += 1;
              if (expiredRetries > PREVIEW_MAX_TICKET_EXPIRED_RETRIES) {
                applySnapshot(runKey, {
                  preview: null,
                  loading: false,
                  error: dramaErrorMessage(polled.error, "预览票据已过期，请重试预览"),
                  errorSource: "init",
                });
                return;
              }
              continue;
            }
            if (polled.kind === "manifest_stale") {
              staleRetries += 1;
              applySnapshot(runKey, { preview: null, staleNotice: true, loading: true });
              if (staleRetries > PREVIEW_MAX_STALE_RETRIES) {
                applySnapshot(runKey, {
                  preview: null,
                  loading: false,
                  staleNotice: true,
                  error: dramaErrorMessage(polled.error, "分镜清单版本已变更，请刷新预览"),
                  errorSource: "init",
                });
                return;
              }
              await onManifestStaleRef.current?.();
              await controller.sleep(0);
              if (controller.cancelled) return;
              continue;
            }
            // fatal 或 transient_exhausted：保留已创建的 ticket，允许用户点击重试直接继续查询 GET
            applySnapshot(runKey, {
              loading: false,
              error: dramaErrorMessage(polled.error, "CPU 预览状态查询失败，请重试"),
              errorSource: "poll",
            });
            return;
          }

          try {
            const manifestResult = await (
              useV2 ? waitForEpisodeManifestV2 : waitForEpisodeManifest
            )(dramaId, no, {
              signal: controller.signal,
              isCancelled: () =>
                controller.cancelled || activeControllerRef.current?.controller !== controller,
              sleep: (ms) => controller.sleep(ms),
            });
            if (manifestResult.kind === "cancelled") return;
            if (manifestResult.kind === "timeout") {
              applySnapshot(runKey, {
                loading: false,
                error: "分镜清单准备超时，请稍后重试",
                errorSource: "init",
              });
              return;
            }
            const currentManifest = manifestResult.manifest;
            if (controller.cancelled || activeControllerRef.current?.controller !== controller) {
              return;
            }
            const diagnostics = manifestProgramDiagnostics(currentManifest);
            applySnapshot(runKey, { manifest: currentManifest, programDiagnostics: diagnostics });
            if (diagnostics.length || (useV2 && !currentManifest.manifestHash)) {
              applySnapshot(runKey, {
                loading: false,
                error: diagnostics.length ? "请修复节目单后再预览" : "暂无可预览的片段",
                errorSource: "init",
              });
              return;
            }
            const clip =
              previewRange === "segment"
                ? currentManifest.manifest?.clips.find((item) => item.segmentId === rangeSegment)
                : null;
            if (previewRange === "segment" && !clip) {
              applySnapshot(runKey, {
                loading: false,
                error: "当前镜头不在预览清单中",
                errorSource: "init",
              });
              return;
            }
            const requested = clip
              ? { inF: clip.masterInF, outF: clip.masterOutF }
              : previewRange === "segment"
                ? null
                : previewRange;
            const rangeF =
              requested && currentManifest.manifest
                ? clampPreviewRange(requested, currentManifest.manifest.totalFrames)
                : undefined;

            const created = await createEpisodePreview(dramaId, no, {
              segmentVersion: currentManifest.segmentVersion,
              manifestHash: currentManifest.manifestHash,
              variant,
              ...(rangeF ? { rangeF } : {}),
            });
            if (controller.cancelled) return;

            if (
              created.status === "pending" ||
              (created.status === "ready" && !created.streamUrl)
            ) {
              applySnapshot(runKey, {
                preview: created,
                loading: true,
                error: null,
                errorSource: null,
              });
              ticketToPoll = created.ticket;
              continue;
            }
            if (created.status === "failed") {
              applySnapshot(runKey, (prev) => failedSnapshotPatch(created, prev));
              return;
            }
            applySnapshot(runKey, {
              preview: created,
              failedObservedAtMs: null,
              loading: false,
              error: null,
              errorSource: null,
            });
            return;
          } catch (err) {
            if (controller.cancelled) return;
            const v1EmptyRetry =
              !useV2 && !checkedV1Empty && isDramaError(err, DRAMA_ERR.nothingToCompose);
            if (isManifestStaleError(err) || v1EmptyRetry) {
              if (v1EmptyRetry) {
                checkedV1Empty = true;
              } else {
                staleRetries += 1;
              }
              applySnapshot(runKey, {
                preview: null,
                staleNotice: !v1EmptyRetry,
                loading: true,
              });
              if (staleRetries > PREVIEW_MAX_STALE_RETRIES) {
                applySnapshot(runKey, {
                  preview: null,
                  loading: false,
                  staleNotice: true,
                  error: dramaErrorMessage(err, "分镜清单版本已变更，请刷新预览"),
                  errorSource: "init",
                });
                return;
              }
              await onManifestStaleRef.current?.();
              await controller.sleep(0);
              if (controller.cancelled) return;
              continue;
            }
            applySnapshot(runKey, {
              loading: false,
              error: dramaErrorMessage(err, "CPU 预览加载失败，请稍后重试"),
              programDiagnostics: programDiagnosticsFrom(err),
              errorSource: "init",
            });
            return;
          }
        }
      } finally {
        controller.lifecycleRunning = false;
      }
    },
    [applySnapshot, dramaId, no, previewRange, rangeSegment, useV2, variant],
  );

  useEffect(() => {
    const controller = createPreviewRunController();
    activeControllerRef.current = { runKey: activeRunKey, controller };
    void runLifecycle(controller, activeRunKey);
    return () => {
      controller.cancel();
      resumePlaybackRef.current = null;
      manualRefreshInFlightRef.current = false;
      if (activeControllerRef.current?.controller === controller) {
        activeControllerRef.current = null;
      }
    };
  }, [activeRunKey, runLifecycle]);

  const activeReadyTicket = preview?.status === "ready" && !error ? preview.ticket : null;

  useEffect(() => {
    if (!activeReadyTicket) return;
    const activeEntry = activeControllerRef.current;
    if (!activeEntry || activeEntry.runKey !== activeRunKey || activeEntry.controller.cancelled) {
      return;
    }
    const { controller } = activeEntry;
    const expectedRunKey = activeRunKey;

    const renewStreamUrl = async () => {
      if (
        controller.cancelled ||
        controller.bgRenewalInFlight ||
        controller.videoRecoveryInFlight ||
        activeControllerRef.current?.controller !== controller
      ) {
        return;
      }
      controller.bgRenewalInFlight = true;
      try {
        const res = await getPreviewWithRetry(dramaId, no, activeReadyTicket, controller, 2);
        if (controller.cancelled || activeControllerRef.current?.controller !== controller) return;

        if (res.kind === "ok") {
          if (res.preview.status === "ready" && res.preview.streamUrl) {
            if (res.preview.streamUrl !== previewUrlRef.current) {
              capturePlaybackResume(videoRef.current, resumePlaybackRef);
              applySnapshot(expectedRunKey, { preview: res.preview });
            }
          } else if (res.preview.status === "pending") {
            applySnapshot(expectedRunKey, {
              preview: res.preview,
              loading: true,
              error: null,
              errorSource: null,
            });
            void runLifecycle(controller, expectedRunKey, res.preview.ticket);
          } else if (res.preview.status === "failed") {
            applySnapshot(expectedRunKey, (prev) => failedSnapshotPatch(res.preview, prev));
          }
          return;
        }
        if (res.kind === "ticket_invalid") {
          triggerRebuild(controller, expectedRunKey, false);
        } else if (res.kind === "manifest_stale") {
          triggerRebuild(controller, expectedRunKey, true);
        } else if (res.kind === "fatal") {
          applySnapshot(expectedRunKey, {
            loading: false,
            error: dramaErrorMessage(res.error, "CPU 预览加载失败，请稍后重试"),
            errorSource: "init",
          });
        }
        // transient_exhausted：后台定时续签遇到瞬态失败时保持现有可用播放，不丢弃 ticket
      } finally {
        controller.bgRenewalInFlight = false;
      }
    };

    const timer = setInterval(() => {
      void renewStreamUrl();
    }, PREVIEW_STREAM_REFRESH_MS);
    return () => {
      clearInterval(timer);
    };
  }, [activeReadyTicket, activeRunKey, applySnapshot, dramaId, no, runLifecycle, triggerRebuild]);

  const handleVideoError = async () => {
    const ticket = preview?.ticket;
    const streamUrl = preview?.streamUrl ?? null;
    const activeEntry = activeControllerRef.current;
    if (
      !ticket ||
      !activeEntry ||
      activeEntry.runKey !== activeRunKey ||
      activeEntry.controller.cancelled ||
      activeEntry.controller.videoRecoveryInFlight
    ) {
      return;
    }
    const { controller } = activeEntry;
    const expectedRunKey = activeRunKey;
    resetPlaybackBaseline(controller, null);
    if (controller.streamErrorCount >= PREVIEW_MAX_STREAM_ERROR_RETRIES) {
      applySnapshot(expectedRunKey, {
        loading: false,
        error: "预览流加载失败，请重试",
        errorSource: "stream",
      });
      return;
    }

    controller.videoRecoveryInFlight = true;
    const attemptIndex = controller.streamErrorCount;
    controller.streamErrorCount += 1;
    capturePlaybackResume(videoRef.current, resumePlaybackRef);

    try {
      if (attemptIndex > 0) {
        const awake = await controller.sleep(backoffMs(attemptIndex - 1));
        if (
          !awake ||
          controller.cancelled ||
          activeControllerRef.current?.controller !== controller
        ) {
          return;
        }
      }

      const res = await getPreviewWithRetry(
        dramaId,
        no,
        ticket,
        controller,
        PREVIEW_MAX_TRANSIENT_GET_RETRIES,
      );
      if (controller.cancelled || activeControllerRef.current?.controller !== controller) return;

      if (res.kind === "ok") {
        if (res.preview.status === "ready" && res.preview.streamUrl) {
          if (res.preview.streamUrl !== streamUrl) {
            applySnapshot(expectedRunKey, {
              preview: res.preview,
              loading: false,
              error: null,
              errorSource: null,
            });
          } else {
            // 同一秒续签可能返回完全相同的签名 URL：保留原 ticket 并重载同一地址，不重发 POST
            applySnapshot(expectedRunKey, (prev) => ({
              preview: res.preview,
              loading: false,
              error: null,
              errorSource: null,
              streamReloadNonce: prev.streamReloadNonce + 1,
            }));
          }
        } else if (res.preview.status === "pending") {
          applySnapshot(expectedRunKey, {
            preview: res.preview,
            loading: true,
            error: null,
            errorSource: null,
          });
          void runLifecycle(controller, expectedRunKey, res.preview.ticket);
        } else {
          applySnapshot(expectedRunKey, (prev) => failedSnapshotPatch(res.preview, prev));
        }
        return;
      }

      if (res.kind === "ticket_invalid") {
        triggerRebuild(controller, expectedRunKey, false);
      } else if (res.kind === "manifest_stale") {
        triggerRebuild(controller, expectedRunKey, true);
      } else if (res.kind === "transient_exhausted") {
        // 瞬态 GET 失败时保留原 ticket，由用户重试直接续查 GET
        applySnapshot(expectedRunKey, {
          loading: false,
          error: dramaErrorMessage(res.error, "预览流恢复失败，请重试"),
          errorSource: "poll",
        });
      } else if (res.kind === "fatal") {
        applySnapshot(expectedRunKey, {
          loading: false,
          error: dramaErrorMessage(res.error, "预览授权失败，请重试"),
          errorSource: "init",
        });
      }
    } finally {
      controller.videoRecoveryInFlight = false;
    }
  };

  const markStreamStable = (controller: PreviewRunController) => {
    if (
      !controller.cancelled &&
      activeControllerRef.current?.controller === controller &&
      !controller.videoRecoveryInFlight
    ) {
      controller.streamErrorCount = 0;
      rebuildCountRef.current = 0;
    }
  };

  const getActiveController = (): PreviewRunController | null => {
    const activeEntry = activeControllerRef.current;
    if (!activeEntry || activeEntry.runKey !== activeRunKey || activeEntry.controller.cancelled) {
      return null;
    }
    return activeEntry.controller;
  };

  const armStabilitySampler = (controller: PreviewRunController) => {
    if (controller.streamErrorCount === 0 && rebuildCountRef.current === 0) {
      controller.clearStableTimer();
      return;
    }
    controller.scheduleStableSample(() => {
      const liveController = getActiveController();
      if (liveController !== controller || controller.videoRecoveryInFlight) {
        return;
      }
      observePlaybackSample(controller, videoRef.current, monotonicNowMs());
    });
  };

  /** timeupdate 与 400ms 定时采样共用的连续播放窗口观测：任一停滞、低速或长盲区即重建窗口 */
  const observePlaybackSample = (
    controller: PreviewRunController,
    video: HTMLVideoElement | null,
    tickWallMs: number,
  ) => {
    const nowTime = finitePlaybackTime(video);
    if (!isActivelyPlaying(video) || nowTime === null) {
      resetInactiveBaseline(controller, video);
      return;
    }
    if (controller.seeking) {
      // seeking 事件后未见 seeked 时的首个非 seek 样本：只重设基线，不计推进
      resetPlaybackBaseline(controller, nowTime, false, tickWallMs);
      armStabilitySampler(controller);
      return;
    }
    if (
      controller.playbackAnchorTime === null ||
      controller.playbackAnchorWallMs === null ||
      controller.lastProgressTime === null ||
      controller.lastProgressWallMs === null
    ) {
      resetPlaybackBaseline(controller, nowTime, false, tickWallMs);
      armStabilitySampler(controller);
      return;
    }
    if (nowTime < controller.playbackAnchorTime || nowTime < controller.lastProgressTime) {
      resetPlaybackBaseline(controller, nowTime, false, tickWallMs);
      armStabilitySampler(controller);
      return;
    }

    const stepWallMs = Math.max(0, tickWallMs - controller.lastProgressWallMs);
    const stepMedia = nowTime - controller.lastProgressTime;
    const windowWallMs = Math.max(0, tickWallMs - controller.playbackAnchorWallMs);

    if (stepWallMs > PREVIEW_MAX_SILENT_GAP_MS) {
      // 超过 1.1s 未见中间推进样本，不能跨越未观测盲区累计连续播放
      resetPlaybackBaseline(controller, nowTime, false, tickWallMs);
      armStabilitySampler(controller);
      return;
    }

    const stepRateStalled =
      stepWallMs >= PREVIEW_RATE_CHECK_MIN_WALL_MS &&
      (stepMedia <= PREVIEW_PROGRESS_EPSILON_SEC ||
        isProgressRateStalled(
          stepMedia,
          stepWallMs,
          video.playbackRate,
          PREVIEW_RATE_CHECK_MIN_WALL_MS,
          PREVIEW_MIN_ADVANCE_RATE_RATIO,
        ));
    const windowRateStalled =
      windowWallMs >= PREVIEW_RATE_CHECK_MIN_WALL_MS &&
      isProgressRateStalled(
        nowTime - controller.playbackAnchorTime,
        windowWallMs,
        video.playbackRate,
        PREVIEW_RATE_CHECK_MIN_WALL_MS,
        PREVIEW_MIN_ADVANCE_RATE_RATIO,
      );
    if (stepRateStalled || windowRateStalled) {
      resetPlaybackBaseline(controller, nowTime, false, tickWallMs);
      armStabilitySampler(controller);
      return;
    }

    if (stepMedia <= PREVIEW_PROGRESS_EPSILON_SEC) {
      // < 200ms 内的重复同位样本保留原 lastProgressWallMs，等跨过卡顿阈值再判
      armStabilitySampler(controller);
      return;
    }
    if (
      stepWallMs > 0 &&
      stepWallMs < PREVIEW_RATE_CHECK_MIN_WALL_MS &&
      isProgressRateStalled(
        stepMedia,
        stepWallMs,
        video.playbackRate,
        0,
        PREVIEW_MIN_ADVANCE_RATE_RATIO,
      )
    ) {
      armStabilitySampler(controller);
      return;
    }

    controller.lastProgressTime = nowTime;
    controller.lastProgressWallMs = tickWallMs;

    if (
      nowTime - controller.playbackAnchorTime >= PREVIEW_STABLE_PLAYBACK_SECONDS &&
      windowWallMs >= PREVIEW_STABLE_MIN_WALL_MS
    ) {
      resetPlaybackBaseline(controller, nowTime, false, tickWallMs);
      markStreamStable(controller);
      return;
    }

    armStabilitySampler(controller);
  };

  const handleVideoInterrupted = () => {
    const controller = getActiveController();
    if (!controller) return;
    resetInactiveBaseline(controller, videoRef.current);
  };

  const handleVideoEnded = () => {
    const controller = getActiveController();
    if (!controller) return;
    resetPlaybackBaseline(controller, null);
  };

  const handleVideoSeeking = () => {
    const controller = getActiveController();
    if (!controller) return;
    resetPlaybackBaseline(controller, null, true);
  };

  const handleVideoSeeked = () => {
    const controller = getActiveController();
    if (!controller) return;
    const video = videoRef.current;
    if (video?.seeking) {
      resetPlaybackBaseline(controller, null, true);
      return;
    }
    resetPlaybackBaseline(controller, finitePlaybackTime(video), false);
    if (!controller.videoRecoveryInFlight && isActivelyPlaying(video)) {
      observePlaybackSample(controller, video, monotonicNowMs());
    }
  };

  const handleVideoRateChange = () => {
    const controller = getActiveController();
    if (!controller) return;
    const video = videoRef.current;
    if (!isActivelyPlaying(video)) {
      resetInactiveBaseline(controller, video);
      return;
    }
    if (!controller.videoRecoveryInFlight) {
      observePlaybackSample(controller, video, monotonicNowMs());
    }
  };

  const handleVideoLoadedMetadata = () => {
    const pending = resumePlaybackRef.current;
    resumePlaybackRef.current = null;
    const video = videoRef.current;
    if (pending && video) {
      try {
        if (pending.time > 0 && Number.isFinite(pending.time)) {
          video.currentTime = pending.time;
        }
      } catch {
        // jsdom 等无媒体解码实现环境下忽略 currentTime 异常
      }
      if (!pending.paused) {
        try {
          void video.play?.()?.catch?.(() => {});
        } catch {
          // 忽略播放恢复异常
        }
      }
    }
    const controller = getActiveController();
    if (controller) {
      // 仅记录基线时间，不在此处清零错误预算，防止 loadedmetadata->error 循环无限重试
      const anchor = finitePlaybackTime(video) ?? pending?.time ?? 0;
      resetPlaybackBaseline(
        controller,
        isActivelyPlaying(video) ? anchor : null,
        Boolean(video?.seeking),
      );
    }
  };

  const handleVideoPlaying = () => {
    const controller = getActiveController();
    if (!controller || controller.videoRecoveryInFlight) return;
    observePlaybackSample(controller, videoRef.current, monotonicNowMs());
  };

  const handleVideoTimeUpdate = () => {
    const controller = getActiveController();
    if (!controller || controller.videoRecoveryInFlight) return;
    observePlaybackSample(controller, videoRef.current, monotonicNowMs());
  };

  const triggerManualRebuild = () => {
    rebuildCountRef.current = 0;
    pendingStaleNoticeRef.current = false;
    cooldownV1EmptyCheckedRef.current = null;
    setRefreshTick((n) => n + 1);
  };

  const handleManualRefresh = () => {
    if (
      errorSource === "failed" &&
      cooldownDeadlineMs !== null &&
      Date.now() < cooldownDeadlineMs
    ) {
      if (manualRefreshInFlightRef.current) return;
      const activeEntry = activeControllerRef.current;
      const expectedRunKey = activeRunKey;
      const knownManifest = manifest;
      if (!knownManifest) {
        setCooldownTickMs(Date.now());
        return;
      }
      manualRefreshInFlightRef.current = true;
      void (useV2 ? getEpisodeManifestV2 : getEpisodeManifest)(dramaId, no, {
        signal: activeEntry?.controller.signal,
      })
        .then((fresh) => {
          if (
            activeControllerRef.current?.runKey !== expectedRunKey ||
            activeControllerRef.current?.controller !== activeEntry?.controller ||
            activeEntry?.controller.cancelled
          ) {
            return;
          }
          if (
            fresh.status === "preparing" ||
            fresh.manifestHash !== knownManifest.manifestHash ||
            fresh.segmentVersion !== knownManifest.segmentVersion
          ) {
            triggerManualRebuild();
            return;
          }
          setCooldownTickMs(Date.now());
        })
        .catch((err: unknown) => {
          if (
            activeControllerRef.current?.runKey !== expectedRunKey ||
            activeControllerRef.current?.controller !== activeEntry?.controller ||
            activeEntry?.controller.cancelled
          ) {
            return;
          }
          if (
            !useV2 &&
            isDramaError(err, DRAMA_ERR.nothingToCompose) &&
            cooldownV1EmptyCheckedRef.current !== expectedRunKey &&
            onManifestStaleRef.current
          ) {
            cooldownV1EmptyCheckedRef.current = expectedRunKey;
            void Promise.resolve(onManifestStaleRef.current())
              .catch(() => {})
              .finally(() => {
                if (
                  activeControllerRef.current?.runKey === expectedRunKey &&
                  activeControllerRef.current?.controller === activeEntry?.controller &&
                  !activeEntry?.controller.cancelled
                ) {
                  setCooldownTickMs(Date.now());
                }
              });
            return;
          }
          setCooldownTickMs(Date.now());
        })
        .finally(() => {
          manualRefreshInFlightRef.current = false;
        });
      return;
    }
    triggerManualRebuild();
  };

  const handleRetry = () => {
    const activeEntry = activeControllerRef.current;
    if (
      errorSource === "poll" &&
      preview?.ticket &&
      activeEntry &&
      activeEntry.runKey === activeRunKey &&
      !activeEntry.controller.cancelled
    ) {
      resetPlaybackBaseline(activeEntry.controller, null);
      applySnapshot(activeRunKey, { loading: true, error: null, errorSource: null });
      void runLifecycle(activeEntry.controller, activeRunKey, preview.ticket);
      return;
    }
    if (
      errorSource === "failed" &&
      cooldownDeadlineMs !== null &&
      Date.now() < cooldownDeadlineMs
    ) {
      setCooldownTickMs(Date.now());
      return;
    }
    triggerManualRebuild();
  };

  return (
    <div
      aria-label="整集 CPU 预览"
      className="bg-card/95 flex h-full w-full flex-col gap-3 overflow-y-auto rounded-2xl border p-4 backdrop-blur-md"
    >
      <div className="flex flex-wrap items-center justify-between gap-2 border-b pb-2.5">
        <div className="flex items-center gap-2">
          <Film className="text-primary size-4" />
          <span className="text-sm font-semibold">第 {no} 集 CPU 预览</span>
          {manifest && (
            <span className="bg-muted text-muted-foreground rounded px-1.5 py-0.5 font-mono text-[11px]">
              v{manifest.segmentVersion} · #{manifest.manifestHash.slice(0, 8)} ·{" "}
              {manifest.manifest?.totalFrames ?? 0}f
            </span>
          )}
        </div>
        <div className="flex items-center gap-1.5">
          <div className="flex items-center gap-1" role="group" aria-label="预览版本">
            {COMPOSE_VARIANTS.map((item) => (
              <button
                key={item}
                type="button"
                aria-pressed={variant === item}
                onClick={() => setVariant(item)}
                className={cn(
                  "rounded-md border px-2 py-1 text-xs transition-colors",
                  variant === item
                    ? "border-primary bg-primary/15 text-primary font-medium"
                    : "border-border text-muted-foreground hover:text-foreground",
                )}
              >
                {COMPOSE_VARIANT_LABEL[item]}
              </button>
            ))}
          </div>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={loading}
            onClick={handleManualRefresh}
          >
            {loading ? <Loader2 className="animate-spin" /> : <RefreshCw />}
            刷新预览
          </Button>
          <Button type="button" variant="ghost" size="sm" onClick={onClose}>
            返回单镜
          </Button>
        </div>
      </div>

      {staleNotice && (
        <div
          role="status"
          className="border-amber-500/40 bg-amber-500/10 rounded-lg border px-3 py-2 text-xs text-amber-900 dark:text-amber-200"
        >
          {preview?.status === "failed" && preview.errorCode === DRAMA_ERR.manifestStale
            ? `分镜清单版本已变更（${DRAMA_ERR.manifestStale}），请刷新预览。`
            : error
              ? `分镜清单版本已变更（${DRAMA_ERR.manifestStale}），已拉取最新清单。`
              : `分镜清单版本已变更（${DRAMA_ERR.manifestStale}），已自动拉取最新清单重新构建预览。`}
        </div>
      )}

      {manifest && manifest.skippedSeqs.length > 0 && (
        <div
          role="status"
          className="border-amber-500/40 bg-amber-500/10 flex items-center gap-2 rounded-lg border px-3 py-2 text-xs text-amber-900 dark:text-amber-200"
        >
          <TriangleAlert className="size-3.5 shrink-0 text-amber-500" />
          <span>{skippedText(manifest.skippedSeqs)} 暂无采用视频，预览中已跳过。</span>
        </div>
      )}

      {useV2 && (
        <form
          aria-label="预览帧范围"
          className="flex flex-wrap items-end gap-1.5"
          onSubmit={(event) => {
            event.preventDefault();
            if (!inText.trim() || !outText.trim()) return;
            setRange({ inF: Number(inText), outF: Number(outText) });
          }}
        >
          <Button
            type="button"
            variant={range === null ? "secondary" : "outline"}
            size="sm"
            onClick={() => setRange(null)}
          >
            整集
          </Button>
          <Button
            type="button"
            variant={range === "segment" ? "secondary" : "outline"}
            size="sm"
            disabled={!selectedSegmentId}
            onClick={() => setRange("segment")}
          >
            当前镜头
          </Button>
          <label className="text-muted-foreground grid gap-0.5 text-[11px]">
            入点帧
            <input
              className="border-input w-20 rounded border px-1 py-1"
              type="number"
              step={1}
              value={inText}
              onChange={(event) => setInText(event.target.value)}
            />
          </label>
          <label className="text-muted-foreground grid gap-0.5 text-[11px]">
            出点帧（不含）
            <input
              className="border-input w-20 rounded border px-1 py-1"
              type="number"
              step={1}
              value={outText}
              onChange={(event) => setOutText(event.target.value)}
            />
          </label>
          <Button
            type="submit"
            variant={range && range !== "segment" ? "secondary" : "outline"}
            size="sm"
            disabled={!outText.trim()}
          >
            预览帧范围
          </Button>
        </form>
      )}

      {programDiagnostics.length > 0 && (
        <div
          role="alert"
          className="border-status-warning/30 bg-status-warning/10 rounded-lg border p-2 text-xs"
        >
          <div className="text-status-warning font-medium">节目单诊断</div>
          <ul className="mt-1 list-disc pl-4">
            {programDiagnostics.map((issue, idx) => (
              <li key={`${issue.cue_id ?? issue.segment_id}:${issue.reason}:${idx}`}>
                {programDiagnosticText(issue)}
                <span className="text-muted-foreground ml-1">
                  {[
                    issue.cue_id && `条目 ${issue.cue_id}`,
                    issue.segment_id && `分镜 ${issue.segment_id}`,
                    `${issue.field} · ${issue.reason}`,
                  ]
                    .filter(Boolean)
                    .join(" · ")}
                </span>
              </li>
            ))}
          </ul>
        </div>
      )}

      {manifest && manifest.diagnostics.length > 0 && (
        <div className="bg-muted/60 rounded-lg border px-3 py-2 text-xs">
          <div className="mb-1 font-medium">清单诊断信息</div>
          <ul className="text-muted-foreground list-disc space-y-0.5 pl-4">
            {manifest.diagnostics.map((item, idx) => (
              <li key={`${idx}:${item}`}>{item}</li>
            ))}
          </ul>
        </div>
      )}

      <div className="relative flex min-h-0 flex-1 items-center justify-center">
        {error ? (
          <div
            role="alert"
            className="flex flex-col items-center gap-2 text-center text-xs text-destructive"
          >
            <span>{error}</span>
            {inFailedCooldown && (
              <span className="text-muted-foreground max-w-sm text-[11px]">
                同版本失败票据缓存中，约 {cooldownSec}{" "}
                秒后可重试（切换成片包装版/纯净版或修改片段后可立即重试）。
              </span>
            )}
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={inFailedCooldown}
              onClick={handleRetry}
            >
              {inFailedCooldown ? `重试预览（${cooldownSec}s）` : "重试预览"}
            </Button>
          </div>
        ) : preview?.status === "ready" && preview.streamUrl ? (
          <div
            style={{ aspectRatio: aspectOf(ratio) }}
            className="relative h-full max-h-[560px] max-w-full overflow-hidden rounded-xl bg-black ring-1 ring-white/10"
          >
            <video
              key={`${preview.ticket}:${streamReloadNonce}`}
              ref={videoRef}
              src={preview.streamUrl}
              controls
              playsInline
              preload="auto"
              onLoadedMetadata={handleVideoLoadedMetadata}
              onPlaying={handleVideoPlaying}
              onTimeUpdate={handleVideoTimeUpdate}
              onPause={handleVideoInterrupted}
              onWaiting={handleVideoInterrupted}
              onSeeking={handleVideoSeeking}
              onSeeked={handleVideoSeeked}
              onRateChange={handleVideoRateChange}
              onEnded={handleVideoEnded}
              onError={() => void handleVideoError()}
              className="size-full object-contain"
            />
          </div>
        ) : (
          <div className="text-muted-foreground flex flex-col items-center gap-2 text-xs">
            <Loader2 className="size-5 animate-spin" />
            <span>
              {preview?.status === "pending" ? "正在合成 CPU 预览流…" : "正在生成预览清单…"}
            </span>
          </div>
        )}
      </div>
    </div>
  );
}

/**
 * 中间的预览：这个片段生成过结果时是版本轮播（正中是采用的那一版，滑到哪版就采用哪版）；
 * 连播时换成双缓冲的大播放器显示当前片段。成片在「成片」页看，不在这里。
 * 连播用的两个 video 一直挂着（轮播时只是藏起来），否则开始连播时拿不到它们。
 */
export function Preview() {
  const {
    drama,
    episode,
    no,
    selected,
    actions,
    submitting,
    segments,
    trimDrawer,
    program,
    assets,
    refresh: refreshStudio,
  } = useStudio();
  const playback = usePlaybackContext();
  const settledCount = useDramaJobsStore((state) => state.settled[drama.id] ?? 0);
  const [cpuPreviewOpen, setCpuPreviewOpen] = useState(false);
  const [localTrimTarget, setLocalTrimTarget] = useState<{
    segmentId: string;
    pendingTaskId?: string | null;
  } | null>(null);
  const [trimConflict, setTrimConflict] = useState<string | null>(null);

  const epKey = segmentKey(drama.id, no);
  const [staleSegments, setStaleSegments] = useState<{
    key: string;
    base: readonly Segment[];
    items: Segment[];
  } | null>(null);
  const effectiveSegments =
    staleSegments && staleSegments.key === epKey && staleSegments.base === segments
      ? staleSegments.items
      : segments;

  const stageC = usesProgramFeatures(effectiveSegments, program?.savedDocument?.audio?.length);
  const useV2 = drama.ratio === "16:9" && stageC;
  const blockedPreview =
    stageC && !useV2
      ? VISUAL_RATIO_MESSAGE
      : !useV2 && program && !program.savedDocument && !program.error
        ? "正在读取节目单"
        : null;
  const activeTrimTarget = trimDrawer ? trimDrawer.target : localTrimTarget;
  const openTrim = (segment: Segment, pendingTaskId?: string | null) => {
    if (!isVideoVisual(segment)) return;
    setTrimConflict(null);
    if (trimDrawer) trimDrawer.open(segment.id, pendingTaskId ?? null);
    else setLocalTrimTarget({ segmentId: segment.id, pendingTaskId: pendingTaskId ?? null });
  };
  const closeTrim = () => {
    setTrimConflict(null);
    if (trimDrawer) trimDrawer.close();
    else setLocalTrimTarget(null);
  };

  const trimSegment = activeTrimTarget
    ? (effectiveSegments.find(
        (item) => item.id === activeTrimTarget.segmentId && isVideoVisual(item),
      ) ?? null)
    : null;

  const handleSaveTrim = async (
    input: Omit<UpdateSegmentInput, "revision">,
    options?: { expectedRevision?: number },
  ) => {
    if (!trimSegment) return;
    setTrimConflict(null);
    try {
      await actions.patch(trimSegment.id, input, options);
    } catch (err) {
      if (isRevisionConflict(err) || isDramaError(err, DRAMA_ERR.jobBusy)) {
        const msg = dramaErrorMessage(err, "这个片段在别处被修改过，已为你加载最新内容");
        setTrimConflict(msg);
        throw new Error(msg);
      }
      if (isEditInvalidError(err)) {
        throw new Error(dramaErrorMessage(err, "分镜剪辑参数不合法"));
      }
      throw err;
    }
  };

  // 连播时预览跟着当前片段（选中也会跟过去，这里直接按连播的当前片段取，不等选中更新）
  const current = segments.find((item) => item.id === playback.currentId) ?? selected;
  const take = selected && isVideoVisual(selected) ? playableTake(selected) : null;
  const shownTake =
    playback.active && current && isVideoVisual(current) ? playableTake(current) : take;
  const running = !!selected && (selected.status === "running" || submitting.has(selected.id));
  const carousel =
    !playback.active &&
    !cpuPreviewOpen &&
    !!selected &&
    isVideoVisual(selected) &&
    selected.takes.length > 0;
  const canTrim = !!selected && !!take && !playback.active;
  const selectedTrimmed =
    !!selected && isVideoVisual(selected) && hasNonDefaultSegmentEdit(segmentEditOf(selected));
  const stillSrc =
    selected?.visualKind === "still" && typeof selected.stillAssetId === "number"
      ? assets.find(
          (asset) =>
            asset.kind === "image" &&
            asset.assetId === (selected.stillAssetIdText ?? String(selected.stillAssetId)),
        )?.src
      : null;
  const invalidationKey = `${drama.id}:${no}:${drama.ratio}:${episode?.segmentVersion ?? 0}:${settledCount}:${effectiveSegments.map((s) => `${s.id}:${s.revision}:${s.visualKind ?? "video"}:${s.stillAssetIdText ?? s.stillAssetId ?? ""}:${s.chosenTaskId}`).join("|")}`;

  return (
    <section
      aria-label="预览"
      className="relative flex h-full w-full min-w-0 flex-col items-center gap-3 overflow-hidden px-5 py-4"
    >
      <Ambient src={shownTake?.asset?.url ?? null} />
      <div className="relative z-10 flex w-full items-center justify-between gap-2">
        <div className="flex items-center gap-2 text-xs">
          {selected && (
            <span className="bg-background/70 text-foreground rounded-md border px-2 py-1 font-mono backdrop-blur-xs">
              镜头 {String(selected.seq + 1).padStart(2, "0")}
            </span>
          )}
          {selectedTrimmed && (
            <span className="bg-primary/15 text-primary rounded-md px-2 py-0.5 font-medium">
              已精裁
            </span>
          )}
        </div>
        <div className="flex items-center gap-1.5">
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={!canTrim}
            title={canTrim ? "精裁入出点与原声" : "先生成并采用视频后才可修剪"}
            onClick={() => selected && openTrim(selected)}
          >
            <Scissors />
            修剪
          </Button>
          <Button
            type="button"
            variant={cpuPreviewOpen ? "default" : "outline"}
            size="sm"
            disabled={playback.active || (!cpuPreviewOpen && !!blockedPreview)}
            aria-pressed={cpuPreviewOpen}
            title={blockedPreview ?? "拉取冻结清单并在服务端生成整集 CPU 预览流"}
            onClick={() => setCpuPreviewOpen((prev) => !prev)}
          >
            <Film />
            CPU 预览
          </Button>
        </div>
      </div>

      <div className="relative z-10 flex min-h-0 w-full flex-1 items-center justify-center">
        {cpuPreviewOpen && (
          <div className="absolute inset-0 z-20">
            <CpuEpisodePreview
              dramaId={drama.id}
              no={no}
              ratio={drama.ratio}
              invalidationKey={invalidationKey}
              useV2={useV2}
              selectedSegmentId={selected?.id}
              onManifestStale={async () => {
                const [, reloaded] = await Promise.all([
                  refreshStudio?.(),
                  reloadSegments(drama.id, no),
                  program?.sync?.(),
                ]);
                if (reloaded) {
                  const next = useDramaSegmentsStore.getState().byEpisode[epKey];
                  if (next) setStaleSegments({ key: epKey, base: segments, items: next });
                }
              }}
              onClose={() => {
                setStaleSegments(null);
                setCpuPreviewOpen(false);
              }}
            />
          </div>
        )}
        {carousel && selected && (
          <div className="absolute inset-0">
            <TakeCarousel
              key={selected.id}
              takes={selected.takes}
              chosenTaskId={selected.chosenTaskId}
              aspect={aspectValue(drama.ratio)}
              onChoose={(taskId) => void actions.choose(selected, taskId)}
              onTrim={(taskId) =>
                openTrim(selected, taskId !== selected.chosenTaskId ? taskId : null)
              }
            />
          </div>
        )}
        <div
          aria-hidden={carousel || cpuPreviewOpen}
          style={{ aspectRatio: aspectOf(drama.ratio) }}
          className={cn(
            "relative grid h-full max-h-[680px] max-w-full place-items-center overflow-hidden rounded-2xl bg-black shadow-[0_40px_80px_-30px_rgb(0_0_0/0.9)] ring-1 ring-white/10",
            (carousel || cpuPreviewOpen) && "invisible",
          )}
        >
          {/* 连播的两个 video 一直挂着（下一段在不可见的那个里预加载），只显示当前画面；无连播时都是透明的 */}
          {([0, 1] as const).map((which) => {
            const slot = playback.slot(which);
            return (
              <video
                key={which}
                ref={slot.ref}
                playsInline
                preload="auto"
                aria-hidden={!(playback.active && slot.visible)}
                onClick={playback.toggle}
                onEnded={slot.onEnded}
                onError={slot.onError}
                onTimeUpdate={slot.onTimeUpdate}
                className={cn(
                  "absolute inset-0 size-full bg-black object-contain",
                  !(playback.active && slot.visible) && "pointer-events-none opacity-0",
                )}
              />
            );
          })}
          {playback.active ? (
            <div className="pointer-events-none absolute inset-x-0 bottom-0 flex justify-between bg-gradient-to-t from-black/70 to-transparent px-3 pt-6 pb-2 font-mono text-[11px] text-white">
              <span>
                片段 {String((current?.seq ?? 0) + 1).padStart(2, "0")}
                <span className="bg-primary/90 text-primary-foreground ml-2 rounded px-1.5 py-px font-sans text-[10px] font-medium">
                  连播{playback.playing ? "中" : "已暂停"}
                </span>
              </span>
              <span>{current?.seconds}s</span>
            </div>
          ) : selected?.visualKind === "black" ? (
            <span className="text-xs text-white/70">黑场</span>
          ) : selected?.visualKind === "still" ? (
            stillSrc ? (
              <img src={stillSrc} alt="静帧" className="size-full object-cover" />
            ) : (
              <span className="text-xs text-white/70">静帧</span>
            )
          ) : running ? (
            <TaskGenerationStatus className="size-full" />
          ) : (
            <div className="grid size-full place-items-center bg-[repeating-linear-gradient(135deg,oklch(0.17_0_0)_0_14px,oklch(0.15_0_0)_14px_28px)] text-center text-xs text-white/70">
              <div>
                <span className="mb-1 block font-mono text-4xl font-semibold tracking-tight text-white/20">
                  {String((selected?.seq ?? 0) + 1).padStart(2, "0")}
                </span>
                <Clapperboard className="mx-auto mb-1.5 size-5" />
                还没有生成内容{" "}
              </div>
            </div>
          )}
        </div>
      </div>
      <SegmentTrimDrawer
        open={!!trimSegment}
        onOpenChange={(open) => {
          if (!open) closeTrim();
        }}
        segment={trimSegment}
        pendingTaskId={activeTrimTarget?.pendingTaskId ?? null}
        conflictMessage={trimConflict}
        onSave={handleSaveTrim}
      />
    </section>
  );
}
