import { useEffect, useId, useRef, useState } from "react";

import { listLibraryItems } from "@/api/asset-library";
import { dramaErrorMessage } from "@/api/drama/errors";
import type { Segment, SegmentFx, UpdateSegmentInput, VisualKind } from "@/api/drama/type";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Sheet, SheetContent, SheetFooter, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import {
  parseProgramAssetId,
  programTimeline,
  PROGRAM_MAX_FRAMES,
  VISUAL_RATIO_MESSAGE,
} from "@/utils/drama/program";
import type { RefAsset } from "@/utils/drama/segment-assets";

const controlClass = "border-input bg-background min-h-8 max-w-full rounded-lg border px-2 text-sm";

type AssetOption = { id: string; label: string; url?: string };

/** 手填十进制 ID 或从已有自有素材中选择。 */
export function AssetIdField({
  kind,
  value,
  onChange,
  assets = [],
}: {
  kind: "image" | "audio";
  value: string;
  onChange: (id: string) => void;
  assets?: AssetOption[];
}) {
  const listId = useId();
  const [items, setItems] = useState<AssetOption[]>([]);
  const [cursor, setCursor] = useState<string | null>(null);
  const [listed, setListed] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState(false);
  const request = useRef(0);
  useEffect(
    () => () => {
      request.current++;
    },
    [],
  );
  const load = async (more = false) => {
    const seq = ++request.current;
    setLoading(true);
    setError(false);
    try {
      const page = await listLibraryItems({
        folderId: "all",
        kind,
        ...(more && cursor ? { cursor } : {}),
      });
      if (request.current !== seq) return;
      const next = page.items.map(({ asset }) => ({
        id: asset.id,
        label: asset.fileName || asset.id,
        url: asset.url,
      }));
      setItems((prev) => (more ? [...prev, ...next] : next));
      setCursor(page.nextCursor ?? null);
      setListed(true);
    } catch {
      if (request.current === seq) setError(true);
    } finally {
      if (request.current === seq) setLoading(false);
    }
  };
  const options = [...new Map([...assets, ...items].map((asset) => [asset.id, asset])).values()];
  return (
    <div className="grid gap-2">
      <label className="grid gap-1 text-xs">
        {kind === "image" ? "图片素材 ID" : "音频素材 ID"}
        <Input
          inputMode="numeric"
          value={value}
          onChange={(event) => onChange(event.target.value)}
        />
      </label>
      <div className="flex flex-wrap items-end gap-2">
        {options.length > 0 && (
          <label className="grid min-w-0 flex-1 gap-1 text-xs" htmlFor={listId}>
            {kind === "image" ? "选择图片" : "选择音频"}
            <select
              id={listId}
              className={controlClass}
              value={options.some((asset) => asset.id === value) ? value : ""}
              onChange={(event) => onChange(event.target.value)}
            >
              <option value="" disabled>
                选择素材
              </option>
              {options.map((asset) => (
                <option key={asset.id} value={asset.id}>
                  {asset.label} · {asset.id}
                </option>
              ))}
            </select>
          </label>
        )}
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={loading}
          onClick={() => void load(Boolean(cursor))}
        >
          {loading ? "读取中…" : cursor ? "更多素材" : "浏览素材库"}
        </Button>
      </div>
      {error && (
        <p role="alert" className="text-destructive text-xs">
          素材库读取失败
        </p>
      )}
      {listed && !items.length && (
        <p className="text-muted-foreground text-xs">
          素材库暂无{kind === "image" ? "图片" : "音频"}
        </p>
      )}
    </div>
  );
}

const FRAME_FX = [
  ["fade_in_f", "画面淡入", 0, 2147483647, 1],
  ["fade_out_f", "画面淡出", 0, 2147483647, 1],
  ["flash_in_f", "起始闪帧", 0, 2147483647, 1],
  ["flash_out_f", "结束闪帧", 0, 2147483647, 1],
] as const;
const NATIVE_FX = [
  ["native_fade_in_f", "原声淡入", 0, 2147483647, 1],
  ["native_fade_out_f", "原声淡出", 0, 2147483647, 1],
] as const;
const STILL_FX = [
  ["zoom_start", "起始缩放", 1, 1.25, 0.01],
  ["zoom_end", "结束缩放", 1, 1.25, 0.01],
  ["shake_px", "抖动（像素）", 0, 6, 0.1],
  ["crop_y", "纵向裁剪位置", 0, 1, 0.01],
  ["seed", "随机种子", 0, 2147483647, 1],
] as const;
const fieldsOf = (kind: VisualKind) => [
  ...FRAME_FX,
  ...(kind === "video" ? NATIVE_FX : kind === "still" ? STILL_FX : []),
];

const isFrameRunActive = (segment: Segment) =>
  !!segment.frameRun && !["bound", "failed", "cancelled"].includes(segment.frameRun.state);

type DrawerProps = {
  open: boolean;
  ratio: string;
  segment: Segment | null;
  assets: readonly RefAsset[];
  onOpenChange: (open: boolean) => void;
  onSave: (
    input: Omit<UpdateSegmentInput, "revision">,
    options: { expectedRevision: number },
  ) => Promise<unknown>;
};

function VisualForm({
  segment,
  ratio,
  assets,
  onSave,
  onOpenChange,
}: Omit<DrawerProps, "segment" | "open"> & { segment: Segment }) {
  const [revision] = useState(segment.revision);
  const original = segment.visualKind ?? "video";
  const [kind, setKind] = useState<VisualKind>(original);
  const [still, setStill] = useState(
    () =>
      segment.stillAssetIdText ??
      (typeof segment.stillAssetId === "number" && Number.isSafeInteger(segment.stillAssetId)
        ? String(segment.stillAssetId)
        : ""),
  );
  const [duration, setDuration] = useState(() =>
    String(
      (original !== "video" ? segment.edit?.sourceOutF : null) ??
        Math.max(1, Math.round(segment.seconds * 24)),
    ),
  );
  const [fx, setFx] = useState<Partial<Record<keyof SegmentFx, string>>>(() =>
    Object.fromEntries(
      Object.entries(segment.fx ?? {}).map(([key, value]) => [key, String(value)]),
    ),
  );
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const allowedRatio = ratio === "16:9";
  const active = isFrameRunActive(segment);
  const selectedAsset = parseProgramAssetId(still);
  const ownedImages = assets.flatMap((asset) =>
    asset.kind === "image" && asset.assetId
      ? [{ id: asset.assetId, label: asset.label || asset.name }]
      : [],
  );

  const save = async () => {
    if ((!allowedRatio || active) && kind !== "video") return;
    if (saving) return;
    const next: Omit<UpdateSegmentInput, "revision"> = {};
    if (kind !== original) next.visualKind = kind;
    const frames = Number(duration);
    if (kind !== "video") {
      if (!Number.isInteger(frames) || frames < 1 || frames > PROGRAM_MAX_FRAMES) {
        return setError(`片段时长须为 1 至 ${PROGRAM_MAX_FRAMES} 整数帧`);
      }
      if (kind === "still" && selectedAsset === null)
        return setError("请选择图片或输入有效的十进制素材 ID");
      next.edit = { sourceInF: 0, sourceOutF: frames };
      next.stillAssetId = kind === "still" ? selectedAsset : null;
    } else if (original !== "video") {
      next.stillAssetId = null;
    }

    const effects: SegmentFx = {};
    if (allowedRatio) {
      for (const [key, label, min, max, step] of fieldsOf(kind)) {
        const raw = fx[key]?.trim();
        if (!raw) continue;
        const value = Number(raw);
        if (
          !Number.isFinite(value) ||
          value < min ||
          value > max ||
          (step === 1 && !Number.isInteger(value))
        ) {
          return setError(`${label}须为 ${min} 至 ${max}${step === 1 ? " 的整数" : ""}`);
        }
        if (!key.endsWith("_f") || value !== 0) effects[key] = value;
      }
      const total =
        kind === "video"
          ? original === "video"
            ? programTimeline([segment]).totalFrames
            : 0
          : frames;
      const pairs = [
        [effects.fade_in_f, effects.fade_out_f],
        [effects.flash_in_f, effects.flash_out_f],
        [effects.native_fade_in_f, effects.native_fade_out_f],
      ];
      if (total > 0 && pairs.some(([start, end]) => (start ?? 0) + (end ?? 0) > total)) {
        return setError("淡入、淡出或闪帧超过片段长度");
      }
      if (Object.keys(effects).length || Object.keys(segment.fx ?? {}).length) next.fx = effects;
    }
    if (!Object.keys(next).length) {
      onOpenChange(false);
      return;
    }
    setError(null);
    setSaving(true);
    try {
      await onSave(next, { expectedRevision: revision });
      onOpenChange(false);
    } catch (err) {
      setError(dramaErrorMessage(err, "保存画面设置失败"));
    } finally {
      setSaving(false);
    }
  };

  return (
    <form
      className="flex min-h-0 flex-1 flex-col"
      onSubmit={(event) => {
        event.preventDefault();
        void save();
      }}
    >
      <div className="grid min-h-0 content-start gap-4 overflow-y-auto px-4 pb-4">
        {!allowedRatio && (
          <p role="status" className="text-status-warning text-xs">
            {VISUAL_RATIO_MESSAGE}
          </p>
        )}
        {active && (
          <p role="status" className="text-status-warning text-xs">
            首帧控镜正在进行，暂不能切换到静帧或黑场
          </p>
        )}
        <label className="grid gap-1 text-xs">
          画面模式
          <select
            className={controlClass}
            value={kind}
            onChange={(event) => setKind(event.target.value as VisualKind)}
          >
            <option value="video">视频</option>
            <option value="still" disabled={!allowedRatio || active}>
              静帧
            </option>
            <option value="black" disabled={!allowedRatio || active}>
              黑场
            </option>
          </select>
        </label>
        {kind === "still" && (
          <AssetIdField kind="image" assets={ownedImages} value={still} onChange={setStill} />
        )}
        {kind !== "video" && (
          <label className="grid gap-1 text-xs">
            时长（24fps 帧）
            <Input
              type="number"
              min={1}
              max={PROGRAM_MAX_FRAMES}
              step={1}
              value={duration}
              onChange={(event) => setDuration(event.target.value)}
            />
          </label>
        )}
        <fieldset disabled={!allowedRatio} className="grid grid-cols-2 gap-2 disabled:opacity-55">
          <legend className="mb-2 text-xs font-medium">特效</legend>
          {fieldsOf(kind).map(([key, label, min, max, step]) => (
            <label key={key} className="grid gap-1 text-xs">
              {label}
              {key.endsWith("_f") ? "（帧）" : ""}
              <Input
                type="number"
                min={min}
                max={max}
                step={step === 1 ? 1 : "any"}
                value={fx[key] ?? ""}
                onChange={(event) => setFx((prev) => ({ ...prev, [key]: event.target.value }))}
              />
            </label>
          ))}
        </fieldset>
        {error && (
          <p role="alert" className="text-destructive text-xs">
            {error}
          </p>
        )}
      </div>
      <SheetFooter className="border-border border-t">
        <Button type="submit" disabled={saving || ((!allowedRatio || active) && kind !== "video")}>
          {saving ? "保存中…" : "保存画面"}
        </Button>
      </SheetFooter>
    </form>
  );
}

export function SegmentVisualDrawer(props: DrawerProps) {
  const { open, onOpenChange, segment } = props;
  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent className="sm:max-w-md">
        <SheetHeader>
          <SheetTitle>画面与特效</SheetTitle>
        </SheetHeader>
        {segment && <VisualForm key={segment.id} {...props} segment={segment} />}
      </SheetContent>
    </Sheet>
  );
}
