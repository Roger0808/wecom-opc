package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

type KnowledgeSyncer struct {
	baseURL       string
	adminToken    string
	tokenProvider func(ctx context.Context) (string, error)
	httpClient    *http.Client
}

func NewKnowledgeSyncer(baseURL, adminToken string) *KnowledgeSyncer {
	baseURL = strings.TrimRight(baseURL, "/")
	return &KnowledgeSyncer{
		baseURL:    baseURL,
		adminToken: strings.TrimSpace(adminToken),
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// WithTokenProvider 设置动态 Token 提供方（当静态 adminToken 未配置时，使用平台签发的管理员 Token）
func (k *KnowledgeSyncer) WithTokenProvider(fn func(ctx context.Context) (string, error)) *KnowledgeSyncer {
	k.tokenProvider = fn
	return k
}

type knowledgeResponse struct {
	Success *bool           `json:"success"`
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Error   json.RawMessage `json:"error"`
	Data    json.RawMessage `json:"data"`
}

func (k *KnowledgeSyncer) resolveToken(ctx context.Context) (string, error) {
	if k.adminToken != "" {
		return k.adminToken, nil
	}
	if k.tokenProvider != nil {
		tok, err := k.tokenProvider(ctx)
		if err != nil {
			return "", fmt.Errorf("resolve knowledge admin token: %w", err)
		}
		if strings.TrimSpace(tok) != "" {
			return strings.TrimSpace(tok), nil
		}
	}
	return "", fmt.Errorf("knowledge admin token is not configured")
}

// PushAndEnableSkill 将 skill.zip 推送到 knowledge-opc 技能池，并在沙盒中安装与启用
func (k *KnowledgeSyncer) PushAndEnableSkill(ctx context.Context, skillZipBytes []byte) (string, error) {
	if k.baseURL == "" {
		return "", fmt.Errorf("knowledge base url is not configured")
	}
	token, err := k.resolveToken(ctx)
	if err != nil {
		return "", err
	}

	// 1. 上传至技能池 Catalog
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "skill.zip")
	if err != nil {
		return "", fmt.Errorf("create skill form file: %w", err)
	}
	if _, err := part.Write(skillZipBytes); err != nil {
		return "", fmt.Errorf("write skill zip bytes: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close writer: %w", err)
	}

	catalogURL := k.baseURL + "/api/v1/skills/catalog"
	req, err := http.NewRequestWithContext(ctx, "POST", catalogURL, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := k.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload skill to catalog request: %w", err)
	}
	defer resp.Body.Close()

	var catResp knowledgeResponse
	if err := json.NewDecoder(resp.Body).Decode(&catResp); err != nil {
		return "", fmt.Errorf("decode catalog response (http %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errDetail := catResp.Message
		if errDetail == "" && len(catResp.Error) > 0 {
			errDetail = string(catResp.Error)
		}
		return "", fmt.Errorf("upload skill to catalog failed (http %d): %s", resp.StatusCode, errDetail)
	}
	if catResp.Success != nil && !*catResp.Success {
		return "", fmt.Errorf("upload skill to catalog failed: %s", catResp.Message)
	}
	if catResp.Success == nil && catResp.Code != 0 {
		return "", fmt.Errorf("upload skill to catalog failed: code=%d msg=%s", catResp.Code, catResp.Message)
	}

	var catData struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(catResp.Data, &catData)
	catalogSkillID := strings.TrimSpace(catData.ID)
	if catalogSkillID == "" {
		return "", fmt.Errorf("upload skill to catalog returned empty catalog id")
	}

	// 2. 安装至当前租户/沙盒
	installURL := fmt.Sprintf("%s/api/v1/skills/catalog/%s/install", k.baseURL, catalogSkillID)
	instPayload, _ := json.Marshal(map[string]interface{}{
		"sandbox_config_ids": []string{"host"},
	})
	instReq, err := http.NewRequestWithContext(ctx, "POST", installURL, bytes.NewReader(instPayload))
	if err == nil {
		instReq.Header.Set("Content-Type", "application/json")
		instReq.Header.Set("Authorization", "Bearer "+token)
		if instResp, err := k.httpClient.Do(instReq); err == nil {
			defer instResp.Body.Close()
			var instResult knowledgeResponse
			if err := json.NewDecoder(instResp.Body).Decode(&instResult); err == nil {
				var instData struct {
					ID       string            `json:"id"`
					Installs map[string]string `json:"installs"`
				}
				_ = json.Unmarshal(instResult.Data, &instData)
				installedID := instData.ID
				if installedID == "" && len(instData.Installs) > 0 {
					for _, id := range instData.Installs {
						if id != "" {
							installedID = id
							break
						}
					}
				}
				if installedID != "" {
					// 3. 启用技能
					enableURL := fmt.Sprintf("%s/api/v1/skills/%s", k.baseURL, installedID)
					enableBody, _ := json.Marshal(map[string]bool{"enabled": true})
					enableReq, _ := http.NewRequestWithContext(ctx, "PATCH", enableURL, bytes.NewReader(enableBody))
					if enableReq != nil {
						enableReq.Header.Set("Content-Type", "application/json")
						enableReq.Header.Set("Authorization", "Bearer "+token)
						if enableResp, err := k.httpClient.Do(enableReq); err == nil {
							_ = enableResp.Body.Close()
						}
					}
				}
			}
		}
	}

	return catalogSkillID, nil
}
