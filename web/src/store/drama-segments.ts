import { create } from "zustand";

import type { Segment } from "@/api/drama/type";
import {
  mergeSegment,
  preserveNewerFrameRun,
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

type DramaSegmentsState = {
  /** 每集的片段，按 seq 排序；没加载过的集没有键 */
  byEpisode: Record<string, Segment[]>;
  /** 正在打开的片段工作区（引用计数）：WebSocket 重连 / 回到前台时只对账这些 */
  watching: Record<string, number>;
  /** 单片段首帧本地更新纪元，防止发起较早的整集拉取写回旧流程快照 */
  runFloors: Record<string, number>;
  /** 标记某片段发生了本地首帧流程状态推进 */
  bumpRunFloor: (segmentId: string) => void;
  /** 整体替换一集的片段（HTTP 读取 / 重排返回），按片段保留更新的首帧快照 */
  setList: (
    dramaId: string,
    no: number,
    list: readonly Segment[],
    startFloors?: Readonly<Record<string, number>>,
  ) => void;
  /** 并入一条片段快照（WebSocket 推送 / 接口返回） */
  apply: (dramaId: string, no: number, segment: Segment) => void;
  /** 删掉一个片段 */
  remove: (dramaId: string, no: number, id: string) => void;
  /** 片段工作区挂载时登记，卸载时取消 */
  watch: (dramaId: string, no: number) => () => void;
  /** 丢掉一部短剧缓存的全部集（删集后集号补位，按集号存的旧缓存会对错集），下次打开重新读 */
  forgetDrama: (dramaId: string) => void;
};

export const useDramaSegmentsStore = create<DramaSegmentsState>((set) => ({
  byEpisode: {},
  watching: {},
  runFloors: {},

  bumpRunFloor: (segmentId) =>
    set((state) => ({
      runFloors: { ...state.runFloors, [segmentId]: (state.runFloors[segmentId] ?? 0) + 1 },
    })),

  setList: (dramaId, no, list, startFloors) =>
    set((state) => {
      const key = segmentKey(dramaId, no);
      const existingById = new Map((state.byEpisode[key] ?? []).map((item) => [item.id, item]));
      const reconciled = list.map((incoming) => {
        const existing = existingById.get(incoming.id);
        if (
          existing &&
          startFloors &&
          (state.runFloors[incoming.id] ?? 0) > (startFloors[incoming.id] ?? 0)
        ) {
          return { ...incoming, frameRun: existing.frameRun };
        }
        return preserveNewerFrameRun(existing, incoming);
      });
      return {
        byEpisode: { ...state.byEpisode, [key]: sortSegments(reconciled) },
      };
    }),

  apply: (dramaId, no, segment) =>
    set((state) => {
      const key = segmentKey(dramaId, no);
      return {
        byEpisode: { ...state.byEpisode, [key]: mergeSegment(state.byEpisode[key] ?? [], segment) },
      };
    }),

  remove: (dramaId, no, id) =>
    set((state) => {
      const key = segmentKey(dramaId, no);
      return {
        byEpisode: { ...state.byEpisode, [key]: removeSegmentFrom(state.byEpisode[key] ?? [], id) },
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
    })),
}));
