-- 回滚 000007：仅用于全新环境重建
DROP TABLE IF EXISTS workspace_links;

DELETE FROM schema_migrations WHERE version = '000007_workspace_links';
