package service_test

import (
	"context"
	"errors"
	"testing"

	"video-canvas/internal/marketing"
	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
)

// deletedMarketingRepo 模拟父项目已软删但子行仍然存在，拒绝任何子表写入。
type deletedMarketingRepo struct {
	service.MarketingRepo
	writes int
}

func (r *deletedMarketingRepo) WithTx(_ context.Context, fn func(repository.MarketingTx) error) error {
	return fn(r)
}
func (r *deletedMarketingRepo) Project(context.Context, uint64, uint64, bool) (*model.MarketingProject, error) {
	return nil, repository.ErrNotFound
}
func (r *deletedMarketingRepo) Round(context.Context, uint64, uint64, bool) (*model.MarketingRound, error) {
	return &model.MarketingRound{ID: 2, UserID: 7, ProjectID: 1, Status: model.MarketingRoundScripting, Settings: []byte(`{}`), Reverse: []byte(`{}`), ReverseOriginal: []byte(`{}`)}, nil
}
func (r *deletedMarketingRepo) Script(context.Context, uint64, uint64, bool) (*model.MarketingScript, error) {
	return &model.MarketingScript{ID: 3, UserID: 7, ProjectID: 1, RoundID: 2, Revision: 1, Shots: []byte(`[]`), Original: []byte(`{"shots":[]}`)}, nil
}
func (r *deletedMarketingRepo) RoundScripts(context.Context, ...uint64) ([]model.MarketingScript, error) {
	return nil, nil
}
func (r *deletedMarketingRepo) ScriptTasks(context.Context, uint64, []string) ([]model.GenerationTask, error) {
	return nil, nil
}
func (r *deletedMarketingRepo) RoundRuns(context.Context, ...uint64) ([]model.MarketingStepRun, error) {
	return nil, nil
}
func (r *deletedMarketingRepo) UpdateRound(context.Context, uint64, map[string]any) error {
	r.writes++
	return errors.New("禁止子表写")
}
func (r *deletedMarketingRepo) UpdateScript(context.Context, uint64, map[string]any) error {
	r.writes++
	return errors.New("禁止子表写")
}
func (r *deletedMarketingRepo) SaveScript(context.Context, uint64, uint64, int64, map[string]any) error {
	r.writes++
	return errors.New("禁止子表写")
}

func TestMarketingV21_DeletedParentRejectsBeforeChildWrites(t *testing.T) {
	for _, name := range []string{"cancel", "reverse", "restore_reverse", "narrations", "restore_script", "retry", "observer"} {
		t.Run(name, func(t *testing.T) {
			repo := &deletedMarketingRepo{}
			svc := service.NewMarketingService(service.MarketingDeps{Repo: repo})
			ctx := context.Background()
			var err error
			switch name {
			case "cancel":
				_, err = svc.CancelRound(ctx, 7, 2)
			case "reverse":
				_, err = svc.UpdateReverse(ctx, 7, 2, &model.UpdateMarketingReverseReq{})
			case "restore_reverse":
				_, err = svc.RestoreReverse(ctx, 7, 2)
			case "narrations":
				_, err = svc.UpdateScript(ctx, 7, 3, &model.UpdateMarketingScriptReq{Revision: 1})
			case "restore_script":
				_, err = svc.RestoreScript(ctx, 7, 3)
			case "retry":
				_, err = svc.RetryStep(ctx, 7, 9)
			case "observer":
				err = svc.OnTaskChanged(ctx, &model.GenerationTask{UserID: 7, Kind: model.KindVideo, NodeID: "mkt:3"})
			}
			if name == "observer" {
				if err != nil {
					t.Fatalf("删除后回调应忽略: %v", err)
				}
			} else {
				wantMktCode(t, err, errcode.ErrMarketingNotFound.Code)
			}
			if repo.writes != 0 {
				t.Fatalf("删除后子表写次数=%d", repo.writes)
			}
		})
	}
}

func (r *deletedMarketingRepo) RoundByID(context.Context, uint64, bool) (*model.MarketingRound, error) {
	return nil, errors.New("未锁存活父项目就读取轮次")
}
func (r *deletedMarketingRepo) ProjectByID(context.Context, uint64) (*model.MarketingProject, error) {
	return nil, repository.ErrNotFound
}

func TestMarketingV21_LatestRunUsesGenerationThenID(t *testing.T) {
	got := service.LatestMarketingRunsForTest([]model.MarketingStepRun{
		{ID: 20, StepKey: "marketing.follow", RoundGen: 3},
		{ID: 99, StepKey: "marketing.follow", RoundGen: 2},
		{ID: 19, StepKey: "marketing.follow", RoundGen: 3},
	})
	if len(got) != 1 || got[0].ID != 20 {
		t.Fatalf("最新运行应是第3代ID20: %+v", got)
	}
}

func (r *deletedMarketingRepo) Run(context.Context, uint64, uint64, bool) (*model.MarketingStepRun, error) {
	return &model.MarketingStepRun{ID: 9, UserID: 7, RoundID: 2, Status: model.MarketingRunSucceeded}, nil
}

// 实际素材、角色、镜头和限制都是转写输入，原稿文字相同不能证明缓存有效。
func TestMarketingV21_AdaptInputHashCoversRealInput(t *testing.T) {
	base := marketing.AdaptInput{Source: "手改原稿", Seconds: 15, ProductImages: 1, MaxLen: 6000, Shots: []marketing.AdaptShot{{No: 1, Narration: "开场"}}}
	original := marketing.AdaptInputHash("h3", base, 1)
	if original == "" || original != marketing.AdaptInputHash("h3", base, 1) {
		t.Fatal("指纹必须稳定")
	}
	for _, change := range []func(*marketing.AdaptInput){
		func(in *marketing.AdaptInput) { in.ProductImages = 2 },
		func(in *marketing.AdaptInput) { in.HasModel = true },
		func(in *marketing.AdaptInput) { in.HasRefVideo = true },
		func(in *marketing.AdaptInput) { in.Seconds = 10 },
		func(in *marketing.AdaptInput) { in.MaxLen = 5000 },
		func(in *marketing.AdaptInput) { in.Shots = []marketing.AdaptShot{{No: 1, Narration: "新旁白"}} },
	} {
		next := base
		change(&next)
		if marketing.AdaptInputHash("h3", next, 1) == original {
			t.Fatal("输入变更必须失效")
		}
	}
	if marketing.AdaptInputHash("h3", base, 2) == original || marketing.AdaptInputHash("generic", base, 1) == original || marketing.AdaptInputHash("h3", base, 1, 2) == marketing.AdaptInputHash("h3", base, 2, 1) {
		t.Fatal("格式、素材身份与顺序必须参与指纹")
	}
}

type versionProbeRepo struct {
	service.MarketingRepo
	reads int
}

func (r *versionProbeRepo) Script(context.Context, uint64, uint64, bool) (*model.MarketingScript, error) {
	r.reads++
	return nil, errors.New("不应读取")
}
func TestMarketingV21_MissingVersionRejectsBeforeDependencies(t *testing.T) {
	for _, op := range []string{"adapt", "adaptation", "prompt", "restore"} {
		t.Run(op, func(t *testing.T) {
			repo := &versionProbeRepo{}
			svc := service.NewMarketingService(service.MarketingDeps{Repo: repo})
			var err error
			switch op {
			case "adapt":
				_, err = svc.Adapt(context.Background(), 7, 3, &model.AdaptMarketingScriptReq{})
			case "adaptation":
				_, err = svc.EditAdaptation(context.Background(), 7, 3, &model.EditMarketingAdaptationReq{Format: "h3", Prompt: "新稿"})
			case "prompt":
				_, err = svc.EditPrompt(context.Background(), 7, 3, &model.EditMarketingPromptReq{Format: "h3", Prompt: "新稿"})
			case "restore":
				_, err = svc.RestorePrompt(context.Background(), 7, 3, &model.RestoreMarketingPromptReq{Format: "h3"})
			}
			wantMktCode(t, err, errcode.ErrMarketingConflict.Code)
			if repo.reads != 0 {
				t.Fatal("缺版本时必须在加载或调用模型前拒绝")
			}
		})
	}
}

func TestMarketingV21_RenderRoundMissingOriginalTargetsFailsBeforeLLM(t *testing.T) {
	m, e := m2AdaptMemory(t)
	_, err := e.svc.RenderRound(context.Background(), 7, m.r.ID, &model.RenderMarketingRoundReq{IdempotencyKey: "old-click"})
	wantMktCode(t, err, errcode.ErrInvalidParams.Code)
	if e.caller.count(mktAdaptStep) != 0 || len(m.ledgers) != 0 {
		t.Fatal("无原目标凭据不得调用模型或收费")
	}
}
