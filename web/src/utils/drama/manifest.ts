import { getEpisodeManifest } from "@/api/drama";
import type { DramaManifestView } from "@/api/drama/type";

export const MANIFEST_POLL_BASE_MS = 200;
export const MANIFEST_POLL_MAX_MS = 2_000;
export const MANIFEST_PREPARE_MAX_WAIT_MS = 120_000;
export const MANIFEST_PREPARE_MAX_ATTEMPTS = 80;

export type WaitManifestOutcome =
  | { kind: "ready"; manifest: DramaManifestView }
  | { kind: "timeout" }
  | { kind: "cancelled" };

export interface WaitManifestOptions {
  signal?: AbortSignal;
  isCancelled?: () => boolean;
  sleep?: (ms: number) => Promise<boolean>;
  baseDelayMs?: number;
  maxDelayMs?: number;
  maxWaitMs?: number;
  maxAttempts?: number;
  nowMs?: () => number;
}

const MAX_TIMER_MS = 2_147_483_647;

const defaultSleep = (ms: number, signal?: AbortSignal): Promise<boolean> => {
  if (signal?.aborted) return Promise.resolve(false);
  return new Promise((resolve) => {
    let onAbort: (() => void) | null = null;
    const timer = setTimeout(() => {
      if (onAbort) signal?.removeEventListener("abort", onAbort);
      resolve(true);
    }, ms);
    if (signal) {
      onAbort = () => {
        clearTimeout(timer);
        resolve(false);
      };
      signal.addEventListener("abort", onAbort, { once: true });
    }
  });
};

const defaultNowMs = (): number =>
  typeof performance !== "undefined" && typeof performance.now === "function"
    ? performance.now()
    : Date.now();

type StepOutcome<T> = { kind: "ok"; value: T } | { kind: "timeout" } | { kind: "cancelled" };

async function raceWithinDeadline<T>(
  work: (reqController: AbortController) => Promise<T>,
  deadlineMs: number,
  nowMs: () => number,
  aborted: () => boolean,
  signal?: AbortSignal,
): Promise<StepOutcome<T>> {
  if (aborted()) return { kind: "cancelled" };
  if (nowMs() >= deadlineMs) return { kind: "timeout" };

  const reqController = new AbortController();
  let timedOut = false;
  let timer: ReturnType<typeof setTimeout> | null = null;
  let onAbort: (() => void) | null = null;

  try {
    const workPromise = work(reqController);
    // 防止取消或超时竞态胜出后，底层任务晚到拒绝触发未捕获异常
    workPromise.catch(() => {});

    const stopPromise = new Promise<{ kind: "cancelled" | "timeout" }>((resolve) => {
      onAbort = () => {
        reqController.abort();
        resolve({ kind: "cancelled" });
      };
      if (signal) {
        if (signal.aborted) {
          onAbort();
          return;
        }
        signal.addEventListener("abort", onAbort, { once: true });
      }
      const armTimer = () => {
        const leftMs = deadlineMs - nowMs();
        if (!Number.isFinite(leftMs)) return;
        if (leftMs <= 0) {
          timedOut = true;
          reqController.abort();
          resolve({ kind: "timeout" });
          return;
        }
        timer = setTimeout(
          () => {
            if (leftMs > MAX_TIMER_MS && nowMs() < deadlineMs) {
              armTimer();
              return;
            }
            timedOut = true;
            reqController.abort();
            resolve({ kind: "timeout" });
          },
          Math.min(leftMs, MAX_TIMER_MS),
        );
      };
      armTimer();
    });

    const raced = await Promise.race([
      workPromise.then((value) => ({ kind: "ok" as const, value })),
      stopPromise,
    ]);
    if (raced.kind !== "ok") {
      return raced.kind === "cancelled" || aborted() ? { kind: "cancelled" } : { kind: "timeout" };
    }
    if (aborted()) return { kind: "cancelled" };
    if (nowMs() > deadlineMs) return { kind: "timeout" };
    return { kind: "ok", value: raced.value };
  } catch (error) {
    if (aborted()) return { kind: "cancelled" };
    if (timedOut || nowMs() >= deadlineMs) return { kind: "timeout" };
    throw error;
  } finally {
    if (timer !== null) clearTimeout(timer);
    if (onAbort) signal?.removeEventListener("abort", onAbort);
  }
}

/**
 * 读取分集冻结清单；当后端返回 202 (`status: "preparing"`) 表示冷媒体正在后台探测时，
 * 按有界指数退避轮询直至就绪、超时或调用方取消。
 */
export async function waitForEpisodeManifest(
  dramaId: string,
  episodeNo: number,
  options: WaitManifestOptions = {},
): Promise<WaitManifestOutcome> {
  const {
    signal,
    isCancelled,
    sleep,
    baseDelayMs = MANIFEST_POLL_BASE_MS,
    maxDelayMs = MANIFEST_POLL_MAX_MS,
    maxWaitMs = MANIFEST_PREPARE_MAX_WAIT_MS,
    maxAttempts = MANIFEST_PREPARE_MAX_ATTEMPTS,
    nowMs = defaultNowMs,
  } = options;

  const aborted = () => Boolean(signal?.aborted || isCancelled?.());
  const startedAtMs = nowMs();
  const deadlineMs = startedAtMs + maxWaitMs;
  let attempt = 0;

  for (;;) {
    const step = await raceWithinDeadline(
      (reqController) =>
        getEpisodeManifest(dramaId, episodeNo, {
          signal: reqController.signal,
        }),
      deadlineMs,
      nowMs,
      aborted,
      signal,
    );
    if (step.kind !== "ok") return step;

    const manifest = step.value;
    if (manifest.status !== "preparing") {
      return { kind: "ready", manifest };
    }

    attempt += 1;
    const remainingBeforeSleepMs = deadlineMs - nowMs();
    if (attempt >= maxAttempts || remainingBeforeSleepMs <= 0) {
      return { kind: "timeout" };
    }
    const delayMs = Math.min(maxDelayMs, baseDelayMs * 2 ** (attempt - 1), remainingBeforeSleepMs);
    const sleepStep = await raceWithinDeadline(
      (reqController) => (sleep ? sleep(delayMs) : defaultSleep(delayMs, reqController.signal)),
      deadlineMs,
      nowMs,
      aborted,
      signal,
    );
    if (sleepStep.kind !== "ok") return sleepStep;
    if (!sleepStep.value || aborted()) return { kind: "cancelled" };
    if (nowMs() >= deadlineMs) return { kind: "timeout" };
  }
}
