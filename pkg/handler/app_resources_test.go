package handler

import (
	"strings"
	"testing"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/model"
)

func TestConfirmedResourceValuesNotErasedByBlankInputs(t *testing.T) {
	resources := []config.OnlineResourceConfig{{ID: "redis", EnvVars: map[string]string{"REDIS_URL": "redis://approved:6379", "REDIS_PASSWORD": "test-runtime-only"}}}
	env, err := mergeAppEnvironment(resources, []string{"redis"}, []model.AppConfigItem{{Key: "REDIS_URL", Required: true}, {Key: "REDIS_PASSWORD", Required: true}})
	if err != nil || len(env) != 2 || env[0].Value != "test-runtime-only" || env[1].Value != "redis://approved:6379" {
		t.Fatalf("resource injection was erased or invalid: count=%d error=%v", len(env), err)
	}
}

func TestFieldBindingsOnlyReplaceConfirmedFields(t *testing.T) {
	resources := []config.OnlineResourceConfig{
		{ID: "a", Name: "A", EnvVars: map[string]string{"DB_DSN": "db-a", "PASSWORD": "must-not-replace", "EXTRA": "must-not-inject"}},
		{ID: "b", Name: "B", EnvVars: map[string]string{"DB_DSN": "db-b", "CACHE_URL": "cache-b"}},
	}
	items := []model.AppConfigItem{
		{Key: "DB_DSN", ResourceID: "a", ValueSource: "resource", Required: true},
		{Key: "PASSWORD", Value: "original", ValueSource: "original"},
		{Key: "CACHE_URL", ResourceID: "b", ValueSource: "resource"},
		{Key: "OPTIONAL", ValueSource: "manual"},
	}
	env, err := mergeAppEnvironment(resources, []string{"a", "b"}, items)
	if err != nil || len(env) != 4 {
		t.Fatalf("per-field bindings failed: %v", err)
	}
	values := map[string]string{}
	for _, item := range env {
		values[item.Key] = item.Value
	}
	if values["DB_DSN"] != "db-a" || values["PASSWORD"] != "original" || values["CACHE_URL"] != "cache-b" {
		t.Fatal("confirmed values were changed")
	}
	if _, exists := values["EXTRA"]; exists {
		t.Fatal("unconfirmed resource field injected")
	}
	view := describeAppConfig(resources, []string{"a", "b"}, items)
	if !view[0].HasValue || view[0].SourceLabel != "A" || view[0].Value != "" || view[0].ResourceID != "a" {
		t.Fatal("field binding lost or credential exposed")
	}
	if _, err := mergeAppEnvironment(resources, []string{"a", "b"}, view); err != nil {
		t.Fatal(err)
	}
	items[0].ResourceID = "unknown"
	if _, err := mergeAppEnvironment(resources, []string{"a"}, items); err == nil {
		t.Fatal("unknown field resource accepted")
	}
}

func TestExplicitBlankDoesNotInheritResourceCredential(t *testing.T) {
	resources := []config.OnlineResourceConfig{{ID: "a", EnvVars: map[string]string{"PASSWORD": "not-selected"}}}
	_, err := mergeAppEnvironment(resources, []string{"a"}, []model.AppConfigItem{{Key: "PASSWORD", ValueSource: "manual", Required: true}})
	if err == nil {
		t.Fatal("blank field inherited unconfirmed credential")
	}
}

func TestResourceInjectedConfigurationHasValueWithoutExposingCredential(t *testing.T) {
	resources := []config.OnlineResourceConfig{{ID: "mysql", Name: "Approved MySQL", EnvVars: map[string]string{"DB_DSN": "fixture-secret", "PASSWORD": "resource-secret"}}}
	stored := []model.AppConfigItem{{Key: "DB_DSN", Value: ""}, {Key: "PASSWORD", Value: "own-original"}, {Key: "MISSING", Value: ""}}
	view := describeAppConfig(resources, []string{"mysql"}, stored)
	if !view[0].HasValue || view[0].Value != "" || view[0].ValueSource != "resource" || view[0].SourceLabel != "Approved MySQL" {
		t.Fatal("resource injection must be shown as configured, without returning its secret")
	}
	if view[1].Value != "own-original" || view[1].ValueSource != "configured" || !view[1].HasValue {
		t.Fatal("original application override must be preserved")
	}
	if view[2].HasValue || view[2].ValueSource != "missing" || stored[0].ValueSource != "" {
		t.Fatal("missing and stored values must not be changed by presentation metadata")
	}
	env, err := mergeAppEnvironment(resources, []string{"mysql"}, view)
	if err != nil || env[0].Value != "fixture-secret" || env[2].Value != "own-original" {
		t.Fatal("saving unchanged resource bindings must keep the effective credentials")
	}
}

func TestUnknownConflictingAndMissingResourceBindingsRejected(t *testing.T) {
	resources := []config.OnlineResourceConfig{{ID: "a", EnvVars: map[string]string{"SERVICE_URL": "http://a"}}, {ID: "b", EnvVars: map[string]string{"SERVICE_URL": "http://b"}}}
	for _, ids := range [][]string{{"unknown"}, {"a", "a"}, {"a", "b"}} {
		if _, err := mergeAppEnvironment(resources, ids, nil); err == nil {
			t.Fatalf("invalid resource selection accepted: %v", ids)
		}
	}
	if _, err := mergeAppEnvironment(nil, nil, []model.AppConfigItem{{Key: "MONGODB_URI", Required: true}}); err == nil {
		t.Fatal("missing required connection accepted")
	}
}

func TestResourcePreviewMasksCredentialsButShowsReplacementHost(t *testing.T) {
	preview := resourcePreview(map[string]string{"REDIS_HOST": "redis.internal", "REDIS_PASSWORD": "private-value", "MONGODB_URI": "mongodb://user:private-value@db", "SQLSERVER": "Server=db;Password=private-value", "EXTERNAL_TOKEN": "private-value"})
	if preview["REDIS_HOST"] != "redis.internal" {
		t.Fatal("replacement host missing")
	}
	for key, value := range preview {
		if strings.Contains(value, "private-value") {
			t.Fatalf("credential leaked in %s", key)
		}
	}
}

func TestRuntimeFailureKeepsReasonWithoutCredentials(t *testing.T) {
	log := "CONTACTS_MODE must be ready\nmysql connection: user:private@tcp(db:3306)/test\napi_key=unknown-secret-value\n"
	out := redactAppRuntimeLogs(log, []model.AppConfigItem{{Key: "DB_DSN", Value: "user:private@tcp(db:3306)/test"}})
	if !strings.Contains(out, "CONTACTS_MODE must be ready") || strings.Contains(out, "private") || strings.Contains(out, "unknown-secret-value") {
		t.Fatal("runtime diagnosis was lost or a credential was exposed")
	}
}
