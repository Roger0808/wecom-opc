package authz

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func openAuthzTestSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", uuid.NewString())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	return db
}

func setupAuthzStorage(t *testing.T) (*storage.Storage, *gorm.DB) {
	t.Helper()
	mainDB := openAuthzTestSQLite(t)
	stateDB := openAuthzTestSQLite(t)
	if err := stateDB.AutoMigrate(
		&model.AuditEvent{},
		&model.IdentityLink{},
		&model.ManagedEntitlement{},
		&model.AuthzEpoch{},
	); err != nil {
		t.Fatalf("migrate state schema: %v", err)
	}
	return storage.NewStorageWithDBs(mainDB, stateDB), stateDB
}

func insertAuthzTestLink(t *testing.T, s *storage.Storage, subject, knowledgeUserID, status string) {
	t.Helper()
	link := &model.IdentityLink{
		IdentitySource:   "wecom",
		OrganizationID:   "ww-test",
		ImmutableSubject: subject,
		KnowledgeUserID:  knowledgeUserID,
		Status:           status,
	}
	if err := s.InsertIdentityLink(context.Background(), link); err != nil {
		t.Fatalf("InsertIdentityLink: %v", err)
	}
}

func insertAuthzTestEntitlement(t *testing.T, s *storage.Storage, subject, tenant, capability string) {
	t.Helper()
	ent := &model.ManagedEntitlement{
		Subject:    subject,
		TenantID:   tenant,
		Capability: capability,
		Source:     "sync",
	}
	if err := s.InsertManagedEntitlement(context.Background(), ent); err != nil {
		t.Fatalf("InsertManagedEntitlement: %v", err)
	}
}

// ---------- Clock ----------

func TestFakeClock(t *testing.T) {
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	clock := NewFakeClock(base)
	if !clock.Now().Equal(base) {
		t.Fatalf("Now() = %v, want %v", clock.Now(), base)
	}
	clock.Advance(30 * time.Second)
	if !clock.Now().Equal(base.Add(30 * time.Second)) {
		t.Fatalf("Now() after advance = %v", clock.Now())
	}
}

// ---------- VersionSource ----------

// TestVersionSourceCacheTTL：≤5s 缓存以注入 Clock 判定新鲜度，过期后重读数据库。
func TestVersionSourceCacheTTL(t *testing.T) {
	s, _ := setupAuthzStorage(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	clock := NewFakeClock(base)

	src := NewVersionSource(s, clock, 5*time.Second)

	if v, err := src.Current(ctx, "subj-1"); err != nil || v != 0 {
		t.Fatalf("Current() = %d, %v; want 0", v, err)
	}
	if _, err := s.BumpAuthzVersion(ctx, "subj-1"); err != nil {
		t.Fatalf("BumpAuthzVersion: %v", err)
	}

	clock.Advance(4 * time.Second)
	if v, _ := src.Current(ctx, "subj-1"); v != 0 {
		t.Fatalf("cached Current() = %d, want 0 (4s < 5s TTL)", v)
	}

	clock.Advance(1 * time.Second) // 恰满 5s，缓存过期
	if v, _ := src.Current(ctx, "subj-1"); v != 1 {
		t.Fatalf("Current() after TTL = %d, want 1", v)
	}
}

// TestVersionSourceCurrentFresh：CurrentFresh 绕过缓存直读（revoke 后判定路径用）。
func TestVersionSourceCurrentFresh(t *testing.T) {
	s, _ := setupAuthzStorage(t)
	ctx := context.Background()
	clock := NewFakeClock(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	src := NewVersionSource(s, clock, 5*time.Second)

	if _, err := s.BumpAuthzVersion(ctx, "subj-1"); err != nil {
		t.Fatalf("bump: %v", err)
	}
	// 预热缓存
	if v, _ := src.Current(ctx, "subj-1"); v != 1 {
		t.Fatalf("Current() = %d, want 1", v)
	}
	if _, err := s.BumpAuthzVersion(ctx, "subj-1"); err != nil {
		t.Fatalf("bump2: %v", err)
	}
	if v, _ := src.CurrentFresh(ctx, "subj-1"); v != 2 {
		t.Fatalf("CurrentFresh() = %d, want 2 (bypass cache)", v)
	}
	if v, _ := src.Current(ctx, "subj-1"); v != 1 {
		t.Fatalf("Current() = %d, want 1 (cache intact)", v)
	}
}

// TestBumpAuthzVersionConcurrentNoLoss：并发 bump 不丢版本。
// 使用文件型 sqlite（WAL + busy_timeout）放开连接池，让并发真实发生。
func TestBumpAuthzVersionConcurrentNoLoss(t *testing.T) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", filepath.Join(t.TempDir(), "state.db"))
	stateDB, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open file sqlite: %v", err)
	}
	if err := stateDB.AutoMigrate(&model.AuthzEpoch{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := storage.NewStorageWithDBs(nil, stateDB)
	ctx := context.Background()

	const workers = 64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.BumpAuthzVersion(ctx, "subj-race"); err != nil {
				t.Errorf("bump: %v", err)
			}
		}()
	}
	wg.Wait()

	if v, err := s.CurrentAuthzVersion(ctx, "subj-race"); err != nil || v != workers {
		t.Fatalf("final version = %d, %v; want %d (no lost update)", v, err, workers)
	}
}

// ---------- EntitlementChecker ----------

func TestRequireEntitlement(t *testing.T) {
	s, _ := setupAuthzStorage(t)
	ctx := context.Background()
	clock := NewFakeClock(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	checker := NewEntitlementChecker(s, NewVersionSource(s, clock, 0))

	// 未供给：无 identity link
	if _, err := checker.RequireEntitlement(ctx, "ghost", "t1", ""); !errors.Is(err, ErrIdentityDisabled) {
		t.Fatalf("ghost subject err = %v, want ErrIdentityDisabled", err)
	}

	insertAuthzTestLink(t, s, "subj-1", "ku-1", "disabled")
	if _, err := checker.RequireEntitlement(ctx, "subj-1", "t1", ""); !errors.Is(err, ErrIdentityDisabled) {
		t.Fatalf("disabled link err = %v, want ErrIdentityDisabled", err)
	}

	insertAuthzTestLink(t, s, "subj-2", "ku-2", "active")
	if _, err := checker.RequireEntitlement(ctx, "subj-2", "t1", ""); !errors.Is(err, ErrEntitlementMissing) {
		t.Fatalf("no entitlement err = %v, want ErrEntitlementMissing", err)
	}

	insertAuthzTestEntitlement(t, s, "subj-2", "t1", "chat")
	if _, err := checker.RequireEntitlement(ctx, "subj-2", "t1", "docs"); !errors.Is(err, ErrEntitlementMissing) {
		t.Fatalf("wrong capability err = %v, want ErrEntitlementMissing", err)
	}

	version, err := checker.RequireEntitlement(ctx, "subj-2", "t1", "")
	if err != nil {
		t.Fatalf("wildcard capability: %v", err)
	}
	if version != 0 {
		t.Fatalf("version = %d, want 0", version)
	}
	if _, err := s.BumpAuthzVersion(ctx, "subj-2"); err != nil {
		t.Fatalf("bump: %v", err)
	}
	version, err = checker.RequireEntitlement(ctx, "subj-2", "t1", "chat")
	if err != nil || version != 1 {
		t.Fatalf("RequireEntitlement = %d, %v; want 1", version, err)
	}
}

// ---------- Notifier ----------

type fakeChannel struct {
	name string
	mu   sync.Mutex
	err  error
	got  []InvalidateEvent
}

func (f *fakeChannel) Name() string { return f.name }

func (f *fakeChannel) Notify(ctx context.Context, ev InvalidateEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, ev)
	return f.err
}

func (f *fakeChannel) delivered() []InvalidateEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]InvalidateEvent(nil), f.got...)
}

func TestNotifierDeliverAndRetryTTL(t *testing.T) {
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	clock := NewFakeClock(base)
	n := NewNotifier(clock)

	okCh := &fakeChannel{name: "ok"}
	failCh := &fakeChannel{name: "fail", err: errors.New("channel down")}
	n.Register(okCh)
	n.Register(failCh)

	ev := InvalidateEvent{Subject: "subj-1", Reason: "offboard", Version: 3, At: base}
	n.Invalidate(context.Background(), ev)

	if got := okCh.delivered(); len(got) != 1 || got[0].Subject != "subj-1" {
		t.Fatalf("ok channel delivered = %v", got)
	}
	if n.PendingRetries() != 1 {
		t.Fatalf("PendingRetries = %d, want 1 (failed notify enqueued)", n.PendingRetries())
	}

	// 未到期不投递
	n.DispatchDue(base.Add(500 * time.Millisecond))
	if n.PendingRetries() != 1 {
		t.Fatalf("PendingRetries before due = %d, want 1", n.PendingRetries())
	}

	// 到期重投：失败仍在队列
	n.DispatchDue(base.Add(time.Second))
	if n.PendingRetries() != 1 {
		t.Fatalf("PendingRetries after first retry = %d, want 1", n.PendingRetries())
	}

	// 修复通道：下一轮重投成功出队
	failCh.err = nil
	n.DispatchDue(base.Add(2 * time.Second))
	if n.PendingRetries() != 0 {
		t.Fatalf("PendingRetries after success = %d, want 0", n.PendingRetries())
	}
	if got := failCh.delivered(); len(got) != 3 {
		t.Fatalf("fail channel delivered = %d, want 3 (initial + 2 retries)", len(got))
	}
}

func TestNotifierRetryDropsAfterTTL(t *testing.T) {
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	clock := NewFakeClock(base)
	n := NewNotifier(clock)
	n.Register(&fakeChannel{name: "fail", err: errors.New("down")})

	n.Invalidate(context.Background(), InvalidateEvent{Subject: "subj-1", At: base})
	if n.PendingRetries() != 1 {
		t.Fatalf("PendingRetries = %d, want 1", n.PendingRetries())
	}

	// 30s TTL 之后：事件丢弃，不再重试
	n.DispatchDue(base.Add(31 * time.Second))
	if n.PendingRetries() != 0 {
		t.Fatalf("PendingRetries after TTL = %d, want 0", n.PendingRetries())
	}
}

func TestNotifierNoChannelIsNoop(t *testing.T) {
	n := NewNotifier(NewFakeClock(time.Now()))
	n.Invalidate(context.Background(), InvalidateEvent{Subject: "subj-1"})
	if n.PendingRetries() != 0 {
		t.Fatalf("PendingRetries = %d, want 0", n.PendingRetries())
	}
}

func TestNoopTokenRevoker(t *testing.T) {
	deleted, err := NoopTokenRevoker{}.RevokeTokens(context.Background(), "ku-1")
	if err != nil || deleted != 0 {
		t.Fatalf("NoopTokenRevoker = %d, %v; want 0, nil", deleted, err)
	}
}
