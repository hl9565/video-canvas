import { useEffect } from "react";
import { afterAll, afterEach, beforeAll, describe, expect, spyOn, test } from "bun:test";
import { MemoryRouter, useLocation } from "react-router";

import {
  CanvasEditorApiProvider,
  type CanvasEditorApi,
} from "@/components/canvas-editor/editor-api";
import { DramaFrameBridge } from "@/pages/canvas/chrome/drama-frame-bridge";
import { useWorkspaceNav } from "@/pages/drama/workspace/use-nav";
import { installDom, type DomEnv } from "@/tests/helpers/dom";
import { ApiError } from "@/utils/requests/request";
import service from "@/utils/requests/service";

let dom: DomEnv;
const spies: Array<{ mockRestore: () => void }> = [];
beforeAll(async () => {
  dom = await installDom();
});
afterAll(() => dom.restore());
afterEach(() => {
  while (spies.length) spies.pop()!.mockRestore();
  dom.win.document.body.innerHTML = "";
});

const nodes = [
  {
    id: "first",
    type: "canvas",
    position: { x: 0, y: 0 },
    data: { kind: "image", label: "第一张", assetId: "a1", status: "done" },
  },
  {
    id: "selected",
    type: "canvas",
    selected: true,
    position: { x: 0, y: 0 },
    data: { kind: "image", label: "选中图片", assetId: "a2", status: "done" },
  },
  {
    id: "running",
    type: "canvas",
    position: { x: 0, y: 0 },
    data: { kind: "image", label: "生成中旧图", assetId: "old", status: "running" },
  },
  {
    id: "video",
    type: "canvas",
    position: { x: 0, y: 0 },
    data: { kind: "video", label: "视频", assetId: "v1", status: "done" },
  },
];
const editor = { canvasId: "c1", nodes } as unknown as CanvasEditorApi;
const rawRun = {
  id: "r1",
  segment_id: "s1",
  request_key: "req",
  frame_source: "canvas",
  state: "draft",
  segment_revision: 1,
  revision: 5,
  confirmed_version: 0,
  adopt_if_empty: false,
  model_key: "",
  error_code: "",
  error_message: "",
  created_at: "",
  updated_at: "",
};
const activeUrl =
  "/canvas/c1?drama_id=d1&episode_no=2&segment_id=s1&frame_run_id=r1&frame_run_revision=1&canvas_ticket=t%26%3D1";

function LocationProbe({ onLocation }: { onLocation: (path: string) => void }) {
  const location = useLocation();
  useEffect(() => onLocation(location.pathname + location.search), [location, onLocation]);
  return null;
}
function Harness({
  url,
  onLocation = () => {},
}: {
  url: string;
  onLocation?: (path: string) => void;
}) {
  return (
    <MemoryRouter initialEntries={[url]}>
      <CanvasEditorApiProvider value={editor}>
        <DramaFrameBridge />
        <LocationProbe onLocation={onLocation} />
      </CanvasEditorApiProvider>
    </MemoryRouter>
  );
}
const button = (text: string) =>
  Array.from(dom.win.document.querySelectorAll("button")).find((b) => b.textContent === text);

describe("Canvas 首帧桥接", () => {
  test("无完整桥接参数时不显示也不发请求", async () => {
    const get = spyOn(service, "get");
    spies.push(get);
    const mounted = await dom.mount(<Harness url="/canvas/c1" />);
    try {
      expect(mounted.container.textContent).toBe("");
      expect(get).not.toHaveBeenCalled();
    } finally {
      await mounted.unmount();
    }
  });

  test("默认选中就绪图片，使用最新 revision 和原票据回传，广播并返回工作台定位链接", async () => {
    const get = spyOn(service, "get").mockResolvedValue(rawRun);
    const post = spyOn(service, "post").mockResolvedValue({
      ...rawRun,
      state: "frame_ready",
      revision: 6,
      frame_asset_id: "cropped",
      frame_sha256: "a".repeat(64),
    });
    spies.push(get, post);
    const messages: unknown[] = [];
    const originalChannel = globalThis.BroadcastChannel;
    const channels: string[] = [];
    globalThis.BroadcastChannel = class {
      constructor(name: string) {
        channels.push(name);
      }
      postMessage(message: unknown) {
        messages.push(message);
      }
      close() {}
    } as unknown as typeof BroadcastChannel;
    let location = "";
    const mounted = await dom.mount(
      <Harness
        url={activeUrl}
        onLocation={(path) => {
          location = path;
        }}
      />,
    );
    try {
      const picker = mounted.container.querySelector("select") as HTMLSelectElement;
      expect(picker.value).toBe("selected");
      expect(Array.from(picker.options).map((o) => o.value)).toEqual(["first", "selected"]);
      await dom.click(button("设为分镜首帧"));
      expect(get.mock.calls[0][0]).toBe("/drama-segments/s1/frame-runs/r1");
      expect(post).toHaveBeenCalledWith(
        "/drama-segments/s1/frame-runs/r1/frame",
        { revision: 5, frame_asset_id: "a2", origin_canvas_id: "c1", canvas_ticket: "t&=1" },
        { silent: true },
      );
      expect(channels).toEqual(["vc-drama-frame-run"]);
      expect(messages[0]).toMatchObject({
        segmentId: "s1",
        frameRunId: "r1",
        run: { revision: 6, frameAssetId: "cropped" },
      });
      await dom.click(button("返回分镜工作台"));
      expect(location).toBe("/drama/d1?area=studio&ep=2&segment=s1&frame_run=r1");
    } finally {
      await mounted.unmount();
      globalThis.BroadcastChannel = originalChannel;
    }
  });

  test("长时停留后 URL 票据过期时自动改用服务端刷新票据并同步 URL", async () => {
    const get = spyOn(service, "get").mockResolvedValue({
      ...rawRun,
      canvas_ticket: "fresh-ticket-99",
    });
    const post = spyOn(service, "post").mockResolvedValue({
      ...rawRun,
      state: "frame_ready",
      revision: 6,
      frame_asset_id: 902,
      frame_sha256: "b".repeat(64),
      canvas_ticket: "fresh-ticket-100",
    });
    spies.push(get, post);
    let location = "";
    const mounted = await dom.mount(
      <Harness
        url={activeUrl}
        onLocation={(path) => {
          location = path;
        }}
      />,
    );
    try {
      await dom.click(button("设为分镜首帧"));
      expect(post).toHaveBeenCalledWith(
        "/drama-segments/s1/frame-runs/r1/frame",
        {
          revision: 5,
          frame_asset_id: "a2",
          origin_canvas_id: "c1",
          canvas_ticket: "fresh-ticket-99",
        },
        { silent: true },
      );
      expect(location).toContain("canvas_ticket=fresh-ticket-100");
    } finally {
      await mounted.unmount();
    }
  });

  test("并发 409 会刷新状态并保留可重试错误，不广播成功", async () => {
    const get = spyOn(service, "get").mockResolvedValue(rawRun);
    const post = spyOn(service, "post").mockRejectedValue(new ApiError("conflict", 91005, 409));
    spies.push(get, post);
    const mounted = await dom.mount(<Harness url={activeUrl} />);
    try {
      await dom.click(button("设为分镜首帧"));
      expect(dom.win.document.body.textContent).toContain("首帧控镜状态已变更或存在进行中流程");
      expect(get.mock.calls.length).toBe(2);
    } finally {
      await mounted.unmount();
    }
  });

  test("工作台首次规范化 URL 时保留 segment 与 frame_run", async () => {
    let path = "";
    function NavProbe() {
      useWorkspaceNav({ outlineMd: "已生成", episodes: [{ no: 2, scriptStatus: "ready" }] });
      return (
        <LocationProbe
          onLocation={(value) => {
            path = value;
          }}
        />
      );
    }
    const mounted = await dom.mount(
      <MemoryRouter initialEntries={["/drama/d1?ep=2&segment=s1&frame_run=r1"]}>
        <NavProbe />
      </MemoryRouter>,
    );
    try {
      expect(path).toContain("segment=s1");
      expect(path).toContain("frame_run=r1");
    } finally {
      await mounted.unmount();
    }
  });
});
