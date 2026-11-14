import {
  CloudAlert,
  CloudCheck,
  Hash,
  Loader2,
  MonitorPlay,
  RotateCw,
  SlidersHorizontal,
  Timer,
} from "lucide-react";

import type { Capabilities } from "@/api/model/type";
import { NumberControl } from "@/components/canvas/video-param-panel";
import { ChipPopover, MenuLabel } from "@/components/drama/pickers";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { useDramaJobsStore } from "@/store/drama";
import { jobConflictReason } from "@/utils/drama/jobs";
import { durationChoices, effectiveDuration, modelMaxSeconds } from "@/utils/drama/video-models";
import {
  effectiveValue,
  matchOption,
  paramEntries,
  validateParamValue,
  type ParamEntry,
} from "@/utils/tasks/capabilities";

import { useStoryboardStart } from "../workspace/storyboard-start";
import { GenerateControls } from "./generate-controls";
import type { SaveSummary } from "../workspace/use-workspace";
import { useStudio } from "./use-studio";

const SAVE_LABEL: Record<SaveSummary, { text: string; icon: React.ReactNode; tone?: string }> = {
  saved: { text: "已保存", icon: <CloudCheck className="size-3.5" /> },
  saving: { text: "保存中", icon: <Loader2 className="size-3.5 animate-spin" /> },
  error: {
    text: "保存失败，将重试",
    icon: <CloudAlert className="size-3.5" />,
    tone: "text-destructive",
  },
  conflict: {
    text: "有冲突，已停止保存",
    icon: <CloudAlert className="size-3.5" />,
    tone: "text-status-warning",
  },
};

function openStudioField(
  caps: Capabilities | undefined,
  name: string,
  type: "enum" | "number",
): ParamEntry | undefined {
  return paramEntries(caps).find(
    (entry) => entry.open && entry.type === type && entry.name === name,
  );
}

/** 分辨率选项列表（宽栏与窄屏共用） */
function ResolutionOptions({
  field,
  hit,
  onSelect,
}: {
  field: ParamEntry;
  hit: unknown;
  onSelect: (option: unknown) => void;
}) {
  return (
    <>
      <MenuLabel>{field.label}</MenuLabel>
      {(field.options ?? []).map((option) => (
        <button
          key={String(option)}
          type="button"
          onClick={() => onSelect(option)}
          className={cn(
            "hover:bg-muted focus-visible:ring-ring/50 flex min-h-8 items-center rounded-md px-2 text-left text-sm outline-none focus-visible:ring-2",
            String(option) === String(hit) && "bg-muted font-medium",
          )}
        >
          {String(option)}
        </button>
      ))}
    </>
  );
}

/** 分辨率：模型有开放的分辨率参数才显示；选择存在本机，按短剧分开 */
function ResolutionChip() {
  const { video, overrides, setOverride } = useStudio();
  const field = openStudioField(video.model?.capabilities, "resolution", "enum");
  if (!field) return null;
  const current = overrides[field.name] ?? field.default;
  const hit = matchOption(field, current);
  return (
    <ChipPopover
      variant="bar"
      icon={<MonitorPlay />}
      label={String(hit ?? field.default ?? "分辨率")}
    >
      {(close) => (
        <ResolutionOptions
          field={field}
          hit={hit}
          onSelect={(option) => {
            close();
            setOverride(field.name, option);
          }}
        />
      )}
    </ChipPopover>
  );
}

/** 时长选项列表（宽栏与窄屏共用） */
function SecondsOptions({ close }: { close: () => void }) {
  const { selected, actions, video } = useStudio();
  if (!selected) return null;
  const caps = video.model?.capabilities;
  const max = modelMaxSeconds(caps) ?? 15;
  const options = durationChoices(caps, selected.seconds);
  return (
    <>
      <MenuLabel>片段时长（当前模型上限 {max}s）</MenuLabel>
      {options.map((seconds) => {
        const fit = effectiveDuration(caps, seconds);
        const hint = fit.adjusted
          ? seconds > max
            ? `超出模型上限，将按 ${fit.value}s 提交`
            : `不在模型档位，将按 ${fit.value}s 提交`
          : "";
        return (
          <button
            key={seconds}
            type="button"
            onClick={() => {
              close();
              void actions.setSeconds(selected, seconds);
            }}
            className={cn(
              "hover:bg-muted focus-visible:ring-ring/50 flex min-h-8 items-center gap-2 rounded-md px-2 text-left text-sm tabular-nums outline-none focus-visible:ring-2",
              seconds === selected.seconds && "bg-muted font-medium",
              fit.adjusted && "text-status-warning",
            )}
          >
            <span>{seconds}s</span>
            {hint && <span className="ml-auto text-[11px]">{hint}</span>}
          </button>
        );
      })}
    </>
  );
}

/** 当前片段的时长：按当前视频模型的合法时长（含 2s / 非标准枚举 / step）生成候选；不符合当前模型时在按钮与列表均提示 */
function SecondsChip() {
  const { selected, video } = useStudio();
  if (!selected) return null;
  const caps = video.model?.capabilities;
  const max = modelMaxSeconds(caps) ?? 15;
  const effective = effectiveDuration(caps, selected.seconds);
  const label = effective.adjusted
    ? `${selected.seconds}s→${effective.value}s`
    : `${selected.seconds}s`;
  const title = effective.adjusted
    ? `${selected.seconds}s 不在当前模型合法档位，将按 ${effective.value}s 提交`
    : `片段时长 · 模型上限 ${max}s`;
  return (
    <ChipPopover
      variant="bar"
      icon={<Timer />}
      tone={effective.adjusted ? "warn" : undefined}
      label={label}
      title={title}
      width="w-60"
    >
      {(close) => <SecondsOptions close={close} />}
    </ChipPopover>
  );
}

function useStudioSeed() {
  const { video, overrides, setOverride } = useStudio();
  const field = openStudioField(video.model?.capabilities, "seed", "number");
  if (!field) return null;
  const rawOverride = overrides.seed;
  const hasOverride = rawOverride !== undefined;
  const effective = effectiveValue(field, overrides, "seed");
  const checked =
    effective === undefined || effective === null ? null : validateParamValue(field, effective);
  const error = checked && "error" in checked ? checked.error : undefined;
  return { field, effective, hasOverride, error, setOverride };
}

/** 随机种子编辑块（宽栏与窄屏共用） */
function SeedEditor() {
  const seed = useStudioSeed();
  if (!seed) return null;
  return (
    <div className="px-1.5 py-1">
      <MenuLabel>{seed.field.label}</MenuLabel>
      <div className="mt-1">
        <NumberControl
          field={seed.field}
          value={seed.effective}
          hasOverride={seed.hasOverride}
          invalid={!!seed.error}
          error={seed.error}
          onChange={(next) => seed.setOverride("seed", next)}
        />
      </div>
    </div>
  );
}

/** 随机种子胶囊：仅当当前视频模型开放数值型 seed 时显示 */
function SeedChip() {
  const seed = useStudioSeed();
  if (!seed) return null;
  const { effective, error } = seed;
  const label =
    Number(effective) === -1
      ? "种子 随机"
      : effective !== undefined && effective !== null
        ? `种子 ${effective}`
        : "种子";
  return (
    <ChipPopover
      variant="bar"
      icon={<Hash />}
      tone={error ? "warn" : undefined}
      label={label}
      title={error ?? "随机种子设置（-1 为随机，点击恢复默认可回到模型默认）"}
      width="w-72"
    >
      {() => <SeedEditor />}
    </ChipPopover>
  );
}

/** 窄屏收纳：在 md 以下提供完整时长、分辨率、seed 与比例入口，避免 max-md:hidden 造成无入口 */
function MobileSpecsChip() {
  const { drama, selected, video, overrides, setOverride } = useStudio();
  const caps = video.model?.capabilities;
  const resField = openStudioField(caps, "resolution", "enum");
  const seed = useStudioSeed();
  const resHit = resField
    ? matchOption(resField, overrides[resField.name] ?? resField.default)
    : undefined;
  const durationAdjusted = selected ? effectiveDuration(caps, selected.seconds).adjusted : false;
  const hasWarn = durationAdjusted || !!seed?.error;
  return (
    <ChipPopover
      variant="bar"
      icon={<SlidersHorizontal />}
      tone={hasWarn ? "warn" : undefined}
      label="规格与参数"
      title="查看与修改当前分镜规格及随机种子"
      width="w-72"
    >
      {(close) => (
        <div className="flex max-h-80 flex-col gap-2 overflow-y-auto">
          <div className="text-muted-foreground flex items-center justify-between px-2 pt-1 text-xs">
            <span>画面比例</span>
            <span className="text-foreground font-mono">{drama.ratio || "比例未定"}</span>
          </div>
          {selected && <SecondsOptions close={close} />}
          {resField && (
            <ResolutionOptions
              field={resField}
              hit={resHit}
              onSelect={(option) => {
                close();
                setOverride(resField.name, option);
              }}
            />
          )}
          {seed && <SeedEditor />}
        </div>
      )}
    </ChipPopover>
  );
}

const NO_JOBS: never[] = [];

/**
 * 片场顶部条：集名、保存状态，右边是当前片段时长、分辨率、seed、比例、「重新生成分镜脚本」，最右是视频模型和「立即生成」。
 * 合成整集挪到了「成片」页；这里只管把片段做出来。
 */
export function StudioBar() {
  const { drama, episode, no, save, video } = useStudio();
  const jobs = useDramaJobsStore((state) => state.byDrama[drama.id]) ?? NO_JOBS;
  const starter = useStoryboardStart(video.maxSeconds);
  const status = SAVE_LABEL[save];
  const blocked = jobConflictReason(jobs, "storyboard", [no]);
  return (
    <div className="border-border flex h-12 shrink-0 items-center gap-2 border-b px-4">
      <h2 className="font-display min-w-0 truncate text-[17px] font-bold tracking-wide">
        第 {no} 集 · {episode.title}
      </h2>
      <span
        role="status"
        className={cn(
          "text-muted-foreground inline-flex items-center gap-1 text-xs max-sm:hidden",
          status.tone,
        )}
      >
        {status.icon}
        {status.text}
      </span>
      <span className="flex-1" />
      <div className="flex items-center gap-0.5 max-md:hidden">
        <SecondsChip />
        <ResolutionChip />
        <SeedChip />
        <span className="text-muted-foreground px-2 font-mono text-xs">
          {drama.ratio || "比例未定"}
        </span>
      </div>
      <div className="md:hidden">
        <MobileSpecsChip />
      </div>
      <Button
        variant="ghost"
        size="icon-sm"
        aria-label="重新生成分镜脚本"
        title={blocked ?? "重新生成分镜脚本（已有出视频的片段会先确认）"}
        disabled={!!blocked || starter.pending === no}
        onClick={() => void starter.start(episode)}
      >
        {starter.pending === no ? <Loader2 className="animate-spin" /> : <RotateCw />}
      </Button>
      <GenerateControls />
    </div>
  );
}
