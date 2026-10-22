package deploy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"weknora-tokendance/pkg/model"
)

func TestCapRoverClient_Flow(t *testing.T) {
	loginCalled := false
	registerCalled := false
	updateCalled := false
	uploadCalled := false
	statusPollCount := 0

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/login":
			loginCalled = true
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status":      100,
				"description": "Logged in",
				"data":        map[string]string{"token": "mock-token-xyz"},
			})
		case "/api/v2/user/apps/appDefinitions/register":
			registerCalled = true
			if r.Header.Get("x-captain-auth") != "mock-token-xyz" {
				t.Errorf("missing or invalid auth token: %s", r.Header.Get("x-captain-auth"))
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status":      100,
				"description": "App registered",
			})
		case "/api/v2/user/apps/appDefinitions/update":
			updateCalled = true
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status":      100,
				"description": "App updated",
			})
		case "/api/v2/user/apps/appData/test-app/":
			if r.Method == "POST" {
				uploadCalled = true
				json.NewEncoder(w).Encode(map[string]interface{}{
					"status":      100,
					"description": "Build started",
				})
			} else if r.Method == "GET" {
				statusPollCount++
				// First poll building, second poll done
				isBuilding := statusPollCount < 2
				json.NewEncoder(w).Encode(map[string]interface{}{
					"status":      100,
					"description": "App status",
					"data": map[string]interface{}{
						"isAppBuilding": isBuilding,
						"isBuildFailed": false,
						"logs": map[string]interface{}{
							"lines": []string{"Step 1/2: Build success"},
						},
					},
				})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	healthTs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
			return
		}
		http.NotFound(w, r)
	}))
	defer healthTs.Close()

	client := NewCapRoverClient(ts.URL, "secret123", "apps.opc.intra")
	client.PollInterval = 10 * time.Millisecond
	client.ProbeInterval = 10 * time.Millisecond
	ctx := context.Background()

	// 1. EnsureAppRegistered
	if err := client.EnsureAppRegistered(ctx, "test-app"); err != nil {
		t.Fatalf("register failed: %v", err)
	}
	if !loginCalled || !registerCalled {
		t.Errorf("login or register not called")
	}

	// 2. UpdateAppConfigAndEnv
	envVars := []model.AppConfigItem{
		{Key: "PORT", Value: "8080"},
		{Key: "DATABASE_URL", Value: "postgres://..."},
	}
	if err := client.UpdateAppConfigAndEnv(ctx, "test-app", 8080, envVars); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if !updateCalled {
		t.Errorf("update not called")
	}

	// 3. UploadAppTar
	if err := client.UploadAppTar(ctx, "test-app", []byte("fake-tar-bytes")); err != nil {
		t.Fatalf("upload failed: %v", err)
	}
	if !uploadCalled {
		t.Errorf("upload not called")
	}

	// 4. WaitBuildAndHealthz
	logs, err := client.WaitBuildAndHealthz(ctx, "test-app", healthTs.URL)
	if err != nil {
		t.Fatalf("wait build and healthz failed: %v", err)
	}
	if logs == "" {
		t.Errorf("expected logs to be non-empty")
	}
}

func TestKnowledgeSyncer_PushAndEnableSkill(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/skills/catalog":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"code":    0,
				"message": "success",
				"data":    map[string]string{"id": "cat_test_999"},
			})
		case "/api/v1/skills/catalog/cat_test_999/install":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"code":    0,
				"message": "success",
				"data":    map[string]string{"id": "skill_inst_111"},
			})
		case "/api/v1/skills/skill_inst_111":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"code":    0,
				"message": "success",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	syncer := NewKnowledgeSyncer(ts.URL, "admin-token-xyz")
	ctx := context.Background()

	catID, err := syncer.PushAndEnableSkill(ctx, []byte("fake-skill-zip"))
	if err != nil {
		t.Fatalf("PushAndEnableSkill failed: %v", err)
	}
	if catID != "cat_test_999" {
		t.Errorf("expected cat_test_999, got %s", catID)
	}
}
