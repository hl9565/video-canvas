import { useEffect } from "react";

import type { ModelInfo, ParamOption } from "@/api/model/type";
import {
  EMPTY_PROGRAM_TTS,
  useDramaProgramTtsStore,
  type ProgramTtsAttempt,
} from "@/store/drama-program-tts";
import { useTask } from "@/store/tasks";
import { buildProgramTtsInput, programTtsAsset } from "@/utils/drama/program-tts";
import { cancelTasks, submitProgramTtsTask } from "@/utils/tasks/gateway";
import { isActiveStatus, isCancelableTask, taskKey } from "@/utils/tasks/status";
import { describeSubmitError, isRetryableSubmitError } from "@/utils/tasks/submit";

/** 持久化任务产出的自有音频。 */
type AudioAsset = NonNullable<ReturnType<typeof programTtsAsset>>;

export function useProgramTts({
  canvasId,
  episodeId,
  onAttach,
  canAttach = true,
}: {
  canvasId: string;
  episodeId: string;
  onAttach: (taskId: string, asset: AudioAsset) => void;
  canAttach?: boolean;
}) {
  const current =
    useDramaProgramTtsStore((state) => state.byEpisode[episodeId]) ?? EMPTY_PROGRAM_TTS;
  const { patch, markAttached } = useDramaProgramTtsStore.getState();
  const readEntry = (id: string) =>
    useDramaProgramTtsStore.getState().byEpisode[id] ?? EMPTY_PROGRAM_TTS;
  const task = useTask(current.taskId);
  const asset = programTtsAsset(task);
  const { taskId } = current;
  const attached = !!taskId && current.attachedTaskIds.includes(taskId);
  useEffect(() => {
    if (!canAttach || !taskId || attached || !task) return;
    if (task.canvas_id !== canvasId || task.node_id !== `program-tts:${episodeId}`) return;
    const output = programTtsAsset(task);
    if (output && markAttached(episodeId, taskId)) onAttach(taskId, output);
  }, [attached, canAttach, canvasId, episodeId, markAttached, onAttach, task, taskId]);

  const busy = Boolean(
    current.pending ||
    current.canceling ||
    (taskId && (!task || isActiveStatus(task.status) || (asset && !attached))),
  );
  const run = async (attempt: ProgramTtsAttempt) => {
    const id = attempt.episodeId;
    const state = readEntry(id);
    if (state.pending || state.canceling) return;
    patch(id, {
      attempt,
      taskId: undefined,
      pending: true,
      retryable: false,
      error: undefined,
    });
    try {
      const item = await submitProgramTtsTask(attempt);
      if (!item?.task) {
        patch(id, { pending: false, error: describeSubmitError(item?.error).message });
      } else {
        patch(id, { pending: false, taskId: taskKey(item.task.id) });
      }
    } catch (error) {
      const retryable = isRetryableSubmitError(error);
      patch(id, {
        pending: false,
        retryable,
        error: retryable
          ? "提交结果暂未确认，请重试同一次请求"
          : describeSubmitError(error).message,
      });
    }
  };

  const submit = (model: ModelInfo | undefined, text: string, speaker?: ParamOption) => {
    if (busy || readEntry(episodeId).retryable || !canvasId || !episodeId) return;
    const built = buildProgramTtsInput(
      model?.kind === "audio" ? model.capabilities : undefined,
      text,
      speaker,
    );
    if (built.error || !model) {
      patch(episodeId, { error: built.error ?? "没有可用的音频模型" });
      return;
    }
    void run({
      canvasId,
      episodeId,
      modelId: model.key,
      input: built.input,
      idempotencyKey: crypto.randomUUID(),
    });
  };

  const retry = () => {
    const { attempt, retryable } = readEntry(episodeId);
    if (retryable && attempt) void run(attempt);
  };

  const cancel = async () => {
    const state = readEntry(episodeId);
    if (
      !task ||
      !isCancelableTask(task) ||
      state.taskId !== taskId ||
      state.pending ||
      state.canceling
    )
      return;
    patch(episodeId, { canceling: true, error: undefined });
    try {
      const failed = await cancelTasks([task.id]);
      if (failed.length) patch(episodeId, { error: "取消失败，请重试" });
    } finally {
      patch(episodeId, { canceling: false });
    }
  };
  const status = task?.status;
  const taskError =
    status === "succeeded" && !asset
      ? "任务完成，但没有可用的自有音频素材"
      : status === "failed" || status === "expired"
        ? task?.error_message || "配音生成失败"
        : null;
  const { pending, canceling, retryable, error } = current;
  return { pending, canceling, retryable, error, task, busy, taskError, submit, retry, cancel };
}

export type ProgramTtsState = ReturnType<typeof useProgramTts>;
