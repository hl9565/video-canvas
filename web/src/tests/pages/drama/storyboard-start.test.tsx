import { expect, spyOn, test } from "bun:test";
import { MemoryRouter } from "react-router";
import { toast } from "sonner";

import { mapSegment } from "@/api/drama";
import type { EpisodeSummary } from "@/api/drama/type";
import { DialogHost } from "@/components/dialog-host";
import { DramaWorkspaceContext } from "@/pages/drama/workspace/context";
import { useStoryboardStart } from "@/pages/drama/workspace/storyboard-start";
import type { DramaWorkspace } from "@/pages/drama/workspace/use-workspace";
import { useDialogStore } from "@/store/dialog";
import { useDramaJobsStore } from "@/store/drama";
import { useDramaSegmentsStore } from "@/store/drama-segments";
import { ApiError } from "@/utils/requests/request";
import service from "@/utils/requests/service";
import { installDom } from "../../helpers/dom";

const episode: EpisodeSummary = {
  id: "ep",
  no: 1,
  title: "第一集",
  synopsis: "",
  scriptStatus: "ready",
  locked: true,
  revision: 1,
  updatedAt: "",
  storyboardStatus: "ready",
  segmentCount: 1,
  segmentSeconds: 5,
  doneSegmentCount: 1,
  final: null,
  finalStale: false,
  hook: "",
  cliffhanger: "",
  unitFrom: null,
  unitTo: null,
  editSchema: 1,
};

const rawClip = {
  id: "s1",
  episode_id: "ep",
  episode_no: 1,
  segment_version: 5,
  seq: 0,
  seconds: 5,
  prompt: "keep me",
  takes: [],
  chosen_task_id: "",
  status: "idle",
  revision: 1,
  updated_at: "",
} as const;
const clip = mapSegment({ ...rawClip, takes: [] });
const empty = {
  episode_id: "ep",
  segment_version: 5,
  edit_schema: 1,
  program: { v: 1, audio: [], overlays: [] },
};
const withAudio = {
  ...empty,
  program: {
    ...empty.program,
    audio: [{ id: "voice", role: "narration", asset_id: 30, start_f: 0, end_f: 12 }],
  },
};
const withOverlays = { ...empty, program: { ...empty.program, overlays: [{ id: "overlay" }] } };

function Regenerate({ target }: { target: EpisodeSummary }) {
  const { start, pending } = useStoryboardStart(15);
  return (
    <button disabled={pending === target.no} onClick={() => void start(target)}>
      重新生成
    </button>
  );
}

async function scenario(options: {
  programs: unknown[];
  snapshots?: unknown[];
  done?: number;
  submitError?: Error;
  confirm?: boolean | number;
  accepted?: boolean;
}) {
  const dom = await installDom();
  const store = useDramaSegmentsStore.getState();
  const { byDrama, settled } = useDramaJobsStore.getState();
  store.setList("r12", 1, [clip]);
  let confirmedClicks = 0;
  const get = spyOn(service, "get").mockImplementation(async (url) => {
    if (url.endsWith("/program")) {
      const next = options.programs.shift();
      if (next instanceof Error) throw next;
      return next;
    }
    if (url.endsWith("/segments")) return options.snapshots?.[confirmedClicks] ?? atVersion(5);
    throw new Error(`Unexpected GET ${url}`);
  });
  const post = spyOn(service, "post").mockImplementation(async () => {
    throw new Error("Unexpected POST");
  });
  if (options.submitError) post.mockRejectedValueOnce(options.submitError);
  if (options.accepted)
    post.mockResolvedValueOnce({
      id: "job",
      step: "storyboard",
      episodes: [1],
      status: "queued",
      progress: 0,
      credits: 2,
      created_at: "",
    });
  const remove = spyOn(service, "delete").mockResolvedValue(undefined);
  const feedback = spyOn(toast, "error").mockImplementation(() => "");
  const refresh = async () => {};
  const workspace = { drama: { id: "r12" }, refresh } as DramaWorkspace;
  const target = { ...episode, doneSegmentCount: options.done ?? 1 };
  const mounted = await dom.mount(
    <MemoryRouter>
      <DramaWorkspaceContext.Provider value={workspace}>
        <Regenerate target={target} />
      </DramaWorkspaceContext.Provider>
      <DialogHost />
    </MemoryRouter>,
  );
  const confirmation = () =>
    [...dom.win.document.querySelectorAll("button")].find(
      (button) => button.textContent === "清空并重新生成",
    );
  try {
    await dom.click(mounted.container.querySelector("button"));
    if (options.confirm) {
      for (let click = 0; click < Number(options.confirm); click++) {
        expect(confirmation()).toBeDefined();
        expect(remove).not.toHaveBeenCalled();
        confirmedClicks++;
        await dom.click(confirmation());
      }
    } else {
      expect(Boolean(confirmation())).toBe(false);
    }
    const messages = [
      dom.win.document.querySelector("[role=alertdialog]")?.textContent,
      ...feedback.mock.calls.map(([message]) => message),
    ].join(" ");
    return {
      messages,
      deletes: [...remove.mock.calls],
      submits: post.mock.calls.length,
      requests: post.mock.calls.map(([path, body]) => ({ path, body })),
      version: store.snapshotOf("r12", 1)?.segmentVersion,
      clips: useDramaSegmentsStore.getState().byEpisode["r12:1"],
    };
  } finally {
    await mounted.unmount();
    for (const spy of [get, post, remove, feedback]) spy.mockRestore();
    useDialogStore.setState({ dialogs: [] });
    store.forgetDrama("r12");
    useDramaJobsStore.setState({ byDrama, settled });
    dom.restore();
  }
}

test.each([
  ["authored audio", withAudio, "节目单"],
  ["authored overlays", withOverlays, "节目单"],
  ["a replacement episode", { ...empty, episode_id: "other" }, "该集已被替换"],
  ["a failed program read", new Error("offline"), "无法确认节目单"],
])(
  "regenerate with %s never opens the destructive dialog or deletes clips",
  async (_name, program, message) => {
    const result = await scenario({ programs: [program] });
    expect(result).toMatchObject({ deletes: [], submits: 0, clips: [clip] });
    expect(result.messages).toContain(message);
  },
);

const atVersion = (version: number) => ({
  episode_id: "ep",
  segment_version: version,
  items: [{ ...rawClip, segment_version: version }],
});

test.each([
  ["inconsistent versions", atVersion(6), "分镜已变更，请重新确认"],
  [
    "an unknown list version",
    { ...atVersion(5), segment_version: undefined },
    "分镜已变更，请重新确认",
  ],
  [
    "a clip belonging to another episode",
    { ...atVersion(5), items: [{ ...rawClip, episode_id: "other" }] },
    "该集已被替换",
  ],
])("%s stop regeneration before opening the dialog", async (_name, snapshot, message) => {
  const result = await scenario({ snapshots: [snapshot], programs: [empty] });
  expect(result).toMatchObject({ deletes: [], submits: 0, clips: [clip] });
  expect(result.messages).toContain(message);
});

test("confirmation at v5 cannot clear v6 even when both current reads agree", async () => {
  const result = await scenario({
    snapshots: [atVersion(5), atVersion(6), atVersion(5)],
    programs: [empty, { ...empty, segment_version: 6 }, empty],
    confirm: 2,
  });
  expect(result).toMatchObject({ deletes: [], submits: 0, version: 5, clips: [clip] });
  expect(result.messages).toContain("分镜已变更，请重新确认");
});

test.each([5, 6])(
  "a lost response prevents the same dialog from retrying a clear at v%i",
  async (version) => {
    const result = await scenario({
      snapshots: [atVersion(5), atVersion(5), atVersion(version)],
      programs: [empty, empty, { ...empty, segment_version: version }],
      confirm: 2,
      submitError: new Error("response lost after commit"),
    });
    expect(result).toMatchObject({
      deletes: [],
      submits: 1,
      requests: [{ path: "/dramas/r12/jobs", body: { expected_segment_version: 5 } }],
      version: 5,
      clips: [clip],
    });
    expect(result.messages).toContain("请刷新工作台后重新确认");
  },
);

test.each([
  ["authorship", withAudio, "节目单"],
  ["a read failure", new Error("offline"), "无法确认节目单"],
  ["a newer segment version", { ...empty, segment_version: 6 }, "分镜已变更"],
])(
  "%s after the dialog opens stops confirmation before any deletion",
  async (_name, program, message) => {
    const result = await scenario({ programs: [empty, program], confirm: true });
    expect(result).toMatchObject({ deletes: [], submits: 0, clips: [clip] });
    expect(result.messages).toContain(message);
  },
);

test("a stale summary and existing-takes conflict still verify program content before asking to clear", async () => {
  const result = await scenario({
    done: 0,
    programs: [withAudio],
    submitError: new ApiError("takes", 67013, 409),
  });
  expect(result).toMatchObject({ deletes: [], submits: 1, clips: [clip] });
  expect(result.messages).toContain("节目单");
});

test("the backend program protection error surfaces without a destructive confirmation", async () => {
  const result = await scenario({
    done: 0,
    programs: [],
    submitError: new ApiError("请先清空节目单，再重新生成分镜", 67006, 400),
  });
  expect(result).toMatchObject({ deletes: [], submits: 1, clips: [clip] });
  expect(result.messages).toContain("请先清空节目单，再重新生成分镜");
});

test("confirmed regeneration clears through a single job request", async () => {
  const result = await scenario({ programs: [empty, empty], confirm: true, accepted: true });
  expect(result).toMatchObject({
    deletes: [],
    submits: 1,
    requests: [
      {
        path: "/dramas/r12/jobs",
        body: {
          step: "storyboard",
          episodes: [1],
          max_seconds: 15,
          expected_episode_id: "ep",
          expected_segment_version: 5,
          confirm_clear: true,
        },
      },
    ],
    version: 6,
    clips: [],
  });
});

test("an ordinary submission includes the stable episode ID", async () => {
  const result = await scenario({ programs: [], done: 0, accepted: true });
  expect(result.requests).toEqual([
    {
      path: "/dramas/r12/jobs",
      body: { step: "storyboard", episodes: [1], max_seconds: 15, expected_episode_id: "ep" },
    },
  ]);
  expect(result.clips).toEqual([clip]);
});

test.each([
  ["an anchored cue saved", new ApiError("请先清空节目单，再重新生成分镜", 67006, 400)],
  ["a changed segment version", new ApiError("分镜已变更，请重试", 91001, 409)],
  ["a replaced episode", new ApiError("该集已被替换，请刷新工作台", 91001, 409)],
] as const)(
  "%s after the last precheck leaves cached clips intact on rejection",
  async (_name, error) => {
    const result = await scenario({ programs: [empty, empty], confirm: true, submitError: error });
    expect(result).toMatchObject({ deletes: [], submits: 1, version: 5, clips: [clip] });
    expect(result.messages).toContain(error.message);
  },
);
