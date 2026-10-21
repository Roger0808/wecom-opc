package authz

import (
	"context"
)

// TokenRevoker 上游 token 撤销钩子（REQ-003）：按 subject 的 knowledge user 撤销上游已签发 token。
// P09 起由 pkg/kbridge.Bridge 实现（隔离的 knowledge 兼容桥）；NoopTokenRevoker 供测试/未装配场景。
type TokenRevoker interface {
	// RevokeTokens 撤销指定 knowledge user 的上游 token，返回删除/撤销条数。
	// 实现必须幂等；错误由调用方记审计，不阻断撤权主流程。
	RevokeTokens(ctx context.Context, knowledgeUserID string) (int64, error)
}

// NoopTokenRevoker 空实现：不触碰上游 token（测试或纯平台部署用）。
type NoopTokenRevoker struct{}

func (NoopTokenRevoker) RevokeTokens(ctx context.Context, knowledgeUserID string) (int64, error) {
	return 0, nil
}

// TenantTokenRevoker 可选扩展（L7）：tenant 级撤权只删该 (user, tenant) 的上游
// auth_tokens 行，不清空用户其余空间的 token。实现方按 token claims tenant 过滤；
// 无法区分来源的行只按签发审计关联删除桥发行的短寿行。
type TenantTokenRevoker interface {
	// RevokeTokensForTenant 撤销指定 knowledge user 在指定 tenant 的上游 token。
	// 实现必须幂等；错误由调用方记审计，不阻断撤权主流程。
	RevokeTokensForTenant(ctx context.Context, knowledgeUserID, tenantID string) (int64, error)
}
