package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/pkg/ws"
	"video-canvas/internal/provider"
	"video-canvas/internal/service"
	"video-canvas/internal/service/dramasegmentfake"
)

func TestWaveB_SegmentVersionSingleBumpPerTx(t *testing.T) {
	ctx := context.Background()
	clock := time.Unix(1_700_000_000, 0)
	db, userID, _, _, segSvc, assets := segEditPGEnv(t, &clock)
	p, ep, seg1, _, _ := seedDramaEpisodeWithTakes(t, db, assets, userID, 0, true)

	assertDBVersion := func(want int64) {
		t.Helper()
		var got model.DramaEpisode
		if err := db.First(&got, ep.ID).Error; err != nil {
			t.Fatal(err)
		}
		if got.SegmentVersion != want {
			t.Fatalf("episode.segment_version=%d want=%d", got.SegmentVersion, want)
		}
	}

	assertDBVersion(1)

	// 1. 插入到首位之后（移动后续或新建），应只递增 +1。
	seg2, err := segSvc.Create(ctx, userID, p.ID, 1, &model.CreateDramaSegmentReq{Prompt: "第二段"})
	if err != nil {
		t.Fatal(err)
	}
	assertDBVersion(2)
	if seg2.SegmentVersion != 2 {
		t.Fatalf("Create 返回 segment_version=%d want=2", seg2.SegmentVersion)
	}
	seg3, err := segSvc.Create(ctx, userID, p.ID, 1, &model.CreateDramaSegmentReq{Prompt: "第三段"})
	if err != nil {
		t.Fatal(err)
	}
	assertDBVersion(3)
	// 插在 seg1（seq=0）之后，顺移原 seg2/seg3 两位但只能单次递增 +1（3 -> 4）。
	afterZero := 0
	segMid, err := segSvc.Create(ctx, userID, p.ID, 1, &model.CreateDramaSegmentReq{Prompt: "中间段", AfterSeq: &afterZero})
	if err != nil {
		t.Fatal(err)
	}
	assertDBVersion(4)
	if segMid.SegmentVersion != 4 || segMid.Seq != 1 {
		t.Fatalf("插入中间段返回异常: %+v", segMid)
	}

	// 2. 重排全部 4 个片段，只递增一次 +1（4 -> 5）；相同顺序不递增（仍为 5）。
	ordered, err := segSvc.Order(ctx, userID, p.ID, 1, &model.OrderDramaSegmentsReq{
		IDs: []idcodec.ID{idcodec.ID(seg3.ID), idcodec.ID(segMid.ID), idcodec.ID(seg2.ID), idcodec.ID(seg1.ID)},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDBVersion(5)
	if len(ordered) != 4 || ordered[0].SegmentVersion != 5 {
		t.Fatalf("Order 返回版本异常: %+v", ordered)
	}
	sameOrder, err := segSvc.Order(ctx, userID, p.ID, 1, &model.OrderDramaSegmentsReq{
		IDs: []idcodec.ID{idcodec.ID(seg3.ID), idcodec.ID(segMid.ID), idcodec.ID(seg2.ID), idcodec.ID(seg1.ID)},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDBVersion(5)
	if sameOrder[0].SegmentVersion != 5 {
		t.Fatalf("相同顺序不应递增版本: %+v", sameOrder[0])
	}

	// 3. 删除首位片段并重编号后 3 个片段，只递增一次 +1（5 -> 6）。
	if err := segSvc.Delete(ctx, userID, uint64(seg3.ID)); err != nil {
		t.Fatal(err)
	}
	assertDBVersion(6)

	// 4. 在 edit_schema=0 下以默认精裁参数 PATCH，应升级为 edit_schema=1 且递增 +1（6 -> 7）。
	views, err := segSvc.List(ctx, userID, p.ID, 1)
	if err != nil || len(views) != 3 {
		t.Fatalf("删除后列表异常: err=%v views=%+v", err, views)
	}
	zero := int32(0)
	patched, err := segSvc.Update(ctx, userID, seg1.ID, &model.UpdateDramaSegmentReq{
		Revision: views[2].Revision,
		Edit:     &model.DramaSegmentEditPatch{SourceInF: &zero},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDBVersion(7)
	if patched.SegmentVersion != 7 {
		t.Fatalf("PATCH 返回 segment_version=%d want=7", patched.SegmentVersion)
	}
	var refreshedEp model.DramaEpisode
	if err := db.First(&refreshedEp, ep.ID).Error; err != nil || refreshedEp.EditSchema != 1 {
		t.Fatalf("edit_schema 未升级为 1: ep=%+v err=%v", refreshedEp, err)
	}

	// 5. 仅修改 prompt 不改物料与顺序时，版本不递增（仍为 7）。
	newPrompt := "只改文案"
	promptOnly, err := segSvc.Update(ctx, userID, seg1.ID, &model.UpdateDramaSegmentReq{
		Revision: patched.Revision,
		Prompt:   &newPrompt,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDBVersion(7)
	if promptOnly.SegmentVersion != 7 {
		t.Fatalf("仅改文案不应递增 segment_version: %d", promptOnly.SegmentVersion)
	}
}

func TestWaveB_AspectRatioPortraitComposeAndPreview(t *testing.T) {
	ctx := context.Background()
	t.Run("compose honors project portrait ratio", func(t *testing.T) {
		r := newComposeMemory()
		r.project.Ratio = "9:16"
		r.episodes[0].EditSchema = 1
		r.episodes[0].SegmentVersion = 2
		s, p := previewService(r)
		t.Cleanup(s.ClosePreviews)

		view, err := s.ResolveEpisodeManifest(ctx, 7, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			size  string
			wantW int
			wantH int
		}{
			{size: "720p", wantW: 720, wantH: 1280},
			{size: "1080p", wantW: 1080, wantH: 1920},
		} {
			r.jobs = nil
			req := composeReq(t, `,"compose_request_key":"ratio-`+tc.size+`","compose":{"segment_version":2,"manifest_hash":"`+view.ManifestHash+`","size":"`+tc.size+`","variants":["clean"]}`)
			job, err := s.CreateJob(ctx, 7, 1, req)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.HandleDrama(ctx, 7, 1, uint64(job.ID)); err != nil {
				t.Fatal(err)
			}
			in := p.lastInput()
			if len(in.Outputs) != 1 || in.Outputs[0].Width != tc.wantW || in.Outputs[0].Height != tc.wantH || in.Outputs[0].MaxEdge != tc.wantH {
				t.Fatalf("%s 渲染尺寸错误: %+v", tc.size, in.Outputs)
			}
			jobs, err := s.ListJobs(ctx, 7, 1, false)
			if err != nil {
				t.Fatal(err)
			}
			exp := jobs[0].Result.Exports[0]
			if exp.Width != tc.wantW || exp.Height != tc.wantH {
				t.Fatalf("%s 导出尺寸错误: %+v", tc.size, exp)
			}
		}
	})

	t.Run("preview honors ratio and isolates ratio cache", func(t *testing.T) {
		r := newComposeMemory()
		r.project.Ratio = "9:16"
		r.episodes[0].EditSchema = 1
		s, p := previewService(r)
		t.Cleanup(s.ClosePreviews)

		m, err := s.ResolveEpisodeManifest(ctx, 7, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		req := &model.DramaPreviewReq{SegmentVersion: m.SegmentVersion, ManifestHash: m.ManifestHash, Variant: "clean"}
		v1, _, err := s.CreateEpisodePreview(ctx, 7, 1, 1, req)
		if err != nil {
			t.Fatal(err)
		}
		_ = waitPreviewReady(ctx, t, s, 7, 1, 1, v1.Ticket)
		out1 := p.lastInput().Outputs[0]
		if out1.Width != 720 || out1.Height != 1280 || out1.MaxEdge != 1280 || p.manifestCalls() != 1 {
			t.Fatalf("9:16 预览尺寸错误: out=%+v calls=%d", out1, p.manifestCalls())
		}

		r.project.Ratio = "3:4"
		v2, _, err := s.CreateEpisodePreview(ctx, 7, 1, 1, req)
		if err != nil {
			t.Fatal(err)
		}
		_ = waitPreviewReady(ctx, t, s, 7, 1, 1, v2.Ticket)
		out2 := p.lastInput().Outputs[0]
		if v2.Ticket == v1.Ticket || out2.Width != 720 || out2.Height != 960 || out2.MaxEdge != 960 || p.manifestCalls() != 2 {
			t.Fatalf("改画幅后未隔离缓存或尺寸错误: v1=%s v2=%s out=%+v calls=%d", v1.Ticket, v2.Ticket, out2, p.manifestCalls())
		}
	})
}

func TestWaveB_AsyncPreviewAndCloseLifecycle(t *testing.T) {
	t.Run("returns pending immediately and refreshes ttl after http cancel", func(t *testing.T) {
		r := newComposeMemory()
		s, p := previewService(r)
		t.Cleanup(s.ClosePreviews)

		m, err := s.ResolveEpisodeManifest(context.Background(), 7, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		entered := make(chan struct{})
		release := make(chan struct{})
		p.duringManifest = func() {
			close(entered)
			<-release
		}

		reqCtx, reqCancel := context.WithCancel(context.Background())
		req := &model.DramaPreviewReq{SegmentVersion: m.SegmentVersion, ManifestHash: m.ManifestHash, Variant: "clean"}
		start := time.Now()
		v, cookie, err := s.CreateEpisodePreview(reqCtx, 7, 1, 1, req)
		elapsed := time.Since(start)
		if err != nil || elapsed > 200*time.Millisecond {
			t.Fatalf("创建预览应异步立即返回: err=%v elapsed=%v", err, elapsed)
		}
		if v.Status != "pending" || v.StreamURL != "" || cookie == "" {
			t.Fatalf("初始异步票据错误: %+v cookie=%q", v, cookie)
		}

		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("后台渲染协程未启动")
		}
		// 模拟客户端 HTTP 请求在 60s 写超时断开，后台预览必须继续完成并推进 expires_at。
		reqCancel()
		time.Sleep(1100 * time.Millisecond)
		close(release)

		ready := waitPreviewReady(context.Background(), t, s, 7, 1, 1, v.Ticket)
		if ready.ExpiresAt <= v.ExpiresAt {
			t.Fatalf("就绪后未刷新播放 TTL: initial=%d ready=%d", v.ExpiresAt, ready.ExpiresAt)
		}
		tok := strings.Split(ready.StreamURL, "token=")[1]
		f, err := s.OpenEpisodePreview(context.Background(), 1, 1, v.Ticket, cookie, tok)
		if err != nil {
			t.Fatalf("打开就绪预览流失败: %v", err)
		}
		_ = f.Close()
	})

	t.Run("ClosePreviews cancels inflight renders and waits cleanly", func(t *testing.T) {
		r := newComposeMemory()
		s, p := previewService(r)
		m, err := s.ResolveEpisodeManifest(context.Background(), 7, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		ctxCanceled := make(chan struct{})
		entered := make(chan struct{})
		p.duringManifest = func() {}
		closedSvc := service.NewDramaService(service.DramaDeps{
			Repo: r, Assets: &manifestFiles{composeFiles: &composeFiles{r: r}, sha: strings.Repeat("a", 64)},
			Probes:        &manifestProbes{rows: map[string]*model.AssetMediaProbe{strings.Repeat("a", 64): {AssetID: 30, Sha256: strings.Repeat("a", 64), ProbeVersion: 1, NormalizedFrames24: 120}}},
			Compose:       &cancelAwareComposer{entered: entered, done: ctxCanceled},
			PreviewSecret: "close-test-secret",
		})
		if _, _, err := closedSvc.CreateEpisodePreview(context.Background(), 7, 1, 1, &model.DramaPreviewReq{SegmentVersion: m.SegmentVersion, ManifestHash: m.ManifestHash, Variant: "clean"}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("渲染未进入等待")
		}
		closedSvc.ClosePreviews()
		select {
		case <-ctxCanceled:
		default:
			t.Fatal("ClosePreviews 未取消进行中的预览渲染或未等待退出")
		}
		if _, _, err := closedSvc.CreateEpisodePreview(context.Background(), 7, 1, 1, &model.DramaPreviewReq{SegmentVersion: m.SegmentVersion, ManifestHash: m.ManifestHash, Variant: "clean"}); err == nil {
			t.Fatal("关闭后不应接受新预览")
		}
	})
}

type cancelAwareComposer struct {
	composeProcessor
	entered chan struct{}
	done    chan struct{}
}

func (c *cancelAwareComposer) ComposeManifest(ctx context.Context, _ videoframe.ComposeManifestInput) error {
	close(c.entered)
	<-ctx.Done()
	close(c.done)
	return ctx.Err()
}

func TestWaveB_ReadDegradesOnProbeErrorWhileEditAndManifestStrict(t *testing.T) {
	ctx := context.Background()
	fakeRepo := dramasegmentfake.New()
	taskID, canvasID := uint64(31), uint64(9)
	fakeRepo.Rows = []model.DramaSegment{{BaseModel: model.BaseModel{ID: 11}, UserID: 1, EpisodeID: 10, Seconds: 5, Revision: 1, ChosenTaskID: &taskID}}
	fakeRepo.Tasks = []model.GenerationTask{{ID: taskID, UserID: 1, CanvasProjectID: &canvasID, NodeID: "seg:" + idcodec.Encode(11), Kind: model.KindVideo, Status: model.TaskSucceeded, OutputJSON: []byte(`[{"asset_id":17,"media_type":"video"}]`)}}
	assets := &segEditAssets{assets: map[uint64]*model.Asset{17: {ID: 17, UserID: 1, Kind: model.KindVideo, Sha256: strings.Repeat("a", 64), StorageKey: "video.mp4"}}}
	probeErr := errors.New("ffmpeg busy")
	segSvc := service.NewDramaSegmentService(service.DramaSegmentDeps{
		Repo: fakeRepo, Tasks: fakeRepo, Assets: assets, Probes: &segmentProbeCache{}, Broadcaster: ws.NopBroadcaster{},
		ProbeVideo: func(context.Context, string) (videoframe.NormalizedProbe, error) {
			return videoframe.NormalizedProbe{}, probeErr
		},
	})

	views, err := segSvc.List(ctx, 1, 7, 1)
	if err != nil || len(views) != 1 || views[0].SourceFrameCount != nil {
		t.Fatalf("探测失败时列表应降级为 SourceFrameCount=nil: err=%v views=%+v", err, views)
	}

	in := int32(5)
	if _, err := segSvc.Update(ctx, 1, 11, &model.UpdateDramaSegmentReq{
		Revision: 1,
		Edit:     &model.DramaSegmentEditPatch{SourceInF: &in},
	}); !errors.Is(err, probeErr) {
		t.Fatalf("编辑精裁必须严格拒绝探测失败，实际: %v", err)
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := segSvc.List(canceledCtx, 1, 7, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("请求已取消时不应吞掉 ctx.Err(): %v", err)
	}

	mem := newComposeMemory()
	mem.episodes[0].EditSchema = 1
	_, manifestAssets, emptyProbes := manifestService(mem)
	dramaSvc := service.NewDramaService(service.DramaDeps{
		Repo: mem, Assets: manifestAssets, Probes: emptyProbes, Compose: &composeProcessor{},
		ProbeVideo: func(context.Context, string) (videoframe.NormalizedProbe, error) {
			return videoframe.NormalizedProbe{}, probeErr
		},
	})
	if _, err := dramaSvc.ResolveEpisodeManifest(ctx, 7, 1, 1); err == nil {
		t.Fatal("生成清单必须严格拒绝探测失败")
	} else {
		dramaCheckCode(t, err, errcode.ErrDramaEditInvalid.Code)
	}
}

func TestWaveB_ManifestVersionSampleRateAndRenderGuard(t *testing.T) {
	ctx := context.Background()
	r := newComposeMemory()
	r.episodes[0].EditSchema = 1
	r.episodes[0].SegmentVersion = 4
	s, _ := previewService(r)
	t.Cleanup(s.ClosePreviews)

	view, err := s.ResolveEpisodeManifest(ctx, 7, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if view.Manifest.RenderVersion != model.CurrentDramaManifestRenderVersion ||
		view.Manifest.SampleRate != model.CurrentDramaManifestSampleRate ||
		view.Manifest.EpisodeID != r.episodes[0].ID ||
		view.Manifest.SegmentVersion != 4 ||
		len(view.Manifest.SkippedSeqs) != 1 || view.Manifest.SkippedSeqs[0] != 1 {
		t.Fatalf("清单契约字段不完整: %+v", view.Manifest)
	}

	r.episodes[0].SegmentVersion = 5
	changedVer, err := s.ResolveEpisodeManifest(ctx, 7, 1, 1)
	if err != nil || changedVer.ManifestHash == view.ManifestHash {
		t.Fatalf("segment_version 未纳入 manifest_hash: err=%v old=%s new=%s", err, view.ManifestHash, changedVer.ManifestHash)
	}

	// 提交冻结任务并分别篡改清单四项不变量：render_version / v / fps / sample_rate，执行渲染必须在下载素材前拒绝且不调用 ComposeManifest。
	sha := strings.Repeat("a", 64)
	baseAssets := &manifestFiles{composeFiles: &composeFiles{r: r}, sha: sha}
	countingAssets := &countingOpenAssets{manifestFiles: baseAssets}
	guardProc := &manifestProcessor{}
	guardSvc := service.NewDramaService(service.DramaDeps{
		Repo: r, Assets: countingAssets,
		Probes:  &manifestProbes{rows: map[string]*model.AssetMediaProbe{sha: {AssetID: 30, Sha256: sha, ProbeVersion: 1, NormalizedFrames24: 120}}},
		Compose: guardProc,
	})
	t.Cleanup(guardSvc.ClosePreviews)

	for _, tc := range []struct {
		key   string
		field string
		value int
	}{
		{key: "guard-rv", field: "render_version", value: 2},
		{key: "guard-v", field: "v", value: 2},
		{key: "guard-fps", field: "fps", value: 30},
		{key: "guard-sr", field: "sample_rate", value: 44100},
	} {
		r.jobs = nil
		req := composeReq(t, `,"compose_request_key":"`+tc.key+`","compose":{"segment_version":5,"manifest_hash":"`+changedVer.ManifestHash+`","size":"720p","variants":["clean"]}`)
		job, err := guardSvc.CreateJob(ctx, 7, 1, req)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(r.jobs[0].InputJSON, &payload); err != nil {
			t.Fatal(err)
		}
		manifestObj := payload["manifest"].(map[string]any)
		manifestObj[tc.field] = tc.value
		r.jobs[0].InputJSON, err = json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := guardSvc.HandleDrama(ctx, 7, 1, uint64(job.ID)); err != nil {
			t.Fatal(err)
		}
		if countingAssets.openCalls != 0 || guardProc.manifestCalls() != 0 || r.jobs[0].Status != "failed" || !strings.Contains(r.jobs[0].ErrorMessage, "不支持的清单渲染版本") {
			t.Fatalf("%s=%d 未在下载前拦截: open=%d calls=%d job=%+v", tc.field, tc.value, countingAssets.openCalls, guardProc.manifestCalls(), r.jobs[0])
		}
	}

	// 旧版非清单任务（Manifest == nil）仍走既有 Compose 分支，不受清单不变量校验影响。
	r.jobs = nil
	r.episodes[0].EditSchema = 0
	legacyReq := composeReq(t, `,"compose_request_key":"legacy-non-manifest"`)
	legacyJob, err := guardSvc.CreateJob(ctx, 7, 1, legacyReq)
	if err != nil {
		t.Fatal(err)
	}
	if err := guardSvc.HandleDrama(ctx, 7, 1, uint64(legacyJob.ID)); err != nil {
		t.Fatal(err)
	}
	if r.jobs[0].Status != "succeeded" || guardProc.calls != 1 || guardProc.manifestCalls() != 0 {
		t.Fatalf("旧版非清单合成路径受影响: job=%+v calls=%d manifestCalls=%d", r.jobs[0], guardProc.calls, guardProc.manifestCalls())
	}
}

type countingOpenAssets struct {
	*manifestFiles
	openCalls int
}

func (a *countingOpenAssets) Open(ctx context.Context, user, id uint64) (*provider.AssetFile, error) {
	a.openCalls++
	return a.manifestFiles.Open(ctx, user, id)
}
