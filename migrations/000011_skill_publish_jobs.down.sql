-- 回滚 000011：仅用于全新环境重建
DROP TABLE IF EXISTS skill_publish_jobs;

DELETE FROM schema_migrations WHERE version = '000011_skill_publish_jobs';
