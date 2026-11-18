package service_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/jsonhash"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/provider"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
)

type manifestFiles struct {
	*composeFiles
	sha  string
	body string
}

func (a *manifestFiles) Get(_ context.Context, user, id uint64) (*model.Asset, error) {
	if user != 7 || id != 30 {
		return nil, repository.ErrNotFound
	}
	return &model.Asset{ID: id, UserID: user, Kind: model.KindVideo, Sha256: a.sha}, nil
}

func (a *manifestFiles) Open(ctx context.Context, user, id uint64) (*provider.AssetFile, error) {
	if a.body == "" {
		return a.composeFiles.Open(ctx, user, id)
	}
	if a.downloadErr != nil {
		return nil, a.downloadErr
	}
	if user != 7 || id != 30 {
		return nil, provider.ErrAssetNotFound
	}
	return &provider.AssetFile{
		Asset: &model.Asset{ID: id, UserID: user, Kind: model.KindVideo, Sha256: a.sha},
		Body:  io.NopCloser(strings.NewReader(a.body)),
	}, nil
}

type manifestAudioProbeKey struct {
	id  uint64
	sha string
}

type manifestProbes struct {
	mu    sync.Mutex
	rows  map[string]*model.AssetMediaProbe
	audio map[manifestAudioProbeKey]*model.AssetMediaProbe
	calls int
}

func (p *manifestProbes) Get(_ context.Context, id uint64, sha string, version int16) (*model.AssetMediaProbe, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if version == videoframe.NormalizedAudioProbeVersion {
		if row := p.audio[manifestAudioProbeKey{id, sha}]; row != nil {
			return row, nil
		}
		return nil, repository.ErrNotFound
	}
	if row := p.rows[sha]; row != nil {
		return row, nil
	}
	return nil, repository.ErrNotFound
}
func (p *manifestProbes) Upsert(_ context.Context, row *model.AssetMediaProbe) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if row.ProbeVersion == videoframe.NormalizedAudioProbeVersion {
		if p.audio == nil {
			p.audio = make(map[manifestAudioProbeKey]*model.AssetMediaProbe)
		}
		p.audio[manifestAudioProbeKey{row.AssetID, row.Sha256}] = row
	} else {
		p.rows[row.Sha256] = row
	}
	return nil
}
func manifestService(r *composeMemory) (*service.DramaService, *manifestFiles, *manifestProbes) {
	a := &manifestFiles{composeFiles: &composeFiles{r: r}, sha: verifiedVideoHash()}
	probes := &manifestProbes{rows: map[string]*model.AssetMediaProbe{}}
	s := service.NewDramaService(service.DramaDeps{Repo: r, Assets: a, Probes: probes, Compose: &composeProcessor{}, ProbeVideo: func(context.Context, string) (videoframe.NormalizedProbe, error) {
		return videoframe.NormalizedProbe{FpsNum: 24, FpsDen: 1, NormalizedFrames24: 120}, nil
	}})
	return s, a, probes
}
func TestDramaManifest_TrimHashCacheAndSchemaIsolation(t *testing.T) {
	r := newComposeMemory()
	r.episodes[0].EditSchema = 1
	out := int32(43)
	r.segments[0].SourceInF = 24
	r.segments[0].SourceOutF = &out
	s, a, probes := manifestService(r)
	ctx := context.Background()
	v, err := s.ResolveEpisodeManifest(ctx, 7, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if v.Manifest.TotalFrames != 19 || v.Manifest.Clips[0].SourceOutF != 43 || v.Manifest.Program.Version != 1 || len(v.Manifest.Program.Audio) != 0 || len(v.SkippedSeqs) != 1 || len(v.ManifestHash) != 64 {
		t.Fatalf("清单不符: %+v", v)
	}
	again, err := s.ResolveEpisodeManifest(ctx, 7, 1, 1)
	if err != nil || again.ManifestHash != v.ManifestHash || probes.calls != 1 {
		t.Fatalf("缓存或稳定指纹失败: %+v %v", again, err)
	}
	r.episodes[0].EditSchema = 0
	again, err = s.ResolveEpisodeManifest(ctx, 7, 1, 1)
	if err != nil || again.ManifestHash != v.ManifestHash {
		t.Fatalf("schema=0/1 空包不一致: %v", err)
	}
	a.body = "video-b"
	a.sha = jsonhash.SHA256Hex([]byte(a.body))
	again, err = s.ResolveEpisodeManifest(ctx, 7, 1, 1)
	if err != nil || again.ManifestHash == v.ManifestHash || probes.calls != 2 {
		t.Fatalf("内容变化未失效缓存 %v", err)
	}
	out = 121
	_, err = s.ResolveEpisodeManifest(ctx, 7, 1, 1)
	if err == nil {
		t.Fatal("越界未拒绝")
	}
}

type asyncWarmupFiles struct {
	*composeFiles
}

func asyncWarmupVideoBody(id uint64) string {
	if id == 30 {
		return "video"
	}
	return fmt.Sprintf("video-%d", id)
}

func (a *asyncWarmupFiles) Open(_ context.Context, user, id uint64) (*provider.AssetFile, error) {
	if user != 7 || id < 30 || id > 39 {
		return nil, provider.ErrAssetNotFound
	}
	body := asyncWarmupVideoBody(id)
	return &provider.AssetFile{
		Asset: &model.Asset{ID: id, UserID: user, Kind: model.KindVideo, Sha256: jsonhash.SHA256Hex([]byte(body))},
		Body:  io.NopCloser(strings.NewReader(body)),
	}, nil
}

func (a *asyncWarmupFiles) Get(_ context.Context, user, id uint64) (*model.Asset, error) {
	if user != 7 || id < 30 || id > 39 {
		return nil, repository.ErrNotFound
	}
	return &model.Asset{ID: id, UserID: user, Kind: model.KindVideo, Sha256: jsonhash.SHA256Hex([]byte(asyncWarmupVideoBody(id)))}, nil
}

func newAsyncWarmupFixture() (*composeMemory, *manifestProbes) {
	r := newComposeMemory()
	r.episodes[0].EditSchema = 1
	r.episodes[0].SegmentVersion = 1
	for no := 2; no <= 7; no++ {
		epID := uint64(no + 2)
		taskID := uint64(20 + no - 1)
		assetID := uint64(30 + no - 1)
		segID := uint64(5 + no)
		r.episodes = append(r.episodes, model.DramaEpisode{
			BaseModel: model.BaseModel{ID: epID}, UserID: 7, DramaID: 1, No: no,
			Revision: 1, LockedAt: r.episodes[0].LockedAt, EditSchema: 1, SegmentVersion: 1,
		})
		t := r.tasks[0]
		t.ID = taskID
		t.TaskRef = fmt.Sprintf("t%d-ref", taskID)
		t.IdempotencyKey = fmt.Sprintf("k-t%d", taskID)
		t.NodeID = "seg:" + idcodec.Encode(segID)
		t.OutputJSON = []byte(fmt.Sprintf(`[{"asset_id":%d,"media_type":"video"}]`, assetID))
		r.tasks = append(r.tasks, t)
		chosen := taskID
		r.segments = append(r.segments, model.DramaSegment{
			BaseModel: model.BaseModel{ID: segID}, UserID: 7, EpisodeID: epID, Seq: 0, ChosenTaskID: &chosen,
		})
	}
	return r, &manifestProbes{rows: map[string]*model.AssetMediaProbe{}}
}

func waitManifestReadyOrErr(t *testing.T, s *service.DramaService, no int) (*model.DramaManifestView, error) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		view, prep, err := s.PrepareOrResolveEpisodeManifest(ctx, 7, 1, no)
		if err != nil {
			return nil, err
		}
		if view != nil {
			return view, nil
		}
		if prep == nil || prep.Status != "preparing" {
			t.Fatalf("非预期返回 view=nil prep=%+v", prep)
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("等待分集 %d 清单预热结果超时", no)
	return nil, nil
}

func TestDramaManifest_PrepareOrResolveAsyncWarmupAndErrorOnce(t *testing.T) {
	t.Run("cold_sem_saturated_dedup_and_cached_parity", func(t *testing.T) {
		r, probes := newAsyncWarmupFixture()
		gate := make(chan struct{})
		entered := make(chan struct{}, 8)
		var probeCalls atomic.Int32
		s := service.NewDramaService(service.DramaDeps{
			Repo: r, Assets: &asyncWarmupFiles{composeFiles: &composeFiles{r: r}}, Probes: probes,
			ProbeVideo: func(ctx context.Context, _ string) (videoframe.NormalizedProbe, error) {
				probeCalls.Add(1)
				entered <- struct{}{}
				select {
				case <-gate:
					return videoframe.NormalizedProbe{FpsNum: 24, FpsDen: 1, NormalizedFrames24: 120}, nil
				case <-ctx.Done():
					return videoframe.NormalizedProbe{}, ctx.Err()
				}
			},
		})
		t.Cleanup(s.ClosePreviews)
		ctx := context.Background()

		// 占满 4 个 preview.sem 并发槽位。
		for no := 2; no <= 5; no++ {
			v, prep, err := s.PrepareOrResolveEpisodeManifest(ctx, 7, 1, no)
			if err != nil || v != nil || prep == nil || prep.Status != "preparing" {
				t.Fatalf("冷启动分集 %d 应返回 preparing: v=%v prep=%+v err=%v", no, v, prep, err)
			}
		}
		for i := 0; i < 4; i++ {
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatalf("信号量槽位 %d 未进入探测", i)
			}
		}

		// 第 1 集在信号量全满时仍必须立即返回 202 preparing，且并发重入命中去重。
		start := time.Now()
		v1, prep1, err := s.PrepareOrResolveEpisodeManifest(ctx, 7, 1, 1)
		if err != nil || v1 != nil || prep1 == nil || prep1.Status != "preparing" || time.Since(start) > 250*time.Millisecond {
			t.Fatalf("信号量满载时第 1 集应非阻塞返回 preparing: cost=%v v=%v prep=%+v err=%v", time.Since(start), v1, prep1, err)
		}
		if prep1.SegmentVersion != 1 || len(prep1.SkippedSeqs) != 1 || prep1.SkippedSeqs[0] != 1 || prep1.Diagnostics == nil {
			t.Fatalf("202 preparing 元数据不符合预期: %+v", prep1)
		}
		vDup, prepDup, err := s.PrepareOrResolveEpisodeManifest(ctx, 7, 1, 1)
		if err != nil || vDup != nil || prepDup == nil || prepDup.Status != "preparing" {
			t.Fatalf("重复请求应复用在途预热: v=%v prep=%+v err=%v", vDup, prepDup, err)
		}

		close(gate)
		readyView, err := waitManifestReadyOrErr(t, s, 1)
		if err != nil || readyView == nil {
			t.Fatalf("预热完成后应返回完整清单: view=%+v err=%v", readyView, err)
		}
		syncView, err := s.ResolveEpisodeManifest(ctx, 7, 1, 1)
		if err != nil || readyView.ManifestHash != syncView.ManifestHash {
			t.Fatalf("PrepareOrResolve 与同步 ResolveEpisodeManifest 不一致: ready=%+v sync=%+v err=%v", readyView, syncView, err)
		}
		if got := probeCalls.Load(); got != 5 {
			t.Fatalf("并发去重失败，预期 5 次探测，实际 %d", got)
		}
	})

	t.Run("failure_surfaced_once_then_retry_succeeds", func(t *testing.T) {
		r, probes := newAsyncWarmupFixture()
		var shouldFail atomic.Bool
		shouldFail.Store(true)
		s := service.NewDramaService(service.DramaDeps{
			Repo: r, Assets: &asyncWarmupFiles{composeFiles: &composeFiles{r: r}}, Probes: probes,
			ProbeVideo: func(context.Context, string) (videoframe.NormalizedProbe, error) {
				if shouldFail.Load() {
					return videoframe.NormalizedProbe{}, errors.New("codec corrupt")
				}
				return videoframe.NormalizedProbe{FpsNum: 24, FpsDen: 1, NormalizedFrames24: 120}, nil
			},
		})
		t.Cleanup(s.ClosePreviews)
		ctx := context.Background()

		_, prep, err := s.PrepareOrResolveEpisodeManifest(ctx, 7, 1, 6)
		if err != nil || prep == nil || prep.Status != "preparing" {
			t.Fatalf("首次请求应启动异步预热: prep=%+v err=%v", prep, err)
		}
		if _, err = waitManifestReadyOrErr(t, s, 6); err == nil {
			t.Fatal("后台探测失败后后续轮询必须显露错误而非永久卡在 preparing")
		}

		// 错误只消费一次；恢复探测后下一次请求可重新发起并最终就绪。
		shouldFail.Store(false)
		v2, prep2, err := s.PrepareOrResolveEpisodeManifest(ctx, 7, 1, 6)
		if err != nil || v2 != nil || prep2 == nil || prep2.Status != "preparing" {
			t.Fatalf("错误消费后应允许重新发起预热: v=%v prep=%+v err=%v", v2, prep2, err)
		}
		readyView, err := waitManifestReadyOrErr(t, s, 6)
		if err != nil || readyView == nil {
			t.Fatalf("重试预热应成功返回清单: view=%+v err=%v", readyView, err)
		}
	})

	t.Run("close_previews_cancels_cleanly", func(t *testing.T) {
		r, probes := newAsyncWarmupFixture()
		entered := make(chan struct{}, 1)
		canceled := make(chan struct{}, 1)
		s := service.NewDramaService(service.DramaDeps{
			Repo: r, Assets: &asyncWarmupFiles{composeFiles: &composeFiles{r: r}}, Probes: probes,
			ProbeVideo: func(ctx context.Context, _ string) (videoframe.NormalizedProbe, error) {
				entered <- struct{}{}
				<-ctx.Done()
				canceled <- struct{}{}
				return videoframe.NormalizedProbe{}, ctx.Err()
			},
		})
		if _, prep, err := s.PrepareOrResolveEpisodeManifest(context.Background(), 7, 1, 7); err != nil || prep == nil {
			t.Fatalf("启动预热失败: prep=%+v err=%v", prep, err)
		}
		<-entered
		done := make(chan struct{})
		go func() {
			s.ClosePreviews()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("ClosePreviews 未能取消并回收在途清单预热 goroutine")
		}
		select {
		case <-canceled:
		default:
			t.Fatal("ClosePreviews 未取消后台探测上下文")
		}
	})
}

func newBulkWarmupFixture(total int) (*composeMemory, *manifestProbes) {
	r := newComposeMemory()
	r.episodes = make([]model.DramaEpisode, 0, total)
	r.segments = make([]model.DramaSegment, 0, total)
	baseTask := r.tasks[0]
	r.tasks = r.tasks[:0]
	now := time.Unix(1700000000, 0)
	for no := 1; no <= total; no++ {
		epID := uint64(100 + no)
		segID := uint64(1000 + no)
		taskID := uint64(2000 + no)
		r.episodes = append(r.episodes, model.DramaEpisode{
			BaseModel: model.BaseModel{ID: epID}, UserID: 7, DramaID: 1, No: no,
			Revision: 1, LockedAt: &now, EditSchema: 1, SegmentVersion: 1,
		})
		t := baseTask
		t.ID = taskID
		t.TaskRef = fmt.Sprintf("bulk-t%d", taskID)
		t.IdempotencyKey = fmt.Sprintf("bulk-k%d", taskID)
		t.NodeID = "seg:" + idcodec.Encode(segID)
		t.OutputJSON = []byte(`[{"asset_id":30,"media_type":"video"}]`)
		r.tasks = append(r.tasks, t)
		chosen := taskID
		r.segments = append(r.segments, model.DramaSegment{
			BaseModel: model.BaseModel{ID: segID}, UserID: 7, EpisodeID: epID, Seq: 0, ChosenTaskID: &chosen,
		})
	}
	return r, &manifestProbes{rows: map[string]*model.AssetMediaProbe{}}
}

func TestDramaManifest_ManifestErrPruneExpiredAndEvictOldestOnly(t *testing.T) {
	ctx := context.Background()
	newWarmupSvc := func(total int, nowFn func() time.Time) *service.DramaService {
		r, probes := newBulkWarmupFixture(total)
		s := service.NewDramaService(service.DramaDeps{
			Repo: r, Assets: &asyncWarmupFiles{composeFiles: &composeFiles{r: r}}, Probes: probes,
			ProbeVideo: func(context.Context, string) (videoframe.NormalizedProbe, error) {
				return videoframe.NormalizedProbe{}, errors.New("probe boom")
			},
		})
		s.SetPreviewTiming(5*time.Second, time.Minute, time.Minute, nowFn)
		t.Cleanup(s.ClosePreviews)
		return s
	}
	triggerBatch := func(s *service.DramaService, from, to int) {
		for no := from; no <= to; no++ {
			v, prep, err := s.PrepareOrResolveEpisodeManifest(ctx, 7, 1, no)
			if err != nil || v != nil || prep == nil || prep.Status != "preparing" {
				t.Fatalf("episode %d 应启动预热: v=%v prep=%+v err=%v", no, v, prep, err)
			}
		}
		s.WaitManifestWarmupsForTest()
	}

	t.Run("prunes_expired_before_evicting_valid", func(t *testing.T) {
		var mu sync.Mutex
		now := time.Unix(1_700_000_000, 0)
		nowFn := func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		}
		s := newWarmupSvc(258, nowFn)
		for start := 1; start <= 255; start += 64 {
			triggerBatch(s, start, min(start+63, 255))
		}
		mu.Lock()
		now = now.Add(61 * time.Second)
		mu.Unlock()
		triggerBatch(s, 256, 256)
		mu.Lock()
		now = now.Add(time.Second)
		mu.Unlock()
		triggerBatch(s, 257, 257)

		for _, no := range []int{256, 257} {
			if _, _, err := s.PrepareOrResolveEpisodeManifest(ctx, 7, 1, no); err == nil {
				t.Fatalf("未过期的有效调用方 episode %d 不应丢失单次失败通知", no)
			}
		}
		// 过期条目应已被清理，再次请求返回 preparing 而非陈旧报错。
		v, prep, err := s.PrepareOrResolveEpisodeManifest(ctx, 7, 1, 1)
		if err != nil || v != nil || prep == nil || prep.Status != "preparing" {
			t.Fatalf("已过期条目应重新发起预热: v=%v prep=%+v err=%v", v, prep, err)
		}
		s.WaitManifestWarmupsForTest()
	})

	t.Run("evicts_only_oldest_when_all_fresh", func(t *testing.T) {
		var mu sync.Mutex
		now := time.Unix(1_700_010_000, 0)
		nowFn := func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		}
		s := newWarmupSvc(257, nowFn)
		// 第 1 集最早失败；随后 255 集填满 256 上限；第 257 集插入时仅淘汰第 1 集。
		triggerBatch(s, 1, 1)
		mu.Lock()
		now = now.Add(time.Second)
		mu.Unlock()
		for start := 2; start <= 256; start += 64 {
			triggerBatch(s, start, min(start+63, 256))
		}
		mu.Lock()
		now = now.Add(time.Second)
		mu.Unlock()
		triggerBatch(s, 257, 257)

		// 第 2..257 集全部 256 个有效失败都必须完整保留。
		for no := 2; no <= 257; no++ {
			if _, _, err := s.PrepareOrResolveEpisodeManifest(ctx, 7, 1, no); err == nil {
				t.Fatalf("仅应淘汰最早一条，第 %d 集单次失败被误清除", no)
			}
		}
		// 第 1 集作为唯一最早条目已被淘汰，再次请求会重新进入 preparing。
		v, prep, err := s.PrepareOrResolveEpisodeManifest(ctx, 7, 1, 1)
		if err != nil || v != nil || prep == nil || prep.Status != "preparing" {
			t.Fatalf("最早第 1 集应已被淘汰并允许重建: v=%v prep=%+v err=%v", v, prep, err)
		}
		s.WaitManifestWarmupsForTest()
	})
}

func TestDramaManifestV2MovesAnchorsByIDAndKeepsAbsoluteTiming(t *testing.T) {
	ctx := context.Background()
	r, s := newProgramFixture(t, nil)
	r.segments[1].VisualKind = model.DramaVisualBlack
	r.segments[1].SourceOutF = new(int32(24))
	r.segments[1].FXJSON = []byte(`{"fade_in_f":2}`)
	req := anchoredProgram(0, 5, 10)
	req.Program.Audio = append(req.Program.Audio, absoluteProgramCue("music", model.DramaAudioBGM, 3, 36))
	if _, err := s.PutProgram(ctx, 7, 1, 1, req); err != nil {
		t.Fatal(err)
	}
	assertV2AnchorPlacement(t, s, 10, 20)
	r.segments[0].Seq, r.segments[1].Seq = 1, 0
	r.episodes[0].SegmentVersion++
	assertV2AnchorPlacement(t, s, 34, 44)

	r.segments[1].VisualKind = model.DramaVisualStill
	r.segments[1].StillAssetID = new(uint64(32))
	m, err := s.ResolveEpisodeManifestV2(ctx, 7, 1, 1)
	if err != nil || m.Manifest.TotalFrames != 144 || m.Manifest.Clips[0].TaskID != 0 || m.Manifest.Clips[0].VisualKind != "still" ||
		*m.Manifest.Clips[0].StillAssetID != 32 || m.Manifest.Clips[0].FX.FadeInF != 2 || m.Manifest.Clips[1].TaskID != 20 {
		t.Fatalf("still plus video timeline: %+v %v", m, err)
	}
	r.segments[0].SourceOutF = new(int32(10)) // SourceOutF is exclusive.
	assertV2CueDiagnostic(t, s, "hit", "anchor_frame_trimmed")
	r.segments[0].SourceOutF = nil
	assertV2AnchorPlacement(t, s, 34, 44)

	// A new clip with the same seq must not receive the old segment's anchor.
	saved := r.segments[0]
	r.segments[0] = model.DramaSegment{BaseModel: model.BaseModel{ID: 8}, EpisodeID: 3, UserID: 7, Seq: 1,
		VisualKind: model.DramaVisualBlack, SourceOutF: new(int32(120))}
	assertV2CueDiagnostic(t, s, "hit", "anchor_segment_missing")
	r.segments[0] = saved
	assertV2AnchorPlacement(t, s, 34, 44)
}

func assertV2AnchorPlacement(t *testing.T, s *service.DramaService, wantStart, wantEnd int32) {
	t.Helper()
	m, err := s.ResolveEpisodeManifestV2(context.Background(), 7, 1, 1)
	if err != nil || len(m.ManifestHash) != 64 {
		t.Fatalf("expected valid V2: %+v %v", m, err)
	}
	audio := m.Manifest.Program.Audio
	if len(audio) != 3 || audio[0].Anchor.SegmentID != 5 || *audio[0].StartF != wantStart || *audio[0].EndF != wantEnd ||
		*audio[1].StartF != 0 || *audio[1].EndF != 12 || *audio[2].StartF != 3 || *audio[2].EndF != 36 {
		t.Fatalf("unexpected cue placement: %+v", audio)
	}
}

func assertV2CueDiagnostic(t *testing.T, s *service.DramaService, id, reason string) {
	t.Helper()
	m, err := s.ResolveEpisodeManifestV2(context.Background(), 7, 1, 1)
	if err != nil || m.ManifestHash != "" || len(m.Diagnostics) == 0 {
		t.Fatalf("expected invalid V2: %+v %v", m, err)
	}
	for _, d := range m.ProgramDiagnostics {
		if d.CueID == id && d.Reason == reason {
			return
		}
	}
	t.Fatalf("missing %s diagnostic for %s: %+v", reason, id, m.ProgramDiagnostics)
}

func TestDramaManifestV2DiagnosesBoundsAndUnavailableAnchors(t *testing.T) {
	ctx := context.Background()
	r, s := newProgramFixture(t, nil)
	req := anchoredProgram(0, 5, 115)
	req.Program.Audio = append(req.Program.Audio, absoluteProgramCue("music", model.DramaAudioBGM, 0, 121))
	if _, err := s.PutProgram(ctx, 7, 1, 1, req); err != nil {
		t.Fatal(err)
	}
	assertV2CueDiagnostic(t, s, "hit", "cue_out_of_episode")
	assertV2CueDiagnostic(t, s, "music", "cue_out_of_episode")

	r.segments[0].ChosenTaskID = nil
	assertV2CueDiagnostic(t, s, "hit", "anchor_segment_unready")
	r.segments[0].VisualKind = model.DramaVisualBlack
	r.segments[0].SourceOutF = new(int32(120))
	assertV2CueDiagnostic(t, s, "hit", "anchor_non_video")
	req.RebindAnchorCueIDs = []string{"hit"}
	req.ExpectedSegmentVersion = new(int64(1))
	_, err := s.PutProgram(ctx, 7, 1, 1, req)
	requireProgramError(t, err, 91010, "anchor_non_video")
}

func TestDramaManifestV2ValidatesTimelineLimit(t *testing.T) {
	r, s := newProgramFixture(t, nil)
	r.segments[0].VisualKind = model.DramaVisualBlack
	r.segments[0].ChosenTaskID = nil
	r.segments[0].SourceOutF = new(int32(model.DramaProgramMaxFrames + 1))
	assertV2CueDiagnostic(t, s, "", "timeline_limit")
}
