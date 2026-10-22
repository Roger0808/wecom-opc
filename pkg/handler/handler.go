package handler

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/accountcenter"
	"weknora-tokendance/pkg/auth"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"
	"weknora-tokendance/pkg/syncer"
	"weknora-tokendance/pkg/syncservice"
	"weknora-tokendance/pkg/wecom"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// wecomUserAPI 旧登录回调依赖的企微用户查询表面（*wecom.Client 满足；测试注入假企微）。
type wecomUserAPI interface {
	GetUserInfoByCode(code string) (*wecom.OAuthUserResponse, error)
	GetUserDetailByID(userID string) (*wecom.UserDetail, error)
}

type Handler struct {
	cfg     *config.Config
	wecom   wecomUserAPI
	ac      *accountcenter.Client
	syncer  *syncer.Syncer
	auth    *auth.AuthService
	storage *storage.Storage

	// 手动同步服务（TASK-P08）：/admin/api/sync/all 触发全量同步的唯一入口（走账本/锁/快照契约）
	syncService *syncservice.Service

	// 临时授权码缓存 (用于 OIDC /token 兑换)
	authCodesLock sync.RWMutex
	authCodes     map[string]*OIDCAuthSession
}

type OIDCAuthSession struct {
	User      *model.WeKnoraUser
	ExpiresAt time.Time
}

func NewHandler(cfg *config.Config, wecomClient wecomUserAPI, acClient *accountcenter.Client, syncer *syncer.Syncer, authSvc *auth.AuthService, storage *storage.Storage) *Handler {
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

// SetSyncService 注入手动同步服务（/admin/api/sync/all 触发全量同步走账本/锁/快照契约）。
func (h *Handler) SetSyncService(svc *syncservice.Service) { h.syncService = svc }

// LoginRedirect 跳转到企微 OAuth2 授权页面 (支持微信/企微内嵌打开或桌面扫码)。
// redirect_uri 查询参数仅对集成登录流程开放：必须与配置的对外规范地址
// （server.external_base_url）同源且路径为 /integration/v1/auth/callback，
// 防止授权码被劫持到外站；同源判定与缺省回跳拼接都不信请求 Host /
// X-Forwarded-Host（可伪造）。缺省保持既有 /wecom/callback 行为不变。
func (h *Handler) LoginRedirect(c *gin.Context) {
	redirectURI := h.cfg.WeCom.RedirectURI
	if override := c.Query("redirect_uri"); override != "" {
		u, err := url.Parse(override)
		base, berr := url.Parse(h.externalBaseURL())
		// 集成登录回跳：与对外规范地址同源且固定路径。
		integrationOK := err == nil && berr == nil && u.Scheme == base.Scheme && u.Host == base.Host &&
			u.Path == "/integration/v1/auth/callback"
		// 既有 knowledge UI 企微登录回跳：与配置的 WeKnora 规范地址同源且固定路径
		//（部署实测：H2 同源加固误伤了该既有流程，此处显式放行受信源）。
		legacyOK := false
		if err == nil {
			wbase, wberr := url.Parse(h.cfg.WeKnora.BaseURL)
			legacyOK = wberr == nil && wbase.Scheme != "" && u.Scheme == wbase.Scheme && u.Host == wbase.Host &&
				u.Path == "/api/v1/auth/oidc/callback"
		}
		if !integrationOK && !legacyOK {
			c.JSON(http.StatusBadRequest, gin.H{"error": "redirect_uri must be same-origin (server.external_base_url) /integration/v1/auth/callback or weknora.base_url /api/v1/auth/oidc/callback"})
			return
		}
		redirectURI = override
	}
	if redirectURI == "" {
		base := h.externalBaseURL()
		if base == "" {
			// 未配置对外规范地址时不再回退到请求 Host（防伪造）：明确服务端配置错误
			c.JSON(http.StatusInternalServerError, gin.H{"error": "server.external_base_url is not configured"})
			return
		}
		redirectURI = base + "/wecom/callback"
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

// externalBaseURL 返回配置的本服务对外规范地址（server.external_base_url，去尾部斜杠）。
// 回跳地址拼接只信配置，不信请求 Host / X-Forwarded-Host（H2）。
func (h *Handler) externalBaseURL() string {
	if h.cfg == nil {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(h.cfg.Server.ExternalBaseURL), "/")
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

		// 企微要求原样输出解密后的明文字符串。
		// 安全基线（BND-170）：禁止使用回调内 CorpID 覆写运行时配置，
		// CorpID 只能来自配置文件/环境变量，防止回调被伪造时配置被劫持。
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
	// INT-130：登录路径不再隐式触发任何同步（kickDepartmentSync 已移除）；
	// 部门空间未供给（映射表未命中）→ 403 SUBJECT_NOT_PROVISIONED，提示管理员处理。
	var targetDept *accountcenter.SysDepartment
	if acUser != nil && acUser.DepartmentCode != "" {
		targetDept, _ = h.syncer.FindTargetDepartmentForUser(ctx, acUser.DepartmentCode)
		if targetDept == nil {
			log.Printf("[OAuth] subject %s department %s not provisioned (no tenant mapping), denying login", acUser.ID, acUser.DepartmentCode)
			c.JSON(http.StatusForbidden, gin.H{"error": gin.H{
				"code":    "SUBJECT_NOT_PROVISIONED",
				"message": "department space not provisioned, please contact administrator to run a sync",
			}})
			return
		}
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
		if _, syncErr := h.syncer.SyncUserAndMemberships(ctx, acUser, targetDept, isLeader); syncErr != nil {
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

// HandleEventCallback 接收企微事件与消息通知 (POST 请求)。
// 安全基线（BND-170）：必须使用 WXBizMsgCrypt 验签 + 解密；
// 验签失败返回 403。本任务不实现事件驱动同步（NG-007），
// 验签通过的消息体只写审计留痕，随后按企微要求返回 "success"。
func (h *Handler) HandleEventCallback(c *gin.Context) {
	token := h.cfg.WeCom.Token
	aesKey := h.cfg.WeCom.EncodingAESKey
	if token == "" || aesKey == "" {
		log.Printf("[WeCom] Token or EncodingAESKey not configured, refusing event callback")
		h.auditWecomCallback(c, "wecom.callback.denied", "", "token or encoding_aes_key not configured")
		c.String(http.StatusBadRequest, "Token or EncodingAESKey not configured")
		return
	}

	wxcpt, err := wecom.NewWXBizMsgCrypt(token, aesKey, h.cfg.WeCom.CorpID)
	if err != nil {
		log.Printf("[WeCom] Init WXBizMsgCrypt error: %v", err)
		c.String(http.StatusInternalServerError, "init crypt failed")
		return
	}

	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.String(http.StatusBadRequest, "read body failed")
		return
	}

	// 企微回调签名校验对象是 XML 中 <Encrypt> 节点的密文内容
	encryptMsg := extractEncryptCDATA(bodyBytes)
	if encryptMsg == "" {
		log.Printf("[WeCom] Event callback missing Encrypt content")
		h.auditWecomCallback(c, "wecom.callback.denied", "", "missing Encrypt content")
		c.String(http.StatusForbidden, "verify failed")
		return
	}

	msgSig := c.Query("msg_signature")
	timestamp := c.Query("timestamp")
	nonce := c.Query("nonce")

	plainMsg, corpID, err := wxcpt.DecryptMsg(msgSig, timestamp, nonce, encryptMsg)
	if err != nil {
		log.Printf("[WeCom] Event callback verify/decrypt failed: %v", err)
		h.auditWecomCallback(c, "wecom.callback.denied", "", "verify failed: "+err.Error())
		c.String(http.StatusForbidden, "verify failed")
		return
	}

	// 事件驱动同步不在本任务范围（NG-007），解密后的消息体先只记审计
	log.Printf("[WeCom] Event callback verified (corp=%s), %d bytes message audited only", corpID, len(plainMsg))
	h.auditWecomCallback(c, "wecom.event.received", corpID, string(plainMsg))

	// 企微规定：接收到消息后必须在 5 秒内返回 "success" 或 200 空内容
	c.String(http.StatusOK, "success")
}

var encryptCDATARe = regexp.MustCompile(`(?s)<Encrypt><!\[CDATA\[(.*?)\]\]></Encrypt>`)

// extractEncryptCDATA 从企微回调 XML body 中提取 <Encrypt> 节点的 CDATA 内容
func extractEncryptCDATA(body []byte) string {
	m := encryptCDATARe.FindSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return string(m[1])
}

// auditWecomCallback 企微回调审计落库（best-effort，actor 记 wecom / anonymous）
func (h *Handler) auditWecomCallback(c *gin.Context, action, corpID, detail string) {
	if h.storage == nil {
		return
	}
	actor := "wecom"
	if corpID == "" {
		actor = "wecom:anonymous"
	}
	event := &model.AuditEvent{
		Actor:     actor,
		Action:    action,
		Target:    c.Request.Method + " " + c.FullPath(),
		After:     detail,
		RequestID: c.GetHeader("X-Request-ID"),
	}
	if event.RequestID == "" {
		event.RequestID = uuid.New().String()
	}
	if err := h.storage.InsertAuditEvent(c.Request.Context(), event); err != nil {
		log.Printf("[WeCom] failed to write audit event %s: %v", action, err)
	}
}

// TriggerSync 旧的手动全量同步触发端点（BND-130/INT-130 禁止条款）：
// 隐式/无鉴权的全量同步已移除，统一走 /integration/v1/admin/sync/runs（RequireAdmin + CSRF）。
// 路径保留并返回 410 Gone，避免旧调用方误以为同步已触发。
func (h *Handler) TriggerSync(c *gin.Context) {
	c.JSON(http.StatusGone, gin.H{
		"code":    -1,
		"message": "gone: use POST /integration/v1/admin/sync/runs (manual sync service)",
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
