import { afterEach, beforeEach, describe, expect, spyOn, test } from "bun:test";
import { toast } from "sonner";

import { DRAMA_ERR, IDEMPOTENCY_CONFLICT_CODE } from "@/api/drama/errors";
import type { BackendJobView, BackendManifestResponse, DramaManifestView } from "@/api/drama/type";
import { useCompose } from "@/pages/drama/use-compose";
import { useDramaJobsStore } from "@/store/drama";
import { waitForEpisodeManifest } from "@/utils/drama/manifest";
import { ApiError } from "@/utils/requests/request";
import service from "@/utils/requests/service";

import { installDom, type DomEnv } from "../../helpers/dom";

const rawManifest = (version = 3, hash = "a".repeat(64)): BackendManifestResponse => ({
  manifest: {
    v: 1,
    fps: 24,
    total_frames: 120,
    clips: [],
    program: { v: 1, audio: [], overlays: [] },
  },
  manifest_hash: hash,
  segment_version: version,
  skipped_seqs: [],
  diagnostics: [],
});

const manifestView = (version = 3, hash = "a".repeat(64)): DramaManifestView => ({
  manifest: {
    v: 1,
    fps: 24,
    totalFrames: 120,
    clips: [],
    program: { v: 1, audio: [], overlays: [] },
  },
  manifestHash: hash,
  segmentVersion: version,
  skippedSeqs: [],
  diagnostics: [],
});

const backendJob = (
  id = "j-1",
  status: BackendJobView["status"] = "queued",
  ep = 1,
): BackendJobView => ({
  id,
  step: "compose",
  episodes: [ep],
  status,
  progress: status === "succeeded" ? 100 : 10,
  credits: 0,
  error_code: 0,
  error_message: "",
  created_at: "2026-11-15T10:00:00Z",
  finished_at: status === "succeeded" ? "2026-11-15T10:01:00Z" : null,
  result: null,
});

const deferred = <T,>() => {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
};

describe("useCompose 幂等键保留与竞态防护", () => {
  let dom: DomEnv;
  let spies: Array<{ mockRestore: () => void }> = [];

  beforeEach(async () => {
    dom = await installDom();
    useDramaJobsStore.setState({ byDrama: {}, settled: 0 });
    spies = [
      spyOn(toast, "error").mockImplementation(() => ""),
      spyOn(toast, "warning").mockImplementation(() => ""),
    ];
  });

  afterEach(() => {
    for (const s of spies) s.mockRestore();
    spies = [];
    useDramaJobsStore.setState({ byDrama: {}, settled: 0 });
    dom.restore();
  });

  const mountHook = async (initialDramaId = "d-1") => {
    const stateRef: {
      latest?: ReturnType<typeof useCompose>;
      setDrama?: (id: string) => void;
    } = {};
    const React = await import("react");

    function Harness() {
      const [id, setId] = React.useState(initialDramaId);
      const api = useCompose(id);
      React.useEffect(() => {
        stateRef.setDrama = setId;
      }, []);
      React.useEffect(() => {
        stateRef.latest = api;
      }, [api]);
      return null;
    }

    const mounted = await dom.mount(React.createElement(Harness));
    return {
      get: () => stateRef.latest!,
      setDramaId: async (next: string) => {
        await dom.act(async () => {
          stateRef.setDrama?.(next);
        });
      },
      unmount: mounted.unmount,
    };
  };

  test("瞬时错误（503 / 429 / 408 / status=0 / 网络异常）重用同一 composeRequestKey；成功后轮换新键", async () => {
    const post = spyOn(service, "post")
      .mockRejectedValueOnce(new ApiError("upstream unavailable", 503, 503))
      .mockRejectedValueOnce(new ApiError("rate limited", 429, 429))
      .mockRejectedValueOnce(new ApiError("timeout", 408, 408))
      .mockRejectedValueOnce(new ApiError("disconnected", 0, 0))
      .mockRejectedValueOnce(new Error("Network Error"))
      .mockResolvedValueOnce(backendJob("j-ok"))
      .mockResolvedValueOnce(backendJob("j-ok-2"));
    spies.push(post);

    const h = await mountHook();
    const m = manifestView(3, "a".repeat(64));

    for (let i = 0; i < 5; i++) {
      let ok = true;
      await dom.act(async () => {
        ok = await h
          .get()
          .compose(1, { useManifest: true, size: "720p", variants: ["clean"], manifest: m });
      });
      expect(ok).toBe(false);
    }
    let ok = false;
    await dom.act(async () => {
      ok = await h
        .get()
        .compose(1, { useManifest: true, size: "720p", variants: ["clean"], manifest: m });
    });
    expect(ok).toBe(true);

    const keysDuringRetries = post.mock.calls
      .slice(0, 6)
      .map((c) => (c[1] as { compose_request_key: string }).compose_request_key);
    expect(new Set(keysDuringRetries).size).toBe(1);

    // 成功后再次提交同一参数 -> 生成新的 composeRequestKey
    await dom.act(async () => {
      ok = await h
        .get()
        .compose(1, { useManifest: true, size: "720p", variants: ["clean"], manifest: m });
    });
    expect(ok).toBe(true);
    const keyAfterSuccess = (post.mock.calls[6][1] as { compose_request_key: string })
      .compose_request_key;
    expect(keyAfterSuccess).not.toBe(keysDuringRetries[0]);

    await h.unmount();
  });

  test("未传 options.manifest 时首次自动拉取，失败重试传入已加载 manifest 仍复用同一 key", async () => {
    const get = spyOn(service, "get").mockResolvedValue(rawManifest(4, "b".repeat(64)));
    const post = spyOn(service, "post")
      .mockRejectedValueOnce(new ApiError("bad gateway", 502, 502))
      .mockResolvedValueOnce(backendJob("j-retry-ok"));
    spies.push(get, post);

    const h = await mountHook();
    await dom.act(async () => {
      await h
        .get()
        .compose(1, { useManifest: true, size: "1080p", variants: ["clean", "packaged"] });
    });
    const loadedManifest = h.get().manifests[1]?.data;
    expect(loadedManifest?.manifestHash).toBe("b".repeat(64));

    await dom.act(async () => {
      await h.get().compose(1, {
        useManifest: true,
        size: "1080p",
        variants: ["clean", "packaged"],
        manifest: loadedManifest,
      });
    });

    expect(post).toHaveBeenCalledTimes(2);
    const k1 = (post.mock.calls[0][1] as { compose_request_key: string }).compose_request_key;
    const k2 = (post.mock.calls[1][1] as { compose_request_key: string }).compose_request_key;
    expect(k2).toBe(k1);

    await h.unmount();
  });

  test("同意图在途并发提交复用单次在途请求，失败后重试仍保留同一 key", async () => {
    const d1 = deferred<BackendJobView>();
    const post = spyOn(service, "post")
      .mockReturnValueOnce(d1.promise)
      .mockResolvedValueOnce(backendJob("j-2"));
    spies.push(post);

    const h = await mountHook();
    const m = manifestView(3, "c".repeat(64));

    let p1!: Promise<boolean>;
    let p2!: Promise<boolean>;
    await dom.act(async () => {
      p1 = h
        .get()
        .compose(1, { useManifest: true, size: "720p", variants: ["clean"], manifest: m });
      p2 = h
        .get()
        .compose(1, { useManifest: true, size: "720p", variants: ["clean"], manifest: m });
    });
    expect(h.get().pending).toBe(1);
    expect(post).toHaveBeenCalledTimes(1);

    await dom.act(async () => {
      d1.reject(new ApiError("timeout", 504, 504));
      expect(await Promise.all([p1, p2])).toEqual([false, false]);
    });
    expect(h.get().pending).toBeNull();

    await dom.act(async () => {
      expect(
        await h
          .get()
          .compose(1, { useManifest: true, size: "720p", variants: ["clean"], manifest: m }),
      ).toBe(true);
    });
    expect(post).toHaveBeenCalledTimes(2);
    const k1 = (post.mock.calls[0][1] as { compose_request_key: string }).compose_request_key;
    const k2 = (post.mock.calls[1][1] as { compose_request_key: string }).compose_request_key;
    expect(k2).toBe(k1);

    await h.unmount();
  });

  test("91001（manifestStale）与 90001（幂等冲突）和非重试 4xx 会清理旧 key，下次使用新 key", async () => {
    const get = spyOn(service, "get").mockResolvedValue(rawManifest(9, "f".repeat(64)));
    const post = spyOn(service, "post")
      // 91001 后轮换
      .mockRejectedValueOnce(new ApiError("stale", DRAMA_ERR.manifestStale, 409))
      // 90001 后轮换
      .mockRejectedValueOnce(new ApiError("idem", IDEMPOTENCY_CONFLICT_CODE, 409))
      // 400 业务拒绝后轮换
      .mockRejectedValueOnce(new ApiError("empty", DRAMA_ERR.nothingToCompose, 400))
      .mockResolvedValueOnce(backendJob("j-final"));
    spies.push(get, post);

    const h = await mountHook();
    const m1 = manifestView(3, "a".repeat(64));

    await dom.act(async () => {
      await h
        .get()
        .compose(1, { useManifest: true, size: "720p", variants: ["clean"], manifest: m1 });
    });
    expect(h.get().manifests[1]?.stale).toBe(true);
    expect(h.get().manifests[1]?.data?.segmentVersion).toBe(9);

    const m2 = h.get().manifests[1]!.data!;
    await dom.act(async () => {
      await h
        .get()
        .compose(1, { useManifest: true, size: "720p", variants: ["clean"], manifest: m2 });
    });
    await dom.act(async () => {
      await h
        .get()
        .compose(1, { useManifest: true, size: "720p", variants: ["clean"], manifest: m2 });
    });
    await dom.act(async () => {
      await h
        .get()
        .compose(1, { useManifest: true, size: "720p", variants: ["clean"], manifest: m2 });
    });

    const keys = post.mock.calls.map(
      (c) => (c[1] as { compose_request_key: string }).compose_request_key,
    );
    expect(new Set(keys).size).toBe(4);

    await h.unmount();
  });

  test("切换 size / variants / manifestHash / 集号 / 传统模式 / dramaId 时轮换 key", async () => {
    const post = spyOn(service, "post").mockRejectedValue(new ApiError("503", 503, 503));
    spies.push(post);

    const h = await mountHook("d-1");
    const m1 = manifestView(1, "a".repeat(64));
    const m2 = manifestView(2, "b".repeat(64));

    const calls: Array<() => Promise<boolean>> = [
      () =>
        h.get().compose(1, { useManifest: true, size: "720p", variants: ["clean"], manifest: m1 }),
      () =>
        h.get().compose(1, { useManifest: true, size: "1080p", variants: ["clean"], manifest: m1 }),
      () =>
        h.get().compose(1, {
          useManifest: true,
          size: "1080p",
          variants: ["clean", "packaged"],
          manifest: m1,
        }),
      () =>
        h.get().compose(1, {
          useManifest: true,
          size: "1080p",
          variants: ["clean", "packaged"],
          manifest: m2,
        }),
      () => h.get().compose(1, { useManifest: false }),
      () => h.get().compose(2, { useManifest: false }),
    ];

    for (const run of calls) {
      await dom.act(async () => {
        await run();
      });
    }

    await h.setDramaId("d-2");
    await dom.act(async () => {
      await h.get().compose(2, { useManifest: false });
    });

    const keys = post.mock.calls.map(
      (c) => (c[1] as { compose_request_key: string }).compose_request_key,
    );
    expect(new Set(keys).size).toBe(7);

    await h.unmount();
  });

  test("store 收到新建并已结算的 compose 任务时清理保留键；旧终态任务不误清", async () => {
    const post = spyOn(service, "post")
      .mockRejectedValueOnce(new ApiError("504", 504, 504))
      .mockRejectedValueOnce(new ApiError("504", 504, 504))
      .mockResolvedValueOnce(backendJob("j-new"));
    spies.push(post);

    useDramaJobsStore.getState().applyJob("d-1", {
      id: "j-pre",
      step: "compose",
      episodes: [1],
      status: "succeeded",
      progress: 100,
      credits: 0,
      errorCode: 0,
      errorMessage: "",
      createdAt: "2026-11-15T09:00:00Z",
      finishedAt: "2026-11-15T09:01:00Z",
      result: null,
    });

    const h = await mountHook("d-1");
    await dom.act(async () => {
      await h.get().compose(1, { useManifest: false });
    });
    // 重试仍复用旧键
    await dom.act(async () => {
      await h.get().compose(1, { useManifest: false });
    });

    const k1 = (post.mock.calls[0][1] as { compose_request_key: string }).compose_request_key;
    const k2 = (post.mock.calls[1][1] as { compose_request_key: string }).compose_request_key;
    expect(k2).toBe(k1);

    // 外部 WS 推送该集的终态合成任务 -> 清理已保留的幂等键
    await dom.act(async () => {
      useDramaJobsStore.getState().applyJob("d-1", {
        id: "j-ws-done",
        step: "compose",
        episodes: [1],
        status: "succeeded",
        progress: 100,
        credits: 0,
        errorCode: 0,
        errorMessage: "",
        createdAt: "2026-11-15T10:00:00Z",
        finishedAt: "2026-11-15T10:02:00Z",
        result: null,
      });
    });

    await dom.act(async () => {
      await h.get().compose(1, { useManifest: false });
    });
    const k3 = (post.mock.calls[2][1] as { compose_request_key: string }).compose_request_key;
    expect(k3).not.toBe(k1);

    await h.unmount();
  });

  test("loadManifest 后发先至时，旧请求响应不会覆盖新请求结果", async () => {
    const slow = deferred<BackendManifestResponse>();
    const fast = deferred<BackendManifestResponse>();
    const get = spyOn(service, "get")
      .mockReturnValueOnce(slow.promise)
      .mockReturnValueOnce(fast.promise);
    spies.push(get);

    const h = await mountHook("d-1");
    let pSlow!: Promise<DramaManifestView | null>;
    let pFast!: Promise<DramaManifestView | null>;
    await dom.act(async () => {
      pSlow = h.get().loadManifest(1);
      pFast = h.get().loadManifest(1);
    });

    await dom.act(async () => {
      fast.resolve(rawManifest(2, "2".repeat(64)));
      await pFast;
    });
    expect(h.get().manifests[1]?.loading).toBe(false);
    expect(h.get().manifests[1]?.data?.segmentVersion).toBe(2);

    await dom.act(async () => {
      slow.resolve(rawManifest(1, "1".repeat(64)));
      expect(await pSlow).toBeNull();
    });
    expect(h.get().manifests[1]?.data?.segmentVersion).toBe(2);
    expect(h.get().manifests[1]?.data?.manifestHash).toBe("2".repeat(64));

    await h.unmount();
  });

  test("submitJob 在途期间若 loadManifest 已刷新到新版清单，提交完成不会回写旧清单", async () => {
    const m1 = manifestView(1, "1".repeat(64));
    const get = spyOn(service, "get").mockResolvedValueOnce(rawManifest(2, "2".repeat(64)));
    const submitDeferred = deferred<BackendJobView>();
    const post = spyOn(service, "post").mockReturnValueOnce(submitDeferred.promise);
    spies.push(get, post);

    const h = await mountHook("d-1");
    let pSubmit!: Promise<boolean>;
    await dom.act(async () => {
      pSubmit = h
        .get()
        .compose(1, { useManifest: true, size: "720p", variants: ["clean"], manifest: m1 });
    });
    expect(post).toHaveBeenCalledTimes(1);

    await dom.act(async () => {
      await h.get().loadManifest(1);
    });
    expect(h.get().manifests[1]?.data?.segmentVersion).toBe(2);
    expect(h.get().manifests[1]?.data?.manifestHash).toBe("2".repeat(64));

    await dom.act(async () => {
      submitDeferred.resolve(backendJob("j-m3-direct", "queued", 1));
      expect(await pSubmit).toBe(true);
    });

    expect(h.get().manifests[1]?.data?.segmentVersion).toBe(2);
    expect(h.get().manifests[1]?.data?.manifestHash).toBe("2".repeat(64));

    await h.unmount();
  });

  test("compose 内部先拉取旧版清单再提交，在途期间 loadManifest 刷新后不会被回写覆盖", async () => {
    const get = spyOn(service, "get")
      .mockResolvedValueOnce(rawManifest(1, "1".repeat(64)))
      .mockResolvedValueOnce(rawManifest(2, "2".repeat(64)));
    const submitDeferred = deferred<BackendJobView>();
    const post = spyOn(service, "post").mockReturnValueOnce(submitDeferred.promise);
    spies.push(get, post);

    const h = await mountHook("d-1");
    let pSubmit!: Promise<boolean>;
    await dom.act(async () => {
      pSubmit = h.get().compose(1, { useManifest: true, size: "720p", variants: ["clean"] });
      await dom.flush(10);
    });
    expect(post).toHaveBeenCalledTimes(1);

    await dom.act(async () => {
      await h.get().loadManifest(1);
    });
    expect(h.get().manifests[1]?.data?.segmentVersion).toBe(2);

    await dom.act(async () => {
      submitDeferred.resolve(backendJob("j-m3-inline", "queued", 1));
      expect(await pSubmit).toBe(true);
    });

    expect(h.get().manifests[1]?.data?.segmentVersion).toBe(2);
    expect(h.get().manifests[1]?.data?.manifestHash).toBe("2".repeat(64));

    await h.unmount();
  });

  test("旧版合成保留瞬时失败幂等键，不发送 compose，且 ratio/版本/分镜素材或顺序变化时轮换 key", async () => {
    const post = spyOn(service, "post")
      .mockRejectedValueOnce(new ApiError("unavailable-1", 503, 503))
      .mockRejectedValueOnce(new ApiError("unavailable-2", 503, 503))
      .mockRejectedValueOnce(new ApiError("unavailable-3", 503, 503))
      .mockRejectedValueOnce(new ApiError("unavailable-4", 503, 503))
      .mockRejectedValueOnce(new ApiError("unavailable-5", 503, 503))
      .mockResolvedValueOnce(backendJob("j-legacy-ok", "queued", 1));
    spies.push(post);

    const h = await mountHook("d-1");
    const segA = {
      id: "seg-a",
      seq: 0,
      chosenTaskId: "task-a1",
      takes: [{ taskId: "task-a1", asset: { id: "asset-a1" } }],
    };
    const segB = {
      id: "seg-b",
      seq: 1,
      chosenTaskId: "task-b1",
      takes: [{ taskId: "task-b1", asset: { id: "asset-b1" } }],
    };
    const baseOpts = {
      useManifest: false,
      ratio: "9:16",
      segmentVersion: 1,
      segments: [segA, segB],
    };

    // 1. 首次 503 失败
    await dom.act(async () => {
      expect(await h.get().compose(1, baseOpts)).toBe(false);
    });
    // 2. 完全相同语义原样重试 -> 复用幂等键
    await dom.act(async () => {
      expect(await h.get().compose(1, baseOpts)).toBe(false);
    });
    // 3. ratio 变更 -> 轮换幂等键
    await dom.act(async () => {
      expect(await h.get().compose(1, { ...baseOpts, ratio: "16:9" })).toBe(false);
    });
    // 4. segmentVersion 变更 -> 轮换幂等键
    await dom.act(async () => {
      expect(await h.get().compose(1, { ...baseOpts, ratio: "16:9", segmentVersion: 2 })).toBe(
        false,
      );
    });
    // 5. 选中分镜素材变更 -> 轮换幂等键
    const segAUpdated = {
      id: "seg-a",
      seq: 0,
      chosenTaskId: "task-a2",
      takes: [{ taskId: "task-a2", asset: { id: "asset-a2" } }],
    };
    await dom.act(async () => {
      expect(
        await h.get().compose(1, {
          useManifest: false,
          ratio: "16:9",
          segmentVersion: 2,
          segments: [segAUpdated, segB],
        }),
      ).toBe(false);
    });
    // 6. 分镜顺序变更 -> 轮换幂等键并成功提交
    await dom.act(async () => {
      expect(
        await h.get().compose(1, {
          useManifest: false,
          ratio: "16:9",
          segmentVersion: 2,
          segments: [segB, segAUpdated],
        }),
      ).toBe(true);
    });

    expect(post).toHaveBeenCalledTimes(6);
    const payloads = post.mock.calls.map(
      (call) => call[1] as { compose_request_key: string; compose?: unknown },
    );
    for (const payload of payloads) {
      expect("compose" in payload).toBe(false);
      expect(typeof payload.compose_request_key).toBe("string");
    }

    const keys = payloads.map((p) => p.compose_request_key);
    expect(keys[1]).toBe(keys[0]);
    expect(new Set([keys[0], keys[2], keys[3], keys[4], keys[5]]).size).toBe(5);

    await h.unmount();
  });

  test('loadManifest 与 compose 对 status="preparing" 轮询退避等待就绪后再提交合成', async () => {
    let manifestMode: "preparing_then_ready" | "compose_preparing_then_ready" =
      "preparing_then_ready";
    let manifestGetCount = 0;

    spies.push(
      spyOn(service, "get").mockImplementation(async (url: string) => {
        if (url === "/dramas/d-1/episodes/1/manifest") {
          manifestGetCount += 1;
          if (manifestMode === "preparing_then_ready") {
            if (manifestGetCount <= 2) {
              return {
                status: "preparing",
                segment_version: 3,
                skipped_seqs: [],
                diagnostics: [],
              } as never;
            }
            return {
              status: "ready",
              ...rawManifest(3, "hash-cold-ready"),
            } as never;
          }
          if (manifestGetCount <= 1) {
            return {
              status: "preparing",
              segment_version: 4,
              skipped_seqs: [],
              diagnostics: [],
            } as never;
          }
          return {
            status: "ready",
            ...rawManifest(4, "hash-compose-ready"),
          } as never;
        }
        return {} as never;
      }),
    );

    const post = spyOn(service, "post").mockImplementation(
      async () => backendJob("j-cold") as never,
    );
    spies.push(post);

    const h = await mountHook("d-1");
    try {
      let loadPromise!: Promise<DramaManifestView | null>;
      await dom.act(async () => {
        loadPromise = h.get().loadManifest(1);
      });
      await dom.flush(30);
      expect(h.get().manifests[1]?.loading).toBe(true);
      expect(h.get().manifests[1]?.data).toBeNull();

      let loaded: DramaManifestView | null = null;
      await dom.act(async () => {
        loaded = await loadPromise;
      });
      expect(manifestGetCount).toBe(3);
      expect(loaded).not.toBeNull();
      expect(loaded!.status).toBe("ready");
      expect(loaded!.manifestHash).toBe("hash-cold-ready");
      expect(h.get().manifests[1]?.loading).toBe(false);
      expect(h.get().manifests[1]?.data?.manifestHash).toBe("hash-cold-ready");

      manifestMode = "compose_preparing_then_ready";
      manifestGetCount = 0;
      let ok = false;
      await dom.act(async () => {
        ok = await h.get().compose(1, { useManifest: true });
      });
      expect(ok).toBe(true);
      expect(manifestGetCount).toBe(2);
      expect(post).toHaveBeenCalledTimes(1);
      expect(post.mock.calls[0]?.[1]).toMatchObject({
        compose: {
          manifest_hash: "hash-compose-ready",
          segment_version: 4,
        },
      });
    } finally {
      await h.unmount();
    }
  });

  test("loadManifest / compose 在 preparing 轮询中卸载取消与超时处理不泄漏定时器", async () => {
    let manifestGetCount = 0;
    spies.push(
      spyOn(service, "get").mockImplementation(async () => {
        manifestGetCount += 1;
        return {
          status: "preparing",
          segment_version: 5,
          skipped_seqs: [],
          diagnostics: [],
        } as never;
      }),
    );
    const post = spyOn(service, "post").mockImplementation(
      async () => backendJob("j-never") as never,
    );
    spies.push(post);

    // 1) 卸载后立即停止 preparing 轮询，不遗留后台定时器
    const hUnmount = await mountHook("d-cancel");
    let cancelledPromise!: Promise<DramaManifestView | null>;
    await dom.act(async () => {
      cancelledPromise = hUnmount.get().loadManifest(1);
    });
    await dom.flush(30);
    const callsBeforeUnmount = manifestGetCount;
    await hUnmount.unmount();
    expect(await cancelledPromise).toBeNull();
    await dom.flush(600);
    expect(manifestGetCount).toBe(callsBeforeUnmount);

    // 2) preparing 超时不卡死在 loading 且不提交 compose
    let nowMs = 0;
    const nowSpy = spyOn(performance, "now").mockImplementation(() => nowMs);
    spies.push(nowSpy);

    const hTimeout = await mountHook("d-timeout");
    try {
      manifestGetCount = 0;
      let timedOutLoad!: Promise<DramaManifestView | null>;
      await dom.act(async () => {
        timedOutLoad = hTimeout.get().loadManifest(1);
      });
      await dom.flush(30);
      nowMs = 130_000;
      let loadRes: DramaManifestView | null = null;
      await dom.act(async () => {
        loadRes = await timedOutLoad;
      });
      expect(loadRes).toBeNull();
      expect(hTimeout.get().manifests[1]?.loading).toBe(false);
      expect(hTimeout.get().manifests[1]?.error).toBe("分镜清单准备超时，请稍后重试");

      nowMs = 0;
      let timedOutCompose!: Promise<boolean>;
      await dom.act(async () => {
        timedOutCompose = hTimeout.get().compose(1, { useManifest: true });
      });
      await dom.flush(30);
      nowMs = 130_000;
      let composeRes = true;
      await dom.act(async () => {
        composeRes = await timedOutCompose;
      });
      expect(composeRes).toBe(false);
      expect(post).not.toHaveBeenCalled();
      expect(toast.error).toHaveBeenCalledWith("分镜清单准备超时，请稍后重试");
    } finally {
      await hTimeout.unmount();
    }
  });

  test("waitForEpisodeManifest 严格执行 120s 绝对截止时间、拒绝过期 ready 并中止在途请求", async () => {
    // 1) 119.9s preparing -> 睡眠钳制后跨过 120s 截止线，不得再发第二次 GET
    let clockMs = 0;
    const recordedSleeps: number[] = [];
    let getCalls = 0;
    const sGet1 = spyOn(service, "get").mockImplementation(async () => {
      getCalls += 1;
      clockMs = 119_900;
      return {
        status: "preparing",
        segment_version: 5,
        skipped_seqs: [],
        diagnostics: [],
      } as never;
    });
    spies.push(sGet1);

    const outcome1 = await waitForEpisodeManifest("d-dl-1", 1, {
      maxWaitMs: 120_000,
      baseDelayMs: 500,
      nowMs: () => clockMs,
      sleep: async (ms) => {
        recordedSleeps.push(ms);
        clockMs += ms;
        return true;
      },
    });
    expect(outcome1).toEqual({ kind: "timeout" });
    expect(getCalls).toBe(1);
    expect(recordedSleeps).toEqual([100]);
    sGet1.mockRestore();

    // 2) 119.9s preparing -> 第二次 GET 跨过截止线到 210.1s 才返回 ready，必须判定 timeout 而非 ready
    clockMs = 0;
    let attempt = 0;
    const sGet2 = spyOn(service, "get").mockImplementation(async () => {
      attempt += 1;
      if (attempt === 1) {
        clockMs = 119_900;
        return {
          status: "preparing",
          segment_version: 5,
          skipped_seqs: [],
          diagnostics: [],
        } as never;
      }
      clockMs = 210_100;
      return {
        status: "ready",
        ...rawManifest(5, "d".repeat(64)),
      } as never;
    });
    spies.push(sGet2);

    const outcome2 = await waitForEpisodeManifest("d-dl-2", 1, {
      maxWaitMs: 120_000,
      baseDelayMs: 50,
      nowMs: () => clockMs,
      sleep: async (ms) => {
        clockMs += ms;
        return true;
      },
    });
    expect(outcome2).toEqual({ kind: "timeout" });
    expect(attempt).toBe(2);
    sGet2.mockRestore();

    // 3) 在途请求挂起时触发超时或外部 AbortSignal，必须 abort 底层 service.get signal
    let capturedSignals: AbortSignal[] = [];
    const sGet3 = spyOn(service, "get").mockImplementation(
      async (_url: string, _params?: unknown, config?: unknown) => {
        const sig = (config as { signal?: AbortSignal } | undefined)?.signal;
        if (sig) capturedSignals.push(sig);
        return await new Promise((_resolve, reject) => {
          sig?.addEventListener("abort", () => reject(new Error("aborted")), { once: true });
        });
      },
    );
    spies.push(sGet3);

    const timedOutHanging = await waitForEpisodeManifest("d-dl-3", 1, {
      maxWaitMs: 30,
    });
    expect(timedOutHanging).toEqual({ kind: "timeout" });
    expect(capturedSignals[0]?.aborted).toBe(true);

    capturedSignals = [];
    const callerAbort = new AbortController();
    const waitCancelled = waitForEpisodeManifest("d-dl-4", 1, {
      signal: callerAbort.signal,
      maxWaitMs: 5_000,
    });
    await dom.flush(15);
    expect(capturedSignals[0]?.aborted).toBe(false);
    callerAbort.abort();
    expect(await waitCancelled).toEqual({ kind: "cancelled" });
    expect(capturedSignals[0]?.aborted).toBe(true);
    sGet3.mockRestore();

    // 4) useCompose 卸载取消挂起 GET 请求的 signal 并返回 null
    let hookSignal: AbortSignal | undefined;
    spies.push(
      spyOn(service, "get").mockImplementation(
        async (_url: string, _params?: unknown, config?: unknown) => {
          hookSignal = (config as { signal?: AbortSignal } | undefined)?.signal;
          return await new Promise((_resolve, reject) => {
            hookSignal?.addEventListener("abort", () => reject(new Error("aborted")), {
              once: true,
            });
          });
        },
      ),
    );
    const hAbort = await mountHook("d-abort-hook");
    let pendingLoad!: Promise<DramaManifestView | null>;
    await dom.act(async () => {
      pendingLoad = hAbort.get().loadManifest(1);
    });
    await dom.flush(20);
    expect(hookSignal?.aborted).toBe(false);
    await hAbort.unmount();
    expect(await pendingLoad).toBeNull();
    expect(hookSignal?.aborted).toBe(true);
  });

  test("waitForEpisodeManifest 注入永不返回的自定义 sleep 时仍严格受 maxWaitMs 截止超时约束", async () => {
    let getCalls = 0;
    spies.push(
      spyOn(service, "get").mockImplementation(async () => {
        getCalls += 1;
        return {
          status: "preparing",
          segment_version: 5,
          skipped_seqs: [],
          diagnostics: [],
        } as never;
      }),
    );

    const abortController = new AbortController();
    let sleepCalled = false;
    const outcome = await Promise.race([
      waitForEpisodeManifest("d-hang-sleep", 1, {
        maxWaitMs: 20,
        baseDelayMs: 5,
        signal: abortController.signal,
        sleep: () => {
          sleepCalled = true;
          return new Promise<boolean>(() => {});
        },
      }),
      new Promise<"hung">((resolve) => {
        setTimeout(() => resolve("hung"), 250);
      }),
    ]);

    expect(sleepCalled).toBe(true);
    expect(getCalls).toBe(1);
    expect(outcome).toEqual({ kind: "timeout" });
  });
});
