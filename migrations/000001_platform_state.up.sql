-- BND-170/REQ-005: 网关安全基线审计表（只追加语义，禁止 UPDATE/DELETE 业务约定）
CREATE TABLE IF NOT EXISTS audit_events (
    id          UUID PRIMARY KEY,
    actor       VARCHAR(128) NOT NULL,               -- 操作者：管理员 sub / anonymous / wecom 等
    action      VARCHAR(255) NOT NULL,               -- 动作：如 admin.auth.denied / wecom.event.received
    target      VARCHAR(512) NOT NULL DEFAULT '',    -- 目标：请求方法+路径、空间 ID 等
    before      JSONB,                               -- 变更前状态（可选）
    after       JSONB,                               -- 变更后状态（可选）
    request_id  VARCHAR(64)  NOT NULL DEFAULT '',    -- 链路追踪 ID（X-Request-ID）
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_audit_events_created_at ON audit_events (created_at);
CREATE INDEX IF NOT EXISTS idx_audit_events_actor ON audit_events (actor);
