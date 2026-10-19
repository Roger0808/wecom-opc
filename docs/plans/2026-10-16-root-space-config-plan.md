# 母空间（Root Tenant）基准配置管理实施计划

> **针对智能体工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 按任务实施此计划。步骤使用复选框 (`- [ ]`) 语法进行追踪。

**目标：** 在 `weknora-tokendance` 控制台中实现母空间（ID: 10001）的 LLM 模型与 MinIO 存储配置的可视化查看、修改及一键级联广播下发。

**架构：** 
- 后端复用 WeKnora 原生 `AES-256-GCM`（`enc:v1:`）算法对模型 API Key 与 MinIO Secret 进行透明脱敏与安全加密入库；
- 扩展 `pkg/storage/admin.go` 提供母空间模型增删改、存储配置更新与子空间全量广播同步；
- 前端在 `admin.html` 中提供顶部常驻抽屉面板，支持直观的交互表单。

**技术栈：** Go 1.22+, GORM, Gin, Vue 3, Tailwind CSS.

---

### 任务 1：实现 AES-256-GCM 加解密工具包与配置支持

**文件：**
- 创建：`pkg/crypto/crypto.go`
- 创建：`pkg/crypto/crypto_test.go`
- 修改：`config/config.go`
- 修改：`config.yaml`

- [x] **步骤 1：编写加密与解密单元测试**
  测试验证 `EncryptAESGCM` 生成包含 `enc:v1:` 前缀的密文，`DecryptAESGCM` 能还原明文，且对已有 `enc:v1:` 密文具有幂等性。

- [x] **步骤 2：编写 `pkg/crypto/crypto.go` 实施方案**
  实现与 WeKnora 原生完全一致的 `EncryptAESGCM` 和 `DecryptAESGCM` 函数。

- [x] **步骤 3：在 `config/config.go` 中解析 `SystemAESKey`**
  优先读取环境变量 `SYSTEM_AES_KEY`，其次读取 `config.yaml` 中的 `system_aes_key`。

- [x] **步骤 4：运行测试并确认通过**
  执行 `go test ./pkg/crypto -v`，确保所有用例通过。

---

### 任务 2：在 Storage 层实现母空间配置的读取、修改与广播

**文件：**
- 修改：`pkg/storage/admin.go`
- 修改：`pkg/model/weknora.go`

- [x] **步骤 1：定义母空间 DTO 结构**
  定义 `RootModelDTO`、`RootStorageDTO`、`RootConfigDTO`，包含脱敏字段与完整参数结构。

- [x] **步骤 2：实现 `GetRootSpaceConfig`**
  从 `models` 和 `storage_backends` 查询母空间记录，解密凭证生成脱敏串返回。

- [x] **步骤 3：实现 `UpdateRootModels`**
  支持更新已有模型 Base URL、名称、默认状态；若传入新 API Key 则加密为 `enc:v1:` 后写入；支持新增模型与软删除。

- [x] **步骤 4：实现 `UpdateRootStorage`**
  更新母空间 MinIO 端点、Bucket、Prefix；若传入新 Secret Key 则加密后写入。

- [x] **步骤 5：实现 `BroadcastRootConfigToAllTenants`**
  复用 `pkg/syncer` 的同步逻辑，遍历所有部门租户，自动将母空间的模型与 MinIO 存储对齐。

---

### 任务 3：在 Handler 层注册 API 与路由

**文件：**
- 修改：`pkg/handler/admin.go`
- 修改：`main.go`

- [x] **步骤 1：编写 Handler 方法**
  - `AdminGetRootConfig`
  - `AdminUpdateRootModels`
  - `AdminUpdateRootStorage`
  - `AdminBroadcastRootConfig`

- [x] **步骤 2：在 `main.go` 注册路由**
  ```go
  adminAPI.GET("/root-config", h.AdminGetRootConfig)
  adminAPI.PUT("/root-config/models", h.AdminUpdateRootModels)
  adminAPI.PUT("/root-config/storage", h.AdminUpdateRootStorage)
  adminAPI.POST("/root-config/broadcast", h.AdminBroadcastRootConfig)
  ```

---

### 任务 4：在 Web 控制台实现母空间基准配置抽屉

**文件：**
- 修改：`pkg/web/admin.html`

- [x] **步骤 1：添加顶部「⚙️ 母空间基准配置」按钮**
  在导航栏右上角显著位置增加常驻按钮，点击打开抽屉。

- [x] **步骤 2：实现抽屉 UI 与 Vue 数据绑定**
  - Tab 1: LLM 模型基准配置列表、编辑模态窗、添加模型、删除模型；
  - Tab 2: MinIO 对象存储参数表单（Endpoint、Bucket、Access Key、Secret Key）；
  - 操作栏：「保存母空间配置」、「保存并立即广播至全量子空间」。

- [x] **步骤 3：联调前端与 API 交互**
  测试抽屉打开、保存、广播提示与通知。

---

### 任务 5：交叉编译、服务器部署与端到端验证

**文件：**
- 部署：`tokendance-gateway-linux` -> `10.101.102.116:/data/weknora-tokendance/tokendance-gateway`

- [x] **步骤 1：本地编译 Linux amd64 二进制**
  ```bash
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o tokendance-gateway-linux main.go
  ```

- [x] **步骤 2：上传并重启服务器容器**
  ```bash
  scp tokendance-gateway-linux ecs-user@10.101.102.116:/data/weknora-tokendance/tokendance-gateway.new
  ssh ecs-user@10.101.102.116 "mv /data/weknora-tokendance/tokendance-gateway.new /data/weknora-tokendance/tokendance-gateway && chmod +x /data/weknora-tokendance/tokendance-gateway && sudo docker restart weknora-gateway"
  ```

- [x] **步骤 3：在线验证母空间配置读取、修改与广播下发**
  通过 curl 测试接口返回，并在浏览器访问 `http://10.101.102.116:3002/admin` 验证抽屉交互。
