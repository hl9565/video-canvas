package service_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"video-canvas/internal/handler"
	"video-canvas/internal/middleware"
	"video-canvas/internal/migration"
	"video-canvas/internal/model"
	"video-canvas/internal/pkg/idcodec"
	"video-canvas/internal/pkg/jsonhash"
	"video-canvas/internal/pkg/videoframe"
	"video-canvas/internal/provider"
	"video-canvas/internal/repository"
	"video-canvas/internal/service"
	"video-canvas/migrations"
)

// slice2cPG16 建立独立 PG16 schema；只从真实 SQL 迁移建库，不能由 AutoMigrate 补齐越界列。
func slice2cPG16(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("未设置 TEST_DATABASE_DSN，PG16 Slice 2C 集成测试需真库补跑")
	}
	cfg := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err = admin.Raw("SHOW server_version_num").Scan(&version).Error; err != nil {
		t.Fatal(err)
	}
	if version < 160000 || version >= 170000 {
		t.Fatalf("此验收要求 PostgreSQL 16，实际 %d", version)
	}
	schema := fmt.Sprintf("slice2c_%d", time.Now().UnixNano())
	if err = admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(postgres.Open(dsn+" search_path="+schema), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if pool, e := db.DB(); e == nil {
			_ = pool.Close()
		}
		_ = admin.Exec("DROP SCHEMA " + schema + " CASCADE").Error
		if pool, e := admin.DB(); e == nil {
			_ = pool.Close()
		}
	})
	return db
}

const (
	slice2cUpstream35Only         = false
	slice2cUpstream35WithFork9001 = true
	composeStageAMigration        = "9001_drama_edit.sql"
	composeStageBMigration        = "9002_drama_frame_runs.sql"
)

// slice2cMigrationFiles 固定上游截至 0035，显式选择是否追加 Fork 9001，不纳入未来上游 0036/0037 或其他 Fork。
func slice2cMigrationFiles(t *testing.T, source fs.FS, includeFork bool) fstest.MapFS {
	t.Helper()
	if includeFork {
		return dramaComposeMigrationSubset(t, source, "0035", composeStageAMigration)
	}
	return dramaComposeMigrationSubset(t, source, "0035")
}

func dramaComposeMigrationSubset(t *testing.T, source fs.FS, lastUpstream string, forkFiles ...string) fstest.MapFS {
	t.Helper()
	subset := fstest.MapFS{}
	names, err := fs.Glob(source, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		prefix, _, ok := strings.Cut(name, "_")
		upstream := ok && prefix >= "0001" && prefix <= lastUpstream
		fork := slices.Contains(forkFiles, name)
		if !upstream && !fork {
			continue
		}
		data, e := fs.ReadFile(source, name)
		if e != nil {
			t.Fatal(e)
		}
		subset[name] = &fstest.MapFile{Data: data}
	}
	return subset
}

func slice2cMigrate(t *testing.T, db *gorm.DB, includeFork bool) {
	t.Helper()
	if err := migration.Run(context.Background(), db, migration.Options{FS: slice2cMigrationFiles(t, migrations.FS, includeFork), Logger: zap.NewNop()}); err != nil {
		t.Fatal(err)
	}
}

func migrateCurrentSchemaPG16(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := migration.Run(context.Background(), db, migration.Options{FS: migrations.FS, Logger: zap.NewNop()}); err != nil {
		t.Fatal(err)
	}
}

func TestDramaComposeMigrationFiles_FixtureStages(t *testing.T) {
	source := fstest.MapFS{}
	for _, name := range []string{
		"0001_baseline.sql", "0035_marketing.sql", "0036_upstream.sql", "0037_upstream.sql",
		"0038_upstream.sql", "9001_drama_edit.sql", "9002_drama_frame_runs.sql", "9003_drama_program.sql",
	} {
		source[name] = &fstest.MapFile{Data: []byte("SELECT 1;")}
	}
	for _, includeFork := range []bool{slice2cUpstream35Only, slice2cUpstream35WithFork9001} {
		names, err := fs.Glob(slice2cMigrationFiles(t, source, includeFork), "*.sql")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"0001_baseline.sql", "0035_marketing.sql"}
		if includeFork {
			want = append(want, "9001_drama_edit.sql")
		}
		if !slices.Equal(names, want) {
			t.Fatalf("includeFork=%v，得到 %v，期望 %v", includeFork, names, want)
		}
	}
	stageB := dramaComposeMigrationSubset(t, source, "0037", composeStageAMigration, composeStageBMigration)
	names, err := fs.Glob(stageB, "*.sql")
	want := []string{"0001_baseline.sql", "0035_marketing.sql", "0036_upstream.sql", "0037_upstream.sql", "9001_drama_edit.sql", "9002_drama_frame_runs.sql"}
	if err != nil || !slices.Equal(names, want) {
		t.Fatalf("Stage B 夹具：得到 %v，期望 %v / %v", names, want, err)
	}
}

func TestDramaComposePG16_0035UpgradeLegacyApprovalReplay(t *testing.T) {
	db := slice2cPG16(t)
	slice2cMigrate(t, db, slice2cUpstream35Only)
	for _, stmt := range []string{
		`INSERT INTO drama_projects (id,user_id,canvas_project_id,title,source_text,style_key) VALUES (1,7,2,'剧','文','custom')`,
		`INSERT INTO drama_jobs (id,user_id,drama_id,step,episodes,status,approval_id,input_json) VALUES (10,7,1,'compose','[1]','succeeded',55,'{"episode_id":3,"segment_version":4,"asset_ids":[30]}')`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
	slice2cMigrate(t, db, slice2cUpstream35WithFork9001)
	repo := repository.NewDramaRepository(db)
	svc := service.NewDramaService(service.DramaDeps{Repo: repo})
	job, err := svc.CreateAgentJob(context.Background(), 7, 1, 55, composeReq(t, `,"compose":{"segment_version":4}`))
	if err != nil || uint64(job.ID) != 10 {
		t.Fatalf("升级后同审批未重放: %+v %v", job, err)
	}
	for _, extra := range []string{`,"compose":{"size":"1080p"}`, `,"compose_request_key":"new-key"`} {
		_, err = svc.CreateAgentJob(context.Background(), 7, 1, 55, composeReq(t, extra))
		dramaCheckCode(t, err, 90001)
	}
	var count int64
	if err = db.Model(&model.DramaJob{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("升级重试创建第二单: %d %v", count, err)
	}
}

type slice2cQueue struct{ calls atomic.Int32 }

func (q *slice2cQueue) EnqueueDramaTx(context.Context, *sql.Tx, uint64, uint64, uint64) error {
	q.calls.Add(1)
	return nil
}

func TestDramaComposePG16_ConcurrentKeyAndSchemaStageBOnly(t *testing.T) {
	db := slice2cPG16(t)
	migrateCurrentSchemaPG16(t, db)
	fixture := newComposeMemory()
	fixture.episodes[0].EditSchema = 1
	seedComposePGTask(&fixture.tasks[0])
	for _, value := range []any{&fixture.project, &fixture.episodes[0], &fixture.segments[0], &fixture.segments[1], &fixture.tasks[0], &model.Asset{ID: 30, UserID: 7, Kind: model.KindVideo, Sha256: verifiedVideoHash(), StorageKey: "source.mp4", Source: model.AssetSourceUpload}} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	repo := repository.NewDramaRepository(db)
	q := &slice2cQueue{}
	repo.SetEnqueuer(q)
	_, files, _ := manifestService(fixture)
	probeRepo := repository.NewAssetMediaProbeRepository(db)
	if err := probeRepo.Upsert(context.Background(), &model.AssetMediaProbe{AssetID: 30, Sha256: files.sha, ProbeVersion: videoframe.NormalizedProbeVersion, FpsNum: 24, FpsDen: 1, NormalizedFrames24: 120}); err != nil {
		t.Fatal(err)
	}
	svc := service.NewDramaService(service.DramaDeps{Repo: repo, Assets: files, Probes: probeRepo})
	start := make(chan struct{})
	results := make(chan *model.DramaJobView, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			v, err := svc.CreateJob(context.Background(), 7, 1, &model.DramaJobReq{Step: "compose", Episodes: []int{1}, ComposeRequestKey: "concurrent"})
			results <- v
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id uint64
	for v := range results {
		if id != 0 && uint64(v.ID) != id {
			t.Fatal("同键返回不同 job")
		}
		id = uint64(v.ID)
	}
	if q.calls.Load() != 1 {
		t.Fatalf("重复入队: %d", q.calls.Load())
	}
	stored, err := repo.Job(context.Background(), 7, 1, id, false)
	if err != nil || stored.RequestIntentSig == "" {
		t.Fatalf("未持久化意图: %+v %v", stored, err)
	}

	t.Run("pinned_stage_b_schema", assertComposeStageBOnly)
}

func assertComposeStageBOnly(t *testing.T) {
	t.Helper()
	db := slice2cPG16(t)
	files := dramaComposeMigrationSubset(t, migrations.FS, "0037", composeStageAMigration, composeStageBMigration)
	if err := migration.Run(context.Background(), db, migration.Options{FS: files, Logger: zap.NewNop()}); err != nil {
		t.Fatal(err)
	}
	var columns int64
	if err := db.Raw(`SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND column_name IN ('program_json','visual_kind','still_asset_id','fx_json')`).Scan(&columns).Error; err != nil || columns != 0 {
		t.Fatalf("Stage B schema 不应有 PR3 字段：%d / %v", columns, err)
	}
}

func seedComposePGTask(t *model.GenerationTask) {
	t.TaskRef = "t20-ref"
	t.ModelKey = "m1"
	t.Provider = "p1"
	t.IdempotencyKey = "k-t20"
	t.InputJSON = []byte("{}")
	t.ConfigSnapshot = []byte("{}")
}

func TestDramaComposePG16_ProbeReleasesLocksAndTx2RejectsConcurrentEdit(t *testing.T) {
	db := slice2cPG16(t)
	migrateCurrentSchemaPG16(t, db)
	fixture := newComposeMemory()
	fixture.episodes[0].EditSchema = 1
	seedComposePGTask(&fixture.tasks[0])
	for _, value := range []any{&fixture.project, &fixture.episodes[0], &fixture.segments[0], &fixture.segments[1], &fixture.tasks[0], &model.Asset{ID: 30, UserID: 7, Kind: model.KindVideo, Sha256: verifiedVideoHash(), StorageKey: "source.mp4", Source: model.AssetSourceUpload}} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	repo := repository.NewDramaRepository(db)
	q := &slice2cQueue{}
	repo.SetEnqueuer(q)
	_, files, _ := manifestService(fixture)
	entered, release := make(chan struct{}), make(chan struct{})
	svc := service.NewDramaService(service.DramaDeps{Repo: repo, Assets: files, Probes: repository.NewAssetMediaProbeRepository(db), ProbeVideo: func(ctx context.Context, _ string) (videoframe.NormalizedProbe, error) {
		close(entered)
		select {
		case <-release:
			return videoframe.NormalizedProbe{FpsNum: 24, FpsDen: 1, NormalizedFrames24: 120}, nil
		case <-ctx.Done():
			return videoframe.NormalizedProbe{}, ctx.Err()
		}
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := svc.CreateJob(ctx, 7, 1, &model.DramaJobReq{Step: "compose", Episodes: []int{1}, ComposeRequestKey: "edit-during-probe"})
		result <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("未进入锁外探测")
	}
	editCtx, editCancel := context.WithTimeout(ctx, time.Second)
	defer editCancel()
	err := repo.WithTx(editCtx, func(tx repository.DramaTx) error {
		if _, err := tx.Project(editCtx, 7, 1, true); err != nil {
			return err
		}
		rows, err := tx.LockEpisodes(editCtx, 7, []uint64{3})
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			return repository.ErrNotFound
		}
		segments, err := tx.LockSegments(editCtx, 7, []uint64{5})
		if err != nil {
			return err
		}
		segments[0].MuteOriginal = true
		return tx.SaveSegment(editCtx, &segments[0], segments[0].Revision)
	})
	close(release)
	if err != nil {
		t.Fatalf("探测期间不应占用 Project/Episode/Segment 锁: %v", err)
	}
	select {
	case err = <-result:
		dramaCheckCode(t, err, 91001)
	case <-ctx.Done():
		t.Fatal("提交未结束")
	}
	if q.calls.Load() != 0 {
		t.Fatal("过期清单仍然入队")
	}
}

func TestDramaComposePG16_RatioChangeInvalidatesAndBlocksStaleSubmit(t *testing.T) {
	db := slice2cPG16(t)
	migrateCurrentSchemaPG16(t, db)

	fixture := newComposeMemory()
	fixture.project.Ratio = "16:9"
	finalID := uint64(30)
	// 新版（edit_schema=1）已定版集 + 旧版（edit_schema=0）对照组，验证画幅变更仅失效受影响的新版集。
	fixture.episodes[0].EditSchema = 1
	fixture.episodes[0].SegmentVersion = 2
	fixture.episodes[0].FinalAssetID = &finalID
	fixture.episodes[0].FinalSegmentVersion = 2
	legacyEp := model.DramaEpisode{
		BaseModel: model.BaseModel{ID: 4}, UserID: 7, DramaID: 1, No: 2, Revision: 1,
		LockedAt: fixture.episodes[0].LockedAt, EditSchema: 0, SegmentVersion: 1, FinalAssetID: &finalID, FinalSegmentVersion: 1,
	}
	seedComposePGTask(&fixture.tasks[0])
	for _, value := range []any{
		&fixture.project,
		&fixture.episodes[0],
		&legacyEp,
		&fixture.segments[0],
		&fixture.segments[1],
		&fixture.tasks[0],
		&model.Asset{ID: 30, UserID: 7, Kind: model.KindVideo, Sha256: verifiedVideoHash(), StorageKey: "source.mp4", Source: model.AssetSourceUpload},
	} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}

	repo := repository.NewDramaRepository(db)
	q := &slice2cQueue{}
	repo.SetEnqueuer(q)
	_, files, _ := manifestService(fixture)
	ctx := context.Background()

	// 子测试 1 & 2：改画幅触发新版集版本递增与 final_stale=true；相同画幅和未传画幅均不递增。
	svc := service.NewDramaService(service.DramaDeps{
		Repo: repo, Assets: files, Probes: repository.NewAssetMediaProbeRepository(db),
		ProbeVideo: func(context.Context, string) (videoframe.NormalizedProbe, error) {
			return videoframe.NormalizedProbe{FpsNum: 24, FpsDen: 1, NormalizedFrames24: 120}, nil
		},
	})
	view0, err := svc.Get(ctx, 7, 1)
	if err != nil || view0.Episodes[0].FinalStale {
		t.Fatalf("初始已定版分集应新鲜: err=%v view=%+v", err, view0)
	}

	sameRatio := "16:9"
	updatedSame, err := svc.Update(ctx, 7, 1, &model.UpdateDramaReq{Revision: view0.Revision, Ratio: &sameRatio})
	if err != nil {
		t.Fatal(err)
	}
	if updatedSame.Episodes[0].SegmentVersion != 2 || updatedSame.Episodes[0].FinalStale {
		t.Fatalf("相同画幅不应递增 segment_version: %+v", updatedSame.Episodes[0])
	}

	newRatio := "9:16"
	updatedDiff, err := svc.Update(ctx, 7, 1, &model.UpdateDramaReq{Revision: updatedSame.Revision, Ratio: &newRatio})
	if err != nil {
		t.Fatal(err)
	}
	if updatedDiff.Episodes[0].SegmentVersion != 3 || !updatedDiff.Episodes[0].FinalStale {
		t.Fatalf("画幅变更未失效新版已定版分集: %+v", updatedDiff.Episodes[0])
	}
	if updatedDiff.Episodes[1].SegmentVersion != 1 || updatedDiff.Episodes[1].FinalStale {
		t.Fatalf("旧版分集不应受画幅重版递增影响: %+v", updatedDiff.Episodes[1])
	}

	// 子测试 3：合成两阶段期间（锁外探测）发生画幅变更，tx2 重验必须拦截并拒绝入队（含未升版旧版集走显式清单路径）。
	task7 := fixture.tasks[0]
	task7.ID = 21
	task7.TaskRef = "t21-ref"
	task7.IdempotencyKey = "k-t21"
	task7.NodeID = "seg:" + idcodec.Encode(7)
	chosen7 := task7.ID
	if err := db.Create(&task7).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.DramaSegment{BaseModel: model.BaseModel{ID: 7}, UserID: 7, EpisodeID: 4, Seq: 0, ChosenTaskID: &chosen7}).Error; err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		epNo int
		key  string
		size string
	}{
		{name: "edit_schema=1", epNo: 1, key: "mid-submit-ratio-v1", size: "720p"},
		{name: "edit_schema=0 explicit manifest", epNo: 2, key: "mid-submit-ratio-v0", size: "720p"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			submitSvc := service.NewDramaService(service.DramaDeps{
				Repo: repo, Assets: files, Probes: repository.NewAssetMediaProbeRepository(db),
				ProbeVideo: func(ctx context.Context, _ string) (videoframe.NormalizedProbe, error) {
					close(entered)
					select {
					case <-release:
						return videoframe.NormalizedProbe{FpsNum: 24, FpsDen: 1, NormalizedFrames24: 120}, nil
					case <-ctx.Done():
						return videoframe.NormalizedProbe{}, ctx.Err()
					}
				},
			})
			// 清除探针缓存确保进入锁外探测钩子。
			if err := db.Exec("DELETE FROM asset_media_probes").Error; err != nil {
				t.Fatal(err)
			}
			cur, err := submitSvc.Get(ctx, 7, 1)
			if err != nil {
				t.Fatal(err)
			}
			beforeCalls := q.calls.Load()
			subCtx, subCancel := context.WithTimeout(ctx, 5*time.Second)
			defer subCancel()
			resCh := make(chan error, 1)
			go func() {
				_, e := submitSvc.CreateJob(subCtx, 7, 1, &model.DramaJobReq{
					Step: "compose", Episodes: []int{tc.epNo}, ComposeRequestKey: tc.key,
					Compose: &model.DramaComposeReq{Size: tc.size, Variants: []string{"clean"}},
				})
				resCh <- e
			}()
			select {
			case <-entered:
			case err := <-resCh:
				t.Fatalf("未进入锁外探测即返回: %v", err)
			case <-subCtx.Done():
				t.Fatal("未进入锁外探测")
			}
			targetRatio := "3:4"
			if cur.Ratio == "3:4" {
				targetRatio = "16:9"
			}
			if _, err := submitSvc.Update(subCtx, 7, 1, &model.UpdateDramaReq{Revision: cur.Revision, Ratio: &targetRatio}); err != nil {
				t.Fatal(err)
			}
			close(release)
			select {
			case err = <-resCh:
				dramaCheckCode(t, err, 91001)
			case <-subCtx.Done():
				t.Fatal("提交未结束")
			}
			if q.calls.Load() != beforeCalls {
				t.Fatal("画幅并发变更后仍错误入队")
			}
		})
	}
}

func seedSchema0RatioPG16(t *testing.T, db *gorm.DB) {
	t.Helper()
	fixture := newComposeMemory()
	fixture.project.Ratio = "9:16"
	fixture.episodes[0].SegmentVersion = 1
	seedComposePGTask(&fixture.tasks[0])
	lockedAt := fixture.episodes[0].LockedAt
	ep2 := model.DramaEpisode{BaseModel: model.BaseModel{ID: 4}, UserID: 7, DramaID: 1, No: 2, Revision: 1, LockedAt: lockedAt, EditSchema: 0, SegmentVersion: 1}
	ep3 := model.DramaEpisode{BaseModel: model.BaseModel{ID: 5}, UserID: 7, DramaID: 1, No: 3, Revision: 1, LockedAt: lockedAt, EditSchema: 0, SegmentVersion: 1}
	ep4 := model.DramaEpisode{BaseModel: model.BaseModel{ID: 6}, UserID: 7, DramaID: 1, No: 4, Revision: 1, LockedAt: lockedAt, EditSchema: 0, SegmentVersion: 1}
	task21 := fixture.tasks[0]
	task21.ID, task21.TaskRef, task21.IdempotencyKey, task21.NodeID = 21, "t21-ref", "k-t21", "seg:"+idcodec.Encode(7)
	task22 := fixture.tasks[0]
	task22.ID, task22.TaskRef, task22.IdempotencyKey, task22.NodeID = 22, "t22-ref", "k-t22", "seg:"+idcodec.Encode(8)
	task23 := fixture.tasks[0]
	task23.ID, task23.TaskRef, task23.IdempotencyKey, task23.NodeID = 23, "t23-ref", "k-t23", "seg:"+idcodec.Encode(9)
	c21, c22, c23 := task21.ID, task22.ID, task23.ID
	for _, value := range []any{
		&fixture.project, &fixture.episodes[0], &ep2, &ep3, &ep4,
		&fixture.segments[0], &fixture.segments[1],
		&model.DramaSegment{BaseModel: model.BaseModel{ID: 7}, UserID: 7, EpisodeID: 4, Seq: 0, ChosenTaskID: &c21},
		&model.DramaSegment{BaseModel: model.BaseModel{ID: 8}, UserID: 7, EpisodeID: 5, Seq: 0, ChosenTaskID: &c22},
		&model.DramaSegment{BaseModel: model.BaseModel{ID: 9}, UserID: 7, EpisodeID: 6, Seq: 0, ChosenTaskID: &c23},
		&fixture.tasks[0], &task21, &task22, &task23,
		&model.Asset{ID: 30, UserID: 7, Kind: model.KindVideo, Sha256: verifiedVideoHash(), StorageKey: "source.mp4", Source: model.AssetSourceUpload},
	} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func latestPGComposeJobID(t *testing.T, db *gorm.DB, episodeID uint64) (uint64, string) {
	t.Helper()
	var row struct {
		ID           uint64
		ManifestType sql.NullString
	}
	err := db.Raw(
		"SELECT id, jsonb_typeof(input_json->'manifest') AS manifest_type FROM drama_jobs WHERE user_id = 7 AND drama_id = 1 AND step = 'compose' AND (input_json->>'episode_id') = ? ORDER BY id DESC LIMIT 1",
		fmt.Sprint(episodeID),
	).Scan(&row).Error
	if err != nil || row.ID == 0 {
		t.Fatalf("未找到 episode=%d 的 compose 任务: id=%d err=%v", episodeID, row.ID, err)
	}
	return row.ID, row.ManifestType.String
}

type pgUniqueComposeAssets struct {
	*manifestFiles
	seq atomic.Int64
}

func (a *pgUniqueComposeAssets) PrepareDramaVideo(ctx context.Context, user uint64, path string) (*model.Asset, error) {
	asset, err := a.manifestFiles.PrepareDramaVideo(ctx, user, path)
	if err != nil {
		return nil, err
	}
	asset.StorageKey = fmt.Sprintf("compose-%d.mp4", a.seq.Add(1))
	return asset, nil
}

func TestDramaComposePG16_Schema0FrozenManifestRatioInvalidation(t *testing.T) {
	db := slice2cPG16(t)
	migrateCurrentSchemaPG16(t, db)
	seedSchema0RatioPG16(t, db)

	repo := repository.NewDramaRepository(db)
	q := &slice2cQueue{}
	repo.SetEnqueuer(q)
	files, _ := verifiedManifestAssets(newComposeMemory())
	proc := &manifestProcessor{}
	svc := service.NewDramaService(service.DramaDeps{
		Repo: repo, Assets: &pgUniqueComposeAssets{manifestFiles: files}, Probes: repository.NewAssetMediaProbeRepository(db), Compose: proc,
		ProbeVideo: func(context.Context, string) (videoframe.NormalizedProbe, error) {
			return videoframe.NormalizedProbe{FpsNum: 24, FpsDen: 1, NormalizedFrames24: 120}, nil
		},
	})
	ctx := context.Background()

	// 第 1 集（ID=3）：schema=0 显式 720p 冻结合成在排队态；第 4 集（ID=6）：schema=0 真无清单（Manifest=nil）在排队态。
	if _, err := svc.CreateJob(ctx, 7, 1, &model.DramaJobReq{
		Step: "compose", Episodes: []int{1}, ComposeRequestKey: "s0-frozen-queued",
		Compose: &model.DramaComposeReq{Size: "720p", Variants: []string{"clean"}},
	}); err != nil {
		t.Fatal(err)
	}
	ep1JobID, ep1MType := latestPGComposeJobID(t, db, 3)
	if ep1MType != "object" {
		t.Fatalf("schema=0 compose.size=720p 应生成 manifest object，实际 %q", ep1MType)
	}
	if _, err := svc.CreateJob(ctx, 7, 1, &model.DramaJobReq{
		Step: "compose", Episodes: []int{4}, ComposeRequestKey: "s0-legacy-nil",
	}); err != nil {
		t.Fatal(err)
	}
	ep4JobID, ep4MType := latestPGComposeJobID(t, db, 6)
	if ep4MType != "" {
		t.Fatalf("schema=0 无冻结清单任务不应写入 manifest object，实际 %q", ep4MType)
	}

	// 改画幅 9:16 -> 16:9：仅含冻结清单任务的第 1 集 segment_version 应立即递增，第 4 集真旧版保持不变。
	cur, err := svc.Get(ctx, 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	r169 := "16:9"
	cur, err = svc.Update(ctx, 7, 1, &model.UpdateDramaReq{Revision: cur.Revision, Ratio: &r169})
	if err != nil {
		t.Fatal(err)
	}
	if cur.Episodes[0].SegmentVersion != 2 || cur.Episodes[3].SegmentVersion != 1 {
		t.Fatalf("改画幅仅应递增含冻结清单任务的分集版本: ep1=%+v ep4=%+v", cur.Episodes[0], cur.Episodes[3])
	}

	// 旧 9:16 排队任务落库后第 1 集应标记 final_stale=true；真旧版第 4 集落库后 final_stale=false。
	if err = svc.HandleDrama(ctx, 7, 1, ep1JobID); err != nil {
		t.Fatal(err)
	}
	if err = svc.HandleDrama(ctx, 7, 1, ep4JobID); err != nil {
		t.Fatal(err)
	}
	cur, err = svc.Get(ctx, 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !cur.Episodes[0].FinalStale || cur.Episodes[3].FinalStale {
		t.Fatalf("预期 ep1.final_stale=true 且 ep4.final_stale=false，实际 ep1=%+v ep4=%+v", cur.Episodes[0], cur.Episodes[3])
	}

	// 第 2 集（ID=4）：schema=0 显式 720p 先完成发布，再改画幅；第 3 集（ID=5）：同集若已有更新 FinalSegmentVersion，旧画幅任务重投不得覆盖。
	if _, err = svc.CreateJob(ctx, 7, 1, &model.DramaJobReq{
		Step: "compose", Episodes: []int{2}, ComposeRequestKey: "s0-frozen-publish-ep2",
		Compose: &model.DramaComposeReq{Size: "720p", Variants: []string{"clean"}},
	}); err != nil {
		t.Fatal(err)
	}
	ep2JobID, _ := latestPGComposeJobID(t, db, 4)
	if err = svc.HandleDrama(ctx, 7, 1, ep2JobID); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.CreateJob(ctx, 7, 1, &model.DramaJobReq{
		Step: "compose", Episodes: []int{3}, ComposeRequestKey: "s0-frozen-publish-ep3",
		Compose: &model.DramaComposeReq{Size: "720p", Variants: []string{"clean"}},
	}); err != nil {
		t.Fatal(err)
	}
	ep3OldJobID, _ := latestPGComposeJobID(t, db, 5)
	cur, err = svc.Get(ctx, 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Episodes[1].FinalStale {
		t.Fatalf("第 2 集刚完成定版应新鲜: %+v", cur.Episodes[1])
	}

	// 验证以旧版本=1（改画幅前版本）再发起第 1 集合成被 91001 拒绝。
	staleVer := int64(1)
	if _, err = svc.CreateJob(ctx, 7, 1, &model.DramaJobReq{
		Step: "compose", Episodes: []int{1}, ComposeRequestKey: "s0-stale-ver-reject",
		Compose: &model.DramaComposeReq{SegmentVersion: &staleVer, Size: "720p", Variants: []string{"clean"}},
	}); err == nil {
		t.Fatal("改画幅后旧 segment_version 发起合成未被拒绝")
	} else {
		dramaCheckCode(t, err, 91001)
	}

	r34 := "3:4"
	if _, err = svc.Update(ctx, 7, 1, &model.UpdateDramaReq{Revision: cur.Revision, Ratio: &r34}); err != nil {
		t.Fatal(err)
	}
	// 模拟第 3 集旧 16:9 任务暂挂失败后，新 3:4 任务完成发布，随后延迟投递的旧 16:9 任务重跑，不得覆盖新版 FinalAssetID。
	if err = db.Model(&model.DramaJob{}).Where("id = ?", ep3OldJobID).Update("status", "failed").Error; err != nil {
		t.Fatal(err)
	}
	if _, err = svc.CreateJob(ctx, 7, 1, &model.DramaJobReq{
		Step: "compose", Episodes: []int{3}, ComposeRequestKey: "s0-frozen-newer",
		Compose: &model.DramaComposeReq{Size: "720p", Variants: []string{"clean"}},
	}); err != nil {
		t.Fatal(err)
	}
	ep3NewJobID, _ := latestPGComposeJobID(t, db, 5)
	if err = svc.HandleDrama(ctx, 7, 1, ep3NewJobID); err != nil {
		t.Fatal(err)
	}
	var ep3RowAfterNew model.DramaEpisode
	if err = db.First(&ep3RowAfterNew, 5).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Model(&model.DramaJob{}).Where("id = ?", ep3OldJobID).Update("status", "queued").Error; err != nil {
		t.Fatal(err)
	}
	if err = svc.HandleDrama(ctx, 7, 1, ep3OldJobID); err != nil {
		t.Fatal(err)
	}
	var ep3RowAfterOld model.DramaEpisode
	if err = db.First(&ep3RowAfterOld, 5).Error; err != nil {
		t.Fatal(err)
	}
	if ep3RowAfterNew.FinalAssetID == nil || ep3RowAfterOld.FinalAssetID == nil || *ep3RowAfterOld.FinalAssetID != *ep3RowAfterNew.FinalAssetID || ep3RowAfterOld.FinalSegmentVersion != 2 {
		t.Fatalf("旧画幅延迟合成覆盖了新版定版成片: new=%+v old=%+v", ep3RowAfterNew, ep3RowAfterOld)
	}

	cur, err = svc.Get(ctx, 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !cur.Episodes[1].FinalStale || cur.Episodes[1].SegmentVersion != 2 || cur.Episodes[2].FinalStale || cur.Episodes[3].FinalStale {
		t.Fatalf("已定版冻结清单变更画幅状态错误: ep2=%+v ep3=%+v ep4=%+v", cur.Episodes[1], cur.Episodes[2], cur.Episodes[3])
	}
}

func TestDramaComposePG16_HistoricalFrozenJobDoesNotStaleNilManifestExport(t *testing.T) {
	db := slice2cPG16(t)
	migrateCurrentSchemaPG16(t, db)
	seedSchema0RatioPG16(t, db)

	repo := repository.NewDramaRepository(db)
	q := &slice2cQueue{}
	repo.SetEnqueuer(q)
	files, _ := verifiedManifestAssets(newComposeMemory())
	proc := &manifestProcessor{}
	svc := service.NewDramaService(service.DramaDeps{
		Repo: repo, Assets: &pgUniqueComposeAssets{manifestFiles: files}, Probes: repository.NewAssetMediaProbeRepository(db), Compose: proc,
		ProbeVideo: func(context.Context, string) (videoframe.NormalizedProbe, error) {
			return videoframe.NormalizedProbe{FpsNum: 24, FpsDen: 1, NormalizedFrames24: 120}, nil
		},
	})
	ctx := context.Background()

	// 1. 第 4 集（ID=6, edit_schema=0）存在历史失败、取消及被替换的冻结清单合成任务，
	//    随后以 nil-manifest 整段合成定版发布（final_compose_job_id 指向 nil-manifest 任务）。
	for _, status := range []string{"failed", "canceled", "succeeded"} {
		job := model.DramaJob{
			UserID:    7,
			DramaID:   1,
			Step:      "compose",
			Episodes:  []int{4},
			Status:    status,
			Lane:      "user",
			InputJSON: []byte(`{"episode_id":6,"segment_version":1,"asset_ids":[30],"manifest":{"v":1}}`),
			Output:    []byte(`{}`),
		}
		if err := db.Create(&job).Error; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.CreateJob(ctx, 7, 1, &model.DramaJobReq{
		Step: "compose", Episodes: []int{4}, ComposeRequestKey: "s0-nil-manifest-active",
	}); err != nil {
		t.Fatal(err)
	}
	ep4JobID, ep4MType := latestPGComposeJobID(t, db, 6)
	if ep4MType != "" {
		t.Fatalf("nil-manifest 任务不应写入 manifest object，实际 %q", ep4MType)
	}
	if err := svc.HandleDrama(ctx, 7, 1, ep4JobID); err != nil {
		t.Fatal(err)
	}

	// 2. 第 2 集（ID=4, edit_schema=0）存在暂停态（paused）在途冻结清单任务；
	//    第 3 集（ID=5, edit_schema=0）当前已定版任务（final_compose_job_id）为冻结清单任务。
	pausedFrozen := model.DramaJob{
		UserID:    7,
		DramaID:   1,
		Step:      "compose",
		Episodes:  []int{2},
		Status:    "paused",
		Lane:      "user",
		InputJSON: []byte(`{"episode_id":4,"segment_version":1,"asset_ids":[30],"manifest":{"v":1}}`),
		Output:    []byte(`{}`),
	}
	if err := db.Create(&pausedFrozen).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateJob(ctx, 7, 1, &model.DramaJobReq{
		Step: "compose", Episodes: []int{3}, ComposeRequestKey: "s0-published-frozen",
		Compose: &model.DramaComposeReq{Size: "720p", Variants: []string{"clean"}},
	}); err != nil {
		t.Fatal(err)
	}
	ep3JobID, _ := latestPGComposeJobID(t, db, 5)
	if err := svc.HandleDrama(ctx, 7, 1, ep3JobID); err != nil {
		t.Fatal(err)
	}

	// 3. 改画幅 9:16 -> 16:9：第 4 集不得因历史冻结任务而递增版本号或误报 final_stale=true；
	//    第 2 集（paused 在途冻结任务）与第 3 集（已定版冻结任务）应正常递增版本并标记过期。
	cur, err := svc.Get(ctx, 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Episodes[3].FinalStale || cur.Episodes[3].SegmentVersion != 1 {
		t.Fatalf("改画幅前第 4 集应为新鲜态: %+v", cur.Episodes[3])
	}
	r169 := "16:9"
	updated, err := svc.Update(ctx, 7, 1, &model.UpdateDramaReq{Revision: cur.Revision, Ratio: &r169})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Episodes[3].SegmentVersion != 1 || updated.Episodes[3].FinalStale {
		t.Fatalf("历史失败/取消/淘汰冻结任务不应误使 nil-manifest 成片过期: ep4=%+v", updated.Episodes[3])
	}
	if updated.Episodes[1].SegmentVersion != 2 {
		t.Fatalf("paused 在途冻结任务改画幅应递增版本号: ep2=%+v", updated.Episodes[1])
	}
	if updated.Episodes[2].SegmentVersion != 2 || !updated.Episodes[2].FinalStale {
		t.Fatalf("已发布冻结清单成片改画幅应递增版本号并标记过期: ep3=%+v", updated.Episodes[2])
	}
}

// pgProgramAudio holds both requests after their unlocked snapshot reads and before either write transaction.
type pgProgramAudio struct {
	service.DramaComposeAssets
	entered chan struct{}
	proceed chan struct{}
}

func (a pgProgramAudio) Get(ctx context.Context, user, id uint64) (*model.Asset, error) {
	if a.entered != nil {
		select {
		case a.entered <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		select {
		case <-a.proceed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &model.Asset{ID: id, UserID: user, Kind: model.KindAudio}, nil
}

func newProgramPG16(t *testing.T) (*gorm.DB, *repository.DramaRepository) {
	t.Helper()
	db := slice2cPG16(t)
	migrateCurrentSchemaPG16(t, db)
	fixture := newComposeMemory()
	fixture.project.Ratio = "16:9"
	fixture.episodes[0].SegmentVersion = 4
	for _, row := range []any{
		&fixture.project, &fixture.episodes[0],
		&model.DramaEpisode{BaseModel: model.BaseModel{ID: 4}, DramaID: 1, UserID: 7, No: 2, SegmentVersion: 4},
		&model.Asset{ID: 40, UserID: 7, Kind: model.KindAudio, Source: model.AssetSourceUpload, StorageKey: "stage-c-audio.m4a"},
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db, repository.NewDramaRepository(db)
}

func programPGHTTP(svc *service.DramaService) *gin.Engine {
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(middleware.CtxUserIDKey, uint(7)) })
	r.PUT("/dramas/:id/episodes/:no/program", handler.NewDramaHandler(svc).PutProgram)
	return r
}

func putProgramPG(ctx context.Context, r *gin.Engine, ep uint64, v, end int) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"expected_episode_id":%q,"expected_segment_version":%d,"program":{"v":1,"audio":[{"id":"voice","role":"narration","asset_id":40,"start_f":0,"end_f":%d}],"overlays":[]}}`,
		idcodec.Encode(ep), v, end)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequestWithContext(ctx, http.MethodPut, "/dramas/"+idcodec.Encode(1)+"/episodes/1/program", strings.NewReader(body)))
	return w
}

func requireProgramPGStale(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	var response struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != http.StatusConflict || response.Code != 91001 {
		t.Fatalf("expected stale 409/91001: %d %s %v", w.Code, w.Body.String(), err)
	}
}

func waitProgramPGAsset(ctx context.Context, t *testing.T, a pgProgramAudio) {
	t.Helper()
	select {
	case <-a.entered:
	case <-ctx.Done():
		t.Fatal("did not reach outside-lock asset read")
	}
}

func TestDramaProgramPG16ConcurrentSameVersionPutAndNoOp(t *testing.T) {
	_, repo := newProgramPG16(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	barrier := pgProgramAudio{entered: make(chan struct{}, 2), proceed: make(chan struct{})}
	svc := service.NewDramaService(service.DramaDeps{Repo: repo, Assets: barrier})
	r := programPGHTTP(svc)
	type result struct {
		response *httptest.ResponseRecorder
		end      int
	}
	out := make(chan result, 2)
	for _, end := range []int{12, 24} {
		go func() { out <- result{putProgramPG(ctx, r, 3, 4, end), end} }()
	}
	waitProgramPGAsset(ctx, t, barrier)
	waitProgramPGAsset(ctx, t, barrier)
	close(barrier.proceed)

	var wins, winnerEnd int
	for range 2 {
		select {
		case res := <-out:
			if res.response.Code == http.StatusOK {
				wins++
				winnerEnd = res.end
			} else {
				requireProgramPGStale(t, res.response)
			}
		case <-ctx.Done():
			t.Fatal("concurrent requests did not complete")
		}
	}
	if wins != 1 {
		t.Fatalf("expected one winner, got %d", wins)
	}
	steady := programPGHTTP(service.NewDramaService(service.DramaDeps{Repo: repo, Assets: pgProgramAudio{}}))
	w := putProgramPG(ctx, steady, 3, 5, winnerEnd)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"segment_version":5`) {
		t.Fatalf("no-op changed segment version: %d %s", w.Code, w.Body.String())
	}
	requireProgramPGStale(t, putProgramPG(ctx, steady, 3, 4, winnerEnd))
	view, err := svc.GetProgram(ctx, 7, 1, 1)
	if err != nil || view.SegmentVersion != 5 || *view.Program.Audio[0].EndF != int32(winnerEnd) {
		t.Fatalf("winner was overwritten: %+v %v", view, err)
	}
}

func TestDramaProgramCASRenumberRace(t *testing.T) {
	db, repo := newProgramPG16(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	barrier := pgProgramAudio{entered: make(chan struct{}, 1), proceed: make(chan struct{})}
	svc := service.NewDramaService(service.DramaDeps{Repo: repo, Assets: barrier})
	r := programPGHTTP(svc)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- putProgramPG(ctx, r, 3, 4, 12) }()
	waitProgramPGAsset(ctx, t, barrier)
	canvas := repository.NewDramaCanvasRepository(db)
	if err := canvas.WithTx(ctx, func(tx repository.DramaCanvasTx) error {
		if err := tx.LockProject(ctx, 7, 1); err != nil {
			return err
		}
		return tx.DeleteEpisode(ctx, 7, 1, 1)
	}); err != nil {
		t.Fatal(err)
	}
	close(barrier.proceed)
	select {
	case w := <-result:
		requireProgramPGStale(t, w)
	case <-ctx.Done():
		t.Fatal("save raced with renumber without returning")
	}
	requireProgramPGStale(t, putProgramPG(ctx, r, 3, 4, 12))
	view, err := svc.GetProgram(ctx, 7, 1, 1)
	if err != nil || view.EpisodeID != 4 || view.SegmentVersion != 4 || len(view.Program.Audio) != 0 {
		t.Fatalf("renumbered episode was overwritten: %+v %v", view, err)
	}
}

type slice5PGAssets struct {
	*composeFiles
	repo *repository.DramaRepository
	seq  int
}

func (a *slice5PGAssets) Get(ctx context.Context, user, id uint64) (*model.Asset, error) {
	return a.repo.ComposeAsset(ctx, user, id)
}

func (a *slice5PGAssets) Open(ctx context.Context, user, id uint64) (*provider.AssetFile, error) {
	asset, err := a.Get(ctx, user, id)
	if err != nil {
		return nil, err
	}
	body := "audio"
	if asset.Kind == model.KindVideo {
		body = "video"
	}
	return &provider.AssetFile{Asset: asset, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func (a *slice5PGAssets) PrepareDramaVideo(ctx context.Context, user uint64, path string) (*model.Asset, error) {
	asset, err := a.composeFiles.PrepareDramaVideo(ctx, user, path)
	if err != nil {
		return nil, err
	}
	a.seq++
	asset.StorageKey = fmt.Sprintf("slice5-out-%d.mp4", a.seq)
	return asset, nil
}

func newSlice5PGCompose(t *testing.T, version int, configure func(*service.DramaDeps)) (*gorm.DB, *service.DramaService, *slice2cQueue) {
	t.Helper()
	db, repo := newProgramPG16(t)
	queue := &slice2cQueue{}
	repo.SetEnqueuer(queue)
	fixture := newComposeMemory()
	segment := &fixture.segments[0]
	if version == 2 {
		stageCSegment(segment, model.DramaVisualBlack, 24)
	} else {
		seedComposePGTask(&fixture.tasks[0])
		for _, row := range []any{
			&fixture.tasks[0],
			&model.Asset{ID: 30, UserID: 7, Kind: model.KindVideo, Sha256: verifiedVideoHash(), StorageKey: "slice5-source.mp4", Source: model.AssetSourceUpload},
		} {
			if err := db.Create(row).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := db.Create(segment).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.Asset{}).Where("id = ?", 40).Update("sha256", jsonhash.SHA256Hex([]byte("audio"))).Error; err != nil {
		t.Fatal(err)
	}
	deps := service.DramaDeps{
		Repo: repo, Assets: &slice5PGAssets{composeFiles: &composeFiles{r: fixture}, repo: repo},
		Probes: repository.NewAssetMediaProbeRepository(db),
		ProbeVideo: func(context.Context, string) (videoframe.NormalizedProbe, error) {
			return videoframe.NormalizedProbe{FpsNum: 24, FpsDen: 1, NormalizedFrames24: 120}, nil
		},
		ProbeAudio: func(context.Context, string) (videoframe.AudioProbe, error) {
			return videoframe.AudioProbe{DecodedAudioSamples: 8000}, nil
		},
	}
	configure(&deps)
	svc := service.NewDramaService(deps)
	t.Cleanup(svc.ClosePreviews)
	return db, svc, queue
}

func waitSlice5PGGate(ctx context.Context, t *testing.T, entered <-chan struct{}, early <-chan error) {
	t.Helper()
	select {
	case <-entered:
	case err := <-early:
		t.Fatalf("operation ended before the gate: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func waitSlice5PGRelease(ctx context.Context, entered, release chan struct{}) error {
	close(entered)
	select {
	case <-release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestDramaProgramComposePG16PreservesConcurrentProgramEdit(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			db, svc, _ := newSlice5PGCompose(t, version, func(d *service.DramaDeps) {
				renderer := &slice5ProgramRenderer{}
				renderer.during = func(ctx context.Context, _ videoframe.ComposeProgramInput) error {
					return waitSlice5PGRelease(ctx, entered, release)
				}
				renderer.duringManifest = func() { _ = waitSlice5PGRelease(ctx, entered, release) }
				d.Compose = renderer
			})
			router, frozenVersion := programPGHTTP(svc), 4
			if version == 2 {
				if w := putProgramPG(ctx, router, 3, frozenVersion, 12); w.Code != http.StatusOK {
					t.Fatalf("initial program: %d %s", w.Code, w.Body.String())
				}
				frozenVersion++
			}
			job, err := svc.CreateJob(ctx, 7, 1, composeReq(t, `,"compose":{"variants":["packaged","clean"]}`))
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- svc.HandleDrama(ctx, 7, 1, uint64(job.ID)) }()
			waitSlice5PGGate(ctx, t, entered, done)
			w := putProgramPG(ctx, router, 3, frozenVersion, 20)
			if w.Code != http.StatusOK {
				t.Fatalf("concurrent PUT: %d %s", w.Code, w.Body.String())
			}
			close(release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			assertSlice5PGComposePublication(ctx, t, db, svc, uint64(job.ID), frozenVersion, version)
		})
	}
}

func assertSlice5PGComposePublication(ctx context.Context, t *testing.T, db *gorm.DB, svc *service.DramaService, jobID uint64, frozenVersion, rendererVersion int) {
	t.Helper()
	view, err := svc.Get(ctx, 7, 1)
	if err != nil || len(view.Episodes) == 0 || !view.Episodes[0].FinalStale ||
		view.Episodes[0].FinalComposeJobID == nil || uint64(*view.Episodes[0].FinalComposeJobID) != jobID {
		t.Fatalf("snapshot should publish as stale with its job reference: %+v %v", view, err)
	}
	var ep model.DramaEpisode
	if err := db.First(&ep, 3).Error; err != nil {
		t.Fatal(err)
	}
	if ep.SegmentVersion != int64(frozenVersion+1) || ep.FinalSegmentVersion != int64(frozenVersion) {
		t.Fatalf("wrong snapshot versions: %+v", ep)
	}
	program, err := svc.GetProgram(ctx, 7, 1, 1)
	if err != nil || len(program.Program.Audio) != 1 || program.Program.Audio[0].EndF == nil || *program.Program.Audio[0].EndF != 20 {
		t.Fatalf("compose overwrote committed PUT: %+v %v", program, err)
	}
	jobs, err := svc.ListJobs(ctx, 7, 1, false)
	if err != nil || len(jobs) != 1 || jobs[0].Status != "succeeded" || jobs[0].Result == nil || len(jobs[0].Result.Exports) != 2 {
		t.Fatalf("variants not committed together: %+v %v", jobs, err)
	}
	for _, export := range jobs[0].Result.Exports {
		if (export.DramaComposeExportReport != nil) != (rendererVersion == 2) {
			t.Fatalf("wrong renderer report: %+v", export)
		}
	}
}

func TestDramaProgramComposePG16RejectsHashChangedDuringProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	db, svc, queue := newSlice5PGCompose(t, 2, func(d *service.DramaDeps) {
		d.ProbeAudio = func(ctx context.Context, _ string) (videoframe.AudioProbe, error) {
			err := waitSlice5PGRelease(ctx, entered, release)
			return videoframe.AudioProbe{DecodedAudioSamples: 8000}, err
		}
	})
	if w := putProgramPG(ctx, programPGHTTP(svc), 3, 4, 12); w.Code != http.StatusOK {
		t.Fatalf("initial program: %d %s", w.Code, w.Body.String())
	}
	done := make(chan error, 1)
	go func() {
		_, err := svc.CreateJob(ctx, 7, 1, composeReq(t, ""))
		done <- err
	}()
	waitSlice5PGGate(ctx, t, entered, done)
	if err := db.WithContext(ctx).Model(&model.Asset{}).Where("id = ?", 40).Update("sha256", strings.Repeat("e", 64)).Error; err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-done:
		dramaCheckCode(t, err, 91001)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	jobs, err := svc.ListJobs(ctx, 7, 1, false)
	if err != nil || len(jobs) != 0 || queue.calls.Load() != 0 {
		t.Fatalf("stale probe enqueued a job: %+v %v", jobs, err)
	}
}

func TestDramaComposePG16_ExpectedEpisodeIDRejectsReplacementDuringProbeWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	db := slice2cPG16(t)
	migrateCurrentSchemaPG16(t, db)
	seedSchema0RatioPG16(t, db)

	repo := repository.NewDramaRepository(db)
	q := &slice2cQueue{}
	repo.SetEnqueuer(q)
	files, _ := verifiedManifestAssets(newComposeMemory())
	entered, release := make(chan struct{}), make(chan struct{})
	svc := service.NewDramaService(service.DramaDeps{
		Repo:    repo,
		Assets:  &pgUniqueComposeAssets{manifestFiles: files},
		Probes:  repository.NewAssetMediaProbeRepository(db),
		Compose: &manifestProcessor{},
		ProbeVideo: func(ctx context.Context, _ string) (videoframe.NormalizedProbe, error) {
			err := waitSlice5PGRelease(ctx, entered, release)
			return videoframe.NormalizedProbe{FpsNum: 24, FpsDen: 1, NormalizedFrames24: 120}, err
		},
	})

	expectedID := uint64(3) // seed 中 no=1 初始对应 ID=3，no=2 对应 ID=4
	done := make(chan error, 1)
	go func() {
		_, err := svc.CreateJob(ctx, 7, 1, &model.DramaJobReq{
			Step:              "compose",
			Episodes:          []int{1},
			ExpectedEpisodeID: &expectedID,
			ComposeRequestKey: "ep-replace-window",
			Compose:           &model.DramaComposeReq{Size: "720p", Variants: []string{"clean"}},
		})
		done <- err
	}()
	waitSlice5PGGate(ctx, t, entered, done)

	// 在 tx1 校验通过后的锁外探测窗口中，按真实 DeleteEpisode 语义删除原第 1 集（ID=3）并让原第 2 集（ID=4）补位为 no=1
	if err := db.WithContext(ctx).Where("user_id = ? AND episode_id = ?", 7, 3).Delete(&model.DramaSegment{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Unscoped().Delete(&model.DramaEpisode{}, 3).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Model(&model.DramaEpisode{}).Where("id = ?", 4).Update("no", 1).Error; err != nil {
		t.Fatal(err)
	}
	close(release)

	select {
	case err := <-done:
		dramaCheckCode(t, err, 91001)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	jobs, err := svc.ListJobs(ctx, 7, 1, false)
	if err != nil || len(jobs) != 0 || q.calls.Load() != 0 {
		t.Fatalf("补位集被误提交了合成任务: jobs=%+v err=%v", jobs, err)
	}
}

func TestDramaRepositoryPG16_WithSnapshotTxRepeatableReadReadOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	db := slice2cPG16(t)
	migrateCurrentSchemaPG16(t, db)
	seedSchema0RatioPG16(t, db)

	repo := repository.NewDramaRepository(db)
	var beforeVer int64
	var beforeCount int
	var isolation, readOnly string
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- repo.WithSnapshotTx(ctx, func(tx repository.DramaTx) error {
			// 在第一次查询之后并发插入并发提交新分段并升级版本号，验证同一 snapshot 内前后一致
			eps, err := tx.Episodes(ctx, 7, 1)
			if err != nil {
				return err
			}
			beforeVer = eps[0].SegmentVersion
			rows, err := tx.Segments(ctx, 7, 3)
			if err != nil {
				return err
			}
			beforeCount = len(rows)
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			epsAfter, err := tx.Episodes(ctx, 7, 1)
			if err != nil {
				return err
			}
			rowsAfter, err := tx.Segments(ctx, 7, 3)
			if err != nil {
				return err
			}
			if epsAfter[0].SegmentVersion != beforeVer || len(rowsAfter) != beforeCount {
				return fmt.Errorf("snapshot 内部看到跨事务提交: before=(v=%d n=%d) after=(v=%d n=%d)", beforeVer, beforeCount, epsAfter[0].SegmentVersion, len(rowsAfter))
			}
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("WithSnapshotTx 提前失败: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := db.WithContext(ctx).Create(&model.DramaSegment{
		BaseModel: model.BaseModel{ID: 999}, UserID: 7, EpisodeID: 3, Seq: 9, Seconds: 5, Revision: 1,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := repo.BumpSegmentVersion(ctx, 7, 3); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Raw("SHOW transaction_isolation").Scan(&isolation).Error; err != nil {
			return err
		}
		return tx.Raw("SHOW transaction_read_only").Scan(&readOnly).Error
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if isolation != "repeatable read" || readOnly != "on" {
		t.Fatalf("unexpected snapshot tx options: isolation=%q read_only=%q", isolation, readOnly)
	}
}

func TestDramaComposePG16_StaleIdentityPrecedesReplacementState(t *testing.T) {
	ctx := t.Context()
	db := slice2cPG16(t)
	migrateCurrentSchemaPG16(t, db)
	seedSchema0RatioPG16(t, db)
	repo := repository.NewDramaRepository(db)
	queue := &slice2cQueue{}
	repo.SetEnqueuer(queue)
	svc := service.NewDramaService(service.DramaDeps{Repo: repo})
	old := uint64(3)
	req := &model.DramaJobReq{Step: "compose", Episodes: []int{1}, ExpectedEpisodeID: &old}
	checkStale := func() {
		t.Helper()
		_, err := svc.CreateJob(ctx, 7, 1, req)
		dramaCheckCode(t, err, 91001)
	}
	// Move the original episode away; the old number is now missing.
	if err := db.Model(&model.DramaEpisode{}).Where("id = ?", 3).Update("no", 5).Error; err != nil {
		t.Fatal(err)
	}
	checkStale()
	// An unlocked replacement and its busy compose must both lose to the old identity.
	if err := db.Model(&model.DramaEpisode{}).Where("id = ?", 4).Updates(map[string]any{
		"no": 1, "locked_at": nil,
	}).Error; err != nil {
		t.Fatal(err)
	}
	checkStale()
	if err := db.Create(&model.DramaJob{
		UserID: 7, DramaID: 1, Step: "compose", Episodes: []int{1}, Status: "queued", Lane: "user",
		InputJSON: []byte(`{"episode_id":4}`), Output: []byte(`{}`),
	}).Error; err != nil {
		t.Fatal(err)
	}
	checkStale()
	if queue.calls.Load() != 0 {
		t.Fatal("stale request enqueued a job")
	}
}

func TestDramaSegmentPG16_ListEpisodeContinuesWhileProjectLocked(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	db := slice2cPG16(t)
	migrateCurrentSchemaPG16(t, db)
	seedSchema0RatioPG16(t, db)
	repo := repository.NewDramaRepository(db)
	svc := service.NewDramaSegmentService(service.DramaSegmentDeps{Repo: repo, Assets: pgTakeAssets{db: db}})
	err := repo.WithTx(ctx, func(tx repository.DramaTx) error {
		if _, err := tx.Project(ctx, 7, 1, true); err != nil {
			return err
		}
		readCtx, readCancel := context.WithTimeout(ctx, time.Second)
		defer readCancel()
		ep, views, err := svc.ListEpisode(readCtx, 7, 1, 1)
		if err != nil {
			return err
		}
		if ep.ID != 3 || len(views) != 2 || views[0].EpisodeID != idcodec.Encode(3) {
			return fmt.Errorf("episode or membership mismatch: ep=%+v views=%+v", ep, views)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read-only ListEpisode waited on a project write lock: %v", err)
	}
}
