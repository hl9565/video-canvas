// Package dramasegmentfake 提供片段service与handler共用的内存仓储，保持事务回滚和归属语义。
package dramasegmentfake

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"time"

	"video-canvas/internal/drama"
	"video-canvas/internal/model"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/ws"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
)

// Repo 是片段测试的完整依赖，未使用的短剧原语通过嵌入暴露误调用。
type Repo struct {
	repository.DramaTx
	Rows            []model.DramaSegment
	FrameRuns       []model.DramaSegmentFrameRun
	TakeLinks       []model.DramaSegmentTakeLink
	Tasks           []model.GenerationTask
	Messages        []ws.Message
	ProjectDeleted  bool
	CanvasDeleted   bool
	Unlocked        bool
	InTx            bool
	JobRows         []model.DramaJob
	Created         *model.CreateGenerationTaskReq
	IdempotencyKey  string
	CreateError     *model.TaskItemError
	Err             error
	SegmentVersion  int64
	EditSchema      int16
	EpisodeRevision int64
	Ratio           string
	ProgramJSON     json.RawMessage
	Assets          map[uint64]model.Asset
	MediaProbes     map[uint64]model.AssetMediaProbe
	// Credit 是用户 1 的积分账户（nil 表示没有账户）；Ledger 记录写入的流水
	Credit *model.UserCredit
	Ledger []model.CreditLedger
	nextID uint64
}

// New 返回用户1、项目7、画布9、集10的独立测试仓储。
func New() *Repo { return &Repo{nextID: 1, Ratio: "16:9"} }

// WithTx 在错误时恢复片段快照，防止非法重排造成部分写入。
func (r *Repo) WithTx(_ context.Context, fn func(repository.DramaTx) error) error {
	if r.Err != nil {
		return r.Err
	}
	before := *r
	r.InTx = true
	defer func() { r.InTx = false }()
	before.Rows = slices.Clone(r.Rows)
	before.FrameRuns = selectFakeRows(r.FrameRuns, nil, cloneFrameRun)
	before.TakeLinks = selectFakeRows(r.TakeLinks, nil, cloneTakeLink)
	before.Ledger, before.Credit = slices.Clone(r.Ledger), clonePointer(r.Credit)
	err := fn(r)
	if err != nil {
		*r = before
	}
	return err
}

// WithSnapshotTx 在只读一致性快照中运行读闭包，默认沿用内存测试仓储的事务克隆隔离。
func (r *Repo) WithSnapshotTx(ctx context.Context, fn func(repository.DramaTx) error) error {
	return r.WithTx(ctx, fn)
}

// Project 只返回当前用户的存活项目。
func (r *Repo) Project(_ context.Context, userID, id uint64, _ bool) (*model.DramaProject, error) {
	if r.Err != nil {
		return nil, r.Err
	}
	if r.ProjectDeleted || userID != 1 || id != 7 {
		return nil, repository.ErrNotFound
	}
	return &model.DramaProject{BaseModel: model.BaseModel{ID: 7}, UserID: 1, CanvasID: 9, Ratio: r.Ratio, Revision: 1}, nil
}

// Episodes 返回存活项目的一集。
func (r *Repo) Episodes(ctx context.Context, userID, id uint64) ([]model.DramaEpisode, error) {
	if _, err := r.Project(ctx, userID, id, false); err != nil {
		return nil, err
	}
	var lockedAt *time.Time
	if !r.Unlocked {
		v := time.Now()
		lockedAt = &v
	}
	return []model.DramaEpisode{{BaseModel: model.BaseModel{ID: 10}, UserID: 1, DramaID: 7, No: 1, LockedAt: lockedAt, Revision: max(r.EpisodeRevision, 1), EditSchema: r.EditSchema, SegmentVersion: r.SegmentVersion, ProgramJSON: []byte(slices.Clone(r.ProgramJSON))}}, nil
}

// GetByID 返回用户 1 的画布 9；模拟软删后来源画布不再可读。
func (r *Repo) GetByID(_ context.Context, userID, canvasID uint64) (*model.CanvasProject, error) {
	if r.Err != nil {
		return nil, r.Err
	}
	if userID != 1 || canvasID != 9 || r.CanvasDeleted {
		return nil, repository.ErrNotFound
	}
	return &model.CanvasProject{BaseModel: model.BaseModel{ID: 9}, UserID: 1}, nil
}

// SegmentEpisode 校验分集ID与父项目归属。
func (r *Repo) SegmentEpisode(ctx context.Context, userID, episodeID uint64) (*model.DramaEpisode, error) {
	eps, err := r.Episodes(ctx, userID, 7)
	if err != nil {
		return nil, err
	}
	if episodeID != 10 {
		return nil, repository.ErrNotFound
	}
	return &eps[0], nil
}

// Segment 返回当前用户的存活片段副本。
func (r *Repo) Segment(ctx context.Context, userID, sid uint64, _ bool) (*model.DramaSegment, error) {
	if _, err := r.Project(ctx, userID, 7, false); err != nil {
		return nil, err
	}
	for _, row := range r.Rows {
		if row.ID == sid && row.UserID == userID && !row.DeletedAt.Valid {
			return &row, nil
		}
	}
	return nil, repository.ErrNotFound
}

// Segments 返回本集片段，按seq、ID排序。
func (r *Repo) Segments(ctx context.Context, userID, episodeID uint64) ([]model.DramaSegment, error) {
	if _, err := r.SegmentEpisode(ctx, userID, episodeID); err != nil {
		return nil, err
	}
	rows := selectFakeRows(r.Rows, func(row model.DramaSegment) bool {
		return row.UserID == userID && row.EpisodeID == episodeID && !row.DeletedAt.Valid
	}, nil)
	slices.SortFunc(rows, func(a, b model.DramaSegment) int {
		return cmp.Or(cmp.Compare(a.Seq, b.Seq), cmp.Compare(a.ID, b.ID))
	})
	return rows, nil
}

// SaveSegment 插入或CAS更新，原样保留任务与素材。
func (r *Repo) SaveSegment(ctx context.Context, row *model.DramaSegment, revision int64) error {
	changed, err := r.SaveSegmentNoBump(ctx, row, revision)
	if err != nil || !changed {
		return err
	}
	return r.BumpSegmentVersion(ctx, row.UserID, row.EpisodeID)
}

// SaveSegmentNoBump 保存片段但不递增 SegmentVersion，返回是否发生实质变更。
func (r *Repo) SaveSegmentNoBump(ctx context.Context, row *model.DramaSegment, revision int64) (bool, error) {
	if _, err := r.SegmentEpisode(ctx, row.UserID, row.EpisodeID); err != nil {
		return false, err
	}
	row.VisualKind = row.EffectiveVisualKind()
	row.FXJSON = model.CanonicalDramaSegmentFXJSON(row.FXJSON)
	if row.ID == 0 {
		row.ID, row.CreatedAt, row.UpdatedAt = r.nextID, time.Now(), time.Now()
		r.nextID++
		r.Rows = append(r.Rows, *row)
		return true, nil
	}
	i := slices.IndexFunc(r.Rows, func(old model.DramaSegment) bool {
		return old.ID == row.ID && old.UserID == row.UserID && !old.DeletedAt.Valid
	})
	if i < 0 || r.Rows[i].Revision != revision {
		return false, repository.ErrStateConflict
	}
	changed := segmentMaterialChanged(r.Rows[i], *row)
	row.Revision, row.UpdatedAt = revision+1, time.Now()
	r.Rows[i] = *row
	return changed, nil
}

// DeleteSegment 软删片段，不动Tasks。
func (r *Repo) DeleteSegment(ctx context.Context, userID, sid uint64) error {
	epID, err := r.DeleteSegmentNoBump(ctx, userID, sid)
	if err != nil {
		return err
	}
	return r.BumpSegmentVersion(ctx, userID, epID)
}

// DeleteSegmentNoBump 软删片段但不递增 SegmentVersion，返回所属 episodeID。
func (r *Repo) DeleteSegmentNoBump(ctx context.Context, userID, sid uint64) (uint64, error) {
	row, err := r.Segment(ctx, userID, sid, false)
	if err != nil {
		return 0, err
	}
	if err := r.CancelSegmentFrameRuns(ctx, userID, sid); err != nil {
		return 0, err
	}
	i := slices.IndexFunc(r.Rows, func(old model.DramaSegment) bool { return old.ID == sid && old.UserID == userID })
	r.Rows[i].DeletedAt.Time, r.Rows[i].DeletedAt.Valid = time.Now(), true
	return row.EpisodeID, nil
}

// BumpSegmentVersion 显式递增假仓储的 SegmentVersion。
func (r *Repo) BumpSegmentVersion(ctx context.Context, userID, episodeID uint64) error {
	if _, err := r.SegmentEpisode(ctx, userID, episodeID); err != nil {
		return err
	}
	r.SegmentVersion++
	return nil
}

// ReplaceSegments 软删用户的全集片段并替换，任务与素材保留，集版本只递增一次。
func (r *Repo) ReplaceSegments(ctx context.Context, userID, episodeID uint64, rows []model.DramaSegment) error {
	if _, err := r.SegmentEpisode(ctx, userID, episodeID); err != nil {
		return err
	}
	if err := r.CancelEpisodeFrameRuns(ctx, userID, episodeID); err != nil {
		return err
	}
	now := time.Now()
	for i := range r.Rows {
		row := &r.Rows[i]
		if row.UserID == userID && row.EpisodeID == episodeID && !row.DeletedAt.Valid {
			row.DeletedAt.Time, row.DeletedAt.Valid = now, true
		}
		r.nextID = max(r.nextID, row.ID+1)
	}
	for i := range rows {
		rows[i].BaseModel = model.BaseModel{}
		rows[i].UserID, rows[i].EpisodeID, rows[i].Seq, rows[i].Revision = userID, episodeID, i, 1
		if _, err := r.SaveSegmentNoBump(ctx, &rows[i], 0); err != nil {
			return err
		}
	}
	return r.BumpSegmentVersion(ctx, userID, episodeID)
}

// SegmentTasks 按用户、画布、节点过滤正式任务，并稳定按创建时间和ID排序。
func (r *Repo) SegmentTasks(_ context.Context, userID, canvasID uint64, nodeID string) ([]model.GenerationTask, error) {
	rows := selectFakeRows(r.Tasks, func(task model.GenerationTask) bool {
		return task.UserID == userID && task.CanvasProjectID != nil && *task.CanvasProjectID == canvasID && task.NodeID == nodeID && !task.IsTest
	}, nil)
	slices.SortFunc(rows, func(a, b model.GenerationTask) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return rows, nil
}

// Create 记录生成参数，并模拟现有生成服务的逐项结果。
func (r *Repo) Create(_ context.Context, _ uint64, key string, req *model.CreateGenerationTaskReq) (*model.CreateGenerationTaskResp, error) {
	r.Created, r.IdempotencyKey = req, key
	if r.CreateError != nil {
		return &model.CreateGenerationTaskResp{Items: []model.CreateTaskItem{{NodeID: req.NodeID, Error: r.CreateError}}}, nil
	}
	canvas := req.CanvasID
	task := &model.GenerationTaskView{ID: 101, CanvasProjectID: &canvas, NodeID: req.NodeID, Kind: req.Kind, Status: model.TaskPending}
	return &model.CreateGenerationTaskResp{Items: []model.CreateTaskItem{{NodeID: req.NodeID, Task: task}}}, nil
}

// View 返回测试素材视图。
func (r *Repo) View(_ context.Context, _, id uint64) (*model.AssetView, error) {
	return &model.AssetView{ID: id, URL: "/files/test", Kind: "video"}, nil
}

// Publish 记录完整用户事件。
func (r *Repo) Publish(_ context.Context, _ string, message ws.Message) {
	r.Messages = append(r.Messages, message)
}

// HasSegmentTakes 检查现存片段的有效导入候选、双向匹配流程及原生视频任务，忽略悬空关联与流程孤儿。
func (r *Repo) HasSegmentTakes(ctx context.Context, userID, episodeID uint64) (bool, error) {
	rows, err := r.Segments(ctx, userID, episodeID)
	if err != nil {
		return false, err
	}
	taskByID := make(map[uint64]model.GenerationTask, len(r.Tasks))
	for _, task := range r.Tasks {
		taskByID[task.ID] = task
	}
	for _, row := range rows {
		for _, run := range r.FrameRuns {
			if run.UserID != userID || run.SegmentID != row.ID {
				continue
			}
			if run.State != model.FrameRunStateFailed && run.State != model.FrameRunStateCancelled && run.State != model.FrameRunStateBound {
				return true, nil
			}
			if run.State == model.FrameRunStateBound && run.VideoTaskID != nil {
				if task, ok := taskByID[*run.VideoTaskID]; ok && task.UserID == userID && !task.IsTest && task.Kind == model.KindVideo &&
					task.FrameRunID != nil && *task.FrameRunID == run.ID && task.FrameRunConfirmRev != nil && *task.FrameRunConfirmRev == run.ConfirmedVersion {
					return true, nil
				}
			}
		}
		for _, link := range r.TakeLinks {
			if link.UserID != userID || link.SegmentID != row.ID {
				continue
			}
			task, ok := taskByID[link.TaskID]
			if !ok || task.UserID != userID || task.IsTest || task.Kind != model.KindVideo || task.FrameRunID != nil || task.CanvasProjectID == nil {
				continue
			}
			if link.Origin == model.TakeOriginCanvasImport && *task.CanvasProjectID == link.SourceCanvasID ||
				link.Origin == model.TakeOriginNative && *task.CanvasProjectID == 9 {
				return true, nil
			}
		}
		if slices.ContainsFunc(r.Tasks, func(task model.GenerationTask) bool {
			return task.UserID == userID && task.CanvasProjectID != nil && *task.CanvasProjectID == 9 && task.NodeID == "seg:"+idcodec.Encode(row.ID) && !task.IsTest && task.Kind == model.KindVideo && task.FrameRunID == nil
		}) {
			return true, nil
		}
	}
	return false, nil
}

// sameChosen 比较可空采用ID，让共享fake遵循真实仓储的过期判定。
func sameChosen(a, b *uint64) bool {
	return a == b || a != nil && b != nil && *a == *b
}

// Jobs 返回用户项目下的可配置任务，默认没有短剧媒体任务。
func (r *Repo) Jobs(_ context.Context, userID, dramaID uint64, active bool) ([]model.DramaJob, error) {
	return selectFakeRows(r.JobRows, func(job model.DramaJob) bool {
		return job.UserID == userID && job.DramaID == dramaID && (!active || job.Status == "queued" || job.Status == "running")
	}, nil), r.Err
}

// Adapter 模拟文本模型转写：返回预设的 Out / Err；Gate 非空时阻塞到被关闭，用来测并发。
type Adapter struct {
	Out     string
	Credits int
	Err     error
	Calls   []drama.AdaptInput
	Format  []string
	Gate    chan struct{}
	// Entered 非空时，每次进入转写先发一个信号，测试据此确认第一次调用已经拿到锁
	Entered chan struct{}
	Mutate  func()
}

// AdaptSegment 记录输入并返回预设结果；Mutate 在返回前执行，模拟转写期间用户改了原稿。
func (a *Adapter) AdaptSegment(_ context.Context, _ *model.DramaProject, format string, in drama.AdaptInput) (string, int, error) {
	a.Calls, a.Format = append(a.Calls, in), append(a.Format, format)
	if a.Entered != nil {
		a.Entered <- struct{}{}
	}
	if a.Gate != nil {
		<-a.Gate
	}
	if a.Mutate != nil {
		a.Mutate()
	}
	if a.Err != nil {
		return "", 0, a.Err
	}
	return a.Out, a.Credits, nil
}

// EnsureCredit 没有账户时按初始积分建一个。
func (r *Repo) EnsureCredit(_ context.Context, userID uint64, initial int) error {
	if r.Credit == nil {
		r.Credit = &model.UserCredit{UserID: userID, Balance: initial}
	}
	return nil
}

// LockCredit 返回账户副本；没有账户返回 ErrNotFound。
func (r *Repo) LockCredit(_ context.Context, userID uint64) (*model.UserCredit, error) {
	if r.Credit == nil || r.Credit.UserID != userID {
		return nil, repository.ErrNotFound
	}
	return clonePointer(r.Credit), nil
}

// AddCredit 增减余额和冻结额。
func (r *Repo) AddCredit(_ context.Context, _ uint64, balance, frozen int) error {
	if r.Credit == nil {
		return repository.ErrNotFound
	}
	r.Credit.Balance += balance
	r.Credit.Frozen += frozen
	return nil
}

// InsertLedger 记录流水。
func (r *Repo) InsertLedger(_ context.Context, e *model.CreditLedger) (bool, error) {
	r.Ledger = append(r.Ledger, *e)
	return true, nil
}

// Quote 记录报价参数并返回固定测试报价。
func (r *Repo) Quote(_ context.Context, _ uint64, req *model.QuoteGenerationTaskReq, _ ...service.QuoteTaskOptions) (*model.TaskQuoteView, error) {
	if r.Err != nil {
		return nil, r.Err
	}
	return &model.TaskQuoteView{EffectiveInput: req.Input, Credits: 10, QuoteToken: "test-quote", ExpiresAt: time.Now().Add(time.Minute)}, nil
}

// CheckIdempotentReplay 默认没有幂等重放；真实意图校验由任务服务测试覆盖。
func (r *Repo) CheckIdempotentReplay(context.Context, uint64, string, *model.CreateGenerationTaskReq) (*model.GenerationTaskView, error) {
	return nil, nil
}

// LockEpisodes 返回指定集副本，模拟项目之后的集锁。
func (r *Repo) LockEpisodes(ctx context.Context, userID uint64, ids []uint64) ([]model.DramaEpisode, error) {
	rows := []model.DramaEpisode{}
	for _, id := range ids {
		ep, err := r.SegmentEpisode(ctx, userID, id)
		if err != nil {
			return nil, err
		}
		rows = append(rows, *ep)
	}
	return rows, nil
}

// LockSegments 返回指定片段副本，模拟集之后的片段锁。
func (r *Repo) LockSegments(ctx context.Context, userID uint64, ids []uint64) ([]model.DramaSegment, error) {
	rows := []model.DramaSegment{}
	for _, id := range ids {
		row, err := r.Segment(ctx, userID, id, false)
		if err != nil {
			return nil, err
		}
		rows = append(rows, *row)
	}
	return rows, nil
}

// List 返回测试用户唯一的项目。
func (r *Repo) List(ctx context.Context, userID uint64, _ string, _, _ int) ([]model.DramaProject, int64, error) {
	p, err := r.Project(ctx, userID, 7, false)
	if err != nil {
		return nil, 0, err
	}
	return []model.DramaProject{*p}, 1, nil
}

// Get 返回当前用户的素材元数据。
func (r *Repo) Get(ctx context.Context, userID, id uint64) (*model.Asset, error) {
	return r.ComposeAsset(ctx, userID, id)
}

// ComposeAsset 仅读取当前用户的素材快照。
func (r *Repo) ComposeAsset(_ context.Context, userID, id uint64) (*model.Asset, error) {
	asset, ok := r.Assets[id]
	if !ok || asset.UserID != userID {
		return nil, repository.ErrNotFound
	}
	return &asset, nil
}

// SegmentVideoProbe 只返回匹配快照素材指纹与版本的探测缓存。
func (r *Repo) SegmentVideoProbe(_ context.Context, id uint64, sha256 string, version int16) (*model.AssetMediaProbe, error) {
	probe, ok := r.MediaProbes[id]
	if !ok || probe.Sha256 != sha256 || probe.ProbeVersion != version {
		return nil, repository.ErrNotFound
	}
	return &probe, nil
}

// SaveEpisodeProgram 模拟集主键和片段版本 CAS，节目单保存不影响其他集字段。
func (r *Repo) SaveEpisodeProgram(ctx context.Context, userID, dramaID, epID uint64, expectedSegmentVersion int64, editSchema int16, programJSON json.RawMessage, materialChange bool) error {
	if _, err := r.Project(ctx, userID, dramaID, false); err != nil || epID != 10 || expectedSegmentVersion != r.SegmentVersion {
		return repository.ErrStateConflict
	}
	r.ProgramJSON = slices.Clone(programJSON)
	r.EpisodeRevision = max(r.EpisodeRevision, 1) + 1
	if editSchema > 0 {
		r.EditSchema = editSchema
	}
	if materialChange {
		r.SegmentVersion++
	}
	return nil
}

// SaveEpisode 模拟集级版本校验与精裁标记保存，不额外递增片段版本。
func (r *Repo) SaveEpisode(_ context.Context, ep *model.DramaEpisode, revision int64) error {
	if revision != max(r.EpisodeRevision, 1) {
		return repository.ErrStateConflict
	}
	r.EditSchema, r.EpisodeRevision = ep.EditSchema, revision+1
	ep.Revision = r.EpisodeRevision
	return nil
}

// segmentMaterialChanged 判断需要递增集版本的采用、顺序或精裁变更。
func segmentMaterialChanged(a, b model.DramaSegment) bool {
	sameOut := a.SourceOutF == nil && b.SourceOutF == nil || a.SourceOutF != nil && b.SourceOutF != nil && *a.SourceOutF == *b.SourceOutF
	return a.EffectiveVisualKind() != b.EffectiveVisualKind() ||
		!sameChosen(a.StillAssetID, b.StillAssetID) ||
		!bytes.Equal(model.CanonicalDramaSegmentFXJSON(a.FXJSON), model.CanonicalDramaSegmentFXJSON(b.FXJSON)) ||
		a.Seq != b.Seq || a.ChosenCleared != b.ChosenCleared || !sameChosen(a.ChosenTaskID, b.ChosenTaskID) || a.SourceInF != b.SourceInF || !sameOut || a.MuteOriginal != b.MuteOriginal || math.Round(a.AudioGainDB*100) != math.Round(b.AudioGainDB*100) || a.NativeIsDialogue != b.NativeIsDialogue || a.AvPtsOffsetMs != b.AvPtsOffsetMs
}

func clonePointer[T any](p *T) *T {
	if p == nil {
		return nil
	}
	return new(*p)
}

// selectFakeRows 统一复制、筛选假仓储结果，指针快照使用显式复制函数避免写穿。
func selectFakeRows[T any](rows []T, keep func(T) bool, clone func(T) T) []T {
	out := append([]T{}, rows...)
	if keep != nil {
		out = slices.DeleteFunc(out, func(row T) bool { return !keep(row) })
	}
	if clone != nil {
		for i := range out {
			out[i] = clone(out[i])
		}
	}
	return out
}

// cloneTakeLink 隔离候选关联中的可空流程指针。
func cloneTakeLink(row model.DramaSegmentTakeLink) model.DramaSegmentTakeLink {
	row.RunID = clonePointer(row.RunID)
	return row
}

func cloneFrameRun(row model.DramaSegmentFrameRun) model.DramaSegmentFrameRun {
	row.OriginCanvasID, row.FrameTaskID = clonePointer(row.OriginCanvasID), clonePointer(row.FrameTaskID)
	row.FrameAssetID, row.VideoTaskID = clonePointer(row.FrameAssetID), clonePointer(row.VideoTaskID)
	row.ConfirmedInputHash, row.QuoteToken = clonePointer(row.ConfirmedInputHash), clonePointer(row.QuoteToken)
	row.SubmittingAt, row.ConfirmedInput = clonePointer(row.SubmittingAt), slices.Clone(row.ConfirmedInput)
	return row
}

func activeFrameRun(state model.DramaFrameRunState) bool {
	return slices.Contains([]model.DramaFrameRunState{model.FrameRunStateDraft, model.FrameRunStateFramePending,
		model.FrameRunStateFrameReady, model.FrameRunStateConfirmed, model.FrameRunStateSubmitting, model.FrameRunStateVideoPending}, state)
}

func (r *Repo) findFrameRun(user uint64, match func(model.DramaSegmentFrameRun) bool) (*model.DramaSegmentFrameRun, error) {
	if r.Err != nil {
		return nil, r.Err
	}
	i := slices.IndexFunc(r.FrameRuns, func(row model.DramaSegmentFrameRun) bool { return row.UserID == user && match(row) })
	if i >= 0 {
		return new(cloneFrameRun(r.FrameRuns[i])), nil
	}
	return nil, repository.ErrNotFound
}

// FrameRun 返回用户流程副本，缺失返回 ErrNotFound。
func (r *Repo) FrameRun(_ context.Context, user, id uint64, _ bool) (*model.DramaSegmentFrameRun, error) {
	return r.findFrameRun(user, func(row model.DramaSegmentFrameRun) bool { return row.ID == id })
}

// ActiveFrameRun 返回用户片段的活跃流程，缺失返回 ErrNotFound。
func (r *Repo) ActiveFrameRun(_ context.Context, user, segment uint64, _ bool) (*model.DramaSegmentFrameRun, error) {
	return r.findFrameRun(user, func(row model.DramaSegmentFrameRun) bool {
		return row.SegmentID == segment && activeFrameRun(row.State)
	})
}

// FrameRunByRequestKey 按用户和非空请求键查询。
func (r *Repo) FrameRunByRequestKey(_ context.Context, user uint64, key string) (*model.DramaSegmentFrameRun, error) {
	return r.findFrameRun(user, func(row model.DramaSegmentFrameRun) bool { return key != "" && row.RequestKey == key })
}

// FrameRunByFrameTask 按用户和首帧任务查询。
func (r *Repo) FrameRunByFrameTask(_ context.Context, user, task uint64) (*model.DramaSegmentFrameRun, error) {
	return r.findFrameRun(user, func(row model.DramaSegmentFrameRun) bool {
		return task != 0 && row.FrameTaskID != nil && *row.FrameTaskID == task
	})
}

// FrameRunByVideoTask 按用户和视频任务查询。
func (r *Repo) FrameRunByVideoTask(_ context.Context, user, task uint64) (*model.DramaSegmentFrameRun, error) {
	return r.findFrameRun(user, func(row model.DramaSegmentFrameRun) bool {
		return task != 0 && row.VideoTaskID != nil && *row.VideoTaskID == task
	})
}

// FrameRunByVideoIdempotencyKey 按用户和非空视频幂等键查询。
func (r *Repo) FrameRunByVideoIdempotencyKey(_ context.Context, user uint64, key string) (*model.DramaSegmentFrameRun, error) {
	return r.findFrameRun(user, func(row model.DramaSegmentFrameRun) bool { return key != "" && row.VideoIdempotencyKey == key })
}

func (r *Repo) frameRunUnique(row *model.DramaSegmentFrameRun, except uint64) error {
	for _, old := range r.FrameRuns {
		if old.ID != except && (old.ID == row.ID || old.UserID == row.UserID && old.RequestKey == row.RequestKey ||
			old.SegmentID == row.SegmentID && activeFrameRun(old.State) && activeFrameRun(row.State) ||
			old.VideoTaskID != nil && row.VideoTaskID != nil && *old.VideoTaskID == *row.VideoTaskID) {
			return repository.ErrDuplicate
		}
	}
	return r.Err
}

// CreateFrameRun 插入流程并模拟默认值、请求键及活跃槽位唯一约束。
func (r *Repo) CreateFrameRun(_ context.Context, row *model.DramaSegmentFrameRun) error {
	if err := r.frameRunUnique(row, 0); err != nil {
		return err
	}
	if row.ID == 0 {
		for _, old := range r.FrameRuns {
			row.ID = max(row.ID, old.ID)
		}
		row.ID++
	}
	if row.Revision == 0 {
		row.Revision = 1
	}
	if len(row.ConfirmedInput) == 0 {
		row.ConfirmedInput = []byte(`{}`)
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now()
	}
	if row.UpdatedAt.IsZero() {
		row.UpdatedAt = row.CreatedAt
	}
	r.FrameRuns = append(r.FrameRuns, cloneFrameRun(*row))
	return nil
}

// SaveFrameRun 按用户、片段、ID 和 revision 执行 CAS，保留不可变的请求身份。
func (r *Repo) SaveFrameRun(_ context.Context, row *model.DramaSegmentFrameRun, revision int64) error {
	for i, old := range r.FrameRuns {
		if old.ID == row.ID && old.UserID == row.UserID && old.SegmentID == row.SegmentID && old.Revision == revision {
			stored := cloneFrameRun(*row)
			stored.RequestKey, stored.RequestHash, stored.CreatedAt = old.RequestKey, old.RequestHash, old.CreatedAt
			if err := r.frameRunUnique(&stored, row.ID); err != nil {
				return err
			}
			row.Revision, row.UpdatedAt = revision+1, time.Now()
			stored.Revision, stored.UpdatedAt = row.Revision, row.UpdatedAt
			if len(stored.ConfirmedInput) == 0 {
				stored.ConfirmedInput = []byte(`{}`)
			}
			r.FrameRuns[i] = stored
			return nil
		}
	}
	return repository.ErrStateConflict
}

func (r *Repo) cancelFrameRuns(user uint64, match func(model.DramaSegmentFrameRun) bool) error {
	if r.Err != nil {
		return r.Err
	}
	for i, row := range r.FrameRuns {
		if row.UserID == user && activeFrameRun(row.State) && match(row) {
			r.FrameRuns[i].State, r.FrameRuns[i].Revision, r.FrameRuns[i].UpdatedAt = model.FrameRunStateCancelled, row.Revision+1, time.Now()
		}
	}
	return nil
}

// CancelSegmentFrameRuns 取消指定用户片段的活跃流程。
func (r *Repo) CancelSegmentFrameRuns(_ context.Context, user, segment uint64) error {
	return r.cancelFrameRuns(user, func(row model.DramaSegmentFrameRun) bool { return row.SegmentID == segment })
}

// CancelEpisodeFrameRuns 取消指定用户分集下包括已软删片段的活跃流程。
func (r *Repo) CancelEpisodeFrameRuns(_ context.Context, user, episode uint64) error {
	return r.cancelFrameRuns(user, func(run model.DramaSegmentFrameRun) bool {
		return slices.ContainsFunc(r.Rows, func(row model.DramaSegment) bool {
			return row.UserID == user && row.EpisodeID == episode && row.ID == run.SegmentID
		})
	})
}

// CancelProjectFrameRuns 取消固定测试项目 7 下的活跃流程。
func (r *Repo) CancelProjectFrameRuns(ctx context.Context, user, project uint64) error {
	if project != 7 {
		return r.Err
	}
	return r.CancelEpisodeFrameRuns(ctx, user, 10)
}

// ListRecoverableFrameRuns 按用户、时间和游标筛选可恢复流程，有界并稳定排序。
func (r *Repo) ListRecoverableFrameRuns(_ context.Context, filter repository.FrameRunRecoveryFilter, limit int) ([]model.DramaSegmentFrameRun, error) {
	rows := selectFakeRows(r.FrameRuns, func(row model.DramaSegmentFrameRun) bool {
		recoverable := row.State == model.FrameRunStateFramePending ||
			row.State == model.FrameRunStateSubmitting ||
			row.State == model.FrameRunStateVideoPending ||
			(row.FrameSource == model.FrameSourceGenerate && row.State == model.FrameRunStateDraft)
		if !recoverable {
			return false
		}
		if filter.UserID != 0 && row.UserID != filter.UserID {
			return false
		}
		if !filter.UpdatedBefore.IsZero() && row.UpdatedAt.After(filter.UpdatedBefore) {
			return false
		}
		if !filter.AfterUpdatedAt.IsZero() && filter.AfterID > 0 {
			if row.UpdatedAt.Before(filter.AfterUpdatedAt) || (row.UpdatedAt.Equal(filter.AfterUpdatedAt) && row.ID <= filter.AfterID) {
				return false
			}
		}
		return true
	}, cloneFrameRun)
	slices.SortFunc(rows, func(a, b model.DramaSegmentFrameRun) int {
		return cmp.Or(a.UpdatedAt.Compare(b.UpdatedAt), cmp.Compare(a.ID, b.ID))
	})
	if limit <= 0 {
		limit = 50
	}
	return rows[:min(len(rows), limit, 500)], r.Err
}

// HasActiveFrameRunForTask 与真实仓储一致，阻止直接取消接口绕过活跃流程取消关联任务。
func (r *Repo) HasActiveFrameRunForTask(_ context.Context, user uint64, task *model.GenerationTask) (bool, error) {
	if r.Err != nil {
		return false, r.Err
	}
	if task == nil || task.ID == 0 || user == 0 {
		return false, nil
	}
	var draftSID uint64
	if task.Kind == model.KindImage && strings.HasPrefix(task.NodeID, "seg-frame:") {
		if sid, ok := idcodec.Decode(strings.TrimPrefix(task.NodeID, "seg-frame:")); ok {
			draftSID = sid
		}
	}
	return slices.ContainsFunc(r.FrameRuns, func(run model.DramaSegmentFrameRun) bool {
		if run.UserID != user || run.State == model.FrameRunStateBound || run.State == model.FrameRunStateFailed || run.State == model.FrameRunStateCancelled {
			return false
		}
		return (task.FrameRunID != nil && *task.FrameRunID == run.ID) ||
			(run.FrameTaskID != nil && *run.FrameTaskID == task.ID) ||
			(run.VideoTaskID != nil && *run.VideoTaskID == task.ID) ||
			(draftSID != 0 && run.SegmentID == draftSID && run.FrameSource == model.FrameSourceGenerate && run.State == model.FrameRunStateDraft && run.FrameTaskID == nil)
	}), nil
}

// ListTakeLinks 返回用户片段的候选副本，按任务 ID 排序。
func (r *Repo) ListTakeLinks(_ context.Context, user, segment uint64) ([]model.DramaSegmentTakeLink, error) {
	rows := selectFakeRows(r.TakeLinks, func(row model.DramaSegmentTakeLink) bool { return row.UserID == user && row.SegmentID == segment }, cloneTakeLink)
	slices.SortFunc(rows, func(a, b model.DramaSegmentTakeLink) int { return cmp.Compare(a.TaskID, b.TaskID) })
	return rows, r.Err
}

func (r *Repo) findTakeLink(user uint64, match func(model.DramaSegmentTakeLink) bool) (*model.DramaSegmentTakeLink, error) {
	if r.Err != nil {
		return nil, r.Err
	}
	i := slices.IndexFunc(r.TakeLinks, func(row model.DramaSegmentTakeLink) bool { return row.UserID == user && match(row) })
	if i >= 0 {
		return new(cloneTakeLink(r.TakeLinks[i])), nil
	}
	return nil, repository.ErrNotFound
}

// TakeLink 按用户和复合主键查询候选副本。
func (r *Repo) TakeLink(_ context.Context, user, segment, task uint64) (*model.DramaSegmentTakeLink, error) {
	return r.findTakeLink(user, func(row model.DramaSegmentTakeLink) bool { return row.SegmentID == segment && row.TaskID == task })
}

// TakeLinkByRun 按用户和非零 run_id 查询候选副本。
func (r *Repo) TakeLinkByRun(_ context.Context, user, run uint64) (*model.DramaSegmentTakeLink, error) {
	return r.findTakeLink(user, func(row model.DramaSegmentTakeLink) bool { return run != 0 && row.RunID != nil && *row.RunID == run })
}

// CreateTakeLinkIfAbsent 只补缺复合主键，绝不重写来源或质检；run_id 冲突返回 ErrDuplicate。
func (r *Repo) CreateTakeLinkIfAbsent(_ context.Context, row *model.DramaSegmentTakeLink) (bool, error) {
	if r.Err != nil {
		return false, r.Err
	}
	if slices.ContainsFunc(r.TakeLinks, func(old model.DramaSegmentTakeLink) bool {
		return old.SegmentID == row.SegmentID && old.TaskID == row.TaskID
	}) {
		return false, nil
	}
	if slices.ContainsFunc(r.TakeLinks, func(old model.DramaSegmentTakeLink) bool {
		return old.RunID != nil && row.RunID != nil && *old.RunID == *row.RunID
	}) {
		return false, repository.ErrDuplicate
	}
	if row.QC == "" {
		row.QC = model.TakeQCUnreviewed
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now()
	}
	stored := *row
	stored.RunID = clonePointer(row.RunID)
	r.TakeLinks = append(r.TakeLinks, stored)
	return true, nil
}

// UpdateTakeLinkQC 只更新用户候选的质检结果，不修改来源。
func (r *Repo) UpdateTakeLinkQC(_ context.Context, user, segment, task uint64, qc model.DramaSegmentTakeQC) error {
	if r.Err != nil {
		return r.Err
	}
	for i, row := range r.TakeLinks {
		if row.UserID == user && row.SegmentID == segment && row.TaskID == task {
			r.TakeLinks[i].QC = qc
			return nil
		}
	}
	return repository.ErrNotFound
}

// TasksByIDs 返回用户正式任务，排除他人和试跑。
func (r *Repo) TasksByIDs(_ context.Context, user uint64, ids []uint64) ([]model.GenerationTask, error) {
	rows := selectFakeRows(r.Tasks, func(task model.GenerationTask) bool {
		return task.UserID == user && !task.IsTest && slices.Contains(ids, task.ID)
	}, nil)
	slices.SortFunc(rows, func(a, b model.GenerationTask) int { return cmp.Compare(a.ID, b.ID) })
	return rows, r.Err
}

// FrameRunsBySegment 按 ID 返回本用户片段的全部历史流程。
func (r *Repo) FrameRunsBySegment(_ context.Context, user, sid uint64) ([]model.DramaSegmentFrameRun, error) {
	rows := selectFakeRows(r.FrameRuns, func(run model.DramaSegmentFrameRun) bool { return run.UserID == user && run.SegmentID == sid }, cloneFrameRun)
	slices.SortFunc(rows, func(a, b model.DramaSegmentFrameRun) int { return cmp.Compare(a.ID, b.ID) })
	return rows, r.Err
}

// ActiveFrameRunsBySegments 返回指定存活段的媒体进行态。
func (r *Repo) ActiveFrameRunsBySegments(_ context.Context, user uint64, ids []uint64) ([]model.DramaSegmentFrameRun, error) {
	rows := selectFakeRows(r.FrameRuns, func(run model.DramaSegmentFrameRun) bool {
		return run.UserID == user && slices.Contains(ids, run.SegmentID) && slices.Contains([]model.DramaFrameRunState{model.FrameRunStateFramePending, model.FrameRunStateSubmitting, model.FrameRunStateVideoPending}, run.State)
	}, cloneFrameRun)
	return rows, r.Err
}

// TakeLinksByTask 反查同用户任务被导入的片段。
func (r *Repo) TakeLinksByTask(_ context.Context, user, task uint64) ([]model.DramaSegmentTakeLink, error) {
	rows := selectFakeRows(r.TakeLinks, func(row model.DramaSegmentTakeLink) bool { return row.UserID == user && row.TaskID == task }, cloneTakeLink)
	return rows, r.Err
}
