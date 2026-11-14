import { describe, expect, test } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";

import type { Segment } from "@/api/drama/type";
import { GenerateControls } from "@/pages/drama/studio/generate-controls";
import { PromptPane } from "@/pages/drama/studio/prompt-pane";
import { StudioContext, type Studio } from "@/pages/drama/studio/use-studio";

const seg = (patch: Partial<Segment> = {}): Segment => ({
  id: "s1",
  episodeNo: 1,
  seq: 0,
  seconds: 10,
  prompt: "【场景】原稿",
  takes: [],
  chosenTaskId: "",
  status: "idle",
  revision: 1,
  updatedAt: "",
  adaptations: {},
  ...patch,
});

const studio = (patch: Partial<Studio>): Studio =>
  ({
    assets: [],
    assetsStatus: "ready",
    no: 1,
    format: "h3",
    adapting: new Set<string>(),
    adaptErrors: {},
    adaptStateOf: () => "none",
    setDraft: () => {},
    reportSave: () => {},
    registerEditor: () => () => {},
    submitting: new Set<string>(),
    plan: { errors: {}, input: {}, refs: [], imageCount: 0, imageMax: 9, deadLabels: [] },
    video: { models: [], status: "ready", model: { key: "h3" }, setModel: () => {} },
    actions: { patch: async () => seg(), generate: async () => {}, adapt: async () => true },
    ...patch,
  }) as unknown as Studio;

const render = (value: Studio, node: React.ReactNode) =>
  renderToStaticMarkup(<StudioContext.Provider value={value}>{node}</StudioContext.Provider>);

describe("片场：原稿 / 转写标签", () => {
  test("顶栏是 Seedance 模型：没有转写标签和转写按钮", () => {
    const html = render(
      studio({ format: "seedance", adaptStateOf: () => null }),
      <PromptPane segment={seg()} />,
    );
    expect(html).not.toContain("原稿 · Seedance");
    expect(html).not.toContain("转写为");
  });

  test("H3 模型、还没转写：两个标签，按钮写「转写为 H3」，默认停在原稿", () => {
    const html = render(studio({}), <PromptPane segment={seg()} />);
    expect(html).toContain("原稿 · Seedance");
    expect(html).toContain("转写 · H3");
    expect(html).toContain("转写为 H3");
    expect(html).toMatch(
      /aria-selected="true"[^>]*>(<span[^>]*><\/span>)?<span class="relative">原稿/,
    );
  });

  test("有转写稿但原稿改过：默认停在转写，标「已过期」并提示重新转写", () => {
    const segment = seg({
      adaptations: {
        h3: { prompt: "integrated", sourceHash: "old", edited: false, updatedAt: "" },
      },
    });
    const html = render(studio({ adaptStateOf: () => "stale" }), <PromptPane segment={segment} />);
    expect(html).toContain("已过期");
    expect(html).toContain("原稿已修改");
    expect(html).toContain("重新转写");
  });

  test("重新转写失败：转写标签里显示原因和重试，旧转写稿还在", () => {
    const segment = seg({
      adaptations: { h3: { prompt: "旧转写", sourceHash: "h", edited: false, updatedAt: "" } },
    });
    const html = render(
      studio({ adaptStateOf: () => "fresh", adaptErrors: { "s1:h3": "转写结果不合格，请重试" } }),
      <PromptPane segment={segment} />,
    );
    expect(html).toContain("转写结果不合格，请重试");
    expect(html).toContain(">重试<");
  });
});

describe("片场：生成按钮的拦截原因", () => {
  test("H3 模型还没转写：禁用并写明先转写", () => {
    const html = render(studio({ selected: seg() }), <GenerateControls />);
    expect(html).toContain("先把原稿转写为 H3");
  });

  test("正在转写：禁用并写明正在转写", () => {
    const html = render(
      studio({ selected: seg(), adaptStateOf: () => "busy" }),
      <GenerateControls />,
    );
    expect(html).toContain("正在转写");
  });

  test("时间轴超过模型生效时长或转写秒数不匹配时给出清晰提示且拦截越界", () => {
    const html = render(
      {
        format: "h3",
        assets: [],
        assetsStatus: "ready",
        adaptStateOf: () => "stale",
        adaptErrors: {},
        video: {
          models: [],
          status: "ready",
          setModel: () => {},
          model: {
            key: "vid-2s",
            kind: "video",
            label: "2s",
            pricing: {},
            capabilities: {
              ops: ["i2v"],
              refs: {
                image: { on: true, max: 9, max_mb: 10 },
                audio: { on: false, max: 0, max_mb: 0 },
                video: { on: false, max: 0, max_mb: 0 },
              },
              prompt: { max_length: 2000 },
              params: {
                duration: {
                  type: "number",
                  label: "时长",
                  open: false,
                  default: 2,
                  min: 2,
                  max: 15,
                },
              },
            },
          },
        } as unknown as Studio["video"],
        actions: { adapt: async () => true } as unknown as Studio["actions"],
      },
      <PromptPane
        segment={{
          ...seg,
          seconds: 5,
          adaptations: {
            h3: {
              prompt: "[Shot 2] At 00:04.000, cut",
              sourceHash: "h",
              seconds: 5,
              edited: false,
              updatedAt: "",
            },
          },
        }}
      />,
    );
    expect(html).toContain("4s");
    expect(html).toContain("2s");
    // 片段标 5s 而模型锁定 2s 时不直接给错误“按 5s 重转”按钮
    expect(html).toContain("alert");
  });
});
