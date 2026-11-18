import { useEffect } from "react";
import {
  afterAll,
  afterEach,
  beforeAll,
  beforeEach,
  describe,
  expect,
  spyOn,
  test,
} from "bun:test";

import { mapDrama, mapSegment } from "@/api/drama";
import type { BackendSegmentFrameRun, BackendSegmentView } from "@/api/drama/type";
import { FrameRunDrawer } from "@/components/drama/frame-run-drawer";
import { ImportTakesDialog } from "@/components/drama/import-takes-dialog";
import { RejectTakeDialog } from "@/components/drama/reject-take-dialog";
import { GenerateControls } from "@/pages/drama/studio/generate-controls";
import { TakeCarousel } from "@/pages/drama/studio/take-carousel";
import { StudioContext, useStudioState, type Studio } from "@/pages/drama/studio/use-studio";
import { useDramaSegmentsStore } from "@/store/drama-segments";
import { useModelsStore } from "@/store/models";
import { useCreditsStore } from "@/store/credits";
import { useTasksStore } from "@/store/tasks";
import { installDom, type DomEnv } from "@/tests/helpers/dom";
import { handleDramaSegmentEvent, reloadSegments } from "@/utils/drama/segment-events";
import service from "@/utils/requests/service";
import { ApiError } from "@/utils/requests/request";
import * as taskApi from "@/api/generation-task";
import { toast } from "sonner";

let dom: DomEnv;
const spies: Array<{ mockRestore: () => void }> = [];
const calls: Array<{ url: string; body: Record<string, unknown> }> = [];
let run: BackendSegmentFrameRun | null = null;
let failure: { suffix: string; error: ApiError } | null = null;
const rawSegment = (): BackendSegmentView => ({
  id: "s1",
  episode_no: 1,
  seq: 0,
  seconds: 5,
  prompt: "@[场景图](n1) 镜头缓慢推进",
  takes: [],
  chosen_task_id: "",
  revision: 2,
  segment_version: 3,
  status: "idle",
  updated_at: "",
  frame_run: run,
});
const makeRun = (patch: Partial<BackendSegmentFrameRun> = {}): BackendSegmentFrameRun => ({
  id: "r1",
  segment_id: "s1",
  request_key: "req",
  frame_source: "existing",
  state: "frame_ready",
  segment_revision: 2,
  revision: 1,
  confirmed_version: 0,
  model_key: "",
  frame_asset_id: "cropped",
  frame_sha256: "a".repeat(64),
  adopt_if_empty: false,
  error_code: "",
  error_message: "",
  created_at: "",
  updated_at: "",
  ...patch,
});
const drama = mapDrama({
  id: "d1",
  canvas_id: "c1",
  video_model: "ref2v",
  ratio: "16:9",
  episodes: [{ no: 1, title: "第一集", revision: 1, segment_version: 3 }],
  active_jobs: [],
} as never);
const imageAsset = {
  id: "n1",
  type: "scene",
  kind: "image",
  label: "场景图",
  name: "场景图",
  look: "",
  episodes: [],
  src: "/frame.png",
  assetId: "f1",
  generating: false,
} as const;
const task = {
  id: "t1",
  canvas_id: "c1",
  node_id: "seg:s1",
  kind: "video",
  model_id: "m-1c89decdf8fd",
  status: "queued",
  outputs: [],
  version: 1,
  credits: 10,
};

const confirmedRun = makeRun({ state: "confirmed", revision: 4, confirmed_version: 1 });
const pendingRun = makeRun({
  state: "video_pending",
  revision: 6,
  confirmed_version: 1,
  video_task_id: task.id,
});
const adoptedTake = {
  task_id: "adopted",
  status: "succeeded",
  asset: { id: 2, kind: "video", url: "/adopted.mp4" },
  error_message: "",
  created_at: "2026-11-17T10:01:00Z",
} as const;
const completedSegment = (videoResolved = false): BackendSegmentView => ({
  ...rawSegment(),
  chosen_task_id: adoptedTake.task_id,
  takes: videoResolved ? [adoptedTake, { ...adoptedTake, task_id: task.id }] : [adoptedTake],
  status: "done",
  frame_run: confirmedRun,
});

beforeAll(async () => {
  dom = await installDom();
});
afterAll(() => dom.restore());
afterEach(() => {
  while (spies.length) spies.pop()!.mockRestore();
  useDramaSegmentsStore.setState({ byEpisode: {}, watching: {}, runFloors: {} });
  useTasksStore.setState({ tasks: {} });
  dom.win.history.replaceState(null, "", "/");
  dom.win.document.body.innerHTML = "";
});
beforeEach(() => {
  run = null;
  failure = null;
  calls.length = 0;
  const capabilities = {
    ops: ["i2v", "omni"],
    refs: { image: { on: true, max: 4 }, video: { on: false }, audio: { on: false } },
    prompt: { max_length: 1000 },
    prompt_format: "seedance",
    params: {},
  };
  useModelsStore.setState({
    byKind: {
      video: {
        status: "ready",
        loadedAt: Date.now(),
        models: [
          {
            key: "ref2v",
            label: "Ref2V",
            kind: "video",
            pricing: { billing: "per_call", unit: 10 },
            capabilities,
          },
        ],
      },
      image: {
        status: "ready",
        loadedAt: Date.now(),
        models: [
          {
            key: "img1",
            label: "图片模型",
            kind: "image",
            capabilities: { ...capabilities, ops: ["t2i"] },
            pricing: { billing: "per_call", unit: 1 },
          },
        ],
      },
    },
  } as never);
  useCreditsStore.setState({ credits: { available: 100 } } as never);
  spies.push(
    spyOn(service, "get").mockImplementation(async (url: string) => {
      if (url.endsWith("/segments")) return { items: [rawSegment()] } as never;
      if (url === "/drama-segments/s1") return rawSegment() as never;
      if (url.includes("/frame-runs/")) return run as never;
      if (url.startsWith("/assets/"))
        return {
          id: "cropped",
          kind: "image",
          url: "/actual-16-9.png",
          width: 1600,
          height: 900,
        } as never;
      if (url === "/canvas")
        return { list: [{ id: "c1", title: "短剧画布", revision: 1 }], total: 1 } as never;
      return {
        id: "c1",
        payload_json: {
          nodes: [
            {
              id: "n1",
              type: "canvas",
              position: { x: 0, y: 0 },
              data: {
                kind: "image",
                label: "场景图",
                assetId: "f1",
                src: "/frame.png",
                drama: { type: "scene", name: "场景图", episodes: [] },
              },
            },
          ],
          edges: [],
        },
      } as never;
    }),
    spyOn(service, "post").mockImplementation(async (url: string, body?: unknown) => {
      const input = (body ?? {}) as Record<string, unknown>;
      calls.push({ url, body: input });
      if (failure && url.endsWith(failure.suffix)) {
        const error = failure.error;
        failure = null;
        throw error;
      }
      if (url === "/assets/upload-intents") return { mode: "proxy" } as never;
      if (url.endsWith("/quote"))
        return {
          effective_input: input.input,
          raw_size: "1280×720",
          credits: 10,
          quote_token: "quote-1",
          expires_at: "2099-01-01",
          price_fingerprint: "p",
        } as never;
      if (url.endsWith("/frame-runs")) {
        run = makeRun({
          request_key: input.request_key as string,
          frame_source: input.frame_source as BackendSegmentFrameRun["frame_source"],
          state:
            input.frame_source === "canvas"
              ? "draft"
              : input.frame_source === "generate"
                ? "frame_pending"
                : "frame_ready",
          origin_canvas_id: "c1",
          canvas_ticket: "t&1",
        });
        return run as never;
      }
      if (url.endsWith("/confirm")) {
        run = {
          ...run!,
          state: "confirmed",
          revision: run!.revision + 1,
          confirmed_version: run!.confirmed_version + 1,
          confirmed_input: input.input as Record<string, unknown>,
          model_key: input.model_id as string,
        };
        return run as never;
      }
      if (url.endsWith("/submit")) {
        run = { ...run!, state: "video_pending", revision: run!.revision + 1 };
        return { run, task } as never;
      }
      if (url.endsWith("/cancel")) {
        run = { ...run!, state: "cancelled", revision: run!.revision + 1 };
        return run as never;
      }
      if (url.endsWith("/frame")) {
        run = {
          ...run!,
          state: "frame_ready",
          revision: run!.revision + 1,
          frame_asset_id: "cropped-2",
          frame_sha256: "b".repeat(64),
          confirmed_input: undefined,
        };
        return run as never;
      }
      return { task } as never;
    }),
  );
});

function Harness({
  open = true,
  onStudio,
}: {
  open?: boolean;
  onStudio?: (studio: Studio) => void;
}) {
  const studio = useStudioState(drama, 1, async () => {});
  useEffect(() => onStudio?.(studio), [onStudio, studio]);
  return (
    <StudioContext.Provider value={studio}>
      <GenerateControls />
      <FrameRunDrawer
        open={open}
        onOpenChange={() => {}}
        segment={studio.selected}
        dramaId="d1"
        episodeNo={1}
        canvasId="c1"
        assets={[{ ...imageAsset, episodes: [] }]}
        actions={studio.frameRunActions}
      />
    </StudioContext.Provider>
  );
}

const button = (text: string) =>
  Array.from(dom.win.document.querySelectorAll("button")).find(
    (el) => el.textContent?.replace(/\s/g, "") === text.replace(/\s/g, ""),
  );
const select = async (label: string, value: string) =>
  dom.act(async () => {
    const el = dom.win.document.querySelector(`select[aria-label="${label}"]`) as HTMLSelectElement;
    el.value = value;
    el.dispatchEvent(new dom.win.Event("change", { bubbles: true }));
  });

/** 等待可观察状态，避免用固定延时猜测轮询完成时间。 */
async function waitFor(check: () => boolean, timeoutMs = 4000) {
  const deadline = Date.now() + timeoutMs;
  while (!check()) {
    if (Date.now() >= deadline) throw new Error("等待界面状态超时");
    await dom.flush(20);
  }
}

describe("首帧控镜与普通视频生成", () => {
  test("生成中的流程自动轮询到首帧就绪并展示实际首帧", async () => {
    run = makeRun({ state: "frame_pending", frame_asset_id: undefined, frame_sha256: undefined });
    const mounted = await dom.mount(<Harness />);
    try {
      await dom.flush(30);
      run = makeRun({ state: "frame_ready", revision: 2 });
      await waitFor(() => !!dom.win.document.querySelector('img[alt="实际送入 I2V 的首帧"]'));
      expect(dom.win.document.body.textContent).toContain("首帧就绪");
      expect(dom.win.document.querySelector('img[alt="实际送入 I2V 的首帧"]')).not.toBeNull();
    } finally {
      await mounted.unmount();
    }
  });

  test("读取锁定模型的合法档位与默认值，仍强制 I2V、16:9 和单条", async () => {
    const video = useModelsStore.getState().byKind.video!;
    useModelsStore.setState({
      byKind: {
        ...useModelsStore.getState().byKind,
        video: {
          ...video,
          models: [
            ...video.models,
            {
              ...video.models[0]!,
              key: "m-1c89decdf8fd",
              label: "锁定 I2V 模型",
              capabilities: {
                ...video.models[0]!.capabilities,
                params: {
                  duration: {
                    type: "enum",
                    label: "时长",
                    open: true,
                    options: [5.5, 6, 12],
                    default: 12,
                  },
                  resolution: {
                    type: "enum",
                    label: "分辨率",
                    open: true,
                    options: ["480P", "720P"],
                    default: "480P",
                  },
                  count: { type: "number", label: "数量", open: false, default: 4 },
                },
              },
            },
          ],
        },
      },
    });
    run = makeRun();
    const mounted = await dom.mount(<Harness />);
    try {
      await dom.flush(120);
      expect(dom.win.document.body.textContent).toContain("锁定 I2V 模型");
      const picker = dom.win.document.querySelector(
        'select[aria-label="I2V 时长"]',
      ) as HTMLSelectElement;
      expect(Array.from(picker.options).map((option) => option.value)).toEqual(["6", "12"]);
      expect(picker.value).toBe("12");
      await dom.click(button("确认首帧与报价"));
      expect(calls.find((call) => call.url.endsWith("/confirm"))?.body.input).toMatchObject({
        op: "i2v",
        aspect_ratio: "16:9",
        count: 1,
        duration: 12,
        resolution: "480P",
      });
    } finally {
      await mounted.unmount();
    }
  });

  test("参数变更后迟到的报价不能覆盖新报价或用于确认", async () => {
    run = makeRun();
    let resolveOld!: (value: unknown) => void;
    const delayed = new Promise((resolve) => {
      resolveOld = resolve;
    });
    const quoteFor = (input: unknown, token: string, credits: number) => ({
      effective_input: input,
      raw_size: "1280×720",
      credits,
      quote_token: token,
      expires_at: "2099-01-01",
      price_fingerprint: "p",
    });
    const post = spyOn(service, "post").mockImplementation(async (url: string, body?: unknown) => {
      const payload = body as { model_id: string; input: { duration: number } };
      if (url.endsWith("/quote"))
        return payload.model_id === "m-1c89decdf8fd" && payload.input.duration === 5
          ? ((await delayed) as never)
          : (quoteFor(payload.input, "new-quote", 30) as never);
      if (url.endsWith("/confirm")) {
        run = {
          ...run!,
          state: "confirmed",
          revision: 2,
          confirmed_version: 1,
          confirmed_input: payload.input,
        };
        return run as never;
      }
      return {} as never;
    });
    spies.push(post);
    const mounted = await dom.mount(<Harness />);
    try {
      await dom.flush(100);
      await select("I2V 时长", "10");
      await dom.flush(100);
      await dom.act(async () => {
        resolveOld(quoteFor({ duration: 5 }, "old-quote", 99));
      });
      expect(dom.win.document.body.textContent).toContain("预计 30 积分");
      expect(dom.win.document.body.textContent).not.toContain("预计 99 积分");
      await dom.click(button("确认首帧与报价"));
      const body = post.mock.calls.find((call) => call[0].endsWith("/confirm"))?.[1];
      expect(body).toMatchObject({ quote_token: "new-quote", input: { duration: 10 } });
    } finally {
      await mounted.unmount();
    }
  });
  test("迟到的历史流程刷新不能覆盖刚创建的新流程", async () => {
    const oldRun = makeRun({ id: "r-old", state: "cancelled" });
    run = oldRun;
    let resolveOld!: (value: unknown) => void;
    const oldRead = new Promise((resolve) => {
      resolveOld = resolve;
    });
    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url.endsWith("/segments")) return { items: [rawSegment()] } as never;
        if (url === "/drama-segments/s1") return rawSegment() as never;
        if (url.endsWith("/frame-runs/r-old")) return (await oldRead) as never;
        return { id: "c1", payload_json: { nodes: [], edges: [] } } as never;
      }),
    );
    let studio!: Studio;
    const mounted = await dom.mount(
      <Harness
        open={false}
        onStudio={(value) => {
          studio = value;
        }}
      />,
    );
    try {
      await dom.flush(30);
      let pending!: ReturnType<Studio["frameRunActions"]["refresh"]>;
      await dom.act(async () => {
        pending = studio.frameRunActions.refresh("s1", "r-old");
      });
      await dom.act(async () => {
        await studio.frameRunActions.create("s1", {
          requestKey: "new-round",
          segmentRevision: 2,
          frameSource: "existing",
          frameAssetId: "f1",
        });
      });
      await dom.act(async () => {
        resolveOld(oldRun);
        await pending;
      });
      expect(studio.selected?.frameRun?.id).toBe("r1");
    } finally {
      await mounted.unmount();
    }
  });

  test.each([
    ["queued", "running", false],
    ["finalizing", "running", false],
    ["succeeded", "done", false],
    ["queued", "done", true],
  ] as const)(
    "a %s video task updates an adopted clip to %s before the delayed list",
    async (taskStatus, status, videoResolved) => {
      const oldSegment = completedSegment(videoResolved);
      let finishList!: () => void;
      const delayedList = new Promise<void>((resolve) => {
        finishList = resolve;
      });
      let listReads = 0;
      const get = service.get;
      spies.push(
        spyOn(service, "get").mockImplementation(async (url, ...args) => {
          if (!url.endsWith("/segments")) return get(url, ...args);
          if (++listReads > 1) await delayedList;
          return { items: [oldSegment] } as never;
        }),
        spyOn(service, "post").mockResolvedValue({
          run: pendingRun,
          task: { ...task, status: taskStatus },
        }),
      );
      const schedule = spyOn(globalThis, "setInterval");
      spies.push(schedule);
      let studio!: Studio;
      const mounted = await dom.mount(
        <Harness
          open={false}
          onStudio={(value) => {
            studio = value;
          }}
        />,
      );
      try {
        await waitFor(() => studio.selected?.status === "done");
        await dom.act(async () => {
          await studio.frameRunActions.submit("s1", confirmedRun.id, {
            revision: confirmedRun.revision,
            confirmedVersion: confirmedRun.confirmed_version,
          });
        });
        expect(studio.selected).toMatchObject({
          status,
          frameRun: { state: "video_pending", revision: 6 },
        });
        await dom.act(async () => {
          finishList();
        });
        expect(studio.selected).toMatchObject({
          status,
          frameRun: { state: "video_pending", revision: 6 },
        });

        const poll = schedule.mock.calls.find(([, interval]) => interval === 8000)?.[0];
        expect(Boolean(poll)).toBe(status === "running");
        if (poll) {
          const readsBeforePoll = listReads;
          await dom.act(async () => {
            poll();
          });
          expect(listReads).toBe(readsBeforePoll + 1);
        }
      } finally {
        finishList();
        await mounted.unmount();
      }
    },
  );

  test.each([
    ["an older confirmed run", confirmedRun, false, "running"],
    ["no run", null, false, "running"],
    ["an already completed take", confirmedRun, true, "done"],
  ] as const)(
    "refresh keeps %s consistent with the latest video run",
    async (_name, oldRun, resolved, status) => {
      const oldSegment = {
        ...completedSegment(resolved),
        frame_run: oldRun,
      };
      const get = service.get;
      spies.push(
        spyOn(service, "get").mockImplementation(async (url, ...args) => {
          if (url.endsWith("/segments")) return { items: [oldSegment] } as never;
          if (url === "/drama-segments/s1") return oldSegment as never;
          if (url.endsWith("/frame-runs/r1")) return pendingRun as never;
          return get(url, ...args);
        }),
      );
      let studio!: Studio;
      const mounted = await dom.mount(
        <Harness
          open={false}
          onStudio={(value) => {
            studio = value;
          }}
        />,
      );
      try {
        await waitFor(() => studio.selected?.status === "done");
        await dom.act(async () => {
          await studio.frameRunActions.refresh("s1", pendingRun.id);
        });
        expect(studio.selected).toMatchObject({
          status,
          frameRun: { state: "video_pending", revision: 6 },
        });
      } finally {
        await mounted.unmount();
      }
    },
  );

  test("画布返回链接自动选中片段并打开指定历史流程，广播、焦点与可见性同步最新首帧", async () => {
    dom.win.history.replaceState(
      null,
      "",
      "/drama/d1?area=studio&ep=1&segment=s2&frame_run=r-history",
    );
    run = makeRun({ id: "r-history", segment_id: "s2" });
    const originalChannel = globalThis.BroadcastChannel;
    const channels: Array<{ onmessage: ((event: MessageEvent) => void) | null; closed: boolean }> =
      [];
    globalThis.BroadcastChannel = class {
      onmessage: ((event: MessageEvent) => void) | null = null;
      closed = false;
      constructor() {
        channels.push(this);
      }
      close() {
        this.closed = true;
      }
    } as unknown as typeof BroadcastChannel;
    const get = spyOn(service, "get").mockImplementation(async (url: string) => {
      if (url.endsWith("/segments"))
        return {
          items: [
            { ...rawSegment(), frame_run: null },
            { ...rawSegment(), id: "s2", seq: 1 },
          ],
        } as never;
      if (url === "/drama-segments/s2")
        return { ...rawSegment(), id: "s2", frame_run: { ...run!, id: "active-run" } } as never;
      if (url.includes("/frame-runs/")) return run as never;
      return { id: "c1", payload_json: { nodes: [], edges: [] } } as never;
    });
    spies.push(get);
    let studio!: Studio;
    const mounted = await dom.mount(
      <Harness
        open={false}
        onStudio={(value) => {
          studio = value;
        }}
      />,
    );
    const runReads = () => get.mock.calls.filter((call) => call[0].includes("/frame-runs/")).length;
    try {
      await dom.flush(30);
      expect(studio.selected?.id).toBe("s2");
      expect(studio.frameRunDrawer?.openSegmentId).toBe("s2");
      expect(studio.selected?.frameRun?.id).toBe("r-history");
      const initialReads = runReads();
      run = { ...run!, revision: 2, frame_asset_id: "canvas-new" };
      await dom.act(async () => {
        channels[0]?.onmessage?.({
          data: { segmentId: "s2", frameRunId: "r-history" },
        } as MessageEvent);
      });
      expect(studio.selected?.frameRun?.frameAssetId).toBe("canvas-new");
      await dom.act(async () => {
        dom.win.dispatchEvent(new dom.win.Event("focus"));
        dom.win.document.dispatchEvent(new dom.win.Event("visibilitychange"));
      });
      expect(runReads()).toBeGreaterThan(initialReads + 1);
      await dom.act(async () => {
        studio.frameRunDrawer?.close();
      });
      expect(channels[0]?.closed).toBe(true);
      const closedReads = runReads();
      await dom.act(async () => {
        dom.win.dispatchEvent(new dom.win.Event("focus"));
      });
      expect(runReads()).toBe(closedReads);
    } finally {
      await mounted.unmount();
      globalThis.BroadcastChannel = originalChannel;
    }
  });
  test("上传本地图片后通过已有素材来源创建，服务端裁切结果仍是预览依据", async () => {
    spies.push(
      spyOn(service, "upload").mockResolvedValue({
        id: "local-image",
        kind: "image",
        url: "/upload.png",
        width: 1000,
        height: 1000,
      }),
    );
    const mounted = await dom.mount(<Harness />);
    try {
      await dom.flush(30);
      const input = dom.win.document.querySelector('input[aria-label="上传本地图片"]')!;
      await dom.act(async () => {
        Object.defineProperty(input, "files", {
          configurable: true,
          value: [new dom.win.File(["image"], "local.png", { type: "image/png" })],
        });
        input.dispatchEvent(new dom.win.Event("change", { bubbles: true }));
      });
      await dom.flush(100);
      expect(calls.find((call) => call.url.endsWith("/frame-runs"))?.body).toMatchObject({
        frame_source: "existing",
        frame_asset_id: "local-image",
        segment_revision: 2,
      });
      expect(
        dom.win.document.querySelector('img[alt="实际送入 I2V 的首帧"]')?.getAttribute("src"),
      ).toBe("/actual-16-9.png");
    } finally {
      await mounted.unmount();
    }
  });

  test("生成首帧图使用图片模型，刷新到首帧就绪后可取消并开始新一轮", async () => {
    const mounted = await dom.mount(<Harness />);
    try {
      await dom.flush(30);
      await dom.click(button("生成首帧图"));
      await dom.click(button("开始首帧控镜"));
      expect(calls.find((call) => call.url.endsWith("/frame-runs"))?.body).toMatchObject({
        frame_source: "generate",
        model_id: "img1",
        input: { op: "t2i", aspect_ratio: "16:9", count: 1 },
      });
      expect(dom.win.document.body.textContent).toContain("生成首帧中");
      run = { ...run!, state: "frame_ready", revision: 2 };
      await dom.click(button("刷新首帧状态"));
      await dom.flush(100);
      expect(dom.win.document.body.textContent).toContain("首帧就绪");
      await dom.click(button("取消控镜流程"));
      expect(calls.find((call) => call.url.endsWith("/cancel"))?.body).toEqual({ revision: 2 });
      expect(dom.win.document.body.textContent).toContain("已取消");
      await dom.click(button("新建一轮首帧控镜"));
      expect(button("开始首帧控镜")).toBeDefined();
    } finally {
      await mounted.unmount();
    }
  });

  test("画布来源生成携带票据与定位参数的链接，回传后刷新使用新帧", async () => {
    const mounted = await dom.mount(<Harness />);
    try {
      await dom.flush(30);
      await dom.click(button("打开画布构图"));
      await dom.flush(30);
      await dom.click(button("开始首帧控镜"));
      expect(calls.find((call) => call.url.endsWith("/frame-runs"))?.body).toMatchObject({
        frame_source: "canvas",
        origin_canvas_id: "c1",
      });
      const link = dom.win.document.querySelector('a[href^="/canvas/"]')!;
      const url = new URL(link.getAttribute("href")!, "http://localhost");
      expect(url.pathname).toBe("/canvas/c1");
      expect(Object.fromEntries(url.searchParams)).toEqual({
        drama_id: "d1",
        episode_no: "1",
        segment_id: "s1",
        frame_run_id: "r1",
        frame_run_revision: "1",
        canvas_ticket: "t&1",
      });
      run = {
        ...run!,
        state: "frame_ready",
        revision: 2,
        frame_asset_id: "from-canvas",
        frame_sha256: "c".repeat(64),
      };
      await dom.click(button("刷新首帧状态"));
      await dom.flush(100);
      expect(dom.win.document.body.textContent).toContain("cccccccccccc");
    } finally {
      await mounted.unmount();
    }
  });

  test("已确认后修改参数禁止提交，替换首帧作废旧确认并展示新指纹", async () => {
    const mounted = await dom.mount(<Harness />);
    try {
      await dom.flush(30);
      await select("已有图片", "f1");
      await dom.click(button("开始首帧控镜"));
      await dom.flush(100);
      await dom.click(button("确认首帧与报价"));
      expect(button("提交首帧 I2V 生成")?.hasAttribute("disabled")).toBe(false);
      await select("I2V 时长", "10");
      expect(button("提交首帧 I2V 生成")?.hasAttribute("disabled")).toBe(true);
      await dom.click(button("替换首帧"));
      expect(dom.win.document.body.textContent).toContain("替换首帧将使此前确认失效");
      await select("替换图片", "f1");
      await dom.click(button("使用替换图片"));
      await dom.flush(100);
      expect(calls.find((call) => call.url.endsWith("/frame"))?.body).toEqual({
        revision: 2,
        frame_asset_id: "f1",
        origin_canvas_id: "c1",
        canvas_ticket: "t&1",
      });
      expect(dom.win.document.body.textContent).toContain("bbbbbbbbbbbb");
      expect(button("提交首帧 I2V 生成")?.hasAttribute("disabled")).toBe(true);
    } finally {
      await mounted.unmount();
    }
  });

  test("91008 保留选择与幂等键可重试，91002 要求刷新报价后再确认，409 读取最新状态", async () => {
    failure = { suffix: "/frame-runs", error: new ApiError("invalid", 91008, 400) };
    const mounted = await dom.mount(<Harness />);
    try {
      await dom.flush(30);
      await select("已有图片", "f1");
      await dom.click(button("开始首帧控镜"));
      expect(dom.win.document.body.textContent).toContain("首帧素材无效或尚未就绪");
      await dom.click(button("开始首帧控镜"));
      await dom.flush(100);
      const creates = calls.filter((call) => call.url.endsWith("/frame-runs"));
      expect(creates[0]!.body.request_key).toBe(creates[1]!.body.request_key);
      failure = { suffix: "/confirm", error: new ApiError("expired", 91002, 409) };
      await dom.click(button("确认首帧与报价"));
      expect(dom.win.document.body.textContent).toContain("报价已过期，请刷新报价");
      expect(button("确认首帧与报价")?.hasAttribute("disabled")).toBe(true);
      await dom.click(button("刷新报价"));
      await dom.click(button("确认首帧与报价"));
      expect(dom.win.document.body.textContent).toContain("已确认 v1");
      failure = { suffix: "/frame", error: new ApiError("conflict", 91005, 409) };
      await dom.click(button("替换首帧"));
      await select("替换图片", "f1");
      await dom.click(button("使用替换图片"));
      expect(dom.win.document.body.textContent).toContain("首帧控镜状态已变更或存在进行中流程");
    } finally {
      await mounted.unmount();
    }
  });

  test("submitting 可重试匹配的确认版本，提交快照经 gateway 入任务库", async () => {
    run = makeRun({
      state: "submitting",
      revision: 4,
      confirmed_version: 2,
      confirmed_input: {
        op: "i2v",
        aspect_ratio: "16:9",
        count: 1,
        images: ["cropped"],
        prompt: "推镜",
        duration: 5,
        resolution: "720P",
      },
    });
    const mounted = await dom.mount(<Harness />);
    try {
      await dom.flush(30);
      expect(button("提交首帧 I2V 生成")?.hasAttribute("disabled")).toBe(false);
      await dom.click(button("提交首帧 I2V 生成"));
      expect(calls.find((call) => call.url.endsWith("/submit"))?.body).toEqual({
        revision: 4,
        confirmed_version: 2,
      });
      expect(useTasksStore.getState().tasks.t1?.status).toBe("queued");
    } finally {
      await mounted.unmount();
    }
  });

  test("失败流程展示原因且允许显式新建一轮", async () => {
    run = makeRun({ state: "failed", error_message: "首帧生成失败" });
    const mounted = await dom.mount(<Harness />);
    try {
      await dom.flush(30);
      expect(dom.win.document.body.textContent).toContain("首帧生成失败");
      expect(button("取消控镜流程")).toBeUndefined();
      await dom.click(button("新建一轮首帧控镜"));
      expect(button("开始首帧控镜")).toBeDefined();
    } finally {
      await mounted.unmount();
    }
  });
  test("普通 Ref2V 的立即生成仍直接报价并提交，不创建首帧流程", async () => {
    const mounted = await dom.mount(<Harness open={false} />);
    try {
      await dom.flush(100);
      await dom.click(button("立即生成·10"));
      await dom.flush(30);
      expect(calls.some((c) => c.url.endsWith("/generate"))).toBe(true);
      expect(calls.some((c) => c.url.endsWith("/frame-runs"))).toBe(false);
    } finally {
      await mounted.unmount();
    }
  });

  test("真实十进制素材 ID 与后端标准化数字 images[] 一致启用提交，报价过期刷新后仍须重新确认", async () => {
    const decimalAsset = { ...imageAsset, assetId: "901" };
    const get = spyOn(service, "get").mockImplementation(async (url: string) => {
      if (url.endsWith("/segments")) return { items: [rawSegment()] } as never;
      if (url === "/drama-segments/s1") return rawSegment() as never;
      if (url.includes("/frame-runs/")) return run as never;
      if (url === "/assets/902")
        return {
          id: 902,
          kind: "image",
          url: "/actual-902.png",
          width: 1600,
          height: 900,
        } as never;
      return { id: "c1", payload_json: { nodes: [], edges: [] } } as never;
    });
    const post = spyOn(service, "post").mockImplementation(async (url: string, body?: unknown) => {
      const payload = (body ?? {}) as Record<string, unknown>;
      calls.push({ url, body: payload });
      if (failure && url.endsWith(failure.suffix)) {
        const error = failure.error;
        failure = null;
        throw error;
      }
      if (url.endsWith("/quote")) {
        return {
          effective_input: payload.input,
          raw_size: "1280×720",
          credits: 10,
          quote_token: `q-${calls.length}`,
          expires_at: "2099-01-01",
          price_fingerprint: "p",
        } as never;
      }
      if (url.endsWith("/frame-runs")) {
        run = makeRun({
          request_key: payload.request_key as string,
          frame_asset_id: 902,
          frame_sha256: "d".repeat(64),
        });
        return run as never;
      }
      if (url.endsWith("/confirm")) {
        const inputObj = payload.input as Record<string, unknown>;
        run = {
          ...run!,
          state: "confirmed",
          revision: run!.revision + 1,
          confirmed_version: run!.confirmed_version + 1,
          confirmed_input: { ...inputObj, images: [902] },
          model_key: "m-1c89decdf8fd",
        };
        return run as never;
      }
      if (url.endsWith("/submit")) {
        run = { ...run!, state: "video_pending", revision: run!.revision + 1 };
        return { run, task } as never;
      }
      return {} as never;
    });
    spies.push(get, post);
    const StudioRealHarness = () => {
      const studio = useStudioState(drama, 1, async () => {});
      return (
        <StudioContext.Provider value={studio}>
          <FrameRunDrawer
            open
            onOpenChange={() => {}}
            segment={studio.selected}
            dramaId="d1"
            episodeNo={1}
            canvasId="c1"
            assets={[decimalAsset]}
            actions={studio.frameRunActions}
          />
        </StudioContext.Provider>
      );
    };
    const mounted = await dom.mount(<StudioRealHarness />);
    try {
      await dom.flush(30);
      await select("已有图片", "901");
      await dom.click(button("开始首帧控镜"));
      await dom.flush(100);
      expect(get.mock.calls.some((call) => call[0] === "/assets/902")).toBe(true);
      expect(calls.find((c) => c.url.endsWith("/quote"))?.body).toMatchObject({
        input: { images: [902] },
      });
      await dom.click(button("确认首帧与报价"));
      expect(button("提交首帧 I2V 生成")?.hasAttribute("disabled")).toBe(false);
      failure = { suffix: "/submit", error: new ApiError("expired", 91002, 409) };
      await dom.click(button("提交首帧 I2V 生成"));
      expect(button("提交首帧 I2V 生成")?.hasAttribute("disabled")).toBe(true);
      await dom.click(button("刷新报价"));
      await dom.flush(30);
      // 刷新报价仅解除重新确认限制，在真正重新 confirm 前不得直接提交已过期确认版本
      expect(button("确认首帧与报价")?.hasAttribute("disabled")).toBe(false);
      expect(button("提交首帧 I2V 生成")?.hasAttribute("disabled")).toBe(true);
      await dom.click(button("确认首帧与报价"));
      expect(button("提交首帧 I2V 生成")?.hasAttribute("disabled")).toBe(false);
    } finally {
      await mounted.unmount();
    }
  });

  test("已确认报价在刷新获取新报价后仍按原确认报价期限到期并锁定提交直至重新确认", async () => {
    const decimalAsset = {
      ...imageAsset,
      assetId: "901",
      episodes: [],
    };
    let quoteCall = 0;
    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url.endsWith("/segments")) return { items: [rawSegment()] } as never;
        if (url === "/drama-segments/s1") return rawSegment() as never;
        if (url.includes("/frame-runs/")) return run as never;
        if (url === "/assets/902")
          return {
            id: 902,
            kind: "image",
            url: "/actual-902.png",
            width: 1600,
            height: 900,
          } as never;
        return { id: "c1", payload_json: { nodes: [], edges: [] } } as never;
      }),
      spyOn(service, "post").mockImplementation(async (url: string, body?: unknown) => {
        const payload = (body ?? {}) as Record<string, unknown>;
        if (url.endsWith("/quote")) {
          quoteCall++;
          return {
            effective_input: payload.input,
            raw_size: "1280×720",
            credits: 10,
            quote_token: `q-${quoteCall}`,
            expires_at:
              quoteCall === 1 ? new Date(Date.now() + 70).toISOString() : "2099-01-01T00:00:00Z",
            price_fingerprint: "p",
          } as never;
        }
        if (url.endsWith("/frame-runs")) {
          run = makeRun({
            request_key: payload.request_key as string,
            frame_asset_id: 902,
            frame_sha256: "d".repeat(64),
          });
          return run as never;
        }
        if (url.endsWith("/confirm")) {
          const inputObj = payload.input as Record<string, unknown>;
          run = {
            ...run!,
            state: "confirmed",
            revision: run!.revision + 1,
            confirmed_version: run!.confirmed_version + 1,
            confirmed_input: { ...inputObj, images: [902] },
            model_key: "m-1c89decdf8fd",
          };
          return run as never;
        }
        return {} as never;
      }),
    );
    const StudioRealHarness = () => {
      const studio = useStudioState(drama, 1, async () => {});
      return (
        <StudioContext.Provider value={studio}>
          <FrameRunDrawer
            open
            onOpenChange={() => {}}
            segment={studio.selected}
            dramaId="d1"
            episodeNo={1}
            canvasId="c1"
            assets={[decimalAsset]}
            actions={studio.frameRunActions}
          />
        </StudioContext.Provider>
      );
    };
    const mounted = await dom.mount(<StudioRealHarness />);
    try {
      await dom.flush(30);
      await select("已有图片", "901");
      await dom.click(button("开始首帧控镜"));
      await dom.flush(100);
      await dom.click(button("确认首帧与报价"));
      expect(button("提交首帧 I2V 生成")?.hasAttribute("disabled")).toBe(false);
      await dom.click(button("刷新报价"));
      await dom.flush(20);
      // 刷新拿到新报价 B 后，等到原确认报价 A 期限过去，提交仍须被锁定直到重新确认
      await dom.flush(110);
      expect(dom.win.document.body.textContent).toContain("报价已过期，请刷新报价");
      expect(button("确认首帧与报价")?.hasAttribute("disabled")).toBe(false);
      expect(button("提交首帧 I2V 生成")?.hasAttribute("disabled")).toBe(true);
      await dom.click(button("确认首帧与报价"));
      expect(button("提交首帧 I2V 生成")?.hasAttribute("disabled")).toBe(false);
    } finally {
      await mounted.unmount();
    }
  });

  test("迟到的整集列表拉取与 WS 旧快照不能覆盖本地新建或确认的首帧流程", async () => {
    let listAttempt = 0;
    let resolveSlowList!: (value: unknown) => void;
    let markSlowStarted!: () => void;
    const slowStarted = new Promise<void>((resolve) => {
      markSlowStarted = resolve;
    });
    const slowList = new Promise((resolve) => {
      resolveSlowList = resolve;
    });
    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url.endsWith("/segments")) {
          listAttempt++;
          if (listAttempt === 1) return { items: [rawSegment()] } as never;
          markSlowStarted();
          return (await slowList) as never;
        }
        return { id: "c1", payload_json: { nodes: [], edges: [] } } as never;
      }),
    );
    let studio!: Studio;
    const mounted = await dom.mount(
      <Harness
        open={false}
        onStudio={(value) => {
          studio = value;
        }}
      />,
    );
    try {
      await dom.flush(30);
      const slowReload = reloadSegments("d1", 1);
      await slowStarted;
      await dom.act(async () => {
        await studio.frameRunActions.create("s1", {
          requestKey: "race-list",
          segmentRevision: 2,
          frameSource: "existing",
          frameAssetId: "901",
        });
      });
      await dom.act(async () => {
        resolveSlowList({ items: [{ ...rawSegment(), frame_run: null }] });
        await slowReload;
      });
      expect(studio.selected?.frameRun?.id).toBe("r1");
      // 终态流程也不能被带有早期 updated_at 的 WS 空快照抹除
      await dom.act(async () => {
        const cancelled = makeRun({
          id: "r-cancelled",
          state: "cancelled",
          revision: 3,
          created_at: "2026-11-16T10:00:00Z",
          updated_at: "2026-11-16T10:05:00Z",
        });
        run = cancelled;
        handleDramaSegmentEvent({
          drama_id: "d1",
          episode_no: 1,
          segment: {
            ...rawSegment(),
            updated_at: "2026-11-16T10:05:00Z",
            frame_run: cancelled,
          },
        });
        handleDramaSegmentEvent({
          drama_id: "d1",
          episode_no: 1,
          segment: {
            ...rawSegment(),
            updated_at: "2026-11-16T10:01:00Z",
            frame_run: null,
          },
        });
      });
      expect(studio.selected?.frameRun?.id).toBe("r-cancelled");
    } finally {
      await mounted.unmount();
    }
  });

  test("已有图片 → 服务端实际首帧预览 → 报价确认 → 提交已确认版本", async () => {
    const mounted = await dom.mount(<Harness />);
    try {
      await dom.flush(30);
      await select("已有图片", "f1");
      await dom.click(button("开始首帧控镜"));
      await dom.flush(100);
      expect(dom.win.document.body.textContent).toContain("16:9 首帧预览 · 实际送入模型");
      expect(
        dom.win.document.querySelector('img[alt="实际送入 I2V 的首帧"]')?.getAttribute("src"),
      ).toBe("/actual-16-9.png");
      expect(dom.win.document.body.textContent).toContain("1600×900");
      expect(dom.win.document.body.textContent).toContain("1280×720");
      expect(button("提交首帧 I2V 生成")?.hasAttribute("disabled")).toBe(true);
      await dom.click(button("确认首帧与报价"));
      await dom.flush(30);
      expect(dom.win.document.body.textContent).toContain("已确认 v1");
      const confirmed = calls.find((c) => c.url.endsWith("/confirm"))!.body;
      expect(confirmed).toMatchObject({
        frame_asset_id: "cropped",
        frame_sha256: "a".repeat(64),
        model_id: "m-1c89decdf8fd",
        quote_token: "quote-1",
        input: {
          op: "i2v",
          aspect_ratio: "16:9",
          count: 1,
          images: ["cropped"],
          duration: 5,
          resolution: "720P",
        },
      });
      await dom.click(button("提交首帧 I2V 生成"));
      await dom.flush(30);
      expect(calls.find((c) => c.url.endsWith("/submit"))?.body).toEqual({
        revision: 2,
        confirmed_version: 1,
      });
      expect(dom.win.document.body.textContent).toContain("生成视频中");
    } finally {
      await mounted.unmount();
    }
  });
});

describe("候选导入与驳回当前采用项", () => {
  test("Studio 拦截直接采用已驳回候选并给出具体重置质检提示", async () => {
    const rejected = {
      task_id: "rejected",
      status: "succeeded",
      asset: { id: "a1", kind: "video", url: "/1.mp4" },
      qc: "rejected",
      error_message: "",
      created_at: "",
    };
    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) =>
        url.endsWith("/segments")
          ? ({ items: [{ ...rawSegment(), takes: [rejected] }] } as never)
          : ({ id: "c1", payload_json: { nodes: [], edges: [] } } as never),
      ),
    );
    const errorToast = spyOn(toast, "error").mockImplementation(() => "toast");
    const patch = spyOn(service, "patch");
    spies.push(errorToast, patch);
    let studio!: Studio;
    const mounted = await dom.mount(
      <Harness
        open={false}
        onStudio={(value) => {
          studio = value;
        }}
      />,
    );
    try {
      await dom.flush(30);
      await dom.act(async () => {
        await studio.actions.choose(studio.selected!, "rejected");
      });
      expect(errorToast).toHaveBeenCalledWith("该候选已被驳回，请先重置质检后再采用");
      expect(patch).not.toHaveBeenCalled();
    } finally {
      await mounted.unmount();
    }
  });

  test("手动查找未链接视频、单选采用与导入错误保留选择供重试", async () => {
    spies.push(
      spyOn(taskApi, "getGenerationTasksByIds").mockImplementation(async (ids) =>
        ids.length
          ? ([
              {
                ...task,
                id: "manual-video",
                canvas_id: "c1",
                status: "succeeded",
                outputs: [{ asset_id: "a1", url: "/video.mp4", media_type: "video" }],
              },
            ] as never)
          : [],
      ),
    );
    const segment = mapSegment(rawSegment());
    let errorCode: number | null = 91007;
    const imported: unknown[] = [];
    const mounted = await dom.mount(
      <ImportTakesDialog
        open
        defaultCanvasId="c1"
        onOpenChange={() => {}}
        segment={segment}
        onImport={async (input) => {
          imported.push(input);
          if (errorCode) throw new ApiError("", errorCode, errorCode === 67011 ? 400 : 409);
        }}
      />,
    );
    try {
      await dom.flush(30);
      const manualToggle = Array.from(dom.win.document.querySelectorAll("label"))
        .find((label) => label.textContent?.includes("手动输入 task_id"))
        ?.querySelector("input");
      await dom.click(manualToggle);
      await dom.type(
        dom.win.document.querySelector('input[aria-label="手动 task_id"]'),
        "manual-video",
      );
      await dom.click(button("查找任务"));
      const adopt = dom.win.document.querySelector('input[aria-label="导入后立即采用"]');
      expect(adopt?.hasAttribute("disabled")).toBe(false);
      await dom.click(adopt);
      await dom.click(button("导入候选"));
      expect(dom.win.document.body.textContent).toContain("该候选已被驳回，无法直接采用");
      errorCode = 91005;
      await dom.click(button("导入候选"));
      expect(dom.win.document.body.textContent).toContain("首帧控镜状态已变更或存在进行中流程");
      errorCode = 67011;
      await dom.click(button("导入候选"));
      expect(dom.win.document.body.textContent).toContain("只能采用本片段已成功的结果");
      errorCode = null;
      await dom.click(button("导入候选"));
      expect(imported).toHaveLength(4);
      expect(imported[3]).toEqual({
        sourceCanvasId: "c1",
        taskIds: ["manual-video"],
        adopt: true,
        resetEditOnTaskChange: true,
      });
    } finally {
      await mounted.unmount();
    }
  });
  test("轮播展示来源与质检；驳回当前采用先弹窗，已驳回版本不会滑动即采用", async () => {
    const originalObserver = globalThis.ResizeObserver;
    globalThis.ResizeObserver = class {
      callback: ResizeObserverCallback;
      constructor(callback: ResizeObserverCallback) {
        this.callback = callback;
      }
      observe() {
        this.callback(
          [{ contentRect: { width: 900, height: 500 } }] as ResizeObserverEntry[],
          this as unknown as ResizeObserver,
        );
      }
      unobserve() {}
      disconnect() {}
    } as unknown as typeof ResizeObserver;
    const segment = mapSegment(rawSegment());
    const asset = {
      id: "a1",
      kind: "video",
      url: "/1.mp4",
      mimeType: "video/mp4",
      byteSize: 1,
      width: 1280,
      height: 720,
      durationMs: 5000,
      fileName: null,
    } as const;
    segment.takes = [
      { taskId: "t1", origin: "native", qc: "accepted" },
      { taskId: "t2", origin: "canvas_import", qc: "rejected" },
      { taskId: "t3", origin: "i2v", qc: "unreviewed" },
    ].map((item) => ({
      ...item,
      asset,
      status: "succeeded",
      errorMessage: "",
      createdAt: "",
    })) as typeof segment.takes;
    segment.chosenTaskId = "t1";
    const patched: unknown[] = [],
      chosen: string[] = [],
      cleared: string[] = [],
      opened: string[] = [];
    const studio = {
      selected: segment,
      episode: { segmentVersion: 3 },
      importTakesModal: { open: (id: string) => opened.push(id) },
      actions: {
        clearChosen: async () => {
          cleared.push(segment.id);
        },
        patchTakeQc: async (_: unknown, tid: string, input: unknown) => {
          patched.push({ tid, input });
          return segment;
        },
      },
    } as unknown as Studio;
    const mounted = await dom.mount(
      <StudioContext.Provider value={studio}>
        <TakeCarousel
          takes={segment.takes}
          chosenTaskId="t1"
          aspect={16 / 9}
          onChoose={(id) => chosen.push(id)}
        />
      </StudioContext.Provider>,
    );
    try {
      const text = dom.win.document.body.textContent;
      for (const label of ["直接生成", "画布导入", "首帧 I2V", "已通过", "已驳回", "待质检"])
        expect(text).toContain(label);
      await dom.click(button("导入画布视频"));
      expect(opened).toEqual(["s1"]);
      await dom.click(button("清空采用"));
      expect(cleared).toEqual(["s1"]);
      await dom.click(dom.win.document.querySelector('button[aria-label="第 2 版"]'));
      expect(chosen).toEqual([]);
      await dom.click(dom.win.document.querySelector('button[aria-label="驳回第 1 版"]'));
      expect(dom.win.document.body.textContent).toContain("驳回当前采用候选");
      expect(patched).toEqual([]);
      await dom.click(dom.win.document.querySelector('input[value="clear"]'));
      await dom.click(button("确认驳回"));
      expect(patched[0]).toEqual({
        tid: "t1",
        input: { qc: "rejected", expectedSegmentVersion: 3, clearChosen: true },
      });
      await dom.click(dom.win.document.querySelector('button[aria-label="撤销质检第 2 版"]'));
      expect(patched[1]).toEqual({
        tid: "t2",
        input: { qc: "unreviewed", expectedSegmentVersion: 3 },
      });
    } finally {
      await mounted.unmount();
      globalThis.ResizeObserver = originalObserver;
    }
  });
  test("驳回已采用候选必须显式选择替换或清空，不能默认选中另一候选", async () => {
    const segment = mapSegment(rawSegment());
    const asset = {
      id: "a1",
      kind: "video",
      url: "/1.mp4",
      mimeType: "video/mp4",
      byteSize: 10,
      width: 1280,
      height: 720,
      durationMs: 5000,
      fileName: null,
    } as const;
    segment.takes = ["t1", "t2", "t3"].map((taskId) => ({
      taskId,
      asset,
      status: "succeeded",
      errorMessage: "",
      createdAt: "",
      qc: taskId === "t3" ? "rejected" : "unreviewed",
    }));
    segment.chosenTaskId = "t1";
    const rejected: unknown[] = [];
    const mounted = await dom.mount(
      <RejectTakeDialog
        open
        onOpenChange={() => {}}
        segment={segment}
        taskId="t1"
        expectedSegmentVersion={3}
        onReject={async (input) => {
          rejected.push(input);
        }}
      />,
    );
    try {
      expect(button("确认驳回")?.hasAttribute("disabled")).toBe(true);
      await dom.click(dom.win.document.querySelector('input[value="replace"]'));
      expect(button("确认驳回")?.hasAttribute("disabled")).toBe(true);
      const options = dom.win.document.querySelectorAll('select[aria-label="替换候选"] option');
      expect(Array.from(options).some((o) => o.getAttribute("value") === "t3")).toBe(false);
      await select("替换候选", "t2");
      await dom.click(dom.win.document.querySelector('input[type="checkbox"]'));
      await dom.click(button("确认驳回"));
      expect(rejected[0]).toMatchObject({
        qc: "rejected",
        expectedSegmentVersion: 3,
        replacementTaskId: "t2",
        resetEditOnTaskChange: false,
      });
      await dom.click(dom.win.document.querySelector('input[value="clear"]'));
      await dom.click(button("确认驳回"));
      expect(rejected[1]).toMatchObject({ qc: "rejected", clearChosen: true });
      expect(rejected[1]).not.toHaveProperty("replacementTaskId");
    } finally {
      await mounted.unmount();
    }
  });

  test("读取画布当前与历史任务并去重，仅导入成功视频；已驳回候选禁止导入即采用", async () => {
    const segment = mapSegment(rawSegment());
    segment.takes = [
      {
        taskId: "t1",
        status: "succeeded",
        qc: "rejected",
        asset: null,
        errorMessage: "",
        createdAt: "",
      },
    ];
    spies.push(
      spyOn(taskApi, "getGenerationTasksByIds").mockImplementation(async (ids) => {
        expect(ids).toEqual(["t1", "t2", "image", "failed", "empty"]);
        return [
          {
            ...task,
            id: "t1",
            canvas_id: "c2",
            status: "succeeded",
            outputs: [{ asset_id: "a1", url: "/1.mp4", media_type: "video" }],
          },
          {
            ...task,
            id: "t2",
            canvas_id: "c2",
            status: "succeeded",
            outputs: [{ asset_id: "a2", url: "/2.mp4", media_type: "video" }],
          },
          { ...task, id: "image", canvas_id: "c2", kind: "image", status: "succeeded" },
          { ...task, id: "failed", canvas_id: "c2", status: "failed" },
          { ...task, id: "empty", canvas_id: "c2", status: "succeeded", outputs: [] },
        ] as never;
      }),
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url === "/canvas")
          return { list: [{ id: "c2", title: "来源画布", revision: 1 }], total: 1 } as never;
        if (url === "/canvas/c2")
          return {
            id: "c2",
            payload_json: {
              nodes: [
                {
                  type: "canvas",
                  data: {
                    taskId: "t1",
                    outputs: [
                      { taskId: "t1" },
                      { taskId: "t2" },
                      { taskId: "image" },
                      { taskId: "failed" },
                      { taskId: "empty" },
                    ],
                  },
                },
              ],
              edges: [],
            },
          } as never;
        return {} as never;
      }),
    );
    const imported: unknown[] = [];
    const mounted = await dom.mount(
      <ImportTakesDialog
        open
        onOpenChange={() => {}}
        segment={segment}
        onImport={async (input) => {
          imported.push(input);
        }}
      />,
    );
    try {
      await dom.flush(30);
      await select("来源画布", "c2");
      await dom.flush(30);
      expect(dom.win.document.querySelectorAll('input[name="import-task"]')).toHaveLength(2);
      await dom.click(dom.win.document.querySelector('input[value="t1"]'));
      const adopt = dom.win.document.querySelector('input[aria-label="导入后立即采用"]');
      expect(adopt?.hasAttribute("disabled")).toBe(true);
      expect(dom.win.document.body.textContent).toContain("该候选已被驳回，无法直接采用");
      await dom.click(button("导入候选"));
      expect(imported[0]).toMatchObject({ sourceCanvasId: "c2", taskIds: ["t1"], adopt: false });
      await dom.click(dom.win.document.querySelector('input[value="t1"]'));
      await dom.click(dom.win.document.querySelector('input[value="t2"]'));
      await dom.click(adopt);
      await dom.click(dom.win.document.querySelector('input[value="t1"]'));
      expect(adopt?.hasAttribute("disabled")).toBe(true);
      await dom.click(button("导入候选"));
      expect(imported[1]).toMatchObject({
        sourceCanvasId: "c2",
        taskIds: ["t2", "t1"],
        adopt: false,
      });
    } finally {
      await mounted.unmount();
    }
  });

  test("画布导入提交编码 task_ref 而非十进制 id，且切换画布后不被旧手动查询锁死", async () => {
    const segment = mapSegment(rawSegment());
    segment.takes = [
      {
        taskId: "encoded-ref-1",
        status: "succeeded",
        qc: "rejected",
        asset: null,
        errorMessage: "",
        createdAt: "",
      },
    ];
    let resolveCanvasALookup!: (value: unknown) => void;
    const delayedLookup = new Promise((resolve) => {
      resolveCanvasALookup = resolve;
    });
    spies.push(
      spyOn(taskApi, "getGenerationTasksByIds").mockImplementation(async (ids) => {
        if (ids.includes("slow-manual")) return (await delayedLookup) as never;
        if (ids.includes("101")) {
          return [
            {
              ...task,
              id: 101,
              task_ref: "encoded-ref-1",
              canvas_id: "c2",
              status: "succeeded",
              outputs: [{ asset_id: 700, url: "/2.mp4", media_type: "video" }],
            },
          ] as never;
        }
        return [
          {
            ...task,
            id: 102,
            task_ref: "encoded-ref-2",
            canvas_id: "c2",
            status: "succeeded",
            outputs: [{ asset_id: 701, url: "/3.mp4", media_type: "video" }],
          },
        ] as never;
      }),
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url === "/canvas")
          return {
            list: [
              { id: "c1", title: "画布 A", revision: 1 },
              { id: "c2", title: "画布 B", revision: 1 },
            ],
            total: 2,
          } as never;
        if (url === "/canvas/c1")
          return { id: "c1", payload_json: { nodes: [], edges: [] } } as never;
        if (url === "/canvas/c2")
          return {
            id: "c2",
            payload_json: { nodes: [{ type: "canvas", data: { taskId: "101" } }], edges: [] },
          } as never;
        return {} as never;
      }),
    );
    const imported: unknown[] = [];
    const mounted = await dom.mount(
      <ImportTakesDialog
        open
        defaultCanvasId="c1"
        onOpenChange={() => {}}
        segment={segment}
        onImport={async (input) => {
          imported.push(input);
        }}
      />,
    );
    try {
      await dom.flush(30);
      const manualToggle = Array.from(dom.win.document.querySelectorAll("label"))
        .find((label) => label.textContent?.includes("手动输入 task_id"))
        ?.querySelector("input");
      await dom.click(manualToggle);
      await dom.type(
        dom.win.document.querySelector('input[aria-label="手动 task_id"]'),
        "slow-manual",
      );
      await dom.click(button("查找任务"));
      await select("来源画布", "c2");
      await dom.flush(30);
      await dom.act(async () => {
        resolveCanvasALookup([]);
      });
      // 用 task_ref（encoded-ref-1）而非十进制 101 匹配段内已驳回历史并提交导入
      const checkbox = dom.win.document.querySelector(
        'input[name="import-task"][value="encoded-ref-1"]',
      ) as HTMLInputElement;
      expect(checkbox.disabled).toBe(false);
      await dom.click(checkbox);
      const adopt = dom.win.document.querySelector('input[aria-label="导入后立即采用"]');
      expect(adopt?.hasAttribute("disabled")).toBe(true);
      await dom.click(button("导入候选"));
      expect(imported[0]).toMatchObject({
        sourceCanvasId: "c2",
        taskIds: ["encoded-ref-1"],
        adopt: false,
      });
    } finally {
      await mounted.unmount();
    }
  });
});
