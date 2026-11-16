import type {
  DramaComposeExport,
  DramaComposeInput,
  DramaComposeSize,
  DramaComposeVariant,
  DramaJob,
  DramaJobInput,
  DramaManifestView,
  EpisodeSummary,
  Segment,
} from "@/api/drama/type";

import { isActiveJob } from "./jobs";
import { hasVideo } from "./segments";

/** 后端 PR1 支持的合成尺寸与变体（sample 只是前端显示别名，不发给接口） */
export const COMPOSE_SIZES = ["720p", "1080p"] as const satisfies readonly DramaComposeSize[];
export const COMPOSE_VARIANTS = [
  "clean",
  "packaged",
] as const satisfies readonly DramaComposeVariant[];
export const DEFAULT_COMPOSE_SIZE: DramaComposeSize = "720p";
export const DEFAULT_COMPOSE_VARIANTS: readonly DramaComposeVariant[] = ["clean"];

export const COMPOSE_VARIANT_LABEL: Record<DramaComposeVariant, string> = {
  clean: "纯净版",
  packaged: "成片包装版",
};

/** 后端 `dramaAspectBase` 支持的项目比例 -> 720p 基准分辨率 */
export const COMPOSE_ASPECT_BASE = {
  "16:9": [1280, 720],
  "9:16": [720, 1280],
  "4:3": [960, 720],
  "3:4": [720, 960],
  "1:1": [720, 720],
  "21:9": [1680, 720],
} as const satisfies Record<string, readonly [number, number]>;

export type SupportedComposeRatio = keyof typeof COMPOSE_ASPECT_BASE;

/** 计算指定比例与输出档位（720p / 1080p）的目标像素分辨率；未知比例按 16:9 */
export function composeDimensions(
  ratio: string,
  size: DramaComposeSize,
): readonly [number, number] {
  const base = COMPOSE_ASPECT_BASE[ratio as SupportedComposeRatio] ?? COMPOSE_ASPECT_BASE["16:9"];
  if (size === "1080p") {
    return [(base[0] * 3) / 2, (base[1] * 3) / 2] as const;
  }
  return base;
}

/** 项目比例受后端冻结清单支持且该集已进入精裁协议（editSchema === 1）时走新版冻结清单导出 */
export const canUseManifestCompose = (
  ratio: string,
  episode?: Pick<EpisodeSummary, "editSchema"> | null,
): boolean => ratio in COMPOSE_ASPECT_BASE && (episode?.editSchema ?? 0) === 1;

/** 导出项唯一键：PR1 阶段 clean/packaged 可能内容同哈希，不能用 sha256 当身份 */
export const composeExportKey = (
  item: Pick<DramaComposeExport, "variant" | "size" | "asset">,
): string => `${item.variant}:${item.size}:${item.asset.id}`;

/** 归一化变体列表：去重、保序、限最多 2 项；空选回退 clean */
export function normalizeComposeVariants(
  variants?: readonly DramaComposeVariant[] | null,
): DramaComposeVariant[] {
  const seen = new Set<DramaComposeVariant>();
  for (const item of variants ?? []) {
    if (item === "clean" || item === "packaged") seen.add(item);
    if (seen.size >= 2) break;
  }
  return seen.size > 0 ? [...seen] : ["clean"];
}

/** 生成符合后端 96 字符上限且不撞 approval: 前缀的 compose 请求幂等键 */
export const createComposeRequestKey = (): string => `cmp-${crypto.randomUUID()}`;

export interface BuildComposeJobInputArgs {
  episodeNo: number;
  manifest?: Pick<DramaManifestView, "segmentVersion" | "manifestHash"> | null;
  segmentVersion?: number;
  manifestHash?: string;
  size?: DramaComposeSize;
  variants?: readonly DramaComposeVariant[];
  composeRequestKey?: string;
}

/** 构造提交合成任务的输入：仅当带有新版清单 / 档位 / 变体字段时附带 compose 对象 */
export function buildComposeJobInput(args: BuildComposeJobInputArgs): DramaJobInput {
  const segmentVersion = args.manifest?.segmentVersion ?? args.segmentVersion;
  const manifestHash = args.manifest?.manifestHash ?? args.manifestHash;
  const variants = args.variants ? normalizeComposeVariants(args.variants) : undefined;
  const compose: DramaComposeInput | undefined =
    segmentVersion !== undefined || manifestHash || args.size || variants
      ? {
          ...(segmentVersion !== undefined ? { segmentVersion } : {}),
          ...(manifestHash ? { manifestHash } : {}),
          ...(args.size ? { size: args.size } : {}),
          ...(variants ? { variants } : {}),
        }
      : undefined;
  return {
    step: "compose",
    episodes: [args.episodeNo],
    ...(compose ? { compose } : {}),
    ...(args.composeRequestKey ? { composeRequestKey: args.composeRequestKey } : {}),
  };
}

/** 从单条 compose 任务读取各变体导出资产；未成功或非 compose 任务返回空数组 */
export function composeExportsOf(job: DramaJob | null | undefined): DramaComposeExport[] {
  if (!job || job.step !== "compose" || job.status !== "succeeded") return [];
  return job.result?.exports ?? [];
}

/**
 * 读取某一集当前成片对应的 exports 列表：
 * - 若提供了 `finalComposeJobId`（字符串），精确匹配该任务；找不到或未成功则不回退到其他历史任务，返回 `[]`。
 * - 若显式为 `null`（新后端表明当前没有绑定的定稿合成任务），返回 `[]`。
 * - 仅当 `finalComposeJobId === undefined`（旧版后端未下发该字段）时，回退取最近一条成功 compose 任务。
 */
export function latestComposeExports(
  jobs: readonly DramaJob[],
  no: number,
  finalComposeJobId?: string | null,
): DramaComposeExport[] {
  if (finalComposeJobId === null) {
    return [];
  }
  if (typeof finalComposeJobId === "string") {
    if (!finalComposeJobId) return [];
    const target = jobs.find(
      (job) =>
        job.id === finalComposeJobId &&
        job.step === "compose" &&
        job.status === "succeeded" &&
        job.episodes.includes(no),
    );
    return composeExportsOf(target);
  }
  const succeeded = jobs
    .filter(
      (job) => job.step === "compose" && job.status === "succeeded" && job.episodes.includes(no),
    )
    .sort((a, b) => (b.finishedAt ?? b.createdAt).localeCompare(a.finishedAt ?? a.createdAt));
  return composeExportsOf(succeeded[0]);
}

/** 「合成整集」按钮能不能点；不能点时 reason 就是按钮的说明 */
export type ComposeCheck = { enabled: boolean; reason: string | null };

/**
 * 合成整集的可用性（契约 §14）：至少一个片段有采用视频才可点（否则后端 67010）；
 * 同一集有进行中的分镜 / 合成任务时会撞上后端 67004，有就禁用并写明是哪个；不同集可以并行。
 */
export function composeCheck(
  segments: readonly Segment[],
  busyReason: string | null,
): ComposeCheck {
  if (!segments.some(hasVideo)) {
    return { enabled: false, reason: "至少要有一个片段采用了视频才能合成" };
  }
  if (busyReason) return { enabled: false, reason: busyReason };
  return { enabled: true, reason: null };
}

/** 分集卡片上的判定：没加载片段，只看集摘要里「有采用视频的片段数」，规则同 composeCheck */
export function composeCheckEpisode(
  episode: Pick<EpisodeSummary, "doneSegmentCount">,
  busyReason: string | null,
): ComposeCheck {
  if (episode.doneSegmentCount <= 0) {
    return { enabled: false, reason: "至少要有一个片段采用了视频才能合成" };
  }
  if (busyReason) return { enabled: false, reason: busyReason };
  return { enabled: true, reason: null };
}

/** 没有采用视频的片段（合成时会被跳过）的 seq，按顺序；seq 从 0 起 */
export const missingVideoSeqs = (segments: readonly Segment[]): number[] =>
  [...segments]
    .sort((a, b) => a.seq - b.seq)
    .filter((segment) => !hasVideo(segment))
    .map((segment) => segment.seq);

/** 把 seq 列表写成界面上的序号：`[1, 3]` → 「02、04」 */
export const skippedText = (seqs: readonly number[]): string =>
  seqs.map((seq) => String(seq + 1).padStart(2, "0")).join("、");

/** 一集成片的状态：none 没有；fresh 最新；stale 成片之后片段改过（提示可重新合成） */
export type FinalState = "none" | "fresh" | "stale";

export function finalState(episode: Pick<EpisodeSummary, "final" | "finalStale">): FinalState {
  if (!episode.final) return "none";
  return episode.finalStale ? "stale" : "fresh";
}

/** 这一集进行中（排队 / 运行）的合成任务，没有为 null */
export const activeComposeJob = (jobs: readonly DramaJob[], no: number): DramaJob | null =>
  jobs.find((job) => job.step === "compose" && isActiveJob(job) && job.episodes.includes(no)) ??
  null;

/**
 * 合成成功后要不要提示：跳过了没有采用视频的片段才提示，写明是哪几个；
 * 不是合成任务、没成功、没有跳过都返回 null。
 */
export function composeNotice(job: DramaJob): string | null {
  if (job.step !== "compose" || job.status !== "succeeded") return null;
  const skipped = job.result?.skippedSeqs ?? [];
  if (skipped.length === 0) return null;
  const no = job.episodes[0];
  return `${no ? `第 ${no} 集` : "整集"}已合成，跳过了没有采用视频的片段 ${skippedText(skipped)}`;
}
