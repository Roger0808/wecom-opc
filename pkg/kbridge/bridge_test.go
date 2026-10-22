// TASK-P09 契约自检测试（ADR-004：对固定契约验证 token 持久化/停用/撤销）：
//   - 桥签发 token 与 pkg/auth/service.go 既有 WeKnora 签发同 claims 集；
//   - 用既有 WeKnora JWT secret 验签往返 + ValidateToken「行存在」语义；
//   - 签发条件：停用 link / 无 entitlement / 管理员 / 停用用户 / 非法 TTL 全部拒绝；
//   - 撤权删除（TokenRevoker）与 Notifier 通道投递。
package kbridge

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/auth"
	"weknora-tokendance/pkg/authz"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"

	"github.com/glebarez/sqlite"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	testWeKnoraJWTSecret = "kbridge-test-weknora-jwt-secret"
)

// setupBridgeTest 装配 sqlite 双库 + 桥。桥签名密钥按生产语义注入为与
// WeKnora JWT secret 同值（生产 Bridge.Key 与 weknora.jwt_secret 同源但独立
// 配置注入），契约用例即以此验证 WeKnora secret 验签往返。
func setupBridgeTest(t *testing.T) (*storage.Storage, *Bridge) {
	t.Helper()
	dsn := func() string { return fmt.Sprintf("file:%s?mode=memory&cache=shared", uuid.NewString()) }
	open := func() *gorm.DB {
		db, err := gorm.Open(sqlite.Open(dsn()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		sqlDB, _ := db.DB()
		sqlDB.SetMaxOpenConns(1)
		return db
	}
	mainDB, stateDB := open(), open()
	if err := mainDB.AutoMigrate(&model.WeKnoraUser{}, &model.WeKnoraTenantMember{}, &model.WeKnoraAuthToken{}); err != nil {
		t.Fatalf("migrate main schema: %v", err)
	}
	if err := stateDB.AutoMigrate(&model.IdentityLink{}, &model.ManagedEntitlement{}, &model.AuditEvent{}); err != nil {
		t.Fatalf("migrate state schema: %v", err)
	}
	s := storage.NewStorageWithDBs(mainDB, stateDB)
	bridge := New(
		Config{SignKey: testWeKnoraJWTSecret, Audit: s.InsertAuditEvent},
		s,
		authz.NewEntitlementChecker(s, nil),
		NewWeKnoraStore(s),
	)
	return s, bridge
}

func insertLink(t *testing.T, s *storage.Storage, subject, knowledgeUserID, status string) *model.IdentityLink {
	t.Helper()
	link := &model.IdentityLink{
		IdentitySource:   "wecom",
		OrganizationID:   "corp-test",
		ImmutableSubject: subject,
		KnowledgeUserID:  knowledgeUserID,
		Status:           status,
	}
	if err := s.InsertIdentityLink(context.Background(), link); err != nil {
		t.Fatalf("InsertIdentityLink: %v", err)
	}
	return link
}

func insertEntitlement(t *testing.T, s *storage.Storage, subject, tenant string) {
	t.Helper()
	ent := &model.ManagedEntitlement{Subject: subject, TenantID: tenant, Capability: "chat", Source: "sync"}
	if err := s.InsertManagedEntitlement(context.Background(), ent); err != nil {
		t.Fatalf("InsertManagedEntitlement: %v", err)
	}
}

func seedUpstreamUser(t *testing.T, s *storage.Storage, id, email string, active, systemAdmin bool) {
	t.Helper()
	// 用 map 写入：WeKnoraUser.IsActive 带 default:true tag，struct 创建时
	// false 会被 gorm 视为零值跳过而落库默认 true。
	values := map[string]interface{}{
		"id":              id,
		"username":        id,
		"email":           email,
		"password_hash":   "",
		"tenant_id":       1001,
		"is_active":       active,
		"is_system_admin": systemAdmin,
		"preferences":     "{}",
	}
	if err := s.DB().WithContext(context.Background()).Model(&model.WeKnoraUser{}).Create(values).Error; err != nil {
		t.Fatalf("create upstream user: %v", err)
	}
}

func seedMembership(t *testing.T, s *storage.Storage, userID string, tenantID uint64, status string) {
	t.Helper()
	m := &model.WeKnoraTenantMember{UserID: userID, TenantID: tenantID, Role: "contributor", Status: status}
	if err := s.DB().WithContext(context.Background()).Create(m).Error; err != nil {
		t.Fatalf("create membership: %v", err)
	}
}

func parseWithSecret(t *testing.T, raw, secret string) jwt.MapClaims {
	t.Helper()
	claims := jwt.MapClaims{}
	token, err := jwt.ParseWithClaims(raw, claims, func(tok *jwt.Token) (interface{}, error) {
		if _, ok := tok.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil || !token.Valid {
		t.Fatalf("parse token with secret: %v", err)
	}
	return claims
}

// TestBridge_IssueUpstreamToken 全条件签发：claims 结构、行持久化、审计关联。
func TestBridge_IssueUpstreamToken(t *testing.T) {
	s, bridge := setupBridgeTest(t)
	ctx := context.Background()

	link := insertLink(t, s, "subj-1", "ku-1", "active")
	insertEntitlement(t, s, "subj-1", "1001")
	seedUpstreamUser(t, s, "ku-1", "user@example.com", true, false)
	seedMembership(t, s, "ku-1", 1001, "active")

	token, err := bridge.IssueUpstreamToken(ctx, link.ID, "1001", 60*time.Second)
	if err != nil {
		t.Fatalf("IssueUpstreamToken: %v", err)
	}

	// 契约：用 WeKnora JWT secret 验签往返（生产 Bridge.Key 与之一致）。
	claims := parseWithSecret(t, token, testWeKnoraJWTSecret)
	if claims["user_id"] != "ku-1" || claims["email"] != "user@example.com" {
		t.Fatalf("claims user mismatch: %v", claims)
	}
	if claims["type"] != "access" {
		t.Fatalf("claims type = %v, want access", claims["type"])
	}
	if tenant, ok := claims["tenant_id"].(float64); !ok || tenant != 1001 {
		t.Fatalf("claims tenant_id = %v, want 1001", claims["tenant_id"])
	}
	exp, iat := int64(claims["exp"].(float64)), int64(claims["iat"].(float64))
	if exp-iat > 120 || exp-iat <= 0 {
		t.Fatalf("token ttl = %ds, want (0,120]", exp-iat)
	}

	// 行持久化：ValidateToken 依赖行存在。
	var rows int64
	if err := s.DB().Model(&model.WeKnoraAuthToken{}).Where("user_id = ?", "ku-1").Count(&rows).Error; err != nil {
		t.Fatalf("count auth tokens: %v", err)
	}
	if rows != 1 {
		t.Fatalf("auth_tokens rows = %d, want 1", rows)
	}

	// 审计关联（来源标识）。
	events, err := s.ListAuditEvents(ctx)
	if err != nil || len(events) != 1 {
		t.Fatalf("audit events = %d, err=%v, want 1", len(events), err)
	}
	if events[0].Action != "kbridge.token.issued" {
		t.Fatalf("audit action = %q", events[0].Action)
	}
	if events[0].Actor != "subj-1" {
		t.Fatalf("audit actor = %q, want subj-1", events[0].Actor)
	}
}

// TestBridge_ClaimsAlignedWithLegacyIssue 契约自检：桥 token 的 claims 键集与
// pkg/auth/service.go:40-47 既有 WeKnora access token 签发完全对齐（同 claims 集）。
func TestBridge_ClaimsAlignedWithLegacyIssue(t *testing.T) {
	s, bridge := setupBridgeTest(t)
	ctx := context.Background()

	// 既有签发路径（pkg/auth/service.go）。
	authSvc := auth.NewAuthService(&config.Config{WeKnora: config.WeKnoraConfig{JWTSecret: testWeKnoraJWTSecret, RootTenantID: 1001}}, s)

	// 桥签发路径（签名密钥生产同源）；用同一上游用户，键值应完全一致。
	link := insertLink(t, s, "subj-2", "ku-2", "active")
	insertEntitlement(t, s, "subj-2", "1001")
	seedUpstreamUser(t, s, "ku-2", "user2@example.com", true, false)
	seedMembership(t, s, "ku-2", 1001, "active")
	sameUser, err := NewWeKnoraStore(s).GetUserByID(ctx, "ku-2")
	if err != nil {
		t.Fatalf("load upstream user: %v", err)
	}
	legacyAccess, _, err := authSvc.IssueWeKnoraTokens(ctx, sameUser, 1001)
	if err != nil {
		t.Fatalf("legacy IssueWeKnoraTokens for same user: %v", err)
	}
	legacyClaims := parseWithSecret(t, legacyAccess, testWeKnoraJWTSecret)

	bridgeToken, err := bridge.IssueUpstreamToken(ctx, link.ID, "1001", 60*time.Second)
	if err != nil {
		t.Fatalf("IssueUpstreamToken: %v", err)
	}
	bridgeClaims := parseWithSecret(t, bridgeToken, testWeKnoraJWTSecret)

	// 键集完全一致。
	legacyKeys, bridgeKeys := map[string]bool{}, map[string]bool{}
	for k := range legacyClaims {
		legacyKeys[k] = true
	}
	for k := range bridgeClaims {
		bridgeKeys[k] = true
	}
	for k := range legacyKeys {
		if !bridgeKeys[k] {
			t.Fatalf("claim %q present in legacy issue but missing in bridge token", k)
		}
	}
	for k := range bridgeKeys {
		if !legacyKeys[k] {
			t.Fatalf("claim %q present in bridge token but missing in legacy issue", k)
		}
	}
	// 共享键值语义一致。
	for _, k := range []string{"user_id", "email", "tenant_id", "type"} {
		if legacyClaims[k] != bridgeClaims[k] {
			t.Fatalf("claim %q legacy=%v bridge=%v", k, legacyClaims[k], bridgeClaims[k])
		}
	}
}

// TestBridge_ValidateTokenSemantics 契约自检：模拟 knowledge ValidateToken
// （JWT 验签 + auth_tokens 行存在 + 非 refresh + 用户 active）对桥 token 放行。
func TestBridge_ValidateTokenSemantics(t *testing.T) {
	s, bridge := setupBridgeTest(t)
	ctx := context.Background()
	link := insertLink(t, s, "subj-3", "ku-3", "active")
	insertEntitlement(t, s, "subj-3", "1001")
	seedUpstreamUser(t, s, "ku-3", "user3@example.com", true, false)
	seedMembership(t, s, "ku-3", 1001, "active")

	token, err := bridge.IssueUpstreamToken(ctx, link.ID, "1001", 60*time.Second)
	if err != nil {
		t.Fatalf("IssueUpstreamToken: %v", err)
	}

	// 1) JWT 验签（WeKnora secret）。
	claims := parseWithSecret(t, token, testWeKnoraJWTSecret)
	if claims["type"] == "refresh" {
		t.Fatal("refresh token presented as access")
	}
	// 2) 行存在。
	var record model.WeKnoraAuthToken
	if err := s.DB().WithContext(ctx).Where("token = ?", token).First(&record).Error; err != nil {
		t.Fatalf("auth_tokens row lookup: %v", err)
	}
	if record.TokenType != "access_token" || record.TokenType == "refresh_token" {
		t.Fatalf("token_type = %q", record.TokenType)
	}
	// 3) 用户 active。
	user, err := NewWeKnoraStore(s).GetUserByID(ctx, "ku-3")
	if err != nil || !user.IsActive {
		t.Fatalf("upstream user missing or inactive: %v", err)
	}
}

// TestBridge_IssueDenyConditions 签发条件负向用例。
func TestBridge_IssueDenyConditions(t *testing.T) {
	ctx := context.Background()

	t.Run("ttl over 120s and non-positive rejected", func(t *testing.T) {
		s, bridge := setupBridgeTest(t)
		link := insertLink(t, s, "subj-t", "ku-t", "active")
		insertEntitlement(t, s, "subj-t", "1001")
		seedUpstreamUser(t, s, "ku-t", "t@example.com", true, false)
		seedMembership(t, s, "ku-t", 1001, "active")
		for _, ttl := range []time.Duration{121 * time.Second, 0, -time.Second} {
			if _, err := bridge.IssueUpstreamToken(ctx, link.ID, "1001", ttl); !errors.Is(err, ErrTTLInvalid) {
				t.Fatalf("ttl=%s err=%v, want ErrTTLInvalid", ttl, err)
			}
		}
	})

	t.Run("disabled identity link rejected", func(t *testing.T) {
		s, bridge := setupBridgeTest(t)
		link := insertLink(t, s, "subj-d", "ku-d", "disabled")
		insertEntitlement(t, s, "subj-d", "1001")
		seedUpstreamUser(t, s, "ku-d", "d@example.com", true, false)
		seedMembership(t, s, "ku-d", 1001, "active")
		if _, err := bridge.IssueUpstreamToken(ctx, link.ID, "1001", 60*time.Second); !errors.Is(err, ErrIdentityDisabled) {
			t.Fatalf("err=%v, want ErrIdentityDisabled", err)
		}
	})

	t.Run("entitlement missing for tenant rejected", func(t *testing.T) {
		s, bridge := setupBridgeTest(t)
		link := insertLink(t, s, "subj-e", "ku-e", "active")
		insertEntitlement(t, s, "subj-e", "2002") // 只授权 2002，申请 1001
		seedUpstreamUser(t, s, "ku-e", "e@example.com", true, false)
		seedMembership(t, s, "ku-e", 1001, "active")
		if _, err := bridge.IssueUpstreamToken(ctx, link.ID, "1001", 60*time.Second); !errors.Is(err, ErrEntitlementMissing) {
			t.Fatalf("err=%v, want ErrEntitlementMissing", err)
		}
	})

	t.Run("system admin never minted", func(t *testing.T) {
		s, bridge := setupBridgeTest(t)
		link := insertLink(t, s, "subj-a", "ku-a", "active")
		insertEntitlement(t, s, "subj-a", "1001")
		seedUpstreamUser(t, s, "ku-a", "a@example.com", true, true)
		seedMembership(t, s, "ku-a", 1001, "active")
		if _, err := bridge.IssueUpstreamToken(ctx, link.ID, "1001", 60*time.Second); !errors.Is(err, ErrAdminTokenRefused) {
			t.Fatalf("err=%v, want ErrAdminTokenRefused", err)
		}
	})

	t.Run("inactive upstream user rejected", func(t *testing.T) {
		s, bridge := setupBridgeTest(t)
		link := insertLink(t, s, "subj-i", "ku-i", "active")
		insertEntitlement(t, s, "subj-i", "1001")
		seedUpstreamUser(t, s, "ku-i", "i@example.com", false, false)
		seedMembership(t, s, "ku-i", 1001, "active")
		if _, err := bridge.IssueUpstreamToken(ctx, link.ID, "1001", 60*time.Second); !errors.Is(err, ErrIdentityDisabled) {
			t.Fatalf("err=%v, want ErrIdentityDisabled", err)
		}
	})

	t.Run("missing tenant membership rejected", func(t *testing.T) {
		s, bridge := setupBridgeTest(t)
		link := insertLink(t, s, "subj-m", "ku-m", "active")
		insertEntitlement(t, s, "subj-m", "1001")
		seedUpstreamUser(t, s, "ku-m", "m@example.com", true, false)
		if _, err := bridge.IssueUpstreamToken(ctx, link.ID, "1001", 60*time.Second); !errors.Is(err, ErrEntitlementMissing) {
			t.Fatalf("err=%v, want ErrEntitlementMissing", err)
		}
	})

	t.Run("unknown identity link rejected", func(t *testing.T) {
		_, bridge := setupBridgeTest(t)
		if _, err := bridge.IssueUpstreamToken(ctx, uuid.NewString(), "1001", 60*time.Second); !errors.Is(err, ErrIdentityUnknown) {
			t.Fatalf("err=%v, want ErrIdentityUnknown", err)
		}
	})
}

// TestBridge_RevokeUpstreamTokens 撤权删除：删该用户全部行且幂等。
func TestBridge_RevokeUpstreamTokens(t *testing.T) {
	s, bridge := setupBridgeTest(t)
	ctx := context.Background()
	link := insertLink(t, s, "subj-r", "ku-r", "active")
	insertEntitlement(t, s, "subj-r", "1001")
	seedUpstreamUser(t, s, "ku-r", "r@example.com", true, false)
	seedMembership(t, s, "ku-r", 1001, "active")

	for i := 0; i < 2; i++ {
		if _, err := bridge.IssueUpstreamToken(ctx, link.ID, "1001", 60*time.Second); err != nil {
			t.Fatalf("issue %d: %v", i, err)
		}
	}
	deleted, err := bridge.RevokeUpstreamTokens(ctx, "ku-r")
	if err != nil {
		t.Fatalf("RevokeUpstreamTokens: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	// 幂等：再次删除 0 行。
	deleted, err = bridge.RevokeTokens(ctx, "ku-r")
	if err != nil || deleted != 0 {
		t.Fatalf("revoke again = %d/%v, want 0/nil", deleted, err)
	}
}

// TestBridge_NotifierChannel 桥注册为撤权失效通知通道：事件按主体映射删行。
func TestBridge_NotifierChannel(t *testing.T) {
	s, bridge := setupBridgeTest(t)
	ctx := context.Background()
	link := insertLink(t, s, "subj-n", "ku-n", "active")
	insertEntitlement(t, s, "subj-n", "1001")
	seedUpstreamUser(t, s, "ku-n", "n@example.com", true, false)
	seedMembership(t, s, "ku-n", 1001, "active")
	if _, err := bridge.IssueUpstreamToken(ctx, link.ID, "1001", 60*time.Second); err != nil {
		t.Fatalf("issue: %v", err)
	}

	notifier := authz.NewNotifier(nil)
	notifier.Register(bridge)
	notifier.Invalidate(ctx, authz.InvalidateEvent{Subject: "subj-n", Reason: "test", At: time.Now()})

	var rows int64
	if err := s.DB().Model(&model.WeKnoraAuthToken{}).Where("user_id = ?", "ku-n").Count(&rows).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Fatalf("auth_tokens rows = %d after notify, want 0", rows)
	}
	if notifier.PendingRetries() != 0 {
		t.Fatalf("pending retries = %d, want 0", notifier.PendingRetries())
	}
	if bridge.Name() != "kbridge" {
		t.Fatalf("channel name = %q", bridge.Name())
	}
}
