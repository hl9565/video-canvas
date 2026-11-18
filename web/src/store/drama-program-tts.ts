import { create } from "zustand";

import type { ProgramAudioCueInput } from "@/api/drama/type";
import type { submitProgramTtsTask } from "@/utils/tasks/gateway";

/** 一次提交及其重试共享的请求。 */
export type ProgramTtsAttempt = Parameters<typeof submitProgramTtsTask>[0];

/** 分集的配音任务状态，切集后仍由任务库提供进度。 */
export type ProgramTtsEntry = {
  /** 最近一次已受理的任务 ID。 */
  taskId?: string;
  /** 提交请求正在等待响应。 */
  pending?: boolean;
  /** 取消请求正在等待响应。 */
  canceling?: boolean;
  /** 提交结果未知，只允许用原请求重试。 */
  retryable?: boolean;
  /** 提交或取消的就地错误。 */
  error?: string;
  /** 最近一次请求，包含重试必须复用的幂等键。 */
  attempt?: ProgramTtsAttempt;
  /** 已交给节目草稿的任务 ID。 */
  attachedTaskIds: string[];
  /** 已附加但尚未保存的配音条目，切回该集时恢复。 */
  unsavedCues: ProgramAudioCueInput[];
};

export const EMPTY_PROGRAM_TTS: ProgramTtsEntry = { attachedTaskIds: [], unsavedCues: [] };

/** 任务 ID 对应的节目条目 ID，供草稿和任务快照去重。 */
export const programTtsCueId = (taskId: string) => `tts-${taskId}`;

/** 配音状态在本次工作台会话中按集隔离。 */
type ProgramTtsStore = {
  /** 稳定集 ID 到配音状态的映射。 */
  byEpisode: Record<string, ProgramTtsEntry>;
  /** 更新指定分集的状态。 */
  patch: (episodeId: string, change: Partial<ProgramTtsEntry>) => void;
  /** 首次认领任务返回 true，重复认领返回 false。 */
  markAttached: (episodeId: string, taskId: string) => boolean;
};

export const useDramaProgramTtsStore = create<ProgramTtsStore>((set, get) => ({
  byEpisode: {},
  patch: (episodeId, change) =>
    set((state) => ({
      byEpisode: {
        ...state.byEpisode,
        [episodeId]: { ...(state.byEpisode[episodeId] ?? EMPTY_PROGRAM_TTS), ...change },
      },
    })),
  markAttached: (episodeId, taskId) => {
    const entry = get().byEpisode[episodeId];
    if (entry?.taskId !== taskId || entry.attachedTaskIds.includes(taskId)) return false;
    get().patch(episodeId, { attachedTaskIds: [...entry.attachedTaskIds, taskId] });
    return true;
  },
}));
