package service_test

import (
	"context"
	"errors"
	"testing"

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
