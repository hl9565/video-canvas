package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/jsonhash"
	"video-canvas/internal/pkg/videoframe"
)

// SetPreviewTiming 允许测试覆盖排队与渲染统一截止时间、就绪 TTL、失败冷却期和时钟。
func (s *DramaService) SetPreviewTiming(execTimeout, readyTTL, failTTL time.Duration, now func() time.Time) {
	p := s.preview
	p.mu.Lock()
	defer p.mu.Unlock()
	if execTimeout > 0 {
		p.execTimeout = execTimeout
	}
	if readyTTL > 0 {
		p.readyTTL = readyTTL
	}
	if failTTL > 0 {
		p.failTTL = failTTL
	}
	if now != nil {
		p.now = now
	}
}

// CreateEpisodePreview 按用户、清单版本、变体和区间复用 CPU 票据，绝不登记素材或写成片引用。
// 建票阶段只执行轻量项目/集/版本校验并优先使用已缓存探针；冷片段下载与解码统一延后至受控后台任务。
func (s *DramaService) CreateEpisodePreview(ctx context.Context, user, id uint64, no int, req *model.DramaPreviewReq) (*model.DramaPreviewView, string, error) {
	if req == nil || req.SegmentVersion < 0 || !validDramaManifestHash(req.ManifestHash) || (req.Variant != "clean" && req.Variant != "packaged") {
		return nil, "", errcode.ErrInvalidParams
	}
	if req.RangeF != nil && (req.RangeF.InF < 0 || req.RangeF.OutF <= req.RangeF.InF) {
		return nil, "", errcode.ErrDramaEditInvalid
	}
	source, err := s.previewSourceAndCachedManifest(ctx, user, id, no, req)
	if err != nil {
		return nil, "", err
	}
	identity := dramaPreviewIdentity{
		User: user, Drama: id, Episode: no, EpisodeID: source.episode.ID,
		AspectRatio: source.project.Ratio, RenderVersion: model.CurrentDramaManifestRenderVersion, Req: *req,
	}
	if source.stageC != nil {
		identity.RenderVersion = model.CurrentDramaManifestV2RenderVersion
	}
	key, err := jsonhash.CanonicalSHA256(identity)
	if err != nil {
		return nil, "", err
	}
	ticket, view, fresh, err := s.reservePreview(identity, source, key)
	if err != nil {
		return nil, "", err
	}
	if fresh {
		s.startPreviewRender(ticket, source, identity)
	}
	cookie := s.preview.sign(dramaPreviewClaims{User: user, Expires: s.preview.now().Add(time.Hour).Unix(), Purpose: "session"})
	return view, cookie, nil
}

// dramaPreviewSource 保存两版渲染之一的媒体快照，热缓存时同时携带冻结清单。
type dramaPreviewSource struct {
	*dramaManifestSource
	v1 *model.DramaManifestView
	v2 *dramaV2Resolution
}

// previewSourceAndCachedManifest 校验项目和集版本，仅检查热探针与不依赖冷素材的错误。
func (s *DramaService) previewSourceAndCachedManifest(ctx context.Context, user, id uint64, no int, req *model.DramaPreviewReq) (*dramaPreviewSource, error) {
	p, ep, err := s.readProgramEpisode(ctx, user, id, no)
	if err != nil {
		return nil, err
	}
	if req.SegmentVersion != ep.SegmentVersion {
		return nil, errcode.ErrDramaManifestStale
	}
	reader, ok := s.repo.(dramaManifestReader)
	if !ok {
		return nil, errcode.ErrInternal
	}
	source, err := readDramaRenderSource(ctx, reader, p, *ep)
	if err != nil {
		return nil, dramaError(err)
	}
	out := &dramaPreviewSource{dramaManifestSource: source}
	if source.stageC != nil {
		out.v2, err = s.cachedProgramPreview(ctx, source.stageC, req)
		return out, err
	}
	view, hit, err := s.resolveCachedDramaManifestSource(ctx, source)
	if err != nil || !hit {
		return out, err
	}
	out.v1 = view
	return out, checkPreviewTimeline(req, view.SegmentVersion, view.ManifestHash, view.Manifest.TotalFrames)
}

// cachedProgramPreview 拒绝已知诊断，即使其余素材仍需后台探测。
func (s *DramaService) cachedProgramPreview(ctx context.Context, source *dramaManifestV2Source, req *model.DramaPreviewReq) (*dramaV2Resolution, error) {
	if err := checkDramaV2Ratio(source.project.Ratio); err != nil {
		return nil, err
	}
	view, err := s.resolveCachedDramaManifestV2Source(ctx, source)
	if err != nil || view == nil {
		return nil, err
	}
	return view, checkProgramPreview(req, view)
}

// checkProgramPreview 对已有诊断提供可定位的错误，再核对客户端清单与区间。
func checkProgramPreview(req *model.DramaPreviewReq, view *dramaV2Resolution) error {
	if err := dramaV2ComposeDiagnostic(view.ProgramDiagnostics); err != nil {
		return err
	}
	return checkPreviewTimeline(req, view.SegmentVersion, view.ManifestHash, view.frozen.TotalFrames)
}

// checkPreviewTimeline 预览只能渲染请求指向的冻结版本与有效半开区间。
func checkPreviewTimeline(req *model.DramaPreviewReq, version int64, hash string, total int32) error {
	if req.SegmentVersion != version || req.ManifestHash != hash {
		return errcode.ErrDramaManifestStale
	}
	if req.RangeF != nil && req.RangeF.OutF > total {
		return errcode.ErrDramaEditInvalid
	}
	return nil
}

// reservePreview 清理过期票据、取消被覆盖的旧任务，并用同一锁保护初始化 pending 视图。
func (s *DramaService) reservePreview(identity dramaPreviewIdentity, source *dramaPreviewSource, key string) (*dramaPreviewTicket, *model.DramaPreviewView, bool, error) {
	p := s.preview
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, nil, false, errcode.ErrDramaJobBusy
	}
	now := p.now()
	pruneExpiredPreviewsLocked(p, now)

	fk := dramaPreviewKey{episodeID: identity.EpisodeID, key: key}
	if existing := p.byKey[fk]; existing != nil && existing.episodeID == identity.EpisodeID && now.Before(existing.expires) {
		return existing, s.previewViewLocked(existing, now), false, nil
	}
	if len(p.tickets) >= 128 {
		return nil, nil, false, errcode.ErrDramaJobBusy
	}
	random := make([]byte, 24)
	if _, err := rand.Read(random); err != nil {
		return nil, nil, false, err
	}
	dir, err := os.MkdirTemp("", "vc-private-preview-*")
	if err != nil {
		return nil, nil, false, err
	}
	deadline := now.Add(p.execTimeout)
	tCtx, tCancel := context.WithTimeout(p.ctx, p.execTimeout)
	t := &dramaPreviewTicket{
		user: identity.User, drama: identity.Drama, episode: identity.Episode, episodeID: identity.EpisodeID, key: key,
		segmentVersion: identity.Req.SegmentVersion, manifestHash: identity.Req.ManifestHash, aspectRatio: identity.AspectRatio, renderVersion: identity.RenderVersion,
		stageC: source.stageC, v2: source.v2,
		id: hex.EncodeToString(random), path: filepath.Join(dir, "preview.mp4"),
		status: "pending", expires: deadline, ctx: tCtx, cancel: tCancel,
	}
	p.tickets[t.id] = t
	p.byKey[fk] = t
	p.wg.Add(1)
	return t, &model.DramaPreviewView{Ticket: t.id, Status: "pending", ExpiresAt: deadline.Unix()}, true, nil
}

func pruneExpiredPreviewsLocked(p *dramaPreviewStore, now time.Time) {
	for id, ticket := range p.tickets {
		if now.Before(ticket.expires) {
			continue
		}
		if ticket.cancel != nil {
			ticket.cancel()
		}
		// 仍在途的任务交由其渲染协程退出时统一删临时目录，避免与写文件竞态。
		if ticket.status != "pending" {
			_ = os.RemoveAll(filepath.Dir(ticket.path))
		}
		delete(p.tickets, id)
		fk := dramaPreviewKey{episodeID: ticket.episodeID, key: ticket.key}
		if p.byKey[fk] == ticket {
			delete(p.byKey, fk)
		}
	}
}

// startPreviewRender 在受控并发池中执行冷探测与片段渲染，队列等待与执行共用票据的统一截止时间。
func (s *DramaService) startPreviewRender(t *dramaPreviewTicket, source *dramaPreviewSource, identity dramaPreviewIdentity) {
	req := identity.Req
	if req.RangeF != nil {
		rng := *req.RangeF
		req.RangeF = &rng
	}
	go func() {
		defer s.preview.wg.Done()
		defer t.cancel()
		select {
		case s.preview.sem <- struct{}{}:
			defer func() { <-s.preview.sem }()
		case <-t.ctx.Done():
			s.finishPreview(t, t.ctx.Err())
			return
		}
		s.finishPreview(t, s.renderPreview(t, source, identity.AspectRatio, &req))
	}()
}

// renderPreview 先按需完成冷探测，再用票据指定的版本渲染同一份冻结清单。
func (s *DramaService) renderPreview(t *dramaPreviewTicket, source *dramaPreviewSource, ratio string, req *model.DramaPreviewReq) error {
	var rf *videoframe.FrameRange
	if req.RangeF != nil {
		rf = &videoframe.FrameRange{InF: req.RangeF.InF, OutF: req.RangeF.OutF}
	}
	req.SegmentVersion, req.ManifestHash = t.segmentVersion, t.manifestHash
	if t.renderVersion == model.CurrentDramaManifestV2RenderVersion {
		return s.renderProgramPreview(t, ratio, req, rf)
	}
	view := source.v1
	if view == nil {
		resolved, err := s.resolveDramaManifestSource(t.ctx, source.dramaManifestSource)
		if err != nil {
			return err
		}
		view = resolved
	}
	if err := checkPreviewTimeline(req, view.SegmentVersion, view.ManifestHash, view.Manifest.TotalFrames); err != nil {
		return err
	}
	input := dramaComposeInput{Manifest: &view.Manifest, AspectRatio: ratio, Size: "720p", Variants: []string{req.Variant}}
	return s.renderFrozenManifest(t.ctx, t.user, &input, filepath.Dir(t.path), []string{t.path}, rf)
}

// renderProgramPreview 传入完整节目单和独立选区，以整集混音、闪避及归一化结果为准。
func (s *DramaService) renderProgramPreview(t *dramaPreviewTicket, ratio string, req *model.DramaPreviewReq, rf *videoframe.FrameRange) error {
	view := t.v2
	if view == nil {
		resolved, err := s.resolveDramaManifestV2Source(t.ctx, t.stageC)
		if err != nil {
			return err
		}
		view = resolved
	}
	if err := checkProgramPreview(req, view); err != nil {
		return err
	}
	width, height := composeDimensions(ratio, "720p")
	outputs := []videoframe.ComposeProgramOutputSpec{{Destination: t.path, Variant: req.Variant, Width: width, Height: height}}
	_, err := s.RenderProgramManifest(t.ctx, t.user, view.frozen, filepath.Dir(t.path), outputs, rf)
	return err
}

// finishPreview 在渲染结束时转换终态；就绪播放期与失败冷却期均从各自终态时刻起算。
func (s *DramaService) finishPreview(t *dramaPreviewTicket, err error) {
	p := s.preview
	p.mu.Lock()
	defer p.mu.Unlock()
	active := !p.closed && p.tickets[t.id] == t
	now := p.now()
	if err != nil || !active {
		t.status = "failed"
		if !active {
			err = nil // 已覆盖或关闭的任务不再公开原失败原因。
		}
		t.err = err
		t.expires = now.Add(p.failTTL)
		_ = os.RemoveAll(filepath.Dir(t.path))
		return
	}
	t.status = "ready"
	t.expires = now.Add(p.readyTTL)
}

func (s *DramaService) previewViewLocked(t *dramaPreviewTicket, now time.Time) *model.DramaPreviewView {
	view := &model.DramaPreviewView{Ticket: t.id, Status: t.status, ExpiresAt: t.expires.Unix()}
	if t.status == "failed" {
		view.ErrorCode, view.ErrorMessage, view.ProgramDiagnostics = previewFailure(t.err)
	}
	if t.status == "ready" {
		expires := min(t.expires.Unix(), now.Add(2*time.Minute).Unix())
		token := s.preview.sign(dramaPreviewClaims{User: t.user, Drama: t.drama, Episode: t.episode, Ticket: t.id, Expires: expires, Purpose: "stream"})
		view.StreamURL = fmt.Sprintf("/api/v1/dramas/%s/episodes/%d/preview/%s/stream?token=%s", idcodec.Encode(t.drama), t.episode, t.id, token)
	}
	return view
}

// previewFailure 只投影业务文案与公开诊断，内部原因和临时文件路径永不进入票据。
func previewFailure(err error) (int, string, []model.DramaPreviewDiagnostic) {
	var business *errcode.Error
	var program *model.DramaProgramError
	switch {
	case errors.As(err, &business):
		if business.Code != errcode.ErrInternal.Code {
			return business.Code, business.Msg, previewDiagnostics(business.Data)
		}
	case errors.As(err, &program):
		msg := program.Msg
		if msg == "" {
			msg = errcode.ErrDramaProgramInvalid.Msg
		}
		return errcode.ErrDramaProgramInvalid.Code, msg, previewDiagnostics(program.Diagnostics)
	case errors.Is(err, context.DeadlineExceeded):
		return errcode.ErrInternal.Code, "CPU 预览生成超时，请缩小范围或重试", nil
	}
	return errcode.ErrInternal.Code, "CPU 预览生成失败，请检查片段视频并重试", nil
}

// previewDiagnostics 丢弃非诊断数据，仅保留与节目单 API 相同的五个公开字段。
func previewDiagnostics(data any) []model.DramaPreviewDiagnostic {
	var items []DramaProgramDiagnostic
	switch value := data.(type) {
	case DramaProgramDiagnosticsData:
		items = value.Diagnostics
	case *DramaProgramDiagnosticsData:
		if value != nil {
			items = value.Diagnostics
		}
	case []model.DramaProgramDiagnostic:
		items = dramaProgramDiagnosticsView(value)
	}
	out := make([]model.DramaPreviewDiagnostic, 0, len(items))
	for _, d := range items {
		item := model.DramaPreviewDiagnostic{CueID: d.CueID, Field: d.Field, Reason: d.Reason, Severity: d.Severity}
		if d.SegmentID != 0 {
			item.SegmentID = idcodec.Encode(uint64(d.SegmentID))
		}
		out = append(out, item)
	}
	return out
}

// GetEpisodePreview 先校验当前资源归属，再返回本人票据，跨用户与已删除资源统一不存在。
func (s *DramaService) GetEpisodePreview(ctx context.Context, user, id uint64, no int, ticket string) (*model.DramaPreviewView, error) {
	pProj, ep, err := s.readProgramEpisode(ctx, user, id, no)
	if err != nil {
		return nil, err
	}
	p := s.preview
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	t := p.tickets[ticket]
	if t == nil || t.user != user || t.drama != id || t.episode != no || !now.Before(t.expires) {
		return nil, errcode.ErrDramaNotFound
	}
	if t.stale(pProj, ep) {
		return nil, errcode.ErrDramaManifestStale
	}
	return s.previewViewLocked(t, now), nil
}

// OpenEpisodePreview 强制验证 Cookie、短期 token、票据及当前项目集归属，返回仅供代理读取的文件。
func (s *DramaService) OpenEpisodePreview(ctx context.Context, id uint64, no int, ticket, cookie, token string) (*os.File, error) {
	session, err := s.preview.verify(cookie, "session")
	if err != nil {
		return nil, errcode.ErrUnauthorized
	}
	claims, err := s.preview.verify(token, "stream")
	if err != nil {
		return nil, errcode.ErrUnauthorized
	}
	if claims.User != session.User || claims.Drama != id || claims.Episode != no || claims.Ticket != ticket {
		return nil, errcode.ErrForbidden
	}
	pProj, ep, err := s.readProgramEpisode(ctx, session.User, id, no)
	if err != nil {
		return nil, errcode.ErrForbidden
	}
	p := s.preview
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.tickets[ticket]
	if t == nil || t.user != session.User || t.drama != id || t.episode != no || !p.now().Before(t.expires) {
		return nil, errcode.ErrUnauthorized
	}
	if t.stale(pProj, ep) {
		return nil, errcode.ErrDramaManifestStale
	}
	if t.status != "ready" {
		return nil, errcode.ErrUnauthorized
	}
	file, err := os.Open(t.path)
	if err != nil {
		return nil, errcode.ErrUnauthorized
	}
	return file, nil
}

// stale 同时识别当前项目变更和冷探测完成后才发现的清单冲突；调用时持有票据锁。
func (t *dramaPreviewTicket) stale(project *model.DramaProject, ep *model.DramaEpisode) bool {
	return t.episodeID != ep.ID || t.segmentVersion != ep.SegmentVersion || t.aspectRatio != project.Ratio ||
		errors.Is(t.err, errcode.ErrDramaManifestStale)
}

// ClosePreviews 在服务退出时取消后台预览、等待渲染协程退出并回收全部临时目录。
func (s *DramaService) ClosePreviews() {
	p := s.preview
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		p.cancel()
	}
	p.mu.Unlock()
	p.wg.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, t := range p.tickets {
		_ = os.RemoveAll(filepath.Dir(t.path))
	}
	p.tickets = map[string]*dramaPreviewTicket{}
	p.byKey = map[dramaPreviewKey]*dramaPreviewTicket{}
	p.manifestInFlight = map[string]struct{}{}
	p.manifestErr = map[string]manifestWarmupErr{}
}
