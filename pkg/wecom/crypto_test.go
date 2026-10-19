package wecom

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"io"
	"testing"
)

func TestWXBizMsgCrypt_VerifyURL(t *testing.T) {
	token := "9747m"
	aesKeyStr := "Tn07bzPoaQk1venTHABXT4DWA77PuFrYyhu6wV72jj2"
	corpID := "ww0190d212b68acac6"

	wxcpt, err := NewWXBizMsgCrypt(token, aesKeyStr, corpID)
	if err != nil {
		t.Fatalf("NewWXBizMsgCrypt failed: %v", err)
	}

	// 模拟构造一个企微加密的 echostr
	testEcho := "hello_wecom_verification_123456"

	// 16 字节随机串
	randomBytes := make([]byte, 16)
	_, _ = io.ReadFull(rand.Reader, randomBytes)

	// 4 字节 msgLen
	msgLenBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(msgLenBytes, uint32(len(testEcho)))

	raw := append(randomBytes, msgLenBytes...)
	raw = append(raw, []byte(testEcho)...)
	raw = append(raw, []byte(corpID)...)
	padded := pkcs7Pad(raw, 32)

	block, err := aes.NewCipher(wxcpt.aesKey)
	if err != nil {
		t.Fatalf("NewCipher error: %v", err)
	}
	iv := wxcpt.aesKey[:16]
	mode := cipher.NewCBCEncrypter(block, iv)
	encrypted := make([]byte, len(padded))
	mode.CryptBlocks(encrypted, padded)

	echostr := base64.StdEncoding.EncodeToString(encrypted)
	timestamp := "1791882515"
	nonce := "12345678"

	// 计算签名
	msgSig := wxcpt.calculateSignature(token, timestamp, nonce, echostr)

	// 验证 VerifyURL
	plain, extractedCorp, err := wxcpt.VerifyURL(msgSig, timestamp, nonce, echostr)
	if err != nil {
		t.Fatalf("VerifyURL failed: %v", err)
	}

	if plain != testEcho {
		t.Fatalf("plain mismatch: got %s, want %s", plain, testEcho)
	}
	if extractedCorp != corpID {
		t.Fatalf("corpID mismatch: got %s, want %s", extractedCorp, corpID)
	}

	t.Logf("VerifyURL successfully decrypted: %s (corpID: %s)", plain, extractedCorp)
}
