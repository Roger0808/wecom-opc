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
	"weknora-tokendance/pkg/adminauth"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// setupAuthzTestHandler 搭建带 /admin/api/spaces/:id 路由与可配置白名单的测试环境。
// 返回主库句柄（WeKnora 角色）与状态库句柄（audit_events 落在状态库）。
func setupAuthzTestHandler(t *testing.T, allowedOrigins []string) (*gin.Engine, *gorm.DB, *gorm.DB) {
	t.Helper()
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

	stateDSN := fmt.Sprintf("file:%s?mode=memory&cache=shared", uuid.New().String())
	stateDB, err := gorm.Open(sqlite.Open(stateDSN), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("failed to open sqlite in-memory state db: %v", err)
	}
	stateSQLDB, _ := stateDB.DB()
	stateSQLDB.SetMaxOpenConns(1)

	if err := db.AutoMigrate(
		&model.WeKnoraTenant{},
		&model.WeKnoraTenantMember{},
		&model.WeKnoraUser{},
	); err != nil {
		t.Fatalf("failed to auto migrate: %v", err)
	}
	if err := stateDB.AutoMigrate(&model.AuditEvent{}); err != nil {
		t.Fatalf("failed to auto migrate state db: %v", err)
	}

	cfg := &config.Config{
		Admin: config.AdminConfig{Key: string(testAdminSecret)},
	}
	s := storage.NewStorageWithDBs(db, stateDB)
	h := NewHandler(cfg, nil, nil, nil, nil, s)

	r := gin.New()
	adminAPI := r.Group("/admin/api", adminauth.RequireAdmin(adminauth.MiddlewareConfig{
		Secret:         testAdminSecret,
		AllowedOrigins: allowedOrigins,
		Audit:          s.InsertAuditEvent,
	}))
	{
		adminAPI.GET("/root-config", h.AdminGetRootConfig)
		adminAPI.POST("/spaces/:id/admins", h.AdminUpdateSpaceAdmin)
	}
	return r, db, stateDB
}

func doAuthzRequest(t *testing.T, r *gin.Engine, method, path, token, origin, body string) *httptest.ResponseRecorder {
	t.Helper()
	req, _ := http.NewRequest(method, path, bytes.NewBufferString(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func countAuditByAction(t *testing.T, db *gorm.DB, action string) int64 {
	t.Helper()
	var count int64
	if err := db.Model(&model.AuditEvent{}).Where("action = ?", action).Count(&count).Error; err != nil {
		t.Fatalf("count audit events: %v", err)
	}
	return count
}

func TestRequireAdmin_NegativeCases(t *testing.T) {
	r, db, stateDB := setupAuthzTestHandler(t, []string{"https://admin.example.com"})

	now := time.Now()
	_ = db.Create(&model.WeKnoraTenant{ID: 10002, Name: "部门空间A", CreatedAt: now, UpdatedAt: now})
	_ = db.Create(&model.WeKnoraTenant{ID: 10003, Name: "部门空间B", CreatedAt: now, UpdatedAt: now})

	validBody := `{"user_id":"user-001","action":"add"}`

	t.Run("no token returns 401 and audits anonymous denial", func(t *testing.T) {
		w := doAuthzRequest(t, r, http.MethodPost, "/admin/api/spaces/10002/admins", "", "", validBody)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d, body: %s", w.Code, w.Body.String())
		}

		var event model.AuditEvent
		if err := stateDB.Where("action = ?", "admin.auth.denied").First(&event).Error; err != nil {
			t.Fatalf("expected denied audit row: %v", err)
		}
		if event.Actor != "anonymous" {
			t.Errorf("expected actor anonymous, got %s", event.Actor)
		}
	})

	t.Run("forged token returns 401", func(t *testing.T) {
		w := doAuthzRequest(t, r, http.MethodPost, "/admin/api/spaces/10002/admins", "not.a.token", "", validBody)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", w.Code)
		}
	})

	t.Run("system_admin passes", func(t *testing.T) {
		token := mustAdminToken(t, "root-admin", adminauth.RoleSystemAdmin, 0)
		w := doAuthzRequest(t, r, http.MethodPost, "/admin/api/spaces/10002/admins", token, "", validBody)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("dept_admin same tenant passes", func(t *testing.T) {
		token := mustAdminToken(t, "dept-admin-a", adminauth.RoleDeptAdmin, 10002)
		w := doAuthzRequest(t, r, http.MethodPost, "/admin/api/spaces/10002/admins", token, "", validBody)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("dept_admin cross tenant returns 403 and audits", func(t *testing.T) {
		before := countAuditByAction(t, stateDB, "admin.authz.denied")
		token := mustAdminToken(t, "dept-admin-a", adminauth.RoleDeptAdmin, 10002)
		w := doAuthzRequest(t, r, http.MethodPost, "/admin/api/spaces/10003/admins", token, "", validBody)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d, body: %s", w.Code, w.Body.String())
		}
		if after := countAuditByAction(t, stateDB, "admin.authz.denied"); after != before+1 {
			t.Errorf("expected one more authz.denied audit row, before=%d after=%d", before, after)
		}
	})

	t.Run("dept_admin on non-space route returns 403", func(t *testing.T) {
		token := mustAdminToken(t, "dept-admin-a", adminauth.RoleDeptAdmin, 10002)
		w := doAuthzRequest(t, r, http.MethodGet, "/admin/api/root-config", token, "", "")
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d, body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("forged origin on write returns 403", func(t *testing.T) {
		before := countAuditByAction(t, stateDB, "admin.csrf.denied")
		token := mustAdminToken(t, "root-admin", adminauth.RoleSystemAdmin, 0)
		w := doAuthzRequest(t, r, http.MethodPost, "/admin/api/spaces/10002/admins", token, "https://evil.example.com", validBody)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d, body: %s", w.Code, w.Body.String())
		}
		if after := countAuditByAction(t, stateDB, "admin.csrf.denied"); after != before+1 {
			t.Errorf("expected one more csrf.denied audit row, before=%d after=%d", before, after)
		}
	})

	t.Run("allowed origin on write passes", func(t *testing.T) {
		token := mustAdminToken(t, "root-admin", adminauth.RoleSystemAdmin, 0)
		w := doAuthzRequest(t, r, http.MethodPost, "/admin/api/spaces/10002/admins", token, "https://admin.example.com", validBody)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("no-origin API call passes but audits origin absence", func(t *testing.T) {
		before := countAuditByAction(t, stateDB, "admin.csrf.no_origin")
		token := mustAdminToken(t, "root-admin", adminauth.RoleSystemAdmin, 0)
		w := doAuthzRequest(t, r, http.MethodPost, "/admin/api/spaces/10002/admins", token, "", validBody)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
		}
		if after := countAuditByAction(t, stateDB, "admin.csrf.no_origin"); after != before+1 {
			t.Errorf("expected one more csrf.no_origin audit row, before=%d after=%d", before, after)
		}
	})

	t.Run("successful request writes allowed audit with actor", func(t *testing.T) {
		var event model.AuditEvent
		if err := stateDB.Where("action = ? AND actor = ?", "admin.auth.allowed", "root-admin").Last(&event).Error; err != nil {
			t.Fatalf("expected allowed audit row: %v", err)
		}
		var detail map[string]interface{}
		if err := json.Unmarshal([]byte(event.After), &detail); err != nil {
			t.Fatalf("audit detail should be JSON: %v", err)
		}
		if detail["result"] != "allowed" {
			t.Errorf("expected result=allowed, got %v", detail["result"])
		}
	})
}

func TestRequireAdmin_EmptyWhitelistRejectsAllBrowserOrigins(t *testing.T) {
	r, _, _ := setupAuthzTestHandler(t, nil)

	t.Run("browser origin rejected when whitelist empty", func(t *testing.T) {
		token := mustAdminToken(t, "root-admin", adminauth.RoleSystemAdmin, 0)
		w := doAuthzRequest(t, r, http.MethodPost, "/admin/api/spaces/10002/admins", token, "https://admin.example.com", `{"user_id":"u","action":"add"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", w.Code)
		}
	})

	t.Run("GET without origin still passes", func(t *testing.T) {
		token := mustAdminToken(t, "root-admin", adminauth.RoleSystemAdmin, 0)
		w := doAuthzRequest(t, r, http.MethodGet, "/admin/api/root-config", token, "", "")
		// 无 Origin 头 + GET：通过 CSRF 检查；handler 可能因无数据返回错误，但不应是 401/403
		if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
			t.Fatalf("expected handler-level status, got %d, body: %s", w.Code, w.Body.String())
		}
	})
}
