# SaaS 路线第二阶段：Go Control Plane 收口与生产化计划

> **阶段命名说明**：这里的“第二阶段”对应产品路线中的“新建 Go Control Plane”。它不等同于
> `docs/plans/saas-tenancy-organization-user-plan.md` 内部编号为 Phase 2 的“Project 与 Agent Session
> 归属”。
>
> **执行要求**：先核对当前工作区，不得把已经实现的功能重写一遍。当前分支存在大规模未提交
> 实现，执行者必须基于现状增量开发，不得清理、重置或覆盖无关改动。
>
> **验证规则**：使用 `go test` 验证 Go 服务；仓库级 `bun fmt`、`bun lint`、`bun typecheck`
> 只有在当前对话中由操作人明确要求时才运行。禁止使用 `bun test`，只能使用 `bun run test`。

## 1. 状态

- **优先级**：P0
- **预计工作量**：XL
- **风险**：HIGH
- **计划基线分支**：`codex/saas-tenancy-user`
- **计划基线提交**：`05c3c5da`
- **工作区状态**：包含尚未提交的 Control Plane、Web、部署和远程运行实现
- **依赖**：SaaS Tenant/Organization/User 领域模型、Worker Protocol v1、Runtime Event v1
- **目标结果**：Go Control Plane 达到可供前端长期依赖、支持多副本部署和可靠恢复的生产基线

## 2. 阶段目标

完成后，系统应满足：

1. 登录、Tenant、Organization、Project、Agent Session、Execution 和 Worker API 由 Go
   Control Plane 统一管理。
2. Control Plane 可以运行多个无状态副本，不依赖进程内唯一状态保证正确性。
3. PostgreSQL 是 SaaS 事务状态和 Session Event 的权威来源。
4. S3/MinIO 是企业和单机服务版 Artifact Payload 的权威来源。
5. Outbox 不再只是写表和统计，而是具备可靠 Claim、发布、重试和死信处理。
6. Web 前端具备正式的登录、Tenant/Organization 选择、Project/Session 创建和事件订阅路径。
7. Worker 注册、心跳、Claim、Lease、Generation Fencing 和恢复可在多控制面副本下工作。
8. `personal`、`single-node`、`enterprise` Profile 继续共享同一领域模型和协议。
9. Docker Compose 与双副本 K8s Control Plane 都有可重复的验收流程。
10. 现有 TypeScript Provider Runtime 继续复用，不在本阶段全量重写为 Go。

## 3. 当前实现基线

执行前必须把以下内容视为“已有能力”，先验证再补缺口。

### 3.1 已有 Go Control Plane

位置：`services/control-plane`

现有模块包括：

- Identity、Login Session。
- Tenant、Organization、Membership、Invitation。
- 固定角色 Permission Map。
- Project、Agent Session、Turn、Session Event。
- Execution、Worker、Lease、Generation Fencing。
- Execution Target：Local、SSH、Docker、Kubernetes。
- Local `synara-agentd` Supervisor。
- Artifact Local/S3/MinIO Store。
- Provider Credential Envelope Encryption。
- Quota、Retention、Audit、Service Account。
- OIDC、SAML、SCIM。
- Prometheus Metrics 和基础 Readiness。
- Personal Metadata Export/Import。

### 3.2 已有数据库迁移

当前已有：

```text
000001 identity tenancy audit outbox
000002 projects sessions events
000003 executions workers leases
000004 deployment profiles execution targets
000005 artifacts
000006 tenant quotas
000007 provider credentials
000008 session credential binding
000009 retention policies
000010 enterprise identity
```

### 3.3 已有前端接入

当前 Web 侧已经出现：

- `controlPlaneClient.ts`。
- Tenant/Organization Settings Panel。
- Project/Session Settings Section。
- Execution Target、Quota、Retention、Audit、Identity、Credential、Service Account 设置。
- Dev Login 和企业 SSO 入口。

当前接入主要集中在 Settings，还没有确认普通聊天主流程是否已经完全以 Control Plane 的
Project/Session/Execution 为权威来源。

### 3.4 已有代理与部署

- TypeScript Server 支持同源 `/v1` 和 `/scim` Proxy。
- Proxy 支持 SSE 和附件流式响应，不会把 Event Stream 全量缓冲。
- `deploy/saas` 已有 PostgreSQL、MinIO、Control Plane 和 Synara Compose。
- `deploy/kubernetes` 已有双副本 Control Plane 基础清单和监控资源。
- `deploy/saas/acceptance.sh` 已覆盖 Tenant、Session、SSE、Worker、Lease、Artifact 和隔离流程。

### 3.5 已确认的主要缺口

当前 `outbox_messages` 已经被事务性写入，但仓库中没有完整的 Outbox Publisher/Dispatcher。
现有字段只有：

```text
attempts
available_at
published_at
last_error
```

尚未形成多副本安全的 Claim、发布、失败重试、Claim 超时恢复和死信状态。这是本阶段的
首要可靠性缺口。

## 4. 范围

### 4.1 本阶段必须完成

- 生产登录与 Session 安全收口。
- Tenant/Organization Context 在前端应用级生效。
- Session API 和 Execution API 的幂等性与状态机审计。
- Worker 注册、心跳、Drain、Lease 和版本兼容收口。
- PostgreSQL 多副本并发语义验证。
- S3 Presigned URL 和 Artifact 完成流程收口。
- SSE 多副本断线恢复和代理验证。
- Outbox Publisher/Dispatcher。
- Web 主流程 Control Plane 接入方案和最小切换实现。
- Docker Compose 与 K8s 多副本验收。
- 生产配置、Metrics、Alert 和 Runbook。

### 4.2 本阶段不做

- 全量重写 Codex、Claude、Cursor 等 Provider Adapter。
- 将所有 Worker 通信立即切换成 gRPC。
- 实时双向本地文件同步。
- 自定义 RBAC 角色编辑器。
- 复杂跨 Tenant 资源共享。
- 计费支付系统。
- 将 NATS/Kafka 作为强制依赖。
- 为了微服务化而拆分多个独立部署单元。

## 5. 关键设计不变量

1. 前端只连接 Control Plane 或同源 Proxy，不连接 Worker。
2. Worker 不访问 Control Plane 数据库。
3. Worker 本地状态不是 Agent Session 的权威来源。
4. 所有业务 Repository 查询显式携带 `tenantID`。
5. Agent Session 内 Event Sequence 严格递增。
6. Outbox 采用至少一次投递，消费者必须幂等。
7. Worker Event 必须匹配当前 Worker、Lease Token 和 Generation。
8. 多副本正确性不能依赖进程内 Mutex、Channel 或本地文件。
9. SSE 的进程内 Broker 只能作为低延迟提示，PostgreSQL Event 表始终是恢复来源。
10. Dev Login 在生产环境必须不可用。
11. Provider Credential 明文不得进入日志、Event、Outbox、Artifact Metadata 或命令行参数。
12. Personal Profile 仍保留 Tenant、Organization、Execution、Lease 和 Event 模型。

## 6. 工作流 A：生产认证与 Tenant Context

### A1. 审计现有登录入口

检查：

- `/v1/auth/dev-login`
- `/v1/auth/session`
- `/v1/auth/logout`
- `/v1/auth/active-tenant`
- OIDC/SAML Connection Discovery、Start 和 Callback
- Login Session Cookie 策略

明确每个 Deployment Profile 允许的认证方式：

| Profile | 允许方式 |
| --- | --- |
| `personal` | 自动 Local Owner、显式本地登录 |
| `single-node` | OIDC/SAML，受控环境可启用 Dev Bootstrap |
| `enterprise` | OIDC/SAML/SCIM，禁止 Dev Bootstrap |

### A2. 强制生产配置

实现启动校验：

- `enterprise` 下 `SYNARA_CONTROL_PLANE_DEV_BOOTSTRAP=true` 直接拒绝启动。
- 非 Loopback HTTPS 部署必须使用 Secure Cookie。
- 明确 SameSite、Domain、Path 和 Proxy Header 信任规则。
- `X-Forwarded-*` 只信任配置的反向代理链路。
- 回调 URL 必须根据受信任 Public URL 生成，不能直接信任任意 Host Header。

### A3. Login Session 安全

补齐：

- 登录成功后 Session Token Rotation。
- Logout 和管理员撤销后立即失效。
- 可配置绝对过期和空闲过期。
- Session Fixation 测试。
- 多副本同时认证/撤销的一致性测试。
- Cookie、Token 和 Identity Callback 全链路 `Cache-Control: no-store`。

### A4. 前端 Tenant Context

应用级建立：

- 当前 User。
- 可访问 Tenant 列表。
- Active Tenant。
- Active Organization。
- 权限能力快照。

切换 Tenant 时必须：

- 清理旧 Tenant Query Cache。
- 关闭旧 Tenant SSE Subscription。
- 清理未提交的跨 Tenant Session Draft 或要求用户确认。
- 禁止旧请求结果覆盖新 Tenant 状态。

### A 验收

- Enterprise 无法启用 Dev Login。
- OIDC/SAML 登录成功后生成数据库 Login Session。
- 被撤销的 Session 在所有副本立即失效。
- Tenant 切换后 UI 不显示旧 Tenant Project/Session。
- 未认证接口、公开 SSO Discovery 和 Worker API 边界明确。

## 7. 工作流 B：Session API 收口

### B1. API 契约审计

核对：

```text
GET    /v1/projects/{projectId}/sessions
POST   /v1/projects/{projectId}/sessions
GET    /v1/sessions/{sessionId}
GET    /v1/sessions/{sessionId}/events
GET    /v1/sessions/{sessionId}/events/stream
POST   /v1/sessions/{sessionId}/turns
POST   /v1/sessions/{sessionId}/archive
```

确认错误码、分页、权限和幂等规则与 `docs/contracts/saas-api-conventions.md` 一致。

### B2. 创建操作幂等

为可能被浏览器或代理重试的写入接口增加统一 `Idempotency-Key`：

- Project Create。
- Agent Session Create。
- Turn Create。
- Archive/Cancel 命令。

要求：

- 保存请求 Hash。
- 同 Key 同请求返回原结果。
- 同 Key 不同请求返回 `409 idempotency_conflict`。
- 幂等记录和业务写入在同一事务提交。

### B3. Session 状态机

明确允许状态转换：

```text
active -> archived
active -> suspended
suspended -> active
archived -> retained/deleted
```

禁止由 Worker 直接更新 Agent Session 状态；Worker 只能提交 Runtime Event 和 Execution 结果，
由 Control Plane 投影业务状态。

### B4. Event Sequence

验证：

- 并发 Turn/Create/Worker Event 不产生重复 Sequence。
- 重复 `eventId` 幂等。
- 相同 Sequence 不同 Event ID 必须失败。
- Event Payload 通过 Runtime Event v1 验证。
- 超大 Payload 被拒绝并改用 Artifact Reference。

### B 验收

- Session API 可以被安全重试。
- Event Sequence 在 PostgreSQL 并发测试下无缺口、无重复。
- Archive/Retention 不破坏 Event Replay。
- 跨 Tenant Session 始终返回不可枚举的 Not Found。

## 8. 工作流 C：Execution API 与 Worker 生命周期

### C1. Execution 状态机

冻结：

```text
queued
leased
running
waiting-for-approval
recovering
succeeded
failed
cancelled
interrupted
```

每个转换必须定义：

- 调用主体。
- 前置状态。
- 数据库锁。
- Session Event。
- Outbox Event。
- 审计要求。
- 幂等行为。

### C2. Worker 注册与版本兼容

补齐：

- Worker Protocol Version。
- Worker Image/Build Version。
- Capability Negotiation。
- 最低支持版本。
- Drain 状态。
- Token Rotation 和撤销。
- 重复注册和旧 Token 行为。

远程 Worker 必须继续声明：

```json
{
  "leaseSupported": true,
  "fencingSupported": true
}
```

### C3. Lease 与恢复

验证多副本下：

- 同一 Execution 只有一个 Worker Claim 成功。
- Lease Renew 和 RecoverExpired 不会互相覆盖。
- 旧 Generation 永久失效。
- Worker Heartbeat 过期后先进入 Recovering，不直接丢失 Session。
- Cancel 与 Worker Complete 并发时只有一个合法终态。

### C4. Approval 和 User Input

如果 Worker Protocol 已包含 Approval/Input 命令但 Control Plane 业务 API 尚未完整暴露，则补齐：

- Pending Approval 持久化。
- 用户 Resolve API。
- 权限检查。
- 过期 Lease 下的 Resolve 行为。
- Approval Event Replay。
- 前端 Pending UI 接入边界。

### C 验收

- Worker 注册、心跳、Claim、Renew、Complete、Fail、Release 全部可重试。
- Cancel/Complete、Renew/Recover 并发测试稳定。
- 多副本执行测试不依赖请求落到同一 Pod。
- Worker 版本不兼容时返回稳定错误码和升级提示。

## 9. 工作流 D：PostgreSQL 与无状态多副本

### D1. 进程内状态审计

逐项审计：

- Session Event Broker。
- Background Reconciler。
- Retention Sweeper。
- Worker Receipt Cache。
- Login Session。
- Execution Claim。
- Provider Resume Cursor。
- Artifact Completion。

每一项必须分类为：

```text
authoritative-postgres
authoritative-object-store
best-effort-local-cache
process-local-wakeup-only
```

### D2. 分布式协调

所有周期任务满足其一：

- PostgreSQL Advisory Lock。
- 数据库 Row Claim + `SKIP LOCKED`。
- 外部 Queue Consumer Group。

禁止仅通过 `sync.Mutex` 保证跨副本唯一性。

### D3. 数据库连接与迁移

补齐：

- 最大连接数、空闲连接数和 Connection Lifetime 配置。
- Startup Migration Advisory Lock 超时。
- Migration Checksum 冲突错误说明。
- Readiness 同时检查数据库写能力和必要 Schema Version。
- Graceful Shutdown 停止接收请求后等待短事务完成。

### D4. 两副本故障测试

至少测试：

- 两副本并发创建 Turn。
- 两副本并发 Claim Execution。
- 一个副本处理中退出。
- SSE 连接到副本 A，Event 由副本 B 写入。
- Retention/Reconciler 同时唤醒。
- 登录撤销发生在另一个副本。

### D 验收

- 任意 Control Plane Pod 删除不会丢失权威状态。
- SSE 最迟通过 PostgreSQL Poll 补齐跨副本 Event。
- 所有唯一 Background Job 有分布式协调证据。
- Readiness 不会在缺少数据库或错误 Schema 时返回 Ready。

## 10. 工作流 E：S3/MinIO Artifact 收口

### E1. Presigned Upload

必须保持：

1. 先创建 Pending Artifact Metadata。
2. 上传到临时 Object Key。
3. Complete 时由 Control Plane `Stat` 并重新读取计算 SHA-256。
4. 校验 Size、Hash、Content-Type 和 Tenant/Session Ownership。
5. 验证后提升为 Ready Object。

客户端提交的 Hash 不能作为唯一可信依据。

### E2. 下载授权

- 下载前重新执行 Tenant/Organization/Session Permission。
- Presigned URL 使用短 TTL。
- Pending、Failed、Deleted Artifact 不签发下载 URL。
- 响应不得暴露 Bucket Credential 或内部 Endpoint。

### E3. 多节点一致性

- 多 Control Plane 副本不使用本地 Artifact 缓存作为权威。
- Delete/Retention 与 Download Grant 并发行为明确。
- Complete 重试幂等。
- 临时 Object 清理任务具备分布式锁。

### E 验收

- MinIO 与真实 S3 兼容测试通过。
- 越权 Object Key、伪造 Hash、超限上传均失败。
- Control Plane 重启后 Pending Upload 可以安全继续或过期清理。
- Artifact Payload 不经由 WebSocket Event 内联传输。

## 11. 工作流 F：SSE/WebSocket 事件订阅

### F1. SSE 契约

保持：

- `Last-Event-ID`。
- `afterSequence`。
- 先回放 PostgreSQL Backlog，再进入实时等待。
- Heartbeat Comment。
- `X-Accel-Buffering: no`。
- `Cache-Control: no-store`。

### F2. 多副本行为

现有进程内 Broker 仅用于当前副本快速通知。跨副本 Event 通过定期 PostgreSQL Catch-up
恢复。需要补充：

- Poll Interval 和最大延迟指标。
- 慢客户端 Backpressure。
- 单用户/单 Tenant 最大 SSE 连接数。
- Proxy/Ingress Idle Timeout 指南。
- 控制面滚动发布时客户端自动重连。

### F3. Web 接入

提供统一 Hook/Client：

```text
subscribeSessionEvents(sessionId, afterSequence)
```

要求：

- Tenant 切换时取消旧订阅。
- 重连使用最后成功持久化 Sequence。
- 重复 Event 去重。
- Gap 自动请求 Backlog。
- 不把“已连接 SSE”当作 Session 正在运行。

### F 验收

- Event 由另一个副本写入时客户端仍可收到。
- Proxy、Ingress 和浏览器断线后能够无重复恢复。
- 慢客户端不会阻塞 Event 写入事务。
- SSE 连接数和 Catch-up 延迟有 Metrics。

## 12. 工作流 G：可靠 Outbox 和任务分发

这是本阶段最高优先级实现工作。

### G1. 扩展 Outbox Schema

新增迁移，例如：

```text
000011_outbox_delivery.sql
```

建议字段：

```text
claimed_by
claimed_at
claim_expires_at
published_at
dead_lettered_at
attempts
available_at
last_error
```

增加索引：

- Pending + Available。
- Expired Claim。
- Dead Letter 查询。

### G2. Outbox Claim

实现 `internal/outbox`：

1. PostgreSQL 使用 `FOR UPDATE SKIP LOCKED` 批量 Claim。
2. Claim 事务只负责占有记录，不在数据库事务中执行外部网络发布。
3. 发布成功后按 `claimed_by` 条件标记 `published_at`。
4. 发布失败增加 Attempts、记录安全的错误摘要并计算下一次 `available_at`。
5. Claim 超时后允许其他副本重新领取。

### G3. 投递语义

采用至少一次投递：

- Outbox Message ID 是消费幂等键。
- Consumer 必须支持重复消息。
- 不承诺跨 Topic 的全局顺序。
- 同一 Session/Execution 的 Message Key 顺序必须明确。

### G4. Driver

定义：

```go
type Publisher interface {
    Publish(ctx context.Context, message Message) error
}
```

实现顺序：

1. `postgres-outbox` Dispatcher。
2. 测试用 Memory Publisher。
3. External Queue Adapter 接口。
4. NATS/Kafka 具体实现延后，除非 Enterprise 首发明确需要。

Personal Profile 的 `in-process` Queue 仍必须遵循同一 Message Contract，但可以在单副本中使用
简化 Dispatcher。

### G5. Dead Letter 与运维

- 指数退避并加入抖动。
- 最大 Attempts 可配置。
- 超限进入 Dead Letter。
- 提供 Metrics 和管理员只读查询。
- 提供受审计的 Replay 操作。
- 错误摘要禁止包含 Credential、Token 或完整 Payload。

### G6. 生命周期接入

至少发布：

```text
execution.queued
execution.recovering
execution.cancelled
session.archived
worker.offline
artifact.ready
```

业务事务和 Outbox Insert 必须保持原子提交。

### G 验收

- 两个 Dispatcher 副本不会同时成功 Claim 同一消息。
- Publisher 在“实际发送成功但确认前崩溃”时允许重复投递，Consumer 不产生重复副作用。
- Claim 进程崩溃后记录可恢复。
- Retry、Dead Letter、Replay 有集成测试。
- Pending/Oldest Age/Retry/Dead Letter 有 Metrics 和告警阈值。

## 13. 工作流 H：Web 主流程 Control Plane 切换

### H1. 应用级 Provider

将当前 Settings 内的 Control Plane Query 提升为应用级 Context：

- Authentication State。
- Active Tenant。
- Active Organization。
- Platform Profile。
- Permission Capabilities。
- Control Plane Availability。

避免多个页面各自重复请求 Session/Tenant 并产生不一致缓存。

### H2. Project/Session 主路径

确定并实现最小权威切换：

1. Project 创建先写 Control Plane。
2. Agent Session 创建先写 Control Plane。
3. Turn 创建生成 Control Plane Execution。
4. SSE Event 驱动远程 Session 状态。
5. TypeScript Provider Runtime 作为 Worker/Provider Host 执行，不再作为 SaaS Session Metadata
   的唯一来源。

如果当前聊天 Store 仍依赖本地 Orchestration Snapshot，必须明确过渡投影：

```text
Control Plane Session/Event
    -> Web SaaS Projection Adapter
    -> 现有 Thread UI Model
```

禁止让 Web 同时把 Go Session 和本地 Thread 都当权威状态。

### H3. 降级策略

- 未配置 Control Plane：保持本地 Synara 行为。
- Control Plane 暂时不可用：显示明确状态，不创建只存在前端的远程 Session。
- 已登录但 Tenant Suspended：阻止新 Turn，允许受策略控制的只读访问。
- SSE 断开：进入 Reconnecting，不把 Session 标为 Completed。

### H4. Proxy 边界

继续使用 TypeScript Server 的同源 `/v1` Proxy 作为过渡，验证：

- Cookie 转发。
- 多个 `Set-Cookie`。
- SSE 不缓冲。
- 大文件下载不缓冲。
- `X-Forwarded-*` 安全。
- Upstream Timeout 不误杀长 SSE。

### H 验收

- 用户可以从主界面完成登录、选择 Tenant/Organization、创建 Project/Session 和发送 Turn。
- 页面刷新后通过 PostgreSQL/SSE 恢复 Session。
- Control Plane 不可用时不会生成孤儿 Session。
- 本地模式没有回归。

## 14. 工作流 I：可观测性和运维

### I1. Metrics

至少包含：

```text
HTTP request count/latency/status
DB pool usage/wait
login success/failure
active login sessions
queued/running/recovering executions
worker online/offline/stale
lease renew/fencing rejection
session event append latency
SSE connections/catch-up delay
artifact upload/complete/failure/bytes
outbox pending/oldest/retry/dead-letter
background job duration/failure
```

### I2. Structured Logging

统一字段：

```text
requestId
traceId
tenantId
organizationId
sessionId
executionId
workerId
generation
errorCode
```

不得默认记录：

- Prompt 全文。
- Provider Credential。
- Login/Worker/Lease Token。
- Presigned URL Query。
- SAML Assertion、OIDC Token。

### I3. Alerts

至少覆盖：

- Control Plane Not Ready。
- PostgreSQL Connection Saturation。
- Outbox Oldest Age。
- Dead Letter 增长。
- Worker Offline 激增。
- Lease Recovery 激增。
- Artifact Failure Rate。
- SSE Catch-up Delay。

### I4. Runbook

新增运维说明：

- 数据库迁移失败。
- Outbox 堆积。
- Worker 全部离线。
- S3/MinIO 不可用。
- K8s Reconciler 权限不足。
- Provider Credential KMS 失败。
- 双副本滚动升级。

## 15. 实施顺序

### Step 0：Drift Audit

- 读取当前所有 Go Migration、Contract 和 API Route。
- 运行只读代码搜索，确认每项能力的真实实现状态。
- 将任务标记为 `already-implemented`、`partial` 或 `missing`。
- 不因为计划中的旧描述而覆盖更完整的现有实现。

### Step 1：Outbox Publisher

- 新增 Outbox Delivery Migration。
- 实现 Claim、Publish、Retry、Dead Letter。
- 接入 Metrics 和 Graceful Shutdown。
- 添加 PostgreSQL 并发测试。

### 当前执行进度（2026-07-12）

- [x] Step 0 Drift Audit：已完成，结果记录在
  `docs/plans/stage-2-drift-audit.md`；工作流 A-F、H-I 为 `partial`，工作流 G 为 `missing`。
- [x] Step 1 Reliable Outbox：已完成 Delivery Migration、PostgreSQL 多副本安全 Claim、Claim
  超时恢复、至少一次投递、指数退避、Dead Letter、审计 Replay、Tenant 运维 API、权限、Metrics、
  Alert 和 Contract。
- [x] Outbox 已接入 `execution.queued`、`execution.recovering`、`session.archived`、
  `worker.offline`、`artifact.ready` 生命周期事务。
- [x] Go 单元测试和真实 PostgreSQL 17 集成测试通过，包括双 Dispatcher Claim 唯一性与过期恢复。
- [x] Single-node SaaS Compose Acceptance 通过，覆盖 Outbox 发布结果，并修复 Synara Web 镜像错误
  落到 `worker` Stage 的 Docker 构建缺陷。
- [x] Step 2 多副本正确性：已完成进程内状态分类、Background Job Advisory Lock 审计、数据库连接池
  配置、Migration Lock 超时、Schema/写能力 Readiness、跨副本 SSE Catch-up、并发 Turn/Claim、跨副本
  Login Session 撤销和单副本退出恢复。
- [x] 可重复的双副本 Compose Acceptance 已通过，报告见
  `docs/reports/stage-2-multi-replica-acceptance.md`。
- [x] Step 3 生产认证收口：已完成 Profile 启动策略、Cookie/Public URL/可信代理规则、绝对与空闲
  Session 过期、登录 Token Rotation、管理员审计撤销，以及独立 PostgreSQL 连接池下的跨副本撤销和
  Authenticate/Revoke 并发测试。
- [x] 生产认证改动后的双副本 Compose Acceptance 已重新通过；验收客户端改为 Python 标准库，
  不再重复构建 TypeScript Provider Runtime，也不依赖测试容器在线安装工具。
- [x] Step 4 Session/Execution API 幂等：已完成 Project/Session/Turn、Suspend/Resume/Archive、
  Execution Cancel 和 Approval/User Input Resolve 的事务型 `Idempotency-Key`，同 Key 冲突返回稳定
  `409`，跨 PostgreSQL 连接池并发只执行一次副作用。
- [x] Session 状态机已补齐 active/suspended/archived；Execution 已补齐用户 Cancel、
  waiting-for-approval、Cancel/Complete 终态竞争、Worker Protocol Version、Drain 和重新注册 Token
  Rotation 语义。
- [x] Approval/User Input 请求与 Runtime Event 原子持久化；Resolve 校验 Lease/Generation、拒绝过期
  Lease 并生成可回放 resolved Event。Provider Runner 双向投递明确进入 Stage 3。
- [x] Step 4 后双副本 Compose Acceptance 已通过，包含跨副本 Project/Session Replay 和同 Key Turn
  并发。
- [x] Step 5 Artifact/SSE 实现收口：Artifact 临时 Key 提升、Pending/Ready 过期临时对象分布式清理、
  伪造 Hash 拒绝、真实 MinIO Presigned 生命周期、SSE PostgreSQL 全局 Tenant/User 连接租约、慢客户端
  写超时、Catch-up/连接/Artifact/DB Pool Metrics 和告警已完成。
- [x] Step 5 默认 SQLite、完整 PostgreSQL 17 + MinIO、单节点 Control Plane Acceptance 和双副本
  Compose Acceptance 已通过；独立 PostgreSQL 连接池并发 SSE 配额只有一个合法赢家。
- [ ] 真实 AWS S3 Live Store 验收需要操作人提供明确授权的可写测试 Bucket；共享
  `SYNARA_TEST_S3_*` 测试入口已完成。在此之前不宣称真实 AWS S3 已验收。
- [x] Step 6 Web 主流程切换：应用级 Authentication/Tenant/Organization Context、Control Plane
  Project/Session/Turn 主路径、SSE 到 Thread UI 的单向投影和 Control Plane Projection Authority 已完成。
- [x] SaaS 浏览器闭环通过：登录、Context、Project、Session、Turn、Worker Event/Complete、SSE 输出和
  PostgreSQL 刷新恢复均正常；延迟本地 Snapshot 不会覆盖 SaaS Projection。
- [x] Settings 空闲预热不再调用会创建临时 Match 的 `preloadRoute`，改为只加载生成的 Route Chunk；
  三个全新浏览器标签在预热、导航和刷新后均无相关 TanStack Router Warning/Error。
- [x] 未配置 Control Plane 的隔离本地实例通过：无登录门、可从真实路径创建 Project、刷新后从 SQLite
  Snapshot 恢复，Console 无相关错误。
- [x] Step 7 部署与生产验收：Single-node、双副本 Compose、PVC-backed Kind 双副本、Pod 删除、数据库
  和 MinIO 暂时不可用、Worker 失联/Generation 接管均已通过。
- [x] Control Plane Operations Runbook、Stage 2 Release Checklist、生产验收报告和随机 Sentinel
  Credential/Token/Prompt 日志泄漏审计已完成。
- [x] Stage 2 仓库内实现与可控环境验收完成。真实 AWS S3 是目标部署使用 AWS 时的外部发布证据，
  仍保持未执行状态，不能以 MinIO 结果替代。
- [x] 当前 `1a53c93a` 基线在迁移扩展到 `000028` 后重新通过完整 Go/Race、四套独立 PostgreSQL
  集成数据库、Single-node、双副本 Compose、故障注入和 Kind 双副本验收；验收脚本已同步 Worker
  Protocol v2 Heartbeat 与 ready Workspace 完成约束，当前报告见
  `docs/reports/stage-2-production-acceptance-1a53c93a.md`。

### Step 2：多副本正确性

- 审计进程内状态。
- 补齐 Background Job 分布式锁。
- 添加双副本 Integration/Acceptance。
- 验证 SSE 跨副本 Catch-up。

### Step 3：生产认证收口

- Profile 认证策略验证。
- Cookie、Public URL、Proxy 信任收口。
- Session Rotation/Revocation 测试。
- Enterprise Dev Login Fail-fast。

### Step 4：Session/Execution API 幂等

- 统一 `Idempotency-Key`。
- 冻结状态机和稳定错误码。
- 补充 Approval/Input 持久化缺口。
- 增加并发终态测试。

### Step 5：Artifact 和 SSE 收口

- 完成临时 Key 提升与清理验证。
- 增加真实 MinIO/S3 兼容测试。
- 增加 SSE 连接限制、指标和重连测试。

### Step 6：Web 主流程切换

- 建立应用级 Control Plane Context。
- 接入主 Project/Session/Turn 流程。
- 建立 SaaS Event 到现有 UI Model 的单向投影。
- 保留未配置 Control Plane 时的本地模式。

### Step 7：部署与生产验收

- Single-node Compose 验收。
- 双副本 K8s Control Plane 验收。
- Pod 删除、数据库短暂不可用、S3 故障和 Worker 失联测试。
- 完成 Runbook 和 Release Checklist。

## 16. 预计修改区域

### Go Control Plane

```text
services/control-plane/cmd/api/main.go
services/control-plane/internal/config/
services/control-plane/internal/identity/
services/control-plane/internal/httpapi/
services/control-plane/internal/sessions/
services/control-plane/internal/executions/
services/control-plane/internal/outbox/              # 新增
services/control-plane/internal/observability/
services/control-plane/internal/persistence/
services/control-plane/migrations/000011_*.sql       # 新增
```

### Web

```text
apps/web/src/lib/controlPlaneClient.ts
apps/web/src/components/settings/*
apps/web/src/bootstrap.ts
apps/web/src/store.ts 或独立 SaaS Projection Store
apps/web/src/routes/_chat.*
apps/web/src/components/ChatView.tsx
```

### TypeScript Proxy

```text
apps/server/src/controlPlaneProxy.ts
apps/server/src/controlPlaneProxy.test.ts
apps/server/src/config.ts
apps/server/src/http.ts
```

### 部署和文档

```text
deploy/saas/
deploy/kubernetes/
docs/contracts/
docs/runbooks/                              # 建议新增
services/control-plane/README.md
```

## 17. 测试计划

### 17.1 Go 单元测试

```bash
cd services/control-plane
go test ./...
```

重点：

- Outbox Backoff/Claim/Dead Letter。
- Login Session Rotation。
- Worker Version/Capability。
- Idempotency-Key Hash Conflict。
- Event Sequence。
- Artifact Complete。

### 17.2 Go Race Test

在适合的平台运行：

```bash
cd services/control-plane
go test -race ./...
```

### 17.3 PostgreSQL 集成测试

必须使用真实 PostgreSQL，不以 SQLite 结果代替：

- `FOR UPDATE SKIP LOCKED`。
- Advisory Lock。
- 并发 Event Sequence。
- 并发 Worker Claim。
- Quota Lock。
- Outbox Claim Recovery。

### 17.4 Web/Proxy Focused Tests

```bash
cd apps/server
bun run test src/controlPlaneProxy.test.ts

cd apps/web
bun run test src/lib/controlPlaneClient.test.ts
```

后续根据实际新增文件补充 Tenant Context、SSE Reconnect 和主流程测试。

### 17.5 Single-node Acceptance

```bash
docker compose --env-file deploy/saas/.env -f deploy/saas/docker-compose.yml up -d --build
deploy/saas/acceptance.sh http://127.0.0.1:3773
```

### 17.6 Enterprise 多副本 Acceptance

至少验证：

- 两个 Control Plane Pod Ready。
- 请求可落到不同副本。
- Worker Claim 唯一。
- SSE 跨副本恢复。
- 一个 Pod 删除不影响 Session。
- Migration 只执行一次。
- Outbox Claim 不重复占有。

### 17.7 仓库级检查

只有操作人在当前对话明确要求时才运行：

```bash
bun fmt
bun lint
bun typecheck
```

测试必须使用：

```bash
bun run test
```

禁止使用 `bun test`。

## 18. 完成标准

- [x] Enterprise Profile 无法启用 Dev Login。
- [x] 登录 Cookie、Public URL 和 Proxy Trust 配置安全。
- [x] Tenant Context 在 Web 应用级统一管理。
- [x] Project、Session、Turn 创建具备幂等语义。
- [x] Execution 状态机和并发终态已冻结并测试。
- [x] Worker Protocol Version、Drain 和 Token Rotation 有明确实现。
- [x] 多 Control Plane 副本不依赖进程内权威状态。
- [x] SSE 能跨副本、跨重启从 PostgreSQL Sequence 恢复。
- [x] Artifact Complete 由 Control Plane 独立验证 Object 内容。
- [x] Outbox 具备 Claim、Retry、Dead Letter、Replay 和 Metrics。
- [x] Web 主流程可以创建 Control Plane Session/Execution 并消费 Event。
- [x] 未配置 Control Plane 时本地模式保持可用。
- [x] Single-node Compose Acceptance 通过。
- [x] Enterprise 双副本 Acceptance 通过。
- [x] 生产 Runbook 和告警规则完成。
- [x] 未引入 Credential、Token、Prompt 的日志泄漏。

## 19. STOP 条件

出现以下情况时暂停并重新评审：

- 无法确定 Go Session 和本地 Thread 哪个是 SaaS 权威状态。
- Web 必须直接连接 Worker 才能实现主流程。
- Outbox Consumer 无法设计为幂等。
- Outbox Publisher 需要在数据库事务中长时间执行网络请求。
- 多副本正确性仍依赖进程内 Mutex、Channel 或本地文件。
- Enterprise 登录只能依赖 Dev Bootstrap。
- S3 Complete 无法由 Control Plane 独立验证 Hash/Size。
- Worker Credential 或 Lease Token 必须传递给 Provider Runner。
- 同一个 Execution 可能被两个有效 Generation 同时写入。
- PostgreSQL Integration Test 环境无法覆盖实际并发语义。
- 为了阶段交付需要全量重写现有 Provider Adapter。

## 20. 交付物

完成本阶段应产生：

1. Go Control Plane Production Baseline。
2. Outbox Delivery Migration 和 Dispatcher。
3. Production Authentication Policy。
4. Session/Execution 状态机文档。
5. Worker Version/Capability Contract。
6. Web Control Plane Application Context。
7. 主聊天路径的 SaaS Projection Adapter。
8. Single-node Acceptance Report。
9. Enterprise Multi-replica Acceptance Report。
10. Control Plane Operations Runbook。
11. Metrics 和 Alert 清单。
12. 更新后的 `services/control-plane/README.md` 和相关 Contract。
