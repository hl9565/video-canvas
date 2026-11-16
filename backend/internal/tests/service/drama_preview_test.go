package service_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/provider"
	"video-canvas/internal/service"
)

type manifestProcessor struct {
	composeProcessor
	mu             sync.Mutex
	callsManifest  int
	input          videoframe.ComposeManifestInput
	duringManifest func()
}

func (p *manifestProcessor) manifestCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.callsManifest
}

func (p *manifestProcessor) lastInput() videoframe.ComposeManifestInput {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.input
}

func (p *manifestProcessor) ComposeManifest(_ context.Context, in videoframe.ComposeManifestInput) error {
	p.mu.Lock()
	p.callsManifest++
	p.input = in
	fn := p.duringManifest
	p.mu.Unlock()
	if fn != nil {
		fn()
	}
	for _, out := range in.Outputs {
		if err := os.WriteFile(out.Destination, []byte("preview-video-data"), 0600); err != nil {
			return err
		}
	}
	return nil
}

func waitPreviewReady(ctx context.Context, t *testing.T, s *service.DramaService, user, drama uint64, ep int, ticket string) *model.DramaPreviewView {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		v, err := s.GetEpisodePreview(ctx, user, drama, ep, ticket)
		if err != nil {
			t.Fatalf("轮询预览状态失败: %v", err)
		}
		if v.Status == "ready" {
			return v
		}
		if v.Status == "failed" {
			t.Fatalf("预览意外失败: %+v", v)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待预览就绪超时: ticket=%s", ticket)
	return nil
}
func previewService(r *composeMemory) (*service.DramaService, *manifestProcessor) {
	_, a, probes := manifestService(r)
	probes.rows[a.sha] = &model.AssetMediaProbe{AssetID: 30, Sha256: a.sha, ProbeVersion: 1, NormalizedFrames24: 120}
	p := &manifestProcessor{}
	return service.NewDramaService(service.DramaDeps{Repo: r, Assets: a, Probes: probes, Compose: p, PreviewSecret: "test-preview-secret"}), p
}
func TestDramaPreview_CookieTokenOwnershipAndCache(t *testing.T) {
	r := newComposeMemory()
	s, p := previewService(r)
	ctx := context.Background()
	m, err := s.ResolveEpisodeManifest(ctx, 7, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	req := &model.DramaPreviewReq{SegmentVersion: m.SegmentVersion, ManifestHash: m.ManifestHash, Variant: "clean"}
	v, cookie, err := s.CreateEpisodePreview(ctx, 7, 1, 1, req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.ClosePreviews() })
	again, _, err := s.CreateEpisodePreview(ctx, 7, 1, 1, req)
	if err != nil || again.Ticket != v.Ticket {
		t.Fatalf("重复建票失败 %v %+v", err, again)
	}
	if r.episodes[0].FinalAssetID != nil || len(r.assets) != 0 {
		t.Fatal("预览登记素材或覆盖成片")
	}
	status := waitPreviewReady(ctx, t, s, 7, 1, 1, v.Ticket)
	if !strings.Contains(status.StreamURL, "token=") || p.manifestCalls() != 1 {
		t.Fatalf("状态 %+v calls=%d", status, p.manifestCalls())
	}
	token := strings.Split(status.StreamURL, "token=")[1]
	for _, c := range []string{"", cookie + "tampered"} {
		file, err := s.OpenEpisodePreview(ctx, 1, 1, v.Ticket, c, token)
		if err == nil {
			_ = file.Close()
			t.Fatal("伪造身份获取视频")
		}
	}
	file, err := s.OpenEpisodePreview(ctx, 1, 1, v.Ticket, cookie, token)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err := s.GetEpisodePreview(ctx, 8, 1, 1, v.Ticket); err == nil {
		t.Fatal("跨用户票据泄露")
	}
	if _, err := s.OpenEpisodePreview(ctx, 1, 2, v.Ticket, cookie, token); err == nil {
		t.Fatal("跨集 token 被接受")
	}
	req.ManifestHash = strings.Repeat("b", 64)
	_, _, err = s.CreateEpisodePreview(ctx, 7, 1, 1, req)
	dramaCheckCode(t, err, 91001)
}

func TestDramaPreview_DeletedEpisodeCannotReuseTicketAfterRenumber(t *testing.T) {
	r := newComposeMemory()
	s, _ := previewService(r)
	t.Cleanup(s.ClosePreviews)
	ctx := context.Background()
	m, err := s.ResolveEpisodeManifest(ctx, 7, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	v, cookie, err := s.CreateEpisodePreview(ctx, 7, 1, 1, &model.DramaPreviewReq{ManifestHash: m.ManifestHash, Variant: "clean"})
	if err != nil {
		t.Fatal(err)
	}
	v = waitPreviewReady(ctx, t, s, 7, 1, 1, v.Ticket)
	token := strings.Split(v.StreamURL, "token=")[1]
	r.episodes[0].ID = 77 // 删除原集后，另一集补位到相同 no。
	if _, err = s.GetEpisodePreview(ctx, 7, 1, 1, v.Ticket); err == nil {
		t.Fatal("补位集继承了已删除集的预览")
	}
	f, err := s.OpenEpisodePreview(ctx, 1, 1, v.Ticket, cookie, token)
	if err == nil {
		_ = f.Close()
		t.Fatal("已删除集预览仍可播放")
	}
}

func TestDramaPreview_ConcurrentSameIntentRendersOnce(t *testing.T) {
	r := newComposeMemory()
	s, p := previewService(r)
	t.Cleanup(s.ClosePreviews)
	ctx := context.Background()
	m, err := s.ResolveEpisodeManifest(ctx, 7, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	req := &model.DramaPreviewReq{ManifestHash: m.ManifestHash, Variant: "clean"}
	entered, release := make(chan struct{}), make(chan struct{})
	p.duringManifest = func() { close(entered); <-release }
	firstV, _, err := s.CreateEpisodePreview(ctx, 7, 1, 1, req)
	if err != nil || firstV.Status != "pending" {
		t.Fatalf("首次建票应立即返回 pending: view=%+v err=%v", firstV, err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("预览未开始")
	}
	pending, _, err := s.CreateEpisodePreview(ctx, 7, 1, 1, req)
	close(release)
	done := waitPreviewReady(ctx, t, s, 7, 1, 1, firstV.Ticket)
	if err != nil || pending.Ticket != firstV.Ticket || pending.Status != "pending" || done.Status != "ready" || p.manifestCalls() != 1 {
		t.Fatalf("同意图重复渲染: pending=%+v done=%+v calls=%d err=%v", pending, done, p.manifestCalls(), err)
	}
}

func TestDramaPreview_ColdProbeInBackgroundAndInitialPendingOnFastFailure(t *testing.T) {
	t.Run("cold probe runs in background task instead of blocking POST", func(t *testing.T) {
		r := newComposeMemory()
		_, baseAssets, emptyProbes := manifestService(r)
		openCalls := atomic.Int32{}
		probeCalls := atomic.Int32{}
		probeEntered, releaseProbe := make(chan struct{}), make(chan struct{})
		coldAssets := &previewCountingOpenAssets{manifestFiles: baseAssets, openCalls: &openCalls}
		proc := &manifestProcessor{}
		s := service.NewDramaService(service.DramaDeps{
			Repo: r, Assets: coldAssets, Probes: emptyProbes, Compose: proc, PreviewSecret: "cold-preview-secret",
			ProbeVideo: func(ctx context.Context, _ string) (videoframe.NormalizedProbe, error) {
				probeCalls.Add(1)
				close(probeEntered)
				select {
				case <-releaseProbe:
					return videoframe.NormalizedProbe{FpsNum: 24, FpsDen: 1, NormalizedFrames24: 120}, nil
				case <-ctx.Done():
					return videoframe.NormalizedProbe{}, ctx.Err()
				}
			},
		})
		t.Cleanup(s.ClosePreviews)

		start := time.Now()
		v, cookie, err := s.CreateEpisodePreview(context.Background(), 7, 1, 1, &model.DramaPreviewReq{
			SegmentVersion: 0, ManifestHash: strings.Repeat("a", 64), Variant: "clean",
		})
		if err != nil || time.Since(start) > 200*time.Millisecond {
			t.Fatalf("冷片段建票不应阻塞等待下载探测: err=%v elapsed=%v", err, time.Since(start))
		}
		if v.Status != "pending" || v.StreamURL != "" || cookie == "" {
			t.Fatalf("冷片段建票首响应必须返回预先构造的 pending 视图与 Cookie: %+v cookie=%q", v, cookie)
		}
		select {
		case <-probeEntered:
		case <-time.After(time.Second):
			t.Fatal("冷探测未在后台任务中执行")
		}
		// 需要用真实冷清单哈希放行校验，再次验证探针真正走通并产出可播放流。
		close(releaseProbe)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			st, getErr := s.GetEpisodePreview(context.Background(), 7, 1, 1, v.Ticket)
			if getErr == nil && st.Status == "failed" {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if openCalls.Load() == 0 || probeCalls.Load() == 0 || proc.manifestCalls() != 0 {
			t.Fatalf("假哈希冷探测完成后未阻断错误清单渲染: open=%d probe=%d manifestCalls=%d", openCalls.Load(), probeCalls.Load(), proc.manifestCalls())
		}

		// 现探测缓存已热，用真实 manifestHash 再建票应顺利就绪。
		m, err := s.ResolveEpisodeManifest(context.Background(), 7, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		okV, _, err := s.CreateEpisodePreview(context.Background(), 7, 1, 1, &model.DramaPreviewReq{
			SegmentVersion: m.SegmentVersion, ManifestHash: m.ManifestHash, Variant: "clean",
		})
		if err != nil || okV.Status != "pending" {
			t.Fatalf("真实清单建票失败: err=%v view=%+v", err, okV)
		}
		ready := waitPreviewReady(context.Background(), t, s, 7, 1, 1, okV.Ticket)
		if ready.Status != "ready" || ready.StreamURL == "" {
			t.Fatalf("冷转热后预览未就绪: %+v", ready)
		}
	})

	t.Run("initial POST returns pending even when background fails immediately", func(t *testing.T) {
		r := newComposeMemory()
		_, a, probes := manifestService(r)
		probes.rows[a.sha] = &model.AssetMediaProbe{AssetID: 30, Sha256: a.sha, ProbeVersion: 1, NormalizedFrames24: 120}
		failProc := &failingManifestProcessor{err: errors.New("boom")}
		s := service.NewDramaService(service.DramaDeps{
			Repo: r, Assets: a, Probes: probes, Compose: failProc, PreviewSecret: "fast-fail-secret",
		})
		t.Cleanup(s.ClosePreviews)

		m, err := s.ResolveEpisodeManifest(context.Background(), 7, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		v, cookie, err := s.CreateEpisodePreview(context.Background(), 7, 1, 1, &model.DramaPreviewReq{
			SegmentVersion: m.SegmentVersion, ManifestHash: m.ManifestHash, Variant: "clean",
		})
		if err != nil || v.Status != "pending" || v.StreamURL != "" || cookie == "" {
			t.Fatalf("即使后台立即失败，新建 POST 首响应也必须是 pending: err=%v view=%+v cookie=%q", err, v, cookie)
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			st, gErr := s.GetEpisodePreview(context.Background(), 7, 1, 1, v.Ticket)
			if gErr == nil && st.Status == "failed" {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("后台立即失败后轮询状态未转为 failed")
	})
}

func TestDramaPreview_UnifiedDeadlineDuplicateCancellationAndTerminalTTL(t *testing.T) {
	t.Run("ready playback window and failure cooldown start from terminal transitions", func(t *testing.T) {
		r := newComposeMemory()
		s, p := previewService(r)
		t.Cleanup(s.ClosePreviews)

		nowMu := sync.Mutex{}
		curNow := time.Unix(1_700_000_000, 0)
		nowFn := func() time.Time {
			nowMu.Lock()
			defer nowMu.Unlock()
			return curNow
		}
		advance := func(d time.Duration) {
			nowMu.Lock()
			curNow = curNow.Add(d)
			nowMu.Unlock()
		}
		s.SetPreviewTiming(10*time.Minute, 10*time.Minute, time.Minute, nowFn)

		m, err := s.ResolveEpisodeManifest(context.Background(), 7, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		entered, release := make(chan struct{}), make(chan struct{})
		p.duringManifest = func() {
			close(entered)
			<-release
		}
		req := &model.DramaPreviewReq{SegmentVersion: m.SegmentVersion, ManifestHash: m.ManifestHash, Variant: "clean"}
		v0, _, err := s.CreateEpisodePreview(context.Background(), 7, 1, 1, req)
		if err != nil || v0.ExpiresAt != curNow.Add(10*time.Minute).Unix() {
			t.Fatalf("初始 pending 截止时间不符: err=%v view=%+v", err, v0)
		}
		<-entered
		// 模拟排队与渲染共耗时 6 分钟（仍在 10m 统一截止时间内），就绪后应从第 6 分钟起算完整 10m 播放期。
		advance(6 * time.Minute)
		close(release)
		ready := waitPreviewReady(context.Background(), t, s, 7, 1, 1, v0.Ticket)
		wantReadyExp := time.Unix(1_700_000_000, 0).Add(16 * time.Minute).Unix()
		if ready.ExpiresAt != wantReadyExp {
			t.Fatalf("就绪播放期应从终态起算 10 分钟: got=%d want=%d", ready.ExpiresAt, wantReadyExp)
		}

		// 失败分支验证：排队 4 分钟后失败，冷却期从第 4 分钟起算 1 分钟；冷却内重 POST 不重复转码，冷却后可重触发。
		r2 := newComposeMemory()
		_, a2, probes2 := manifestService(r2)
		probes2.rows[a2.sha] = &model.AssetMediaProbe{AssetID: 30, Sha256: a2.sha, ProbeVersion: 1, NormalizedFrames24: 120}
		failEntered, releaseFail := make(chan struct{}), make(chan struct{})
		failProc := &hookedFailManifestProcessor{
			beforeErr: func() {
				close(failEntered)
				<-releaseFail
			},
			err: errors.New("render boom"),
		}
		s2 := service.NewDramaService(service.DramaDeps{
			Repo: r2, Assets: a2, Probes: probes2, Compose: failProc, PreviewSecret: "fail-ttl-secret",
		})
		t.Cleanup(s2.ClosePreviews)
		failNow := time.Unix(1_700_000_000, 0)
		var fMu sync.Mutex
		s2.SetPreviewTiming(10*time.Minute, 10*time.Minute, time.Minute, func() time.Time {
			fMu.Lock()
			defer fMu.Unlock()
			return failNow
		})
		m2, err := s2.ResolveEpisodeManifest(context.Background(), 7, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		fv0, _, err := s2.CreateEpisodePreview(context.Background(), 7, 1, 1, &model.DramaPreviewReq{
			SegmentVersion: m2.SegmentVersion, ManifestHash: m2.ManifestHash, Variant: "clean",
		})
		if err != nil {
			t.Fatal(err)
		}
		<-failEntered
		fMu.Lock()
		failNow = failNow.Add(4 * time.Minute)
		fMu.Unlock()
		close(releaseFail)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			st, gErr := s2.GetEpisodePreview(context.Background(), 7, 1, 1, fv0.Ticket)
			if gErr == nil && st.Status == "failed" {
				if st.ExpiresAt != time.Unix(1_700_000_000, 0).Add(5*time.Minute).Unix() {
					t.Fatalf("失败冷却期应从失败终态起算 1 分钟: got=%d", st.ExpiresAt)
				}
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		// 冷却期内重复 POST 返回同一失败票据且不重渲染；超过冷却期才建新票。
		hitFail, _, err := s2.CreateEpisodePreview(context.Background(), 7, 1, 1, &model.DramaPreviewReq{
			SegmentVersion: m2.SegmentVersion, ManifestHash: m2.ManifestHash, Variant: "clean",
		})
		if err != nil || hitFail.Ticket != fv0.Ticket || failProc.calls.Load() != 1 {
			t.Fatalf("冷却期内不应重复渲染: hit=%+v calls=%d err=%v", hitFail, failProc.calls.Load(), err)
		}
	})

	t.Run("queue timeout unified with render cancels task and duplicate POST cancels expired old task cleanly", func(t *testing.T) {
		r := newComposeMemory()
		_, a, probes := manifestService(r)
		probes.rows[a.sha] = &model.AssetMediaProbe{AssetID: 30, Sha256: a.sha, ProbeVersion: 1, NormalizedFrames24: 120}

		gate := make(chan struct{})
		queuedCanceled := make(chan struct{}, 1)
		var slotCalls atomic.Int32
		comp := &slotGateComposer{
			gate:     gate,
			calls:    &slotCalls,
			canceled: queuedCanceled,
		}
		s := service.NewDramaService(service.DramaDeps{
			Repo: r, Assets: a, Probes: probes, Compose: comp, PreviewSecret: "queue-deadline-secret",
		})
		t.Cleanup(s.ClosePreviews)
		s.SetPreviewTiming(200*time.Millisecond, 2*time.Second, 200*time.Millisecond, time.Now)

		m, err := s.ResolveEpisodeManifest(context.Background(), 7, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		// 先占满 4 个并发槽位，使第 5 个任务排队直到统一截止时间到期被取消。
		for i := 0; i < 4; i++ {
			rng := &model.DramaPreviewRange{InF: int32(i), OutF: int32(i + 10)}
			if _, _, err := s.CreateEpisodePreview(context.Background(), 7, 1, 1, &model.DramaPreviewReq{
				SegmentVersion: m.SegmentVersion, ManifestHash: m.ManifestHash, Variant: "clean", RangeF: rng,
			}); err != nil {
				t.Fatal(err)
			}
		}
		deadline := time.Now().Add(time.Second)
		for comp.entered.Load() < 4 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		queuedReq := &model.DramaPreviewReq{
			SegmentVersion: m.SegmentVersion, ManifestHash: m.ManifestHash, Variant: "clean",
			RangeF: &model.DramaPreviewRange{InF: 20, OutF: 30},
		}
		v5, _, err := s.CreateEpisodePreview(context.Background(), 7, 1, 1, queuedReq)
		if err != nil || v5.Status != "pending" {
			t.Fatalf("第 5 个任务应先返回 pending: err=%v view=%+v", err, v5)
		}
		// 在统一截止期内重复 POST 返回同一排队中的 T5，绝不发起重复转码。
		v5Dup, _, err := s.CreateEpisodePreview(context.Background(), 7, 1, 1, queuedReq)
		if err != nil || v5Dup.Ticket != v5.Ticket {
			t.Fatalf("排队期间重复 POST 应命中原票据: dup=%+v orig=%+v err=%v", v5Dup, v5, err)
		}
		// 等统一截止期(200ms)触发：T5 在队列中被取消并进入 failed 冷却，释放槽位前不应渲染第 5 个任务。
		waitDeadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(waitDeadline) {
			st, gErr := s.GetEpisodePreview(context.Background(), 7, 1, 1, v5.Ticket)
			if gErr == nil && st.Status == "failed" {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		close(gate)
		st5, gErr := s.GetEpisodePreview(context.Background(), 7, 1, 1, v5.Ticket)
		if gErr != nil || st5.Status != "failed" {
			t.Fatalf("排队超统一截止时间应转入 failed 而非消失或偷跑渲染: err=%v st=%+v", gErr, st5)
		}

		// 验证过期覆盖时取消旧任务：时钟跨越旧任务 deadline 后再发重复 POST，旧渲染必须收到 cancel 并让新票就绪。
		r2 := newComposeMemory()
		_, a2, probes2 := manifestService(r2)
		probes2.rows[a2.sha] = &model.AssetMediaProbe{AssetID: 30, Sha256: a2.sha, ProbeVersion: 1, NormalizedFrames24: 120}
		firstEntered, firstDone := make(chan struct{}), make(chan struct{})
		swComp := &supersedeComposer{firstEntered: firstEntered, firstDone: firstDone}
		s2 := service.NewDramaService(service.DramaDeps{
			Repo: r2, Assets: a2, Probes: probes2, Compose: swComp, PreviewSecret: "supersede-secret",
		})
		t.Cleanup(s2.ClosePreviews)
		var mu sync.Mutex
		mockNow := time.Unix(1_700_000_000, 0)
		s2.SetPreviewTiming(10*time.Minute, 10*time.Minute, time.Minute, func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return mockNow
		})
		m2, err := s2.ResolveEpisodeManifest(context.Background(), 7, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		req2 := &model.DramaPreviewReq{SegmentVersion: m2.SegmentVersion, ManifestHash: m2.ManifestHash, Variant: "clean"}
		t1, _, err := s2.CreateEpisodePreview(context.Background(), 7, 1, 1, req2)
		if err != nil {
			t.Fatal(err)
		}
		<-firstEntered
		// 推进逻辑时钟跳过旧票 TTL，重复 POST 覆盖旧票并立即取消旧渲染任务。
		mu.Lock()
		mockNow = mockNow.Add(11 * time.Minute)
		mu.Unlock()
		t2, cookie2, err := s2.CreateEpisodePreview(context.Background(), 7, 1, 1, req2)
		if err != nil || t2.Ticket == t1.Ticket || t2.Status != "pending" {
			t.Fatalf("过期后重 POST 应覆盖并建立新票: err=%v t1=%+v t2=%+v", err, t1, t2)
		}
		select {
		case <-firstDone:
		case <-time.After(time.Second):
			t.Fatal("重 POST 覆盖过期旧票时未取消旧渲染任务")
		}
		ready2 := waitPreviewReady(context.Background(), t, s2, 7, 1, 1, t2.Ticket)
		tok2 := strings.Split(ready2.StreamURL, "token=")[1]
		f2, err := s2.OpenEpisodePreview(context.Background(), 1, 1, t2.Ticket, cookie2, tok2)
		if err != nil {
			t.Fatalf("新票据预览应可正常打开播放: %v", err)
		}
		_ = f2.Close()
	})
}

type previewCountingOpenAssets struct {
	*manifestFiles
	openCalls *atomic.Int32
}

func (a *previewCountingOpenAssets) Open(ctx context.Context, user, id uint64) (*provider.AssetFile, error) {
	a.openCalls.Add(1)
	return a.manifestFiles.Open(ctx, user, id)
}

type failingManifestProcessor struct {
	composeProcessor
	err error
}

func (p *failingManifestProcessor) ComposeManifest(context.Context, videoframe.ComposeManifestInput) error {
	return p.err
}

type hookedFailManifestProcessor struct {
	composeProcessor
	beforeErr func()
	err       error
	calls     atomic.Int32
}

func (p *hookedFailManifestProcessor) ComposeManifest(context.Context, videoframe.ComposeManifestInput) error {
	if p.calls.Add(1) == 1 && p.beforeErr != nil {
		p.beforeErr()
	}
	return p.err
}

type slotGateComposer struct {
	composeProcessor
	gate     chan struct{}
	calls    *atomic.Int32
	entered  atomic.Int32
	canceled chan struct{}
}

func (c *slotGateComposer) ComposeManifest(ctx context.Context, in videoframe.ComposeManifestInput) error {
	c.calls.Add(1)
	c.entered.Add(1)
	select {
	case <-c.gate:
		return os.WriteFile(in.Outputs[0].Destination, []byte("ok"), 0600)
	case <-ctx.Done():
		select {
		case c.canceled <- struct{}{}:
		default:
		}
		return ctx.Err()
	}
}

type supersedeComposer struct {
	composeProcessor
	seq          atomic.Int32
	firstEntered chan struct{}
	firstDone    chan struct{}
}

func (c *supersedeComposer) ComposeManifest(ctx context.Context, in videoframe.ComposeManifestInput) error {
	if c.seq.Add(1) == 1 {
		close(c.firstEntered)
		<-ctx.Done()
		close(c.firstDone)
		return ctx.Err()
	}
	return os.WriteFile(in.Outputs[0].Destination, []byte("fresh-preview"), 0600)
}
