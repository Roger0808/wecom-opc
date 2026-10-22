-- TASK-P03/DATA-101d: 运行时授权租约（platform 自有状态库）
CREATE TABLE IF NOT EXISTS runtime_grants (
    grant_id            UUID PRIMARY KEY,
    session_id          UUID,                          -- work 会话绑定（可空）
    task_id             TEXT,                          -- code 任务绑定（可空）
    workspace_id        TEXT,
    runtime_id          TEXT,
    subject             TEXT NOT NULL,
    tenant_id           TEXT NOT NULL,
    originating_user_id TEXT NOT NULL,
    capabilities        JSONB,
    authz_version       BIGINT NOT NULL DEFAULT 0,
    lease_expires_at    TIMESTAMPTZ NOT NULL,
    revoked_at          TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_runtime_grants_subject_tenant ON runtime_grants (subject, tenant_id);
