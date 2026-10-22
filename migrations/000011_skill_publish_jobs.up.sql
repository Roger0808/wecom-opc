-- TASK-P03/DATA-120b: 技能跨空间发布任务记账（platform 只协调与记账，内容与分发权限裁定在 knowledge）
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
