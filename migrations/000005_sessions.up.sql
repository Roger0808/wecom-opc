-- TASK-P03/DATA-101c: 会话与刷新轮换族（platform 自有状态库）
CREATE TABLE IF NOT EXISTS sessions (
    session_id             UUID PRIMARY KEY,
    identity_link_id       UUID NOT NULL,
    family_id              UUID NOT NULL,               -- 轮换族：同族会话随 refresh 复用撤销
    refresh_verifier_hash  TEXT NOT NULL,              -- 只存哈希，不存明文 verifier
    device_fingerprint     TEXT NOT NULL,
    authz_version          BIGINT NOT NULL DEFAULT 0,  -- 会话可见授权版本快照
    expires_at             TIMESTAMPTZ NOT NULL,
    revoked_at             TIMESTAMPTZ,
    revoke_reason          TEXT,
    last_seen_at           TIMESTAMPTZ,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_sessions_identity_link_id ON sessions (identity_link_id);
CREATE INDEX IF NOT EXISTS idx_sessions_family_id ON sessions (family_id);
