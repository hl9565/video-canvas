import { create } from "zustand";

/** 一部短剧在片场记住的本机偏好（不写库，不同设备各自记住） */
export type StudioPrefs = {
  /** 视频模型 key；空串 = 取第一个可用的 */
  videoModel: string;
  /**
   * 旧版未分模型存储的生成参数覆盖；仅作一次性迁移源，认领给原模型后清空。
   * 新写入一律按模型 key 存在 modelOverrides 里。
   */
  overrides: Record<string, unknown>;
  /** 用户在顶栏选的生成参数，按模型 key 隔离保存，切换模型时不互相覆盖 */
  modelOverrides?: Record<string, Record<string, unknown>>;
};

const EMPTY: StudioPrefs = { videoModel: "", overrides: {} };
const EMPTY_RECORD: Record<string, unknown> = {};
const storageKey = (dramaId: string) => `drama-studio:${dramaId}`;

function parseRecord(raw: unknown): Record<string, unknown> {
  return raw && typeof raw === "object" && !Array.isArray(raw)
    ? (raw as Record<string, unknown>)
    : {};
}

function parseModelOverrides(raw: unknown): Record<string, Record<string, unknown>> | undefined {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return undefined;
  const out: Record<string, Record<string, unknown>> = {};
  for (const [modelKey, val] of Object.entries(raw as Record<string, unknown>)) {
    if (!modelKey || !val || typeof val !== "object" || Array.isArray(val)) continue;
    out[modelKey] = { ...(val as Record<string, unknown>) };
  }
  return Object.keys(out).length > 0 ? out : undefined;
}

/** 读本机存的偏好；读不到、解析失败都当没存过 */
export function readPrefs(dramaId: string): StudioPrefs {
  try {
    const raw = localStorage.getItem(storageKey(dramaId));
    if (!raw) return EMPTY;
    const parsed = JSON.parse(raw) as Partial<StudioPrefs> | null;
    const modelOverrides = parseModelOverrides(parsed?.modelOverrides);
    return {
      videoModel: typeof parsed?.videoModel === "string" ? parsed.videoModel : "",
      overrides: parseRecord(parsed?.overrides),
      ...(modelOverrides ? { modelOverrides } : {}),
    };
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
  if (!hasKeys(prefs.overrides)) return prefs;
  const owner = prefs.videoModel || fallbackOwner || "";
  if (!owner) return prefs;
  const existing = prefs.modelOverrides?.[owner];
  return {
    ...prefs,
    overrides: {},
    modelOverrides: {
      ...(prefs.modelOverrides ?? {}),
      [owner]: existing ? { ...prefs.overrides, ...existing } : { ...prefs.overrides },
    },
  };
}

/** 按当前模型取出对应的参数覆盖，切换模型时互不串味 */
export function overridesForModel(
  prefs: StudioPrefs,
  modelKey: string | undefined,
): Record<string, unknown> {
  if (modelKey && prefs.modelOverrides?.[modelKey]) {
    return prefs.modelOverrides[modelKey];
  }
  if (
    hasKeys(prefs.overrides) &&
    (!modelKey || !prefs.videoModel || prefs.videoModel === modelKey)
  ) {
    return prefs.overrides;
  }
  return EMPTY_RECORD;
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
  /** 原子更新指定模型的单个生成参数覆盖，不覆盖其他模型的已保存覆盖 */
  setModelOverride: (
    dramaId: string,
    modelKey: string,
    name: string,
    value: unknown,
    fallbackOwnerKey?: string,
  ) => void;
};

export const useStudioPrefsStore = create<StudioPrefsState>((set, get) => ({
  byDrama: {},
  get: (dramaId) => get().byDrama[dramaId] ?? readPrefs(dramaId),
  patch: (dramaId, change) => {
    set((state) => {
      const cur = state.byDrama[dramaId] ?? readPrefs(dramaId);
      const next = { ...cur, ...change };
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
  setModelOverride: (dramaId, modelKey, name, value, fallbackOwnerKey) => {
    set((state) => {
      const cur = state.byDrama[dramaId] ?? readPrefs(dramaId);
      const claimed = claimLegacyOverrides(cur, fallbackOwnerKey || modelKey);
      const bucket = { ...(claimed.modelOverrides?.[modelKey] ?? {}) };
      if (value === undefined) delete bucket[name];
      else bucket[name] = value;
      const next: StudioPrefs = {
        ...claimed,
        modelOverrides: {
          ...(claimed.modelOverrides ?? {}),
          [modelKey]: bucket,
        },
      };
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
