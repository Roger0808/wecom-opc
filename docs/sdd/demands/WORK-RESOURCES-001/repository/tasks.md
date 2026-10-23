---
project: work-opc
document: repository-tasks
demandId: WORK-RESOURCES-001
repositoryId: platform-opc
version: "1.0"
status: confirmed
updatedAt: 2026-10-22
---

# platform-opc 本仓任务投影

本文件保存在主仓，只投影本仓 Tasks；唯一总体任务为 [Tasks v1.0](../../loop-1/tasks.md)。生成来源已确认 Tasks SHA-256：`71edce650f713d8898c6a065026e51bfaadb13f17ffa54d48e4ed42e7676519f`。修改总体任务后重新生成投影，不在参与仓独立编辑需求或改变 activeLoop。

范围：资源代理、受限知识读取、旧发布协调删除和 statedb 迁移恢复。基线、规范 remote、标准分支以 [repositories.json](../../repositories.json) 为准。以下任务均未实施或验证通过。

共享前置：TASK-001 隔离、TASK-002 fixture。共享交付责任：TASK-025 联调本仓适配；TASK-026/027 本仓发布及 statedb 回退；TASK-028/029 本仓 verification/review/receipt 和只读导出；TASK-030 提供本仓固定证据。

规格依据：[Specification v1.0](../../loop-1/specification.md)；当前阶段只维护任务，不预建 implementation/verification/review 或就绪 receipt。

### TASK-009 资源列表与单项目录接口

- 依赖：TASK-002/004；引用：REQ-001/005/008、ADR-003/005、规格 DATA-101、BND-002、INT-101/105。
- 位置：pkg/handler 新资源 adapter、knowledge_upstream.go、main.go。
- 完成条件：实现 KB/catalog 列表和 catalog 单项，按当前 entitlement 的个人桥查询；名称搜索、排序分页、complete/failures 与 partial 提示依据正确。丢失空间与请求失败区分；不查 release 或发布状态，不返回存储位置/沙箱配置。
- 验证方法：VAL-201/202/208；多空间同名、无权过滤、分页去重、部分/全部失败与资源删除；Fake upstream 明确禁止收到 release/publish 调用。

### TASK-010 原始归档下载代理

- 依赖：TASK-004/009；引用：REQ-006/008、ADR-005/007、规格 INT-103/105、BND-006。
- 位置：pkg/handler resources adapter、knowledge_upstream.go、对应测试。
- 完成条件：携个人身份调用新 catalog archive，转发 expected SHA，三种 hash 保持一致；流中断/超限不返回假 ZIP。版本冲突、未就绪与无权按规格映射，支持实际下载预算，不复用 15 秒元数据超时误杀全部大包。
- 验证方法：VAL-209/210/211/218，包含不可信 Content-Length、途中断连、坏 hash 和归档字节完全一致。

### TASK-011 平台受限知识读取

- 依赖：TASK-002；引用：REQ-003/004/008、ADR-004、规格 BND-005、INT-104。
- 位置：pkg/handler/knowledge.go、knowledge_upstream.go 及 knowledge_test.go。
- 完成条件：显式 scope 模式只用指定 tenant/KB；取正文的权威结果校验实际归属后再输出，scope 错配不泄露正文。当前无范围调用保持既有行为；不能遍历其他空间找到同 ID 补结果。
- 验证方法：VAL-205/207；文档移动、不同空间相同标识、KB 删除/撤权、元数据与正文归属变化；断言越界正文未出响应且无额外检索。

### TASK-012 删除发布协调与旧 release 分发

- 依赖：TASK-009/010；引用：REQ-009、ADR-008、规格 BND-010。
- 位置：main.go、pkg/handler/skills.go、knowledge_upstream.go、发布 job model/storage 与专属测试。
- 完成条件：删除旧 skills/internal skills 发布聚合、publish_id 下载、admin publish-jobs；保留共享 resolveAccessSession/entitlement/error/bridge 能力供新 adapter，旧路由 404。code-opc 不改、不保留兼容。
- 验证方法：VAL-216；旧路径全表路由断言、新 resource 契约测试，以及知识/MCP/登录鉴权定向回归；删除后 Go 编译无悬空引用。

### TASK-013 platform 旧账本清理与恢复

- 依赖：TASK-012；引用：ADR-008/DATA-001、规格 BND-011、VAL-217。
- 位置：migrations 新序号、pkg/storage 迁移测试、备份/恢复工具。
- 完成条件：仅删除 statedb.skill_publish_jobs；新迁移不改历史 000011，不改 knowledge 主库、session/entitlement/audit 表。空库和有任务行环境均可恢复，迁移失败停止发布，备份元数据不含秘密。
- 验证方法：临时真实 PostgreSQL up/down 与从备份恢复，校验旧行/约束和其他关键表；不是只跑 empty down 创建空表。

### TASK-014 platform 测试、属性与构建

- 依赖：TASK-009～013；引用：规格 VAL-201/202/205/207/208/210/216/217/218、VAL-102。
- 位置：pkg/handler、pkg/storage、main.go 相关测试；本仓 verification.md。
- 完成条件：个人授权、KB scope、资源归档与错误映射、旧路由退役、存量迁移恢复有证据；属性 P1/P2 使用固定 seed，测试不放宽原权限断言。
- 验证方法：`go test ./pkg/handler ./pkg/storage ./pkg/kbridge`，受影响包通过后 `go test ./...` 与 `go build ./...`；外部依赖缺失如实记录，不能算通过。

## 投影确认

Roger0808 / yj198788@hotmail.com 于 2026-10-22 以“确认，后续交接给 kimi 去开发完成”批准主仓 Tasks v1.0，本仓投影随同确认；任务定义未改，不表示任务已执行。
