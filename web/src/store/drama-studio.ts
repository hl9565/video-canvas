import { create } from "zustand";

/** 不跨片段继承的参数键：绝不写入共享 overrides / modelOverrides，仅按 (segmentId, modelKey) 草稿保存 */
export const STICKY_DRAFT_KEYS = ["resolution", "count"] as const;
const STICKY_KEY_SET = new Set<string>(STICKY_DRAFT_KEYS);

export const isStickyDraftKey = (name: string): boolean => STICKY_KEY_SET.has(name);

const hasStickyKey = (record: Record<string, unknown> | undefined | null): boolean =>
  !!record && Object.keys(record).some((key) => STICKY_KEY_SET.has(key));

/** 剔除 resolution / count，返回只包含可共享参数的浅拷贝 */
export function stripStickyDraftKeys(
  raw: Record<string, unknown> | undefined | null,
): Record<string, unknown> {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return {};
  const out: Record<string, unknown> = {};
  for (const [key, val] of Object.entries(raw)) {
    if (STICKY_KEY_SET.has(key)) continue;
    out[key] = val;
  }
  return out;
}

const cleanSharedRecord = (record: Record<string, unknown>): Record<string, unknown> =>
  hasStickyKey(record) ? stripStickyDraftKeys(record) : record;

/** 一部短剧在片场记住的本机偏好（不写库，不同设备各自记住） */
export type StudioPrefs = {
  /** 视频模型 key；空串 = 取第一个可用的 */
  videoModel: string;
  /**
   * 旧版未分模型存储的生成参数覆盖；仅作一次性迁移源，认领给原模型后清空。
   * 新写入一律按模型 key 存在 modelOverrides 里（不含 resolution / count）。
   */
  overrides: Record<string, unknown>;
  /** 用户在顶栏选的共享生成参数（不含 resolution / count），按模型 key 隔离保存 */
  modelOverrides?: Record<string, Record<string, unknown>>;
  /** 按 `(segmentId, modelKey)` 保存的单镜草稿参数（resolution / count 等，不跨段粘滞） */
  segmentOverrides?: Record<string, Record<string, Record<string, unknown>>>;
};

const EMPTY: StudioPrefs = { videoModel: "", overrides: {} };
const EMPTY_RECORD: Record<string, unknown> = {};
const storageKey = (dramaId: string) => `drama-studio:${dramaId}`;

function parseRecord(raw: unknown): Record<string, unknown> {
  return raw && typeof raw === "object" && !Array.isArray(raw)
    ? (raw as Record<string, unknown>)
    : {};
}

function parseModelOverrides(raw: unknown): {
  value?: Record<string, Record<string, unknown>>;
  stripped: boolean;
} {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) {
    return { value: undefined, stripped: false };
  }
  const out: Record<string, Record<string, unknown>> = {};
  let stripped = false;
  for (const [modelKey, val] of Object.entries(raw as Record<string, unknown>)) {
    if (!modelKey || !val || typeof val !== "object" || Array.isArray(val)) continue;
    const record = val as Record<string, unknown>;
    if (hasStickyKey(record)) stripped = true;
    const clean = stripStickyDraftKeys(record);
    if (Object.keys(clean).length > 0) out[modelKey] = clean;
  }
  return { value: Object.keys(out).length > 0 ? out : undefined, stripped };
}

function parseSegmentOverrides(
  raw: unknown,
): Record<string, Record<string, Record<string, unknown>>> | undefined {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return undefined;
  const out: Record<string, Record<string, Record<string, unknown>>> = {};
  for (const [segmentId, perModel] of Object.entries(raw as Record<string, unknown>)) {
    if (!segmentId || !perModel || typeof perModel !== "object" || Array.isArray(perModel)) {
      continue;
    }
    const modelsOut: Record<string, Record<string, unknown>> = {};
    for (const [modelKey, params] of Object.entries(perModel as Record<string, unknown>)) {
      if (!modelKey || !params || typeof params !== "object" || Array.isArray(params)) continue;
      const copy = { ...(params as Record<string, unknown>) };
      if (Object.keys(copy).length > 0) modelsOut[modelKey] = copy;
    }
    if (Object.keys(modelsOut).length > 0) out[segmentId] = modelsOut;
  }
  return Object.keys(out).length > 0 ? out : undefined;
}

/** 读本机存的偏好；读不到、解析失败都当没存过，并自动剔除旧缓存中的粘滞参数 */
export function readPrefs(dramaId: string): StudioPrefs {
  try {
    const raw = localStorage.getItem(storageKey(dramaId));
    if (!raw) return EMPTY;
    const parsed = JSON.parse(raw) as Partial<StudioPrefs> | null;
    const rawOverrides = parseRecord(parsed?.overrides);
    const overridesHadSticky = hasStickyKey(rawOverrides);
    const overrides = stripStickyDraftKeys(rawOverrides);
    const modelParsed = parseModelOverrides(parsed?.modelOverrides);
    const segmentOverrides = parseSegmentOverrides(parsed?.segmentOverrides);
    const next: StudioPrefs = {
      videoModel: typeof parsed?.videoModel === "string" ? parsed.videoModel : "",
      overrides,
      ...(modelParsed.value ? { modelOverrides: modelParsed.value } : {}),
      ...(segmentOverrides ? { segmentOverrides } : {}),
    };
    if (overridesHadSticky || modelParsed.stripped) {
      writePrefs(dramaId, next);
    }
    return next;
  } catch {
    return EMPTY;
  }
}

function writePrefs(dramaId: string, prefs: StudioPrefs) {
  try {
    localStorage.setItem(storageKey(dramaId), JSON.stringify(prefs));
  } catch {
    // 存不下（隐私模式、配额）就只在本次会话里生效
  }
}

function hasKeys(record: Record<string, unknown> | undefined): boolean {
  return !!record && Object.keys(record).length > 0;
}

function claimLegacyOverrides(prefs: StudioPrefs, fallbackOwner?: string): StudioPrefs {
  const cleanOverrides = stripStickyDraftKeys(prefs.overrides);
  const hadStickyOnly = hasKeys(prefs.overrides) && !hasKeys(cleanOverrides);
  if (!hasKeys(cleanOverrides)) {
    return hadStickyOnly ? { ...prefs, overrides: {} } : prefs;
  }
  const owner = prefs.videoModel || fallbackOwner || "";
  if (!owner) {
    return hasStickyKey(prefs.overrides) ? { ...prefs, overrides: cleanOverrides } : prefs;
  }
  const existing = prefs.modelOverrides?.[owner];
  const merged = existing
    ? { ...cleanOverrides, ...stripStickyDraftKeys(existing) }
    : cleanOverrides;
  return {
    ...prefs,
    overrides: {},
    modelOverrides: {
      ...(prefs.modelOverrides ?? {}),
      [owner]: merged,
    },
  };
}

/** 按当前模型（及可选当前片段）取出对应参数覆盖；共享层绝不透传 resolution / count */
export function overridesForModel(
  prefs: StudioPrefs,
  modelKey: string | undefined,
  segmentId?: string,
): Record<string, unknown> {
  const sharedRaw =
    modelKey && prefs.modelOverrides?.[modelKey]
      ? prefs.modelOverrides[modelKey]
      : hasKeys(prefs.overrides) &&
          (!modelKey || !prefs.videoModel || prefs.videoModel === modelKey)
        ? prefs.overrides
        : undefined;
  const shared = sharedRaw ? cleanSharedRecord(sharedRaw) : undefined;
  const seg = segmentId && modelKey ? prefs.segmentOverrides?.[segmentId]?.[modelKey] : undefined;

  if (hasKeys(shared) && hasKeys(seg)) return { ...shared, ...seg };
  if (hasKeys(seg)) return seg!;
  if (hasKeys(shared)) return shared!;
  return EMPTY_RECORD;
}

function updateSegmentBucket(
  prefs: StudioPrefs,
  segmentId: string,
  modelKey: string,
  name: string,
  value: unknown,
): StudioPrefs {
  const prevDramaSegs = prefs.segmentOverrides ?? {};
  const prevSegModels = prevDramaSegs[segmentId] ?? {};
  const bucket = { ...(prevSegModels[modelKey] ?? {}) };
  if (value === undefined) delete bucket[name];
  else bucket[name] = value;

  const nextSegModels = { ...prevSegModels };
  if (Object.keys(bucket).length > 0) nextSegModels[modelKey] = bucket;
  else delete nextSegModels[modelKey];

  const nextDramaSegs = { ...prevDramaSegs };
  if (Object.keys(nextSegModels).length > 0) nextDramaSegs[segmentId] = nextSegModels;
  else delete nextDramaSegs[segmentId];

  const { segmentOverrides: _dropped, ...rest } = prefs;
  return Object.keys(nextDramaSegs).length > 0
    ? { ...rest, segmentOverrides: nextDramaSegs }
    : rest;
}

type StudioPrefsState = {
  byDrama: Record<string, StudioPrefs>;
  /** 读（首次从本机存储载入） */
  get: (dramaId: string) => StudioPrefs;
  patch: (dramaId: string, change: Partial<StudioPrefs>) => void;
  /** 将旧版扁平 overrides 一次性认领给原模型后清空 */
  migrateLegacyOverrides: (dramaId: string, ownerModelKey: string) => void;
  /** 切换视频模型：先把未认领的旧版 overrides 归档到切换前模型，再切换 */
  setVideoModel: (dramaId: string, nextModelKey: string, previousModelKey?: string) => void;
  /** 更新指定 `(segmentId, modelKey)` 的单镜草稿覆盖（resolution / count 等） */
  setSegmentOverride: (
    dramaId: string,
    segmentId: string,
    modelKey: string,
    name: string,
    value: unknown,
  ) => void;
  /**
   * 原子更新参数覆盖：resolution / count 只写进 `(segmentId, modelKey)` 草稿，
   * 绝不写入共享 `modelOverrides`；其他共享参数按 `modelKey` 保存。
   */
  setModelOverride: (
    dramaId: string,
    modelKey: string,
    name: string,
    value: unknown,
    fallbackOwnerKey?: string,
    segmentId?: string,
  ) => void;
};

export const useStudioPrefsStore = create<StudioPrefsState>((set, get) => ({
  byDrama: {},
  get: (dramaId) => get().byDrama[dramaId] ?? readPrefs(dramaId),
  patch: (dramaId, change) => {
    set((state) => {
      const cur = state.byDrama[dramaId] ?? readPrefs(dramaId);
      const next: StudioPrefs = { ...cur, ...change };
      if (change.overrides !== undefined) {
        next.overrides = stripStickyDraftKeys(change.overrides);
      }
      if ("modelOverrides" in change) {
        const cleaned = parseModelOverrides(change.modelOverrides).value;
        if (cleaned) next.modelOverrides = cleaned;
        else delete next.modelOverrides;
      }
      if ("segmentOverrides" in change) {
        const segs = parseSegmentOverrides(change.segmentOverrides);
        if (segs) next.segmentOverrides = segs;
        else delete next.segmentOverrides;
      }
      writePrefs(dramaId, next);
      return { byDrama: { ...state.byDrama, [dramaId]: next } };
    });
  },
  migrateLegacyOverrides: (dramaId, ownerModelKey) => {
    set((state) => {
      const cur = state.byDrama[dramaId] ?? readPrefs(dramaId);
      const next = claimLegacyOverrides(cur, ownerModelKey);
      if (next === cur) return state;
      writePrefs(dramaId, next);
      return { byDrama: { ...state.byDrama, [dramaId]: next } };
    });
  },
  setVideoModel: (dramaId, nextModelKey, previousModelKey) => {
    set((state) => {
      const cur = state.byDrama[dramaId] ?? readPrefs(dramaId);
      const claimed = claimLegacyOverrides(cur, previousModelKey);
      const next: StudioPrefs = { ...claimed, videoModel: nextModelKey };
      writePrefs(dramaId, next);
      return { byDrama: { ...state.byDrama, [dramaId]: next } };
    });
  },
  setSegmentOverride: (dramaId, segmentId, modelKey, name, value) => {
    if (!segmentId || !modelKey) return;
    set((state) => {
      const cur = state.byDrama[dramaId] ?? readPrefs(dramaId);
      const next = updateSegmentBucket(cur, segmentId, modelKey, name, value);
      writePrefs(dramaId, next);
      return { byDrama: { ...state.byDrama, [dramaId]: next } };
    });
  },
  setModelOverride: (dramaId, modelKey, name, value, fallbackOwnerKey, segmentId) => {
    set((state) => {
      const cur = state.byDrama[dramaId] ?? readPrefs(dramaId);
      const claimed = claimLegacyOverrides(cur, fallbackOwnerKey || modelKey);
      if (isStickyDraftKey(name)) {
        if (!segmentId) {
          if (claimed === cur) return state;
          writePrefs(dramaId, claimed);
          return { byDrama: { ...state.byDrama, [dramaId]: claimed } };
        }
        const next = updateSegmentBucket(claimed, segmentId, modelKey, name, value);
        writePrefs(dramaId, next);
        return { byDrama: { ...state.byDrama, [dramaId]: next } };
      }
      const bucket = { ...stripStickyDraftKeys(claimed.modelOverrides?.[modelKey]) };
      if (value === undefined) delete bucket[name];
      else bucket[name] = value;
      const nextModels = { ...(claimed.modelOverrides ?? {}) };
      if (Object.keys(bucket).length > 0) nextModels[modelKey] = bucket;
      else delete nextModels[modelKey];
      const { modelOverrides: _dropped, ...rest } = claimed;
      const next: StudioPrefs =
        Object.keys(nextModels).length > 0 ? { ...rest, modelOverrides: nextModels } : rest;
      writePrefs(dramaId, next);
      return { byDrama: { ...state.byDrama, [dramaId]: next } };
    });
  },
}));

/** 订阅一部短剧的偏好；没存过时是稳定的空对象 */
export function useStudioPrefs(dramaId: string): StudioPrefs {
  return useStudioPrefsStore((state) => state.byDrama[dramaId]) ?? readCached(dramaId);
}

const cache = new Map<string, StudioPrefs>();
function readCached(dramaId: string): StudioPrefs {
  let value = cache.get(dramaId);
  if (!value) {
    value = readPrefs(dramaId);
    cache.set(dramaId, value);
  }
  return value;
}
