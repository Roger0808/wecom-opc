-- TASK-P03/DATA-110c/BND-112: 受管授权项（同步/手动授予的 capability 账本，platform 自有状态库）
CREATE TABLE IF NOT EXISTS managed_entitlements (
    id             UUID PRIMARY KEY,
    subject        TEXT NOT NULL,
    tenant_id      TEXT NOT NULL,
    capability     TEXT NOT NULL,
    source         TEXT NOT NULL,                      -- sync/manual
    authz_version  BIGINT NOT NULL DEFAULT 0,
    rule_version   TEXT,
    granted_by     TEXT,
    granted_at     TIMESTAMPTZ,
    revoked_at     TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT managed_entitlements_subject_tenant_cap_source_key UNIQUE (subject, tenant_id, capability, source)
);
