package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	source, cachedView, err := s.previewSourceAndCachedManifest(ctx, user, id, no, req)
	if err != nil {
		return nil, "", err
	}
	key, err := jsonhash.CanonicalSHA256(struct {
		User, Drama uint64
		Episode     int
		Ratio       string
		Req         model.DramaPreviewReq
	}{user, id, no, source.project.Ratio, *req})
	if err != nil {
		return nil, "", err
	}
	ticket, view, fresh, err := s.reservePreview(user, id, no, source.episode.ID, key)
	if err != nil {
		return nil, "", err
	}
	if fresh {
		s.startPreviewRender(ticket, source, cachedView, source.project.Ratio, req)
	}
	cookie := s.preview.sign(dramaPreviewClaims{User: user, Expires: s.preview.now().Add(time.Hour).Unix(), Purpose: "session"})
	return view, cookie, nil
}

// previewSourceAndCachedManifest 只读取项目/集/片段/探针缓存元数据，不触发任何冷视频下载或 FFmpeg 探测。
func (s *DramaService) previewSourceAndCachedManifest(ctx context.Context, user, id uint64, no int, req *model.DramaPreviewReq) (*dramaManifestSource, *model.DramaManifestView, error) {
	p, eps, err := s.loadEpisodeProjectAndEpisodes(ctx, user, id)
	if err != nil {
		return nil, nil, err
	}
	var ep *model.DramaEpisode
	for i := range eps {
		if eps[i].No == no {
			ep = &eps[i]
			break
		}
	}
	if ep == nil {
		return nil, nil, errcode.ErrDramaNotFound
	}
	if req.SegmentVersion != ep.SegmentVersion {
		return nil, nil, errcode.ErrDramaManifestStale
	}
	reader, ok := s.repo.(dramaManifestReader)
	if !ok {
		return nil, nil, errcode.ErrInternal
	}
	source, err := readDramaManifestSource(ctx, reader, p, *ep)
	if err != nil {
		return nil, nil, dramaError(err)
	}
	cachedView, hit, err := s.resolveCachedDramaManifestSource(ctx, source)
	if err != nil {
		return nil, nil, err
	}
	if hit {
		if req.ManifestHash != cachedView.ManifestHash {
			return nil, nil, errcode.ErrDramaManifestStale
		}
		if req.RangeF != nil && req.RangeF.OutF > cachedView.Manifest.TotalFrames {
			return nil, nil, errcode.ErrDramaEditInvalid
		}
		return source, cachedView, nil
	}
	return source, nil, nil
}

func (s *DramaService) loadEpisodeProjectAndEpisodes(ctx context.Context, user, id uint64) (*model.DramaProject, []model.DramaEpisode, error) {
	p, err := s.repo.Project(ctx, user, id, false)
	if err != nil {
		return nil, nil, dramaError(err)
	}
	eps, err := s.repo.Episodes(ctx, user, id)
	if err != nil {
		return nil, nil, err
	}
	return p, eps, nil
}

// reservePreview 清理过期票据、取消被覆盖的旧任务，并用同一锁保护初始化 pending 视图。
func (s *DramaService) reservePreview(user, id uint64, no int, episodeID uint64, key string) (*dramaPreviewTicket, *model.DramaPreviewView, bool, error) {
	p := s.preview
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, nil, false, errcode.ErrDramaJobBusy
	}
	now := p.now()
	pruneExpiredPreviewsLocked(p, now)

	fk := dramaPreviewKey{episodeID: episodeID, key: key}
	if existing := p.byKey[fk]; existing != nil && existing.episodeID == episodeID && now.Before(existing.expires) {
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
		user: user, drama: id, episode: no, episodeID: episodeID, key: key,
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
func (s *DramaService) startPreviewRender(t *dramaPreviewTicket, source *dramaManifestSource, view *model.DramaManifestView, ratio string, req *model.DramaPreviewReq) {
	reqCopy := *req
	if req.RangeF != nil {
		rng := *req.RangeF
		reqCopy.RangeF = &rng
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
		s.renderPreview(t, source, view, ratio, &reqCopy)
	}()
}

// renderPreview 先按需在后台完成冷片段下载与探测，再复用冻结清单渲染；失败仅记录票据终态。
func (s *DramaService) renderPreview(t *dramaPreviewTicket, source *dramaManifestSource, view *model.DramaManifestView, ratio string, req *model.DramaPreviewReq) {
	if view == nil {
		resolved, err := s.resolveDramaManifestSource(t.ctx, source)
		if err != nil {
			s.finishPreview(t, err)
			return
		}
		if req.SegmentVersion != resolved.SegmentVersion || req.ManifestHash != resolved.ManifestHash {
			s.finishPreview(t, errcode.ErrDramaManifestStale)
			return
		}
		if req.RangeF != nil && (req.RangeF.InF < 0 || req.RangeF.OutF <= req.RangeF.InF || req.RangeF.OutF > resolved.Manifest.TotalFrames) {
			s.finishPreview(t, errcode.ErrDramaEditInvalid)
			return
		}
		view = resolved
	}
	input := dramaComposeInput{Manifest: &view.Manifest, ManifestHash: view.ManifestHash, AspectRatio: ratio, Size: "720p", Variants: []string{req.Variant}}
	for _, c := range view.Manifest.Clips {
		input.AssetIDs = append(input.AssetIDs, c.AssetID)
	}
	var rf *videoframe.FrameRange
	if req.RangeF != nil {
		rf = &videoframe.FrameRange{InF: req.RangeF.InF, OutF: req.RangeF.OutF}
	}
	err := s.renderFrozenManifest(t.ctx, t.user, &input, filepath.Dir(t.path), []string{t.path}, rf)
	s.finishPreview(t, err)
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
		t.expires = now.Add(p.failTTL)
		_ = os.RemoveAll(filepath.Dir(t.path))
		return
	}
	t.status = "ready"
	t.expires = now.Add(p.readyTTL)
}

func (s *DramaService) previewViewLocked(t *dramaPreviewTicket, now time.Time) *model.DramaPreviewView {
	view := &model.DramaPreviewView{Ticket: t.id, Status: t.status, ExpiresAt: t.expires.Unix()}
	if t.status == "ready" {
		expires := min(t.expires.Unix(), now.Add(2*time.Minute).Unix())
		token := s.preview.sign(dramaPreviewClaims{User: t.user, Drama: t.drama, Episode: t.episode, Ticket: t.id, Expires: expires, Purpose: "stream"})
		view.StreamURL = fmt.Sprintf("/api/v1/dramas/%s/episodes/%d/preview/%s/stream?token=%s", idcodec.Encode(t.drama), t.episode, t.id, token)
	}
	return view
}

// GetEpisodePreview 先校验当前资源归属，再返回本人票据，跨用户与已删除资源统一不存在。
func (s *DramaService) GetEpisodePreview(ctx context.Context, user, id uint64, no int, ticket string) (*model.DramaPreviewView, error) {
	_, episodeID, err := s.previewEpisode(ctx, user, id, no)
	if err != nil {
		return nil, err
	}
	p := s.preview
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	t := p.tickets[ticket]
	if t == nil || t.user != user || t.drama != id || t.episode != no || t.episodeID != episodeID || !now.Before(t.expires) {
		return nil, errcode.ErrDramaNotFound
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
	_, episodeID, err := s.previewEpisode(ctx, session.User, id, no)
	if err != nil {
		return nil, errcode.ErrForbidden
	}
	p := s.preview
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.tickets[ticket]
	if t == nil || t.user != session.User || t.drama != id || t.episode != no || t.episodeID != episodeID || t.status != "ready" || !p.now().Before(t.expires) {
		return nil, errcode.ErrUnauthorized
	}
	file, err := os.Open(t.path)
	if err != nil {
		return nil, errcode.ErrUnauthorized
	}
	return file, nil
}

// previewEpisode 复核软删除、项目画幅和当前集号，签名本身不能代替资源权限。
func (s *DramaService) previewEpisode(ctx context.Context, user, id uint64, no int) (*model.DramaProject, uint64, error) {
	p, err := s.repo.Project(ctx, user, id, false)
	if err != nil {
		return nil, 0, dramaError(err)
	}
	eps, err := s.repo.Episodes(ctx, user, id)
	if err != nil {
		return nil, 0, err
	}
	for _, ep := range eps {
		if ep.No == no {
			return p, ep.ID, nil
		}
	}
	return nil, 0, errcode.ErrDramaNotFound
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
