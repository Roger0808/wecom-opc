package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"weknora-tokendance/pkg/crypto"
	"weknora-tokendance/pkg/model"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type AdminOverviewStats struct {
	TotalTenants        int64  `json:"total_tenants"`
	DeptTenants         int64  `json:"dept_tenants"`
	ModelsSyncedTenants int64  `json:"models_synced_tenants"`
	StorageReadyTenants int64  `json:"storage_ready_tenants"`
	RootModelsCount     int64  `json:"root_models_count"`
	CronEnabled         bool   `json:"cron_enabled"`
	CronSchedule        string `json:"cron_schedule"`
	LastSyncTime        string `json:"last_sync_time"`
	RootTenantID        uint64 `json:"root_tenant_id"`
}

type AdminUserBrief struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	Role     string `json:"role"`
}

type SpaceConfigDTO struct {
	ID                  uint64           `json:"id"`
	Name                string           `json:"name"`
	Description         string           `json:"description"`
	DepartmentCode      string           `json:"department_code"`
	WeComDeptID         int64            `json:"wecom_dept_id"`
	Status              string           `json:"status"`
	ModelsCount         int64            `json:"models_count"`
	ExpectedModelsCount int64            `json:"expected_models_count"`
	HasMinioStorage     bool             `json:"has_minio_storage"`
	HasLocalStorage     bool             `json:"has_local_storage"`
	DefaultStorageID    string           `json:"default_storage_id"`
	Admins              []AdminUserBrief `json:"admins"`
	MemberCount         int64            `json:"member_count"`
	CreatedAt           time.Time        `json:"created_at"`
	UpdatedAt           time.Time        `json:"updated_at"`
}

type UserSearchDTO struct {
	ID        string           `json:"id"`
	Username  string           `json:"username"`
	Email     string           `json:"email"`
	Avatar    string           `json:"avatar"`
	IsActive  bool             `json:"is_active"`
	Spaces    []UserSpaceBrief `json:"spaces"`
	CreatedAt time.Time        `json:"created_at"`
}

type UserSpaceBrief struct {
	TenantID uint64 `json:"tenant_id"`
	Name     string `json:"name"`
	Role     string `json:"role"`
}

// GetOverviewStats 获取大盘统计
func (s *Storage) GetOverviewStats(ctx context.Context, rootTenantID uint64) (*AdminOverviewStats, error) {
	stats := &AdminOverviewStats{
		RootTenantID: rootTenantID,
	}

	// 1. 总空间数
	if err := s.db.WithContext(ctx).Model(&model.WeKnoraTenant{}).
		Where("deleted_at IS NULL").Count(&stats.TotalTenants).Error; err != nil {
		return nil, fmt.Errorf("count total tenants: %w", err)
	}

	// 2. 部门空间映射数
	if err := s.db.WithContext(ctx).Model(&model.WeComDeptTenantMapping{}).
		Count(&stats.DeptTenants).Error; err != nil {
		return nil, fmt.Errorf("count dept tenants: %w", err)
	}

	// 3. 根空间期望模型数
	if rootTenantID > 0 {
		_ = s.db.WithContext(ctx).Model(&model.WeKnoraModel{}).
			Where("tenant_id = ? AND deleted_at IS NULL", rootTenantID).
			Count(&stats.RootModelsCount).Error
	}

	// 4. 存储引擎已绑定就绪的空间数
	_ = s.db.WithContext(ctx).Model(&model.WeKnoraTenant{}).
		Where("deleted_at IS NULL AND default_storage_backend_id IS NOT NULL AND default_storage_backend_id != ''").
		Count(&stats.StorageReadyTenants).Error

	// 5. 模型同步齐全的空间数
	if stats.RootModelsCount > 0 {
		var count int64
		_ = s.db.WithContext(ctx).Raw(`
			SELECT count(DISTINCT tenant_id) FROM (
				SELECT tenant_id, count(*) as c 
				FROM models 
				WHERE deleted_at IS NULL 
				GROUP BY tenant_id 
				HAVING count(*) >= ?
			) sub
		`, stats.RootModelsCount).Scan(&count).Error
		stats.ModelsSyncedTenants = count
	}

	return stats, nil
}

// ListSpacesWithConfig 获取空间列表与详细配置
func (s *Storage) ListSpacesWithConfig(ctx context.Context, rootTenantID uint64, keyword string) ([]SpaceConfigDTO, error) {
	query := s.db.WithContext(ctx).Table("tenants t").
		Select(`
			t.id, t.name, t.description, t.status, t.default_storage_backend_id, t.created_at, t.updated_at,
			COALESCE(m.department_code, '') as department_code,
			COALESCE(m.we_com_dept_id, 0) as we_com_dept_id
		`).
		Joins("LEFT JOIN wecom_dept_tenant_mappings m ON t.id = m.tenant_id").
		Where("t.deleted_at IS NULL")

	if strings.TrimSpace(keyword) != "" {
		kw := "%" + strings.TrimSpace(keyword) + "%"
		query = query.Where("t.name ILIKE ? OR CAST(t.id AS TEXT) ILIKE ? OR m.department_code ILIKE ?", kw, kw, kw)
	}

	type spaceRow struct {
		ID                      uint64
		Name                    string
		Description             string
		Status                  string
		DefaultStorageBackendID *string
		DepartmentCode          string
		WeComDeptID             int64
		CreatedAt               time.Time
		UpdatedAt               time.Time
	}

	var rows []spaceRow
	if err := query.Order("t.id ASC").Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("query spaces: %w", err)
	}

	if len(rows) == 0 {
		return []SpaceConfigDTO{}, nil
	}

	// 批量查模型计数
	type modelCountRow struct {
		TenantID uint64
		Count    int64
	}
	var modelCounts []modelCountRow
	_ = s.db.WithContext(ctx).Table("models").
		Select("tenant_id, count(*) as count").
		Where("deleted_at IS NULL").
		Group("tenant_id").
		Scan(&modelCounts).Error
	modelCountMap := make(map[uint64]int64, len(modelCounts))
	for _, mc := range modelCounts {
		modelCountMap[mc.TenantID] = mc.Count
	}

	// 批量查存储后端
	type storageRow struct {
		TenantID uint64
		Provider string
	}
	var storageRows []storageRow
	_ = s.db.WithContext(ctx).Table("storage_backends").
		Select("tenant_id, provider").
		Where("deleted_at IS NULL AND status = 'active'").
		Scan(&storageRows).Error
	storageMap := make(map[uint64]map[string]bool)
	for _, sr := range storageRows {
		if storageMap[sr.TenantID] == nil {
			storageMap[sr.TenantID] = make(map[string]bool)
		}
		storageMap[sr.TenantID][sr.Provider] = true
	}

	// 批量查管理员与成员总数
	type adminRow struct {
		TenantID uint64
		UserID   string
		Username string
		Email    string
		Role     string
	}
	var adminRows []adminRow
	_ = s.db.WithContext(ctx).Table("tenant_members tm").
		Select("tm.tenant_id, tm.user_id, u.username, u.email, tm.role").
		Joins("JOIN users u ON tm.user_id = u.id").
		Where("tm.deleted_at IS NULL AND tm.role = 'admin' AND tm.status = 'active'").
		Scan(&adminRows).Error
	adminMap := make(map[uint64][]AdminUserBrief)
	for _, ar := range adminRows {
		adminMap[ar.TenantID] = append(adminMap[ar.TenantID], AdminUserBrief{
			ID:       ar.UserID,
			Username: ar.Username,
			Email:    ar.Email,
			Role:     ar.Role,
		})
	}

	type memberCountRow struct {
		TenantID uint64
		Count    int64
	}
	var memberCounts []memberCountRow
	_ = s.db.WithContext(ctx).Table("tenant_members").
		Select("tenant_id, count(*) as count").
		Where("deleted_at IS NULL AND status = 'active'").
		Group("tenant_id").
		Scan(&memberCounts).Error
	memberCountMap := make(map[uint64]int64, len(memberCounts))
	for _, mc := range memberCounts {
		memberCountMap[mc.TenantID] = mc.Count
	}

	var rootModelsCount int64 = 5
	if rootTenantID > 0 {
		if c, ok := modelCountMap[rootTenantID]; ok && c > 0 {
			rootModelsCount = c
		}
	}

	result := make([]SpaceConfigDTO, len(rows))
	for i, r := range rows {
		defaultStorageID := ""
		if r.DefaultStorageBackendID != nil {
			defaultStorageID = *r.DefaultStorageBackendID
		}

		sMap := storageMap[r.ID]
		hasMinio := sMap != nil && sMap["minio"]
		hasLocal := sMap != nil && sMap["local"]

		result[i] = SpaceConfigDTO{
			ID:                  r.ID,
			Name:                r.Name,
			Description:         r.Description,
			DepartmentCode:      r.DepartmentCode,
			WeComDeptID:         r.WeComDeptID,
			Status:              r.Status,
			ModelsCount:         modelCountMap[r.ID],
			ExpectedModelsCount: rootModelsCount,
			HasMinioStorage:     hasMinio,
			HasLocalStorage:     hasLocal,
			DefaultStorageID:    defaultStorageID,
			Admins:              adminMap[r.ID],
			MemberCount:         memberCountMap[r.ID],
			CreatedAt:           r.CreatedAt,
			UpdatedAt:           r.UpdatedAt,
		}
	}

	return result, nil
}

// SyncSpaceStorage 为单个空间同步 MinIO 与 Local 存储后端
func (s *Storage) SyncSpaceStorage(ctx context.Context, tenantID uint64, rootTenantID uint64) error {
	if tenantID == 0 {
		return fmt.Errorf("invalid tenant id")
	}

	// 1. 查找 root 空间的 MinIO 配置作为模板
	var rootMinio model.WeKnoraStorageBackend
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND provider = 'minio' AND deleted_at IS NULL", rootTenantID).
		First(&rootMinio).Error
	if err != nil {
		// 回退查任意可用的 minio backend
		_ = s.db.WithContext(ctx).
			Where("provider = 'minio' AND deleted_at IS NULL").
			First(&rootMinio).Error
	}

	now := time.Now()

	// 2. 确保存储后端有 minio
	var minioBackend model.WeKnoraStorageBackend
	minioErr := s.db.WithContext(ctx).
		Where("tenant_id = ? AND provider = 'minio' AND deleted_at IS NULL", tenantID).
		First(&minioBackend).Error

	var targetMinioID string
	if minioErr != nil && minioErr == gorm.ErrRecordNotFound {
		targetMinioID = uuid.New().String()
		cfg := rootMinio.Config
		if cfg == "" {
			cfg = `{"mode": "remote", "endpoint": "minio:9000", "bucket_name": "weknora", "path_prefix": "weknora/"}`
		}
		newMinio := model.WeKnoraStorageBackend{
			ID:          targetMinioID,
			TenantID:    tenantID,
			Name:        "System MINIO",
			Provider:    "minio",
			Config:      cfg,
			Source:      "env",
			Status:      "active",
			LegacyAlias: true,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := s.db.WithContext(ctx).Create(&newMinio).Error; err != nil {
			return fmt.Errorf("create minio backend for tenant %d: %w", tenantID, err)
		}
	} else if minioErr == nil {
		targetMinioID = minioBackend.ID
		if rootMinio.Config != "" {
			_ = s.db.WithContext(ctx).Model(&model.WeKnoraStorageBackend{}).
				Where("id = ?", minioBackend.ID).
				Updates(map[string]interface{}{
					"config":     rootMinio.Config,
					"updated_at": now,
				}).Error
		}
	}

	// 3. 确保存储后端有 local 兼容别名
	var localBackend model.WeKnoraStorageBackend
	localErr := s.db.WithContext(ctx).
		Where("tenant_id = ? AND provider = 'local' AND deleted_at IS NULL", tenantID).
		First(&localBackend).Error

	if localErr != nil && localErr == gorm.ErrRecordNotFound {
		newLocal := model.WeKnoraStorageBackend{
			ID:          uuid.New().String(),
			TenantID:    tenantID,
			Name:        "Local",
			Provider:    "local",
			Config:      `{"path_prefix": ""}`,
			Source:      "env",
			Status:      "active",
			LegacyAlias: true,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		_ = s.db.WithContext(ctx).Create(&newLocal).Error
	}

	// 4. 更新 tenants 表的 default_storage_backend_id
	if targetMinioID != "" {
		if err := s.db.WithContext(ctx).Model(&model.WeKnoraTenant{}).
			Where("id = ?", tenantID).
			Updates(map[string]interface{}{
				"default_storage_backend_id": targetMinioID,
				"updated_at":                 now,
			}).Error; err != nil {
			return fmt.Errorf("update tenant default storage: %w", err)
		}
	}

	return nil
}

// SearchUsers 搜索用户及归属空间
func (s *Storage) SearchUsers(ctx context.Context, keyword string, limit int) ([]UserSearchDTO, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}

	query := s.db.WithContext(ctx).Model(&model.WeKnoraUser{}).
		Where("deleted_at IS NULL")

	if strings.TrimSpace(keyword) != "" {
		kw := "%" + strings.TrimSpace(keyword) + "%"
		query = query.Where("username ILIKE ? OR email ILIKE ? OR CAST(id AS TEXT) ILIKE ?", kw, kw, kw)
	}

	var users []model.WeKnoraUser
	if err := query.Order("created_at DESC").Limit(limit).Find(&users).Error; err != nil {
		return nil, fmt.Errorf("search users: %w", err)
	}

	if len(users) == 0 {
		return []UserSearchDTO{}, nil
	}

	userIDs := make([]string, len(users))
	for i, u := range users {
		userIDs[i] = u.ID
	}

	type memberWithTenant struct {
		UserID     string
		TenantID   uint64
		TenantName string
		Role       string
	}
	var members []memberWithTenant
	_ = s.db.WithContext(ctx).Table("tenant_members tm").
		Select("tm.user_id, tm.tenant_id, t.name as tenant_name, tm.role").
		Joins("JOIN tenants t ON tm.tenant_id = t.id").
		Where("tm.user_id IN ? AND tm.deleted_at IS NULL AND tm.status = 'active'", userIDs).
		Scan(&members).Error

	memberMap := make(map[string][]UserSpaceBrief)
	for _, m := range members {
		memberMap[m.UserID] = append(memberMap[m.UserID], UserSpaceBrief{
			TenantID: m.TenantID,
			Name:     m.TenantName,
			Role:     m.Role,
		})
	}

	res := make([]UserSearchDTO, len(users))
	for i, u := range users {
		res[i] = UserSearchDTO{
			ID:        u.ID,
			Username:  u.Username,
			Email:     u.Email,
			Avatar:    u.Avatar,
			IsActive:  u.IsActive,
			Spaces:    memberMap[u.ID],
			CreatedAt: u.CreatedAt,
		}
	}
	return res, nil
}

// UpdateSpaceAdmin 赋予或撤销空间管理员角色
func (s *Storage) UpdateSpaceAdmin(ctx context.Context, tenantID uint64, userID string, action string) error {
	if tenantID == 0 || userID == "" {
		return fmt.Errorf("tenant_id and user_id are required")
	}

	now := time.Now()
	var member model.WeKnoraTenantMember
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND user_id = ?", tenantID, userID).
		First(&member).Error

	if action == "add" {
		if err == gorm.ErrRecordNotFound {
			newMember := model.WeKnoraTenantMember{
				UserID:    userID,
				TenantID:  tenantID,
				Role:      "admin",
				Status:    "active",
				JoinedAt:  now,
				CreatedAt: now,
				UpdatedAt: now,
			}
			return s.db.WithContext(ctx).Create(&newMember).Error
		} else if err == nil {
			return s.db.WithContext(ctx).Model(&member).
				Updates(map[string]interface{}{
					"role":       "admin",
					"status":     "active",
					"updated_at": now,
				}).Error
		}
		return err
	} else if action == "remove" {
		if err == nil {
			// 降级为 contributor
			return s.db.WithContext(ctx).Model(&member).
				Updates(map[string]interface{}{
					"role":       "contributor",
					"updated_at": now,
				}).Error
		}
		return nil
	}

	return fmt.Errorf("unsupported action: %s", action)
}

// MaskSecret 对敏感密钥进行脱敏处理（保留前 4 位和后 4 位，中间星号；过短则显示 ***）
func MaskSecret(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) <= 10 {
		return "***"
	}
	return s[:4] + "********" + s[len(s)-4:]
}

func maskSecret(s string) string {
	return MaskSecret(s)
}

// GetRootSpaceConfig 获取母空间的基准配置（包含模型列表与 MinIO 存储配置，敏感字段脱敏）
func (s *Storage) GetRootSpaceConfig(ctx context.Context, rootTenantID uint64, aesKey []byte) (*model.RootConfigDTO, error) {
	if rootTenantID == 0 {
		return nil, fmt.Errorf("invalid root tenant id")
	}

	// 1. 查询母空间租户信息
	var tenant model.WeKnoraTenant
	tenantErr := s.db.WithContext(ctx).
		Where("id = ? AND deleted_at IS NULL", rootTenantID).
		First(&tenant).Error
	if tenantErr != nil && tenantErr != gorm.ErrRecordNotFound {
		return nil, fmt.Errorf("query root tenant: %w", tenantErr)
	}

	tenantName := tenant.Name
	if tenantName == "" {
		tenantName = fmt.Sprintf("母空间 (%d)", rootTenantID)
	}

	// 2. 查询母空间下的所有模型
	var models []model.WeKnoraModel
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND deleted_at IS NULL", rootTenantID).
		Order("created_at ASC, id ASC").
		Find(&models).Error; err != nil {
		return nil, fmt.Errorf("query root models: %w", err)
	}

	rootModels := make([]model.RootModelDTO, 0, len(models))
	for _, m := range models {
		var params map[string]interface{}
		if m.Parameters != "" {
			_ = json.Unmarshal([]byte(m.Parameters), &params)
		}
		if params == nil {
			params = make(map[string]interface{})
		}

		baseURL, _ := params["base_url"].(string)
		provider, _ := params["provider"].(string)
		if provider == "" {
			provider = "generic"
		}

		apiKeyRaw, _ := params["api_key"].(string)
		if apiKeyRaw == "" {
			apiKeyRaw, _ = params["app_secret"].(string)
		}

		decryptedKey, err := crypto.DecryptAESGCM(apiKeyRaw, aesKey)
		if err != nil {
			decryptedKey = apiKeyRaw
		}
		apiKeyMasked := maskSecret(decryptedKey)

		// 脱敏 parameters 字典中的 sensitive key
		clonedParams := make(map[string]interface{}, len(params))
		for k, v := range params {
			clonedParams[k] = v
		}
		if _, hasKey := clonedParams["api_key"]; hasKey {
			clonedParams["api_key"] = apiKeyMasked
		}
		if _, hasSecret := clonedParams["app_secret"]; hasSecret {
			clonedParams["app_secret"] = apiKeyMasked
		}

		source := m.Source
		if source == "" {
			source = "remote"
		}

		rootModels = append(rootModels, model.RootModelDTO{
			ID:           m.ID,
			Name:         m.Name,
			DisplayName:  m.DisplayName,
			Type:         m.Type,
			Source:       source,
			Provider:     provider,
			BaseURL:      baseURL,
			APIKeyMasked: apiKeyMasked,
			IsDefault:    m.IsDefault,
			Status:       m.Status,
			Parameters:   clonedParams,
		})
	}

	// 3. 查询母空间的 MinIO 存储配置
	var sb model.WeKnoraStorageBackend
	sbErr := s.db.WithContext(ctx).
		Where("tenant_id = ? AND provider = 'minio' AND deleted_at IS NULL", rootTenantID).
		First(&sb).Error

	var storageDTO *model.RootStorageDTO
	if sbErr == nil {
		var cfg map[string]interface{}
		if sb.Config != "" {
			_ = json.Unmarshal([]byte(sb.Config), &cfg)
		}
		if cfg == nil {
			cfg = make(map[string]interface{})
		}

		endpoint, _ := cfg["endpoint"].(string)
		bucketName, _ := cfg["bucket_name"].(string)
		pathPrefix, _ := cfg["path_prefix"].(string)

		accessKeyRaw, _ := cfg["access_key_id"].(string)
		if accessKeyRaw == "" {
			accessKeyRaw, _ = cfg["access_key"].(string)
		}
		secretKeyRaw, _ := cfg["secret_access_key"].(string)
		if secretKeyRaw == "" {
			secretKeyRaw, _ = cfg["secret_key"].(string)
		}

		decryptedAccess, err := crypto.DecryptAESGCM(accessKeyRaw, aesKey)
		if err != nil {
			decryptedAccess = accessKeyRaw
		}
		accessKeyMasked := maskSecret(decryptedAccess)

		decryptedSecret, err := crypto.DecryptAESGCM(secretKeyRaw, aesKey)
		if err != nil {
			decryptedSecret = secretKeyRaw
		}
		secretKeyMasked := maskSecret(decryptedSecret)

		storageDTO = &model.RootStorageDTO{
			ID:              sb.ID,
			Name:            sb.Name,
			Provider:        sb.Provider,
			Endpoint:        endpoint,
			BucketName:      bucketName,
			PathPrefix:      pathPrefix,
			AccessKeyMasked: accessKeyMasked,
			SecretKeyMasked: secretKeyMasked,
			Status:          sb.Status,
		}
	}

	// 4. 查询母空间的沙箱环境配置
	sandboxDTO, err := s.GetRootSandboxConfig(ctx, rootTenantID)
	if err != nil {
		return nil, fmt.Errorf("query root sandbox config: %w", err)
	}

	return &model.RootConfigDTO{
		TenantID:    rootTenantID,
		Name:        tenantName,
		Description: tenant.Description,
		Models:      rootModels,
		Storage:     storageDTO,
		Sandbox:     sandboxDTO,
	}, nil
}

// UpdateRootModel 更新或新增母空间单个模型
func (s *Storage) UpdateRootModel(ctx context.Context, rootTenantID uint64, req model.UpdateModelReq, aesKey []byte) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return s.updateRootModelTx(ctx, tx, rootTenantID, req, aesKey)
	})
}

func (s *Storage) updateRootModelTx(ctx context.Context, tx *gorm.DB, rootTenantID uint64, req model.UpdateModelReq, aesKey []byte) error {
	if rootTenantID == 0 {
		return fmt.Errorf("invalid root tenant id")
	}
	if req.Name == "" {
		return fmt.Errorf("model name is required")
	}
	if req.Type == "" {
		return fmt.Errorf("model type is required")
	}

	now := time.Now()
	var existing model.WeKnoraModel
	var found bool

	if req.ID != "" {
		err := tx.Where("tenant_id = ? AND id = ? AND deleted_at IS NULL", rootTenantID, req.ID).
			First(&existing).Error
		if err == nil {
			found = true
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("query existing model: %w", err)
		}
	}

	if found {
		var params map[string]interface{}
		if existing.Parameters != "" {
			_ = json.Unmarshal([]byte(existing.Parameters), &params)
		}
		if params == nil {
			params = make(map[string]interface{})
		}

		// 合并 req.Parameters (跳过 api_key 和 app_secret，保留原有或由 req.APIKey 替换)
		if req.Parameters != nil {
			for k, v := range req.Parameters {
				if k != "api_key" && k != "app_secret" {
					params[k] = v
				}
			}
		}

		if req.BaseURL != "" {
			params["base_url"] = req.BaseURL
		}
		if req.Provider != "" {
			params["provider"] = req.Provider
		}

		// 处理 API Key：非空且不包含星号掩码，表示传入了新 Key，加密入库
		if req.APIKey != "" && !strings.Contains(req.APIKey, "*") {
			encKey, err := crypto.EncryptAESGCM(req.APIKey, aesKey)
			if err != nil {
				return fmt.Errorf("encrypt api key: %w", err)
			}
			params["api_key"] = encKey
		}

		paramsBytes, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("marshal parameters: %w", err)
		}

		if req.IsDefault {
			if err := tx.Model(&model.WeKnoraModel{}).
				Where("tenant_id = ? AND type = ? AND id != ? AND deleted_at IS NULL", rootTenantID, req.Type, existing.ID).
				Update("is_default", false).Error; err != nil {
				return fmt.Errorf("clear default model flag: %w", err)
			}
		}

		updates := map[string]interface{}{
			"name":       req.Name,
			"type":       req.Type,
			"parameters": string(paramsBytes),
			"is_default": req.IsDefault,
			"updated_at": now,
		}
		if req.DisplayName != "" {
			updates["display_name"] = req.DisplayName
		}
		if req.Source != "" {
			updates["source"] = req.Source
		}
		if req.Status != "" {
			updates["status"] = req.Status
		}

		return tx.Model(&model.WeKnoraModel{}).
			Where("id = ?", existing.ID).
			Updates(updates).Error
	}

	// 新增模型
	modelID := req.ID
	if modelID == "" {
		modelID = uuid.New().String()
	}

	params := make(map[string]interface{})
	if req.Parameters != nil {
		for k, v := range req.Parameters {
			if k != "api_key" && k != "app_secret" {
				params[k] = v
			}
		}
	}
	if req.BaseURL != "" {
		params["base_url"] = req.BaseURL
	}
	if req.Provider != "" {
		params["provider"] = req.Provider
	} else if _, has := params["provider"]; !has {
		params["provider"] = "generic"
	}
	if req.APIKey != "" && !strings.Contains(req.APIKey, "*") {
		encKey, err := crypto.EncryptAESGCM(req.APIKey, aesKey)
		if err != nil {
			return fmt.Errorf("encrypt api key: %w", err)
		}
		params["api_key"] = encKey
	}

	paramsBytes, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal parameters: %w", err)
	}

	if req.IsDefault {
		if err := tx.Model(&model.WeKnoraModel{}).
			Where("tenant_id = ? AND type = ? AND deleted_at IS NULL", rootTenantID, req.Type).
			Update("is_default", false).Error; err != nil {
			return fmt.Errorf("clear default model flag: %w", err)
		}
	}

	displayName := req.DisplayName
	if displayName == "" {
		displayName = req.Name
	}
	source := req.Source
	if source == "" {
		source = "remote"
	}
	status := req.Status
	if status == "" {
		status = "active"
	}

	newModel := model.WeKnoraModel{
		ID:          modelID,
		TenantID:    rootTenantID,
		Name:        req.Name,
		DisplayName: displayName,
		Type:        req.Type,
		Source:      source,
		Parameters:  string(paramsBytes),
		IsDefault:   req.IsDefault,
		Status:      status,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	return tx.Create(&newModel).Error
}

// DeleteRootModel 软删除母空间指定模型
func (s *Storage) DeleteRootModel(ctx context.Context, rootTenantID uint64, modelID string) error {
	return s.deleteRootModelTx(ctx, s.db.WithContext(ctx), rootTenantID, modelID)
}

func (s *Storage) deleteRootModelTx(ctx context.Context, db *gorm.DB, rootTenantID uint64, modelID string) error {
	if rootTenantID == 0 || modelID == "" {
		return fmt.Errorf("tenant_id and model_id are required")
	}
	return db.Where("tenant_id = ? AND id = ?", rootTenantID, modelID).
		Delete(&model.WeKnoraModel{}).Error
}

// UpdateRootModels 批量更新或删除母空间模型
func (s *Storage) UpdateRootModels(ctx context.Context, rootTenantID uint64, reqs []model.UpdateModelReq, aesKey []byte) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, req := range reqs {
			if req.Deleted {
				if req.ID != "" {
					if err := s.deleteRootModelTx(ctx, tx, rootTenantID, req.ID); err != nil {
						return err
					}
				}
				continue
			}
			if err := s.updateRootModelTx(ctx, tx, rootTenantID, req, aesKey); err != nil {
				return err
			}
		}
		return nil
	})
}

// UpdateRootStorage 更新母空间 MinIO 存储配置
func (s *Storage) UpdateRootStorage(ctx context.Context, rootTenantID uint64, req model.UpdateStorageReq, aesKey []byte) error {
	if rootTenantID == 0 {
		return fmt.Errorf("invalid root tenant id")
	}

	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var sb model.WeKnoraStorageBackend
		var exists bool
		findErr := tx.Where("tenant_id = ? AND provider = 'minio' AND deleted_at IS NULL", rootTenantID).
			First(&sb).Error
		if findErr == nil {
			exists = true
		} else if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return fmt.Errorf("query storage backend: %w", findErr)
		}

		now := time.Now()
		var cfg map[string]interface{}
		if exists && sb.Config != "" {
			_ = json.Unmarshal([]byte(sb.Config), &cfg)
		}
		if cfg == nil {
			cfg = make(map[string]interface{})
			cfg["mode"] = "remote"
		}

		if req.Endpoint != "" {
			cfg["endpoint"] = req.Endpoint
		}
		if req.BucketName != "" {
			cfg["bucket_name"] = req.BucketName
		}
		if req.PathPrefix != "" {
			cfg["path_prefix"] = req.PathPrefix
		}
		if req.AccessKeyID != "" && !strings.Contains(req.AccessKeyID, "*") {
			cfg["access_key_id"] = req.AccessKeyID
		}
		if req.SecretAccessKey != "" && !strings.Contains(req.SecretAccessKey, "*") {
			encSecret, err := crypto.EncryptAESGCM(req.SecretAccessKey, aesKey)
			if err != nil {
				return fmt.Errorf("encrypt secret access key: %w", err)
			}
			cfg["secret_access_key"] = encSecret
		}

		cfgBytes, err := json.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("marshal storage config: %w", err)
		}

		var storageBackendID string
		if !exists {
			storageBackendID = uuid.New().String()
			newSb := model.WeKnoraStorageBackend{
				ID:          storageBackendID,
				TenantID:    rootTenantID,
				Name:        "System MINIO",
				Provider:    "minio",
				Config:      string(cfgBytes),
				Source:      "env",
				Status:      "active",
				LegacyAlias: true,
				CreatedAt:   now,
				UpdatedAt:   now,
			}
			if err := tx.Create(&newSb).Error; err != nil {
				return fmt.Errorf("create minio backend: %w", err)
			}
		} else {
			storageBackendID = sb.ID
			if err := tx.Model(&model.WeKnoraStorageBackend{}).
				Where("id = ?", sb.ID).
				Updates(map[string]interface{}{
					"config":     string(cfgBytes),
					"updated_at": now,
				}).Error; err != nil {
				return fmt.Errorf("update minio backend: %w", err)
			}
		}

		if storageBackendID != "" {
			if err := tx.Model(&model.WeKnoraTenant{}).
				Where("id = ? AND (default_storage_backend_id IS NULL OR default_storage_backend_id = '')", rootTenantID).
				Update("default_storage_backend_id", storageBackendID).Error; err != nil {
				return fmt.Errorf("update tenant default storage backend id: %w", err)
			}
		}

		return nil
	})
}

func (s *Storage) syncDepartmentModels(ctx context.Context, rootTenantID, deptTenantID uint64) error {
	return s.SyncTenantModelsFromParent(ctx, rootTenantID, deptTenantID)
}

func (s *Storage) syncDepartmentStorage(ctx context.Context, rootTenantID, deptTenantID uint64) error {
	return s.SyncSpaceStorage(ctx, deptTenantID, rootTenantID)
}

func (s *Storage) syncDepartmentSandbox(ctx context.Context, rootTenantID, deptTenantID uint64) error {
	return s.SyncSpaceSandbox(ctx, deptTenantID, rootTenantID)
}

// BroadcastRootConfigToAllTenants 将母空间的最新模型与存储配置级联广播同步至所有部门子空间
func (s *Storage) BroadcastRootConfigToAllTenants(ctx context.Context, rootTenantID uint64) (int, error) {
	if rootTenantID == 0 {
		return 0, fmt.Errorf("invalid root tenant id")
	}

	var tenantIDs []uint64
	err := s.db.WithContext(ctx).Model(&model.WeComDeptTenantMapping{}).
		Distinct("tenant_id").
		Pluck("tenant_id", &tenantIDs).Error
	if err != nil {
		return 0, fmt.Errorf("query dept tenant mappings: %w", err)
	}

	if len(tenantIDs) == 0 {
		return 0, nil
	}

	successCount := 0
	var errs []error

	for _, tid := range tenantIDs {
		if tid == 0 || tid == rootTenantID {
			continue
		}

		// 1. 同步模型配置
		if err := s.syncDepartmentModels(ctx, rootTenantID, tid); err != nil {
			errs = append(errs, fmt.Errorf("sync models to tenant %d: %w", tid, err))
			continue
		}

		// 2. 同步存储后端
		if err := s.syncDepartmentStorage(ctx, rootTenantID, tid); err != nil {
			errs = append(errs, fmt.Errorf("sync storage to tenant %d: %w", tid, err))
			continue
		}

		// 3. 同步沙箱配置
		if err := s.syncDepartmentSandbox(ctx, rootTenantID, tid); err != nil {
			errs = append(errs, fmt.Errorf("sync sandbox to tenant %d: %w", tid, err))
			continue
		}

		successCount++
	}

	var finalErr error
	if len(errs) > 0 {
		finalErr = errors.Join(errs...)
	}

	return successCount, finalErr
}

// GetRootSandboxConfig 获取母空间的沙箱配置
func (s *Storage) GetRootSandboxConfig(ctx context.Context, rootTenantID uint64) (*model.RootSandboxDTO, error) {
	if rootTenantID == 0 {
		return nil, fmt.Errorf("invalid root tenant id")
	}

	var entity model.TenantSandboxConfigEntity
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND deleted_at IS NULL", rootTenantID).
		Order("CASE WHEN name = '默认Docker沙箱' THEN 0 ELSE 1 END, created_at ASC, id ASC").
		First(&entity).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("query root sandbox: %w", err)
	}

	var cfgMap map[string]interface{}
	if entity.Config != "" {
		_ = json.Unmarshal([]byte(entity.Config), &cfgMap)
	}
	if cfgMap == nil {
		cfgMap = make(map[string]interface{})
	}

	var image string
	var cpuLimit float64
	var memoryLimitMB int
	if dockerRaw, ok := cfgMap["docker"].(map[string]interface{}); ok {
		if v, ok := dockerRaw["image"].(string); ok {
			image = v
		}
		if v, ok := dockerRaw["cpu_limit"].(float64); ok {
			cpuLimit = v
		}
		if v, ok := dockerRaw["memory_limit_mb"].(float64); ok {
			memoryLimitMB = int(v)
		} else if v, ok := dockerRaw["memory_limit_mb"].(int); ok {
			memoryLimitMB = v
		}
	}
	if image == "" {
		if v, ok := cfgMap["image"].(string); ok {
			image = v
		}
	}

	var defaultTimeout int
	if v, ok := cfgMap["default_timeout_sec"].(float64); ok {
		defaultTimeout = int(v)
	} else if v, ok := cfgMap["default_timeout_sec"].(int); ok {
		defaultTimeout = v
	} else if dockerRaw, ok := cfgMap["docker"].(map[string]interface{}); ok {
		if v, ok := dockerRaw["default_timeout_sec"].(float64); ok {
			defaultTimeout = int(v)
		} else if v, ok := dockerRaw["default_timeout_sec"].(int); ok {
			defaultTimeout = v
		}
	}

	name := entity.Name
	if name == "" {
		name = "默认Docker沙箱"
	}
	sandboxType := entity.SandboxType
	if sandboxType == "" {
		sandboxType = "docker"
	}

	return &model.RootSandboxDTO{
		ID:                entity.ID,
		Name:              name,
		Description:       entity.Description,
		SandboxType:       sandboxType,
		Image:             image,
		CPULimit:          cpuLimit,
		MemoryLimitMB:     memoryLimitMB,
		DefaultTimeoutSec: defaultTimeout,
		UpdatedAt:         entity.UpdatedAt,
	}, nil
}

// UpdateRootSandboxConfig 更新或创建母空间沙箱配置
func (s *Storage) UpdateRootSandboxConfig(ctx context.Context, rootTenantID uint64, req model.UpdateSandboxReq) error {
	if rootTenantID == 0 {
		return fmt.Errorf("invalid root tenant id")
	}
	if err := req.Validate(); err != nil {
		return fmt.Errorf("validate sandbox req: %w", err)
	}

	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var entity model.TenantSandboxConfigEntity
		err := tx.Where("tenant_id = ? AND deleted_at IS NULL", rootTenantID).
			Order("CASE WHEN name = '默认Docker沙箱' THEN 0 ELSE 1 END, created_at ASC, id ASC").
			First(&entity).Error

		now := time.Now()
		if err == nil {
			var cfgMap map[string]interface{}
			if entity.Config != "" {
				_ = json.Unmarshal([]byte(entity.Config), &cfgMap)
			}
			if cfgMap == nil {
				cfgMap = make(map[string]interface{})
			}

			dockerMap, ok := cfgMap["docker"].(map[string]interface{})
			if !ok || dockerMap == nil {
				dockerMap = make(map[string]interface{})
			}
			dockerMap["image"] = req.Image
			dockerMap["cpu_limit"] = req.CPULimit
			dockerMap["memory_limit_mb"] = req.MemoryLimitMB
			cfgMap["docker"] = dockerMap
			cfgMap["default_timeout_sec"] = req.DefaultTimeoutSec
			if req.SandboxType != "" {
				cfgMap["sandbox_type"] = req.SandboxType
			}

			cfgBytes, err := json.Marshal(cfgMap)
			if err != nil {
				return fmt.Errorf("marshal sandbox config: %w", err)
			}

			updates := map[string]interface{}{
				"config":     string(cfgBytes),
				"updated_at": now,
			}
			if req.Name != "" {
				updates["name"] = req.Name
			}
			if req.Description != "" {
				updates["description"] = req.Description
			}
			if req.SandboxType != "" {
				updates["sandbox_type"] = req.SandboxType
			}

			return tx.Model(&model.TenantSandboxConfigEntity{}).
				Where("id = ?", entity.ID).
				Updates(updates).Error
		} else if errors.Is(err, gorm.ErrRecordNotFound) {
			name := req.Name
			if name == "" {
				name = "默认Docker沙箱"
			}
			sandboxType := req.SandboxType
			if sandboxType == "" {
				sandboxType = "docker"
			}
			cfgMap := map[string]interface{}{
				"sandbox_type":        sandboxType,
				"default_timeout_sec": req.DefaultTimeoutSec,
				"docker": map[string]interface{}{
					"image":           req.Image,
					"cpu_limit":       req.CPULimit,
					"memory_limit_mb": req.MemoryLimitMB,
				},
			}
			cfgBytes, err := json.Marshal(cfgMap)
			if err != nil {
				return fmt.Errorf("marshal sandbox config: %w", err)
			}

			newEntity := model.TenantSandboxConfigEntity{
				ID:          uuid.New().String(),
				TenantID:    rootTenantID,
				Name:        name,
				Description: req.Description,
				SandboxType: sandboxType,
				Config:      string(cfgBytes),
				CreatedAt:   now,
				UpdatedAt:   now,
			}
			return tx.Create(&newEntity).Error
		} else {
			return fmt.Errorf("query root sandbox: %w", err)
		}
	})
}

// SyncSpaceSandbox 将母空间沙箱基准配置安全同步到子空间（严格保留现有子空间记录主键 ID）
func (s *Storage) SyncSpaceSandbox(ctx context.Context, targetTenantID, rootTenantID uint64) error {
	if targetTenantID == 0 || rootTenantID == 0 || targetTenantID == rootTenantID {
		return nil
	}

	// 1. 获取母空间沙箱配置记录作为基准，若无记录直接返回 nil
	var rootEntity model.TenantSandboxConfigEntity
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND deleted_at IS NULL", rootTenantID).
		Order("CASE WHEN name = '默认Docker沙箱' THEN 0 ELSE 1 END, created_at ASC, id ASC").
		First(&rootEntity).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return fmt.Errorf("find root sandbox config: %w", err)
	}

	now := time.Now()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 2. 查询目标子空间未删除沙箱记录
		var targetEntity model.TenantSandboxConfigEntity
		targetErr := tx.
			Where("tenant_id = ? AND deleted_at IS NULL", targetTenantID).
			Order("CASE WHEN name = '默认Docker沙箱' THEN 0 ELSE 1 END, created_at ASC, id ASC").
			First(&targetEntity).Error

		if targetErr == nil {
			// 若目标子空间已存在沙箱记录：绝对保留其现有主键 id！
			// 仅更新其 sandbox_type、description 和 config（对齐母空间的 docker 镜像与配额）
			var rootCfgMap map[string]interface{}
			if rootEntity.Config != "" {
				_ = json.Unmarshal([]byte(rootEntity.Config), &rootCfgMap)
			}

			var targetCfgMap map[string]interface{}
			if targetEntity.Config != "" {
				_ = json.Unmarshal([]byte(targetEntity.Config), &targetCfgMap)
			}
			if targetCfgMap == nil {
				targetCfgMap = make(map[string]interface{})
			}

			if rootDocker, ok := rootCfgMap["docker"]; ok {
				targetCfgMap["docker"] = rootDocker
			}
			if rootTimeout, ok := rootCfgMap["default_timeout_sec"]; ok {
				targetCfgMap["default_timeout_sec"] = rootTimeout
			}
			if rootType, ok := rootCfgMap["sandbox_type"]; ok {
				targetCfgMap["sandbox_type"] = rootType
			}

			mergedBytes, err := json.Marshal(targetCfgMap)
			if err != nil {
				return fmt.Errorf("marshal merged sandbox config: %w", err)
			}

			updates := map[string]interface{}{
				"sandbox_type": rootEntity.SandboxType,
				"description":  rootEntity.Description,
				"config":       string(mergedBytes),
				"updated_at":   now,
			}
			return tx.Model(&model.TenantSandboxConfigEntity{}).
				Where("id = ?", targetEntity.ID).
				Updates(updates).Error
		} else if errors.Is(targetErr, gorm.ErrRecordNotFound) {
			// 若目标子空间不存在沙箱记录：生成新 UUID，插入一条记录给 targetTenantID
			name := rootEntity.Name
			if name == "" {
				name = "默认Docker沙箱"
			}
			newEntity := model.TenantSandboxConfigEntity{
				ID:          uuid.New().String(),
				TenantID:    targetTenantID,
				Name:        name,
				Description: rootEntity.Description,
				SandboxType: rootEntity.SandboxType,
				Config:      rootEntity.Config,
				CreatedAt:   now,
				UpdatedAt:   now,
			}
			return tx.Create(&newEntity).Error
		}

		return fmt.Errorf("query target sandbox config: %w", targetErr)
	})
}
