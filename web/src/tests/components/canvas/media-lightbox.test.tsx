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

import { MediaLightbox } from "@/components/canvas/media-lightbox";
import { TooltipProvider } from "@/components/ui/tooltip";
import { installDom, type DomEnv } from "@/tests/helpers/dom";
import type { PreviewItem } from "@/utils/canvas/preview-items";

const SRC = "/files/abc/lightbox.png";
const THUMB = `${SRC}?v=thumb&pv=2`;
const noop = () => {};

function Lightbox({
  src,
  mediaType = "image",
}: {
  src: string;
  mediaType?: PreviewItem["mediaType"];
}) {
  return (
    <TooltipProvider>
      <MediaLightbox
        items={[{ id: "preview", src, mediaType, label: "预览" }]}
        target={{ id: "preview", origin: null }}
        onActiveChange={noop}
        onClose={noop}
      />
    </TooltipProvider>
  );
}

describe("MediaLightbox 舞台的媒体加载", () => {
  let dom: DomEnv;
  let unmount: (() => Promise<void>) | undefined;
  let restoreFetch: (() => void) | undefined;

  beforeAll(async () => {
    dom = await installDom();
  });
  beforeEach(() => {
    const fetchSpy = spyOn(globalThis, "fetch").mockImplementation(async () => new Response(null));
    restoreFetch = () => fetchSpy.mockRestore();
  });
  afterEach(async () => {
    await unmount?.();
    unmount = undefined;
    restoreFetch?.();
  });
  afterAll(() => dom.restore());

  function popup() {
    const element = dom.win.document.querySelector<HTMLElement>('[data-slot="media-lightbox"]');
    if (!element) throw new Error("灯箱没有打开");
    return element;
  }

  async function render(src = SRC, mediaType?: PreviewItem["mediaType"]) {
    const container = dom.win.document.createElement("div");
    dom.win.document.body.appendChild(container);
    const root = dom.createRoot(container);
    unmount = async () => {
      await dom.act(async () => root.unmount());
      container.remove();
    };
    const update = (next: string) =>
      dom.act(async () => root.render(<Lightbox src={next} mediaType={mediaType} />));
    await update(src);
    return update;
  }

  function sources() {
    return [...popup().querySelectorAll("img")].map((img) => img.getAttribute("src"));
  }

  function image(src: string) {
    const img = popup().querySelector<HTMLImageElement>(`img[src="${src}"]`);
    if (!img) throw new Error(`图片未挂载：${src}`);
    return img;
  }

  async function load(img: HTMLImageElement, width: number, height: number) {
    Object.defineProperties(img, {
      naturalWidth: { value: width },
      naturalHeight: { value: height },
    });
    await dom.act(async () => img.dispatchEvent(new dom.win.Event("load")));
  }

  async function fail(img: HTMLImageElement) {
    await dom.act(async () => img.dispatchEvent(new dom.win.Event("error")));
  }

  test("先等严格缩略图加载再挂原图，保留画幅和真实尺寸", async () => {
    await render();
    expect(sources()).toEqual([THUMB]);
    const thumb = image(THUMB);
    await load(thumb, 400, 500);
    expect(sources()).toEqual([THUMB, SRC]);
    const original = image(SRC);
    expect(original.style.opacity).toBe("0");
    expect(image(THUMB)).toBe(thumb);
    expect(thumb.closest<HTMLElement>('[style*="aspect-ratio"]')?.style.aspectRatio).toBe("0.8");
    await load(original, 800, 1000);
    expect(thumb.style.opacity).toBe("0");
    expect(popup().querySelector('[aria-live="polite"]')?.textContent).toBe("800×1000");
  });

  test("缩略图失败才退回原图，原图失败后可重试", async () => {
    await render();
    expect(sources()).toEqual([THUMB]);
    await fail(image(THUMB));
    expect(sources()).toEqual([SRC]);
    const original = image(SRC);
    await fail(original);
    expect(popup().querySelector('[aria-label="原图加载失败"]')).not.toBeNull();
    const retry = [...popup().querySelectorAll("button")].find((button) =>
      button.textContent?.includes("重试"),
    );
    await dom.click(retry);
    expect(sources()).toEqual([SRC]);
    expect(image(SRC)).not.toBe(original);
    expect(popup().querySelector('[aria-label="原图加载失败"]')).toBeNull();
  });

  test("外链直接挂原图，同一项替换 src 后重置加载状态并重新等待缩略图", async () => {
    const external = "https://example.com/lightbox.png";
    const update = await render(external);
    expect(sources()).toEqual([external]);
    await load(image(external), 1440, 1080);
    await update(SRC);
    expect(sources()).toEqual([THUMB]);
    await load(image(THUMB), 400, 300);
    expect(sources()).toEqual([THUMB, SRC]);
    await load(image(SRC), 800, 600);
    await update("/files/abc/next.png");
    expect(sources()).toEqual(["/files/abc/next.png?v=thumb&pv=2"]);
  });

  test("视频舞台保留实际播放，并使用严格封面地址", async () => {
    const prototype = dom.win.HTMLMediaElement.prototype;
    const play = spyOn(prototype, "play").mockResolvedValue(undefined);
    const pause = spyOn(prototype, "pause").mockImplementation(() => {});
    const reload = spyOn(prototype, "load").mockImplementation(() => {});
    try {
      const src = "/files/abc/lightbox.mp4";
      await render(src, "video");
      expect(sources()).toEqual([`${src}?v=poster&pv=2`]);
      expect(popup().querySelector("video")?.getAttribute("src")).toBe(src);
    } finally {
      await unmount?.();
      unmount = undefined;
      play.mockRestore();
      pause.mockRestore();
      reload.mockRestore();
    }
  });
});
