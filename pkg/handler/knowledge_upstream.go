// WORK-RESOURCES-001（TASK-009/010/011）上游 knowledge REST 客户端。
//
// 只走 knowledge 个人 JWT REST（ADR-006）：桥签发的短时上游 token 作 Bearer，
// base URL 复用 cfg.WeKnora.BaseURL。knowledge 响应统一包 {success, data}，
// 错误包 {success:false, error:{code,message}}（或 AppError 中间件的
// {error:{code,message}} 形态），客户端按状态码 + code 透传映射。
// 目录契约（/api/v1/skills/catalog*）不查询 release/发布状态（BND-010）。
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
	"sync"
	"time"
)

// upstreamTimeout 桥/上游不可用的判定阈值（P07：15s 超时 → 502 UPSTREAM_UNAVAILABLE）。
// 生产固定 15s；测试可临时改小构造超时场景（包内单测，非并发用例）。
var upstreamTimeout = 15 * time.Second

// 归档下载独立预算（BND-105/INT-105）：总预算 120s、连接/无进度等待 ≤15s，
// 不复用元数据的 15s 超时（避免误杀大包）。测试可临时改小构造超时场景。
var (
	archiveDownloadTotalBudget = 120 * time.Second
	archiveDownloadIdleTimeout = 15 * time.Second
)

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
	// client 元数据/检索用：整体 15s 超时。
	client *http.Client
	// downloadClient 归档下载专用：无整体 Timeout（总预算由调用方 ctx 控），
	// 首包/连接等待 15s；传输层读 idle 由 idleReader 兜底。
	downloadClient *http.Client
}

// newMetadataClient 构造元数据/检索用 HTTP client（15s 整体超时；测试可在
// 装配环境前替换以构造超时场景）。
var newMetadataClient = func() *http.Client {
	return &http.Client{Timeout: upstreamTimeout}
}

// newDownloadClient 构造归档下载专用 client：首包/连接等待
// archiveDownloadIdleTimeout；无整体 Timeout（总预算由调用方 ctx 控制，
// 默认 archiveDownloadTotalBudget）。测试可在装配环境前替换。
var newDownloadClient = func() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			ResponseHeaderTimeout: archiveDownloadIdleTimeout,
		},
	}
}

func newKnowledgeUpstream(baseURL string) *knowledgeUpstream {
	return &knowledgeUpstream{
		baseURL:        strings.TrimRight(baseURL, "/"),
		client:         newMetadataClient(),
		downloadClient: newDownloadClient(),
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
		ID                string `json:"id"`
		Name              string `json:"name"`
		KnowledgeBaseID   string `json:"knowledge_base_id"`
		KnowledgeBaseName string `json:"knowledge_base_name"`
		Nested            *struct {
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

// ---------- WORK-RESOURCES-001 目录契约（INT-101/103，不经过 release） ----------

// upstreamCatalog knowledge /api/v1/skills/catalog 目录视图（INT-101 元数据子集；
// 对应 knowledge service.SkillCatalogView 的 JSON 形态）。
type upstreamCatalog struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Version      string `json:"version"`
	Description  string `json:"description"`
	BundleSHA256 string `json:"bundle_sha256"`
}

// listSkillCatalogs GET /api/v1/skills/catalog（Viewer+ 按空间过滤；只读上传
// 目录，不查 release/published 状态）。
func (u *knowledgeUpstream) listSkillCatalogs(ctx context.Context, bearer string) ([]upstreamCatalog, error) {
	data, err := u.invoke(ctx, http.MethodGet, "/api/v1/skills/catalog", bearer, nil)
	if err != nil {
		return nil, err
	}
	var catalogs []upstreamCatalog
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &catalogs); err != nil {
			return nil, fmt.Errorf("decode skill catalog list: %w", err)
		}
	}
	return catalogs, nil
}

// getSkillCatalog GET /api/v1/skills/catalog/:id（Viewer+ 且当前空间成员）。
func (u *knowledgeUpstream) getSkillCatalog(ctx context.Context, bearer, id string) (*upstreamCatalog, error) {
	data, err := u.invoke(ctx, http.MethodGet, "/api/v1/skills/catalog/"+id, bearer, nil)
	if err != nil {
		return nil, err
	}
	var catalog upstreamCatalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, fmt.Errorf("decode skill catalog: %w", err)
	}
	return &catalog, nil
}

// getCatalogArchive GET /api/v1/skills/catalog/:id/archive?expected_sha256=...
// （INT-103；下载独立预算：总 archiveDownloadTotalBudget、连接/无进度
// archiveDownloadIdleTimeout）。返回流式响应体（需 Close）与响应头；
// 上游业务错误以 *upstreamError 上抛，绝不返回假 ZIP。
func (u *knowledgeUpstream) getCatalogArchive(ctx context.Context, bearer, id, expectedSHA256 string) (io.ReadCloser, http.Header, error) {
	if u.baseURL == "" {
		return nil, nil, errUpstreamUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		u.baseURL+"/api/v1/skills/catalog/"+id+"/archive?expected_sha256="+expectedSHA256, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build upstream request: %w", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := u.downloadClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", errUpstreamUnavailable, err)
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
		return nil, nil, &upstreamError{Status: resp.StatusCode, Code: code, Message: message}
	}
	// 无进度等待 ≤15s：body 包 idleReader，超时时关闭连接使读取失败。
	body := newIdleReader(resp.Body, archiveDownloadIdleTimeout)
	return body, resp.Header, nil
}

// idleReader 包装上游响应体：每次读到字节重置 idle 计时；超时关闭底层连接
// 使 Read 返回错误（BND-105：下载连接/无进度等待不超过 15s）。
type idleReader struct {
	src   io.ReadCloser
	idle  time.Duration
	timer *time.Timer
	ch    chan struct{}
	done  chan struct{}
	once  sync.Once
}

func newIdleReader(src io.ReadCloser, idle time.Duration) *idleReader {
	r := &idleReader{
		src:   src,
		idle:  idle,
		timer: time.NewTimer(idle),
		ch:    make(chan struct{}, 1),
		done:  make(chan struct{}),
	}
	go r.watch()
	return r
}

func (r *idleReader) watch() {
	for {
		select {
		case <-r.timer.C:
			_ = r.src.Close()
			return
		case <-r.ch:
			if !r.timer.Stop() {
				select {
				case <-r.timer.C:
				default:
				}
			}
			r.timer.Reset(r.idle)
		case <-r.done:
			if !r.timer.Stop() {
				select {
				case <-r.timer.C:
				default:
				}
			}
			return
		}
	}
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if n > 0 {
		select {
		case r.ch <- struct{}{}:
		default:
		}
	}
	return n, err
}

func (r *idleReader) Close() error {
	r.once.Do(func() { close(r.done) })
	return r.src.Close()
}
