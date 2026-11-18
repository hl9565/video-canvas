package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/idcodec"
)

// PutDramaProgramReq 携带剧集身份与内容版本，用于整体替换集级节目单。
type PutDramaProgramReq struct {
	ExpectedEpisodeID      idcodec.ID        `json:"expected_episode_id" binding:"required"`
	ExpectedSegmentVersion *int64            `json:"expected_segment_version" binding:"required,min=0"`
	Program                DramaProgramInput `json:"program"`
	RebindAnchorCueIDs     []string          `json:"rebind_anchor_cue_ids,omitempty"`
}

// DramaProgramInput 是客户端可提交的节目单；不接收服务器保存的源绑定。
type DramaProgramInput struct {
	Version  int                      `json:"v"`
	Audio    []DramaProgramAudioInput `json:"audio"`
	Overlays []json.RawMessage        `json:"overlays"`
}

// DramaProgramAudioInput 接受十进制数字或字符串素材 ID，以及编码的分镜 ID。
type DramaProgramAudioInput struct {
	model.DramaProgramAudioParams
	AssetID model.AssetID            `json:"asset_id"`
	Anchor  *DramaProgramAnchorInput `json:"anchor,omitempty"`
}

// DramaProgramAnchorInput 使用指针区分省略源帧与明确指定第零帧。
type DramaProgramAnchorInput struct {
	SegmentID idcodec.ID `json:"segment_id"`
	SourceF   *int32     `json:"source_f"`
}

// UnmarshalJSON 严格校验节目单结构，成功后才替换请求值。
func (r *PutDramaProgramReq) UnmarshalJSON(data []byte) error {
	req, err := DecodePutDramaProgramReq(bytes.NewReader(data))
	if err != nil {
		return err
	}
	*r = *req
	return nil
}

// DecodePutDramaProgramReq 从完整请求流读取一个节目单，拒绝未知字段与尾随数据。
func DecodePutDramaProgramReq(r io.Reader) (*PutDramaProgramReq, error) {
	type plain PutDramaProgramReq
	var decoded plain
	if err := model.DecodeDramaJSON(r, &decoded); err != nil {
		return nil, invalidProgramRequest(err, "program", "", "invalid_json")
	}
	req := PutDramaProgramReq(decoded)
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if req.Program.Audio == nil {
		req.Program.Audio = []DramaProgramAudioInput{}
	}
	if req.Program.Overlays == nil {
		req.Program.Overlays = []json.RawMessage{}
	}
	return &req, nil
}

// Validate 检查 CAS 基线与节目单参数，不读取分镜或素材。
func (r PutDramaProgramReq) Validate() error {
	if r.ExpectedEpisodeID == 0 {
		return invalidProgramRequest(&model.DramaProgramError{Msg: "必须指定 expected_episode_id"}, "expected_episode_id", "", "required")
	}
	if r.ExpectedSegmentVersion == nil || *r.ExpectedSegmentVersion < 0 {
		return invalidProgramRequest(&model.DramaProgramError{Msg: "必须指定非负的 expected_segment_version"}, "expected_segment_version", "", "invalid_version")
	}
	if len(r.Program.Overlays) != 0 {
		err := model.ValidateDramaProgramShape(r.Program.Version, len(r.Program.Audio), len(r.Program.Overlays))
		return invalidProgramRequest(err, "overlays", "", "overlays_unsupported")
	}
	program, err := r.Program.ToDomain()
	if err != nil {
		return invalidProgramRequest(err, "program", "", "invalid_program")
	}
	return validateDramaProgramRebind(program, r.RebindAnchorCueIDs)
}

// validateDramaProgramRebind 仅允许明确列出本次保存中存在的锚定音效，且每条只出现一次。
func validateDramaProgramRebind(program model.DramaProgram, ids []string) error {
	remaining := make(map[string]bool, len(program.Audio))
	for _, cue := range program.Audio {
		if cue.Anchor != nil && cue.Role == model.DramaAudioSFX {
			remaining[cue.ID] = true
		}
	}
	for _, id := range ids {
		available, exists := remaining[id]
		if !exists {
			return invalidProgramRequest(nil, "rebind_anchor_cue_ids", id, "rebind_cue_not_anchored")
		}
		if !available {
			return invalidProgramRequest(nil, "rebind_anchor_cue_ids", id, "rebind_cue_duplicate")
		}
		remaining[id] = false
	}
	return nil
}

func invalidProgramRequest(err error, field, id, reason string) error {
	var msg string
	var programErr *model.DramaProgramError
	if errors.As(err, &programErr) {
		msg = programErr.Msg
	}
	return &model.DramaProgramError{
		Msg:         msg,
		Diagnostics: []model.DramaProgramDiagnostic{{CueID: id, Field: field, Reason: reason, Severity: "error"}},
		Cause:       err,
	}
}

// DramaEpisodeProgramView 携带编辑节目单所需的集身份与 CAS 版本。
type DramaEpisodeProgramView struct {
	EpisodeID      idcodec.ID       `json:"episode_id"`
	SegmentVersion int64            `json:"segment_version"`
	EditSchema     int16            `json:"edit_schema"`
	Program        DramaProgramView `json:"program"`
}

// DramaProgramDiagnosticsData 是节目单业务错误附带的诊断数组。
type DramaProgramDiagnosticsData struct {
	Diagnostics []DramaProgramDiagnostic `json:"diagnostics"`
}

// ToDomain 校验参数，将分镜与素材 ID 转换为内部主键并规范化空数组。
func (p DramaProgramInput) ToDomain() (model.DramaProgram, error) {
	if err := model.ValidateDramaProgramShape(p.Version, len(p.Audio), len(p.Overlays)); err != nil {
		return model.DramaProgram{}, err
	}
	program := model.DramaProgram{Version: p.Version, Audio: make([]model.DramaProgramAudioCue, 0, len(p.Audio)), Overlays: []json.RawMessage{}}
	for _, cue := range p.Audio {
		value := model.DramaProgramAudioSource{DramaProgramAudioParams: cue.DramaProgramAudioParams, AssetID: uint64(cue.AssetID)}
		if cue.Anchor != nil {
			if cue.Anchor.SourceF == nil {
				return model.DramaProgram{}, &model.DramaProgramError{Msg: "锚点必须指定 source_f"}
			}
			value.Anchor = &model.DramaProgramAnchor{SegmentID: uint64(cue.Anchor.SegmentID), SourceF: *cue.Anchor.SourceF}
		}
		program.Audio = append(program.Audio, model.DramaProgramAudioCue{DramaProgramAudioSource: value})
	}
	if err := program.Validate(); err != nil {
		return model.DramaProgram{}, err
	}
	return program, nil
}

// DramaProgramView 是公开节目单；Stage C 的 overlays 固定输出空数组。
type DramaProgramView struct {
	Version  int                     `json:"v"`
	Audio    []DramaProgramAudioView `json:"audio"`
	Overlays [0]struct{}             `json:"overlays"`
}

// DramaProgramAudioView 延续 Stage B 的十进制数字素材 ID，仅公开可编辑参数。
type DramaProgramAudioView struct {
	model.DramaProgramAudioParams
	AssetID uint64                  `json:"asset_id"`
	Anchor  *DramaProgramAnchorView `json:"anchor,omitempty"`
}

// DramaProgramAnchorView 公开编码后的分镜 ID 和源视频帧。
type DramaProgramAnchorView struct {
	SegmentID idcodec.ID `json:"segment_id"`
	SourceF   int32      `json:"source_f"`
}

// DramaProgramViewFromDomain 投影公开节目单，锚点绑定信息保留在存储结构内。
func DramaProgramViewFromDomain(p model.DramaProgram) DramaProgramView {
	view := DramaProgramView{Version: p.Version, Audio: make([]DramaProgramAudioView, 0, len(p.Audio))}
	for _, cue := range p.Audio {
		view.Audio = append(view.Audio, dramaProgramAudioView(cue.DramaProgramAudioSource))
	}
	return view
}

func dramaProgramAudioView(a model.DramaProgramAudioSource) DramaProgramAudioView {
	view := DramaProgramAudioView{DramaProgramAudioParams: a.DramaProgramAudioParams, AssetID: a.AssetID}
	if a.Anchor != nil {
		view.Anchor = &DramaProgramAnchorView{SegmentID: idcodec.ID(a.Anchor.SegmentID), SourceF: a.Anchor.SourceF}
	}
	return view
}

// DramaProgramDiagnostic 指向节目单中的具体条目、分镜或参数。
type DramaProgramDiagnostic struct {
	CueID     string     `json:"cue_id,omitempty"`
	SegmentID idcodec.ID `json:"segment_id,omitempty"`
	Field     string     `json:"field"`
	Reason    string     `json:"reason"`
	Severity  string     `json:"severity"`
}

func dramaProgramDiagnosticsView(items []model.DramaProgramDiagnostic) []DramaProgramDiagnostic {
	out := make([]DramaProgramDiagnostic, 0, len(items))
	for _, item := range items {
		out = append(out, DramaProgramDiagnostic{
			CueID: item.CueID, SegmentID: idcodec.ID(item.SegmentID), Field: item.Field, Reason: item.Reason, Severity: item.Severity,
		})
	}
	return out
}
