-- TASK-P03/DATA-110b: 同步步骤明细（platform 自有状态库）
CREATE TABLE IF NOT EXISTS sync_steps (
    run_id          UUID NOT NULL,
    step_seq        INT NOT NULL,
    step_type       TEXT NOT NULL,                     -- snapshot/validate/diff/provision/revoke/reconcile
    subject_ref     TEXT,
    idempotency_key TEXT,
    status          TEXT NOT NULL DEFAULT 'pending',   -- pending/applied/failed/skipped
    error           TEXT,
    retryable       BOOLEAN NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (run_id, step_seq)
);
