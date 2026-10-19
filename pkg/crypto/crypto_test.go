package crypto

import (
	"strings"
	"testing"
)

const testAESKey = "0123456789abcdef0123456789abcdef" // 32 bytes

func TestEncryptAESGCM(t *testing.T) {
	key := []byte(testAESKey)

	t.Run("encrypts plaintext with enc:v1: prefix", func(t *testing.T) {
		encrypted, err := EncryptAESGCM("minio-secret-password", key)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.HasPrefix(encrypted, EncPrefix) {
			t.Errorf("expected prefix %s, got %s", EncPrefix, encrypted)
		}
		if encrypted == "minio-secret-password" {
			t.Errorf("expected ciphertext to differ from plaintext")
		}
	})

	t.Run("returns empty string as-is", func(t *testing.T) {
		encrypted, err := EncryptAESGCM("", key)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if encrypted != "" {
			t.Errorf("expected empty string, got %s", encrypted)
		}
	})

	t.Run("returns already encrypted string as-is (idempotent)", func(t *testing.T) {
		first, err := EncryptAESGCM("minio-secret-password", key)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		second, err := EncryptAESGCM(first, key)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if first != second {
			t.Errorf("re-encrypting should be a no-op: first=%s, second=%s", first, second)
		}
	})

	t.Run("returns plaintext when key is nil", func(t *testing.T) {
		encrypted, err := EncryptAESGCM("minio-secret-password", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if encrypted != "minio-secret-password" {
			t.Errorf("expected plaintext when key is nil, got %s", encrypted)
		}
	})
}

func TestDecryptAESGCM(t *testing.T) {
	key := []byte(testAESKey)

	t.Run("round-trip encrypt then decrypt", func(t *testing.T) {
		original := "minio-secret-access-key-12345"
		encrypted, err := EncryptAESGCM(original, key)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		decrypted, err := DecryptAESGCM(encrypted, key)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if decrypted != original {
			t.Errorf("expected %s, got %s", original, decrypted)
		}
	})

	t.Run("returns legacy plaintext as-is (no enc:v1: prefix)", func(t *testing.T) {
		decrypted, err := DecryptAESGCM("legacy-plaintext-credential", key)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if decrypted != "legacy-plaintext-credential" {
			t.Errorf("expected legacy-plaintext-credential, got %s", decrypted)
		}
	})

	t.Run("returns empty string as-is", func(t *testing.T) {
		decrypted, err := DecryptAESGCM("", key)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if decrypted != "" {
			t.Errorf("expected empty string, got %s", decrypted)
		}
	})

	t.Run("returns ErrEncryptedDataMissingKey when key is nil on encrypted data", func(t *testing.T) {
		_, err := DecryptAESGCM("enc:v1:something", nil)
		if err != ErrEncryptedDataMissingKey {
			t.Errorf("expected ErrEncryptedDataMissingKey, got %v", err)
		}
	})

	t.Run("fails when corrupt ciphertext", func(t *testing.T) {
		_, err := DecryptAESGCM("enc:v1:invalid-base64!!!", key)
		if err == nil {
			t.Errorf("expected error on invalid base64")
		}
	})

	t.Run("fails when wrong key decrypts ciphertext", func(t *testing.T) {
		encrypted, err := EncryptAESGCM("secret-token", key)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		wrongKey := []byte("00000000000000000000000000000000")
		_, err = DecryptAESGCM(encrypted, wrongKey)
		if err == nil {
			t.Errorf("expected authentication tag error with wrong key")
		}
	})
}
