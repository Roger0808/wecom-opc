package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"weknora-tokendance/pkg/model"

	"github.com/gin-gonic/gin"
)

func TestAdminSandbox_GetRootSandbox(t *testing.T) {
	const rootID uint64 = 10001
	h, r, db, _ := setupTestHandler(t, rootID)

	now := time.Now()
	if err := db.Create(&model.WeKnoraTenant{
		ID:          rootID,
		Name:        "母空间",
		Description: "测试母空间",
		CreatedAt:   now,
		UpdatedAt:   now,
	}).Error; err != nil {
		t.Fatalf("create root tenant: %v", err)
	}

	t.Run("returns 200 with null data when no config exists", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/admin/api/root-config/sandbox", nil)
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d, body: %s", w.Code, w.Body.String())
		}

		var resp struct {
			Code int                  `json:"code"`
			Data *model.RootSandboxDTO `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if resp.Code != 0 {
			t.Fatalf("expected code 0, got %d", resp.Code)
		}
		if resp.Data != nil {
			t.Fatalf("expected nil sandbox data, got %+v", resp.Data)
		}
	})

	t.Run("returns 200 with sandbox config when exists via router", func(t *testing.T) {
		// 先写入沙箱配置
		updateReq := model.UpdateSandboxReq{
			Name:              "默认Docker沙箱",
			Description:       "测试环境Docker沙箱",
			SandboxType:       "docker",
			Image:             "python:3.11-slim",
			CPULimit:          2.0,
			MemoryLimitMB:     2048,
			DefaultTimeoutSec: 60,
		}
		reqBody, _ := json.Marshal(updateReq)
		putReq, _ := http.NewRequest(http.MethodPut, "/admin/api/root-config/sandbox", bytes.NewBuffer(reqBody))
		putReq.Header.Set("Content-Type", "application/json")
		putW := httptest.NewRecorder()
		r.ServeHTTP(putW, putReq)
		if putW.Code != http.StatusOK {
			t.Fatalf("failed to seed sandbox config: %s", putW.Body.String())
		}

		// GET 测试
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodGet, "/admin/api/root-config/sandbox", nil)
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d, body: %s", w.Code, w.Body.String())
		}

		var resp struct {
			Code int                  `json:"code"`
			Data *model.RootSandboxDTO `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if resp.Code != 0 || resp.Data == nil {
			t.Fatalf("unexpected response: %+v", resp)
		}
		if resp.Data.Image != "python:3.11-slim" {
			t.Errorf("expected image python:3.11-slim, got %s", resp.Data.Image)
		}
		if resp.Data.CPULimit != 2.0 {
			t.Errorf("expected cpu_limit 2.0, got %f", resp.Data.CPULimit)
		}
		if resp.Data.MemoryLimitMB != 2048 {
			t.Errorf("expected memory_limit_mb 2048, got %d", resp.Data.MemoryLimitMB)
		}
	})

	t.Run("using CreateTestContext", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request, _ = http.NewRequest(http.MethodGet, "/admin/api/root-config/sandbox", nil)

		h.AdminGetRootSandbox(c)

		if w.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d, body: %s", w.Code, w.Body.String())
		}

		var resp struct {
			Code int                  `json:"code"`
			Data *model.RootSandboxDTO `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if resp.Code != 0 || resp.Data == nil {
			t.Fatalf("unexpected response: %+v", resp)
		}
		if resp.Data.Image != "python:3.11-slim" {
			t.Errorf("expected image python:3.11-slim, got %s", resp.Data.Image)
		}
	})
}

func TestAdminSandbox_UpdateRootSandbox_Success(t *testing.T) {
	const rootID uint64 = 10001
	h, r, db, _ := setupTestHandler(t, rootID)

	now := time.Now()
	_ = db.Create(&model.WeKnoraTenant{
		ID:        rootID,
		Name:      "母空间",
		CreatedAt: now,
		UpdatedAt: now,
	})

	t.Run("router PUT returns 200 on create", func(t *testing.T) {
		body := model.UpdateSandboxReq{
			Name:              "默认Docker沙箱",
			Description:       "测试初始化沙箱",
			SandboxType:       "docker",
			Image:             "golang:1.22-alpine",
			CPULimit:          1.5,
			MemoryLimitMB:     1024,
			DefaultTimeoutSec: 30,
		}
		bodyBytes, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPut, "/admin/api/root-config/sandbox", bytes.NewBuffer(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d, body: %s", w.Code, w.Body.String())
		}

		var resp struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if resp.Code != 0 {
			t.Fatalf("expected code 0, got %d", resp.Code)
		}
		if resp.Message != "母空间沙箱环境配置保存成功" {
			t.Fatalf("expected message 母空间沙箱环境配置保存成功, got %s", resp.Message)
		}

		// 检查数据库记录
		var entity model.TenantSandboxConfigEntity
		if err := db.Where("tenant_id = ? AND deleted_at IS NULL", rootID).First(&entity).Error; err != nil {
			t.Fatalf("find sandbox entity in db: %v", err)
		}
		if entity.Name != "默认Docker沙箱" {
			t.Errorf("expected name 默认Docker沙箱, got %s", entity.Name)
		}
	})

	t.Run("using CreateTestContext returns 200 on update", func(t *testing.T) {
		body := model.UpdateSandboxReq{
			Name:              "自定义Docker沙箱",
			Description:       "更新沙箱镜像",
			SandboxType:       "docker",
			Image:             "python:3.12-slim",
			CPULimit:          4.0,
			MemoryLimitMB:     4096,
			DefaultTimeoutSec: 120,
		}
		bodyBytes, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request, _ = http.NewRequest(http.MethodPut, "/admin/api/root-config/sandbox", bytes.NewBuffer(bodyBytes))
		c.Request.Header.Set("Content-Type", "application/json")

		h.AdminUpdateRootSandbox(c)

		if w.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d, body: %s", w.Code, w.Body.String())
		}

		var resp struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if resp.Code != 0 {
			t.Fatalf("expected code 0, got %d", resp.Code)
		}
		if resp.Message != "母空间沙箱环境配置保存成功" {
			t.Fatalf("expected message 母空间沙箱环境配置保存成功, got %s", resp.Message)
		}

		// 验证数据库最新数据
		var entity model.TenantSandboxConfigEntity
		if err := db.Where("tenant_id = ? AND deleted_at IS NULL", rootID).First(&entity).Error; err != nil {
			t.Fatalf("find sandbox entity in db: %v", err)
		}
		if entity.Name != "自定义Docker沙箱" {
			t.Errorf("expected name 自定义Docker沙箱, got %s", entity.Name)
		}
	})
}

func TestAdminSandbox_UpdateRootSandbox_ValidationErrors(t *testing.T) {
	const rootID uint64 = 10001
	h, r, _, _ := setupTestHandler(t, rootID)

	t.Run("router PUT missing image returns 400", func(t *testing.T) {
		body := map[string]interface{}{
			"name":        "沙箱无镜像",
			"description": "缺少image字段",
		}
		bodyBytes, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPut, "/admin/api/root-config/sandbox", bytes.NewBuffer(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected status 400, got %d, body: %s", w.Code, w.Body.String())
		}

		var resp struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if resp.Code != -1 {
			t.Fatalf("expected code -1, got %d", resp.Code)
		}
		if resp.Message == "" {
			t.Fatalf("expected non-empty error message")
		}
	})

	t.Run("using CreateTestContext empty image string returns 400", func(t *testing.T) {
		body := model.UpdateSandboxReq{
			Name:  "沙箱镜像为空格",
			Image: "   ",
		}
		bodyBytes, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request, _ = http.NewRequest(http.MethodPut, "/admin/api/root-config/sandbox", bytes.NewBuffer(bodyBytes))
		c.Request.Header.Set("Content-Type", "application/json")

		h.AdminUpdateRootSandbox(c)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected status 400, got %d, body: %s", w.Code, w.Body.String())
		}

		var resp struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if resp.Code != -1 {
			t.Fatalf("expected code -1, got %d", resp.Code)
		}
	})

	t.Run("negative cpu limit returns 400", func(t *testing.T) {
		body := model.UpdateSandboxReq{
			Image:    "python:3.11",
			CPULimit: -1.0,
		}
		bodyBytes, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPut, "/admin/api/root-config/sandbox", bytes.NewBuffer(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected status 400, got %d, body: %s", w.Code, w.Body.String())
		}

		var resp struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if resp.Code != -1 {
			t.Fatalf("expected code -1, got %d", resp.Code)
		}
	})

	t.Run("negative memory limit returns 400", func(t *testing.T) {
		body := model.UpdateSandboxReq{
			Image:         "python:3.11",
			MemoryLimitMB: -512,
		}
		bodyBytes, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPut, "/admin/api/root-config/sandbox", bytes.NewBuffer(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected status 400, got %d, body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("negative timeout returns 400", func(t *testing.T) {
		body := model.UpdateSandboxReq{
			Image:             "python:3.11",
			DefaultTimeoutSec: -10,
		}
		bodyBytes, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPut, "/admin/api/root-config/sandbox", bytes.NewBuffer(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected status 400, got %d, body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("malformed json returns 400", func(t *testing.T) {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPut, "/admin/api/root-config/sandbox", bytes.NewBufferString("{invalid-json"))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected status 400, got %d, body: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Code int `json:"code"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.Code != -1 {
			t.Fatalf("expected code -1, got %d", resp.Code)
		}
	})
}

func TestAdminSandbox_StorageErrors(t *testing.T) {
	const rootID uint64 = 10001
	h, _, db, _ := setupTestHandler(t, rootID)

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get sqlDB: %v", err)
	}
	_ = sqlDB.Close()

	t.Run("GetRootSandbox returns 500 on db error", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request, _ = http.NewRequest(http.MethodGet, "/admin/api/root-config/sandbox", nil)

		h.AdminGetRootSandbox(c)

		if w.Code != http.StatusInternalServerError {
			t.Fatalf("expected status 500, got %d, body: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Code int `json:"code"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.Code != -1 {
			t.Fatalf("expected code -1, got %d", resp.Code)
		}
	})

	t.Run("UpdateRootSandbox returns 500 on db error", func(t *testing.T) {
		body := model.UpdateSandboxReq{
			Image: "python:3.11",
		}
		bodyBytes, _ := json.Marshal(body)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request, _ = http.NewRequest(http.MethodPut, "/admin/api/root-config/sandbox", bytes.NewBuffer(bodyBytes))
		c.Request.Header.Set("Content-Type", "application/json")

		h.AdminUpdateRootSandbox(c)

		if w.Code != http.StatusInternalServerError {
			t.Fatalf("expected status 500, got %d, body: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Code int `json:"code"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.Code != -1 {
			t.Fatalf("expected code -1, got %d", resp.Code)
		}
	})
}

func TestAdminSandbox_DefaultRootTenantID(t *testing.T) {
	// 当 RootTenantID 未设置（为 0）时，回退至默认母空间 ID 10001
	h, r, db, _ := setupTestHandler(t, 0)

	now := time.Now()
	_ = db.Create(&model.WeKnoraTenant{
		ID:        10001,
		Name:      "默认回退母空间",
		CreatedAt: now,
		UpdatedAt: now,
	})

	// PUT 更新沙箱
	body := model.UpdateSandboxReq{
		Name:  "默认沙箱配置",
		Image: "python:3.11-alpine",
	}
	bodyBytes, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/admin/api/root-config/sandbox", bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d, body: %s", w.Code, w.Body.String())
	}

	// 确认写入的是 10001
	var entity model.TenantSandboxConfigEntity
	if err := db.Where("tenant_id = 10001 AND deleted_at IS NULL").First(&entity).Error; err != nil {
		t.Fatalf("expected sandbox config for tenant 10001, got error: %v", err)
	}

	// GET 验证
	wGet := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(wGet)
	c.Request, _ = http.NewRequest(http.MethodGet, "/admin/api/root-config/sandbox", nil)
	h.AdminGetRootSandbox(c)

	if wGet.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d, body: %s", wGet.Code, wGet.Body.String())
	}
	var resp struct {
		Code int                  `json:"code"`
		Data *model.RootSandboxDTO `json:"data"`
	}
	_ = json.Unmarshal(wGet.Body.Bytes(), &resp)
	if resp.Data == nil || resp.Data.Image != "python:3.11-alpine" {
		t.Fatalf("expected image python:3.11-alpine, got %+v", resp.Data)
	}
}
