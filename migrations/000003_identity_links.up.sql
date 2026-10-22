-- TASK-P03/DATA-101a: 身份关联账本（platform 自有状态库）
CREATE TABLE IF NOT EXISTS identity_links (
    id                 UUID PRIMARY KEY,
    identity_source    TEXT NOT NULL,               -- 身份来源，如 wecom
    organization_id    TEXT NOT NULL,               -- 组织维度（企微 corpId 等）
    immutable_subject  TEXT NOT NULL,               -- 账户中心 SysUser.ID，永不变更；改邮箱不新建行
    knowledge_user_id  VARCHAR(36) NOT NULL,
    code_user_id       UUID,
    status             TEXT NOT NULL DEFAULT 'active',  -- active/conflicted/disabled
    created_at         TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT identity_links_source_org_subject_key UNIQUE (identity_source, organization_id, immutable_subject)
);
