// INT-160 测试：GET /integration/v1/internal/skills（服务 token）——
// entitlement 租户过滤、停用 subject 403、复用 knowledge 假件断言逐租户查询。
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/authz"
	"weknora-tokendance/pkg/kbridge"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"

	"github.com/gin-gonic/gin"
)

// setupInternalSkillsEnv 装配 sqlite 双库 + 桥 + 假 knowledge（INT-120 假件），
// 注册 /integration/v1/internal/skills（RequireServiceToken）。
func setupInternalSkillsEnv(t *testing.T, fake *fakeKnowledge) (*gin.Engine, *storage.Storage) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	mainDB := openIntegrationTestSQLite(t)
	stateDB := openIntegrationTestSQLite(t)
	if err := mainDB.AutoMigrate(&model.WeKnoraUser{}, &model.WeKnoraTenantMember{}, &model.WeKnoraAuthToken{}); err != nil {
		t.Fatalf("migrate main: %v", err)
	}
	if err := stateDB.AutoMigrate(&model.IdentityLink{}, &model.ManagedEntitlement{}, &model.Session{}, &model.AuditEvent{}, &model.SkillPublishJob{}); err != nil {
		t.Fatalf("migrate state: %v", err)
	}
	s := storage.NewStorageWithDBs(mainDB, stateDB)

	server := httptest.NewServer(fake.handler(t))
	t.Cleanup(server.Close)

	cfg := &config.Config{
		Admin:   config.AdminConfig{Key: testAdminKey},
		Auth:    config.AuthConfig{Key: testAuthKey},
		WeKnora: config.WeKnoraConfig{BaseURL: server.URL},
		Bridge:  config.BridgeConfig{Key: testBridgeSecret},
	}
	bridge := kbridge.New(
		kbridge.Config{SignKey: testBridgeSecret},
		s,
		authz.NewEntitlementChecker(s, nil),
		kbridge.NewWeKnoraStore(s),
	)
	sh := NewSkillsHandler(cfg, s, bridge)
	internalHandler := NewInternalHandler(cfg, s)
	r := gin.New()
	internal := r.Group("/integration/v1/internal", internalHandler.RequireServiceToken)
	internal.GET("/skills", sh.InternalList)
	return r, s
}

// internalSkillsFixture rel-1 跨两空间、rel-2 仅 1002、rel-3 delisted。
func internalSkillsFixture(fake *fakeKnowledge) {
	rel1 := releaseFixture("rel-1", "1001", "1.0.0", 1001, 1002)
	rel2 := releaseFixture("rel-2", "1002", "2.0.0", 1002)
	rel3 := releaseFixture("rel-3", "1001", "3.0.0", 1001)
	rel3["status"] = "delisted"
	fake.releaseListByTenant["1001"] = []map[string]interface{}{rel1, rel3}
	fake.releaseListByTenant["1002"] = []map[string]interface{}{rel1, rel2}
}

// TestInternalSkillsList_MultiTenant 省略 tenant：全部 entitlement 租户聚合、
// publish_id 去重、delisted 排除、服务 token 缺失 401。
func TestInternalSkillsList_MultiTenant(t *testing.T) {
	fake := newFakeKnowledge()
	internalSkillsFixture(fake)
	r, s := setupInternalSkillsEnv(t, fake)
	seedKnowledgeSubject(t, s, "subj-k", "ku-k", "1001", "1002")
	headers := internalServiceHeaders(t)

	w := doGet(t, r, "/integration/v1/internal/skills?subject=subj-k", headers)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d, body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	skills, ok := body["skills"].([]interface{})
	if !ok || len(skills) != 2 {
		t.Fatalf("skills = %v, want 2 entries", body["skills"])
	}
	first := skills[0].(map[string]interface{})
	if first["publish_id"] != "rel-1" || first["version"] != "1.0.0" || first["source_tenant"] != "1001" {
		t.Fatalf("rel-1 entry = %v", first)
	}
	if first["manifest"] == nil || first["hash"] == nil {
		t.Fatalf("rel-1 missing manifest/hash: %v", first)
	}
	second := skills[1].(map[string]interface{})
	if second["publish_id"] != "rel-2" || second["source_tenant"] != "1002" {
		t.Fatalf("rel-2 entry = %v", second)
	}
	for _, entry := range skills {
		if entry.(map[string]interface{})["publish_id"] == "rel-3" {
			t.Fatalf("delisted release leaked into list: %v", skills)
		}
	}

	// 无服务 token → 401（RequireServiceToken）。
	w = doGet(t, r, "/integration/v1/internal/skills?subject=subj-k", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", w.Code)
	}
	// dept_admin 角色不足 → 403。
	w = doGet(t, r, "/integration/v1/internal/skills?subject=subj-k", internalDeptAdminHeaders(t))
	if w.Code != http.StatusForbidden {
		t.Fatalf("dept_admin token = %d, want 403", w.Code)
	}

	if n := len(auditEventsByAction(t, s, "integration.internal.skills.listed")); n != 1 {
		t.Fatalf("internal.skills.listed audit events = %d, want 1", n)
	}
}

// TestInternalSkillsList_TenantFilter 显式 tenant：仅该租户可见技能；
// 未授权 tenant → 403 且零上游 release 查询。
func TestInternalSkillsList_TenantFilter(t *testing.T) {
	fake := newFakeKnowledge()
	internalSkillsFixture(fake)
	r, s := setupInternalSkillsEnv(t, fake)
	seedKnowledgeSubject(t, s, "subj-k", "ku-k", "1001", "1002")
	headers := internalServiceHeaders(t)

	w := doGet(t, r, "/integration/v1/internal/skills?subject=subj-k&tenant=1002", headers)
	if w.Code != http.StatusOK {
		t.Fatalf("tenant filter = %d, body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	skills, ok := body["skills"].([]interface{})
	if !ok || len(skills) != 2 {
		t.Fatalf("skills = %v, want 2 entries from tenant 1002", body["skills"])
	}
	for _, entry := range skills {
		item := entry.(map[string]interface{})
		if item["source_tenant"] != "1002" {
			t.Fatalf("entry source_tenant = %v, want 1002: %v", item["source_tenant"], item)
		}
	}

	w = doGet(t, r, "/integration/v1/internal/skills?subject=subj-k&tenant=9999", headers)
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeEntitlementRevoked {
		t.Fatalf("unentitled tenant = %d/%s, want 403/%s", w.Code, errorCode(t, w), errCodeEntitlementRevoked)
	}
}

// TestInternalSkillsList_SubjectStates 停用 subject / 无 entitlement /
// 未知 subject / 缺 subject 参数 → 4xx。
func TestInternalSkillsList_SubjectStates(t *testing.T) {
	fake := newFakeKnowledge()
	internalSkillsFixture(fake)
	r, s := setupInternalSkillsEnv(t, fake)
	headers := internalServiceHeaders(t)

	// 停用 subject：identity link disabled → 403 ENTITLEMENT_REVOKED。
	link := insertIdentityLink(t, s, "subj-off", "ku-off")
	if _, err := storage.DisableIdentityLinkByIDTx(s.StateDB().WithContext(context.Background()), link.ID, time.Now()); err != nil {
		t.Fatalf("DisableIdentityLinkByIDTx: %v", err)
	}
	insertEntitlement(t, s, "subj-off", "1001", "chat", 0, false)
	seedKnowledgeUpstreamUser(t, s, "ku-off", "1001")
	w := doGet(t, r, "/integration/v1/internal/skills?subject=subj-off", headers)
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeEntitlementRevoked {
		t.Fatalf("disabled subject = %d/%s, want 403/%s", w.Code, errorCode(t, w), errCodeEntitlementRevoked)
	}

	// active link 但无任何 entitlement → 403。
	insertIdentityLink(t, s, "subj-noent", "ku-noent")
	seedKnowledgeUpstreamUser(t, s, "ku-noent", "1001")
	w = doGet(t, r, "/integration/v1/internal/skills?subject=subj-noent", headers)
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeEntitlementRevoked {
		t.Fatalf("no entitlement = %d/%s, want 403/%s", w.Code, errorCode(t, w), errCodeEntitlementRevoked)
	}

	// 未知 subject → 403。
	w = doGet(t, r, "/integration/v1/internal/skills?subject=ghost", headers)
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeEntitlementRevoked {
		t.Fatalf("unknown subject = %d/%s, want 403/%s", w.Code, errorCode(t, w), errCodeEntitlementRevoked)
	}

	// 缺 subject 参数 → 400。
	w = doGet(t, r, "/integration/v1/internal/skills", headers)
	if w.Code != http.StatusBadRequest || errorCode(t, w) != errCodeInvalidRequest {
		t.Fatalf("missing subject = %d/%s, want 400/%s", w.Code, errorCode(t, w), errCodeInvalidRequest)
	}
}
