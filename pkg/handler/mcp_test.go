// TASK-P11（INT-150 platform 段）测试：假 knowledge（httptest）校验平台转发时
// 附带的 Bearer 上游 token 与 invocation_context 双身份，body/状态码原样透传。
package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/authz"
	"weknora-tokendance/pkg/kbridge"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// fakeMCPResponse 单条假 knowledge 响应。
type fakeMCPResponse struct {
	status int
	body   string
}

// mcpCall 一次被记录的转发请求。
type mcpCall struct {
	method string
	path   string
	bearer jwt.MapClaims // 解析后的上游 token claims
	body   string
	ctx    jwt.MapClaims // 解析后的 invocation_context claims
	header http.Header   // 原始请求头副本（task 查询重放断言用）
}

// fakeMCP 假 knowledge MCP integration 路由（/api/v1/integration/mcp/*）。
type fakeMCP struct {
	mu     sync.Mutex
	routes map[string]fakeMCPResponse // "METHOD /path" 精确匹配
	delay  time.Duration
	calls  []mcpCall
}

func newFakeMCP() *fakeMCP {
	return &fakeMCP{
		routes: map[string]fakeMCPResponse{
			"POST /api/v1/integration/mcp/invocations":                {http.StatusOK, `{"success":true,"data":{"invocation_id":"inv-1","status":"created"}}`},
			"GET /api/v1/integration/mcp/invocations/inv-1":           {http.StatusOK, `{"success":true,"data":{"invocation_id":"inv-1","status":"executing"}}`},
			"POST /api/v1/integration/mcp/invocations/inv-1/cancel":   {http.StatusOK, `{"success":true,"data":{"invocation_id":"inv-1","status":"cancelled"}}`},
			"POST /api/v1/integration/mcp/invocations/inv-1/decision": {http.StatusOK, `{"success":true,"data":{"invocation":{"invocation_id":"inv-1","status":"executing"},"decision":{"decision":"approve"}}}`},
			"GET /api/v1/integration/mcp/services":                    {http.StatusOK, `{"success":true,"data":{"services":[{"service_id":"svc-1","tools":["tool.x"]}]}}`},
		},
	}
}

func parseTestJWT(raw string) jwt.MapClaims {
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (interface{}, error) {
		return []byte(testBridgeSecret), nil
	}, jwt.WithoutClaimsValidation())
	if err != nil {
		return nil
	}
	return claims
}

func (f *fakeMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	call := mcpCall{method: r.Method, path: r.URL.Path, body: string(body), header: r.Header.Clone()}
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	call.bearer = parseTestJWT(auth)
	var payload struct {
		InvocationContext string `json:"invocation_context"`
	}
	if len(body) > 0 && json.Unmarshal(body, &payload) == nil && payload.InvocationContext != "" {
		call.ctx = parseTestJWT(payload.InvocationContext)
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	delay := f.delay
	resp, ok := f.routes[r.Method+" "+r.URL.Path]
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"error":{"code":"NOT_FOUND","message":"no canned route"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.status)
	_, _ = w.Write([]byte(resp.body))
}

func (f *fakeMCP) lastCall(t *testing.T) mcpCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatalf("fake knowledge recorded no calls")
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakeMCP) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// setupMCPEnv 装配 sqlite 双库 + 桥 + 假 knowledge（INT-120 假件复用 + fakeMCP）。
func setupMCPEnv(t *testing.T) (*gin.Engine, *storage.Storage, *fakeMCP) {
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
	knowledgeMux := (&fakeKnowledge{
		searchStatus:        http.StatusOK,
		knowledgeData:       map[string]json.RawMessage{},
		knowledgeCode:       map[string]int{},
		fileBytes:           map[string][]byte{},
		fileStatus:          map[string]int{},
		releaseByID:         map[string]map[string]interface{}{},
		releaseListByTenant: map[string][]map[string]interface{}{},
		metadataStatus:      map[string]int{},
		archiveBytes:        map[string][]byte{},
		archiveStatus:       map[string]int{},
	}).handler(t)
	mux.Handle("/api/v1/knowledge-bases/", knowledgeMux)
	mux.Handle("/api/v1/skills/", knowledgeMux)
	mux.Handle("/api/v1/knowledge/", knowledgeMux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	cfg := &config.Config{
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
	r.POST("/integration/v1/mcp/invoke", mh.Invoke)
	r.GET("/integration/v1/mcp/invocations/:id", mh.GetInvocation)
	r.POST("/integration/v1/mcp/invocations/:id/cancel", mh.CancelInvocation)
	r.POST("/integration/v1/mcp/invocations/:id/decision", mh.DecideInvocation)
	r.GET("/integration/v1/mcp/services", mh.ListServices)
	return r, s, fake
}

func mcpTestClient(t *testing.T, r *gin.Engine, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func digestOf(t *testing.T, args map[string]interface{}) string {
	t.Helper()
	canonical, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// TestMCPInvokeForwardsDualIdentity 正常路径：上游 token + 调用上下文均可解、
// claims 正确，body 原样转发，响应原样透传。
func TestMCPInvokeForwardsDualIdentity(t *testing.T) {
	r, s, fake := setupMCPEnv(t)
	link := seedKnowledgeSubject(t, s, "subj-1", "ku-1", "2001", "2002")
	headers := platformAccessHeaders(t, s, link.ID)

	body := `{"service_id":"svc-1","tool_name":"tool.x","arguments":{"b":2,"a":"txt"},"idempotency_key":"key-1"}`
	rec := mcpTestClient(t, r, http.MethodPost, "/integration/v1/mcp/invoke", body, headers)
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
	// Bearer 上游 token：桥签发，tenant 取升序首个授权空间。
	if call.bearer == nil {
		t.Fatalf("upstream bearer token not parseable")
	}
	if call.bearer["type"] != "access" || call.bearer["user_id"] != "ku-1" {
		t.Fatalf("upstream bearer claims wrong: %v", call.bearer)
	}
	if tenant, _ := call.bearer["tenant_id"].(float64); tenant != 2001 {
		t.Fatalf("upstream bearer tenant = %v, want 2001", call.bearer["tenant_id"])
	}
	// 调用上下文：与 knowledge 侧校验一致的 claims。
	if call.ctx == nil {
		t.Fatalf("invocation_context not parseable")
	}
	checks := map[string]interface{}{
		"iss":        mcpContextIssuer,
		"sub":        "ku-1",
		"user":       "subj-1",
		"tenant":     float64(2001),
		"aud":        mcpContextAudience,
		"actor_kind": "user",
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
	wantDigest := digestOf(t, map[string]interface{}{"a": "txt", "b": float64(2)})
	if call.ctx["args_digest"] != wantDigest {
		t.Fatalf("args_digest = %v, want %v", call.ctx["args_digest"], wantDigest)
	}
	// body 原样：arguments 字节 verbatim + 平台附加 invocation_context。
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
	if _, ok := up["invocation_context"]; !ok {
		t.Fatalf("invocation_context missing upstream: %s", call.body)
	}
}

// TestMCPInvokeExplicitTenant 显式 tenant_id：在授权集内生效，集外 403。
func TestMCPInvokeExplicitTenant(t *testing.T) {
	r, s, fake := setupMCPEnv(t)
	link := seedKnowledgeSubject(t, s, "subj-1", "ku-1", "2001", "2002")
	headers := platformAccessHeaders(t, s, link.ID)

	body := `{"service_id":"svc-1","tool_name":"tool.x","arguments":{},"tenant_id":"2002"}`
	rec := mcpTestClient(t, r, http.MethodPost, "/integration/v1/mcp/invoke", body, headers)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	call := fake.lastCall(t)
	if tenant, _ := call.bearer["tenant_id"].(float64); tenant != 2002 {
		t.Fatalf("bearer tenant = %v, want 2002", call.bearer["tenant_id"])
	}
	if tenant, _ := call.ctx["tenant"].(float64); tenant != 2002 {
		t.Fatalf("context tenant = %v, want 2002", call.ctx["tenant"])
	}

	bad := `{"service_id":"svc-1","tool_name":"tool.x","arguments":{},"tenant_id":"9999"}`
	rec = mcpTestClient(t, r, http.MethodPost, "/integration/v1/mcp/invoke", bad, headers)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), errCodeSpaceForbidden) {
		t.Fatalf("code not SPACE_FORBIDDEN: %s", rec.Body.String())
	}
	if fake.callCount() != 1 {
		t.Fatalf("upstream called for unauthorized tenant")
	}
}

// TestMCPInvokeNoEntitlement 无任何 active entitlement → 403，不触达上游。
func TestMCPInvokeNoEntitlement(t *testing.T) {
	r, s, fake := setupMCPEnv(t)
	link := insertIdentityLink(t, s, "subj-1", "ku-1")
	seedKnowledgeUpstreamUser(t, s, "ku-1", "2001")
	headers := platformAccessHeaders(t, s, link.ID)

	body := `{"service_id":"svc-1","tool_name":"tool.x","arguments":{}}`
	rec := mcpTestClient(t, r, http.MethodPost, "/integration/v1/mcp/invoke", body, headers)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), errCodeSpaceForbidden) {
		t.Fatalf("code not SPACE_FORBIDDEN: %s", rec.Body.String())
	}
	if fake.callCount() != 0 {
		t.Fatalf("upstream must not be called without entitlement")
	}
}

// TestMCPInvokeDisabledIdentity 身份关联停用 → 桥拒签 → 403 SUBJECT_NOT_PROVISIONED。
func TestMCPInvokeDisabledIdentity(t *testing.T) {
	r, s, fake := setupMCPEnv(t)
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
	headers := platformAccessHeaders(t, s, link.ID)

	body := `{"service_id":"svc-1","tool_name":"tool.x","arguments":{}}`
	rec := mcpTestClient(t, r, http.MethodPost, "/integration/v1/mcp/invoke", body, headers)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), errCodeSubjectNotProv) {
		t.Fatalf("code not SUBJECT_NOT_PROVISIONED: %s", rec.Body.String())
	}
	if fake.callCount() != 0 {
		t.Fatalf("upstream must not be called for disabled identity")
	}
}

// TestMCPInvokeKnowledgeErrorPassthrough knowledge 403 MCP_POLICY_DENIED /
// 202 MCP_APPROVAL_REQUIRED 原样透传（状态码 + code + body）。
func TestMCPInvokeKnowledgeErrorPassthrough(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantStatus int
	}{
		{"policy denied", http.StatusForbidden, `{"success":false,"error":{"code":"MCP_POLICY_DENIED","message":"tool not allowed"}}`, http.StatusForbidden},
		{"approval required", http.StatusAccepted, `{"success":true,"code":"MCP_APPROVAL_REQUIRED","data":{"invocation_id":"inv-1","status":"waiting_approval"}}`, http.StatusAccepted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, s, fake := setupMCPEnv(t)
			fake.routes["POST /api/v1/integration/mcp/invocations"] = fakeMCPResponse{tc.status, tc.body}
			link := seedKnowledgeSubject(t, s, "subj-1", "ku-1", "2001")
			headers := platformAccessHeaders(t, s, link.ID)

			body := `{"service_id":"svc-1","tool_name":"tool.x","arguments":{}}`
			rec := mcpTestClient(t, r, http.MethodPost, "/integration/v1/mcp/invoke", body, headers)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if strings.TrimSpace(rec.Body.String()) != tc.body {
				t.Fatalf("body not passthrough: %s", rec.Body.String())
			}
		})
	}
}

// TestMCPInvokeUpstreamTimeout 上游超时 → 502 UPSTREAM_UNAVAILABLE。
func TestMCPInvokeUpstreamTimeout(t *testing.T) {
	orig := upstreamTimeout
	upstreamTimeout = 50 * time.Millisecond
	defer func() { upstreamTimeout = orig }()

	r, s, fake := setupMCPEnv(t)
	fake.delay = 300 * time.Millisecond
	link := seedKnowledgeSubject(t, s, "subj-1", "ku-1", "2001")
	headers := platformAccessHeaders(t, s, link.ID)

	body := `{"service_id":"svc-1","tool_name":"tool.x","arguments":{}}`
	rec := mcpTestClient(t, r, http.MethodPost, "/integration/v1/mcp/invoke", body, headers)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), errCodeUpstreamUnavailable) {
		t.Fatalf("code not UPSTREAM_UNAVAILABLE: %s", rec.Body.String())
	}
}

// TestMCPQueryCancelDecisionServicesForward 查询/取消/决定/目录四路转发：
// 路径、Bearer、body（decision）原样，tenant query 显式指定空间生效。
func TestMCPQueryCancelDecisionServicesForward(t *testing.T) {
	r, s, fake := setupMCPEnv(t)
	link := seedKnowledgeSubject(t, s, "subj-1", "ku-1", "2001", "2002")
	headers := platformAccessHeaders(t, s, link.ID)

	cases := []struct {
		name       string
		method     string
		path       string
		body       string
		wantPath   string
		wantStatus int
		wantBody   string
	}{
		{"query", http.MethodGet, "/integration/v1/mcp/invocations/inv-1", "", "/api/v1/integration/mcp/invocations/inv-1", http.StatusOK, `{"success":true,"data":{"invocation_id":"inv-1","status":"executing"}}`},
		{"cancel", http.MethodPost, "/integration/v1/mcp/invocations/inv-1/cancel", "", "/api/v1/integration/mcp/invocations/inv-1/cancel", http.StatusOK, `{"success":true,"data":{"invocation_id":"inv-1","status":"cancelled"}}`},
		{"decision", http.MethodPost, "/integration/v1/mcp/invocations/inv-1/decision", `{"decision":"approve","args_digest":"d1","policy_version":"v1"}`, "/api/v1/integration/mcp/invocations/inv-1/decision", http.StatusOK, `{"success":true,"data":{"invocation":{"invocation_id":"inv-1","status":"executing"},"decision":{"decision":"approve"}}}`},
		{"services", http.MethodGet, "/integration/v1/mcp/services", "", "/api/v1/integration/mcp/services", http.StatusOK, `{"success":true,"data":{"services":[{"service_id":"svc-1","tools":["tool.x"]}]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := mcpTestClient(t, r, tc.method, tc.path, tc.body, headers)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if strings.TrimSpace(rec.Body.String()) != tc.wantBody {
				t.Fatalf("body not passthrough: %s", rec.Body.String())
			}
			call := fake.lastCall(t)
			if call.method != tc.method || call.path != tc.wantPath {
				t.Fatalf("upstream call = %s %s, want %s %s", call.method, call.path, tc.method, tc.wantPath)
			}
			if call.bearer == nil || call.bearer["user_id"] != "ku-1" {
				t.Fatalf("upstream bearer missing/invalid: %v", call.bearer)
			}
			if tenant, _ := call.bearer["tenant_id"].(float64); tenant != 2001 {
				t.Fatalf("bearer tenant = %v, want 2001", call.bearer["tenant_id"])
			}
			if tc.body != "" && call.body != tc.body {
				t.Fatalf("request body not verbatim: %s", call.body)
			}
		})
	}

	// query 显式 ?tenant=2002 → 上游 token 与上下文落 2002。
	rec := mcpTestClient(t, r, http.MethodGet, "/integration/v1/mcp/invocations/inv-1?tenant=2002", "", headers)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	call := fake.lastCall(t)
	if tenant, _ := call.bearer["tenant_id"].(float64); tenant != 2002 {
		t.Fatalf("bearer tenant = %v, want 2002", call.bearer["tenant_id"])
	}

	// 显式未授权 tenant → 403，不触达上游。
	rec = mcpTestClient(t, r, http.MethodGet, "/integration/v1/mcp/services?tenant=9999", "", headers)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestMCPInvokeMissingToken 无平台会话 → 401。
func TestMCPInvokeMissingToken(t *testing.T) {
	r, _, fake := setupMCPEnv(t)
	rec := mcpTestClient(t, r, http.MethodPost, "/integration/v1/mcp/invoke", `{"service_id":"svc-1","tool_name":"tool.x"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if fake.callCount() != 0 {
		t.Fatalf("upstream must not be called without session")
	}
}

// TestMCPInvokeArgumentsMustBeObject 非对象 arguments → 400，不触达上游。
func TestMCPInvokeArgumentsMustBeObject(t *testing.T) {
	r, s, fake := setupMCPEnv(t)
	link := seedKnowledgeSubject(t, s, "subj-1", "ku-1", "2001")
	headers := platformAccessHeaders(t, s, link.ID)

	body := `{"service_id":"svc-1","tool_name":"tool.x","arguments":[1,2]}`
	rec := mcpTestClient(t, r, http.MethodPost, "/integration/v1/mcp/invoke", body, headers)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if fake.callCount() != 0 {
		t.Fatalf("upstream must not be called with invalid arguments")
	}
}
