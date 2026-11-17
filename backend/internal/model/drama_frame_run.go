package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"gorm.io/datatypes"

	"video-canvas/internal/pkg/idcodec" //nolint:depguard // 首帧流程沿用短剧对外 ID 编码契约
)

// AssetID 对齐素材接口（/assets/:id、AssetView、modelcfg），JSON 接受十进制正整数或其字符串并序列化为十进制数字。
type AssetID uint64

// MarshalJSON 输出十进制素材主键；0 序列化为 null。
func (a AssetID) MarshalJSON() ([]byte, error) {
	if a == 0 {
		return []byte("null"), nil
	}
	return json.Marshal(uint64(a))
}

// UnmarshalJSON 接受正整数 JSON 数字或十进制字符串，拒绝十六进制、小数、负数与非法字符。
func (a *AssetID) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*a = 0
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return errors.New("素材 id 格式错误")
		}
		s = strings.TrimSpace(s)
		if s == "" {
			*a = 0
			return nil
		}
		id, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return errors.New("素材 id 格式错误")
		}
		*a = AssetID(id)
		return nil
	}
	id, err := strconv.ParseUint(string(trimmed), 10, 64)
	if err != nil {
		return errors.New("素材 id 格式错误")
	}
	*a = AssetID(id)
	return nil
}

// DramaFrameSource 是首帧的来源。
type DramaFrameSource string

// 首帧来源只覆盖已有素材、生成与画布导入。
const (
	FrameSourceExisting DramaFrameSource = "existing"
	FrameSourceGenerate DramaFrameSource = "generate"
	FrameSourceCanvas   DramaFrameSource = "canvas"
)

// DramaFrameRunState 是首帧确认及视频绑定流程的持久状态。
type DramaFrameRunState string

// 首帧流程终态不占用片段的活跃唯一槽位。
const (
	FrameRunStateDraft        DramaFrameRunState = "draft"
	FrameRunStateFramePending DramaFrameRunState = "frame_pending"
	FrameRunStateFrameReady   DramaFrameRunState = "frame_ready"
	FrameRunStateConfirmed    DramaFrameRunState = "confirmed"
	FrameRunStateSubmitting   DramaFrameRunState = "submitting"
	FrameRunStateVideoPending DramaFrameRunState = "video_pending"
	FrameRunStateBound        DramaFrameRunState = "bound"
	FrameRunStateFailed       DramaFrameRunState = "failed"
	FrameRunStateCancelled    DramaFrameRunState = "cancelled"
)

// DramaSegmentTakeOrigin 区分控镜生成、画布导入与原生候选。
type DramaSegmentTakeOrigin string

// 候选来源在补缺导入时保持不变。
const (
	TakeOriginI2V          DramaSegmentTakeOrigin = "i2v"
	TakeOriginCanvasImport DramaSegmentTakeOrigin = "canvas_import"
	TakeOriginNative       DramaSegmentTakeOrigin = "native"
)

// DramaSegmentTakeQC 是候选的人工质检结果。
type DramaSegmentTakeQC string

// 质检初值为 unreviewed，不引入额外的处理中状态。
const (
	TakeQCUnreviewed DramaSegmentTakeQC = "unreviewed"
	TakeQCAccepted   DramaSegmentTakeQC = "accepted"
	TakeQCRejected   DramaSegmentTakeQC = "rejected"
)

// DramaSegmentFrameRun 保存单次首帧确认快照及双向任务归属；不软删，以保留幂等与恢复证据。
type DramaSegmentFrameRun struct {
	ID          uint64 `gorm:"primaryKey;index:ix_dfr_segment,priority:3"`
	UserID      uint64 `gorm:"not null;uniqueIndex:uk_dfr_user_req,priority:1;index:ix_dfr_segment,priority:1"`
	SegmentID   uint64 `gorm:"not null;index:ix_dfr_segment,priority:2;uniqueIndex:uk_dfr_segment_active,where:state IN ('draft'\\,'frame_pending'\\,'frame_ready'\\,'confirmed'\\,'submitting'\\,'video_pending')"`
	RequestKey  string `gorm:"type:varchar(96);not null;uniqueIndex:uk_dfr_user_req,priority:2"`
	RequestHash string `gorm:"type:char(64);not null"`
	// GORM 的 CHECK 直接拼接 SQL，仅索引 where 的逗号需要反斜杠转义。
	FrameSource         DramaFrameSource   `gorm:"type:varchar(16);not null;check:ck_dfr_source,frame_source IN ('existing','generate','canvas')"`
	State               DramaFrameRunState `gorm:"type:varchar(20);not null;index:ix_dfr_recover,priority:1,where:state IN ('frame_pending'\\,'submitting'\\,'video_pending');check:ck_dfr_state,state IN ('draft','frame_pending','frame_ready','confirmed','submitting','video_pending','bound','failed','cancelled')"`
	SegmentRevision     int64              `gorm:"not null;check:ck_dfr_segment_rev,segment_revision > 0"`
	Revision            int64              `gorm:"not null;default:1"`
	ConfirmedVersion    int64              `gorm:"not null;default:0"`
	VideoIdempotencyKey string             `gorm:"type:varchar(128);not null;default:''"`
	ModelKey            string             `gorm:"type:varchar(128);not null;default:''"`
	OriginCanvasID      *uint64
	FrameTaskID         *uint64 `gorm:"index:ix_dfr_frame_task,where:frame_task_id IS NOT NULL"`
	FrameAssetID        *uint64
	VideoTaskID         *uint64        `gorm:"uniqueIndex:uk_dfr_video,where:video_task_id IS NOT NULL"`
	AdoptIfEmpty        bool           `gorm:"not null;default:false"`
	ConfirmedInput      datatypes.JSON `gorm:"type:jsonb;not null;default:'{}';check:ck_dfr_confirmed_obj,jsonb_typeof(confirmed_input) = 'object'"`
	ConfirmedInputHash  *string        `gorm:"type:char(64)"`
	QuoteToken          *string        `gorm:"type:text"`
	SubmittingAt        *time.Time
	ErrorCode           string    `gorm:"type:varchar(64);not null;default:''"`
	ErrorMessage        string    `gorm:"type:varchar(512);not null;default:''"`
	CreatedAt           time.Time `gorm:"not null;default:now()"`
	UpdatedAt           time.Time `gorm:"not null;default:now();index:ix_dfr_recover,priority:2"`
}

// TableName 返回首帧控镜流程表名。
func (DramaSegmentFrameRun) TableName() string { return "drama_segment_frame_runs" }

// DramaSegmentTakeLink 保存候选归属及人工质检，不复制生成任务状态，也不软删。
type DramaSegmentTakeLink struct {
	UserID         uint64                 `gorm:"not null;index:ix_dst_task,priority:1"`
	SegmentID      uint64                 `gorm:"primaryKey;autoIncrement:false;not null"`
	TaskID         uint64                 `gorm:"primaryKey;autoIncrement:false;not null;index:ix_dst_task,priority:2"`
	SourceCanvasID uint64                 `gorm:"not null"`
	AssetID        uint64                 `gorm:"not null"`
	RunID          *uint64                `gorm:"uniqueIndex:uk_dst_run,where:run_id IS NOT NULL"`
	Origin         DramaSegmentTakeOrigin `gorm:"type:varchar(16);not null;check:ck_dst_origin,origin IN ('i2v','canvas_import','native')"`
	QC             DramaSegmentTakeQC     `gorm:"type:varchar(10);not null;default:'unreviewed';check:ck_dst_qc,qc IN ('unreviewed','accepted','rejected')"`
	CreatedAt      time.Time              `gorm:"not null;default:now()"`
}

// TableName 返回片段候选关联表名。
func (DramaSegmentTakeLink) TableName() string { return "drama_segment_take_links" }

// DramaSegmentFrameRunView 仅暴露用户可见状态，隐藏请求指纹、视频幂等键及报价令牌。
type DramaSegmentFrameRunView struct {
	ID               idcodec.ID         `json:"id"`
	SegmentID        idcodec.ID         `json:"segment_id"`
	RequestKey       string             `json:"request_key"`
	FrameSource      DramaFrameSource   `json:"frame_source"`
	State            DramaFrameRunState `json:"state"`
	SegmentRevision  int64              `json:"segment_revision"`
	Revision         int64              `json:"revision"`
	ConfirmedVersion int64              `json:"confirmed_version"`
	ModelKey         string             `json:"model_key"`
	OriginCanvasID   *idcodec.ID        `json:"origin_canvas_id,omitempty"`
	FrameTaskID      *idcodec.ID        `json:"frame_task_id,omitempty"`
	FrameAssetID     *uint64            `json:"frame_asset_id,omitempty"`
	VideoTaskID      *idcodec.ID        `json:"video_task_id,omitempty"`
	AdoptIfEmpty     bool               `json:"adopt_if_empty"`
	FrameSHA256      string             `json:"frame_sha256,omitempty"`
	CanvasTicket     string             `json:"canvas_ticket,omitempty"`
	ConfirmedInput   map[string]any     `json:"confirmed_input,omitempty"`
	SubmittingAt     *time.Time         `json:"submitting_at,omitempty"`
	ErrorCode        string             `json:"error_code"`
	ErrorMessage     string             `json:"error_message"`
	CreatedAt        time.Time          `json:"created_at"`
	UpdatedAt        time.Time          `json:"updated_at"`
}

// CreateFrameRunReq 在指定片段版本上创建可幂等重放的首帧流程。
type CreateFrameRunReq struct {
	RequestKey      string           `json:"request_key" binding:"required,max=96" label:"请求键"`
	SegmentRevision int64            `json:"segment_revision" binding:"required,min=1" label:"片段版本"`
	FrameSource     DramaFrameSource `json:"frame_source" binding:"required,oneof=existing generate canvas" label:"首帧来源"`
	FrameAssetID    *AssetID         `json:"frame_asset_id" binding:"omitempty,gt=0" label:"首帧素材"`
	OriginCanvasID  *idcodec.ID      `json:"origin_canvas_id" binding:"omitempty,gt=0" label:"来源画布"`
	ModelID         string           `json:"model_id" binding:"omitempty,max=128" label:"首帧模型"`
	Input           map[string]any   `json:"input" binding:"omitempty" label:"首帧生成参数"`
	AdoptIfEmpty    bool             `json:"adopt_if_empty" binding:"omitempty" label:"空采用时自动采用"`
}

// SetFrameRunFrameReq 带流程版本替换首帧素材。
type SetFrameRunFrameReq struct {
	Revision       int64       `json:"revision" binding:"required,min=1" label:"流程版本"`
	FrameAssetID   AssetID     `json:"frame_asset_id" binding:"required,gt=0" label:"首帧素材"`
	OriginCanvasID *idcodec.ID `json:"origin_canvas_id" binding:"omitempty,gt=0" label:"来源画布"`
	CanvasTicket   string      `json:"canvas_ticket" binding:"omitempty,max=4096" label:"画布票据"`
}

// ConfirmFrameRunReq 冻结用户确认的视频输入及对应片段版本。
type ConfirmFrameRunReq struct {
	Revision        int64          `json:"revision" binding:"required,min=1" label:"流程版本"`
	SegmentRevision *int64         `json:"segment_revision" binding:"omitempty,min=1" label:"片段版本"`
	FrameAssetID    *AssetID       `json:"frame_asset_id" binding:"omitempty,gt=0" label:"首帧素材"`
	FrameSHA256     string         `json:"frame_sha256" binding:"required,len=64,hexadecimal" label:"首帧指纹"`
	AdoptIfEmpty    *bool          `json:"adopt_if_empty" binding:"omitempty" label:"空采用时自动采用"`
	ModelID         string         `json:"model_id" binding:"omitempty,max=128" label:"视频模型"`
	Input           map[string]any `json:"input" binding:"required" label:"视频生成参数"`
	QuoteToken      string         `json:"quote_token" binding:"required,max=4096" label:"报价令牌"`
}

// SubmitFrameRunReq 只提交已确认版本，不接受新的生成输入。
type SubmitFrameRunReq struct {
	Revision         int64 `json:"revision" binding:"required,min=1" label:"流程版本"`
	ConfirmedVersion int64 `json:"confirmed_version" binding:"required,min=1" label:"确认版本"`
}

// CancelFrameRunReq 带流程版本取消进行中的流程。
type CancelFrameRunReq struct {
	Revision int64 `json:"revision" binding:"required,min=1" label:"流程版本"`
}

// ImportDramaTakesReq 将来源画布的已有任务补入片段候选，不覆盖既有来源和质检结果。
type ImportDramaTakesReq struct {
	Revision              int64        `json:"revision" binding:"required,min=1" label:"片段版本"`
	SourceCanvasID        idcodec.ID   `json:"source_canvas_id" binding:"required,gt=0" label:"来源画布"`
	TaskID                idcodec.ID   `json:"task_id" binding:"omitempty,gt=0" label:"候选任务"`
	Adopt                 bool         `json:"adopt" binding:"omitempty" label:"采用候选"`
	ResetEditOnTaskChange *bool        `json:"reset_edit_on_task_change" binding:"omitempty" label:"换镜时重置编辑"`
	TaskIDs               []idcodec.ID `json:"task_ids" binding:"omitempty,min=1,max=100,dive,gt=0" label:"候选任务"`
}

// PatchDramaTakeQCReq 更新人工质检结果，采用限制由业务层统一判断。
type PatchDramaTakeQCReq struct {
	Revision               int64              `json:"revision" binding:"required,min=1" label:"片段版本"`
	ExpectedSegmentVersion *int64             `json:"expected_segment_version" binding:"omitempty,min=0" label:"集片段版本"`
	ReplacementTaskID      *idcodec.ID        `json:"replacement_task_id" binding:"omitempty,gt=0" label:"替换候选"`
	ClearChosen            bool               `json:"clear_chosen" binding:"omitempty" label:"清空采用"`
	ResetEditOnTaskChange  *bool              `json:"reset_edit_on_task_change" binding:"omitempty" label:"换镜时重置编辑"`
	QC                     DramaSegmentTakeQC `json:"qc" binding:"required,oneof=unreviewed accepted rejected" label:"质检结果"`
}
