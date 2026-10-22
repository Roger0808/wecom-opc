package storage

import (
	"context"
	"time"

	"weknora-tokendance/pkg/model"

	"github.com/google/uuid"
)

// 本文件承载 platform 自有状态库（statedb）各账本表的最小 repository 方法（Get/Insert/List），
// 供 P04~P10 的认证、授权、同步与发布流程使用。这里只做存取，不实现业务逻辑；
// 全部读写都落在状态库句柄 s.stateDB 上，绝不写入 WeKnora 主库。

// ---------- identity_links (DATA-101a) ----------

// GetIdentityLink 按主键 ID 读取身份关联（不存在返回 gorm.ErrRecordNotFound）。
func (s *Storage) GetIdentityLink(ctx context.Context, id string) (*model.IdentityLink, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var link model.IdentityLink
	if err := s.stateDB.WithContext(ctx).Where("id = ?", id).First(&link).Error; err != nil {
		return nil, err
	}
	return &link, nil
}

// InsertIdentityLink 写入一条身份关联。ID 为空时自动生成 UUID，CreatedAt/UpdatedAt 为空时填充当前时间。
func (s *Storage) InsertIdentityLink(ctx context.Context, link *model.IdentityLink) error {
	if s.stateDB == nil {
		return errStateDBNotConfigured
	}
	if link.ID == "" {
		link.ID = uuid.New().String()
	}
	now := time.Now()
	if link.CreatedAt.IsZero() {
		link.CreatedAt = now
	}
	if link.UpdatedAt.IsZero() {
		link.UpdatedAt = now
	}
	return s.stateDB.WithContext(ctx).Create(link).Error
}

// ListIdentityLinks 返回全部身份关联（created_at 升序）。
func (s *Storage) ListIdentityLinks(ctx context.Context) ([]model.IdentityLink, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var links []model.IdentityLink
	err := s.stateDB.WithContext(ctx).Order("created_at ASC").Find(&links).Error
	return links, err
}

// ---------- auth_transactions (DATA-101b) ----------

// GetAuthTransaction 按 login_txn_id 读取登录事务。
func (s *Storage) GetAuthTransaction(ctx context.Context, loginTxnID string) (*model.AuthTransaction, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var txn model.AuthTransaction
	if err := s.stateDB.WithContext(ctx).Where("login_txn_id = ?", loginTxnID).First(&txn).Error; err != nil {
		return nil, err
	}
	return &txn, nil
}

// InsertAuthTransaction 写入一条登录事务，自动填充主键与时间戳。
func (s *Storage) InsertAuthTransaction(ctx context.Context, txn *model.AuthTransaction) error {
	if s.stateDB == nil {
		return errStateDBNotConfigured
	}
	if txn.LoginTxnID == "" {
		txn.LoginTxnID = uuid.New().String()
	}
	now := time.Now()
	if txn.CreatedAt.IsZero() {
		txn.CreatedAt = now
	}
	if txn.UpdatedAt.IsZero() {
		txn.UpdatedAt = now
	}
	return s.stateDB.WithContext(ctx).Create(txn).Error
}

// ListAuthTransactions 返回全部登录事务（created_at 升序）。
func (s *Storage) ListAuthTransactions(ctx context.Context) ([]model.AuthTransaction, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var txns []model.AuthTransaction
	err := s.stateDB.WithContext(ctx).Order("created_at ASC").Find(&txns).Error
	return txns, err
}

// ---------- sessions (DATA-101c) ----------

// GetSession 按 session_id 读取会话。
func (s *Storage) GetSession(ctx context.Context, sessionID string) (*model.Session, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var session model.Session
	if err := s.stateDB.WithContext(ctx).Where("session_id = ?", sessionID).First(&session).Error; err != nil {
		return nil, err
	}
	return &session, nil
}

// InsertSession 写入一条会话，自动填充主键与时间戳。
func (s *Storage) InsertSession(ctx context.Context, session *model.Session) error {
	if s.stateDB == nil {
		return errStateDBNotConfigured
	}
	if session.SessionID == "" {
		session.SessionID = uuid.New().String()
	}
	now := time.Now()
	if session.CreatedAt.IsZero() {
		session.CreatedAt = now
	}
	if session.UpdatedAt.IsZero() {
		session.UpdatedAt = now
	}
	return s.stateDB.WithContext(ctx).Create(session).Error
}

// ListSessions 返回全部会话（created_at 升序）。
func (s *Storage) ListSessions(ctx context.Context) ([]model.Session, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var sessions []model.Session
	err := s.stateDB.WithContext(ctx).Order("created_at ASC").Find(&sessions).Error
	return sessions, err
}

// ---------- runtime_grants (DATA-101d) ----------

// GetRuntimeGrant 按 grant_id 读取授权租约。
func (s *Storage) GetRuntimeGrant(ctx context.Context, grantID string) (*model.RuntimeGrant, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var grant model.RuntimeGrant
	if err := s.stateDB.WithContext(ctx).Where("grant_id = ?", grantID).First(&grant).Error; err != nil {
		return nil, err
	}
	return &grant, nil
}

// InsertRuntimeGrant 写入一条授权租约，自动填充主键与时间戳。
func (s *Storage) InsertRuntimeGrant(ctx context.Context, grant *model.RuntimeGrant) error {
	if s.stateDB == nil {
		return errStateDBNotConfigured
	}
	if grant.GrantID == "" {
		grant.GrantID = uuid.New().String()
	}
	now := time.Now()
	if grant.CreatedAt.IsZero() {
		grant.CreatedAt = now
	}
	if grant.UpdatedAt.IsZero() {
		grant.UpdatedAt = now
	}
	return s.stateDB.WithContext(ctx).Create(grant).Error
}

// ListRuntimeGrants 返回全部授权租约（created_at 升序）。
func (s *Storage) ListRuntimeGrants(ctx context.Context) ([]model.RuntimeGrant, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var grants []model.RuntimeGrant
	err := s.stateDB.WithContext(ctx).Order("created_at ASC").Find(&grants).Error
	return grants, err
}

// ---------- workspace_links (DATA-101e) ----------

// GetWorkspaceLink 按 (identity_link_id, code_workspace_id) 复合键读取工作区关联。
func (s *Storage) GetWorkspaceLink(ctx context.Context, identityLinkID, codeWorkspaceID string) (*model.WorkspaceLink, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var link model.WorkspaceLink
	err := s.stateDB.WithContext(ctx).
		Where("identity_link_id = ? AND code_workspace_id = ?", identityLinkID, codeWorkspaceID).
		First(&link).Error
	if err != nil {
		return nil, err
	}
	return &link, nil
}

// InsertWorkspaceLink 写入一条工作区关联，自动填充时间戳。
func (s *Storage) InsertWorkspaceLink(ctx context.Context, link *model.WorkspaceLink) error {
	if s.stateDB == nil {
		return errStateDBNotConfigured
	}
	now := time.Now()
	if link.CreatedAt.IsZero() {
		link.CreatedAt = now
	}
	if link.UpdatedAt.IsZero() {
		link.UpdatedAt = now
	}
	return s.stateDB.WithContext(ctx).Create(link).Error
}

// ListWorkspaceLinks 返回全部工作区关联（created_at 升序）。
func (s *Storage) ListWorkspaceLinks(ctx context.Context) ([]model.WorkspaceLink, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var links []model.WorkspaceLink
	err := s.stateDB.WithContext(ctx).Order("created_at ASC").Find(&links).Error
	return links, err
}

// ---------- managed_entitlements (DATA-110c) ----------

// GetManagedEntitlement 按主键 ID 读取受管授权项。
func (s *Storage) GetManagedEntitlement(ctx context.Context, id string) (*model.ManagedEntitlement, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var entitlement model.ManagedEntitlement
	if err := s.stateDB.WithContext(ctx).Where("id = ?", id).First(&entitlement).Error; err != nil {
		return nil, err
	}
	return &entitlement, nil
}

// InsertManagedEntitlement 写入一条受管授权项，自动填充主键与时间戳。
func (s *Storage) InsertManagedEntitlement(ctx context.Context, entitlement *model.ManagedEntitlement) error {
	if s.stateDB == nil {
		return errStateDBNotConfigured
	}
	if entitlement.ID == "" {
		entitlement.ID = uuid.New().String()
	}
	now := time.Now()
	if entitlement.CreatedAt.IsZero() {
		entitlement.CreatedAt = now
	}
	if entitlement.UpdatedAt.IsZero() {
		entitlement.UpdatedAt = now
	}
	return s.stateDB.WithContext(ctx).Create(entitlement).Error
}

// ListManagedEntitlements 返回全部受管授权项（created_at 升序）。
func (s *Storage) ListManagedEntitlements(ctx context.Context) ([]model.ManagedEntitlement, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var entitlements []model.ManagedEntitlement
	err := s.stateDB.WithContext(ctx).Order("created_at ASC").Find(&entitlements).Error
	return entitlements, err
}

// ---------- sync_runs (DATA-110a) ----------

// GetSyncRun 按 run_id 读取同步运行记录。
func (s *Storage) GetSyncRun(ctx context.Context, runID string) (*model.SyncRun, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var run model.SyncRun
	if err := s.stateDB.WithContext(ctx).Where("run_id = ?", runID).First(&run).Error; err != nil {
		return nil, err
	}
	return &run, nil
}

// InsertSyncRun 写入一条同步运行记录，自动填充主键与时间戳。
func (s *Storage) InsertSyncRun(ctx context.Context, run *model.SyncRun) error {
	if s.stateDB == nil {
		return errStateDBNotConfigured
	}
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
	return s.stateDB.WithContext(ctx).Create(run).Error
}

// ListSyncRuns 返回全部同步运行记录（created_at 升序）。
func (s *Storage) ListSyncRuns(ctx context.Context) ([]model.SyncRun, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var runs []model.SyncRun
	err := s.stateDB.WithContext(ctx).Order("created_at ASC").Find(&runs).Error
	return runs, err
}

// ---------- sync_steps (DATA-110b) ----------

// GetSyncStep 按 (run_id, step_seq) 复合主键读取同步步骤。
func (s *Storage) GetSyncStep(ctx context.Context, runID string, stepSeq int) (*model.SyncStep, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var step model.SyncStep
	err := s.stateDB.WithContext(ctx).
		Where("run_id = ? AND step_seq = ?", runID, stepSeq).
		First(&step).Error
	if err != nil {
		return nil, err
	}
	return &step, nil
}

// InsertSyncStep 写入一条同步步骤，自动填充时间戳。
func (s *Storage) InsertSyncStep(ctx context.Context, step *model.SyncStep) error {
	if s.stateDB == nil {
		return errStateDBNotConfigured
	}
	now := time.Now()
	if step.CreatedAt.IsZero() {
		step.CreatedAt = now
	}
	if step.UpdatedAt.IsZero() {
		step.UpdatedAt = now
	}
	return s.stateDB.WithContext(ctx).Create(step).Error
}

// ListSyncSteps 返回指定运行下的全部同步步骤（step_seq 升序）。
func (s *Storage) ListSyncSteps(ctx context.Context, runID string) ([]model.SyncStep, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var steps []model.SyncStep
	err := s.stateDB.WithContext(ctx).
		Where("run_id = ?", runID).
		Order("step_seq ASC").
		Find(&steps).Error
	return steps, err
}

// ---------- skill_publish_jobs (DATA-120b) ----------

// GetSkillPublishJob 按 job_id 读取技能发布任务。
func (s *Storage) GetSkillPublishJob(ctx context.Context, jobID string) (*model.SkillPublishJob, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var job model.SkillPublishJob
	if err := s.stateDB.WithContext(ctx).Where("job_id = ?", jobID).First(&job).Error; err != nil {
		return nil, err
	}
	return &job, nil
}

// GetSkillPublishJobByIdempotencyKey 按幂等键读取已存在的发布任务（INT-141：
// 相同 Idempotency-Key 返回原任务）。不存在返回 gorm.ErrRecordNotFound。
func (s *Storage) GetSkillPublishJobByIdempotencyKey(ctx context.Context, key string) (*model.SkillPublishJob, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var job model.SkillPublishJob
	if err := s.stateDB.WithContext(ctx).Where("idempotency_key = ?", key).First(&job).Error; err != nil {
		return nil, err
	}
	return &job, nil
}

// InsertSkillPublishJob 写入一条技能发布任务，自动填充主键与时间戳。
func (s *Storage) InsertSkillPublishJob(ctx context.Context, job *model.SkillPublishJob) error {
	if s.stateDB == nil {
		return errStateDBNotConfigured
	}
	if job.JobID == "" {
		job.JobID = uuid.New().String()
	}
	now := time.Now()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	if job.UpdatedAt.IsZero() {
		job.UpdatedAt = now
	}
	return s.stateDB.WithContext(ctx).Create(job).Error
}

// ListSkillPublishJobs 返回全部技能发布任务（created_at 升序）。
func (s *Storage) ListSkillPublishJobs(ctx context.Context) ([]model.SkillPublishJob, error) {
	if s.stateDB == nil {
		return nil, errStateDBNotConfigured
	}
	var jobs []model.SkillPublishJob
	err := s.stateDB.WithContext(ctx).Order("created_at ASC").Find(&jobs).Error
	return jobs, err
}
