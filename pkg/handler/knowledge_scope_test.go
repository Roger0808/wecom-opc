// WORK-RESOURCES-001（TASK-011）平台受限知识读取测试：VAL-205/207（BND-005 /
// INT-104）。显式 scope 模式只用指定 tenant/KB；权威结果校验实际归属后再输出
// 正文；scope 错配 403 SCOPE_MISMATCH 且不泄露正文；绝不遍历其他空间补结果。
package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// knowledgeDocFixture 构造带权威归属字段的上游文档 JSON。
func knowledgeDocFixture(id string, tenant uint64, kb string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"id":%q,"tenant_id":%d,"knowledge_base_id":%q,"title":"文档"}`, id, tenant, kb))
}

func TestKnowledgeGet_ScopedReadOK(t *testing.T) {
	fake := newFakeKnowledge()
	fake.knowledgeData["1001:kn-1"] = knowledgeDocFixture("kn-1", 1001, "A")
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001", "1002")

	w := doGet(t, r, "/integration/v1/knowledge/kn-1?tenant=1001&knowledge_base_id=A",
		platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("scoped read = %d, body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["source_tenant"] != "1001" {
		t.Fatalf("source_tenant = %v", body["source_tenant"])
	}
	if !strings.Contains(w.Body.String(), "文档") {
		t.Fatalf("body missing content: %s", w.Body.String())
	}
}

func TestKnowledgeGet_ScopeMismatchLeaksNothing(t *testing.T) {
	fake := newFakeKnowledge()
	// 权威结果：文档实际属于 T1 的 B 库，scope 请求的是 A 库（VAL-205）。
	fake.knowledgeData["1001:kn-1"] = knowledgeDocFixture("kn-1", 1001, "B")
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001", "1002")

	w := doGet(t, r, "/integration/v1/knowledge/kn-1?tenant=1001&knowledge_base_id=A",
		platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeScopeMismatch {
		t.Fatalf("scope mismatch = %d/%s, want 403/%s", w.Code, errorCode(t, w), errCodeScopeMismatch)
	}
	// 正文/片段不泄露：错误信封不含文档标题或 KB 归属。
	if strings.Contains(w.Body.String(), "文档") || strings.Contains(w.Body.String(), `"knowledge_base_id"`) {
		t.Fatalf("scoped body leaked: %s", w.Body.String())
	}
	// 越界读取只发生在 scope 空间的一次请求，无额外检索/遍历。
	fake.mu.Lock()
	upstreamGets := 0
	for _, entry := range fake.requestLog {
		if strings.HasPrefix(entry, "GET /api/v1/knowledge/") {
			upstreamGets++
		}
	}
	fake.mu.Unlock()
	if upstreamGets != 1 {
		t.Fatalf("upstream knowledge reads = %d, want exactly 1 (no traversal)", upstreamGets)
	}
}

func TestKnowledgeGet_ScopeTenantMismatchLeaksNothing(t *testing.T) {
	fake := newFakeKnowledge()
	// 元数据/正文归属变化：上游在 scope 空间返回的文档实际属于另一空间（VAL-207）。
	fake.knowledgeData["1001:kn-1"] = knowledgeDocFixture("kn-1", 1002, "A")
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001", "1002")

	w := doGet(t, r, "/integration/v1/knowledge/kn-1?tenant=1001&knowledge_base_id=A",
		platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeScopeMismatch {
		t.Fatalf("tenant mismatch = %d/%s, want 403/%s", w.Code, errorCode(t, w), errCodeScopeMismatch)
	}
	if strings.Contains(w.Body.String(), "文档") {
		t.Fatalf("tenant mismatch leaked body: %s", w.Body.String())
	}
}

func TestKnowledgeGet_ScopedInvisibleIs404NotTraversal(t *testing.T) {
	fake := newFakeKnowledge()
	// 文档只在 T2 可见；scope 指定 T1 的 A 库 → 404，绝不遍历 T2 补结果。
	fake.knowledgeData["1002:kn-1"] = knowledgeDocFixture("kn-1", 1002, "A")
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001", "1002")

	w := doGet(t, r, "/integration/v1/knowledge/kn-1?tenant=1001&knowledge_base_id=A",
		platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusNotFound || errorCode(t, w) != errCodeResourceNotAccessible {
		t.Fatalf("scoped invisible = %d/%s, want 404/%s", w.Code, errorCode(t, w), errCodeResourceNotAccessible)
	}
	fake.mu.Lock()
	upstreamGets := 0
	for _, entry := range fake.requestLog {
		if strings.HasPrefix(entry, "GET /api/v1/knowledge/") {
			upstreamGets++
		}
	}
	fake.mu.Unlock()
	if upstreamGets != 1 {
		t.Fatalf("upstream knowledge reads = %d, want 1 (no cross-tenant traversal)", upstreamGets)
	}
}

func TestKnowledgeGet_ScopeValidation(t *testing.T) {
	fake := newFakeKnowledge()
	fake.knowledgeData["1001:kn-1"] = knowledgeDocFixture("kn-1", 1001, "A")
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001")
	headers := platformAccessHeaders(t, s, link.ID)

	// knowledge_base_id 单独出现（缺 tenant）→ 400。
	if w := doGet(t, r, "/integration/v1/knowledge/kn-1?knowledge_base_id=A", headers); w.Code != http.StatusBadRequest {
		t.Fatalf("kb without tenant = %d", w.Code)
	}
	// scope 指定无权空间 → 403 SPACE_FORBIDDEN。
	if w := doGet(t, r, "/integration/v1/knowledge/kn-1?tenant=9999&knowledge_base_id=A", headers); w.Code != http.StatusForbidden {
		t.Fatalf("unentitled scope tenant = %d", w.Code)
	}
	// 恶意 knowledge_base_id → 400。
	if w := doGet(t, r, "/integration/v1/knowledge/kn-1?tenant=1001&knowledge_base_id=a%09b", headers); w.Code != http.StatusBadRequest {
		t.Fatalf("hostile kb id = %d", w.Code)
	}
	// 缺归属字段的上游结果 → fail-closed 403（不凭请求参数补造）。
	fake.mu.Lock()
	fake.knowledgeData["1001:kn-2"] = json.RawMessage(`{"id":"kn-2","title":"无归属字段"}`)
	fake.mu.Unlock()
	w := doGet(t, r, "/integration/v1/knowledge/kn-2?tenant=1001&knowledge_base_id=A", headers)
	if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeScopeMismatch {
		t.Fatalf("missing ownership = %d/%s, want 403/%s", w.Code, errorCode(t, w), errCodeScopeMismatch)
	}
}

func TestKnowledgeGet_DefaultModeUnchanged(t *testing.T) {
	fake := newFakeKnowledge()
	// 无 scope 调用保持既有行为：逐空间首个可见命中（不校验 KB 归属）。
	fake.knowledgeData["1002:kn-1"] = knowledgeDocFixture("kn-1", 1002, "B")
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-u1", "ku-u1", "1001", "1002")

	w := doGet(t, r, "/integration/v1/knowledge/kn-1", platformAccessHeaders(t, s, link.ID))
	if w.Code != http.StatusOK || decodeBody(t, w)["source_tenant"] != "1002" {
		t.Fatalf("default mode = %d, body=%s", w.Code, w.Body.String())
	}
}
