package storage

import (
	"context"
	"errors"
	"time"

	"weknora-tokendance/pkg/model"

	"github.com/google/uuid"
)

// errStateDBNotConfigured 在未注入状态库连接时返回（审计与状态账本不允许静默落到 WeKnora 主库）。
var errStateDBNotConfigured = errors.New("state db not configured")

// InsertAuditEvent 向平台自有状态库追加写入一条审计事件。ID 与 CreatedAt 为空时自动填充。
func (s *Storage) InsertAuditEvent(ctx context.Context, event *model.AuditEvent) error {
	if s.stateDB == nil {
		return errStateDBNotConfigured
	}
	if event.ID == "" {
		event.ID = uuid.New().String()
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	return s.stateDB.WithContext(ctx).Create(event).Error
}

// GetAuditEvent 按 ID 从状态库读取一条审计事件（不存在返回 gorm.ErrRecordNotFound）。
func (s *Storage) GetAuditEvent(ctx context.Context, id string) (*model.AuditEvent, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var event model.AuditEvent
	if err := s.stateDB.WithContext(ctx).Where("id = ?", id).First(&event).Error; err != nil {
		return nil, err
	}
	return &event, nil
}

// ListAuditEvents 返回状态库中的全部审计事件（created_at 升序，确定性输出便于断言）。
func (s *Storage) ListAuditEvents(ctx context.Context) ([]model.AuditEvent, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var events []model.AuditEvent
	err := s.stateDB.WithContext(ctx).Order("created_at ASC").Find(&events).Error
	return events, err
}

// ListAuditEventsByAction 按 action 过滤读取审计事件（created_at 升序）。
// 租户级撤权用它关联桥签发的 auth_tokens 行（kbridge.token.issued 记录 auth_token_id）。
func (s *Storage) ListAuditEventsByAction(ctx context.Context, action string) ([]model.AuditEvent, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var events []model.AuditEvent
	err := s.stateDB.WithContext(ctx).
		Where("action = ?", action).
		Order("created_at ASC").
		Find(&events).Error
	return events, err
}
