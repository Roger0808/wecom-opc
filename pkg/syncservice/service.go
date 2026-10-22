package syncservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/accountcenter"
	"weknora-tokendance/pkg/authz"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"
	"weknora-tokendance/pkg/syncer"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// defaultLockTTL 持久化互斥租约时长（heartbeat 每 TTL/3 续期，崩溃后 TTL 过期可被接管）。
const defaultLockTTL = 2 * time.Minute

// maxShrinkRatio 较上次成功 run 统计缩减超过该比例即判定快照不可信（ validating 失败）。
const maxShrinkRatio = 0.5

// Service 手动同步服务（INT-130）。全部账本读写落在平台状态库；
// knowledge 侧开户/停用复用既有 pkg/syncer + pkg/storage 直写（P09 再隔离）。
type Service struct {
	cfg     *config.Config
	ac      *accountcenter.Client
	storage *storage.Storage
	syncer  *syncer.Syncer
	revoker authz.TokenRevoker
	lockTTL time.Duration
}

// Option 覆盖默认组装（测试注入 FakeClock / 撤销器等）。
type Option func(*Service)

// WithRevoker 注入上游 token 撤销器（默认 Noop）。
func WithRevoker(r authz.TokenRevoker) Option {
	return func(s *Service) {
		if r != nil {
			s.revoker = r
		}
	}
}

// WithLockTTL 覆盖互斥租约时长（测试可缩短）。
func WithLockTTL(d time.Duration) Option {
	return func(s *Service) {
		if d > 0 {
			s.lockTTL = d
		}
	}
}

// NewService 构造同步服务。
func NewService(cfg *config.Config, ac *accountcenter.Client, s *storage.Storage, sy *syncer.Syncer, opts ...Option) *Service {
	svc := &Service{
		cfg:     cfg,
		ac:      ac,
		storage: s,
		syncer:  sy,
		revoker: authz.NoopTokenRevoker{},
		lockTTL: defaultLockTTL,
	}
	for _, opt := range opts {
		opt(svc)
	}
	return svc
}

func (s *Service) orgID() string {
	if s.cfg != nil && s.cfg.WeCom.CorpID != "" {
		return s.cfg.WeCom.CorpID
	}
	return "default"
}

// ---------- 快照 ----------

type snapshot struct {
	Departments []accountcenter.SysDepartment `json:"-"`
	Users       []accountcenter.SysUser       `json:"-"`
	DeptMap     map[string]accountcenter.SysDepartment
	TargetDepts map[string]accountcenter.SysDepartment
	TargetDept  *accountcenter.SysDepartment // department scope 的目标部门
	Digest      string
	Stats       snapshotStats
}

type snapshotStats struct {
	DepartmentsTotal   int `json:"departments_total"`
	DepartmentsInScope int `json:"departments_in_scope"`
	MembersTotal       int `json:"members_total"`
	MembersInScope     int `json:"members_in_scope"`
	PlannedProvision   int `json:"planned_provision"`
	PlannedRevoke      int `json:"planned_revoke"`
	Conflicts          int `json:"conflicts"`
}

// canonicalDigest 对快照做规范化（排序）后取 sha256，作为 snapshot_digest 落 run 行。
func canonicalDigest(depts []accountcenter.SysDepartment, users []accountcenter.SysUser) string {
	type canonDept struct {
		Code       string `json:"code"`
		ParentCode string `json:"parent_code"`
		Name       string `json:"name"`
		Level      int    `json:"level"`
	}
	type canonUser struct {
		Subject        string `json:"subject"`
		DepartmentCode string `json:"department_code"`
		UserName       string `json:"user_name"`
		UnionNo        string `json:"union_no"`
	}
	cd := make([]canonDept, 0, len(depts))
	for _, d := range depts {
		cd = append(cd, canonDept{Code: d.DepartmentCode, ParentCode: d.ParentCode, Name: d.DepartmentName, Level: d.DepartmentLevel})
	}
	sort.Slice(cd, func(i, j int) bool { return cd[i].Code < cd[j].Code })
	cu := make([]canonUser, 0, len(users))
	for _, u := range users {
		cu = append(cu, canonUser{Subject: u.ID, DepartmentCode: u.DepartmentCode, UserName: u.UserName, UnionNo: u.UnionNo})
	}
	sort.Slice(cu, func(i, j int) bool { return cu[i].Subject < cu[j].Subject })
	payload, _ := json.Marshal(struct {
		Departments []canonDept `json:"departments"`
		Members     []canonUser `json:"members"`
	}{cd, cu})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// normalizeSnapshotErr 把账户中心分页对账失败统一转为 SYNC_SNAPSHOT_INCOMPLETE 语义码。
func normalizeSnapshotErr(err error) error {
	if errors.Is(err, accountcenter.ErrSnapshotIncomplete) {
		return fmt.Errorf("%w: %v", ErrSnapshotIncomplete, err)
	}
	return err
}

func buildDeptMap(depts []accountcenter.SysDepartment) (map[string]accountcenter.SysDepartment, map[string]accountcenter.SysDepartment) {
	deptMap := make(map[string]accountcenter.SysDepartment, len(depts))
	targets := make(map[string]accountcenter.SysDepartment)
	for _, d := range depts {
		deptMap[d.DepartmentCode] = d
		if d.DepartmentLevel == 1 {
			targets[d.DepartmentCode] = d
		}
	}
	return deptMap, targets
}

// fetchSnapshot 按 scope 拉取账户中心快照。
// full：分页全量部门 + 全量在职成员；department：分页全量部门（父链存在性校验）
// + 按 departmentCode 过滤的在职成员。分页与聚合计数对账不符由客户端返回
// ErrSnapshotIncomplete，此处原样上抛转为运行失败。
func (s *Service) fetchSnapshot(ctx context.Context, scope Scope, departmentCode string) (*snapshot, error) {
	snap := &snapshot{}

	if scope == ScopeFull {
		depts, err := s.ac.FetchActiveDepartmentsPaged(ctx)
		if err != nil {
			return nil, normalizeSnapshotErr(err)
		}
		users, err := s.ac.FetchActiveUsersPaged(ctx, "")
		if err != nil {
			return nil, normalizeSnapshotErr(err)
		}
		snap.Departments = depts
		snap.Users = users
	} else {
		// department scope：分页拉全量部门树（父链与 level-1 空间解析用），
		// 与 full 同一分页/聚合对账协议，计数不符即 ErrSnapshotIncomplete
		depts, err := s.ac.FetchActiveDepartmentsPaged(ctx)
		if err != nil {
			return nil, normalizeSnapshotErr(err)
		}
		snap.Departments = depts
		users, err := s.ac.FetchActiveUsersPaged(ctx, departmentCode)
		if err != nil {
			return nil, normalizeSnapshotErr(err)
		}
		snap.Users = users
	}

	snap.DeptMap, snap.TargetDepts = buildDeptMap(snap.Departments)

	// department scope：目标部门必须存在，且父链各级都在快照内（存在性校验）
	if scope == ScopeDepartment {
		target, ok := snap.DeptMap[departmentCode]
		if !ok {
			return nil, fmt.Errorf("%w: department %q not found in snapshot", ErrSnapshotIncomplete, departmentCode)
		}
		for curr := target; ; {
			parent := curr.ParentCode
			if parent == "" {
				break
			}
			p, ok := snap.DeptMap[parent]
			if !ok {
				return nil, fmt.Errorf("%w: department %q parent %q missing", ErrSnapshotIncomplete, curr.DepartmentCode, parent)
			}
			curr = p
		}
		snap.TargetDept = &target
	}

	snap.Stats.DepartmentsTotal = len(snap.Departments)
	snap.Stats.MembersTotal = len(snap.Users)
	if scope == ScopeDepartment {
		snap.Stats.DepartmentsInScope = 1
		snap.Stats.MembersInScope = len(snap.Users)
		// digest 覆盖目标部门及其父链 + 在范围内的成员
		chain := []accountcenter.SysDepartment{*snap.TargetDept}
		for curr := *snap.TargetDept; curr.ParentCode != ""; {
			p := snap.DeptMap[curr.ParentCode]
			chain = append(chain, p)
			curr = p
		}
		snap.Digest = canonicalDigest(chain, snap.Users)
	} else {
		snap.Stats.DepartmentsInScope = len(snap.Departments)
		snap.Stats.MembersInScope = len(snap.Users)
		snap.Digest = canonicalDigest(snap.Departments, snap.Users)
	}
	return snap, nil
}

// ---------- 差异 ----------

// action 一步供给/收回计划。幂等键 = run_id + step_type + subject_ref。
type action struct {
	stepType        string
	subject         string
	user            *accountcenter.SysUser // provision：待供给成员
	deptCode        string                 // provision：目标空间部门编码（level-1）
	entitlementID   string                 // revoke：目标授权项
	tenantID        string                 // revoke：授权项所在 tenant
	knowledgeUserID string                 // revoke：knowledge 侧用户
	// depart=true 表示非在职收回：附加撤销上游 token + 停用 knowledge 用户；
	// false 为调岗收回（收旧 entitlement，不动登录态）。
	// department scope 的快照无法区分调岗与离职，一律按调岗处理，停用只由 full run 判定。
	depart bool
}

// diff 对照 identity_links 与 managed_entitlements（source=sync）得出供给/收回计划。
func (s *Service) diff(ctx context.Context, scope Scope, departmentCode string, snap *snapshot) ([]action, error) {
	links, err := s.storage.ListIdentityLinks(ctx)
	if err != nil {
		return nil, err
	}
	linksBySubject := make(map[string][]model.IdentityLink, len(links))
	for _, l := range links {
		linksBySubject[l.ImmutableSubject] = append(linksBySubject[l.ImmutableSubject], l)
	}
	activeLink := func(subject string) *model.IdentityLink {
		for i := range linksBySubject[subject] {
			if linksBySubject[subject][i].Status == linkActive {
				return &linksBySubject[subject][i]
			}
		}
		return nil
	}

	var ents []model.ManagedEntitlement
	departKind := true
	if scope == ScopeDepartment {
		// department scope 的快照无法区分调岗与离职：收回一律按调岗处理（不停用账号），
		// 收回候选只出自本部门空间的授权项
		departKind = false
		tenantID := ""
		if snap.TargetDept != nil {
			if mapping, err := s.storage.FindTenantByDeptCode(ctx, departmentCode); err == nil && mapping != nil {
				tenantID = strconv.FormatUint(mapping.TenantID, 10)
			}
		}
		if tenantID != "" {
			ents, err = s.storage.ListActiveSyncSourceEntitlementsByTenant(ctx, tenantID)
		}
	} else {
		ents, err = s.storage.ListActiveSyncSourceEntitlements(ctx)
	}
	if err != nil {
		return nil, err
	}

	inSnapshot := make(map[string]bool, len(snap.Users))
	var actions []action
	revokedTenants := make(map[string]bool) // (subject|tenant) 已计划收回，避免重复

	// 供给/调岗：遍历快照成员
	for i := range snap.Users {
		u := &snap.Users[i]
		inSnapshot[u.ID] = true

		target := syncer.FindTargetDepartmentInMap(u.DepartmentCode, snap.DeptMap, snap.TargetDepts)
		if target == nil {
			continue // 无法解析空间部门：跳过（记录为 skipped 步骤）
		}

		desiredTenant := ""
		if mapping, err := s.storage.FindTenantByDeptCode(ctx, target.DepartmentCode); err == nil && mapping != nil {
			desiredTenant = strconv.FormatUint(mapping.TenantID, 10)
		}

		// 已有同 tenant 的未撤销 sync 授权 → 已在同步内
		already := false
		for _, e := range ents {
			if e.Subject == u.ID && desiredTenant != "" && e.TenantID == desiredTenant {
				already = true
				break
			}
		}
		if already {
			continue
		}

		// 调岗收回：该主体其他 tenant 的未撤销 sync 授权（含目标 tenant 映射尚不存在的场景）
		for _, e := range ents {
			if e.Subject != u.ID {
				continue
			}
			key := e.Subject + "|" + e.TenantID
			if revokedTenants[key] {
				continue
			}
			if desiredTenant != "" && e.TenantID == desiredTenant {
				continue
			}
			revokedTenants[key] = true
			kuid := e.Subject
			if l := activeLink(u.ID); l != nil {
				kuid = l.KnowledgeUserID
			}
			actions = append(actions, action{
				stepType: StepRevoke, subject: u.ID,
				entitlementID: e.ID, tenantID: e.TenantID, knowledgeUserID: kuid,
				depart: false, // 快照内成员的收回 = 调岗收旧，不停用账号
			})
		}

		actions = append(actions, action{
			stepType: StepProvision, subject: u.ID, user: u, deptCode: target.DepartmentCode,
		})
	}

	// 停用收回：有未撤销 sync 授权但不在快照内（full=全量在职；department=本部门空间）
	for _, e := range ents {
		if inSnapshot[e.Subject] {
			continue
		}
		key := e.Subject + "|" + e.TenantID
		if revokedTenants[key] {
			continue
		}
		revokedTenants[key] = true
		kuid := e.Subject
		if l := activeLink(e.Subject); l != nil {
			kuid = l.KnowledgeUserID
		}
		actions = append(actions, action{
			stepType: StepRevoke, subject: e.Subject,
			entitlementID: e.ID, tenantID: e.TenantID, knowledgeUserID: kuid,
			depart: departKind,
		})
	}

	sort.SliceStable(actions, func(i, j int) bool {
		if actions[i].stepType != actions[j].stepType {
			return actions[i].stepType < actions[j].stepType
		}
		return actions[i].subject < actions[j].subject
	})
	return actions, nil
}

// ---------- 运行编排 ----------

// Run 执行一次同步运行（同步阻塞至终态）。幂等：idempotency_key 命中既有 run
// 直接返回原 run_id；互斥：同 scope+department_code 持锁运行中 → ErrConflict。
func (s *Service) Run(ctx context.Context, scope, departmentCode, idempotencyKey, triggeredBy, ruleVersion string) (string, error) {
	if err := validateParams(scope, departmentCode, idempotencyKey, ruleVersion); err != nil {
		return "", err
	}

	now := time.Now()
	lockUntil := now.Add(s.lockTTL)
	run := &model.SyncRun{
		RunID:          uuid.New().String(),
		Scope:          string(scope),
		DepartmentCode: nil,
		IdempotencyKey: idempotencyKey,
		RuleVersion:    ruleVersion,
		TriggeredBy:    triggeredBy,
		Status:         StatusQueued,
		LockUntil:      &lockUntil,
	}
	if Scope(scope) == ScopeDepartment {
		run.DepartmentCode = &departmentCode
	}

	// 持久化互斥与幂等重放收敛到同一事务：先 touch 在跑行（行锁串行化冲突判定，
	// READ COMMITTED 下后提交者重看到已提交持锁行）→ 接管过期锁 → 同事务内查幂等键
	// （并发同键串行化后必见原 run，命中即重放）→ 冲突检查 → 插入。
	// sync_runs.idempotency_key 唯一索引兜底并发同键插入，撞键时回查返回原 run。
	var replay *model.SyncRun
	err := s.storage.StateDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := s.storage.TouchSyncRunLocksTx(tx, string(scope), departmentCode, now); err != nil {
			return err
		}
		if _, err := s.storage.FailStaleSyncRunsTx(tx, string(scope), departmentCode, now); err != nil {
			return err
		}
		if existing, err := s.storage.FindSyncRunByIdempotencyKeyTx(tx, idempotencyKey); err == nil {
			replay = existing
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		conflicts, err := s.storage.CountLockedRunningSyncRunsTx(tx, string(scope), departmentCode, now, "")
		if err != nil {
			return err
		}
		if conflicts > 0 {
			return ErrConflict
		}
		return s.storage.InsertSyncRunTx(tx, run)
	})
	if replay != nil {
		return replay.RunID, nil
	}
	if err != nil {
		if errors.Is(err, ErrConflict) {
			return "", ErrConflict
		}
		// 并发同键插入撞唯一索引：回查返回先提交的原 run（INT-130a：同键返回原运行）
		if existing, qerr := s.storage.FindSyncRunByIdempotencyKey(ctx, idempotencyKey); qerr == nil && existing != nil {
			return existing.RunID, nil
		}
		return "", err
	}

	if err := s.execute(ctx, run, "", false); err != nil {
		return run.RunID, err
	}
	return run.RunID, nil
}

// Retry 重放 failed/partial_failed run：重新拉快照（快照不落盘，只能重取），
// 对未 applied/skipped 的步骤按幂等键去重重放，成功后步骤转 applied 并重算 run 状态。
func (s *Service) Retry(ctx context.Context, runID string) (string, error) {
	run, err := s.storage.GetSyncRun(ctx, runID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", ErrNotFound
		}
		return "", err
	}
	if run.Status != StatusFailed && run.Status != StatusPartialFailed {
		return "", fmt.Errorf("%w: status=%s", ErrNotRetryable, run.Status)
	}

	now := time.Now()
	lockUntil := now.Add(s.lockTTL)
	deptCode := ""
	if run.DepartmentCode != nil {
		deptCode = *run.DepartmentCode
	}

	// 互斥：同 scope+department 持锁运行中（含本 run 之外的并发 run）→ 冲突
	err = s.storage.StateDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := s.storage.TouchSyncRunLocksTx(tx, run.Scope, deptCode, now); err != nil {
			return err
		}
		conflicts, err := s.storage.CountLockedRunningSyncRunsTx(tx, run.Scope, deptCode, now, run.RunID)
		if err != nil {
			return err
		}
		if conflicts > 0 {
			return ErrConflict
		}
		_, err = s.storage.UpdateSyncRunTx(tx, run.RunID, map[string]interface{}{
			"status":     StatusSnapshotting,
			"lock_until": lockUntil,
			"updated_at": now,
		})
		return err
	})
	if err != nil {
		if errors.Is(err, ErrConflict) {
			return "", ErrConflict
		}
		return "", err
	}
	run.Status = StatusSnapshotting
	run.LockUntil = &lockUntil

	if err := s.execute(ctx, run, "", true); err != nil {
		return "", err
	}
	final, err := s.storage.GetSyncRun(ctx, runID)
	if err != nil {
		return "", err
	}
	return final.Status, nil
}

func validateParams(scope, departmentCode, idempotencyKey, ruleVersion string) error {
	switch Scope(scope) {
	case ScopeFull:
		if departmentCode != "" {
			return fmt.Errorf("%w: department_code only allowed for scope=department", ErrInvalidRequest)
		}
	case ScopeDepartment:
		if departmentCode == "" {
			return fmt.Errorf("%w: department_code is required for scope=department", ErrInvalidRequest)
		}
	default:
		return fmt.Errorf("%w: scope must be full|department", ErrInvalidRequest)
	}
	if idempotencyKey == "" {
		return fmt.Errorf("%w: idempotency_key is required", ErrInvalidRequest)
	}
	if ruleVersion == "" {
		return fmt.Errorf("%w: rule_version is required", ErrInvalidRequest)
	}
	return nil
}

// execute 从当前 run 状态推进状态机至终态。sweepFailed 供 Retry 使用：计划重放后
// 对仍 failed/retryable 的步骤做孤儿回收重放（如收回已落库但撤销器失败的步骤）。
//
// 注意：快照不落盘（只留 digest/stats），Retry 必须重取快照、重算差异，
// 因此快照/差异的数据推导无条件执行；sync_steps 记账只在步骤未 applied 时落
// （applied 的步骤保留首次结论，不重复判定）。
func (s *Service) execute(ctx context.Context, run *model.SyncRun, _ string, sweepFailed bool) error {
	stopBeat := s.heartbeat(ctx, run.RunID)
	defer stopBeat()

	deptCode := ""
	if run.DepartmentCode != nil {
		deptCode = *run.DepartmentCode
	}

	// snapshotting：快照是 validating/diffing 的输入，每次执行都重取
	s.setRunStatus(ctx, run, StatusSnapshotting)
	snap, err := s.fetchSnapshot(ctx, Scope(run.Scope), deptCode)
	if err == nil {
		stats, _ := json.Marshal(snap.Stats)
		_, err = s.storage.UpdateSyncRunTx(s.freshTx(ctx), run.RunID, map[string]interface{}{
			"snapshot_digest": snap.Digest,
			"snapshot_stats":  string(stats),
		})
	}
	if berr := s.recordStageStep(ctx, run, 1, StepSnapshot, true, err); berr != nil {
		return berr
	}
	if err != nil {
		return s.failRun(ctx, run, err)
	}

	// validating：守卫与步骤记账解耦——空快照/缩减 >50% 守卫对每次执行重取的新快照必跑
	// （Retry 不允许凭首跑结论跳过守卫，退化快照必须整运行失败关闭）；
	// recordStageStep 内部保持幂等（已 applied 的记账不覆盖，守卫结果仍会拦截运行）。
	s.setRunStatus(ctx, run, StatusValidating)
	err = s.validateSnapshot(ctx, run, snap)
	if berr := s.recordStageStep(ctx, run, 2, StepValidate, false, err); berr != nil {
		return berr
	}
	if err != nil {
		return s.failRun(ctx, run, err)
	}

	// diffing：差异计划每次重算（动作级幂等保证重放安全）
	s.setRunStatus(ctx, run, StatusDiffing)
	actions, err := s.diff(ctx, Scope(run.Scope), deptCode, snap)
	if err == nil {
		snap.Stats.PlannedProvision, snap.Stats.PlannedRevoke = 0, 0
		for _, a := range actions {
			if a.stepType == StepProvision {
				snap.Stats.PlannedProvision++
			} else {
				snap.Stats.PlannedRevoke++
			}
		}
		stats, _ := json.Marshal(snap.Stats)
		_, err = s.storage.UpdateSyncRunTx(s.freshTx(ctx), run.RunID, map[string]interface{}{
			"snapshot_stats": string(stats),
		})
	}
	if berr := s.recordStageStep(ctx, run, 3, StepDiff, true, err); berr != nil {
		return berr
	}
	if err != nil {
		return s.failRun(ctx, run, err)
	}

	// applying（部分失败不中断，继续后续步骤）
	s.setRunStatus(ctx, run, StatusApplying)
	s.applyAll(ctx, run, snap, actions)
	if sweepFailed {
		s.sweepFailedSteps(ctx, run, snap)
	}

	// reconciling
	s.setRunStatus(ctx, run, StatusReconciling)
	return s.reconcile(ctx, run)
}

// recordStageStep 落元步骤（snapshot/validate/diff）执行结果：步骤不存在先建 pending 行；
// 已 applied 不覆盖；否则按执行结果转 applied/failed（error + retryable）。
func (s *Service) recordStageStep(ctx context.Context, run *model.SyncRun, seq int, stepType string, retryable bool, execErr error) error {
	step, err := s.storage.GetSyncStep(ctx, run.RunID, seq)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		step = &model.SyncStep{
			RunID: run.RunID, StepSeq: seq, StepType: stepType,
			Status: "pending", Retryable: retryable,
		}
		if err := s.storage.InsertSyncStepTx(s.freshTx(ctx), step); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if step.Status == "applied" {
		return nil
	}
	now := time.Now()
	if execErr == nil {
		_, err = s.storage.UpdateSyncStepTx(s.freshTx(ctx), run.RunID, seq, map[string]interface{}{
			"status": "applied", "error": nil, "updated_at": now,
		})
		return err
	}
	msg := execErr.Error()
	_, err = s.storage.UpdateSyncStepTx(s.freshTx(ctx), run.RunID, seq, map[string]interface{}{
		"status": "failed", "error": msg, "retryable": retryable, "updated_at": now,
	})
	return err
}

// freshTx 开一个即时提交的小事务承载单条 UPDATE（阶段间状态迁移）。
func (s *Service) freshTx(ctx context.Context) *gorm.DB {
	return s.storage.StateDB().WithContext(ctx)
}

func (s *Service) setRunStatus(ctx context.Context, run *model.SyncRun, status string) {
	now := time.Now()
	updates := map[string]interface{}{"status": status, "updated_at": now}
	if status == StatusSnapshotting && run.StartedAt == nil {
		updates["started_at"] = now
		run.StartedAt = &now
	}
	if _, err := s.storage.UpdateSyncRunTx(s.freshTx(ctx), run.RunID, updates); err != nil {
		log.Printf("[SyncService] 更新 run 状态失败 [%s -> %s]: %v", run.RunID, status, err)
	}
	run.Status = status
}

// validateSnapshot 空快照（0 部门或 0 成员）或较上次成功 run 统计缩减 >50% → 失败关闭。
func (s *Service) validateSnapshot(ctx context.Context, run *model.SyncRun, snap *snapshot) error {
	if snap.Stats.DepartmentsInScope == 0 || snap.Stats.MembersInScope == 0 {
		return fmt.Errorf("%w: empty snapshot (departments=%d members=%d)",
			ErrSnapshotIncomplete, snap.Stats.DepartmentsInScope, snap.Stats.MembersInScope)
	}

	last, err := s.lastSucceededRun(ctx, run)
	if err != nil {
		return err
	}
	if last == nil || last.SnapshotStats == nil {
		return nil
	}
	var prev snapshotStats
	if err := json.Unmarshal([]byte(*last.SnapshotStats), &prev); err != nil {
		return nil // 历史统计不可解析：不做缩减判定
	}
	if prev.MembersInScope > 0 && float64(snap.Stats.MembersInScope) < float64(prev.MembersInScope)*maxShrinkRatio {
		return fmt.Errorf("%w: member count shrank %.0f%% (prev=%d curr=%d)",
			ErrSnapshotIncomplete, 100*maxShrinkRatio, prev.MembersInScope, snap.Stats.MembersInScope)
	}
	if prev.DepartmentsInScope > 0 && float64(snap.Stats.DepartmentsInScope) < float64(prev.DepartmentsInScope)*maxShrinkRatio {
		return fmt.Errorf("%w: department count shrank %.0f%% (prev=%d curr=%d)",
			ErrSnapshotIncomplete, 100*maxShrinkRatio, prev.DepartmentsInScope, snap.Stats.DepartmentsInScope)
	}
	return nil
}

// lastSucceededRun 取同 scope+department_code 最近的成功 run（不含自身）。
func (s *Service) lastSucceededRun(ctx context.Context, run *model.SyncRun) (*model.SyncRun, error) {
	q := s.storage.StateDB().WithContext(ctx).
		Where("scope = ? AND status = ? AND run_id <> ?", run.Scope, StatusSucceeded, run.RunID)
	if run.DepartmentCode != nil {
		q = q.Where("department_code = ?", *run.DepartmentCode)
	} else {
		q = q.Where("department_code IS NULL")
	}
	var last model.SyncRun
	err := q.Order("finished_at DESC").First(&last).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &last, nil
}

// failRun 把运行置为 failed 并释放锁（snapshotting/validating 失败路径，无差异产生）。
func (s *Service) failRun(ctx context.Context, run *model.SyncRun, cause error) error {
	now := time.Now()
	if _, err := s.storage.UpdateSyncRunTx(s.freshTx(ctx), run.RunID, map[string]interface{}{
		"status": StatusFailed, "finished_at": now, "lock_until": nil, "updated_at": now,
	}); err != nil {
		return err
	}
	run.Status = StatusFailed
	log.Printf("[SyncService] run %s failed: %v", run.RunID, cause)
	return nil // 运行失败是已记账的终态，不再向上抛
}

// heartbeat 每 TTL/3 续期 lock_until，直至运行到达终态（停止信号由调用方触发）。
func (s *Service) heartbeat(ctx context.Context, runID string) func() {
	interval := s.lockTTL / 3
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		running := storage.SyncRunRunningStatuses
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := time.Now()
				until := now.Add(s.lockTTL)
				if err := s.storage.StateDB().WithContext(ctx).Exec(
					`UPDATE sync_runs SET lock_until = ?, updated_at = ?
					 WHERE run_id = ? AND status IN `+running,
					until, now, runID,
				).Error; err != nil {
					log.Printf("[SyncService] heartbeat 续期失败 [%s]: %v", runID, err)
				}
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}
