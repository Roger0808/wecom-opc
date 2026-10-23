// WORK-RESOURCES-001（TASK-002 fixture）假 knowledge（httptest）：sqlite state +
// 假 WeKnora sqlite + 兼容桥。fake 解析桥 token 的 tenant_id 声明，按空间差异化
// 响应（模型化 knowledge 的空间可见性裁定）：
//   - KB 列表/目录列表按租户隔离（T1/T2 同名可区分、个人/企业样例）；
//   - 可注入租户级请求失败、延迟（超时）、目录归档未就绪/版本冲突/坏 hash/
//     途中断流、部分空间失败；
//   - release/publish 哨兵：任何 /api/v1/skills/releases* 或 catalog publish
//     调用计入 releaseCalls 并返回 404——新 adapter 绝不触达旧发布协议。
package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/authz"
	"weknora-tokendance/pkg/kbridge"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const testBridgeSecret = "knowledge-handler-test-bridge-secret"

// fakeArchiveError 目录归档错误注入（状态码 + INT-105 code）。
type fakeArchiveError struct {
	status int
	code   string
}

// fakeKnowledge 假 knowledge 服务。
type fakeKnowledge struct {
	mu sync.Mutex

	// KB 列表（按租户差异化；kbList 为缺省回退，保证旧单库用例不变）。
	kbList         []map[string]interface{}
	kbListByTenant map[string][]map[string]interface{}
	kbStatus       map[string]int // tenant → 强制状态码（请求失败注入）

	// hybrid-search 检索。
	searchHits   []map[string]interface{}
	searchStatus int
	searchDelay  time.Duration
	searchAuth   string
	searchBody   string
	searchTenant string

	// 知识正文/原文件（"<tenant>:<id>" → 数据；knowledgeAttrs 存权威归属）。
	knowledgeData  map[string]json.RawMessage
	knowledgeCode  map[string]int // "<tenant>:<id>" → 强制状态码
	knowledgeAttrs map[string]map[string]interface{}
	fileBytes      map[string][]byte
	fileStatus     map[string]int

	// 技能目录（按租户差异化）。
	catalogListByTenant map[string][]map[string]interface{}
	catalogListStatus   map[string]int              // tenant → 列表强制状态码
	catalogGetStatus    map[string]int              // "<tenant>:<catalog>" → 单项强制状态码
	catalogArchive      map[string][]byte           // "<tenant>:<catalog>" → 权威原始字节
	archiveErr          map[string]fakeArchiveError // "<tenant>:<catalog>" → 强制错误
	archiveTruncate     map[string]int              // "<tenant>:<catalog>" → 截断到 N 字节后断流
	archiveBadHeader    map[string]bool             // "<tenant>:<catalog>" → X-SHA256 头与字节不一致
	archiveDelay        time.Duration

	// release/publish 哨兵：新 adapter 收到即测试失败。
	releaseCalls int

	requestLog []string
}

func newFakeKnowledge() *fakeKnowledge {
	return &fakeKnowledge{
		searchStatus:        http.StatusOK,
		kbListByTenant:      map[string][]map[string]interface{}{},
		kbStatus:            map[string]int{},
		knowledgeData:       map[string]json.RawMessage{},
		knowledgeCode:       map[string]int{},
		knowledgeAttrs:      map[string]map[string]interface{}{},
		fileBytes:           map[string][]byte{},
		fileStatus:          map[string]int{},
		catalogListByTenant: map[string][]map[string]interface{}{},
		catalogListStatus:   map[string]int{},
		catalogGetStatus:    map[string]int{},
		catalogArchive:      map[string][]byte{},
		archiveErr:          map[string]fakeArchiveError{},
		archiveTruncate:     map[string]int{},
		archiveBadHeader:    map[string]bool{},
	}
}

// assertNoReleaseCalls 断言新链路运行期间 fake 未收到任何 release/publish
// 调用（TASK-002：收到即测试失败）。
func (f *fakeKnowledge) assertNoReleaseCalls(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.releaseCalls != 0 {
		t.Fatalf("fake knowledge received %d release/publish calls (old protocol must be gone): %v", f.releaseCalls, f.requestLog)
	}
}

// bearerTenant 解析请求 Bearer 桥 token 的 tenant_id 声明（测试专用）。
func bearerTenant(r *http.Request) string {
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (interface{}, error) {
		return []byte(testBridgeSecret), nil
	}, jwt.WithoutClaimsValidation())
	if err != nil {
		return ""
	}
	tenant, _ := claims["tenant_id"].(float64)
	return strconv.FormatUint(uint64(tenant), 10)
}

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:])
}

func (f *fakeKnowledge) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/knowledge-bases", f.handleKBList)
	mux.HandleFunc("/api/v1/knowledge-bases/", f.handleHybridSearch)
	mux.HandleFunc("/api/v1/skills/catalog", f.handleCatalogList)
	mux.HandleFunc("/api/v1/skills/catalog/", f.handleCatalogItem)
	mux.HandleFunc("/api/v1/skills/", f.handleSkillsSentinel)
	mux.HandleFunc("/api/v1/knowledge/", f.handleKnowledge)
	return mux
}

// catalogFixture 构造目录元数据样例（id/name/version/description/bundle_sha256）。
func catalogFixture(id, name, version, description, bundleSHA string) map[string]interface{} {
	return map[string]interface{}{
		"id":            id,
		"name":          name,
		"version":       version,
		"description":   description,
		"bundle_sha256": bundleSHA,
		"created_at":    time.Now().UTC().Format(time.RFC3339),
		"updated_at":    time.Now().UTC().Format(time.RFC3339),
		"installations": []interface{}{},
		"installed_on":  []interface{}{},
	}
}

// catalogSHA 读取夹具目录条目的 bundle_sha256。
func catalogSHA(entry map[string]interface{}) string {
	s, _ := entry["bundle_sha256"].(string)
	return s
}

// ---------- KB 列表 / hybrid-search ----------

func (f *fakeKnowledge) handleKBList(w http.ResponseWriter, r *http.Request) {
	tenant := bearerTenant(r)
	f.mu.Lock()
	f.requestLog = append(f.requestLog, "kb-list tenant="+tenant)
	if status, forced := f.kbStatus[tenant]; forced {
		f.mu.Unlock()
		writeFakeKnowledgeError(w, status, "UPSTREAM_UNAVAILABLE", "kb list failed")
		return
	}
	kbs := f.kbList
	if per, ok := f.kbListByTenant[tenant]; ok {
		kbs = per
	}
	f.mu.Unlock()
	if kbs == nil {
		kbs = []map[string]interface{}{{"id": "kb-default", "name": "默认知识库"}}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "data": kbs})
}

func (f *fakeKnowledge) handleHybridSearch(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.searchAuth = r.Header.Get("Authorization")
	f.searchTenant = bearerTenant(r)
	body, _ := io.ReadAll(r.Body)
	f.searchBody = string(body)
	hits, status, delay := f.searchHits, f.searchStatus, f.searchDelay
	f.requestLog = append(f.requestLog, "hybrid-search tenant="+f.searchTenant)
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if status != http.StatusOK {
		writeFakeKnowledgeError(w, status, "SPACE_FORBIDDEN", "kb access denied")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "data": hits})
}

// ---------- 技能目录（新链路） ----------

func (f *fakeKnowledge) tenantCatalogs(tenant string) []map[string]interface{} {
	if per, ok := f.catalogListByTenant[tenant]; ok {
		return per
	}
	return []map[string]interface{}{}
}

func (f *fakeKnowledge) handleCatalogList(w http.ResponseWriter, r *http.Request) {
	tenant := bearerTenant(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requestLog = append(f.requestLog, "catalog-list tenant="+tenant)
	if status, forced := f.catalogListStatus[tenant]; forced {
		writeFakeKnowledgeError(w, status, "UPSTREAM_UNAVAILABLE", "catalog list failed")
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"data":    f.tenantCatalogs(tenant),
	})
}

func (f *fakeKnowledge) handleCatalogItem(w http.ResponseWriter, r *http.Request) {
	tenant := bearerTenant(r)
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/skills/catalog/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	key := tenant + ":" + id

	f.mu.Lock()
	defer f.mu.Unlock()
	f.requestLog = append(f.requestLog, r.Method+" "+r.URL.Path+" tenant="+tenant)

	// 可见性：目录必须存在于该租户空间（模型化 knowledge 的空间归属裁定）。
	var entry map[string]interface{}
	for _, row := range f.tenantCatalogs(tenant) {
		if row["id"] == id {
			entry = row
			break
		}
	}
	if entry == nil {
		writeFakeKnowledgeError(w, http.StatusNotFound, "RESOURCE_NOT_ACCESSIBLE", "skill catalog is not accessible")
		return
	}

	if len(parts) == 2 && parts[1] == "archive" {
		f.handleCatalogArchiveLocked(w, r, tenant, id, key, entry)
		return
	}
	if len(parts) != 1 {
		writeFakeKnowledgeError(w, http.StatusNotFound, "RESOURCE_NOT_ACCESSIBLE", "skill catalog is not accessible")
		return
	}
	if status, forced := f.catalogGetStatus[key]; forced {
		writeFakeKnowledgeError(w, status, "UPSTREAM_ERROR", "catalog get failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "data": entry})
}

// handleCatalogArchiveLocked 模型化 knowledge 新归档路由（fake 侧已持锁）：
// expected_sha256 与当前目录 digest 不符 → 409 SKILL_VERSION_CHANGED；归档字节
// 缺失 → 409 ARCHIVE_NOT_READY；错误注入按 INT-105 code 上抛。
func (f *fakeKnowledge) handleCatalogArchiveLocked(w http.ResponseWriter, r *http.Request, tenant, id, key string, entry map[string]interface{}) {
	if forced, ok := f.archiveErr[key]; ok {
		writeFakeKnowledgeError(w, forced.status, forced.code, "archive error")
		return
	}
	want := catalogSHA(entry)
	got := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("expected_sha256")))
	if want == "" {
		writeFakeKnowledgeError(w, http.StatusConflict, "ARCHIVE_NOT_READY", "the archive of this skill catalog is not ready yet")
		return
	}
	if got != want {
		writeFakeKnowledgeError(w, http.StatusConflict, "SKILL_VERSION_CHANGED", "the skill catalog has a different version now")
		return
	}
	archive, ok := f.catalogArchive[key]
	if !ok || len(archive) == 0 {
		writeFakeKnowledgeError(w, http.StatusConflict, "ARCHIVE_NOT_READY", "the archive of this skill catalog is not ready yet")
		return
	}
	if f.archiveDelay > 0 {
		// 延迟必须在锁外 sleep 才不阻塞其他请求；此处先放锁由调用方负责——
		// 简化：直接 sleep（测试里archiveDelay用例独立运行）。
		time.Sleep(f.archiveDelay)
	}
	w.Header().Set("Content-Type", "application/zip")
	if f.archiveBadHeader[key] {
		w.Header().Set("X-SHA256", strings.Repeat("0", 64))
	} else {
		w.Header().Set("X-SHA256", sha256Of(archive))
	}
	w.Header().Set("Content-Disposition", `attachment; filename="skill-`+id+`.zip"`)
	if cut, ok := f.archiveTruncate[key]; ok && cut < len(archive) {
		// 声明完整长度后只写前缀：客户端读到 unexpected EOF（流中断注入）。
		w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(archive[:cut])
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(archive)
}

// ---------- release/publish 哨兵 ----------

// handleSkillsSentinel 旧 release/发布协议哨兵：任何触达即计入 releaseCalls
// 并返回 404（新 adapter 若调用旧协议，对应测试用 assertNoReleaseCalls 失败）。
func (f *fakeKnowledge) handleSkillsSentinel(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCalls++
	f.requestLog = append(f.requestLog, r.Method+" "+r.URL.Path+" tenant="+bearerTenant(r))
	writeFakeKnowledgeError(w, http.StatusNotFound, "RESOURCE_NOT_ACCESSIBLE", "release endpoints are gone")
}

// ---------- 知识正文/原文件 ----------

func (f *fakeKnowledge) handleKnowledge(w http.ResponseWriter, r *http.Request) {
	tenant := bearerTenant(r)
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/knowledge/"), "/download")
	isDownload := strings.HasSuffix(r.URL.Path, "/download")
	key := tenant + ":" + id
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requestLog = append(f.requestLog, r.Method+" "+r.URL.Path+" tenant="+tenant)
	data, hasData := f.knowledgeData[key]
	code, hasCode := f.knowledgeCode[key]
	fileBytes := f.fileBytes[key]
	fileStatus, hasFileStatus := f.fileStatus[key]

	if isDownload {
		status := code
		if !hasCode {
			status = http.StatusOK
		}
		if hasFileStatus {
			status = fileStatus
		}
		if status != http.StatusOK {
			writeFakeKnowledgeError(w, status, "SPACE_FORBIDDEN", "download requires contributor")
			return
		}
		if fileBytes == nil {
			writeFakeKnowledgeError(w, http.StatusNotFound, "NOT_FOUND", "knowledge not found")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="doc.pdf"`)
		_, _ = w.Write(fileBytes)
		return
	}

	if hasCode && code != http.StatusOK {
		writeFakeKnowledgeError(w, code, "SPACE_FORBIDDEN", "kb access denied")
		return
	}
	if !hasData {
		writeFakeKnowledgeError(w, http.StatusNotFound, "NOT_FOUND", "knowledge not found")
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "data": json.RawMessage(data)})
}

func writeFakeKnowledgeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": false,
		"error":   map[string]string{"code": code, "message": message},
	})
}

// ---------- 测试环境装配 ----------

// setupKnowledgeEnv 装配 sqlite 双库 + 桥 + 假 knowledge，注册知识接口组与
// 企业资源接口组。
func setupKnowledgeEnv(t *testing.T, fake *fakeKnowledge) (*gin.Engine, *storage.Storage, *httptest.Server) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	mainDB := openIntegrationTestSQLite(t)
	stateDB := openIntegrationTestSQLite(t)
	if err := mainDB.AutoMigrate(&model.WeKnoraUser{}, &model.WeKnoraTenantMember{}, &model.WeKnoraAuthToken{}, &model.WeKnoraTenant{}); err != nil {
		t.Fatalf("migrate main: %v", err)
	}
	if err := stateDB.AutoMigrate(&model.IdentityLink{}, &model.ManagedEntitlement{}, &model.Session{}, &model.AuditEvent{}); err != nil {
		t.Fatalf("migrate state: %v", err)
	}
	s := storage.NewStorageWithDBs(mainDB, stateDB)

	server := httptest.NewServer(fake.handler(t))
	t.Cleanup(server.Close)

	cfg := &config.Config{
		Auth:    config.AuthConfig{Key: testAuthKey},
		WeKnora: config.WeKnoraConfig{BaseURL: server.URL},
	}
	bridge := kbridge.New(
		kbridge.Config{SignKey: testBridgeSecret},
		s,
		authz.NewEntitlementChecker(s, nil),
		kbridge.NewWeKnoraStore(s),
	)
	r := gin.New()
	kh := NewKnowledgeHandler(cfg, s, bridge)
	r.POST("/integration/v1/knowledge/search", kh.Search)
	r.GET("/integration/v1/knowledge/:id", kh.GetKnowledge)
	r.GET("/integration/v1/knowledge/:id/file", kh.DownloadFile)
	rh := NewResourcesHandler(cfg, s, bridge)
	r.GET("/integration/v1/resources/knowledge-bases", rh.ListKnowledgeBases)
	r.GET("/integration/v1/resources/skills", rh.ListSkillCatalogs)
	r.GET("/integration/v1/resources/skills/:catalog_id", rh.GetSkillCatalog)
	r.GET("/integration/v1/resources/skills/:catalog_id/archive", rh.DownloadSkillCatalogArchive)
	return r, s, server
}

// seedKnowledgeSubject 供给映射用户（identity link + entitlements + 上游用户/成员关系）。
func seedKnowledgeSubject(t *testing.T, s *storage.Storage, subject, ku string, tenants ...string) *model.IdentityLink {
	t.Helper()
	link := insertIdentityLink(t, s, subject, ku)
	for _, tenant := range tenants {
		insertEntitlement(t, s, subject, tenant, "chat", 0, false)
	}
	seedKnowledgeUpstreamUser(t, s, ku, tenants...)
	return link
}

// seedKnowledgeUpstreamUser 写 WeKnora users + 各租户 active membership。
func seedKnowledgeUpstreamUser(t *testing.T, s *storage.Storage, ku string, tenants ...string) {
	t.Helper()
	ctx := context.Background()
	values := map[string]interface{}{
		"id":              ku,
		"username":        ku,
		"email":           ku + "@example.com",
		"password_hash":   "",
		"tenant_id":       1001,
		"is_active":       true,
		"is_system_admin": false,
		"preferences":     "{}",
	}
	if err := s.DB().WithContext(ctx).Model(&model.WeKnoraUser{}).Create(values).Error; err != nil {
		t.Fatalf("create upstream user: %v", err)
	}
	for _, tenant := range tenants {
		numeric, err := strconv.ParseUint(tenant, 10, 64)
		if err != nil {
			t.Fatalf("bad tenant %q: %v", tenant, err)
		}
		m := &model.WeKnoraTenantMember{UserID: ku, TenantID: numeric, Role: "contributor", Status: "active"}
		if err := s.DB().WithContext(ctx).Create(m).Error; err != nil {
			t.Fatalf("create membership: %v", err)
		}
	}
}

// seedTenantRow 写 WeKnora 租户行（tenant_name 显示用）。
func seedTenantRow(t *testing.T, s *storage.Storage, id uint64, name string) {
	t.Helper()
	if err := s.DB().WithContext(context.Background()).Create(&model.WeKnoraTenant{
		ID: id, Name: name, Status: "active",
	}).Error; err != nil {
		t.Fatalf("create tenant row: %v", err)
	}
}

// platformAccessHeaders 建会话 + 平台 access token。
func platformAccessHeaders(t *testing.T, s *storage.Storage, linkID string) map[string]string {
	t.Helper()
	sid := uuid.NewString()
	session := &model.Session{
		SessionID:           sid,
		IdentityLinkID:      linkID,
		FamilyID:            uuid.NewString(),
		RefreshVerifierHash: sha256Hex("verifier-" + sid),
		DeviceFingerprint:   "dev-1",
		ExpiresAt:           time.Now().Add(time.Hour),
	}
	if err := s.InsertSession(context.Background(), session); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": linkID, "sid": sid, "aud": "work", "type": "access",
		"exp": now.Add(15 * time.Minute).Unix(), "iat": now.Unix(), "jti": uuid.NewString(),
	})
	raw, err := token.SignedString([]byte(testAuthKey))
	if err != nil {
		t.Fatalf("sign platform token: %v", err)
	}
	return map[string]string{"Authorization": "Bearer " + raw}
}
