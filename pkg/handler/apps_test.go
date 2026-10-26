package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupTestAppHandler(t *testing.T) (*Handler, *gin.Engine) {
	gin.SetMode(gin.TestMode)

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", uuid.New().String())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("failed to open sqlite in-memory db: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)

	if err := db.AutoMigrate(&model.DeployedApp{}); err != nil {
		t.Fatalf("auto migrate failed: %v", err)
	}

	cfg := &config.Config{
		AppDeploy: config.AppDeployConfig{
			IntranetRootDomain: "apps.opc.intra",
			OnlineResources: []config.OnlineResourceConfig{
				{
					ID:          "pg-test",
					Name:        "Test PostgreSQL",
					Type:        "postgres",
					Description: "Test DB",
					EnvVars: map[string]string{
						"DATABASE_URL": "postgresql://usr:pwd@10.0.0.1:5432/db",
						"PGHOST":       "10.0.0.1",
					},
				},
			},
		},
	}

	storageInstance := storage.NewStorageWithDB(db)
	h := NewHandler(cfg, nil, nil, nil, nil, storageInstance)

	r := gin.New()
	apps := r.Group("/api/v1/apps")
	{
		apps.POST("/check-slug", h.CheckSlug)
		apps.GET("/resources", h.ListResources)
		apps.POST("/publish", h.PublishApp)
		apps.GET("", h.ListApps)
		apps.GET("/:slug", h.GetApp)
		apps.PUT("/:slug/config", h.UpdateAppConfig)
	}

	return h, r
}

func TestHandler_CheckSlug(t *testing.T) {
	_, r := setupTestAppHandler(t)

	// 1. Valid and available
	body, _ := json.Marshal(map[string]string{"slug": "test-app"})
	req := httptest.NewRequest("POST", "/api/v1/apps/check-slug", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp CheckSlugResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !resp.Available || resp.AppURL != "http://test-app.apps.opc.intra" || resp.SkillName != "app-test-app" {
		t.Errorf("unexpected check-slug resp: %+v", resp)
	}

	// 2. Invalid format
	body, _ = json.Marshal(map[string]string{"slug": "INVALID_SLUG!"})
	req = httptest.NewRequest("POST", "/api/v1/apps/check-slug", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid slug, got %d", w.Code)
	}
}

func TestHandler_ListResources(t *testing.T) {
	_, r := setupTestAppHandler(t)

	req := httptest.NewRequest("GET", "/api/v1/apps/resources", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var result struct {
		Resources []model.ResourceSummary `json:"resources"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if len(result.Resources) != 1 || result.Resources[0].ID != "pg-test" {
		t.Fatalf("unexpected resources: %+v", result)
	}
	if len(result.Resources[0].EnvKeys) != 2 {
		t.Errorf("expected 2 env keys, got %v", result.Resources[0].EnvKeys)
	}
}

func TestHandler_PublishAndList(t *testing.T) {
	_, r := setupTestAppHandler(t)

	// Publish multipart
	buf := &bytes.Buffer{}
	writer := multipart.NewWriter(buf)
	_ = writer.WriteField("name", "报销系统")
	_ = writer.WriteField("slug", "expense-app")
	_ = writer.WriteField("description", "报销管理系统")
	_ = writer.WriteField("container_port", "8080")
	_ = writer.WriteField("app_config", `[{"key":"MAX_SIZE","value":"100"}]`)
	_ = writer.WriteField("resource_ids", `["pg-test"]`)

	part1, _ := writer.CreateFormFile("app_tar", "app.tar")
	part1.Write([]byte("mock-tar"))
	part2, _ := writer.CreateFormFile("skill_zip", "skill.zip")
	part2.Write([]byte("mock-zip"))
	writer.Close()

	req := httptest.NewRequest("POST", "/api/v1/apps/publish", buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted, got %d: %s", w.Code, w.Body.String())
	}

	// GET /api/v1/apps
	listReq := httptest.NewRequest("GET", "/api/v1/apps", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, listReq)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var listResp struct {
		Apps []AppDetailResponse `json:"apps"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &listResp)
	if len(listResp.Apps) != 1 || listResp.Apps[0].Slug != "expense-app" {
		t.Fatalf("expected 1 app, got %+v", listResp.Apps)
	}

	// GET /api/v1/apps/expense-app
	getReq := httptest.NewRequest("GET", "/api/v1/apps/expense-app", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, getReq)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// PUT /api/v1/apps/expense-app/config
	updatePayload, _ := json.Marshal(UpdateAppConfigRequest{
		ContainerPort: 9000,
		AppConfig: []model.AppConfigItem{
			{Key: "MAX_SIZE", Value: "200"},
		},
		ResourceIDs: []string{"pg-test"},
	})
	putReq := httptest.NewRequest("PUT", "/api/v1/apps/expense-app/config", bytes.NewReader(updatePayload))
	putReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, putReq)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected deployment service unavailable without a controller, got %d: %s", w.Code, w.Body.String())
	}
}
