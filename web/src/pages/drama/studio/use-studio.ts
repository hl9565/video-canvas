import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { toast } from "sonner";

import {
  adaptSegment,
  addSegment,
  deleteSegment,
  quoteSegment,
  reorderSegments,
  updateSegment,
} from "@/api/drama";
import {
  DRAMA_ERR,
  dramaErrorMessage,
  isDramaError,
  isEditInvalidError,
  isQuoteExpiredError,
  isRevisionConflict,
} from "@/api/drama/errors";
import type {
  Drama,
  EpisodeSummary,
  Segment,
  TaskQuote,
  UpdateSegmentInput,
} from "@/api/drama/type";
import { confirm } from "@/components/admin-ui/confirm-dialog";
import { choose as chooseOption } from "@/components/drama/choice-dialog";
import { useTextHashes } from "@/hooks/use-text-hashes";
import { useDramaAssets, type DramaAssetsState } from "@/hooks/use-drama-assets";
import { useDramaVideoModel } from "@/hooks/use-drama-video-model";
import { useCreditsStore } from "@/store/credits";
import { useDramaJobsStore } from "@/store/drama";
import { segmentKey, useDramaSegmentsStore } from "@/store/drama-segments";
import { overridesForModel, useStudioPrefs, useStudioPrefsStore } from "@/store/drama-studio";
import { hasNonDefaultSegmentEdit, segmentEditOf } from "@/utils/drama/segment-trim";
import {
  adaptationDurationMismatch,
  adaptState,
  evaluateSeedanceBatch,
  formatLabel,
  segmentNos,
  sha256Hex,
  splitBatch,
  submitText,
  targetFormat,
  type AdaptState,
  type TargetFormat,
} from "@/utils/drama/adaptation";
import { reloadSegments } from "@/utils/drama/segment-events";
import type { RefAsset } from "@/utils/drama/segment-assets";
import { planSegmentInput, type SegmentPlan } from "@/utils/drama/segment-input";
import {
  formatTimelineSeconds,
  timelineOverrun,
  timelineOverrunMessage,
} from "@/utils/drama/timeline";
import { effectiveDuration } from "@/utils/drama/video-models";
import {
  applyOrder,
  batchTargets,
  chooseCheck,
  isRunning,
  orderIds,
  sameOrder,
} from "@/utils/drama/segments";
import { describeSubmitError } from "@/utils/tasks/submit";
import { submitSegmentTask } from "@/utils/tasks/gateway";
import { priceSpecOf } from "@/utils/tasks/capabilities";
import { quote } from "@/utils/pricing/quote";
import { ApiError } from "@/utils/requests/request";

import { DramaWorkspaceContext } from "../workspace/context";
import { summarizeSave, type DramaWorkspace, type SaveSummary } from "../workspace/use-workspace";
import type { AutosaveStatus } from "@/utils/drama/autosave";

const NO_SEGMENTS: Segment[] = [];
/** 有片段在生成时多久对账一次：WebSocket 是主通道，这里兜底 */
const POLL_MS = 8000;
/** 新增片段的默认时长（秒），不超过模型上限 */
const DEFAULT_SECONDS = 10;

function canonicalJson(value: unknown): string {
  if (value === null || typeof value !== "object") return JSON.stringify(value);
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  const entries = Object.entries(value as Record<string, unknown>)
    .filter(([, v]) => v !== undefined)
    .sort(([a], [b]) => a.localeCompare(b));
  return `{${entries.map(([k, v]) => `${JSON.stringify(k)}:${canonicalJson(v)}`).join(",")}}`;
}

function sameQuoteTerms(a: TaskQuote, b: TaskQuote): boolean {
  return (
    a.credits === b.credits &&
    a.priceFingerprint === b.priceFingerprint &&
    a.rawSize === b.rawSize &&
    canonicalJson(a.effectiveInput) === canonicalJson(b.effectiveInput)
  );
}

/** 片段工作区的全部状态与动作；各区域组件从 context 取 */
export type Studio = {
  drama: Drama;
  episode: EpisodeSummary;
  no: number;
  segments: Segment[];
  segmentsStatus: "loading" | "ready" | "error";
  selected: Segment | null;
  select: (id: string) => void;
  assets: RefAsset[];
  assetsStatus: DramaAssetsState["status"];
  video: ReturnType<typeof useDramaVideoModel>;
  /** 顶栏选的生成参数（分辨率等） */
  overrides: Record<string, unknown>;
  setOverride: (name: string, value: unknown) => void;
  /**
   * 选中片段此刻要提交的文字（还没存下来的也算）：目标格式是 Seedance 时是原稿，
   * 其他格式有转写稿时是转写稿、没有时是原稿；参考素材条和报价跟着它变
   */
  prompt: string;
  /** 未存盘的草稿内容（键为 segmentId:source 或 segmentId:format），供编辑与时间轴实时校验共用 */
  drafts?: Readonly<Record<string, string>>;
  /** 记下编辑中的文字；kind 默认原稿，转写稿传格式 */
  setDraft: (id: string, text: string, kind?: DraftKind) => void;
  /** 顶栏视频模型要的提示词格式（按后台配置的 prompt_format） */
  format: TargetFormat;
  /** 正在转写的片段 */
  adapting: ReadonlySet<string>;
  /** 转写失败原因，键为「片段 id:格式」；重新转写或成功后清掉 */
  adaptErrors: Readonly<Record<string, string>>;
  /** 片段在当前格式下转写稿的状态；目标是 Seedance 时为 null */
  adaptStateOf: (segment: Segment) => AdaptState | "busy" | null;
  /** 选中片段此刻的组装结果 */
  plan: SegmentPlan | null;
  /** 选中片段的预估 / 报价积分；算不出来为 undefined */
  credits: number | undefined;
  /** 选中片段最近一次服务端报价状态（含刷新入口与 91002 过期标记） */
  quoteState?: {
    quote: TaskQuote | null;
    loading: boolean;
    expired: boolean;
    error: string | null;
    refresh: () => Promise<TaskQuote | null>;
  };
  /** 共享修剪抽屉状态（Preview / ClipBar / TakeCarousel 共用） */
  trimDrawer?: {
    target: { segmentId: string; pendingTaskId?: string | null } | null;
    open: (segmentId: string, pendingTaskId?: string | null) => void;
    close: () => void;
  };
  availableCredits: number | null;
  save: SaveSummary;
  reportSave: (key: string, status: AutosaveStatus) => void;
  /** 正在提交视频的片段 */
  submitting: ReadonlySet<string>;
  /** 提示词编辑器登记：生成 / 转写前立即存盘；返回注销函数 */
  registerEditor: (handle: EditorHandle) => () => void;
  actions: {
    add: () => Promise<void>;
    remove: (segment: Segment) => Promise<void>;
    reorder: (ids: string[]) => Promise<void>;
    setSeconds: (segment: Segment, seconds: number) => Promise<void>;
    patch: (
      sid: string,
      fields: Omit<UpdateSegmentInput, "revision">,
      options?: { expectedRevision?: number },
    ) => Promise<Segment>;
    choose: (segment: Segment, taskId: string) => Promise<void>;
    generate: (segment: Segment) => Promise<void>;
    batch: () => Promise<void>;
    /**
     * 把原稿转写成当前格式；转写稿手改过时先确认覆盖（force 跳过确认）。
     * 成功返回 true；失败原因写进 adaptErrors
     */
    adapt: (segment: Segment, options?: { force?: boolean }) => Promise<boolean>;
  };
};

/** 编辑中文字的种类：原稿，或某个格式的转写稿 */
export type DraftKind = "source" | TargetFormat;

/** 转写失败等状态的键 */
export const adaptKey = (sid: string, format: TargetFormat) => `${sid}:${format}`;

/** 批量转写的并发数 */
const ADAPT_CONCURRENCY = 3;

export type EditorHandle = {
  flush: () => Promise<void>;
};

export const StudioContext = createContext<Studio | null>(null);

export function useStudio(): Studio {
  const value = useContext(StudioContext);
  if (!value) throw new Error("useStudio 必须在片段工作区内部使用");
  return value;
}

/**
 * 片段工作区的数据层：片段列表（HTTP 读取 + WebSocket 推送 + 生成中兜底轮询）、资产库、模型与偏好、
 * 选中的片段及其参考素材组装、积分预估，以及增删改排序、采用结果、立即生成、批量生成这些动作。
 */
export function useStudioState(
  drama: Drama,
  no: number,
  refresh: () => Promise<void>,
  applyEpisode?: DramaWorkspace["applyEpisode"],
): Studio {
  const dramaId = drama.id;
  const episode = drama.episodes.find((ep) => ep.no === no);
  const episodeId = episode?.id;
  const workspace = useContext(DramaWorkspaceContext);
  const syncEpisode = applyEpisode ?? workspace?.applyEpisode;
  const syncEpisodeVersion = useCallback(
    (...items: ReadonlyArray<Pick<Segment, "segmentVersion" | "episodeId"> | null | undefined>) => {
      let maxVersion: number | undefined;
      let itemEpisodeId: string | undefined;
      for (const item of items) {
        if (!itemEpisodeId && item?.episodeId) {
          itemEpisodeId = item.episodeId;
        }
        if (typeof item?.segmentVersion === "number") {
          maxVersion =
            maxVersion === undefined
              ? item.segmentVersion
              : Math.max(maxVersion, item.segmentVersion);
        }
      }
      if (maxVersion !== undefined) {
        const targetId = itemEpisodeId ?? episodeId;
        syncEpisode?.(no, {
          ...(targetId ? { id: targetId } : {}),
          segmentVersion: maxVersion,
        });
      }
    },
    [episodeId, no, syncEpisode],
  );
  const triggerRefresh = useCallback(() => {
    void Promise.resolve(refresh()).catch(() => undefined);
  }, [refresh]);
  const key = segmentKey(dramaId, no);
  const list = useDramaSegmentsStore((state) => state.byEpisode[key]);
  const segments = list ?? NO_SEGMENTS;
  const [segmentsStatus, setSegmentsStatus] = useState<Studio["segmentsStatus"]>("loading");
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [drafts, setDrafts] = useState<Record<string, string>>({});
  const [adapting, setAdapting] = useState<ReadonlySet<string>>(new Set());
  const [adaptErrors, setAdaptErrors] = useState<Record<string, string>>({});
  const [submitting, setSubmitting] = useState<ReadonlySet<string>>(new Set());
  const [saveStates, setSaveStates] = useState<Record<string, AutosaveStatus>>({});
  const editors = useRef(new Set<EditorHandle>());
  const queues = useRef(new Map<string, Promise<unknown>>());

  // 载入 + 登记「正在看这一集」（WebSocket 重连 / 回到前台时据此对账）
  useEffect(() => {
    let active = true;
    void reloadSegments(dramaId, no).then(
      (ok) => active && setSegmentsStatus(ok ? "ready" : "error"),
    );
    const unwatch = useDramaSegmentsStore.getState().watch(dramaId, no);
    return () => {
      active = false;
      unwatch();
    };
  }, [dramaId, no]);

  // 有片段在生成：定时对账，推送丢了也能看到结果
  const anyRunning = segments.some(isRunning);
  useEffect(() => {
    if (!anyRunning) return;
    const timer = setInterval(() => void reloadSegments(dramaId, no), POLL_MS);
    return () => clearInterval(timer);
  }, [anyRunning, dramaId, no]);

  const settled = useDramaJobsStore((state) => state.settled[dramaId] ?? 0);
  const {
    assets,
    status: assetsStatus,
    reload: reloadAssets,
  } = useDramaAssets(drama.canvasId, settled);
  const video = useDramaVideoModel(dramaId, drama.videoModel);
  const prefs = useStudioPrefs(dramaId);
  const activeModelKey = video.model?.key ?? "";
  useEffect(() => {
    if (!activeModelKey || Object.keys(prefs.overrides).length === 0) return;
    useStudioPrefsStore
      .getState()
      .migrateLegacyOverrides(dramaId, drama.videoModel || activeModelKey);
  }, [activeModelKey, drama.videoModel, dramaId, prefs.overrides]);
  const availableCredits = useCreditsStore((state) => state.credits?.available ?? null);

  const selected = useMemo(
    () => segments.find((item) => item.id === selectedId) ?? segments[0] ?? null,
    [segments, selectedId],
  );
  const modelOverrides = useMemo(
    () => overridesForModel(prefs, activeModelKey || undefined, selected?.id),
    [activeModelKey, prefs, selected?.id],
  );
  const caps = video.model?.capabilities;
  const format = targetFormat(caps);
  const setDraft = useCallback<Studio["setDraft"]>((id, text, kind = "source") => {
    const name = `${id}:${kind}`;
    setDrafts((prev) => (prev[name] === text ? prev : { ...prev, [name]: text }));
  }, []);
  /** 片段原稿此刻的文字：选中片段编辑中的优先（只有它挂着编辑器） */
  const selectedId0 = selected?.id;
  const sourceOf = useCallback(
    (segment: Segment) =>
      (segment.id === selectedId0 ? drafts[`${segment.id}:source`] : undefined) ?? segment.prompt,
    [drafts, selectedId0],
  );
  const sourceTexts = useMemo(() => segments.map(sourceOf), [segments, sourceOf]);
  const hashOf = useTextHashes(sourceTexts);
  const adaptStateOf = useCallback<Studio["adaptStateOf"]>(
    (segment) => {
      if (format === "seedance") return null;
      if (adapting.has(segment.id)) return "busy";
      if (!segment.adaptations[format]) return "none";
      const hash = hashOf(sourceOf(segment));
      // 指纹还没算出来先按最新，算完会重新渲染
      return hash === undefined
        ? "fresh"
        : adaptState(segment, format, hash, effectiveDuration(caps, segment.seconds).value);
    },
    [adapting, caps, format, hashOf, sourceOf],
  );
  const prompt = useMemo(() => {
    if (!selected) return "";
    const adaptation = format === "seedance" ? undefined : selected.adaptations[format];
    if (!adaptation) return sourceOf(selected);
    return drafts[`${selected.id}:${format}`] ?? adaptation.prompt;
  }, [drafts, format, selected, sourceOf]);
  const plan = useMemo(
    () =>
      selected
        ? planSegmentInput({
            caps,
            prompt,
            seconds: selected.seconds,
            ratio: drama.ratio,
            overrides: modelOverrides,
            assets,
          })
        : null,
    [assets, caps, drama.ratio, modelOverrides, prompt, selected],
  );
  const [quoteSnapshot, setQuoteSnapshot] = useState<{
    key: string;
    quote: TaskQuote | null;
    loading: boolean;
    expired: boolean;
    error: string | null;
  } | null>(null);
  const quoteSeqRef = useRef(0);
  const quoteSnapshotRef = useRef(quoteSnapshot);
  useEffect(() => {
    quoteSnapshotRef.current = quoteSnapshot;
  }, [quoteSnapshot]);

  const singleShotBlocked =
    episode?.editSchema === 1 && plan?.input?.count !== undefined && Number(plan.input.count) !== 1;
  const canLiveQuote =
    !!selected &&
    !!video.model &&
    !!plan &&
    Object.keys(plan.errors).length === 0 &&
    Object.keys(plan.input).length > 0 &&
    !singleShotBlocked;
  const quoteInputJson = useMemo(() => (plan ? canonicalJson(plan.input) : ""), [plan]);
  const liveQuoteKey =
    canLiveQuote && selected && video.model
      ? `${selected.id}::${video.model.key}::${quoteInputJson}`
      : "";

  const refreshQuote = useCallback(async (): Promise<TaskQuote | null> => {
    if (!selected || !video.model || !plan || !liveQuoteKey) return null;
    const seq = ++quoteSeqRef.current;
    setQuoteSnapshot((prev) => ({
      key: liveQuoteKey,
      quote: prev?.key === liveQuoteKey ? prev.quote : null,
      loading: true,
      expired: false,
      error: null,
    }));
    try {
      const next = await quoteSegment(selected.id, {
        modelId: video.model.key,
        input: plan.input,
      });
      if (seq !== quoteSeqRef.current) return null;
      setQuoteSnapshot({
        key: liveQuoteKey,
        quote: next,
        loading: false,
        expired: false,
        error: null,
      });
      return next;
    } catch (e) {
      if (seq !== quoteSeqRef.current) return null;
      setQuoteSnapshot((prev) => ({
        key: liveQuoteKey,
        quote: prev?.key === liveQuoteKey ? prev.quote : null,
        loading: false,
        expired: isQuoteExpiredError(e),
        error: dramaErrorMessage(e, "获取报价失败"),
      }));
      return null;
    }
  }, [liveQuoteKey, plan, selected, video.model]);

  useEffect(() => {
    if (!liveQuoteKey) return;
    const timer = setTimeout(() => {
      void refreshQuote();
    }, 60);
    return () => {
      clearTimeout(timer);
    };
  }, [liveQuoteKey, refreshQuote]);

  const activeQuoteState =
    liveQuoteKey && quoteSnapshot?.key === liveQuoteKey ? quoteSnapshot : null;
  const activeServerQuote = activeQuoteState?.quote ?? null;
  const quoteLoading = activeQuoteState?.loading ?? false;
  const quoteExpired = activeQuoteState?.expired ?? false;
  const quoteError = activeQuoteState?.error ?? null;

  const credits = useMemo(() => {
    if (activeServerQuote) return activeServerQuote.credits;
    if (!plan || !video.model || Object.keys(plan.input).length === 0) return undefined;
    return quote(video.model.pricing, caps, priceSpecOf(caps, plan.input));
  }, [activeServerQuote, caps, plan, video.model]);

  const reportSave = useCallback((name: string, status: AutosaveStatus) => {
    setSaveStates((prev) => (prev[name] === status ? prev : { ...prev, [name]: status }));
  }, []);
  const save = useMemo(() => summarizeSave(saveStates), [saveStates]);

  /** 改片段：同一个片段的修改串行，默认取 store 最新 revision；若传入 expectedRevision 则严格按基线 CAS */
  const patch = useCallback<Studio["actions"]["patch"]>(
    (sid, fields, options) => {
      const run = async () => {
        const current = useDramaSegmentsStore
          .getState()
          .byEpisode[key]?.find((item) => item.id === sid);
        if (!current) throw new Error("片段不存在");
        const expectedRev = options?.expectedRevision ?? current.revision;
        if (
          options?.expectedRevision !== undefined &&
          current.revision !== options.expectedRevision
        ) {
          toast.warning("这个片段在别处被修改过，已为你加载最新内容");
          void reloadSegments(dramaId, no);
          throw new ApiError("这个片段在别处被修改过，请重新载入最新版本后再保存", 30002, 409);
        }
        try {
          const next = await updateSegment(sid, { ...fields, revision: expectedRev });
          useDramaSegmentsStore.getState().apply(dramaId, no, next);
          syncEpisodeVersion(next);
          if (
            fields.edit !== undefined ||
            fields.chosenTaskId !== undefined ||
            fields.expectedTaskId !== undefined
          ) {
            triggerRefresh();
          }
          return next;
        } catch (e) {
          if (isRevisionConflict(e)) {
            toast.warning("这个片段在别处被修改过，已为你加载最新内容");
            void reloadSegments(dramaId, no);
          } else if (isDramaError(e, DRAMA_ERR.jobBusy)) {
            toast.warning(dramaErrorMessage(e, "这一集还有任务在跑，请等任务结束后再修改"));
          } else if (isEditInvalidError(e)) {
            toast.error(dramaErrorMessage(e, "分镜剪辑参数不合法"));
          } else if (isDramaError(e, DRAMA_ERR.segmentNotFound)) {
            toast.error(dramaErrorMessage(e));
            useDramaSegmentsStore.getState().remove(dramaId, no, sid);
          } else if (isDramaError(e, DRAMA_ERR.takeInvalid)) {
            toast.error(dramaErrorMessage(e));
            void reloadSegments(dramaId, no);
          } else {
            toast.error(dramaErrorMessage(e, "保存失败，请稍后重试"));
          }
          throw e;
        }
      };
      const prev = queues.current.get(sid) ?? Promise.resolve();
      const result = prev.then(run, run);
      queues.current.set(
        sid,
        result.catch(() => undefined),
      );
      return result;
    },
    [dramaId, key, no, syncEpisodeVersion, triggerRefresh],
  );

  /** 带保存状态的一次性修改（时长、采用） */
  const patchWithStatus = useCallback(
    async (sid: string, fields: Omit<UpdateSegmentInput, "revision">) => {
      reportSave("op", "saving");
      try {
        await patch(sid, fields);
      } finally {
        reportSave("op", "saved");
      }
    },
    [patch, reportSave],
  );

  const markSubmitting = useCallback((sid: string, on: boolean) => {
    setSubmitting((prev) => {
      const next = new Set(prev);
      if (on) next.add(sid);
      else next.delete(sid);
      return next;
    });
  }, []);

  /** 所有挂着的提示词编辑器立即存盘（原稿、转写稿） */
  const flushEditors = useCallback(async () => {
    await Promise.all([...editors.current].map((handle) => handle.flush()));
  }, []);

  const markAdapting = useCallback((sid: string, on: boolean) => {
    setAdapting((prev) => {
      const next = new Set(prev);
      if (on) next.add(sid);
      else next.delete(sid);
      return next;
    });
  }, []);

  /** 从 store 取片段的最新快照 */
  const latestOf = useCallback(
    (sid: string) =>
      useDramaSegmentsStore.getState().byEpisode[key]?.find((item) => item.id === sid) ?? null,
    [key],
  );

  /**
   * 转写一个片段到当前格式：先存盘原稿，用 store 里最新的 revision 调后端；
   * 失败只记原因，不动已有转写稿（后端失败也不覆盖）
   */
  const adapt = useCallback<Studio["actions"]["adapt"]>(
    async (segment, options) => {
      if (format === "seedance" || adapting.has(segment.id)) return false;
      const existing = segment.adaptations[format];
      if (existing?.edited && !options?.force) {
        const go = await confirm({
          title: "覆盖手改的转写稿？",
          description: `这份 ${formatLabel(format)} 转写稿被你改过，重新转写会覆盖这些修改。`,
          confirmLabel: "重新转写",
        });
        if (!go) return false;
      }
      const errorKey = adaptKey(segment.id, format);
      markAdapting(segment.id, true);
      setAdaptErrors((prev) => {
        const { [errorKey]: _old, ...rest } = prev;
        return rest;
      });
      try {
        await flushEditors();
        const current = latestOf(segment.id) ?? segment;
        if (!current.prompt.trim()) throw new Error("原稿是空的，先写好原稿再转写");
        const next = await adaptSegment(segment.id, { format, revision: current.revision });
        useDramaSegmentsStore.getState().apply(dramaId, no, next);
        syncEpisodeVersion(next);
        return true;
      } catch (e) {
        const message =
          e instanceof ApiError || !(e instanceof Error)
            ? dramaErrorMessage(e, "转写失败，请稍后重试")
            : e.message;
        setAdaptErrors((prev) => ({ ...prev, [errorKey]: message }));
        if (isRevisionConflict(e)) void reloadSegments(dramaId, no);
        if (isDramaError(e, DRAMA_ERR.segmentNotFound)) {
          useDramaSegmentsStore.getState().remove(dramaId, no, segment.id);
        }
        return false;
      } finally {
        markAdapting(segment.id, false);
      }
    },
    [adapting, dramaId, flushEditors, format, latestOf, markAdapting, no, syncEpisodeVersion],
  );

  /**
   * 提交前重读一次画布里的资产，用最新的形象和声音组装（资产页刚改的、还没被定时重读捕到的也算）；
   * 读取失败返回 null
   */
  const freshAssets = useCallback(() => reloadAssets(), [reloadAssets]);

  const submitVideo = useCallback(
    async (segment: Segment, latest: RefAsset[]) => {
      const model = video.model;
      if (!model) return { ok: false as const, message: "没有可用的视频模型" };
      const currentPrefs = useStudioPrefsStore.getState().byDrama[dramaId] ?? prefs;
      const segOverrides = overridesForModel(currentPrefs, model.key, segment.id);
      const built = planSegmentInput({
        caps: model.capabilities,
        prompt: segment.prompt,
        seconds: segment.seconds,
        ratio: drama.ratio,
        overrides: segOverrides,
        assets: latest,
      });
      const firstError = Object.values(built.errors)[0];
      if (firstError) return { ok: false as const, message: firstError };
      if (
        episode?.editSchema === 1 &&
        built.input.count !== undefined &&
        Number(built.input.count) !== 1
      ) {
        return {
          ok: false as const,
          message: "精裁模式（edit_schema=1）单次仅支持生成 1 条，请先将生成数量改为 1",
        };
      }

      const quoteKey = `${segment.id}::${model.key}::${canonicalJson(built.input)}`;
      const prevDisplayed =
        selected?.id === segment.id &&
        quoteSnapshotRef.current?.key === quoteKey &&
        !quoteSnapshotRef.current.expired
          ? quoteSnapshotRef.current.quote
          : null;

      const fetchQuote = async () => {
        const quoted = await quoteSegment(segment.id, {
          modelId: model.key,
          input: built.input,
        });
        if (selected?.id === segment.id) {
          setQuoteSnapshot({
            key: quoteKey,
            quote: quoted,
            loading: false,
            expired: false,
            error: null,
          });
        }
        return quoted;
      };

      const errorText = (err: unknown) => {
        if (
          isEditInvalidError(err) ||
          isQuoteExpiredError(err) ||
          (typeof (err as { code?: unknown })?.code === "number" &&
            (err as { code: number }).code >= 67000)
        ) {
          return dramaErrorMessage(err);
        }
        return describeSubmitError(err).message;
      };

      let quoted: TaskQuote;
      try {
        quoted = await fetchQuote();
      } catch (e) {
        if (isQuoteExpiredError(e) && selected?.id === segment.id) {
          setQuoteSnapshot((prev) => ({
            key: quoteKey,
            quote: prev?.key === quoteKey ? prev.quote : null,
            loading: false,
            expired: true,
            error: dramaErrorMessage(e),
          }));
        }
        return { ok: false as const, message: errorText(e) };
      }

      if (prevDisplayed && !sameQuoteTerms(prevDisplayed, quoted)) {
        const msg = `报价已更新（${prevDisplayed.credits} → ${quoted.credits} 积分），请核对生效参数后再次点击生成`;
        if (selected?.id === segment.id) {
          setQuoteSnapshot({
            key: quoteKey,
            quote: quoted,
            loading: false,
            expired: false,
            error: msg,
          });
        }
        return { ok: false as const, message: msg };
      }

      try {
        await submitSegmentTask(segment.id, {
          model_id: model.key,
          input: built.input,
          quote_token: quoted.quoteToken,
        });
        return { ok: true as const };
      } catch (e) {
        if (isQuoteExpiredError(e)) {
          try {
            const refreshed = await fetchQuote();
            // 仅当刷新前后价格、指纹与生效参数完全一致时自动重提；否则停下同步 UI 并等待用户再次点击
            if (!sameQuoteTerms(quoted, refreshed)) {
              const msg = `报价已过期且计费或生效参数已变化（${quoted.credits} → ${refreshed.credits} 积分），请核对后再次点击生成`;
              if (selected?.id === segment.id) {
                setQuoteSnapshot({
                  key: quoteKey,
                  quote: refreshed,
                  loading: false,
                  expired: false,
                  error: msg,
                });
              }
              return { ok: false as const, message: msg };
            }
            await submitSegmentTask(segment.id, {
              model_id: model.key,
              input: built.input,
              quote_token: refreshed.quoteToken,
            });
            return { ok: true as const };
          } catch (retryErr) {
            if (isQuoteExpiredError(retryErr) && selected?.id === segment.id) {
              setQuoteSnapshot((prev) => ({
                key: quoteKey,
                quote: prev?.key === quoteKey ? prev.quote : null,
                loading: false,
                expired: true,
                error: dramaErrorMessage(retryErr),
              }));
            }
            return { ok: false as const, message: errorText(retryErr) };
          }
        }
        return { ok: false as const, message: errorText(e) };
      }
    },
    [drama.ratio, dramaId, episode?.editSchema, prefs, selected?.id, video.model],
  );

  /**
   * 决定这个片段要提交哪份提示词：
   * 1) 始终按模型最终生效时长校验实际时间轴，实际越界禁止绕过（不可走“仍用当前转写稿”）；
   * 2) 无时间轴越界时，原稿文字过期或转写时长变化可由用户选择保留或重新转写。
   */
  const resolveSubmit = useCallback(
    async (segment: Segment): Promise<Segment | null> => {
      let current = latestOf(segment.id) ?? segment;
      const effectiveSec = effectiveDuration(caps, current.seconds).value;
      if (format === "seedance") {
        const tl = timelineOverrun(current.prompt, effectiveSec);
        if (tl.overrun) {
          toast.error(timelineOverrunMessage(tl.end, effectiveSec));
          return null;
        }
        return current;
      }
      const adaptation = current.adaptations[format];
      if (!adaptation) {
        toast.error(`先把原稿转写为 ${formatLabel(format)}`);
        return null;
      }
      const label = formatLabel(format);
      const tl = timelineOverrun(adaptation.prompt, effectiveSec);
      if (tl.overrun) {
        if (current.seconds !== effectiveSec) {
          toast.error(
            `当前 ${label} 转写稿时间轴写到 ${formatTimelineSeconds(tl.end)}s，超过模型实际生成时长 ${effectiveSec}s；请先将片段时长调为 ${effectiveSec}s 后重新转写，或手动修改时间轴`,
          );
          return null;
        }
        const picked = await chooseOption<"readapt">({
          title: "转写稿时间轴超过生成时长",
          description: `当前 ${label} 转写稿时间轴写到 ${formatTimelineSeconds(tl.end)}s，超过片段时长 ${effectiveSec}s${adaptation.edited ? "；重新转写会覆盖你手改的修改" : ""}，不能直接提交。`,
          options: [{ value: "readapt", label: "重新转写并生成", primary: true }],
        });
        if (picked !== "readapt") return null;
        if (!(await adapt(current, { force: true }))) {
          toast.error("转写失败，没有提交");
          return null;
        }
        current = latestOf(segment.id) ?? current;
        const nextText = submitText(current, format);
        const nextEffective = effectiveDuration(caps, current.seconds).value;
        const nextTl = timelineOverrun(nextText, nextEffective);
        if (nextTl.overrun) {
          toast.error(timelineOverrunMessage(nextTl.end, nextEffective));
          return null;
        }
        return { ...current, prompt: nextText };
      }

      const textStale = adaptation.sourceHash !== (await sha256Hex(current.prompt));
      const durationStale = adaptationDurationMismatch(adaptation, effectiveSec);
      if (textStale || durationStale) {
        const reasons: string[] = [];
        if (textStale) reasons.push("原稿在转写后改过");
        if (durationStale) {
          reasons.push(`转写稿按 ${adaptation.seconds}s 生成，当前将按 ${effectiveSec}s 生成`);
        }
        const canReadapt = current.seconds === effectiveSec;
        const picked = await chooseOption({
          title: textStale ? "原稿在转写后改过" : "生成时长与转写时不一致",
          description:
            `${reasons.join("；")}${adaptation.edited ? "，并且被你手改过，重新转写会覆盖这些修改" : ""}。` +
            (!canReadapt
              ? `若要按 ${effectiveSec}s 重新转写，请先将片段时长改为 ${effectiveSec}s。`
              : ""),
          options: [
            { value: "keep", label: "仍用当前转写稿生成", primary: !canReadapt },
            ...(canReadapt
              ? [{ value: "readapt" as const, label: "重新转写并生成", primary: true }]
              : []),
          ],
        });
        if (!picked) return null;
        if (picked === "readapt") {
          if (!(await adapt(current, { force: true }))) {
            toast.error("转写失败，没有提交");
            return null;
          }
          current = latestOf(segment.id) ?? current;
          const nextText = submitText(current, format);
          const nextEffective = effectiveDuration(caps, current.seconds).value;
          const nextTl = timelineOverrun(nextText, nextEffective);
          if (nextTl.overrun) {
            toast.error(timelineOverrunMessage(nextTl.end, nextEffective));
            return null;
          }
        }
      }
      return { ...current, prompt: submitText(current, format) };
    },
    [adapt, caps, format, latestOf],
  );

  const generate = useCallback<Studio["actions"]["generate"]>(
    async (segment) => {
      if (submitting.has(segment.id)) return;
      markSubmitting(segment.id, true);
      try {
        // 编辑中的提示词先存盘，提交的就是屏幕上看到的
        await flushEditors();
        const current = await resolveSubmit(segment);
        if (!current) return;
        const latest = await freshAssets();
        if (!latest) {
          toast.error("资产信息读取失败，请稍后重试");
          return;
        }
        const currentPrefs = useStudioPrefsStore.getState().byDrama[dramaId] ?? prefs;
        const preview = planSegmentInput({
          caps: video.model?.capabilities,
          prompt: current.prompt,
          seconds: current.seconds,
          ratio: drama.ratio,
          overrides: overridesForModel(currentPrefs, video.model?.key, current.id),
          assets: latest,
        });
        if (preview.deadLabels.length > 0) {
          const go = await confirm({
            title: "有引用已失效",
            description: `「${preview.deadLabels.join("」「")}」对应的资产已被删除，这部分只会按文字提交。要继续生成吗？`,
            confirmLabel: "继续生成",
          });
          if (!go) return;
        }
        const result = await submitVideo(current, latest);
        if (!result.ok) {
          toast.error(result.message);
          return;
        }
        void reloadSegments(dramaId, no);
      } finally {
        markSubmitting(segment.id, false);
      }
    },
    [
      drama.ratio,
      dramaId,
      flushEditors,
      freshAssets,
      markSubmitting,
      no,
      prefs,
      resolveSubmit,
      submitVideo,
      submitting,
      video.model,
    ],
  );

  /**
   * 批量前按模型生效时长复核所有片段：
   * 1) Seedance 过滤时间轴越界的片段；
   * 2) 其他格式把时间轴超限片段排除出「直接提交」集合（仅允许重转且重转后再校验），绝不走「仍用旧稿」。
   */
  const prepareBatch = useCallback(
    async (targets: Segment[]): Promise<Segment[] | null> => {
      const effectiveSecondsOf = (item: Segment) => effectiveDuration(caps, item.seconds).value;
      if (format === "seedance") {
        const checked = evaluateSeedanceBatch(targets, effectiveSecondsOf);
        if (checked.blocked.length === 0) return checked.ready;
        if (checked.ready.length === 0) {
          toast.error(`以下片段时间轴超过生成时长，不能提交：片段 ${segmentNos(checked.blocked)}`);
          return null;
        }
        const go = await confirm({
          title: `有 ${checked.blocked.length} 个片段时间轴超过生成时长`,
          description: `片段 ${segmentNos(checked.blocked)} 的时间轴超出实际生成时长，本次不会提交；是否先生成其余 ${checked.ready.length} 个片段？`,
          confirmLabel: "仅生成其余片段",
        });
        return go ? checked.ready : null;
      }

      const hashes = new Map(
        await Promise.all(
          targets.map(async (item) => [item.id, await sha256Hex(item.prompt)] as const),
        ),
      );
      const split = splitBatch(
        targets,
        format,
        (item) => hashes.get(item.id) ?? "",
        effectiveSecondsOf,
      );
      const pending = [...split.none, ...split.stale, ...split.overrun];
      let picked: "adapt" | "ready" = "ready";
      if (pending.length > 0) {
        const label = formatLabel(format);
        const parts = [`已就绪 ${split.ready.length} 个`];
        if (split.none.length > 0) parts.push(`未转写 ${split.none.length} 个`);
        if (split.stale.length > 0) parts.push(`已过期 ${split.stale.length} 个`);
        if (split.overrun.length > 0) parts.push(`时间轴超限须重转 ${split.overrun.length} 个`);
        if (split.blocked.length > 0) {
          parts.push(`时长与模型锁定秒数不符且超限 ${split.blocked.length} 个（须先手动调整）`);
        }
        const choice = await chooseOption({
          title: `有 ${pending.length} 个片段需要先转写为 ${label}`,
          description:
            `${parts.join("；")}。` +
            (split.edited > 0 ? `其中 ${split.edited} 个转写稿被手改过，重新转写会覆盖。` : ""),
          options: [
            ...(split.ready.length > 0
              ? [{ value: "ready" as const, label: "只生成已就绪的" }]
              : []),
            { value: "adapt" as const, label: "转写后生成", primary: true },
          ],
        });
        if (!choice) return null;
        picked = choice;
      } else if (split.blocked.length > 0) {
        if (split.ready.length === 0) {
          toast.error(
            `以下片段时间轴超过生成时长且无法自动重转，请先调整时长或手改时间轴：片段 ${segmentNos(split.blocked)}`,
          );
          return null;
        }
        const go = await confirm({
          title: `有 ${split.blocked.length} 个片段时间轴超过生成时长`,
          description: `片段 ${segmentNos(split.blocked)} 的时间轴超出实际生成时长且片段秒数尚未同步，本次将跳过；是否先生成其余 ${split.ready.length} 个片段？`,
          confirmLabel: "仅生成其余片段",
        });
        if (!go) return null;
      }

      const ids = new Set(split.ready.map((item) => item.id));
      if (picked === "adapt" && pending.length > 0) {
        const failed: Segment[] = [];
        const queue = [...pending];
        const worker = async () => {
          for (let item = queue.shift(); item; item = queue.shift()) {
            if (await adapt(item, { force: true })) ids.add(item.id);
            else failed.push(item);
          }
        };
        await Promise.all(
          Array.from({ length: Math.min(ADAPT_CONCURRENCY, queue.length) }, worker),
        );
        if (failed.length > 0) {
          toast.error(`${failed.length} 个片段转写失败，已跳过：片段 ${segmentNos(failed)}`);
        }
      } else if (split.blocked.length > 0 && pending.length > 0) {
        toast.warning(
          `片段 ${segmentNos(split.blocked)} 时间轴超过模型生效时长且秒数未同步，已跳过`,
        );
      }

      const finalTargets: Segment[] = [];
      const stillOverrun: Segment[] = [];
      for (const item of targets) {
        if (!ids.has(item.id)) continue;
        const current = latestOf(item.id) ?? item;
        const text = submitText(current, format);
        const sec = effectiveSecondsOf(current);
        if (!text || timelineOverrun(text, sec).overrun) {
          stillOverrun.push(current);
          continue;
        }
        finalTargets.push({ ...current, prompt: text });
      }
      if (stillOverrun.length > 0) {
        toast.error(`以下片段时间轴仍超出生成时长，已跳过：片段 ${segmentNos(stillOverrun)}`);
      }
      return finalTargets;
    },
    [adapt, caps, format, latestOf],
  );

  const batch = useCallback<Studio["actions"]["batch"]>(async () => {
    const all = batchTargets(useDramaSegmentsStore.getState().byEpisode[key] ?? NO_SEGMENTS);
    if (all.length === 0) {
      toast.info("没有待生成的片段");
      return;
    }
    await flushEditors();
    const targets = await prepareBatch(all);
    if (!targets) return;
    if (targets.length === 0) {
      toast.info("没有可以直接生成的片段");
      return;
    }
    const latest = await freshAssets();
    if (!latest) {
      toast.error("资产信息读取失败，请稍后重试");
      return;
    }
    // 引用失效的先一起问一次，允许继续
    const currentPrefs = useStudioPrefsStore.getState().byDrama[dramaId] ?? prefs;
    const dead = new Set<string>();
    for (const item of targets) {
      const planned = planSegmentInput({
        caps: video.model?.capabilities,
        prompt: item.prompt,
        seconds: item.seconds,
        ratio: drama.ratio,
        overrides: overridesForModel(currentPrefs, video.model?.key, item.id),
        assets: latest,
      });
      planned.deadLabels.forEach((label) => dead.add(label));
    }
    if (dead.size > 0) {
      const go = await confirm({
        title: "有引用已失效",
        description: `「${[...dead].join("」「")}」对应的资产已被删除，这部分只会按文字提交。要继续批量生成吗？`,
        confirmLabel: "继续生成",
      });
      if (!go) return;
    }
    let ok = 0;
    const failed: string[] = [];
    for (const item of targets) {
      markSubmitting(item.id, true);
      const result = await submitVideo(item, latest);
      markSubmitting(item.id, false);
      if (result.ok) ok += 1;
      else {
        failed.push(result.message);
        // 积分不足、排队已满之类每个片段都一样，不必再试
        if (/积分不足|排队任务已达上限/.test(result.message)) break;
      }
    }
    void reloadSegments(dramaId, no);
    if (ok > 0) toast.info(`已提交 ${ok} 个片段`);
    if (failed.length > 0) toast.error(`有 ${targets.length - ok} 个片段没能提交：${failed[0]}`);
  }, [
    drama.ratio,
    dramaId,
    flushEditors,
    freshAssets,
    key,
    markSubmitting,
    no,
    prefs,
    prepareBatch,
    submitVideo,
    video.model,
  ]);

  const add = useCallback<Studio["actions"]["add"]>(async () => {
    try {
      const created = await addSegment(dramaId, no, {
        seconds: effectiveDuration(video.model?.capabilities, DEFAULT_SECONDS).value,
      });
      useDramaSegmentsStore.getState().apply(dramaId, no, created);
      syncEpisodeVersion(created);
      setSelectedId(created.id);
      triggerRefresh();
    } catch (e) {
      toast.error(dramaErrorMessage(e, "新增失败，请稍后重试"));
    }
  }, [dramaId, no, syncEpisodeVersion, triggerRefresh, video.model]);

  const remove = useCallback<Studio["actions"]["remove"]>(
    async (segment) => {
      const ok = await confirm({
        title: `删除片段 ${String(segment.seq + 1).padStart(2, "0")}？`,
        description: "只删除这个片段和它的提示词；已经生成的视频和素材不会被删除。",
        confirmLabel: "删除",
        destructive: true,
        onConfirm: () => deleteSegment(segment.id),
      });
      if (!ok) return;
      useDramaSegmentsStore.getState().remove(dramaId, no, segment.id);
      void reloadSegments(dramaId, no);
      triggerRefresh();
    },
    [dramaId, no, triggerRefresh],
  );

  const reorder = useCallback<Studio["actions"]["reorder"]>(
    async (ids) => {
      const current = useDramaSegmentsStore.getState().byEpisode[key] ?? NO_SEGMENTS;
      if (sameOrder(ids, orderIds(current))) return;
      // 先在本地换序，界面不等请求
      useDramaSegmentsStore.getState().setList(dramaId, no, applyOrder(current, ids));
      try {
        const reordered = await reorderSegments(dramaId, no, ids);
        useDramaSegmentsStore.getState().setList(dramaId, no, reordered);
        syncEpisodeVersion(...reordered);
        // 顺序变了：成片是否过期要跟着刷新
        triggerRefresh();
      } catch (e) {
        toast.error(dramaErrorMessage(e, "排序失败，已恢复原顺序"));
        void reloadSegments(dramaId, no);
      }
    },
    [dramaId, key, no, syncEpisodeVersion, triggerRefresh],
  );

  const setSeconds = useCallback<Studio["actions"]["setSeconds"]>(
    async (segment, seconds) => {
      if (seconds === segment.seconds) return;
      await patchWithStatus(segment.id, { seconds }).catch(() => undefined);
      triggerRefresh();
    },
    [patchWithStatus, triggerRefresh],
  );

  const choose = useCallback<Studio["actions"]["choose"]>(
    async (segment, taskId) => {
      const check = chooseCheck(segment, taskId);
      if (check === "already") return;
      if (check === "invalid") {
        toast.error(dramaErrorMessage(null, "只能采用本片段已成功的结果"));
        return;
      }
      let resetEditOnTaskChange: boolean | undefined;
      if (hasNonDefaultSegmentEdit(segmentEditOf(segment))) {
        const picked = await chooseOption<"reset" | "keep">({
          title: "当前镜头已有剪辑或原声设置",
          description:
            "切换采用的视频版本时，请选择重置精裁与原声参数，或保留当前参数继续应用到新视频。",
          options: [
            { value: "reset", label: "重置剪辑并切换", primary: true },
            { value: "keep", label: "保留剪辑并切换" },
          ],
        });
        if (!picked) return;
        resetEditOnTaskChange = picked === "reset";
      }
      await patchWithStatus(segment.id, {
        chosenTaskId: taskId,
        expectedTaskId: segment.chosenTaskId || "",
        ...(resetEditOnTaskChange !== undefined ? { resetEditOnTaskChange } : {}),
      }).catch(() => undefined);
      triggerRefresh();
    },
    [patchWithStatus, triggerRefresh],
  );

  const registerEditor = useCallback((handle: EditorHandle) => {
    editors.current.add(handle);
    return () => {
      editors.current.delete(handle);
    };
  }, []);

  const setOverride = useCallback(
    (name: string, value: unknown) => {
      if (!activeModelKey) return;
      useStudioPrefsStore
        .getState()
        .setModelOverride(
          dramaId,
          activeModelKey,
          name,
          value,
          drama.videoModel || activeModelKey,
          selected?.id,
        );
    },
    [activeModelKey, drama.videoModel, dramaId, selected?.id],
  );

  const [trimTarget, setTrimTarget] = useState<{
    segmentId: string;
    pendingTaskId?: string | null;
  } | null>(null);
  const openTrim = useCallback((segmentId: string, pendingTaskId?: string | null) => {
    setTrimTarget({ segmentId, pendingTaskId: pendingTaskId ?? null });
  }, []);
  const closeTrim = useCallback(() => {
    setTrimTarget(null);
  }, []);
  const trimDrawer = useMemo(
    () => ({ target: trimTarget, open: openTrim, close: closeTrim }),
    [closeTrim, openTrim, trimTarget],
  );

  const quoteState = useMemo(
    () => ({
      quote: activeServerQuote,
      loading: quoteLoading,
      expired: quoteExpired,
      error: quoteError,
      refresh: refreshQuote,
    }),
    [activeServerQuote, quoteError, quoteExpired, quoteLoading, refreshQuote],
  );

  return {
    drama,
    // 调用方保证这一集存在
    episode: episode as EpisodeSummary,
    no,
    segments,
    segmentsStatus,
    selected,
    select: setSelectedId,
    assets,
    assetsStatus,
    video,
    overrides: modelOverrides,
    setOverride,
    prompt,
    drafts,
    setDraft,
    format,
    adapting,
    adaptErrors,
    adaptStateOf,
    plan,
    credits,
    quoteState,
    trimDrawer,
    availableCredits,
    save,
    reportSave,
    submitting,
    registerEditor,
    actions: { add, remove, reorder, setSeconds, patch, choose, generate, batch, adapt },
  };
}
