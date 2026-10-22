// TASK-P07/P10 上游 knowledge REST 客户端（INT-120/INT-142 共用）。
//
// 只走 knowledge 个人 JWT REST（ADR-006）：桥签发的短时上游 token 作 Bearer，
// base URL 复用 cfg.WeKnora.BaseURL。knowledge 响应统一包 {success, data}，
// 错误包 {success:false, error:{code,message}}（或 AppError 中间件的
// {error:{code,message}} 形态），客户端按状态码 + code 透传映射。
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// upstreamTimeout 桥/上游不可用的判定阈值（P07：15s 超时 → 502 UPSTREAM_UNAVAILABLE）。
// 生产固定 15s；测试可临时改小构造超时场景（包内单测，非并发用例）。
var upstreamTimeout = 15 * time.Second

// errUpstreamUnavailable 表示 knowledge 传输层不可达（超时/连接失败）。
var errUpstreamUnavailable = errors.New("upstream knowledge unavailable")

// upstreamError knowledge 业务错误（带 HTTP 状态与稳定 code，供平台错误映射）。
type upstreamError struct {
	Status  int
	Code    string
	Message string
}

func (e *upstreamError) Error() string {
	return fmt.Sprintf("upstream %d %s: %s", e.Status, e.Code, e.Message)
}

// upstreamResponse knowledge 统一响应信封（data 延迟解析）。
type upstreamResponse struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// knowledgeUpstream knowledge 个人 REST 客户端（零值不可直接用，经 newKnowledgeUpstream 构造）。
type knowledgeUpstream struct {
	baseURL string
	client  *http.Client
}

func newKnowledgeUpstream(baseURL string) *knowledgeUpstream {
	return &knowledgeUpstream{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: upstreamTimeout},
	}
}

// invoke 执行一次 knowledge 调用：解析统一信封，业务错误返回 *upstreamError，
// 传输层错误返回 errUpstreamUnavailable（调用方映射 502）。
func (u *knowledgeUpstream) invoke(ctx context.Context, method, path, bearer string, body io.Reader) (json.RawMessage, error) {
	if u.baseURL == "" {
		return nil, errUpstreamUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, method, u.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUpstreamUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %v", errUpstreamUnavailable, err)
	}
	var envelope upstreamResponse
	if len(raw) > 0 && json.Valid(raw) {
		_ = json.Unmarshal(raw, &envelope)
	}
	if resp.StatusCode >= 400 {
		code, message := "UPSTREAM_ERROR", strings.TrimSpace(string(raw))
		if envelope.Error != nil {
			if envelope.Error.Code != "" {
				code = envelope.Error.Code
			}
			if envelope.Error.Message != "" {
				message = envelope.Error.Message
			}
		}
		return nil, &upstreamError{Status: resp.StatusCode, Code: code, Message: message}
	}
	if !envelope.Success && len(envelope.Data) == 0 {
		// knowledge 个别成功路由不包 success 标记；视为成功原始负载处理。
		return json.RawMessage(raw), nil
	}
	return envelope.Data, nil
}

// invokePassthrough 原样透传（INT-150）：不解析统一信封、不按状态码改写，
// 返回上游状态码与原始响应字节（4xx/5xx 业务错误同样原样返回，由调用方
// 决定透传策略）；传输层错误返回 errUpstreamUnavailable。
// extra 中的键值对附加为请求头（如 task 查询重放的 X-Invocation-Context）。
func (u *knowledgeUpstream) invokePassthrough(ctx context.Context, method, path, bearer string, body io.Reader, extra map[string]string) (int, []byte, error) {
	if u.baseURL == "" {
		return 0, nil, errUpstreamUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, method, u.baseURL+path, body)
	if err != nil {
		return 0, nil, fmt.Errorf("build upstream request: %w", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", errUpstreamUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: read body: %v", errUpstreamUnavailable, err)
	}
	return resp.StatusCode, raw, nil
}

// invokeRaw 与 invoke 相同，但返回完整原始字节（下载/流式转发用）。
func (u *knowledgeUpstream) invokeRaw(ctx context.Context, method, path, bearer string) (io.ReadCloser, http.Header, int, error) {
	if u.baseURL == "" {
		return nil, nil, 0, errUpstreamUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, method, u.baseURL+path, nil)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("build upstream request: %w", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("%w: %v", errUpstreamUnavailable, err)
	}
	if resp.StatusCode >= 400 {
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var envelope upstreamResponse
		if json.Valid(raw) {
			_ = json.Unmarshal(raw, &envelope)
		}
		code, message := "UPSTREAM_ERROR", strings.TrimSpace(string(raw))
		if envelope.Error != nil {
			if envelope.Error.Code != "" {
				code = envelope.Error.Code
			}
			if envelope.Error.Message != "" {
				message = envelope.Error.Message
			}
		}
		return nil, nil, resp.StatusCode, &upstreamError{Status: resp.StatusCode, Code: code, Message: message}
	}
	return resp.Body, resp.Header, resp.StatusCode, nil
}

// ---------- knowledge 检索/内容契约（与 knowledge-opc routes_knowledge.go 对齐） ----------

// upstreamSearchHit hybrid-search 单条结果（types.SearchResult 的 platform 视图）。
type upstreamSearchHit struct {
	ID             string  `json:"id"`
	Content        string  `json:"content"`
	KnowledgeID    string  `json:"knowledge_id"`
	ChunkIndex     int     `json:"chunk_index"`
	KnowledgeTitle string  `json:"knowledge_title"`
	StartAt        int     `json:"start_at"`
	EndAt          int     `json:"end_at"`
	Seq            int     `json:"seq"`
	Score          float64 `json:"score"`
}

// hybridSearch POST /api/v1/knowledge-bases/:id/hybrid-search（Viewer+KBAccessRead）。
func (u *knowledgeUpstream) hybridSearch(ctx context.Context, bearer, kbID string, reqBody map[string]interface{}) ([]upstreamSearchHit, error) {
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	data, err := u.invoke(ctx, http.MethodPost, "/api/v1/knowledge-bases/"+kbID+"/hybrid-search", bearer, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	var hits []upstreamSearchHit
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &hits); err != nil {
			return nil, fmt.Errorf("decode hybrid search results: %w", err)
		}
	}
	return hits, nil
}

// getKnowledge GET /api/v1/knowledge/:id（Viewer 预览级元数据/内容）。
func (u *knowledgeUpstream) getKnowledge(ctx context.Context, bearer, id string) (json.RawMessage, error) {
	return u.invoke(ctx, http.MethodGet, "/api/v1/knowledge/"+id, bearer, nil)
}

// upstreamKB knowledge 知识库视图最小子集（INT-120a 跨库检索的列表项）。
// 解析对字段名宽容：knowledge 列表行既有 id/name 也有
// knowledge_base_id/knowledge_base_name 形态（列表视图带 share 包装）。
type upstreamKB struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (k *upstreamKB) UnmarshalJSON(data []byte) error {
	var wire struct {
		ID                  string `json:"id"`
		Name                string `json:"name"`
		KnowledgeBaseID     string `json:"knowledge_base_id"`
		KnowledgeBaseName   string `json:"knowledge_base_name"`
		Nested              *struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"knowledge_base"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	k.ID = wire.ID
	if k.ID == "" {
		k.ID = wire.KnowledgeBaseID
	}
	k.Name = wire.Name
	if k.Name == "" {
		k.Name = wire.KnowledgeBaseName
	}
	if k.ID == "" && wire.Nested != nil {
		k.ID = wire.Nested.ID
		k.Name = wire.Nested.Name
	}
	return nil
}

// listKnowledgeBases GET /api/v1/knowledge-bases：返回上游身份（兼容桥个人
// token，已绑定目标租户）在该租户内可见的知识库列表。invoke 已拆统一
// 信封，这里直接解析 data 数组。
func (u *knowledgeUpstream) listKnowledgeBases(ctx context.Context, bearer string) ([]upstreamKB, error) {
	data, err := u.invoke(ctx, http.MethodGet, "/api/v1/knowledge-bases", bearer, nil)
	if err != nil {
		return nil, err
	}
	var kbs []upstreamKB
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &kbs); err != nil {
			return nil, fmt.Errorf("decode knowledge base list: %w", err)
		}
	}
	return kbs, nil
}

// downloadKnowledgeFile GET /api/v1/knowledge/:id/download（原文件，knowledge
// Contributor+KBAccessWrite；Viewer 命中 403 由调用方透传 SPACE_FORBIDDEN）。
func (u *knowledgeUpstream) downloadKnowledgeFile(ctx context.Context, bearer, id string) (io.ReadCloser, http.Header, error) {
	body, header, _, err := u.invokeRaw(ctx, http.MethodGet, "/api/v1/knowledge/"+id+"/download", bearer)
	if err != nil {
		return nil, nil, err
	}
	return body, header, nil
}

// ---------- EXT-001 技能 release 契约 ----------

// upstreamRelease knowledge skill_release 视图（INT-140b/c 元数据子集）。
type upstreamRelease struct {
	ID            string          `json:"id"`
	CatalogID     string          `json:"catalog_id"`
	Version       string          `json:"version"`
	BundleSHA256  string          `json:"bundle_sha256"`
	Manifest      json.RawMessage `json:"manifest"`
	TargetTenants []uint64        `json:"target_tenants"`
	TargetRoles   []string        `json:"target_roles"`
	Status        string          `json:"status"`
}

// listSkillReleases GET /api/v1/skills/releases（Viewer+ 按空间过滤）。
func (u *knowledgeUpstream) listSkillReleases(ctx context.Context, bearer string) ([]upstreamRelease, error) {
	data, err := u.invoke(ctx, http.MethodGet, "/api/v1/skills/releases", bearer, nil)
	if err != nil {
		return nil, err
	}
	var releases []upstreamRelease
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &releases); err != nil {
			return nil, fmt.Errorf("decode skill releases: %w", err)
		}
	}
	return releases, nil
}

// getSkillRelease GET /api/v1/skills/releases/:id（Viewer+ 且目标空间成员）。
func (u *knowledgeUpstream) getSkillRelease(ctx context.Context, bearer, id string) (*upstreamRelease, error) {
	data, err := u.invoke(ctx, http.MethodGet, "/api/v1/skills/releases/"+id, bearer, nil)
	if err != nil {
		return nil, err
	}
	var release upstreamRelease
	if err := json.Unmarshal(data, &release); err != nil {
		return nil, fmt.Errorf("decode skill release: %w", err)
	}
	return &release, nil
}

// getSkillReleaseArchive GET /api/v1/skills/releases/:id/archive（流式归档字节，
// 响应带 X-SHA256；403 SKILL_DELISTED / 422 SKILL_HASH_MISMATCH 以 *upstreamError 上抛）。
func (u *knowledgeUpstream) getSkillReleaseArchive(ctx context.Context, bearer, id string) (io.ReadCloser, http.Header, error) {
	body, header, _, err := u.invokeRaw(ctx, http.MethodGet, "/api/v1/skills/releases/"+id+"/archive", bearer)
	if err != nil {
		return nil, nil, err
	}
	return body, header, nil
}
