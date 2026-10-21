package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/accountcenter"
	"weknora-tokendance/pkg/adminauth"
	"weknora-tokendance/pkg/auth"
	"weknora-tokendance/pkg/authz"
	"weknora-tokendance/pkg/handler"
	"weknora-tokendance/pkg/kbridge"
	"weknora-tokendance/pkg/storage"
	"weknora-tokendance/pkg/syncer"
	"weknora-tokendance/pkg/syncservice"
	"weknora-tokendance/pkg/web"
	"weknora-tokendance/pkg/wecom"

	"github.com/gin-gonic/gin"
)

func main() {
	configPath := "config.yaml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load config from %s: %v", configPath, err)
	}

	log.Println("[Main] Initializing Storage...")
	storageInstance, err := storage.NewStorage(cfg.Database)
	if err != nil {
		log.Fatalf("Failed to connect database: %v", err)
	}

	// 平台自有状态库（与 WeKnora 库分离、独立账号）：审计、身份、授权与同步账本均落此库
	log.Println("[Main] Connecting platform state database...")
	stateDB, err := storage.NewStateDB(cfg.StateDB)
	if err != nil {
		log.Fatalf("Failed to connect state database: %v", err)
	}
	storageInstance.WithStateDB(stateDB)

	// 启动时在状态库上按序执行显式版本化迁移（幂等，事务内执行）
	if err := storageInstance.RunMigrations(context.Background()); err != nil {
		log.Fatalf("Failed to run state database migrations: %v", err)
	}

	if cfg.Admin.Key == "" {
		log.Fatal("Admin.Key is required (config admin.key or env PLATFORM_ADMIN_KEY): refusing to start with open admin API")
	}
	if cfg.Auth.Key == "" {
		log.Fatal("Auth.Key is required (config auth.key or env PLATFORM_AUTH_KEY): refusing to start with open integration auth API")
	}
	if cfg.Bridge.Key == "" {
		log.Fatal("Bridge.Key is required (config bridge.key or env PLATFORM_BRIDGE_KEY): refusing to start without knowledge compat bridge signing key")
	}
	if strings.TrimSpace(cfg.Server.ExternalBaseURL) == "" {
		log.Fatal("Server.ExternalBaseURL is required (config server.external_base_url or env EXTERNAL_BASE_URL): refusing to derive callback URLs from forgeable request Host headers")
	}

	log.Println("[Main] Initializing WeCom Client...")
	wecomClient := wecom.NewClient(cfg.WeCom)

	log.Println("[Main] Initializing Unified Account Center Client...")
	acClient := accountcenter.NewClient(cfg.GraphQL.Endpoint)

	log.Println("[Main] Initializing Syncer & AuthService...")
	syncerInstance := syncer.NewSyncer(cfg, wecomClient, acClient, storageInstance)
	authInstance := auth.NewAuthService(cfg, storageInstance)

	// knowledge 兼容桥（TASK-P09 / ADR-004）：为有效映射用户签发 ≤120s 上游个人
	// token（签名密钥 Bridge.Key 独立注入），并作为撤权上游 token 撤销器
	// （authz.TokenRevoker）与失效通知通道接入撤权引擎。
	bridge := kbridge.New(
		kbridge.Config{SignKey: cfg.Bridge.Key, Audit: storageInstance.InsertAuditEvent},
		storageInstance,
		authz.NewEntitlementChecker(storageInstance, nil),
		kbridge.NewWeKnoraStore(storageInstance),
	)

	// 手动同步服务（TASK-P08 / INT-130）：同步运行状态机的唯一入口。
	// BND-130：删除启动即跑的全量同步与 cron 定时注册（旧触发点归一），
	// 全量/单部门同步统一走 /integration/v1/admin/sync/runs。
	syncService := syncservice.NewService(cfg, acClient, storageInstance, syncerInstance,
		syncservice.WithRevoker(bridge))

	// 初始化 HTTP 服务
	r := gin.Default()
	h := handler.NewHandler(cfg, wecomClient, acClient, syncerInstance, authInstance, storageInstance)
	h.SetSyncService(syncService)

	// 注册路由
	wecomGroup := r.Group("/wecom")
	{
		wecomGroup.GET("/login", h.LoginRedirect)           // 企微扫码/网页免登入口
		wecomGroup.GET("/callback", h.OAuthCallback)        // 企微回调 (含 URL 验证与 OAuth 登录)
		wecomGroup.POST("/callback", h.HandleEventCallback) // 企微事件与消息通知回调
		wecomGroup.POST("/sync", h.TriggerSync)             // 旧手动同步端点：410 Gone（BND-130，归一到 admin/sync/runs）
	}

	oidcGroup := r.Group("/oidc")
	{
		oidcGroup.GET("/authorize", h.OIDCAuthorize)
		oidcGroup.POST("/token", h.OIDCToken)
		oidcGroup.GET("/userinfo", h.OIDCUserInfo)
	}

	// 空间与用户配置同步管理后台
	r.GET("/admin", func(c *gin.Context) {
		content, err := web.AdminFS.ReadFile("admin.html")
		if err != nil {
			c.String(http.StatusInternalServerError, "Admin UI template not found")
			return
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", content)
	})

	// 管理 API 全组挂载服务内认证 + CSRF 防护（BND-170/171）；
	// 后续 INT-130/INT-141 新增的管理路由复用同一中间件，按路由组追加即可
	adminAuthCfg := adminauth.MiddlewareConfig{
		Secret:         []byte(cfg.Admin.Key),
		AllowedOrigins: cfg.Admin.AllowedOrigins,
		Audit:          storageInstance.InsertAuditEvent,
	}
	adminAPI := r.Group("/admin/api", adminauth.RequireAdmin(adminAuthCfg))
	{
		adminAPI.GET("/overview", h.AdminGetOverview)
		adminAPI.GET("/spaces", h.AdminListSpaces)
		adminAPI.POST("/sync/all", h.AdminTriggerSyncAll)
		adminAPI.POST("/spaces/:id/sync", h.AdminTriggerSyncSpace)
		adminAPI.GET("/users", h.AdminListUsers)
		adminAPI.POST("/spaces/:id/admins", h.AdminUpdateSpaceAdmin)
		adminAPI.GET("/root-config", h.AdminGetRootConfig)
		adminAPI.PUT("/root-config/models", h.AdminUpdateRootModels)
		adminAPI.PUT("/root-config/storage", h.AdminUpdateRootStorage)
		adminAPI.GET("/root-config/sandbox", h.AdminGetRootSandbox)
		adminAPI.PUT("/root-config/sandbox", h.AdminUpdateRootSandbox)
		adminAPI.POST("/root-config/broadcast", h.AdminBroadcastRootConfig)
	}

	r.GET("/health", h.HealthCheck)

	// 客户端认证接口组（TASK-P04 / INT-101）：桌面客户端登录事务、令牌轮换与兑换。
	// 审计落在平台状态库；错误信封 {error:{code,message,request_id}}。
	integrationHandler := handler.NewIntegrationHandler(cfg, wecomClient, acClient, storageInstance)
	v1 := r.Group("/integration/v1")
	{
		v1.POST("/auth/authorize", integrationHandler.Authorize)
		v1.GET("/auth/callback", integrationHandler.Callback)
		v1.POST("/auth/token", integrationHandler.Token)
		v1.POST("/auth/refresh", integrationHandler.Refresh)
		v1.POST("/auth/logout", integrationHandler.Logout)
		v1.GET("/session", integrationHandler.Session)
		v1.POST("/auth/code-exchange", integrationHandler.CodeExchange)
	}

	// 手动同步服务管理接口组（TASK-P08 / INT-130）：同步运行与身份冲突治理。
	// 复用 P01 认证 + CSRF（RequireAdminAuth：dept_admin 的租户范围由各端点按 claims 判定）。
	adminSyncHandler := handler.NewAdminSyncHandler(cfg, storageInstance, syncService)
	adminSync := v1.Group("/admin", adminauth.RequireAdminAuth(adminAuthCfg))
	{
		adminSync.POST("/sync/runs", adminSyncHandler.CreateSyncRun)
		adminSync.GET("/sync/runs", adminSyncHandler.ListSyncRuns)
		adminSync.GET("/sync/runs/:id", adminSyncHandler.GetSyncRun)
		adminSync.POST("/sync/runs/:id/retry", adminSyncHandler.RetrySyncRun)
		adminSync.GET("/identity-conflicts", adminSyncHandler.ListIdentityConflicts)
		adminSync.POST("/identity-conflicts/:id/resolve", adminSyncHandler.ResolveIdentityConflict)
	}

	// 服务间接口组（TASK-P05/P06 / INT-102）：任务租约签发/续期、授权视图与撤权。
	// 全部端点要求 system_admin 服务 token；撤权先事务持久化、提交后异步通知。
	// P09：上游 token 撤销器注入兼容桥，桥同时注册为失效通知通道。
	internalHandler := handler.NewInternalHandler(cfg, storageInstance, handler.WithTokenRevoker(bridge))
	internal := v1.Group("/internal", internalHandler.RequireServiceToken)
	{
		internal.POST("/grants", internalHandler.IssueGrant)
		internal.POST("/grants/:id/renew", internalHandler.RenewGrant)
		internal.GET("/authz", internalHandler.AuthzView)
		internal.POST("/authz", internalHandler.AuthzBatch)
		internal.POST("/revoke", internalHandler.Revoke)
	}
	internalHandler.Notifier().Register(bridge)
	if cfg.Code.BaseURL != "" {
		internalHandler.Notifier().Register(&authz.CodeInvalidateChannel{BaseURL: cfg.Code.BaseURL, ServiceToken: cfg.Code.ServiceToken})
		log.Printf("[Init] code invalidate push channel registered (base_url=%s)", cfg.Code.BaseURL)
	}
	internalHandler.Notifier().Start(context.Background())

	// 知识适配接口组（TASK-P07 / INT-120）：检索、内容读取与原文件下载。
	// Bearer 平台 access token；服务端重授权（tenant ∈ active entitlements，
	// 桥内强制）；桥不可用/上游超时 15s → 502 UPSTREAM_UNAVAILABLE，绝不降级共享凭据。
	knowledgeHandler := handler.NewKnowledgeHandler(cfg, storageInstance, bridge)
	{
		v1.POST("/knowledge/search", knowledgeHandler.Search)
		v1.GET("/knowledge/:id", knowledgeHandler.GetKnowledge)
		v1.GET("/knowledge/:id/file", knowledgeHandler.DownloadFile)
	}

	// MCP 精确调用转发接口组（INT-150 platform 段，REQ-009/ADR-009/EXT-002）：
	// 客户端不直连 knowledge integration 路由；平台校验会话与 entitlement 后，
	// 桥签 120s 上游 token + HS256 调用上下文（bridge.key 签名），knowledge
	// 按 actor_kind=user 裁定策略，状态码与 code 原样透传，超时 502。
	// invoke-task 与 task 查询/取消为 task-actor 段（P12）：服务 token 入口，
	// actor_kind=task 且 task_id/device 绑定；查询/取消与会话入口同路径按
	// Authorization 类型分发。
	mcpHandler := handler.NewMCPHandler(cfg, storageInstance, bridge)
	{
		v1.POST("/mcp/invoke", mcpHandler.Invoke)
		v1.POST("/mcp/invoke-task", internalHandler.RequireServiceToken, mcpHandler.InvokeTask)
		v1.GET("/mcp/invocations/:id", mcpHandler.GetInvocation)
		v1.POST("/mcp/invocations/:id/cancel", mcpHandler.CancelInvocation)
		v1.POST("/mcp/invocations/:id/decision", mcpHandler.DecideInvocation)
		v1.GET("/mcp/services", mcpHandler.ListServices)
	}

	// 技能协调接口组（TASK-P10 / INT-141/142）：发布任务记账与客户端获取。
	// 管理端点挂 RequireAdminAuth（system_admin 或源空间 dept_admin）；
	// 客户端列表/下载按租户 entitlement 逐空间查 knowledge release。
	// internal/skills（INT-160）供 code 后端服务 token 查询技能清单。
	skillsHandler := handler.NewSkillsHandler(cfg, storageInstance, bridge)
	internal.GET("/skills", skillsHandler.InternalList)
	{
		v1.GET("/skills", skillsHandler.List)
		v1.GET("/skills/:publish_id/download", skillsHandler.Download)
		adminSkills := v1.Group("/admin", adminauth.RequireAdminAuth(adminAuthCfg))
		adminSkills.POST("/skills/publish-jobs", skillsHandler.CreatePublishJob)
		adminSkills.GET("/skills/publish-jobs/:id", skillsHandler.GetPublishJob)
	}

	port := cfg.Server.Port
	if port == 0 {
		port = 3002
	}

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: r,
	}

	go func() {
		log.Printf("[Main] TokenDance Gateway 服务正在监听端口 :%d ...", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	// 优雅停机
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("[Main] 正在关闭服务...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server forced to shutdown:", err)
	}

	log.Println("[Main] 服务已安全退出。")
}
