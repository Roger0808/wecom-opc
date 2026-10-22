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
	baseURL    string
	adminToken string
	httpClient *http.Client
}

func NewKnowledgeSyncer(baseURL, adminToken string) *KnowledgeSyncer {
	baseURL = strings.TrimRight(baseURL, "/")
	return &KnowledgeSyncer{
		baseURL:    baseURL,
		adminToken: adminToken,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

type knowledgeResponse struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// PushAndEnableSkill 将 skill.zip 推送到 knowledge-opc 技能池，并在沙盒中安装与启用
func (k *KnowledgeSyncer) PushAndEnableSkill(ctx context.Context, skillZipBytes []byte) (string, error) {
	if k.baseURL == "" || k.adminToken == "" {
		// 未配置 knowledge-opc 时，直接返回模拟成功 ID，避免阻断本地纯内网开发测试
		return "skill_local_mock", nil
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
	req.Header.Set("Authorization", "Bearer "+k.adminToken)

	resp, err := k.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload skill to catalog request: %w", err)
	}
	defer resp.Body.Close()

	var catResp knowledgeResponse
	if err := json.NewDecoder(resp.Body).Decode(&catResp); err != nil {
		return "", fmt.Errorf("decode catalog response: %w", err)
	}
	if catResp.Code != 0 {
		return "", fmt.Errorf("upload skill to catalog failed: code=%d msg=%s", catResp.Code, catResp.Message)
	}

	var catData struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(catResp.Data, &catData)
	catalogSkillID := catData.ID

	// 2. 安装至当前租户/沙盒
	if catalogSkillID != "" {
		installURL := fmt.Sprintf("%s/api/v1/skills/catalog/%s/install", k.baseURL, catalogSkillID)
		instReq, err := http.NewRequestWithContext(ctx, "POST", installURL, nil)
		if err == nil {
			instReq.Header.Set("Authorization", "Bearer "+k.adminToken)
			if instResp, err := k.httpClient.Do(instReq); err == nil {
				defer instResp.Body.Close()
				var instResult knowledgeResponse
				if err := json.NewDecoder(instResp.Body).Decode(&instResult); err == nil && instResult.Code == 0 {
					var instData struct {
						ID string `json:"id"`
					}
					_ = json.Unmarshal(instResult.Data, &instData)
					installedID := instData.ID
					if installedID != "" {
						// 3. 启用技能
						enableURL := fmt.Sprintf("%s/api/v1/skills/%s", k.baseURL, installedID)
						enableBody, _ := json.Marshal(map[string]bool{"enabled": true})
						enableReq, _ := http.NewRequestWithContext(ctx, "PATCH", enableURL, bytes.NewReader(enableBody))
						if enableReq != nil {
							enableReq.Header.Set("Content-Type", "application/json")
							enableReq.Header.Set("Authorization", "Bearer "+k.adminToken)
							if enableResp, err := k.httpClient.Do(enableReq); err == nil {
								_ = enableResp.Body.Close()
							}
						}
					}
				}
			}
		}
	}

	return catalogSkillID, nil
}
