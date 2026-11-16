import type { ReactElement } from "react";
import { afterAll, afterEach, beforeAll, describe, expect, it, mock } from "bun:test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import type { Segment, UpdateSegmentInput } from "@/api/drama/type";
import { SegmentTrimDrawer, type TrimSourceEngine } from "@/components/drama/segment-trim-drawer";
import { installDom } from "@/tests/helpers/dom";

let dom: Awaited<ReturnType<typeof installDom>>;
const cleanups: Array<() => Promise<void>> = [];
const unexpectedDomErrors: unknown[] = [];
const onWindowError = (event: ErrorEvent) => {
  unexpectedDomErrors.push(event.error ?? event.message);
};

beforeAll(async () => {
  dom = await installDom();
  dom.win.addEventListener("error", onWindowError);
  const winRecord = dom.win as unknown as Record<string, unknown>;
  if (!winRecord.PointerEvent) {
    winRecord.PointerEvent = dom.win.MouseEvent;
    (globalThis as unknown as Record<string, unknown>).PointerEvent = dom.win.MouseEvent;
  }
  const mediaProto = dom.win.HTMLMediaElement?.prototype as
    | { pause?: () => void; play?: () => Promise<void> }
    | undefined;
  if (mediaProto) {
    mediaProto.pause = () => {};
    mediaProto.play = async () => {};
  }
});

afterEach(async () => {
  while (cleanups.length > 0) {
    const fn = cleanups.pop();
    if (fn) await fn();
  }
  (dom.win.document.activeElement as HTMLElement | null)?.blur?.();
  dom.win.getSelection()?.removeAllRanges();
  await dom.flush(0);
  expect(unexpectedDomErrors.splice(0)).toEqual([]);
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

async function mountDrawer(initial: ReactElement) {
  const container = dom.win.document.createElement("div");
  dom.win.document.body.appendChild(container);
  let root!: ReturnType<typeof dom.createRoot>;
  await dom.act(async () => {
    root = dom.createRoot(container);
    root.render(initial);
  });
  const unmount = async () => {
    await dom.act(async () => {
      root.unmount();
      container.remove();
    });
  };
  cleanups.push(unmount);
  return {
    container,
    rerender: (next: ReactElement) => dom.act(async () => root.render(next)),
    unmount,
  };
}

function makeFakeEngine(hasAudio = true, frames = 124): TrimSourceEngine {
  return {
    probe: async () => ({
      duration: frames / 24,
      hasAudio,
      width: 1280,
      height: 720,
    }),
    thumbnails: async () => ["blob:frame-1", "blob:frame-2", "blob:frame-3"],
    encode: async () => {
      throw new Error("SegmentTrimDrawer must never call encode");
    },
  };
}

function makeSegment(overrides: Partial<Segment> = {}): Segment {
  const taskId = overrides.chosenTaskId ?? "task-1";
  const url = `https://example.test/${overrides.id ?? "seg-1"}-${taskId}.mp4`;
  return {
    id: "seg-1",
    episodeNo: 1,
    seq: 0,
    seconds: 124 / 24,
    prompt: "特写镜头",
    chosenTaskId: taskId,
    sourceFrameCount: 124,
    hasAudio: true,
    edit: {
      sourceInF: 0,
      sourceOutF: null,
      muteOriginal: false,
      audioGainDb: 0,
      nativeIsDialogue: false,
      avPtsOffsetMs: 0,
    },
    takes: [
      {
        taskId: "task-1",
        status: "succeeded",
        asset: { id: "asset-1", url, width: 1280, height: 720, mimeType: "video/mp4" } as never,
        errorMessage: "",
        createdAt: "2026-01-01T00:00:00Z",
      },
      {
        taskId: "task-2",
        status: "succeeded",
        asset: {
          id: "asset-2",
          url: `https://example.test/${overrides.id ?? "seg-1"}-task-2.mp4`,
          width: 1280,
          height: 720,
          mimeType: "video/mp4",
        } as never,
        errorMessage: "",
        createdAt: "2026-01-01T00:01:00Z",
      },
    ],
    status: "idle",
    revision: 3,
    updatedAt: "2026-01-01T00:00:00Z",
    adaptations: {},
    ...overrides,
  };
}

function q<T extends Element>(selector: string): T | null {
  return dom.win.document.body.querySelector<T>(selector);
}

function buttonByText(label: string): HTMLButtonElement | undefined {
  return Array.from(dom.win.document.body.querySelectorAll("button")).find(
    (btn) => btn.textContent?.trim() === label || btn.textContent?.includes(label),
  );
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

describe("SegmentTrimDrawer", () => {
  it("supports 24fps integer-frame keyboard stepping, minGap=1 and out-1 inclusive preview", async () => {
    const seg = makeSegment({
      id: "seg-trim-19f",
      edit: {
        sourceInF: 24,
        sourceOutF: 43,
        muteOriginal: false,
        audioGainDb: 0,
        nativeIsDialogue: false,
        avPtsOffsetMs: 0,
      },
    });
    const saved: Array<Omit<UpdateSegmentInput, "revision">> = [];
    const savedOptions: Array<{ expectedRevision?: number } | undefined> = [];
    const onSave = mock(
      async (
        input: Omit<UpdateSegmentInput, "revision">,
        options?: { expectedRevision?: number },
      ) => {
        saved.push(input);
        savedOptions.push(options);
      },
    );
    const onOpenChange = mock(() => {});

    await mountDrawer(
      <SegmentTrimDrawer
        open
        onOpenChange={onOpenChange}
        segment={seg}
        trimSourceEngine={makeFakeEngine(true, 124)}
        onSave={onSave}
      />,
    );
    await dom.flush(10);

    expect(dom.win.document.body.textContent).toContain("19 帧 · 0.79 s");
    expect(dom.win.document.body.textContent).toContain("末帧 f42");
    expect(q<HTMLInputElement>('input[aria-label="入点帧"]')?.value).toBe("24");
    expect(q<HTMLInputElement>('input[aria-label="出点帧"]')?.value).toBe("43");

    const startHandle = q<HTMLElement>('[data-trim-handle="start"]')!;
    const endHandle = q<HTMLElement>('[data-trim-handle="end"]')!;
    const video = q<HTMLVideoElement>("video")!;

    // 出点向后步进 +1 帧 -> out=44，预览帧停在 out - 1 = f43，绝不预览 f44
    await dom.act(async () => {
      dispatchKey(endHandle, "ArrowRight");
    });
    expect(q<HTMLInputElement>('input[aria-label="出点帧"]')?.value).toBe("44");
    expect(dom.win.document.body.textContent).toContain("预览帧 f43");
    expect(video.currentTime).toBeGreaterThanOrEqual(43 / 24);
    expect(video.currentTime).toBeLessThan(44 / 24);

    // 大步进 Shift+ArrowRight +10 帧 -> out=54，预览停在 f53
    await dom.act(async () => {
      dispatchKey(endHandle, "ArrowRight", true);
    });
    expect(q<HTMLInputElement>('input[aria-label="出点帧"]')?.value).toBe("54");
    expect(dom.win.document.body.textContent).toContain("预览帧 f53");

    // 播放到 out 边界时立即暂停并回到入点，绝不落在 out 帧
    await dom.act(async () => {
      video.currentTime = 54 / 24;
      video.dispatchEvent(new dom.win.Event("timeupdate", { bubbles: true }));
    });
    expect(dom.win.document.body.textContent).toContain("预览帧 f24");

    // 入点向前尝试越界（Shift+ArrowRight x3 尝试从 24 推到 54，被 minGap=1 拦在 53）
    await dom.act(async () => {
      dispatchKey(startHandle, "ArrowRight", true); // 34
    });
    await dom.act(async () => {
      dispatchKey(startHandle, "ArrowRight", true); // 44
    });
    await dom.act(async () => {
      dispatchKey(startHandle, "ArrowRight", true); // clamped at 53
    });
    expect(q<HTMLInputElement>('input[aria-label="入点帧"]')?.value).toBe("53");

    await dom.click(q<HTMLButtonElement>('button[aria-label="保存剪辑"]')!);
    expect(onSave).toHaveBeenCalledTimes(1);
    expect(saved[0]).toEqual({
      expectedTaskId: "task-1",
      edit: { sourceInF: 53, sourceOutF: 54 },
    });
    expect(savedOptions[0]).toEqual({ expectedRevision: 3 });
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it('restores full length ("恢复全长") with explicit sourceOutF: null while preserving audio settings', async () => {
    const seg = makeSegment({
      id: "seg-restore-full",
      edit: {
        sourceInF: 24,
        sourceOutF: 43,
        muteOriginal: false,
        audioGainDb: -6,
        nativeIsDialogue: true,
        avPtsOffsetMs: 40,
      },
    });
    const saved: Array<Omit<UpdateSegmentInput, "revision">> = [];

    await mountDrawer(
      <SegmentTrimDrawer
        open
        onOpenChange={() => {}}
        segment={seg}
        trimSourceEngine={makeFakeEngine(true, 124)}
        onSave={async (input) => {
          saved.push(input);
        }}
      />,
    );
    await dom.flush(10);

    const restoreBtn = buttonByText("恢复全长")!;
    expect(restoreBtn.disabled).toBe(false);
    await dom.click(restoreBtn);

    expect(q<HTMLInputElement>('input[aria-label="入点帧"]')?.value).toBe("0");
    expect(dom.win.document.body.textContent).toContain("全长 (EOF)");
    expect(restoreBtn.disabled).toBe(true);

    await dom.click(q<HTMLButtonElement>('button[aria-label="保存剪辑"]')!);
    expect(saved).toHaveLength(1);
    expect(saved[0]).toEqual({
      expectedTaskId: "task-1",
      edit: { sourceInF: 0, sourceOutF: null },
    });
    expect(saved[0]?.edit && "sourceOutF" in saved[0].edit).toBe(true);
    expect(saved[0]?.edit?.sourceOutF).toBeNull();
  });

  it("allows full-length mute-only, clamped audioGainDb (-60..+12), avPtsOffsetMs and nativeIsDialogue", async () => {
    const seg = makeSegment({ id: "seg-audio-controls" });
    const saved: Array<Omit<UpdateSegmentInput, "revision">> = [];

    await mountDrawer(
      <SegmentTrimDrawer
        open
        onOpenChange={() => {}}
        segment={seg}
        trimSourceEngine={makeFakeEngine(true, 124)}
        onSave={async (input) => {
          saved.push(input);
        }}
      />,
    );
    await dom.flush(10);

    // 初始未改参数，保存按钮禁用
    const saveBtn = q<HTMLButtonElement>('button[aria-label="保存剪辑"]')!;
    expect(saveBtn.disabled).toBe(true);

    // 调整增益（超限输入自动钳制到 -60..+12）
    const gainInput = q<HTMLInputElement>('input[aria-label="原声增益 (dB)"]')!;
    await dom.type(gainInput, "20");
    await dom.act(async () => {
      gainInput.dispatchEvent(new dom.win.FocusEvent("focusout", { bubbles: true }));
    });
    expect(gainInput.value).toBe("12");

    // 调整音画偏移
    const ptsInput = q<HTMLInputElement>('input[aria-label="音画偏移 (ms)"]')!;
    await dom.type(ptsInput, "-150");
    await dom.act(async () => {
      ptsInput.dispatchEvent(new dom.win.FocusEvent("focusout", { bubbles: true }));
    });
    expect(ptsInput.value).toBe("-150");

    // 切换人物对白开关
    const dialogueSwitch = q<HTMLElement>('[aria-label="原声对白"]')!;
    await dom.click(dialogueSwitch);

    // 开启全长静音（即便范围是全长 [0, null) 也允许保存）
    const muteBtn = q<HTMLButtonElement>('button[aria-label="原声静音"]')!;
    await dom.click(muteBtn);
    expect(saveBtn.disabled).toBe(false);

    await dom.click(saveBtn);
    expect(saved).toEqual([
      {
        expectedTaskId: "task-1",
        edit: {
          muteOriginal: true,
          audioGainDb: 12,
          nativeIsDialogue: true,
          avPtsOffsetMs: -150,
        },
      },
    ]);
  });

  it('shows non-blocking "素材无原声" hint when hasAudio is false without blocking frame trim', async () => {
    const seg = makeSegment({ id: "seg-no-audio", hasAudio: false });
    const saved: Array<Omit<UpdateSegmentInput, "revision">> = [];

    await mountDrawer(
      <SegmentTrimDrawer
        open
        onOpenChange={() => {}}
        segment={seg}
        hasAudio={false}
        trimSourceEngine={makeFakeEngine(false, 124)}
        onSave={async (input) => {
          saved.push(input);
        }}
      />,
    );
    await dom.flush(10);

    expect(dom.win.document.body.textContent).toContain("素材无原声");
    expect(q<HTMLButtonElement>('button[aria-label="原声静音"]')?.disabled).toBe(true);
    expect(q<HTMLInputElement>('input[aria-label="原声增益 (dB)"]')?.disabled).toBe(true);

    // 帧剪辑依然完全可用
    const startHandle = q<HTMLElement>('[data-trim-handle="start"]')!;
    await dom.act(async () => {
      dispatchKey(startHandle, "ArrowRight", true); // +10 帧
    });

    const saveBtn = q<HTMLButtonElement>('button[aria-label="保存剪辑"]')!;
    expect(saveBtn.disabled).toBe(false);
    await dom.click(saveBtn);
    expect(saved).toEqual([
      {
        expectedTaskId: "task-1",
        edit: { sourceInF: 10 },
      },
    ]);
  });

  it("displays explicit take-switch warning with keep/reset toggle", async () => {
    const seg = makeSegment({
      id: "seg-take-switch",
      chosenTaskId: "task-1",
      edit: {
        sourceInF: 24,
        sourceOutF: 43,
        muteOriginal: false,
        audioGainDb: -3,
        nativeIsDialogue: false,
        avPtsOffsetMs: 0,
      },
    });
    const saved: Array<Omit<UpdateSegmentInput, "revision">> = [];

    const firstView = await mountDrawer(
      <SegmentTrimDrawer
        open
        onOpenChange={() => {}}
        segment={seg}
        pendingTaskId="task-2"
        trimSourceEngine={makeFakeEngine(true, 124)}
        onSave={async (input) => {
          saved.push(input);
        }}
      />,
    );
    await dom.flush(10);

    const alertBanner = q<HTMLElement>('[role="alert"]');
    expect(alertBanner?.textContent).toContain("正在切换采用视频");
    expect(dom.win.document.body.textContent).toContain("切换时重置为全长与默认原声");
    expect(q<HTMLInputElement>('input[aria-label="入点帧"]')?.value).toBe("0");

    // 关闭「切换时重置精裁与原声」开关 -> 保留原 [24, 43) 与 -3dB
    const resetSwitch = q<HTMLElement>('[aria-label="切换时重置精裁与原声"]')!;
    await dom.click(resetSwitch);
    expect(dom.win.document.body.textContent).toContain("保留当前精裁与原声参数");
    expect(q<HTMLInputElement>('input[aria-label="入点帧"]')?.value).toBe("24");
    expect(q<HTMLInputElement>('input[aria-label="出点帧"]')?.value).toBe("43");

    await dom.click(q<HTMLButtonElement>('button[aria-label="保存剪辑"]')!);
    expect(saved[0]).toEqual({
      chosenTaskId: "task-2",
      expectedTaskId: "task-1",
      resetEditOnTaskChange: false,
    });
    await firstView.unmount();

    // 再次打开默认保持重置并保存
    const savedReset: Array<Omit<UpdateSegmentInput, "revision">> = [];
    await mountDrawer(
      <SegmentTrimDrawer
        open
        onOpenChange={() => {}}
        segment={seg}
        pendingTaskId="task-2"
        trimSourceEngine={makeFakeEngine(true, 124)}
        onSave={async (input) => {
          savedReset.push(input);
        }}
      />,
    );
    await dom.flush(10);
    await dom.click(q<HTMLButtonElement>('button[aria-label="保存剪辑"]')!);
    expect(savedReset[0]).toEqual({
      chosenTaskId: "task-2",
      expectedTaskId: "task-1",
      resetEditOnTaskChange: true,
    });
  });

  it("shows external conflicting-task warning when chosenTaskId changes underneath", async () => {
    const seg = makeSegment({ id: "seg-conflict", chosenTaskId: "task-1" });
    const view = await mountDrawer(
      <SegmentTrimDrawer
        open
        onOpenChange={() => {}}
        segment={seg}
        trimSourceEngine={makeFakeEngine(true, 124)}
        onSave={async () => {}}
      />,
    );
    await dom.flush(10);

    expect(q('[role="alert"]')).toBeNull();

    await view.rerender(
      <SegmentTrimDrawer
        open
        onOpenChange={() => {}}
        segment={{ ...seg, chosenTaskId: "task-2" }}
        trimSourceEngine={makeFakeEngine(true, 124)}
        onSave={async () => {}}
      />,
    );
    expect(q('[role="alert"]')?.textContent).toContain("采用视频已在外部切换");
  });

  it("freezes baseline revision & edit on external revision bump and requires reloading before CAS save", async () => {
    const seg = makeSegment({
      id: "seg-rev-cas",
      revision: 3,
      edit: {
        sourceInF: 12,
        sourceOutF: 60,
        muteOriginal: false,
        audioGainDb: 0,
        nativeIsDialogue: false,
        avPtsOffsetMs: 0,
      },
    });
    const saved: Array<Omit<UpdateSegmentInput, "revision">> = [];
    const savedOptions: Array<{ expectedRevision?: number } | undefined> = [];

    const view = await mountDrawer(
      <SegmentTrimDrawer
        open
        onOpenChange={() => {}}
        segment={seg}
        trimSourceEngine={makeFakeEngine(true, 124)}
        onSave={async (input, options) => {
          saved.push(input);
          savedOptions.push(options);
        }}
      />,
    );
    await dom.flush(10);

    // 本地修改出点为 72
    const outInput = q<HTMLInputElement>('input[aria-label="出点帧"]')!;
    await dom.type(outInput, "72");
    await dom.act(async () => {
      dispatchKey(outInput, "Enter");
    });

    const saveBtn = q<HTMLButtonElement>('button[aria-label="保存剪辑"]')!;
    expect(saveBtn.disabled).toBe(false);

    // 外部同 task 修改并递增 revision=4
    const externalSeg = makeSegment({
      id: "seg-rev-cas",
      revision: 4,
      edit: {
        sourceInF: 50,
        sourceOutF: 81,
        muteOriginal: true,
        audioGainDb: 0,
        nativeIsDialogue: false,
        avPtsOffsetMs: 0,
      },
    });
    await view.rerender(
      <SegmentTrimDrawer
        open
        onOpenChange={() => {}}
        segment={externalSeg}
        trimSourceEngine={makeFakeEngine(true, 124)}
        onSave={async (input, options) => {
          saved.push(input);
          savedOptions.push(options);
        }}
      />,
    );

    expect(q('[role="alert"]')?.textContent).toContain("已在别处修改");
    expect(saveBtn.disabled).toBe(true);

    // 点「载入最新版本」切换到 revision=4 基线后再修改保存，携带 expectedRevision=4 与新 diff
    const reloadBtn = buttonByText("载入最新版本")!;
    expect(reloadBtn).toBeDefined();
    await dom.click(reloadBtn);

    expect(q<HTMLInputElement>('input[aria-label="入点帧"]')?.value).toBe("50");
    expect(q<HTMLInputElement>('input[aria-label="出点帧"]')?.value).toBe("81");
    expect(q('[role="alert"]')).toBeNull();

    await dom.type(outInput, "90");
    await dom.act(async () => {
      dispatchKey(outInput, "Enter");
    });
    expect(saveBtn.disabled).toBe(false);
    await dom.click(saveBtn);
    expect(saved[0]).toEqual({
      expectedTaskId: "task-1",
      edit: { sourceOutF: 90 },
    });
    expect(savedOptions[0]).toEqual({ expectedRevision: 4 });
  });

  it("seeks <video> to sourceInF on loadedmetadata and before play when starting from frame 0", async () => {
    const seg = makeSegment({
      id: "seg-initial-seek",
      edit: {
        sourceInF: 24,
        sourceOutF: 72,
        muteOriginal: false,
        audioGainDb: 0,
        nativeIsDialogue: false,
        avPtsOffsetMs: 0,
      },
    });

    await mountDrawer(
      <SegmentTrimDrawer
        open
        onOpenChange={() => {}}
        segment={seg}
        trimSourceEngine={makeFakeEngine(true, 124)}
        onSave={async () => {}}
      />,
    );
    await dom.flush(10);

    const video = q<HTMLVideoElement>("video")!;
    video.currentTime = 0;
    await dom.act(async () => {
      video.dispatchEvent(new dom.win.Event("loadedmetadata"));
    });
    expect(video.currentTime).toBeCloseTo(24.5 / 24, 3);

    // 若浏览器重置了播放头到 0，点播放预览时也先 seek 到入点再播放
    video.currentTime = 0;
    await dom.click(q<HTMLButtonElement>('button[aria-label="播放预览"]')!);
    expect(video.currentTime).toBeCloseTo(24.5 / 24, 3);
  });

  it("allows multi-digit typing into 出点帧 & 增益 without keystroke clamping, then commits on Enter/blur", async () => {
    const seg = makeSegment({
      id: "seg-typing",
      edit: {
        sourceInF: 24,
        sourceOutF: 43,
        muteOriginal: false,
        audioGainDb: 0,
        nativeIsDialogue: false,
        avPtsOffsetMs: 0,
      },
    });
    const saved: Array<Omit<UpdateSegmentInput, "revision">> = [];

    await mountDrawer(
      <SegmentTrimDrawer
        open
        onOpenChange={() => {}}
        segment={seg}
        trimSourceEngine={makeFakeEngine(true, 124)}
        onSave={async (input) => {
          saved.push(input);
        }}
      />,
    );
    await dom.flush(10);

    const outInput = q<HTMLInputElement>('input[aria-label="出点帧"]')!;
    // 先键入 "8"（此时小于入点 24，不应被强行夹成 25），再键入 "81"，回车提交
    await dom.type(outInput, "8");
    expect(outInput.value).toBe("8");
    await dom.type(outInput, "81");
    expect(outInput.value).toBe("81");
    await dom.act(async () => {
      dispatchKey(outInput, "Enter");
    });
    expect(dom.win.document.body.textContent).toContain("57 帧");

    // 增益允许键入中间越界数字或负值并在失焦后钳制提交
    const gainInput = q<HTMLInputElement>('input[aria-label="原声增益 (dB)"]')!;
    await dom.type(gainInput, "18");
    expect(gainInput.value).toBe("18");
    await dom.type(gainInput, "-12");
    expect(gainInput.value).toBe("-12");
    await dom.act(async () => {
      gainInput.dispatchEvent(new dom.win.FocusEvent("focusout", { bubbles: true }));
    });
    expect(gainInput.value).toBe("-12");

    await dom.click(q<HTMLButtonElement>('button[aria-label="保存剪辑"]')!);
    expect(saved[0]).toEqual({
      expectedTaskId: "task-1",
      edit: { sourceOutF: 81, audioGainDb: -12 },
    });
  });

  it("uses target take probe hasAudio when switching from a no-audio adopted take to a take with audio", async () => {
    const seg = makeSegment({
      id: "seg-switch-no-audio-to-audio",
      chosenTaskId: "task-1",
      hasAudio: false,
    });
    const saved: Array<Omit<UpdateSegmentInput, "revision">> = [];
    const perTakeEngine: TrimSourceEngine = {
      probe: async (src) => ({
        duration: 124 / 24,
        hasAudio: src.includes("task-2"),
        width: 1280,
        height: 720,
      }),
      thumbnails: async () => ["blob:frame-1", "blob:frame-2", "blob:frame-3"],
      encode: async () => {
        throw new Error("SegmentTrimDrawer must never call encode");
      },
    };

    await mountDrawer(
      <SegmentTrimDrawer
        open
        onOpenChange={() => {}}
        segment={seg}
        pendingTaskId="task-2"
        trimSourceEngine={perTakeEngine}
        onSave={async (input) => {
          saved.push(input);
        }}
      />,
    );
    await dom.flush(10);

    expect(dom.win.document.body.textContent).not.toContain("素材无原声");
    const muteBtn = q<HTMLButtonElement>('button[aria-label="原声静音"]')!;
    const gainInput = q<HTMLInputElement>('input[aria-label="原声增益 (dB)"]')!;
    const ptsInput = q<HTMLInputElement>('input[aria-label="音画偏移 (ms)"]')!;
    const dialogueSwitch = q<HTMLElement>('[aria-label="原声对白"]')!;
    expect(muteBtn.disabled).toBe(false);
    expect(gainInput.disabled).toBe(false);
    expect(ptsInput.disabled).toBe(false);
    expect(dialogueSwitch.getAttribute("aria-disabled")).not.toBe("true");
    expect(dialogueSwitch.hasAttribute("data-disabled")).toBe(false);

    // 新 take 有原声，可以直接调整增益和静音并保存切换
    await dom.type(gainInput, "-3");
    await dom.act(async () => {
      gainInput.dispatchEvent(new dom.win.FocusEvent("focusout", { bubbles: true }));
    });
    await dom.click(muteBtn);
    await dom.click(q<HTMLButtonElement>('button[aria-label="保存剪辑"]')!);

    expect(saved).toEqual([
      {
        chosenTaskId: "task-2",
        expectedTaskId: "task-1",
        resetEditOnTaskChange: false,
        edit: { muteOriginal: true, audioGainDb: -3 },
      },
    ]);
  });

  it("disables audio controls when switching to a no-audio target take and preserves segment.hasAudio on normal edit", async () => {
    const segWithAudio = makeSegment({
      id: "seg-switch-audio-to-no-audio",
      chosenTaskId: "task-1",
      hasAudio: true,
    });
    const switchView = await mountDrawer(
      <SegmentTrimDrawer
        open
        onOpenChange={() => {}}
        segment={segWithAudio}
        pendingTaskId="task-2"
        trimSourceEngine={makeFakeEngine(false, 124)}
        onSave={async () => {}}
      />,
    );
    await dom.flush(10);

    expect(dom.win.document.body.textContent).toContain("素材无原声");
    expect(q<HTMLButtonElement>('button[aria-label="原声静音"]')?.disabled).toBe(true);
    expect(q<HTMLInputElement>('input[aria-label="原声增益 (dB)"]')?.disabled).toBe(true);
    await switchView.unmount();

    // 非切换场景（isTakeSwitch === false）且未传 hasAudio prop 时，仍沿用 segment.hasAudio
    const segNormalNoAudio = makeSegment({
      id: "seg-normal-no-audio-prop-omitted",
      chosenTaskId: "task-1",
      hasAudio: false,
    });
    await mountDrawer(
      <SegmentTrimDrawer
        open
        onOpenChange={() => {}}
        segment={segNormalNoAudio}
        trimSourceEngine={makeFakeEngine(false, 124)}
        onSave={async () => {}}
      />,
    );
    await dom.flush(10);

    expect(dom.win.document.body.textContent).toContain("素材无原声");
    expect(q<HTMLButtonElement>('button[aria-label="原声静音"]')?.disabled).toBe(true);
    expect(q<HTMLInputElement>('input[aria-label="原声增益 (dB)"]')?.disabled).toBe(true);
  });

  it("never imports or calls trimRunner.start / createFrameRun / /canvas/frame-runs", () => {
    const drawerSrc = readFileSync(
      resolve(process.cwd(), "src/components/drama/segment-trim-drawer.tsx"),
      "utf8",
    );
    const utilsSrc = readFileSync(
      resolve(process.cwd(), "src/utils/drama/segment-trim.ts"),
      "utf8",
    );
    for (const forbidden of ["trimRunner", "createFrameRun", "/canvas/frame-runs"]) {
      expect(drawerSrc).not.toContain(forbidden);
      expect(utilsSrc).not.toContain(forbidden);
    }
  });
});
