import { Fragment, useEffect, useState } from "react";
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

import { mapDrama } from "@/api/drama";
import type { Drama, Segment } from "@/api/drama/type";
import type { ModelInfo } from "@/api/model/type";
import { GenerateControls } from "@/pages/drama/studio/generate-controls";
import { StudioContext, useStudioState, type Studio } from "@/pages/drama/studio/use-studio";
import { DramaWorkspaceContext } from "@/pages/drama/workspace/context";
import {
  mergeEpisodeListWithMonotonicVersions,
  useDramaWorkspace,
  type DramaWorkspace,
} from "@/pages/drama/workspace/use-workspace";
import { useCreditsStore } from "@/store/credits";
import { useDialogStore } from "@/store/dialog";
import { useDramaSegmentsStore } from "@/store/drama-segments";
import { useStudioPrefsStore } from "@/store/drama-studio";
import { useModelsStore } from "@/store/models";
import { installDom, type DomEnv } from "@/tests/helpers/dom";
import { ApiError } from "@/utils/requests/request";
import service from "@/utils/requests/service";

let dom: DomEnv;
const spies: Array<{ mockRestore: () => void }> = [];

beforeAll(async () => {
  dom = await installDom();
});

afterEach(() => {
  while (spies.length > 0) {
    spies.pop()?.mockRestore();
  }
  // 清理 choice-dialog 等直接挂到 body 的遗留节点
  dom.win.document.body.innerHTML = "";
  localStorage.clear();
  useDialogStore.setState({ dialogs: [] });
  useStudioPrefsStore.setState({ byDrama: {} });
  useDramaSegmentsStore.setState({ byEpisode: {}, watching: {} });
  useCreditsStore.setState({ credits: { available: 500 } } as never);
});

afterAll(() => {
  dom.restore();
});

const modelInfo: ModelInfo = {
  key: "seedance-2",
  kind: "video",
  label: "Seedance 2.0",
  pricing: { billing: "per_call", unit: 10 },
  capabilities: {
    ops: ["i2v", "t2v"],
    refs: {
      image: { on: true, max: 4, max_mb: 10 },
      video: { on: false, max: 0, max_mb: 0 },
      audio: { on: false, max: 0, max_mb: 0 },
    },
    prompt: { max_length: 1000 },
    prompt_format: "seedance",
    params: {
      duration: { type: "enum", label: "时长", open: true, options: [5, 10], default: 5 },
      resolution: {
        type: "enum",
        label: "分辨率",
        open: true,
        options: ["720P", "1080P"],
        default: "720P",
        spec: true,
      },
      generate_audio: {
        type: "boolean",
        label: "生成音频",
        open: true,
        default: false,
        spec: true,
      },
      count: {
        type: "enum",
        label: "数量",
        open: true,
        options: [1, 2, 4],
        default: 1,
        fanout: true,
      },
    },
  },
};

const makeSegment = (id: string, seq: number, patch: Partial<Segment> = {}): Segment => ({
  id,
  episodeNo: 1,
  seq,
  seconds: 5,
  prompt: `第 ${seq + 1} 镜特写镜头`,
  takes: [
    {
      taskId: `t-${id}-1`,
      status: "succeeded",
      asset: {
        id: `a-${id}-1`,
        url: `https://example.test/${id}-1.mp4`,
        mimeType: "video/mp4",
        sizeBytes: 1024,
        width: 1280,
        height: 720,
        durationMs: 5000,
        createdAt: "2026-11-15T00:00:00Z",
      },
      errorMessage: "",
      createdAt: "2026-11-15T00:00:00Z",
    },
  ],
  chosenTaskId: `t-${id}-1`,
  status: "done",
  revision: 1,
  updatedAt: "2026-11-15T00:00:00Z",
  adaptations: {},
  ...patch,
});

const makeDrama = (editSchema = 0): Drama =>
  ({
    id: "d-studio",
    title: "测试短剧",
    canvasId: "c-studio",
    status: "ready",
    sourceKind: "script",
    genre: "都市",
    styleKey: "real_city_film",
    styleCustom: "",
    ratio: "9:16",
    targetEpisodes: 1,
    episodeSeconds: 30,
    textModel: "",
    videoModel: modelInfo.key,
    imageModels: { character: "", scene: "", prop: "" },
    outlineMd: "",
    charactersMd: "",
    revision: 1,
    episodes: [
      {
        no: 1,
        title: "第一集",
        synopsis: "",
        scriptStatus: "ready",
        locked: true,
        revision: 1,
        updatedAt: "",
        storyboardStatus: "ready",
        segmentCount: 2,
        segmentSeconds: 10,
        doneSegmentCount: 2,
        final: null,
        finalStale: false,
        hook: "",
        cliffhanger: "",
        unitFrom: null,
        unitTo: null,
        segmentVersion: 1,
        editSchema,
      },
    ],
    activeJobs: [],
    createdAt: "",
    updatedAt: "",
    sourceChars: 0,
    unitsTotal: 1,
    unitKind: "episode",
    phase: "done",
    planCursor: 1,
    rangeEnd: 1,
    pauseReason: "",
    pauseCode: "",
    promptSettings: {
      adapt: { source: "builtin", label: "内置" },
      assets: { source: "builtin", label: "内置" },
      asset_prompts: { source: "builtin", label: "内置" },
      video: { source: "builtin", label: "内置" },
    },
  }) as Drama;

const toBackendSegment = (seg: Segment) => ({
  id: seg.id,
  episode_no: seg.episodeNo,
  seq: seg.seq,
  seconds: seg.seconds,
  prompt: seg.prompt,
  takes: seg.takes.map((t) => ({
    task_id: t.taskId,
    status: t.status,
    asset: t.asset
      ? {
          id: t.asset.id,
          url: t.asset.url,
          mime_type: t.asset.mimeType,
          size_bytes: t.asset.sizeBytes,
          width: t.asset.width,
          height: t.asset.height,
          duration_ms: t.asset.durationMs,
          created_at: t.asset.createdAt,
        }
      : null,
    error_message: t.errorMessage,
    created_at: t.createdAt,
  })),
  chosen_task_id: seg.chosenTaskId,
  source_frame_count: seg.sourceFrameCount ?? 120,
  has_audio: seg.hasAudio ?? true,
  edit: seg.edit
    ? {
        source_in_f: seg.edit.sourceInF,
        source_out_f: seg.edit.sourceOutF,
        mute_original: seg.edit.muteOriginal,
        audio_gain_db: seg.edit.audioGainDb,
        native_is_dialogue: seg.edit.nativeIsDialogue,
        av_pts_offset_ms: seg.edit.avPtsOffsetMs,
      }
    : undefined,
  status: seg.status,
  revision: seg.revision,
  updated_at: seg.updatedAt,
});

function TestDialogHost() {
  const dialogs = useDialogStore((state) => state.dialogs);
  const close = useDialogStore((state) => state.close);
  const remove = useDialogStore((state) => state.remove);
  return dialogs.map((item) => (
    <Fragment key={item.id}>
      {item.render({
        open: item.open,
        onClose: () => close(item.id),
        onExited: () => remove(item.id),
      })}
    </Fragment>
  ));
}

function StudioTestHarness({
  drama,
  onStudio,
  renderControls = true,
  refresh,
  applyEpisode,
}: {
  drama: Drama;
  onStudio: (studio: Studio) => void;
  renderControls?: boolean;
  refresh?: () => Promise<void>;
  applyEpisode?: DramaWorkspace["applyEpisode"];
}) {
  const studio = useStudioState(drama, 1, refresh ?? (async () => {}), applyEpisode);
  useEffect(() => {
    onStudio(studio);
  }, [onStudio, studio]);
  return (
    <StudioContext.Provider value={studio}>
      {renderControls && <GenerateControls />}
      <TestDialogHost />
    </StudioContext.Provider>
  );
}

beforeEach(() => {
  localStorage.clear();
  useStudioPrefsStore.setState({ byDrama: {} });
  useCreditsStore.setState({ credits: { available: 500 } } as never);
  useModelsStore.setState({
    byKind: {
      video: { status: "ready", models: [modelInfo], loadedAt: Date.now() },
    },
  });
});

describe("片场 quote-then-generate、单段粘滞隔离、单镜约束与版本切换安全", () => {
  test("resolution 与 count 仅按片段保存，batch() 逐段调用 overridesForModel + quoteSegment 并透传 quoteToken", async () => {
    const s1 = makeSegment("s1", 0, { chosenTaskId: "", takes: [], status: "idle" });
    const s2 = makeSegment("s2", 1, { chosenTaskId: "", takes: [], status: "idle" });
    useDramaSegmentsStore.getState().setList("d-studio", 1, [s1, s2]);

    const callOrder: string[] = [];
    const quoteInputs: Record<string, Record<string, unknown>> = {};
    const genBodies: Record<string, Record<string, unknown>> = {};

    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url.startsWith("/canvas/"))
          return { id: "c-studio", graph: { nodes: [], edges: [] } } as never;
        if (url.endsWith("/segments")) {
          return { items: [toBackendSegment(s1), toBackendSegment(s2)] } as never;
        }
        return {} as never;
      }),
      spyOn(service, "post").mockImplementation(async (url: string, body?: unknown) => {
        const quoteMatch = /^\/drama-segments\/([^/]+)\/quote$/.exec(url);
        if (quoteMatch) {
          const sid = quoteMatch[1]!;
          callOrder.push(`quote:${sid}`);
          quoteInputs[sid] = (body as { input: Record<string, unknown> }).input;
          return {
            effective_input: quoteInputs[sid],
            raw_size: sid === "s1" ? "1080p" : "720p",
            credits: sid === "s1" ? 45 : 15,
            price_fingerprint: `fp-${sid}`,
            quote_token: `qt-${sid}`,
            expires_at: "2099-01-01T00:00:00Z",
          } as never;
        }
        const genMatch = /^\/drama-segments\/([^/]+)\/generate$/.exec(url);
        if (genMatch) {
          const sid = genMatch[1]!;
          callOrder.push(`generate:${sid}`);
          genBodies[sid] = body as Record<string, unknown>;
          return {
            task: {
              id: `task-${sid}`,
              canvas_id: "c-studio",
              node_id: `seg:${sid}`,
              kind: "video",
              model_id: modelInfo.key,
              status: "queued",
              progress: 0,
              outputs: [],
              error_code: null,
              error_message: null,
              credits: 15,
              charged_credits: null,
              version: 1,
              deadline_at: null,
              created_at: "2026-11-15T00:00:00Z",
              submitted_at: null,
              finished_at: null,
            },
          } as never;
        }
        return {} as never;
      }),
    );

    let studio!: Studio;
    const mounted = await dom.mount(
      <StudioTestHarness drama={makeDrama(0)} onStudio={(s) => (studio = s)} />,
    );
    try {
      await dom.flush(100);
      // 给 s1 设置 resolution=1080P、count=2 和模型共享开关 generate_audio=true
      await dom.act(async () => {
        studio.setOverride("resolution", "1080P");
        studio.setOverride("count", 2);
        studio.setOverride("generate_audio", true);
      });
      await dom.flush(80);
      expect(studio.overrides.resolution).toBe("1080P");
      expect(studio.overrides.count).toBe(2);
      expect(studio.overrides.generate_audio).toBe(true);

      // 切到 s2：s2 继承 generate_audio=true，但 resolution 与 count 不得从 s1 泄漏过来
      await dom.act(async () => {
        studio.select("s2");
      });
      await dom.flush(80);
      expect(studio.overrides.generate_audio).toBe(true);
      expect(studio.overrides.resolution).toBeUndefined();
      expect(studio.overrides.count).toBeUndefined();

      // 批量生成两个空白片段
      callOrder.length = 0;
      await dom.act(async () => {
        await studio.actions.batch();
      });
      await dom.flush();

      expect(callOrder).toEqual(["quote:s1", "generate:s1", "quote:s2", "generate:s2"]);
      expect(quoteInputs.s1?.resolution).toBe("1080P");
      expect(quoteInputs.s1?.count).toBe(2);
      expect(quoteInputs.s1?.generate_audio).toBe(true);
      expect(quoteInputs.s2?.resolution).toBe("720P");
      expect(quoteInputs.s2?.count).toBe(1);
      expect(quoteInputs.s2?.generate_audio).toBe(true);
      expect(genBodies.s1?.quote_token).toBe("qt-s1");
      expect(genBodies.s2?.quote_token).toBe("qt-s2");
    } finally {
      await mounted.unmount();
    }
  });

  test("单段生成遇 91002 自动刷新一次 quote 并重试成功；持续 91002 时展示「刷新报价」且不误当 revision 冲突", async () => {
    const s1 = makeSegment("s1", 0);
    useDramaSegmentsStore.getState().setList("d-studio", 1, [s1]);

    let segmentGetCalls = 0;
    let quoteSeq = 0;
    let generateAttempts = 0;
    let forceAlwaysExpired = false;
    let bumpPriceOnNextQuote = false;
    let nextCredits = 31;
    let nextRawSize = "720p";
    let nextFingerprint = "fp-720";

    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url.startsWith("/canvas/"))
          return { id: "c-studio", graph: { nodes: [], edges: [] } } as never;
        if (url.endsWith("/segments")) {
          segmentGetCalls += 1;
          return { items: [toBackendSegment(s1)] } as never;
        }
        return {} as never;
      }),
      spyOn(service, "post").mockImplementation(async (url: string, body?: unknown) => {
        if (url === "/drama-segments/s1/quote") {
          quoteSeq += 1;
          if (bumpPriceOnNextQuote) {
            bumpPriceOnNextQuote = false;
            nextCredits = nextCredits === 31 ? 45 : 60;
            nextRawSize = "1080p";
            nextFingerprint = `fp-${nextCredits}`;
          }
          return {
            effective_input: { resolution: nextRawSize, duration: 5, count: 1 },
            raw_size: nextRawSize,
            credits: nextCredits,
            price_fingerprint: nextFingerprint,
            quote_token: `qt-${quoteSeq}`,
            expires_at: "2099-01-01T00:00:00Z",
          } as never;
        }
        if (url === "/drama-segments/s1/generate") {
          generateAttempts += 1;
          const token = (body as { quote_token?: string }).quote_token;
          if (forceAlwaysExpired || generateAttempts === 1 || generateAttempts === 3) {
            if (generateAttempts === 3) {
              bumpPriceOnNextQuote = true;
            }
            throw new ApiError("生成报价已失效", 91002, 409);
          }
          expect(token).toBe(`qt-${quoteSeq}`);
          return {
            task: {
              id: "task-retry-ok",
              canvas_id: "c-studio",
              node_id: "seg:s1",
              kind: "video",
              model_id: modelInfo.key,
              status: "queued",
              progress: 0,
              outputs: [],
              error_code: null,
              error_message: null,
              credits: nextCredits,
              charged_credits: null,
              version: 1,
              deadline_at: null,
              created_at: "2026-11-15T00:00:00Z",
              submitted_at: null,
              finished_at: null,
            },
          } as never;
        }
        return {} as never;
      }),
    );

    let studio!: Studio;
    const mounted = await dom.mount(
      <StudioTestHarness drama={makeDrama(0)} onStudio={(s) => (studio = s)} />,
    );
    try {
      await dom.flush(100);
      // 展示报价积分及 effective_input / raw_size 摘要
      expect(mounted.container.textContent).toContain("·31");
      expect(mounted.container.querySelector('[data-slot="quote-summary"]')?.textContent).toContain(
        "720p",
      );

      // 首次提交遇 91002 且价格条款一致 -> 刷新 quote 并自动重试成功
      await dom.act(async () => {
        await studio.actions.generate(s1);
      });
      await dom.flush();
      expect(generateAttempts).toBe(2);
      expect(quoteSeq).toBeGreaterThanOrEqual(2);
      const reloadBefore = segmentGetCalls;

      // 报价前预检发现价格上涨（31 -> 45）：在 /generate 提交前即停下，同步新报价避免按高价静默下单
      bumpPriceOnNextQuote = true;
      await dom.act(async () => {
        await studio.actions.generate(s1);
      });
      await dom.flush();
      expect(generateAttempts).toBe(2);
      expect(mounted.container.textContent).toContain("·45");
      expect(mounted.container.querySelector('[data-slot="quote-summary"]')?.textContent).toContain(
        "1080p",
      );

      // 遇 91002 刷新后价格/规格变化：同步新报价但停止第 2 次重提，避免按更高价位自动扣费
      nextCredits = 45;
      bumpPriceOnNextQuote = false;
      // 进入 generateAttempts === 3 触发 91002 后在回调里将下次 quote 涨到更高的下一档
      await dom.act(async () => {
        await studio.actions.generate(s1);
      });
      await dom.flush();
      expect(generateAttempts).toBe(3);
      expect(mounted.container.textContent).toContain("·60");
      expect(segmentGetCalls - reloadBefore).toBe(0);

      // 持续 91002：不得触发 revision 冲突重载，且暴露 quoteState.expired + 「刷新报价」按钮
      forceAlwaysExpired = true;
      const beforeAlwaysFailReloads = segmentGetCalls - reloadBefore;
      await dom.act(async () => {
        await studio.actions.generate(s1);
      });
      await dom.flush();
      expect(studio.quoteState?.expired).toBe(true);
      expect(mounted.container.textContent).toContain("刷新报价");
      expect(segmentGetCalls - reloadBefore).toBe(beforeAlwaysFailReloads);
    } finally {
      await mounted.unmount();
    }
  });

  test("mapDrama 真实后端 DTO 携带 edit_schema=1 / segment_version / final_compose_job_id 时立即激活精裁单镜锁定", async () => {
    const s1 = makeSegment("s1", 0);
    useDramaSegmentsStore.getState().setList("d-dto", 1, [s1]);
    useStudioPrefsStore.getState().setSegmentOverride("d-dto", "s1", modelInfo.key, "count", 2);

    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url.endsWith("/segments")) {
          return { items: [toBackendSegment(s1)] } as never;
        }
        return {
          id: "c-dto",
          graph: { nodes: [], edges: [] },
        } as never;
      }),
    );

    const mapped = mapDrama({
      id: "d-dto",
      title: "DTO 测试",
      canvas_id: "c-dto",
      status: "ready",
      source_kind: "script",
      genre: "都市",
      style_key: "real_city_film",
      style_custom: "",
      ratio: "16:9",
      target_episodes: 1,
      episode_seconds: 30,
      text_model: "",
      video_model: modelInfo.key,
      image_models: { character: "", scene: "", prop: "" },
      outline_md: "",
      characters_md: "",
      revision: 1,
      episodes: [
        {
          no: 1,
          title: "第一集",
          synopsis: "",
          script_status: "ready",
          locked: true,
          revision: 1,
          updated_at: "",
          storyboard_status: "ready",
          segment_count: 1,
          segment_seconds: 5,
          done_segment_count: 1,
          final: null,
          final_stale: false,
          hook: "",
          cliffhanger: "",
          unit_from: null,
          unit_to: null,
          segment_version: 4,
          edit_schema: 1,
          final_compose_job_id: "9001",
        },
      ],
      active_jobs: [],
      created_at: "",
      updated_at: "",
    });
    expect(mapped.episodes[0]?.segmentVersion).toBe(4);
    expect(mapped.episodes[0]?.editSchema).toBe(1);
    expect(mapped.episodes[0]?.finalComposeJobId).toBe("9001");

    const mounted = await dom.mount(<StudioTestHarness drama={mapped} onStudio={() => {}} />);
    try {
      await dom.flush(100);
      const genBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("再生成一次"),
      ) as HTMLButtonElement | undefined;
      expect(genBtn).toBeDefined();
      expect(genBtn?.disabled).toBe(true);
      expect(genBtn?.title).toContain("精裁模式仅支持单条生成");
    } finally {
      await mounted.unmount();
    }
  });

  test("editSchema=1（精裁模式）禁止 count > 1，GenerateControls 禁用生成并展示明确单镜拦截文案", async () => {
    const s1 = makeSegment("s1", 0);
    useDramaSegmentsStore.getState().setList("d-studio", 1, [s1]);
    useStudioPrefsStore.getState().setSegmentOverride("d-studio", "s1", modelInfo.key, "count", 2);

    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url.endsWith("/segments")) {
          return { items: [toBackendSegment(s1)] } as never;
        }
        return {
          id: "c-studio",
          graph: { nodes: [], edges: [] },
        } as never;
      }),
    );

    const mounted = await dom.mount(<StudioTestHarness drama={makeDrama(1)} onStudio={() => {}} />);
    try {
      await dom.flush(100);
      const genBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("再生成一次"),
      ) as HTMLButtonElement | undefined;
      expect(genBtn).toBeDefined();
      expect(genBtn?.disabled).toBe(true);
      expect(genBtn?.title).toContain("精裁模式仅支持单条生成");
    } finally {
      await mounted.unmount();
    }
  });

  test("actions.choose 对已有非默认 edit 的片段先弹保留/重置确认并传 expectedTaskId + resetEditOnTaskChange；patch 捕获 91003", async () => {
    const segWithEdit = makeSegment("s1", 0, {
      takes: [
        ...makeSegment("s1", 0).takes,
        {
          taskId: "t-s1-2",
          status: "succeeded",
          asset: {
            id: "a-s1-2",
            url: "https://example.test/s1-2.mp4",
            mimeType: "video/mp4",
            sizeBytes: 1024,
            width: 1280,
            height: 720,
            durationMs: 5000,
            createdAt: "2026-11-15T00:00:00Z",
          },
          errorMessage: "",
          createdAt: "2026-11-15T00:00:00Z",
        },
      ],
      chosenTaskId: "t-s1-1",
      edit: {
        sourceInF: 10,
        sourceOutF: 90,
        muteOriginal: false,
        audioGainDb: 0,
        nativeIsDialogue: false,
        avPtsOffsetMs: 0,
      },
    });
    useDramaSegmentsStore.getState().setList("d-studio", 1, [segWithEdit]);

    const patchPayloads: Array<Record<string, unknown>> = [];
    let rejectNextPatchWith91003 = false;
    let currentSeg = segWithEdit;

    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url.endsWith("/segments")) {
          return { items: [toBackendSegment(currentSeg)] } as never;
        }
        return {
          id: "c-studio",
          graph: { nodes: [], edges: [] },
        } as never;
      }),
      spyOn(service, "patch").mockImplementation(async (_url: string, body?: unknown) => {
        if (rejectNextPatchWith91003) {
          throw new ApiError("出点超出新素材总帧数", 91003, 400);
        }
        const raw = body as Record<string, unknown>;
        patchPayloads.push(raw);
        currentSeg = {
          ...currentSeg,
          chosenTaskId: String(raw.chosen_task_id ?? currentSeg.chosenTaskId),
          revision: currentSeg.revision + 1,
        };
        return toBackendSegment(currentSeg) as never;
      }),
    );

    let studio!: Studio;
    const mounted = await dom.mount(
      <StudioTestHarness
        drama={makeDrama(1)}
        onStudio={(s) => (studio = s)}
        renderControls={false}
      />,
    );
    try {
      await dom.flush(100);

      // 1. 选择保留剪辑并切换
      let choosePromise!: Promise<void>;
      await dom.act(async () => {
        choosePromise = studio.actions.choose(segWithEdit, "t-s1-2");
      });
      await dom.flush();
      const keepBtn = Array.from(dom.win.document.body.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("保留剪辑并切换"),
      );
      expect(keepBtn).toBeDefined();
      await dom.click(keepBtn);
      await choosePromise;
      await dom.flush();
      expect(patchPayloads[0]?.chosen_task_id).toBe("t-s1-2");
      expect(patchPayloads[0]?.expected_task_id).toBe("t-s1-1");
      expect(patchPayloads[0]?.reset_edit_on_task_change).toBe(false);

      // 2. 选择重置剪辑并切换
      await dom.act(async () => {
        choosePromise = studio.actions.choose(segWithEdit, "t-s1-2");
      });
      await dom.flush();
      const resetBtn = Array.from(dom.win.document.body.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("重置剪辑并切换"),
      );
      expect(resetBtn).toBeDefined();
      await dom.click(resetBtn);
      await choosePromise;
      await dom.flush();
      expect(patchPayloads[1]?.reset_edit_on_task_change).toBe(true);

      // 3. patch 遇 91003 透出 ApiError
      rejectNextPatchWith91003 = true;
      await expect(studio.actions.patch("s1", { edit: { sourceInF: 200 } })).rejects.toThrow(
        "出点超出新素材总帧数",
      );
    } finally {
      await mounted.unmount();
    }
  });

  test("PATCH 返回 segment_version 时直接更新 workspace episode.segmentVersion（即使 refresh 失败），且版本单调不回退", async () => {
    const s1 = makeSegment("s1", 0, { revision: 2 });
    useDramaSegmentsStore.getState().setList("d-studio", 1, [s1]);

    let refreshAttempts = 0;
    let patchIndex = 0;
    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url.startsWith("/canvas/")) {
          return { id: "c-studio", graph: { nodes: [], edges: [] } } as never;
        }
        if (url.endsWith("/segments")) {
          return { items: [toBackendSegment(s1)] } as never;
        }
        return {} as never;
      }),
      spyOn(service, "patch").mockImplementation(async (_url: string, body?: unknown) => {
        patchIndex += 1;
        const payload = body as Record<string, unknown>;
        return {
          ...toBackendSegment(
            makeSegment("s1", 0, {
              revision: (payload.revision as number) + 1,
            }),
          ),
          // 第 1 次返回 segment_version=8，第 2 次模拟乱序回传更低版本 5
          segment_version: patchIndex === 1 ? 8 : 5,
        } as never;
      }),
    );

    const appliedUpdates: Array<{ no: number; segmentVersion?: number }> = [];
    let latestDrama!: Drama;
    let studio!: Studio;

    const ContextHarness = ({ onDrama }: { onDrama: (d: Drama) => void }) => {
      const [drama, setDrama] = useState<Drama>(() => {
        const base = makeDrama();
        return {
          ...base,
          episodes: base.episodes.map((ep) => (ep.no === 1 ? { ...ep, segmentVersion: 3 } : ep)),
        };
      });
      useEffect(() => {
        onDrama(drama);
      }, [drama, onDrama]);
      const applyEpisode: DramaWorkspace["applyEpisode"] = (no, change) => {
        appliedUpdates.push({ no, segmentVersion: change.segmentVersion });
        setDrama((prev) => ({
          ...prev,
          episodes: prev.episodes.map((ep) => {
            if (ep.no !== no) return ep;
            const nextVersion =
              typeof change.segmentVersion === "number"
                ? Math.max(ep.segmentVersion ?? 0, change.segmentVersion)
                : ep.segmentVersion;
            return {
              ...ep,
              ...change,
              ...(nextVersion !== undefined ? { segmentVersion: nextVersion } : {}),
            };
          }),
        }));
      };
      const workspace = { drama, jobs: [], applyEpisode } as unknown as DramaWorkspace;
      return (
        <DramaWorkspaceContext.Provider value={workspace}>
          <StudioTestHarness
            drama={drama}
            renderControls={false}
            refresh={async () => {
              refreshAttempts += 1;
              throw new Error("refresh network error");
            }}
            onStudio={(s) => {
              studio = s;
            }}
          />
        </DramaWorkspaceContext.Provider>
      );
    };

    const mounted = await dom.mount(
      <ContextHarness
        onDrama={(d) => {
          latestDrama = d;
        }}
      />,
    );
    try {
      await dom.flush(30);

      // edit PATCH 会触发 refresh()，即便 refresh() 失败，episode.segmentVersion 仍直接从 PATCH 响应升级到 8
      await dom.act(async () => {
        await studio.actions.patch("s1", { edit: { sourceInF: 10 } });
      });
      await dom.flush(30);

      expect(refreshAttempts).toBe(1);
      expect(appliedUpdates).toEqual([{ no: 1, segmentVersion: 8 }]);
      expect(latestDrama.episodes.find((ep) => ep.no === 1)?.segmentVersion).toBe(8);

      // 第 2 次响应若携带乱序较低 segment_version=5，工作台状态保持 8 不回退
      await dom.act(async () => {
        await studio.actions.patch("s1", { prompt: "updated prompt" });
      });
      await dom.flush(30);
      expect(latestDrama.episodes.find((ep) => ep.no === 1)?.segmentVersion).toBe(8);
    } finally {
      await mounted.unmount();
    }
  });

  test("useDramaWorkspace applyEpisode 真实实现保证 segmentVersion 只升不降", async () => {
    const rawDrama = {
      id: "d-ws",
      title: "测试短剧",
      canvas_id: "c-ws",
      status: "ready",
      source_kind: "script",
      genre: "都市",
      style_key: "real_city_film",
      style_custom: "",
      ratio: "9:16",
      target_episodes: 1,
      episode_seconds: 30,
      text_model: "",
      video_model: modelInfo.key,
      image_models: { character: "", scene: "", prop: "" },
      outline_md: "",
      characters_md: "",
      revision: 1,
      episodes: [
        {
          no: 1,
          title: "第一集",
          synopsis: "",
          script_status: "ready",
          locked: true,
          revision: 1,
          updated_at: "",
          storyboard_status: "ready",
          segment_count: 1,
          segment_seconds: 5,
          done_segment_count: 1,
          final: null,
          final_stale: false,
          hook: "",
          cliffhanger: "",
          unit_from: null,
          unit_to: null,
          segment_version: 3,
          edit_schema: 1,
          final_compose_job_id: null,
        },
      ],
      active_jobs: [],
      created_at: "",
      updated_at: "",
      source_chars: 0,
      units_total: 1,
      unit_kind: "episode",
      phase: "done",
      plan_cursor: 1,
      range_end: 1,
      pause_reason: "",
      pause_code: "",
      prompt_settings: {
        adapt: { source: "builtin", label: "内置" },
        assets: { source: "builtin", label: "内置" },
        asset_prompts: { source: "builtin", label: "内置" },
        video: { source: "builtin", label: "内置" },
      },
    };
    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-ws") return rawDrama as never;
        if (url === "/dramas/d-ws/jobs") return { items: [] } as never;
        return {} as never;
      }),
    );

    let wsState: ReturnType<typeof useDramaWorkspace> | null = null;
    const WorkspaceProbe = ({
      onState,
    }: {
      onState: (next: ReturnType<typeof useDramaWorkspace>) => void;
    }) => {
      const ws = useDramaWorkspace("d-ws");
      useEffect(() => {
        onState(ws);
      }, [onState, ws]);
      return null;
    };
    const mounted = await dom.mount(
      <WorkspaceProbe
        onState={(next) => {
          wsState = next;
        }}
      />,
    );
    try {
      await dom.flush(30);
      expect(wsState?.drama?.episodes[0]?.segmentVersion).toBe(3);

      await dom.act(async () => {
        wsState?.applyEpisode(1, { segmentVersion: 8 });
      });
      expect(wsState?.drama?.episodes[0]?.segmentVersion).toBe(8);

      await dom.act(async () => {
        wsState?.applyEpisode(1, { segmentVersion: 5 });
      });
      expect(wsState?.drama?.episodes[0]?.segmentVersion).toBe(8);
    } finally {
      await mounted.unmount();
    }
  });

  test("useDramaWorkspace patch 与 refresh 返回过期 segment_version 时单调不回退，切换剧集或新建分集时使用新版本", async () => {
    const baseEpisode = (epNo: number, segmentVersion: number) => ({
      id: `ep-${epNo}`,
      drama_id: "d-ws-mono",
      no: epNo,
      title: `第${epNo}集`,
      summary: "",
      script: "",
      status: "draft" as const,
      locked: true,
      revision: 1,
      updated_at: "",
      storyboard_status: "ready" as const,
      segment_count: 1,
      segment_seconds: 5,
      done_segment_count: 1,
      final: null,
      final_stale: false,
      hook: "",
      cliffhanger: "",
      unit_from: null,
      unit_to: null,
      segment_version: segmentVersion,
      edit_schema: 1,
      final_compose_job_id: null,
    });
    const makeBackendDrama = (id: string, title: string, revision: number, epVersion: number) => ({
      id,
      title,
      genre: "",
      style: "",
      tone: "",
      ratio: "16:9",
      characters: [],
      status: "ready" as const,
      revision,
      episodes: [baseEpisode(1, epVersion)],
      active_jobs: [],
      created_at: "",
      updated_at: "",
      source_chars: 0,
      units_total: 1,
      unit_kind: "episode" as const,
      phase: "done" as const,
      plan_cursor: 1,
      range_end: 1,
      pause_reason: "",
      pause_code: "",
      prompt_settings: {
        adapt: { source: "builtin", label: "内置" },
        assets: { source: "builtin", label: "内置" },
        asset_prompts: { source: "builtin", label: "内置" },
        video: { source: "builtin", label: "内置" },
      },
    });

    let refreshEpisodeVersion = 3;
    let patchEpisodeVersion = 3;
    let patchCallCount = 0;

    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-ws-mono") {
          return makeBackendDrama("d-ws-mono", "原始剧名", 1, refreshEpisodeVersion) as never;
        }
        if (url === "/dramas/d-ws-mono/jobs") return { items: [] } as never;
        return {} as never;
      }),
      spyOn(service, "patch").mockImplementation(async (url: string, body?: unknown) => {
        if (url === "/dramas/d-ws-mono") {
          patchCallCount += 1;
          const title = (body as { title?: string } | undefined)?.title ?? "原始剧名";
          return makeBackendDrama(
            "d-ws-mono",
            title,
            1 + patchCallCount,
            patchEpisodeVersion,
          ) as never;
        }
        return {} as never;
      }),
    );

    let wsState: ReturnType<typeof useDramaWorkspace> | null = null;
    const WorkspaceProbe = ({
      onState,
    }: {
      onState: (next: ReturnType<typeof useDramaWorkspace>) => void;
    }) => {
      const ws = useDramaWorkspace("d-ws-mono");
      useEffect(() => {
        onState(ws);
      }, [onState, ws]);
      return null;
    };
    const mounted = await dom.mount(
      <WorkspaceProbe
        onState={(next) => {
          wsState = next;
        }}
      />,
    );
    try {
      await dom.flush(30);
      expect(wsState?.drama?.episodes[0]?.segmentVersion).toBe(3);

      // 中间分镜 PATCH 将 episode 1 segmentVersion 推进到 8
      await dom.act(async () => {
        wsState?.applyEpisode(1, { id: "ep-1", segmentVersion: 8 });
      });
      expect(wsState?.drama?.episodes[0]?.segmentVersion).toBe(8);

      // 延迟返回的项目 PATCH 携带过期 segment_version=3，不得将 8 回退到 3
      patchEpisodeVersion = 3;
      await dom.act(async () => {
        await wsState?.patch({ title: "新标题" });
      });
      expect(wsState?.drama?.title).toBe("新标题");
      expect(wsState?.drama?.episodes[0]?.segmentVersion).toBe(8);

      // 延迟返回的项目 GET refresh 携带过期 segment_version=5，也不得回退 8
      refreshEpisodeVersion = 5;
      await dom.act(async () => {
        await wsState?.refresh();
      });
      expect(wsState?.drama?.episodes[0]?.segmentVersion).toBe(8);

      // 后端 legitimately 推进 segment_version=9 时，必须升级到 9
      patchEpisodeVersion = 9;
      await dom.act(async () => {
        await wsState?.patch({ ratio: "9:16" });
      });
      expect(wsState?.drama?.episodes[0]?.segmentVersion).toBe(9);

      // 不同 dramaId 或新出现的分集号不得沿用旧剧集/旧分集的 segmentVersion
      const currentDrama = wsState!.drama;
      const switchedDrama: Drama = {
        ...currentDrama,
        id: "d-ws-other",
        episodes: [{ ...currentDrama.episodes[0]!, segmentVersion: 1 }],
      };
      expect(
        mergeEpisodeListWithMonotonicVersions(currentDrama, switchedDrama)[0]?.segmentVersion,
      ).toBe(1);

      const newEpisodeAdded: Drama = {
        ...currentDrama,
        episodes: [
          ...currentDrama.episodes,
          { ...currentDrama.episodes[0]!, id: "ep-2", no: 2, segmentVersion: 2 },
        ],
      };
      const mergedNewEp = mergeEpisodeListWithMonotonicVersions(currentDrama, newEpisodeAdded);
      expect(mergedNewEp.find((ep) => ep.no === 1)?.segmentVersion).toBe(9);
      expect(mergedNewEp.find((ep) => ep.no === 2)?.segmentVersion).toBe(2);
    } finally {
      await mounted.unmount();
    }
  });

  test("删除分集重编号后按稳定分集 id 匹配单调版本，防止旧集 v8 与过期 applyEpisode 污染后续分集 v2", async () => {
    const rawEpisodeBase = (id: string, no: number, segmentVersion: number) => ({
      id,
      no,
      title: `第${no}集`,
      synopsis: "",
      script_status: "ready" as const,
      locked: true,
      revision: 1,
      updated_at: "",
      storyboard_status: "ready" as const,
      segment_count: 1,
      segment_seconds: 5,
      done_segment_count: 1,
      final: null,
      final_stale: false,
      hook: "",
      cliffhanger: "",
      unit_from: null,
      unit_to: null,
      segment_version: segmentVersion,
      edit_schema: 1,
      final_compose_job_id: null,
    });

    const rawBeforeDelete = {
      id: "d-ws-del",
      title: "删集重编号测试",
      canvas_id: "c-ws-del",
      status: "ready",
      source_kind: "script",
      genre: "都市",
      style_key: "real_city_film",
      style_custom: "",
      ratio: "9:16",
      target_episodes: 2,
      episode_seconds: 30,
      text_model: "",
      video_model: modelInfo.key,
      image_models: { character: "", scene: "", prop: "" },
      outline_md: "",
      characters_md: "",
      revision: 1,
      episodes: [rawEpisodeBase("ep-a", 1, 8), rawEpisodeBase("ep-b", 2, 2)],
      active_jobs: [],
      created_at: "",
      updated_at: "",
      source_chars: 0,
      units_total: 2,
      unit_kind: "episode",
      phase: "done",
      plan_cursor: 2,
      range_end: 2,
      pause_reason: "",
      pause_code: "",
      prompt_settings: {
        adapt: { source: "builtin", label: "内置" },
        assets: { source: "builtin", label: "内置" },
        asset_prompts: { source: "builtin", label: "内置" },
        video: { source: "builtin", label: "内置" },
      },
    };
    const rawAfterDelete = {
      ...rawBeforeDelete,
      revision: 2,
      target_episodes: 1,
      episodes: [rawEpisodeBase("ep-b", 1, 2)],
    };

    const beforeDeleteDrama = mapDrama(rawBeforeDelete as never);
    const afterDeleteDrama = mapDrama(rawAfterDelete as never);
    const mergedAfterDelete = mergeEpisodeListWithMonotonicVersions(
      beforeDeleteDrama,
      afterDeleteDrama,
    );
    expect(mergedAfterDelete).toHaveLength(1);
    expect(mergedAfterDelete[0]?.id).toBe("ep-b");
    expect(mergedAfterDelete[0]?.no).toBe(1);
    expect(mergedAfterDelete[0]?.segmentVersion).toBe(2);

    // 工作台真实 refresh() 与过期 applyEpisode(1, { id: "ep-a", segmentVersion: 8 }) 均不得污染 ep-b
    let fetchCount = 0;
    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-ws-del") {
          fetchCount += 1;
          return (fetchCount === 1 ? rawBeforeDelete : rawAfterDelete) as never;
        }
        if (url === "/dramas/d-ws-del/jobs") return { items: [] } as never;
        return {} as never;
      }),
    );

    let wsState: ReturnType<typeof useDramaWorkspace> | null = null;
    const WorkspaceDeleteProbe = ({
      onState,
    }: {
      onState: (next: ReturnType<typeof useDramaWorkspace>) => void;
    }) => {
      const ws = useDramaWorkspace("d-ws-del");
      useEffect(() => {
        onState(ws);
      }, [onState, ws]);
      return null;
    };
    const mounted = await dom.mount(
      <WorkspaceDeleteProbe
        onState={(next) => {
          wsState = next;
        }}
      />,
    );
    try {
      await dom.flush(30);
      expect(wsState?.drama?.episodes.map((e) => `${e.id}:${e.no}:v${e.segmentVersion}`)).toEqual([
        "ep-a:1:v8",
        "ep-b:2:v2",
      ]);

      await dom.act(async () => {
        await wsState?.refresh();
      });
      expect(wsState?.drama?.episodes).toHaveLength(1);
      expect(wsState?.drama?.episodes[0]?.id).toBe("ep-b");
      expect(wsState?.drama?.episodes[0]?.no).toBe(1);
      expect(wsState?.drama?.episodes[0]?.segmentVersion).toBe(2);

      // 携带已删分集 id="ep-a" 的延迟更新命中 no=1 时不得污染 ep-b 的 segmentVersion
      await dom.act(async () => {
        wsState?.applyEpisode(1, { id: "ep-a", segmentVersion: 8 });
      });
      expect(wsState?.drama?.episodes[0]?.segmentVersion).toBe(2);

      // 分集摘要已有稳定 id 时，缺 id 的异步补丁 applyEpisode(1, { segmentVersion: 8 }) 必须丢弃
      await dom.act(async () => {
        wsState?.applyEpisode(1, { segmentVersion: 8 });
      });
      expect(wsState?.drama?.episodes[0]?.segmentVersion).toBe(2);

      // 携带正确 id="ep-b" 的更新即使传入旧集号 2，也能精确更新 ep-b 自身且保留新集号 1
      await dom.act(async () => {
        wsState?.applyEpisode(2, { id: "ep-b", segmentVersion: 6 });
      });
      expect(wsState?.drama?.episodes[0]?.no).toBe(1);
      expect(wsState?.drama?.episodes[0]?.segmentVersion).toBe(6);
    } finally {
      await mounted.unmount();
    }

    // useStudioState 分镜 PATCH 返回 episode_id 时把 id 透传给 syncEpisode(applyEpisode)
    const segB = makeSegment("seg-epb", 0, { episodeNo: 1, episodeId: "ep-b", revision: 1 });
    useDramaSegmentsStore.getState().setList("d-studio", 1, [segB]);
    const syncedCalls: Array<{ no: number; change: Partial<Drama["episodes"][number]> }> = [];
    const baseDrama = makeDrama(1);
    const dramaWithEpId: Drama = {
      ...baseDrama,
      episodes: [{ ...baseDrama.episodes[0]!, id: "ep-b", segmentVersion: 2 }],
    };
    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url.startsWith("/canvas/")) {
          return { id: "c-studio", graph: { nodes: [], edges: [] } } as never;
        }
        if (url.endsWith("/segments")) {
          return {
            items: [{ ...toBackendSegment(segB), episode_id: "ep-b", segment_version: 2 }],
          } as never;
        }
        return {} as never;
      }),
      spyOn(service, "patch").mockResolvedValue({
        ...toBackendSegment(segB),
        episode_id: "ep-b",
        seconds: 6,
        revision: 2,
        segment_version: 7,
      } as never),
    );
    let studioRef!: Studio;
    const mountedStudio = await dom.mount(
      <StudioTestHarness
        drama={dramaWithEpId}
        renderControls={false}
        onStudio={(next) => {
          studioRef = next;
        }}
        applyEpisode={(no, change) => {
          syncedCalls.push({ no, change });
        }}
      />,
    );
    try {
      await dom.flush(30);
      await dom.act(async () => {
        await studioRef.actions.patch("seg-epb", { seconds: 6 });
      });
      expect(
        syncedCalls.some(
          (c) => c.no === 1 && c.change.id === "ep-b" && c.change.segmentVersion === 7,
        ),
      ).toBe(true);
    } finally {
      await mountedStudio.unmount();
    }
  });
});
