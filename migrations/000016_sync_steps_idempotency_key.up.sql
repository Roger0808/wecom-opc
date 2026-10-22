-- L5: sync_steps 幂等键唯一约束（(run_id, idempotency_key)）。
-- apply.go 的 stepKey 注释声明该键唯一；此前仅靠 (run_id, step_seq) 主键兜底，
-- 本迁移把注释语义落为真实约束。idempotency_key 可空：PostgreSQL/SQLite 唯一索引
-- 均将 NULL 视为互不相等，元步骤（snapshot/validate/diff/reconcile，无幂等键）不受影响。
CREATE UNIQUE INDEX IF NOT EXISTS sync_steps_run_id_idempotency_key_key
    ON sync_steps (run_id, idempotency_key);
