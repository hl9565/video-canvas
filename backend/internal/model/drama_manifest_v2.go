package model

const (
	// CurrentDramaManifestV2SchemaVersion 是 Stage C 规范清单的结构版本。
	CurrentDramaManifestV2SchemaVersion = 2
	// CurrentDramaManifestV2RenderVersion 是 Stage C 规范清单的渲染版本。
	CurrentDramaManifestV2RenderVersion = 2
)

// DramaEpisodeManifestV2 是用于冻结与哈希的 Stage C 清单，使用内部主键。
type DramaEpisodeManifestV2 struct {
	Version        int                    `json:"v"`
	RenderVersion  int                    `json:"render_version"`
	EpisodeID      uint64                 `json:"episode_id"`
	SegmentVersion int64                  `json:"segment_version"`
	FPS            int                    `json:"fps"`
	SampleRate     int                    `json:"sample_rate"`
	TotalFrames    int32                  `json:"total_frames"`
	SkippedSeqs    []int                  `json:"skipped_seqs"`
	Clips          []DramaManifestV2Clip  `json:"clips"`
	Program        DramaManifestV2Program `json:"program"`
}

// DramaManifestV2Clip 是冻结后的画面片段。静帧、黑场没有生成任务。
type DramaManifestV2Clip struct {
	SegmentID uint64 `json:"segment_id"`
	TaskID    uint64 `json:"task_id,omitempty"`
	DramaManifestV2ClipParams
}

// DramaManifestV2ClipParams 是清单与公开视图共用的渲染参数，素材 ID 为十进制数字。
type DramaManifestV2ClipParams struct {
	VisualKind          string          `json:"visual_kind"`
	AssetID             uint64          `json:"asset_id,omitempty"`
	StillAssetID        *uint64         `json:"still_asset_id,omitempty"`
	FX                  *DramaSegmentFX `json:"fx,omitempty"`
	Sha256              string          `json:"sha256,omitempty"`
	Seq                 int             `json:"seq"`
	SourceInF           int32           `json:"source_in_f"`
	SourceOutF          int32           `json:"source_out_f"`
	MasterInF           int32           `json:"master_in_f"`
	MasterOutF          int32           `json:"master_out_f"`
	NormalizedFrames24  int32           `json:"normalized_frames_24"`
	HasAudio            bool            `json:"has_audio"`
	DecodedAudioSamples int64           `json:"decoded_audio_samples"`
	VStartUs            int64           `json:"v_start_us"`
	AStartUs            int64           `json:"a_start_us"`
	MuteOriginal        bool            `json:"mute_original"`
	AudioGainDB         float64         `json:"audio_gain_db"`
	NativeIsDialogue    bool            `json:"native_is_dialogue"`
	AvPtsOffsetMs       int32           `json:"av_pts_offset_ms"`
}

// DramaManifestV2Program 冻结渲染所需的音频条目。私有绑定只留在节目单存储层。
type DramaManifestV2Program struct {
	Version  int                       `json:"v"`
	Audio    []DramaManifestV2AudioCue `json:"audio"`
	Overlays [0]struct{}               `json:"overlays"`
}

// DramaManifestV2AudioCue 将源指纹和规范化采样数冻结在清单内；公开视图只投影节目单字段。
type DramaManifestV2AudioCue struct {
	DramaProgramAudioSource
	Sha256              string `json:"sha256"`
	DecodedAudioSamples int64  `json:"decoded_audio_samples"`
	AStartUs            int64  `json:"a_start_us"`
}

// ManifestProgram 提取 v2 清单需要的节目单参数。
func (p DramaProgram) ManifestProgram() DramaManifestV2Program {
	program := DramaManifestV2Program{Version: p.Version, Audio: make([]DramaManifestV2AudioCue, 0, len(p.Audio))}
	for _, cue := range p.Audio {
		program.Audio = append(program.Audio, DramaManifestV2AudioCue{DramaProgramAudioSource: cue.DramaProgramAudioSource})
	}
	return program
}

// DramaComposeExportReport 记录混音母带的指纹与响度验收；无测量值时保留 null。
type DramaComposeExportReport struct {
	AudioSHA256    string   `json:"audio_sha256"`
	LUFS           *float64 `json:"lufs"`
	TruePeak       *float64 `json:"true_peak"`
	LoudnessStatus string   `json:"loudness_status"`
}
