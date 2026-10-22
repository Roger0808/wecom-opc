package handler

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/accountcenter"
	"weknora-tokendance/pkg/adminauth"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"
	"weknora-tokendance/pkg/syncer"
	"weknora-tokendance/pkg/wecom"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	testAuthKey  = "0123456789abcdef0123456789abcdef-auth"
	testAdminKey = "0123456789abcdef0123456789abcdef-admn"
	testCorpID   = "ww-integration-test"
	testVerifier = "test-verifier-0123456789abcdef"
)

// ---------- 假企微 / 假账户中心 ----------

// fakeWecomIdentity 假企微 OAuth（真实 client 直连 qyapi.weixin.qq.com，无法走 httptest）。
type fakeWecomIdentity struct {
	mu     sync.Mutex
	userID string
	err    error
}

func (f *fakeWecomIdentity) GetUserInfoByCode(code string) (*wecom.OAuthUserResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return &wecom.OAuthUserResponse{UserID: f.userID}, nil
}

// GetUserDetailByID 满足 handler.wecomUserAPI（H6 旧登录回调测试用）。
func (f *fakeWecomIdentity) GetUserDetailByID(userID string) (*wecom.UserDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return &wecom.UserDetail{UserID: userID, Name: "员工-" + userID, Status: 1}, nil
}

func (f *fakeWecomIdentity) set(userID string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userID = userID
	f.err = err
}

// fakeAccountCenter 假统一账户中心 GraphQL（httptest + 真实 accountcenter.Client）。
type fakeAccountCenter struct {
	mu     sync.Mutex
	users  map[string]*accountcenter.SysUser
	errs   map[string]bool
	server *httptest.Server
}

func newFakeAccountCenter(t *testing.T) *fakeAccountCenter {
	t.Helper()
	f := &fakeAccountCenter{
		users: map[string]*accountcenter.SysUser{},
		errs:  map[string]bool{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeAccountCenter) handle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Variables struct {
			UnionNo string `json:"unionNo"`
		} `json:"variables"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	f.mu.Lock()
	user := f.users[req.Variables.UnionNo]
	isErr := f.errs[req.Variables.UnionNo]
	f.mu.Unlock()

	if isErr {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	edges := make([]map[string]interface{}, 0)
	if user != nil {
		edges = append(edges, map[string]interface{}{"node": user})
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"data": map[string]interface{}{
			"sysUsers": map[string]interface{}{"edges": edges},
		},
	})
}

func (f *fakeAccountCenter) client() *accountcenter.Client {
	return accountcenter.NewClient(f.server.URL)
}

func (f *fakeAccountCenter) setUser(unionNo, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[unionNo] = &accountcenter.SysUser{ID: id, UnionNo: unionNo, UserName: "员工-" + id}
}

func (f *fakeAccountCenter) setError(unionNo string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[unionNo] = true
}

// ---------- 测试装配 ----------

func openIntegrationTestSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", uuid.NewString())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("failed to open sqlite in-memory db: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	return db
}

func setupIntegrationTest(t *testing.T, fac *fakeAccountCenter, wk *fakeWecomIdentity) (*gin.Engine, *storage.Storage) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	mainDB := openIntegrationTestSQLite(t)
	stateDB := openIntegrationTestSQLite(t)
	if err := mainDB.AutoMigrate(&model.WeKnoraTenantMember{}); err != nil {
		t.Fatalf("failed to auto migrate main schema: %v", err)
	}
	if err := stateDB.AutoMigrate(
		&model.AuditEvent{},
		&model.IdentityLink{},
		&model.AuthTransaction{},
		&model.Session{},
		&model.ManagedEntitlement{},
	); err != nil {
		t.Fatalf("failed to auto migrate state schema: %v", err)
	}
	s := storage.NewStorageWithDBs(mainDB, stateDB)

	cfg := &config.Config{
		Server: config.ServerConfig{ExternalBaseURL: "http://127.0.0.1:3002"}, // 与 doPostJSON/doGet 的 req.Host 一致
		WeKnora:  config.WeKnoraConfig{BaseURL: "http://weknora.internal"},
		WeCom:  config.WeComConfig{CorpID: testCorpID},
		Admin:  config.AdminConfig{Key: testAdminKey},
		Auth:   config.AuthConfig{Key: testAuthKey},
	}
	ih := NewIntegrationHandler(cfg, wk, fac.client(), s)
	h := NewHandler(cfg, nil, nil, nil, nil, s) // 仅复用 LoginRedirect 旧入口

	r := gin.New()
	v1 := r.Group("/integration/v1")
	{
		v1.POST("/auth/authorize", ih.Authorize)
		v1.GET("/auth/callback", ih.Callback)
		v1.POST("/auth/token", ih.Token)
		v1.POST("/auth/refresh", ih.Refresh)
		v1.POST("/auth/logout", ih.Logout)
		v1.GET("/session", ih.Session)
		v1.POST("/auth/code-exchange", ih.CodeExchange)
	}
	r.GET("/wecom/login", h.LoginRedirect)
	return r, s
}

// ---------- HTTP 助手 ----------

func doPostJSON(t *testing.T, r *gin.Engine, path string, body interface{}, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	req.Host = "127.0.0.1:3002" // 模拟网关对外地址，authorize_url 同源回跳校验依赖它
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func doGet(t *testing.T, r *gin.Engine, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "127.0.0.1:3002"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response body %q: %v", w.Body.String(), err)
	}
	return out
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	body := decodeBody(t, w)
	errObj, ok := body["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected error envelope, got %v", body)
	}
	code, _ := errObj["code"].(string)
	if _, has := errObj["request_id"].(string); !has {
		t.Fatalf("error envelope missing request_id: %v", errObj)
	}
	return code
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func insertIdentityLink(t *testing.T, s *storage.Storage, subject, knowledgeUserID string) *model.IdentityLink {
	t.Helper()
	link := &model.IdentityLink{
		IdentitySource:   "wecom",
		OrganizationID:   testCorpID,
		ImmutableSubject: subject,
		KnowledgeUserID:  knowledgeUserID,
		Status:           "active",
	}
	if err := s.InsertIdentityLink(context.Background(), link); err != nil {
		t.Fatalf("InsertIdentityLink: %v", err)
	}
	return link
}

func insertEntitlement(t *testing.T, s *storage.Storage, subject, tenant, capability string, authzVersion int64, revoked bool) {
	t.Helper()
	ent := &model.ManagedEntitlement{
		Subject:      subject,
		TenantID:     tenant,
		Capability:   capability,
		Source:       "sync",
		AuthzVersion: authzVersion,
	}
	if revoked {
		now := time.Now()
		ent.RevokedAt = &now
	}
	if err := s.InsertManagedEntitlement(context.Background(), ent); err != nil {
		t.Fatalf("InsertManagedEntitlement: %v", err)
	}
}

// authorizeCallbackChain 走通 authorize → 企微回调，返回一次性授权码。
func authorizeCallbackChain(t *testing.T, r *gin.Engine, clientID, redirectURI, state, challenge, device string) (loginTxnID, code string) {
	t.Helper()
	w := doPostJSON(t, r, "/integration/v1/auth/authorize", gin.H{
		"client_id":      clientID,
		"redirect_uri":   redirectURI,
		"state":          state,
		"code_challenge": challenge,
		"device":         device,
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("authorize = %d, body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	loginTxnID, _ = body["login_txn_id"].(string)
	if loginTxnID == "" {
		t.Fatalf("authorize missing login_txn_id: %v", body)
	}
	authorizeURL, _ := body["authorize_url"].(string)
	if !strings.Contains(authorizeURL, "/wecom/login") || !strings.Contains(authorizeURL, "redirect_uri=") {
		t.Fatalf("authorize_url should point to existing wecom login entry: %v", authorizeURL)
	}

	cb := doGet(t, r, "/integration/v1/auth/callback?code=wc-code&state="+url.QueryEscape(state), nil)
	if cb.Code != http.StatusFound {
		t.Fatalf("callback = %d, body=%s", cb.Code, cb.Body.String())
	}
	loc, err := url.Parse(cb.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse callback location: %v", err)
	}
	if loc.Query().Get("state") != state {
		t.Fatalf("state not echoed back: %s", loc.RawQuery)
	}
	code = loc.Query().Get("code")
	if code == "" {
		t.Fatalf("callback missing code: %s", loc.RawQuery)
	}
	return loginTxnID, code
}

func tokenBody(code, clientID, device, audience string) gin.H {
	return gin.H{
		"grant_type":    "authorization_code",
		"code":          code,
		"code_verifier": testVerifier,
		"client_id":     clientID,
		"device":        device,
		"audience":      audience,
	}
}

func parsePlatformClaims(t *testing.T, raw string) jwt.MapClaims {
	t.Helper()
	claims := jwt.MapClaims{}
	tok, err := jwt.ParseWithClaims(raw, claims, func(tk *jwt.Token) (interface{}, error) {
		return []byte(testAuthKey), nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !tok.Valid {
		t.Fatalf("parse platform token: %v", err)
	}
	return claims
}

func serviceToken(t *testing.T, role adminauth.Role, tenantID uint64) string {
	t.Helper()
	raw, err := adminauth.IssueToken([]byte(testAdminKey), "svc-code-backend", role, tenantID, time.Hour)
	if err != nil {
		t.Fatalf("issue service token: %v", err)
	}
	return raw
}

func auditActions(t *testing.T, s *storage.Storage) []string {
	t.Helper()
	events, err := s.ListAuditEvents(context.Background())
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	actions := make([]string, 0, len(events))
	for _, e := range events {
		actions = append(actions, e.Action)
	}
	sort.Strings(actions)
	return actions
}

func hasAction(actions []string, want string) bool {
	for _, a := range actions {
		if a == want {
			return true
		}
	}
	return false
}

// ---------- 用例 ----------

// TestIntegrationFlow_WorkDesktopFullChain 走通 authorize→callback→token→session→
// refresh→复用检测→logout 全链路（假企微 + 假账户中心，sqlite 内存状态库）。
func TestIntegrationFlow_WorkDesktopFullChain(t *testing.T) {
	fac := newFakeAccountCenter(t)
	wk := &fakeWecomIdentity{}
	r, s := setupIntegrationTest(t, fac, wk)

	fac.setUser("union-1", "subj-1")
	wk.set("union-1", nil)
	link := insertIdentityLink(t, s, "subj-1", "ku-1")
	insertEntitlement(t, s, "subj-1", "tenant-10002", "doc.read", 3, false)
	insertEntitlement(t, s, "subj-1", "tenant-10001", "skill.install", 7, false)
	insertEntitlement(t, s, "subj-1", "tenant-99999", "ghost.view", 1, true) // 已撤销不计入

	const redirectURI = "http://127.0.0.1:5173/callback"

	// 1) authorize：白名单 client、回环 redirect_uri、授权 URL 指向既有企微入口。
	aw := doPostJSON(t, r, "/integration/v1/auth/authorize", gin.H{
		"client_id":      "work-desktop",
		"redirect_uri":   redirectURI,
		"state":          "st-1",
		"code_challenge": pkceChallenge(testVerifier),
		"device":         "dev-1",
	}, nil)
	if aw.Code != http.StatusOK {
		t.Fatalf("authorize = %d, body=%s", aw.Code, aw.Body.String())
	}
	authorizeURL, _ := decodeBody(t, aw)["authorize_url"].(string)
	if !strings.HasPrefix(authorizeURL, "http://") || !strings.Contains(authorizeURL, "/wecom/login") {
		t.Fatalf("bad authorize_url: %s", authorizeURL)
	}

	// authorize_url 跟随到既有 /wecom/login：redirect_uri 覆盖生效且 state 透传。
	loginURL, _ := url.Parse(authorizeURL)
	lw := doGet(t, r, loginURL.RequestURI(), nil)
	if lw.Code != http.StatusFound {
		t.Fatalf("/wecom/login = %d, body=%s", lw.Code, lw.Body.String())
	}
	loginLoc := lw.Header().Get("Location")
	if !strings.Contains(loginLoc, "redirect_uri=") ||
		!strings.Contains(loginLoc, url.QueryEscape("/integration/v1/auth/callback")) ||
		!strings.Contains(loginLoc, "state=st-1") {
		t.Fatalf("wecom login location missing txn params: %s", loginLoc)
	}

	// 2) callback：302 回 redirect_uri 携带一次性授权码与 state。
	cb := doGet(t, r, "/integration/v1/auth/callback?code=wc-code&state=st-1", nil)
	if cb.Code != http.StatusFound {
		t.Fatalf("callback = %d, body=%s", cb.Code, cb.Body.String())
	}
	cbLoc, _ := url.Parse(cb.Header().Get("Location"))
	if !strings.HasPrefix(cbLoc.String(), redirectURI+"?") {
		t.Fatalf("callback should redirect to whitelisted redirect_uri: %s", cbLoc.String())
	}
	if cbLoc.Query().Get("state") != "st-1" {
		t.Fatalf("state mismatch: %s", cbLoc.RawQuery)
	}
	code := cbLoc.Query().Get("code")
	if code == "" {
		t.Fatalf("code missing in callback redirect")
	}

	// 3) token：正确 verifier + 受众，签发 15min access + 14d refresh。
	tw := doPostJSON(t, r, "/integration/v1/auth/token", tokenBody(code, "work-desktop", "dev-1", "work"), nil)
	if tw.Code != http.StatusOK {
		t.Fatalf("token = %d, body=%s", tw.Code, tw.Body.String())
	}
	tokenResp := decodeBody(t, tw)
	if tokenResp["expires_in"].(float64) != 900 {
		t.Fatalf("expires_in = %v", tokenResp["expires_in"])
	}
	access, _ := tokenResp["access_token"].(string)
	refresh, _ := tokenResp["refresh_token"].(string)
	if access == "" || refresh == "" {
		t.Fatalf("token response missing tokens: %v", tokenResp)
	}
	accessClaims := parsePlatformClaims(t, access)
	if accessClaims["sub"] != link.ID || accessClaims["aud"] != "work" || accessClaims["type"] != "access" {
		t.Fatalf("access claims mismatch: %v", accessClaims)
	}
	refreshClaims := parsePlatformClaims(t, refresh)
	if refreshClaims["sid"] != accessClaims["sid"] || refreshClaims["type"] != "refresh" {
		t.Fatalf("refresh claims mismatch: %v", refreshClaims)
	}

	// 4) GET /session：主体视图 + active 空间 + 能力 + 授权版本。
	sw := doGet(t, r, "/integration/v1/session", map[string]string{"Authorization": "Bearer " + access})
	if sw.Code != http.StatusOK {
		t.Fatalf("session = %d, body=%s", sw.Code, sw.Body.String())
	}
	sessionResp := decodeBody(t, sw)
	if sessionResp["identity_link_id"] != link.ID || sessionResp["subject"] != "subj-1" {
		t.Fatalf("session identity mismatch: %v", sessionResp)
	}
	if fmt.Sprint(sessionResp["spaces"]) != "[tenant-10001 tenant-10002]" {
		t.Fatalf("spaces = %v", sessionResp["spaces"])
	}
	if sessionResp["authz_version"].(float64) != 7 {
		t.Fatalf("authz_version = %v", sessionResp["authz_version"])
	}
	if fmt.Sprint(sessionResp["capabilities"]) != "[doc.read skill.install]" {
		t.Fatalf("capabilities = %v", sessionResp["capabilities"])
	}

	// 5) refresh 轮换：拿到新对，旧 refresh 呈上触发复用检测、整个 family 被撤销。
	rw := doPostJSON(t, r, "/integration/v1/auth/refresh", gin.H{"refresh_token": refresh}, nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("refresh = %d, body=%s", rw.Code, rw.Body.String())
	}
	newRefresh, _ := decodeBody(t, rw)["refresh_token"].(string)
	if newRefresh == "" || newRefresh == refresh {
		t.Fatalf("refresh rotation returned same token")
	}

	reuse := doPostJSON(t, r, "/integration/v1/auth/refresh", gin.H{"refresh_token": refresh}, nil)
	if reuse.Code != http.StatusUnauthorized || errorCode(t, reuse) != "REFRESH_REUSE_DETECTED" {
		t.Fatalf("reuse detection = %d, body=%s", reuse.Code, reuse.Body.String())
	}
	after := doPostJSON(t, r, "/integration/v1/auth/refresh", gin.H{"refresh_token": newRefresh}, nil)
	if after.Code != http.StatusUnauthorized || errorCode(t, after) != "SESSION_REVOKED" {
		t.Fatalf("refresh after family revoke = %d, body=%s", after.Code, after.Body.String())
	}

	// 6) 授权码重放：同一 code 再次兑换被拒绝。
	replay := doPostJSON(t, r, "/integration/v1/auth/token", tokenBody(code, "work-desktop", "dev-1", "work"), nil)
	if replay.Code != http.StatusBadRequest || errorCode(t, replay) != "AUTH_CODE_REPLAYED" {
		t.Fatalf("code replay = %d, body=%s", replay.Code, replay.Body.String())
	}

	actions := auditActions(t, s)
	for _, want := range []string{
		"integration.auth.authorize.created",
		"integration.auth.code.issued",
		"integration.auth.token.issued",
		"integration.auth.session.viewed",
		"integration.auth.refresh.rotated",
		"integration.auth.refresh.reuse_detected",
	} {
		if !hasAction(actions, want) {
			t.Errorf("audit missing %s (got %v)", want, actions)
		}
	}
}

// TestIntegrationFlow_LogoutRevokesFamily 撤销整个会话族，access/refresh 同时失效。
func TestIntegrationFlow_LogoutRevokesFamily(t *testing.T) {
	fac := newFakeAccountCenter(t)
	wk := &fakeWecomIdentity{}
	r, s := setupIntegrationTest(t, fac, wk)

	fac.setUser("union-1", "subj-1")
	wk.set("union-1", nil)
	insertIdentityLink(t, s, "subj-1", "ku-1")

	_, code := authorizeCallbackChain(t, r, "work-desktop", "http://127.0.0.1:5173/callback", "st-lo", pkceChallenge(testVerifier), "dev-lo")
	tw := doPostJSON(t, r, "/integration/v1/auth/token", tokenBody(code, "work-desktop", "dev-lo", "work"), nil)
	if tw.Code != http.StatusOK {
		t.Fatalf("token = %d, body=%s", tw.Code, tw.Body.String())
	}
	tokenResp := decodeBody(t, tw)
	access, _ := tokenResp["access_token"].(string)
	refresh, _ := tokenResp["refresh_token"].(string)

	lw := doPostJSON(t, r, "/integration/v1/auth/logout", gin.H{"refresh_token": refresh}, nil)
	if lw.Code != http.StatusOK {
		t.Fatalf("logout = %d, body=%s", lw.Code, lw.Body.String())
	}
	if decodeBody(t, lw)["revoked"].(float64) != 1 {
		t.Fatalf("logout revoked = %v", decodeBody(t, lw))
	}

	sw := doGet(t, r, "/integration/v1/session", map[string]string{"Authorization": "Bearer " + access})
	if sw.Code != http.StatusUnauthorized || errorCode(t, sw) != "SESSION_REVOKED" {
		t.Fatalf("session after logout = %d, body=%s", sw.Code, sw.Body.String())
	}
	rw := doPostJSON(t, r, "/integration/v1/auth/refresh", gin.H{"refresh_token": refresh}, nil)
	if rw.Code != http.StatusUnauthorized || errorCode(t, rw) != "SESSION_REVOKED" {
		t.Fatalf("refresh after logout = %d, body=%s", rw.Code, rw.Body.String())
	}

	// access_token 也能定位会话族并完成撤销。
	_, code2 := authorizeCallbackChain(t, r, "work-desktop", "http://127.0.0.1:5173/callback", "st-lo2", pkceChallenge(testVerifier), "dev-lo2")
	tw2 := doPostJSON(t, r, "/integration/v1/auth/token", tokenBody(code2, "work-desktop", "dev-lo2", "work"), nil)
	access2, _ := decodeBody(t, tw2)["access_token"].(string)
	lw2 := doPostJSON(t, r, "/integration/v1/auth/logout", gin.H{"access_token": access2}, nil)
	if lw2.Code != http.StatusOK || decodeBody(t, lw2)["revoked"].(float64) != 1 {
		t.Fatalf("logout by access = %d, body=%s", lw2.Code, lw2.Body.String())
	}
}

// TestIntegrationFlow_CodeBackendExchange code-desktop 受众：兑换材料一次性消费，不签发平台令牌。
// code-exchange 增补契约：请求体可带 code_verifier（事务存了 challenge 时必传，S256 复核）；
// 响应 spaces 条目为 {tenant_id, role}（role 直出 knowledge 成员角色）。
func TestIntegrationFlow_CodeBackendExchange(t *testing.T) {
	fac := newFakeAccountCenter(t)
	wk := &fakeWecomIdentity{}
	r, s := setupIntegrationTest(t, fac, wk)

	fac.setUser("union-1", "subj-1")
	wk.set("union-1", nil)
	link := insertIdentityLink(t, s, "subj-1", "ku-1")
	insertEntitlement(t, s, "subj-1", "10001", "skill.install", 5, false)
	insertEntitlement(t, s, "subj-1", "10001", "doc.read", 6, false) // 同 tenant 多 capability：spaces 去重
	insertEntitlement(t, s, "subj-1", "10002", "doc.read", 7, false)
	// knowledge 侧成员角色：ku-1 在 10001 为 admin，10002 无成员关系（role 空串）
	if err := s.DB().WithContext(context.Background()).Create(&model.WeKnoraTenantMember{
		UserID: "ku-1", TenantID: 10001, Role: "admin", Status: "active", JoinedAt: time.Now(),
	}).Error; err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	_, code := authorizeCallbackChain(t, r, "code-desktop", "http://127.0.0.1:6262/callback", "st-cb", pkceChallenge(testVerifier), "dev-cb")
	tw := doPostJSON(t, r, "/integration/v1/auth/token", tokenBody(code, "code-desktop", "dev-cb", "code-backend"), nil)
	if tw.Code != http.StatusOK {
		t.Fatalf("token = %d, body=%s", tw.Code, tw.Body.String())
	}
	tokenResp := decodeBody(t, tw)
	exchange, _ := tokenResp["exchange_token"].(string)
	if exchange == "" {
		t.Fatalf("code-backend token response missing exchange_token: %v", tokenResp)
	}
	if _, hasAccess := tokenResp["access_token"]; hasAccess {
		t.Fatalf("code-backend must not issue platform access token: %v", tokenResp)
	}
	exClaims := parsePlatformClaims(t, exchange)
	if exClaims["aud"] != "code-backend" || exClaims["type"] != "exchange" || exClaims["sub"] != link.ID {
		t.Fatalf("exchange claims mismatch: %v", exClaims)
	}

	// 服务身份消费：system_admin 角色放行，返回主体视图且不返回平台令牌。
	headers := map[string]string{"Authorization": "Bearer " + serviceToken(t, adminauth.RoleSystemAdmin, 0)}

	// PKCE 复核：缺 code_verifier（事务已存 challenge）→ 400 AUTH_STATE_MISMATCH
	missingVerifier := doPostJSON(t, r, "/integration/v1/auth/code-exchange", gin.H{"exchange_token": exchange}, headers)
	if missingVerifier.Code != http.StatusBadRequest || errorCode(t, missingVerifier) != "AUTH_STATE_MISMATCH" {
		t.Fatalf("missing code_verifier = %d, body=%s", missingVerifier.Code, missingVerifier.Body.String())
	}
	// 错 verifier → 400 AUTH_STATE_MISMATCH（材料不消费，后续正确调用仍可成功）
	wrongVerifier := doPostJSON(t, r, "/integration/v1/auth/code-exchange", gin.H{"exchange_token": exchange, "code_verifier": "wrong-verifier"}, headers)
	if wrongVerifier.Code != http.StatusBadRequest || errorCode(t, wrongVerifier) != "AUTH_STATE_MISMATCH" {
		t.Fatalf("wrong code_verifier = %d, body=%s", wrongVerifier.Code, wrongVerifier.Body.String())
	}

	xw := doPostJSON(t, r, "/integration/v1/auth/code-exchange", gin.H{"exchange_token": exchange, "code_verifier": testVerifier}, headers)
	if xw.Code != http.StatusOK {
		t.Fatalf("code-exchange = %d, body=%s", xw.Code, xw.Body.String())
	}
	xResp := decodeBody(t, xw)
	if xResp["subject"] != "subj-1" || xResp["identity_link_id"] != link.ID || xResp["knowledge_user_id"] != "ku-1" {
		t.Fatalf("code-exchange identity mismatch: %v", xResp)
	}
	if xResp["code_user_id"] != nil {
		t.Fatalf("code_user_id should be null before workspace linking: %v", xResp)
	}
	// spaces：去重后 [10001(admin) 10002("")]，role 直出 knowledge 成员角色
	spaces, ok := xResp["spaces"].([]interface{})
	if !ok || len(spaces) != 2 {
		t.Fatalf("spaces = %v, want 2 deduped entries", xResp["spaces"])
	}
	first, _ := spaces[0].(map[string]interface{})
	second, _ := spaces[1].(map[string]interface{})
	if first["tenant_id"] != "10001" || first["role"] != "admin" {
		t.Fatalf("spaces[0] = %v, want {10001 admin}", spaces[0])
	}
	if second["tenant_id"] != "10002" || second["role"] != "" {
		t.Fatalf("spaces[1] = %v, want {10002 \"\"}", spaces[1])
	}
	if xResp["authz_version"].(float64) != 7 {
		t.Fatalf("authz_version = %v", xResp["authz_version"])
	}
	if _, hasAccess := xResp["access_token"]; hasAccess {
		t.Fatalf("code-exchange must not return platform access token")
	}

	// 一次性消费：重放被拒绝（即使带对 verifier）。
	replay := doPostJSON(t, r, "/integration/v1/auth/code-exchange", gin.H{"exchange_token": exchange, "code_verifier": testVerifier}, headers)
	if replay.Code != http.StatusBadRequest || errorCode(t, replay) != "AUTH_CODE_REPLAYED" {
		t.Fatalf("exchange replay = %d, body=%s", replay.Code, replay.Body.String())
	}

	// 角色不足：dept_admin 服务 token 被拒。
	badRole := doPostJSON(t, r, "/integration/v1/auth/code-exchange", gin.H{"exchange_token": exchange, "code_verifier": testVerifier},
		map[string]string{"Authorization": "Bearer " + serviceToken(t, adminauth.RoleDeptAdmin, 10001)})
	if badRole.Code != http.StatusForbidden {
		t.Fatalf("code-exchange dept_admin = %d, body=%s", badRole.Code, badRole.Body.String())
	}

	// 兑换材料不能当 refresh 用（受众/类型绑定）。
	misuse := doPostJSON(t, r, "/integration/v1/auth/refresh", gin.H{"refresh_token": exchange}, nil)
	if misuse.Code != http.StatusUnauthorized {
		t.Fatalf("exchange as refresh = %d, body=%s", misuse.Code, misuse.Body.String())
	}
}

// TestIntegrationFlow_TokenBindingRejections 错 verifier / 跨受众 / 错 client / 错 device。
func TestIntegrationFlow_TokenBindingRejections(t *testing.T) {
	fac := newFakeAccountCenter(t)
	wk := &fakeWecomIdentity{}
	r, s := setupIntegrationTest(t, fac, wk)

	fac.setUser("union-1", "subj-1")
	wk.set("union-1", nil)
	insertIdentityLink(t, s, "subj-1", "ku-1")

	cases := []struct {
		name   string
		mutate func(gin.H)
	}{
		{"wrong verifier", func(b gin.H) { b["code_verifier"] = "wrong-verifier" }},
		{"cross audience", func(b gin.H) { b["audience"] = "code-backend" }},
		{"wrong client", func(b gin.H) { b["client_id"] = "code-desktop" }},
		{"wrong device", func(b gin.H) { b["device"] = "other-device" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, code := authorizeCallbackChain(t, r, "work-desktop", "http://127.0.0.1:5173/callback",
				"st-"+tc.name, pkceChallenge(testVerifier), "dev-1")
			body := tokenBody(code, "work-desktop", "dev-1", "work")
			tc.mutate(body)
			w := doPostJSON(t, r, "/integration/v1/auth/token", body, nil)
			if w.Code != http.StatusBadRequest || errorCode(t, w) != "AUTH_STATE_MISMATCH" {
				t.Fatalf("%s = %d, body=%s", tc.name, w.Code, w.Body.String())
			}
		})
	}
}

// TestIntegrationFlow_ConcurrentTokenRedemption 并发兑换同一授权码，只放行一个。
func TestIntegrationFlow_ConcurrentTokenRedemption(t *testing.T) {
	fac := newFakeAccountCenter(t)
	wk := &fakeWecomIdentity{}
	r, s := setupIntegrationTest(t, fac, wk)

	fac.setUser("union-1", "subj-1")
	wk.set("union-1", nil)
	insertIdentityLink(t, s, "subj-1", "ku-1")

	_, code := authorizeCallbackChain(t, r, "work-desktop", "http://127.0.0.1:5173/callback", "st-conc", pkceChallenge(testVerifier), "dev-1")

	const goroutines = 8
	results := make(chan int, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := doPostJSON(t, r, "/integration/v1/auth/token", tokenBody(code, "work-desktop", "dev-1", "work"), nil)
			results <- w.Code
		}()
	}
	wg.Wait()
	close(results)

	ok, rejected := 0, 0
	for status := range results {
		switch status {
		case http.StatusOK:
			ok++
		case http.StatusBadRequest:
			rejected++
		default:
			t.Fatalf("unexpected token status %d", status)
		}
	}
	if ok != 1 || rejected != goroutines-1 {
		t.Fatalf("concurrent redemption: ok=%d rejected=%d", ok, rejected)
	}
}

// TestIntegrationCallback_Rejections 未供给 / 冲突 / 账户中心失败 / 企微失败 / 未知 state。
func TestIntegrationCallback_Rejections(t *testing.T) {
	fac := newFakeAccountCenter(t)
	wk := &fakeWecomIdentity{}
	r, s := setupIntegrationTest(t, fac, wk)

	const redirectURI = "http://127.0.0.1:5173/callback"
	challenge := pkceChallenge(testVerifier)

	callbackError := func(t *testing.T, state string) string {
		t.Helper()
		cb := doGet(t, r, "/integration/v1/auth/callback?code=wc-code&state="+url.QueryEscape(state), nil)
		if cb.Code != http.StatusFound {
			t.Fatalf("callback = %d, body=%s", cb.Code, cb.Body.String())
		}
		loc, _ := url.Parse(cb.Header().Get("Location"))
		if !strings.HasPrefix(loc.String(), redirectURI+"?") {
			t.Fatalf("rejection must redirect to whitelisted redirect_uri: %s", loc.String())
		}
		return loc.Query().Get("error")
	}

	t.Run("subject not provisioned", func(t *testing.T) {
		fac.setUser("union-np", "subj-np")
		wk.set("union-np", nil)
		// 不插 identity link，走显式 authorize + 错误断言
		doPostJSON(t, r, "/integration/v1/auth/authorize", gin.H{
			"client_id": "work-desktop", "redirect_uri": redirectURI, "state": "st-np-2",
			"code_challenge": challenge, "device": "dev-1",
		}, nil)
		if got := callbackError(t, "st-np-2"); got != "SUBJECT_NOT_PROVISIONED" {
			t.Fatalf("not provisioned error = %q", got)
		}
	})

	// 冲突：同一 knowledge_user_id 被两个 active 主体占用。
	fac.setUser("union-c1", "subj-c1")
	wk.set("union-c1", nil)
	insertIdentityLink(t, s, "subj-c1", "ku-shared")
	insertIdentityLink(t, s, "subj-c2", "ku-shared")
	doPostJSON(t, r, "/integration/v1/auth/authorize", gin.H{
		"client_id": "work-desktop", "redirect_uri": redirectURI, "state": "st-cf",
		"code_challenge": challenge, "device": "dev-1",
	}, nil)
	if got := callbackError(t, "st-cf"); got != "SUBJECT_CONFLICT" {
		t.Fatalf("conflict error = %q", got)
	}

	// 账户中心不可用：事务 failed，错误码 ACCOUNT_CENTER_UNAVAILABLE。
	fac.setUser("union-err", "subj-err")
	fac.setError("union-err")
	wk.set("union-err", nil)
	doPostJSON(t, r, "/integration/v1/auth/authorize", gin.H{
		"client_id": "work-desktop", "redirect_uri": redirectURI, "state": "st-err",
		"code_challenge": challenge, "device": "dev-1",
	}, nil)
	if got := callbackError(t, "st-err"); got != "ACCOUNT_CENTER_UNAVAILABLE" {
		t.Fatalf("account center error = %q", got)
	}

	// 企微 code 换 UserID 失败：映射 WECOM_EXCHANGE_FAILED（L2：与账户中心失败区分），不重试不降级。
	fac.setUser("union-wx", "subj-wx")
	wk.set("union-wx", errors.New("wecom getuserinfo failed"))
	doPostJSON(t, r, "/integration/v1/auth/authorize", gin.H{
		"client_id": "work-desktop", "redirect_uri": redirectURI, "state": "st-wx",
		"code_challenge": challenge, "device": "dev-1",
	}, nil)
	if got := callbackError(t, "st-wx"); got != "WECOM_EXCHANGE_FAILED" {
		t.Fatalf("wecom error = %q, want WECOM_EXCHANGE_FAILED", got)
	}

	// 未知 state：JSON 400 而非重定向。
	wk.set("union-wx", nil)
	cb := doGet(t, r, "/integration/v1/auth/callback?code=wc-code&state=st-unknown", nil)
	if cb.Code != http.StatusBadRequest || errorCode(t, cb) != "AUTH_STATE_MISMATCH" {
		t.Fatalf("unknown state = %d, body=%s", cb.Code, cb.Body.String())
	}
}

// TestIntegrationFlow_ExpiredCode 授权码过期后兑换返回 AUTH_CODE_REPLAYED。
func TestIntegrationFlow_ExpiredCode(t *testing.T) {
	fac := newFakeAccountCenter(t)
	wk := &fakeWecomIdentity{}
	r, s := setupIntegrationTest(t, fac, wk)

	fac.setUser("union-1", "subj-1")
	wk.set("union-1", nil)
	insertIdentityLink(t, s, "subj-1", "ku-1")

	loginTxnID, code := authorizeCallbackChain(t, r, "work-desktop", "http://127.0.0.1:5173/callback", "st-exp", pkceChallenge(testVerifier), "dev-1")
	if err := s.StateDB().Exec("UPDATE auth_transactions SET expires_at = ? WHERE login_txn_id = ?",
		time.Now().Add(-time.Minute), loginTxnID).Error; err != nil {
		t.Fatalf("force expire txn: %v", err)
	}

	w := doPostJSON(t, r, "/integration/v1/auth/token", tokenBody(code, "work-desktop", "dev-1", "work"), nil)
	if w.Code != http.StatusBadRequest || errorCode(t, w) != "AUTH_CODE_REPLAYED" {
		t.Fatalf("expired code = %d, body=%s", w.Code, w.Body.String())
	}
}

// TestIntegrationAuthorize_Validation client_id 白名单与回环 redirect_uri 校验。
func TestIntegrationAuthorize_Validation(t *testing.T) {
	fac := newFakeAccountCenter(t)
	wk := &fakeWecomIdentity{}
	r, _ := setupIntegrationTest(t, fac, wk)

	valid := gin.H{
		"client_id":      "work-desktop",
		"redirect_uri":   "http://127.0.0.1:5173/callback",
		"state":          "st-v",
		"code_challenge": pkceChallenge(testVerifier),
		"device":         "dev-1",
	}
	cases := []struct {
		name   string
		mutate func(gin.H)
	}{
		{"unknown client", func(b gin.H) { b["client_id"] = "evil-desktop" }},
		{"non loopback host", func(b gin.H) { b["redirect_uri"] = "http://example.com/callback" }},
		{"localhost name", func(b gin.H) { b["redirect_uri"] = "http://localhost:5173/callback" }},
		{"missing port", func(b gin.H) { b["redirect_uri"] = "http://127.0.0.1/callback" }},
		{"https scheme", func(b gin.H) { b["redirect_uri"] = "https://127.0.0.1:5173/callback" }},
		{"wrong path", func(b gin.H) { b["redirect_uri"] = "http://127.0.0.1:5173/other" }},
		{"missing challenge", func(b gin.H) { b["code_challenge"] = "" }},
		{"missing state", func(b gin.H) { b["state"] = "" }}, // L3：state 强制非空
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := gin.H{}
			for k, v := range valid {
				body[k] = v
			}
			tc.mutate(body)
			w := doPostJSON(t, r, "/integration/v1/auth/authorize", body, nil)
			if w.Code != http.StatusBadRequest || errorCode(t, w) != "AUTH_STATE_MISMATCH" {
				t.Fatalf("%s = %d, body=%s", tc.name, w.Code, w.Body.String())
			}
		})
	}

	// code-desktop 是合法白名单客户端。
	w := doPostJSON(t, r, "/integration/v1/auth/authorize", gin.H{
		"client_id": "code-desktop", "redirect_uri": "http://127.0.0.1:6262/callback",
		"state": "st-v2", "code_challenge": pkceChallenge(testVerifier), "device": "dev-1",
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code-desktop authorize = %d, body=%s", w.Code, w.Body.String())
	}
}

// TestIntegrationSession_AuthFailures refresh 令牌 / 伪造令牌访问 session 一律 401。
func TestIntegrationSession_AuthFailures(t *testing.T) {
	fac := newFakeAccountCenter(t)
	wk := &fakeWecomIdentity{}
	r, s := setupIntegrationTest(t, fac, wk)

	fac.setUser("union-1", "subj-1")
	wk.set("union-1", nil)
	insertIdentityLink(t, s, "subj-1", "ku-1")

	_, code := authorizeCallbackChain(t, r, "work-desktop", "http://127.0.0.1:5173/callback", "st-sf", pkceChallenge(testVerifier), "dev-1")
	tw := doPostJSON(t, r, "/integration/v1/auth/token", tokenBody(code, "work-desktop", "dev-1", "work"), nil)
	tokenResp := decodeBody(t, tw)
	refresh, _ := tokenResp["refresh_token"].(string)

	wrongType := doGet(t, r, "/integration/v1/session", map[string]string{"Authorization": "Bearer " + refresh})
	if wrongType.Code != http.StatusUnauthorized || errorCode(t, wrongType) != "SESSION_REVOKED" {
		t.Fatalf("refresh as access = %d, body=%s", wrongType.Code, wrongType.Body.String())
	}

	noHeader := doGet(t, r, "/integration/v1/session", nil)
	if noHeader.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer = %d", noHeader.Code)
	}

	fake := doGet(t, r, "/integration/v1/session", map[string]string{"Authorization": "Bearer not-a-jwt"})
	if fake.Code != http.StatusUnauthorized || errorCode(t, fake) != "SESSION_REVOKED" {
		t.Fatalf("forged token = %d, body=%s", fake.Code, fake.Body.String())
	}
}

// TestLoginRedirect_IntegrationOverride /wecom/login 支持受控 redirect_uri 覆盖且不影响旧行为。
func TestLoginRedirect_IntegrationOverride(t *testing.T) {
	fac := newFakeAccountCenter(t)
	wk := &fakeWecomIdentity{}
	r, s := setupIntegrationTest(t, fac, wk)

	// 旧行为：无 redirect_uri 参数时回跳 /wecom/callback。
	old := doGet(t, r, "/wecom/login?state=legacy", nil)
	if old.Code != http.StatusFound || !strings.Contains(old.Header().Get("Location"), "%2Fwecom%2Fcallback") {
		t.Fatalf("legacy login redirect = %d, loc=%s", old.Code, old.Header().Get("Location"))
	}

	// 覆盖：同源集成回调地址被透传给企微。
	override := "http://127.0.0.1:3002/integration/v1/auth/callback"
	ov := doGet(t, r, "/wecom/login?redirect_uri="+url.QueryEscape(override)+"&state=st-1", nil)
	if ov.Code != http.StatusFound {
		t.Fatalf("override login = %d, body=%s", ov.Code, ov.Body.String())
	}
	loc := ov.Header().Get("Location")
	if !strings.Contains(loc, "redirect_uri="+url.QueryEscape(override)) {
		t.Fatalf("override not forwarded: %s", loc)
	}

	// 跨站覆盖：400，且不会落到企微。
	bad := doGet(t, r, "/wecom/login?redirect_uri="+url.QueryEscape("http://evil.example.com/integration/v1/auth/callback"), nil)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("evil override = %d", bad.Code)
	}

	// 既有 knowledge UI 回跳：与 WeKnora 规范地址同源的 oidc 回调放行（部署实测回归）。
	legacyURI := "http://weknora.internal/api/v1/auth/oidc/callback"
	lgRes := doGet(t, r, "/wecom/login?redirect_uri="+url.QueryEscape(legacyURI)+"&state=st-legacy", nil)
	if lgRes.Code != http.StatusFound {
		t.Fatalf("legacy knowledge override = %d, body=%s", lgRes.Code, lgRes.Body.String())
	}
	if !strings.Contains(lgRes.Header().Get("Location"), "redirect_uri="+url.QueryEscape(legacyURI)) {
		t.Fatalf("legacy override not forwarded: %s", lgRes.Header().Get("Location"))
	}

	// WeKnora 同源但路径不对：仍 400。
	wrongLegacy := doGet(t, r, "/wecom/login?redirect_uri="+url.QueryEscape("http://weknora.internal/evil"), nil)
	if wrongLegacy.Code != http.StatusBadRequest {
		t.Fatalf("wrong-path legacy override = %d", wrongLegacy.Code)
	}

	// 同源但路径不对：同样 400。
	wrongPath := doGet(t, r, "/wecom/login?redirect_uri="+url.QueryEscape("http://127.0.0.1:3002/other"), nil)
	if wrongPath.Code != http.StatusBadRequest {
		t.Fatalf("wrong-path override = %d", wrongPath.Code)
	}

	// H2：伪造 Host / X-Forwarded-Host 不影响判定——同源锚是 server.external_base_url。
	// 请求头伪造成 evil 域名 + 同域名 override：仍须 400（不走请求头同源）。
	forged := doGet(t, r, "/wecom/login?redirect_uri="+url.QueryEscape("http://evil.example.com/integration/v1/auth/callback"),
		map[string]string{"X-Forwarded-Host": "evil.example.com"})
	if forged.Code != http.StatusBadRequest {
		t.Fatalf("forged X-Forwarded-Host override = %d, want 400", forged.Code)
	}
	// 伪造请求头不影响缺省回跳：仍指向配置的规范地址。
	legacy := doGet(t, r, "/wecom/login?state=x", map[string]string{
		"X-Forwarded-Host":  "evil.example.com",
		"X-Forwarded-Proto": "https",
	})
	if legacy.Code != http.StatusFound ||
		!strings.Contains(legacy.Header().Get("Location"), url.QueryEscape("http://127.0.0.1:3002/wecom/callback")) {
		t.Fatalf("legacy redirect must use configured base, loc=%s", legacy.Header().Get("Location"))
	}
	// scheme 不一致也算跨源（配置 https 时 http override 拒绝）：用独立 fixture 验证。
	{
		gin.SetMode(gin.TestMode)
		cfgHTTPS := &config.Config{
			Server: config.ServerConfig{ExternalBaseURL: "https://gw.example.com"},
			WeCom:  config.WeComConfig{CorpID: testCorpID},
			Admin:  config.AdminConfig{Key: testAdminKey},
			Auth:   config.AuthConfig{Key: testAuthKey},
		}
		h2 := NewHandler(cfgHTTPS, nil, nil, nil, nil, s)
		r2 := gin.New()
		r2.GET("/wecom/login", h2.LoginRedirect)
		w := doGet(t, r2, "/wecom/login?redirect_uri="+url.QueryEscape("http://gw.example.com/integration/v1/auth/callback"), nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("scheme mismatch override = %d, want 400", w.Code)
		}
		ok := doGet(t, r2, "/wecom/login?redirect_uri="+url.QueryEscape("https://gw.example.com/integration/v1/auth/callback"), nil)
		if ok.Code != http.StatusFound {
			t.Fatalf("https same-origin override = %d, want 302", ok.Code)
		}
	}
}

// TestAuthorize_ForgedHostStillUsesConfiguredBase H2：伪造 Host 头的 authorize 响应
// 仍用 server.external_base_url 拼 callback/authorize_url（劫持者改 Host 不影响回跳目标）。
func TestAuthorize_ForgedHostStillUsesConfiguredBase(t *testing.T) {
	fac := newFakeAccountCenter(t)
	wk := &fakeWecomIdentity{}
	r, _ := setupIntegrationTest(t, fac, wk)

	w := doPostJSON(t, r, "/integration/v1/auth/authorize", gin.H{
		"client_id":      "work-desktop",
		"redirect_uri":   "http://127.0.0.1:5173/callback",
		"state":          "st-forged",
		"code_challenge": pkceChallenge(testVerifier),
		"device":         "dev-1",
	}, map[string]string{"X-Forwarded-Host": "evil.example.com", "X-Forwarded-Proto": "https"})
	if w.Code != http.StatusOK {
		t.Fatalf("authorize = %d, body=%s", w.Code, w.Body.String())
	}
	authorizeURL, _ := decodeBody(t, w)["authorize_url"].(string)
	if !strings.HasPrefix(authorizeURL, "http://127.0.0.1:3002/") {
		t.Fatalf("authorize_url must use configured external_base_url, got %s", authorizeURL)
	}
	if strings.Contains(authorizeURL, "evil.example.com") {
		t.Fatalf("forged host leaked into authorize_url: %s", authorizeURL)
	}
	u, err := url.Parse(authorizeURL)
	if err != nil {
		t.Fatalf("parse authorize_url: %v", err)
	}
	cb, _ := url.QueryUnescape(u.Query().Get("redirect_uri"))
	if cb != "http://127.0.0.1:3002/integration/v1/auth/callback" {
		t.Fatalf("embedded callback = %q, want configured base", cb)
	}
}

// TestIntegrationRefreshSession_AfterRevocation H3：授权清空 / 身份停用后，
// refresh 与 session 视图立即拒绝（403 ENTITLEMENT_REVOKED / 401 SESSION_REVOKED），
// 不再允许被撤主体在 refresh 有效期内续签。
func TestIntegrationRefreshSession_AfterRevocation(t *testing.T) {
	fac := newFakeAccountCenter(t)
	wk := &fakeWecomIdentity{}
	r, s := setupIntegrationTest(t, fac, wk)

	fac.setUser("union-1", "subj-1")
	wk.set("union-1", nil)
	link := insertIdentityLink(t, s, "subj-1", "ku-1")
	insertEntitlement(t, s, "subj-1", "tenant-10001", "doc.read", 1, false)

	_, code := authorizeCallbackChain(t, r, "work-desktop", "http://127.0.0.1:5173/callback", "st-rv", pkceChallenge(testVerifier), "dev-rv")
	tw := doPostJSON(t, r, "/integration/v1/auth/token", tokenBody(code, "work-desktop", "dev-rv", "work"), nil)
	if tw.Code != http.StatusOK {
		t.Fatalf("token = %d, body=%s", tw.Code, tw.Body.String())
	}
	tokenResp := decodeBody(t, tw)
	access, _ := tokenResp["access_token"].(string)
	refresh, _ := tokenResp["refresh_token"].(string)

	// 1) entitlement 全撤（身份仍 active）→ refresh/session 403 ENTITLEMENT_REVOKED
	if err := s.StateDB().Exec("UPDATE managed_entitlements SET revoked_at = ? WHERE subject = ?", time.Now(), "subj-1").Error; err != nil {
		t.Fatalf("revoke entitlements: %v", err)
	}
	rw := doPostJSON(t, r, "/integration/v1/auth/refresh", gin.H{"refresh_token": refresh}, nil)
	if rw.Code != http.StatusForbidden || errorCode(t, rw) != "ENTITLEMENT_REVOKED" {
		t.Fatalf("refresh after entitlement revoke = %d, body=%s", rw.Code, rw.Body.String())
	}
	sw := doGet(t, r, "/integration/v1/session", map[string]string{"Authorization": "Bearer " + access})
	if sw.Code != http.StatusForbidden || errorCode(t, sw) != "ENTITLEMENT_REVOKED" {
		t.Fatalf("session after entitlement revoke = %d, body=%s", sw.Code, sw.Body.String())
	}

	// 2) 恢复授权 + 停用身份关联 → refresh/session 401 SESSION_REVOKED
	if err := s.StateDB().Exec("UPDATE managed_entitlements SET revoked_at = NULL WHERE subject = ?", "subj-1").Error; err != nil {
		t.Fatalf("restore entitlements: %v", err)
	}
	if err := s.StateDB().Exec("UPDATE identity_links SET status = 'disabled' WHERE id = ?", link.ID).Error; err != nil {
		t.Fatalf("disable link: %v", err)
	}
	rw = doPostJSON(t, r, "/integration/v1/auth/refresh", gin.H{"refresh_token": refresh}, nil)
	if rw.Code != http.StatusUnauthorized || errorCode(t, rw) != "SESSION_REVOKED" {
		t.Fatalf("refresh after identity disabled = %d, body=%s", rw.Code, rw.Body.String())
	}
	sw = doGet(t, r, "/integration/v1/session", map[string]string{"Authorization": "Bearer " + access})
	if sw.Code != http.StatusUnauthorized || errorCode(t, sw) != "SESSION_REVOKED" {
		t.Fatalf("session after identity disabled = %d, body=%s", sw.Code, sw.Body.String())
	}

	// 3) 身份恢复 active：同一会话放行（证明拒绝由身份状态驱动，会话本身未被误撤）
	if err := s.StateDB().Exec("UPDATE identity_links SET status = 'active' WHERE id = ?", link.ID).Error; err != nil {
		t.Fatalf("re-enable link: %v", err)
	}
	rw = doPostJSON(t, r, "/integration/v1/auth/refresh", gin.H{"refresh_token": refresh}, nil)
	if rw.Code != http.StatusOK {
		t.Fatalf("refresh after identity restored = %d, body=%s", rw.Code, rw.Body.String())
	}
}

// TestIntegrationCallback_ConcurrentRetryKeepsCompleted H4：同一 state 的并发/重试回调中，
// 只有首到者签发授权码；后到者的拒绝不得把已 completed 的事务翻转回 failed
// （FailAuthTransaction 仅 pending 可转 failed），原授权码仍可正常兑换。
func TestIntegrationCallback_ConcurrentRetryKeepsCompleted(t *testing.T) {
	fac := newFakeAccountCenter(t)
	wk := &fakeWecomIdentity{}
	r, s := setupIntegrationTest(t, fac, wk)

	fac.setUser("union-1", "subj-1")
	wk.set("union-1", nil)
	insertIdentityLink(t, s, "subj-1", "ku-1")

	aw := doPostJSON(t, r, "/integration/v1/auth/authorize", gin.H{
		"client_id": "work-desktop", "redirect_uri": "http://127.0.0.1:5173/callback",
		"state": "st-conc-cb", "code_challenge": pkceChallenge(testVerifier), "device": "dev-1",
	}, nil)
	if aw.Code != http.StatusOK {
		t.Fatalf("authorize = %d, body=%s", aw.Code, aw.Body.String())
	}
	loginTxnID, _ := decodeBody(t, aw)["login_txn_id"].(string)

	const n = 6
	var wg sync.WaitGroup
	locations := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cb := doGet(t, r, "/integration/v1/auth/callback?code=wc-code&state=st-conc-cb", nil)
			if cb.Code != http.StatusFound {
				t.Errorf("callback = %d, body=%s", cb.Code, cb.Body.String())
				return
			}
			locations <- cb.Header().Get("Location")
		}()
	}
	wg.Wait()
	close(locations)

	issued := map[string]int{}
	for loc := range locations {
		u, err := url.Parse(loc)
		if err != nil {
			t.Fatalf("parse callback location: %v", err)
		}
		if c := u.Query().Get("code"); c != "" {
			issued[c]++
		}
	}
	if len(issued) != 1 {
		t.Fatalf("exactly one code should be issued across concurrent callbacks, got %v", issued)
	}

	// 事务保持 completed（后到者的失败路径不得翻转终态）
	txn, err := s.GetAuthTransaction(context.Background(), loginTxnID)
	if err != nil {
		t.Fatalf("GetAuthTransaction: %v", err)
	}
	if txn.Status != "completed" {
		t.Fatalf("txn status = %s, want completed（H4 终态不可逆）", txn.Status)
	}

	// 原授权码仍可兑换（ConsumeAuthCode 认 completed+未消费，与 code-exchange 口径一致）
	var code string
	for c := range issued {
		code = c
	}
	tw := doPostJSON(t, r, "/integration/v1/auth/token", tokenBody(code, "work-desktop", "dev-1", "work"), nil)
	if tw.Code != http.StatusOK {
		t.Fatalf("token after concurrent callbacks = %d, body=%s", tw.Code, tw.Body.String())
	}
}

// TestOAuthCallback_SubjectNotProvisioned H6：旧登录回调不再隐式触发部门同步
// （kickDepartmentSync 已移除）；部门空间映射未命中 → 403 SUBJECT_NOT_PROVISIONED。
func TestOAuthCallback_SubjectNotProvisioned(t *testing.T) {
	fac := newFakeAccountCenter(t)
	wk := &fakeWecomIdentity{}
	_, s := setupIntegrationTest(t, fac, wk)

	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Server: config.ServerConfig{ExternalBaseURL: "http://127.0.0.1:3002"},
		WeCom:  config.WeComConfig{CorpID: testCorpID},
	}
	syn := syncer.NewSyncer(cfg, nil, fac.client(), s)
	h := NewHandler(cfg, wk, fac.client(), syn, nil, s)
	r := gin.New()
	r.GET("/wecom/callback", h.OAuthCallback)

	// 账户中心可解析主体，但其部门无租户映射（映射表未供给）
	fac.setUser("union-np", "subj-np")
	fac.mu.Lock()
	fac.users["union-np"].DepartmentCode = "D-NO-MAPPING"
	fac.mu.Unlock()
	wk.set("union-np", nil)

	w := doGet(t, r, "/wecom/callback?code=wc-code", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("callback = %d, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "SUBJECT_NOT_PROVISIONED") {
		t.Fatalf("callback body missing SUBJECT_NOT_PROVISIONED: %s", w.Body.String())
	}

	// 不触发任何同步、不开户：无 identity link / 无新用户落库
	links, err := s.ListIdentityLinks(context.Background())
	if err != nil {
		t.Fatalf("list identity links: %v", err)
	}
	for _, l := range links {
		if l.ImmutableSubject == "subj-np" {
			t.Fatalf("login must not provision identity link: %+v", l)
		}
	}
}

// 占位 OIDC 端点必须失败关闭（部署实测：假 userinfo 曾让企微扫码登录为
// 幽灵用户 employee@benlai.com 并被自动开户）。
func TestOIDCPlaceholderFailsClosed(t *testing.T) {
	r := gin.New()
	h := &Handler{}
	r.GET("/oidc/token", h.OIDCToken)
	r.GET("/oidc/userinfo", h.OIDCUserInfo)

	for _, path := range []string{"/oidc/token", "/oidc/userinfo"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s = %d, want 501", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "employee@benlai.com") {
			t.Fatalf("%s still leaks the fake userinfo: %s", path, rec.Body.String())
		}
	}
}
