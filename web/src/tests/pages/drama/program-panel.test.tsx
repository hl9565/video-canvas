import { expect, spyOn, test } from "bun:test";
import { useState } from "react";

import type { PutEpisodeProgramReq, Segment } from "@/api/drama/type";
import { ProgramPanel } from "@/pages/drama/studio/program/program-panel";
import { useProgram } from "@/pages/drama/studio/program/use-program";
import { segmentKey, useDramaSegmentsStore } from "@/store/drama-segments";
import { ApiError } from "@/utils/requests/request";
import service from "@/utils/requests/service";

import { installDom } from "../../helpers/dom";

const segments: Segment[] = [
  {
    id: "segment-1",
    episodeNo: 1,
    seq: 0,
    seconds: 4,
    prompt: "",
    chosenTaskId: "take-1",
    takes: [
      {
        taskId: "take-1",
        status: "succeeded",
        createdAt: "",
        errorMessage: "",
        asset: {
          id: "4",
          kind: "video",
          url: "/take.mp4",
          mimeType: "video/mp4",
          byteSize: 1024,
          width: 1280,
          height: 720,
          durationMs: 4000,
          fileName: "take.mp4",
        },
      },
    ],
    status: "idle",
    revision: 3,
    updatedAt: "",
    adaptations: {},
  },
];
const anchor = {
  role: "sfx",
  asset_id: 12,
  anchor: { segment_id: "segment-1", source_f: 12 },
  length_f: 6,
};
const issue = {
  field: "anchor",
  reason: "anchor_source_replaced",
  severity: "error",
  cue_id: "sfx-1",
};
const view = (id: string, version: number, audio: object[] = []) => ({
  episode_id: id,
  segment_version: version,
  edit_schema: 1,
  program: { v: 1, audio, overlays: [] },
});
const backendSegments = (frames = 96, segmentVersion = 4, episodeId = "ep-1") => ({
  episode_id: episodeId,
  segment_version: segmentVersion,
  items: [
    {
      id: "segment-1",
      episode_no: 1,
      seq: 0,
      seconds: frames / 24,
      segment_version: segmentVersion,
      prompt: "",
      chosen_task_id: "take-1",
      source_frame_count: frames,
      takes: [
        {
          task_id: "take-1",
          status: "succeeded",
          created_at: "",
          error_message: "",
          asset: {
            id: 4,
            kind: "video",
            url: "/take.mp4",
            mime_type: "video/mp4",
            byte_size: 1024,
            width: 1280,
            height: 720,
            duration_ms: Math.round((frames / 24) * 1000),
            file_name: "take.mp4",
          },
        },
      ],
      status: "idle",
      revision: 3,
      updated_at: "",
      adaptations: {},
    },
  ],
});
const button = (root: ParentNode, label: string) =>
  [...root.querySelectorAll("button")].find((el) => el.textContent?.trim() === label);

function Harness() {
  const program = useProgram({ dramaId: "d", episodeId: "ep-1", episodeNo: 1 });
  return (
    <ProgramPanel open canvasId="canvas-1" episodeId="ep-1" segments={segments} program={program} />
  );
}

test("在全长配乐旁添加环境底噪，默认开启人声闪避且可以取消", async () => {
  const dom = await installDom();
  const bgm = { id: "music", role: "bgm", asset_id: 12, start_f: 0, end_f: 96 };
  const get = spyOn(service, "get").mockImplementation(async (url: string) =>
    url.endsWith("/program") ? view("ep-1", 3, [bgm]) : { status: "ready", segment_version: 3 },
  );
  const put = spyOn(service, "put").mockImplementation(async (_url, body) =>
    view("ep-1", 4, (body as PutEpisodeProgramReq).program.audio ?? []),
  );
  const mounted = await dom.mount(<Harness />);
  try {
    const { container } = mounted;
    const addRoom = button(container, "添加环境底噪");
    expect(addRoom).toBeDefined();
    await dom.click(addRoom);
    const duck = () => container.querySelector<HTMLInputElement>("input[type=checkbox]");
    expect(duck()?.checked).toBe(true);
    await dom.type(container.querySelector("input[inputmode=numeric]"), "13");
    await dom.click(button(container, "保存节目音频"));
    const req = (index: number) => put.mock.calls[index]?.[1] as PutEpisodeProgramReq;
    expect(req(0).program.audio).toMatchObject([
      bgm,
      { role: "room_tone", start_f: 0, end_f: 24, duck_db: -8 },
    ]);
    await dom.click(duck());
    await dom.click(button(container, "保存节目音频"));
    expect(req(1).program.audio?.[1]).not.toHaveProperty("duck_db");
  } finally {
    await mounted.unmount();
    get.mockRestore();
    put.mockRestore();
    dom.restore();
  }
});

test("a save consumes only submitted rebinds and keeps a rebind queued by the hook", async () => {
  const dom = await installDom();
  const audio = [
    { ...anchor, id: "sfx-1" },
    { ...anchor, id: "sfx-2" },
  ];
  const secondIssue = { ...issue, cue_id: "sfx-2" };
  let version = 3;
  const get = spyOn(service, "get").mockImplementation(async (url: string) =>
    url.endsWith("/program")
      ? view("ep-1", version, audio)
      : {
          status: "ready",
          segment_version: version,
          manifest_hash: "",
          program_diagnostics: version === 3 ? [issue, secondIssue] : [secondIssue],
        },
  );
  let finish!: (value: ReturnType<typeof view>) => void;
  const put = spyOn(service, "put")
    .mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          finish = resolve;
        }),
    )
    .mockImplementationOnce(async () => view("ep-1", 5, audio));

  function SaveHarness() {
    const program = useProgram({ dramaId: "d", episodeId: "ep-1", episodeNo: 1 });
    return (
      <>
        <ProgramPanel
          open
          canvasId="canvas-1"
          episodeId="ep-1"
          segments={segments}
          program={program}
        />
        <button onClick={() => program.toggleRebind("sfx-2", true)}>Queue second rebind</button>
        <output>{JSON.stringify({ ids: program.rebindIds, dirty: program.dirty })}</output>
      </>
    );
  }

  const mounted = await dom.mount(<SaveHarness />);
  try {
    const { container } = mounted;
    const checkbox = () => container.querySelector<HTMLInputElement>("input[type=checkbox]");
    await dom.click(checkbox());
    await dom.click(button(container, "保存节目音频"));
    expect(put.mock.calls[0]?.[1]).toMatchObject({ rebind_anchor_cue_ids: ["sfx-1"] });
    await dom.click(button(container, "Queue second rebind"));
    expect(checkbox()?.disabled).toBe(true);
    await dom.act(async () => {
      version = 4;
      finish(view("ep-1", version, audio));
    });
    expect(container.querySelector("output")?.textContent).toBe('{"ids":["sfx-2"],"dirty":false}');
    await dom.click(button(container, "保存节目音频"));
    expect(put.mock.calls[1]?.[1]).toMatchObject({
      expected_segment_version: 4,
      rebind_anchor_cue_ids: ["sfx-2"],
    });
    expect(container.querySelector("output")?.textContent).toBe('{"ids":[],"dirty":false}');
  } finally {
    await mounted.unmount();
    get.mockRestore();
    put.mockRestore();
    dom.restore();
  }
});

test("版本冲突重试保留远端新增和删除，以及本地编辑和锚点重绑", async () => {
  const dom = await installDom();
  const audio = [
    { ...anchor, id: "sfx-1" },
    { ...anchor, id: "sfx-2" },
    { ...anchor, id: "sfx-deleted" },
  ];
  const voice = { id: "voice-remote", role: "narration", asset_id: 9, start_f: 0, end_f: 24 };
  let version = 3;
  const sent: PutEpisodeProgramReq[] = [];
  const get = spyOn(service, "get").mockImplementation(async (url: string) => {
    if (url.endsWith("/program"))
      return view("ep-1", version, version === 3 ? audio : [...audio.slice(0, 2), voice]);
    if (url.endsWith("/segments")) return backendSegments(96, version);
    if (url.endsWith("/manifest?v=2"))
      return {
        status: "ready",
        segment_version: version,
        manifest_hash: "",
        program_diagnostics: [issue, { ...issue, cue_id: "sfx-2" }],
      };
    throw new Error(url);
  });
  const put = spyOn(service, "put").mockImplementation(async (_url, body) => {
    const req = body as PutEpisodeProgramReq;
    sent.push(req);
    if (req.expected_segment_version === 3) {
      version = 4;
      throw new ApiError("节目单版本冲突", 91001, 409);
    }
    return view("ep-1", 5, req.program.audio ?? []);
  });
  const mounted = await dom.mount(<Harness />);
  try {
    const { container } = mounted;
    expect(container.querySelectorAll('[aria-label="节目音频时间轴"] [role="group"]')).toHaveLength(
      3,
    );
    expect(container.textContent).not.toContain("字幕");
    const assetId = container.querySelector("input[inputmode=numeric]");
    await dom.type(assetId, "9007199254740993");
    const rebind = container.querySelector("input[type=checkbox]");
    await dom.click(rebind);
    await dom.type(container.querySelectorAll("input[type=number]")[0], "13");
    expect((rebind as HTMLInputElement).checked).toBe(true);
    await dom.click(button(container, "保存节目音频"));
    expect(container.textContent).toContain("节目单版本已变更，草稿已保留");
    expect((container.querySelector("input[inputmode=numeric]") as HTMLInputElement).value).toBe(
      "9007199254740993",
    );
    expect(sent).toHaveLength(1);
    await dom.click(button(container, "基于最新版本重试"));
    expect(sent[1]).toMatchObject({
      expected_episode_id: "ep-1",
      expected_segment_version: 4,
      rebind_anchor_cue_ids: ["sfx-1"],
      program: {
        overlays: [],
        audio: [
          { id: "sfx-1", asset_id: "9007199254740993", anchor: { source_f: 13 } },
          { id: "sfx-2" },
          voice,
        ],
      },
    });
    expect(container.textContent).not.toContain("草稿已保留");
  } finally {
    await mounted.unmount();
    get.mockRestore();
    put.mockRestore();
    dom.restore();
  }
});

test.each(["同一条目", "旁白重叠"])("%s冲突要求确认，重试不会发送覆盖请求", async (kind) => {
  const dom = await installDom();
  const voice = { id: "voice", role: "narration", asset_id: 10, start_f: 0, end_f: 24 };
  const remote =
    kind === "同一条目"
      ? [{ ...voice, asset_id: 11 }]
      : [voice, { ...voice, id: "voice-remote", start_f: 24, end_f: 48 }];
  let version = 3;
  const get = spyOn(service, "get").mockImplementation(async (url: string) =>
    url.endsWith("/program")
      ? view("ep-1", version, version === 3 ? [voice] : remote)
      : url.endsWith("/segments")
        ? backendSegments(96, version)
        : { status: "ready", segment_version: version },
  );
  const put = spyOn(service, "put").mockImplementation(async (_url, body) => {
    if (version === 3) {
      version = 4;
      throw new ApiError("节目单版本冲突", 91001, 409);
    }
    return view("ep-1", 5, (body as PutEpisodeProgramReq).program.audio ?? []);
  });
  const mounted = await dom.mount(<Harness />);
  try {
    const { container } = mounted;
    await dom.click(container.querySelector('[data-cue-id="voice"]'));
    await dom.type(container.querySelector("input[inputmode=numeric]"), "12");
    if (kind === "旁白重叠") {
      await dom.type(container.querySelectorAll("input[type=number]")[1], "30");
    }
    await dom.click(button(container, "保存节目音频"));
    await dom.click(button(container, "基于最新版本重试"));
    expect(put).toHaveBeenCalledTimes(1);
    const confirm = button(container, "确认覆盖服务端版本");
    expect(confirm).toBeDefined();
    expect(button(container, "基于最新版本重试")).toBeUndefined();
    if (kind === "旁白重叠") {
      expect(container.querySelector('[data-cue-id="voice-remote"]')).not.toBeNull();
      expect(confirm?.disabled).toBe(true);
      await dom.type(container.querySelectorAll("input[type=number]")[1], "24");
    }
    await dom.click(confirm);
    expect(put).toHaveBeenCalledTimes(2);
    expect(put.mock.calls[1]?.[1]).toMatchObject({
      expected_segment_version: 4,
      program: {
        audio:
          kind === "同一条目"
            ? [{ ...voice, asset_id: 12 }]
            : [{ ...voice, asset_id: 12 }, remote[1]],
      },
    });
  } finally {
    await mounted.unmount();
    get.mockRestore();
    put.mockRestore();
    dom.restore();
  }
});

test("同集号换了集 ID 后，旧 GET 的较晚响应不会覆盖当前草稿", async () => {
  const dom = await installDom();
  let finishFirst!: (result: object) => void;
  const first = new Promise<object>((resolve) => {
    finishFirst = resolve;
  });
  const get = spyOn(service, "get")
    .mockImplementationOnce(() => first)
    .mockResolvedValue(view("new", 5));
  function Harness() {
    const [id, setId] = useState("old");
    const program = useProgram({ dramaId: "d", episodeId: id, episodeNo: 1 });
    return (
      <>
        <button onClick={() => setId("new")}>切换</button>
        <output>{program.baseEpisodeId}</output>
      </>
    );
  }
  const mounted = await dom.mount(<Harness />);
  try {
    await dom.click(button(mounted.container, "切换"));
    expect(mounted.container.querySelector("output")?.textContent).toBe("new");
    await dom.act(async () => finishFirst(view("old", 99)));
    expect(mounted.container.querySelector("output")?.textContent).toBe("new");
  } finally {
    await mounted.unmount();
    useDramaSegmentsStore.setState({ byEpisode: {}, watching: {} });
    get.mockRestore();
    dom.restore();
  }
});

test("冲突重试按服务端最新时间轴重校验，剧集缩短后阻断超界保存直到修正并确认", async () => {
  const dom = await installDom();
  const voice = { id: "voice", role: "narration", asset_id: 10, start_f: 0, end_f: 24 };
  let version = 3;
  const get = spyOn(service, "get").mockImplementation(async (url: string) =>
    url.endsWith("/program")
      ? view("ep-1", version, [voice])
      : url.endsWith("/segments")
        ? backendSegments(48, version)
        : { status: "ready", segment_version: version },
  );
  const put = spyOn(service, "put").mockImplementation(async (_url, body) => {
    if (version === 3) {
      version = 4;
      throw new ApiError("节目单版本冲突", 91001, 409);
    }
    return view("ep-1", 5, (body as PutEpisodeProgramReq).program.audio ?? []);
  });
  const mounted = await dom.mount(<Harness />);
  try {
    const { container } = mounted;
    await dom.click(container.querySelector('[data-cue-id="voice"]'));
    await dom.type(container.querySelectorAll("input[type=number]")[1], "72");
    await dom.click(button(container, "保存节目音频"));
    await dom.click(button(container, "基于最新版本重试"));
    expect(put).toHaveBeenCalledTimes(1);
    expect(container.textContent).toContain("合并后的节目音频未通过校验，请修正条目后再确认覆盖。");
    expect(container.textContent).toContain("48f");
    const confirm = button(container, "确认覆盖服务端版本");
    expect(confirm?.disabled).toBe(true);
    await dom.type(container.querySelectorAll("input[type=number]")[1], "48");
    expect(confirm?.disabled).toBe(false);
    await dom.click(confirm);
    expect(put).toHaveBeenCalledTimes(2);
    expect(put.mock.calls[1]?.[1]).toMatchObject({
      expected_segment_version: 4,
      program: { audio: [{ ...voice, end_f: 48 }] },
    });
  } finally {
    await mounted.unmount();
    useDramaSegmentsStore.setState({ byEpisode: {}, watching: {} });
    get.mockRestore();
    put.mockRestore();
    dom.restore();
  }
});

test("确认保存采用推送的 v3 时间轴，即使节目单仍停在 v2", async () => {
  const dom = await installDom();
  const voice = { id: "voice", role: "narration", asset_id: 10, start_f: 0, end_f: 24 };
  let version = 1;
  const get = spyOn(service, "get").mockImplementation(async (url: string) =>
    url.endsWith("/program")
      ? view("ep-1", version, [voice])
      : url.endsWith("/segments")
        ? backendSegments(48, version)
        : { status: "ready", segment_version: version },
  );
  const put = spyOn(service, "put").mockImplementation(async (_url, body) => {
    if (version === 1) {
      version = 2;
      throw new ApiError("节目单版本冲突", 91001, 409);
    }
    return view("ep-1", 3, (body as PutEpisodeProgramReq).program.audio ?? []);
  });
  function LiveTimelineHarness() {
    const [live, setLive] = useState(segments);
    const program = useProgram({ dramaId: "d", episodeId: "ep-1", episodeNo: 1 });
    return (
      <>
        <output data-testid="program-version">{program.serverVersion}</output>
        <button
          type="button"
          onClick={() => setLive(segments.map((s) => ({ ...s, segmentVersion: 3 })))}
        >
          推入 v3
        </button>
        <ProgramPanel open canvasId="c" episodeId="ep-1" segments={live} program={program} />
      </>
    );
  }
  const mounted = await dom.mount(<LiveTimelineHarness />);
  try {
    const { container } = mounted;
    await dom.click(container.querySelector('[data-cue-id="voice"]'));
    await dom.type(container.querySelectorAll("input[type=number]")[1], "72");
    await dom.click(button(container, "保存节目音频"));
    await dom.click(button(container, "基于最新版本重试"));
    expect(put).toHaveBeenCalledTimes(1);
    expect(container.textContent).toContain("合并后的节目音频未通过校验，请修正条目后再确认覆盖。");
    expect(container.textContent).toContain("48f");
    const confirm = button(container, "确认覆盖服务端版本");
    expect(confirm?.disabled).toBe(true);
    await dom.click(button(container, "推入 v3"));
    expect(container.textContent).toContain("96f");
    expect(container.querySelector('[data-testid="program-version"]')?.textContent).toBe("2");
    expect(confirm?.disabled).toBe(false);
    await dom.click(confirm);
    expect(put).toHaveBeenCalledTimes(2);
    expect(put.mock.calls[1]?.[1]).toMatchObject({
      expected_segment_version: 2,
      program: { audio: [{ ...voice, end_f: 72 }] },
    });
  } finally {
    await mounted.unmount();
    useDramaSegmentsStore.setState({ byEpisode: {}, watching: {} });
    get.mockRestore();
    put.mockRestore();
    dom.restore();
  }
});

test("远端 segmentVersion 递增时刷新 savedDocument，同时完整保留本地脏草稿", async () => {
  const dom = await installDom();
  const remoteCue = { id: "remote-bgm", role: "bgm", asset_id: 12, start_f: 0, end_f: 24 };
  let serverVersion = 1;
  const get = spyOn(service, "get").mockImplementation(async (url: string) =>
    url.endsWith("/program")
      ? view("ep-1", serverVersion, serverVersion === 1 ? [] : [remoteCue])
      : { status: "ready", segment_version: serverVersion },
  );
  function VersionedHarness() {
    const [ver, setVer] = useState(1);
    const program = useProgram({
      dramaId: "d",
      episodeId: "ep-1",
      episodeNo: 1,
      segmentVersion: ver,
    });
    return (
      <>
        <button
          type="button"
          onClick={() => {
            serverVersion = 2;
            setVer(2);
          }}
        >
          远端新增音频
        </button>
        <output data-testid="saved-count">{program.savedDocument?.audio.length ?? 0}</output>
        <output data-testid="dirty">{String(program.dirty)}</output>
        <ProgramPanel
          open
          canvasId="canvas-1"
          episodeId="ep-1"
          segments={segments}
          program={program}
        />
      </>
    );
  }
  const mounted = await dom.mount(<VersionedHarness />);
  try {
    const { container } = mounted;
    expect(container.querySelector('[data-testid="saved-count"]')?.textContent).toBe("0");
    await dom.click(button(container, "添加环境底噪"));
    expect(container.querySelector('[data-testid="dirty"]')?.textContent).toBe("true");
    await dom.click(button(container, "远端新增音频"));
    expect(container.querySelector('[data-testid="saved-count"]')?.textContent).toBe("1");
    expect(container.querySelector('[data-testid="dirty"]')?.textContent).toBe("true");
    expect(container.querySelectorAll('[aria-label="节目音频时间轴"] [data-cue-id]')).toHaveLength(
      1,
    );
  } finally {
    await mounted.unmount();
    get.mockRestore();
    dom.restore();
  }
});

test("冲突重试时片段与节目单版本持续不一致则阻断保存，绝不拿旧时间轴配合新版本号提交", async () => {
  const dom = await installDom();
  const voice = { id: "voice", role: "narration", asset_id: 10, start_f: 0, end_f: 24 };
  let programVersion = 3;
  let segmentVersion = 3;
  const get = spyOn(service, "get").mockImplementation(async (url: string) =>
    url.endsWith("/program")
      ? view("ep-1", programVersion, [voice])
      : url.endsWith("/segments")
        ? backendSegments(segmentVersion === 3 ? 96 : 48, segmentVersion)
        : { status: "ready", segment_version: programVersion },
  );
  const put = spyOn(service, "put").mockImplementation(async () => {
    programVersion = 4;
    segmentVersion = 3;
    throw new ApiError("节目单版本冲突", 91001, 409);
  });
  const mounted = await dom.mount(<Harness />);
  try {
    const { container } = mounted;
    await dom.click(container.querySelector('[data-cue-id="voice"]'));
    await dom.type(container.querySelectorAll("input[type=number]")[1], "72");
    await dom.click(button(container, "保存节目音频"));
    await dom.click(button(container, "基于最新版本重试"));
    expect(put).toHaveBeenCalledTimes(1);
    expect(container.textContent).toContain("剧本分镜版本已变更，请刷新清单");
    expect(container.textContent).not.toContain("48f");
  } finally {
    await mounted.unmount();
    useDramaSegmentsStore.setState({ byEpisode: {}, watching: {} });
    get.mockRestore();
    put.mockRestore();
    dom.restore();
  }
});

test("并发同步乱序响应不回滚 savedDocument，且自动同步失败后不抑制后续重试", async () => {
  const dom = await installDom();
  const cueV2 = { id: "remote-v2", role: "bgm", asset_id: 11, start_f: 0, end_f: 24 };
  const cueV3 = { id: "remote-v3", role: "room_tone", asset_id: 12, start_f: 0, end_f: 48 };
  let finishSlowV2!: (v: object) => void;
  let failAutoOnce = true;
  let callIndex = 0;
  let syncPairPromise: Promise<unknown> | null = null;
  let racePromise: Promise<unknown> | null = null;
  const get = spyOn(service, "get").mockImplementation(async (url: string) => {
    if (!url.endsWith("/program")) return { status: "ready", segment_version: 3 };
    callIndex += 1;
    if (callIndex === 1) return view("ep-1", 1, []);
    if (callIndex === 2 && failAutoOnce) {
      failAutoOnce = false;
      throw new ApiError("网络超时", 500, 500);
    }
    if (callIndex === 3) return view("ep-1", 2, [cueV2]);
    if (callIndex === 4) {
      return await new Promise<object>((resolve) => {
        finishSlowV2 = resolve;
      });
    }
    return view("ep-1", 3, [cueV2, cueV3]);
  });
  function SyncHarness() {
    const [ver, setVer] = useState(1);
    const [renderCount, setRenderCount] = useState(0);
    const program = useProgram({
      dramaId: "d",
      episodeId: "ep-1",
      episodeNo: 1,
      segmentVersion: ver,
    });
    return (
      <>
        <button type="button" onClick={() => setVer(2)}>
          触发同步
        </button>
        <button type="button" onClick={() => setRenderCount((n) => n + 1)}>
          同版本重渲染
        </button>
        <button
          type="button"
          onClick={() => {
            syncPairPromise = Promise.all([program.sync(), program.sync()]);
          }}
        >
          并发同步
        </button>
        <button
          type="button"
          onClick={() => {
            racePromise = Promise.all([program.reload(), program.sync()]);
          }}
        >
          重加载与同步竞态
        </button>
        <output data-testid="saved-count">{program.savedDocument?.audio.length ?? 0}</output>
        <output data-testid="server-ver">{program.serverVersion ?? -1}</output>
        <output data-testid="render-count">{renderCount}</output>
      </>
    );
  }
  const mounted = await dom.mount(<SyncHarness />);
  try {
    const { container } = mounted;
    await dom.click(button(container, "触发同步"));
    expect(container.querySelector('[data-testid="server-ver"]')?.textContent).toBe("1");
    await dom.click(button(container, "同版本重渲染"));
    expect(container.querySelector('[data-testid="server-ver"]')?.textContent).toBe("2");
    expect(container.querySelector('[data-testid="saved-count"]')?.textContent).toBe("1");
    await dom.click(button(container, "重加载与同步竞态"));
    expect(container.querySelector('[data-testid="server-ver"]')?.textContent).toBe("3");
    expect(container.querySelector('[data-testid="saved-count"]')?.textContent).toBe("2");
    await dom.act(async () => {
      finishSlowV2(view("ep-1", 2, [cueV2]));
      await racePromise;
    });
    expect(container.querySelector('[data-testid="server-ver"]')?.textContent).toBe("3");
    expect(container.querySelector('[data-testid="saved-count"]')?.textContent).toBe("2");
    await dom.click(button(container, "并发同步"));
    expect(container.querySelector('[data-testid="server-ver"]')?.textContent).toBe("3");
    expect(container.querySelector('[data-testid="saved-count"]')?.textContent).toBe("2");
    await dom.act(async () => {
      await syncPairPromise;
    });
    expect(container.querySelector('[data-testid="server-ver"]')?.textContent).toBe("3");
    expect(container.querySelector('[data-testid="saved-count"]')?.textContent).toBe("2");
  } finally {
    await mounted.unmount();
    get.mockRestore();
    dom.restore();
  }
});

test("冲突重试时 /segments 返回错误集 ID 且列表为空时阻断保存并保留本地片段缓存", async () => {
  const dom = await installDom();
  const voice = { id: "voice", role: "narration", asset_id: 10, start_f: 0, end_f: 24 };
  let version = 3;
  useDramaSegmentsStore.getState().setList("d", 1, segments);
  const get = spyOn(service, "get").mockImplementation(async (url: string) =>
    url.endsWith("/program")
      ? view("ep-1", version, [voice])
      : url.endsWith("/segments")
        ? { episode_id: "ep-B", segment_version: version, items: [] }
        : { status: "ready", segment_version: version },
  );
  const put = spyOn(service, "put").mockImplementation(async () => {
    version = 4;
    throw new ApiError("节目单版本冲突", 91001, 409);
  });
  const mounted = await dom.mount(<Harness />);
  try {
    const { container } = mounted;
    await dom.click(container.querySelector('[data-cue-id="voice"]'));
    await dom.type(container.querySelectorAll("input[type=number]")[1], "48");
    await dom.click(button(container, "保存节目音频"));
    await dom.click(button(container, "基于最新版本重试"));
    expect(put).toHaveBeenCalledTimes(1);
    expect(useDramaSegmentsStore.getState().byEpisode["d:1"]).toEqual(segments);
    expect(container.textContent).toContain("剧本分镜版本已变更，请刷新清单");
  } finally {
    await mounted.unmount();
    useDramaSegmentsStore.setState({ byEpisode: {}, watching: {} });
    get.mockRestore();
    put.mockRestore();
    dom.restore();
  }
});

test("冲突重试 clean rebase 后若 PUT 失败，savedDocument 仍同步到 v2 且保留合并后的脏草稿", async () => {
  const dom = await installDom();
  const remoteCue = { id: "remote-bgm", role: "bgm", asset_id: 12, start_f: 0, end_f: 24 };
  let serverVer = 1;
  let putAttempt = 0;
  const sent: PutEpisodeProgramReq[] = [];
  const get = spyOn(service, "get").mockImplementation(async (url: string) =>
    url.endsWith("/program")
      ? view("ep-1", serverVer, serverVer === 1 ? [] : [remoteCue])
      : url.endsWith("/segments")
        ? backendSegments(96, serverVer)
        : { status: "ready", segment_version: serverVer },
  );
  const put = spyOn(service, "put").mockImplementation(async (_url, body) => {
    const req = body as PutEpisodeProgramReq;
    sent.push(req);
    putAttempt += 1;
    if (putAttempt === 1) {
      serverVer = 2;
      throw new ApiError("节目单版本冲突", 91001, 409);
    }
    if (putAttempt === 2) {
      throw new ApiError("临时服务错误", 500, 500);
    }
    return view("ep-1", 3, req.program.audio ?? []);
  });
  function RebaseFailHarness() {
    const [ver, setVer] = useState(1);
    const program = useProgram({
      dramaId: "d",
      episodeId: "ep-1",
      episodeNo: 1,
      segmentVersion: ver,
    });
    return (
      <>
        <button type="button" onClick={() => setVer(2)}>
          同步版本2
        </button>
        <output data-testid="saved-count">{program.savedDocument?.audio.length ?? 0}</output>
        <output data-testid="server-ver">{program.serverVersion ?? -1}</output>
        <output data-testid="dirty">{String(program.dirty)}</output>
        <output data-testid="draft-count">{program.draft?.audio.length ?? 0}</output>
        <ProgramPanel
          open
          canvasId="canvas-1"
          episodeId="ep-1"
          segments={segments}
          program={program}
        />
      </>
    );
  }
  const mounted = await dom.mount(<RebaseFailHarness />);
  try {
    const { container } = mounted;
    await dom.click(button(container, "添加环境底噪"));
    await dom.type(container.querySelector("input[inputmode=numeric]"), "99");
    await dom.click(button(container, "保存节目音频"));
    expect(container.textContent).toContain("节目单版本已变更，草稿已保留");
    await dom.click(button(container, "基于最新版本重试"));
    // rebase 获取 v=2 后 PUT 失败（500）：savedDocument 应已是 v2，并保留合并后的脏草稿（remote-bgm + local room_tone）
    expect(put).toHaveBeenCalledTimes(2);
    expect(container.querySelector('[data-testid="server-ver"]')?.textContent).toBe("2");
    expect(container.querySelector('[data-testid="saved-count"]')?.textContent).toBe("1");
    expect(container.querySelector('[data-testid="dirty"]')?.textContent).toBe("true");
    expect(container.querySelector('[data-testid="draft-count"]')?.textContent).toBe("2");
    await dom.click(button(container, "同步版本2"));
    expect(container.querySelector('[data-testid="server-ver"]')?.textContent).toBe("2");
    expect(container.querySelector('[data-testid="saved-count"]')?.textContent).toBe("1");
    await dom.click(button(container, "保存节目音频"));
    expect(put).toHaveBeenCalledTimes(3);
    expect(sent[2]).toMatchObject({ expected_segment_version: 2 });
    expect(container.querySelector('[data-testid="server-ver"]')?.textContent).toBe("3");
    expect(container.querySelector('[data-testid="dirty"]')?.textContent).toBe("false");
  } finally {
    await mounted.unmount();
    useDramaSegmentsStore.setState({ byEpisode: {}, watching: {} });
    get.mockRestore();
    put.mockRestore();
    dom.restore();
  }
});

test("409 后 clean rebase 直接保存成功时 dirty 回到 false，不再误显示未保存", async () => {
  const dom = await installDom();
  const remoteCue = { id: "remote-bgm", role: "bgm", asset_id: 12, start_f: 0, end_f: 24 };
  let serverVer = 1;
  const sent: PutEpisodeProgramReq[] = [];
  const get = spyOn(service, "get").mockImplementation(async (url: string) =>
    url.endsWith("/program")
      ? view("ep-1", serverVer, serverVer === 1 ? [] : [remoteCue])
      : url.endsWith("/segments")
        ? backendSegments(96, serverVer)
        : { status: "ready", segment_version: serverVer },
  );
  const put = spyOn(service, "put").mockImplementation(async (_url, body) => {
    const req = body as PutEpisodeProgramReq;
    sent.push(req);
    if (req.expected_segment_version === 1) {
      serverVer = 2;
      throw new ApiError("节目单版本冲突", 91001, 409);
    }
    serverVer = 3;
    return view("ep-1", 3, req.program.audio ?? []);
  });
  function CleanRebaseHarness() {
    const program = useProgram({ dramaId: "d", episodeId: "ep-1", episodeNo: 1 });
    return (
      <>
        <output data-testid="server-ver">{program.serverVersion ?? -1}</output>
        <output data-testid="saved-count">{program.savedDocument?.audio.length ?? 0}</output>
        <output data-testid="dirty">{String(program.dirty)}</output>
        <ProgramPanel
          open
          canvasId="canvas-1"
          episodeId="ep-1"
          segments={segments}
          program={program}
        />
      </>
    );
  }
  const mounted = await dom.mount(<CleanRebaseHarness />);
  try {
    const { container } = mounted;
    await dom.click(button(container, "添加环境底噪"));
    await dom.type(container.querySelector("input[inputmode=numeric]"), "99");
    await dom.click(button(container, "保存节目音频"));
    expect(container.textContent).toContain("节目单版本已变更，草稿已保留");

    await dom.click(button(container, "基于最新版本重试"));
    expect(put).toHaveBeenCalledTimes(2);
    expect(sent[1]).toMatchObject({ expected_segment_version: 2 });
    expect(container.querySelector('[data-testid="server-ver"]')?.textContent).toBe("3");
    expect(container.querySelector('[data-testid="saved-count"]')?.textContent).toBe("2");
    expect(container.querySelector('[data-testid="dirty"]')?.textContent).toBe("false");
    expect(container.textContent).not.toContain("未保存");
  } finally {
    await mounted.unmount();
    useDramaSegmentsStore.setState({ byEpisode: {}, watching: {} });
    get.mockRestore();
    put.mockRestore();
    dom.restore();
  }
});

test("PUT 在途期间收到并发 sync(v2)（含 GET 先行发起与入口拦截）被拒绝后不会阻断 PUT 失败后的自动同步恢复", async () => {
  const dom = await installDom();
  const remoteCue = { id: "remote-v2", role: "bgm", asset_id: 12, start_f: 0, end_f: 24 };
  let serverVer = 1;
  let rejectPut!: (err: unknown) => void;
  let holdSecondGet = true;
  const heldSyncGet = Promise.withResolvers<object>();
  let syncCalls = 0;
  const get = spyOn(service, "get").mockImplementation(async (url: string) => {
    if (!url.endsWith("/program")) return { status: "ready", segment_version: serverVer };
    syncCalls += 1;
    if (syncCalls === 1) return view("ep-1", 1, []);
    if (syncCalls === 2 && holdSecondGet) return heldSyncGet.promise;
    return view("ep-1", 2, [remoteCue]);
  });
  const put = spyOn(service, "put").mockImplementation(
    () =>
      new Promise((_, reject) => {
        rejectPut = reject;
      }),
  );
  function SavingSyncRaceHarness() {
    const [ver, setVer] = useState(1);
    const program = useProgram({
      dramaId: "d",
      episodeId: "ep-1",
      episodeNo: 1,
      segmentVersion: ver,
    });
    return (
      <>
        <button type="button" onClick={() => void program.sync()}>
          在途同步
        </button>
        <button type="button" onClick={() => setVer(2)}>
          推进版本2
        </button>
        <output data-testid="server-ver">{program.serverVersion ?? -1}</output>
        <output data-testid="saved-count">{program.savedDocument?.audio.length ?? 0}</output>
        <output data-testid="dirty">{String(program.dirty)}</output>
        <ProgramPanel
          open
          canvasId="canvas-1"
          episodeId="ep-1"
          segments={segments}
          program={program}
        />
      </>
    );
  }
  const mounted = await dom.mount(<SavingSyncRaceHarness />);
  try {
    const { container } = mounted;
    await dom.click(button(container, "添加环境底噪"));
    await dom.type(container.querySelector("input[inputmode=numeric]"), "99");

    // 1) sync 的 GET 在 PUT 之前发出，并在 PUT 在途期间返回 v2：await 后的守卫必须丢弃且不得标记 v2 已同步
    serverVer = 2;
    await dom.click(button(container, "在途同步"));
    await dom.click(button(container, "保存节目音频"));
    holdSecondGet = false;
    await dom.act(async () => {
      heldSyncGet.resolve(view("ep-1", 2, [remoteCue]));
    });
    expect(container.querySelector('[data-testid="server-ver"]')?.textContent).toBe("1");

    // 2) PUT 在途时再次调用 sync：入口守卫同样拒绝且不得把 v2 标成已同步
    await dom.click(button(container, "在途同步"));
    expect(container.querySelector('[data-testid="server-ver"]')?.textContent).toBe("1");

    // 3) PUT 失败后推进 segmentVersion = 2，自动同步仍应触发并将 savedDocument 更新到 v2
    await dom.act(async () => {
      rejectPut(new ApiError("临时服务错误", 500, 500));
    });
    await dom.click(button(container, "推进版本2"));
    expect(container.querySelector('[data-testid="server-ver"]')?.textContent).toBe("2");
    expect(container.querySelector('[data-testid="saved-count"]')?.textContent).toBe("1");
    expect(container.querySelector('[data-testid="dirty"]')?.textContent).toBe("true");
  } finally {
    await mounted.unmount();
    useDramaSegmentsStore.setState({ byEpisode: {}, watching: {} });
    get.mockRestore();
    put.mockRestore();
    dom.restore();
  }
});

test("rebase 失败后若后续同步推进到更大版本，失效旧的 48f 固定时间轴并使用新版 96f 时间轴校验保存", async () => {
  const dom = await installDom();
  const voice = { id: "voice", role: "narration", asset_id: 10, start_f: 0, end_f: 24 };
  let serverVer = 1;
  let timelineFrames = 96;
  let putAttempt = 0;
  const sent: PutEpisodeProgramReq[] = [];
  const get = spyOn(service, "get").mockImplementation(async (url: string) => {
    if (url.endsWith("/program")) return view("ep-1", serverVer, [voice]);
    if (url.endsWith("/segments")) return backendSegments(timelineFrames, serverVer);
    return { status: "ready", segment_version: serverVer };
  });
  const put = spyOn(service, "put").mockImplementation(async (_url, body) => {
    const req = body as PutEpisodeProgramReq;
    sent.push(req);
    putAttempt += 1;
    if (putAttempt === 1) {
      serverVer = 2;
      timelineFrames = 48;
      throw new ApiError("节目单版本冲突", 91001, 409);
    }
    if (putAttempt === 2) {
      throw new ApiError("临时服务错误", 500, 500);
    }
    if (req.expected_segment_version !== serverVer) {
      throw new ApiError("节目单版本冲突", 91001, 409);
    }
    serverVer = 4;
    return view("ep-1", 4, req.program.audio ?? []);
  });
  function RebaseTimelineInvalidationHarness() {
    const [ver, setVer] = useState(1);
    const [epSegments, setEpSegments] = useState<Segment[]>(segments);
    const program = useProgram({
      dramaId: "d",
      episodeId: "ep-1",
      episodeNo: 1,
      segmentVersion: ver,
    });
    return (
      <>
        <button
          type="button"
          onClick={() => {
            serverVer = 3;
            timelineFrames = 96;
            setEpSegments(segments.map((s) => ({ ...s, segmentVersion: 3, seconds: 4 })));
            setVer(3);
          }}
        >
          同步扩张时间轴v3
        </button>
        <output data-testid="server-ver">{program.serverVersion ?? -1}</output>
        <ProgramPanel
          open
          canvasId="canvas-1"
          episodeId="ep-1"
          segments={epSegments}
          program={program}
        />
      </>
    );
  }
  const mounted = await dom.mount(<RebaseTimelineInvalidationHarness />);
  try {
    const { container } = mounted;
    await dom.click(container.querySelector('[data-cue-id="voice"]'));
    await dom.type(container.querySelectorAll("input[type=number]")[1], "48");
    await dom.click(button(container, "保存节目音频"));
    expect(container.textContent).toContain("节目单版本已变更，草稿已保留");

    // rebase 获取 v=2 (48f) 后 PUT 失败（500），此时固定了 48f 的 rebasedTimeline
    await dom.click(button(container, "基于最新版本重试"));
    expect(put).toHaveBeenCalledTimes(2);
    expect(container.querySelector('[data-testid="server-ver"]')?.textContent).toBe("2");

    // 随后同步推进到 v=3（扩展回 96f）：旧的 48f 固定时间轴应被清除
    await dom.click(button(container, "同步扩张时间轴v3"));
    expect(container.querySelector('[data-testid="server-ver"]')?.textContent).toBe("3");

    // 将 end_f 改为 72（>48f 但 <=96f）：不应被旧 48f 时间轴阻断，冲突重试到 v=3 后应成功保存
    await dom.type(container.querySelectorAll("input[type=number]")[1], "72");
    expect(container.textContent).not.toContain("48f");
    await dom.click(button(container, "保存节目音频"));
    await dom.click(button(container, "基于最新版本重试"));
    expect(put).toHaveBeenCalledTimes(4);
    expect(sent[3]).toMatchObject({
      expected_segment_version: 3,
      program: { audio: [{ ...voice, end_f: 72 }] },
    });
    expect(container.querySelector('[data-testid="server-ver"]')?.textContent).toBe("4");
  } finally {
    await mounted.unmount();
    useDramaSegmentsStore.setState({ byEpisode: {}, watching: {} });
    get.mockRestore();
    put.mockRestore();
    dom.restore();
  }
});

test("迟到 v2 (48f) rebase 与新版 v3 (120f) 推送在两种到达顺序下均不得把时间轴回滚到 48f", async () => {
  const voice = { id: "voice", role: "narration", asset_id: 10, start_f: 0, end_f: 24 };
  const segWithFrames = (seconds: number, segmentVersion: number): Segment => ({
    ...segments[0],
    seconds,
    episodeId: "ep-1",
    segmentVersion,
    takes: [
      {
        ...segments[0].takes[0],
        asset: { ...segments[0].takes[0].asset!, durationMs: seconds * 1000 },
      },
    ],
    sourceFrameCount: seconds * 24,
  });

  function DualOrderHarness({ epVer, epNo }: { epVer: number; epNo: number }) {
    const epSegments = useDramaSegmentsStore(
      (s) => s.byEpisode[segmentKey("d", epNo)] ?? [segWithFrames(2, 1)],
    );
    const program = useProgram({
      dramaId: "d",
      episodeId: "ep-1",
      episodeNo: epNo,
      segmentVersion: epVer,
    });
    return (
      <>
        <output data-testid="server-ver">{program.serverVersion ?? -1}</output>
        <output data-testid="seg-ver">{epSegments[0]?.segmentVersion ?? -1}</output>
        <ProgramPanel
          open
          canvasId="canvas-1"
          episodeId="ep-1"
          segments={epSegments}
          program={program}
        />
      </>
    );
  }

  // 顺序 A：先收到 v3 (120f) 推送与 program 同步，之后迟到的 v2 (48f) rebase 响应不得覆盖 store 或时间轴
  {
    const dom = await installDom();
    let resolveDelayedV2Segs!: (v: unknown) => void;
    const delayedV2Segs = new Promise((r) => {
      resolveDelayedV2Segs = r;
    });
    let progVer = 1;
    let rebaseSegCall = 0;
    const get = spyOn(service, "get").mockImplementation(async (url: string) => {
      if (url.endsWith("/program")) return view("ep-1", progVer, [voice]);
      if (url.endsWith("/segments")) {
        rebaseSegCall += 1;
        if (rebaseSegCall === 1) return delayedV2Segs;
        return backendSegments(48, 2);
      }
      return { status: "ready", segment_version: progVer };
    });
    const put = spyOn(service, "put").mockRejectedValueOnce(new ApiError("冲突", 91001, 409));
    useDramaSegmentsStore.setState({ byEpisode: {}, watching: {} });
    useDramaSegmentsStore.getState().setList("d", 1, [segWithFrames(2, 1)], undefined, {
      episodeId: "ep-1",
      segmentVersion: 1,
    });
    function OrderAWrapper() {
      const [epVer, setEpVer] = useState(1);
      return (
        <>
          <button
            type="button"
            onClick={() => {
              useDramaSegmentsStore
                .getState()
                .apply("d", 1, { ...segWithFrames(5, 3), revision: 9 });
              setEpVer(3);
            }}
          >
            推入v3片段
          </button>
          <DualOrderHarness epVer={epVer} epNo={1} />
        </>
      );
    }
    const mounted = await dom.mount(<OrderAWrapper />);
    try {
      const { container } = mounted;
      await dom.click(container.querySelector('[data-cue-id="voice"]'));
      await dom.type(container.querySelectorAll("input[type=number]")[1], "30");
      await dom.click(button(container, "保存节目音频"));
      expect(container.textContent).toContain("基于最新版本重试");

      // 发起 v2 rebase，在 GET /segments(v2, 48f) 尚在途时推入 v3 (120f)
      progVer = 2;
      await dom.click(button(container, "基于最新版本重试"));
      progVer = 3;
      await dom.click(button(container, "推入v3片段"));
      // 现在释放迟到的 v2 (48f) segments（且后续 2 次重试也滞后停在 v2）
      await dom.act(async () => {
        resolveDelayedV2Segs(backendSegments(48, 2));
      });
      await dom.flush(10);

      expect(useDramaSegmentsStore.getState().byEpisode[segmentKey("d", 1)]?.[0].seconds).toBe(5);
      expect(container.textContent).toContain("120f");
      expect(container.textContent).not.toContain("48f");
    } finally {
      await mounted.unmount();
      useDramaSegmentsStore.setState({ byEpisode: {}, watching: {} });
      get.mockRestore();
      put.mockRestore();
      dom.restore();
    }
  }

  // 顺序 B：v2 (48f) rebase 先落到 conflict/保存态，随后 v3 (120f) 事件到达且再来一次迟到 v2 全量列表，必须维持 120f
  {
    const dom = await installDom();
    let progVer = 1;
    let segFrames = 48;
    let putCount = 0;
    const get = spyOn(service, "get").mockImplementation(async (url: string) => {
      if (url.endsWith("/program")) return view("ep-1", progVer, [voice]);
      if (url.endsWith("/segments")) return backendSegments(segFrames, progVer);
      return { status: "ready", segment_version: progVer };
    });
    const put = spyOn(service, "put").mockImplementation(async () => {
      putCount += 1;
      if (putCount === 1) {
        progVer = 2;
        throw new ApiError("冲突", 91001, 409);
      }
      throw new ApiError("瞬态失败", 500, 500);
    });
    useDramaSegmentsStore.setState({ byEpisode: {}, watching: {} });
    useDramaSegmentsStore.getState().setList("d", 2, [segWithFrames(2, 1)], undefined, {
      episodeId: "ep-1",
      segmentVersion: 1,
    });
    function OrderBWrapper() {
      const [epVer, setEpVer] = useState(1);
      return (
        <>
          <button
            type="button"
            onClick={() => {
              useDramaSegmentsStore
                .getState()
                .apply("d", 2, { ...segWithFrames(5, 3), revision: 9 });
              setEpVer(3);
            }}
          >
            推入v3片段B
          </button>
          <DualOrderHarness epVer={epVer} epNo={2} />
        </>
      );
    }
    const mounted = await dom.mount(<OrderBWrapper />);
    try {
      const { container } = mounted;
      await dom.click(container.querySelector('[data-cue-id="voice"]'));
      await dom.type(container.querySelectorAll("input[type=number]")[1], "30");
      await dom.click(button(container, "保存节目音频"));
      await dom.click(button(container, "基于最新版本重试"));
      await dom.flush(10);
      expect(container.textContent).toContain("48f");

      // 先来 v3 (120f) 与同步推进，再来一次迟到的 v2 (48f) setList
      progVer = 3;
      segFrames = 120;
      await dom.click(button(container, "推入v3片段B"));
      await dom.flush(10);
      expect(
        useDramaSegmentsStore.getState().setList("d", 2, [segWithFrames(2, 2)], undefined, {
          episodeId: "ep-1",
          segmentVersion: 2,
        }),
      ).toBe(false);
      await dom.flush(10);
      expect(container.textContent).toContain("120f");
      expect(container.textContent).not.toContain("48f");
    } finally {
      await mounted.unmount();
      useDramaSegmentsStore.setState({ byEpisode: {}, watching: {} });
      get.mockRestore();
      put.mockRestore();
      dom.restore();
    }
  }
});
