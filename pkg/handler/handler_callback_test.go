package handler

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	testCallbackToken  = "callback_token_12345"
	testCallbackAESKey = "Tn07bzPoaQk1venTHABXT4DWA77PuFrYyhu6wV72jj2" // 43 字符
	testCallbackCorpID = "ww0190d212b68acac6"
)

// setupCallbackTestHandler 返回的路由与审计库句柄；审计事件写入 platform 状态库（与主库分离）。
func setupCallbackTestHandler(t *testing.T) (*gin.Engine, *gorm.DB) {
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
	if err := stateDB.AutoMigrate(&model.AuditEvent{}); err != nil {
		t.Fatalf("failed to auto migrate: %v", err)
	}

	cfg := &config.Config{
		WeCom: config.WeComConfig{
			CorpID:         testCallbackCorpID,
			Token:          testCallbackToken,
			EncodingAESKey: testCallbackAESKey,
		},
	}
	s := storage.NewStorageWithDBs(db, stateDB)
	h := NewHandler(cfg, nil, nil, nil, nil, s)

	r := gin.New()
	r.POST("/wecom/callback", h.HandleEventCallback)
	return r, stateDB
}

// encryptCallbackMsg 按企微协议加密消息体（AES-CBC + PKCS#7(32) + base64），返回密文
func encryptCallbackMsg(t *testing.T, plaintext string) (aesKey []byte, ciphertext string) {
	t.Helper()
	key, err := base64.StdEncoding.DecodeString(testCallbackAESKey + "=")
	if err != nil {
		t.Fatalf("decode aes key: %v", err)
	}

	randomBytes := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, randomBytes); err != nil {
		t.Fatalf("random: %v", err)
	}
	msgLenBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(msgLenBytes, uint32(len(plaintext)))

	raw := append(randomBytes, msgLenBytes...)
	raw = append(raw, []byte(plaintext)...)
	raw = append(raw, []byte(testCallbackCorpID)...)
	pad := 32 - (len(raw) % 32)
	raw = append(raw, bytes.Repeat([]byte{byte(pad)}, pad)...)

	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	encrypted := make([]byte, len(raw))
	cipher.NewCBCEncrypter(block, key[:16]).CryptBlocks(encrypted, raw)
	return key, base64.StdEncoding.EncodeToString(encrypted)
}

func callbackSignature(params ...string) string {
	sorted := append([]string(nil), params...)
	sort.Strings(sorted)
	h := sha1.New()
	for _, p := range sorted {
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestHandleEventCallback_VerifyAndDecrypt(t *testing.T) {
	r, db := setupCallbackTestHandler(t)

	const timestamp = "1791882515"
	const nonce = "abcdef123456"

	t.Run("valid signature returns success and audits message", func(t *testing.T) {
		_, ciphertext := encryptCallbackMsg(t, `<xml><Event>change_contact</Event></xml>`)
		msgSig := callbackSignature(testCallbackToken, timestamp, nonce, ciphertext)
		body := `<xml><ToUserName><![CDATA[corp]]></ToUserName><Encrypt><![CDATA[` + ciphertext + `]]></Encrypt></xml>`

		req, _ := http.NewRequest(http.MethodPost, "/wecom/callback?msg_signature="+msgSig+"&timestamp="+timestamp+"&nonce="+nonce, bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
		}
		if w.Body.String() != "success" {
			t.Errorf("expected body 'success', got %s", w.Body.String())
		}

		var event model.AuditEvent
		if err := db.Where("action = ?", "wecom.event.received").First(&event).Error; err != nil {
			t.Fatalf("expected wecom.event.received audit row: %v", err)
		}
		if event.Actor != "wecom" || event.Target == "" {
			t.Errorf("unexpected audit event: %+v", event)
		}
	})

	t.Run("bad signature returns 403 and audits denial", func(t *testing.T) {
		_, ciphertext := encryptCallbackMsg(t, `<xml><Event>change_contact</Event></xml>`)
		body := `<xml><Encrypt><![CDATA[` + ciphertext + `]]></Encrypt></xml>`

		req, _ := http.NewRequest(http.MethodPost, "/wecom/callback?msg_signature=forged&timestamp="+timestamp+"&nonce="+nonce, bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d, body: %s", w.Code, w.Body.String())
		}

		var event model.AuditEvent
		if err := db.Where("action = ?", "wecom.callback.denied").Last(&event).Error; err != nil {
			t.Fatalf("expected wecom.callback.denied audit row: %v", err)
		}
		if event.Actor != "wecom:anonymous" {
			t.Errorf("expected actor wecom:anonymous, got %s", event.Actor)
		}
	})

	t.Run("missing encrypt node returns 403", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "/wecom/callback?msg_signature=x&timestamp="+timestamp+"&nonce="+nonce, bytes.NewBufferString(`<xml></xml>`))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", w.Code)
		}
	})

	t.Run("missing token config returns 400", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		cfg := &config.Config{WeCom: config.WeComConfig{CorpID: testCallbackCorpID}}
		h := NewHandler(cfg, nil, nil, nil, nil, nil)
		r2 := gin.New()
		r2.POST("/wecom/callback", h.HandleEventCallback)

		req, _ := http.NewRequest(http.MethodPost, "/wecom/callback", bytes.NewBufferString(`<xml/>`))
		w := httptest.NewRecorder()
		r2.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", w.Code)
		}
	})
}
