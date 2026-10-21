// TASK-P07（INT-120）测试：假 knowledge（httptest）+ sqlite state + 假 WeKnora
// sqlite + 兼容桥。覆盖：search 规范化与重授权、read/file 逐空间可见性与
// Viewer 下载 403 透传、桥超时 502 UPSTREAM_UNAVAILABLE、缺Bearer 401。
package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"weknora-tokendance/pkg/model"

	"github.com/gin-gonic/gin"
)

// ---------- POST /integration/v1/knowledge/search ----------

func TestKnowledgeSearch_Normalized(t *testing.T) {
	fake := newFakeKnowledge()
	fake.searchHits = []map[string]interface{}{
		{"id": "chunk-1", "knowledge_id": "kn-1", "knowledge_title": "报销制度", "content": "差旅标准……",
			"chunk_index": 3, "start_at": 120, "end_at": 240, "seq": 1, "score": 0.87},
	}
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-k", "ku-k", "1001")

	w := doPostJSON(t, r, "/integration/v1/knowledge/search", gin.H{
		"tenant_id": "1001", "knowledge_base_id": "kb-9", "query": "差旅报销",
	}, platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("search = %d, body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["source_tenant"] != "1001" || body["knowledge_base_id"] != "kb-9" {
		t.Fatalf("envelope mismatch: %v", body)
	}
	results, ok := body["results"].([]interface{})
	if !ok || len(results) != 1 {
		t.Fatalf("results = %v", body["results"])
	}
	hit := results[0].(map[string]interface{})
	if hit["knowledge_id"] != "kn-1" || hit["chunk_id"] != "chunk-1" || hit["title"] != "报销制度" {
		t.Fatalf("hit identity fields = %v", hit)
	}
	if hit["source_tenant"] != "1001" {
		t.Fatalf("hit source_tenant = %v", hit)
	}
	locator, ok := hit["locator"].(map[string]interface{})
	if !ok || locator["start_at"] != float64(120) || locator["end_at"] != float64(240) || locator["chunk_index"] != float64(3) {
		t.Fatalf("locator = %v", hit["locator"])
	}
	// 桥 token 走 Bearer；请求体携带 query_text；上游可见 tenant_id=1001。
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !strings.HasPrefix(fake.searchAuth, "Bearer ") {
		t.Fatalf("upstream Authorization = %q", fake.searchAuth)
	}
	if !strings.Contains(fake.searchBody, "差旅报销") || !strings.Contains(fake.searchBody, "query_text") {
		t.Fatalf("upstream body = %q", fake.searchBody)
	}
	if fake.searchTenant != "1001" {
		t.Fatalf("upstream tenant claim = %q, want 1001", fake.searchTenant)
	}
}

func TestKnowledgeSearch_TenantAliasSpace(t *testing.T) {
	fake := newFakeKnowledge()
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-k", "ku-k", "1001")
	w := doPostJSON(t, r, "/integration/v1/knowledge/search", gin.H{
		"space": "1001", "knowledge_base_id": "kb-9", "query": "q",
	}, platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("space alias = %d, body=%s", w.Code, w.Body.String())
	}
}

func TestKnowledgeSearch_ReauthDenied(t *testing.T) {
	// 无 entitlement：桥拒绝，403 SPACE_FORBIDDEN，不触达上游。
	fake := newFakeKnowledge()
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := insertIdentityLink(t, s, "subj-k", "ku-k")
	seedKnowledgeUpstreamUser(t, s, "ku-k", "1001")

	w := doPostJSON(t, r, "/integration/v1/knowledge/search", gin.H{
		"tenant_id": "1001", "knowledge_base_id": "kb-9", "query": "q",
	}, platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeSpaceForbidden {
		t.Fatalf("no entitlement = %d/%s", w.Code, errorCode(t, w))
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requestLog) != 0 {
		t.Fatalf("upstream called despite denial: %v", fake.requestLog)
	}

	// identity link 停用：403 SUBJECT_NOT_PROVISIONED。
	link2 := insertIdentityLink(t, s, "subj-x", "ku-x")
	insertEntitlement(t, s, "subj-x", "1001", "chat", 0, false)
	seedKnowledgeUpstreamUser(t, s, "ku-x", "1001")
	s.StateDB().Model(&model.IdentityLink{}).Where("id = ?", link2.ID).Update("status", "disabled")
	w = doPostJSON(t, r, "/integration/v1/knowledge/search", gin.H{
		"tenant_id": "1001", "knowledge_base_id": "kb-9", "query": "q",
	}, platformAccessHeaders(t, s, link2.ID))
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeSubjectNotProv {
		t.Fatalf("disabled identity = %d/%s", w.Code, errorCode(t, w))
	}
}

func TestKnowledgeSearch_UpstreamErrorMapping(t *testing.T) {
	// knowledge 403 → 平台透传 SPACE_FORBIDDEN。
	fake := newFakeKnowledge()
	fake.searchStatus = http.StatusForbidden
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-k", "ku-k", "1001")
	w := doPostJSON(t, r, "/integration/v1/knowledge/search", gin.H{
		"tenant_id": "1001", "knowledge_base_id": "kb-9", "query": "q",
	}, platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeSpaceForbidden {
		t.Fatalf("upstream 403 = %d/%s", w.Code, errorCode(t, w))
	}
}

func TestKnowledgeSearch_UpstreamTimeout(t *testing.T) {
	fake := newFakeKnowledge()
	fake.searchDelay = 300 * time.Millisecond
	old := upstreamTimeout
	upstreamTimeout = 50 * time.Millisecond
	t.Cleanup(func() { upstreamTimeout = old })
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-k", "ku-k", "1001")
	w := doPostJSON(t, r, "/integration/v1/knowledge/search", gin.H{
		"tenant_id": "1001", "knowledge_base_id": "kb-9", "query": "q",
	}, platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusBadGateway || errorCode(t, w) != errCodeUpstreamUnavailable {
		t.Fatalf("timeout = %d/%s, want 502/%s", w.Code, errorCode(t, w), errCodeUpstreamUnavailable)
	}
}

func TestKnowledgeSearch_MissingBearer(t *testing.T) {
	fake := newFakeKnowledge()
	r, _, _ := setupKnowledgeEnv(t, fake)
	w := doPostJSON(t, r, "/integration/v1/knowledge/search", gin.H{
		"tenant_id": "1001", "knowledge_base_id": "kb-9", "query": "q",
	}, nil)
	if w.Code != http.StatusUnauthorized || errorCode(t, w) != errCodeSessionRevoked {
		t.Fatalf("missing bearer = %d/%s", w.Code, errorCode(t, w))
	}
}

func TestKnowledgeSearch_MissingFields(t *testing.T) {
	fake := newFakeKnowledge()
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-k", "ku-k", "1001")
	w := doPostJSON(t, r, "/integration/v1/knowledge/search", gin.H{"query": "q"}, platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusBadRequest || errorCode(t, w) != errCodeInvalidRequest {
		t.Fatalf("missing fields = %d/%s", w.Code, errorCode(t, w))
	}
}

// ---------- GET /integration/v1/knowledge/:id ----------

func TestKnowledgeGet_IteratesEntitledTenants(t *testing.T) {
	fake := newFakeKnowledge()
	// 1001 不可见（404），1002 可见。
	fake.knowledgeCode["1001:kn-1"] = http.StatusNotFound
	fake.knowledgeData["1002:kn-1"] = json.RawMessage(`{"id":"kn-1","title":"制度"}`)
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-k", "ku-k", "1001", "1002")

	w := doGet(t, r, "/integration/v1/knowledge/kn-1", platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d, body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["source_tenant"] != "1002" || body["knowledge_id"] != "kn-1" {
		t.Fatalf("response = %v", body)
	}
}

func TestKnowledgeGet_ExplicitTenantReauth(t *testing.T) {
	fake := newFakeKnowledge()
	fake.knowledgeData["1002:kn-1"] = json.RawMessage(`{"id":"kn-1"}`)
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-k", "ku-k", "1001", "1002")

	// 显式租户已授权 → 200。
	w := doGet(t, r, "/integration/v1/knowledge/kn-1?tenant=1002", platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("explicit entitled tenant = %d, body=%s", w.Code, w.Body.String())
	}
	// 显式租户未授权 → 403 SPACE_FORBIDDEN。
	w = doGet(t, r, "/integration/v1/knowledge/kn-1?tenant=9999", platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeSpaceForbidden {
		t.Fatalf("explicit unentitled tenant = %d/%s", w.Code, errorCode(t, w))
	}
}

func TestKnowledgeGet_NotVisibleAnywhere(t *testing.T) {
	fake := newFakeKnowledge()
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-k", "ku-k", "1001")
	w := doGet(t, r, "/integration/v1/knowledge/kn-404", platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeSpaceForbidden {
		t.Fatalf("not visible = %d/%s", w.Code, errorCode(t, w))
	}
}

// ---------- GET /integration/v1/knowledge/:id/file ----------

func TestKnowledgeFile_DownloadOK(t *testing.T) {
	fake := newFakeKnowledge()
	fake.fileBytes["1001:kn-1"] = []byte("PDF-BYTES")
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-k", "ku-k", "1001")

	w := doGet(t, r, "/integration/v1/knowledge/kn-1/file", platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("download = %d, body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != "PDF-BYTES" {
		t.Fatalf("body = %q", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("content-type = %q", ct)
	}
}

func TestKnowledgeFile_ViewerForbiddenPassthrough(t *testing.T) {
	// knowledge 对 Viewer 返 403 → 平台透传 403 SPACE_FORBIDDEN（预览权不升级为下载权）。
	fake := newFakeKnowledge()
	fake.knowledgeCode["1001:kn-1"] = http.StatusForbidden
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-k", "ku-k", "1001")
	w := doGet(t, r, "/integration/v1/knowledge/kn-1/file", platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeSpaceForbidden {
		t.Fatalf("viewer download = %d/%s", w.Code, errorCode(t, w))
	}
}

func TestKnowledgeFile_NotVisibleAnywhere(t *testing.T) {
	fake := newFakeKnowledge()
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-k", "ku-k", "1001")
	w := doGet(t, r, "/integration/v1/knowledge/kn-9/file", platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeSpaceForbidden {
		t.Fatalf("not visible file = %d/%s", w.Code, errorCode(t, w))
	}
}
