-- 回滚 000016：仅用于全新环境重建
DROP INDEX IF EXISTS sync_steps_run_id_idempotency_key_key;

DELETE FROM schema_migrations WHERE version = '000016_sync_steps_idempotency_key';
