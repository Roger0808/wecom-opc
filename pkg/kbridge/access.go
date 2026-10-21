package kbridge

import (
	"context"
	"errors"
	"fmt"

	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"

	"gorm.io/gorm"
)

// WeKnoraStore 是 WeKnoraAccess 的生产实现：持有既有主库句柄，但只暴露
// users / tenant_members 只读 + auth_tokens 写必要操作（P09 收窄表面；
// 数据库账号侧另配最小权限，见包注释）。
type WeKnoraStore struct {
	s *storage.Storage
}

// NewWeKnoraStore 构造收窄的 WeKnora 主库访问面。
func NewWeKnoraStore(s *storage.Storage) *WeKnoraStore {
	if s == nil || s.DB() == nil {
		panic("kbridge: storage with main db is required")
	}
	return &WeKnoraStore{s: s}
}

var _ WeKnoraAccess = (*WeKnoraStore)(nil)

// GetUserByID users 只读（桥签发前校验用户存在/未停用/非管理员）。
func (st *WeKnoraStore) GetUserByID(ctx context.Context, id string) (*model.WeKnoraUser, error) {
	var user model.WeKnoraUser
	err := st.s.DB().WithContext(ctx).Where("id = ?", id).First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("user %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// GetTenantMembership tenant_members 只读（校验上游成员关系 active）。
func (st *WeKnoraStore) GetTenantMembership(ctx context.Context, userID string, tenantID uint64) (*model.WeKnoraTenantMember, error) {
	var member model.WeKnoraTenantMember
	err := st.s.DB().WithContext(ctx).
		Where("user_id = ? AND tenant_id = ?", userID, tenantID).
		First(&member).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &member, nil
}

// CreateAuthToken auth_tokens 写：插入桥签发的行（WeKnora ValidateToken 依赖行存在）。
func (st *WeKnoraStore) CreateAuthToken(ctx context.Context, token *model.WeKnoraAuthToken) error {
	return st.s.CreateAuthToken(ctx, token)
}

// DeleteAuthTokensByUser auth_tokens 写：撤权删除该用户全部行（幂等）。
func (st *WeKnoraStore) DeleteAuthTokensByUser(ctx context.Context, knowledgeUserID string) (int64, error) {
	return st.s.DeleteAuthTokensByUser(ctx, knowledgeUserID)
}

// ListAuthTokensByUser auth_tokens 读：租户级撤权按 claims 过滤用。
func (st *WeKnoraStore) ListAuthTokensByUser(ctx context.Context, knowledgeUserID string) ([]model.WeKnoraAuthToken, error) {
	return st.s.ListAuthTokensByUser(ctx, knowledgeUserID)
}

// DeleteAuthTokensByIDs auth_tokens 写：按主键定向删除（租户级撤权，幂等）。
func (st *WeKnoraStore) DeleteAuthTokensByIDs(ctx context.Context, ids []string) (int64, error) {
	return st.s.DeleteAuthTokensByIDs(ctx, ids)
}
