package authz

import (
	"context"
	"errors"

	"weknora-tokendance/pkg/storage"
)

// Entitlement 校验错误（P06.2 RequireEntitlement）。
var (
	// ErrIdentityDisabled 表示该 subject 没有任何 active 身份关联（停用/未供给）。
	ErrIdentityDisabled = errors.New("authz: identity link disabled")
	// ErrEntitlementMissing 表示 (subject, tenant) 下没有所需 capability 的 active 授权项。
	ErrEntitlementMissing = errors.New("authz: entitlement missing")
)

// EntitlementChecker 校验辅助：identity active + capability 在 active entitlements 内。
// C01 接入后同一条校验覆盖 JWT/PAT/WS/task token 入口；当前由 internal 接口与租约续期调用。
type EntitlementChecker struct {
	storage  *storage.Storage
	versions *VersionSource
}

// NewEntitlementChecker 构造校验辅助；versions 可为 nil（校验不再返回版本，调用方自行读取）。
func NewEntitlementChecker(s *storage.Storage, versions *VersionSource) *EntitlementChecker {
	return &EntitlementChecker{storage: s, versions: versions}
}

// RequireEntitlement 校验 (subject, tenant, capability)：
//   - subject 无任何 active identity_link → ErrIdentityDisabled；
//   - capability 不在 (subject, tenant) 的 active entitlements 内 → ErrEntitlementMissing
//     （capability 传空串表示「该 tenant 下任意 capability 即可」）；
//   - 通过时返回当前授权版本（versions 为 nil 时返回 0）。
func (e *EntitlementChecker) RequireEntitlement(ctx context.Context, subject, tenantID, capability string) (int64, error) {
	links, err := e.storage.ListIdentityLinksByImmutableSubject(ctx, subject)
	if err != nil {
		return 0, err
	}
	active := false
	for _, l := range links {
		if l.Status == "active" {
			active = true
			break
		}
	}
	if !active {
		return 0, ErrIdentityDisabled
	}

	ents, err := e.storage.ListActiveManagedEntitlementsBySubjectTenant(ctx, subject, tenantID)
	if err != nil {
		return 0, err
	}
	found := capability == "" && len(ents) > 0
	for _, ent := range ents {
		if ent.Capability == capability {
			found = true
			break
		}
	}
	if !found {
		return 0, ErrEntitlementMissing
	}

	if e.versions == nil {
		return 0, nil
	}
	return e.versions.Current(ctx, subject)
}
