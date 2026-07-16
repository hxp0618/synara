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
- **最近稳定检查点**：`253052aa`（同一 clean SHA 的真实 Codex/Claude consolidated Local product 与
  failure 四矩阵已通过；正式证据见
  `docs/reports/stage-3-real-provider-local-release-gate-253052aa.md`。Kubernetes deterministic fixture
  13/13 的历史 clean checkpoint 仍为 `2763ebd3`）
- **工作区状态**：Stage 3 持续执行中，执行时以当前分支和已验证证据为准
- **发布文档**：
  `docs/release-checklists/stage-3-provider-runtime-remote-worker.md`、
  `docs/runbooks/worker-release-rollout.md`、
  `docs/reports/stage-3-real-provider-local-release-gate-253052aa.md`、
  `docs/reports/stage-3-real-provider-local-standalone-generated-file-matrix-be919393.md`
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

| 状态           | 含义                                    | 用户行为                              |
| -------------- | --------------------------------------- | ------------------------------------- |
| `native`       | Provider 原生协议直接支持               | 使用原生能力并保留原生引用            |
| `emulated`     | Synara 可基于权威历史/Artifact 安全模拟 | UI 明确使用统一行为，不承诺原生细节   |
| `unsupported`  | 无法可靠实现                            | 请求前禁用或返回稳定 Unsupported 错误 |
| `experimental` | 尚未达到正式 SLA                        | 仅显式开启，不能作为默认能力          |

`experimental` 是发布等级，不替代前三种行为状态。例如某能力可以是“实验 Provider 的
原生 Fork”。

### A2. 定义正式支持等级

| 等级         | 要求                                                     |
| ------------ | -------------------------------------------------------- |
| Tier 1       | 核心能力、远程恢复、Credential、四类 Target 验收全部通过 |
| Tier 2       | 核心聊天与恢复通过，部分高级能力明确 Unsupported         |
| Experimental | 只在 Feature Flag 下启用，不承诺升级兼容                 |
| Local-only   | 保留现有本地 Adapter，但远程 Target 不允许选择           |

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

### A5. 当前实现证据（2026-07-14）

- `packages/contracts/src/providerCapabilityCatalog.json` 是机器可读能力清单的单一 TypeScript 源；
  Control Plane 的 Go Catalog 由它生成，并以 Source SHA256 和 generator `-check` 防止漂移。
- Control Plane 提供脱敏的 Project Target Projection 与 Session Execution Projection，只暴露
  `supported / unsupported / unobserved`、稳定 Reason Code 和 `native / emulated`，不暴露 Worker、
  Manifest、心跳、Build 或 Runtime Version 运维字段。
- 有 Active Execution 时只使用该 Execution 固定的 Worker Manifest；Target Projection 只聚合当前
  可 Claim 的兼容 Manifest，不能借用不兼容或未声明能力的 Worker。
- Create Session 和 Create Turn 在幂等事务内分别复检 Start/Send 与 Send/Plan；local-only、Droid、
  明确不兼容或不支持的能力在持久化前拒绝，`unobserved` 的可排队能力保留 scale-to-zero 语义。
- Web Provider Picker、Plan、Steer、Interrupt 和高级命令门禁消费同一 Projection；未配置 Control
  Plane 时保持既有本地行为。
- 本增量复用既有 Target、Manifest、Session 和 Execution 表，不新增或修改 DDL。

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

### C 当前证据（2026-07-15）

- Forward Migration `000032_session_advanced_operations.sql` 将 Compact、Review、Rollback、Fork 冻结为
  独立 Turn Kind，并要求每个 Provider 执行型高级 Turn 只有一个匹配的 Primary Control Command；
  PostgreSQL deferred trigger 同时保护 Fork 形状、逻辑祖先 Turn、循环、Primary Command UPDATE/DELETE
  与父 Execution cascade。SQLite 镜像关键唯一索引和安全触发器。
- Compact/Review 作为 queued Execution 执行，Provider Host 写命令前先持久化 `delivered`，等待真实
  Provider 终态后原子确认 Control Command、Execution、Turn 与 Lease；普通 `Complete` 不会覆盖该终态。
  Rollback/Fork 由 Control Plane 以零拷贝逻辑历史 `emulated`，不启动 Worker；Rollback 明确记录
  `workspaceDisposition=unchanged` 和 `externalSideEffectsReverted=false`。
- Codex Review/Compact 为 native；Claude Review 使用固定只读 Tool Policy 的 `emulated` 路径，Compact
  为 Explicit Unsupported。Codex Review 可以从无 Cursor 的新 Thread 启动，Compact 必须有 usable native
  Cursor；新 Session、Fork 或 Rollback 后会在 Capability Projection 与 API 返回稳定门禁。
- Service、HTTP、agentd、Provider Host、Contracts 与 Web 定向测试覆盖 private Session、CAS、Quota、
  Capability、幂等 replay、并发单赢家、HTTP Replay Header/非法 JSON/缺 Key、Primary terminal-before-ack、
  Fork Prefix Page/Tail、循环/深链终止、501 条 Resume 尾部、Rollback Chain 和 SaaS 路由不读取本地
  Native API。PostgreSQL 17 Migration Integration 与 SQLite Safety 测试均通过。
- C 仍需真实 Codex/Claude 在 Remote Worker 替换后的 Compact/Review/后续 Turn Release Acceptance；
  deterministic fixture 与静态 Capability 证据不能替代该发布门禁。

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

### D 当前证据（2026-07-14）

- 已完成 Session 级 pending-only Snapshot、`snapshotSequence` 竞态对账、页面刷新恢复和 SaaS/Web
  Resolve/Interrupt 权威路由；本地模式继续使用 Native API。
- 已完成 Approval/User Input Schema 校验、过期拒绝、无审批权限 Event 脱敏，以及 SSE 连接内角色
  降级后的实时重新授权。
- 已完成 Interaction 24 小时等待上限：Lease Renew 精确检查，Claim/Recovery 与 Retention 后台 Sweep
  复用 `idx_execution_interactions_expiry`，超时后 Fence 旧 Generation 并转入 `recovering`。
- 已用真实 PostgreSQL 17 验证双 Control Plane 实例并发 Resolve：不同决策仅一个终态；相同决策、
  不同幂等键不重复 Event、Audit 或 Resolution delivery。
- `2763ebd3` 的 disposable Kind deterministic Codex fixture 13/13 通过：Pending Approval 期间删除准确的
  execution-pinned Pod 后，同一 Execution 从 Generation 1 恢复到 2，旧 Interaction/Request/Pod UID 均被
  替换，只有新 Request 可以 Resolve，后续 Artifact、User Input、Provider Error、Control Plane Restart
  和第二 Turn 连续完成；见
  `docs/reports/stage-3-kubernetes-provider-fixture-acceptance-2763ebd3.md`。
- D 尚未整体完成：仍需完成 Drain、真实 Eviction、Provider 不支持恢复时的跨 Target 故障矩阵，以及
  真实 Codex/Claude Adapter 的 Interaction Release Acceptance。

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
3. Control Plane 在 Claim 时验证 Cursor Envelope、Binding、Lineage、年龄和时钟偏差；不满足策略时直接
   选择权威历史，不把 Cursor 交给 Worker。
4. 只有已通过 Control Plane 策略的 Cursor 才作为优化尝试原生 Resume。Provider Host 仅在 Provider
   明确返回 native Session invalid/expired 且尚无 Turn activity 时选择权威历史；认证、限流、传输和
   模糊错误保持终态失败，不静默重放 Turn。
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
- 默认最大年龄为 720 小时，由 `SYNARA_PROVIDER_CURSOR_MAX_AGE` 配置且不得超过 8760 小时；
  `now >= issuedAt + TTL` 在精确边界失效。允许最多五分钟未来时钟偏差，超过时隔离。
- Credential/Binding 明确不兼容时可以清空为 `absent`；错误密钥、缺失 Cipher、未知/旧 Envelope、
  非原生 Runtime、过期或未来时间戳必须保留密文并转为 `quarantined`。
- Cursor 明文、密文和 Binding Digest 不进入 Web API、日志、Event 或 Artifact Metadata；既有
  `execution.leased.providerResume` 只记录 bounded Resume 决策、时间和来源 Lineage。
- 同一 Generation 的 Claim receipt replay 复用已提交决策，不重新计算 TTL。首次选择的 native Cursor
  已被替换、隔离或无法精确打开时返回 `409 claim_replay_resume_cursor_unavailable`，不得静默切换到
  Authoritative History；新 Generation 才重新评估策略。

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
- Forward Migration `000031_session_execution_cursor_lineage.sql` 增加 `absent`、`usable`、`quarantined`
  状态、来源 Execution/Generation/History Sequence 约束和存量 Cursor 安全
  隔离；错误密钥、缺失 Cipher、未知/旧 Envelope 或非原生 Resume Runtime 不会阻断
  Execution Lifecycle，也不会让旧 Cursor 复活。明确的 Binding/Credential 不匹配会丢弃不
  兼容密文。
- 同一 Session 仅允许一个活跃 Execution，范围包含 `queued`、`leased`、`running`、
  `waiting-for-approval` 和 `recovering`；Service 锁内检查与 PostgreSQL/SQLite 部分唯一
  索引共同防止并发占用 Session。
- `queued` 或 `recovering` Execution 收到 Interrupt 时不等待 Worker：Control Command 立即
  `acknowledged`，Execution/Turn 同步取消并释放 Session 单活槽位。
- `2763ebd3` 的 disposable Kind deterministic fixture 证明 Pod 替换后 Session Event Sequence 从 1 到
  57 连续，Control Plane 重启后第二 Turn 可继续；该用例使用空 Repository Project 和 fixture Cursor，
  不替代真实 Provider Cursor 失效或删除本地 Workspace/Provider State 的四 Target 验收。
- `36ae47d6` 实现完整 Cursor 年龄策略和可审计选择：默认 720 小时、8760 小时上限、精确 TTL
  边界、五分钟未来时钟容差、隔离不复活、当前 Execution 新 Cursor 恢复，以及 receipt replay
  固化/冲突规则。Provider Host 只分类明确 invalid/expired，并发出 exact-shape canonical warning；
  agentd 为该语义槽派生稳定 Event ID。
- SQLite 与真实 PostgreSQL 17 覆盖 TTL 前 1ns、精确 TTL、最大/超限未来时钟偏差、密文保留、延长
  TTL 或恢复密钥不复活、fresh current-Generation Cursor 恢复，以及两个独立数据库连接池的 Claim
  重试/并发只提交一条 `execution.leased` 决策。
- F 仍未完全关闭：真实 Codex/Claude native Session invalid/expired、Local/SSH/Docker/Kubernetes 删除
  Provider 本地状态后的迁移，以及已完成 Turn/工具副作用不重复，仍需由工作流 L 的 Live Acceptance
  证明。deterministic fixture Cursor 不能替代真实 Provider Release Gate。

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

### G 当前证据（2026-07-16）

- agentd、Provider Host 和 Codex/Claude 子进程从显式 allowlist 构造运行时环境；ambient Worker、Lease、
  Control Plane、Cloud、GitHub、Database、Object Store、Proxy、SSH Agent 和 `NODE_OPTIONS` 不继承。
  Provider Credential 只经 FD 3 与 Provider-specific Payload Allowlist 注入；显式
  `SYNARA_PROVIDER_*_PROXY` 映射后的认证信息也进入 SecretGuard redaction set。
- Migration `000033` 实现 User/Organization/Tenant/Platform Provider Credential Scope、selector、
  per-Credential auto-select 和 Tenant Platform policy。解析优先级固定为 explicit Session、User、
  Organization、Tenant、Platform；同层多候选返回稳定 ambiguity error，Platform 需 enterprise
  entitlement 与两层显式 policy。
- Migration `000035` 增加 Project/Target Credential Binding 与 immutable per-Generation Grant；
  `000036` 完成 legacy Project Git backfill 后清空并禁止继续写 `projects.git_credential_id`；`000038`
  为 disabled Binding 历史补齐 FK lookup indexes；`000039` 保证每个 Target 最多一个 active
  `worker_image_pull` Binding。Agentd 只在当前 Git/Registry/Package stage 解析
  Grant，HTTPS 使用 exact-host AskPass，SSH 使用 pinned Host Key 和临时单 Key Agent。
- `worker_image_pull` Registry Credential 只由 Control Plane Target provisioner 解析，Binding selector
  必须精确匹配镜像 Registry Host，不进入 Worker Workload、agentd Provider 环境、Event 或 Audit。
- SecretGuard、agentd Credential/terminal tests、真实 PostgreSQL/SQLite migration tests，以及当前
  deterministic Local/Docker/Kubernetes report output scans 已通过。真实 Provider 四 Target 的
  stdout/stderr、Crash dump、Artifact/集中日志 Secret canary 与 Windows FD transport 仍未关闭，因此
  G 保持 `partial`。
- 共享 Acceptance Runner 的真实 SSH/Docker/Kubernetes 路径现要求显式指定 operator-owned 环境变量，
  并在构建 Worker Image 或启动任何子进程前验证值、注册 Secret/Base URL 脱敏。Runner 只把值提交给
  本次隔离 Control Plane 创建加密 Provider Credential，Session 仅绑定 Credential ID；Worker 仍通过
  匿名 FD 3 获取解析后的 allowlist payload。命令行、Target 配置、Image、报告和持久化 evidence 均不记录
  环境变量名或 Secret。缺失、空值、控制字符和不安全变量名会在 CLI preflight 阶段 fail closed；Claude
  的 `authToken` 与可选 Base URL 也走同一受控路径。该实现尚未生成真实 Docker Provider 报告，因此不
  改变 G 的 `partial` 状态。

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

| 数据                    | Session Event           | Artifact Payload  |
| ----------------------- | ----------------------- | ----------------- |
| 短文本输出 Delta        | 是                      | 否                |
| Tool/Terminal 生命周期  | 是                      | 否                |
| 长 Terminal Log         | 只保存引用和摘要        | 是                |
| Generated File          | 只保存引用和 Metadata   | 是                |
| 大 Diff/Patch           | 只保存引用和摘要        | 是                |
| Workspace Snapshot      | 只保存 Checkpoint Event | 是                |
| Provider Raw Diagnostic | 受限引用                | 短期加密 Artifact |

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

当前实现记录（2026-07-15）：deterministic fixture 使用不触发 JSON/HTML 转义的 63 KiB Chunk 产生
`2 MiB + 257 B` Terminal Stream；agentd 在真实 Local 与 Docker 产品路径中均只持久化前 32 KiB 安全
Preview，并生成 `1 MiB / 1 MiB / 257 B` 三个 Ready `terminal_log` Artifact。Acceptance 已校验连续
Offset、固定 SHA-256、Completion Total/Exit Code、无重复 `artifact.ready` 以及 Session Event 中不存在
Runtime Output 物理路径。2026-07-16 clean commit `f1b1aa53` 将真实 `terminal-large` 纳入 canonical matrix，但没有把
Provider 截断伪装为通过：Codex `0.144.x` 默认 `unified_exec` 只保留 1 MiB Head/Tail，且禁用它会破坏
durable Approval 与跨 Turn Cursor 语义，因此记录为 Explicit Unsupported。Claude ambient OAuth 的 SDK
保留文件位于 agentd Runtime Output Root 外，也记录为 Explicit Unsupported；其严格真实大日志路径仍要求
受控 Provider Credential 将 `CLAUDE_CONFIG_DIR` 绑定到 Runtime Output Root。Generated File、大 Diff、
真实 Codex/Claude lossless 大日志和跨 Target Release Acceptance 仍待完成。

2026-07-16 clean commit `f1b1aa53` 新增第 10 个 canonical case `generated-file-checkpoint`。真实 Codex 与 Claude Local
均写入精确 `1 MiB + 257 B` Workspace 文件，并在 Execution 完成前形成
`workspace.dirty -> checkpoint.created -> workspace_snapshot artifact.ready -> checkpoint.ready`。Runner 经
用户 Artifact 下载授权读取 Ready Snapshot，拒绝 Absolute/Traversal/Symlink/非 Regular Tar Member，并校验
目标文件的相对路径、Size/SHA-256、已知 Runner 哨兵内容、Artifact Metadata、生命周期顺序、无重复 Ready
和无物理路径泄漏。
该证据关闭真实 Local Workspace Generated File 的 Checkpoint 捕获路径；standalone Provider
`generated_file` ArtifactCandidate、大 Diff、跨 Target 与 Retention 并发仍保持开放。详见
`docs/reports/stage-3-real-provider-local-generated-file-matrix-f1b1aa53.md`。

2026-07-16 clean commit `be919393` 在同一个 canonical case 中增加独立的 standalone 文件边界。Codex 只从
成功完成的原生 `fileChange` item 收集精确路径，Claude 只从成功的 `PostToolUse`
`Write/Edit/MultiEdit/NotebookEdit` 收集精确路径；不解析 shell、不扫描 Workspace、不把 Checkpoint
Snapshot 冒充 standalone Artifact。两条真实 Local matrix 均在 `workspace.dirty` 前产生唯一 Ready
`generated_file`，经用户下载授权重新读取后验证精确 `43 B`、SHA-256、Metadata 和无物理路径泄漏；随后
同一 Execution 的大文件仍按既有 Checkpoint 顺序完成。Codex 为 `21 pass + 1 unsupported`，Claude 为
`20 pass + 2 unsupported`。该证据关闭已实现的 Local standalone Generated File ArtifactCandidate；大
Diff、跨 Target 与 Retention 并发仍保持开放。详见
`docs/reports/stage-3-real-provider-local-standalone-generated-file-matrix-be919393.md`。

2026-07-16 clean commit `90fae52c` 把 canonical matrix 扩展为第 11 个 `large-diff` case。Codex 为
`22 pass + 1 unsupported`，原生 `turn/diff/updated` 删除 5,000 行并生成 `320,258 B` Ready `diff`；
Claude 为 `21 pass + 2 unsupported`，通过 canonical Workspace realpath alias、原生
`Read(offset=1, limit=1)` 与 `Write` 完整替换并生成 `320,201 B` Ready `diff`。两份 clean-worktree
报告均经用户授权下载复核 Size/SHA-256、UTF-8、相对文件标记、
`artifact.ready -> turn.diff.updated -> execution.completed` 严格顺序、无 inline 大 Payload、无 Runtime
Output 物理路径、Control Plane restart/Cursor 连续性、精确 cleanup 和零 Secret finding。先前隔离运行与
agentd 回归还覆盖删除最后一个非 Git 文件后的空 Snapshot Checkpoint。该证据关闭实现层面的真实 Local
Large Diff 路径；跨 Target 与 Retention 并发仍保持开放。详见
`docs/reports/stage-3-real-provider-local-large-diff-matrix-90fae52c.md`。

2026-07-16 clean commit `61e38f4f` 新增独立真实 Provider failure matrix。Codex 与 Claude Agent 均以
Node.js `24.13.1` 通过 `16/16`：Runner-owned loopback 401/429 分别稳定映射
`authentication_required` / `provider_rate_limited`，每个故障后均由新 Execution 恢复；Host crash 只在
隔离 Control Plane 子树内等待真实 `item.started` 后 `SIGKILL` 唯一 `--protocol-v2` 进程；Cursor 通过
`SYNARA_PROVIDER_CURSOR_MAX_AGE=1s` 自然过期，restart 后租约选择
`authoritative-history / cursor_expired` 并精确恢复上一轮 marker。Codex controlled Credential 使用
execution-local `CODEX_HOME`，Claude 对稳定 401/429 SDK `api_retry` 结束隐藏重试；两份 cleanup 和 Secret
scan 均通过。该证据关闭实现层面的真实 Local failure slice，不关闭跨 Target、并发或 soak。详见
`docs/reports/stage-3-real-provider-local-failure-matrix-61e38f4f.md`。

2026-07-16 clean commit `253052aa` 将 product/capability 与 controlled-failure 保持为四份独立子报告，
并由 `local_release_gate.py` 汇总为同一真实 Local release unit。Provider Host 使用 Node.js `24.13.1`
从当前 checkout 重建；Codex product 为 `22 pass + 1 unsupported`、failure 为 `16/16`，Claude product 为
`21 pass + 2 unsupported`、failure 为 `16/16`。四份报告共享同一 clean Git SHA 与 Capability Catalog
hash，无 fail/skipped，只有冻结的 Compact/lossless-Terminal Explicit Unsupported，精确 cleanup 与 output
Secret scan 均通过。首次尝试发现 Codex Approval Turn 未调用工具却直接终止；Runner 现对“Turn 已终止但
缺所需 Interaction”立即返回 `runner.interaction_missing_after_terminal`，不自动重试、不降级断言。该证据
关闭实现层面的真实 Codex/Claude consolidated Local slice，不关闭 SSH、Docker、Kubernetes、Registry
rollout、并发或 soak。详见 `docs/reports/stage-3-real-provider-local-release-gate-253052aa.md`。

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

### J 当前证据（2026-07-16）

- Migration `000034` 将 compatibility 与不可逆 operator revocation 分离，新增 immutable logical
  identity tombstone，并在 Token、Heartbeat、Claim、Lease 和同身份 re-registration 边界 fail closed。
  Revoke API 使用 Idempotency Key 与 expected Incarnation，返回 released Lease、recovering、outcome
  unknown、checkpoint unconfirmed 和 requeued cleanup 计数；普通 rollout 不得以 Revoke 代替 Drain。
- Migration `000037` 增加 target-scoped immutable Release Revision、单行 CAS Policy、immutable
  Transition History，以及 Worker/未租用 Execution 的 Revision/Channel pin。Policy 支持 initial
  promote、newer canary、active-canary promote、abort-canary 和 older-revision rollback，stale Version
  必须返回 conflict；`000040` 进一步要求最新 Transition 与当前 Policy 完全一致。
- Control Plane API 和 SaaS Settings 已提供 Worker list/revoke、Release Overview/Create、Canary、Promote、
  Rollback；Registry `worker_image_pull` Binding 为 Docker pull request 和 Kubernetes target-scoped
  docker config 提供最小 auth。运维边界已记录在
  `docs/runbooks/worker-release-rollout.md`。
- 当前 deterministic Docker/Kubernetes tests 与 failure-only image-canary 只证明 release shape、Target
  isolation、replacement 和恢复编排。新增 `registry_release_gate.py` 从完全 clean SHA 执行 cached 与
  independent no-cache 两次 `linux/amd64,linux/arm64` Registry push，校验 Registry-returned OCI index、
  per-platform manifest digest、SPDX/SLSA attestation、non-root config，以及 Image 内 Manifest/SBOM/lockfile
  与当前源码一致性；精确清理本地 inspection container/image/state，不执行广域 prune，并保留远端 tag 作为
  发布证据。当前 Registry gate tests `18/18`、Stage 3 Python `171/171` 已通过；真实 clean-SHA Registry
  运行尚待记录。它们仍没有证明镜像签名策略、生产 Registry retention、真实
  Codex/Claude rollout、Busy Worker 长任务、生产多节点 Drain/PDB/Eviction 或 rollback under load；
  rollout/reconciler 最终评审和真实 Gate 完成前，J 保持 `partial`。

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

### K6. 当前实现证据（2026-07-14）

- 首次 SaaS 发送在创建 Session 前组合校验 Start/Send/Plan，避免能力不满足时留下孤立 Session；
  后续 Turn、Steer 和 Interrupt 使用 Session/Execution Projection，并由 Control Plane 再次校验。
- SaaS Provider Picker 和 Settings 使用受 Target Projection 约束的 Provider Catalog，不依赖本机 CLI
  可用性；local-only Provider 与未进入 Catalog 的 Droid 无法绕过搜索或直接 Mutation。
- 已有 SaaS Session 的模型切换由 Control Plane `POST /v1/sessions/{sessionID}/model-switch`
  持久化；请求携带必填且可为 `null` 的 `expectedModel`，Web 在 Mutation 前重新读取 Session 和
  `model-switch` Capability，成功后才更新 Query Cache、Session Event Projection 和 Picker，失败时
  保留服务端权威模型，不再制造只改变本地 Draft 的假切换。
- 模型切换在 Session 行锁内执行 strict CAS，拒绝五种 Active Execution，要求已观测且 Supported 的
  Capability，释放旧 Runtime Binding、清空 Provider Cursor/lineage，并创建下一 revision 的
  `authoritative-history` Binding；`session.model.changed` 同时进入 Session Event/SSE 和 Audit。该闭环复用
  现有 `agent_sessions`、`provider_runtime_bindings` 与 Cursor 字段，不新增 DDL，Migration 仍为 `000031`。
- Codex 与 Claude Agent 的 `model-switch` 在机器可读 Catalog 中标记为 `emulated`：当前保证来自
  Control Plane 轮换 Binding 并用权威历史重建，而不是宣称旧 Provider-native Cursor 可跨模型复用。
- SaaS 模式不执行本地 Provider discovery、Provider-native Slash Command、Compact、Review、Fork、
  Side、Checkpoint 回滚、历史消息回滚重发或“Implement in a new thread”；handler 仍保留二次门禁。
- 未配置 Control Plane 时，上述门禁返回 local-allowed，现有本地 Provider Runtime 和 Native API 路径
  保持不变。

### K 验收

- 主界面可以创建 Control Plane Project/Session/Turn 并展示远程输出。
- 页面刷新、浏览器重连、Server 重启后 Transcript 一致。
- 一个 Session 只存在一个权威 Session ID/Event Sequence。
- 已有 SaaS Session 只有在无 Active Execution 且 Worker Manifest 已观测 `model-switch` 时才能切换；
  stale CAS、Active Execution、Unsupported/Unobserved Capability 均稳定失败，刷新或另一浏览器 SSE
  更新后 Picker 使用服务端模型。
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

| Target     | 必须验证                                                         |
| ---------- | ---------------------------------------------------------------- |
| Local      | Supervisor Restart、无 Control Plane 时本地兼容                  |
| SSH        | Host Key、systemd Restart、断网重连、远程磁盘清理                |
| Docker     | Container Replace、Volume/Checkpoint、Resource Limit             |
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

### L 当前证据（2026-07-16）

- `scripts/stage3-provider-acceptance/acceptance_runner.py` 已形成同一套用例编排、红线脱敏与
  JSON/Markdown 报告；Local、Docker 和 Kubernetes 通过用户 API、真实 Control Plane/agentd
  与产品 Target 生命周期执行，不代替 Worker 注册、Heartbeat 或 Claim。
- Runner 已显式建模 `standing` 与 `execution-pinned` Worker Allocation，并以 Capability 声明
  Managed Replacement；execution-pinned 路径使用 Approval 作为 Worker/Manifest 可观测屏障。
- 2026-07-15 当前工作区的 deterministic Codex fixture 已通过 Local 12/12 与 Docker 14/14；新增的
  Terminal Large Log case 覆盖 32 KiB Preview、三个 1 MiB 分段边界、Ready Artifact Size/SHA、退出汇总
  和物理路径泄漏扫描。Docker 同时通过 Managed Worker Replacement、Workspace 连续性、Control Plane
  Restart 与后续 Turn；Runner 精确清理其 Container、Volume、Network 和自动构建 Image。该结果来自
  未提交工作区，只作为实现期证据，最终 Commit 后仍需重新生成发布报告。
- Clean commit `fb9e25ec` 上，真实 Codex App Server 与 Claude Agent SDK 均通过共享 Runner 的 Local
  `real-provider-smoke` 12/12：路径经过用户 API、Control Plane、LocalSupervisor、agentd、Worker
  Protocol 与真实 Provider Host。第一 Turn 从 `cursor_absent` 的 authoritative history 启动；Control
  Plane restart 后第二 Turn 均命中 `native-cursor / cursor_usable` 并精确复现上一轮 marker。Codex Session
  Sequence 为 `1..42`，Claude 为 `1..41`；两份报告均为 `worktreeDirty=false`、精确 cleanup 和零 Secret
  finding。该证据关闭真实 Provider 的最小 Local 两轮产品路径 smoke，但仍不是完整 Local Release Suite。
- Clean commit `0b3f9214` 将 `real-provider-smoke` 扩展为 8 个可组合真实能力 case。Codex clean
  worktree 报告为 20 pass；Claude 为 19 pass + 1 explicit unsupported。Approval、Plan Mode User Input、
  Steer、Interrupt、Restart/Continuity、Review、Compact boundary、Rollback 和 Fork 均经过真实产品路径；
  Codex Review/Compact 为 native，Claude Review 为只读 emulated、Compact 为无 Session mutation 的稳定
  `capability_unsupported`，Rollback/Fork 为 Worker-free Control Plane emulation，Fork 后真实 Turn 使用
  `authoritative-history / cursor_absent` 精确复现 source marker。详见
  `docs/reports/stage-3-real-provider-local-control-matrix-0b3f9214.md`。该证据关闭已实现能力的 Local matrix，
  仍不替代真实 Provider 四 Target、故障、大输出和 soak Release Gate。
- Clean commit `f1b1aa53` 纳入第 9 个 canonical case `terminal-large`。Deterministic Fixture 对 32 KiB Preview、
  `1 MiB / 1 MiB / 257 B` 三段 Ready Artifact、固定 Size/SHA-256 与路径隔离保持严格断言。真实 Codex
  `0.144.x` 因 `unified_exec` 仅保留 1 MiB Head/Tail 而明确 Unsupported；不通过禁用执行路径来牺牲
  durable Approval。Claude ambient OAuth 也明确 Unsupported，因为不能同时保留用户登录查找路径并把 SDK
  `tool-results` 约束到 execution-scoped Runtime Output Root；受控 Credential 路径仍复用完整严格断言。
  不读取或复制用户 ambient Credential，也不放宽路径 containment。该边界不关闭真实 lossless 大输出、
  故障、SSH/Docker/Kubernetes 或 soak Gate。
- 第 10 个 canonical case `generated-file-checkpoint` 在真实 Codex/Claude Local 完整 matrix 中均通过。
  两者都把精确 `1 MiB + 257 B` 文件封装为 Ready `workspace_snapshot` Checkpoint Artifact；Runner 通过用户
  下载授权重新读取 Snapshot，并验证 Tar 安全、目标相对文件、已知 Runner 哨兵、固定内容 SHA、事件顺序、无重复 Ready、
  cleanup 与零 Secret finding。Codex 为 `21 pass + 1 unsupported`，Claude 为
  `20 pass + 2 unsupported`。它证明 Workspace Checkpoint 捕获，不把 standalone `generated_file`
  ArtifactCandidate、大 Diff、SSH/Docker/Kubernetes 或 Retention 并发伪装为已完成。详见
  `docs/reports/stage-3-real-provider-local-generated-file-matrix-f1b1aa53.md`。
- Clean commit `be919393` 将第 10 个 case 扩展为两个独立 Artifact 边界。Provider-native 精确路径先形成
  唯一 Ready `generated_file`，Runner 通过用户授权下载并验证 `43 B` 固定 payload；随后 shell 创建的大
  文件只通过 `workspace_snapshot` Checkpoint 持久化。Codex/Claude 均满足 standalone Ready 在
  `workspace.dirty` 前、Checkpoint Ready 在 Execution 完成前，且无重复 Ready、Tar 风险、物理路径或
  Secret 泄漏。该 clean-SHA matrix 关闭 Local standalone Generated File gate，但不关闭大 Diff、真实
  failure、SSH/Docker/Kubernetes 或 soak gate。详见
  `docs/reports/stage-3-real-provider-local-standalone-generated-file-matrix-be919393.md`。
- Clean commit `90fae52c` 的第 11 个 `large-diff` case 在完整真实 Local matrix 中通过。Codex 下载验证
  `320,258 B / 5,000 deletions`，Claude 下载验证 `320,201 B / 1 addition / 5,000 deletions`；两者均确认
  唯一 Ready `diff`、Artifact 引用顺序、无 inline 大 Payload、无物理路径、restart/Cursor continuity、
  cleanup 和零 Secret finding。Claude 同时覆盖配置路径与 canonical realpath 不同的 SDK 响应。详见
  `docs/reports/stage-3-real-provider-local-large-diff-matrix-90fae52c.md`。
- 首次 Claude 产品路径运行暴露 ambient OAuth 被 Execution-local `CLAUDE_CONFIG_DIR` 隔离掉的问题；
  Provider Host 现仅在受控 Credential 路径使用 Runtime Output Root 作为 Claude Config，ambient OAuth
  保留用户配置查找路径，并由单测和真实 clean-commit smoke 保护。
- 当前 dirty worktree 的 failure-only reports 通过 Local Provider malformed/oversized/crash、Docker
  Worker network interruption，以及 Kubernetes Worker network、Node drain、Pod eviction 和 image
  canary；所有 cleanup 和 output Secret scan 均通过。Kubernetes image canary 使用同内容 alias，不是
  registry-pushed immutable Revision promote/rollback。
- `2763ebd3` 的 Kubernetes Driver 在 owned disposable Kind Context 上通过真实 Kubernetes API 创建隔离
  Namespace、ServiceAccount/RBAC、短期 Token/CA 和 execution-pinned Pod/Manifest；重新构建当前
  Worker fixture 后 13/13 通过。该结果覆盖 Pending Approval Pod Delete、Generation 1→2 Fence、
  Interaction Request 换代、Artifact/User Input/Provider Error、Control Plane Restart、第二 Turn、
  Event Sequence 1→57；报告生成后的定向检查确认 owned 集群和精确自动构建镜像均已不存在。见
  `docs/reports/stage-3-kubernetes-provider-fixture-acceptance-2763ebd3.md`。当前 failure-only run 已补充
  deterministic Drain、Eviction、网络和同内容 Image Canary，但 clean-commit core report 仍是旧证据，
  两者都不覆盖真实 Provider Release Gate。
- 默认运行的是 deterministic Provider Host Protocol 2.1 fixture。它能证明共享 Contract、
  Control Plane-to-Worker-to-Host 通路和 Local/SSH/Docker/Kubernetes 恢复编排，**不等于**使用真实
  Codex App Server 或 Claude Agent SDK 的 Release Acceptance。
- SSH Driver 已实现 disposable OrbStack VM、一次性 SSH Credential、Host Key 固定以及产品级
  install/upgrade/revoke 生命周期。2026-07-14 的 deterministic Codex fixture 在 isolated disposable
  OrbStack Ubuntu 24.04 VM 上 13/13 通过，覆盖 Host Key mismatch 负例、sshd 重启、systemd Worker
  replacement、Workspace 连续性、Control Plane 重启、第二 Turn、revoke 与精确 VM 清理；另行对报告和
  日志执行的 Secret Scan 未发现 Private Key 模式。该结果不等于真实 Codex/Claude Adapter Release
  Acceptance；真实 Codex/Claude 各 Target、registry-pushed multi-arch rollout、长 Session、生产多节点
  Kubernetes 仍待执行；clean commit `253052aa` 已关闭真实 Codex/Claude consolidated Local slice，但
  不得声称四 Target 统一发布门禁已完成。当前 Local release 证据见
  `docs/reports/stage-3-real-provider-local-release-gate-253052aa.md`，较早 failure 证据见
  `docs/reports/stage-3-real-provider-local-failure-matrix-61e38f4f.md`，总体证据见
  `docs/reports/stage-3-provider-runtime-acceptance-fb9e25ec.md`；dirty-worktree/fixture 汇总保留在
  `docs/reports/stage-3-provider-runtime-acceptance-2026-07-15.md`。
- Runner 已补齐真实远程 Provider 的受控认证入口：SSH/Docker/Kubernetes 不再允许依赖宿主机 ambient
  登录，缺少显式 Credential source 时会在 Image build 前拒绝；Docker Worker 使用 Image 内的
  `/usr/local/bin/provider-host`，Credential 仍由 Control Plane 经 agentd FD 3 交付。当前只证明入口、
  fail-closed 和脱敏实现，不构成真实 Docker Codex/Claude product matrix 证据。
- SSH/Docker/Kubernetes 真实 Provider failure 路径已补齐受控 401/429 endpoint 与 scoped Host crash 注入。
  Endpoint 使用每次运行随机 route token 和临时宿主机端口；Docker 从精确 managed Worker 容器主动探测，
  Kubernetes 复用 Worker Control Plane 的 host-gateway，并以 execution-pinned Pod 发出的实际受控 Provider
  请求完成可达性证明，避免把完整 endpoint 持久化到 probe Pod Spec。SSH 不保留 provision 后已删除的一次性
  私钥，也不创建第二条隧道；它在既有 pinned-Host-Key Worker-only reverse relay 上临时注册精确 route-token
  upstream，仅该路由允许 Provider `x-api-key`/版本头与 429 响应头，并以实际 Provider 请求证明可达性后注销。
  Crash 共用 Linux `/proc` 扫描器：Docker 限定精确容器，Kubernetes 先要求 Target 下唯一 Running execution
  Pod，再在其 `agentd` 容器内遍历 PID 1 后代；SSH 以 owned disposable machine 中 systemd service MainPID
  为 agentd root。三者均只允许唯一 `--protocol-v2` 后代并发送 `SIGKILL`，候选为 0/多个、Pod 多义或返回
  结构异常时 fail closed。当前工作区已通过 Runner `99/99`、全套 Python `171/171`、真实 Linux 容器 crash
  探针、SSH token-scoped proxy focused tests、既有 Docker endpoint 探针与 deterministic Docker `16/16`；
  当前 dirty-worktree disposable OrbStack SSH fixture 也通过 `16/16`、精确 machine/key cleanup 与零 Secret
  finding。SSH real-provider runtime 预检进一步从当前源码构建并上传 Host bundle，按 checked-in npm lock
  在 disposable Ubuntu 24.04 安装并验证 Codex `0.144.1`、Claude Code `2.1.197`、Host SHA 与
  `/usr/local/bin/provider-host`，随后删除 machine/key 且未发现 Credential/private-key material。但仍缺受控
  Provider 凭据下的真实 SSH/Docker/Kubernetes product/failure 报告，因此四 Target Gate 保持 open。
- Consolidated release gate 已抽取 target-aware 公共验证器，Local 既有 CLI/Schema 与冻结 Unsupported 边界
  保持兼容；新增 `docker_release_gate.py` 在完全 clean SHA 上串行运行 Codex/Claude product + failure 四份
  child report。每个 child 仅继承工具白名单与当前 Provider Credential；Gate 从 clean SHA 单次构建带唯一
  ownership label 的 `worker-acceptance` Image，四个 child 通过 `--docker-skip-worker-build` 复用同一 tag，
  只清理各自 container/volume/network/state 并证明没有删除共享 Image。聚合器要求同一 Capability Catalog
  hash、与 Gate build 完全一致的 Worker Image ID、完整 case、exact child cleanup、空 Secret scan，且
  child/aggregate 均不持久化 operator 环境变量名或值；Gate 在包括 child 失败的 `finally` 路径校验 Image
  ownership + ID 后自行删除。当前 Local+Docker release-gate tests `32/32` 与缺 Credential/preflight 泄漏负例已通过；
  尚未执行 clean-SHA 真实 Docker 四矩阵，因此仍不构成发布证据。
- 受控远程 Gate 现进一步抽取 `controlled_remote_release_gate.py`，统一 Credential environment isolation、
  单次 Gate-owned Worker Image build、四 child 聚合、Catalog/Image consensus、Secret scan 与 ownership cleanup；
  `docker_release_gate.py` 保留原 CLI/Schema 的薄适配层。新增 `kubernetes_release_gate.py` 让四个 child 各自
  创建并删除 disposable Kind cluster，复用同一 host Worker Image 且验证嵌套
  `kubernetes.containerEngine` Image ID、`ownedClusterRemoved=true`、`ownedWorkerImageRemoved=false` 与
  isolated state cleanup。当前 Local/Docker/Kubernetes release-gate tests 合计 `40/40`，Stage 3 Python
  `171/171`，SSH/Docker/Kubernetes Gate 的缺 Credential/dirty-worktree 脱敏负例通过。当前机器的 Kind `v0.32.0`、
  kubectl `v1.33.9` 与 Docker `29.4.0` runtime preflight 已通过；任务环境仍没有专用 Provider Credential，
  因此尚未执行 clean-SHA Kubernetes 四矩阵，Gate 保持 open。
- 新增 `ssh_release_gate.py`，四个 child 各自 cross-build agentd/real Provider Host、创建唯一 disposable
  OrbStack machine/SSH key、安装 checked-in lock 固定的 Codex/Claude runtime，并经产品 SSH install/revoke
  路径运行 product/failure matrix。聚合器要求同一 clean SHA/Catalog、同一 agentd/Host digest 与 CLI version、
  四个不同 machine、完整 case、`machine/key/state` exact cleanup、空 Secret scan 和无 operator 环境变量名；
  fixture runtime、复用 machine 或任一 runtime/cleanup mismatch 均 fail closed。当前 SSH gate tests `10/10`，
  Local/SSH/Docker/Kubernetes release-gate tests 合计 `50/50`；尚缺专用 Provider Credential 下的 clean-SHA
  四 child 报告，因此真实 SSH Gate 保持 open。
- 新增 `registry_release_gate.py`，要求 clean worktree、现有 `docker-container` Buildx builder 和
  `linux/amd64,linux/arm64` 能力；对同一 Git SHA 分别执行 cached/no-cache push，并聚合 Registry digest、
  双平台 manifest、SPDX/SLSA attestation、non-root image config、嵌入 Manifest/SBOM/三类 lockfile 和
  Provider Host/agentd hash。BuildKit Syft scanner 也由 checked-in digest lock 固定，不再解析可变
  `stable-1` tag。可选 `--go-proxy` 只允许 credential-free HTTPS/direct/off，输出经过 redaction 与
  Secret scan，本地只清理本次 inspection container/image/state，禁止 prune，远端 image 保留为发布证据。
  当前 Registry gate tests `18/18`，全部 release-gate tests 合计 `68/68`，Stage 3 Python `171/171`；
  尚待在 clean implementation SHA 上完成真实双架构 Registry 运行并记录 digest，因此签名、生产 retention、
  四 Target rollout 与 soak 仍保持 open。

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
- 当前进度：`000034`/`000037`、Worker/Releases API、Web 运维入口和 rollout Runbook 已形成；真实
  Registry gate 已实现并由 `18/18` 单测覆盖，clean-SHA 双架构实测、image signing、真实 canary/rollback、
  Busy Worker 长任务和生产多节点证据仍是完成门禁。

### Step 6：Web 主流程切换

- 确认 Stage 2 依赖门禁。
- 建立 SaaS Projection Adapter。
- 逐步切换 Project/Session/Turn/Event 主路径。
- 保留未配置 Control Plane 的本地 Backend Adapter。

### Step 7：四 Target 验收与长稳测试

- Local/SSH/Docker/Kubernetes 运行同一 Provider Suite。
- 执行 Crash/Network/Upgrade/Approval 故障测试。
- 生成兼容矩阵和 Acceptance Report。
- 当前进度：deterministic Local/Docker core、Local Provider fault、Docker network、Kubernetes
  Network/Drain/Eviction/Image Canary 已通过实现期运行；SSH 13/13 与 Kubernetes 13/13 core 仍是
  2026-07-14 历史 fixture 证据。真实 Codex/Claude 已在 clean commit `fb9e25ec` 通过 Local 产品路径
  两轮 restart/native-Cursor smoke，clean commit `be919393` 的完整 Local matrix 也通过 standalone
  Generated File Artifact 与 Workspace Checkpoint 捕获；clean commit `90fae52c` 的 Codex/Claude 11-case
  matrix 也通过真实 Local Large Diff；clean commit `61e38f4f` 的两份独立 failure matrix 各通过
  `16/16` 真实 401/429、scoped Host crash 和 Cursor expiry/restart。clean commit `253052aa` 已将四份
  product/failure 报告聚合为同一 clean-SHA Local release gate 并通过；SSH、Docker、Kubernetes Gate 与
  soak 尚未完成。

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
- [ ] Provider Resume Cursor 失效时可以按策略从权威历史继续。（策略、数据库与 Host 契约测试已完成，
      clean commit `61e38f4f` 已通过真实 Codex/Claude Local expiry/restart；跨 Target Live Acceptance 仍未完成。）
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
