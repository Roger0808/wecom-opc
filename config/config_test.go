package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_SystemAESKey(t *testing.T) {
	tempDir := t.TempDir()
	yamlContent := `
server:
  port: 3002
system_aes_key: "0123456789abcdef0123456789abcdef"
`
	configPath := filepath.Join(tempDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	t.Run("loads system_aes_key from yaml", func(t *testing.T) {
		t.Setenv("SYSTEM_AES_KEY", "")
		cfg, err := LoadConfig(configPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.SystemAESKey != "0123456789abcdef0123456789abcdef" {
			t.Errorf("expected 0123456789abcdef0123456789abcdef, got %s", cfg.SystemAESKey)
		}
		key := cfg.GetAESKey()
		if string(key) != "0123456789abcdef0123456789abcdef" {
			t.Errorf("GetAESKey returned incorrect key: %s", string(key))
		}
	})

	t.Run("env SYSTEM_AES_KEY overrides yaml", func(t *testing.T) {
		overrideKey := "12345678901234567890123456789012"
		t.Setenv("SYSTEM_AES_KEY", overrideKey)
		cfg, err := LoadConfig(configPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.SystemAESKey != overrideKey {
			t.Errorf("expected env override %s, got %s", overrideKey, cfg.SystemAESKey)
		}
		if string(cfg.GetAESKey()) != overrideKey {
			t.Errorf("expected %s, got %s", overrideKey, string(cfg.GetAESKey()))
		}
	})

	t.Run("GetAESKey returns nil for invalid length", func(t *testing.T) {
		cfg := &Config{SystemAESKey: "short-key"}
		if cfg.GetAESKey() != nil {
			t.Errorf("expected nil for short key, got %v", cfg.GetAESKey())
		}
	})
}

func TestLoadConfig_AppDeploy(t *testing.T) {
	tempDir := t.TempDir()
	yamlContent := `
server:
  port: 3002
app_deploy:
  intranet_root_domain: "internal.company.net"
  caprover_url: "http://deploy.internal:3000"
  caprover_password: "initial-password"
  knowledge_base_url: "http://knowledge.internal:8080"
  knowledge_admin_token: "init-token"
  online_resources:
    - id: "pg-shared"
      name: "Shared Postgres"
      type: "postgres"
      description: "Company shared DB"
      env_vars:
        DATABASE_URL: "postgres://user:pass@10.0.0.1:5432/db"
`
	configPath := filepath.Join(tempDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	t.Run("loads app_deploy config from yaml", func(t *testing.T) {
		cfg, err := LoadConfig(configPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.AppDeploy.IntranetRootDomain != "internal.company.net" {
			t.Errorf("expected internal.company.net, got %s", cfg.AppDeploy.IntranetRootDomain)
		}
		if len(cfg.AppDeploy.OnlineResources) != 1 {
			t.Fatalf("expected 1 online resource, got %d", len(cfg.AppDeploy.OnlineResources))
		}
		if cfg.AppDeploy.OnlineResources[0].ID != "pg-shared" {
			t.Errorf("expected pg-shared, got %s", cfg.AppDeploy.OnlineResources[0].ID)
		}
	})

	t.Run("env overrides app_deploy values", func(t *testing.T) {
		t.Setenv("APP_DEPLOY_INTRANET_ROOT_DOMAIN", "apps.custom.intra")
		t.Setenv("APP_DEPLOY_CAPROVER_URL", "http://custom-deploy:3000")
		t.Setenv("APP_DEPLOY_CAPROVER_PASSWORD", "secret-caprover")
		t.Setenv("APP_DEPLOY_KNOWLEDGE_BASE_URL", "http://custom-knowledge:8080")
		t.Setenv("APP_DEPLOY_KNOWLEDGE_ADMIN_TOKEN", "secret-knowledge-token")

		cfg, err := LoadConfig(configPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.AppDeploy.IntranetRootDomain != "apps.custom.intra" {
			t.Errorf("expected apps.custom.intra, got %s", cfg.AppDeploy.IntranetRootDomain)
		}
		if cfg.AppDeploy.CaproverURL != "http://custom-deploy:3000" {
			t.Errorf("expected http://custom-deploy:3000, got %s", cfg.AppDeploy.CaproverURL)
		}
		if cfg.AppDeploy.CaproverPassword != "secret-caprover" {
			t.Errorf("expected secret-caprover, got %s", cfg.AppDeploy.CaproverPassword)
		}
		if cfg.AppDeploy.KnowledgeBaseURL != "http://custom-knowledge:8080" {
			t.Errorf("expected http://custom-knowledge:8080, got %s", cfg.AppDeploy.KnowledgeBaseURL)
		}
		if cfg.AppDeploy.KnowledgeAdminToken != "secret-knowledge-token" {
			t.Errorf("expected secret-knowledge-token, got %s", cfg.AppDeploy.KnowledgeAdminToken)
		}
	})
}
