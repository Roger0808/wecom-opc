-- 回滚 000004：仅用于全新环境重建
DROP TABLE IF EXISTS auth_transactions;

DELETE FROM schema_migrations WHERE version = '000004_auth_transactions';
