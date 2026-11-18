package service_test

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
	"video-canvas/internal/service/dramasegmentfake"
)

type contendedPGSegmentSnapshots struct{ *repository.DramaRepository }

func (contendedPGSegmentSnapshots) WithSnapshotTx(context.Context, func(repository.DramaTx) error) error {
	return context.DeadlineExceeded
}

type segmentPGAssetViews struct{ pgTakeAssets }

func (segmentPGAssetViews) ViewOf(ctx context.Context, row *model.Asset) (*model.AssetView, error) {
	return (segmentSnapshotAssetViews{}).ViewOf(ctx, row)
}

func seedSegmentPGRecovery(t *testing.T, db *gorm.DB) {
	t.Helper()
	fixture := newComposeMemory()
	clip := &fixture.segments[0]
	clip.ChosenTaskID, clip.Revision = nil, 1
	task := &fixture.tasks[0]
	task.FrameRunID, task.FrameRunConfirmRev = new(uint64(31)), new(int64(2))
	seedComposePGTask(task)
	run := &model.DramaSegmentFrameRun{
		ID: 31, UserID: 7, SegmentID: clip.ID, Revision: 1, SegmentRevision: clip.Revision,
		State: model.FrameRunStateVideoPending, AdoptIfEmpty: true, VideoTaskID: &task.ID, ConfirmedVersion: 2,
		RequestKey: "segment-single-conn", RequestHash: verifiedVideoHash(), FrameSource: model.FrameSourceExisting,
	}
	video := &model.Asset{ID: 30, UserID: 7, Kind: model.KindVideo, Sha256: verifiedVideoHash(),
		StorageKey: "source.mp4", Source: model.AssetSourceUpload}
	probe := &model.AssetMediaProbe{AssetID: video.ID, Sha256: video.Sha256, ProbeVersion: videoframe.NormalizedProbeVersion,
		FpsNum: 24, FpsDen: 1, NormalizedFrames24: 120}
	for _, row := range []any{clip, &fixture.segments[1], task, run, video, probe} {
		mustTakeSQL(t, db.Create(row))
	}
}

func TestDramaSegmentPG16_RecoveryOrderAndDeleteWithOneConnection(t *testing.T) {
	db, repo := newProgramPG16(t)
	seedSegmentPGRecovery(t, db)
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	assets := segmentPGAssetViews{pgTakeAssets{db}}
	probes := repository.NewAssetMediaProbeRepository(db)
	feed := dramasegmentfake.New()
	deps := service.DramaSegmentDeps{Repo: repo, Assets: assets, Probes: probes, Broadcaster: feed}
	svc := service.NewDramaSegmentService(deps)
	ep, recovered, err := svc.ListEpisode(ctx, 7, 1, 1)
	if err != nil || len(recovered) != 2 || ep.SegmentVersion != 5 {
		t.Fatalf("single-connection recovery failed: ep=%+v rows=%+v err=%v", ep, recovered, err)
	}
	assertCommittedClip(t, recovered[0], 20, 31, 120)
	if recovered[0].FrameRun.State != model.FrameRunStateBound {
		t.Fatalf("recovery did not advance the run: %+v", recovered[0].FrameRun)
	}

	deps.Repo = contendedPGSegmentSnapshots{repo}
	svc = service.NewDramaSegmentService(deps)
	ordered, err := svc.Order(ctx, 7, 1, 1, &model.OrderDramaSegmentsReq{IDs: []idcodec.ID{6, 5}})
	if err != nil || len(ordered) != 2 || ordered[1].ID != 5 || ordered[1].Seq != 1 {
		t.Fatalf("Order with unavailable follow-up snapshots: %+v %v", ordered, err)
	}
	assertCommittedClip(t, ordered[1], 20, 31, 120)
	assertPublishedClip(t, feed, 5, 6, 20, 31, 120)
	if err := svc.Delete(ctx, 7, 6); err != nil {
		t.Fatal(err)
	}
	assertPublishedClip(t, feed, 5, 7, 20, 31, 120)
	ep, after, err := service.NewDramaSegmentService(service.DramaSegmentDeps{
		Repo: repo, Assets: assets, Probes: probes,
	}).ListEpisode(ctx, 7, 1, 1)
	if err != nil || ep.SegmentVersion != 7 || len(after) != 1 || after[0].ID != 5 || after[0].Seq != 0 {
		t.Fatalf("Delete did not commit reindexed membership: ep=%+v rows=%+v err=%v", ep, after, err)
	}
	assertCommittedClip(t, after[0], 20, 31, 120)
}
