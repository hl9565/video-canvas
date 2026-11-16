import type { ReactElement } from "react";
import {
  afterAll,
  afterEach,
  beforeAll,
  beforeEach,
  describe,
  expect,
  it,
  mock,
  spyOn,
} from "bun:test";
import { ReactFlow, ReactFlowProvider } from "@xyflow/react";

import { TrimOverlay } from "@/components/canvas/trim-overlay";
import {
  Filmstrip,
  MuteButton,
  Playhead,
  RangeHandles,
  TrimPanel,
} from "@/components/canvas/trim-panel";
import { useTrimStore } from "@/store/trim";
import { installDom } from "@/tests/helpers/dom";
import type { CanvasNode, CanvasNodeData } from "@/types";
import { trimEngine } from "@/utils/canvas/trim-engine";
import { trimRunner } from "@/utils/canvas/trim-runner";

let dom: Awaited<ReturnType<typeof installDom>>;
const unexpectedDomErrors: unknown[] = [];
const onWindowError = (event: ErrorEvent) => {
  unexpectedDomErrors.push(event.error ?? event.message);
};

beforeAll(async () => {
  dom = await installDom();
  dom.win.addEventListener("error", onWindowError);
});

afterAll(async () => {
  try {
    (dom.win.document.activeElement as HTMLElement | null)?.blur?.();
    dom.win.getSelection()?.removeAllRanges();
    await dom.flush(20);
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(unexpectedDomErrors.splice(0)).toEqual([]);
  } finally {
    dom.win.removeEventListener("error", onWindowError);
    dom.restore();
  }
});

async function mountRerenderable(initial: ReactElement) {
  const container = dom.win.document.createElement("div");
  dom.win.document.body.appendChild(container);
  let root!: ReturnType<typeof dom.createRoot>;
  await dom.act(async () => {
    root = dom.createRoot(container);
    root.render(initial);
  });
  return {
    container,
    rerender: (next: ReactElement) => dom.act(async () => root.render(next)),
    unmount: () =>
      dom.act(async () => {
        root.unmount();
        container.remove();
      }),
  };
}

const spies: Array<{ mockRestore: () => void }> = [];

beforeEach(() => {
  useTrimStore.getState().reset();
});

afterEach(async () => {
  while (spies.length > 0) spies.pop()?.mockRestore();
  (dom.win.document.activeElement as HTMLElement | null)?.blur?.();
  dom.win.getSelection()?.removeAllRanges();
  await dom.flush(0);
  expect(unexpectedDomErrors.splice(0)).toEqual([]);
});

function mockTrackRect(track: HTMLElement, width = 200, left = 0) {
  Object.defineProperty(track, "getBoundingClientRect", {
    configurable: true,
    value: () => ({
      left,
      right: left + width,
      width,
      top: 0,
      bottom: 48,
      height: 48,
      x: left,
      y: 0,
      toJSON: () => ({}),
    }),
  });
}

function dispatchPointer(
  target: EventTarget,
  type: "pointerdown" | "pointermove" | "pointerup" | "pointercancel" | "lostpointercapture",
  init: { clientX?: number; button?: number; pointerId?: number } = {},
) {
  const EventCtor =
    (dom.win as unknown as { PointerEvent?: typeof Event }).PointerEvent ?? dom.win.MouseEvent;
  const event = new EventCtor(type, {
    bubbles: type !== "lostpointercapture",
    cancelable: true,
    button: init.button ?? 0,
    clientX: init.clientX ?? 0,
  });
  Object.defineProperty(event, "pointerId", { value: init.pointerId ?? 1 });
  target.dispatchEvent(event);
}

function trackWindowPointerListeners() {
  const active = new Map<string, Set<EventListenerOrEventListenerObject>>();
  const pointerTypes = new Set(["pointermove", "pointerup", "pointercancel"]);
  const origAdd = dom.win.addEventListener.bind(dom.win);
  const origRemove = dom.win.removeEventListener.bind(dom.win);

  spies.push(
    spyOn(dom.win, "addEventListener").mockImplementation(((
      type: string,
      listener: EventListenerOrEventListenerObject | null,
      options?: boolean | AddEventListenerOptions,
    ) => {
      if (listener && pointerTypes.has(type)) {
        const set = active.get(type) ?? new Set();
        set.add(listener);
        active.set(type, set);
      }
      return origAdd(type, listener, options);
    }) as typeof dom.win.addEventListener),
    spyOn(dom.win, "removeEventListener").mockImplementation(((
      type: string,
      listener: EventListenerOrEventListenerObject | null,
      options?: boolean | EventListenerOptions,
    ) => {
      if (listener && pointerTypes.has(type)) {
        active.get(type)?.delete(listener);
      }
      return origRemove(type, listener, options);
    }) as typeof dom.win.removeEventListener),
  );

  return {
    count: (type: "pointermove" | "pointerup" | "pointercancel") => active.get(type)?.size ?? 0,
    totalCount: () => Array.from(active.values()).reduce((sum, set) => sum + set.size, 0),
  };
}

function dispatchKey(target: EventTarget, key: string, shiftKey = false) {
  target.dispatchEvent(
    new dom.win.KeyboardEvent("keydown", {
      bubbles: true,
      cancelable: true,
      key,
      shiftKey,
    }),
  );
}

describe("TrimPanel", () => {
  it("renders loading placeholder until source metadata is ready", async () => {
    useTrimStore.getState().enter("node-1", 0);
    const view = await dom.mount(
      <TrimPanel width={420} thumbs={[]} onConfirm={() => {}} onClose={() => {}} />,
    );

    expect(view.container.textContent).toContain("加载中…");
    expect(view.container.querySelectorAll("img").length).toBe(0);

    const confirmBtn = view.container.querySelector<HTMLButtonElement>(
      'button[aria-label="确认修剪"]',
    );
    expect(confirmBtn?.disabled).toBe(true);
    await view.unmount();
  });

  it("handles drag, keyboard stepping, mute toggle and confirm when trimmed", async () => {
    useTrimStore.getState().enter("node-1", 0);
    useTrimStore.getState().ready(10, true);
    const onConfirm = mock(() => {});
    const onClose = mock(() => {});

    const view = await dom.mount(
      <TrimPanel
        width={420}
        thumbs={["blob:thumb-1", "blob:thumb-2"]}
        onConfirm={onConfirm}
        onClose={onClose}
      />,
    );

    const confirmBtn = view.container.querySelector<HTMLButtonElement>(
      'button[aria-label="确认修剪"]',
    )!;
    // 全长未裁剪时不可确认
    expect(confirmBtn.disabled).toBe(true);
    expect(view.container.textContent).toContain("10.00 s");

    const track = view.container.querySelector<HTMLElement>('[data-slot="trim-range-track"]')!;
    mockTrackRect(track, 200, 0);

    const startHandle = view.container.querySelector<HTMLElement>('[data-trim-handle="start"]')!;
    const endHandle = view.container.querySelector<HTMLElement>('[data-trim-handle="end"]')!;

    // 指针拖拽入点到 20% -> 2.00s
    await dom.act(async () => {
      dispatchPointer(startHandle, "pointerdown", { clientX: 0, button: 0, pointerId: 1 });
      dispatchPointer(dom.win, "pointermove", { clientX: 40, pointerId: 1 });
      dispatchPointer(dom.win, "pointerup", { clientX: 40, pointerId: 1 });
    });

    expect(useTrimStore.getState().start).toBe(2);
    expect(useTrimStore.getState().seek?.time).toBe(2);
    expect(confirmBtn.disabled).toBe(false);

    // 键盘微调入点：+0.1s、+1s，出点：-0.1s
    await dom.act(async () => {
      dispatchKey(startHandle, "ArrowRight");
    });
    expect(useTrimStore.getState().start).toBe(2.1);

    await dom.act(async () => {
      dispatchKey(startHandle, "ArrowRight", true);
    });
    expect(useTrimStore.getState().start).toBe(3.1);

    await dom.act(async () => {
      dispatchKey(endHandle, "ArrowLeft");
    });
    expect(useTrimStore.getState().end).toBe(9.9);

    // 去音轨开关
    const muteBtn = view.container.querySelector<HTMLButtonElement>('button[aria-label="去音轨"]')!;
    expect(muteBtn.getAttribute("aria-pressed")).toBe("false");
    await dom.click(muteBtn);
    expect(useTrimStore.getState().discardAudio).toBe(true);
    expect(muteBtn.getAttribute("aria-pressed")).toBe("true");

    // 确认修剪与关闭
    await dom.click(confirmBtn);
    expect(onConfirm).toHaveBeenCalledTimes(1);

    const closeBtn = view.container.querySelector<HTMLButtonElement>(
      'button[aria-label="关闭修剪"]',
    )!;
    await dom.click(closeBtn);
    expect(onClose).toHaveBeenCalledTimes(1);

    await view.unmount();
  });

  it("disables mute when video has no audio and disables confirm on loadError", async () => {
    useTrimStore.getState().enter("node-2", 0);
    useTrimStore.getState().ready(8, false);
    useTrimStore.getState().setRange(1, 6);

    const view = await dom.mount(
      <TrimPanel
        width={420}
        thumbs={[null, null]}
        loadError="读取视频失败"
        onConfirm={() => {}}
        onClose={() => {}}
      />,
    );

    const muteBtn = view.container.querySelector<HTMLButtonElement>('button[aria-label="去音轨"]')!;
    const confirmBtn = view.container.querySelector<HTMLButtonElement>(
      'button[aria-label="确认修剪"]',
    )!;
    const alertEl = view.container.querySelector('[role="alert"]');

    expect(muteBtn.disabled).toBe(true);
    expect(confirmBtn.disabled).toBe(true);
    expect(alertEl?.textContent).toContain("读取视频失败");

    await view.unmount();
  });

  it("locks handles and renders encoding progress bar while encoding", async () => {
    useTrimStore.getState().enter("node-3", 0);
    useTrimStore.getState().ready(8, true);
    useTrimStore.getState().setRange(1, 5);
    useTrimStore.getState().setPhase("encoding");
    useTrimStore.getState().setProgress(42);
    const onClose = mock(() => {});

    const view = await dom.mount(
      <TrimPanel width={420} thumbs={["blob:1"]} onConfirm={() => {}} onClose={onClose} />,
    );

    const startHandle = view.container.querySelector<HTMLElement>('[data-trim-handle="start"]')!;
    expect(startHandle.getAttribute("aria-disabled")).toBe("true");
    expect(view.container.textContent).toContain("编码中 42%");

    const abortBtn = view.container.querySelector<HTMLButtonElement>(
      'button[aria-label="中止编码"]',
    )!;
    await dom.click(abortBtn);
    expect(onClose).toHaveBeenCalledTimes(1);

    await view.unmount();
  });

  it("renders store encoding error when returning to editing", async () => {
    useTrimStore.getState().enter("node-4", 0);
    useTrimStore.getState().ready(8, true);
    useTrimStore.getState().fail("编码失败：超时");

    const view = await dom.mount(
      <TrimPanel width={420} thumbs={["blob:1"]} onConfirm={() => {}} onClose={() => {}} />,
    );

    const alertEl = view.container.querySelector('[role="alert"]');
    expect(alertEl?.textContent).toContain("编码失败：超时");
    await view.unmount();
  });
});

describe("extracted trim primitives", () => {
  it("Filmstrip renders 16 placeholders when empty and images when populated", async () => {
    const view = await mountRerenderable(<Filmstrip thumbs={[]} />);
    expect(view.container.querySelectorAll("img").length).toBe(0);
    expect(view.container.firstElementChild?.children.length).toBe(17); // 16 cells + shimmer overlay

    await view.rerender(
      <Filmstrip thumbs={["https://example.test/1.jpg", null, "https://example.test/3.jpg"]} />,
    );
    const imgs = view.container.querySelectorAll("img");
    expect(imgs.length).toBe(2);
    expect(imgs[0]?.getAttribute("src")).toBe("https://example.test/1.jpg");
    expect(imgs[1]?.getAttribute("src")).toBe("https://example.test/3.jpg");

    await view.unmount();
  });

  it("Playhead clamps percentage between 0% and 100%", async () => {
    const view = await mountRerenderable(<Playhead value={6} total={24} />);
    const head = () => view.container.querySelector<HTMLElement>('[data-slot="trim-playhead"]')!;
    expect(head().style.left).toBe("25%");

    await view.rerender(<Playhead value={-5} total={24} />);
    expect(head().style.left).toBe("0%");

    await view.rerender(<Playhead value={30} total={24} />);
    expect(head().style.left).toBe("100%");

    await view.unmount();
  });

  it("RangeHandles supports integer-frame minGap=1 stepping and track seeking", async () => {
    const changes: Array<{ start: number; end: number; which: string }> = [];
    const seeks: number[] = [];
    const snap = (v: number) => Math.round(Math.min(Math.max(v, 0), 124));

    const view = await dom.mount(
      <RangeHandles
        start={24}
        end={25}
        total={124}
        minGap={1}
        step={1}
        bigStep={10}
        snap={snap}
        onChange={(next, which) => changes.push({ ...next, which })}
        onSeek={(v) => seeks.push(v)}
      >
        <Filmstrip thumbs={["blob:a"]} />
      </RangeHandles>,
    );

    const track = view.container.querySelector<HTMLElement>('[data-slot="trim-range-track"]')!;
    mockTrackRect(track, 248, 0);

    const startHandle = view.container.querySelector<HTMLElement>('[data-trim-handle="start"]')!;
    const endHandle = view.container.querySelector<HTMLElement>('[data-trim-handle="end"]')!;

    // start=24, end=25, minGap=1 -> start ArrowRight clamped at 24
    await dom.act(async () => {
      dispatchKey(startHandle, "ArrowRight");
    });
    expect(changes.at(-1)).toEqual({ start: 24, end: 25, which: "start" });

    // end Shift+ArrowRight -> 35
    await dom.act(async () => {
      dispatchKey(endHandle, "ArrowRight", true);
    });
    expect(changes.at(-1)).toEqual({ start: 24, end: 35, which: "end" });

    // 点击轨道空白处定位播放头：clientX=124 -> 50% -> frame 62
    await dom.act(async () => {
      dispatchPointer(track, "pointerdown", { clientX: 124, button: 0, pointerId: 2 });
      dispatchPointer(dom.win, "pointerup", { clientX: 124, pointerId: 2 });
    });
    expect(seeks.at(-1)).toBe(62);

    await view.unmount();
  });

  it("cleans up window pointer listeners when unmounting mid-drag without pointerup", async () => {
    const tracker = trackWindowPointerListeners();

    // 1. 手柄拖动中途卸载（未触发 pointerup）
    const onChange = mock(() => {});
    const onSeek = mock(() => {});
    const handleView = await dom.mount(
      <RangeHandles start={2} end={8} total={10} minGap={1} onChange={onChange} onSeek={onSeek} />,
    );
    const handleTrack = handleView.container.querySelector<HTMLElement>(
      '[data-slot="trim-range-track"]',
    )!;
    mockTrackRect(handleTrack, 200, 0);
    const startHandle = handleView.container.querySelector<HTMLElement>(
      '[data-trim-handle="start"]',
    )!;

    await dom.act(async () => {
      dispatchPointer(startHandle, "pointerdown", { clientX: 40, button: 0, pointerId: 1 });
      dispatchPointer(dom.win, "pointermove", { clientX: 80, pointerId: 1 });
    });
    expect(onChange).toHaveBeenCalledTimes(1);
    expect(tracker.count("pointermove")).toBe(1);
    expect(tracker.count("pointerup")).toBe(1);
    expect(tracker.count("pointercancel")).toBe(1);

    onChange.mockClear();
    await handleView.unmount();

    expect(tracker.totalCount()).toBe(0);
    await dom.act(async () => {
      dispatchPointer(dom.win, "pointermove", { clientX: 120, pointerId: 1 });
      dispatchPointer(dom.win, "pointerup", { clientX: 120, pointerId: 1 });
    });
    expect(onChange).not.toHaveBeenCalled();

    // 2. 轨道 scrub 拖动中途卸载（未触发 pointerup）
    const scrubView = await dom.mount(
      <RangeHandles start={2} end={8} total={10} minGap={1} onChange={onChange} onSeek={onSeek} />,
    );
    const scrubTrack = scrubView.container.querySelector<HTMLElement>(
      '[data-slot="trim-range-track"]',
    )!;
    mockTrackRect(scrubTrack, 200, 0);

    await dom.act(async () => {
      dispatchPointer(scrubTrack, "pointerdown", { clientX: 60, button: 0, pointerId: 2 });
      dispatchPointer(dom.win, "pointermove", { clientX: 100, pointerId: 2 });
    });
    expect(onSeek).toHaveBeenCalledTimes(2);
    expect(tracker.count("pointermove")).toBe(1);
    expect(tracker.count("pointerup")).toBe(1);
    expect(tracker.count("pointercancel")).toBe(1);

    onSeek.mockClear();
    await scrubView.unmount();

    expect(tracker.totalCount()).toBe(0);
    await dom.act(async () => {
      dispatchPointer(dom.win, "pointermove", { clientX: 140, pointerId: 2 });
      dispatchPointer(dom.win, "pointerup", { clientX: 140, pointerId: 2 });
    });
    expect(onSeek).not.toHaveBeenCalled();
  });

  it("cleans up drag listeners on lostpointercapture and pointercancel", async () => {
    const tracker = trackWindowPointerListeners();

    const onChange = mock(() => {});
    const onSeek = mock(() => {});
    const view = await dom.mount(
      <RangeHandles start={2} end={8} total={10} minGap={1} onChange={onChange} onSeek={onSeek} />,
    );
    const track = view.container.querySelector<HTMLElement>('[data-slot="trim-range-track"]')!;
    mockTrackRect(track, 200, 0);
    const endHandle = view.container.querySelector<HTMLElement>('[data-trim-handle="end"]')!;

    // lostpointercapture 清理手柄拖拽监听器
    await dom.act(async () => {
      dispatchPointer(endHandle, "pointerdown", { clientX: 160, button: 0, pointerId: 3 });
      dispatchPointer(dom.win, "pointermove", { clientX: 140, pointerId: 3 });
    });
    expect(onChange).toHaveBeenCalledTimes(1);
    expect(tracker.totalCount()).toBe(3);

    onChange.mockClear();
    await dom.act(async () => {
      dispatchPointer(endHandle, "lostpointercapture", { pointerId: 3 });
    });
    expect(tracker.totalCount()).toBe(0);
    await dom.act(async () => {
      dispatchPointer(dom.win, "pointermove", { clientX: 120, pointerId: 3 });
    });
    expect(onChange).not.toHaveBeenCalled();

    // pointercancel 清理轨道 scrub 监听器
    await dom.act(async () => {
      dispatchPointer(track, "pointerdown", { clientX: 60, button: 0, pointerId: 4 });
      dispatchPointer(dom.win, "pointermove", { clientX: 90, pointerId: 4 });
    });
    expect(onSeek).toHaveBeenCalledTimes(2);
    expect(tracker.totalCount()).toBe(3);

    onSeek.mockClear();
    await dom.act(async () => {
      dispatchPointer(dom.win, "pointercancel", { pointerId: 4 });
    });
    expect(tracker.totalCount()).toBe(0);
    await dom.act(async () => {
      dispatchPointer(dom.win, "pointermove", { clientX: 110, pointerId: 4 });
    });
    expect(onSeek).not.toHaveBeenCalled();

    await view.unmount();
  });

  it("ignores unrelated pointerId events and secondary pointerdown during active handle or scrub drags", async () => {
    const tracker = trackWindowPointerListeners();
    const changes: Array<{ start: number; end: number; which: string }> = [];
    const seeks: number[] = [];

    const view = await dom.mount(
      <RangeHandles
        start={2}
        end={8}
        total={10}
        minGap={1}
        onChange={(next, which) => changes.push({ ...next, which })}
        onSeek={(value) => seeks.push(value)}
      />,
    );
    const track = view.container.querySelector<HTMLElement>('[data-slot="trim-range-track"]')!;
    mockTrackRect(track, 200, 0);
    const startHandle = view.container.querySelector<HTMLElement>('[data-trim-handle="start"]')!;
    const endHandle = view.container.querySelector<HTMLElement>('[data-trim-handle="end"]')!;

    // 1. 手柄拖拽（pointerId=1）：另一根指针（pointerId=2）按下、移动、抬起、取消、失捕均不得篡改或中止
    await dom.act(async () => {
      dispatchPointer(startHandle, "pointerdown", { clientX: 40, button: 0, pointerId: 1 });
      dispatchPointer(dom.win, "pointermove", { clientX: 60, pointerId: 1 });
    });
    expect(changes).toEqual([{ start: 3, end: 8, which: "start" }]);
    expect(tracker.totalCount()).toBe(3);

    await dom.act(async () => {
      dispatchPointer(endHandle, "pointerdown", { clientX: 120, button: 0, pointerId: 2 });
      dispatchPointer(track, "pointerdown", { clientX: 100, button: 0, pointerId: 2 });
      dispatchPointer(dom.win, "pointermove", { clientX: 120, pointerId: 2 });
      dispatchPointer(startHandle, "lostpointercapture", { pointerId: 2 });
      dispatchPointer(dom.win, "pointercancel", { pointerId: 2 });
      dispatchPointer(dom.win, "pointerup", { clientX: 120, pointerId: 2 });
    });
    expect(changes).toEqual([{ start: 3, end: 8, which: "start" }]);
    expect(seeks).toEqual([]);
    expect(tracker.totalCount()).toBe(3);

    // 原指针 pointerId=1 仍能继续拖拽并正常结束
    await dom.act(async () => {
      dispatchPointer(dom.win, "pointermove", { clientX: 80, pointerId: 1 });
      dispatchPointer(dom.win, "pointerup", { clientX: 80, pointerId: 1 });
    });
    expect(changes).toEqual([
      { start: 3, end: 8, which: "start" },
      { start: 4, end: 8, which: "start" },
    ]);
    expect(tracker.totalCount()).toBe(0);

    // 2. 轨道 scrub 拖拽（pointerId=1）：另一根指针（pointerId=2）按下、移动、抬起、取消、失捕均不得篡改或中止
    changes.length = 0;
    seeks.length = 0;

    await dom.act(async () => {
      dispatchPointer(track, "pointerdown", { clientX: 40, button: 0, pointerId: 1 });
      dispatchPointer(dom.win, "pointermove", { clientX: 80, pointerId: 1 });
    });
    expect(seeks).toEqual([2, 4]);
    expect(tracker.totalCount()).toBe(3);

    await dom.act(async () => {
      dispatchPointer(track, "pointerdown", { clientX: 160, button: 0, pointerId: 2 });
      dispatchPointer(startHandle, "pointerdown", { clientX: 160, button: 0, pointerId: 2 });
      dispatchPointer(dom.win, "pointermove", { clientX: 160, pointerId: 2 });
      dispatchPointer(track, "lostpointercapture", { pointerId: 2 });
      dispatchPointer(dom.win, "pointercancel", { pointerId: 2 });
      dispatchPointer(dom.win, "pointerup", { clientX: 160, pointerId: 2 });
    });
    expect(seeks).toEqual([2, 4]);
    expect(changes).toEqual([]);
    expect(tracker.totalCount()).toBe(3);

    // 原指针 pointerId=1 仍能继续 scrub 并正常结束
    await dom.act(async () => {
      dispatchPointer(dom.win, "pointermove", { clientX: 100, pointerId: 1 });
      dispatchPointer(dom.win, "pointerup", { clientX: 100, pointerId: 1 });
    });
    expect(seeks).toEqual([2, 4, 5]);
    expect(tracker.totalCount()).toBe(0);

    await view.unmount();
  });

  it("MuteButton supports button variant and no-audio disabled state", async () => {
    const onToggle = mock((_next: boolean) => {});
    const view = await mountRerenderable(
      <MuteButton variant="button" muted={false} hasAudio onToggle={onToggle} />,
    );

    const btn = view.container.querySelector<HTMLButtonElement>("button")!;
    expect(btn.disabled).toBe(false);
    expect(btn.textContent).toContain("去掉声音");
    await dom.click(btn);
    expect(onToggle).toHaveBeenCalledWith(true);

    await view.rerender(<MuteButton variant="button" muted hasAudio={false} onToggle={onToggle} />);
    expect(btn.disabled).toBe(true);
    expect(btn.textContent).toContain("该视频没有声音");

    await view.unmount();
  });

  it("TrimOverlay confirm invokes trimRunner.start with nodeId, src, start, end, duration and discardAudio", async () => {
    const testSrc = "https://example.test/overlay-trim-confirm.mp4";
    const startCalls: unknown[] = [];

    spies.push(
      spyOn(trimEngine, "probe").mockResolvedValue({
        duration: 12,
        hasAudio: true,
        width: 1280,
        height: 720,
      }),
      spyOn(trimEngine, "thumbnails").mockResolvedValue(Array(16).fill(null)),
      spyOn(globalThis, "fetch").mockResolvedValue(
        new Response(null, { status: 200, headers: { "content-length": "2048" } }),
      ),
      spyOn(trimRunner, "start").mockImplementation(async (job) => {
        startCalls.push(job);
        return { outcome: "aborted" };
      }),
    );

    expect(useTrimStore.getState().enter("trim-node-1")).toBe(true);

    const nodeData: CanvasNodeData = {
      type: "video",
      label: "源视频",
      src: testSrc,
    };
    const nodes: CanvasNode[] = [
      {
        id: "trim-node-1",
        type: "video",
        position: { x: 0, y: 0 },
        width: 240,
        height: 180,
        data: nodeData,
      },
    ];
    const nodeTypes = {
      video: ({ id, data }: { id: string; data: CanvasNodeData }) => (
        <TrimOverlay id={id} data={data} />
      ),
    };

    const view = await mountRerenderable(
      <div style={{ width: 600, height: 400 }}>
        <ReactFlowProvider>
          <ReactFlow nodes={nodes} nodeTypes={nodeTypes} />
        </ReactFlowProvider>
      </div>,
    );

    try {
      await dom.flush(30);
      await dom.act(async () => {
        useTrimStore.getState().setRange(2.5, 8.25);
        useTrimStore.getState().setDiscardAudio(true);
      });
      await dom.flush();

      const confirmBtn = view.container.querySelector<HTMLButtonElement>(
        'button[aria-label="确认修剪"]',
      );
      expect(confirmBtn).not.toBeNull();
      expect(confirmBtn?.disabled).toBe(false);

      await dom.click(confirmBtn);
      await dom.flush();

      expect(startCalls).toHaveLength(1);
      expect(startCalls[0]).toMatchObject({
        nodeId: "trim-node-1",
        src: testSrc,
        start: 2.5,
        end: 8.25,
        duration: 12,
        discardAudio: true,
        sourceBytes: 2048,
      });
    } finally {
      await view.unmount();
    }
  });
});
