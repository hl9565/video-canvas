package migration_test

import (
	"context"
	"errors"
	"fmt"
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

// frameRunMigrationFiles 显式固定上游 0037 与 Fork 9001/9002，不按 Fork 文件名字典序截断历史。
func frameRunMigrationFiles(t *testing.T, include9002 bool) fstest.MapFS {
	t.Helper()
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{}
	for _, name := range names {
		prefix := name[:4]
		upstream := prefix >= "0001" && prefix <= "0037"
		fork := prefix == "9001" || include9002 && prefix == "9002"
		if !upstream && !fork {
			continue
		}
		data, readErr := fs.ReadFile(migrations.FS, name)
		if readErr != nil {
			t.Fatal(readErr)
		}
		files[name] = &fstest.MapFile{Data: data}
	}
	return files
}

func TestMigration9002_UpgradeAndConstraints(t *testing.T) {
	db := isolatedDB(t)
	ctx := context.Background()
	if err := migration.Run(ctx, db, migration.Options{FS: frameRunMigrationFiles(t, false), Logger: testLogger()}); err != nil {
		t.Fatal(err)
	}
	mustExecOK(t, db, `INSERT INTO drama_segments (id, user_id, episode_id, prompt, chosen_task_id) VALUES (71, 1, 90, '旧片段', 77)`)
	mustExecOK(t, db, `INSERT INTO generation_tasks
		(id, user_id, kind, model_id, provider, status, input_json, config_snapshot, next_poll_at, deadline_at)
		VALUES (77, 1, 'video', 'old-model', 'old-provider', 'succeeded', '{}', '{}', now(), now())`)
	for range 2 {
		if err := migration.Run(ctx, db, migration.Options{FS: frameRunMigrationFiles(t, true), Logger: testLogger()}); err != nil {
			t.Fatal(err)
		}
	}
	var seg model.DramaSegment
	if err := db.First(&seg, 71).Error; err != nil || seg.ChosenCleared || seg.Prompt != "旧片段" || seg.ChosenTaskID == nil || *seg.ChosenTaskID != 77 {
		t.Fatalf("旧片段数据和默认值不一致：%+v / %v", seg, err)
	}
	var task model.GenerationTask
	if err := db.First(&task, 77).Error; err != nil || task.FrameRunID != nil || task.FrameRunConfirmRev != nil || task.Status != "succeeded" {
		t.Fatalf("旧任务数据和默认值不一致：%+v / %v", task, err)
	}
	names, err := fs.Glob(frameRunMigrationFiles(t, true), "*.sql")
	if err != nil || !slices.Equal(applied(t, db), names) {
		t.Fatalf("升级记录不完整或被重复执行：%v / %v", applied(t, db), err)
	}
	assert9002Catalog(t, db)
	assert9002RunChecks(t, db)
	assert9002RunUniqueness(t, db)
	assert9002TakeConstraints(t, db)
}

func assert9002Catalog(t *testing.T, db *gorm.DB) {
	t.Helper()
	checks := map[string][]string{
		"ck_dfr_state":  {"draft", "frame_pending", "frame_ready", "confirmed", "submitting", "video_pending", "bound", "failed", "cancelled"},
		"ck_dfr_source": {"existing", "generate", "canvas"}, "ck_dfr_segment_rev": {"segment_revision > 0"},
		"ck_dfr_confirmed_obj": {"jsonb_typeof(confirmed_input)", "object"},
		"ck_dst_origin":        {"i2v", "canvas_import", "native"}, "ck_dst_qc": {"unreviewed", "accepted", "rejected"},
	}
	for name, terms := range checks {
		var definition string
		if err := db.Raw(`SELECT pg_get_constraintdef(c.oid) FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace
			WHERE n.nspname = current_schema() AND c.conname = ? AND c.contype = 'c'`, name).Scan(&definition).Error; err != nil {
			t.Fatal(err)
		}
		for _, term := range terms {
			if !strings.Contains(definition, term) {
				t.Errorf("CHECK %s 缺少 %s：%s", name, term, definition)
			}
		}
	}
	indexes := []struct {
		name, table, columns string
		unique               bool
		predicate            []string
	}{
		{"idx_task_frame_run", "generation_tasks", "frame_run_id", false, []string{"frame_run_id IS NOT NULL"}},
		{"uk_dfr_user_req", "drama_segment_frame_runs", "user_id, request_key", true, nil},
		{"uk_dfr_segment_active", "drama_segment_frame_runs", "segment_id", true, []string{"draft", "frame_pending", "frame_ready", "confirmed", "submitting", "video_pending"}},
		{"uk_dfr_video", "drama_segment_frame_runs", "video_task_id", true, []string{"video_task_id IS NOT NULL"}},
		{"ix_dfr_segment", "drama_segment_frame_runs", "user_id, segment_id, id", false, nil},
		{"ix_dfr_recover", "drama_segment_frame_runs", "state, updated_at", false, []string{"frame_pending", "submitting", "video_pending"}},
		{"ix_dfr_frame_task", "drama_segment_frame_runs", "frame_task_id", false, []string{"frame_task_id IS NOT NULL"}},
		{"ix_dst_task", "drama_segment_take_links", "user_id, task_id", false, nil},
		{"uk_dst_run", "drama_segment_take_links", "run_id", true, []string{"run_id IS NOT NULL"}},
	}
	for _, want := range indexes {
		var got struct {
			Definition, Predicate string
			IsUnique              bool
		}
		err := db.Raw(`SELECT pg_get_indexdef(i.indexrelid) AS definition, COALESCE(pg_get_expr(i.indpred, i.indrelid), '') AS predicate, i.indisunique AS is_unique
			FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = current_schema() AND c.relname = ? AND i.indrelid = to_regclass(?)`, want.name, want.table).Scan(&got).Error
		if err != nil || got.IsUnique != want.unique || !strings.Contains(got.Definition, "("+want.columns+")") {
			t.Fatalf("索引 %s 不一致：%+v / %v", want.name, got, err)
		}
		if len(want.predicate) == 0 && got.Predicate != "" {
			t.Errorf("索引 %s 不应带条件：%s", want.name, got.Predicate)
		}
		for _, term := range want.predicate {
			if !strings.Contains(got.Predicate, term) {
				t.Errorf("索引 %s 条件缺少 %s：%s", want.name, term, got.Predicate)
			}
		}
		if want.name == "uk_dfr_segment_active" || want.name == "ix_dfr_recover" {
			for _, state := range []string{"draft", "frame_pending", "frame_ready", "confirmed", "submitting", "video_pending", "bound", "failed", "cancelled"} {
				if strings.Contains(got.Predicate, "'"+state+"'") != slices.Contains(want.predicate, state) {
					t.Errorf("索引 %s 状态集合错误：%s", want.name, got.Predicate)
				}
			}
		}
	}
	for _, table := range []string{"drama_segment_frame_runs", "drama_segment_take_links"} {
		if hasColumn(t, db, table, "deleted_at") {
			t.Errorf("%s 不应软删", table)
		}
	}
}

const insert9002RunSQL = `INSERT INTO drama_segment_frame_runs
	(user_id, segment_id, request_key, request_hash, frame_source, state, segment_revision, confirmed_input)
	VALUES (?, ?, ?, repeat('a', 64), ?, ?, ?, ?::jsonb)`

func insert9002Run(t *testing.T, db *gorm.DB, segment uint64, key, state string) uint64 {
	t.Helper()
	var id uint64
	if err := db.Raw(insert9002RunSQL+` RETURNING id`, 1, segment, key, "existing", state, 1, `{}`).Scan(&id).Error; err != nil {
		t.Fatal(err)
	}
	return id
}

func assert9002Violation(t *testing.T, err error, state, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != state || pgErr.ConstraintName != constraint {
		t.Fatalf("期望 %s/%s，实际 %v", state, constraint, err)
	}
}

func assert9002RunChecks(t *testing.T, db *gorm.DB) {
	t.Helper()
	for i, source := range []string{"existing", "generate", "canvas"} {
		if err := db.Exec(insert9002RunSQL, 1, 200+i, fmt.Sprintf("source-%d", i), source, "draft", 1, `{"ok":true}`).Error; err != nil {
			t.Fatal(err)
		}
	}
	for i, state := range []string{"draft", "frame_pending", "frame_ready", "confirmed", "submitting", "video_pending", "bound", "failed", "cancelled"} {
		insert9002Run(t, db, uint64(300+i), fmt.Sprintf("state-%d", i), state)
	}
	for _, tc := range []struct {
		name, source, state, input, constraint string
		revision                               int64
	}{
		{"状态", "existing", "unknown", `{}`, "ck_dfr_state", 1},
		{"来源", "unknown", "draft", `{}`, "ck_dfr_source", 1},
		{"零版本", "existing", "draft", `{}`, "ck_dfr_segment_rev", 0},
		{"负版本", "existing", "draft", `{}`, "ck_dfr_segment_rev", -1},
		{"数组", "existing", "draft", `[]`, "ck_dfr_confirmed_obj", 1},
		{"JSON空值", "existing", "draft", `null`, "ck_dfr_confirmed_obj", 1},
		{"标量", "existing", "draft", `3`, "ck_dfr_confirmed_obj", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := db.Exec(insert9002RunSQL, 1, 999, "invalid-"+tc.name, tc.source, tc.state, tc.revision, tc.input).Error
			assert9002Violation(t, err, "23514", tc.constraint)
		})
	}
	var id uint64
	if err := db.Raw(`INSERT INTO drama_segment_frame_runs (user_id, segment_id, request_key, request_hash, frame_source, state, segment_revision)
		VALUES (1, 400, 'defaults', repeat('a', 64), 'existing', 'draft', 1) RETURNING id`).Scan(&id).Error; err != nil {
		t.Fatal(err)
	}
	var row model.DramaSegmentFrameRun
	if err := db.First(&row, id).Error; err != nil {
		t.Fatal(err)
	}
	if row.Revision != 1 || row.ConfirmedVersion != 0 || row.AdoptIfEmpty || row.ModelKey != "" || row.VideoIdempotencyKey != "" || row.ErrorCode != "" || row.ErrorMessage != "" || row.CreatedAt.IsZero() || row.UpdatedAt.IsZero() || string(row.ConfirmedInput) != `{}` {
		t.Fatalf("流程默认值错误：%+v", row)
	}
}

func assert9002RunUniqueness(t *testing.T, db *gorm.DB) {
	t.Helper()
	insert9002Run(t, db, 500, "unique-request", "bound")
	err := db.Exec(insert9002RunSQL, 1, 501, "unique-request", "existing", "draft", 1, `{}`).Error
	assert9002Violation(t, err, "23505", "uk_dfr_user_req")
	if err := db.Exec(insert9002RunSQL, 2, 501, "unique-request", "existing", "draft", 1, `{}`).Error; err != nil {
		t.Fatalf("不同用户允许同请求键：%v", err)
	}
	for i, terminal := range []string{"cancelled", "bound", "failed"} {
		segment := uint64(600 + i)
		id := insert9002Run(t, db, segment, terminal+"-first", "draft")
		for _, active := range []string{"draft", "frame_pending", "frame_ready", "confirmed", "submitting", "video_pending"} {
			err := db.Exec(insert9002RunSQL, 1, segment, terminal+"-"+active, "existing", active, 1, `{}`).Error
			assert9002Violation(t, err, "23505", "uk_dfr_segment_active")
		}
		if err := db.Exec(`UPDATE drama_segment_frame_runs SET state = ? WHERE id = ?`, terminal, id).Error; err != nil {
			t.Fatal(err)
		}
		insert9002Run(t, db, segment, terminal+"-next", "draft")
	}
	first := insert9002Run(t, db, 700, "video-first", "bound")
	second := insert9002Run(t, db, 701, "video-second", "cancelled")
	if err := db.Exec(`UPDATE drama_segment_frame_runs SET video_task_id = 77 WHERE id = ?`, first).Error; err != nil {
		t.Fatal(err)
	}
	assert9002Violation(t, db.Exec(`UPDATE drama_segment_frame_runs SET video_task_id = 77 WHERE id = ?`, second).Error, "23505", "uk_dfr_video")
}

func assert9002TakeConstraints(t *testing.T, db *gorm.DB) {
	t.Helper()
	const insertSQL = `INSERT INTO drama_segment_take_links (user_id, segment_id, task_id, source_canvas_id, asset_id, origin, qc, run_id) VALUES (1, ?, ?, 9, 201, ?, ?, ?)`
	for i, origin := range []string{"i2v", "canvas_import", "native"} {
		for j, qc := range []string{"unreviewed", "accepted", "rejected"} {
			if err := db.Exec(insertSQL, 800+i, 1000+j, origin, qc, nil).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	assert9002Violation(t, db.Exec(insertSQL, 900, 101, "unknown", "unreviewed", nil).Error, "23514", "ck_dst_origin")
	assert9002Violation(t, db.Exec(insertSQL, 900, 101, "native", "pending", nil).Error, "23514", "ck_dst_qc")
	if err := db.Exec(insertSQL, 900, 101, "i2v", "rejected", 55).Error; err != nil {
		t.Fatal(err)
	}
	assert9002Violation(t, db.Exec(insertSQL, 900, 101, "native", "accepted", nil).Error, "23505", "drama_segment_take_links_pkey")
	assert9002Violation(t, db.Exec(insertSQL, 901, 102, "i2v", "unreviewed", 55).Error, "23505", "uk_dst_run")
	mustExecOK(t, db, `INSERT INTO drama_segment_take_links (user_id, segment_id, task_id, source_canvas_id, asset_id, origin) VALUES (1, 902, 103, 9, 201, 'native')`)
	var row model.DramaSegmentTakeLink
	if err := db.Where("segment_id = ? AND task_id = ?", 902, 103).First(&row).Error; err != nil || row.QC != model.TakeQCUnreviewed || row.CreatedAt.IsZero() {
		t.Fatalf("候选默认值错误：%+v / %v", row, err)
	}
}

func TestMigration9002_EmbeddedAndDualBand(t *testing.T) {
	data, err := fs.ReadFile(migrations.FS, "9002_drama_frame_runs.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"chosen_cleared", "frame_run_confirm_rev", "drama_segment_frame_runs", "drama_segment_take_links"} {
		if !strings.Contains(string(data), name) {
			t.Errorf("9002 缺少 %s", name)
		}
	}
	names, err := fs.Glob(frameRunMigrationFiles(t, true), "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateMigrationFileNames(names); err != nil {
		t.Fatal(err)
	}
	var tail []string
	for _, name := range names {
		if name[:4] >= "0035" {
			tail = append(tail, name[:4])
		}
	}
	if !slices.Equal(tail, []string{"0035", "0036", "0037", "9001", "9002"}) {
		t.Fatalf("迁移链不连续：%v", tail)
	}
}
