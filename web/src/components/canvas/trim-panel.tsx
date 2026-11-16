import { useEffect, useRef, type KeyboardEvent, type PointerEvent, type ReactNode } from "react";
import { Check, Loader2, Volume2, VolumeX, X } from "lucide-react";

import {
  ChromeButton,
  ChromePill,
  ChromeSeparator,
  ChromeTooltip,
} from "@/components/canvas/chrome/chrome";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { useTrimStore } from "@/store/trim";
import {
  TRIM_MIN_GAP,
  canConfirmTrim,
  clampTrimTime,
  confirmHint,
  formatTrimSeconds,
  type TrimEnd,
} from "@/utils/canvas/trim";

/** 手柄的键盘微调步长（秒）；按住 Shift 走大步 */
const KEY_STEP = 0.1;
const KEY_STEP_BIG = 1;

/**
 * 胶片条的缩略图：还在读时整条铺一层扫光占位，读好后每格淡入；失败的格子用灰底占位。
 * thumbs 为空表示还在读（读失败时上游给的是一排 null）。
 */
export function Filmstrip({
  thumbs,
  className,
}: {
  thumbs: (string | null)[];
  className?: string;
}) {
  const loading = thumbs.length === 0;
  const cells = loading ? Array.from({ length: 16 }, () => null) : thumbs;
  return (
    <div className={cn("absolute inset-0 flex overflow-hidden rounded-md", className)} aria-hidden>
      {cells.map((url, index) => (
        <div key={index} className="bg-muted/60 relative h-full min-w-0 flex-1">
          {url && (
            <img
              src={url}
              alt=""
              draggable={false}
              className="size-full animate-[canvas-fade-in_0.3s_ease-out_both] object-cover"
              style={{ animationDelay: `${index * 20}ms` }}
            />
          )}
        </div>
      ))}
      {loading && (
        <div className="pointer-events-none absolute inset-0 overflow-hidden motion-reduce:hidden">
          <div className="absolute inset-0 animate-[node-sheen_1.4s_ease-in-out_infinite] bg-[linear-gradient(120deg,transparent_30%,var(--sheen)_50%,transparent_70%)]" />
        </div>
      )}
    </div>
  );
}

/** 白色播放头竖线：按 value / total 定位，不拦截指针事件 */
export function Playhead({
  value,
  total,
  className,
}: {
  value: number;
  total: number;
  className?: string;
}) {
  const percent = total > 0 ? Math.min(Math.max((value / total) * 100, 0), 100) : 0;
  return (
    <div
      aria-hidden
      data-slot="trim-playhead"
      className={cn(
        "pointer-events-none absolute inset-y-[-2px] z-10 w-0.5 -translate-x-1/2 rounded-full bg-white shadow",
        className,
      )}
      style={{ left: `${percent}%` }}
    />
  );
}

/** 一个手柄：8px 宽的蓝条，点击热区 24px；role=slider 可聚焦、可键盘微调 */
function Handle({
  which,
  label,
  value,
  min,
  max,
  valueText,
  disabled,
  percent,
  onPointerDown,
  onKeyDown,
}: {
  which: TrimEnd;
  label: string;
  value: number;
  min: number;
  max: number;
  valueText?: string;
  disabled: boolean;
  percent: number;
  onPointerDown: (event: PointerEvent<HTMLDivElement>) => void;
  onKeyDown: (event: KeyboardEvent<HTMLDivElement>) => void;
}) {
  return (
    <div
      role="slider"
      tabIndex={disabled ? -1 : 0}
      aria-label={label}
      aria-valuemin={min}
      aria-valuemax={max}
      aria-valuenow={value}
      aria-valuetext={valueText}
      aria-disabled={disabled}
      data-trim-handle={which}
      onPointerDown={onPointerDown}
      onKeyDown={onKeyDown}
      className={cn(
        "nodrag nopan absolute top-0 z-20 flex h-full w-6 -translate-x-1/2 touch-none items-stretch justify-center outline-none",
        disabled ? "cursor-default" : "cursor-ew-resize",
      )}
      style={{ left: `${percent}%` }}
    >
      <span
        className={cn(
          "w-2 rounded-sm bg-blue-500 shadow-sm transition-colors",
          "focus-visible:ring-node-ring/60",
          disabled && "opacity-60",
        )}
      />
    </div>
  );
}

export type RangeHandlesProps = {
  start: number;
  end: number;
  total: number;
  minGap: number;
  disabled?: boolean;
  step?: number;
  bigStep?: number;
  snap?: (raw: number) => number;
  formatValue?: (value: number) => string;
  startLabel?: string;
  endLabel?: string;
  onChange: (next: { start: number; end: number }, which: TrimEnd) => void;
  onSeek?: (value: number) => void;
  className?: string;
  children?: ReactNode;
};

/**
 * 受控的双端选区手柄与遮罩轨道：不绑定任何业务 store，
 * 画布按 0.01s、短剧按 24fps 整数帧分别传入 snap / minGap / step 即可复用。
 */
export function RangeHandles({
  start,
  end,
  total,
  minGap,
  disabled = false,
  step = 1,
  bigStep = 10,
  snap,
  formatValue,
  startLabel = "入点",
  endLabel = "出点",
  onChange,
  onSeek,
  className,
  children,
}: RangeHandlesProps) {
  const trackRef = useRef<HTMLDivElement | null>(null);
  const dragCleanupRef = useRef<(() => void) | null>(null);
  const latestRef = useRef({
    start,
    end,
    total,
    minGap,
    disabled,
    snap,
    onChange,
    onSeek,
  });
  useEffect(() => {
    latestRef.current = { start, end, total, minGap, disabled, snap, onChange, onSeek };
  });
  useEffect(() => {
    if (disabled) {
      dragCleanupRef.current?.();
    }
  }, [disabled]);
  useEffect(() => {
    return () => {
      dragCleanupRef.current?.();
    };
  }, []);

  const safeTotal = total > 0 ? total : 1;
  const clampedStart = total > 0 ? Math.min(Math.max(start, 0), total) : 0;
  const clampedEnd = total > 0 ? Math.min(Math.max(end, clampedStart), total) : 0;
  const pct = (value: number) => Math.min(Math.max((value / safeTotal) * 100, 0), 100);
  const startPct = pct(clampedStart);
  const endPct = pct(clampedEnd);

  const rawAt = (clientX: number): number | null => {
    const rect = trackRef.current?.getBoundingClientRect();
    const currentTotal = latestRef.current.total;
    if (!rect || rect.width <= 0 || currentTotal <= 0) return null;
    return ((clientX - rect.left) / rect.width) * currentTotal;
  };

  const snapValue = (raw: number, curTotal: number, curSnap?: (v: number) => number) =>
    curSnap ? curSnap(raw) : Math.min(Math.max(raw, 0), curTotal);

  const clampPair = (which: TrimEnd, rawValue: number) => {
    const cur = latestRef.current;
    const snapped = snapValue(rawValue, cur.total, cur.snap);
    if (which === "start") {
      return {
        start: Math.min(Math.max(snapped, 0), Math.max(cur.end - cur.minGap, 0)),
        end: cur.end,
      };
    }
    return {
      start: cur.start,
      end: Math.max(Math.min(snapped, cur.total), Math.min(cur.start + cur.minGap, cur.total)),
    };
  };

  const startPointerDrag = (
    target: HTMLElement,
    pointerId: number,
    onMoveClientX: (clientX: number) => void,
  ) => {
    if (dragCleanupRef.current !== null) return;
    if (typeof pointerId === "number") {
      try {
        target.setPointerCapture?.(pointerId);
      } catch {
        // 忽略合成事件或不支持 pointerId 的环境异常
      }
    }

    let cleaned = false;
    let lastEvent: Event | null = null;
    const isActivePointer = (e: globalThis.PointerEvent) => e.pointerId === pointerId;
    const onMove = (e: globalThis.PointerEvent) => {
      if (cleaned || e === lastEvent || !isActivePointer(e)) return;
      lastEvent = e;
      onMoveClientX(e.clientX);
    };
    const cleanup = () => {
      if (cleaned) return;
      cleaned = true;
      if (dragCleanupRef.current === cleanup) {
        dragCleanupRef.current = null;
      }
      target.removeEventListener("pointermove", onMove);
      target.removeEventListener("pointerup", onEnd);
      target.removeEventListener("pointercancel", onEnd);
      target.removeEventListener("lostpointercapture", onLostCapture);
      window.removeEventListener("pointermove", onMove);
      window.removeEventListener("pointerup", onEnd);
      window.removeEventListener("pointercancel", onEnd);
      if (typeof pointerId === "number") {
        try {
          if (!target.hasPointerCapture || target.hasPointerCapture(pointerId)) {
            target.releasePointerCapture?.(pointerId);
          }
        } catch {
          // 忽略已卸载或指针捕获已失效时的异常
        }
      }
    };
    const onEnd = (e: globalThis.PointerEvent) => {
      if (cleaned || e === lastEvent || !isActivePointer(e)) return;
      lastEvent = e;
      cleanup();
    };
    const onLostCapture = (e: globalThis.PointerEvent) => {
      if (cleaned || !isActivePointer(e)) return;
      cleanup();
    };

    dragCleanupRef.current = cleanup;
    target.addEventListener("pointermove", onMove);
    target.addEventListener("pointerup", onEnd);
    target.addEventListener("pointercancel", onEnd);
    target.addEventListener("lostpointercapture", onLostCapture);
    window.addEventListener("pointermove", onMove);
    window.addEventListener("pointerup", onEnd);
    window.addEventListener("pointercancel", onEnd);
  };

  const beginDrag = (which: TrimEnd) => (event: PointerEvent<HTMLDivElement>) => {
    if (
      latestRef.current.disabled ||
      latestRef.current.total <= 0 ||
      event.button !== 0 ||
      dragCleanupRef.current !== null
    ) {
      return;
    }
    event.preventDefault();
    event.stopPropagation();
    const target = event.currentTarget;
    target.focus();
    startPointerDrag(target, event.pointerId, (clientX) => {
      const raw = rawAt(clientX);
      if (raw === null) return;
      latestRef.current.onChange(clampPair(which, raw), which);
    });
  };

  const keyDrag = (which: TrimEnd) => (event: KeyboardEvent<HTMLDivElement>) => {
    if (disabled || total <= 0) return;
    const delta = event.shiftKey ? bigStep : step;
    const base = which === "start" ? clampedStart : clampedEnd;
    if (event.key === "ArrowLeft") onChange(clampPair(which, base - delta), which);
    else if (event.key === "ArrowRight") onChange(clampPair(which, base + delta), which);
    else return;
    event.preventDefault();
  };

  const handleTrackPointerDown = (event: PointerEvent<HTMLDivElement>) => {
    const cur = latestRef.current;
    if (cur.disabled || cur.total <= 0 || event.button !== 0 || dragCleanupRef.current !== null) {
      return;
    }
    if ((event.target as HTMLElement).closest("[data-trim-handle]")) return;
    const raw = rawAt(event.clientX);
    if (raw === null) return;
    cur.onSeek?.(snapValue(raw, cur.total, cur.snap));
    if (!cur.onSeek) return;

    startPointerDrag(event.currentTarget, event.pointerId, (clientX) => {
      const moveRaw = rawAt(clientX);
      if (moveRaw === null) return;
      const latest = latestRef.current;
      latest.onSeek?.(snapValue(moveRaw, latest.total, latest.snap));
    });
  };

  return (
    <div
      ref={trackRef}
      data-slot="trim-range-track"
      onPointerDown={handleTrackPointerDown}
      className={cn(children ? "relative" : "absolute inset-0", "select-none", className)}
    >
      {children}
      {/* 选区外压暗 */}
      <div
        className="pointer-events-none absolute inset-y-0 left-0 rounded-l-md bg-black/55"
        style={{ width: `${startPct}%` }}
      />
      <div
        className="pointer-events-none absolute inset-y-0 right-0 rounded-r-md bg-black/55"
        style={{ width: `${100 - endPct}%` }}
      />
      {/* 选区上下边框 */}
      <div
        className="pointer-events-none absolute inset-y-0 border-y-2 border-blue-500"
        style={{ left: `${startPct}%`, width: `${Math.max(endPct - startPct, 0)}%` }}
      />
      <Handle
        which="start"
        label={startLabel}
        value={clampedStart}
        min={0}
        max={Math.max(clampedEnd - minGap, 0)}
        valueText={formatValue?.(clampedStart)}
        percent={startPct}
        disabled={disabled}
        onPointerDown={beginDrag("start")}
        onKeyDown={keyDrag("start")}
      />
      <Handle
        which="end"
        label={endLabel}
        value={clampedEnd}
        min={Math.min(clampedStart + minGap, total)}
        max={total}
        valueText={formatValue?.(clampedEnd)}
        percent={endPct}
        disabled={disabled}
        onPointerDown={beginDrag("end")}
        onKeyDown={keyDrag("end")}
      />
    </div>
  );
}

export type MuteButtonProps = {
  muted: boolean;
  hasAudio?: boolean;
  disabled?: boolean;
  onToggle: (nextMuted: boolean) => void;
  variant?: "chrome" | "button";
  noAudioLabel?: string;
  mutedLabel?: string;
  unmutedLabel?: string;
  "aria-label"?: string;
  tooltipSide?: "top" | "bottom" | "left" | "right";
  className?: string;
  children?: ReactNode;
};

/** 受控的原声静音开关：画布走胶囊按钮，短剧抽屉走标准按钮 */
export function MuteButton({
  muted,
  hasAudio = true,
  disabled = false,
  onToggle,
  variant = "chrome",
  noAudioLabel = "该视频没有声音",
  mutedLabel = "保留声音",
  unmutedLabel = "去掉声音",
  "aria-label": ariaLabel = "去音轨",
  tooltipSide = "top",
  className,
  children,
}: MuteButtonProps) {
  const label = hasAudio ? (muted ? mutedLabel : unmutedLabel) : noAudioLabel;
  const isDisabled = disabled || !hasAudio;

  if (variant === "button") {
    return (
      <Button
        type="button"
        variant={muted ? "secondary" : "outline"}
        size="sm"
        aria-label={ariaLabel}
        aria-pressed={muted}
        title={label}
        disabled={isDisabled}
        onClick={() => onToggle(!muted)}
        className={className}
      >
        {muted || !hasAudio ? <VolumeX /> : <Volume2 />}
        {children ?? <span>{label}</span>}
      </Button>
    );
  }

  return (
    <ChromeTooltip label={label} side={tooltipSide}>
      <ChromeButton
        aria-label={ariaLabel}
        aria-pressed={muted}
        disabled={isDisabled}
        active={muted}
        onClick={() => onToggle(!muted)}
        className={className}
      >
        {muted || !hasAudio ? <VolumeX /> : <Volume2 />}
        {children}
      </ChromeButton>
    </ChromeTooltip>
  );
}

/**
 * 修剪面板（设计稿「视频修剪」§6.3，参考图 1 与图 3）：
 * 左 × 关闭；中间胶片条（缩略图 + 两端蓝色手柄 + 居中时长胶囊 + 白色播放头）；
 * 右 去音轨、✓ 确认。编码中手柄锁定、✓ 变成转圈，面板下方出现进度条「编码中 x%」。
 * 选区、去音轨、进度全在 useTrimStore 里，面板卸载（取消选中）后重新挂上能接着显示。
 */
export function TrimPanel({
  width,
  thumbs,
  loadError,
  onConfirm,
  onClose,
}: {
  width: number;
  thumbs: (string | null)[];
  /** 读源视频失败的原因；有它时手柄仍可拖，但 ✓ 不可用 */
  loadError?: string;
  onConfirm: () => void;
  /** 点 ×：编辑态直接退出；编码中由调用方弹确认再中止 */
  onClose: () => void;
}) {
  const phase = useTrimStore((state) => state.phase);
  const start = useTrimStore((state) => state.start);
  const end = useTrimStore((state) => state.end);
  const duration = useTrimStore((state) => state.duration);
  const hasAudio = useTrimStore((state) => state.hasAudio);
  const discardAudio = useTrimStore((state) => state.discardAudio);
  const progress = useTrimStore((state) => state.progress);
  const error = useTrimStore((state) => state.error);
  const current = useTrimStore((state) => state.current);

  const locked = phase !== "editing";
  const encoding = phase === "encoding";
  const gate = { start, end, duration, phase };
  const canConfirm = canConfirmTrim(gate) && !loadError;
  // 读源视频信息还没好：时长胶囊先显示「加载中」；缩略图没好时胶片条自己有扫光占位
  const loading = !loadError && phase === "loading";
  const hint = loadError ?? confirmHint(gate);

  return (
    <div className="nodrag nopan nowheel flex flex-col gap-2" style={{ width }}>
      <ChromePill className="h-16 w-full gap-2 rounded-2xl px-2" aria-label="视频修剪">
        <ChromeTooltip label={encoding ? "中止编码" : "关闭"}>
          <ChromeButton aria-label={encoding ? "中止编码" : "关闭修剪"} onClick={onClose}>
            <X />
          </ChromeButton>
        </ChromeTooltip>
        <ChromeSeparator />

        <RangeHandles
          start={start}
          end={end}
          total={duration}
          minGap={TRIM_MIN_GAP}
          disabled={locked}
          step={KEY_STEP}
          bigStep={KEY_STEP_BIG}
          snap={(raw) => clampTrimTime(raw, duration)}
          formatValue={formatTrimSeconds}
          onChange={(next, which) => {
            const store = useTrimStore.getState();
            store.setRange(next.start, next.end);
            store.seekTo(which === "start" ? next.start : next.end);
          }}
          onSeek={(raw) => {
            const latest = useTrimStore.getState();
            const time = clampTrimTime(
              Math.min(Math.max(raw, latest.start), latest.end),
              latest.duration,
            );
            latest.seekTo(time);
          }}
          className="mx-1.5 h-12 min-w-0 flex-1"
        >
          <Filmstrip thumbs={thumbs} />
          <Playhead value={current} total={duration} />
          {/* 居中的时长胶囊 */}
          <div className="pointer-events-none absolute inset-0 z-10 grid place-items-center">
            <span className="rounded-full bg-black/70 px-3 py-1 font-mono text-[13px] font-semibold tabular-nums text-white">
              {loading ? "加载中…" : formatTrimSeconds(end - start)}
            </span>
          </div>
        </RangeHandles>

        <ChromeSeparator />
        <MuteButton
          muted={discardAudio}
          hasAudio={hasAudio}
          disabled={locked}
          onToggle={(next) => useTrimStore.getState().setDiscardAudio(next)}
        />
        <ChromeTooltip label={hint}>
          <span className="inline-flex">
            <ChromeButton
              variant="primary"
              size="lg"
              aria-label="确认修剪"
              disabled={!canConfirm}
              onClick={onConfirm}
              className="rounded-xl"
            >
              {encoding ? <Loader2 className="animate-spin" /> : <Check />}
            </ChromeButton>
          </span>
        </ChromeTooltip>
      </ChromePill>

      {encoding && (
        <div className="px-1" role="status" aria-live="polite">
          <div className="bg-muted h-0.5 w-full overflow-hidden rounded-full">
            <div
              className="bg-foreground h-full rounded-full transition-[width] duration-200 ease-out"
              style={{ width: `${progress}%` }}
            />
          </div>
          <p className="text-muted-foreground mt-1.5 text-center text-xs tabular-nums">
            编码中 {progress}%
          </p>
        </div>
      )}
      {!encoding && (error || loadError) && (
        <p role="alert" className="text-destructive px-2 text-center text-xs">
          {error ?? loadError}
        </p>
      )}
    </div>
  );
}
