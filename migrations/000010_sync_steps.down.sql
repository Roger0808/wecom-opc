-- 回滚 000010：仅用于全新环境重建
DROP TABLE IF EXISTS sync_steps;

DELETE FROM schema_migrations WHERE version = '000010_sync_steps';
