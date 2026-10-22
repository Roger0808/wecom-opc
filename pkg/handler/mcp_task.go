// TASK-P12（INT-150 task-actor 段，REQ-009，ADR-009，EXT-002 收尾）：
// 无用户会话的 task-actor MCP 转发。
//
// 背景：code 后端在无用户会话场景（后台任务、定时任务）需要代任务调用
// knowledge MCP 精确调用面。平台提供三个服务 token（system_admin）入口：
//
//   - POST /integration/v1/mcp/invoke-task
//     创建 task-actor 精确调用：body {subject, tenant_id, service_id,
//     tool_name, arguments, idempotency_key, task_id, device}；校验
//     identity_links active + authz.RequireEntitlement(subject, tenant, 通配)
//     （tenant 必须在该 subject active entitlement 集内），桥签发 120s 上游
//     token（user_id=该 subject 的 knowledge 用户），再签 actor_kind=task
//     调用上下文（task_id/device 与 body 一致绑定、args_digest、jti、
//     exp≤120s），knowledge 按 task-actor 裁定策略，状态码与响应体原样透传；
//
//   - GET  /integration/v1/mcp/invocations/:id?subject=&tenant=&task_id=
//
//   - POST /integration/v1/mcp/invocations/:id/cancel?subject=&tenant=...
//     task 查询/取消转发：knowledge 查询要求 Viewer+ 本人，平台以 subject 的
//     上游 token 调用（knowledge 按 tenant 与 token 校验归属），并重放
//     actor_kind=task 调用上下文（X-Invocation-Context 头）供 knowledge 绑定
//     校验。这两个路由与交互会话入口同路径，按 Authorization 类型分发：
//     可解析为 system_admin 服务 token → task 流程；否则回退会话流程。
//
// 无人值守 task 上下文无审批决定权（BND-151，knowledge 侧 403 裁定），
// 平台不提供 task 版 decision 转发。
package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"weknora-tokendance/pkg/adminauth"
	"weknora-tokendance/pkg/authz"
	"weknora-tokendance/pkg/model"

	"github.com/gin-gonic/gin"
)

// mcpTaskContextHeader task 查询/取消重放上下文的透传头（knowledge 侧消费）。
const mcpTaskContextHeader = "X-Invocation-Context"

// ---------- POST /integration/v1/mcp/invoke-task ----------

type mcpTaskInvokeRequest struct {
	Subject        string          `json:"subject"`
	TenantID       string          `json:"tenant_id"`
	ServiceID      string          `json:"service_id"`
	ToolName       string          `json:"tool_name"`
	Arguments      json.RawMessage `json:"arguments"`
	IdempotencyKey string          `json:"idempotency_key"`
	TaskID         string          `json:"task_id"`
	Device         string          `json:"device"`
}

// InvokeTask 服务 token 代任务创建精确调用（task-actor）。task_id/device 必填：
// 二者与 actor_kind=task 一起写入调用上下文，knowledge 强制绑定校验；
// 状态码与响应体原样透传（202 MCP_APPROVAL_REQUIRED 对 task 上下文不出现——
// 无人值守无审批权，命中即 knowledge 403 透传）。
func (h *MCPHandler) InvokeTask(c *gin.Context) {
	var req mcpTaskInvokeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.Subject == "" || req.TenantID == "" || req.ServiceID == "" || req.ToolName == "" || req.TaskID == "" || req.Device == "" {
		writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "subject, tenant_id, service_id, tool_name, task_id and device are required")
		return
	}
	var args map[string]interface{}
	if len(req.Arguments) > 0 && string(req.Arguments) != "null" {
		if err := json.Unmarshal(req.Arguments, &args); err != nil {
			writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "arguments must be a JSON object")
			return
		}
	}

	if !h.requireTaskEntitlement(c, req.Subject, req.TenantID) {
		return
	}
	link, ok := h.taskIdentityLink(c, req.Subject)
	if !ok {
		return
	}
	token, ok := issueBridgeToken(c, h.bridge, link, req.TenantID, mcpBridgeTokenTTL)
	if !ok {
		return
	}
	ctxJWT, err := h.signInvocationContext(args, req.TenantID, link.KnowledgeUserID, req.Subject, mcpActorKindTask, req.TaskID, req.Device)
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
		"integration.mcp.invoke_task", h.taskServiceActor(c), map[string]interface{}{
			"result":      "forwarded",
			"subject":     req.Subject,
			"tenant_id":   req.TenantID,
			"service_id":  req.ServiceID,
			"tool_name":   req.ToolName,
			"task_id":     req.TaskID,
			"device":      req.Device,
			"args_digest": mcpArgsDigest(args),
		})
}

// ---------- task 查询/取消转发（服务 token 分发） ----------

// forwardTaskInvocation 服务 token 分发：Authorization 可解析为 system_admin
// 服务 token 时按 task-actor 流程转发查询/取消并返回 true；角色不足已写 403
// 同样返回 true；非服务 token（无头/平台会话 token/非法串）返回 false，
// 交由会话流程处理。
func (h *MCPHandler) forwardTaskInvocation(c *gin.Context, method, suffix, auditAction string) bool {
	svcSubject, isService, denied := h.taskServiceSubject(c)
	if !isService {
		return false
	}
	if denied {
		return true
	}

	subject := c.Query("subject")
	tenant := c.Query("tenant")
	if subject == "" || tenant == "" {
		writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "subject and tenant query parameters are required")
		return true
	}
	if !h.requireTaskEntitlement(c, subject, tenant) {
		return true
	}
	link, ok := h.taskIdentityLink(c, subject)
	if !ok {
		return true
	}
	token, ok := issueBridgeToken(c, h.bridge, link, tenant, mcpBridgeTokenTTL)
	if !ok {
		return true
	}

	// 重放 actor_kind=task 调用上下文：task_id/device 来自 query（code 后端
	// 发起任务时持有），供 knowledge 侧查询归属与任务绑定校验。
	taskID := c.Query("task_id")
	device := c.Query("device")
	ctxJWT, err := h.signInvocationContext(nil, tenant, link.KnowledgeUserID, subject, mcpActorKindTask, taskID, device)
	if err != nil {
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "sign invocation context failed")
		return true
	}

	var body io.Reader
	if method == http.MethodPost {
		raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
		if err != nil {
			writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "read body failed")
			return true
		}
		body = bytes.NewReader(raw)
	}
	path := "/api/v1/integration/mcp/invocations/" + c.Param("id") + suffix
	h.proxyWithHeader(c, method, path, token, body,
		map[string]string{mcpTaskContextHeader: ctxJWT},
		auditAction, svcSubject, map[string]interface{}{
			"result":    "forwarded",
			"subject":   subject,
			"tenant_id": tenant,
			"task_id":   taskID,
			"device":    device,
		})
	return true
}

// ---------- task 流程共享助手 ----------

// taskServiceSubject 识别服务 token，三态返回：
//   - (subject, true, false)：system_admin 服务 token，继续 task 流程；
//   - ("", false, false)：非服务 token（无头/平台会话 token/非法串/Admin.Key
//     未配），回退会话流程；
//   - ("", true, true)：服务 token 已识别但角色不足，403 信封已写入。
func (h *MCPHandler) taskServiceSubject(c *gin.Context) (string, bool, bool) {
	raw := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	if raw == "" || h.cfg.Admin.Key == "" {
		return "", false, false
	}
	claims, err := adminauth.ParseToken([]byte(h.cfg.Admin.Key), raw)
	if err != nil {
		return "", false, false
	}
	if claims.Role != adminauth.RoleSystemAdmin {
		writeErrorEnvelope(c, http.StatusForbidden, errCodeSessionRevoked, "service token requires system_admin role")
		return "", true, true
	}
	return claims.Subject, true, false
}

// taskServiceActor 审计 actor：invoke-task 经 RequireServiceToken 中间件时
// 取内部服务主体（internalServiceSubjectKey）；缺省 anonymous。
func (h *MCPHandler) taskServiceActor(c *gin.Context) string {
	if v, ok := c.Get(internalServiceSubjectKey); ok {
		if subject, ok := v.(string); ok && subject != "" {
			return subject
		}
	}
	return "anonymous"
}

// requireTaskEntitlement task 流程授权校验：identity_links active +
// (subject, tenant) active entitlement（通配能力）。identity 停用 → 403
// SUBJECT_NOT_PROVISIONED；tenant 不在 active entitlement 集内 → 403
// SPACE_FORBIDDEN。
func (h *MCPHandler) requireTaskEntitlement(c *gin.Context, subject, tenant string) bool {
	if _, err := h.checker.RequireEntitlement(c.Request.Context(), subject, tenant, ""); err != nil {
		switch {
		case errors.Is(err, authz.ErrIdentityDisabled):
			writeErrorEnvelope(c, http.StatusForbidden, errCodeSubjectNotProv, "subject identity link is not active")
		case errors.Is(err, authz.ErrEntitlementMissing):
			writeErrorEnvelope(c, http.StatusForbidden, errCodeSpaceForbidden, "tenant not in active entitlement set")
		default:
			writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "entitlement check failed")
		}
		return false
	}
	return true
}

// taskIdentityLink 解析 subject 的 active 身份关联（knowledge 用户映射 +
// 桥签发前提）；无 active 关联 → 403 SUBJECT_NOT_PROVISIONED。
func (h *MCPHandler) taskIdentityLink(c *gin.Context, subject string) (*model.IdentityLink, bool) {
	links, err := h.storage.ListIdentityLinksByImmutableSubject(c.Request.Context(), subject)
	if err != nil {
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "list identity links failed")
		return nil, false
	}
	for i := range links {
		if links[i].Status == "active" && links[i].KnowledgeUserID != "" {
			return &links[i], true
		}
	}
	writeErrorEnvelope(c, http.StatusForbidden, errCodeSubjectNotProv, "subject identity link is not active")
	return nil, false
}
