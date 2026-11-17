import { useRef, useState } from "react";
import { useNavigate, useSearchParams } from "react-router";
import { toast } from "sonner";

import { getFrameRun, setFrameRunFrame } from "@/api/drama";
import { dramaErrorMessage } from "@/api/drama/errors";
import { useCanvasEditor } from "@/components/canvas-editor/editor-api";
import { ChromeButton, ChromePill, ChromeSeparator } from "@/components/canvas/chrome/chrome";
import type { CanvasNode } from "@/types";
import { ApiError } from "@/utils/requests/request";

/** 带完整片场来源的构图会话；票据只在此次回传中使用。 */
interface BridgeContext {
  /** 短剧 ID */
  dramaId: string;
  /** 集号 */
  episodeNo: number;
  /** 片段 ID */
  segmentId: string;
  /** 流程 ID */
  frameRunId: string;
  /** 回传票据 */
  canvasTicket: string;
}

function ActiveBridge({ dramaId, episodeNo, segmentId, frameRunId, canvasTicket }: BridgeContext) {
  const editor = useCanvasEditor();
  const navigate = useNavigate();
  const [, setParams] = useSearchParams();
  const [choice, setChoice] = useState<{ id: string; selectedId?: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const pending = useRef(false);
  const [error, setError] = useState<string | null>(null);
  const ready = editor.nodes.filter(
    (node): node is CanvasNode =>
      node.type === "canvas" &&
      node.data.kind === "image" &&
      !!node.data.assetId?.trim() &&
      node.data.status !== "running",
  );
  const selectedId = ready.find((node) => node.selected)?.id;
  const candidate =
    (choice?.selectedId === selectedId ? ready.find((node) => node.id === choice?.id) : null) ??
    ready.find((node) => node.id === selectedId) ??
    ready[0];
  const submit = async () => {
    if (!candidate?.data.assetId || pending.current) return;
    pending.current = true;
    setBusy(true);
    setError(null);
    try {
      /** URL 的 revision 和票据可能已过时，每次回传前先读取最新状态与票据。 */
      const latest = await getFrameRun(segmentId, frameRunId);
      const effectiveTicket = latest.canvasTicket || canvasTicket;
      const run = await setFrameRunFrame(segmentId, frameRunId, {
        revision: latest.revision,
        frameAssetId: candidate.data.assetId,
        originCanvasId: editor.canvasId,
        canvasTicket: effectiveTicket,
      });
      setParams(
        (previous) => {
          const next = new URLSearchParams(previous);
          next.set("frame_run_revision", String(run.revision));
          if (run.canvasTicket || effectiveTicket) {
            next.set("canvas_ticket", run.canvasTicket || effectiveTicket);
          }
          return next;
        },
        { replace: true },
      );
      if (typeof BroadcastChannel !== "undefined") {
        const channel = new BroadcastChannel("vc-drama-frame-run");
        try {
          channel.postMessage({ segmentId, frameRunId, run });
        } finally {
          channel.close();
        }
      }
      toast.success("已设为分镜首帧");
    } catch (e) {
      setError(dramaErrorMessage(e, "设置分镜首帧失败，请重试"));
      if (e instanceof ApiError && e.status === 409) {
        try {
          const latest = await getFrameRun(segmentId, frameRunId);
          setParams(
            (previous) => {
              const next = new URLSearchParams(previous);
              next.set("frame_run_revision", String(latest.revision));
              return next;
            },
            { replace: true },
          );
        } catch {
          /** 保留原始冲突提示，下一次点击会重新读取流程。 */
        }
      }
    } finally {
      pending.current = false;
      setBusy(false);
    }
  };
  return (
    <div
      role="region"
      aria-label="分镜首帧构图"
      className="canvas-overlay-interactive pointer-events-auto absolute top-16 left-1/2 z-20 w-max max-w-[calc(100%-1.5rem)] -translate-x-1/2 md:top-3"
    >
      <ChromePill className="h-auto flex-wrap justify-center gap-2 rounded-2xl p-2 md:rounded-full">
        <span className="px-2 text-xs font-medium">第 {episodeNo} 集 · 首帧构图</span>
        <select
          aria-label="画布首帧图片"
          className="bg-chrome-hover border-chrome-border h-9 max-w-44 rounded-full border px-3 text-sm"
          value={candidate?.id ?? ""}
          disabled={busy || !ready.length}
          onChange={(event) => setChoice({ id: event.target.value, selectedId })}
        >
          {!ready.length && <option value="">暂无就绪图片</option>}
          {ready.map((node) => (
            <option key={node.id} value={node.id}>
              {node.data.label || node.id}
            </option>
          ))}
        </select>
        <ChromeButton
          disabled={busy || !candidate}
          onClick={() => void submit()}
          className="text-sm"
        >
          {busy ? "设置中…" : "设为分镜首帧"}
        </ChromeButton>
        <ChromeSeparator />
        <ChromeButton
          className="text-sm"
          onClick={() =>
            navigate(
              `/drama/${encodeURIComponent(dramaId)}?${new URLSearchParams({ area: "studio", ep: String(episodeNo), segment: segmentId, frame_run: frameRunId })}`,
            )
          }
        >
          返回分镜工作台
        </ChromeButton>
      </ChromePill>
      {error && (
        <p
          role="alert"
          className="bg-chrome text-destructive mt-2 rounded-xl p-3 text-sm shadow-lg ring-1 ring-chrome-border backdrop-blur-xl"
        >
          {error}
        </p>
      )}
    </div>
  );
}

/** 普通画布无此浮条；仅带片场来源、有效集号及票据的 URL 激活桥接。 */
export function DramaFrameBridge() {
  const [params] = useSearchParams();
  const dramaId = params.get("drama_id");
  const episodeNo = Number(params.get("episode_no"));
  const segmentId = params.get("segment_id");
  const frameRunId = params.get("frame_run_id");
  const canvasTicket = params.get("canvas_ticket");
  const revision = Number(params.get("frame_run_revision"));
  if (
    !dramaId ||
    !Number.isInteger(episodeNo) ||
    episodeNo < 1 ||
    !segmentId ||
    !frameRunId ||
    !canvasTicket ||
    !Number.isInteger(revision) ||
    revision < 1
  )
    return null;
  return (
    <ActiveBridge
      key={`${segmentId}:${frameRunId}`}
      dramaId={dramaId}
      episodeNo={episodeNo}
      segmentId={segmentId}
      frameRunId={frameRunId}
      canvasTicket={canvasTicket}
    />
  );
}
