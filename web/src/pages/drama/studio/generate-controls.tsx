import { Loader2 } from "lucide-react";

import type { TaskQuote } from "@/api/drama/type";
import { VideoModelMenu } from "@/components/drama/video-model-menu";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { formatLabel } from "@/utils/drama/adaptation";
import { hasVideo } from "@/utils/drama/segments";

import { useStudio } from "./use-studio";

function formatQuoteSummary(quote: TaskQuote): string {
  const input = quote.effectiveInput ?? {};
  const parts: string[] = [];
  if (quote.rawSize) parts.push(quote.rawSize);
  const res = typeof input.resolution === "string" ? input.resolution.trim() : "";
  if (res && !quote.rawSize.toLowerCase().includes(res.toLowerCase())) {
    parts.push(res);
  }
  const rawDuration = input.duration ?? input.seconds;
  if (typeof rawDuration === "number" && Number.isFinite(rawDuration) && rawDuration > 0) {
    parts.push(`${rawDuration}s`);
  }
  const rawCount = input.count;
  if (typeof rawCount === "number" && Number.isFinite(rawCount) && rawCount > 0) {
    parts.push(`${rawCount}条`);
  }
  const rawAudio = input.generate_audio;
  if (typeof rawAudio === "boolean") {
    parts.push(rawAudio ? "音频开" : "音频关");
  }
  return parts.join(" · ");
}

/** 为什么现在不能生成；能生成为 null（写进按钮的悬停提示） */
function useBlockedReason(): string | null {
  const { episode, selected, plan, video, submitting, format, adaptStateOf } = useStudio();
  if (!selected || !plan) return "还没有片段";
  if (selected.status === "running") return "这个片段正在生成";
  if (submitting.has(selected.id)) return "正在提交";
  if (video.status === "idle" || video.status === "loading") return "正在加载模型清单";
  if (!video.model) return "没有可用的视频模型（需要支持参考图）";
  const adapt = adaptStateOf(selected);
  if (adapt === "busy") return "正在转写";
  if (adapt === "none" && format !== "seedance") return `先把原稿转写为 ${formatLabel(format)}`;
  if (
    episode?.editSchema === 1 &&
    plan.input?.count !== undefined &&
    Number(plan.input.count) !== 1
  ) {
    return "精裁模式仅支持单条生成（请将数量改为 1）";
  }
  return Object.values(plan.errors)[0] ?? null;
}

/**
 * 片场顶栏右侧的生成操作：视频模型选择 + 「立即生成 · 积分」。
 * 生成的是当前选中的片段；积分不够时数字标红，原因写在悬停提示里。
 */
export function GenerateControls() {
  const {
    selected,
    credits,
    quoteState,
    availableCredits,
    actions,
    submitting,
    video,
    frameRunDrawer,
  } = useStudio();
  const blocked = useBlockedReason();
  const busy = !!selected && (submitting.has(selected.id) || selected.status === "running");
  const out = !!selected && hasVideo(selected);
  const activeQuote = quoteState?.quote ?? null;
  const quoteSummary = activeQuote ? formatQuoteSummary(activeQuote) : "";
  const displayCredits = activeQuote?.credits ?? credits;
  const lowCredits =
    displayCredits !== undefined && availableCredits !== null && displayCredits > availableCredits;

  return (
    <div className="flex items-center gap-1.5">
      <VideoModelMenu
        models={video.models}
        loading={video.status === "idle" || video.status === "loading"}
        model={video.model}
        onPick={video.setModel}
      />
      {quoteSummary && (
        <span
          data-slot="quote-summary"
          title={`服务端生效报价参数：${quoteSummary}`}
          className="bg-muted/70 text-muted-foreground hidden items-center rounded-md border px-2 py-0.5 font-mono text-[11px] xl:inline-flex"
        >
          {quoteSummary}
        </span>
      )}
      {quoteState?.expired && (
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={quoteState.loading}
          title={quoteState.error ?? "报价已失效，点击刷新最新报价"}
          onClick={() => void quoteState.refresh()}
        >
          {quoteState.loading && <Loader2 className="animate-spin" />}
          刷新报价
        </Button>
      )}
      <Button
        type="button"
        size="sm"
        variant="outline"
        disabled={!selected || !frameRunDrawer}
        onClick={() => selected && frameRunDrawer?.open(selected.id)}
      >
        首帧控镜
      </Button>
      <Button
        size="sm"
        disabled={!!blocked}
        title={
          blocked ??
          (quoteState?.expired
            ? "上次报价已过期，重新生成将自动刷新最新报价"
            : quoteState?.error
              ? quoteState.error
              : lowCredits
                ? `积分不足：本次约需 ${displayCredits}，当前可用 ${availableCredits}`
                : activeQuote
                  ? `已获取最新报价：${displayCredits} 积分${quoteSummary ? `（${quoteSummary}）` : ""}（⌘↵）`
                  : "开始生成这个片段的视频（⌘↵）")
        }
        onClick={() => selected && void actions.generate(selected)}
      >
        {busy && <Loader2 className="animate-spin" />}
        {out ? "再生成一次" : "立即生成"}
        {displayCredits !== undefined ? (
          <>
            <span aria-hidden className="opacity-50">
              ·
            </span>
            <span className={cn("tabular-nums", lowCredits && "text-destructive")}>
              {displayCredits}
            </span>
          </>
        ) : (
          quoteState?.loading && (
            <>
              <span aria-hidden className="opacity-50">
                ·
              </span>
              <span className="text-xs opacity-75">报价中</span>
            </>
          )
        )}
      </Button>
    </div>
  );
}
