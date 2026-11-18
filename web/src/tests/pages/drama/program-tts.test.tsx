import { expect, spyOn, test } from "bun:test";
import { useState } from "react";

import { getGenerationTask } from "@/api/generation-task";
import type { Segment } from "@/api/drama/type";
import type { ModelInfo } from "@/api/model/type";
import { ProgramPanel } from "@/pages/drama/studio/program/program-panel";
import { useProgram } from "@/pages/drama/studio/program/use-program";
import { useProgramTts } from "@/pages/drama/studio/program/use-program-tts";
import { useDramaProgramTtsStore } from "@/store/drama-program-tts";
import { useModelsStore } from "@/store/models";
import { useTasksStore } from "@/store/tasks";
import { programTtsAsset } from "@/utils/drama/program-tts";
import * as gateway from "@/utils/tasks/gateway";
import instance, { ApiError } from "@/utils/requests/request";
import service from "@/utils/requests/service";
import { parseServerMessage } from "@/utils/ws/socket-client";
import { handleTaskView } from "@/utils/ws/task-events";

import { installDom } from "../../helpers/dom";
import { makeTask } from "../../utils/tasks/fixtures";

const audioModel: ModelInfo = {
  key: "tts",
  kind: "audio",
  label: "配音模型",
  pricing: { billing: "per_call", unit: 5 },
  capabilities: {
    refs: {
      image: { on: false, max: 0, max_mb: 0 },
      video: { on: false, max: 0, max_mb: 0 },
      audio: { on: false, max: 0, max_mb: 0 },
    },
    prompt: { max_length: 100 },
  },
};

test("配音不确定提交保留幂等键，失败不插入，成功任务的重复快照只附加一次", async () => {
  const dom = await installDom();
  const task = makeTask({
    id: 123,
    canvas_id: "canvas-1",
    node_id: "program-tts:ep-1",
    kind: "audio",
    status: "running",
  });
  const second = { ...task, id: 124 };
  const submit = spyOn(gateway, "submitProgramTtsTask")
    .mockRejectedValueOnce(new ApiError("timeout", "TIMEOUT", 0))
    .mockResolvedValueOnce({ node_id: task.node_id, task })
    .mockResolvedValueOnce({ node_id: task.node_id, task: second });
  const attachments: Array<{ id: string; assetId: number | string }> = [];
  function Harness() {
    const tts = useProgramTts({
      canvasId: "canvas-1",
      episodeId: "ep-1",
      onAttach: (id, asset) => attachments.push({ id, assetId: asset.assetId }),
    });
    return (
      <>
        <button onClick={() => tts.submit(audioModel, "台词")}>生成</button>
        {tts.retryable && <button onClick={tts.retry}>重试</button>}
        <output>{tts.task?.status}</output>
      </>
    );
  }
  const mounted = await dom.mount(<Harness />);
  try {
    const output = {
      media_type: "audio",
      asset_id: 7,
      asset_id_str: "9007199254740993",
      url: "https://provider.invalid/tmp",
    };
    await dom.click(mounted.container.querySelector("button"));
    await dom.click(mounted.container.querySelectorAll("button")[1]);
    expect(submit.mock.calls[1]?.[0]).toEqual(submit.mock.calls[0]?.[0]);
    expect(submit.mock.calls[0]?.[0]?.input).toEqual({ prompt: "台词" });
    await dom.act(async () => {
      useTasksStore.getState().upsert({ ...task, status: "failed", version: 2, outputs: [output] });
    });
    expect(attachments).toEqual([]);
    await dom.click(mounted.container.querySelector("button"));
    expect(submit.mock.calls[2]?.[0]?.idempotencyKey).not.toBe(
      submit.mock.calls[1]?.[0]?.idempotencyKey,
    );
    for (const version of [2, 3, 3]) {
      await dom.act(async () => {
        useTasksStore
          .getState()
          .upsert({ ...second, status: "succeeded", version, outputs: [output] });
      });
    }
    expect(attachments).toEqual([{ id: "124", assetId: "9007199254740993" }]);
  } finally {
    submit.mockRestore();
    await mounted.unmount();
    useTasksStore.setState({ tasks: {} });
    useDramaProgramTtsStore.setState({ byEpisode: {} });
    dom.restore();
  }
});

const segments: Segment[] = [
  {
    id: "segment",
    episodeNo: 1,
    seq: 0,
    visualKind: "black",
    seconds: 4,
    prompt: "",
    chosenTaskId: "",
    takes: [],
    status: "idle",
    revision: 1,
    updatedAt: "",
    adaptations: {},
  },
];

test.each([
  ["HTTP 返回前", true],
  ["HTTP 返回后", false],
] as const)("%s切集，已付费配音只进入原集的草稿一次", async (_label, leaveWhilePending) => {
  const dom = await installDom();
  const task = makeTask({
    id: "t-paid",
    canvas_id: "canvas-1",
    node_id: "program-tts:ep-1",
    kind: "audio",
  });
  const item = { node_id: task.node_id, task };
  const { promise, resolve } = Promise.withResolvers<typeof item>();
  const submit = spyOn(gateway, "submitProgramTtsTask").mockReturnValue(promise);
  const get = spyOn(service, "get").mockImplementation(async (url: string) => ({
    episode_id: url.endsWith("/2/program") ? "ep-2" : "ep-1",
    segment_version: 1,
    edit_schema: 1,
    program: { v: 1, audio: [], overlays: [] },
  }));
  const previousModels = useModelsStore.getState().byKind;
  useModelsStore.setState({
    byKind: {
      ...previousModels,
      audio: { status: "ready", models: [audioModel], loadedAt: Date.now() },
    },
  });
  function Harness() {
    const [no, setNo] = useState(1);
    const episodeId = `ep-${no}`;
    const program = useProgram({ dramaId: "d", episodeId, episodeNo: no });
    return (
      <>
        <button onClick={() => setNo(no === 1 ? 2 : 1)}>切换分集</button>
        <ProgramPanel
          key={episodeId}
          open
          canvasId="canvas-1"
          episodeId={episodeId}
          segments={segments}
          program={program}
        />
      </>
    );
  }
  const mounted = await dom.mount(<Harness />);
  try {
    const { container } = mounted;
    const button = (root: ParentNode, label: string) =>
      [...root.querySelectorAll("button")].find((el) => el.textContent?.trim() === label);
    const toggleEpisode = () => dom.click(button(container, "切换分集"));
    const completeSubmit = () =>
      dom.act(async () => {
        useTasksStore.getState().upsert(task);
        resolve(item);
      });
    const succeed = (version: number) =>
      dom.act(async () => {
        useTasksStore.getState().upsert({
          ...task,
          status: "succeeded",
          version,
          outputs: [{ media_type: "audio", asset_id: 41, duration_ms: 1000 }],
        });
      });
    const cues = () => container.querySelectorAll('[data-cue-id="tts-t-paid"]');

    await dom.click(button(container, "AI 生成配音 (TTS)"));
    await dom.type(dom.win.document.querySelector("textarea"), "付费台词");
    await dom.click(button(dom.win.document, "生成配音"));
    expect(submit).toHaveBeenCalledTimes(1);
    expect(submit.mock.calls[0]?.[0].episodeId).toBe("ep-1");

    if (!leaveWhilePending) await completeSubmit();
    await toggleEpisode();
    if (leaveWhilePending) await completeSubmit();
    await succeed(2);
    expect(cues()).toHaveLength(0);

    await toggleEpisode();
    expect(cues()).toHaveLength(1);
    expect(container.querySelector<HTMLInputElement>("input[inputmode=numeric]")?.value).toBe("41");
    await succeed(3);
    expect(cues()).toHaveLength(1);
    await toggleEpisode();
    expect(cues()).toHaveLength(0);
    await toggleEpisode();
    expect(cues()).toHaveLength(1);
  } finally {
    await mounted.unmount();
    get.mockRestore();
    submit.mockRestore();
    useTasksStore.setState({ tasks: {} });
    useDramaProgramTtsStore.setState({ byEpisode: {} });
    useModelsStore.setState({ byKind: previousModels });
    dom.restore();
  }
});

test("WS task.updated 与 HTTP 读取纯数字 > MAX_SAFE_INTEGER asset_id 9007199254740993 精确附加到配音草稿", async () => {
  const dom = await installDom();
  const prevAdapter = instance.defaults.adapter;
  const rawTask = (id: string) =>
    `{"id":${JSON.stringify(id)},"canvas_id":"canvas-1","node_id":"program-tts:ep-1","kind":"audio","model_id":"tts","status":"succeeded","progress":100,"outputs":[{"media_type":"audio","asset_id":9007199254740993,"duration_ms":1000}],"error_code":null,"error_message":null,"credits":5,"charged_credits":5,"version":2,"deadline_at":null,"created_at":"2026-11-17T00:00:00Z","submitted_at":null,"finished_at":null}`;
  instance.defaults.adapter = async (config) => ({
    data: rawTask("t-http-big"),
    status: 200,
    statusText: "OK",
    headers: {},
    config,
  });

  const running = makeTask({
    id: "t-ws-big",
    canvas_id: "canvas-1",
    node_id: "program-tts:ep-1",
    kind: "audio",
    status: "running",
  });
  const submit = spyOn(gateway, "submitProgramTtsTask").mockResolvedValueOnce({
    node_id: running.node_id,
    task: running,
  });
  const attachments: Array<{ id: string; assetId: number | string }> = [];
  function BigIdHarness() {
    const tts = useProgramTts({
      canvasId: "canvas-1",
      episodeId: "ep-1",
      onAttach: (id, asset) => attachments.push({ id, assetId: asset.assetId }),
    });
    return <button onClick={() => tts.submit(audioModel, "配")}>生成</button>;
  }
  const mounted = await dom.mount(<BigIdHarness />);
  try {
    const httpTask = await getGenerationTask("t-http-big");
    expect(programTtsAsset(httpTask)).toEqual({
      assetId: "9007199254740993",
      durationFrames: 24,
    });

    await dom.click(mounted.container.querySelector("button"));
    const msg = parseServerMessage(`{"type":"task.updated","data":${rawTask("t-ws-big")}}`);
    expect(msg?.type).toBe("task.updated");
    await dom.act(async () => {
      if (msg?.type === "task.updated") handleTaskView(msg.data, "live");
    });
    expect(attachments).toEqual([{ id: "t-ws-big", assetId: "9007199254740993" }]);
  } finally {
    instance.defaults.adapter = prevAdapter;
    submit.mockRestore();
    await mounted.unmount();
    useTasksStore.setState({ tasks: {} });
    useDramaProgramTtsStore.setState({ byEpisode: {} });
    dom.restore();
  }
});
