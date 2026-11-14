import { useState } from "react";
import { afterAll, beforeAll, describe, expect, test } from "bun:test";

import type { ModelInfo } from "@/api/model/type";
import { StudioBar } from "@/pages/drama/studio/studio-bar";
import { StudioContext, type Studio } from "@/pages/drama/studio/use-studio";
import { DramaWorkspaceContext, type DramaWorkspace } from "@/pages/drama/workspace/context";

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
