package model

import (
	"time"
)

// 本文件承载 platform 自有状态库（statedb，默认库名 weknora_gateway_state）的 GORM 模型。
// 表结构由 migrations/ 内嵌 SQL 经 storage.RunMigrations 显式建立，本文件只做列映射；
// 与 WeKnora 库（pkg/model/weknora.go）严格分离，不得互相引用。

// AuditEvent 平台安全审计事件（只追加语义，对应 state 库 audit_events 表，migrations/000002_audit_events）。
// Before/After 承载 JSON 文本（PostgreSQL 中为 jsonb 列），调用方负责序列化。
type AuditEvent struct {
	ID        string    `gorm:"column:id;type:varchar(64);primaryKey" json:"id"`
	Actor     string    `gorm:"column:actor;type:varchar(128);not null;index" json:"actor"`
	Action    string    `gorm:"column:action;type:varchar(255);not null" json:"action"`
	Target    string    `gorm:"column:target;type:varchar(512);not null" json:"target"`
	Before    string    `gorm:"column:before;type:text" json:"before,omitempty"`
	After     string    `gorm:"column:after;type:text" json:"after,omitempty"`
	RequestID string    `gorm:"column:request_id;type:varchar(64);not null" json:"request_id"`
	CreatedAt time.Time `gorm:"column:created_at;not null;index" json:"created_at"`
}

func (AuditEvent) TableName() string {
	return "audit_events"
}

// IdentityLink 身份关联账本（DATA-101a，migrations/000003_identity_links，
// 000015 增加 conflict_evidence）。ImmutableSubject 对应账户中心 SysUser.ID，
// 永不变更；改邮箱不新建行。
type IdentityLink struct {
	ID               string    `gorm:"column:id;type:uuid;primaryKey" json:"id"`
	IdentitySource   string    `gorm:"column:identity_source;type:text;not null" json:"identity_source"`
	OrganizationID   string    `gorm:"column:organization_id;type:text;not null" json:"organization_id"`
	ImmutableSubject string    `gorm:"column:immutable_subject;type:text;not null" json:"immutable_subject"`
	KnowledgeUserID  string    `gorm:"column:knowledge_user_id;type:varchar(36);not null" json:"knowledge_user_id"`
	CodeUserID       *string   `gorm:"column:code_user_id;type:uuid" json:"code_user_id,omitempty"`
	Status           string    `gorm:"column:status;type:text;not null;default:'active'" json:"status"`        // active/conflicted/disabled
	ConflictEvidence *string   `gorm:"column:conflict_evidence;type:jsonb" json:"conflict_evidence,omitempty"` // INT-130：冲突证据 JSON（status=conflicted 时非空）
	CreatedAt        time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt        time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

func (IdentityLink) TableName() string {
	return "identity_links"
}

// AuthTransaction 登录事务（DATA-101b，migrations/000004_auth_transactions），
// 授权码 120s 一次性原子消费的记账载体。
type AuthTransaction struct {
	LoginTxnID        string     `gorm:"column:login_txn_id;type:uuid;primaryKey" json:"login_txn_id"`
	ClientID          string     `gorm:"column:client_id;type:text;not null" json:"client_id"`
	RedirectURI       string     `gorm:"column:redirect_uri;type:text;not null" json:"redirect_uri"`
	StateHash         string     `gorm:"column:state_hash;type:text;not null" json:"state_hash"`
	CodeChallenge     string     `gorm:"column:code_challenge;type:text;not null" json:"code_challenge"`
	DeviceFingerprint string     `gorm:"column:device_fingerprint;type:text;not null" json:"device_fingerprint"`
	CodeHash          *string    `gorm:"column:code_hash;type:text" json:"code_hash,omitempty"`
	IdentityLinkID    *string    `gorm:"column:identity_link_id;type:uuid" json:"identity_link_id,omitempty"`
	Audience          string     `gorm:"column:audience;type:text;not null" json:"audience"`
	ExpiresAt         time.Time  `gorm:"column:expires_at;not null" json:"expires_at"`
	ConsumedAt        *time.Time `gorm:"column:consumed_at" json:"consumed_at,omitempty"`
	Status            string     `gorm:"column:status;type:text;not null;default:'pending'" json:"status"` // pending/completed/failed/expired
	CreatedAt         time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
}

func (AuthTransaction) TableName() string {
	return "auth_transactions"
}

// Session 会话与刷新轮换族（DATA-101c，migrations/000005_sessions，000012 增加复用守卫列）。
// RefreshVerifierHash 只存哈希，不存明文 verifier；PreviousVerifierHash 记录上一轮
// verifier_hash，用于 refresh 复用检测（命中历史值即撤销整个 family）。
// audience=code-backend 的一次性兑换材料也暂存在本表（120s 租约，revoke_reason 记兑换消费）。
type Session struct {
	SessionID            string     `gorm:"column:session_id;type:uuid;primaryKey" json:"session_id"`
	IdentityLinkID       string     `gorm:"column:identity_link_id;type:uuid;not null;index" json:"identity_link_id"`
	FamilyID             string     `gorm:"column:family_id;type:uuid;not null;index" json:"family_id"`
	RefreshVerifierHash  string     `gorm:"column:refresh_verifier_hash;type:text;not null" json:"refresh_verifier_hash"`
	PreviousVerifierHash *string    `gorm:"column:previous_verifier_hash;type:text" json:"previous_verifier_hash,omitempty"`
	DeviceFingerprint    string     `gorm:"column:device_fingerprint;type:text;not null" json:"device_fingerprint"`
	AuthzVersion         int64      `gorm:"column:authz_version;type:bigint;not null;default:0" json:"authz_version"`
	ExpiresAt            time.Time  `gorm:"column:expires_at;not null" json:"expires_at"`
	RevokedAt            *time.Time `gorm:"column:revoked_at" json:"revoked_at,omitempty"`
	RevokeReason         *string    `gorm:"column:revoke_reason;type:text" json:"revoke_reason,omitempty"`
	LastSeenAt           *time.Time `gorm:"column:last_seen_at" json:"last_seen_at,omitempty"`
	CreatedAt            time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt            time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
}

func (Session) TableName() string {
	return "sessions"
}

// RuntimeGrant 运行时授权租约（DATA-101d，migrations/000006_runtime_grants）。
type RuntimeGrant struct {
	GrantID           string     `gorm:"column:grant_id;type:uuid;primaryKey" json:"grant_id"`
	SessionID         *string    `gorm:"column:session_id;type:uuid" json:"session_id,omitempty"`
	TaskID            *string    `gorm:"column:task_id;type:text" json:"task_id,omitempty"`
	WorkspaceID       *string    `gorm:"column:workspace_id;type:text" json:"workspace_id,omitempty"`
	RuntimeID         *string    `gorm:"column:runtime_id;type:text" json:"runtime_id,omitempty"`
	Subject           string     `gorm:"column:subject;type:text;not null" json:"subject"`
	TenantID          string     `gorm:"column:tenant_id;type:text;not null" json:"tenant_id"`
	OriginatingUserID string     `gorm:"column:originating_user_id;type:text;not null" json:"originating_user_id"`
	Capabilities      *string    `gorm:"column:capabilities;type:jsonb" json:"capabilities,omitempty"`
	AuthzVersion      int64      `gorm:"column:authz_version;type:bigint;not null;default:0" json:"authz_version"`
	LeaseExpiresAt    time.Time  `gorm:"column:lease_expires_at;not null" json:"lease_expires_at"`
	RevokedAt         *time.Time `gorm:"column:revoked_at" json:"revoked_at,omitempty"`
	CreatedAt         time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
}

func (RuntimeGrant) TableName() string {
	return "runtime_grants"
}

// WorkspaceLink 身份与 code 工作区关联（DATA-101e，migrations/000007_workspace_links）。
type WorkspaceLink struct {
	IdentityLinkID    string    `gorm:"column:identity_link_id;type:uuid;not null" json:"identity_link_id"`
	CodeWorkspaceID   string    `gorm:"column:code_workspace_id;type:uuid;not null" json:"code_workspace_id"`
	KnowledgeTenantID string    `gorm:"column:knowledge_tenant_id;type:text;not null" json:"knowledge_tenant_id"`
	Source            string    `gorm:"column:source;type:text;not null" json:"source"` // sync/manual
	CreatedAt         time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt         time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

func (WorkspaceLink) TableName() string {
	return "workspace_links"
}

// ManagedEntitlement 受管授权项账本（DATA-110c/BND-112，migrations/000008_managed_entitlements）。
type ManagedEntitlement struct {
	ID           string     `gorm:"column:id;type:uuid;primaryKey" json:"id"`
	Subject      string     `gorm:"column:subject;type:text;not null" json:"subject"`
	TenantID     string     `gorm:"column:tenant_id;type:text;not null" json:"tenant_id"`
	Capability   string     `gorm:"column:capability;type:text;not null" json:"capability"`
	Source       string     `gorm:"column:source;type:text;not null" json:"source"` // sync/manual
	AuthzVersion int64      `gorm:"column:authz_version;type:bigint;not null;default:0" json:"authz_version"`
	RuleVersion  *string    `gorm:"column:rule_version;type:text" json:"rule_version,omitempty"`
	GrantedBy    *string    `gorm:"column:granted_by;type:text" json:"granted_by,omitempty"`
	GrantedAt    *time.Time `gorm:"column:granted_at" json:"granted_at,omitempty"`
	RevokedAt    *time.Time `gorm:"column:revoked_at" json:"revoked_at,omitempty"`
	CreatedAt    time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
}

func (ManagedEntitlement) TableName() string {
	return "managed_entitlements"
}

// AuthzEpoch 授权版本纪元（DATA-101f，migrations/000014_authz_epochs，TASK-P06 版本源统一）。
// 每个 subject 一行单调递增版本：任何 entitlement 变更、identity 停用、revoke 都在同一事务里 +1。
// grant 签发/续期把当前版本写入 runtime_grants.authz_version，续期时版本不一致即视为已撤销。
type AuthzEpoch struct {
	Subject   string    `gorm:"column:subject;type:text;primaryKey" json:"subject"`
	Version   int64     `gorm:"column:version;type:bigint;not null;default:0" json:"version"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

func (AuthzEpoch) TableName() string {
	return "authz_epochs"
}

// SyncRun 同步运行账本（DATA-110a，migrations/000009_sync_runs）。
type SyncRun struct {
	RunID          string     `gorm:"column:run_id;type:uuid;primaryKey" json:"run_id"`
	Scope          string     `gorm:"column:scope;type:text;not null" json:"scope"` // full/department
	DepartmentCode *string    `gorm:"column:department_code;type:text" json:"department_code,omitempty"`
	IdempotencyKey string     `gorm:"column:idempotency_key;type:text;not null;uniqueIndex" json:"idempotency_key"`
	RuleVersion    string     `gorm:"column:rule_version;type:text;not null" json:"rule_version"`
	TriggeredBy    string     `gorm:"column:triggered_by;type:text;not null" json:"triggered_by"`
	Status         string     `gorm:"column:status;type:text;not null;index" json:"status"` // queued/snapshotting/validating/diffing/applying/reconciling/succeeded/partial_failed/failed
	SnapshotDigest *string    `gorm:"column:snapshot_digest;type:text" json:"snapshot_digest,omitempty"`
	SnapshotStats  *string    `gorm:"column:snapshot_stats;type:jsonb" json:"snapshot_stats,omitempty"`
	LockUntil      *time.Time `gorm:"column:lock_until" json:"lock_until,omitempty"`
	StartedAt      *time.Time `gorm:"column:started_at" json:"started_at,omitempty"`
	FinishedAt     *time.Time `gorm:"column:finished_at" json:"finished_at,omitempty"`
	CreatedAt      time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
}

func (SyncRun) TableName() string {
	return "sync_runs"
}

// SyncStep 同步步骤明细（DATA-110b，migrations/000010_sync_steps），主键 (run_id, step_seq)。
type SyncStep struct {
	RunID          string    `gorm:"column:run_id;type:uuid;primaryKey" json:"run_id"`
	StepSeq        int       `gorm:"column:step_seq;type:int;primaryKey" json:"step_seq"`
	StepType       string    `gorm:"column:step_type;type:text;not null" json:"step_type"` // snapshot/validate/diff/provision/revoke/reconcile
	SubjectRef     *string   `gorm:"column:subject_ref;type:text" json:"subject_ref,omitempty"`
	IdempotencyKey *string   `gorm:"column:idempotency_key;type:text" json:"idempotency_key,omitempty"`
	Status         string    `gorm:"column:status;type:text;not null;default:'pending'" json:"status"` // pending/applied/failed/skipped
	Error          *string   `gorm:"column:error;type:text" json:"error,omitempty"`
	Retryable      bool      `gorm:"column:retryable;type:boolean;not null;default:true" json:"retryable"`
	CreatedAt      time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

func (SyncStep) TableName() string {
	return "sync_steps"
}

// SkillPublishJob 技能跨空间发布任务记账（DATA-120b，migrations/000011_skill_publish_jobs）。
// platform 只协调与记账，内容与分发权限裁定在 knowledge。
type SkillPublishJob struct {
	JobID          string    `gorm:"column:job_id;type:uuid;primaryKey" json:"job_id"`
	ReleaseID      string    `gorm:"column:release_id;type:text;not null" json:"release_id"`
	SourceTenant   string    `gorm:"column:source_tenant;type:text;not null" json:"source_tenant"`
	Targets        string    `gorm:"column:targets;type:jsonb;not null" json:"targets"`
	IdempotencyKey string    `gorm:"column:idempotency_key;type:text;not null;uniqueIndex" json:"idempotency_key"`
	Status         string    `gorm:"column:status;type:text;not null" json:"status"`
	Steps          *string   `gorm:"column:steps;type:jsonb" json:"steps,omitempty"`
	CreatedAt      time.Time `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at;not null" json:"updated_at"`
}

func (SkillPublishJob) TableName() string {
	return "skill_publish_jobs"
}
