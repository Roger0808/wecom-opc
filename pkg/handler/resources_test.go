// WORK-RESOURCES-001（TASK-009/010/014）测试：资源列表/单项（VAL-201/202/208）、
// 归档下载代理与错误映射（VAL-209/210/211/218）、旧路由 404（VAL-216）。
// 覆盖：多空间同名样例、无权过滤、分页去重、部分/全部失败、资源删除、
// 三种 hash 一致校验、不可信 Content-Length/途中断连/坏 hash、独立下载预算。
package handler

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"weknora-tokendance/config"
)

// seedTwoSpaceCatalogs 样例（DATA-101/VAL-101）：T1 有 C1（包 h1），T2 有同名
// C2（包 h2），另有 T1 目录 C1x 无归档（not_ready）。h1/h2 由真实包字节计算。
func seedTwoSpaceCatalogs(fake *fakeKnowledge) (h1, h2 string, bundle1, bundle2 []byte) {
	bundle1 = []byte("SKILL-C1-BUNDLE-H1-BYTES")
	bundle2 = []byte("SKILL-C2-BUNDLE-H2-BYTES-UPDATED")
	h1 = sha256Of(bundle1)
	h2 = sha256Of(bundle2)
	fake.catalogListByTenant["1001"] = []map[string]interface{}{
		catalogFixture("C1", "同名技能", "1.0.0", "T1 的技能", h1),
		catalogFixture("C1x", "未就绪技能", "0.1.0", "归档缺失", ""),
	}
	fake.catalogListByTenant["1002"] = []map[string]interface{}{
		catalogFixture("C2", "同名技能", "2.0.0", "T2 的同名技能", h2),
	}
	fake.catalogArchive["1001:C1"] = bundle1
	fake.catalogArchive["1002:C2"] = bundle2
	return h1, h2, bundle1, bundle2
}

// ---------- VAL-201 列表：多空间、搜索、排序、分页、partial ----------

func TestResourcesKBList_MultiTenantAndSearch(t *testing.T) {
	fake := newFakeKnowledge()
	fake.kbListByTenant["1001"] = []map[string]interface{}{{"id": "A", "name": "苹果型号"}, {"id": "B", "name": "香蕉型号"}}
	fake.kbListByTenant["1002"] = []map[string]interface{}{{"id": "A2", "name": "苹果型号"}}
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001", "1002")
	seedTenantRow(t, s, 1001, "空间T1")

	headers := platformAccessHeaders(t, s, link.ID)
	w := doGet(t, r, "/integration/v1/resources/knowledge-bases", headers)
	if w.Code != http.StatusOK {
		t.Fatalf("kb list = %d, body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["complete"] != true {
		t.Fatalf("complete = %v", body["complete"])
	}
	if failures, ok := body["failures"].([]interface{}); !ok || len(failures) != 0 {
		t.Fatalf("failures = %v", body["failures"])
	}
	items := body["items"].([]interface{})
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	// 默认排序 tenant_id 升序：1001 的两条在前，且名称 NFC 折叠升序。
	first := items[0].(map[string]interface{})
	second := items[1].(map[string]interface{})
	third := items[2].(map[string]interface{})
	if first["knowledge_base_id"] != "A" || first["tenant_id"] != "1001" {
		t.Fatalf("first item = %v", first)
	}
	if second["knowledge_base_id"] != "B" || third["knowledge_base_id"] != "A2" || third["tenant_id"] != "1002" {
		t.Fatalf("order = %v / %v", second, third)
	}
	if first["resource_kind"] != "knowledge_base" || first["availability"] != "available" {
		t.Fatalf("first item fields = %v", first)
	}
	if first["tenant_name"] != "空间T1" {
		t.Fatalf("tenant_name = %v", first["tenant_name"])
	}
	if first["catalog_id"] != nil {
		t.Fatalf("kb item must not carry catalog_id: %v", first)
	}

	// 名称搜索（大小写无关）："苹果" 命中 A 与 A2，不命中 B。
	w = doGet(t, r, "/integration/v1/resources/knowledge-bases?q=%20%E8%8B%B9%E6%9E%9C%20", headers)
	if w.Code != http.StatusOK {
		t.Fatalf("search = %d, body=%s", w.Code, w.Body.String())
	}
	body = decodeBody(t, w)
	if body["total"] != float64(2) {
		t.Fatalf("search total = %v", body["total"])
	}
	for _, raw := range body["items"].([]interface{}) {
		it := raw.(map[string]interface{})
		if it["knowledge_base_id"] == "B" {
			t.Fatalf("search leaked B: %v", body["items"])
		}
	}
	fake.assertNoReleaseCalls(t)
}

func TestResourcesKBList_Pagination(t *testing.T) {
	fake := newFakeKnowledge()
	kbs := make([]map[string]interface{}, 0, 5)
	for i := 0; i < 5; i++ {
		kbs = append(kbs, map[string]interface{}{"id": fmt.Sprintf("kb-%d", i), "name": fmt.Sprintf("库%d", i)})
	}
	fake.kbListByTenant["1001"] = kbs
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001")

	w := doGet(t, r, "/integration/v1/resources/knowledge-bases?offset=1&limit=2", platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("page = %d, body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["total"] != float64(5) || body["offset"] != float64(1) || body["limit"] != float64(2) {
		t.Fatalf("envelope = %v", body)
	}
	items := body["items"].([]interface{})
	if len(items) != 2 {
		t.Fatalf("page items = %d, want 2", len(items))
	}

	// 参数校验：limit>100 / offset<0 / q 超长 → 400。
	headers := platformAccessHeaders(t, s, link.ID)
	if w := doGet(t, r, "/integration/v1/resources/knowledge-bases?limit=101", headers); w.Code != http.StatusBadRequest {
		t.Fatalf("limit=101 = %d", w.Code)
	}
	if w := doGet(t, r, "/integration/v1/resources/knowledge-bases?offset=-1", headers); w.Code != http.StatusBadRequest {
		t.Fatalf("offset=-1 = %d", w.Code)
	}
	if w := doGet(t, r, "/integration/v1/resources/knowledge-bases?q="+strings.Repeat("x", 201), headers); w.Code != http.StatusBadRequest {
		t.Fatalf("q>200 = %d", w.Code)
	}
}

func TestResourcesSkillList_MultiTenantSameNameAndNotReady(t *testing.T) {
	fake := newFakeKnowledge()
	h1, h2, _, _ := seedTwoSpaceCatalogs(fake)
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001", "1002")

	w := doGet(t, r, "/integration/v1/resources/skills", platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("skill list = %d, body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	items := body["items"].([]interface{})
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	// 排序：1001 的 C1/C1x（名称折叠升序：C1 < C1x），再 1002 的 C2。
	byID := map[string]map[string]interface{}{}
	for _, raw := range items {
		it := raw.(map[string]interface{})
		byID[it["catalog_id"].(string)] = it
		if it["resource_kind"] != "skill_catalog" {
			t.Fatalf("kind = %v", it)
		}
		if strings.Contains(fmt.Sprintf("%v", it), "installed_on") || strings.Contains(fmt.Sprintf("%v", it), "manifest") {
			t.Fatalf("storage/sandbox fields leaked: %v", it)
		}
	}
	c1 := byID["C1"]
	if c1["bundle_sha256"] != h1 || c1["version"] != "1.0.0" || c1["availability"] != "available" {
		t.Fatalf("C1 = %v", c1)
	}
	c1x := byID["C1x"]
	if c1x["availability"] != "not_ready" || c1x["unavailable_reason"] != "ARCHIVE_NOT_READY" {
		t.Fatalf("C1x not_ready = %v", c1x)
	}
	if sha, ok := c1x["bundle_sha256"]; ok && sha != "" {
		t.Fatalf("C1x bundle_sha256 must be empty when unknown: %v", sha)
	}
	c2 := byID["C2"]
	if c2["bundle_sha256"] != h2 || c2["catalog_id"] != "C2" || c2["tenant_id"] != "1002" {
		t.Fatalf("C2 = %v", c2)
	}
	// 同名不合并：C1 与 C2 各自独立成行。
	if c1["name"] != c2["name"] {
		t.Fatalf("same-name sample diverged: %v vs %v", c1["name"], c2["name"])
	}
	fake.assertNoReleaseCalls(t)
}

func TestResourcesSkillList_SearchMatchesFoldedName(t *testing.T) {
	fake := newFakeKnowledge()
	fake.catalogListByTenant["1001"] = []map[string]interface{}{
		catalogFixture("cat-cafe", "Café Skill", "1.0.0", "", sha256Of([]byte("cafe-bundle"))),
		catalogFixture("cat-other", "其他", "1.0.0", "", sha256Of([]byte("other-bundle"))),
	}
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001")

	// decomposed "Café"（e + U+0301 = %CC%81）应命中 composed "Café"（NFC + 大小写无关）。
	w := doGet(t, r, "/integration/v1/resources/skills?q=cafe%CC%81", platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("nfc search = %d, body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["total"] != float64(1) {
		t.Fatalf("nfc search total = %v", body["total"])
	}
	if got := body["items"].([]interface{})[0].(map[string]interface{})["catalog_id"]; got != "cat-cafe" {
		t.Fatalf("nfc search hit = %v", got)
	}
}

func TestResourcesList_PartialFailure(t *testing.T) {
	fake := newFakeKnowledge()
	fake.kbListByTenant["1001"] = []map[string]interface{}{{"id": "A", "name": "苹果型号"}}
	fake.kbStatus["1002"] = http.StatusBadGateway
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001", "1002")

	w := doGet(t, r, "/integration/v1/resources/knowledge-bases", platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("partial list = %d, body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["complete"] != false {
		t.Fatalf("complete = %v, want false", body["complete"])
	}
	failures, ok := body["failures"].([]interface{})
	if !ok || len(failures) != 1 {
		t.Fatalf("failures = %v", body["failures"])
	}
	failure := failures[0].(map[string]interface{})
	if failure["tenant_id"] != "1002" || failure["code"] != "UPSTREAM_UNAVAILABLE" {
		t.Fatalf("failure entry = %v", failure)
	}
	// 成功空间的条目仍在，total 只统计成功集合。
	if body["total"] != float64(1) {
		t.Fatalf("total = %v", body["total"])
	}
	items := body["items"].([]interface{})
	if len(items) != 1 || items[0].(map[string]interface{})["knowledge_base_id"] != "A" {
		t.Fatalf("items = %v", items)
	}
}

func TestResourcesList_AllFailedIsErrorNotEmptyList(t *testing.T) {
	fake := newFakeKnowledge()
	fake.kbStatus["1001"] = http.StatusBadGateway
	fake.kbStatus["1002"] = http.StatusBadGateway
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001", "1002")

	w := doGet(t, r, "/integration/v1/resources/knowledge-bases", platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusBadGateway || errorCode(t, w) != errCodeUpstreamUnavailable {
		t.Fatalf("all failed = %d/%s, want 502/UPSTREAM_UNAVAILABLE", w.Code, errorCode(t, w))
	}
}

func TestResourcesList_EntitledButEmptyIsComplete(t *testing.T) {
	fake := newFakeKnowledge()
	fake.kbListByTenant["1001"] = []map[string]interface{}{}
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001")

	w := doGet(t, r, "/integration/v1/resources/knowledge-bases", platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("empty list = %d", w.Code)
	}
	body := decodeBody(t, w)
	if body["complete"] != true || body["total"] != float64(0) {
		t.Fatalf("empty envelope = %v", body)
	}
	if items, ok := body["items"].([]interface{}); !ok || len(items) != 0 {
		t.Fatalf("items = %v", body["items"])
	}
}

// ---------- VAL-202 鉴权负向 ----------

func TestResourcesList_Unauthenticated(t *testing.T) {
	fake := newFakeKnowledge()
	r, _, _ := setupKnowledgeEnv(t, fake)
	if w := doGet(t, r, "/integration/v1/resources/knowledge-bases", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer = %d", w.Code)
	}
	if w := doGet(t, r, "/integration/v1/resources/skills", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer skills = %d", w.Code)
	}
}

func TestResourcesList_ExplicitUnentitledTenantForbidden(t *testing.T) {
	fake := newFakeKnowledge()
	fake.kbListByTenant["1001"] = []map[string]interface{}{{"id": "A", "name": "苹果型号"}}
	r, s, _ := setupKnowledgeEnv(t, fake)
	// U3：无 T1 entitlement（VAL-101/VAL-202）。
	link := seedKnowledgeSubject(t, s, "subj-u3", "ku-u3", "1002")

	w := doGet(t, r, "/integration/v1/resources/knowledge-bases?tenant_id=1001", platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeSpaceForbidden {
		t.Fatalf("unentitled tenant filter = %d/%s", w.Code, errorCode(t, w))
	}
	fake.assertNoReleaseCalls(t)
}

func TestResourcesSkillItem_UnentitledAndInvisible(t *testing.T) {
	fake := newFakeKnowledge()
	h1, _, _, _ := seedTwoSpaceCatalogs(fake)
	r, s, _ := setupKnowledgeEnv(t, fake)
	linkU3 := seedKnowledgeSubject(t, s, "subj-u3", "ku-u3", "1002")
	linkU1 := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001")

	// U3 请求 T1 的 C1 → 403（不返回该空间信息）。
	w := doGet(t, r, "/integration/v1/resources/skills/C1?tenant_id=1001", platformAccessHeaders(t, s, linkU3.ID))
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeSpaceForbidden {
		t.Fatalf("cross-tenant item = %d/%s", w.Code, errorCode(t, w))
	}

	// U1 请求 T1 不存在的目录（T2 有同名也不替代）→ 404 RESOURCE_NOT_ACCESSIBLE。
	w = doGet(t, r, "/integration/v1/resources/skills/C2?tenant_id=1001", platformAccessHeaders(t, s, linkU1.ID))
	if w.Code != http.StatusNotFound || errorCode(t, w) != errCodeResourceNotAccessible {
		t.Fatalf("invisible item = %d/%s, want 404/%s", w.Code, errorCode(t, w), errCodeResourceNotAccessible)
	}

	// 单项成功：返回当前 hash（执行前检查，VAL-201/INT-101）。
	w = doGet(t, r, "/integration/v1/resources/skills/C1?tenant_id=1001", platformAccessHeaders(t, s, linkU1.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("item = %d, body=%s", w.Code, w.Body.String())
	}
	item := decodeBody(t, w)["item"].(map[string]interface{})
	if item["catalog_id"] != "C1" || item["bundle_sha256"] != h1 || item["availability"] != "available" {
		t.Fatalf("item = %v", item)
	}

	// 参数负向：缺 tenant_id / 恶意 catalog_id → 400。
	headers := platformAccessHeaders(t, s, linkU1.ID)
	if w := doGet(t, r, "/integration/v1/resources/skills/C1", headers); w.Code != http.StatusBadRequest {
		t.Fatalf("missing tenant_id = %d", w.Code)
	}
	if w := doGet(t, r, "/integration/v1/resources/skills/a%09b?tenant_id=1001", headers); w.Code != http.StatusBadRequest {
		t.Fatalf("hostile catalog_id = %d", w.Code)
	}
	fake.assertNoReleaseCalls(t)
}

// ---------- VAL-208 资源删除 ----------

func TestResourcesSkillList_DeletedCatalogDisappears(t *testing.T) {
	fake := newFakeKnowledge()
	h1, _, bundle1, _ := seedTwoSpaceCatalogs(fake)
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001", "1002")
	headers := platformAccessHeaders(t, s, link.ID)

	w := doGet(t, r, "/integration/v1/resources/skills", headers)
	if got := len(decodeBody(t, w)["items"].([]interface{})); got != 3 {
		t.Fatalf("before delete items = %d", got)
	}

	// 网页侧删除目录：fake 的空间归属裁定立即不再返回 C1。
	fake.mu.Lock()
	fake.catalogListByTenant["1001"] = fake.catalogListByTenant["1001"][1:]
	fake.mu.Unlock()

	w = doGet(t, r, "/integration/v1/resources/skills", headers)
	body := decodeBody(t, w)
	items := body["items"].([]interface{})
	if len(items) != 2 {
		t.Fatalf("after delete items = %d", len(items))
	}
	for _, raw := range items {
		if it := raw.(map[string]interface{}); it["catalog_id"] == "C1" && it["tenant_id"] == "1001" {
			t.Fatalf("deleted catalog still listed: %v", items)
		}
	}
	// 已删目录单项/归档均 404，且旧目录即使字节仍在也不可复活下载。
	w = doGet(t, r, "/integration/v1/resources/skills/C1?tenant_id=1001", headers)
	if w.Code != http.StatusNotFound || errorCode(t, w) != errCodeResourceNotAccessible {
		t.Fatalf("deleted item = %d/%s", w.Code, errorCode(t, w))
	}
	w = doGet(t, r, "/integration/v1/resources/skills/C1/archive?tenant_id=1001&expected_sha256="+h1, headers)
	if w.Code != http.StatusNotFound || errorCode(t, w) != errCodeResourceNotAccessible {
		t.Fatalf("deleted archive = %d/%s", w.Code, errorCode(t, w))
	}
	_ = bundle1
	fake.assertNoReleaseCalls(t)
}

// ---------- VAL-209/210/211/218 归档下载代理 ----------

func TestResourcesArchiveDownload_OKAndHashConsistency(t *testing.T) {
	fake := newFakeKnowledge()
	h1, _, bundle1, _ := seedTwoSpaceCatalogs(fake)
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001", "1002")

	w := doGet(t, r, "/integration/v1/resources/skills/C1/archive?tenant_id=1001&expected_sha256="+h1,
		platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("archive = %d, body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != string(bundle1) {
		t.Fatalf("archive bytes mismatch: %q", w.Body.String())
	}
	if got := w.Header().Get("X-SHA256"); got != h1 {
		t.Fatalf("X-SHA256 = %q, want %q", got, h1)
	}
	if got := w.Header().Get("Content-Type"); got != "application/zip" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := w.Header().Get("Content-Length"); got != fmt.Sprintf("%d", len(bundle1)) {
		t.Fatalf("Content-Length = %q", got)
	}
	if w.Header().Get("Content-Disposition") == "" {
		t.Fatalf("Content-Disposition missing")
	}
	fake.assertNoReleaseCalls(t)
}

func TestResourcesArchiveDownload_UppercaseSHANormalized(t *testing.T) {
	fake := newFakeKnowledge()
	h1, _, _, _ := seedTwoSpaceCatalogs(fake)
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001")

	w := doGet(t, r, "/integration/v1/resources/skills/C1/archive?tenant_id=1001&expected_sha256="+strings.ToUpper(h1),
		platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("uppercase sha = %d, body=%s", w.Code, w.Body.String())
	}
}

func TestResourcesArchiveDownload_VersionConflict(t *testing.T) {
	fake := newFakeKnowledge()
	h1, _, _, _ := seedTwoSpaceCatalogs(fake)
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001")

	// 列表 SHA 已变 h2，客户端仍带旧预期 h1（VAL-210）→ 409 SKILL_VERSION_CHANGED。
	fake.mu.Lock()
	entry := fake.catalogListByTenant["1001"][0]
	bundle2 := []byte("SKILL-C1-BUNDLE-H2-UPDATED")
	entry["bundle_sha256"] = sha256Of(bundle2)
	fake.catalogArchive["1001:C1"] = bundle2
	fake.mu.Unlock()
	h2 := sha256Of(bundle2)

	w := doGet(t, r, "/integration/v1/resources/skills/C1/archive?tenant_id=1001&expected_sha256="+h1,
		platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusConflict || errorCode(t, w) != errCodeSkillVersionChanged {
		t.Fatalf("stale expected sha = %d/%s, want 409/%s", w.Code, errorCode(t, w), errCodeSkillVersionChanged)
	}
	// 刷新后按新 hash 可下载。
	w = doGet(t, r, "/integration/v1/resources/skills/C1/archive?tenant_id=1001&expected_sha256="+h2,
		platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("fresh sha = %d, body=%s", w.Code, w.Body.String())
	}
}

func TestResourcesArchiveDownload_NotReady(t *testing.T) {
	fake := newFakeKnowledge()
	seedTwoSpaceCatalogs(fake)
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001")

	// 上游 409 ARCHIVE_NOT_READY（目录归档暂缺）→ 原样映射 409。
	fake.archiveErr["1001:C1"] = fakeArchiveError{status: http.StatusConflict, code: errCodeArchiveNotReady}
	w := doGet(t, r, "/integration/v1/resources/skills/C1/archive?tenant_id=1001&expected_sha256="+sha256Of([]byte("x")),
		platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusConflict || errorCode(t, w) != errCodeArchiveNotReady {
		t.Fatalf("not ready = %d/%s, want 409/%s", w.Code, errorCode(t, w), errCodeArchiveNotReady)
	}
}

func TestResourcesArchiveDownload_BadBytesHashMismatch422(t *testing.T) {
	fake := newFakeKnowledge()
	h1, _, _, _ := seedTwoSpaceCatalogs(fake)
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001")

	// 存储字节与权威元数据不符（VAL-211）→ 平台代理 422，绝不返回假 ZIP。
	fake.mu.Lock()
	fake.catalogArchive["1001:C1"] = []byte("CORRUPTED-BYTES-DIFFERENT")
	fake.mu.Unlock()
	w := doGet(t, r, "/integration/v1/resources/skills/C1/archive?tenant_id=1001&expected_sha256="+h1,
		platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusUnprocessableEntity || errorCode(t, w) != errCodeSkillHashMismatch {
		t.Fatalf("bad bytes = %d/%s, want 422/%s", w.Code, errorCode(t, w), errCodeSkillHashMismatch)
	}
	if strings.Contains(w.Body.String(), "CORRUPTED") {
		t.Fatalf("bad bytes leaked into error body: %q", w.Body.String())
	}
}

func TestResourcesArchiveDownload_UntrustworthyHeaderRejected(t *testing.T) {
	fake := newFakeKnowledge()
	h1, _, _, _ := seedTwoSpaceCatalogs(fake)
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001")

	// 上游 X-SHA256 头与字节不符（不可信 Content-Length/头）→ 422。
	fake.archiveBadHeader["1001:C1"] = true
	w := doGet(t, r, "/integration/v1/resources/skills/C1/archive?tenant_id=1001&expected_sha256="+h1,
		platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusUnprocessableEntity || errorCode(t, w) != errCodeSkillHashMismatch {
		t.Fatalf("bad header = %d/%s, want 422/%s", w.Code, errorCode(t, w), errCodeSkillHashMismatch)
	}
}

func TestResourcesArchiveDownload_StreamTruncationNoFakeZip(t *testing.T) {
	fake := newFakeKnowledge()
	h1, _, bundle1, _ := seedTwoSpaceCatalogs(fake)
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001")

	// 途中断连（声明完整长度但只发前缀）→ 502，绝不返回半个 ZIP。
	fake.archiveTruncate["1001:C1"] = len(bundle1) / 2
	w := doGet(t, r, "/integration/v1/resources/skills/C1/archive?tenant_id=1001&expected_sha256="+h1,
		platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusBadGateway || errorCode(t, w) != errCodeUpstreamUnavailable {
		t.Fatalf("truncated = %d/%s, want 502/%s", w.Code, errorCode(t, w), errCodeUpstreamUnavailable)
	}
	if strings.HasPrefix(w.Body.String(), "PK") || strings.Contains(w.Body.String(), string(bundle1[:len(bundle1)/2])) {
		t.Fatalf("truncated zip leaked: %q", w.Body.String())
	}
}

func TestResourcesArchiveDownload_IndependentBudgetNotMetadataTimeout(t *testing.T) {
	fake := newFakeKnowledge()
	h1, _, bundle1, _ := seedTwoSpaceCatalogs(fake)

	// 元数据 15s 超时缩到 50ms（装配环境前替换 client 工厂才生效）；下载流
	// 300ms 完成（无进度 idle 保持 15s）→ 仍 200，证明下载不复用元数据超时。
	old := upstreamTimeout
	oldFactory := newMetadataClient
	upstreamTimeout = 50 * time.Millisecond
	newMetadataClient = func() *http.Client { return &http.Client{Timeout: upstreamTimeout} }
	t.Cleanup(func() {
		upstreamTimeout = old
		newMetadataClient = oldFactory
	})

	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001")

	fake.mu.Lock()
	fake.archiveDelay = 300 * time.Millisecond
	fake.mu.Unlock()
	w := doGet(t, r, "/integration/v1/resources/skills/C1/archive?tenant_id=1001&expected_sha256="+h1,
		platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("slow download killed by metadata timeout = %d, body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != string(bundle1) {
		t.Fatalf("bytes mismatch after slow download")
	}
}

func TestResourcesArchiveDownload_IdleTimeoutKillsStalledStream(t *testing.T) {
	fake := newFakeKnowledge()
	h1, _, _, _ := seedTwoSpaceCatalogs(fake)

	// 无进度等待超时缩到 100ms（装配环境前替换 client 工厂才生效）→ 502。
	oldIdle := archiveDownloadIdleTimeout
	oldFactory := newDownloadClient
	archiveDownloadIdleTimeout = 100 * time.Millisecond
	newDownloadClient = func() *http.Client {
		return &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: archiveDownloadIdleTimeout}}
	}
	t.Cleanup(func() {
		archiveDownloadIdleTimeout = oldIdle
		newDownloadClient = oldFactory
	})

	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001")

	fake.mu.Lock()
	fake.archiveDelay = 500 * time.Millisecond
	fake.mu.Unlock()
	w := doGet(t, r, "/integration/v1/resources/skills/C1/archive?tenant_id=1001&expected_sha256="+h1,
		platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusBadGateway || errorCode(t, w) != errCodeUpstreamUnavailable {
		t.Fatalf("stalled stream = %d/%s, want 502/%s", w.Code, errorCode(t, w), errCodeUpstreamUnavailable)
	}
}

func TestResourcesArchiveDownload_TotalBudgetApplies(t *testing.T) {
	fake := newFakeKnowledge()
	h1, _, _, _ := seedTwoSpaceCatalogs(fake)
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001")

	// 总预算（缩到 200ms）超时 → 502。
	oldBudget := archiveDownloadTotalBudget
	archiveDownloadTotalBudget = 200 * time.Millisecond
	t.Cleanup(func() { archiveDownloadTotalBudget = oldBudget })

	fake.mu.Lock()
	fake.archiveDelay = 800 * time.Millisecond
	fake.mu.Unlock()
	w := doGet(t, r, "/integration/v1/resources/skills/C1/archive?tenant_id=1001&expected_sha256="+h1,
		platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusBadGateway || errorCode(t, w) != errCodeUpstreamUnavailable {
		t.Fatalf("budget exceeded = %d/%s, want 502/%s", w.Code, errorCode(t, w), errCodeUpstreamUnavailable)
	}
}

func TestResourcesArchiveDownload_UnentitledAndMissingSHA(t *testing.T) {
	fake := newFakeKnowledge()
	h1, _, _, _ := seedTwoSpaceCatalogs(fake)
	r, s, _ := setupKnowledgeEnv(t, fake)
	linkU3 := seedKnowledgeSubject(t, s, "subj-u3", "ku-u3", "1002")
	linkU1 := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001")

	w := doGet(t, r, "/integration/v1/resources/skills/C1/archive?tenant_id=1001&expected_sha256="+h1,
		platformAccessHeaders(t, s, linkU3.ID))
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeSpaceForbidden {
		t.Fatalf("cross-tenant archive = %d/%s", w.Code, errorCode(t, w))
	}
	headers := platformAccessHeaders(t, s, linkU1.ID)
	if w := doGet(t, r, "/integration/v1/resources/skills/C1/archive?tenant_id=1001&expected_sha256=xyz", headers); w.Code != http.StatusBadRequest {
		t.Fatalf("bad sha = %d", w.Code)
	}
	if w := doGet(t, r, "/integration/v1/resources/skills/C1/archive?tenant_id=1001", headers); w.Code != http.StatusBadRequest {
		t.Fatalf("missing sha = %d", w.Code)
	}
	fake.assertNoReleaseCalls(t)
}

// ---------- detail_url 可信 origin ----------

func TestResourcesDetailURLTrustedOriginOnly(t *testing.T) {
	fake := newFakeKnowledge()
	r, s, _ := setupKnowledgeEnv(t, fake)
	// 直接构造 handler 以注入 https 可信 base（env 的 httptest 为 http，不出链接）。
	cfg := &config.Config{WeKnora: config.WeKnoraConfig{BaseURL: "https://shtokendance.benlai.com"}}
	h := NewResourcesHandler(cfg, s, nil)
	if got := h.detailURL(resourceKindKnowledgeBase, "kb A/1"); got != "https://shtokendance.benlai.com/platform/knowledge-bases/kb%20A%2F1" {
		t.Fatalf("kb detail_url = %q", got)
	}
	if got := h.detailURL(resourceKindSkillCatalog, "cat 1"); got != "https://shtokendance.benlai.com/platform/toolbox/skills?catalog_id=cat+1" {
		t.Fatalf("skill detail_url = %q", got)
	}
	// 非 https / userinfo / 空 base 一律不出链接（绝不拼接存储路径或明文地址）。
	for _, base := range []string{"http://shtokendance.benlai.com", "https://user@host.example.com", "", "not a url"} {
		h := NewResourcesHandler(&config.Config{WeKnora: config.WeKnoraConfig{BaseURL: base}}, s, nil)
		if got := h.detailURL(resourceKindSkillCatalog, "c1"); got != "" {
			t.Fatalf("detail_url(%q) = %q, want empty", base, got)
		}
	}
	_ = r
}

// ---------- VAL-216 旧路由 404 ----------

func TestOldSkillRoutes_NotRegistered(t *testing.T) {
	fake := newFakeKnowledge()
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001")
	headers := platformAccessHeaders(t, s, link.ID)

	// 旧发布协调/分发路径全部 404（BND-010：不重定向、不暗中创建 release）。
	oldPaths := []string{
		"/integration/v1/skills",
		"/integration/v1/skills/rel-1/download",
		"/integration/v1/internal/skills?subject=subj-u1",
		"/integration/v1/admin/skills/publish-jobs",
		"/integration/v1/admin/skills/publish-jobs/00000000-0000-0000-0000-000000000000",
	}
	for _, p := range oldPaths {
		w := doGet(t, r, p, headers)
		if w.Code != http.StatusNotFound {
			t.Fatalf("old path %s = %d, want 404 (body=%s)", p, w.Code, w.Body.String())
		}
	}
	w := doPostJSON(t, r, "/integration/v1/admin/skills/publish-jobs", map[string]interface{}{
		"release_id": "rel-1", "source_tenant": "1001", "targets": []string{"1001"},
	}, headers)
	if w.Code != http.StatusNotFound {
		t.Fatalf("old POST publish-jobs = %d, want 404", w.Code)
	}
}
