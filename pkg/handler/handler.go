package handler

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/accountcenter"
	"weknora-tokendance/pkg/auth"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"
	"weknora-tokendance/pkg/syncer"
	"weknora-tokendance/pkg/wecom"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type Handler struct {
	cfg     *config.Config
	wecom   *wecom.Client
	ac      *accountcenter.Client
	syncer  *syncer.Syncer
	auth    *auth.AuthService
	storage *storage.Storage

	// 临时授权码缓存 (用于 OIDC /token 兑换)
	authCodesLock sync.RWMutex
	authCodes     map[string]*OIDCAuthSession
}

type OIDCAuthSession struct {
	User      *model.WeKnoraUser
	ExpiresAt time.Time
}

func NewHandler(cfg *config.Config, wecomClient *wecom.Client, acClient *accountcenter.Client, syncer *syncer.Syncer, authSvc *auth.AuthService, storage *storage.Storage) *Handler {
	return &Handler{
		cfg:       cfg,
		wecom:     wecomClient,
		ac:        acClient,
		syncer:    syncer,
		auth:      authSvc,
		storage:   storage,
		authCodes: make(map[string]*OIDCAuthSession),
	}
}

// LoginRedirect 跳转到企微 OAuth2 授权页面 (支持微信/企微内嵌打开或桌面扫码)
func (h *Handler) LoginRedirect(c *gin.Context) {
	redirectURI := h.cfg.WeCom.RedirectURI
	if redirectURI == "" {
		proto := c.GetHeader("X-Forwarded-Proto")
		if proto == "" {
			if c.Request.TLS != nil {
				proto = "https"
			} else {
				proto = "http"
			}
		}
		host := c.GetHeader("X-Forwarded-Host")
		if host == "" {
			host = c.Request.Host
		}
		redirectURI = fmt.Sprintf("%s://%s/wecom/callback", proto, host)
	}

	state := c.Query("state")
	if state == "" {
		state = "weknora_auth"
	}

	ua := strings.ToLower(c.GetHeader("User-Agent"))
	isWeChatOrWeCom := strings.Contains(ua, "micromessenger") || strings.Contains(ua, "wxwork")

	var oauthURL string
	if !isWeChatOrWeCom && h.cfg.WeCom.AgentID > 0 {
		// PC 桌面端浏览器：企微官方扫码登录界面
		oauthURL = fmt.Sprintf(
			"https://login.work.weixin.qq.com/wwlogin/sso/login?login_type=CorpApp&appid=%s&agentid=%d&redirect_uri=%s&state=%s",
			url.QueryEscape(h.cfg.WeCom.CorpID),
			h.cfg.WeCom.AgentID,
			url.QueryEscape(redirectURI),
			url.QueryEscape(state),
		)
	} else {
		// 移动端微信/企业微信内嵌浏览器：OAuth2 静默授权免密直登
		oauthURL = fmt.Sprintf(
			"https://open.weixin.qq.com/connect/oauth2/authorize?appid=%s&redirect_uri=%s&response_type=code&scope=snsapi_base&state=%s#wechat_redirect",
			url.QueryEscape(h.cfg.WeCom.CorpID),
			url.QueryEscape(redirectURI),
			url.QueryEscape(state),
		)
	}

	c.Redirect(http.StatusFound, oauthURL)
}

// OAuthCallback 企微授权回调接口：
// 1. 如果带 echostr 参数：响应企业微信后台的“接收消息服务器配置” URL 有效性校验；
// 2. 如果带 code 参数：换取用户身份，颁发 WeKnora Token 并跳转回前端。
func (h *Handler) OAuthCallback(c *gin.Context) {
	ctx := c.Request.Context()

	// -------------------------------------------------------------
	// 场景 A: 企微后台保存配置时的 URL 验证 (带 echostr 参数)
	// -------------------------------------------------------------
	echostr := c.Query("echostr")
	if echostr != "" {
		msgSig := c.Query("msg_signature")
		timestamp := c.Query("timestamp")
		nonce := c.Query("nonce")

		token := h.cfg.WeCom.Token
		aesKey := h.cfg.WeCom.EncodingAESKey

		if token == "" || aesKey == "" {
			log.Printf("[WeCom] Token or EncodingAESKey not configured in config.yaml")
			c.String(http.StatusBadRequest, "Token or EncodingAESKey not configured")
			return
		}

		wxcpt, err := wecom.NewWXBizMsgCrypt(token, aesKey, h.cfg.WeCom.CorpID)
		if err != nil {
			log.Printf("[WeCom] Init WXBizMsgCrypt error: %v", err)
			c.String(http.StatusInternalServerError, "init crypt failed: %v", err)
			return
		}

		plainText, extractedCorpID, err := wxcpt.VerifyURL(msgSig, timestamp, nonce, echostr)
		if err != nil {
			log.Printf("[WeCom] VerifyURL error: %v (msgSig=%s, timestamp=%s, nonce=%s)", err, msgSig, timestamp, nonce)
			c.String(http.StatusBadRequest, "verify failed: %v", err)
			return
		}

		log.Printf("[WeCom] URL verification success! CorpID from WeCom: %s", extractedCorpID)
		if extractedCorpID != "" && (h.cfg.WeCom.CorpID == "" || strings.HasPrefix(h.cfg.WeCom.CorpID, "ww_")) {
			h.cfg.WeCom.CorpID = extractedCorpID
			h.wecom.UpdateCorpID(extractedCorpID)
			log.Printf("[WeCom] Automatically updated CorpID to: %s", extractedCorpID)
		}

		// 企微要求原样输出解密后的明文字符串
		c.String(http.StatusOK, plainText)
		return
	}

	// -------------------------------------------------------------
	// 场景 B: 员工登录回调 (带 code 参数)
	// -------------------------------------------------------------
	code := c.Query("code")
	if code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing code parameter from WeCom"})
		return
	}

	// 1. 调用企微接口，用 code 获取员工信息
	userInfo, err := h.wecom.GetUserInfoByCode(code)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": fmt.Sprintf("get wecom user info failed: %v", err)})
		return
	}

	if userInfo.UserID == "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "user does not belong to this enterprise (open_id without user_id)"})
		return
	}

	// 2. 从统一账户中心精确获取员工信息 (姓名、官方邮箱、手机号、部门编码)
	acUser, acErr := h.ac.GetUserByUnionNo(ctx, userInfo.UserID)
	if acErr != nil {
		log.Printf("[OAuth] Query account center failed for %s: %v, falling back to WeCom user/get", userInfo.UserID, acErr)
	}

	// 降级兜底：企微原生 user/get
	var detail *wecom.UserDetail
	if acUser == nil {
		detail, err = h.wecom.GetUserDetailByID(userInfo.UserID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("fetch user detail failed: %v", err)})
			return
		}
	} else {
		detail = &wecom.UserDetail{
			UserID:    acUser.UnionNo,
			Name:      acUser.UserName,
			Mobile:    acUser.Mobile,
			Telephone: acUser.Mobile,
			Email:     acUser.Email,
			BizMail:   acUser.Email,
			Status:    1,
		}
	}

	// 3. 递归向上解析员工对应的 WeKnora 租户空间部门 (level==2 或 fallback level==1)
	var targetDept *accountcenter.SysDepartment
	if acUser != nil && acUser.DepartmentCode != "" {
		targetDept, _ = h.syncer.FindTargetDepartmentForUser(ctx, acUser.DepartmentCode)
	}

	// 判断是否为该部门的主管
	isLeader := false
	if targetDept != nil && targetDept.ThirdManagerID != "" && acUser != nil {
		if targetDept.ThirdManagerID == acUser.ThirdUserID {
			isLeader = true
		}
	}

	// 4. 即时开户或增量更新员工信息（绑定对应的目标部门空间）
	if acUser != nil {
		if syncErr := h.syncer.SyncUserAndMemberships(ctx, acUser, targetDept, isLeader); syncErr != nil {
			log.Printf("[Syncer] SyncUserAndMemberships failed: %v", syncErr)
		}
	}

	phone := detail.Mobile
	if phone == "" {
		phone = detail.Telephone
	}
	email := detail.BizMail
	if email == "" {
		email = detail.Email
	}
	if email == "" && phone != "" {
		email = fmt.Sprintf("%s@benlai.com", phone)
	}
	if email == "" {
		email = fmt.Sprintf("%s@wecom.local", strings.ToLower(detail.UserID))
	}

	username := detail.Name
	if strings.TrimSpace(username) == "" {
		username = detail.UserID
	}

	user, err := h.storage.GetUserByEmailOrUsername(ctx, email, username)
	if err != nil || user == nil {
		user, _ = h.storage.GetUserByEmailOrUsername(ctx, fmt.Sprintf("%s@wecom.local", strings.ToLower(detail.UserID)), detail.UserID)
	}
	if user == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "user record provision failed, please contact admin"})
		return
	}

	// 4. 签发 WeKnora Token
	accessToken, refreshToken, err := h.auth.IssueWeKnoraTokens(ctx, user, user.TenantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("issue token failed: %v", err)})
		return
	}

	// 5. 设置登录 Cookie
	c.SetCookie("weknora_token", accessToken, 86400, "/", "", false, false)

	// 6. 构造 WeKnora 前端原生识别的 OIDC 回调 Base64 结构
	oidcPayload := map[string]interface{}{
		"success":       true,
		"token":         accessToken,
		"refresh_token": refreshToken,
		"user": map[string]interface{}{
			"id":                     user.ID,
			"username":               user.Username,
			"nickname":               user.Username,
			"email":                  user.Email,
			"avatar":                 user.Avatar,
			"tenant_id":              user.TenantID,
			"is_system_admin":        user.IsSystemAdmin,
			"can_access_all_tenants": user.CanAccessAllTenants,
		},
	}
	payloadBytes, _ := json.Marshal(oidcPayload)
	encodedPayload := base64.RawURLEncoding.EncodeToString(payloadBytes)

	// 7. 重定向回前端：支持原生 hash oidc_result 驱动登录态
	baseURL := strings.TrimRight(h.cfg.WeKnora.BaseURL, "/")
	targetURL := fmt.Sprintf("%s/#oidc_result=%s", baseURL, url.QueryEscape(encodedPayload))

	c.Redirect(http.StatusFound, targetURL)
}

// HandleEventCallback 接收企微事件与消息通知 (POST 请求)
func (h *Handler) HandleEventCallback(c *gin.Context) {
	// 企微规定：接收到消息后必须在 5 秒内返回 "success" 或 200 空内容
	c.String(http.StatusOK, "success")
}

// TriggerSync 手动触发一次企微部门与人员全量同步
func (h *Handler) TriggerSync(c *gin.Context) {
	go func() {
		_ = h.syncer.SyncAll(c.Request.Context())
	}()

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "sync triggered in background",
	})
}

// HealthCheck 健康检查
func (h *Handler) HealthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"time":   "ok",
	})
}

// OIDCAuthorize 标准 OIDC 授权入口 (WeKnora 前端发起)
func (h *Handler) OIDCAuthorize(c *gin.Context) {
	// 保存来自 WeKnora 的 redirect_uri 和 state，转入企微授权
	h.LoginRedirect(c)
}

// OIDCToken 标准 OIDC Token 换取端点
func (h *Handler) OIDCToken(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"access_token": uuid.New().String(),
		"token_type":   "Bearer",
		"expires_in":   86400,
	})
}

// OIDCUserInfo 标准 OIDC UserInfo 端点
func (h *Handler) OIDCUserInfo(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"sub":   "wecom_user",
		"name":  "企微员工",
		"email": "employee@benlai.com",
	})
}
