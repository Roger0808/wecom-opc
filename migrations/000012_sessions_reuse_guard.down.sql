-- 回滚 000012：仅用于全新环境重建
ALTER TABLE sessions DROP COLUMN previous_verifier_hash;

DELETE FROM schema_migrations WHERE version = '000012_sessions_reuse_guard';
