import { describe, expect, test } from "bun:test";

import { formatTimelineSeconds, timelineOverrun } from "@/utils/drama/timeline";

describe("timelineOverrun（与后端 TimelineOverrun 同源）", () => {
  test("拦截 2s 下超出时长的中文与英文时间区间", () => {
    expect(timelineOverrun("0-4秒：主角推门", 2)).toEqual({ overrun: true, end: 4 });
    expect(timelineOverrun("0~3.5s：特写镜头", 2)).toEqual({ overrun: true, end: 3.5 });
    expect(timelineOverrun("0-2.5至3.0s", 2)).toEqual({ overrun: true, end: 3 });
  });

  test("拦截 H3 切点时间（At MM:SS.mmm）", () => {
    const prompt = "Shot 1.\n[Shot 2] At 00:04.000, the camera cuts to a close-up.";
    expect(timelineOverrun(prompt, 2)).toEqual({ overrun: true, end: 4 });
    expect(timelineOverrun("[Shot 2] At 00:01.900, cut.", 2)).toEqual({
      overrun: false,
      end: 1.9,
    });
  });

  test("豁免监控时钟写法、非秒单位字母后缀及 1ms 容差", () => {
    expect(timelineOverrun("监控画面 00:10-00:20 24-60fps，0-2s：特写", 2)).toEqual({
      overrun: false,
      end: 2,
    });
    expect(timelineOverrun("0-5sec 里出现 00:03-00:05 字幕", 2)).toEqual({
      overrun: false,
      end: 0,
    });
    expect(timelineOverrun("0-2.001秒：卡点", 2)).toEqual({ overrun: false, end: 2.001 });
  });

  test("formatTimelineSeconds 去掉无意义小数尾零", () => {
    expect(formatTimelineSeconds(4)).toBe("4");
    expect(formatTimelineSeconds(3.5)).toBe("3.5");
  });
});
