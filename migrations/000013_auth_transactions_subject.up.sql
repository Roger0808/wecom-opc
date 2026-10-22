-- TASK-P04/INT-101: 登录事务与已认证主体的绑定（platform 自有状态库）。
-- callback 签发授权码时写入 identity_links.id；/auth/token 据此为 audience=work 建会话、
-- 为 audience=code-backend 签发绑定主体的兑换材料。回调是主体已知的唯一时点，必须落账。
ALTER TABLE auth_transactions ADD COLUMN identity_link_id UUID;
