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
  type: "enum" | "number" | "boolean",
): ParamEntry | undefined {
  return paramEntries(caps).find(
    (entry) => entry.open && entry.type === type && entry.name === name,
  );
}

function openBooleanFields(caps: Capabilities | undefined): ParamEntry[] {
  return paramEntries(caps).filter((entry) => entry.open && entry.type === "boolean");
}

function openCountField(caps: Capabilities | undefined): ParamEntry | undefined {
  return paramEntries(caps).find(
    (entry) =>
      entry.open &&
      (entry.name === "count" || entry.fanout) &&
      (entry.type === "enum" || entry.type === "number"),
  );
}

function booleanFieldChecked(overrides: Record<string, unknown>, field: ParamEntry): boolean {
  const raw = overrides[field.name] ?? field.default;
  return raw === true || raw === "true";
}

function booleanFieldLabel(field: ParamEntry): string {
  if (field.label) return field.label;
  return field.name === "generate_audio" ? "生成音频" : field.name;
}

function countOptionsFor(field: ParamEntry, singleShot: boolean): number[] {
  let values: number[] = [];
  if (field.type === "enum" && field.options?.length) {
    values = [
      ...new Set(
        field.options.map((opt) => Number(opt)).filter((n) => Number.isInteger(n) && n >= 1),
      ),
    ];
  } else if (field.type === "number") {
    const min = Math.max(1, Math.trunc(field.min ?? 1));
    const max = Math.max(min, Math.min(8, Math.trunc(field.max ?? min)));
    const step = Math.max(1, Math.trunc(field.step ?? 1));
    for (let n = min; n <= max; n += step) values.push(n);
  }
  if (singleShot && !values.includes(1)) {
    values = [1, ...values];
  }
  return values;
}

function resolvedCountValue(overrides: Record<string, unknown>, field: ParamEntry): number {
  const raw = overrides[field.name] ?? field.default;
  const num = Number(raw);
  return Number.isInteger(num) && num >= 1 ? num : 1;
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
function AudioWaveIcon({ active }: { active: boolean }) {
  return (
    <svg
      aria-hidden="true"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      className="size-3.5 shrink-0"
    >
      <polygon points="11 5 6 9 2 9 2 15 6 15 11 19 11 5" />
      {active ? (
        <>
          <path d="M15.54 8.46a5 5 0 0 1 0 7.07" />
          <path d="M19.07 4.93a10 10 0 0 1 0 14.14" />
        </>
      ) : (
        <>
          <line x1="22" y1="9" x2="16" y2="15" />
          <line x1="16" y1="9" x2="22" y2="15" />
        </>
      )}
    </svg>
  );
}

function CountGridIcon() {
  return (
    <svg
      aria-hidden="true"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      className="size-3.5 shrink-0"
    >
      <rect width="7" height="7" x="3" y="3" rx="1" />
      <rect width="7" height="7" x="14" y="3" rx="1" />
      <rect width="7" height="7" x="14" y="14" rx="1" />
      <rect width="7" height="7" x="3" y="14" rx="1" />
    </svg>
  );
}

function BooleanToggleChip({ field }: { field: ParamEntry }) {
  const { overrides, setOverride } = useStudio();
  const checked = booleanFieldChecked(overrides, field);
  const label = booleanFieldLabel(field);
  return (
    <button
      type="button"
      aria-pressed={checked}
      title={`${label}（当前：${checked ? "开启" : "关闭"}，按当前视频模型生效）`}
      onClick={() => setOverride(field.name, !checked)}
      className={
        checked
          ? "bg-muted text-foreground focus-visible:ring-ring/50 inline-flex h-7 items-center gap-1.5 rounded-md px-2 text-xs font-medium outline-none transition-colors focus-visible:ring-2"
          : "text-muted-foreground hover:bg-muted hover:text-foreground focus-visible:ring-ring/50 inline-flex h-7 items-center gap-1.5 rounded-md px-2 text-xs outline-none transition-colors focus-visible:ring-2"
      }
    >
      {field.name === "generate_audio" && <AudioWaveIcon active={checked} />}
      <span>{label}</span>
      <span
        className={
          checked
            ? "bg-primary/15 text-primary rounded px-1 py-0.5 text-[10px] leading-none"
            : "bg-muted text-muted-foreground rounded px-1 py-0.5 text-[10px] leading-none"
        }
      >
        {checked ? "开" : "关"}
      </span>
    </button>
  );
}

function BooleanToggleRows({ fields }: { fields: ParamEntry[] }) {
  const { overrides, setOverride } = useStudio();
  if (fields.length === 0) return null;
  return (
    <div className="flex flex-col gap-1 border-t pt-2">
      <div className="text-muted-foreground px-1.5 text-xs">生成开关</div>
      {fields.map((field) => {
        const checked = booleanFieldChecked(overrides, field);
        const label = booleanFieldLabel(field);
        return (
          <button
            key={field.name}
            type="button"
            aria-pressed={checked}
            onClick={() => setOverride(field.name, !checked)}
            className={
              checked
                ? "bg-muted hover:bg-muted flex min-h-8 items-center justify-between rounded-md px-2 text-left text-sm font-medium"
                : "hover:bg-muted flex min-h-8 items-center justify-between rounded-md px-2 text-left text-sm"
            }
          >
            <span className="flex items-center gap-2">
              {field.name === "generate_audio" && <AudioWaveIcon active={checked} />}
              <span>{label}</span>
            </span>
            <span
              className={
                checked
                  ? "bg-primary/15 text-primary rounded px-1.5 py-0.5 text-xs"
                  : "text-muted-foreground text-xs"
              }
            >
              {checked ? "开" : "关"}
            </span>
          </button>
        );
      })}
    </div>
  );
}

function CountOptions({
  field,
  current,
  singleShot,
  onSelect,
}: {
  field: ParamEntry;
  current: number;
  singleShot: boolean;
  onSelect: (value: number) => void;
}) {
  const options = countOptionsFor(field, singleShot);
  return (
    <div className="flex flex-col gap-0.5">
      <div className="text-muted-foreground flex items-center justify-between px-1.5 pt-1 text-xs">
        <span>{field.label || "生成数量"}（仅本片段）</span>
        {singleShot && <span className="text-amber-600 dark:text-amber-400">单镜模式锁定</span>}
      </div>
      {options.map((count) => {
        const blocked = singleShot && count !== 1;
        const active = count === current;
        return (
          <button
            key={count}
            type="button"
            disabled={blocked}
            aria-pressed={active}
            title={
              blocked
                ? "精裁模式（edit_schema=1）单次仅支持生成 1 条"
                : `为当前片段生成 ${count} 条`
            }
            onClick={() => onSelect(count)}
            className={
              active
                ? "bg-muted hover:bg-muted flex min-h-8 items-center justify-between rounded-md px-2 text-left text-sm font-medium disabled:cursor-not-allowed disabled:opacity-40"
                : "hover:bg-muted flex min-h-8 items-center justify-between rounded-md px-2 text-left text-sm disabled:cursor-not-allowed disabled:opacity-40"
            }
          >
            <span>{count} 条</span>
            {blocked && <span className="text-muted-foreground text-xs">单镜锁定</span>}
          </button>
        );
      })}
    </div>
  );
}

function useSingleShot(): boolean {
  const { drama, selected } = useStudio();
  if (!selected) return false;
  return drama.episodes?.find((item) => item.no === selected.episodeNo)?.editSchema === 1;
}

function BooleanChips() {
  const { video } = useStudio();
  const fields = openBooleanFields(video.model?.capabilities);
  if (fields.length === 0) return null;
  return (
    <>
      {fields.map((field) => (
        <BooleanToggleChip key={field.name} field={field} />
      ))}
    </>
  );
}

function CountChip() {
  const { selected, video, overrides, setOverride } = useStudio();
  const singleShot = useSingleShot();
  const field = openCountField(video.model?.capabilities);
  if (!field || !selected) return null;
  const current = resolvedCountValue(overrides, field);
  const effective = singleShot && current === 1 ? 1 : current;
  const warn = singleShot && current > 1;
  return (
    <ChipPopover
      variant="bar"
      icon={<CountGridIcon />}
      tone={warn ? "warn" : undefined}
      label={warn ? `${effective} 条（需为 1）` : `${effective} 条`}
      title={
        singleShot
          ? "当前集已启用单镜精裁约束，仅可生成 1 条（不跨片段继承）"
          : "设置当前片段的生成数量（不跨片段继承）"
      }
      width="w-56"
    >
      {(close) => (
        <CountOptions
          field={field}
          current={current}
          singleShot={singleShot}
          onSelect={(value) => {
            close();
            setOverride(field.name, value);
          }}
        />
      )}
    </ChipPopover>
  );
}

function MobileSpecsChip() {
  const { drama, selected, video, overrides, setOverride } = useStudio();
  const singleShot = useSingleShot();
  const caps = video.model?.capabilities;
  const resField = openStudioField(caps, "resolution", "enum");
  const countField = openCountField(caps);
  const boolFields = openBooleanFields(caps);
  const seed = useStudioSeed();
  const resHit = resField
    ? matchOption(resField, overrides[resField.name] ?? resField.default)
    : undefined;
  const curCount = countField ? resolvedCountValue(overrides, countField) : 1;
  const durationAdjusted = selected ? effectiveDuration(caps, selected.seconds).adjusted : false;
  const hasWarn = durationAdjusted || !!seed?.error || (singleShot && curCount > 1);
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
          {countField && selected && (
            <CountOptions
              field={countField}
              current={curCount}
              singleShot={singleShot}
              onSelect={(value) => {
                close();
                setOverride(fieldValueKey(countField), value);
              }}
            />
          )}
          <BooleanToggleRows fields={boolFields} />
          {seed && <SeedEditor />}
        </div>
      )}
    </ChipPopover>
  );
}

function fieldValueKey(field: ParamEntry): string {
  return field.name;
}

const NO_JOBS: never[] = [];

/**
 * 片场顶部条：集名、保存状态、规格、重新生成分镜脚本，以及 GenerateControls 内并列的首帧控镜与立即生成。
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
        <CountChip />
        <BooleanChips />
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
