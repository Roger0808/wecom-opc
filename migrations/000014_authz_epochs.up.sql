-- TASK-P06/DATA-101f/BND-110~112: 授权版本纪元（platform 自有状态库）。
-- 每个 subject 一行单调递增版本：任何 entitlement 变更、identity 停用、revoke 都 +1。
-- grant 签发/续期以此为准判定「签发后授权是否被撤销」；读路径为 CurrentVersion(subject)。
CREATE TABLE IF NOT EXISTS authz_epochs (
    subject    TEXT PRIMARY KEY,
    version    BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
