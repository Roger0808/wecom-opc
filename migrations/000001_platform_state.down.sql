-- 回滚 000001：仅用于全新环境重建，生产环境请遵守只追加语义，不执行本文件
DROP INDEX IF EXISTS idx_audit_events_actor;
DROP INDEX IF EXISTS idx_audit_events_created_at;
DROP TABLE IF EXISTS audit_events;

DELETE FROM schema_migrations WHERE version = '000001_platform_state';
