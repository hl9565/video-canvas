/**
 * 提示词时间轴解析与时长越界检查，逻辑与后端 internal/drama/storyboard.go TimelineOverrun 保持一致：
 * 只识别明确的时间区间终点（`0-3s`、`0~3秒`）和 H3 切点（`At 00:03.500`）；
 * 排除时钟区间（`00:10-00:20`）与非秒单位字母后缀（`24-60fps`、`0-3sec`）。
 */

const TIMELINE_RANGE_RE = /(\d+(?:\.\d+)?)\s*[-~～–—至到]\s*(\d+(?:\.\d+)?)\s*(?:秒|[sS])/g;
const TIMELINE_CUT_RE = /\bAt\s+(\d{1,2}):(\d{2})(?:\.(\d{1,3}))?/g;

function clockOrFraction(text: string, index: number): boolean {
  if (index <= 0) return false;
  const prev = text[index - 1];
  return prev === ":" || prev === ".";
}

function letterAfter(text: string, endIndex: number): boolean {
  if (endIndex >= text.length) return false;
  const code = text.charCodeAt(endIndex);
  return (code >= 65 && code <= 90) || (code >= 97 && code <= 122);
}

/** 格式化展示时间（去掉无意义尾零，如 4.000 -> 4，3.500 -> 3.5） */
export function formatTimelineSeconds(value: number): string {
  return Number.isFinite(value) ? String(Number(value.toFixed(3))) : "0";
}

/**
 * 判断提示词时间轴是否超出目标时长（seconds）；
 * 允许 1ms 浮点容差（与后端 `float64(seconds) + 0.001` 一致）。
 */
export function timelineOverrun(
  prompt: string,
  seconds: number,
): { overrun: boolean; end: number } {
  if (seconds <= 0 || !Number.isFinite(seconds) || !prompt) {
    return { overrun: false, end: 0 };
  }
  const limit = seconds + 0.001;
  let worst = 0;

  for (const match of prompt.matchAll(TIMELINE_RANGE_RE)) {
    const startIdx = match.index ?? 0;
    const endIdx = startIdx + match[0].length;
    if (clockOrFraction(prompt, startIdx) || letterAfter(prompt, endIdx)) {
      continue;
    }
    const start = Number(match[1]);
    const end = Number(match[2]);
    if (!Number.isFinite(start) || !Number.isFinite(end) || end <= start) {
      continue;
    }
    worst = Math.max(worst, end);
  }

  for (const match of prompt.matchAll(TIMELINE_CUT_RE)) {
    const minutes = Number(match[1]);
    const secs = Number(match[2]);
    const frac = match[3] ? Number(`0.${match[3]}`) : 0;
    const at = minutes * 60 + secs + frac;
    if (Number.isFinite(at)) {
      worst = Math.max(worst, at);
    }
  }

  return { overrun: worst > limit, end: worst };
}

export function timelineOverrunMessage(end: number, effectiveSeconds: number): string {
  return `提示词时间轴写到 ${formatTimelineSeconds(end)}s，超过生成时长 ${effectiveSeconds}s，请调整提示词或时长`;
}
