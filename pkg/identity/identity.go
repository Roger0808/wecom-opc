// Package identity 承载平台身份关联账本（identity_links，DATA-101a）的查询/写入逻辑，
// 供 TASK-P04 登录回调等流程解析「外部身份主体 → 平台身份」的映射关系。
//
// 解析规则（INT-101 登录事务状态机）：
//   - 命中唯一 active 行 → 返回该行；
//   - 同 (source, org, subject) 出现多行 active（防御性，唯一约束之外的异常数据）
//     或 knowledge_user_id 被多个 active 主体占用 → ErrConflict；
//   - 无任何 active 行 → ErrNotProvisioned（不自动开户、不触发同步）。
package identity

import (
	"context"
	"errors"

	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"
)

var (
	// ErrNotProvisioned 表示该主体在身份关联账本中没有 active 记录。
	ErrNotProvisioned = errors.New("identity: subject not provisioned")
	// ErrConflict 表示身份关联存在多映射或 knowledge_user_id 被多主体占用，无法判定。
	ErrConflict = errors.New("identity: subject conflict")
)

// Service 身份关联解析服务（只依赖状态库，不触碰 WeKnora 主库）。
type Service struct {
	storage *storage.Storage
}

func NewService(s *storage.Storage) *Service {
	return &Service{storage: s}
}

// Resolve 按 (identity_source, organization_id, immutable_subject) 解析身份关联。
// 返回的 link 保证为 active 状态且未与其他主体共享 knowledge_user_id。
func (s *Service) Resolve(ctx context.Context, source, org, subject string) (*model.IdentityLink, error) {
	links, err := s.storage.ListIdentityLinksBySubject(ctx, source, org, subject)
	if err != nil {
		return nil, err
	}
	active := make([]model.IdentityLink, 0, len(links))
	for _, l := range links {
		if l.Status == "active" {
			active = append(active, l)
		}
	}
	if len(active) == 0 {
		return nil, ErrNotProvisioned
	}
	if len(active) > 1 {
		return nil, ErrConflict
	}

	link := active[0]
	occupants, err := s.storage.ListIdentityLinksByKnowledgeUser(ctx, link.KnowledgeUserID)
	if err != nil {
		return nil, err
	}
	subjects := make(map[string]struct{}, len(occupants))
	for _, l := range occupants {
		if l.Status != "active" {
			continue
		}
		subjects[l.ImmutableSubject] = struct{}{}
	}
	if len(subjects) > 1 {
		return nil, ErrConflict
	}
	return &link, nil
}

// Insert 写入一条身份关联（供后续同步流程调用方使用；ID/时间戳由 storage 层自动填充）。
func (s *Service) Insert(ctx context.Context, link *model.IdentityLink) error {
	return s.storage.InsertIdentityLink(ctx, link)
}
