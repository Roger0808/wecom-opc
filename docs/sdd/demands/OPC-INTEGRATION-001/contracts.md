# OPC-INTEGRATION-001 contracts（platform-opc 只读投影）

> 本文件为主仓权威内容的只读投影，不得在参与仓独立修改。

# 跨仓契约（platform-opc 视角）

- → 账户中心/企微：GraphQL 游标分页 + aggregate 对账（在职白名单 userStatus==1 AND isDeleted==0）；企微 OAuth 回调验签。
- → knowledge：兼容桥个人上游 token（≤120s，auth_tokens 行，六条件签发）；EXT-001 发布/下载；EXT-002 调用转发（user/task 双 actor，上下文 aud=platform-integration、sub=knowledge 用户 id、args_digest 同算法）。
- ← work/code：INT-101 认证组（PKCE，code_verifier 必传）、INT-102 租约（task_id/runtime_id/tenant_id 键名）、INT-130 手动同步、撤权推送 code invalidate（CODE_BASE_URL/CODE_SERVICE_TOKEN）。
- 错误码 BND-180 枚举；撤权 60s 预算 T+5s 起。
