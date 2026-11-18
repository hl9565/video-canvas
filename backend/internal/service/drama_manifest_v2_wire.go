package service

import (
	"video-canvas/internal/model"
	"video-canvas/internal/pkg/idcodec"
)

// DramaManifestV2View 是公开清单，使用编码的集、分镜和任务 ID。
type DramaManifestV2View struct {
	Version        int                       `json:"v"`
	RenderVersion  int                       `json:"render_version"`
	EpisodeID      idcodec.ID                `json:"episode_id"`
	SegmentVersion int64                     `json:"segment_version"`
	FPS            int                       `json:"fps"`
	SampleRate     int                       `json:"sample_rate"`
	TotalFrames    int32                     `json:"total_frames"`
	SkippedSeqs    []int                     `json:"skipped_seqs"`
	Clips          []DramaManifestV2ClipView `json:"clips"`
	Program        DramaProgramView          `json:"program"`
}

// DramaManifestV2ClipView 是以公开 ID 引用分镜和任务的画面片段。
type DramaManifestV2ClipView struct {
	SegmentID idcodec.ID `json:"segment_id"`
	TaskID    idcodec.ID `json:"task_id,omitempty"`
	model.DramaManifestV2ClipParams
}

// DramaManifestV2ViewFromDomain 将冻结清单投影为公开清单，素材 ID 保持十进制数字。
func DramaManifestV2ViewFromDomain(m model.DramaEpisodeManifestV2) DramaManifestV2View {
	view := DramaManifestV2View{
		Version: m.Version, RenderVersion: m.RenderVersion, EpisodeID: idcodec.ID(m.EpisodeID), SegmentVersion: m.SegmentVersion,
		FPS: m.FPS, SampleRate: m.SampleRate, TotalFrames: m.TotalFrames, SkippedSeqs: m.SkippedSeqs,
		Clips:   make([]DramaManifestV2ClipView, 0, len(m.Clips)),
		Program: DramaProgramView{Version: m.Program.Version, Audio: make([]DramaProgramAudioView, 0, len(m.Program.Audio))},
	}
	for _, clip := range m.Clips {
		view.Clips = append(view.Clips, DramaManifestV2ClipView{
			SegmentID: idcodec.ID(clip.SegmentID), TaskID: idcodec.ID(clip.TaskID), DramaManifestV2ClipParams: clip.DramaManifestV2ClipParams,
		})
	}
	for _, cue := range m.Program.Audio {
		view.Program.Audio = append(view.Program.Audio, dramaProgramAudioView(cue.DramaProgramAudioSource))
	}
	return view
}

// DramaManifestV2ResponseView 返回已完成解析的清单；诊断存在时哈希为空。
type DramaManifestV2ResponseView struct {
	Status             string                   `json:"status,omitempty"`
	Manifest           *DramaManifestV2View     `json:"manifest"`
	ManifestHash       string                   `json:"manifest_hash"`
	SegmentVersion     int64                    `json:"segment_version"`
	SkippedSeqs        []int                    `json:"skipped_seqs"`
	Diagnostics        []string                 `json:"diagnostics"`
	ProgramDiagnostics []DramaProgramDiagnostic `json:"program_diagnostics"`
}

// DramaManifestV2PrepareView 返回探针预热状态，此时不提供可提交的哈希。
type DramaManifestV2PrepareView struct {
	Status             string                   `json:"status"`
	SegmentVersion     int64                    `json:"segment_version"`
	SkippedSeqs        []int                    `json:"skipped_seqs"`
	Diagnostics        []string                 `json:"diagnostics"`
	ProgramDiagnostics []DramaProgramDiagnostic `json:"program_diagnostics"`
}
