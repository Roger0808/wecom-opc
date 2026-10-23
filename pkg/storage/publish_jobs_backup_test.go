// WORK-RESOURCES-001（TASK-013/014）测试：skill_publish_jobs 备份/恢复工具与
// 000017 清理迁移语义。SQLite 覆盖结构与往返；真实 PostgreSQL 环境
// （PLATFORM_STATE_TEST_PG_DSN 指向一次性空库）覆盖 up/down/恢复与共享表保护。
package storage

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"weknora-tokendance/migrations"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// skillPublishJobsDDL 与 000011/000017 锁定的目标表结构一致（SQLite 往返用）。
const skillPublishJobsDDL = `
CREATE TABLE IF NOT EXISTS skill_publish_jobs (
    job_id           UUID PRIMARY KEY,
    release_id       TEXT NOT NULL,
    source_tenant    TEXT NOT NULL,
    targets          JSONB NOT NULL,
    idempotency_key  TEXT NOT NULL,
    status           TEXT NOT NULL,
    steps            JSONB,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT skill_publish_jobs_idempotency_key_key UNIQUE (idempotency_key)
)`

// seedSkillPublishJobRows 写入代表性存量行（VAL-217：有任务行环境）。
func seedSkillPublishJobRows(t *testing.T, stateDB *gorm.DB, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := uuid.NewString()
		ids = append(ids, id)
		err := stateDB.Exec(
			`INSERT INTO skill_publish_jobs (job_id, release_id, source_tenant, targets, idempotency_key, status, steps)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, fmt.Sprintf("release-%d", i), "1001",
			fmt.Sprintf(`[{"tenant_id":"t%d"}]`, i),
			fmt.Sprintf("publish-%03d", i), "succeeded",
			fmt.Sprintf(`[{"tenant_id":"t%d","status":"validated"}]`, i)).Error
		if err != nil {
			t.Fatalf("seed skill publish job row: %v", err)
		}
	}
	return ids
}

func TestSkillPublishJobsBackup_RoundTrip(t *testing.T) {
	s, _, stateDB := setupStateTestStorage(t)
	ctx := context.Background()
	if err := stateDB.Exec(skillPublishJobsDDL).Error; err != nil {
		t.Fatalf("create table: %v", err)
	}
	ids := seedSkillPublishJobRows(t, stateDB, 3)

	backup, err := s.ExportSkillPublishJobs(ctx)
	if err != nil {
		t.Fatalf("ExportSkillPublishJobs: %v", err)
	}
	if backup.RowCount != 3 || len(backup.Rows) != 3 {
		t.Fatalf("backup rows = %d/%d", backup.RowCount, len(backup.Rows))
	}
	if backup.SHA256 == "" {
		t.Fatalf("backup checksum missing")
	}

	// drop 表（等价迁移 000017 up）→ 空库导出为合法空备份。
	if err := stateDB.Exec("DROP TABLE skill_publish_jobs").Error; err != nil {
		t.Fatalf("drop table: %v", err)
	}
	empty, err := s.ExportSkillPublishJobs(ctx)
	if err != nil {
		t.Fatalf("export empty: %v", err)
	}
	if empty.RowCount != 0 || len(empty.Rows) != 0 {
		t.Fatalf("empty backup rows = %d", empty.RowCount)
	}

	// 重建（等价 down）→ 恢复行数/内容/幂等重放一致。
	if err := stateDB.Exec(skillPublishJobsDDL).Error; err != nil {
		t.Fatalf("recreate table: %v", err)
	}
	if err := s.ImportSkillPublishJobs(ctx, backup); err != nil {
		t.Fatalf("ImportSkillPublishJobs: %v", err)
	}
	if err := s.ImportSkillPublishJobs(ctx, backup); err != nil {
		t.Fatalf("idempotent re-import: %v", err)
	}
	var count int64
	if err := stateDB.Table("skill_publish_jobs").Count(&count).Error; err != nil {
		t.Fatalf("count restored: %v", err)
	}
	if count != 3 {
		t.Fatalf("restored rows = %d, want 3", count)
	}
	var got struct {
		ReleaseID      string
		IdempotencyKey string
	}
	if err := stateDB.Raw(`SELECT release_id, idempotency_key FROM skill_publish_jobs WHERE job_id = ?`, ids[0]).
		Scan(&got).Error; err != nil {
		t.Fatalf("query restored row: %v", err)
	}
	if got.ReleaseID != "release-0" || got.IdempotencyKey != "publish-000" {
		t.Fatalf("restored row content = %+v", got)
	}
}

func TestSkillPublishJobsBackup_RejectsTampered(t *testing.T) {
	s, _, stateDB := setupStateTestStorage(t)
	ctx := context.Background()
	if err := stateDB.Exec(skillPublishJobsDDL).Error; err != nil {
		t.Fatalf("create table: %v", err)
	}
	seedSkillPublishJobRows(t, stateDB, 1)

	backup, err := s.ExportSkillPublishJobs(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	backup.Rows[0].Status = "tampered"
	if err := s.ImportSkillPublishJobs(ctx, backup); err == nil {
		t.Fatalf("expected tampered backup rejection")
	} else if !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("tamper error = %v, want checksum mismatch", err)
	}

	// 行数不符同样拒绝。
	backup2, err := s.ExportSkillPublishJobs(ctx)
	if err != nil {
		t.Fatalf("export 2: %v", err)
	}
	backup2.RowCount = 99
	if err := s.ImportSkillPublishJobs(ctx, backup2); err == nil {
		t.Fatalf("expected row count mismatch rejection")
	}

	// 表缺失（未先 down 重建）拒绝恢复。
	if err := stateDB.Exec("DROP TABLE skill_publish_jobs").Error; err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := s.ImportSkillPublishJobs(ctx, backup2); err == nil {
		t.Fatalf("expected missing-table rejection")
	}
}

// TestSkillPublishJobsMigration00017_SQLiteSequence 验证 000017 随全量迁移在
// SQLite 上干净应用（000011 建表 → 000017 drop，最终状态为清理后）。
func TestSkillPublishJobsMigration00017_SQLiteSequence(t *testing.T) {
	s, _, stateDB := setupStateTestStorage(t)
	ctx := context.Background()

	if err := s.RunMigrations(ctx); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	if stateDB.Migrator().HasTable("skill_publish_jobs") {
		t.Fatalf("skill_publish_jobs must be dropped after full up (000017 applied)")
	}
	if !stateDB.Migrator().HasTable("schema_migrations") {
		t.Fatalf("schema_migrations missing")
	}
	var applied []string
	if err := stateDB.Table("schema_migrations").Pluck("version", &applied).Error; err != nil {
		t.Fatalf("list versions: %v", err)
	}
	found := false
	for _, v := range applied {
		if v == "000017_drop_skill_publish_jobs" {
			found = true
		}
	}
	if !found {
		t.Fatalf("000017 version not recorded: %v", applied)
	}
}

// TestSkillPublishJobsMigration00017_Targeted 模拟真实清理顺序：库已停在
// 000016（含存量行）→ 导出备份 → 应用 000017 up（drop）→ down 重建 →
// 导入恢复 → 行数/hash 一致，共享表不受影响。
func TestSkillPublishJobsMigration00017_Targeted(t *testing.T) {
	s, _, stateDB := setupStateTestStorage(t)
	ctx := context.Background()
	if err := s.RunMigrations(ctx); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	// 全量迁移已应用 000017（表已 drop）；先 down 回滚 000017 重建存量环境。
	downSQL, err := migrations.FS.ReadFile("000017_drop_skill_publish_jobs.down.sql")
	if err != nil {
		t.Fatalf("read 000017 down: %v", err)
	}
	if err := stateDB.Exec(string(downSQL)).Error; err != nil {
		t.Fatalf("apply 000017 down: %v", err)
	}
	ids := seedSkillPublishJobRows(t, stateDB, 2)

	// 备份（BND-011：drop 前必做）。
	backup, err := s.ExportSkillPublishJobs(ctx)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if backup.RowCount != 2 {
		t.Fatalf("backup rows = %d", backup.RowCount)
	}

	// 应用 000017 up：只 drop 目标表；共享表（sessions/entitlement/audit）不动。
	upSQL, err := migrations.FS.ReadFile("000017_drop_skill_publish_jobs.up.sql")
	if err != nil {
		t.Fatalf("read 000017 up: %v", err)
	}
	if err := stateDB.Exec(string(upSQL)).Error; err != nil {
		t.Fatalf("apply 000017 up: %v", err)
	}
	if stateDB.Migrator().HasTable("skill_publish_jobs") {
		t.Fatalf("skill_publish_jobs must be dropped by 000017 up")
	}
	for _, shared := range []string{"sessions", "managed_entitlements", "audit_events", "identity_links", "sync_runs"} {
		if !stateDB.Migrator().HasTable(shared) {
			t.Fatalf("shared table %s must survive 000017", shared)
		}
	}

	// 恢复：down 重建结构 → 导入行。
	if err := stateDB.Exec(string(downSQL)).Error; err != nil {
		t.Fatalf("re-apply 000017 down: %v", err)
	}
	if err := s.ImportSkillPublishJobs(ctx, backup); err != nil {
		t.Fatalf("import: %v", err)
	}
	var count int64
	if err := stateDB.Table("skill_publish_jobs").Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("restored count = %d, %v", count, err)
	}
	var key string
	if err := stateDB.Raw(`SELECT idempotency_key FROM skill_publish_jobs WHERE job_id = ?`, ids[1]).Scan(&key).Error; err != nil {
		t.Fatalf("query restored: %v", err)
	}
	if key != "publish-001" {
		t.Fatalf("restored idempotency_key = %q", key)
	}
}

// TestSkillPublishJobsMigration00017_Postgres 在真实 PostgreSQL 上验证
// up/down/恢复与共享表保护（VAL-217）。需要 PLATFORM_STATE_TEST_PG_DSN
// 指向一次性空库；缺环境时 skip 并在 verification 记录为未执行（不算 PASS）。
func TestSkillPublishJobsMigration00017_Postgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PLATFORM_STATE_TEST_PG_DSN"))
	if dsn == "" {
		t.Skip("PLATFORM_STATE_TEST_PG_DSN not set; real-PG migration check not executed")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("connect pg: %v", err)
	}
	mainDB := openStateTestSQLite(t)
	s := NewStorageWithDBs(mainDB, db)
	ctx := context.Background()

	// 全量迁移到 000016 之后（含 000017）。空库 up 幂等。
	if err := s.RunMigrations(ctx); err != nil {
		t.Fatalf("pg RunMigrations: %v", err)
	}
	// 重建存量环境：down 000017 → 插行 → 备份 → up → 校验 → down → 导入。
	downSQL, err := migrations.FS.ReadFile("000017_drop_skill_publish_jobs.down.sql")
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	upSQL, err := migrations.FS.ReadFile("000017_drop_skill_publish_jobs.up.sql")
	if err != nil {
		t.Fatalf("read up: %v", err)
	}
	if err := db.Exec(string(downSQL)).Error; err != nil {
		t.Fatalf("pg down: %v", err)
	}
	seedSkillPublishJobRows(t, db, 2)
	backup, err := s.ExportSkillPublishJobs(ctx)
	if err != nil {
		t.Fatalf("pg backup: %v", err)
	}
	if err := db.Exec(string(upSQL)).Error; err != nil {
		t.Fatalf("pg up: %v", err)
	}
	if db.Migrator().HasTable("skill_publish_jobs") {
		t.Fatalf("pg: skill_publish_jobs must be dropped")
	}
	for _, shared := range []string{"sessions", "managed_entitlements", "audit_events", "identity_links", "sync_runs", "sync_steps"} {
		if !db.Migrator().HasTable(shared) {
			t.Fatalf("pg: shared table %s must survive", shared)
		}
	}
	if err := db.Exec(string(downSQL)).Error; err != nil {
		t.Fatalf("pg re-down: %v", err)
	}
	if err := s.ImportSkillPublishJobs(ctx, backup); err != nil {
		t.Fatalf("pg import: %v", err)
	}
	var count int64
	if err := db.Table("skill_publish_jobs").Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("pg restored count = %d, %v", count, err)
	}
	// 重启语义：RunMigrations 幂等重放 000017（DROP IF EXISTS），清理后账本保持
	// dropped，版本行补齐到 17；不得手工删历史版本伪装新装。
	if err := s.RunMigrations(ctx); err != nil {
		t.Fatalf("pg re-run migrations: %v", err)
	}
	var versions []string
	if err := db.Table("schema_migrations").Pluck("version", &versions).Error; err != nil {
		t.Fatalf("pg versions: %v", err)
	}
	if len(versions) != 17 {
		t.Fatalf("pg applied versions = %d, want 17 (000001~000017)", len(versions))
	}
	if db.Migrator().HasTable("skill_publish_jobs") {
		t.Fatalf("pg: skill_publish_jobs must stay dropped after restart re-migration")
	}
}

// TestSkillPublishJobsMigration00017_UpIdempotent 迁移 SQL 幂等：重复应用
// 000017 up 不报错、目标表保持 dropped（真实发布中失败重试不制造半状态）。
func TestSkillPublishJobsMigration00017_UpIdempotent(t *testing.T) {
	s, _, stateDB := setupStateTestStorage(t)
	ctx := context.Background()
	if err := s.RunMigrations(ctx); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	upSQL, err := migrations.FS.ReadFile("000017_drop_skill_publish_jobs.up.sql")
	if err != nil {
		t.Fatalf("read up: %v", err)
	}
	if err := stateDB.Exec(string(upSQL)).Error; err != nil {
		t.Fatalf("re-apply 000017 up: %v", err)
	}
	if stateDB.Migrator().HasTable("skill_publish_jobs") {
		t.Fatalf("skill_publish_jobs must stay dropped after re-apply")
	}
}
