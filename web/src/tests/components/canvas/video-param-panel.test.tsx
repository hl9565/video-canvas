import { afterAll, beforeAll, describe, expect, mock, test } from "bun:test";

import type { Capabilities } from "@/api/model/type";
import { VideoParamPanel } from "@/components/canvas/video-param-panel";
import { emptyBindings } from "@/utils/tasks/capabilities";

import { installDom, type DomEnv } from "../../helpers/dom";

let dom: DomEnv;

beforeAll(async () => {
  dom = await installDom();
});

afterAll(() => {
  dom.restore();
});

const seedCaps = (seedField: NonNullable<Capabilities["params"]>["seed"]): Capabilities => ({
  ops: ["t2v"],
  refs: {
    image: { on: false, max: 0, max_mb: 0 },
    video: { on: false, max: 0, max_mb: 0 },
    audio: { on: false, max: 0, max_mb: 0 },
  },
  prompt: { max_length: 500 },
  params: {
    seed: seedField,
  },
});

const defaultSeed = {
  type: "number" as const,
  label: "随机种子",
  open: true,
  min: -1,
  max: 4_294_967_295,
  step: 1,
  default: 3_916_315_536,
};

describe("VideoParamPanel：seed 大范围整数与恢复默认 / 随机", () => {
  test("隐藏无意义滑轨，清晰显示模型默认，输入 1.8 不截断并标红，且区分恢复默认与随机 (-1)", async () => {
    const changes: Array<[string, unknown]> = [];
    const onChange = mock((name: string, value: unknown) => {
      changes.push([name, value]);
    });

    const first = await dom.mount(
      <VideoParamPanel
        section="params"
        caps={seedCaps(defaultSeed)}
        params={{}}
        bindings={emptyBindings()}
        errors={{}}
        showErrors={false}
        onChange={onChange}
        onAddRef={() => {}}
        onRemoveRef={() => {}}
        listAssets={() => []}
      />,
    );

    try {
      expect(first.container.querySelector('input[type="range"]')).toBeNull();
      expect(first.container.textContent).toContain("当前按模型默认：3916315536");
      expect(first.container.textContent).not.toContain("恢复默认");
      expect(first.container.textContent).toContain("随机 (-1)");
      expect(first.container.textContent).toContain("范围 -1 – 4294967295（-1 为随机）");

      const input = first.container.querySelector(
        'input[aria-label="随机种子（数值）"]',
      ) as HTMLInputElement;
      expect(input.value).toBe("3916315536");

      await dom.type(input, "1.8");
      expect(input.value).toBe("1.8");
      expect(input.getAttribute("aria-invalid")).toBe("true");
      expect(first.container.textContent).toContain("随机种子需要是整数");
      expect(changes).toEqual([]);

      await dom.type(input, "4294967295");
      expect(changes.at(-1)).toEqual(["seed", 4_294_967_295]);

      const randomBtn = Array.from(first.container.querySelectorAll("button")).find(
        (btn) => btn.textContent === "随机 (-1)",
      );
      await dom.click(randomBtn);
      expect(changes.at(-1)).toEqual(["seed", -1]);
    } finally {
      await first.unmount();
    }

    const overridden = await dom.mount(
      <VideoParamPanel
        section="params"
        caps={seedCaps(defaultSeed)}
        params={{ seed: 12345 }}
        bindings={emptyBindings()}
        errors={{}}
        showErrors={false}
        onChange={onChange}
        onAddRef={() => {}}
        onRemoveRef={() => {}}
        listAssets={() => []}
      />,
    );

    try {
      expect(overridden.container.textContent).toContain("当前覆盖：12345");
      const resetBtn = Array.from(overridden.container.querySelectorAll("button")).find(
        (btn) => btn.textContent === "恢复默认",
      );
      expect(resetBtn).toBeDefined();
      await dom.click(resetBtn);
      expect(changes.at(-1)).toEqual(["seed", undefined]);
    } finally {
      await overridden.unmount();
    }
  });

  test("不允许 -1 的 seed 模型不显示随机 (-1) 按钮和提示", async () => {
    const mounted = await dom.mount(
      <VideoParamPanel
        section="params"
        caps={seedCaps({
          type: "number",
          label: "随机种子",
          open: true,
          min: 0,
          max: 4_294_967_295,
          step: 1,
          default: 42,
        })}
        params={{}}
        bindings={emptyBindings()}
        errors={{}}
        showErrors={false}
        onChange={() => {}}
        onAddRef={() => {}}
        onRemoveRef={() => {}}
        listAssets={() => []}
      />,
    );

    try {
      expect(mounted.container.textContent).not.toContain("随机 (-1)");
      expect(mounted.container.textContent).not.toContain("（-1 为随机）");
      expect(mounted.container.textContent).toContain("范围 0 – 4294967295");
    } finally {
      await mounted.unmount();
    }
  });
});
