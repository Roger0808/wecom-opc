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
