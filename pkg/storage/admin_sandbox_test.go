package storage

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"weknora-tokendance/pkg/model"
)

func TestSandbox_GetRootConfig_NotFound(t *testing.T) {
	s, _ := setupTestDB(t)
	ctx := context.Background()

	cfg, err := s.GetRootSandboxConfig(ctx, 10001)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != nil {
		t.Fatalf("expected nil when no sandbox config exists, got %+v", cfg)
	}
}

func TestSandbox_UpdateAndGetRootConfig(t *testing.T) {
	s, _ := setupTestDB(t)
	ctx := context.Background()
	const rootID uint64 = 10001

	// 1. 验证校验失败
	invalidReq := model.UpdateSandboxReq{
		Image:             "",
		DefaultTimeoutSec: -5,
	}
	err := s.UpdateRootSandboxConfig(ctx, rootID, invalidReq)
	if err == nil {
		t.Fatalf("expected validation error for invalid req")
	}

	// 2. 初始创建
	req := model.UpdateSandboxReq{
		Name:              "默认Docker沙箱",
		Description:       "母空间测试沙箱",
		SandboxType:       "docker",
		Image:             "wechatopenai/weknora-sandbox:v1.0",
		CPULimit:          2.0,
		MemoryLimitMB:     2048,
		DefaultTimeoutSec: 120,
	}

	err = s.UpdateRootSandboxConfig(ctx, rootID, req)
	if err != nil {
		t.Fatalf("UpdateRootSandboxConfig create error: %v", err)
	}

	// 3. 读取验证
	cfg, err := s.GetRootSandboxConfig(ctx, rootID)
	if err != nil {
		t.Fatalf("GetRootSandboxConfig error: %v", err)
	}
	if cfg == nil {
		t.Fatalf("expected sandbox config to be non-nil")
	}

	if cfg.Image != req.Image {
		t.Errorf("expected image %q, got %q", req.Image, cfg.Image)
	}
	if cfg.CPULimit != req.CPULimit {
		t.Errorf("expected cpu_limit %f, got %f", req.CPULimit, cfg.CPULimit)
	}
	if cfg.MemoryLimitMB != req.MemoryLimitMB {
		t.Errorf("expected memory_limit_mb %d, got %d", req.MemoryLimitMB, cfg.MemoryLimitMB)
	}
	if cfg.DefaultTimeoutSec != req.DefaultTimeoutSec {
		t.Errorf("expected default_timeout_sec %d, got %d", req.DefaultTimeoutSec, cfg.DefaultTimeoutSec)
	}
	if cfg.SandboxType != req.SandboxType {
		t.Errorf("expected sandbox_type %q, got %q", req.SandboxType, cfg.SandboxType)
	}

	// 4. 更新配置并验证扩展字段保留
	// 先向数据库中注入一个带有扩展字段（如 skill_image）的记录
	var existing model.TenantSandboxConfigEntity
	if err := s.db.Where("tenant_id = ?", rootID).First(&existing).Error; err != nil {
		t.Fatalf("find existing sandbox entity: %v", err)
	}

	var rawMap map[string]interface{}
	_ = json.Unmarshal([]byte(existing.Config), &rawMap)
	rawMap["skill_image"] = "python:3.11-slim"
	updatedConfigBytes, _ := json.Marshal(rawMap)
	s.db.Model(&existing).Update("config", string(updatedConfigBytes))

	// 更新沙箱配置
	updateReq := model.UpdateSandboxReq{
		Name:              "默认Docker沙箱",
		Description:       "更新后的沙箱",
		SandboxType:       "docker",
		Image:             "wechatopenai/weknora-sandbox:v2.0",
		CPULimit:          4.0,
		MemoryLimitMB:     4096,
		DefaultTimeoutSec: 300,
	}
	err = s.UpdateRootSandboxConfig(ctx, rootID, updateReq)
	if err != nil {
		t.Fatalf("UpdateRootSandboxConfig update error: %v", err)
	}

	// 重新读取并验证
	cfgUpdated, err := s.GetRootSandboxConfig(ctx, rootID)
	if err != nil {
		t.Fatalf("GetRootSandboxConfig error: %v", err)
	}
	if cfgUpdated.Image != "wechatopenai/weknora-sandbox:v2.0" {
		t.Errorf("expected updated image, got %q", cfgUpdated.Image)
	}
	if cfgUpdated.CPULimit != 4.0 || cfgUpdated.MemoryLimitMB != 4096 || cfgUpdated.DefaultTimeoutSec != 300 {
		t.Errorf("expected updated limits, got cpu=%f mem=%d timeout=%d",
			cfgUpdated.CPULimit, cfgUpdated.MemoryLimitMB, cfgUpdated.DefaultTimeoutSec)
	}

	// 验证底层 JSONB 中的 skill_image 未丢失
	var reloaded model.TenantSandboxConfigEntity
	_ = s.db.Where("id = ?", existing.ID).First(&reloaded)
	var reloadedMap map[string]interface{}
	_ = json.Unmarshal([]byte(reloaded.Config), &reloadedMap)
	if reloadedMap["skill_image"] != "python:3.11-slim" {
		t.Errorf("expected skill_image to be preserved, got %v", reloadedMap["skill_image"])
	}
}

func TestSandbox_SyncSpace(t *testing.T) {
	s, _ := setupTestDB(t)
	ctx := context.Background()
	const rootID uint64 = 10001
	const subTenantNew uint64 = 20001
	const subTenantExisting uint64 = 20002

	// 1. 母空间无配置时，应直接返回 nil
	err := s.SyncSpaceSandbox(ctx, subTenantNew, rootID)
	if err != nil {
		t.Fatalf("expected nil when root has no config, got: %v", err)
	}

	// 2. 初始化母空间配置
	rootReq := model.UpdateSandboxReq{
		Name:              "默认Docker沙箱",
		Description:       "基准沙箱配置",
		SandboxType:       "docker",
		Image:             "wechatopenai/weknora-sandbox:v3.0",
		CPULimit:          2.0,
		MemoryLimitMB:     2048,
		DefaultTimeoutSec: 120,
	}
	if err := s.UpdateRootSandboxConfig(ctx, rootID, rootReq); err != nil {
		t.Fatalf("setup root sandbox: %v", err)
	}

	// 3. 情况一：子空间尚无记录，同步应创建新记录
	err = s.SyncSpaceSandbox(ctx, subTenantNew, rootID)
	if err != nil {
		t.Fatalf("SyncSpaceSandbox to new tenant failed: %v", err)
	}

	var newEntity model.TenantSandboxConfigEntity
	err = s.db.Where("tenant_id = ? AND deleted_at IS NULL", subTenantNew).First(&newEntity).Error
	if err != nil {
		t.Fatalf("find new tenant sandbox config: %v", err)
	}
	if newEntity.ID == "" {
		t.Errorf("expected generated UUID, got empty")
	}
	if newEntity.TenantID != subTenantNew {
		t.Errorf("expected tenant_id %d, got %d", subTenantNew, newEntity.TenantID)
	}

	// 验证配置内容
	var newCfgMap map[string]interface{}
	_ = json.Unmarshal([]byte(newEntity.Config), &newCfgMap)
	dockerMap, _ := newCfgMap["docker"].(map[string]interface{})
	if dockerMap["image"] != "wechatopenai/weknora-sandbox:v3.0" {
		t.Errorf("expected image to match root, got %v", dockerMap["image"])
	}

	// 4. 情况二：子空间已存在沙箱记录（如已有智能体绑定），必须绝对保留原有 ID
	const fixedExistingID = "agent-bound-sandbox-uuid-9999"
	existingTargetCfg := map[string]interface{}{
		"docker": map[string]interface{}{
			"image":           "old-sandbox:0.1",
			"cpu_limit":       1.0,
			"memory_limit_mb": 1024,
		},
		"custom_agent_field": "agent_data_value",
	}
	existingTargetCfgBytes, _ := json.Marshal(existingTargetCfg)

	now := time.Now()
	err = s.db.Create(&model.TenantSandboxConfigEntity{
		ID:          fixedExistingID,
		TenantID:    subTenantExisting,
		Name:        "旧版自定义沙箱",
		Description: "已被某智能体引用",
		SandboxType: "docker",
		Config:      string(existingTargetCfgBytes),
		CreatedAt:   now,
		UpdatedAt:   now,
	}).Error
	if err != nil {
		t.Fatalf("create existing subtenant sandbox: %v", err)
	}

	// 执行同步
	err = s.SyncSpaceSandbox(ctx, subTenantExisting, rootID)
	if err != nil {
		t.Fatalf("SyncSpaceSandbox to existing tenant failed: %v", err)
	}

	// 验证记录的主键 ID 严格保持不变
	var reloadedEntity model.TenantSandboxConfigEntity
	err = s.db.Where("tenant_id = ? AND deleted_at IS NULL", subTenantExisting).First(&reloadedEntity).Error
	if err != nil {
		t.Fatalf("query reloaded entity: %v", err)
	}
	if reloadedEntity.ID != fixedExistingID {
		t.Fatalf("CRITICAL: Existing sandbox ID changed! Expected %q, got %q", fixedExistingID, reloadedEntity.ID)
	}

	// 验证配置更新为母空间基准
	var reloadedCfg map[string]interface{}
	_ = json.Unmarshal([]byte(reloadedEntity.Config), &reloadedCfg)
	reloadedDocker, _ := reloadedCfg["docker"].(map[string]interface{})
	if reloadedDocker["image"] != "wechatopenai/weknora-sandbox:v3.0" {
		t.Errorf("expected updated image %q, got %v", "wechatopenai/weknora-sandbox:v3.0", reloadedDocker["image"])
	}
	if reloadedDocker["cpu_limit"] != 2.0 || reloadedDocker["memory_limit_mb"] != float64(2048) {
		t.Errorf("expected updated limits, got %+v", reloadedDocker)
	}
	// 验证原有自定义配置字段被保留
	if reloadedCfg["custom_agent_field"] != "agent_data_value" {
		t.Errorf("expected custom_agent_field to be preserved, got %v", reloadedCfg["custom_agent_field"])
	}
}

func TestSandbox_GetRootSpaceConfig_Integration(t *testing.T) {
	s, aesKey := setupTestDB(t)
	ctx := context.Background()
	const rootID uint64 = 10001

	// 创建母空间
	now := time.Now()
	_ = s.db.Create(&model.WeKnoraTenant{
		ID:        rootID,
		Name:      "本来生活集团",
		CreatedAt: now,
		UpdatedAt: now,
	}).Error

	// 初始化沙箱配置
	rootReq := model.UpdateSandboxReq{
		Name:              "默认Docker沙箱",
		Description:       "系统基准沙箱",
		SandboxType:       "docker",
		Image:             "wechatopenai/weknora-sandbox:v1.2.3",
		CPULimit:          3.0,
		MemoryLimitMB:     3072,
		DefaultTimeoutSec: 180,
	}
	if err := s.UpdateRootSandboxConfig(ctx, rootID, rootReq); err != nil {
		t.Fatalf("update root sandbox: %v", err)
	}

	// 调用 GetRootSpaceConfig
	cfg, err := s.GetRootSpaceConfig(ctx, rootID, aesKey)
	if err != nil {
		t.Fatalf("GetRootSpaceConfig error: %v", err)
	}
	if cfg.Sandbox == nil {
		t.Fatalf("expected cfg.Sandbox to be non-nil")
	}
	if cfg.Sandbox.Image != "wechatopenai/weknora-sandbox:v1.2.3" {
		t.Errorf("expected sandbox image %q, got %q", "wechatopenai/weknora-sandbox:v1.2.3", cfg.Sandbox.Image)
	}
	if cfg.Sandbox.CPULimit != 3.0 || cfg.Sandbox.MemoryLimitMB != 3072 || cfg.Sandbox.DefaultTimeoutSec != 180 {
		t.Errorf("expected sandbox limits matched, got %+v", cfg.Sandbox)
	}
}

func TestSandbox_BroadcastRootConfig_Integration(t *testing.T) {
	s, aesKey := setupTestDB(t)
	ctx := context.Background()
	const rootID uint64 = 10001
	const subID uint64 = 20005

	// 创建部门映射
	now := time.Now()
	_ = s.db.Create(&model.WeComDeptTenantMapping{
		WeComDeptID:    505,
		DepartmentCode: "D505",
		TenantID:       subID,
		DeptName:       "技术中心",
		CreatedAt:      now,
		UpdatedAt:      now,
	}).Error

	// 初始化母空间模型、存储、沙箱
	_ = s.UpdateRootModel(ctx, rootID, model.UpdateModelReq{
		Name:      "qa-model",
		Type:      "KnowledgeQA",
		IsDefault: true,
	}, aesKey)

	_ = s.UpdateRootStorage(ctx, rootID, model.UpdateStorageReq{
		Endpoint:   "minio:9000",
		BucketName: "weknora",
	}, aesKey)

	_ = s.UpdateRootSandboxConfig(ctx, rootID, model.UpdateSandboxReq{
		Image:             "wechatopenai/weknora-sandbox:broadcast-v1",
		CPULimit:          2.0,
		MemoryLimitMB:     2048,
		DefaultTimeoutSec: 120,
	})

	// 执行广播
	count, err := s.BroadcastRootConfigToAllTenants(ctx, rootID)
	if err != nil {
		t.Fatalf("broadcast error: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 tenant broadcasted, got %d", count)
	}

	// 验证子空间沙箱已自动生成
	var subSandbox model.TenantSandboxConfigEntity
	err = s.db.Where("tenant_id = ? AND deleted_at IS NULL", subID).First(&subSandbox).Error
	if err != nil {
		t.Fatalf("subtenant sandbox not found: %v", err)
	}

	var subCfg map[string]interface{}
	_ = json.Unmarshal([]byte(subSandbox.Config), &subCfg)
	dockerMap, _ := subCfg["docker"].(map[string]interface{})
	if dockerMap["image"] != "wechatopenai/weknora-sandbox:broadcast-v1" {
		t.Errorf("expected broadcasted image, got %v", dockerMap["image"])
	}
}
