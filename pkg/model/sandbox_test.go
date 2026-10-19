package model_test

import (
	"encoding/json"
	"testing"
	"time"

	"weknora-tokendance/pkg/model"
)

func TestRootSandboxDTO_JSON(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	dto := model.RootSandboxDTO{
		ID:                "test-uuid",
		Name:              "默认Docker沙箱",
		Description:       "测试沙箱环境",
		SandboxType:       "docker",
		Image:             "wechatopenai/weknora-sandbox:latest",
		CPULimit:          2.0,
		MemoryLimitMB:     2048,
		DefaultTimeoutSec: 120,
		UpdatedAt:         now,
	}

	data, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var parsed model.RootSandboxDTO
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if parsed.ID != dto.ID ||
		parsed.Name != dto.Name ||
		parsed.Description != dto.Description ||
		parsed.SandboxType != dto.SandboxType ||
		parsed.Image != dto.Image ||
		parsed.CPULimit != dto.CPULimit ||
		parsed.MemoryLimitMB != dto.MemoryLimitMB ||
		parsed.DefaultTimeoutSec != dto.DefaultTimeoutSec ||
		!parsed.UpdatedAt.Equal(dto.UpdatedAt) {
		t.Errorf("parsed dto mismatch, expected %+v, got %+v", dto, parsed)
	}
}

func TestRootConfigDTO_WithSandbox(t *testing.T) {
	cfg := model.RootConfigDTO{
		TenantID:    10001,
		Name:        "母空间",
		Description: "系统母空间",
		Sandbox: &model.RootSandboxDTO{
			ID:          "sandbox-uuid",
			SandboxType: "docker",
			Image:       "wechatopenai/weknora-sandbox:latest",
		},
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var parsed model.RootConfigDTO
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if parsed.Sandbox == nil {
		t.Fatalf("expected sandbox not to be nil")
	}
	if parsed.Sandbox.ID != "sandbox-uuid" || parsed.Sandbox.Image != "wechatopenai/weknora-sandbox:latest" {
		t.Errorf("parsed sandbox mismatch: %+v", parsed.Sandbox)
	}
}

func TestTenantSandboxConfigEntity_TableName(t *testing.T) {
	entity := model.TenantSandboxConfigEntity{}
	if entity.TableName() != "tenant_sandbox_configs" {
		t.Errorf("expected TableName to be tenant_sandbox_configs, got %s", entity.TableName())
	}
}

func TestUpdateSandboxReq_Validation(t *testing.T) {
	tests := []struct {
		name    string
		req     model.UpdateSandboxReq
		wantErr bool
	}{
		{
			name: "valid request",
			req: model.UpdateSandboxReq{
				Name:              "默认Docker沙箱",
				SandboxType:       "docker",
				Image:             "wechatopenai/weknora-sandbox:latest",
				CPULimit:          2.0,
				MemoryLimitMB:     2048,
				DefaultTimeoutSec: 120,
			},
			wantErr: false,
		},
		{
			name: "missing image",
			req: model.UpdateSandboxReq{
				Name:        "默认Docker沙箱",
				SandboxType: "docker",
				Image:       "",
			},
			wantErr: true,
		},
		{
			name: "negative cpu limit",
			req: model.UpdateSandboxReq{
				Image:    "wechatopenai/weknora-sandbox:latest",
				CPULimit: -1,
			},
			wantErr: true,
		},
		{
			name: "negative memory limit",
			req: model.UpdateSandboxReq{
				Image:         "wechatopenai/weknora-sandbox:latest",
				MemoryLimitMB: -512,
			},
			wantErr: true,
		},
		{
			name: "negative timeout",
			req: model.UpdateSandboxReq{
				Image:             "wechatopenai/weknora-sandbox:latest",
				DefaultTimeoutSec: -10,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
