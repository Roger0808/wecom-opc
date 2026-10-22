package storage

import (
	"context"
	"fmt"
	"time"

	"weknora-tokendance/pkg/model"

	"github.com/google/uuid"

	"gorm.io/gorm"
)

// 本文件承载 TASK-P08（INT-130、BND-130/131）手动同步服务的持久化支撑：
// sync_runs 互斥租约（行锁抢占 + 过期接管）、sync_steps 逐步记账、
// managed_entitlements（source=sync）与 identity_links 的定向读写。
//
// 互斥锁（BND-130，禁用进程内 isSyncing）：
//   - 运行中集合 = queued/snapshotting/validating/diffing/applying/reconciling；
//   - 同 scope+department_code 且运行中且 lock_until > now 的 run 持锁；
//   - 抢占序：单事务内先「touch」在跑行（UPDATE 取行锁，READ COMMITTED 下
//     后提交者会重看到已提交持锁行），再统计持锁行数，>0 即 SYNC_RUN_CONFLICT；
//   - lock_until 过期的运行中 run 视为崩溃残留，由新 run 在同一事务里接管为 failed。
//
// 全部读写都落在状态库句柄 s.stateDB 上（SetUserActive 除外，属 WeKnora 主库兼容写）。

// SyncRunRunningStatuses SQL 枚举：sync_runs 的运行中状态集合（持久化互斥判定用）。
const SyncRunRunningStatuses = `('queued','snapshotting','validating','diffing','applying','reconciling')`

// FindSyncRunByIdempotencyKey 按幂等键读取同步运行（幂等重放：命中即返回原 run）。
func (s *Storage) FindSyncRunByIdempotencyKey(ctx context.Context, key string) (*model.SyncRun, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var run model.SyncRun
	if err := s.stateDB.WithContext(ctx).Where("idempotency_key = ?", key).First(&run).Error; err != nil {
		return nil, err
	}
	return &run, nil
}

// FindSyncRunByIdempotencyKeyTx 在调用方事务内按幂等键读取同步运行
// （Run 的锁事务内做重放判定：先锁再查键，并发同键串行化后必见原 run）。
func (s *Storage) FindSyncRunByIdempotencyKeyTx(tx *gorm.DB, key string) (*model.SyncRun, error) {
	var run model.SyncRun
	if err := tx.Where("idempotency_key = ?", key).First(&run).Error; err != nil {
		return nil, err
	}
	return &run, nil
}

// conflictPredicate 返回「与 (scope, department_code) 新运行互斥」的 SQL 谓词与参数：
// full 运行触碰全部主体，与任何运行中的 run（full 或 department）互斥；
// department 运行与运行中的 full 运行或同部门 department 运行互斥。
func conflictPredicate(scope, departmentCode string) (string, []interface{}) {
	if scope == "department" {
		return `(scope = 'full' OR (scope = 'department' AND department_code IS NOT DISTINCT FROM ?))`, []interface{}{departmentCode}
	}
	return `(1 = 1)`, nil
}

// TouchSyncRunLocksTx 在调用方事务内「触摸」与新运行互斥且仍在持锁的运行中 run：
// UPDATE 会对命中行加行锁，使并发 Run 的冲突判定在 READ COMMITTED 下也串行可见。
// 返回触摸行数（>0 表示存在持锁中的并发运行）。
func (s *Storage) TouchSyncRunLocksTx(tx *gorm.DB, scope, departmentCode string, now time.Time) (int64, error) {
	predicate, args := conflictPredicate(scope, departmentCode)
	query := `UPDATE sync_runs SET lock_until = lock_until, updated_at = ?
		WHERE status IN ` + SyncRunRunningStatuses + ` AND lock_until > ? AND ` + predicate
	res := tx.Exec(query, append([]interface{}{now, now}, args...)...)
	return res.RowsAffected, res.Error
}

// FailStaleSyncRunsTx 把与新运行互斥、运行中但持锁已过期（或锁为空）的 run 标记为
// failed 并释放锁：崩溃进程残留的运行在此被新运行接管（BND-130）。
func (s *Storage) FailStaleSyncRunsTx(tx *gorm.DB, scope, departmentCode string, now time.Time) (int64, error) {
	predicate, args := conflictPredicate(scope, departmentCode)
	query := `UPDATE sync_runs SET status = 'failed', lock_until = NULL, finished_at = ?, updated_at = ?
		WHERE status IN ` + SyncRunRunningStatuses + ` AND (lock_until IS NULL OR lock_until <= ?) AND ` + predicate
	res := tx.Exec(query, append([]interface{}{now, now, now}, args...)...)
	return res.RowsAffected, res.Error
}

// CountLockedRunningSyncRunsTx 统计与新运行互斥且持锁中的运行中 run 数（可排除指定 run，
// 供 Retry 排除自身）。excludeRunID 为空时不排除（创建路径，避免 uuid 空串比较在 PG 上
// 22P02——sqlite 动态类型掩盖了该问题，部署实测暴露）。
func (s *Storage) CountLockedRunningSyncRunsTx(tx *gorm.DB, scope, departmentCode string, now time.Time, excludeRunID string) (int64, error) {
	predicate, args := conflictPredicate(scope, departmentCode)
	query := `SELECT COUNT(*) FROM sync_runs
		WHERE status IN ` + SyncRunRunningStatuses + ` AND lock_until > ? AND ` + predicate
	params := append([]interface{}{now}, args...)
	if excludeRunID != "" {
		query += ` AND run_id <> ?`
		params = append(params, excludeRunID)
	}
	var count int64
	err := tx.Raw(query, params...).Scan(&count).Error
	return count, err
}

// InsertSyncRunTx 在调用方事务内写入同步运行记录。
func (s *Storage) InsertSyncRunTx(tx *gorm.DB, run *model.SyncRun) error {
	if run.RunID == "" {
		run.RunID = uuid.New().String()
	}
	now := time.Now()
	if run.CreatedAt.IsZero() {
		run.CreatedAt = now
	}
	if run.UpdatedAt.IsZero() {
		run.UpdatedAt = now
	}
	return tx.Create(run).Error
}

// UpdateSyncRunTx 在调用方事务内条件更新 run 行（fields 至少含 status/updated_at）。
func (s *Storage) UpdateSyncRunTx(tx *gorm.DB, runID string, fields map[string]interface{}) (int64, error) {
	res := tx.Model(&model.SyncRun{}).Where("run_id = ?", runID).Updates(fields)
	return res.RowsAffected, res.Error
}

// ListSyncRunsPage 按状态过滤分页返回同步运行（status 为空不过滤；created_at 降序，新的在前）。
func (s *Storage) ListSyncRunsPage(ctx context.Context, status string, page, pageSize int) ([]model.SyncRun, int64, error) {
	if s.stateDB == nil {
		return nil, 0, errStateDBNotConfigured
	}
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 20
	}
	q := s.stateDB.WithContext(ctx).Model(&model.SyncRun{})
	if status != "" {
		q = q.Where("status = ?", status)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var runs []model.SyncRun
	err := q.Order("created_at DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&runs).Error
	return runs, total, err
}

// FindSyncStepByKey 按 (run_id, idempotency_key) 定位步骤行（apply 幂等去重）。
func (s *Storage) FindSyncStepByKey(ctx context.Context, runID, idempotencyKey string) (*model.SyncStep, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var step model.SyncStep
	err := s.stateDB.WithContext(ctx).
		Where("run_id = ? AND idempotency_key = ?", runID, idempotencyKey).
		First(&step).Error
	if err != nil {
		return nil, err
	}
	return &step, nil
}

// InsertSyncStepTx 在调用方事务内写入步骤行。
func (s *Storage) InsertSyncStepTx(tx *gorm.DB, step *model.SyncStep) error {
	now := time.Now()
	if step.CreatedAt.IsZero() {
		step.CreatedAt = now
	}
	if step.UpdatedAt.IsZero() {
		step.UpdatedAt = now
	}
	return tx.Create(step).Error
}

// UpdateSyncStepTx 在调用方事务内更新步骤状态。
func (s *Storage) UpdateSyncStepTx(tx *gorm.DB, runID string, stepSeq int, fields map[string]interface{}) (int64, error) {
	res := tx.Model(&model.SyncStep{}).Where("run_id = ? AND step_seq = ?", runID, stepSeq).Updates(fields)
	return res.RowsAffected, res.Error
}

// CountActionSteps 统计指定 run 的行动步骤（provision/revoke，排除 skipped）：
// 返回 planned 与 applied 数，reconciling 阶段 applied==planned 判定用。
func (s *Storage) CountActionSteps(ctx context.Context, runID string) (planned, applied int64, err error) {
	if s.stateDB == nil {
		return 0, 0, errStateDBNotConfigured
	}
	base := s.stateDB.WithContext(ctx).Model(&model.SyncStep{}).
		Where("run_id = ? AND step_type IN ('provision','revoke') AND status <> 'skipped'", runID)
	if err := base.Count(&planned).Error; err != nil {
		return 0, 0, err
	}
	if err := base.Where("status = 'applied'").Count(&applied).Error; err != nil {
		return 0, 0, err
	}
	return planned, applied, nil
}

// ListActiveSyncSourceEntitlements 返回全部未撤销的 sync 源受管授权项。
func (s *Storage) ListActiveSyncSourceEntitlements(ctx context.Context) ([]model.ManagedEntitlement, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var ents []model.ManagedEntitlement
	err := s.stateDB.WithContext(ctx).
		Where("source = 'sync' AND revoked_at IS NULL").
		Order("created_at ASC").
		Find(&ents).Error
	return ents, err
}

// ListActiveSyncSourceEntitlementsByTenant 返回指定 tenant 下未撤销的 sync 源受管授权项
// （department scope 的收回候选只出自本部门空间）。
func (s *Storage) ListActiveSyncSourceEntitlementsByTenant(ctx context.Context, tenantID string) ([]model.ManagedEntitlement, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var ents []model.ManagedEntitlement
	err := s.stateDB.WithContext(ctx).
		Where("source = 'sync' AND tenant_id = ? AND revoked_at IS NULL", tenantID).
		Order("created_at ASC").
		Find(&ents).Error
	return ents, err
}

// FindActiveSyncEntitlement 读取 (subject, tenant) 下未撤销的 sync 源授权项（apply 幂等去重）。
func (s *Storage) FindActiveSyncEntitlement(ctx context.Context, subject, tenantID string) (*model.ManagedEntitlement, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var ent model.ManagedEntitlement
	err := s.stateDB.WithContext(ctx).
		Where("subject = ? AND tenant_id = ? AND source = 'sync' AND revoked_at IS NULL", subject, tenantID).
		First(&ent).Error
	if err != nil {
		return nil, err
	}
	return &ent, nil
}

// InsertManagedEntitlementTx 在调用方事务内写入受管授权项。
func (s *Storage) InsertManagedEntitlementTx(tx *gorm.DB, ent *model.ManagedEntitlement) error {
	if ent.ID == "" {
		ent.ID = uuid.New().String()
	}
	now := time.Now()
	if ent.CreatedAt.IsZero() {
		ent.CreatedAt = now
	}
	if ent.UpdatedAt.IsZero() {
		ent.UpdatedAt = now
	}
	return tx.Create(ent).Error
}

// RevokeManagedEntitlementByIDTx 把单条授权项落 revoked_at（条件更新保证幂等）。
func RevokeManagedEntitlementByIDTx(tx *gorm.DB, id string, now time.Time) (int64, error) {
	res := tx.Model(&model.ManagedEntitlement{}).
		Where("id = ? AND revoked_at IS NULL", id).
		Updates(map[string]interface{}{
			"revoked_at": now,
			"updated_at": now,
		})
	return res.RowsAffected, res.Error
}

// FindIdentityLinkBySubject 按 (identity_source, organization_id, immutable_subject) 唯一键读取
// 身份关联（provision 幂等去重与冲突判定用；不存在返回 gorm.ErrRecordNotFound）。
func (s *Storage) FindIdentityLinkBySubject(ctx context.Context, source, org, subject string) (*model.IdentityLink, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var link model.IdentityLink
	err := s.stateDB.WithContext(ctx).
		Where("identity_source = ? AND organization_id = ? AND immutable_subject = ?", source, org, subject).
		First(&link).Error
	if err != nil {
		return nil, err
	}
	return &link, nil
}

// ListConflictedIdentityLinks 返回全部 conflicted 身份关联（INT-130 冲突清单）。
func (s *Storage) ListConflictedIdentityLinks(ctx context.Context) ([]model.IdentityLink, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var links []model.IdentityLink
	err := s.stateDB.WithContext(ctx).
		Where("status = 'conflicted'").
		Order("updated_at DESC").
		Find(&links).Error
	return links, err
}

// UpdateIdentityLinkTx 在调用方事务内条件更新身份关联。
func (s *Storage) UpdateIdentityLinkTx(tx *gorm.DB, id string, fields map[string]interface{}) (int64, error) {
	res := tx.Model(&model.IdentityLink{}).Where("id = ?", id).Updates(fields)
	return res.RowsAffected, res.Error
}

// SetUserActive 直写 WeKnora 主库 users.is_active（同步撤权的既有兼容行为，P09 再隔离）。
func (s *Storage) SetUserActive(ctx context.Context, userID string, active bool) (int64, error) {
	if s.db == nil {
		return 0, fmt.Errorf("main db not configured")
	}
	res := s.db.WithContext(ctx).Model(&model.WeKnoraUser{}).
		Where("id = ?", userID).
		Updates(map[string]interface{}{
			"is_active":  active,
			"updated_at": time.Now(),
		})
	return res.RowsAffected, res.Error
}
