import { useState } from "react";
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

import type { Drama, Segment, UpdateSegmentInput } from "@/api/drama/type";
import { ClipBar } from "@/pages/drama/studio/clip-bar";
import { Preview } from "@/pages/drama/studio/preview";
import { useProgram } from "@/pages/drama/studio/program/use-program";
import { TakeCarousel } from "@/pages/drama/studio/take-carousel";
import { PlaybackContext, usePlayback } from "@/pages/drama/studio/use-playback";
import { StudioContext, type Studio } from "@/pages/drama/studio/use-studio";
import { DramaWorkspaceContext, type DramaWorkspace } from "@/pages/drama/workspace/context";
import { FinalArea } from "@/pages/drama/workspace/final-area";
import { useDramaJobsStore } from "@/store/drama";
import { useDramaSegmentsStore } from "@/store/drama-segments";
import { installDom, type DomEnv } from "@/tests/helpers/dom";
import { trimEngine } from "@/utils/canvas/trim-engine";
import { DRAMA_ERR } from "@/api/drama/errors";
import { ApiError } from "@/utils/requests/request";
import service from "@/utils/requests/service";

let dom: DomEnv;
const spies: Array<{ mockRestore: () => void }> = [];

beforeAll(async () => {
  dom = await installDom();
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

beforeEach(() => {
  spies.push(
    spyOn(trimEngine, "probe").mockResolvedValue({
      duration: 5,
      hasAudio: true,
      width: 1280,
      height: 720,
    }),
    spyOn(trimEngine, "thumbnails").mockResolvedValue(["blob:f1", "blob:f2"]),
    spyOn(trimEngine, "encode").mockRejectedValue(
      new Error("Studio trim flow must never call browser encode"),
    ),
  );
  useDramaJobsStore.setState({ byDrama: {}, settled: {}, watching: {} });
  useDramaSegmentsStore.setState({ byEpisode: {}, watching: {} });
});

afterEach(() => {
  while (spies.length > 0) {
    spies.pop()?.mockRestore();
  }
  dom.win.document.body.innerHTML = "";
});

afterAll(() => {
  dom.restore();
});

const makeSegment = (id: string, seq: number, patch: Partial<Segment> = {}): Segment => ({
  id,
  episodeNo: 1,
  seq,
  seconds: 5,
  prompt: `片段 ${seq + 1}`,
  takes: [
    {
      taskId: `t-${id}-1`,
      status: "succeeded",
      asset: {
        id: `a-${id}-1`,
        url: `https://example.test/${id}-1.mp4`,
        mimeType: "video/mp4",
        sizeBytes: 2048,
        width: 1280,
        height: 720,
        durationMs: 5000,
        createdAt: "2026-11-15T00:00:00Z",
      },
      errorMessage: "",
      createdAt: "2026-11-15T00:00:00Z",
    },
  ],
  chosenTaskId: `t-${id}-1`,
  sourceFrameCount: 120,
  hasAudio: true,
  edit: {
    sourceInF: 0,
    sourceOutF: null,
    muteOriginal: false,
    audioGainDb: 0,
    nativeIsDialogue: false,
    avPtsOffsetMs: 0,
  },
  status: "done",
  revision: 1,
  updatedAt: "2026-11-15T00:00:00Z",
  adaptations: {},
  ...patch,
});

const makeDrama = (patch: Partial<Drama> = {}): Drama =>
  ({
    id: "d-prev",
    title: "预览与导出短剧",
    canvasId: "c-prev",
    status: "ready",
    sourceKind: "script",
    genre: "悬疑",
    styleKey: "real_city_film",
    styleCustom: "",
    ratio: "16:9",
    targetEpisodes: 1,
    episodeSeconds: 30,
    textModel: "",
    videoModel: "seedance-2",
    imageModels: { character: "", scene: "", prop: "" },
    outlineMd: "",
    charactersMd: "",
    revision: 1,
    episodes: [
      {
        no: 1,
        title: "序幕",
        synopsis: "",
        scriptStatus: "ready",
        locked: true,
        revision: 1,
        updatedAt: "",
        storyboardStatus: "ready",
        segmentCount: 2,
        segmentSeconds: 10,
        doneSegmentCount: 1,
        final: {
          id: "final-1",
          url: "https://example.test/final-default.mp4",
          mimeType: "video/mp4",
          sizeBytes: 4096,
          width: 1280,
          height: 720,
          durationMs: 10000,
          createdAt: "2026-11-15T00:00:00Z",
        },
        finalStale: false,
        hook: "",
        cliffhanger: "",
        unitFrom: null,
        unitTo: null,
        segmentVersion: 3,
        editSchema: 1,
      },
    ],
    activeJobs: [],
    createdAt: "",
    updatedAt: "",
    sourceChars: 0,
    unitsTotal: 1,
    unitKind: "episode",
    phase: "done",
    planCursor: 1,
    rangeEnd: 1,
    pauseReason: "",
    pauseCode: "",
    promptSettings: {
      adapt: { source: "builtin", label: "内置" },
      assets: { source: "builtin", label: "内置" },
      asset_prompts: { source: "builtin", label: "内置" },
      video: { source: "builtin", label: "内置" },
    },
    ...patch,
  }) as Drama;

function StudioPreviewHarness({
  initialDrama,
  initialSegments,
  onPatch,
  enableProgram = false,
  onRefresh,
}: {
  initialDrama: Drama;
  initialSegments: Segment[];
  onPatch: (sid: string, fields: Omit<UpdateSegmentInput, "revision">) => Promise<Segment>;
  enableProgram?: boolean;
  onRefresh?: () => Promise<void>;
}) {
  const [drama, setDrama] = useState(initialDrama);
  const [segments, setSegments] = useState(initialSegments);
  const [selectedId, setSelectedId] = useState(initialSegments[0]?.id ?? "");
  const [trimTarget, setTrimTarget] = useState<{
    segmentId: string;
    pendingTaskId?: string | null;
  } | null>(null);
  const playback = usePlayback(segments);
  const selected = segments.find((s) => s.id === selectedId) ?? segments[0] ?? null;
  const program = useProgram({
    dramaId: drama.id,
    episodeId: drama.episodes[0]?.id,
    episodeNo: 1,
    enabled: enableProgram && drama.ratio === "16:9",
    segmentVersion: drama.episodes[0]?.segmentVersion,
  });

  const studio = {
    drama,
    episode: drama.episodes[0]!,
    no: 1,
    segments,
    segmentsStatus: "ready",
    selected,
    select: setSelectedId,
    assets: [],
    assetsStatus: "ready",
    video: { models: [], status: "ready", model: undefined, setModel: () => {} },
    overrides: {},
    setOverride: () => {},
    prompt: selected?.prompt ?? "",
    drafts: {},
    setDraft: () => {},
    format: "seedance",
    adapting: new Set<string>(),
    adaptErrors: {},
    adaptStateOf: () => "fresh",
    plan: null,
    credits: undefined,
    availableCredits: 500,
    save: "saved",
    reportSave: () => {},
    refresh: () => {
      void onRefresh?.();
    },
    ...(enableProgram ? { program } : {}),
    submitting: new Set<string>(),
    registerEditor: () => () => {},
    trimDrawer: {
      target: trimTarget,
      open: (segmentId: string, pendingTaskId?: string | null) =>
        setTrimTarget({ segmentId, pendingTaskId: pendingTaskId ?? null }),
      close: () => setTrimTarget(null),
    },
    actions: {
      add: async () => {},
      remove: async () => {},
      reorder: async () => {},
      setSeconds: async () => {},
      patch: onPatch,
      choose: async () => {},
      generate: async () => {},
      batch: async () => {},
      adapt: async () => true,
    },
  } as unknown as Studio;

  return (
    <StudioContext.Provider value={studio}>
      <PlaybackContext.Provider value={playback}>
        <button
          type="button"
          data-testid="bump-version"
          onClick={() => {
            setSegments((prev) =>
              prev.map((item, idx) =>
                idx === 0 ? { ...item, revision: item.revision + 1 } : item,
              ),
            );
            setDrama((prev) => ({
              ...prev,
              episodes: prev.episodes.map((ep) =>
                ep.no === 1 ? { ...ep, segmentVersion: (ep.segmentVersion ?? 1) + 1 } : ep,
              ),
            }));
          }}
        >
          升版本
        </button>
        <Preview />
        <ClipBar />
      </PlaybackContext.Provider>
    </StudioContext.Provider>
  );
}

describe("片场修剪入口联动、CPU 整集预览与成片导出（FinalArea）", () => {
  test("Preview 与 ClipBar 的「修剪」打开 SegmentTrimDrawer 并通过 actions.patch 保存，91003 在抽屉内就地告警", async () => {
    const s1 = makeSegment("s1", 0);
    const patchCalls: Array<{ sid: string; fields: Omit<UpdateSegmentInput, "revision"> }> = [];
    let shouldFail91003 = false;

    const mounted = await dom.mount(
      <StudioPreviewHarness
        initialDrama={makeDrama()}
        initialSegments={[s1]}
        onPatch={async (sid, fields) => {
          if (shouldFail91003) {
            throw new ApiError("入点帧不能超过出点帧", 91003, 400);
          }
          patchCalls.push({ sid, fields });
          return { ...s1, edit: { ...s1.edit!, ...fields.edit }, revision: s1.revision + 1 };
        }}
      />,
    );

    try {
      // 点击 ClipBar / Preview 中的「修剪」打开抽屉
      const trimBtns = Array.from(mounted.container.querySelectorAll("button")).filter((b) =>
        b.textContent?.includes("修剪"),
      );
      expect(trimBtns.length).toBeGreaterThanOrEqual(2);
      await dom.click(trimBtns[trimBtns.length - 1]);
      await dom.flush();

      expect(dom.win.document.body.textContent).toContain("镜头 #1 精裁与原声");

      // 点静音开关并保存
      const muteBtn = Array.from(dom.win.document.body.querySelectorAll("button")).find((b) =>
        /静音|取消静音|原声/.test(b.getAttribute("aria-label") ?? b.textContent ?? ""),
      );
      expect(muteBtn).toBeDefined();
      await dom.click(muteBtn);
      await dom.flush();

      // 先测试 91003 错误就地留抽屉不崩溃
      shouldFail91003 = true;
      const saveBtn = Array.from(dom.win.document.body.querySelectorAll("button")).find(
        (b) => b.getAttribute("aria-label") === "保存剪辑",
      );
      expect(saveBtn).toBeDefined();
      await dom.click(saveBtn);
      await dom.flush();
      expect(dom.win.document.body.textContent).toContain("入点帧不能超过出点帧");

      // 恢复正常保存：调用 actions.patch 并关闭抽屉
      shouldFail91003 = false;
      await dom.click(saveBtn);
      await dom.flush();
      expect(patchCalls.length).toBe(1);
      expect(patchCalls[0]?.sid).toBe("s1");
      expect(patchCalls[0]?.fields.edit?.muteOriginal).toBe(true);
    } finally {
      await mounted.unmount();
    }
  });

  test("TakeCarousel 在聚焦可播放版本时展示「修剪」并回传目标 taskId", async () => {
    const g = globalThis as unknown as { ResizeObserver?: typeof ResizeObserver };
    const win = dom.win as unknown as { ResizeObserver?: typeof ResizeObserver };
    const prevGlobalRO = g.ResizeObserver;
    const prevWinRO = win.ResizeObserver;
    class ImmediateResizeObserver {
      private cb: ResizeObserverCallback;
      constructor(cb: ResizeObserverCallback) {
        this.cb = cb;
      }
      observe(target: Element) {
        this.cb(
          [
            {
              target,
              contentRect: {
                width: 800,
                height: 500,
                top: 0,
                left: 0,
                right: 800,
                bottom: 500,
                x: 0,
                y: 0,
                toJSON: () => {},
              } as DOMRectReadOnly,
              borderBoxSize: [],
              contentBoxSize: [],
              devicePixelContentBoxSize: [],
            },
          ],
          this as unknown as ResizeObserver,
        );
      }
      unobserve() {}
      disconnect() {}
    }
    g.ResizeObserver = ImmediateResizeObserver as unknown as typeof ResizeObserver;
    win.ResizeObserver = ImmediateResizeObserver as unknown as typeof ResizeObserver;

    spies.push(
      spyOn(dom.win.HTMLElement.prototype, "getBoundingClientRect").mockReturnValue({
        width: 800,
        height: 500,
        top: 0,
        left: 0,
        right: 800,
        bottom: 500,
        x: 0,
        y: 0,
        toJSON: () => {},
      }),
    );

    const s1 = makeSegment("s1", 0);
    const trimmedIds: string[] = [];
    const mounted = await dom.mount(
      <div style={{ width: 800, height: 500 }}>
        <TakeCarousel
          takes={s1.takes}
          chosenTaskId={s1.chosenTaskId}
          aspect={16 / 9}
          onChoose={() => {}}
          onTrim={(taskId) => trimmedIds.push(taskId)}
        />
      </div>,
    );
    try {
      await dom.flush();
      const trimBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("修剪"),
      );
      expect(trimBtn).toBeDefined();
      await dom.click(trimBtn);
      expect(trimmedIds).toEqual(["t-s1-1"]);
    } finally {
      await mounted.unmount();
      g.ResizeObserver = prevGlobalRO;
      win.ResizeObserver = prevWinRO;
    }
  });

  test("远端新增第一条音频或遇 91001 重试时刷新节目单，Preview 从 v1 切到 manifest?v=2", async () => {
    const s1 = makeSegment("s1", 0);
    let serverVersion = 1;
    const get = spyOn(service, "get").mockImplementation(async (url: string) => {
      if (url === "/dramas/d-prev/episodes/1/program") {
        return {
          episode_id: "ep-1",
          segment_version: serverVersion,
          edit_schema: 1,
          program: {
            v: 1,
            overlays: [],
            audio:
              serverVersion === 1
                ? []
                : [{ id: "remote-bgm", role: "bgm", asset_id: 12, start_f: 0, end_f: 48 }],
          },
        } as never;
      }
      if (url === "/dramas/d-prev/episodes/1/manifest") {
        return {
          manifest: { v: 1, fps: 24, total_frames: 120, clips: [] },
          manifest_hash: "v1-hash",
          segment_version: 1,
          skipped_seqs: [],
          diagnostics: [],
        } as never;
      }
      if (url === "/dramas/d-prev/episodes/1/manifest?v=2") {
        return {
          status: "ready",
          manifest_hash: "v2-hash",
          segment_version: 2,
          skipped_seqs: [],
          program_diagnostics: [],
          diagnostics: [],
          manifest: {
            v: 2,
            render_version: 2,
            episode_id: "ep-1",
            segment_version: 2,
            fps: 24,
            sample_rate: 48000,
            total_frames: 120,
            skipped_seqs: [],
            clips: [
              {
                segment_id: "s1",
                visual_kind: "video",
                task_id: "t-s1-1",
                asset_id: 10,
                sha256: "a".repeat(64),
                seq: 0,
                source_in_f: 0,
                source_out_f: 120,
                master_in_f: 0,
                master_out_f: 120,
              },
            ],
            program: {
              v: 1,
              overlays: [],
              audio: [{ id: "remote-bgm", role: "bgm", asset_id: 12, start_f: 0, end_f: 48 }],
            },
          },
        } as never;
      }
      return {
        ticket: "tk-v2",
        status: "ready",
        stream_url: "/api/v1/dramas/d-prev/episodes/1/preview/tk-v2/stream?token=ok",
        expires_at: 1999999999,
      } as never;
    });
    const post = spyOn(service, "post").mockImplementation(async (url: string, body?: unknown) => {
      if (
        url === "/dramas/d-prev/episodes/1/preview" &&
        (body as { manifest_hash?: string } | undefined)?.manifest_hash === "v1-hash"
      ) {
        serverVersion = 2;
        throw new ApiError("清单已变更", 91001, 409);
      }
      return {
        ticket: "tk-v2",
        status: "ready",
        stream_url: "/api/v1/dramas/d-prev/episodes/1/preview/tk-v2/stream?token=ok",
        expires_at: 1999999999,
      } as never;
    });
    spies.push(get, post);

    const baseDrama = makeDrama();
    const mounted = await dom.mount(
      <StudioPreviewHarness
        enableProgram
        initialDrama={{
          ...baseDrama,
          episodes: [{ ...baseDrama.episodes[0]!, id: "ep-1", segmentVersion: 1 }],
        }}
        initialSegments={[s1]}
        onPatch={async () => s1}
      />,
    );
    try {
      await dom.flush(10);
      const previewBtn = [...mounted.container.querySelectorAll("button")].find(
        (b) => b.textContent?.trim() === "CPU 预览",
      );
      expect(previewBtn?.disabled).toBe(false);
      await dom.click(previewBtn);
      await dom.flush(30);
      expect(get.mock.calls.map(([url]) => url)).toContain(
        "/dramas/d-prev/episodes/1/manifest?v=2",
      );
      expect(
        post.mock.calls.some(
          ([, body]) => (body as { manifest_hash?: string }).manifest_hash === "v2-hash",
        ),
      ).toBe(true);
    } finally {
      await mounted.unmount();
    }
  });

  test("远端切换黑场触发 91001 时刷新片段列表，Preview 自动从 v1 切到 manifest?v=2", async () => {
    const s1 = makeSegment("s1", 0);
    let serverVersion = 1;
    let blackRemote = false;
    const get = spyOn(service, "get").mockImplementation(async (url: string) => {
      if (url === "/dramas/d-prev/episodes/1/program") {
        return {
          episode_id: "ep-1",
          segment_version: serverVersion,
          edit_schema: 1,
          program: { v: 1, overlays: [], audio: [] },
        } as never;
      }
      if (url === "/dramas/d-prev/episodes/1/segments") {
        return {
          items: [
            {
              id: "s1",
              episode_id: "ep-1",
              episode_no: 1,
              seq: 0,
              seconds: 5,
              prompt: "",
              visual_kind: blackRemote ? "black" : "video",
              takes: [],
              status: "idle",
              revision: blackRemote ? 2 : 1,
              segment_version: serverVersion,
              updated_at: "2026-11-15T00:00:00Z",
              adaptations: {},
            },
          ],
        } as never;
      }
      if (url === "/dramas/d-prev/episodes/1/manifest") {
        return {
          manifest: { v: 1, fps: 24, total_frames: 120, clips: [] },
          manifest_hash: "v1-hash",
          segment_version: 1,
          skipped_seqs: [],
          diagnostics: [],
        } as never;
      }
      if (url === "/dramas/d-prev/episodes/1/manifest?v=2") {
        return {
          status: "ready",
          manifest_hash: "v2-black-hash",
          segment_version: 2,
          skipped_seqs: [],
          program_diagnostics: [],
          diagnostics: [],
          manifest: {
            v: 2,
            render_version: 2,
            episode_id: "ep-1",
            segment_version: 2,
            fps: 24,
            sample_rate: 48000,
            total_frames: 120,
            skipped_seqs: [],
            clips: [
              {
                segment_id: "s1",
                visual_kind: "black",
                seq: 0,
                source_in_f: 0,
                source_out_f: 120,
                master_in_f: 0,
                master_out_f: 120,
              },
            ],
            program: { v: 1, overlays: [], audio: [] },
          },
        } as never;
      }
      return {
        ticket: "tk-black",
        status: "ready",
        stream_url: "/api/v1/dramas/d-prev/episodes/1/preview/tk-black/stream?token=ok",
        expires_at: 1999999999,
      } as never;
    });
    const post = spyOn(service, "post").mockImplementation(async (url: string, body?: unknown) => {
      if (
        url === "/dramas/d-prev/episodes/1/preview" &&
        (body as { manifest_hash?: string } | undefined)?.manifest_hash === "v1-hash"
      ) {
        serverVersion = 2;
        blackRemote = true;
        throw new ApiError("清单已变更", 91001, 409);
      }
      return {
        ticket: "tk-black",
        status: "ready",
        stream_url: "/api/v1/dramas/d-prev/episodes/1/preview/tk-black/stream?token=ok",
        expires_at: 1999999999,
      } as never;
    });
    spies.push(get, post);

    const baseDrama = makeDrama();
    const mounted = await dom.mount(
      <StudioPreviewHarness
        enableProgram
        initialDrama={{
          ...baseDrama,
          episodes: [{ ...baseDrama.episodes[0]!, id: "ep-1", segmentVersion: 1 }],
        }}
        initialSegments={[s1]}
        onPatch={async () => s1}
      />,
    );
    try {
      await dom.flush(10);
      const previewBtn = [...mounted.container.querySelectorAll("button")].find(
        (b) => b.textContent?.trim() === "CPU 预览",
      );
      await dom.click(previewBtn);
      await dom.flush(30);
      expect(get.mock.calls.map(([url]) => url)).toContain(
        "/dramas/d-prev/episodes/1/manifest?v=2",
      );
      expect(
        post.mock.calls.some(
          ([, body]) => (body as { manifest_hash?: string }).manifest_hash === "v2-black-hash",
        ),
      ).toBe(true);
    } finally {
      useDramaSegmentsStore.setState({ byEpisode: {}, watching: {} });
      await mounted.unmount();
    }
  });

  test("最后一个视频切片已被远端改为黑场导致 V1 返回 67010 时，自动刷新并切到 manifest?v=2", async () => {
    const s1 = makeSegment("s1", 0);
    let remoteBlack = false;
    const get = spyOn(service, "get").mockImplementation(async (url: string) => {
      if (url === "/dramas/d-prev/episodes/1/program") {
        return {
          episode_id: "ep-1",
          segment_version: remoteBlack ? 2 : 1,
          edit_schema: 1,
          program: { v: 1, overlays: [], audio: [] },
        } as never;
      }
      if (url === "/dramas/d-prev/episodes/1/manifest") {
        remoteBlack = true;
        throw new ApiError("这一集还没有可合成的片段", 67010, 400);
      }
      if (url === "/dramas/d-prev/episodes/1/segments") {
        return {
          episode_id: "ep-1",
          segment_version: 2,
          items: [
            {
              id: "s1",
              episode_id: "ep-1",
              episode_no: 1,
              seq: 0,
              seconds: 5,
              prompt: "",
              visual_kind: "black",
              takes: [],
              status: "idle",
              revision: 2,
              segment_version: 2,
              updated_at: "2026-11-15T00:00:00Z",
              adaptations: {},
            },
          ],
        } as never;
      }
      if (url === "/dramas/d-prev/episodes/1/manifest?v=2") {
        return {
          status: "ready",
          manifest_hash: "v2-67010-hash",
          segment_version: 2,
          skipped_seqs: [],
          program_diagnostics: [],
          diagnostics: [],
          manifest: {
            v: 2,
            render_version: 2,
            episode_id: "ep-1",
            segment_version: 2,
            fps: 24,
            sample_rate: 48000,
            total_frames: 120,
            skipped_seqs: [],
            clips: [
              {
                segment_id: "s1",
                visual_kind: "black",
                seq: 0,
                source_in_f: 0,
                source_out_f: 120,
                master_in_f: 0,
                master_out_f: 120,
              },
            ],
            program: { v: 1, overlays: [], audio: [] },
          },
        } as never;
      }
      return {
        ticket: "tk-67010",
        status: "ready",
        stream_url: "/api/v1/dramas/d-prev/episodes/1/preview/tk-67010/stream?token=ok",
        expires_at: 1999999999,
      } as never;
    });
    const post = spyOn(service, "post").mockResolvedValue({
      ticket: "tk-67010",
      status: "ready",
      stream_url: "/api/v1/dramas/d-prev/episodes/1/preview/tk-67010/stream?token=ok",
      expires_at: 1999999999,
    } as never);
    spies.push(get, post);

    const baseDrama = makeDrama();
    const mounted = await dom.mount(
      <StudioPreviewHarness
        enableProgram
        initialDrama={{
          ...baseDrama,
          episodes: [{ ...baseDrama.episodes[0]!, id: "ep-1", segmentVersion: 1 }],
        }}
        initialSegments={[s1]}
        onPatch={async () => s1}
      />,
    );
    try {
      await dom.flush(10);
      const previewBtn = [...mounted.container.querySelectorAll("button")].find(
        (b) => b.textContent?.trim() === "CPU 预览",
      );
      await dom.click(previewBtn);
      await dom.flush(30);
      expect(get.mock.calls.map(([url]) => url)).toContain(
        "/dramas/d-prev/episodes/1/manifest?v=2",
      );
      expect(
        post.mock.calls.some(
          ([, body]) => (body as { manifest_hash?: string }).manifest_hash === "v2-67010-hash",
        ),
      ).toBe(true);
    } finally {
      useDramaSegmentsStore.setState({ byEpisode: {}, watching: {} });
      await mounted.unmount();
    }
  });

  test("Stage C 的黑场预览使用 V2 清单并按权威时长夹紧当前镜头范围", async () => {
    const black = makeSegment("s1", 0, { visualKind: "black", takes: [], chosenTaskId: "" });
    const get = spyOn(service, "get").mockResolvedValue({
      status: "ready",
      manifest_hash: "v2-hash",
      segment_version: 3,
      skipped_seqs: [],
      program_diagnostics: [],
      diagnostics: [],
      manifest: {
        v: 2,
        render_version: 2,
        episode_id: "ep-1",
        segment_version: 3,
        fps: 24,
        sample_rate: 48000,
        total_frames: 36,
        skipped_seqs: [],
        clips: [
          {
            segment_id: "s1",
            visual_kind: "black",
            seq: 0,
            source_in_f: 0,
            source_out_f: 36,
            master_in_f: 0,
            master_out_f: 36,
          },
        ],
        program: { v: 1, audio: [], overlays: [] },
      },
    });
    const post = spyOn(service, "post").mockResolvedValue({
      status: "ready",
      ticket: "range-ticket",
      stream_url: "/preview/stream",
      expires_at: 1999999999,
    });
    spies.push(get, post);
    const mounted = await dom.mount(
      <StudioPreviewHarness
        initialDrama={makeDrama()}
        initialSegments={[black]}
        onPatch={async () => black}
      />,
    );
    try {
      const byLabel = (label: string) =>
        [...mounted.container.querySelectorAll("button")].find(
          (button) => button.textContent?.trim() === label,
        );
      await dom.click(byLabel("CPU 预览"));
      expect(get.mock.calls[0]?.[0]).toBe("/dramas/d-prev/episodes/1/manifest?v=2");
      await dom.click(byLabel("当前镜头"));
      expect(post.mock.calls.at(-1)?.[1]).toMatchObject({
        manifest_hash: "v2-hash",
        range_f: { in_f: 0, out_f: 36 },
      });
      const inputs = mounted.container.querySelectorAll('form[aria-label="预览帧范围"] input');
      await dom.type(inputs[0], "28");
      await dom.type(inputs[1], "999");
      await dom.click(byLabel("预览帧范围"));
      expect(post.mock.calls.at(-1)?.[1]).toMatchObject({ range_f: { in_f: 28, out_f: 36 } });
    } finally {
      await mounted.unmount();
    }
  });
  for (const source of ["create", "poll"] as const) {
    for (const code of [91012, 91013, 91001] as const) {
      test(`CPU 预览 ${source} 失败 ${code} 展示公开原因与节目单诊断`, async () => {
        const segment = makeSegment("s1", 0);
        const message =
          code === 91012
            ? "引用素材不可用或已变更"
            : code === 91013
              ? "成片混音未达到响度或真峰交付标准"
              : "清单已变更，请重新预览";
        const failed = {
          ticket: "failed-ticket",
          status: "failed",
          expires_at: Math.floor(Date.now() / 1000) + 60,
          error_code: code,
          error_message: `  ${message}  `,
          program_diagnostics: [
            {
              cue_id: "voice-failed",
              segment_id: "s1",
              field: "asset_id",
              reason: "media_unavailable",
              severity: "error",
            },
          ],
        };
        spies.push(
          spyOn(service, "get").mockImplementation(async (url: string) => {
            if (url.endsWith("/manifest"))
              return {
                manifest: { v: 1, fps: 24, total_frames: 120, clips: [] },
                manifest_hash: "hash",
                segment_version: 3,
                skipped_seqs: [],
                diagnostics: [],
              } as never;
            if (url.endsWith("/preview/failed-ticket")) return failed as never;
            return {} as never;
          }),
          spyOn(service, "post").mockResolvedValue(
            source === "create"
              ? failed
              : {
                  ticket: "failed-ticket",
                  status: "pending",
                  expires_at: Math.floor(Date.now() / 1000) + 60,
                },
          ),
        );
        const mounted = await dom.mount(
          <StudioPreviewHarness
            initialDrama={makeDrama()}
            initialSegments={[segment]}
            onPatch={async () => segment}
          />,
        );
        try {
          await dom.click(
            [...mounted.container.querySelectorAll("button")].find((b) =>
              b.textContent?.includes("CPU 预览"),
            ),
          );
          for (
            const deadline = Date.now() + 2000;
            Date.now() < deadline && !mounted.container.textContent?.includes(message);
          )
            await dom.flush(20);
          expect(mounted.container.textContent).toContain(message);
          expect(mounted.container.textContent).toContain("节目单诊断");
          expect(mounted.container.textContent).toContain("voice-failed");
          expect(mounted.container.textContent).toContain("s1");
          expect(mounted.container.textContent).toContain("media_unavailable");
          if (code === 91001) expect(mounted.container.textContent).toContain("清单版本已变更");
        } finally {
          await mounted.unmount();
        }
      });
    }
  }

  test("CPU 整集预览走 manifest -> createPreview -> poll -> <video src>，处理 91001 并随版本变更重建", async () => {
    const s1 = makeSegment("s1", 0);
    let manifestRound = 0;
    let createCalls = 0;
    let pollCalls = 0;
    let trigger91001Once = true;
    const previewPayloads: Array<{ body: unknown; withCredentials?: boolean; timeout?: number }> =
      [];

    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-prev/episodes/1/manifest") {
          manifestRound += 1;
          return {
            manifest: { v: 1, fps: 24, total_frames: 120 * manifestRound, clips: [] },
            manifest_hash: `hash-v${manifestRound}`,
            segment_version: manifestRound,
            skipped_seqs: [2],
            diagnostics: ["片段 1 音频按 48kHz 重采样"],
          } as never;
        }
        if (url.startsWith("/dramas/d-prev/episodes/1/preview/tk-")) {
          pollCalls += 1;
          return {
            ticket: url.split("/").pop(),
            status: "ready",
            stream_url: `/api/v1/dramas/d-prev/episodes/1/preview/${url.split("/").pop()}/stream?token=sig-ok`,
            expires_at: 1999999999,
          } as never;
        }
        return {} as never;
      }),
      spyOn(service, "post").mockImplementation(
        async (
          url: string,
          body?: unknown,
          config?: { withCredentials?: boolean; timeout?: number },
        ) => {
          if (url === "/dramas/d-prev/episodes/1/preview") {
            createCalls += 1;
            previewPayloads.push({
              body,
              withCredentials: config?.withCredentials,
              timeout: config?.timeout,
            });
            if (trigger91001Once) {
              trigger91001Once = false;
              throw new ApiError("分镜清单已变更", 91001, 409);
            }
            return {
              ticket: `tk-${createCalls}`,
              status: "pending",
              expires_at: 1999999999,
            } as never;
          }
          return {} as never;
        },
      ),
    );

    const mounted = await dom.mount(
      <StudioPreviewHarness
        initialDrama={makeDrama()}
        initialSegments={[s1]}
        onPatch={async () => s1}
      />,
    );
    try {
      const cpuBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("CPU 预览"),
      );
      expect(cpuBtn).toBeDefined();
      await dom.click(cpuBtn);
      // 等待 pending 轮询（400ms）与 91001 自动重拉完成
      await dom.flush(550);

      expect(previewPayloads[0]?.withCredentials).toBe(true);
      expect(previewPayloads[0]?.timeout).toBe(630_000);
      // 因第 1 次 create 返回 91001，自动刷新清单到 hash-v2 再创建 tk-2
      expect(previewPayloads.length).toBe(2);
      expect(
        (previewPayloads[1]?.body as { manifest_hash?: string } | undefined)?.manifest_hash,
      ).toBe("hash-v2");
      expect(pollCalls).toBeGreaterThanOrEqual(1);

      const video = mounted.container.querySelector('div[aria-label="整集 CPU 预览"] video');
      expect(video).not.toBeNull();
      expect(video?.getAttribute("src")).toContain(
        "/api/v1/dramas/d-prev/episodes/1/preview/tk-2/stream?token=sig-ok",
      );
      expect(mounted.container.textContent).toContain("03 暂无采用视频");
      expect(mounted.container.textContent).toContain("片段 1 音频按 48kHz 重采样");

      // segmentVersion 变更后旧预览立即失效并拉新清单重建
      await dom.click(mounted.container.querySelector('[data-testid="bump-version"]'));
      await dom.flush(550);
      expect(manifestRound).toBeGreaterThanOrEqual(3);
      const updatedVideo = mounted.container.querySelector('div[aria-label="整集 CPU 预览"] video');
      expect(updatedVideo?.getAttribute("src")).toContain("tk-3/stream");
    } finally {
      await mounted.unmount();
    }
  });

  test("FinalArea：读取清单、选 1080p 与 clean+packaged 导出、处理 91001 过期清单、渲染 exports[] 预览/下载", async () => {
    const s1 = makeSegment("s1", 0);
    const s2 = makeSegment("s2", 1, { chosenTaskId: "", takes: [], status: "idle" });
    useDramaSegmentsStore.getState().setList("d-prev", 1, [s1, s2]);

    const drama = makeDrama();
    useDramaJobsStore.getState().applyJob("d-prev", {
      id: "job-prev-1",
      step: "compose",
      episodes: [1],
      status: "succeeded",
      progress: 100,
      credits: 0,
      errorCode: 0,
      errorMessage: "",
      createdAt: "2026-11-15T00:00:00Z",
      finishedAt: "2026-11-15T00:01:00Z",
      lane: "user",
      batchFrom: null,
      batchTo: null,
      result: {
        asset: {
          id: "final-1",
          url: "https://example.test/final-default.mp4",
          mimeType: "video/mp4",
          sizeBytes: 4096,
          width: 1280,
          height: 720,
          durationMs: 10000,
          createdAt: "2026-11-15T00:00:00Z",
        },
        skippedSeqs: [1],
        exports: [
          {
            sha256: "sha-clean-1080",
            variant: "clean",
            size: "1080p",
            width: 1920,
            height: 1080,
            manifestHash: "hash-ep-1",
            asset: {
              id: "exp-clean",
              url: "https://example.test/clean-1080.mp4",
              mimeType: "video/mp4",
              sizeBytes: 5000,
              width: 1920,
              height: 1080,
              durationMs: 10000,
              createdAt: "2026-11-15T00:01:00Z",
            },
          },
          {
            sha256: "sha-pkg-1080",
            variant: "packaged",
            size: "1080p",
            width: 1920,
            height: 1080,
            manifestHash: "hash-ep-1",
            asset: {
              id: "exp-pkg",
              url: "https://example.test/pkg-1080.mp4",
              mimeType: "video/mp4",
              sizeBytes: 6000,
              width: 1920,
              height: 1080,
              durationMs: 10000,
              createdAt: "2026-11-15T00:01:00Z",
            },
          },
        ],
      },
    });

    let manifestReads = 0;
    const jobPosts: Array<Record<string, unknown>> = [];
    let rejectFirstComposeWith91001 = true;
    const fetchUrls: string[] = [];

    spies.push(
      spyOn(globalThis, "fetch").mockImplementation(async (input) => {
        fetchUrls.push(String(input));
        return new Response(new Blob(["mp4"], { type: "video/mp4" }), { status: 200 });
      }),
      spyOn(URL, "createObjectURL").mockReturnValue("blob:mock-download"),
      spyOn(URL, "revokeObjectURL").mockImplementation(() => {}),
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-prev/episodes/1/manifest") {
          manifestReads += 1;
          return {
            manifest: { v: 1, fps: 24, total_frames: 120, clips: [] },
            manifest_hash: `hash-ep-${manifestReads}`,
            segment_version: manifestReads,
            skipped_seqs: [1],
            diagnostics: ["第 2 镜未采用视频，已跳过"],
          } as never;
        }
        if (url === "/dramas/d-prev/episodes/1/segments") {
          const toBackend = (seg: Segment) => ({
            id: seg.id,
            episode_no: seg.episodeNo,
            seq: seg.seq,
            seconds: seg.seconds,
            prompt: seg.prompt,
            takes: seg.takes.map((t) => ({
              task_id: t.taskId,
              status: t.status,
              asset: t.asset
                ? {
                    id: t.asset.id,
                    url: t.asset.url,
                    mime_type: t.asset.mimeType,
                    size_bytes: t.asset.sizeBytes,
                    width: t.asset.width,
                    height: t.asset.height,
                    duration_ms: t.asset.durationMs,
                    created_at: t.asset.createdAt,
                  }
                : null,
              error_message: t.errorMessage,
              created_at: t.createdAt,
            })),
            chosen_task_id: seg.chosenTaskId,
            source_frame_count: seg.sourceFrameCount ?? 120,
            has_audio: seg.hasAudio ?? true,
            status: seg.status,
            revision: seg.revision,
            updated_at: seg.updatedAt,
          });
          return { items: [toBackend(s1), toBackend(s2)] } as never;
        }
        return {} as never;
      }),
      spyOn(service, "post").mockImplementation(async (url: string, body?: unknown) => {
        if (url === "/dramas/d-prev/jobs") {
          jobPosts.push(body as Record<string, unknown>);
          if (rejectFirstComposeWith91001) {
            rejectFirstComposeWith91001 = false;
            throw new ApiError("分镜清单已过期", 91001, 409);
          }
          return {
            id: "job-compose-2",
            step: "compose",
            episodes: [1],
            status: "queued",
            progress: 0,
            credits: 0,
            error_code: 0,
            error_message: "",
            created_at: "2026-11-15T00:02:00Z",
            finished_at: null,
          } as never;
        }
        return {} as never;
      }),
    );

    const workspace = {
      drama,
      jobs: useDramaJobsStore.getState().byDrama["d-prev"] ?? [],
      save: "saved",
      refresh: async () => {},
      patch: async () => 1,
      applyEpisode: () => {},
      reportSave: () => {},
    } as unknown as DramaWorkspace;

    const mounted = await dom.mount(
      <DramaWorkspaceContext.Provider value={workspace}>
        <FinalArea ep={1} onGo={() => {}} />
      </DramaWorkspaceContext.Provider>,
    );

    try {
      await dom.flush();
      expect(mounted.container.textContent).toContain("第 2 镜未采用视频，已跳过");
      expect(mounted.container.textContent).toContain("成片包装版");
      expect(mounted.container.textContent).toContain("1920×1080");

      // 切换分辨率 1080p 与勾选 packaged 变体
      const btn1080 = Array.from(mounted.container.querySelectorAll("button")).find(
        (b) => b.textContent?.trim() === "1080p",
      );
      const pkgBtn = Array.from(mounted.container.querySelectorAll("button")).find(
        (b) => b.textContent?.trim() === "成片包装版",
      );
      await dom.click(btn1080);
      await dom.click(pkgBtn);
      await dom.flush();

      // 首次提交遇 91001 -> 刷新 manifest 并提示再次导出
      const composeBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("重新合成"),
      );
      expect(composeBtn).toBeDefined();
      await dom.click(composeBtn);
      await dom.flush();

      expect(jobPosts.length).toBe(1);
      const firstCompose = jobPosts[0]?.compose as
        | { size?: string; variants?: string[]; manifest_hash?: string }
        | undefined;
      expect(firstCompose?.size).toBe("1080p");
      expect(firstCompose?.variants).toEqual(["clean", "packaged"]);
      expect(typeof jobPosts[0]?.compose_request_key).toBe("string");
      expect(String(jobPosts[0]?.compose_request_key).startsWith("approval:")).toBe(false);
      expect(mounted.container.textContent).toContain("清单版本已更新（91001）");

      // 使用刷新后的 manifestHash 再点一次提交，成功并生成新的 composeRequestKey
      await dom.click(composeBtn);
      await dom.flush();
      expect(jobPosts.length).toBe(2);
      const secondCompose = jobPosts[1]?.compose as { manifest_hash?: string } | undefined;
      expect(secondCompose?.manifest_hash).not.toBe(firstCompose?.manifest_hash);
      expect(jobPosts[1]?.compose_request_key).not.toBe(jobPosts[0]?.compose_request_key);

      // exports[] 切换预览与点击下载
      const exportSection = mounted.container.querySelector('div[aria-label="本次导出结果"]');
      expect(exportSection).not.toBeNull();
      const previewBtns = Array.from(exportSection!.querySelectorAll("button")).filter((b) =>
        b.textContent?.includes("预览"),
      );
      await dom.click(previewBtns[1]);
      await dom.flush();
      expect(mounted.container.textContent).toContain("切回成片");

      const dlBtns = Array.from(exportSection!.querySelectorAll("button")).filter((b) =>
        b.textContent?.includes("下载"),
      );
      await dom.click(dlBtns[1]);
      await dom.flush();
      expect(fetchUrls).toContain("https://example.test/pkg-1080.mp4");
    } finally {
      await mounted.unmount();
    }
  });

  test("CPU 预览流 <video onError> 自动续签 stream_url 且不重建 preview ticket", async () => {
    const s1 = makeSegment("s1", 0);
    let createCalls = 0;
    let pollCalls = 0;

    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-prev/episodes/1/manifest") {
          return {
            manifest: { v: 1, fps: 24, total_frames: 120, clips: [] },
            manifest_hash: "hash-renew-1",
            segment_version: 3,
            skipped_seqs: [],
            diagnostics: [],
          } as never;
        }
        if (url === "/dramas/d-prev/episodes/1/preview/tk-renew") {
          pollCalls += 1;
          return {
            ticket: "tk-renew",
            status: "ready",
            stream_url:
              "/api/v1/dramas/d-prev/episodes/1/preview/tk-renew/stream?token=sig-renewed",
            expires_at: 1999999999,
          } as never;
        }
        return {} as never;
      }),
      spyOn(service, "post").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-prev/episodes/1/preview") {
          createCalls += 1;
          return {
            ticket: "tk-renew",
            status: "ready",
            stream_url:
              "/api/v1/dramas/d-prev/episodes/1/preview/tk-renew/stream?token=sig-initial",
            expires_at: 1999999999,
          } as never;
        }
        return {} as never;
      }),
    );

    const mounted = await dom.mount(
      <StudioPreviewHarness
        initialDrama={makeDrama()}
        initialSegments={[s1]}
        onPatch={async () => s1}
      />,
    );
    try {
      const cpuBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("CPU 预览"),
      );
      await dom.click(cpuBtn);
      await dom.flush(50);

      const video = mounted.container.querySelector(
        'div[aria-label="整集 CPU 预览"] video',
      ) as HTMLVideoElement | null;
      expect(video?.getAttribute("src")).toContain("token=sig-initial");
      expect(createCalls).toBe(1);

      await dom.act(async () => {
        video?.dispatchEvent(new dom.win.Event("error"));
      });
      await dom.flush(50);

      expect(createCalls).toBe(1);
      expect(pollCalls).toBeGreaterThanOrEqual(1);
      const renewedVideo = mounted.container.querySelector(
        'div[aria-label="整集 CPU 预览"] video',
      ) as HTMLVideoElement | null;
      expect(renewedVideo?.getAttribute("src")).toContain("token=sig-renewed");
    } finally {
      await mounted.unmount();
    }
  });

  test("CPU 预览流 <video onError> 同秒续签返回相同 URL、重复 error 与瞬态 GET 失败时均保留原 ticket 不重发 POST", async () => {
    const s1 = makeSegment("s1", 0);
    let createCalls = 0;
    let pollCalls = 0;
    let mode: "same" | "transient_then_new" | "always_same" = "same";

    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-prev/episodes/1/manifest") {
          return {
            manifest: { v: 1, fps: 24, total_frames: 120, clips: [] },
            manifest_hash: "hash-same-url",
            segment_version: 3,
            skipped_seqs: [],
            diagnostics: [],
          } as never;
        }
        if (url === "/dramas/d-prev/episodes/1/preview/tk-same") {
          pollCalls += 1;
          if (mode === "same" || mode === "always_same") {
            return {
              ticket: "tk-same",
              status: "ready",
              stream_url: "/api/v1/dramas/d-prev/episodes/1/preview/tk-same/stream?token=same-sec",
              expires_at: 1999999999,
            } as never;
          }
          if (pollCalls === 2) {
            throw new ApiError("网络异常", "NETWORK_ERROR", 0);
          }
          return {
            ticket: "tk-same",
            status: "ready",
            stream_url:
              "/api/v1/dramas/d-prev/episodes/1/preview/tk-same/stream?token=recovered-sec",
            expires_at: 1999999999,
          } as never;
        }
        return {} as never;
      }),
      spyOn(service, "post").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-prev/episodes/1/preview") {
          createCalls += 1;
          return {
            ticket: "tk-same",
            status: "ready",
            stream_url: "/api/v1/dramas/d-prev/episodes/1/preview/tk-same/stream?token=same-sec",
            expires_at: 1999999999,
          } as never;
        }
        return {} as never;
      }),
    );

    const mounted = await dom.mount(
      <StudioPreviewHarness
        initialDrama={makeDrama()}
        initialSegments={[s1]}
        onPatch={async () => s1}
      />,
    );
    try {
      const cpuBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("CPU 预览"),
      );
      await dom.click(cpuBtn);
      await dom.flush(50);

      const initialVideo = mounted.container.querySelector(
        'div[aria-label="整集 CPU 预览"] video',
      ) as HTMLVideoElement | null;
      expect(initialVideo?.getAttribute("src")).toContain("token=same-sec");
      expect(createCalls).toBe(1);

      // 连续触发两次 error（模拟重复事件），且 GET 返回同秒相同 stream_url -> 仅查 1 次 GET 并重载同一地址，不重建 POST
      await dom.act(async () => {
        initialVideo?.dispatchEvent(new dom.win.Event("error"));
        initialVideo?.dispatchEvent(new dom.win.Event("error"));
      });
      await dom.flush(50);

      expect(createCalls).toBe(1);
      expect(pollCalls).toBe(1);
      const reloadedVideo = mounted.container.querySelector(
        'div[aria-label="整集 CPU 预览"] video',
      ) as HTMLVideoElement | null;
      expect(reloadedVideo).not.toBeNull();
      expect(reloadedVideo).not.toBe(initialVideo);
      expect(reloadedVideo?.getAttribute("src")).toContain("token=same-sec");

      // 下一次 error 时 GET 先遭遇瞬态断网后恢复 -> 有界退避重试 GET 并更新新 URL，仍不重发 POST
      mode = "transient_then_new";
      await dom.act(async () => {
        reloadedVideo?.dispatchEvent(new dom.win.Event("error"));
      });
      await dom.flush(550);

      expect(createCalls).toBe(1);
      expect(pollCalls).toBe(3);
      const recoveredVideo = mounted.container.querySelector(
        'div[aria-label="整集 CPU 预览"] video',
      ) as HTMLVideoElement | null;
      expect(recoveredVideo?.getAttribute("src")).toContain("token=recovered-sec");

      // 仅 loadedmetadata 不清零计数；真实稳定播放推进 (>= 1.5s) 后重置连续失败计数，随后连续 4 次 error 达上限中止
      let resetNowMs = 1_000;
      const resetNowSpy = spyOn(performance, "now").mockImplementation(() => resetNowMs);
      try {
        await dom.act(async () => {
          Object.defineProperty(recoveredVideo!, "paused", { configurable: true, value: false });
          Object.defineProperty(recoveredVideo!, "currentTime", {
            configurable: true,
            writable: true,
            value: 0,
          });
          recoveredVideo?.dispatchEvent(new dom.win.Event("loadedmetadata"));
          recoveredVideo?.dispatchEvent(new dom.win.Event("playing"));
          resetNowMs = 1_800;
          recoveredVideo!.currentTime = 0.8;
          recoveredVideo?.dispatchEvent(new dom.win.Event("timeupdate"));
          resetNowMs = 2_600;
          recoveredVideo!.currentTime = 1.6;
          recoveredVideo?.dispatchEvent(new dom.win.Event("timeupdate"));
        });
      } finally {
        resetNowSpy.mockRestore();
      }
      const pollsAfterReset = pollCalls;
      mode = "always_same";
      for (const waitMs of [50, 260, 460, 50]) {
        const curVideo = mounted.container.querySelector(
          'div[aria-label="整集 CPU 预览"] video',
        ) as HTMLVideoElement | null;
        await dom.act(async () => {
          curVideo?.dispatchEvent(new dom.win.Event("error"));
        });
        await dom.flush(waitMs);
      }

      expect(createCalls).toBe(1);
      expect(pollCalls).toBe(pollsAfterReset + 3);
      expect(mounted.container.textContent).toContain("预览流加载失败，请重试");
    } finally {
      await mounted.unmount();
    }
  });

  test("CPU 预览流每次重载都收到 loadedmetadata/canplay 后继续 error，仍累计消耗预算并在上限处中止", async () => {
    const s1 = makeSegment("s1", 0);
    let createCalls = 0;
    let pollCalls = 0;
    const validExpiresAt = Math.floor(Date.now() / 1000) + 600;

    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-prev/episodes/1/manifest") {
          return {
            manifest: { v: 1, fps: 24, total_frames: 120, clips: [] },
            manifest_hash: "hash-m1-budget",
            segment_version: 3,
            skipped_seqs: [],
            diagnostics: [],
          } as never;
        }
        if (url === "/dramas/d-prev/episodes/1/preview/tk-m1") {
          pollCalls += 1;
          return {
            ticket: "tk-m1",
            status: "ready",
            stream_url: `/api/v1/dramas/d-prev/episodes/1/preview/tk-m1/stream?token=v${pollCalls}`,
            expires_at: validExpiresAt,
          } as never;
        }
        return {} as never;
      }),
      spyOn(service, "post").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-prev/episodes/1/preview") {
          createCalls += 1;
          return {
            ticket: "tk-m1",
            status: "ready",
            stream_url: "/api/v1/dramas/d-prev/episodes/1/preview/tk-m1/stream?token=v0",
            expires_at: validExpiresAt,
          } as never;
        }
        return {} as never;
      }),
    );

    const mounted = await dom.mount(
      <StudioPreviewHarness
        initialDrama={makeDrama()}
        initialSegments={[s1]}
        onPatch={async () => s1}
      />,
    );
    try {
      const cpuBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("CPU 预览"),
      );
      await dom.click(cpuBtn);
      await dom.flush(50);

      for (let attempt = 0; attempt < 6; attempt += 1) {
        const curVideo = mounted.container.querySelector(
          'div[aria-label="整集 CPU 预览"] video',
        ) as HTMLVideoElement | null;
        if (curVideo) {
          await dom.act(async () => {
            curVideo.dispatchEvent(new dom.win.Event("loadedmetadata"));
            curVideo.dispatchEvent(new dom.win.Event("canplay"));
            curVideo.dispatchEvent(new dom.win.Event("error"));
          });
        }
        await dom.flush(500);
      }

      expect(createCalls).toBe(1);
      expect(pollCalls).toBe(3);
      expect(mounted.container.textContent).toContain("预览流加载失败，请重试");
      expect(mounted.container.querySelector('div[aria-label="整集 CPU 预览"] video')).toBeNull();
    } finally {
      await mounted.unmount();
    }
  });

  test("CPU 预览流在 playbackRate=0 停滞、仅 seek 快进与等待/暂停打断时不重置错误预算", async () => {
    const s1 = makeSegment("s1", 0);
    let createCalls = 0;
    let pollCalls = 0;
    const validExpiresAt = Math.floor(Date.now() / 1000) + 600;

    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-prev/episodes/1/manifest") {
          return {
            manifest: { v: 1, fps: 24, total_frames: 120, clips: [] },
            manifest_hash: "hash-m1-adv",
            segment_version: 3,
            skipped_seqs: [],
            diagnostics: [],
          } as never;
        }
        if (url === "/dramas/d-prev/episodes/1/preview/tk-m1-adv") {
          pollCalls += 1;
          return {
            ticket: "tk-m1-adv",
            status: "ready",
            stream_url: `/api/v1/dramas/d-prev/episodes/1/preview/tk-m1-adv/stream?token=v${pollCalls}`,
            expires_at: validExpiresAt,
          } as never;
        }
        return {} as never;
      }),
      spyOn(service, "post").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-prev/episodes/1/preview") {
          createCalls += 1;
          return {
            ticket: "tk-m1-adv",
            status: "ready",
            stream_url: "/api/v1/dramas/d-prev/episodes/1/preview/tk-m1-adv/stream?token=v0",
            expires_at: validExpiresAt,
          } as never;
        }
        return {} as never;
      }),
    );

    const mounted = await dom.mount(
      <StudioPreviewHarness
        initialDrama={makeDrama()}
        initialSegments={[s1]}
        onPatch={async () => s1}
      />,
    );
    try {
      const cpuBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("CPU 预览"),
      );
      await dom.click(cpuBtn);
      await dom.flush(50);

      // 消耗 2 次错误预算（剩余最后 1 次重试机会）
      for (const waitMs of [260, 460]) {
        const curVideo = mounted.container.querySelector(
          'div[aria-label="整集 CPU 预览"] video',
        ) as HTMLVideoElement | null;
        await dom.act(async () => {
          curVideo?.dispatchEvent(new dom.win.Event("error"));
        });
        await dom.flush(waitMs);
      }
      expect(pollCalls).toBe(2);

      const video = mounted.container.querySelector(
        'div[aria-label="整集 CPU 预览"] video',
      ) as HTMLVideoElement;
      expect(video).toBeTruthy();

      let currentTimeVal = 0;
      let pausedVal = false;
      let seekingVal = false;
      const endedVal = false;
      let playbackRateVal = 0;
      Object.defineProperties(video, {
        currentTime: {
          configurable: true,
          get: () => currentTimeVal,
          set: (val: number) => {
            currentTimeVal = val;
          },
        },
        paused: { configurable: true, get: () => pausedVal },
        seeking: { configurable: true, get: () => seekingVal },
        ended: { configurable: true, get: () => endedVal },
        playbackRate: { configurable: true, get: () => playbackRateVal },
      });

      // 1) playbackRate=0 且停留在 0s 超过 2.6s，不得清零错误预算
      await dom.act(async () => {
        video.dispatchEvent(new dom.win.Event("loadedmetadata"));
        video.dispatchEvent(new dom.win.Event("playing"));
        video.dispatchEvent(new dom.win.Event("ratechange"));
        video.dispatchEvent(new dom.win.Event("timeupdate"));
      });
      await dom.flush(2650);

      // 2) 仅通过 seek 前进 +5s，不得清零错误预算
      await dom.act(async () => {
        playbackRateVal = 1;
        video.dispatchEvent(new dom.win.Event("ratechange"));
        seekingVal = true;
        video.dispatchEvent(new dom.win.Event("seeking"));
        currentTimeVal = 5;
        video.dispatchEvent(new dom.win.Event("timeupdate"));
        seekingVal = false;
        video.dispatchEvent(new dom.win.Event("seeked"));
      });

      // 3) 连续短段被 waiting / pause 打断（每段 < 1.5s），不得累加误判为稳定播放
      await dom.act(async () => {
        video.dispatchEvent(new dom.win.Event("playing"));
        currentTimeVal = 5.9;
        video.dispatchEvent(new dom.win.Event("timeupdate"));
        video.dispatchEvent(new dom.win.Event("waiting"));
        currentTimeVal = 6.7;
        video.dispatchEvent(new dom.win.Event("timeupdate"));
        pausedVal = true;
        video.dispatchEvent(new dom.win.Event("pause"));
        pausedVal = false;
        video.dispatchEvent(new dom.win.Event("playing"));
        currentTimeVal = 7.5;
        video.dispatchEvent(new dom.win.Event("timeupdate"));
      });

      // 第 3 次 error 用尽最后 1 次重试名额，第 4 次 error 立即中止
      const videoBeforeThirdError = mounted.container.querySelector(
        'div[aria-label="整集 CPU 预览"] video',
      ) as HTMLVideoElement | null;
      await dom.act(async () => {
        videoBeforeThirdError?.dispatchEvent(new dom.win.Event("error"));
      });
      await dom.flush(650);
      expect(pollCalls).toBe(3);

      const videoBeforeFourthError = mounted.container.querySelector(
        'div[aria-label="整集 CPU 预览"] video',
      ) as HTMLVideoElement | null;
      await dom.act(async () => {
        videoBeforeFourthError?.dispatchEvent(new dom.win.Event("error"));
      });
      await dom.flush(50);

      expect(createCalls).toBe(1);
      expect(pollCalls).toBe(3);
      expect(mounted.container.textContent).toContain("预览流加载失败，请重试");
      expect(mounted.container.querySelector('div[aria-label="整集 CPU 预览"] video')).toBeNull();
    } finally {
      await mounted.unmount();
    }
  }, 15000);

  test("CPU 预览流真实连续推进播放超过稳定窗口后重置错误预算（定时器路径与 timeupdate 路径）", async () => {
    const s1 = makeSegment("s1", 0);
    let createCalls = 0;
    let pollCalls = 0;
    const validExpiresAt = Math.floor(Date.now() / 1000) + 600;

    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-prev/episodes/1/manifest") {
          return {
            manifest: { v: 1, fps: 24, total_frames: 120, clips: [] },
            manifest_hash: "hash-m1-stable",
            segment_version: 3,
            skipped_seqs: [],
            diagnostics: [],
          } as never;
        }
        if (url === "/dramas/d-prev/episodes/1/preview/tk-m1-stable") {
          pollCalls += 1;
          return {
            ticket: "tk-m1-stable",
            status: "ready",
            stream_url: `/api/v1/dramas/d-prev/episodes/1/preview/tk-m1-stable/stream?token=v${pollCalls}`,
            expires_at: validExpiresAt,
          } as never;
        }
        return {} as never;
      }),
      spyOn(service, "post").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-prev/episodes/1/preview") {
          createCalls += 1;
          return {
            ticket: "tk-m1-stable",
            status: "ready",
            stream_url: "/api/v1/dramas/d-prev/episodes/1/preview/tk-m1-stable/stream?token=v0",
            expires_at: validExpiresAt,
          } as never;
        }
        return {} as never;
      }),
    );

    const mounted = await dom.mount(
      <StudioPreviewHarness
        initialDrama={makeDrama()}
        initialSegments={[s1]}
        onPatch={async () => s1}
      />,
    );
    try {
      const cpuBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("CPU 预览"),
      );
      await dom.click(cpuBtn);
      await dom.flush(50);

      // 先累计 2 次失败
      for (const waitMs of [260, 460]) {
        const curVideo = mounted.container.querySelector(
          'div[aria-label="整集 CPU 预览"] video',
        ) as HTMLVideoElement | null;
        await dom.act(async () => {
          curVideo?.dispatchEvent(new dom.win.Event("error"));
        });
        await dom.flush(waitMs);
      }
      expect(pollCalls).toBe(2);

      // 真实播放推进（无 seek、playbackRate=1、未暂停），经过 400ms 周期采样持续推进 >= 1.5s 后应清零错误预算
      let timerNowMs = 1_000;
      const timerNowSpy = spyOn(performance, "now").mockImplementation(() => timerNowMs);
      spies.push(timerNowSpy);
      const video = mounted.container.querySelector(
        'div[aria-label="整集 CPU 预览"] video',
      ) as HTMLVideoElement;
      let currentTimeVal = 1.0;
      Object.defineProperties(video, {
        currentTime: {
          configurable: true,
          get: () => currentTimeVal,
          set: (val: number) => {
            currentTimeVal = val;
          },
        },
        paused: { configurable: true, value: false },
        seeking: { configurable: true, value: false },
        ended: { configurable: true, value: false },
        playbackRate: { configurable: true, value: 1 },
      });
      await dom.act(async () => {
        video.dispatchEvent(new dom.win.Event("loadedmetadata"));
        video.dispatchEvent(new dom.win.Event("playing"));
      });
      // 仅通过真实播放头逐帧连续推进 >= 1.5s（未触发 timeupdate），由 400ms 周期采样清零错误预算
      for (let i = 0; i < 4; i += 1) {
        timerNowMs += 400;
        currentTimeVal += 0.4;
        await dom.flush(450);
      }
      timerNowSpy.mockRestore();

      // 清零后可再次连续重试 3 次，第 4 次才触发上限中止
      for (const waitMs of [260, 460, 650, 50]) {
        const curVideo = mounted.container.querySelector(
          'div[aria-label="整集 CPU 预览"] video',
        ) as HTMLVideoElement | null;
        await dom.act(async () => {
          curVideo?.dispatchEvent(new dom.win.Event("error"));
        });
        await dom.flush(waitMs);
      }

      expect(createCalls).toBe(1);
      expect(pollCalls).toBe(5);
      expect(mounted.container.textContent).toContain("预览流加载失败，请重试");
    } finally {
      await mounted.unmount();
    }
  }, 15000);

  test("CPU 预览轮询遇瞬态 GET 失败保留 ticket 退避重试；耗尽后点「重试预览」继续 GET 不重复 POST", async () => {
    const s1 = makeSegment("s1", 0);
    let createCalls = 0;
    let pollCalls = 0;
    let exhaustTicket = false;
    let recoverExhausted = false;

    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-prev/episodes/1/manifest") {
          return {
            manifest: { v: 1, fps: 24, total_frames: 120, clips: [] },
            manifest_hash: "hash-poll-retry",
            segment_version: 3,
            skipped_seqs: [],
            diagnostics: [],
          } as never;
        }
        if (url === "/dramas/d-prev/episodes/1/preview/tk-poll-retry") {
          pollCalls += 1;
          if (pollCalls === 1) throw new ApiError("网络异常", "NETWORK_ERROR", 0);
          if (pollCalls === 2) throw new ApiError("网关错误", "HTTP_502", 502);
          return {
            ticket: "tk-poll-retry",
            status: "ready",
            stream_url:
              "/api/v1/dramas/d-prev/episodes/1/preview/tk-poll-retry/stream?token=poll-ok",
            expires_at: 1999999999,
          } as never;
        }
        if (url === "/dramas/d-prev/episodes/1/preview/tk-exhaust") {
          pollCalls += 1;
          if (!recoverExhausted) {
            throw new ApiError("网络超时", "TIMEOUT", 0);
          }
          return {
            ticket: "tk-exhaust",
            status: "ready",
            stream_url:
              "/api/v1/dramas/d-prev/episodes/1/preview/tk-exhaust/stream?token=resumed-ok",
            expires_at: 1999999999,
          } as never;
        }
        return {} as never;
      }),
      spyOn(service, "post").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-prev/episodes/1/preview") {
          createCalls += 1;
          return {
            ticket: exhaustTicket ? "tk-exhaust" : "tk-poll-retry",
            status: "pending",
            expires_at: 1999999999,
          } as never;
        }
        return {} as never;
      }),
    );

    const mounted = await dom.mount(
      <StudioPreviewHarness
        initialDrama={makeDrama()}
        initialSegments={[s1]}
        onPatch={async () => s1}
      />,
    );
    try {
      const cpuBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("CPU 预览"),
      );
      await dom.click(cpuBtn);
      // 200ms + 400ms 退避重试后在第 3 次 GET 就绪
      await dom.flush(750);

      expect(createCalls).toBe(1);
      expect(pollCalls).toBe(3);
      const video = mounted.container.querySelector('div[aria-label="整集 CPU 预览"] video');
      expect(video?.getAttribute("src")).toContain("tk-poll-retry/stream?token=poll-ok");

      // 显式点击「刷新预览」创建 tk-exhaust，连续瞬态失败耗尽退避重试后仍保留 tk-exhaust
      exhaustTicket = true;
      const refreshBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("刷新预览"),
      );
      await dom.click(refreshBtn);
      await dom.flush(3300);

      expect(createCalls).toBe(2);
      const retryBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("重试预览"),
      );
      expect(retryBtn).toBeDefined();

      // 恢复网络后点「重试预览」直接用保留的 tk-exhaust 续查 GET，不重发 POST
      recoverExhausted = true;
      await dom.click(retryBtn);
      await dom.flush(50);

      expect(createCalls).toBe(2);
      const resumedVideo = mounted.container.querySelector('div[aria-label="整集 CPU 预览"] video');
      expect(resumedVideo?.getAttribute("src")).toContain("tk-exhaust/stream?token=resumed-ok");
    } finally {
      await mounted.unmount();
    }
  });

  test("CPU 预览在 67001/404 时确认票据失效重建，failed 按 expiresAt 冷却且切换版本即时解除，卸载后取消定时器", async () => {
    const s1 = makeSegment("s1", 0);
    let createCalls = 0;
    let pollCalls = 0;
    let issuedSeq = 0;
    let scenario: "expired_on_error" | "failed_status" | "pending_unmount" = "expired_on_error";

    type CachedMockTicket = {
      ticket: string;
      status: "pending" | "ready" | "failed";
      stream_url?: string;
      expires_at: number;
    };
    const ticketCache = new Map<string, CachedMockTicket>();
    const byTicketId = new Map<string, CachedMockTicket>();

    const findValidTicket = (cacheKey: string): CachedMockTicket | null => {
      const existing = ticketCache.get(cacheKey);
      if (!existing) return null;
      if (Date.now() >= existing.expires_at * 1000) {
        ticketCache.delete(cacheKey);
        byTicketId.delete(existing.ticket);
        return null;
      }
      return existing;
    };

    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-prev/episodes/1/manifest") {
          return {
            manifest: { v: 1, fps: 24, total_frames: 120, clips: [] },
            manifest_hash: "hash-lifecycle",
            segment_version: 3,
            skipped_seqs: [],
            diagnostics: [],
          } as never;
        }
        const match = url.match(/\/dramas\/d-prev\/episodes\/1\/preview\/([^/]+)$/);
        if (match) {
          const ticketId = match[1]!;
          pollCalls += 1;
          if (ticketId === "tk-1") {
            const old = byTicketId.get("tk-1");
            if (old) {
              for (const [k, v] of ticketCache.entries()) {
                if (v.ticket === "tk-1") ticketCache.delete(k);
              }
              byTicketId.delete("tk-1");
            }
            throw new ApiError("预览票据不存在或已过期", 67001, 404);
          }
          const entry = byTicketId.get(ticketId);
          if (!entry || Date.now() >= entry.expires_at * 1000) {
            throw new ApiError("预览票据不存在或已过期", 67001, 404);
          }
          if (ticketId === "tk-2" || ticketId === "tk-4") {
            entry.status = "ready";
            entry.stream_url = `/api/v1/dramas/d-prev/episodes/1/preview/${ticketId}/stream?token=rebuilt`;
          } else if (ticketId === "tk-3" && entry.status !== "failed") {
            entry.status = "failed";
            entry.expires_at = Math.ceil(Date.now() / 1000) + 2;
          }
          return {
            ticket: entry.ticket,
            status: entry.status,
            stream_url: entry.stream_url,
            expires_at: entry.expires_at,
          } as never;
        }
        return {} as never;
      }),
      spyOn(service, "post").mockImplementation(async (url: string, body?: unknown) => {
        if (url === "/dramas/d-prev/episodes/1/preview") {
          createCalls += 1;
          const payload = (body ?? {}) as {
            segment_version?: number;
            manifest_hash?: string;
            variant?: string;
          };
          const cacheKey = `${payload.segment_version ?? 0}:${payload.manifest_hash ?? ""}:${payload.variant ?? "clean"}`;
          const cached = findValidTicket(cacheKey);
          if (cached) {
            return {
              ticket: cached.ticket,
              status: cached.status,
              stream_url: cached.stream_url,
              expires_at: cached.expires_at,
            } as never;
          }
          issuedSeq += 1;
          const ticketId = `tk-${issuedSeq}`;
          const created: CachedMockTicket =
            scenario === "expired_on_error" && issuedSeq === 1
              ? {
                  ticket: ticketId,
                  status: "ready",
                  stream_url: `/api/v1/dramas/d-prev/episodes/1/preview/${ticketId}/stream?token=old`,
                  expires_at: Math.floor(Date.now() / 1000) + 600,
                }
              : {
                  ticket: ticketId,
                  status: "pending",
                  expires_at: Math.floor(Date.now() / 1000) + 600,
                };
          ticketCache.set(cacheKey, created);
          byTicketId.set(ticketId, created);
          return {
            ticket: created.ticket,
            status: created.status,
            stream_url: created.stream_url,
            expires_at: created.expires_at,
          } as never;
        }
        return {} as never;
      }),
    );

    const mounted = await dom.mount(
      <StudioPreviewHarness
        initialDrama={makeDrama()}
        initialSegments={[s1]}
        onPatch={async () => s1}
      />,
    );
    let unmounted = false;
    try {
      const cpuBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("CPU 预览"),
      );
      await dom.click(cpuBtn);
      await dom.flush(50);

      const firstVideo = mounted.container.querySelector(
        'div[aria-label="整集 CPU 预览"] video',
      ) as HTMLVideoElement | null;
      expect(firstVideo?.getAttribute("src")).toContain("tk-1/stream");

      // 67001 / 404 确认票据失效 -> 重建 tk-2 并轮询就绪
      await dom.act(async () => {
        firstVideo?.dispatchEvent(new dom.win.Event("error"));
      });
      await dom.flush(80);
      expect(createCalls).toBe(2);
      const rebuiltVideo = mounted.container.querySelector('div[aria-label="整集 CPU 预览"] video');
      expect(rebuiltVideo?.getAttribute("src")).toContain("tk-2/stream?token=rebuilt");

      // 清空缓存以模拟版本升级后的新失败票据 tk-3，验证 failed 不自动循环且按 expiresAt 冷却
      ticketCache.clear();
      byTicketId.clear();
      scenario = "failed_status";
      await dom.click(mounted.container.querySelector('[data-testid="bump-version"]'));
      await dom.flush(100);
      expect(createCalls).toBe(3);
      expect(mounted.container.textContent).toContain("CPU 预览生成失败，请检查片段视频并重试");
      expect(mounted.container.textContent).toContain("同版本失败票据缓存中");

      const cooldownRetryBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("重试预览"),
      );
      expect(cooldownRetryBtn?.disabled).toBe(true);
      expect(cooldownRetryBtn?.textContent).toMatch(/重试预览（\d+s）/);

      // 冷却期内点「重试预览」或清单未变的「刷新预览」都不重发 POST 命中失败缓存
      const refreshBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("刷新预览"),
      );
      await dom.click(cooldownRetryBtn);
      await dom.click(refreshBtn);
      await dom.flush(40);
      expect(createCalls).toBe(3);

      // 切换成片包装版（variant 变化）立即跳过 clean 的冷却并创建 tk-4
      const pkgVariantBtn = Array.from(mounted.container.querySelectorAll("button")).find(
        (b) => b.textContent?.trim() === "成片包装版",
      );
      expect(pkgVariantBtn).toBeDefined();
      await dom.click(pkgVariantBtn);
      await dom.flush(80);
      expect(createCalls).toBe(4);
      const pkgVideo = mounted.container.querySelector('div[aria-label="整集 CPU 预览"] video');
      expect(pkgVideo?.getAttribute("src")).toContain("tk-4/stream?token=rebuilt");

      // 切回纯净版仍在 tk-3 有效期内：后端复用 failed 票据 tk-3，前端再次进入冷却而不生成新票据
      const cleanVariantBtn = Array.from(mounted.container.querySelectorAll("button")).find(
        (b) => b.textContent?.trim() === "纯净版",
      );
      expect(cleanVariantBtn).toBeDefined();
      await dom.click(cleanVariantBtn);
      await dom.flush(60);
      expect(createCalls).toBe(5);
      expect(issuedSeq).toBe(4);
      expect(mounted.container.textContent).toContain("同版本失败票据缓存中");

      // 等待冷却到期 -> 解锁「重试预览」，点重试后创建全新票据 tk-5 并进入 pending 轮询，再卸载验证定时器清理
      scenario = "pending_unmount";
      await dom.flush(3100);
      const unlockedRetryBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("重试预览"),
      );
      expect(unlockedRetryBtn?.disabled).toBe(false);
      expect(unlockedRetryBtn?.textContent).toBe("重试预览");

      await dom.click(unlockedRetryBtn);
      await dom.flush(50);
      expect(createCalls).toBe(6);
      expect(issuedSeq).toBe(5);

      const pollsAtUnmount = pollCalls;
      const createsAtUnmount = createCalls;
      await mounted.unmount();
      unmounted = true;
      await dom.flush(900);

      expect(pollCalls).toBe(pollsAtUnmount);
      expect(createCalls).toBe(createsAtUnmount);
    } finally {
      if (!unmounted) await mounted.unmount();
    }
  });

  test("FinalArea 对 editSchema=0 或不支持比例回退旧全片合成，且支持 9:16 竖屏清单合成", async () => {
    const s1 = makeSegment("s1", 0);
    useDramaSegmentsStore.getState().setList("d-prev", 1, [s1]);

    const toBackend = (seg: Segment) => ({
      id: seg.id,
      episode_no: seg.episodeNo,
      seq: seg.seq,
      seconds: seg.seconds,
      prompt: seg.prompt,
      takes: seg.takes.map((t) => ({
        task_id: t.taskId,
        status: t.status,
        asset: t.asset
          ? {
              id: t.asset.id,
              url: t.asset.url,
              mime_type: t.asset.mimeType,
              size_bytes: t.asset.sizeBytes,
              width: t.asset.width,
              height: t.asset.height,
              duration_ms: t.asset.durationMs,
              created_at: t.asset.createdAt,
            }
          : null,
        error_message: t.errorMessage,
        created_at: t.createdAt,
      })),
      chosen_task_id: seg.chosenTaskId,
      source_frame_count: seg.sourceFrameCount ?? 120,
      has_audio: seg.hasAudio ?? true,
      status: seg.status,
      revision: seg.revision,
      updated_at: seg.updatedAt,
    });

    for (const variant of [
      {
        label: "3:2 + editSchema=1",
        drama: makeDrama({ ratio: "3:2" }),
        expectedHint: "当前项目比例为 3:2，将按原比例整段合成。",
      },
      {
        label: "empty ratio + editSchema=1",
        drama: makeDrama({ ratio: "" }),
        expectedHint: "当前项目比例为 未设置，将按原比例整段合成。",
      },
      {
        label: "16:9 + editSchema=0",
        drama: (() => {
          const base = makeDrama({ ratio: "16:9" });
          return {
            ...base,
            episodes: [{ ...base.episodes[0]!, editSchema: 0 }],
          };
        })(),
        expectedHint: "启用 16:9 分档与版本导出",
      },
    ]) {
      let manifestFetched = false;
      const jobPosts: Array<Record<string, unknown>> = [];

      const sGet = spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url.endsWith("/manifest")) manifestFetched = true;
        if (url.endsWith("/segments")) return { items: [toBackend(s1)] } as never;
        return {} as never;
      });
      const sPost = spyOn(service, "post").mockImplementation(
        async (url: string, body?: unknown) => {
          if (url === "/dramas/d-prev/jobs") {
            jobPosts.push(body as Record<string, unknown>);
            return {
              id: `job-${variant.label}`,
              step: "compose",
              episodes: [1],
              status: "queued",
              progress: 0,
              credits: 0,
              error_code: 0,
              error_message: "",
              created_at: "2026-11-15T00:00:00Z",
              finished_at: null,
            } as never;
          }
          return {} as never;
        },
      );

      const workspace = {
        drama: variant.drama,
        jobs: [],
        save: "saved",
        refresh: async () => {},
        patch: async () => 1,
        applyEpisode: () => {},
        reportSave: () => {},
      } as unknown as DramaWorkspace;

      const mounted = await dom.mount(
        <DramaWorkspaceContext.Provider value={workspace}>
          <FinalArea ep={1} onGo={() => {}} />
        </DramaWorkspaceContext.Provider>,
      );
      try {
        await dom.flush(30);
        expect(manifestFetched).toBe(false);
        expect(mounted.container.textContent).not.toContain("成片包装版");
        expect(mounted.container.textContent).toContain(variant.expectedHint);

        const composeBtn = Array.from(mounted.container.querySelectorAll("button")).find(
          (b) => b.textContent?.includes("合成第") || b.textContent?.includes("重新合成"),
        );
        expect(composeBtn).toBeDefined();
        expect(composeBtn?.disabled).toBe(false);
        await dom.click(composeBtn);
        await dom.flush(20);

        expect(jobPosts.length).toBe(1);
        expect(jobPosts[0]?.compose).toBeUndefined();
      } finally {
        await mounted.unmount();
        sGet.mockRestore();
        sPost.mockRestore();
      }
    }

    // 9:16 + editSchema=1 现已启用精裁清单合成与竖屏尺寸提示
    let portraitManifestFetched = false;
    const portraitJobPosts: Array<Record<string, unknown>> = [];
    const sGetPortrait = spyOn(service, "get").mockImplementation(async (url: string) => {
      if (url.endsWith("/manifest")) {
        portraitManifestFetched = true;
        return {
          manifest: {
            v: 1,
            fps: 24,
            total_frames: 18,
            clips: [
              {
                segment_id: s1.id,
                seq: 0,
                master_in_f: 0,
                master_out_f: 18,
                video_duration_ms: 5000,
                has_audio: true,
              },
            ],
          },
          manifest_hash: "hash-9-16",
          segment_version: 2,
          skipped_seqs: [],
          diagnostics: [],
        } as never;
      }
      if (url.endsWith("/segments")) return { items: [toBackend(s1)] } as never;
      return {} as never;
    });
    const sPostPortrait = spyOn(service, "post").mockImplementation(
      async (url: string, body?: unknown) => {
        if (url === "/dramas/d-prev/jobs") {
          portraitJobPosts.push(body as Record<string, unknown>);
          return {
            id: "job-portrait",
            step: "compose",
            episodes: [1],
            status: "queued",
            progress: 0,
            credits: 0,
            error_code: 0,
            error_message: "",
            created_at: "2026-11-15T00:00:00Z",
            finished_at: null,
          } as never;
        }
        return {} as never;
      },
    );

    const portraitWorkspace = {
      drama: makeDrama({ ratio: "9:16" }),
      jobs: [],
      save: "saved",
      refresh: async () => {},
      patch: async () => 1,
      applyEpisode: () => {},
      reportSave: () => {},
    } as unknown as DramaWorkspace;

    const mountedPortrait = await dom.mount(
      <DramaWorkspaceContext.Provider value={portraitWorkspace}>
        <FinalArea ep={1} onGo={() => {}} />
      </DramaWorkspaceContext.Provider>,
    );
    try {
      await dom.flush(30);
      expect(portraitManifestFetched).toBe(true);
      expect(mountedPortrait.container.textContent).toContain("成片包装版");
      expect(mountedPortrait.container.textContent).toContain("9:16 · 720×1280");
      expect(mountedPortrait.container.textContent).toContain("0.75s");

      const btn1080 = Array.from(mountedPortrait.container.querySelectorAll("button")).find(
        (b) => b.textContent?.trim() === "1080p",
      );
      expect(btn1080).toBeDefined();
      await dom.click(btn1080);
      await dom.flush(10);
      expect(mountedPortrait.container.textContent).toContain("9:16 · 1080×1920");

      const composeBtn = Array.from(mountedPortrait.container.querySelectorAll("button")).find(
        (b) => b.textContent?.includes("合成第") || b.textContent?.includes("重新合成"),
      );
      expect(composeBtn).toBeDefined();
      await dom.click(composeBtn);
      await dom.flush(20);

      expect(portraitJobPosts.length).toBe(1);
      expect(portraitJobPosts[0]?.compose).toEqual({
        manifest_hash: "hash-9-16",
        segment_version: 2,
        size: "1080p",
        variants: ["clean"],
      });
    } finally {
      await mountedPortrait.unmount();
      sGetPortrait.mockRestore();
      sPostPortrait.mockRestore();
    }
  });

  test("FinalArea exports[] 共享 identical sha256 时按 variant+size+assetId 独立切换预览", async () => {
    const s1 = makeSegment("s1", 0);
    useDramaSegmentsStore.getState().setList("d-prev", 1, [s1]);
    useDramaJobsStore.getState().applyJob("d-prev", {
      id: "job-same-sha",
      step: "compose",
      episodes: [1],
      status: "succeeded",
      progress: 100,
      credits: 0,
      errorCode: 0,
      errorMessage: "",
      createdAt: "2026-11-15T00:00:00Z",
      finishedAt: "2026-11-15T00:01:00Z",
      result: {
        episodeNo: 1,
        asset: {
          id: "final-default",
          url: "https://example.test/final-default.mp4",
          mimeType: "video/mp4",
          sizeBytes: 4096,
          width: 1280,
          height: 720,
          durationMs: 10000,
          createdAt: "2026-11-15T00:00:00Z",
        },
        skippedSeqs: [],
        exports: [
          {
            sha256: "sha-identical",
            variant: "clean",
            size: "1080p",
            width: 1920,
            height: 1080,
            manifestHash: "hash-same",
            asset: {
              id: "exp-clean",
              url: "https://example.test/clean-identical.mp4",
              mimeType: "video/mp4",
              sizeBytes: 5000,
              width: 1920,
              height: 1080,
              durationMs: 10000,
              createdAt: "2026-11-15T00:01:00Z",
            },
          },
          {
            sha256: "sha-identical",
            variant: "packaged",
            size: "1080p",
            width: 1920,
            height: 1080,
            manifestHash: "hash-same",
            asset: {
              id: "exp-pkg",
              url: "https://example.test/pkg-identical.mp4",
              mimeType: "video/mp4",
              sizeBytes: 6000,
              width: 1920,
              height: 1080,
              durationMs: 10000,
              createdAt: "2026-11-15T00:01:00Z",
            },
          },
        ],
      },
    });

    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url.endsWith("/segments")) {
          return {
            items: [
              {
                id: s1.id,
                episode_no: s1.episodeNo,
                seq: s1.seq,
                seconds: s1.seconds,
                prompt: s1.prompt,
                takes: s1.takes.map((t) => ({
                  task_id: t.taskId,
                  status: t.status,
                  asset: t.asset
                    ? {
                        id: t.asset.id,
                        url: t.asset.url,
                        mime_type: t.asset.mimeType,
                        size_bytes: t.asset.sizeBytes,
                        width: t.asset.width,
                        height: t.asset.height,
                        duration_ms: t.asset.durationMs,
                        created_at: t.asset.createdAt,
                      }
                    : null,
                  error_message: t.errorMessage,
                  created_at: t.createdAt,
                })),
                chosen_task_id: s1.chosenTaskId,
                source_frame_count: s1.sourceFrameCount ?? 120,
                has_audio: s1.hasAudio ?? true,
                status: s1.status,
                revision: s1.revision,
                updated_at: s1.updatedAt,
              },
            ],
          } as never;
        }
        return {
          manifest: { v: 1, fps: 24, total_frames: 120, clips: [] },
          manifest_hash: "hash-same",
          segment_version: 3,
          skipped_seqs: [],
          diagnostics: [],
        } as never;
      }),
    );

    const workspace = {
      drama: makeDrama(),
      jobs: useDramaJobsStore.getState().byDrama["d-prev"] ?? [],
      save: "saved",
      refresh: async () => {},
      patch: async () => 1,
      applyEpisode: () => {},
      reportSave: () => {},
    } as unknown as DramaWorkspace;

    const mounted = await dom.mount(
      <DramaWorkspaceContext.Provider value={workspace}>
        <FinalArea ep={1} onGo={() => {}} />
      </DramaWorkspaceContext.Provider>,
    );
    try {
      await dom.flush(30);
      const exportSection = mounted.container.querySelector('div[aria-label="本次导出结果"]')!;
      const previewBtns = Array.from(exportSection.querySelectorAll("button")).filter((b) =>
        b.textContent?.includes("预览"),
      );
      expect(previewBtns).toHaveLength(2);

      await dom.click(previewBtns[1]);
      await dom.flush();

      const cards = Array.from(exportSection.querySelectorAll<HTMLElement>(".rounded-lg.border"));
      expect(cards[0]?.className).not.toContain("border-primary");
      expect(cards[1]?.className).toContain("border-primary");
      const playBtn = mounted.container.querySelector<HTMLButtonElement>(
        'button[aria-label="播放视频"]',
      );
      expect(playBtn).not.toBeNull();
      await dom.click(playBtn);
      await dom.flush(10);
      const playerVideo = mounted.container.querySelector("video");
      expect(playerVideo?.getAttribute("src")).toBe("https://example.test/pkg-identical.mp4");
    } finally {
      await mounted.unmount();
    }
  });

  test("M1 防绕过(a)：播放 0.8s 后卡顿 2.4s（同 currentTime timeupdate 无 waiting）再播 0.8s，及稳定性定时器跨卡顿场景均不得清零错误预算", async () => {
    const s1 = makeSegment("s1", 0);
    let pollCalls = 0;
    let nowMs = 1_000;
    const validExpiresAt = Math.floor(Date.now() / 1000) + 600;
    const nowSpy = spyOn(performance, "now").mockImplementation(() => nowMs);
    spies.push(
      nowSpy,
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-prev/episodes/1/manifest") {
          return {
            manifest: { v: 1, fps: 24, total_frames: 120, clips: [] },
            manifest_hash: "hash-m1-stall",
            segment_version: 3,
            skipped_seqs: [],
            diagnostics: [],
          } as never;
        }
        if (url === "/dramas/d-prev/episodes/1/preview/tk-m1-stall") {
          pollCalls += 1;
          return {
            ticket: "tk-m1-stall",
            status: "ready",
            stream_url: `/api/v1/dramas/d-prev/episodes/1/preview/tk-m1-stall/stream?token=v${pollCalls}`,
            expires_at: validExpiresAt,
          } as never;
        }
        return {} as never;
      }),
      spyOn(service, "post").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-prev/episodes/1/preview") {
          return {
            ticket: "tk-m1-stall",
            status: "ready",
            stream_url: "/api/v1/dramas/d-prev/episodes/1/preview/tk-m1-stall/stream?token=v0",
            expires_at: validExpiresAt,
          } as never;
        }
        return {} as never;
      }),
    );

    const mounted = await dom.mount(
      <StudioPreviewHarness
        initialDrama={makeDrama()}
        initialSegments={[s1]}
        onPatch={async () => s1}
      />,
    );
    try {
      const cpuBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("CPU 预览"),
      );
      await dom.click(cpuBtn);
      await dom.flush(50);

      // 先消耗 2 次流恢复预算（上限为 3 次，此时仅剩 1 次）
      for (const waitMs of [40, 260]) {
        const curVideo = mounted.container.querySelector(
          'div[aria-label="整集 CPU 预览"] video',
        ) as HTMLVideoElement | null;
        await dom.act(async () => {
          curVideo?.dispatchEvent(new dom.win.Event("error"));
        });
        await dom.flush(waitMs);
      }
      expect(pollCalls).toBe(2);

      const video = mounted.container.querySelector(
        'div[aria-label="整集 CPU 预览"] video',
      ) as HTMLVideoElement;
      let currentTimeVal = 0;
      const pausedVal = false;
      const seekingVal = false;
      const endedVal = false;
      const playbackRateVal = 1;
      Object.defineProperties(video, {
        currentTime: {
          configurable: true,
          get: () => currentTimeVal,
          set: (val: number) => {
            currentTimeVal = val;
          },
        },
        paused: { configurable: true, get: () => pausedVal },
        seeking: { configurable: true, get: () => seekingVal },
        ended: { configurable: true, get: () => endedVal },
        playbackRate: { configurable: true, get: () => playbackRateVal },
      });

      // Repro 1：播放 0.8s -> 停顿 2.4s（仅同 currentTime timeupdate，无 waiting 事件）-> 再播 0.8s
      await dom.act(async () => {
        nowMs = 1_000;
        currentTimeVal = 0;
        video.dispatchEvent(new dom.win.Event("loadedmetadata"));
        video.dispatchEvent(new dom.win.Event("playing"));

        nowMs = 1_800;
        currentTimeVal = 0.8;
        video.dispatchEvent(new dom.win.Event("timeupdate"));

        nowMs = 4_200;
        currentTimeVal = 0.8;
        video.dispatchEvent(new dom.win.Event("timeupdate"));

        nowMs = 5_000;
        currentTimeVal = 1.6;
        video.dispatchEvent(new dom.win.Event("timeupdate"));
      });

      // 稳定性定时器也经历停滞窗口：停留在 1.6s 经过 2.6s 挂钟后触发定时器，不得累积清零预算
      nowMs = 7_600;
      await dom.flush(2650);

      // 验证预算未清零：第 3 次误差耗尽最后 1 次额度并恢复，第 4 次立即失败退出
      for (const waitMs of [460, 120]) {
        const curVideo = mounted.container.querySelector(
          'div[aria-label="整集 CPU 预览"] video',
        ) as HTMLVideoElement | null;
        await dom.act(async () => {
          curVideo?.dispatchEvent(new dom.win.Event("error"));
        });
        await dom.flush(waitMs);
      }
      expect(pollCalls).toBe(3);
      expect(mounted.container.textContent).toContain("预览流加载失败，请重试");
      expect(mounted.container.querySelector('div[aria-label="整集 CPU 预览"] video')).toBeNull();
    } finally {
      await mounted.unmount();
    }
  });

  test("M1 防绕过(b)：同 ticket 手动重试（errorSource=poll）不得清零 streamErrorCount 预算", async () => {
    const s1 = makeSegment("s1", 0);
    let createCalls = 0;
    let pollSuccessCalls = 0;
    let failPollTransiently = false;
    const validExpiresAt = Math.floor(Date.now() / 1000) + 600;

    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-prev/episodes/1/manifest") {
          return {
            manifest: { v: 1, fps: 24, total_frames: 120, clips: [] },
            manifest_hash: "hash-m1-retry",
            segment_version: 3,
            skipped_seqs: [],
            diagnostics: [],
          } as never;
        }
        if (url === "/dramas/d-prev/episodes/1/preview/tk-m1-retry") {
          if (failPollTransiently) {
            throw new Error("transient network");
          }
          pollSuccessCalls += 1;
          return {
            ticket: "tk-m1-retry",
            status: "ready",
            stream_url: `/api/v1/dramas/d-prev/episodes/1/preview/tk-m1-retry/stream?token=v${pollSuccessCalls}`,
            expires_at: validExpiresAt,
          } as never;
        }
        return {} as never;
      }),
      spyOn(service, "post").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-prev/episodes/1/preview") {
          createCalls += 1;
          return {
            ticket: "tk-m1-retry",
            status: "ready",
            stream_url: "/api/v1/dramas/d-prev/episodes/1/preview/tk-m1-retry/stream?token=v0",
            expires_at: validExpiresAt,
          } as never;
        }
        return {} as never;
      }),
    );

    const mounted = await dom.mount(
      <StudioPreviewHarness
        initialDrama={makeDrama()}
        initialSegments={[s1]}
        onPatch={async () => s1}
      />,
    );
    try {
      const cpuBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("CPU 预览"),
      );
      await dom.click(cpuBtn);
      await dom.flush(50);

      // 第 1 次 error 正常恢复（streamErrorCount=1）
      const v0 = mounted.container.querySelector(
        'div[aria-label="整集 CPU 预览"] video',
      ) as HTMLVideoElement;
      await dom.act(async () => {
        v0.dispatchEvent(new dom.win.Event("error"));
      });
      await dom.flush(50);
      expect(pollSuccessCalls).toBe(1);

      // 第 2 次 error 在恢复 GET 时遭遇瞬态失败（streamErrorCount=2，进入 errorSource="poll"）
      failPollTransiently = true;
      const v1 = mounted.container.querySelector(
        'div[aria-label="整集 CPU 预览"] video',
      ) as HTMLVideoElement;
      await dom.act(async () => {
        v1.dispatchEvent(new dom.win.Event("error"));
      });
      await dom.flush(3600);
      expect(mounted.container.textContent).toContain("预览流恢复失败，请重试");

      // 恢复 GET 并点「重试预览」（同 ticket 手动重试），确认复用原票据但不清零 streamErrorCount
      failPollTransiently = false;
      const retryBtn = Array.from(mounted.container.querySelectorAll("button")).find(
        (b) => b.textContent?.trim() === "重试预览",
      );
      expect(retryBtn).toBeDefined();
      await dom.click(retryBtn);
      await dom.flush(50);
      expect(createCalls).toBe(1);
      expect(
        mounted.container.querySelector('div[aria-label="整集 CPU 预览"] video'),
      ).not.toBeNull();

      // 接着再触发 2 次 error：第 3 次（总第 3 次额度）恢复，第 4 次立刻触发流加载失败
      const pollsBeforeRemaining = pollSuccessCalls;
      for (const waitMs of [460, 120]) {
        const curVideo = mounted.container.querySelector(
          'div[aria-label="整集 CPU 预览"] video',
        ) as HTMLVideoElement | null;
        await dom.act(async () => {
          curVideo?.dispatchEvent(new dom.win.Event("error"));
        });
        await dom.flush(waitMs);
      }
      expect(pollSuccessCalls).toBe(pollsBeforeRemaining + 1);
      expect(mounted.container.textContent).toContain("预览流加载失败，请重试");
    } finally {
      await mounted.unmount();
    }
  });

  test("M1 正向回归(c)：无卡顿无 seek 的真实连续播放 >= 1.5s 正常清零 streamErrorCount 预算", async () => {
    const s1 = makeSegment("s1", 0);
    let pollCalls = 0;
    let nowMs = 1_000;
    const validExpiresAt = Math.floor(Date.now() / 1000) + 600;
    spies.push(
      spyOn(performance, "now").mockImplementation(() => nowMs),
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-prev/episodes/1/manifest") {
          return {
            manifest: { v: 1, fps: 24, total_frames: 120, clips: [] },
            manifest_hash: "hash-m1-ok",
            segment_version: 3,
            skipped_seqs: [],
            diagnostics: [],
          } as never;
        }
        if (url === "/dramas/d-prev/episodes/1/preview/tk-m1-ok") {
          pollCalls += 1;
          return {
            ticket: "tk-m1-ok",
            status: "ready",
            stream_url: `/api/v1/dramas/d-prev/episodes/1/preview/tk-m1-ok/stream?token=v${pollCalls}`,
            expires_at: validExpiresAt,
          } as never;
        }
        return {} as never;
      }),
      spyOn(service, "post").mockImplementation(
        async () =>
          ({
            ticket: "tk-m1-ok",
            status: "ready",
            stream_url: "/api/v1/dramas/d-prev/episodes/1/preview/tk-m1-ok/stream?token=v0",
            expires_at: validExpiresAt,
          }) as never,
      ),
    );

    const mounted = await dom.mount(
      <StudioPreviewHarness
        initialDrama={makeDrama()}
        initialSegments={[s1]}
        onPatch={async () => s1}
      />,
    );
    try {
      const cpuBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("CPU 预览"),
      );
      await dom.click(cpuBtn);
      await dom.flush(50);

      for (const waitMs of [40, 260]) {
        const curVideo = mounted.container.querySelector(
          'div[aria-label="整集 CPU 预览"] video',
        ) as HTMLVideoElement | null;
        await dom.act(async () => {
          curVideo?.dispatchEvent(new dom.win.Event("error"));
        });
        await dom.flush(waitMs);
      }
      expect(pollCalls).toBe(2);

      const video = mounted.container.querySelector(
        'div[aria-label="整集 CPU 预览"] video',
      ) as HTMLVideoElement;
      let currentTimeVal = 0;
      Object.defineProperties(video, {
        currentTime: {
          configurable: true,
          get: () => currentTimeVal,
          set: (val: number) => {
            currentTimeVal = val;
          },
        },
        paused: { configurable: true, get: () => false },
        seeking: { configurable: true, get: () => false },
        ended: { configurable: true, get: () => false },
        playbackRate: { configurable: true, get: () => 1 },
      });

      await dom.act(async () => {
        nowMs = 1_000;
        currentTimeVal = 0;
        video.dispatchEvent(new dom.win.Event("loadedmetadata"));
        video.dispatchEvent(new dom.win.Event("playing"));

        nowMs = 1_800;
        currentTimeVal = 0.8;
        video.dispatchEvent(new dom.win.Event("timeupdate"));

        nowMs = 2_600;
        currentTimeVal = 1.6;
        video.dispatchEvent(new dom.win.Event("timeupdate"));
      });

      // 错误预算已重置：接下来 3 次流错全部应正常恢复
      for (const waitMs of [40, 260, 460]) {
        const curVideo = mounted.container.querySelector(
          'div[aria-label="整集 CPU 预览"] video',
        ) as HTMLVideoElement | null;
        await dom.act(async () => {
          curVideo?.dispatchEvent(new dom.win.Event("error"));
        });
        await dom.flush(waitMs);
      }
      expect(pollCalls).toBe(5);
      expect(mounted.container.textContent).not.toContain("预览流加载失败，请重试");
      expect(
        mounted.container.querySelector('div[aria-label="整集 CPU 预览"] video'),
      ).not.toBeNull();
    } finally {
      await mounted.unmount();
    }
  });

  test("CPU 预览冷启动支持 manifest preparing 退避轮询、冷却期刷新识别 preparing 与超时处理", async () => {
    const s1 = makeSegment("s1", 0);
    let manifestMode: "cold_ready" | "failed_cooldown" | "refresh_preparing" | "timeout" =
      "cold_ready";
    let manifestCalls = 0;
    let createCalls = 0;
    let nowMs = 0;
    const validExpiresAt = Math.floor(Date.now() / 1000) + 600;

    spies.push(
      spyOn(performance, "now").mockImplementation(() => nowMs),
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-prev/episodes/1/manifest") {
          manifestCalls += 1;
          if (manifestMode === "cold_ready") {
            if (manifestCalls <= 2) {
              return {
                status: "preparing",
                segment_version: 3,
                skipped_seqs: [],
                diagnostics: [],
              } as never;
            }
            return {
              status: "ready",
              manifest: { v: 1, fps: 24, total_frames: 120, clips: [] },
              manifest_hash: "hash-cold-ok",
              segment_version: 3,
              skipped_seqs: [],
              diagnostics: [],
            } as never;
          }
          if (manifestMode === "failed_cooldown") {
            return {
              status: "ready",
              manifest: { v: 1, fps: 24, total_frames: 120, clips: [] },
              manifest_hash: "hash-failed-1",
              segment_version: 4,
              skipped_seqs: [],
              diagnostics: [],
            } as never;
          }
          if (manifestMode === "refresh_preparing") {
            if (manifestCalls === 1 || manifestCalls === 2) {
              return {
                status: "preparing",
                segment_version: 5,
                skipped_seqs: [],
                diagnostics: [],
              } as never;
            }
            return {
              status: "ready",
              manifest: { v: 1, fps: 24, total_frames: 120, clips: [] },
              manifest_hash: "hash-rebuilt-5",
              segment_version: 5,
              skipped_seqs: [],
              diagnostics: [],
            } as never;
          }
          return {
            status: "preparing",
            segment_version: 6,
            skipped_seqs: [],
            diagnostics: [],
          } as never;
        }
        return {} as never;
      }),
      spyOn(service, "post").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-prev/episodes/1/preview") {
          createCalls += 1;
          if (manifestMode === "failed_cooldown") {
            return {
              ticket: "tk-cold-fail",
              status: "failed",
              stream_url: null,
              expires_at: Math.floor(Date.now() / 1000) + 10,
              error: "failed",
            } as never;
          }
          return {
            ticket: `tk-cold-${createCalls}`,
            status: "ready",
            stream_url: `/api/v1/dramas/d-prev/episodes/1/preview/tk-cold-${createCalls}/stream?token=v1`,
            expires_at: validExpiresAt,
          } as never;
        }
        return {} as never;
      }),
    );

    const mounted = await dom.mount(
      <StudioPreviewHarness
        initialDrama={makeDrama()}
        initialSegments={[s1]}
        onPatch={async () => s1}
      />,
    );
    try {
      const cpuBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("CPU 预览"),
      );
      await dom.click(cpuBtn);
      await dom.flush(30);
      // preparing 期间维持 loading 且绝不显示占位 v3 · # · 0f，也不发 POST /preview
      expect(createCalls).toBe(0);
      expect(mounted.container.textContent).not.toContain("v3 · # · 0f");

      await dom.flush(700);
      expect(manifestCalls).toBe(3);
      expect(createCalls).toBe(1);
      expect(mounted.container.textContent).toContain("v3 · #hash-col · 120f");
      expect(
        mounted.container
          .querySelector('div[aria-label="整集 CPU 预览"] video')
          ?.getAttribute("src"),
      ).toContain("tk-cold-1/stream");

      // 关闭再进入失败冷却场景 -> 点击「刷新预览」遇到 fresh.status === "preparing" 时应触发重建流程
      manifestMode = "failed_cooldown";
      manifestCalls = 0;
      const closeBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("返回单镜"),
      );
      await dom.click(closeBtn);
      await dom.flush(20);
      await dom.click(cpuBtn);
      await dom.flush(50);
      expect(mounted.container.textContent).toContain("同版本失败票据缓存中");

      manifestMode = "refresh_preparing";
      manifestCalls = 0;
      const refreshBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("刷新预览"),
      );
      await dom.click(refreshBtn);
      await dom.flush(350);
      expect(createCalls).toBe(3);
      expect(mounted.container.textContent).toContain("v5 · #hash-reb · 120f");

      // preparing 超时分支应终止 loading 并给出明确可重试提示，不请求 POST /preview
      manifestMode = "timeout";
      manifestCalls = 0;
      const createsBeforeTimeout = createCalls;
      nowMs = 0;
      const refreshTimeoutBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("刷新预览"),
      );
      await dom.click(refreshTimeoutBtn);
      await dom.flush(30);
      nowMs = 130_000;
      await dom.flush(250);
      expect(createCalls).toBe(createsBeforeTimeout);
      expect(mounted.container.textContent).toContain("分镜清单准备超时，请稍后重试");
    } finally {
      await mounted.unmount();
    }
  });

  test("对抗性停顿样本 (0,0)->(800,0.8)->(1700,1.0)->(2200,1.5)（timeupdate）与 0.8s 播+0.9s 卡+0.8s 播（定时器）均不得重置错误预算，连续 timeupdate >=1.5s 才重置", async () => {
    const setupPreview = async () => {
      const s1 = makeSegment("s1", 0);
      let createCalls = 0;
      let pollCalls = 0;
      const validExpiresAt = Math.floor(Date.now() / 1000) + 600;
      const sGet = spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-prev/episodes/1/manifest") {
          return {
            manifest: { v: 1, fps: 24, total_frames: 120, clips: [] },
            manifest_hash: "hash-adv",
            segment_version: 3,
            skipped_seqs: [],
            diagnostics: [],
          } as never;
        }
        if (url === "/dramas/d-prev/episodes/1/preview/tk-adv") {
          pollCalls += 1;
          return {
            ticket: "tk-adv",
            status: "ready",
            stream_url: `/api/v1/dramas/d-prev/episodes/1/preview/tk-adv/stream?token=v${pollCalls}`,
            expires_at: validExpiresAt,
          } as never;
        }
        return {} as never;
      });
      const sPost = spyOn(service, "post").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-prev/episodes/1/preview") {
          createCalls += 1;
          return {
            ticket: "tk-adv",
            status: "ready",
            stream_url: "/api/v1/dramas/d-prev/episodes/1/preview/tk-adv/stream?token=v0",
            expires_at: validExpiresAt,
          } as never;
        }
        return {} as never;
      });
      spies.push(sGet, sPost);

      const mounted = await dom.mount(
        <StudioPreviewHarness
          initialDrama={makeDrama()}
          initialSegments={[s1]}
          onPatch={async () => s1}
        />,
      );
      const cpuBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("CPU 预览"),
      );
      await dom.click(cpuBtn);
      await dom.flush(50);

      // 先消耗 2 次错误重试预算（上限 3 次，剩 1 次）
      for (const waitMs of [260, 460]) {
        const curVideo = mounted.container.querySelector(
          'div[aria-label="整集 CPU 预览"] video',
        ) as HTMLVideoElement | null;
        await dom.act(async () => {
          curVideo?.dispatchEvent(new dom.win.Event("error"));
        });
        await dom.flush(waitMs);
      }
      expect(pollCalls).toBe(2);
      const video = mounted.container.querySelector(
        'div[aria-label="整集 CPU 预览"] video',
      ) as HTMLVideoElement;
      let currentTimeVal = 0;
      Object.defineProperties(video, {
        currentTime: {
          configurable: true,
          get: () => currentTimeVal,
          set: (val: number) => {
            currentTimeVal = val;
          },
        },
        paused: { configurable: true, value: false },
        seeking: { configurable: true, value: false },
        ended: { configurable: true, value: false },
        playbackRate: { configurable: true, value: 1 },
      });
      return {
        mounted,
        video,
        setCurrentTime: (t: number) => {
          currentTimeVal = t;
        },
        getCounts: () => ({ createCalls, pollCalls }),
      };
    };

    // Case 1: timeupdate 对抗序列 (0,0) -> (800,0.8) -> (1700,1.0) -> (2200,1.5)
    {
      let nowMs = 0;
      const nowSpy = spyOn(performance, "now").mockImplementation(() => nowMs);
      spies.push(nowSpy);
      const ctx = await setupPreview();
      try {
        await dom.act(async () => {
          ctx.setCurrentTime(0);
          ctx.video.dispatchEvent(new dom.win.Event("loadedmetadata"));
          ctx.video.dispatchEvent(new dom.win.Event("playing"));
          nowMs = 800;
          ctx.setCurrentTime(0.8);
          ctx.video.dispatchEvent(new dom.win.Event("timeupdate"));
          nowMs = 1_700;
          ctx.setCurrentTime(1.0);
          ctx.video.dispatchEvent(new dom.win.Event("timeupdate"));
          nowMs = 2_200;
          ctx.setCurrentTime(1.5);
          ctx.video.dispatchEvent(new dom.win.Event("timeupdate"));
        });
        nowSpy.mockRestore();

        // 若错误预算被错误重置则还能重试 3 次；未重置时第 3 次是最后 1 次重试，第 4 次立即失败中止
        await dom.act(async () => {
          const curVideo = ctx.mounted.container.querySelector(
            'div[aria-label="整集 CPU 预览"] video',
          ) as HTMLVideoElement | null;
          curVideo?.dispatchEvent(new dom.win.Event("error"));
        });
        await dom.flush(650);
        expect(ctx.getCounts().pollCalls).toBe(3);

        await dom.act(async () => {
          const curVideo = ctx.mounted.container.querySelector(
            'div[aria-label="整集 CPU 预览"] video',
          ) as HTMLVideoElement | null;
          curVideo?.dispatchEvent(new dom.win.Event("error"));
        });
        await dom.flush(50);
        expect(ctx.getCounts().createCalls).toBe(1);
        expect(ctx.getCounts().pollCalls).toBe(3);
        expect(ctx.mounted.container.textContent).toContain("预览流加载失败，请重试");
      } finally {
        await ctx.mounted.unmount();
      }
    }

    // Case 2: 定时器采样路径（无 DOM timeupdate）0.8s 播 -> 0.9s 卡顿 -> 0.8s 播
    {
      let nowMs = 1_000;
      const nowSpy = spyOn(performance, "now").mockImplementation(() => nowMs);
      spies.push(nowSpy);
      const ctx = await setupPreview();
      try {
        await dom.act(async () => {
          ctx.setCurrentTime(0);
          ctx.video.dispatchEvent(new dom.win.Event("loadedmetadata"));
          ctx.video.dispatchEvent(new dom.win.Event("playing"));
        });
        // 0.8s 播放
        nowMs = 1_400;
        ctx.setCurrentTime(0.4);
        await dom.flush(450);
        nowMs = 1_800;
        ctx.setCurrentTime(0.8);
        await dom.flush(450);
        // 0.9s 停顿（播放头冻结在 0.8）
        nowMs = 2_250;
        await dom.flush(450);
        nowMs = 2_700;
        await dom.flush(450);
        // 再播 0.8s（距停顿重置仅推进 0.8s < 1.5s）
        nowMs = 3_100;
        ctx.setCurrentTime(1.2);
        await dom.flush(450);
        nowMs = 3_500;
        ctx.setCurrentTime(1.6);
        await dom.flush(450);
        nowSpy.mockRestore();

        // 第 3 次 error 用尽剩余预算，第 4 次立刻中止
        await dom.act(async () => {
          const curVideo = ctx.mounted.container.querySelector(
            'div[aria-label="整集 CPU 预览"] video',
          ) as HTMLVideoElement | null;
          curVideo?.dispatchEvent(new dom.win.Event("error"));
        });
        await dom.flush(650);
        expect(ctx.getCounts().pollCalls).toBe(3);

        await dom.act(async () => {
          const curVideo = ctx.mounted.container.querySelector(
            'div[aria-label="整集 CPU 预览"] video',
          ) as HTMLVideoElement | null;
          curVideo?.dispatchEvent(new dom.win.Event("error"));
        });
        await dom.flush(50);
        expect(ctx.getCounts().pollCalls).toBe(3);
        expect(ctx.mounted.container.textContent).toContain("预览流加载失败，请重试");
      } finally {
        await ctx.mounted.unmount();
      }
    }

    // Case 3: 正向校验 — 连续无 seek 的 timeupdate 推进 >= 1.5s 且墙钟跨度 >= 1.2s 时重置预算
    {
      let nowMs = 1_000;
      const nowSpy = spyOn(performance, "now").mockImplementation(() => nowMs);
      spies.push(nowSpy);
      const ctx = await setupPreview();
      try {
        await dom.act(async () => {
          ctx.setCurrentTime(0);
          ctx.video.dispatchEvent(new dom.win.Event("loadedmetadata"));
          ctx.video.dispatchEvent(new dom.win.Event("playing"));
          nowMs = 1_800;
          ctx.setCurrentTime(0.8);
          ctx.video.dispatchEvent(new dom.win.Event("timeupdate"));
          nowMs = 2_600;
          ctx.setCurrentTime(1.6);
          ctx.video.dispatchEvent(new dom.win.Event("timeupdate"));
        });
        nowSpy.mockRestore();

        for (const waitMs of [50, 260, 460, 50]) {
          await dom.act(async () => {
            const curVideo = ctx.mounted.container.querySelector(
              'div[aria-label="整集 CPU 预览"] video',
            ) as HTMLVideoElement | null;
            curVideo?.dispatchEvent(new dom.win.Event("error"));
          });
          await dom.flush(waitMs);
        }
        expect(ctx.getCounts().createCalls).toBe(1);
        expect(ctx.getCounts().pollCalls).toBe(5);
        expect(ctx.mounted.container.textContent).toContain("预览流加载失败，请重试");
      } finally {
        await ctx.mounted.unmount();
      }
    }
  }, 15000);

  test("Preview 轮询 manifest 超过 120s 绝对截止时间即使慢响应返回 ready 也判定超时，关闭预览立即 abort 在途请求", async () => {
    const s1 = makeSegment("s1", 0);
    let nowMs = 0;
    let createCalls = 0;
    let manifestCalls = 0;
    let capturedAbortSignal: AbortSignal | undefined;
    let mode: "slow_ready_past_deadline" | "hang_until_abort" = "slow_ready_past_deadline";

    spies.push(
      spyOn(performance, "now").mockImplementation(() => nowMs),
      spyOn(service, "get").mockImplementation(
        async (url: string, _params?: unknown, cfg?: unknown) => {
          if (url === "/dramas/d-prev/episodes/1/manifest") {
            manifestCalls += 1;
            capturedAbortSignal = (cfg as { signal?: AbortSignal } | undefined)?.signal;
            if (mode === "slow_ready_past_deadline") {
              if (manifestCalls === 1) {
                nowMs = 119_900;
                return {
                  status: "preparing",
                  segment_version: 3,
                  skipped_seqs: [],
                  diagnostics: [],
                } as never;
              }
              // 慢请求在 210.1s 才返回 ready（已超过 120s 绝对截止线），严禁继续创建预览
              nowMs = 210_100;
              return {
                status: "ready",
                manifest: { v: 1, fps: 24, total_frames: 120, clips: [] },
                manifest_hash: "hash-late",
                segment_version: 3,
                skipped_seqs: [],
                diagnostics: [],
              } as never;
            }
            return await new Promise((_resolve, reject) => {
              capturedAbortSignal?.addEventListener("abort", () => reject(new Error("aborted")), {
                once: true,
              });
            });
          }
          return {} as never;
        },
      ),
      spyOn(service, "post").mockImplementation(async () => {
        createCalls += 1;
        return {} as never;
      }),
    );

    const mounted = await dom.mount(
      <StudioPreviewHarness
        initialDrama={makeDrama()}
        initialSegments={[s1]}
        onPatch={async () => s1}
      />,
    );
    try {
      const cpuBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("CPU 预览"),
      );
      await dom.click(cpuBtn);
      await dom.flush(150);

      expect(manifestCalls).toBe(2);
      expect(createCalls).toBe(0);
      expect(mounted.container.textContent).toContain("分镜清单准备超时，请稍后重试");

      // 关闭重试挂起请求时，返回单镜必须立即触发在途请求 AbortSignal
      mode = "hang_until_abort";
      nowMs = 0;
      const refreshBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("刷新预览"),
      );
      await dom.click(refreshBtn);
      await dom.flush(30);
      expect(capturedAbortSignal?.aborted).toBe(false);

      const closeBtn = Array.from(mounted.container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("返回单镜"),
      );
      await dom.click(closeBtn);
      await dom.flush(30);
      expect(capturedAbortSignal?.aborted).toBe(true);
      expect(createCalls).toBe(0);
    } finally {
      await mounted.unmount();
    }
  });

  test("V1 失败冷却期内远端切换为黑场后，点击「刷新预览」遇到 67010 时应重拉片段并切换到 manifest?v=2", async () => {
    const s1 = makeSegment("s1", 0);
    let switchedToBlack = false;
    let postCalls = 0;
    const get = spyOn(service, "get").mockImplementation(async (url: string) => {
      if (url === "/dramas/d-prev/episodes/1/program") {
        return {
          episode_id: "ep-1",
          segment_version: switchedToBlack ? 2 : 1,
          edit_schema: 1,
          program: { v: 1, overlays: [], audio: [] },
        } as never;
      }
      if (url === "/dramas/d-prev/episodes/1/segments") {
        return {
          episode_id: "ep-1",
          segment_version: switchedToBlack ? 2 : 1,
          items: [
            switchedToBlack
              ? {
                  id: "s1",
                  episode_id: "ep-1",
                  episode_no: 1,
                  seq: 0,
                  seconds: 5,
                  prompt: "seg",
                  chosen_task_id: "",
                  visual_kind: "black",
                  segment_version: 2,
                  takes: [],
                  status: "idle",
                  revision: 2,
                  updated_at: "2026-01-01T00:00:01Z",
                }
              : {
                  id: "s1",
                  episode_id: "ep-1",
                  episode_no: 1,
                  seq: 0,
                  seconds: 5,
                  prompt: "seg",
                  chosen_task_id: "tk-s1",
                  takes: [
                    {
                      task_id: "tk-s1",
                      status: "succeeded",
                      url: "https://cdn/s1.mp4",
                      asset: { id: 100, url: "https://cdn/s1.mp4" },
                    },
                  ],
                  status: "done",
                  revision: 1,
                  updated_at: "2026-01-01T00:00:00Z",
                },
          ],
        } as never;
      }
      if (url === "/dramas/d-prev/episodes/1/manifest") {
        if (switchedToBlack) {
          throw new ApiError("这一集还没有可合成的片段", DRAMA_ERR.nothingToCompose, 400);
        }
        return {
          status: "ready",
          manifest_hash: "v1-fail-hash",
          segment_version: 1,
          skipped_seqs: [],
          diagnostics: [],
          manifest: { v: 1, fps: 24, total_frames: 120, clips: [] },
        } as never;
      }
      if (url === "/dramas/d-prev/episodes/1/manifest?v=2") {
        return {
          status: "ready",
          manifest_hash: "v2-cooldown-hash",
          segment_version: 2,
          skipped_seqs: [],
          program_diagnostics: [],
          diagnostics: [],
          manifest: {
            v: 2,
            render_version: 2,
            episode_id: "ep-1",
            segment_version: 2,
            fps: 24,
            sample_rate: 48000,
            total_frames: 120,
            skipped_seqs: [],
            clips: [
              {
                segment_id: "s1",
                visual_kind: "black",
                seq: 0,
                source_in_f: 0,
                source_out_f: 120,
                master_in_f: 0,
                master_out_f: 120,
              },
            ],
            program: { v: 1, overlays: [], audio: [] },
          },
        } as never;
      }
      throw new Error(`unexpected GET ${url}`);
    });
    const post = spyOn(service, "post").mockImplementation(async (_url, body) => {
      postCalls += 1;
      const hash = (body as { manifest_hash?: string }).manifest_hash;
      if (hash === "v1-fail-hash") {
        return {
          ticket: "tk-v1-fail",
          status: "failed",
          stream_url: null,
          expires_at: Math.floor(Date.now() / 1000) + 20,
          error: "failed",
        } as never;
      }
      return {
        ticket: "tk-v2-ok",
        status: "ready",
        stream_url: "/api/v1/dramas/d-prev/episodes/1/preview/tk-v2-ok/stream?token=ok",
        expires_at: Math.floor(Date.now() / 1000) + 600,
      } as never;
    });
    spies.push(get, post);

    const baseDrama = makeDrama();
    const mounted = await dom.mount(
      <StudioPreviewHarness
        enableProgram
        initialDrama={{
          ...baseDrama,
          episodes: [{ ...baseDrama.episodes[0]!, id: "ep-1", segmentVersion: 1 }],
        }}
        initialSegments={[s1]}
        onPatch={async () => s1}
      />,
    );
    try {
      const cpuBtn = [...mounted.container.querySelectorAll("button")].find((b) =>
        b.textContent?.includes("CPU 预览"),
      );
      await dom.click(cpuBtn);
      await dom.flush(30);
      expect(mounted.container.textContent).toContain("同版本失败票据缓存中");

      switchedToBlack = true;
      const refreshBtn = [...mounted.container.querySelectorAll("button")].find((b) =>
        b.textContent?.includes("刷新预览"),
      );
      expect(refreshBtn).toBeTruthy();
      await dom.click(refreshBtn);
      await dom.flush(50);

      expect(get.mock.calls.map(([url]) => url)).toContain(
        "/dramas/d-prev/episodes/1/manifest?v=2",
      );
      expect(postCalls).toBe(2);
      expect(mounted.container.textContent).not.toContain("同版本失败票据缓存中");
      expect(
        mounted.container
          .querySelector('div[aria-label="整集 CPU 预览"] video')
          ?.getAttribute("src"),
      ).toContain("tk-v2-ok/stream");
    } finally {
      useDramaSegmentsStore.setState({ byEpisode: {}, watching: {} });
      await mounted.unmount();
    }
  });
});
