package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"weknora-tokendance/pkg/model"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var slugRegex = regexp.MustCompile(`^[a-z][a-z0-9-]{2,39}$`)

// CheckSlugRequest 校验标识请求体
type CheckSlugRequest struct {
	Slug string `json:"slug" binding:"required"`
}

// CheckSlugResponse 校验标识响应体
type CheckSlugResponse struct {
	Available bool   `json:"available"`
	Slug      string `json:"slug"`
	AppURL    string `json:"app_url"`
	SkillName string `json:"skill_name"`
	Message   string `json:"message,omitempty"`
}

// AppDetailResponse 单个应用详情响应
type AppDetailResponse struct {
	ID              uuid.UUID             `json:"id"`
	Name            string                `json:"name"`
	Slug            string                `json:"slug"`
	Description     string                `json:"description"`
	AppURL          string                `json:"app_url"`
	ContainerPort   int                   `json:"container_port"`
	Status          string                `json:"status"`
	AppConfig       []model.AppConfigItem `json:"app_config"`
	ResourceIDs     []string              `json:"resource_ids"`
	SkillName       string                `json:"skill_name"`
	CatalogSkillID  string                `json:"catalog_skill_id"`
	SkillSynced     bool                  `json:"skill_synced"`
	BuildLog        string                `json:"build_log"`
	ErrorMessage    string                `json:"error_message"`
	CreatedBy       string                `json:"created_by"`
	CreatedAt       time.Time             `json:"created_at"`
	UpdatedAt       time.Time             `json:"updated_at"`
}

// UpdateAppConfigRequest 修改配置请求体
type UpdateAppConfigRequest struct {
	ContainerPort int                   `json:"container_port"`
	AppConfig     []model.AppConfigItem `json:"app_config"`
	ResourceIDs   []string              `json:"resource_ids"`
}

// CheckSlug 校验 slug 唯一性并分配内网访问地址
func (h *Handler) CheckSlug(c *gin.Context) {
	var req CheckSlugRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request parameters"})
		return
	}

	slug := strings.TrimSpace(strings.ToLower(req.Slug))
	if !slugRegex.MatchString(slug) {
		c.JSON(http.StatusBadRequest, CheckSlugResponse{
			Available: false,
			Slug:      slug,
			Message:   "slug 必须以小写字母开头，只包含小写字母、数字和连字符，长度在 3 到 40 个字符之间",
		})
		return
	}

	available, err := h.storage.CheckSlugAvailable(c.Request.Context(), slug)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("check slug: %v", err)})
		return
	}

	intranetDomain := h.cfg.AppDeploy.IntranetRootDomain
	if intranetDomain == "" {
		intranetDomain = "apps.opc.intra"
	}

	appURL := fmt.Sprintf("http://%s.%s", slug, intranetDomain)
	skillName := fmt.Sprintf("app-%s", slug)

	c.JSON(http.StatusOK, CheckSlugResponse{
		Available: available,
		Slug:      slug,
		AppURL:    appURL,
		SkillName: skillName,
	})
}

// ListResources 返回脱敏后的线上现有可用资源列表
func (h *Handler) ListResources(c *gin.Context) {
	resources := make([]model.ResourceSummary, 0, len(h.cfg.AppDeploy.OnlineResources))
	for _, res := range h.cfg.AppDeploy.OnlineResources {
		keys := make([]string, 0, len(res.EnvVars))
		for k := range res.EnvVars {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		resources = append(resources, model.ResourceSummary{
			ID:          res.ID,
			Name:        res.Name,
			Type:        res.Type,
			Description: res.Description,
			EnvKeys:     keys,
		})
	}

	c.JSON(http.StatusOK, gin.H{"resources": resources})
}

// PublishApp 接收应用包与配置并触发异步部署
func (h *Handler) PublishApp(c *gin.Context) {
	name := strings.TrimSpace(c.PostForm("name"))
	slug := strings.TrimSpace(strings.ToLower(c.PostForm("slug")))
	description := strings.TrimSpace(c.PostForm("description"))
	portStr := strings.TrimSpace(c.PostForm("container_port"))
	appConfigStr := strings.TrimSpace(c.PostForm("app_config"))
	resourceIDsStr := strings.TrimSpace(c.PostForm("resource_ids"))

	if name == "" || slug == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name and slug are required"})
		return
	}
	if !slugRegex.MatchString(slug) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid slug format"})
		return
	}

	port := 8080
	if portStr != "" {
		if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
			port = p
		}
	}

	if appConfigStr == "" {
		appConfigStr = "[]"
	}
	if resourceIDsStr == "" {
		resourceIDsStr = "[]"
	}

	var parsedAppConfig []model.AppConfigItem
	if err := json.Unmarshal([]byte(appConfigStr), &parsedAppConfig); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid app_config json"})
		return
	}

	var parsedResourceIDs []string
	if err := json.Unmarshal([]byte(resourceIDsStr), &parsedResourceIDs); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid resource_ids json"})
		return
	}

	// 读取上传的 app.tar
	appTarFile, err := c.FormFile("app_tar")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "app_tar file is required"})
		return
	}
	tarReader, err := appTarFile.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "open app_tar file failed"})
		return
	}
	defer tarReader.Close()
	tarBytes, err := io.ReadAll(tarReader)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read app_tar file failed"})
		return
	}

	// 读取上传的 skill.zip
	skillZipFile, err := c.FormFile("skill_zip")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "skill_zip file is required"})
		return
	}
	zipReader, err := skillZipFile.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "open skill_zip file failed"})
		return
	}
	defer zipReader.Close()
	zipBytes, err := io.ReadAll(zipReader)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read skill_zip file failed"})
		return
	}

	intranetDomain := h.cfg.AppDeploy.IntranetRootDomain
	if intranetDomain == "" {
		intranetDomain = "apps.opc.intra"
	}
	appURL := fmt.Sprintf("http://%s.%s", slug, intranetDomain)
	skillName := fmt.Sprintf("app-%s", slug)

	// 落库 deployed_apps (初始状态 building)
	deployedApp := &model.DeployedApp{
		Name:            name,
		Slug:            slug,
		Description:     description,
		AppURL:          appURL,
		ContainerPort:   port,
		Status:          model.AppStatusBuilding,
		AppConfigJSON:   appConfigStr,
		ResourceIDsJSON: resourceIDsStr,
		SkillName:       skillName,
		BuildLog:        "Deployment job received. Initializing build...",
	}

	if err := h.storage.CreateOrUpdateApp(c.Request.Context(), deployedApp); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("save app record: %v", err)})
		return
	}

	// 异步启动部署流程
	go h.runDeploymentAsync(slug, port, appURL, parsedAppConfig, parsedResourceIDs, tarBytes, zipBytes)

	c.JSON(http.StatusAccepted, gin.H{
		"slug":       slug,
		"status":     model.AppStatusBuilding,
		"app_url":    appURL,
		"skill_name": skillName,
	})
}

// runDeploymentAsync 在后台执行部署至 deploy-opc 并同步技能至 knowledge-opc
func (h *Handler) runDeploymentAsync(slug string, port int, appURL string, appConfigs []model.AppConfigItem, resourceIDs []string, tarBytes []byte, zipBytes []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	log.Printf("[DeployJob][%s] Starting background deployment...", slug)

	// 1. 合并环境变量 (选中的线上资源配置 + 用户确认的应用配置)
	mergedEnvsMap := make(map[string]string)

	// 注入选中的线上资源
	resMap := make(map[string]map[string]string)
	for _, res := range h.cfg.AppDeploy.OnlineResources {
		resMap[res.ID] = res.EnvVars
	}
	for _, resID := range resourceIDs {
		if envs, ok := resMap[resID]; ok {
			for k, v := range envs {
				mergedEnvsMap[k] = v
			}
		}
	}

	// 注入应用自定义配置（用户自定义配置具有最高优先级）
	for _, cfg := range appConfigs {
		if cfg.Key != "" {
			mergedEnvsMap[cfg.Key] = cfg.Value
		}
	}

	mergedEnvList := make([]model.AppConfigItem, 0, len(mergedEnvsMap))
	for k, v := range mergedEnvsMap {
		mergedEnvList = append(mergedEnvList, model.AppConfigItem{Key: k, Value: v})
	}

	if h.caproverClient == nil {
		log.Printf("[DeployJob][%s] CapRover client not initialized. Marked as failed.", slug)
		_ = h.storage.UpdateAppStatus(ctx, slug, model.AppStatusFailed, "CapRover client not configured", "CapRover client not configured", "", false)
		return
	}

	// 2. 注册应用
	if err := h.caproverClient.EnsureAppRegistered(ctx, slug); err != nil {
		errMsg := fmt.Sprintf("EnsureAppRegistered failed: %v", err)
		log.Printf("[DeployJob][%s] %s", slug, errMsg)
		_ = h.storage.UpdateAppStatus(ctx, slug, model.AppStatusFailed, errMsg, errMsg, "", false)
		return
	}

	// 3. 注入配置与端口
	if err := h.caproverClient.UpdateAppConfigAndEnv(ctx, slug, port, mergedEnvList); err != nil {
		errMsg := fmt.Sprintf("UpdateAppConfigAndEnv failed: %v", err)
		log.Printf("[DeployJob][%s] %s", slug, errMsg)
		_ = h.storage.UpdateAppStatus(ctx, slug, model.AppStatusFailed, errMsg, errMsg, "", false)
		return
	}

	// 4. 上传 app.tar
	if err := h.caproverClient.UploadAppTar(ctx, slug, tarBytes); err != nil {
		errMsg := fmt.Sprintf("UploadAppTar failed: %v", err)
		log.Printf("[DeployJob][%s] %s", slug, errMsg)
		_ = h.storage.UpdateAppStatus(ctx, slug, model.AppStatusFailed, errMsg, errMsg, "", false)
		return
	}

	// 5. 等待构建完成并探测 /healthz
	buildLog, err := h.caproverClient.WaitBuildAndHealthz(ctx, slug, appURL)
	if err != nil {
		errMsg := fmt.Sprintf("WaitBuildAndHealthz failed: %v", err)
		log.Printf("[DeployJob][%s] %s", slug, errMsg)
		_ = h.storage.UpdateAppStatus(ctx, slug, model.AppStatusFailed, buildLog+"\n"+errMsg, errMsg, "", false)
		return
	}

	// 6. 同步技能至 knowledge-opc 技能池
	catalogSkillID := ""
	skillSynced := false
	if h.knowledgeSyncer != nil {
		catID, err := h.knowledgeSyncer.PushAndEnableSkill(ctx, zipBytes)
		if err != nil {
			log.Printf("[DeployJob][%s] PushAndEnableSkill warning: %v", slug, err)
			buildLog += fmt.Sprintf("\n[Warning] Push to knowledge-opc failed: %v", err)
		} else {
			catalogSkillID = catID
			skillSynced = true
			buildLog += fmt.Sprintf("\n[Success] Pushed to knowledge-opc Catalog ID: %s", catalogSkillID)
		}
	}

	// 7. 更新状态为 running
	if err := h.storage.UpdateAppStatus(ctx, slug, model.AppStatusRunning, buildLog, "", catalogSkillID, skillSynced); err != nil {
		log.Printf("[DeployJob][%s] Failed to set status to running: %v", slug, err)
		return
	}
	log.Printf("[DeployJob][%s] App deployed successfully! Running at %s", slug, appURL)
}

// ListApps 查询所有部署应用列表
func (h *Handler) ListApps(c *gin.Context) {
	apps, err := h.storage.ListApps(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("list apps: %v", err)})
		return
	}

	result := make([]AppDetailResponse, 0, len(apps))
	for _, app := range apps {
		var configs []model.AppConfigItem
		_ = json.Unmarshal([]byte(app.AppConfigJSON), &configs)

		var resIDs []string
		_ = json.Unmarshal([]byte(app.ResourceIDsJSON), &resIDs)

		result = append(result, AppDetailResponse{
			ID:             app.ID,
			Name:           app.Name,
			Slug:           app.Slug,
			Description:    app.Description,
			AppURL:         app.AppURL,
			ContainerPort:  app.ContainerPort,
			Status:         app.Status,
			AppConfig:      configs,
			ResourceIDs:    resIDs,
			SkillName:      app.SkillName,
			CatalogSkillID: app.CatalogSkillID,
			SkillSynced:    app.SkillSynced,
			BuildLog:       app.BuildLog,
			ErrorMessage:   app.ErrorMessage,
			CreatedBy:      app.CreatedBy,
			CreatedAt:      app.CreatedAt,
			UpdatedAt:      app.UpdatedAt,
		})
	}

	c.JSON(http.StatusOK, gin.H{"apps": result})
}

// GetApp 查询单个应用详情及部署状态
func (h *Handler) GetApp(c *gin.Context) {
	slug := strings.TrimSpace(strings.ToLower(c.Param("slug")))
	app, err := h.storage.GetAppBySlug(c.Request.Context(), slug)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}

	var configs []model.AppConfigItem
	_ = json.Unmarshal([]byte(app.AppConfigJSON), &configs)

	var resIDs []string
	_ = json.Unmarshal([]byte(app.ResourceIDsJSON), &resIDs)

	c.JSON(http.StatusOK, AppDetailResponse{
		ID:             app.ID,
		Name:           app.Name,
		Slug:           app.Slug,
		Description:    app.Description,
		AppURL:         app.AppURL,
		ContainerPort:  app.ContainerPort,
		Status:         app.Status,
		AppConfig:      configs,
		ResourceIDs:    resIDs,
		SkillName:      app.SkillName,
		CatalogSkillID: app.CatalogSkillID,
		SkillSynced:    app.SkillSynced,
		BuildLog:       app.BuildLog,
		ErrorMessage:   app.ErrorMessage,
		CreatedBy:      app.CreatedBy,
		CreatedAt:      app.CreatedAt,
		UpdatedAt:      app.UpdatedAt,
	})
}

// UpdateAppConfig 修改应用配置与绑定的线上资源，并触发容器秒级重启
func (h *Handler) UpdateAppConfig(c *gin.Context) {
	slug := strings.TrimSpace(strings.ToLower(c.Param("slug")))
	var req UpdateAppConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	app, err := h.storage.GetAppBySlug(c.Request.Context(), slug)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}

	port := req.ContainerPort
	if port <= 0 {
		port = app.ContainerPort
	}
	if port <= 0 {
		port = 8080
	}

	configBytes, _ := json.Marshal(req.AppConfig)
	resourceBytes, _ := json.Marshal(req.ResourceIDs)

	if err := h.storage.UpdateAppConfig(c.Request.Context(), slug, port, string(configBytes), string(resourceBytes)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("update app config in db: %v", err)})
		return
	}

	// 重新计算合并环境变量
	mergedEnvsMap := make(map[string]string)
	resMap := make(map[string]map[string]string)
	for _, res := range h.cfg.AppDeploy.OnlineResources {
		resMap[res.ID] = res.EnvVars
	}
	for _, resID := range req.ResourceIDs {
		if envs, ok := resMap[resID]; ok {
			for k, v := range envs {
				mergedEnvsMap[k] = v
			}
		}
	}
	for _, cfg := range req.AppConfig {
		if cfg.Key != "" {
			mergedEnvsMap[cfg.Key] = cfg.Value
		}
	}

	mergedEnvList := make([]model.AppConfigItem, 0, len(mergedEnvsMap))
	for k, v := range mergedEnvsMap {
		mergedEnvList = append(mergedEnvList, model.AppConfigItem{Key: k, Value: v})
	}

	// 调用 CapRover 更新环境变量触发重启
	if h.caproverClient != nil {
		if err := h.caproverClient.UpdateAppConfigAndEnv(c.Request.Context(), slug, port, mergedEnvList); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("update caprover app env: %v", err)})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "App configuration updated and restarted successfully",
		"slug":    slug,
	})
}
