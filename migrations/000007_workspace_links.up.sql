-- TASK-P03/DATA-101e: 身份与 code 工作区关联（platform 自有状态库）
CREATE TABLE IF NOT EXISTS workspace_links (
    identity_link_id    UUID NOT NULL,
    code_workspace_id   UUID NOT NULL,
    knowledge_tenant_id TEXT NOT NULL,
    source              TEXT NOT NULL,                 -- sync/manual
    created_at          TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT workspace_links_link_workspace_key UNIQUE (identity_link_id, code_workspace_id)
);
