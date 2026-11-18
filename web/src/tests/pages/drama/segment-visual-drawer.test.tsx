import { expect, test } from "bun:test";

import { toSegmentUpdateBody } from "@/api/drama";
import type { Segment, UpdateSegmentInput } from "@/api/drama/type";
import { SegmentVisualDrawer } from "@/pages/drama/studio/segment-visual-drawer";
import instance from "@/utils/requests/request";
import { installDom } from "../../helpers/dom";

const segment: Segment = {
  id: "seg",
  episodeNo: 1,
  seq: 0,
  seconds: 5,
  prompt: "",
  takes: [],
  chosenTaskId: "",
  status: "idle",
  revision: 1,
  updatedAt: "",
  adaptations: {},
};

test("非 16:9 禁止静帧和黑场；特效字段不可编辑", async () => {
  const dom = await installDom();
  try {
    const view = await dom.mount(
      <SegmentVisualDrawer
        open
        ratio="9:16"
        segment={segment}
        assets={[]}
        onOpenChange={() => {}}
        onSave={async () => {}}
      />,
    );
    expect(document.body.textContent).toContain("静帧、黑场与画面特效仅支持 16:9 剧集");
    expect((document.querySelector('option[value="black"]') as HTMLOptionElement).disabled).toBe(
      true,
    );
    expect(document.querySelector("fieldset[disabled]")).not.toBeNull();
    await view.unmount();
  } finally {
    dom.restore();
  }
});

test("保存静帧按整数帧更新且保留大素材 ID；离开静帧可显式清空素材", async () => {
  const dom = await installDom();
  let patch: Omit<UpdateSegmentInput, "revision"> | undefined;
  const mounted = await dom.mount(
    <SegmentVisualDrawer
      open
      ratio="16:9"
      segment={segment}
      assets={[]}
      onOpenChange={() => {}}
      onSave={async (input) => {
        patch = input;
      }}
    />,
  );
  try {
    const select = document.querySelector("select")!;
    await dom.act(async () => {
      select.value = "still";
      select.dispatchEvent(new dom.win.Event("change", { bubbles: true }));
    });
    await dom.type(document.querySelector("input[inputmode=numeric]"), "9007199254740993");
    await dom.type(document.querySelector('input[type="number"]'), "7");
    await dom.click(document.querySelector('button[type="submit"]'));
    expect(patch).toEqual({
      visualKind: "still",
      stillAssetId: "9007199254740993",
      edit: { sourceInF: 0, sourceOutF: 7 },
    });
    expect(toSegmentUpdateBody({ revision: 1, ...patch })).toMatchObject({
      visual_kind: "still",
      still_asset_id: "9007199254740993",
      edit: { source_in_f: 0, source_out_f: 7 },
    });
    expect(toSegmentUpdateBody({ revision: 1, stillAssetId: "17" }).still_asset_id).toBe(17);
    expect(toSegmentUpdateBody({ revision: 1, stillAssetId: null }).still_asset_id).toBeNull();
    expect(toSegmentUpdateBody({ revision: 1 })).not.toHaveProperty("still_asset_id");
  } finally {
    await mounted.unmount();
    dom.restore();
  }
});

test("从素材库浏览选择 > MAX_SAFE_INTEGER 素材 ID（9007199254740992 与 9007199254740993）保持精确十进制串不串位", async () => {
  const dom = await installDom();
  const prevAdapter = instance.defaults.adapter;
  const rawBody = JSON.stringify({
    items: [
      {
        id: "lib-1",
        folder_id: null,
        created_at: "2026-11-17T00:00:00Z",
        asset: {
          id: "__ID_1__",
          kind: "image",
          url: "https://cdn/1.png",
          file_name: "a.png",
        },
      },
      {
        id: "lib-2",
        folder_id: null,
        created_at: "2026-11-17T00:00:00Z",
        asset: {
          id: "__ID_2__",
          kind: "image",
          url: "https://cdn/2.png",
          file_name: "b.png",
        },
      },
    ],
    next_cursor: "",
  })
    .replace('"__ID_1__"', "9007199254740992")
    .replace('"__ID_2__"', "9007199254740993");
  instance.defaults.adapter = async (config) => ({
    data: rawBody,
    status: 200,
    statusText: "OK",
    headers: {},
    config,
  });
  let patch: Omit<UpdateSegmentInput, "revision"> | undefined;
  const mounted = await dom.mount(
    <SegmentVisualDrawer
      open
      ratio="16:9"
      segment={segment}
      assets={[]}
      onOpenChange={() => {}}
      onSave={async (input) => {
        patch = input;
      }}
    />,
  );
  try {
    const modeSelect = document.querySelector("select")!;
    await dom.act(async () => {
      modeSelect.value = "still";
      modeSelect.dispatchEvent(new dom.win.Event("change", { bubbles: true }));
    });
    const browseBtn = [...document.querySelectorAll("button")].find(
      (b) => b.textContent?.trim() === "浏览素材库",
    );
    expect(browseBtn).toBeTruthy();
    await dom.click(browseBtn!);
    await dom.flush(10);

    const selects = [...document.querySelectorAll("select")];
    const assetSelect = selects[1]!;
    const optionValues = [...assetSelect.querySelectorAll("option")].map((o) => o.value);
    expect(optionValues).toContain("9007199254740992");
    expect(optionValues).toContain("9007199254740993");

    await dom.act(async () => {
      assetSelect.value = "9007199254740993";
      assetSelect.dispatchEvent(new dom.win.Event("change", { bubbles: true }));
    });
    await dom.type(document.querySelector('input[type="number"]'), "7");
    await dom.click(document.querySelector('button[type="submit"]'));
    expect(patch).toEqual({
      visualKind: "still",
      stillAssetId: "9007199254740993",
      edit: { sourceInF: 0, sourceOutF: 7 },
    });
  } finally {
    instance.defaults.adapter = prevAdapter;
    await mounted.unmount();
    dom.restore();
  }
});
