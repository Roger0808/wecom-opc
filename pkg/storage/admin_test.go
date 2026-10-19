package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"weknora-tokendance/pkg/crypto"
	"weknora-tokendance/pkg/model"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupTestDB(t *testing.T) (*Storage, []byte) {
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

	aesKey := []byte("12345678901234567890123456789012") // 32 bytes
	return &Storage{db: db}, aesKey
}

func TestMaskSecret(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"empty string", "", ""},
		{"all spaces", "   ", ""},
		{"short secret 4 chars", "1234", "***"},
		{"short secret 8 chars", "12345678", "***"},
		{"short secret 10 chars", "1234567890", "***"},
		{"11 chars secret", "12345678901", "1234********8901"},
		{"typical api key", "sk-proj-1234567890abcdef", "sk-p********cdef"},
		{"whitespace trimmed", "  sk-secret-key-12345678  ", "sk-s********5678"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MaskSecret(tc.input)
			if got != tc.expected {
				t.Errorf("MaskSecret(%q) = %q, expected %q", tc.input, got, tc.expected)
			}
			if maskSecret(tc.input) != got {
				t.Errorf("maskSecret should match MaskSecret")
			}
		})
	}
}

func TestGetRootSpaceConfig(t *testing.T) {
	s, aesKey := setupTestDB(t)
	ctx := context.Background()
	const rootID uint64 = 10001

	// 1. 初始化母空间记录
	now := time.Now()
	err := s.db.Create(&model.WeKnoraTenant{
		ID:          rootID,
		Name:        "本来生活集团",
		Description: "基准配置母空间",
		CreatedAt:   now,
		UpdatedAt:   now,
	}).Error
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	// 2. 初始化加密模型
	rawAPIKey := "sk-glm-secret-key-12345678"
	encAPIKey, err := crypto.EncryptAESGCM(rawAPIKey, aesKey)
	if err != nil {
		t.Fatalf("encrypt key: %v", err)
	}

	paramsJSON, _ := json.Marshal(map[string]interface{}{
		"base_url": "https://api.example.com/v1",
		"provider": "generic",
		"api_key":  encAPIKey,
	})

	err = s.db.Create(&model.WeKnoraModel{
		ID:          "model-root-qa",
		TenantID:    rootID,
		Name:        "GLM-5.3-Flash",
		DisplayName: "通用问答模型",
		Type:        "KnowledgeQA",
		Source:      "remote",
		Parameters:  string(paramsJSON),
		IsDefault:   true,
		Status:      "active",
		CreatedAt:   now,
		UpdatedAt:   now,
	}).Error
	if err != nil {
		t.Fatalf("create model: %v", err)
	}

	// 3. 初始化 MinIO 存储
	rawSecret := "minio-secret-access-key-999"
	encSecret, _ := crypto.EncryptAESGCM(rawSecret, aesKey)
	storageCfgJSON, _ := json.Marshal(map[string]interface{}{
		"endpoint":          "minio:9000",
		"bucket_name":       "weknora",
		"path_prefix":       "weknora/",
		"access_key_id":     "minioadmin",
		"secret_access_key": encSecret,
	})

	err = s.db.Create(&model.WeKnoraStorageBackend{
		ID:        "storage-root-minio",
		TenantID:  rootID,
		Name:      "System MINIO",
		Provider:  "minio",
		Config:    string(storageCfgJSON),
		Source:    "env",
		Status:    "active",
		CreatedAt: now,
		UpdatedAt: now,
	}).Error
	if err != nil {
		t.Fatalf("create storage backend: %v", err)
	}

	// 4. 调用 GetRootSpaceConfig
	cfg, err := s.GetRootSpaceConfig(ctx, rootID, aesKey)
	if err != nil {
		t.Fatalf("GetRootSpaceConfig error: %v", err)
	}

	if cfg.TenantID != rootID {
		t.Errorf("expected tenant id %d, got %d", rootID, cfg.TenantID)
	}
	if cfg.Name != "本来生活集团" {
		t.Errorf("expected tenant name '本来生活集团', got %q", cfg.Name)
	}
	if len(cfg.Models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(cfg.Models))
	}

	m := cfg.Models[0]
	if m.ID != "model-root-qa" || m.Name != "GLM-5.3-Flash" {
		t.Errorf("unexpected model: %+v", m)
	}
	if !m.IsDefault {
		t.Errorf("expected model to be default")
	}
	expectedMaskedKey := MaskSecret(rawAPIKey)
	if m.APIKeyMasked != expectedMaskedKey {
		t.Errorf("expected masked api key %q, got %q", expectedMaskedKey, m.APIKeyMasked)
	}
	// 验证 parameters 里的敏感 key 也已被脱敏
	if m.Parameters["api_key"] != expectedMaskedKey {
		t.Errorf("expected parameters.api_key to be masked %q, got %v", expectedMaskedKey, m.Parameters["api_key"])
	}

	if cfg.Storage == nil {
		t.Fatalf("expected storage to be non-nil")
	}
	if cfg.Storage.Provider != "minio" || cfg.Storage.Endpoint != "minio:9000" {
		t.Errorf("unexpected storage: %+v", cfg.Storage)
	}
	expectedSecretMasked := MaskSecret(rawSecret)
	if cfg.Storage.SecretKeyMasked != expectedSecretMasked {
		t.Errorf("expected masked secret key %q, got %q", expectedSecretMasked, cfg.Storage.SecretKeyMasked)
	}
}

func TestUpdateRootModel(t *testing.T) {
	s, aesKey := setupTestDB(t)
	ctx := context.Background()
	const rootID uint64 = 10001

	// 1. 新增模型
	req1 := model.UpdateModelReq{
		Name:        "bge-m3",
		DisplayName: "BGE-M3 向量模型",
		Type:        "Embedding",
		Source:      "remote",
		Provider:    "generic",
		BaseURL:     "https://embedding.example.com",
		APIKey:      "sk-new-embedding-key-12345",
		IsDefault:   true,
	}

	err := s.UpdateRootModel(ctx, rootID, req1, aesKey)
	if err != nil {
		t.Fatalf("UpdateRootModel create error: %v", err)
	}

	var m1 model.WeKnoraModel
	err = s.db.Where("tenant_id = ? AND name = ?", rootID, "bge-m3").First(&m1).Error
	if err != nil {
		t.Fatalf("find created model: %v", err)
	}
	if !m1.IsDefault {
		t.Errorf("expected is_default=true")
	}

	var p1 map[string]interface{}
	_ = json.Unmarshal([]byte(m1.Parameters), &p1)
	apiKeyStored, _ := p1["api_key"].(string)
	if !strings.HasPrefix(apiKeyStored, crypto.EncPrefix) {
		t.Fatalf("expected api_key to be encrypted with %s prefix, got %s", crypto.EncPrefix, apiKeyStored)
	}
	decryptedKey, err := crypto.DecryptAESGCM(apiKeyStored, aesKey)
	if err != nil || decryptedKey != req1.APIKey {
		t.Errorf("expected decrypted key %q, got %q (err: %v)", req1.APIKey, decryptedKey, err)
	}

	// 2. 新增第二个同类型模型并设为默认，原模型的 is_default 应自动置为 false
	req2 := model.UpdateModelReq{
		Name:        "bge-large-zh",
		DisplayName: "BGE-Large 向量模型",
		Type:        "Embedding",
		BaseURL:     "https://embedding.example.com/v2",
		APIKey:      "sk-bge-large-key-99999",
		IsDefault:   true,
	}
	err = s.UpdateRootModel(ctx, rootID, req2, aesKey)
	if err != nil {
		t.Fatalf("UpdateRootModel create second model error: %v", err)
	}

	// 验证原 m1 的 is_default 变为了 false
	var m1After model.WeKnoraModel
	_ = s.db.Where("id = ?", m1.ID).First(&m1After)
	if m1After.IsDefault {
		t.Errorf("expected m1 is_default to become false after m2 set as default")
	}

	// 3. 更新现有模型但不传新 key (保留掩码或空)，原有 key 应保持不变
	req1Update := model.UpdateModelReq{
		ID:          m1.ID,
		Name:        "bge-m3",
		DisplayName: "BGE-M3 (已更新)",
		Type:        "Embedding",
		BaseURL:     "https://embedding-new.example.com",
		APIKey:      MaskSecret(req1.APIKey), // 传掩码表示不更改
		IsDefault:   false,
	}
	err = s.UpdateRootModel(ctx, rootID, req1Update, aesKey)
	if err != nil {
		t.Fatalf("UpdateRootModel update error: %v", err)
	}

	var m1Updated model.WeKnoraModel
	_ = s.db.Where("id = ?", m1.ID).First(&m1Updated)
	if m1Updated.DisplayName != "BGE-M3 (已更新)" {
		t.Errorf("expected display_name updated, got %q", m1Updated.DisplayName)
	}
	var p1Updated map[string]interface{}
	_ = json.Unmarshal([]byte(m1Updated.Parameters), &p1Updated)
	if p1Updated["api_key"] != apiKeyStored {
		t.Errorf("api_key should remain unchanged when masked key passed, got %v", p1Updated["api_key"])
	}

	// 4. 删除模型 (软删除)
	err = s.DeleteRootModel(ctx, rootID, m1.ID)
	if err != nil {
		t.Fatalf("DeleteRootModel error: %v", err)
	}

	var m1Deleted model.WeKnoraModel
	err = s.db.Where("tenant_id = ? AND id = ?", rootID, m1.ID).First(&m1Deleted).Error
	if err != gorm.ErrRecordNotFound {
		t.Errorf("expected record not found due to soft delete, got err: %v", err)
	}
}

func TestUpdateRootStorage(t *testing.T) {
	s, aesKey := setupTestDB(t)
	ctx := context.Background()
	const rootID uint64 = 10001

	// 1. 初始化更新母空间存储
	req := model.UpdateStorageReq{
		Endpoint:        "minio.internal:9000",
		BucketName:      "weknora-base",
		PathPrefix:      "base/",
		AccessKeyID:     "ak-admin-123",
		SecretAccessKey: "sk-minio-secret-value-8888",
	}

	err := s.UpdateRootStorage(ctx, rootID, req, aesKey)
	if err != nil {
		t.Fatalf("UpdateRootStorage error: %v", err)
	}

	var sb model.WeKnoraStorageBackend
	err = s.db.Where("tenant_id = ? AND provider = 'minio'", rootID).First(&sb).Error
	if err != nil {
		t.Fatalf("find storage backend: %v", err)
	}

	var cfg map[string]interface{}
	_ = json.Unmarshal([]byte(sb.Config), &cfg)
	if cfg["endpoint"] != "minio.internal:9000" || cfg["bucket_name"] != "weknora-base" {
		t.Errorf("unexpected storage config: %+v", cfg)
	}
	encSecret, _ := cfg["secret_access_key"].(string)
	if !strings.HasPrefix(encSecret, crypto.EncPrefix) {
		t.Fatalf("expected secret_access_key to be encrypted, got %s", encSecret)
	}
	decSecret, err := crypto.DecryptAESGCM(encSecret, aesKey)
	if err != nil || decSecret != req.SecretAccessKey {
		t.Errorf("expected decrypted secret %q, got %q", req.SecretAccessKey, decSecret)
	}

	// 2. 传入掩码保留原有 secret_access_key
	req2 := model.UpdateStorageReq{
		Endpoint:        "minio.internal:9001",
		BucketName:      "weknora-base",
		PathPrefix:      "base/",
		AccessKeyID:     "ak-admin-123",
		SecretAccessKey: MaskSecret(req.SecretAccessKey),
	}
	err = s.UpdateRootStorage(ctx, rootID, req2, aesKey)
	if err != nil {
		t.Fatalf("UpdateRootStorage second update error: %v", err)
	}

	var sb2 model.WeKnoraStorageBackend
	_ = s.db.Where("id = ?", sb.ID).First(&sb2)
	var cfg2 map[string]interface{}
	_ = json.Unmarshal([]byte(sb2.Config), &cfg2)
	if cfg2["endpoint"] != "minio.internal:9001" {
		t.Errorf("endpoint not updated")
	}
	if cfg2["secret_access_key"] != encSecret {
		t.Errorf("secret_access_key should be preserved when masked key passed")
	}
}

func TestBroadcastRootConfigToAllTenants(t *testing.T) {
	s, aesKey := setupTestDB(t)
	ctx := context.Background()
	const rootID uint64 = 10001
	const deptTenant1 uint64 = 20001
	const deptTenant2 uint64 = 20002

	// 1. 初始化母空间模型与存储
	err := s.UpdateRootModel(ctx, rootID, model.UpdateModelReq{
		Name:        "glm-flash",
		DisplayName: "GLM Flash",
		Type:        "KnowledgeQA",
		BaseURL:     "https://glm.example.com",
		APIKey:      "sk-root-model-secret-key",
		IsDefault:   true,
	}, aesKey)
	if err != nil {
		t.Fatalf("setup root model: %v", err)
	}

	err = s.UpdateRootStorage(ctx, rootID, model.UpdateStorageReq{
		Endpoint:        "minio:9000",
		BucketName:      "weknora-root",
		PathPrefix:      "weknora/",
		AccessKeyID:     "rootak",
		SecretAccessKey: "root-secret-access-key-999",
	}, aesKey)
	if err != nil {
		t.Fatalf("setup root storage: %v", err)
	}

	// 2. 创建部门空间映射
	now := time.Now()
	_ = s.db.Create(&model.WeComDeptTenantMapping{
		WeComDeptID:    101,
		DepartmentCode: "D101",
		TenantID:       deptTenant1,
		DeptName:       "技术开发部",
		CreatedAt:      now,
		UpdatedAt:      now,
	}).Error

	_ = s.db.Create(&model.WeComDeptTenantMapping{
		WeComDeptID:    102,
		DepartmentCode: "D102",
		TenantID:       deptTenant2,
		DeptName:       "市场营销部",
		CreatedAt:      now,
		UpdatedAt:      now,
	}).Error

	// 3. 执行广播
	count, err := s.BroadcastRootConfigToAllTenants(ctx, rootID)
	if err != nil {
		t.Fatalf("BroadcastRootConfigToAllTenants error: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 tenants broadcasted, got %d", count)
	}

	// 4. 验证子空间 20001 和 20002 是否同步了母空间模型和 MinIO 存储
	for _, tid := range []uint64{deptTenant1, deptTenant2} {
		var models []model.WeKnoraModel
		err := s.db.Where("tenant_id = ? AND deleted_at IS NULL", tid).Find(&models).Error
		if err != nil || len(models) != 1 {
			t.Errorf("tenant %d: expected 1 synced model, got %d (err: %v)", tid, len(models), err)
		} else {
			if models[0].Name != "glm-flash" || models[0].Type != "KnowledgeQA" {
				t.Errorf("tenant %d: unexpected model: %+v", tid, models[0])
			}
		}

		var sb model.WeKnoraStorageBackend
		err = s.db.Where("tenant_id = ? AND provider = 'minio' AND deleted_at IS NULL", tid).First(&sb).Error
		if err != nil {
			t.Errorf("tenant %d: minio storage backend not found: %v", tid, err)
		} else {
			var cfg map[string]interface{}
			_ = json.Unmarshal([]byte(sb.Config), &cfg)
			if cfg["bucket_name"] != "weknora-root" || cfg["endpoint"] != "minio:9000" {
				t.Errorf("tenant %d: unexpected minio config: %+v", tid, cfg)
			}
		}
	}
}

func TestUpdateRootModelsBatch(t *testing.T) {
	s, aesKey := setupTestDB(t)
	ctx := context.Background()
	const rootID uint64 = 10001

	// 1. 批量新增 2 个模型
	reqs := []model.UpdateModelReq{
		{
			ID:        "m1",
			Name:      "Model-1",
			Type:      "KnowledgeQA",
			APIKey:    "test-key-12345",
			IsDefault: true,
		},
		{
			ID:        "m2",
			Name:      "Model-2",
			Type:      "Embedding",
			APIKey:    "test-key-67890",
			IsDefault: true,
		},
	}

	err := s.UpdateRootModels(ctx, rootID, reqs, aesKey)
	if err != nil {
		t.Fatalf("batch update failed: %v", err)
	}

	var count int64
	s.db.Model(&model.WeKnoraModel{}).Where("tenant_id = ? AND deleted_at IS NULL", rootID).Count(&count)
	if count != 2 {
		t.Fatalf("expected 2 models, got %d", count)
	}

	// 2. 批量操作：删除 m1，更新 m2，新增 m3
	batchReqs := []model.UpdateModelReq{
		{
			ID:      "m1",
			Name:    "Model-1",
			Type:    "KnowledgeQA",
			Deleted: true,
		},
		{
			ID:        "m2",
			Name:      "Model-2-Renamed",
			Type:      "Embedding",
			APIKey:    "sk-p********cdef", // 保留原密钥
			IsDefault: true,
		},
		{
			ID:        "m3",
			Name:      "Model-3",
			Type:      "Rerank",
			APIKey:    "test-key-rerank",
			IsDefault: true,
		},
	}

	err = s.UpdateRootModels(ctx, rootID, batchReqs, aesKey)
	if err != nil {
		t.Fatalf("second batch update failed: %v", err)
	}

	s.db.Model(&model.WeKnoraModel{}).Where("tenant_id = ? AND deleted_at IS NULL", rootID).Count(&count)
	if count != 2 {
		t.Fatalf("expected 2 active models after deletion and addition, got %d", count)
	}

	var m2 model.WeKnoraModel
	s.db.Where("tenant_id = ? AND id = ?", rootID, "m2").First(&m2)
	if m2.Name != "Model-2-Renamed" {
		t.Errorf("expected m2 to be renamed, got %s", m2.Name)
	}
}

