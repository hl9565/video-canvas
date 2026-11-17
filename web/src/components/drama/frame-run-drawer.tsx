import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Loader2, RefreshCw } from "lucide-react";

import { getAsset, uploadAsset } from "@/api/asset";
import type { AssetDto } from "@/api/asset/type";
import { getCanvasList } from "@/api/canvas";
import type { CanvasListItemDto } from "@/api/canvas/type";
import { quoteSegment } from "@/api/drama";
import { dramaErrorMessage, isQuoteExpiredError } from "@/api/drama/errors";
import type {
  CreateFrameRunInput,
  FrameRunState,
  FrameSource,
  Segment,
  SegmentFrameRun,
  TaskQuote,
} from "@/api/drama/type";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { Textarea } from "@/components/ui/textarea";
import type { StudioFrameRunActions } from "@/pages/drama/studio/use-studio";
import { useModelsStore } from "@/store/models";
import type { RefAsset } from "@/utils/drama/segment-assets";
import { ApiError } from "@/utils/requests/request";

/** Stage B 固定首帧 I2V 模型。 */
const I2V_MODEL = "m-1c89decdf8fd";
const EMPTY_MODELS: never[] = [];
const SELECT_CLASS =
  "bg-chrome-hover border-chrome-border min-h-10 w-full rounded-xl border px-3 text-sm";
const STATE_LABELS: Record<FrameRunState, string> = {
  draft: "草稿",
  frame_pending: "生成首帧中",
  frame_ready: "首帧就绪",
  confirmed: "已确认",
  submitting: "提交中",
  video_pending: "生成视频中",
  bound: "已绑定完成",
  failed: "失败",
  cancelled: "已取消",
};
const isTerminal = (run: SegmentFrameRun) => ["bound", "failed", "cancelled"].includes(run.state);

function normalizeConfirmedField(key: string, value: unknown): string {
  if (key === "images") {
    return JSON.stringify(Array.isArray(value) ? value.map(String) : []);
  }
  return JSON.stringify(value);
}

/** 常驻首帧控镜抽屉；任务动作只能通过 Studio 提供的 gateway 适配器。 */
export interface FrameRunDrawerProps {
  /** 是否打开 */
  open: boolean;
  /** 关闭回调 */
  onOpenChange: (open: boolean) => void;
  /** 当前片段 */
  segment: Segment | null;
  /** 短剧 ID */
  dramaId: string;
  /** 集号 */
  episodeNo: number;
  /** 默认构图画布 */
  canvasId: string;
  /** 短剧图片资产 */
  assets: readonly RefAsset[];
  /** 流程动作及快照同步 */
  actions: StudioFrameRunActions;
}

/** 返回模型声明的合法整数时长；缺少元数据时使用首帧模型的常用档位。 */
function durationOptions(
  field: { options?: (string | number)[]; min?: number; max?: number; step?: number } | undefined,
): number[] {
  const values = field?.options?.map(Number).filter((n) => Number.isInteger(n) && n > 0) ?? [];
  if (values.length) return [...new Set(values)];
  if (field?.min && field.max && field.max <= 120) {
    const result: number[] = [];
    for (
      let n = Math.ceil(field.min);
      n <= field.max;
      n += Math.max(1, Math.trunc(field.step ?? 1))
    )
      result.push(n);
    if (result.length) return result;
  }
  return [5, 10];
}

/** 展示服务端裁切后的真实素材，不能用用户最初选择的图片替代。 */
function FramePreview({ run }: { run: SegmentFrameRun }) {
  const [preview, setPreview] = useState<{ id: string; asset?: AssetDto; error?: string } | null>(
    null,
  );
  useEffect(() => {
    if (!run.frameAssetId) return;
    let active = true;
    const id = run.frameAssetId;
    void getAsset(id).then(
      (asset) => {
        if (active) setPreview({ id, asset });
      },
      () => {
        if (active) setPreview({ id, error: "首帧预览加载失败" });
      },
    );
    return () => {
      active = false;
    };
  }, [run.frameAssetId, run.frameSha256]);
  if (!run.frameAssetId) return null;
  const current = preview?.id === run.frameAssetId ? preview : null;
  return (
    <section className="space-y-2" aria-label="实际首帧预览">
      <h3 className="font-medium">16:9 首帧预览 · 实际送入模型</h3>
      {current?.asset ? (
        <>
          <img
            alt="实际送入 I2V 的首帧"
            src={current.asset.url}
            className="aspect-video w-full rounded-xl bg-black object-contain"
          />
          <p className="text-muted-foreground text-xs tabular-nums">
            {current.asset.width}×{current.asset.height} · SHA-256{" "}
            {run.frameSha256?.slice(0, 12) || "待就绪"}
          </p>
        </>
      ) : (
        <p role="status">{current?.error ?? "加载首帧预览…"}</p>
      )}
    </section>
  );
}

/** 图片选择与上传在初建、替换时共用；上传后只提交素材 ID。 */
function ImagePicker({
  assets,
  value,
  onChange,
  onUpload,
  replacing,
  disabled,
  uploadedAsset,
}: {
  assets: readonly RefAsset[];
  value: string;
  onChange: (id: string) => void;
  onUpload: (file: File) => void;
  replacing?: boolean;
  disabled: boolean;
  uploadedAsset?: AssetDto | null;
}) {
  const images = assets.filter(
    (asset) =>
      asset.assetId &&
      !asset.generating &&
      (asset.kind === "image" || asset.kind === "character" || asset.mediaType === "image"),
  );
  return (
    <div className="space-y-3">
      <label className="grid gap-1.5 text-sm">
        {replacing ? "替换图片" : "选已有图片"}
        <select
          aria-label={replacing ? "替换图片" : "已有图片"}
          className={SELECT_CLASS}
          value={value}
          disabled={disabled}
          onChange={(e) => onChange(e.target.value)}
        >
          <option value="">选择图片</option>
          {uploadedAsset && !images.some((asset) => asset.assetId === uploadedAsset.id) && (
            <option value={uploadedAsset.id}>{uploadedAsset.fileName || "已上传图片"}</option>
          )}
          {images.map((asset) => (
            <option key={asset.id} value={asset.assetId}>
              {asset.label}
            </option>
          ))}
        </select>
      </label>
      <label className="grid gap-1.5 text-sm">
        {replacing ? "上传替换图片" : "上传本地图片"}
        <Input
          type="file"
          aria-label={replacing ? "上传替换图片" : "上传本地图片"}
          accept="image/png,image/jpeg,image/webp"
          disabled={disabled}
          onChange={(event) => {
            const file = event.target.files?.[0];
            event.target.value = "";
            if (file) onUpload(file);
          }}
        />
      </label>
    </div>
  );
}

function FrameRunBody({
  segment,
  dramaId,
  episodeNo,
  canvasId,
  assets,
  actions,
}: Omit<FrameRunDrawerProps, "open" | "onOpenChange"> & { segment: Segment }) {
  const [overrideRun, setOverrideRun] = useState<{
    run: SegmentFrameRun;
    replacedId: string | null;
  } | null>(null);
  const incomingRun = segment.frameRun ?? null;
  const run = !overrideRun
    ? incomingRun
    : incomingRun &&
        incomingRun.id === overrideRun.run.id &&
        incomingRun.revision > overrideRun.run.revision
      ? incomingRun
      : incomingRun &&
          incomingRun.id !== overrideRun.run.id &&
          incomingRun.id !== overrideRun.replacedId
        ? incomingRun
        : overrideRun.run;
  const [newRound, setNewRound] = useState(false);
  const [source, setSource] = useState<FrameSource>("existing");
  const [assetId, setAssetId] = useState("");
  const [uploadedAsset, setUploadedAsset] = useState<AssetDto | null>(null);
  const [imageModelId, setImageModelId] = useState("");
  const [imagePrompt, setImagePrompt] = useState(segment.prompt);
  const [originCanvasId, setOriginCanvasId] = useState(canvasId);
  const [canvases, setCanvases] = useState<CanvasListItemDto[]>([]);
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const [error, setError] = useState<string | null>(null);
  const [replacing, setReplacing] = useState(false);
  const pendingCreate = useRef<{ signature: string; input: CreateFrameRunInput } | null>(null);
  const imageModels = useModelsStore((state) => state.byKind.image?.models ?? EMPTY_MODELS);
  const videoModels = useModelsStore((state) => state.byKind.video?.models ?? EMPTY_MODELS);
  const model = videoModels.find((item) => item.key === I2V_MODEL);
  const durations = useMemo(() => durationOptions(model?.capabilities?.params?.duration), [model]);
  const resolutions = useMemo(() => {
    const values = model?.capabilities?.params?.resolution?.options?.map(String).filter(Boolean);
    return values?.length ? values : ["720P", "1080P"];
  }, [model]);
  const defaultDuration = Number(model?.capabilities?.params?.duration?.default);
  const defaultResolution = String(model?.capabilities?.params?.resolution?.default ?? "");
  const confirmationKey = run ? `${run.id}:${run.confirmedVersion}` : "";
  const [formDraft, setFormDraft] = useState<{
    key: string;
    prompt?: string;
    duration?: number;
    resolution?: string;
    adoptIfEmpty?: boolean;
  } | null>(null);
  const scopedDraft = formDraft?.key === confirmationKey ? formDraft : null;
  const updateDraft = (patch: {
    prompt?: string;
    duration?: number;
    resolution?: string;
    adoptIfEmpty?: boolean;
  }) =>
    setFormDraft((previous) => ({
      ...(previous?.key === confirmationKey ? previous : {}),
      key: confirmationKey,
      ...patch,
    }));
  const prompt = scopedDraft?.prompt ?? String(run?.confirmedInput?.prompt ?? segment.prompt);
  const rawDuration =
    scopedDraft?.duration ??
    (run?.confirmedInput?.duration !== undefined
      ? Number(run.confirmedInput.duration)
      : durations.includes(defaultDuration)
        ? defaultDuration
        : durations[0]);
  const duration = durations.includes(rawDuration) ? rawDuration : durations[0];
  const rawResolution =
    scopedDraft?.resolution ??
    (run?.confirmedInput?.resolution !== undefined
      ? String(run.confirmedInput.resolution)
      : resolutions.includes(defaultResolution)
        ? defaultResolution
        : resolutions[0]);
  const resolution = resolutions.includes(rawResolution) ? rawResolution : resolutions[0];
  const adoptIfEmpty = scopedDraft?.adoptIfEmpty ?? run?.adoptIfEmpty ?? true;
  const [quoteState, setQuoteState] = useState<{
    key: string;
    quote?: TaskQuote;
    loading?: boolean;
    expired?: boolean;
    error?: string;
  } | null>(null);
  const quoteSeq = useRef(0);

  useEffect(() => {
    void useModelsStore.getState().load("image");
    void useModelsStore.getState().load("video");
  }, []);
  useEffect(() => {
    if (source !== "canvas") return;
    let active = true;
    const loadCanvases = async () => {
      const all: CanvasListItemDto[] = [];
      let page = 1;
      do {
        const response = await getCanvasList({ page, page_size: 100 });
        all.push(...response.items);
        if (!response.nextCursor) break;
        page = Number(response.nextCursor);
      } while (active);
      if (active) setCanvases(all);
    };
    void loadCanvases().catch((e: unknown) => {
      if (active) setError(dramaErrorMessage(e, "画布列表加载失败"));
    });
    return () => {
      active = false;
    };
  }, [source]);

  const incomingRunIdRef = useRef<string | null>(incomingRun?.id ?? null);
  useEffect(() => {
    incomingRunIdRef.current = incomingRun?.id ?? null;
  }, [incomingRun?.id]);
  const receiveRun = useCallback((next: SegmentFrameRun | null) => {
    if (!next) return;
    setOverrideRun((previous) => {
      if (previous?.run.id === next.id && previous.run.revision > next.revision) return previous;
      return {
        run: next,
        replacedId:
          previous && previous.run.id === next.id ? previous.replacedId : incomingRunIdRef.current,
      };
    });
  }, []);
  const refresh = useCallback(async () => {
    const next = await actions.refresh(segment.id, run?.id);
    receiveRun(next);
    return next;
  }, [actions, receiveRun, run?.id, segment.id]);
  const runState = run?.state;
  useEffect(() => {
    if (!runState || !["frame_pending", "submitting", "video_pending"].includes(runState)) return;
    const timer = setInterval(() => {
      void refresh().catch((e: unknown) => setError(dramaErrorMessage(e, "刷新首帧状态失败")));
    }, 2000);
    return () => clearInterval(timer);
  }, [refresh, runState]);

  const frameAssetId = run?.frameAssetId;
  const input = useMemo(
    () => ({
      op: "i2v",
      aspect_ratio: "16:9",
      count: 1,
      images: frameAssetId
        ? [/^\d+$/.test(frameAssetId) ? Number(frameAssetId) : frameAssetId]
        : [],
      prompt,
      duration,
      resolution,
    }),
    [duration, prompt, resolution, frameAssetId],
  );
  const quoteKey =
    run && ["frame_ready", "confirmed"].includes(run.state) && run.frameAssetId && run.frameSha256
      ? `${run.id}:${run.frameSha256}:${JSON.stringify(input)}`
      : "";
  const [expiredConfirmed, setExpiredConfirmed] = useState<{
    runId: string;
    version: number;
  } | null>(null);
  const [confirmedDeadline, setConfirmedDeadline] = useState<{
    runId: string;
    version: number;
    expiresAtMs: number;
  } | null>(null);
  const refreshQuote = useCallback(async () => {
    if (!quoteKey) return;
    const seq = ++quoteSeq.current;
    setQuoteState((previous) => ({
      key: quoteKey,
      quote: previous?.key === quoteKey ? previous.quote : undefined,
      expired: previous?.key === quoteKey ? previous.expired : undefined,
      loading: true,
    }));
    try {
      const quote = await quoteSegment(segment.id, { modelId: I2V_MODEL, input });
      if (seq === quoteSeq.current) setQuoteState({ key: quoteKey, quote, expired: false });
    } catch (e) {
      if (seq === quoteSeq.current)
        setQuoteState((previous) => ({
          key: quoteKey,
          expired: isQuoteExpiredError(e) || (previous?.key === quoteKey && !!previous.expired),
          error: isQuoteExpiredError(e)
            ? "报价已过期，请刷新报价"
            : dramaErrorMessage(e, "获取报价失败"),
        }));
    }
  }, [input, quoteKey, segment.id]);
  useEffect(() => {
    if (!quoteKey) return;
    const timer = setTimeout(() => {
      void refreshQuote();
    }, 80);
    return () => {
      clearTimeout(timer);
    };
  }, [quoteKey, refreshQuote]);
  const currentQuote = quoteState?.key === quoteKey ? quoteState : null;
  const expiresAt = currentQuote?.quote?.expiresAt;
  const hasOwnConfirmedDeadline =
    !!run &&
    confirmedDeadline?.runId === run.id &&
    confirmedDeadline.version === run.confirmedVersion;
  useEffect(() => {
    if (!confirmedDeadline) return;
    const delay = confirmedDeadline.expiresAtMs - Date.now();
    if (!Number.isFinite(delay) || delay > 2_147_483_647) return;
    const timer = setTimeout(
      () => {
        setExpiredConfirmed({ runId: confirmedDeadline.runId, version: confirmedDeadline.version });
      },
      Math.max(0, delay),
    );
    return () => clearTimeout(timer);
  }, [confirmedDeadline]);
  useEffect(() => {
    if (!expiresAt) return;
    const delay = Date.parse(expiresAt) - Date.now();
    if (!Number.isFinite(delay) || delay > 2_147_483_647) return;
    const timer = setTimeout(
      () => {
        setQuoteState((previous) =>
          previous?.key === quoteKey ? { ...previous, expired: true } : previous,
        );
        if (!hasOwnConfirmedDeadline && run?.state === "confirmed" && run.confirmedVersion > 0) {
          setExpiredConfirmed({ runId: run.id, version: run.confirmedVersion });
        }
      },
      Math.max(0, delay),
    );
    return () => clearTimeout(timer);
  }, [expiresAt, hasOwnConfirmedDeadline, quoteKey, run?.confirmedVersion, run?.id, run?.state]);
  const confirmedExpired =
    !!run &&
    ["confirmed", "submitting"].includes(run.state) &&
    expiredConfirmed?.runId === run.id &&
    expiredConfirmed.version === run.confirmedVersion;
  const expired = currentQuote?.expired;

  /** 失败保留输入；冲突读取最新流程，报价过期必须由用户重新确认。 */
  const perform = async (operation: () => Promise<unknown>) => {
    if (busyRef.current) return;
    busyRef.current = true;
    setBusy(true);
    setError(null);
    try {
      await operation();
    } catch (e) {
      setError(
        isQuoteExpiredError(e)
          ? "报价已过期，请刷新报价"
          : e instanceof ApiError
            ? dramaErrorMessage(e)
            : e instanceof Error
              ? e.message
              : "首帧控镜操作失败",
      );
      if (isQuoteExpiredError(e)) {
        setQuoteState({ key: quoteKey, expired: true });
        if (run?.state === "confirmed" && run.confirmedVersion > 0) {
          setExpiredConfirmed({ runId: run.id, version: run.confirmedVersion });
        }
      }
      if (e instanceof ApiError && e.status === 409) {
        try {
          await refresh();
        } catch {
          /** 原始冲突保留在面板内，刷新失败不覆盖它。 */
        }
      }
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };
  const createFromSource = async (uploadedId?: string) => {
    const body = {
      segmentRevision: segment.revision,
      frameSource: source,
      adoptIfEmpty,
      ...(source === "existing"
        ? { frameAssetId: uploadedId ?? assetId }
        : source === "generate"
          ? {
              modelId: imageModelId || imageModels[0]?.key,
              input: { op: "t2i", prompt: imagePrompt, aspect_ratio: "16:9", count: 1 },
            }
          : { originCanvasId }),
    };
    const signature = JSON.stringify(body);
    if (pendingCreate.current?.signature !== signature)
      pendingCreate.current = { signature, input: { ...body, requestKey: crypto.randomUUID() } };
    const next = await actions.create(segment.id, pendingCreate.current.input);
    pendingCreate.current = null;
    receiveRun(next);
    setNewRound(false);
  };
  const start = () => perform(() => createFromSource());
  const frameTicketFields = (target: SegmentFrameRun) => ({
    ...(target.originCanvasId ? { originCanvasId: target.originCanvasId } : {}),
    ...(target.canvasTicket ? { canvasTicket: target.canvasTicket } : {}),
  });
  const setFrame = (id: string) =>
    perform(async () => {
      if (!run) return;
      receiveRun(
        await actions.setFrame(segment.id, run.id, {
          revision: run.revision,
          frameAssetId: id,
          ...frameTicketFields(run),
        }),
      );
      setReplacing(false);
      setAssetId("");
    });
  const upload = (file: File, replace: boolean) =>
    perform(async () => {
      if (!["image/png", "image/jpeg", "image/webp"].includes(file.type))
        throw new Error("请选择 PNG、JPEG 或 WebP 图片");
      const asset = await uploadAsset(file);
      setUploadedAsset(asset);
      setAssetId(asset.id);
      if (replace && run) {
        receiveRun(
          await actions.setFrame(segment.id, run.id, {
            revision: run.revision,
            frameAssetId: asset.id,
            ...frameTicketFields(run),
          }),
        );
        setReplacing(false);
      } else await createFromSource(asset.id);
    });
  const creating = !run || newRound;
  const editable = !!run && ["frame_ready", "confirmed"].includes(run.state);
  const matchesConfirmed =
    !!run?.confirmedInput &&
    ["prompt", "duration", "resolution", "op", "aspect_ratio", "count", "images"].every(
      (key) =>
        normalizeConfirmedField(key, run.confirmedInput?.[key]) ===
        normalizeConfirmedField(key, input[key as keyof typeof input]),
    ) &&
    run.adoptIfEmpty === adoptIfEmpty;
  const canSubmit =
    !!run &&
    ["confirmed", "submitting"].includes(run.state) &&
    run.confirmedVersion > 0 &&
    matchesConfirmed &&
    !expired &&
    !confirmedExpired;
  const canStart =
    source === "existing"
      ? !!assetId
      : source === "generate"
        ? !!(imageModelId || imageModels[0]?.key) && !!imagePrompt.trim()
        : !!originCanvasId;
  const canvasParams = run
    ? new URLSearchParams({
        drama_id: dramaId,
        episode_no: String(episodeNo),
        segment_id: segment.id,
        frame_run_id: run.id,
        frame_run_revision: String(run.revision),
        canvas_ticket: run.canvasTicket ?? "",
      })
    : null;

  return (
    <div className="min-h-0 flex-1 space-y-6 overflow-y-auto px-5 pb-6">
      {run && (
        <span role="status" className="bg-chrome-hover inline-flex rounded-full px-3 py-1 text-sm">
          {STATE_LABELS[run.state]}
          {run.state === "confirmed" ? ` v${run.confirmedVersion}` : ""}
        </span>
      )}
      {(error || run?.errorMessage) && (
        <p role="alert" className="text-destructive text-sm">
          {error || run?.errorMessage}
        </p>
      )}
      {creating && (
        <section className="space-y-4" aria-label="首帧来源">
          <div className="flex flex-wrap gap-2">
            {(
              [
                ["existing", "选已有图片"],
                ["generate", "生成首帧图"],
                ["canvas", "打开画布构图"],
              ] as const
            ).map(([value, label]) => (
              <Button
                key={value}
                type="button"
                variant={source === value ? "secondary" : "ghost"}
                aria-pressed={source === value}
                disabled={busy}
                onClick={() => {
                  setSource(value);
                  setError(null);
                }}
              >
                {label}
              </Button>
            ))}
          </div>
          {source === "existing" && (
            <ImagePicker
              assets={assets}
              value={assetId}
              onChange={setAssetId}
              onUpload={(file) => void upload(file, false)}
              disabled={busy}
              uploadedAsset={uploadedAsset}
            />
          )}
          {source === "generate" && (
            <>
              <label className="grid gap-1.5">
                图片模型
                <select
                  aria-label="图片模型"
                  className={SELECT_CLASS}
                  value={imageModelId || imageModels[0]?.key || ""}
                  disabled={busy}
                  onChange={(event) => setImageModelId(event.target.value)}
                >
                  {imageModels.map((item) => (
                    <option key={item.key} value={item.key}>
                      {item.label}
                    </option>
                  ))}
                </select>
              </label>
              <label className="grid gap-1.5">
                首帧提示词
                <Textarea
                  aria-label="首帧提示词"
                  className="max-h-36 overflow-y-auto"
                  value={imagePrompt}
                  disabled={busy}
                  onChange={(e) => setImagePrompt(e.target.value)}
                />
              </label>
            </>
          )}
          {source === "canvas" && (
            <label className="grid gap-1.5">
              构图画布
              <select
                aria-label="构图画布"
                className={SELECT_CLASS}
                value={originCanvasId}
                disabled={busy}
                onChange={(e) => setOriginCanvasId(e.target.value)}
              >
                {!canvases.some((canvas) => canvas.id === canvasId) && (
                  <option value={canvasId}>短剧画布</option>
                )}
                {canvases.map((canvas) => (
                  <option key={canvas.id} value={canvas.id}>
                    {canvas.title}
                  </option>
                ))}
              </select>
            </label>
          )}
          <Button disabled={busy || !canStart} onClick={() => void start()}>
            {busy && <Loader2 className="animate-spin" />}开始首帧控镜
          </Button>
        </section>
      )}
      {run && !creating && (
        <>
          {!isTerminal(run) &&
            run.frameSource === "canvas" &&
            run.originCanvasId &&
            run.canvasTicket && (
              <a
                href={`/canvas/${encodeURIComponent(run.originCanvasId)}?${canvasParams}`}
                target="_blank"
                rel="noreferrer"
                className="text-primary inline-flex min-h-10 items-center underline underline-offset-4"
              >
                打开画布构图
              </a>
            )}
          <Button variant="outline" disabled={busy} onClick={() => void perform(refresh)}>
            <RefreshCw />
            刷新首帧状态
          </Button>
          <FramePreview run={run} />
          {["draft", "frame_ready", "confirmed"].includes(run.state) && (
            <section className="space-y-3">
              <Button
                variant="outline"
                disabled={busy}
                onClick={() => {
                  setAssetId("");
                  setReplacing((value) => !value);
                }}
              >
                替换首帧
              </Button>
              {replacing && (
                <>
                  {run.confirmedVersion > 0 && (
                    <p role="status" className="text-status-warning text-sm">
                      替换首帧将使此前确认失效，需要重新确认首帧与报价
                    </p>
                  )}
                  <ImagePicker
                    replacing
                    assets={assets}
                    value={assetId}
                    onChange={setAssetId}
                    onUpload={(file) => void upload(file, true)}
                    disabled={busy}
                    uploadedAsset={uploadedAsset}
                  />
                  <Button disabled={busy || !assetId} onClick={() => void setFrame(assetId)}>
                    使用替换图片
                  </Button>
                </>
              )}
            </section>
          )}
          {(editable || run.state === "submitting") && (
            <section className="space-y-4" aria-label="首帧 I2V 确认">
              <p className="font-medium">{model?.label ?? "首帧 I2V"} · 16:9</p>
              <fieldset disabled={busy || run.state === "submitting"} className="space-y-4">
                <label className="grid gap-1.5">
                  视频提示词
                  <Textarea
                    aria-label="视频提示词"
                    value={prompt}
                    className="max-h-36 overflow-y-auto"
                    onChange={(e) => updateDraft({ prompt: e.target.value })}
                  />
                </label>
                <div className="grid grid-cols-2 gap-3">
                  <label className="grid gap-1.5">
                    时长
                    <select
                      aria-label="I2V 时长"
                      value={duration}
                      className={SELECT_CLASS}
                      onChange={(e) => updateDraft({ duration: Number(e.target.value) })}
                    >
                      {durations.map((seconds) => (
                        <option key={seconds} value={seconds}>
                          {seconds}s
                        </option>
                      ))}
                    </select>
                  </label>
                  <label className="grid gap-1.5">
                    分辨率
                    <select
                      aria-label="I2V 分辨率"
                      value={resolution}
                      className={SELECT_CLASS}
                      onChange={(e) => updateDraft({ resolution: e.target.value })}
                    >
                      {resolutions.map((value) => (
                        <option key={value}>{value}</option>
                      ))}
                    </select>
                  </label>
                </div>
                <label className="flex min-h-10 items-center gap-2">
                  <input
                    type="checkbox"
                    checked={adoptIfEmpty}
                    onChange={(e) => updateDraft({ adoptIfEmpty: e.target.checked })}
                  />
                  当前未采用时自动采用
                </label>
              </fieldset>
              {currentQuote?.quote && (
                <p role="status">
                  预计 {currentQuote.quote.credits} 积分 · {currentQuote.quote.rawSize}
                </p>
              )}
              {(expired || confirmedExpired || currentQuote?.error) && (
                <p role="alert" className="text-destructive">
                  {expired || confirmedExpired ? "报价已过期，请刷新报价" : currentQuote?.error}
                </p>
              )}
              {editable && (
                <div className="flex flex-wrap gap-2">
                  <Button
                    variant="outline"
                    disabled={busy || currentQuote?.loading}
                    onClick={() => void refreshQuote()}
                  >
                    {currentQuote?.loading && <Loader2 className="animate-spin" />}刷新报价
                  </Button>
                  <Button
                    disabled={busy || !currentQuote?.quote || !!expired || !prompt.trim()}
                    onClick={() =>
                      void perform(async () => {
                        if (!currentQuote?.quote || !run.frameSha256) return;
                        const quoteDeadlineMs = Date.parse(currentQuote.quote.expiresAt);
                        const confirmedRun = await actions.confirm(segment.id, run.id, {
                          revision: run.revision,
                          segmentRevision: segment.revision,
                          frameAssetId: run.frameAssetId,
                          frameSha256: run.frameSha256,
                          adoptIfEmpty,
                          modelId: I2V_MODEL,
                          input,
                          quoteToken: currentQuote.quote.quoteToken,
                        });
                        setExpiredConfirmed(null);
                        setConfirmedDeadline(
                          Number.isFinite(quoteDeadlineMs) && confirmedRun.confirmedVersion > 0
                            ? {
                                runId: confirmedRun.id,
                                version: confirmedRun.confirmedVersion,
                                expiresAtMs: quoteDeadlineMs,
                              }
                            : null,
                        );
                        receiveRun(confirmedRun);
                      })
                    }
                  >
                    确认首帧与报价
                  </Button>
                </div>
              )}
              <Button
                disabled={busy || !canSubmit}
                onClick={() =>
                  void perform(async () => {
                    receiveRun(
                      (
                        await actions.submit(segment.id, run.id, {
                          revision: run.revision,
                          confirmedVersion: run.confirmedVersion,
                        })
                      ).run,
                    );
                  })
                }
              >
                提交首帧 I2V 生成
              </Button>
            </section>
          )}
          {!isTerminal(run) && (
            <Button
              variant="destructive"
              disabled={busy}
              onClick={() =>
                void perform(async () =>
                  receiveRun(await actions.cancel(segment.id, run.id, { revision: run.revision })),
                )
              }
            >
              取消控镜流程
            </Button>
          )}
          {isTerminal(run) && (
            <Button
              variant="outline"
              disabled={busy}
              onClick={() => {
                setNewRound(true);
                setError(null);
                setAssetId("");
              }}
            >
              新建一轮首帧控镜
            </Button>
          )}
        </>
      )}
    </div>
  );
}

/** 打开时挂载独立编辑会话，关闭动画由常驻 Sheet 管理。 */
export function FrameRunDrawer(props: FrameRunDrawerProps) {
  return (
    <Sheet open={props.open} onOpenChange={props.onOpenChange}>
      <SheetContent className="bg-chrome w-full gap-3 backdrop-blur-xl sm:max-w-xl">
        <SheetHeader className="px-5 pt-5">
          <SheetTitle>首帧控镜</SheetTitle>
          <SheetDescription>片段 {props.segment ? props.segment.seq + 1 : ""}</SheetDescription>
        </SheetHeader>
        {props.open && props.segment && (
          <FrameRunBody key={props.segment.id} {...props} segment={props.segment} />
        )}
      </SheetContent>
    </Sheet>
  );
}
