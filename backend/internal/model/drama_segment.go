package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"gorm.io/datatypes"

	"video-canvas/internal/pkg/idcodec" //nolint:depguard // 对外 ID 与短剧项目沿用十六进制契约
)

// DramaSegment 单集内可独立生成的片段，任务和素材保留各自的生命周期。
type DramaSegment struct {
	BaseModel
	UserID        uint64 `gorm:"not null;index"`
	EpisodeID     uint64 `gorm:"not null;index:idx_drama_segment_episode_seq,priority:1"`
	Seq           int    `gorm:"not null;default:0;index:idx_drama_segment_episode_seq,priority:2"`
	Seconds       int    `gorm:"not null;default:5"`
	Prompt        string `gorm:"type:text;not null;default:''"`
	ChosenTaskID  *uint64
	ChosenCleared bool `gorm:"not null;default:false"`
	// Adaptations 按格式（h3 / generic）存原稿的转写稿，值为 DramaSegmentAdaptation；原稿就是 Seedance，不存 seedance 键
	Adaptations      datatypes.JSON `gorm:"type:jsonb;not null;default:'{}'"`
	SourceInF        int32          `gorm:"type:integer;not null;default:0;check:ck_ds_range,source_in_f >= 0 AND (source_out_f IS NULL OR source_out_f > source_in_f)"`
	SourceOutF       *int32         `gorm:"type:integer"`
	MuteOriginal     bool           `gorm:"not null;default:false"`
	AudioGainDB      float64        `gorm:"type:numeric(5,2);not null;default:0;check:ck_ds_gain,audio_gain_db BETWEEN -60 AND 12"`
	NativeIsDialogue bool           `gorm:"not null;default:false"`
	AvPtsOffsetMs    int32          `gorm:"type:integer;not null;default:0;check:ck_ds_pts,av_pts_offset_ms BETWEEN -10000 AND 10000"`
	VisualKind       string         `gorm:"size:8;not null;default:video;check:ck_ds_visual_kind,visual_kind IN ('video','still','black')"`
	StillAssetID     *uint64        `gorm:"type:bigint;check:ck_ds_visual,(visual_kind = 'video' AND still_asset_id IS NULL) OR (visual_kind = 'still' AND still_asset_id IS NOT NULL AND chosen_task_id IS NULL AND source_in_f = 0 AND source_out_f IS NOT NULL) OR (visual_kind = 'black' AND still_asset_id IS NULL AND chosen_task_id IS NULL AND source_in_f = 0 AND source_out_f IS NOT NULL)"`
	FXJSON           datatypes.JSON `gorm:"column:fx_json;type:jsonb;not null;default:'{}'::jsonb;check:ck_ds_fx_obj,jsonb_typeof(fx_json) = 'object'"`
	Revision         int64          `gorm:"not null;default:1"`
}

// DramaSegmentAdaptation 是一份转写稿；SourceHash 是转写时原稿的 SHA-256，与当前原稿不同即已过期。
// Seconds 记录转写时对应片段时长（秒）；旧数据未知时长时保持 0（omitempty 省略，不虚构历史时长）。
type DramaSegmentAdaptation struct {
	Prompt     string    `json:"prompt"`
	SourceHash string    `json:"source_hash"`
	Seconds    int       `json:"seconds,omitempty"`
	Edited     bool      `json:"edited"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// EffectiveVisualKind 将旧片段的空视觉模式视为视频。
func (r DramaSegment) EffectiveVisualKind() string {
	if r.VisualKind == "" {
		return DramaVisualVideo
	}
	return r.VisualKind
}

// TableName 返回片段表名。
func (DramaSegment) TableName() string { return "drama_segments" }

// DramaSegmentTakeView 从生成任务读取的结果，不复制保存任务状态。
type DramaSegmentTakeView struct {
	TaskID         idcodec.ID  `json:"task_id"`
	Status         string      `json:"status"`
	Asset          *AssetView  `json:"asset"`
	ErrorMessage   string      `json:"error_message"`
	CreatedAt      time.Time   `json:"created_at"`
	Origin         string      `json:"origin"`
	QC             string      `json:"qc"`
	RunID          *idcodec.ID `json:"run_id,omitempty"`
	SourceCanvasID *idcodec.ID `json:"source_canvas_id,omitempty"`
	FrameAssetID   *uint64     `json:"frame_asset_id,omitempty"`
}

// NullableInt32 区分 PATCH 请求里的“字段缺失”、“显式传 null”和“传入整数”。
type NullableInt32 struct {
	Set   bool
	Value *int32
}

// UnmarshalJSON 仅接受 null 或精确整数，拒绝字符串、浮点数、布尔值与对象。
func (n *NullableInt32) UnmarshalJSON(data []byte) error {
	if n == nil {
		return errors.New("NullableInt32: UnmarshalJSON on nil pointer")
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return errors.New("NullableInt32: empty json")
	}
	if bytes.Equal(trimmed, []byte("null")) {
		n.Set = true
		n.Value = nil
		return nil
	}
	var v int32
	if err := json.Unmarshal(trimmed, &v); err != nil {
		return err
	}
	n.Set = true
	n.Value = &v
	return nil
}

// MarshalJSON 将未设置或空指针序列化为 null，有值则序列化为整数。
func (n NullableInt32) MarshalJSON() ([]byte, error) {
	if n.Value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(*n.Value)
}

// DramaSegmentEditView 返回片段当前的逐镜精裁和原生音轨设置。
type DramaSegmentEditView struct {
	SourceInF        int32   `json:"source_in_f"`
	SourceOutF       *int32  `json:"source_out_f"`
	MuteOriginal     bool    `json:"mute_original"`
	AudioGainDB      float64 `json:"audio_gain_db"`
	NativeIsDialogue bool    `json:"native_is_dialogue"`
	AvPtsOffsetMs    int32   `json:"av_pts_offset_ms"`
}

// DramaSegmentEditPatch 部分更新片段的逐镜精裁设置，source_out_f 显式 null 表示恢复到片尾 EOF。
type DramaSegmentEditPatch struct {
	SourceInF        *int32        `json:"source_in_f"`
	SourceOutF       NullableInt32 `json:"source_out_f"`
	MuteOriginal     *bool         `json:"mute_original"`
	AudioGainDB      *float64      `json:"audio_gain_db"`
	NativeIsDialogue *bool         `json:"native_is_dialogue"`
	AvPtsOffsetMs    *int32        `json:"av_pts_offset_ms"`
}

// DramaProgramAudio 是 v1 清单的音频占位项，JSON 结构保留以维持历史哈希。
type DramaProgramAudio struct {
	ID string `json:"id"`
}

// DramaProgramOverlay 是 v1 清单的叠层占位项。
type DramaProgramOverlay struct {
	ID string `json:"id"`
}

// DramaEpisodeProgram 描述集级扩展音画包；PR1 在 edit_schema=0/1 上固定返回默认 Version=1 空包。
type DramaEpisodeProgram struct {
	Version  int                   `json:"v"`
	Audio    []DramaProgramAudio   `json:"audio"`
	Overlays []DramaProgramOverlay `json:"overlays"`
}

// DramaSegmentView 是片段工作区和 WebSocket 共用视图。
type DramaSegmentView struct {
	ID               idcodec.ID                        `json:"id"`
	EpisodeID        string                            `json:"episode_id"`
	EpisodeNo        int                               `json:"episode_no"`
	Seq              int                               `json:"seq"`
	Seconds          int                               `json:"seconds"`
	Prompt           string                            `json:"prompt"`
	Adaptations      map[string]DramaSegmentAdaptation `json:"adaptations"`
	Takes            []DramaSegmentTakeView            `json:"takes"`
	ChosenTaskID     string                            `json:"chosen_task_id"`
	ChosenCleared    bool                              `json:"chosen_cleared"`
	FrameRun         *DramaSegmentFrameRunView         `json:"frame_run,omitempty"`
	SourceFrameCount *int32                            `json:"source_frame_count"`
	Edit             DramaSegmentEditView              `json:"edit"`
	VisualKind       string                            `json:"visual_kind"`
	StillAssetID     *uint64                           `json:"still_asset_id,omitempty"`
	FX               *DramaSegmentFX                   `json:"fx,omitempty"`
	Status           string                            `json:"status"`
	Revision         int64                             `json:"revision"`
	SegmentVersion   int64                             `json:"segment_version"`
	UpdatedAt        time.Time                         `json:"updated_at"`
}

// MarshalJSON 补齐旧调用方的默认模式，并省略无特效的空对象。
func (v DramaSegmentView) MarshalJSON() ([]byte, error) {
	type plain DramaSegmentView
	if v.VisualKind == "" {
		v.VisualKind = DramaVisualVideo
	}
	if v.VisualKind == DramaVisualStill || v.VisualKind == DramaVisualBlack {
		v.SourceFrameCount = nil
	}
	if v.FX != nil && *v.FX == (DramaSegmentFX{}) {
		v.FX = nil
	}
	return json.Marshal(plain(v))
}

// CreateDramaSegmentReq 新增片段，after_seq 未传则追加到末尾。
type CreateDramaSegmentReq struct {
	Prompt   string `json:"prompt"`
	Seconds  *int   `json:"seconds" binding:"omitempty,min=1" label:"片段时长"`
	AfterSeq *int   `json:"after_seq" binding:"omitempty,min=0" label:"插入位置"`
}

// UpdateDramaSegmentReq 带版本部分更新，采用结果必须属于本段且已成功。
type UpdateDramaSegmentReq struct {
	Revision              int64       `json:"revision" binding:"required,min=1" label:"版本"`
	Prompt                *string     `json:"prompt"`
	Seconds               *int        `json:"seconds" binding:"omitempty,min=1" label:"片段时长"`
	ChosenTaskID          *idcodec.ID `json:"chosen_task_id"`
	ClearChosen           bool        `json:"clear_chosen" binding:"omitempty" label:"清空采用"`
	ExpectedTaskID        *idcodec.ID `json:"expected_task_id"`
	ResetEditOnTaskChange *bool       `json:"reset_edit_on_task_change"`
	// Edit 更新逐镜裁剪和原声设置
	Edit         *DramaSegmentEditPatch `json:"edit"`
	VisualKind   *string                `json:"visual_kind"`
	StillAssetID OptionalAssetID        `json:"still_asset_id,omitzero"`
	FX           *DramaSegmentFX        `json:"fx"`
	// Adaptation 手改某个格式的转写稿；该格式必须已经转写过
	Adaptation *DramaSegmentAdaptationEdit `json:"adaptation"`
}

// UnmarshalJSON 拒绝未知字段，并将显式 null / 空串采用值规范为清空。
func (r *UpdateDramaSegmentReq) UnmarshalJSON(data []byte) error {
	if r == nil {
		return errors.New("UpdateDramaSegmentReq: UnmarshalJSON on nil pointer")
	}
	type plain UpdateDramaSegmentReq
	var decoded *plain
	if err := DecodeDramaJSON(bytes.NewReader(data), &decoded); err != nil {
		return err
	}
	if decoded == nil {
		return errors.New("分镜更新请求必须是 JSON 对象")
	}
	if decoded.VisualKind != nil {
		if err := validateDramaVisualKind(*decoded.VisualKind); err != nil {
			return err
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for key := range fields {
		if strings.EqualFold(key, "chosen_task_id") && (decoded.ChosenTaskID == nil || *decoded.ChosenTaskID == 0) {
			decoded.ChosenTaskID, decoded.ClearChosen = nil, true
		}
	}
	*r = UpdateDramaSegmentReq(*decoded)
	return nil
}

// DramaSegmentAdaptationEdit 是对一份转写稿的手改。
type DramaSegmentAdaptationEdit struct {
	Format string `json:"format" binding:"required,oneof=h3 generic" label:"转写格式"`
	Prompt string `json:"prompt"`
}

// AdaptDramaSegmentReq 把片段原稿转写成目标格式；revision 用来确认转写的是用户眼前这一版。
type AdaptDramaSegmentReq struct {
	Format   string `json:"format" binding:"required,oneof=h3 generic" label:"转写格式"`
	Revision int64  `json:"revision" binding:"required,min=1" label:"版本"`
}

// OrderDramaSegmentsReq 必须包含该集完整且不重复的片段集合。
type OrderDramaSegmentsReq struct {
	IDs []idcodec.ID `json:"ids" binding:"required" label:"片段列表"`
}

// GenerateDramaSegmentReq 复用生成任务的输入结构，种类、画布与节点由服务指定。
type GenerateDramaSegmentReq struct {
	ModelID    string         `json:"model_id" binding:"required,max=128" label:"模型"`
	Input      map[string]any `json:"input" binding:"required" label:"生成参数"`
	QuoteToken string         `json:"quote_token" binding:"max=4096" label:"报价令牌"`
}
