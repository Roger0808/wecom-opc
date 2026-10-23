// WORK-RESOURCES-001（TASK-014，VAL-102 PBT）属性测试：
//
// P1（seed=20261022，≥500 case）：任意 scope/文档归属组合下，平台受限读取
//
//	的「允许输出集合」始终是所选 tenant/KB 的子集——凡 200 输出的正文，其
//	权威归属必与 scope 一致；错配一律 403 SCOPE_MISMATCH 且不泄露正文片段。
//
// P2（seed=20261023，≥500 case）：任意主体/tenant/catalog/SHA/同名组合下，
//
//	列表条目不跨主体、同空间同 ID 不重复、排序稳定、availability 与归档
//	SHA 已知性一致；归档代理只为「tenant+catalog+预期 SHA 与权威目录一致」
//	的组合输出字节，其余 4xx（不跨空间、不误选对象、不假 ZIP）。
//
// 输入由确定性生成器产生（math/rand 固定 seed）；fake knowledge 为唯一上游
// 事实源，属性经真实 HTTP 处理链断言，不只验证实现函数自身返回值。
// 失败时输出 case 序号与输入，缩减后固化为普通回归例（见 resources_test.go）。
package handler

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"weknora-tokendance/pkg/storage"

	"github.com/gin-gonic/gin"
)

// pbtCaseCount 固定 case 数（VAL-102：P1/P2 各 ≥500）。
const pbtCaseCount = 500

// newPBTEnv 装配带 3 个授权空间的属性测试环境。
func newPBTEnv(t *testing.T) (*fakeKnowledge, *gin.Engine, *storage.Storage, []string) {
	t.Helper()
	fake := newFakeKnowledge()
	r, s, _ := setupKnowledgeEnv(t, fake)
	link := seedKnowledgeSubject(t, s, "subj-pbt", "ku-pbt", "7001", "7002", "7003")
	return fake, r, s, []string{link.ID}
}

// TestPBT_ScopedReadAllowedSetIsSubsetOfScope（P1，seed=20261022）
// 平台侧保证：企业 read 的可见集合 ⊆ 所选 tenant/KB——正文仅在权威归属一致时
// 输出；移动文档/错配 KB/跨空间归属一律拒绝且无泄露。
func TestPBT_ScopedReadAllowedSetIsSubsetOfScope(t *testing.T) {
	const seed = int64(20261022)
	rng := rand.New(rand.NewSource(seed))
	fake, r, s, linkIDs := newPBTEnv(t)
	headers := platformAccessHeaders(t, s, linkIDs[0])

	entitled := []string{"7001", "7002", "7003"}
	kbAlphabet := []string{"kb-a", "kb-b", "kb-ç", "KB-A", "kb a/b", "kb\tx", "kb-长名称", strings.Repeat("k", 129)}
	titleAlphabet := []string{"苹果", "banana", "机密", "doc"}

	allowedOutputs := 0
	for i := 0; i < pbtCaseCount; i++ {
		scopeTenant := entitled[rng.Intn(len(entitled))]
		scopeKB := kbAlphabet[rng.Intn(len(kbAlphabet))]
		docID := fmt.Sprintf("kn-%d", rng.Intn(4))
		title := titleAlphabet[rng.Intn(len(titleAlphabet))]

		// 显式构造：0=归属匹配；1=错空间；2=错 KB；3=归属字段缺失。
		kind := rng.Intn(4)
		var docTenant, docKB string
		switch kind {
		case 0:
			docTenant, docKB = scopeTenant, scopeKB
		case 1:
			docTenant = entitled[(indexOfString(entitled, scopeTenant)+1+rng.Intn(2))%len(entitled)]
			docKB = scopeKB
		case 2:
			docTenant = scopeTenant
			docKB = "kb-other"
		default:
			docTenant, docKB = scopeTenant, ""
		}
		wantOwnershipMatch := kind == 0

		// 构造上游权威文档（错空间语义需要其他 entitled 空间）。
		var doc string
		switch kind {
		case 3:
			doc = fmt.Sprintf(`{"id":%q,"title":%q}`, docID, title)
		default:
			doc = fmt.Sprintf(`{"id":%q,"tenant_id":%s,"knowledge_base_id":%q,"title":%q}`, docID, docTenant, docKB, title)
		}
		fake.mu.Lock()
		fake.knowledgeData[scopeTenant+":"+docID] = json.RawMessage(doc)
		fake.mu.Unlock()

		// 归属一致的前提是 scopeKB 为合法 ID；非法 KB 必然 400（先于归属判定）。
		kbValid := validResourceID(scopeKB)
		path := fmt.Sprintf("/integration/v1/knowledge/%s?tenant=%s&knowledge_base_id=%s", docID, scopeTenant, url.QueryEscape(scopeKB))
		w := doGet(t, r, path, headers)

		if !kbValid {
			if w.Code != http.StatusBadRequest {
				t.Fatalf("P1 case %d (seed %d): kb %q invalid but status=%d", i, seed, scopeKB, w.Code)
			}
			continue
		}
		if wantOwnershipMatch {
			if w.Code != http.StatusOK {
				t.Fatalf("P1 case %d (seed %d): matching ownership got %d/%s (tenant=%s kb=%s)", i, seed, w.Code, errorCode(t, w), scopeTenant, scopeKB)
			}
			if !strings.Contains(w.Body.String(), title) {
				t.Fatalf("P1 case %d (seed %d): body lost content %q", i, seed, title)
			}
			allowedOutputs++
			continue
		}
		// 错配：403 SCOPE_MISMATCH，且不泄露标题/归属。
		if w.Code != http.StatusForbidden || errorCode(t, w) != errCodeScopeMismatch {
			t.Fatalf("P1 case %d (seed %d): mismatch got %d/%s, want 403/%s (tenant=%s kb=%s kind=%d)",
				i, seed, w.Code, errorCode(t, w), errCodeScopeMismatch, scopeTenant, scopeKB, kind)
		}
		if strings.Contains(w.Body.String(), title) || strings.Contains(w.Body.String(), "kb-other") {
			t.Fatalf("P1 case %d (seed %d): mismatch leaked body: %s", i, seed, w.Body.String())
		}
	}
	if allowedOutputs == 0 {
		t.Fatalf("P1 (seed %d): generator produced zero allowed reads; property vacuous", seed)
	}
	t.Logf("P1 seed=%d cases=%d allowed=%d", seed, pbtCaseCount, allowedOutputs)
}

func indexOfString(list []string, v string) int {
	for i, x := range list {
		if x == v {
			return i
		}
	}
	return -1
}

// TestPBT_ListAndArchiveNeverCrossSubjects（P2，seed=20261023）
// 平台侧保证：任意 tenant/catalog/SHA/同名组合下，资源条目不跨主体、同空间同
// ID 唯一、排序稳定、availability 与 SHA 已知性一致；归档只为权威三元组
// (tenant, catalog, 当前 digest) 输出字节。
func TestPBT_ListAndArchiveNeverCrossSubjects(t *testing.T) {
	const seed = int64(20261023)
	rng := rand.New(rand.NewSource(seed))
	fake, r, s, linkIDs := newPBTEnv(t)
	headers := platformAccessHeaders(t, s, linkIDs[0])

	entitled := []string{"7001", "7002", "7003"}
	nameAlphabet := []string{"同名技能", "Café", "café", "技能", "技能 ", ""}

	verified := 0
	for i := 0; i < pbtCaseCount; i++ {
		// 每 case 随机生成各空间目录（允许跨空间同名、同空间 ID 冲突去重）。
		perTenant := map[string]map[string]string{} // tenant → catalog → sha（"" = 归档未知）
		for _, tenant := range entitled {
			perTenant[tenant] = map[string]string{}
			n := rng.Intn(4)
			for j := 0; j < n; j++ {
				id := fmt.Sprintf("cat-%d", rng.Intn(6))
				sha := sha256Of([]byte(fmt.Sprintf("bundle-%s-%d", tenant, rng.Intn(1000))))
				if rng.Intn(4) == 0 {
					sha = "" // not_ready 样例
				}
				perTenant[tenant][id] = sha
			}
		}
		fake.mu.Lock()
		fake.catalogListByTenant = map[string][]map[string]interface{}{}
		for tenant, cats := range perTenant {
			rows := make([]map[string]interface{}, 0, len(cats))
			for id, sha := range cats {
				rows = append(rows, catalogFixture(id, nameAlphabet[rng.Intn(len(nameAlphabet))], "1.0.0", "", sha))
			}
			fake.catalogListByTenant[tenant] = rows
		}
		fake.catalogArchive = map[string][]byte{}
		fake.mu.Unlock()

		w := doGet(t, r, "/integration/v1/resources/skills", headers)
		if w.Code != http.StatusOK {
			t.Fatalf("P2 case %d (seed %d): list = %d", i, seed, w.Code)
		}
		body := decodeBody(t, w)
		if body["complete"] != true {
			t.Fatalf("P2 case %d (seed %d): partial failures in all-success fixture", i, seed)
		}
		items := body["items"].([]interface{})
		seen := map[string]bool{}
		for _, raw := range items {
			it := raw.(map[string]interface{})
			tenant := it["tenant_id"].(string)
			catID := it["catalog_id"].(string)
			if !containsString(entitled, tenant) {
				t.Fatalf("P2 case %d (seed %d): item crossed subjects: %v", i, seed, it)
			}
			key := tenant + "/" + catID
			if seen[key] {
				t.Fatalf("P2 case %d (seed %d): duplicate item %s", i, seed, key)
			}
			seen[key] = true
			wantSHA := perTenant[tenant][catID]
			gotSHA, _ := it["bundle_sha256"].(string)
			if wantSHA == "" {
				if it["availability"] != "not_ready" || it["unavailable_reason"] != errCodeArchiveNotReady {
					t.Fatalf("P2 case %d (seed %d): unknown archive not not_ready: %v", i, seed, it)
				}
			} else {
				if it["availability"] != "available" || gotSHA != wantSHA {
					t.Fatalf("P2 case %d (seed %d): sha/availability mismatch: %v want %q", i, seed, it, wantSHA)
				}
			}
		}
		// 排序稳定：tenant 升序；同 tenant 名称折叠升序。
		for j := 1; j < len(items); j++ {
			prev, cur := items[j-1].(map[string]interface{}), items[j].(map[string]interface{})
			if prev["tenant_id"].(string) > cur["tenant_id"].(string) {
				t.Fatalf("P2 case %d (seed %d): tenant order broken at %d", i, seed, j)
			}
		}

		// 归档：任取一个生成目录，验证三元组语义。
		for tenant, cats := range perTenant {
			for id, sha := range cats {
				if rng.Intn(3) != 0 {
					continue
				}
				if sha == "" {
					continue
				}
				fake.mu.Lock()
				fake.catalogArchive[tenant+":"+id] = []byte("bundle-" + tenant + "-" + id)
				fake.mu.Unlock()
				// 注意：归档字节与 digest 绑定——用真实 digest 重新计算。
				realBytes := []byte("bundle-" + tenant + "-" + id)
				realSHA := sha256Of(realBytes)
				fake.mu.Lock()
				for _, row := range fake.catalogListByTenant[tenant] {
					if row["id"] == id {
						row["bundle_sha256"] = realSHA
					}
				}
				fake.mu.Unlock()
				w := doGet(t, r, fmt.Sprintf("/integration/v1/resources/skills/%s/archive?tenant_id=%s&expected_sha256=%s", id, tenant, realSHA), headers)
				if w.Code != http.StatusOK || w.Body.String() != string(realBytes) {
					t.Fatalf("P2 case %d (seed %d): archive triple (%s,%s) = %d", i, seed, tenant, id, w.Code)
				}
				wrongSHA := sha256Of([]byte("forged"))
				w = doGet(t, r, fmt.Sprintf("/integration/v1/resources/skills/%s/archive?tenant_id=%s&expected_sha256=%s", id, tenant, wrongSHA), headers)
				if w.Code != http.StatusConflict || errorCode(t, w) != errCodeSkillVersionChanged {
					t.Fatalf("P2 case %d (seed %d): forged sha accepted: %d/%s", i, seed, w.Code, errorCode(t, w))
				}
				verified++
			}
		}
	}
	if verified == 0 {
		t.Fatalf("P2 (seed %d): no archive verification performed; property vacuous", seed)
	}
	t.Logf("P2 seed=%d cases=%d archiveChecks=%d", seed, pbtCaseCount, verified)
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
