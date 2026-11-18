import { describe, expect, test } from "bun:test";

import type { DramaJob, EpisodeSummary, Segment } from "@/api/drama/type";
import {
  activeComposeJob,
  buildComposeJobInput,
  canUseManifestCompose,
  COMPOSE_ASPECT_BASE,
  COMPOSE_SIZES,
  COMPOSE_VARIANTS,
  composeCheck,
  composeCheckEpisode,
  composeDimensions,
  composeExportsOf,
  composeNotice,
  createComposeRequestKey,
  finalState,
  latestComposeExports,
  missingVideoSeqs,
  normalizeComposeVariants,
  skippedText,
} from "@/utils/drama/compose";
import { jobTitle } from "@/utils/drama/jobs";

const asset = (id: string, url = `https://cdn/${id}.mp4`) => ({
  id,
  url,
  kind: "video" as const,
  mimeType: "video/mp4",
  byteSize: 1,
  width: null,
  height: null,
  durationMs: null,
  fileName: null,
});

const seg = (id: string, seq: number, withVideo: boolean): Segment => ({
  id,
  episodeNo: 1,
  seq,
  seconds: 10,
  prompt: "",
  takes: withVideo
    ? [
        {
          taskId: `t${id}`,
          status: "succeeded",
          asset: asset(id),
          errorMessage: "",
          createdAt: "x",
        },
      ]
    : [],
  chosenTaskId: withVideo ? `t${id}` : "",
  status: "idle",
  revision: 1,
  updatedAt: "x",
});

const episode = (patch: Partial<EpisodeSummary> = {}): EpisodeSummary => ({
  no: 1,
  title: "第一集",
  synopsis: "",
  scriptStatus: "ready",
  locked: true,
  revision: 1,
  updatedAt: "x",
  storyboardStatus: "ready",
  segmentCount: 3,
  segmentSeconds: 30,
  doneSegmentCount: 2,
  final: null,
  finalStale: false,
  ...patch,
});

const job = (patch: Partial<DramaJob> = {}): DramaJob => ({
  id: "j1",
  step: "compose",
  episodes: [1],
  status: "running",
  progress: 30,
  credits: 0,
  errorCode: 0,
  errorMessage: "",
  createdAt: "2026-10-28T10:00:00Z",
  finishedAt: null,
  result: null,
  ...patch,
});

describe("合成整集按钮可用性", () => {
  test("至少一个片段有采用视频才可点", () => {
    const check = composeCheck([seg("a", 0, false), seg("b", 1, true)], null);
    expect(check).toEqual({ enabled: true, reason: null });
  });

  test("一个采用视频都没有：禁用并写原因", () => {
    const check = composeCheck([seg("a", 0, false)], null);
    expect(check.enabled).toBe(false);
    expect(check.reason).toBe("至少要有一个片段采用了视频才能合成");
  });

  test("没有片段：同样禁用", () => {
    expect(composeCheck([], null).enabled).toBe(false);
  });

  test("有进行中的任务：禁用并带上任务原因（同一部短剧同时只有一个任务）", () => {
    const check = composeCheck([seg("a", 0, true)], "有进行中的任务：分集剧本 · 3 集");
    expect(check).toEqual({ enabled: false, reason: "有进行中的任务：分集剧本 · 3 集" });
  });
});

describe("分集卡片上的合成判定", () => {
  test("集摘要里有采用视频的片段才可点；有进行中的任务时禁用", () => {
    expect(composeCheckEpisode(episode({ doneSegmentCount: 1 }), null).enabled).toBe(true);
    expect(composeCheckEpisode(episode({ doneSegmentCount: 0 }), null).enabled).toBe(false);
    expect(composeCheckEpisode(episode({ doneSegmentCount: 2 }), "有进行中的任务：x")).toEqual({
      enabled: false,
      reason: "有进行中的任务：x",
    });
  });
});

describe("没有采用视频的片段（合成时会被跳过）", () => {
  test("missingVideoSeqs 返回 0 起的 seq，按顺序", () => {
    expect(missingVideoSeqs([seg("a", 0, false), seg("b", 1, true), seg("c", 2, false)])).toEqual([
      0, 2,
    ]);
  });

  test("skippedText 用界面上的序号（seq+1，两位）", () => {
    expect(skippedText([1, 3])).toBe("02、04");
    expect(skippedText([])).toBe("");
  });
});

describe("成片状态 finalState", () => {
  test("没有成片", () => {
    expect(finalState(episode())).toBe("none");
  });
  test("有成片且没过期", () => {
    expect(finalState(episode({ final: asset("f") }))).toBe("fresh");
  });
  test("有成片但片段之后改过：已过期", () => {
    expect(finalState(episode({ final: asset("f"), finalStale: true }))).toBe("stale");
  });
  test("没有成片时 finalStale 无意义，仍是 none", () => {
    expect(finalState(episode({ finalStale: true }))).toBe("none");
  });
});

describe("合成任务", () => {
  test("activeComposeJob 只认这一集进行中的合成任务", () => {
    const jobs = [
      job({ id: "a", episodes: [2] }),
      job({ id: "b", step: "storyboard" }),
      job({ id: "c", status: "succeeded" }),
      job({ id: "d" }),
    ];
    expect(activeComposeJob(jobs, 1)?.id).toBe("d");
    expect(activeComposeJob(jobs, 3)).toBeNull();
  });

  test("jobTitle 带步骤名和集号", () => {
    expect(jobTitle(job())).toBe("合成整集 · 第 1 集");
  });

  test("composeNotice：成功且跳过了片段才提示，带上序号", () => {
    const done = job({
      status: "succeeded",
      result: { asset: asset("f"), skippedSeqs: [1, 3] },
    });
    expect(composeNotice(done)).toBe("第 1 集已合成，跳过了没有采用视频的片段 02、04");
  });

  test("composeNotice：没跳过、非合成、未成功都不提示", () => {
    expect(
      composeNotice(job({ status: "succeeded", result: { asset: asset("f"), skippedSeqs: [] } })),
    ).toBeNull();
    expect(composeNotice(job({ status: "running" }))).toBeNull();
    expect(
      composeNotice(job({ step: "storyboard", status: "succeeded", result: null })),
    ).toBeNull();
  });

  test("PR1 导出档位、变体归一化、compose 请求构建与 exports 提取", () => {
    expect(COMPOSE_SIZES).toEqual(["720p", "1080p"]);
    expect(COMPOSE_VARIANTS).toEqual(["clean", "packaged"]);
    expect(normalizeComposeVariants([])).toEqual(["clean"]);
    expect(normalizeComposeVariants(["packaged", "clean", "packaged"])).toEqual([
      "packaged",
      "clean",
    ]);

    const key = createComposeRequestKey();
    expect(key.startsWith("approval:")).toBe(false);
    expect(key.length).toBeLessThanOrEqual(96);

    const input = buildComposeJobInput({
      episodeNo: 2,
      manifest: { segmentVersion: 5, manifestHash: "e".repeat(64) },
      size: "1080p",
      variants: ["clean", "packaged"],
      composeRequestKey: key,
    });
    expect(input).toEqual({
      step: "compose",
      episodes: [2],
      compose: {
        segmentVersion: 5,
        manifestHash: "e".repeat(64),
        size: "1080p",
        variants: ["clean", "packaged"],
      },
      composeRequestKey: key,
    });

    const older = job({
      id: "j-old",
      episodes: [2],
      status: "succeeded",
      finishedAt: "2026-10-28T10:00:00Z",
      result: {
        asset: asset("old"),
        skippedSeqs: [],
        exports: [
          {
            sha256: "1".repeat(64),
            variant: "clean",
            size: "720p",
            width: 1280,
            height: 720,
            manifestHash: "1".repeat(64),
            asset: asset("old"),
          },
        ],
      },
    });
    const newer = job({
      id: "j-new",
      episodes: [2],
      status: "succeeded",
      finishedAt: "2026-10-28T12:00:00Z",
      result: {
        asset: asset("new"),
        skippedSeqs: [],
        exports: [
          {
            sha256: "2".repeat(64),
            variant: "packaged",
            size: "1080p",
            width: 1920,
            height: 1080,
            manifestHash: "e".repeat(64),
            asset: asset("new"),
          },
        ],
      },
    });
    expect(composeExportsOf(newer)).toHaveLength(1);
    expect(latestComposeExports([older, newer], 2)[0]?.variant).toBe("packaged");
    expect(latestComposeExports([older, newer], 2, null)).toEqual([]);
    expect(latestComposeExports([older, newer], 2, "")).toEqual([]);
    expect(latestComposeExports([older, newer], 2, "j-old")[0]?.variant).toBe("clean");
    expect(latestComposeExports([older, newer], 2, "j-missing")).toEqual([]);
    expect(latestComposeExports([older, newer], 2, "j-failed")).toEqual([]);
    expect(latestComposeExports([older, newer], 3)).toEqual([]);
  });

  test("canUseManifestCompose 与 composeDimensions 覆盖全部 6 种受支持比例", () => {
    const ratios = ["16:9", "9:16", "4:3", "3:4", "1:1", "21:9"] as const;
    for (const ratio of ratios) {
      expect(canUseManifestCompose(ratio, { editSchema: 1 })).toBe(true);
      expect(canUseManifestCompose(ratio, { editSchema: 0 })).toBe(false);
      expect(canUseManifestCompose(ratio, null)).toBe(false);
    }
    expect(canUseManifestCompose("", { editSchema: 1 })).toBe(false);
    expect(canUseManifestCompose("3:2", { editSchema: 1 })).toBe(false);

    expect(COMPOSE_ASPECT_BASE["9:16"]).toEqual([720, 1280]);
    expect(composeDimensions("16:9", "720p")).toEqual([1280, 720]);
    expect(composeDimensions("16:9", "1080p")).toEqual([1920, 1080]);
    expect(composeDimensions("9:16", "720p")).toEqual([720, 1280]);
    expect(composeDimensions("9:16", "1080p")).toEqual([1080, 1920]);
    expect(composeDimensions("4:3", "720p")).toEqual([960, 720]);
    expect(composeDimensions("4:3", "1080p")).toEqual([1440, 1080]);
    expect(composeDimensions("3:4", "720p")).toEqual([720, 960]);
    expect(composeDimensions("3:4", "1080p")).toEqual([1080, 1440]);
    expect(composeDimensions("1:1", "720p")).toEqual([720, 720]);
    expect(composeDimensions("1:1", "1080p")).toEqual([1080, 1080]);
    expect(composeDimensions("21:9", "720p")).toEqual([1680, 720]);
    expect(composeDimensions("21:9", "1080p")).toEqual([2520, 1080]);
    expect(composeDimensions("unknown", "720p")).toEqual([1280, 720]);
  });

  test("latestComposeExports 在 finalComposeJobId 指向未成功/跨集任务时不回退", () => {
    const wrongEpisode = job({
      id: "j-other-ep",
      episodes: [1],
      status: "succeeded",
      result: {
        asset: asset("other"),
        skippedSeqs: [],
        exports: [
          {
            sha256: "3".repeat(64),
            variant: "clean",
            size: "720p",
            width: 720,
            height: 1280,
            manifestHash: "3".repeat(64),
            asset: asset("other"),
          },
        ],
      },
    });
    const failedTarget = job({
      id: "j-failed",
      episodes: [2],
      status: "failed",
    });
    const fallbackSucceeded = job({
      id: "j-fallback",
      episodes: [2],
      status: "succeeded",
      result: {
        asset: asset("fb"),
        skippedSeqs: [],
        exports: [
          {
            sha256: "4".repeat(64),
            variant: "packaged",
            size: "720p",
            width: 720,
            height: 1280,
            manifestHash: "4".repeat(64),
            asset: asset("fb"),
          },
        ],
      },
    });

    expect(latestComposeExports([wrongEpisode, fallbackSucceeded], 2, "j-other-ep")).toEqual([]);
    expect(latestComposeExports([failedTarget, fallbackSucceeded], 2, "j-failed")).toEqual([]);
    expect(latestComposeExports([failedTarget, fallbackSucceeded], 2)).toHaveLength(1);
  });
});

test("静帧和黑场可单独导出；16:9 限制和节目单诊断可阻断导出", () => {
  const black = { ...seg("black", 0, false), visualKind: "black" as const };
  const still = { ...seg("still", 1, false), visualKind: "still" as const, stillAssetId: 22 };
  const missing = seg("missing", 2, false);
  expect(composeCheck([black], null, { ratio: "16:9" }).enabled).toBe(true);
  expect(composeCheck([still], null, { ratio: "16:9" }).enabled).toBe(true);
  expect(composeCheck([black], null, { ratio: "9:16" }).enabled).toBe(false);
  expect(missingVideoSeqs([black, still, missing])).toEqual([2]);
  expect(missingVideoSeqs([black, still, missing], { skippedSeqs: [] })).toEqual([]);
  const manifestV2 = {
    status: "ready" as const,
    manifest: null,
    manifestHash: "",
    segmentVersion: 1,
    skippedSeqs: [],
    diagnostics: [],
    programDiagnostics: [
      { cue_id: "sfx", field: "anchor", reason: "anchor_source_replaced", severity: "error" },
    ],
  };
  expect(composeCheck([black], null, { manifestV2, ratio: "16:9" }).reason).toContain(
    "锚点的视频来源已更换",
  );
});
