---
demandId: OPC-INTEGRATION-001
repositoryId: platform-opc
document: review
status: draft
---

# platform-opc 独立审查记录

reviewer 路径：当前 Agent 的隔离只读 subagent（用户 2026-10-21 选择）。

## 第一轮（2026-10-21，指纹：HEAD=d6a07bf0…、变更面 53 条目、组合摘要 6295f8a2…；reviewer 复跑 go test -race 全绿）

结论：**CHANGES_REQUIRED**。

- H1（阻断）：sync Retry 对新快照跳过 validating（stepApplied(run,2) 首跑已 applied），退化快照下可批量误停在职用户——违反 BND-130 停止条件。
- H2（高）：登录跳转同源校验锚在可伪造 Host/X-Forwarded-Host（钓鱼代理链可兑换受害者会话）。
- H3（高）：/internal/revoke 不撤 sessions 族；Refresh 不重查 identity/entitlement；被撤主体 14 天内可续签（BND-111 覆盖清单未落实）。
- H4（高）：FailAuthTransaction 无状态守卫，重试回调可把 completed 翻 failed（终态不可逆被破坏；code 受众登录可被 DoS；两处口径不一致）。
- H5（高）：/admin/api/sync/all 仍走旧 SyncAll（无账本/锁/快照契约/幂等键），与"触发点归一"不符；SyncDepartment 内裸 goroutine 仍在。
- H6（中，需人类裁决）：登录缓存未命中触发 scope=department 隐式同步（主会话给 P08 的指令偏差）——按 INT-130 原文移除。
- 低 L1~L9：spaces 未去重；企微换票错误码张冠李戴；Authorize 未强制非空 state；dept scope 部门树仍用旧非分页查询；sync_steps 幂等键注释与迁移不符；Run 幂等键预查在锁事务外（并发撞 500）；tenant 级 revoke 超范围删全部 auth_tokens；config.example.yaml 残留 cron 宣传；iterateConnection 无页数上限/停滞检测。

已核对通过要点（原子消费/PKCE/refresh 族撤销/租约/桥六条件/管理面/分页对账/TLS/回调验签等）见审查输出。

处置：修复切片处理 H1~H6 与 L1~L9；完成后重新固定指纹并重审。

## 第二轮（2026-10-21，差异重审，新指纹 f0f4fb65…）

结论：**READY_FOR_HUMAN_REVIEW**。H1~H6 逐条核对已修复且有针对性测试（含 retry 退化快照失败关闭），L1~L9 全部处置，契约增补与推送通道实现正确；reviewer 独立复跑 -race 全绿 exit 0。

遗留（低/观察，不阻断）：N2 CodeInvalidateChannel 补单测（主会话补）；N3 同 run 并发 Retry 可并存（唯一键+幂等兜底，后续收口）；N4 租户级撤权连带删同租户原生 token（fail-closed 方向，注释措辞修正）；N5 运维须知：新部门员工首登前需先手动同步（部署说明写明）；N6 AdminGetOverview 死配置回显（trivial）。跨仓契约两项（code_verifier 必传、租约键名）列 X01 确认。N1：verification.md 由主会话刷新（000016、新测试、推送通道证据）。
