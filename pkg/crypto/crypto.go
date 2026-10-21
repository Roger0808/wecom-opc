package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"strings"
)

// EncPrefix marks a string as AES-256-GCM encrypted
const EncPrefix = "enc:v1:"

// ErrEncryptedDataMissingKey is returned when the value carries the enc:v1: prefix but no valid key is provided.
var ErrEncryptedDataMissingKey = errors.New("encrypted data found but AES key is not set or has wrong length")

// ErrMissingKey is returned when encryption is requested but the AES key is not set or has wrong length.
// Callers must surface this error instead of silently writing plaintext secrets.
var ErrMissingKey = errors.New("AES key is not set or has wrong length, refusing to handle plaintext")

// EncryptAESGCM encrypts plaintext with AES-256-GCM.
// Returns the original string if empty or already encrypted.
// Returns ErrMissingKey if a non-empty plaintext would otherwise be stored unencrypted.
func EncryptAESGCM(plaintext string, key []byte) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if strings.HasPrefix(plaintext, EncPrefix) {
		return plaintext, nil
	}
	if len(key) != 32 {
		return "", ErrMissingKey
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, aesgcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	ciphertext := aesgcm.Seal(nil, nonce, []byte(plaintext), nil)
	combined := append(nonce, ciphertext...)
	return EncPrefix + base64.RawURLEncoding.EncodeToString(combined), nil
}

// DecryptAESGCM decrypts an AES-256-GCM encrypted string.
// If the string lacks the enc:v1: prefix, it's treated as legacy plaintext and returned as-is.
func DecryptAESGCM(encrypted string, key []byte) (string, error) {
	if encrypted == "" {
		return "", nil
	}
	if !strings.HasPrefix(encrypted, EncPrefix) {
		return encrypted, nil
	}
	if len(key) != 32 {
		return "", ErrEncryptedDataMissingKey
	}

	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(encrypted, EncPrefix))
	if err != nil {
		return "", err
	}
	if len(data) < 28 { // 12-byte nonce + 16-byte auth tag minimum
		return "", errors.New("invalid encrypted data: too short")
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce, ciphertext := data[:aesgcm.NonceSize()], data[aesgcm.NonceSize():]
	plaintext, err := aesgcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}
