import { useId, useRef, useState } from "react";

import { dramaErrorMessage } from "@/api/drama/errors";
import type { PatchTakeQCInput, Segment } from "@/api/drama/type";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";

/** 驳回当前采用项时，必须由用户明确选择替换或清空。 */
export interface RejectTakeDialogProps {
  /** 是否打开 */
  open: boolean;
  /** 弹窗开关 */
  onOpenChange: (open: boolean) => void;
  /** 候选所属片段 */
  segment: Segment | null;
  /** 要驳回的任务 */
  taskId: string | null;
  /** 集级 CAS 版本 */
  expectedSegmentVersion?: number;
  /** 由 Studio 串行更新片段 */
  onReject: (input: Omit<PatchTakeQCInput, "revision">) => Promise<unknown>;
}

function RejectBody({
  segment,
  taskId,
  expectedSegmentVersion,
  onReject,
  onOpenChange,
}: RejectTakeDialogProps & { segment: Segment; taskId: string }) {
  const uid = useId();
  const [mode, setMode] = useState<"" | "replace" | "clear">("");
  const [replacementTaskId, setReplacementTaskId] = useState("");
  const [resetEdit, setResetEdit] = useState(true);
  const [busy, setBusy] = useState(false);
  const pending = useRef(false);
  const [error, setError] = useState<string | null>(null);
  const replacements = segment.takes.filter(
    (take) =>
      take.taskId !== taskId &&
      take.status === "succeeded" &&
      !!take.asset &&
      take.qc !== "rejected",
  );
  const valid =
    mode === "clear" ||
    (mode === "replace" && replacements.some((take) => take.taskId === replacementTaskId));
  const submit = async () => {
    if (!valid || pending.current) return;
    pending.current = true;
    setBusy(true);
    setError(null);
    try {
      await onReject({
        qc: "rejected",
        expectedSegmentVersion,
        ...(mode === "clear"
          ? { clearChosen: true }
          : { replacementTaskId, resetEditOnTaskChange: resetEdit }),
      });
      onOpenChange(false);
    } catch (e) {
      setError(dramaErrorMessage(e, "驳回失败，请重试"));
    } finally {
      pending.current = false;
      setBusy(false);
    }
  };
  return (
    <>
      <div className="min-h-0 flex-1 space-y-4 overflow-y-auto">
        <fieldset disabled={busy} className="space-y-3">
          <label className="flex min-h-10 items-center gap-2">
            <input
              type="radio"
              name={uid}
              value="replace"
              checked={mode === "replace"}
              onChange={() => setMode("replace")}
            />
            替换为其他候选
          </label>
          {mode === "replace" && (
            <div className="space-y-3 pl-6">
              <select
                aria-label="替换候选"
                className="bg-chrome-hover border-chrome-border min-h-10 w-full rounded-xl border px-3"
                value={replacementTaskId}
                onChange={(e) => setReplacementTaskId(e.target.value)}
              >
                <option value="">请选择替换候选</option>
                {replacements.map((take) => (
                  <option key={take.taskId} value={take.taskId}>
                    第 {segment.takes.indexOf(take) + 1} 版 · {take.taskId}
                  </option>
                ))}
              </select>
              <label className="flex min-h-10 items-center gap-2">
                <input
                  type="checkbox"
                  checked={resetEdit}
                  onChange={(e) => setResetEdit(e.target.checked)}
                />
                重置剪辑设置
              </label>
            </div>
          )}
          <label className="flex min-h-10 items-center gap-2">
            <input
              type="radio"
              name={uid}
              value="clear"
              checked={mode === "clear"}
              onChange={() => setMode("clear")}
            />
            清空当前采用
          </label>
        </fieldset>
        {error && (
          <p role="alert" className="text-destructive text-sm">
            {error}
          </p>
        )}
      </div>
      <DialogFooter className="bg-transparent">
        <Button variant="outline" disabled={busy} onClick={() => onOpenChange(false)}>
          取消
        </Button>
        <Button variant="destructive" disabled={busy || !valid} onClick={() => void submit()}>
          确认驳回
        </Button>
      </DialogFooter>
    </>
  );
}

/** 弹窗常驻；表单按每次打开重新挂载，不将 revision 当作会话 key。 */
export function RejectTakeDialog(props: RejectTakeDialogProps) {
  return (
    <Dialog open={props.open} onOpenChange={props.onOpenChange}>
      <DialogContent className="bg-chrome flex max-h-[88svh] flex-col rounded-3xl backdrop-blur-xl sm:max-w-md">
        <DialogHeader>
          <DialogTitle>驳回当前采用候选</DialogTitle>
          <DialogDescription>请选择替换候选或清空当前采用</DialogDescription>
        </DialogHeader>
        {props.open && props.segment && props.taskId && (
          <RejectBody
            key={`${props.segment.id}:${props.taskId}`}
            {...props}
            segment={props.segment}
            taskId={props.taskId}
          />
        )}
      </DialogContent>
    </Dialog>
  );
}
