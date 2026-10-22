// Package kbridge 是 TASK-P09 的 knowledge 兼容桥（ADR-004 / DATA-010）。
//
// 桥把既有 WeKnora JWT/auth_tokens 约定封装在平台适配器内：只为「已验证且
// 有效的映射用户 + 当前允许空间」生成 ≤120s 的个人上游 token，永不签发
// 管理员代理 token，也不把上游凭据交给员工电脑。knowledge 仍是空间/资源
// 权限的最终裁定方（ADR-004）。
//
// 访问面收窄（部署侧配独立最小权限 DB 账号；本包接口只暴露以下表面）：
//   - WeKnora 主库：users / tenant_members 只读，auth_tokens 只写必要操作
//     （签发插入行 + 撤权删除该用户全部行）；
//   - 平台状态库：identity_links / managed_entitlements 只读（经
//     EntitlementChecker 语义），审计只追加。
//
// 签名密钥：上游 token 用 Bridge.Key（env PLATFORM_BRIDGE_KEY）签发，生产值
// 与 WeKnora JWTSecret 同源（knowledge ValidateToken 以同一 secret 验签），
// 但作为桥专属凭据独立注入，与 auth.key（平台会话）/ admin.key（管理面）/
// weknora.jwt_secret（配置项）分离。
//
// auth_tokens 行来源标识：该表无 source 列，且 knowledge ValidateToken 先
// 解析 JWT、再按 token 串精确查行，token 串无法加前缀而不破坏验签；因此桥
// 签发的行以平台审计事件关联识别（audit 记录 auth_tokens 行 id / 主体 /
// 空间 / 过期时间，见 AuditHook）。
package kbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"weknora-tokendance/pkg/authz"
	"weknora-tokendance/pkg/model"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// 桥拒绝语义（P09 完成条件：停用/无 entitlement/管理员拒绝/非法 TTL）。
var (
	// ErrTTLInvalid 表示请求的 ttl 不在 (0, 120s] 区间。
	ErrTTLInvalid = errors.New("kbridge: ttl must be within (0, 120s]")
	// ErrIdentityUnknown 表示 identityLinkID 在身份关联账本中不存在。
	ErrIdentityUnknown = errors.New("kbridge: identity link not found")
	// ErrIdentityDisabled 表示身份关联已停用（或上游用户被停用）。
	ErrIdentityDisabled = errors.New("kbridge: identity link or upstream user disabled")
	// ErrEntitlementMissing 表示该租户不在主体的 active entitlement 允许集内
	//（或上游 tenant_members 无 active 成员关系）。
	ErrEntitlementMissing = errors.New("kbridge: tenant not in active entitlement set")
	// ErrUpstreamUserMissing 表示映射的 knowledge 用户不存在。
	ErrUpstreamUserMissing = errors.New("kbridge: upstream knowledge user not found")
	// ErrAdminTokenRefused 表示映射用户是 is_system_admin，桥永不签管理员 token。
	ErrAdminTokenRefused = errors.New("kbridge: refusing to mint token for system admin")
)

// MaxUpstreamTokenTTL 是上游个人 token 的最长有效期（P09：≤120s）。
const MaxUpstreamTokenTTL = 120 * time.Second

// IdentityAccess 状态库身份关联只读表面（identity_links）+ 签发审计只读
// （租户级撤权按审计关联定位桥发行的 auth_tokens 行）。
type IdentityAccess interface {
	GetIdentityLink(ctx context.Context, id string) (*model.IdentityLink, error)
	ListIdentityLinksByImmutableSubject(ctx context.Context, subject string) ([]model.IdentityLink, error)
	ListAuditEventsByAction(ctx context.Context, action string) ([]model.AuditEvent, error)
}

// EntitlementGate 授权校验表面（生产注入 *authz.EntitlementChecker）。
type EntitlementGate interface {
	RequireEntitlement(ctx context.Context, subject, tenantID, capability string) (int64, error)
}

// WeKnoraAccess 收窄的 WeKnora 主库表面（P09）：
// users / tenant_members 只读 + auth_tokens 写必要操作（插入行 / 按用户删除 /
// 租户级撤权的按行读取与按主键删除）。
// 接口之外的主库写（知识内容、配置、其他表）桥一律不可达。
type WeKnoraAccess interface {
	GetUserByID(ctx context.Context, id string) (*model.WeKnoraUser, error)
	GetTenantMembership(ctx context.Context, userID string, tenantID uint64) (*model.WeKnoraTenantMember, error)
	CreateAuthToken(ctx context.Context, token *model.WeKnoraAuthToken) error
	DeleteAuthTokensByUser(ctx context.Context, knowledgeUserID string) (int64, error)
	ListAuthTokensByUser(ctx context.Context, knowledgeUserID string) ([]model.WeKnoraAuthToken, error)
	DeleteAuthTokensByIDs(ctx context.Context, ids []string) (int64, error)
}

// AuditHook 桥签发上游 token 后的审计回调（生产注入 storage.InsertAuditEvent；
// 用 audit 关联 auth_tokens 行作为来源标识，见包注释）。可为 nil（不审计）。
type AuditHook func(ctx context.Context, event *model.AuditEvent) error

// Config 桥配置。
type Config struct {
	// SignKey 上游 token 的 HS256 签名密钥（cfg.Bridge.Key）。
	SignKey string
	// Audit 签发审计回调，可为 nil。
	Audit AuditHook
	// Clock 可注入时钟（测试用）；nil 用系统时钟。
	Clock authz.Clock
}

// Bridge knowledge 兼容桥。同时满足 authz.TokenRevoker（撤权删除上游行）
// 与 authz.Channel（失效通知通道，按主体映射 knowledge 用户后删行）。
type Bridge struct {
	cfg     Config
	state   IdentityAccess
	gate    EntitlementGate
	weknora WeKnoraAccess
}

var (
	_ authz.TokenRevoker = (*Bridge)(nil)
	_ authz.Channel      = (*Bridge)(nil)
)

// New 组装兼容桥；state/gate/weknora 任一 nil 即 panic（启动期装配错误）。
func New(cfg Config, state IdentityAccess, gate EntitlementGate, weknora WeKnoraAccess) *Bridge {
	if cfg.SignKey == "" {
		panic("kbridge: sign key is required")
	}
	if state == nil || gate == nil || weknora == nil {
		panic("kbridge: state, gate and weknora access are required")
	}
	if cfg.Clock == nil {
		cfg.Clock = authz.RealClock{}
	}
	return &Bridge{cfg: cfg, state: state, gate: gate, weknora: weknora}
}

// IssueUpstreamToken 为 identityLinkID 指向的 active 映射用户、在 tenantID
// 空间签发 ≤120s 的上游个人 token（WeKnora JWT 约定：HS256 + claims
// user_id/email/tenant_id/type=access，与 pkg/auth/service.go 既有签发同
// claims 集），并插入 auth_tokens 行（WeKnora ValidateToken 校验行存在，
// 既有兼容语义）。签发前依次校验：
//   - ttl ∈ (0, 120s]；
//   - identity_links 行存在且 active；
//   - authz.RequireEntitlement(subject, tenant, 通配) 通过（tenant 必须在
//     该用户 active entitlement 允许集内）；
//   - 上游 users 行存在、is_active=true 且非 is_system_admin；
//   - 上游 tenant_members 存在 active 成员关系。
//
// 返回的 token 可直接作 knowledge 个人 REST 的 Bearer（24h 体系同源，但仅短寿）。
func (b *Bridge) IssueUpstreamToken(ctx context.Context, identityLinkID, tenantID string, ttl time.Duration) (string, error) {
	if ttl <= 0 || ttl > MaxUpstreamTokenTTL {
		return "", fmt.Errorf("%w: got %s", ErrTTLInvalid, ttl)
	}
	link, err := b.state.GetIdentityLink(ctx, identityLinkID)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrIdentityUnknown, err)
	}
	if link == nil || link.Status != "active" {
		return "", ErrIdentityDisabled
	}

	if _, err := b.gate.RequireEntitlement(ctx, link.ImmutableSubject, tenantID, ""); err != nil {
		return "", mapEntitlementError(err)
	}

	tenantNumeric, err := strconv.ParseUint(tenantID, 10, 64)
	if err != nil {
		return "", fmt.Errorf("%w: tenant_id %q is not numeric", ErrEntitlementMissing, tenantID)
	}

	user, err := b.weknora.GetUserByID(ctx, link.KnowledgeUserID)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUpstreamUserMissing, err)
	}
	if user.IsSystemAdmin {
		return "", ErrAdminTokenRefused
	}
	if !user.IsActive {
		return "", ErrIdentityDisabled
	}

	member, err := b.weknora.GetTenantMembership(ctx, user.ID, tenantNumeric)
	if err != nil || member == nil || member.Status != "active" {
		return "", ErrEntitlementMissing
	}

	now := b.cfg.Clock.Now()
	claims := jwt.MapClaims{
		"user_id":   user.ID,
		"email":     user.Email,
		"tenant_id": tenantNumeric,
		"exp":       now.Add(ttl).Unix(),
		"iat":       now.Unix(),
		"type":      "access",
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(b.cfg.SignKey))
	if err != nil {
		return "", fmt.Errorf("kbridge: sign upstream token: %w", err)
	}

	// auth_tokens 行：token 串必须逐字可被查回（WeKnora 按串精确查找），
	// 行来源经审计事件关联识别，见包注释。
	record := &model.WeKnoraAuthToken{
		ID:        uuid.New().String(),
		UserID:    user.ID,
		Token:     token,
		TokenType: "access_token",
		ExpiresAt: now.Add(ttl),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := b.weknora.CreateAuthToken(ctx, record); err != nil {
		return "", fmt.Errorf("kbridge: persist auth token row: %w", err)
	}

	if b.cfg.Audit != nil {
		_ = b.cfg.Audit(ctx, &model.AuditEvent{
			ID:     uuid.New().String(),
			Actor:  link.ImmutableSubject,
			Action: "kbridge.token.issued",
			Target: "knowledge upstream token",
			After: auditJSON(map[string]interface{}{
				"result":            "allowed",
				"auth_token_id":     record.ID,
				"knowledge_user_id": user.ID,
				"identity_link_id":  link.ID,
				"tenant_id":         tenantID,
				"expires_at":        record.ExpiresAt.Format(time.RFC3339),
			}),
			RequestID: "",
		})
	}

	return token, nil
}

// RevokeUpstreamTokens 删除 knowledge 用户在上游 auth_tokens 的全部行
// （既有兼容语义：行删除即 ValidateToken 拒绝）。幂等；实现 authz.TokenRevoker。
func (b *Bridge) RevokeUpstreamTokens(ctx context.Context, knowledgeUserID string) (int64, error) {
	if knowledgeUserID == "" {
		return 0, nil
	}
	return b.weknora.DeleteAuthTokensByUser(ctx, knowledgeUserID)
}

// RevokeTokens authz.TokenRevoker 适配（撤权流程零改动替换既有直写实现）。
func (b *Bridge) RevokeTokens(ctx context.Context, knowledgeUserID string) (int64, error) {
	return b.RevokeUpstreamTokens(ctx, knowledgeUserID)
}

// RevokeUpstreamTokensForTenant 租户级撤权（L7）：只删除该 (knowledgeUser, tenant)
// 的上游 auth_tokens 行，不清空用户其余空间的 token。行选择规则：
//   - token claims 可解析（桥密钥验签）：tenant_id 匹配才删；
//   - claims 无法解析或缺 tenant_id（非桥签发/异密钥的历史行）：只删「签发审计
//     （kbridge.token.issued）可定位且 ttl≤120s」的桥发行行。
//
// 幂等；实现 authz.TenantTokenRevoker。
func (b *Bridge) RevokeUpstreamTokensForTenant(ctx context.Context, knowledgeUserID, tenantID string) (int64, error) {
	if knowledgeUserID == "" || tenantID == "" {
		return 0, nil
	}
	tenantNumeric, err := strconv.ParseUint(tenantID, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("kbridge: tenant_id %q is not numeric: %w", tenantID, err)
	}
	rows, err := b.weknora.ListAuthTokensByUser(ctx, knowledgeUserID)
	if err != nil {
		return 0, fmt.Errorf("kbridge: list auth tokens: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}

	// 审计关联集：该用户在该 tenant 经桥签发的 auth_tokens 行 id
	audited := map[string]bool{}
	events, aerr := b.state.ListAuditEventsByAction(ctx, "kbridge.token.issued")
	if aerr != nil {
		return 0, fmt.Errorf("kbridge: list issue audits: %w", aerr)
	}
	for _, ev := range events {
		var detail struct {
			AuthTokenID     string `json:"auth_token_id"`
			KnowledgeUserID string `json:"knowledge_user_id"`
			TenantID        string `json:"tenant_id"`
		}
		if json.Unmarshal([]byte(ev.After), &detail) == nil &&
			detail.AuthTokenID != "" && detail.KnowledgeUserID == knowledgeUserID && detail.TenantID == tenantID {
			audited[detail.AuthTokenID] = true
		}
	}

	var ids []string
	for _, row := range rows {
		claims, perr := parseUpstreamClaims(b.cfg.SignKey, row.Token)
		if perr == nil {
			if tid, ok := claims["tenant_id"].(float64); ok {
				if uint64(tid) == tenantNumeric {
					ids = append(ids, row.ID)
				}
				continue // claims 可验签且 tenant_id 匹配即删；桥与原生登录 token 同密钥同构
				// 无法区分来源——该租户授权已撤，连带删除其原生会话属 fail-closed 方向
				// 的可辩护语义（审查 N4）：受影响用户重新登录原生 UI 即可恢复。
			}
		}
		// 无法区分：只删审计可定位的桥发行短寿行
		if audited[row.ID] && row.ExpiresAt.Sub(row.CreatedAt) <= MaxUpstreamTokenTTL {
			ids = append(ids, row.ID)
		}
	}
	return b.weknora.DeleteAuthTokensByIDs(ctx, ids)
}

// RevokeTokensForTenant authz.TenantTokenRevoker 适配。
func (b *Bridge) RevokeTokensForTenant(ctx context.Context, knowledgeUserID, tenantID string) (int64, error) {
	return b.RevokeUpstreamTokensForTenant(ctx, knowledgeUserID, tenantID)
}

// parseUpstreamClaims 用桥签名密钥解析上游 token claims（不校验有效期：
// 撤权需要覆盖已过期但未清理的行）。
func parseUpstreamClaims(signKey, raw string) (jwt.MapClaims, error) {
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(signKey), nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithoutClaimsValidation())
	if err != nil {
		return nil, err
	}
	return claims, nil
}

// Name authz.Channel 名称（Notifier 注册用）。
func (b *Bridge) Name() string { return "kbridge" }

// Notify authz.Channel 实现：失效事件按主体映射 knowledge 用户后删除上游行；
// 删除幂等，部分失败聚合返回错误进入重试队列（通知失败不影响已生效拒绝）。
func (b *Bridge) Notify(ctx context.Context, ev authz.InvalidateEvent) error {
	links, err := b.state.ListIdentityLinksByImmutableSubject(ctx, ev.Subject)
	if err != nil {
		return fmt.Errorf("kbridge: list identity links for notify: %w", err)
	}
	var lastErr error
	for _, link := range links {
		if link.KnowledgeUserID == "" {
			continue
		}
		if _, err := b.weknora.DeleteAuthTokensByUser(ctx, link.KnowledgeUserID); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// mapEntitlementError 把 authz 校验错误映射到桥拒绝语义。
func mapEntitlementError(err error) error {
	switch {
	case errors.Is(err, authz.ErrIdentityDisabled):
		return ErrIdentityDisabled
	case errors.Is(err, authz.ErrEntitlementMissing):
		return ErrEntitlementMissing
	default:
		return fmt.Errorf("kbridge: entitlement check failed: %w", err)
	}
}
