import { afterAll, afterEach, beforeAll, describe, expect, spyOn, test } from "bun:test";
import { useEffect, useState, type ComponentProps } from "react";

import type { Drama, EpisodeSummary } from "@/api/drama/type";
import { DramaWorkspaceContext } from "@/pages/drama/workspace/context";
import type { ScriptArea as ScriptAreaType } from "@/pages/drama/workspace/script-area";
import type { DramaWorkspace } from "@/pages/drama/workspace/use-workspace";
import { ApiError } from "@/utils/requests/request";
import service from "@/utils/requests/service";
import { installDom, type DomEnv } from "@/tests/helpers/dom";

const makeSummary = (
  patch: Partial<EpisodeSummary> & Pick<EpisodeSummary, "no">,
): EpisodeSummary => ({
  title: `第${patch.no}集`,
  synopsis: "",
  scriptStatus: "ready",
  locked: false,
  revision: 1,
  updatedAt: "2026-11-16T00:00:00Z",
  storyboardStatus: "empty",
  segmentCount: 0,
  segmentSeconds: 0,
  doneSegmentCount: 0,
  final: null,
  finalStale: false,
  hook: "",
  cliffhanger: "",
  unitFrom: 1,
  unitTo: 1,
  ...patch,
});

const makeDrama = (id: string, episodes: EpisodeSummary[]): Drama => ({
  id,
  title: "测试短剧",
  canvasId: `c-${id}`,
  status: "ready",
  sourceKind: "script",
  genre: "都市",
  styleKey: "real_city_film",
  styleCustom: "",
  ratio: "9:16",
  targetEpisodes: episodes.length,
  episodeSeconds: 30,
  textModel: "",
  videoModel: "",
  imageModels: { character: "", scene: "", prop: "" },
  outlineMd: "# 大纲",
  charactersMd: "",
  revision: 1,
  episodes,
  activeJobs: [],
  createdAt: "2026-11-16T00:00:00Z",
  updatedAt: "2026-11-16T00:00:00Z",
  sourceChars: 100,
  unitsTotal: episodes.length,
  unitKind: "episode",
  phase: "done",
  planCursor: episodes.length,
  rangeEnd: episodes.length,
  pauseReason: "",
  pauseCode: "",
  promptSettings: {
    adapt: { source: "builtin", label: "内置" },
    assets: { source: "builtin", label: "内置" },
    asset_prompts: { source: "builtin", label: "内置" },
    video: { source: "builtin", label: "内置" },
  },
});

const makeBackendEpisode = (id: string, no: number, revision: number, scriptMd: string) => ({
  id,
  no,
  title: `第${no}集`,
  synopsis: "",
  script_status: "ready" as const,
  locked: false,
  revision,
  updated_at: "2026-11-16T00:00:00Z",
  storyboard_status: "empty" as const,
  segment_count: 0,
  segment_seconds: 0,
  done_segment_count: 0,
  final: null,
  final_stale: false,
  hook: "",
  cliffhanger: "",
  unit_from: 1,
  unit_to: 1,
  script_md: scriptMd,
});

describe("ScriptArea：删集重排与实体身份隔离", () => {
  let dom: DomEnv;
  let ScriptArea: typeof ScriptAreaType;
  const spies: Array<{ mockRestore: () => void }> = [];

  beforeAll(async () => {
    dom = await installDom();
    ScriptArea = (await import("@/pages/drama/workspace/script-area")).ScriptArea;
  });

  afterEach(() => {
    while (spies.length > 0) spies.pop()?.mockRestore();
  });

  afterAll(async () => {
    // 等待 @tiptap/react 卸载时调度的 setTimeout(..., 1) 销毁完成
    await dom.flush(25);
    dom.restore();
  });

  async function editProseMirrorText(container: HTMLElement, nextText: string) {
    const pm = container.querySelector(".ProseMirror");
    expect(pm).not.toBeNull();
    const paragraph = pm!.querySelector("p") ?? pm!;
    await dom.act(async () => {
      paragraph.textContent = nextText;
    });
    await dom.flush(30);
  }

  test("V4 反例：A(ep-a,no=1,rev=5) 删集后 B(ep-b,no=2,rev=4)->(no=1,rev=5) 不复用 A 正文缓存且卸载/防抖不覆盖 B", async () => {
    const epA = makeSummary({ id: "ep-a", no: 1, revision: 5, scriptStatus: "ready" });
    const epBBefore = makeSummary({ id: "ep-b", no: 2, revision: 4, scriptStatus: "ready" });
    const epBAfter = makeSummary({ id: "ep-b", no: 1, revision: 5, scriptStatus: "ready" });

    let phase: "before-delete" | "after-delete" = "before-delete";
    const getCalls: string[] = [];
    const patchCalls: Array<{ url: string; body: Record<string, unknown> }> = [];
    const saveStates: Record<string, string> = {};
    const appliedCalls: Array<{ no: number; change: Partial<EpisodeSummary> }> = [];

    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        getCalls.push(url);
        if (url === "/dramas/d-race/episodes/1") {
          return (
            phase === "before-delete"
              ? makeBackendEpisode("ep-a", 1, 5, "script-A")
              : makeBackendEpisode("ep-b", 1, 5, "script-B")
          ) as never;
        }
        return {} as never;
      }),
      spyOn(service, "patch").mockImplementation(async (url: string, body?: unknown) => {
        const record = (body ?? {}) as Record<string, unknown>;
        patchCalls.push({ url, body: record });
        return makeBackendEpisode(
          "ep-b",
          1,
          Number(record.revision ?? 5) + 1,
          String(record.script_md ?? "script-B"),
        ) as never;
      }),
    );

    let setDramaState!: (next: Drama) => void;
    function Harness({
      initialDrama,
      onReady,
    }: {
      initialDrama: Drama;
      onReady: (setter: (next: Drama) => void) => void;
    }) {
      const [drama, setDrama] = useState(initialDrama);
      useEffect(() => {
        onReady(setDrama);
      }, [onReady]);
      const value = {
        drama,
        jobs: [],
        save: "saved",
        refresh: async () => {},
        patch: async () => drama.revision,
        applyEpisode: (no, change) => {
          appliedCalls.push({ no, change });
        },
        reportSave: (key, status) => {
          saveStates[key] = status;
        },
      } as DramaWorkspace;
      const props: ComponentProps<typeof ScriptAreaType> = {
        ep: 1,
        doc: null,
        onEp: () => {},
        onDoc: () => {},
        onBreakdownOpen: () => {},
      };
      return (
        <DramaWorkspaceContext.Provider value={value}>
          <ScriptArea {...props} />
        </DramaWorkspaceContext.Provider>
      );
    }

    const mounted = await dom.mount(
      <Harness
        initialDrama={makeDrama("d-race", [epA, epBBefore])}
        onReady={(setter) => {
          setDramaState = setter;
        }}
      />,
    );
    try {
      await dom.flush(30);
      expect(getCalls).toEqual(["/dramas/d-race/episodes/1"]);
      expect(mounted.container.textContent).toContain("script-A");
      expect(mounted.container.querySelector(".ProseMirror")).not.toBeNull();

      // 编辑 A 但尚未触发自动保存（仍在防抖窗口内）
      await editProseMirrorText(mounted.container, "script-A edited");
      expect(mounted.container.textContent).toContain("script-A edited");
      expect(saveStates["ep-a"]).toBe("dirty");
      expect(patchCalls).toHaveLength(0);

      // 删除 A，B 重排补位成为第 1 集且 revision=5、scriptStatus="ready"
      phase = "after-delete";
      await dom.act(async () => {
        setDramaState(makeDrama("d-race", [epBAfter]));
      });
      await dom.flush(40);

      // 必须重新 GET 一次 B（共 2 次 GET），绝不复用 A 的缓存或内容
      expect(getCalls).toEqual(["/dramas/d-race/episodes/1", "/dramas/d-race/episodes/1"]);
      expect(mounted.container.textContent).toContain("script-B");
      expect(mounted.container.textContent).not.toContain("script-A");

      // 卸载 A 时不得把 "script-A edited" 刷给第 1 集 B，也不得卡在 dirty/error
      expect(patchCalls).toHaveLength(0);
      expect(saveStates["ep-a"]).toBe("saved");

      // 等待超过原 800ms 防抖时间，确认 A 的旧定时器已被清理且不会延迟触发
      await dom.flush(900);
      expect(patchCalls).toHaveLength(0);

      // 编辑 B 并卸载，必须携带 expected_episode_id: "ep-b" 保存 B 自己的改动
      await editProseMirrorText(mounted.container, "script-B edited");
      await mounted.unmount();
      await dom.flush(30);

      expect(patchCalls).toEqual([
        {
          url: "/dramas/d-race/episodes/1",
          body: {
            revision: 5,
            expected_episode_id: "ep-b",
            script_md: "script-B edited",
          },
        },
      ]);
      expect(appliedCalls.some((c) => c.change.id === "ep-b" && c.change.revision === 6)).toBe(
        true,
      );
    } finally {
      await mounted.unmount();
    }
  });

  test("在途 GET 返回串集响应或 PATCH 命中 expected_episode_id 409 时丢弃脏写并触发 refresh", async () => {
    const epA = makeSummary({ id: "ep-a", no: 1, revision: 5, scriptStatus: "ready" });
    let refreshCalls = 0;
    let getMode: "mismatch-initial" | "match-initial" | "mismatch-after-conflict" =
      "mismatch-initial";
    const patchPayloads: Array<Record<string, unknown>> = [];
    const appliedCalls: Array<{ no: number; change: Partial<EpisodeSummary> }> = [];

    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-inflight/episodes/1") {
          if (getMode === "match-initial") {
            return makeBackendEpisode("ep-a", 1, 5, "script-A") as never;
          }
          return makeBackendEpisode("ep-b", 1, 5, "script-B") as never;
        }
        return {} as never;
      }),
      spyOn(service, "patch").mockImplementation(async (_url: string, body?: unknown) => {
        patchPayloads.push((body ?? {}) as Record<string, unknown>);
        throw new ApiError("分集已变更", 30002, 409);
      }),
    );

    const baseWorkspace = (drama: Drama): DramaWorkspace => ({
      drama,
      jobs: [],
      save: "saved",
      refresh: async () => {
        refreshCalls += 1;
      },
      patch: async () => drama.revision,
      applyEpisode: (no, change) => {
        appliedCalls.push({ no, change });
      },
      reportSave: () => {},
    });

    // 1) 在途 GET 期望 ep-a，但服务端已删 A 重排返回 ep-b：绝不把 B 渲染成 A，并触发 refresh
    const m1 = await dom.mount(
      <DramaWorkspaceContext.Provider value={baseWorkspace(makeDrama("d-inflight", [epA]))}>
        <ScriptArea ep={1} doc={null} onEp={() => {}} onDoc={() => {}} onBreakdownOpen={() => {}} />
      </DramaWorkspaceContext.Provider>,
    );
    try {
      await dom.flush(30);
      expect(m1.container.textContent).not.toContain("script-B");
      expect(m1.container.textContent).toContain("这一集读取失败");
      expect(refreshCalls).toBe(1);
    } finally {
      await m1.unmount();
    }

    // 2) A 已加载，随后后端删 A 重排使 PATCH 409：请求带 expected_episode_id="ep-a"，conflict 回查拿到 ep-b 时不回写 A
    refreshCalls = 0;
    getMode = "match-initial";
    const m2 = await dom.mount(
      <DramaWorkspaceContext.Provider value={baseWorkspace(makeDrama("d-inflight", [epA]))}>
        <ScriptArea ep={1} doc={null} onEp={() => {}} onDoc={() => {}} onBreakdownOpen={() => {}} />
      </DramaWorkspaceContext.Provider>,
    );
    try {
      await dom.flush(30);
      expect(m2.container.textContent).toContain("script-A");

      getMode = "mismatch-after-conflict";
      await editProseMirrorText(m2.container, "script-A edited");
      await m2.unmount();
      await dom.flush(40);

      expect(patchPayloads).toEqual([
        {
          revision: 5,
          expected_episode_id: "ep-a",
          script_md: "script-A edited",
        },
      ]);
      expect(appliedCalls).toHaveLength(0);
      expect(refreshCalls).toBe(1);
    } finally {
      await m2.unmount();
    }
  });

  test("正常切集（ep-a 仍存在）卸载时照常自动保存并携带 expected_episode_id", async () => {
    const epA = makeSummary({ id: "ep-a", no: 1, revision: 5, scriptStatus: "ready" });
    const epB = makeSummary({ id: "ep-b", no: 2, revision: 4, scriptStatus: "ready" });
    const patchCalls: Array<{ url: string; body: Record<string, unknown> }> = [];

    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-normal/episodes/1") {
          return makeBackendEpisode("ep-a", 1, 5, "script-A") as never;
        }
        if (url === "/dramas/d-normal/episodes/2") {
          return makeBackendEpisode("ep-b", 2, 4, "script-B") as never;
        }
        return {} as never;
      }),
      spyOn(service, "patch").mockImplementation(async (url: string, body?: unknown) => {
        const record = (body ?? {}) as Record<string, unknown>;
        patchCalls.push({ url, body: record });
        return makeBackendEpisode("ep-a", 1, 6, String(record.script_md ?? "")) as never;
      }),
    );

    let setEpState!: (ep: number) => void;
    function SwitchHarness({ onReady }: { onReady: (setter: (ep: number) => void) => void }) {
      const [ep, setEp] = useState(1);
      useEffect(() => {
        onReady(setEp);
      }, [onReady]);
      const drama = makeDrama("d-normal", [epA, epB]);
      const value = {
        drama,
        jobs: [],
        save: "saved",
        refresh: async () => {},
        patch: async () => drama.revision,
        applyEpisode: () => {},
        reportSave: () => {},
      } as DramaWorkspace;
      return (
        <DramaWorkspaceContext.Provider value={value}>
          <ScriptArea ep={ep} doc={null} onEp={setEp} onDoc={() => {}} onBreakdownOpen={() => {}} />
        </DramaWorkspaceContext.Provider>
      );
    }

    const mounted = await dom.mount(
      <SwitchHarness
        onReady={(setter) => {
          setEpState = setter;
        }}
      />,
    );
    try {
      await dom.flush(30);
      expect(mounted.container.textContent).toContain("script-A");

      await editProseMirrorText(mounted.container, "script-A edited");
      await dom.act(async () => {
        setEpState(2);
      });
      await dom.flush(40);

      expect(patchCalls).toEqual([
        {
          url: "/dramas/d-normal/episodes/1",
          body: {
            revision: 5,
            expected_episode_id: "ep-a",
            script_md: "script-A edited",
          },
        },
      ]);
      expect(mounted.container.textContent).toContain("script-B");
    } finally {
      await mounted.unmount();
    }
  });
});
