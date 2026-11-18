import { useCallback, useState } from "react";
import { Loader2 } from "lucide-react";
import { toast } from "sonner";

import { getEpisodeProgram, listEpisodeSegments, submitJob } from "@/api/drama";
import { DRAMA_ERR, dramaErrorMessage, isDramaError } from "@/api/drama/errors";
import type { EpisodeSummary } from "@/api/drama/type";
import { confirm } from "@/components/admin-ui/confirm-dialog";
import { Button } from "@/components/ui/button";
import { useDramaJobsStore } from "@/store/drama";
import { useDramaSegmentsStore } from "@/store/drama-segments";
import { storyboardStart } from "@/utils/drama/episode-cards";

import { useWorkspace } from "./context";
import { useEstimate } from "./use-estimate";

/** 任何未知或不匹配的节目单状态都不能授权清空片段。 */
async function checkStoryboardProgram(
  dramaId: string,
  no: number,
  episodeId: string,
  segmentVersion: number,
) {
  const view = await getEpisodeProgram(dramaId, no, { silent: true }).catch(() => {
    throw new Error("无法确认节目单状态，未清空片段，请重试");
  });
  if (view.episodeId !== episodeId) {
    throw new Error("该集已被替换，请刷新工作台");
  }
  if (view.segmentVersion !== segmentVersion) return false;
  if (view.program.audio.length || view.program.overlays.length) {
    throw new Error("请先清空节目单，再重新生成分镜");
  }
  return true;
}

/** 读取同一集、同一版本的分镜和节目单；版本已变更时返回 null。 */
async function checkClearSnapshot(
  dramaId: string,
  no: number,
  expectedEpisodeId?: string,
  confirmedVersion?: number,
) {
  const snapshot = await listEpisodeSegments(dramaId, no);
  const { episodeId, segmentVersion } = snapshot;
  if (
    !episodeId ||
    (expectedEpisodeId && episodeId !== expectedEpisodeId) ||
    snapshot.items.some((segment) => segment.episodeId !== episodeId)
  ) {
    throw new Error("该集已被替换，请刷新工作台");
  }
  if (
    segmentVersion === undefined ||
    (confirmedVersion !== undefined && segmentVersion !== confirmedVersion)
  ) {
    return null;
  }
  if (!(await checkStoryboardProgram(dramaId, no, episodeId, segmentVersion))) return null;
  return { episodeId, segmentVersion, segmentIds: snapshot.items.map((row) => row.id) };
}

/** 仅对可清空的已有候选打开确认框；后端也会在提交及替换时保护节目单。 */
export function useStoryboardStart(maxSeconds: number) {
  const { drama, refresh } = useWorkspace();
  const [pending, setPending] = useState<number | null>(null);

  const submit = useCallback(
    async (
      no: number,
      episodeId?: string,
      clear?: { segmentVersion: number; segmentIds: string[] },
    ) => {
      const job = await submitJob(drama.id, {
        step: "storyboard",
        episodes: [no],
        maxSeconds,
        expectedEpisodeId: episodeId,
        ...(clear && { confirmClear: true, expectedSegmentVersion: clear.segmentVersion }),
      });
      if (clear && episodeId) {
        useDramaSegmentsStore
          .getState()
          .applyConfirmedClear(drama.id, no, episodeId, clear.segmentVersion + 1, clear.segmentIds);
      }
      useDramaJobsStore.getState().applyJob(drama.id, job);
      void refresh();
    },
    [drama.id, maxSeconds, refresh],
  );

  const askClear = useCallback(
    async (episode: EpisodeSummary) => {
      const { no, id } = episode;
      const snapshot = await checkClearSnapshot(drama.id, no, id);
      if (!snapshot) throw new Error("分镜已变更，请重新确认");
      const { episodeId, segmentVersion: confirmedVersion, segmentIds } = snapshot;
      let sent = false;
      let stale = false;
      const checkConfirmation = () => {
        if (sent) throw new Error("清空请求已发送，请刷新工作台后重新确认");
        if (stale) throw new Error("分镜已变更，请重新确认");
      };
      await confirm({
        title: `重新生成第 ${no} 集的分镜脚本？`,
        description:
          "这一集已经有生成结果，重新生成会清空已有片段（片段提示词和采用状态）。已生成的视频文件不会被删除。",
        confirmLabel: "清空并重新生成",
        destructive: true,
        onConfirm: async () => {
          checkConfirmation();
          if (!(await checkClearSnapshot(drama.id, no, episodeId, confirmedVersion))) stale = true;
          checkConfirmation();
          /** 请求可能已提交但回执丢失，因此当前弹窗只发送一次。 */
          sent = true;
          await submit(no, episodeId, { segmentVersion: confirmedVersion, segmentIds });
        },
      });
    },
    [drama.id, submit],
  );

  const start = useCallback(
    async (episode: EpisodeSummary) => {
      setPending(episode.no);
      try {
        if (!storyboardStart(episode).confirm) {
          try {
            await submit(episode.no, episode.id);
            return;
          } catch (e) {
            if (!isDramaError(e, DRAMA_ERR.storyboardHasTakes)) {
              toast.error(dramaErrorMessage(e, "提交失败，请稍后重试"));
              return;
            }
          }
        }
        await askClear(episode);
      } catch (e) {
        toast.error(e instanceof Error ? e.message : "无法确认重生成条件，请重试");
      } finally {
        setPending(null);
      }
    },
    [askClear, submit],
  );

  return { start, pending };
}

/** 「生成脚本 · n 积分」主按钮：估价随所选视频模型的时长上限变化 */
export function StoryboardButton({
  episode,
  label,
  disabledReason,
  maxSeconds,
  busy,
  onStart,
}: {
  episode: EpisodeSummary;
  label: string;
  disabledReason: string | null;
  maxSeconds: number;
  busy: boolean;
  onStart: () => void;
}) {
  const { drama } = useWorkspace();
  const estimate = useEstimate(drama.id, {
    step: "storyboard",
    episodes: [episode.no],
    maxSeconds,
  });
  return (
    <Button
      variant="outline"
      size="sm"
      disabled={!!disabledReason || busy}
      title={disabledReason ?? (estimate.status === "error" ? estimate.message : undefined)}
      onClick={onStart}
    >
      {busy && <Loader2 className="animate-spin" />}
      {label}
      {estimate.status === "ready" && (
        <>
          <span aria-hidden>·</span>
          <span className="text-credit tabular-nums">{estimate.credits}</span>
        </>
      )}
    </Button>
  );
}
