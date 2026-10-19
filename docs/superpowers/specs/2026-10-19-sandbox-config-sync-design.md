# 沙箱环境配置同步与管理设计规范 (Spec)

## 1. 背景与目标
在 WeKnora 多租户（空间）体系中，母空间（Tenant ID: `10001`）维护了标准的基准配置。除了 LLM 模型与 MinIO 对象存储之外，智能体代码执行与脚本运行所依赖的「沙箱环境」（Sandbox）配置同样需要支持在管理控制台统一维护，并级联同步下发至全量 63+ 个部门子空间。

### 核心目标
1. **母空间沙箱基准可维护**：在「母空间基准配置」抽屉中新增沙箱 Tab，支持查看并编辑 Docker 沙箱镜像及资源配额（CPU、内存、超时时间）。
2. **级联广播与自动下发**：提供一键广播及定时任务同步机制，将母空间的沙箱配置同步至所有部门子空间。
3. **现有智能体 ID 保持安全性**：子空间已有沙箱配置时，严格保留原有 `id` 主键，仅更新参数，确保已绑定该沙箱的自定义智能体（如数字员工）不会因外键/ID 变动而失效。

---

## 2. 数据结构设计

### 2.1 实体映射与 DTO (`pkg/model/weknora.go`)

```go
// TenantSandboxConfig 对应 WeKnora public.tenant_sandbox_configs
type TenantSandboxConfigEntity struct {
    ID          string     `gorm:"column:id;primaryKey" json:"id"`
    TenantID    uint64     `gorm:"column:tenant_id;not null" json:"tenant_id"`
    Name        string     `gorm:"column:name;not null" json:"name"`
    Description string     `gorm:"column:description" json:"description"`
    SandboxType string     `gorm:"column:sandbox_type;not null" json:"sandbox_type"`
    Config      string     `gorm:"column:config;type:jsonb;not null" json:"config"`
    CordonedAt  *time.Time `gorm:"column:cordoned_at" json:"cordoned_at"`
    CreatedAt   time.Time  `gorm:"column:created_at" json:"created_at"`
    UpdatedAt   time.Time  `gorm:"column:updated_at" json:"updated_at"`
    DeletedAt   *time.Time `gorm:"column:deleted_at" json:"deleted_at"`
}

func (TenantSandboxConfigEntity) TableName() string {
    return "tenant_sandbox_configs"
}

// RootSandboxDTO 用于管理后台前端交互
type RootSandboxDTO struct {
    ID                string    `json:"id"`
    Name              string    `json:"name"`
    Description       string    `json:"description"`
    SandboxType       string    `json:"sandbox_type"`       // 默认 "docker"
    Image             string    `json:"image"`              // 如 "wechatopenai/weknora-sandbox:latest"
    CPULimit          float64   `json:"cpu_limit"`          // 如 2.0
    MemoryLimitMB     int       `json:"memory_limit_mb"`    // 如 2048
    DefaultTimeoutSec int       `json:"default_timeout_sec"`// 默认执行超时（秒）
    UpdatedAt         time.Time `json:"updated_at"`
}

type UpdateSandboxReq struct {
    Name              string  `json:"name"`
    Description       string  `json:"description"`
    SandboxType       string  `json:"sandbox_type"`
    Image             string  `json:"image" binding:"required"`
    CPULimit          float64 `json:"cpu_limit"`
    MemoryLimitMB     int     `json:"memory_limit_mb"`
    DefaultTimeoutSec int     `json:"default_timeout_sec"`
}
```

并在 `RootConfigDTO` 中新增 `Sandbox *RootSandboxDTO` 字段。

---

## 3. Storage 存储层设计 (`pkg/storage/admin.go`)

### 3.1 获取母空间沙箱配置
* 方法签名：`GetRootSandboxConfig(ctx context.Context, rootTenantID uint64) (*RootSandboxDTO, error)`
* 逻辑：
  1. 查询 `tenant_sandbox_configs` 表中 `tenant_id = rootTenantID AND deleted_at IS NULL`，优先取 `name = '默认Docker沙箱'` 或第一条活跃记录。
  2. 解析 `config` JSONB，提取 `docker.image`、`docker.cpu_limit`、`docker.memory_limit_mb` 及 `default_timeout_sec`。
  3. 组装成 `RootSandboxDTO` 返回。

### 3.2 更新母空间沙箱配置
* 方法签名：`UpdateRootSandboxConfig(ctx context.Context, rootTenantID uint64, req UpdateSandboxReq) error`
* 逻辑：
  1. 查询母空间已有记录。
  2. 若不存在，生成新 UUID，初始化一条默认沙箱记录。
  3. 若存在，保留原 `skill_image` 等扩展字段（如有），更新 `docker.image`、`docker.cpu_limit`、`docker.memory_limit_mb` 等核心参数，写回数据库并更新 `updated_at`。

### 3.3 部门子空间沙箱级联同步
* 方法签名：`SyncSpaceSandbox(ctx context.Context, targetTenantID, rootTenantID uint64) error`
* 核心保障逻辑：
  1. 获取母空间最新的有效沙箱记录作为基准。
  2. 查询目标空间 `targetTenantID` 的未删除沙箱记录：
     - **情况 1：目标空间已存在沙箱配置**
       - 严格保留目标记录的原有 `id`，防止破坏已有自定义智能体的 `sandbox_config_id` 引用。
       - 提取母空间基准的 `docker` 配置及 `sandbox_type`，覆盖更新目标记录的 `config.docker`、`sandbox_type`、`description`。
       - 保存更新并记录更新时间。
     - **情况 2：目标空间尚无沙箱配置**
       - 生成新 UUID，插入一条对应 `targetTenantID` 的 `默认Docker沙箱` 记录，`config` 继承母空间模板。

### 3.4 广播与定时任务串联
* 在 `BroadcastRootConfigToAllTenants` 中增加沙箱同步：
  ```go
  // 3. 同步沙箱配置
  if err := s.syncDepartmentSandbox(ctx, rootTenantID, tid); err != nil {
      errs = append(errs, fmt.Errorf("sync sandbox to tenant %d: %w", tid, err))
      continue
  }
  ```
* 在 `SyncAll`（全量自动同步/定时 Cron 任务）中同样包含沙箱同步。

---

## 4. 路由与 API 设计 (`pkg/handler/admin.go` & `main.go`)

### 4.1 新增/更新路由
```go
adminAPI.GET("/root-config/sandbox", h.AdminGetRootSandbox)
adminAPI.PUT("/root-config/sandbox", h.AdminUpdateRootSandbox)
```
* `AdminGetRootSandbox`: 调用 `GetRootSandboxConfig`，返回标准 JSON。
* `AdminUpdateRootSandbox`: 校验参数，调用 `UpdateRootSandboxConfig`。
* 现有 `GET /admin/api/root-config`: 在其聚合数据中包含 `sandbox` 节点。
* 现有 `POST /admin/api/root-config/broadcast`: 自动串联包含沙箱的一键广播。

---

## 5. Web 管理前端设计 (`pkg/web/admin.html`)

### 5.1 UI 抽屉布局扩展
在「母空间基准配置」抽屉中添加第三个选项卡：
- **Tab 1: 🤖 LLM 模型基准配置 (Models)**
- **Tab 2: 📦 MinIO 存储基准配置 (Storage)**
- **Tab 3: 🛠️ 沙箱环境基准配置 (Sandbox)**

### 5.2 沙箱表单项
1. **沙箱类型**：展示为 `Docker`（带徽标或下拉单选）。
2. **容器镜像 (Image)**：输入框，默认 `wechatopenai/weknora-sandbox:latest`，带说明提示。
3. **CPU 配额 (Cores)**：数字输入框，默认 `2` 核。
4. **内存配额 (Memory)**：数字输入框，默认 `2048` MB。
5. **执行超时时间 (Timeout)**：数字输入框，默认 `120` 秒。

### 5.3 底部操作按钮
- 【仅保存当前配置】：在 Sandbox Tab 下调用 `PUT /admin/api/root-config/sandbox`。
- 【💾 保存并广播至全量子空间】：保存当前 Tab 配置后调用 `POST /admin/api/root-config/broadcast`，一键下发至所有子空间。
