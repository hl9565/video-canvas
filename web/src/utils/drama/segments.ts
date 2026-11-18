import type { Segment, SegmentTake } from "@/api/drama/type";
import { isTerminalStatus } from "@/utils/tasks/status";

/** 按 seq 排序（不改原数组） */
export const sortSegments = (list: readonly Segment[]): Segment[] =>
  [...list].sort((a, b) => a.seq - b.seq);

const parseTimeMs = (value: string | undefined): number | null => {
  if (!value) return null;
  const ms = Date.parse(value);
  return Number.isFinite(ms) ? ms : null;
};

/** 避免不递增 fragment revision 的列表拉取或迟到推送覆盖更新的首帧流程快照。 */
export function preserveNewerFrameRun(
  existing: Segment | undefined,
  incoming: Segment,
  preserveLocalRun = false,
): Segment {
  const previous = existing?.frameRun;
  if (!previous) return incoming;
  const next = incoming.frameRun;
  if (previous.id === next?.id) {
    if (previous.revision !== next.revision) {
      return previous.revision > next.revision ? { ...incoming, frameRun: previous } : incoming;
    }
  } else {
    const prevTime = parseTimeMs(previous.updatedAt || previous.createdAt);
    const nextTime = parseTimeMs(next ? next.updatedAt || next.createdAt : incoming.updatedAt);
    if (prevTime !== null && nextTime !== null && prevTime !== nextTime) {
      return prevTime > nextTime ? { ...incoming, frameRun: previous } : incoming;
    }
  }
  return preserveLocalRun ? { ...incoming, frameRun: previous } : incoming;
}

/** 候选任务独立于片段修订推进，终态不会再回到进行态。 */
const takeProgress: Record<SegmentTake["status"], number> = {
  pending: 0,
  queued: 1,
  running: 2,
  finalizing: 3,
  succeeded: 4,
  failed: 4,
  canceled: 4,
  expired: 4,
};

/** 同一视频任务已有终态候选时，旧首帧快照不能让它恢复进行。 */
export const hasTerminalTake = (takes: readonly SegmentTake[], taskId: string | undefined) =>
  Boolean(taskId && takes.some((take) => take.taskId === taskId && isTerminalStatus(take.status)));

function mergeTake(
  existing: SegmentTake,
  incoming: SegmentTake,
  newerRevision: boolean,
): SegmentTake {
  const [fresher, older] =
    takeProgress[existing.status] > takeProgress[incoming.status]
      ? [existing, incoming]
      : [incoming, existing];
  const qc = newerRevision
    ? (incoming.qc ?? existing.qc)
    : fresher.qc && fresher.qc !== "unreviewed"
      ? fresher.qc
      : (older.qc ?? fresher.qc);
  const asset = fresher.asset ?? older.asset;
  const errorMessage = fresher.errorMessage || older.errorMessage;
  if (fresher.qc === qc && fresher.asset === asset && fresher.errorMessage === errorMessage)
    return fresher;
  return { ...fresher, asset, errorMessage, qc };
}

/** 与服务端一致：按创建顺序取最后一个能决定状态的候选；取消和过期不覆盖之前的状态。 */
function statusFromTakes(takes: readonly SegmentTake[]): Segment["status"] {
  for (let i = takes.length - 1; i >= 0; i--) {
    const take = takes[i];
    if (takeProgress[take.status] < takeProgress.succeeded) return "running";
    if (take.status === "failed") return "error";
    if (take.status === "succeeded" && take.asset) return "done";
  }
  return "idle";
}

function reconcileTakes(existing: Segment, incoming: Segment): Segment {
  if (existing.takes.length === 0) return incoming;
  const newerRevision = incoming.revision > existing.revision;
  const previousById = new Map(existing.takes.map((take) => [take.taskId, take]));
  const mergedById = new Map(
    incoming.takes.map((take) => {
      const previous = previousById.get(take.taskId);
      return [take.taskId, previous ? mergeTake(previous, take, newerRevision) : take];
    }),
  );
  let takes = [...mergedById.values()];
  /** 相同修订的旧快照可能尚未看到新候选；更高修订则以服务端的候选集合为准。 */
  if (!newerRevision && existing.takes.some((take) => !mergedById.has(take.taskId))) {
    takes = [
      ...existing.takes.map((take) => mergedById.get(take.taskId) ?? take),
      ...takes.filter((take) => !previousById.has(take.taskId)),
    ].sort((a, b) => Date.parse(a.createdAt) - Date.parse(b.createdAt));
  }
  /** 进行中的首帧视频任务可能只出现在服务端状态里，不出现在候选列表。 */
  const status =
    incoming.status === statusFromTakes(incoming.takes) ? statusFromTakes(takes) : incoming.status;
  return { ...incoming, takes, status };
}

/** WS 与整集列表共用同行合并规则；首帧流程修订独立于片段行修订。 */
export function reconcileSegmentRow(
  existing: Segment | undefined,
  incoming: Segment,
  preserveLocalRun = false,
  frameVideoActive?: boolean,
): Segment {
  if (!existing) return incoming;
  const staleRow = incoming.revision < existing.revision;
  if (staleRow && (!existing.frameRun || !incoming.frameRun)) return existing;
  const { frameRun } = preserveNewerFrameRun(existing, incoming, preserveLocalRun || staleRow);
  const row = staleRow ? existing : incoming;
  let merged = staleRow ? existing : reconcileTakes(existing, incoming);
  let frameRunning = false;
  let frameResolved = false;
  if (frameRun?.state === "video_pending") {
    const fromIncoming = frameRun === incoming.frameRun;
    const previousRun = existing.frameRun;
    const sameRunRevision =
      frameRun.id === previousRun?.id && frameRun.revision === previousRun.revision;
    const taskActive = fromIncoming ? frameVideoActive : undefined;
    frameResolved =
      taskActive === false ||
      hasTerminalTake(merged.takes, frameRun.videoTaskId) ||
      hasTerminalTake(existing.takes, frameRun.videoTaskId);
    // 未完成的控镜视频可能还不是候选；流程修订比片段行独立推进。
    frameRunning =
      !frameResolved &&
      (taskActive === true ||
        (fromIncoming &&
          (incoming.status === "running" || (!sameRunRevision && !!frameRun.videoTaskId))) ||
        (sameRunRevision &&
          existing.status === "running" &&
          (!!frameRun.videoTaskId || statusFromTakes(existing.takes) !== "running")));
  }
  if (frameRun !== row.frameRun || frameRunning || (frameResolved && merged.status === "running")) {
    merged = {
      ...merged,
      frameRun,
      status: frameRunning ? "running" : statusFromTakes(merged.takes),
    };
  }
  if (
    incoming.revision === existing.revision &&
    existing.stillAssetIdText &&
    !merged.stillAssetIdText &&
    merged.visualKind === existing.visualKind &&
    merged.stillAssetId === existing.stillAssetId
  ) {
    merged = { ...merged, stillAssetIdText: existing.stillAssetIdText };
  }
  return merged;
}

/**
 * 并入一条服务端快照（WebSocket 推送 / 接口返回）：
 * 同 id 按各自修订更新片段行和首帧流程；相同片段修订也合并 take 状态；
 * 没见过的 id 插入。结果按 seq 排序。
 */
export function mergeSegment(
  list: readonly Segment[],
  incoming: Segment,
  frameVideoActive?: boolean,
): Segment[] {
  const existing = list.find((item) => item.id === incoming.id);
  const merged = reconcileSegmentRow(existing, incoming, false, frameVideoActive);
  return sortSegments([...list.filter((item) => item.id !== incoming.id), merged]);
}

/** 从列表里拿掉一个片段；其余片段的 seq 不动（以服务端为准） */
export const removeSegmentFrom = (list: readonly Segment[], id: string): Segment[] =>
  list.filter((item) => item.id !== id);

/**
 * 按 ids 的顺序排好并重写 seq（从 0 起连续）；ids 里没有的片段排在最后，ids 里多出来的忽略。
 * 拖动片段条时先在本地换序，再把 ids 交给 PUT /segments/order。
 */
export function applyOrder(list: readonly Segment[], ids: readonly string[]): Segment[] {
  const byId = new Map(list.map((item) => [item.id, item]));
  const picked = ids.flatMap((id) => {
    const item = byId.get(id);
    byId.delete(id);
    return item ? [item] : [];
  });
  const rest = list.filter((item) => byId.has(item.id));
  return [...picked, ...rest].map((item, index) => ({ ...item, seq: index }));
}

export const orderIds = (list: readonly Segment[]): string[] => list.map((item) => item.id);

export const sameOrder = (a: readonly string[], b: readonly string[]): boolean =>
  a.length === b.length && a.every((id, index) => id === b[index]);

/** 片段条上一个片段的最小宽度（像素），再短也要放得下序号和时长 */
const CLIP_MIN = 64;
/** 片段条每秒多宽（像素） */
const CLIP_PER_SECOND = 12;

/** 片段条上的宽度：按时长，有下限 */
export const clipWidth = (seconds: number): number =>
  Math.max(CLIP_MIN, Math.round(seconds * CLIP_PER_SECOND));

/** 各片段时长之和（秒） */
export const totalSeconds = (list: readonly Pick<Segment, "seconds">[]): number =>
  list.reduce((sum, item) => sum + item.seconds, 0);

/** 采用的那条 take；没有采用或找不到为 null */
export const chosenTake = (segment: Segment): SegmentTake | null =>
  (segment.chosenTaskId && segment.takes.find((take) => take.taskId === segment.chosenTaskId)) ||
  null;

/** 能播放的采用视频：采用的那条成功并且有素材地址 */
export function playableTake(segment: Segment): SegmentTake | null {
  const take = chosenTake(segment);
  return take && take.status === "succeeded" && take.asset ? take : null;
}

/** 片段有没有采用的视频 */
export const hasVideo = (segment: Segment): boolean => playableTake(segment) !== null;

/** 片段正在生成视频 */
export const isRunning = (segment: Segment): boolean => segment.status === "running";

/**
 * 点选一条 take 想采用它：ok 可以请求；already 已经是采用的，不重复请求；
 * invalid 只能采用本片段已成功（且有素材）的结果（后端对应 67011）。
 */
export function chooseCheck(segment: Segment, taskId: string): "ok" | "already" | "invalid" {
  const take = segment.takes.find((item) => item.taskId === taskId);
  if (!take || take.status !== "succeeded" || !take.asset || take.qc === "rejected")
    return "invalid";
  return segment.chosenTaskId === taskId ? "already" : "ok";
}

/** 批量生成的对象：没有采用视频、不在生成中、提示词不为空 */
export const batchTargets = (list: readonly Segment[]): Segment[] =>
  list.filter(
    (item) =>
      (!item.visualKind || item.visualKind === "video") &&
      !hasVideo(item) &&
      !isRunning(item) &&
      item.prompt.trim() !== "",
  );

/** 失败时的说明：最近一条失败 take 的原因；不是 error 状态为 null */
export function latestError(segment: Segment): string | null {
  if (segment.status !== "error") return null;
  const failed = [...segment.takes].reverse().find((take) => take.status === "failed");
  return failed?.errorMessage || "生成失败";
}
