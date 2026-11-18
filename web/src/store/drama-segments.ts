import { create } from "zustand";

import type { Segment } from "@/api/drama/type";
import {
  applyOrder,
  mergeSegment,
  reconcileSegmentRow,
  removeSegmentFrom,
  sortSegments,
} from "@/utils/drama/segments";

/** 一部短剧的一集：片段列表的键 */
export const segmentKey = (dramaId: string, no: number) => `${dramaId}:${no}`;

/** 解开 segmentKey */
export function parseSegmentKey(key: string): { dramaId: string; no: number } {
  const at = key.lastIndexOf(":");
  return { dramaId: key.slice(0, at), no: Number(key.slice(at + 1)) };
}

export type SegmentListSnapshot = {
  episodeId?: string;
  segmentVersion?: number;
  expectedEpisodeId?: string;
};

type StoredEpisodeMeta = {
  episodeId?: string;
  segmentVersion?: number;
  /** 仅完整列表有权确认哪些片段已被移除。 */
  listVersion?: number;
  /** 已确认删除的片段 ID 不会复用，保护持续到此集缓存失效。 */
  tombstones?: ReadonlySet<string>;
  listRef: Segment[];
};

const maxSegmentVersion = (list: readonly Segment[]): number | undefined => {
  let maxVer: number | undefined;
  for (const item of list) {
    if (typeof item.segmentVersion === "number") {
      maxVer = maxVer === undefined ? item.segmentVersion : Math.max(maxVer, item.segmentVersion);
    }
  }
  return maxVer;
};

const maxOptNumber = (a: number | undefined, b: number | undefined): number | undefined => {
  if (a === undefined) return b;
  if (b === undefined) return a;
  return Math.max(a, b);
};

const belongsToEpisode = (active?: string, incoming?: string) =>
  !active || !incoming || incoming === active;

const firstSegmentEpisodeId = (list: readonly Segment[]): string | undefined =>
  list.find((item) => Boolean(item.episodeId))?.episodeId;

const resolveEpisodeMeta = (
  list: Segment[] | undefined,
  stored: StoredEpisodeMeta | undefined,
): Omit<StoredEpisodeMeta, "listRef"> | null => {
  if (list === undefined) return null;
  const trusted = stored && stored.listRef === list ? stored : undefined;
  return {
    episodeId: trusted?.episodeId ?? firstSegmentEpisodeId(list),
    segmentVersion: maxOptNumber(trusted?.segmentVersion, maxSegmentVersion(list)),
    listVersion: trusted?.listVersion,
    tombstones: trusted?.tombstones,
  };
};

type DramaSegmentsState = {
  /** 每集的片段，按 seq 排序；没加载过的集没有键 */
  byEpisode: Record<string, Segment[]>;
  /** 每集最后一次写入的权威身份与内容版本，并与数组引用绑定以兼容测试 setState 重置 */
  metaByEpisode: Record<string, StoredEpisodeMeta>;
  /** 正在打开的片段工作区（引用计数）：WebSocket 重连 / 回到前台时只对账这些 */
  watching: Record<string, number>;
  /** 单片段首帧本地更新纪元，防止发起较早的整集拉取写回旧流程快照 */
  runFloors: Record<string, number>;
  /** 读取某集当前已缓存的分集身份与版本号 */
  snapshotOf: (
    dramaId: string,
    no: number,
  ) => { episodeId?: string; segmentVersion?: number } | null;
  /** 切换集号对应的权威身份，旧集的列表和版本不能继续使用。 */
  activateEpisode: (dramaId: string, no: number, episodeId: string) => void;
  /** 删完全部片段后显式清空缓存，保留已知版本下限。 */
  clearEpisode: (dramaId: string, no: number, expectedEpisodeId?: string) => void;
  /** 确认再生成已原子清空旧成员，留下版本下限和删除记录。 */
  applyConfirmedClear: (
    dramaId: string,
    no: number,
    episodeId: string,
    clearedVersion: number,
    segmentIds: readonly string[],
  ) => void;
  /** 标记某片段发生了本地首帧流程状态推进 */
  bumpRunFloor: (segmentId: string) => void;
  /** 整体替换一集的片段（HTTP 读取 / 重排返回），按集身份和版本号防御迟到回退，并保留更新的首帧快照 */
  setList: (
    dramaId: string,
    no: number,
    list: readonly Segment[],
    startFloors?: Readonly<Record<string, number>>,
    snapshot?: SegmentListSnapshot,
  ) => boolean;
  /** 乐观换序只更新显示顺序，成员与服务器版本均保持不变。 */
  reorderLocal: (
    dramaId: string,
    no: number,
    ids: readonly string[],
    expectedEpisodeId?: string,
  ) => boolean;
  /** 并入一条片段快照；首帧提交可附上已知的视频任务进度。 */
  apply: (
    dramaId: string,
    no: number,
    segment: Segment,
    expectedEpisodeId?: string,
    frameVideoActive?: boolean,
  ) => boolean;
  /** 服务端确认删除或不存在后移除片段。 */
  remove: (dramaId: string, no: number, id: string, expectedEpisodeId?: string) => void;
  /** 片段工作区挂载时登记，卸载时取消 */
  watch: (dramaId: string, no: number) => () => void;
  /** 丢掉一部短剧缓存的全部集（删集后集号补位，按集号存的旧缓存会对错集），下次打开重新读 */
  forgetDrama: (dramaId: string) => void;
};

export const useDramaSegmentsStore = create<DramaSegmentsState>((set, get) => ({
  byEpisode: {},
  metaByEpisode: {},
  watching: {},
  runFloors: {},

  snapshotOf: (dramaId, no) => {
    const key = segmentKey(dramaId, no);
    const state = get();
    const current = resolveEpisodeMeta(state.byEpisode[key], state.metaByEpisode[key]);
    return current && { episodeId: current.episodeId, segmentVersion: current.segmentVersion };
  },

  activateEpisode: (dramaId, no, episodeId) =>
    set((state) => {
      const key = segmentKey(dramaId, no);
      const existing = state.byEpisode[key];
      const current = resolveEpisodeMeta(existing, state.metaByEpisode[key]);
      if (current?.episodeId === episodeId) return state;
      const keep = existing && !current?.episodeId;
      const listRef = keep ? existing : [];
      return {
        byEpisode: { ...state.byEpisode, [key]: listRef },
        metaByEpisode: {
          ...state.metaByEpisode,
          [key]: { ...(keep ? current : undefined), episodeId, listRef },
        },
      };
    }),

  clearEpisode: (dramaId, no, expectedEpisodeId) => {
    const current = get().snapshotOf(dramaId, no);
    get().setList(dramaId, no, [], undefined, { ...current, expectedEpisodeId });
  },

  applyConfirmedClear: (dramaId, no, episodeId, clearedVersion, segmentIds) =>
    set((state) => {
      const key = segmentKey(dramaId, no);
      const existing = state.byEpisode[key] ?? [];
      const current = resolveEpisodeMeta(state.byEpisode[key], state.metaByEpisode[key]);
      if (!belongsToEpisode(current?.episodeId, episodeId)) return state;

      const tombstones = new Set(current?.tombstones);
      for (const id of segmentIds) tombstones.add(id);
      const hasNewerRows = (current?.segmentVersion ?? -1) > clearedVersion;
      const listRef = existing.filter((row) => {
        /** 回执可能晚于新分镜，保留仍存活的更新成员。 */
        if (hasNewerRows && (row.segmentVersion ?? clearedVersion) >= clearedVersion)
          return !tombstones.has(row.id);
        tombstones.add(row.id);
        return false;
      });
      return {
        byEpisode: { ...state.byEpisode, [key]: listRef },
        metaByEpisode: {
          ...state.metaByEpisode,
          [key]: {
            episodeId,
            segmentVersion: maxOptNumber(current?.segmentVersion, clearedVersion),
            listVersion: maxOptNumber(current?.listVersion, clearedVersion),
            tombstones,
            listRef,
          },
        },
      };
    }),

  bumpRunFloor: (segmentId) =>
    set((state) => ({
      runFloors: { ...state.runFloors, [segmentId]: (state.runFloors[segmentId] ?? 0) + 1 },
    })),

  setList: (dramaId, no, list, startFloors, snapshot) => {
    let accepted = false;
    set((state) => {
      const key = segmentKey(dramaId, no);
      const existingList = state.byEpisode[key];
      const current = resolveEpisodeMeta(existingList, state.metaByEpisode[key]);
      const incomingEpisodeId = snapshot?.episodeId ?? firstSegmentEpisodeId(list);
      const incomingVersion = maxOptNumber(snapshot?.segmentVersion, maxSegmentVersion(list));
      const expected = snapshot?.expectedEpisodeId;
      const episodeId = current?.episodeId ?? expected ?? incomingEpisodeId;
      if (
        !belongsToEpisode(episodeId, expected) ||
        !belongsToEpisode(episodeId, incomingEpisodeId) ||
        list.some((item) => !belongsToEpisode(episodeId, item.episodeId))
      )
        return state;
      if (
        current?.segmentVersion !== undefined &&
        (incomingVersion === undefined
          ? list.length === 0
          : incomingVersion < current.segmentVersion)
      )
        return state;
      accepted = true;
      const existingById = new Map((existingList ?? []).map((item) => [item.id, item]));
      const reconciled = list
        .filter((item) => !current?.tombstones?.has(item.id))
        .map((incoming) => {
          const existing = existingById.get(incoming.id);
          const preserveLocalRun =
            startFloors !== undefined &&
            (state.runFloors[incoming.id] ?? 0) > (startFloors[incoming.id] ?? 0);
          const merged = reconcileSegmentRow(existing, incoming, preserveLocalRun);
          if (
            existing &&
            incoming.revision < existing.revision &&
            incomingVersion !== undefined &&
            incomingVersion > (merged.segmentVersion ?? -1)
          ) {
            return { ...merged, segmentVersion: incomingVersion };
          }
          return merged;
        });
      const nextList = sortSegments(reconciled);
      const nextVersion = maxOptNumber(current?.segmentVersion, incomingVersion);
      return {
        byEpisode: { ...state.byEpisode, [key]: nextList },
        metaByEpisode: {
          ...state.metaByEpisode,
          [key]: {
            episodeId,
            segmentVersion: nextVersion,
            listVersion: maxOptNumber(current?.listVersion, incomingVersion),
            tombstones: current?.tombstones,
            listRef: nextList,
          },
        },
      };
    });
    return accepted;
  },

  reorderLocal: (dramaId, no, ids, expectedEpisodeId) => {
    let accepted = false;
    set((state) => {
      const key = segmentKey(dramaId, no);
      const existingList = state.byEpisode[key];
      const current = resolveEpisodeMeta(existingList, state.metaByEpisode[key]);
      if (!existingList || !current || !belongsToEpisode(current.episodeId, expectedEpisodeId))
        return state;
      accepted = true;
      const nextList = applyOrder(existingList, ids);
      return {
        byEpisode: { ...state.byEpisode, [key]: nextList },
        metaByEpisode: { ...state.metaByEpisode, [key]: { ...current, listRef: nextList } },
      };
    });
    return accepted;
  },

  apply: (dramaId, no, segment, expectedEpisodeId, frameVideoActive) => {
    let accepted = false;
    set((state) => {
      const key = segmentKey(dramaId, no);
      const existingList = state.byEpisode[key];
      const current = resolveEpisodeMeta(existingList, state.metaByEpisode[key]);
      const episodeId = current?.episodeId ?? expectedEpisodeId ?? segment.episodeId;
      if (
        !belongsToEpisode(episodeId, expectedEpisodeId) ||
        !belongsToEpisode(episodeId, segment.episodeId) ||
        current?.tombstones?.has(segment.id) ||
        (current?.listVersion !== undefined &&
          typeof segment.segmentVersion === "number" &&
          segment.segmentVersion < current.listVersion &&
          !existingList?.some((item) => item.id === segment.id))
      )
        return state;
      accepted = true;
      const nextList = mergeSegment(existingList ?? [], segment, frameVideoActive);
      return {
        byEpisode: { ...state.byEpisode, [key]: nextList },
        metaByEpisode: {
          ...state.metaByEpisode,
          [key]: {
            episodeId,
            segmentVersion: maxOptNumber(current?.segmentVersion, segment.segmentVersion),
            listVersion: current?.listVersion,
            tombstones: current?.tombstones,
            listRef: nextList,
          },
        },
      };
    });
    return accepted;
  },

  remove: (dramaId, no, id, expectedEpisodeId) =>
    set((state) => {
      const key = segmentKey(dramaId, no);
      const existingList = state.byEpisode[key];
      const current = resolveEpisodeMeta(existingList, state.metaByEpisode[key]);
      if (!belongsToEpisode(current?.episodeId, expectedEpisodeId)) return state;
      const tombstones = new Set(current?.tombstones);
      tombstones.add(id);
      const nextList = removeSegmentFrom(existingList ?? [], id);
      return {
        byEpisode: { ...state.byEpisode, [key]: nextList },
        metaByEpisode: {
          ...state.metaByEpisode,
          [key]: {
            ...current,
            episodeId: current?.episodeId ?? expectedEpisodeId,
            tombstones,
            listRef: nextList,
          },
        },
      };
    }),

  watch: (dramaId, no) => {
    const key = segmentKey(dramaId, no);
    set((state) => ({ watching: { ...state.watching, [key]: (state.watching[key] ?? 0) + 1 } }));
    return () =>
      set((state) => {
        const count = (state.watching[key] ?? 1) - 1;
        const watching = { ...state.watching };
        if (count <= 0) delete watching[key];
        else watching[key] = count;
        return { watching };
      });
  },

  forgetDrama: (dramaId) =>
    set((state) => ({
      byEpisode: Object.fromEntries(
        Object.entries(state.byEpisode).filter(([key]) => parseSegmentKey(key).dramaId !== dramaId),
      ),
      metaByEpisode: Object.fromEntries(
        Object.entries(state.metaByEpisode).filter(
          ([key]) => parseSegmentKey(key).dramaId !== dramaId,
        ),
      ),
    })),
}));
