-- 回滚 000008：仅用于全新环境重建
DROP TABLE IF EXISTS managed_entitlements;

DELETE FROM schema_migrations WHERE version = '000008_managed_entitlements';
