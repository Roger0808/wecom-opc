// TASK-P05/P06（INT-102、DATA-101d/f、BND-102/110~112、REQ-003）：
// 服务间接口组 /integration/v1/internal/*。
//
// 认证：全部端点要求 Bearer 服务 token（复用 adminauth 体系，角色 system_admin；
// 错误信封与 /integration/v1 一致：{error:{code,message,request_id}}）。
//
// 端点：
//   - POST /internal/grants            签发 30s 任务租约（lease_token = HS256 JWT, type=task_lease）；
//   - POST /internal/grants/:id/renew  续期（宽容窗 15s；重校验授权版本，已撤销即拒绝并落 revoked_at）；
//   - GET  /internal/authz             单 subject 授权视图；
//   - POST /internal/authz             批量授权视图 {subjects[]}；
//   - POST /internal/revoke            撤权：一个事务先持久化（identity 停用 + entitlement revoked_at
//   - authz_epochs 版本提升），提交后异步通知与上游 token 撤销（TokenRevoker，P09 注入桥实现）。
//
// 计时预算（BND-110）：lease 30s / renew 宽容窗 15s / 版本缓存 ≤5s 全部经由注入 Clock，
// 网关拒绝 T+≤5s 由「无缓存 + 事务先提交」天然满足。
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/adminauth"
	"weknora-tokendance/pkg/authz"
	"weknora-tokendance/pkg/identity"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// 本接口组的错误码（BND-180 枚举扩展）。
const (
	errCodeInvalidRequest     = "INVALID_REQUEST"
	errCodeEntitlementRevoked = "ENTITLEMENT_REVOKED"
	errCodeLeaseExpired       = "LEASE_EXPIRED"
	errCodeLeaseConflict      = "LEASE_CONFLICT"
	errCodeGrantNotFound      = "GRANT_NOT_FOUND"
)

// 租约计时（BND-102/BND-110）。
const (
	taskLeaseTTL       = 30 * time.Second // 租约有效期
	renewGraceWindow   = 15 * time.Second // 续期宽容窗：lease_expires_at > now-15s 才可续
	maxVersionCacheTTL = 5 * time.Second  // 版本读缓存上限（P06.2）
)

// context key：服务 token 主体。
const internalServiceSubjectKey = "internal.service_subject"

// InternalHandler /integration/v1/internal 处理器。
type InternalHandler struct {
	cfg      *config.Config
	storage  *storage.Storage
	identity *identity.Service

	clock     authz.Clock
	versions  *authz.VersionSource // 版本读路径（签发/续期用 CurrentFresh 无缓存直读）
	checker   *authz.EntitlementChecker
	notifier  *authz.Notifier
	revoker   authz.TokenRevoker
	renewGate func() // 测试专用：原子续期前闸门（生产 nil）
}

// InternalOption 覆盖默认组装（测试注入 FakeClock / 通知通道 / 撤销器）。
type InternalOption func(*InternalHandler)

// WithClock 注入时钟；如需配合 WithVersionCacheTTL，必须先调用本选项。
func WithClock(clock authz.Clock) InternalOption {
	return func(h *InternalHandler) { h.clock = clock }
}

// WithVersionCacheTTL 设置版本读缓存（0=不缓存，上限 5s）；须在 WithClock 之后调用。
func WithVersionCacheTTL(ttl time.Duration) InternalOption {
	return func(h *InternalHandler) {
		if ttl > maxVersionCacheTTL {
			ttl = maxVersionCacheTTL
		}
		h.versions = authz.NewVersionSource(h.storage, h.clock, ttl)
		h.checker = authz.NewEntitlementChecker(h.storage, h.versions)
	}
}

// WithNotifier 注入失效通知中心。
func WithNotifier(n *authz.Notifier) InternalOption {
	return func(h *InternalHandler) { h.notifier = n }
}

// WithTokenRevoker 注入上游 token 撤销器。
func WithTokenRevoker(r authz.TokenRevoker) InternalOption {
	return func(h *InternalHandler) { h.revoker = r }
}

// withRenewGate 测试专用闸门：在租约原子续期前调用，用于并发续期测试对齐时序（生产为 nil）。
func withRenewGate(gate func()) InternalOption {
	return func(h *InternalHandler) { h.renewGate = gate }
}

// Notifier 返回失效通知中心（main 启动 30s TTL 重试循环用）。
func (h *InternalHandler) Notifier() *authz.Notifier { return h.notifier }

// NewInternalHandler 组装内部接口组处理器。
// 生产默认：RealClock、版本不缓存、NoopTokenRevoker；上游 token 撤销由调用方
// 注入 pkg/kbridge.Bridge（P09 兼容桥，见 main.go 装配与 WithTokenRevoker）。
func NewInternalHandler(cfg *config.Config, s *storage.Storage, opts ...InternalOption) *InternalHandler {
	h := &InternalHandler{
		cfg:     cfg,
		storage: s,
		clock:   authz.RealClock{},
	}
	h.identity = identity.NewService(s)
	h.versions = authz.NewVersionSource(s, h.clock, 0)
	h.checker = authz.NewEntitlementChecker(s, h.versions)
	h.notifier = authz.NewNotifier(h.clock)
	h.revoker = authz.NoopTokenRevoker{}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// ---------- 服务 token 中间件 ----------

// RequireServiceToken 校验 Bearer 服务 token（adminauth 体系 + system_admin 角色）。
// 挂在 /internal 路由组上对所有端点生效。
func (h *InternalHandler) RequireServiceToken(c *gin.Context) {
	raw := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	if raw == "" {
		h.deny(c, http.StatusUnauthorized, errCodeSessionRevoked, "missing service bearer token", "anonymous")
		return
	}
	svcClaims, err := adminauth.ParseServiceCredential(h.cfg.Admin.Key, h.cfg.Server.ServiceToken, raw)
	if err != nil {
		h.deny(c, http.StatusUnauthorized, errCodeSessionRevoked, "invalid service token: "+err.Error(), "anonymous")
		return
	}
	if svcClaims.Role != adminauth.RoleSystemAdmin {
		h.deny(c, http.StatusForbidden, errCodeSessionRevoked, "service token requires system_admin role", svcClaims.Subject)
		return
	}
	c.Set(internalServiceSubjectKey, svcClaims.Subject)
	c.Next()
}

func (h *InternalHandler) serviceSubject(c *gin.Context) string {
	v, _ := c.Get(internalServiceSubjectKey)
	subject, _ := v.(string)
	if subject == "" {
		return "anonymous"
	}
	return subject
}

// ---------- POST /integration/v1/internal/grants ----------

type grantRequest struct {
	Subject           string   `json:"subject"`
	TenantID          string   `json:"tenant_id"`
	WorkspaceID       string   `json:"workspace_id"`
	TaskID            string   `json:"task_id"`
	RuntimeID         string   `json:"runtime_id"`
	OriginatingUserID string   `json:"originating_user_id"`
	Capabilities      []string `json:"capabilities"`
}

// IssueGrant 签发任务租约：subject 必须有 active identity_link 且 (subject, tenant) 存在
// active entitlement（否则 403 ENTITLEMENT_REVOKED）；originating_user_id 必填。
// 租约行 lease_expires_at=now+30s，authz_version 取当前版本（无缓存直读）。
func (h *InternalHandler) IssueGrant(c *gin.Context) {
	ctx := c.Request.Context()
	var req grantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.deny(c, http.StatusBadRequest, errCodeInvalidRequest, "invalid JSON body: "+err.Error(), h.serviceSubject(c))
		return
	}
	if req.Subject == "" || req.TenantID == "" {
		h.deny(c, http.StatusBadRequest, errCodeInvalidRequest, "subject and tenant_id are required", h.serviceSubject(c))
		return
	}
	if req.OriginatingUserID == "" {
		h.deny(c, http.StatusBadRequest, errCodeInvalidRequest, "originating_user_id is required", h.serviceSubject(c))
		return
	}

	if _, err := h.checker.RequireEntitlement(ctx, req.Subject, req.TenantID, ""); err != nil {
		h.denyGrant(c, ctx, req, err)
		return
	}
	version, err := h.versions.CurrentFresh(ctx, req.Subject)
	if err != nil {
		h.deny(c, http.StatusInternalServerError, errCodeInternal, "read authz version failed", req.Subject)
		return
	}

	now := h.clock.Now()
	grantID := uuid.NewString()
	capabilities := normalizeCapabilities(req.Capabilities)
	var capsJSON *string
	if capabilities != nil {
		raw, _ := json.Marshal(capabilities)
		text := string(raw)
		capsJSON = &text
	}
	grant := &model.RuntimeGrant{
		GrantID:           grantID,
		TaskID:            stringPtrOrNil(req.TaskID),
		WorkspaceID:       stringPtrOrNil(req.WorkspaceID),
		RuntimeID:         stringPtrOrNil(req.RuntimeID),
		Subject:           req.Subject,
		TenantID:          req.TenantID,
		OriginatingUserID: req.OriginatingUserID,
		Capabilities:      capsJSON,
		AuthzVersion:      version,
		LeaseExpiresAt:    now.Add(taskLeaseTTL),
	}
	if err := h.storage.InsertRuntimeGrant(ctx, grant); err != nil {
		h.deny(c, http.StatusInternalServerError, errCodeInternal, "persist runtime grant failed", req.Subject)
		return
	}

	leaseToken, err := h.signLeaseToken(grant, now)
	if err != nil {
		h.deny(c, http.StatusInternalServerError, errCodeInternal, "sign lease token failed", req.Subject)
		return
	}

	h.audit(ctx, h.serviceSubject(c), "integration.internal.grant.issued", "POST /integration/v1/internal/grants", map[string]interface{}{
		"result":           "allowed",
		"grant_id":         grantID,
		"subject":          req.Subject,
		"tenant_id":        req.TenantID,
		"authz_version":    version,
		"lease_expires_at": grant.LeaseExpiresAt.Format(time.RFC3339),
	}, c)

	c.JSON(http.StatusOK, gin.H{
		"grant_id":         grantID,
		"lease_token":      leaseToken,
		"lease_expires_at": grant.LeaseExpiresAt,
	})
}

func (h *InternalHandler) denyGrant(c *gin.Context, ctx context.Context, req grantRequest, cause error) {
	code := errCodeEntitlementRevoked
	message := "subject entitlement revoked or identity disabled"
	if errors.Is(cause, authz.ErrIdentityDisabled) {
		message = "subject identity link is not active"
	} else if errors.Is(cause, authz.ErrEntitlementMissing) {
		message = "no active entitlement for subject in tenant"
	}
	h.audit(ctx, h.serviceSubject(c), "integration.internal.grant.denied", "POST /integration/v1/internal/grants", map[string]interface{}{
		"result":    "denied",
		"subject":   req.Subject,
		"tenant_id": req.TenantID,
		"error":     code,
		"cause":     cause.Error(),
	}, c)
	h.deny(c, http.StatusForbidden, code, message, req.Subject)
}

// ---------- POST /integration/v1/internal/grants/:id/renew ----------

// RenewGrant 续期租约：
//   - grant 不存在 → 404 GRANT_NOT_FOUND；已撤销 → 403 ENTITLEMENT_REVOKED；
//   - 宽容窗外（lease_expires_at ≤ now-15s）→ 401 LEASE_EXPIRED；
//   - 重新校验授权：版本提升或 entitlement 失效 → 403 ENTITLEMENT_REVOKED 并落 revoked_at；
//   - 并发续期由条件 UPDATE（旧到期值比对）保证单赢，输家 409 LEASE_CONFLICT。
func (h *InternalHandler) RenewGrant(c *gin.Context) {
	ctx := c.Request.Context()
	grantID := c.Param("id")
	grant, err := h.storage.GetRuntimeGrant(ctx, grantID)
	if err != nil {
		h.deny(c, http.StatusNotFound, errCodeGrantNotFound, "grant not found", h.serviceSubject(c))
		return
	}

	now := h.clock.Now()
	if grant.RevokedAt != nil {
		h.denyRenew(c, ctx, grant, http.StatusForbidden, errCodeEntitlementRevoked, "grant already revoked")
		return
	}
	if !grant.LeaseExpiresAt.After(now.Add(-renewGraceWindow)) {
		h.denyRenew(c, ctx, grant, http.StatusUnauthorized, errCodeLeaseExpired, "lease expired beyond renewal grace window")
		return
	}

	// 重新校验 subject 授权未撤销：版本未提升 + entitlement 仍 active。
	version, err := h.versions.CurrentFresh(ctx, grant.Subject)
	if err != nil {
		h.deny(c, http.StatusInternalServerError, errCodeInternal, "read authz version failed", grant.Subject)
		return
	}
	if version != grant.AuthzVersion {
		h.revokeGrantQuietly(ctx, grant, now)
		h.denyRenew(c, ctx, grant, http.StatusForbidden, errCodeEntitlementRevoked, "subject authorization version bumped since grant issued")
		return
	}
	if _, err := h.checker.RequireEntitlement(ctx, grant.Subject, grant.TenantID, ""); err != nil {
		h.revokeGrantQuietly(ctx, grant, now)
		h.denyRenew(c, ctx, grant, http.StatusForbidden, errCodeEntitlementRevoked, "subject entitlement revoked or identity disabled")
		return
	}

	// 原子续期：旧到期值参与 WHERE，并发双续期只有一个能赢。
	if h.renewGate != nil {
		h.renewGate()
	}
	newExpiry := now.Add(taskLeaseTTL)
	rows, err := h.storage.RenewRuntimeGrantLease(ctx, grantID, grant.LeaseExpiresAt, newExpiry, now.Add(-renewGraceWindow))
	if err != nil {
		h.deny(c, http.StatusInternalServerError, errCodeInternal, "renew grant lease failed", grant.Subject)
		return
	}
	if rows == 0 {
		h.denyRenewLostRace(c, ctx, grantID, grant)
		return
	}

	grant.LeaseExpiresAt = newExpiry
	leaseToken, err := h.signLeaseToken(grant, now)
	if err != nil {
		h.deny(c, http.StatusInternalServerError, errCodeInternal, "sign lease token failed", grant.Subject)
		return
	}

	h.audit(ctx, h.serviceSubject(c), "integration.internal.grant.renewed", "POST /integration/v1/internal/grants/:id/renew", map[string]interface{}{
		"result":           "allowed",
		"grant_id":         grantID,
		"subject":          grant.Subject,
		"authz_version":    version,
		"lease_expires_at": newExpiry.Format(time.RFC3339),
	}, c)

	c.JSON(http.StatusOK, gin.H{
		"grant_id":         grantID,
		"lease_token":      leaseToken,
		"lease_expires_at": newExpiry,
	})
}

// denyRenew 续期拒绝（含审计）。
func (h *InternalHandler) denyRenew(c *gin.Context, ctx context.Context, grant *model.RuntimeGrant, status int, code, message string) {
	h.audit(ctx, h.serviceSubject(c), "integration.internal.grant.renew.denied", "POST /integration/v1/internal/grants/:id/renew", map[string]interface{}{
		"result":   "denied",
		"grant_id": grant.GrantID,
		"subject":  grant.Subject,
		"error":    code,
	}, c)
	h.deny(c, status, code, message, grant.Subject)
}

// denyRenewLostRace 条件 UPDATE 未命中：区分已撤销 / 窗口外 / 并发冲突。
func (h *InternalHandler) denyRenewLostRace(c *gin.Context, ctx context.Context, grantID string, grant *model.RuntimeGrant) {
	latest, err := h.storage.GetRuntimeGrant(ctx, grantID)
	if err != nil {
		h.deny(c, http.StatusConflict, errCodeLeaseConflict, "concurrent renewal conflict", grant.Subject)
		return
	}
	now := h.clock.Now()
	switch {
	case latest.RevokedAt != nil:
		h.denyRenew(c, ctx, latest, http.StatusForbidden, errCodeEntitlementRevoked, "grant already revoked")
	case !latest.LeaseExpiresAt.After(now.Add(-renewGraceWindow)):
		h.denyRenew(c, ctx, latest, http.StatusUnauthorized, errCodeLeaseExpired, "lease expired beyond renewal grace window")
	default:
		h.denyRenew(c, ctx, latest, http.StatusConflict, errCodeLeaseConflict, "concurrent renewal lost")
	}
}

// revokeGrantQuietly 续期校验失败时把租约落 revoked_at（best-effort）。
func (h *InternalHandler) revokeGrantQuietly(ctx context.Context, grant *model.RuntimeGrant, now time.Time) {
	if _, err := h.storage.RevokeRuntimeGrant(ctx, grant.GrantID, now); err != nil {
		log.Printf("[Internal] revoke grant on renew denial failed (grant=%s): %v", grant.GrantID, err)
	}
}

// ---------- GET /integration/v1/internal/authz & POST 批量 ----------

type authzBatchRequest struct {
	Subjects []string `json:"subjects"`
	TenantID string   `json:"tenant_id"`
}

// AuthzView 单 subject 授权视图：{<subject>: {authz_version, revoked, entitlements}}。
// subject 停用（identity_links.status != active）→ revoked=true 且 entitlements 为空。
func (h *InternalHandler) AuthzView(c *gin.Context) {
	ctx := c.Request.Context()
	subject := c.Query("subject")
	if subject == "" {
		h.deny(c, http.StatusBadRequest, errCodeInvalidRequest, "subject query parameter is required", h.serviceSubject(c))
		return
	}
	tenant := c.Query("tenant")

	view, err := h.buildAuthzView(ctx, subject, tenant)
	if err != nil {
		h.deny(c, http.StatusInternalServerError, errCodeInternal, "build authz view failed", subject)
		return
	}

	h.audit(ctx, h.serviceSubject(c), "integration.internal.authz.viewed", "GET /integration/v1/internal/authz", map[string]interface{}{
		"result":  "allowed",
		"subject": subject,
		"tenant":  tenant,
		"revoked": view.Revoked,
	}, c)

	c.JSON(http.StatusOK, gin.H{subject: view})
}

// AuthzBatch 批量授权视图：body {subjects[], tenant?}，逐 subject 返回同单查结构。
func (h *InternalHandler) AuthzBatch(c *gin.Context) {
	ctx := c.Request.Context()
	var req authzBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.deny(c, http.StatusBadRequest, errCodeInvalidRequest, "invalid JSON body: "+err.Error(), h.serviceSubject(c))
		return
	}
	if len(req.Subjects) == 0 {
		h.deny(c, http.StatusBadRequest, errCodeInvalidRequest, "subjects is required", h.serviceSubject(c))
		return
	}

	out := make(gin.H, len(req.Subjects))
	for _, subject := range req.Subjects {
		view, err := h.buildAuthzView(ctx, subject, req.TenantID)
		if err != nil {
			h.deny(c, http.StatusInternalServerError, errCodeInternal, "build authz view failed", subject)
			return
		}
		out[subject] = view
	}

	h.audit(ctx, h.serviceSubject(c), "integration.internal.authz.viewed", "POST /integration/v1/internal/authz", map[string]interface{}{
		"result":   "allowed",
		"subjects": len(req.Subjects),
	}, c)

	c.JSON(http.StatusOK, out)
}

// authzView 单 subject 授权视图（响应值对象）。
type authzView struct {
	AuthzVersion int64    `json:"authz_version"`
	Revoked      bool     `json:"revoked"`
	Entitlements []string `json:"entitlements"`
}

func (h *InternalHandler) buildAuthzView(ctx context.Context, subject, tenantID string) (*authzView, error) {
	links, err := h.storage.ListIdentityLinksByImmutableSubject(ctx, subject)
	if err != nil {
		return nil, err
	}
	active := false
	for _, l := range links {
		if l.Status == "active" {
			active = true
			break
		}
	}
	version, err := h.versions.CurrentFresh(ctx, subject)
	if err != nil {
		return nil, err
	}

	view := &authzView{
		AuthzVersion: version,
		Revoked:      !active,
	}
	if !active {
		view.Entitlements = []string{}
		return view, nil
	}

	ents, err := h.storage.ListActiveManagedEntitlementsBySubject(ctx, subject)
	if err != nil {
		return nil, err
	}
	capSet := make(map[string]struct{})
	for _, e := range ents {
		if tenantID != "" && e.TenantID != tenantID {
			continue
		}
		if e.Capability != "" {
			capSet[e.Capability] = struct{}{}
		}
	}
	caps := make([]string, 0, len(capSet))
	for cap := range capSet {
		caps = append(caps, cap)
	}
	sort.Strings(caps)
	view.Entitlements = caps
	return view, nil
}

// ---------- POST /integration/v1/internal/revoke ----------

type revokeRequest struct {
	Subject        string `json:"subject"`
	TenantID       string `json:"tenant_id"`
	IdentityLinkID string `json:"identity_link_id"`
	Reason         string `json:"reason"`
}

type revokeAffected struct {
	knowledgeUserID string
	tenantID        string
	version         int64
}

// Revoke 撤权（BND-110~112）：selector 三选一（subject / tenant_id / identity_link_id）。
// 顺序：一个事务内先持久化（identity 置 disabled 或 entitlements 落 revoked_at +
// 受影响 subject 的 sessions 族撤销（revoke_reason=revoke）+ authz_epochs 版本 +1），
// 提交后返回 {applied_at}；
// 失效通知与上游 token 删除在事务提交后异步执行，失败只记审计，不影响已生效的拒绝。
// 重复调用幂等：无可撤销内容时不 bump 版本，照常返回 applied_at。
func (h *InternalHandler) Revoke(c *gin.Context) {
	ctx := c.Request.Context()
	var req revokeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.deny(c, http.StatusBadRequest, errCodeInvalidRequest, "invalid JSON body: "+err.Error(), h.serviceSubject(c))
		return
	}
	selectors := 0
	for _, v := range []string{req.Subject, req.TenantID, req.IdentityLinkID} {
		if v != "" {
			selectors++
		}
	}
	if selectors != 1 {
		h.deny(c, http.StatusBadRequest, errCodeInvalidRequest, "exactly one of subject, tenant_id, identity_link_id is required", h.serviceSubject(c))
		return
	}
	reason := req.Reason
	if reason == "" {
		reason = "manual"
	}

	now := h.clock.Now()
	affected := make(map[string]*revokeAffected)
	err := h.storage.StateDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		switch {
		case req.IdentityLinkID != "":
			var link model.IdentityLink
			err := tx.Where("id = ?", req.IdentityLinkID).First(&link).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil // 幂等：未知关联视为无内容可撤
			}
			if err != nil {
				return err
			}
			if link.Status == "active" {
				rows, err := storage.DisableIdentityLinkByIDTx(tx, link.ID, now)
				if err != nil {
					return err
				}
				if rows > 0 {
					affected[link.ImmutableSubject] = &revokeAffected{knowledgeUserID: link.KnowledgeUserID}
				}
			}
			// 同事务撤会话族（BND-111 覆盖清单：platform 会话族）
			if _, err := storage.RevokeSessionsByIdentityLinkIDsTx(tx, []string{link.ID}, "revoke", now); err != nil {
				return err
			}
		case req.Subject != "":
			var links []model.IdentityLink
			if err := tx.Where("immutable_subject = ?", req.Subject).Order("created_at ASC").Find(&links).Error; err != nil {
				return err
			}
			linkRows, err := storage.DisableIdentityLinksBySubjectTx(tx, req.Subject, now)
			if err != nil {
				return err
			}
			entRows, err := storage.RevokeManagedEntitlementsBySubjectTx(tx, req.Subject, now)
			if err != nil {
				return err
			}
			linkIDs := make([]string, 0, len(links))
			for _, l := range links {
				linkIDs = append(linkIDs, l.ID)
			}
			if _, err := storage.RevokeSessionsByIdentityLinkIDsTx(tx, linkIDs, "revoke", now); err != nil {
				return err
			}
			if linkRows+entRows > 0 {
				affected[req.Subject] = &revokeAffected{knowledgeUserID: knowledgeUserIDFromLinks(links)}
			}
		default: // tenant_id
			var subjects []string
			if err := tx.Model(&model.ManagedEntitlement{}).
				Where("tenant_id = ? AND revoked_at IS NULL", req.TenantID).
				Distinct().
				Pluck("subject", &subjects).Error; err != nil {
				return err
			}
			entRows, err := storage.RevokeManagedEntitlementsByTenantTx(tx, req.TenantID, now)
			if err != nil {
				return err
			}
			if entRows > 0 {
				for _, subj := range subjects {
					var links []model.IdentityLink
					if err := tx.Where("immutable_subject = ?", subj).Order("created_at ASC").Find(&links).Error; err != nil {
						return err
					}
					linkIDs := make([]string, 0, len(links))
					for _, l := range links {
						linkIDs = append(linkIDs, l.ID)
					}
					if _, err := storage.RevokeSessionsByIdentityLinkIDsTx(tx, linkIDs, "revoke", now); err != nil {
						return err
					}
					affected[subj] = &revokeAffected{
						knowledgeUserID: knowledgeUserIDFromLinks(links),
						tenantID:        req.TenantID,
					}
				}
			}
		}

		// 版本提升与持久化同事务提交：revoke 提交后立即拒绝新 grant 与续期。
		for subj, af := range affected {
			version, err := storage.BumpAuthzVersionTx(tx, subj, now)
			if err != nil {
				return err
			}
			af.version = version
		}
		return nil
	})
	if err != nil {
		log.Printf("[Internal] revoke transaction failed: %v", err)
		h.deny(c, http.StatusInternalServerError, errCodeInternal, "revoke transaction failed", h.serviceSubject(c))
		return
	}

	appliedAt := h.clock.Now()

	// 事务提交后：失效通知（失败进 30s TTL 重试队列，不影响已生效的拒绝）。
	subjects := make([]string, 0, len(affected))
	for subj, af := range affected {
		subjects = append(subjects, subj)
		h.notifier.Invalidate(ctx, authz.InvalidateEvent{
			Subject:  subj,
			TenantID: af.tenantID,
			Reason:   reason,
			Version:  af.version,
			At:       appliedAt,
		})
	}

	// 上游 token 撤销钩子（REQ-003）：删除 WeKnora 主库 auth_tokens 行；失败只记审计。
	// tenant 级撤权只删该 (user, tenant) 的行（L7：撤销器支持 TenantTokenRevoker 时）；
	// subject / identity_link 级删除该用户全部行。
	for subj, af := range affected {
		if af.knowledgeUserID == "" {
			continue
		}
		deleted, err := h.revokeUpstreamTokens(ctx, af)
		if err != nil {
			log.Printf("[Internal] upstream token revoke failed (subject=%s, knowledge_user=%s): %v", subj, af.knowledgeUserID, err)
			h.audit(ctx, h.serviceSubject(c), "integration.internal.revoke.token_revoke.failed", "POST /integration/v1/internal/revoke", map[string]interface{}{
				"result":            "denied",
				"subject":           subj,
				"knowledge_user_id": af.knowledgeUserID,
				"error":             err.Error(),
			}, c)
			continue
		}
		h.audit(ctx, h.serviceSubject(c), "integration.internal.revoke.token_revoked", "POST /integration/v1/internal/revoke", map[string]interface{}{
			"result":            "allowed",
			"subject":           subj,
			"knowledge_user_id": af.knowledgeUserID,
			"deleted":           deleted,
		}, c)
	}

	h.audit(ctx, h.serviceSubject(c), "integration.internal.revoke.applied", "POST /integration/v1/internal/revoke", map[string]interface{}{
		"result":     "allowed",
		"selector":   revokeSelectorLabel(req),
		"reason":     reason,
		"subjects":   subjects,
		"version":    affectedVersions(affected),
		"applied_at": appliedAt.Format(time.RFC3339),
	}, c)

	c.JSON(http.StatusOK, gin.H{"applied_at": appliedAt})
}

// knowledgeUserIDFromLinks 从身份关联里取 knowledge_user_id（优先 active 行）。
func knowledgeUserIDFromLinks(links []model.IdentityLink) string {
	for _, l := range links {
		if l.Status == "active" && l.KnowledgeUserID != "" {
			return l.KnowledgeUserID
		}
	}
	for _, l := range links {
		if l.KnowledgeUserID != "" {
			return l.KnowledgeUserID
		}
	}
	return ""
}

// revokeUpstreamTokens 按撤权粒度选择上游 token 撤销范围：
// tenant 级只删该 (knowledgeUser, tenant) 的 auth_tokens 行（L7，撤销器支持
// authz.TenantTokenRevoker 时；不支持则记日志并降级为该用户全量删除）；
// 其余粒度（subject / identity_link）删除该用户全部行。
func (h *InternalHandler) revokeUpstreamTokens(ctx context.Context, af *revokeAffected) (int64, error) {
	if af.tenantID != "" {
		if tr, ok := h.revoker.(authz.TenantTokenRevoker); ok {
			return tr.RevokeTokensForTenant(ctx, af.knowledgeUserID, af.tenantID)
		}
		log.Printf("[Internal] revoker lacks tenant-granular support, falling back to full user token revoke (tenant=%s)", af.tenantID)
	}
	return h.revoker.RevokeTokens(ctx, af.knowledgeUserID)
}

func revokeSelectorLabel(req revokeRequest) string {
	switch {
	case req.IdentityLinkID != "":
		return "identity_link_id"
	case req.Subject != "":
		return "subject"
	default:
		return "tenant_id"
	}
}

func affectedVersions(affected map[string]*revokeAffected) map[string]int64 {
	out := make(map[string]int64, len(affected))
	for subj, af := range affected {
		out[subj] = af.version
	}
	return out
}

// ---------- 助手 ----------

// signLeaseToken 签发租约 JWT（HS256 / auth.key；claims grant_id/sub/tenant_id/task_id/runtime_id,
// type=task_lease）。claim 键名与 code 侧 platform.ParseLease 的解析契约（INT-102）对齐。
func (h *InternalHandler) signLeaseToken(grant *model.RuntimeGrant, now time.Time) (string, error) {
	if h.cfg.Auth.Key == "" {
		return "", errors.New("auth key is not configured")
	}
	claims := jwt.MapClaims{
		"grant_id":  grant.GrantID,
		"sub":       grant.Subject,
		"tenant_id": grant.TenantID,
		"type":      "task_lease",
		"exp":       grant.LeaseExpiresAt.Unix(),
		"iat":       now.Unix(),
		"jti":       uuid.NewString(),
	}
	if grant.TaskID != nil {
		claims["task_id"] = *grant.TaskID
	}
	if grant.RuntimeID != nil {
		claims["runtime_id"] = *grant.RuntimeID
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(h.cfg.Auth.Key))
}

// deny 统一错误信封并写拒绝审计（与 /integration/v1 同构）。
func (h *InternalHandler) deny(c *gin.Context, status int, code, message, actor string) {
	ctx := c.Request.Context()
	h.audit(ctx, actor, "integration.internal.denied", c.Request.Method+" "+c.FullPath(), map[string]interface{}{
		"result": "denied",
		"error":  code,
	}, c)
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{
		"code":       code,
		"message":    message,
		"request_id": integrationRequestID(c),
	}})
}

// audit 写审计事件（best-effort，失败只记日志）。
func (h *InternalHandler) audit(ctx context.Context, actor, action, target string, detail map[string]interface{}, c *gin.Context) {
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
		log.Printf("[Internal] failed to write audit %s: %v", action, err)
	}
}

// normalizeCapabilities 去重去空串；无有效项时返回 nil（存 NULL）。
func normalizeCapabilities(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, cap := range in {
		cap = strings.TrimSpace(cap)
		if cap == "" {
			continue
		}
		if _, ok := seen[cap]; ok {
			continue
		}
		seen[cap] = struct{}{}
		out = append(out, cap)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func stringPtrOrNil(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
