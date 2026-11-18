type StageCAssetKey = "id" | "asset_id" | "still_asset_id";

/** JSON.parse 保留视图的数字类型，同时记录超出安全整数范围的原始十进制串供写回或显示。 */
export const originalStageCAssetIds = new WeakMap<
  object,
  Partial<Record<StageCAssetKey, string>>
>();

const STAGE_C_ASSET_KEYS = new Set<string>(["id", "asset_id", "still_asset_id"]);
const DIGIT_RE = /^\d+$/;
const HAS_UNSAFE_INT_RE = /\d{16}/;

function recordStageCAssetId(target: object, key: StageCAssetKey, source: string) {
  const record = originalStageCAssetIds.get(target) ?? {};
  record[key] = source;
  originalStageCAssetIds.set(target, record);
}

function skipWs(raw: string, start: number): number {
  let i = start;
  while (i < raw.length) {
    const ch = raw.charCodeAt(i);
    if (ch === 32 || ch === 9 || ch === 10 || ch === 13) i += 1;
    else break;
  }
  return i;
}

function skipJsonString(raw: string, start: number): number {
  let i = start + 1;
  while (i < raw.length) {
    const ch = raw.charCodeAt(i);
    if (ch === 92) i += 2;
    else if (ch === 34) return i + 1;
    else i += 1;
  }
  return i;
}

function readJsonString(raw: string, start: number): [text: string, next: number] {
  let i = start + 1;
  let hasEscape = false;
  while (i < raw.length) {
    const ch = raw.charCodeAt(i);
    if (ch === 92) {
      hasEscape = true;
      i += 2;
    } else if (ch === 34) {
      const value = hasEscape
        ? (JSON.parse(raw.slice(start, i + 1)) as string)
        : raw.slice(start + 1, i);
      return [value, i + 1];
    } else {
      i += 1;
    }
  }
  return ["", i];
}

function readJsonNumber(raw: string, start: number): [token: string, next: number] {
  let i = start;
  while (i < raw.length) {
    const ch = raw.charCodeAt(i);
    if ((ch >= 48 && ch <= 57) || ch === 45 || ch === 43 || ch === 46 || ch === 101 || ch === 69) {
      i += 1;
    } else {
      break;
    }
  }
  return [raw.slice(start, i), i];
}

function scanValue(raw: string, index: number, node: unknown): number {
  let i = skipWs(raw, index);
  const ch = raw.charCodeAt(i);
  if (ch === 123) {
    i = skipWs(raw, i + 1);
    const obj =
      typeof node === "object" && node !== null ? (node as Record<string, unknown>) : null;
    if (raw.charCodeAt(i) === 125) return i + 1;
    while (i < raw.length) {
      i = skipWs(raw, i);
      const [key, afterKey] = readJsonString(raw, i);
      i = skipWs(raw, afterKey);
      if (raw.charCodeAt(i) === 58) i += 1;
      i = skipWs(raw, i);
      const child = obj ? obj[key] : undefined;
      const valChar = raw.charCodeAt(i);
      if (obj && STAGE_C_ASSET_KEYS.has(key) && valChar >= 48 && valChar <= 57) {
        const [numTok, nextIdx] = readJsonNumber(raw, i);
        if (typeof child === "number" && !Number.isSafeInteger(child) && DIGIT_RE.test(numTok)) {
          recordStageCAssetId(obj, key as StageCAssetKey, numTok);
        }
        i = nextIdx;
      } else {
        i = scanValue(raw, i, child);
      }
      i = skipWs(raw, i);
      const sep = raw.charCodeAt(i);
      if (sep === 44) {
        i += 1;
      } else {
        if (sep === 125) return i + 1;
        break;
      }
    }
    return i;
  }
  if (ch === 91) {
    i = skipWs(raw, i + 1);
    const arr = Array.isArray(node) ? node : null;
    if (raw.charCodeAt(i) === 93) return i + 1;
    let idx = 0;
    while (i < raw.length) {
      i = scanValue(raw, i, arr ? arr[idx] : undefined);
      idx += 1;
      i = skipWs(raw, i);
      const sep = raw.charCodeAt(i);
      if (sep === 44) {
        i += 1;
      } else {
        if (sep === 93) return i + 1;
        break;
      }
    }
    return i;
  }
  if (ch === 34) return skipJsonString(raw, i);
  if (ch === 116 || ch === 110) return i + 4;
  if (ch === 102) return i + 5;
  const [, next] = readJsonNumber(raw, i);
  return next;
}

/**
 * 解析 Stage C 原始 JSON，即使浏览器不支持 ES2024 JSON.parse source context（如 Safari 16.4），
 * 也能为超出 JS 安全整数范围的 id / asset_id / still_asset_id 无损保留原始十进制字符串。
 */
export function parseStageCJson(raw: string): unknown {
  const parsed = JSON.parse(raw);
  if (typeof parsed === "object" && parsed !== null && HAS_UNSAFE_INT_RE.test(raw)) {
    scanValue(raw, 0, parsed);
  }
  return parsed;
}

/** 解析 Stage C JSON 时登记超出安全整数范围的 asset_id / still_asset_id 原始十进制字符串。 */
export const stageCAssetIdReviver = function (
  this: object,
  key: string,
  value: unknown,
  context?: { source?: string },
) {
  if (
    STAGE_C_ASSET_KEYS.has(key) &&
    typeof value === "number" &&
    !Number.isSafeInteger(value) &&
    context?.source &&
    DIGIT_RE.test(context.source)
  ) {
    recordStageCAssetId(this, key as StageCAssetKey, context.source);
  }
  return value;
};
