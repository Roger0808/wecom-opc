-- TASK-P03/DATA-101b: 登录事务（授权码一次性原子消费的记账载体，platform 自有状态库）
CREATE TABLE IF NOT EXISTS auth_transactions (
    login_txn_id        UUID PRIMARY KEY,
    client_id           TEXT NOT NULL,
    redirect_uri        TEXT NOT NULL,
    state_hash          TEXT NOT NULL,
    code_challenge      TEXT NOT NULL,
    device_fingerprint  TEXT NOT NULL,
    code_hash           TEXT,
    audience            TEXT NOT NULL,
    expires_at          TIMESTAMPTZ NOT NULL,
    consumed_at         TIMESTAMPTZ,
    status              TEXT NOT NULL DEFAULT 'pending',  -- pending/completed/failed/expired
    created_at          TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
