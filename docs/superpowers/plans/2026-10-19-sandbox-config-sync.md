# 沙箱环境配置同步与管理实施计划

> **针对智能体工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 按任务实施此计划。步骤使用复选框 (`- [ ]`) 语法进行追踪。

**目标：** 实现 WeKnora 沙箱环境（Sandbox）在母空间的配置维护、Web 控制台抽屉 Tab 展示，以及保留子空间现存 ID 的级联广播同步到全量 63+ 部门空间。

**架构：**
1. 在 `pkg/model` 中新增 `TenantSandboxConfigEntity` 和 `RootSandboxDTO` / `UpdateSandboxReq`。
2. 在 `pkg/storage` 中实现 `GetRootSandboxConfig`、`UpdateRootSandboxConfig` 与 `SyncSpaceSandbox`，并挂载至 `BroadcastRootConfigToAllTenants` 与 `SyncAll`。
3. 在 `pkg/handler` 与 `main.go` 中注册 `GET/PUT /admin/api/root-config/sandbox` 路由。
4. 在 `pkg/web/admin.html` 的母空间抽屉中添加 Tab 3「🛠️ 沙箱环境基准配置」。

**技术栈：** Go 1.22+, GORM, Gin, PostgreSQL, Vue 3, Tailwind CSS

---

### 任务 1：扩展数据模型与沙箱 DTO

**文件：**
- 修改：`/Users/fangqq/Code/weknora-tokendance/pkg/model/weknora.go`
- 创建：`/Users/fangqq/Code/weknora-tokendance/pkg/model/sandbox_test.go`

- [x] **步骤 1：编写 DTO 转换与验证单元测试**
  在 `pkg/model/sandbox_test.go` 中编写单元测试，验证 `RootSandboxDTO` 的 JSON 序列化与反序列化，以及配置合法性校验。

```go
package model_test

import (
	"encoding/json"
	"testing"
	"time"

	"weknora-tokendance/pkg/model"
)

func TestRootSandboxDTO_JSON(t *testing.T) {
	dto := model.RootSandboxDTO{
		ID:                "test-uuid",
		Name:              "默认Docker沙箱",
		Description:       "测试沙箱环境",
		SandboxType:       "docker",
		Image:             "wechatopenai/weknora-sandbox:latest",
		CPULimit:          2.0,
		MemoryLimitMB:     2048,
		DefaultTimeoutSec: 120,
		UpdatedAt:         time.Now(),
	}

	data, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var parsed model.RootSandboxDTO
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if parsed.Image != dto.Image || parsed.CPULimit != 2.0 || parsed.MemoryLimitMB != 2048 {
		t.Errorf("parsed dto mismatch: %+v", parsed)
	}
}
```

- [x] **步骤 2：在 `pkg/model/weknora.go` 中添加结构体定义**
  添加 `TenantSandboxConfigEntity`、`RootSandboxDTO`、`UpdateSandboxReq`，并在 `RootConfigDTO` 中新增 `Sandbox *RootSandboxDTO` 字段。

```go
// TenantSandboxConfigEntity 映射 public.tenant_sandbox_configs
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

// RootSandboxDTO 供前端交互与大盘使用
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
```

- [x] **步骤 3：运行测试并验证通过**
  运行：`go test ./pkg/model -v`

---

### 任务 2：在 Storage 层实现沙箱配置获取、更新与安全同步

**文件：**
- 修改：`/Users/fangqq/Code/weknora-tokendance/pkg/storage/admin.go`
- 修改：`/Users/fangqq/Code/weknora-tokendance/pkg/storage/storage.go` (如接口声明)
- 创建：`/Users/fangqq/Code/weknora-tokendance/pkg/storage/admin_sandbox_test.go`

- [x] **步骤 1：编写 Storage 沙箱方法单元测试**
  在 `pkg/storage/admin_sandbox_test.go` 中，使用 SQLite 模拟 `tenant_sandbox_configs`，测试：
  1. `GetRootSandboxConfig` 在存在/空记录时的表现。
  2. `UpdateRootSandboxConfig` 正确写回 JSONB 配置。
  3. `SyncSpaceSandbox` 在目标空间已存在沙箱时，保留其原有 `id`，仅更新配置；在目标空间不存在时新建一条记录。

- [x] **步骤 2：在 `pkg/storage/admin.go` 中实现核心逻辑**
  1. `GetRootSandboxConfig(ctx context.Context, rootTenantID uint64) (*model.RootSandboxDTO, error)`
  2. `UpdateRootSandboxConfig(ctx context.Context, rootTenantID uint64, req model.UpdateSandboxReq) error`
  3. `SyncSpaceSandbox(ctx context.Context, targetTenantID, rootTenantID uint64) error`
  4. 修改 `GetRootSpaceConfig`：将查询到的 `Sandbox` 填入 `RootConfigDTO`。
  5. 修改 `BroadcastRootConfigToAllTenants`：在循环中对每个 `tid` 调用 `s.SyncSpaceSandbox(ctx, tid, rootTenantID)`。
  6. 修改 `SyncAll`：同步每个部门空间时，一并调用 `s.SyncSpaceSandbox`。

- [x] **步骤 3：运行测试并验证通过**
  运行：`go test ./pkg/storage -run TestSandbox -v`

---

### 任务 3：在 Handler 层注册沙箱 API 路由

**文件：**
- 修改：`/Users/fangqq/Code/weknora-tokendance/pkg/handler/admin.go`
- 修改：`/Users/fangqq/Code/weknora-tokendance/main.go`
- 创建：`/Users/fangqq/Code/weknora-tokendance/pkg/handler/admin_sandbox_test.go`

- [x] **步骤 1：编写 Handler 单元测试**
  在 `pkg/handler/admin_sandbox_test.go` 中测试 `AdminGetRootSandbox` 与 `AdminUpdateRootSandbox` 接口状态码与返回结果。

- [x] **步骤 2：在 `pkg/handler/admin.go` 中添加 Handler 方法**
  - `AdminGetRootSandbox(c *gin.Context)`
  - `AdminUpdateRootSandbox(c *gin.Context)`

- [x] **步骤 3：在 `main.go` 中注册路由**
  ```go
  adminAPI.GET("/root-config/sandbox", h.AdminGetRootSandbox)
  adminAPI.PUT("/root-config/sandbox", h.AdminUpdateRootSandbox)
  ```

- [x] **步骤 4：运行测试并验证通过**
  运行：`go test ./pkg/handler -run TestAdminSandbox -v`

---

### 任务 4：在 Web 控制台抽屉中实现「沙箱环境」Tab 3 与交互

**文件：**
- 修改：`/Users/fangqq/Code/weknora-tokendance/pkg/web/admin.html`

- [x] **步骤 1：在抽屉顶部 Tab 栏添加 Tab 3**
  在 Tab 切换中增加：
  - `Tab 3: 🛠️ 沙箱环境基准配置 (Sandbox)`

- [x] **步骤 2：实现 Tab 3 的表单组件**
  - 沙箱类型展示（固定或选择 `docker`）
  - 容器镜像输入框 (`rootSandbox.image`)
  - CPU 配额输入框 (`rootSandbox.cpu_limit`)
  - 内存配额输入框 (`rootSandbox.memory_limit_mb`)
  - 默认超时输入框 (`rootSandbox.default_timeout_sec`)

- [x] **步骤 3：在 Vue 脚本中添加数据绑定与保存逻辑**
  - 在 `rootConfig` 中增加 `sandbox: { image: '', cpu_limit: 2, memory_limit_mb: 2048, default_timeout_sec: 120, sandbox_type: 'docker' }`
  - 新增 `saveRootSandbox()` 方法，请求 `PUT /admin/api/root-config/sandbox`
  - 底部「仅保存当前配置」根据 `activeRootTab === 'sandbox'` 路由调用 `saveRootSandbox()`

- [x] **步骤 4：本地浏览器或语法检查验证**
  检查 HTML、Vue 模板语法与 Tailwind 类名，确保无未闭合标签或脚本错误。

---

### 任务 5：端到端验证与编译部署上线

**文件：**
- 部署目标服务器：`10.101.102.116`
- 目标容器：`weknora-gateway`

- [x] **步骤 1：全量运行单元测试**
  在本地运行：`go test ./... -v`，确保全部单测通过。

- [x] **步骤 2：交叉编译 Linux 二进制**
  ```bash
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o tokendance-gateway-linux-amd64 main.go
  ```

- [x] **步骤 3：上传部署至服务器并重启网关容器**
  将编译后的二进制上传至 `10.101.102.116`，复制入 `weknora-gateway` 容器并重启：
  ```bash
  scp tokendance-gateway-linux-amd64 ecs-user@10.101.102.116:/tmp/
  ssh ecs-user@10.101.102.116 "sudo docker cp /tmp/tokendance-gateway-linux-amd64 weknora-gateway:/app/tokendance-gateway && sudo docker restart weknora-gateway"
  ```

- [x] **步骤 4：通过 API 触发一次全量级联广播同步，并验证数据库状态**
  1. 调用 `POST /admin/api/root-config/broadcast`。
  2. 查询 PostgreSQL `SELECT count(*) FROM tenant_sandbox_configs WHERE deleted_at IS NULL`，确认所有 63+ 个部门租户均拥有沙箱配置。
  3. 检查技术交付中心（296492）的沙箱配置：确认原 ID `518db26d-cb0a-4387-ba89-9bb85dcebe8b` 依然存在且其镜像等配置已平滑更新，已有智能体保持正常。
