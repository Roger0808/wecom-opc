package model

import (
	"time"

	"github.com/google/uuid"
)

const (
	AppStatusBuilding = "building"
	AppStatusRunning  = "running"
	AppStatusFailed   = "failed"
)

// AppConfigItem 表示单项环境变量应用配置
type AppConfigItem struct {
	Key         string `json:"key"`
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	HasValue    bool   `json:"has_value,omitempty"`
	ValueSource string `json:"value_source,omitempty"`
	SourceLabel string `json:"source_label,omitempty"`
	ResourceID  string `json:"resource_id,omitempty"`
}

// ResourceSummary 用于给前端展示线上可用的内网资源（敏感连接串脱敏，仅返回包含的环境变量 key）
type ResourceSummary struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Type        string            `json:"type"` // mysql, postgres, redis, oss 等
	Description string            `json:"description"`
	EnvKeys     []string          `json:"env_keys"`
	EnvPreview  map[string]string `json:"env_preview"`
}

// DeployedApp 对应 deployed_apps 数据库表，记录应用状态、配置与技能同步情况
type DeployedApp struct {
	ID              uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	Name            string    `gorm:"type:varchar(128);not null" json:"name"`
	Slug            string    `gorm:"type:varchar(64);uniqueIndex;not null" json:"slug"`
	Description     string    `gorm:"type:text;not null;default:''" json:"description"`
	AppURL          string    `gorm:"type:varchar(255);not null" json:"app_url"`
	ContainerPort   int       `gorm:"type:integer;not null;default:8080" json:"container_port"`
	Status          string    `gorm:"type:varchar(32);index;not null;default:'building'" json:"status"` // building | running | failed
	AppConfigJSON   string    `gorm:"type:text;not null;default:'[]'" json:"app_config_json"`           // JSON 字符串: []AppConfigItem
	ResourceIDsJSON string    `gorm:"type:text;not null;default:'[]'" json:"resource_ids_json"`         // JSON 字符串: []string
	SkillName       string    `gorm:"type:varchar(128);not null;default:''" json:"skill_name"`          // 技能名: app-<slug>
	CatalogSkillID  string    `gorm:"type:varchar(128);not null;default:''" json:"catalog_skill_id"`    // knowledge-opc 技能 Catalog ID
	SkillSynced     bool      `gorm:"type:boolean;not null;default:false" json:"skill_synced"`          // 是否已同步至 knowledge-opc
	BuildLog        string    `gorm:"type:text;not null;default:''" json:"build_log"`                   // 构建与部署日志
	ErrorMessage    string    `gorm:"type:text;not null;default:''" json:"error_message"`               // 失败原因
	CreatedBy       string    `gorm:"type:varchar(128);not null;default:''" json:"created_by"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func (DeployedApp) TableName() string {
	return "deployed_apps"
}
