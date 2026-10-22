// TASK-P08（INT-130、BND-130/131、REQ-006、ACC-006）：手动同步服务管理接口组。
//
// 挂载：/integration/v1/admin/*，复用 P01 RequireAdmin 的认证 + CSRF
// （RequireAdminAuth 变体：不做 :id 租户中间检查，授权范围由各端点按 claims 判定；
// 审计行为与 RequireAdmin 一致）。错误信封与 /integration/v1 同构：
// {error:{code,message,request_id}}。
//
// 端点：
//   - POST /sync/runs                      触发一次同步运行（幂等键 + 持久化互斥）；
//   - GET  /sync/runs/{id}                 全量进度/统计/步骤/失败明细；
//   - GET  /sync/runs?status=&page=        分页列表；
//   - POST /sync/runs/{id}/retry           重放 failed/retryable 步骤并重算 run 状态；
//   - GET  /identity-conflicts             identity_links conflicted 清单（含证据）；
//   - POST /identity-conflicts/{id}/resolve {action: bind|quarantine}，写审计。
//
// 授权范围：full 仅 system_admin；scope=department 允许 dept_admin，但其部门映射
// 的 tenant 必须等于 claims.TenantID（wecom_dept_tenant_mappings 校验）。
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/adminauth"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"
	"weknora-tokendance/pkg/syncservice"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// AdminSyncHandler /integration/v1/admin 处理器。
type AdminSyncHandler struct {
	cfg     *config.Config
	storage *storage.Storage
	service *syncservice.Service
}

func NewAdminSyncHandler(cfg *config.Config, s *storage.Storage, svc *syncservice.Service) *AdminSyncHandler {
	return &AdminSyncHandler{cfg: cfg, storage: s, service: svc}
}

// syncAdminKnownStatuses GET /sync/runs 合法状态过滤值。
var syncAdminKnownStatuses = map[string]bool{
	syncservice.StatusQueued:        true,
	syncservice.StatusSnapshotting:  true,
	syncservice.StatusValidating:    true,
	syncservice.StatusDiffing:       true,
	syncservice.StatusApplying:      true,
	syncservice.StatusReconciling:   true,
	syncservice.StatusSucceeded:     true,
	syncservice.StatusPartialFailed: true,
	syncservice.StatusFailed:        true,
}

func (h *AdminSyncHandler) deny(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{
		"code":       code,
		"message":    message,
		"request_id": integrationRequestID(c),
	}})
}

// claims 读取管理身份（RequireAdminAuth 保证存在）。
func (h *AdminSyncHandler) claims(c *gin.Context) *adminauth.Claims {
	claims, _ := adminauth.GetClaims(c)
	return claims
}

// audit 写管理审计（best-effort）。
func (h *AdminSyncHandler) audit(ctx context.Context, actor, action, target string, detail map[string]interface{}, c *gin.Context) {
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
		log.Printf("[AdminSync] failed to write audit %s: %v", action, err)
	}
}

// checkRunScope 校验触发者对该 scope/department 的授权范围（403）。
func (h *AdminSyncHandler) checkRunScope(c *gin.Context, claims *adminauth.Claims, scope, departmentCode string) bool {
	if claims.Role == adminauth.RoleSystemAdmin {
		return true
	}
	// dept_admin：仅允许 scope=department 且部门映射 tenant 属于自己
	if scope != string(syncservice.ScopeDepartment) {
		h.deny(c, http.StatusForbidden, "FORBIDDEN", "full sync requires system_admin")
		return false
	}
	mapping, err := h.storage.FindTenantByDeptCode(c.Request.Context(), departmentCode)
	if err != nil || mapping == nil || mapping.TenantID != claims.TenantID {
		h.deny(c, http.StatusForbidden, "FORBIDDEN", "department not in your tenant scope")
		return false
	}
	return true
}

// requireSystemAdmin GET / retry / conflicts 等管理端点：仅 system_admin。
func (h *AdminSyncHandler) requireSystemAdmin(c *gin.Context, claims *adminauth.Claims) bool {
	if claims.Role != adminauth.RoleSystemAdmin {
		h.deny(c, http.StatusForbidden, "FORBIDDEN", "system_admin role required")
		return false
	}
	return true
}

type createSyncRunRequest struct {
	Scope          string `json:"scope" binding:"required"`
	DepartmentCode string `json:"department_code"`
	IdempotencyKey string `json:"idempotency_key" binding:"required"`
	RuleVersion    string `json:"rule_version" binding:"required"`
}

// CreateSyncRun POST /sync/runs：触发一次同步运行（同步执行至终态后返回 run_id+status）。
func (h *AdminSyncHandler) CreateSyncRun(c *gin.Context) {
	var req createSyncRunRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.deny(c, http.StatusBadRequest, "INVALID_REQUEST", "scope, idempotency_key and rule_version are required")
		return
	}
	claims := h.claims(c)
	if !h.checkRunScope(c, claims, req.Scope, req.DepartmentCode) {
		return
	}

	ctx := context.WithoutCancel(c.Request.Context())
	runID, err := h.service.Run(ctx, req.Scope, req.DepartmentCode, req.IdempotencyKey, claims.Subject, req.RuleVersion)
	if err != nil {
		switch {
		case errors.Is(err, syncservice.ErrConflict):
			h.deny(c, http.StatusConflict, "SYNC_RUN_CONFLICT", "another sync run holds the lock for this scope")
		case errors.Is(err, syncservice.ErrInvalidRequest):
			h.deny(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		default:
			h.deny(c, http.StatusInternalServerError, "INTERNAL", err.Error())
		}
		return
	}

	h.audit(ctx, claims.Subject, "integration.admin.sync_run.created", "POST "+c.FullPath(), map[string]interface{}{
		"run_id": runID, "scope": req.Scope, "department_code": req.DepartmentCode,
	}, c)

	run, err := h.storage.GetSyncRun(ctx, runID)
	if err != nil {
		h.deny(c, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"run_id": run.RunID, "status": run.Status})
}

// GetSyncRun GET /sync/runs/{id}：全量进度/统计/步骤/失败明细。
func (h *AdminSyncHandler) GetSyncRun(c *gin.Context) {
	claims := h.claims(c)
	if !h.requireSystemAdmin(c, claims) {
		return
	}
	run, err := h.storage.GetSyncRun(c.Request.Context(), c.Param("id"))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		h.deny(c, http.StatusNotFound, "SYNC_RUN_NOT_FOUND", "sync run not found")
		return
	}
	if err != nil {
		h.deny(c, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	steps, err := h.storage.ListSyncSteps(c.Request.Context(), run.RunID)
	if err != nil {
		h.deny(c, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"run": run, "steps": steps})
}

// ListSyncRuns GET /sync/runs?status=&page=&page_size=：分页列表。
func (h *AdminSyncHandler) ListSyncRuns(c *gin.Context) {
	claims := h.claims(c)
	if !h.requireSystemAdmin(c, claims) {
		return
	}
	status := strings.TrimSpace(c.Query("status"))
	if status != "" && !syncAdminKnownStatuses[status] {
		h.deny(c, http.StatusBadRequest, "INVALID_REQUEST", "unknown status: "+status)
		return
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 20
	}

	runs, total, err := h.storage.ListSyncRunsPage(c.Request.Context(), status, page, pageSize)
	if err != nil {
		h.deny(c, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"runs": runs, "total": total, "page": page, "page_size": pageSize,
	})
}

// RetrySyncRun POST /sync/runs/{id}/retry：重放 failed/retryable 步骤并重算 run 状态。
func (h *AdminSyncHandler) RetrySyncRun(c *gin.Context) {
	claims := h.claims(c)
	if !h.requireSystemAdmin(c, claims) {
		return
	}
	runID := c.Param("id")
	if _, err := h.storage.GetSyncRun(c.Request.Context(), runID); errors.Is(err, gorm.ErrRecordNotFound) {
		h.deny(c, http.StatusNotFound, "SYNC_RUN_NOT_FOUND", "sync run not found")
		return
	}

	ctx := context.WithoutCancel(c.Request.Context())
	status, err := h.service.Retry(ctx, runID)
	if err != nil {
		switch {
		case errors.Is(err, syncservice.ErrNotFound):
			h.deny(c, http.StatusNotFound, "SYNC_RUN_NOT_FOUND", "sync run not found")
		case errors.Is(err, syncservice.ErrNotRetryable):
			h.deny(c, http.StatusBadRequest, "SYNC_RUN_NOT_RETRYABLE", err.Error())
		case errors.Is(err, syncservice.ErrConflict):
			h.deny(c, http.StatusConflict, "SYNC_RUN_CONFLICT", "another sync run holds the lock for this scope")
		default:
			h.deny(c, http.StatusInternalServerError, "INTERNAL", err.Error())
		}
		return
	}

	h.audit(ctx, claims.Subject, "integration.admin.sync_run.retried", "POST "+c.FullPath(), map[string]interface{}{
		"run_id": runID, "status": status,
	}, c)
	c.JSON(http.StatusOK, gin.H{"run_id": runID, "status": status})
}

// ListIdentityConflicts GET /identity-conflicts：conflicted 清单（含冲突证据字段）。
func (h *AdminSyncHandler) ListIdentityConflicts(c *gin.Context) {
	claims := h.claims(c)
	if !h.requireSystemAdmin(c, claims) {
		return
	}
	links, err := h.storage.ListConflictedIdentityLinks(c.Request.Context())
	if err != nil {
		h.deny(c, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"conflicts": links, "total": len(links)})
}

type resolveConflictRequest struct {
	Action          string `json:"action" binding:"required"` // bind | quarantine
	KnowledgeUserID string `json:"knowledge_user_id"`
}

// ResolveIdentityConflict POST /identity-conflicts/{id}/resolve：bind 需显式
// knowledge_user_id 且通过占用检查（409）；quarantine 直接隔离。均写审计。
func (h *AdminSyncHandler) ResolveIdentityConflict(c *gin.Context) {
	claims := h.claims(c)
	if !h.requireSystemAdmin(c, claims) {
		return
	}
	var req resolveConflictRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.deny(c, http.StatusBadRequest, "INVALID_REQUEST", "action is required (bind|quarantine)")
		return
	}

	ctx := c.Request.Context()
	link, err := h.storage.GetIdentityLink(ctx, c.Param("id"))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		h.deny(c, http.StatusNotFound, "IDENTITY_CONFLICT_NOT_FOUND", "identity link not found")
		return
	}
	if err != nil {
		h.deny(c, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	if link.Status != "conflicted" {
		h.deny(c, http.StatusBadRequest, "INVALID_REQUEST", "identity link is not conflicted")
		return
	}

	now := time.Now()
	switch req.Action {
	case "bind":
		if strings.TrimSpace(req.KnowledgeUserID) == "" {
			h.deny(c, http.StatusBadRequest, "INVALID_REQUEST", "knowledge_user_id is required for bind")
			return
		}
		// 占用检查：其他 active 主体不得占用该 knowledge_user_id
		occupants, err := h.storage.ListIdentityLinksByKnowledgeUser(ctx, req.KnowledgeUserID)
		if err != nil {
			h.deny(c, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		for _, l := range occupants {
			if l.Status == "active" && l.ID != link.ID {
				h.deny(c, http.StatusConflict, "IDENTITY_CONFLICT_OCCUPIED",
					"knowledge_user_id already bound to an active subject")
				return
			}
		}
		if _, err := h.storage.UpdateIdentityLinkTx(h.storage.StateDB().WithContext(ctx), link.ID, map[string]interface{}{
			"status":            "active",
			"knowledge_user_id": req.KnowledgeUserID,
			"updated_at":        now,
		}); err != nil {
			h.deny(c, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
	case "quarantine":
		if _, err := h.storage.UpdateIdentityLinkTx(h.storage.StateDB().WithContext(ctx), link.ID, map[string]interface{}{
			"status":     "disabled",
			"updated_at": now,
		}); err != nil {
			h.deny(c, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
	default:
		h.deny(c, http.StatusBadRequest, "INVALID_REQUEST", "action must be bind|quarantine")
		return
	}

	h.audit(ctx, claims.Subject, "integration.admin.identity_conflict.resolved", "POST "+c.FullPath(), map[string]interface{}{
		"link_id": link.ID, "subject": link.ImmutableSubject,
		"action": req.Action, "knowledge_user_id": req.KnowledgeUserID,
	}, c)

	final, err := h.storage.GetIdentityLink(ctx, link.ID)
	if err != nil {
		h.deny(c, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": final.ID, "status": final.Status, "knowledge_user_id": final.KnowledgeUserID})
}
