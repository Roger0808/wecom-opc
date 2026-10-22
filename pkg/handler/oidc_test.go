package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/accountcenter"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"
	"weknora-tokendance/pkg/wecom"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// ---------- OIDC 测试装配 ----------

const (
	testOIDCClientID     = "knowledge-oidc"
	testOIDCClientSecret = "oidc-secret-for-tests"
	testOIDCRedirectURI  = "http://weknora.internal/api/v1/auth/oidc/callback"
)

// fakeOIDCWecom 一次性 code 假企微：每个 code 只允许兑换一次 UserID
// （模拟企微真实语义：authorization_code 被消费后再次使用报错）。
type fakeOIDCWecom struct {
	mu       sync.Mutex
	codes    map[string]string // code → wecom UserID
	lastCode string
}

func newFakeOIDCWecom() *fakeOIDCWecom {
	return &fakeOIDCWecom{codes: map[string]string{}}
}

func (f *fakeOIDCWecom) addCode(code, userID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codes[code] = userID
}

func (f *fakeOIDCWecom) GetUserInfoByCode(code string) (*wecom.OAuthUserResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastCode = code
	userID, ok := f.codes[code]
	if !ok {
		return nil, errors.New("wecom: code consumed or unknown")
	}
	delete(f.codes, code) // 一次性消费
	return &wecom.OAuthUserResponse{UserID: userID}, nil
}

func (f *fakeOIDCWecom) GetUserDetailByID(userID string) (*wecom.UserDetail, error) {
	return &wecom.UserDetail{UserID: userID, Name: "员工-" + userID, Status: 1}, nil
}

// setUserProfile 给假账户中心的员工补全姓名/邮箱（既有 setUser 不含 email）。
func (f *fakeAccountCenter) setUserProfile(unionNo, id, name, email string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[unionNo] = &accountcenter.SysUser{ID: id, UnionNo: unionNo, UserName: name, Email: email}
}

// setupOIDCTest 装配已配置 OIDC 客户端凭据的 Handler + 路由（sqlite 内存状态库记审计）。
func setupOIDCTest(t *testing.T, wk *fakeOIDCWecom, fac *fakeAccountCenter, mutateCfg func(*config.Config)) (*gin.Engine, *storage.Storage) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	stateDB := openIntegrationTestSQLite(t)
	if err := stateDB.AutoMigrate(&model.AuditEvent{}); err != nil {
		t.Fatalf("failed to auto migrate state schema: %v", err)
	}
	s := storage.NewStorageWithDBs(openIntegrationTestSQLite(t), stateDB)

	cfg := &config.Config{
		Server:  config.ServerConfig{ExternalBaseURL: "http://127.0.0.1:3002"},
		WeKnora: config.WeKnoraConfig{BaseURL: "http://weknora.internal"},
		WeCom:   config.WeComConfig{CorpID: testCorpID},
		Admin:   config.AdminConfig{Key: testAdminKey},
		Auth:    config.AuthConfig{Key: testAuthKey},
		OIDC: config.OIDCConfig{
			ClientID:     testOIDCClientID,
			ClientSecret: testOIDCClientSecret,
		},
	}
	if mutateCfg != nil {
		mutateCfg(cfg)
	}
	h := NewHandler(cfg, wk, fac.client(), nil, nil, s)

	r := gin.New()
	r.POST("/oidc/token", h.OIDCToken)
	r.GET("/oidc/userinfo", h.OIDCUserInfo)
	return r, s
}

func doPostForm(t *testing.T, r *gin.Engine, path string, form url.Values, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "127.0.0.1:3002"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func oidcTokenForm(code string) url.Values {
	return url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {testOIDCRedirectURI},
		"client_id":     {testOIDCClientID},
		"client_secret": {testOIDCClientSecret},
	}
}

// ---------- 用例 ----------

// TestOIDCFullChain code→token→userinfo 全链路：企微 code 兑换 JWT，
// userinfo 返回账户中心真实档案；access_token 无状态可复用；code 重放被拒。
func TestOIDCFullChain(t *testing.T) {
	fac := newFakeAccountCenter(t)
	wk := newFakeOIDCWecom()
	r, s := setupOIDCTest(t, wk, fac, nil)

	fac.setUserProfile("union-oidc-1", "subj-oidc-1", "张三", "zhangsan@benlai.com")
	wk.addCode("wc-oidc-1", "union-oidc-1")

	// 1) token：企微 code → 账户中心 → 10min JWT。
	tw := doPostForm(t, r, "/oidc/token", oidcTokenForm("wc-oidc-1"), nil)
	if tw.Code != http.StatusOK {
		t.Fatalf("token = %d, body=%s", tw.Code, tw.Body.String())
	}
	tokenResp := decodeBody(t, tw)
	if tokenResp["token_type"] != "Bearer" || tokenResp["expires_in"].(float64) != 600 {
		t.Fatalf("token response mismatch: %v", tokenResp)
	}
	access, _ := tokenResp["access_token"].(string)
	if access == "" {
		t.Fatalf("missing access_token: %v", tokenResp)
	}
	claims := parsePlatformClaims(t, access)
	if claims["iss"] != "platform-oidc" || claims["aud"] != testOIDCClientID || claims["sub"] != "union-oidc-1" {
		t.Fatalf("token claims mismatch: %v", claims)
	}
	if claims["email"] != "zhangsan@benlai.com" || claims["name"] != "张三" {
		t.Fatalf("token identity claims mismatch: %v", claims)
	}
	exp, iat := claims["exp"].(float64), claims["iat"].(float64)
	if exp-iat != 600 {
		t.Fatalf("token lifetime = %v, want 600", exp-iat)
	}
	if _, ok := claims["jti"].(string); !ok || claims["jti"] == "" {
		t.Fatalf("token missing jti: %v", claims)
	}

	// 2) userinfo：Bearer access_token → 真实档案 {sub, name, email}。
	uw := doGet(t, r, "/oidc/userinfo", map[string]string{"Authorization": "Bearer " + access})
	if uw.Code != http.StatusOK {
		t.Fatalf("userinfo = %d, body=%s", uw.Code, uw.Body.String())
	}
	info := decodeBody(t, uw)
	if info["sub"] != "union-oidc-1" || info["name"] != "张三" || info["email"] != "zhangsan@benlai.com" {
		t.Fatalf("userinfo identity mismatch: %v", info)
	}

	// 3) access_token 无状态复用：第二次 userinfo 仍可用（JWT 不一次性消费）。
	uw2 := doGet(t, r, "/oidc/userinfo", map[string]string{"Authorization": "Bearer " + access})
	if uw2.Code != http.StatusOK || decodeBody(t, uw2)["email"] != "zhangsan@benlai.com" {
		t.Fatalf("userinfo reuse = %d, body=%s", uw2.Code, uw2.Body.String())
	}

	// 4) code 重放：同一 code 第二次 token 请求因企微拒绝 → 400 invalid_grant。
	replay := doPostForm(t, r, "/oidc/token", oidcTokenForm("wc-oidc-1"), nil)
	if replay.Code != http.StatusBadRequest || decodeBody(t, replay)["error"] != "invalid_grant" {
		t.Fatalf("code replay = %d, body=%s", replay.Code, replay.Body.String())
	}

	// 5) 审计：签发与拒绝都留痕，且不出现任何占位假用户。
	actions := auditActions(t, s)
	for _, want := range []string{"integration.oidc.token.issued", "integration.oidc.token.denied", "integration.oidc.userinfo.viewed"} {
		if !hasAction(actions, want) {
			t.Errorf("audit missing %s (got %v)", want, actions)
		}
	}
	events, err := s.ListAuditEvents(context.Background())
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	for _, e := range events {
		if strings.Contains(e.After, "employee@benlai.com") {
			t.Fatalf("audit leaks the fake placeholder user: %+v", e)
		}
	}
}

// TestOIDCToken_ClientAuth client_id / client_secret 错误 → 401 invalid_client。
func TestOIDCToken_ClientAuth(t *testing.T) {
	fac := newFakeAccountCenter(t)
	wk := newFakeOIDCWecom()
	r, _ := setupOIDCTest(t, wk, fac, nil)

	wk.addCode("wc-oidc-auth", "union-oidc-1")
	fac.setUserProfile("union-oidc-1", "subj-1", "张三", "zhangsan@benlai.com")

	badID := oidcTokenForm("wc-oidc-auth")
	badID.Set("client_id", "evil-client")
	w := doPostForm(t, r, "/oidc/token", badID, nil)
	if w.Code != http.StatusUnauthorized || decodeBody(t, w)["error"] != "invalid_client" {
		t.Fatalf("wrong client_id = %d, body=%s", w.Code, w.Body.String())
	}

	badSecret := oidcTokenForm("wc-oidc-auth")
	badSecret.Set("client_secret", "wrong-secret")
	w = doPostForm(t, r, "/oidc/token", badSecret, nil)
	if w.Code != http.StatusUnauthorized || decodeBody(t, w)["error"] != "invalid_client" {
		t.Fatalf("wrong client_secret = %d, body=%s", w.Code, w.Body.String())
	}

	// 企微 code 未被消费：认证失败的请求不得触碰下游。
	wk.mu.Lock()
	if _, consumed := wk.codes["wc-oidc-auth"]; !consumed {
		t.Fatalf("client auth failure must not consume the wecom code")
	}
	wk.mu.Unlock()

	// 正确凭据仍可正常兑换（前面的失败不产生任何副作用）。
	ok := doPostForm(t, r, "/oidc/token", oidcTokenForm("wc-oidc-auth"), nil)
	if ok.Code != http.StatusOK {
		t.Fatalf("token with correct credentials = %d, body=%s", ok.Code, ok.Body.String())
	}
}

// TestOIDCToken_RedirectURI redirect_uri 必须与 weknora.base_url 同源且
// 路径为 /api/v1/auth/oidc/callback，否则 400。
func TestOIDCToken_RedirectURI(t *testing.T) {
	fac := newFakeAccountCenter(t)
	wk := newFakeOIDCWecom()
	r, _ := setupOIDCTest(t, wk, fac, nil)

	fac.setUserProfile("union-oidc-1", "subj-1", "张三", "zhangsan@benlai.com")

	cases := []struct {
		name       string
		redirect   string
		wantStatus int
	}{
		{"valid", testOIDCRedirectURI, http.StatusOK},
		{"cross origin", "http://evil.example.com/api/v1/auth/oidc/callback", http.StatusBadRequest},
		{"wrong path", "http://weknora.internal/other/callback", http.StatusBadRequest},
		{"scheme mismatch", "https://weknora.internal/api/v1/auth/oidc/callback", http.StatusBadRequest},
		{"empty", "", http.StatusBadRequest},
	}
	for i, tc := range cases {
		code := "wc-oidc-rd-" + string(rune('a'+i))
		wk.addCode(code, "union-oidc-1")
		form := oidcTokenForm(code)
		form.Set("redirect_uri", tc.redirect)
		w := doPostForm(t, r, "/oidc/token", form, nil)
		if w.Code != tc.wantStatus {
			t.Fatalf("%s: redirect_uri=%q → %d, body=%s", tc.name, tc.redirect, w.Code, w.Body.String())
		}
		if tc.wantStatus == http.StatusOK && decodeBody(t, w)["access_token"] == "" {
			t.Fatalf("%s: valid redirect_uri returned no access_token", tc.name)
		}
	}
}

// TestOIDCToken_GrantAndUpstreamFailures grant_type / code 缺省与上游失败路径。
func TestOIDCToken_GrantAndUpstreamFailures(t *testing.T) {
	fac := newFakeAccountCenter(t)
	wk := newFakeOIDCWecom()
	r, _ := setupOIDCTest(t, wk, fac, nil)

	// grant_type 非 authorization_code。
	badGrant := oidcTokenForm("wc-oidc-x")
	badGrant.Set("grant_type", "refresh_token")
	w := doPostForm(t, r, "/oidc/token", badGrant, nil)
	if w.Code != http.StatusBadRequest || decodeBody(t, w)["error"] != "unsupported_grant_type" {
		t.Fatalf("bad grant_type = %d, body=%s", w.Code, w.Body.String())
	}

	// 缺 code。
	missingCode := oidcTokenForm("")
	missingCode.Del("code")
	w = doPostForm(t, r, "/oidc/token", missingCode, nil)
	if w.Code != http.StatusBadRequest || decodeBody(t, w)["error"] != "invalid_request" {
		t.Fatalf("missing code = %d, body=%s", w.Code, w.Body.String())
	}

	// 企微拒绝 code（伪造/已消费）。
	wk.addCode("wc-oidc-bad", "union-oidc-bad")
	fac.setUserProfile("union-oidc-bad", "subj-bad", "李四", "lisi@benlai.com")
	w = doPostForm(t, r, "/oidc/token", oidcTokenForm("wc-oidc-bad"), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("first use of good code = %d, body=%s", w.Code, w.Body.String())
	}
	// 同 code 复用：企微已消费 → 400 invalid_grant（绝不本地放行）。
	w = doPostForm(t, r, "/oidc/token", oidcTokenForm("wc-oidc-bad"), nil)
	if w.Code != http.StatusBadRequest || decodeBody(t, w)["error"] != "invalid_grant" {
		t.Fatalf("consumed wecom code = %d, body=%s", w.Code, w.Body.String())
	}

	// 账户中心 500：→ 400 invalid_grant，不签发。
	wk.addCode("wc-oidc-acerr", "union-oidc-acerr")
	fac.setError("union-oidc-acerr")
	w = doPostForm(t, r, "/oidc/token", oidcTokenForm("wc-oidc-acerr"), nil)
	if w.Code != http.StatusBadRequest || decodeBody(t, w)["error"] != "invalid_grant" {
		t.Fatalf("account center error = %d, body=%s", w.Code, w.Body.String())
	}

	// 账户中心查无此人：→ 400 invalid_grant，绝不造用户。
	wk.addCode("wc-oidc-nouser", "union-oidc-nouser")
	w = doPostForm(t, r, "/oidc/token", oidcTokenForm("wc-oidc-nouser"), nil)
	if w.Code != http.StatusBadRequest || decodeBody(t, w)["error"] != "invalid_grant" {
		t.Fatalf("unknown subject = %d, body=%s", w.Code, w.Body.String())
	}

	// 员工无邮箱：无法按 email 匹配本地用户 → 400 invalid_grant。
	wk.addCode("wc-oidc-noemail", "union-oidc-noemail")
	fac.users["union-oidc-noemail"] = &accountcenter.SysUser{ID: "subj-ne", UnionNo: "union-oidc-noemail", UserName: "无邮箱"}
	w = doPostForm(t, r, "/oidc/token", oidcTokenForm("wc-oidc-noemail"), nil)
	if w.Code != http.StatusBadRequest || decodeBody(t, w)["error"] != "invalid_grant" {
		t.Fatalf("no-email subject = %d, body=%s", w.Code, w.Body.String())
	}
}

// TestOIDCUserInfo_AuthFailures 无效 / 过期 / 错签 / 错 iss / 错 aud 的 Bearer 一律 401。
func TestOIDCUserInfo_AuthFailures(t *testing.T) {
	fac := newFakeAccountCenter(t)
	wk := newFakeOIDCWecom()
	r, _ := setupOIDCTest(t, wk, fac, nil)

	fac.setUserProfile("union-oidc-1", "subj-1", "张三", "zhangsan@benlai.com")
	wk.addCode("wc-oidc-ui", "union-oidc-1")
	tw := doPostForm(t, r, "/oidc/token", oidcTokenForm("wc-oidc-ui"), nil)
	access, _ := decodeBody(t, tw)["access_token"].(string)

	sign := func(key string, mutate func(jwt.MapClaims)) string {
		t.Helper()
		claims := jwt.MapClaims{
			"iss": "platform-oidc", "aud": testOIDCClientID, "sub": "union-oidc-1",
			"email": "zhangsan@benlai.com", "name": "张三",
			"iat": time.Now().Unix(), "exp": time.Now().Add(10 * time.Minute).Unix(), "jti": "jti-test",
		}
		if mutate != nil {
			mutate(claims)
		}
		raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(key))
		if err != nil {
			t.Fatalf("sign test token: %v", err)
		}
		return raw
	}

	cases := []struct {
		name  string
		token string
	}{
		{"garbage", "not-a-jwt"},
		{"wrong signing key", sign("wrong-key-0123456789abcdef0123456789", nil)},
		{"wrong issuer", sign(testAuthKey, func(c jwt.MapClaims) { c["iss"] = "evil-issuer" })},
		{"wrong audience", sign(testAuthKey, func(c jwt.MapClaims) { c["aud"] = "evil-client" })},
		{"expired", sign(testAuthKey, func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Minute).Unix() })},
	}
	for _, tc := range cases {
		w := doGet(t, r, "/oidc/userinfo", map[string]string{"Authorization": "Bearer " + tc.token})
		if w.Code != http.StatusUnauthorized || decodeBody(t, w)["error"] != "invalid_token" {
			t.Fatalf("%s = %d, body=%s", tc.name, w.Code, w.Body.String())
		}
	}

	// 缺 Authorization 头。
	noHeader := doGet(t, r, "/oidc/userinfo", nil)
	if noHeader.Code != http.StatusUnauthorized || decodeBody(t, noHeader)["error"] != "invalid_token" {
		t.Fatalf("missing bearer = %d, body=%s", noHeader.Code, noHeader.Body.String())
	}

	// 网关签发的真 token 可用（对照组）。
	ok := doGet(t, r, "/oidc/userinfo", map[string]string{"Authorization": "Bearer " + access})
	if ok.Code != http.StatusOK {
		t.Fatalf("gateway-issued token = %d, body=%s", ok.Code, ok.Body.String())
	}
}

// TestOIDCUnconfigured_FailsClosed 未配置 oidc 凭据时两个端点保持 501（eab7450 行为），
// 即使请求本身格式完整、上游可用也不放行。
func TestOIDCUnconfigured_FailsClosed(t *testing.T) {
	fac := newFakeAccountCenter(t)
	wk := newFakeOIDCWecom()
	r, _ := setupOIDCTest(t, wk, fac, func(cfg *config.Config) {
		cfg.OIDC = config.OIDCConfig{}
	})

	fac.setUserProfile("union-oidc-1", "subj-1", "张三", "zhangsan@benlai.com")
	wk.addCode("wc-oidc-cfg", "union-oidc-1")

	tw := doPostForm(t, r, "/oidc/token", oidcTokenForm("wc-oidc-cfg"), nil)
	if tw.Code != http.StatusNotImplemented {
		t.Fatalf("token unconfigured = %d, body=%s", tw.Code, tw.Body.String())
	}
	// 501 不得消费企微 code（配置补齐后同一 code 仍应可兑换）。
	wk.mu.Lock()
	if _, kept := wk.codes["wc-oidc-cfg"]; !kept {
		t.Fatalf("fail-closed token endpoint must not consume the wecom code")
	}
	wk.mu.Unlock()

	uw := doGet(t, r, "/oidc/userinfo", map[string]string{"Authorization": "Bearer anything"})
	if uw.Code != http.StatusNotImplemented {
		t.Fatalf("userinfo unconfigured = %d, body=%s", uw.Code, uw.Body.String())
	}
}
