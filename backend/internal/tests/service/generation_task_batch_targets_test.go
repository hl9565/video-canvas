package service_test

import (
	"context"
	"slices"
	"testing"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
	"video-canvas/internal/service/dramasegmentfake"
)

type batchTargetRepo struct {
	*fakeTaskRepo
	drama        *batchDramaTargets
	insertGap    bool
	creditLocked bool
}

func (r *batchTargetRepo) WithTx(ctx context.Context, fn func(repository.GenerationTaskTx) error) error {
	return r.fakeTaskRepo.WithTx(ctx, func(repository.GenerationTaskTx) error { return fn(r) })
}
func (r *batchTargetRepo) DramaTx() repository.DramaTx { return r.drama }
func (r *batchTargetRepo) TxEnsureCredit(ctx context.Context, userID uint64, initial int) error {
	r.creditLocked = true
	if r.insertGap {
		r.drama.Rows = append(r.drama.Rows, model.DramaSegment{BaseModel: model.BaseModel{ID: 2}, UserID: 1, EpisodeID: 10, Seconds: 5, Revision: 1})
	}
	return r.fakeTaskRepo.TxEnsureCredit(ctx, userID, initial)
}

type batchDramaTargets struct {
	*dramasegmentfake.Repo
	store *batchTargetRepo
	t     *testing.T
	locks []string
}

func (d *batchDramaTargets) targetRead() {
	d.t.Helper()
	if d.store.creditLocked {
		d.t.Fatal("积分锁内禁止重新解析业务目标")
	}
}
func (d *batchDramaTargets) Segment(ctx context.Context, u, sid uint64, lock bool) (*model.DramaSegment, error) {
	d.targetRead()
	return d.Repo.Segment(ctx, u, sid, lock)
}
func (d *batchDramaTargets) SegmentEpisode(ctx context.Context, u, e uint64) (*model.DramaEpisode, error) {
	d.targetRead()
	return d.Repo.SegmentEpisode(ctx, u, e)
}
func (d *batchDramaTargets) LockProjects(ctx context.Context, u uint64, ids []uint64) ([]model.DramaProject, error) {
	d.targetRead()
	d.locks = append(d.locks, "project")
	ps := []model.DramaProject{}
	for _, id := range ids {
		p, err := d.Project(ctx, u, id, true)
		if err != nil {
			return nil, err
		}
		ps = append(ps, *p)
	}
	return ps, nil
}
func (d *batchDramaTargets) LockEpisodes(ctx context.Context, u uint64, ids []uint64) ([]model.DramaEpisode, error) {
	d.targetRead()
	d.locks = append(d.locks, "episode")
	return d.Repo.LockEpisodes(ctx, u, ids)
}
func (d *batchDramaTargets) LockSegments(ctx context.Context, u uint64, ids []uint64) ([]model.DramaSegment, error) {
	d.targetRead()
	d.locks = append(d.locks, "segment")
	return d.Repo.LockSegments(ctx, u, ids)
}
func (d *batchDramaTargets) Jobs(ctx context.Context, u, p uint64, active bool) ([]model.DramaJob, error) {
	d.targetRead()
	return d.Repo.Jobs(ctx, u, p, active)
}
func (d *batchDramaTargets) Segments(ctx context.Context, u, e uint64) ([]model.DramaSegment, error) {
	d.targetRead()
	return d.Repo.Segments(ctx, u, e)
}
func (d *batchDramaTargets) SegmentTasks(_ context.Context, u, c uint64, node string) ([]model.GenerationTask, error) {
	out := []model.GenerationTask{}
	for _, task := range d.store.tasks {
		if task.UserID == u && task.CanvasProjectID != nil && *task.CanvasProjectID == c && task.NodeID == node && !task.IsTest {
			out = append(out, *task)
		}
	}
	return out, nil
}

func TestGenerationTaskService_BatchPreResolvedMissingGap(t *testing.T) {
	env := newTaskSvcEnv(&fakeLimits{initial: 100, defMax: 10})
	env.registry.snap = v21MultiRegistry(10).snap
	repo := &batchTargetRepo{fakeTaskRepo: env.repo, insertGap: true}
	targets := &batchDramaTargets{Repo: dramasegmentfake.New(), store: repo, t: t}
	targets.Rows = []model.DramaSegment{{BaseModel: model.BaseModel{ID: 1}, UserID: 1, EpisodeID: 10, Seconds: 5, Revision: 1}}
	repo.drama = targets
	svc := service.NewGenerationTaskService(service.GenerationTaskDeps{Repo: repo, Registry: env.registry, Assets: env.assets, Broadcaster: env.bc, Limits: &fakeLimits{initial: 100, defMax: 10}})
	resp, err := svc.Create(context.Background(), 1, "gap", &model.CreateGenerationTaskReq{Kind: model.KindVideo, ModelID: "m1", CanvasID: 9, NodeIDs: []string{"seg:" + idcodec.Encode(1), "seg:" + idcodec.Encode(2)}, Input: map[string]any{"prompt": "镜头", "count": 2}})
	if err != nil || resp.Items[0].Task == nil || resp.Items[1].Error == nil || resp.Items[1].Error.Code != errcode.ErrDramaSegmentNotFound.Code {
		t.Fatalf("预锁后插入不能改变缺失快照：%+v %v", resp, err)
	}
	if !slices.Equal(targets.locks, []string{"project", "episode", "segment"}) {
		t.Fatalf("锁序=%v", targets.locks)
	}
	if len(repo.tasks) != 1 || len(env.bc.msgs) != 1 {
		t.Fatal("只有预解析有效的项可入库与推送")
	}
}
