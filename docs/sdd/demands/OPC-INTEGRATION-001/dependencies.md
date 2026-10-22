# OPC-INTEGRATION-001 dependencies（platform-opc 只读投影）

> 本文件为主仓权威内容的只读投影，不得在参与仓独立修改。

# 依赖（platform-opc）

- knowledge-opc：先于 platform 部署（迁移 000129~132 + WeKnora JWTSecret 桥配对）；契约 INT-004/INT-005/INT-007。
- code-opc：后于 platform（federation exchange、租约、invalidate 推送目标）。
- work-opc：后于 platform（INT-101 客户端）。
- 发布顺序见主仓 release-checklist（备份 → knowledge → platform → code/work → 灰度）。
