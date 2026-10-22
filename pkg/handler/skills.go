// TASK-P10（INT-141/142，REQ-007，ADR-007，DATA-120b，EXT-001）：技能发布协调。
//
// 分工：knowledge 是 catalog/发布内容/分发权限的裁定方（INT-140）；
// platform 只协调与记账——发布任务进度、幂等键与逐目标结果落
// skill_publish_jobs，客户端列表/下载按租户 entitlement 逐空间查 knowledge
// release 并规范化转发，字节与 hash 以 knowledge 为权威。
package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/adminauth"
	"weknora-tokendance/pkg/kbridge"
	"weknora-tokendance/pkg/model"
	"weknora-tokendance/pkg/storage"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// skillArchiveMaxBytes 技能归档下载缓冲上限（BND-140 总包 ≤512MiB；平台先完整
// 收齐再校验 hash，避免把不一致字节先发出去）。
const skillArchiveMaxBytes = 512 << 20

// publishJobStep 逐目标可见性校验结果（落 skill_publish_jobs.steps）。
type publishJobStep struct {
	Tenant string `json:"tenant"`
	Status string `json:"status"` // validated / failed
	Error  string `json:"error,omitempty"`
}

// publishJobStatus 聚合状态：全部通过 succeeded / 部分失败 partial_failed / 全失败 failed。
const (
	jobStatusSucceeded     = "succeeded"
	jobStatusPartialFailed = "partial_failed"
	jobStatusFailed        = "failed"
)

// SkillsHandler /integration/v1/skills 与 /integration/v1/admin/skills 处理器。
type SkillsHandler struct {
	cfg      *config.Config
	storage  *storage.Storage
	bridge   *kbridge.Bridge
	upstream *knowledgeUpstream
}

// NewSkillsHandler 组装技能协调处理器；bridge 为 P09 兼容桥。
func NewSkillsHandler(cfg *config.Config, s *storage.Storage, bridge *kbridge.Bridge) *SkillsHandler {
	return &SkillsHandler{
		cfg:      cfg,
		storage:  s,
		bridge:   bridge,
		upstream: newKnowledgeUpstream(cfg.WeKnora.BaseURL),
	}
}

// ---------- INT-142a GET /integration/v1/skills ----------

// listSkillEntry 客户端可见企业技能条目（INT-142：publish_id/version/manifest/hash）。
type listSkillEntry struct {
	PublishID    string          `json:"publish_id"`
	Version      string          `json:"version"`
	Manifest     json.RawMessage `json:"manifest"`
	Hash         string          `json:"hash"`
	SourceTenant string          `json:"source_tenant"`
}

// errSkillBridgeDenied 标记共用助手内桥拒签已写错误信封（c.IsAborted()），
// 调用方只需终止处理，不得再写信封。
var errSkillBridgeDenied = errors.New("handler: skill bridge token denied")

// queryPublishedSkillReleases 逐授权租户经桥查 knowledge release 并归一化：
// 只收 published 状态，按 publish_id 去重（首个可见租户作为 source_tenant，
// tenants 升序保证确定性），按 publish_id 升序输出。桥拒签已写错误信封并
// 返回 errSkillBridgeDenied；上游错误原样上抛由调用方映射。
func (h *SkillsHandler) queryPublishedSkillReleases(ctx context.Context, c *gin.Context, link *model.IdentityLink, tenants []string) ([]listSkillEntry, error) {
	seen := make(map[string]struct{})
	skills := make([]listSkillEntry, 0)
	for _, tenant := range tenants {
		token, ok := issueBridgeToken(c, h.bridge, link, tenant, bridgeTokenTTL)
		if !ok {
			return nil, errSkillBridgeDenied
		}
		releases, err := h.upstream.listSkillReleases(ctx, token)
		if err != nil {
			return nil, err
		}
		for _, release := range releases {
			if release.Status != "published" {
				continue
			}
			if _, dup := seen[release.ID]; dup {
				continue
			}
			seen[release.ID] = struct{}{}
			skills = append(skills, listSkillEntry{
				PublishID:    release.ID,
				Version:      release.Version,
				Manifest:     release.Manifest,
				Hash:         release.BundleSHA256,
				SourceTenant: tenant,
			})
		}
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].PublishID < skills[j].PublishID })
	return skills, nil
}

// List 当前用户可见企业技能列表：按 active entitlement 租户逐个经桥查
// knowledge releases（Viewer+ 按空间过滤），只收 published 状态，按 publish_id 去重。
func (h *SkillsHandler) List(c *gin.Context) {
	ctx := c.Request.Context()
	sess, ok := resolveAccessSession(h.cfg, h.storage, c)
	if !ok {
		return
	}
	tenants, err := entitledTenants(ctx, h.storage, sess.link.ImmutableSubject)
	if err != nil {
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "list entitlements failed")
		return
	}

	skills, err := h.queryPublishedSkillReleases(ctx, c, sess.link, tenants)
	if err != nil {
		if !c.IsAborted() {
			denyUpstream(c, err)
		}
		return
	}

	auditEvent(ctx, h.storage, sess.link.ImmutableSubject, "integration.skills.listed", "GET "+c.FullPath(), integrationRequestID(c), map[string]interface{}{
		"result":  "allowed",
		"tenants": len(tenants),
		"skills":  len(skills),
	})

	c.JSON(http.StatusOK, gin.H{"skills": skills})
}

// ---------- INT-142b GET /integration/v1/skills/:publish_id/download ----------

// Download 企业技能归档下载：校验 entitlement（逐授权空间查 release 元数据，
// 首个可见空间为准）→ 桥 token 调 knowledge /archive → 完整收齐字节并校验
// sha256 后带 X-SHA256 头转发；hash 不符 422 SKILL_HASH_MISMATCH；
// knowledge 403 SKILL_DELISTED 透传。
func (h *SkillsHandler) Download(c *gin.Context) {
	ctx := c.Request.Context()
	sess, ok := resolveAccessSession(h.cfg, h.storage, c)
	if !ok {
		return
	}
	publishID := c.Param("publish_id")

	tenants, err := entitledTenants(ctx, h.storage, sess.link.ImmutableSubject)
	if err != nil {
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "list entitlements failed")
		return
	}

	// 定位首个可见该 release 的授权空间，并取权威 hash。
	var release *upstreamRelease
	tenant := ""
	for _, t := range tenants {
		token, ok := issueBridgeToken(c, h.bridge, sess.link, t, bridgeTokenTTL)
		if !ok {
			return
		}
		rel, err := h.upstream.getSkillRelease(ctx, token, publishID)
		if err != nil {
			var uerr *upstreamError
			if errors.As(err, &uerr) && (uerr.Status == http.StatusForbidden || uerr.Status == http.StatusNotFound) {
				continue
			}
			denyUpstream(c, err)
			return
		}
		release = rel
		tenant = t
		break
	}
	if release == nil {
		writeErrorEnvelope(c, http.StatusForbidden, errCodeSpaceForbidden, "release not visible in any entitled tenant")
		return
	}
	expectedHash := strings.ToLower(release.BundleSHA256)

	token, ok := issueBridgeToken(c, h.bridge, sess.link, tenant, bridgeTokenTTL)
	if !ok {
		return
	}
	body, header, err := h.upstream.getSkillReleaseArchive(ctx, token, publishID)
	if err != nil {
		denyUpstream(c, err) // 403 SKILL_DELISTED 等按 code 透传
		return
	}
	defer func() { _ = body.Close() }()

	archive, err := io.ReadAll(io.LimitReader(body, skillArchiveMaxBytes+1))
	if err != nil {
		writeErrorEnvelope(c, http.StatusBadGateway, errCodeUpstreamUnavailable, "read upstream archive failed")
		return
	}
	if len(archive) > skillArchiveMaxBytes {
		writeErrorEnvelope(c, http.StatusBadGateway, errCodeUpstreamUnavailable, "upstream archive exceeds size limit")
		return
	}

	sum := sha256.Sum256(archive)
	gotHash := hex.EncodeToString(sum[:])
	// hash 以 release 元数据为准；元数据缺省时回退 knowledge 响应头 X-SHA256。
	if expectedHash == "" {
		expectedHash = strings.ToLower(header.Get("X-SHA256"))
	}
	if expectedHash == "" || gotHash != expectedHash {
		auditEvent(ctx, h.storage, sess.link.ImmutableSubject, "integration.skills.download.denied", "GET "+c.FullPath(), integrationRequestID(c), map[string]interface{}{
			"result":     "denied",
			"publish_id": publishID,
			"error":      errCodeSkillHashMismatch,
			"got_hash":   gotHash,
			"want_hash":  expectedHash,
		})
		writeErrorEnvelope(c, http.StatusUnprocessableEntity, errCodeSkillHashMismatch, "archive bytes do not match release hash")
		return
	}

	auditEvent(ctx, h.storage, sess.link.ImmutableSubject, "integration.skills.downloaded", "GET "+c.FullPath(), integrationRequestID(c), map[string]interface{}{
		"result":        "allowed",
		"publish_id":    publishID,
		"source_tenant": tenant,
		"bytes":         len(archive),
		"sha256":        gotHash,
	})

	c.Header("Content-Type", "application/zip")
	c.Header("X-SHA256", gotHash)
	c.Header("Content-Length", strconv.Itoa(len(archive)))
	c.Status(http.StatusOK)
	_, _ = c.Writer.Write(archive)
}

// ---------- INT-160 GET /integration/v1/internal/skills ----------

// InternalList 服务 token 版企业技能清单（供 code 后端 claim 发放 enterprise
// SkillRefs 前查询可用技能）。挂在 /internal 组上（RequireServiceToken）。
//   - subject 必填：identity_links active 且存在 knowledge 用户映射，否则 403；
//   - tenant 显式给出时必须在该 subject active entitlement 集内，否则 403；
//     省略时返回全部 entitlement 租户的可见技能；
//   - 逐租户复用 queryPublishedSkillReleases（与 INT-142 客户端列表同一
//     查询语义），响应 {skills: [{publish_id, version, manifest, hash,
//     source_tenant}]}。
func (h *SkillsHandler) InternalList(c *gin.Context) {
	ctx := c.Request.Context()
	subject := c.Query("subject")
	if subject == "" {
		writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "subject query parameter is required")
		return
	}

	links, err := h.storage.ListIdentityLinksByImmutableSubject(ctx, subject)
	if err != nil {
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "list identity links failed")
		return
	}
	var link *model.IdentityLink
	for i := range links {
		if links[i].Status == "active" && links[i].KnowledgeUserID != "" {
			link = &links[i]
			break
		}
	}
	if link == nil {
		writeErrorEnvelope(c, http.StatusForbidden, errCodeEntitlementRevoked, "subject identity link is not active")
		return
	}

	tenants, err := entitledTenants(ctx, h.storage, subject)
	if err != nil {
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "list entitlements failed")
		return
	}
	if len(tenants) == 0 {
		writeErrorEnvelope(c, http.StatusForbidden, errCodeEntitlementRevoked, "subject has no active entitlement")
		return
	}
	if explicit := c.Query("tenant"); explicit != "" {
		allowed := false
		for _, t := range tenants {
			if t == explicit {
				allowed = true
				break
			}
		}
		if !allowed {
			writeErrorEnvelope(c, http.StatusForbidden, errCodeEntitlementRevoked, "tenant not in active entitlement set")
			return
		}
		tenants = []string{explicit}
	}

	skills, err := h.queryPublishedSkillReleases(ctx, c, link, tenants)
	if err != nil {
		if !c.IsAborted() {
			denyUpstream(c, err)
		}
		return
	}

	auditEvent(ctx, h.storage, "service:"+internalServiceActor(c), "integration.internal.skills.listed", "GET "+c.FullPath(), integrationRequestID(c), map[string]interface{}{
		"result":  "allowed",
		"subject": subject,
		"tenants": len(tenants),
		"skills":  len(skills),
	})

	c.JSON(http.StatusOK, gin.H{"skills": skills})
}

// internalServiceActor 读 RequireServiceToken 中间件写入的服务 token 主体。
func internalServiceActor(c *gin.Context) string {
	v, _ := c.Get(internalServiceSubjectKey)
	subject, _ := v.(string)
	if subject == "" {
		return "anonymous"
	}
	return subject
}

// ---------- INT-141a POST /integration/v1/admin/skills/publish-jobs ----------

type createPublishJobRequest struct {
	ReleaseID    string   `json:"release_id"`
	SourceTenant string   `json:"source_tenant"`
	Targets      []string `json:"targets"`
}

// CreatePublishJob 创建跨空间发布任务（记账 + 逐目标可见性校验）。
// 权限：system_admin 任意源空间；dept_admin 仅限本空间（claims.TenantID）。
// 幂等：Idempotency-Key 头必填，重复提交返回原任务。
// 校验：经调用者桥 token 查源空间 release 元数据，逐目标确认在
// release.target_tenants 内（内容与分发权限裁定在 knowledge，平台不代裁定）。
func (h *SkillsHandler) CreatePublishJob(c *gin.Context) {
	ctx := c.Request.Context()
	claims, ok := adminauth.GetClaims(c)
	if !ok {
		writeErrorEnvelope(c, http.StatusUnauthorized, errCodeSessionRevoked, "missing admin claims")
		return
	}
	var req createPublishJobRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.ReleaseID == "" || req.SourceTenant == "" || len(req.Targets) == 0 {
		writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "release_id, source_tenant and non-empty targets are required")
		return
	}
	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if idempotencyKey == "" {
		writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "Idempotency-Key header is required")
		return
	}

	sourceTenantNumeric, err := strconv.ParseUint(req.SourceTenant, 10, 64)
	if err != nil {
		writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "source_tenant must be numeric")
		return
	}
	if claims.Role == adminauth.RoleDeptAdmin && claims.TenantID != sourceTenantNumeric {
		writeErrorEnvelope(c, http.StatusForbidden, errCodeSpaceForbidden, "dept_admin is limited to the source tenant")
		return
	}

	// 幂等：同键返回原任务。
	if existing, err := h.storage.GetSkillPublishJobByIdempotencyKey(ctx, idempotencyKey); err == nil && existing != nil {
		h.respondPublishJob(c, existing)
		return
	}

	// 调用者需有 active 身份关联（桥只为有效映射用户签发上游 token）。
	link, ok := h.adminIdentityLink(c, claims.Subject)
	if !ok {
		return
	}
	token, ok := issueBridgeToken(c, h.bridge, link, req.SourceTenant, bridgeTokenTTL)
	if !ok {
		return
	}
	release, err := h.upstream.getSkillRelease(ctx, token, req.ReleaseID)
	if err != nil {
		var uerr *upstreamError
		if errors.As(err, &uerr) && uerr.Status == http.StatusNotFound {
			writeErrorEnvelope(c, http.StatusNotFound, errCodeInvalidRequest, "release not found or not visible in source tenant")
			return
		}
		denyUpstream(c, err)
		return
	}

	allowedTargets := make(map[uint64]struct{}, len(release.TargetTenants))
	for _, t := range release.TargetTenants {
		allowedTargets[t] = struct{}{}
	}
	targetSet := make(map[string]struct{}, len(req.Targets))
	steps := make([]publishJobStep, 0, len(req.Targets))
	for _, target := range req.Targets {
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		if _, dup := targetSet[target]; dup {
			continue
		}
		targetSet[target] = struct{}{}
		numeric, err := strconv.ParseUint(target, 10, 64)
		if err != nil {
			steps = append(steps, publishJobStep{Tenant: target, Status: "failed", Error: "target must be numeric tenant id"})
			continue
		}
		if _, allowed := allowedTargets[numeric]; !allowed {
			steps = append(steps, publishJobStep{Tenant: target, Status: "failed", Error: "target not in release target_tenants"})
			continue
		}
		steps = append(steps, publishJobStep{Tenant: target, Status: "validated"})
	}
	if len(steps) == 0 {
		writeErrorEnvelope(c, http.StatusBadRequest, errCodeInvalidRequest, "targets must contain at least one valid tenant id")
		return
	}

	status := jobStatusSucceeded
	failed := 0
	for _, step := range steps {
		if step.Status != "validated" {
			failed++
		}
	}
	switch {
	case failed == len(steps):
		status = jobStatusFailed
	case failed > 0:
		status = jobStatusPartialFailed
	}

	targetsJSON, _ := json.Marshal(sortedKeys(targetSet))
	stepsJSON, _ := json.Marshal(steps)
	job := &model.SkillPublishJob{
		JobID:          uuid.NewString(),
		ReleaseID:      req.ReleaseID,
		SourceTenant:   req.SourceTenant,
		Targets:        string(targetsJSON),
		IdempotencyKey: idempotencyKey,
		Status:         status,
		Steps:          stringPtrOrNil(string(stepsJSON)),
	}
	if err := h.storage.InsertSkillPublishJob(ctx, job); err != nil {
		// 并发同键：唯一约束冲突时返回先落库的原任务。
		if existing, lookupErr := h.storage.GetSkillPublishJobByIdempotencyKey(ctx, idempotencyKey); lookupErr == nil && existing != nil {
			h.respondPublishJob(c, existing)
			return
		}
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "persist publish job failed: "+err.Error())
		return
	}

	auditEvent(ctx, h.storage, claims.Subject, "integration.admin.skill_publish_job.created", "POST "+c.FullPath(), integrationRequestID(c), map[string]interface{}{
		"result":          "allowed",
		"job_id":          job.JobID,
		"release_id":      req.ReleaseID,
		"source_tenant":   req.SourceTenant,
		"targets":         len(steps),
		"status":          status,
		"idempotency_key": idempotencyKey,
	})

	h.respondPublishJob(c, job)
}

// adminIdentityLink 解析管理调用者的 active 身份关联（桥签发前提）。
// INT-141 以 claims.Subject 为 immutable_subject 匹配（部署约定：管理 token
// 的 sub 使用供给主体的不可变 ID）；无 active 关联拒绝。
func (h *SkillsHandler) adminIdentityLink(c *gin.Context, subject string) (*model.IdentityLink, bool) {
	links, err := h.storage.ListIdentityLinksByImmutableSubject(c.Request.Context(), subject)
	if err != nil {
		writeErrorEnvelope(c, http.StatusInternalServerError, errCodeInternal, "list identity links failed")
		return nil, false
	}
	for _, link := range links {
		if link.Status == "active" && link.KnowledgeUserID != "" {
			return &link, true
		}
	}
	writeErrorEnvelope(c, http.StatusForbidden, errCodeSubjectNotProv, "admin subject has no active identity link for upstream validation")
	return nil, false
}

// respondPublishJob 统一发布任务响应 {job_id, status, steps, ...}。
func (h *SkillsHandler) respondPublishJob(c *gin.Context, job *model.SkillPublishJob) {
	var steps []publishJobStep
	if job.Steps != nil {
		_ = json.Unmarshal([]byte(*job.Steps), &steps)
	}
	c.JSON(http.StatusOK, gin.H{
		"job_id":        job.JobID,
		"release_id":    job.ReleaseID,
		"source_tenant": job.SourceTenant,
		"targets":       json.RawMessage(job.Targets),
		"status":        job.Status,
		"steps":         steps,
		"created_at":    job.CreatedAt,
		"updated_at":    job.UpdatedAt,
	})
}

// ---------- INT-141b GET /integration/v1/admin/skills/publish-jobs/:id ----------

// GetPublishJob 查询发布任务进度与逐目标结果；dept_admin 仅限本空间任务。
func (h *SkillsHandler) GetPublishJob(c *gin.Context) {
	ctx := c.Request.Context()
	claims, ok := adminauth.GetClaims(c)
	if !ok {
		writeErrorEnvelope(c, http.StatusUnauthorized, errCodeSessionRevoked, "missing admin claims")
		return
	}
	job, err := h.storage.GetSkillPublishJob(ctx, c.Param("id"))
	if err != nil {
		writeErrorEnvelope(c, http.StatusNotFound, errCodeInvalidRequest, "publish job not found")
		return
	}
	if claims.Role == adminauth.RoleDeptAdmin {
		jobTenant, _ := strconv.ParseUint(job.SourceTenant, 10, 64)
		if claims.TenantID != jobTenant {
			writeErrorEnvelope(c, http.StatusForbidden, errCodeSpaceForbidden, "dept_admin is limited to the source tenant")
			return
		}
	}
	auditEvent(ctx, h.storage, claims.Subject, "integration.admin.skill_publish_job.viewed", "GET "+c.FullPath(), integrationRequestID(c), map[string]interface{}{
		"result": "allowed",
		"job_id": job.JobID,
		"status": job.Status,
	})
	h.respondPublishJob(c, job)
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
