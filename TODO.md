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
| Stage 3 | Provider Runtime 与远程 Worker 产品化           | 已完成 / 已验收     | Stage 2          |
| Stage 4 | 分布式执行平台和 K8s 多集群生产化               | IN PROGRESS         | Stage 2、Stage 3 |
| Stage 5 | 企业 SaaS GA、运营、安全与商业化                | TODO                | Stage 2-4        |

Stage 2 的独立执行计划：
[`docs/plans/stage-2-go-control-plane-productionization.md`](docs/plans/stage-2-go-control-plane-productionization.md)

Stage 3 的独立执行计划：
[`docs/plans/stage-3-provider-runtime-remote-worker-productization.md`](docs/plans/stage-3-provider-runtime-remote-worker-productization.md)

### Stage 3：Provider Runtime 与远程 Worker 产品化

状态：COMPLETE。Runtime 发布源码固定为 `8415efa15cebc48a23723dbdb147d3bafd7071bf`；本地验证环境、临时输出和
最终生成报告已按操作人要求清理。

验收边界：远程 Agent 的正式路径是第三方 API Key、可选 Base URL 和自定义模型。每个 Provider Adapter
保留契约、产品路径和可控故障验证；耗额度的长时间 load/soak、多节点与 immutable rollout 只要求一个
代表性 API-key Provider 通过。订阅/OAuth 登录属于低优先级兼容项，延期到 Stage 3 之后，不阻断本阶段发布。

#### 目标

- Control Plane 只依赖稳定的 Worker/Provider Host Contract，不依赖 Provider SDK 细节。
- 所有正式支持的 Provider 在 Local、SSH、Docker、Kubernetes Target 中具有一致的核心行为。
- TypeScript 本地 Orchestration 不再与 Go Agent Session 同时充当 SaaS 权威状态。
- Worker 可以升级、Drain、重连和恢复，不丢失 Session、Event、Artifact 或审批状态。
- Worker canary/promoted 观察窗口可自动判定并触发安全回滚；第三方 Provider 鉴权、限流和网络故障不作为
  Worker Release 回滚信号。

#### TODO

- [x] 对 Codex、Claude、Cursor、Antigravity、Grok、Kilo、OpenCode、Pi 做 Provider Host 能力矩阵审计。
- [x] 冻结 Provider Host Protocol Version、最低兼容版本和能力协商规则。
- [x] 为不支持的 Provider 能力返回显式 Capability/Unsupported 错误，不静默降级。
- [x] 实现 Web/Control Plane Provider Capability 投影与发送前门禁，并保持本地模式不变。
- [x] 统一 Start、Resume、Send、Steer、Interrupt、Compact、Rollback 和 Fork 语义。
- [x] 统一 Approval、Structured User Input、Plan Mode 和 Review 流程。
- [x] 统一 Runtime Event 映射、Event Version 和未知事件兼容策略。
- [x] 实现 Provider Cursor TTL、未来时钟隔离、不可复活状态、可审计 Claim 选择，以及 Provider
      native invalid/expired 的 Turn-activity 前安全 fallback。
- [x] 在真实 Codex/Claude 的 Local、SSH、Docker、Kubernetes Worker/Pod 迁移中验证 native Cursor
      invalid/expired、删除 Provider 本地状态后的恢复，以及已完成副作用不重复。
- [x] 保持 Worker Token、Lease Token 和 Credential 不进入 Provider Runner 输入或日志。
- [x] 完成 Tenant/Organization/User/Platform 四级 Provider Credential 解析策略评审。
- [x] 完成 Git Clone/Fetch/Branch/Worktree/Push/PR 的远程 Workspace 生命周期。
- [x] 明确 Workspace 清理、保留、快照、恢复和磁盘配额策略。
- [x] 将终端、长日志、生成文件和 Checkpoint 统一投影为 Artifact/Event 引用。
- [x] 建立 Worker/Provider Host 的 Graceful Shutdown、Drain 和正在执行任务交接协议。
- [x] 增加 Worker Image 与 Provider CLI/SDK 的版本清单和可重复构建机制。
- [x] 增加 Worker 自动升级、回滚和不兼容版本隔离能力。
- [x] 建立应用级 Control Plane Context 和 SaaS Session Projection Adapter。
- [x] 将主聊天创建 Project/Session/Turn 的权威写入切换到 Go Control Plane。
- [x] 保留未配置 Control Plane 时的本地个人模式，避免维护两套领域模型。
- [x] 为 Local、SSH、Docker、Kubernetes 分别建立相同的 Provider Acceptance Suite。
- [x] 验证 Worker 崩溃、网络中断、Provider 崩溃和控制面滚动升级后的 Session 连续性。

#### 完成条件

- [x] 所有正式支持 Provider 的核心能力矩阵有自动化验证。
- [x] Web 主流程只存在一个 SaaS Session 权威来源。
- [x] Worker/Provider Host 升级不需要迁移业务数据库结构。
- [x] 不同 Execution Target 使用相同 Worker Protocol 和 Runtime Event Contract。
- [x] Pod/Worker 替换后可以继续后续 Turn，并保持有序 Event 历史。
- [x] Credential、Token、Prompt 和用户文件没有非预期日志泄漏。

### Stage 4：分布式执行平台和 K8s 多集群生产化

状态：IN PROGRESS。独立执行计划：
[`docs/plans/stage-4-distributed-execution-resource-lifecycle.md`](docs/plans/stage-4-distributed-execution-resource-lifecycle.md)

当前 Kubernetes Reconciler、Docker Worker Pool、SSH Provisioner 和执行级 Pod
已经存在，本阶段目标是将其提升为可容量规划、可跨集群调度、可升级和可灾难恢复的执行平台。

#### 目标

- Control Plane 可以将 Execution 调度到不同 Cluster、Region、Worker Pool 和资源等级。
- 扩缩容、调度、配额、故障恢复和升级都具备明确的一致性与运维语义。
- Personal/Single-node 的 Local、SSH、Docker 方式继续可用，不因企业 K8s 能力而分叉。

#### TODO

- [x] 建立按 Execution Generation 冻结、带 SHA-256 和 predecessor lineage 的原子 Recovery Bundle。
- [x] 建立独立资源状态及 `meaningfulActivityAt`、`resourceIdleSince`、`absoluteExpiresAt` 权威时间轴。
- [x] 建立 User、Project、Session scoped Memory Revision/Artifact authority，并固定进 Recovery Bundle。
- [x] 将跨域 DR 的 Result Artifact required-set 与 `fitResumeSnapshotBudget` 的最终裁剪集合收敛到同一契约：
      Resume Snapshot 的 byte/token budget 不再删除 Result Artifact 引用本体，只会先裁剪 narrative / tool /
      interaction 元数据以及可选 Artifact metadata；若精确引用集合本身仍超预算则 fail closed。
- [x] 实现 `waiting-for-approval` suspend attempt、quiesce 后 Checkpoint、lease-free `suspended`、Pod 删除和 Resolve 后新 Generation Resume；支持签名 strict-containment 与 Kubernetes Pod-terminal 两种完成证明。
- [x] 实现 Deployment defaults、Tenant override、Project override 三层资源生命周期策略，创建 Session 时冻结 effective snapshot，并提供 Web 配置/展示。
- [x] 增加 suspend attempt、Session resource state、Recovery Bundle reason 和 Generation claim-to-start 基础指标。
- [x] 实现 `absoluteExpiresAt` server-authoritative controller；到期 Session 会拒绝新的 execution-bearing 操作，并权威取消已在飞 / suspended Execution。
- [x] 为 `suspendAfterIdleSeconds` 实现 active-turn Checkpoint/Suspend/显式 Resume：Provider Host 2.2
      `SuspendTurn` 必须返回 interrupted terminal + native cursor，Control Plane 将 Generation、active command、
      history/activity sequence、runtime binding 和 cursor digest 原子冻结成一次性 receipt；无 receipt、终止证明
      不完整、cursor 失效或已消费 Generation 丢失都 fail closed，不能回退到 authoritative-history 重放在途 Tool。
- [x] 将 supervisor/provider 的 mode/version/probe/identity 声明冻结进不可变 Worker Manifest，并让 Suspend
      eligibility 只信当前 Execution 绑定的 Manifest；Execution Target `signed-v1` Ed25519 policy 会验证绑定
      Target/Pod UID/image/build/probe 的签名，策略 key 轮换使旧 Manifest fail closed，Migration `000052` 将
      历史 self-attestation 标记为 `legacy-untrusted`。临时 Worker capability 不能宣告 strict containment。
- [x] 为 Linux/非 Kubernetes `worker-attested-v1` Provider 建立不同安全身份、专属受保护 cgroup-v2
      supervisor：启动前 fd-relative 绑定、`Pdeathsig`、generation/incarnation fence、`cgroup.kill`、
      `populated=0` 和 root-only 签名 gate 已落地，并有特权 Linux 容器与隔离 OrbStack Ubuntu VM 证明。
      生产 SSH 宿主机仍必须执行同一 gate，不能用本地证明替代生产 escape-resistance 验收；Kubernetes waiting
      与 active-turn Suspend 继续使用 kubelet 对精确 Pod UID 的 `Succeeded` terminal proof。
- [x] 建立按 Execution Generation 冻结的 opaque Provider Credential Grant，并让新 Generation 的 agentd
      只通过 Grant + 当前 Lease 解析 Provider Credential；旧 Credential-ID 路径不能绕过已存在的 Grant，legacy
      Bundle/receipt 缺 Grant 时重放 fail closed，tenant deleting 后停止 Secret 下发。
- [x] 将 Worker Lease 冻结到物理 incarnation / instance UID；重注册 Worker 不得继承旧 Pod 的 Generation 权限，
      Suspend completion 会再次检查当前 Manifest 的 strict-containment proof，或精确匹配 Pod-bound Worker
      identity 与 kubelet terminal proof。
- [x] Migration `000054` 冻结 suspend attempt 的 Target/Worker incarnation/Pod name/UID，增加一次写入的
      checkpoint-ready 与 `Succeeded` terminal proof；只有 Reconciler 观察到唯一 agentd container exit 0 才能
      原子完成 Kubernetes Suspend，`DELETE 202`、`NotFound`、`Failed/Unknown` 均不算证明。
- [x] 将 semantic activity 接到 Generation-fenced Provider Credential 短期 access broker：Migration `000055`
      持久化单调 activity watermark 与 Worker-Lease authorization，只有显式 Grant resolve 可首次签发；agentd
      对 rotation/revocation/scope loss、hard expiry、元数据回退和续租流中断 fail closed。当前静态
      `apiKey`/`authToken` 不做 Generation 内热换，KMS/STS workload identity 仍按各云生产验证项推进。
- [x] 定义 target-local Cluster/Region、Worker Pool、Capacity Class 和 Placement Policy v1，并实现带健康、容量、
      source DR domain、复制 watermark 和 Artifact/Checkpoint/Memory readiness 的跨 Target/Region/Cluster 分组选择；
      Create Session、普通 Turn、review/compact 与 failover 都会逐候选执行 placement-aware Provider capability
      门禁，失败候选不会提前推进 Session 路由 authority，同 Target 的其他 Pool observation 也不能误放行。
- [x] 为 tenant-owned managed Kubernetes Target 建立由 Reconciler 每次 fresh per-target 结论同步触发的
      `execution_target_health` 自动发布；platform-shared/external Target 与跨域 `execution_target_dr_readiness`
      仍保持 operator/integration-owned authority。
- [x] 建立 tenant-scoped Region/Cluster evacuation authority，并让 fresh location outage 在普通路由与 lease-free
      自动 failover sweep 中都能 fail closed 地触发 `region-failover`，同时保持 source Execution placement 不可变。
- [ ] 定义 Tenant/Organization 对 Cluster、Region、Provider 和资源规格的允许策略。
- [ ] 在现有 Target/Region、Provider/Pool correctness filtering 上增加 live capacity、配额和 Provider affinity
      的统一排序决策。
- [ ] 实现 Worker Pool 容量上报、可调度容量和排队时间指标。
- [x] 冻结每 Execution Pod、常驻 Worker Pool、Warm Pool 的适用场景与 one-shot Warm Pod 安全契约。
- [ ] 为交互式 Agent 建立冷启动硬上限；target-local release-aware Warm Pool 与实测 P50/P95/P99 已落地，
      生产 SLO 门禁仍未完成。
- [ ] 为自动化和批处理任务建立独立 Queue/Priority/Class。
- [ ] 实现 Tenant、Project、Session 和 Automation 级并发与资源配额。
- [ ] 实现公平调度，避免单个 Tenant 占满共享 Worker Pool。
- [ ] 支持 Priority、Preemption 或明确拒绝不支持的抢占策略。
- [ ] 完成 K8s Namespace、ServiceAccount、NetworkPolicy、ResourceQuota 和 Pod Security 的生产验收（本地
      OrbStack real-kubelet final5 Migration 68/RBAC/TokenReview/Leader takeover 与 disposable Kind 4 节点 6/6 场景、
      20 秒 partition、实际 601 秒 soak 已通过；仍需托管云多可用区、云存储/负载均衡和生产时长验收）。
- [x] 将 Kubernetes Worker Registration Secret 改为 10 分钟自动轮换、Pod-bound、target-scoped audience 的
      ServiceAccount token；Control Plane 通过 TokenReview + 实时 Pod GET 校验 SA、Pod name/UID 和 ownership，
      同一物理 Pod UID 只能注册一次。Local/SSH/Docker 仍保留独立 shared-token 路径。
- [ ] 验证 AWS/GCP/Azure Workload Identity，不依赖长期静态云密钥。
- [ ] 建立 Provider API Egress Allowlist 和 Tenant 自定义网络策略。
- [ ] 支持私有 Git、企业代理、私有镜像仓库和内部 Package Registry。
- [ ] 设计 Workspace Storage：Ephemeral Disk、PVC、Snapshot、Object Storage 和 Git 的边界。
- [ ] 实现 Worker/Pod Drain、滚动升级、灰度发布和自动回滚。
- [x] 实现 Cluster 不可用时的 Execution 停止、重排和恢复 authority：过期 Target health、tenant-scoped
      Region/Cluster evacuation 与 lease-free failover 会冻结 source、选择新 Target 并创建带 predecessor lineage
      的 successor；实际跨集群数据面仍受 DR readiness watermark 门禁。
- [x] 实现 Region 故障时的 Artifact/Metadata 恢复 authority：Recovery Bundle required-set、source DR domain、
      destination replicated-through watermark 和 Artifact/Checkpoint/Memory readiness 已落地；实际跨 Region
      复制与控制面切换仍是部署集成/演练门禁。
- [ ] 增加 Outbox/Queue 堆积时的自动扩容和限流策略。
- [x] 建立多集群 Reconciler Leader Election 和幂等 Apply/Delete；PostgreSQL lease 使用数据库时间、单调
      fencing token、controller-cycle advisory lock 和 mutation-transaction fence，SQLite profile 仍限制为单副本。
- [ ] 增加 Worker Pod 创建失败、ImagePullBackOff、Pending、Evicted、OOMKilled 分类。
- [x] 建立 durable Pod/Worker incarnation runtime、active/idle、requested CPU/Memory/Ephemeral Storage seconds、
      warm hit/fallback、恢复终态和端到端冷启动 P50/P95/P99 指标；requested-resource seconds 是成本代理，
      不是货币账单。
- [x] 建立 versioned 价格表、AWS/GCP/Azure 实际云账单只读导入、幂等对账和 leader-scoped scheduler；真实云
      workload identity/账单导出验收仍是部署门禁。`estimateAfterImport` 已使用显式 tenant-owned Target scope 的
      durable/idempotent Worker-fact sweeper，invoice replay 可补偿 import 后崩溃窗口。shared global tariff catalog
      的追加 API 还要求显式 platform tariff-operator Tenant，不能由普通租户 `billing.manage` 越权改写；PG/SQLite
      都在 DB 层拒绝重叠区间，PG 额外串行化并发写。新的 authoritative Worker claim ledger 已支持完整 incarnation
      的 per-period request delta；同 request ID 的 Execution/Cleanup 并发 claim 已在真实 PostgreSQL 证明只写一次
      ledger/receipt，OrbStack final5 也已现场确认 Migration 68 的表、业务唯一索引和权威触发器。shared allocation
      与历史未回填 claim ledger 的 incarnation 继续 fail closed/保持待办。
- [ ] 增加独立 Execution 排队时间、Pod provisioning 失败分类和长期滚动预聚合。
- [x] 增加 durable Generation start/Provider-ready outcome、Provider-ready duration、bounded resume decision
      和 validated runtime fallback reason 指标（Migration `000053`）；Migration `000059` 已把冷启动和恢复结果
      转为 trailing 30-day durable Generation facts，Worker incarnation 长期资源事实仍需滚动预聚合。
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
