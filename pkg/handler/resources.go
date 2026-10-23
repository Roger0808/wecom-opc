// WORK-RESOURCES-001（TASK-009/010，REQ-001/005/006/008，ADR-003/005/007）：
// 企业资源只读适配接口组 /integration/v1/resources/*。
//
// 分工：knowledge 是空间/资源权限与归档字节的最终裁定方；platform 按当前
// entitlement 逐空间用个人桥 token 查询并归一化元数据，代理原始归档下载并做
// 哈希一致校验。列表只读当前授权空间（BND-002.4：不得从目录全表读出后靠
// GUI 隐藏无权项）；不查 release/发布状态，不返回存储位置/沙箱配置（BND-010）。
package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/kbridge"
	"weknora-tokendance/pkg/storage"

	"github.com/gin-gonic/gin"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// 资源接口错误码（INT-105 枚举扩展）。
const (
	errCodeResourceNotAccessible = "RESOURCE_NOT_ACCESSIBLE"
	errCodeSkillVersionChanged   = "SKILL_VERSION_CHANGED"
	errCodeArchiveNotReady       = "ARCHIVE_NOT_READY"
	errCodeScopeMismatch         = "SCOPE_MISMATCH"
)

// 资源种类（DATA-101）。
const (
	resourceKindKnowledgeBase = "knowledge_base"
	resourceKindSkillCatalog  = "skill_catalog"
)

// 可用性取值（DATA-101）。
const (
	availabilityAvailable = "available"
	availabilityNotReady  = "not_ready"
)

// 列表/单项查询参数边界（BND-002/DATA-101）。
const (
	resourceQueryMaxRunes     = 200
	resourceIDMaxLen          = 128
	resourceListDefaultLimit  = 50
	resourceListMaxLimit      = 100
	resourceArchiveMaxBytes   = 512 << 20 // BND-140：平台代理收包上限，与 work 安装器一致
	resourceNameFoldMaxLength = 4096      // NFC+fold 名称缓冲上限（防内存放大）
)

// ResourcesHandler /integration/v1/resources 处理器。
type ResourcesHandler struct {
	cfg      *config.Config
	storage  *storage.Storage
	bridge   *kbridge.Bridge
	upstream *knowledgeUpstream
}

// NewResourcesHandler 组装企业资源处理器；bridge 为 P09 兼容桥。
func NewResourcesHandler(cfg *config.Config, s *storage.Storage, bridge *kbridge.Bridge) *ResourcesHandler {
	return &ResourcesHandler{
		cfg:      cfg,
		storage:  s,
		bridge:   bridge,
		upstream: newKnowledgeUpstream(cfg.WeKnora.BaseURL),
	}
}

// resourceItem 规范化资源条目（DATA-101 只读视图；不携带存储位置/沙箱配置）。
type resourceItem struct {
	ResourceKind      string `json:"resource_kind"`
	TenantID          string `json:"tenant_id"`
	TenantName        string `json:"tenant_name"`
	KnowledgeBaseID   string `json:"knowledge_base_id,omitempty"`
	CatalogID         string `json:"catalog_id,omitempty"`
	Name              string `json:"name"`
	Description       string `json:"description"`
	Version           string `json:"version,omitempty"`
	BundleSHA256      string `json:"bundle_sha256,omitempty"`
	Availability      string `json:"availability"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
	DetailURL         string `json:"detail_url,omitempty"`
}

// tenantFailure 原已授权空间的失败标识（BND-002.5：仅限原已授权空间）。
type tenantFailure struct {
	TenantID string `json:"tenant_id"`
	Code     string `json:"code"`
}

// resourceListEnvelope 列表成功信封（INT-101）。
type resourceListEnvelope struct {
	Items    []resourceItem  `json:"items"`
	Total    int             `json:"total"`
	Offset   int             `json:"offset"`
	Limit    int             `json:"limit"`
	Complete bool            `json:"complete"`
	Failures []tenantFailure `json:"failures"`
}

// validResourceID 报告 raw 是否可作单一资源/租户 ID（DATA-101：非空、≤128、
// 无路径分隔符/..、无控制与空白字符）。tenant_id/kb_id/catalog_id 同口径。
func validResourceID(raw string) bool {
	if raw == "" || len(raw) > resourceIDMaxLen || strings.TrimSpace(raw) != raw {
		return false
	}
	if strings.Contains(raw, "..") || strings.ContainsAny(raw, `/\`) {
		return false
	}
	for _, r := range raw {
		if r < 0x21 || r == 0x7f {
			return false
		}
	}
	return true
}

// normalizeExpectedSHA256 校验并把调用方预期的 SHA-256 规范为小写（DATA-101：
// 64 位十六进制；不规范输入由调用方判 400）。
func normalizeExpectedSHA256(raw string) (string, bool) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if len(v) != 64 {
		return "", false
	}
	for _, r := range v {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return "", false
		}
	}
	return v, true
}

// foldName NFC 规范化 + Unicode 大小写折叠后的名称键（BND-002.3 名称匹配口径；
// 超长名称不做匹配（返回 false），既不误命中也不为搜索分配无界缓冲）。
func foldName(s string) (string, bool) {
	if len(s) > resourceNameFoldMaxLength {
		return "", false
	}
	return cases.Fold().String(norm.NFC.String(s)), true
}

// parseListQuery 校验并解析列表查询参数（BND-002：offset 从 0 起，limit
// 缺省 50、范围 1~100，q ≤200 个 Unicode 字符且 trim；tenant_id 可省略）。
func parseListQuery(c *gin.Context) (q, tenantFilter string, offset, limit int, ok bool) {
	q = strings.TrimSpace(c.Query("q"))
	if utf8.RuneCountInString(q) > resourceQueryMaxRunes {
		writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "q exceeds 200 unicode characters")
		return "", "", 0, 0, false
	}
	tenantFilter = strings.TrimSpace(c.Query("tenant_id"))
	if tenantFilter != "" && !validResourceID(tenantFilter) {
		writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "tenant_id is invalid")
		return "", "", 0, 0, false
	}
	offset = 0
	if raw := strings.TrimSpace(c.Query("offset")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "offset must be a non-negative integer")
			return "", "", 0, 0, false
		}
		offset = v
	}
	limit = resourceListDefaultLimit
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 || v > resourceListMaxLimit {
			writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "limit must be within 1..100")
			return "", "", 0, 0, false
		}
		limit = v
	}
	return q, tenantFilter, offset, limit, true
}

// detailURL 构造同受信 origin 的资源详情链接（DATA-101/BND-003）。仅当
// cfg.WeKnora.BaseURL 为合法 https 绝对地址时返回链接；否则返回空（客户端
// 退化为不展示详情入口），绝不拼接存储路径或 http 明文地址。
func (h *ResourcesHandler) detailURL(kind, id string) string {
	base := strings.TrimSpace(h.cfg.WeKnora.BaseURL)
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return ""
	}
	if kind == resourceKindKnowledgeBase {
		// u.Path 持有解码形态；RawPath 提供 %2F 编码，保证 ID 始终是单一路径段
		//（BND-003：ID 使用 URL 编码，不拼接存储路径）。
		dir := strings.TrimRight(u.Path, "/") + "/platform/knowledge-bases/"
		u.Path = dir + id
		u.RawPath = dir + url.PathEscape(id)
		u.RawQuery = ""
		return u.String()
	}
	if kind == resourceKindSkillCatalog {
		u.Path = strings.TrimRight(u.Path, "/") + "/platform/toolbox/skills"
		q := u.Query()
		q.Set("catalog_id", id)
		u.RawQuery = q.Encode()
		return u.String()
	}
	return ""
}

// bridgeFailureCode 把桥拒签错误映射为失败标识码（不写信封，供列表聚合）。
func bridgeFailureCode(err error) string {
	switch {
	case errors.Is(err, kbridge.ErrEntitlementMissing), errors.Is(err, kbridge.ErrAdminTokenRefused):
		return errCodeSpaceForbidden
	case errors.Is(err, kbridge.ErrIdentityDisabled),
		errors.Is(err, kbridge.ErrUpstreamUserMissing),
		errors.Is(err, kbridge.ErrIdentityUnknown):
		return errCodeSubjectNotProv
	default:
		return errCodeUpstreamUnavailable
	}
}

// queryTenantResources 用个人桥 token 查一个授权空间的指定资源集合；桥拒签与
// 上游错误原样上抛（列表聚合方按 complete/failures 语义处理）。
func (h *ResourcesHandler) queryTenantResources(ctx context.Context, linkID, tenant, kind string) ([]resourceItem, error) {
	token, err := h.bridge.IssueUpstreamToken(ctx, linkID, tenant, bridgeTokenTTL)
	if err != nil {
		return nil, err
	}
	tenantName := h.tenantDisplayName(ctx, tenant)
	if kind == resourceKindKnowledgeBase {
		kbs, err := h.upstream.listKnowledgeBases(ctx, token)
		if err != nil {
			return nil, err
		}
		items := make([]resourceItem, 0, len(kbs))
		for _, kb := range kbs {
			if kb.ID == "" {
				continue
			}
			items = append(items, resourceItem{
				ResourceKind:    resourceKindKnowledgeBase,
				TenantID:        tenant,
				TenantName:      tenantName,
				KnowledgeBaseID: kb.ID,
				Name:            kb.Name,
				Availability:    availabilityAvailable,
				DetailURL:       h.detailURL(resourceKindKnowledgeBase, kb.ID),
			})
		}
		return items, nil
	}
	catalogs, err := h.upstream.listSkillCatalogs(ctx, token)
	if err != nil {
		return nil, err
	}
	items := make([]resourceItem, 0, len(catalogs))
	for _, cat := range catalogs {
		if cat.ID == "" {
			continue
		}
		item := resourceItem{
			ResourceKind: resourceKindSkillCatalog,
			TenantID:     tenant,
			TenantName:   tenantName,
			CatalogID:    cat.ID,
			Name:         cat.Name,
			Description:  cat.Description,
			Version:      cat.Version,
			BundleSHA256: strings.ToLower(cat.BundleSHA256),
			Availability: availabilityAvailable,
			DetailURL:    h.detailURL(resourceKindSkillCatalog, cat.ID),
		}
		// 归档未知（SHA 缺失）→ not_ready + ARCHIVE_NOT_READY（DATA-101/INT-101：
		// 可显示但安装禁用，不虚构已完成状态）。
		if item.BundleSHA256 == "" {
			item.Availability = availabilityNotReady
			item.UnavailableReason = errCodeArchiveNotReady
		}
		items = append(items, item)
	}
	return items, nil
}

// tenantDisplayName 解析空间显示名（best-effort；缺失/失败回退为 ID，
// DATA-101：名称缺失显示 ID）。
func (h *ResourcesHandler) tenantDisplayName(ctx context.Context, tenantID string) string {
	numeric, err := strconv.ParseUint(tenantID, 10, 64)
	if err != nil {
		return tenantID
	}
	names, err := h.storage.ListWeKnoraTenantNames(ctx, []uint64{numeric})
	if err != nil {
		return tenantID
	}
	if name := names[numeric]; name != "" {
		return name
	}
	return tenantID
}

// filterSortPage 按 BND-002 统一筛选/排序/分页：名称 NFC+大小写无关包含匹配、
// 默认按 tenant_id、名称、资源 ID 升序稳定排序，offset/limit 切片。
// total 是成功读取集合中匹配筛选的条目数。
func filterSortPage(items []resourceItem, q, tenantFilter string, offset, limit int) resourceListEnvelope {
	if tenantFilter != "" {
		kept := items[:0]
		for _, it := range items {
			if it.TenantID == tenantFilter {
				kept = append(kept, it)
			}
		}
		items = kept
	}
	if q != "" {
		folded, ok := foldName(q)
		if ok {
			kept := items[:0]
			for _, it := range items {
				name, ok := foldName(it.Name)
				if ok && strings.Contains(name, folded) {
					kept = append(kept, it)
				}
			}
			items = kept
		} else {
			// 超长查询词不做名称匹配：结果为空（不误命中）。
			items = nil
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].TenantID != items[j].TenantID {
			return items[i].TenantID < items[j].TenantID
		}
		ni, oki := foldName(items[i].Name)
		nj, okj := foldName(items[j].Name)
		if oki && okj && ni != nj {
			return ni < nj
		}
		return resourceItemID(items[i]) < resourceItemID(items[j])
	})
	total := len(items)
	if offset > len(items) {
		items = nil
	} else {
		items = items[offset:]
	}
	if len(items) > limit {
		items = items[:limit]
	}
	if items == nil {
		items = []resourceItem{}
	}
	return resourceListEnvelope{
		Items:    items,
		Total:    total,
		Offset:   offset,
		Limit:    limit,
		Complete: true,
		Failures: []tenantFailure{},
	}
}

// resourceItemID 返回条目的稳定资源 ID（排序与复合键去重用）。
func resourceItemID(it resourceItem) string {
	if it.CatalogID != "" {
		return it.CatalogID
	}
	return it.KnowledgeBaseID
}

// listResources 逐授权空间查询并聚合（BND-002.4/5）：全部成功 complete=true；
// 部分空间失败 complete=false 且 failures 只含失败的原授权空间；全部失败返回
// 502 错误，不以空列表吞失败。显式 tenant_id 未授权 → 403（不返回该空间信息）。
func (h *ResourcesHandler) listResources(c *gin.Context, kind string) {
	ctx := c.Request.Context()
	sess, ok := resolveAccessSession(h.cfg, h.storage, c)
	if !ok {
		return
	}
	q, tenantFilter, offset, limit, ok := parseListQuery(c)
	if !ok {
		return
	}
	tenants, err := entitledTenants(ctx, h.storage, sess.link.ImmutableSubject)
	if err != nil {
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "list entitlements failed")
		return
	}
	if tenantFilter != "" {
		allowed := false
		for _, t := range tenants {
			if t == tenantFilter {
				allowed = true
				break
			}
		}
		if !allowed {
			writeErrorEnvelope(c, http.StatusForbidden, errCodeSpaceForbidden, "tenant not in active entitlement set")
			return
		}
		tenants = []string{tenantFilter}
	}

	failures := make([]tenantFailure, 0)
	items := make([]resourceItem, 0)
	for _, tenant := range tenants {
		tenantItems, err := h.queryTenantResources(ctx, sess.link.ID, tenant, kind)
		if err != nil {
			var uerr *upstreamError
			code := errCodeUpstreamUnavailable
			switch {
			case errors.As(err, &uerr):
				code = uerr.Code
			case errors.Is(err, errUpstreamUnavailable):
				code = errCodeUpstreamUnavailable
			default:
				code = bridgeFailureCode(err)
			}
			failures = append(failures, tenantFailure{TenantID: tenant, Code: code})
			continue
		}
		items = append(items, tenantItems...)
	}
	if len(tenants) > 0 && len(failures) == len(tenants) {
		// 全部空间失败：返回错误，不伪装成空列表（BND-002.5）。
		writeErrorEnvelope(c, http.StatusBadGateway, errCodeUpstreamUnavailable, "all entitled spaces failed to load")
		return
	}

	env := filterSortPage(items, q, "", offset, limit)
	env.Complete = len(failures) == 0
	env.Failures = failures

	auditEvent(ctx, h.storage, sess.link.ImmutableSubject, "integration.resources.listed", "GET "+c.FullPath(), integrationRequestID(c), map[string]interface{}{
		"result":   "allowed",
		"kind":     kind,
		"tenants":  len(tenants),
		"items":    len(env.Items),
		"total":    env.Total,
		"complete": env.Complete,
		"failures": len(failures),
	})

	c.JSON(http.StatusOK, env)
}

// ListKnowledgeBases GET /integration/v1/resources/knowledge-bases（INT-101：
// 只读已授权空间及 KB）。
func (h *ResourcesHandler) ListKnowledgeBases(c *gin.Context) {
	h.listResources(c, resourceKindKnowledgeBase)
}

// ListSkillCatalogs GET /integration/v1/resources/skills（INT-101：只读上传
// 目录，不查 release/published）。
func (h *ResourcesHandler) ListSkillCatalogs(c *gin.Context) {
	h.listResources(c, resourceKindSkillCatalog)
}

// resolveTenantParam 解析并校验单项接口的 tenant_id 查询参数（DATA-101）：
// 必填、单一安全 ID；显式无权空间 → 403（不返回该空间信息）。
func resolveTenantParam(c *gin.Context) (string, bool) {
	tenant := strings.TrimSpace(c.Query("tenant_id"))
	if tenant == "" {
		writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "tenant_id query parameter is required")
		return "", false
	}
	if !validResourceID(tenant) {
		writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "tenant_id is invalid")
		return "", false
	}
	return tenant, true
}

// GetSkillCatalog GET /integration/v1/resources/skills/:catalog_id（INT-101：
// 单项授权与当前 hash，用于执行前检查；不存在或不可见统一
// RESOURCE_NOT_ACCESSIBLE，与请求失败区分）。
func (h *ResourcesHandler) GetSkillCatalog(c *gin.Context) {
	ctx := c.Request.Context()
	sess, ok := resolveAccessSession(h.cfg, h.storage, c)
	if !ok {
		return
	}
	catalogID := c.Param("catalog_id")
	if !validResourceID(catalogID) {
		writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "catalog_id is invalid")
		return
	}
	tenant, ok := resolveTenantParam(c)
	if !ok {
		return
	}

	tenants, err := entitledTenants(ctx, h.storage, sess.link.ImmutableSubject)
	if err != nil {
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "list entitlements failed")
		return
	}
	allowed := false
	for _, t := range tenants {
		if t == tenant {
			allowed = true
			break
		}
	}
	if !allowed {
		writeErrorEnvelope(c, http.StatusForbidden, errCodeSpaceForbidden, "tenant not in active entitlement set")
		return
	}

	token, err := h.bridge.IssueUpstreamToken(ctx, sess.link.ID, tenant, bridgeTokenTTL)
	if err != nil {
		writeErrorEnvelope(c, http.StatusForbidden, bridgeFailureCode(err), "upstream token issue failed")
		return
	}
	catalog, err := h.upstream.getSkillCatalog(ctx, token, catalogID)
	if err != nil {
		var uerr *upstreamError
		if errors.As(err, &uerr) && (uerr.Status == http.StatusForbidden || uerr.Status == http.StatusNotFound) {
			// 目录不存在/不可见统一 404（DATA-105：避免枚举；同名其他空间不替代）。
			writeErrorEnvelope(c, http.StatusNotFound, errCodeResourceNotAccessible, "skill catalog is not accessible")
			return
		}
		denyUpstream(c, err)
		return
	}

	item := resourceItem{
		ResourceKind: resourceKindSkillCatalog,
		TenantID:     tenant,
		TenantName:   h.tenantDisplayName(ctx, tenant),
		CatalogID:    catalog.ID,
		Name:         catalog.Name,
		Description:  catalog.Description,
		Version:      catalog.Version,
		BundleSHA256: strings.ToLower(catalog.BundleSHA256),
		Availability: availabilityAvailable,
		DetailURL:    h.detailURL(resourceKindSkillCatalog, catalog.ID),
	}
	if item.BundleSHA256 == "" {
		item.Availability = availabilityNotReady
		item.UnavailableReason = errCodeArchiveNotReady
	}

	auditEvent(ctx, h.storage, sess.link.ImmutableSubject, "integration.resources.skill.viewed", "GET "+c.FullPath(), integrationRequestID(c), map[string]interface{}{
		"result":     "allowed",
		"tenant_id":  tenant,
		"catalog_id": catalogID,
		"available":  item.Availability == availabilityAvailable,
	})

	c.JSON(http.StatusOK, gin.H{"item": item})
}

// mapArchiveUpstreamError 归档下载上游错误 → INT-105 映射（版本冲突/未就绪/
// 无权按规格映射；字节 hash 不一致也在下载侧以 422 上抛）。
func mapArchiveUpstreamError(c *gin.Context, err error) {
	var uerr *upstreamError
	switch {
	case errors.Is(err, errUpstreamUnavailable):
		writeErrorEnvelope(c, http.StatusBadGateway, errCodeUpstreamUnavailable, "knowledge upstream unavailable")
	case errors.As(err, &uerr):
		switch uerr.Code {
		case errCodeSkillVersionChanged:
			writeErrorEnvelope(c, http.StatusConflict, errCodeSkillVersionChanged, "the skill catalog has a different version now; refresh and confirm again")
		case errCodeArchiveNotReady:
			writeErrorEnvelope(c, http.StatusConflict, errCodeArchiveNotReady, "the archive of this skill catalog is not ready yet")
		case errCodeSkillHashMismatch:
			writeErrorEnvelope(c, http.StatusUnprocessableEntity, errCodeSkillHashMismatch, "the stored archive does not match the catalog digest")
		case errCodeResourceNotAccessible:
			writeErrorEnvelope(c, http.StatusNotFound, errCodeResourceNotAccessible, "skill catalog is not accessible")
		case errCodeSpaceForbidden:
			writeErrorEnvelope(c, http.StatusForbidden, errCodeSpaceForbidden, uerr.Message)
		case errCodeInvalidRequest:
			writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, uerr.Message)
		default:
			if uerr.Status == http.StatusNotFound {
				writeErrorEnvelope(c, http.StatusNotFound, errCodeResourceNotAccessible, "skill catalog is not accessible")
			} else if uerr.Status == http.StatusForbidden {
				writeErrorEnvelope(c, http.StatusForbidden, errCodeSpaceForbidden, uerr.Message)
			} else {
				writeErrorEnvelope(c, http.StatusBadGateway, errCodeUpstreamUnavailable, "knowledge upstream error")
			}
		}
	default:
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "unexpected upstream error")
	}
}

// DownloadSkillCatalogArchive GET /integration/v1/resources/skills/:catalog_id/archive
// （INT-103）：携个人身份调 knowledge 新 catalog archive 路由并转发 expected SHA；
// 完整收齐字节后做三向哈希一致校验（请求预期、上游 X-SHA256、实际字节），
// 任一不符 422 SKILL_HASH_MISMATCH；流中断/超限不返回假 ZIP（BND-006/INT-105）。
// 下载走独立预算（总 120s、连接/无进度 15s），不复用元数据 15s 超时。
func (h *ResourcesHandler) DownloadSkillCatalogArchive(c *gin.Context) {
	ctx := c.Request.Context()
	sess, ok := resolveAccessSession(h.cfg, h.storage, c)
	if !ok {
		return
	}
	catalogID := c.Param("catalog_id")
	if !validResourceID(catalogID) {
		writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "catalog_id is invalid")
		return
	}
	tenant, ok := resolveTenantParam(c)
	if !ok {
		return
	}
	expectedSHA, ok := normalizeExpectedSHA256(c.Query("expected_sha256"))
	if !ok {
		writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "expected_sha256 must be a 64-character hexadecimal SHA-256")
		return
	}

	tenants, err := entitledTenants(ctx, h.storage, sess.link.ImmutableSubject)
	if err != nil {
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "list entitlements failed")
		return
	}
	allowed := false
	for _, t := range tenants {
		if t == tenant {
			allowed = true
			break
		}
	}
	if !allowed {
		writeErrorEnvelope(c, http.StatusForbidden, errCodeSpaceForbidden, "tenant not in active entitlement set")
		return
	}

	token, err := h.bridge.IssueUpstreamToken(ctx, sess.link.ID, tenant, bridgeTokenTTL)
	if err != nil {
		writeErrorEnvelope(c, http.StatusForbidden, bridgeFailureCode(err), "upstream token issue failed")
		return
	}

	// 独立下载预算：总 120s（首包/连接等待由 downloadClient 的
	// ResponseHeaderTimeout 与 idleReader 控制在 15s 内）。
	dlCtx, cancel := context.WithTimeout(ctx, archiveDownloadTotalBudget)
	defer cancel()
	body, header, err := h.upstream.getCatalogArchive(dlCtx, token, catalogID, expectedSHA)
	if err != nil {
		mapArchiveUpstreamError(c, err)
		return
	}
	defer func() { _ = body.Close() }()

	archive, err := io.ReadAll(io.LimitReader(body, resourceArchiveMaxBytes+1))
	if err != nil {
		// 流中断/超时：失败处理，绝不返回半个 ZIP。
		auditEvent(ctx, h.storage, sess.link.ImmutableSubject, "integration.resources.skill.archive_denied", "GET "+c.FullPath(), integrationRequestID(c), map[string]interface{}{
			"result":     "denied",
			"tenant_id":  tenant,
			"catalog_id": catalogID,
			"error":      errCodeUpstreamUnavailable,
		})
		writeErrorEnvelope(c, http.StatusBadGateway, errCodeUpstreamUnavailable, "upstream archive stream interrupted")
		return
	}
	if len(archive) > resourceArchiveMaxBytes {
		auditEvent(ctx, h.storage, sess.link.ImmutableSubject, "integration.resources.skill.archive_denied", "GET "+c.FullPath(), integrationRequestID(c), map[string]interface{}{
			"result":     "denied",
			"tenant_id":  tenant,
			"catalog_id": catalogID,
			"error":      errCodeUpstreamUnavailable,
		})
		writeErrorEnvelope(c, http.StatusBadGateway, errCodeUpstreamUnavailable, "upstream archive exceeds size limit")
		return
	}

	// 三向哈希一致校验：实际字节 vs 请求预期 vs 上游 X-SHA256。
	sum := sha256.Sum256(archive)
	gotHash := hex.EncodeToString(sum[:])
	headerHash := strings.ToLower(strings.TrimSpace(header.Get("X-SHA256")))
	if gotHash != expectedSHA || (headerHash != "" && headerHash != expectedSHA) {
		auditEvent(ctx, h.storage, sess.link.ImmutableSubject, "integration.resources.skill.archive_denied", "GET "+c.FullPath(), integrationRequestID(c), map[string]interface{}{
			"result":     "denied",
			"tenant_id":  tenant,
			"catalog_id": catalogID,
			"error":      errCodeSkillHashMismatch,
			"got_hash":   gotHash,
			"want_hash":  expectedSHA,
		})
		writeErrorEnvelope(c, http.StatusUnprocessableEntity, errCodeSkillHashMismatch, "archive bytes do not match the expected digest")
		return
	}

	auditEvent(ctx, h.storage, sess.link.ImmutableSubject, "integration.resources.skill.archive_downloaded", "GET "+c.FullPath(), integrationRequestID(c), map[string]interface{}{
		"result":     "allowed",
		"tenant_id":  tenant,
		"catalog_id": catalogID,
		"bytes":      len(archive),
		"sha256":     gotHash,
	})

	c.Header("Content-Type", "application/zip")
	c.Header("X-SHA256", gotHash)
	if disposition := header.Get("Content-Disposition"); disposition != "" {
		c.Header("Content-Disposition", disposition)
	}
	c.Header("Content-Length", strconv.Itoa(len(archive)))
	c.Status(http.StatusOK)
	_, _ = c.Writer.Write(archive)
}
