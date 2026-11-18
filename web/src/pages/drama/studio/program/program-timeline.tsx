import { useRef } from "react";

import type { ProgramAudioCueInput, ProgramAudioRole } from "@/api/drama/type";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import {
  adjustCue,
  audioTrack,
  PROGRAM_ROWS,
  resolveCue,
  type ProgramTimelineData,
} from "@/utils/drama/program";

type Drag = {
  cue: ProgramAudioCueInput;
  action: "move" | "start" | "end";
  x: number;
  width: number;
  pointerId: number;
};

export function ProgramTimeline({
  timeline,
  audio,
  selectedId,
  invalidIds,
  onSelect,
  onChange,
  onAdd,
}: {
  timeline: ProgramTimelineData;
  audio: readonly ProgramAudioCueInput[];
  selectedId: string | null;
  invalidIds: ReadonlySet<string>;
  onSelect: (id: string) => void;
  onChange: (cue: ProgramAudioCueInput) => void;
  onAdd: (role: ProgramAudioRole) => void;
}) {
  const drag = useRef<Drag | null>(null);
  const total = Math.max(1, timeline.totalFrames);
  const position = (inF: number, outF: number) => ({
    left: `${(Math.max(0, Math.min(total, inF)) / total) * 100}%`,
    width: `${(Math.max(0, Math.min(total, outF) - Math.max(0, inF)) / total) * 100}%`,
  });

  return (
    <div className="min-w-0 overflow-x-auto" aria-label="节目音频时间轴">
      <div className="grid min-w-100 grid-cols-[8rem_minmax(0,1fr)] gap-x-2 gap-y-1.5">
        <div className="text-muted-foreground self-end text-xs">母版 · 24 fps</div>
        <div aria-label="画面切点" className="relative h-6 overflow-hidden rounded-md">
          {timeline.clips.map((clip) => (
            <span
              key={clip.segmentId}
              title={`镜头 ${clip.seq + 1} · ${clip.masterInF}–${clip.masterOutF}f`}
              className="bg-muted text-muted-foreground absolute inset-y-0 truncate border-r px-1 text-xs leading-6"
              style={position(clip.masterInF, clip.masterOutF)}
            >
              {String(clip.seq + 1).padStart(2, "0")}
            </span>
          ))}
        </div>
        {PROGRAM_ROWS.map((row) => (
          <div key={row.id} className="contents">
            <div className="flex min-w-0 items-center justify-between gap-0.5 text-xs">
              <span>{row.label}</span>
              <Button
                type="button"
                size="icon-xs"
                variant="ghost"
                aria-label={`添加${row.label}`}
                disabled={!timeline.totalFrames || audio.length >= 256}
                onClick={() => onAdd(row.role)}
              >
                +
              </Button>
            </div>
            <div
              role="group"
              aria-label={row.label}
              className={cn(
                "bg-muted/65 relative min-h-9 rounded-lg",
                row.id === "narration" && "min-h-13",
              )}
            >
              {row.id === "narration" &&
                timeline.clips
                  .filter((clip) => clip.nativeDialogue)
                  .map((clip) => (
                    <span
                      key={clip.segmentId}
                      aria-label={`镜头 ${clip.seq + 1} 原声对白（只读）`}
                      role="img"
                      title={`镜头 ${clip.seq + 1} 原声对白（只读）`}
                      className="bg-muted-foreground/30 text-foreground absolute top-1 h-4 truncate rounded px-1 text-[10px] leading-4"
                      style={position(clip.masterInF, clip.masterOutF)}
                    >
                      原声对白
                    </span>
                  ))}
              {audio
                .filter((cue) => audioTrack(cue.role) === row.id)
                .map((cue, index) => {
                  const resolved = resolveCue(cue, timeline);
                  const isSelected = cue.id === selectedId;
                  const actionLabel =
                    row.id === "bed" ? (cue.role === "bgm" ? "配乐" : "环境底噪") : row.label;
                  const label = `${actionLabel} ${index + 1}${cue.anchor ? " · 锚点" : ""}`;
                  return (
                    <button
                      key={cue.id}
                      type="button"
                      data-cue-id={cue.id}
                      aria-label={
                        resolved
                          ? `${label}，${resolved.inF}–${resolved.outF} 帧`
                          : `${label}，锚点失效`
                      }
                      title={
                        resolved
                          ? `${label} · ${resolved.inF}–${resolved.outF}f`
                          : `${label} · 锚点失效`
                      }
                      aria-pressed={isSelected}
                      className={cn(
                        "bg-primary/20 text-foreground absolute bottom-1 isolate h-7 min-w-3 touch-none overflow-hidden rounded-md border px-1 text-left text-xs outline-none select-none focus-visible:ring-2",
                        isSelected && "border-primary bg-primary/30 z-10 ring-1 ring-primary",
                        invalidIds.has(cue.id) && "border-destructive",
                        !resolved && "left-0 bg-destructive/15",
                      )}
                      style={resolved ? position(resolved.inF, resolved.outF) : undefined}
                      onClick={() => onSelect(cue.id)}
                      onPointerDown={(event) => {
                        if (event.button !== 0) return;
                        onSelect(cue.id);
                        const width =
                          event.currentTarget.parentElement?.getBoundingClientRect().width ?? 0;
                        if (!resolved || !width) return;
                        const rect = event.currentTarget.getBoundingClientRect();
                        const edge = Math.min(7, rect.width / 3);
                        const action =
                          event.clientX - rect.left < edge
                            ? "start"
                            : rect.right - event.clientX < edge
                              ? "end"
                              : "move";
                        drag.current = {
                          cue,
                          action,
                          x: event.clientX,
                          width,
                          pointerId: event.pointerId,
                        };
                        event.currentTarget.setPointerCapture?.(event.pointerId);
                      }}
                      onPointerMove={(event) => {
                        const active = drag.current;
                        if (!active || active.pointerId !== event.pointerId) return;
                        const delta = Math.round(
                          ((event.clientX - active.x) * total) / active.width,
                        );
                        onChange(adjustCue(active.cue, active.action, delta, timeline));
                      }}
                      onPointerUp={() => {
                        drag.current = null;
                      }}
                      onPointerCancel={() => {
                        drag.current = null;
                      }}
                      onLostPointerCapture={() => {
                        drag.current = null;
                      }}
                      onKeyDown={(event) => {
                        if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") return;
                        event.preventDefault();
                        onSelect(cue.id);
                        onChange(
                          adjustCue(
                            cue,
                            "move",
                            (event.key === "ArrowLeft" ? -1 : 1) * (event.shiftKey ? 10 : 1),
                            timeline,
                          ),
                        );
                      }}
                    >
                      {resolved && (
                        <span
                          aria-hidden
                          className="absolute inset-y-1 left-0.5 w-px bg-current opacity-40"
                        />
                      )}
                      <span className="block truncate px-1">{label}</span>
                      {resolved && (
                        <span
                          aria-hidden
                          className="absolute inset-y-1 right-0.5 w-px bg-current opacity-40"
                        />
                      )}
                    </button>
                  );
                })}
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}
