-- TASK-P03/DATA-110a: 同步运行账本（platform 自有状态库）
CREATE TABLE IF NOT EXISTS sync_runs (
    run_id            UUID PRIMARY KEY,
    scope             TEXT NOT NULL,                   -- full/department
    department_code   TEXT,
    idempotency_key   TEXT NOT NULL,
    rule_version      TEXT NOT NULL,
    triggered_by      TEXT NOT NULL,                   -- 管理员主体
    status            TEXT NOT NULL,                   -- queued/snapshotting/validating/diffing/applying/reconciling/succeeded/partial_failed/failed
    snapshot_digest   TEXT,
    snapshot_stats    JSONB,
    lock_until        TIMESTAMPTZ,                     -- 持久化互斥租约
    started_at        TIMESTAMPTZ,
    finished_at       TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT sync_runs_idempotency_key_key UNIQUE (idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_sync_runs_status ON sync_runs (status);
