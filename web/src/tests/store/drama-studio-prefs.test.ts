import { afterEach, beforeEach, describe, expect, test } from "bun:test";

import {
  overridesForModel,
  readPrefs,
  STICKY_DRAFT_KEYS,
  stripStickyDraftKeys,
  useStudioPrefsStore,
} from "@/store/drama-studio";

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

  test("patch 写进内存与本机，下次读得到；不同短剧互不影响，粘滞键不入共享层", () => {
    useStudioPrefsStore.getState().patch("d1", { videoModel: "veo" });
    useStudioPrefsStore
      .getState()
      .patch("d1", { overrides: { generate_audio: true, resolution: "1080P", count: 2 } });
    expect(useStudioPrefsStore.getState().get("d1")).toEqual({
      videoModel: "veo",
      overrides: { generate_audio: true },
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

  test("modelOverrides 按模型隔离非粘滞参数，旧版扁平 overrides 只一次性迁给初始模型且自动剔除 resolution/count", () => {
    // 旧版 localStorage：带有共享音轨开关及不应跨段粘滞的 resolution/count
    memory.set(
      "drama-studio:d1",
      JSON.stringify({
        videoModel: "",
        overrides: { resolution: "1080P", count: 3, generate_audio: true },
        modelOverrides: { legacyOnly: { resolution: "1080P", count: 2 } },
      }),
    );
    const cleaned = readPrefs("d1");
    expect(cleaned.overrides).toEqual({ generate_audio: true });
    expect(cleaned.modelOverrides).toBeUndefined();
    // 迁移应同步写回 localStorage，防止旧脏缓存残留
    expect(JSON.parse(memory.get("drama-studio:d1")!)).toEqual({
      videoModel: "",
      overrides: { generate_audio: true },
    });

    const store = useStudioPrefsStore.getState();
    // 切换模型前应把旧版非粘滞 overrides 认领给原模型 kling，并清空扁平 overrides
    store.setVideoModel("d1", "veo", "kling");
    const afterSwitch = useStudioPrefsStore.getState().get("d1");
    expect(afterSwitch.videoModel).toBe("veo");
    expect(afterSwitch.overrides).toEqual({});
    expect(overridesForModel(afterSwitch, "kling")).toEqual({ generate_audio: true });
    expect(overridesForModel(afterSwitch, "veo")).toEqual({});

    // 为 veo 与 third 分别写入共享覆盖，嵌套更新不得冲掉已保存的模型覆盖
    store.setModelOverride("d1", "veo", "camera_fixed", true, "kling");
    store.setModelOverride("d1", "third", "generate_audio", false, "veo");
    const finalPrefs = useStudioPrefsStore.getState().get("d1");
    expect(overridesForModel(finalPrefs, "kling")).toEqual({ generate_audio: true });
    expect(overridesForModel(finalPrefs, "veo")).toEqual({ camera_fixed: true });
    expect(overridesForModel(finalPrefs, "third")).toEqual({ generate_audio: false });

    // 删除某个共享覆盖键
    store.setModelOverride("d1", "veo", "camera_fixed", undefined);
    expect(overridesForModel(useStudioPrefsStore.getState().get("d1"), "veo")).toEqual({});
    expect(overridesForModel(useStudioPrefsStore.getState().get("d1"), "kling")).toEqual({
      generate_audio: true,
    });
  });

  test("护栏 A：resolution 与 count 仅按 (segmentId, modelKey) 隔离保存，绝不跨片段继承", () => {
    expect(STICKY_DRAFT_KEYS).toEqual(["resolution", "count"]);
    expect(
      stripStickyDraftKeys({ resolution: "1080P", count: 4, generate_audio: true, cfg: 7 }),
    ).toEqual({ generate_audio: true, cfg: 7 });

    const store = useStudioPrefsStore.getState();
    // 共享参数按模型生效
    store.setModelOverride("d1", "kling", "generate_audio", true);
    // 即使通过 setModelOverride，未传 segmentId 也不得把 resolution/count 写进共享层
    store.setModelOverride("d1", "kling", "resolution", "1080P");
    store.setModelOverride("d1", "kling", "count", 3);
    expect(store.get("d1").modelOverrides?.kling).toEqual({ generate_audio: true });
    expect(overridesForModel(store.get("d1"), "kling")).toEqual({ generate_audio: true });

    // S2 设置 resolution=1080P、count=2；支持 setModelOverride(..., segmentId) 与 setSegmentOverride
    store.setModelOverride("d1", "kling", "resolution", "1080P", undefined, "s2");
    store.setSegmentOverride("d1", "s2", "kling", "count", 2);

    // 切到 S7 时只拿共享非粘滞参数，resolution/count 不跨段继承
    const latest = useStudioPrefsStore.getState().get("d1");
    expect(overridesForModel(latest, "kling", "s7")).toEqual({ generate_audio: true });
    // 回到 S2 恢复 S2 自己的草稿，并持久化到 localStorage
    expect(overridesForModel(latest, "kling", "s2")).toEqual({
      generate_audio: true,
      resolution: "1080P",
      count: 2,
    });
    expect(overridesForModel(readPrefs("d1"), "kling", "s2")).toEqual({
      generate_audio: true,
      resolution: "1080P",
      count: 2,
    });

    // 清理单镜草稿后回退到只含共享参数
    store.setModelOverride("d1", "kling", "resolution", undefined, undefined, "s2");
    store.setSegmentOverride("d1", "s2", "kling", "count", undefined);
    expect(overridesForModel(useStudioPrefsStore.getState().get("d1"), "kling", "s2")).toEqual({
      generate_audio: true,
    });
    expect(useStudioPrefsStore.getState().get("d1").segmentOverrides).toBeUndefined();
  });
});
