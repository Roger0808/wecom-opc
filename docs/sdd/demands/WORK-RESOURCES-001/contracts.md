# WORK-RESOURCES-001 对外契约摘要（platform-opc 只读投影）

> 本文件为只读追溯包的一部分，不得在参与仓独立修改；权威版本见主仓。

## 对外接口

- `GET /integration/v1/resources/knowledge-bases`、`/integration/v1/resources/skills`、`/integration/v1/resources/skills/:catalog_id`、`/integration/v1/resources/skills/:catalog_id/archive`（INT-101/103）
  - 按当前 entitlement 的个人桥逐空间查询 knowledge；名称 NFC+大小写无关包含匹配，tenant/name/id 升序稳定排序，offset/limit 分页（缺省 50，1~100）。
  - 列表信封 `{items,total,offset,limit,complete,failures}`；部分空间失败 `complete=false` 且 failures 只含失败的原授权空间；全部失败 502 不伪装空列表；显式无权 tenant_id 403。
  - 不查 release/发布状态，不返回存储位置/沙箱配置；目录 SHA 缺失条目 `not_ready`+`ARCHIVE_NOT_READY`。
  - 归档代理携个人身份调 knowledge `GET /api/v1/skills/catalog/:id/archive?expected_sha256=`，完整收齐（512MiB 上限）后三向哈希一致校验（请求预期/上游 X-SHA256/实际字节）；下载独立预算（总 120s、首包/连接与无进度 15s），不复用元数据 15s 超时。
- `GET /integration/v1/knowledge/:id?tenant=<scope tenant>&knowledge_base_id=<scope KB>`（INT-104）
  - scope 模式：`tenant` 与 `knowledge_base_id` 成对出现时启用；只用指定 tenant/KB，绝不遍历其他授权空间补结果。
  - 输出正文前校验权威文档实际 tenant/KB 归属（字段缺失 fail-closed），错配 403 `SCOPE_MISMATCH` 且不泄露正文/片段；目标不可见统一 404 `RESOURCE_NOT_ACCESSIBLE`；显式无权空间 403。
  - `tenant` 单独出现保持既有缺省行为（逐空间首个可见命中，回归不变）。

## 错误映射（INT-105）

- 统一 `{error:{code,message,request_id}}`，不以 200 空 items 吞失败。
- 400 `INVALID_REQUEST`、401 `SESSION_REVOKED`、403 `SPACE_FORBIDDEN`、403 `SCOPE_MISMATCH`、404 `RESOURCE_NOT_ACCESSIBLE`、409 `CONTEXT_CHANGED`、409 `SKILL_VERSION_CHANGED`、422 `SKILL_HASH_MISMATCH`、502 `UPSTREAM_UNAVAILABLE`（流中断/无进度/坏 hash 不返回假 ZIP）。
- 上游 409 `SKILL_VERSION_CHANGED`/`ARCHIVE_NOT_READY`、404 按规格映射。

## 旧路由

- 旧 `/integration/v1/skills`、`/skills/:publish_id/download`、`/internal/skills`、admin `/skills/publish-jobs*` 全部 404（VAL-216）。
- 迁移 `migrations/000017_drop_skill_publish_jobs.{up,down}.sql` 仅 drop statedb.skill_publish_jobs；up 幂等，down 事务幂等恢复；session/entitlement/audit 等共享表不动。

## 上下游

- → knowledge-opc：INT-103/105 归档上游（个人桥 token）。
- ← work-opc：INT-101/104/105 客户端。
