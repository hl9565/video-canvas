import { afterAll, beforeAll, describe, expect, test } from "bun:test";

import type { ModelInfo } from "@/api/model/type";
import { ParamsPopover } from "@/components/home/composer-params";
import { useComposerStore } from "@/store/composer";
import { paramSummary } from "@/utils/tasks/capabilities";

import { installDom, type DomEnv } from "../../helpers/dom";

let dom: DomEnv;

beforeAll(async () => {
  dom = await installDom();
});

afterAll(() => {
  dom.restore();
});

const seedModel: ModelInfo = {
  key: "h3-test",
  kind: "video",
  label: "H3 Test",
  pricing: { billing: "unit", unit: 1 },
  capabilities: {
    ops: ["t2v"],
    refs: {
      image: { on: false, max: 0, max_mb: 0 },
      video: { on: false, max: 0, max_mb: 0 },
      audio: { on: false, max: 0, max_mb: 0 },
    },
    prompt: { max_length: 500 },
    params: {
      duration: {
        type: "enum",
        label: "时长",
        open: true,
        options: [2, 5],
        default: 5,
        unit: "秒",
      },
      seed: {
        type: "number",
        label: "随机种子",
        open: true,
        min: -1,
        max: 4_294_967_295,
        step: 1,
        default: -1,
      },
    },
  },
};

describe("Home Composer 参数状态与 ParamsPopover", () => {
  test("setParam(undefined) 清理覆盖键并在空对象时移除模型桶", () => {
    useComposerStore.setState({ paramsByModel: {} });
    const store = useComposerStore.getState();
    store.setParam("m1", "seed", 3_916_315_536);
    expect(useComposerStore.getState().paramsByModel.m1).toEqual({ seed: 3_916_315_536 });

    store.setParam("m1", "seed", undefined);
    expect("seed" in (useComposerStore.getState().paramsByModel.m1 ?? {})).toBe(false);
    expect(useComposerStore.getState().paramsByModel.m1).toBeUndefined();
  });

  test("paramSummary 对 seed=-1 与固定整数摘要友好", () => {
    expect(paramSummary(seedModel.capabilities, {})).toBe("5秒 · 随机种子");
    expect(paramSummary(seedModel.capabilities, { seed: 3_916_315_536 })).toBe(
      "5秒 · 种子 3916315536",
    );
  });

  test("ParamsPopover 对越界覆盖立即在触发按钮与弹层标红", async () => {
    const mounted = await dom.mount(
      <ParamsPopover
        model={seedModel}
        params={{ seed: 4_294_967_296 }}
        refs={[]}
        onChange={() => {}}
      />,
    );

    try {
      const trigger = mounted.container.querySelector(
        'button[aria-label="生成参数"]',
      ) as HTMLElement;
      expect(trigger.className).toContain("text-destructive");
      await dom.click(trigger);
      await dom.flush();
      expect(dom.win.document.body.textContent).toContain("随机种子不能大于 4294967295");
    } finally {
      await mounted.unmount();
    }
  });
});
