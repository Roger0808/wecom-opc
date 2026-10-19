# WeKnora TokenDance Gateway (`weknora-tokendance-gateway`)

企业微信组织架构同步与 OAuth2 免密认证网关，专为 WeKnora 知识库系统打造，实现：
1. **部门作为空间（Tenant）**：全自动同步企微部门树到 WeKnora 租户空间，实现部门知识库自然物理隔离。
2. **员工关系映射（TenantMember）**：按企微人员所属部门自动分配空间读写/管理权限，支持跨部门与全员公共空间。
3. **企微扫码/网页免密登录（WeCom OAuth2）**：企微一键登录，直接置换 WeKnora JWT Token 自动跳转进站。
4. **架构零侵入**：通过外层 Nginx 与独立网关解耦，无需修改任何 WeKnora 官方源码，无缝支持 WeKnora 升级。

---

## 目录结构规划

```
weknora-tokendance/
├── README.md                 # 项目设计与使用说明
├── config.example.yaml       # 配置文件模板 (企微凭据, WeKnora 数据库/API/JWT 密钥)
├── docker-compose.yaml       # 容器化部署
├── Dockerfile                # 构建文件
├── go.mod                    # Go 依赖定义
├── main.go                   # 服务主入口
├── api/                      # HTTP 路由与控制器 (企微 OAuth、回调、健康检查)
├── core/                     # 业务核心逻辑
│   ├── wecom/                # 企微 API 通信 (通讯录拉取、OAuth 换取 user_ticket)
│   ├── syncer/               # 部门与人员到 WeKnora 的同步管道
│   └── auth/                 # WeKnora JWT 签发与租户上下文装配
└── storage/                  # 数据库操作 (PostgreSQL直连 或 WeKnora OpenAPI)
```
