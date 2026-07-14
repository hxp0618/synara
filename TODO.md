# TODO

## Small things

- [x] Submitting new messages should scroll to bottom
- [x] Only show last 10 threads for a given project
- [ ] Thread archiving
- [x] New projects should go on top
- [ ] Projects should be sorted by latest thread update

## Bigger things

- [x] Queueing messages

## SaaS product roadmap

> 这里的 Stage 是产品路线阶段，不等同于
> `docs/plans/saas-tenancy-organization-user-plan.md` 中的技术 Phase 0-6。
>
> 当前工作区已经包含大量 Control Plane、Provider Host、agentd、Docker、Kubernetes、
> Artifact、Credential 和企业身份基础实现。下面的 TODO 表示生产化与产品收口目标；执行每个
> Stage 前必须先做差距审计，禁止按旧计划重复实现已有模块。

| Stage   | 目标                                            | 状态                | 依赖             |
| ------- | ----------------------------------------------- | ------------------- | ---------------- |
| Stage 1 | 定义 SaaS 边界、Tenant/Organization/User 和协议 | 基线完成            | —                |
| Stage 2 | Go Control Plane 收口与生产化                   | 仓库内完成 / 已验收 | Stage 1          |
| Stage 3 | Provider Runtime 与远程 Worker 产品化           | IN PROGRESS         | Stage 2          |
| Stage 4 | 分布式执行平台和 K8s 多集群生产化               | TODO                | Stage 2、Stage 3 |
| Stage 5 | 企业 SaaS GA、运营、安全与商业化                | TODO                | Stage 2-4        |

Stage 2 的独立执行计划：
[`docs/plans/stage-2-go-control-plane-productionization.md`](docs/plans/stage-2-go-control-plane-productionization.md)

Stage 3 的独立执行计划：
[`docs/plans/stage-3-provider-runtime-remote-worker-productization.md`](docs/plans/stage-3-provider-runtime-remote-worker-productization.md)

### Stage 3：Provider Runtime 与远程 Worker 产品化

状态：IN PROGRESS。现有 `provider-host`、`synara-agentd`、Worker Protocol、Local/SSH/Docker/Kubernetes
Target 和 Codex/Claude 执行闭环属于基础实现，本阶段负责补齐 Provider 一致性、主流程权威
切换、协议兼容和长期运行能力。

#### 目标

- Control Plane 只依赖稳定的 Worker/Provider Host Contract，不依赖 Provider SDK 细节。
- 所有正式支持的 Provider 在 Local、SSH、Docker、Kubernetes Target 中具有一致的核心行为。
- TypeScript 本地 Orchestration 不再与 Go Agent Session 同时充当 SaaS 权威状态。
- Worker 可以升级、Drain、重连和恢复，不丢失 Session、Event、Artifact 或审批状态。

#### TODO

- [ ] 对 Codex、Claude、Cursor、Gemini、Grok、Kilo、OpenCode、Pi 做 Provider Host 能力矩阵审计。
- [ ] 冻结 Provider Host Protocol Version、最低兼容版本和能力协商规则。
- [ ] 为不支持的 Provider 能力返回显式 Capability/Unsupported 错误，不静默降级。
- [x] 实现 Web/Control Plane Provider Capability 投影与发送前门禁，并保持本地模式不变。
- [ ] 统一 Start、Resume、Send、Steer、Interrupt、Compact、Rollback 和 Fork 语义。
- [ ] 统一 Approval、Structured User Input、Plan Mode 和 Review 流程。
- [ ] 统一 Runtime Event 映射、Event Version 和未知事件兼容策略。
- [x] 实现 Provider Cursor TTL、未来时钟隔离、不可复活状态、可审计 Claim 选择，以及 Provider
      native invalid/expired 的 Turn-activity 前安全 fallback。
- [ ] 在真实 Codex/Claude 的 Local、SSH、Docker、Kubernetes Worker/Pod 迁移中验证 native Cursor
      invalid/expired、删除 Provider 本地状态后的恢复，以及已完成副作用不重复。
- [ ] 保持 Worker Token、Lease Token 和 Credential 不进入 Provider Runner 输入或日志。
- [ ] 完成 Tenant/Organization/User/Platform 四级 Provider Credential 解析策略评审。
- [ ] 完成 Git Clone/Fetch/Branch/Worktree/Push/PR 的远程 Workspace 生命周期。
- [ ] 明确 Workspace 清理、保留、快照、恢复和磁盘配额策略。
- [ ] 将终端、长日志、生成文件和 Checkpoint 统一投影为 Artifact/Event 引用。
- [ ] 建立 Worker/Provider Host 的 Graceful Shutdown、Drain 和正在执行任务交接协议。
- [x] 增加 Worker Image 与 Provider CLI/SDK 的版本清单和可重复构建机制。
- [ ] 增加 Worker 自动升级、回滚和不兼容版本隔离能力。
- [ ] 建立应用级 Control Plane Context 和 SaaS Session Projection Adapter。
- [ ] 将主聊天创建 Project/Session/Turn 的权威写入切换到 Go Control Plane。
- [ ] 保留未配置 Control Plane 时的本地个人模式，避免维护两套领域模型。
- [ ] 为 Local、SSH、Docker、Kubernetes 分别建立相同的 Provider Acceptance Suite。
- [ ] 验证 Worker 崩溃、网络中断、Provider 崩溃和控制面滚动升级后的 Session 连续性。

#### 完成条件

- [ ] 所有正式支持 Provider 的核心能力矩阵有自动化验证。
- [ ] Web 主流程只存在一个 SaaS Session 权威来源。
- [ ] Worker/Provider Host 升级不需要迁移业务数据库结构。
- [ ] 不同 Execution Target 使用相同 Worker Protocol 和 Runtime Event Contract。
- [ ] Pod/Worker 替换后可以继续后续 Turn，并保持有序 Event 历史。
- [ ] Credential、Token、Prompt 和用户文件没有非预期日志泄漏。

### Stage 4：分布式执行平台和 K8s 多集群生产化

状态：TODO。当前 Kubernetes Reconciler、Docker Worker Pool、SSH Provisioner 和执行级 Pod
已经存在，本阶段目标是将其提升为可容量规划、可跨集群调度、可升级和可灾难恢复的执行平台。

#### 目标

- Control Plane 可以将 Execution 调度到不同 Cluster、Region、Worker Pool 和资源等级。
- 扩缩容、调度、配额、故障恢复和升级都具备明确的一致性与运维语义。
- Personal/Single-node 的 Local、SSH、Docker 方式继续可用，不因企业 K8s 能力而分叉。

#### TODO

- [ ] 定义 Cluster、Region、Worker Pool、Capacity Class 和 Placement Policy 数据模型。
- [ ] 定义 Tenant/Organization 对 Cluster、Region、Provider 和资源规格的允许策略。
- [ ] 实现基于 Target、Provider、Region、容量和配额的调度决策。
- [ ] 实现 Worker Pool 容量上报、可调度容量和排队时间指标。
- [ ] 决定每 Execution Pod、常驻 Worker Pool、Warm Pool 的适用场景。
- [ ] 为交互式 Agent 建立 Warm Pool 和冷启动上限。
- [ ] 为自动化和批处理任务建立独立 Queue/Priority/Class。
- [ ] 实现 Tenant、Project、Session 和 Automation 级并发与资源配额。
- [ ] 实现公平调度，避免单个 Tenant 占满共享 Worker Pool。
- [ ] 支持 Priority、Preemption 或明确拒绝不支持的抢占策略。
- [ ] 完成 K8s Namespace、ServiceAccount、NetworkPolicy、ResourceQuota 和 Pod Security 收口。
- [ ] 将 Worker Registration Secret 改为短期、可轮换或工作负载身份凭据。
- [ ] 验证 AWS/GCP/Azure Workload Identity，不依赖长期静态云密钥。
- [ ] 建立 Provider API Egress Allowlist 和 Tenant 自定义网络策略。
- [ ] 支持私有 Git、企业代理、私有镜像仓库和内部 Package Registry。
- [ ] 设计 Workspace Storage：Ephemeral Disk、PVC、Snapshot、Object Storage 和 Git 的边界。
- [ ] 实现 Worker/Pod Drain、滚动升级、灰度发布和自动回滚。
- [ ] 实现 Cluster 不可用时的 Execution 停止、重排和恢复策略。
- [ ] 实现 Region 故障时的控制面和 Artifact/Metadata 恢复策略。
- [ ] 增加 Outbox/Queue 堆积时的自动扩容和限流策略。
- [ ] 建立多集群 Reconciler Leader Election 和幂等 Apply/Delete。
- [ ] 增加 Worker Pod 创建失败、ImagePullBackOff、Pending、Evicted、OOMKilled 分类。
- [ ] 建立资源成本、Pod 启动时间、Execution 排队时间和利用率指标。
- [ ] 执行多副本 Control Plane、多个 K8s Cluster 的压力和混沌测试。
- [ ] 编写 Cluster 接入、升级、下线、Credential 轮换和事故处理 Runbook。

#### 完成条件

- [ ] 单个 Cluster 或 Control Plane Pod 故障不会丢失权威 Session 状态。
- [ ] 调度决策可解释、可审计、可重放。
- [ ] Shared 和 Dedicated Worker Pool 均有隔离与容量验证。
- [ ] Worker Pool 可以根据 Queue Depth 和目标延迟安全扩缩容。
- [ ] 滚动升级期间已运行 Execution 不被错误重复执行。
- [ ] 跨 Cluster/Region 恢复有可重复演练记录。
- [ ] Personal、SSH、Docker 单机部署没有因 K8s 调度模型产生回归。

### Stage 5：企业 SaaS GA、运营、安全与商业化

状态：TODO。当前 OIDC、SAML、SCIM、Service Account、Audit、Quota、Retention、KMS 和基础
Observability 已存在，本阶段负责补齐企业可运营、可支持、可计费、可合规和可正式发布的能力。

#### 目标

- 产品可以面向多家公司正式提供服务，而不依赖人工数据库操作和开发模式配置。
- Tenant 生命周期、用户生命周期、用量、成本、配额、安全、审计和支持流程形成闭环。
- 建立明确的 SLO、发布、备份恢复、安全响应和数据治理制度。

#### TODO

- [ ] 完成 Tenant 注册、试用、启用、暂停、关闭、删除和恢复状态机。
- [ ] 完成企业邀请、OIDC/SAML、SCIM、Group Mapping 和离职回收闭环。
- [ ] 支持多个 Identity Connection、Domain Verification 和 SSO Enforcement。
- [ ] 评估是否需要自定义 Role；若不需要，冻结固定 RBAC v1。
- [ ] 完成 User、Service Account、Credential 和 API Token 的生命周期管理。
- [ ] 实现 Provider BYOK、企业统一 Credential 和用户 Credential 的策略与优先级。
- [ ] 完成 Credential Rotation、KMS Key Rotation、Re-encryption 和应急撤销 Runbook。
- [ ] 建立 Tenant Plan、Entitlement、Quota 和 Feature Flag 模型。
- [ ] 记录 Token、Execution Time、CPU、Memory、Storage、Network 和 Provider Cost 用量。
- [ ] 实现用量聚合、账单周期、超限行为和管理员报表。
- [ ] 如需要对外收费，接入 Billing Provider；内部平台则接入成本中心/部门分摊。
- [ ] 建立 Tenant/Organization 管理后台和 Platform Admin 后台。
- [ ] 实现 Worker、Execution、Queue、Artifact、Credential 和 Identity Connection 运维视图。
- [ ] 建立 Audit Search、Export、Legal Hold 和保留策略。
- [ ] 完成数据导出、Tenant 删除、用户删除和隐私请求流程。
- [ ] 定义 PostgreSQL、S3、KMS、Queue 的备份、恢复和 RPO/RTO。
- [ ] 定期执行数据库恢复、对象恢复和区域灾备演练。
- [ ] 定义 Availability、API Latency、Execution Start Delay、Event Delay 等 SLO。
- [ ] 建立错误预算、告警分级、On-call 和事故响应流程。
- [ ] 完成结构化日志、Tracing、Metrics 与 Tenant 安全边界审计。
- [ ] 执行跨 Tenant 越权、SSRF、命令注入、路径穿越、供应链和容器逃逸测试。
- [ ] 对 Worker Image、Provider CLI、依赖和 SBOM 建立签名与漏洞扫描。
- [ ] 完成 Secret 管理、生产配置、证书、域名和密钥轮换流程。
- [ ] 建立数据库 Migration、协议版本、Worker Image 和前端的兼容发布矩阵。
- [ ] 建立 Canary、灰度、回滚和向后兼容发布流程。
- [ ] 完成用户文档、管理员文档、API 文档、部署文档和故障排查文档。
- [ ] 明确 Provider 许可、账号共享、数据使用、隐私和企业合规要求。
- [ ] 完成容量测试、长时间稳定性测试、渗透测试和上线评审。

#### 完成条件

- [ ] 新 Tenant 可以不经人工数据库操作完成注册或企业开通。
- [ ] 用户入职、调岗、离职和 Credential 回收有完整审计链路。
- [ ] 用量、配额、成本和超限策略可解释且并发安全。
- [ ] 生产环境有明确 SLO、告警、值班和事故处理流程。
- [ ] 备份恢复与区域灾备经过真实演练。
- [ ] 安全测试没有未接受的高危问题。
- [ ] 协议、数据库和 Worker 升级支持灰度与回滚。
- [ ] 企业管理员和平台运维人员不依赖开发工具完成日常操作。
- [ ] GA Release Checklist 全部通过。

### Roadmap-wide rules

- [ ] 每个 Stage 开始前重新审计当前代码和计划状态。
- [ ] 已有功能以真实测试和验收证据为准，不以文件名或计划勾选状态为准。
- [ ] Personal、Single-node、Enterprise 保持同一领域模型，不维护独立产品分支。
- [ ] Local、SSH、Docker、Kubernetes 保持同一 Worker Protocol。
- [ ] 前端不直接连接 Worker，Worker 不直接访问 Control Plane 数据库。
- [ ] PostgreSQL 保存事务状态，S3/MinIO 保存大对象，Worker 本地状态可随时丢弃。
- [ ] 所有跨进程命令和事件都必须幂等、版本化并可审计。
