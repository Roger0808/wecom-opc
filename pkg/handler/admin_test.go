package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/crypto"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupTestHandler(t *testing.T, rootTenantID uint64) (*Handler, *gin.Engine, *gorm.DB, []byte) {
	gin.SetMode(gin.TestMode)

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", uuid.New().String())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("failed to open sqlite in-memory db: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to get sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)

	err = db.AutoMigrate(
		&model.WeKnoraTenant{},
		&model.WeKnoraModel{},
		&model.WeKnoraStorageBackend{},
		&model.TenantSandboxConfigEntity{},
		&model.WeComDeptTenantMapping{},
		&model.WeKnoraTenantMember{},
		&model.WeKnoraUser{},
	)
	if err != nil {
		t.Fatalf("failed to auto migrate: %v", err)
	}

	aesKey := "12345678901234567890123456789012"
	cfg := &config.Config{
		WeKnora: config.WeKnoraConfig{
			RootTenantID: rootTenantID,
		},
		SystemAESKey: aesKey,
	}

	s := storage.NewStorageWithDB(db)
	h := NewHandler(cfg, nil, nil, nil, nil, s)

	r := gin.New()
	adminAPI := r.Group("/admin/api")
	{
		adminAPI.GET("/root-config", h.AdminGetRootConfig)
		adminAPI.PUT("/root-config/models", h.AdminUpdateRootModels)
		adminAPI.PUT("/root-config/storage", h.AdminUpdateRootStorage)
		adminAPI.GET("/root-config/sandbox", h.AdminGetRootSandbox)
		adminAPI.PUT("/root-config/sandbox", h.AdminUpdateRootSandbox)
		adminAPI.POST("/root-config/broadcast", h.AdminBroadcastRootConfig)
	}

	return h, r, db, []byte(aesKey)
}

func TestAdminGetRootConfig(t *testing.T) {
	const rootID uint64 = 10001
	_, r, db, aesKey := setupTestHandler(t, rootID)

	// 准备母空间与模型、存储测试数据
	now := time.Now()
	if err := db.Create(&model.WeKnoraTenant{
		ID:          rootID,
		Name:        "本来生活母空间",
		Description: "基准配置母空间",
		CreatedAt:   now,
		UpdatedAt:   now,
	}).Error; err != nil {
		t.Fatalf("create root tenant: %v", err)
	}

	encAPIKey, _ := crypto.EncryptAESGCM("sk-root-model-secret-key-1234", aesKey)
	paramsJSON, _ := json.Marshal(map[string]interface{}{
		"provider": "openai",
		"base_url": "https://api.openai.com/v1",
		"api_key":  encAPIKey,
	})
	if err := db.Create(&model.WeKnoraModel{
		ID:          uuid.New().String(),
		TenantID:    rootID,
		Name:        "gpt-4o",
		DisplayName: "GPT-4o",
		Type:        "chat",
		Source:      "remote",
		Parameters:  string(paramsJSON),
		IsDefault:   true,
		Status:      "active",
		CreatedAt:   now,
		UpdatedAt:   now,
	}).Error; err != nil {
		t.Fatalf("create root model: %v", err)
	}

	encSecret, _ := crypto.EncryptAESGCM("minio-secret-key-9999", aesKey)
	storageCfgJSON, _ := json.Marshal(map[string]interface{}{
		"endpoint":          "http://10.0.0.1:9000",
		"bucket_name":       "weknora-bucket",
		"path_prefix":       "uploads",
		"access_key_id":     "minioadmin",
		"secret_access_key": encSecret,
	})
	if err := db.Create(&model.WeKnoraStorageBackend{
		ID:        uuid.New().String(),
		TenantID:  rootID,
		Name:      "System MINIO",
		Provider:  "minio",
		Config:    string(storageCfgJSON),
		Status:    "active",
		CreatedAt: now,
		UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("create root storage: %v", err)
	}

	// 执行 GET 请求
	req, _ := http.NewRequest(http.MethodGet, "/admin/api/root-config", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d, body: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Code int                 `json:"code"`
		Data model.RootConfigDTO `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Code != 0 {
		t.Errorf("expected code 0, got %d", resp.Code)
	}
	if resp.Data.TenantID != rootID {
		t.Errorf("expected tenant_id %d, got %d", rootID, resp.Data.TenantID)
	}
	if resp.Data.Name != "本来生活母空间" {
		t.Errorf("expected name '本来生活母空间', got %s", resp.Data.Name)
	}
	if len(resp.Data.Models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(resp.Data.Models))
	}
	if resp.Data.Models[0].Name != "gpt-4o" {
		t.Errorf("expected model name 'gpt-4o', got %s", resp.Data.Models[0].Name)
	}
	if resp.Data.Models[0].APIKeyMasked == "" || resp.Data.Models[0].APIKeyMasked == "sk-root-model-secret-key-1234" {
		t.Errorf("expected masked api key, got %s", resp.Data.Models[0].APIKeyMasked)
	}
	if resp.Data.Storage == nil {
		t.Fatalf("expected storage to be non-nil")
	}
	if resp.Data.Storage.BucketName != "weknora-bucket" {
		t.Errorf("expected bucket_name 'weknora-bucket', got %s", resp.Data.Storage.BucketName)
	}
	if resp.Data.Storage.SecretKeyMasked == "" || resp.Data.Storage.SecretKeyMasked == "minio-secret-key-9999" {
		t.Errorf("expected masked secret key, got %s", resp.Data.Storage.SecretKeyMasked)
	}
}

func TestAdminGetRootConfig_DefaultRootTenantID(t *testing.T) {
	// RootTenantID 设置为 0，应默认回退为 10001
	h, _, db, _ := setupTestHandler(t, 0)
	if h.getRootTenantID() != 10001 {
		t.Fatalf("expected fallback rootTenantID 10001, got %d", h.getRootTenantID())
	}

	now := time.Now()
	_ = db.Create(&model.WeKnoraTenant{
		ID:        10001,
		Name:      "默认回退母空间",
		CreatedAt: now,
		UpdatedAt: now,
	})

	// 使用 gin.CreateTestContext 测试
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/admin/api/root-config", nil)

	h.AdminGetRootConfig(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp struct {
		Code int                 `json:"code"`
		Data model.RootConfigDTO `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Data.TenantID != 10001 {
		t.Errorf("expected tenant_id 10001, got %d", resp.Data.TenantID)
	}
}

func TestAdminUpdateRootModels(t *testing.T) {
	const rootID uint64 = 10001
	h, r, db, _ := setupTestHandler(t, rootID)

	now := time.Now()
	_ = db.Create(&model.WeKnoraTenant{
		ID:        rootID,
		Name:      "母空间",
		CreatedAt: now,
		UpdatedAt: now,
	})

	t.Run("400 Bad Request on missing models", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPut, "/admin/api/root-config/models", bytes.NewBufferString(`{}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected status 400, got %d, body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("400 Bad Request on invalid JSON", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPut, "/admin/api/root-config/models", bytes.NewBufferString(`invalid-json`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected status 400, got %d", w.Code)
		}
	})

	t.Run("200 OK on successful create and update", func(t *testing.T) {
		body := map[string]interface{}{
			"models": []map[string]interface{}{
				{
					"name":         "deepseek-chat",
					"display_name": "DeepSeek Chat",
					"type":         "chat",
					"provider":     "openai",
					"base_url":     "https://api.deepseek.com",
					"api_key":      "sk-deepseek-test-12345678",
					"is_default":   true,
					"status":       "active",
				},
			},
		}
		bodyBytes, _ := json.Marshal(body)

		req, _ := http.NewRequest(http.MethodPut, "/admin/api/root-config/models", bytes.NewBuffer(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d, body: %s", w.Code, w.Body.String())
		}

		var resp struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.Code != 0 {
			t.Errorf("expected code 0, got %d", resp.Code)
		}
		if resp.Message != "母空间模型配置保存成功" {
			t.Errorf("expected message '母空间模型配置保存成功', got %s", resp.Message)
		}

		// 检查数据库记录
		var saved model.WeKnoraModel
		err := db.Where("tenant_id = ? AND name = ?", rootID, "deepseek-chat").First(&saved).Error
		if err != nil {
			t.Fatalf("find saved model: %v", err)
		}
		if !saved.IsDefault {
			t.Errorf("expected saved model to be default")
		}
	})

	t.Run("500 Internal Server Error when model validation fails", func(t *testing.T) {
		// 模型缺少必填的 type
		body := map[string]interface{}{
			"models": []map[string]interface{}{
				{
					"name": "invalid-no-type",
				},
			},
		}
		bodyBytes, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request, _ = http.NewRequest(http.MethodPut, "/admin/api/root-config/models", bytes.NewBuffer(bodyBytes))
		c.Request.Header.Set("Content-Type", "application/json")

		h.AdminUpdateRootModels(c)

		if w.Code != http.StatusInternalServerError {
			t.Fatalf("expected status 500, got %d, body: %s", w.Code, w.Body.String())
		}
	})
}

func TestAdminUpdateRootStorage(t *testing.T) {
	const rootID uint64 = 10001
	h, r, db, _ := setupTestHandler(t, rootID)

	now := time.Now()
	_ = db.Create(&model.WeKnoraTenant{
		ID:        rootID,
		Name:      "母空间",
		CreatedAt: now,
		UpdatedAt: now,
	})

	t.Run("400 Bad Request on invalid JSON", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPut, "/admin/api/root-config/storage", bytes.NewBufferString(`{invalid json`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected status 400, got %d", w.Code)
		}
	})

	t.Run("200 OK on update root storage", func(t *testing.T) {
		body := model.UpdateStorageReq{
			Endpoint:        "http://minio.corp.internal:9000",
			BucketName:      "weknora-system",
			PathPrefix:      "tenant-data",
			AccessKeyID:     "admin_access_key",
			SecretAccessKey: "admin_secret_key_12345",
		}
		bodyBytes, _ := json.Marshal(body)

		req, _ := http.NewRequest(http.MethodPut, "/admin/api/root-config/storage", bytes.NewBuffer(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d, body: %s", w.Code, w.Body.String())
		}

		var resp struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.Code != 0 {
			t.Errorf("expected code 0, got %d", resp.Code)
		}
		if resp.Message != "母空间 MinIO 存储配置保存成功" {
			t.Errorf("expected message '母空间 MinIO 存储配置保存成功', got %s", resp.Message)
		}

		// 检查数据库记录
		var sb model.WeKnoraStorageBackend
		err := db.Where("tenant_id = ? AND provider = 'minio'", rootID).First(&sb).Error
		if err != nil {
			t.Fatalf("find storage backend: %v", err)
		}
	})

	t.Run("using CreateTestContext", func(t *testing.T) {
		body := model.UpdateStorageReq{
			Endpoint:   "http://minio2:9000",
			BucketName: "weknora-bucket2",
		}
		bodyBytes, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request, _ = http.NewRequest(http.MethodPut, "/admin/api/root-config/storage", bytes.NewBuffer(bodyBytes))
		c.Request.Header.Set("Content-Type", "application/json")

		h.AdminUpdateRootStorage(c)

		if w.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", w.Code)
		}
	})
}

func TestAdminBroadcastRootConfig(t *testing.T) {
	const rootID uint64 = 10001
	h, r, db, aesKey := setupTestHandler(t, rootID)

	now := time.Now()
	// 1. 创建母空间及配置
	_ = db.Create(&model.WeKnoraTenant{ID: rootID, Name: "母空间", CreatedAt: now, UpdatedAt: now})
	encAPIKey, _ := crypto.EncryptAESGCM("sk-test-key-12345678", aesKey)
	paramsJSON, _ := json.Marshal(map[string]interface{}{"api_key": encAPIKey, "provider": "openai"})
	_ = db.Create(&model.WeKnoraModel{
		ID:         uuid.New().String(),
		TenantID:   rootID,
		Name:       "qwen-plus",
		Type:       "chat",
		Parameters: string(paramsJSON),
		CreatedAt:  now,
		UpdatedAt:  now,
	})
	encSecret, _ := crypto.EncryptAESGCM("minio-secret-9999", aesKey)
	storageJSON, _ := json.Marshal(map[string]interface{}{"secret_access_key": encSecret, "bucket_name": "corp-files"})
	_ = db.Create(&model.WeKnoraStorageBackend{
		ID:        uuid.New().String(),
		TenantID:  rootID,
		Name:      "System MINIO",
		Provider:  "minio",
		Config:    string(storageJSON),
		CreatedAt: now,
		UpdatedAt: now,
	})

	// 2. 创建 2 个子空间并建立部门映射
	child1 := uint64(10002)
	child2 := uint64(10003)
	_ = db.Create(&model.WeKnoraTenant{ID: child1, Name: "技术研发部", CreatedAt: now, UpdatedAt: now})
	_ = db.Create(&model.WeKnoraTenant{ID: child2, Name: "市场运营部", CreatedAt: now, UpdatedAt: now})
	_ = db.Create(&model.WeComDeptTenantMapping{WeComDeptID: 2, TenantID: child1})
	_ = db.Create(&model.WeComDeptTenantMapping{WeComDeptID: 3, TenantID: child2})

	t.Run("200 OK broadcast via Router", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "/admin/api/root-config/broadcast", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d, body: %s", w.Code, w.Body.String())
		}

		var resp struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Count   int    `json:"count"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)

		if resp.Code != 0 {
			t.Errorf("expected code 0, got %d", resp.Code)
		}
		if resp.Count != 2 {
			t.Errorf("expected count 2, got %d", resp.Count)
		}
		expectedMsg := "已成功将母空间最新配置级联下发至 2 个子空间"
		if resp.Message != expectedMsg {
			t.Errorf("expected message %q, got %q", expectedMsg, resp.Message)
		}

		// 验证子空间获得了模型与存储
		var m1 model.WeKnoraModel
		if err := db.Where("tenant_id = ? AND name = ?", child1, "qwen-plus").First(&m1).Error; err != nil {
			t.Errorf("child1 model missing: %v", err)
		}
		var sb1 model.WeKnoraStorageBackend
		if err := db.Where("tenant_id = ? AND provider = 'minio'", child1).First(&sb1).Error; err != nil {
			t.Errorf("child1 storage missing: %v", err)
		}
	})

	t.Run("using CreateTestContext", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request, _ = http.NewRequest(http.MethodPost, "/admin/api/root-config/broadcast", nil)

		h.AdminBroadcastRootConfig(c)

		if w.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d, body: %s", w.Code, w.Body.String())
		}
	})
}

func TestAdminHandlers_StorageErrors(t *testing.T) {
	_, r, db, _ := setupTestHandler(t, 10001)

	// 关闭底层数据库连接以模拟存储层 500 异常
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get sql.DB: %v", err)
	}
	_ = sqlDB.Close()

	t.Run("GetRootConfig returns 500 on storage error", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/admin/api/root-config", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", w.Code)
		}
	})

	t.Run("UpdateRootModels returns 500 on storage error", func(t *testing.T) {
		body := map[string]interface{}{
			"models": []map[string]interface{}{
				{"name": "test", "type": "chat"},
			},
		}
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPut, "/admin/api/root-config/models", bytes.NewBuffer(b))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", w.Code)
		}
	})

	t.Run("UpdateRootStorage returns 500 on storage error", func(t *testing.T) {
		body := model.UpdateStorageReq{
			Endpoint: "http://minio:9000",
		}
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPut, "/admin/api/root-config/storage", bytes.NewBuffer(b))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", w.Code)
		}
	})

	t.Run("BroadcastRootConfig returns 500 on storage error", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "/admin/api/root-config/broadcast", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", w.Code)
		}
	})
}

