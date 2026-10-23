# WORK-RESOURCES-001 依赖与发布顺序（platform-opc 只读投影）

> 本文件为只读追溯包的一部分，不得在参与仓独立修改；权威版本见主仓。

## 发布顺序

knowledge → platform → work（主仓 release-checklist.md）。platform-opc 第 2 步发布。

## 本仓依赖

- knowledge-opc：先于本仓发布（INT-103/105 归档上游）；本仓部署前置为 knowledge 第 1 步已上线且新归档路由可用。
- work-opc：后于本仓发布，消费 INT-101/104/105。
- 迁移：本仓迁移 `000017_drop_skill_publish_jobs` 只 drop statedb.skill_publish_jobs，与 knowledge/work 迁移无链式依赖，但须在 knowledge 第 1 步完成后执行。

## 回滚顺序（发布失败时）

work 回退 → platform 回退二进制并执行 000017 down + 备份恢复 → knowledge 回退镜像并执行 000133/000047 down + 备份恢复（主仓 release-checklist.md）。
