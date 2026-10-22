-- 回滚 000014：仅用于全新环境重建
DROP TABLE IF EXISTS authz_epochs;

DELETE FROM schema_migrations WHERE version = '000014_authz_epochs';
