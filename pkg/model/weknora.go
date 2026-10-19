package model

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
)

// WeKnoraUser 与 WeKnora 的 users 表对齐
type WeKnoraUser struct {
	ID                  string         `gorm:"column:id;type:varchar(36);primaryKey" json:"id"`
	Username            string         `gorm:"column:username;type:varchar(100);uniqueIndex;not null" json:"username"`
	Email               string         `gorm:"column:email;type:varchar(255);uniqueIndex;not null" json:"email"`
	PasswordHash        string         `gorm:"column:password_hash;type:varchar(255);not null" json:"-"`
	Avatar              string         `gorm:"column:avatar;type:varchar(500)" json:"avatar"`
	TenantID            uint64         `gorm:"column:tenant_id;index" json:"tenant_id"`
	IsActive            bool           `gorm:"column:is_active;default:true" json:"is_active"`
	CanAccessAllTenants bool           `gorm:"column:can_access_all_tenants;default:false" json:"can_access_all_tenants"`
	IsSystemAdmin       bool           `gorm:"column:is_system_admin;default:false;index" json:"is_system_admin"`
	Preferences         string         `gorm:"column:preferences;type:jsonb;not null;default:'{}'" json:"preferences"`
	CreatedAt           time.Time      `gorm:"column:created_at" json:"created_at"`
	UpdatedAt           time.Time      `gorm:"column:updated_at" json:"updated_at"`
	DeletedAt           gorm.DeletedAt `gorm:"column:deleted_at;index" json:"deleted_at"`
}

func (WeKnoraUser) TableName() string {
	return "users"
}

// WeKnoraTenant 与 WeKnora 的 tenants 表对齐
type WeKnoraTenant struct {
	ID                      uint64         `gorm:"column:id;primaryKey" json:"id"`
	Name                    string         `gorm:"column:name" json:"name"`
	Description             string         `gorm:"column:description" json:"description"`
	Status                  string         `gorm:"column:status;default:'active'" json:"status"`
	RetrieverEngines        string         `gorm:"column:retriever_engines;type:json" json:"retriever_engines"`
	Business                string         `gorm:"column:business" json:"business"`
	StorageQuota            int64          `gorm:"column:storage_quota;default:10737418240" json:"storage_quota"`
	StorageUsed             int64          `gorm:"column:storage_used;default:0" json:"storage_used"`
	DefaultStorageBackendID *string        `gorm:"column:default_storage_backend_id" json:"default_storage_backend_id"`
	CreatedAt               time.Time      `gorm:"column:created_at" json:"created_at"`
	UpdatedAt               time.Time      `gorm:"column:updated_at" json:"updated_at"`
	DeletedAt               gorm.DeletedAt `gorm:"column:deleted_at;index" json:"deleted_at"`
}

func (WeKnoraTenant) TableName() string {
	return "tenants"
}

// WeKnoraStorageBackend 与 WeKnora 的 storage_backends 表对齐
type WeKnoraStorageBackend struct {
	ID          string         `gorm:"column:id;type:varchar(36);primaryKey" json:"id"`
	TenantID    uint64         `gorm:"column:tenant_id;not null" json:"tenant_id"`
	Name        string         `gorm:"column:name;type:varchar(255);not null" json:"name"`
	Provider    string         `gorm:"column:provider;type:varchar(32);not null" json:"provider"`
	Config      string         `gorm:"column:config;type:jsonb;not null" json:"config"`
	Source      string         `gorm:"column:source;type:varchar(16);not null" json:"source"`
	Status      string         `gorm:"column:status;type:varchar(16);not null" json:"status"`
	LegacyAlias bool           `gorm:"column:legacy_alias;not null" json:"legacy_alias"`
	CreatedAt   time.Time      `gorm:"column:created_at" json:"created_at"`
	UpdatedAt   time.Time      `gorm:"column:updated_at" json:"updated_at"`
	DeletedAt   gorm.DeletedAt `gorm:"column:deleted_at;index" json:"deleted_at"`
}

func (WeKnoraStorageBackend) TableName() string {
	return "storage_backends"
}

// WeKnoraTenantMember 与 WeKnora 的 tenant_members 表对齐
type WeKnoraTenantMember struct {
	ID        uint64         `gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	UserID    string         `gorm:"column:user_id;type:varchar(36);not null;index" json:"user_id"`
	TenantID  uint64         `gorm:"column:tenant_id;not null;index" json:"tenant_id"`
	Role      string         `gorm:"column:role;type:varchar(20);not null;default:'contributor'" json:"role"`
	Status    string         `gorm:"column:status;type:varchar(20);not null;default:'active'" json:"status"`
	InvitedBy *string        `gorm:"column:invited_by;type:varchar(36)" json:"invited_by,omitempty"`
	JoinedAt  time.Time      `gorm:"column:joined_at" json:"joined_at"`
	CreatedAt time.Time      `gorm:"column:created_at" json:"created_at"`
	UpdatedAt time.Time      `gorm:"column:updated_at" json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at;index" json:"deleted_at"`
}

func (WeKnoraTenantMember) TableName() string {
	return "tenant_members"
}

// WeKnoraAuthToken 与 WeKnora 的 auth_tokens 表对齐
type WeKnoraAuthToken struct {
	ID        string    `gorm:"column:id;type:varchar(36);primaryKey" json:"id"`
	UserID    string    `gorm:"column:user_id;type:varchar(36);not null;index" json:"user_id"`
	Token     string    `gorm:"column:token;type:text;not null;index" json:"token"`
	TokenType string    `gorm:"column:token_type;type:varchar(20);not null" json:"token_type"`
	ExpiresAt time.Time `gorm:"column:expires_at;not null;index" json:"expires_at"`
	CreatedAt time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at" json:"updated_at"`
}

func (WeKnoraAuthToken) TableName() string {
	return "auth_tokens"
}

// WeComDeptTenantMapping 记录企业微信部门ID/统一账户中心编码与 WeKnora 租户ID的关联表
type WeComDeptTenantMapping struct {
	ID             uint64    `gorm:"primaryKey;autoIncrement" json:"id"`
	WeComDeptID    int64     `gorm:"column:we_com_dept_id;uniqueIndex;not null" json:"wecom_dept_id"`
	DepartmentCode string    `gorm:"column:department_code;type:varchar(64);index" json:"department_code"`
	TenantID       uint64    `gorm:"uniqueIndex;not null" json:"tenant_id"`
	DeptName       string    `gorm:"type:varchar(255)" json:"dept_name"`
	ParentID       int64     `json:"parent_id"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (WeComDeptTenantMapping) TableName() string {
	return "wecom_dept_tenant_mappings"
}

// WeKnoraModel 与 WeKnora 的 models 表对齐 (用于 LLM Provider 配置同步)
type WeKnoraModel struct {
	ID          string         `gorm:"column:id;type:varchar(64);primaryKey" json:"id"`
	TenantID    uint64         `gorm:"column:tenant_id;not null" json:"tenant_id"`
	Name        string         `gorm:"column:name;type:varchar(255);not null" json:"name"`
	DisplayName string         `gorm:"column:display_name;type:varchar(255);not null;default:''" json:"display_name"`
	Type        string         `gorm:"column:type;type:varchar(50);not null" json:"type"`
	Source      string         `gorm:"column:source;type:varchar(50);not null" json:"source"`
	Description string         `gorm:"column:description;type:text" json:"description"`
	Parameters  string         `gorm:"column:parameters;type:jsonb;not null" json:"parameters"`
	IsDefault   bool           `gorm:"column:is_default;not null;default:false" json:"is_default"`
	IsBuiltin   bool           `gorm:"column:is_builtin;not null;default:false" json:"is_builtin"`
	ManagedBy   string         `gorm:"column:managed_by;type:varchar(32);not null;default:''" json:"managed_by"`
	Status      string         `gorm:"column:status;type:varchar(50);not null;default:'active'" json:"status"`
	CreatedAt   time.Time      `gorm:"column:created_at" json:"created_at"`
	UpdatedAt   time.Time      `gorm:"column:updated_at" json:"updated_at"`
	DeletedAt   gorm.DeletedAt `gorm:"column:deleted_at;index" json:"deleted_at"`
}

func (WeKnoraModel) TableName() string {
	return "models"
}

// TenantSandboxConfigEntity 与 WeKnora 的 tenant_sandbox_configs 表对齐
type TenantSandboxConfigEntity struct {
	ID          string         `gorm:"column:id;type:varchar(36);primaryKey" json:"id"`
	TenantID    uint64         `gorm:"column:tenant_id;not null;index" json:"tenant_id"`
	Name        string         `gorm:"column:name;type:varchar(255);not null" json:"name"`
	Description string         `gorm:"column:description;type:text" json:"description"`
	SandboxType string         `gorm:"column:sandbox_type;type:varchar(32);not null" json:"sandbox_type"`
	Config      string         `gorm:"column:config;type:jsonb;not null" json:"config"`
	CordonedAt  *time.Time     `gorm:"column:cordoned_at" json:"cordoned_at"`
	CreatedAt   time.Time      `gorm:"column:created_at" json:"created_at"`
	UpdatedAt   time.Time      `gorm:"column:updated_at" json:"updated_at"`
	DeletedAt   gorm.DeletedAt `gorm:"column:deleted_at;index" json:"deleted_at"`
}

func (TenantSandboxConfigEntity) TableName() string {
	return "tenant_sandbox_configs"
}

// RootModelDTO 母空间 LLM 模型配置 DTO
type RootModelDTO struct {
	ID           string                 `json:"id"`
	Name         string                 `json:"name"`
	DisplayName  string                 `json:"display_name"`
	Type         string                 `json:"type"`     // KnowledgeQA, Embedding, Rerank
	Source       string                 `json:"source"`   // remote
	Provider     string                 `json:"provider"` // generic
	BaseURL      string                 `json:"base_url"`
	APIKeyMasked string                 `json:"api_key_masked"`
	IsDefault    bool                   `json:"is_default"`
	Status       string                 `json:"status"`
	Parameters   map[string]interface{} `json:"parameters"`
}

// RootStorageDTO 母空间存储配置 DTO
type RootStorageDTO struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Provider        string `json:"provider"` // minio
	Endpoint        string `json:"endpoint"`
	BucketName      string `json:"bucket_name"`
	PathPrefix      string `json:"path_prefix"`
	AccessKeyMasked string `json:"access_key_masked"`
	SecretKeyMasked string `json:"secret_key_masked"`
	Status          string `json:"status"`
}

// RootSandboxDTO 母空间沙箱配置 DTO
type RootSandboxDTO struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	Description       string    `json:"description"`
	SandboxType       string    `json:"sandbox_type"`
	Image             string    `json:"image"`
	CPULimit          float64   `json:"cpu_limit"`
	MemoryLimitMB     int       `json:"memory_limit_mb"`
	DefaultTimeoutSec int       `json:"default_timeout_sec"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// RootConfigDTO 母空间整体配置 DTO
type RootConfigDTO struct {
	TenantID    uint64          `json:"tenant_id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Models      []RootModelDTO  `json:"models"`
	Storage     *RootStorageDTO `json:"storage"`
	Sandbox     *RootSandboxDTO `json:"sandbox"`
}

// UpdateModelReq 更新/创建模型请求
type UpdateModelReq struct {
	ID          string                 `json:"id"`
	Name        string                 `json:"name"`
	DisplayName string                 `json:"display_name"`
	Type        string                 `json:"type"`
	Source      string                 `json:"source"`
	Provider    string                 `json:"provider"`
	BaseURL     string                 `json:"base_url"`
	APIKey      string                 `json:"api_key"`
	IsDefault   bool                   `json:"is_default"`
	Status      string                 `json:"status"`
	Parameters  map[string]interface{} `json:"parameters"`
	Deleted     bool                   `json:"deleted,omitempty"`
}

// UpdateStorageReq 更新存储请求
type UpdateStorageReq struct {
	Endpoint        string `json:"endpoint"`
	BucketName      string `json:"bucket_name"`
	PathPrefix      string `json:"path_prefix"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
}

// UpdateSandboxReq 保存沙箱配置请求
type UpdateSandboxReq struct {
	Name              string  `json:"name"`
	Description       string  `json:"description"`
	SandboxType       string  `json:"sandbox_type"`
	Image             string  `json:"image" binding:"required"`
	CPULimit          float64 `json:"cpu_limit"`
	MemoryLimitMB     int     `json:"memory_limit_mb"`
	DefaultTimeoutSec int     `json:"default_timeout_sec"`
}

// Validate 校验沙箱配置请求合法性
func (r *UpdateSandboxReq) Validate() error {
	if strings.TrimSpace(r.Image) == "" {
		return errors.New("image is required")
	}
	if r.CPULimit < 0 {
		return errors.New("cpu_limit must be non-negative")
	}
	if r.MemoryLimitMB < 0 {
		return errors.New("memory_limit_mb must be non-negative")
	}
	if r.DefaultTimeoutSec < 0 {
		return errors.New("default_timeout_sec must be non-negative")
	}
	return nil
}
