package storage

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"weknora-tokendance/migrations"
	"weknora-tokendance/pkg/model"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// openStateTestSQLite 打开一个独立连接的 sqlite 内存库（glebarez/sqlite，跟随仓库现有测试模式）。
func openStateTestSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", uuid.New().String())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("failed to open sqlite in-memory db: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to get sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	return db
}

// setupStateTestStorage 返回同时持有主库（WeKnora 角色）与状态库（statedb 角色）内存连接的 Storage，
// 状态库为空 schema（由调用方决定跑真实迁移 SQL 还是 GORM AutoMigrate）。
func setupStateTestStorage(t *testing.T) (*Storage, *gorm.DB, *gorm.DB) {
	t.Helper()
	mainDB := openStateTestSQLite(t)
	stateDB := openStateTestSQLite(t)
	return NewStorageWithDBs(mainDB, stateDB), mainDB, stateDB
}

// migrateStateSchema 用 GORM AutoMigrate 在状态库上建立平台模型 schema
// （与 handler 测试同模式；sqlite 下 datetime 列可被驱动扫描，适合 repository 往返测试）。
func migrateStateSchema(t *testing.T, stateDB *gorm.DB) {
	t.Helper()
	if err := stateDB.AutoMigrate(
		&model.AuditEvent{},
		&model.IdentityLink{},
		&model.AuthTransaction{},
		&model.Session{},
		&model.RuntimeGrant{},
		&model.WorkspaceLink{},
		&model.ManagedEntitlement{},
		&model.SyncRun{},
		&model.SyncStep{},
		&model.SkillPublishJob{},
		&model.AuthzEpoch{},
	); err != nil {
		t.Fatalf("failed to auto migrate state schema: %v", err)
	}
}

// appliedVersions 读取状态库 schema_migrations 中已记录的版本。
func appliedVersions(t *testing.T, db *gorm.DB) []string {
	t.Helper()
	var versions []string
	if err := db.Table("schema_migrations").Pluck("version", &versions).Error; err != nil {
		t.Fatalf("failed to list schema_migrations: %v", err)
	}
	sort.Strings(versions)
	return versions
}

// embeddedUpMigrations 返回内嵌的全部 up 迁移文件名（排序后）。
func embeddedUpMigrations(t *testing.T) []string {
	t.Helper()
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("failed to read embedded migrations: %v", err)
	}
	names := make([]string, 0)
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

var platformTables = []string{
	"audit_events", "identity_links", "auth_transactions",
	"sessions", "runtime_grants", "workspace_links", "managed_entitlements",
	"sync_runs", "sync_steps", "skill_publish_jobs",
}

// TestRunMigrations_UpDownUp_Idempotent 用真实内嵌迁移 SQL 验证 up→down→up 幂等，
// 且平台表全部落在状态库、主库零新增。
func TestRunMigrations_UpDownUp_Idempotent(t *testing.T) {
	s, mainDB, stateDB := setupStateTestStorage(t)
	ctx := context.Background()

	// up：首次执行全部迁移
	if err := s.RunMigrations(ctx); err != nil {
		t.Fatalf("first RunMigrations failed: %v", err)
	}
	ups := embeddedUpMigrations(t)
	versions := appliedVersions(t, stateDB)
	if len(versions) != len(ups) {
		t.Fatalf("expected %d applied versions, got %d: %v", len(ups), len(versions), versions)
	}

	// 幂等：重复执行不新增版本、不报错
	if err := s.RunMigrations(ctx); err != nil {
		t.Fatalf("second RunMigrations failed: %v", err)
	}
	if again := appliedVersions(t, stateDB); len(again) != len(ups) {
		t.Fatalf("re-run changed applied versions: %v", again)
	}

	// 平台表全部建立在状态库上，主库不受影响
	for _, table := range platformTables {
		if !stateDB.Migrator().HasTable(table) {
			t.Errorf("state db missing table %s", table)
		}
		if mainDB.Migrator().HasTable(table) {
			t.Errorf("main db must not have platform table %s", table)
		}
	}

	// down：按版本倒序执行全部 down.sql（文件内自带版本行删除）
	downs := make([]string, 0, len(ups))
	for _, name := range ups {
		downs = append(downs, strings.TrimSuffix(name, ".up.sql")+".down.sql")
	}
	sort.Sort(sort.Reverse(sort.StringSlice(downs)))
	for _, name := range downs {
		sqlBytes, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatalf("read down migration %s: %v", name, err)
		}
		if err := stateDB.Exec(string(sqlBytes)).Error; err != nil {
			t.Fatalf("apply down migration %s: %v", name, err)
		}
	}
	for _, table := range platformTables {
		if stateDB.Migrator().HasTable(table) {
			t.Errorf("after down, state db should not have table %s", table)
		}
	}
	if versions := appliedVersions(t, stateDB); len(versions) != 0 {
		t.Fatalf("after down, expected no applied versions, got %v", versions)
	}

	// 再 up：全新重建成功
	if err := s.RunMigrations(ctx); err != nil {
		t.Fatalf("re-up RunMigrations failed: %v", err)
	}
	if versions := appliedVersions(t, stateDB); len(versions) != len(ups) {
		t.Fatalf("re-up applied versions mismatch: %v", versions)
	}
}

func TestRunMigrations_RequiresStateDB(t *testing.T) {
	s := NewStorageWithDB(openStateTestSQLite(t))
	if err := s.RunMigrations(context.Background()); err == nil {
		t.Fatalf("expected error when state db is not configured")
	}
}

// TestUniqueConstraints_FromMigrationSQL 在真实迁移 SQL 建立的 schema 上，
// 用 raw INSERT 验证唯一约束（不经过 GORM，约束即迁移文件里声明的那一条）。
func TestUniqueConstraints_FromMigrationSQL(t *testing.T) {
	s, _, stateDB := setupStateTestStorage(t)
	ctx := context.Background()
	if err := s.RunMigrations(ctx); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}

	exec := func(sql string) error {
		return stateDB.Exec(sql).Error
	}

	t.Run("identity_links (identity_source, organization_id, immutable_subject)", func(t *testing.T) {
		base := `INSERT INTO identity_links (id, identity_source, organization_id, immutable_subject, knowledge_user_id, status) VALUES ('%s','wecom','corp-1','subj-1','ku-1','active')`
		if err := exec(fmt.Sprintf(base, uuid.New().String())); err != nil {
			t.Fatalf("first insert failed: %v", err)
		}
		if err := exec(fmt.Sprintf(base, uuid.New().String())); err == nil {
			t.Fatalf("expected unique constraint violation")
		}
		// 不同 immutable_subject 放行
		ok := `INSERT INTO identity_links (id, identity_source, organization_id, immutable_subject, knowledge_user_id, status) VALUES ('%s','wecom','corp-1','subj-2','ku-2','active')`
		if err := exec(fmt.Sprintf(ok, uuid.New().String())); err != nil {
			t.Fatalf("different subject should be allowed: %v", err)
		}
	})

	t.Run("workspace_links (identity_link_id, code_workspace_id)", func(t *testing.T) {
		linkID, wsID := uuid.New().String(), uuid.New().String()
		base := fmt.Sprintf(`INSERT INTO workspace_links (identity_link_id, code_workspace_id, knowledge_tenant_id, source) VALUES ('%s','%s','tenant-1','manual')`, linkID, wsID)
		if err := exec(base); err != nil {
			t.Fatalf("first insert failed: %v", err)
		}
		if err := exec(base); err == nil {
			t.Fatalf("expected unique constraint violation")
		}
	})

	t.Run("managed_entitlements (subject, tenant_id, capability, source)", func(t *testing.T) {
		base := `INSERT INTO managed_entitlements (id, subject, tenant_id, capability, source) VALUES ('%s','user-1','tenant-1','skill.install','sync')`
		if err := exec(fmt.Sprintf(base, uuid.New().String())); err != nil {
			t.Fatalf("first insert failed: %v", err)
		}
		if err := exec(fmt.Sprintf(base, uuid.New().String())); err == nil {
			t.Fatalf("expected unique constraint violation")
		}
		// 不同 source 放行
		ok := `INSERT INTO managed_entitlements (id, subject, tenant_id, capability, source) VALUES ('%s','user-1','tenant-1','skill.install','manual')`
		if err := exec(fmt.Sprintf(ok, uuid.New().String())); err != nil {
			t.Fatalf("different source should be allowed: %v", err)
		}
	})

	t.Run("sync_runs.idempotency_key", func(t *testing.T) {
		base := `INSERT INTO sync_runs (run_id, scope, idempotency_key, rule_version, triggered_by, status) VALUES ('%s','full','idem-001','v1','admin:test','queued')`
		if err := exec(fmt.Sprintf(base, uuid.New().String())); err != nil {
			t.Fatalf("first insert failed: %v", err)
		}
		if err := exec(fmt.Sprintf(base, uuid.New().String())); err == nil {
			t.Fatalf("expected unique constraint violation")
		}
	})

	t.Run("skill_publish_jobs.idempotency_key", func(t *testing.T) {
		base := `INSERT INTO skill_publish_jobs (job_id, release_id, source_tenant, targets, idempotency_key, status) VALUES ('%s','release-1','tenant-10001','[{"tenant_id":"t2"}]','publish-001','queued')`
		if err := exec(fmt.Sprintf(base, uuid.New().String())); err != nil {
			t.Fatalf("first insert failed: %v", err)
		}
		if err := exec(fmt.Sprintf(base, uuid.New().String())); err == nil {
			t.Fatalf("expected unique constraint violation")
		}
	})

	// L5：sync_steps (run_id, idempotency_key) 唯一约束（000016）；NULL 键不参与唯一判定。
	t.Run("sync_steps (run_id, idempotency_key)", func(t *testing.T) {
		runA, runB := uuid.New().String(), uuid.New().String()
		insert := func(runID string, seq int, key interface{}) error {
			keySQL := "NULL"
			if k, ok := key.(string); ok {
				keySQL = fmt.Sprintf("'%s'", k)
			}
			return exec(fmt.Sprintf(
				`INSERT INTO sync_steps (run_id, step_seq, step_type, idempotency_key) VALUES ('%s',%d,'provision',%s)`,
				runID, seq, keySQL))
		}
		if err := insert(runA, 1, "key-1"); err != nil {
			t.Fatalf("first insert failed: %v", err)
		}
		if err := insert(runA, 2, "key-1"); err == nil {
			t.Fatalf("expected unique constraint violation on (run_id, idempotency_key)")
		}
		if err := insert(runB, 1, "key-1"); err != nil {
			t.Fatalf("same key in another run should be allowed: %v", err)
		}
		// 可空列语义保持：同 run 多个 NULL 幂等键（元步骤）不冲突
		if err := insert(runA, 3, nil); err != nil {
			t.Fatalf("nil key insert failed: %v", err)
		}
		if err := insert(runA, 4, nil); err != nil {
			t.Fatalf("second nil key must be allowed (nullable unique semantics): %v", err)
		}
	})
}

// TestAuthTransactionStateGuards H4：FailAuthTransaction 仅 pending 可转 failed
// （completed 终态不被迟到回调翻转）；ConsumeAuthCode 只认 completed+未消费
// （与 code-exchange 对事务状态的口径一致）。
func TestAuthTransactionStateGuards(t *testing.T) {
	s, _, _ := setupStateTestStorage(t)
	migrateStateSchema(t, s.stateDB)
	ctx := context.Background()

	newTxn := func() *model.AuthTransaction {
		txn := &model.AuthTransaction{
			ClientID: "work-desktop", RedirectURI: "http://127.0.0.1:5173/callback",
			StateHash: uuid.New().String(), CodeChallenge: "cc", DeviceFingerprint: "dev",
			Audience: "work", ExpiresAt: time.Now().Add(10 * time.Minute),
		}
		if err := s.InsertAuthTransaction(ctx, txn); err != nil {
			t.Fatalf("InsertAuthTransaction: %v", err)
		}
		return txn
	}

	// pending → failed：生效一次；重复 failed 是幂等空操作
	txn := newTxn()
	if rows, err := s.FailAuthTransaction(ctx, txn.LoginTxnID); err != nil || rows != 1 {
		t.Fatalf("fail pending txn: rows=%d err=%v, want 1", rows, err)
	}
	if rows, err := s.FailAuthTransaction(ctx, txn.LoginTxnID); err != nil || rows != 0 {
		t.Fatalf("re-fail failed txn: rows=%d err=%v, want 0（幂等）", rows, err)
	}

	// completed 是终态：迟到/重试回调的失败路径不得翻转
	txn2 := newTxn()
	codeHash := "hash-code-2"
	if rows, err := s.IssueAuthCode(ctx, txn2.LoginTxnID, codeHash, uuid.New().String(), time.Now().Add(2*time.Minute)); err != nil || rows != 1 {
		t.Fatalf("issue auth code: rows=%d err=%v", rows, err)
	}
	if rows, err := s.FailAuthTransaction(ctx, txn2.LoginTxnID); err != nil || rows != 0 {
		t.Fatalf("fail completed txn: rows=%d err=%v, want 0（终态不可逆）", rows, err)
	}
	got, err := s.GetAuthTransaction(ctx, txn2.LoginTxnID)
	if err != nil {
		t.Fatalf("GetAuthTransaction: %v", err)
	}
	if got.Status != "completed" {
		t.Fatalf("txn status = %s, want completed", got.Status)
	}

	// ConsumeAuthCode 口径：completed+未消费才可消费；failed 事务上的 code 不可消费
	if rows, err := s.ConsumeAuthCode(ctx, txn2.LoginTxnID, codeHash); err != nil || rows != 1 {
		t.Fatalf("consume completed code: rows=%d err=%v, want 1", rows, err)
	}
	if rows, err := s.ConsumeAuthCode(ctx, txn2.LoginTxnID, codeHash); err != nil || rows != 0 {
		t.Fatalf("replay consume: rows=%d err=%v, want 0", rows, err)
	}
	// failed 事务即使塞入 code_hash 也不可消费（status 口径统一为 completed）
	txn3 := newTxn()
	if rows, err := s.FailAuthTransaction(ctx, txn3.LoginTxnID); err != nil || rows != 1 {
		t.Fatalf("fail txn3: rows=%d err=%v", rows, err)
	}
	if err := s.stateDB.Exec("UPDATE auth_transactions SET code_hash = ? WHERE login_txn_id = ?", "hash-code-3", txn3.LoginTxnID).Error; err != nil {
		t.Fatalf("seed code_hash on failed txn: %v", err)
	}
	if rows, err := s.ConsumeAuthCode(ctx, txn3.LoginTxnID, "hash-code-3"); err != nil || rows != 0 {
		t.Fatalf("consume on failed txn: rows=%d err=%v, want 0（非 completed 不认）", rows, err)
	}
}

// TestAuditEvents_StateOnlyAndAppendOnly 验证审计写入只落状态库、主键拒绝重放。
func TestAuditEvents_StateOnlyAndAppendOnly(t *testing.T) {
	s, mainDB, _ := setupStateTestStorage(t)
	migrateStateSchema(t, s.stateDB)
	ctx := context.Background()

	event := &model.AuditEvent{Actor: "admin:test", Action: "admin.auth.denied", Target: "GET /admin/api/x", RequestID: "req-1"}
	if err := s.InsertAuditEvent(ctx, event); err != nil {
		t.Fatalf("InsertAuditEvent failed: %v", err)
	}
	if event.ID == "" {
		t.Fatalf("expected auto-generated event ID")
	}
	got, err := s.GetAuditEvent(ctx, event.ID)
	if err != nil {
		t.Fatalf("GetAuditEvent failed: %v", err)
	}
	if got.Actor != event.Actor || got.Action != event.Action || got.RequestID != event.RequestID {
		t.Errorf("round-trip mismatch: got %+v", got)
	}
	events, err := s.ListAuditEvents(ctx)
	if err != nil || len(events) != 1 {
		t.Fatalf("ListAuditEvents = %d, %v", len(events), err)
	}
	if mainDB.Migrator().HasTable("audit_events") {
		t.Fatalf("audit_events must not be created on main db")
	}

	// 同一 ID 第二次插入必须被主键约束拒绝（只追加语义下的重放防护）
	dup := &model.AuditEvent{ID: event.ID, Actor: "admin:test", Action: "admin.auth.denied", Target: "t", RequestID: "req-2", CreatedAt: time.Now()}
	if err := s.InsertAuditEvent(ctx, dup); err == nil {
		t.Fatalf("expected primary key violation on duplicate audit event id")
	}
}

func TestIdentityLinks_RoundTrip(t *testing.T) {
	s, _, _ := setupStateTestStorage(t)
	migrateStateSchema(t, s.stateDB)
	ctx := context.Background()

	link := &model.IdentityLink{
		IdentitySource:   "wecom",
		OrganizationID:   "corp-1",
		ImmutableSubject: "subject-1",
		KnowledgeUserID:  "k-user-1",
	}
	if err := s.InsertIdentityLink(ctx, link); err != nil {
		t.Fatalf("InsertIdentityLink failed: %v", err)
	}
	if link.ID == "" {
		t.Fatalf("expected auto-generated ID")
	}

	got, err := s.GetIdentityLink(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetIdentityLink failed: %v", err)
	}
	if got.ImmutableSubject != link.ImmutableSubject || got.KnowledgeUserID != link.KnowledgeUserID {
		t.Errorf("round-trip mismatch: got %+v", got)
	}
	if got.Status != "active" { // DB 默认值
		t.Errorf("expected default status active, got %q", got.Status)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("expected timestamps auto-filled: %+v", got)
	}

	other := &model.IdentityLink{
		IdentitySource:   "wecom",
		OrganizationID:   "corp-1",
		ImmutableSubject: "subject-2",
		KnowledgeUserID:  "k-user-3",
		Status:           "conflicted",
	}
	if err := s.InsertIdentityLink(ctx, other); err != nil {
		t.Fatalf("InsertIdentityLink other failed: %v", err)
	}
	links, err := s.ListIdentityLinks(ctx)
	if err != nil || len(links) != 2 {
		t.Fatalf("ListIdentityLinks = %d, %v", len(links), err)
	}
}

func TestWorkspaceLinks_RoundTrip(t *testing.T) {
	s, _, _ := setupStateTestStorage(t)
	migrateStateSchema(t, s.stateDB)
	ctx := context.Background()

	link := &model.WorkspaceLink{
		IdentityLinkID:    uuid.New().String(),
		CodeWorkspaceID:   uuid.New().String(),
		KnowledgeTenantID: "tenant-1",
		Source:            "manual",
	}
	if err := s.InsertWorkspaceLink(ctx, link); err != nil {
		t.Fatalf("InsertWorkspaceLink failed: %v", err)
	}

	got, err := s.GetWorkspaceLink(ctx, link.IdentityLinkID, link.CodeWorkspaceID)
	if err != nil {
		t.Fatalf("GetWorkspaceLink failed: %v", err)
	}
	if got.KnowledgeTenantID != link.KnowledgeTenantID || got.Source != "manual" {
		t.Errorf("round-trip mismatch: got %+v", got)
	}

	links, err := s.ListWorkspaceLinks(ctx)
	if err != nil || len(links) != 1 {
		t.Fatalf("ListWorkspaceLinks = %d, %v", len(links), err)
	}
}

func TestManagedEntitlements_RoundTrip(t *testing.T) {
	s, _, _ := setupStateTestStorage(t)
	migrateStateSchema(t, s.stateDB)
	ctx := context.Background()

	ent := &model.ManagedEntitlement{
		Subject:     "user-1",
		TenantID:    "tenant-1",
		Capability:  "skill.install",
		Source:      "sync",
		RuleVersion: strPtr("v1"),
		GrantedBy:   strPtr("admin:test"),
	}
	if err := s.InsertManagedEntitlement(ctx, ent); err != nil {
		t.Fatalf("InsertManagedEntitlement failed: %v", err)
	}
	got, err := s.GetManagedEntitlement(ctx, ent.ID)
	if err != nil {
		t.Fatalf("GetManagedEntitlement failed: %v", err)
	}
	if got.Subject != ent.Subject || got.Capability != ent.Capability || got.Source != "sync" {
		t.Errorf("round-trip mismatch: got %+v", got)
	}
	if got.RuleVersion == nil || *got.RuleVersion != "v1" || got.GrantedBy == nil || *got.GrantedBy != "admin:test" {
		t.Errorf("nullable fields mismatch: got %+v", got)
	}
	if got.AuthzVersion != 0 {
		t.Errorf("expected default authz_version 0, got %d", got.AuthzVersion)
	}

	list, err := s.ListManagedEntitlements(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListManagedEntitlements = %d, %v", len(list), err)
	}
}

func TestSyncRunsAndSteps_RoundTrip(t *testing.T) {
	s, _, _ := setupStateTestStorage(t)
	migrateStateSchema(t, s.stateDB)
	ctx := context.Background()

	run := &model.SyncRun{
		Scope:          "full",
		IdempotencyKey: "sync-2026-10-20-001",
		RuleVersion:    "v1",
		TriggeredBy:    "admin:test",
		Status:         "queued",
	}
	if err := s.InsertSyncRun(ctx, run); err != nil {
		t.Fatalf("InsertSyncRun failed: %v", err)
	}
	gotRun, err := s.GetSyncRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetSyncRun failed: %v", err)
	}
	if gotRun.IdempotencyKey != run.IdempotencyKey || gotRun.Status != "queued" {
		t.Errorf("round-trip mismatch: got %+v", gotRun)
	}

	step1 := &model.SyncStep{RunID: run.RunID, StepSeq: 1, StepType: "snapshot", Status: "applied", Retryable: true}
	step2 := &model.SyncStep{RunID: run.RunID, StepSeq: 2, StepType: "diff", Status: "pending", SubjectRef: strPtr("user-1")}
	for _, step := range []*model.SyncStep{step1, step2} {
		if err := s.InsertSyncStep(ctx, step); err != nil {
			t.Fatalf("InsertSyncStep failed: %v", err)
		}
	}
	gotStep, err := s.GetSyncStep(ctx, run.RunID, 2)
	if err != nil {
		t.Fatalf("GetSyncStep failed: %v", err)
	}
	if gotStep.StepType != "diff" || gotStep.SubjectRef == nil || *gotStep.SubjectRef != "user-1" {
		t.Errorf("round-trip mismatch: got %+v", gotStep)
	}
	if !gotStep.Retryable {
		t.Errorf("expected retryable true round-trip")
	}
	steps, err := s.ListSyncSteps(ctx, run.RunID)
	if err != nil || len(steps) != 2 || steps[0].StepSeq != 1 || steps[1].StepSeq != 2 {
		t.Fatalf("ListSyncSteps = %v, %v", steps, err)
	}
}

func TestSkillPublishJobs_RoundTrip(t *testing.T) {
	s, _, _ := setupStateTestStorage(t)
	migrateStateSchema(t, s.stateDB)
	ctx := context.Background()

	job := &model.SkillPublishJob{
		ReleaseID:      "release-1",
		SourceTenant:   "tenant-10001",
		Targets:        `[{"tenant_id":"t2"},{"tenant_id":"t3"}]`,
		IdempotencyKey: "publish-001",
		Status:         "queued",
		Steps:          strPtr(`[{"tenant_id":"t2","status":"succeeded"}]`),
	}
	if err := s.InsertSkillPublishJob(ctx, job); err != nil {
		t.Fatalf("InsertSkillPublishJob failed: %v", err)
	}
	got, err := s.GetSkillPublishJob(ctx, job.JobID)
	if err != nil {
		t.Fatalf("GetSkillPublishJob failed: %v", err)
	}
	if got.ReleaseID != job.ReleaseID || got.Targets != job.Targets || got.Status != "queued" {
		t.Errorf("round-trip mismatch: got %+v", got)
	}
	if got.Steps == nil || *got.Steps != *job.Steps {
		t.Errorf("steps mismatch: got %+v", got)
	}

	jobs, err := s.ListSkillPublishJobs(ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("ListSkillPublishJobs = %d, %v", len(jobs), err)
	}
}

func TestAuthTransactionsAndSessionsAndGrants_RoundTrip(t *testing.T) {
	s, _, _ := setupStateTestStorage(t)
	migrateStateSchema(t, s.stateDB)
	ctx := context.Background()

	now := time.Now()
	txn := &model.AuthTransaction{
		ClientID:          "opc-web",
		RedirectURI:       "https://app.example.com/cb",
		StateHash:         "hash-state",
		CodeChallenge:     "challenge",
		DeviceFingerprint: "fp-1",
		CodeHash:          strPtr("hash-code"),
		Audience:          "opc",
		ExpiresAt:         now.Add(2 * time.Minute),
	}
	if err := s.InsertAuthTransaction(ctx, txn); err != nil {
		t.Fatalf("InsertAuthTransaction failed: %v", err)
	}
	gotTxn, err := s.GetAuthTransaction(ctx, txn.LoginTxnID)
	if err != nil {
		t.Fatalf("GetAuthTransaction failed: %v", err)
	}
	if gotTxn.ClientID != txn.ClientID || gotTxn.CodeHash == nil || *gotTxn.CodeHash != "hash-code" {
		t.Errorf("auth transaction round-trip mismatch: got %+v", gotTxn)
	}
	if gotTxn.Status != "pending" { // DB 默认值
		t.Errorf("expected default status pending, got %q", gotTxn.Status)
	}
	if txns, err := s.ListAuthTransactions(ctx); err != nil || len(txns) != 1 {
		t.Fatalf("ListAuthTransactions = %d, %v", len(txns), err)
	}

	linkID := uuid.New().String()
	session := &model.Session{
		IdentityLinkID:      linkID,
		FamilyID:            uuid.New().String(),
		RefreshVerifierHash: "verifier-hash",
		DeviceFingerprint:   "fp-1",
		AuthzVersion:        3,
		ExpiresAt:           now.Add(24 * time.Hour),
		LastSeenAt:          &now,
	}
	if err := s.InsertSession(ctx, session); err != nil {
		t.Fatalf("InsertSession failed: %v", err)
	}
	gotSession, err := s.GetSession(ctx, session.SessionID)
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}
	if gotSession.IdentityLinkID != linkID || gotSession.AuthzVersion != 3 || gotSession.RefreshVerifierHash != "verifier-hash" {
		t.Errorf("session round-trip mismatch: got %+v", gotSession)
	}
	if gotSession.LastSeenAt == nil || gotSession.LastSeenAt.Unix() != now.Unix() {
		t.Errorf("last_seen_at mismatch: got %+v", gotSession.LastSeenAt)
	}
	if sessions, err := s.ListSessions(ctx); err != nil || len(sessions) != 1 {
		t.Fatalf("ListSessions = %d, %v", len(sessions), err)
	}

	grant := &model.RuntimeGrant{
		SessionID:         &session.SessionID,
		Subject:           "svc-work",
		TenantID:          "tenant-1",
		OriginatingUserID: "user-1",
		Capabilities:      strPtr(`{"read":true}`),
		AuthzVersion:      3,
		LeaseExpiresAt:    now.Add(30 * time.Second),
	}
	if err := s.InsertRuntimeGrant(ctx, grant); err != nil {
		t.Fatalf("InsertRuntimeGrant failed: %v", err)
	}
	gotGrant, err := s.GetRuntimeGrant(ctx, grant.GrantID)
	if err != nil {
		t.Fatalf("GetRuntimeGrant failed: %v", err)
	}
	if gotGrant.Subject != "svc-work" || gotGrant.SessionID == nil || *gotGrant.SessionID != session.SessionID {
		t.Errorf("grant round-trip mismatch: got %+v", gotGrant)
	}
	if gotGrant.Capabilities == nil || *gotGrant.Capabilities != `{"read":true}` {
		t.Errorf("capabilities mismatch: got %+v", gotGrant.Capabilities)
	}
	if grants, err := s.ListRuntimeGrants(ctx); err != nil || len(grants) != 1 {
		t.Fatalf("ListRuntimeGrants = %d, %v", len(grants), err)
	}
}

func TestStateRepositories_RequireStateDB(t *testing.T) {
	s := NewStorageWithDB(openStateTestSQLite(t))
	ctx := context.Background()

	if err := s.InsertIdentityLink(ctx, &model.IdentityLink{}); err == nil {
		t.Fatalf("expected error when state db not configured")
	}
	if err := s.InsertAuditEvent(ctx, &model.AuditEvent{}); err == nil {
		t.Fatalf("expected error when state db not configured")
	}
	if _, err := s.GetSyncRun(ctx, "x"); err == nil {
		t.Fatalf("expected error when state db not configured")
	}
	if _, err := s.ListSessions(ctx); err == nil {
		t.Fatalf("expected error when state db not configured")
	}
}

func strPtr(s string) *string { return &s }
