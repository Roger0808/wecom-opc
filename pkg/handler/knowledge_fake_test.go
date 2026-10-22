// TASK-P07（INT-120）与 TASK-P10（INT-141/142）测试共用假 knowledge（httptest）：
// sqlite state + 假 WeKnora sqlite + 兼容桥。fake 解析桥 token 的 tenant_id
// 声明，按空间差异化响应（模型化 knowledge 的空间可见性裁定）。
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
	"weknora-tokendance/pkg/adminauth"
	"weknora-tokendance/pkg/authz"
	"weknora-tokendance/pkg/kbridge"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const testBridgeSecret = "knowledge-handler-test-bridge-secret"

// fakeKnowledge 假 knowledge 服务。
type fakeKnowledge struct {
	mu sync.Mutex

	searchHits          []map[string]interface{}
	searchStatus        int
	searchDelay         time.Duration
	searchAuth          string
	searchBody          string
	searchTenant        string
	knowledgeData       map[string]json.RawMessage // "<tenant>:<id>" → data（缺省 404）
	knowledgeCode       map[string]int             // "<tenant>:<id>" → 状态码（403 等）
	fileBytes           map[string][]byte          // "<tenant>:<id>" → 文件字节（缺省 404）
	fileStatus          map[string]int             // "<tenant>:<id>" → 强制状态码
	releaseByID         map[string]map[string]interface{}
	releaseListByTenant map[string][]map[string]interface{}
	kbList              []map[string]interface{} // 显式 KB 列表（缺省单占位库）
	metadataStatus      map[string]int // release id → 强制元数据状态码
	archiveBytes        map[string][]byte
	archiveStatus       map[string]int // release id → 强制状态码
	requestLog          []string
}

func newFakeKnowledge() *fakeKnowledge {
	return &fakeKnowledge{
		searchStatus:        http.StatusOK,
		knowledgeData:       map[string]json.RawMessage{},
		knowledgeCode:       map[string]int{},
		fileBytes:           map[string][]byte{},
		fileStatus:          map[string]int{},
		releaseByID:         map[string]map[string]interface{}{},
		releaseListByTenant: map[string][]map[string]interface{}{},
		metadataStatus:      map[string]int{},
		archiveBytes:        map[string][]byte{},
		archiveStatus:       map[string]int{},
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

// releaseVisibleInTenant 模型化 knowledge「源空间可见 + target_tenants 可见」。
// target_tenants 兼容 JSON 解码的 []interface{} 与测试夹具直接构造的 []float64。
func releaseVisibleInTenant(release map[string]interface{}, tenant string) bool {
	if release["source_tenant"] == tenant {
		return true
	}
	switch targets := release["target_tenants"].(type) {
	case []interface{}:
		for _, t := range targets {
			if strconv.FormatUint(uint64(t.(float64)), 10) == tenant {
				return true
			}
		}
	case []float64:
		for _, t := range targets {
			if strconv.FormatUint(uint64(t), 10) == tenant {
				return true
			}
		}
	}
	return false
}

func (f *fakeKnowledge) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/knowledge-bases", f.handleKBList)
	mux.HandleFunc("/api/v1/knowledge-bases/", f.handleHybridSearch)
	mux.HandleFunc("/api/v1/skills/", f.handleSkills)
	mux.HandleFunc("/api/v1/knowledge/", f.handleKnowledge)
	return mux
}

// handleKBList GET /api/v1/knowledge-bases：返回 fake 的 KB 列表（无列表时
// 退化为单个占位库，保证缺省 knowledge_base_id 的旧用例仍按单库路径走）。
func (f *fakeKnowledge) handleKBList(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	kbs := f.kbList
	f.requestLog = append(f.requestLog, "kb-list tenant="+bearerTenant(r))
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

func (f *fakeKnowledge) handleKnowledge(w http.ResponseWriter, r *http.Request) {
	tenant := bearerTenant(r)
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/knowledge/"), "/download")
	isDownload := strings.HasSuffix(r.URL.Path, "/download")
	key := tenant + ":" + id
	f.mu.Lock()
	f.requestLog = append(f.requestLog, r.Method+" "+r.URL.Path+" tenant="+tenant)
	data, hasData := f.knowledgeData[key]
	code, hasCode := f.knowledgeCode[key]
	fileBytes := f.fileBytes[key]
	fileStatus, hasFileStatus := f.fileStatus[key]
	f.mu.Unlock()

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

// handleSkills 假 EXT-001 release 接口（按租户可见性 + 归档字节/状态）。
func (f *fakeKnowledge) handleSkills(w http.ResponseWriter, r *http.Request) {
	tenant := bearerTenant(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requestLog = append(f.requestLog, r.Method+" "+r.URL.Path+" tenant="+tenant)

	if r.URL.Path == "/api/v1/skills/releases" {
		list := f.releaseListByTenant[tenant]
		if list == nil {
			list = []map[string]interface{}{}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "data": list})
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/skills/releases/")
	parts := strings.Split(rest, "/")
	id := parts[0]

	if len(parts) == 2 && parts[1] == "archive" {
		if status, ok := f.archiveStatus[id]; ok {
			code := "SKILL_DELISTED"
			if status != http.StatusForbidden {
				code = "SKILL_ERROR"
			}
			writeFakeKnowledgeError(w, status, code, "archive error")
			return
		}
		release, ok := f.releaseByID[id]
		if !ok || !releaseVisibleInTenant(release, tenant) {
			writeFakeKnowledgeError(w, http.StatusNotFound, "NOT_FOUND", "release not found")
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("X-SHA256", sha256Of(f.archiveBytes[id]))
		w.Header().Set("Content-Disposition", `attachment; filename="skill.zip"`)
		_, _ = w.Write(f.archiveBytes[id])
		return
	}

	release, ok := f.releaseByID[id]
	if !ok || !releaseVisibleInTenant(release, tenant) {
		writeFakeKnowledgeError(w, http.StatusNotFound, "NOT_FOUND", "release not found")
		return
	}
	if status, forced := f.metadataStatus[id]; forced {
		writeFakeKnowledgeError(w, status, "UPSTREAM_ERROR", "metadata error")
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "data": release})
}

func writeFakeKnowledgeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": false,
		"error":   map[string]string{"code": code, "message": message},
	})
}

// setupKnowledgeEnv 装配 sqlite 双库 + 桥 + 假 knowledge。
func setupKnowledgeEnv(t *testing.T, fake *fakeKnowledge) (*gin.Engine, *storage.Storage, *httptest.Server) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	mainDB := openIntegrationTestSQLite(t)
	stateDB := openIntegrationTestSQLite(t)
	if err := mainDB.AutoMigrate(&model.WeKnoraUser{}, &model.WeKnoraTenantMember{}, &model.WeKnoraAuthToken{}); err != nil {
		t.Fatalf("migrate main: %v", err)
	}
	if err := stateDB.AutoMigrate(&model.IdentityLink{}, &model.ManagedEntitlement{}, &model.Session{}, &model.AuditEvent{}, &model.SkillPublishJob{}); err != nil {
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
	sh := NewSkillsHandler(cfg, s, bridge)
	r.GET("/integration/v1/skills", sh.List)
	r.GET("/integration/v1/skills/:publish_id/download", sh.Download)
	adminSkills := r.Group("/integration/v1/admin", adminauth.RequireAdminAuth(adminauth.MiddlewareConfig{Secret: []byte(testAdminKey)}))
	adminSkills.POST("/skills/publish-jobs", sh.CreatePublishJob)
	adminSkills.GET("/skills/publish-jobs/:id", sh.GetPublishJob)
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
