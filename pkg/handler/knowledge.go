// TASK-P07（INT-120，REQ-004，ADR-006）：platform 知识适配接口组 /integration/v1/knowledge/*。
//
// 认证：Bearer 平台 access token（INT-101 会话体系），经 resolveAccessSession
// 校验会话未撤销/未过期且身份关联存在。服务端重授权：tenant 必须在该用户
// active entitlements 内（authz.RequireEntitlement，桥内强制执行）；KB/文档
// ID 仅作请求参数、不作授权依据。桥不可用/上游超时（15s）→ 502
// UPSTREAM_UNAVAILABLE，绝不降级共享 endpoint token。
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/kbridge"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// 本接口组错误码（BND-180 枚举扩展）。
const (
	errCodeSpaceForbidden      = "SPACE_FORBIDDEN"
	errCodeSubjectNotProv      = "SUBJECT_NOT_PROVISIONED"
	errCodeUpstreamUnavailable = "UPSTREAM_UNAVAILABLE"
	errCodeSkillHashMismatch   = "SKILL_HASH_MISMATCH"
	errCodeSkillDelisted       = "SKILL_DELISTED"
)

// bridgeTokenTTL 单次知识操作的上游 token 寿命（P09：≤120s）。
const bridgeTokenTTL = 60 * time.Second

// KnowledgeHandler /integration/v1/knowledge 处理器。
type KnowledgeHandler struct {
	cfg      *config.Config
	storage  *storage.Storage
	bridge   *kbridge.Bridge
	upstream *knowledgeUpstream
}

// NewKnowledgeHandler 组装知识适配处理器；bridge 为 P09 兼容桥。
func NewKnowledgeHandler(cfg *config.Config, s *storage.Storage, bridge *kbridge.Bridge) *KnowledgeHandler {
	return &KnowledgeHandler{
		cfg:      cfg,
		storage:  s,
		bridge:   bridge,
		upstream: newKnowledgeUpstream(cfg.WeKnora.BaseURL),
	}
}

// ---------- 会话与桥助手（SkillsHandler 共用） ----------

// accessSession 已校验的平台会话身份。
type accessSession struct {
	claims  jwt.MapClaims
	session *model.Session
	link    *model.IdentityLink
}

// resolveAccessSession 解析并校验 Bearer 平台 access token：
// JWT 验签（type=access）→ 会话行（未撤销、未过期、主体绑定一致）→ 身份关联。
func resolveAccessSession(cfg *config.Config, s *storage.Storage, c *gin.Context) (*accessSession, bool) {
	ctx := c.Request.Context()
	deny := func(status int, code, message string) {
		writeErrorEnvelope(c, status, code, message)
	}
	raw := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	if raw == "" {
		deny(http.StatusUnauthorized, errCodeSessionRevoked, "missing bearer token")
		return nil, false
	}
	claims := jwt.MapClaims{}
	token, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(cfg.Auth.Key), nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil || !token.Valid || claims["type"] != "access" {
		deny(http.StatusUnauthorized, errCodeSessionRevoked, "invalid bearer token")
		return nil, false
	}
	sid, _ := claims["sid"].(string)
	sub, _ := claims["sub"].(string)
	if sid == "" || sub == "" {
		deny(http.StatusUnauthorized, errCodeSessionRevoked, "access token missing sid/sub")
		return nil, false
	}
	session, err := s.GetSession(ctx, sid)
	if err != nil || session.IdentityLinkID != sub {
		deny(http.StatusUnauthorized, errCodeSessionRevoked, "session not found")
		return nil, false
	}
	if session.RevokedAt != nil || !time.Now().Before(session.ExpiresAt) {
		deny(http.StatusUnauthorized, errCodeSessionRevoked, "session revoked or expired")
		return nil, false
	}
	link, err := s.GetIdentityLink(ctx, sub)
	if err != nil {
		deny(http.StatusUnauthorized, errCodeSessionRevoked, "identity link missing")
		return nil, false
	}
	return &accessSession{claims: claims, session: session, link: link}, true
}

// entitledTenants 返回主体 active entitlements 的去重租户集（升序，确定性输出）。
func entitledTenants(ctx context.Context, s *storage.Storage, subject string) ([]string, error) {
	ents, err := s.ListActiveManagedEntitlementsBySubject(ctx, subject)
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{}, len(ents))
	for _, e := range ents {
		if e.TenantID != "" {
			set[e.TenantID] = struct{}{}
		}
	}
	tenants := make([]string, 0, len(set))
	for t := range set {
		tenants = append(tenants, t)
	}
	sort.Strings(tenants)
	return tenants, nil
}

// issueBridgeToken 经桥签发指定租户的上游 token（ttl ∈ (0,120s]）；拒绝时已写错误信封，
// ok=false。桥内强制执行 identity active 与 authz.RequireEntitlement（tenant 必须
// 在 active entitlement 允许集内），停用 → SUBJECT_NOT_PROVISIONED。
func issueBridgeToken(c *gin.Context, bridge *kbridge.Bridge, link *model.IdentityLink, tenantID string, ttl time.Duration) (string, bool) {
	token, err := bridge.IssueUpstreamToken(c.Request.Context(), link.ID, tenantID, ttl)
	if err == nil {
		return token, true
	}
	switch {
	case errors.Is(err, kbridge.ErrIdentityDisabled):
		writeErrorEnvelope(c, http.StatusForbidden, errCodeSubjectNotProv, "subject identity is disabled")
	case errors.Is(err, kbridge.ErrEntitlementMissing):
		writeErrorEnvelope(c, http.StatusForbidden, errCodeSpaceForbidden, "tenant not in active entitlement set")
	case errors.Is(err, kbridge.ErrAdminTokenRefused):
		writeErrorEnvelope(c, http.StatusForbidden, errCodeSpaceForbidden, "upstream admin token is not allowed")
	default:
		log.Printf("[Knowledge] bridge token issue failed (link=%s, tenant=%s): %v", link.ID, tenantID, err)
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "bridge token issue failed")
	}
	return "", false
}

// denyUpstream 把上游错误映射为平台错误信封：
// 传输层 → 502 UPSTREAM_UNAVAILABLE；已知业务 code 透传；403 → SPACE_FORBIDDEN。
func denyUpstream(c *gin.Context, err error) {
	var uerr *upstreamError
	switch {
	case errors.Is(err, errUpstreamUnavailable):
		writeErrorEnvelope(c, http.StatusBadGateway, errCodeUpstreamUnavailable, "knowledge upstream unavailable")
	case errors.As(err, &uerr):
		switch uerr.Code {
		case errCodeSpaceForbidden, errCodeSkillDelisted, errCodeSkillHashMismatch:
			writeErrorEnvelope(c, uerr.Status, uerr.Code, uerr.Message)
		default:
			if uerr.Status == http.StatusForbidden {
				writeErrorEnvelope(c, http.StatusForbidden, errCodeSpaceForbidden, uerr.Message)
			} else if uerr.Status == http.StatusNotFound {
				writeErrorEnvelope(c, http.StatusNotFound, errCodeInvalidRequest, "upstream resource not found")
			} else {
				writeErrorEnvelope(c, http.StatusBadGateway, errCodeUpstreamUnavailable, "knowledge upstream error")
			}
		}
	default:
		log.Printf("[Knowledge] unexpected upstream error: %v", err)
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "unexpected upstream error")
	}
}

// writeErrorEnvelope 统一错误信封 {error:{code,message,request_id}}。
func writeErrorEnvelope(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{
		"code":       code,
		"message":    message,
		"request_id": integrationRequestID(c),
	}})
}

// auditEvent 写审计（best-effort，失败只记日志）。
func auditEvent(ctx context.Context, s *storage.Storage, actor, action, target, requestID string, detail map[string]interface{}) {
	if s == nil {
		return
	}
	after, _ := json.Marshal(detail)
	event := &model.AuditEvent{
		Actor:     actor,
		Action:    action,
		Target:    target,
		After:     string(after),
		RequestID: requestID,
	}
	if err := s.InsertAuditEvent(ctx, event); err != nil {
		log.Printf("[Knowledge] failed to write audit %s: %v", action, err)
	}
}

// ---------- INT-120a POST /integration/v1/knowledge/search ----------

type knowledgeSearchRequest struct {
	TenantID        string   `json:"tenant_id"`
	Space           string   `json:"space"` // tenant_id 别名
	KnowledgeBaseID string   `json:"knowledge_base_id"`
	Query           string   `json:"query"`
	MatchCount      int      `json:"match_count,omitempty"`
	KnowledgeIDs    []string `json:"knowledge_ids,omitempty"`
}

// searchHit 规范化检索命中（BND-180/INT-120：knowledge_id/chunk_id/title/locator/source_tenant）。
type searchHit struct {
	KnowledgeID  string      `json:"knowledge_id"`
	ChunkID      string      `json:"chunk_id"`
	Title        string      `json:"title"`
	Locator      *hitLocator `json:"locator,omitempty"`
	SourceTenant string      `json:"source_tenant"`
	Content      string      `json:"content,omitempty"`
	Score        float64     `json:"score,omitempty"`
}

type hitLocator struct {
	StartAt    int `json:"start_at"`
	EndAt      int `json:"end_at"`
	ChunkIndex int `json:"chunk_index"`
	Seq        int `json:"seq"`
}

// Search 指定空间内混合检索：会话主体 → 桥取上游 token（内含 entitlement 校验）
// → knowledge hybrid-search → 规范化响应。
func (h *KnowledgeHandler) Search(c *gin.Context) {
	ctx := c.Request.Context()
	sess, ok := resolveAccessSession(h.cfg, h.storage, c)
	if !ok {
		return
	}
	var req knowledgeSearchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "invalid JSON body: "+err.Error())
		return
	}
	tenant := req.TenantID
	if tenant == "" {
		tenant = req.Space
	}
	if tenant == "" || req.KnowledgeBaseID == "" || strings.TrimSpace(req.Query) == "" {
		writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "tenant_id, knowledge_base_id and query are required")
		return
	}

	token, ok := issueBridgeToken(c, h.bridge, sess.link, tenant, bridgeTokenTTL)
	if !ok {
		return
	}
	upstreamBody := map[string]interface{}{"query_text": req.Query}
	if req.MatchCount > 0 {
		upstreamBody["match_count"] = req.MatchCount
	}
	if len(req.KnowledgeIDs) > 0 {
		upstreamBody["knowledge_ids"] = req.KnowledgeIDs
	}
	hits, err := h.upstream.hybridSearch(ctx, token, req.KnowledgeBaseID, upstreamBody)
	if err != nil {
		denyUpstream(c, err)
		return
	}

	results := make([]searchHit, 0, len(hits))
	for _, hit := range hits {
		results = append(results, searchHit{
			KnowledgeID:  hit.KnowledgeID,
			ChunkID:      hit.ID,
			Title:        hit.KnowledgeTitle,
			Locator:      &hitLocator{StartAt: hit.StartAt, EndAt: hit.EndAt, ChunkIndex: hit.ChunkIndex, Seq: hit.Seq},
			SourceTenant: tenant,
			Content:      hit.Content,
			Score:        hit.Score,
		})
	}

	auditEvent(ctx, h.storage, sess.link.ImmutableSubject, "integration.knowledge.search", "POST "+c.FullPath(), integrationRequestID(c), map[string]interface{}{
		"result":            "allowed",
		"tenant_id":         tenant,
		"knowledge_base_id": req.KnowledgeBaseID,
		"hits":              len(results),
	})

	c.JSON(http.StatusOK, gin.H{
		"results":           results,
		"source_tenant":     tenant,
		"knowledge_base_id": req.KnowledgeBaseID,
	})
}

// ---------- INT-120b GET /integration/v1/knowledge/:id ----------

// GetKnowledge 片段/内容读取（Viewer 预览级）。?tenant= 显式指定空间时必须
// 已授权；缺省按 active entitlement 租户升序逐个尝试，首个可见租户命中返回。
func (h *KnowledgeHandler) GetKnowledge(c *gin.Context) {
	ctx := c.Request.Context()
	sess, ok := resolveAccessSession(h.cfg, h.storage, c)
	if !ok {
		return
	}
	knowledgeID := c.Param("id")

	tenants, err := entitledTenants(ctx, h.storage, sess.link.ImmutableSubject)
	if err != nil {
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "list entitlements failed")
		return
	}
	if explicit := c.Query("tenant"); explicit != "" {
		allowed := false
		for _, t := range tenants {
			if t == explicit {
				allowed = true
				break
			}
		}
		if !allowed {
			writeErrorEnvelope(c, http.StatusForbidden, errCodeSpaceForbidden, "tenant not in active entitlement set")
			return
		}
		tenants = []string{explicit}
	}

	var data json.RawMessage
	hitTenant := ""
	for _, tenant := range tenants {
		token, ok := issueBridgeToken(c, h.bridge, sess.link, tenant, bridgeTokenTTL)
		if !ok {
			return
		}
		data, err = h.upstream.getKnowledge(ctx, token, knowledgeID)
		if err == nil {
			hitTenant = tenant
			break
		}
		var uerr *upstreamError
		if errors.As(err, &uerr) && (uerr.Status == http.StatusForbidden || uerr.Status == http.StatusNotFound) {
			continue // 该空间不可见，尝试下一个授权空间
		}
		denyUpstream(c, err)
		return
	}
	if hitTenant == "" {
		writeErrorEnvelope(c, http.StatusForbidden, errCodeSpaceForbidden, "knowledge not visible in any entitled tenant")
		return
	}

	auditEvent(ctx, h.storage, sess.link.ImmutableSubject, "integration.knowledge.viewed", "GET "+c.FullPath(), integrationRequestID(c), map[string]interface{}{
		"result":        "allowed",
		"knowledge_id":  knowledgeID,
		"source_tenant": hitTenant,
	})

	c.JSON(http.StatusOK, gin.H{
		"knowledge_id":  knowledgeID,
		"source_tenant": hitTenant,
		"data":          data,
	})
}

// ---------- INT-120c GET /integration/v1/knowledge/:id/file ----------

// DownloadFile 原文件下载（knowledge Contributor+KBAccessWrite；Viewer 命中
// knowledge 403 → 平台透传 403 SPACE_FORBIDDEN，预览权不升级为下载权，ADR-006）。
// 租户解析策略同 GetKnowledge。
func (h *KnowledgeHandler) DownloadFile(c *gin.Context) {
	ctx := c.Request.Context()
	sess, ok := resolveAccessSession(h.cfg, h.storage, c)
	if !ok {
		return
	}
	knowledgeID := c.Param("id")

	tenants, err := entitledTenants(ctx, h.storage, sess.link.ImmutableSubject)
	if err != nil {
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "list entitlements failed")
		return
	}
	if explicit := c.Query("tenant"); explicit != "" {
		allowed := false
		for _, t := range tenants {
			if t == explicit {
				allowed = true
				break
			}
		}
		if !allowed {
			writeErrorEnvelope(c, http.StatusForbidden, errCodeSpaceForbidden, "tenant not in active entitlement set")
			return
		}
		tenants = []string{explicit}
	}

	for _, tenant := range tenants {
		token, ok := issueBridgeToken(c, h.bridge, sess.link, tenant, bridgeTokenTTL)
		if !ok {
			return
		}
		body, header, err := h.upstream.downloadKnowledgeFile(ctx, token, knowledgeID)
		if err != nil {
			var uerr *upstreamError
			if errors.As(err, &uerr) && (uerr.Status == http.StatusForbidden || uerr.Status == http.StatusNotFound) {
				continue
			}
			denyUpstream(c, err)
			return
		}
		defer func() { _ = body.Close() }()

		auditEvent(ctx, h.storage, sess.link.ImmutableSubject, "integration.knowledge.file_downloaded", "GET "+c.FullPath(), integrationRequestID(c), map[string]interface{}{
			"result":        "allowed",
			"knowledge_id":  knowledgeID,
			"source_tenant": tenant,
		})

		c.Header("Content-Type", header.Get("Content-Type"))
		if disposition := header.Get("Content-Disposition"); disposition != "" {
			c.Header("Content-Disposition", disposition)
		}
		c.Status(http.StatusOK)
		_, _ = io.Copy(c.Writer, body)
		return
	}
	writeErrorEnvelope(c, http.StatusForbidden, errCodeSpaceForbidden, "knowledge file not downloadable in any entitled tenant")
}
