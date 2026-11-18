import type { ProgramAudioCueInput, ProgramDiagnostic } from "@/api/drama/type";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  parseProgramAssetId,
  programDiagnosticText,
  resolveCue,
  type ProgramTimelineData,
} from "@/utils/drama/program";

import { AssetIdField } from "../segment-visual-drawer";

const selectClass = "border-input bg-background min-h-8 rounded-lg border px-2 text-xs";

function FrameField({
  label,
  value,
  onChange,
  min = 0,
}: {
  label: string;
  value: number;
  onChange: (value: number) => void;
  min?: number;
}) {
  return (
    <label className="grid min-w-0 gap-1 text-xs">
      {label}
      <Input
        type="number"
        step={1}
        min={min}
        value={value}
        onChange={(event) => {
          const n = event.target.valueAsNumber;
          if (Number.isFinite(n)) onChange(Math.max(min, Math.round(n)));
        }}
        onKeyDown={(event) => {
          if (event.key !== "ArrowUp" && event.key !== "ArrowDown") return;
          event.preventDefault();
          onChange(
            Math.max(min, value + (event.key === "ArrowUp" ? 1 : -1) * (event.shiftKey ? 10 : 1)),
          );
        }}
      />
    </label>
  );
}

export function CueInspector({
  cue,
  timeline,
  diagnostics,
  rebind,
  rebindDisabled,
  onRebind,
  onChange,
  onRemove,
}: {
  cue: ProgramAudioCueInput;
  timeline: ProgramTimelineData;
  diagnostics: readonly ProgramDiagnostic[];
  rebind: boolean;
  rebindDisabled: boolean;
  onRebind: (checked: boolean) => void;
  onChange: (cue: ProgramAudioCueInput) => void;
  onRemove: () => void;
}) {
  const videoClips = timeline.clips.filter((clip) => clip.video);

  const anchorTo = (segmentId: string) => {
    const clip = videoClips.find((item) => item.segmentId === segmentId);
    if (!clip || cue.role !== "sfx") return;
    const { start_f, end_f, ...core } = cue;
    const length = start_f !== undefined && end_f !== undefined ? end_f - start_f : cue.length_f;
    const source =
      start_f === undefined
        ? clip.sourceInF
        : clip.sourceInF +
          Math.max(0, Math.min(clip.masterOutF - clip.masterInF - 1, start_f - clip.masterInF));
    onChange({
      ...core,
      role: "sfx",
      anchor: { segment_id: segmentId, source_f: source },
      length_f: length ?? 1,
    });
  };

  const toggleAnchor = (value: string) => {
    if (value === "anchor" && videoClips.length) {
      const range = resolveCue(cue, timeline);
      const nearest =
        videoClips.find(
          (item) => range && range.inF >= item.masterInF && range.inF < item.masterOutF,
        ) ?? videoClips[0];
      anchorTo(nearest.segmentId);
    } else if (cue.anchor) {
      const range = resolveCue(cue, timeline);
      const { anchor: _anchor, length_f: length, ...core } = cue;
      onChange({ ...core, role: "sfx", start_f: range?.inF ?? 0, end_f: range?.outF ?? length });
    }
  };

  return (
    <section
      aria-label="音频条目检查器"
      className="border-border grid content-start gap-2.5 border-t pt-2.5 lg:border-t-0 lg:border-l lg:pt-0 lg:pl-3"
    >
      <div className="flex items-center justify-between gap-2">
        <h4 className="text-xs font-semibold">音频条目</h4>
        <Button type="button" variant="ghost" size="xs" onClick={onRemove}>
          删除条目
        </Button>
      </div>
      <AssetIdField
        kind="audio"
        value={String(cue.asset_id || "")}
        onChange={(id) => {
          onChange({ ...cue, asset_id: parseProgramAssetId(id) ?? id });
        }}
      />
      {(cue.role === "bgm" || cue.role === "room_tone") && (
        <>
          <label className="grid gap-1 text-xs">
            背景声音类型
            <select
              className={selectClass}
              value={cue.role}
              onChange={(event) =>
                onChange({ ...cue, role: event.target.value === "room_tone" ? "room_tone" : "bgm" })
              }
            >
              <option value="bgm">配乐</option>
              <option value="room_tone">环境底噪</option>
            </select>
          </label>
          <div className="flex flex-wrap gap-x-3 gap-y-1 text-xs">
            <label className="inline-flex items-center gap-1.5">
              <input
                type="checkbox"
                checked={cue.duck_db === -8}
                onChange={(event) => {
                  const { duck_db: _duck, ...withoutDuck } = cue;
                  onChange(event.target.checked ? { ...withoutDuck, duck_db: -8 } : withoutDuck);
                }}
              />
              人声闪避 −8 dB
            </label>
            <label className="inline-flex items-center gap-1.5">
              <input
                type="checkbox"
                checked={cue.loop ?? false}
                onChange={(event) => onChange({ ...cue, loop: event.target.checked })}
              />
              循环
            </label>
          </div>
        </>
      )}
      {cue.role === "sfx" && (
        <label className="grid gap-1 text-xs">
          时间坐标
          <select
            className={selectClass}
            value={cue.anchor ? "anchor" : "absolute"}
            onChange={(event) => toggleAnchor(event.target.value)}
          >
            <option value="absolute">绝对区间</option>
            <option value="anchor" disabled={!videoClips.length && !cue.anchor}>
              视频源帧锚点
            </option>
          </select>
        </label>
      )}
      {cue.anchor ? (
        <>
          <label className="grid gap-1 text-xs">
            锚定视频
            <select
              className={selectClass}
              value={cue.anchor.segment_id}
              onChange={(event) => anchorTo(event.target.value)}
            >
              {!videoClips.some((clip) => clip.segmentId === cue.anchor.segment_id) && (
                <option value={cue.anchor.segment_id}>原镜头不可用</option>
              )}
              {videoClips.map((clip) => (
                <option key={clip.segmentId} value={clip.segmentId}>
                  镜头 {clip.seq + 1}
                </option>
              ))}
            </select>
          </label>
          <div className="grid grid-cols-2 gap-2">
            <FrameField
              label="源帧"
              value={cue.anchor.source_f}
              onChange={(source_f) => onChange({ ...cue, anchor: { ...cue.anchor, source_f } })}
            />
            <FrameField
              label="长度（帧）"
              value={cue.length_f}
              min={1}
              onChange={(length_f) => onChange({ ...cue, length_f })}
            />
          </div>
        </>
      ) : (
        <div className="grid grid-cols-2 gap-2">
          <FrameField
            label="入点（帧）"
            value={cue.start_f}
            onChange={(start_f) => onChange({ ...cue, start_f })}
          />
          <FrameField
            label="出点（不含，帧）"
            value={cue.end_f}
            min={1}
            onChange={(end_f) => onChange({ ...cue, end_f })}
          />
        </div>
      )}
      {diagnostics.length > 0 && (
        <ul className="text-status-warning grid gap-1 text-xs" aria-label="条目诊断">
          {diagnostics.map((issue, index) => (
            <li key={`${issue.field}:${issue.reason}:${index}`}>{programDiagnosticText(issue)}</li>
          ))}
        </ul>
      )}
      {cue.anchor && diagnostics.some((issue) => issue.reason === "anchor_source_replaced") && (
        <label className="inline-flex items-center gap-2 text-xs">
          <input
            type="checkbox"
            checked={rebind}
            disabled={rebindDisabled}
            onChange={(event) => onRebind(event.target.checked)}
          />
          保存时重新绑定当前视频源
        </label>
      )}
    </section>
  );
}
