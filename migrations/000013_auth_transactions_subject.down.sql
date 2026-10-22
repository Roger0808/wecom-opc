-- 回滚 000013：仅用于全新环境重建
ALTER TABLE auth_transactions DROP COLUMN identity_link_id;

DELETE FROM schema_migrations WHERE version = '000013_auth_transactions_subject';
