# WeKnora-TokenDance 用户与空间配置同步管理界面设计规范 (Spec)

## 1. 概述
为 `weknora-tokendance` 网关扩展一套现代化的轻量级内置管理控制台，提供空间状态监控、LLM 模型与存储引擎同步状态看板、一键手动同步、以及用户与空间管理员分配管理功能。

## 2. 系统架构

### 2.1 部署形态
- 单二进制部署：HTML/JS/CSS 静态资源通过 Go `embed.FS` 内嵌在二进制程序中。
- 端口复用：直接监听现有网关端口 `3002`，访问路径为 `/admin`。
- API 路径：所有管理 API 路由挂载在 `/admin/api/...`。

### 2.2 后端数据与服务层
复用 `weknora-tokendance` 已有的组件：
- `pkg/storage`：操作 PostgreSQL (`weknora` 库)，读取/修改 `tenants`、`models`、`storage_backends`、`tenant_members`、`users` 等。
- `pkg/syncer`：调用现有的组织架构与模型同步逻辑，并增强存储引擎同步。
- `pkg/accountcenter`：用户中心信息查询。

## 3. API 接口定义

### 3.1 概览大盘
- `GET /admin/api/overview`
  - 响应：
    ```json
    {
      "code": 0,
      "data": {
        "total_tenants": 69,
        "dept_tenants": 63,
        "models_synced_tenants": 69,
        "storage_ready_tenants": 69,
        "cron_enabled": true,
        "cron_schedule": "0 */30 * * *",
        "last_sync_time": "2026-10-16 11:19:23",
        "root_tenant_id": 10001
      }
    }
    ```

### 3.2 空间与配置列表
- `GET /admin/api/spaces`
  - 查询参数：`keyword`（部门名/ID搜索）、`status`
  - 响应列表项：
    - `id`: 租户 ID (如 296492)
    - `name`: 部门名称 (如 技术交付中心)
    - `description`: 描述/层级路径
    - `level`: 部门层级
    - `models_count`: 已配置模型数
    - `expected_models_count`: 期望模型数 (父空间模型数, 如 5)
    - `has_minio_storage`: 是否已绑定 MinIO
    - `default_storage_id`: 默认存储 ID
    - `admins`: 当前空间管理员列表 `[{id, username, email}]`
    - `member_count`: 成员总数

### 3.3 同步操作
- `POST /admin/api/sync/all`
  - 触发全量同步（拉取组织架构、同步一级部门为空间、从根空间同步模型、补全 MinIO 存储后端配置）。
- `POST /admin/api/spaces/:id/sync`
  - 针对指定空间同步模型与存储引擎配置。

### 3.4 用户与管理员配置
- `GET /admin/api/users`
  - 查询参数：`keyword`（工号/姓名/邮箱/手机号）
  - 响应：用户列表及所在部门空间、角色。
- `POST /admin/api/spaces/:id/admins`
  - 请求体：`{"user_id": "...", "action": "add" | "remove"}`
  - 将用户添加为该空间 Admin 或撤销。

## 4. 前端界面设计
- 采用简洁高颜值的 TailwindCSS + Vue 3 (通过 CDN，内置完整独立单文件无需构建工具)。
- 顶部导航：服务状态、上次同步时间、刷新按钮、全量同步按钮。
- 指标卡：统计数据一目了然。
- 空间看板：
  - 过滤与搜索
  - 响应式表格
  - 状态气泡（模型同步率、MinIO存储状态）
  - 管理员头像与名字
  - 操作按钮：单空间同步、配置管理员
- 弹窗模块：快速搜索员工并设置为当前空间管理员。

## 5. 测试与验证
1. 本地代码编译通过（`go build`）。
2. 在服务器部署并访问 `http://10.101.102.116:3002/admin` 验证界面渲染。
3. 验证 API 数据交互：概览数据、空间列表筛选、单空间同步、管理员变更。
