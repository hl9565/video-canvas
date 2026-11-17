package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"slices"
	"strings"
	"time"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/jsonhash"
	"video-canvas/internal/pkg/ws"
	"video-canvas/internal/provider"
	"video-canvas/internal/repository"
)

// DefaultDramaI2VModelKey 是首帧控镜默认且唯一允许确认的视频模型。
const DefaultDramaI2VModelKey = "m-1c89decdf8fd"

var errFrameRunUnchanged = errors.New("frame run unchanged")

// DramaFrameRunRepo 提供父链归属、事务和事务外恢复读取。
type DramaFrameRunRepo interface {
	DramaSegmentRepo
	// FrameRun 按用户读取流程，锁必须在父链锁之后取得。
	FrameRun(context.Context, uint64, uint64, bool) (*model.DramaSegmentFrameRun, error)
	// FrameRunByRequestKey 按用户恢复创建请求。
	FrameRunByRequestKey(context.Context, uint64, string) (*model.DramaSegmentFrameRun, error)
	// FrameRunByFrameTask 按用户查找首帧图任务所属流程。
	FrameRunByFrameTask(context.Context, uint64, uint64) (*model.DramaSegmentFrameRun, error)
	// ListRecoverableFrameRuns 有界列出三种待恢复状态。
	ListRecoverableFrameRuns(context.Context, repository.FrameRunRecoveryFilter, int) ([]model.DramaSegmentFrameRun, error)
}

// DramaFrameTasks 只暴露首帧流程所需的任务提交、验价及取消能力。
type DramaFrameTasks interface {
	// Create 幂等创建首帧图片任务，不绑定视频片段节点。
	Create(context.Context, uint64, string, *model.CreateGenerationTaskReq) (*model.CreateGenerationTaskResp, error)
	// CreateWithOptions 原子创建、冻结积分并双向绑定视频任务。
	CreateWithOptions(context.Context, uint64, string, *model.CreateGenerationTaskReq, CreateTaskOptions) (*model.CreateGenerationTaskResp, error)
	// Cancel 尽力取消尚未完成的正式任务。
	Cancel(context.Context, uint64, uint64) (*model.GenerationTaskView, error)
	// Quote 计算绑定片段版本的报价。
	Quote(context.Context, uint64, *model.QuoteGenerationTaskReq, ...QuoteTaskOptions) (*model.TaskQuoteView, error)
	// VerifyQuoteToken 在确认前校验实时单镜报价。
	VerifyQuoteToken(context.Context, uint64, *model.QuoteGenerationTaskReq, string, ...QuoteTaskOptions) (*model.TaskQuoteView, error)
	// FindByIdempotencyKey 只读任务证据，缺失返回 repository.ErrNotFound。
	FindByIdempotencyKey(context.Context, uint64, string) (*model.GenerationTask, error)
}

// DramaFrameRegistry 提供当前模型能力快照。
type DramaFrameRegistry interface {
	// Snapshot 查询当前可用的模型及插件快照。
	Snapshot(context.Context, string) (*provider.Snapshot, error)
}

// DramaFrameAssetStore 提供用户素材读取及事务外派生图片入库。
type DramaFrameAssetStore interface {
	provider.AssetStore
	DramaSegmentAssetReader
	// SaveDerivedImage 保存裁图并返回包含哈希、尺寸的完整素材行。
	SaveDerivedImage(context.Context, uint64, string, string, []byte) (*model.Asset, error)
}

// DramaFrameRunDeps 是首帧控镜的持久化、任务、素材和事件依赖。
type DramaFrameRunDeps struct {
	Repo         DramaFrameRunRepo
	Canvases     DramaStoryboardCanvasReader
	Tasks        DramaFrameTasks
	Registry     DramaFrameRegistry
	Assets       DramaFrameAssetStore
	Segments     *DramaSegmentService
	Broadcaster  ws.Broadcaster
	TicketSecret string
	I2VModelKey  string
}

// DramaFrameRunOption 替换时间和恢复超时，便于确定性验证票据与断线恢复。
type DramaFrameRunOption func(*DramaFrameRunService)

// WithDramaFrameRunClock 替换票据和提交超时使用的时钟。
func WithDramaFrameRunClock(now func() time.Time) DramaFrameRunOption {
	return func(s *DramaFrameRunService) { s.now = now }
}

// WithDramaFrameRunStaleTimeout 设置无任务的 submitting 恢复等待时间。
func WithDramaFrameRunStaleTimeout(timeout time.Duration) DramaFrameRunOption {
	return func(s *DramaFrameRunService) { s.staleTimeout = timeout }
}

// DramaFrameRunService 管理首帧选择、确认、视频提交与可恢复的任务观察。
type DramaFrameRunService struct {
	repo         DramaFrameRunRepo
	canvases     DramaStoryboardCanvasReader
	tasks        DramaFrameTasks
	registry     DramaFrameRegistry
	assets       DramaFrameAssetStore
	segments     *DramaSegmentService
	bc           ws.Broadcaster
	ticketSecret []byte
	i2vModelKey  string
	now          func() time.Time
	staleTimeout time.Duration
}

// NewDramaFrameRunService 创建首帧流程；未配置的签名密钥使用随机值，不能跨用途重放。
func NewDramaFrameRunService(d DramaFrameRunDeps, opts ...DramaFrameRunOption) *DramaFrameRunService {
	key := make([]byte, sha256.Size)
	if d.TicketSecret == "" {
		if _, err := rand.Read(key); err != nil {
			panic("无法生成首帧画布签名密钥")
		}
	} else {
		sum := sha256.Sum256([]byte("vc-drama-frame-canvas-v1:" + d.TicketSecret))
		copy(key, sum[:])
	}
	if d.I2VModelKey == "" {
		d.I2VModelKey = DefaultDramaI2VModelKey
	}
	if d.Broadcaster == nil {
		d.Broadcaster = ws.NopBroadcaster{}
	}
	if d.Segments == nil {
		d.Segments = NewDramaSegmentService(DramaSegmentDeps{Repo: d.Repo, Assets: d.Assets, Canvases: d.Canvases, Broadcaster: d.Broadcaster})
	}
	s := &DramaFrameRunService{repo: d.Repo, canvases: d.Canvases, tasks: d.Tasks, registry: d.Registry, assets: d.Assets, segments: d.Segments,
		bc: d.Broadcaster, ticketSecret: key, i2vModelKey: d.I2VModelKey, now: time.Now, staleTimeout: 2 * time.Minute}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Create 按用户请求键创建流程；返回的 bool 仅在首次插入时为 true。
func (s *DramaFrameRunService) Create(ctx context.Context, userID, sid uint64, req *model.CreateFrameRunReq) (*model.DramaSegmentFrameRunView, bool, error) {
	// 1. 规范化意图并优先重放，避免重复裁图或外部任务调用。
	normalized, hash, err := normalizeFrameRunRequest(req)
	if err != nil {
		return nil, false, err
	}
	run, err := s.repo.FrameRunByRequestKey(ctx, userID, normalized.RequestKey)
	if err == nil {
		return s.replayFrameRun(ctx, userID, sid, run, normalized, hash)
	}
	if !errors.Is(err, repository.ErrNotFound) {
		return nil, false, err
	}
	p, _, _, err := s.segments.segmentSource(ctx, userID, sid)
	if err != nil {
		return nil, false, err
	}
	run, err = s.prepareNewFrameRun(ctx, userID, sid, p, normalized, hash)
	if err != nil {
		return nil, false, err
	}
	// 2. 只有数据库变更处于锁内；裁图、任务创建和通知均在事务外。
	err = s.repo.WithTx(ctx, func(tx repository.DramaTx) error { return s.insertFrameRun(ctx, tx, run, normalized) })
	if errors.Is(err, repository.ErrDuplicate) {
		winner, readErr := s.repo.FrameRunByRequestKey(ctx, userID, normalized.RequestKey)
		if readErr == nil {
			return s.replayFrameRun(ctx, userID, sid, winner, normalized, hash)
		}
		if !errors.Is(readErr, repository.ErrNotFound) {
			return nil, false, readErr
		}
		return nil, false, errcode.ErrDramaFrameRunConflict
	}
	if err != nil {
		return nil, false, segmentError(err)
	}
	if err := s.finishFrameCreation(ctx, run, normalized); err != nil {
		return nil, true, err
	}
	v, err := s.changedFrameRunView(ctx, run)
	return v, true, err
}

// normalizeFrameRunRequest 用规范 JSON 哈希绑定完整创建意图，不修改调用方的请求。
func normalizeFrameRunRequest(req *model.CreateFrameRunReq) (*model.CreateFrameRunReq, string, error) {
	if req == nil {
		return nil, "", errcode.ErrInvalidParams
	}
	copyReq := *req
	copyReq.RequestKey = strings.TrimSpace(req.RequestKey)
	if copyReq.RequestKey == "" || len(copyReq.RequestKey) > 96 || copyReq.SegmentRevision < 1 {
		return nil, "", errcode.ErrInvalidParams
	}
	hash, err := jsonhash.CanonicalSHA256(copyReq)
	if err != nil {
		return nil, "", errcode.ErrInvalidParams
	}
	return &copyReq, hash, nil
}

// prepareNewFrameRun 在持锁前完成来源验证及必要的图片写入。
func (s *DramaFrameRunService) prepareNewFrameRun(ctx context.Context, userID, sid uint64, p *model.DramaProject, req *model.CreateFrameRunReq, hash string) (*model.DramaSegmentFrameRun, error) {
	run := &model.DramaSegmentFrameRun{UserID: userID, SegmentID: sid, RequestKey: req.RequestKey, RequestHash: hash, FrameSource: req.FrameSource,
		State: model.FrameRunStateDraft, SegmentRevision: req.SegmentRevision, Revision: 1, AdoptIfEmpty: req.AdoptIfEmpty, ConfirmedInput: []byte(`{}`)}
	switch req.FrameSource {
	case model.FrameSourceExisting:
		if req.FrameAssetID == nil || *req.FrameAssetID == 0 {
			return nil, errcode.ErrDramaFrameInvalid
		}
		asset, err := s.prepareFrameImage(ctx, userID, uint64(*req.FrameAssetID))
		if err != nil {
			return nil, err
		}
		run.FrameAssetID, run.State = &asset.ID, model.FrameRunStateFrameReady
	case model.FrameSourceGenerate:
		prompt, _ := req.Input["prompt"].(string)
		if req.ModelID == "" || strings.TrimSpace(prompt) == "" {
			return nil, errcode.ErrInvalidParams
		}
	case model.FrameSourceCanvas:
		canvasID := p.CanvasID
		if req.OriginCanvasID != nil {
			canvasID = uint64(*req.OriginCanvasID)
			if err := s.checkFrameCanvas(ctx, userID, canvasID); err != nil {
				return nil, err
			}
		}
		run.OriginCanvasID = &canvasID
	default:
		return nil, errcode.ErrInvalidParams
	}
	return run, nil
}

// insertFrameRun 严格按父链再流程锁检查互斥，用户请求键竞争在事务外重读胜者。
func (s *DramaFrameRunService) insertFrameRun(ctx context.Context, tx repository.DramaTx, run *model.DramaSegmentFrameRun, req *model.CreateFrameRunReq) error {
	p, ep, seg, err := lockSegment(ctx, tx, run.UserID, run.SegmentID)
	if err != nil {
		return err
	}
	if _, err := tx.FrameRunByRequestKey(ctx, run.UserID, run.RequestKey); err == nil {
		return repository.ErrDuplicate
	} else if !errors.Is(err, repository.ErrNotFound) {
		return err
	}
	if seg.Revision != req.SegmentRevision {
		return errcode.ErrCanvasConflict
	}
	if _, err := tx.ActiveFrameRun(ctx, run.UserID, seg.ID, true); err == nil {
		return errcode.ErrDramaFrameRunConflict
	} else if !errors.Is(err, repository.ErrNotFound) {
		return err
	}
	if err := checkFrameRunMedia(ctx, tx, p, ep, ""); err != nil {
		return err
	}
	return tx.CreateFrameRun(ctx, run)
}

// checkFrameRunMedia 复用集级媒体守卫；提交自己的视频键重入时只排除本次流程和任务。
func checkFrameRunMedia(ctx context.Context, tx repository.DramaTx, p *model.DramaProject, ep *model.DramaEpisode, key string) error {
	if ep.LockedAt == nil {
		return errcode.ErrDramaStepNotReady
	}
	jobs, err := tx.Jobs(ctx, p.UserID, p.ID, true)
	if err != nil {
		return err
	}
	if dramaJobsConflict(jobs, &model.DramaJobReq{Step: "segment", Episodes: []int{ep.No}}) {
		return errcode.ErrDramaJobBusy
	}
	return checkDramaMediaTasks(ctx, tx, p, []model.DramaEpisode{*ep}, key)
}

// Get 校验父链及流程归属，在事务外机会式恢复图片、提交和视频状态。
func (s *DramaFrameRunService) Get(ctx context.Context, userID, sid, rid uint64) (*model.DramaSegmentFrameRunView, error) {
	// 1. 恢复读取与后台观察者共用规则，终态不再推进。
	run, err := s.loadFrameRun(ctx, userID, sid, rid)
	if err != nil {
		return nil, err
	}
	if err := s.reconcileFrameRun(ctx, run); err != nil {
		return nil, err
	}
	return s.reloadFrameRunView(ctx, userID, sid, rid)
}

// SetFrame 带流程 CAS 更换首帧并使旧确认失效；确认版本只增不减。
func (s *DramaFrameRunService) SetFrame(ctx context.Context, userID, sid, rid uint64, req *model.SetFrameRunFrameReq) (*model.DramaSegmentFrameRunView, error) {
	// 1. 先验证状态和画布票据，再在锁外加载及裁图。
	if req == nil || req.Revision < 1 || req.FrameAssetID == 0 {
		return nil, errcode.ErrInvalidParams
	}
	run, err := s.loadFrameRun(ctx, userID, sid, rid)
	if err != nil {
		return nil, err
	}
	if !frameRunEditable(run.State) {
		return nil, errcode.ErrDramaFrameRunConflict
	}
	if err := s.verifyFrameCanvas(ctx, run, req); err != nil {
		return nil, err
	}
	asset, err := s.prepareFrameImage(ctx, userID, uint64(req.FrameAssetID))
	if err != nil {
		return nil, err
	}
	// 2. 重新锁定快照，拒绝过期版本和裁图期间发生的提交或取消。
	run, err = s.updateFrameRun(ctx, userID, sid, rid, func(row *model.DramaSegmentFrameRun) error {
		if row.Revision != req.Revision {
			return errcode.ErrCanvasConflict
		}
		if !frameRunEditable(row.State) {
			return errcode.ErrDramaFrameRunConflict
		}
		row.FrameAssetID, row.State = &asset.ID, model.FrameRunStateFrameReady
		clearFrameConfirmation(row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.changedFrameRunView(ctx, run)
}

// frameRunEditable 排除提交中、视频进行态和所有终态。
func frameRunEditable(state model.DramaFrameRunState) bool {
	return slices.Contains([]model.DramaFrameRunState{model.FrameRunStateDraft, model.FrameRunStateFramePending, model.FrameRunStateFrameReady, model.FrameRunStateConfirmed}, state)
}

// clearFrameConfirmation 保留单调确认版本，避免换图后的内部任务键与旧任务碰撞。
func clearFrameConfirmation(run *model.DramaSegmentFrameRun) {
	run.ConfirmedInput, run.ConfirmedInputHash, run.QuoteToken = []byte(`{}`), nil, nil
	run.ModelKey, run.VideoIdempotencyKey = "", ""
	run.SubmittingAt, run.ErrorCode, run.ErrorMessage = nil, "", ""
}

// Cancel 先提交取消状态，再在锁外取消明确属于本流程的任务，迟到产物永远不能重新绑定。
func (s *DramaFrameRunService) Cancel(ctx context.Context, userID, sid, rid uint64, req *model.CancelFrameRunReq) (*model.DramaSegmentFrameRunView, error) {
	// 1. 终态和版本都在父链、流程锁内校验，重复取消也必须匹配当前 revision。
	if req == nil || req.Revision < 1 {
		return nil, errcode.ErrInvalidParams
	}
	var previous model.DramaFrameRunState
	run, err := s.updateFrameRun(ctx, userID, sid, rid, func(row *model.DramaSegmentFrameRun) error {
		if row.State == model.FrameRunStateBound || row.State == model.FrameRunStateFailed {
			return errcode.ErrDramaFrameRunConflict
		}
		if row.Revision != req.Revision {
			return errcode.ErrCanvasConflict
		}
		if row.State == model.FrameRunStateCancelled {
			return errFrameRunUnchanged
		}
		previous = row.State
		row.State, row.SubmittingAt = model.FrameRunStateCancelled, nil
		return nil
	})
	if err != nil {
		return nil, err
	}
	// 2. 取消任务会触发观察者并重入父链锁，因此绝不能在事务内调用。
	s.cancelFrameRunTasks(ctx, run, previous)
	return s.changedFrameRunView(ctx, run)
}

// loadFrameRun 先验证存活父链，再读取流程，隐藏跨片段和跨用户流程。
func (s *DramaFrameRunService) loadFrameRun(ctx context.Context, userID, sid, rid uint64) (*model.DramaSegmentFrameRun, error) {
	if _, _, _, err := s.segments.segmentSource(ctx, userID, sid); err != nil {
		return nil, err
	}
	run, err := s.repo.FrameRun(ctx, userID, rid, false)
	return ownedFrameRun(run, sid, err)
}

// ownedFrameRun 将流程缺失及不匹配归属统一为流程不存在。
func ownedFrameRun(run *model.DramaSegmentFrameRun, sid uint64, err error) (*model.DramaSegmentFrameRun, error) {
	if errors.Is(err, repository.ErrNotFound) || err == nil && (run == nil || run.SegmentID != sid) {
		return nil, errcode.ErrDramaFrameRunNotFound
	}
	return run, err
}

// updateFrameRun 统一父链→流程锁与 CAS；无变化的恢复或幂等取消不递增版本。
func (s *DramaFrameRunService) updateFrameRun(ctx context.Context, userID, sid, rid uint64, mutate func(*model.DramaSegmentFrameRun) error) (*model.DramaSegmentFrameRun, error) {
	var run *model.DramaSegmentFrameRun
	err := s.repo.WithTx(ctx, func(tx repository.DramaTx) error {
		if _, _, _, err := lockSegment(ctx, tx, userID, sid); err != nil {
			return err
		}
		var err error
		run, err = tx.FrameRun(ctx, userID, rid, true)
		run, err = ownedFrameRun(run, sid, err)
		if err != nil {
			return err
		}
		revision := run.Revision
		err = mutate(run)
		if errors.Is(err, errFrameRunUnchanged) {
			return nil
		}
		if err != nil {
			return err
		}
		return tx.SaveFrameRun(ctx, run, revision)
	})
	return run, segmentError(err)
}
