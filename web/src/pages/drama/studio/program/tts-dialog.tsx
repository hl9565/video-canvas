import { useState } from "react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Textarea } from "@/components/ui/textarea";
import { useRemoteModels } from "@/hooks/use-models";
import { buildProgramTtsInput, programSpeakers } from "@/utils/drama/program-tts";
import { isCancelableTask } from "@/utils/tasks/status";

import type { ProgramTtsState } from "./use-program-tts";

export function TtsDialog({
  open,
  onOpenChange,
  generation,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  generation: ProgramTtsState;
}) {
  const remote = useRemoteModels("audio", open);
  const models = remote.models.filter((model) => model.kind === "audio");
  const [modelId, setModelId] = useState("");
  const [text, setText] = useState("");
  const [selectedSpeaker, setSpeaker] = useState("");
  const model = models.find((item) => item.key === modelId) ?? models[0];
  const speakers = programSpeakers(model?.capabilities);
  const speaker = speakers?.options?.find((value) => String(value) === selectedSpeaker);
  const built = buildProgramTtsInput(model?.capabilities, text, speaker);
  const task = generation.task;
  const locked = generation.busy || generation.retryable;
  const priceLabel = remote.options.find((item) => item.id === model?.key)?.priceLabel;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="flex max-h-[min(88svh,520px)] flex-col">
        <DialogHeader>
          <DialogTitle>AI 生成配音 (TTS)</DialogTitle>
        </DialogHeader>
        <form
          className="flex min-h-0 flex-col gap-3"
          onSubmit={(event) => {
            event.preventDefault();
            generation.submit(model, text, speaker);
          }}
        >
          <div className="grid min-h-0 gap-3 overflow-y-auto px-1">
            <label className="grid gap-1 text-xs">
              音频模型
              <select
                className="border-input rounded-lg border p-2"
                value={model?.key ?? ""}
                disabled={locked || !models.length}
                onChange={(event) => {
                  setModelId(event.target.value);
                  setSpeaker("");
                }}
              >
                {!models.length && (
                  <option value="">
                    {remote.status === "ready" ? "暂无已启用的音频模型" : "读取音频模型…"}
                  </option>
                )}
                {models.map((item) => (
                  <option key={item.key} value={item.key}>
                    {item.label}
                  </option>
                ))}
              </select>
            </label>
            {remote.status === "error" && (
              <Button
                type="button"
                size="sm"
                variant="outline"
                onClick={() => void remote.reload()}
              >
                重试读取模型
              </Button>
            )}
            {speakers?.options?.length ? (
              <label className="grid gap-1 text-xs">
                音色
                <select
                  className="border-input rounded-lg border p-2"
                  value={selectedSpeaker}
                  disabled={locked}
                  onChange={(event) => setSpeaker(event.target.value)}
                >
                  <option value="">默认音色</option>
                  {speakers.options.map((value) => (
                    <option key={value} value={String(value)}>
                      {value}
                    </option>
                  ))}
                </select>
              </label>
            ) : null}
            <label className="grid gap-1 text-xs">
              配音台词
              <Textarea
                className="max-h-36 overflow-y-auto"
                value={text}
                onChange={(event) => setText(event.target.value)}
                disabled={locked}
              />
            </label>
            {text.trim() && built.error && !locked && (
              <p role="alert" className="text-destructive text-xs">
                {built.error}
              </p>
            )}
            {priceLabel && !task && (
              <p className="text-muted-foreground text-xs">模型价格：{priceLabel}</p>
            )}
            {task && (
              <p role="status" className="text-muted-foreground text-xs">
                {generation.busy
                  ? `配音生成中${task.progress === null ? "" : ` · ${task.progress}%`}`
                  : task.status === "canceled"
                    ? "配音已取消"
                    : task.status === "succeeded" && !generation.taskError
                      ? "配音已加入节目草稿"
                      : "配音已结束"}
                {" · "}
                {task.charged_credits === null
                  ? `冻结 ${task.credits} 积分`
                  : `实扣 ${task.charged_credits} 积分`}
              </p>
            )}
            {(generation.error || generation.taskError) && (
              <p role="alert" className="text-destructive text-xs">
                {generation.error ?? generation.taskError}
              </p>
            )}
          </div>
          <DialogFooter>
            {task && isCancelableTask(task) && (
              <Button
                type="button"
                variant="outline"
                disabled={generation.canceling}
                onClick={() => void generation.cancel()}
              >
                {generation.canceling ? "取消中…" : "取消配音"}
              </Button>
            )}
            {generation.retryable ? (
              <Button type="button" onClick={generation.retry}>
                重试同一次请求
              </Button>
            ) : (
              <Button type="submit" disabled={locked || !!built.error}>
                {generation.pending
                  ? "提交中…"
                  : generation.taskError || task?.status === "canceled"
                    ? "重新生成配音"
                    : "生成配音"}
              </Button>
            )}
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
