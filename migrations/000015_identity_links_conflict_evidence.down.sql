-- 回滚 000015：仅用于全新环境重建
ALTER TABLE identity_links DROP COLUMN conflict_evidence;

DELETE FROM schema_migrations WHERE version = '000015_identity_links_conflict_evidence';
