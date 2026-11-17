package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/datatypes"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/provider/modelcfg"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
	"video-canvas/internal/service/dramasegmentfake"
)

func TestDramaSegmentAdapt_RecordsAdaptSecondsAndPreservesLegacyZero(t *testing.T) {
	ctx := context.Background()
	r := dramasegmentfake.New()
	r.Credit = &model.UserCredit{UserID: 1, Balance: 20}
	var svc *service.DramaSegmentService
	var sid uint64
	adapter := &dramasegmentfake.Adapter{
		Out: "0-5秒：远处传来脚步声。",
	}
	svc = service.NewDramaSegmentService(service.DramaSegmentDeps{
		Repo: r, Tasks: r, Assets: r, Broadcaster: r, Adapter: adapter,
	})
	five := 5
	created, err := svc.Create(ctx, 1, 7, 1, &model.CreateDramaSegmentReq{Prompt: "原稿", Seconds: &five})
	if err != nil {
		t.Fatal(err)
	}
	sid = uint64(created.ID)
	// 转写耗时期间用户将片段改为 2 秒，转写结果应记录发起转写时使用的 5 秒。
	adapter.Mutate = func() {
		two := 2
		if _, err := svc.Update(ctx, 1, sid, &model.UpdateDramaSegmentReq{Revision: 1, Seconds: &two}); err != nil {
			t.Fatalf("转写期间改时长失败: %v", err)
		}
	}

	view, err := svc.Adapt(ctx, 1, sid, &model.AdaptDramaSegmentReq{Format: "h3", Revision: 1})
	if err != nil {
		t.Fatalf("转写失败: %v", err)
	}
	if len(adapter.Calls) != 1 || adapter.Calls[0].Seconds != 5 {
		t.Fatalf("传给适配器的秒数不对: %+v", adapter.Calls)
	}
	if got := view.Adaptations["h3"].Seconds; got != 5 {
		t.Fatalf("adaptations[h3].Seconds = %d, want 5", got)
	}

	// 存量未知时长的转写稿：手改后仍保持 Seconds == 0 且 JSON 不输出 seconds，不把当前行秒数虚构为历史生成时长。
	legacy := model.DramaSegment{
		ID:          99,
		UserID:      1,
		EpisodeID:   10,
		Seq:         1,
		Seconds:     2,
		Prompt:      "原稿",
		Adaptations: datatypes.JSON(`{"h3":{"prompt":"legacy draft","source_hash":"old","edited":false}}`),
		Revision:    1,
	}
	r.Rows = append(r.Rows, legacy)
	edited, err := svc.Update(ctx, 1, 99, &model.UpdateDramaSegmentReq{
		Revision:   1,
		Adaptation: &model.DramaSegmentAdaptationEdit{Format: "h3", Prompt: "manual edit"},
	})
	if err != nil {
		t.Fatalf("手改存量转写稿失败: %v", err)
	}
	if got := edited.Adaptations["h3"].Seconds; got != 0 {
		t.Fatalf("存量未知时长被篡改: Seconds=%d, want 0", got)
	}
	raw, err := json.Marshal(edited.Adaptations["h3"])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"seconds"`) {
		t.Fatalf("存量 Seconds==0 应被 omitempty 省略, got %s", raw)
	}
}

func TestDramaSegmentGenerate_RejectsTimelineOverrun(t *testing.T) {
	ctx := context.Background()
	r := dramasegmentfake.New()
	svc := service.NewDramaSegmentService(service.DramaSegmentDeps{Repo: r, Tasks: r, Assets: r, Broadcaster: r})
	two := 2
	created, err := svc.Create(ctx, 1, 7, 1, &model.CreateDramaSegmentReq{Prompt: "0-4秒：走路", Seconds: &two})
	if err != nil {
		t.Fatal(err)
	}
	sid := uint64(created.ID)

	for _, tc := range []struct {
		name    string
		input   map[string]any
		wantErr bool
	}{
		{"缺省走片段2秒拦截0-4秒", map[string]any{"prompt": "0-4秒：走路"}, true},
		{"字符串枚举2秒拦截H3切点", map[string]any{"prompt": "[Shot 1] 开场\n[Shot 2] At 00:04.000, 拉近", "duration": "2"}, true},
		{"时钟与帧率写法豁免", map[string]any{"prompt": "监控画面 00:10-00:20 24-60fps，0-2秒：特写", "duration": 2}, false},
		{"显式5秒放行0-4秒", map[string]any{"prompt": "0-4秒：走路", "duration": 5}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r.Created = nil
			_, err := svc.Generate(ctx, 1, sid, "", &model.GenerateDramaSegmentReq{ModelID: "video", Input: tc.input})
			if tc.wantErr {
				var appErr *errcode.Error
				if !errors.As(err, &appErr) || appErr.Code != errcode.ErrTaskInput.Code {
					t.Fatalf("err=%v, want code %d", err, errcode.ErrTaskInput.Code)
				}
				if r.Created != nil {
					t.Fatal("时间轴越界不应继续调用下游任务创建")
				}
				return
			}
			if err != nil {
				t.Fatalf("预期放行但失败: %v", err)
			}
		})
	}
}

func TestDramaMediaIntegration_RejectsTimelineOverrunWithModelCap(t *testing.T) {
	setupSeg := func(t *testing.T, segSeconds int) (*gtiEnv, string) {
		t.Helper()
		e := newGTIEnv(t, defaultTaskLimits())
		if err := e.db.AutoMigrate(&model.DramaProject{}, &model.DramaEpisode{}, &model.DramaSegment{}, &model.DramaSegmentFrameRun{}, &model.DramaSegmentTakeLink{}, &model.DramaJob{}); err != nil {
			t.Fatal(err)
		}
		p := model.DramaProject{UserID: e.user, CanvasID: 7, Title: "越界测试", SourceText: "原文", StyleKey: "custom"}
		if err := e.db.Create(&p).Error; err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		ep := model.DramaEpisode{UserID: e.user, DramaID: p.ID, No: 1, LockedAt: &now}
		if err := e.db.Create(&ep).Error; err != nil {
			t.Fatal(err)
		}
		seg := model.DramaSegment{UserID: e.user, EpisodeID: ep.ID, Seq: 0, Seconds: segSeconds}
		if err := e.db.Create(&seg).Error; err != nil {
			t.Fatal(err)
		}
		initial, err := defaultTaskLimits().InitialCredits(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := e.store.WithTx(context.Background(), func(tx repository.GenerationTaskTx) error {
			return tx.EnsureCredit(context.Background(), e.user, initial)
		}); err != nil {
			t.Fatal(err)
		}
		return e, "seg:" + idcodec.Encode(seg.ID)
	}

	t.Run("模型未开放duration锁定2秒时拦截5秒片段的0-4秒原稿", func(t *testing.T) {
		e, nodeID := setupSeg(t, 5)
		var min2, max2 int64 = 2, 2
		e.registry.snap.Model.Capabilities.Params = modelcfg.ParamSet{{
			Name: modelcfg.DurationParam,
			ParamField: modelcfg.ParamField{
				Type:    modelcfg.ParamNumber,
				Open:    false,
				Default: 2,
				Min:     &min2,
				Max:     &max2,
			},
		}}
		req := validCreateReq()
		req.NodeID = nodeID
		req.Input = map[string]any{"prompt": "0-4秒：主角推门而入"}
		_, err := createSingle(context.Background(), e.svc, e.user, "locked-2s", req)
		assertTaskCode(t, err, errcode.ErrTaskInput.Code)
		rec := e.assertReconciled(t)
		var taskCount int64
		if err := e.db.Model(&model.GenerationTask{}).Count(&taskCount).Error; err != nil {
			t.Fatal(err)
		}
		if rec.Frozen != 0 || taskCount != 0 {
			t.Fatalf("越界拒绝不应冻结积分或创建任务: %+v tasks=%d", rec, taskCount)
		}
	})

	t.Run("无duration参数模型在事务内回退片段2秒并拦截H3切点", func(t *testing.T) {
		e, nodeID := setupSeg(t, 2)
		req := validCreateReq()
		req.NodeID = nodeID
		req.Input = map[string]any{"prompt": "[Shot 1] Wide.\n[Shot 2] At 00:03.500, pan left"}
		_, err := createSingle(context.Background(), e.svc, e.user, "no-duration-2s", req)
		assertTaskCode(t, err, errcode.ErrTaskInput.Code)
		rec := e.assertReconciled(t)
		var taskCount int64
		if err := e.db.Model(&model.GenerationTask{}).Count(&taskCount).Error; err != nil {
			t.Fatal(err)
		}
		if rec.Frozen != 0 || taskCount != 0 {
			t.Fatalf("越界拒绝不应冻结积分或创建任务: %+v tasks=%d", rec, taskCount)
		}
	})
}
