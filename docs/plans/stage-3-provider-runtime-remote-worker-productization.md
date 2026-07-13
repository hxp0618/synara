# SaaS 路线第三阶段：Provider Runtime 与远程 Worker 产品化计划

> **阶段命名说明**：这里的“第三阶段”对应产品路线中的“Provider Runtime 与远程 Worker
> 产品化”。它不等同于 `docs/plans/saas-tenancy-organization-user-plan.md` 中技术 Phase 3 的
> “Execution、Worker、Lease”。
>
> **执行要求**：先审计当前工作区，不得重新实现已经存在的 `provider-host`、
> `synara-agentd`、Worker Protocol、Artifact、Credential、Docker/Kubernetes Worker 闭环。
> 本阶段是在现有基础上补齐 Provider 一致性、协议演进、远程 Workspace、升级恢复和主流程
> 权威切换。
>
> **验证规则**：优先运行修改区域的 Focused Test。仓库级 `bun fmt`、`bun lint`、
> `bun typecheck` 只有在当前对话中由操作人明确要求时才运行。禁止使用 `bun test`，只能使用
> `bun run test`。

## 1. 状态

- **优先级**：P0
- **预计工作量**：XL
- **风险**：HIGH
- **计划基线分支**：`codex/saas-tenancy-user`
- **最近稳定检查点**：`e88563e9`（Stage 2 已验收并推送；Durable Interaction Web 权威接线已完成）
- **工作区状态**：Stage 3 持续执行中，执行时以当前分支和已验证证据为准
- **依赖**：Stage 2 的 Control Plane Session/Execution 权威、Worker Lease/Fencing、Artifact、SSE
- **目标结果**：所有正式支持的 Provider 可以通过统一 Provider Host 和 Worker Contract，稳定运行在
  Local、SSH、Docker、Kubernetes Execution Target，并能跨 Worker/Pod 恢复后续 Turn

## 2. 阶段目标

完成后，系统应满足：

1. Control Plane 只依赖版本化的 Worker Protocol、Provider Host Protocol 和 Runtime Event，
   不依赖具体 Provider SDK、CLI 输出或本地状态目录。
2. Codex、Claude、Cursor、Gemini、Grok、Kilo、OpenCode、Pi 都有经过验证的能力矩阵；
   未支持能力返回稳定、可解释的错误，不静默降级。
3. Start、Resume、Send、Steer、Interrupt、Compact、Rollback、Fork、Review、Approval 和
   Structured User Input 具备跨 Provider 的统一业务语义。
4. PostgreSQL 中的 Agent Session、Turn、Session Event 和 Execution 是 SaaS 权威状态；
   Provider Resume Cursor 只是加密的恢复优化，不是唯一历史来源。
5. Worker/Pod 可以重启、Drain、升级、回滚或迁移，已持久化 Event、Artifact、审批状态和后续
   Turn 不丢失。
6. Remote Workspace 的 Clone、Fetch、Branch、Worktree、Checkpoint、Cleanup 和恢复具备明确
   生命周期，不依赖人工登录 Worker 排查。
7. Provider Credential、Worker Token、Lease Token 和 Git Credential 保持隔离，不进入
   Runner Input、命令行参数、Event、Artifact Metadata 或日志。
8. Web 主聊天流程在启用 Control Plane 时只使用一个 SaaS Session 权威来源；未启用时继续支持
   本地个人模式，两种模式共享领域 Contract 和 UI Projection。
9. Local、SSH、Docker、Kubernetes 使用同一套 Provider Acceptance Suite，不维护四套 Provider
   实现。
10. Worker Image、Provider CLI/SDK 和 Protocol 版本可追溯、可重复构建、可灰度和可回滚。

## 3. 当前实现基线

执行前必须把以下内容视为已有能力，先验证再补差距。

### 3.1 已有本地 Provider Runtime

`apps/server/src/provider` 当前已经存在：

- Codex Adapter。
- Claude Agent Adapter。
- Cursor Adapter。
- Gemini Adapter。
- Grok Adapter。
- Kilo/OpenCode Adapter。
- Pi Adapter。
- Provider Adapter Registry、Discovery、Health 和 Session Directory。
- ACP 支持、Runtime Event 标准化、Plan Mode、Approval、User Input、Review、Skills、Plugins、
  Commands、Model Discovery 等不同程度的实现。

这些实现是 Provider 行为和兼容策略的重要参考，不代表已经全部满足远程 Worker 产品要求。

### 3.2 已有 Provider Host v1

位置：`apps/provider-host`

当前 v1 已具备：

- Codex：通过 `codex exec --json` 执行，并读取原生 Resume Cursor。
- Claude：通过 `claude --print --output-format stream-json` 执行，并读取 Session ID。
- 一行一个 JSON Request/Message 的 Runner 边界。
- Provider Credential 通过匿名文件描述符 3 注入。
- Codex/Claude Credential 字段严格 Allowlist。
- 移除 Worker Token、Lease Token、Control Plane URL 和 agentd 内部环境变量。
- Provider 输出和错误 Secret Redaction。
- Provider 原生事件向 Runtime Event 的基础归一化。
- 存在持久化历史时进行有界的权威对话重建。

Provider Host v1 当前不应被描述为八个 Provider 的完整远程运行时。

### 3.3 已有 `synara-agentd`

位置：`services/control-plane/internal/agentd`

当前已具备：

- Worker 注册、心跳、Execution Claim、Lease Renew 和终态上报。
- 从 Claim Workload 构造 Runner Input。
- 当前 Lease 下的 Provider Credential 解析。
- JSONL Runner Event、Artifact、Result 协议。
- Artifact 上传和 Control Plane 确认。
- Workspace Path Containment 和 Symlink Escape 拒绝。
- Provider Resume Cursor 持久化。
- Local Supervisor、异常重启和 Control Plane Shutdown 处理。

### 3.4 已有 Worker 与 Execution Target

- Local、SSH、Docker、Kubernetes Execution Target 已使用统一 Worker Protocol 基础。
- Worker Image 已包含 Node.js 24、`synara-agentd`、`provider-host`、Codex CLI 和 Claude CLI。
- Worker 以非 Root 用户运行，不内置 Worker、Lease、Provider 或云 Credential。
- Docker 和 kind Kubernetes 已有真实 Worker 执行、Pod 删除、后续 Turn 连续性验收记录。

### 3.5 已有权威数据边界

- PostgreSQL/Metadata Repository：Agent Session、Turn、Execution、Session Event、Lease、Resume
  Cursor Metadata。
- S3/MinIO/Local Artifact Store：附件、生成文件、长日志、快照和 Checkpoint Payload。
- Worker 本地 Workspace：可丢弃的执行缓存和工作副本，不是业务权威来源。

### 3.6 当前主要差距

1. Provider Host v1 仅覆盖 Codex/Claude 的基础单轮执行，和本地 Provider Adapter 的完整能力
   存在明显差距。
2. Provider Host、Worker 和 Control Plane 之间尚未冻结完整版本协商、最低兼容版本和
   Capability Negotiation。
3. Approval、Structured User Input、Plan Mode、Review、Steer、Compact、Rollback、Fork 尚未
   形成跨 Worker 的统一远程生命周期。
4. Remote Workspace/Git 生命周期尚未形成完整 Contract 和恢复策略。
5. Worker Image 中 Provider CLI/SDK 的版本、兼容性、升级和回滚机制需要生产化。
6. Web 普通聊天主路径是否完全以 Go Control Plane 为唯一 SaaS 权威来源仍需完成 Stage 2
   切换和验收。
7. Local、SSH、Docker、Kubernetes 尚缺同源、可重复的 Provider Acceptance Suite。

## 4. 范围

### 4.1 本阶段必须完成

- Provider 能力矩阵和正式支持等级。
- Provider Host Protocol v2 与版本/能力协商。
- Codex、Claude 远程能力补齐。
- Cursor、Gemini、Grok、Kilo、OpenCode、Pi 的远程 Adapter 接入或明确阻断。
- Start/Resume/Send/Steer/Interrupt/Compact/Rollback/Fork/Review 语义统一。
- Approval、Structured User Input 和 Plan Mode 持久化闭环。
- Runtime Event 映射、版本演进和未知事件兼容。
- Authoritative History、Resume Cursor 和跨 Worker 恢复规则。
- Provider/Git Credential Scope 和进程 Secret 隔离。
- Remote Git/Workspace 生命周期。
- Terminal、Log、Generated File、Diff、Checkpoint 的 Artifact/Event 投影。
- Worker Drain、Graceful Shutdown、升级、回滚和版本隔离。
- Worker Image Manifest、SBOM 基础和可重复构建输入。
- Web SaaS Session Projection 与主聊天权威切换。
- Local/SSH/Docker/Kubernetes 共用 Acceptance Suite。
- 崩溃、断网、Pod 替换、Provider 异常和 Control Plane 滚动升级连续性测试。

### 4.2 本阶段不做

- 将 Go Control Plane 改写成 Provider SDK Host。
- 将全部 TypeScript Provider Adapter 重写为 Go。
- 为每种 Execution Target 维护独立 Provider 实现。
- 强制引入 gRPC、NATS 或 Kafka 才能完成 Provider Runtime。
- 多集群调度、Warm Pool、跨 Region Placement 和公平调度；这些属于 Stage 4。
- 企业计费、成本分摊、完整运营后台和 GA 合规；这些属于 Stage 5。
- 实时双向同步用户本地目录。
- 把完整 Terminal Stream、仓库内容或生成文件直接写入 Session Event JSON。
- 承诺所有 Provider 都具有相同的原生能力。
- 通过伪造成功结果隐藏 Provider 不支持的能力。

## 5. 关键设计不变量

1. 前端只连接 Control Plane 或同源 Proxy，不连接 Worker/Provider Host。
2. Control Plane 不解析 Provider 原生 stdout、JSON-RPC 或 SDK Event。
3. `synara-agentd` 不访问 Control Plane 数据库。
4. Provider Runner 不接收 Worker Token、Lease Token、对象存储 Credential 或数据库 Credential。
5. Provider Resume Cursor 必须加密存储，且不得成为恢复 Session 的唯一数据。
6. 有持久化历史时，后续 Turn 必须能在没有原 Worker 本地 Provider State 的情况下继续。
7. Worker 上报必须匹配当前 Worker ID、Lease Token 和 Generation。
8. Provider Capability 必须显式声明为 `native`、`emulated` 或 `unsupported`，不得以“可能可用”
   作为生产行为。
9. `emulated` 行为必须在 Contract 中定义，不能由 UI 临时拼接。
10. Unknown Command 必须拒绝；Unknown Event 必须按版本策略保留、忽略或降级展示，不能导致
    Worker Crash Loop。
11. 大 Payload 只通过 Artifact Reference 进入 Event。
12. Worker 本地 Workspace 和 Provider State 允许随时丢失。
13. Personal、Single-node、Enterprise 共享同一 Provider/Session/Execution 状态机。
14. Local、SSH、Docker、Kubernetes 共享同一 Worker/Provider Host Contract。
15. Provider CLI/SDK 升级不得隐式改变 Runtime Event 或数据库业务语义。

## 6. 工作流 A：Provider 能力矩阵与支持等级

### A1. 定义能力状态

每个 Provider 的每项能力必须标记为：

| 状态 | 含义 | 用户行为 |
| --- | --- | --- |
| `native` | Provider 原生协议直接支持 | 使用原生能力并保留原生引用 |
| `emulated` | Synara 可基于权威历史/Artifact 安全模拟 | UI 明确使用统一行为，不承诺原生细节 |
| `unsupported` | 无法可靠实现 | 请求前禁用或返回稳定 Unsupported 错误 |
| `experimental` | 尚未达到正式 SLA | 仅显式开启，不能作为默认能力 |

`experimental` 是发布等级，不替代前三种行为状态。例如某能力可以是“实验 Provider 的
原生 Fork”。

### A2. 定义正式支持等级

| 等级 | 要求 |
| --- | --- |
| Tier 1 | 核心能力、远程恢复、Credential、四类 Target 验收全部通过 |
| Tier 2 | 核心聊天与恢复通过，部分高级能力明确 Unsupported |
| Experimental | 只在 Feature Flag 下启用，不承诺升级兼容 |
| Local-only | 保留现有本地 Adapter，但远程 Target 不允许选择 |

Stage 3 开始时建议：

- Codex、Claude 作为首批 Tier 1 候选。
- Cursor、Gemini、Grok、Kilo、OpenCode、Pi 先完成审计，再决定 Tier 1、Tier 2、Experimental
  或 Local-only。
- 不因现有本地 Adapter 文件存在就自动标记为远程支持。

### A3. 冻结能力维度

能力矩阵至少覆盖：

```text
discovery
start-session
resume-session
send-turn
steer-turn
interrupt-turn
approval
structured-user-input
plan-mode
review
compact
rollback
fork
read-history
model-list
model-switch
skill-discovery
skill-mentions
plugin-discovery
plugin-mentions
native-commands
tool-events
diff-events
usage-events
checkpoint
credential-injection
authoritative-history-reconstruction
worker-migration
```

### A4. 形成机器可读清单

不要只维护 Markdown 表格。Provider Host 或共享 Contract 应提供机器可读的 Capability
Descriptor，至少包含：

```json
{
  "provider": "codex",
  "adapterVersion": "...",
  "providerCliVersion": "...",
  "capabilities": {
    "send-turn": "native",
    "fork": "emulated",
    "structured-user-input": "unsupported"
  }
}
```

静态声明必须由 Acceptance Test 校验，避免声明和实现漂移。

### A 验收

- 八个 Provider 均有审计记录和支持等级。
- UI、Control Plane 和 Worker 使用同一份 Capability ID，不各自维护字符串。
- 每个 `native`/`emulated` 能力都有自动化测试。
- 每个 `unsupported` 能力有稳定 Error Code 和用户可理解文案。
- Provider CLI 不可用或版本不兼容时不会仍显示为可调度。

## 7. 工作流 B：Provider Host Protocol v2

### B1. 协议分层

保持三个独立版本，禁止混为一个版本号：

```text
Worker Protocol Version
Provider Host Protocol Version
Runtime Event Version
```

- Worker Protocol：Control Plane 与 agentd 的执行、Lease、Event、Artifact 边界。
- Provider Host Protocol：agentd 与 provider-host 的命令、交互请求和终态边界。
- Runtime Event：Provider 行为映射到业务事件的 Payload Contract。

### B2. Provider Host 握手

Provider Host v2 启动后应支持 `hello/describe`，返回：

- Protocol Major/Minor。
- Host Build Version。
- Adapter Version。
- Provider CLI/SDK Version。
- Provider Capability Descriptor。
- 最大 Message/Payload 大小。
- 支持的 Runtime Event Version 范围。
- 支持的 Credential Delivery Mode。
- 支持的 Resume Strategy。

版本规则：

- Major 不兼容：拒绝执行并标记 Worker 不可调度。
- Minor 向后兼容：忽略未知可选字段，保留已知必填字段。
- Capability 缺失：该能力为 Unsupported，不允许猜测。
- Worker 注册时上报摘要，Claim 前再次校验实际 Host/Provider 组合。

### B3. 通用 Command Envelope

所有命令至少包含：

```text
requestId
protocolVersion
executionId
generation
commandType
commandId
occurredAt
payload
```

Provider Host 不需要 Tenant 数据库权限；只接收本次执行所需的只读 Workload Snapshot。

建议 v2 命令集合：

```text
Describe
StartSession
ResumeSession
SendTurn
SteerTurn
InterruptTurn
ResolveApproval
ResolveUserInput
CompactSession
RollbackSession
ForkSession
StartReview
StopSession
```

### B4. 通用 Message Envelope

Provider Host 输出分为：

```text
Event
InteractionRequest
ArtifactCandidate
Checkpoint
Result
Error
Heartbeat/Progress
```

约束：

- 每个 Command 恰好一个终态 `Result` 或 `Error`。
- Event/Interaction/Artifact 可以在终态前出现多次。
- `commandId` 和 `requestId` 用于幂等和关联。
- 超大 Message 被拒绝并要求改为 Artifact。
- stdout 只允许协议 JSONL；诊断信息进入受限 stderr 并脱敏。
- Malformed Message、重复终态和终态后输出属于 Protocol Violation。

### B5. 稳定错误模型

至少冻结：

```text
provider_not_installed
provider_version_incompatible
capability_unsupported
credential_missing
credential_invalid
authentication_required
session_resume_invalid
session_resume_expired
provider_rate_limited
provider_unavailable
workspace_invalid
protocol_violation
cancelled
interrupted
internal_error
```

错误必须声明：

- 是否可重试。
- 是否需要新 Execution。
- 是否需要用户动作。
- 是否可以使用权威历史重建。
- 是否允许换 Worker/Target。
- 安全的用户文案，不包含原始 Secret 或完整 Provider stderr。

### B 验收

- agentd 可以在执行前拒绝不兼容的 Provider Host/CLI。
- v1 Worker 与 v2 Control Plane 的兼容边界有明确测试或明确拒绝。
- Unknown Optional Field 不导致旧 Minor 版本崩溃。
- Unknown Command、缺少必填 Capability 和 Major 不兼容均返回稳定错误。
- Provider Host Crash、Malformed JSONL 和超大 Message 被 agentd 正确分类。

## 8. 工作流 C：统一 Session 与 Turn 语义

### C1. Start 与 Resume

统一定义：

- `StartSession`：为 Agent Session 创建 Provider Runtime Binding，不创建新的 SaaS Agent
  Session。
- `ResumeSession`：在当前 Execution 中恢复 Provider Runtime；可以使用原生 Cursor，也可以使用
  权威历史重建。
- Provider Runtime Binding 可以随 Worker 更换，但 Agent Session ID 不变。
- 一个 Agent Session 可以有多个 Execution，每个 Turn 的执行边界必须可审计。

### C2. Send 与 Steer

- `SendTurn` 必须绑定持久化 Turn ID 和当前 Execution。
- Provider 原生支持 Steer 时使用 `native`。
- 不支持原生 Steer 时，只能在尚未提交不可逆副作用且 Contract 明确允许时 `emulated`；否则
  返回 Unsupported。
- Steer 不得被静默转换成新的普通 Turn。
- 重试同一 Command 不得重复创建 Provider Turn 或重复执行工具副作用。

### C3. Interrupt 与 Cancel

区分：

- `InterruptTurn`：停止当前 Provider 生成，Agent Session 仍可继续。
- `CancelExecution`：Control Plane 取消整个 Execution。
- `StopSession`：释放当前 Provider Runtime，不删除 Agent Session 历史。
- Worker Shutdown：先 Drain，再按期限 Interrupt/Release，不伪装为成功完成。

Provider 不支持精确 Interrupt 时，允许终止子进程，但必须上报实际语义和可恢复性。

### C4. Compact

- 原生 Compact 标记为 `native`。
- 基于权威历史生成摘要标记为 `emulated`，摘要本身必须作为 Event/Artifact 持久化。
- Compact 不得删除原始 Session Event 或 Artifact。
- 新 Worker 必须能识别 Compact Boundary 并构造后续上下文。

### C5. Rollback

- Rollback 是 Agent Session 的显式命令，不是删除数据库 Event。
- 回滚目标、操作者、Provider 行为和产生的新 Checkpoint 必须审计。
- 原生 Provider 回滚不能成为唯一记录；Control Plane 持久化逻辑回滚边界。
- 已发生外部副作用时必须提示“对话状态回滚不等于外部系统回滚”。

### C6. Fork

- Fork 创建新的 Agent Session，并记录来源 Session、来源 Turn/Sequence 和 Fork Strategy。
- 原生 Provider Fork 为优化；没有原生能力时可以从权威历史 `emulated`。
- Fork 后两个 Session 的 Event Sequence、Execution、Artifact 新增写入彼此独立。
- 是否共享历史 Artifact 通过只读引用或复制策略明确实现，禁止复用可变 Workspace。

### C7. Review 与 Plan Mode

- Review 是明确的 Execution/Turn Mode，不使用 Prompt 文案猜测。
- Plan Mode 状态由 Control Plane 持久化，并作为 Workload Snapshot 传给 Worker。
- Provider 不支持原生 Review/Plan Mode 时，只有经过验收的 Prompt/Tool Policy 模拟才可标记为
  `emulated`。
- UI 根据 Capability Descriptor 展示入口，不在发送后才发现不支持。

### C 验收

- 每个命令都有跨 Provider Contract Test。
- 重复 Command ID 不产生重复 Turn、工具副作用或 Session Fork。
- Interrupt 后可以在新 Execution 继续后续 Turn。
- Compact/Rollback/Fork 后跨 Worker 重建结果与原 Worker 一致。
- 不支持能力不会被静默转换为另一种命令。

## 9. 工作流 D：Approval 与 Structured User Input

### D1. 持久化交互状态

Approval/User Input 必须保存：

```text
interactionId
tenantId / organizationId / sessionId / turnId / executionId
generation
provider
interactionType
safePromptMetadata
state
expiresAt
resolvedBy
resolution
createdAt / resolvedAt
```

Provider 原始 Request 中的敏感内容需要过滤；大内容通过 Artifact Reference 保存。

### D2. 状态机

```text
pending -> approved / denied / answered / expired / cancelled / superseded
```

规则：

- 同一 Interaction 只允许一个终态。
- Resolve API 必须具备权限和幂等性。
- 旧 Generation 的 Worker 不能提交新的 Interaction，也不能消费 Resolution。
- 用户响应后，通过当前 Lease 安全投递给仍活跃的 Provider Host。
- Worker 已丢失时，Resolution 持久化；恢复策略决定重建、重新询问或明确失败。
- Provider 不支持恢复 Pending Interaction 时不能假装已经恢复。

### D3. Lease 与等待策略

冻结两种实现之一，并在 Capability 中声明：

1. **Keep-alive**：Execution 保持 Lease 和 Provider 进程等待用户响应。
2. **Suspend-and-resume**：持久化 Interaction，释放昂贵运行资源，响应后创建恢复 Execution。

首版可以优先 Keep-alive，但必须设置：

- 最大等待时间。
- Lease Renew 行为。
- Worker Drain 行为。
- Pod Eviction 后的恢复/失败语义。
- Tenant 并发配额是否继续占用。

### D4. 安全策略

- Approval 的 Allow Once、Allow Session、Deny 等决策必须明确作用域。
- 不把 Provider 的任意字符串直接映射为永久授权。
- Structured Input 通过 Schema 校验，拒绝额外字段和超大答案。
- Pending Interaction 对无权限成员不可见。
- Resolution 不进入 Provider Credential 或 Worker Token 日志。

### D 验收

- Approval/User Input 页面刷新后仍可见并可处理。
- 两个浏览器并发 Resolve 只有一个成功终态。
- Worker 丢失、Lease 过期、Control Plane 重启后状态不丢失。
- Drain 中的 Worker 不再领取新 Execution，并能处理或安全交接 Pending Interaction。
- 跨 Tenant 无法读取或解决 Interaction。

### D 当前证据（2026-07-13）

- 已完成 Session 级 pending-only Snapshot、`snapshotSequence` 竞态对账、页面刷新恢复和 SaaS/Web
  Resolve/Interrupt 权威路由；本地模式继续使用 Native API。
- 已完成 Approval/User Input Schema 校验、过期拒绝、无审批权限 Event 脱敏，以及 SSE 连接内角色
  降级后的实时重新授权。
- 已完成 Interaction 24 小时等待上限：Lease Renew 精确检查，Claim/Recovery 与 Retention 后台 Sweep
  复用 `idx_execution_interactions_expiry`，超时后 Fence 旧 Generation 并转入 `recovering`。
- 已用真实 PostgreSQL 17 验证双 Control Plane 实例并发 Resolve：不同决策仅一个终态；相同决策、
  不同幂等键不重复 Event、Audit 或 Resolution delivery。
- D 尚未整体完成：仍需把 Pending Interaction 纳入统一 Provider Acceptance Suite，并完成 Drain、Pod
  Eviction、Provider 不支持恢复时的跨 Target 故障矩阵。

## 10. 工作流 E：Runtime Event 版本与兼容

### E1. 事件分类

Runtime Event 至少分为：

```text
session.lifecycle
turn.lifecycle
output.delta / output.completed
reasoning.summary
tool.started / tool.updated / tool.completed
approval.requested / approval.resolved
user-input.requested / user-input.resolved
plan.updated
review.finding
diff.updated
terminal.reference
artifact.reference
checkpoint.created
usage.updated
provider.warning
provider.error
```

具体 Event Type 应复用现有 Provider Runtime Contract，不能无审计地创造第二套同义事件。

### E2. Event Version 策略

- Envelope Version 和 Payload/Event Type Version 分离评估。
- 增加可选字段不提升 Major。
- 删除字段、修改含义或类型必须提升版本。
- Control Plane 保存规范化 Event 和有限、安全的 Provider Reference。
- Raw Provider Event 默认不进入数据库；诊断需要时使用受限、短期 Artifact。
- 未知 Event Type 可以持久化为 Unknown/Extension Event，但不得破坏 Session Replay。

### E3. Sequence 与幂等

- Event ID 由 Worker/Host 生成并支持重复上报幂等。
- Session Sequence 由 Control Plane 分配或验证，不信任 Provider 原生序号作为业务 Sequence。
- 同 Event ID 不同 Payload 拒绝。
- 同 Sequence 不同 Event ID 拒绝。
- Delta 重试不能在 UI 重复拼接。

### E4. Event 到 UI Projection

建立单向投影：

```text
Provider Native Event
    -> Provider Host Normalizer
    -> Runtime Event Contract
    -> Control Plane Session Event
    -> Web SaaS Projection Adapter
    -> Existing Thread UI Model
```

UI 不解析 Provider 原生 Event，不根据 Provider Name 分叉核心 Session 状态机。

### E 验收

- 八个 Provider 的核心 Event Mapping 有 Golden/Fixture Test。
- 旧 UI 可以忽略新可选 Event，新 UI 可以展示旧 Event。
- Event 重试、SSE 重连和页面刷新不重复渲染 Delta。
- 未知 Provider Event 不导致 Execution 卡死或 Worker Crash Loop。
- 大日志、Diff 和文件内容只通过 Artifact Reference 传递。

## 11. 工作流 F：Authoritative History 与跨 Worker Resume

### F1. 权威历史来源

恢复顺序固定为：

1. Control Plane 读取 Agent Session、Turn、Event、Interaction 和 Artifact Metadata。
2. 根据 Context Policy 生成有界、确定性的 Resume Snapshot。
3. 如果 Provider Resume Cursor 仍有效且安全，则作为优化尝试原生 Resume。
4. 原生 Resume 失败时，按 Capability 退回权威历史重建或返回稳定错误。
5. 任何情况下不依赖旧 Worker 本地唯一目录才能继续 Session。

### F2. Resume Snapshot

Snapshot 至少包含：

- Session/Turn 标识和 Provider/Model。
- 有序 User/Assistant 消息。
- 已完成 Tool 结果的安全摘要或 Artifact Reference。
- Plan/Review Mode。
- Compact Boundary/Summary。
- Pending Interaction。
- Workspace/Repository Revision 和 Checkpoint Reference。
- Context 截断原因和原始 Sequence Range。

Snapshot 必须有大小上限、Token 预算和确定性排序。

### F3. Provider Cursor 规则

- Cursor 以 Credential KMS 或等价机制加密。
- Cursor Envelope v2 必须通过认证头与 Execution Claim 冻结的 Tenant、Session、Provider、
  Model、Credential ID/Version、Capability Descriptor、Provider Host/Adapter/Runtime 与 Release Policy
  摘要绑定。
- Session Cursor 状态只能是 `absent`、`usable` 或 `quarantined`；`quarantined` 保留密文但
  强制使用 Authoritative History，不得因密钥恢复或 Runtime 回切而复活旧 Cursor。
- `usable` Cursor 必须持久化产生它的 Execution ID、Generation 和当时的 Authoritative
  History Sequence；只有当前 Execution 产生的新 Cursor 才能从 `quarantined` 恢复为
  `usable`。
- Credential Rotation、Provider 版本跨越或 Cursor 过期时，按策略失效。
- Cursor 不进入 Web API、日志、Event Payload 或 Artifact Metadata。
- Retry 当前未持久化 Turn 和恢复历史 Turn 必须区分，避免重复副作用。

### F4. 迁移边界

必须验证：

- 同 Target 新 Worker 恢复。
- Docker 容器替换后恢复。
- Kubernetes Pod 删除后恢复。
- SSH 服务重启后恢复。
- 兼容 Target 之间迁移后恢复。
- Provider 原生 Cursor 不可用时从权威历史继续。

### F 验收

- 删除 Worker Workspace 和 Provider 本地状态后仍可继续后续 Turn。
- Pod 替换前后 Session Event Sequence 连续。
- 原生 Cursor 失效不会破坏 Agent Session 历史。
- 恢复不会重复执行已确认完成的 Turn 或工具副作用。
- Snapshot 超限时使用 Compact/Artifact 策略并产生可审计记录。

### F 当前证据（2026-07-14）

- `Workload.resumeSnapshot` v1 已作为 additive Worker Contract 实现；旧 `conversationHistory` 从同一
  Snapshot Message 投影，不再存在独立聚合路径。
- Snapshot 已覆盖 legacy/v2 Assistant Text、Tool Summary、Plan/Review、Compact Boundary、Pending
  Interaction 白名单 Metadata、Ready Artifact 与 Workspace/Checkpoint 引用，以及确定性的
  Sequence/Byte/Token 截断记录。
- Claim 事务会单调推进 Runtime Binding 的 `authoritative_history_sequence`；SQLite 与 PostgreSQL 17
  均有测试，超过 500 个 Event 后的 Review/Compact Marker 也有恢复测试。
- Forward Migration `000030_execution_provider_cursor_snapshots.sql` 将 Credential Version、Resume Strategy
  和 Cursor Binding Digest 冻结到 Execution Generation；Cursor Envelope v2 使用该 Digest 作为
  AES-GCM 认证头的一部分。
- Forward Migration `000031_session_execution_cursor_lineage.sql` 增加 `absent / usable /
  quarantined` 状态、来源 Execution/Generation/History Sequence 约束和存量 Cursor 安全
  隔离；错误密钥、缺失 Cipher、未知/旧 Envelope 或非原生 Resume Runtime 不会阻断
  Execution Lifecycle，也不会让旧 Cursor 复活。明确的 Binding/Credential 不匹配会丢弃不
  兼容密文。
- 同一 Session 仅允许一个活跃 Execution，范围包含 `queued`、`leased`、`running`、
  `waiting-for-approval` 和 `recovering`；Service 锁内检查与 PostgreSQL/SQLite 部分唯一
  索引共同防止并发占用 Session。
- `queued` 或 `recovering` Execution 收到 Interrupt 时不等待 Worker：Control Command 立即
  `acknowledged`，Execution/Turn 同步取消并释放 Session 单活槽位。
- 当前仍未关闭 F：Cursor Payload 已记录 `IssuedAt`，但还没有完整的过期策略；
  Local/SSH/Docker/Kubernetes 删除本地状态后的同源 Live Acceptance 仍需由工作流 L
  证明。

## 12. 工作流 G：Credential 与 Secret 隔离

### G1. Provider Credential 作用域

评审并冻结解析优先级：

```text
explicit session binding
user credential
organization credential
tenant credential
platform credential
```

建议规则：

- 显式 Session Binding 优先。
- User Credential 只有本人 Session 可使用。
- Organization Credential 只允许组织内授权 Session。
- Tenant Credential 可以被策略限制到 Organization/Provider/Model。
- Platform Credential 必须由 Entitlement 和安全策略显式允许，不能成为无提示默认值。
- 同一级出现多个候选时拒绝模糊选择，要求显式绑定。

最终策略如改变现有 v1 Contract，需要新增 ADR。

### G2. Credential Delivery

- Control Plane 只向持有当前 Lease/Generation 的 Worker 返回明文。
- agentd 只在启动 Provider 进程前解析 Credential。
- 继续使用匿名 FD/内存 Pipe，禁止命令行参数和普通持久文件。
- Provider Host 对字段使用 Provider-specific Allowlist。
- Provider 子进程只收到最小环境变量集合。
- 执行结束、失败或取消后立即关闭 Pipe、释放 Buffer 和清理临时环境。

### G3. Git/Registry/Package Credential

Provider Credential 与 Workspace Credential 分离：

- Git Clone/Fetch/Push 使用短期 Git Credential 或受控 Agent。
- 私有 Registry/Package Token 只注入需要的命令阶段。
- SSH Private Key 使用临时 Agent/FD，固定 Host Key，不写入 Workspace。
- 不允许 Provider 任意读取 Control Plane 的云身份或宿主机 Credential Store。

### G4. 泄漏防护

测试至少覆盖：

- Provider stdout/stderr 回显 Secret。
- CLI Crash Dump。
- Process Environment 检查。
- `/proc`/进程参数可见性。
- Artifact/Terminal Log 上传。
- Event/Outbox/Audit Metadata。
- Error Message 和 Metrics Label。

### G 验收

- Worker/Lease Token 从不进入 Provider Runner 输入和环境。
- Provider/Git Credential 不出现在数据库明文字段、Event、Artifact Metadata 和日志。
- Credential Rotation 后旧版本不能被新 Execution 解析。
- 旧 Lease/Generation 无法获取 Credential。
- 每个正式 Provider 的 Credential Allowlist 有测试。

### G 当前证据（2026-07-14）

- agentd、Provider Host 和 Codex/Claude 子进程改为从空环境构造显式运行时白名单；ambient Worker、
  Lease、Control Plane、Provider、Cloud、GitHub、Database、Object Store、Proxy、SSH Agent 和
  `NODE_OPTIONS` 均不继承。
- Provider Credential 继续只经 FD 3 与 Provider-specific Payload Allowlist 注入；认证代理只能经
  显式 `SYNARA_PROVIDER_*_PROXY` 输入，映射后会参与诊断脱敏。
- Provider Host Build Version 使用构建包版本，不再接受宿主环境伪造；构建后的真实 Host/agentd
  Describe 集成已通过。
- 当前仍未关闭 G：Credential Scope 尚未实现 User/Platform 层及完整优先级 ADR；Registry/Package/
  SSH Workspace Credential 与全链路 Artifact/Outbox/Audit/Metrics Secret Canary 仍缺失。

## 13. 工作流 H：Remote Workspace 与 Git 生命周期

### H1. Workspace 状态机

建议冻结：

```text
requested
preparing
ready
in-use
checkpointing
retained
cleaning
deleted
failed
```

Workspace ID 与本地路径分离。Control Plane 持久化 Workspace Metadata，具体路径只在当前
Execution Target 内有效。

### H2. Prepare

准备步骤：

1. 校验 Repository URL、协议和 Tenant Policy。
2. 解析 Git Credential，不写入 Clone URL。
3. Clone 或复用经过校验的只读缓存。
4. Fetch 目标 Ref。
5. 创建 Execution/Session 隔离的 Branch/Worktree。
6. 校验实际路径、所有权、Symlink 和磁盘配额。
7. 记录 Base Commit、Branch、Remote 和 Workspace Manifest。

### H3. Git 操作边界

统一支持并审计：

```text
Clone
Fetch
Checkout/Create Branch
Create/Remove Worktree
Status/Diff
Commit
Push
Create Pull Request Reference
```

- Provider 可以提出 Git 操作，但 Credential 获取和网络策略由 Worker Runtime 控制。
- Push/PR 是否需要 Approval 由 Tenant/Project Policy 决定。
- 默认不允许 Force Push、改写受保护分支或向未授权 Remote 推送。
- Repository URL 必须防 SSRF、Local File、Link-local、Metadata Service 和协议走私。

### H4. Workspace 恢复

恢复优先级：

1. Git Base Commit + 已持久化 Patch/Checkpoint。
2. Workspace Snapshot Artifact。
3. 已提交远程 Branch。
4. 无法恢复时明确标记 Data Loss Risk，不能假装空 Workspace 是原状态。

Checkpoint 应包含：

- Base Commit。
- Current Branch/HEAD。
- Tracked/Untracked File Manifest。
- Patch 或 Snapshot Artifact Reference。
- 文件大小、Hash 和忽略规则。
- 创建时的 Session Sequence/Turn ID。

### H5. Cleanup 与 Retention

- Execution 结束不等于立即删除 Workspace。
- Retention 按 Deployment Profile、Tenant Policy、Session 状态和 Artifact Ready 状态决定。
- 清理操作幂等且不能越过 Workspace Root。
- Active Lease、Pending Artifact Upload、Pending Checkpoint 时禁止清理。
- 磁盘压力时按可解释优先级回收，并记录 Metrics/Audit。

### H 验收

- 私有仓库可在 SSH/Docker/Kubernetes Target 安全 Clone/Fetch。
- Repository URL SSRF、Path Traversal 和 Symlink Escape 被拒绝。
- Worker 删除后可以从 Git + Checkpoint/Artifact 恢复 Workspace。
- Push/PR 使用受控 Credential，不暴露给 Provider 日志。
- 清理不会删除其他 Tenant、Session 或宿主机目录。

## 14. 工作流 I：Terminal、Log、Generated File 与 Checkpoint

### I1. 数据分类

| 数据 | Session Event | Artifact Payload |
| --- | --- | --- |
| 短文本输出 Delta | 是 | 否 |
| Tool/Terminal 生命周期 | 是 | 否 |
| 长 Terminal Log | 只保存引用和摘要 | 是 |
| Generated File | 只保存引用和 Metadata | 是 |
| 大 Diff/Patch | 只保存引用和摘要 | 是 |
| Workspace Snapshot | 只保存 Checkpoint Event | 是 |
| Provider Raw Diagnostic | 受限引用 | 短期加密 Artifact |

### I2. Terminal 生命周期

至少投影：

```text
terminal.started
terminal.output.reference
terminal.exited
terminal.failed
```

- 小 Delta 可以实时展示，但必须限速、限大小并允许合并。
- 长日志滚动写入 Artifact，Event 只携带 Artifact ID、Offset/Range 和安全摘要。
- Terminal 环境、命令和 CWD 根据安全策略脱敏。
- Process Exit、Signal、Timeout 和 OOM 分类明确。

### I3. Generated File

- Worker 只允许上传 Workspace Root 内 Regular File。
- Symlink、Device、Socket、FIFO 和 Path Traversal 拒绝。
- 上传前计算 Size/Hash/Content-Type。
- Control Plane 独立确认 Artifact Ready。
- UI 只在 Ready 后提供预览/下载。

### I4. Checkpoint

- Checkpoint 创建具有 Idempotency Key。
- Checkpoint Event 绑定 Session Sequence、Turn、Execution、Generation 和 Artifact。
- Checkpoint Ready 后才能作为恢复点。
- 失败 Checkpoint 不得覆盖最后一个可用恢复点。
- Retention 删除 Checkpoint 前检查 Session/Fork/Recovery 引用。

### I 验收

- 超大日志不会撑爆 Event 表、SSE 或浏览器内存。
- Provider 输出二进制/无效 UTF-8 时安全降级为 Artifact。
- Artifact 未 Ready 时不能被作为恢复依据。
- Terminal/Artifact 重试不产生重复 Ready Artifact。
- Checkpoint 失败后仍保留上一个可恢复版本。

## 15. 工作流 J：Worker Drain、升级与版本隔离

### J1. Worker 生命周期

冻结：

```text
registering
ready
busy
draining
offline
incompatible
revoked
```

- `draining` Worker 不领取新 Execution。
- 已运行 Execution 在 Drain Deadline 内继续或完成安全 Checkpoint。
- Deadline 到期后按命令语义 Interrupt/Release，不能直接报告成功。
- Worker Token 撤销和 Worker Incompatible 是不同状态。

### J2. Graceful Shutdown

Shutdown 顺序：

1. 标记 Draining。
2. 停止 Claim。
3. 继续 Heartbeat/Renew 当前 Lease。
4. 等待当前 Command 到安全边界。
5. Flush Event/Artifact/Checkpoint。
6. 上报 Complete/Fail/Release。
7. 停止 Provider Host 和 agentd。

Kubernetes `preStop` 和 `terminationGracePeriodSeconds` 必须覆盖上述流程。

### J3. Worker Manifest

每个 Worker 注册并暴露：

- Worker Build Git SHA。
- Worker Protocol Version Range。
- Provider Host Version。
- Runtime Event Version Range。
- OS/Arch。
- Worker Image Digest。
- Provider CLI/SDK 版本。
- Capability Descriptor Hash。
- Feature Flags。

Manifest 不包含 Credential、路径中的用户信息或高基数 Secret。

### J4. 可重复构建

- Provider CLI/SDK 使用锁定版本和校验 Hash。
- Worker Image 使用固定 Base Image Digest。
- 构建输出生成 Version Manifest 和基础 SBOM。
- 不使用运行时无约束 `latest` 安装正式 Provider。
- 同一源码/锁文件可以重建等价 Worker Image。

### J5. 升级、灰度和回滚

- Control Plane 根据兼容矩阵决定 Worker 是否可调度。
- 新 Worker 先进入 Canary Pool，通过 Smoke Test 后扩大。
- 旧 Worker Drain，不原地替换 Busy Worker 二进制。
- Provider CLI 升级按 Provider 分开灰度。
- 新版本 Event/Capability 失败时可以回滚旧 Image Digest。
- Incompatible Worker 隔离并产生运维告警，不进入 Claim 热循环。

### J 验收

- Worker Drain 后不再 Claim 新任务。
- Busy Worker 在升级中完成、Checkpoint 或明确 Release，不产生双执行。
- 旧 Generation 在新 Worker 接管后永久失效。
- Image/CLI 版本可以从 Execution 和运维视图追溯。
- 不兼容 Worker 不影响兼容 Worker 继续服务。

## 16. 工作流 K：Web 主流程权威切换

### K1. 依赖门禁

开始主流程切换前，Stage 2 至少满足：

- Control Plane Project/Session/Turn/Execution API 稳定。
- Session Event/SSE 可跨副本恢复。
- Worker Claim、Lease、Generation 和 Artifact 闭环通过。
- 应用级 Authentication/Tenant/Organization Context 可用。

如果这些条件未满足，本工作流只完成 Adapter 和 Contract，不提前制造双权威写入。

### K2. 单一权威来源

启用 Control Plane 时：

```text
Control Plane Agent Session/Event
    -> SaaS Projection Adapter
    -> Existing Thread UI Model
```

- Project 创建先写 Control Plane。
- Session 创建先写 Control Plane。
- Turn 创建先写 Control Plane 并生成/排队 Execution。
- Runtime Event 从 SSE 投影到现有 Transcript。
- 页面刷新从 Control Plane Snapshot/Event 恢复。
- 本地 TypeScript Orchestration 不再并行创建同一 SaaS Session 的权威记录。

### K3. 本地模式兼容

未配置 Control Plane 时：

- 保持现有本地 Provider Runtime 和本地 Session UX。
- 使用同一 `ProviderKind`、Capability、Runtime Event 和 UI Projection 概念。
- 不建立另一套 Personal/Enterprise 领域模型。
- UI 根据 Control Plane Availability 选择 Backend Adapter，而不是在组件中散落条件分支。

### K4. 失败与重连

- Control Plane 不可用时不创建仅存在浏览器内存的远程 Session/Turn。
- SSE 断开显示 Reconnecting，并从最后 Sequence 恢复。
- Worker Offline 显示 Recovering，不把 Agent Session 直接标记 Failed。
- Unsupported Capability 在发送前可见，发送后仍有稳定错误兜底。
- Pending Approval/Input 在刷新后由 Control Plane Snapshot 恢复。

### K5. 切换策略

建议按 Feature Flag 分批：

1. 内部 Tenant + Codex + Local/Docker。
2. 内部 Tenant + Codex/Claude + Kubernetes。
3. Tier 2 Provider。
4. 默认启用 Control Plane 主流程。
5. 删除只为双写/双权威保留的临时代码。

Feature Flag 只能控制路由，不能允许同一 Session 同时双写两个权威 Store。

### K 验收

- 主界面可以创建 Control Plane Project/Session/Turn 并展示远程输出。
- 页面刷新、浏览器重连、Server 重启后 Transcript 一致。
- 一个 Session 只存在一个权威 Session ID/Event Sequence。
- Pending Approval、Artifact 和 Worker Recovering 状态可正确恢复。
- 未配置 Control Plane 的本地模式没有回归。

## 17. 工作流 L：统一 Acceptance Suite

### L1. 测试维度

测试按三维组合：

```text
Provider × Capability × Execution Target
```

不是所有组合都必须为 Supported，但每个组合必须得到：

- Pass。
- Explicit Unsupported。
- Skipped with documented infrastructure reason。

禁止无说明跳过。

### L2. Provider 核心套件

每个正式 Provider 至少测试：

1. Discovery/Version。
2. Credential Injection。
3. Start Session。
4. Send Turn 和 Text Output。
5. Tool/Activity Event。
6. Usage 或 Explicit Unsupported。
7. Interrupt。
8. 第二个 Turn。
9. Worker 替换后的后续 Turn。
10. Artifact/Generated File。
11. Approval/User Input 或 Explicit Unsupported。
12. Provider Error/Rate Limit/Auth Failure 分类。

### L3. Target 核心套件

| Target | 必须验证 |
| --- | --- |
| Local | Supervisor Restart、无 Control Plane 时本地兼容 |
| SSH | Host Key、systemd Restart、断网重连、远程磁盘清理 |
| Docker | Container Replace、Volume/Checkpoint、Resource Limit |
| Kubernetes | Pod Delete、Drain、Eviction、Image Rollout、Network Interruption |

### L4. 故障注入

至少覆盖：

- Provider 子进程启动失败。
- Provider 中途 Crash。
- Provider stdout Malformed/Oversized。
- agentd Crash。
- Worker 网络中断。
- Lease Renew 超时。
- Control Plane Pod 滚动升级。
- PostgreSQL 短暂不可用。
- S3/MinIO 上传或确认失败。
- Worker Drain Deadline 到期。
- Provider CLI 升级后 Resume Cursor 不兼容。
- Pending Approval 期间 Pod 被删除。

### L5. 长时间稳定性

- 多 Turn 长 Session。
- 长日志和大量 Tool Event。
- 多次 Compact/Checkpoint/Resume。
- Worker 重复重连。
- 多 Provider 并发执行。
- Artifact Retention/Cleanup 与运行并发。

### L 验收

- 同一 Provider Core Test 可以使用 Target Fixture 运行在四类 Target。
- 所有 Tier 1 Provider × Target 核心组合通过。
- Tier 2 的 Unsupported 项与 Capability Descriptor 一致。
- 故障测试没有 Event 丢失、重复终态、双 Worker 写入或 Credential 泄漏。
- Acceptance 结果生成机器可读报告和 Markdown 摘要。

### L 当前证据（2026-07-14）

- `scripts/stage3-provider-acceptance/acceptance_runner.py` 已形成同一套用例编排、红线脱敏与
  JSON/Markdown 报告；Local、Docker 和 Kubernetes 通过用户 API、真实 Control Plane/agentd
  与产品 Target 生命周期执行，不代替 Worker 注册、Heartbeat 或 Claim。
- Runner 已显式建模 `standing` 与 `execution-pinned` Worker Allocation，并以 Capability 声明
  Managed Replacement；execution-pinned 路径使用 Approval 作为 Worker/Manifest 可观测屏障。
- Kubernetes Driver 已在 disposable kind Context 上通过真实 Kubernetes API 创建隔离 Namespace、
  ServiceAccount/RBAC、短期 Token/CA 和 execution-pinned Pod/Manifest；deterministic Codex fixture
  的核心连续性用例通过，并验证终态 Pod/Manifest 清理。该证据尚不覆盖 Eviction、网络故障、
  Image Rollout 或真实 Provider Release Gate。
- 默认运行的是 deterministic Provider Host Protocol 2.1 fixture。它能证明共享 Contract、
  Control Plane-to-Worker-to-Host 通路和 Local/Docker/Kubernetes 恢复编排，**不等于**使用真实
  Codex App Server 或 Claude Agent SDK 的 Release Acceptance。
- SSH 当前仍返回 `runner.target_driver_missing`；Kubernetes Claude/failure matrix、真实
  Codex/Claude、SSH、长 Session 和完整故障矩阵仍待执行，不得声称四 Target 统一发布门禁已完成。

## 18. 实施顺序

### Step 0：差距审计与冻结边界

- 核对当前 Stage 2 完成状态和工作区变更。
- 生成八个 Provider 的本地 Adapter 与 Provider Host 差距表。
- 冻结 Tier、Capability ID、Error Code 和版本策略。
- 明确哪些现有代码复用、迁移或保留为 Local-only。

### Step 1：Provider Host Protocol v2

- 增加 Describe/Handshake。
- 增加版本协商和稳定错误模型。
- 把当前单请求 JSONL 扩展为命令/交互/终态协议。
- 保持 v1 兼容层或明确一次性受控升级边界。

### Step 2：Codex/Claude Tier 1 收口

- 补齐多 Turn、Interrupt、Approval/Input、Plan/Review 和 Resume 能力。
- 完成 Credential、Event、Artifact 和 Worker Migration 测试。
- 形成首个完整 Provider Acceptance Fixture。

### Step 3：其余 Provider 分批接入

- 优先复用现有 TypeScript Adapter/ACP 实现。
- 每个 Provider 单独决定 Native/Emulated/Unsupported。
- 每完成一个 Provider 即运行同一 Acceptance Suite。
- 不等待八个 Provider 全部完成才验证架构。

### Step 4：Remote Workspace/Git 与 Checkpoint

- 冻结 Workspace Contract 和状态机。
- 实现安全 Clone/Fetch/Worktree/Cleanup。
- 建立 Patch/Snapshot Checkpoint 和恢复。
- 接入 Artifact 与 Retention。

### Step 5：Worker 生命周期生产化

- 完成 Drain/Graceful Shutdown。
- 增加 Worker/Host/CLI Manifest。
- 完成可重复 Worker Image、灰度、隔离和回滚。

### Step 6：Web 主流程切换

- 确认 Stage 2 依赖门禁。
- 建立 SaaS Projection Adapter。
- 逐步切换 Project/Session/Turn/Event 主路径。
- 保留未配置 Control Plane 的本地 Backend Adapter。

### Step 7：四 Target 验收与长稳测试

- Local/SSH/Docker/Kubernetes 运行同一 Provider Suite。
- 执行 Crash/Network/Upgrade/Approval 故障测试。
- 生成兼容矩阵和 Acceptance Report。

### Step 8：文档、Runbook 与发布门禁

- Provider 接入指南。
- Worker 升级/回滚 Runbook。
- Workspace 恢复/清理 Runbook。
- Unsupported/Degraded 用户文案清单。
- Stage 3 Release Checklist。

## 19. 预计修改区域

实际执行前以差距审计为准，不要求为了匹配本列表创建无必要文件。

### Provider Host

```text
apps/provider-host/src/
apps/provider-host/package.json
docs/contracts/provider-host-v2.md                    # 建议新增
```

### Shared Contracts

```text
packages/contracts/src/provider*.ts
packages/contracts/src/providerRuntime.ts
packages/contracts/src/orchestration.ts
docs/contracts/runtime-event-v*.schema.json
docs/contracts/provider-capability-v1.md              # 建议新增
```

`packages/contracts` 保持 Schema-only，不加入运行时探测或进程管理逻辑。

### Worker/agentd

```text
services/control-plane/internal/agentd/
services/control-plane/internal/executions/
services/control-plane/internal/workload.go
services/control-plane/internal/httpapi/
```

### Provider Adapter

```text
apps/server/src/provider/
apps/server/src/provider/acp/
apps/server/src/orchestration/
```

优先抽取可共享的 Provider 逻辑，不在 `provider-host` 内复制八份本地 Adapter 行为。

### Workspace/Git/Artifact

```text
services/control-plane/internal/agentd/
services/control-plane/internal/artifacts/
apps/server/src/git/
docs/contracts/artifact-v1.md
docs/contracts/workspace-v1.md                         # 建议新增
```

### Web

```text
apps/web/src/lib/controlPlaneClient.ts
apps/web/src/store.ts 或独立 SaaS Projection Store
apps/web/src/routes/_chat.*
apps/web/src/components/ChatView.tsx
apps/web/src/components/settings/
```

### Deployment/Test/Docs

```text
Dockerfile
deploy/saas/
deploy/kubernetes/
deploy/remote/
docs/worker-image.md
docs/runbooks/                                         # 建议新增
docs/reports/
```

## 20. 测试与验证计划

### 20.1 Provider Host Focused Test

```bash
cd apps/provider-host
bun run test
```

重点：

- Handshake/Version/Capability。
- Command Idempotency。
- Credential FD 和 Allowlist。
- stdout/stderr Redaction。
- Provider Event Fixture Mapping。
- Malformed/Oversized JSONL。
- Interrupt/Shutdown。
- Resume/History Reconstruction。

### 20.2 Server Provider Test

根据实际修改文件运行：

```bash
cd apps/server
bun run test src/provider
```

必要时使用具体 Test File，避免迭代期间反复执行全仓测试。

### 20.3 Go Test

```bash
cd services/control-plane
go test ./...
```

重点：

- agentd Runner Protocol。
- Worker Manifest/Compatibility。
- Drain/Lease/Generation。
- Interaction Persistence。
- Workspace Path/Git URL Security。
- Artifact/Checkpoint。

### 20.4 Contract/Fixture Test

- Capability Descriptor Schema。
- Provider Host v1/v2 Compatibility。
- Runtime Event Golden Fixture。
- Unknown Field/Event Compatibility。
- Stable Error Code Mapping。

### 20.5 Target Acceptance

运行顺序：

1. Local Isolated Instance。
2. SSH Test Host。
3. Docker Worker。
4. kind/Kubernetes Worker。

启动本地 Synara 时必须遵守仓库的隔离端口和独立 Home Directory 规则，不与用户正在运行的
实例共享端口或状态。

### 20.6 安全测试

- Credential/Token Log Scan。
- Repository URL SSRF。
- Workspace Traversal/Symlink Escape。
- Artifact Content/Hash 验证。
- Old Lease/Generation 写入和 Secret Resolve。
- Provider 输出注入控制字符/超大 Payload。

### 20.7 仓库级检查

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

## 21. 完成标准

- [ ] 八个 Provider 均有审计完成的 Capability Matrix 和支持等级。
- [ ] Tier 1/Tier 2/Experimental/Local-only 的发布边界明确。
- [ ] Provider Host Protocol v2、最低兼容版本和 Capability Negotiation 冻结。
- [ ] Unsupported Capability 返回稳定错误，不静默降级。
- [ ] Start/Resume/Send/Steer/Interrupt/Compact/Rollback/Fork/Review 语义冻结并测试。
- [ ] Approval、Structured User Input 和 Plan Mode 可以跨页面刷新、Control Plane 重启恢复。
- [ ] Runtime Event 版本和未知事件策略完成。
- [ ] Provider Resume Cursor 失效时可以按策略从权威历史继续。
- [ ] Worker/Pod 替换后可继续后续 Turn，Event Sequence 连续。
- [ ] Worker、Lease、Provider、Git Credential 没有非预期日志或 Artifact 泄漏。
- [ ] Remote Workspace Clone/Fetch/Worktree/Checkpoint/Cleanup 生命周期完成。
- [ ] Terminal、长日志、Generated File、Diff 和 Checkpoint 使用 Event/Artifact 引用。
- [ ] Worker Drain、Graceful Shutdown、Manifest、升级、回滚和版本隔离完成。
- [ ] Worker Image/Provider CLI 版本可追溯并可重复构建。
- [ ] Web 主聊天在 SaaS 模式只使用 Go Control Plane 作为 Session 权威来源。
- [ ] 未配置 Control Plane 时本地个人模式保持可用。
- [ ] Local、SSH、Docker、Kubernetes 运行同一 Provider Acceptance Suite。
- [ ] Crash、Network、Provider Failure、Pod Delete、Rolling Upgrade 测试通过。
- [ ] 形成 Stage 3 Compatibility Matrix、Acceptance Report 和 Operations Runbook。

## 22. STOP 条件

出现以下情况时暂停实现并重新评审：

- Control Plane 必须解析 Provider 原生 Event 才能推进主流程。
- 前端必须直接连接 Worker 或 Provider Host。
- Provider Credential、Worker Token 或 Lease Token 必须进入 Runner Input、命令行或日志。
- Session 恢复必须依赖旧 Worker 本地唯一 Provider State。
- 同一 Session 在 Web/TypeScript Orchestration 和 Go Control Plane 中同时双写权威状态。
- 为某个 Execution Target 建立绕过 Worker Protocol 的独立 Provider 通道。
- 不支持能力只能通过静默转换成另一种命令实现。
- Approval/User Input 只能保存在 Provider 进程内存中。
- Worker Drain/升级可能让两个有效 Generation 同时继续写入。
- Workspace 恢复只能依赖未持久化的本地文件。
- Git Credential 只能嵌入 Repository URL 或写入 Workspace。
- 大日志/文件必须直接写入 Session Event Payload。
- Provider CLI 升级会要求迁移 Agent Session 业务表结构。
- Personal 和 Enterprise 开始维护不同 Provider/Session 状态机。
- Stage 2 尚未形成单一 Session 权威，却准备强行切换 Web 主流程。

## 23. 交付物

完成本阶段应产生：

1. Provider Capability Matrix 和正式支持等级清单。
2. Provider Host Protocol v2 Contract。
3. Provider Capability/Compatibility Contract。
4. Codex/Claude Tier 1 远程运行基线。
5. Cursor/Gemini/Grok/Kilo/OpenCode/Pi 的远程支持结论和实现。
6. Runtime Event Mapping/Version Compatibility 清单。
7. Approval/User Input/Plan/Review 远程生命周期实现。
8. Authoritative History 与 Resume Strategy 文档。
9. Remote Workspace/Git/Checkpoint Contract。
10. Worker Drain/Upgrade/Rollback Contract 和 Runbook。
11. Worker Image Version Manifest 和可重复构建规则。
12. Web SaaS Session Projection Adapter 与主流程切换结果。
13. Provider × Capability × Execution Target 机器可读测试矩阵。
14. Local/SSH/Docker/Kubernetes Acceptance Report。
15. Crash/Network/Upgrade Continuity Report。
16. Stage 3 Release Checklist。

## 24. 阶段结束后的边界

Stage 3 完成后：

- Stage 4 可以把已经稳定的 Worker/Provider Runtime 当作调度单元，专注多集群、容量、Warm
  Pool、Placement、隔离和灾难恢复。
- Stage 5 可以基于稳定的 Provider/Worker Manifest 统计用量、成本、SLO、安全和企业运维能力。
- 新增 Provider 应主要实现 Provider Adapter/Host Contract 并运行统一 Acceptance Suite，不应再
  修改 Control Plane Session/Execution 核心状态机。
