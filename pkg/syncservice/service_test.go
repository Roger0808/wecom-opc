package syncservice_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/accountcenter"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"
	"weknora-tokendance/pkg/syncer"
	"weknora-tokendance/pkg/syncservice"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ---------- 假统一账户中心（httptest + 真实 accountcenter.Client，分页/聚合/单查协议） ----------

type fakeAccountCenter struct {
	mu             sync.Mutex
	depts          map[string]accountcenter.SysDepartment
	users          map[string]accountcenter.SysUser
	aggregateDelta int // 聚合计数偏移：模拟分页中断/计数不符

	gate     chan struct{} // 非 nil 时 sysUsers 分页请求阻塞（并发互斥测试用）
	sawUsers chan struct{} // 首次 sysUsers 分页请求信号
	sawOnce  sync.Once

	server *httptest.Server
}

func newFakeAccountCenter(t *testing.T) *fakeAccountCenter {
	t.Helper()
	f := &fakeAccountCenter{
		depts: map[string]accountcenter.SysDepartment{},
		users: map[string]accountcenter.SysUser{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeAccountCenter) client() *accountcenter.Client {
	return accountcenter.NewClient(f.server.URL)
}

func (f *fakeAccountCenter) setDepts(depts ...accountcenter.SysDepartment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.depts = map[string]accountcenter.SysDepartment{}
	for _, d := range depts {
		f.depts[d.DepartmentCode] = d
	}
}

func (f *fakeAccountCenter) setUsers(users ...accountcenter.SysUser) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users = map[string]accountcenter.SysUser{}
	for _, u := range users {
		f.users[u.ID] = u
	}
}

func (f *fakeAccountCenter) removeUser(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.users, id)
}

func (f *fakeAccountCenter) moveUser(id, deptCode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.users[id]
	u.DepartmentCode = deptCode
	f.users[id] = u
}

func (f *fakeAccountCenter) activeDepts() []accountcenter.SysDepartment {
	out := make([]accountcenter.SysDepartment, 0, len(f.depts))
	for _, d := range f.depts {
		if d.DepartmentStatus == 1 {
			out = append(out, d)
		}
	}
	return out
}

func (f *fakeAccountCenter) activeUsers(deptCode string) []accountcenter.SysUser {
	out := make([]accountcenter.SysUser, 0, len(f.users))
	for _, u := range f.users {
		if u.UserStatus != 1 || u.IsDeleted != 0 {
			continue
		}
		if deptCode != "" && u.DepartmentCode != deptCode {
			continue
		}
		out = append(out, u)
	}
	return out
}

type gqlRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables"`
}

func (f *fakeAccountCenter) handle(w http.ResponseWriter, r *http.Request) {
	var req gqlRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	q := req.Query

	write := func(payload interface{}) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": payload})
	}

	switch {
	case strings.Contains(q, "sysDepartmentAggregate"):
		f.mu.Lock()
		count := len(f.activeDepts()) + f.aggregateDelta
		f.mu.Unlock()
		write(map[string]interface{}{"sysDepartmentAggregate": []map[string]interface{}{{"count": map[string]interface{}{"id": count}}}})
	case strings.Contains(q, "sysUserAggregate"):
		deptCode := filterDeptCode(req.Variables)
		f.mu.Lock()
		count := len(f.activeUsers(deptCode)) + f.aggregateDelta
		f.mu.Unlock()
		write(map[string]interface{}{"sysUserAggregate": []map[string]interface{}{{"count": map[string]interface{}{"id": count}}}})
	case strings.Contains(q, "$paging") && strings.Contains(q, "sysDepartments"):
		f.mu.Lock()
		all := f.activeDepts()
		f.mu.Unlock()
		write(map[string]interface{}{"sysDepartments": connectionPage(all, req.Variables)})
	case strings.Contains(q, "$paging") && strings.Contains(q, "sysUsers"):
		if f.gate != nil {
			f.sawOnce.Do(func() {
				if f.sawUsers != nil {
					close(f.sawUsers)
				}
			})
			<-f.gate
		}
		deptCode := filterDeptCode(req.Variables)
		f.mu.Lock()
		all := f.activeUsers(deptCode)
		f.mu.Unlock()
		write(map[string]interface{}{"sysUsers": connectionPage(all, req.Variables)})
	case strings.Contains(q, "sysDepartments"):
		// 既有单查（FetchAllActiveDepartments，内联 paging，无 $paging 变量）
		f.mu.Lock()
		all := f.activeDepts()
		f.mu.Unlock()
		edges := make([]map[string]interface{}, 0, len(all))
		for _, d := range all {
			edges = append(edges, map[string]interface{}{"node": d})
		}
		write(map[string]interface{}{"sysDepartments": map[string]interface{}{"edges": edges}})
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func filterDeptCode(vars map[string]interface{}) string {
	filter, _ := vars["filter"].(map[string]interface{})
	dc, _ := filter["departmentCode"].(map[string]interface{})
	code, _ := dc["eq"].(string)
	return code
}

// connectionPage 按游标分页截取（endCursor = 偏移量）。
func connectionPage[T any](all []T, vars map[string]interface{}) map[string]interface{} {
	first := 500
	after := 0
	if paging, ok := vars["paging"].(map[string]interface{}); ok {
		if v, ok := paging["first"].(float64); ok && v > 0 {
			first = int(v)
		}
		if cursor, ok := paging["after"].(string); ok && cursor != "" {
			if n, err := strconv.Atoi(cursor); err == nil {
				after = n
			}
		}
	}
	end := after + first
	hasNext := end < len(all)
	if end > len(all) {
		end = len(all)
	}
	edges := make([]map[string]interface{}, 0, end-after)
	for _, item := range all[after:end] {
		edges = append(edges, map[string]interface{}{"node": item})
	}
	return map[string]interface{}{
		"pageInfo": map[string]interface{}{"hasNextPage": hasNext, "endCursor": strconv.Itoa(end)},
		"edges":    edges,
	}
}

// ---------- 假撤销器 ----------

type fakeRevoker struct {
	mu    sync.Mutex
	calls []string
	errs  map[string]error
}

func (f *fakeRevoker) RevokeTokens(ctx context.Context, knowledgeUserID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, knowledgeUserID)
	if f.errs != nil {
		if err := f.errs[knowledgeUserID]; err != nil {
			return 0, err
		}
	}
	return 1, nil
}

func (f *fakeRevoker) called(knowledgeUserID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == knowledgeUserID {
			return true
		}
	}
	return false
}

// ---------- 测试装配 ----------

func openSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", uuid.NewString())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	return db
}

type harness struct {
	fac     *fakeAccountCenter
	revoker *fakeRevoker
	storage *storage.Storage
	service *syncservice.Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fac := newFakeAccountCenter(t)
	revoker := &fakeRevoker{}

	mainDB := openSQLite(t)
	stateDB := openSQLite(t)
	if err := stateDB.AutoMigrate(
		&model.AuditEvent{},
		&model.IdentityLink{},
		&model.ManagedEntitlement{},
		&model.SyncRun{},
		&model.SyncStep{},
		&model.AuthzEpoch{},
	); err != nil {
		t.Fatalf("migrate state schema: %v", err)
	}
	if err := mainDB.AutoMigrate(
		&model.WeKnoraTenant{},
		&model.WeComDeptTenantMapping{},
		&model.WeKnoraUser{},
		&model.WeKnoraTenantMember{},
	); err != nil {
		t.Fatalf("migrate weknora schema: %v", err)
	}
	st := storage.NewStorageWithDBs(mainDB, stateDB)

	cfg := &config.Config{WeCom: config.WeComConfig{CorpID: "ww-test"}}
	syn := syncer.NewSyncer(cfg, nil, fac.client(), st)
	svc := syncservice.NewService(cfg, fac.client(), st, syn, syncservice.WithRevoker(revoker))
	return &harness{fac: fac, revoker: revoker, storage: st, service: svc}
}

// 标准组织：T01(一级) / T02(二级, 父 T01) / F01(一级)；u1,u3 在 T01，u2 在 T02，u4 在 F01。
func standardOrg(fac *fakeAccountCenter) {
	fac.setDepts(
		accountcenter.SysDepartment{ID: "101", DepartmentCode: "T01", DepartmentName: "技术中心", DepartmentLevel: 1, DepartmentStatus: 1},
		accountcenter.SysDepartment{ID: "102", DepartmentCode: "T02", DepartmentName: "架构部", DepartmentLevel: 2, DepartmentStatus: 1, ParentCode: "T01"},
		accountcenter.SysDepartment{ID: "103", DepartmentCode: "F01", DepartmentName: "财务部", DepartmentLevel: 1, DepartmentStatus: 1},
	)
	fac.setUsers(
		accountcenter.SysUser{ID: "u1", UserName: "员工一", Email: "u1@corp.test", DepartmentCode: "T01", UserStatus: 1},
		accountcenter.SysUser{ID: "u2", UserName: "员工二", Email: "u2@corp.test", DepartmentCode: "T02", UserStatus: 1},
		accountcenter.SysUser{ID: "u3", UserName: "员工三", Email: "u3@corp.test", DepartmentCode: "T01", UserStatus: 1},
		accountcenter.SysUser{ID: "u4", UserName: "员工四", Email: "u4@corp.test", DepartmentCode: "F01", UserStatus: 1},
	)
}

func runFull(t *testing.T, h *harness, key string) (string, error) {
	t.Helper()
	return h.service.Run(context.Background(), "full", "", key, "admin-1", "rules/v1")
}

func getRun(t *testing.T, h *harness, runID string) *model.SyncRun {
	t.Helper()
	run, err := h.storage.GetSyncRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	return run
}

func activeEntitlement(t *testing.T, h *harness, subject, tenantID string) *model.ManagedEntitlement {
	t.Helper()
	ent, err := h.storage.FindActiveSyncEntitlement(context.Background(), subject, tenantID)
	if err != nil {
		t.Fatalf("active entitlement (%s,%s): %v", subject, tenantID, err)
	}
	return ent
}

func identityLink(t *testing.T, h *harness, subject string) *model.IdentityLink {
	t.Helper()
	link, err := h.storage.FindIdentityLinkBySubject(context.Background(), "wecom", "ww-test", subject)
	if err != nil {
		t.Fatalf("identity link %s: %v", subject, err)
	}
	return link
}

func weknoraUserByID(t *testing.T, h *harness, id string) *model.WeKnoraUser {
	t.Helper()
	var u model.WeKnoraUser
	if err := h.storage.DB().WithContext(context.Background()).Where("id = ?", id).First(&u).Error; err != nil {
		t.Fatalf("weknora user %s: %v", id, err)
	}
	return &u
}

func epochVersion(t *testing.T, h *harness, subject string) int64 {
	t.Helper()
	v, err := h.storage.CurrentAuthzVersion(context.Background(), subject)
	if err != nil {
		t.Fatalf("epoch %s: %v", subject, err)
	}
	return v
}

// ---------- 用例 ----------

// 全量快照 → 差异 → 供给新部门成员（含 level-2 成员向上追溯 level-1 空间）。
func TestRunFullProvision(t *testing.T) {
	h := newHarness(t)
	standardOrg(h.fac)

	runID, err := runFull(t, h, "run-1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	run := getRun(t, h, runID)
	if run.Status != syncservice.StatusSucceeded {
		t.Fatalf("status = %s, want succeeded", run.Status)
	}
	if run.SnapshotDigest == nil || *run.SnapshotDigest == "" {
		t.Fatal("snapshot digest not recorded")
	}

	// identity_links 4 条 active，entitlement 落在正确 tenant（u2 追溯 T01 → 20101；u4 → 20103）
	wantTenants := map[string]string{"u1": "20101", "u2": "20101", "u3": "20101", "u4": "20103"}
	for subject, tenantID := range wantTenants {
		link := identityLink(t, h, subject)
		if link.Status != "active" {
			t.Fatalf("link %s status = %s", subject, link.Status)
		}
		ent := activeEntitlement(t, h, subject, tenantID)
		if ent.Source != "sync" || ent.Capability != "member" {
			t.Fatalf("entitlement %s: source=%s capability=%s", subject, ent.Source, ent.Capability)
		}
		if ent.AuthzVersion != 1 {
			t.Fatalf("entitlement %s authz_version = %d, want 1", subject, ent.AuthzVersion)
		}
		if v := epochVersion(t, h, subject); v != 1 {
			t.Fatalf("epoch %s = %d, want 1", subject, v)
		}
	}

	// knowledge 侧开户 + 空间成员（既有兼容行为）
	link := identityLink(t, h, "u1")
	u1 := weknoraUserByID(t, h, link.KnowledgeUserID)
	if !u1.IsActive || u1.Email != "u1@corp.test" {
		t.Fatalf("weknora user u1: %+v", u1)
	}
	var memberships int64
	h.storage.DB().Model(&model.WeKnoraTenantMember{}).
		Where("user_id = ? AND tenant_id = ?", link.KnowledgeUserID, uint64(20101)).Count(&memberships)
	if memberships != 1 {
		t.Fatalf("u1 membership in tenant 20101 = %d", memberships)
	}

	// 步骤记账：snapshot/validate/diff applied + 4 provision applied
	steps, err := h.storage.ListSyncSteps(context.Background(), runID)
	if err != nil {
		t.Fatalf("list steps: %v", err)
	}
	applied := map[string]int{}
	for _, st := range steps {
		if st.Status != "applied" {
			t.Fatalf("step %d (%s) status = %s", st.StepSeq, st.StepType, st.Status)
		}
		applied[st.StepType]++
	}
	if applied[syncservice.StepProvision] != 4 || applied[syncservice.StepSnapshot] != 1 ||
		applied[syncservice.StepValidate] != 1 || applied[syncservice.StepDiff] != 1 ||
		applied[syncservice.StepReconcile] != 1 {
		t.Fatalf("step accounting wrong: %v", applied)
	}

	// 统计落 run 行
	var stats struct {
		DepartmentsTotal   int `json:"departments_total"`
		DepartmentsInScope int `json:"departments_in_scope"`
		MembersInScope     int `json:"members_in_scope"`
		PlannedProvision   int `json:"planned_provision"`
	}
	if err := json.Unmarshal([]byte(*run.SnapshotStats), &stats); err != nil {
		t.Fatalf("stats json: %v", err)
	}
	if stats.DepartmentsTotal != 3 || stats.MembersInScope != 4 || stats.PlannedProvision != 4 {
		t.Fatalf("stats wrong: %+v", stats)
	}
}

// 幂等重复运行：同键返回原 run_id，不产生新 run、不重复供给。
func TestRunIdempotentReplay(t *testing.T) {
	h := newHarness(t)
	standardOrg(h.fac)

	runID1, err := runFull(t, h, "same-key")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	runID2, err := runFull(t, h, "same-key")
	if err != nil {
		t.Fatalf("Run replay: %v", err)
	}
	if runID1 != runID2 {
		t.Fatalf("idempotent replay returned different run: %s vs %s", runID1, runID2)
	}
	runs, err := h.storage.ListSyncRuns(context.Background())
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if v := epochVersion(t, h, "u1"); v != 1 {
		t.Fatalf("replay bumped epoch: %d", v)
	}

	// 快照变化 → digest 变化
	digest1 := *getRun(t, h, runID1).SnapshotDigest
	users := h.fac.activeUsers("")
	users = append(users, accountcenter.SysUser{ID: "u5", UserName: "员工五", Email: "u5@corp.test", DepartmentCode: "T01", UserStatus: 1})
	h.fac.setUsers(users...)
	runID3, err := runFull(t, h, "run-2")
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	digest2 := *getRun(t, h, runID3).SnapshotDigest
	if digest1 == digest2 {
		t.Fatal("digest unchanged after snapshot change")
	}
}

// 调岗：增新收旧（新 entitlement + 旧 revoked），不停用账号、不动上游 token。
func TestRunMoveDepartment(t *testing.T) {
	h := newHarness(t)
	standardOrg(h.fac)
	if _, err := runFull(t, h, "run-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	link := identityLink(t, h, "u2")
	kuid := link.KnowledgeUserID

	h.fac.moveUser("u2", "F01")
	runID, err := runFull(t, h, "run-2")
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if run := getRun(t, h, runID); run.Status != syncservice.StatusSucceeded {
		t.Fatalf("status = %s", run.Status)
	}

	if _, err := h.storage.FindActiveSyncEntitlement(context.Background(), "u2", "20101"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatal("old entitlement u2@20101 still active")
	}
	activeEntitlement(t, h, "u2", "20103")

	var old model.ManagedEntitlement
	if err := h.storage.StateDB().WithContext(context.Background()).
		Where("subject = ? AND tenant_id = ?", "u2", "20101").First(&old).Error; err != nil {
		t.Fatalf("old entitlement row: %v", err)
	}
	if old.RevokedAt == nil {
		t.Fatal("old entitlement revoked_at not set")
	}
	if weknoraUserByID(t, h, kuid).IsActive != true {
		t.Fatal("moved user deactivated")
	}
	if h.revoker.called(kuid) {
		t.Fatal("token revoker called for move")
	}
	if v := epochVersion(t, h, "u2"); v < 2 {
		t.Fatalf("epoch u2 = %d, want >= 2", v)
	}
}

// 停用撤权：非在职 → entitlement revoked + epoch bump + 上游 token 删除 + knowledge 用户停用。
func TestRunDepartedRevoke(t *testing.T) {
	h := newHarness(t)
	standardOrg(h.fac)
	if _, err := runFull(t, h, "run-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	link := identityLink(t, h, "u3")
	kuid := link.KnowledgeUserID

	h.fac.removeUser("u3")
	runID, err := runFull(t, h, "run-2")
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if run := getRun(t, h, runID); run.Status != syncservice.StatusSucceeded {
		t.Fatalf("status = %s", run.Status)
	}

	var ent model.ManagedEntitlement
	if err := h.storage.StateDB().WithContext(context.Background()).
		Where("subject = ? AND tenant_id = ?", "u3", "20101").First(&ent).Error; err != nil {
		t.Fatalf("entitlement row: %v", err)
	}
	if ent.RevokedAt == nil {
		t.Fatal("revoked_at not set")
	}
	if v := epochVersion(t, h, "u3"); v != 2 {
		t.Fatalf("epoch u3 = %d, want 2 (provision 1 + revoke 1)", v)
	}
	if !h.revoker.called(kuid) {
		t.Fatal("token revoker not called for departed user")
	}
	if weknoraUserByID(t, h, kuid).IsActive {
		t.Fatal("departed knowledge user still active")
	}
}

// 空快照 / 分页中断计数不符 / 统计缩减 >50% → validating/snapshotting 失败关闭。
func TestRunSnapshotValidationFailures(t *testing.T) {
	// 空快照
	h := newHarness(t)
	h.fac.setDepts()
	h.fac.setUsers()
	runID, err := runFull(t, h, "run-empty")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	run := getRun(t, h, runID)
	if run.Status != syncservice.StatusFailed {
		t.Fatalf("empty snapshot: status = %s, want failed", run.Status)
	}
	step, _ := h.storage.GetSyncStep(context.Background(), runID, 2) // validate 步
	if step.Status != "failed" || step.Error == nil || !strings.Contains(*step.Error, "SYNC_SNAPSHOT_INCOMPLETE") {
		t.Fatalf("validate step: %+v", step)
	}

	// 分页中断/计数不符（aggregate 计数大于拉取数）
	h2 := newHarness(t)
	standardOrg(h2.fac)
	h2.fac.aggregateDelta = 1
	runID2, err := runFull(t, h2, "run-mismatch")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	run2 := getRun(t, h2, runID2)
	if run2.Status != syncservice.StatusFailed {
		t.Fatalf("count mismatch: status = %s, want failed", run2.Status)
	}
	step2, _ := h2.storage.GetSyncStep(context.Background(), runID2, 1) // snapshot 步
	if step2.Status != "failed" || step2.Error == nil || !strings.Contains(*step2.Error, "SYNC_SNAPSHOT_INCOMPLETE") {
		t.Fatalf("snapshot step: %+v", step2)
	}

	// 较上次成功 run 缩减 >50%
	h3 := newHarness(t)
	standardOrg(h3.fac)
	if _, err := runFull(t, h3, "run-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	h3.fac.removeUser("u1")
	h3.fac.removeUser("u2")
	h3.fac.removeUser("u3")
	runID3, err := runFull(t, h3, "run-2")
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	run3 := getRun(t, h3, runID3)
	if run3.Status != syncservice.StatusFailed {
		t.Fatalf("shrink: status = %s, want failed", run3.Status)
	}
}

// 单步失败部分失败 → 运行 partial_failed；修复后 retry 成功转 succeeded。
func TestRunPartialFailedAndRetry(t *testing.T) {
	h := newHarness(t)
	standardOrg(h.fac)
	if _, err := runFull(t, h, "run-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	linkU3 := identityLink(t, h, "u3")

	// u1、u3 离职；撤销器对 u3 的 knowledge user 报错
	h.fac.removeUser("u1")
	h.fac.removeUser("u3")
	h.revoker.errs = map[string]error{linkU3.KnowledgeUserID: errors.New("upstream token service down")}

	runID, err := runFull(t, h, "run-2")
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	run := getRun(t, h, runID)
	if run.Status != syncservice.StatusPartialFailed {
		t.Fatalf("status = %s, want partial_failed", run.Status)
	}
	// 失败步骤记账：u3 revoke failed/retryable，u1 revoke applied
	var failed, applied int
	steps, _ := h.storage.ListSyncSteps(context.Background(), runID)
	for _, st := range steps {
		if st.StepType != syncservice.StepRevoke {
			continue
		}
		switch st.Status {
		case "failed":
			failed++
			if !st.Retryable || st.SubjectRef == nil || *st.SubjectRef != "u3" {
				t.Fatalf("failed step wrong: %+v", st)
			}
		case "applied":
			applied++
		}
	}
	if failed != 1 || applied != 1 {
		t.Fatalf("revoke steps failed=%d applied=%d, want 1/1", failed, applied)
	}
	// u3 的持久化收回已生效，但停用未完成（撤销器失败）
	var ent model.ManagedEntitlement
	h.storage.StateDB().Where("subject = ? AND tenant_id = ?", "u3", "20101").First(&ent)
	if ent.RevokedAt == nil {
		t.Fatal("u3 entitlement should be revoked before revoker call")
	}
	if weknoraUserByID(t, h, linkU3.KnowledgeUserID).IsActive != true {
		t.Fatal("u3 knowledge user should stay active until revoker succeeds")
	}

	// 修复撤销器 → retry 成功
	h.revoker.errs = nil
	status, err := h.service.Retry(context.Background(), runID)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if status != syncservice.StatusSucceeded {
		t.Fatalf("retry status = %s, want succeeded", status)
	}
	if !h.revoker.called(linkU3.KnowledgeUserID) {
		t.Fatal("revoker not retried for u3")
	}
	if weknoraUserByID(t, h, linkU3.KnowledgeUserID).IsActive {
		t.Fatal("u3 knowledge user still active after retry")
	}
	if v := epochVersion(t, h, "u3"); v != 2 {
		t.Fatalf("epoch u3 = %d, want 2", v)
	}
	// 幂等键不变：retry 未新建 revoke 步骤
	steps, _ = h.storage.ListSyncSteps(context.Background(), runID)
	revokeSteps := 0
	for _, st := range steps {
		if st.StepType == syncservice.StepRevoke && st.Status != "skipped" {
			revokeSteps++
		}
	}
	if revokeSteps != 2 {
		t.Fatalf("revoke steps after retry = %d, want 2 (no dup)", revokeSteps)
	}
}

// 并发触发互斥（409 SYNC_RUN_CONFLICT）与崩溃锁过期接管。
func TestRunConcurrentConflictAndTakeover(t *testing.T) {
	h := newHarness(t)
	standardOrg(h.fac)

	h.fac.gate = make(chan struct{})
	h.fac.sawUsers = make(chan struct{})

	runDone := make(chan struct{})
	var runID1 string
	var runErr1 error
	go func() {
		defer close(runDone)
		runID1, runErr1 = h.service.Run(context.Background(), "full", "", "run-gated", "admin-1", "rules/v1")
	}()

	<-h.fac.sawUsers // run-1 已持锁并阻塞在成员拉取

	_, err := h.service.Run(context.Background(), "full", "", "run-2", "admin-1", "rules/v1")
	if !errors.Is(err, syncservice.ErrConflict) {
		t.Fatalf("concurrent run err = %v, want SYNC_RUN_CONFLICT", err)
	}

	close(h.fac.gate) // 放行 run-1
	<-runDone
	if runErr1 != nil {
		t.Fatalf("run-1: %v", runErr1)
	}
	if run := getRun(t, h, runID1); run.Status != syncservice.StatusSucceeded {
		t.Fatalf("run-1 status = %s", run.Status)
	}

	// department 与并发 full 运行同样互斥
	h.fac.gate = nil
	runDept, err := h.service.Run(context.Background(), "department", "T01", "run-dept", "admin-1", "rules/v1")
	if err != nil {
		t.Fatalf("department run: %v", err)
	}
	if run := getRun(t, h, runDept); run.Status != syncservice.StatusSucceeded {
		t.Fatalf("dept run status = %s", run.Status)
	}

	// 崩溃接管：插入持锁已过期的运行中 run，新 run 应把它置 failed 并成功
	now := time.Now()
	past := now.Add(-time.Minute)
	stale := &model.SyncRun{
		RunID: "stale-run", Scope: "full", IdempotencyKey: "stale-key",
		RuleVersion: "rules/v1", TriggeredBy: "crashed-process",
		Status: "applying", LockUntil: &past, CreatedAt: now, UpdatedAt: now,
	}
	if err := h.storage.InsertSyncRun(context.Background(), stale); err != nil {
		t.Fatalf("insert stale run: %v", err)
	}
	runID, err := runFull(t, h, "run-after-crash")
	if err != nil {
		t.Fatalf("run after crash: %v", err)
	}
	if getRun(t, h, runID).Status != syncservice.StatusSucceeded {
		t.Fatal("run after crash not succeeded")
	}
	staleAfter := getRun(t, h, "stale-run")
	if staleAfter.Status != syncservice.StatusFailed || staleAfter.FinishedAt == nil {
		t.Fatalf("stale run not taken over: %+v", staleAfter)
	}
}

// 单部门运行：只同步该部门成员；部门不存在 → 失败关闭。
func TestDepartmentScope(t *testing.T) {
	h := newHarness(t)
	standardOrg(h.fac)

	runID, err := h.service.Run(context.Background(), "department", "T01", "dept-1", "admin-1", "rules/v1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	run := getRun(t, h, runID)
	if run.Status != syncservice.StatusSucceeded {
		t.Fatalf("status = %s", run.Status)
	}
	// T01 直属成员 u1/u3（u2 在 T02，不属于本部门快照）
	activeEntitlement(t, h, "u1", "20101")
	activeEntitlement(t, h, "u3", "20101")
	if _, err := h.storage.FindActiveSyncEntitlement(context.Background(), "u2", "20101"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatal("u2 should not be provisioned by department T01 run")
	}
	var stats struct {
		DepartmentsInScope int `json:"departments_in_scope"`
		MembersInScope     int `json:"members_in_scope"`
	}
	_ = json.Unmarshal([]byte(*run.SnapshotStats), &stats)
	if stats.MembersInScope != 2 || stats.DepartmentsInScope != 1 {
		t.Fatalf("dept stats wrong: %+v", stats)
	}

	// 部门不存在 → snapshotting 失败（SYNC_SNAPSHOT_INCOMPLETE）
	badID, err := h.service.Run(context.Background(), "department", "ZZZ", "dept-bad", "admin-1", "rules/v1")
	if err != nil {
		t.Fatalf("Run bad dept: %v", err)
	}
	if run := getRun(t, h, badID); run.Status != syncservice.StatusFailed {
		t.Fatalf("bad dept status = %s, want failed", run.Status)
	}
}

// 冲突：两名成员同邮箱归并到同一 knowledge user → 后供给主体落 conflicted + 证据，
// 步骤 skipped（不计失败），运行仍可 succeeded；冲突可经 resolve 语义恢复（handler 层测试）。
func TestProvisionConflictSkipped(t *testing.T) {
	h := newHarness(t)
	h.fac.setDepts(
		accountcenter.SysDepartment{ID: "101", DepartmentCode: "T01", DepartmentName: "技术中心", DepartmentLevel: 1, DepartmentStatus: 1},
	)
	h.fac.setUsers(
		accountcenter.SysUser{ID: "u1", UserName: "员工一", Email: "shared@corp.test", DepartmentCode: "T01", UserStatus: 1},
		accountcenter.SysUser{ID: "u2", UserName: "员工二", Email: "shared@corp.test", DepartmentCode: "T01", UserStatus: 1},
	)

	runID, err := runFull(t, h, "run-1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run := getRun(t, h, runID); run.Status != syncservice.StatusSucceeded {
		t.Fatalf("status = %s, want succeeded (conflict steps are skipped, not failed)", run.Status)
	}

	link1 := identityLink(t, h, "u1")
	if link1.Status != "active" {
		t.Fatalf("u1 link status = %s", link1.Status)
	}
	link2 := identityLink(t, h, "u2")
	if link2.Status != "conflicted" {
		t.Fatalf("u2 link status = %s, want conflicted", link2.Status)
	}
	if link2.ConflictEvidence == nil || !strings.Contains(*link2.ConflictEvidence, "occupied") {
		t.Fatalf("conflict evidence missing: %+v", link2.ConflictEvidence)
	}
	if link2.KnowledgeUserID != link1.KnowledgeUserID {
		t.Fatal("conflicted link should record the colliding knowledge user")
	}

	// u2 的 provision 步骤 skipped 且带证据
	steps, _ := h.storage.ListSyncSteps(context.Background(), runID)
	var skipped int
	for _, st := range steps {
		if st.StepType == syncservice.StepProvision && st.Status == "skipped" {
			skipped++
		}
	}
	if skipped != 1 {
		t.Fatalf("skipped provision steps = %d, want 1", skipped)
	}

	// 再次运行：冲突保持（不产生第二条 active 行，也不重复记步骤）
	if _, err := runFull(t, h, "run-2"); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	link2 = identityLink(t, h, "u2")
	if link2.Status != "conflicted" {
		t.Fatalf("u2 link after re-run = %s", link2.Status)
	}
	links, _ := h.storage.ListIdentityLinks(context.Background())
	activeU2 := 0
	for _, l := range links {
		if l.ImmutableSubject == "u2" && l.Status == "active" {
			activeU2++
		}
	}
	if activeU2 != 0 {
		t.Fatalf("active u2 links = %d, want 0", activeU2)
	}
}

// 参数校验：非法 scope/缺 department_code/缺幂等键/缺 rule_version。
func TestRunParamValidation(t *testing.T) {
	h := newHarness(t)
	standardOrg(h.fac)
	cases := []struct {
		scope, dept, key, rule string
	}{
		{"", "", "k", "r"},
		{"bogus", "", "k", "r"},
		{"department", "", "k", "r"},
		{"full", "T01", "k", "r"},
		{"full", "", "", "r"},
		{"full", "", "k", ""},
	}
	for _, tc := range cases {
		if _, err := h.service.Run(context.Background(), tc.scope, tc.dept, tc.key, "admin-1", tc.rule); !errors.Is(err, syncservice.ErrInvalidRequest) {
			t.Fatalf("(%+v) err = %v, want SYNC_INVALID_REQUEST", tc, err)
		}
	}
	runs, _ := h.storage.ListSyncRuns(context.Background())
	if len(runs) != 0 {
		t.Fatalf("invalid params created %d runs", len(runs))
	}
}

// 单部门运行部分失败：供给步骤因 tenant 映射冲突失败（既有映射占用 20101）→
// partial_failed（步骤保留可重试）；移除冲突后 retry 成功，步骤幂等键不变。
func TestDepartmentScopePartialFailedAndRetry(t *testing.T) {
	h := newHarness(t)
	standardOrg(h.fac)
	ctx := context.Background()

	// 预置幽灵映射占用 T01 将分配的 tenant 20101（member provision 时 SaveDeptTenantMapping 撞唯一索引）
	if err := h.storage.SaveDeptTenantMapping(ctx, &model.WeComDeptTenantMapping{
		WeComDeptID: 999, DepartmentCode: "GHOST", TenantID: 20101, DeptName: "幽灵",
	}); err != nil {
		t.Fatalf("ghost mapping: %v", err)
	}

	runDept := func(key string) (string, error) {
		return h.service.Run(ctx, "department", "T01", key, "admin-1", "rules/v1")
	}
	runID, err := runDept("dept-1")
	if err != nil {
		t.Fatalf("dept run: %v", err)
	}
	run := getRun(t, h, runID)
	if run.Status != syncservice.StatusPartialFailed {
		t.Fatalf("status = %s, want partial_failed", run.Status)
	}
	steps, _ := h.storage.ListSyncSteps(ctx, runID)
	failed := 0
	for _, st := range steps {
		if st.StepType == syncservice.StepProvision && st.Status == "failed" {
			failed++
			if !st.Retryable {
				t.Fatalf("failed provision step not retryable: %+v", st)
			}
		}
	}
	if failed != 2 { // u1、u3 两个 T01 成员
		t.Fatalf("failed provision steps = %d, want 2", failed)
	}
	if _, err := h.storage.FindActiveSyncEntitlement(ctx, "u1", "20101"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatal("entitlement should not exist while provision failing")
	}

	// 移除冲突映射 → retry 成功，且不新建步骤
	if err := h.storage.DB().WithContext(ctx).Where("department_code = ?", "GHOST").Delete(&model.WeComDeptTenantMapping{}).Error; err != nil {
		t.Fatalf("remove ghost mapping: %v", err)
	}
	status, err := h.service.Retry(ctx, runID)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if status != syncservice.StatusSucceeded {
		t.Fatalf("retry status = %s", status)
	}
	activeEntitlement(t, h, "u1", "20101")
	activeEntitlement(t, h, "u3", "20101")
	steps, _ = h.storage.ListSyncSteps(ctx, runID)
	provisionSteps := 0
	for _, st := range steps {
		if st.StepType == syncservice.StepProvision && st.Status != "skipped" {
			provisionSteps++
		}
	}
	if provisionSteps != 2 {
		t.Fatalf("provision steps after retry = %d, want 2 (idempotency keys unchanged)", provisionSteps)
	}
}

// failed（空快照关闭）的 run 在数据恢复后可 retry 成功。
func TestRetryFailedRunAfterDataRestored(t *testing.T) {
	h := newHarness(t)
	h.fac.setDepts()
	h.fac.setUsers()

	runID, err := runFull(t, h, "run-empty")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if getRun(t, h, runID).Status != syncservice.StatusFailed {
		t.Fatal("empty snapshot run should be failed")
	}

	standardOrg(h.fac)
	status, err := h.service.Retry(context.Background(), runID)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if status != syncservice.StatusSucceeded {
		t.Fatalf("retry status = %s, want succeeded", status)
	}
	activeEntitlement(t, h, "u1", "20101")
}

// H1：Retry 必须对新快照重跑空/缩减守卫——守卫与 validating 步骤记账解耦。
// partial_failed 的 run 在快照退化（较上次成功缩减 >50%）后 retry：整运行转 failed，
// 不产生任何差异/清权动作（不新建/重放任何 provision/revoke 步骤，不触达撤销器与停用）。
func TestRetryDegradedSnapshotFailsClosed(t *testing.T) {
	h := newHarness(t)
	standardOrg(h.fac)
	if _, err := runFull(t, h, "run-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 构造 partial_failed：u1 离职 + 撤销器对其 knowledge user 报错
	linkU1 := identityLink(t, h, "u1")
	h.fac.removeUser("u1")
	h.revoker.errs = map[string]error{linkU1.KnowledgeUserID: errors.New("upstream token service down")}
	runID, err := runFull(t, h, "run-2")
	if err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if run := getRun(t, h, runID); run.Status != syncservice.StatusPartialFailed {
		t.Fatalf("status = %s, want partial_failed", run.Status)
	}
	h.revoker.errs = nil

	stepsBefore, err := h.storage.ListSyncSteps(context.Background(), runID)
	if err != nil {
		t.Fatalf("list steps: %v", err)
	}
	h.revoker.mu.Lock()
	revokerCallsBefore := len(h.revoker.calls)
	h.revoker.mu.Unlock()

	// 快照退化：成员 4 → 1（较 run-1 缩减 75% > 50%）
	h.fac.removeUser("u2")
	h.fac.removeUser("u3")

	status, err := h.service.Retry(context.Background(), runID)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if status != syncservice.StatusFailed {
		t.Fatalf("retry status = %s, want failed（退化快照必须失败关闭）", status)
	}

	// 不产生差异/清权：撤销器未被再次调用，u1 的 knowledge 用户未被停用
	h.revoker.mu.Lock()
	revokerCallsAfter := len(h.revoker.calls)
	h.revoker.mu.Unlock()
	if revokerCallsAfter != revokerCallsBefore {
		t.Fatalf("revoker called during degraded retry: before=%d after=%d", revokerCallsBefore, revokerCallsAfter)
	}
	if !weknoraUserByID(t, h, linkU1.KnowledgeUserID).IsActive {
		t.Fatal("degraded retry must not deactivate knowledge user")
	}

	// 步骤面不变（applying 未执行：没有新建/重放任何动作步骤）
	stepsAfter, err := h.storage.ListSyncSteps(context.Background(), runID)
	if err != nil {
		t.Fatalf("list steps after retry: %v", err)
	}
	if len(stepsAfter) != len(stepsBefore) {
		t.Fatalf("steps changed by degraded retry: before=%d after=%d", len(stepsBefore), len(stepsAfter))
	}
}

// L6：并发同幂等键触发——锁事务内查键，后到者归并返回原 run（不 500、不新建运行）。
func TestRunConcurrentSameIdempotencyKey(t *testing.T) {
	h := newHarness(t)
	standardOrg(h.fac)

	h.fac.gate = make(chan struct{})
	h.fac.sawUsers = make(chan struct{})

	runDone := make(chan struct{})
	var runID1 string
	var runErr1 error
	go func() {
		defer close(runDone)
		runID1, runErr1 = h.service.Run(context.Background(), "full", "", "dup-key", "admin-1", "rules/v1")
	}()

	<-h.fac.sawUsers // run-1 已持锁并阻塞在成员拉取

	// 同键并发触发：命中锁事务内的幂等重放，返回原 run 而非 SYNC_RUN_CONFLICT/唯一键 500
	runID2, err := h.service.Run(context.Background(), "full", "", "dup-key", "admin-2", "rules/v1")
	if err != nil {
		t.Fatalf("concurrent same-key run err = %v, want replay of original run", err)
	}

	close(h.fac.gate)
	<-runDone
	if runErr1 != nil {
		t.Fatalf("run-1: %v", runErr1)
	}
	if runID2 != runID1 {
		t.Fatalf("same-key concurrent run returned %s, want original run %s", runID2, runID1)
	}
	runs, err := h.storage.ListSyncRuns(context.Background())
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1（同键归并）", len(runs))
	}
}
