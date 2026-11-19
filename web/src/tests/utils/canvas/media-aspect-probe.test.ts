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

import { installDom, type DomEnv } from "@/tests/helpers/dom";
import { probeMediaAspect } from "@/utils/canvas/media-aspect";

const VIDEO = "/files/abc/preview.mp4";
const IMAGE = "/files/abc/preview.png";

class ProbeImage {
  static requests: { image: ProbeImage; src: string }[] = [];
  onload?: () => void;
  onerror?: () => void;
  naturalWidth = 0;
  naturalHeight = 0;

  set src(src: string) {
    ProbeImage.requests.push({ image: this, src });
  }

  load(width: number, height: number) {
    this.naturalWidth = width;
    this.naturalHeight = height;
    this.onload?.();
  }
}

describe("probeMediaAspect 媒体请求", () => {
  let dom: DomEnv;
  let savedImage: PropertyDescriptor | undefined;

  beforeAll(async () => {
    dom = await installDom();
    savedImage = Object.getOwnPropertyDescriptor(globalThis, "Image");
    Object.defineProperty(globalThis, "Image", { configurable: true, value: ProbeImage });
  });
  beforeEach(() => {
    ProbeImage.requests = [];
  });
  afterEach(() => {
    ProbeImage.requests = [];
  });
  afterAll(() => {
    if (savedImage) Object.defineProperty(globalThis, "Image", savedImage);
    else Reflect.deleteProperty(globalThis, "Image");
    dom.restore();
  });

  const sources = () => ProbeImage.requests.map(({ src }) => src);

  test("站内视频通过严格封面拿到画幅", async () => {
    const result = probeMediaAspect(VIDEO, "video");
    expect(sources()).toEqual([`${VIDEO}?v=poster&pv=2`]);
    ProbeImage.requests[0].image.load(400, 300);
    expect(await result).toBe(4 / 3);
  });

  test.each(["请求失败", "没有尺寸"])(
    "站内视频封面%s，返回 null 且不创建 video",
    async (reason) => {
      const createElement = spyOn(dom.win.document, "createElement");
      try {
        const result = probeMediaAspect(VIDEO, "video");
        const [{ image }] = ProbeImage.requests;
        if (reason === "请求失败") image.onerror?.();
        else image.load(0, 300);
        await Promise.resolve();
        expect(createElement.mock.calls.map(([tag]) => tag)).not.toContain("video");
        expect(await result).toBeNull();
        expect(sources()).toEqual([`${VIDEO}?v=poster&pv=2`]);
      } finally {
        createElement.mockRestore();
      }
    },
  );

  test.each(["https://cdn.example.com/preview.mp4", "blob:http://localhost/video"])(
    "无变体的视频仍读元数据：%s",
    async (src) => {
      const createElement = spyOn(dom.win.document, "createElement");
      const load = spyOn(dom.win.HTMLMediaElement.prototype, "load").mockImplementation(() => {});
      try {
        const result = probeMediaAspect(src, "video");
        expect(sources()).toEqual([]);
        expect(createElement.mock.calls).toEqual([["video"]]);
        const video = createElement.mock.results[0]?.value;
        if (!(video instanceof dom.win.HTMLVideoElement)) throw new Error("未创建 video");
        expect(video.getAttribute("src")).toBe(src);
        expect(video.preload).toBe("metadata");
        Object.defineProperties(video, {
          videoWidth: { value: 1920 },
          videoHeight: { value: 1080 },
        });
        video.dispatchEvent(new dom.win.Event("loadedmetadata"));
        expect(await result).toBe(16 / 9);
      } finally {
        createElement.mockRestore();
        load.mockRestore();
      }
    },
  );

  test("严格图片缩略图失败后仍会探测原图", async () => {
    const result = probeMediaAspect(IMAGE, "image");
    expect(sources()).toEqual([`${IMAGE}?v=thumb&pv=2`]);
    ProbeImage.requests[0].image.onerror?.();
    await Promise.resolve();
    expect(sources()).toEqual([`${IMAGE}?v=thumb&pv=2`, IMAGE]);
    ProbeImage.requests[1].image.load(900, 1200);
    expect(await result).toBe(3 / 4);
  });
});
