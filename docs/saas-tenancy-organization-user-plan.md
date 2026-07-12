# Synara SaaS 多租户控制面实施计划

> 目标：将 Synara 从本地优先的单用户应用演进为面向企业用户的通用 Agent SaaS。
>
> 本计划只定义 SaaS 控制面、租户/组织/用户、Session/Execution/Worker、持久化和远程执行边界；不要求一次性重写现有 TypeScript Provider Runtime。
>
> 当前状态：Phase 0-3 已实现。Phase 2 包含 Project/Agent Session/Turn/Event 的 DDL 与
> GORM 服务、Automation 归属、租户隔离 API、SSE 实时推送和断点续传；Phase 3 包含
> Execution/Worker/Lease、Worker 认证与心跳、Generation Fencing、过期恢复、回调幂等、
> Provider Resume Cursor 加密，以及 Turn/Execution/Outbox 原子入队。Phase 4-5 已完成；Phase 5
> 已通过真实 Docker 与 Kubernetes Worker 动态执行、Provider Host、Credential 管道和跨 Pod
> Session 连续性验收。Phase 6 已落地 Tenant 暂停、执行/Artifact 配额、Provider Credential KMS、
> 数据保留、低基数可观测性、OIDC、SAML、Service Account 和 SCIM。SAML 已通过
> Keycloak 企业 IdP 的真实 Redirect → POST → Session Cookie 端到端验收。
> 本次修订重新打开的 Phase 0 基础工作已实现：Deployment Profile/Execution Target v1
> Contract、Profile 启动兼容校验、PostgreSQL/Personal SQLite Metadata Adapter、Personal
> 首启初始化、Execution Target 持久化/API/Worker 绑定，以及 Personal → PostgreSQL 元数据
> 导出/导入与 Local Artifact → MinIO/S3 Payload 迁移已经落地。企业身份中的 SAML 使用
> 每连接独立且 KMS 加密的 SP 签名密钥、RSA-SHA256 AuthnRequest、一次性
> RelayState、InResponseTo/Destination/Audience/Issuer/时效校验和 Group Role Mapping。
>
> SaaS 产品路线“第二阶段：Go Control Plane 收口与生产化”的独立执行计划见
> `docs/plans/stage-2-go-control-plane-productionization.md`。

## 0. 本次主要变更点

本次修订在原有“企业 SaaS + K8s 动态 Worker”目标上，增加了个人用户最小化部署的
长期兼容要求。后续执行计划时必须理解以下差异，不能继续假设系统只有企业集群一种
运行形态。

### 0.1 新增两个正交维度

原计划主要按企业 K8s 形态设计。本次明确拆分为：

- `DeploymentProfile`：控制面和基础设施如何部署。
- `ExecutionTarget`：Agent 实际在哪里运行。

两者不能合并成同一个枚举。Docker 既可能是控制面的部署载体，也可能是 Agent 的执行目标，
必须在配置、API、数据库和代码命名中保持区分。

### 0.2 新增三种正式 Deployment Profile

- `personal`：个人本地、SSH 远程机或单 Docker 最小安装。
- `single-node`：单机服务端或 Docker Compose 部署。
- `enterprise`：多副本控制面、PostgreSQL、S3、Queue 和 K8s Worker。

只维护经过验证的 Profile，不允许用户任意拼装未经支持的基础设施组合。

### 0.3 新增四种 Execution Target

- `local`
- `ssh`
- `docker`
- `kubernetes`

所有 Target 必须通过统一 Worker Protocol、Runtime Event、Execution Lease 和 Artifact 接口
接入。不得为个人版建立绕过控制面的直连 Agent 通道。

### 0.4 个人版继续使用完整 SaaS 领域模型

个人版不允许省略 Tenant、Organization、User、Membership、Execution、Lease 等模型。
首次启动时自动创建 Personal Tenant、Root Organization 和 Local Owner User，避免未来从
个人版迁移到企业版时再次重构资源归属。

### 0.5 基础设施改为 Profile Adapter

原计划中 PostgreSQL 和 S3 是 SaaS 权威存储。本次补充：个人版允许使用 SQLite 和本地
Artifact 目录，但业务层必须依赖统一 Repository/ArtifactStore 接口。SQLite 仅允许单控制面
实例，不提供分布式语义。

### 0.6 新增升级和兼容要求

需要支持将 Personal Profile 的 SQLite、本地 Artifact 和 Provider Session 元数据迁移到
PostgreSQL、S3 和企业控制面。迁移是存储与部署形态迁移，不是领域模型迁移。

### 0.7 不变的核心原则

以下原则没有改变：

- 前端始终只连接 Control Plane。
- Worker 不拥有权威 Session 状态。
- Provider Adapter 不在第一阶段全量重写为 Go。
- Session Event 必须有序、幂等、可恢复。
- Worker Generation/Fencing 必须阻止双 Worker 写入。
- 企业模式仍以 PostgreSQL、S3 和 K8s 为正式生产基础设施。

## 1. 背景

当前 Synara 的认证模型主要服务于本地桌面和远程浏览器连接：

- `AuthSessionRole` 只有 `owner` 和 `client`。
- `auth_sessions` 记录的是设备/浏览器连接，而不是 SaaS 用户身份。
- 项目、线程、自动化、附件和 Provider Session 没有统一的 `tenant_id`。
- SQLite、Provider 进程、Git、PTY 和工作区都默认属于单个服务端实例。
- Session 的实时状态与进程内 Provider Session 绑定，尚不能由另一个 Worker 安全接管。

SaaS 目标要求：

- 支持公司、组织、用户和成员关系。
- 支持多租户数据隔离、RBAC、审计、配额和企业 SSO。
- 前端只连接控制面，不直接依赖具体 Agent Pod。
- Agent Worker 可以由 K8s 动态创建和回收。
- Session、Turn、Execution 和 Runtime Event 必须持久化并可恢复。
- PostgreSQL 保存事务状态，S3 保存附件、日志、产物和快照。
- Worker 本地磁盘和进程不能成为权威状态来源。

## 2. 总体决策

### 2.1 服务边界

采用“Go 控制面 + TypeScript Provider Runtime + 可选 Go Worker Supervisor”的渐进架构：

```text
Web Frontend
    |
    v
Go Control Plane
    |-- PostgreSQL
    |-- S3 / MinIO
    |-- Queue / Outbox
    |-- K8s Scheduler / Reconciler
    |
    v
Agent Worker Pod
    |-- Go agentd（后续）
    |-- TypeScript provider-host
    |-- Codex / Claude / Cursor / 其他 Provider
    |-- Git / PTY / Workspace
```

### 2.2 不做全量重写

第一阶段不重写现有 Provider Adapter。优先复用：

- Provider Adapter 接口。
- Codex App Server 管理。
- Claude Agent SDK 集成。
- Provider Runtime Event 标准化。
- Skills、Plugins、Commands 和模型发现。

Go 优先承担：

- SaaS API 和认证授权。
- 租户、组织、用户和资源管理。
- Session/Execution 状态机。
- Worker 注册、心跳、租约和调度。
- K8s Controller/Reconciler。
- 配额、审计、计费和可观测性。

### 2.3 数据存储职责

| 数据类型 | 权威存储 |
| --- | --- |
| 用户、租户、组织、成员关系 | PostgreSQL |
| Project、Agent Session、Turn、Execution | PostgreSQL |
| Session Event、审批、幂等记录、Worker Lease | PostgreSQL |
| 附件、图片、生成文件、终端长日志 | S3 / MinIO |
| Workspace 快照、Checkpoint 压缩包 | S3 / MinIO |
| 在线状态、短期缓存、限流计数 | Redis，可选，非权威 |
| 任务分发 | PostgreSQL Outbox 起步，后续可迁移 NATS/Kafka |

### 2.4 Deployment Profile

控制面的部署形态定义为：

```text
personal
single-node
enterprise
```

| 能力 | `personal` | `single-node` | `enterprise` |
| --- | --- | --- | --- |
| 典型安装 | 本地安装、SSH、单 Docker | 远程服务器、Docker Compose | K8s/Helm |
| 控制面副本 | 1 | 1 | 多副本 |
| Metadata Store | SQLite | PostgreSQL | PostgreSQL HA |
| Artifact Store | Local FS | MinIO/S3 | S3/Object Storage |
| 任务分发 | 进程内或数据库 | PostgreSQL Outbox | Outbox + NATS/Kafka 可选 |
| 认证 | Local Owner | Local/OIDC | OIDC/SAML/SCIM |
| 多租户 | 自动 Personal Tenant | 支持 | 强制 |
| 高可用 | 不支持 | 不支持或有限 | 支持 |

Profile 是经过验证的能力集合，不是配置模板名称。启动时必须拒绝不安全组合，例如：

- SQLite + 多控制面副本。
- Local Artifact Store + 多节点控制面。
- 进程内 Queue + 多副本调度。
- 未启用 Lease/Fencing 的远程 Worker。

### 2.5 Execution Target

Agent 的执行位置独立建模：

```text
local
ssh
docker
kubernetes
```

建议数据结构：

```text
execution_targets
  id
  tenant_id NULL              # NULL 表示平台级共享 Target
  organization_id NULL
  kind
  name
  status
  configuration_encrypted
  capabilities
  created_at
  updated_at
```

规则：

1. `local/worktree` 仍表示 Workspace Mode，不表示执行位置。
2. Session 绑定 `execution_target_id`，路径只在该 Target 内有意义。
3. 前端不能读取 Target 的连接密钥和底层 Pod 地址。
4. SSH Target 应运行 `synara-agentd`；SSH 主要用于安装、启动和升级，不长期依赖裸
   `ssh "codex ..."` 命令。
5. Docker Target 表示 Agent 在容器内执行，不等同于 Control Plane 运行在 Docker。
6. Kubernetes Target 通过 Worker Pool/Pod 调度，不直接暴露给前端。

### 2.6 统一 Worker Protocol

所有 Profile 和 Target 使用相同协议语义：

```text
Frontend -> Control Plane -> Worker -> Provider Runtime
```

个人版可以使用进程内 Transport、Unix Socket 或 Loopback；企业版可以使用 gRPC 双向流，
但命令、事件、Lease、Generation 和错误语义必须一致。

禁止出现：

```text
个人版：Frontend -> Provider
企业版：Frontend -> Control Plane -> Worker
```

这种双路径会导致权限、恢复、审批和事件处理长期分叉。

### 2.7 基础设施 Adapter

控制面业务层依赖统一接口：

```text
MetadataRepository
  |-- SQLiteRepository        # personal only
  `-- PostgreSQLRepository    # single-node / enterprise

ArtifactStore
  |-- LocalArtifactStore      # personal only
  |-- MinIOArtifactStore      # single-node
  `-- S3ArtifactStore         # enterprise

ExecutionTargetDriver
  |-- LocalDriver
  |-- SSHDriver
  |-- DockerDriver
  `-- KubernetesDriver
```

SQLite Adapter 只需要满足单节点事务语义，不得伪装成支持多副本 Claim/Lease。PostgreSQL
实现是企业模式的并发语义基准。

### 2.8 个人版自动初始化

Personal Profile 首次启动时自动创建：

```text
Tenant: personal-{installationId}
Organization: personal
User: local-owner
Tenant Membership: owner
Organization Membership: owner
Execution Target: local-default
```

所有 Project、Session、Execution、Artifact 仍写入完整的 Tenant/Organization 字段。

## 3. 领域术语

### 3.1 User

平台级身份。User 不直接包含 `tenant_id`，通过 Membership 加入一个或多个 Tenant。

### 3.2 Tenant

客户、合同、计费、安全和数据隔离边界，通常对应一家公司。

Tenant 负责：

- 套餐、计费和用量。
- 数据区域和保留策略。
- SSO、Provider Credential 和 KMS 策略。
- K8s 执行策略和资源配额。
- 全租户审计与安全策略。

### 3.3 Organization

Tenant 内部的协作和资源归属单元，例如研发部、业务团队或 Workspace。

Organization 负责：

- Project 归属。
- 成员和组织级权限。
- 默认 Provider、模型和执行环境。
- 部门配额和审批策略。
- Agent Session 的默认可见范围。

### 3.4 Membership

Membership 是 User 获得 Tenant 或 Organization 权限的唯一入口。

### 3.5 Login Session

用户浏览器或客户端登录状态。不得与 Agent Session、Provider Session 混用。

### 3.6 Agent Session

用户与 Agent 的长期会话，包含多个 Turn 和多次 Execution。

### 3.7 Agent Execution

一次具体执行尝试。Pod 重建、失败重试或恢复都会创建新的 Execution。

### 3.8 Provider Session

Codex、Claude 等 Provider 内部的会话，通过加密的 Resume Cursor 与 Agent Session 关联。

## 4. 多租户层级

```text
Tenant
├── Root Organization
├── Organization A
│   ├── Project A1
│   │   ├── Agent Session
│   │   │   ├── Turn
│   │   │   ├── Execution
│   │   │   └── Artifact
│   │   └── Automation
│   └── Project A2
└── Organization B
```

规则：

1. 创建 Tenant 时自动创建一个 Root Organization。
2. User 可以加入多个 Tenant。
3. Tenant Member 不自动拥有全部 Organization 权限，Tenant Owner/Admin 除外。
4. Organization 可以预留父子结构，但 v1 不实现复杂权限继承。
5. Project 必须属于一个 Organization，不允许 `organization_id` 为空。
6. Agent Session、Execution、Artifact 必须显式携带 `tenant_id`。
7. 资源一旦创建，不允许直接修改 `tenant_id`；跨租户移动必须走独立迁移流程。

## 5. 角色与权限

### 5.1 Tenant 固定角色

| 角色 | 职责 |
| --- | --- |
| `owner` | 全部权限，包括转移所有权和删除 Tenant |
| `admin` | 用户、组织、项目和 Agent 管理 |
| `security_admin` | SSO、Credential、审计和安全策略 |
| `billing_admin` | 套餐、额度和用量 |
| `auditor` | 只读审计和执行记录 |
| `member` | 只能进入被授权的 Organization |

### 5.2 Organization 固定角色

| 角色 | 职责 |
| --- | --- |
| `owner` | 组织全部管理权限 |
| `admin` | 成员、Project 和 Agent 设置管理 |
| `agent_operator` | 创建、停止、审批 Agent Execution |
| `member` | 创建和使用普通 Agent Session |
| `viewer` | 只读查看被授权资源 |

### 5.3 Permission 命名

业务代码只检查 Permission，不直接检查 Role 字符串。

```text
tenant.read
tenant.update
tenant.delete
tenant.members.read
tenant.members.invite
tenant.members.update
tenant.members.remove

organization.read
organization.update
organization.members.manage

project.create
project.read
project.update
project.delete

session.create
session.read
session.share
session.archive
session.delete

execution.create
execution.cancel
execution.approve
execution.read_logs

credentials.read
credentials.manage
worker.read
worker.manage
audit.read
billing.manage
```

v1 使用固定角色和代码内 Permission Map。自定义角色和复杂 ABAC 延后。

## 6. PostgreSQL Schema 规划

### 6.1 Identity 与登录

#### `users`

- `id UUID PRIMARY KEY`
- `email`
- `display_name`
- `avatar_url`
- `status`
- `email_verified_at`
- `created_at`
- `updated_at`
- `deleted_at`

约束：未删除用户的邮箱大小写不敏感唯一。

#### `user_identities`

- `id`
- `user_id`
- `provider`
- `issuer`
- `subject`
- `profile JSONB`
- `created_at`
- `last_login_at`

约束：`UNIQUE (issuer, subject)`。

#### `login_sessions`

- `id`
- `user_id`
- `active_tenant_id`
- `refresh_token_hash`
- `ip_address`
- `user_agent`
- `expires_at`
- `last_seen_at`
- `revoked_at`
- `created_at`

### 6.2 Tenant 与 Organization

#### `tenants`

- `id`
- `slug`
- `name`
- `status`
- `plan_code`
- `region`
- `settings JSONB`
- `created_by`
- `created_at`
- `updated_at`
- `deleted_at`

#### `tenant_memberships`

- `tenant_id`
- `user_id`
- `role`
- `status`
- `invited_by`
- `joined_at`
- `created_at`
- `updated_at`

主键：`(tenant_id, user_id)`。

#### `organizations`

- `id`
- `tenant_id`
- `parent_organization_id`
- `slug`
- `name`
- `kind`: `root | team | department | personal`
- `status`
- `settings JSONB`
- `created_by`
- `created_at`
- `updated_at`
- `archived_at`

约束：`UNIQUE (tenant_id, slug)` 和 `UNIQUE (tenant_id, id)`。

#### `organization_memberships`

- `tenant_id`
- `organization_id`
- `user_id`
- `role`
- `status`
- `created_at`
- `updated_at`

约束：Organization Member 必须同时是有效 Tenant Member。

### 6.3 Project 与 Agent Session

#### `projects`

- `id`
- `tenant_id`
- `organization_id`
- `name`
- `repository_url`
- `default_branch`
- `visibility`
- `created_by`
- `created_at`
- `updated_at`
- `archived_at`

#### `agent_sessions`

- `id`
- `tenant_id`
- `organization_id`
- `project_id`
- `created_by`
- `title`
- `status`
- `visibility`: `private | project | organization`
- `provider`
- `model`
- `execution_target_id`
- `provider_resume_cursor_encrypted`
- `last_event_sequence`
- `created_at`
- `updated_at`
- `archived_at`

#### `agent_turns`

- `id`
- `tenant_id`
- `session_id`
- `created_by`
- `status`
- `input_text`
- `started_at`
- `completed_at`
- `created_at`

#### `session_events`

- `tenant_id`
- `session_id`
- `sequence`
- `event_id`
- `event_version`
- `event_type`
- `actor_type`
- `actor_id`
- `execution_id`
- `payload JSONB`
- `occurred_at`

主键：`(tenant_id, session_id, sequence)`。

唯一约束：`(tenant_id, event_id)`。

### 6.4 Execution 与 Worker

#### `agent_executions`

- `id`
- `tenant_id`
- `session_id`
- `turn_id`
- `attempt`
- `status`
- `execution_target_id`
- `target_kind`
- `worker_id`
- `generation`
- `requested_by`
- `queued_at`
- `started_at`
- `finished_at`
- `failure_code`
- `failure_message`

#### `worker_instances`

- `id`
- `execution_target_id`
- `target_kind`
- `cluster_id`
- `namespace`
- `pod_name`
- `version`
- `capabilities JSONB`
- `lease_supported`
- `fencing_supported`
- `status`
- `registered_at`
- `last_heartbeat_at`
- `draining_at`
- `terminated_at`

#### `worker_leases`

- `tenant_id`
- `execution_id`
- `worker_id`
- `generation`
- `lease_token_hash`
- `acquired_at`
- `heartbeat_at`
- `expires_at`

主键：`execution_id`。

### 6.5 Artifact 与 Credential

#### `artifacts`

- `id`
- `tenant_id`
- `organization_id`
- `project_id`
- `session_id`
- `execution_id`
- `kind`
- `bucket`
- `object_key`
- `object_version`
- `content_type`
- `size_bytes`
- `sha256`
- `encryption_key_id`
- `created_by_type`
- `created_by_id`
- `created_at`
- `expires_at`
- `deleted_at`

#### `provider_credentials`

- `id`
- `tenant_id`
- `organization_id NULL`
- `provider`
- `credential_type`
- `encrypted_payload`
- `kms_key_id`
- `created_by`
- `created_at`
- `updated_at`
- `revoked_at`

Credential 明文不得写入日志、Event Payload 或 S3 Metadata。

### 6.6 Service Account 与审计

#### `service_accounts`

用于 CI/CD、自动化、Webhook、Worker 和系统集成，不伪装成普通 User。

#### `audit_logs`

- `tenant_id`
- `event_id`
- `actor_type`: `user | service_account | worker | system`
- `actor_id`
- `action`
- `resource_type`
- `resource_id`
- `organization_id`
- `request_id`
- `ip_address`
- `metadata JSONB`
- `occurred_at`

审计日志只追加，不允许业务 API 修改。

## 7. 多租户安全约束

以下规则是强制不变量：

1. 除全局身份表外，业务表必须包含 `tenant_id`。
2. Repository 查询必须显式接收 `tenantID`。
3. 禁止只通过资源 ID 查询业务资源。
4. 子表使用 `(tenant_id, resource_id)` 组合外键防止跨 Tenant 关联。
5. 唯一索引默认包含 `tenant_id`，除平台全局唯一字段外。
6. API Path 和认证上下文共同确定 Tenant，不能只信任客户端 Header。
7. PostgreSQL 可启用 RLS 作为第二层防御，但不能替代应用层授权。
8. Presigned URL 必须在 RBAC 校验后签发。
9. S3 Object Key 前缀不是权限边界。
10. 每个 Tenant 至少保留一个有效 Owner。

Go Request Context：

```go
type RequestContext struct {
    UserID         uuid.UUID
    TenantID       uuid.UUID
    OrganizationID *uuid.UUID
    LoginSessionID uuid.UUID
    RequestID      string
}
```

Repository 示例：

```go
GetAgentSession(ctx context.Context, tenantID, sessionID uuid.UUID)
```

禁止：

```go
GetAgentSession(ctx context.Context, sessionID uuid.UUID)
```

## 8. API 规划

### 8.1 Tenant

```text
POST   /v1/tenants
GET    /v1/tenants
GET    /v1/tenants/{tenantId}
PATCH  /v1/tenants/{tenantId}
DELETE /v1/tenants/{tenantId}
```

### 8.2 Tenant Membership

```text
GET    /v1/tenants/{tenantId}/members
POST   /v1/tenants/{tenantId}/invitations
PATCH  /v1/tenants/{tenantId}/members/{userId}
DELETE /v1/tenants/{tenantId}/members/{userId}
```

### 8.3 Organization

```text
GET    /v1/tenants/{tenantId}/organizations
POST   /v1/tenants/{tenantId}/organizations
GET    /v1/tenants/{tenantId}/organizations/{organizationId}
PATCH  /v1/tenants/{tenantId}/organizations/{organizationId}
DELETE /v1/tenants/{tenantId}/organizations/{organizationId}
```

### 8.4 Organization Membership

```text
GET    /v1/organizations/{organizationId}/members
POST   /v1/organizations/{organizationId}/members
PATCH  /v1/organizations/{organizationId}/members/{userId}
DELETE /v1/organizations/{organizationId}/members/{userId}
```

### 8.5 Agent Session

```text
POST   /v1/projects/{projectId}/sessions
GET    /v1/sessions/{sessionId}
GET    /v1/sessions/{sessionId}/events
POST   /v1/sessions/{sessionId}/turns
POST   /v1/sessions/{sessionId}/interrupt
POST   /v1/sessions/{sessionId}/archive
```

浏览器实时事件使用 WebSocket 或 SSE，并支持从 `lastSequence` 断点续传。

### 8.6 Execution Target 与 Platform Profile

```text
GET    /v1/platform/profile
GET    /v1/tenants/{tenantId}/execution-targets
POST   /v1/tenants/{tenantId}/execution-targets
GET    /v1/tenants/{tenantId}/execution-targets/{executionTargetId}
```

Profile Endpoint 只返回安全能力字段；Execution Target API 永不返回
`configuration_encrypted`。

## 9. Runtime Event 规范

所有 Runtime Event 必须包含：

```text
eventId
eventVersion
tenantId
organizationId
projectId
sessionId
executionId
workerId
generation
sequence
eventType
occurredAt
payload
```

规则：

1. `eventId` 全局唯一。
2. `sequence` 在单个 Agent Session 内单调递增。
3. Worker 上报重复 Event 时必须幂等。
4. 未持有当前 Lease Generation 的 Worker Event 必须拒绝。
5. Event Schema 必须版本化，不允许直接覆盖旧版本含义。
6. 大文件内容只引用 Artifact ID，不直接嵌入 Event Payload。

## 10. Worker Protocol 与 Lease

协议至少包含：

```text
RegisterWorker
Heartbeat
ClaimExecution
StartSession
ResumeSession
SendTurn
InterruptTurn
ResolveApproval
ResolveUserInput
RuntimeEvent
UploadArtifact
CompleteExecution
FailExecution
ReleaseLease
```

`RegisterWorker` 和 `ClaimExecution` 使用 `executionTargetId`/`targetKind`。Worker 注册后只能领取
该 Target 的 Execution；SSH/Docker/Kubernetes Worker 必须声明支持 Lease 和 Generation
Fencing。

Lease 规则：

1. 每个 Execution 同一时间只能有一个有效 Lease。
2. 每次重新分配都递增 `generation`。
3. Worker 定期续租。
4. Lease 过期后控制面可以创建恢复 Execution。
5. 旧 Generation 不允许提交 Event、Artifact 或最终状态。
6. Worker 失联不等于 Agent Session 立即失败，应先进入恢复状态。

## 11. S3 Artifact 规范

建议 Object Key：

```text
tenants/{tenantId}/
  organizations/{organizationId}/
    projects/{projectId}/
      sessions/{sessionId}/
        executions/{executionId}/
          artifacts/{artifactId}
```

上传流程：

1. 客户端或 Worker 请求创建 Artifact。
2. 控制面校验 Tenant、Organization、Session 和 Permission。
3. 控制面创建 `artifacts` Pending 记录。
4. 控制面签发短期 Presigned Upload URL。
5. 上传完成后提交 SHA-256、Size 和 Content-Type。
6. 控制面通过 HeadObject 校验后标记 Artifact Ready。

下载流程必须先校验 RBAC，再签发短期 Presigned Download URL。

## 12. K8s 隔离策略

### 12.1 标准租户

- 使用共享 Worker Namespace。
- 每个 Execution 独立 Pod 或从安全的 Worker Pool 中领取。
- Pod 通过 Label 标记 Tenant、Session、Execution 和 Generation。

### 12.2 企业独享租户

- 可分配独立 Namespace、Node Pool、NetworkPolicy 和 KMS Key。
- 可配置独立 Provider Credential 和数据区域。

### 12.3 Pod 标签

```yaml
synara.io/tenant-id: "..."
synara.io/organization-id: "..."
synara.io/session-id: "..."
synara.io/execution-id: "..."
synara.io/generation: "7"
```

### 12.4 Worker 不变量

- Worker Pod 不保存权威业务状态。
- Provider Credential 使用短期或可撤销凭据。
- Pod 使用非 Root 用户。
- 配置 CPU、内存、临时磁盘、执行时长和网络策略。
- Pod 被删除后必须能够通过 PostgreSQL 和 S3 恢复。

## 13. Go 控制面模块划分

第一版使用模块化单体，不提前拆成大量微服务：

```text
services/control-plane/
  cmd/api/
  internal/identity/
  internal/tenancy/
  internal/authorization/
  internal/organizations/
  internal/projects/
  internal/sessions/
  internal/executions/
  internal/executiontargets/
  internal/bootstrap/
  internal/database/
  internal/metadatamigration/
  internal/workers/
  internal/artifacts/
  internal/credentials/
  internal/audit/
  internal/outbox/
  internal/platform/
```

每个模块内部按以下层次组织：

```text
domain
application
repository
transport
```

跨模块只通过 Application Service 或明确的 Domain Contract 调用。

## 14. 实施阶段

### Phase 0：架构基线与协议冻结

状态：已完成原始基线、Profile/Target 基础决策和 ArtifactStore Contract。具体 Execution
Target Driver 和企业安全能力仍按后续阶段推进。

交付：

- Tenant/Organization/User ADR。
- 资源归属和隔离规则。
- ID、时间、错误码、分页规范。
- Runtime Event v1 Schema。
- Worker Protocol v1 草案。
- Role → Permission Matrix。
- Deployment Profile v1 Contract。
- Execution Target v1 Contract。
- Profile 能力矩阵和非法组合校验规则。

验收：

- 所有核心资源均能回答“属于哪个 Tenant、Organization、User”。
- 所有实时事件均能回答“由哪个 Execution、Worker Generation 产生”。
- 前端不需要知道 K8s Pod 地址。
- Personal、Single-node、Enterprise 使用同一套领域模型和 Worker Protocol。

### Phase 1：Identity、Tenant、Organization

实现：

- `users`、`user_identities`、`login_sessions`。
- `tenants`、`tenant_memberships`。
- `organizations`、`organization_memberships`。
- Tenant/Organization API。
- 固定角色 Permission Map。
- 邀请、加入、暂停、移除流程。
- 审计日志基础能力。

验收：

- User 可以属于多个 Tenant。
- 同一 User 在不同 Tenant 可拥有不同角色。
- Organization Member 必须是 Tenant Member。
- 无权限用户无法读取其他 Tenant 资源。
- 每个 Tenant 至少保留一个 Owner。

### Phase 2：Project 与 Agent Session 归属

状态：已完成。关系模型、组合外键、GORM API、前端创建/查看入口、基于 Sequence 的事件恢复、
SSE 实时推送和 `Last-Event-ID` 断点续传均已落地。

实现：

- Project 强制绑定 Tenant 和 Organization。
- Agent Session、Turn、Automation 全部绑定 Tenant。
- Session Visibility。
- Tenant-scoped Repository。
- Session Event 有序持久化。
- WebSocket/SSE 断点续传。

验收：

- 所有资源查询均包含 Tenant 条件。
- 跨 Tenant Project/Session 关联被数据库约束拒绝。
- 客户端可以从指定 Sequence 恢复 Event。

### Phase 3：Execution、Worker、Lease

状态：已完成。

实现：

- `agent_executions`、`worker_instances`、`worker_leases`。
- Worker 注册和心跳。
- Execution Claim、续租、完成和失败。
- Generation/Fencing Token。
- Worker 失联恢复状态机。
- Provider Resume Cursor 加密存储。

验收：

- 同一个 Execution 不能被两个 Worker 同时持有。
- 旧 Generation 不能写入新 Event。
- Worker 崩溃后可以创建恢复 Execution。
- 重复回调和 Event 不产生重复数据。

### Phase 4：S3 Artifact 与远程 Workspace

状态：已完成。Artifact Metadata、Local/MinIO/S3 Store、用户与 Worker 上传确认、下载删除、
服务端 Size/SHA-256/Content-Type 校验、短期 URL/Token、Generation Fencing，以及可重入
Local → MinIO/S3 Payload 迁移均已落地并通过 SQLite、PostgreSQL、Local FS、MinIO 实测。

实现：

- Artifact Metadata。
- Presigned Upload/Download。
- Attachment、生成文件、终端日志上传。
- Workspace Snapshot 和 Checkpoint 存储。
- 生命周期和删除策略。
- `LocalArtifactStore`、`MinIOArtifactStore`、`S3ArtifactStore` 统一接口。
- 本地 Artifact 到 S3 的可重入迁移任务。

验收：

- Worker 本地文件删除后，已提交 Artifact 仍可下载。
- 用户不能通过猜测 Object Key 访问其他 Tenant 文件。
- 上传内容通过 Size、Hash 和 Content-Type 校验。
- Personal Profile 不依赖 S3 也能完成相同 Artifact 生命周期。

### Phase 5：K8s 动态执行

状态：已完成。Execution Target 持久化/API、Session/Execution/Worker Target 绑定、
远程 Worker Lease/Fencing 能力校验、Claim Workload、通用 `synara-agentd` 出站 Worker、Runner
Event/Artifact/Result 协议、Agentd 容器镜像，以及配置驱动的 Local Agentd 自动监督、重启与
优雅停机、SSH Host Key 固定校验与 systemd 生命周期，以及 Docker Engine Worker Pool 调和、
资源限制、持久 Workspace、配置滚动替换和 Lease 感知缩容，以及 K8s Scheduler/Reconciler、
Pod 安全策略、ResourceQuota、NetworkPolicy 与集群 RBAC 部署基础已落地。Codex/Claude
Provider Host、Worker 镜像、匿名 FD Credential 传递、输出脱敏与有界权威历史重建已落地，
并通过真实 Docker Worker 与 kind Kubernetes Pod 的端到端执行、Pod 删除后后续 Turn 连续性验收。

实现：

- Execution Target 和 Worker Pool。
- K8s Pod Template。
- Pod 创建、观察、回收和恢复。
- ResourceQuota、NetworkPolicy 和 ServiceAccount。
- TypeScript `provider-host` 容器化。
- Worker 通过出站连接接入控制面。
- Local、SSH、Docker、Kubernetes Execution Target Driver。
- Personal、Single-node、Enterprise Profile 启动校验。
- SSH Worker 安装、注册、升级和撤销流程。
- Docker Worker 容器生命周期和资源限制。

验收：

- 前端只访问控制面。
- Pod 可以动态扩缩容。
- Pod 删除后 Session 不丢失。
- Session Event 在 Pod 重建前后保持连续和幂等。
- 同一 Agent Session 可以在兼容的 Execution Target 之间恢复或迁移。
- 不安全的 Profile/基础设施组合会在启动阶段被拒绝。

### Phase 6：企业安全与运营

状态：已完成。Tenant 暂停阻止新 Execution、Worker Claim 过滤暂停 Tenant、Tenant
并发 Execution 与 Ready Artifact 字节配额、固定角色配额权限、设置页查询/编辑入口、审计日志
筛选与稳定游标、JSONL/CSV 流式导出均已落地。配额校验通过 Tenant 行锁串行化，不维护冗余
用量计数，并已覆盖 SQLite 行为测试与 PostgreSQL 并发竞争测试；审计导出开始与完整结束都会
追加独立审计记录。
Provider Credential KMS、密钥轮换/撤销和 Worker Lease 约束解析已落地；Tenant Retention Policy、
单活 Sweeper、Artifact 先删对象后删 Metadata、过期临时记录清理已落地；Prometheus 指标、
Request/Trace 关联、依赖就绪检查和告警示例已落地；OIDC、Group Role Mapping、Service Account
与 SCIM v2 已落地。SAML 已完成 IdP Metadata、KMS 加密 SP 密钥、签名 AuthnRequest、
签名断言验证、一次性 RelayState、请求关联、域限制、Group Role Mapping 和真实 Keycloak E2E。

实现：

- OIDC/SAML SSO。
- SCIM 和 Group 预留。
- Provider Credential KMS 加密。
- Tenant 配额、并发限制和用量统计。
- 审计检索和导出。
- 数据保留、归档和删除流程。
- 指标、日志、Tracing、告警。

验收：

- Tenant 暂停后不能创建新 Execution。
- Credential 不出现在日志和 Event Payload。
- 所有敏感操作都有审计记录。
- 配额检查具备幂等性和并发安全。

## 15. 从当前 Synara 迁移

### 15.1 认证模型

现有 `owner/client` 只作为连接级角色保留，不映射为 Tenant RBAC。

迁移后的命名：

```text
auth_sessions          -> login_sessions / device sessions
thread session         -> agent_sessions
provider runtime state -> provider_sessions / resume cursors
```

### 15.2 本地单用户兼容

为桌面/本地版本提供单租户兼容模式：

1. 控制面首次启动时自动创建确定性的 `personal-{installationId}` Tenant。
2. 自动创建 Root/Personal Organization。
3. 自动创建本地 Owner User 和两级 Owner Membership。
4. 自动创建 Tenant/Organization 归属的 `local-default` Execution Target。
5. 现有 Project、Thread、Automation 回填 Tenant 和 Organization。
6. Personal 使用 SQLite；Single-node/Enterprise 使用 PostgreSQL Metadata Adapter。

### 15.3 Provider Runtime 抽取

优先抽取为独立 `provider-host`：

- Provider Adapter Registry。
- Provider Session Directory。
- Runtime Event normalization。
- Provider discovery。
- Codex、Claude、Cursor 等具体 Adapter。

控制面不直接依赖 Provider SDK，只依赖 Worker Protocol 和 Runtime Event Contract。

## 16. 测试计划

### 16.1 单元测试

- Role → Permission 映射。
- Tenant/Organization Membership 状态机。
- Session Event Sequence。
- Worker Lease Generation。
- Artifact Key 和校验逻辑。

### 16.2 PostgreSQL 集成测试

- 组合外键阻止跨 Tenant 关联。
- 并发 Claim 只有一个 Worker 成功。
- 重复 Idempotency Key 返回同一结果。
- Outbox 与业务事务原子提交。
- Tenant-scoped 查询不会返回其他 Tenant 数据。

### 16.3 安全测试

- Horizontal privilege escalation。
- 越权 Presigned URL。
- 旧 Lease Token 写入。
- Credential 日志泄漏。
- Organization Member 跨 Tenant 注入。

### 16.4 端到端测试

- 注册 → 创建 Tenant → 邀请用户 → 加入 Organization。
- 创建 Project → 创建 Session → 启动 Execution。
- Worker 崩溃 → Lease 过期 → 新 Worker 恢复。
- Artifact 上传 → Pod 删除 → Artifact 下载。
- WebSocket 断开 → 使用 Sequence 续传。
- Personal Profile：SQLite + Local Artifact + Local Worker 完整执行。
- Personal Remote：单机控制面 + SSH Worker 完整执行。
- Single-node：PostgreSQL + MinIO + Docker Worker 完整执行。
- Enterprise：多副本控制面 + S3 + K8s Worker 故障恢复。
- Personal 数据迁移到 PostgreSQL/S3 后 Session 和 Artifact 保持可访问。

### 16.5 Profile 兼容矩阵测试

必须覆盖：

- SQLite + 单副本：允许。
- SQLite + 多副本：拒绝启动。
- Local Artifact + 单副本：允许。
- Local Artifact + 多节点：拒绝启动。
- PostgreSQL + Local/SSH/Docker Worker：允许。
- PostgreSQL + K8s Worker：允许。
- 任意远程 Worker 未启用 Lease/Fencing：拒绝注册或拒绝领取 Execution。

## 17. 完成标准

- [x] Tenant、Organization、User 和 Membership 模型落地。
- [x] 登录 Session、Agent Session、Provider Session 明确分离。
- [x] 当前已落地业务资源显式绑定 Tenant；Personal 默认资源同时绑定 Organization。
- [x] Tenant-scoped Repository 成为强制规范。
- [x] 固定角色 Permission Map 完成。
- [x] Agent Session、Turn、Execution 和 Event 可持久化恢复。
- [x] Worker Lease 和 Generation 能阻止重复执行。
- [x] Artifact 使用 PostgreSQL Metadata + S3 Object。
- [x] 前端不感知具体 Worker Pod。
- [x] K8s Worker 可动态创建和销毁。
- [x] Codex/Claude Provider Runtime 通过独立 Worker Protocol、Provider Host 与统一 Worker 镜像复用。
- [x] 多租户隔离、安全、审计和恢复测试通过。
- [x] `personal`、`single-node`、`enterprise` 三种 Profile 有明确能力矩阵和启动校验。
- [x] `local`、`ssh`、`docker`、`kubernetes` 使用统一 Execution Target Contract 基础。
- [x] Personal Profile 首启自动初始化完整 Tenant/Organization/User/Membership/Target。
- [x] SQLite Metadata 作为单控制面 Adapter，不渗透到业务服务。
- [x] Personal 元数据可显式导出/导入 PostgreSQL并保留 Domain ID/加密 Resume Cursor。
- [x] Personal Local Artifact Payload 可迁移到 S3（Phase 4）。
- [x] 前端在所有 Profile 中都只连接 Control Plane。
- [x] Tenant Retention Policy、归档/删除顺序、单活 Sweeper 与幂等 Audit 落地。
- [x] Prometheus 指标、Request/Trace 关联、依赖就绪检查和告警示例不使用高基数业务 ID Label。
- [x] OIDC、Group Mapping、Service Account 与 SCIM v2 完成前后端/API 联通。
- [x] SAML 签名断言验证和真实企业 IdP 端到端验收。

## 18. 暂不实现

- 自定义角色编辑器。
- 复杂 ABAC 策略。
- 组织树权限自动继承。
- 跨 Tenant Project 共享。
- 经销商/子客户多级 Tenant。
- 每个 Tenant 独立数据库。
- 实时双向 Workspace 文件同步。
- 一次性将所有 Provider Adapter 重写为 Go。

## 19. STOP 条件

出现以下情况时暂停实现并重新评审设计：

- 无法明确某类资源的 Tenant 所有权。
- Worker Protocol 需要依赖控制面内部数据库结构。
- 前端需要直接连接 Worker Pod。
- Provider Credential 只能通过明文传输或落盘。
- Session 恢复依赖 Worker 本地唯一文件。
- 同一 Session 无法通过 Generation 阻止双 Worker 写入。
- 跨 Tenant 外键和查询无法由数据库或 Repository 强制约束。
- S3 Object 成为唯一 Session 状态来源。
- 个人版和企业版开始维护不同的领域模型或不同的 Session 状态机。
- SSH/Docker/K8s Driver 绕过 Worker Protocol 直接写 Session 状态。
- 系统允许 SQLite、Local Artifact 或进程内 Queue 运行在多控制面副本下。
- 个人版资源允许 `tenant_id` 或 `organization_id` 为空。

## 20. 第一批待确认决策

以下原有决策已由 `docs/adr/0001-saas-control-plane-tenancy.md` 冻结为 v1 默认值；如需变更
必须新增 ADR：

1. Tenant 是否严格对应一个付费客户/公司。
2. User 是否允许加入多个 Tenant。
3. Organization 是否需要父子层级，还是只做平级 Workspace。
4. 标准租户使用共享 Namespace，还是 Tenant 独立 Namespace。
5. Agent Session 默认可见性是 `private` 还是 `project`。
6. v1 使用 PostgreSQL Outbox，还是直接引入 NATS JetStream。
7. Runtime Event Payload 是 JSON Schema 还是 Protobuf。
8. Provider Credential 使用公司统一账号、用户账号，还是两者都支持。
9. Workspace v1 是否只支持 Git Clone/Push，不支持本地文件实时同步。
10. 桌面本地版和 SaaS 版是否长期共用同一套领域 Contract。

以下本次双轨部署设计决策已由
`docs/adr/0002-deployment-profile-execution-target-v1.md` 冻结为 v1 默认值；如需变更必须新增 ADR：

11. Personal Profile 的默认 Metadata Store 是否固定为 SQLite。
12. Single-node Profile 是否强制 PostgreSQL，还是允许 SQLite 兼容模式。
13. SSH Target 是否支持自动安装 `synara-agentd`。
14. Docker Target 是否按 Session 创建容器，还是使用常驻 Worker Pool。
15. Personal → Enterprise 迁移是否支持原地迁移，还是只支持导出/导入。
