import { describe, expect, test } from "bun:test";

import * as mediaLod from "@/utils/canvas/media-lod";
import {
  LOD_DOWN_ZOOM,
  LOD_UP_ZOOM,
  createVideoSlotPool,
  formatMediaDuration,
  nextLowDetail,
  variantUrl,
} from "@/utils/canvas/media-lod";

describe("variantUrl：给 /files/ 地址追加变体参数", () => {
  test("相对路径追加 v=thumb", () => {
    expect(variantUrl("/files/a/b.png", "thumb")).toEqual({
      url: "/files/a/b.png?v=thumb",
      hasVariant: true,
    });
  });

  test("绝对地址保留域名，追加 v=poster", () => {
    expect(variantUrl("https://cdn.example.com/files/x.mp4", "poster")).toEqual({
      url: "https://cdn.example.com/files/x.mp4?v=poster",
      hasVariant: true,
    });
  });

  test("保留已有 query，已有 v 被替换而不是重复", () => {
    expect(variantUrl("/files/x.png?sig=1", "thumb").url).toBe("/files/x.png?sig=1&v=thumb");
    expect(variantUrl("/files/x.png?v=poster&sig=1", "thumb").url).toBe(
      "/files/x.png?sig=1&v=thumb",
    );
  });

  test("保留 hash", () => {
    expect(variantUrl("/files/x.png#a", "thumb").url).toBe("/files/x.png?v=thumb#a");
  });

  test("严格模式追加 pv=2，并替换旧版本和变体参数，保留其余 query 与 hash", () => {
    expect(variantUrl("/files/a/b.png", "thumb", { strict: true })).toEqual({
      url: "/files/a/b.png?v=thumb&pv=2",
      hasVariant: true,
    });
    expect(
      variantUrl("https://cdn.example.com/files/x.mp4?pv=1&sig=a%2Fb&v=thumb&pv=0#t=2", "poster", {
        strict: true,
      }).url,
    ).toBe("https://cdn.example.com/files/x.mp4?sig=a%2Fb&v=poster&pv=2#t=2");
  });

  test("strict: false 保留兼容路径，不添加或替换 pv", () => {
    expect(variantUrl("/files/x.png", "thumb", { strict: false }).url).toBe("/files/x.png?v=thumb");
    const src = "/files/x.png?pv=1&v=poster#ref";
    expect(variantUrl(src, "thumb", { strict: false }).url).toBe("/files/x.png?pv=1&v=thumb#ref");
    expect(variantUrl(src, "thumb").url).toBe("/files/x.png?pv=1&v=thumb#ref");
  });

  test("严格模式也替换编码过的 v 和 pv，不重写其他参数", () => {
    expect(
      variantUrl("/files/x.png?sig=a+b&&%76=poster&p%76=1&flag#ref", "thumb", { strict: true }).url,
    ).toBe("/files/x.png?sig=a+b&flag&v=thumb&pv=2#ref");
  });

  test("非 /files/ 地址原样返回并标记无变体", () => {
    for (const src of [
      "https://other.com/a.png",
      "blob:http://localhost/123",
      "data:image/png;base64,AAA",
      "/static/files/a.png",
      "",
    ]) {
      expect(variantUrl(src, "thumb")).toEqual({ url: src, hasVariant: false });
      expect(variantUrl(src, "thumb", { strict: true })).toEqual({ url: src, hasVariant: false });
    }
  });
});

describe("resolveImageLayers：图片先等缩略图，再按需挂原图", () => {
  test.each([
    ["无变体直接加载原图", false, "pending", true, false, false, true, true],
    ["缩略图未完成时只挂缩略图", true, "pending", true, false, true, false, false],
    ["高清模式也先等缩略图", true, "pending", false, false, true, false, false],
    ["缩略图未完成时也保留已发起的请求", true, "pending", false, true, true, true, false],
    ["低清模式不请求原图", true, "loaded", true, false, true, false, false],
    ["高清模式开始请求原图", true, "loaded", false, false, true, true, true],
    ["切回低清模式不卸载原图", true, "loaded", true, true, true, true, false],
    ["高清模式保留已发起的原图", true, "loaded", false, true, true, true, true],
    ["缩略图失败后低清模式也回退", true, "failed", true, false, false, true, true],
    ["缩略图失败后高清模式直接回退", true, "failed", false, false, false, true, true],
    ["缩略图失败后保留已启动的原图", true, "failed", true, true, false, true, true],
  ] as const)(
    "%s",
    (
      _name,
      hasVariant,
      thumbState,
      preferThumb,
      originalStarted,
      showThumb,
      showOriginal,
      startOriginal,
    ) => {
      expect(
        mediaLod.resolveImageLayers({ hasVariant, thumbState, preferThumb, originalStarted }),
      ).toEqual({
        showThumb,
        showOriginal,
        startOriginal,
      });
    },
  );
});

describe("nextLowDetail：缩放滞回", () => {
  test("阈值常量符合设计", () => {
    expect(LOD_DOWN_ZOOM).toBe(0.55);
    expect(LOD_UP_ZOOM).toBe(0.65);
  });

  test("高清态下降到 0.55 以下才降级，0.55 到 0.65 之间保持", () => {
    expect(nextLowDetail(false, 0.6)).toBe(false);
    expect(nextLowDetail(false, 0.55)).toBe(false);
    expect(nextLowDetail(false, 0.549)).toBe(true);
  });

  test("低清态升到 0.65 及以上才升级", () => {
    expect(nextLowDetail(true, 0.6)).toBe(true);
    expect(nextLowDetail(true, 0.649)).toBe(true);
    expect(nextLowDetail(true, 0.65)).toBe(false);
  });

  test("在阈值 0.6 附近来回抖动不会翻转", () => {
    let state = false;
    for (const z of [0.61, 0.59, 0.6, 0.57, 0.62, 0.58]) state = nextLowDetail(state, z);
    expect(state).toBe(false);
  });
});

type Fake = { playing: boolean; released: boolean };
const fake = (playing: boolean): [Fake, { isPlaying: () => boolean; release: () => void }] => {
  const state = { playing, released: false };
  return [
    state,
    {
      isPlaying: () => state.playing,
      release: () => {
        state.released = true;
      },
    },
  ];
};

describe("createVideoSlotPool：视频并发上限", () => {
  test("未满时直接放行", () => {
    const pool = createVideoSlotPool(2);
    pool.acquire("a", fake(false)[1]);
    pool.acquire("b", fake(false)[1]);
    expect(pool.size()).toBe(2);
  });

  test("满了先把已暂停的退回封面，播放中的不打断", () => {
    const pool = createVideoSlotPool(2);
    const [a, ha] = fake(true);
    const [b, hb] = fake(false);
    pool.acquire("a", ha);
    pool.acquire("b", hb);
    pool.acquire("c", fake(true)[1]);
    expect(b.released).toBe(true);
    expect(a.released).toBe(false);
    expect(pool.size()).toBe(2);
  });

  test("全在播放时允许超限，不打断任何一个", () => {
    const pool = createVideoSlotPool(2);
    const [a, ha] = fake(true);
    const [b, hb] = fake(true);
    pool.acquire("a", ha);
    pool.acquire("b", hb);
    pool.acquire("c", fake(true)[1]);
    expect(a.released || b.released).toBe(false);
    expect(pool.size()).toBe(3);
  });

  test("release 归还名额，重复 acquire 同一个 id 不重复计数", () => {
    const pool = createVideoSlotPool(4);
    const handle = fake(false)[1];
    pool.acquire("a", handle);
    pool.acquire("a", handle);
    expect(pool.size()).toBe(1);
    pool.release("a");
    expect(pool.size()).toBe(0);
  });

  test("默认上限为 4", () => {
    const pool = createVideoSlotPool();
    const paused = Array.from({ length: 4 }, () => fake(false));
    paused.forEach(([, h], i) => pool.acquire(`v${i}`, h));
    pool.acquire("new", fake(true)[1]);
    expect(paused.filter(([s]) => s.released).length).toBe(1);
  });
});

describe("formatMediaDuration", () => {
  test("毫秒转 m:ss，超过一小时带小时", () => {
    expect(formatMediaDuration(5000)).toBe("0:05");
    expect(formatMediaDuration(65_400)).toBe("1:05");
    expect(formatMediaDuration(3_725_000)).toBe("1:02:05");
  });

  test("没有时长或非法值返回 null", () => {
    expect(formatMediaDuration(null)).toBeNull();
    expect(formatMediaDuration(undefined)).toBeNull();
    expect(formatMediaDuration(0)).toBeNull();
    expect(formatMediaDuration(Number.NaN)).toBeNull();
  });
});
