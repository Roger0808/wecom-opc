// Package adminauth 实现管理面 (/admin/api) 的服务内认证、授权范围与 CSRF 防护（BND-170/171, REQ-005）。
//
// 管理 token 为 HS256 JWT，密钥来自配置项 Admin.Key（env PLATFORM_ADMIN_KEY），
// 与 WeKnora 用户态 JWTSecret 完全分离。claims: sub / role / tenant_id。
//
// 授权范围：
//   - system_admin：放行全部管理端点；
//   - dept_admin：仅允许 tenant_id 与请求路径 :id 参数匹配的空间级端点
//     （覆盖 /admin/api/spaces/:id/* 与 sync space 类端点；后续 INT-130/INT-141
//     新增空间级路由时按路由组复用本中间件即可）。
//
// CSRF/来源防护（仅对非 GET/HEAD/OPTIONS 生效）：
//   - 请求携带 Origin 或 Referer 头时，必须命中 Admin.AllowedOrigins 白名单，否则 403；
//   - 未携带来源头的非浏览器 API 调用放行，但写入一条审计记录。
//
// 审计：所有管理端点的放行与拒绝都会追加一行审计（拒绝时 actor 记 anonymous 或尝试身份）。
package adminauth

import (
	"context"
	"crypto/subtle"
	"errors"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"weknora-tokendance/pkg/model"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Role 管理端角色枚举
type Role string

const (
	RoleSystemAdmin Role = "system_admin"
	RoleDeptAdmin   Role = "dept_admin"
)

// ContextKeyClaims 保存解析后的管理身份 claims 的 gin context key
const ContextKeyClaims = "adminauth.claims"

// Claims 管理面 JWT claims
type Claims struct {
	Role     Role   `json:"role"`
	TenantID uint64 `json:"tenant_id,omitempty"`
	jwt.RegisteredClaims
}

// AuditFunc 追加写审计事件的回调（由 storage 层注入；nil 表示不审计）
type AuditFunc func(ctx context.Context, event *model.AuditEvent) error

// MiddlewareConfig RequireAdmin 配置
type MiddlewareConfig struct {
	// Secret 管理 JWT HS256 密钥（必填）
	Secret []byte
	// AllowedOrigins 允许携带浏览器 Origin/Referer 的来源白名单。
	// 条目格式："https://admin.example.com"、"https://*.example.com"、"admin.example.com:8080"。
	// 空列表表示拒绝一切带来源头的管理写请求（无来源头的 API 调用仍放行，但记审计）。
	AllowedOrigins []string
	// Audit 审计写入回调（可为 nil）
	Audit AuditFunc
}

// IssueToken 签发管理面 token。dept_admin 必须携带非零 tenantID。
func IssueToken(secret []byte, subject string, role Role, tenantID uint64, ttl time.Duration) (string, error) {
	if len(secret) == 0 {
		return "", fmt.Errorf("admin JWT secret is empty")
	}
	if role == RoleDeptAdmin && tenantID == 0 {
		return "", fmt.Errorf("tenant_id is required for dept_admin")
	}
	now := time.Now()
	claims := Claims{
		Role:     role,
		TenantID: tenantID,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject,
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(now),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
}

// ParseToken 校验管理 token 的 HS256 签名与过期时间，并做角色/租户声明的基本校验。
func ParseToken(secret []byte, raw string) (*Claims, error) {
	if len(secret) == 0 {
		return nil, fmt.Errorf("admin JWT secret is empty")
	}
	var claims Claims
	token, err := jwt.ParseWithClaims(raw, &claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, fmt.Errorf("invalid token")
	}
	switch claims.Role {
	case RoleSystemAdmin:
	case RoleDeptAdmin:
		if claims.TenantID == 0 {
			return nil, fmt.Errorf("dept_admin token missing tenant_id")
		}
	default:
		return nil, fmt.Errorf("unknown admin role: %q", claims.Role)
	}
	if claims.Subject == "" {
		return nil, fmt.Errorf("token missing sub")
	}
	return &claims, nil
}

// RequireAdmin 管理面认证 + 授权范围 + CSRF 中间件。
// 按路由组挂载即可对整组生效（当前挂载于 /admin/api 全组，INT-130/INT-141 复用同一中间件）。
func RequireAdmin(cfg MiddlewareConfig) gin.HandlerFunc {
	return requireAdmin(cfg, true)
}

// RequireAdminAuth 仅认证 + CSRF（不做 dept_admin 的 :id 租户范围检查），
// 供 INT-130 /integration/v1/admin 等路由组使用：这些端点的授权范围
// 由处理器按 claims 自行判定（如 sync runs 的 scope=department 租户归属校验）。
// 审计行为与 RequireAdmin 完全一致。
func RequireAdminAuth(cfg MiddlewareConfig) gin.HandlerFunc {
	return requireAdmin(cfg, false)
}

func requireAdmin(cfg MiddlewareConfig, tenantScopeCheck bool) gin.HandlerFunc {
	allowed := normalizeOrigins(cfg.AllowedOrigins)
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		// 1. 认证：Bearer token 校验（签名 + 过期 + 角色声明）
		raw := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
		var claims *Claims
		var authErr error
		if raw == "" {
			authErr = fmt.Errorf("missing bearer token")
		} else {
			claims, authErr = ParseToken(cfg.Secret, raw)
		}
		if authErr != nil {
			actor := "anonymous"
			if claims != nil && claims.Subject != "" {
				actor = claims.Subject
			}
			recordAudit(cfg.Audit, ctx, &model.AuditEvent{
				Actor:     actor,
				Action:    "admin.auth.denied",
				Target:    c.Request.Method + " " + c.FullPath(),
				After:     auditDetail("unauthorized", authErr.Error(), nil),
				RequestID: requestID(c),
			})
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": -1, "message": "unauthorized"})
			return
		}

		// 2. 来源/CSRF 校验：仅对非 GET/HEAD/OPTIONS 生效
		if origin := requestOrigin(c); origin != "" {
			if !originAllowed(origin, allowed) {
				recordAudit(cfg.Audit, ctx, &model.AuditEvent{
					Actor:     claims.Subject,
					Action:    "admin.csrf.denied",
					Target:    c.Request.Method + " " + c.FullPath(),
					After:     auditDetail("forbidden", "origin not allowed: "+origin, claims),
					RequestID: requestID(c),
				})
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"code": -1, "message": "origin not allowed"})
				return
			}
		} else {
			// 无来源头：视为非浏览器 API 调用，放行但记审计
			recordAudit(cfg.Audit, ctx, &model.AuditEvent{
				Actor:     claims.Subject,
				Action:    "admin.csrf.no_origin",
				Target:    c.Request.Method + " " + c.FullPath(),
				After:     auditDetail("allowed", "no Origin/Referer header", claims),
				RequestID: requestID(c),
			})
		}

		// 3. 授权范围：dept_admin 仅限本空间（路径 :id 参数）端点
		if tenantScopeCheck && claims.Role == RoleDeptAdmin {
			idStr := c.Param("id")
			tenantID, err := strconv.ParseUint(idStr, 10, 64)
			if err != nil || tenantID != claims.TenantID {
				recordAudit(cfg.Audit, ctx, &model.AuditEvent{
					Actor:     claims.Subject,
					Action:    "admin.authz.denied",
					Target:    c.Request.Method + " " + c.FullPath(),
					After:     auditDetail("forbidden", "dept_admin tenant scope mismatch", claims),
					RequestID: requestID(c),
				})
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"code": -1, "message": "forbidden: tenant scope mismatch"})
				return
			}
		}

		recordAudit(cfg.Audit, ctx, &model.AuditEvent{
			Actor:     claims.Subject,
			Action:    "admin.auth.allowed",
			Target:    c.Request.Method + " " + c.FullPath(),
			After:     auditDetail("allowed", "", claims),
			RequestID: requestID(c),
		})

		c.Set(ContextKeyClaims, claims)
		c.Next()
	}
}

// GetClaims 从 gin context 读取管理身份（RequireAdmin 之后可用）
func GetClaims(c *gin.Context) (*Claims, bool) {
	v, ok := c.Get(ContextKeyClaims)
	if !ok {
		return nil, false
	}
	claims, ok := v.(*Claims)
	return claims, ok
}

type normalizedOrigin struct {
	scheme   string // 空串表示任意 scheme
	host     string
	hasPort  bool
	port     string
	wildcard bool // host 为 *.suffix 形式
}

func normalizeOrigins(entries []string) []normalizedOrigin {
	out := make([]normalizedOrigin, 0, len(entries))
	for _, e := range entries {
		e = strings.TrimSpace(strings.ToLower(e))
		if e == "" {
			continue
		}
		var no normalizedOrigin
		if u, err := url.Parse(e); err == nil && u.Hostname() != "" {
			no.scheme = u.Scheme
			no.host = strings.TrimSuffix(u.Hostname(), ".")
			no.hasPort = u.Port() != ""
			no.port = u.Port()
		} else {
			host := e
			if h, p, err := netSplitHostPort(e); err == nil {
				host, no.hasPort, no.port = h, true, p
			}
			no.host = strings.TrimSuffix(strings.Trim(host, "/"), ".")
		}
		if strings.HasPrefix(no.host, "*.") {
			no.wildcard = true
		}
		if no.host != "" {
			out = append(out, no)
		}
	}
	return out
}

func netSplitHostPort(s string) (string, string, error) {
	i := strings.LastIndex(s, ":")
	if i <= 0 || i == len(s)-1 {
		return s, "", fmt.Errorf("no port")
	}
	for _, r := range s[i+1:] {
		if r < '0' || r > '9' {
			return s, "", fmt.Errorf("invalid port")
		}
	}
	return s[:i], s[i+1:], nil
}

func originAllowed(origin string, allowed []normalizedOrigin) bool {
	u, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || u.Hostname() == "" {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	port := u.Port()
	for _, a := range allowed {
		if a.scheme != "" && a.scheme != strings.ToLower(u.Scheme) {
			continue
		}
		if a.hasPort && a.port != port {
			continue
		}
		if a.wildcard {
			base := strings.TrimPrefix(a.host, "*.")
			if host == base || strings.HasSuffix(host, "."+base) {
				return true
			}
			continue
		}
		if host == a.host {
			return true
		}
	}
	return false
}

func requestOrigin(c *gin.Context) string {
	if origin := c.GetHeader("Origin"); origin != "" {
		return origin
	}
	return c.GetHeader("Referer")
}

func requestID(c *gin.Context) string {
	if id := c.GetHeader("X-Request-ID"); id != "" {
		return id
	}
	return uuid.New().String()
}

func auditDetail(result, reason string, claims *Claims) string {
	detail := map[string]interface{}{
		"result": result,
	}
	if reason != "" {
		detail["reason"] = reason
	}
	if claims != nil {
		detail["role"] = string(claims.Role)
		detail["tenant_id"] = claims.TenantID
	}
	b, _ := json.Marshal(detail)
	return string(b)
}

func recordAudit(fn AuditFunc, ctx context.Context, event *model.AuditEvent) {
	if fn == nil {
		return
	}
	if err := fn(ctx, event); err != nil {
		log.Printf("[AdminAuth] failed to write audit event %s: %v", event.Action, err)
	}
}

// ParseServiceCredential 解析服务身份凭据：优先按 HS256 JWT（Admin.Key 签发、
// system_admin 角色）；不匹配且配置了共享服务令牌时，退化为常量时间比对并
// 合成等价 claims。覆盖 code 后端持原始 PLATFORM_SERVICE_TOKEN 的调用形态
//（联调实测：code 发送共享令牌而非 JWT，旧实现一律按 JWT 解析而拒绝）。
func ParseServiceCredential(adminKey, sharedToken, raw string) (*Claims, error) {
	if claims, err := ParseToken([]byte(adminKey), raw); err == nil {
		return claims, nil
	}
	if sharedToken != "" && subtle.ConstantTimeCompare([]byte(raw), []byte(sharedToken)) == 1 {
		return &Claims{Role: RoleSystemAdmin, RegisteredClaims: jwt.RegisteredClaims{Subject: "service:platform-shared"}}, nil
	}
	return nil, errors.New("invalid service token")
}
