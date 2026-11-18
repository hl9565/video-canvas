import { describe, expect, spyOn, test } from "bun:test";

import type { ProgramAudioCueInput, Segment } from "@/api/drama/type";
import {
  clampPreviewRange,
  programTimeline,
  resolveCue,
  validateProgram,
} from "@/utils/drama/program";

const segment = (id: string, seq: number, frames: number): Segment => ({
  id,
  seq,
  visualKind: "black",
  seconds: 5,
  episodeNo: 1,
  prompt: "",
  takes: [],
  chosenTaskId: "",
  status: "idle",
  revision: 1,
  updatedAt: "",
  adaptations: {},
  edit: {
    sourceInF: 0,
    sourceOutF: frames,
    muteOriginal: false,
    nativeIsDialogue: true,
    audioGainDb: 0,
    avPtsOffsetMs: 0,
  },
});

describe("节目单的母版坐标", () => {
  test("黑场进入母版但没有原声；剪辑仅移动视频锚点，绝对节目条目保持原位", () => {
    const first = segment("black", 0, 12);
    const second: Segment = {
      ...segment("video", 1, 36),
      visualKind: "video",
      chosenTaskId: "take",
      edit: { ...segment("video", 1, 36).edit!, sourceInF: 12 },
      takes: [
        {
          taskId: "take",
          status: "succeeded",
          errorMessage: "",
          createdAt: "",
          asset: {
            id: "42",
            kind: "video",
            url: "video.mp4",
            mimeType: "video/mp4",
            byteSize: 10,
            width: 1280,
            height: 720,
            durationMs: 3000,
            fileName: "video.mp4",
          },
        },
      ],
    };
    const anchored: ProgramAudioCueInput = {
      id: "hit",
      role: "sfx",
      asset_id: 1,
      anchor: { segment_id: "video", source_f: 20 },
      length_f: 4,
    };
    const absolute: ProgramAudioCueInput = {
      id: "voice",
      role: "narration",
      asset_id: 2,
      start_f: 8,
      end_f: 16,
    };
    const timeline = programTimeline([second, first]);
    expect(timeline.totalFrames).toBe(36);
    expect(timeline.clips.map((clip) => clip.nativeDialogue)).toEqual([false, true]);
    expect(resolveCue(anchored, timeline)).toEqual({ inF: 20, outF: 24 });
    const shifted = programTimeline([segment("black", 0, 24), second]);
    expect(resolveCue(anchored, shifted)).toEqual({ inF: 32, outF: 36 });
    expect(resolveCue(absolute, shifted)).toEqual({ inF: 8, outF: 16 });
    expect(clampPreviewRange({ inF: 99, outF: -2 }, timeline.totalFrames)).toEqual({
      inF: 35,
      outF: 36,
    });
  });

  test("同类非音效不能重叠，配乐和环境底噪可以占用同一区间", () => {
    const bgm: ProgramAudioCueInput = {
      id: "music",
      role: "bgm",
      asset_id: 1,
      start_f: 0,
      end_f: 120,
    };
    const room: ProgramAudioCueInput = {
      ...bgm,
      id: "room",
      role: "room_tone",
    };
    const sfx: ProgramAudioCueInput = {
      id: "sfx",
      role: "sfx",
      asset_id: 2,
      start_f: 0,
      end_f: 120,
    };
    const timeline = programTimeline([segment("black", 0, 120)]);
    expect(validateProgram([bgm, room, sfx, { ...sfx, id: "sfx-2" }], timeline)).toEqual([]);
    for (const cue of [bgm, room, { ...bgm, role: "narration" } as const]) {
      expect(validateProgram([cue, { ...cue, id: "duplicate" }], timeline)).toMatchObject([
        { cue_id: "duplicate", field: "range" },
      ]);
    }
  });
});

test("节目单视图保持数字字段，同时能按服务端原始十进制资产 ID 写回", async () => {
  const { getEpisodeProgram, programCueAssetId } = await import("@/api/drama");
  const { default: service } = await import("@/utils/requests/service");
  const get = spyOn(service, "get").mockImplementation(async (_url, _params, options) =>
    JSON.parse(
      '{"episode_id":"ep","segment_version":1,"edit_schema":1,"program":{"v":1,"audio":[{"id":"voice","role":"narration","asset_id":9007199254740993,"start_f":0,"end_f":6}],"overlays":[]}}',
      options?.parseReviver,
    ),
  );
  try {
    const { program } = await getEpisodeProgram("d", 1);
    expect(typeof program.audio[0]?.asset_id).toBe("number");
    expect(programCueAssetId(program.audio[0]!)).toBe("9007199254740993");
  } finally {
    get.mockRestore();
  }
});
