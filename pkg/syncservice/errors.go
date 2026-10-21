// Package syncservice 实现 TASK-P08（INT-130、BND-130/131、REQ-006、ACC-006）手动同步服务：
// 账户中心组织架构快照 → 校验 → 差异 → 逐步供给/收回 → 对账的同步运行状态机。
//
// 互斥（BND-130）：sync_runs 持久化锁（同 scope+department_code 运行中且 lock_until>now
// 即 SYNC_RUN_CONFLICT；运行中进程崩溃后锁过期可被新 run 接管）。进程内 isSyncing 语义
// 不再参与互斥。
//
// 幂等：idempotency_key 命中既有 run 直接返回原 run_id；步骤幂等键
// run_id+step_type+subject_ref 唯一，重放/重试均按行去重；
// provision/revoke 动作本身按「先查后写」实现天然幂等。
package syncservice

import "errors"

// Scope 同步范围。
type Scope string

const (
	// ScopeFull 全量：全部启用部门与全部在职成员。
	ScopeFull Scope = "full"
	// ScopeDepartment 单部门：指定 departmentCode 的部门与直属在职成员。
	ScopeDepartment Scope = "department"
)

// sync_runs.status 状态机：
// queued → snapshotting → validating → diffing → applying → reconciling → succeeded；
// snapshotting/validating 失败 → failed（无差异产生）；
// applying/reconciling 存在未竟步骤 → partial_failed（保留已应用与可重试项）。
const (
	StatusQueued        = "queued"
	StatusSnapshotting  = "snapshotting"
	StatusValidating    = "validating"
	StatusDiffing       = "diffing"
	StatusApplying      = "applying"
	StatusReconciling   = "reconciling"
	StatusSucceeded     = "succeeded"
	StatusPartialFailed = "partial_failed"
	StatusFailed        = "failed"
)

// sync_steps.step_type 取值。
const (
	StepSnapshot  = "snapshot"
	StepValidate  = "validate"
	StepDiff      = "diff"
	StepProvision = "provision"
	StepRevoke    = "revoke"
	StepReconcile = "reconcile"
)

// identity_links 账本写入常量。
const (
	identitySource = "wecom"
	linkActive     = "active"
	linkConflicted = "conflicted"
	linkDisabled   = "disabled"
	// entitlementCapability 同步供给的成员能力项（managed_entitlements.capability）。
	entitlementCapability = "member"
)

// 错误码（BND-180 枚举扩展，管理接口按码映射 HTTP 状态）。
var (
	// ErrInvalidRequest 参数校验失败（400 INVALID_REQUEST）。
	ErrInvalidRequest = errors.New("SYNC_INVALID_REQUEST")
	// ErrConflict 同 scope+department_code 存在持锁运行中的 run（409 SYNC_RUN_CONFLICT）。
	ErrConflict = errors.New("SYNC_RUN_CONFLICT")
	// ErrNotFound run 不存在（404）。
	ErrNotFound = errors.New("SYNC_RUN_NOT_FOUND")
	// ErrNotRetryable run 不处于 failed/partial_failed，不允许重放（400）。
	ErrNotRetryable = errors.New("SYNC_RUN_NOT_RETRYABLE")
	// ErrSnapshotIncomplete 快照不完整（空快照 / 分页与计数对账不符 / 父链缺失），
	// 运行转入 failed 关闭（语义同账户中心 ErrSnapshotIncomplete）。
	ErrSnapshotIncomplete = errors.New("SYNC_SNAPSHOT_INCOMPLETE")
)
