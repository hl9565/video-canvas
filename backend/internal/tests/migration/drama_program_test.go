package migration_test

import (
	"context"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"video-canvas/internal/migration"
	"video-canvas/internal/model"
	"video-canvas/migrations"
)

const programMigration = "9003_drama_program.sql"

func TestMigration9003_EmptyAndConstraints(t *testing.T) {
	if _, err := fs.ReadFile(migrations.FS, programMigration); err != nil {
		t.Fatal(err)
	}
	db := programPG16(t)
	runReal(t, db)
	if !slices.Contains(applied(t, db), programMigration) {
		t.Fatalf("未从空库应用 %s", programMigration)
	}
	mustExecOK(t, db, `INSERT INTO drama_episodes (id, user_id, drama_id, no) VALUES (10, 1, 1, 1)`)
	mustExecOK(t, db, `INSERT INTO drama_segments (id, user_id, episode_id) VALUES (11, 1, 10)`)
	assert9003Defaults(t, db)
	t.Run("program", func(t *testing.T) { assert9003ProgramChecks(t, db) })
	t.Run("visual", func(t *testing.T) { assert9003VisualChecks(t, db) })
}

func TestMigration9003_StagedUpgrade(t *testing.T) {
	db := programPG16(t)
	applyProgramStage(t, db, "0035", "")
	mustExecOK(t, db, `INSERT INTO drama_episodes (id, user_id, drama_id, no, script_md, segment_version)
		VALUES (10, 1, 1, 1, '旧剧本', 7)`)
	mustExecOK(t, db, `INSERT INTO drama_segments (id, user_id, episode_id, prompt, chosen_task_id)
		VALUES (11, 1, 10, '旧分镜', 77)`)
	applyProgramStage(t, db, "0035", "9001")
	mustExecOK(t, db, `UPDATE drama_segments SET source_in_f=6, source_out_f=30, audio_gain_db=-4 WHERE id=11`)
	applyProgramStage(t, db, "0035", "9002")
	mustExecOK(t, db, `INSERT INTO drama_segment_take_links (segment_id, user_id, task_id, source_canvas_id, asset_id, origin, qc)
		VALUES (11, 1, 77, 1, 100, 'native', 'accepted')`)
	applyProgramStage(t, db, "0035", "9003")
	// 已有高段记录的库仍会补跑后来发布的上游低段迁移。
	runReal(t, db)
	assert9003Defaults(t, db)
	assert9003PreservedData(t, db)
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil || !slices.Equal(applied(t, db), names) {
		t.Fatalf("升级记录不完整：%v / %v", applied(t, db), err)
	}
}

func programPG16(t *testing.T) *gorm.DB {
	t.Helper()
	db := isolatedDB(t)
	var version int
	if err := db.Raw("SHOW server_version_num").Scan(&version).Error; err != nil || version < 160000 || version >= 170000 {
		t.Fatalf("9003 集成测试需要 PostgreSQL 16，实际 %d / %v", version, err)
	}
	return db
}

func applyProgramStage(t *testing.T, db *gorm.DB, lastUpstream, lastFork string) {
	t.Helper()
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{}
	for _, name := range names {
		prefix, _, _ := strings.Cut(name, "_")
		upstream := prefix >= "0001" && prefix <= lastUpstream
		fork := prefix >= "9001" && prefix <= lastFork
		if !upstream && !fork {
			continue
		}
		data, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = &fstest.MapFile{Data: data}
	}
	if err := migration.Run(context.Background(), db, migration.Options{FS: files, Logger: testLogger()}); err != nil {
		t.Fatal(err)
	}
}

func assert9003Defaults(t *testing.T, db *gorm.DB) {
	t.Helper()
	var ok bool
	if err := db.Raw(`SELECT program_json = '{"v":1,"audio":[],"overlays":[]}'::jsonb FROM drama_episodes WHERE id = 10`).Scan(&ok).Error; err != nil || !ok {
		t.Fatalf("节目单默认值错误：%t / %v", ok, err)
	}
	var segment model.DramaSegment
	if err := db.First(&segment, 11).Error; err != nil || segment.VisualKind != "video" || segment.StillAssetID != nil || string(segment.FXJSON) != "{}" {
		t.Fatalf("视觉模式默认值错误：%+v / %v", segment, err)
	}
}

func assert9003PreservedData(t *testing.T, db *gorm.DB) {
	t.Helper()
	var episode model.DramaEpisode
	if err := db.First(&episode, 10).Error; err != nil || episode.ScriptMD != "旧剧本" || episode.SegmentVersion != 7 {
		t.Fatalf("旧剧集数据未保留：%+v / %v", episode, err)
	}
	var segment model.DramaSegment
	if err := db.First(&segment, 11).Error; err != nil || segment.Prompt != "旧分镜" || segment.ChosenTaskID == nil || *segment.ChosenTaskID != 77 || segment.SourceInF != 6 || segment.SourceOutF == nil || *segment.SourceOutF != 30 || segment.AudioGainDB != -4 {
		t.Fatalf("旧分镜与 Stage A 剪辑未保留：%+v / %v", segment, err)
	}
	var take model.DramaSegmentTakeLink
	if err := db.Where("segment_id = ? AND task_id = ?", 11, 77).First(&take).Error; err != nil || take.AssetID != 100 || take.QC != model.TakeQCAccepted {
		t.Fatalf("Stage B 候选未保留：%+v / %v", take, err)
	}
}

func assert9003ProgramChecks(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, invalid := range []string{
		`[]`, `null`, `{"v":1,"audio":[]}`, `{"v":1,"overlays":[]}`, `{"audio":[],"overlays":[]}`,
		`{"v":2,"audio":[],"overlays":[]}`, `{"v":"1","audio":[],"overlays":[]}`,
		`{"v":1,"audio":{},"overlays":[]}`, `{"v":1,"audio":[],"overlays":null}`,
	} {
		t.Run(invalid, func(t *testing.T) {
			assert9003Check(t, "ck_de_program",
				db.Exec(`INSERT INTO drama_episodes (user_id, drama_id, no, program_json) VALUES (1, 1, 2, ?::jsonb)`, invalid).Error)
			assert9003Check(t, "ck_de_program",
				db.Exec(`UPDATE drama_episodes SET program_json = ?::jsonb WHERE id=10`, invalid).Error)
		})
	}
	// B0 的内容限制在程序层，数据库保留未来可扩展的数组槽位。
	mustExecOK(t, db, `UPDATE drama_episodes SET program_json = '{"v":1,"audio":[],"overlays":[{}]}' WHERE id=10`)
}

func assert9003VisualChecks(t *testing.T, db *gorm.DB) {
	t.Helper()
	const insert = `INSERT INTO drama_segments (user_id, episode_id, visual_kind, still_asset_id, chosen_task_id, source_in_f, source_out_f, fx_json)
		VALUES (1, 10, ?, ?, ?, ?, ?, ?::jsonb)`
	const update = `UPDATE drama_segments SET visual_kind=?, still_asset_id=?, chosen_task_id=?, source_in_f=?, source_out_f=?, fx_json=?::jsonb WHERE id=11`
	for _, valid := range [][]any{
		{"video", nil, 77, 6, nil, `{}`}, {"still", 100, nil, 0, 24, `{"zoom_start":1}`}, {"black", nil, nil, 0, 24, `{}`},
	} {
		for _, stmt := range []string{insert, update} {
			if err := db.Exec(stmt, valid...).Error; err != nil {
				t.Fatalf("应接受 %v：%v", valid, err)
			}
		}
	}
	for _, tc := range []struct {
		name string
		args []any
	}{
		{"video_has_still", []any{"video", 100, nil, 0, nil, `{}`}},
		{"still_no_asset", []any{"still", nil, nil, 0, 24, `{}`}},
		{"still_has_task", []any{"still", 100, 77, 0, 24, `{}`}},
		{"still_nonzero_in", []any{"still", 100, nil, 6, 24, `{}`}},
		{"still_open_end", []any{"still", 100, nil, 0, nil, `{}`}},
		{"black_has_asset", []any{"black", 100, nil, 0, 24, `{}`}},
		{"black_has_task", []any{"black", nil, 77, 0, 24, `{}`}},
		{"black_nonzero_in", []any{"black", nil, nil, 6, 24, `{}`}},
		{"black_open_end", []any{"black", nil, nil, 0, nil, `{}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, stmt := range []string{insert, update} {
				assert9003Check(t, "ck_ds_visual", db.Exec(stmt, tc.args...).Error)
			}
		})
	}
	for _, stmt := range []string{insert, update} {
		assert9003Check(t, "ck_ds_fx_obj", db.Exec(stmt, "video", nil, nil, 0, nil, `[]`).Error)
	}
	// 未知模式同时违反两条 CHECK；先在隔离 schema 移除组合约束，单独验证模式枚举。
	mustExecOK(t, db, `ALTER TABLE drama_segments DROP CONSTRAINT ck_ds_visual`)
	for _, stmt := range []string{insert, update} {
		assert9003Check(t, "ck_ds_visual_kind", db.Exec(stmt, "image", nil, nil, 0, nil, `{}`).Error)
	}
}

func assert9003Check(t *testing.T, constraint string, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != constraint {
		t.Fatalf("期望 CHECK %s 失败，实际 %v", constraint, err)
	}
}
