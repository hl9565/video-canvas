import { describe, expect, it } from "bun:test";

import {
  AUDIO_GAIN_MAX_DB,
  AUDIO_GAIN_MIN_DB,
  AV_PTS_OFFSET_MAX_MS,
  AV_PTS_OFFSET_MIN_MS,
  DEFAULT_SEGMENT_EDIT,
  FPS,
  SEGMENT_TRIM_MIN_GAP,
  buildSegmentTrimPatch,
  clampAudioGainDb,
  clampAvPtsOffsetMs,
  clampFrame,
  effectiveOutFrame,
  formatFrameLabel,
  formatFramesDuration,
  formatTimecode24,
  frameToSeekSeconds,
  framesToSeconds,
  hasNonDefaultSegmentEdit,
  isFullLengthRange,
  lastPreviewFrame,
  normalizeSegmentEdit,
  resolveTotalFrames,
  restoreFullLengthEdit,
  secondsToFrame,
  timeToFrameIndex,
  trimFrameRange,
  validateSegmentEdit,
} from "@/utils/drama/segment-trim";

describe("segment-trim 24fps math & frame helpers", () => {
  it("uses 24fps constants and computes demo_trim_19f duration accurately", () => {
    expect(FPS).toBe(24);
    expect(SEGMENT_TRIM_MIN_GAP).toBe(1);
    expect(AUDIO_GAIN_MIN_DB).toBe(-60);
    expect(AUDIO_GAIN_MAX_DB).toBe(12);

    // [24, 43) on 124f source -> 19 frames, 0.791667 s
    const trimmedFrames = effectiveOutFrame(43, 124) - 24;
    expect(trimmedFrames).toBe(19);
    expect(framesToSeconds(trimmedFrames)).toBeCloseTo(0.791667, 6);
    expect(formatFramesDuration(trimmedFrames)).toBe("0.79 s");
    expect(secondsToFrame(framesToSeconds(trimmedFrames))).toBe(19);
  });

  it("resolves totalFrames from sourceFrameCount or falls back to duration", () => {
    expect(resolveTotalFrames(124, 5.0)).toBe(124);
    expect(resolveTotalFrames(null, 5.0)).toBe(120);
    expect(resolveTotalFrames(0, 0)).toBe(0);
  });

  it("clamps frames, resolves EOF sentinel null, and computes inclusive out - 1 preview frame", () => {
    expect(clampFrame(-3, 124)).toBe(0);
    expect(clampFrame(200, 124)).toBe(124);
    expect(clampFrame(23.6, 124)).toBe(24);

    expect(effectiveOutFrame(null, 124)).toBe(124);
    expect(effectiveOutFrame(43, 124)).toBe(43);
    expect(effectiveOutFrame(150, 124)).toBe(124);

    // [24, 43) -> last visible preview frame is 42 (never 43)
    expect(lastPreviewFrame(24, 43, 124)).toBe(42);
    // Single frame [24, 25) -> last preview frame is 24
    expect(lastPreviewFrame(24, 25, 124)).toBe(24);
    // EOF sentinel [0, null) on 124f -> last preview frame is 123
    expect(lastPreviewFrame(0, null, 124)).toBe(123);
  });

  it("enforces minGap=1 frame and emits null when end reaches totalFrames", () => {
    // 尝试将入点推到出点或超过出点，保留 1 帧间隔
    expect(trimFrameRange(43, 43, 124, "start")).toEqual({
      sourceInF: 42,
      sourceOutF: 43,
    });
    // 尝试将出点推到入点之前，保留 1 帧间隔
    expect(trimFrameRange(24, 20, 124, "end")).toEqual({
      sourceInF: 24,
      sourceOutF: 25,
    });
    // 将出点推到 totalFrames 时返回 null EOF 哨兵
    expect(trimFrameRange(24, 124, 124, "end")).toEqual({
      sourceInF: 24,
      sourceOutF: null,
    });
    // 在 null EOF 下移动入点保持 sourceOutF: null
    expect(trimFrameRange(10, null, 124, "start")).toEqual({
      sourceInF: 10,
      sourceOutF: null,
    });
  });

  it("maps seek seconds and currentTime cleanly inside integer frame boundaries", () => {
    const t0 = frameToSeekSeconds(0, 124, 124 / 24);
    const t24 = frameToSeekSeconds(24, 124, 124 / 24);
    const t42 = frameToSeekSeconds(42, 124, 124 / 24);
    const t123 = frameToSeekSeconds(123, 124, 124 / 24);

    expect(t0).toBe(0);
    expect(timeToFrameIndex(t0, 124)).toBe(0);
    expect(timeToFrameIndex(t24, 124)).toBe(24);
    expect(timeToFrameIndex(t42, 124)).toBe(42);
    expect(timeToFrameIndex(t123, 124)).toBe(123);
    expect(t123).toBeLessThan(124 / 24);
  });

  it("formats 24fps timecodes and labels", () => {
    expect(formatTimecode24(0)).toBe("00:00:00");
    expect(formatTimecode24(24)).toBe("00:01:00");
    expect(formatTimecode24(42)).toBe("00:01:18");
    expect(formatTimecode24(43)).toBe("00:01:19");
    expect(formatFrameLabel(24)).toBe("24f · 00:01:00");
  });
});

describe("audio & patch helpers", () => {
  it("clamps audioGainDb (-60..+12) and avPtsOffsetMs (-10000..+10000)", () => {
    expect(clampAudioGainDb(-100)).toBe(AUDIO_GAIN_MIN_DB);
    expect(clampAudioGainDb(24)).toBe(AUDIO_GAIN_MAX_DB);
    expect(clampAudioGainDb(-3.46)).toBe(-3.5);
    expect(clampAudioGainDb(-0)).toBe(0);

    expect(clampAvPtsOffsetMs(-20000)).toBe(AV_PTS_OFFSET_MIN_MS);
    expect(clampAvPtsOffsetMs(20000)).toBe(AV_PTS_OFFSET_MAX_MS);
    expect(clampAvPtsOffsetMs(125.4)).toBe(125);
  });

  it("validates boundaries strictly and supports restoreFullLengthEdit", () => {
    expect(
      validateSegmentEdit({ ...DEFAULT_SEGMENT_EDIT, sourceInF: 24, sourceOutF: 43 }, 124),
    ).toEqual({
      valid: true,
      errors: [],
      effectiveOutF: 43,
      trimmedFrames: 19,
    });

    const invalidZeroGap = validateSegmentEdit(
      { ...DEFAULT_SEGMENT_EDIT, sourceInF: 24, sourceOutF: 24 },
      124,
    );
    expect(invalidZeroGap.valid).toBe(false);
    expect(invalidZeroGap.errors.length).toBeGreaterThan(0);

    const invalidOutOfRange = validateSegmentEdit(
      {
        ...DEFAULT_SEGMENT_EDIT,
        sourceInF: 0,
        sourceOutF: 130,
        audioGainDb: 20,
        avPtsOffsetMs: 12000,
      },
      124,
    );
    expect(invalidOutOfRange.valid).toBe(false);
    expect(invalidOutOfRange.errors.length).toBe(3);

    const edited = {
      ...DEFAULT_SEGMENT_EDIT,
      sourceInF: 24,
      sourceOutF: 43,
      muteOriginal: true,
      audioGainDb: -6,
    };
    const restored = restoreFullLengthEdit(edited);
    expect(restored.sourceInF).toBe(0);
    expect(restored.sourceOutF).toBeNull();
    expect(restored.muteOriginal).toBe(true);
    expect(restored.audioGainDb).toBe(-6);
    expect(isFullLengthRange(restored)).toBe(true);
  });

  it("normalizes edits, detects non-default edits, and builds minimal diffs preserving explicit null", () => {
    const normalized = normalizeSegmentEdit(
      { sourceInF: 24, sourceOutF: 124, audioGainDb: -99 },
      124,
    );
    expect(normalized.sourceOutF).toBeNull();
    expect(normalized.audioGainDb).toBe(-60);

    expect(hasNonDefaultSegmentEdit(DEFAULT_SEGMENT_EDIT)).toBe(false);
    expect(hasNonDefaultSegmentEdit({ ...DEFAULT_SEGMENT_EDIT, muteOriginal: true })).toBe(true);
    expect(hasNonDefaultSegmentEdit({ ...DEFAULT_SEGMENT_EDIT, nativeIsDialogue: true })).toBe(
      true,
    );
    expect(hasNonDefaultSegmentEdit({ ...DEFAULT_SEGMENT_EDIT, avPtsOffsetMs: 40 })).toBe(true);

    // 无变化返回空对象
    expect(buildSegmentTrimPatch(DEFAULT_SEGMENT_EDIT, { ...DEFAULT_SEGMENT_EDIT })).toEqual({});

    // 仅变化 muteOriginal（全长去原声）
    expect(
      buildSegmentTrimPatch(DEFAULT_SEGMENT_EDIT, {
        ...DEFAULT_SEGMENT_EDIT,
        muteOriginal: true,
      }),
    ).toEqual({ muteOriginal: true });

    // 从 [24, 43) 恢复全长时显式携带 sourceOutF: null
    const patchBackToFull = buildSegmentTrimPatch(
      { ...DEFAULT_SEGMENT_EDIT, sourceInF: 24, sourceOutF: 43 },
      { ...DEFAULT_SEGMENT_EDIT, sourceInF: 0, sourceOutF: null },
    );
    expect(patchBackToFull).toEqual({ sourceInF: 0, sourceOutF: null });
    expect("sourceOutF" in patchBackToFull).toBe(true);
    expect(patchBackToFull.sourceOutF).toBeNull();
  });
});
