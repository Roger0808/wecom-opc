package storage

import (
	"context"
	"fmt"
	"time"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/model"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Storage struct {
	db      *gorm.DB // WeKnora 库连接（既有兼容桥与映射表）
	stateDB *gorm.DB // platform 自有状态库连接（迁移、审计、身份/授权/同步账本）
}

func postgresDSN(cfg config.DatabaseConfig) string {
	return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.DBName, cfg.SSLMode)
}

func NewStorage(cfg config.DatabaseConfig) (*Storage, error) {
	db, err := gorm.Open(postgres.Open(postgresDSN(cfg)), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}

	// 自动创建 mapping 表与 deployed_apps 表
	if err := db.AutoMigrate(&model.WeComDeptTenantMapping{}, &model.DeployedApp{}); err != nil {
		return nil, fmt.Errorf("auto migrate tables: %w", err)
	}

	return &Storage{db: db}, nil
}

// NewStateDB 打开 platform 自有状态库连接（与 WeKnora 库分离、独立账号）。
// 不执行 AutoMigrate：表结构全部由 migrations/ 内嵌 SQL 经 RunMigrations 显式版本化建立。
func NewStateDB(cfg config.DatabaseConfig) (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(postgresDSN(cfg)), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("connect state postgres: %w", err)
	}
	return db, nil
}

// WithStateDB 注入平台状态库连接并返回 s（主连接不变），便于启动时组装。
func (s *Storage) WithStateDB(stateDB *gorm.DB) *Storage {
	s.stateDB = stateDB
	return s
}

// NewStorageWithDB 使用现有的 gorm.DB 初始化 Storage 实例 (用于单元测试或自定义连接)
func NewStorageWithDB(db *gorm.DB) *Storage {
	return &Storage{db: db}
}

// NewStorageWithDBs 使用现有的主库/状态库 gorm.DB 初始化 Storage 实例 (用于单元测试)
func NewStorageWithDBs(db, stateDB *gorm.DB) *Storage {
	return &Storage{db: db, stateDB: stateDB}
}

// DB 返回底层 gorm.DB 实例
func (s *Storage) DB() *gorm.DB {
	return s.db
}

// StateDB 返回平台自有状态库连接（未配置时返回 nil）
func (s *Storage) StateDB() *gorm.DB {
	return s.stateDB
}

// GetUserByEmailOrUsername 查找用户
func (s *Storage) GetUserByEmailOrUsername(ctx context.Context, email, username string) (*model.WeKnoraUser, error) {
	var user model.WeKnoraUser
	err := s.db.WithContext(ctx).
		Where("email = ? OR username = ?", email, username).
		First(&user).Error
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// GetFirstActiveAdminUser 查找首个可用管理员用户（用于系统向 knowledge-opc 签发技能同步 Token）
func (s *Storage) GetFirstActiveAdminUser(ctx context.Context) (*model.WeKnoraUser, error) {
	if s.db == nil {
		return nil, fmt.Errorf("main db not configured")
	}
	var user model.WeKnoraUser
	err := s.db.WithContext(ctx).
		Where("is_active = ?", true).
		Order("is_system_admin DESC, created_at ASC").
		First(&user).Error
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// UpsertUser 创建或更新用户
func (s *Storage) UpsertUser(ctx context.Context, user *model.WeKnoraUser) error {
	var existing model.WeKnoraUser
	err := s.db.WithContext(ctx).
		Where("email = ? OR username = ?", user.Email, user.Username).
		First(&existing).Error
	if err == nil {
		user.ID = existing.ID
		return s.db.WithContext(ctx).Model(&existing).Updates(map[string]interface{}{
			"username":   user.Username,
			"avatar":     user.Avatar,
			"is_active":  true,
			"updated_at": time.Now(),
		}).Error
	}
	return s.db.WithContext(ctx).Create(user).Error
}

// FindTenantByWeComDeptID 根据企微部门 ID 查租户映射
func (s *Storage) FindTenantByWeComDeptID(ctx context.Context, deptID int64) (*model.WeComDeptTenantMapping, error) {
	var mapping model.WeComDeptTenantMapping
	err := s.db.WithContext(ctx).Where("we_com_dept_id = ?", deptID).First(&mapping).Error
	if err != nil {
		return nil, err
	}
	return &mapping, nil
}

// FindTenantByDeptCode 根据统一账户中心部门编码查租户映射
func (s *Storage) FindTenantByDeptCode(ctx context.Context, deptCode string) (*model.WeComDeptTenantMapping, error) {
	var mapping model.WeComDeptTenantMapping
	err := s.db.WithContext(ctx).Where("department_code = ?", deptCode).First(&mapping).Error
	if err != nil {
		return nil, err
	}
	return &mapping, nil
}

// SaveDeptTenantMapping 保存部门与租户空间的映射 (Upsert 保证幂等)
func (s *Storage) SaveDeptTenantMapping(ctx context.Context, mapping *model.WeComDeptTenantMapping) error {
	var existing model.WeComDeptTenantMapping
	query := s.db.WithContext(ctx)
	if mapping.DepartmentCode != "" && mapping.WeComDeptID > 0 {
		query = query.Where("department_code = ? OR we_com_dept_id = ?", mapping.DepartmentCode, mapping.WeComDeptID)
	} else if mapping.DepartmentCode != "" {
		query = query.Where("department_code = ?", mapping.DepartmentCode)
	} else {
		query = query.Where("we_com_dept_id = ?", mapping.WeComDeptID)
	}

	err := query.First(&existing).Error
	if err == nil {
		mapping.ID = existing.ID
		return s.db.WithContext(ctx).Save(mapping).Error
	}
	return s.db.WithContext(ctx).Create(mapping).Error
}

// ListWeKnoraTenantNames 批量读取 WeKnora 空间显示名（WORK-RESOURCES-001
// DATA-101：列表 tenant_name 字段；best-effort，缺失/失败由调用方回退 ID）。
func (s *Storage) ListWeKnoraTenantNames(ctx context.Context, ids []uint64) (map[uint64]string, error) {
	out := make(map[uint64]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var rows []model.WeKnoraTenant
	if err := s.db.WithContext(ctx).
		Select("id", "name").
		Where("id IN ?", ids).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.ID] = row.Name
	}
	return out, nil
}

// CreateTenantIfNotExists 创建空间
func (s *Storage) CreateTenantIfNotExists(ctx context.Context, tenant *model.WeKnoraTenant) error {
	var existing model.WeKnoraTenant
	err := s.db.WithContext(ctx).Where("id = ?", tenant.ID).First(&existing).Error
	if err == nil {
		return s.db.WithContext(ctx).Model(&existing).Updates(map[string]interface{}{
			"name":        tenant.Name,
			"description": tenant.Description,
			"updated_at":  time.Now(),
		}).Error
	}
	return s.db.WithContext(ctx).Create(tenant).Error
}

// EnsureTenantMembership 确保用户在指定空间中拥有成员身份
func (s *Storage) EnsureTenantMembership(ctx context.Context, userID string, tenantID uint64, role string) error {
	var member model.WeKnoraTenantMember
	err := s.db.WithContext(ctx).
		Where("user_id = ? AND tenant_id = ?", userID, tenantID).
		First(&member).Error
	if err == nil {
		// 已存在成员，保持既有角色，不进行意外覆盖
		return nil
	}

	newMember := model.WeKnoraTenantMember{
		UserID:    userID,
		TenantID:  tenantID,
		Role:      role,
		Status:    "active",
		JoinedAt:  time.Now(),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	return s.db.WithContext(ctx).Create(&newMember).Error
}

// CreateAuthToken 记录颁发的 Token 到数据库（与 WeKnora 校验规则对齐）
func (s *Storage) CreateAuthToken(ctx context.Context, token *model.WeKnoraAuthToken) error {
	return s.db.WithContext(ctx).Create(token).Error
}

// GetTenantMembershipRole 返回用户在指定空间的成员角色（code-exchange 空间视图用）；
// 无成员关系或非 active 时返回空串。
func (s *Storage) GetTenantMembershipRole(ctx context.Context, userID string, tenantID uint64) (string, error) {
	if s.db == nil {
		return "", fmt.Errorf("main db not configured")
	}
	var member model.WeKnoraTenantMember
	err := s.db.WithContext(ctx).
		Where("user_id = ? AND tenant_id = ? AND status = 'active'", userID, tenantID).
		First(&member).Error
	if err != nil {
		return "", nil // 无成员关系不视为错误：角色置空
	}
	return member.Role, nil
}

// GetModelsByTenantID 获取指定租户的所有有效模型
func (s *Storage) GetModelsByTenantID(ctx context.Context, tenantID uint64) ([]model.WeKnoraModel, error) {
	var list []model.WeKnoraModel
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND deleted_at IS NULL", tenantID).
		Find(&list).Error
	return list, err
}

// SyncTenantModelsFromParent 将父共享空间的 LLM Provider / Model 所有的配置同步至目标部门空间
func (s *Storage) SyncTenantModelsFromParent(ctx context.Context, parentTenantID, targetTenantID uint64) error {
	if parentTenantID == 0 || targetTenantID == 0 || parentTenantID == targetTenantID {
		return nil
	}

	parentModels, err := s.GetModelsByTenantID(ctx, parentTenantID)
	if err != nil {
		return fmt.Errorf("fetch parent models: %w", err)
	}
	if len(parentModels) == 0 {
		return nil
	}

	targetModels, err := s.GetModelsByTenantID(ctx, targetTenantID)
	if err != nil {
		return fmt.Errorf("fetch target models: %w", err)
	}

	// 按 Type + Name 建立索引
	targetModelMap := make(map[string]model.WeKnoraModel, len(targetModels))
	for _, m := range targetModels {
		key := fmt.Sprintf("%s:%s", m.Type, m.Name)
		targetModelMap[key] = m
	}

	now := time.Now()
	for _, pm := range parentModels {
		key := fmt.Sprintf("%s:%s", pm.Type, pm.Name)
		if existing, exists := targetModelMap[key]; exists {
			// 更新既有模型配置，保持 Provider / parameters / key / 属性完全同步
			err := s.db.WithContext(ctx).Model(&model.WeKnoraModel{}).
				Where("id = ?", existing.ID).
				Updates(map[string]interface{}{
					"display_name": pm.DisplayName,
					"source":       pm.Source,
					"description":  pm.Description,
					"parameters":   pm.Parameters,
					"is_default":   pm.IsDefault,
					"is_builtin":   pm.IsBuiltin,
					"managed_by":   pm.ManagedBy,
					"status":       pm.Status,
					"updated_at":   now,
				}).Error
			if err != nil {
				return fmt.Errorf("update target model %s (%s): %w", pm.Name, pm.Type, err)
			}
		} else {
			// 新增模型
			newModel := model.WeKnoraModel{
				ID:          uuid.New().String(),
				TenantID:    targetTenantID,
				Name:        pm.Name,
				DisplayName: pm.DisplayName,
				Type:        pm.Type,
				Source:      pm.Source,
				Description: pm.Description,
				Parameters:  pm.Parameters,
				IsDefault:   pm.IsDefault,
				IsBuiltin:   pm.IsBuiltin,
				ManagedBy:   pm.ManagedBy,
				Status:      pm.Status,
				CreatedAt:   now,
				UpdatedAt:   now,
			}
			if err := s.db.WithContext(ctx).Create(&newModel).Error; err != nil {
				return fmt.Errorf("create target model %s (%s): %w", pm.Name, pm.Type, err)
			}
		}
	}

	// 保持与母空间严格一致：软删除子空间中母空间未配置的多余模型（如旧版本废弃模型）
	parentKeyMap := make(map[string]bool, len(parentModels))
	for _, pm := range parentModels {
		parentKeyMap[fmt.Sprintf("%s:%s", pm.Type, pm.Name)] = true
	}
	for _, tm := range targetModels {
		key := fmt.Sprintf("%s:%s", tm.Type, tm.Name)
		if !parentKeyMap[key] {
			_ = s.db.WithContext(ctx).Where("id = ?", tm.ID).Delete(&model.WeKnoraModel{}).Error
		}
	}

	return nil
}
