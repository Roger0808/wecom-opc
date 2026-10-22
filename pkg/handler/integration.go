// TASK-P04 / INT-101: 客户端认证接口组（/integration/v1）。
//
// 本文件实现桌面客户端（work-desktop / code-desktop）的企微登录事务流程：
// authorize → 企微登录回调 callback → 授权码原子兑换 token → refresh 轮换与
// 复用检测 → logout 族撤销 → GET session 会话视图；code-desktop 走
// code-backend 受众，一次性 exchange_token 由 code 后端服务在 code-exchange 消费。
//
// 安全要点：
//   - redirect_uri 仅允许 http://127.0.0.1:<port>/callback（OPEN-104 回环已决）；
//   - 授权码/兑换材料只存哈希，原子 UPDATE 消费，影响行数 0 即拒绝并重放审计；
//   - 回调路径不触发 SyncAll、不自动开户（未供给/冲突显式拒绝）；
//   - 全部签发/消费/拒绝写 audit_events（平台状态库）。
package handler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/accountcenter"
	"weknora-tokendance/pkg/adminauth"
	"weknora-tokendance/pkg/identity"
	"weknora-tokendance/pkg/kbridge"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"
	"weknora-tokendance/pkg/wecom"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// BND-180 错误码（本接口组允许使用的枚举子集；WECOM_EXCHANGE_FAILED 为追加枚举，
// 企微换票失败专用，账户中心失败才用 ACCOUNT_CENTER_UNAVAILABLE）
const (
	errCodeAuthCodeReplayed         = "AUTH_CODE_REPLAYED"
	errCodeAuthStateMismatch        = "AUTH_STATE_MISMATCH"
	errCodeSubjectNotProvisioned    = "SUBJECT_NOT_PROVISIONED"
	errCodeSubjectConflict          = "SUBJECT_CONFLICT"
	errCodeWecomExchangeFailed      = "WECOM_EXCHANGE_FAILED"
	errCodeAccountCenterUnavailable = "ACCOUNT_CENTER_UNAVAILABLE"
	errCodeSessionRevoked           = "SESSION_REVOKED"
	errCodeRefreshReuseDetected     = "REFRESH_REUSE_DETECTED"
	errCodeInternal                 = "INTERNAL_ERROR"
)

// 客户端与受众绑定（BND-101/102）：client_id 白名单固定映射受众。
const (
	clientWorkDesktop = "work-desktop"
	clientCodeDesktop = "code-desktop"

	audienceWork        = "work"
	audienceCodeBackend = "code-backend"
)

// 令牌生命周期（INT-101）
const (
	authorizeTxnTTL  = 10 * time.Minute // 登录事务授权窗口
	authCodeTTL      = 120 * time.Second
	exchangeTokenTTL = 120 * time.Second
	accessTokenTTL   = 15 * time.Minute
	refreshTokenTTL  = 14 * 24 * time.Hour
)

const (
	identitySourceWecom = "wecom"
	grantTypeAuthCode   = "authorization_code"
)

// wecomIdentityAPI 企微身份解析接口（*wecom.Client 满足；测试注入假企微）。
type wecomIdentityAPI interface {
	GetUserInfoByCode(code string) (*wecom.OAuthUserResponse, error)
}

// accountCenterAPI 统一账户中心查询接口（*accountcenter.Client 满足）。
type accountCenterAPI interface {
	GetUserByUnionNo(ctx context.Context, unionNo string) (*accountcenter.SysUser, error)
}

// IntegrationHandler /integration/v1 认证接口组处理器。
type IntegrationHandler struct {
	cfg      *config.Config
	wecom    wecomIdentityAPI
	ac       accountCenterAPI
	storage  *storage.Storage
	identity *identity.Service
}

// NewIntegrationHandler 组装认证接口组处理器。wecomClient/acClient 接受满足接口的
// 实现（生产传入 *wecom.Client / *accountcenter.Client，测试可注入假企微/假账户中心）。
func NewIntegrationHandler(cfg *config.Config, wecomClient wecomIdentityAPI, acClient accountCenterAPI, s *storage.Storage) *IntegrationHandler {
	return &IntegrationHandler{
		cfg:      cfg,
		wecom:    wecomClient,
		ac:       acClient,
		storage:  s,
		identity: identity.NewService(s),
	}
}

// ---------- 请求/响应模型 ----------

type authorizeRequest struct {
	ClientID      string `json:"client_id"`
	RedirectURI   string `json:"redirect_uri"`
	State         string `json:"state"`
	CodeChallenge string `json:"code_challenge"`
	Device        string `json:"device"`
}

type tokenRequest struct {
	GrantType    string `json:"grant_type"`
	Code         string `json:"code"`
	CodeVerifier string `json:"code_verifier"`
	ClientID     string `json:"client_id"`
	Device       string `json:"device"`
	Audience     string `json:"audience"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type logoutRequest struct {
	RefreshToken string `json:"refresh_token"`
	AccessToken  string `json:"access_token"`
}

type codeExchangeRequest struct {
	ExchangeToken string `json:"exchange_token"`
	// CodeVerifier PKCE 复核（INT-101g 增补）：authorize 已存 code_challenge 时必传，
	// S256(verifier) 不匹配即 400 AUTH_STATE_MISMATCH。
	CodeVerifier string `json:"code_verifier"`
}

// codeSpaceView code-exchange 响应的 spaces 条目：tenant + 该主体在该空间的
// knowledge 成员角色（viewer/contributor/admin/owner 直出 tenant_members.role，
// 无成员关系时为空串）。
type codeSpaceView struct {
	TenantID string `json:"tenant_id"`
	Role     string `json:"role"`
}

// ---------- POST /integration/v1/auth/authorize ----------

// Authorize 校验客户端白名单与回环 redirect_uri，创建 pending 登录事务（10min），
// 返回 login_txn_id 与指向既有企微登录入口（/wecom/login）的 authorize_url。
func (h *IntegrationHandler) Authorize(c *gin.Context) {
	ctx := c.Request.Context()
	var req authorizeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.deny(c, http.StatusBadRequest, errCodeAuthStateMismatch, "invalid JSON body: "+err.Error(), "anonymous", nil)
		return
	}

	audience, ok := map[string]string{
		clientWorkDesktop: audienceWork,
		clientCodeDesktop: audienceCodeBackend,
	}[req.ClientID]
	if !ok {
		h.deny(c, http.StatusBadRequest, errCodeAuthStateMismatch, "unknown client_id: "+req.ClientID, "client:"+req.ClientID, nil)
		return
	}
	if h.externalBaseURL() == "" {
		h.deny(c, http.StatusInternalServerError, errCodeInternal, "server.external_base_url is not configured", "client:"+req.ClientID, nil)
		return
	}
	if !ValidIntegrationCallbackURI(req.RedirectURI) {
		h.deny(c, http.StatusBadRequest, errCodeAuthStateMismatch, "redirect_uri must be http://127.0.0.1:<port>/callback", "client:"+req.ClientID, nil)
		return
	}
	if req.State == "" {
		h.deny(c, http.StatusBadRequest, errCodeAuthStateMismatch, "state is required", "client:"+req.ClientID, nil)
		return
	}
	if req.CodeChallenge == "" {
		h.deny(c, http.StatusBadRequest, errCodeAuthStateMismatch, "code_challenge is required (PKCE S256)", "client:"+req.ClientID, nil)
		return
	}

	txn := &model.AuthTransaction{
		ClientID:          req.ClientID,
		RedirectURI:       req.RedirectURI,
		StateHash:         sha256Hex(req.State),
		CodeChallenge:     req.CodeChallenge,
		DeviceFingerprint: req.Device,
		Audience:          audience,
		ExpiresAt:         time.Now().Add(authorizeTxnTTL),
		Status:            "pending",
	}
	if err := h.storage.InsertAuthTransaction(ctx, txn); err != nil {
		h.deny(c, http.StatusInternalServerError, errCodeInternal, "persist auth transaction failed", "client:"+req.ClientID, nil)
		return
	}

	callbackURL := h.externalBaseURL() + "/integration/v1/auth/callback"
	authorizeURL := "/wecom/login?redirect_uri=" + url.QueryEscape(callbackURL) + "&state=" + url.QueryEscape(req.State)

	h.audit(ctx, "client:"+req.ClientID, "integration.auth.authorize.created", "POST /integration/v1/auth/authorize", map[string]interface{}{
		"result":       "allowed",
		"login_txn_id": txn.LoginTxnID,
		"audience":     audience,
		"redirect_uri": req.RedirectURI,
		"expires_in_s": int(authorizeTxnTTL.Seconds()),
	}, c)

	c.JSON(http.StatusOK, gin.H{
		"login_txn_id":  txn.LoginTxnID,
		"authorize_url": h.externalBaseURL() + authorizeURL,
	})
}

// ---------- GET /integration/v1/auth/callback ----------

// Callback 企微 OAuth 回调落点（与旧 /wecom/callback 并存，互不影响）。
// 校验 state↔txn（state_hash 比对），企微 code 换 UserID，账户中心解析主体，
// 命中身份关联则签发 120s 一次性授权码并 302 回 redirect_uri；
// 未供给/冲突/上游失败均把事务标记 failed 并携带错误码重定向。本路径不触发任何同步。
func (h *IntegrationHandler) Callback(c *gin.Context) {
	ctx := c.Request.Context()
	code := c.Query("code")
	state := c.Query("state")
	if code == "" {
		h.deny(c, http.StatusBadRequest, errCodeAuthStateMismatch, "missing code parameter", "anonymous", nil)
		return
	}

	txn, err := h.storage.GetLatestAuthTransactionByStateHash(ctx, sha256Hex(state))
	if err != nil {
		h.deny(c, http.StatusBadRequest, errCodeAuthStateMismatch, "unknown or expired state", "anonymous", nil)
		return
	}
	redirectWithError := func(code string) {
		h.failTxnAndRedirect(c, ctx, txn, state, code)
	}

	if txn.Status != "pending" || !time.Now().Before(txn.ExpiresAt) {
		redirectWithError(errCodeAuthStateMismatch)
		return
	}

	userInfo, err := h.wecom.GetUserInfoByCode(code)
	if err != nil || userInfo == nil || userInfo.UserID == "" {
		reason := "wecom code exchange failed"
		if err != nil {
			reason = err.Error()
		}
		// 企微换票失败与账户中心失败分开映射（BND-180 追加枚举）
		redirectWithError(errCodeWecomExchangeFailed)
		log.Printf("[Integration] wecom getuserinfo failed (txn=%s): %s", txn.LoginTxnID, reason)
		return
	}

	acUser, err := h.ac.GetUserByUnionNo(ctx, userInfo.UserID)
	if err != nil {
		redirectWithError(errCodeAccountCenterUnavailable)
		log.Printf("[Integration] account center query failed (txn=%s, unionNo=%s): %v", txn.LoginTxnID, userInfo.UserID, err)
		return
	}
	if acUser == nil {
		redirectWithError(errCodeSubjectNotProvisioned)
		return
	}

	link, err := h.identity.Resolve(ctx, identitySourceWecom, h.cfg.WeCom.CorpID, acUser.ID)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrNotProvisioned):
			redirectWithError(errCodeSubjectNotProvisioned)
		case errors.Is(err, identity.ErrConflict):
			redirectWithError(errCodeSubjectConflict)
		default:
			h.failTxnAndRedirect(c, ctx, txn, state, errCodeInternal)
			log.Printf("[Integration] identity resolve failed (txn=%s): %v", txn.LoginTxnID, err)
		}
		return
	}

	rawCode, err := randomURLToken(32)
	if err != nil {
		h.failTxnAndRedirect(c, ctx, txn, state, errCodeInternal)
		return
	}
	rows, err := h.storage.IssueAuthCode(ctx, txn.LoginTxnID, sha256Hex(rawCode), link.ID, time.Now().Add(authCodeTTL))
	if err != nil || rows == 0 {
		h.failTxnAndRedirect(c, ctx, txn, state, errCodeAuthStateMismatch)
		return
	}

	h.audit(ctx, link.ID, "integration.auth.code.issued", "GET /integration/v1/auth/callback", map[string]interface{}{
		"result":       "allowed",
		"login_txn_id": txn.LoginTxnID,
		"subject":      acUser.ID,
		"expires_in_s": int(authCodeTTL.Seconds()),
	}, c)

	target := txn.RedirectURI + "?code=" + url.QueryEscape(rawCode) + "&state=" + url.QueryEscape(state)
	c.Redirect(http.StatusFound, target)
}

// failTxnAndRedirect 把事务标记 failed 并 302 回 redirect_uri 携带错误码。
func (h *IntegrationHandler) failTxnAndRedirect(c *gin.Context, ctx context.Context, txn *model.AuthTransaction, state, errCode string) {
	if _, err := h.storage.FailAuthTransaction(ctx, txn.LoginTxnID); err != nil {
		log.Printf("[Integration] mark txn failed (txn=%s): %v", txn.LoginTxnID, err)
	}
	h.audit(ctx, "client:"+txn.ClientID, "integration.auth.callback.denied", "GET /integration/v1/auth/callback", map[string]interface{}{
		"result":       "denied",
		"login_txn_id": txn.LoginTxnID,
		"error":        errCode,
	}, c)
	target := txn.RedirectURI + "?error=" + errCode + "&state=" + url.QueryEscape(state)
	c.Redirect(http.StatusFound, target)
}

// ---------- POST /integration/v1/auth/token ----------

// Token 原子消费一次性授权码并按受众签发令牌：
//   - audience=work：创建 sessions 行（新轮换族），返回 15min access + 14d refresh；
//   - audience=code-backend：签发 120s 一次性兑换材料，返回 exchange_token。
func (h *IntegrationHandler) Token(c *gin.Context) {
	ctx := c.Request.Context()
	var req tokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.deny(c, http.StatusBadRequest, errCodeAuthStateMismatch, "invalid JSON body: "+err.Error(), "anonymous", nil)
		return
	}
	if req.GrantType != grantTypeAuthCode {
		h.deny(c, http.StatusBadRequest, errCodeAuthStateMismatch, "unsupported grant_type: "+req.GrantType, "client:"+req.ClientID, nil)
		return
	}
	if req.Code == "" {
		h.deny(c, http.StatusBadRequest, errCodeAuthStateMismatch, "code is required", "client:"+req.ClientID, nil)
		return
	}

	codeHash := sha256Hex(req.Code)
	txn, err := h.storage.GetLatestAuthTransactionByCodeHash(ctx, codeHash)
	if err != nil {
		h.deny(c, http.StatusBadRequest, errCodeAuthCodeReplayed, "unknown authorization code", "client:"+req.ClientID, nil)
		return
	}

	rows, err := h.storage.ConsumeAuthCode(ctx, txn.LoginTxnID, codeHash)
	if err != nil || rows == 0 {
		h.deny(c, http.StatusBadRequest, errCodeAuthCodeReplayed, "authorization code replayed or expired", "client:"+txn.ClientID, auditTxnDetail(txn))
		return
	}

	// 授权码已消费；以下绑定校验失败一律按状态不匹配拒绝（码不再恢复可用）。
	if req.ClientID != txn.ClientID {
		h.deny(c, http.StatusBadRequest, errCodeAuthStateMismatch, "client_id mismatch with transaction", "client:"+req.ClientID, auditTxnDetail(txn))
		return
	}
	if req.Device != txn.DeviceFingerprint {
		h.deny(c, http.StatusBadRequest, errCodeAuthStateMismatch, "device mismatch with transaction", "client:"+req.ClientID, auditTxnDetail(txn))
		return
	}
	if req.Audience != txn.Audience {
		h.deny(c, http.StatusBadRequest, errCodeAuthStateMismatch, "audience mismatch with transaction", "client:"+req.ClientID, auditTxnDetail(txn))
		return
	}
	if txn.CodeChallenge == "" || pkceS256(req.CodeVerifier) != txn.CodeChallenge {
		h.deny(c, http.StatusBadRequest, errCodeAuthStateMismatch, "PKCE verification failed", "client:"+req.ClientID, auditTxnDetail(txn))
		return
	}
	if txn.IdentityLinkID == nil || *txn.IdentityLinkID == "" {
		h.deny(c, http.StatusBadRequest, errCodeAuthStateMismatch, "transaction missing authenticated subject", "client:"+req.ClientID, auditTxnDetail(txn))
		return
	}

	if txn.Audience == audienceWork {
		h.issueWorkTokens(c, ctx, txn)
		return
	}
	h.issueExchangeToken(c, ctx, txn)
}

// issueWorkTokens 创建会话族并签发平台自有 HS256 access/refresh。
// refresh_verifier_hash = sha256(refresh token 串本身)，轮换时旧值挪入 previous。
func (h *IntegrationHandler) issueWorkTokens(c *gin.Context, ctx context.Context, txn *model.AuthTransaction) {
	now := time.Now()
	sessionID := uuid.NewString()
	familyID := uuid.NewString()
	accessJWT, err := h.signPlatformJWT(map[string]interface{}{
		"sub":  *txn.IdentityLinkID,
		"sid":  sessionID,
		"aud":  audienceWork,
		"type": "access",
		"exp":  now.Add(accessTokenTTL).Unix(),
		"iat":  now.Unix(),
		"jti":  uuid.NewString(),
	})
	if err != nil {
		h.deny(c, http.StatusInternalServerError, errCodeInternal, "sign access token failed", "client:"+txn.ClientID, auditTxnDetail(txn))
		return
	}
	refreshExp := now.Add(refreshTokenTTL)
	refreshJWT, err := h.signPlatformJWT(map[string]interface{}{
		"sub":  *txn.IdentityLinkID,
		"sid":  sessionID,
		"aud":  audienceWork,
		"type": "refresh",
		"exp":  refreshExp.Unix(),
		"iat":  now.Unix(),
		"jti":  uuid.NewString(),
	})
	if err != nil {
		h.deny(c, http.StatusInternalServerError, errCodeInternal, "sign refresh token failed", "client:"+txn.ClientID, auditTxnDetail(txn))
		return
	}
	session := &model.Session{
		SessionID:           sessionID,
		IdentityLinkID:      *txn.IdentityLinkID,
		FamilyID:            familyID,
		RefreshVerifierHash: sha256Hex(refreshJWT),
		DeviceFingerprint:   txn.DeviceFingerprint,
		ExpiresAt:           refreshExp,
	}
	if err := h.storage.InsertSession(ctx, session); err != nil {
		h.deny(c, http.StatusInternalServerError, errCodeInternal, "persist session failed", "client:"+txn.ClientID, auditTxnDetail(txn))
		return
	}

	// work 桌面上下文键 = issuer|subject|tenant|generation（DATA-120c），
	// 受管技能目录按 opc/<tenant>/<publish_id> 命名；租户从该主体 active
	// entitlement 集推导（与 INT-142 技能/知识桥同一来源），单租户直出、
	// 多租户取排序首个（确定性），无授权租户时为空串由客户端按无技能处理。
	// subject/spaces/authz_version 同包进响应：work 客户端据其渲染企业
	// 区并初始化上下文（fake 平台一贯带这些字段，真平台此前漏发）。
	link, err := h.storage.GetIdentityLink(ctx, *txn.IdentityLinkID)
	if err != nil {
		h.deny(c, http.StatusInternalServerError, errCodeInternal, "identity link missing", "client:"+txn.ClientID, auditTxnDetail(txn))
		return
	}
	ents, err := h.storage.ListActiveManagedEntitlementsBySubject(ctx, link.ImmutableSubject)
	if err != nil {
		h.deny(c, http.StatusInternalServerError, errCodeInternal, "list entitlements failed", "client:"+txn.ClientID, auditTxnDetail(txn))
		return
	}
	// 真实姓名/邮箱取自 knowledge 用户行（与 code-exchange 同一来源），
	// work 客户端据此在企业区展示企业身份；解析失败留空串。
	email, displayName := "", ""
	if ku, uerr := kbridge.NewWeKnoraStore(h.storage).GetUserByID(ctx, link.KnowledgeUserID); uerr == nil && ku != nil {
		email, displayName = ku.Email, ku.Username
	}
	tenantSet := make(map[string]struct{}, len(ents))
	capSet := make(map[string]struct{}, len(ents))
	authzVersion := int64(0)
	for _, e := range ents {
		if e.TenantID != "" {
			tenantSet[e.TenantID] = struct{}{}
		}
		if e.Capability != "" {
			capSet[e.Capability] = struct{}{}
		}
		if e.AuthzVersion > authzVersion {
			authzVersion = e.AuthzVersion
		}
	}
	tenants := make([]string, 0, len(tenantSet))
	for t := range tenantSet {
		tenants = append(tenants, t)
	}
	sort.Strings(tenants)
	capabilities := make([]string, 0, len(capSet))
	for cap := range capSet {
		capabilities = append(capabilities, cap)
	}
	sort.Strings(capabilities)
	tenant := ""
	if len(tenants) > 0 {
		tenant = tenants[0]
	}

	h.audit(ctx, *txn.IdentityLinkID, "integration.auth.token.issued", "POST /integration/v1/auth/token", map[string]interface{}{
		"result":       "allowed",
		"login_txn_id": txn.LoginTxnID,
		"audience":     audienceWork,
		"session_id":   sessionID,
		"family_id":    familyID,
		"expires_in_s": int(accessTokenTTL.Seconds()),
	}, c)

	c.JSON(http.StatusOK, gin.H{
		"access_token":  accessJWT,
		"refresh_token": refreshJWT,
		"expires_in":    int(accessTokenTTL.Seconds()),
		"subject":       link.ImmutableSubject,
		"spaces":        tenants,
		"authz_version": authzVersion,
		"capabilities":  capabilities,
		"email":         email,
		"name":          displayName,
		"tenant":        tenant,
		"tenants":       tenants,
	})
}

// issueExchangeToken 为 code-backend 受众签发一次性兑换材料（120s，
// aud=code-backend，绑定 txn+subject+device）。材料哈希暂存 sessions 表做原子消费。
func (h *IntegrationHandler) issueExchangeToken(c *gin.Context, ctx context.Context, txn *model.AuthTransaction) {
	now := time.Now()
	sessionID := uuid.NewString()
	exchangeJWT, err := h.signPlatformJWT(map[string]interface{}{
		"sub":    *txn.IdentityLinkID,
		"sid":    sessionID,
		"txn":    txn.LoginTxnID,
		"device": txn.DeviceFingerprint,
		"aud":    audienceCodeBackend,
		"type":   "exchange",
		"exp":    now.Add(exchangeTokenTTL).Unix(),
		"iat":    now.Unix(),
		"jti":    uuid.NewString(),
	})
	if err != nil {
		h.deny(c, http.StatusInternalServerError, errCodeInternal, "sign exchange token failed", "client:"+txn.ClientID, auditTxnDetail(txn))
		return
	}
	material := &model.Session{
		SessionID:           sessionID,
		IdentityLinkID:      *txn.IdentityLinkID,
		FamilyID:            uuid.NewString(),
		RefreshVerifierHash: sha256Hex(exchangeJWT),
		DeviceFingerprint:   txn.DeviceFingerprint,
		ExpiresAt:           now.Add(exchangeTokenTTL),
	}
	if err := h.storage.InsertSession(ctx, material); err != nil {
		h.deny(c, http.StatusInternalServerError, errCodeInternal, "persist exchange material failed", "client:"+txn.ClientID, auditTxnDetail(txn))
		return
	}

	h.audit(ctx, *txn.IdentityLinkID, "integration.auth.exchange.issued", "POST /integration/v1/auth/token", map[string]interface{}{
		"result":       "allowed",
		"login_txn_id": txn.LoginTxnID,
		"audience":     audienceCodeBackend,
		"expires_in_s": int(exchangeTokenTTL.Seconds()),
	}, c)

	c.JSON(http.StatusOK, gin.H{"exchange_token": exchangeJWT})
}

// ---------- POST /integration/v1/auth/refresh ----------

// Refresh 校验 refresh token 并轮换会话族：作废旧 verifier、返回新 access+refresh。
// 收到的 verifier 命中历史值（previous_verifier_hash）时判定复用，撤销整个 family。
func (h *IntegrationHandler) Refresh(c *gin.Context) {
	ctx := c.Request.Context()
	var req refreshRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.RefreshToken == "" {
		h.deny(c, http.StatusUnauthorized, errCodeSessionRevoked, "refresh_token is required", "anonymous", nil)
		return
	}

	claims, err := h.parsePlatformJWT(req.RefreshToken)
	if err != nil || claims["type"] != "refresh" {
		h.deny(c, http.StatusUnauthorized, errCodeSessionRevoked, "invalid refresh token", "anonymous", nil)
		return
	}
	sid, _ := claims["sid"].(string)
	sub, _ := claims["sub"].(string)
	if sid == "" {
		h.deny(c, http.StatusUnauthorized, errCodeSessionRevoked, "refresh token missing sid", "anonymous", nil)
		return
	}
	session, err := h.storage.GetSession(ctx, sid)
	if err != nil {
		h.deny(c, http.StatusUnauthorized, errCodeSessionRevoked, "session not found", "anonymous", nil)
		return
	}
	if session.RevokedAt != nil {
		h.deny(c, http.StatusUnauthorized, errCodeSessionRevoked, "session revoked: "+stringValue(session.RevokeReason), session.IdentityLinkID, auditSessionDetail(session))
		return
	}
	if !time.Now().Before(session.ExpiresAt) {
		h.deny(c, http.StatusUnauthorized, errCodeSessionRevoked, "session expired", session.IdentityLinkID, auditSessionDetail(session))
		return
	}

	// 撤权覆盖（BND-111）：refresh 重查身份关联与授权——主体停用 → 401 SESSION_REVOKED；
	// 无任何 active entitlement → 403 ENTITLEMENT_REVOKED。被撤主体不得在 refresh 有效期内续签。
	link, err := h.storage.GetIdentityLink(ctx, session.IdentityLinkID)
	if err != nil || link.Status != "active" {
		h.deny(c, http.StatusUnauthorized, errCodeSessionRevoked, "identity link missing or disabled", session.IdentityLinkID, auditSessionDetail(session))
		return
	}
	ents, err := h.storage.ListActiveManagedEntitlementsBySubject(ctx, link.ImmutableSubject)
	if err != nil {
		h.deny(c, http.StatusInternalServerError, errCodeInternal, "list entitlements failed", session.IdentityLinkID, auditSessionDetail(session))
		return
	}
	if len(ents) == 0 {
		h.deny(c, http.StatusForbidden, errCodeEntitlementRevoked, "no active entitlement for subject", session.IdentityLinkID, auditSessionDetail(session))
		return
	}

	presentedHash := sha256Hex(req.RefreshToken)
	switch {
	case presentedHash == session.RefreshVerifierHash:
		h.rotateRefresh(c, ctx, session, sub)
	case session.PreviousVerifierHash != nil && presentedHash == *session.PreviousVerifierHash:
		h.revokeFamilyForReuse(c, ctx, session)
	default:
		h.deny(c, http.StatusUnauthorized, errCodeSessionRevoked, "refresh token does not match current or previous verifier", session.IdentityLinkID, auditSessionDetail(session))
	}
}

func (h *IntegrationHandler) rotateRefresh(c *gin.Context, ctx context.Context, session *model.Session, sub string) {
	now := time.Now()
	accessJWT, err := h.signPlatformJWT(map[string]interface{}{
		"sub":  session.IdentityLinkID,
		"sid":  session.SessionID,
		"aud":  audienceWork,
		"type": "access",
		"exp":  now.Add(accessTokenTTL).Unix(),
		"iat":  now.Unix(),
		"jti":  uuid.NewString(),
	})
	if err != nil {
		h.deny(c, http.StatusInternalServerError, errCodeInternal, "sign access token failed", session.IdentityLinkID, auditSessionDetail(session))
		return
	}
	refreshJWT, err := h.signPlatformJWT(map[string]interface{}{
		"sub":  session.IdentityLinkID,
		"sid":  session.SessionID,
		"aud":  audienceWork,
		"type": "refresh",
		"exp":  session.ExpiresAt.Unix(),
		"iat":  now.Unix(),
		"jti":  uuid.NewString(),
	})
	if err != nil {
		h.deny(c, http.StatusInternalServerError, errCodeInternal, "sign refresh token failed", session.IdentityLinkID, auditSessionDetail(session))
		return
	}
	// 先签新 JWT，再以其哈希参与原子轮换：并发双刷新只有一个能改写 verifier。
	rows, err := h.storage.RotateSessionVerifier(ctx, session.SessionID, session.RefreshVerifierHash, sha256Hex(refreshJWT))
	if err != nil || rows == 0 {
		h.deny(c, http.StatusUnauthorized, errCodeSessionRevoked, "concurrent rotation lost, refresh denied", session.IdentityLinkID, auditSessionDetail(session))
		return
	}

	h.audit(ctx, session.IdentityLinkID, "integration.auth.refresh.rotated", "POST /integration/v1/auth/refresh", map[string]interface{}{
		"result":     "allowed",
		"session_id": session.SessionID,
		"family_id":  session.FamilyID,
		"subject":    sub,
	}, c)

	c.JSON(http.StatusOK, gin.H{
		"access_token":  accessJWT,
		"refresh_token": refreshJWT,
		"expires_in":    int(accessTokenTTL.Seconds()),
	})
}

// revokeFamilyForReuse refresh 复用检测：历史 verifier 被呈上即撤销整个 family。
func (h *IntegrationHandler) revokeFamilyForReuse(c *gin.Context, ctx context.Context, session *model.Session) {
	if _, err := h.storage.RevokeSessionFamily(ctx, session.FamilyID, "REFRESH_REUSE_DETECTED"); err != nil {
		log.Printf("[Integration] revoke family on reuse failed (family=%s): %v", session.FamilyID, err)
	}
	h.audit(ctx, session.IdentityLinkID, "integration.auth.refresh.reuse_detected", "POST /integration/v1/auth/refresh", map[string]interface{}{
		"result":     "denied",
		"session_id": session.SessionID,
		"family_id":  session.FamilyID,
		"error":      errCodeRefreshReuseDetected,
	}, c)
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{
		"code":       errCodeRefreshReuseDetected,
		"message":    "refresh token reuse detected, session family revoked",
		"request_id": integrationRequestID(c),
	}})
}

// ---------- POST /integration/v1/auth/logout ----------

// Logout 撤销 refresh_token 或 access_token 所属的整个会话族（reason=logout）。
func (h *IntegrationHandler) Logout(c *gin.Context) {
	ctx := c.Request.Context()
	var req logoutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.deny(c, http.StatusBadRequest, errCodeAuthStateMismatch, "invalid JSON body: "+err.Error(), "anonymous", nil)
		return
	}
	raw := req.RefreshToken
	if raw == "" {
		raw = req.AccessToken
	}
	if raw == "" {
		h.deny(c, http.StatusBadRequest, errCodeAuthStateMismatch, "refresh_token or access_token is required", "anonymous", nil)
		return
	}

	claims, err := h.parsePlatformJWT(raw)
	if err != nil {
		h.deny(c, http.StatusUnauthorized, errCodeSessionRevoked, "invalid token", "anonymous", nil)
		return
	}
	typ, _ := claims["type"].(string)
	if typ != "refresh" && typ != "access" {
		h.deny(c, http.StatusUnauthorized, errCodeSessionRevoked, "token type must be access or refresh", "anonymous", nil)
		return
	}
	sid, _ := claims["sid"].(string)
	if sid == "" {
		h.deny(c, http.StatusUnauthorized, errCodeSessionRevoked, "token missing sid", "anonymous", nil)
		return
	}
	session, err := h.storage.GetSession(ctx, sid)
	if err != nil {
		// 会话已不存在：logout 幂等成功，无族可撤。
		h.audit(ctx, "anonymous", "integration.auth.logout", "POST /integration/v1/auth/logout", map[string]interface{}{
			"result":     "allowed",
			"session_id": sid,
			"revoked":    0,
		}, c)
		c.JSON(http.StatusOK, gin.H{"revoked": 0})
		return
	}

	rows, err := h.storage.RevokeSessionFamily(ctx, session.FamilyID, "logout")
	if err != nil {
		h.deny(c, http.StatusInternalServerError, errCodeInternal, "revoke session family failed", session.IdentityLinkID, auditSessionDetail(session))
		return
	}
	h.audit(ctx, session.IdentityLinkID, "integration.auth.logout", "POST /integration/v1/auth/logout", map[string]interface{}{
		"result":     "allowed",
		"session_id": session.SessionID,
		"family_id":  session.FamilyID,
		"revoked":    rows,
	}, c)
	c.JSON(http.StatusOK, gin.H{"revoked": rows})
}

// ---------- GET /integration/v1/session ----------

// Session 返回当前 access token 对应的身份视图：主体、active 受管租约空间、
// 会话授权版本与能力清单。
func (h *IntegrationHandler) Session(c *gin.Context) {
	ctx := c.Request.Context()
	claims, ok := h.bearerClaims(c, "access")
	if !ok {
		return
	}
	sid, _ := claims["sid"].(string)
	sub, _ := claims["sub"].(string)
	if sid == "" || sub == "" {
		h.deny(c, http.StatusUnauthorized, errCodeSessionRevoked, "access token missing sid/sub", "anonymous", nil)
		return
	}
	session, err := h.storage.GetSession(ctx, sid)
	if err != nil || session.IdentityLinkID != sub {
		h.deny(c, http.StatusUnauthorized, errCodeSessionRevoked, "session not found", "anonymous", nil)
		return
	}
	if session.RevokedAt != nil || !time.Now().Before(session.ExpiresAt) {
		h.deny(c, http.StatusUnauthorized, errCodeSessionRevoked, "session revoked or expired", session.IdentityLinkID, auditSessionDetail(session))
		return
	}

	link, err := h.storage.GetIdentityLink(ctx, sub)
	if err != nil {
		h.deny(c, http.StatusUnauthorized, errCodeSessionRevoked, "identity link missing", sub, auditSessionDetail(session))
		return
	}
	// 撤权覆盖（BND-111）：身份停用 → 401；无任何 active entitlement → 403。
	if link.Status != "active" {
		h.deny(c, http.StatusUnauthorized, errCodeSessionRevoked, "identity link disabled", sub, auditSessionDetail(session))
		return
	}
	ents, err := h.storage.ListActiveManagedEntitlementsBySubject(ctx, link.ImmutableSubject)
	if err != nil {
		h.deny(c, http.StatusInternalServerError, errCodeInternal, "list entitlements failed", sub, nil)
		return
	}
	if len(ents) == 0 {
		h.deny(c, http.StatusForbidden, errCodeEntitlementRevoked, "no active entitlement for subject", sub, auditSessionDetail(session))
		return
	}

	// spaces 按 tenant 去重（同 tenant 多 capability 只出一个条目）
	spaces := make([]string, 0, len(ents))
	seenTenants := make(map[string]struct{}, len(ents))
	capSet := make(map[string]struct{})
	authzVersion := session.AuthzVersion
	for _, e := range ents {
		if _, seen := seenTenants[e.TenantID]; !seen {
			seenTenants[e.TenantID] = struct{}{}
			spaces = append(spaces, e.TenantID)
		}
		if e.Capability != "" {
			capSet[e.Capability] = struct{}{}
		}
		if e.AuthzVersion > authzVersion {
			authzVersion = e.AuthzVersion
		}
	}
	sort.Strings(spaces)
	capabilities := make([]string, 0, len(capSet))
	for cap := range capSet {
		capabilities = append(capabilities, cap)
	}
	sort.Strings(capabilities)

	now := time.Now()
	_ = h.storage.TouchSessionLastSeen(ctx, session.SessionID, now)

	h.audit(ctx, sub, "integration.auth.session.viewed", "GET /integration/v1/session", map[string]interface{}{
		"result":     "allowed",
		"session_id": sid,
	}, c)

	c.JSON(http.StatusOK, gin.H{
		"identity_link_id": link.ID,
		"subject":          link.ImmutableSubject,
		"spaces":           spaces,
		"authz_version":    authzVersion,
		"capabilities":     capabilities,
	})
}

// ---------- POST /integration/v1/auth/code-exchange ----------

// CodeExchange code 后端服务身份消费一次性兑换材料。调用方必须持有 system_admin
// 服务 token（Admin.Key 体系）；材料原子消费后返回主体与空间视图，不签发任何平台令牌。
func (h *IntegrationHandler) CodeExchange(c *gin.Context) {
	ctx := c.Request.Context()

	raw := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	if raw == "" {
		h.deny(c, http.StatusUnauthorized, errCodeSessionRevoked, "missing service bearer token", "anonymous", nil)
		return
	}
	svcClaims, err := adminauth.ParseServiceCredential(h.cfg.Admin.Key, h.cfg.Server.ServiceToken, raw)
	if err != nil {
		h.deny(c, http.StatusUnauthorized, errCodeSessionRevoked, "invalid service token: "+err.Error(), "anonymous", nil)
		return
	}
	if svcClaims.Role != adminauth.RoleSystemAdmin {
		h.deny(c, http.StatusForbidden, errCodeSessionRevoked, "service token requires system_admin role", svcClaims.Subject, nil)
		return
	}

	var req codeExchangeRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.ExchangeToken == "" {
		h.deny(c, http.StatusBadRequest, errCodeAuthStateMismatch, "exchange_token is required", svcClaims.Subject, nil)
		return
	}

	claims, err := h.parsePlatformJWT(req.ExchangeToken)
	if err != nil || claims["type"] != "exchange" || claims["aud"] != audienceCodeBackend {
		h.deny(c, http.StatusBadRequest, errCodeAuthStateMismatch, "invalid exchange token (audience/type mismatch)", svcClaims.Subject, nil)
		return
	}
	sid, _ := claims["sid"].(string)
	sub, _ := claims["sub"].(string)
	txnID, _ := claims["txn"].(string)
	if sid == "" || sub == "" || txnID == "" {
		h.deny(c, http.StatusBadRequest, errCodeAuthStateMismatch, "exchange token missing binding claims", svcClaims.Subject, nil)
		return
	}

	session, err := h.storage.GetSession(ctx, sid)
	if err != nil || session.IdentityLinkID != sub {
		h.deny(c, http.StatusBadRequest, errCodeAuthStateMismatch, "exchange material binding mismatch", svcClaims.Subject, nil)
		return
	}
	deviceClaim, _ := claims["device"].(string)
	txn, err := h.storage.GetAuthTransaction(ctx, txnID)
	if err != nil || txn.Audience != audienceCodeBackend ||
		txn.DeviceFingerprint != deviceClaim || txn.Status != "completed" {
		h.deny(c, http.StatusBadRequest, errCodeAuthStateMismatch, "exchange transaction binding mismatch", svcClaims.Subject, nil)
		return
	}

	// PKCE 复核（INT-101g 增补）：事务存了 code_challenge 时 code_verifier 必传且 S256 匹配。
	// 先验后消费：verifier 错误不烧毁一次性材料，合法持有者随后仍可完成兑换。
	if txn.CodeChallenge != "" {
		if req.CodeVerifier == "" {
			h.deny(c, http.StatusBadRequest, errCodeAuthStateMismatch, "code_verifier is required (transaction holds code_challenge)", svcClaims.Subject, nil)
			return
		}
		if pkceS256(req.CodeVerifier) != txn.CodeChallenge {
			h.deny(c, http.StatusBadRequest, errCodeAuthStateMismatch, "PKCE verification failed", svcClaims.Subject, nil)
			return
		}
	}

	rows, err := h.storage.ConsumeExchangeMaterial(ctx, sid, sha256Hex(req.ExchangeToken))
	if err != nil || rows == 0 {
		h.deny(c, http.StatusBadRequest, errCodeAuthCodeReplayed, "exchange token replayed or expired", svcClaims.Subject, nil)
		return
	}

	link, err := h.storage.GetIdentityLink(ctx, sub)
	if err != nil {
		h.deny(c, http.StatusInternalServerError, errCodeInternal, "identity link missing", svcClaims.Subject, nil)
		return
	}
	// 真实姓名/邮箱取自 knowledge 用户行（identity link 的供给目标），
	// 让 code 后端把占位 enterprise-<subject>@platform.local 换成企业身份
	// 展示；解析失败留空，客户端回退占位值，不阻断兑换。
	email, displayName := "", ""
	if ku, uerr := kbridge.NewWeKnoraStore(h.storage).GetUserByID(ctx, link.KnowledgeUserID); uerr == nil && ku != nil {
		email, displayName = ku.Email, ku.Username
	}
	ents, err := h.storage.ListActiveManagedEntitlementsBySubject(ctx, link.ImmutableSubject)
	if err != nil {
		h.deny(c, http.StatusInternalServerError, errCodeInternal, "list entitlements failed", svcClaims.Subject, nil)
		return
	}
	// spaces 按 tenant 去重；role 直出 knowledge 成员角色（tenant_members.role），
	// tenant 非数字或查无成员关系时 role 为空串。
	spaces := make([]codeSpaceView, 0, len(ents))
	seenTenants := make(map[string]struct{}, len(ents))
	authzVersion := int64(0)
	for _, e := range ents {
		if _, seen := seenTenants[e.TenantID]; seen {
			continue
		}
		seenTenants[e.TenantID] = struct{}{}
		role := ""
		if tenantNumeric, perr := strconv.ParseUint(e.TenantID, 10, 64); perr == nil {
			if r, rerr := h.storage.GetTenantMembershipRole(ctx, link.KnowledgeUserID, tenantNumeric); rerr != nil {
				log.Printf("[Integration] code-exchange membership role lookup failed (tenant=%s): %v", e.TenantID, rerr)
			} else {
				role = r
			}
		}
		spaces = append(spaces, codeSpaceView{TenantID: e.TenantID, Role: role})
		if e.AuthzVersion > authzVersion {
			authzVersion = e.AuthzVersion
		}
	}
	sort.Slice(spaces, func(i, j int) bool { return spaces[i].TenantID < spaces[j].TenantID })

	h.audit(ctx, svcClaims.Subject, "integration.auth.code_exchange.consumed", "POST /integration/v1/auth/code-exchange", map[string]interface{}{
		"result":           "allowed",
		"login_txn_id":     txnID,
		"identity_link_id": sub,
		"spaces":           len(spaces),
	}, c)

	c.JSON(http.StatusOK, gin.H{
		"subject":           link.ImmutableSubject,
		"identity_link_id":  link.ID,
		"knowledge_user_id": link.KnowledgeUserID,
		"code_user_id":      codeUserIDValue(link.CodeUserID),
		"spaces":            spaces,
		"authz_version":     authzVersion,
		"email":             email,
		"name":              displayName,
	})
}

// ---------- 助手 ----------

// bearerClaims 解析并校验 Authorization: Bearer 平台令牌（type 匹配）。
func (h *IntegrationHandler) bearerClaims(c *gin.Context, typ string) (jwt.MapClaims, bool) {
	raw := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	if raw == "" {
		h.deny(c, http.StatusUnauthorized, errCodeSessionRevoked, "missing bearer token", "anonymous", nil)
		return nil, false
	}
	claims, err := h.parsePlatformJWT(raw)
	if err != nil || claims["type"] != typ {
		h.deny(c, http.StatusUnauthorized, errCodeSessionRevoked, "invalid bearer token", "anonymous", nil)
		return nil, false
	}
	return claims, true
}

// signPlatformJWT 以 Auth.Key 签发平台自有 HS256 JWT。
func (h *IntegrationHandler) signPlatformJWT(claims jwt.MapClaims) (string, error) {
	if h.cfg.Auth.Key == "" {
		return "", fmt.Errorf("auth key is not configured")
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(h.cfg.Auth.Key))
}

// parsePlatformJWT 校验平台 JWT 的 HS256 签名与 exp。
func (h *IntegrationHandler) parsePlatformJWT(raw string) (jwt.MapClaims, error) {
	if h.cfg.Auth.Key == "" {
		return nil, fmt.Errorf("auth key is not configured")
	}
	claims := jwt.MapClaims{}
	token, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(h.cfg.Auth.Key), nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil || !token.Valid {
		return nil, fmt.Errorf("invalid platform token")
	}
	return claims, nil
}

// deny 统一错误信封 {error:{code,message,request_id}} 并写拒绝审计。
func (h *IntegrationHandler) deny(c *gin.Context, status int, code, message, actor string, detail map[string]interface{}) {
	ctx := c.Request.Context()
	payload := map[string]interface{}{
		"result": "denied",
		"error":  code,
	}
	for k, v := range detail {
		payload[k] = v
	}
	h.audit(ctx, actor, "integration.auth.denied", c.Request.Method+" "+c.FullPath(), payload, c)
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{
		"code":       code,
		"message":    message,
		"request_id": integrationRequestID(c),
	}})
}

// audit 写审计事件（best-effort，失败只记日志）。
func (h *IntegrationHandler) audit(ctx context.Context, actor, action, target string, detail map[string]interface{}, c *gin.Context) {
	if h.storage == nil {
		return
	}
	after, _ := json.Marshal(detail)
	event := &model.AuditEvent{
		Actor:     actor,
		Action:    action,
		Target:    target,
		After:     string(after),
		RequestID: integrationRequestID(c),
	}
	if err := h.storage.InsertAuditEvent(ctx, event); err != nil {
		log.Printf("[Integration] failed to write audit %s: %v", action, err)
	}
}

// integrationRequestID 取 X-Request-ID，缺省生成。
func integrationRequestID(c *gin.Context) string {
	if id := c.GetHeader("X-Request-ID"); id != "" {
		return id
	}
	return uuid.NewString()
}

// externalBaseURL 返回配置的本服务对外规范地址（server.external_base_url，去尾部斜杠）。
// 回跳/授权 URL 拼接只信配置，不信请求 Host / X-Forwarded-Host（两者均可伪造）。
func (h *IntegrationHandler) externalBaseURL() string {
	if h.cfg == nil {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(h.cfg.Server.ExternalBaseURL), "/")
}

// ValidIntegrationCallbackURI 校验回跳地址必须是 http://127.0.0.1:<port>/callback（OPEN-104）。
func ValidIntegrationCallbackURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" || u.Hostname() != "127.0.0.1" {
		return false
	}
	port := u.Port()
	if port == "" {
		return false
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return false
	}
	return u.Path == "/callback" && u.RawQuery == "" && u.Fragment == ""
}

// pkceS256 计算 RFC 7636 PKCE S256 challenge。
func pkceS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum[:])
}

// randomURLToken 生成 n 字节随机数的 base64url（无填充）表示。
func randomURLToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func stringValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func codeUserIDValue(p *string) interface{} {
	if p == nil {
		return nil
	}
	return *p
}

func auditTxnDetail(txn *model.AuthTransaction) map[string]interface{} {
	return map[string]interface{}{"login_txn_id": txn.LoginTxnID}
}

func auditSessionDetail(session *model.Session) map[string]interface{} {
	return map[string]interface{}{
		"session_id": session.SessionID,
		"family_id":  session.FamilyID,
	}
}
