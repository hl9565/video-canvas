import { DEFAULT_SEGMENT_EDIT, segmentEditOf } from "@/api/drama";
import type { SegmentEdit, SegmentEditPatch } from "@/api/drama/type";

export { DEFAULT_SEGMENT_EDIT, segmentEditOf };
export type { SegmentEdit, SegmentEditPatch };

/** @deprecated 请直接使用 SegmentEdit */
export type SegmentEditValue = SegmentEdit;

export const FPS = 24;
export const SEGMENT_TRIM_MIN_GAP = 1;
export const AUDIO_GAIN_MIN_DB = -60;
export const AUDIO_GAIN_MAX_DB = 12;
export const AV_PTS_OFFSET_MIN_MS = -10000;
export const AV_PTS_OFFSET_MAX_MS = 10000;

function cleanZero(value: number): number {
  return Object.is(value, -0) ? 0 : value;
}

/** 优先用源视频真实帧数，缺省时按 24fps 与时长推算 */
export function resolveTotalFrames(
  sourceFrameCount?: number | null,
  durationSeconds?: number | null,
  fps = FPS,
): number {
  if (
    typeof sourceFrameCount === "number" &&
    Number.isFinite(sourceFrameCount) &&
    sourceFrameCount > 0
  ) {
    return Math.trunc(sourceFrameCount);
  }
  if (
    typeof durationSeconds === "number" &&
    Number.isFinite(durationSeconds) &&
    durationSeconds > 0
  ) {
    return Math.max(1, Math.round(durationSeconds * fps));
  }
  return 0;
}

/** 将帧序号夹到 [0, maxFrame] 的整数帧 */
export function clampFrame(frame: number, maxFrame: number): number {
  if (!Number.isFinite(frame)) return 0;
  const upper = Number.isFinite(maxFrame) && maxFrame > 0 ? Math.trunc(maxFrame) : 0;
  return cleanZero(Math.min(Math.max(Math.round(frame), 0), upper));
}

/** sourceOutF === null 表示 EOF 哨兵（到片尾全长） */
export function effectiveOutFrame(
  sourceOutF: number | null | undefined,
  totalFrames: number,
): number {
  const safeTotal = Number.isFinite(totalFrames) && totalFrames > 0 ? Math.trunc(totalFrames) : 0;
  if (sourceOutF === null || sourceOutF === undefined || !Number.isFinite(sourceOutF)) {
    return safeTotal;
  }
  const rounded = Math.round(sourceOutF);
  if (safeTotal <= 0) return Math.max(0, rounded);
  return cleanZero(Math.min(Math.max(rounded, 0), safeTotal));
}

/** 半开区间 [in, out) 的最后一个可见预览帧（out - 1，绝不越界到 out） */
export function lastPreviewFrame(
  sourceInF: number,
  sourceOutF: number | null | undefined,
  totalFrames: number,
): number {
  const inF = Number.isFinite(sourceInF) ? Math.max(0, Math.round(sourceInF)) : 0;
  const outF = effectiveOutFrame(sourceOutF, totalFrames);
  if (outF <= 0) return Math.max(0, inF);
  return Math.max(inF >= outF ? 0 : inF, outF - 1);
}

/**
 * 约束 24fps 整数帧双端选区：
 * 保证 0 <= sourceInF < effectiveOut <= totalFrames 且间距至少 minGap（默认 1 帧）；
 * 当右端推到 totalFrames 时返回 sourceOutF: null 作为 EOF 哨兵。
 */
export function trimFrameRange(
  startFrame: number,
  endFrame: number | null,
  totalFrames: number,
  which: "start" | "end",
  minGap = SEGMENT_TRIM_MIN_GAP,
): { sourceInF: number; sourceOutF: number | null } {
  const total = Number.isFinite(totalFrames) && totalFrames > 0 ? Math.trunc(totalFrames) : 0;
  if (total <= 0) {
    return { sourceInF: 0, sourceOutF: null };
  }
  const gap = Math.max(1, Math.min(Math.trunc(minGap), total));
  const curOut = endFrame === null ? total : effectiveOutFrame(endFrame, total);

  if (which === "start") {
    const maxStart = Math.max(curOut - gap, 0);
    const nextIn = Math.min(clampFrame(startFrame, total), maxStart);
    return {
      sourceInF: nextIn,
      sourceOutF: endFrame === null || curOut >= total ? null : curOut,
    };
  }

  const safeStart = Math.min(clampFrame(startFrame, total), Math.max(total - gap, 0));
  if (endFrame === null) {
    return { sourceInF: safeStart, sourceOutF: null };
  }
  const snappedEnd = clampFrame(endFrame, total);
  const nextOut = Math.min(Math.max(snappedEnd, Math.min(safeStart + gap, total)), total);
  return {
    sourceInF: safeStart,
    sourceOutF: nextOut >= total ? null : nextOut,
  };
}

export function framesToSeconds(frames: number, fps = FPS): number {
  if (!Number.isFinite(frames) || fps <= 0) return 0;
  return frames / fps;
}

export function secondsToFrame(seconds: number, fps = FPS): number {
  if (!Number.isFinite(seconds) || fps <= 0) return 0;
  return cleanZero(Math.round(seconds * fps));
}

/** 将 <video>.currentTime 映射到整数帧序号 [0, totalFrames - 1] */
export function timeToFrameIndex(seconds: number, totalFrames: number, fps = FPS): number {
  if (!Number.isFinite(seconds) || totalFrames <= 0 || fps <= 0) return 0;
  const maxIndex = Math.trunc(totalFrames) - 1;
  const raw = Math.floor(Math.max(0, seconds) * fps + 1e-6);
  return Math.min(Math.max(raw, 0), maxIndex);
}

/**
 * 计算定位到第 frame 帧的播放时间点：
 * frame > 0 时取帧中点 (frame + 0.5) / fps 并低于总时长，避免浮点边界回落到 frame - 1。
 */
export function frameToSeekSeconds(
  frame: number,
  totalFrames?: number | null,
  durationSeconds?: number | null,
  fps = FPS,
): number {
  if (!Number.isFinite(frame) || fps <= 0) return 0;
  const safeTotal =
    typeof totalFrames === "number" && Number.isFinite(totalFrames) && totalFrames > 0
      ? Math.trunc(totalFrames)
      : null;
  const clamped =
    safeTotal !== null
      ? Math.min(Math.max(Math.round(frame), 0), Math.max(0, safeTotal - 1))
      : Math.max(0, Math.round(frame));
  if (clamped <= 0) return 0;
  const midpoint = (clamped + 0.5) / fps;
  if (
    typeof durationSeconds === "number" &&
    Number.isFinite(durationSeconds) &&
    durationSeconds > 0
  ) {
    return Math.min(midpoint, Math.max(0, durationSeconds - 1e-4));
  }
  return midpoint;
}

/** 按 24fps 时码展示 MM:SS:FF */
export function formatTimecode24(frame: number, fps = FPS): string {
  const totalFrames = Number.isFinite(frame) ? Math.max(0, Math.round(frame)) : 0;
  const safeFps = fps > 0 ? Math.round(fps) : FPS;
  const f = totalFrames % safeFps;
  const totalSec = Math.floor(totalFrames / safeFps);
  const s = totalSec % 60;
  const m = Math.floor(totalSec / 60);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(m)}:${pad(s)}:${pad(f)}`;
}

export function formatFrameLabel(frame: number, fps = FPS): string {
  const safe = Number.isFinite(frame) ? Math.max(0, Math.round(frame)) : 0;
  return `${safe}f · ${formatTimecode24(safe, fps)}`;
}

export function formatFramesDuration(frames: number, decimals = 2, fps = FPS): string {
  const safe = Number.isFinite(frames) ? Math.max(0, Math.round(frames)) : 0;
  return `${framesToSeconds(safe, fps).toFixed(decimals)} s`;
}

export function clampAudioGainDb(value: number): number {
  if (!Number.isFinite(value)) return 0;
  const clamped = Math.min(Math.max(value, AUDIO_GAIN_MIN_DB), AUDIO_GAIN_MAX_DB);
  return cleanZero(Math.round(clamped * 10) / 10);
}

export function clampAvPtsOffsetMs(value: number): number {
  if (!Number.isFinite(value)) return 0;
  const clamped = Math.min(Math.max(Math.round(value), AV_PTS_OFFSET_MIN_MS), AV_PTS_OFFSET_MAX_MS);
  return cleanZero(clamped);
}

export function normalizeSegmentEdit(
  edit?: Partial<SegmentEditValue> | null,
  totalFrames?: number,
): SegmentEditValue {
  const rawIn = Number.isFinite(edit?.sourceInF)
    ? Math.max(0, Math.round(edit!.sourceInF!))
    : DEFAULT_SEGMENT_EDIT.sourceInF;
  const rawOut =
    edit?.sourceOutF === null || edit?.sourceOutF === undefined || !Number.isFinite(edit.sourceOutF)
      ? null
      : Math.round(edit.sourceOutF);

  let sourceInF = cleanZero(rawIn);
  let sourceOutF = rawOut;

  if (typeof totalFrames === "number" && Number.isFinite(totalFrames) && totalFrames > 0) {
    const total = Math.trunc(totalFrames);
    sourceInF = Math.min(sourceInF, Math.max(total - SEGMENT_TRIM_MIN_GAP, 0));
    if (sourceOutF !== null) {
      const clampedOut = Math.min(
        Math.max(sourceOutF, Math.min(sourceInF + SEGMENT_TRIM_MIN_GAP, total)),
        total,
      );
      sourceOutF = clampedOut >= total ? null : cleanZero(clampedOut);
    }
  }

  return {
    sourceInF,
    sourceOutF,
    muteOriginal: Boolean(edit?.muteOriginal),
    audioGainDb: clampAudioGainDb(edit?.audioGainDb ?? DEFAULT_SEGMENT_EDIT.audioGainDb),
    nativeIsDialogue: Boolean(edit?.nativeIsDialogue),
    avPtsOffsetMs: clampAvPtsOffsetMs(edit?.avPtsOffsetMs ?? DEFAULT_SEGMENT_EDIT.avPtsOffsetMs),
  };
}

/** 恢复全长范围：将入点归零、出点设回 null（EOF 哨兵），保留音频设置 */
export function restoreFullLengthEdit(edit: SegmentEditValue): SegmentEditValue {
  return {
    ...edit,
    sourceInF: 0,
    sourceOutF: null,
  };
}

export function isFullLengthRange(
  edit: Pick<SegmentEditValue, "sourceInF" | "sourceOutF">,
): boolean {
  return edit.sourceInF === 0 && edit.sourceOutF === null;
}

export interface SegmentEditValidation {
  valid: boolean;
  errors: string[];
  effectiveOutF: number;
  trimmedFrames: number;
}

export function validateSegmentEdit(
  edit: SegmentEditValue,
  totalFrames: number,
): SegmentEditValidation {
  const errors: string[] = [];
  const safeTotal = Number.isFinite(totalFrames) && totalFrames > 0 ? Math.trunc(totalFrames) : 0;

  if (!Number.isInteger(edit.sourceInF) || edit.sourceInF < 0) {
    errors.push("入点帧必须为不小于 0 的整数");
  }
  if (edit.sourceOutF !== null && !Number.isInteger(edit.sourceOutF)) {
    errors.push("出点帧必须为整数或全长");
  }

  const effectiveOut = edit.sourceOutF === null ? safeTotal : edit.sourceOutF;
  if (safeTotal > 0) {
    if (edit.sourceInF >= effectiveOut || effectiveOut - edit.sourceInF < SEGMENT_TRIM_MIN_GAP) {
      errors.push("选区至少保留 1 帧（入点需小于出点）");
    }
    if (effectiveOut > safeTotal) {
      errors.push(`出点帧不能超过总帧数 ${safeTotal}`);
    }
  } else if (edit.sourceOutF !== null && edit.sourceOutF - edit.sourceInF < SEGMENT_TRIM_MIN_GAP) {
    errors.push("选区至少保留 1 帧（入点需小于出点）");
  }

  if (
    !Number.isFinite(edit.audioGainDb) ||
    edit.audioGainDb < AUDIO_GAIN_MIN_DB ||
    edit.audioGainDb > AUDIO_GAIN_MAX_DB
  ) {
    errors.push(`原声音量增益需在 ${AUDIO_GAIN_MIN_DB} 至 +${AUDIO_GAIN_MAX_DB} dB 之间`);
  }

  if (
    !Number.isInteger(edit.avPtsOffsetMs) ||
    edit.avPtsOffsetMs < AV_PTS_OFFSET_MIN_MS ||
    edit.avPtsOffsetMs > AV_PTS_OFFSET_MAX_MS
  ) {
    errors.push(`音画同步偏移需在 ${AV_PTS_OFFSET_MIN_MS} 至 +${AV_PTS_OFFSET_MAX_MS} ms 之间`);
  }

  const trimmedFrames = Math.max(0, effectiveOut - edit.sourceInF);
  return {
    valid: errors.length === 0,
    errors,
    effectiveOutF: effectiveOut,
    trimmedFrames,
  };
}

export function hasNonDefaultSegmentEdit(edit?: Partial<SegmentEditValue> | null): boolean {
  if (!edit) return false;
  return (
    (edit.sourceInF ?? 0) !== DEFAULT_SEGMENT_EDIT.sourceInF ||
    (edit.sourceOutF ?? null) !== DEFAULT_SEGMENT_EDIT.sourceOutF ||
    Boolean(edit.muteOriginal) !== DEFAULT_SEGMENT_EDIT.muteOriginal ||
    (edit.audioGainDb ?? 0) !== DEFAULT_SEGMENT_EDIT.audioGainDb ||
    Boolean(edit.nativeIsDialogue) !== DEFAULT_SEGMENT_EDIT.nativeIsDialogue ||
    (edit.avPtsOffsetMs ?? 0) !== DEFAULT_SEGMENT_EDIT.avPtsOffsetMs
  );
}

/** 仅输出发生变化的字段；当出点恢复全长时显式保留 sourceOutF: null */
export function buildSegmentTrimPatch(
  initial: SegmentEditValue,
  next: SegmentEditValue,
): SegmentEditPatch {
  const patch: SegmentEditPatch = {};
  if (next.sourceInF !== initial.sourceInF) {
    patch.sourceInF = next.sourceInF;
  }
  if (next.sourceOutF !== initial.sourceOutF) {
    patch.sourceOutF = next.sourceOutF;
  }
  if (next.muteOriginal !== initial.muteOriginal) {
    patch.muteOriginal = next.muteOriginal;
  }
  if (Math.abs(next.audioGainDb - initial.audioGainDb) > 1e-6) {
    patch.audioGainDb = next.audioGainDb;
  }
  if (next.nativeIsDialogue !== initial.nativeIsDialogue) {
    patch.nativeIsDialogue = next.nativeIsDialogue;
  }
  if (next.avPtsOffsetMs !== initial.avPtsOffsetMs) {
    patch.avPtsOffsetMs = next.avPtsOffsetMs;
  }
  return patch;
}
