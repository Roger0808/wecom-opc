package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/adminauth"
	"weknora-tokendance/pkg/authz"
	"weknora-tokendance/pkg/kbridge"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/golang-jwt/jwt/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ---------- 测试装配 ----------

var internalTestBase = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// setupInternalTest 装配 /integration/v1/internal 全部端点（FakeClock 默认注入）。
func setupInternalTest(t *testing.T, opts ...InternalOption) (*gin.Engine, *storage.Storage, *authz.FakeClock) {
	t.Helper()
	return setupInternalTestWithDBs(t, openIntegrationTestSQLite(t), openIntegrationTestSQLite(t), opts...)
}

func setupInternalTestWithDBs(t *testing.T, mainDB, stateDB *gorm.DB, opts ...InternalOption) (*gin.Engine, *storage.Storage, *authz.FakeClock) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if err := mainDB.AutoMigrate(&model.WeKnoraAuthToken{}); err != nil {
		t.Fatalf("migrate main schema: %v", err)
	}
	if err := stateDB.AutoMigrate(
		&model.AuditEvent{},
		&model.IdentityLink{},
		&model.ManagedEntitlement{},
		&model.RuntimeGrant{},
		&model.AuthzEpoch{},
		&model.Session{}, // revoke 同事务撤会话族（H3）
	); err != nil {
		t.Fatalf("migrate state schema: %v", err)
	}
	s := storage.NewStorageWithDBs(mainDB, stateDB)
	clock := authz.NewFakeClock(internalTestBase)

	cfg := &config.Config{
		WeCom: config.WeComConfig{CorpID: testCorpID},
		Admin: config.AdminConfig{Key: testAdminKey},
		Auth:  config.AuthConfig{Key: testAuthKey},
	}
	// 默认注入 FakeClock 与 P09 兼容桥作为上游 token 撤销器（生产同构，见 main.go）；
	// 显式 opts 在前，可被调用方覆盖顺序（WithClock 需最先）。
	testBridge := kbridge.New(
		kbridge.Config{SignKey: "test-bridge-key"},
		s,
		authz.NewEntitlementChecker(s, nil),
		kbridge.NewWeKnoraStore(s),
	)
	allOpts := append([]InternalOption{WithClock(clock), WithTokenRevoker(testBridge)}, opts...)
	ih := NewInternalHandler(cfg, s, allOpts...)

	r := gin.New()
	internal := r.Group("/integration/v1/internal", ih.RequireServiceToken)
	{
		internal.POST("/grants", ih.IssueGrant)
		internal.POST("/grants/:id/renew", ih.RenewGrant)
		internal.GET("/authz", ih.AuthzView)
		internal.POST("/authz", ih.AuthzBatch)
		internal.POST("/revoke", ih.Revoke)
	}
	return r, s, clock
}

// internalServiceHeaders 生成 system_admin 服务 token 请求头。
func internalServiceHeaders(t *testing.T) map[string]string {
	t.Helper()
	tok, err := adminauth.IssueToken([]byte(testAdminKey), "svc-internal-test", adminauth.RoleSystemAdmin, 0, time.Hour)
	if err != nil {
		t.Fatalf("issue service token: %v", err)
	}
	return map[string]string{"Authorization": "Bearer " + tok}
}

// internalDeptAdminHeaders 生成 dept_admin 服务 token（角色不足）。
func internalDeptAdminHeaders(t *testing.T) map[string]string {
	t.Helper()
	tok, err := adminauth.IssueToken([]byte(testAdminKey), "svc-dept", adminauth.RoleDeptAdmin, 1001, time.Hour)
	if err != nil {
		t.Fatalf("issue dept token: %v", err)
	}
	return map[string]string{"Authorization": "Bearer " + tok}
}

// provisionSubject 供给主体：active identity link + active entitlement。
func provisionSubject(t *testing.T, s *storage.Storage, subject, knowledgeUserID, tenant, capability string) {
	t.Helper()
	insertIdentityLink(t, s, subject, knowledgeUserID)
	insertEntitlement(t, s, subject, tenant, capability, 0, false)
}

// issueGrantOK 走通签发并断言成功，返回 grant_id 与 lease_token。
func issueGrantOK(t *testing.T, r *gin.Engine, headers map[string]string, subject, tenant string) (grantID, leaseToken string, expiresAt time.Time) {
	t.Helper()
	w := doPostJSON(t, r, "/integration/v1/internal/grants", gin.H{
		"subject":             subject,
		"tenant_id":           tenant,
		"task_id":             "task-1",
		"workspace_id":        "ws-1",
		"runtime_id":          "rt-1",
		"originating_user_id": "op-1",
		"capabilities":        []string{"chat"},
	}, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("issue grant = %d, body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	grantID, _ = body["grant_id"].(string)
	leaseToken, _ = body["lease_token"].(string)
	expRaw, _ := body["lease_expires_at"].(string)
	parsed, err := time.Parse(time.RFC3339Nano, expRaw)
	if err != nil {
		t.Fatalf("parse lease_expires_at %q: %v", expRaw, err)
	}
	if grantID == "" || leaseToken == "" {
		t.Fatalf("grant response missing grant_id/lease_token: %v", body)
	}
	return grantID, leaseToken, parsed
}

// parseLeaseToken 校验 lease_token 签名并返回 claims（过期由用例按注入时钟自行断言）。
func parseLeaseToken(t *testing.T, raw string) jwt.MapClaims {
	t.Helper()
	claims := jwt.MapClaims{}
	parser := jwt.NewParser(jwt.WithValidMethods([]string{"HS256"}), jwt.WithoutClaimsValidation())
	token, err := parser.ParseWithClaims(raw, claims, func(tok *jwt.Token) (interface{}, error) {
		if _, ok := tok.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return []byte(testAuthKey), nil
	})
	if err != nil || !token.Valid {
		t.Fatalf("parse lease token: %v", err)
	}
	return claims
}

// auditEventsByAction 按 action 查审计事件。
func auditEventsByAction(t *testing.T, s *storage.Storage, action string) []model.AuditEvent {
	t.Helper()
	var events []model.AuditEvent
	if err := s.StateDB().WithContext(context.Background()).
		Where("action = ?", action).
		Order("created_at ASC").
		Find(&events).Error; err != nil {
		t.Fatalf("query audit events: %v", err)
	}
	return events
}

// ---------- 服务 token 中间件 ----------

// TestInternalServiceToken 覆盖缺 token / 非法 token / 角色不足三种拒绝。
func TestInternalServiceToken(t *testing.T) {
	r, s, _ := setupInternalTest(t)
	provisionSubject(t, s, "subj-1", "ku-1", "t1", "chat")

	w := doPostJSON(t, r, "/integration/v1/internal/grants", gin.H{
		"subject": "subj-1", "tenant_id": "t1", "originating_user_id": "op-1",
	}, nil)
	if w.Code != http.StatusUnauthorized || errorCode(t, w) != errCodeSessionRevoked {
		t.Fatalf("missing token = %d/%s, want 401/%s", w.Code, errorCode(t, w), errCodeSessionRevoked)
	}

	w = doPostJSON(t, r, "/integration/v1/internal/grants", gin.H{
		"subject": "subj-1", "tenant_id": "t1", "originating_user_id": "op-1",
	}, map[string]string{"Authorization": "Bearer not-a-jwt"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token = %d, want 401", w.Code)
	}

	w = doPostJSON(t, r, "/integration/v1/internal/grants", gin.H{
		"subject": "subj-1", "tenant_id": "t1", "originating_user_id": "op-1",
	}, internalDeptAdminHeaders(t))
	if w.Code != http.StatusForbidden {
		t.Fatalf("dept_admin token = %d, want 403", w.Code)
	}
}

// ---------- POST /internal/grants ----------

func TestInternalIssueGrant_NoEntitlement(t *testing.T) {
	r, s, _ := setupInternalTest(t)
	// 只有 active link、无 entitlement → 403 ENTITLEMENT_REVOKED
	insertIdentityLink(t, s, "subj-1", "ku-1")
	w := doPostJSON(t, r, "/integration/v1/internal/grants", gin.H{
		"subject": "subj-1", "tenant_id": "t1", "originating_user_id": "op-1",
	}, internalServiceHeaders(t))
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeEntitlementRevoked {
		t.Fatalf("no entitlement = %d/%s, want 403/%s", w.Code, errorCode(t, w), errCodeEntitlementRevoked)
	}

	// 无 active link（未供给）→ 403 ENTITLEMENT_REVOKED
	w = doPostJSON(t, r, "/integration/v1/internal/grants", gin.H{
		"subject": "ghost", "tenant_id": "t1", "originating_user_id": "op-1",
	}, internalServiceHeaders(t))
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeEntitlementRevoked {
		t.Fatalf("ghost subject = %d/%s, want 403/%s", w.Code, errorCode(t, w), errCodeEntitlementRevoked)
	}

	if n := len(auditEventsByAction(t, s, "integration.internal.grant.denied")); n != 2 {
		t.Fatalf("grant.denied audit events = %d, want 2", n)
	}
}

func TestInternalIssueGrant_MissingFields(t *testing.T) {
	r, _, _ := setupInternalTest(t)
	headers := internalServiceHeaders(t)

	w := doPostJSON(t, r, "/integration/v1/internal/grants", gin.H{
		"subject": "subj-1", "tenant_id": "t1", // 缺 originating_user_id
	}, headers)
	if w.Code != http.StatusBadRequest || errorCode(t, w) != errCodeInvalidRequest {
		t.Fatalf("missing originating_user_id = %d/%s, want 400/%s", w.Code, errorCode(t, w), errCodeInvalidRequest)
	}

	w = doPostJSON(t, r, "/integration/v1/internal/grants", gin.H{
		"subject": "subj-1", "originating_user_id": "op-1", // 缺 tenant_id
	}, headers)
	if w.Code != http.StatusBadRequest || errorCode(t, w) != errCodeInvalidRequest {
		t.Fatalf("missing tenant_id = %d/%s, want 400/%s", w.Code, errorCode(t, w), errCodeInvalidRequest)
	}
}

func TestInternalIssueGrant_Success(t *testing.T) {
	r, s, clock := setupInternalTest(t)
	provisionSubject(t, s, "subj-1", "ku-1", "t1", "chat")

	grantID, leaseToken, expiresAt := issueGrantOK(t, r, internalServiceHeaders(t), "subj-1", "t1")
	wantExpiry := clock.Now().Add(30 * time.Second)
	if !expiresAt.Equal(wantExpiry) {
		t.Fatalf("lease_expires_at = %v, want %v (now+30s)", expiresAt, wantExpiry)
	}

	claims := parseLeaseToken(t, leaseToken)
	if claims["type"] != "task_lease" || claims["sub"] != "subj-1" || claims["tenant_id"] != "t1" ||
		claims["grant_id"] != grantID || claims["task_id"] != "task-1" || claims["runtime_id"] != "rt-1" {
		t.Fatalf("lease claims mismatch: %v", claims)
	}
	if exp, _ := claims["exp"].(float64); int64(exp) != wantExpiry.Unix() {
		t.Fatalf("lease exp = %v, want %d", exp, wantExpiry.Unix())
	}

	grant, err := s.GetRuntimeGrant(context.Background(), grantID)
	if err != nil {
		t.Fatalf("GetRuntimeGrant: %v", err)
	}
	if !grant.LeaseExpiresAt.Equal(wantExpiry) || grant.AuthzVersion != 0 || grant.OriginatingUserID != "op-1" {
		t.Fatalf("grant row mismatch: %+v", grant)
	}

	if n := len(auditEventsByAction(t, s, "integration.internal.grant.issued")); n != 1 {
		t.Fatalf("grant.issued audit events = %d, want 1", n)
	}
}

// ---------- POST /internal/grants/:id/renew ----------

func TestInternalRenew_GraceWindow(t *testing.T) {
	r, s, clock := setupInternalTest(t)
	provisionSubject(t, s, "subj-1", "ku-1", "t1", "chat")
	headers := internalServiceHeaders(t)
	grantID, _, _ := issueGrantOK(t, r, headers, "subj-1", "t1")

	// t0+31s：租约名义过期（t0+30），但仍在 15s 宽容窗内 → 续期成功
	clock.Advance(31 * time.Second)
	w := doPostJSON(t, r, "/integration/v1/internal/grants/"+grantID+"/renew", gin.H{}, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("renew within grace = %d, body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	expRaw, _ := body["lease_expires_at"].(string)
	parsed, err := time.Parse(time.RFC3339Nano, expRaw)
	if err != nil {
		t.Fatalf("parse lease_expires_at: %v", err)
	}
	if want := clock.Now().Add(30 * time.Second); !parsed.Equal(want) {
		t.Fatalf("renewed expiry = %v, want %v", parsed, want)
	}

	// 新租约到期 t0+61；推进到 t0+77（宽容窗外）→ 401 LEASE_EXPIRED
	clock.Advance(46 * time.Second)
	w = doPostJSON(t, r, "/integration/v1/internal/grants/"+grantID+"/renew", gin.H{}, headers)
	if w.Code != http.StatusUnauthorized || errorCode(t, w) != errCodeLeaseExpired {
		t.Fatalf("renew beyond grace = %d/%s, want 401/%s", w.Code, errorCode(t, w), errCodeLeaseExpired)
	}
}

func TestInternalRenew_GrantNotFound(t *testing.T) {
	r, _, _ := setupInternalTest(t)
	w := doPostJSON(t, r, "/integration/v1/internal/grants/"+"does-not-exist"+"/renew", gin.H{}, internalServiceHeaders(t))
	if w.Code != http.StatusNotFound || errorCode(t, w) != errCodeGrantNotFound {
		t.Fatalf("renew unknown grant = %d/%s, want 404/%s", w.Code, errorCode(t, w), errCodeGrantNotFound)
	}
}

func TestInternalRenew_RevokedSubjectDenied(t *testing.T) {
	r, s, _ := setupInternalTest(t, WithVersionCacheTTL(5*time.Second))
	provisionSubject(t, s, "subj-1", "ku-1", "t1", "chat")
	headers := internalServiceHeaders(t)
	grantID, _, _ := issueGrantOK(t, r, headers, "subj-1", "t1")

	// revoke 事务提交后立即拒绝续期（即使版本读配了 5s 缓存，续期也走无缓存直读）
	w := doPostJSON(t, r, "/integration/v1/internal/revoke", gin.H{
		"subject": "subj-1", "reason": "offboard",
	}, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke = %d, body=%s", w.Code, w.Body.String())
	}

	w = doPostJSON(t, r, "/integration/v1/internal/grants/"+grantID+"/renew", gin.H{}, headers)
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeEntitlementRevoked {
		t.Fatalf("renew after revoke = %d/%s, want 403/%s", w.Code, errorCode(t, w), errCodeEntitlementRevoked)
	}

	grant, err := s.GetRuntimeGrant(context.Background(), grantID)
	if err != nil {
		t.Fatalf("GetRuntimeGrant: %v", err)
	}
	if grant.RevokedAt == nil {
		t.Fatalf("grant revoked_at not persisted after denied renew: %+v", grant)
	}

	// 新 grant 签发同样立即拒绝
	w = doPostJSON(t, r, "/integration/v1/internal/grants", gin.H{
		"subject": "subj-1", "tenant_id": "t1", "originating_user_id": "op-1",
	}, headers)
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeEntitlementRevoked {
		t.Fatalf("issue after revoke = %d/%s, want 403/%s", w.Code, errorCode(t, w), errCodeEntitlementRevoked)
	}
}

// TestInternalRenew_ConcurrentSingleWinner 并发续期单赢（race）。
// 真实时钟 + 闸门对齐：全部 racer 完成读校验后同时进入原子 UPDATE，
// 条件 UPDATE（旧到期值比对）保证恰好一个 200，其余 409 LEASE_CONFLICT。
func TestInternalRenew_ConcurrentSingleWinner(t *testing.T) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", filepath.Join(t.TempDir(), "state.db"))
	stateDB, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open state sqlite: %v", err)
	}
	mainDB := openIntegrationTestSQLite(t)

	const racers = 8
	release := make(chan struct{})
	arrivedCh := make(chan struct{}, racers)
	gate := func() {
		arrivedCh <- struct{}{} // 通知测试：本 racer 已完成读校验
		<-release               // 等全员到齐后统一放行原子 UPDATE
	}
	// 真实时钟：赢家写入的新到期值与旧值必然不同（冻结时钟下两者相等，
	// 条件 UPDATE 无法区分单赢）。闸门保证全部 racer 先完成读校验再并发 UPDATE。
	r, s, _ := setupInternalTestWithDBs(t, mainDB, stateDB,
		WithClock(authz.RealClock{}), withRenewGate(gate))
	provisionSubject(t, s, "subj-1", "ku-1", "t1", "chat")

	headers := internalServiceHeaders(t)
	grantID, _, _ := issueGrantOK(t, r, headers, "subj-1", "t1")

	server := httptest.NewServer(r)
	t.Cleanup(server.Close)

	client := &http.Client{Timeout: 10 * time.Second}
	var wg sync.WaitGroup
	statuses := make(chan int, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, server.URL+"/integration/v1/internal/grants/"+grantID+"/renew", nil)
			if err != nil {
				statuses <- -1
				return
			}
			req.Header.Set("Authorization", headers["Authorization"])
			resp, err := client.Do(req)
			if err != nil {
				statuses <- -2
				return
			}
			_ = resp.Body.Close()
			statuses <- resp.StatusCode
		}()
	}

	// 等全员到达闸门后统一放行（超时保护）
	for i := 0; i < racers; i++ {
		select {
		case <-arrivedCh:
		case <-time.After(10 * time.Second):
			close(release)
			wg.Wait()
			t.Fatal("racers did not reach renew gate in time")
		}
	}
	close(release)
	wg.Wait()

	wins, conflicts := 0, 0
	for i := 0; i < racers; i++ {
		switch code := <-statuses; code {
		case http.StatusOK:
			wins++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("unexpected renew status: %d", code)
		}
	}
	if wins != 1 || conflicts != racers-1 {
		t.Fatalf("wins=%d conflicts=%d, want 1/%d (single winner)", wins, conflicts, racers-1)
	}
}

// ---------- GET/POST /internal/authz ----------

func TestInternalAuthz_View(t *testing.T) {
	r, s, _ := setupInternalTest(t)
	provisionSubject(t, s, "subj-1", "ku-1", "t1", "chat")
	insertEntitlement(t, s, "subj-1", "t2", "docs", 0, false)
	headers := internalServiceHeaders(t)

	w := doGet(t, r, "/integration/v1/internal/authz?subject=subj-1", headers)
	if w.Code != http.StatusOK {
		t.Fatalf("authz view = %d, body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	view, ok := body["subj-1"].(map[string]interface{})
	if !ok {
		t.Fatalf("authz view missing subject key: %v", body)
	}
	if view["revoked"] != false {
		t.Fatalf("revoked = %v, want false", view["revoked"])
	}
	if view["authz_version"] != float64(0) {
		t.Fatalf("authz_version = %v, want 0", view["authz_version"])
	}
	caps, _ := view["entitlements"].([]interface{})
	if len(caps) != 2 {
		t.Fatalf("entitlements = %v, want [chat docs]", caps)
	}

	// tenant 过滤
	w = doGet(t, r, "/integration/v1/internal/authz?subject=subj-1&tenant=t1", headers)
	body = decodeBody(t, w)
	view = body["subj-1"].(map[string]interface{})
	caps, _ = view["entitlements"].([]interface{})
	if len(caps) != 1 || caps[0] != "chat" {
		t.Fatalf("tenant-filtered entitlements = %v, want [chat]", caps)
	}

	// 缺 subject → 400
	w = doGet(t, r, "/integration/v1/internal/authz", headers)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("authz without subject = %d, want 400", w.Code)
	}
}

func TestInternalAuthz_Batch(t *testing.T) {
	r, s, _ := setupInternalTest(t)
	provisionSubject(t, s, "subj-1", "ku-1", "t1", "chat")
	headers := internalServiceHeaders(t)

	w := doPostJSON(t, r, "/integration/v1/internal/authz", gin.H{
		"subjects": []string{"subj-1", "ghost"},
	}, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("authz batch = %d, body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	active, ok := body["subj-1"].(map[string]interface{})
	if !ok || active["revoked"] != false {
		t.Fatalf("subj-1 view = %v", body["subj-1"])
	}
	ghost, ok := body["ghost"].(map[string]interface{})
	if !ok || ghost["revoked"] != true {
		t.Fatalf("ghost view = %v, want revoked=true", body["ghost"])
	}

	w = doPostJSON(t, r, "/integration/v1/internal/authz", gin.H{}, headers)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty batch = %d, want 400", w.Code)
	}
}

// ---------- POST /internal/revoke ----------

// H3：/internal/revoke 在同事务内把该 subject 的 sessions 族全部撤销
// （revoke_reason=revoke）；其他主体的会话不受影响。
func TestInternalRevoke_SessionFamilySwept(t *testing.T) {
	r, s, _ := setupInternalTest(t)
	link := insertIdentityLink(t, s, "subj-1", "ku-1")
	other := insertIdentityLink(t, s, "subj-2", "ku-2")
	insertEntitlement(t, s, "subj-1", "t1", "chat", 0, false)
	headers := internalServiceHeaders(t)

	now := time.Now()
	for _, fam := range []string{"fam-1", "fam-2"} {
		sess := &model.Session{
			SessionID: "sess-" + fam, IdentityLinkID: link.ID, FamilyID: fam,
			RefreshVerifierHash: "hash-" + fam, DeviceFingerprint: "dev", ExpiresAt: now.Add(time.Hour),
		}
		if err := s.InsertSession(context.Background(), sess); err != nil {
			t.Fatalf("insert session: %v", err)
		}
	}
	otherSess := &model.Session{
		SessionID: "sess-other", IdentityLinkID: other.ID, FamilyID: "fam-other",
		RefreshVerifierHash: "hash-other", DeviceFingerprint: "dev", ExpiresAt: now.Add(time.Hour),
	}
	if err := s.InsertSession(context.Background(), otherSess); err != nil {
		t.Fatalf("insert other session: %v", err)
	}

	w := doPostJSON(t, r, "/integration/v1/internal/revoke", gin.H{
		"subject": "subj-1", "reason": "offboard",
	}, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke = %d, body=%s", w.Code, w.Body.String())
	}

	// 该 subject 的全部会话同事务撤销，reason=revoke
	for _, sid := range []string{"sess-fam-1", "sess-fam-2"} {
		sess, err := s.GetSession(context.Background(), sid)
		if err != nil {
			t.Fatalf("get session %s: %v", sid, err)
		}
		if sess.RevokedAt == nil || sess.RevokeReason == nil || *sess.RevokeReason != "revoke" {
			t.Fatalf("session %s not revoked with reason=revoke: %+v", sid, sess)
		}
	}
	// 其他主体会话不受影响
	sess, err := s.GetSession(context.Background(), "sess-other")
	if err != nil {
		t.Fatalf("get other session: %v", err)
	}
	if sess.RevokedAt != nil {
		t.Fatalf("unrelated session revoked: %+v", sess)
	}
}

// L7：tenant 级撤权只删该 (user, tenant) 的上游 auth_tokens 行——
// claims 可解析的按 tenant_id 过滤；claims 无法解析的只删审计可定位的桥发行短寿行。
func TestInternalRevoke_TenantScopeDeletesOnlyThatTenantTokens(t *testing.T) {
	r, s, _ := setupInternalTest(t)
	provisionSubject(t, s, "subj-1", "ku-1", "10001", "chat")
	insertEntitlement(t, s, "subj-1", "10002", "docs", 0, false)
	headers := internalServiceHeaders(t)

	signUpstream := func(tenantID uint64) string {
		claims := jwt.MapClaims{
			"user_id": "ku-1", "tenant_id": tenantID, "type": "access",
			"exp": time.Now().Add(2 * time.Minute).Unix(), "iat": time.Now().Unix(),
		}
		raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("test-bridge-key"))
		if err != nil {
			t.Fatalf("sign upstream token: %v", err)
		}
		return raw
	}
	now := time.Now()
	seedToken := func(id, token string, ttl time.Duration) {
		rec := &model.WeKnoraAuthToken{
			ID: id, UserID: "ku-1", Token: token, TokenType: "access_token",
			ExpiresAt: now.Add(ttl), CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAuthToken(context.Background(), rec); err != nil {
			t.Fatalf("CreateAuthToken: %v", err)
		}
	}
	seedToken("tok-t10001", signUpstream(10001), 2*time.Minute)  // claims 可解析，本 tenant → 删
	seedToken("tok-t10002", signUpstream(10002), 2*time.Minute)  // claims 可解析，其他 tenant → 留
	seedToken("tok-legacy", "not-a-jwt-legacy", 24*time.Hour)    // 不可解析且无审计 → 留
	seedToken("tok-bridged", "not-a-jwt-bridged", 2*time.Minute) // 不可解析但审计可定位 → 删
	if err := s.InsertAuditEvent(context.Background(), &model.AuditEvent{
		Actor: "subj-1", Action: "kbridge.token.issued", Target: "knowledge upstream token",
		After: `{"result":"allowed","auth_token_id":"tok-bridged","knowledge_user_id":"ku-1","tenant_id":"10001"}`,
	}); err != nil {
		t.Fatalf("insert bridge audit: %v", err)
	}

	w := doPostJSON(t, r, "/integration/v1/internal/revoke", gin.H{
		"tenant_id": "10001", "reason": "tenant offboard",
	}, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("tenant revoke = %d, body=%s", w.Code, w.Body.String())
	}

	remaining := map[string]bool{}
	var rows []model.WeKnoraAuthToken
	if err := s.DB().WithContext(context.Background()).Find(&rows).Error; err != nil {
		t.Fatalf("list auth tokens: %v", err)
	}
	for _, row := range rows {
		remaining[row.ID] = true
	}
	if remaining["tok-t10001"] {
		t.Fatal("tenant-10001 token should be deleted (claims match)")
	}
	if remaining["tok-bridged"] {
		t.Fatal("audit-correlated bridge token should be deleted (tenant 10001)")
	}
	if !remaining["tok-t10002"] {
		t.Fatal("tenant-10002 token must survive tenant-10001 revoke")
	}
	if !remaining["tok-legacy"] {
		t.Fatal("unattributed legacy token must survive tenant-scoped revoke")
	}
}

func TestInternalRevoke_SubjectDeletesUpstreamTokens(t *testing.T) {
	r, s, _ := setupInternalTest(t)
	provisionSubject(t, s, "subj-1", "ku-1", "t1", "chat")
	provisionSubject(t, s, "subj-2", "ku-2", "t1", "chat")
	headers := internalServiceHeaders(t)

	// 上游 auth_tokens：ku-1 两条、ku-2 一条
	for _, row := range []struct{ user, token string }{
		{"ku-1", "tok-a"}, {"ku-1", "tok-b"}, {"ku-2", "tok-c"},
	} {
		rec := &model.WeKnoraAuthToken{
			ID:        row.token,
			UserID:    row.user,
			Token:     row.token,
			TokenType: "access_token",
			ExpiresAt: internalTestBase.Add(time.Hour),
		}
		if err := s.CreateAuthToken(context.Background(), rec); err != nil {
			t.Fatalf("CreateAuthToken: %v", err)
		}
	}

	w := doPostJSON(t, r, "/integration/v1/internal/revoke", gin.H{
		"subject": "subj-1", "reason": "offboard",
	}, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke = %d, body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if _, ok := body["applied_at"].(string); !ok {
		t.Fatalf("revoke response missing applied_at: %v", body)
	}

	// 钩子生效：ku-1 token 全删，ku-2 不动
	var remaining int64
	s.DB().WithContext(context.Background()).Model(&model.WeKnoraAuthToken{}).
		Where("user_id = ?", "ku-1").Count(&remaining)
	if remaining != 0 {
		t.Fatalf("ku-1 tokens remaining = %d, want 0", remaining)
	}
	s.DB().WithContext(context.Background()).Model(&model.WeKnoraAuthToken{}).
		Where("user_id = ?", "ku-2").Count(&remaining)
	if remaining != 1 {
		t.Fatalf("ku-2 tokens remaining = %d, want 1", remaining)
	}

	// 审计：token_revoked + revoke.applied
	if n := len(auditEventsByAction(t, s, "integration.internal.revoke.token_revoked")); n != 1 {
		t.Fatalf("token_revoked audit events = %d, want 1", n)
	}
	if n := len(auditEventsByAction(t, s, "integration.internal.revoke.applied")); n != 1 {
		t.Fatalf("revoke.applied audit events = %d, want 1", n)
	}

	// 撤权后授权视图：revoked=true 全量
	w = doGet(t, r, "/integration/v1/internal/authz?subject=subj-1", headers)
	view := decodeBody(t, w)["subj-1"].(map[string]interface{})
	if view["revoked"] != true {
		t.Fatalf("authz view after revoke = %v, want revoked=true", view)
	}
	if view["authz_version"] != float64(1) {
		t.Fatalf("authz_version after revoke = %v, want 1 (bumped)", view["authz_version"])
	}
	caps, _ := view["entitlements"].([]interface{})
	if len(caps) != 0 {
		t.Fatalf("entitlements after revoke = %v, want empty", caps)
	}
}

func TestInternalRevoke_Idempotent(t *testing.T) {
	r, s, _ := setupInternalTest(t)
	provisionSubject(t, s, "subj-1", "ku-1", "t1", "chat")
	headers := internalServiceHeaders(t)

	for i := 0; i < 2; i++ {
		w := doPostJSON(t, r, "/integration/v1/internal/revoke", gin.H{
			"subject": "subj-1", "reason": "offboard",
		}, headers)
		if w.Code != http.StatusOK {
			t.Fatalf("revoke #%d = %d, body=%s", i+1, w.Code, w.Body.String())
		}
	}

	// 第二次调用幂等：版本不再次提升
	w := doGet(t, r, "/integration/v1/internal/authz?subject=subj-1", headers)
	view := decodeBody(t, w)["subj-1"].(map[string]interface{})
	if view["authz_version"] != float64(1) {
		t.Fatalf("authz_version after double revoke = %v, want 1 (no re-bump)", view["authz_version"])
	}

	// 未知 subject 同样幂等成功
	w = doPostJSON(t, r, "/integration/v1/internal/revoke", gin.H{
		"subject": "ghost", "reason": "offboard",
	}, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke ghost = %d, want 200 (idempotent no-op)", w.Code)
	}
}

func TestInternalRevoke_TenantScope(t *testing.T) {
	r, s, _ := setupInternalTest(t)
	provisionSubject(t, s, "subj-1", "ku-1", "t1", "chat")
	insertEntitlement(t, s, "subj-1", "t2", "docs", 0, false)
	headers := internalServiceHeaders(t)

	w := doPostJSON(t, r, "/integration/v1/internal/revoke", gin.H{
		"tenant_id": "t1", "reason": "tenant offboard",
	}, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("tenant revoke = %d, body=%s", w.Code, w.Body.String())
	}

	// t1 授权被撤：新 grant 拒绝；t2 不受影响
	w = doPostJSON(t, r, "/integration/v1/internal/grants", gin.H{
		"subject": "subj-1", "tenant_id": "t1", "originating_user_id": "op-1",
	}, headers)
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeEntitlementRevoked {
		t.Fatalf("grant in revoked tenant = %d/%s, want 403/%s", w.Code, errorCode(t, w), errCodeEntitlementRevoked)
	}
	grantID, _, _ := issueGrantOK(t, r, headers, "subj-1", "t2")

	// 身份未停用：revoked=false，t1 capabilities 清空、t2 保留
	w = doGet(t, r, "/integration/v1/internal/authz?subject=subj-1", headers)
	view := decodeBody(t, w)["subj-1"].(map[string]interface{})
	if view["revoked"] != false {
		t.Fatalf("revoked = %v, want false (identity untouched)", view["revoked"])
	}
	if _, err := s.GetRuntimeGrant(context.Background(), grantID); err != nil {
		t.Fatalf("t2 grant row missing: %v", err)
	}
}

func TestInternalRevoke_LinkSelector(t *testing.T) {
	r, s, _ := setupInternalTest(t)
	link := insertIdentityLink(t, s, "subj-1", "ku-1")
	insertEntitlement(t, s, "subj-1", "t1", "chat", 0, false)
	headers := internalServiceHeaders(t)

	w := doPostJSON(t, r, "/integration/v1/internal/revoke", gin.H{
		"identity_link_id": link.ID, "reason": "link disabled",
	}, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("link revoke = %d, body=%s", w.Code, w.Body.String())
	}

	stored, err := s.GetIdentityLink(context.Background(), link.ID)
	if err != nil {
		t.Fatalf("GetIdentityLink: %v", err)
	}
	if stored.Status != "disabled" {
		t.Fatalf("link status = %s, want disabled", stored.Status)
	}

	// subject 停用 → authz 查询 revoked=true 全量
	w = doGet(t, r, "/integration/v1/internal/authz?subject=subj-1", headers)
	view := decodeBody(t, w)["subj-1"].(map[string]interface{})
	if view["revoked"] != true {
		t.Fatalf("authz view = %v, want revoked=true", view)
	}
}

// failingChannel 永远投递失败的通道（BND-110：通知失败不影响拒绝生效）。
type failingChannel struct{}

func (failingChannel) Name() string { return "failing" }
func (failingChannel) Notify(ctx context.Context, ev authz.InvalidateEvent) error {
	return errors.New("channel unreachable")
}

func TestInternalRevoke_NotificationFailureStillDenies(t *testing.T) {
	clock := authz.NewFakeClock(internalTestBase)
	notifier := authz.NewNotifier(clock)
	notifier.Register(failingChannel{})

	r, s, _ := setupInternalTest(t, WithClock(clock), WithNotifier(notifier))
	provisionSubject(t, s, "subj-1", "ku-1", "t1", "chat")
	headers := internalServiceHeaders(t)
	grantID, _, _ := issueGrantOK(t, r, headers, "subj-1", "t1")

	w := doPostJSON(t, r, "/integration/v1/internal/revoke", gin.H{
		"subject": "subj-1", "reason": "offboard",
	}, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke with failing channel = %d, body=%s", w.Code, w.Body.String())
	}

	// 失败事件进入 30s TTL 重试队列
	if n := notifier.PendingRetries(); n != 1 {
		t.Fatalf("PendingRetries = %d, want 1", n)
	}

	// 拒绝已生效：新 grant 与续期都被拒
	w = doPostJSON(t, r, "/integration/v1/internal/grants", gin.H{
		"subject": "subj-1", "tenant_id": "t1", "originating_user_id": "op-1",
	}, headers)
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeEntitlementRevoked {
		t.Fatalf("grant after failed-notify revoke = %d/%s, want 403/%s", w.Code, errorCode(t, w), errCodeEntitlementRevoked)
	}
	w = doPostJSON(t, r, "/integration/v1/internal/grants/"+grantID+"/renew", gin.H{}, headers)
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeEntitlementRevoked {
		t.Fatalf("renew after failed-notify revoke = %d/%s, want 403/%s", w.Code, errorCode(t, w), errCodeEntitlementRevoked)
	}
}

// failingRevoker 永远失败的 token 撤销器：revoke 主流程不失败，只记审计。
type failingRevoker struct{}

func (failingRevoker) RevokeTokens(ctx context.Context, knowledgeUserID string) (int64, error) {
	return 0, errors.New("upstream token store unreachable")
}

func TestInternalRevoke_TokenRevokeFailureAudited(t *testing.T) {
	r, s, _ := setupInternalTest(t, WithTokenRevoker(failingRevoker{}))
	provisionSubject(t, s, "subj-1", "ku-1", "t1", "chat")
	headers := internalServiceHeaders(t)

	rec := &model.WeKnoraAuthToken{
		ID: "tok-a", UserID: "ku-1", Token: "tok-a",
		TokenType: "access_token", ExpiresAt: internalTestBase.Add(time.Hour),
	}
	if err := s.CreateAuthToken(context.Background(), rec); err != nil {
		t.Fatalf("CreateAuthToken: %v", err)
	}

	w := doPostJSON(t, r, "/integration/v1/internal/revoke", gin.H{
		"subject": "subj-1", "reason": "offboard",
	}, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke with failing revoker = %d, want 200", w.Code)
	}

	if n := len(auditEventsByAction(t, s, "integration.internal.revoke.token_revoke.failed")); n != 1 {
		t.Fatalf("token_revoke.failed audit events = %d, want 1", n)
	}

	// 上游 token 未删（桥不可达），但拒绝已生效
	var remaining int64
	s.DB().WithContext(context.Background()).Model(&model.WeKnoraAuthToken{}).
		Where("user_id = ?", "ku-1").Count(&remaining)
	if remaining != 1 {
		t.Fatalf("ku-1 tokens remaining = %d, want 1 (revoker failed)", remaining)
	}
	w = doPostJSON(t, r, "/integration/v1/internal/grants", gin.H{
		"subject": "subj-1", "tenant_id": "t1", "originating_user_id": "op-1",
	}, headers)
	if w.Code != http.StatusForbidden {
		t.Fatalf("grant after revoker failure = %d, want 403", w.Code)
	}
}

func TestInternalRevoke_SelectorValidation(t *testing.T) {
	r, _, _ := setupInternalTest(t)
	headers := internalServiceHeaders(t)

	w := doPostJSON(t, r, "/integration/v1/internal/revoke", gin.H{"reason": "x"}, headers)
	if w.Code != http.StatusBadRequest || errorCode(t, w) != errCodeInvalidRequest {
		t.Fatalf("no selector = %d/%s, want 400/%s", w.Code, errorCode(t, w), errCodeInvalidRequest)
	}

	w = doPostJSON(t, r, "/integration/v1/internal/revoke", gin.H{
		"subject": "a", "tenant_id": "t1",
	}, headers)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("double selector = %d, want 400", w.Code)
	}
}
