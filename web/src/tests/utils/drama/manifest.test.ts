import { expect, spyOn, test } from "bun:test";

import { mapJob } from "@/api/drama";
import type { BackendComposeExport, BackendJobView } from "@/api/drama/type";
import service from "@/utils/requests/service";
import { waitForEpisodeManifestV2 } from "@/utils/drama/manifest";

const preparing = { status: "preparing", segment_version: 4 } as const;

test("V2 polls preparing responses and stops at a terminal program diagnostic", async () => {
  const diagnostic = {
    cue_id: "hit",
    field: "anchor",
    reason: "anchor_source_replaced",
    severity: "error",
  };
  const get = spyOn(service, "get")
    .mockResolvedValueOnce(preparing)
    .mockResolvedValueOnce(preparing)
    .mockResolvedValueOnce({
      status: "ready",
      segment_version: 4,
      manifest_hash: "",
      program_diagnostics: [diagnostic],
    });
  const waits: number[] = [];
  try {
    const result = await waitForEpisodeManifestV2("drama", 2, {
      sleep: async (ms) => {
        waits.push(ms);
        return true;
      },
    });
    expect(result).toMatchObject({
      kind: "ready",
      manifest: { manifestHash: "", segmentVersion: 4, programDiagnostics: [diagnostic] },
    });
    expect(waits).toEqual([200, 400]);
    expect(get.mock.calls.map(([path]) => path)).toEqual(
      Array(3).fill("/dramas/drama/episodes/2/manifest?v=2"),
    );
  } finally {
    get.mockRestore();
  }
});

test("V2 aborts an in-flight request", async () => {
  const get = spyOn(service, "get").mockImplementation(() => new Promise(() => {}));
  try {
    const controller = new AbortController();
    const waiting = waitForEpisodeManifestV2("drama", 2, { signal: controller.signal });
    const requestSignal = get.mock.calls[0]?.[2]?.signal;
    controller.abort();
    expect(await waiting).toEqual({ kind: "cancelled" });
    expect(requestSignal?.aborted).toBe(true);
  } finally {
    get.mockRestore();
  }
});

test("V2 times out at the deadline and respects the attempt limit", async () => {
  const get = spyOn(service, "get").mockImplementation(() => new Promise(() => {}));
  try {
    expect(await waitForEpisodeManifestV2("drama", 2, { maxWaitMs: 20 })).toEqual({
      kind: "timeout",
    });
    expect(get.mock.calls[0]?.[2]?.signal?.aborted).toBe(true);
    get.mockResolvedValue(preparing);
    expect(await waitForEpisodeManifestV2("drama", 2, { maxAttempts: 1 })).toEqual({
      kind: "timeout",
    });
    expect(get).toHaveBeenCalledTimes(2);
  } finally {
    get.mockRestore();
  }
});

test("maps flat V2 export reports without inventing measurements on V1 exports", () => {
  const rawExport = {
    sha256: "video-hash",
    variant: "clean",
    size: "720p",
    width: 1280,
    height: 720,
    manifest_hash: "manifest-hash",
    asset: { id: 30, kind: "video", url: "/output" },
  } satisfies BackendComposeExport;
  const rawJob = {
    id: "compose-job",
    step: "compose",
    episodes: [2],
    status: "succeeded",
    progress: 100,
    credits: 0,
    error_code: 0,
    error_message: "",
    created_at: "",
    finished_at: null,
    result: {
      asset: rawExport.asset,
      skipped_seqs: [],
      exports: [
        rawExport,
        {
          ...rawExport,
          audio_sha256: "audio-hash",
          lufs: null,
          true_peak: null,
          loudness_status: "silence",
        },
      ],
    },
  } satisfies BackendJobView;
  const exports = mapJob(rawJob).result?.exports;
  expect(exports?.[0]).not.toHaveProperty("lufs");
  expect(exports?.[1]).toMatchObject({
    audioSha256: "audio-hash",
    lufs: null,
    truePeak: null,
    loudnessStatus: "silence",
  });
});
