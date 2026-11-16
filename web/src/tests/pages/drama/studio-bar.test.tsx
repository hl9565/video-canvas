import { useState } from "react";
import { afterAll, beforeAll, describe, expect, test } from "bun:test";

import type { Segment } from "@/api/drama/type";
import type { ModelInfo } from "@/api/model/type";
import { StudioBar } from "@/pages/drama/studio/studio-bar";
import { StudioContext, type Studio } from "@/pages/drama/studio/use-studio";
import { DramaWorkspaceContext, type DramaWorkspace } from "@/pages/drama/workspace/context";
import { overridesForModel, useStudioPrefs, useStudioPrefsStore } from "@/store/drama-studio";

import { installDom, type DomEnv } from "../../helpers/dom";

let dom: DomEnv;

beforeAll(async () => {
  dom = await installDom();
});

afterAll(() => {
  dom.restore();
});

const modelWithSeed: ModelInfo = {
  key: "h3-colab",
  kind: "video",
  label: "H3",
  pricing: { billing: "unit", unit: 1 },
  capabilities: {
    ops: ["i2v"],
    refs: {
      image: { on: true, max: 1, max_mb: 10 },
      video: { on: false, max: 0, max_mb: 0 },
      audio: { on: false, max: 0, max_mb: 0 },
    },
    prompt: { max_length: 1000 },
    params: {
      duration: { type: "enum", label: "时长", open: true, options: [2, 5], default: 5 },
      resolution: {
        type: "enum",
        label: "分辨率",
        open: true,
        options: ["720P", "1080P"],
        default: "720P",
      },
      seed: {
        type: "number",
        label: "随机种子",
        open: true,
        min: -1,
        max: 4_294_967_295,
        step: 1,
        default: 3_916_315_536,
      },
    },
  },
};

const studioValue = (patch: Partial<Studio> = {}): Studio =>
  ({
    drama: { id: "d1", ratio: "9:16" },
    episode: { id: "e1", episodeNo: 1, title: "第一集" },
    no: 1,
    save: "saved",
    selected: {
      id: "s1",
      episodeNo: 1,
      seq: 0,
      seconds: 3,
      prompt: "测试",
      takes: [],
      chosenTaskId: "",
      status: "idle",
      revision: 1,
      updatedAt: "",
      adaptations: {},
    },
    assets: [],
    assetsStatus: "ready",
    format: "h3",
    adapting: new Set<string>(),
    adaptErrors: {},
    adaptStateOf: () => "fresh",
    submitting: new Set<string>(),
    plan: { errors: {}, input: {}, refs: [], imageCount: 0, imageMax: 9, deadLabels: [] },
    credits: undefined,
    availableCredits: null,
    video: { models: [modelWithSeed], status: "ready", model: modelWithSeed, setModel: () => {} },
    overrides: {},
    setOverride: () => {},
    actions: { setSeconds: async () => {}, add: async () => {} },
    ...patch,
  }) as unknown as Studio;

const workspaceValue = {
  drama: { id: "d1", ratio: "9:16" },
  refresh: async () => {},
} as unknown as DramaWorkspace;

function StudioBarHarness({ patch }: { patch?: Partial<Studio> }) {
  const [overrides, setOverrides] = useState<Record<string, unknown>>(patch?.overrides ?? {});
  const value = studioValue({
    ...patch,
    overrides,
    setOverride: (name, next) =>
      setOverrides((prev) => {
        const copy = { ...prev };
        if (next === undefined) delete copy[name];
        else copy[name] = next;
        return copy;
      }),
  });
  return (
    <DramaWorkspaceContext.Provider value={workspaceValue}>
      <StudioContext.Provider value={value}>
        <StudioBar />
      </StudioContext.Provider>
    </DramaWorkspaceContext.Provider>
  );
}

describe("StudioBar：seed 胶囊、不合档位警示与窄屏收纳", () => {
  test("宽栏展示 seed 胶囊（可切换随机/恢复默认）、历史时长不合档位警示，窄屏展示「规格与参数」入口", async () => {
    const mounted = await dom.mount(<StudioBarHarness />);

    try {
      const text = mounted.container.textContent ?? "";
      expect(text).toContain("3s→5s");
      expect(text).toContain("种子 3916315536");
      expect(text).toContain("规格与参数");

      const seedTrigger = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("种子 3916315536"),
      );
      expect(seedTrigger).toBeDefined();
      await dom.click(seedTrigger);
      await dom.flush();
      const randomBtn = Array.from(dom.win.document.body.querySelectorAll("button")).find(
        (b) => b.textContent === "随机 (-1)",
      );
      await dom.click(randomBtn);
      await dom.flush();
      expect(mounted.container.textContent).toContain("种子 随机");
      const resetBtn = Array.from(dom.win.document.body.querySelectorAll("button")).find(
        (b) => b.textContent === "恢复默认",
      );
      await dom.click(resetBtn);
      await dom.flush();
      expect(mounted.container.textContent).toContain("种子 3916315536");

      const mobileTrigger = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("规格与参数"),
      );
      expect(mobileTrigger).toBeDefined();
      await dom.click(mobileTrigger);
      await dom.flush();

      const body = dom.win.document.body.textContent ?? "";
      expect(body).toContain("画面比例");
      expect(body).toContain("9:16");
      expect(body).toContain("不在模型档位，将按 5s 提交");
      expect(body).toContain("随机 (-1)");
    } finally {
      await mounted.unmount();
    }
  });

  test("没有开放 seed 时不渲染 seed 胶囊；seed=-1 摘要为「种子 随机」", async () => {
    const noSeedModel: ModelInfo = {
      ...modelWithSeed,
      capabilities: {
        ...modelWithSeed.capabilities,
        params: { duration: modelWithSeed.capabilities.params!.duration },
      },
    };
    const withoutSeed = await dom.mount(
      <StudioBarHarness
        patch={{
          video: { models: [noSeedModel], status: "ready", model: noSeedModel, setModel: () => {} },
        }}
      />,
    );
    try {
      expect(withoutSeed.container.textContent).not.toContain("种子");
    } finally {
      await withoutSeed.unmount();
    }

    const withRandomSeed = await dom.mount(
      <StudioBarHarness patch={{ overrides: { seed: -1 } }} />,
    );
    try {
      expect(withRandomSeed.container.textContent).toContain("种子 随机");
    } finally {
      await withRandomSeed.unmount();
    }
  });
});

const modelWithAudioAndCount: ModelInfo = {
  ...modelWithSeed,
  key: "seedance-audio",
  label: "Seedance Audio",
  capabilities: {
    ...modelWithSeed.capabilities,
    params: {
      ...modelWithSeed.capabilities.params,
      generate_audio: {
        type: "boolean",
        label: "生成音频",
        open: true,
        default: false,
      },
      count: {
        type: "enum",
        label: "生成数量",
        open: true,
        options: [1, 2, 4],
        default: 1,
        fanout: true,
      },
    },
  },
};

const makeSegment = (id: string, seq: number): Segment => ({
  id,
  episodeNo: 1,
  seq,
  seconds: 5,
  prompt: `镜头 ${seq + 1}`,
  takes: [],
  chosenTaskId: "",
  status: "idle",
  revision: 1,
  updatedAt: "",
  adaptations: {},
});

function PerSegmentStudioBarHarness({ editSchema = 0 }: { editSchema?: number }) {
  const seg1 = makeSegment("s1", 0);
  const seg2 = makeSegment("s2", 1);
  const segments = [seg1, seg2];
  const [selectedId, setSelectedId] = useState("s1");
  const selected = segments.find((item) => item.id === selectedId) ?? seg1;
  const prefs = useStudioPrefs("d-audio");
  const overrides = overridesForModel(prefs, modelWithAudioAndCount.key, selected.id);
  const drama = {
    id: "d-audio",
    ratio: "9:16",
    episodes: [{ no: 1, title: "第一集", editSchema }],
  };
  const value = studioValue({
    drama: drama as unknown as Studio["drama"],
    selected,
    select: setSelectedId,
    segments,
    overrides,
    setOverride: (name, next) =>
      useStudioPrefsStore
        .getState()
        .setModelOverride(
          "d-audio",
          modelWithAudioAndCount.key,
          name,
          next,
          undefined,
          selected.id,
        ),
    video: {
      models: [modelWithAudioAndCount],
      status: "ready",
      model: modelWithAudioAndCount,
      setModel: () => {},
    },
  });
  return (
    <DramaWorkspaceContext.Provider
      value={{ drama, refresh: async () => {} } as unknown as DramaWorkspace}
    >
      <StudioContext.Provider value={value}>
        <button type="button" data-testid="switch-s1" onClick={() => setSelectedId("s1")}>
          切到S1
        </button>
        <button type="button" data-testid="switch-s2" onClick={() => setSelectedId("s2")}>
          切到S2
        </button>
        <StudioBar />
      </StudioContext.Provider>
    </DramaWorkspaceContext.Provider>
  );
}

describe("StudioBar：generate_audio 开关、per-sid count 与单镜约束同步", () => {
  test("桌面与移动端「规格与参数」同步 generate_audio 开关，且 resolution / count 仅按片段生效不跨段粘滞", async () => {
    localStorage.clear();
    useStudioPrefsStore.setState({ byDrama: {} });
    const mounted = await dom.mount(<PerSegmentStudioBarHarness editSchema={0} />);

    try {
      // 1. 桌面切换 generate_audio -> 开，再打开窄屏浮层核对同步
      const audioBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("生成音频"),
      );
      expect(audioBtn).toBeDefined();
      expect(audioBtn?.getAttribute("aria-pressed")).toBe("false");
      await dom.click(audioBtn);
      await dom.flush();
      expect(audioBtn?.getAttribute("aria-pressed")).toBe("true");
      expect(audioBtn?.textContent).toContain("开");

      // 2. 桌面修改 s1 的 resolution=1080P 与 count=2
      const resBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("720P"),
      );
      await dom.click(resBtn);
      await dom.flush();
      const opt1080 = Array.from(dom.win.document.body.querySelectorAll("button")).find(
        (b) => b.textContent?.trim() === "1080P",
      );
      await dom.click(opt1080);
      await dom.flush();

      const countBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("1 条"),
      );
      await dom.click(countBtn);
      await dom.flush();
      const opt2 = Array.from(dom.win.document.body.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("2 条"),
      );
      await dom.click(opt2);
      await dom.flush();
      expect(mounted.container.textContent).toContain("1080P");
      expect(mounted.container.textContent).toContain("2 条");

      // 3. 切到 s2：s2 继承模型级 generate_audio=开，但绝不继承 s1 的 resolution=1080P 和 count=2
      await dom.click(mounted.container.querySelector('[data-testid="switch-s2"]'));
      await dom.flush();
      expect(mounted.container.textContent).toContain("720P");
      expect(mounted.container.textContent).toContain("1 条");
      expect(mounted.container.textContent).not.toContain("1080P");
      expect(mounted.container.textContent).not.toContain("2 条");
      const s2AudioBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("生成音频"),
      );
      expect(s2AudioBtn?.getAttribute("aria-pressed")).toBe("true");

      // 4. 在 MobileSpecsChip 浮层里切回 generate_audio=关，桌面按钮应同步变为关
      const mobileTrigger = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("规格与参数"),
      );
      await dom.click(mobileTrigger);
      await dom.flush();
      const mobileAudioRow = Array.from(dom.win.document.body.querySelectorAll("button")).find(
        (b) => b.textContent?.includes("生成音频") && b !== s2AudioBtn,
      );
      expect(mobileAudioRow?.getAttribute("aria-pressed")).toBe("true");
      await dom.click(mobileAudioRow);
      await dom.flush();
      expect(s2AudioBtn?.getAttribute("aria-pressed")).toBe("false");
      expect(dom.win.document.body.textContent).toContain("生成数量（仅本片段）");
    } finally {
      await mounted.unmount();
    }
  });

  test("editSchema=1（精裁模式）时桌面与移动端均锁定 count=1（禁用 2 条/4 条）", async () => {
    localStorage.clear();
    useStudioPrefsStore.setState({ byDrama: {} });
    const mounted = await dom.mount(<PerSegmentStudioBarHarness editSchema={1} />);

    try {
      const countBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("1 条"),
      );
      await dom.click(countBtn);
      await dom.flush();
      const opt2 = Array.from(dom.win.document.body.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("2 条"),
      ) as HTMLButtonElement | undefined;
      expect(opt2).toBeDefined();
      expect(opt2?.disabled).toBe(true);
      expect(dom.win.document.body.textContent).toContain("单镜模式锁定");
    } finally {
      await mounted.unmount();
    }
  });
});
