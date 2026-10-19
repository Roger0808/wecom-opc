package wecom

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
)

type WXBizMsgCrypt struct {
	token          string
	encodingAESKey string
	corpID         string
	aesKey         []byte
}

func NewWXBizMsgCrypt(token, encodingAESKey, corpID string) (*WXBizMsgCrypt, error) {
	if len(encodingAESKey) != 43 {
		return nil, errors.New("invalid encodingAESKey length, must be 43 chars")
	}

	aesKey, err := base64.StdEncoding.DecodeString(encodingAESKey + "=")
	if err != nil {
		return nil, fmt.Errorf("decode encodingAESKey: %w", err)
	}

	return &WXBizMsgCrypt{
		token:          token,
		encodingAESKey: encodingAESKey,
		corpID:         corpID,
		aesKey:         aesKey,
	}, nil
}

// VerifyURL 企业微信后台配置回调 URL 时的验证接口
// 返回解密后的明文 echostr 以及企微传入的企业 CorpID
func (w *WXBizMsgCrypt) VerifyURL(msgSignature, timestamp, nonce, echostr string) (string, string, error) {
	// 1. 签名验证
	calculatedSig := w.calculateSignature(w.token, timestamp, nonce, echostr)
	if calculatedSig != msgSignature {
		return "", "", fmt.Errorf("signature mismatch: expected %s, got %s", calculatedSig, msgSignature)
	}

	// 2. 解密 echostr
	plainText, corpID, err := w.decrypt(echostr)
	if err != nil {
		return "", "", fmt.Errorf("decrypt echostr: %w", err)
	}

	return string(plainText), corpID, nil
}

// DecryptMsg 解密企微推送的普通消息或事件通知
func (w *WXBizMsgCrypt) DecryptMsg(msgSignature, timestamp, nonce, postData string) ([]byte, string, error) {
	calculatedSig := w.calculateSignature(w.token, timestamp, nonce, postData)
	if calculatedSig != msgSignature {
		return nil, "", fmt.Errorf("signature mismatch")
	}

	return w.decrypt(postData)
}

func (w *WXBizMsgCrypt) calculateSignature(params ...string) string {
	sort.Strings(params)
	h := sha1.New()
	for _, p := range params {
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (w *WXBizMsgCrypt) decrypt(text string) ([]byte, string, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return nil, "", fmt.Errorf("base64 decode: %w", err)
	}

	if len(ciphertext) < aes.BlockSize || len(ciphertext)%aes.BlockSize != 0 {
		return nil, "", errors.New("invalid ciphertext block size")
	}

	block, err := aes.NewCipher(w.aesKey)
	if err != nil {
		return nil, "", err
	}

	iv := w.aesKey[:16]
	mode := cipher.NewCBCDecrypter(block, iv)
	plaintext := make([]byte, len(ciphertext))
	mode.CryptBlocks(plaintext, ciphertext)

	// PKCS#7 去除填充 (企微 block 大小为 32)
	plaintext, err = pkcs7Unpad(plaintext, 32)
	if err != nil {
		return nil, "", err
	}

	// 结构: 16 字节随机字符串 + 4 字节 msgLen (BigEndian) + msg + corpID
	if len(plaintext) < 20 {
		return nil, "", errors.New("plaintext too short")
	}

	msgLen := binary.BigEndian.Uint32(plaintext[16:20])
	totalLen := 20 + int(msgLen)
	if len(plaintext) < totalLen {
		return nil, "", errors.New("plaintext length header invalid")
	}

	msg := plaintext[20:totalLen]
	corpID := string(plaintext[totalLen:])

	return msg, corpID, nil
}

func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("empty data")
	}
	if len(data)%blockSize != 0 {
		return nil, errors.New("invalid block size for unpad")
	}

	pad := int(data[len(data)-1])
	if pad < 1 || pad > blockSize {
		return nil, fmt.Errorf("invalid padding length: %d", pad)
	}

	for i := len(data) - pad; i < len(data); i++ {
		if data[i] != byte(pad) {
			return nil, errors.New("invalid padding bytes")
		}
	}

	return data[:len(data)-pad], nil
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	pad := blockSize - (len(data) % blockSize)
	return append(data, bytes.Repeat([]byte{byte(pad)}, pad)...)
}
