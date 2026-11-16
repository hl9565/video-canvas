package model

import "time"

// AssetMediaProbe 缓存视频素材按规范 24fps 时间基探测出的精确帧数与音频采样元数据，按 (asset_id, sha256, probe_version) 唯一识别。
type AssetMediaProbe struct {
	AssetID             uint64    `gorm:"primaryKey;autoIncrement:false" json:"asset_id"`
	Sha256              string    `gorm:"type:char(64);primaryKey;autoIncrement:false" json:"sha256"`
	ProbeVersion        int16     `gorm:"type:smallint;primaryKey;autoIncrement:false" json:"probe_version"`
	FpsNum              int32     `gorm:"type:integer;not null" json:"fps_num"`
	FpsDen              int32     `gorm:"type:integer;not null" json:"fps_den"`
	NormalizedFrames24  int32     `gorm:"column:normalized_frames_24;type:integer;not null" json:"normalized_frames_24"`
	HasAudio            bool      `gorm:"not null;default:false" json:"has_audio"`
	AudioSampleRate     int32     `gorm:"type:integer;not null;default:0" json:"audio_sample_rate"`
	DecodedAudioSamples int64     `gorm:"not null;default:0" json:"decoded_audio_samples"`
	VStartUs            int64     `gorm:"not null;default:0" json:"v_start_us"`
	AStartUs            int64     `gorm:"not null;default:0" json:"a_start_us"`
	ProbedAt            time.Time `gorm:"not null;default:now()" json:"probed_at"`
}

// TableName 返回探测缓存表的物理表名。
func (AssetMediaProbe) TableName() string { return "asset_media_probes" }
