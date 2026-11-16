package service_test

import (
	"context"
	"testing"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/ws"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
)

type replayRaceRepo struct {
	*composeMemory
	lookups   int
	hitAt     int
	winner    model.DramaJob
	duplicate bool
}

func (r *replayRaceRepo) WithTx(ctx context.Context, fn func(repository.DramaTx) error) error {
	return r.composeMemory.WithTx(ctx, func(repository.DramaTx) error { return fn(r) })
}
func (r *replayRaceRepo) JobByComposeKey(context.Context, uint64, uint64, string) (*model.DramaJob, error) {
	r.lookups++
	if r.lookups >= r.hitAt {
		j := r.winner
		return &j, nil
	}
	return nil, repository.ErrNotFound
}
func (r *replayRaceRepo) InsertJob(ctx context.Context, j *model.DramaJob) error {
	if r.duplicate {
		return repository.ErrDuplicate
	}
	return r.composeMemory.InsertJob(ctx, j)
}

type replayBroadcast struct{ calls int }

func (b *replayBroadcast) Publish(context.Context, string, ws.Message) { b.calls++ }

func TestDramaComposeIntent_AllFourReplayExitsRejectTampering(t *testing.T) {
	// 先通过真实公共提交获得本请求指纹，再模拟竞争赢家在四个出口出现。
	reference := newComposeMemory()
	s := composeService(reference, &composeFiles{r: reference}, &composeProcessor{})
	req := composeReq(t, `,"compose_request_key":"race"`)
	if _, err := s.CreateJob(context.Background(), 7, 1, req); err != nil {
		t.Fatal(err)
	}
	signature := reference.jobs[0].RequestIntentSig
	for _, exit := range []struct {
		name      string
		hitAt     int
		duplicate bool
	}{{"approved", 1, false}, {"tx1", 2, false}, {"tx2", 3, false}, {"unique-conflict-recovery", 4, true}} {
		for _, same := range []bool{true, false} {
			t.Run(exit.name+map[bool]string{true: "/same", false: "/different"}[same], func(t *testing.T) {
				memory := newComposeMemory()
				winner := reference.jobs[0]
				winner.ID = 999
				winner.Status = "succeeded"
				winner.RequestIntentSig = signature
				if !same {
					winner.RequestIntentSig = "different-intent"
				}
				repo := &replayRaceRepo{composeMemory: memory, winner: winner, hitAt: exit.hitAt, duplicate: exit.duplicate}
				bc := &replayBroadcast{}
				svc := service.NewDramaService(service.DramaDeps{Repo: repo, Assets: &composeFiles{r: memory}, Compose: &composeProcessor{}, Broadcaster: bc})
				got, err := svc.CreateJob(context.Background(), 7, 1, req)
				if same {
					if err != nil || uint64(got.ID) != 999 {
						t.Fatalf("同意图未恢复赢家: %+v %v", got, err)
					}
				} else {
					dramaCheckCode(t, err, 40015)
				}
				if len(memory.jobs) != 0 || len(memory.assets) != 0 || memory.creditCalls != 0 || bc.calls != 0 {
					t.Fatal("重放出口产生写入、冻结或推送")
				}
			})
		}
	}
}
