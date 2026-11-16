package migration_test

import (
	"context"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"video-canvas/internal/migration"
	"video-canvas/internal/migration/legacymodel"
	"video-canvas/internal/model"
	"video-canvas/migrations"
)

// autoMigrateOnly 是 GORM 标签表达不了、只在 SQL 迁移里存在的结构，结构一致性比较时跳过。
// 每一项都要写明原因。
var autoMigrateOnly = map[string]bool{
	// worker 扫描非终态任务用的部分索引，GORM 的 where 标签写不了 IN 列表
	"index idx_task_poll": true,
	// 名额与排队相关的部分索引（条件带 IN 列表或 IS NULL），见 0007_task_queue.sql
	"index idx_task_slot_user":     true,
	"index idx_task_slot_provider": true,
	"index idx_task_waiting":       true,
}

// TestMigrationFileNames 检查命名、基线和全局唯一前缀：上游从 0001、Fork 从 9001 分别连续编号。
func TestMigrationFileNames(t *testing.T) {
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateMigrationFileNames(names); err != nil {
		t.Fatal(err)
	}
}

// validateMigrationFileNames 同时约束两个独立连续号段，避免 Fork 高编号挤占未来上游迁移。
func validateMigrationFileNames(names []string) error {
	names = append([]string(nil), names...)
	sort.Strings(names)
	if len(names) == 0 {
		return fmt.Errorf("第一个迁移必须是 %s", migration.BaselineFile)
	}
	pattern := regexp.MustCompile(`^([0-9]{4})_[a-z0-9_]+(\.notx)?\.sql$`)
	seen := map[string]string{}
	nextUpstream, nextFork := 1, 9001
	for i, name := range names {
		m := pattern.FindStringSubmatch(name)
		if m == nil {
			return fmt.Errorf("文件名 %s 不合规：应为 NNNN_小写下划线描述.sql 或 .notx.sql", name)
		}
		n, _ := strconv.Atoi(m[1])
		if n == 0 || n == 9000 {
			return fmt.Errorf("迁移编号 %04d 禁用：%s", n, name)
		}
		if prev, ok := seen[m[1]]; ok {
			return fmt.Errorf("迁移文件 %s 与 %s 编号重复", prev, name)
		}
		seen[m[1]] = name
		if i == 0 && name != migration.BaselineFile {
			return fmt.Errorf("第一个迁移必须是 %s，得到 %s", migration.BaselineFile, name)
		}
		if n <= 8999 {
			if n != nextUpstream {
				return fmt.Errorf("上游迁移 0001–8999 应连续：期望 %04d，得到 %s", nextUpstream, name)
			}
			nextUpstream++
		} else {
			if n != nextFork {
				return fmt.Errorf("Fork 迁移 9001–9999 应连续：期望 %04d，得到 %s", nextFork, name)
			}
			nextFork++
		}
	}
	return nil
}

func TestMigrationFileNames_DualRanges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		valid bool
	}{
		{"只有基线", []string{"0001_baseline.sql"}, true},
		{"双段各自连续且允许 notx", []string{"9002_index.notx.sql", "0002_upstream.sql", "9001_fork.sql", "0001_baseline.sql"}, true},
		{"空集合", nil, false},
		{"基线名称错误", []string{"0001_other.sql"}, false},
		{"0000 禁用", []string{"0000_x.sql", "0001_baseline.sql"}, false},
		{"9000 禁用", []string{"0001_baseline.sql", "9000_x.sql"}, false},
		{"低段断档", []string{"0001_baseline.sql", "0003_x.sql", "9001_fork.sql"}, false},
		{"高段起点断档", []string{"0001_baseline.sql", "9002_x.sql"}, false},
		{"高段内部断档", []string{"0001_baseline.sql", "9001_x.sql", "9003_x.sql"}, false},
		{"低段重复前缀", []string{"0001_baseline.sql", "0002_x.sql", "0002_y.notx.sql"}, false},
		{"高段重复前缀", []string{"0001_baseline.sql", "9001_x.sql", "9001_y.notx.sql"}, false},
		{"超过四位", []string{"0001_baseline.sql", "10000_x.sql"}, false},
		{"名称必须小写", []string{"0001_baseline.sql", "0002_X.sql"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateMigrationFileNames(tc.files); (err == nil) != tc.valid {
				t.Fatalf("files=%v valid=%v err=%v", tc.files, tc.valid, err)
			}
		})
	}
}

func runReal(t *testing.T, db *gorm.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := migration.Run(ctx, db, migration.Options{AppVersion: "test", Logger: testLogger()}); err != nil {
		t.Fatalf("执行迁移失败：%v", err)
	}
}

// TestMigrationsMatchModels 保证模型与 SQL 迁移不会各走各的：
// 一个 schema 只跑 SQL 迁移，另一个只跑 AutoMigrate(model.All())，两边的列、索引、约束必须一致。
// 失败说明改了 internal/model 却没新增迁移文件（或反过来）。
func TestMigrationsMatchModels(t *testing.T) {
	sqlSchema := newSchema(t)
	sqlDB := connect(t, sqlSchema)
	runReal(t, sqlDB)

	gormSchema := newSchema(t)
	gormDB := connect(t, gormSchema)
	if err := gormDB.AutoMigrate(model.All()...); err != nil {
		t.Fatal(err)
	}

	left := schemaSnapshot(t, sqlDB, sqlSchema)
	delete(left, "index schema_migrations_pkey")
	delete(left, "constraint schema_migrations.schema_migrations_pkey")
	for k := range left {
		if strings.HasPrefix(k, "column schema_migrations.") {
			delete(left, k)
		}
	}
	if diff := diffSnapshots(left, schemaSnapshot(t, gormDB, gormSchema), autoMigrateOnly); len(diff) > 0 {
		t.Fatalf("SQL 迁移（左）与 internal/model（右）的结构不一致，改模型时要同时新增迁移文件：\n%s", strings.Join(diff, "\n"))
	}
}

// TestLegacyDatabaseUpgrade 模拟接入迁移器之前的老库（比 v0.3.2 更早，还留着已经删掉的旧结构），
// 升级后结构必须和全新库完全一致，数据保留，基线只记录不执行。
func TestLegacyDatabaseUpgrade(t *testing.T) {
	oldSchema := newSchema(t)
	old := connect(t, oldSchema)
	if err := old.AutoMigrate(legacymodel.All()...); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		// 更早版本留下的结构：旧迁移链会删掉它们
		`ALTER TABLE ai_channels ADD COLUMN trusted_internal boolean NOT NULL DEFAULT false`,
		`CREATE TABLE ai_providers (key varchar(64) PRIMARY KEY)`,
		`INSERT INTO users (username, password, role, status, created_at, updated_at) VALUES ('old-user', 'x', 'user', 'active', now(), now())`,
	} {
		if err := old.Exec(stmt).Error; err != nil {
			t.Fatalf("准备老库失败（%s）：%v", stmt, err)
		}
	}
	runReal(t, old)

	freshSchema := newSchema(t)
	fresh := connect(t, freshSchema)
	runReal(t, fresh)

	if diff := diffSnapshots(schemaSnapshot(t, old, oldSchema), schemaSnapshot(t, fresh, freshSchema), nil); len(diff) > 0 {
		t.Fatalf("老库升级后（左）与全新库（右）的结构不一致：\n%s", strings.Join(diff, "\n"))
	}
	var n int64
	old.Raw("SELECT count(*) FROM users WHERE username = 'old-user'").Scan(&n)
	if n != 1 {
		t.Fatal("老库的数据应保留")
	}
	if got := applied(t, old); len(got) == 0 || got[0] != migration.BaselineFile {
		t.Fatalf("老库应记录基线：%v", got)
	}
	runReal(t, old) // 接入后再次启动，什么都不做
}
