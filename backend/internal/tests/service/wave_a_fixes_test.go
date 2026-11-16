package service_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"video-canvas/internal/config"
	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/provider"
	"video-canvas/internal/provider/modelcfg"
	"video-canvas/internal/repository"
	. "video-canvas/internal/service"
)

type sequenceTaskRegistry struct {
	snaps []*provider.Snapshot
	idx   atomic.Int32
}

func (r *sequenceTaskRegistry) ListModels(context.Context, string) ([]provider.ModelInfo, error) {
	return nil, nil
}

func (r *sequenceTaskRegistry) Snapshot(context.Context, string) (*provider.Snapshot, error) {
	n := int(r.idx.Add(1)) - 1
	if n >= len(r.snaps) {
		n = len(r.snaps) - 1
	}
	return r.snaps[n], nil
}

// TestQuoteLockSnapshotParity_VerifyAndEnqueueSameSnap 验证：报价对应 B、创建预取 A、锁内重读 B 且有效输入一致时，
// 最终扣费与入库快照必须同为 B，不能出现“验 B 收 B 却按 A 执行”。
func TestQuoteLockSnapshotParity_VerifyAndEnqueueSameSnap(t *testing.T) {
	ctx := context.Background()
	snapA := fakeTaskSnapshot(model.KindVideo, 10)
	snapA.Channel.Key = "chan-a"
	snapA.Model.UpstreamModel = "upstream-a"
	snapA.ModelRevisionID = 101

	snapB := fakeTaskSnapshot(model.KindVideo, 10)
	snapB.Channel.Key = "chan-b"
	snapB.Model.UpstreamModel = "upstream-b"
	snapB.ModelRevisionID = 102

	// 1. 先按快照 B 出具报价（Quote 调用 1 次 Snapshot -> B）。
	// 2. 紧接着 Create 预取得到快照 A（第 2 次 Snapshot -> A），锁内重读恢复为 B（第 3 次 Snapshot -> B）。
	reg := &sequenceTaskRegistry{snaps: []*provider.Snapshot{snapB, snapA, snapB}}
	env := newTaskSvcEnv(defaultTaskLimits())
	env.svc = NewGenerationTaskService(GenerationTaskDeps{
		Repo:        env.repo,
		Registry:    reg,
		Executor:    env.exec,
		Assets:      env.assets,
		Broadcaster: env.bc,
		Limits:      defaultTaskLimits(),
		QuoteSecret: "test-quote-secret-key",
	}, WithTaskClock(func() time.Time { return env.now }))

	q, err := env.svc.Quote(ctx, 1, &model.QuoteGenerationTaskReq{
		Kind:    model.KindVideo,
		ModelID: "m1",
		Input:   map[string]any{"prompt": "同一有效输入"},
	})
	if err != nil {
		t.Fatalf("Quote 失败：%v", err)
	}

	resp, err := env.svc.Create(ctx, 1, "k-snap-ab", &model.CreateGenerationTaskReq{
		Kind:       model.KindVideo,
		ModelID:    "m1",
		CanvasID:   9,
		NodeID:     "n1",
		Input:      map[string]any{"prompt": "同一有效输入"},
		QuoteToken: q.QuoteToken,
	})
	if err != nil || len(resp.Items) != 1 || resp.Items[0].Task == nil {
		t.Fatalf("Create 应使用锁内 B 快照入库并成功：err=%v resp=%+v", err, resp)
	}
	var saved *model.GenerationTask
	for _, task := range env.repo.tasks {
		saved = task
		break
	}
	if saved == nil || saved.Provider != "chan-b" {
		t.Fatalf("入库任务必须使用锁内校验通过的快照 chan-b，实际 saved=%+v", saved)
	}
	var snapSaved provider.Snapshot
	if err := json.Unmarshal(saved.ConfigSnapshot, &snapSaved); err != nil {
		t.Fatal(err)
	}
	if snapSaved.Channel.Key != "chan-b" || snapSaved.Model.UpstreamModel != "upstream-b" || snapSaved.ModelRevisionID != 102 {
		t.Fatalf("ConfigSnapshot 必须是快照 B：%#v", snapSaved)
	}
}

// TestQuoteLockAssetLookup_MaxOpenConnsOneNoDeadlock 验证连接池仅 1 个连接时，
// 持信用锁核验含素材引用的报价绝不会另取连接死锁，也不使用 context.Background()。
func TestQuoteLockAssetLookup_MaxOpenConnsOneNoDeadlock(t *testing.T) {
	db := gtiTestDB(t)
	if err := db.AutoMigrate(&model.Asset{}); err != nil {
		t.Fatalf("迁移 assets 失败：%v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	userID := uint64(42)
	asset := model.Asset{
		UserID:     userID,
		StorageID:  1,
		StorageKey: "k/ref.png",
		Kind:       model.KindImage,
		MimeType:   "image/png",
		Source:     "upload",
		ByteSize:   1024,
	}
	if err := db.Create(&asset).Error; err != nil {
		t.Fatal(err)
	}

	snap := fakeTaskSnapshot(model.KindVideo, 10)
	snap.Model.Capabilities.Ops = []string{modelcfg.OpI2V}
	snap.Model.Capabilities.Refs.Image = modelcfg.RefSpec{On: true, Max: 1, MaxMB: 10}
	reg := &fakeTaskRegistry{snap: snap}
	assetSvc := NewAssetService(repository.NewAssetRepository(db), nil, config.Storage{})
	store := repository.NewGenerationTaskRepository(db)
	svc := NewGenerationTaskService(GenerationTaskDeps{
		Repo:        store,
		Registry:    reg,
		Executor:    &fakeTaskExecutor{},
		Assets:      assetSvc,
		Broadcaster: &fakeTaskBroadcaster{},
		Limits:      defaultTaskLimits(),
		QuoteSecret: "test-quote-secret-key",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	input := map[string]any{"op": modelcfg.OpI2V, "prompt": "带图报价", "images": []any{asset.ID}}
	q, err := svc.Quote(ctx, userID, &model.QuoteGenerationTaskReq{
		Kind:    model.KindVideo,
		ModelID: "m1",
		Input:   input,
	})
	if err != nil {
		t.Fatalf("Quote 失败：%v", err)
	}
	resp, err := svc.Create(ctx, userID, "k-pool-1", &model.CreateGenerationTaskReq{
		Kind:       model.KindVideo,
		ModelID:    "m1",
		CanvasID:   9,
		NodeID:     "n1",
		Input:      input,
		QuoteToken: q.QuoteToken,
	})
	if err != nil || len(resp.Items) != 1 || resp.Items[0].Task == nil {
		t.Fatalf("单连接池持信用锁核验含素材报价失败（若阻塞超时即死锁）：err=%v resp=%+v", err, resp)
	}
}

// TestQuoteLockSnapshot_MaxOpenConnsOneExpiredRegistryNoDeadlock 验证单连接池（MaxOpenConns=1）下，
// 持信用锁核验报价断言期间即使 Registry TTL 过期，仍通过当前事务连接直查且不自锁，同时感知库中实时改价。
func TestQuoteLockSnapshot_MaxOpenConnsOneExpiredRegistryNoDeadlock(t *testing.T) {
	db := slice2cPG16(t)
	slice2cMigrate(t, db, slice2cUpstream35WithFork9001)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	plugRepo := repository.NewAIPluginRepository(db)
	chanRepo := repository.NewAIChannelRepository(db)
	cfgRepo := repository.NewAIConfigRepository(db)
	if err := plugRepo.SaveVersion(ctx,
		&model.AIPlugin{Key: "kling", Name: "Kling", Source: model.PluginSourceUploaded, Enabled: true},
		&model.AIPluginVersion{PluginKey: "kling", Version: "1.0.0", SHA256: "kling-sha", Code: "module.exports={}", MetaJSON: model.JSONText(adminMetaJSON("kling", "1.0.0"))},
	); err != nil {
		t.Fatal(err)
	}
	if err := chanRepo.CreateChannel(ctx, &model.AIChannel{Key: "c1", Name: "Chan 1", BaseURL: "https://example.com", Enabled: true, Sort: 1}); err != nil {
		t.Fatal(err)
	}

	aic := NewAIConfigService(cfgRepo, chanRepo, plugRepo, "test-master-key")
	if err := aic.SetSecret(ctx, model.ChannelSecretName("c1"), "sk-c1", 1); err != nil {
		t.Fatal(err)
	}
	baseNow := time.Unix(1_700_000_000, 0)
	aic.SetNow(func() time.Time { return baseNow })
	if _, err := aic.SaveModel(ctx, ModelSaveInput{Create: true, Body: aicModelBody("m1", "c1", ""), AdminID: 1}); err != nil {
		t.Fatal(err)
	}
	if err := aic.SetModelEnabled(ctx, "m1", true, 1); err != nil {
		t.Fatal(err)
	}
	if err := aic.RefreshRegistry(ctx); err != nil {
		t.Fatal(err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	store := repository.NewGenerationTaskRepository(db)
	svc := NewGenerationTaskService(GenerationTaskDeps{
		Repo:        store,
		Registry:    aic,
		Executor:    &fakeTaskExecutor{},
		Assets:      NewAssetService(repository.NewAssetRepository(db), nil, config.Storage{}),
		Broadcaster: &fakeTaskBroadcaster{},
		Limits:      defaultTaskLimits(),
		QuoteSecret: "test-quote-secret-key",
	})

	input := map[string]any{"prompt": "单连接防死锁回归"}
	q, err := svc.Quote(ctx, 42, &model.QuoteGenerationTaskReq{Kind: model.KindVideo, ModelID: "m1", Input: input})
	if err != nil {
		t.Fatalf("Quote 失败: %v", err)
	}

	// 第 1 次 now（Create 锁外 preflight）命中缓存，第 2 次 now（持信用锁 SnapshotInTx）强制 TTL 过期。
	var ticks atomic.Int32
	aic.SetNow(func() time.Time {
		if ticks.Add(1) >= 2 {
			return baseNow.Add(10 * time.Minute)
		}
		return baseNow
	})
	resp, err := svc.Create(ctx, 42, "k-maxconns-1", &model.CreateGenerationTaskReq{
		Kind:       model.KindVideo,
		ModelID:    "m1",
		CanvasID:   9,
		NodeID:     "n1",
		Input:      input,
		QuoteToken: q.QuoteToken,
	})
	if err != nil || len(resp.Items) != 1 || resp.Items[0].Task == nil {
		t.Fatalf("单连接池持锁核验遇到过期 TTL 不应死锁或失败: err=%v resp=%+v", err, resp)
	}

	// 验证锁内直读实时配置：预先让缓存保留旧价(unit=5)，底层库改为 unit=12，锁内过期触发直查并拒绝旧报价。
	ticks.Store(0)
	aic.SetNow(func() time.Time { return baseNow })
	if err := aic.RefreshRegistry(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`UPDATE ai_config_revisions SET body_json = replace(body_json::text, '"unit":5', '"unit":12')::jsonb WHERE target_key = 'm1'`).Error; err != nil {
		t.Fatal(err)
	}
	ticks.Store(0)
	aic.SetNow(func() time.Time {
		if ticks.Add(1) >= 2 {
			return baseNow.Add(10 * time.Minute)
		}
		return baseNow
	})
	respStale, err := svc.Create(ctx, 42, "k-maxconns-stale", &model.CreateGenerationTaskReq{
		Kind:       model.KindVideo,
		ModelID:    "m1",
		CanvasID:   9,
		NodeID:     "n2",
		Input:      input,
		QuoteToken: q.QuoteToken,
	})
	if err != nil {
		t.Fatalf("Create 请求本身不应返回协议错误: %v", err)
	}
	if len(respStale.Items) != 1 || respStale.Items[0].Error == nil || respStale.Items[0].Error.Code != errcode.ErrDramaQuoteExpired.Code {
		t.Fatalf("库内改价后锁内事务读取未拦截过期报价: %+v", respStale)
	}
}

// TestAIConfig_TestRunReplayIgnoresSecretAndEnumRemoval 验证带幂等键试跑重放先核对历史任务，
// 撤销渠道密钥、移除旧枚举项甚至下架模型后，同一请求仍能重放原任务，异载仍返回 409。
func TestAIConfig_TestRunReplayIgnoresSecretAndEnumRemoval(t *testing.T) {
	ctx := context.Background()
	aic, aicRepo, _, _ := aicRunSetup(t)
	taskEnv := newTaskSvcEnv(defaultTaskLimits())
	snap := fakeTaskSnapshot(model.KindVideo, 10)
	snap.Model.Capabilities.Ops = []string{modelcfg.OpT2V}
	snap.Model.Capabilities.Params = modelcfg.ParamSet{{
		Name: "resolution",
		ParamField: modelcfg.ParamField{
			Type:    "enum",
			Label:   "分辨率",
			Open:    true,
			Options: []any{"legacy-480p", "720p"},
			Default: "720p",
		},
	}}
	taskEnv.registry = &fakeTaskRegistry{snap: snap}
	taskEnv.svc = NewGenerationTaskService(GenerationTaskDeps{
		Repo:        taskEnv.repo,
		Registry:    taskEnv.registry,
		Executor:    taskEnv.exec,
		Assets:      taskEnv.assets,
		Broadcaster: taskEnv.bc,
		Limits:      defaultTaskLimits(),
	}, WithTaskClock(func() time.Time { return taskEnv.now }))
	aic.SetTestTaskCreator(taskEnv.svc)

	bodyV1 := aicModelBody("m1", "c1", `"capabilities":{"ops":["t2v"],"prompt":{"max_length":2000},"params":{"resolution":{"type":"enum","label":"分辨率","open":true,"options":["legacy-480p","720p"],"default":"720p"}}}`)
	if _, err := aicSave(aic, "m1", false, bodyV1); err != nil {
		t.Fatal(err)
	}

	raw := map[string]any{"prompt": "hi", "resolution": "legacy-480p"}
	first, err := aic.TestRunIdempotent(ctx, 1, "m1", raw, "trial-replay-stable")
	if err != nil {
		t.Fatalf("首次试跑失败：%v", err)
	}

	// 撤销渠道凭证并删除旧枚举值、甚至删除模型配置。
	delete(aicRepo.Secrets, model.ChannelSecretName("c1"))
	delete(aicRepo.Secrets, model.ChannelSecretKeyName("c1"))
	bodyV2 := aicModelBody("m1", "c1", `"capabilities":{"ops":["t2v"],"prompt":{"max_length":2000},"params":{"resolution":{"type":"enum","label":"分辨率","open":true,"options":["720p","1080p"],"default":"1080p"}}}`)
	if _, err := aicSave(aic, "m1", false, bodyV2); err != nil {
		t.Fatal(err)
	}
	delete(aicRepo.Models, "m1")

	replayed, err := aic.TestRunIdempotent(ctx, 1, "m1", map[string]any{"prompt": "hi", "resolution": "legacy-480p"}, "trial-replay-stable")
	if err != nil || replayed.ID != first.ID {
		t.Fatalf("撤销密钥与移除旧枚举后同键重放应直接返回原任务：err=%v replayed=%+v first=%+v", err, replayed, first)
	}

	// 同键不同模型或不同原始输入仍返回 409，即使当前模型配置已被移除。
	_, err = aic.TestRunIdempotent(ctx, 1, "m1", map[string]any{"prompt": "changed", "resolution": "legacy-480p"}, "trial-replay-stable")
	assertTaskCode(t, err, errcode.ErrIdempotencyConflict.Code)
	_, err = aic.TestRunIdempotent(ctx, 1, "other-model", map[string]any{"prompt": "hi", "resolution": "legacy-480p"}, "trial-replay-stable")
	assertTaskCode(t, err, errcode.ErrIdempotencyConflict.Code)
}

// TestQuote_ColabGpuStudioRawSizeMatrix 验证 colab-gpu-studio 插件与报价对 Ref2V / I2V 与渠道默认分辨率的尺寸矩阵严格一致。
func TestQuote_ColabGpuStudioRawSizeMatrix(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		version  string
		upstream string
		settings map[string]any
		input    map[string]any
		wantSize string
	}{
		{
			name:     "Ref2V 16:9 720P",
			version:  "1.4.0",
			upstream: "minimax-h3-ref2v",
			input:    map[string]any{"prompt": "x", "aspect_ratio": "16:9", "resolution": "720P"},
			wantSize: "864x480",
		},
		{
			name:     "Ref2V 16:9 1080P",
			version:  "1.4.0",
			upstream: "minimax-h3-ref2v",
			input:    map[string]any{"prompt": "x", "aspect_ratio": "16:9", "resolution": "1080P"},
			wantSize: "960x544",
		},
		{
			name:     "Ref2V 16:9 4K",
			version:  "1.4.0",
			upstream: "minimax-h3-ref2v",
			input:    map[string]any{"prompt": "x", "aspect_ratio": "16:9", "resolution": "4K"},
			wantSize: "1024x576",
		},
		{
			name:     "Ref2V 9:16 1080P",
			version:  "1.4.0",
			upstream: "minimax-h3-ref2v",
			input:    map[string]any{"prompt": "x", "aspect_ratio": "9:16", "resolution": "1080P"},
			wantSize: "544x960",
		},
		{
			name:     "Ref2V Auto 缺省分辨率回落渠道 default_resolution=1080P",
			version:  "1.4.0",
			upstream: "minimax-h3-ref2v",
			settings: map[string]any{"default_resolution": "1080P"},
			input:    map[string]any{"prompt": "x", "aspect_ratio": "Auto"},
			wantSize: "544x960",
		},
		{
			name:     "Ref2V Auto 无渠道默认值回落 720P",
			version:  "1.4.0",
			upstream: "minimax-h3-ref2v",
			input:    map[string]any{"prompt": "x", "aspect_ratio": "Auto"},
			wantSize: "480x864",
		},
		{
			name:     "I2V 16:9 1080P",
			version:  "1.4.0",
			upstream: "minimax-h3-i2v",
			input:    map[string]any{"prompt": "x", "aspect_ratio": "16:9", "resolution": "1080P"},
			wantSize: "1024x576",
		},
		{
			name:     "未知主版本 v2 不强行猜尺寸",
			version:  "2.0.0",
			upstream: "minimax-h3-ref2v",
			input:    map[string]any{"prompt": "x", "aspect_ratio": "16:9", "resolution": "1080P"},
			wantSize: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := fakeTaskSnapshot(model.KindVideo, 10)
			snap.Plugin = provider.PluginSnapshot{Key: "colab-gpu-studio", Version: tc.version, SHA256: "sha-colab"}
			snap.Model.UpstreamModel = tc.upstream
			snap.Channel.Settings = tc.settings
			snap.Model.Capabilities.Params = modelcfg.ParamSet{
				{
					Name: "aspect_ratio",
					ParamField: modelcfg.ParamField{
						Type: "enum", Label: "画幅", Open: true, Options: []any{"16:9", "9:16", "Auto"},
					},
				},
				{
					Name: "resolution",
					ParamField: modelcfg.ParamField{
						Type: "enum", Label: "清晰度", Open: true, Options: []any{"720P", "1080P", "4K"},
					},
				},
			}
			env := newTaskSvcEnv(defaultTaskLimits())
			env.svc = NewGenerationTaskService(GenerationTaskDeps{
				Repo:        env.repo,
				Registry:    &fakeTaskRegistry{snap: snap},
				Executor:    env.exec,
				Assets:      env.assets,
				Broadcaster: env.bc,
				Limits:      defaultTaskLimits(),
				QuoteSecret: "test-quote-secret-key",
			})
			q, err := env.svc.Quote(ctx, 1, &model.QuoteGenerationTaskReq{
				Kind:    model.KindVideo,
				ModelID: "m1",
				Input:   tc.input,
			})
			if err != nil {
				t.Fatalf("Quote 失败：%v", err)
			}
			if !strings.EqualFold(q.RawSize, tc.wantSize) {
				t.Fatalf("raw_size=%q want=%q (effective=%v)", q.RawSize, tc.wantSize, q.EffectiveInput)
			}
		})
	}
}
