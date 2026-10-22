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

func TestLoadConfig_StateDB(t *testing.T) {
	tempDir := t.TempDir()
	yamlContent := `
server:
  port: 3002
statedb:
  host: "state-db"
  port: 5433
  user: "state-user"
  password: "yaml-state-pass"
  dbname: "custom_state"
  sslmode: "require"
`
	configPath := filepath.Join(tempDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	t.Run("loads statedb from yaml", func(t *testing.T) {
		for _, key := range []string{"STATE_DB_HOST", "STATE_DB_PORT", "STATE_DB_USER", "STATE_DB_PASSWORD", "STATE_DB_NAME", "STATE_DB_SSLMODE"} {
			t.Setenv(key, "")
		}
		cfg, err := LoadConfig(configPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.StateDB.Host != "state-db" || cfg.StateDB.Port != 5433 || cfg.StateDB.User != "state-user" ||
			cfg.StateDB.Password != "yaml-state-pass" || cfg.StateDB.DBName != "custom_state" || cfg.StateDB.SSLMode != "require" {
			t.Errorf("unexpected statedb config: %+v", cfg.StateDB)
		}
	})

	t.Run("env STATE_DB_* overrides yaml", func(t *testing.T) {
		t.Setenv("STATE_DB_HOST", "env-state-db")
		t.Setenv("STATE_DB_PORT", "5544")
		t.Setenv("STATE_DB_USER", "env-user")
		t.Setenv("STATE_DB_PASSWORD", "env-pass")
		t.Setenv("STATE_DB_NAME", "env_state")
		t.Setenv("STATE_DB_SSLMODE", "disable")
		cfg, err := LoadConfig(configPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.StateDB.Host != "env-state-db" || cfg.StateDB.Port != 5544 || cfg.StateDB.User != "env-user" ||
			cfg.StateDB.Password != "env-pass" || cfg.StateDB.DBName != "env_state" || cfg.StateDB.SSLMode != "disable" {
			t.Errorf("env override failed: %+v", cfg.StateDB)
		}
	})

	t.Run("dbname defaults to weknora_gateway_state", func(t *testing.T) {
		for _, key := range []string{"STATE_DB_HOST", "STATE_DB_PORT", "STATE_DB_USER", "STATE_DB_PASSWORD", "STATE_DB_NAME", "STATE_DB_SSLMODE"} {
			t.Setenv(key, "")
		}
		emptyPath := filepath.Join(tempDir, "config-empty.yaml")
		if err := os.WriteFile(emptyPath, []byte("server:\n  port: 3002\n"), 0644); err != nil {
			t.Fatalf("failed to write temp config: %v", err)
		}
		cfg, err := LoadConfig(emptyPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.StateDB.DBName != "weknora_gateway_state" {
			t.Errorf("expected default dbname weknora_gateway_state, got %q", cfg.StateDB.DBName)
		}
	})
}

func TestLoadConfig_AuthKey(t *testing.T) {
	tempDir := t.TempDir()
	yamlContent := `
server:
  port: 3002
auth:
  key: "yaml-auth-key"
`
	configPath := filepath.Join(tempDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	t.Run("loads auth key from yaml", func(t *testing.T) {
		t.Setenv("PLATFORM_AUTH_KEY", "")
		cfg, err := LoadConfig(configPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Auth.Key != "yaml-auth-key" {
			t.Errorf("expected yaml-auth-key, got %q", cfg.Auth.Key)
		}
	})

	t.Run("env PLATFORM_AUTH_KEY overrides yaml", func(t *testing.T) {
		t.Setenv("PLATFORM_AUTH_KEY", "env-auth-key")
		cfg, err := LoadConfig(configPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Auth.Key != "env-auth-key" {
			t.Errorf("expected env override env-auth-key, got %q", cfg.Auth.Key)
		}
	})
}

func TestLoadConfig_BridgeKey(t *testing.T) {
	tempDir := t.TempDir()
	yamlContent := `
server:
  port: 3002
bridge:
  key: "yaml-bridge-key"
`
	configPath := filepath.Join(tempDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	t.Run("loads bridge key from yaml", func(t *testing.T) {
		t.Setenv("PLATFORM_BRIDGE_KEY", "")
		cfg, err := LoadConfig(configPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Bridge.Key != "yaml-bridge-key" {
			t.Errorf("expected yaml-bridge-key, got %q", cfg.Bridge.Key)
		}
	})

	t.Run("env PLATFORM_BRIDGE_KEY overrides yaml", func(t *testing.T) {
		t.Setenv("PLATFORM_BRIDGE_KEY", "env-bridge-key")
		cfg, err := LoadConfig(configPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Bridge.Key != "env-bridge-key" {
			t.Errorf("expected env override env-bridge-key, got %q", cfg.Bridge.Key)
		}
	})
}

func TestLoadConfig_OIDC(t *testing.T) {
	tempDir := t.TempDir()
	yamlContent := `
server:
  port: 3002
oidc:
  client_id: "yaml-oidc-client"
  client_secret: "yaml-oidc-secret"
`
	configPath := filepath.Join(tempDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	t.Run("loads oidc client credentials from yaml", func(t *testing.T) {
		t.Setenv("OIDC_CLIENT_ID", "")
		t.Setenv("OIDC_CLIENT_SECRET", "")
		cfg, err := LoadConfig(configPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.OIDC.ClientID != "yaml-oidc-client" {
			t.Errorf("expected yaml-oidc-client, got %q", cfg.OIDC.ClientID)
		}
		if cfg.OIDC.ClientSecret != "yaml-oidc-secret" {
			t.Errorf("expected yaml-oidc-secret, got %q", cfg.OIDC.ClientSecret)
		}
	})

	t.Run("env OIDC_CLIENT_ID/OIDC_CLIENT_SECRET overrides yaml", func(t *testing.T) {
		t.Setenv("OIDC_CLIENT_ID", "env-oidc-client")
		t.Setenv("OIDC_CLIENT_SECRET", "env-oidc-secret")
		cfg, err := LoadConfig(configPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.OIDC.ClientID != "env-oidc-client" {
			t.Errorf("expected env override env-oidc-client, got %q", cfg.OIDC.ClientID)
		}
		if cfg.OIDC.ClientSecret != "env-oidc-secret" {
			t.Errorf("expected env override env-oidc-secret, got %q", cfg.OIDC.ClientSecret)
		}
	})

	t.Run("unconfigured oidc stays empty (endpoints fail closed 501)", func(t *testing.T) {
		t.Setenv("OIDC_CLIENT_ID", "")
		t.Setenv("OIDC_CLIENT_SECRET", "")
		emptyPath := filepath.Join(tempDir, "empty.yaml")
		if err := os.WriteFile(emptyPath, []byte("server:\n  port: 3002\n"), 0644); err != nil {
			t.Fatalf("failed to write temp config: %v", err)
		}
		cfg, err := LoadConfig(emptyPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.OIDC.ClientID != "" || cfg.OIDC.ClientSecret != "" {
			t.Errorf("expected empty oidc config, got %+v", cfg.OIDC)
		}
	})
}
