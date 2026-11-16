package migration_test

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"video-canvas/internal/migration"
	"video-canvas/migrations"
)

// mapFSUpTo 从真实内嵌迁移集截取到指定文件名（含）的子集，用于构造旧版本升级测试库。
func mapFSUpTo(t *testing.T, maxName string) fstest.MapFS {
	t.Helper()
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("读取内嵌迁移失败：%v", err)
	}
	sub := fstest.MapFS{}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() > maxName {
			continue
		}
		data, readErr := fs.ReadFile(migrations.FS, entry.Name())
		if readErr != nil {
			t.Fatalf("读取迁移 %s 失败：%v", entry.Name(), readErr)
		}
		sub[entry.Name()] = &fstest.MapFile{Data: data}
	}
	return sub
}

func seedPre0036Rows(t *testing.T, db *gorm.DB) {
	t.Helper()
	stmts := []string{
		`INSERT INTO drama_jobs (user_id, drama_id, step, episodes, status, approval_id, output)
		 VALUES (1, 10, 'compose', '[]'::jsonb, 'succeeded', 88, '{}'::jsonb)`,
		`INSERT INTO drama_jobs (user_id, drama_id, step, episodes, status, approval_id, output)
		 VALUES (1, 10, 'storyboard', '[]'::jsonb, 'succeeded', 89, '{}'::jsonb)`,
		`INSERT INTO marketing_projects (id, user_id, title, product_asset_ids, revision)
		 VALUES (501, 1, '旧营销项目', '[]'::jsonb, 1)`,
		`INSERT INTO marketing_rounds (id, user_id, project_id, no, status, settings)
		 VALUES (601, 1, 501, 1, 'scripting', '{}'::jsonb)`,
		`INSERT INTO marketing_step_runs (id, user_id, round_id, step_key, status, started_at)
		 VALUES (701, 1, 601, 'marketing.facts', 'running', now())`,
	}
	for _, stmt := range stmts {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("预置 0035 数据失败（%s）：%v", stmt, err)
		}
	}
}

func mustExecOK(t *testing.T, db *gorm.DB, stmt string) {
	t.Helper()
	if err := db.Exec(stmt).Error; err != nil {
		t.Fatalf("期望 SQL 成功却失败（%s）：%v", stmt, err)
	}
}

func mustExecFail(t *testing.T, db *gorm.DB, stmt string) {
	t.Helper()
	if err := db.Exec(stmt).Error; err == nil {
		t.Fatalf("期望 SQL 被约束拒绝却成功（%s）", stmt)
	}
}

// TestMigration0036_FileEmbedded 验证 0036 迁移已内嵌且包含全量新列、回填和探测表定义。
func TestMigration0036_FileEmbedded(t *testing.T) {
	data, err := fs.ReadFile(migrations.FS, "0036_drama_edit.sql")
	if err != nil {
		t.Fatalf("读取 0036_drama_edit.sql 失败：%v", err)
	}
	sql := string(data)
	for _, want := range []string{
		"source_in_f", "source_out_f", "ck_ds_range", "ck_ds_gain", "ck_ds_pts",
		"edit_schema", "final_compose_job_id", "ck_de_edit_schema",
		"compose_request_key", "uk_drama_compose_request",
		"UPDATE drama_jobs", "'approval:' || approval_id::text",
		"request_intent_sig", "request_batch_index",
		"execution_gen", "round_gen", "cancel_requested", "exec_token", "lease_until",
		"CREATE TABLE asset_media_probes",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("0036_drama_edit.sql 缺少关键定义：%s", want)
		}
	}
}

// TestMigration0036_UpgradeAndConstraints 验证 0035→0036 升级回填、旧 running 行识别及全量 CHECK/唯一索引负测。
func TestMigration0036_UpgradeAndConstraints(t *testing.T) {
	db := isolatedDB(t)
	ctx := context.Background()

	if err := migration.Run(ctx, db, migration.Options{
		FS:     mapFSUpTo(t, "0035_marketing.sql"),
		Logger: zap.NewNop(),
	}); err != nil {
		t.Fatalf("迁移至 0035 失败：%v", err)
	}
	seedPre0036Rows(t, db)

	if err := migration.Run(ctx, db, migration.Options{Logger: zap.NewNop()}); err != nil {
		t.Fatalf("升级至 0036 失败：%v", err)
	}

	assertUpgradeBackfill(t, db)
	assert0036SegmentConstraints(t, db)
	assert0036EpisodeAndJobConstraints(t, db)
	assert0036AssetMediaProbesPK(t, db)
}

func assertUpgradeBackfill(t *testing.T, db *gorm.DB) {
	t.Helper()
	var composeKey *string
	var composeIntentSig string
	if err := db.Raw(
		`SELECT compose_request_key, request_intent_sig FROM drama_jobs WHERE approval_id = 88`,
	).Row().Scan(&composeKey, &composeIntentSig); err != nil {
		t.Fatalf("读取回填 compose 行失败：%v", err)
	}
	if composeKey == nil || *composeKey != "approval:88" || composeIntentSig != "" {
		t.Fatalf("旧审批 compose 回填错误：key=%v sig=%q", composeKey, composeIntentSig)
	}

	var storyKey *string
	if err := db.Raw(
		`SELECT compose_request_key FROM drama_jobs WHERE approval_id = 89`,
	).Row().Scan(&storyKey); err != nil {
		t.Fatalf("读取 storyboard 行失败：%v", err)
	}
	if storyKey != nil {
		t.Fatalf("非 compose 步骤不应回填 compose_request_key，实际 %q", *storyKey)
	}

	var gen int64
	if err := db.Raw(`SELECT execution_gen FROM marketing_rounds WHERE id = 601`).Row().Scan(&gen); err != nil {
		t.Fatalf("读取 marketing_rounds 失败：%v", err)
	}
	if gen != 1 {
		t.Fatalf("旧轮次 execution_gen 应默认 1，实际 %d", gen)
	}

	var execToken string
	var leaseIsNull bool
	var roundGen int64
	var cancelRequested bool
	if err := db.Raw(
		`SELECT exec_token, lease_until IS NULL, round_gen, cancel_requested FROM marketing_step_runs WHERE id = 701`,
	).Row().Scan(&execToken, &leaseIsNull, &roundGen, &cancelRequested); err != nil {
		t.Fatalf("读取 0035 running 行失败：%v", err)
	}
	if execToken != "" || !leaseIsNull || roundGen != 1 || cancelRequested {
		t.Fatalf("0035 running 行标记异常：token=%q lease_null=%v round_gen=%d cancel=%v",
			execToken, leaseIsNull, roundGen, cancelRequested)
	}
}

func assert0036SegmentConstraints(t *testing.T, db *gorm.DB) {
	t.Helper()
	mustExecFail(t, db, `INSERT INTO drama_segments (user_id, episode_id, source_in_f) VALUES (1, 90, -1)`)
	mustExecFail(t, db, `INSERT INTO drama_segments (user_id, episode_id, source_in_f, source_out_f) VALUES (1, 90, 10, 10)`)
	mustExecFail(t, db, `INSERT INTO drama_segments (user_id, episode_id, source_in_f, source_out_f) VALUES (1, 90, 10, 5)`)
	mustExecOK(t, db, `INSERT INTO drama_segments (user_id, episode_id, source_in_f, source_out_f) VALUES (1, 90, 0, NULL)`)
	mustExecOK(t, db, `INSERT INTO drama_segments (user_id, episode_id, source_in_f, source_out_f) VALUES (1, 90, 24, 43)`)

	mustExecFail(t, db, `INSERT INTO drama_segments (user_id, episode_id, audio_gain_db) VALUES (1, 90, -60.01)`)
	mustExecFail(t, db, `INSERT INTO drama_segments (user_id, episode_id, audio_gain_db) VALUES (1, 90, 12.01)`)
	mustExecOK(t, db, `INSERT INTO drama_segments (user_id, episode_id, audio_gain_db) VALUES (1, 90, -60.00)`)
	mustExecOK(t, db, `INSERT INTO drama_segments (user_id, episode_id, audio_gain_db) VALUES (1, 90, 12.00)`)

	mustExecFail(t, db, `INSERT INTO drama_segments (user_id, episode_id, av_pts_offset_ms) VALUES (1, 90, -10001)`)
	mustExecFail(t, db, `INSERT INTO drama_segments (user_id, episode_id, av_pts_offset_ms) VALUES (1, 90, 10001)`)
	mustExecOK(t, db, `INSERT INTO drama_segments (user_id, episode_id, av_pts_offset_ms) VALUES (1, 90, -10000)`)
	mustExecOK(t, db, `INSERT INTO drama_segments (user_id, episode_id, av_pts_offset_ms) VALUES (1, 90, 10000)`)
}

func assert0036EpisodeAndJobConstraints(t *testing.T, db *gorm.DB) {
	t.Helper()
	mustExecFail(t, db, `INSERT INTO drama_episodes (user_id, drama_id, no, edit_schema) VALUES (1, 10, 1, -1)`)
	mustExecFail(t, db, `INSERT INTO drama_episodes (user_id, drama_id, no, edit_schema) VALUES (1, 10, 2, 2)`)
	mustExecOK(t, db, `INSERT INTO drama_episodes (user_id, drama_id, no, edit_schema) VALUES (1, 10, 1, 0)`)
	mustExecOK(t, db, `INSERT INTO drama_episodes (user_id, drama_id, no, edit_schema) VALUES (1, 10, 2, 1)`)

	mustExecOK(t, db, `INSERT INTO drama_jobs (user_id, drama_id, step, episodes, status, output, compose_request_key)
		VALUES (1, 11, 'compose', '[]'::jsonb, 'succeeded', '{}'::jsonb, 'cmp-1')`)
	mustExecFail(t, db, `INSERT INTO drama_jobs (user_id, drama_id, step, episodes, status, output, compose_request_key)
		VALUES (1, 11, 'compose', '[]'::jsonb, 'succeeded', '{}'::jsonb, 'cmp-1')`)
	mustExecOK(t, db, `INSERT INTO drama_jobs (user_id, drama_id, step, episodes, status, output, compose_request_key)
		VALUES (1, 11, 'compose', '[]'::jsonb, 'succeeded', '{}'::jsonb, NULL)`)
	mustExecOK(t, db, `INSERT INTO drama_jobs (user_id, drama_id, step, episodes, status, output, compose_request_key)
		VALUES (1, 11, 'storyboard', '[]'::jsonb, 'succeeded', '{}'::jsonb, 'cmp-1')`)
}

func assert0036AssetMediaProbesPK(t *testing.T, db *gorm.DB) {
	t.Helper()
	sha := strings.Repeat("a", 64)
	mustExecOK(t, db, `INSERT INTO asset_media_probes (asset_id, sha256, probe_version, fps_num, fps_den, normalized_frames_24)
		VALUES (100, '`+sha+`', 1, 24, 1, 120)`)
	mustExecFail(t, db, `INSERT INTO asset_media_probes (asset_id, sha256, probe_version, fps_num, fps_den, normalized_frames_24)
		VALUES (100, '`+sha+`', 1, 24, 1, 120)`)
	mustExecOK(t, db, `INSERT INTO asset_media_probes (asset_id, sha256, probe_version, fps_num, fps_den, normalized_frames_24)
		VALUES (100, '`+sha+`', 2, 24, 1, 120)`)
}
