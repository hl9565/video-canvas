package service

import (
	"context"
	"errors"
	"sync"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/pkg/ws"
	"video-canvas/internal/repository"
)

// DramaSegmentRepo 片段归属查询和事务边界。
type DramaSegmentRepo interface {
	DramaTakeReader
	// WithTx 原子执行片段变更，锁顺序为项目后片段。
	WithTx(context.Context, func(repository.DramaTx) error) error
	// WithSnapshotTx 在只读可重复读事务中组装所有视图输入。
	WithSnapshotTx(context.Context, func(repository.DramaTx) error) error
	// Project 校验项目归属和存活状态。
	Project(context.Context, uint64, uint64, bool) (*model.DramaProject, error)
	// Episodes 返回项目分集。
	Episodes(context.Context, uint64, uint64) ([]model.DramaEpisode, error)
	// SegmentEpisode 按集ID反查存活项目下的分集。
	SegmentEpisode(context.Context, uint64, uint64) (*model.DramaEpisode, error)
	// SegmentTasks 返回本用户画布节点的正式任务，供事务外探测使用。
	SegmentTasks(context.Context, uint64, uint64, string) ([]model.GenerationTask, error)
	// Segment 查询用户片段，缺失返回ErrNotFound。
	Segment(context.Context, uint64, uint64, bool) (*model.DramaSegment, error)
	// Segments 返回按seq排序的用户片段。
	Segments(context.Context, uint64, uint64) ([]model.DramaSegment, error)
}

// DramaSegmentTaskCreator 直接复用现有生成任务提交逻辑。
type DramaSegmentTaskCreator interface {
	// Quote 返回绑定片段版本的单镜报价。
	Quote(context.Context, uint64, *model.QuoteGenerationTaskReq, ...QuoteTaskOptions) (*model.TaskQuoteView, error)
	// CheckIdempotentReplay 在状态校验前核验已有任务的原始意图。
	CheckIdempotentReplay(context.Context, uint64, string, *model.CreateGenerationTaskReq) (*model.GenerationTaskView, error)
	// Create 校验生成参数、冻结积分并返回逐项创建结果。
	Create(context.Context, uint64, string, *model.CreateGenerationTaskReq) (*model.CreateGenerationTaskResp, error)
}

// DramaSegmentAssetReader 读取用户素材视图，复用AssetService。
type DramaSegmentAssetReader interface {
	// View 返回用户拥有的素材及稳定URL。
	View(context.Context, uint64, uint64) (*model.AssetView, error)
}

// DramaFrameRunEnricher 在事务外补齐首帧内容指纹和短期画布票据。
type DramaFrameRunEnricher interface {
	// EnrichFrameRunView 为 HTTP、片段列表及 WS 使用同一套流程视图。
	EnrichFrameRunView(context.Context, uint64, *model.DramaSegmentFrameRun, *model.DramaSegmentFrameRunView) error
}

// DramaSegmentProbeStore 读写按素材内容指纹标识的媒体探测缓存。
type DramaSegmentProbeStore interface {
	// Get 按素材、哈希和版本精确读取缓存，缺失返回 repository.ErrNotFound。
	Get(context.Context, uint64, string, int16) (*model.AssetMediaProbe, error)
	// Upsert 幂等保存素材媒体探测结果。
	Upsert(context.Context, *model.AssetMediaProbe) error
}

// DramaSegmentDeps 提供片段的持久化、生成、素材与用户事件依赖。
type DramaSegmentDeps struct {
	Canvases DramaStoryboardCanvasReader
	Probes   DramaSegmentProbeStore
	// ProbeVideo 在事务外探测本地视频的规范帧数；未注入时使用默认探测器。
	ProbeVideo  func(context.Context, string) (videoframe.NormalizedProbe, error)
	Repo        DramaSegmentRepo
	Tasks       DramaSegmentTaskCreator
	Assets      DramaSegmentAssetReader
	Broadcaster ws.Broadcaster
	// Adapter 转写原稿；为 nil 时转写返回「没有可用的 Agent 模型」
	Adapter DramaSegmentAdapter
}

// DramaSegmentService 管理片段编辑、真实任务结果与自动采用。
type DramaSegmentService struct {
	probes     DramaSegmentProbeStore
	probeVideo func(context.Context, string) (videoframe.NormalizedProbe, error)
	canvases   DramaStoryboardCanvasReader
	repo       DramaSegmentRepo
	tasks      DramaSegmentTaskCreator
	assets     DramaSegmentAssetReader
	bc         ws.Broadcaster
	adapter    DramaSegmentAdapter
	frameRuns  DramaFrameRunEnricher
	// adapting 是进程内的转写锁，键为「片段:格式」
	adapting sync.Map
}

// SetFrameRunEnricher 注入首帧视图补全器，只在初始化时调用。
func (s *DramaSegmentService) SetFrameRunEnricher(enricher DramaFrameRunEnricher) {
	s.frameRuns = enricher
}

// NewDramaSegmentService 创建独立片段服务，任务状态观察者由initialize组装。
func NewDramaSegmentService(d DramaSegmentDeps) *DramaSegmentService {
	bc := d.Broadcaster
	if bc == nil {
		bc = ws.NopBroadcaster{}
	}
	probe := d.ProbeVideo
	if probe == nil {
		probe = videoframe.New(videoframe.Options{}).ProbeNormalized24FPS
	}
	return &DramaSegmentService{probes: d.Probes, probeVideo: probe, repo: d.Repo, canvases: d.Canvases, tasks: d.Tasks, assets: d.Assets, bc: bc, adapter: d.Adapter}
}

// List 校验用户项目后按顺序返回片段，并恢复漏处理的成功任务写回。
func (s *DramaSegmentService) List(ctx context.Context, userID, dramaID uint64, no int) ([]model.DramaSegmentView, error) {
	_, items, err := s.ListEpisode(ctx, userID, dramaID, no)
	return items, err
}

// ListEpisode 从一个只读快照返回集身份、版本、成员、候选和流程；只为待恢复片段加锁。
func (s *DramaSegmentService) ListEpisode(ctx context.Context, userID, dramaID uint64, no int) (model.DramaEpisode, []model.DramaSegmentView, error) {
	// 1. 仅在只读快照发现待推进的流程或待采用的视频时执行恢复。
	p, ep, reads, err := s.readEpisodeSegments(ctx, userID, dramaID, no)
	if err != nil {
		return model.DramaEpisode{}, nil, err
	}
	changed, attempted, err := s.recoverEpisodeSegments(ctx, reads)
	if err != nil {
		return model.DramaEpisode{}, nil, err
	}
	// 2. 恢复后重读整集；不把分别读取的片段拼进同一个响应。
	if attempted {
		p, ep, reads, err = s.readEpisodeSegments(ctx, userID, dramaID, no)
		if err != nil {
			return model.DramaEpisode{}, nil, err
		}
	}
	views, err := s.projectSegmentReads(ctx, &ep, reads)
	if err != nil {
		return model.DramaEpisode{}, nil, err
	}
	for i := range views {
		if changed[uint64(views[i].ID)] {
			s.publishSegment(ctx, p, &ep, &views[i])
		}
	}
	return ep, views, nil
}

func (s *DramaSegmentService) recoverEpisodeSegments(ctx context.Context, reads []segmentRead) (map[uint64]bool, bool, error) {
	changed := make(map[uint64]bool)
	attempted := false
	for i := range reads {
		read := &reads[i]
		if !read.needsRecovery() {
			continue
		}
		attempted = true
		wrote, err := s.reconcileReadSegment(ctx, read.row.UserID, read.row.ID)
		if err != nil && !isSegmentMissing(err) {
			return nil, false, err
		}
		changed[read.row.ID] = wrote
	}
	return changed, attempted, nil
}

// Summary 返回某集片段数、总秒数和已采用数，计数之前恢复任务写回。
func (s *DramaSegmentService) Summary(ctx context.Context, userID, episodeID uint64) (count, seconds, done int, err error) {
	// 1. 使用集主键反查存活项目，沿用列表的归属和任务恢复规则。
	ep, err := s.repo.SegmentEpisode(ctx, userID, episodeID)
	if err != nil {
		return 0, 0, 0, dramaError(err)
	}
	views, err := s.List(ctx, userID, ep.DramaID, ep.No)
	if err != nil {
		return 0, 0, 0, err
	}
	// 2. 已采用按chosen_task_id计数，不因后续失败take减少。
	for _, v := range views {
		seconds += v.Seconds
		if v.ChosenTaskID != "" {
			done++
		}
	}
	return len(views), seconds, done, nil
}

// episode 根据项目ID和集号定位，其他用户和父项目删除统一为短剧不存在。
func (s *DramaSegmentService) episode(ctx context.Context, userID, dramaID uint64, no int) (*model.DramaProject, *model.DramaEpisode, error) {
	return locateEpisode(ctx, s.repo, userID, dramaID, no)
}

type segmentEpisodeLocator interface {
	Project(context.Context, uint64, uint64, bool) (*model.DramaProject, error)
	Episodes(context.Context, uint64, uint64) ([]model.DramaEpisode, error)
}

func locateEpisode(ctx context.Context, reader segmentEpisodeLocator, userID, dramaID uint64, no int) (*model.DramaProject, *model.DramaEpisode, error) {
	p, err := reader.Project(ctx, userID, dramaID, false)
	if err != nil {
		return nil, nil, dramaError(err)
	}
	rows, err := reader.Episodes(ctx, userID, dramaID)
	if err != nil {
		return nil, nil, err
	}
	for i := range rows {
		if rows[i].No == no {
			return p, &rows[i], nil
		}
	}
	return nil, nil, errcode.ErrDramaNotFound
}

// lockSegment 无锁反查父链后按项目→集→片段顺序加锁，并复核持锁父链。
func lockSegment(ctx context.Context, tx repository.DramaTx, userID, sid uint64) (*model.DramaProject, *model.DramaEpisode, *model.DramaSegment, error) {
	row, err := tx.Segment(ctx, userID, sid, false)
	if err != nil {
		return nil, nil, nil, err
	}
	ep, err := tx.SegmentEpisode(ctx, userID, row.EpisodeID)
	if err != nil {
		return nil, nil, nil, err
	}
	p, err := tx.Project(ctx, userID, ep.DramaID, true)
	if err != nil {
		return nil, nil, nil, err
	}
	eps, err := tx.LockEpisodes(ctx, userID, []uint64{ep.ID})
	if err != nil {
		return nil, nil, nil, err
	}
	rows, err := tx.LockSegments(ctx, userID, []uint64{sid})
	if err != nil {
		return nil, nil, nil, err
	}
	if len(eps) != 1 || len(rows) != 1 || eps[0].DramaID != p.ID || rows[0].EpisodeID != eps[0].ID {
		return nil, nil, nil, repository.ErrNotFound
	}
	return p, &eps[0], &rows[0], nil
}

// segmentError 将片段归属和版本哨兵转换为契约错误，不泄漏父项目是否存在。
func segmentError(err error) error {
	if errors.Is(err, repository.ErrNotFound) {
		return errcode.ErrDramaSegmentNotFound
	}
	if errors.Is(err, repository.ErrStateConflict) {
		return errcode.ErrCanvasConflict
	}
	var business *errcode.Error
	if errors.As(err, &business) && business.Code == errcode.ErrDramaNotFound.Code {
		return errcode.ErrDramaSegmentNotFound
	}
	return err
}

// publishSegment 在事务提交后推送完整视图，前端与HTTP共用相同字段。
func (s *DramaSegmentService) publishSegment(ctx context.Context, p *model.DramaProject, ep *model.DramaEpisode, v *model.DramaSegmentView) {
	publishDramaSegment(ctx, s.bc, p, ep, v)
}

// publishDramaSegment 使片段和首帧服务共用用户事件契约。
func publishDramaSegment(ctx context.Context, bc ws.Broadcaster, p *model.DramaProject, ep *model.DramaEpisode, v *model.DramaSegmentView) {
	bc.Publish(ctx, ws.UserChannel(p.UserID), ws.Message{Type: ws.TypeDramaSegment, Data: struct {
		DramaID   idcodec.ID              `json:"drama_id"`
		EpisodeNo int                     `json:"episode_no"`
		Segment   *model.DramaSegmentView `json:"segment"`
	}{idcodec.ID(p.ID), ep.No, v}})
}

// changedView 读取最终状态并通知用户，避免在持有数据库锁时访问素材或WebSocket。
func (s *DramaSegmentService) changedView(ctx context.Context, userID, sid uint64) (*model.DramaSegmentView, error) {
	v, p, ep, _, err := s.readView(ctx, userID, sid)
	if err == nil {
		s.publishSegment(ctx, p, ep, v)
	}
	return v, err
}

// EnrichFrameRunView 在事务外补齐内容指纹和非终态画布票据，历史素材缺失不阻止取消与查看。
func (s *DramaFrameRunService) EnrichFrameRunView(ctx context.Context, userID uint64, run *model.DramaSegmentFrameRun, view *model.DramaSegmentFrameRunView) error {
	// 1. 指纹来自实际素材行，不使用客户端或生成任务的缓存信息。
	if run == nil || view == nil || run.UserID != userID {
		return errcode.ErrDramaFrameRunNotFound
	}
	var asset *model.Asset
	if run.FrameAssetID != nil {
		var err error
		asset, err = s.assets.Get(ctx, userID, *run.FrameAssetID)
		if err != nil && !isFrameInvalid(frameAssetError(err)) {
			return err
		}
	}
	s.projectFrameRunDetails(run, asset, view)
	return nil
}

// projectFrameRunDetails 使用调用方已读取的素材元数据，签名只在本地进行。
func (s *DramaFrameRunService) projectFrameRunDetails(run *model.DramaSegmentFrameRun, asset *model.Asset, view *model.DramaSegmentFrameRunView) {
	view.FrameSHA256, view.CanvasTicket = "", ""
	if run.FrameAssetID != nil && asset != nil && asset.ID == *run.FrameAssetID && asset.UserID == run.UserID && asset.Kind == model.KindImage {
		view.FrameSHA256 = asset.Sha256
	}
	if run.FrameSource == model.FrameSourceCanvas && run.OriginCanvasID != nil && !terminalFrameRun(run.State) {
		view.CanvasTicket = s.signFrameCanvasTicket(run)
	}
}

// terminalFrameRun 终态释放唯一活跃槽位，并禁止继续更新首帧。
func terminalFrameRun(state model.DramaFrameRunState) bool {
	return state == model.FrameRunStateBound || state == model.FrameRunStateFailed || state == model.FrameRunStateCancelled
}

// isFrameInvalid 只将素材缺失、类型和内容错误视为可展示的历史失效。
func isFrameInvalid(err error) bool {
	var business *errcode.Error
	return errors.As(err, &business) && business.Code == errcode.ErrDramaFrameInvalid.Code
}

// reloadFrameRunView 从最终持久化状态生成视图，避免任务观察者已推进状态时返回旧版本。
func (s *DramaFrameRunService) reloadFrameRunView(ctx context.Context, userID, sid, rid uint64) (*model.DramaSegmentFrameRunView, error) {
	run, err := s.loadFrameRun(ctx, userID, sid, rid)
	if err != nil {
		return nil, err
	}
	view := frameRunView(run)
	if err := s.EnrichFrameRunView(ctx, userID, run, view); err != nil {
		return nil, err
	}
	return view, nil
}

// publishFrameRun 在提交后发布完整片段；广播失败不改变已提交的流程状态。
func (s *DramaFrameRunService) publishFrameRun(ctx context.Context, run *model.DramaSegmentFrameRun) error {
	view, p, ep, _, err := s.segments.readView(ctx, run.UserID, run.SegmentID)
	if err != nil {
		return err
	}
	publishDramaSegment(ctx, s.bc, p, ep, view)
	return nil
}

// changedFrameRunView 通知后重新读取，包含统一候选对账可能产生的 bound 状态。
func (s *DramaFrameRunService) changedFrameRunView(ctx context.Context, run *model.DramaSegmentFrameRun) (*model.DramaSegmentFrameRunView, error) {
	if err := s.publishFrameRun(ctx, run); err != nil {
		return nil, err
	}
	return s.reloadFrameRunView(ctx, run.UserID, run.SegmentID, run.ID)
}
