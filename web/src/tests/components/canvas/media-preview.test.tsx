import { afterAll, afterEach, beforeAll, describe, expect, test } from "bun:test";
import { ReactFlowProvider, useStoreApi } from "@xyflow/react";
import { renderToStaticMarkup } from "react-dom/server";

import { MediaPreview, VideoPoster } from "@/components/canvas/media-preview";
import { installDom, type DomEnv } from "@/tests/helpers/dom";

const SRC = "/files/abc/a.png";
const THUMB = `${SRC}?v=thumb&pv=2`;

const markupSources = (out: string) =>
  [...out.matchAll(/<img[^>]*\ssrc="([^"]*)"/g)].map((match) => match[1]);

describe("MediaPreview 初始渲染", () => {
  test("full 模式只挂严格缩略图，不提前挂原图", () => {
    const out = renderToStaticMarkup(<MediaPreview src={SRC} mode="full" />);
    expect(markupSources(out)).toEqual([`${SRC}?v=thumb&amp;pv=2`]);
    expect(out).not.toContain("<video");
  });

  test("两层都没加载出来时盖着灰色扫光占位", () => {
    const out = renderToStaticMarkup(<MediaPreview src={SRC} mode="full" />);
    expect(out).toContain('data-slot="media-skeleton"');
  });

  test("thumb 模式只请求缩略图，不叠原图", () => {
    const out = renderToStaticMarkup(<MediaPreview src={SRC} mode="thumb" />);
    expect(markupSources(out)).toEqual([`${SRC}?v=thumb&amp;pv=2`]);
  });

  test("外链没有缩略图变体，直接看原图", () => {
    const out = renderToStaticMarkup(
      <MediaPreview src="https://cdn.example.com/a.png" mode="full" />,
    );
    expect(markupSources(out)).toEqual(["https://cdn.example.com/a.png"]);
  });
});

function ZoomControls() {
  const store = useStoreApi();
  return (
    <>
      <button onClick={() => store.setState({ transform: [0, 0, 0.5] })}>缩小</button>
      <button onClick={() => store.setState({ transform: [0, 0, 0.7] })}>放大</button>
      <MediaPreview src={SRC} mode="lod" />
    </>
  );
}

describe("MediaPreview 与 VideoPoster 的 DOM 加载顺序", () => {
  let dom: DomEnv;
  let mounted: Awaited<ReturnType<DomEnv["mount"]>> | undefined;

  beforeAll(async () => {
    dom = await installDom();
  });
  afterEach(async () => {
    await mounted?.unmount();
    mounted?.container.remove();
    mounted = undefined;
  });
  afterAll(() => dom.restore());

  async function render(element: React.ReactElement) {
    mounted = await dom.mount(element);
    return mounted.container;
  }

  function sources(container: HTMLElement) {
    return [...container.querySelectorAll("img")].map((img) => img.getAttribute("src"));
  }

  function image(container: HTMLElement, src: string) {
    const img = container.querySelector<HTMLImageElement>(`img[src="${src}"]`);
    if (!img) throw new Error(`图片未挂载：${src}`);
    return img;
  }

  async function dispatch(img: HTMLImageElement, event: "load" | "error") {
    await dom.act(async () => {
      img.dispatchEvent(new dom.win.Event(event));
    });
  }

  test("full：缩略图加载后才挂原图，等原图加载完再显示", async () => {
    const container = await render(<MediaPreview src={SRC} mode="full" />);
    expect(sources(container)).toEqual([THUMB]);
    const thumb = image(container, THUMB);
    await dispatch(thumb, "load");
    expect(sources(container)).toEqual([THUMB, SRC]);
    const original = image(container, SRC);
    expect(original.style.opacity).toBe("0");
    expect(thumb.style.opacity).toBe("1");
    await dispatch(original, "load");
    expect(original.style.opacity).toBe("1");
    expect(thumb.style.opacity).toBe("0");
    expect(image(container, THUMB)).toBe(thumb);
  });

  test.each(["full", "thumb"] as const)("%s：缩略图失败后只挂一次原图", async (mode) => {
    const container = await render(<MediaPreview src={SRC} mode={mode} alt="预览" />);
    expect(sources(container)).toEqual([THUMB]);
    await dispatch(image(container, THUMB), "error");
    expect(sources(container)).toEqual([SRC]);
    const original = image(container, SRC);
    await dispatch(original, "load");
    expect(image(container, SRC)).toBe(original);
    expect(original.alt).toBe("预览");
  });

  test("原图失败仍显示已加载的缩略图，不显示破图", async () => {
    const container = await render(<MediaPreview src={SRC} mode="full" />);
    const thumb = image(container, THUMB);
    await dispatch(thumb, "load");
    const original = image(container, SRC);
    await dispatch(original, "error");
    expect(image(container, THUMB)).toBe(thumb);
    expect(thumb.style.opacity).toBe("1");
    expect(original.style.opacity).toBe("0");
    expect(container.querySelector('[role="img"]')).toBeNull();
  });

  test("缩略图和原图均失败才显示破图", async () => {
    const container = await render(<MediaPreview src={SRC} mode="full" />);
    await dispatch(image(container, THUMB), "error");
    await dispatch(image(container, SRC), "error");
    expect(sources(container)).toEqual([]);
    expect(container.querySelector('[aria-label="图片加载失败"]')).not.toBeNull();
  });

  test("thumb：缩略图加载后不挂原图", async () => {
    const container = await render(<MediaPreview src={SRC} mode="thumb" />);
    await dispatch(image(container, THUMB), "load");
    expect(sources(container)).toEqual([THUMB]);
  });

  test("lod：跨过滞回阈值保留已开始加载的原图，低清时不显示它", async () => {
    const container = await render(
      <ReactFlowProvider>
        <ZoomControls />
      </ReactFlowProvider>,
    );
    expect(sources(container)).toEqual([THUMB]);
    await dispatch(image(container, THUMB), "load");
    const original = image(container, SRC);
    const [zoomOut, zoomIn] = container.querySelectorAll("button");
    await dom.click(zoomOut);
    expect(image(container, SRC)).toBe(original);
    await dispatch(original, "load");
    expect(original.style.opacity).toBe("0");
    await dom.click(zoomIn);
    expect(image(container, SRC)).toBe(original);
    expect(original.style.opacity).toBe("1");
  });

  test("命中已加载的缓存时，无需再次等 load 事件就可升级原图", async () => {
    const prototype = dom.win.HTMLImageElement.prototype;
    const descriptors = ["complete", "naturalWidth"] as const;
    const saved = Object.getOwnPropertyDescriptors(prototype);
    Object.defineProperties(prototype, {
      complete: { configurable: true, get: () => true },
      naturalWidth: { configurable: true, get: () => 800 },
    });
    try {
      const container = await render(<MediaPreview src={SRC} mode="full" />);
      expect(sources(container)).toEqual([THUMB, SRC]);
      expect(image(container, SRC).style.opacity).toBe("1");
    } finally {
      for (const key of descriptors) Object.defineProperty(prototype, key, saved[key]);
    }
  });

  test("严格视频封面失败后结束等待并显示图标，从不挂 video", async () => {
    const video = "/files/abc/a.mp4";
    const settled: boolean[] = [];
    const container = await render(
      <VideoPoster src={video} onSettledChange={(value) => settled.push(value)} />,
    );
    expect(sources(container)).toEqual([`${video}?v=poster&pv=2`]);
    expect(container.querySelector('[data-slot="media-skeleton"]')).not.toBeNull();
    expect(settled).toEqual([false]);
    expect(container.querySelector("video")).toBeNull();
    await dispatch(image(container, `${video}?v=poster&pv=2`), "error");
    expect(sources(container)).toEqual([]);
    expect(container.querySelector("svg")).not.toBeNull();
    expect(settled).toEqual([false, true]);
    expect(container.querySelector("video")).toBeNull();
  });
});
