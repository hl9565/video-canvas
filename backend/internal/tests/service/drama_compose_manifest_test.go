package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
)

func (r *composeMemory) LockEpisodes(_ context.Context, user uint64, ids []uint64) ([]model.DramaEpisode, error) {
	return r.Episodes(context.Background(), user, 1)
}
func (r *composeMemory) LockSegments(context.Context, uint64, []uint64) ([]model.DramaSegment, error) {
	return r.segments, nil
}
func (r *composeMemory) ComposeAsset(_ context.Context, user, id uint64) (*model.Asset, error) {
	return &model.Asset{ID: id, UserID: user, Kind: model.KindVideo, Sha256: strings.Repeat("a", 64)}, nil
}

func TestDramaComposeManifest_FrozenVariantsAndFinalJob(t *testing.T) {
	r := newComposeMemory()
	r.episodes[0].EditSchema = 1
	r.episodes[0].SegmentVersion = 3
	s, p := previewService(r)
	ctx := context.Background()
	view, err := s.ResolveEpisodeManifest(ctx, 7, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	req := composeReq(t, `,"compose_request_key":"two-variants","compose":{"segment_version":3,"manifest_hash":"`+view.ManifestHash+`","size":"1080p","variants":["packaged","clean"]}`)
	job, err := s.CreateJob(ctx, 7, 1, req)
	if err != nil {
		t.Fatal(err)
	}
	var input struct {
		Manifest     *model.DramaEpisodeManifest `json:"manifest"`
		ManifestHash string                      `json:"manifest_hash"`
	}
	if err = json.Unmarshal(r.jobs[0].InputJSON, &input); err != nil || input.Manifest == nil || input.ManifestHash != view.ManifestHash {
		t.Fatalf("未冻结清单: %s %v", r.jobs[0].InputJSON, err)
	}
	p.duringManifest = func() { r.episodes[0].SegmentVersion++ }
	if err = s.HandleDrama(ctx, 7, 1, uint64(job.ID)); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.ListJobs(ctx, 7, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	result := jobs[0].Result
	if result == nil || len(result.Exports) != 2 || result.Exports[0].Variant != "clean" || result.Exports[1].Variant != "packaged" || result.Exports[0].Width != 1920 || r.episodes[0].FinalComposeJobID == nil || *r.episodes[0].FinalComposeJobID != uint64(job.ID) || r.episodes[0].FinalSegmentVersion != 3 {
		t.Fatalf("变体或主片引用错误: %+v %+v", result, r.episodes[0])
	}
	if p.callsManifest != 1 || p.calls != 0 || p.input.Outputs[0].Width != 1920 || p.input.Outputs[0].Height != 1080 || !p.input.Outputs[1].WithOverlays {
		t.Fatalf("未走新版渲染: %+v", p)
	}
}

func TestDramaComposeManifest_ProbeOutsideLocksAndTx2VersionFence(t *testing.T) {
	r := newComposeMemory()
	r.episodes[0].EditSchema = 1
	_, a, probes := manifestService(r)
	probes.rows = map[string]*model.AssetMediaProbe{}
	s := service.NewDramaService(service.DramaDeps{Repo: r, Assets: a, Probes: probes, Compose: &manifestProcessor{}, ProbeVideo: func(context.Context, string) (videoframe.NormalizedProbe, error) {
		r.episodes[0].SegmentVersion++
		return videoframe.NormalizedProbe{NormalizedFrames24: 120}, nil
	}})
	_, err := s.CreateJob(context.Background(), 7, 1, composeReq(t, `,"compose_request_key":"changed-during-probe"`))
	dramaCheckCode(t, err, 91001)
	if len(r.jobs) != 0 || len(r.assets) != 0 || r.creditCalls != 0 {
		t.Fatal("tx2 冲突写入或冻结")
	}
}

type failingVariantAssets struct {
	*manifestFiles
	discardCtx error
}

func (a *failingVariantAssets) PrepareDramaVideo(ctx context.Context, user uint64, path string) (*model.Asset, error) {
	if a.prepared == 1 {
		return nil, errors.New("second upload failed")
	}
	return a.composeFiles.PrepareDramaVideo(ctx, user, path)
}
func (a *failingVariantAssets) DiscardCopies(ctx context.Context, assets []*model.Asset) {
	a.discardCtx = ctx.Err()
	a.composeFiles.DiscardCopies(ctx, assets)
}

func TestDramaComposeManifest_SecondUploadFailureDiscardsFirst(t *testing.T) {
	r := newComposeMemory()
	r.episodes[0].EditSchema = 1
	_, base, probes := manifestService(r)
	probes.rows[base.sha] = &model.AssetMediaProbe{NormalizedFrames24: 120}
	a := &failingVariantAssets{manifestFiles: base}
	p := &manifestProcessor{}
	s := service.NewDramaService(service.DramaDeps{Repo: r, Assets: a, Probes: probes, Compose: p})
	job, err := s.CreateJob(context.Background(), 7, 1, composeReq(t, `,"compose":{"variants":["clean","packaged"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.HandleDrama(context.Background(), 7, 1, uint64(job.ID)); err != nil {
		t.Fatal(err)
	}
	if a.discarded != 1 || a.discardCtx != nil || len(r.assets) != 0 || r.episodes[0].FinalAssetID != nil {
		t.Fatalf("部分发布或未清理: %+v", a)
	}
}

func TestDramaComposeManifest_OlderCompletionCannotReplaceNewerFinal(t *testing.T) {
	r := newComposeMemory()
	r.episodes[0].EditSchema = 1
	r.episodes[0].SegmentVersion = 3
	s, _ := previewService(r)
	job, err := s.CreateJob(context.Background(), 7, 1, composeReq(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	newer := uint64(200)
	newerJob := uint64(201)
	r.episodes[0].FinalAssetID = &newer
	r.episodes[0].FinalComposeJobID = &newerJob
	r.episodes[0].FinalSegmentVersion = 4
	if err = s.HandleDrama(context.Background(), 7, 1, uint64(job.ID)); err != nil {
		t.Fatal(err)
	}
	if *r.episodes[0].FinalAssetID != newer || *r.episodes[0].FinalComposeJobID != newerJob || r.episodes[0].FinalSegmentVersion != 4 {
		t.Fatal("旧完成任务覆盖较新成片")
	}
	if r.jobs[0].Status != "succeeded" {
		t.Fatal("旧完成任务仍应保留自己的可读导出结果")
	}
}

type changedComposeHashRepo struct{ *composeMemory }

func (r *changedComposeHashRepo) WithTx(ctx context.Context, fn func(repository.DramaTx) error) error {
	return r.composeMemory.WithTx(ctx, func(repository.DramaTx) error { return fn(r) })
}
func (r *changedComposeHashRepo) ComposeAsset(_ context.Context, user, id uint64) (*model.Asset, error) {
	return &model.Asset{ID: id, UserID: user, Kind: model.KindVideo, Sha256: strings.Repeat("b", 64)}, nil
}

func TestDramaComposeManifest_Tx2ChecksAssetContentHash(t *testing.T) {
	memory := newComposeMemory()
	memory.episodes[0].EditSchema = 1
	repo := &changedComposeHashRepo{memory}
	_, files, probes := manifestService(memory)
	probes.rows[files.sha] = &model.AssetMediaProbe{NormalizedFrames24: 120}
	svc := service.NewDramaService(service.DramaDeps{Repo: repo, Assets: files, Probes: probes})
	_, err := svc.CreateJob(context.Background(), 7, 1, composeReq(t, ""))
	dramaCheckCode(t, err, 91001)
	if len(memory.jobs) != 0 || memory.creditCalls != 0 {
		t.Fatal("素材哈希变化仍然入队")
	}
}

func TestDramaComposeManifest_PromptOnlyEditDoesNotInvalidateFrozenMedia(t *testing.T) {
	r := newComposeMemory()
	r.episodes[0].EditSchema = 1
	_, files, probes := manifestService(r)
	svc := service.NewDramaService(service.DramaDeps{Repo: r, Assets: files, Probes: probes, ProbeVideo: func(context.Context, string) (videoframe.NormalizedProbe, error) {
		r.segments[0].Prompt = "只改未来生成提示词"
		r.segments[0].Revision++
		return videoframe.NormalizedProbe{NormalizedFrames24: 120}, nil
	}})
	if _, err := svc.CreateJob(context.Background(), 7, 1, composeReq(t, "")); err != nil {
		t.Fatalf("未改变已采用视频与剪点，不应拒绝合成: %v", err)
	}
}

type multiEpComposeMemory struct {
	*composeMemory
}

func (m *multiEpComposeMemory) WithTx(ctx context.Context, fn func(repository.DramaTx) error) error {
	return m.composeMemory.WithTx(ctx, func(repository.DramaTx) error { return fn(m) })
}

func (m *multiEpComposeMemory) LockEpisodes(ctx context.Context, user uint64, ids []uint64) ([]model.DramaEpisode, error) {
	eps, err := m.Episodes(ctx, user, 1)
	if err != nil {
		return nil, err
	}
	want := make(map[uint64]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	out := make([]model.DramaEpisode, 0, len(ids))
	for _, ep := range eps {
		if _, ok := want[ep.ID]; ok {
			out = append(out, ep)
		}
	}
	return out, nil
}

func (m *multiEpComposeMemory) LockSegments(_ context.Context, user uint64, ids []uint64) ([]model.DramaSegment, error) {
	want := make(map[uint64]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	out := make([]model.DramaSegment, 0, len(ids))
	for _, seg := range m.segments {
		if _, ok := want[seg.ID]; ok && seg.UserID == user {
			out = append(out, seg)
		}
	}
	return out, nil
}

func (m *multiEpComposeMemory) CreateAsset(_ context.Context, asset *model.Asset) error {
	if err := m.composeMemory.CreateAsset(context.Background(), asset); err != nil {
		return err
	}
	asset.ID = uint64(98 + len(m.assets))
	m.assets[len(m.assets)-1].ID = asset.ID
	return nil
}

func TestDramaComposeManifest_Schema0FrozenRatioInvalidation(t *testing.T) {
	base := newComposeMemory()
	base.project.Ratio = "9:16"
	t21, t22 := base.tasks[0], base.tasks[0]
	t21.ID, t21.TaskRef, t21.IdempotencyKey, t21.NodeID = 21, "u-t21", "u-k21", "seg:"+idcodec.Encode(7)
	t22.ID, t22.TaskRef, t22.IdempotencyKey, t22.NodeID = 22, "u-t22", "u-k22", "seg:"+idcodec.Encode(8)
	base.tasks = append(base.tasks, t21, t22)
	c21, c22 := t21.ID, t22.ID
	base.episodes = append(base.episodes,
		model.DramaEpisode{BaseModel: model.BaseModel{ID: 4}, UserID: 7, DramaID: 1, No: 2, Revision: 1, LockedAt: base.episodes[0].LockedAt, EditSchema: 0, SegmentVersion: 0},
		model.DramaEpisode{BaseModel: model.BaseModel{ID: 5}, UserID: 7, DramaID: 1, No: 3, Revision: 1, LockedAt: base.episodes[0].LockedAt, EditSchema: 0, SegmentVersion: 0},
	)
	base.segments = append(base.segments,
		model.DramaSegment{BaseModel: model.BaseModel{ID: 7}, UserID: 7, EpisodeID: 4, Seq: 0, ChosenTaskID: &c21},
		model.DramaSegment{BaseModel: model.BaseModel{ID: 8}, UserID: 7, EpisodeID: 5, Seq: 0, ChosenTaskID: &c22},
	)
	r := &multiEpComposeMemory{composeMemory: base}
	_, files, probes := manifestService(base)
	svc := service.NewDramaService(service.DramaDeps{
		Repo: r, Segments: r, Assets: files, Probes: probes, Compose: &manifestProcessor{},
		ProbeVideo: func(context.Context, string) (videoframe.NormalizedProbe, error) {
			return videoframe.NormalizedProbe{FpsNum: 24, FpsDen: 1, NormalizedFrames24: 120}, nil
		},
	})
	t.Cleanup(svc.ClosePreviews)
	ctx := context.Background()

	// 1. 第 1 集：schema=0 + compose.size=720p 在排队中；第 3 集：真无清单（Compose=nil）。
	queuedJob, err := svc.CreateJob(ctx, 7, 1, &model.DramaJobReq{
		Step: "compose", Episodes: []int{1}, ComposeRequestKey: "u-s0-queued",
		Compose: &model.DramaComposeReq{Size: "720p", Variants: []string{"clean"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	nilJob, err := svc.CreateJob(ctx, 7, 1, &model.DramaJobReq{
		Step: "compose", Episodes: []int{3}, ComposeRequestKey: "u-s0-nil",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.HandleDrama(ctx, 7, 1, uint64(nilJob.ID)); err != nil {
		t.Fatal(err)
	}

	// 2. 第 2 集：schema=0 + compose.size=720p 先完成发布。
	pubJob, err := svc.CreateJob(ctx, 7, 1, &model.DramaJobReq{
		Step: "compose", Episodes: []int{2}, ComposeRequestKey: "u-s0-pub",
		Compose: &model.DramaComposeReq{Size: "720p", Variants: []string{"clean"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.HandleDrama(ctx, 7, 1, uint64(pubJob.ID)); err != nil {
		t.Fatal(err)
	}

	// 3. 切换工程画幅 9:16 -> 16:9。
	r169 := "16:9"
	view, err := svc.Update(ctx, 7, 1, &model.UpdateDramaReq{Revision: r.project.Revision, Ratio: &r169})
	if err != nil {
		t.Fatal(err)
	}
	if view.Episodes[0].SegmentVersion != 1 || view.Episodes[1].SegmentVersion != 1 || !view.Episodes[1].FinalStale || view.Episodes[2].SegmentVersion != 0 || view.Episodes[2].FinalStale {
		t.Fatalf("改画幅后状态不符合预期: ep1=%+v ep2=%+v ep3=%+v", view.Episodes[0], view.Episodes[1], view.Episodes[2])
	}

	// 4. 若第 1 集旧画幅排队任务延迟完成，落库后仍呈 final_stale=true；若新画幅已先发布则旧任务不得覆盖新成片。
	if err = svc.HandleDrama(ctx, 7, 1, uint64(queuedJob.ID)); err != nil {
		t.Fatal(err)
	}
	view, err = svc.Get(ctx, 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Episodes[0].FinalStale {
		t.Fatalf("排队中的旧画幅冻结任务完成后应标记 final_stale=true: %+v", view.Episodes[0])
	}
	// 验证已有更高 FinalSegmentVersion 时延迟旧画幅任务不会覆盖。
	newerJob, err := svc.CreateJob(ctx, 7, 1, &model.DramaJobReq{
		Step: "compose", Episodes: []int{1}, ComposeRequestKey: "u-s0-newer-169",
		Compose: &model.DramaComposeReq{Size: "720p", Variants: []string{"clean"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	olderResubmitID := uint64(queuedJob.ID)
	r.jobs[0].Status = "queued"
	if err = svc.HandleDrama(ctx, 7, 1, uint64(newerJob.ID)); err != nil {
		t.Fatal(err)
	}
	newerFinal := *r.episodes[0].FinalAssetID
	if err = svc.HandleDrama(ctx, 7, 1, olderResubmitID); err != nil {
		t.Fatal(err)
	}
	if *r.episodes[0].FinalAssetID != newerFinal || r.episodes[0].FinalSegmentVersion != 1 {
		t.Fatalf("延迟旧画幅冻结任务覆盖了新版成片: asset=%d ver=%d", *r.episodes[0].FinalAssetID, r.episodes[0].FinalSegmentVersion)
	}
}
