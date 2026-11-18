package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// DramaProgramDiagnostic 指向节目单校验失败的具体条目或字段（内部领域模型，由服务层编码输出）。
type DramaProgramDiagnostic struct {
	CueID     string
	SegmentID uint64
	Field     string
	Reason    string
	Severity  string
}

// DramaProgramError 表示节目单结构或参数校验失败，由服务层映射为业务错误。
type DramaProgramError struct {
	Msg         string
	Diagnostics []DramaProgramDiagnostic
	Cause       error
}

// Error 返回节目单校验错误文案。
func (e *DramaProgramError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return "节目单参数或结构不合法"
}

// Unwrap 返回触发校验失败的底层原因。
func (e *DramaProgramError) Unwrap() error { return e.Cause }

// DramaVisualError 表示分镜视觉模式或特效参数校验失败，由服务层映射为业务错误。
type DramaVisualError struct {
	Msg   string
	Cause error
}

// Error 返回视觉参数校验错误文案。
func (e *DramaVisualError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return "分镜视觉模式或特效参数不合法"
}

// Unwrap 返回触发视觉校验失败的底层原因。
func (e *DramaVisualError) Unwrap() error { return e.Cause }

// 分镜的三种视觉模式，旧分镜默认为视频。
const (
	DramaVisualVideo = "video"
	DramaVisualStill = "still"
	DramaVisualBlack = "black"
)

// 节目单持久版本与 Stage C 母版的容量、时间基约定。
const (
	CurrentDramaProgramVersion  = 1
	DramaProgramMaxSegments     = 200
	DramaProgramMaxAudioCues    = 256
	DramaProgramFPS             = 24
	DramaProgramSampleRate      = 48000
	DramaProgramSamplesPerFrame = DramaProgramSampleRate / DramaProgramFPS
	DramaProgramMaxFrames       = 600 * DramaProgramFPS
)

// DramaProgram 持久化集级节目单，锚点使用内部主键；公开接口使用单独的 DTO。
type DramaProgram struct {
	Version  int                    `json:"v"`
	Audio    []DramaProgramAudioCue `json:"audio"`
	Overlays []json.RawMessage      `json:"overlays"`
}

// DecodeStoredDramaProgram 读取持久化节目单，将历史空值规范为 v1 空节目单。
func DecodeStoredDramaProgram(raw json.RawMessage) (DramaProgram, error) {
	program := DramaProgram{Version: CurrentDramaProgramVersion}
	if len(bytes.TrimSpace(raw)) != 0 {
		if err := json.Unmarshal(raw, &program); err != nil {
			return DramaProgram{}, fmt.Errorf("解析已保存的节目单: %w", err)
		}
	}
	return program.withArrays(), nil
}

// HasAuthoredContent 报告节目单是否包含任何已编排的条目。
func (p DramaProgram) HasAuthoredContent() bool {
	return len(p.Audio) > 0 || len(p.Overlays) > 0
}

// MarshalJSON 规范化持久化 JSON 的空数组，用于存储及内容比较。
func (p DramaProgram) MarshalJSON() ([]byte, error) {
	type stored DramaProgram
	return json.Marshal(stored(p.withArrays()))
}

// withArrays 保留原有条目，只规范化空数组。
func (p DramaProgram) withArrays() DramaProgram {
	if p.Audio == nil {
		p.Audio = []DramaProgramAudioCue{}
	}
	if p.Overlays == nil {
		p.Overlays = []json.RawMessage{}
	}
	return p
}

// DramaProgramAudioRole 表示独立音轨在混音中的用途。
type DramaProgramAudioRole string

// 背景音乐和环境底噪可闪避，音效可随视频源帧锚定。
const (
	DramaAudioNarration DramaProgramAudioRole = "narration"
	DramaAudioSFX       DramaProgramAudioRole = "sfx"
	DramaAudioBGM       DramaProgramAudioRole = "bgm"
	DramaAudioRoomTone  DramaProgramAudioRole = "room_tone"
)

// DramaProgramAudioParams 是输入、存储和公开视图共用的音频参数。
// 绝对区间与锚定区间二选一；源音频采样区间的空结束值表示 EOF。
type DramaProgramAudioParams struct {
	ID            string                `json:"id"`
	Role          DramaProgramAudioRole `json:"role"`
	StartF        *int32                `json:"start_f,omitempty"`
	EndF          *int32                `json:"end_f,omitempty"`
	LengthF       *int32                `json:"length_f,omitempty"`
	SrcInSample   int64                 `json:"src_in_sample"`
	SrcOutSample  *int64                `json:"src_out_sample"`
	GainDB        float64               `json:"gain_db"`
	DuckDB        *float64              `json:"duck_db,omitempty"`
	AvPtsOffsetMs int32                 `json:"av_pts_offset_ms"`
	FadeInF       int32                 `json:"fade_in_f,omitempty"`
	FadeOutF      int32                 `json:"fade_out_f,omitempty"`
	Loop          bool                  `json:"loop,omitempty"`
}

// DramaProgramAudioSource 是使用内部素材和分镜主键的音频项，也是 v2 清单中的音频项。
type DramaProgramAudioSource struct {
	DramaProgramAudioParams
	AssetID uint64              `json:"asset_id"`
	Anchor  *DramaProgramAnchor `json:"anchor,omitempty"`
}

// DramaProgramAudioCue 额外持久化服务器读取锚点时的源指纹。
type DramaProgramAudioCue struct {
	DramaProgramAudioSource
	BoundSource *DramaProgramBoundSource `json:"bound_source,omitempty"`
}

// DramaProgramAnchor 将音效定位到一个分镜的源视频帧。
type DramaProgramAnchor struct {
	SegmentID uint64 `json:"segment_id"`
	SourceF   int32  `json:"source_f"`
}

// DramaProgramBoundSource 是服务端私有换源凭据，仅保存于节目单存储结构。
type DramaProgramBoundSource struct {
	TaskID  uint64 `json:"task_id"`
	AssetID uint64 `json:"asset_id"`
	Sha256  string `json:"sha256"`
}

// ValidateDramaProgramTimeline 检查单集分镜数量与母版帧数的上限。
func ValidateDramaProgramTimeline(segmentCount int, totalFrames int32) error {
	if segmentCount < 0 || segmentCount > DramaProgramMaxSegments {
		return &DramaProgramError{Msg: "每集最多支持 200 个分镜"}
	}
	if totalFrames < 0 || totalFrames > DramaProgramMaxFrames {
		return &DramaProgramError{Msg: "节目单母版最多支持 14400 帧（600 秒）"}
	}
	return nil
}

// Validate 检查无需读取分镜或素材的节目单规则。
func (p DramaProgram) Validate() error {
	if err := ValidateDramaProgramShape(p.Version, len(p.Audio), len(p.Overlays)); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(p.Audio))
	for _, cue := range p.Audio {
		if err := cue.validate(); err != nil {
			return err
		}
		if _, ok := seen[cue.ID]; ok {
			return &DramaProgramError{Msg: "音频条目 id 不能重复"}
		}
		if cue.BoundSource != nil && cue.Anchor == nil {
			return &DramaProgramError{Msg: "只有锚定的音效可以保存源绑定"}
		}
		seen[cue.ID] = struct{}{}
	}
	return nil
}

// ValidateDramaProgramShape 检查节目单版本、数量上限及 Stage C 暂不支持的叠加字段。
func ValidateDramaProgramShape(version, audioCount, overlayCount int) error {
	if overlayCount > 0 {
		return &DramaProgramError{Msg: "当前版本暂不支持字幕与角标叠加，overlays 必须为空"}
	}
	if version != CurrentDramaProgramVersion {
		return &DramaProgramError{Msg: "节目单 v 必须为 1"}
	}
	if audioCount > DramaProgramMaxAudioCues {
		return &DramaProgramError{Msg: "每集最多支持 256 个音频条目"}
	}
	return nil
}

func (a DramaProgramAudioSource) validate() error {
	if strings.TrimSpace(a.ID) == "" {
		return &DramaProgramError{Msg: "音频条目 id 不能为空"}
	}
	switch a.Role {
	case DramaAudioNarration, DramaAudioSFX, DramaAudioBGM, DramaAudioRoomTone:
	default:
		return &DramaProgramError{Msg: "无效的节目单音频角色"}
	}
	if !validDramaAssetID(a.AssetID) {
		return &DramaProgramError{Msg: "asset_id 必须为 1 到 9223372036854775807 之间的十进制素材 ID"}
	}
	if !inDramaRange(a.GainDB, -60, 12) {
		return &DramaProgramError{Msg: "gain_db 必须在 -60 到 12 之间"}
	}
	if a.DuckDB != nil && (*a.DuckDB != -8 || (a.Role != DramaAudioBGM && a.Role != DramaAudioRoomTone)) {
		return &DramaProgramError{Msg: "duck_db 只允许用于 bgm 或 room_tone，且必须为 -8"}
	}
	if a.SrcInSample < 0 || (a.SrcOutSample != nil && *a.SrcOutSample <= a.SrcInSample) {
		return &DramaProgramError{Msg: "源音频起点必须非负，结束采样点必须晚于起点或为 null"}
	}
	if a.AvPtsOffsetMs < -10000 || a.AvPtsOffsetMs > 10000 {
		return &DramaProgramError{Msg: "av_pts_offset_ms 必须在 -10000 到 10000 之间"}
	}
	if a.Loop && a.Role != DramaAudioBGM && a.Role != DramaAudioRoomTone {
		return &DramaProgramError{Msg: "loop 只允许用于 bgm 或 room_tone"}
	}
	if a.FadeInF < 0 || a.FadeOutF < 0 {
		return &DramaProgramError{Msg: "fade_in_f 与 fade_out_f 必须为非负整数帧"}
	}
	if err := a.validateTiming(); err != nil {
		return err
	}
	windowFrames := a.windowFrames()
	if int64(a.FadeInF)+int64(a.FadeOutF) > int64(windowFrames) {
		return &DramaProgramError{Msg: "fade_in_f 与 fade_out_f 之和不能超过音频条目窗口长度"}
	}
	return nil
}

func (a DramaProgramAudioSource) windowFrames() int32 {
	if a.Anchor != nil && a.LengthF != nil {
		return *a.LengthF
	}
	if a.StartF != nil && a.EndF != nil {
		return *a.EndF - *a.StartF
	}
	return 0
}

func (a DramaProgramAudioSource) validateTiming() error {
	if a.Anchor != nil {
		return a.validateAnchor()
	}
	if a.LengthF != nil || a.StartF == nil || a.EndF == nil || *a.StartF < 0 || *a.EndF <= *a.StartF || *a.EndF > DramaProgramMaxFrames {
		return &DramaProgramError{Msg: "绝对音频区间必须满足 0 ≤ start_f < end_f ≤ 14400，且不能设置 length_f"}
	}
	return nil
}

func (a DramaProgramAudioSource) validateAnchor() error {
	if a.Role != DramaAudioSFX || a.StartF != nil || a.EndF != nil {
		return &DramaProgramError{Msg: "只有 sfx 可以设置 anchor，且不能同时设置 start_f 或 end_f"}
	}
	if a.Anchor.SegmentID == 0 || a.Anchor.SourceF < 0 {
		return &DramaProgramError{Msg: "锚点必须指定分镜和非负源帧"}
	}
	if a.LengthF == nil || *a.LengthF <= 0 || *a.LengthF > DramaProgramMaxFrames {
		return &DramaProgramError{Msg: "锚定音效的 length_f 必须在 1 到 14400 之间"}
	}
	return nil
}

// DramaSegmentFX 是分镜的整数帧特效及可选静帧预设；空对象表示无特效。
type DramaSegmentFX struct {
	FadeInF        int32    `json:"fade_in_f,omitempty"`
	FadeOutF       int32    `json:"fade_out_f,omitempty"`
	FlashInF       int32    `json:"flash_in_f,omitempty"`
	FlashOutF      int32    `json:"flash_out_f,omitempty"`
	NativeFadeInF  int32    `json:"native_fade_in_f,omitempty"`
	NativeFadeOutF int32    `json:"native_fade_out_f,omitempty"`
	ZoomStart      *float64 `json:"zoom_start,omitempty"`
	ZoomEnd        *float64 `json:"zoom_end,omitempty"`
	ShakePx        *float64 `json:"shake_px,omitempty"`
	CropY          *float64 `json:"crop_y,omitempty"`
	Seed           *int32   `json:"seed,omitempty"`
}

// HasStillMotion 报告是否显式配置了任一静帧专用字段。
func (fx DramaSegmentFX) HasStillMotion() bool {
	return fx.ZoomStart != nil || fx.ZoomEnd != nil || fx.ShakePx != nil || fx.CropY != nil || fx.Seed != nil
}

// HasNativeFade 报告是否配置了原声淡入淡出。
func (fx DramaSegmentFX) HasNativeFade() bool { return fx.NativeFadeInF != 0 || fx.NativeFadeOutF != 0 }

// HasFrameEffects 报告是否存在依赖剪辑帧数的效果。
func (fx DramaSegmentFX) HasFrameEffects() bool {
	return fx.HasNativeFade() || fx.FadeInF != 0 || fx.FadeOutF != 0 || fx.FlashInF != 0 || fx.FlashOutF != 0
}

// ValidateForClip 检查视觉模式和已知剪辑时长；帧数为零时只检查模式。
func (fx DramaSegmentFX) ValidateForClip(kind string, frames int32) error {
	if err := fx.Validate(); err != nil {
		return err
	}
	if kind != DramaVisualStill && fx.HasStillMotion() {
		return &DramaVisualError{Msg: "缩放、抖动、纵向裁剪和种子仅适用于静帧"}
	}
	if kind != DramaVisualVideo && fx.HasNativeFade() {
		return &DramaVisualError{Msg: "原声淡入淡出仅适用于视频"}
	}
	for _, pair := range [][2]int32{{fx.FadeInF, fx.FadeOutF}, {fx.FlashInF, fx.FlashOutF}, {fx.NativeFadeInF, fx.NativeFadeOutF}} {
		if frames > 0 && int64(pair[0])+int64(pair[1]) > int64(frames) {
			return &DramaVisualError{Msg: "淡入、淡出或闪帧超过片段长度"}
		}
	}
	return nil
}

// DecodeDramaSegmentFX 将历史空值和空对象统一为零值特效。
func DecodeDramaSegmentFX(raw []byte) (DramaSegmentFX, error) {
	var fx DramaSegmentFX
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return fx, nil
	}
	err := json.Unmarshal(raw, &fx)
	return fx, err
}

// CanonicalDramaSegmentFXJSON 统一空对象和数字表示，保留不合法的原始数据以便诊断。
func CanonicalDramaSegmentFXJSON(raw []byte) []byte {
	fx, err := DecodeDramaSegmentFX(raw)
	if err != nil {
		return raw
	}
	// JSONB 把 -0 规整为 0；版本比较也必须采用相同表示。
	for _, field := range []**float64{&fx.ShakePx, &fx.CropY} {
		if *field != nil && **field == 0 {
			*field = new(float64)
		}
	}
	out, err := json.Marshal(fx)
	if err != nil {
		return raw
	}
	return out
}

// Validate 检查特效参数的静态范围。依赖分镜时长和模式的校验由调用方处理。
func (fx DramaSegmentFX) Validate() error {
	for _, frames := range [...]int32{fx.FadeInF, fx.FadeOutF, fx.FlashInF, fx.FlashOutF, fx.NativeFadeInF, fx.NativeFadeOutF} {
		if frames < 0 {
			return &DramaVisualError{Msg: "淡入、淡出及闪帧时长必须是非负整数帧"}
		}
	}
	for _, f := range [...]struct {
		value    *float64
		min, max float64
		msg      string
	}{
		{fx.ZoomStart, 1, 1.25, "zoom_start 必须在 1 到 1.25 之间"},
		{fx.ZoomEnd, 1, 1.25, "zoom_end 必须在 1 到 1.25 之间"},
		{fx.ShakePx, 0, 6, "shake_px 必须在 0 到 6 之间"},
		{fx.CropY, 0, 1, "crop_y 必须在 0 到 1 之间"},
	} {
		if f.value != nil && !inDramaRange(*f.value, f.min, f.max) {
			return &DramaVisualError{Msg: f.msg}
		}
	}
	if fx.Seed != nil && *fx.Seed < 0 {
		return &DramaVisualError{Msg: "seed 必须在 0 到 2147483647 之间"}
	}
	return nil
}

func validateDramaVisualKind(kind string) error {
	switch kind {
	case DramaVisualVideo, DramaVisualStill, DramaVisualBlack:
		return nil
	default:
		return &DramaVisualError{Msg: "visual_kind 必须为 video、still 或 black"}
	}
}

func inDramaRange(value, minValue, maxValue float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= minValue && value <= maxValue
}

func validDramaAssetID(id uint64) bool { return id > 0 && id <= math.MaxInt64 }
