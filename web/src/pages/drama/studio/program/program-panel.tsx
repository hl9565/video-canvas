import { useEffect, useMemo, useRef, useState } from "react";

import type { ProgramAudioCueInput, ProgramAudioRole, Segment } from "@/api/drama/type";
import { Button } from "@/components/ui/button";
import { programTtsCueId } from "@/store/drama-program-tts";
import {
  isBedRole,
  programDiagnosticText,
  PROGRAM_MAX_FRAMES,
  validateProgram,
} from "@/utils/drama/program";

import { CueInspector } from "./cue-inspector";
import { ProgramTimeline } from "./program-timeline";
import { TtsDialog } from "./tts-dialog";
import { effectiveProgramTimeline, type ProgramStateApi } from "./use-program";
import { useProgramTts } from "./use-program-tts";

const EMPTY_AUDIO: ProgramAudioCueInput[] = [];

function addCue(
  audio: ProgramAudioCueInput[],
  id: string,
  role: ProgramAudioRole,
  assetId: number | string,
  total: number,
  duration = 24,
): ProgramAudioCueInput[] {
  if (audio.some((cue) => cue.id === id)) return audio;
  const end = Math.max(1, Math.min(total, PROGRAM_MAX_FRAMES));
  const length = Math.min(end, Math.max(1, duration));
  let start = 0;
  if (role !== "sfx") {
    const existing = audio
      .filter((cue) => cue.role === role && !cue.anchor)
      .sort((a, b) => a.start_f! - b.start_f!);
    for (const cue of existing) {
      if (cue.start_f! >= start + length) break;
      start = Math.max(start, cue.end_f!);
    }
  }
  if (start + length > end) start = Math.max(0, end - length);
  const placement = { id, asset_id: assetId, start_f: start, end_f: start + length };
  return [...audio, isBedRole(role) ? { ...placement, role, duck_db: -8 } : { ...placement, role }];
}

export function ProgramPanel({
  open,
  canvasId,
  episodeId,
  segments,
  program,
}: {
  open: boolean;
  canvasId: string;
  episodeId: string;
  segments: readonly Segment[];
  program: ProgramStateApi;
}) {
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [ttsOpen, setTtsOpen] = useState(false);
  const { rebasedTimeline, rebasedSegmentVersion, serverVersion } = program;
  const activeTimeline = useMemo(
    () =>
      effectiveProgramTimeline({ rebasedTimeline, rebasedSegmentVersion, serverVersion }, segments),
    [rebasedTimeline, rebasedSegmentVersion, serverVersion, segments],
  );
  const audio = program.draft?.audio ?? EMPTY_AUDIO;
  const validation = useMemo(() => validateProgram(audio, activeTimeline), [audio, activeTimeline]);
  const diagnostics = [...program.diagnostics, ...validation];
  const invalidIds = new Set(
    diagnostics.map((issue) => issue.cue_id).filter((id) => id !== undefined),
  );
  const selected = audio.find((cue) => cue.id === selectedId);
  const changeCue = (cue: ProgramAudioCueInput) =>
    program.editAudio((cues) => cues.map((current) => (current.id === cue.id ? cue : current)));
  const add = (role: ProgramAudioRole) => {
    const id = crypto.randomUUID();
    program.editAudio((cues) => addCue(cues, id, role, 0, activeTimeline.totalFrames));
    setSelectedId(id);
  };
  const generation = useProgramTts({
    canvasId,
    episodeId,
    canAttach: !!program.draft && !program.loading && program.baseEpisodeId === episodeId,
    onAttach: (taskId, asset) => {
      const id = programTtsCueId(taskId);
      program.editAudio((cues) =>
        addCue(
          cues,
          id,
          "narration",
          asset.assetId,
          activeTimeline.totalFrames,
          asset.durationFrames ?? 24,
        ),
      );
      setSelectedId(id);
    },
  });
  const lastIssue = useRef<string | null>(null);
  const offendingId =
    program.diagnostics.find(
      (issue) => issue.cue_id && audio.some((cue) => cue.id === issue.cue_id),
    )?.cue_id ?? null;
  useEffect(() => {
    if (offendingId && offendingId !== lastIssue.current) setSelectedId(offendingId);
    lastIssue.current = offendingId;
  }, [offendingId]);

  const canSave =
    !program.loading &&
    !program.saving &&
    !validation.length &&
    (program.dirty || program.rebindIds.length > 0);
  return (
    <>
      <section
        hidden={!open}
        aria-label="节目音频"
        className="border-border max-h-[48svh] shrink-0 overflow-y-auto border-t px-4 py-3"
      >
        <div className="mb-2.5 flex flex-wrap items-center justify-between gap-2">
          <div className="flex items-center gap-2">
            <h3 className="text-sm font-semibold">节目音频</h3>
            <span className="text-muted-foreground text-xs tabular-nums">
              {activeTimeline.totalFrames}f
            </span>
            {program.dirty && <span className="text-status-warning text-xs">未保存</span>}
          </div>
          <div className="flex items-center gap-1.5">
            <Button
              type="button"
              size="sm"
              variant="outline"
              disabled={!program.draft || !activeTimeline.totalFrames || audio.length >= 256}
              onClick={() => add("room_tone")}
            >
              添加环境底噪
            </Button>
            <Button
              type="button"
              size="sm"
              variant="outline"
              disabled={!program.draft || !activeTimeline.totalFrames || audio.length >= 256}
              onClick={() => setTtsOpen(true)}
            >
              {generation.busy ? "配音生成中…" : "AI 生成配音 (TTS)"}
            </Button>
            <Button
              type="button"
              size="sm"
              disabled={!canSave || program.conflict}
              onClick={() => void program.save(segments)}
            >
              {program.saving ? "保存中…" : "保存节目音频"}
            </Button>
          </div>
        </div>
        {program.conflict && (
          <div
            role="alert"
            className="bg-status-warning/10 mb-2 flex flex-wrap items-center gap-2 rounded-lg p-2 text-xs"
          >
            <span className="text-status-warning">
              {program.conflictMessage ?? "节目单版本已变更，草稿已保留。"}
            </span>
            <Button
              type="button"
              size="sm"
              variant="outline"
              disabled={program.saving || program.loading}
              onClick={() => void program.reload(true)}
            >
              重新加载最新（丢弃草稿）
            </Button>
            {program.conflictMessage ? (
              <Button
                type="button"
                size="sm"
                variant="destructive"
                disabled={!canSave}
                onClick={() => void program.forceOverwrite(segments)}
              >
                确认覆盖服务端版本
              </Button>
            ) : (
              <Button
                type="button"
                size="sm"
                variant="outline"
                disabled={program.saving || program.loading}
                onClick={() => void program.save(segments, true)}
              >
                基于最新版本重试
              </Button>
            )}
          </div>
        )}
        {program.error && (
          <p role="alert" className="text-destructive mb-2 text-xs">
            {program.error}
          </p>
        )}
        {program.diagnostics.length > 0 && (
          <ul
            aria-label="节目单诊断"
            className="text-status-warning mb-2 flex flex-wrap gap-2 text-xs"
          >
            {program.diagnostics.map((issue, index) => (
              <li key={`${issue.cue_id ?? issue.segment_id}:${issue.reason}:${index}`}>
                {issue.cue_id && audio.some((cue) => cue.id === issue.cue_id) ? (
                  <button
                    type="button"
                    className="underline underline-offset-2"
                    onClick={() => setSelectedId(issue.cue_id!)}
                  >
                    {programDiagnosticText(issue)}
                  </button>
                ) : (
                  programDiagnosticText(issue)
                )}
              </li>
            ))}
          </ul>
        )}
        {program.draft ? (
          <div className="grid gap-3 lg:grid-cols-[minmax(0,1fr)_minmax(240px,300px)]">
            <ProgramTimeline
              timeline={activeTimeline}
              audio={audio}
              selectedId={selectedId}
              invalidIds={invalidIds}
              onSelect={setSelectedId}
              onAdd={add}
              onChange={changeCue}
            />
            {selected && (
              <CueInspector
                key={selected.id}
                cue={selected}
                timeline={activeTimeline}
                diagnostics={diagnostics.filter((issue) => issue.cue_id === selected.id)}
                rebind={program.rebindIds.includes(selected.id)}
                rebindDisabled={program.saving}
                onRebind={(checked) => program.toggleRebind(selected.id, checked)}
                onChange={changeCue}
                onRemove={() => {
                  program.editAudio((cues) => cues.filter((cue) => cue.id !== selected.id));
                  setSelectedId(null);
                }}
              />
            )}
          </div>
        ) : program.error ? (
          <Button type="button" size="sm" variant="outline" onClick={() => void program.reload()}>
            重试读取节目音频
          </Button>
        ) : (
          <p role="status" className="text-muted-foreground text-xs">
            读取节目音频…
          </p>
        )}
      </section>
      <TtsDialog open={open && ttsOpen} onOpenChange={setTtsOpen} generation={generation} />
    </>
  );
}
