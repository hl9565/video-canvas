import { useEffect, useRef, useState } from "react";
import { Loader2 } from "lucide-react";

import { getCanvas, getCanvasList } from "@/api/canvas";
import type { CanvasGraphDto, CanvasListItemDto } from "@/api/canvas/type";
import { dramaErrorMessage } from "@/api/drama/errors";
import type { ImportSegmentTakesInput, Segment } from "@/api/drama/type";
import { getGenerationTasksByIds } from "@/api/generation-task";
import type { TaskView } from "@/api/generation-task/type";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";

/** 导入只补入候选；自动采用限单条，且尊重已有的人工驳回。 */
export interface ImportTakesDialogProps {
  /** 是否打开 */
  open: boolean;
  /** 弹窗开关 */
  onOpenChange: (open: boolean) => void;
  /** 当前片段 */
  segment: Segment | null;
  /** 默认选中的短剧画布 */
  defaultCanvasId?: string;
  /** 由 Studio 同步版本、处理冲突 */
  onImport: (input: Omit<ImportSegmentTakesInput, "revision">) => Promise<unknown>;
}

/** 兼容历史输出字段尚未进入持久化节点类型的画布数据。 */
function canvasTaskIds(nodes: CanvasGraphDto["nodes"]): string[] {
  const ids = new Set<string>();
  const add = (id: unknown) => {
    if ((typeof id === "string" && id.trim()) || (typeof id === "number" && id > 0))
      ids.add(String(id));
  };
  for (const node of nodes) {
    const data = node.data as Record<string, unknown>;
    add(data.taskId);
    if (Array.isArray(data.outputs))
      for (const output of data.outputs) {
        if (output && typeof output === "object" && "taskId" in output) add(output.taskId);
      }
  }
  return [...ids];
}

const takeRefOf = (view: TaskView): string =>
  view.task_ref?.trim() ? view.task_ref.trim() : String(view.id);

/** 不相信画布节点的展示状态，必须读取服务端任务状态和素材产出。 */
const importable = (task: TaskView, canvasId: string): boolean =>
  task.status === "succeeded" &&
  task.kind === "video" &&
  task.canvas_id === canvasId &&
  !!task.outputs?.some((output) => output.asset_id);

function ImportBody({
  segment,
  defaultCanvasId,
  onImport,
  onOpenChange,
}: ImportTakesDialogProps & { segment: Segment }) {
  const [canvases, setCanvases] = useState<CanvasListItemDto[]>([]);
  const [canvasId, setCanvasId] = useState(defaultCanvasId ?? "");
  const [tasks, setTasks] = useState<TaskView[]>([]);
  const [selected, setSelected] = useState<string[]>([]);
  const [manual, setManual] = useState(false);
  const [manualId, setManualId] = useState("");
  const [adopt, setAdopt] = useState(false);
  const [resetEdit, setResetEdit] = useState(true);
  const [loading, setLoading] = useState(true);
  const [resolvedCanvasId, setResolvedCanvasId] = useState("");
  const [manualLookupCanvas, setManualLookupCanvas] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const pending = useRef(false);
  const canvasRef = useRef(canvasId);
  useEffect(() => {
    canvasRef.current = canvasId;
  }, [canvasId]);
  const [error, setError] = useState<string | null>(null);
  const manualResolving = !!canvasId && manualLookupCanvas === canvasId;
  const resolving = manualResolving || (!!canvasId && resolvedCanvasId !== canvasId);
  const selectCanvas = (nextId: string) => {
    setCanvasId(nextId);
    setTasks([]);
    setSelected([]);
    setAdopt(false);
    setManualId("");
    setError(null);
    setManualLookupCanvas(null);
    if (!nextId) setResolvedCanvasId("");
  };
  useEffect(() => {
    let active = true;
    const load = async () => {
      const all: CanvasListItemDto[] = [];
      let page = 1;
      do {
        const response = await getCanvasList({ page, page_size: 100 });
        all.push(...response.items);
        if (!response.nextCursor) break;
        page = Number(response.nextCursor);
      } while (active);
      if (active) {
        setCanvases(all);
        setLoading(false);
      }
    };
    void load().catch((e: unknown) => {
      if (active) {
        setError(dramaErrorMessage(e, "画布列表加载失败"));
        setLoading(false);
      }
    });
    return () => {
      active = false;
    };
  }, []);
  useEffect(() => {
    if (!canvasId) return;
    let active = true;
    void getCanvas(canvasId)
      .then((canvas) => getGenerationTasksByIds(canvasTaskIds(canvas.graph.nodes)))
      .then((views) => {
        if (active) setTasks(views.filter((view) => importable(view, canvasId)));
      })
      .catch((e: unknown) => {
        if (active) setError(dramaErrorMessage(e, "读取画布视频失败"));
      })
      .finally(() => {
        if (active) setResolvedCanvasId(canvasId);
      });
    return () => {
      active = false;
    };
  }, [canvasId]);
  const rejected =
    selected.length === 1 &&
    segment.takes.some((take) => take.taskId === selected[0] && take.qc === "rejected");
  const canAdopt = selected.length === 1 && !rejected;
  const toggle = (id: string) => {
    setSelected((previous) =>
      previous.includes(id) ? previous.filter((value) => value !== id) : [...previous, id],
    );
    setAdopt(false);
  };
  const findManual = async () => {
    if (!canvasId || !manualId.trim() || resolving) return;
    const requestedCanvas = canvasId;
    setManualLookupCanvas(requestedCanvas);
    setError(null);
    try {
      const views = (await getGenerationTasksByIds([manualId.trim()])).filter((view) =>
        importable(view, requestedCanvas),
      );
      if (canvasRef.current !== requestedCanvas) return;
      if (!views.length) {
        setError("只能导入此画布已成功且有素材的视频任务");
        return;
      }
      setTasks((previous) => [
        ...new Map([...previous, ...views].map((view) => [takeRefOf(view), view])).values(),
      ]);
      setSelected([takeRefOf(views[0]!)]);
      setAdopt(false);
    } catch (e) {
      if (canvasRef.current === requestedCanvas) setError(dramaErrorMessage(e, "读取任务失败"));
    } finally {
      setManualLookupCanvas((current) => (current === requestedCanvas ? null : current));
    }
  };
  const submit = async () => {
    if (pending.current || !canvasId || !selected.length || (adopt && !canAdopt)) return;
    pending.current = true;
    setSaving(true);
    setError(null);
    try {
      await onImport({
        sourceCanvasId: canvasId,
        taskIds: selected,
        adopt: adopt && canAdopt,
        ...(adopt ? { resetEditOnTaskChange: resetEdit } : {}),
      });
      onOpenChange(false);
    } catch (e) {
      setError(dramaErrorMessage(e, "导入失败，请重试"));
    } finally {
      pending.current = false;
      setSaving(false);
    }
  };
  return (
    <>
      <div className="min-h-0 flex-1 space-y-4 overflow-y-auto">
        <label className="grid gap-1.5">
          来源画布
          <select
            aria-label="来源画布"
            className="bg-chrome-hover border-chrome-border min-h-10 rounded-xl border px-3"
            value={canvasId}
            disabled={loading || saving}
            onChange={(e) => selectCanvas(e.target.value)}
          >
            <option value="">选择画布</option>
            {defaultCanvasId && !canvases.some((canvas) => canvas.id === defaultCanvasId) && (
              <option value={defaultCanvasId}>短剧画布</option>
            )}
            {canvases.map((canvas) => (
              <option key={canvas.id} value={canvas.id}>
                {canvas.title}
              </option>
            ))}
          </select>
        </label>
        {resolving && (
          <p role="status" className="flex items-center gap-2">
            <Loader2 className="size-4 animate-spin" />
            读取视频候选…
          </p>
        )}
        <div role="group" aria-label="画布视频候选" className="space-y-2">
          {tasks.map((view) => {
            const ref = takeRefOf(view);
            const output = view.outputs?.find((item) => item.asset_id);
            return (
              <label
                key={ref}
                className="bg-chrome-hover flex min-h-12 cursor-pointer items-center gap-3 rounded-xl px-3 py-2"
              >
                <input
                  type="checkbox"
                  name="import-task"
                  value={ref}
                  checked={selected.includes(ref)}
                  disabled={saving || resolving}
                  onChange={() => toggle(ref)}
                />
                <span className="min-w-0 flex-1 truncate">
                  {view.prompt || `视频任务 ${view.id}`}
                </span>
                {output?.duration_ms && (
                  <span className="text-muted-foreground text-xs">
                    {output.duration_ms / 1000}s
                  </span>
                )}
              </label>
            );
          })}
        </div>
        {canvasId && !resolving && tasks.length === 0 && (
          <p className="text-muted-foreground text-sm">暂无可导入的视频</p>
        )}
        <label className="flex min-h-10 items-center gap-2">
          <input
            type="checkbox"
            checked={manual}
            disabled={saving}
            onChange={(e) => setManual(e.target.checked)}
          />
          手动输入 task_id
        </label>
        {manual && (
          <div className="flex gap-2">
            <Input
              aria-label="手动 task_id"
              value={manualId}
              disabled={saving}
              onChange={(e) => setManualId(e.target.value)}
            />
            <Button
              variant="outline"
              disabled={!canvasId || !manualId.trim() || resolving || saving}
              onClick={() => void findManual()}
            >
              查找任务
            </Button>
          </div>
        )}
        <label className="flex min-h-10 items-center gap-2">
          <input
            type="checkbox"
            aria-label="导入后立即采用"
            checked={adopt && canAdopt}
            disabled={!canAdopt || saving}
            onChange={(e) => setAdopt(e.target.checked)}
          />
          导入后立即采用
        </label>
        {adopt && canAdopt && (
          <label className="flex min-h-10 items-center gap-2">
            <input
              type="checkbox"
              checked={resetEdit}
              disabled={saving}
              onChange={(e) => setResetEdit(e.target.checked)}
            />
            重置剪辑设置
          </label>
        )}
        {rejected && (
          <p role="status" className="text-status-warning text-sm">
            该候选已被驳回，无法直接采用
          </p>
        )}
        {error && (
          <p role="alert" className="text-destructive text-sm">
            {error}
          </p>
        )}
      </div>
      <DialogFooter className="bg-transparent">
        <Button variant="outline" disabled={saving} onClick={() => onOpenChange(false)}>
          取消
        </Button>
        <Button
          disabled={
            saving || resolving || !canvasId || selected.length === 0 || (adopt && !canAdopt)
          }
          onClick={() => void submit()}
        >
          {saving && <Loader2 className="animate-spin" />}导入候选
        </Button>
      </DialogFooter>
    </>
  );
}

/** 弹窗关闭时保留外壳退出动画；每次打开重新读取画布与任务。 */
export function ImportTakesDialog(props: ImportTakesDialogProps) {
  return (
    <Dialog open={props.open} onOpenChange={props.onOpenChange}>
      <DialogContent className="bg-chrome flex max-h-[min(88svh,720px)] flex-col rounded-3xl backdrop-blur-xl sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>导入画布视频</DialogTitle>
          <DialogDescription>片段 {props.segment ? props.segment.seq + 1 : ""}</DialogDescription>
        </DialogHeader>
        {props.open && props.segment && (
          <ImportBody key={props.segment.id} {...props} segment={props.segment} />
        )}
      </DialogContent>
    </Dialog>
  );
}
