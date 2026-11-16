import { useCallback, useEffect, useRef, useState } from "react";
import { toast } from "sonner";

import { submitJob } from "@/api/drama";
import {
  DRAMA_ERR,
  dramaErrorMessage,
  IDEMPOTENCY_CONFLICT_CODE,
  isDramaError,
  isManifestStaleError,
} from "@/api/drama/errors";
import type {
  DramaComposeSize,
  DramaComposeVariant,
  DramaJob,
  DramaManifestView,
  Segment,
  SegmentTake,
} from "@/api/drama/type";
import { useDramaJobsStore } from "@/store/drama";
import {
  buildComposeJobInput,
  composeNotice,
  createComposeRequestKey,
  DEFAULT_COMPOSE_SIZE,
  DEFAULT_COMPOSE_VARIANTS,
  normalizeComposeVariants,
} from "@/utils/drama/compose";
import { waitForEpisodeManifest } from "@/utils/drama/manifest";
import { ApiError } from "@/utils/requests/request";

/**
 * 提交「合成整集」（契约 §14）：服务端 ffmpeg 拼接，不扣积分，进度走 drama.job 推送 + 任务卡。
 * 67010（没有可合成的片段）/ 67004（已有进行中的任务）等错误给出明确文案。
 * 返回 compose(no)：提交成功为 true；pending 是正在提交的集号。
 */

export interface ComposeManifestState {
  data: DramaManifestView | null;
  loading: boolean;
  error: string | null;
  stale: boolean;
}

export type ComposeSegmentIdentity = Pick<
  Segment,
  "id" | "seq" | "chosenTaskId" | "revision" | "seconds"
> & {
  takes?: readonly SegmentTake[];
};

export interface ComposeSubmitOptions {
  useManifest?: boolean;
  size?: DramaComposeSize;
  variants?: readonly DramaComposeVariant[];
  manifest?: DramaManifestView | null;
  /** 项目比例（用于本地意图签名；旧协议同样依赖比例变化轮换幂等键，不上送后端 compose 字段） */
  ratio?: string;
  /** 分集版本号（用于旧协议本地意图签名） */
  segmentVersion?: number;
  /** 片段顺序与采用视频签名列表（用于旧协议本地意图签名） */
  segments?: readonly ComposeSegmentIdentity[];
  /** 已预计算的片段签名字符串（可选，优先于 segments） */
  segmentsKey?: string;
}

const legacySegmentsIdentity = (options?: ComposeSubmitOptions): string => {
  if (options?.segmentsKey !== undefined) return options.segmentsKey;
  if (!options?.segments) return "";
  return options.segments
    .map((s) => {
      const chosen = s.takes?.find((t) => t.taskId === s.chosenTaskId && t.status === "succeeded");
      const assetId = chosen?.asset?.id ?? "";
      return `${s.id}@${s.seq}:${s.chosenTaskId}:${assetId}:${s.revision}:${s.seconds}`;
    })
    .join("|");
};

interface PendingComposeEntry {
  key: string;
  preIntent: string;
  resolvedIntent: string | null;
  knownTerminalJobIds: Set<string>;
}

interface InflightComposeEntry {
  preIntent: string;
  resolvedIntent: string | null;
  promise: Promise<boolean>;
  token: symbol;
}

const TERMINAL_JOB_STATUSES = new Set<DramaJob["status"]>(["succeeded", "failed", "canceled"]);

const scopeKey = (dramaId: string, no: number) => `${dramaId}:${no}`;

const terminalComposeJobIds = (dramaId: string, no: number): Set<string> => {
  const jobs = useDramaJobsStore.getState().byDrama[dramaId] ?? [];
  const ids = new Set<string>();
  for (const job of jobs) {
    if (
      job.step === "compose" &&
      job.episodes.includes(no) &&
      TERMINAL_JOB_STATUSES.has(job.status)
    ) {
      ids.add(job.id);
    }
  }
  return ids;
};

/** 仅网络异常、超时、408/429、5xx 属于可原样重试的瞬时失败；业务拒绝与 40015 应清理幂等键 */
const isTransientComposeError = (error: unknown): boolean => {
  if (isManifestStaleError(error) || isDramaError(error, IDEMPOTENCY_CONFLICT_CODE)) {
    return false;
  }
  if (!(error instanceof ApiError)) return true;
  const status = error.status;
  if (typeof status !== "number" || status === 0) return true;
  return status === 408 || status === 429 || status >= 500;
};

const canReuseEntry = (
  entry: { preIntent: string; resolvedIntent: string | null } | undefined,
  basePreIntent: string,
  callerResolvedIntent: string | null,
): boolean => {
  if (!entry || entry.preIntent !== basePreIntent) return false;
  return (
    callerResolvedIntent === null ||
    entry.resolvedIntent === null ||
    entry.resolvedIntent === callerResolvedIntent
  );
};

const EMPTY_MANIFESTS: Record<number, ComposeManifestState> = {};

interface ManifestStoreState {
  dramaId: string;
  map: Record<number, ComposeManifestState>;
}

const patchManifestMap = (
  prev: ManifestStoreState,
  dramaId: string,
  no: number,
  build: (cur: ComposeManifestState | undefined) => ComposeManifestState,
): ManifestStoreState => {
  const base = prev.dramaId === dramaId ? prev.map : {};
  return {
    dramaId,
    map: {
      ...base,
      [no]: build(base[no]),
    },
  };
};

export function useCompose(dramaId: string) {
  const [pendingState, setPendingState] = useState<{ dramaId: string; no: number | null }>({
    dramaId,
    no: null,
  });
  const [manifestsState, setManifestsState] = useState<ManifestStoreState>({
    dramaId,
    map: {},
  });
  const pending = pendingState.dramaId === dramaId ? pendingState.no : null;
  const manifests = manifestsState.dramaId === dramaId ? manifestsState.map : EMPTY_MANIFESTS;
  const mountedDramaRef = useRef<string | null>(dramaId);
  const manifestReqSeqRef = useRef<Record<string, number>>({});
  const manifestWaitersRef = useRef<Map<string, () => void>>(new Map());
  const manifestAbortRef = useRef<Map<string, AbortController>>(new Map());
  const pendingKeysRef = useRef<Map<string, PendingComposeEntry>>(new Map());
  const inflightRef = useRef<Map<string, InflightComposeEntry>>(new Map());

  const cancelManifestWaiter = useCallback((key: string) => {
    const cancel = manifestWaitersRef.current.get(key);
    if (cancel) {
      manifestWaitersRef.current.delete(key);
      cancel();
    }
  }, []);

  const abortManifestRequest = useCallback((key: string) => {
    const ac = manifestAbortRef.current.get(key);
    if (ac) {
      manifestAbortRef.current.delete(key);
      ac.abort();
    }
  }, []);

  const nextManifestSeq = useCallback(
    (key: string): number => {
      cancelManifestWaiter(key);
      abortManifestRequest(key);
      const seq = (manifestReqSeqRef.current[key] ?? 0) + 1;
      manifestReqSeqRef.current[key] = seq;
      return seq;
    },
    [abortManifestRequest, cancelManifestWaiter],
  );

  const startManifestWait = useCallback(
    (key: string) => {
      const seq = nextManifestSeq(key);
      const ac = new AbortController();
      manifestAbortRef.current.set(key, ac);
      return {
        seq,
        signal: ac.signal,
        cleanup: () => {
          if (manifestAbortRef.current.get(key) === ac) {
            manifestAbortRef.current.delete(key);
          }
        },
      };
    },
    [nextManifestSeq],
  );

  const sleepForManifestKey = useCallback(
    (key: string, seq: number, ms: number): Promise<boolean> => {
      if (mountedDramaRef.current !== dramaId || manifestReqSeqRef.current[key] !== seq) {
        return Promise.resolve(false);
      }
      cancelManifestWaiter(key);
      return new Promise<boolean>((resolve) => {
        let settled = false;
        const finish = (awake: boolean) => {
          if (settled) return;
          settled = true;
          clearTimeout(timer);
          manifestWaitersRef.current.delete(key);
          resolve(
            awake && mountedDramaRef.current === dramaId && manifestReqSeqRef.current[key] === seq,
          );
        };
        const timer = setTimeout(() => finish(true), ms);
        manifestWaitersRef.current.set(key, () => finish(false));
      });
    },
    [cancelManifestWaiter, dramaId],
  );

  useEffect(() => {
    mountedDramaRef.current = dramaId;
    const waiters = manifestWaitersRef.current;
    const aborters = manifestAbortRef.current;
    return () => {
      mountedDramaRef.current = null;
      for (const cancel of Array.from(waiters.values())) {
        cancel();
      }
      waiters.clear();
      for (const ac of Array.from(aborters.values())) {
        ac.abort();
      }
      aborters.clear();
    };
  }, [dramaId]);

  useEffect(() => {
    return useDramaJobsStore.subscribe((state) => {
      const jobs = state.byDrama[dramaId] ?? [];
      if (jobs.length === 0 || pendingKeysRef.current.size === 0) return;
      for (const [key, entry] of pendingKeysRef.current.entries()) {
        if (!key.startsWith(`${dramaId}:`)) continue;
        const no = Number(key.slice(dramaId.length + 1));
        const hasNewTerminalComposeJob = jobs.some(
          (job) =>
            job.step === "compose" &&
            job.episodes.includes(no) &&
            TERMINAL_JOB_STATUSES.has(job.status) &&
            !entry.knownTerminalJobIds.has(job.id),
        );
        if (hasNewTerminalComposeJob) {
          pendingKeysRef.current.delete(key);
        }
      }
    });
  }, [dramaId]);

  const loadManifest = useCallback(
    async (no: number): Promise<DramaManifestView | null> => {
      const key = scopeKey(dramaId, no);
      const { seq, signal, cleanup } = startManifestWait(key);
      setManifestsState((prev) =>
        patchManifestMap(prev, dramaId, no, (cur) => ({
          data: cur?.data ?? null,
          loading: true,
          error: null,
          stale: cur?.stale ?? false,
        })),
      );
      try {
        const res = await waitForEpisodeManifest(dramaId, no, {
          signal,
          isCancelled: () =>
            mountedDramaRef.current !== dramaId || manifestReqSeqRef.current[key] !== seq,
          sleep: (ms) => sleepForManifestKey(key, seq, ms),
        });
        if (
          res.kind === "cancelled" ||
          mountedDramaRef.current !== dramaId ||
          manifestReqSeqRef.current[key] !== seq
        ) {
          return null;
        }
        if (res.kind === "timeout") {
          setManifestsState((prev) =>
            patchManifestMap(prev, dramaId, no, (cur) => ({
              data: cur?.data ?? null,
              loading: false,
              error: "分镜清单准备超时，请稍后重试",
              stale: false,
            })),
          );
          return null;
        }
        const data = res.manifest;
        setManifestsState((prev) =>
          patchManifestMap(prev, dramaId, no, () => ({
            data,
            loading: false,
            error: null,
            stale: false,
          })),
        );
        return data;
      } catch (e) {
        if (mountedDramaRef.current !== dramaId || manifestReqSeqRef.current[key] !== seq) {
          return null;
        }
        const empty = isDramaError(e, DRAMA_ERR.nothingToCompose);
        const message = empty ? null : dramaErrorMessage(e, "读取合成清单失败，请稍后重试");
        setManifestsState((prev) =>
          patchManifestMap(prev, dramaId, no, (cur) => ({
            data: empty ? null : (cur?.data ?? null),
            loading: false,
            error: message,
            stale: false,
          })),
        );
        return null;
      } finally {
        cleanup();
      }
    },
    [dramaId, sleepForManifestKey, startManifestWait],
  );

  const compose = useCallback(
    async (no: number, options?: ComposeSubmitOptions): Promise<boolean> => {
      const key = scopeKey(dramaId, no);
      const useManifest = options?.useManifest ?? options?.manifest !== undefined;
      const size = options?.size ?? DEFAULT_COMPOSE_SIZE;
      const variants = useManifest
        ? normalizeComposeVariants(options?.variants ?? DEFAULT_COMPOSE_VARIANTS)
        : [];
      const ratio = options?.ratio ?? "";
      const segmentVersion = options?.segmentVersion ?? 0;
      const legacySegSig = useManifest ? "" : legacySegmentsIdentity(options);
      const basePreIntent = useManifest
        ? `manifest:${ratio}:${size}:${variants.join(",")}`
        : `legacy:${ratio}:${segmentVersion}:${legacySegSig}`;
      const callerResolvedIntent =
        useManifest && options?.manifest
          ? `${basePreIntent}:${options.manifest.segmentVersion}:${options.manifest.manifestHash}`
          : useManifest
            ? null
            : basePreIntent;

      const existingInflight = inflightRef.current.get(key);
      if (canReuseEntry(existingInflight, basePreIntent, callerResolvedIntent)) {
        return existingInflight!.promise;
      }

      const existingPending = pendingKeysRef.current.get(key);
      let pendingEntry: PendingComposeEntry = canReuseEntry(
        existingPending,
        basePreIntent,
        callerResolvedIntent,
      )
        ? existingPending!
        : {
            key: createComposeRequestKey(),
            preIntent: basePreIntent,
            resolvedIntent: callerResolvedIntent,
            knownTerminalJobIds: terminalComposeJobIds(dramaId, no),
          };
      pendingKeysRef.current.set(key, pendingEntry);

      const token = Symbol("compose-submit");
      if (mountedDramaRef.current === dramaId) {
        setPendingState({ dramaId, no });
      }

      const run = (async (): Promise<boolean> => {
        try {
          if (!useManifest) {
            const input = buildComposeJobInput({
              episodeNo: no,
              composeRequestKey: pendingEntry.key,
            });
            const job = await submitJob(dramaId, input);
            useDramaJobsStore.getState().applyJob(dramaId, job);
            if (pendingKeysRef.current.get(key) === pendingEntry) {
              pendingKeysRef.current.delete(key);
            }
            return true;
          }

          let manifest = options?.manifest ?? null;
          let submitManifestSeq = manifestReqSeqRef.current[key] ?? 0;
          if (!manifest || manifest.status === "preparing") {
            const { seq, signal, cleanup } = startManifestWait(key);
            submitManifestSeq = seq;
            let res: Awaited<ReturnType<typeof waitForEpisodeManifest>>;
            try {
              res = await waitForEpisodeManifest(dramaId, no, {
                signal,
                isCancelled: () =>
                  mountedDramaRef.current !== dramaId || manifestReqSeqRef.current[key] !== seq,
                sleep: (ms) => sleepForManifestKey(key, seq, ms),
              });
            } finally {
              cleanup();
            }
            if (
              res.kind === "cancelled" ||
              mountedDramaRef.current !== dramaId ||
              manifestReqSeqRef.current[key] !== seq
            ) {
              if (pendingKeysRef.current.get(key) === pendingEntry) {
                pendingKeysRef.current.delete(key);
              }
              return false;
            }
            if (res.kind === "timeout") {
              if (pendingKeysRef.current.get(key) === pendingEntry) {
                pendingKeysRef.current.delete(key);
              }
              setManifestsState((prev) =>
                patchManifestMap(prev, dramaId, no, (cur) => ({
                  data: cur?.data ?? null,
                  loading: false,
                  error: "分镜清单准备超时，请稍后重试",
                  stale: false,
                })),
              );
              toast.error("分镜清单准备超时，请稍后重试");
              return false;
            }
            const loaded = res.manifest;
            setManifestsState((prev) =>
              patchManifestMap(prev, dramaId, no, () => ({
                data: loaded,
                loading: false,
                error: null,
                stale: false,
              })),
            );
            manifest = loaded;
          }

          const actualResolvedIntent = `${basePreIntent}:${manifest.segmentVersion}:${manifest.manifestHash}`;
          if (
            pendingEntry.resolvedIntent !== null &&
            pendingEntry.resolvedIntent !== actualResolvedIntent
          ) {
            pendingEntry = {
              key: createComposeRequestKey(),
              preIntent: basePreIntent,
              resolvedIntent: actualResolvedIntent,
              knownTerminalJobIds: terminalComposeJobIds(dramaId, no),
            };
          } else {
            pendingEntry.resolvedIntent = actualResolvedIntent;
          }
          if (inflightRef.current.get(key)?.token === token) {
            pendingKeysRef.current.set(key, pendingEntry);
            const activeInflight = inflightRef.current.get(key);
            if (activeInflight) activeInflight.resolvedIntent = actualResolvedIntent;
          }

          const input = buildComposeJobInput({
            episodeNo: no,
            manifest,
            size,
            variants,
            composeRequestKey: pendingEntry.key,
          });
          const job = await submitJob(dramaId, input);
          useDramaJobsStore.getState().applyJob(dramaId, job);
          if (pendingKeysRef.current.get(key) === pendingEntry) {
            pendingKeysRef.current.delete(key);
          }
          if (
            mountedDramaRef.current === dramaId &&
            (manifestReqSeqRef.current[key] ?? 0) === submitManifestSeq
          ) {
            setManifestsState((prev) =>
              patchManifestMap(prev, dramaId, no, (cur) => {
                if (
                  cur?.data &&
                  (cur.data.segmentVersion > manifest.segmentVersion ||
                    (cur.data.segmentVersion === manifest.segmentVersion &&
                      cur.data.manifestHash !== manifest.manifestHash))
                ) {
                  return cur;
                }
                return {
                  data: manifest,
                  loading: false,
                  error: null,
                  stale: false,
                };
              }),
            );
          }
          return true;
        } catch (e) {
          if (isManifestStaleError(e)) {
            if (pendingKeysRef.current.get(key) === pendingEntry) {
              pendingKeysRef.current.delete(key);
            }
            await loadManifest(no);
            if (mountedDramaRef.current === dramaId) {
              setManifestsState((prev) =>
                patchManifestMap(prev, dramaId, no, (cur) => ({
                  data: cur?.data ?? null,
                  loading: false,
                  error: cur?.error ?? null,
                  stale: true,
                })),
              );
            }
            toast.warning("分镜清单已更新，已为你刷新最新版本，请再次点击导出");
          } else {
            if (!isTransientComposeError(e) && pendingKeysRef.current.get(key) === pendingEntry) {
              pendingKeysRef.current.delete(key);
            }
            toast.error(dramaErrorMessage(e, "合成提交失败，请稍后重试"));
          }
          return false;
        } finally {
          if (inflightRef.current.get(key)?.token === token) {
            inflightRef.current.delete(key);
            if (mountedDramaRef.current === dramaId) {
              setPendingState((prev) =>
                prev.dramaId === dramaId && prev.no === no ? { dramaId, no: null } : prev,
              );
            }
          }
        }
      })();

      inflightRef.current.set(key, {
        preIntent: basePreIntent,
        resolvedIntent: callerResolvedIntent,
        promise: run,
        token,
      });

      return run;
    },
    [dramaId, loadManifest, sleepForManifestKey, startManifestWait],
  );
  return { compose, pending, manifests, loadManifest };
}

/**
 * 合成结束时的提示：成功且跳过了没有采用视频的片段，就提示跳过了哪几个。
 * 只提示这次页面打开之后才结束的任务（进入时已经成功的旧任务不重复提示）。
 */
export function useComposeNotices(dramaId: string) {
  useEffect(() => {
    const seen = new Set(
      (useDramaJobsStore.getState().byDrama[dramaId] ?? [])
        .filter((job) => job.status === "succeeded")
        .map((job) => job.id),
    );
    return useDramaJobsStore.subscribe((state) => {
      for (const job of state.byDrama[dramaId] ?? []) {
        if (job.status !== "succeeded" || seen.has(job.id)) continue;
        seen.add(job.id);
        const message = composeNotice(job);
        if (message) toast.warning(message);
      }
    });
  }, [dramaId]);
}
