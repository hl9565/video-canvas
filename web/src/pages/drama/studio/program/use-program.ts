import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";

import {
  getEpisodeManifestV2,
  getEpisodeProgram,
  listEpisodeSegments,
  programCueAssetId,
  putEpisodeProgram,
} from "@/api/drama";
import {
  DRAMA_ERR,
  dramaErrorMessage,
  isDramaError,
  isManifestStaleError,
} from "@/api/drama/errors";
import type {
  EpisodeProgramView,
  ProgramAudioCueInput,
  ProgramDiagnostic,
  Segment,
} from "@/api/drama/type";
import { useDramaSegmentsStore } from "@/store/drama-segments";
import { programTtsCueId, useDramaProgramTtsStore } from "@/store/drama-program-tts";
import {
  mergeProgramAudio,
  programDiagnosticsFrom,
  programTimeline,
  validateProgram,
  type ProgramTimelineData,
} from "@/utils/drama/program";
import { ApiError } from "@/utils/requests/request";

type Draft = { v: 1; audio: ProgramAudioCueInput[]; overlays: [] };

type ProgramState = {
  key: string;
  /** 服务端最新确认的节目单，用于预览版本选择等实时视图。 */
  savedDocument: EpisodeProgramView["program"] | null;
  /** 本地草稿对应的原始基线，用于冲突时执行三方合并。 */
  baseDocument: EpisodeProgramView["program"] | null;
  draft: Draft | null;
  baseVersion: number | null;
  serverVersion: number | null;
  baseEpisodeId: string | null;
  rebasedTimeline: ProgramTimelineData | null;
  rebasedSegmentVersion: number | null;
  diagnostics: ProgramDiagnostic[];
  conflict: boolean;
  /** 需要用户明确确认覆盖时显示的原因。 */
  conflictMessage: string | null;
  dirty: boolean;
  saving: boolean;
  loading: boolean;
  error: string | null;
  saveSeq: number;
  editVersion: number;
  rebindIds: string[];
};

/** 面板与保存都以较新的片段版本决定是否还可使用 rebase 时间轴。 */
export function effectiveProgramTimeline(
  program: Pick<ProgramState, "rebasedTimeline" | "rebasedSegmentVersion" | "serverVersion">,
  liveSegments: readonly Segment[],
): ProgramTimelineData {
  const pinnedVersion = program.rebasedSegmentVersion;
  if (
    program.rebasedTimeline &&
    pinnedVersion !== null &&
    (program.serverVersion ?? -1) <= pinnedVersion &&
    !liveSegments.some((segment) => (segment.segmentVersion ?? -1) > pinnedVersion)
  )
    return program.rebasedTimeline;
  return programTimeline(liveSegments);
}

const empty = (key: string): ProgramState => ({
  key,
  savedDocument: null,
  baseDocument: null,
  draft: null,
  baseVersion: null,
  serverVersion: null,
  baseEpisodeId: null,
  rebasedTimeline: null,
  rebasedSegmentVersion: null,
  diagnostics: [],
  conflict: false,
  conflictMessage: null,
  dirty: false,
  saving: false,
  loading: false,
  error: null,
  saveSeq: 0,
  editVersion: 0,
  rebindIds: [],
});

const editableAudio = (audio: EpisodeProgramView["program"]["audio"]) =>
  (audio ?? []).map((cue) => ({ ...cue, asset_id: programCueAssetId(cue) }));

const fromView = (view: EpisodeProgramView) => ({
  savedDocument: view.program,
  baseDocument: view.program,
  baseVersion: view.segmentVersion,
  serverVersion: view.segmentVersion,
  baseEpisodeId: view.episodeId,
  rebasedTimeline: null,
  rebasedSegmentVersion: null,
  draft: {
    v: 1 as const,
    audio: editableAudio(view.program.audio),
    overlays: [] as [],
  },
});

/** 只保留仍指向用户勾选时同一镜头的重绑请求。 */
function retainedRebindIds(
  ids: string[],
  before: ProgramAudioCueInput[],
  after: ProgramAudioCueInput[],
) {
  return ids.filter((id) => {
    const segmentId = before.find((cue) => cue.id === id)?.anchor?.segment_id;
    return (
      segmentId !== undefined &&
      after.some((cue) => cue.id === id && cue.anchor?.segment_id === segmentId)
    );
  });
}

/** 恢复已认领但未保存的配音，已写入服务端的条目以服务端版本为准。 */
function restoreTtsDraft(view: EpisodeProgramView, discardDraft: boolean) {
  const next = fromView(view);
  if (discardDraft) return { ...next, dirty: false };
  const unsaved = useDramaProgramTtsStore.getState().byEpisode[view.episodeId]?.unsavedCues ?? [];
  const savedIds = new Set(next.draft.audio.map((cue) => cue.id));
  const restored = unsaved.filter((cue) => !savedIds.has(cue.id));
  return {
    ...next,
    draft: { ...next.draft, audio: [...next.draft.audio, ...restored] },
    dirty: restored.length > 0,
  };
}

/** 同一次挂载可以切集；网络结果必须匹配集身份及当前操作序号。 */
export function useProgram({
  dramaId,
  episodeId,
  episodeNo,
  enabled = true,
  visualStageC = false,
  segmentVersion,
  onSaved,
}: {
  dramaId: string;
  episodeId: string | undefined;
  episodeNo: number;
  enabled?: boolean;
  visualStageC?: boolean;
  segmentVersion?: number;
  onSaved?: (view: EpisodeProgramView) => void;
}) {
  const key = JSON.stringify([dramaId, episodeId, episodeNo]);
  const [state, setState] = useState(() => empty(key));
  const current = enabled && state.key === key ? state : empty(key);
  const operations = useRef({ key, seq: 0 });
  const savingScopeRef = useRef<{ scope: { key: string; seq: number }; seq: number } | null>(null);
  const latestServerRef = useRef<{ key: string; version: number } | null>(null);
  const syncedVersionRef = useRef<{ key: string; version: number } | null>(null);
  const autoSyncInFlightRef = useRef<{ key: string; version: number } | null>(null);
  const autoSyncFailedRef = useRef<{ key: string; version: number; attempts: number } | null>(null);
  const autoSyncScopeRef = useRef<AbortController | null>(null);
  const autoSyncRetryTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const onSavedRef = useRef(onSaved);
  useLayoutEffect(() => {
    onSavedRef.current = onSaved;
  }, [onSaved]);
  useLayoutEffect(() => {
    const scope = { key, seq: 0 };
    const controller = new AbortController();
    operations.current = scope;
    savingScopeRef.current = null;
    latestServerRef.current = null;
    syncedVersionRef.current = null;
    autoSyncInFlightRef.current = null;
    autoSyncFailedRef.current = null;
    autoSyncScopeRef.current = controller;
    return () => {
      scope.seq++;
      savingScopeRef.current = null;
      controller.abort();
      if (autoSyncRetryTimerRef.current !== null) {
        clearTimeout(autoSyncRetryTimerRef.current);
        autoSyncRetryTimerRef.current = null;
      }
      if (autoSyncScopeRef.current === controller) {
        autoSyncScopeRef.current = null;
      }
    };
  }, [enabled, key]);

  /** 仅在 React 实际提交了服务端快照后登记已同步版本，防止被延迟的 updater 拒绝后提前抑制自动同步。 */
  useLayoutEffect(() => {
    if (
      !enabled ||
      !episodeId ||
      current.key !== key ||
      current.baseEpisodeId !== episodeId ||
      current.serverVersion === null
    ) {
      return;
    }
    const prevSynced =
      syncedVersionRef.current?.key === key ? syncedVersionRef.current.version : -1;
    if (current.serverVersion > prevSynced) {
      syncedVersionRef.current = { key, version: current.serverVersion };
    }
    if (
      autoSyncFailedRef.current?.key === key &&
      current.serverVersion >= autoSyncFailedRef.current.version
    ) {
      autoSyncFailedRef.current = null;
    }
  }, [current.baseEpisodeId, current.key, current.serverVersion, enabled, episodeId, key]);

  const noteLatestServerVersion = useCallback(
    (version: number) => {
      const prevLatest =
        latestServerRef.current?.key === key ? latestServerRef.current.version : -1;
      latestServerRef.current = { key, version: Math.max(prevLatest, version) };
    },
    [key],
  );

  const read = useCallback(
    async (signal?: AbortSignal, discardDraft = false) => {
      if (!enabled || !episodeId) return;
      const scope = operations.current;
      const seq = ++scope.seq;
      setState((prev) => ({
        ...(prev.key === key ? prev : empty(key)),
        loading: true,
        saving: false,
        error: null,
        saveSeq: seq,
      }));
      try {
        const view = await getEpisodeProgram(dramaId, episodeNo, { signal });
        if (operations.current !== scope || seq !== scope.seq) return;
        if (view.episodeId !== episodeId) throw new Error("该集已被替换，请刷新工作台");
        if (
          latestServerRef.current?.key === key &&
          view.segmentVersion < latestServerRef.current.version
        ) {
          setState((prev) =>
            prev.key === key && prev.loading ? { ...prev, loading: false } : prev,
          );
          return;
        }
        noteLatestServerVersion(view.segmentVersion);
        setState((prev) => {
          if (prev.key !== key) return prev;
          if (prev.serverVersion !== null && view.segmentVersion < prev.serverVersion) {
            return prev.loading ? { ...prev, loading: false } : prev;
          }
          return { ...empty(key), ...restoreTtsDraft(view, discardDraft), saveSeq: seq };
        });
      } catch (error) {
        if (operations.current !== scope || seq !== scope.seq) return;
        setState((prev) => ({
          ...prev,
          loading: false,
          error: dramaErrorMessage(
            error,
            error instanceof Error ? error.message : "读取节目单失败",
          ),
        }));
      }
    },
    [dramaId, enabled, episodeId, episodeNo, key, noteLatestServerVersion],
  );

  const sync = useCallback(
    async (signal?: AbortSignal): Promise<boolean> => {
      if (!enabled || !episodeId) return true;
      if (savingScopeRef.current !== null) return false;
      const scope = operations.current;
      const startSeq = scope.seq;
      try {
        const view = await getEpisodeProgram(dramaId, episodeNo, { signal });
        if (
          operations.current !== scope ||
          scope.seq !== startSeq ||
          savingScopeRef.current !== null
        )
          return false;
        if (view.episodeId !== episodeId) return true;
        if (
          latestServerRef.current?.key === key &&
          view.segmentVersion < latestServerRef.current.version
        ) {
          return true;
        }
        noteLatestServerVersion(view.segmentVersion);
        setState((prev) => {
          if (prev.key !== key || prev.saving) return prev;
          if (prev.baseEpisodeId && prev.baseEpisodeId !== view.episodeId) return prev;
          if (prev.serverVersion !== null && view.segmentVersion < prev.serverVersion) return prev;
          if (!prev.dirty && !prev.rebindIds.length && !prev.conflict) {
            return { ...empty(key), ...restoreTtsDraft(view, false), saveSeq: scope.seq };
          }
          return {
            ...prev,
            savedDocument: view.program,
            serverVersion: view.segmentVersion,
            ...(prev.rebasedSegmentVersion !== null &&
            view.segmentVersion > prev.rebasedSegmentVersion
              ? { rebasedTimeline: null, rebasedSegmentVersion: null }
              : {}),
          };
        });
        return true;
      } catch {
        // 后台同步失败时不覆盖当前草稿，也不提前标记已同步，以便后续同版本重渲染或重试恢复。
        return false;
      }
    },
    [dramaId, enabled, episodeId, episodeNo, key, noteLatestServerVersion],
  );

  useEffect(() => {
    const controller = new AbortController();
    void Promise.resolve().then(() => {
      if (!controller.signal.aborted) return read(controller.signal);
    });
    return () => controller.abort();
  }, [read]);

  const scheduleAutoSync = useCallback(
    function runAutoSync(targetVersion: number) {
      if (
        !enabled ||
        !episodeId ||
        current.baseEpisodeId !== episodeId ||
        current.serverVersion === null ||
        targetVersion <= current.serverVersion ||
        current.loading ||
        current.saving
      ) {
        return;
      }
      if (
        (syncedVersionRef.current?.key === key &&
          syncedVersionRef.current.version >= targetVersion) ||
        (autoSyncInFlightRef.current?.key === key &&
          autoSyncInFlightRef.current.version >= targetVersion)
      ) {
        return;
      }
      const controller = autoSyncScopeRef.current;
      if (!controller || controller.signal.aborted) return;
      if (autoSyncRetryTimerRef.current !== null) {
        clearTimeout(autoSyncRetryTimerRef.current);
        autoSyncRetryTimerRef.current = null;
      }
      const token = { key, version: targetVersion };
      autoSyncInFlightRef.current = token;
      void Promise.resolve()
        .then(() => {
          if (controller.signal.aborted) return true;
          return sync(controller.signal);
        })
        .then((ok) => {
          if (autoSyncInFlightRef.current === token) {
            autoSyncInFlightRef.current = null;
          }
          if (controller.signal.aborted) return;
          if (ok) {
            if (
              autoSyncFailedRef.current?.key === key &&
              autoSyncFailedRef.current.version <= targetVersion
            ) {
              autoSyncFailedRef.current = null;
            }
            return;
          }
          const prevAttempts =
            autoSyncFailedRef.current?.key === key &&
            autoSyncFailedRef.current.version === targetVersion
              ? autoSyncFailedRef.current.attempts
              : 0;
          const attempts = prevAttempts + 1;
          autoSyncFailedRef.current = { key, version: targetVersion, attempts };
          if (attempts <= 2) {
            autoSyncRetryTimerRef.current = setTimeout(() => {
              autoSyncRetryTimerRef.current = null;
              if (!controller.signal.aborted) {
                runAutoSync(targetVersion);
              }
            }, 25 * attempts);
          }
        });
    },
    [
      current.baseEpisodeId,
      current.loading,
      current.saving,
      current.serverVersion,
      enabled,
      episodeId,
      key,
      sync,
    ],
  );

  /** 远端增改节目单使集版本前进时同步 savedDocument，同时完整保留本地未保存草稿。 */
  useEffect(() => {
    if (segmentVersion === undefined) return;
    scheduleAutoSync(segmentVersion);
  }, [scheduleAutoSync, segmentVersion]);

  /** 上次同版本自动同步瞬态失败后，组件在相同 segmentVersion 下重渲染时立即重新尝试恢复。 */
  useEffect(() => {
    if (segmentVersion === undefined) return;
    const failed = autoSyncFailedRef.current;
    if (
      failed &&
      failed.key === key &&
      failed.version === segmentVersion &&
      failed.attempts < 5 &&
      !autoSyncInFlightRef.current
    ) {
      scheduleAutoSync(segmentVersion);
    }
  });

  /** 认领只发生一次；单独保留未保存的配音，避免切集时丢失已付费产物。 */
  useLayoutEffect(() => {
    if (
      !episodeId ||
      current.baseEpisodeId !== episodeId ||
      current.loading ||
      !current.draft ||
      !current.savedDocument
    )
      return;
    const { byEpisode, patch } = useDramaProgramTtsStore.getState();
    const entry = byEpisode[episodeId];
    if (!entry?.attachedTaskIds.length) return;
    const ttsIds = new Set(entry.attachedTaskIds.map(programTtsCueId));
    const savedIds = new Set((current.savedDocument.audio ?? []).map((cue) => cue.id));
    const unsavedCues = current.draft.audio.filter(
      (cue) => ttsIds.has(cue.id) && !savedIds.has(cue.id),
    );
    if (
      unsavedCues.length !== entry.unsavedCues.length ||
      unsavedCues.some((cue, index) => cue !== entry.unsavedCues[index])
    ) {
      patch(episodeId, { unsavedCues });
    }
  }, [episodeId, current.baseEpisodeId, current.draft, current.loading, current.savedDocument]);

  const refreshDiagnostics =
    !!current.savedDocument && (visualStageC || (current.savedDocument.audio?.length ?? 0) > 0);
  useEffect(() => {
    if (!enabled || !episodeId || !refreshDiagnostics || current.saving || current.loading) return;
    const controller = new AbortController();
    const scope = operations.current;
    const seq = scope.seq;
    const acceptDiagnostics = (diagnostics: ProgramDiagnostic[]) => {
      if (controller.signal.aborted || scope !== operations.current || seq !== scope.seq) return;
      setState((prev) =>
        prev.key === key
          ? {
              ...prev,
              diagnostics,
              rebindIds: prev.rebindIds.filter((id) =>
                diagnostics.some(
                  (issue) => issue.cue_id === id && issue.reason === "anchor_source_replaced",
                ),
              ),
            }
          : prev,
      );
    };
    void getEpisodeManifestV2(dramaId, episodeNo, { signal: controller.signal })
      .then((response) => {
        if (
          response.status === "preparing" ||
          (response.manifest && response.manifest.episodeId !== episodeId)
        )
          return;
        acceptDiagnostics(response.programDiagnostics ?? []);
      })
      .catch((error: unknown) => {
        const diagnostics = programDiagnosticsFrom(error);
        if (diagnostics.length) acceptDiagnostics(diagnostics);
      });
    return () => controller.abort();
  }, [
    current.baseVersion,
    current.saving,
    current.loading,
    dramaId,
    enabled,
    episodeId,
    episodeNo,
    key,
    refreshDiagnostics,
    segmentVersion,
  ]);

  const editAudio = useCallback(
    (edit: (cues: ProgramAudioCueInput[]) => ProgramAudioCueInput[]) => {
      setState((prev) => {
        if (prev.key !== key || !prev.draft) return prev;
        const audio = edit(prev.draft.audio);
        if (audio === prev.draft.audio) return prev;
        return {
          ...prev,
          draft: { v: 1, audio, overlays: [] },
          dirty: true,
          editVersion: prev.editVersion + 1,
          error: null,
          rebindIds: retainedRebindIds(prev.rebindIds, prev.draft.audio, audio),
        };
      });
    },
    [key],
  );

  const toggleRebind = useCallback(
    (id: string, checked: boolean) => {
      setState((prev) => {
        if (
          prev.key !== key ||
          prev.rebindIds.includes(id) === checked ||
          !prev.diagnostics.some(
            (issue) => issue.cue_id === id && issue.reason === "anchor_source_replaced",
          )
        )
          return prev;
        return {
          ...prev,
          rebindIds: checked
            ? [...prev.rebindIds, id]
            : prev.rebindIds.filter((item) => item !== id),
        };
      });
    },
    [key],
  );

  const write = async (mode: "save" | "rebase" | "force", liveSegments: readonly Segment[]) => {
    if (
      !episodeId ||
      current.baseEpisodeId !== episodeId ||
      !current.draft ||
      !current.savedDocument ||
      current.baseVersion === null ||
      current.loading ||
      current.saving ||
      (mode === "save" && current.conflict) ||
      (mode === "rebase" && (!current.conflict || current.conflictMessage)) ||
      (mode === "force" && !current.conflictMessage) ||
      (mode !== "rebase" &&
        validateProgram(current.draft.audio, effectiveProgramTimeline(current, liveSegments))
          .length) ||
      (!current.dirty && !current.rebindIds.length)
    )
      return;
    const local = current.draft;
    const base = current.baseDocument ?? current.savedDocument;
    const startEditVersion = current.editVersion;
    const scope = operations.current;
    const seq = ++scope.seq;
    const active = () => operations.current === scope && scope.seq === seq;
    savingScopeRef.current = { scope, seq };
    setState((prev) => ({ ...prev, saving: true, saveSeq: seq, error: null }));
    try {
      let version = current.baseVersion;
      let audio = local.audio;
      if (mode === "rebase") {
        let latest: EpisodeProgramView | null = null;
        let latestSegments: Segment[] = [];
        for (let attempt = 0; attempt < 3; attempt++) {
          const nextView = await getEpisodeProgram(dramaId, episodeNo);
          if (!active()) return;
          if (nextView.episodeId !== episodeId) {
            throw new ApiError("该集已被替换，请刷新工作台", 91001, 409);
          }
          const nextSegSnapshot = await listEpisodeSegments(dramaId, episodeNo);
          if (!active()) return;
          if (
            nextSegSnapshot.episodeId !== episodeId ||
            nextSegSnapshot.items.some((seg) => seg.episodeId && seg.episodeId !== episodeId)
          ) {
            throw new ApiError("该集已被替换，请刷新工作台", 91001, 409);
          }
          const storeSnapshot = useDramaSegmentsStore.getState().snapshotOf(dramaId, episodeNo);
          const minKnownVersion = Math.max(
            latestServerRef.current?.key === key ? latestServerRef.current.version : -1,
            storeSnapshot && (!storeSnapshot.episodeId || storeSnapshot.episodeId === episodeId)
              ? (storeSnapshot.segmentVersion ?? -1)
              : -1,
          );
          const versions = nextSegSnapshot.items
            .map((seg) => seg.segmentVersion)
            .filter((v): v is number => typeof v === "number");
          if (
            nextSegSnapshot.segmentVersion === nextView.segmentVersion &&
            versions.every((v) => v === nextView.segmentVersion) &&
            nextView.segmentVersion >= minKnownVersion
          ) {
            latest = nextView;
            latestSegments = nextSegSnapshot.items;
            break;
          }
        }
        if (!latest) {
          throw new ApiError("分集与节目单版本尚在同步，请稍后重试", 91001, 409);
        }
        const accepted = useDramaSegmentsStore
          .getState()
          .setList(dramaId, episodeNo, latestSegments, undefined, {
            episodeId,
            segmentVersion: latest.segmentVersion,
            expectedEpisodeId: episodeId,
          });
        if (!accepted) {
          throw new ApiError("分集与节目单版本尚在同步，请稍后重试", 91001, 409);
        }
        const latestTimeline = programTimeline(latestSegments);
        const next = fromView(latest);
        const merged = mergeProgramAudio(editableAudio(base.audio), local.audio, next.draft.audio);
        const conflictMessage = merged.hasConflict
          ? "同一音频条目在本地和服务端均有不同改动，确认后将采用本地改动。"
          : validateProgram(merged.audio, latestTimeline).length
            ? "合并后的节目音频未通过校验，请修正条目后再确认覆盖。"
            : null;
        noteLatestServerVersion(latest.segmentVersion);
        setState((prev) => {
          if (
            prev.key !== key ||
            (prev.serverVersion !== null && latest.segmentVersion < prev.serverVersion)
          ) {
            return prev;
          }
          const edited = prev.editVersion !== startEditVersion && prev.draft ? prev.draft : null;
          const reviewed = edited
            ? mergeProgramAudio(local.audio, edited.audio, merged.audio).audio
            : merged.audio;
          return {
            ...prev,
            ...next,
            rebasedTimeline: latestTimeline,
            rebasedSegmentVersion: latest.segmentVersion,
            draft: { ...next.draft, audio: reviewed },
            dirty: true,
            saving: !conflictMessage,
            conflict: conflictMessage ? prev.conflict : false,
            conflictMessage,
            rebindIds: retainedRebindIds(prev.rebindIds, (edited ?? local).audio, reviewed),
          };
        });
        if (conflictMessage) return;
        version = next.baseVersion;
        audio = merged.audio;
      }
      const submittedRebindIds = retainedRebindIds(current.rebindIds, local.audio, audio).filter(
        (id) =>
          current.diagnostics.some(
            (issue) => issue.cue_id === id && issue.reason === "anchor_source_replaced",
          ),
      );
      const view = await putEpisodeProgram(dramaId, episodeNo, {
        expected_episode_id: episodeId,
        expected_segment_version: version,
        program: { v: 1, audio, overlays: [] },
        ...(submittedRebindIds.length ? { rebind_anchor_cue_ids: submittedRebindIds } : {}),
      });
      if (!active()) return;
      if (view.episodeId !== episodeId)
        throw new ApiError("该集已被替换，请刷新工作台", 91001, 409);
      noteLatestServerVersion(view.segmentVersion);
      setState((prev) => {
        const edited = prev.draft && prev.editVersion !== startEditVersion ? prev.draft : null;
        const next = fromView(view);
        const draft = edited
          ? { ...edited, audio: mergeProgramAudio(audio, edited.audio, next.draft.audio).audio }
          : next.draft;
        /** 只清除本次 PUT 已提交的重绑，不把未提交的勾选算作音频编辑。 */
        const rebindIds = retainedRebindIds(
          prev.rebindIds.filter((id) => !submittedRebindIds.includes(id)),
          (prev.draft ?? local).audio,
          draft.audio,
        );
        return {
          ...empty(key),
          ...next,
          saveSeq: seq,
          editVersion: prev.editVersion,
          draft,
          dirty: !!edited,
          rebindIds,
          diagnostics: prev.diagnostics.filter(
            (issue) =>
              issue.reason === "anchor_source_replaced" &&
              issue.cue_id !== undefined &&
              rebindIds.includes(issue.cue_id),
          ),
        };
      });
      onSavedRef.current?.(view);
    } catch (error) {
      if (!active()) return;
      const issues = programDiagnosticsFrom(error);
      const versionConflict =
        isManifestStaleError(error) ||
        (error instanceof ApiError &&
          error.status === 409 &&
          !issues.length &&
          !isDramaError(error, DRAMA_ERR.anchorInvalid) &&
          !isDramaError(error, DRAMA_ERR.mediaUnavailable));
      setState((prev) => ({
        ...prev,
        saving: false,
        conflict: prev.conflict || versionConflict,
        conflictMessage: versionConflict ? null : prev.conflictMessage,
        diagnostics: issues.length ? issues : prev.diagnostics,
        error: dramaErrorMessage(error, "保存节目单失败"),
      }));
    } finally {
      if (savingScopeRef.current?.scope === scope && savingScopeRef.current.seq === seq) {
        savingScopeRef.current = null;
      }
    }
  };

  const save = (liveSegments: readonly Segment[], rebase = false) =>
    write(rebase ? "rebase" : "save", liveSegments);
  const forceOverwrite = (liveSegments: readonly Segment[]) => write("force", liveSegments);
  const reload = (discardDraft = false) => read(undefined, discardDraft);
  return {
    ...current,
    editAudio,
    toggleRebind,
    save,
    forceOverwrite,
    reload,
    sync,
  };
}

export type ProgramStateApi = ReturnType<typeof useProgram>;
