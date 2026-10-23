---
project: work-opc
document: verification
demandId: WORK-RESOURCES-001
repositoryId: platform-opc
loop: 1
status: draft
updatedAt: 2026-10-22
---

# platform-opc 验证记录（TASK-009～014）

本仓代码指纹：worktree `platform-opc.wr001`，分支 `feature/261022_WORK-RESOURCES-001_resource-entry`，基线 `27de3c77de6be0fb08e8147450b3a449d296dd16`，改动未提交。Go `go1.26.5 darwin/arm64`。全部命令在 worktree 根执行；退出码均为 0。

## 自动化验证

| 命令 | 退出码 | 覆盖 |
|---|---|---|
| `go test ./pkg/handler ./pkg/storage ./pkg/kbridge`（`-count=1`） | 0 | handler（资源/知识/MCP/admin/integration 全量）、storage（含 000017 SQLite 序列与备份恢复）、kbridge |
| `go test ./...`（`-count=1`） | 0 | 全仓 15 个测试包全 ok，无 FAIL/skip |
| `go test -race -count=1 ./pkg/handler` | 0 | 27.7s，无数据竞争（idleReader goroutine + fake mutex 路径） |
| `go build ./...` | 0 | 含 main.go 新路由注册，无悬空引用 |
| `go vet ./...` | 0 | 无输出 |

真实 PostgreSQL 16（Homebrew，本机临时库 `wr001_mig_test`，验证后已 DROP）：

| 命令 | 退出码 | 覆盖 |
|---|---|---|
| `psql -h localhost -d postgres -c "CREATE DATABASE wr001_mig_test"` | 0 | 临时库创建 |
| `PLATFORM_STATE_TEST_PG_DSN="host=localhost user=<user> dbname=wr001_mig_test port=5432 sslmode=disable" go test ./pkg/storage -run TestSkillPublishJobs -count=1 -v` | 0 | 6 个子测试全 PASS，含 `TestSkillPublishJobsMigration00017_Postgres`：全量迁移 000001~000017 → down 重建 → 存量行备份 → 000017 up drop → 共享表（sessions/managed_entitlements/audit_events/identity_links/sync_runs/sync_steps）存活 → down 重建 → 导入恢复行数/内容一致 → 重启 RunMigrations 幂等（版本 17、表保持 dropped） |
| `psql -h localhost -d postgres -c "DROP DATABASE wr001_mig_test"` | 0 | 临时库删除 |

## 验收映射（VAL）

| VAL | 证据 |
|---|---|
| VAL-201 多空间/搜索/翻页/partial | `TestResourcesKBList_MultiTenantAndSearch`（T1/T2 同名、tenant_name、NFC 搜索）、`_Pagination`（total/offset/limit/参数负向）、`TestResourcesSkillList_MultiTenantSameNameAndNotReady`、`TestResourcesList_PartialFailure`（complete=false + failures 只含失败原授权空间）、`_AllFailedIsErrorNotEmptyList`（502）、`_EntitledButEmptyIsComplete` |
| VAL-202 未登录/无权 | `TestResourcesList_Unauthenticated`（401）、`TestResourcesList_ExplicitUnentitledTenantForbidden`（U3 403）、`TestResourcesSkillItem_UnentitledAndInvisible`、`TestResourcesArchiveDownload_UnentitledAndMissingSHA` |
| VAL-205 scope 强制 | `TestKnowledgeGet_ScopedReadOK`、`_ScopeMismatchLeaksNothing`（403 SCOPE_MISMATCH、正文零泄露、上游仅 1 次请求）、P1 属性（seed=20261022） |
| VAL-207 文档移动/撤权不越界 | `TestKnowledgeGet_ScopeTenantMismatchLeaksNothing`（元数据与正文同次校验）、`_ScopedInvisibleIs404NotTraversal` |
| VAL-208 删除资源/无 release | `TestResourcesSkillList_DeletedCatalogDisappears`（删除后列表/单项/归档均 404）、全部用例末尾 `assertNoReleaseCalls`（fake 收到 release/publish 即失败） |
| VAL-209 安装前检查/下载 | `TestResourcesArchiveDownload_OKAndHashConsistency`（字节完全一致、X-SHA256/Content-Type/Content-Length/Content-Disposition）、`_UppercaseSHANormalized` |
| VAL-210 版本冲突 | `TestResourcesArchiveDownload_VersionConflict`（409 SKILL_VERSION_CHANGED，刷新后新 hash 200）、P2 伪造 SHA 拒绝 |
| VAL-211 坏包/假 ZIP | `TestResourcesArchiveDownload_BadBytesHashMismatch422`（422）、`_UntrustworthyHeaderRejected`（X-SHA256 不可信 422）、`_StreamTruncationNoFakeZip`（声明完整长度断流 → 502、半个 ZIP 不泄露） |
| VAL-216 旧路由 404 | `TestOldSkillRoutes_NotRegistered`（skills/skills download/internal skills/publish-jobs 全 404，含 POST）；`main.go` 注册段已删除、`go build` 无引用 |
| VAL-217 迁移与恢复 | `TestSkillPublishJobsBackup_RoundTrip`（导出/空备份/重建/导入/幂等）、`_RejectsTampered`（篡改/行数不符/表缺失拒绝）、`_SQLiteSequence`、`_Targeted`、`_UpIdempotent`、`_Postgres`（真实 PG 全链路） |
| VAL-218 超时/中断/恢复 | `TestResourcesArchiveDownload_IndependentBudgetNotMetadataTimeout`（元数据超时 50ms 不杀 300ms 下载）、`_IdleTimeoutKillsStalledStream`（无进度 100ms → 502）、`_TotalBudgetApplies`（总预算 200ms → 502） |

## 属性测试（VAL-102）

| Property | seed | case 数 | 结果 |
|---|---|---|---|
| P1 任意 scope/文档归属，允许输出集合 ⊆ 所选 tenant/KB | 20261022 | 500（其中 69 允许输出、其余含非法 KB 400 与三类错配 403 全部断言） | PASS，无反例 |
| P2 任意主体/tenant/catalog/SHA/同名组合，列表不跨主体、同空间同 ID 唯一、availability 与 SHA 已知性一致、归档只为权威三元组输出字节 | 20261023 | 500（含 513 次归档三元组校验） | PASS，无反例 |

生成器为测试目录内 `math/rand` 确定性实现，无新增 PBT 依赖；属性经真实 HTTP 处理链 + fake knowledge 上游断言，不只验证实现函数自身返回。当前无失败反例待固化。

## 工程扩展（VAL-102 表）

| 扩展 | 结论 | 证据 |
|---|---|---|
| testing | PASS | 上表 VAL 映射，全部实跑 |
| pbt | PASS | P1/P2 固定 seed、各 500 case |
| security | PASS | 401/403/404 负向、SCOPE_MISMATCH 零泄露、Viewer 管理权限不扩张（旧 admin publish-jobs 404）、恶意 ID/SHA 400、不可信头/断流拒绝、`TestResourcesDetailURLTrustedOriginOnly`（仅 https 可信 origin、ID 单段编码、http/userinfo/非法 base 一律不出链接） |
| resiliency | PASS | 部分/全部失败语义、超时三件套（元数据/无进度/总预算）、迁移 up/down/备份恢复（SQLite + 真实 PG 存量行） |

## 未执行项与原因

- 真实 60 秒撤权计时（VAL-215 平台侧时序）：属 TASK-027 测试环境部署 + 真实计时，需部署授权与三仓联调，本仓单元阶段无法执行——如实记录，不算通过。
- 三仓端到端联调（TASK-025/027，真实 knowledge 上传→platform→work 闭环、DMG 手测）：work 侧任务未实施，未执行。
- 归档下载「超大包不被误杀」的真实 120s 级验证：测试以缩时预算等价验证机制（50ms 元数据超时 vs 300ms 下载成功；100ms idle/200ms 总预算拒绝）；生产 120s/15s 常量未做真实时长跑测（无必要且无授权环境），记录为机制级证据。
- 旧服务端兼容性（VAL-218 的「旧服务端 501/明确失败」映射为 work 侧 RESOURCE_API_UNSUPPORTED 判断）：platform 对上游结构性 404 按规格映射 RESOURCE_NOT_ACCESSIBLE；sidecar 的 501 映射在 work 仓实现与验证。

## 验证边界声明

以上均为本仓真实执行结果；skip/缺环境未计入任何 PASS。`PLATFORM_STATE_TEST_PG_DSN` 未设置时 PG 用例 skip（缺环境不算通过）；本次已用真实 PG 16 临时库实跑通过并删除临时库。自动化验证通过不等于代码审查通过；AI Review 与人工审查在后续阶段按锁定流程执行。
