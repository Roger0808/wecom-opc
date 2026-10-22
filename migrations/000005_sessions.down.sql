-- 回滚 000005：仅用于全新环境重建
DROP INDEX IF EXISTS idx_sessions_family_id;
DROP INDEX IF EXISTS idx_sessions_identity_link_id;
DROP TABLE IF EXISTS sessions;

DELETE FROM schema_migrations WHERE version = '000005_sessions';
