package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/accountcenter"
	"weknora-tokendance/pkg/auth"
	"weknora-tokendance/pkg/handler"
	"weknora-tokendance/pkg/storage"
	"weknora-tokendance/pkg/syncer"
	"weknora-tokendance/pkg/web"
	"weknora-tokendance/pkg/wecom"

	"github.com/gin-gonic/gin"
	"github.com/robfig/cron/v3"
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

	log.Println("[Main] Initializing WeCom Client...")
	wecomClient := wecom.NewClient(cfg.WeCom)

	log.Println("[Main] Initializing Unified Account Center Client...")
	acClient := accountcenter.NewClient(cfg.GraphQL.Endpoint)

	log.Println("[Main] Initializing Syncer & AuthService...")
	syncerInstance := syncer.NewSyncer(cfg, wecomClient, acClient, storageInstance)
	authInstance := auth.NewAuthService(cfg, storageInstance)

	// 启动时立即异步执行一次全量同步，确保服务一启动数据就是最新的
	go func() {
		log.Println("[Main] 服务启动，立即触发首次全量组织架构同步...")
		if err := syncerInstance.SyncAll(context.Background()); err != nil {
			log.Printf("[Main] 首次同步失败 (请检查企微凭据或网络): %v", err)
		}
	}()

	// 定时同步任务
	if cfg.Sync.EnableCron {
		c := cron.New()
		schedule := cfg.Sync.CronSchedule
		if schedule == "" {
			schedule = "0 */30 * * *" // 默认每 30 分钟同步一次
		}
		_, err := c.AddFunc(schedule, func() {
			log.Println("[Cron] 触发定时组织架构同步...")
			if err := syncerInstance.SyncAll(context.Background()); err != nil {
				log.Printf("[Cron] 组织架构同步失败: %v", err)
			}
		})
		if err != nil {
			log.Printf("[Cron] 定时任务配置失败: %v", err)
		} else {
			c.Start()
			log.Printf("[Cron] 定时任务已启动，执行周期: %s", schedule)
		}
	}

	// 初始化 HTTP 服务
	r := gin.Default()
	h := handler.NewHandler(cfg, wecomClient, acClient, syncerInstance, authInstance, storageInstance)

	// 注册路由
	wecomGroup := r.Group("/wecom")
	{
		wecomGroup.GET("/login", h.LoginRedirect)          // 企微扫码/网页免登入口
		wecomGroup.GET("/callback", h.OAuthCallback)       // 企微回调 (含 URL 验证与 OAuth 登录)
		wecomGroup.POST("/callback", h.HandleEventCallback)// 企微事件与消息通知回调
		wecomGroup.POST("/sync", h.TriggerSync)            // 手动全量同步触发
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

	adminAPI := r.Group("/admin/api")
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

	// 应用部署与转技能一期核心 API 路由
	appsAPI := r.Group("/api/v1/apps")
	{
		appsAPI.POST("/check-slug", h.CheckSlug)       // 校验 slug 唯一性并返回分配内网地址
		appsAPI.GET("/resources", h.ListResources)    // 获取脱敏后的线上现有可用资源列表
		appsAPI.POST("/publish", h.PublishApp)         // 上传应用包、技能包与配置触发异步部署
		appsAPI.GET("", h.ListApps)                   // 查询所有应用列表
		appsAPI.GET("/:slug", h.GetApp)               // 查询单个应用详情及部署状态
		appsAPI.PUT("/:slug/config", h.UpdateAppConfig)// 修改应用配置并触发容器秒级重启
	}

	r.GET("/health", h.HealthCheck)

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
