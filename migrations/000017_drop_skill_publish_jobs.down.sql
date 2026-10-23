-- 回滚 000017：结构恢复（全新环境重建空表）。
-- 注意（BND-011）：down 只重建空表，存量行的恢复必须走备份导入
-- （Export/ImportSkillPublishJobs）并校验行数与 checksum；「down 重建空表」
-- 不构成数据恢复通过。DDL 与历史 000011 保持一致。
CREATE TABLE IF NOT EXISTS skill_publish_jobs (
    job_id           UUID PRIMARY KEY,
    release_id       TEXT NOT NULL,
    source_tenant    TEXT NOT NULL,
    targets          JSONB NOT NULL,
    idempotency_key  TEXT NOT NULL,
    status           TEXT NOT NULL,
    steps            JSONB,                            -- 逐目标结果
    created_at       TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT skill_publish_jobs_idempotency_key_key UNIQUE (idempotency_key)
);

DELETE FROM schema_migrations WHERE version = '000017_drop_skill_publish_jobs';
