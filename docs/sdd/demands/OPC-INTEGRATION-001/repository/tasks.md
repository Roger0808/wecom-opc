---
demandId: OPC-INTEGRATION-001
repositoryId: platform-opc
document: tasks
---

# platform-opc 任务（总体 tasks.md 的本仓视图）

基线：main / `d6a07bf02fd7aeded69fca810c9c7e4f2f07d74c`；分支 `feature/261020_OPC-INTEGRATION-001_opc-integration`（已从固定基线创建）。本仓任务引用总体编号：TASK-P01~P11（见 loop-1/tasks.md）。验证命令：`go test ./...`。

- TASK-P01 安全基线（管理面鉴权/CSRF/审计、TLS、事件回调、AES key 失败）→ 本仓首个任务，无前置。
- TASK-P02 账户中心分页重写（依赖 P01 仅因同仓顺序）。
- TASK-P03 平台自有状态库迁移。
- TASK-P04 认证接口组 INT-101。
- TASK-P05 服务间接口 INT-102。
- TASK-P06 撤权引擎。
- TASK-P07 知识适配 INT-120。
- TASK-P08 手动同步服务 INT-130。
- TASK-P09 knowledge 兼容桥隔离。
- TASK-P10 技能发布协调 INT-141/142。
- TASK-P11 测试与工程扩展证据（PBT/安全/Resiliency）。

依赖与验收对应见总体 tasks.md 第 6 节与规格 VAL-101~111。
