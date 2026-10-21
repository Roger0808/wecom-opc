-- 回滚 000006：仅用于全新环境重建
DROP INDEX IF EXISTS idx_runtime_grants_subject_tenant;
DROP TABLE IF EXISTS runtime_grants;

DELETE FROM schema_migrations WHERE version = '000006_runtime_grants';
