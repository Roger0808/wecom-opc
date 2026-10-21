// TASK-P12（INT-150 task-actor 段）测试：假 knowledge 断言 task-actor 转发时
// 调用上下文的 actor_kind=task 与 task_id/device 绑定、无 entitlement 403、
// 查询/取消以 subject 上游 token 调用并重放 task 上下文、会话流程不受分发影响。
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/authz"
	"weknora-tokendance/pkg/kbridge"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"

	"github.com/gin-gonic/gin"
)

// setupMCPTaskEnv 装配 sqlite 双库 + 桥 + 假 knowledge（fakeMCP），注册
// invoke-task（RequireServiceToken）与带服务 token 分发的查询/取消路由。
// 与 setupMCPEnv 的差异：cfg 带 Admin.Key（服务 token 体系）。
func setupMCPTaskEnv(t *testing.T) (*gin.Engine, *storage.Storage, *fakeMCP) {
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

	mux := http.NewServeMux()
	fake := newFakeMCP()
	mux.Handle("/api/v1/integration/mcp/", fake)
	server := httptest.NewServer(mux)
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
	r := gin.New()
	mh := NewMCPHandler(cfg, s, bridge)
	internalHandler := NewInternalHandler(cfg, s)
	r.POST("/integration/v1/mcp/invoke-task", internalHandler.RequireServiceToken, mh.InvokeTask)
	r.GET("/integration/v1/mcp/invocations/:id", mh.GetInvocation)
	r.POST("/integration/v1/mcp/invocations/:id/cancel", mh.CancelInvocation)
	return r, s, fake
}

// ---------- POST /integration/v1/mcp/invoke-task ----------

// TestMCPInvokeTaskForwardsTaskBinding 正常路径：假 knowledge 断言上游 token
// 与 actor_kind=task 上下文（task_id/device 绑定、args_digest、TTL 120s）。
func TestMCPInvokeTaskForwardsTaskBinding(t *testing.T) {
	r, s, fake := setupMCPTaskEnv(t)
	seedKnowledgeSubject(t, s, "subj-1", "ku-1", "2001", "2002")
	headers := internalServiceHeaders(t)

	body := `{"subject":"subj-1","tenant_id":"2002","service_id":"svc-1","tool_name":"tool.x","arguments":{"b":2,"a":"txt"},"idempotency_key":"key-1","task_id":"task-9","device":"dev-9"}`
	rec := mcpTestClient(t, r, http.MethodPost, "/integration/v1/mcp/invoke-task", body, headers)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != `{"success":true,"data":{"invocation_id":"inv-1","status":"created"}}` {
		t.Fatalf("response not passthrough: %s", rec.Body.String())
	}

	call := fake.lastCall(t)
	if call.method != http.MethodPost || call.path != "/api/v1/integration/mcp/invocations" {
		t.Fatalf("upstream call = %s %s", call.method, call.path)
	}
	if call.bearer == nil || call.bearer["user_id"] != "ku-1" {
		t.Fatalf("upstream bearer claims wrong: %v", call.bearer)
	}
	if tenant, _ := call.bearer["tenant_id"].(float64); tenant != 2002 {
		t.Fatalf("upstream bearer tenant = %v, want 2002", call.bearer["tenant_id"])
	}
	if call.ctx == nil {
		t.Fatalf("invocation_context not parseable")
	}
	checks := map[string]interface{}{
		"iss":        mcpContextIssuer,
		"sub":        "ku-1",
		"user":       "subj-1",
		"tenant":     float64(2002),
		"aud":        mcpContextAudience,
		"actor_kind": "task",
		"task_id":    "task-9",
		"device":     "dev-9",
	}
	for key, want := range checks {
		if call.ctx[key] != want {
			t.Fatalf("context claim %s = %v, want %v (all=%v)", key, call.ctx[key], want, call.ctx)
		}
	}
	for _, key := range []string{"request_id", "jti"} {
		if v, _ := call.ctx[key].(string); v == "" {
			t.Fatalf("context claim %s empty", key)
		}
	}
	exp, _ := call.ctx["exp"].(float64)
	iat, _ := call.ctx["iat"].(float64)
	if exp-iat != 120 {
		t.Fatalf("context ttl = %v, want 120", exp-iat)
	}
	if want := digestOf(t, map[string]interface{}{"a": "txt", "b": float64(2)}); call.ctx["args_digest"] != want {
		t.Fatalf("args_digest = %v, want %v", call.ctx["args_digest"], want)
	}
	var up map[string]json.RawMessage
	if err := json.Unmarshal([]byte(call.body), &up); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	if string(up["arguments"]) != `{"b":2,"a":"txt"}` {
		t.Fatalf("arguments not verbatim: %s", up["arguments"])
	}
	if string(up["service_id"]) != `"svc-1"` || string(up["tool_name"]) != `"tool.x"` || string(up["idempotency_key"]) != `"key-1"` {
		t.Fatalf("upstream body fields wrong: %s", call.body)
	}
	if _, ok := up["tenant_id"]; ok {
		t.Fatalf("platform-only tenant_id leaked upstream: %s", call.body)
	}

	if n := len(auditEventsByAction(t, s, "integration.mcp.invoke_task")); n != 1 {
		t.Fatalf("invoke_task audit events = %d, want 1", n)
	}
}

// TestMCPInvokeTaskDenials 服务 token 缺失/角色不足、必填缺失、identity 停用、
// tenant 不在 entitlement 集内——一律 4xx 且零上游触达。
func TestMCPInvokeTaskDenials(t *testing.T) {
	validBody := `{"subject":"subj-1","tenant_id":"2001","service_id":"svc-1","tool_name":"tool.x","arguments":{},"task_id":"task-9","device":"dev-9"}`

	t.Run("missing service token", func(t *testing.T) {
		r, _, fake := setupMCPTaskEnv(t)
		rec := mcpTestClient(t, r, http.MethodPost, "/integration/v1/mcp/invoke-task", validBody, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d, want 401", rec.Code)
		}
		if fake.callCount() != 0 {
			t.Fatalf("upstream must not be called without service token")
		}
	})

	t.Run("dept_admin service token", func(t *testing.T) {
		r, s, fake := setupMCPTaskEnv(t)
		seedKnowledgeSubject(t, s, "subj-1", "ku-1", "2001")
		rec := mcpTestClient(t, r, http.MethodPost, "/integration/v1/mcp/invoke-task", validBody, internalDeptAdminHeaders(t))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status=%d, want 403", rec.Code)
		}
		if fake.callCount() != 0 {
			t.Fatalf("upstream must not be called for dept_admin token")
		}
	})

	t.Run("missing task binding", func(t *testing.T) {
		r, s, fake := setupMCPTaskEnv(t)
		seedKnowledgeSubject(t, s, "subj-1", "ku-1", "2001")
		headers := internalServiceHeaders(t)
		bodies := []string{
			`{"subject":"subj-1","tenant_id":"2001","service_id":"svc-1","tool_name":"tool.x","arguments":{},"device":"dev-9"}`,
			`{"subject":"subj-1","tenant_id":"2001","service_id":"svc-1","tool_name":"tool.x","arguments":{},"task_id":"task-9"}`,
		}
		for _, body := range bodies {
			rec := mcpTestClient(t, r, http.MethodPost, "/integration/v1/mcp/invoke-task", body, headers)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("body=%s status=%d, want 400", body, rec.Code)
			}
		}
		if fake.callCount() != 0 {
			t.Fatalf("upstream must not be called with missing task binding")
		}
	})

	t.Run("unknown subject", func(t *testing.T) {
		r, _, fake := setupMCPTaskEnv(t)
		rec := mcpTestClient(t, r, http.MethodPost, "/integration/v1/mcp/invoke-task", validBody, internalServiceHeaders(t))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), errCodeSubjectNotProv) {
			t.Fatalf("status=%d body=%s, want 403/%s", rec.Code, rec.Body.String(), errCodeSubjectNotProv)
		}
		if fake.callCount() != 0 {
			t.Fatalf("upstream must not be called for unknown subject")
		}
	})

	t.Run("tenant not entitled", func(t *testing.T) {
		r, s, fake := setupMCPTaskEnv(t)
		seedKnowledgeSubject(t, s, "subj-1", "ku-1", "2001")
		body := `{"subject":"subj-1","tenant_id":"9999","service_id":"svc-1","tool_name":"tool.x","arguments":{},"task_id":"task-9","device":"dev-9"}`
		rec := mcpTestClient(t, r, http.MethodPost, "/integration/v1/mcp/invoke-task", body, internalServiceHeaders(t))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), errCodeSpaceForbidden) {
			t.Fatalf("status=%d body=%s, want 403/%s", rec.Code, rec.Body.String(), errCodeSpaceForbidden)
		}
		if fake.callCount() != 0 {
			t.Fatalf("upstream must not be called for unentitled tenant")
		}
	})

	t.Run("disabled identity", func(t *testing.T) {
		r, s, fake := setupMCPTaskEnv(t)
		link := &model.IdentityLink{
			IdentitySource:   "wecom",
			OrganizationID:   testCorpID,
			ImmutableSubject: "subj-1",
			KnowledgeUserID:  "ku-1",
			Status:           "disabled",
		}
		if err := s.InsertIdentityLink(context.Background(), link); err != nil {
			t.Fatalf("InsertIdentityLink: %v", err)
		}
		insertEntitlement(t, s, "subj-1", "2001", "chat", 0, false)
		seedKnowledgeUpstreamUser(t, s, "ku-1", "2001")
		rec := mcpTestClient(t, r, http.MethodPost, "/integration/v1/mcp/invoke-task", validBody, internalServiceHeaders(t))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), errCodeSubjectNotProv) {
			t.Fatalf("status=%d body=%s, want 403/%s", rec.Code, rec.Body.String(), errCodeSubjectNotProv)
		}
		if fake.callCount() != 0 {
			t.Fatalf("upstream must not be called for disabled identity")
		}
	})
}

// ---------- task 查询/取消转发 ----------

// TestMCPTaskQueryForward 服务 token 查询：以 subject 上游 token 调用并
// 重放 actor_kind=task 上下文（X-Invocation-Context），响应原样透传。
func TestMCPTaskQueryForward(t *testing.T) {
	r, s, fake := setupMCPTaskEnv(t)
	seedKnowledgeSubject(t, s, "subj-1", "ku-1", "2001", "2002")
	headers := internalServiceHeaders(t)

	rec := mcpTestClient(t, r, http.MethodGet, "/integration/v1/mcp/invocations/inv-1?subject=subj-1&tenant=2001&task_id=task-9&device=dev-9", "", headers)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != `{"success":true,"data":{"invocation_id":"inv-1","status":"executing"}}` {
		t.Fatalf("response not passthrough: %s", rec.Body.String())
	}
	call := fake.lastCall(t)
	if call.method != http.MethodGet || call.path != "/api/v1/integration/mcp/invocations/inv-1" {
		t.Fatalf("upstream call = %s %s", call.method, call.path)
	}
	if call.bearer == nil || call.bearer["user_id"] != "ku-1" {
		t.Fatalf("upstream bearer claims wrong: %v", call.bearer)
	}
	if tenant, _ := call.bearer["tenant_id"].(float64); tenant != 2001 {
		t.Fatalf("upstream bearer tenant = %v, want 2001", call.bearer["tenant_id"])
	}
	raw := call.header.Get(mcpTaskContextHeader)
	if raw == "" {
		t.Fatalf("%s header missing on task query", mcpTaskContextHeader)
	}
	ctxClaims := parseTestJWT(raw)
	if ctxClaims == nil {
		t.Fatalf("task context header not parseable")
	}
	if ctxClaims["actor_kind"] != "task" || ctxClaims["task_id"] != "task-9" || ctxClaims["device"] != "dev-9" ||
		ctxClaims["sub"] != "ku-1" || ctxClaims["user"] != "subj-1" {
		t.Fatalf("task context claims wrong: %v", ctxClaims)
	}
	if tenant, _ := ctxClaims["tenant"].(float64); tenant != 2001 {
		t.Fatalf("task context tenant = %v, want 2001", ctxClaims["tenant"])
	}
	if n := len(auditEventsByAction(t, s, "integration.mcp.task_query")); n != 1 {
		t.Fatalf("task_query audit events = %d, want 1", n)
	}
}

// TestMCPTaskQueryDenials 缺 subject/tenant、tenant 未授权、未知 subject——
// 一律 4xx 且零上游触达。
func TestMCPTaskQueryDenials(t *testing.T) {
	t.Run("missing subject param", func(t *testing.T) {
		r, s, fake := setupMCPTaskEnv(t)
		seedKnowledgeSubject(t, s, "subj-1", "ku-1", "2001")
		rec := mcpTestClient(t, r, http.MethodGet, "/integration/v1/mcp/invocations/inv-1?tenant=2001", "", internalServiceHeaders(t))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want 400", rec.Code)
		}
		if fake.callCount() != 0 {
			t.Fatalf("upstream must not be called without subject")
		}
	})

	t.Run("tenant not entitled", func(t *testing.T) {
		r, s, fake := setupMCPTaskEnv(t)
		seedKnowledgeSubject(t, s, "subj-1", "ku-1", "2001")
		rec := mcpTestClient(t, r, http.MethodGet, "/integration/v1/mcp/invocations/inv-1?subject=subj-1&tenant=9999", "", internalServiceHeaders(t))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), errCodeSpaceForbidden) {
			t.Fatalf("status=%d body=%s, want 403/%s", rec.Code, rec.Body.String(), errCodeSpaceForbidden)
		}
		if fake.callCount() != 0 {
			t.Fatalf("upstream must not be called for unentitled tenant")
		}
	})

	t.Run("unknown subject", func(t *testing.T) {
		r, _, fake := setupMCPTaskEnv(t)
		rec := mcpTestClient(t, r, http.MethodGet, "/integration/v1/mcp/invocations/inv-1?subject=ghost&tenant=2001", "", internalServiceHeaders(t))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), errCodeSubjectNotProv) {
			t.Fatalf("status=%d body=%s, want 403/%s", rec.Code, rec.Body.String(), errCodeSubjectNotProv)
		}
		if fake.callCount() != 0 {
			t.Fatalf("upstream must not be called for unknown subject")
		}
	})
}

// TestMCPTaskCancelForward task 取消转发：POST /cancel 带重放上下文。
func TestMCPTaskCancelForward(t *testing.T) {
	r, s, fake := setupMCPTaskEnv(t)
	seedKnowledgeSubject(t, s, "subj-1", "ku-1", "2001")
	headers := internalServiceHeaders(t)

	rec := mcpTestClient(t, r, http.MethodPost, "/integration/v1/mcp/invocations/inv-1/cancel?subject=subj-1&tenant=2001&task_id=task-9", "", headers)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	call := fake.lastCall(t)
	if call.method != http.MethodPost || call.path != "/api/v1/integration/mcp/invocations/inv-1/cancel" {
		t.Fatalf("upstream call = %s %s", call.method, call.path)
	}
	if call.bearer == nil || call.bearer["user_id"] != "ku-1" {
		t.Fatalf("upstream bearer claims wrong: %v", call.bearer)
	}
	raw := call.header.Get(mcpTaskContextHeader)
	if raw == "" {
		t.Fatalf("%s header missing on task cancel", mcpTaskContextHeader)
	}
	if ctxClaims := parseTestJWT(raw); ctxClaims == nil || ctxClaims["actor_kind"] != "task" || ctxClaims["task_id"] != "task-9" {
		t.Fatalf("task context claims wrong: %v", ctxClaims)
	}
}

// TestMCPQueryAuthSchemeDispatch 分发边界：dept_admin 服务 token 已识别但
// 角色不足 → 403；平台会话 token 不受分发影响仍走会话转发（无重放头）。
func TestMCPQueryAuthSchemeDispatch(t *testing.T) {
	r, s, fake := setupMCPTaskEnv(t)
	link := seedKnowledgeSubject(t, s, "subj-1", "ku-1", "2001", "2002")

	rec := mcpTestClient(t, r, http.MethodGet, "/integration/v1/mcp/invocations/inv-1?subject=subj-1&tenant=2001", "", internalDeptAdminHeaders(t))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("dept_admin query status=%d, want 403", rec.Code)
	}
	if fake.callCount() != 0 {
		t.Fatalf("upstream must not be called for dept_admin token")
	}

	rec = mcpTestClient(t, r, http.MethodGet, "/integration/v1/mcp/invocations/inv-1?subject=subj-1&tenant=2001", "", platformAccessHeaders(t, s, link.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("session query status=%d body=%s", rec.Code, rec.Body.String())
	}
	call := fake.lastCall(t)
	if call.header.Get(mcpTaskContextHeader) != "" {
		t.Fatalf("session flow must not carry task context header")
	}
	if call.bearer == nil || call.bearer["user_id"] != "ku-1" {
		t.Fatalf("session flow bearer wrong: %v", call.bearer)
	}
}
