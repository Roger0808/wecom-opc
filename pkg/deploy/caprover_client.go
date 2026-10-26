package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"weknora-tokendance/pkg/model"
)

type CapRoverClient struct {
	baseURL       string
	password      string
	rootDomain    string
	httpClient    *http.Client
	PollInterval  time.Duration
	ProbeInterval time.Duration

	mu        sync.RWMutex
	authToken string
}

func NewCapRoverClient(baseURL, password, rootDomain string) *CapRoverClient {
	baseURL = strings.TrimRight(baseURL, "/")
	return &CapRoverClient{
		baseURL:       baseURL,
		password:      password,
		rootDomain:    rootDomain,
		httpClient:    &http.Client{Timeout: 60 * time.Second},
		PollInterval:  2 * time.Second,
		ProbeInterval: 1 * time.Second,
	}
}

type caproverResponse struct {
	Status      int             `json:"status"`
	Description string          `json:"description"`
	Data        json.RawMessage `json:"data"`
}

func (c *CapRoverClient) getToken(ctx context.Context) (string, error) {
	c.mu.RLock()
	token := c.authToken
	c.mu.RUnlock()
	if token != "" {
		return token, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.authToken != "" {
		return c.authToken, nil
	}

	payload, _ := json.Marshal(map[string]string{"password": c.password})
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/v2/login", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("caprover login request failed: %w", err)
	}
	defer resp.Body.Close()

	var result caproverResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode login response: %w", err)
	}
	if result.Status != 100 {
		return "", fmt.Errorf("caprover login failed (status %d): %s", result.Status, result.Description)
	}

	var data struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(result.Data, &data); err != nil {
		return "", fmt.Errorf("parse token data: %w", err)
	}

	c.authToken = data.Token
	return c.authToken, nil
}

// EnsureAppRegistered 确保应用在 CapRover 注册
func (c *CapRoverClient) EnsureAppRegistered(ctx context.Context, slug string) error {
	token, err := c.getToken(ctx)
	if err != nil {
		return err
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"appName":           slug,
		"hasPersistentData": false,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/v2/user/apps/appDefinitions/register", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-captain-auth", token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("register app request: %w", err)
	}
	defer resp.Body.Close()

	var result caproverResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode register response: %w", err)
	}

	// 100 表示成功；若已存在 (description 包含 already exists)，也是合法的幂等结果
	if result.Status != 100 && !strings.Contains(strings.ToLower(result.Description), "already exists") {
		return fmt.Errorf("register app failed (status %d): %s", result.Status, result.Description)
	}
	return nil
}

// UpdateAppConfigAndEnv 注入应用配置与线上资源环境变量（强制内网 HTTP，关闭 SSL）
func (c *CapRoverClient) UpdateAppConfigAndEnv(ctx context.Context, slug string, port int, envVars []model.AppConfigItem) error {
	token, err := c.getToken(ctx)
	if err != nil {
		return err
	}

	type envVar struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	envs := make([]envVar, 0, len(envVars))
	for _, item := range envVars {
		envs = append(envs, envVar{Key: item.Key, Value: item.Value})
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"appName":               slug,
		"instanceCount":         1,
		"containerHttpPort":     port,
		"notExposeAsWebApp":     false,
		"forceSsl":              false,
		"websocketSupport":      true,
		"serviceUpdateOverride": `{"EndpointSpec":{"Mode":"dnsrr"}}`,
		"envVars":               envs,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/v2/user/apps/appDefinitions/update", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-captain-auth", token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("update app definition request: %w", err)
	}
	defer resp.Body.Close()

	var result caproverResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode update response: %w", err)
	}
	if result.Status != 100 {
		return fmt.Errorf("update app definition failed (status %d): %s", result.Status, result.Description)
	}
	return nil
}

// UploadAppTar 上传应用源码包 app.tar 触发构建
func (c *CapRoverClient) UploadAppTar(ctx context.Context, slug string, tarBytes []byte) error {
	token, err := c.getToken(ctx)
	if err != nil {
		return err
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("sourceFile", "app.tar")
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(tarBytes); err != nil {
		return fmt.Errorf("write tar bytes: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close writer: %w", err)
	}

	uploadURL := fmt.Sprintf("%s/api/v2/user/apps/appData/%s/?detached=1", c.baseURL, slug)
	req, err := http.NewRequestWithContext(ctx, "POST", uploadURL, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("x-captain-auth", token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("upload app tar request: %w", err)
	}
	defer resp.Body.Close()

	var result caproverResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode upload response: %w", err)
	}
	// CapRover returns 100 (STATUS_OK) or 101 (STATUS_OK_DEPLOY_STARTED) when detached=1
	if result.Status != 100 && result.Status != 101 {
		return fmt.Errorf("upload app tar failed (status %d): %s", result.Status, result.Description)
	}
	return nil
}

// invalidateToken 清空缓存的 CapRover token，用于 1106 过期重试
func (c *CapRoverClient) invalidateToken() {
	c.mu.Lock()
	c.authToken = ""
	c.mu.Unlock()
}

// RuntimeLogs returns bounded application output for a failed deployment diagnosis.
func (c *CapRoverClient) RuntimeLogs(ctx context.Context, slug string) (string, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/api/v2/user/apps/appData/"+url.PathEscape(slug)+"/logs?encoding=utf8", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("x-captain-auth", token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result caproverResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2*1024*1024)).Decode(&result); err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK || result.Status != 100 {
		return "", fmt.Errorf("runtime log request failed: %d", result.Status)
	}
	var data struct {
		Logs string `json:"logs"`
	}
	if err := json.Unmarshal(result.Data, &data); err != nil {
		return "", err
	}
	if len(data.Logs) > 32768 {
		data.Logs = data.Logs[len(data.Logs)-32768:]
	}
	return data.Logs, nil
}

// isValidHealthzResponse 校验 /healthz 响应体不是 CapRover 默认占位页或未配置的 HTML 页
func isValidHealthzResponse(body []byte) bool {
	s := strings.ToLower(strings.TrimSpace(string(body)))
	if strings.Contains(s, "powered by caprover") ||
		strings.Contains(s, "captain-root-domain") ||
		strings.Contains(s, "ng-version") ||
		strings.HasPrefix(s, "<!doctype html") ||
		strings.HasPrefix(s, "<html") {
		return false
	}
	return true
}

// WaitBuildAndHealthz 轮询构建状态并探测内网健康检查 /healthz
func (c *CapRoverClient) WaitBuildAndHealthz(ctx context.Context, slug, appURL string) (string, error) {
	return c.WaitBuildAndHealthzWithCallback(ctx, slug, appURL, nil)
}

// WaitBuildAndHealthzWithCallback 轮询构建状态并支持实时日志回调
func (c *CapRoverClient) WaitBuildAndHealthzWithCallback(ctx context.Context, slug, appURL string, onLog func(string)) (string, error) {
	return c.WaitBuildConfigureAndHealthz(ctx, slug, appURL, nil, onLog)
}

// WaitBuildConfigureAndHealthz applies approved configuration after the first image exists,
// before probing a service which may require these values just to start.
func (c *CapRoverClient) WaitBuildConfigureAndHealthz(ctx context.Context, slug, appURL string, configure func() error, onLog func(string)) (string, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return "", err
	}

	statusURL := fmt.Sprintf("%s/api/v2/user/apps/appData/%s/", c.baseURL, slug)
	var lastLogs string

	// 1. 轮询构建直至 isAppBuilding == false (最长等待 10 分钟)
	pollInterval := c.PollInterval
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	buildTicker := time.NewTicker(pollInterval)
	defer buildTicker.Stop()
	buildTimeout := time.After(10 * time.Minute)

	buildingDone := false
	for !buildingDone {
		select {
		case <-ctx.Done():
			return lastLogs, ctx.Err()
		case <-buildTimeout:
			return lastLogs, fmt.Errorf("build timeout after 10 minutes")
		case <-buildTicker.C:
			req, err := http.NewRequestWithContext(ctx, "GET", statusURL, nil)
			if err != nil {
				continue
			}
			req.Header.Set("x-captain-auth", token)

			resp, err := c.httpClient.Do(req)
			if err != nil {
				continue
			}

			var result caproverResponse
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				resp.Body.Close()
				continue
			}
			resp.Body.Close()

			if result.Status != 100 {
				// 1106/1105 表示 token 失效，重新登录后继续轮询
				if result.Status == 1106 || result.Status == 1105 {
					c.invalidateToken()
					if newToken, loginErr := c.getToken(ctx); loginErr == nil {
						token = newToken
					}
				}
				continue
			}

			var data struct {
				IsAppBuilding bool `json:"isAppBuilding"`
				IsBuildFailed bool `json:"isBuildFailed"`
				Logs          struct {
					Lines []string `json:"lines"`
				} `json:"logs"`
			}
			_ = json.Unmarshal(result.Data, &data)
			if len(data.Logs.Lines) > 0 {
				lastLogs = strings.Join(data.Logs.Lines, "\n")
				if onLog != nil {
					onLog(lastLogs)
				}
			}

			if !data.IsAppBuilding {
				buildingDone = true
				if data.IsBuildFailed {
					return lastLogs, fmt.Errorf("container build failed on CapRover")
				}
			}
		}
	}

	if configure != nil {
		if err := configure(); err != nil {
			return lastLogs, fmt.Errorf("apply approved configuration after build: %w", err)
		}
	}

	// 2. 探测 /healthz 返回 200 OK (最长探测 90 秒，每次间隔 probeInterval)
	healthURL := strings.TrimRight(appURL, "/") + "/healthz"
	var fallbackURL, fallbackHost string
	if parsedApp, err := url.Parse(healthURL); err == nil && parsedApp.Host != "" {
		fallbackHost = parsedApp.Host
		if parsedCap, err := url.Parse(c.baseURL); err == nil && parsedCap.Hostname() != "" {
			capHost := parsedCap.Hostname()
			if capHost != "127.0.0.1" && capHost != "localhost" {
				fallbackURL = fmt.Sprintf("http://%s:80%s", capHost, parsedApp.RequestURI())
			}
		}
	}

	probeClient := &http.Client{Timeout: 5 * time.Second}
	probeTimeout := time.After(90 * time.Second)
	probeInterval := c.ProbeInterval
	if probeInterval <= 0 {
		probeInterval = 1 * time.Second
	}
	probeTicker := time.NewTicker(probeInterval)
	defer probeTicker.Stop()

	tryProbe := func(targetURL, hostHeader string) bool {
		req, err := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
		if err != nil {
			return false
		}
		if hostHeader != "" {
			req.Host = hostHeader
		}
		resp, err := probeClient.Do(req)
		if err != nil {
			return false
		}
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK && isValidHealthzResponse(bodyBytes)
	}

	for {
		select {
		case <-ctx.Done():
			return lastLogs, ctx.Err()
		case <-probeTimeout:
			return lastLogs, fmt.Errorf("health check timeout: %s did not return valid HTTP 200 within 90s", healthURL)
		case <-probeTicker.C:
			if tryProbe(healthURL, "") || (fallbackURL != "" && tryProbe(fallbackURL, fallbackHost)) {
				lastLogs += fmt.Sprintf("\n[HealthCheck] Healthz probe succeeded: %s returned 200 OK", healthURL)
				return lastLogs, nil
			}
		}
	}
}
