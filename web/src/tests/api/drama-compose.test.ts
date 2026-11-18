import { describe, expect, spyOn, test } from "bun:test";

import {
  getDrama,
  getEpisode,
  getEpisodeManifest,
  listActiveJobs,
  listJobs,
  mapEpisodeSummary,
  mapJob,
  submitJob,
  toJobBody,
} from "@/api/drama";
import { DRAMA_ERR, dramaErrorMessage, jobFailureMessage } from "@/api/drama/errors";
import type { BackendEpisodeSummary, BackendJobView } from "@/api/drama/type";
import { stageCAssetIdReviver } from "@/utils/drama/asset-id";
import instance, { ApiError } from "@/utils/requests/request";
import service from "@/utils/requests/service";

import { installDom } from "../helpers/dom";

const rawAsset = {
  id: 42,
  kind: "video" as const,
  url: "https://cdn/final.mp4",
  mime_type: "video/mp4",
  byte_size: 100,
  width: 720,
  height: 1280,
  duration_ms: 30000,
  file_name: "final.mp4",
};

const rawEpisode = (patch: Partial<BackendEpisodeSummary> = {}): BackendEpisodeSummary => ({
  no: 1,
  title: "第一集",
  synopsis: "",
  script_status: "ready",
  locked: true,
  revision: 1,
  updated_at: "x",
  ...patch,
});

const rawJob = (patch: Partial<BackendJobView> = {}): BackendJobView => ({
  id: "j1",
  step: "compose",
  episodes: [1],
  status: "succeeded",
  progress: 100,
  credits: 0,
  error_code: 0,
  error_message: "",
  created_at: "x",
  finished_at: "y",
  ...patch,
});

describe("契约 §14：集摘要的成片字段", () => {
  test("final / final_stale 映射；后端没带时按「没有成片、没过期」", () => {
    const bare = mapEpisodeSummary(rawEpisode());
    expect(bare.final).toBeNull();
    expect(bare.finalStale).toBe(false);
    const done = mapEpisodeSummary(rawEpisode({ final: rawAsset, final_stale: true }));
    expect(done.final).toMatchObject({ id: "42", url: "https://cdn/final.mp4", durationMs: 30000 });
    expect(done.finalStale).toBe(true);
    expect(
      mapEpisodeSummary(
        rawEpisode({
          segment_version: 12,
          edit_schema: 1,
          final_compose_job_id: 99,
        }),
      ),
    ).toMatchObject({
      segmentVersion: 12,
      editSchema: 1,
      finalComposeJobId: "99",
    });
  });
});

describe("契约 §14：JobView.result", () => {
  test("compose 成功带 asset、skipped_seqs 与 exports[]", () => {
    const job = mapJob(
      rawJob({
        result: {
          asset: rawAsset,
          skipped_seqs: [2],
          exports: [
            {
              sha256: "b".repeat(64),
              variant: "clean",
              size: "1080p",
              width: 1920,
              height: 1080,
              manifest_hash: "c".repeat(64),
              asset: { ...rawAsset, id: 77, url: "https://cdn/clean-1080p.mp4" },
            },
          ],
        },
      }),
    );
    expect(job.result?.asset.url).toBe("https://cdn/final.mp4");
    expect(job.result?.skippedSeqs).toEqual([2]);
    expect(job.result?.exports).toEqual([
      {
        sha256: "b".repeat(64),
        variant: "clean",
        size: "1080p",
        width: 1920,
        height: 1080,
        manifestHash: "c".repeat(64),
        asset: {
          id: "77",
          url: "https://cdn/clean-1080p.mp4",
          kind: "video",
          mimeType: "video/mp4",
          byteSize: 100,
          width: 720,
          height: 1280,
          durationMs: 30000,
          fileName: "final.mp4",
        },
      },
    ]);
  });
  test("其他步骤 result 为 null / 缺省；skipped_seqs 与 exports 缺省为空数组", () => {
    expect(mapJob(rawJob({ step: "storyboard", result: null })).result).toBeNull();
    expect(mapJob(rawJob()).result).toBeNull();
    const mapped = mapJob(rawJob({ result: { asset: rawAsset, skipped_seqs: null } }));
    expect(mapped.result?.skippedSeqs).toEqual([]);
    expect(mapped.result?.exports).toEqual([]);
  });
  test("提交 compose 仅按新参数透传 compose 与 compose_request_key，非 compose 不携带", () => {
    expect(toJobBody({ step: "compose", episodes: [3], maxSeconds: 15 })).toEqual({
      step: "compose",
      episodes: [3],
    });
    expect(
      toJobBody({
        step: "compose",
        episodes: [1],
        composeRequestKey: "cmp-123",
        expectedEpisodeId: "ep-1",
        compose: {
          segmentVersion: 0,
          manifestHash: "d".repeat(64),
          size: "1080p",
          variants: ["clean", "packaged"],
        },
      }),
    ).toEqual({
      step: "compose",
      episodes: [1],
      compose_request_key: "cmp-123",
      expected_episode_id: "ep-1",
      compose: {
        segment_version: 0,
        manifest_hash: "d".repeat(64),
        size: "1080p",
        variants: ["clean", "packaged"],
      },
    });
    expect(
      toJobBody({
        step: "scripts",
        episodes: [1],
        composeRequestKey: "cmp-should-ignore",
        expectedEpisodeId: "ep-should-ignore",
        compose: { size: "1080p" },
      }),
    ).toEqual({
      step: "scripts",
      episodes: [1],
    });
  });
});

describe("契约 §16：67010", () => {
  test("文案与失败任务说明", () => {
    expect(DRAMA_ERR.nothingToCompose).toBe(67010);
    const error = new ApiError("", 67010, 400);
    expect(dramaErrorMessage(error)).toBe("这一集还没有可合成的片段");
    expect(jobFailureMessage({ errorCode: 67010, errorMessage: "" })).toBe(
      "这一集还没有可合成的片段",
    );
  });
});

describe("listJobs 全量任务查询", () => {
  test("请求 /dramas/:id/jobs（不带 status=active）并映射导出任务，null items 返回空数组", async () => {
    const rawJob: BackendJobView = {
      id: "j-final-1",
      step: "compose",
      episodes: [1],
      status: "succeeded",
      progress: 100,
      credits: 0,
      error_code: 0,
      error_message: "",
      created_at: "2026-11-15T10:00:00Z",
      finished_at: "2026-11-15T10:01:00Z",
      result: {
        asset: rawAsset,
        skipped_seqs: [],
        exports: [
          {
            sha256: "a".repeat(64),
            variant: "packaged",
            size: "1080p",
            width: 1080,
            height: 1920,
            manifest_hash: "b".repeat(64),
            asset: rawAsset,
          },
        ],
      },
    };
    const spy = spyOn(service, "get")
      .mockResolvedValueOnce({ items: [rawJob] })
      .mockResolvedValueOnce({ items: null });
    try {
      const jobs = await listJobs("drama-1");
      expect(spy).toHaveBeenNthCalledWith(1, "/dramas/drama-1/jobs", undefined, {
        silent: true,
        parseReviver: stageCAssetIdReviver,
      });
      expect(jobs).toHaveLength(1);
      expect(jobs[0]?.id).toBe("j-final-1");
      expect(jobs[0]?.result?.exports?.[0]?.size).toBe("1080p");

      const empty = await listJobs("drama-1");
      expect(empty).toEqual([]);
    } finally {
      spy.mockRestore();
    }
  });

  test("listJobs / getDrama / getEpisode / submitJob / manifest 经真实 Axios 原始 JSON 无损保留超安全整数成片与素材 ID", async () => {
    const dom = await installDom();
    const prevAdapter = instance.defaults.adapter;
    const rawJobBody = JSON.stringify({
      id: "j-unsafe",
      step: "compose",
      episodes: [1],
      status: "succeeded",
      progress: 100,
      credits: 0,
      error_code: 0,
      error_message: "",
      created_at: "2026-11-15T10:00:00Z",
      finished_at: "2026-11-15T10:01:00Z",
      result: {
        asset: { ...rawAsset, id: "__FINAL_ID__" },
        skipped_seqs: [],
        exports: [
          {
            sha256: "a".repeat(64),
            variant: "packaged",
            size: "1080p",
            width: 1080,
            height: 1920,
            manifest_hash: "b".repeat(64),
            asset: { ...rawAsset, id: "__EXPORT_ID__" },
          },
        ],
      },
    })
      .replace('"__FINAL_ID__"', "9007199254740993")
      .replace('"__EXPORT_ID__"', "9007199254740993");

    const rawDramaBody = JSON.stringify({
      id: "drama-1",
      title: "test",
      canvas_id: "c1",
      status: "done",
      source_kind: "script",
      style_key: "realistic",
      target_episodes: 1,
      episode_seconds: 15,
      revision: 1,
      episodes: [rawEpisode({ final: { ...rawAsset, id: "__EP_FINAL__" } })],
      active_jobs: [JSON.parse(rawJobBody.replace(/9007199254740993/g, "9007199254740995"))],
      created_at: "2026-11-15T10:00:00Z",
      updated_at: "2026-11-15T10:01:00Z",
    }).replace('"__EP_FINAL__"', "9007199254740993");

    const rawManifestBody = JSON.stringify({
      manifest: {
        v: 1,
        fps: 24,
        total_frames: 24,
        clips: [
          {
            segment_id: 1,
            task_id: 2,
            asset_id: "__CLIP_ASSET__",
            sha256: "a".repeat(64),
            seq: 0,
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
        program: { v: 1, audio: [], overlays: [] },
      },
      manifest_hash: "d".repeat(64),
      segment_version: 1,
      skipped_seqs: [],
      diagnostics: [],
    }).replace('"__CLIP_ASSET__"', "9007199254740993");

    instance.defaults.adapter = async (config) => {
      const url = config.url ?? "";
      let body = `{"items":[${rawJobBody}]}`;
      if (url === "/dramas/drama-1") body = rawDramaBody;
      else if (url === "/dramas/drama-1/episodes/1") {
        body = JSON.stringify({
          ...rawEpisode({ final: { ...rawAsset, id: "__ONE_EP__" } }),
          script_md: "body",
        }).replace('"__ONE_EP__"', "9007199254740993");
      } else if (config.method === "post" && url === "/dramas/drama-1/jobs") {
        body = rawJobBody;
      } else if (url.endsWith("/manifest")) {
        body = rawManifestBody;
      }
      return {
        data: `{"code":0,"msg":"","data":${body}}`,
        status: 200,
        statusText: "OK",
        headers: {},
        config,
      };
    };

    try {
      const jobs = await listJobs("drama-1");
      expect(jobs[0]?.result?.asset.id).toBe("9007199254740993");
      expect(jobs[0]?.result?.exports?.[0]?.asset.id).toBe("9007199254740993");

      const activeJobs = await listActiveJobs("drama-1");
      expect(activeJobs[0]?.result?.exports?.[0]?.asset.id).toBe("9007199254740993");

      const submitted = await submitJob("drama-1", { step: "compose", episodes: [1] });
      expect(submitted.result?.exports?.[0]?.asset.id).toBe("9007199254740993");

      const drama = await getDrama("drama-1", true);
      expect(drama.episodes[0]?.final?.id).toBe("9007199254740993");

      const episode = await getEpisode("drama-1", 1);
      expect(episode.final?.id).toBe("9007199254740993");

      const manifest = await getEpisodeManifest("drama-1", 1);
      expect(manifest.manifest.clips[0]?.assetId).toBe("9007199254740993");
    } finally {
      instance.defaults.adapter = prevAdapter;
      dom.restore();
    }
  });
});
