import { useCallback, useContext, useEffect, useLayoutEffect, useRef, useState } from "react";
import {
  animate,
  motion,
  useDragControls,
  useMotionValue,
  useTransform,
  type MotionValue,
} from "motion/react";

import { Scissors } from "lucide-react";

import type { SegmentTake, TakeQC } from "@/api/drama/type";
import { dramaErrorMessage } from "@/api/drama/errors";
import { VideoPoster } from "@/components/canvas/media-preview";
import { RejectTakeDialog } from "@/components/drama/reject-take-dialog";
import { Button } from "@/components/ui/button";
import { VideoPlayer } from "@/components/video-player";
import { TaskGenerationStatus } from "@/components/generation-status";
import { isTerminalStatus } from "@/utils/tasks/status";
import { cn } from "@/lib/utils";
import { initialTakeIndex, snapTakeIndex } from "@/utils/drama/take-carousel";
import { StudioContext } from "./use-studio";

/** 吸附用的弹簧：略带回弹，像被磁铁吸过去再稳住 */
const MAGNET = { type: "spring", stiffness: 420, damping: 30, mass: 0.9 } as const;

/** 播放器最高多高（px），和原来的单播放器一致 */
const MAX_CARD_HEIGHT = 680;

/** 底部留给位置点的高度（px） */
const DOTS_SPACE = 22;

/** 横图时中间卡片最多占预览区宽度的比例，两边要留出露出邻居的位置 */
const MAX_CARD_WIDTH_RATIO = 0.62;

/** 相邻两版的间距是卡片宽度的多少：小于 1 让两边的卡片藏一点在中间那张后面 */
const STEP_RATIO = 0.86;

/** 滚轮 / 触控板横滑累积多少像素换一版，换完冷却多久（毫秒），免得一滑就跳好几版 */
const WHEEL_THRESHOLD = 60;
const WHEEL_COOLDOWN = 260;

const isPlayable = (take: SegmentTake) => take.status === "succeeded" && !!take.asset;
const isRunning = (take: SegmentTake) =>
  !isPlayable(take) && (!isTerminalStatus(take.status) || take.status === "succeeded");

const ORIGIN_LABEL = { i2v: "首帧 I2V", canvas_import: "画布导入", native: "直接生成" } as const;
const QC_LABEL = { accepted: "已通过", rejected: "已驳回", unreviewed: "待质检" } as const;

/** 预览区的尺寸，用来算卡片大小 */
function useBoxSize() {
  const ref = useRef<HTMLDivElement>(null);
  const [size, setSize] = useState({ w: 0, h: 0 });
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const observer = new ResizeObserver(([entry]) => {
      const { width, height } = entry.contentRect;
      setSize({ w: width, h: height });
    });
    observer.observe(el);
    return () => observer.disconnect();
  }, []);
  return { ref, ...size };
}

/** 一张卡：按离中间的距离算位置、缩放、透明度和层级，拖动时跟着连续变化 */
function TakeCard({
  take,
  index,
  x,
  step,
  width,
  height,
  focused,
  chosen,
  onPick,
  onTrim,
  onQc,
  qcBusy,
}: {
  take: SegmentTake;
  index: number;
  x: MotionValue<number>;
  step: number;
  width: number;
  height: number;
  focused: boolean;
  chosen: boolean;
  onPick: () => void;
  onTrim?: () => void;
  onQc?: (qc: TakeQC) => void;
  qcBusy?: boolean;
}) {
  const offset = useTransform(x, (value) => index * step + value);
  const distance = useTransform(offset, (value) =>
    step > 0 ? Math.min(Math.abs(value) / step, 2) : 0,
  );
  const scale = useTransform(distance, [0, 1, 2], [1, 0.8, 0.66]);
  const opacity = useTransform(distance, [0, 1, 2], [1, 0.5, 0.18]);
  const zIndex = useTransform(distance, (value) => Math.round(20 - value * 8));
  const playable = isPlayable(take);

  return (
    <motion.div
      data-take-card
      data-qc={take.qc ?? "unreviewed"}
      style={{
        x: offset,
        scale,
        opacity,
        zIndex,
        width,
        height,
        marginLeft: -width / 2,
        marginTop: -height / 2,
      }}
      onClick={focused ? undefined : onPick}
      className={cn(
        "absolute top-1/2 left-1/2 overflow-hidden rounded-2xl bg-black ring-1 ring-white/10",
        focused ? "shadow-[0_40px_80px_-30px_rgb(0_0_0/0.9)]" : "cursor-pointer",
        chosen && "ring-primary ring-2",
        take.qc === "rejected" && "ring-destructive/50",
      )}
    >
      <div className={cn("size-full", take.qc === "rejected" && "opacity-45 grayscale")}>
        {playable && take.asset ? (
          focused ? (
            // 和画布视频节点同一个播放器：先显示封面、播放钮和时长，点了才加载；滑走时停播退回封面
            <VideoPlayer
              key={take.taskId}
              src={take.asset.url}
              durationMs={take.asset.durationMs}
            />
          ) : (
            <VideoPoster src={take.asset.url} />
          )
        ) : (
          <TaskGenerationStatus
            taskId={take.taskId}
            fallback={{ status: take.status, error_message: take.errorMessage }}
            className="size-full"
          />
        )}
      </div>
      <div className="pointer-events-none absolute top-2 left-2 flex max-w-[calc(100%-5rem)] flex-wrap gap-1 text-[11px]">
        <span className="rounded-full bg-black/70 px-2 py-1 text-white">
          {ORIGIN_LABEL[take.origin ?? "native"]}
        </span>
        <span
          className={cn(
            "rounded-full bg-black/70 px-2 py-1 text-white",
            take.qc === "rejected" && "bg-destructive/85",
          )}
        >
          {QC_LABEL[take.qc ?? "unreviewed"]}
        </span>
        {chosen && (
          <span className="bg-primary text-primary-foreground rounded-full px-2 py-1 font-semibold">
            采用
          </span>
        )}
      </div>
      {playable && onQc && (
        <div
          className="nodrag absolute inset-x-0 bottom-2 z-20 flex justify-center gap-1"
          onPointerDown={(event) => event.stopPropagation()}
          onClick={(event) => event.stopPropagation()}
        >
          {(
            [
              ["accepted", "通过"],
              ["rejected", "驳回"],
              ["unreviewed", "撤销质检"],
            ] as const
          ).map(([qc, label]) => (
            <button
              key={qc}
              type="button"
              aria-label={`${label}第 ${index + 1} 版`}
              disabled={qcBusy || (take.qc ?? "unreviewed") === qc}
              onClick={() => onQc(qc)}
              className="rounded-full bg-black/75 px-2 py-1 text-xs text-white backdrop-blur-xs disabled:opacity-40"
            >
              {label}
            </button>
          ))}
        </div>
      )}
      {focused && playable && take.asset && onTrim && (
        <button
          type="button"
          aria-label="修剪当前版本"
          title="逐镜精裁与原声参数"
          onPointerDown={(event) => event.stopPropagation()}
          onClick={(event) => {
            event.stopPropagation();
            onTrim();
          }}
          className="nodrag focus-visible:ring-ring/60 absolute top-2 right-2 z-20 inline-flex h-7 items-center gap-1 rounded-md bg-black/70 px-2 text-xs font-medium text-white backdrop-blur-xs transition-colors hover:bg-black/85 focus-visible:ring-2 outline-none"
        >
          <Scissors className="size-3" />
          修剪
        </button>
      )}
    </motion.div>
  );
}

/**
 * 片段的版本轮播：各版左右排开，正中是当前版本（放大、可播放），两边缩小变暗露出一截。
 * 中间那张用和画布视频节点相同的播放器（封面 + 播放钮 + 时长，点了才加载）。
 * 拖动（播放钮和控制条上不触发，免得和拖进度条打架）、滚轮横滑、← → 都能换版；
 * 松手后按位移加惯性算出最近的一版，用带回弹的弹簧「吸」到中间。
 * 吸到中间的是成功的版本就自动采用；生成中、失败的只是看，不会被采用。
 * 有采用变化（如第一版成功被自动采用）或新提交了一版时，自动滑过去。
 */
export function TakeCarousel({
  takes,
  chosenTaskId,
  aspect,
  onChoose,
  onTrim,
}: {
  takes: readonly SegmentTake[];
  chosenTaskId: string;
  /** 宽 / 高 */
  aspect: number;
  onChoose: (taskId: string) => void;
  onTrim?: (taskId: string) => void;
}) {
  const studio = useContext(StudioContext);
  const segment = studio?.selected;
  const [rejectTaskId, setRejectTaskId] = useState<string | null>(null);
  const [rejectOpen, setRejectOpen] = useState(false);
  const [qcBusy, setQcBusy] = useState<string | null>(null);
  const pending = useRef(false);
  const [error, setError] = useState<string | null>(null);
  const changeQc = async (take: SegmentTake, qc: TakeQC) => {
    if (!studio || !segment || pending.current) return;
    if (qc === "rejected" && segment.chosenTaskId === take.taskId) {
      setRejectTaskId(take.taskId);
      setRejectOpen(true);
      return;
    }
    pending.current = true;
    setQcBusy(take.taskId);
    setError(null);
    try {
      await studio.actions.patchTakeQc(segment, take.taskId, {
        qc,
        expectedSegmentVersion: studio.episode?.segmentVersion,
      });
    } catch (e) {
      setError(dramaErrorMessage(e, "质检更新失败"));
    } finally {
      pending.current = false;
      setQcBusy(null);
    }
  };
  const clearChosen = async () => {
    if (!studio || !segment || pending.current) return;
    pending.current = true;
    setQcBusy("clear");
    setError(null);
    try {
      await studio.actions.clearChosen(segment);
    } catch (e) {
      setError(dramaErrorMessage(e, "清空采用失败"));
    } finally {
      pending.current = false;
      setQcBusy(null);
    }
  };
  const box = useBoxSize();
  const [focus, setFocus] = useState(() => initialTakeIndex(takes, chosenTaskId));
  const focusRef = useRef(focus);
  const dragFrom = useRef(focus);
  const wheel = useRef({ total: 0, lockedUntil: 0 });
  const controls = useDragControls();

  // 卡片尺寸：竖图按高度定，横图不超过宽度的 62%，给两边留出位置
  const dots = takes.length > 1;
  let height = Math.max(0, Math.min(box.h - 48 - (dots ? DOTS_SPACE : 0), MAX_CARD_HEIGHT));
  let width = height * aspect;
  if (width > box.w * MAX_CARD_WIDTH_RATIO) {
    width = box.w * MAX_CARD_WIDTH_RATIO;
    height = width / aspect;
  }
  const step = takes.length > 1 ? width * STEP_RATIO : 0;
  const x = useMotionValue(-focus * step);

  /** 滑到第 index 版；adopt 为真且是成功的版本时设为采用 */
  const goTo = useCallback(
    (index: number, adopt: boolean) => {
      const target = Math.min(takes.length - 1, Math.max(0, index));
      focusRef.current = target;
      setFocus(target);
      void animate(x, -target * step, MAGNET);
      const take = takes[target];
      if (
        adopt &&
        take &&
        isPlayable(take) &&
        take.qc !== "rejected" &&
        take.taskId !== chosenTaskId
      )
        onChoose(take.taskId);
    },
    [chosenTaskId, onChoose, step, takes, x],
  );

  // 尺寸变了：直接对齐到当前版本，不播动画
  useEffect(() => {
    x.set(-focusRef.current * step);
  }, [step, x]);

  // 采用在别处变了（第一版成功被自动采用、别的标签页改了）：滑过去
  useEffect(() => {
    const index = takes.findIndex((take) => take.taskId === chosenTaskId);
    if (index >= 0 && index !== focusRef.current) goTo(index, false);
    // 只跟着采用变化走，不因 takes 引用变化反复触发
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [chosenTaskId]);

  // 新提交了一版：滑到最新那一版看进度
  const count = useRef(takes.length);
  useEffect(() => {
    if (takes.length > count.current) goTo(takes.length - 1, false);
    count.current = takes.length;
  }, [goTo, takes.length]);

  const onWheel = (event: React.WheelEvent) => {
    const delta =
      Math.abs(event.deltaX) > Math.abs(event.deltaY)
        ? event.deltaX
        : event.shiftKey
          ? event.deltaY
          : 0;
    if (!delta) return;
    const now = performance.now();
    if (now < wheel.current.lockedUntil) return;
    wheel.current.total += delta;
    if (Math.abs(wheel.current.total) < WHEEL_THRESHOLD) return;
    goTo(focusRef.current + (wheel.current.total > 0 ? 1 : -1), true);
    wheel.current = { total: 0, lockedUntil: now + WHEEL_COOLDOWN };
  };

  return (
    <div
      ref={box.ref}
      role="group"
      aria-roledescription="carousel"
      aria-label={`版本 ${focus + 1} / ${takes.length}`}
      tabIndex={0}
      onWheel={onWheel}
      onKeyDown={(event) => {
        if ((event.target as Element).closest("input, select, [role=dialog]")) return;
        if (event.key === "ArrowLeft") goTo(focusRef.current - 1, true);
        else if (event.key === "ArrowRight") goTo(focusRef.current + 1, true);
        else return;
        event.preventDefault();
      }}
      onPointerDown={(event) => {
        // 播放按钮、控制条（进度条、静音）自己处理点击和拖动；封面和其余地方按下就开始拖
        if ((event.target as Element).closest("button, input, .nodrag")) return;
        dragFrom.current = focusRef.current;
        controls.start(event);
      }}
      className="focus-visible:ring-ring/40 relative size-full touch-pan-y outline-none select-none focus-visible:ring-2"
    >
      {studio && segment && (
        <div className="nodrag absolute inset-x-3 top-2 z-40 flex flex-wrap items-center justify-end gap-2">
          {error && (
            <p role="alert" className="text-destructive text-xs">
              {error}
            </p>
          )}
          <Button
            size="sm"
            variant="outline"
            onClick={() => studio.importTakesModal?.open(segment.id)}
          >
            导入画布视频
          </Button>
          {chosenTaskId && (
            <Button
              size="sm"
              variant="outline"
              disabled={!!qcBusy}
              onClick={() => void clearChosen()}
            >
              清空采用
            </Button>
          )}
        </div>
      )}
      {/* 看不见的拖动把手：只负责把指针位移写进 x，卡片都从 x 读位置 */}
      <motion.div
        aria-hidden
        drag="x"
        dragControls={controls}
        dragListener={false}
        dragMomentum={false}
        dragElastic={0.18}
        dragConstraints={{ left: -(takes.length - 1) * step, right: 0 }}
        onDragEnd={(_, info) =>
          goTo(snapTakeIndex(x.get(), info.velocity.x, step, takes.length, dragFrom.current), true)
        }
        style={{ x }}
        className="pointer-events-none absolute size-0"
      />
      {width > 0 &&
        takes.map((take, index) => (
          <TakeCard
            key={take.taskId}
            take={take}
            index={index}
            x={x}
            step={step}
            width={width}
            height={height}
            focused={index === focus}
            chosen={take.taskId === chosenTaskId}
            onPick={() => goTo(index, true)}
            onTrim={onTrim ? () => onTrim(take.taskId) : undefined}
            onQc={studio && segment ? (qc) => void changeQc(take, qc) : undefined}
            qcBusy={!!qcBusy}
          />
        ))}
      {/* 位置点：当前版本是长条，采用的标主色，失败的标红；点一下滑过去 */}
      {dots && (
        <div className="absolute inset-x-0 bottom-0 z-30 flex justify-center gap-1.5">
          {takes.map((take, index) => (
            <button
              key={take.taskId}
              type="button"
              tabIndex={-1}
              aria-label={`第 ${index + 1} 版`}
              onClick={() => goTo(index, true)}
              className={cn(
                "h-1.5 rounded-full transition-all duration-200",
                index === focus ? "w-5" : "w-1.5",
                take.taskId === chosenTaskId
                  ? "bg-primary"
                  : isPlayable(take)
                    ? "bg-white/50"
                    : isRunning(take)
                      ? "bg-white/25"
                      : "bg-destructive/70",
              )}
            />
          ))}
        </div>
      )}
      <RejectTakeDialog
        open={rejectOpen}
        onOpenChange={setRejectOpen}
        segment={segment ?? null}
        taskId={rejectTaskId}
        expectedSegmentVersion={studio?.episode?.segmentVersion}
        onReject={async (input) => {
          if (studio && segment && rejectTaskId)
            await studio.actions.patchTakeQc(segment, rejectTaskId, input);
        }}
      />
    </div>
  );
}
