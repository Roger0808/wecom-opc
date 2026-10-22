package storage

import (
	"context"
	"errors"
	"time"

	"weknora-tokendance/pkg/model"

	"gorm.io/gorm"
)

// 本文件承载 TASK-P05/P06 的授权版本源（authz_epochs）与撤权事务助手。
// 版本 bump 用「INSERT ... ON CONFLICT DO NOTHING + 单条 UPDATE version=version+1」实现，
// UPDATE 的原子性保证并发 bump 不丢版本（PostgreSQL 行锁串行化，sqlite 单写者串行化）。
// 撤权相关 *Tx 变体供 handler 在同一个事务里组合：先持久化、后通知（BND-110）。

// CurrentAuthzVersion 读取 subject 的当前授权版本；无纪元行时返回 0（从未变更）。
func (s *Storage) CurrentAuthzVersion(ctx context.Context, subject string) (int64, error) {
	if s.stateDB == nil {
		return 0, errStateDBNotConfigured
	}
	return currentAuthzVersion(s.stateDB.WithContext(ctx), subject)
}

func currentAuthzVersion(tx *gorm.DB, subject string) (int64, error) {
	var epoch model.AuthzEpoch
	err := tx.Where("subject = ?", subject).First(&epoch).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return epoch.Version, nil
}

// BumpAuthzVersion 在独立事务里把 subject 的版本 +1 并返回新版本。
// 首次 bump 自动建立纪元行；并发调用由单条 UPDATE 的原子性串行化，不会丢版本。
func (s *Storage) BumpAuthzVersion(ctx context.Context, subject string) (int64, error) {
	if s.stateDB == nil {
		return 0, errStateDBNotConfigured
	}
	var version int64
	err := s.stateDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		v, err := bumpAuthzVersionTx(tx, subject, time.Now())
		if err != nil {
			return err
		}
		version = v
		return nil
	})
	return version, err
}

// BumpAuthzVersionTx 在调用方持有的事务里 bump 版本（供撤权流程与持久化同事务提交）。
func BumpAuthzVersionTx(tx *gorm.DB, subject string, now time.Time) (int64, error) {
	return bumpAuthzVersionTx(tx, subject, now)
}

func bumpAuthzVersionTx(tx *gorm.DB, subject string, now time.Time) (int64, error) {
	if err := tx.Exec(
		`INSERT INTO authz_epochs (subject, version, updated_at) VALUES (?, 0, ?) ON CONFLICT (subject) DO NOTHING`,
		subject, now,
	).Error; err != nil {
		return 0, err
	}
	res := tx.Exec(
		`UPDATE authz_epochs SET version = version + 1, updated_at = ? WHERE subject = ?`,
		now, subject,
	)
	if res.Error != nil {
		return 0, res.Error
	}
	if res.RowsAffected == 0 {
		return 0, errors.New("authz_epochs bump affected 0 rows")
	}
	return currentAuthzVersion(tx, subject)
}

// ListIdentityLinksByImmutableSubject 按 immutable_subject 查全部身份关联（不限状态），
// 供 authz 视图与 RequireEntitlement 判定「是否存在 active 关联」。
func (s *Storage) ListIdentityLinksByImmutableSubject(ctx context.Context, subject string) ([]model.IdentityLink, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var links []model.IdentityLink
	err := s.stateDB.WithContext(ctx).
		Where("immutable_subject = ?", subject).
		Order("created_at ASC").
		Find(&links).Error
	return links, err
}

// ListActiveManagedEntitlementsBySubjectTenant 返回 (subject, tenant) 下未撤销的受管授权项。
func (s *Storage) ListActiveManagedEntitlementsBySubjectTenant(ctx context.Context, subject, tenantID string) ([]model.ManagedEntitlement, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var ents []model.ManagedEntitlement
	err := s.stateDB.WithContext(ctx).
		Where("subject = ? AND tenant_id = ? AND revoked_at IS NULL", subject, tenantID).
		Order("created_at ASC").
		Find(&ents).Error
	return ents, err
}

// ListActiveEntitlementSubjectsByTenant 返回该 tenant 下仍有未撤销授权项的 subject 去重清单
// （tenant 级撤权用于逐 subject bump 版本）。
func (s *Storage) ListActiveEntitlementSubjectsByTenant(ctx context.Context, tenantID string) ([]string, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var subjects []string
	err := s.stateDB.WithContext(ctx).Model(&model.ManagedEntitlement{}).
		Where("tenant_id = ? AND revoked_at IS NULL", tenantID).
		Distinct().
		Pluck("subject", &subjects).Error
	return subjects, err
}

// DisableIdentityLinkByIDTx 把指定身份关联置为 disabled（仅当当前为 active 才生效）。
func DisableIdentityLinkByIDTx(tx *gorm.DB, id string, now time.Time) (int64, error) {
	res := tx.Model(&model.IdentityLink{}).
		Where("id = ? AND status = 'active'", id).
		Updates(map[string]interface{}{
			"status":     "disabled",
			"updated_at": now,
		})
	return res.RowsAffected, res.Error
}

// DisableIdentityLinksBySubjectTx 把 immutable_subject 的全部 active 关联置为 disabled。
func DisableIdentityLinksBySubjectTx(tx *gorm.DB, subject string, now time.Time) (int64, error) {
	res := tx.Model(&model.IdentityLink{}).
		Where("immutable_subject = ? AND status = 'active'", subject).
		Updates(map[string]interface{}{
			"status":     "disabled",
			"updated_at": now,
		})
	return res.RowsAffected, res.Error
}

// RevokeManagedEntitlementsBySubjectTx 把 subject 的全部未撤销授权项落 revoked_at。
func RevokeManagedEntitlementsBySubjectTx(tx *gorm.DB, subject string, now time.Time) (int64, error) {
	res := tx.Model(&model.ManagedEntitlement{}).
		Where("subject = ? AND revoked_at IS NULL", subject).
		Updates(map[string]interface{}{
			"revoked_at": now,
			"updated_at": now,
		})
	return res.RowsAffected, res.Error
}

// RevokeManagedEntitlementsByTenantTx 把 tenant 下全部未撤销授权项落 revoked_at。
func RevokeManagedEntitlementsByTenantTx(tx *gorm.DB, tenantID string, now time.Time) (int64, error) {
	res := tx.Model(&model.ManagedEntitlement{}).
		Where("tenant_id = ? AND revoked_at IS NULL", tenantID).
		Updates(map[string]interface{}{
			"revoked_at": now,
			"updated_at": now,
		})
	return res.RowsAffected, res.Error
}

// RenewRuntimeGrantLease 原子续期租约：仅当 grant 未撤销、旧到期值未被并发改写、
// 且仍在续期宽容窗（lease_expires_at > floor）内才生效。影响行数 0 = 并发单输/已撤销/窗口外。
func (s *Storage) RenewRuntimeGrantLease(ctx context.Context, grantID string, oldExpiry, newExpiry, floor time.Time) (int64, error) {
	if s.stateDB == nil {
		return 0, errStateDBNotConfigured
	}
	res := s.stateDB.WithContext(ctx).Model(&model.RuntimeGrant{}).
		Where("grant_id = ? AND revoked_at IS NULL AND lease_expires_at = ? AND lease_expires_at > ?", grantID, oldExpiry, floor).
		Updates(map[string]interface{}{
			"lease_expires_at": newExpiry,
			"updated_at":       time.Now(),
		})
	return res.RowsAffected, res.Error
}

// RevokeRuntimeGrant 把租约标记为已撤销（revoke 版本校验失败时落库，条件更新保证幂等）。
func (s *Storage) RevokeRuntimeGrant(ctx context.Context, grantID string, now time.Time) (int64, error) {
	if s.stateDB == nil {
		return 0, errStateDBNotConfigured
	}
	res := s.stateDB.WithContext(ctx).Model(&model.RuntimeGrant{}).
		Where("grant_id = ? AND revoked_at IS NULL", grantID).
		Updates(map[string]interface{}{
			"revoked_at": now,
			"updated_at": now,
		})
	return res.RowsAffected, res.Error
}

// DeleteAuthTokensByUser 删除 WeKnora 主库 auth_tokens 中指定用户的全部行
// （既有兼容行为：上游 token 立即失效；P09 建桥后再隔离收窄）。返回删除行数。
func (s *Storage) DeleteAuthTokensByUser(ctx context.Context, knowledgeUserID string) (int64, error) {
	if s.db == nil {
		return 0, errors.New("main db not configured")
	}
	res := s.db.WithContext(ctx).
		Where("user_id = ?", knowledgeUserID).
		Delete(&model.WeKnoraAuthToken{})
	return res.RowsAffected, res.Error
}

// ListAuthTokensByUser 读取 WeKnora 主库 auth_tokens 中指定用户的全部行
// （租户级撤权按 token claims 过滤用；只读）。
func (s *Storage) ListAuthTokensByUser(ctx context.Context, knowledgeUserID string) ([]model.WeKnoraAuthToken, error) {
	if s.db == nil {
		return nil, errors.New("main db not configured")
	}
	var tokens []model.WeKnoraAuthToken
	err := s.db.WithContext(ctx).
		Where("user_id = ?", knowledgeUserID).
		Find(&tokens).Error
	return tokens, err
}

// DeleteAuthTokensByIDs 按主键删除 WeKnora 主库 auth_tokens 行（幂等）。返回删除行数。
func (s *Storage) DeleteAuthTokensByIDs(ctx context.Context, ids []string) (int64, error) {
	if s.db == nil {
		return 0, errors.New("main db not configured")
	}
	if len(ids) == 0 {
		return 0, nil
	}
	res := s.db.WithContext(ctx).
		Where("id IN ?", ids).
		Delete(&model.WeKnoraAuthToken{})
	return res.RowsAffected, res.Error
}
