import { describe, expect, spyOn, test } from "bun:test";

import { listJobs, mapEpisodeSummary, mapJob, toJobBody } from "@/api/drama";
import { DRAMA_ERR, dramaErrorMessage, jobFailureMessage } from "@/api/drama/errors";
import type { BackendEpisodeSummary, BackendJobView } from "@/api/drama/type";
import { ApiError } from "@/utils/requests/request";
import service from "@/utils/requests/service";

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
});
