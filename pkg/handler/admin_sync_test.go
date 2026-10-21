package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/accountcenter"
	"weknora-tokendance/pkg/adminauth"
	"weknora-tokendance/pkg/authz"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"
	"weknora-tokendance/pkg/syncer"
	"weknora-tokendance/pkg/syncservice"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ---------- 假账户中心（分页/聚合/单查协议，供 AdminSync 处理器测试） ----------

type syncFakeAccountCenter struct {
	depts  map[string]accountcenter.SysDepartment
	users  map[string]accountcenter.SysUser
	server *httptest.Server
}

func newSyncFakeAccountCenter(t *testing.T) *syncFakeAccountCenter {
	t.Helper()
	f := &syncFakeAccountCenter{
		depts: map[string]accountcenter.SysDepartment{},
		users: map[string]accountcenter.SysUser{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

func (f *syncFakeAccountCenter) client() *accountcenter.Client {
	return accountcenter.NewClient(f.server.URL)
}

func (f *syncFakeAccountCenter) seed() {
	f.depts["T01"] = accountcenter.SysDepartment{ID: "101", DepartmentCode: "T01", DepartmentName: "技术中心", DepartmentLevel: 1, DepartmentStatus: 1}
	f.depts["T02"] = accountcenter.SysDepartment{ID: "102", DepartmentCode: "T02", DepartmentName: "架构部", DepartmentLevel: 1, DepartmentStatus: 1}
	f.users["u1"] = accountcenter.SysUser{ID: "u1", UserName: "员工一", Email: "u1@corp.test", DepartmentCode: "T01", UserStatus: 1}
}

func (f *syncFakeAccountCenter) activeDepts() []accountcenter.SysDepartment {
	out := []accountcenter.SysDepartment{}
	for _, d := range f.depts {
		if d.DepartmentStatus == 1 {
			out = append(out, d)
		}
	}
	return out
}

func (f *syncFakeAccountCenter) activeUsers(deptCode string) []accountcenter.SysUser {
	out := []accountcenter.SysUser{}
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

func (f *syncFakeAccountCenter) handle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string                 `json:"query"`
		Variables map[string]interface{} `json:"variables"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	q := req.Query
	write := func(payload interface{}) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": payload})
	}
	deptCode := ""
	if filter, ok := req.Variables["filter"].(map[string]interface{}); ok {
		if dc, ok := filter["departmentCode"].(map[string]interface{}); ok {
			deptCode, _ = dc["eq"].(string)
		}
	}
	switch {
	case strings.Contains(q, "sysDepartmentAggregate"):
		write(map[string]interface{}{"sysDepartmentAggregate": []map[string]interface{}{{"count": map[string]interface{}{"id": len(f.activeDepts())}}}})
	case strings.Contains(q, "sysUserAggregate"):
		write(map[string]interface{}{"sysUserAggregate": []map[string]interface{}{{"count": map[string]interface{}{"id": len(f.activeUsers(deptCode))}}}})
	case strings.Contains(q, "$paging") && strings.Contains(q, "sysDepartments"):
		write(map[string]interface{}{"sysDepartments": syncConnectionPage(f.activeDepts(), req.Variables)})
	case strings.Contains(q, "$paging") && strings.Contains(q, "sysUsers"):
		write(map[string]interface{}{"sysUsers": syncConnectionPage(f.activeUsers(deptCode), req.Variables)})
	case strings.Contains(q, "sysDepartments"):
		edges := []map[string]interface{}{}
		for _, d := range f.activeDepts() {
			edges = append(edges, map[string]interface{}{"node": d})
		}
		write(map[string]interface{}{"sysDepartments": map[string]interface{}{"edges": edges}})
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func syncConnectionPage[T any](all []T, vars map[string]interface{}) map[string]interface{} {
	first, after := 500, 0
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
	edges := []map[string]interface{}{}
	for _, item := range all[after:end] {
		edges = append(edges, map[string]interface{}{"node": item})
	}
	return map[string]interface{}{
		"pageInfo": map[string]interface{}{"hasNextPage": hasNext, "endCursor": strconv.Itoa(end)},
		"edges":    edges,
	}
}

// ---------- 装配 ----------

type syncAdminFixture struct {
	fac     *syncFakeAccountCenter
	storage *storage.Storage
	service *syncservice.Service
	router  *gin.Engine
}

func setupSyncAdmin(t *testing.T) *syncAdminFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	fac := newSyncFakeAccountCenter(t)
	fac.seed()

	open := func() *gorm.DB {
		dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", uuid.NewString())
		db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		sqlDB, _ := db.DB()
		sqlDB.SetMaxOpenConns(1)
		return db
	}
	mainDB, stateDB := open(), open()
	if err := stateDB.AutoMigrate(&model.AuditEvent{}, &model.IdentityLink{}, &model.ManagedEntitlement{}, &model.SyncRun{}, &model.SyncStep{}, &model.AuthzEpoch{}); err != nil {
		t.Fatalf("migrate state: %v", err)
	}
	if err := mainDB.AutoMigrate(&model.WeKnoraTenant{}, &model.WeComDeptTenantMapping{}, &model.WeKnoraUser{}, &model.WeKnoraTenantMember{}); err != nil {
		t.Fatalf("migrate main: %v", err)
	}
	st := storage.NewStorageWithDBs(mainDB, stateDB)

	cfg := &config.Config{WeCom: config.WeComConfig{CorpID: "ww-test"}}
	syn := syncer.NewSyncer(cfg, nil, fac.client(), st)
	svc := syncservice.NewService(cfg, fac.client(), st, syn, syncservice.WithRevoker(authz.NoopTokenRevoker{}))

	h := NewAdminSyncHandler(cfg, st, svc)
	r := gin.New()
	admin := r.Group("/integration/v1/admin", adminauth.RequireAdminAuth(adminauth.MiddlewareConfig{
		Secret: []byte(testAdminKey),
	}))
	{
		admin.POST("/sync/runs", h.CreateSyncRun)
		admin.GET("/sync/runs", h.ListSyncRuns)
		admin.GET("/sync/runs/:id", h.GetSyncRun)
		admin.POST("/sync/runs/:id/retry", h.RetrySyncRun)
		admin.GET("/identity-conflicts", h.ListIdentityConflicts)
		admin.POST("/identity-conflicts/:id/resolve", h.ResolveIdentityConflict)
	}
	return &syncAdminFixture{fac: fac, storage: st, service: svc, router: r}
}

func adminToken(t *testing.T, role adminauth.Role, tenantID uint64) string {
	t.Helper()
	token, err := adminauth.IssueToken([]byte(testAdminKey), "admin-1", role, tenantID, time.Hour)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return "Bearer " + token
}

func syncAdminPost(t *testing.T, r *gin.Engine, path, token string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func syncAdminGet(t *testing.T, r *gin.Engine, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func errCodeOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error envelope from %q: %v", w.Body.String(), err)
	}
	return body.Error.Code
}

// ---------- 用例 ----------

// 参数校验 400 + 未认证 401。
func TestAdminSyncRunValidation(t *testing.T) {
	fx := setupSyncAdmin(t)
	sys := adminToken(t, adminauth.RoleSystemAdmin, 0)

	w := syncAdminPost(t, fx.router, "/integration/v1/admin/sync/runs", "", map[string]string{
		"scope": "full", "idempotency_key": "k", "rule_version": "r",
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}

	cases := []map[string]string{
		{"scope": "", "idempotency_key": "k", "rule_version": "r"},
		{"scope": "bogus", "idempotency_key": "k", "rule_version": "r"},
		{"scope": "department", "idempotency_key": "k", "rule_version": "r"},
		{"scope": "full", "department_code": "T01", "idempotency_key": "k", "rule_version": "r"},
		{"scope": "full", "idempotency_key": "", "rule_version": "r"},
		{"scope": "full", "idempotency_key": "k", "rule_version": ""},
	}
	for _, body := range cases {
		w := syncAdminPost(t, fx.router, "/integration/v1/admin/sync/runs", sys, body)
		if w.Code != http.StatusBadRequest || errCodeOf(t, w) != "INVALID_REQUEST" {
			t.Fatalf("body=%v → %d %s", body, w.Code, w.Body.String())
		}
	}
}

// dept_admin 范围：full 403；非本租户部门 403；本租户部门 200。其余端点仅 system_admin。
func TestAdminSyncRunDeptAdminScope(t *testing.T) {
	fx := setupSyncAdmin(t)
	dept := adminToken(t, adminauth.RoleDeptAdmin, 20101)

	// 预置部门→租户映射（T01→20101 本部门管理员租户；T02→20102 他人租户）
	ctx := context.Background()
	if err := fx.storage.SaveDeptTenantMapping(ctx, &model.WeComDeptTenantMapping{WeComDeptID: 101, DepartmentCode: "T01", TenantID: 20101, DeptName: "技术中心"}); err != nil {
		t.Fatalf("mapping T01: %v", err)
	}
	if err := fx.storage.SaveDeptTenantMapping(ctx, &model.WeComDeptTenantMapping{WeComDeptID: 102, DepartmentCode: "T02", TenantID: 20102, DeptName: "架构部"}); err != nil {
		t.Fatalf("mapping T02: %v", err)
	}

	// dept_admin + full → 403
	w := syncAdminPost(t, fx.router, "/integration/v1/admin/sync/runs", dept, map[string]string{
		"scope": "full", "idempotency_key": "k-full", "rule_version": "r",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("dept_admin full: %d", w.Code)
	}

	// dept_admin + 他人部门 → 403
	w = syncAdminPost(t, fx.router, "/integration/v1/admin/sync/runs", dept, map[string]string{
		"scope": "department", "department_code": "T02", "idempotency_key": "k-t02", "rule_version": "r",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("dept_admin other dept: %d", w.Code)
	}

	// dept_admin + 本部门 → 200
	w = syncAdminPost(t, fx.router, "/integration/v1/admin/sync/runs", dept, map[string]string{
		"scope": "department", "department_code": "T01", "idempotency_key": "k-t01", "rule_version": "r",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("dept_admin own dept: %d %s", w.Code, w.Body.String())
	}

	// dept_admin + 无映射部门 → 403
	w = syncAdminPost(t, fx.router, "/integration/v1/admin/sync/runs", dept, map[string]string{
		"scope": "department", "department_code": "T99", "idempotency_key": "k-t99", "rule_version": "r",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("dept_admin unmapped dept: %d", w.Code)
	}

	// 其余端点 dept_admin → 403
	if w := syncAdminGet(t, fx.router, "/integration/v1/admin/sync/runs", dept); w.Code != http.StatusForbidden {
		t.Fatalf("dept_admin list: %d", w.Code)
	}
	if w := syncAdminGet(t, fx.router, "/integration/v1/admin/identity-conflicts", dept); w.Code != http.StatusForbidden {
		t.Fatalf("dept_admin conflicts: %d", w.Code)
	}
}

// 409 SYNC_RUN_CONFLICT：存在持锁运行中的同范围 run 时拒绝触发。
func TestAdminSyncRunConflict(t *testing.T) {
	fx := setupSyncAdmin(t)
	sys := adminToken(t, adminauth.RoleSystemAdmin, 0)

	past := time.Now().Add(2 * time.Minute) // 持锁中
	locked := &model.SyncRun{
		RunID: uuid.NewString(), Scope: "full", IdempotencyKey: "locked-run",
		RuleVersion: "r", TriggeredBy: "other", Status: "applying",
		LockUntil: &past, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := fx.storage.InsertSyncRun(context.Background(), locked); err != nil {
		t.Fatalf("insert locked run: %v", err)
	}

	w := syncAdminPost(t, fx.router, "/integration/v1/admin/sync/runs", sys, map[string]string{
		"scope": "full", "idempotency_key": "k-conflict", "rule_version": "r",
	})
	if w.Code != http.StatusConflict || errCodeOf(t, w) != "SYNC_RUN_CONFLICT" {
		t.Fatalf("conflict: %d %s", w.Code, w.Body.String())
	}
}

// 触发→查询详情/列表→retry 非可重放 400。
func TestAdminSyncRunLifecycle(t *testing.T) {
	fx := setupSyncAdmin(t)
	sys := adminToken(t, adminauth.RoleSystemAdmin, 0)

	w := syncAdminPost(t, fx.router, "/integration/v1/admin/sync/runs", sys, map[string]string{
		"scope": "full", "idempotency_key": "k-1", "rule_version": "rules/v1",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if created.RunID == "" || created.Status != syncservice.StatusSucceeded {
		t.Fatalf("created: %+v", created)
	}

	// 详情：含步骤与统计
	w = syncAdminGet(t, fx.router, "/integration/v1/admin/sync/runs/"+created.RunID, sys)
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d", w.Code)
	}
	var detail struct {
		Run struct {
			Status string `json:"status"`
		} `json:"run"`
		Steps []struct {
			StepType string `json:"step_type"`
			Status   string `json:"status"`
		} `json:"steps"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &detail)
	if len(detail.Steps) == 0 {
		t.Fatal("detail missing steps")
	}

	// 404
	if w := syncAdminGet(t, fx.router, "/integration/v1/admin/sync/runs/"+uuid.NewString(), sys); w.Code != http.StatusNotFound {
		t.Fatalf("get missing: %d", w.Code)
	}

	// 列表 + 状态过滤
	w = syncAdminGet(t, fx.router, "/integration/v1/admin/sync/runs?status=succeeded&page=1", sys)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d", w.Code)
	}
	var list struct {
		Runs  []json.RawMessage `json:"runs"`
		Total int               `json:"total"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if list.Total != 1 || len(list.Runs) != 1 {
		t.Fatalf("list: %+v", list)
	}
	if w := syncAdminGet(t, fx.router, "/integration/v1/admin/sync/runs?status=bogus", sys); w.Code != http.StatusBadRequest {
		t.Fatalf("list bogus status: %d", w.Code)
	}

	// succeeded run 不可 retry → 400
	w = syncAdminPost(t, fx.router, "/integration/v1/admin/sync/runs/"+created.RunID+"/retry", sys, nil)
	if w.Code != http.StatusBadRequest || errCodeOf(t, w) != "SYNC_RUN_NOT_RETRYABLE" {
		t.Fatalf("retry: %d %s", w.Code, w.Body.String())
	}
	// 404 retry
	w = syncAdminPost(t, fx.router, "/integration/v1/admin/sync/runs/"+uuid.NewString()+"/retry", sys, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("retry missing: %d", w.Code)
	}
}

// H5：/admin/api/sync/all 改走 syncservice.Run(scope=full)——走账本/持久化锁/快照契约，
// 不再走 syncer.SyncAll 裸流程；并发冲突由 sync_runs 锁以 409 拒绝（isSyncing 已删除）。
func TestAdminTriggerSyncAll_GoesThroughSyncService(t *testing.T) {
	fx := setupSyncAdmin(t)

	cfg := &config.Config{WeCom: config.WeComConfig{CorpID: "ww-test"}}
	h := NewHandler(cfg, nil, fx.fac.client(), nil, nil, fx.storage)
	h.SetSyncService(fx.service)
	legacy := fx.router.Group("/admin/api", adminauth.RequireAdmin(adminauth.MiddlewareConfig{
		Secret: []byte(testAdminKey),
	}))
	legacy.POST("/sync/all", h.AdminTriggerSyncAll)

	// 触发：同步阻塞至终态，返回 run_id + succeeded；账本/快照/步骤全部落库
	w := syncAdminPost(t, fx.router, "/admin/api/sync/all", adminToken(t, adminauth.RoleSystemAdmin, 0), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("sync/all = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Code int `json:"code"`
		Data struct {
			RunID  string `json:"run_id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (%s)", err, w.Body.String())
	}
	if resp.Data.RunID == "" || resp.Data.Status != syncservice.StatusSucceeded {
		t.Fatalf("sync/all response = %+v, want run_id + succeeded", resp.Data)
	}

	run, err := fx.storage.GetSyncRun(context.Background(), resp.Data.RunID)
	if err != nil {
		t.Fatalf("sync run not in ledger: %v", err)
	}
	if run.Scope != "full" || run.SnapshotDigest == nil || *run.SnapshotDigest == "" {
		t.Fatalf("run ledger row incomplete: %+v", run)
	}
	steps, err := fx.storage.ListSyncSteps(context.Background(), run.RunID)
	if err != nil || len(steps) == 0 {
		t.Fatalf("sync steps missing: %v %v", steps, err)
	}
	// 真实供给发生：u1（T01 成员）获得 20101 空间的 sync 授权
	if _, err := fx.storage.FindActiveSyncEntitlement(context.Background(), "u1", "20101"); err != nil {
		t.Fatalf("u1 entitlement missing after sync/all: %v", err)
	}

	// 并发互斥以持久化锁为准：持锁运行中的同范围 run 使新触发 409
	future := time.Now().Add(2 * time.Minute)
	locked := &model.SyncRun{
		RunID: uuid.NewString(), Scope: "full", IdempotencyKey: "locked-run",
		RuleVersion: "r", TriggeredBy: "other", Status: "applying",
		LockUntil: &future, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := fx.storage.InsertSyncRun(context.Background(), locked); err != nil {
		t.Fatalf("insert locked run: %v", err)
	}
	w = syncAdminPost(t, fx.router, "/admin/api/sync/all", adminToken(t, adminauth.RoleSystemAdmin, 0), nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("sync/all with locked run = %d, want 409, body=%s", w.Code, w.Body.String())
	}
}

// 冲突清单与 resolve：bind 需显式 knowledge_user_id + 占用检查（409）；quarantine；审计留痕。
func TestAdminIdentityConflictsResolve(t *testing.T) {
	fx := setupSyncAdmin(t)
	sys := adminToken(t, adminauth.RoleSystemAdmin, 0)
	ctx := context.Background()

	insertLink := func(subject, kuid, status, evidence string) *model.IdentityLink {
		link := &model.IdentityLink{
			IdentitySource: "wecom", OrganizationID: "ww-test",
			ImmutableSubject: subject, KnowledgeUserID: kuid, Status: status,
		}
		if evidence != "" {
			link.ConflictEvidence = &evidence
		}
		if err := fx.storage.InsertIdentityLink(ctx, link); err != nil {
			t.Fatalf("insert link: %v", err)
		}
		return link
	}

	conflicted := insertLink("u9", "ku-collision", "conflicted", `{"reason":"occupied"}`)
	insertLink("u8", "ku-occupied", "active", "")

	// 清单
	w := syncAdminGet(t, fx.router, "/integration/v1/admin/identity-conflicts", sys)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d", w.Code)
	}
	var list struct {
		Conflicts []struct {
			ID               string `json:"id"`
			Status           string `json:"status"`
			ConflictEvidence string `json:"conflict_evidence"`
		} `json:"conflicts"`
		Total int `json:"total"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if list.Total != 1 || list.Conflicts[0].ConflictEvidence == "" {
		t.Fatalf("conflicts: %+v", list)
	}

	// resolve 缺 action → 400
	w = syncAdminPost(t, fx.router, "/integration/v1/admin/identity-conflicts/"+conflicted.ID+"/resolve", sys, map[string]string{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("resolve no action: %d", w.Code)
	}

	// bind 缺 knowledge_user_id → 400
	w = syncAdminPost(t, fx.router, "/integration/v1/admin/identity-conflicts/"+conflicted.ID+"/resolve", sys, map[string]string{"action": "bind"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bind no kuid: %d", w.Code)
	}

	// bind 占用中 knowledge_user_id → 409
	w = syncAdminPost(t, fx.router, "/integration/v1/admin/identity-conflicts/"+conflicted.ID+"/resolve", sys, map[string]string{
		"action": "bind", "knowledge_user_id": "ku-occupied",
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("bind occupied: %d %s", w.Code, w.Body.String())
	}

	// bind 空闲 knowledge_user_id → 200 active
	w = syncAdminPost(t, fx.router, "/integration/v1/admin/identity-conflicts/"+conflicted.ID+"/resolve", sys, map[string]string{
		"action": "bind", "knowledge_user_id": "ku-free",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("bind: %d %s", w.Code, w.Body.String())
	}
	link, err := fx.storage.GetIdentityLink(ctx, conflicted.ID)
	if err != nil || link.Status != "active" || link.KnowledgeUserID != "ku-free" {
		t.Fatalf("after bind: %+v err=%v", link, err)
	}

	// 非 conflicted 行 resolve → 400
	w = syncAdminPost(t, fx.router, "/integration/v1/admin/identity-conflicts/"+conflicted.ID+"/resolve", sys, map[string]string{
		"action": "quarantine",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("resolve non-conflicted: %d", w.Code)
	}

	// quarantine 另一个 conflicted 行 → 200 disabled
	other := insertLink("u7", "ku-x", "conflicted", `{"reason":"occupied"}`)
	w = syncAdminPost(t, fx.router, "/integration/v1/admin/identity-conflicts/"+other.ID+"/resolve", sys, map[string]string{
		"action": "quarantine",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("quarantine: %d %s", w.Code, w.Body.String())
	}
	link, _ = fx.storage.GetIdentityLink(ctx, other.ID)
	if link.Status != "disabled" {
		t.Fatalf("after quarantine: %s", link.Status)
	}

	// 404
	w = syncAdminPost(t, fx.router, "/integration/v1/admin/identity-conflicts/"+uuid.NewString()+"/resolve", sys, map[string]string{
		"action": "quarantine",
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("resolve missing: %d", w.Code)
	}

	// 审计留痕：resolve 事件 ≥ 2
	var audits int64
	fx.storage.StateDB().Model(&model.AuditEvent{}).
		Where("action = ?", "integration.admin.identity_conflict.resolved").Count(&audits)
	if audits != 2 {
		t.Fatalf("resolve audit events = %d, want 2", audits)
	}
}
