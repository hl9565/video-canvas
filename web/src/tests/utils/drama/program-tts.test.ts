import { expect, test } from "bun:test";

import type { Capabilities } from "@/api/model/type";
import { buildProgramTtsInput, programTtsAsset } from "@/utils/drama/program-tts";
import { makeTask } from "../tasks/fixtures";

const caps: Capabilities = {
  ops: [],
  refs: {
    image: { on: false, max: 0, max_mb: 0 },
    video: { on: false, max: 0, max_mb: 0 },
    audio: { on: false, max: 0, max_mb: 0 },
  },
  prompt: { max_length: 3 },
  params: {},
};

test("配音只提交模型声明的字段并按字符校验台词", () => {
  expect(buildProgramTtsInput(caps, " 中😀 ", "speaker").input).toEqual({ prompt: "中😀" });
  expect(buildProgramTtsInput(caps, "超过上限").error).toBeTruthy();
  expect(
    buildProgramTtsInput(
      {
        ...caps,
        params: { speaker: { type: "enum", label: "音色", options: ["a", "b"], open: true } },
      },
      "你好",
      "b",
    ).input,
  ).toEqual({ prompt: "你好", speaker: "b" });
});

test("配音只采用成功任务中持久化的音频 ID，优先使用精确十进制串", () => {
  const output = {
    media_type: "audio",
    asset_id: 9007199254740992,
    asset_id_str: "9007199254740993",
    url: "https://provider.invalid/tmp",
  };
  expect(programTtsAsset(makeTask({ status: "succeeded", outputs: [output] }))).toEqual({
    assetId: "9007199254740993",
    durationFrames: null,
  });
  expect(programTtsAsset(makeTask({ status: "canceled", outputs: [output] }))).toBeNull();
  expect(
    programTtsAsset(
      makeTask({ status: "succeeded", outputs: [{ media_type: "audio", url: output.url }] }),
    ),
  ).toBeNull();
});
