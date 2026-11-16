import { afterEach, beforeEach, describe, expect, spyOn, test, type Mock } from "bun:test";

import {
  adaptSegment,
  addSegment,
  createEpisodePreview,
  DEFAULT_SEGMENT_EDIT,
  deleteEpisode,
  deleteSegment,
  generateSegment,
  getEpisodeManifest,
  getEpisodePreview,
  listSegments,
  mapEpisodeSummary,
  mapSegment,
  quoteSegment,
  reorderSegments,
  segmentEditOf,
  toJobBody,
  toSegmentUpdateBody,
  updateSegment,
} from "@/api/drama";
import {
  DRAMA_ERR,
  IDEMPOTENCY_CONFLICT_CODE,
  REVISION_CONFLICT_CODE,
  dramaErrorMessage,
  isEditInvalidError,
  isManifestStaleError,
  isQuoteExpiredError,
  isRevisionConflict,
} from "@/api/drama/errors";
import type { BackendSegmentView } from "@/api/drama/type";
import { ApiError } from "@/utils/requests/request";
import service from "@/utils/requests/service";
import { submitSegmentTask } from "@/utils/tasks/gateway";

const rawAsset = (id: number, url = `https://cdn/${id}.mp4`) => ({
  id,
  kind: "video" as const,
  url,
  mime_type: "video/mp4",
  byte_size: 10,
  width: 720,
  height: 1280,
  duration_ms: 8000,
  file_name: null,
});

const rawSegment = (patch: Partial<BackendSegmentView> = {}): BackendSegmentView => ({
  id: "5d2a",
  episode_no: 1,
  seq: 0,
  seconds: 12,
  prompt: "本片段场景设定在 @[山村街道](n_abc)。",
  takes: [
    {
      task_id: "123",
      status: "succeeded",
      asset: rawAsset(9),
      error_message: "",
      created_at: "2026-10-28T10:00:00Z",
    },
    {
      task_id: "124",
      status: "failed",
      asset: null,
      error_message: "内容审核未通过",
      created_at: "2026-10-28T10:05:00Z",
    },
  ],
  chosen_task_id: "123",
  status: "done",
  revision: 1,
  updated_at: "2026-10-28T10:05:00Z",
  ...patch,
});

describe("api/drama：片段 mapper（契约 §9）", () => {
  test("片段 snake→camel：分镜、take、素材一并映射", () => {
    const seg = mapSegment(rawSegment());
    expect(seg).toMatchObject({
      id: "5d2a",
      episodeNo: 1,
      seq: 0,
      seconds: 12,
      chosenTaskId: "123",
      status: "done",
      revision: 1,
    });
    // 片段不再带分镜表
    expect(seg).not.toHaveProperty("shots");
    expect(seg.takes[0].asset?.url).toBe("https://cdn/9.mp4");
    expect(seg.takes[0].asset?.id).toBe("9");
    expect(seg.takes[1]).toMatchObject({ taskId: "124", status: "failed", asset: null });
    expect(seg.takes[1].errorMessage).toBe("内容审核未通过");
  });

  test("takes 为 null、task_id 是数字时兜底", () => {
    const seg = mapSegment(
      rawSegment({
        takes: null,
        chosen_task_id: undefined as unknown as string,
      }),
    );
    expect(seg.takes).toEqual([]);
    expect(seg.chosenTaskId).toBe("");
    const num = mapSegment(
      rawSegment({
        takes: [
          {
            task_id: 77 as unknown as string,
            status: "running",
            asset: null,
            error_message: "",
            created_at: "x",
          },
        ],
      }),
    );
    expect(num.takes[0].taskId).toBe("77");
  });

  test("集摘要：二期字段缺省时按空处理，有值时映射", () => {
    const base = {
      no: 1,
      title: "t",
      synopsis: "",
      script_status: "ready" as const,
      locked: true,
      revision: 1,
      updated_at: "x",
    };
    expect(mapEpisodeSummary(base)).toMatchObject({
      storyboardStatus: "empty",
      segmentCount: 0,
      segmentSeconds: 0,
      doneSegmentCount: 0,
    });
    expect(
      mapEpisodeSummary({
        ...base,
        storyboard_status: "ready",
        segment_count: 4,
        segment_seconds: 50,
        done_segment_count: 1,
      }),
    ).toMatchObject({
      storyboardStatus: "ready",
      segmentCount: 4,
      segmentSeconds: 50,
      doneSegmentCount: 1,
    });
  });
});

describe("api/drama：片段请求体", () => {
  test("改片段只带传了的字段，chosenTaskId 转 snake_case", () => {
    expect(toSegmentUpdateBody({ revision: 2, prompt: "新" })).toEqual({
      revision: 2,
      prompt: "新",
    });
    expect(toSegmentUpdateBody({ revision: 2, seconds: 8, chosenTaskId: "123" })).toEqual({
      revision: 2,
      seconds: 8,
      chosen_task_id: "123",
    });
  });

  test("分镜任务带 max_seconds，其他步骤不带", () => {
    expect(toJobBody({ step: "storyboard", episodes: [1], maxSeconds: 10 })).toEqual({
      step: "storyboard",
      episodes: [1],
      max_seconds: 10,
    });
    expect(toJobBody({ step: "scripts", episodes: [1] })).toEqual({
      step: "scripts",
      episodes: [1],
    });
  });
});

describe("api/drama：片段请求", () => {
  let get: Mock<typeof service.get>;
  let post: Mock<typeof service.post>;
  let patch: Mock<typeof service.patch>;
  let put: Mock<typeof service.put>;
  let del: Mock<typeof service.delete>;
  beforeEach(() => {
    get = spyOn(service, "get");
    post = spyOn(service, "post");
    patch = spyOn(service, "patch");
    put = spyOn(service, "put");
    del = spyOn(service, "delete");
  });
  afterEach(() => {
    get.mockRestore();
    post.mockRestore();
    patch.mockRestore();
    put.mockRestore();
    del.mockRestore();
  });

  test("列表：items 为 null 兜底", async () => {
    get.mockResolvedValue({ items: null });
    expect(await listSegments("1a2b", 1)).toEqual([]);
    expect(get.mock.calls[0][0]).toBe("/dramas/1a2b/episodes/1/segments");
    get.mockResolvedValue({ items: [rawSegment()] });
    expect((await listSegments("1a2b", 1))[0].id).toBe("5d2a");
  });

  test("新增：after_seq 映射；改静默（409 由调用方处理）", async () => {
    post.mockResolvedValue(rawSegment());
    await addSegment("1a2b", 2, { prompt: "p", seconds: 8, afterSeq: 1 });
    expect(post.mock.calls[0][0]).toBe("/dramas/1a2b/episodes/2/segments");
    expect(post.mock.calls[0][1]).toEqual({ prompt: "p", seconds: 8, after_seq: 1 });
    patch.mockResolvedValue(rawSegment());
    await updateSegment("5d2a", { revision: 1, chosenTaskId: "123" });
    expect(patch.mock.calls[0][0]).toBe("/drama-segments/5d2a");
    expect(patch.mock.calls[0][1]).toEqual({ revision: 1, chosen_task_id: "123" });
    expect(patch.mock.calls[0][2]).toMatchObject({ silent: true });
  });

  test("删除与重排", async () => {
    del.mockResolvedValue(undefined);
    await deleteSegment("5d2a");
    expect(del.mock.calls[0][0]).toBe("/drama-segments/5d2a");
    await deleteEpisode("1a2b", 3);
    expect(del.mock.calls[1][0]).toBe("/dramas/1a2b/episodes/3");
    // 失败原因（有任务在跑等）由调用方就地提示，不弹全局 toast
    expect(del.mock.calls[1][1]).toMatchObject({ silent: true });
    put.mockResolvedValue({ items: [rawSegment({ id: "b" }), rawSegment({ id: "a" })] });
    const list = await reorderSegments("1a2b", 1, ["b", "a"]);
    expect(put.mock.calls[0][0]).toBe("/dramas/1a2b/episodes/1/segments/order");
    expect(put.mock.calls[0][1]).toEqual({ ids: ["b", "a"] });
    expect(list.map((s) => s.id)).toEqual(["b", "a"]);
  });

  test("generate：带幂等键，返回任务快照", async () => {
    post.mockResolvedValue({ task: { id: "1", status: "pending" } });
    const body = { model_id: "veo", input: { prompt: "x" } };
    const result = await generateSegment("5d2a", body, "key-1");
    expect(post.mock.calls[0][0]).toBe("/drama-segments/5d2a/generate");
    expect(post.mock.calls[0][1]).toEqual(body);
    expect(post.mock.calls[0][2]).toMatchObject({ headers: { "Idempotency-Key": "key-1" } });
    expect(result.task.id).toBe("1");
  });
});

describe("api/drama：转写稿（片场平台适配）", () => {
  test("adaptations 映射为 camelCase；带上可选 seconds，缺省不虚构", () => {
    const seg = mapSegment(
      rawSegment({
        adaptations: {
          h3: {
            prompt: "integrated",
            source_hash: "abc",
            seconds: 2,
            edited: true,
            updated_at: "t",
          },
          generic: {
            prompt: "legacy",
            source_hash: "def",
            edited: false,
            updated_at: "t2",
          },
        },
      }),
    );
    expect(seg.adaptations).toEqual({
      h3: { prompt: "integrated", sourceHash: "abc", seconds: 2, edited: true, updatedAt: "t" },
      generic: { prompt: "legacy", sourceHash: "def", edited: false, updatedAt: "t2" },
    });
    expect("seconds" in (seg.adaptations.generic ?? {})).toBe(false);
    expect(mapSegment(rawSegment({ adaptations: undefined })).adaptations).toEqual({});
  });

  test("改转写稿：adaptation 原样带上", () => {
    expect(
      toSegmentUpdateBody({ revision: 3, adaptation: { format: "h3", prompt: "新" } }),
    ).toEqual({ revision: 3, adaptation: { format: "h3", prompt: "新" } });
  });

  test("转写：POST /adapt，静默，返回片段", async () => {
    const post = spyOn(service, "post");
    post.mockResolvedValue(rawSegment({ revision: 4 }));
    const seg = await adaptSegment("5d2a", { format: "generic", revision: 3 });
    expect(post.mock.calls[0][0]).toBe("/drama-segments/5d2a/adapt");
    expect(post.mock.calls[0][1]).toEqual({ format: "generic", revision: 3 });
    expect(post.mock.calls[0][2]).toMatchObject({ silent: true });
    expect(seg.revision).toBe(4);
    post.mockRestore();
  });
});

describe("api/drama：二期剪辑、报价、清单、预览与 91001-91003 错误语义", () => {
  test("mapSegment 补齐 edit 默认值与 sourceFrameCount/hasAudio，segmentEditOf 兼容旧夹具", () => {
    const bare = mapSegment(rawSegment());
    expect(bare.sourceFrameCount).toBeNull();
    expect("hasAudio" in bare).toBe(false);
    expect("segmentVersion" in bare).toBe(false);
    expect(bare.edit).toEqual(DEFAULT_SEGMENT_EDIT);
    expect(segmentEditOf(undefined)).toEqual(DEFAULT_SEGMENT_EDIT);

    const probed = mapSegment(
      rawSegment({
        segment_version: 17,
        source_frame_count: 144,
        has_audio: false,
        edit: {
          source_in_f: 12,
          source_out_f: 96,
          mute_original: true,
          audio_gain_db: -6.5,
          native_is_dialogue: true,
          av_pts_offset_ms: 120,
        },
      }),
    );
    expect(probed.sourceFrameCount).toBe(144);
    expect(probed.hasAudio).toBe(false);
    expect(probed.segmentVersion).toBe(17);
    expect(probed.edit).toEqual({
      sourceInF: 12,
      sourceOutF: 96,
      muteOriginal: true,
      audioGainDb: -6.5,
      nativeIsDialogue: true,
      avPtsOffsetMs: 120,
    });
  });

  test("toSegmentUpdateBody 保留 source_out_f: null、expected_task_id 与 reset_edit_on_task_change", () => {
    const body = toSegmentUpdateBody({
      revision: 4,
      chosenTaskId: "t2",
      expectedTaskId: null,
      resetEditOnTaskChange: false,
      edit: {
        sourceInF: 6,
        sourceOutF: null,
        audioGainDb: 3,
      },
    });
    expect(body).toEqual({
      revision: 4,
      chosen_task_id: "t2",
      expected_task_id: "",
      reset_edit_on_task_change: false,
      edit: {
        source_in_f: 6,
        source_out_f: null,
        audio_gain_db: 3,
      },
    });
    expect(JSON.parse(JSON.stringify(body))).toHaveProperty("edit.source_out_f", null);
  });

  test("quoteSegment & generateSegment(quoteToken) 契约", async () => {
    const post = spyOn(service, "post");
    post.mockResolvedValueOnce({
      effective_input: { duration: 5, resolution: "720P" },
      raw_size: "1280x720",
      credits: 24,
      price_fingerprint: "fp-1",
      quote_token: "qt-abc",
      expires_at: "2026-11-15T15:00:00Z",
    });
    const quote = await quoteSegment("5d2a", { modelId: "veo", input: { prompt: "x" } });
    expect(post.mock.calls[0][0]).toBe("/drama-segments/5d2a/quote");
    expect(post.mock.calls[0][1]).toEqual({ model_id: "veo", input: { prompt: "x" } });
    expect(quote).toEqual({
      effectiveInput: { duration: 5, resolution: "720P" },
      rawSize: "1280x720",
      credits: 24,
      priceFingerprint: "fp-1",
      quoteToken: "qt-abc",
      expiresAt: "2026-11-15T15:00:00Z",
    });

    post.mockResolvedValueOnce({ task: { id: "9", status: "pending" } });
    await generateSegment(
      "5d2a",
      { modelId: "veo", input: { prompt: "x" }, quoteToken: "qt-abc" },
      "key-2",
    );
    expect(post.mock.calls[1][0]).toBe("/drama-segments/5d2a/generate");
    expect(post.mock.calls[1][1]).toEqual({
      model_id: "veo",
      input: { prompt: "x" },
      quote_token: "qt-abc",
    });
    expect(post.mock.calls[1][2]).toMatchObject({ headers: { "Idempotency-Key": "key-2" } });

    post.mockResolvedValueOnce({
      task: {
        id: "10",
        canvas_id: "c1",
        node_id: "seg:5d2a",
        kind: "video",
        model_id: "veo",
        status: "pending",
        progress: null,
        outputs: null,
        error_code: null,
        error_message: null,
        credits: 24,
        charged_credits: null,
        version: 1,
        deadline_at: null,
        created_at: "2026-11-15T15:00:00Z",
        submitted_at: null,
        finished_at: null,
      },
    });
    const get = spyOn(service, "get").mockResolvedValue({ available: 100 });
    const submitted = await submitSegmentTask(
      "5d2a",
      { modelId: "veo", input: { prompt: "x" } },
      "qt-via-arg",
    );
    expect(String(submitted.id)).toBe("10");
    expect(post.mock.calls[2][0]).toBe("/drama-segments/5d2a/generate");
    expect(post.mock.calls[2][1]).toEqual({
      model_id: "veo",
      input: { prompt: "x" },
      quote_token: "qt-via-arg",
    });
    expect(
      (post.mock.calls[2][2] as { headers: Record<string, string> }).headers["Idempotency-Key"],
    ).toBeTruthy();
    get.mockRestore();
    post.mockRestore();
  });

  test("getEpisodeManifest / createEpisodePreview / getEpisodePreview（preview 必传 withCredentials）", async () => {
    const get = spyOn(service, "get");
    const post = spyOn(service, "post");

    get.mockResolvedValueOnce({
      manifest: {
        v: 1,
        fps: 24,
        total_frames: 120,
        clips: [
          {
            segment_id: 1,
            task_id: 2,
            asset_id: 3,
            sha256: "h".repeat(64),
            seq: 0,
            source_in_f: 0,
            source_out_f: 120,
            master_in_f: 0,
            master_out_f: 120,
            normalized_frames_24: 120,
            has_audio: true,
            decoded_audio_samples: 240000,
            v_start_us: 0,
            a_start_us: 0,
            mute_original: false,
            audio_gain_db: 0,
            native_is_dialogue: false,
            av_pts_offset_ms: 0,
          },
        ],
        program: { v: 1, audio: [], overlays: [] },
      },
      manifest_hash: "a".repeat(64),
      segment_version: 7,
      skipped_seqs: null,
      diagnostics: ["clip-ok"],
    });
    const manifest = await getEpisodeManifest("d1", 2);
    expect(get.mock.calls[0][0]).toBe("/dramas/d1/episodes/2/manifest");
    expect(manifest.segmentVersion).toBe(7);
    expect(manifest.manifestHash).toBe("a".repeat(64));
    expect(manifest.manifest.clips[0]).toMatchObject({
      segmentId: "1",
      taskId: "2",
      assetId: "3",
      hasAudio: true,
    });
    expect(manifest.skippedSeqs).toEqual([]);
    expect(manifest.diagnostics).toEqual(["clip-ok"]);

    post.mockResolvedValueOnce({ ticket: "tk-1", status: "pending", expires_at: 1800000000 });
    const created = await createEpisodePreview("d1", 2, {
      segmentVersion: 7,
      manifestHash: "a".repeat(64),
      variant: "clean",
      rangeF: { inF: 0, outF: 48 },
    });
    expect(post.mock.calls[0][0]).toBe("/dramas/d1/episodes/2/preview");
    expect(post.mock.calls[0][1]).toEqual({
      segment_version: 7,
      manifest_hash: "a".repeat(64),
      variant: "clean",
      range_f: { in_f: 0, out_f: 48 },
    });
    expect(post.mock.calls[0][2]).toMatchObject({ withCredentials: true });
    expect(created).toEqual({
      ticket: "tk-1",
      status: "pending",
      streamUrl: null,
      expiresAt: 1800000000,
    });

    get.mockResolvedValueOnce({
      ticket: "tk-1",
      status: "ready",
      stream_url: "/api/v1/dramas/d1/episodes/2/preview/tk-1/stream?token=tok",
      expires_at: 1800000000,
    });
    const polled = await getEpisodePreview("d1", 2, "tk-1");
    expect(get.mock.calls[1][0]).toBe("/dramas/d1/episodes/2/preview/tk-1");
    expect(get.mock.calls[1][2]).toMatchObject({ withCredentials: true });
    expect(polled.streamUrl).toBe("/api/v1/dramas/d1/episodes/2/preview/tk-1/stream?token=tok");

    get.mockRestore();
    post.mockRestore();
  });

  test("91001 / 91002 绝不当作 revision 冲突，91003 优先展示后端具体 Msg", () => {
    const stale = new ApiError("stale", DRAMA_ERR.manifestStale, 409);
    const expired = new ApiError("expired", DRAMA_ERR.quoteExpired, 409);
    const invalid = new ApiError("帧终点越界", DRAMA_ERR.editInvalid, 400);

    expect(DRAMA_ERR.manifestStale).toBe(91001);
    expect(DRAMA_ERR.quoteExpired).toBe(91002);
    expect(DRAMA_ERR.editInvalid).toBe(91003);

    expect(isManifestStaleError(stale)).toBe(true);
    expect(isQuoteExpiredError(expired)).toBe(true);
    expect(isEditInvalidError(invalid)).toBe(true);

    expect(isRevisionConflict(stale)).toBe(false);
    expect(isRevisionConflict(expired)).toBe(false);
    expect(isRevisionConflict(new ApiError("rev", REVISION_CONFLICT_CODE, 409))).toBe(true);
    expect(isRevisionConflict(new ApiError("idem", IDEMPOTENCY_CONFLICT_CODE, 409))).toBe(false);
    expect(isRevisionConflict(new ApiError("other", 40900, 409))).toBe(false);

    expect(dramaErrorMessage(new ApiError("", DRAMA_ERR.manifestStale, 409))).toContain("清单");
    expect(dramaErrorMessage(new ApiError("", DRAMA_ERR.quoteExpired, 409))).toContain("报价");
    expect(dramaErrorMessage(invalid)).toBe("帧终点越界");
    expect(dramaErrorMessage(new ApiError("", DRAMA_ERR.editInvalid, 400))).toContain("剪辑参数");
  });

  test("updateSegment PATCH 响应回传 segment_version 时正确映射为 segmentVersion", async () => {
    const patch = spyOn(service, "patch").mockResolvedValue(
      rawSegment({
        id: "seg-patch",
        revision: 4,
        segment_version: 9,
      }) as never,
    );
    try {
      const updated = await updateSegment("seg-patch", { revision: 3, seconds: 8 });
      expect(patch).toHaveBeenCalledWith(
        "/drama-segments/seg-patch",
        {
          revision: 3,
          seconds: 8,
        },
        { silent: true },
      );
      expect(updated.revision).toBe(4);
      expect(updated.segmentVersion).toBe(9);
    } finally {
      patch.mockRestore();
    }
  });

  test('getEpisodeManifest 支持 HTTP 202 status="preparing" 异步冷备清单元数据与 status="ready"', async () => {
    const get = spyOn(service, "get");
    try {
      get.mockResolvedValueOnce({
        status: "preparing",
        segment_version: 5,
        skipped_seqs: [2],
        diagnostics: ["warming-probes"],
      });
      const preparing = await getEpisodeManifest("d1", 2);
      expect(preparing.status).toBe("preparing");
      expect(preparing.segmentVersion).toBe(5);
      expect(preparing.manifestHash).toBe("");
      expect(preparing.skippedSeqs).toEqual([2]);
      expect(preparing.diagnostics).toEqual(["warming-probes"]);
      expect(preparing.manifest).toEqual({
        v: 1,
        fps: 24,
        totalFrames: 0,
        clips: [],
        program: { v: 1, audio: [], overlays: [] },
      });

      get.mockResolvedValueOnce({
        status: "ready",
        manifest: { v: 1, fps: 24, total_frames: 96, clips: [] },
        manifest_hash: "b".repeat(64),
        segment_version: 5,
        skipped_seqs: [],
        diagnostics: [],
      });
      const ready = await getEpisodeManifest("d1", 2);
      expect(ready.status).toBe("ready");
      expect(ready.manifestHash).toBe("b".repeat(64));
      expect(ready.manifest.totalFrames).toBe(96);

      const abortController = new AbortController();
      get.mockResolvedValueOnce({
        status: "ready",
        manifest: { v: 1, fps: 24, total_frames: 48, clips: [] },
        manifest_hash: "c".repeat(64),
        segment_version: 6,
        skipped_seqs: [],
        diagnostics: [],
      });
      const withSignal = await getEpisodeManifest("d1", 2, { signal: abortController.signal });
      expect(withSignal.manifest.totalFrames).toBe(48);
      expect(get).toHaveBeenLastCalledWith("/dramas/d1/episodes/2/manifest", undefined, {
        silent: true,
        signal: abortController.signal,
      });
    } finally {
      get.mockRestore();
    }
  });

  test("mapEpisodeSummary 与 mapSegment 透传稳定分集 id / episodeId 并兼容旧响应", () => {
    const withId = mapEpisodeSummary({
      id: "ep-stable-1",
      no: 1,
      title: "第一集",
      synopsis: "",
      script_status: "ready",
      locked: false,
      revision: 2,
      updated_at: "2026-11-15T00:00:00Z",
      storyboard_status: "ready",
      segment_count: 1,
      segment_seconds: 5,
      done_segment_count: 1,
      final: null,
      final_stale: false,
      hook: "",
      cliffhanger: "",
      unit_from: null,
      unit_to: null,
      segment_version: 4,
      edit_schema: 1,
    });
    expect(withId.id).toBe("ep-stable-1");

    const withoutId = mapEpisodeSummary({
      no: 2,
      title: "第二集",
      synopsis: "",
      script_status: "draft",
      locked: false,
      revision: 1,
      updated_at: "2026-11-15T00:00:00Z",
      storyboard_status: "idle",
      segment_count: 0,
      segment_seconds: 0,
      done_segment_count: 0,
      final: null,
      final_stale: false,
      hook: "",
      cliffhanger: "",
      unit_from: null,
      unit_to: null,
    });
    expect(withoutId.id).toBeUndefined();

    const mappedSeg = mapSegment(rawSegment({ episode_id: "ep-stable-1" }));
    expect(mappedSeg.episodeId).toBe("ep-stable-1");
  });
});
