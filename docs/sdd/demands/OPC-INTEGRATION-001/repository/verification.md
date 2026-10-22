---
demandId: OPC-INTEGRATION-001
repositoryId: platform-opc
document: verification
status: draft
---

# platform-opc 验证记录

验证对象：TASK-P01~P10、P11a/P11b。环境：本机 macOS 27.0.1 arm64、go 1.26.5；测试库 sqlite（glebarez 内存 + 真实迁移 SQL）+ httptest 假账户中心/假 knowledge/假企微。

## 自动化验证（2026-10-20/21，多次执行）

| 命令 | 结果 |
| --- | --- |
| `go test -count=1 ./...` | PASS ×6（每切片后主会话复验，exit 0） |
| `go test -race -count=1 ./...` | PASS ×4（P04/P05+P06/P07-P10/P11b 后与审查修复后，exit 0；reviewer 第二轮独立复跑亦 exit 0） |
| `gofmt -l .` / `go vet ./...` | PASS（干净） |
| 迁移幂等 | `TestRunMigrations_UpDownUp_Idempotent` 覆盖 000001~000016 真实 SQL up/down/up（sqlite 执行） |

审查修复轮（2026-10-21）新增测试证据：retry 退化快照失败关闭（TestRetryDegradedSnapshotFailsClosed）、伪造 Host 头拒绝、revoke 撤会话族（TestInternalRevoke_SessionFamilySwept）、refresh 后撤权拒绝、并发回调不翻转终态、admin 全量走 syncservice、登录隐式同步移除、租约键名契约、code-exchange 先验后消费、推送通道 `CodeInvalidateChannel` 单测（成功/未配置/非 2xx）。

## 规格映射（关键条款 → 证据）

- BND-170/171/P01：管理面认证/CSRF/审计负向矩阵、TLS 恢复、事件回调验签、AES key 失败（admin_authz_test、handler_callback_test）。
- BND-130/P02：游标分页 + aggregate 对账、ErrSnapshotIncomplete（client_test 10 用例）。
- DATA-101/110/P03/P04：迁移 up/down、授权码原子消费（并发 8 竞争 1 放行）、PKCE、refresh 复用整族撤销、code-exchange 服务身份（integration_test）。
- INT-102/P05+P06：租约 30s/续期单赢/撤销先持久化后通知/上游 token 删除（internal_test、authz_test 并发 bump）。
- BND-130/131/P08：全量/单部门同步、幂等重放、调岗增新收旧、停用撤权四联（entitlement+epoch+token+is_active）、空快照/缩减>50% 失败关闭、部分失败 retry、锁互斥与过期接管、dept_admin 越权（syncservice 12 用例 + admin_sync 5 用例）。
- ADR-004/P09：桥六条件签发、claims 逐键对齐、撤权删除（kbridge_test）。
- INT-120/141/142/P07/P10：搜索规范化、Viewer 下载 403 透传、publish-job 幂等、download hash 校验（knowledge/skills 测试）。
- INT-150/P11a/P11b：双身份上下文、actor_kind=user/task、jti、decision 单次绑定（mcp/mcp_task 测试）。

## 差异与遗留（诚实记录）

- 与真实账户中心/knowledge/企微的联调：客户端契约经假 server 全链路验证；真实端联调属跨仓 X03（测试环境部署后）。
- P01 遗留：曾用 WeKnora 主库跑迁移的环境会留 audit_events 孤表（运维切换时清理）。
- POST /sync/runs 同步执行（断连已解耦）；AdminGetOverview 仍回显死配置 CronEnabled（trivial，review N6，记 backlog）。

## 工程扩展

- Testing：见上表全 PASS。
- PBT/Security/Resiliency：跨仓阶段（X01/X03）统一执行回填（含 property/case 数/seed、负向安全矩阵注入、故障注入证据）。

## 测试环境部署证据（2026-10-21，用户授权后执行）

- 备份：/data/backup-opc-integration-20261021/（dump 141MB + tar + 镜像 digest + SHA256SUMS，已验证可读）。
- knowledge：镜像 weknora-app/ui:opc-int-001（sha256 记录在服务器证据文件）；正式库 118→132 受控迁移（副本库先行验证，chunks 行数一致）；健康/401 语义验证。
- platform：二进制 f42850c→8b7d010（审计修复后重发，sha256 在服务器证据文件）；自有库 weknora_gateway_state（opc_platform 账号，对 weknora.chunks 读拒，隔离验证）；密钥 600 注入；/health 200、无 token 401；审计落库验证（修复后）。
- code：opc-code-server:opc-int-001（3095→8080）；opc_code 库迁移 585~590（scratch 先行）；/healthz 200。
- 冒烟：负向 401 矩阵正确；网关↔code 互通；日志密钥扫描 0 命中；3091-3093 服务未动。
- 部署发现（已处置/记录）：审计 22P02（已修 8b7d010）；knowledge EXT 路由匿名探测恒 401（全局 auth 前置，fail-closed 供应态需账号复测）；compose 需 --no-deps（docker.io 不可达）；gateway /health 为静态桩（三档报告未实现，记差异）。
