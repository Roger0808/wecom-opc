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
