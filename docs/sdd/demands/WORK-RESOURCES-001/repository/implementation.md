---
project: work-opc
document: implementation
demandId: WORK-RESOURCES-001
repositoryId: platform-opc
loop: 1
status: draft
updatedAt: 2026-10-22
---

# platform-opc 实施记录

## 基线与分支（TASK-001）

- remote：`https://github.com/Roger0808/wecom-opc.git`
- 固定基线：release @ `27de3c77de6be0fb08e8147450b3a449d296dd16`（固定 SHA 经 git rev-parse 核验一致）
- 标准分支：`feature/261022_WORK-RESOURCES-001_resource-entry`，从固定基线新建（disposition=created）
- 隔离方式：专用 worktree，从固定 commit 检出；原工作区未跟踪内容（AGENTS.md、CLAUDE.md、docs/loops/）保留在原工作区，不带入、不计入交付
- 任务范围：TASK-009～014；当前迁移最大序号 000016，新清理迁移计划 000017（实施前核对空闲号）

## 实施过程

- 2026-10-22 TASK-012（先删旧链路，ADR-008/BND-010）：删除 `pkg/handler/skills.go`（旧 skills 发布聚合 List/Download/InternalList、admin publish-jobs 两路由、release 上游契约）及专属测试 `skills_test.go`、`internal_skills_test.go`；`main.go` 删除 `/integration/v1/skills`、`/skills/:publish_id/download`、`/internal/skills`、admin publish-jobs 注册；`pkg/model/platform_state.go`、`pkg/storage/state.go` 删除 `SkillPublishJob` 模型与 repository 方法。共享能力保留：`resolveAccessSession`/`entitledTenants`/`issueBridgeToken`/`denyUpstream`/`writeErrorEnvelope`/`auditEvent` 留在 `pkg/handler/knowledge.go` 供新 adapter 复用。旧路由在测试引擎断言 404（`TestOldSkillRoutes_NotRegistered`）。未 commit/push。
- 2026-10-22 TASK-011（VAL-205/207，BND-005/INT-104）：`pkg/handler/knowledge.go` `GetKnowledge` 增加显式 scope 模式——`tenant` 与 `knowledge_base_id` 成对出现时启用：只用指定 tenant/KB（绝不遍历其他授权空间补结果），取回权威文档后先校验实际归属（`tenant_id`+`knowledge_base_id` 与 scope 一致才输出正文；字段缺失 fail-closed），错配 403 `SCOPE_MISMATCH` 且不泄露正文/片段；目标不可见统一 404 `RESOURCE_NOT_ACCESSIBLE`；显式无权空间 403。`tenant` 单独出现保持既有缺省模式行为（逐空间首个可见命中，回归不变）。
- 2026-10-22 TASK-009/010（VAL-201/202/208/209/210/211/218，INT-101/103/105）：新增 `pkg/handler/resources.go` `ResourcesHandler` 四路由：`GET /integration/v1/resources/knowledge-bases`、`/skills`、`/skills/:catalog_id`、`/skills/:catalog_id/archive`。列表按当前 entitlement 逐空间用个人桥 token 查 knowledge（`/api/v1/knowledge-bases`、`/api/v1/skills/catalog`），名称 NFC+大小写无关包含匹配、tenant/name/id 升序稳定排序、offset/limit 分页（缺省 50、1~100），`{items,total,offset,limit,complete,failures}` 信封，部分空间失败 `complete=false` 且 failures 只含失败的原授权空间，全部失败 502 不伪装空列表；显式无权 tenant_id 403。目录 SHA 缺失条目 `not_ready`+`ARCHIVE_NOT_READY`。不查 release/发布状态，不返回存储位置/沙箱配置。归档代理携个人身份调 knowledge 新 `GET /api/v1/skills/catalog/:id/archive?expected_sha256=`，完整收齐（512MiB 上限）后三向哈希一致校验（请求预期/上游 X-SHA256/实际字节），不符 422 `SKILL_HASH_MISMATCH`；上游 409 `SKILL_VERSION_CHANGED`/`ARCHIVE_NOT_READY`、404 按规格映射；流中断/无进度/总预算超时 502 不返回假 ZIP。下载独立预算（总 120s、首包/连接与无进度 15s），不复用元数据 15s 超时（`newDownloadClient`/`idleReader`）。detail_url 仅由 `weknora.base_url` 的 https 可信 origin 构造。
- 2026-10-22 TASK-002 fixture：`pkg/handler/knowledge_fake_test.go` 重写——KB/目录按租户隔离（T1/T2 同名 C1/C2、h1/h2 真实字节 hash 由包字节计算）、租户级请求失败/延迟注入、归档错误注入（版本冲突/未就绪/坏 hash/断流/不可信 X-SHA256 头）、release/publish 哨兵（任何 `/api/v1/skills/releases*` 或 publish 调用计数并 404，测试用 `assertNoReleaseCalls` 断言零调用）。
- 2026-10-22 TASK-013（VAL-217，BND-011）：新增迁移 `migrations/000017_drop_skill_publish_jobs.up/down.sql`——只 drop 目标表，不改历史 000011，不动 session/entitlement/audit 等共享表；up 幂等（DROP IF EXISTS）。新增 `pkg/storage/publish_jobs_backup.go` `ExportSkillPublishJobs`/`ImportSkillPublishJobs`（规范化 JSON + SHA-256 校验和、篡改/行数不符/表缺失拒绝、事务内幂等插回）。真实 PostgreSQL 16 临时库验证 up/down/恢复/共享表保护/重启重放（测试用临时库已删除）。
- 2026-10-22 TASK-014：`pkg/handler/resources_test.go`（VAL-201/202/208/209/210/211/216/218）、`knowledge_scope_test.go`（VAL-205/207）、`resources_pbt_test.go`（P1 seed=20261022 500 case 允许输出集⊆scope；P2 seed=20261023 500 case 列表不跨主体/归档三元组）。`go test ./...` 与 `go build ./...` 全绿（详见 verification.md）。`go.mod` 中 `golang.org/x/text` 由 indirect 转为直接依赖（NFC/大小写折叠名称匹配），无新增第三方 PBT 依赖。未 commit/push。

## Change surface

- 代码：`main.go`（路由替换）；`pkg/handler/resources.go`（新增）；`pkg/handler/knowledge.go`（scope 受限读取 + denyUpstream 码表更新）；`pkg/handler/knowledge_upstream.go`（删除 release 契约，新增目录契约与独立下载 client/idleReader）；`pkg/handler/knowledge_fake_test.go`（夹具重写）；`pkg/handler/{resources_test,knowledge_scope_test,resources_pbt_test}.go`（新增测试）；`pkg/handler/skills.go`、`skills_test.go`、`internal_skills_test.go`（删除）；`pkg/handler/mcp_test.go`、`mcp_task_test.go`（AutoMigrate 去掉已删模型）；`pkg/model/platform_state.go`、`pkg/storage/state.go`（删除 SkillPublishJob）；`pkg/storage/state_test.go`（platformTables/用例同步）；`pkg/storage/storage.go`（`ListWeKnoraTenantNames`）；`pkg/storage/publish_jobs_backup.go`（新增）；`pkg/storage/publish_jobs_backup_test.go`（新增）；`go.mod`/`go.sum`（x/text 直接依赖）。
- 迁移：`migrations/000017_drop_skill_publish_jobs.{up,down}.sql`（新增；历史 000011 不改写）。
- 接口：新增 `GET /integration/v1/resources/knowledge-bases|skills|skills/:catalog_id|skills/:catalog_id/archive`；删除 `/integration/v1/skills`、`/skills/:publish_id/download`、`/internal/skills`、admin `/skills/publish-jobs*`（404）；`GET /integration/v1/knowledge/:id` 增加 `knowledge_base_id` scope 参数（与 `tenant` 成对启用，INT-104）。
- 数据：仅 `statedb.skill_publish_jobs` 删除（前向迁移 000017 + 备份恢复工具）；knowledge 主库、session/entitlement/audit 等共享表不动。
- 未执行/不在本仓：code-opc 不改（用户决定）；work 侧任务 015~024 与跨仓 025~030 未开始；真实三服务联调与 60 秒撤权计时属 TASK-025/027。
