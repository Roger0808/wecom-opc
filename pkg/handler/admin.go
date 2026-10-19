package handler

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"weknora-tokendance/pkg/model"

	"github.com/gin-gonic/gin"
)

var (
	syncStateLock sync.RWMutex
	isSyncing     bool
	lastSyncTime  time.Time
	lastSyncErr   string
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

	syncStateLock.RLock()
	if !lastSyncTime.IsZero() {
		stats.LastSyncTime = lastSyncTime.Format("2006-01-02 15:04:05")
	}
	syncStateLock.RUnlock()

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": stats,
		"sync_state": gin.H{
			"is_syncing":      isSyncing,
			"last_sync_time":  stats.LastSyncTime,
			"last_sync_error": lastSyncErr,
		},
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

// AdminTriggerSyncAll 触发全量同步
func (h *Handler) AdminTriggerSyncAll(c *gin.Context) {
	syncStateLock.Lock()
	if isSyncing {
		syncStateLock.Unlock()
		c.JSON(http.StatusConflict, gin.H{"code": -1, "message": "已有同步任务正在进行中，请勿重复触发"})
		return
	}
	isSyncing = true
	syncStateLock.Unlock()

	go func() {
		defer func() {
			syncStateLock.Lock()
			isSyncing = false
			lastSyncTime = time.Now()
			syncStateLock.Unlock()
		}()

		log.Println("[Admin] 手动触发全量组织架构与配置同步...")
		err := h.syncer.SyncAll(context.Background())
		syncStateLock.Lock()
		if err != nil {
			lastSyncErr = err.Error()
			log.Printf("[Admin] 全量同步失败: %v", err)
		} else {
			lastSyncErr = ""
			log.Println("[Admin] 全量同步执行成功！")
		}
		syncStateLock.Unlock()
	}()

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "全量同步任务已在后台启动",
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

