-- 回滚 000003：仅用于全新环境重建
DROP TABLE IF EXISTS identity_links;

DELETE FROM schema_migrations WHERE version = '000003_identity_links';
