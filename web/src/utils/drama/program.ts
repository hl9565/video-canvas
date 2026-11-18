import type {
  DramaAssetIDInput,
  DramaPreviewRange,
  ProgramAudioCueInput,
  ProgramAudioRole,
  ProgramDiagnostic,
  Segment,
  SegmentFx,
} from "@/api/drama/type";
import { ApiError } from "@/utils/requests/request";

import { hasVideo, playableTake, sortSegments } from "./segments";
import { FPS, segmentEditOf } from "./segment-trim";

export const PROGRAM_MAX_FRAMES = 14_400;
export const VISUAL_RATIO_MESSAGE = "静帧、黑场与画面特效仅支持 16:9 剧集";

export const hasPlayableTake = hasVideo;
export const isVideoVisual = (segment: Pick<Segment, "visualKind">) =>
  !segment.visualKind || segment.visualKind === "video";

/** 静帧和黑场以自己的素材与时长为准，即使仍保留可播放的历史视频。 */
export const hasRenderableVisual = (segment: Segment) =>
  segment.visualKind === "black" ||
  (segment.visualKind === "still" && typeof segment.stillAssetId === "number") ||
  hasPlayableTake(segment);

export const hasSegmentFx = (fx?: SegmentFx | null) =>
  !!fx &&
  Object.entries(fx).some(
    ([name, value]) => typeof value === "number" && (!name.endsWith("_f") || value !== 0),
  );

export const usesProgramVisual = (segment: Pick<Segment, "visualKind" | "fx">) =>
  !isVideoVisual(segment) || hasSegmentFx(segment.fx);

export const usesProgramFeatures = (
  segments: readonly Pick<Segment, "visualKind" | "fx">[],
  programAudioCount = 0,
) => programAudioCount > 0 || segments.some(usesProgramVisual);

export const visualBadges = (segment: Pick<Segment, "visualKind" | "fx">) => [
  ...(segment.visualKind === "still" ? ["静帧"] : segment.visualKind === "black" ? ["黑场"] : []),
  ...(hasSegmentFx(segment.fx) ? ["FX"] : []),
];

/** 数字只接受安全整数；大素材 ID 必须以原始的十进制串输入。 */
export function parseProgramAssetId(value: string | number): DramaAssetIDInput | null {
  if (typeof value === "number") return Number.isSafeInteger(value) && value > 0 ? value : null;
  const raw = value.trim();
  if (!/^\d{1,19}$/.test(raw) || BigInt(raw) <= 0n || BigInt(raw) > 9223372036854775807n)
    return null;
  const numeric = Number(raw);
  return Number.isSafeInteger(numeric) ? numeric : raw;
}

/** 分镜的预计母版坐标；服务端清单就绪后，预览范围以服务端帧数为准。 */
export function programTimeline(segments: readonly Segment[]) {
  let totalFrames = 0;
  const clips = sortSegments(segments)
    .filter(hasRenderableVisual)
    .map((segment) => {
      const edit = segmentEditOf(segment);
      const video = isVideoVisual(segment);
      const durationMs = playableTake(segment)?.asset?.durationMs ?? 0;
      const fallback = Math.max(1, Math.round((segment.seconds || 0) * FPS));
      const sourceInF = video ? edit.sourceInF : 0;
      const sourceOutF = Math.max(
        sourceInF + 1,
        edit.sourceOutF ??
          (video
            ? segment.sourceFrameCount || Math.round((durationMs * FPS) / 1000) || fallback
            : fallback),
      );
      const masterInF = totalFrames;
      totalFrames += sourceOutF - sourceInF;
      return {
        segmentId: segment.id,
        seq: segment.seq,
        video,
        sourceInF,
        sourceOutF,
        masterInF,
        masterOutF: totalFrames,
        nativeDialogue:
          video && segment.hasAudio !== false && edit.nativeIsDialogue && !edit.muteOriginal,
      };
    });
  return { clips, totalFrames };
}

export type ProgramTimelineData = ReturnType<typeof programTimeline>;

export function resolveCue(
  cue: ProgramAudioCueInput,
  timeline: ProgramTimelineData,
): DramaPreviewRange | null {
  if (!cue.anchor) return { inF: cue.start_f, outF: cue.end_f };
  const clip = timeline.clips.find(
    (item) => item.video && item.segmentId === cue.anchor?.segment_id,
  );
  if (!clip || cue.anchor.source_f < clip.sourceInF || cue.anchor.source_f >= clip.sourceOutF)
    return null;
  const inF = clip.masterInF + cue.anchor.source_f - clip.sourceInF;
  return { inF, outF: inF + cue.length_f };
}

const clampFrame = (value: number, min: number, max: number) =>
  Math.min(max, Math.max(min, Number.isFinite(value) ? Math.round(value) : min));

export function clampPreviewRange(
  range: DramaPreviewRange,
  totalFrames: number,
): DramaPreviewRange | undefined {
  if (!Number.isFinite(totalFrames) || totalFrames < 1) return undefined;
  const total = Math.floor(totalFrames);
  const inF = clampFrame(range.inF, 0, total - 1);
  return { inF, outF: clampFrame(range.outF, inF + 1, total) };
}

export const isBedRole = (role: ProgramAudioRole) => role === "bgm" || role === "room_tone";
export const audioTrack = (role: ProgramAudioRole) => (isBedRole(role) ? "bed" : role);

export const PROGRAM_ROWS = [
  { id: "narration", label: "旁白", role: "narration" },
  { id: "bed", label: "配乐 / 环境底噪", role: "bgm" },
  { id: "sfx", label: "音效", role: "sfx" },
] as const;

/** 按可编辑字段和服务端默认值比较条目，忽略 JSON 字段的排列顺序。 */
function canonicalCue(cue: ProgramAudioCueInput | undefined) {
  if (!cue) return undefined;
  return JSON.stringify({
    id: cue.id,
    role: cue.role,
    asset_id: String(parseProgramAssetId(cue.asset_id) ?? cue.asset_id),
    placement: cue.anchor
      ? [cue.anchor.segment_id, cue.anchor.source_f, cue.length_f]
      : [cue.start_f, cue.end_f],
    src_in_sample: cue.src_in_sample ?? 0,
    src_out_sample: cue.src_out_sample ?? null,
    gain_db: cue.gain_db ?? 0,
    duck_db: cue.duck_db ?? null,
    av_pts_offset_ms: cue.av_pts_offset_ms ?? 0,
    fade_in_f: cue.fade_in_f ?? 0,
    fade_out_f: cue.fade_out_f ?? 0,
    loop: cue.role === "bgm" || cue.role === "room_tone" ? (cue.loop ?? false) : false,
  });
}

/** 合并同一基线上的条目改动；争议条目暂用本地选择，留给用户确认。 */
export function mergeProgramAudio(
  baseAudio: readonly ProgramAudioCueInput[],
  localAudio: readonly ProgramAudioCueInput[],
  remoteAudio: readonly ProgramAudioCueInput[],
) {
  const byId = (audio: readonly ProgramAudioCueInput[]) =>
    new Map(audio.map((cue) => [cue.id, cue]));
  const base = byId(baseAudio);
  const local = byId(localAudio);
  const remote = byId(remoteAudio);
  const audio: ProgramAudioCueInput[] = [];
  let hasConflict = false;
  for (const id of new Set([...remote.keys(), ...local.keys()])) {
    const original = canonicalCue(base.get(id));
    const localCue = local.get(id);
    const remoteCue = remote.get(id);
    const localKey = canonicalCue(localCue);
    const remoteKey = canonicalCue(remoteCue);
    if (localKey !== original && remoteKey !== original && localKey !== remoteKey) {
      hasConflict = true;
    }
    const cue = localKey === original ? remoteCue : localCue;
    if (cue) audio.push(cue);
  }
  return { audio, hasConflict };
}

export function validateProgram(
  audio: readonly ProgramAudioCueInput[],
  timeline: ProgramTimelineData,
): ProgramDiagnostic[] {
  const issues: ProgramDiagnostic[] = [];
  const previous: { role: ProgramAudioRole; range: DramaPreviewRange }[] = [];
  audio.forEach((cue) => {
    const report = (field: string, reason: string) =>
      issues.push({ cue_id: cue.id, field, reason, severity: "error" });
    if (parseProgramAssetId(cue.asset_id) === null) report("asset_id", "请输入有效的十进制素材 ID");
    const range = resolveCue(cue, timeline);
    if (!range) {
      report("anchor", "锚点必须落在当前采用视频的源帧区间内");
      return;
    }
    if (
      !Number.isInteger(range.inF) ||
      !Number.isInteger(range.outF) ||
      range.inF < 0 ||
      range.outF <= range.inF ||
      range.outF > Math.min(timeline.totalFrames, PROGRAM_MAX_FRAMES)
    ) {
      report("range", "音频区间必须使用整数帧且位于母版内");
      return;
    }
    if (
      cue.role !== "sfx" &&
      previous.some(
        (item) =>
          item.role === cue.role && range.inF < item.range.outF && item.range.inF < range.outF,
      )
    ) {
      report("range", "同类音频的区间不能重叠");
    }
    previous.push({ role: cue.role, range });
  });
  return issues;
}

/** 移动保留原长度；锚点只能在原视频的源帧范围内移动。 */
export function adjustCue(
  cue: ProgramAudioCueInput,
  action: "move" | "start" | "end",
  delta: number,
  timeline: ProgramTimelineData,
): ProgramAudioCueInput {
  const range = resolveCue(cue, timeline);
  if (!range) return cue;
  const total = Math.min(timeline.totalFrames, PROGRAM_MAX_FRAMES);
  const step = Math.round(delta);
  if (cue.anchor) {
    const clip = timeline.clips.find((item) => item.segmentId === cue.anchor.segment_id)!;
    if (action === "end")
      return {
        ...cue,
        length_f: clampFrame(cue.length_f + step, 1, Math.max(1, total - range.inF)),
      };
    const sourceF = clampFrame(
      cue.anchor.source_f + step,
      clip.sourceInF,
      Math.max(
        clip.sourceInF,
        Math.min(clip.sourceOutF - 1, cue.anchor.source_f + total - range.outF),
      ),
    );
    const nextSource =
      action === "start" ? Math.min(sourceF, cue.anchor.source_f + cue.length_f - 1) : sourceF;
    return {
      ...cue,
      anchor: { ...cue.anchor, source_f: nextSource },
      length_f:
        action === "start" ? cue.length_f - (nextSource - cue.anchor.source_f) : cue.length_f,
    };
  }
  if (action === "start")
    return { ...cue, start_f: clampFrame(range.inF + step, 0, range.outF - 1) };
  if (action === "end")
    return {
      ...cue,
      end_f: clampFrame(range.outF + step, range.inF + 1, Math.max(total, range.inF + 1)),
    };
  const length = range.outF - range.inF;
  const start = clampFrame(range.inF + step, 0, Math.max(0, total - length));
  return { ...cue, start_f: start, end_f: start + length };
}

export function programDiagnosticsFrom(error: unknown): ProgramDiagnostic[] {
  if (!(error instanceof ApiError)) return [];
  const data = error.body?.data as
    | { diagnostics?: ProgramDiagnostic[]; program_diagnostics?: ProgramDiagnostic[] }
    | undefined;
  if (Array.isArray(data?.diagnostics)) return data.diagnostics;
  return Array.isArray(data)
    ? data
    : Array.isArray(data?.program_diagnostics)
      ? data.program_diagnostics
      : [];
}

const diagnosticLabels: Record<string, string> = {
  anchor_source_replaced: "锚点的视频来源已更换",
  media_unavailable: "引用的素材不可用",
  asset_changed: "引用的素材已变更",
  anchor_frame_trimmed: "锚点已不在视频裁剪范围内",
  anchor_segment_missing: "锚点对应的分镜已移除",
  anchor_non_video: "锚点对应的分镜已不是视频",
  anchor_segment_unready: "锚点对应的视频尚不可用",
  unsupported_ratio: VISUAL_RATIO_MESSAGE,
  timeline_limit: "节目单超出母版时长或分镜数量限制",
  visual_invalid: "分镜画面设置无效",
  cue_out_of_episode: "音频区间超出母版时长",
  invalid_program: "节目单设置无效",
};

export const programDiagnosticText = (issue: ProgramDiagnostic) =>
  diagnosticLabels[issue.reason] ?? `${issue.field}：${issue.reason}`;
