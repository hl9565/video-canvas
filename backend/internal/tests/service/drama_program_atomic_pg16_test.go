package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"gorm.io/gorm"

	"video-canvas/internal/model"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/jsonhash"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
	"video-canvas/internal/tests/testutil"
)

// goldenPGRender keeps the worker inside rendering until the independent PUT commits.
type goldenPGRender struct {
	*videoframe.Extractor
	entered, committed chan struct{}
}

func (r *goldenPGRender) ComposeProgramManifest(ctx context.Context, in videoframe.ComposeProgramInput) (videoframe.ProgramComposeReport, error) {
	close(r.entered)
	report, err := r.Extractor.ComposeProgramManifest(ctx, in)
	if err != nil {
		return report, err
	}
	select {
	case <-r.committed:
		return report, nil
	case <-ctx.Done():
		return report, ctx.Err()
	}
}

type goldenPGFiles struct {
	*goldenProgramFiles
	dir                      string
	staged                   int
	firstRegistered, publish chan struct{}
	gated                    bool
}

func (a *goldenPGFiles) PrepareDramaVideo(_ context.Context, user uint64, path string) (*model.Asset, error) {
	a.staged++
	key := fmt.Sprintf("golden-export-%d.mp4", a.staged)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(a.dir, key), raw, 0600); err != nil {
		return nil, err
	}
	return &model.Asset{UserID: user, Kind: model.KindVideo, Source: model.AssetSourceUpload, StorageKey: key,
		MimeType: "video/mp4", ByteSize: int64(len(raw)), Sha256: jsonhash.SHA256Hex(raw)}, nil
}

// ViewOf pauses after the real transaction inserts its first asset row.
func (a *goldenPGFiles) ViewOf(ctx context.Context, asset *model.Asset) (*model.AssetView, error) {
	if !a.gated {
		if asset.ID == 0 {
			return nil, fmt.Errorf("export view called before asset registration")
		}
		a.gated = true
		if err := waitSlice5PGRelease(ctx, a.firstRegistered, a.publish); err != nil {
			return nil, err
		}
	}
	return a.goldenProgramFiles.ViewOf(ctx, asset)
}

func (a *goldenPGFiles) DiscardCopies(_ context.Context, assets []*model.Asset) {
	for _, asset := range assets {
		_ = os.Remove(filepath.Join(a.dir, asset.StorageKey))
	}
}

func seedGoldenPGSources(t *testing.T, db *gorm.DB, f *testutil.GoldenProgram) {
	t.Helper()
	for _, m := range f.Media {
		mustTakeSQL(t, db.Create(&m.Asset))
	}
	for _, shot := range f.Shots {
		if shot.Task != nil {
			task := *shot.Task
			seedComposePGTask(&task)
			task.TaskRef = fmt.Sprintf("golden-t%d", task.ID)
			task.IdempotencyKey = task.TaskRef
			mustTakeSQL(t, db.Create(&task))
		}
		mustTakeSQL(t, db.Create(&shot.Segment))
	}
}

func TestDramaProgramGoldenAtomicPG16(t *testing.T) {
	db, repo := newProgramPG16(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	e := testutil.GoldenExtractor()
	if !e.Available() {
		t.Fatal("PG16 release verification requires ffmpeg and ffprobe")
	}
	f := testutil.NewGoldenProgram(t, 7, 3, 2)
	seedGoldenPGSources(t, db.WithContext(ctx), f)
	queue := &slice2cQueue{}
	repo.SetEnqueuer(queue)
	render := &goldenPGRender{Extractor: e, entered: make(chan struct{}), committed: make(chan struct{})}
	files := &goldenPGFiles{goldenProgramFiles: &goldenProgramFiles{
		composeFiles: &composeFiles{}, media: f.Media, lookup: repo.ComposeAsset,
	}, dir: t.TempDir(), firstRegistered: make(chan struct{}), publish: make(chan struct{})}
	deps := goldenDramaDeps(repo, files, repository.NewAssetMediaProbeRepository(db), e)
	deps.Compose = render
	svc := service.NewDramaService(deps)
	t.Cleanup(svc.ClosePreviews)
	initial := putGoldenProgram(t, svc, f, 4)
	manifest := goldenManifest(t, svc)
	req := goldenComposeRequest(manifest, "golden-atomic-pg16", "clean", "packaged")
	job, err := svc.CreateJob(ctx, 7, 1, req)
	if err != nil || queue.calls.Load() != 1 {
		t.Fatalf("queue golden compose: %+v %v", job, err)
	}
	jobID := uint64(job.ID)
	done := make(chan error, 1)
	workerExited := make(chan struct{})
	go func() {
		defer close(workerExited)
		done <- svc.HandleDrama(ctx, 7, 1, jobID)
	}()
	defer func() {
		cancel()
		<-workerExited
	}()
	waitSlice5PGGate(ctx, t, render.entered, done)
	saved := goldenPGConcurrentPut(ctx, t, db, svc, f, initial.SegmentVersion)
	close(render.committed)
	waitSlice5PGGate(ctx, t, files.firstRegistered, done)
	assertGoldenPGUnpublished(ctx, t, db, jobID)
	close(files.publish)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	checkGoldenPGPublication(ctx, t, db, svc, jobID, manifest, saved)
}

func goldenPGConcurrentPut(ctx context.Context, t *testing.T, db *gorm.DB, svc *service.DramaService, f *testutil.GoldenProgram, version int64) model.DramaEpisode {
	t.Helper()
	program := f.Program
	program.Audio = slices.Clone(program.Audio)
	program.Audio[1].GainDB = -5
	payload, err := json.Marshal(service.PutDramaProgramReq{
		ExpectedEpisodeID: 3, ExpectedSegmentVersion: &version, Program: program,
	})
	if err != nil {
		t.Fatal(err)
	}
	putCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req := httptest.NewRequestWithContext(putCtx, http.MethodPut, "/dramas/"+idcodec.Encode(1)+"/episodes/1/program", bytes.NewReader(payload))
	w := httptest.NewRecorder()
	programPGHTTP(svc).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("concurrent PUT: %d %s", w.Code, w.Body.String())
	}
	var saved model.DramaEpisode
	mustTakeSQL(t, db.WithContext(ctx).First(&saved, 3))
	stored, err := model.DecodeStoredDramaProgram(json.RawMessage(saved.ProgramJSON))
	if err != nil || saved.SegmentVersion != version+1 || len(stored.Audio) != 6 ||
		stored.Audio[1].ID != "vo_3" || stored.Audio[1].GainDB != -5 || stored.Audio[0].BoundSource == nil {
		t.Fatalf("concurrent edit did not commit: version=%d program=%s (%v)", saved.SegmentVersion, saved.ProgramJSON, err)
	}
	return saved
}

func assertGoldenPGUnpublished(ctx context.Context, t *testing.T, db *gorm.DB, jobID uint64) {
	t.Helper()
	var episode model.DramaEpisode
	var job model.DramaJob
	var count int64
	db = db.WithContext(ctx)
	mustTakeSQL(t, db.First(&episode, 3))
	mustTakeSQL(t, db.First(&job, jobID))
	mustTakeSQL(t, db.Model(&model.Asset{}).Where("storage_key LIKE ?", "golden-export-%").Count(&count))
	if episode.FinalAssetID != nil || episode.FinalComposeJobID != nil || job.Status != "running" || count != 0 {
		t.Fatalf("partial compose became visible: final=%v job=%s exports=%d", episode.FinalAssetID, job.Status, count)
	}
}

func checkGoldenPGPublication(ctx context.Context, t *testing.T, db *gorm.DB, svc *service.DramaService, jobID uint64, manifest *service.DramaManifestV2ResponseView, saved model.DramaEpisode) {
	t.Helper()
	db = db.WithContext(ctx)
	var final model.DramaEpisode
	var job model.DramaJob
	mustTakeSQL(t, db.First(&final, saved.ID))
	mustTakeSQL(t, db.First(&job, jobID))
	frozen := readGoldenFrozen(t, job)
	checkGoldenPGSnapshot(t, frozen, manifest)
	if final.SegmentVersion != saved.SegmentVersion || !bytes.Equal(final.ProgramJSON, saved.ProgramJSON) ||
		final.FinalSegmentVersion != manifest.SegmentVersion || final.FinalComposeJobID == nil || *final.FinalComposeJobID != jobID {
		t.Fatalf("publish overwrote concurrent edit or lost frozen provenance: %+v", final)
	}
	view, err := svc.Get(ctx, 7, 1)
	if err != nil || len(view.Episodes) == 0 || !view.Episodes[0].FinalStale {
		t.Fatalf("published older snapshot must be stale: %+v %v", view, err)
	}
	jobs, err := svc.ListJobs(ctx, 7, 1, false)
	if err != nil || len(jobs) != 1 || jobs[0].Status != "succeeded" || jobs[0].Result == nil {
		t.Fatalf("expected succeeded job with two exports: %+v %v", jobs, err)
	}
	checkGoldenPGExports(t, db, jobs[0].Result.Exports, final.FinalAssetID, manifest.ManifestHash)
}

func checkGoldenPGSnapshot(t *testing.T, frozen goldenFrozenInput, manifest *service.DramaManifestV2ResponseView) {
	t.Helper()
	m := frozen.ManifestV2
	if frozen.ManifestHash != manifest.ManifestHash || m.TotalFrames != 708 || len(m.Clips) != 10 || len(m.Program.Audio) != 6 ||
		m.SegmentVersion != manifest.SegmentVersion || m.Program.Audio[1].GainDB != 0 ||
		m.Clips[testutil.GoldenS6A].SegmentID == m.Clips[testutil.GoldenS6B].SegmentID ||
		m.Clips[testutil.GoldenS7].TaskID != 115 || m.Clips[testutil.GoldenS7].AssetID != 133 {
		t.Fatalf("unexpected frozen snapshot: %+v", m)
	}
}

func checkGoldenPGExports(t *testing.T, db *gorm.DB, exports []model.DramaComposeExport, primary *uint64, manifestHash string) {
	t.Helper()
	var assets []model.Asset
	mustTakeSQL(t, db.Where("storage_key LIKE ?", "golden-export-%").Find(&assets))
	if len(exports) != 2 || len(assets) != 2 || primary == nil || *primary != exports[0].Asset.ID ||
		exports[0].Variant != "clean" || exports[1].Variant != "packaged" || exports[0].Asset.ID == exports[1].Asset.ID {
		t.Fatalf("expected two registered variants and clean primary: %+v", exports)
	}
	for _, ex := range exports {
		if ex.DramaComposeExportReport == nil || ex.Width != 1280 || ex.Height != 720 || ex.ManifestHash != manifestHash ||
			ex.LoudnessStatus != "accepted" || len(ex.AudioSHA256) != 64 || ex.AudioSHA256 != exports[0].AudioSHA256 ||
			ex.LUFS == nil || math.Abs(*ex.LUFS+16) > 0.5 || ex.TruePeak == nil || *ex.TruePeak > -1.5 {
			t.Fatalf("unexpected export report: %+v", ex)
		}
		if !slices.ContainsFunc(assets, func(a model.Asset) bool { return a.ID == ex.Asset.ID && a.Sha256 == ex.Sha256 && a.ByteSize > 0 }) {
			t.Fatalf("export missing committed asset row: %+v", ex)
		}
	}
}
