import { beforeEach, describe, expect, spyOn, test } from "bun:test";

import { mapSegment } from "@/api/drama";
import type { BackendSegmentView, Segment } from "@/api/drama/type";
import { subscribeDramaChanges } from "@/utils/drama/episode-events";
import { useDramaSegmentsStore, segmentKey } from "@/store/drama-segments";
import {
  handleDramaSegmentEvent,
  reloadSegments,
  reconcileWatchedSegments,
} from "@/utils/drama/segment-events";
import service from "@/utils/requests/service";
import { parseServerMessage } from "@/utils/ws/socket-client";

const raw = (patch: Partial<BackendSegmentView> = {}): BackendSegmentView => ({
  id: "s1",
  episode_no: 1,
  seq: 0,
  seconds: 10,
  prompt: "p",
  takes: null,
  chosen_task_id: "",
  status: "idle",
  revision: 1,
  updated_at: "x",
  ...patch,
});

beforeEach(() => {
  useDramaSegmentsStore.setState({ byEpisode: {}, watching: {}, runFloors: {} });
});

describe("片段 store", () => {
  test("setList 整体替换，apply 并入单条，remove 删除", () => {
    const store = useDramaSegmentsStore.getState();
    store.setList("d1", 1, [{ ...handleShape("a", 0) }, { ...handleShape("b", 1) }]);
    expect(useDramaSegmentsStore.getState().byEpisode[segmentKey("d1", 1)]).toHaveLength(2);
    store.apply("d1", 1, { ...handleShape("a", 0), prompt: "新", revision: 2 });
    expect(useDramaSegmentsStore.getState().byEpisode[segmentKey("d1", 1)]![0].prompt).toBe("新");
    store.remove("d1", 1, "b");
    expect(
      useDramaSegmentsStore.getState().byEpisode[segmentKey("d1", 1)]!.map((s) => s.id),
    ).toEqual(["a"]);
  });

  test("forgetDrama：删集补位后丢掉这部短剧缓存的各集片段（集号变了，旧缓存会对错集），别的短剧不动", () => {
    const store = useDramaSegmentsStore.getState();
    store.setList("d1", 1, [handleShape("a", 0)]);
    store.setList("d1", 2, [handleShape("b", 0)]);
    store.setList("d10", 1, [handleShape("c", 0)]);
    store.forgetDrama("d1");
    expect(Object.keys(useDramaSegmentsStore.getState().byEpisode)).toEqual([segmentKey("d10", 1)]);
  });

  test("watch 引用计数：多处打开同一集，全部关掉才不再对账", () => {
    const store = useDramaSegmentsStore.getState();
    const off1 = store.watch("d1", 1);
    const off2 = store.watch("d1", 1);
    off1();
    expect(useDramaSegmentsStore.getState().watching[segmentKey("d1", 1)]).toBe(1);
    off2();
    expect(useDramaSegmentsStore.getState().watching[segmentKey("d1", 1)]).toBeUndefined();
  });

  test("setList 与 apply 按集身份和版本号防御迟到回退，并兼容仅清理 byEpisode 的测试重置", async () => {
    const store = useDramaSegmentsStore.getState();
    expect(
      store.setList(
        "d1",
        1,
        [{ ...handleShape("a", 0), seconds: 5, episodeId: "ep-1", segmentVersion: 3 }],
        undefined,
        {
          episodeId: "ep-1",
          segmentVersion: 3,
        },
      ),
    ).toBe(true);

    // 同分集更小版本号的迟到 setList 不得回退 5s(v3) 到 2s(v2)
    expect(
      store.setList(
        "d1",
        1,
        [{ ...handleShape("a", 0), seconds: 2, episodeId: "ep-1", segmentVersion: 2 }],
        undefined,
        {
          episodeId: "ep-1",
          segmentVersion: 2,
        },
      ),
    ).toBe(false);
    expect(useDramaSegmentsStore.getState().byEpisode[segmentKey("d1", 1)]?.[0].seconds).toBe(5);

    // 迟到的旧行修订不得覆盖，其他集的事件不得窜集
    store.apply("d1", 1, {
      ...handleShape("a", 0),
      seconds: 2,
      revision: 0,
      episodeId: "ep-1",
      segmentVersion: 2,
    });
    store.apply("d1", 1, {
      ...handleShape("foreign", 1),
      episodeId: "ep-other",
      segmentVersion: 10,
    });
    expect(useDramaSegmentsStore.getState().byEpisode[segmentKey("d1", 1)]).toHaveLength(1);
    expect(useDramaSegmentsStore.getState().byEpisode[segmentKey("d1", 1)]?.[0].seconds).toBe(5);

    // expectedEpisodeId 不匹配时 reloadSegments 拒绝落库
    const get = spyOn(service, "get").mockResolvedValue({
      episode_id: "ep-replaced",
      segment_version: 9,
      items: [raw({ id: "b", episode_id: "ep-replaced", segment_version: 9, seconds: 9 })],
    });
    try {
      expect(await reloadSegments("d1", 1, "ep-1")).toBe(false);
      expect(useDramaSegmentsStore.getState().byEpisode[segmentKey("d1", 1)]?.[0].id).toBe("a");
    } finally {
      get.mockRestore();
    }

    // 外部直接重置 byEpisode: {} 后，meta 不得残留阻塞新的较低版本号
    useDramaSegmentsStore.setState({ byEpisode: {}, watching: {} });
    expect(
      store.setList(
        "d1",
        1,
        [{ ...handleShape("fresh", 0), episodeId: "ep-1", segmentVersion: 1 }],
        undefined,
        {
          episodeId: "ep-1",
          segmentVersion: 1,
        },
      ),
    ).toBe(true);
    expect(useDramaSegmentsStore.getState().byEpisode[segmentKey("d1", 1)]?.[0].id).toBe("fresh");
  });
});

test("a delayed event updates its known clip without rolling back a different clip", () => {
  const store = useDramaSegmentsStore.getState();
  const a = { ...handleShape("a", 0), episodeId: "ep", segmentVersion: 1 };
  const b = { ...a, id: "b", seq: 1 };
  store.setList("d1", 1, [a, b]);
  const updatedB = { ...b, revision: 2, segmentVersion: 3, prompt: "new B" };
  const updatedA = { ...a, revision: 2, segmentVersion: 2, prompt: "new A" };
  expect(store.apply("d1", 1, updatedB)).toBe(true);
  expect(store.apply("d1", 1, updatedA)).toBe(true);
  expect(store.setList("d1", 1, [updatedA, b])).toBe(false);
  store.apply("d1", 1, a);
  expect(useDramaSegmentsStore.getState().byEpisode[segmentKey("d1", 1)]).toEqual([
    updatedA,
    updatedB,
  ]);
  expect(store.snapshotOf("d1", 1)?.segmentVersion).toBe(3);
});

describe("take progress outside the segment revision", () => {
  const adopted = {
    task_id: "adopted",
    status: "succeeded",
    asset: { id: 1, kind: "video", url: "/adopted.mp4" },
    error_message: "",
    created_at: "2026-11-17T10:00:00Z",
  } as const;
  const candidate = {
    task_id: "second",
    asset: null,
    error_message: "",
    created_at: "2026-11-17T10:01:00Z",
  };
  const snapshot = (
    status: Segment["status"],
    second: NonNullable<BackendSegmentView["takes"]>[number],
  ) =>
    mapSegment(
      raw({
        episode_id: "ep",
        segment_version: 5,
        revision: 2,
        status,
        chosen_task_id: "adopted",
        takes: [adopted, second],
      }),
    );
  const running = snapshot("running", { ...candidate, status: "running" });
  const clip = () => useDramaSegmentsStore.getState().byEpisode[segmentKey("d1", 1)]?.[0];
  const receive = (path: "apply" | "setList", segment: Segment) => {
    const store = useDramaSegmentsStore.getState();
    return path === "apply" ? store.apply("d1", 1, segment) : store.setList("d1", 1, [segment]);
  };

  for (const [completed, status] of [
    [
      {
        ...candidate,
        status: "succeeded",
        asset: { id: 2, kind: "video", url: "/second.mp4" },
        qc: "accepted",
      },
      "done",
    ],
    [{ ...candidate, status: "failed", error_message: "provider rejected the prompt" }, "error"],
    [{ ...candidate, status: "canceled" }, "done"],
  ] as const) {
    for (const path of ["apply", "setList"] as const) {
      for (const terminalFirst of [true, false]) {
        test(`${path}: ${completed.status} survives ${terminalFirst ? "terminal then running" : "running then terminal"}`, () => {
          const completedSnapshot = snapshot(status, completed);
          const [first, second] = terminalFirst
            ? [completedSnapshot, running]
            : [running, completedSnapshot];
          const store = useDramaSegmentsStore.getState();
          store.setList("d1", 1, [first]);
          expect(receive(path, second)).toBe(true);
          expect(clip()).toEqual(completedSnapshot);
        });
      }
    }
  }

  const videoPending: Segment["frameRun"] = {
    id: "frame-run",
    segmentId: "s1",
    requestKey: "req",
    frameSource: "existing",
    state: "video_pending",
    segmentRevision: 2,
    revision: 5,
    confirmedVersion: 1,
    videoTaskId: "second",
    modelKey: "model",
    adoptIfEmpty: true,
    errorCode: "",
    errorMessage: "",
    createdAt: "2026-11-17T10:01:00Z",
    updatedAt: "2026-11-17T10:02:00Z",
  };
  const lateFrameSnapshot = {
    ...running,
    frameRun: videoPending,
    takes: running.takes.slice(0, 1), // The unresolved I2V task affects status but is not a take.
  };
  const bound = { ...videoPending, state: "bound" as const, revision: 6 };
  const done = {
    ...snapshot("done", {
      ...candidate,
      status: "succeeded",
      asset: { id: 2, kind: "video", url: "/second.mp4" },
      origin: "i2v",
      run_id: bound.id,
    }),
    frameRun: bound,
  };

  for (const path of ["apply", "setList"] as const) {
    test(`${path}: a late video_pending run cannot roll back a bound clip with two succeeded takes`, () => {
      const store = useDramaSegmentsStore.getState();
      store.setList("d1", 1, [done]);
      expect(receive(path, lateFrameSnapshot)).toBe(true);
      expect(clip()).toMatchObject({ status: "done", frameRun: bound, takes: done.takes });

      const native = { ...running.takes[1]!, taskId: "native", createdAt: "2026-11-17T10:04:00Z" };
      expect(
        receive(path, { ...lateFrameSnapshot, takes: [...lateFrameSnapshot.takes, native] }),
      ).toBe(true);
      expect(clip()).toMatchObject({
        status: "running",
        frameRun: bound,
        takes: [...done.takes, native],
      });
    });

    test(`${path}: newer unresolved video_pending still accounts for running status`, () => {
      const store = useDramaSegmentsStore.getState();
      const pending = { ...lateFrameSnapshot, frameRun: { ...videoPending, revision: 7 } };
      store.setList("d1", 1, [pending]);
      expect(receive(path, { ...pending, status: "done", frameRun: bound })).toBe(true);
      expect(clip()).toMatchObject({ status: "running", frameRun: pending.frameRun });
    });
  }

  test.each([1, 2])(
    "list row rev%i keeps a newer unresolved video running behind an adopted take",
    (revision) => {
      const store = useDramaSegmentsStore.getState();
      const previous = {
        ...lateFrameSnapshot,
        status: "done" as const,
        frameRun: { ...videoPending, state: "confirmed" as const, revision: 4 },
      };
      store.setList("d1", 1, [previous]);
      const newerRun = { ...videoPending, revision: 6 };
      expect(receive("setList", { ...previous, revision, frameRun: newerRun })).toBe(true);
      expect(clip()).toMatchObject({ revision: 2, status: "running", frameRun: newerRun });

      expect(receive("setList", previous)).toBe(true);
      expect(clip()).toMatchObject({ status: "running", frameRun: newerRun });
    },
  );

  test.each([
    ["succeeded", "done"],
    ["failed", "error"],
  ] as const)("an older list row cannot restart the same %s video task", (taskStatus, status) => {
    const store = useDramaSegmentsStore.getState();
    const resolved = {
      ...done,
      status,
      takes: [done.takes[0]!, { ...done.takes[1]!, status: taskStatus }],
    };
    store.setList("d1", 1, [resolved]);
    expect(
      receive("setList", {
        ...lateFrameSnapshot,
        revision: resolved.revision - 1,
        frameRun: { ...videoPending, revision: bound.revision + 1 },
      }),
    ).toBe(true);
    expect(clip()).toMatchObject({ status, takes: resolved.takes });
  });

  test.each([
    ["newer same run", { ...videoPending, revision: 6 }, "", 2, "incoming"],
    ["newer same run on an older segment row", { ...videoPending, revision: 6 }, "", 1, "incoming"],
    ["older same run", { ...videoPending, revision: 3 }, "", 2, "local"],
    ["equal same run", { ...videoPending, revision: 4 }, "", 2, "local"],
    ["newer different run", { ...videoPending, id: "other" }, "", 2, "incoming"],
    ["newer different run on an older row", { ...videoPending, id: "other" }, "", 1, "incoming"],
    [
      "incomparable different run",
      { ...videoPending, id: "other", updatedAt: "", createdAt: "" },
      "",
      2,
      "local",
    ],
    ["incomparable null", null, "", 2, "local"],
    ["older null", null, "2026-11-17T09:59:00Z", 2, "local"],
    ["newer cleared run", null, "2026-11-17T10:03:00Z", 3, "incoming"],
  ] as const)("an in-flight list reconciles %s", (_name, frameRun, updatedAt, revision, winner) => {
    const store = useDramaSegmentsStore.getState();
    const initial = { ...lateFrameSnapshot, revision: 2, frameRun: null, status: "done" as const };
    store.setList("d1", 1, [initial]);
    const startFloors = store.runFloors;
    const confirmed = {
      ...videoPending,
      state: "confirmed" as const,
      revision: 4,
      updatedAt: "2026-11-17T10:01:00Z",
    };
    store.bumpRunFloor(initial.id);
    store.apply("d1", 1, { ...initial, frameRun: confirmed, prompt: "keep current edit" });
    expect(
      store.setList(
        "d1",
        1,
        [
          {
            ...initial,
            frameRun,
            revision,
            updatedAt,
            status: frameRun ? "running" : "done",
          },
        ],
        startFloors,
      ),
    ).toBe(true);
    expect(clip()).toMatchObject({
      frameRun: winner === "incoming" ? frameRun : confirmed,
      status: winner === "incoming" && frameRun ? "running" : "done",
    });
    if (revision === 1) expect(clip()?.prompt).toBe("keep current edit");
  });

  test("an incomparable list does not hide a local pending video behind a succeeded take", () => {
    const store = useDramaSegmentsStore.getState();
    const beforeRun = { ...lateFrameSnapshot, frameRun: null, status: "done" as const };
    store.setList("d1", 1, [beforeRun]);
    const startFloors = store.runFloors;
    store.bumpRunFloor(beforeRun.id);
    store.apply("d1", 1, lateFrameSnapshot);
    expect(store.setList("d1", 1, [{ ...beforeRun, updatedAt: "" }], startFloors)).toBe(true);
    expect(clip()).toMatchObject({ status: "running", frameRun: videoPending });
  });

  for (const path of ["apply", "setList"] as const) {
    test(`${path}: a new candidate can make an adopted clip run and intermediate progress never goes backwards`, () => {
      const beforeCandidate = {
        ...running,
        takes: running.takes.slice(0, 1),
        status: "done" as const,
      };
      useDramaSegmentsStore.getState().setList("d1", 1, [beforeCandidate]);
      for (const status of ["pending", "queued", "running", "finalizing"] as const) {
        receive(path, snapshot("running", { ...candidate, status }));
        expect(clip()?.status).toBe("running");
        expect(clip()?.takes[1]?.status).toBe(status);
      }
      for (const delayed of [
        ...(["running", "queued", "pending"] as const).map((status) =>
          snapshot("running", { ...candidate, status }),
        ),
        beforeCandidate,
      ]) {
        receive(path, delayed);
        expect(clip()?.status).toBe("running");
        expect(clip()?.takes[1]?.status).toBe("finalizing");
      }
    });

    test(`${path}: a higher revision can clear QC and change the chosen take without losing the resolved asset`, () => {
      const rejected = snapshot("done", {
        ...candidate,
        status: "succeeded",
        asset: { id: 2, kind: "video", url: "/second.mp4" },
        qc: "rejected",
      });
      useDramaSegmentsStore.getState().setList("d1", 1, [rejected]);
      const edited = snapshot("running", { ...candidate, status: "running", qc: "unreviewed" });
      receive(path, { ...edited, chosenTaskId: "second", revision: 3, segmentVersion: 6 });
      expect(clip()).toMatchObject({
        status: "done",
        chosenTaskId: "second",
        revision: 3,
        takes: [
          { taskId: "adopted" },
          {
            taskId: "second",
            status: "succeeded",
            qc: "unreviewed",
            asset: { url: "/second.mp4" },
          },
        ],
      });
    });
  }
});

test("a full list prevents reviving a deleted clip, while a newer row event alone does not", () => {
  const store = useDramaSegmentsStore.getState();
  const a = { ...handleShape("a", 0), episodeId: "ep", segmentVersion: 1 };
  const b = { ...a, id: "b", seq: 1, segmentVersion: 2 };
  store.setList("d1", 1, [a]);
  const updatedA = { ...a, revision: 2, segmentVersion: 3 };
  store.apply("d1", 1, updatedA);
  expect(store.apply("d1", 1, b)).toBe(true);
  expect(store.setList("d1", 1, [updatedA])).toBe(true);
  store.apply("d1", 1, { ...updatedA, revision: 3, segmentVersion: 4 });
  expect(store.apply("d1", 1, b)).toBe(false);
  expect(useDramaSegmentsStore.getState().byEpisode[segmentKey("d1", 1)]?.map((s) => s.id)).toEqual(
    ["a"],
  );
});

test("optimistic reorder after a v3 row event still accepts a v2 clip created after the v1 list", () => {
  const store = useDramaSegmentsStore.getState();
  const a = { ...handleShape("a", 0), episodeId: "ep", segmentVersion: 1 };
  const b = { ...a, id: "b", seq: 1 };
  store.setList("d1", 1, [a, b]);
  store.apply("d1", 1, { ...b, revision: 2, prompt: "edited", segmentVersion: 3 });
  expect(store.reorderLocal("d1", 1, ["b", "a"], "other-ep")).toBe(false);
  expect(store.reorderLocal("d1", 1, ["b", "a"], "ep")).toBe(true);
  expect(store.snapshotOf("d1", 1)?.segmentVersion).toBe(3);
  expect(store.apply("d1", 1, { ...a, id: "c", seq: 2, segmentVersion: 2 })).toBe(true);
  expect(
    useDramaSegmentsStore.getState().byEpisode[segmentKey("d1", 1)]?.map((item) => item.id),
  ).toEqual(["b", "a", "c"]);
});

describe("confirmed clip deletion", () => {
  const key = segmentKey("d1", 1);
  const last = { ...handleShape("last", 0), revision: 4, episodeId: "ep", segmentVersion: 5 };
  const unversioned = { ...last, segmentVersion: undefined };

  test("a late WS event cannot revive the last clip, even with no next row to publish", () => {
    const store = useDramaSegmentsStore.getState();
    store.setList("d1", 1, [last]);
    store.remove("d1", 1, last.id, "ep");
    handleDramaSegmentEvent({
      drama_id: "d1",
      episode_no: 1,
      segment: raw({ id: last.id, revision: last.revision, episode_id: "ep", segment_version: 5 }),
    });
    expect(store.apply("d1", 1, { ...last, segmentVersion: 4 })).toBe(false);
    expect(store.apply("d1", 1, unversioned)).toBe(false);
    expect(store.apply("d1", 1, { ...unversioned, revision: 3 })).toBe(false);
    expect(useDramaSegmentsStore.getState().byEpisode[key]).toEqual([]);
    expect(store.snapshotOf("d1", 1)).toEqual({ episodeId: "ep", segmentVersion: 5 });
  });

  test("an equal-version or unversioned full list cannot undo the last deletion; newer empty list confirms it", async () => {
    const store = useDramaSegmentsStore.getState();
    store.setList("d1", 1, [last]);
    let finish!: (value: unknown) => void;
    const get = spyOn(service, "get").mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          finish = resolve;
        }),
    );
    try {
      const delayed = reloadSegments("d1", 1, "ep");
      store.remove("d1", 1, last.id, "ep");
      finish({
        episode_id: "ep",
        segment_version: 5,
        items: [
          raw({ id: last.id, revision: last.revision, episode_id: "ep", segment_version: 5 }),
        ],
      });
      expect(await delayed).toBe(true);
      expect(store.setList("d1", 1, [unversioned])).toBe(true);
      expect(store.setList("d1", 1, [{ ...last, segmentVersion: 4 }])).toBe(false);
      expect(store.apply("d1", 1, last)).toBe(false);
      expect(useDramaSegmentsStore.getState().byEpisode[key]).toEqual([]);
      get.mockResolvedValueOnce({ episode_id: "ep", segment_version: 6, items: [] });
      expect(await reloadSegments("d1", 1, "ep")).toBe(true);
      expect(store.apply("d1", 1, last)).toBe(false);
      expect(store.snapshotOf("d1", 1)?.segmentVersion).toBe(6);
      expect(useDramaSegmentsStore.getState().byEpisode[key]).toEqual([]);
    } finally {
      get.mockRestore();
    }
  });

  test("a pre-delete list still reconciles surviving clips, and a new array resets deletion metadata", () => {
    const store = useDramaSegmentsStore.getState();
    const survivor = { ...last, id: "survivor", seq: 1 };
    store.setList("d1", 1, [last, survivor]);
    store.remove("d1", 1, last.id, "other-ep");
    expect(useDramaSegmentsStore.getState().byEpisode[key]).toEqual([last, survivor]);
    store.remove("d1", 1, last.id, "ep");
    expect(store.setList("d1", 1, [last, { ...survivor, prompt: "new" }])).toBe(true);
    expect(useDramaSegmentsStore.getState().byEpisode[key]).toEqual([
      { ...survivor, prompt: "new" },
    ]);
    useDramaSegmentsStore.setState({ byEpisode: { [key]: [] } });
    expect(store.apply("d1", 1, last)).toBe(true);
    expect(useDramaSegmentsStore.getState().byEpisode[key]).toEqual([last]);
  });

  test("a pre-delete response at the known episode version cannot revive a clip cached at an older version", () => {
    const store = useDramaSegmentsStore.getState();
    const survivor = { ...last, id: "survivor", seq: 1 };
    const edited = { ...survivor, revision: 5, segmentVersion: 6 };
    store.setList("d1", 1, [last, survivor]);
    store.apply("d1", 1, edited);
    store.remove("d1", 1, last.id, "ep");
    const delayed = { ...last, segmentVersion: 6 };
    expect(store.apply("d1", 1, delayed)).toBe(false);
    expect(store.setList("d1", 1, [delayed, edited])).toBe(true);
    expect(useDramaSegmentsStore.getState().byEpisode[key]).toEqual([edited]);
  });

  for (const surviving of [false, true]) {
    for (const path of ["apply", "setList"] as const) {
      test(`${path}: a delayed v6 snapshot after DELETE cannot revive a v5 clip (${surviving ? "multi-clip" : "last clip"})`, () => {
        const store = useDramaSegmentsStore.getState();
        const survivor = { ...last, id: "survivor", seq: 1 };
        store.setList("d1", 1, [last, ...(surviving ? [survivor] : [])]);
        store.remove("d1", 1, last.id, "ep"); // DELETE returned 204; the client still knows v5.
        const delayed = { ...last, segmentVersion: 6, revision: 5 };
        const edited = { ...survivor, segmentVersion: 6, revision: 5, prompt: "edited" };

        if (path === "apply") {
          expect(store.apply("d1", 1, delayed)).toBe(false);
        } else {
          expect(store.setList("d1", 1, [delayed, ...(surviving ? [edited] : [])])).toBe(true);
        }
        const kept = surviving ? [path === "setList" ? edited : survivor] : [];
        expect(useDramaSegmentsStore.getState().byEpisode[key]).toEqual(kept);

        expect(
          store.setList("d1", 1, surviving ? [{ ...edited, segmentVersion: 7 }] : [], undefined, {
            episodeId: "ep",
            segmentVersion: 7,
          }),
        ).toBe(true);
        expect(store.apply("d1", 1, delayed)).toBe(false);
      });
    }
  }

  test("an unversioned higher row revision cannot revive a confirmed deletion", () => {
    const store = useDramaSegmentsStore.getState();
    store.setList("d1", 1, [unversioned], undefined, { episodeId: "ep", segmentVersion: 5 });
    store.remove("d1", 1, last.id, "ep");
    const newer = { ...unversioned, revision: 5 };
    expect(store.apply("d1", 1, newer)).toBe(false);
    expect(store.setList("d1", 1, [newer])).toBe(true);
    expect(useDramaSegmentsStore.getState().byEpisode[key]).toEqual([]);
    store.forgetDrama("d1");
    expect(store.apply("d1", 1, newer)).toBe(true);
  });

  test("local reorder preserves deletion protection, while a different episode starts with fresh membership", () => {
    const store = useDramaSegmentsStore.getState();
    const a = { ...last, id: "a", seq: 1 };
    const b = { ...last, id: "b", seq: 2 };
    store.setList("d1", 1, [last, a, b]);
    store.remove("d1", 1, last.id, "ep");
    expect(store.reorderLocal("d1", 1, ["b", "a", last.id], "ep")).toBe(true);
    expect(store.apply("d1", 1, last)).toBe(false);
    expect(useDramaSegmentsStore.getState().byEpisode[key]?.map((item) => item.id)).toEqual([
      "b",
      "a",
    ]);
    store.activateEpisode("d1", 1, "new-ep");
    const replacement = { ...last, episodeId: "new-ep", segmentVersion: 1, revision: 1 };
    expect(store.apply("d1", 1, replacement)).toBe(true);
    expect(useDramaSegmentsStore.getState().byEpisode[key]).toEqual([replacement]);
  });
});

test("confirmed clear blocks old lists and events without touching another episode", () => {
  const store = useDramaSegmentsStore.getState();
  const old = { ...handleShape("old", 0), episodeId: "ep-a", segmentVersion: 5 };
  const sibling = { ...old, id: "sibling", episodeId: "ep-b", episodeNo: 2 };
  store.setList("d1", 1, [old]);
  store.setList("d1", 2, [sibling]);
  store.applyConfirmedClear("d1", 1, "ep-a", 6, [old.id]);

  expect(store.snapshotOf("d1", 1)).toEqual({ episodeId: "ep-a", segmentVersion: 6 });
  expect(store.setList("d1", 1, [old])).toBe(false);
  expect(store.apply("d1", 1, { ...old, segmentVersion: undefined })).toBe(false);
  expect(useDramaSegmentsStore.getState().byEpisode).toEqual({ "d1:1": [], "d1:2": [sibling] });

  store.activateEpisode("d1", 1, "replacement");
  const replacement = {
    ...old,
    id: "replacement-clip",
    episodeId: "replacement",
    segmentVersion: 1,
  };
  store.setList("d1", 1, [replacement]);
  store.applyConfirmedClear("d1", 1, "ep-a", 6, [old.id]);
  expect(useDramaSegmentsStore.getState().byEpisode["d1:1"]).toEqual([replacement]);
});

test("a clear receipt that follows new storyboard rows preserves them", () => {
  const store = useDramaSegmentsStore.getState();
  const old = { ...handleShape("old", 0), episodeId: "ep", segmentVersion: 5 };
  const latest = { ...old, id: "new", segmentVersion: 7 };
  store.setList("d1", 1, [old]);
  store.apply("d1", 1, latest);
  store.applyConfirmedClear("d1", 1, "ep", 6, [old.id]);
  expect(useDramaSegmentsStore.getState().byEpisode["d1:1"]).toEqual([latest]);
  expect(store.snapshotOf("d1", 1)?.segmentVersion).toBe(7);
  expect(store.apply("d1", 1, { ...old, segmentVersion: undefined })).toBe(false);
});

test("a late list, event and mutation cannot repopulate a replaced episode", async () => {
  const store = useDramaSegmentsStore.getState();
  store.setList("d1", 1, [{ ...handleShape("old", 0), episodeId: "ep-a", segmentVersion: 9 }]);
  let finish!: (value: unknown) => void;
  const get = spyOn(service, "get").mockImplementationOnce(
    () =>
      new Promise((resolve) => {
        finish = resolve;
      }),
  );
  try {
    const late = reloadSegments("d1", 1, "ep-a");
    get.mockResolvedValueOnce({
      episode_id: "ep-b",
      segment_version: 1,
      items: [raw({ id: "new", episode_id: "ep-b" })],
    });
    expect(await reloadSegments("d1", 1, "ep-b", { activate: true })).toBe(true);
    finish({
      episode_id: "ep-a",
      segment_version: 9,
      items: [raw({ id: "old", episode_id: "ep-a" })],
    });
    expect(await late).toBe(false);
    handleDramaSegmentEvent({
      drama_id: "d1",
      episode_no: 1,
      segment: raw({ id: "old", episode_id: "ep-a", revision: 10 }),
    });
    store.apply("d1", 1, { ...handleShape("old", 0), episodeId: "ep-a" });
    expect(store.setList("d1", 1, [])).toBe(false);
    expect(
      store.setList("d1", 1, [handleShape("old", 0)], undefined, {
        episodeId: "ep-b",
        segmentVersion: 2,
        expectedEpisodeId: "ep-a",
      }),
    ).toBe(false);
    expect(
      useDramaSegmentsStore.getState().byEpisode[segmentKey("d1", 1)]?.map((item) => item.id),
    ).toEqual(["new"]);
    expect(store.snapshotOf("d1", 1)).toEqual({ episodeId: "ep-b", segmentVersion: 1 });
  } finally {
    get.mockRestore();
  }
});

test("a full list owns membership but cannot roll back a row revision or erase the version floor", () => {
  const store = useDramaSegmentsStore.getState();
  const a = { ...handleShape("a", 0), episodeId: "ep", segmentVersion: 3 };
  const b = { ...a, id: "b", seq: 1 };
  store.setList("d1", 1, [a, b]);
  store.apply("d1", 1, { ...a, revision: 2, prompt: "latest edit" });
  expect(store.setList("d1", 1, [a])).toBe(true);
  expect(useDramaSegmentsStore.getState().byEpisode[segmentKey("d1", 1)]).toEqual([
    { ...a, revision: 2, prompt: "latest edit" },
  ]);
  expect(store.setList("d1", 1, [{ ...a, segmentVersion: 4 }])).toBe(true);
  expect(useDramaSegmentsStore.getState().byEpisode[segmentKey("d1", 1)]).toEqual([
    { ...a, revision: 2, prompt: "latest edit", segmentVersion: 4 },
  ]);
  expect(store.setList("d1", 1, [])).toBe(false);
  expect(store.setList("d1", 1, [{ ...a, segmentVersion: 2 }])).toBe(false);
  store.clearEpisode("d1", 1, "ep");
  expect(useDramaSegmentsStore.getState().byEpisode[segmentKey("d1", 1)]).toEqual([]);
  expect(store.snapshotOf("d1", 1)?.segmentVersion).toBe(4);
});

describe("WebSocket drama.segment", () => {
  test("消息解析：合法的通过，缺字段的丢弃", () => {
    const ok = parseServerMessage(
      JSON.stringify({
        type: "drama.segment",
        data: { drama_id: "d1", episode_no: 1, segment: raw() },
      }),
    );
    expect(ok?.type).toBe("drama.segment");
    expect(
      parseServerMessage(
        JSON.stringify({ type: "drama.segment", data: { drama_id: "d1", episode_no: 1 } }),
      ),
    ).toBeNull();
    expect(
      parseServerMessage(
        JSON.stringify({
          type: "drama.segment",
          data: { drama_id: 1, episode_no: 1, segment: raw() },
        }),
      ),
    ).toBeNull();
  });

  test("事件处理：映射后并进对应短剧的对应集", () => {
    handleDramaSegmentEvent({
      drama_id: "d9",
      episode_no: 2,
      segment: raw({ episode_no: 2, status: "running", prompt: "来自推送" }),
    });
    const list = useDramaSegmentsStore.getState().byEpisode[segmentKey("d9", 2)]!;
    expect(list[0]).toMatchObject({ id: "s1", status: "running", prompt: "来自推送" });
  });

  test("片段生成完成立即刷新分集摘要，重复或过时推送不重复刷新", () => {
    handleDramaSegmentEvent({ drama_id: "d1", episode_no: 1, segment: raw({ status: "running" }) });
    let refreshes = 0;
    const off = subscribeDramaChanges("d1", () => {
      refreshes++;
    });
    try {
      const done = {
        drama_id: "d1",
        episode_no: 1,
        segment: raw({ status: "done", chosen_task_id: "t1", revision: 2 }),
      };
      handleDramaSegmentEvent(done);
      expect(refreshes).toBe(1);
      handleDramaSegmentEvent(done);
      handleDramaSegmentEvent({ ...done, segment: raw() });
      expect(refreshes).toBe(1);
    } finally {
      off();
    }
  });

  test("断线对账补到完成视频时同步刷新分集摘要", async () => {
    const event = { drama_id: "d1", episode_no: 1, segment: raw({ status: "running" }) };
    handleDramaSegmentEvent(event);
    const get = spyOn(service, "get").mockResolvedValue({
      items: [raw({ status: "done", chosen_task_id: "t1", revision: 2 })],
    });
    let refreshes = 0;
    const off = subscribeDramaChanges("d1", () => {
      refreshes++;
    });
    useDramaSegmentsStore.getState().watch("d1", 1);
    try {
      await reconcileWatchedSegments();
      expect(refreshes).toBe(1);
      await reconcileWatchedSegments();
      expect(refreshes).toBe(1);
    } finally {
      off();
      get.mockRestore();
    }
  });

  test("对账：只重新拉正在看的那几集，请求失败不抛", async () => {
    const get = spyOn(service, "get").mockResolvedValue({ items: [raw({ prompt: "对账来的" })] });
    useDramaSegmentsStore.getState().watch("d1", 3);
    await reconcileWatchedSegments();
    expect(get.mock.calls[0][0]).toBe("/dramas/d1/episodes/3/segments");
    expect(useDramaSegmentsStore.getState().byEpisode[segmentKey("d1", 3)]![0].prompt).toBe(
      "对账来的",
    );
    get.mockRejectedValue(new Error("offline"));
    await reconcileWatchedSegments();
    get.mockRestore();
  });
});

function handleShape(id: string, seq: number) {
  return {
    id,
    episodeNo: 1,
    seq,
    seconds: 10,
    prompt: "p",
    takes: [],
    chosenTaskId: "",
    status: "idle" as const,
    revision: 1,
    updatedAt: "x",
  };
}
