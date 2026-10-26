package handler

import (
	"context"
	"crypto/sha256"
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
	ID             uuid.UUID             `json:"id"`
	Name           string                `json:"name"`
	Slug           string                `json:"slug"`
	Description    string                `json:"description"`
	AppURL         string                `json:"app_url"`
	ContainerPort  int                   `json:"container_port"`
	Status         string                `json:"status"`
	AppConfig      []model.AppConfigItem `json:"app_config"`
	ResourceIDs    []string              `json:"resource_ids"`
	SkillName      string                `json:"skill_name"`
	CatalogSkillID string                `json:"catalog_skill_id"`
	SkillSynced    bool                  `json:"skill_synced"`
	BuildLog       string                `json:"build_log"`
	ErrorMessage   string                `json:"error_message"`
	CreatedBy      string                `json:"created_by"`
	CreatedAt      time.Time             `json:"created_at"`
	UpdatedAt      time.Time             `json:"updated_at"`
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
			EnvPreview:  resourcePreview(res.EnvVars),
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
		if p, err := strconv.Atoi(portStr); err == nil && p > 0 && p <= 65535 {
			port = p
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid container_port"})
			return
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
	if _, err := mergeAppEnvironment(h.cfg.AppDeploy.OnlineResources, parsedResourceIDs, parsedAppConfig); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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
	artifactReceipt := fmt.Sprintf("[Artifact] app.tar:%x\n[Artifact] skill.zip:%x\n", sha256.Sum256(tarBytes), sha256.Sum256(zipBytes))

	log.Printf("[DeployJob][%s] Starting background deployment...", slug)

	mergedEnvList, err := mergeAppEnvironment(h.cfg.AppDeploy.OnlineResources, resourceIDs, appConfigs)
	if err != nil {
		_ = h.storage.UpdateAppStatus(ctx, slug, model.AppStatusFailed, err.Error(), err.Error(), "", false)
		return
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

	// 3. 注入配置与端口（新建应用尚未拥有首个镜像时可能返回 1110，忽略并在构建完成后二次确认）
	if err := h.caproverClient.UpdateAppConfigAndEnv(ctx, slug, port, mergedEnvList); err != nil {
		errLower := strings.ToLower(err.Error())
		if strings.Contains(errLower, "1110") || strings.Contains(errLower, "imagename for deployed version is not available") {
			log.Printf("[DeployJob][%s] Pre-build UpdateAppConfigAndEnv returned 1110 (initial image not ready yet), will apply after build: %v", slug, err)
		} else {
			errMsg := fmt.Sprintf("UpdateAppConfigAndEnv failed: %v", err)
			log.Printf("[DeployJob][%s] %s", slug, errMsg)
			_ = h.storage.UpdateAppStatus(ctx, slug, model.AppStatusFailed, errMsg, errMsg, "", false)
			return
		}
	}

	// 4. 上传 app.tar
	if err := h.caproverClient.UploadAppTar(ctx, slug, tarBytes); err != nil {
		errMsg := fmt.Sprintf("UploadAppTar failed: %v", err)
		log.Printf("[DeployJob][%s] %s", slug, errMsg)
		_ = h.storage.UpdateAppStatus(ctx, slug, model.AppStatusFailed, errMsg, errMsg, "", false)
		return
	}

	// 5. 等待构建完成并探测 /healthz (实时回调更新数据库日志)
	buildLog, err := h.caproverClient.WaitBuildConfigureAndHealthz(ctx, slug, appURL, func() error {
		return h.caproverClient.UpdateAppConfigAndEnv(ctx, slug, port, mergedEnvList)
	}, func(liveLog string) {
		_ = h.storage.UpdateAppLog(ctx, slug, artifactReceipt+liveLog)
	})
	if err != nil {
		errMsg := fmt.Sprintf("WaitBuildAndHealthz failed: %v", err)
		log.Printf("[DeployJob][%s] %s", slug, errMsg)
		logCtx, logCancel := context.WithTimeout(ctx, 10*time.Second)
		runtimeLog, logErr := h.caproverClient.RuntimeLogs(logCtx, slug)
		logCancel()
		if logErr == nil && strings.TrimSpace(runtimeLog) != "" {
			buildLog += "\n[Runtime logs]\n" + redactAppRuntimeLogs(runtimeLog, mergedEnvList)
		}
		_ = h.storage.UpdateAppStatus(ctx, slug, model.AppStatusFailed, artifactReceipt+redactAppRuntimeLogs(buildLog+"\n"+errMsg, mergedEnvList), errMsg, "", false)
		return
	}

	// 6. 同步技能至 knowledge-opc 技能池
	catalogSkillID := ""
	skillSynced := false
	if h.cfg.AppDeploy.AutoSyncSkill && h.knowledgeSyncer != nil {
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
	if err := h.storage.UpdateAppStatus(ctx, slug, model.AppStatusRunning, artifactReceipt+buildLog, "", catalogSkillID, skillSynced); err != nil {
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

	mergedEnvList, mergeErr := mergeAppEnvironment(h.cfg.AppDeploy.OnlineResources, req.ResourceIDs, req.AppConfig)
	if mergeErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": mergeErr.Error()})
		return
	}
	if h.caproverClient == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "deployment service unavailable"})
		return
	}

	// 先调用 CapRover 更新环境变量触发重启，成功后再落库，避免容器更新失败但数据库出现脏状态 (V09)
	if h.caproverClient != nil {
		if err := h.caproverClient.UpdateAppConfigAndEnv(c.Request.Context(), slug, port, mergedEnvList); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("update caprover app env: %v", err)})
			return
		}
	}

	if err := h.storage.UpdateAppConfig(c.Request.Context(), slug, port, string(configBytes), string(resourceBytes)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("update app config in db: %v", err)})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "App configuration updated and restarted successfully",
		"slug":    slug,
	})
}
