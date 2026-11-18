import { describe, expect, spyOn, test } from "bun:test";

import {
  adaptSegment,
  addSegment,
  getEpisodeManifestV2,
  getEpisodeProgram,
  importSegmentTakes,
  mapSegment,
  mapEpisodePreview,
  patchTakeQC,
  programCueAssetId,
  putEpisodeProgram,
  reorderSegments,
  updateSegment,
} from "@/api/drama";
import { DRAMA_ERR, dramaErrorMessage, isRevisionConflict } from "@/api/drama/errors";
import { FORK_ERR } from "@/api/drama/fork-errors";
import type {
  BackendSegmentView,
  BackendSegmentVisualPatch,
  EpisodeProgram,
  ProgramAudioRole,
  ProgramDiagnostic,
  PutEpisodeProgramReq,
} from "@/api/drama/type";
import { mergeSegment } from "@/utils/drama/segments";
import { parseServerMessage } from "@/utils/ws/socket-client";
import instance, { ApiError } from "@/utils/requests/request";
import service from "@/utils/requests/service";

describe("Stage C 节目单契约", () => {
  test("预览票据保留公开失败码、文案与节目单诊断", () => {
    const diagnostics: ProgramDiagnostic[] = [
      {
        cue_id: "voice",
        segment_id: "encoded-segment",
        field: "asset_id",
        reason: "media_unavailable",
        severity: "error",
      },
    ];
    expect(
      mapEpisodePreview({
        ticket: "failed",
        status: "failed",
        expires_at: 123,
        error_code: 91012,
        error_message: "引用素材不可用或已变更",
        program_diagnostics: diagnostics,
      }),
    ).toEqual({
      ticket: "failed",
      status: "failed",
      streamUrl: null,
      expiresAt: 123,
      errorCode: 91012,
      errorMessage: "引用素材不可用或已变更",
      programDiagnostics: diagnostics,
    });
    const ready = mapEpisodePreview({ ticket: "ready", status: "ready", expires_at: 123 });
    expect(ready.errorCode).toBeUndefined();
    expect(ready.errorMessage).toBeUndefined();
    expect(ready.programDiagnostics).toBeUndefined();
  });

  test("业务错误使用 91009–91014，保留后端详情并在无详情时提供兜底", () => {
    const cases = [
      ["programInvalid", 91009, 400, "节目单参数或结构不合法"],
      ["anchorInvalid", 91010, 409, "节目锚点已失效"],
      ["visualInvalid", 91011, 400, "分镜视觉模式或特效参数不合法"],
      ["mediaUnavailable", 91012, 409, "引用素材不可用或已变更"],
      ["loudnessInvalid", 91013, 422, "成片混音未达到响度或真峰交付标准"],
      ["assetInUse", 91014, 409, "素材正被剧集引用，无法执行此操作"],
    ] as const;

    for (const [key, code, status, fallback] of cases) {
      expect(FORK_ERR[key]).toBe(code);
      expect(DRAMA_ERR[key]).toBe(code);
      expect(dramaErrorMessage(new ApiError("", code, status))).toBe(fallback);
      const error = new ApiError("后端具体原因", code, status);
      expect(dramaErrorMessage(error)).toBe("后端具体原因");
      expect(isRevisionConflict(error)).toBe(false);
    }
  });

  test("非空 overlays 的明确提示与锚点诊断可以交给调用方", () => {
    const msg = "当前版本暂不支持字幕与角标叠加，overlays 必须为空";
    expect(dramaErrorMessage(new ApiError(msg, 91009, 400))).toBe(msg);

    const data = [
      {
        cue_id: "sfx-1",
        segment_id: "a9851604a8131a9cabe4f60643915fbe",
        field: "anchor",
        reason: "source_changed",
        severity: "error",
      },
    ] satisfies ProgramDiagnostic[];
    const error = new ApiError("请重新绑定锚点", 91010, 409, {
      code: 91010,
      msg: "请重新绑定锚点",
      data,
    });
    expect(dramaErrorMessage(error)).toBe("请重新绑定锚点");
    expect(error.body?.data).toEqual(data);
  });

  test("视图素材 ID 是数字，写入时可用十进制字符串保持精度", () => {
    const segment = {
      visual_kind: "still",
      still_asset_id: 123,
      fx: { zoom_start: 1, zoom_end: 1.25, seed: 0 },
    } satisfies Pick<BackendSegmentView, "visual_kind" | "still_asset_id" | "fx">;
    const program = {
      v: 1,
      audio: [
        {
          id: "vo-1",
          role: "narration",
          asset_id: 234,
          start_f: 0,
          end_f: 48,
          src_in_sample: 0,
          src_out_sample: null,
          gain_db: 0,
          av_pts_offset_ms: 0,
        },
        {
          id: "bgm-1",
          role: "bgm",
          asset_id: 235,
          start_f: 0,
          end_f: 48,
          src_in_sample: 0,
          src_out_sample: null,
          gain_db: -6,
          duck_db: -8,
          loop: true,
          av_pts_offset_ms: 0,
        },
        {
          id: "sfx-1",
          role: "sfx",
          asset_id: 345,
          anchor: { segment_id: "a9851604a8131a9cabe4f60643915fbe", source_f: 0 },
          length_f: 24,
          src_in_sample: 0,
          src_out_sample: null,
          gain_db: 0,
          av_pts_offset_ms: 0,
        },
      ],
      overlays: [],
    } satisfies EpisodeProgram;
    // @ts-expect-error dialogue 不属于节目单音轨 role，原生对白从分镜派生
    const _rejectedDialogue: ProgramAudioRole = "dialogue";
    // @ts-expect-error voiceover 不属于节目单音轨 role，旁白固定使用 narration
    const _rejectedVoiceover: ProgramAudioRole = "voiceover";
    void _rejectedDialogue;
    void _rejectedVoiceover;
    const req = {
      expected_episode_id: "ae17d7503cf9442079cda2597ea8347f",
      expected_segment_version: 0,
      program: {
        ...program,
        audio: [{ ...program.audio[2]!, asset_id: "9223372036854775807" }],
      },
      rebind_anchor_cue_ids: ["sfx-1"],
    } satisfies PutEpisodeProgramReq;
    const clearStill = { still_asset_id: null } satisfies BackendSegmentVisualPatch;

    expect(JSON.stringify(segment)).toContain('"still_asset_id":123');
    expect(JSON.stringify(program)).toContain('"asset_id":345');
    expect(JSON.stringify(req)).toContain('"asset_id":"9223372036854775807"');
    expect(JSON.stringify(clearStill)).toBe('{"still_asset_id":null}');
  });
});

const programWire = {
  episode_id: "episode",
  segment_version: 2,
  edit_schema: 1,
  program: { v: 1, audio: [], overlays: [] },
} as const;

test("GET /program 使用集号并映射 CAS 基线", async () => {
  const get = spyOn(service, "get").mockResolvedValue(programWire);
  try {
    const view = await getEpisodeProgram("drama", 3);
    expect(get.mock.calls[0]?.[0]).toBe("/dramas/drama/episodes/3/program");
    expect(view).toEqual({
      episodeId: "episode",
      segmentVersion: 2,
      editSchema: 1,
      program: programWire.program,
    });
  } finally {
    get.mockRestore();
  }
});

test("PUT /program 保留十进制字符串素材 ID 和重绑条目", async () => {
  const put = spyOn(service, "put").mockResolvedValue(programWire);
  const req = {
    expected_episode_id: "episode",
    expected_segment_version: 1,
    program: {
      v: 1,
      overlays: [],
      audio: [
        {
          id: "sfx",
          role: "sfx",
          asset_id: "9223372036854775807",
          anchor: { segment_id: "segment", source_f: 0 },
          length_f: 1,
        },
      ],
    },
    rebind_anchor_cue_ids: ["sfx"],
  } satisfies PutEpisodeProgramReq;
  try {
    expect((await putEpisodeProgram("drama", 3, req)).segmentVersion).toBe(2);
    expect(put.mock.calls[0]?.slice(0, 2)).toEqual(["/dramas/drama/episodes/3/program", req]);
  } finally {
    put.mockRestore();
  }
});

test("V2 清单保留诊断、零哈希及可取消的 202", async () => {
  const diagnostic = {
    cue_id: "sfx",
    field: "anchor",
    reason: "anchor_source_replaced",
    severity: "error",
  };
  const pending = {
    status: "preparing",
    segment_version: 2,
    diagnostics: [],
    skipped_seqs: [],
    program_diagnostics: [],
  };
  const ready = {
    status: "ready",
    segment_version: 2,
    manifest_hash: "",
    diagnostics: ["锚点失效"],
    skipped_seqs: [2],
    program_diagnostics: [diagnostic],
    manifest: {
      v: 2,
      render_version: 2,
      episode_id: "episode",
      segment_version: 2,
      fps: 24,
      sample_rate: 48000,
      total_frames: 24,
      skipped_seqs: [2],
      clips: [
        {
          segment_id: "segment",
          visual_kind: "still",
          still_asset_id: 18,
          seq: 1,
          source_in_f: 0,
          source_out_f: 24,
          master_in_f: 0,
          master_out_f: 24,
          normalized_frames_24: 24,
          has_audio: false,
          decoded_audio_samples: 0,
          v_start_us: 0,
          a_start_us: 0,
          mute_original: false,
          audio_gain_db: 0,
          native_is_dialogue: false,
          av_pts_offset_ms: 0,
        },
      ],
      program: programWire.program,
    },
  };
  const get = spyOn(service, "get").mockResolvedValueOnce(pending).mockResolvedValueOnce(ready);
  try {
    const { signal } = new AbortController();
    expect(await getEpisodeManifestV2("drama", 3, { signal })).toMatchObject({
      status: "preparing",
      manifest: null,
    });
    expect(get.mock.calls[0]).toEqual([
      "/dramas/drama/episodes/3/manifest?v=2",
      undefined,
      { silent: true, parseReviver: expect.any(Function), signal },
    ]);
    expect(await getEpisodeManifestV2("drama", 3)).toMatchObject({
      status: "ready",
      manifestHash: "",
      skippedSeqs: [2],
      programDiagnostics: [diagnostic],
      manifest: {
        v: 2,
        renderVersion: 2,
        clips: [{ segmentId: "segment", taskId: null, assetId: null, stillAssetId: 18 }],
      },
    });
  } finally {
    get.mockRestore();
  }
});

test("reorderSegments 与全部片段写入接口保留超安全整数 still_asset_id 精度", async () => {
  const wire =
    '{"id":"s1","episode_no":1,"seq":0,"seconds":5,"prompt":"p","visual_kind":"still","still_asset_id":9007199254740993,"fx":{},"takes":[],"status":"idle","revision":2,"updated_at":"","adaptations":{}}';
  const parseWith = (options?: {
    parseReviver?: (k: string, v: unknown, ctx?: { source?: string }) => unknown;
  }) => JSON.parse(wire, options?.parseReviver);
  const put = spyOn(service, "put").mockImplementation(async (_url, _body, options) =>
    JSON.parse(`{"items":[${wire}]}`, options?.parseReviver),
  );
  const patch = spyOn(service, "patch").mockImplementation(async (_url, _body, options) =>
    parseWith(options),
  );
  const post = spyOn(service, "post").mockImplementation(async (_url, _body, options) =>
    parseWith(options),
  );
  try {
    const reordered = await reorderSegments("d", 1, ["s1"]);
    const patchedPrompt = await updateSegment("s1", { revision: 1, prompt: "next" });
    const added = await addSegment("d", 1, {});
    const adapted = await adaptSegment("s1", { revision: 1, format: "h3" });
    const imported = await importSegmentTakes("s1", {
      revision: 1,
      sourceCanvasId: "c1",
      taskId: "t1",
    });
    const qced = await patchTakeQC("s1", "t1", { revision: 1, qc: "approved" });
    for (const seg of [reordered[0]!, patchedPrompt, added, adapted, imported, qced]) {
      expect(seg.stillAssetIdText).toBe("9007199254740993");
    }
  } finally {
    put.mockRestore();
    patch.mockRestore();
    post.mockRestore();
  }
});

test("getEpisodeManifestV2、WebSocket drama.segment 与同修订并入保留超安全整数精度", async () => {
  const segJson =
    '{"id":"s1","episode_no":1,"seq":0,"seconds":5,"prompt":"p","visual_kind":"still","still_asset_id":9007199254740993,"fx":{},"takes":[],"status":"idle","revision":2,"updated_at":"","adaptations":{}}';
  const manifestJson = `{
    "status": "ready",
    "manifest_hash": "hash",
    "segment_version": 4,
    "skipped_seqs": [],
    "diagnostics": [],
    "program_diagnostics": [],
    "manifest": {
      "v": 2,
      "render_version": 2,
      "episode_id": "ep-1",
      "segment_version": 4,
      "fps": 24,
      "sample_rate": 48000,
      "total_frames": 120,
      "skipped_seqs": [],
      "clips": [
        {
          "segment_id": "s1",
          "visual_kind": "still",
          "asset_id": 9007199254740993,
          "still_asset_id": 9007199254740995,
          "seq": 0,
          "source_in_f": 0,
          "source_out_f": 120,
          "master_in_f": 0,
          "master_out_f": 120,
          "normalized_frames_24": 120,
          "has_audio": false,
          "decoded_audio_samples": 0,
          "v_start_us": 0,
          "a_start_us": 0,
          "mute_original": false,
          "audio_gain_db": 0,
          "native_is_dialogue": false,
          "av_pts_offset_ms": 0
        }
      ],
      "program": {
        "v": 1,
        "overlays": [],
        "audio": [{"id": "vo-1", "role": "narration", "asset_id": 9007199254740993, "start_f": 0, "end_f": 24}]
      }
    }
  }`;
  const get = spyOn(service, "get").mockImplementation(async (_url, _params, options) =>
    JSON.parse(manifestJson, options?.parseReviver),
  );
  try {
    const res = await getEpisodeManifestV2("d", 1);
    expect(res.manifest?.clips[0]?.assetIdText).toBe("9007199254740993");
    expect(res.manifest?.clips[0]?.stillAssetIdText).toBe("9007199254740995");
    expect(programCueAssetId(res.manifest!.program.audio[0]!)).toBe("9007199254740993");

    const wsMsg = parseServerMessage(
      `{"type":"drama.segment","data":{"drama_id":"d","episode_no":1,"segment":${segJson}}}`,
    );
    expect(wsMsg?.type).toBe("drama.segment");
    const wsSeg = mapSegment(
      (wsMsg as Extract<NonNullable<typeof wsMsg>, { type: "drama.segment" }>).data
        .segment as BackendSegmentView,
    );
    expect(wsSeg.stillAssetIdText).toBe("9007199254740993");

    const roundedSameRevision = mapSegment(JSON.parse(segJson) as BackendSegmentView);
    expect(roundedSameRevision.stillAssetIdText).toBeUndefined();
    const merged = mergeSegment([wsSeg], roundedSameRevision);
    expect(merged[0]?.stillAssetIdText).toBe("9007199254740993");
  } finally {
    get.mockRestore();
  }
});

test("Safari 16.4 等无 JSON.parse source context 环境下 HTTP 与 WebSocket 仍无损保留超安全整数 ID", async () => {
  const originalParse = JSON.parse;
  JSON.parse = ((
    text: string,
    reviver?: (this: unknown, key: string, value: unknown) => unknown,
  ) =>
    reviver
      ? originalParse(text, function (this: unknown, key: string, value: unknown) {
          return reviver.call(this, key, value);
        })
      : originalParse(text)) as typeof JSON.parse;
  const rawEnvelope = JSON.stringify({ code: 0, msg: "" }).replace(
    "}",
    ',"data":{"episode_id":"ep-1","segment_version":2,"items":[{"id":"s1","episode_no":1,"seq":0,"seconds":5,"prompt":"\\"still_asset_id\\":9007199254740999","visual_kind":"still","still_asset_id":9007199254740993,"fx":{},"takes":[],"status":"idle","revision":2,"updated_at":"","adaptations":{}}]}}',
  );
  const programEnvelope =
    '{"code":0,"msg":"","data":{"episode_id":"ep-1","segment_version":2,"edit_schema":1,"program":{"v":1,"overlays":[],"audio":[{"id":"cue-1","role":"bgm","asset_id":9007199254740993,"start_f":0,"end_f":24}]}}}';
  const prevAdapter = instance.defaults.adapter;
  const globalRecord = globalThis as { localStorage?: Storage };
  const prevStorage = globalRecord.localStorage;
  globalRecord.localStorage = {
    getItem: () => null,
    setItem: () => {},
    removeItem: () => {},
    clear: () => {},
    key: () => null,
    length: 0,
  } as Storage;
  try {
    let reply = rawEnvelope;
    instance.defaults.adapter = async (config) => ({
      data: reply,
      status: 200,
      statusText: "OK",
      headers: {},
      config,
    });
    const listRes = await service.get<{ items: BackendSegmentView[] }>(
      "/dramas/d/episodes/1/segments",
      undefined,
      { parseReviver: (await import("@/utils/drama/asset-id")).stageCAssetIdReviver },
    );
    expect(mapSegment(listRes.items[0]!).stillAssetIdText).toBe("9007199254740993");

    reply = programEnvelope;
    const prog = await getEpisodeProgram("d", 1);
    expect(programCueAssetId(prog.program.audio[0]!)).toBe("9007199254740993");

    const wsMsg = parseServerMessage(
      '{"type":"drama.segment","data":{"drama_id":"d","episode_no":1,"segment":{"id":"s1","episode_no":1,"seq":0,"seconds":5,"prompt":"\\"still_asset_id\\":9007199254740999","visual_kind":"still","still_asset_id":9007199254740993,"fx":{},"takes":[],"status":"idle","revision":2,"updated_at":"","adaptations":{}}}}',
    );
    const wsSeg = mapSegment(
      (wsMsg as Extract<NonNullable<typeof wsMsg>, { type: "drama.segment" }>).data
        .segment as BackendSegmentView,
    );
    expect(wsSeg.stillAssetIdText).toBe("9007199254740993");
  } finally {
    globalRecord.localStorage = prevStorage;
    instance.defaults.adapter = prevAdapter;
    JSON.parse = originalParse;
  }
});
