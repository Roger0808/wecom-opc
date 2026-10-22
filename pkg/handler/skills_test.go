// TASK-P10（INT-141/142）测试：发布任务幂等/逐目标失败可见、技能列表按租户
// entitlement 过滤与去重、download hash 校验与 delist 透传、管理端权限范围。
package handler

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"weknora-tokendance/pkg/adminauth"

	"github.com/gin-gonic/gin"
)

// releaseFixture 构造假 release 元数据。
func releaseFixture(id, sourceTenant, version string, targetTenants ...float64) map[string]interface{} {
	return map[string]interface{}{
		"id":             id,
		"catalog_id":     "cat-1",
		"version":        version,
		"bundle_sha256":  "",
		"manifest":       map[string]interface{}{"skill_id": id, "version": version},
		"target_tenants": targetTenants,
		"target_roles":   []string{},
		"status":         "published",
		"source_tenant":  sourceTenant,
		"published_by":   "admin",
		"published_at":   time.Now().UTC().Format(time.RFC3339),
	}
}

func adminHeaders(t *testing.T, subject string, role adminauth.Role, tenantID uint64) map[string]string {
	t.Helper()
	tok, err := adminauth.IssueToken([]byte(testAdminKey), subject, role, tenantID, time.Hour)
	if err != nil {
		t.Fatalf("issue admin token: %v", err)
	}
	return map[string]string{"Authorization": "Bearer " + tok}
}

// ---------- INT-142a GET /integration/v1/skills ----------

func TestSkillsList_MultiTenantFilterAndDedup(t *testing.T) {
	fake := newFakeKnowledge()
	rel1 := releaseFixture("rel-1", "1001", "1.0.0", 1001, 1002)
	rel2 := releaseFixture("rel-2", "1002", "2.0.0", 1002)
	rel3 := releaseFixture("rel-3", "1001", "3.0.0", 1001)
	rel3["status"] = "delisted"
	fake.releaseListByTenant["1001"] = []map[string]interface{}{rel1, rel3}
	fake.releaseListByTenant["1002"] = []map[string]interface{}{rel1, rel2}

	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-k", "ku-k", "1001", "1002")

	w := doGet(t, r, "/integration/v1/skills", platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d, body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	skills, ok := body["skills"].([]interface{})
	if !ok || len(skills) != 2 {
		t.Fatalf("skills = %v, want 2 entries", body["skills"])
	}
	first := skills[0].(map[string]interface{})
	if first["publish_id"] != "rel-1" || first["version"] != "1.0.0" || first["source_tenant"] != "1001" {
		t.Fatalf("rel-1 entry = %v", first)
	}
	if first["hash"] == nil || first["manifest"] == nil {
		t.Fatalf("rel-1 missing hash/manifest: %v", first)
	}
	second := skills[1].(map[string]interface{})
	if second["publish_id"] != "rel-2" || second["source_tenant"] != "1002" {
		t.Fatalf("rel-2 entry = %v", second)
	}
	// delisted rel-3 不得出现在列表。
	for _, entry := range skills {
		if entry.(map[string]interface{})["publish_id"] == "rel-3" {
			t.Fatalf("delisted release leaked into list: %v", skills)
		}
	}
}

// ---------- INT-142b GET /integration/v1/skills/:publish_id/download ----------

func TestSkillsDownload_OK(t *testing.T) {
	fake := newFakeKnowledge()
	archive := []byte("ZIP-ARCHIVE-BYTES")
	rel := releaseFixture("rel-1", "1001", "1.0.0", 1002)
	rel["bundle_sha256"] = sha256Of(archive)
	fake.releaseByID["rel-1"] = rel
	fake.archiveBytes["rel-1"] = archive

	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-k", "ku-k", "1002")

	w := doGet(t, r, "/integration/v1/skills/rel-1/download", platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("download = %d, body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != string(archive) {
		t.Fatalf("body = %q", w.Body.String())
	}
	if got := w.Header().Get("X-SHA256"); got != sha256Of(archive) {
		t.Fatalf("X-SHA256 = %q", got)
	}
}

func TestSkillsDownload_HashMismatch(t *testing.T) {
	fake := newFakeKnowledge()
	rel := releaseFixture("rel-1", "1001", "1.0.0", 1002)
	rel["bundle_sha256"] = "0000000000000000000000000000000000000000000000000000000000000000"
	fake.releaseByID["rel-1"] = rel
	fake.archiveBytes["rel-1"] = []byte("ZIP-ARCHIVE-BYTES")

	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-k", "ku-k", "1002")

	w := doGet(t, r, "/integration/v1/skills/rel-1/download", platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusUnprocessableEntity || errorCode(t, w) != errCodeSkillHashMismatch {
		t.Fatalf("hash mismatch = %d/%s", w.Code, errorCode(t, w))
	}
}

func TestSkillsDownload_DelistedPassthrough(t *testing.T) {
	fake := newFakeKnowledge()
	rel := releaseFixture("rel-3", "1001", "3.0.0", 1001)
	rel["status"] = "delisted"
	fake.releaseByID["rel-3"] = rel
	fake.archiveStatus["rel-3"] = http.StatusForbidden

	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-k", "ku-k", "1001")

	w := doGet(t, r, "/integration/v1/skills/rel-3/download", platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeSkillDelisted {
		t.Fatalf("delisted = %d/%s", w.Code, errorCode(t, w))
	}
}

func TestSkillsDownload_NotVisible(t *testing.T) {
	fake := newFakeKnowledge()
	fake.releaseByID["rel-2"] = releaseFixture("rel-2", "1002", "2.0.0", 1002)

	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-k", "ku-k", "1001")

	w := doGet(t, r, "/integration/v1/skills/rel-2/download", platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeSpaceForbidden {
		t.Fatalf("not visible = %d/%s", w.Code, errorCode(t, w))
	}
}

// ---------- INT-141a POST /integration/v1/admin/skills/publish-jobs ----------

func publishJobBody() gin.H {
	return gin.H{
		"release_id":    "rel-1",
		"source_tenant": "1001",
		"targets":       []string{"1001", "1002", "3003"},
	}
}

func TestPublishJob_CreateAndIdempotent(t *testing.T) {
	fake := newFakeKnowledge()
	fake.releaseByID["rel-1"] = releaseFixture("rel-1", "1001", "1.0.0", 1001, 1002)

	r, s, _ := setupKnowledgeEnv(t, fake)
	seedKnowledgeSubject(t, s, "admin-1", "ku-admin", "1001")

	headers := adminHeaders(t, "admin-1", adminauth.RoleSystemAdmin, 0)
	headers["Idempotency-Key"] = "idem-1"

	w := doPostJSON(t, r, "/integration/v1/admin/skills/publish-jobs", publishJobBody(), headers)
	if w.Code != http.StatusOK {
		t.Fatalf("create job = %d, body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["status"] != "partial_failed" {
		t.Fatalf("job status = %v, want partial_failed (3003 not in target_tenants)", body["status"])
	}
	jobID, _ := body["job_id"].(string)
	if jobID == "" {
		t.Fatalf("missing job_id: %v", body)
	}
	steps, ok := body["steps"].([]interface{})
	if !ok || len(steps) != 3 {
		t.Fatalf("steps = %v", body["steps"])
	}
	want := map[string]string{"1001": "validated", "1002": "validated", "3003": "failed"}
	for _, raw := range steps {
		step := raw.(map[string]interface{})
		if step["status"] != want[fmt.Sprintf("%v", step["tenant"])] {
			t.Fatalf("step = %v", step)
		}
	}

	// 幂等：同键重复提交返回原任务，库内仍只有一条。
	w = doPostJSON(t, r, "/integration/v1/admin/skills/publish-jobs", publishJobBody(), headers)
	if w.Code != http.StatusOK {
		t.Fatalf("idempotent replay = %d, body=%s", w.Code, w.Body.String())
	}
	replay := decodeBody(t, w)
	if replay["job_id"] != jobID {
		t.Fatalf("replay job_id = %v, want %s", replay["job_id"], jobID)
	}
	var count int64
	if err := s.StateDB().Table("skill_publish_jobs").Count(&count).Error; err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if count != 1 {
		t.Fatalf("skill_publish_jobs rows = %d, want 1", count)
	}
}

func TestPublishJob_AllTargetsValidated(t *testing.T) {
	fake := newFakeKnowledge()
	fake.releaseByID["rel-1"] = releaseFixture("rel-1", "1001", "1.0.0", 1001, 1002)
	r, s, _ := setupKnowledgeEnv(t, fake)
	seedKnowledgeSubject(t, s, "admin-1", "ku-admin", "1001")

	headers := adminHeaders(t, "admin-1", adminauth.RoleSystemAdmin, 0)
	headers["Idempotency-Key"] = "idem-ok"
	w := doPostJSON(t, r, "/integration/v1/admin/skills/publish-jobs", gin.H{
		"release_id": "rel-1", "source_tenant": "1001", "targets": []string{"1002"},
	}, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("create = %d, body=%s", w.Code, w.Body.String())
	}
	if status := decodeBody(t, w)["status"]; status != "succeeded" {
		t.Fatalf("status = %v, want succeeded", status)
	}
}

func TestPublishJob_DeptAdminScope(t *testing.T) {
	fake := newFakeKnowledge()
	fake.releaseByID["rel-1"] = releaseFixture("rel-1", "1001", "1.0.0", 1001, 1002)
	r, s, _ := setupKnowledgeEnv(t, fake)
	seedKnowledgeSubject(t, s, "admin-1", "ku-admin", "1001", "1002")

	// dept_admin(1002) 发起源空间 1001 的任务 → 403。
	headers := adminHeaders(t, "admin-1", adminauth.RoleDeptAdmin, 1002)
	headers["Idempotency-Key"] = "idem-scope"
	w := doPostJSON(t, r, "/integration/v1/admin/skills/publish-jobs", publishJobBody(), headers)
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeSpaceForbidden {
		t.Fatalf("dept_admin cross-tenant = %d/%s", w.Code, errorCode(t, w))
	}

	// dept_admin(1001) 源空间 1001 → 200。
	headers = adminHeaders(t, "admin-1", adminauth.RoleDeptAdmin, 1001)
	headers["Idempotency-Key"] = "idem-scope-ok"
	w = doPostJSON(t, r, "/integration/v1/admin/skills/publish-jobs", publishJobBody(), headers)
	if w.Code != http.StatusOK {
		t.Fatalf("dept_admin own tenant = %d, body=%s", w.Code, w.Body.String())
	}
}

func TestPublishJob_MissingIdempotencyKey(t *testing.T) {
	fake := newFakeKnowledge()
	r, s, _ := setupKnowledgeEnv(t, fake)
	seedKnowledgeSubject(t, s, "admin-1", "ku-admin", "1001")
	w := doPostJSON(t, r, "/integration/v1/admin/skills/publish-jobs", publishJobBody(),
		adminHeaders(t, "admin-1", adminauth.RoleSystemAdmin, 0))
	if w.Code != http.StatusBadRequest || errorCode(t, w) != errCodeInvalidRequest {
		t.Fatalf("missing key = %d/%s", w.Code, errorCode(t, w))
	}
}

func TestPublishJob_AdminWithoutIdentityLink(t *testing.T) {
	fake := newFakeKnowledge()
	fake.releaseByID["rel-1"] = releaseFixture("rel-1", "1001", "1.0.0", 1001, 1002)
	r, _, _ := setupKnowledgeEnv(t, fake)
	headers := adminHeaders(t, "ghost-admin", adminauth.RoleSystemAdmin, 0)
	headers["Idempotency-Key"] = "idem-ghost"
	w := doPostJSON(t, r, "/integration/v1/admin/skills/publish-jobs", publishJobBody(), headers)
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeSubjectNotProv {
		t.Fatalf("no identity link = %d/%s", w.Code, errorCode(t, w))
	}
}

func TestPublishJob_ReleaseNotFound(t *testing.T) {
	fake := newFakeKnowledge()
	r, s, _ := setupKnowledgeEnv(t, fake)
	seedKnowledgeSubject(t, s, "admin-1", "ku-admin", "1001")
	headers := adminHeaders(t, "admin-1", adminauth.RoleSystemAdmin, 0)
	headers["Idempotency-Key"] = "idem-404"
	w := doPostJSON(t, r, "/integration/v1/admin/skills/publish-jobs", publishJobBody(), headers)
	if w.Code != http.StatusNotFound {
		t.Fatalf("release not found = %d, body=%s", w.Code, w.Body.String())
	}
}

func TestPublishJob_UpstreamFailure(t *testing.T) {
	fake := newFakeKnowledge()
	fake.releaseByID["rel-1"] = releaseFixture("rel-1", "1001", "1.0.0", 1001, 1002)
	fake.metadataStatus["rel-1"] = http.StatusInternalServerError
	r, s, _ := setupKnowledgeEnv(t, fake)
	seedKnowledgeSubject(t, s, "admin-1", "ku-admin", "1001")
	headers := adminHeaders(t, "admin-1", adminauth.RoleSystemAdmin, 0)
	headers["Idempotency-Key"] = "idem-502"
	w := doPostJSON(t, r, "/integration/v1/admin/skills/publish-jobs", publishJobBody(), headers)
	if w.Code != http.StatusBadGateway || errorCode(t, w) != errCodeUpstreamUnavailable {
		t.Fatalf("upstream failure = %d/%s", w.Code, errorCode(t, w))
	}
}

// ---------- INT-141b GET /integration/v1/admin/skills/publish-jobs/:id ----------

func TestPublishJob_GetByID(t *testing.T) {
	fake := newFakeKnowledge()
	fake.releaseByID["rel-1"] = releaseFixture("rel-1", "1001", "1.0.0", 1001, 1002)
	r, s, _ := setupKnowledgeEnv(t, fake)
	seedKnowledgeSubject(t, s, "admin-1", "ku-admin", "1001")

	headers := adminHeaders(t, "admin-1", adminauth.RoleSystemAdmin, 0)
	headers["Idempotency-Key"] = "idem-get"
	w := doPostJSON(t, r, "/integration/v1/admin/skills/publish-jobs", publishJobBody(), headers)
	jobID := decodeBody(t, w)["job_id"].(string)

	w = doGet(t, r, "/integration/v1/admin/skills/publish-jobs/"+jobID, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("get job = %d, body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["job_id"] != jobID || body["status"] != "partial_failed" {
		t.Fatalf("job view = %v", body)
	}

	// 未知任务 → 404。
	w = doGet(t, r, "/integration/v1/admin/skills/publish-jobs/00000000-0000-0000-0000-000000000000", headers)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown job = %d", w.Code)
	}

	// dept_admin(1002) 查源空间 1001 的任务 → 403。
	deptHeaders := adminHeaders(t, "admin-1", adminauth.RoleDeptAdmin, 1002)
	w = doGet(t, r, "/integration/v1/admin/skills/publish-jobs/"+jobID, deptHeaders)
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeSpaceForbidden {
		t.Fatalf("dept_admin get cross-tenant = %d/%s", w.Code, errorCode(t, w))
	}

	// dept_admin(1001) 可查。
	deptHeaders = adminHeaders(t, "admin-1", adminauth.RoleDeptAdmin, 1001)
	w = doGet(t, r, "/integration/v1/admin/skills/publish-jobs/"+jobID, deptHeaders)
	if w.Code != http.StatusOK {
		t.Fatalf("dept_admin get own tenant = %d, body=%s", w.Code, w.Body.String())
	}
}
