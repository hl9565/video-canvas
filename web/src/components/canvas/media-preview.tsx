import { useEffect, useState } from "react";
import { motion } from "motion/react";

import { DURATION, EASE_OUT, EASE_OUT_CSS } from "@/lib/motion";
import { cn } from "@/lib/utils";
import { resolveImageLayers, variantUrl, type ImageLoadPhase } from "@/utils/canvas/media-lod";
import { isMediaPending, type MediaLoadState } from "@/utils/canvas/media-pending";

import { useLowDetail, useNodeSelected } from "./hooks/use-low-detail";
import { MediaSkeleton } from "./media-skeleton";
import { ImagePlaceholderIcon, VideoPlaceholderIcon } from "./placeholder-icons";

const IMAGE_FADE = {
  transitionDuration: `${DURATION.base}s`,
  transitionTimingFunction: EASE_OUT_CSS,
};

type MediaPreviewProps = {
  /** 素材地址（payload 里的稳定地址，不带变体） */
  src: string;
  /** 替代文字 */
  alt?: string;
  /**
   * 加载策略：
   * lod 按画布缩放与节点选中状态在缩略图和原图间切换，只能用在画布节点内；
   * thumb 固定只看缩略图（素材抽屉、历史条等小图位），缩略图失败才退回原图；
   * full 不依赖画布缩放，先出缩略图、原图加载完再叠上去（对话结果格等画布外的大图位）。
   */
  mode?: "lod" | "thumb" | "full";
  /** img 的 object-fit 类，默认 contain */
  fit?: "contain" | "cover";
  className?: string;
  /** 拖拽缩略图时要禁掉浏览器原生的图片拖拽 */
  draggable?: boolean;
};

/**
 * 图片预览的统一出口：先等严格缩略图加载，低缩放且未选中时只看缩略图。
 * 放大或选中后请求原图，加载完成再淡入，避免闪白。
 * 缩略图失败自动改用原图，原图也失败才显示破图占位。
 * 两层都还没加载出来时盖一层灰色扫光占位（设计稿 6.12），缩略图到了就淡入替换它。
 * 填满父级盒子，画幅由父级决定（节点预览框固定 16:9）。
 */
export function MediaPreview({ mode = "lod", ...rest }: MediaPreviewProps) {
  return mode === "lod" ? (
    <LodImage {...rest} />
  ) : (
    <ImageLayers {...rest} preferThumb={mode === "thumb"} />
  );
}

/** 订阅缩放与选中的外壳：布尔值翻转才会重渲染 */
function LodImage(props: Omit<MediaPreviewProps, "mode">) {
  const low = useLowDetail();
  const selected = useNodeSelected();
  return <ImageLayers {...props} preferThumb={low && !selected} />;
}

type ImageLayersProps = Omit<MediaPreviewProps, "mode"> & {
  /** 此刻是否只需要缩略图 */
  preferThumb: boolean;
};

/** src 变了就整体重置加载状态 */
function ImageLayers(props: ImageLayersProps) {
  return <ImageLayersInner key={props.src} {...props} />;
}

function ImageLayersInner({
  src,
  alt = "",
  preferThumb,
  fit = "contain",
  className,
  draggable,
}: ImageLayersProps) {
  const thumb = variantUrl(src, "thumb", { strict: true });
  const [thumbState, setThumbState] = useState<ImageLoadPhase>("pending");
  const [originalState, setOriginalState] = useState<ImageLoadPhase>("pending");
  const [originalStarted, setOriginalStarted] = useState(false);

  const { showThumb, startOriginal, showOriginal } = resolveImageLayers({
    hasVariant: thumb.hasVariant,
    thumbState,
    preferThumb,
    originalStarted,
  });
  if (startOriginal && !originalStarted) setOriginalStarted(true);

  const originalVisible = originalState === "loaded" && (!preferThumb || !showThumb);
  const showBroken = originalState === "failed" && thumbState !== "loaded";
  const fitClass = fit === "cover" ? "object-cover" : "object-contain";
  const layerClass = cn(
    "absolute inset-0 size-full transition-opacity motion-reduce:transition-none",
    fitClass,
  );
  // 只统计此刻真正在渲染的图层，没渲染的缩略图 / 原图不能算“还在等”
  const pending = isMediaPending([
    ...(showThumb ? [thumbState] : []),
    ...(showOriginal ? [originalState] : []),
  ]);

  if (showBroken) {
    return (
      <div
        role="img"
        aria-label={alt || "图片加载失败"}
        className={cn(
          "text-muted-foreground/45 flex size-full items-center justify-center",
          className,
        )}
      >
        <ImagePlaceholderIcon className="size-1/3 max-w-10" />
      </div>
    );
  }

  return (
    <div className={cn("relative size-full", className)}>
      {showThumb && (
        <img
          src={thumb.url}
          alt={alt}
          loading="lazy"
          decoding="async"
          draggable={draggable}
          style={{
            ...IMAGE_FADE,
            opacity: thumbState === "loaded" && !originalVisible ? 1 : 0,
          }}
          onLoad={() => setThumbState("loaded")}
          onError={() => setThumbState("failed")}
          ref={(el: HTMLImageElement | null) => {
            if (thumbState === "pending" && el?.complete && el.naturalWidth > 0) {
              setThumbState("loaded");
            }
          }}
          className={layerClass}
        />
      )}
      {showOriginal && (
        <img
          src={src}
          alt={showThumb ? "" : alt}
          loading="lazy"
          decoding="async"
          draggable={draggable}
          style={{ ...IMAGE_FADE, opacity: originalVisible ? 1 : 0 }}
          onLoad={() => setOriginalState("loaded")}
          onError={() => setOriginalState("failed")}
          ref={(el: HTMLImageElement | null) => {
            if (originalState === "pending" && el?.complete && el.naturalWidth > 0) {
              setOriginalState("loaded");
            }
          }}
          className={layerClass}
        />
      )}
      <MediaSkeleton pending={pending} />
    </div>
  );
}

type VideoPosterProps = {
  /** 视频地址（不带变体） */
  src: string;
  className?: string;
  /** 图标大小类，占位用 */
  iconClassName?: string;
  /**
   * 封面“尘埃落定”时回调：封面加载完成，或确定没有封面（无变体 / 请求失败）。
   * 调用方据此让播放按钮、时长角标和封面一起淡入。
   */
  onSettledChange?: (settled: boolean) => void;
};

/**
 * 严格视频封面：不挂载 video，零视频文件请求。
 * 封面到之前盖一层灰色扫光占位（设计稿 6.12），到了淡入替换它。
 * 地址不归后端管、或封面请求失败（404 等）时显示视频占位图标。
 */
export function VideoPoster({ src, ...rest }: VideoPosterProps) {
  return <VideoPosterInner key={src} src={src} {...rest} />;
}

function VideoPosterInner({ src, className, iconClassName, onSettledChange }: VideoPosterProps) {
  const poster = variantUrl(src, "poster", { strict: true });
  const [state, setState] = useState<MediaLoadState>("pending");

  const noPoster = !poster.hasVariant || state === "failed";
  const settled = noPoster || state === "loaded";
  useEffect(() => {
    onSettledChange?.(settled);
  }, [settled, onSettledChange]);

  if (noPoster) {
    return (
      <span className="text-muted-foreground/45 grid size-full place-items-center">
        <VideoPlaceholderIcon className={cn("size-1/3 max-w-10", iconClassName)} />
      </span>
    );
  }
  return (
    <div className="relative size-full">
      <motion.img
        src={poster.url}
        alt=""
        loading="lazy"
        decoding="async"
        draggable={false}
        initial={false}
        animate={{ opacity: state === "loaded" ? 1 : 0 }}
        transition={{ duration: DURATION.base, ease: EASE_OUT }}
        onLoad={() => setState("loaded")}
        onError={() => setState("failed")}
        ref={(el: HTMLImageElement | null) => {
          if (state === "pending" && el?.complete && el.naturalWidth > 0) setState("loaded");
        }}
        className={cn("size-full object-cover", className)}
      />
      <MediaSkeleton pending={isMediaPending([state])} />
    </div>
  );
}
