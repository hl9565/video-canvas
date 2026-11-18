package model

// DramaComposeReq 指定合成使用的冻结版本、清单与输出档位。
type DramaComposeReq struct {
	SegmentVersion *int64   `json:"segment_version" binding:"omitempty,min=0"`
	ManifestHash   string   `json:"manifest_hash" binding:"omitempty,len=64,hexadecimal"`
	Size           string   `json:"size" binding:"omitempty,oneof=720p 1080p"`
	Variants       []string `json:"variants" binding:"max=2,unique,dive,oneof=clean packaged"`
}

// DramaManifestClip 保存不含 URL 与临时路径的规范视频片段。
type DramaManifestClip struct {
	SegmentID           uint64  `json:"segment_id"`
	TaskID              uint64  `json:"task_id"`
	AssetID             uint64  `json:"asset_id"`
	Sha256              string  `json:"sha256"`
	Seq                 int     `json:"seq"`
	SourceInF           int32   `json:"source_in_f"`
	SourceOutF          int32   `json:"source_out_f"`
	MasterInF           int32   `json:"master_in_f"`
	MasterOutF          int32   `json:"master_out_f"`
	NormalizedFrames24  int32   `json:"normalized_frames_24"`
	HasAudio            bool    `json:"has_audio"`
	DecodedAudioSamples int64   `json:"decoded_audio_samples"`
	VStartUs            int64   `json:"v_start_us"`
	AStartUs            int64   `json:"a_start_us"`
	MuteOriginal        bool    `json:"mute_original"`
	AudioGainDB         float64 `json:"audio_gain_db"`
	NativeIsDialogue    bool    `json:"native_is_dialogue"`
	AvPtsOffsetMs       int32   `json:"av_pts_offset_ms"`
}

const (
	// CurrentDramaManifestSchemaVersion 表示规范清单结构版本（对应字段 v）。
	CurrentDramaManifestSchemaVersion = 1
	// CurrentDramaManifestRenderVersion 表示渲染语义版本，改动画幅/混音/时间基语义时递增。
	CurrentDramaManifestRenderVersion = 1
	// CurrentDramaManifestSampleRate 表示 0036 规范音频采样率（48kHz）。
	CurrentDramaManifestSampleRate = 48000
)

// DramaEpisodeManifest 是 PR1 规范化的 24fps 清单；Program 始终为空版本 1。
type DramaEpisodeManifest struct {
	Version        int                 `json:"v"`
	RenderVersion  int                 `json:"render_version"`
	EpisodeID      uint64              `json:"episode_id"`
	SegmentVersion int64               `json:"segment_version"`
	FPS            int                 `json:"fps"`
	SampleRate     int                 `json:"sample_rate"`
	TotalFrames    int32               `json:"total_frames"`
	SkippedSeqs    []int               `json:"skipped_seqs"`
	Clips          []DramaManifestClip `json:"clips"`
	Program        DramaEpisodeProgram `json:"program"`
}

// DramaManifestView 返回规范清单及其版本、内容指纹和条目诊断。
type DramaManifestView struct {
	Manifest       DramaEpisodeManifest `json:"manifest"`
	ManifestHash   string               `json:"manifest_hash"`
	SegmentVersion int64                `json:"segment_version"`
	SkippedSeqs    []int                `json:"skipped_seqs"`
	Diagnostics    []string             `json:"diagnostics"`
}

// DramaManifestPrepareView 表示冷素材探针仍在后台预热时的 202 响应视图。
type DramaManifestPrepareView struct {
	Status         string   `json:"status"`
	SegmentVersion int64    `json:"segment_version"`
	SkippedSeqs    []int    `json:"skipped_seqs"`
	Diagnostics    []string `json:"diagnostics"`
}

// DramaComposeExport 描述一个已原子发布的合成变体。
type DramaComposeExport struct {
	*DramaComposeExportReport           // v1 不带响度报告；v2 的静音测量值序列化为 null。
	Sha256                    string    `json:"sha256"`
	Variant                   string    `json:"variant"`
	Size                      string    `json:"size"`
	Width                     int       `json:"width"`
	Height                    int       `json:"height"`
	ManifestHash              string    `json:"manifest_hash"`
	Asset                     AssetView `json:"asset"`
}

// DramaPreviewRange 是母版帧的半开预览区间。
type DramaPreviewRange struct {
	InF  int32 `json:"in_f" binding:"min=0"`
	OutF int32 `json:"out_f" binding:"min=1"`
}

// DramaPreviewReq 指定预览清单版本及其局部范围。
type DramaPreviewReq struct {
	SegmentVersion int64              `json:"segment_version" binding:"min=0"`
	ManifestHash   string             `json:"manifest_hash" binding:"required,len=64,hexadecimal"`
	Variant        string             `json:"variant" binding:"required,oneof=clean packaged"`
	RangeF         *DramaPreviewRange `json:"range_f"`
}

// DramaPreviewView 是不含存储路径的 CPU 预览票据。
type DramaPreviewView struct {
	Ticket             string                   `json:"ticket"`
	Status             string                   `json:"status"`
	StreamURL          string                   `json:"stream_url,omitempty"`
	ExpiresAt          int64                    `json:"expires_at"`
	ErrorCode          int                      `json:"error_code,omitempty"`
	ErrorMessage       string                   `json:"error_message,omitempty"`
	ProgramDiagnostics []DramaPreviewDiagnostic `json:"program_diagnostics,omitempty"`
}

// DramaPreviewDiagnostic 仅携带公开诊断，分镜 ID 由服务层预先编码。
type DramaPreviewDiagnostic struct {
	CueID     string `json:"cue_id,omitempty"`
	SegmentID string `json:"segment_id,omitempty"`
	Field     string `json:"field"`
	Reason    string `json:"reason"`
	Severity  string `json:"severity"`
}
