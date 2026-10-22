package config

import (
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server       ServerConfig   `yaml:"server"`
	WeCom        WeComConfig    `yaml:"wecom"`
	WeKnora      WeKnoraConfig  `yaml:"weknora"`
	Database     DatabaseConfig `yaml:"database"`
	StateDB      DatabaseConfig `yaml:"statedb"`
	Sync         SyncConfig     `yaml:"sync"`
	GraphQL      GraphQLConfig  `yaml:"graphql"`
	Admin        AdminConfig    `yaml:"admin"`
	Auth         AuthConfig     `yaml:"auth"`
	Bridge       BridgeConfig   `yaml:"bridge"`
	OIDC         OIDCConfig     `yaml:"oidc"`
	Code         CodeConfig     `yaml:"code"`
	SystemAESKey string         `yaml:"system_aes_key"`
}

// CodeConfig code 后端推送通道（BND-110 撤权通知）配置。
// BaseURL 为 code 后端可达地址；ServiceToken 为与 code 侧共享的服务令牌
// （env 注入，常量时间比较）。未配置 BaseURL 时推送通道不注册。
type CodeConfig struct {
	BaseURL      string `yaml:"base_url"`
	ServiceToken string `yaml:"service_token"`
}

// BridgeConfig knowledge 兼容桥（TASK-P09 / ADR-004）配置。
// Key 是桥签发上游个人 token 的 HS256 签名密钥：生产值与 WeKnora JWTSecret
// 同源（knowledge ValidateToken 用同一 secret 验签），但作为桥专属凭据
// 独立注入，与 auth.key（平台会话）、admin.key（管理面）、weknora.jwt_secret
// 配置项分离，便于单独轮换与最小化暴露面。启动非空校验（main.go）。
type BridgeConfig struct {
	Key string `yaml:"key"`
}

// AuthConfig 集成认证接口组 (/integration/v1) 安全配置。
// Key 用于签发/校验平台自有认证体系 HS256 JWT（access/refresh/exchange），
// 与 Admin.Key、WeKnora JWTSecret 完全分离。
type AuthConfig struct {
	Key string `yaml:"key"`
}

// OIDCConfig knowledge 侧 OIDC 客户端 (/oidc/token、/oidc/userinfo) 安全配置。
// ClientID/ClientSecret 校验 knowledge 的令牌兑换请求；任一为空时两个端点保持
// 501 fail-closed（与历史占位行为一致），不签发任何令牌。OIDC access_token 用
// auth.key（HS256）签名，iss=platform-oidc，与平台自有令牌体系同源但独立 audience。
type OIDCConfig struct {
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
}

// AdminConfig 管理后台 (/admin/api) 安全配置。
// Key 用于签发/校验管理面 HS256 JWT，必须与 WeKnora JWTSecret 分离。
// AllowedOrigins 是允许携带浏览器 Origin/Referer 访问管理写接口的来源白名单；
// 空列表表示拒绝一切带浏览器来源头的写请求（无来源头的纯 API 调用仍放行，但记审计）。
type AdminConfig struct {
	Key            string   `yaml:"key"`
	AllowedOrigins []string `yaml:"allowed_origins"`
}

type GraphQLConfig struct {
	Endpoint string `yaml:"endpoint"` // 统一账户中心 GraphQL 地址，如 http://omni-router.benlai.com/graphql
}

type ServerConfig struct {
	Port int `yaml:"port"`
	// ExternalBaseURL 本服务对外可达的规范地址（如 https://gateway.example.com），
	// 是企微登录回跳与客户端 authorize_url 拼接的唯一事实源：回跳地址一律以它为准，
	// 不再信请求 Host / X-Forwarded-Host（可伪造）。env EXTERNAL_BASE_URL 覆盖；
	// 启动强制非空（main.go fail-fast）。
	ExternalBaseURL string `yaml:"external_base_url"`
}

type WeComConfig struct {
	CorpID             string `yaml:"corp_id"`
	AgentID            int64  `yaml:"agent_id,omitempty"`   // 可选：扫码专用
	Secret             string `yaml:"secret"`               // 自建应用 Secret (用于 OAuth 换取身份)
	ContactSecret      string `yaml:"contact_secret"`       // 通讯录 Secret (用于拉取部门与员工)
	Token              string `yaml:"token"`                // 接收消息服务器配置 Token
	EncodingAESKey     string `yaml:"encoding_aes_key"`     // 接收消息服务器配置 EncodingAESKey
	RedirectURI        string `yaml:"redirect_uri"`         // 回调地址
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"` // 仅调试用途：跳过企微 API TLS 证书校验，默认 false
}

type WeKnoraConfig struct {
	BaseURL      string `yaml:"base_url"`       // 例如 http://127.0.0.1:3003 或 https://shtokendance.benlai.com
	JWTSecret    string `yaml:"jwt_secret"`     // 与 WeKnora 相同的 JWT_SECRET
	RootTenantID uint64 `yaml:"root_tenant_id"` // 全员共享租户空间 ID，如 10001 (本来生活)
	DefaultRole  string `yaml:"default_role"`   // 普通员工空间默认角色: contributor / viewer
	LeaderRole   string `yaml:"leader_role"`    // 部门主管空间角色: admin / owner
}

type DatabaseConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	DBName   string `yaml:"dbname"`
	SSLMode  string `yaml:"sslmode"`
}

// SyncConfig 历史遗留展示字段（管理后台大盘）。cron 定时同步已按 INT-130/BND-130 移除，
// 本组配置不再驱动任何同步行为；新部署不要配置（见 config.example.yaml）。
type SyncConfig struct {
	EnableCron   bool   `yaml:"enable_cron"`
	CronSchedule string `yaml:"cron_schedule"`
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
	if !isHTTPSOrPrivateEndpoint(cfg.GraphQL.Endpoint) {
		// 仅告警，不改变行为：账户中心 endpoint 走明文 HTTP 且非内网地址时，请求内容可能被窃听或篡改
		log.Printf("[Config] WARNING: graphql.endpoint %q 既不是 HTTPS 也不是明确的内网地址，生产环境建议使用 HTTPS 或内网服务名", cfg.GraphQL.Endpoint)
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
	// 平台自有状态库（与 WeKnora 库分离、独立账号）；dbname 缺省为 weknora_gateway_state
	if cfg.StateDB.DBName == "" {
		cfg.StateDB.DBName = "weknora_gateway_state"
	}
	if envStateHost := os.Getenv("STATE_DB_HOST"); envStateHost != "" {
		cfg.StateDB.Host = envStateHost
	}
	if envStatePort := os.Getenv("STATE_DB_PORT"); envStatePort != "" {
		if port, err := strconv.Atoi(envStatePort); err == nil {
			cfg.StateDB.Port = port
		}
	}
	if envStateUser := os.Getenv("STATE_DB_USER"); envStateUser != "" {
		cfg.StateDB.User = envStateUser
	}
	if envStatePass := os.Getenv("STATE_DB_PASSWORD"); envStatePass != "" {
		cfg.StateDB.Password = envStatePass
	}
	if envStateName := os.Getenv("STATE_DB_NAME"); envStateName != "" {
		cfg.StateDB.DBName = envStateName
	}
	if envStateSSLMode := os.Getenv("STATE_DB_SSLMODE"); envStateSSLMode != "" {
		cfg.StateDB.SSLMode = envStateSSLMode
	}
	if envSystemAESKey := os.Getenv("SYSTEM_AES_KEY"); envSystemAESKey != "" {
		cfg.SystemAESKey = envSystemAESKey
	}
	if envAdminKey := os.Getenv("PLATFORM_ADMIN_KEY"); envAdminKey != "" {
		cfg.Admin.Key = envAdminKey
	}
	if envAuthKey := os.Getenv("PLATFORM_AUTH_KEY"); envAuthKey != "" {
		cfg.Auth.Key = envAuthKey
	}
	if envExternalBaseURL := os.Getenv("EXTERNAL_BASE_URL"); envExternalBaseURL != "" {
		cfg.Server.ExternalBaseURL = envExternalBaseURL
	}
	if envBridgeKey := os.Getenv("PLATFORM_BRIDGE_KEY"); envBridgeKey != "" {
		cfg.Bridge.Key = envBridgeKey
	}
	if envOIDCClientID := os.Getenv("OIDC_CLIENT_ID"); envOIDCClientID != "" {
		cfg.OIDC.ClientID = envOIDCClientID
	}
	if envOIDCClientSecret := os.Getenv("OIDC_CLIENT_SECRET"); envOIDCClientSecret != "" {
		cfg.OIDC.ClientSecret = envOIDCClientSecret
	}
	if envCodeBase := os.Getenv("CODE_BASE_URL"); envCodeBase != "" {
		cfg.Code.BaseURL = envCodeBase
	}
	if envCodeToken := os.Getenv("CODE_SERVICE_TOKEN"); envCodeToken != "" {
		cfg.Code.ServiceToken = envCodeToken
	}
	for i, origin := range cfg.Admin.AllowedOrigins {
		cfg.Admin.AllowedOrigins[i] = strings.TrimSpace(origin)
	}

	return &cfg, nil
}

// isHTTPSOrPrivateEndpoint 判断 endpoint 是否满足 https，或主机属于明确的内网/本机范围。
// 内网范围：loopback、RFC1918 私网 IP、IPv6 ULA、以及 .internal/.local/.corp/.lan/.svc 等内网域名后缀。
func isHTTPSOrPrivateEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" {
		return false
	}
	if strings.EqualFold(u.Scheme, "https") {
		return true
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "localhost" {
		return true
	}
	for _, suffix := range []string{".internal", ".local", ".corp", ".lan", ".svc", ".in-addr.arpa"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// GetAESKey returns the 32-byte AES key configured for WeKnora database encryption.
// Returns nil if not set or not exactly 32 bytes.
func (c *Config) GetAESKey() []byte {
	if len(c.SystemAESKey) == 32 {
		return []byte(c.SystemAESKey)
	}
	return nil
}
