# weknora-tokendance 用户与空间配置同步管理界面实施计划

> **针对智能体工作者：** 必需子技能：使用 superpowers:executing-plans 按任务实施此计划。步骤使用复选框 (`- [ ]`) 语法进行追踪。

**目标：** 在 `weknora-tokendance` 服务中构建内置的 Web 管理控制台，实现空间状态监控、模型/存储配置同步看板、一键同步与管理员分配。

**架构：** 后端在 Gin 路由下增加 `/admin/api/*` 接口，复用现有 DB 连接与同步逻辑；前端采用 Go `embed.FS` 内嵌的现代 Vue 3 + Tailwind 单页控制台，挂载于 `/admin`。

**技术栈：** Go 1.22+, Gin, GORM/pgx, Vue 3, TailwindCSS, Heroicons.

---

### 文件结构
- `pkg/storage/admin.go`: 后端管理面板所需的数据层查询与操作（空间状态详情、用户检索、管理员赋予/撤销、存储后端同步）。
- `pkg/handler/admin.go`: 管理后台 RESTful API 处理器。
- `pkg/web/embed.go`: Go `embed.FS` 嵌入前端静态模板。
- `pkg/web/admin.html`: 前端单页管理控制台（HTML + Vue 3 + TailwindCSS）。
- `main.go`: 注册 `/admin` 与 `/admin/api` 路由。

---

### 任务分解

#### Task 1: 扩展 Storage 层支持管理后台查询与配置更新
- [ ] 在 `pkg/storage/admin.go` 中实现：
  - `GetOverviewStats(ctx)`: 统计总空间数、一级部门空间数、模型与存储同步就绪率。
  - `ListSpacesWithConfig(ctx, keyword string)`: 获取空间列表，联表查出模型数量、MinIO 状态、空间当前管理员姓名/工号。
  - `SyncSpaceStorage(ctx, tenantID uint64)`: 为指定空间补齐 MinIO 与 Local 存储后端。
  - `SearchUsers(ctx, keyword string, limit int)`: 搜索员工及其在租户中的角色。
  - `UpdateSpaceAdmin(ctx, tenantID uint64, userID string, action string)`: 添加或移除空间管理员角色。
- [ ] 验证编译通过。

#### Task 2: 增强 Syncer 保证存储配置自动同步
- [ ] 修改 `pkg/syncer/syncer.go` 中的 `syncDepartmentToTenant`：
  - 在创建/更新租户后，除了同步模型外，自动调用 `SyncSpaceStorage` 确保 MinIO 和 Local 存储后端一并注入。
- [ ] 验证编译通过。

#### Task 3: 实现管理后台 API 接口
- [ ] 在 `pkg/handler/admin.go` 中实现：
  - `GetOverview`: 返回大盘统计。
  - `ListSpaces`: 返回空间配置看板列表。
  - `TriggerSyncAll`: 异步触发全量组织架构、模型与存储同步。
  - `TriggerSyncSpace`: 针对单个空间立即同步模型与存储。
  - `ListUsers`: 用户模糊检索。
  - `UpdateSpaceAdmin`: 空间管理员设置/撤销。
- [ ] 注册到 `handler.go` 与 `main.go`。

#### Task 4: 构建并内嵌现代化管理前端界面
- [ ] 编写 `pkg/web/admin.html`：
  - 现代化仪表盘布局，含统计卡片、搜索筛选、空间配置列表。
  - 标签展示：模型同步状态（5/5 绿标 / 缺失红标）、存储引擎状态（MinIO 绿标 / 缺失黄标）。
  - 管理员头像/姓名标签与一键“设置管理员”弹窗。
  - “立即全量同步”与单空间“同步配置”交互按钮与 Loading / Toast 提示。
- [ ] 编写 `pkg/web/embed.go` 使用 `//go:embed admin.html` 嵌入资源。
- [ ] 在 `main.go` 挂载 `GET /admin` 返回页面。

#### Task 5: 编译、部署与端到端验证
- [ ] 交叉编译 `tokendance-gateway-linux`（linux/amd64）。
- [ ] 上传至服务器 `10.101.102.116` 并重启 `weknora-gateway` 容器。
- [ ] 验证 `http://10.101.102.116:3002/admin` 页面可正常加载与交互。
- [ ] 测试单空间同步与全量同步 API 功能。
