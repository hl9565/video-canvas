import { describe, expect, test } from "bun:test";

import type { Capabilities, ModelInfo, ParamField } from "@/api/model/type";
import {
  durationChoices,
  effectiveDuration,
  fitDuration,
  modelMaxSeconds,
  modelSummary,
  pickVideoModel,
  segmentParams,
  videoModelSupport,
} from "@/utils/drama/video-models";

const caps = (patch: Partial<Capabilities> = {}): Capabilities => ({
  ops: ["t2v", "i2v", "omni"],
  refs: {
    image: { on: true, max: 9, max_mb: 10 },
    audio: { on: true, max: 3, max_mb: 10 },
    video: { on: false, max: 0, max_mb: 0 },
  },
  prompt: { max_length: 2000 },
  params: {
    duration: {
      type: "number",
      label: "时长",
      open: true,
      min: 4,
      max: 15,
      default: 5,
      unit: "秒",
    },
    resolution: {
      type: "enum",
      label: "分辨率",
      open: true,
      options: ["720P", "1080P"],
      default: "720P",
    },
    aspect_ratio: {
      type: "enum",
      label: "比例",
      open: true,
      options: ["Auto", "16:9", "9:16"],
      default: "Auto",
    },
  },
  ...patch,
});

const model = (key: string, c: Capabilities): ModelInfo => ({
  key,
  kind: "video",
  label: key,
  pricing: { billing: "per_second", per_second: 2 },
  capabilities: c,
});

describe("视频模型是否适合短剧片段：要能收参考图", () => {
  test("收参考图且支持 omni 或 i2v：可用", () => {
    expect(videoModelSupport(model("a", caps())).ok).toBe(true);
    expect(videoModelSupport(model("b", caps({ ops: ["i2v"] }))).ok).toBe(true);
  });

  test("不收参考图：置灰并写原因（原型 vmodel）", () => {
    const off = caps({
      refs: {
        image: { on: false, max: 0, max_mb: 0 },
        audio: { on: false, max: 0, max_mb: 0 },
        video: { on: false, max: 0, max_mb: 0 },
      },
    });
    const support = videoModelSupport(model("c", off));
    expect(support.ok).toBe(false);
    expect(support.reason).toContain("不支持参考图");
  });

  test("只有文生视频：不可用，原因说明没有图生 / 参考方式", () => {
    const support = videoModelSupport(model("d", caps({ ops: ["t2v"] })));
    expect(support.ok).toBe(false);
    expect(support.reason).toBeTruthy();
  });
});

describe("时长上限与适配", () => {
  test("数字参数取 max；枚举取最大的数字选项；没有时长参数为 null", () => {
    expect(modelMaxSeconds(caps())).toBe(15);
    const enumCaps = caps({
      params: {
        duration: { type: "enum", label: "时长", open: true, options: ["5", "10", 8], default: 5 },
      },
    });
    expect(modelMaxSeconds(enumCaps)).toBe(10);
    expect(modelMaxSeconds(caps({ params: {} }))).toBeNull();
  });

  test("fitDuration：数字夹在范围内，枚举取不小于它的最近选项（都比它小取最大）", () => {
    const num = caps().params!.duration as ParamField;
    expect(fitDuration(num, 12)).toEqual({ value: 12, adjusted: false });
    expect(fitDuration(num, 30)).toEqual({ value: 15, adjusted: true });
    expect(fitDuration(num, 2)).toEqual({ value: 4, adjusted: true });
    const en: ParamField = { type: "enum", label: "时长", open: true, options: [5, 10] };
    expect(fitDuration(en, 7)).toEqual({ value: 10, adjusted: true });
    expect(fitDuration(en, 5)).toEqual({ value: 5, adjusted: false });
    expect(fitDuration(en, 12)).toEqual({ value: 10, adjusted: true });
  });

  test("摘要：参考图上限、参考音频、时长上限", () => {
    expect(modelSummary(model("a", caps()))).toBe("参考图 ≤9 · 参考音频 · ≤15s");
    const noAudio = caps({
      refs: { ...caps().refs, audio: { on: false, max: 0, max_mb: 0 } },
    });
    expect(modelSummary(model("a", noAudio))).toBe("参考图 ≤9 · ≤15s");
  });

  test("摘要带提示词格式名放在最前，通用不显示", () => {
    expect(modelSummary(model("a", caps({ prompt_format: "seedance" })))).toBe(
      "Seedance · 参考图 ≤9 · 参考音频 · ≤15s",
    );
    expect(modelSummary(model("a", caps({ prompt_format: "h3" })))).toContain("MiniMax H3 · ");
    expect(modelSummary(model("a", caps({ prompt_format: "" })))).toBe(
      "参考图 ≤9 · 参考音频 · ≤15s",
    );
  });
});

describe("提交参数：时长跟片段、比例跟短剧", () => {
  test("duration 取片段时长（按模型适配），比例选项里有短剧比例就用，分辨率用用户选的", () => {
    const params = segmentParams(caps(), {
      seconds: 12,
      ratio: "9:16",
      overrides: { resolution: "1080P" },
    });
    expect(params).toMatchObject({ duration: 12, aspect_ratio: "9:16", resolution: "1080P" });
  });

  test("比例选项里没有短剧比例：不覆盖，按模型默认；没有的参数不乱加", () => {
    const params = segmentParams(caps(), { seconds: 20, ratio: "21:9", overrides: {} });
    expect(params.duration).toBe(15);
    expect(params.aspect_ratio).toBeUndefined();
    const bare = segmentParams(caps({ params: {} }), { seconds: 8, ratio: "9:16", overrides: {} });
    expect(bare).toEqual({});
  });

  test("未开放、小数或越界 overrides 不会被下发（跨模型切回来也不串味）", () => {
    const withSeed = caps({
      params: {
        ...caps().params,
        seed: {
          type: "number",
          label: "随机种子",
          open: true,
          min: -1,
          max: 4_294_967_295,
          step: 1,
          default: 3_916_315_536,
        },
        locked: { type: "number", label: "锁定", open: false, default: 7 },
      },
    });
    expect(
      segmentParams(withSeed, {
        seconds: 5,
        ratio: "9:16",
        overrides: { seed: -1, locked: 99 },
      }),
    ).toEqual({ duration: 5, aspect_ratio: "9:16", seed: -1 });
    expect(
      segmentParams(withSeed, {
        seconds: 5,
        ratio: "9:16",
        overrides: { seed: 4_294_967_296 },
      }).seed,
    ).toBeUndefined();
    expect(
      segmentParams(withSeed, {
        seconds: 5,
        ratio: "9:16",
        overrides: { seed: 1.8 },
      }).seed,
    ).toBeUndefined();
  });
});

describe("默认模型", () => {
  const list = [
    model("nope", caps({ ops: ["t2v"] })),
    model("first", caps()),
    model("second", caps()),
  ];

  test("没存过选择：取第一个可用的；存过且仍可用：沿用；存的已不可用：回到第一个可用", () => {
    expect(pickVideoModel(list, "")?.key).toBe("first");
    expect(pickVideoModel(list, "second")?.key).toBe("second");
    expect(pickVideoModel(list, "nope")?.key).toBe("first");
    expect(pickVideoModel(list, "gone")?.key).toBe("first");
    expect(pickVideoModel([list[0]], "")).toBeUndefined();
  });

  test("回退顺序：本机偏好 > 项目建剧时选的 > 第一个可用；项目的模型不可用 / 下线也回到第一个", () => {
    expect(pickVideoModel(list, "", "second")?.key).toBe("second");
    expect(pickVideoModel(list, "first", "second")?.key).toBe("first");
    expect(pickVideoModel(list, "gone", "second")?.key).toBe("second");
    expect(pickVideoModel(list, "", "nope")?.key).toBe("first");
    expect(pickVideoModel(list, "", "gone")?.key).toBe("first");
  });
});

describe("2s、未开放锁定与非整除 step 时长", () => {
  test("fitDuration / effectiveDuration 处理缺失、open:false 与 step", () => {
    expect(fitDuration(undefined, 2)).toEqual({ value: 2, adjusted: false });

    const lockedCaps = caps({
      params: {
        duration: { type: "number", label: "时长", open: false, default: 2, min: 2, max: 15 },
      },
    });
    expect(effectiveDuration(lockedCaps, 5)).toEqual({ value: 2, adjusted: true });
    expect(effectiveDuration(lockedCaps, 2)).toEqual({ value: 2, adjusted: false });
    expect(modelMaxSeconds(lockedCaps)).toBe(2);

    const steppedCaps = caps({
      params: {
        duration: {
          type: "number",
          label: "时长",
          open: true,
          min: 2,
          max: 15,
          step: 4,
          default: 6,
        },
      },
    });
    // max=15 不在 step=4 网格上，真实最大可达为 2 + floor(13/4)*4 = 14
    expect(modelMaxSeconds(steppedCaps)).toBe(14);
    expect(effectiveDuration(steppedCaps, 5)).toEqual({ value: 6, adjusted: true });
    expect(effectiveDuration(steppedCaps, 15)).toEqual({ value: 14, adjusted: true });
    expect(effectiveDuration(steppedCaps, 2)).toEqual({ value: 2, adjusted: false });

    // 未开放时 segmentParams 不下发可控参数，但 effectiveDuration 仍返回锁定值
    expect(segmentParams(lockedCaps, { seconds: 5, ratio: "16:9", overrides: {} })).toEqual({});
  });

  test("durationChoices 展示真实可选秒数（含 2s，不被白名单截断）", () => {
    const fullCaps = caps({
      params: {
        duration: {
          type: "number",
          label: "时长",
          open: true,
          min: 2,
          max: 15,
          step: 1,
          default: 5,
        },
      },
    });
    expect(durationChoices(fullCaps, 5)).toEqual([2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15]);

    const enumCaps = caps({
      params: {
        duration: { type: "enum", label: "时长", open: true, options: ["2", "5"], default: "5" },
      },
    });
    expect(durationChoices(enumCaps, 4)).toEqual([2, 4, 5]);
    expect(durationChoices(undefined, 2)).toContain(2);
  });
});
