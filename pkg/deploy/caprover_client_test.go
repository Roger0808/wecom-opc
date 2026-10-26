package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"weknora-tokendance/pkg/model"
)

func TestApprovedConfigurationAppliedBeforeFirstHealthProbe(t *testing.T) {
	for _, rejectConfig := range []bool{false, true} {
		var configured atomic.Bool
		var probes atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v2/login":
				json.NewEncoder(w).Encode(map[string]interface{}{"status": 100, "data": map[string]string{"token": "test"}})
			case "/api/v2/user/apps/appData/sample/":
				json.NewEncoder(w).Encode(map[string]interface{}{"status": 100, "data": map[string]interface{}{"isAppBuilding": false, "isBuildFailed": false}})
			case "/healthz":
				probes.Add(1)
				if !configured.Load() {
					t.Error("health checked before approved environment was applied")
				}
				w.WriteHeader(http.StatusOK)
			default:
				http.NotFound(w, r)
			}
		}))
		client := NewCapRoverClient(server.URL, "test", "apps.localhost")
		client.PollInterval = time.Millisecond
		client.ProbeInterval = time.Millisecond
		_, err := client.WaitBuildConfigureAndHealthz(context.Background(), "sample", server.URL, func() error {
			if rejectConfig {
				return errors.New("configuration rejected")
			}
			configured.Store(true)
			return nil
		}, nil)
		server.Close()
		if rejectConfig && (err == nil || probes.Load() != 0) {
			t.Fatal("configuration failure did not stop deployment")
		}
		if !rejectConfig && (err != nil || probes.Load() != 1) {
			t.Fatalf("expected configured healthy application: %v", err)
		}
	}
}

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

func TestCapRoverClient_Status101AndPlaceholderFilter(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/login":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status": 100,
				"data":   map[string]string{"token": "tok"},
			})
		case "/api/v2/user/apps/appData/test-app/":
			if r.Method == "POST" {
				// CapRover detached=1 returns 101 STATUS_OK_DEPLOY_STARTED
				json.NewEncoder(w).Encode(map[string]interface{}{
					"status":      101,
					"description": "Deploy started",
				})
			} else {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"status": 100,
					"data": map[string]interface{}{
						"isAppBuilding": false,
						"isBuildFailed": false,
						"logs":          map[string]interface{}{"lines": []string{"Built ok"}},
					},
				})
			}
		}
	}))
	defer ts.Close()

	probeCount := 0
	healthTs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeCount++
		w.WriteHeader(http.StatusOK)
		if probeCount == 1 {
			// First probe returns CapRover placeholder HTML (should be rejected)
			w.Write([]byte("<!DOCTYPE html><html><head><title>Powered by CapRover</title></head></html>"))
			return
		}
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer healthTs.Close()

	client := NewCapRoverClient(ts.URL, "pwd", "apps.localhost")
	client.PollInterval = 5 * time.Millisecond
	client.ProbeInterval = 5 * time.Millisecond
	ctx := context.Background()

	if err := client.UploadAppTar(ctx, "test-app", []byte("tar")); err != nil {
		t.Fatalf("expected status 101 to succeed, got: %v", err)
	}
	if _, err := client.WaitBuildAndHealthz(ctx, "test-app", healthTs.URL); err != nil {
		t.Fatalf("expected healthz to pass on second probe, got: %v", err)
	}
	if probeCount < 2 {
		t.Fatalf("expected placeholder HTML to be rejected on probe 1, got probeCount=%d", probeCount)
	}
}

func TestKnowledgeSyncer_ErrorOnUnauthorizedOrUnconfigured(t *testing.T) {
	ctx := context.Background()
	unconfigured := NewKnowledgeSyncer("http://localhost:8080", "")
	if _, err := unconfigured.PushAndEnableSkill(ctx, []byte("zip")); err == nil {
		t.Fatalf("expected error when token is unconfigured, got nil")
	}

	ts401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": "Unauthorized: invalid or expired token",
		})
	}))
	defer ts401.Close()

	syncer401 := NewKnowledgeSyncer(ts401.URL, "bad-token")
	if _, err := syncer401.PushAndEnableSkill(ctx, []byte("zip")); err == nil {
		t.Fatalf("expected error on HTTP 401, got nil")
	}
}
