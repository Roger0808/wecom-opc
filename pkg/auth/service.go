package auth

import (
	"context"
	"fmt"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

type AuthService struct {
	cfg     *config.Config
	storage *storage.Storage
}

func NewAuthService(cfg *config.Config, storage *storage.Storage) *AuthService {
	return &AuthService{
		cfg:     cfg,
		storage: storage,
	}
}

// IssueWeKnoraTokens 为指定用户签发与 WeKnora 官方格式完全一致的 JWT Token，并持久化到 auth_tokens 表
func (a *AuthService) IssueWeKnoraTokens(ctx context.Context, user *model.WeKnoraUser, activeTenantID uint64) (string, string, error) {
	if activeTenantID == 0 {
		activeTenantID = user.TenantID
	}
	if activeTenantID == 0 {
		activeTenantID = a.cfg.WeKnora.RootTenantID
	}

	secret := []byte(a.cfg.WeKnora.JWTSecret)

	// 1. 生成 Access Token (24小时)
	accessClaims := jwt.MapClaims{
		"user_id":   user.ID,
		"email":     user.Email,
		"tenant_id": activeTenantID,
		"exp":       time.Now().Add(24 * time.Hour).Unix(),
		"iat":       time.Now().Unix(),
		"type":      "access",
	}
	accessTokenObj := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims)
	accessToken, err := accessTokenObj.SignedString(secret)
	if err != nil {
		return "", "", fmt.Errorf("sign access token: %w", err)
	}

	// 2. 生成 Refresh Token (7天)
	refreshClaims := jwt.MapClaims{
		"user_id": user.ID,
		"exp":     time.Now().Add(7 * 24 * time.Hour).Unix(),
		"iat":     time.Now().Unix(),
		"type":    "refresh",
	}
	refreshTokenObj := jwt.NewWithClaims(jwt.SigningMethodHS256, refreshClaims)
	refreshToken, err := refreshTokenObj.SignedString(secret)
	if err != nil {
		return "", "", fmt.Errorf("sign refresh token: %w", err)
	}

	// 3. 将 Token 持久化进 PostgreSQL 的 auth_tokens 表（WeKnora 鉴权中间件会校验 DB 中该行是否存在）
	accessRecord := &model.WeKnoraAuthToken{
		ID:        uuid.New().String(),
		UserID:    user.ID,
		Token:     accessToken,
		TokenType: "access_token",
		ExpiresAt: time.Now().Add(24 * time.Hour),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := a.storage.CreateAuthToken(ctx, accessRecord); err != nil {
		return "", "", fmt.Errorf("persist access token: %w", err)
	}

	refreshRecord := &model.WeKnoraAuthToken{
		ID:        uuid.New().String(),
		UserID:    user.ID,
		Token:     refreshToken,
		TokenType: "refresh_token",
		ExpiresAt: time.Now().Add(7 * 24 * time.Hour),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := a.storage.CreateAuthToken(ctx, refreshRecord); err != nil {
		return "", "", fmt.Errorf("persist refresh token: %w", err)
	}

	return accessToken, refreshToken, nil
}
