package service_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/pkg/ws"
	"video-canvas/internal/provider"
	"video-canvas/internal/provider/modelcfg"
	"video-canvas/internal/provider/pluginmeta"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
)

type segEditAssets struct {
	assets map[uint64]*model.Asset
	views  map[uint64]*model.AssetView
}

func (a *segEditAssets) View(_ context.Context, userID, id uint64) (*model.AssetView, error) {
	if v, ok := a.views[id]; ok {
		return v, nil
	}
	if row, ok := a.assets[id]; ok && row.UserID == userID {
		return &model.AssetView{ID: row.ID, Kind: row.Kind, URL: "/files/" + row.StorageKey}, nil
	}
	return nil, errcode.ErrAssetNotFound
}

func (a *segEditAssets) Get(_ context.Context, userID, id uint64) (*model.Asset, error) {
	row, ok := a.assets[id]
	if !ok || row.UserID != userID {
		return nil, provider.ErrAssetNotFound
	}
	return row, nil
}

func (a *segEditAssets) Open(_ context.Context, userID, id uint64) (*provider.AssetFile, error) {
	row, ok := a.assets[id]
	if !ok || row.UserID != userID {
		return nil, provider.ErrAssetNotFound
	}
	return &provider.AssetFile{Asset: row, Body: io.NopCloser(strings.NewReader("fake-video"))}, nil
}

func segEditPGEnv(t *testing.T, clock *time.Time) (*gorm.DB, uint64, *fakeTaskRegistry, *service.GenerationTaskService, *service.DramaSegmentService, *segEditAssets) {
	t.Helper()
	db := gtiTestDB(t)
	if err := db.AutoMigrate(
		&model.DramaProject{},
		&model.DramaEpisode{},
		&model.DramaSegment{},
		&model.DramaJob{},
		&model.Asset{},
		&model.AssetMediaProbe{},
	); err != nil {
		t.Fatalf("迁移短剧编辑表失败：%v", err)
	}
	taskRepo := repository.NewGenerationTaskRepository(db)
	dramaRepo := repository.NewDramaRepository(db)
	probeRepo := repository.NewAssetMediaProbeRepository(db)
	limits := defaultTaskLimits()
	snap := fakeTaskSnapshot(model.KindVideo, 10)
	snap.Model.Capabilities.Params = modelcfg.ParamSet{
		{Name: "duration", ParamField: modelcfg.ParamField{Type: modelcfg.ParamEnum, Open: true, Options: []any{5.0, 10.0}, Default: 5.0}},
		{Name: "resolution", ParamField: modelcfg.ParamField{Type: modelcfg.ParamEnum, Open: true, Options: []any{"720p", "1080p"}, Default: "720p"}},
		{Name: "aspect_ratio", ParamField: modelcfg.ParamField{Type: modelcfg.ParamEnum, Open: true, Options: []any{"16:9", "9:16"}, Default: "16:9"}},
		{Name: "count", ParamField: modelcfg.ParamField{Type: modelcfg.ParamEnum, Open: true, Options: []any{1.0, 2.0}, Default: 1.0, Fanout: true}},
	}
	reg := &fakeTaskRegistry{snap: snap}
	assets := &segEditAssets{
		assets: map[uint64]*model.Asset{},
		views:  map[uint64]*model.AssetView{},
	}
	nowFn := func() time.Time { return *clock }
	taskSvc := service.NewGenerationTaskService(service.GenerationTaskDeps{
		Repo:        taskRepo,
		Registry:    reg,
		Executor:    &fakeTaskExecutor{},
		Assets:      assets,
		Broadcaster: ws.NopBroadcaster{},
		Limits:      limits,
		QuoteSecret: "test-quote-secret-key",
	}, service.WithTaskClock(nowFn), service.WithQuoteTTL(2*time.Minute))

	segSvc := service.NewDramaSegmentService(service.DramaSegmentDeps{
		Repo:        dramaRepo,
		Tasks:       taskSvc,
		Assets:      assets,
		Probes:      probeRepo,
		Broadcaster: ws.NopBroadcaster{},
	})
	userID := uint64(7001)
	if err := db.Create(&model.UserCredit{UserID: userID, Balance: 500}).Error; err != nil {
		t.Fatal(err)
	}
	return db, userID, reg, taskSvc, segSvc, assets
}

func seedDramaEpisodeWithTakes(t *testing.T, db *gorm.DB, assets *segEditAssets, userID uint64, editSchema int16, locked bool) (*model.DramaProject, *model.DramaEpisode, *model.DramaSegment, uint64, uint64) {
	t.Helper()
	p := model.DramaProject{UserID: userID, CanvasID: 10, Title: "审讯倒计时", SourceText: "src", StyleKey: "custom"}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	var lockedAt *time.Time
	if locked {
		now := time.Now()
		lockedAt = &now
	}
	ep := model.DramaEpisode{UserID: userID, DramaID: p.ID, No: 1, LockedAt: lockedAt, EditSchema: editSchema, SegmentVersion: 1}
	if err := db.Create(&ep).Error; err != nil {
		t.Fatal(err)
	}
	seg := model.DramaSegment{UserID: userID, EpisodeID: ep.ID, Seq: 0, Seconds: 5, Prompt: "旧对白"}
	if err := db.Create(&seg).Error; err != nil {
		t.Fatal(err)
	}
	sha1 := hex.EncodeToString(sha256.New().Sum([]byte("take-1"))[:32])
	sha2 := hex.EncodeToString(sha256.New().Sum([]byte("take-2"))[:32])
	a1 := model.Asset{UserID: userID, Kind: model.KindVideo, StorageKey: "k1.mp4", Sha256: sha1, ByteSize: 100}
	a2 := model.Asset{UserID: userID, Kind: model.KindVideo, StorageKey: "k2.mp4", Sha256: sha2, ByteSize: 100}
	if err := db.Create(&a1).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&a2).Error; err != nil {
		t.Fatal(err)
	}
	assets.assets[a1.ID], assets.assets[a2.ID] = &a1, &a2
	if err := db.Create(&model.AssetMediaProbe{
		AssetID: a1.ID, Sha256: sha1, ProbeVersion: videoframe.NormalizedProbeVersion, NormalizedFrames24: 100,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AssetMediaProbe{
		AssetID: a2.ID, Sha256: sha2, ProbeVersion: videoframe.NormalizedProbeVersion, NormalizedFrames24: 81,
	}).Error; err != nil {
		t.Fatal(err)
	}
	nodeID := "seg:" + idcodec.Encode(seg.ID)
	canvasID := p.CanvasID
	out1, _ := json.Marshal([]model.TaskOutput{{AssetID: a1.ID, MediaType: model.KindVideo}})
	out2, _ := json.Marshal([]model.TaskOutput{{AssetID: a2.ID, MediaType: model.KindVideo}})
	emptyJSON := []byte("{}")
	t1 := model.GenerationTask{UserID: userID, TaskRef: "t1-ref", Kind: model.KindVideo, ModelKey: "m1", Provider: "demo", CanvasProjectID: &canvasID, NodeID: nodeID, IdempotencyKey: "k-t1", Status: model.TaskSucceeded, InputJSON: emptyJSON, ConfigSnapshot: emptyJSON, OutputJSON: out1}
	t2 := model.GenerationTask{UserID: userID, TaskRef: "t2-ref", Kind: model.KindVideo, ModelKey: "m1", Provider: "demo", CanvasProjectID: &canvasID, NodeID: nodeID, IdempotencyKey: "k-t2", Status: model.TaskSucceeded, InputJSON: emptyJSON, ConfigSnapshot: emptyJSON, OutputJSON: out2}
	if err := db.Create(&t1).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&t2).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&seg).Update("chosen_task_id", t1.ID).Error; err != nil {
		t.Fatal(err)
	}
	return &p, &ep, &seg, t1.ID, t2.ID
}

func TestDramaSegmentService_V21_EditBoundsResetAndSchema(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1700000000, 0)
	db, userID, _, _, segSvc, assets := segEditPGEnv(t, &now)
	_, ep, seg, t1ID, t2ID := seedDramaEpisodeWithTakes(t, db, assets, userID, 0, true)

	// 1. 查看片段视图应带 SourceFrameCount = 100
	list, err := segSvc.List(ctx, userID, ep.DramaID, ep.No)
	if err != nil || len(list) != 1 || list[0].SourceFrameCount == nil || *list[0].SourceFrameCount != 100 {
		t.Fatalf("应返回 t1 的 100 帧计数：%v, list=%+v", err, list)
	}

	// 2. ExpectedTaskID 不匹配 -> 409
	wrongExpected := idcodec.ID(t2ID)
	in24, out43, muteTrue, gainNeg6, dialTrue, pts50 := int32(24), int32(43), true, float64(-6), true, int32(50)
	_, err = segSvc.Update(ctx, userID, seg.ID, &model.UpdateDramaSegmentReq{
		Revision:       list[0].Revision,
		ExpectedTaskID: &wrongExpected,
		Edit:           &model.DramaSegmentEditPatch{SourceInF: &in24},
	})
	segmentCode(t, err, errcode.ErrCanvasConflict.Code)

	// 3. 非法编辑边界 -> 91003 (400)
	for _, tc := range []struct {
		name  string
		patch model.DramaSegmentEditPatch
	}{
		{"negative_in", model.DramaSegmentEditPatch{SourceInF: ptrInt32(-1)}},
		{"out_le_in", model.DramaSegmentEditPatch{SourceInF: &in24, SourceOutF: model.NullableInt32{Set: true, Value: &in24}}},
		{"out_gt_frames", model.DramaSegmentEditPatch{SourceOutF: model.NullableInt32{Set: true, Value: ptrInt32(101)}}},
		{"in_ge_frames_eof", model.DramaSegmentEditPatch{SourceInF: ptrInt32(100)}},
		{"gain_low", model.DramaSegmentEditPatch{AudioGainDB: ptrFloat64(-60.5)}},
		{"gain_high", model.DramaSegmentEditPatch{AudioGainDB: ptrFloat64(12.5)}},
		{"pts_low", model.DramaSegmentEditPatch{AvPtsOffsetMs: ptrInt32(-10001)}},
		{"pts_high", model.DramaSegmentEditPatch{AvPtsOffsetMs: ptrInt32(10001)}},
	} {
		_, err = segSvc.Update(ctx, userID, seg.ID, &model.UpdateDramaSegmentReq{Revision: list[0].Revision, Edit: &tc.patch})
		segmentCode(t, err, errcode.ErrDramaEditInvalid.Code)
	}

	// 4. 首次合法编辑 [24, 43) + 全套原声配置 -> EditSchema 0->1 且 SegmentVersion 单次递增 (+1)
	expectedT1 := idcodec.ID(t1ID)
	updated, err := segSvc.Update(ctx, userID, seg.ID, &model.UpdateDramaSegmentReq{
		Revision:       list[0].Revision,
		ExpectedTaskID: &expectedT1,
		Edit: &model.DramaSegmentEditPatch{
			SourceInF:        &in24,
			SourceOutF:       model.NullableInt32{Set: true, Value: &out43},
			MuteOriginal:     &muteTrue,
			AudioGainDB:      &gainNeg6,
			NativeIsDialogue: &dialTrue,
			AvPtsOffsetMs:    &pts50,
		},
	})
	if err != nil {
		t.Fatalf("首次编辑失败：%v", err)
	}
	if updated.Edit.SourceInF != 24 || updated.Edit.SourceOutF == nil || *updated.Edit.SourceOutF != 43 ||
		!updated.Edit.MuteOriginal || updated.Edit.AudioGainDB != -6 || !updated.Edit.NativeIsDialogue || updated.Edit.AvPtsOffsetMs != 50 {
		t.Fatalf("编辑视图不匹配：%+v", updated.Edit)
	}
	var dbEp model.DramaEpisode
	if err := db.First(&dbEp, ep.ID).Error; err != nil {
		t.Fatal(err)
	}
	if dbEp.EditSchema != 1 || dbEp.SegmentVersion != ep.SegmentVersion+1 {
		t.Fatalf("期望 EditSchema=1 且 SegmentVersion=+1，实际 edit_schema=%d segment_version=%d", dbEp.EditSchema, dbEp.SegmentVersion)
	}

	// 5. 显式 source_out_f: null 恢复到 EOF（仅保留静音/增益）
	var rawPatch model.UpdateDramaSegmentReq
	if err := json.Unmarshal([]byte(`{"revision":`+itoa(int(updated.Revision))+`,"edit":{"source_in_f":0,"source_out_f":null,"mute_original":true}}`), &rawPatch); err != nil {
		t.Fatal(err)
	}
	eofView, err := segSvc.Update(ctx, userID, seg.ID, &rawPatch)
	if err != nil || eofView.Edit.SourceInF != 0 || eofView.Edit.SourceOutF != nil || !eofView.Edit.MuteOriginal {
		t.Fatalf("恢复 EOF 失败：err=%v view=%+v", err, eofView)
	}

	// 6. 切换 take 且 reset_edit=false 携带旧 90 出点 (> t2 81帧) -> 91003；合法 81 -> 通过
	keepFalse := false
	newT2 := idcodec.ID(t2ID)
	_, err = segSvc.Update(ctx, userID, seg.ID, &model.UpdateDramaSegmentReq{
		Revision:              eofView.Revision,
		ChosenTaskID:          &newT2,
		ResetEditOnTaskChange: &keepFalse,
		Edit:                  &model.DramaSegmentEditPatch{SourceOutF: model.NullableInt32{Set: true, Value: ptrInt32(90)}},
	})
	segmentCode(t, err, errcode.ErrDramaEditInvalid.Code)

	switched, err := segSvc.Update(ctx, userID, seg.ID, &model.UpdateDramaSegmentReq{
		Revision:     eofView.Revision,
		ChosenTaskID: &newT2,
	})
	if err != nil {
		t.Fatalf("默认重置换 take 失败：%v", err)
	}
	if switched.SourceFrameCount == nil || *switched.SourceFrameCount != 81 {
		t.Fatalf("期望切换后返回 t2 的 81 帧：%+v", switched.SourceFrameCount)
	}
	if switched.Edit.SourceInF != 0 || switched.Edit.SourceOutF != nil || switched.Edit.MuteOriginal ||
		switched.Edit.AudioGainDB != 0 || switched.Edit.NativeIsDialogue || switched.Edit.AvPtsOffsetMs != 0 {
		t.Fatalf("切换 take 未同事务清空 6 列（尤其 source_out_f 必须为 nil）：%+v", switched.Edit)
	}

	// 7. 亚厘贝级精度规整：重复提交 0.004（入库 numeric(5,2) 舍为 0.00）不得反复递增 segment_version。
	if err := db.First(&dbEp, ep.ID).Error; err != nil {
		t.Fatal(err)
	}
	verBeforeGain := dbEp.SegmentVersion
	rev := switched.Revision
	for i := 0; i < 3; i++ {
		subGain := 0.004
		v, err := segSvc.Update(ctx, userID, seg.ID, &model.UpdateDramaSegmentReq{
			Revision: rev,
			Edit:     &model.DramaSegmentEditPatch{AudioGainDB: &subGain},
		})
		if err != nil {
			t.Fatalf("第 %d 次 0.004 PATCH 失败: %v", i+1, err)
		}
		if v.Edit.AudioGainDB != 0 {
			t.Fatalf("期望 0.004 规整为 0，实际: %v", v.Edit.AudioGainDB)
		}
		if err := db.First(&dbEp, ep.ID).Error; err != nil {
			t.Fatal(err)
		}
		if dbEp.SegmentVersion != verBeforeGain {
			t.Fatalf("第 %d 次 0.004 PATCH 不应递增 segment_version: got=%d want=%d", i+1, dbEp.SegmentVersion, verBeforeGain)
		}
		rev = v.Revision
	}
	// 0.006 规整为 0.01（与 0 不同）应仅在首次变更时递增一次，后续重复提交不再递增。
	gainRoundUp := 0.006
	v1, err := segSvc.Update(ctx, userID, seg.ID, &model.UpdateDramaSegmentReq{
		Revision: rev,
		Edit:     &model.DramaSegmentEditPatch{AudioGainDB: &gainRoundUp},
	})
	if err != nil || v1.Edit.AudioGainDB != 0.01 {
		t.Fatalf("期望 0.006 规整为 0.01: err=%v view=%+v", err, v1)
	}
	if err := db.First(&dbEp, ep.ID).Error; err != nil {
		t.Fatal(err)
	}
	if dbEp.SegmentVersion != verBeforeGain+1 {
		t.Fatalf("首次实质增益变更未递增版本: got=%d want=%d", dbEp.SegmentVersion, verBeforeGain+1)
	}
	gainExact := 0.01
	v2, err := segSvc.Update(ctx, userID, seg.ID, &model.UpdateDramaSegmentReq{
		Revision: v1.Revision,
		Edit:     &model.DramaSegmentEditPatch{AudioGainDB: &gainExact},
	})
	if err != nil || v2.Edit.AudioGainDB != 0.01 {
		t.Fatalf("等价 0.01 PATCH 失败: err=%v view=%+v", err, v2)
	}
	if err := db.First(&dbEp, ep.ID).Error; err != nil {
		t.Fatal(err)
	}
	if dbEp.SegmentVersion != verBeforeGain+1 {
		t.Fatalf("等价增益 0.01 不应再次递增版本: got=%d want=%d", dbEp.SegmentVersion, verBeforeGain+1)
	}
}

func TestDramaSegmentService_V21_QuoteAndGenerateGuard(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1700001000, 0)
	db, userID, reg, taskSvc, segSvc, assets := segEditPGEnv(t, &now)
	p, ep, seg, _, _ := seedDramaEpisodeWithTakes(t, db, assets, userID, 1, true)
	nodeID := "seg:" + idcodec.Encode(seg.ID)

	// 1. 保留内部前缀拦截（400）
	_, err := segSvc.Generate(ctx, userID, seg.ID, service.InternalDramaFrameRunKeyPrefix+"run-1", &model.GenerateDramaSegmentReq{
		ModelID: "m1",
		Input:   map[string]any{"prompt": "审讯室"},
	})
	assertTaskCode(t, err, errcode.ErrInvalidParams.Code)

	// 2. Quote 测试 Sora raw_size 解析 & 报价令牌生成，以及 edit_schema=1 时 count=2 拒绝 (91003)
	reg.snap.Plugin = provider.PluginSnapshot{Key: "sora", Version: "1.0.0", SHA256: "sora-sha", Meta: pluginmeta.Meta{Key: "sora", Version: "1.0.0"}}
	qSora, err := segSvc.Quote(ctx, userID, seg.ID, &model.GenerateDramaSegmentReq{
		ModelID: "m1",
		Input:   map[string]any{"prompt": "审讯室", "resolution": "1080p", "aspect_ratio": "16:9"},
	})
	if err != nil {
		t.Fatalf("Quote 失败：%v", err)
	}
	if qSora.RawSize != "1792x1024" || qSora.Credits != 10 || qSora.QuoteToken == "" || qSora.PriceFingerprint == "" || !qSora.ExpiresAt.After(now) {
		t.Fatalf("Quote 返回不符契约：%+v", qSora)
	}
	// edit_schema=1 时 count=2 被 Quote 与 Generate 拒绝 (91003)
	_, err = segSvc.Quote(ctx, userID, seg.ID, &model.GenerateDramaSegmentReq{
		ModelID: "m1",
		Input:   map[string]any{"prompt": "审讯室", "count": 2},
	})
	segmentCode(t, err, errcode.ErrDramaEditInvalid.Code)
	_, err = segSvc.Generate(ctx, userID, seg.ID, "seg-edit-cnt2", &model.GenerateDramaSegmentReq{
		ModelID:    "m1",
		Input:      map[string]any{"prompt": "审讯室", "count": 2},
		QuoteToken: qSora.QuoteToken,
	})
	segmentCode(t, err, errcode.ErrDramaEditInvalid.Code)

	// 3. edit_schema=1 无 quote_token 拒绝 91002，零任务零冻结
	_, err = segSvc.Generate(ctx, userID, seg.ID, "seg-no-quote", &model.GenerateDramaSegmentReq{
		ModelID: "m1",
		Input:   map[string]any{"prompt": "审讯室"},
	})
	segmentCode(t, err, errcode.ErrDramaQuoteExpired.Code)

	// 4. Quote 后调价漂移（10->25）或默认值漂移：Generate 与直接 POST /generation-tasks seg: 均返 91002 (409) 且零冻结
	qValid, err := segSvc.Quote(ctx, userID, seg.ID, &model.GenerateDramaSegmentReq{
		ModelID: "m1",
		Input:   map[string]any{"prompt": "审讯室"},
	})
	if err != nil {
		t.Fatal(err)
	}
	reg.snap.Model.Pricing.Unit = 25
	_, err = segSvc.Generate(ctx, userID, seg.ID, "seg-price-drift", &model.GenerateDramaSegmentReq{
		ModelID:    "m1",
		Input:      map[string]any{"prompt": "审讯室"},
		QuoteToken: qValid.QuoteToken,
	})
	segmentCode(t, err, errcode.ErrDramaQuoteExpired.Code)

	respDirect, err := taskSvc.Create(ctx, userID, "seg-direct-drift", &model.CreateGenerationTaskReq{
		Kind:       model.KindVideo,
		ModelID:    "m1",
		CanvasID:   idcodec.ID(p.CanvasID),
		NodeID:     nodeID,
		Input:      map[string]any{"prompt": "审讯室"},
		QuoteToken: qValid.QuoteToken,
	})
	if err != nil || len(respDirect.Items) != 1 || respDirect.Items[0].Error == nil || respDirect.Items[0].Error.Code != errcode.ErrDramaQuoteExpired.Code {
		t.Fatalf("直接提交 seg: 变价应返回 91002：err=%v resp=%+v", err, respDirect)
	}
	var creditCheck model.UserCredit
	if err := db.Where("user_id = ?", userID).First(&creditCheck).Error; err != nil {
		t.Fatal(err)
	}
	if creditCheck.Frozen != 0 {
		t.Fatalf("变价被拒不应冻结积分，实际 frozen=%d", creditCheck.Frozen)
	}

	// 5. Token 过期（clock 越过 TTL）-> 91002
	now = now.Add(5 * time.Minute)
	reg.snap.Model.Pricing.Unit = 10
	_, err = segSvc.Generate(ctx, userID, seg.ID, "seg-expired", &model.GenerateDramaSegmentReq{
		ModelID:    "m1",
		Input:      map[string]any{"prompt": "审讯室"},
		QuoteToken: qValid.QuoteToken,
	})
	segmentCode(t, err, errcode.ErrDramaQuoteExpired.Code)

	// 6. 新报价提交成功，之后解锁集、造繁忙 job、缩短片长、过期 quote、甚至下架模型 -> 同键同载重放仍 200 返回原任务，异载先返 409（未检查 busy）
	qFresh, err := segSvc.Quote(ctx, userID, seg.ID, &model.GenerateDramaSegmentReq{
		ModelID: "m1",
		Input:   map[string]any{"prompt": "0-4秒：审讯室"},
	})
	if err != nil {
		t.Fatal(err)
	}
	taskReqPayload := &model.GenerateDramaSegmentReq{
		ModelID:    "m1",
		Input:      map[string]any{"prompt": "0-4秒：审讯室"},
		QuoteToken: qFresh.QuoteToken,
	}
	created, err := segSvc.Generate(ctx, userID, seg.ID, "seg-replay-win", taskReqPayload)
	if err != nil {
		t.Fatalf("持有效报价生成失败：%v", err)
	}

	// 制造后期状态漂移：解锁、缩短片段时长到 1 秒、加进行中 segment job、过期时钟、下架模型
	if err := db.Model(&model.DramaEpisode{}).Where("id = ?", ep.ID).Update("locked_at", nil).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.DramaSegment{}).Where("id = ?", seg.ID).Update("seconds", 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.DramaJob{UserID: userID, DramaID: p.ID, Step: "segment", Episodes: []int{ep.No}, Status: "running", Lane: "user", InputJSON: []byte(`{}`), Output: []byte(`{}`)}).Error; err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute)
	reg.snapErr = provider.ErrModelUnavailable

	// 同键异载必须优先 409，不被解锁/busy/时间轴越界掩盖
	_, err = segSvc.Generate(ctx, userID, seg.ID, "seg-replay-win", &model.GenerateDramaSegmentReq{
		ModelID:    "m1",
		Input:      map[string]any{"prompt": "篡改后的内容"},
		QuoteToken: qFresh.QuoteToken,
	})
	assertTaskCode(t, err, errcode.ErrIdempotencyConflict.Code)

	// 同键同载只读重放成功
	replayed, err := segSvc.Generate(ctx, userID, seg.ID, "seg-replay-win", taskReqPayload)
	if err != nil || replayed.ID != created.ID {
		t.Fatalf("外层 Generate 重放应忽略后续状态漂移并返回原任务：err=%v replayed=%+v created=%+v", err, replayed, created)
	}
}

func ptrInt32(v int32) *int32       { return &v }
func ptrFloat64(v float64) *float64 { return &v }
