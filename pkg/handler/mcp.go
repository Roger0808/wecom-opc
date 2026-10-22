// TASK-P11（INT-150 platform 段，REQ-009，ADR-009，EXT-002）：MCP 精确调用转发适配。
//
// 客户端不直连 knowledge integration 路由：平台替客户端组装双身份后转发——
// resolveAccessSession 校验平台会话（未撤销/未过期/主体绑定一致），桥签发
// 120s knowledge 个人上游 token（桥内强制 identity active + entitlement），
// 再签 HS256 调用上下文 JWT（BND-150：bridge.key 签名、aud=platform-integration、
// sub=上游用户 id 与上游 token 一致、tenant=目标空间、actor_kind=user、TTL ≤120s、
// jti 每次新生成一次性使用，重试幂等由 knowledge 侧 idempotency_key 兜底）。
// knowledge 按 actor_kind=user 裁定策略；平台原样透传上游状态码与响应体
// （202 MCP_APPROVAL_REQUIRED / 403 MCP_POLICY_DENIED 等 code 透传），
// 传输层失败 → 502 UPSTREAM_UNAVAILABLE，绝不降级共享凭据。
package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/authz"
	"weknora-tokendance/pkg/kbridge"
	"weknora-tokendance/pkg/storage"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	// mcpBridgeTokenTTL 单次精确调用的上游 token 寿命（INT-150：≤120s）。
	mcpBridgeTokenTTL = 120 * time.Second
	// mcpContextAudience 调用上下文 aud，必须与 knowledge 侧
	// WEKNORA_INTEGRATION_CONTEXT_AUDIENCE 配置同一值。
	mcpContextAudience = "platform-integration"
	// mcpContextIssuer 调用上下文 iss 声明（平台签发）。
	mcpContextIssuer = "platform"
	// mcpActorKindUser 交互用户发起（BND-151：无人值守上下文无审批权）。
	mcpActorKindUser = "user"
	// mcpActorKindTask 无用户会话的 task-actor 代任务调用（INT-150 task-actor 段，
	// 仅服务 token 入口可签发，task_id/device 必填绑定）。
	mcpActorKindTask = "task"
)

// MCPHandler /integration/v1/mcp 精确调用转发处理器（INT-150 platform 段）。
type MCPHandler struct {
	cfg      *config.Config
	storage  *storage.Storage
	bridge   *kbridge.Bridge
	upstream *knowledgeUpstream
	checker  *authz.EntitlementChecker
}

// NewMCPHandler 组装 MCP 精确调用转发处理器；bridge 为 P09 兼容桥。
func NewMCPHandler(cfg *config.Config, s *storage.Storage, bridge *kbridge.Bridge) *MCPHandler {
	return &MCPHandler{
		cfg:      cfg,
		storage:  s,
		bridge:   bridge,
		upstream: newKnowledgeUpstream(cfg.WeKnora.BaseURL),
		checker:  authz.NewEntitlementChecker(s, nil),
	}
}

// ---------- 共享助手 ----------

// resolveMCPTenant 解析本次转发的目标租户：显式 tenant（body tenant_id 或
// query tenant）必须在该用户 active entitlement 集内；缺省取升序首个
// （确定性）。无任何 active entitlement → 403 SPACE_FORBIDDEN，ok=false。
func (h *MCPHandler) resolveMCPTenant(c *gin.Context, sess *accessSession, explicit string) (string, bool) {
	tenants, err := entitledTenants(c.Request.Context(), h.storage, sess.link.ImmutableSubject)
	if err != nil {
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "list entitlements failed")
		return "", false
	}
	if len(tenants) == 0 {
		writeErrorEnvelope(c, http.StatusForbidden, errCodeSpaceForbidden, "subject has no active entitlement")
		return "", false
	}
	if explicit == "" {
		return tenants[0], true
	}
	for _, t := range tenants {
		if t == explicit {
			return explicit, true
		}
	}
	writeErrorEnvelope(c, http.StatusForbidden, errCodeSpaceForbidden, "tenant not in active entitlement set")
	return "", false
}

// issueMCPUpstreamToken 经桥签 120s 上游个人 token（桥内强制 identity active +
// entitlement）；停用 → 403 SUBJECT_NOT_PROVISIONED（issueBridgeToken 已写信封）。
func (h *MCPHandler) issueMCPUpstreamToken(c *gin.Context, sess *accessSession, tenant string) (string, bool) {
	return issueBridgeToken(c, h.bridge, sess.link, tenant, mcpBridgeTokenTTL)
}

// signInvocationContext 签 HS256 调用上下文（BND-150）。sub 必须等于桥签发的
// 上游 token user_id（knowledge 双身份一致性校验）；user 保留平台不可变主体
// 供审计追踪。tenant 以数值写入（knowledge 按 uint64 解码，桥签发前已校验
// 数值合法）。args_digest 与 knowledge 侧 MCPInvocationArgsDigest 同算法
// （解码为 map 后 json.Marshal 排序键，SHA-256 hex）。
// actor_kind=task 时附加 task_id/device 绑定声明（knowledge 强制二者非空）。
func (h *MCPHandler) signInvocationContext(args map[string]interface{}, tenant, knowledgeUserID, immutableSubject, actorKind, taskID, device string) (string, error) {
	tenantNumeric, err := strconv.ParseUint(tenant, 10, 64)
	if err != nil {
		return "", err
	}
	now := time.Now()
	claims := jwt.MapClaims{
		"iss":         mcpContextIssuer,
		"sub":         knowledgeUserID,
		"user":        immutableSubject,
		"tenant":      tenantNumeric,
		"aud":         mcpContextAudience,
		"actor_kind":  actorKind,
		"request_id":  uuid.NewString(),
		"args_digest": mcpArgsDigest(args),
		"iat":         now.Unix(),
		"exp":         now.Add(mcpBridgeTokenTTL).Unix(),
		"jti":         uuid.NewString(),
	}
	if actorKind == mcpActorKindTask {
		claims["task_id"] = taskID
		claims["device"] = device
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(h.cfg.Bridge.Key))
}

// mcpArgsDigest 与 knowledge types.MCPInvocationArgsDigest 对齐的规范摘要。
func mcpArgsDigest(args map[string]interface{}) string {
	canonical, err := json.Marshal(args)
	if err != nil {
		canonical = []byte("null")
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// proxy 原样透传上游响应：状态码与响应字节不改写；传输层错误映射 502。
func (h *MCPHandler) proxy(c *gin.Context, method, path, token string, body io.Reader, auditAction, subject string, auditDetail map[string]interface{}) {
	h.proxyWithHeader(c, method, path, token, body, nil, auditAction, subject, auditDetail)
}

// proxyWithHeader 同 proxy，额外附带上游请求头（task 查询重放的
// X-Invocation-Context 等）；extra 键规范化为 CanonicalMIMEHeaderKey 后设置。
func (h *MCPHandler) proxyWithHeader(c *gin.Context, method, path, token string, body io.Reader, extra map[string]string, auditAction, subject string, auditDetail map[string]interface{}) {
	status, raw, err := h.upstream.invokePassthrough(c.Request.Context(), method, path, token, body, extra)
	if err != nil {
		denyUpstream(c, err)
		return
	}
	if auditDetail == nil {
		auditDetail = map[string]interface{}{}
	}
	auditDetail["upstream_status"] = status
	auditEvent(c.Request.Context(), h.storage, subject, auditAction, method+" "+c.FullPath(), integrationRequestID(c), auditDetail)
	c.Data(status, "application/json", raw)
}

// ---------- INT-150a POST /integration/v1/mcp/invoke ----------

type mcpInvokeRequest struct {
	ServiceID      string          `json:"service_id"`
	ToolName       string          `json:"tool_name"`
	Arguments      json.RawMessage `json:"arguments"`
	IdempotencyKey string          `json:"idempotency_key"`
	TenantID       string          `json:"tenant_id"`
}

// Invoke 创建精确调用：会话校验 → entitlement 校验 → 桥取上游 token →
// 签调用上下文 → 转发 knowledge POST /api/v1/integration/mcp/invocations，
// 上游状态/响应原样透传（202 MCP_APPROVAL_REQUIRED / 403 MCP_POLICY_DENIED…）。
func (h *MCPHandler) Invoke(c *gin.Context) {
	sess, ok := resolveAccessSession(h.cfg, h.storage, c)
	if !ok {
		return
	}
	var req mcpInvokeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.ServiceID == "" || req.ToolName == "" {
		writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "service_id and tool_name are required")
		return
	}
	var args map[string]interface{}
	if len(req.Arguments) > 0 && string(req.Arguments) != "null" {
		if err := json.Unmarshal(req.Arguments, &args); err != nil {
			writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "arguments must be a JSON object")
			return
		}
	}

	tenant, ok := h.resolveMCPTenant(c, sess, req.TenantID)
	if !ok {
		return
	}
	token, ok := h.issueMCPUpstreamToken(c, sess, tenant)
	if !ok {
		return
	}
	ctxJWT, err := h.signInvocationContext(args, tenant, sess.link.KnowledgeUserID, sess.link.ImmutableSubject, mcpActorKindUser, "", "")
	if err != nil {
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "sign invocation context failed")
		return
	}

	upBody := map[string]interface{}{
		"service_id":         req.ServiceID,
		"tool_name":          req.ToolName,
		"invocation_context": ctxJWT,
	}
	if len(req.Arguments) > 0 {
		upBody["arguments"] = req.Arguments
	}
	if req.IdempotencyKey != "" {
		upBody["idempotency_key"] = req.IdempotencyKey
	}
	raw, err := json.Marshal(upBody)
	if err != nil {
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "marshal upstream body failed")
		return
	}

	h.proxy(c, http.MethodPost, "/api/v1/integration/mcp/invocations", token, bytes.NewReader(raw),
		"integration.mcp.invoke", sess.link.ImmutableSubject, map[string]interface{}{
			"result":      "forwarded",
			"tenant_id":   tenant,
			"service_id":  req.ServiceID,
			"tool_name":   req.ToolName,
			"args_digest": mcpArgsDigest(args),
		})
}

// ---------- INT-150b~e 查询/取消/决定/目录 ----------

// GetInvocation 状态与结果查询转发：GET knowledge .../invocations/{id}。
// 服务 token（system_admin）命中走 task-actor 查询转发（INT-150 task-actor 段），
// 否则按交互会话转发。
func (h *MCPHandler) GetInvocation(c *gin.Context) {
	if h.forwardTaskInvocation(c, http.MethodGet, "", "integration.mcp.task_query") {
		return
	}
	h.forwardByID(c, http.MethodGet, "", "integration.mcp.get")
}

// CancelInvocation 取消转发：POST knowledge .../invocations/{id}/cancel。
// 服务 token 命中走 task-actor 取消转发。
func (h *MCPHandler) CancelInvocation(c *gin.Context) {
	if h.forwardTaskInvocation(c, http.MethodPost, "/cancel", "integration.mcp.task_cancel") {
		return
	}
	h.forwardByID(c, http.MethodPost, "/cancel", "integration.mcp.cancel")
}

// DecideInvocation 人工审批决定转发（BND-151：有权交互用户经会话提交，
// body {decision,args_digest,policy_version} 原样转发，knowledge 单次有效裁定）。
func (h *MCPHandler) DecideInvocation(c *gin.Context) {
	h.forwardByID(c, http.MethodPost, "/decision", "integration.mcp.decision")
}

// forwardByID 查询/取消/决定的公共路径：会话 + 租户解析 + 桥 token + 原样转发。
// 有 body（decision）时原样透传客户端字节。
func (h *MCPHandler) forwardByID(c *gin.Context, method, suffix, auditAction string) {
	sess, ok := resolveAccessSession(h.cfg, h.storage, c)
	if !ok {
		return
	}
	tenant, ok := h.resolveMCPTenant(c, sess, c.Query("tenant"))
	if !ok {
		return
	}
	token, ok := h.issueMCPUpstreamToken(c, sess, tenant)
	if !ok {
		return
	}
	var body io.Reader
	if method == http.MethodPost {
		raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
		if err != nil {
			writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "read body failed")
			return
		}
		body = bytes.NewReader(raw)
	}
	path := "/api/v1/integration/mcp/invocations/" + c.Param("id") + suffix
	h.proxy(c, method, path, token, body, auditAction, sess.link.ImmutableSubject, map[string]interface{}{
		"result":    "forwarded",
		"tenant_id": tenant,
	})
}

// ListServices 可用服务与工具目录转发（knowledge 侧按 actor_kind=user 过滤，
// 即目录 ∩ 受管策略 ∩ 交互用户能力）。query tenant 显式指定空间时必须已授权。
func (h *MCPHandler) ListServices(c *gin.Context) {
	sess, ok := resolveAccessSession(h.cfg, h.storage, c)
	if !ok {
		return
	}
	tenant, ok := h.resolveMCPTenant(c, sess, c.Query("tenant"))
	if !ok {
		return
	}
	token, ok := h.issueMCPUpstreamToken(c, sess, tenant)
	if !ok {
		return
	}
	h.proxy(c, http.MethodGet, "/api/v1/integration/mcp/services?actor_kind="+mcpActorKindUser, token, nil,
		"integration.mcp.services", sess.link.ImmutableSubject, map[string]interface{}{
			"result":    "forwarded",
			"tenant_id": tenant,
		})
}
