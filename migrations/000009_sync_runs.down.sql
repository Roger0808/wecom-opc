-- 回滚 000009：仅用于全新环境重建
DROP INDEX IF EXISTS idx_sync_runs_status;
DROP TABLE IF EXISTS sync_runs;

DELETE FROM schema_migrations WHERE version = '000009_sync_runs';
