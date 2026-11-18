package service_test

import (
	"context"
	"errors"
	"testing"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/errcode"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/jsonhash"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
	"video-canvas/internal/service/dramasegmentfake"
)

type segmentProbeCache struct {
	row    *model.AssetMediaProbe
	writes int
	err    error
}

func (p *segmentProbeCache) Get(context.Context, uint64, string, int16) (*model.AssetMediaProbe, error) {
	if p.err != nil {
		return nil, p.err
	}
	if p.row == nil {
		return nil, repository.ErrNotFound
	}
	return p.row, nil
}
func (p *segmentProbeCache) Upsert(_ context.Context, row *model.AssetMediaProbe) error {
	p.writes++
	p.row = row
	return nil
}

type segmentProbeRepo struct {
	*dramasegmentfake.Repo
	inTx bool
}

func (r *segmentProbeRepo) WithTx(ctx context.Context, fn func(repository.DramaTx) error) error {
	r.inTx = true
	defer func() { r.inTx = false }()
	return r.Repo.WithTx(ctx, fn)
}

func TestDramaSegmentService_EditProbeCacheAndRollback(t *testing.T) {
	ctx := context.Background()
	repo := &segmentProbeRepo{Repo: dramasegmentfake.New()}
	taskID, canvasID := uint64(31), uint64(9)
	repo.Rows = []model.DramaSegment{{BaseModel: model.BaseModel{ID: 11}, UserID: 1, EpisodeID: 10, Seconds: 5, Revision: 1, ChosenTaskID: &taskID}}
	repo.Tasks = []model.GenerationTask{{ID: taskID, UserID: 1, CanvasProjectID: &canvasID, NodeID: "seg:" + idcodec.Encode(11), Kind: model.KindVideo, Status: model.TaskSucceeded, OutputJSON: []byte(`[{"asset_id":17,"media_type":"video"}]`)}}
	assets := &segEditAssets{assets: map[uint64]*model.Asset{17: {ID: 17, UserID: 1, Kind: model.KindVideo, Sha256: jsonhash.SHA256Hex([]byte("fake-video")), StorageKey: "video.mp4"}}}
	cache := &segmentProbeCache{}
	calls := 0
	svc := service.NewDramaSegmentService(service.DramaSegmentDeps{Repo: repo, Tasks: repo.Repo, Assets: assets, Probes: cache,
		ProbeVideo: func(context.Context, string) (videoframe.NormalizedProbe, error) {
			if repo.inTx {
				t.Fatal("探测不能在事务内执行")
			}
			calls++
			return videoframe.NormalizedProbe{FpsNum: 24, FpsDen: 1, NormalizedFrames24: 100}, nil
		},
	})
	in, out := int32(24), int32(43)
	view, err := svc.Update(ctx, 1, 11, &model.UpdateDramaSegmentReq{Revision: 1, Edit: &model.DramaSegmentEditPatch{SourceInF: &in, SourceOutF: model.NullableInt32{Set: true, Value: &out}}})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || cache.writes != 1 || view.SourceFrameCount == nil || *view.SourceFrameCount != 100 || view.Edit.SourceInF != 24 || repo.EditSchema != 1 || repo.SegmentVersion != 1 {
		t.Fatalf("缓存/版本/精裁不符：calls=%d writes=%d view=%+v repo=%+v", calls, cache.writes, view, repo.Repo)
	}
	wrong := idcodec.ID(99)
	_, err = svc.Update(ctx, 1, 11, &model.UpdateDramaSegmentReq{Revision: view.Revision, ExpectedTaskID: &wrong, Edit: &model.DramaSegmentEditPatch{MuteOriginal: segmentEditBool(true)}})
	segmentCode(t, err, errcode.ErrCanvasConflict.Code)
	invalidOut := int32(101)
	_, err = svc.Update(ctx, 1, 11, &model.UpdateDramaSegmentReq{Revision: view.Revision, Edit: &model.DramaSegmentEditPatch{SourceOutF: model.NullableInt32{Set: true, Value: &invalidOut}}})
	segmentCode(t, err, errcode.ErrDramaEditInvalid.Code)
	if repo.Rows[0].Revision != view.Revision || *repo.Rows[0].SourceOutF != 43 || calls != 1 || repo.SegmentVersion != 1 {
		t.Fatal("非法编辑必须回滚且复用缓存")
	}
	cache.err = errors.New("probe cache unavailable")
	_, err = svc.Update(ctx, 1, 11, &model.UpdateDramaSegmentReq{Revision: view.Revision, Edit: &model.DramaSegmentEditPatch{MuteOriginal: segmentEditBool(true)}})
	if !errors.Is(err, cache.err) {
		t.Fatalf("缓存故障不能伪装成未命中：%v", err)
	}
	cache.err = nil
	subGain := 0.004
	rev := view.Revision
	for i := 0; i < 2; i++ {
		gView, gErr := svc.Update(ctx, 1, 11, &model.UpdateDramaSegmentReq{Revision: rev, Edit: &model.DramaSegmentEditPatch{AudioGainDB: &subGain}})
		if gErr != nil || gView.Edit.AudioGainDB != 0 || repo.SegmentVersion != 1 {
			t.Fatalf("重复 0.004 不应递增版本: err=%v view=%+v ver=%d", gErr, gView, repo.SegmentVersion)
		}
		rev = gView.Revision
	}
}

func TestDramaSegmentService_ProbeRejectsTamperedBytesAndIgnoresV1Cache(t *testing.T) {
	ctx := context.Background()
	repo := dramasegmentfake.New()
	taskID, canvasID := uint64(31), uint64(9)
	repo.Rows = []model.DramaSegment{{BaseModel: model.BaseModel{ID: 11}, UserID: 1, EpisodeID: 10, Seconds: 5, Revision: 1, ChosenTaskID: &taskID}}
	repo.Tasks = []model.GenerationTask{{ID: taskID, UserID: 1, CanvasProjectID: &canvasID, NodeID: "seg:" + idcodec.Encode(11), Kind: model.KindVideo, Status: model.TaskSucceeded, OutputJSON: []byte(`[{"asset_id":17,"media_type":"video"}]`)}}

	// 1. Mismatched downloaded bytes must fail before ProbeVideo or probes.Upsert can poison cache.
	mismatchAssets := &segEditAssets{assets: map[uint64]*model.Asset{17: {ID: 17, UserID: 1, Kind: model.KindVideo, Sha256: jsonhash.SHA256Hex([]byte("other-video")), StorageKey: "video.mp4"}}}
	probes := &versionedProbeCache{rows: map[int16]*model.AssetMediaProbe{}}
	calls := 0
	mismatchSvc := service.NewDramaSegmentService(service.DramaSegmentDeps{
		Repo: repo, Tasks: repo, Assets: mismatchAssets, Probes: probes,
		ProbeVideo: func(context.Context, string) (videoframe.NormalizedProbe, error) {
			calls++
			return videoframe.NormalizedProbe{FpsNum: 24, FpsDen: 1, NormalizedFrames24: 120}, nil
		},
	})
	in := int32(1)
	if _, err := mismatchSvc.Update(ctx, 1, 11, &model.UpdateDramaSegmentReq{Revision: 1, Edit: &model.DramaSegmentEditPatch{SourceInF: &in}}); err == nil || calls != 0 || len(probes.rows) != 0 {
		t.Fatalf("tampered segment bytes must not reach ProbeVideo or seed probe cache: err=%v calls=%d rows=%+v", err, calls, probes.rows)
	}

	// 2. Legacy v1 cached probe must be ignored; verified bytes re-probe under NormalizedProbeVersion (v3).
	sha := jsonhash.SHA256Hex([]byte("fake-video"))
	validAssets := &segEditAssets{assets: map[uint64]*model.Asset{17: {ID: 17, UserID: 1, Kind: model.KindVideo, Sha256: sha, StorageKey: "video.mp4"}}}
	probes.rows[1] = &model.AssetMediaProbe{AssetID: 17, Sha256: sha, ProbeVersion: 1, NormalizedFrames24: 999}
	verifiedSvc := service.NewDramaSegmentService(service.DramaSegmentDeps{
		Repo: repo, Tasks: repo, Assets: validAssets, Probes: probes,
		ProbeVideo: func(context.Context, string) (videoframe.NormalizedProbe, error) {
			calls++
			return videoframe.NormalizedProbe{FpsNum: 24, FpsDen: 1, NormalizedFrames24: 100}, nil
		},
	})
	view, err := verifiedSvc.Update(ctx, 1, 11, &model.UpdateDramaSegmentReq{Revision: 1, Edit: &model.DramaSegmentEditPatch{SourceInF: &in}})
	if err != nil || calls != 1 || view.SourceFrameCount == nil || *view.SourceFrameCount != 100 || probes.rows[videoframe.NormalizedProbeVersion] == nil {
		t.Fatalf("verified probe must bypass legacy v1 entry: err=%v calls=%d view=%+v rows=%+v", err, calls, view, probes.rows)
	}
}

func segmentEditBool(v bool) *bool { return &v }
