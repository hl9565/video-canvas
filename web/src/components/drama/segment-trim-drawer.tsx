import { useEffect, useId, useRef, useState } from "react";
import { AlertTriangle, Loader2, Pause, Play, RotateCcw, VolumeX } from "lucide-react";

import { Filmstrip, MuteButton, Playhead, RangeHandles } from "@/components/canvas/trim-panel";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { Slider } from "@/components/ui/slider";
import { Switch } from "@/components/ui/switch";
import { useTrimSource } from "@/hooks/use-trim-source";
import { cn } from "@/lib/utils";
import type { Segment, SegmentEdit, SegmentTake, UpdateSegmentInput } from "@/api/drama/type";
import {
  AUDIO_GAIN_MAX_DB,
  AUDIO_GAIN_MIN_DB,
  AV_PTS_OFFSET_MAX_MS,
  AV_PTS_OFFSET_MIN_MS,
  DEFAULT_SEGMENT_EDIT,
  SEGMENT_TRIM_MIN_GAP,
  buildSegmentTrimPatch,
  clampAudioGainDb,
  clampAvPtsOffsetMs,
  clampFrame,
  effectiveOutFrame,
  formatFrameLabel,
  formatFramesDuration,
  formatTimecode24,
  frameToSeekSeconds,
  framesToSeconds,
  isFullLengthRange,
  lastPreviewFrame,
  normalizeSegmentEdit,
  resolveTotalFrames,
  restoreFullLengthEdit,
  segmentEditOf,
  timeToFrameIndex,
  trimFrameRange,
  validateSegmentEdit,
} from "@/utils/drama/segment-trim";
import { chosenTake, playableTake } from "@/utils/drama/segments";

export type TrimSourceEngine = Parameters<typeof useTrimSource>[1];

export interface SegmentTrimDrawerProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  segment: Segment | null;
  /** 切换 take 时传入目标 taskId；与 segment.chosenTaskId 不同时展示告警与重置/保留开关 */
  pendingTaskId?: string | null;
  /** 外部覆盖是否有原声（缺省读取 segment.hasAudio 与源视频探测） */
  hasAudio?: boolean;
  /** 外部覆盖预览视频 URL（缺省取目标 take 或当前采用 take） */
  sourceUrl?: string;
  /** 测试注入用的 Mediabunny 引擎 */
  trimSourceEngine?: TrimSourceEngine;
  /** 保存回调（由父层调 PATCH /api/v1/drama-segments/:sid；绝不发起前端重编码） */
  onSave: (
    input: Omit<UpdateSegmentInput, "revision">,
    options?: { expectedRevision?: number },
  ) => Promise<unknown> | unknown;
  /** 父层捕获到的并发冲突提示（例如 409） */
  conflictMessage?: string | null;
  /** 外部受控保存中状态 */
  saving?: boolean;
}

function resolveTargetTake(segment: Segment, pendingTaskId?: string | null): SegmentTake | null {
  if (pendingTaskId) {
    const candidate = segment.takes.find((item) => item.taskId === pendingTaskId);
    if (candidate?.asset) return candidate;
  }
  return playableTake(segment) ?? chosenTake(segment);
}

function errorMessageOf(err: unknown): string {
  if (err instanceof Error && err.message.trim() !== "") return err.message;
  if (typeof err === "string" && err.trim() !== "") return err;
  return "保存剪辑参数失败";
}

function SegmentTrimDrawerBody({
  segment,
  pendingTaskId,
  hasAudioProp,
  sourceUrlProp,
  trimSourceEngine,
  onSave,
  conflictMessage,
  savingProp,
  onClose,
}: {
  segment: Segment;
  pendingTaskId?: string | null;
  hasAudioProp?: boolean;
  sourceUrlProp?: string;
  trimSourceEngine?: TrimSourceEngine;
  onSave: SegmentTrimDrawerProps["onSave"];
  conflictMessage?: string | null;
  savingProp?: boolean;
  onClose: () => void;
}) {
  const uid = useId();
  const videoRef = useRef<HTMLVideoElement | null>(null);
  const initialSeekDoneRef = useRef(false);

  const [baseline, setBaseline] = useState(() => ({
    revision: segment.revision,
    taskId: segment.chosenTaskId,
    rawEdit: segmentEditOf(segment),
  }));
  const externalRevisionChanged = segment.revision !== baseline.revision;
  const externalTaskChanged = segment.chosenTaskId !== baseline.taskId;

  const isTakeSwitch = Boolean(pendingTaskId && pendingTaskId !== baseline.taskId);
  const showSwitchWarning =
    isTakeSwitch || externalTaskChanged || externalRevisionChanged || Boolean(conflictMessage);

  const targetTake = resolveTargetTake(segment, pendingTaskId);
  const videoUrl = sourceUrlProp ?? targetTake?.asset?.url ?? undefined;
  const trimSource = useTrimSource(videoUrl, trimSourceEngine);

  const probedDuration = trimSource.status === "ready" ? trimSource.probe.duration : null;
  const thumbs = trimSource.status === "ready" ? trimSource.thumbs : [];
  const totalFrames = resolveTotalFrames(
    isTakeSwitch ? null : segment.sourceFrameCount,
    probedDuration ?? segment.seconds,
  );
  const effectiveDuration =
    probedDuration && probedDuration > 0
      ? probedDuration
      : totalFrames > 0
        ? framesToSeconds(totalFrames)
        : segment.seconds;

  const targetMatchesChosen =
    !isTakeSwitch && (!targetTake || targetTake.taskId === segment.chosenTaskId);
  const effectiveHasAudio =
    hasAudioProp !== undefined
      ? hasAudioProp
      : targetMatchesChosen && segment.hasAudio !== undefined
        ? segment.hasAudio
        : trimSource.status === "ready"
          ? trimSource.probe.hasAudio
          : true;

  const baselineEdit = normalizeSegmentEdit(baseline.rawEdit, totalFrames || undefined);

  const [resetOnTaskChange, setResetOnTaskChange] = useState<boolean>(isTakeSwitch);
  const [draft, setDraft] = useState<SegmentEdit>(() =>
    isTakeSwitch ? { ...DEFAULT_SEGMENT_EDIT } : baselineEdit,
  );
  const [playheadFrame, setPlayheadFrame] = useState<number>(() =>
    isTakeSwitch ? DEFAULT_SEGMENT_EDIT.sourceInF : baselineEdit.sourceInF,
  );
  const [playing, setPlaying] = useState(false);
  const [savingInternal, setSavingInternal] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);

  const [inText, setInText] = useState(() => String(draft.sourceInF));
  const [outText, setOutText] = useState(() =>
    String(effectiveOutFrame(draft.sourceOutF, totalFrames)),
  );
  const [gainText, setGainText] = useState(() => String(draft.audioGainDb));
  const [ptsText, setPtsText] = useState(() => String(draft.avPtsOffsetMs));
  const prevTotalFramesRef = useRef(totalFrames);

  const saving = savingProp ?? savingInternal;
  const effectiveOut = effectiveOutFrame(draft.sourceOutF, totalFrames);
  const maxPreviewFrame = lastPreviewFrame(draft.sourceInF, draft.sourceOutF, totalFrames);
  const validation = validateSegmentEdit(draft, totalFrames);

  // 总帧数从探测延迟到达时同步修正默认出点文本，并钳制当前草稿
  useEffect(() => {
    if (prevTotalFramesRef.current === totalFrames) return;
    prevTotalFramesRef.current = totalFrames;
    const clamped = normalizeSegmentEdit(draft, totalFrames || undefined);
    setDraft(clamped);
    setInText(String(clamped.sourceInF));
    setOutText(String(effectiveOutFrame(clamped.sourceOutF, totalFrames)));
  }, [totalFrames, draft]);

  // 将静音与音量状态同步到预览视频标签
  useEffect(() => {
    const video = videoRef.current;
    if (!video) return;
    try {
      video.muted = !effectiveHasAudio || draft.muteOriginal;
      video.volume =
        draft.audioGainDb >= 0 ? 1 : Math.min(Math.max(Math.pow(10, draft.audioGainDb / 20), 0), 1);
    } catch {
      // jsdom 等环境对媒体属性可能只读或限制范围，忽略预览音量同步异常
    }
  }, [effectiveHasAudio, draft.muteOriginal, draft.audioGainDb]);

  const seekVideoToFrame = (frame: number) => {
    const upper = Math.max(0, totalFrames - 1);
    const targetFrame = Math.min(Math.max(Math.round(frame), 0), upper);
    setPlayheadFrame(targetFrame);
    const video = videoRef.current;
    if (!video) return;
    const seekSec = frameToSeekSeconds(targetFrame, totalFrames, effectiveDuration);
    try {
      if (Number.isFinite(seekSec)) {
        video.currentTime = seekSec;
      }
    } catch {
      // 忽略尚未加载元数据时的 currentTime 设置异常
    }
  };

  // 打开已有非零入点的精裁草稿时，等视频元数据就绪后先将真实 currentTime 定位到入点
  useEffect(() => {
    initialSeekDoneRef.current = false;
  }, [videoUrl]);

  const syncInitialVideoPosition = () => {
    if (initialSeekDoneRef.current || totalFrames <= 0) return;
    const video = videoRef.current;
    if (!video) return;
    initialSeekDoneRef.current = true;
    seekVideoToFrame(draft.sourceInF);
  };

  useEffect(() => {
    const video = videoRef.current;
    if (!video || initialSeekDoneRef.current || totalFrames <= 0) return;
    if (video.readyState >= 1) {
      syncInitialVideoPosition();
    }
  });

  const safePause = (video: HTMLVideoElement | null) => {
    if (!video) return;
    try {
      video.pause?.();
    } catch {
      // jsdom 等无媒体解码实现环境下 pause 可能抛 Not implemented
    }
  };

  const applyRange = (nextIn: number, nextOut: number | null, which: "start" | "end") => {
    const video = videoRef.current;
    if (video && !video.paused) {
      safePause(video);
      setPlaying(false);
    }
    const nextRange = trimFrameRange(nextIn, nextOut, totalFrames, which, SEGMENT_TRIM_MIN_GAP);
    setDraft((prev) => ({
      ...prev,
      sourceInF: nextRange.sourceInF,
      sourceOutF: nextRange.sourceOutF,
    }));
    setInText(String(nextRange.sourceInF));
    setOutText(String(effectiveOutFrame(nextRange.sourceOutF, totalFrames)));
    if (isTakeSwitch && resetOnTaskChange) {
      setResetOnTaskChange(false);
    }

    const previewTarget =
      which === "start"
        ? nextRange.sourceInF
        : lastPreviewFrame(nextRange.sourceInF, nextRange.sourceOutF, totalFrames);
    seekVideoToFrame(previewTarget);
  };

  const updateAudioField = (patch: Partial<SegmentEdit>) => {
    setDraft((prev) => ({ ...prev, ...patch }));
    if (isTakeSwitch && resetOnTaskChange) {
      setResetOnTaskChange(false);
    }
  };

  const handleRestoreFullLength = () => {
    const video = videoRef.current;
    if (video && !video.paused) {
      safePause(video);
      setPlaying(false);
    }
    setDraft((prev) => restoreFullLengthEdit(prev));
    setInText("0");
    setOutText(String(totalFrames));
    if (isTakeSwitch && resetOnTaskChange) {
      setResetOnTaskChange(false);
    }
    seekVideoToFrame(0);
  };

  const handleReloadLatestBaseline = () => {
    const freshRaw = segmentEditOf(segment);
    const freshEdit = normalizeSegmentEdit(freshRaw, totalFrames || undefined);
    const freshTakeSwitch = Boolean(pendingTaskId && pendingTaskId !== segment.chosenTaskId);
    const nextDraft = freshTakeSwitch ? { ...DEFAULT_SEGMENT_EDIT } : freshEdit;
    setBaseline({
      revision: segment.revision,
      taskId: segment.chosenTaskId,
      rawEdit: freshRaw,
    });
    setResetOnTaskChange(freshTakeSwitch);
    setDraft(nextDraft);
    setInText(String(nextDraft.sourceInF));
    setOutText(String(effectiveOutFrame(nextDraft.sourceOutF, totalFrames)));
    setGainText(String(nextDraft.audioGainDb));
    setPtsText(String(nextDraft.avPtsOffsetMs));
    setSaveError(null);
    initialSeekDoneRef.current = true;
    seekVideoToFrame(nextDraft.sourceInF);
  };

  const commitInText = (raw: string): number => {
    const trimmed = raw.trim();
    const num = trimmed === "" ? Number.NaN : Number(trimmed);
    if (!Number.isFinite(num)) {
      setInText(String(draft.sourceInF));
      return draft.sourceInF;
    }
    const nextRange = trimFrameRange(
      num,
      draft.sourceOutF,
      totalFrames,
      "start",
      SEGMENT_TRIM_MIN_GAP,
    );
    applyRange(num, draft.sourceOutF, "start");
    return nextRange.sourceInF;
  };

  const commitOutText = (raw: string): number | null => {
    const trimmed = raw.trim();
    const num = trimmed === "" ? Number.NaN : Number(trimmed);
    if (!Number.isFinite(num)) {
      setOutText(String(effectiveOutFrame(draft.sourceOutF, totalFrames)));
      return draft.sourceOutF;
    }
    const nextRange = trimFrameRange(
      draft.sourceInF,
      num,
      totalFrames,
      "end",
      SEGMENT_TRIM_MIN_GAP,
    );
    applyRange(draft.sourceInF, num, "end");
    return nextRange.sourceOutF;
  };

  const commitGainText = (raw: string): number => {
    const trimmed = raw.trim();
    const num = trimmed === "" || trimmed === "-" ? Number.NaN : Number(trimmed);
    if (!Number.isFinite(num)) {
      setGainText(String(draft.audioGainDb));
      return draft.audioGainDb;
    }
    const clamped = clampAudioGainDb(num);
    setGainText(String(clamped));
    updateAudioField({ audioGainDb: clamped });
    return clamped;
  };

  const commitPtsText = (raw: string): number => {
    const trimmed = raw.trim();
    const num = trimmed === "" || trimmed === "-" ? Number.NaN : Number(trimmed);
    if (!Number.isFinite(num)) {
      setPtsText(String(draft.avPtsOffsetMs));
      return draft.avPtsOffsetMs;
    }
    const clamped = clampAvPtsOffsetMs(num);
    setPtsText(String(clamped));
    updateAudioField({ avPtsOffsetMs: clamped });
    return clamped;
  };

  const handleToggleResetOnSwitch = (shouldReset: boolean) => {
    setResetOnTaskChange(shouldReset);
    const targetEdit = shouldReset ? { ...DEFAULT_SEGMENT_EDIT } : baselineEdit;
    setDraft(targetEdit);
    setInText(String(targetEdit.sourceInF));
    setOutText(String(effectiveOutFrame(targetEdit.sourceOutF, totalFrames)));
    setGainText(String(targetEdit.audioGainDb));
    setPtsText(String(targetEdit.avPtsOffsetMs));
    seekVideoToFrame(targetEdit.sourceInF);
  };

  const togglePlay = () => {
    const video = videoRef.current;
    if (!video || totalFrames <= 0) return;
    if (video.paused) {
      const inSec = frameToSeekSeconds(draft.sourceInF, totalFrames, effectiveDuration);
      const outLimitSec = framesToSeconds(effectiveOut);
      const currentTime = Number.isFinite(video.currentTime) ? video.currentTime : 0;
      if (
        playheadFrame < draft.sourceInF ||
        playheadFrame > maxPreviewFrame ||
        currentTime < inSec - 0.01 ||
        currentTime >= outLimitSec - 0.01
      ) {
        seekVideoToFrame(draft.sourceInF);
      }
      initialSeekDoneRef.current = true;
      setPlaying(true);
      try {
        void video.play?.()?.catch?.(() => setPlaying(false));
      } catch {
        setPlaying(false);
      }
    } else {
      safePause(video);
      setPlaying(false);
    }
  };

  const handleTimeUpdate = () => {
    const video = videoRef.current;
    if (!video || totalFrames <= 0) return;
    const outLimitSec = framesToSeconds(effectiveOut);
    if (video.currentTime >= outLimitSec - 1e-4) {
      safePause(video);
      setPlaying(false);
      seekVideoToFrame(draft.sourceInF);
      return;
    }
    const frame = timeToFrameIndex(video.currentTime, totalFrames);
    setPlayheadFrame(Math.min(Math.max(frame, draft.sourceInF), maxPreviewFrame));
  };

  const referenceEdit = isTakeSwitch && resetOnTaskChange ? DEFAULT_SEGMENT_EDIT : baseline.rawEdit;
  const editPatch = buildSegmentTrimPatch(referenceEdit, draft);
  const hasEditDiff = Object.keys(editPatch).length > 0;
  const externalChanged = externalRevisionChanged || externalTaskChanged;
  const hasPendingNumericInput =
    inText.trim() !== String(draft.sourceInF) ||
    outText.trim() !== String(effectiveOut) ||
    gainText.trim() !== String(draft.audioGainDb) ||
    ptsText.trim() !== String(draft.avPtsOffsetMs);
  const canSave =
    !saving &&
    validation.valid &&
    !externalRevisionChanged &&
    (isTakeSwitch || externalTaskChanged || hasEditDiff || hasPendingNumericInput);

  const handleSave = async () => {
    if (!validation.valid || saving || externalRevisionChanged) return;
    let nextDraft = draft;
    if (inText !== String(draft.sourceInF)) {
      const nextIn = commitInText(inText);
      nextDraft = { ...nextDraft, sourceInF: nextIn };
    }
    if (outText !== String(effectiveOutFrame(nextDraft.sourceOutF, totalFrames))) {
      const nextOut = commitOutText(outText);
      nextDraft = { ...nextDraft, sourceOutF: nextOut };
    }
    if (gainText !== String(nextDraft.audioGainDb)) {
      const nextGain = commitGainText(gainText);
      nextDraft = { ...nextDraft, audioGainDb: nextGain };
    }
    if (ptsText !== String(nextDraft.avPtsOffsetMs)) {
      const nextPts = commitPtsText(ptsText);
      nextDraft = { ...nextDraft, avPtsOffsetMs: nextPts };
    }
    const finalPatch = buildSegmentTrimPatch(referenceEdit, nextDraft);
    const finalHasDiff = Object.keys(finalPatch).length > 0;
    const finalValidation = validateSegmentEdit(nextDraft, totalFrames);
    if (!finalValidation.valid) return;

    setSaveError(null);
    setSavingInternal(true);
    try {
      const payload: Omit<UpdateSegmentInput, "revision"> = {};
      if (isTakeSwitch && pendingTaskId) {
        payload.chosenTaskId = pendingTaskId;
        payload.expectedTaskId = baseline.taskId || "";
        payload.resetEditOnTaskChange = resetOnTaskChange;
        if (finalHasDiff) {
          payload.edit = finalPatch;
        }
      } else {
        if (baseline.taskId) {
          payload.expectedTaskId = baseline.taskId;
        }
        if (finalHasDiff) {
          payload.edit = finalPatch;
        } else if (externalTaskChanged && resetOnTaskChange) {
          const resetPatch = buildSegmentTrimPatch(baseline.rawEdit, DEFAULT_SEGMENT_EDIT);
          if (Object.keys(resetPatch).length > 0) {
            payload.edit = resetPatch;
          }
        }
      }

      await onSave(payload, { expectedRevision: baseline.revision });
      onClose();
    } catch (err) {
      setSaveError(errorMessageOf(err));
    } finally {
      setSavingInternal(false);
    }
  };

  const fullLength = isFullLengthRange(draft);

  return (
    <>
      <SheetHeader className="border-b pr-10 pb-3">
        <SheetTitle className="flex items-center gap-2 text-base">
          <span>镜头 #{segment.seq + 1} 精裁与原声</span>
          <span className="bg-muted text-muted-foreground rounded px-1.5 py-0.5 font-mono text-[11px] font-normal">
            24fps · {totalFrames}f
          </span>
        </SheetTitle>
        <SheetDescription className="text-xs">
          左闭右开 [入点, 出点) 整数帧剪辑与原声控制，保存仅写入配置不触发重编码。
        </SheetDescription>
      </SheetHeader>

      <div className="flex flex-1 flex-col gap-4 overflow-y-auto px-4 py-3">
        {showSwitchWarning && (
          <div
            role="alert"
            className="border-amber-500/40 bg-amber-500/10 flex flex-col gap-2.5 rounded-xl border px-3.5 py-3 text-xs"
          >
            <div className="flex items-start justify-between gap-2 font-medium text-amber-900 dark:text-amber-200">
              <div className="flex items-start gap-2">
                <AlertTriangle className="mt-0.5 size-4 shrink-0 text-amber-500" />
                <div className="space-y-1">
                  <p>
                    {conflictMessage
                      ? conflictMessage
                      : externalTaskChanged
                        ? "该镜头的采用视频已在外部切换，请确认保留或重置当前精裁与原声参数后再保存。"
                        : externalRevisionChanged
                          ? `该镜头的配置已在别处修改（版本 v${baseline.revision} → v${segment.revision}），当前草稿基于旧版本，请载入最新版本后再保存。`
                          : "正在切换采用视频，默认将重置当前精裁与原声参数；如需保留现有设置可关闭下方开关。"}
                  </p>
                  {(isTakeSwitch || externalTaskChanged) && (
                    <p className="text-muted-foreground font-normal">
                      当前选择：
                      {resetOnTaskChange ? "切换时重置为全长与默认原声" : "保留当前精裁与原声参数"}
                    </p>
                  )}
                </div>
              </div>
              {externalChanged && (
                <Button
                  type="button"
                  size="sm"
                  variant="outline"
                  onClick={handleReloadLatestBaseline}
                  className="h-7 shrink-0 px-2 text-xs"
                >
                  载入最新版本
                </Button>
              )}
            </div>
            {(isTakeSwitch || externalTaskChanged) && (
              <div className="flex items-center justify-between gap-2 pl-6">
                <Label
                  htmlFor={`${uid}-reset-switch`}
                  className="cursor-pointer text-xs font-medium"
                >
                  切换时重置精裁与原声
                </Label>
                <Switch
                  id={`${uid}-reset-switch`}
                  aria-label="切换时重置精裁与原声"
                  checked={resetOnTaskChange}
                  onCheckedChange={handleToggleResetOnSwitch}
                />
              </div>
            )}
          </div>
        )}

        {/* 预览播放器 */}
        <div className="bg-muted/30 flex flex-col gap-2 rounded-xl border p-2.5">
          <div className="relative aspect-video w-full overflow-hidden rounded-lg bg-black">
            {videoUrl ? (
              <video
                ref={videoRef}
                src={videoUrl}
                playsInline
                preload="metadata"
                onLoadedMetadata={syncInitialVideoPosition}
                onTimeUpdate={handleTimeUpdate}
                onEnded={() => setPlaying(false)}
                onPause={() => setPlaying(false)}
                onPlay={() => setPlaying(true)}
                className="size-full object-contain"
              />
            ) : (
              <div className="text-muted-foreground flex size-full items-center justify-center text-xs">
                暂无可预览视频
              </div>
            )}
            <div className="pointer-events-none absolute top-2 left-2 rounded-md bg-black/70 px-2 py-0.5 font-mono text-[11px] tabular-nums text-white">
              预览帧 f{playheadFrame} · {formatTimecode24(playheadFrame)}
            </div>
          </div>

          <div className="flex items-center justify-between gap-2">
            <div className="flex items-center gap-1.5">
              <Button
                type="button"
                variant="outline"
                size="sm"
                aria-label={playing ? "暂停预览" : "播放预览"}
                disabled={!videoUrl || totalFrames <= 0}
                onClick={togglePlay}
              >
                {playing ? <Pause className="size-3.5" /> : <Play className="size-3.5" />}
                <span>{playing ? "暂停" : "播放"}</span>
              </Button>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                disabled={totalFrames <= 0}
                onClick={() => seekVideoToFrame(draft.sourceInF)}
              >
                看入点 f{draft.sourceInF}
              </Button>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                disabled={totalFrames <= 0}
                onClick={() => seekVideoToFrame(maxPreviewFrame)}
              >
                看出点前 f{maxPreviewFrame}
              </Button>
            </div>
            <span className="text-muted-foreground font-mono text-xs tabular-nums">
              {validation.trimmedFrames} 帧 · {formatFramesDuration(validation.trimmedFrames)}
            </span>
          </div>
        </div>

        {/* 24fps 整数帧胶片条时间轴 */}
        <div className="flex flex-col gap-2.5 rounded-xl border p-3">
          <div className="flex items-center justify-between gap-2">
            <span className="text-xs font-medium">
              时间选区（方向键 ±1 帧，Shift+方向键 ±10 帧）
            </span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={fullLength || totalFrames <= 0}
              onClick={handleRestoreFullLength}
            >
              <RotateCcw className="size-3.5" />
              <span>恢复全长</span>
            </Button>
          </div>

          <RangeHandles
            start={draft.sourceInF}
            end={effectiveOut}
            total={totalFrames}
            minGap={SEGMENT_TRIM_MIN_GAP}
            disabled={totalFrames <= 0}
            step={1}
            bigStep={10}
            snap={(raw) => clampFrame(raw, totalFrames)}
            formatValue={(frame) => formatFrameLabel(frame)}
            onChange={(next, which) => applyRange(next.start, next.end, which)}
            onSeek={(raw) => {
              const snapped = clampFrame(raw, totalFrames);
              const clamped = Math.min(Math.max(snapped, draft.sourceInF), maxPreviewFrame);
              seekVideoToFrame(clamped);
            }}
            className="h-12 w-full"
          >
            <Filmstrip thumbs={thumbs} />
            <Playhead value={playheadFrame} total={totalFrames} />
          </RangeHandles>

          <div className="grid grid-cols-2 gap-3 pt-1">
            <div className="flex flex-col gap-1">
              <div className="flex items-center justify-between">
                <Label htmlFor={`${uid}-in-frame`} className="text-xs">
                  入点帧 (包含)
                </Label>
                <span className="text-muted-foreground font-mono text-[11px] tabular-nums">
                  {formatTimecode24(draft.sourceInF)}
                </span>
              </div>
              <Input
                id={`${uid}-in-frame`}
                aria-label="入点帧"
                type="number"
                min={0}
                max={Math.max(0, effectiveOut - SEGMENT_TRIM_MIN_GAP)}
                step={1}
                disabled={totalFrames <= 0}
                value={inText}
                onChange={(e) => setInText(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") {
                    e.preventDefault();
                    commitInText(inText);
                  }
                }}
                onBlur={() => commitInText(inText)}
                className="h-8 font-mono text-xs tabular-nums"
              />
            </div>

            <div className="flex flex-col gap-1">
              <div className="flex items-center justify-between">
                <Label htmlFor={`${uid}-out-frame`} className="text-xs">
                  出点帧 (不含)
                </Label>
                <span className="text-muted-foreground font-mono text-[11px] tabular-nums">
                  {draft.sourceOutF === null ? "全长 (EOF)" : `末帧 f${maxPreviewFrame}`}
                </span>
              </div>
              <Input
                id={`${uid}-out-frame`}
                aria-label="出点帧"
                type="number"
                min={Math.min(totalFrames, draft.sourceInF + SEGMENT_TRIM_MIN_GAP)}
                max={totalFrames}
                step={1}
                disabled={totalFrames <= 0}
                value={outText}
                onChange={(e) => setOutText(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") {
                    e.preventDefault();
                    commitOutText(outText);
                  }
                }}
                onBlur={() => commitOutText(outText)}
                className="h-8 font-mono text-xs tabular-nums"
              />
            </div>
          </div>
        </div>

        {/* 原声与音画同步控制 */}
        <div className="flex flex-col gap-3.5 rounded-xl border p-3">
          <div className="flex items-center justify-between gap-2">
            <span className="text-xs font-medium">原声与音画参数</span>
            {!effectiveHasAudio && (
              <span className="bg-muted text-muted-foreground inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[11px] font-medium">
                <VolumeX className="size-3" />
                素材无原声
              </span>
            )}
          </div>

          <div className="flex items-center justify-between gap-2">
            <div className="space-y-0.5">
              <span className="text-xs font-medium">原声开关（作用于全长）</span>
              <p className="text-muted-foreground text-[11px]">
                {!effectiveHasAudio
                  ? "素材无原声，仅保留帧区间剪辑；保存不受影响。"
                  : draft.muteOriginal
                    ? "已全长去原声，增益设置暂不生效。"
                    : "保留源视频原声并可微调增益与音画偏移。"}
              </p>
            </div>
            <MuteButton
              variant="button"
              aria-label="原声静音"
              muted={draft.muteOriginal}
              hasAudio={effectiveHasAudio}
              noAudioLabel="素材无原声"
              mutedLabel="已静音"
              unmutedLabel="去原声"
              onToggle={(nextMuted) => updateAudioField({ muteOriginal: nextMuted })}
            />
          </div>

          <div
            className={cn(
              "flex flex-col gap-1.5",
              (!effectiveHasAudio || draft.muteOriginal) && "opacity-60",
            )}
          >
            <div className="flex items-center justify-between gap-2">
              <Label htmlFor={`${uid}-gain-input`} className="text-xs">
                原声增益 ({AUDIO_GAIN_MIN_DB}..+{AUDIO_GAIN_MAX_DB} dB)
              </Label>
              <Input
                id={`${uid}-gain-input`}
                aria-label="原声增益 (dB)"
                type="number"
                min={AUDIO_GAIN_MIN_DB}
                max={AUDIO_GAIN_MAX_DB}
                step={0.5}
                disabled={!effectiveHasAudio || draft.muteOriginal}
                value={gainText}
                onChange={(e) => setGainText(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") {
                    e.preventDefault();
                    commitGainText(gainText);
                  }
                }}
                onBlur={() => commitGainText(gainText)}
                className="h-7 w-24 text-right font-mono text-xs tabular-nums"
              />
            </div>
            <Slider
              aria-label="原声增益滑块"
              min={AUDIO_GAIN_MIN_DB}
              max={AUDIO_GAIN_MAX_DB}
              step={0.5}
              disabled={!effectiveHasAudio || draft.muteOriginal}
              value={[draft.audioGainDb]}
              onValueChange={(next) => {
                const raw = Array.isArray(next) ? next[0] : next;
                const clamped = clampAudioGainDb(raw);
                setGainText(String(clamped));
                updateAudioField({ audioGainDb: clamped });
              }}
            />
          </div>

          <div className={cn("flex flex-col gap-1.5", !effectiveHasAudio && "opacity-60")}>
            <div className="flex items-center justify-between gap-2">
              <Label htmlFor={`${uid}-pts-input`} className="text-xs">
                音画同步偏移 ({AV_PTS_OFFSET_MIN_MS}..+{AV_PTS_OFFSET_MAX_MS} ms)
              </Label>
              <Input
                id={`${uid}-pts-input`}
                aria-label="音画偏移 (ms)"
                type="number"
                min={AV_PTS_OFFSET_MIN_MS}
                max={AV_PTS_OFFSET_MAX_MS}
                step={10}
                disabled={!effectiveHasAudio}
                value={ptsText}
                onChange={(e) => setPtsText(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") {
                    e.preventDefault();
                    commitPtsText(ptsText);
                  }
                }}
                onBlur={() => commitPtsText(ptsText)}
                className="h-7 w-24 text-right font-mono text-xs tabular-nums"
              />
            </div>
            <Slider
              aria-label="音画偏移滑块"
              min={AV_PTS_OFFSET_MIN_MS}
              max={AV_PTS_OFFSET_MAX_MS}
              step={10}
              disabled={!effectiveHasAudio}
              value={[draft.avPtsOffsetMs]}
              onValueChange={(next) => {
                const raw = Array.isArray(next) ? next[0] : next;
                const clamped = clampAvPtsOffsetMs(raw);
                setPtsText(String(clamped));
                updateAudioField({ avPtsOffsetMs: clamped });
              }}
            />
          </div>

          <div className="flex items-center justify-between gap-2 pt-0.5">
            <div className="space-y-0.5">
              <Label
                htmlFor={`${uid}-dialogue-switch`}
                className="cursor-pointer text-xs font-medium"
              >
                原声属于人物对白
              </Label>
              <p className="text-muted-foreground text-[11px]">
                开启后导出成片时按人声对白轨混音并保护清晰度
              </p>
            </div>
            <Switch
              id={`${uid}-dialogue-switch`}
              aria-label="原声对白"
              checked={draft.nativeIsDialogue}
              disabled={!effectiveHasAudio}
              onCheckedChange={(checked) => updateAudioField({ nativeIsDialogue: checked })}
            />
          </div>
        </div>

        {(saveError || !validation.valid) && (
          <p role="alert" className="text-destructive text-xs">
            {saveError ?? validation.errors[0]}
          </p>
        )}
      </div>

      <SheetFooter className="flex-row items-center justify-end gap-2 border-t px-4 py-3">
        <Button type="button" variant="outline" onClick={onClose} disabled={saving}>
          取消
        </Button>
        <Button
          type="button"
          aria-label="保存剪辑"
          disabled={!canSave}
          onClick={() => void handleSave()}
        >
          {saving && <Loader2 className="size-4 animate-spin" />}
          <span>保存</span>
        </Button>
      </SheetFooter>
    </>
  );
}

/**
 * 短剧镜头 24fps 整数帧精裁与原声参数抽屉：
 * 复用画布提炼出的受控原语 Filmstrip / RangeHandles / Playhead / MuteButton；
 * 确认仅通过 onSave 写入片段配置，绝不走前端重编码。
 */
export function SegmentTrimDrawer({
  open,
  onOpenChange,
  segment,
  pendingTaskId,
  hasAudio,
  sourceUrl,
  trimSourceEngine,
  onSave,
  conflictMessage,
  saving,
}: SegmentTrimDrawerProps) {
  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent side="right" className="w-full gap-0 p-0 data-[side=right]:sm:max-w-[540px]">
        {open && segment ? (
          <SegmentTrimDrawerBody
            key={`${segment.id}:${pendingTaskId ?? ""}`}
            segment={segment}
            pendingTaskId={pendingTaskId}
            hasAudioProp={hasAudio}
            sourceUrlProp={sourceUrl}
            trimSourceEngine={trimSourceEngine}
            onSave={onSave}
            conflictMessage={conflictMessage}
            savingProp={saving}
            onClose={() => onOpenChange(false)}
          />
        ) : null}
      </SheetContent>
    </Sheet>
  );
}
