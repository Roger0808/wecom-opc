-- TASK-P08/INT-130: 身份冲突证据（identity_links.conflict_evidence）
-- 同步供给发现 knowledge_user_id 被其他 active 主体占用时，conflicted 行在本列
-- 记录冲突证据（JSON：占用主体、knowledge user、发现 run、时间），供
-- GET /identity-conflicts 展示与 resolve(bind|quarantine) 审计追溯。
-- 迁移只执行一次（schema_migrations 记账），不加 IF NOT EXISTS 以兼容 SQLite 测试库。
ALTER TABLE identity_links ADD COLUMN conflict_evidence JSONB;
