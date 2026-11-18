import { afterEach, beforeEach, describe, expect, spyOn, test } from "bun:test";
import { createElement, useEffect, useState } from "react";
import { toast } from "sonner";

import { DRAMA_ERR } from "@/api/drama/errors";
import type {
  BackendJobView,
  BackendManifestResponse,
  BackendSegmentView,
  DramaDetail,
  DramaJob,
  EpisodeSummary,
  Segment,
} from "@/api/drama/type";
import { DramaWorkspaceContext } from "@/pages/drama/workspace/context";
import { FinalArea } from "@/pages/drama/workspace/final-area";
import type { DramaWorkspace } from "@/pages/drama/workspace/use-workspace";
import { useDramaJobsStore } from "@/store/drama";
import { segmentKey, useDramaSegmentsStore } from "@/store/drama-segments";
import { ApiError } from "@/utils/requests/request";
import service from "@/utils/requests/service";

import { installDom, type DomEnv } from "../../helpers/dom";

const rawSegment = (seq = 0, id = "seg-1", seconds = 10): BackendSegmentView => ({
  id,
  episode_no: 1,
  seq,
  seconds,
  prompt: "镜头描述",
  chosen_task_id: `task-${id}`,
  status: "idle",
  revision: 1,
  updated_at: "2026-11-15T10:00:00Z",
  takes: [
    {
      task_id: `task-${id}`,
      status: "succeeded",
      error_message: "",
      created_at: "2026-11-15T10:00:00Z",
      asset: {
        id: 100 + seq,
        kind: "video",
        url: `https://cdn.example.com/${id}.mp4`,
        mime_type: "video/mp4",
        byte_size: 1024,
        width: 720,
        height: 1280,
        duration_ms: seconds * 1000,
        file_name: `${id}.mp4`,
      },
    },
  ],
});

const storeSegment = (seq = 0, id = "seg-1", seconds = 10): Segment => ({
  id,
  episodeNo: 1,
  seq,
  seconds,
  prompt: "镜头描述",
  chosenTaskId: `task-${id}`,
  status: "idle",
  revision: 1,
  updatedAt: "2026-11-15T10:00:00Z",
  takes: [
    {
      taskId: `task-${id}`,
      status: "succeeded",
      errorMessage: "",
      createdAt: "2026-11-15T10:00:00Z",
      asset: {
        id: String(100 + seq),
        kind: "video",
        url: `https://cdn.example.com/${id}.mp4`,
        mimeType: "video/mp4",
        byteSize: 1024,
        width: 720,
        height: 1280,
        durationMs: seconds * 1000,
        fileName: `${id}.mp4`,
      },
    },
  ],
});

const rawManifest = (totalFrames: number, clipFrames = totalFrames): BackendManifestResponse => ({
  manifest: {
    v: 1,
    fps: 24,
    total_frames: totalFrames,
    clips: [
      {
        segment_id: 0,
        task_id: 1,
        asset_id: 100,
        sha256: "h".repeat(64),
        seq: 0,
        source_in_f: 0,
        source_out_f: clipFrames,
        master_in_f: 0,
        master_out_f: clipFrames,
        normalized_frames_24: clipFrames,
        has_audio: true,
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
  segment_version: 2,
  skipped_seqs: [],
  diagnostics: [],
});

const makeEpisode = (patch: Partial<EpisodeSummary> = {}): EpisodeSummary => ({
  no: 1,
  title: "第一集",
  synopsis: "",
  scriptStatus: "ready",
  locked: true,
  revision: 1,
  updatedAt: "2026-11-15T10:00:00Z",
  storyboardStatus: "ready",
  segmentCount: 1,
  segmentSeconds: 10,
  doneSegmentCount: 1,
  final: null,
  finalStale: false,
  editSchema: 1,
  segmentVersion: 2,
  ...patch,
});

const makeDrama = (ratio = "9:16", epPatch: Partial<EpisodeSummary> = {}): DramaDetail => ({
  id: "d-1",
  title: "测试短剧",
  genre: "",
  ratio,
  style: "",
  story: "",
  episodes: [makeEpisode(epPatch)],
  roles: [],
  scenes: [],
  props: [],
  activeJobs: [],
  createdAt: "2026-11-15T10:00:00Z",
  updatedAt: "2026-11-15T10:00:00Z",
});

const composeJob = (
  id: string,
  variant: "clean" | "packaged",
  url: string,
  finishedAt: string,
  status: DramaJob["status"] = "succeeded",
): DramaJob => ({
  id,
  step: "compose",
  episodes: [1],
  status,
  progress: status === "succeeded" ? 100 : 50,
  credits: 0,
  errorCode: 0,
  errorMessage: "",
  createdAt: finishedAt,
  finishedAt: status === "succeeded" ? finishedAt : null,
  result:
    status === "succeeded"
      ? {
          asset: {
            id: `asset-${id}`,
            kind: "video",
            url,
            mimeType: "video/mp4",
            byteSize: 2048,
            width: 720,
            height: 1280,
            durationMs: 5000,
            fileName: `${id}.mp4`,
          },
          skippedSeqs: [],
          exports: [
            {
              sha256: "e".repeat(64),
              variant,
              size: "720p",
              width: 720,
              height: 1280,
              manifestHash: "d".repeat(64),
              asset: {
                id: `asset-${id}`,
                kind: "video",
                url,
                mimeType: "video/mp4",
                byteSize: 2048,
                width: 720,
                height: 1280,
                durationMs: 5000,
                fileName: `${id}.mp4`,
              },
            },
          ],
        }
      : null,
});

const backendComposeJob = (
  id: string,
  variant: "clean" | "packaged",
  url: string,
  finishedAt: string,
  status: BackendJobView["status"] = "succeeded",
): BackendJobView => ({
  id,
  step: "compose",
  episodes: [1],
  status,
  progress: status === "succeeded" ? 100 : 0,
  credits: 0,
  error_code: 0,
  error_message: "",
  created_at: finishedAt,
  finished_at: status === "succeeded" ? finishedAt : null,
  result:
    status === "succeeded"
      ? {
          asset: {
            id: 99,
            kind: "video",
            url,
            mime_type: "video/mp4",
            byte_size: 2048,
            width: 720,
            height: 1280,
            duration_ms: 5000,
            file_name: `${id}.mp4`,
          },
          skipped_seqs: [],
          exports: [
            {
              sha256: "e".repeat(64),
              variant,
              size: "720p",
              width: 720,
              height: 1280,
              manifest_hash: "d".repeat(64),
              asset: {
                id: 99,
                kind: "video",
                url,
                mime_type: "video/mp4",
                byte_size: 2048,
                width: 720,
                height: 1280,
                duration_ms: 5000,
                file_name: `${id}.mp4`,
              },
            },
          ],
        }
      : null,
});

describe("FinalArea 多比例清单、帧时长与 final_compose_job_id 恢复", () => {
  let dom: DomEnv;
  let spies: Array<{ mockRestore: () => void }> = [];

  let cleanups: Array<() => Promise<void>> = [];

  beforeEach(async () => {
    dom = await installDom();
    cleanups = [];
    useDramaJobsStore.setState({ byDrama: {}, settled: 0 });
    useDramaSegmentsStore.setState({ byEpisode: {}, revisions: {} });
  });

  afterEach(async () => {
    for (const unmount of cleanups.splice(0)) {
      await unmount();
    }
    for (const s of spies) s.mockRestore();
    spies = [];
    useDramaJobsStore.setState({ byDrama: {}, settled: 0 });
    useDramaSegmentsStore.setState({ byEpisode: {}, revisions: {} });
    dom.restore();
  });

  const mountFinal = async (drama: DramaDetail, jobs: DramaJob[] = []) => {
    const workspace = { drama, jobs } as unknown as DramaWorkspace;
    const res = await dom.mount(
      createElement(
        DramaWorkspaceContext.Provider,
        { value: workspace },
        createElement(FinalArea, { ep: 1, onGo: () => {} }),
      ),
    );
    cleanups.push(res.unmount);
    return res;
  };

  test("有节目音频的 16:9 剧集读取 V2 清单、阻断诊断并显示历史导出的实测响度", async () => {
    const e = makeDrama("16:9", { id: "ep-1", finalComposeJobId: "measured" });
    const measured = composeJob("measured", "clean", "/measured.mp4", "2026-11-15T10:00:00Z");
    const item = measured.result!.exports![0]!;
    Object.assign(item, {
      loudnessStatus: "accepted",
      lufs: -16.12,
      truePeak: -1.55,
      audioSha256: "12345678abcdef",
    });
    measured.result!.exports!.push({
      ...item,
      variant: "packaged",
      loudnessStatus: "silence",
      lufs: undefined,
      truePeak: undefined,
    });
    const get = spyOn(service, "get").mockImplementation(async (url: string) => {
      if (url.endsWith("/segments")) return { items: [rawSegment()] };
      if (url.endsWith("/program"))
        return {
          episode_id: "ep-1",
          segment_version: 2,
          edit_schema: 1,
          program: { v: 1, audio: [{ id: "hit" }], overlays: [] },
        };
      if (url.endsWith("/manifest?v=2"))
        return {
          status: "ready",
          segment_version: 2,
          manifest_hash: "",
          skipped_seqs: [],
          diagnostics: [],
          program_diagnostics: [
            { cue_id: "hit", field: "anchor", reason: "anchor_source_replaced", severity: "error" },
          ],
        };
      throw new Error(`unexpected GET ${url}`);
    });
    const post = spyOn(service, "post").mockRejectedValue(
      new Error("diagnostics must block compose"),
    );
    spies.push(get, post);
    const { container } = await mountFinal(e, [measured]);
    expect(get.mock.calls.map(([url]) => url)).toContain("/dramas/d-1/episodes/1/manifest?v=2");
    expect(container.textContent).toContain("锚点的视频来源已更换");
    expect(container.textContent).toContain("-16.1 LUFS / TP -1.6 dBTP · #12345678");
    expect(container.textContent).toContain("静音母带");
    const composeButton = [...container.querySelectorAll("button")].find(
      (el) => el.textContent?.trim() === "合成第 1 集",
    );
    expect(composeButton?.disabled).toBe(true);
    expect(post).not.toHaveBeenCalled();
  });
  test("9:16 + editSchema=1 启用分档与版本导出，显示比例像素提示并按 total_frames/24 展示不足 1 秒时长（0.79s）", async () => {
    const seg = rawSegment(0, "seg-1", 10);
    const manifest = rawManifest(19, 19);
    // 保留后端真实 Go 契约下的数字 segment_id，验证可通过 clip.seq 精确匹配加密 segment.id
    manifest.manifest.clips[0]!.segment_id = 1;

    useDramaSegmentsStore.setState({
      byEpisode: { [segmentKey("d-1", 1)]: [storeSegment(0, "seg-1", 10)] },
    });

    const get = spyOn(service, "get").mockImplementation(async (url) => {
      if (url.endsWith("/segments")) return { items: [seg] };
      if (url.endsWith("/manifest")) return manifest;
      if (url.endsWith("/jobs")) return { items: [] };
      throw new Error(`unexpected GET ${url}`);
    });
    const post = spyOn(service, "post").mockResolvedValueOnce(
      backendComposeJob("j-new", "clean", "https://cdn/new.mp4", "2026-11-15T10:05:00Z", "queued"),
    );
    spies.push(get, post);

    const { container, unmount } = await mountFinal(makeDrama("9:16", { finalComposeJobId: null }));
    await dom.flush(20);

    // 清单面板与比例分辨率提示存在，且按钮文本仍保持 "720p" / "1080p"
    expect(container.textContent).toContain("9:16 · 720×1280");
    expect(container.textContent).toContain("清单 v2 · 19f");
    // total_frames = 19 -> 19/24 = 0.79s，且片段行同样按 clip.seq 展示精裁后时长 0.79s
    expect(container.textContent).toContain("0.79s");
    expect(container.querySelector("ul li")?.textContent).toContain("0.79s");
    expect(container.textContent).not.toContain("00:10");

    const btn1080 = Array.from(container.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "1080p",
    );
    expect(btn1080).toBeTruthy();
    await dom.click(btn1080);
    expect(container.textContent).toContain("9:16 · 1080×1920");

    const submitBtn = Array.from(container.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("合成第 1 集"),
    );
    await dom.click(submitBtn);
    await dom.flush(10);

    expect(post).toHaveBeenCalledTimes(1);
    expect(post.mock.calls[0]?.[1]).toMatchObject({
      step: "compose",
      episodes: [1],
      compose: {
        segment_version: 2,
        manifest_hash: "d".repeat(64),
        size: "1080p",
        variants: ["clean"],
      },
    });

    await unmount();
  });

  test("21:9 + editSchema=1 与 120 帧清单渲染 21:9 分辨率提示和 00:05 时长", async () => {
    const seg = rawSegment(0, "seg-1", 10);
    const manifest = rawManifest(120, 120);
    manifest.manifest.clips[0]!.segment_id = 42;
    useDramaSegmentsStore.setState({
      byEpisode: { [segmentKey("d-1", 1)]: [storeSegment(0, "seg-1", 10)] },
    });

    const get = spyOn(service, "get").mockImplementation(async (url) => {
      if (url.endsWith("/segments")) return { items: [seg] };
      if (url.endsWith("/manifest")) return manifest;
      return { items: [] };
    });
    spies.push(get);

    const { container, unmount } = await mountFinal(makeDrama("21:9", { finalComposeJobId: null }));
    await dom.flush(20);

    expect(container.textContent).toContain("21:9 · 1680×720");
    expect(container.textContent).toContain("00:05");
    expect(container.textContent).toContain("5s");

    const btn1080 = Array.from(container.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "1080p",
    );
    await dom.click(btn1080);
    expect(container.textContent).toContain("21:9 · 2520×1080");

    await unmount();
  });

  test("editSchema=0 或不受支持比例时提示精确原因并走旧整段合成路径", async () => {
    const seg = rawSegment(0, "seg-1", 10);
    useDramaSegmentsStore.setState({
      byEpisode: { [segmentKey("d-1", 1)]: [storeSegment(0, "seg-1", 10)] },
    });
    const get = spyOn(service, "get").mockImplementation(async (url) => {
      if (url.endsWith("/segments")) return { items: [seg] };
      throw new Error(`should not fetch ${url}`);
    });
    const post = spyOn(service, "post").mockResolvedValueOnce(
      backendComposeJob("j-legacy", "clean", "https://cdn/l.mp4", "2026-11-15T10:05:00Z", "queued"),
    );
    spies.push(get, post);

    const { container: c1, unmount: u1 } = await mountFinal(
      makeDrama("9:16", { editSchema: 0, finalComposeJobId: null }),
    );
    await dom.flush(10);
    expect(c1.textContent).toContain("启用 9:16 分档与版本导出");
    const submitBtn = Array.from(c1.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("合成第 1 集"),
    );
    await dom.click(submitBtn);
    await dom.flush(10);
    expect((post.mock.calls[0]?.[1] as { compose?: unknown } | undefined)?.compose).toBeUndefined();
    await u1();

    const { container: c2, unmount: u2 } = await mountFinal(
      makeDrama("", { editSchema: 1, finalComposeJobId: null }),
    );
    await dom.flush(10);
    expect(c2.textContent).toContain("当前项目比例为 未设置，将按原比例整段合成。");
    await u2();
  });

  test("store 缺失 finalComposeJobId 时主动拉取 listJobs 并仅展示目标任务的 exports（不触发 store.settled，不回退到其他成功任务）", async () => {
    const seg = rawSegment(0, "seg-1", 10);
    useDramaSegmentsStore.setState({
      byEpisode: { [segmentKey("d-1", 1)]: [storeSegment(0, "seg-1", 10)] },
    });
    const target = backendComposeJob(
      "j-final-target",
      "clean",
      "https://cdn/target.mp4",
      "2026-11-15T10:00:00Z",
    );
    const newerOther = backendComposeJob(
      "j-newer-other",
      "packaged",
      "https://cdn/newer.mp4",
      "2026-11-15T11:00:00Z",
    );

    let jobFetchCount = 0;
    const get = spyOn(service, "get").mockImplementation(async (url) => {
      if (url.endsWith("/segments")) return { items: [seg] };
      if (url.endsWith("/manifest")) return rawManifest(120, 120);
      if (url === "/dramas/d-1/jobs") {
        jobFetchCount += 1;
        return { items: [newerOther, target] };
      }
      throw new Error(`unexpected GET ${url}`);
    });
    spies.push(get);

    const { container, unmount } = await mountFinal(
      makeDrama("9:16", { finalComposeJobId: "j-final-target" }),
      [],
    );
    await dom.flush(20);

    expect(jobFetchCount).toBe(1);
    const exportsBox = container.querySelector('[aria-label="本次导出结果"]');
    expect(exportsBox?.textContent).toContain("导出文件（1）");
    expect(exportsBox?.textContent).toContain("纯净版");
    expect(exportsBox?.textContent).not.toContain("成片包装版");

    const previewBtn = Array.from(exportsBox?.querySelectorAll("button") ?? []).find(
      (b) => b.textContent?.trim() === "预览",
    );
    await dom.click(previewBtn);
    expect(exportsBox?.textContent).toContain("切回成片");

    const resetBtn = Array.from(exportsBox?.querySelectorAll("button") ?? []).find(
      (b) => b.textContent?.trim() === "切回成片",
    );
    await dom.click(resetBtn);
    expect(exportsBox?.textContent).not.toContain("切回成片");
    expect(useDramaJobsStore.getState().settled).toBe(0);

    await unmount();
  });

  test("finalComposeJobId === null 时不展示任何 exports 也不调用 listJobs", async () => {
    const seg = rawSegment(0, "seg-1", 10);
    useDramaSegmentsStore.setState({
      byEpisode: { [segmentKey("d-1", 1)]: [storeSegment(0, "seg-1", 10)] },
    });
    let jobFetchCount = 0;
    const get = spyOn(service, "get").mockImplementation(async (url) => {
      if (url.endsWith("/segments")) return { items: [seg] };
      if (url.endsWith("/manifest")) return rawManifest(120, 120);
      if (url === "/dramas/d-1/jobs") {
        jobFetchCount += 1;
        return { items: [] };
      }
      throw new Error(`unexpected GET ${url}`);
    });
    spies.push(get);

    const { container, unmount } = await mountFinal(
      makeDrama("9:16", { finalComposeJobId: null }),
      [composeJob("j-stale-old", "packaged", "https://cdn/old.mp4", "2026-11-15T12:00:00Z")],
    );
    await dom.flush(20);

    expect(jobFetchCount).toBe(0);
    expect(container.textContent).not.toContain("导出文件");

    await unmount();
  });

  test("finalComposeJobId 已在 store 中成功时不重复请求 listJobs；若 listJobs 找不到目标也不循环或回退", async () => {
    const seg = rawSegment(0, "seg-1", 10);
    useDramaSegmentsStore.setState({
      byEpisode: { [segmentKey("d-1", 1)]: [storeSegment(0, "seg-1", 10)] },
    });
    let jobFetchCount = 0;
    const get = spyOn(service, "get").mockImplementation(async (url) => {
      if (url.endsWith("/segments")) return { items: [seg] };
      if (url.endsWith("/manifest")) return rawManifest(120, 120);
      if (url === "/dramas/d-1/jobs") {
        jobFetchCount += 1;
        return {
          items: [
            backendComposeJob(
              "j-unrelated",
              "packaged",
              "https://cdn/u.mp4",
              "2026-11-15T12:00:00Z",
            ),
          ],
        };
      }
      throw new Error(`unexpected GET ${url}`);
    });
    spies.push(get);

    // 情况 1：已在 store 且 succeeded
    const { container: c1, unmount: u1 } = await mountFinal(
      makeDrama("9:16", { finalComposeJobId: "j-in-store" }),
      [composeJob("j-in-store", "clean", "https://cdn/s.mp4", "2026-11-15T12:00:00Z")],
    );
    await dom.flush(20);
    expect(jobFetchCount).toBe(0);
    expect(c1.textContent).toContain("导出文件（1）");
    await u1();

    // 情况 2：目标任务不存在 -> 请求一次 listJobs 后停止，不回退到 j-unrelated
    const { container: c2, unmount: u2 } = await mountFinal(
      makeDrama("9:16", { finalComposeJobId: "j-missing" }),
      [composeJob("j-unrelated", "packaged", "https://cdn/u.mp4", "2026-11-15T12:00:00Z")],
    );
    await dom.flush(30);
    expect(jobFetchCount).toBe(1);
    expect(c2.textContent).not.toContain("导出文件");
    await u2();
  });

  test("listJobs 返回的历史 running 任务不会覆盖实时合成进度或阻塞重新合成", async () => {
    const seg = rawSegment(0, "seg-1", 10);
    useDramaSegmentsStore.setState({
      byEpisode: { [segmentKey("d-1", 1)]: [storeSegment(0, "seg-1", 10)] },
    });

    const zombieRunning = backendComposeJob(
      "j-zombie-running",
      "clean",
      "https://cdn/z.mp4",
      "2026-11-15T11:30:00Z",
      "running",
    );
    const targetSucceeded = backendComposeJob(
      "j-target",
      "packaged",
      "https://cdn/t.mp4",
      "2026-11-15T12:00:00Z",
      "succeeded",
    );

    let scenario = 0;
    const get = spyOn(service, "get").mockImplementation(async (url) => {
      if (url.endsWith("/segments")) return { items: [seg] };
      if (url.endsWith("/manifest")) return rawManifest(240);
      if (url.endsWith("/jobs")) {
        return {
          items: scenario === 0 ? [zombieRunning, targetSucceeded] : [zombieRunning],
        };
      }
      return { items: [] };
    });
    spies.push(get);

    // 场景 1：listJobs 同时返回历史 running 与目标 succeeded
    const { container: c1, unmount: u1 } = await mountFinal(
      makeDrama("9:16", { finalComposeJobId: "j-target" }),
      [],
    );
    await dom.flush(30);
    expect(c1.textContent).not.toContain("正在合成");
    expect(c1.textContent).toContain("导出文件（1）");
    const btn1 = Array.from(c1.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("合成第 1 集"),
    );
    expect(btn1).toBeTruthy();
    expect(btn1?.disabled).toBe(false);
    await u1();

    // 场景 2：listJobs 仅返回历史 running（无目标任务）
    scenario = 1;
    const { container: c2, unmount: u2 } = await mountFinal(
      makeDrama("9:16", { finalComposeJobId: "j-target-2" }),
      [],
    );
    await dom.flush(30);
    expect(c2.textContent).not.toContain("正在合成");
    expect(c2.textContent).not.toContain("导出文件");
    const btn2 = Array.from(c2.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("合成第 1 集"),
    );
    expect(btn2).toBeTruthy();
    expect(btn2?.disabled).toBe(false);
    await u2();
  });

  test("listJobs 失败时释放防重锁并显示告警与重试按钮，点击重试后恢复导出列表", async () => {
    const seg = rawSegment(0, "seg-1", 10);
    useDramaSegmentsStore.setState({
      byEpisode: { [segmentKey("d-1", 1)]: [storeSegment(0, "seg-1", 10)] },
    });

    const targetSucceeded = backendComposeJob(
      "j-retry-target",
      "packaged",
      "https://cdn/recovered.mp4",
      "2026-11-15T12:00:00Z",
    );
    let jobFetchCount = 0;
    const get = spyOn(service, "get").mockImplementation(async (url) => {
      if (url.endsWith("/segments")) return { items: [seg] };
      if (url.endsWith("/manifest")) return rawManifest(240);
      if (url.endsWith("/jobs")) {
        jobFetchCount += 1;
        if (jobFetchCount === 1) {
          throw new Error("network down");
        }
        return { items: [targetSucceeded] };
      }
      return { items: [] };
    });
    spies.push(get);

    const { container, unmount } = await mountFinal(
      makeDrama("9:16", { finalComposeJobId: "j-retry-target" }),
      [],
    );
    await dom.flush(30);

    expect(jobFetchCount).toBe(1);
    const alertEl = container.querySelector('[role="alert"]');
    expect(alertEl).toBeTruthy();
    const retryBtn = container.querySelector<HTMLButtonElement>('[aria-label="重试读取成片记录"]');
    expect(retryBtn).toBeTruthy();

    await dom.click(retryBtn!);
    await dom.flush(30);

    expect(jobFetchCount).toBe(2);
    expect(container.querySelector('[role="alert"]')).toBeNull();
    expect(container.textContent).toContain("导出文件（1）");
    await unmount();
  });

  test("listJobs 失败后在恢复 online 或 episode 刷新时自动重试且不死循环", async () => {
    const seg = rawSegment(0, "seg-1", 10);
    useDramaSegmentsStore.setState({
      byEpisode: { [segmentKey("d-1", 1)]: [storeSegment(0, "seg-1", 10)] },
    });

    // 子场景 A：window online 事件触发自动重试
    let onlineJobFetchCount = 0;
    const onlineTarget = backendComposeJob(
      "j-online",
      "clean",
      "https://cdn/online.mp4",
      "2026-11-15T12:00:00Z",
    );
    const getA = spyOn(service, "get").mockImplementation(async (url) => {
      if (url.endsWith("/segments")) return { items: [seg] };
      if (url.endsWith("/manifest")) return rawManifest(240);
      if (url.endsWith("/jobs")) {
        onlineJobFetchCount += 1;
        if (onlineJobFetchCount === 1) throw new Error("offline");
        return { items: [onlineTarget] };
      }
      return { items: [] };
    });

    const { container: cA, unmount: uA } = await mountFinal(
      makeDrama("9:16", { finalComposeJobId: "j-online" }),
      [],
    );
    await dom.flush(30);
    expect(onlineJobFetchCount).toBe(1);
    expect(cA.querySelector('[role="alert"]')).toBeTruthy();

    await dom.act(async () => {
      dom.win.dispatchEvent(new dom.win.Event("online"));
    });
    await dom.flush(30);
    expect(onlineJobFetchCount).toBe(2);
    expect(cA.querySelector('[role="alert"]')).toBeNull();
    expect(cA.textContent).toContain("导出文件（1）");
    await dom.flush(30);
    expect(onlineJobFetchCount).toBe(2);
    await uA();
    getA.mockRestore();

    // 子场景 B：episode.revision / updatedAt 刷新触发自动重试且不死循环
    let refreshJobFetchCount = 0;
    const refreshTarget = backendComposeJob(
      "j-refresh",
      "packaged",
      "https://cdn/refresh.mp4",
      "2026-11-15T12:10:00Z",
    );
    const getB = spyOn(service, "get").mockImplementation(async (url) => {
      if (url.endsWith("/segments")) return { items: [seg] };
      if (url.endsWith("/manifest")) return rawManifest(240);
      if (url.endsWith("/jobs")) {
        refreshJobFetchCount += 1;
        if (refreshJobFetchCount === 1) throw new Error("temporary failure");
        return { items: [refreshTarget] };
      }
      return { items: [] };
    });
    spies.push(getB);

    let updateDrama!: (next: DramaDetail) => void;
    const Wrapper = ({ bind }: { bind: (fn: (next: DramaDetail) => void) => void }) => {
      const [drama, setDrama] = useState(() =>
        makeDrama("9:16", {
          finalComposeJobId: "j-refresh",
          revision: 1,
          updatedAt: "2026-11-15T10:00:00Z",
        }),
      );
      useEffect(() => {
        bind(setDrama);
      }, [bind]);
      const workspace = { drama, jobs: [] } as unknown as DramaWorkspace;
      return createElement(
        DramaWorkspaceContext.Provider,
        { value: workspace },
        createElement(FinalArea, { episode: drama.episodes[0]! }),
      );
    };
    const mounted = await dom.mount(
      createElement(Wrapper, {
        bind: (fn) => {
          updateDrama = fn;
        },
      }),
    );
    cleanups.push(mounted.unmount);

    await dom.flush(30);
    expect(refreshJobFetchCount).toBe(1);
    expect(mounted.container.querySelector('[role="alert"]')).toBeTruthy();

    await dom.act(async () => {
      updateDrama(
        makeDrama("9:16", {
          finalComposeJobId: "j-refresh",
          revision: 2,
          updatedAt: "2026-11-15T12:05:00Z",
        }),
      );
    });
    await dom.flush(30);
    expect(refreshJobFetchCount).toBe(2);
    expect(mounted.container.querySelector('[role="alert"]')).toBeNull();
    expect(mounted.container.textContent).toContain("导出文件（1）");
    await dom.flush(30);
    expect(refreshJobFetchCount).toBe(2);
  });

  test("listJobs 返回 HTTP 200 但暂不含目标 succeeded 任务时禁止自动轮询与兜底其他任务，并显示手动重试按钮", async () => {
    const seg = rawSegment(0, "seg-1", 10);
    useDramaSegmentsStore.setState({
      byEpisode: { [segmentKey("d-1", 1)]: [storeSegment(0, "seg-1", 10)] },
    });

    const otherSucceeded = backendComposeJob(
      "j-other-old",
      "packaged",
      "https://cdn/old.mp4",
      "2026-11-15T11:00:00Z",
    );
    const pendingTarget = {
      ...backendComposeJob(
        "j-missing-200",
        "clean",
        "https://cdn/pending.mp4",
        "2026-11-15T12:00:00Z",
      ),
      status: "running" as const,
      progress: 80,
    };
    const recoveredTarget = backendComposeJob(
      "j-missing-200",
      "packaged",
      "https://cdn/ready.mp4",
      "2026-11-15T12:01:00Z",
    );

    let jobFetchCount = 0;
    const get = spyOn(service, "get").mockImplementation(async (url) => {
      if (url.endsWith("/segments")) return { items: [seg] };
      if (url.endsWith("/manifest")) return rawManifest(240);
      if (url.endsWith("/jobs")) {
        jobFetchCount += 1;
        if (jobFetchCount === 1) {
          return { items: [otherSucceeded, pendingTarget] };
        }
        return { items: [recoveredTarget] };
      }
      return { items: [] };
    });
    spies.push(get);

    const { container, unmount } = await mountFinal(
      makeDrama("9:16", { finalComposeJobId: "j-missing-200" }),
      [],
    );
    await dom.flush(60);

    // HTTP 200 但目标未就绪：不自动轮询，也不兜底展示其他任务产物
    expect(jobFetchCount).toBe(1);
    expect(container.textContent).not.toContain("导出文件（1）");
    expect(container.querySelector('[aria-label="本次导出结果"]')).toBeNull();
    expect(container.querySelector('[role="alert"]')).toBeNull();
    expect(container.querySelector('[role="status"]')).toBeTruthy();
    const retryBtn = container.querySelector<HTMLButtonElement>('[aria-label="重试读取成片记录"]');
    expect(retryBtn).toBeTruthy();

    // 恢复网络事件不触发 200 缺席态自动重试（仍由用户手动触发）
    await dom.act(async () => {
      dom.win.dispatchEvent(new dom.win.Event("online"));
    });
    await dom.flush(30);
    expect(jobFetchCount).toBe(1);

    // 手动点击重试后重新请求一次并恢复目标成片导出
    await dom.click(retryBtn!);
    await dom.flush(30);
    expect(jobFetchCount).toBe(2);
    expect(container.querySelector('[aria-label="重试读取成片记录"]')).toBeNull();
    expect(container.textContent).toContain("导出文件（1）");
    const exportsBox = container.querySelector('[aria-label="本次导出结果"]');
    const previewExportBtn = Array.from(exportsBox?.querySelectorAll("button") ?? []).find((b) =>
      b.textContent?.includes("预览"),
    );
    expect(previewExportBtn).toBeTruthy();
    await dom.click(previewExportBtn!);
    await dom.flush(20);
    expect(exportsBox?.textContent).toContain("切回成片");
    const playBtn = container.querySelector<HTMLButtonElement>('button[aria-label="播放视频"]');
    expect(playBtn).toBeTruthy();
    await dom.click(playBtn!);
    await dom.flush(20);
    expect(container.querySelector("video")?.getAttribute("src")).toBe("https://cdn/ready.mp4");
    await unmount();
  });

  test("FinalArea 旧版合成路径保留瞬时失败幂等键，切换分镜素材或比例时轮换 key 且不发送 compose", async () => {
    const rawSeg1: BackendSegmentView = {
      ...rawSegment(0, "seg-1", 10),
      chosen_task_id: "task-seg-1-v1",
      takes: [
        {
          task_id: "task-seg-1-v1",
          status: "succeeded",
          error_message: "",
          created_at: "2026-11-15T10:00:00Z",
          asset: {
            id: 101,
            kind: "video",
            url: "https://cdn.example.com/seg-1-v1.mp4",
            mime_type: "video/mp4",
            byte_size: 1024,
            width: 720,
            height: 1280,
            duration_ms: 10000,
            file_name: "seg-1-v1.mp4",
          },
        },
        {
          task_id: "task-seg-1-v2",
          status: "succeeded",
          error_message: "",
          created_at: "2026-11-15T10:01:00Z",
          asset: {
            id: 102,
            kind: "video",
            url: "https://cdn.example.com/seg-1-v2.mp4",
            mime_type: "video/mp4",
            byte_size: 1024,
            width: 720,
            height: 1280,
            duration_ms: 10000,
            file_name: "seg-1-v2.mp4",
          },
        },
      ],
    };
    const storeSeg1V1: Segment = {
      ...storeSegment(0, "seg-1", 10),
      chosenTaskId: "task-seg-1-v1",
      takes: [
        {
          taskId: "task-seg-1-v1",
          status: "succeeded",
          errorMessage: "",
          createdAt: "2026-11-15T10:00:00Z",
          asset: {
            id: "101",
            kind: "video",
            url: "https://cdn.example.com/seg-1-v1.mp4",
            mimeType: "video/mp4",
            byteSize: 1024,
            width: 720,
            height: 1280,
            durationMs: 10000,
            fileName: "seg-1-v1.mp4",
          },
        },
        {
          taskId: "task-seg-1-v2",
          status: "succeeded",
          errorMessage: "",
          createdAt: "2026-11-15T10:01:00Z",
          asset: {
            id: "102",
            kind: "video",
            url: "https://cdn.example.com/seg-1-v2.mp4",
            mimeType: "video/mp4",
            byteSize: 1024,
            width: 720,
            height: 1280,
            durationMs: 10000,
            fileName: "seg-1-v2.mp4",
          },
        },
      ],
    };
    const storeSeg1V2: Segment = { ...storeSeg1V1, chosenTaskId: "task-seg-1-v2" };
    useDramaSegmentsStore.setState({
      byEpisode: { [segmentKey("d-1", 1)]: [storeSeg1V1] },
    });

    const get = spyOn(service, "get").mockImplementation(async (url) => {
      if (url.endsWith("/segments")) return { items: [rawSeg1] };
      return { items: [] };
    });
    const post = spyOn(service, "post")
      .mockRejectedValueOnce(new ApiError("unavailable-1", 503, 503))
      .mockRejectedValueOnce(new ApiError("unavailable-2", 503, 503))
      .mockResolvedValueOnce(
        backendComposeJob(
          "j-legacy-3",
          "clean",
          "https://cdn/legacy-3.mp4",
          "2026-11-15T10:05:00Z",
          "queued",
        ),
      )
      .mockResolvedValueOnce(
        backendComposeJob(
          "j-legacy-4",
          "clean",
          "https://cdn/legacy-4.mp4",
          "2026-11-15T10:06:00Z",
          "queued",
        ),
      );
    const toastErr = spyOn(toast, "error").mockImplementation(() => "");
    spies.push(get, post, toastErr);

    let setDramaState!: (next: DramaDetail) => void;
    const Wrapper = ({ bind }: { bind: (fn: (next: DramaDetail) => void) => void }) => {
      const [drama, setDrama] = useState(() =>
        makeDrama("9:16", { editSchema: 0, segmentVersion: 1, finalComposeJobId: null }),
      );
      useEffect(() => {
        bind(setDrama);
      }, [bind]);
      const workspace = { drama, jobs: [] } as unknown as DramaWorkspace;
      return createElement(
        DramaWorkspaceContext.Provider,
        { value: workspace },
        createElement(FinalArea, { episode: drama.episodes[0]! }),
      );
    };
    const { container, unmount } = await dom.mount(
      createElement(Wrapper, {
        bind: (fn) => {
          setDramaState = fn;
        },
      }),
    );
    cleanups.push(unmount);
    await dom.flush(20);

    const findSubmit = () =>
      Array.from(container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("合成第 1 集"),
      );

    // 1. 首次提交 503
    await dom.click(findSubmit()!);
    await dom.flush(15);
    // 2. 条件未变原样重试 -> 复用相同幂等键
    await dom.click(findSubmit()!);
    await dom.flush(15);

    // 3. 切换分镜 adopted take -> 轮换新 key
    await dom.act(async () => {
      useDramaSegmentsStore.setState({
        byEpisode: { [segmentKey("d-1", 1)]: [storeSeg1V2] },
      });
    });
    await dom.flush(10);
    await dom.click(findSubmit()!);
    await dom.flush(15);

    // 4. 变更比例与 segmentVersion -> 轮换新 key
    await dom.act(async () => {
      setDramaState(
        makeDrama("16:9", { editSchema: 0, segmentVersion: 2, finalComposeJobId: null }),
      );
    });
    await dom.flush(10);
    await dom.click(findSubmit()!);
    await dom.flush(15);

    expect(post).toHaveBeenCalledTimes(4);
    const payloads = post.mock.calls.map(
      (call) => call[1] as { compose_request_key: string; compose?: unknown },
    );
    for (const payload of payloads) {
      expect("compose" in payload).toBe(false);
    }
    const keys = payloads.map((p) => p.compose_request_key);
    expect(keys[1]).toBe(keys[0]);
    expect(keys[2]).not.toBe(keys[0]);
    expect(keys[3]).not.toBe(keys[2]);
    expect(keys[3]).not.toBe(keys[0]);
  });

  test("16:9 首次 GET /program 失败时不按无音频降级，重试后切换到 manifest?v=2；91001 过期与手动刷新也会重探节目单", async () => {
    const seg = rawSegment(0, "seg-1", 10);
    useDramaSegmentsStore.setState({
      byEpisode: { [segmentKey("d-1", 1)]: [storeSegment(0, "seg-1", 10)] },
    });

    // 子场景 A：首次 GET /program 瞬态失败 -> 阻断盲目 V1 提交，点击重试后发现节目音频并切换 manifest?v=2
    let programAttempt = 0;
    const getA = spyOn(service, "get").mockImplementation(async (url: string) => {
      if (url.endsWith("/segments")) return { items: [seg] };
      if (url.endsWith("/program")) {
        programAttempt += 1;
        if (programAttempt === 1) throw new ApiError("temporary program failure", 500, 500);
        return {
          episode_id: "ep-1",
          segment_version: 2,
          edit_schema: 1,
          program: {
            v: 1,
            audio: [{ id: "bgm-1", role: "bgm", asset_id: 99, start_f: 0, end_f: 24 }],
            overlays: [],
          },
        };
      }
      if (url.endsWith("/manifest?v=2")) {
        return {
          ...rawManifest(120, 120),
          manifest_hash: "e".repeat(64),
          manifest: {
            ...rawManifest(120, 120).manifest,
            episode_id: "ep-1",
            program: {
              v: 1,
              audio: [{ id: "bgm-1", role: "bgm", asset_id: 99, start_f: 0, end_f: 24 }],
              overlays: [],
            },
          },
          program_diagnostics: [],
        };
      }
      throw new Error(`unexpected GET ${url}`);
    });
    const postA = spyOn(service, "post").mockResolvedValueOnce(
      backendComposeJob("j-v2", "clean", "https://cdn/v2.mp4", "2026-11-15T10:05:00Z", "queued"),
    );
    spies.push(getA, postA);

    const dramaA = makeDrama("16:9", {
      id: "ep-1",
      editSchema: 1,
      segmentVersion: 2,
      finalComposeJobId: null,
    });
    const { container: cA, unmount: uA } = await mountFinal(dramaA, []);
    await dom.flush(20);

    expect(cA.textContent).toContain("读取节目单状态失败");
    expect(getA.mock.calls.map(([u]) => u)).not.toContain("/dramas/d-1/episodes/1/manifest");
    const submitA = () =>
      Array.from(cA.querySelectorAll("button")).find((b) => b.textContent?.includes("合成第 1 集"));
    expect(submitA()?.disabled).toBe(true);

    const retryProgramBtn = cA.querySelector<HTMLButtonElement>('[aria-label="重试读取节目单"]');
    expect(retryProgramBtn).toBeTruthy();
    await dom.click(retryProgramBtn!);
    await dom.flush(20);

    expect(getA.mock.calls.map(([u]) => u)).toContain("/dramas/d-1/episodes/1/manifest?v=2");
    expect(submitA()?.disabled).toBe(false);
    await dom.click(submitA()!);
    await dom.flush(10);
    expect(postA).toHaveBeenCalledTimes(1);
    expect(postA.mock.calls[0]?.[1]).toMatchObject({
      compose: { manifest_hash: "e".repeat(64), segment_version: 2 },
    });
    await uA();
    getA.mockRestore();
    postA.mockRestore();

    // 子场景 B：初始程序为空（V1），远端保存节目音频后导出 91001 过期，自动及点击“刷新”重新探测 /program 并切换至 manifest?v=2
    let remoteHasAudio = false;
    let v2Fetches = 0;
    const getB = spyOn(service, "get").mockImplementation(async (url: string) => {
      if (url.endsWith("/segments")) return { items: [seg] };
      if (url.endsWith("/program")) {
        return {
          episode_id: "ep-1",
          segment_version: 2,
          edit_schema: 1,
          program: {
            v: 1,
            audio: remoteHasAudio
              ? [{ id: "bgm-2", role: "bgm", asset_id: 100, start_f: 0, end_f: 24 }]
              : [],
            overlays: [],
          },
        };
      }
      if (url.endsWith("/manifest")) return rawManifest(120, 120);
      if (url.endsWith("/manifest?v=2")) {
        v2Fetches += 1;
        return {
          ...rawManifest(120, 120),
          manifest_hash: v2Fetches === 1 ? "f".repeat(64) : "0".repeat(64),
          manifest: {
            ...rawManifest(120, 120).manifest,
            episode_id: "ep-1",
            program: {
              v: 1,
              audio: [{ id: "bgm-2", role: "bgm", asset_id: 100, start_f: 0, end_f: 24 }],
              overlays: [],
            },
          },
          program_diagnostics: [],
        };
      }
      throw new Error(`unexpected GET ${url}`);
    });
    const postB = spyOn(service, "post")
      .mockRejectedValueOnce(new ApiError("stale", DRAMA_ERR.manifestStale, 409))
      .mockResolvedValueOnce(
        backendComposeJob(
          "j-v2-b",
          "clean",
          "https://cdn/v2b.mp4",
          "2026-11-15T10:06:00Z",
          "queued",
        ),
      );
    spies.push(getB, postB);

    const dramaB = makeDrama("16:9", {
      id: "ep-1",
      editSchema: 1,
      segmentVersion: 2,
      finalComposeJobId: null,
    });
    const { container: cB, unmount: uB } = await mountFinal(dramaB, []);
    await dom.flush(20);

    remoteHasAudio = true;
    const submitB = Array.from(cB.querySelectorAll("button")).find((b) =>
      b.textContent?.includes("合成第 1 集"),
    )!;
    await dom.click(submitB);
    await dom.flush(20);

    expect(v2Fetches).toBe(1);
    const refreshBtn = Array.from(cB.querySelectorAll("button")).find(
      (b) => b.textContent?.trim() === "刷新",
    );
    expect(refreshBtn).toBeTruthy();
    await dom.click(refreshBtn!);
    await dom.flush(20);
    expect(v2Fetches).toBe(2);

    await dom.click(submitB);
    await dom.flush(10);
    expect(postB).toHaveBeenCalledTimes(2);
    expect(postB.mock.calls[1]?.[1]).toMatchObject({
      compose: { manifest_hash: "0".repeat(64), segment_version: 2 },
    });
    await uB();
  });

  test("切换分集后上一集迟到的 91001 过期回调不得取消当前分集的节目单读取与合成", async () => {
    const seg1 = storeSegment(0, "seg-1", 10);
    const seg2 = { ...storeSegment(0, "seg-2", 10), episodeNo: 2 };
    useDramaSegmentsStore.setState({
      byEpisode: {
        [segmentKey("d-1", 1)]: [seg1],
        [segmentKey("d-1", 2)]: [seg2],
      },
    });

    const ep2Program = Promise.withResolvers<object>();
    const ep1Compose = Promise.withResolvers<object>();
    const get = spyOn(service, "get").mockImplementation(async (url: string) => {
      if (url.endsWith("/episodes/1/segments")) return { items: [rawSegment(0, "seg-1", 10)] };
      if (url.endsWith("/episodes/2/segments"))
        return { items: [{ ...rawSegment(0, "seg-2", 10), episode_no: 2 }] };
      if (url.endsWith("/episodes/1/program")) {
        return {
          episode_id: "ep-1",
          segment_version: 2,
          edit_schema: 1,
          program: { v: 1, audio: [], overlays: [] },
        };
      }
      if (url.endsWith("/episodes/2/program")) return ep2Program.promise;
      if (url.endsWith("/episodes/1/manifest")) return rawManifest(120, 120);
      if (url.endsWith("/episodes/2/manifest")) return rawManifest(120, 120);
      throw new Error(`unexpected GET ${url}`);
    });
    const post = spyOn(service, "post").mockImplementation(async (_url, body) => {
      const episodes = (body as { episodes: number[] }).episodes;
      if (episodes[0] === 1) return ep1Compose.promise;
      return backendComposeJob(
        "j-ep2",
        "clean",
        "https://cdn/ep2.mp4",
        "2026-11-15T10:07:00Z",
        "queued",
      );
    });
    spies.push(get, post);

    const drama: DramaDetail = {
      ...makeDrama("16:9", {
        id: "ep-1",
        no: 1,
        editSchema: 1,
        segmentVersion: 2,
        finalComposeJobId: null,
      }),
      episodes: [
        makeEpisode({
          id: "ep-1",
          no: 1,
          title: "第一集",
          editSchema: 1,
          segmentVersion: 2,
          finalComposeJobId: null,
        }),
        makeEpisode({
          id: "ep-2",
          no: 2,
          title: "第二集",
          editSchema: 1,
          segmentVersion: 2,
          finalComposeJobId: null,
        }),
      ],
    };

    function SwitchHarness() {
      const [epIndex, setEpIndex] = useState(0);
      const workspace = { drama, jobs: [] } as unknown as DramaWorkspace;
      return createElement(
        DramaWorkspaceContext.Provider,
        { value: workspace },
        createElement(
          "div",
          null,
          createElement("button", { type: "button", onClick: () => setEpIndex(1) }, "切到第2集"),
          createElement(FinalArea, { ep: epIndex + 1, onGo: () => {} }),
        ),
      );
    }

    const { container, unmount } = await dom.mount(createElement(SwitchHarness));
    try {
      await dom.flush(20);
      const submit1 = Array.from(container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("合成第 1 集"),
      )!;
      expect(submit1.disabled).toBe(false);
      await dom.click(submit1);
      await dom.flush(10);

      const switchBtn = Array.from(container.querySelectorAll("button")).find(
        (b) => b.textContent?.trim() === "切到第2集",
      )!;
      await dom.click(switchBtn);
      await dom.flush(10);

      // 分集 1 迟到的 91001 响应在分集 2 GET /program 仍挂起时到达
      await dom.act(async () => {
        ep1Compose.reject(new ApiError("stale", DRAMA_ERR.manifestStale, 409));
      });
      await dom.flush(15);

      // 分集 2 GET /program 正常返回，不应被分集 1 的 91001 取消而永久 pending
      await dom.act(async () => {
        ep2Program.resolve({
          episode_id: "ep-2",
          segment_version: 2,
          edit_schema: 1,
          program: { v: 1, audio: [], overlays: [] },
        });
      });
      await dom.flush(20);

      expect(container.textContent).toContain("合成第 2 集");
      const submit2 = Array.from(container.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("合成第 2 集"),
      )!;
      expect(submit2).toBeTruthy();
      expect(submit2.disabled).toBe(false);
      await dom.click(submit2);
      await dom.flush(15);
      expect(post).toHaveBeenCalledTimes(2);
      expect(post.mock.calls[1]?.[1]).toMatchObject({ episodes: [2] });
    } finally {
      await unmount();
    }
  });

  test("本地 schema=0 分集已被服务端替换为新 episode_id 时，阻断导出、提示并触发工作台刷新后再允许提交", async () => {
    const seg1 = storeSegment(0, "seg-1", 10);
    useDramaSegmentsStore.setState({
      byEpisode: { [segmentKey("d-1", 1)]: [seg1] },
    });

    let serverEpId = "ep-new";
    let refreshCalls = 0;
    let allowRefreshUpdate = false;
    const handleRefresh = () => {
      refreshCalls += 1;
      return allowRefreshUpdate ? serverEpId : null;
    };

    const get = spyOn(service, "get").mockImplementation(async (url: string) => {
      if (url.endsWith("/episodes/1/segments")) return { items: [rawSegment(0, "seg-1", 10)] };
      if (url.endsWith("/episodes/1/program")) {
        return {
          episode_id: serverEpId,
          segment_version: 2,
          edit_schema: 0,
          program: { v: 1, audio: [], overlays: [] },
        };
      }
      throw new Error(`unexpected GET ${url}`);
    });
    const post = spyOn(service, "post").mockResolvedValue(
      backendComposeJob("j-ok", "clean", "https://cdn/ok.mp4", "2026-11-15T10:08:00Z", "queued"),
    );
    spies.push(get, post);

    function StaleEpisodeHarness({ onRefresh }: { onRefresh: () => string | null }) {
      const [drama, setDrama] = useState<DramaDetail>(() =>
        makeDrama("9:16", {
          id: "ep-old",
          no: 1,
          title: "旧版第1集",
          editSchema: 0,
          segmentVersion: 1,
          finalComposeJobId: null,
        }),
      );
      const workspace = {
        drama,
        jobs: [],
        refresh: async () => {
          const nextId = onRefresh();
          if (nextId) {
            setDrama(
              makeDrama("9:16", {
                id: nextId,
                no: 1,
                title: "替换后第1集",
                editSchema: 0,
                segmentVersion: 2,
                finalComposeJobId: null,
              }),
            );
          }
        },
      } as unknown as DramaWorkspace;
      return createElement(
        DramaWorkspaceContext.Provider,
        { value: workspace },
        createElement(FinalArea, { ep: 1, onGo: () => {} }),
      );
    }

    // 子场景 A：如果自动 refresh 暂未替换本地旧 episode（如 refresh 返回暂未变更），按钮必须保持禁用且零提交
    const { container, unmount } = await dom.mount(
      createElement(StaleEpisodeHarness, { onRefresh: handleRefresh }),
    );
    try {
      await dom.flush(20);
      expect(refreshCalls).toBe(1);
      expect(container.textContent).toContain("当前分集内容已更新或被替换，请刷新工作台后重试");
      const submitBtn = () =>
        Array.from(container.querySelectorAll("button")).find((b) =>
          b.textContent?.includes("合成第 1 集"),
        )!;
      expect(submitBtn().disabled).toBe(true);
      await dom.click(submitBtn());
      await dom.flush(10);
      expect(post).not.toHaveBeenCalled();

      // 手动点击「刷新工作台」且工作台拿到新 episode_id 后恢复可用
      allowRefreshUpdate = true;
      const refreshBtn = container.querySelector<HTMLButtonElement>('[aria-label="刷新工作台"]');
      expect(refreshBtn).toBeTruthy();
      await dom.click(refreshBtn!);
      await dom.flush(20);
      expect(container.textContent).toContain("替换后第1集");
      expect(submitBtn().disabled).toBe(false);
    } finally {
      await unmount();
    }

    // 子场景 B：挂载时一致，但在点击导出前服务端替换 episode_id，legacy compose 提交前复核拦截且不发 POST /jobs
    serverEpId = "ep-old";
    const { container: cB, unmount: uB } = await dom.mount(
      createElement(StaleEpisodeHarness, { onRefresh: handleRefresh }),
    );
    try {
      await dom.flush(20);
      expect(containerText(cB)).toContain("旧版第1集");
      serverEpId = "ep-replaced-before-click";
      const submitB = Array.from(cB.querySelectorAll("button")).find((b) =>
        b.textContent?.includes("合成第 1 集"),
      )!;
      await dom.click(submitB);
      await dom.flush(20);
      expect(post).not.toHaveBeenCalled();
    } finally {
      await uB();
    }
  });
});

const containerText = (el: HTMLElement) => el.textContent ?? "";
