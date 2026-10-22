-- TASK-P04/INT-101: refresh 复用检测守卫（platform 自有状态库）。
-- 轮换时把旧 verifier_hash 挪到 previous_verifier_hash；持有历史 verifier 的 refresh 请求
-- 与当前值不匹配但命中 previous_verifier_hash 时，判定为复用并撤销整个 family。
-- 只存哈希，不存明文 verifier。
ALTER TABLE sessions ADD COLUMN previous_verifier_hash TEXT;
