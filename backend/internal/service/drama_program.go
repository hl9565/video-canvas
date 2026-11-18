package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/repository"
)

// dramaProgramTx 单独声明节目单写入能力，通用集保存不接收节目单。
type dramaProgramTx interface {
	// SaveEpisodeProgram 以集主键和片段版本为条件，仅写入节目单及版本。
	SaveEpisodeProgram(context.Context, uint64, uint64, uint64, int64, int16, json.RawMessage, bool) error
}

// dramaProgramAssetLocker 防止生产数据库中的素材在事务重验后被替换。
type dramaProgramAssetLocker interface {
	// LockProgramAssets 按主键顺序锁定本次写入使用的素材。
	LockProgramAssets(context.Context, uint64, []uint64) error
}

var (
	_ dramaProgramTx          = (*repository.DramaRepository)(nil)
	_ dramaProgramAssetLocker = (*repository.DramaRepository)(nil)
)

// DramaContractError 将 model 层的阶段三契约校验错误翻译为约定业务错误，保留文案与结构化诊断。
func DramaContractError(err error) error {
	if err == nil {
		return nil
	}
	var programErr *model.DramaProgramError
	if errors.As(err, &programErr) {
		code := errcode.ErrDramaProgramInvalid
		if programErr.Msg != "" {
			code = code.WithMsg(programErr.Msg)
		}
		if len(programErr.Diagnostics) != 0 {
			code = code.WithData(DramaProgramDiagnosticsData{Diagnostics: dramaProgramDiagnosticsView(programErr.Diagnostics)})
		}
		return code
	}
	var visualErr *model.DramaVisualError
	if errors.As(err, &visualErr) {
		code := errcode.ErrDramaVisualInvalid
		if visualErr.Msg != "" {
			code = code.WithMsg(visualErr.Msg)
		}
		return code
	}
	return err
}

// GetProgram 只读取可编辑节目单，不探测媒体，失效引用仍可查看和修复。
func (s *DramaService) GetProgram(ctx context.Context, userID, dramaID uint64, no int) (*DramaEpisodeProgramView, error) {
	_, ep, err := s.readProgramEpisode(ctx, userID, dramaID, no)
	if err != nil {
		return nil, err
	}
	program, err := model.DecodeStoredDramaProgram(json.RawMessage(ep.ProgramJSON))
	if err != nil {
		return nil, err
	}
	return episodeProgramView(ep, program), nil
}

// PutProgram 以集身份和片段版本整体替换节目单，并在锁外探测新锚点。
func (s *DramaService) PutProgram(ctx context.Context, userID, dramaID uint64, no int, req *PutDramaProgramReq) (*DramaEpisodeProgramView, error) {
	// 1. 请求及版本基线合法后才校验素材。
	if req == nil {
		return nil, programDiagnosticError(errcode.ErrDramaProgramInvalid, []DramaProgramDiagnostic{{Field: "program", Reason: "required", Severity: "error"}})
	}
	if err := req.Validate(); err != nil {
		return nil, DramaContractError(err)
	}
	p, ep, err := s.readProgramEpisode(ctx, userID, dramaID, no)
	if p != nil && errors.Is(err, errcode.ErrDramaNotFound) {
		return nil, errcode.ErrDramaManifestStale
	}
	if err != nil {
		return nil, err
	}
	if !matchesProgramBaseline(ep, req) {
		return nil, errcode.ErrDramaManifestStale
	}
	program, err := req.Program.ToDomain()
	if err != nil {
		return nil, DramaContractError(err)
	}
	if err := checkProgramRatio(p.Ratio, program); err != nil {
		return nil, err
	}
	// 2. 已保存的同坐标绑定可以失效，但不能随无关节目单改动而更新。
	bindings, audio, err := s.prepareProgramSave(ctx, p, ep, program, req.RebindAnchorCueIDs)
	if err != nil {
		return nil, err
	}
	// 3. 项目、集、分镜依次锁定，核对所有探测用到的身份再保存。
	var saved *DramaEpisodeProgramView
	var changed bool
	err = s.repo.WithTx(ctx, func(tx repository.DramaTx) error {
		saved, changed, err = saveLockedDramaProgram(ctx, tx, p, no, req, program, bindings, audio)
		return err
	})
	if errors.Is(err, repository.ErrStateConflict) {
		return nil, errcode.ErrDramaManifestStale
	}
	if err != nil {
		return nil, dramaError(err)
	}
	// 4. 只在内容变更成功提交后通知状态刷新。
	if changed {
		s.publishEpisode(ctx, &model.DramaJob{UserID: userID, DramaID: dramaID}, no)
	}
	return saved, nil
}

const dramaStageCRatio = "16:9"

// checkDramaRatioUpdate 在项目锁内阻止已编排节目单或视觉效果的项目切到不支持的画幅。
func checkDramaRatioUpdate(ctx context.Context, tx repository.DramaTx, p *model.DramaProject, ratio string) error {
	if ratio == dramaStageCRatio {
		return nil
	}
	episodes, err := tx.Episodes(ctx, p.UserID, p.ID)
	if err != nil {
		return err
	}
	for _, ep := range episodes {
		if err := checkEpisodeRatioUpdate(ctx, tx, ep); err != nil {
			return err
		}
	}
	return nil
}

// checkEpisodeRatioUpdate 只检测实际内容，精裁 schema 和空节目单都不限制画幅。
func checkEpisodeRatioUpdate(ctx context.Context, tx repository.DramaTx, ep model.DramaEpisode) error {
	program, err := model.DecodeStoredDramaProgram(json.RawMessage(ep.ProgramJSON))
	if err != nil {
		return err
	}
	if program.HasAuthoredContent() {
		return errcode.ErrDramaVisualInvalid.WithMsg("请先清空节目单，再更改项目画幅")
	}
	rows, err := tx.Segments(ctx, ep.UserID, ep.ID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		row.FXJSON = model.CanonicalDramaSegmentFXJSON(row.FXJSON)
		if dramaSegmentUsesStageC(row) {
			return errcode.ErrDramaVisualInvalid.WithMsg("请先移除静帧、黑场和特效，再更改项目画幅")
		}
	}
	return nil
}

// checkProgramRatio 只有 16:9 支持编排，任何画幅均可清空节目单。
func checkProgramRatio(ratio string, program model.DramaProgram) error {
	if ratio != dramaStageCRatio && program.HasAuthoredContent() {
		return errcode.ErrDramaProgramInvalid.WithMsg("节目单仅支持 16:9 项目")
	}
	return nil
}

// readProgramEpisode 读取当前用户的项目及对应集号快照。
func (s *DramaService) readProgramEpisode(ctx context.Context, user, drama uint64, no int) (*model.DramaProject, *model.DramaEpisode, error) {
	p, err := s.repo.Project(ctx, user, drama, false)
	if err != nil {
		return nil, nil, dramaError(err)
	}
	episodes, err := s.repo.Episodes(ctx, user, drama)
	if err != nil {
		return nil, nil, dramaError(err)
	}
	for i := range episodes {
		if episodes[i].No == no {
			return p, &episodes[i], nil
		}
	}
	return p, nil, errcode.ErrDramaNotFound
}

// matchesProgramBaseline 同时比对身份和素材版本，避免旧集号命中重排后的集。
func matchesProgramBaseline(ep *model.DramaEpisode, req *PutDramaProgramReq) bool {
	return ep.ID == uint64(req.ExpectedEpisodeID) && ep.SegmentVersion == *req.ExpectedSegmentVersion
}

// episodeProgramView 去掉仅在数据库中保存的绑定指纹。
func episodeProgramView(ep *model.DramaEpisode, p model.DramaProgram) *DramaEpisodeProgramView {
	return &DramaEpisodeProgramView{EpisodeID: idcodec.ID(ep.ID), SegmentVersion: ep.SegmentVersion, EditSchema: ep.EditSchema, Program: DramaProgramViewFromDomain(p)}
}

// saveLockedDramaProgram 在事务中重验素材及锚点，禁止重新探测或改绑到另一来源。
func saveLockedDramaProgram(ctx context.Context, tx repository.DramaTx, p *model.DramaProject, no int, req *PutDramaProgramReq, program model.DramaProgram, bindings []programAnchorBinding, audio map[uint64]string) (*DramaEpisodeProgramView, bool, error) {
	project, err := tx.Project(ctx, p.UserID, p.ID, true)
	if err != nil {
		return nil, false, err
	}
	episodes, err := tx.LockEpisodes(ctx, p.UserID, []uint64{uint64(req.ExpectedEpisodeID)})
	if errors.Is(err, repository.ErrNotFound) {
		return nil, false, errcode.ErrDramaManifestStale
	}
	if err != nil {
		return nil, false, err
	}
	if len(episodes) != 1 || episodes[0].DramaID != p.ID || episodes[0].No != no || !matchesProgramBaseline(&episodes[0], req) {
		return nil, false, errcode.ErrDramaManifestStale
	}
	if err := checkProgramRatio(project.Ratio, program); err != nil {
		return nil, false, err
	}
	ep := &episodes[0]
	rows, err := lockProgramAnchorRows(ctx, tx, p.UserID, bindings)
	if err != nil {
		return nil, false, err
	}
	if err := lockProgramAssets(ctx, tx, p.UserID, bindings, audio); err != nil {
		return nil, false, err
	}
	if err := recheckProgramAnchors(ctx, tx, project, ep.ID, program, bindings, rows); err != nil {
		return nil, false, err
	}
	if err := recheckProgramAudio(ctx, tx, p.UserID, program, audio); err != nil {
		return nil, false, err
	}
	return writeDramaProgram(ctx, tx, ep, program)
}

// lockProgramAssets 在分镜锁之后按主键锁定媒体，让第二次元数据读取保持有效直到提交。
func lockProgramAssets(ctx context.Context, tx repository.DramaTx, user uint64, bindings []programAnchorBinding, audio map[uint64]string) error {
	locker, ok := tx.(dramaProgramAssetLocker)
	if !ok {
		return nil // 内存假仓储没有独立媒体事务，只需要第二次元数据读取。
	}
	ids := make([]uint64, 0, len(bindings)+len(audio))
	for id := range audio {
		ids = append(ids, id)
	}
	for _, b := range bindings {
		ids = append(ids, b.source.AssetID)
	}
	slices.Sort(ids)
	return locker.LockProgramAssets(ctx, user, slices.Compact(ids))
}

// writeDramaProgram 比较规范内容，空值格式迁移及相同内容不递增片段版本。
func writeDramaProgram(ctx context.Context, tx repository.DramaTx, ep *model.DramaEpisode, program model.DramaProgram) (*DramaEpisodeProgramView, bool, error) {
	writer, ok := tx.(dramaProgramTx)
	if !ok {
		return nil, false, errcode.ErrInternal
	}
	prev, err := model.DecodeStoredDramaProgram(json.RawMessage(ep.ProgramJSON))
	if err != nil {
		return nil, false, err
	}
	before, err := json.Marshal(prev)
	if err != nil {
		return nil, false, err
	}
	after, err := json.Marshal(program)
	if err != nil {
		return nil, false, err
	}
	changed := !bytes.Equal(before, after)
	nextSchema := ep.EditSchema
	if changed {
		nextSchema = 1
	}
	if err := writer.SaveEpisodeProgram(ctx, ep.UserID, ep.DramaID, ep.ID, ep.SegmentVersion, nextSchema, after, changed); err != nil {
		return nil, false, err
	}
	if changed {
		ep.EditSchema = nextSchema
		ep.SegmentVersion++
	}
	return episodeProgramView(ep, program), changed, nil
}
