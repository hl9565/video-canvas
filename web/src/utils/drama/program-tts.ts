import type { TaskOutput, TaskView } from "@/api/generation-task/type";
import type { Capabilities, ParamOption } from "@/api/model/type";
import { paramEntries } from "@/utils/tasks/capabilities";

import { originalStageCAssetIds } from "./asset-id";
import { parseProgramAssetId } from "./program";

export const programSpeakers = (caps?: Capabilities) =>
  paramEntries(caps).find(
    (entry) => entry.name === "speaker" && entry.type === "enum" && entry.open,
  );

export function buildProgramTtsInput(
  caps: Capabilities | undefined,
  text: string,
  speaker?: ParamOption,
) {
  const prompt = text.trim();
  const input: Record<string, unknown> = {};
  const invalid = (error: string) => ({ input, error });
  if (!caps) return invalid("没有可用的音频模型");
  if (!prompt) return invalid("请输入配音台词");
  const max = caps.prompt?.max_length ?? 0;
  if (max > 0 && [...prompt].length > max) return invalid(`配音台词不能超过 ${max} 字`);
  const declared = programSpeakers(caps);
  const choice = speaker ?? declared?.default;
  if (declared && choice !== undefined) {
    const option = declared.options?.find((item) => String(item) === String(choice));
    if (option === undefined) return invalid("所选音色不可用");
    input.speaker = option;
  }
  return { input: { prompt, ...input }, error: null };
}

/** 任务持久化后的自有产物；节目草稿只接收 ID，不读取供应商 URL。 */
export function programTtsAsset(task: TaskView | undefined) {
  if (task?.status !== "succeeded") return null;
  for (const output of (task.outputs ?? []) as (TaskOutput & { asset_id_str?: string })[]) {
    if (output.media_type !== "audio") continue;
    const assetId =
      parseProgramAssetId(originalStageCAssetIds.get(output)?.asset_id ?? "") ??
      parseProgramAssetId(output.asset_id_str ?? "") ??
      parseProgramAssetId(output.asset_id ?? "");
    if (assetId !== null) {
      const ms = output.duration_ms;
      return {
        assetId,
        durationFrames: ms && ms > 0 ? Math.max(1, Math.ceil((ms * 24) / 1000)) : null,
      };
    }
  }
  return null;
}
