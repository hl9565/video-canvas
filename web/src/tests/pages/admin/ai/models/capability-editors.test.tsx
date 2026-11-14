import { afterAll, beforeAll, describe, expect, test } from "bun:test";

import { ParamRowEditor } from "@/pages/admin/ai/models/capability-editors";

import { installDom, type DomEnv } from "../../../../helpers/dom";

let dom: DomEnv;

beforeAll(async () => {
  dom = await installDom();
});

afterAll(() => {
  dom.restore();
});

describe("ParamRowEditor：seed 专用整数域与防小数截断", () => {
  test("seed 允许 [-1, 4294967295]、固定 step=1、隐藏滑轨且不截断 1.8", async () => {
    const updates: unknown[] = [];
    const mounted = await dom.mount(
      <ParamRowEditor
        name="seed"
        field={{
          type: "number",
          label: "随机种子",
          open: true,
          min: -1,
          max: 4_294_967_295,
          default: 3_916_315_536,
          step: 1,
        }}
        first
        last
        canSpec={false}
        onChange={(next) => updates.push(next)}
        onMove={() => {}}
        onRemove={() => {}}
      />,
    );

    try {
      expect(mounted.container.textContent).toContain("允许范围 -1 – 4294967295（-1 为随机）");
      expect(mounted.container.textContent).toContain("步长固定为 1");
      expect(mounted.container.textContent).not.toContain("最短");

      const stepInput = mounted.container.querySelector(
        'input[aria-label="随机种子步长"]',
      ) as HTMLInputElement;
      expect(stepInput.disabled).toBe(true);
      expect(stepInput.value).toBe("1");

      const defInput = mounted.container.querySelector(
        'input[aria-label="随机种子默认"]',
      ) as HTMLInputElement;
      expect(defInput.getAttribute("aria-invalid")).toBe("false");
      expect(defInput.value).toBe("3916315536");

      await dom.type(defInput, "1.8");
      expect(defInput.value).toBe("1.8");
      expect(defInput.getAttribute("aria-invalid")).toBe("true");
      expect(mounted.container.textContent).toContain("请输入整数");
      expect(updates).toEqual([]);

      await dom.type(defInput, "4294967295");
      expect(updates.at(-1)).toMatchObject({ default: 4_294_967_295 });
    } finally {
      await mounted.unmount();
    }
  });

  test("普通 number 参数保留 1..3600 范围与滑轨", async () => {
    const mounted = await dom.mount(
      <ParamRowEditor
        name="duration"
        field={{ type: "number", label: "时长", open: true, min: 2, max: 15, default: 5, step: 1 }}
        first
        last
        canSpec={false}
        onChange={() => {}}
        onMove={() => {}}
        onRemove={() => {}}
      />,
    );

    try {
      expect(mounted.container.textContent).toContain("最短 2");
      expect(mounted.container.textContent).toContain("最长 15");
      const stepInput = mounted.container.querySelector(
        'input[aria-label="时长步长"]',
      ) as HTMLInputElement;
      expect(stepInput.disabled).toBe(false);
    } finally {
      await mounted.unmount();
    }
  });
});
