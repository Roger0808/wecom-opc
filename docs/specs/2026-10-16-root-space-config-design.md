# 母空间（Root Tenant）基准配置管理设计规范

## 1. 概述与目标

在 WeKnora 多租户与企业微信组织架构中，空间 `10001`（本来生活）作为**母空间（Root Space / 基准租户）**。全部 63+ 个企微一级部门空间均继承并对齐母空间的底层基础设施配置：
- **LLM 模型配置**：包括 KnowledgeQA（GLM-5.3-Flash / minimax-m3）、Embedding（bge-m3）、Rerank（bge-reranker-v2-m3）的集群端点（`base_url`）与 API Key；
- **存储引擎配置**：包括 MinIO 对象存储的连接端点（`endpoint`）、Bucket、路径前缀及访问凭证（`access_key_id` / `secret_access_key`）。

本功能旨在 `weknora-tokendance` 管理控制台中提供针对母空间（10001）的可视化查看、修改与全量级联广播能力。

---

## 2. 界面交互设计 (Frontend UX)

### 2.1 顶部导航与快捷入口
- 在顶部控制台右上角增加 **「⚙️ 母空间基准配置 (ID: 10001)」** 按钮，并在空间概览指标区显示母空间当前生效的模型数与存储状态。
- 点击后滑出右侧大抽屉（或居中模态窗口），提供沉浸式配置体验。

### 2.2 抽屉主要分区
1. **Tab 1: 🤖 LLM 模型基准配置 (Models)**
   - 模型列表展示：展示模型名称（Name）、类型（Type: KnowledgeQA / Embedding / Rerank）、提供方（Provider）、端点（Base URL）、API Key（掩码保护）、默认模型标记（Default）；
   - 模型操作：
     - **编辑已有模型**：支持修改 Base URL、输入新 API Key（保留原 Key 或替换）、设置是否默认；
     - **添加新模型**：快捷添加新的商用/开源模型；
     - **下线/删除模型**：软删除不再使用的模型。
2. **Tab 2: 📦 MinIO 存储基准配置 (Storage)**
   - 查看与编辑 MinIO 参数：
     - `endpoint`（如 `minio:9000`）
     - `bucket_name`（如 `weknora`）
     - `path_prefix`（如 `weknora/`）
     - `access_key_id`（明文或掩码）
     - `secret_access_key`（输入新值或保留现有）
3. **底部操作栏**
   - **「保存母空间配置」**：仅更新母空间自身（10001）的数据库记录；
   - **「保存并级联广播至全量子空间」**：更新母空间后，立即异步遍历全部 63+ 个部门空间，自动对齐最新模型与存储配置，并在前端显示同步进度与结果。

---

## 3. 密钥安全与加解密设计 (Security & Cryptography)

与 WeKnora 底层原生机制保持 100% 兼容：
- 加密算法：`AES-256-GCM`，Base64URL 编码，密文前缀 `enc:v1:`；
- 系统秘钥：环境变量或配置中的 `SYSTEM_AES_KEY`（32 字节，当前为 运行时经环境变量注入（真实值不入库））；
- 读写策略：
  - 读：解密敏感字段用于脱敏展示（如显示前 6 位与后 4 位，其余掩码处理）；
  - 写：若用户未修改（传入掩码或空），保留原密文；若用户输入了新明文，自动执行 `AES-256-GCM` 加密生成 `enc:v1:...` 格式后入库。

---

## 4. 后端 API 规范 (API Specifications)

### 4.1 获取母空间配置
- **URL**: `GET /admin/api/root-config`
- **Response**:
```json
{
  "code": 0,
  "data": {
    "tenant_id": 10001,
    "name": "本来生活",
    "storage_quota": 10737418240,
    "models": [
      {
        "id": "bc3a8813-915c-4139-9296-d38f5302fdce",
        "name": "GLM-5.3-Flash",
        "type": "KnowledgeQA",
        "provider": "generic",
        "base_url": "https://zhenze-huhehaote.cmecloud.cn/v1",
        "api_key_masked": "enc:v1:ciE2...HIPaydWU",
        "is_default": true,
        "parameters": { ... }
      }
    ],
    "storage": {
      "id": "08d5304f-d9ef-4d85-9a1c-c05d6e4e0eee",
      "provider": "minio",
      "endpoint": "minio:9000",
      "bucket_name": "weknora",
      "path_prefix": "weknora/",
      "access_key_masked": "enc:v1:N6V0...ZCrU",
      "secret_key_masked": "enc:v1:RUD0...8JdQ"
    }
  }
}
```

### 4.2 保存/更新母空间模型
- **URL**: `PUT /admin/api/root-config/models`
- **Request Body**:
```json
{
  "models": [
    {
      "id": "bc3a8813-915c-4139-9296-d38f5302fdce",
      "name": "GLM-5.3-Flash",
      "type": "KnowledgeQA",
      "provider": "generic",
      "base_url": "https://zhenze-huhehaote.cmecloud.cn/v1",
      "api_key": "", // 空表示不修改原 key，输入新文本则自动加密
      "is_default": true,
      "deleted": false
    }
  ]
}
```

### 4.3 保存/更新母空间 MinIO 存储
- **URL**: `PUT /admin/api/root-config/storage`
- **Request Body**:
```json
{
  "endpoint": "minio:9000",
  "bucket_name": "weknora",
  "path_prefix": "weknora/",
  "access_key_id": "", // 空表示保持不变
  "secret_access_key": ""
}
```

### 4.4 级联广播至全量子空间
- **URL**: `POST /admin/api/root-config/broadcast`
- **Response**:
```json
{
  "code": 0,
  "message": "已成功将母空间最新配置级联下发至 63 个子空间"
}
```

---

## 5. 实施与验证路径
1. 在 `config.go` / `config.yaml` 增加 `system_aes_key`；
2. 封装 AES-256-GCM 加解密工具包 `pkg/crypto/crypto.go`；
3. 在 `pkg/storage/admin.go` 编写母空间配置读取、模型保存、存储保存、级联广播接口；
4. 在 `pkg/handler/admin.go` 中注册 HTTP 处理路由；
5. 在 `pkg/web/admin.html` 嵌入母空间抽屉交互；
6. 交叉编译为 Linux amd64 二进制，部署至 10.101.102.116，重启容器并验证端到端功能。
