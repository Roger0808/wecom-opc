package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	WeCom    WeComConfig    `yaml:"wecom"`
	WeKnora  WeKnoraConfig  `yaml:"weknora"`
	Database DatabaseConfig `yaml:"database"`
	Sync         SyncConfig     `yaml:"sync"`
	GraphQL      GraphQLConfig  `yaml:"graphql"`
	SystemAESKey string         `yaml:"system_aes_key"`
}

type GraphQLConfig struct {
	Endpoint string `yaml:"endpoint"` // 统一账户中心 GraphQL 地址，如 http://omni-router.benlai.com/graphql
}

type ServerConfig struct {
	Port int `yaml:"port"`
}

type WeComConfig struct {
	CorpID         string `yaml:"corp_id"`
	AgentID        int64  `yaml:"agent_id,omitempty"` // 可选：扫码专用
	Secret         string `yaml:"secret"`             // 自建应用 Secret (用于 OAuth 换取身份)
	ContactSecret  string `yaml:"contact_secret"`     // 通讯录 Secret (用于拉取部门与员工)
	Token          string `yaml:"token"`              // 接收消息服务器配置 Token
	EncodingAESKey string `yaml:"encoding_aes_key"`   // 接收消息服务器配置 EncodingAESKey
	RedirectURI    string `yaml:"redirect_uri"`       // 回调地址
}

type WeKnoraConfig struct {
	BaseURL        string `yaml:"base_url"`         // 例如 http://127.0.0.1:3003 或 https://shtokendance.benlai.com
	JWTSecret      string `yaml:"jwt_secret"`       // 与 WeKnora 相同的 JWT_SECRET
	RootTenantID   uint64 `yaml:"root_tenant_id"`   // 全员共享租户空间 ID，如 10001 (本来生活)
	DefaultRole    string `yaml:"default_role"`     // 普通员工空间默认角色: contributor / viewer
	LeaderRole     string `yaml:"leader_role"`      // 部门主管空间角色: admin / owner
}

type DatabaseConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	DBName   string `yaml:"dbname"`
	SSLMode  string `yaml:"sslmode"`
}

type SyncConfig struct {
	EnableCron   bool   `yaml:"enable_cron"`
	CronSchedule string `yaml:"cron_schedule"` // 例如 "0 */30 * * * *" 每30分钟
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config yaml: %w", err)
	}

	if cfg.GraphQL.Endpoint == "" {
		cfg.GraphQL.Endpoint = "http://omni-router.benlai.com/graphql"
	}

	// 允许环境变量覆盖敏感项
	if envCorpID := os.Getenv("WECOM_CORP_ID"); envCorpID != "" {
		cfg.WeCom.CorpID = envCorpID
	}
	if envSecret := os.Getenv("WECOM_SECRET"); envSecret != "" {
		cfg.WeCom.Secret = envSecret
	}
	if envContactSecret := os.Getenv("WECOM_CONTACT_SECRET"); envContactSecret != "" {
		cfg.WeCom.ContactSecret = envContactSecret
	}
	if envToken := os.Getenv("WECOM_TOKEN"); envToken != "" {
		cfg.WeCom.Token = envToken
	}
	if envAESKey := os.Getenv("WECOM_ENCODING_AES_KEY"); envAESKey != "" {
		cfg.WeCom.EncodingAESKey = envAESKey
	}
	if envBaseURL := os.Getenv("WEKNORA_BASE_URL"); envBaseURL != "" {
		cfg.WeKnora.BaseURL = envBaseURL
	}
	if envJWTSecret := os.Getenv("WEKNORA_JWT_SECRET"); envJWTSecret != "" {
		cfg.WeKnora.JWTSecret = envJWTSecret
	}
	if envDBHost := os.Getenv("DB_HOST"); envDBHost != "" {
		cfg.Database.Host = envDBHost
	}
	if envDBUser := os.Getenv("DB_USER"); envDBUser != "" {
		cfg.Database.User = envDBUser
	}
	if envDBPass := os.Getenv("DB_PASSWORD"); envDBPass != "" {
		cfg.Database.Password = envDBPass
	}
	if envDBName := os.Getenv("DB_NAME"); envDBName != "" {
		cfg.Database.DBName = envDBName
	}
	if envSystemAESKey := os.Getenv("SYSTEM_AES_KEY"); envSystemAESKey != "" {
		cfg.SystemAESKey = envSystemAESKey
	}

	return &cfg, nil
}

// GetAESKey returns the 32-byte AES key configured for WeKnora database encryption.
// Returns nil if not set or not exactly 32 bytes.
func (c *Config) GetAESKey() []byte {
	if len(c.SystemAESKey) == 32 {
		return []byte(c.SystemAESKey)
	}
	return nil
}

