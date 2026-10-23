-- WORK-RESOURCES-001（ADR-008 / DATA-001，BND-011）：删除 platform 状态库旧技能
-- 发布协调账本 skill_publish_jobs（platform 只协调与记账，内容与分发权限裁定
-- 在 knowledge；旧发布链路已随 BND-010 下线）。
-- 只 drop 目标表：不改写历史迁移 000011，不动 session/entitlement/audit 等
-- 共享表。执行前置条件：旧写入已停 + 备份已导出并完成恢复演练（见
-- pkg/storage/publish_jobs_backup.go 与 release-checklist）。
DROP TABLE IF EXISTS skill_publish_jobs;
