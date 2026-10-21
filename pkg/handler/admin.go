package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"

	"weknora-tokendance/pkg/adminauth"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/syncservice"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// AdminGetOverview 返回大盘统计数据
func (h *Handler) AdminGetOverview(c *gin.Context) {
	ctx := c.Request.Context()
	stats, err := h.storage.GetOverviewStats(ctx, h.cfg.WeKnora.RootTenantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}

	stats.CronEnabled = h.cfg.Sync.EnableCron
	stats.CronSchedule = h.cfg.Sync.CronSchedule
	if stats.CronSchedule == "" {
		stats.CronSchedule = "0 */30 * * *"
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": stats,
	})
}

// AdminListSpaces 返回空间看板与配置状态
func (h *Handler) AdminListSpaces(c *gin.Context) {
	ctx := c.Request.Context()
	keyword := c.Query("keyword")

	spaces, err := h.storage.ListSpacesWithConfig(ctx, h.cfg.WeKnora.RootTenantID, keyword)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":  0,
		"data":  spaces,
		"total": len(spaces),
	})
}

// AdminTriggerSyncAll 触发全量同步（H5 修复）：统一走 syncservice.Run(scope=full)，
// 享受同步运行账本 / 持久化互斥锁 / 快照守卫 / 幂等键契约；不再走 syncer.SyncAll，
// 也不再使用进程内 isSyncing 布尔（互斥以 sync_runs 持久化锁为准，并发触发 → 409）。
// 同步阻塞至终态后返回 run_id 与最终状态。
func (h *Handler) AdminTriggerSyncAll(c *gin.Context) {
	if h.syncService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": -1, "message": "sync service not assembled"})
		return
	}

	triggeredBy := "admin:unknown"
	if claims, ok := adminauth.GetClaims(c); ok && claims != nil && claims.Subject != "" {
		triggeredBy = "admin:" + claims.Subject
	}

	// 请求断连不取消运行（运行生命周期由持久化锁/租约承载，不用请求 context 跑后台 goroutine）
	ctx := context.WithoutCancel(c.Request.Context())
	log.Printf("[Admin] 手动触发全量组织架构与配置同步（syncservice full run, by %s）...", triggeredBy)
	runID, err := h.syncService.Run(ctx, string(syncservice.ScopeFull), "", "admin-sync-all|"+uuid.NewString(), triggeredBy, "rules/v1")
	if err != nil {
		switch {
		case errors.Is(err, syncservice.ErrConflict):
			c.JSON(http.StatusConflict, gin.H{"code": -1, "message": "已有同步任务正在进行中，请勿重复触发 (SYNC_RUN_CONFLICT)"})
		case errors.Is(err, syncservice.ErrInvalidRequest):
			c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": err.Error()})
		default:
			log.Printf("[Admin] 全量同步失败: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		}
		return
	}

	run, err := h.storage.GetSyncRun(ctx, runID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}
	log.Printf("[Admin] 全量同步运行结束：run_id=%s status=%s", run.RunID, run.Status)
	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "全量同步已执行完成",
		"data": gin.H{
			"run_id": run.RunID,
			"status": run.Status,
		},
	})
}

// AdminTriggerSyncSpace 触发单个空间配置同步
func (h *Handler) AdminTriggerSyncSpace(c *gin.Context) {
	idStr := c.Param("id")
	tenantID, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil || tenantID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "无效的空间ID"})
		return
	}

	ctx := c.Request.Context()
	log.Printf("[Admin] 针对空间 %d 触发模型与存储配置同步...", tenantID)
	if err := h.syncer.SyncSingleSpace(ctx, tenantID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "空间配置已成功同步",
	})
}

// AdminListUsers 搜索员工
func (h *Handler) AdminListUsers(c *gin.Context) {
	ctx := c.Request.Context()
	keyword := c.Query("keyword")
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))

	users, err := h.storage.SearchUsers(ctx, keyword, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":  0,
		"data":  users,
		"total": len(users),
	})
}

type updateSpaceAdminReq struct {
	UserID string `json:"user_id" binding:"required"`
	Action string `json:"action" binding:"required"` // "add" | "remove"
}

// AdminUpdateSpaceAdmin 设置或撤销空间管理员
func (h *Handler) AdminUpdateSpaceAdmin(c *gin.Context) {
	idStr := c.Param("id")
	tenantID, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil || tenantID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "无效的空间ID"})
		return
	}

	var req updateSpaceAdminReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "参数错误: user_id 和 action 不能为空"})
		return
	}

	ctx := c.Request.Context()
	if err := h.storage.UpdateSpaceAdmin(ctx, tenantID, req.UserID, req.Action); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "管理员权限更新成功",
	})
}

// getRootTenantID 获取母空间 ID，若未配置或为 0 则默认为 10001
func (h *Handler) getRootTenantID() uint64 {
	if h.cfg != nil && h.cfg.WeKnora.RootTenantID != 0 {
		return h.cfg.WeKnora.RootTenantID
	}
	return 10001
}

// getAESKey 获取系统 AES-256 密钥
func (h *Handler) getAESKey() []byte {
	if h.cfg != nil {
		return h.cfg.GetAESKey()
	}
	return nil
}

// AdminGetRootConfig 获取母空间的基准配置（包含模型列表与 MinIO 存储配置，敏感字段脱敏）
func (h *Handler) AdminGetRootConfig(c *gin.Context) {
	rootID := h.getRootTenantID()
	aesKey := h.getAESKey()

	config, err := h.storage.GetRootSpaceConfig(c.Request.Context(), rootID, aesKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": config,
	})
}

// updateRootModelsReq 批量更新母空间模型请求结构
type updateRootModelsReq struct {
	Models []model.UpdateModelReq `json:"models" binding:"required"`
}

// AdminUpdateRootModels 保存/更新母空间模型配置
func (h *Handler) AdminUpdateRootModels(c *gin.Context) {
	var req updateRootModelsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": fmt.Sprintf("参数错误: %v", err)})
		return
	}

	rootID := h.getRootTenantID()
	aesKey := h.getAESKey()

	if err := h.storage.UpdateRootModels(c.Request.Context(), rootID, req.Models, aesKey); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "母空间模型配置保存成功",
	})
}

// AdminUpdateRootStorage 保存/更新母空间 MinIO 存储配置
func (h *Handler) AdminUpdateRootStorage(c *gin.Context) {
	var req model.UpdateStorageReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": fmt.Sprintf("参数错误: %v", err)})
		return
	}

	rootID := h.getRootTenantID()
	aesKey := h.getAESKey()

	if err := h.storage.UpdateRootStorage(c.Request.Context(), rootID, req, aesKey); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "母空间 MinIO 存储配置保存成功",
	})
}

// AdminBroadcastRootConfig 级联广播下发母空间最新配置至所有子空间
func (h *Handler) AdminBroadcastRootConfig(c *gin.Context) {
	rootID := h.getRootTenantID()

	count, err := h.storage.BroadcastRootConfigToAllTenants(c.Request.Context(), rootID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": fmt.Sprintf("已成功将母空间最新配置级联下发至 %d 个子空间", count),
		"count":   count,
	})
}

// AdminGetRootSandbox 获取母空间沙箱环境配置
func (h *Handler) AdminGetRootSandbox(c *gin.Context) {
	rootID := h.getRootTenantID()

	sandboxConfig, err := h.storage.GetRootSandboxConfig(c.Request.Context(), rootID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": sandboxConfig,
	})
}

// AdminUpdateRootSandbox 保存/更新母空间沙箱环境配置
func (h *Handler) AdminUpdateRootSandbox(c *gin.Context) {
	var req model.UpdateSandboxReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": fmt.Sprintf("参数错误: %v", err)})
		return
	}

	if err := req.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": fmt.Sprintf("参数校验错误: %v", err)})
		return
	}

	rootID := h.getRootTenantID()

	if err := h.storage.UpdateRootSandboxConfig(c.Request.Context(), rootID, req); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -1, "message": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "母空间沙箱环境配置保存成功",
	})
}
