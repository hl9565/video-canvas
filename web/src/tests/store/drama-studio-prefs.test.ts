import { afterEach, beforeEach, describe, expect, test } from "bun:test";

import { overridesForModel, readPrefs, useStudioPrefsStore } from "@/store/drama-studio";

/** bun 的测试环境没有 localStorage，放一个最小的内存实现 */
const memory = new Map<string, string>();
const fakeStorage = {
  getItem: (key: string) => memory.get(key) ?? null,
  setItem: (key: string, value: string) => void memory.set(key, value),
};

beforeEach(() => {
  memory.clear();
  (globalThis as { localStorage?: unknown }).localStorage = fakeStorage;
  useStudioPrefsStore.setState({ byDrama: {} });
});
afterEach(() => {
  delete (globalThis as { localStorage?: unknown }).localStorage;
});

describe("片段工作区偏好（按短剧存在本机）", () => {
  test("没存过：空偏好", () => {
    expect(readPrefs("d1")).toEqual({ videoModel: "", overrides: {} });
  });

  test("旧版本存过首帧模型：读出来不再带它", () => {
    memory.set("drama-studio:d1", JSON.stringify({ videoModel: "veo", imageModel: "flux" }));
    expect(readPrefs("d1")).toEqual({ videoModel: "veo", overrides: {} });
  });

  test("patch 写进内存与本机，下次读得到；不同短剧互不影响", () => {
    useStudioPrefsStore.getState().patch("d1", { videoModel: "veo" });
    useStudioPrefsStore.getState().patch("d1", { overrides: { resolution: "1080P" } });
    expect(useStudioPrefsStore.getState().get("d1")).toEqual({
      videoModel: "veo",
      overrides: { resolution: "1080P" },
    });
    expect(readPrefs("d1").videoModel).toBe("veo");
    expect(readPrefs("d2").videoModel).toBe("");
  });

  test("存的内容坏了或被改成别的形状：当没存过，不抛", () => {
    memory.set("drama-studio:d1", "{不是 json");
    expect(readPrefs("d1").videoModel).toBe("");
    memory.set("drama-studio:d1", JSON.stringify({ videoModel: 3, overrides: [1] }));
    expect(readPrefs("d1")).toEqual({ videoModel: "", overrides: {} });
  });

  test("localStorage 不可用（隐私模式）：读写都不抛", () => {
    delete (globalThis as { localStorage?: unknown }).localStorage;
    expect(readPrefs("d1").videoModel).toBe("");
    expect(() => useStudioPrefsStore.getState().patch("d1", { videoModel: "x" })).not.toThrow();
    expect(useStudioPrefsStore.getState().get("d1").videoModel).toBe("x");
  });

  test("modelOverrides 按模型隔离，旧版扁平 overrides 只一次性迁给初始模型且切模型不互覆盖", () => {
    // 旧版 localStorage：videoModel="" 表示沿用项目模型 kling，且带有扁平 overrides
    memory.set(
      "drama-studio:d1",
      JSON.stringify({ videoModel: "", overrides: { resolution: "1080P" } }),
    );
    const store = useStudioPrefsStore.getState();
    // 切换模型前应把旧版 overrides 认领给原模型 kling，并清空扁平 overrides
    store.setVideoModel("d1", "veo", "kling");
    const afterSwitch = useStudioPrefsStore.getState().get("d1");
    expect(afterSwitch.videoModel).toBe("veo");
    expect(afterSwitch.overrides).toEqual({});
    expect(overridesForModel(afterSwitch, "kling")).toEqual({ resolution: "1080P" });
    expect(overridesForModel(afterSwitch, "veo")).toEqual({});

    // 为 veo 与 third 分别写入参数覆盖，嵌套更新不得冲掉已保存的模型覆盖
    store.setModelOverride("d1", "veo", "resolution", "720P", "kling");
    store.setModelOverride("d1", "third", "generate_audio", true, "veo");
    const finalPrefs = useStudioPrefsStore.getState().get("d1");
    expect(overridesForModel(finalPrefs, "kling")).toEqual({ resolution: "1080P" });
    expect(overridesForModel(finalPrefs, "veo")).toEqual({ resolution: "720P" });
    expect(overridesForModel(finalPrefs, "third")).toEqual({ generate_audio: true });

    // 删除某个覆盖键
    store.setModelOverride("d1", "veo", "resolution", undefined);
    expect(overridesForModel(useStudioPrefsStore.getState().get("d1"), "veo")).toEqual({});
    expect(overridesForModel(useStudioPrefsStore.getState().get("d1"), "kling")).toEqual({
      resolution: "1080P",
    });
  });
});
