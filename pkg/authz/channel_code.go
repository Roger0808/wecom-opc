package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// CodeInvalidateChannel 把撤权失效事件推送到 code 后端的
// POST /api/internal/enterprise/invalidate（BND-110 推送通道）。
// 认证为共享服务 token；未配置 BaseURL 时通道保持禁用（Register 跳过）。
type CodeInvalidateChannel struct {
	BaseURL      string
	ServiceToken string
	HTTPClient   *http.Client
}

func (c *CodeInvalidateChannel) Name() string { return "code-invalidate" }

func (c *CodeInvalidateChannel) Notify(ctx context.Context, ev InvalidateEvent) error {
	if c.BaseURL == "" {
		return fmt.Errorf("code invalidate channel not configured")
	}
	body, err := json.Marshal(map[string]interface{}{
		"enterprise_subject": ev.Subject,
		"tenant_id":          ev.TenantID,
		"version":            ev.Version,
		"reason":             ev.Reason,
	})
	if err != nil {
		return err
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/internal/enterprise/invalidate", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.ServiceToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.ServiceToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("code invalidate returned %d", resp.StatusCode)
	}
	return nil
}
