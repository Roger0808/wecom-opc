package storage

import (
	"context"
	"time"

	"weknora-tokendance/pkg/model"

	"gorm.io/gorm"
)

// 本文件承载 TASK-P04（INT-101）认证流程依赖的状态库原子状态迁移与定向查询。
// 原子性由「条件 UPDATE + RowsAffected」保证：并发下只会有一个请求赢得竞争，
// 其余请求拿到 0 行影响并走拒绝路径。这里只做存取，业务判定在 handler/service 层。

// GetLatestAuthTransactionByStateHash 按 state_hash 取最近一条登录事务（不限状态），
// 供 /integration/v1/auth/callback 定位事务并取回已白名单校验过的 redirect_uri。
func (s *Storage) GetLatestAuthTransactionByStateHash(ctx context.Context, stateHash string) (*model.AuthTransaction, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var txn model.AuthTransaction
	if err := s.stateDB.WithContext(ctx).
		Where("state_hash = ?", stateHash).
		Order("created_at DESC").
		First(&txn).Error; err != nil {
		return nil, err
	}
	return &txn, nil
}

// GetLatestAuthTransactionByCodeHash 按 code_hash 取最近一条已签发授权码的事务。
func (s *Storage) GetLatestAuthTransactionByCodeHash(ctx context.Context, codeHash string) (*model.AuthTransaction, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var txn model.AuthTransaction
	if err := s.stateDB.WithContext(ctx).
		Where("code_hash = ?", codeHash).
		Order("created_at DESC").
		First(&txn).Error; err != nil {
		return nil, err
	}
	return &txn, nil
}

// IssueAuthCode 在 callback 成功路径上把一次性授权码落到事务上：
// 仅当事务仍处于 pending 时生效；授权码窗口 120s 由 expires_at 一并收紧。
func (s *Storage) IssueAuthCode(ctx context.Context, loginTxnID, codeHash, identityLinkID string, expiresAt time.Time) (int64, error) {
	if s.stateDB == nil {
		return 0, errStateDBNotConfigured
	}
	res := s.stateDB.WithContext(ctx).Model(&model.AuthTransaction{}).
		Where("login_txn_id = ? AND status = 'pending'", loginTxnID).
		Updates(map[string]interface{}{
			"code_hash":        codeHash,
			"identity_link_id": identityLinkIdOrNil(identityLinkID),
			"status":           "completed",
			"expires_at":       expiresAt,
			"updated_at":       time.Now(),
		})
	return res.RowsAffected, res.Error
}

// FailAuthTransaction 把登录事务标记为 failed（回调拒绝路径）。
// 仅当事务仍处于 pending 时生效（影响行数 0 = 已终态）：completed 是终态，
// 重试/迟到的回调拒绝不得把已签发授权码的事务翻转回 failed。
func (s *Storage) FailAuthTransaction(ctx context.Context, loginTxnID string) (int64, error) {
	if s.stateDB == nil {
		return 0, errStateDBNotConfigured
	}
	res := s.stateDB.WithContext(ctx).Model(&model.AuthTransaction{}).
		Where("login_txn_id = ? AND status = 'pending'", loginTxnID).
		Updates(map[string]interface{}{
			"status":     "failed",
			"updated_at": time.Now(),
		})
	return res.RowsAffected, res.Error
}

// ConsumeAuthCode 原子消费一次性授权码：completed 且未消费且未过期才会命中
// （与 code-exchange 对事务状态的口径一致：都认 completed + 未消费）。
// 影响行数为 0 表示重放/过期/未知 code，调用方按 AUTH_CODE_REPLAYED 拒绝。
func (s *Storage) ConsumeAuthCode(ctx context.Context, loginTxnID, codeHash string) (int64, error) {
	if s.stateDB == nil {
		return 0, errStateDBNotConfigured
	}
	res := s.stateDB.WithContext(ctx).Model(&model.AuthTransaction{}).
		Where("login_txn_id = ? AND code_hash = ? AND status = 'completed' AND consumed_at IS NULL AND expires_at > ?", loginTxnID, codeHash, time.Now()).
		Updates(map[string]interface{}{
			"consumed_at": time.Now(),
			"updated_at":  time.Now(),
		})
	return res.RowsAffected, res.Error
}

// RevokeSessionsByIdentityLinkIDsTx 在调用方事务内把指定身份关联的全部未撤销会话
// 落 revoked_at（撤权同事务生效：提交后 refresh/session 立即拒绝）。
func RevokeSessionsByIdentityLinkIDsTx(tx *gorm.DB, identityLinkIDs []string, reason string, now time.Time) (int64, error) {
	if len(identityLinkIDs) == 0 {
		return 0, nil
	}
	res := tx.Model(&model.Session{}).
		Where("identity_link_id IN ? AND revoked_at IS NULL", identityLinkIDs).
		Updates(map[string]interface{}{
			"revoked_at":    now,
			"revoke_reason": reason,
			"updated_at":    now,
		})
	return res.RowsAffected, res.Error
}

// ListIdentityLinksBySubject 按 (identity_source, organization_id, immutable_subject) 查身份关联。
func (s *Storage) ListIdentityLinksBySubject(ctx context.Context, source, org, subject string) ([]model.IdentityLink, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var links []model.IdentityLink
	err := s.stateDB.WithContext(ctx).
		Where("identity_source = ? AND organization_id = ? AND immutable_subject = ?", source, org, subject).
		Order("created_at ASC").
		Find(&links).Error
	return links, err
}

// ListIdentityLinksByKnowledgeUser 按 knowledge_user_id 查身份关联（冲突判定用）。
func (s *Storage) ListIdentityLinksByKnowledgeUser(ctx context.Context, knowledgeUserID string) ([]model.IdentityLink, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var links []model.IdentityLink
	err := s.stateDB.WithContext(ctx).
		Where("knowledge_user_id = ?", knowledgeUserID).
		Order("created_at ASC").
		Find(&links).Error
	return links, err
}

// RotateSessionVerifier 轮换 refresh verifier：旧值挪入 previous_verifier_hash。
// WHERE 带上当前 verifier_hash，并发双刷新只有一个能赢，输家拿到 0 行走拒绝路径。
func (s *Storage) RotateSessionVerifier(ctx context.Context, sessionID, currentHash, newHash string) (int64, error) {
	if s.stateDB == nil {
		return 0, errStateDBNotConfigured
	}
	res := s.stateDB.WithContext(ctx).Model(&model.Session{}).
		Where("session_id = ? AND refresh_verifier_hash = ?", sessionID, currentHash).
		Updates(map[string]interface{}{
			"refresh_verifier_hash":  newHash,
			"previous_verifier_hash": currentHash,
			"updated_at":             time.Now(),
		})
	return res.RowsAffected, res.Error
}

// RevokeSessionFamily 撤销同 family 的全部会话（logout / refresh 复用检测）。
func (s *Storage) RevokeSessionFamily(ctx context.Context, familyID, reason string) (int64, error) {
	if s.stateDB == nil {
		return 0, errStateDBNotConfigured
	}
	res := s.stateDB.WithContext(ctx).Model(&model.Session{}).
		Where("family_id = ? AND revoked_at IS NULL", familyID).
		Updates(map[string]interface{}{
			"revoked_at":    time.Now(),
			"revoke_reason": reason,
			"updated_at":    time.Now(),
		})
	return res.RowsAffected, res.Error
}

// ConsumeExchangeMaterial 原子消费 code-backend 一次性兑换材料（暂存在 sessions 表，
// 120s 租约，revoke_reason 记兑换消费语义）。影响行数 0 = 重放/过期。
func (s *Storage) ConsumeExchangeMaterial(ctx context.Context, sessionID, materialHash string) (int64, error) {
	if s.stateDB == nil {
		return 0, errStateDBNotConfigured
	}
	res := s.stateDB.WithContext(ctx).Model(&model.Session{}).
		Where("session_id = ? AND refresh_verifier_hash = ? AND revoked_at IS NULL AND expires_at > ?", sessionID, materialHash, time.Now()).
		Updates(map[string]interface{}{
			"revoked_at":    time.Now(),
			"revoke_reason": "code_exchange.consumed",
			"updated_at":    time.Now(),
		})
	return res.RowsAffected, res.Error
}

// ListActiveManagedEntitlementsBySubject 返回指定主体未撤销的受管授权项（created_at 升序）。
func (s *Storage) ListActiveManagedEntitlementsBySubject(ctx context.Context, subject string) ([]model.ManagedEntitlement, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var ents []model.ManagedEntitlement
	err := s.stateDB.WithContext(ctx).
		Where("subject = ? AND revoked_at IS NULL", subject).
		Order("created_at ASC").
		Find(&ents).Error
	return ents, err
}

// TouchSessionLastSeen 更新会话最近活跃时间（best-effort 视图维护）。
func (s *Storage) TouchSessionLastSeen(ctx context.Context, sessionID string, seenAt time.Time) error {
	if s.stateDB == nil {
		return errStateDBNotConfigured
	}
	return s.stateDB.WithContext(ctx).Model(&model.Session{}).
		Where("session_id = ?", sessionID).
		Update("last_seen_at", seenAt).Error
}

// identityLinkIdOrNil 空串映射为 NULL，保持列可空语义。
func identityLinkIdOrNil(id string) interface{} {
	if id == "" {
		return nil
	}
	return id
}
