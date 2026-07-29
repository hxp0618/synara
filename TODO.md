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
| Stage 4 | 分布式执行平台和 K8s 多集群生产化               | COMPLETE            | Stage 2、Stage 3 |
| Stage 5 | Provider 沙箱与运行时隔离加固                   | IN PROGRESS         | Stage 3、Stage 4 |
| Stage 6 | 企业 SaaS GA、运营、安全与商业化                | TODO                | Stage 2-5        |
| Stage 7 | 对外 SDK 与开发者平台                           | TODO                | Stage 2、5、6    |
| Stage 8 | 组织内协作与 Agent/人统一提及                   | TODO                | Stage 6、Stage 7 |
| Stage 9 | 开发者工作流集成与自动化                        | TODO                | Stage 4、5、8    |

Stage 2 的独立执行计划：
[`docs/plans/stage-2-go-control-plane-productionization.md`](docs/plans/stage-2-go-control-plane-productionization.md)

Stage 3 的独立执行计划：
[`docs/plans/stage-3-provider-runtime-remote-worker-productization.md`](docs/plans/stage-3-provider-runtime-remote-worker-productization.md)

Stage 7 的独立设计文档（文件名不带阶段编号，后续顺延不需要重命名）：
[`docs/plans/external-sdk-developer-platform.md`](docs/plans/external-sdk-developer-platform.md)

> 独立计划文档的抽取时机（2026-07-27 决定）：**阶段启动时抽取，启动前留在本文件**。理由是
> Roadmap-wide rules 要求"每个 Stage 开始前重新审计当前代码和计划状态"——启动前写的详细设计
> 文档会在真正动工时已经过时，反而制造"文档说的和代码不一样"的负担。Stage 7 已有文档是因为
> 其关键决策（鉴权模型、契约源、命名、免费层）已冻结且不依赖后续代码状态。Stage 5、6、8、9
> 暂留本文件，各自启动时再抽取并做差距审计。

#### 阶段间排期与并行（2026-07-27）

上表"依赖"列是**整阶段完成**的粗粒度声明。实际上除少数硬门禁外，各阶段内部的任务组可以大幅
并行——按整阶段串行推进会把工期拉长数倍且没有必要。以下是实际的排期结论。

**必须串行的硬门禁**（其余一律可并行）：

| 门禁                                          | 原因                                           |
| --------------------------------------------- | ---------------------------------------------- |
| Stage 5 沙箱修复 → Stage 6 第三方渗透验收     | 否则只是重复记录已知缺口，浪费一次外部测试预算 |
| Stage 5 沙箱修复 → Stage 7 GA                 | 公开 SDK = 任意第三方提交任意代码              |
| Stage 5 不可信输入组 → Stage 9 事件触发上线   | 攻击者开一个 Issue 即可投喂无人值守 agent      |
| Stage 8 统一 interaction → Stage 9 评论提及   | 否则 VCS 侧会长出第二套提及机制                |
| Stage 6 Plan/Entitlement → Stage 7 自助免费层 | 免费层需要配额与开通能力落地                   |

**关键路径（面向 GA）**：Stage 5 → Stage 6 → GA。Stage 7/8/9 是产品扩展，
除上表门禁外不阻塞 GA。

**建议尽早启动、不必等待前序阶段的工作**：

- Stage 5 的资源上限与云元数据阻断——无任何依赖，改动最小，且资源上限是唯一无需恶意租户即可
  触发的可靠性风险，应当立即开始。
- Stage 9 的"自动化能力回归"组——修的是本地已有而云端缺失的能力，属于回归而非新功能，拖得越久
  本地与云端分叉越深；该组不依赖 Stage 8。
- Stage 7 的 M1–M2（API 产品化与 TypeScript SDK）——只需要 Stage 2 级别的 API 稳定性。
- Stage 6 的合规认证路径——准备周期以季度计，必须尽早启动而不是 GA 前补。

**排期与优先级的区别**：Stage 编号表达依赖与主题归属，不表达优先级。若资源有限，优先级排序是
Stage 5（安全与可靠性下限）> Stage 9 自动化回归（止损产品分叉）> Stage 6（可正式售卖）>
Stage 7 > Stage 8 > Stage 9 其余。

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

状态：COMPLETE。独立执行计划：
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
- [x] 实现 `waiting-for-approval` suspend attempt、quiesce 后 Checkpoint、lease-free `suspended`、Pod 删除和
      Resolve 后新 Generation Resume；支持签名 strict-containment 与 Kubernetes Pod-terminal 两种完成证明。
      隔离 OrbStack E3 [`final2`](docs/reports/stage-4-orbstack-resource-lifecycle-20260727-final2.md) 已从空 namespace
      完成双副本/DB/MinIO/RBAC baseline，并在真实 kubelet 上验证 60 秒 Keep-alive、首代 Pod `Succeeded` 后精确
      UID 消失、suspended 期间 resolution 落盘、不同 Pod UID 的 Generation 2 恢复，以及配置/上下文/Workspace/
      Memory/Interaction/Scheduling Decision 均进入线性 Recovery Bundle；两套 namespace 与专用 RBAC 已清理。
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
      supervisor：v3 要求 systemd 254+ `DelegateSubgroup=synara-agentd`，委派根必须无进程，启动前 fd-relative
      绑定、`Pdeathsig`、generation/incarnation fence、`cgroup.kill`、`populated=0` 和 root-only 签名 gate 均保留；
      `RecoverOrphans()` 还会启用并回读 `cpu/memory/pids` controller，每个 Provider 在首条指令前必须写入并精确
      回读有限的 `pids.max`、`memory.max`、`cpu.max`，缺配置、缺 controller/interface 或 readback 漂移全部 fail
      closed。v3/probe 2 是新的服务端 exact-version fence，已签名 v1/v2 和 v3/probe 1 均不能继续授权 Suspend。
      隔离 OrbStack Ubuntu VM 的真实 systemd/cgroup 场景与自动清理现已在
      [`v3 final3`](docs/reports/stage-4-protected-cgroup-v3-live-acceptance-20260727-final3.md) 全绿通过 5/5；此前纯
      fencing/termination v2 证据保留在 [`v2 final3`](docs/reports/stage-4-protected-cgroup-v2-live-acceptance-20260726-final3.md)。OrbStack
      2.2.1 build 2020100 的 exact-ID CLI panic 只有在精确版本/commit/panic site 匹配后，才启用 owner-controlled
      `sconrpc.sock` 的单次 `ContainerDelete([capturedOpaqueId])` 兼容路径；无名称参数、无写重试，并由最终 inventory
      证明 captured ID 消失及 `debian` oracle 不变。未知版本、socket replacement、remaining ID 或 same-name
      replacement 均继续 fail closed，绝不回退名称删除；final2 还证明 marker 无法观测时不会宣告 ownership。
      SSH install/upgrade 现在保持 Target offline，直到精确 instance + operation generation 完成 fresh heartbeat、
      兼容性、当前 Manifest 与 signed containment policy 验证，并在锁定 Target/Worker 的同一事务内二次检查后
      才 active；offline bootstrap 只接受当前 install/upgrade 的 expected UID + generation，不能 Claim Execution/
      Workspace cleanup。active 重启只允许同一逻辑 Worker、UID 和持久 generation，并旋转 incarnation/token。
      Migration `000071`–`000073` 还把 revoke fence、Worker/token/lease/recovery/cleanup 撤权和 Audit/outbox 收敛到
      单个本地事务，KMS/远端失败发生在提交之后且可按同 generation 重试。OrbStack disposable SSH final4 已通过
      16/16、Target/Worker generation 等值、重启 replacement、Control Plane restart、撤权清零和 secret scan；
      timeout/旧 instance/untrusted Manifest 均保持 offline。
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
- [x] Migration `000070` 为 Kubernetes Pod 删除建立 exact target/namespace/name/UID 的 durable immutable
      fence；Register/Delete 共用 logical-identity lock，删除前检查 Execution/Cleanup lease 并 drain，fenced
      UID 的 Register/Auth/Heartbeat/Claim/reactivation fail closed，同名新 UID replacement 不受旧 fence 影响。
- [x] 将 semantic activity 接到 Generation-fenced Provider Credential 短期 access broker：Migration `000055`
      持久化单调 activity watermark 与 Worker-Lease authorization，只有显式 Grant resolve 可首次签发；agentd
      对 rotation/revocation/scope loss、hard expiry、元数据回退和续租流中断 fail closed。当前静态
      `apiKey`/`authToken` 不做 Generation 内热换，KMS/STS workload identity 仍按各云生产验证项推进。
- [x] 定义 target-local Cluster/Region、Worker Pool、Capacity Class 和 Placement Policy v1，并实现带健康、容量、
      source DR domain、复制 watermark 和 Artifact/Checkpoint/Memory readiness 的跨 Target/Region/Cluster 分组选择；
      Create Session、普通 Turn、review/compact 与 failover 都会逐候选执行 placement-aware Provider capability
      门禁，失败候选不会提前推进 Session 路由 authority，同 Target 的其他 Pool observation 也不能误放行。普通
      Create Session/Turn/review/compact 现在还会在最终 placement 前按 Tenant -> Target -> Group -> Member ->
      Location Outage -> Health -> DR Readiness 的固定顺序锁定并复核 routing authority；stale 决策原子回滚，进入
      commit phase 后不再持锁改选第二个 Target。failover 保留 hook 后的二次复核回归。
- [x] 为 tenant-owned managed Kubernetes Target 建立由 Reconciler 每次 fresh per-target 结论同步触发的
      `execution_target_health` 自动发布。Migration `000083` 又为 platform-shared/external Target 和跨域
      `execution_target_dr_readiness` 增加 Ed25519 公钥配置、精确 Target/owner/DR 域 scope、五分钟签名窗口、
      immutable nonce receipt 与 health + 多 readiness 原子提交；租户 API 不能覆盖已配置的平台 authority，
      tenant-owned Kubernetes health 也不能与 Reconciler 双写。真实 Target probe 与跨故障域复制仍由外部
      operator/integration publisher 负责；只有 operator 选择宣称物理跨域 DR 时才需要目标环境证据，不作为当前
      单站点/逻辑多 Cluster 支持的 E4 阻塞项。本地 SQLite、OrbStack PostgreSQL
      双连接以及两 Control Plane Pod 的 direct-replica/recreate replay E3 见
      [`final1`](docs/reports/stage-4-platform-routing-authority-orbstack-20260726-final1.md)。
- [x] 建立 tenant-scoped Region/Cluster evacuation authority，并让 fresh location outage 在普通路由与 lease-free
      自动 failover sweep 中都能 fail closed 地触发 `region-failover`，同时保持 source Execution placement 不可变。
- [x] 暴露 Target Group Member 的 `active -> draining -> disabled` exact-version CAS 生命周期：draining 立即退出
      新 placement 但不打断已有 Execution，drain 可显式 abort，disabled 为终态且在同 Group/Target 仍有任一
      `queued/leased/running/waiting-for-approval/recovering/suspended` Execution 时 fail closed；状态、version 和
      bounded Audit 同事务提交。真实 OrbStack PostgreSQL 双连接已证明 launch-first 阻塞后让 disable 拒绝、
      drain-first 让旧 selection stale，以及 replay/数据库不可变门禁，证据见
      [`final1`](docs/reports/stage-4-target-group-member-lifecycle-orbstack-pg-20260727-final1.md)。
- [x] 建立可重复的本地双 Kubernetes API 跨 Cluster DR 演练：以 OrbStack 为主集群、run-unique disposable Kind
      为副集群，覆盖两侧真实 Worker runtime-ready、缺失 readiness fail closed、精确 watermark 放行、唯一 successor、
      source placement 不变、Recovery Bundle 完整性、旧 Pod UID 消失和 UID-safe 清理。final4 又让两侧 projected
      token 通过生产 verifier 在真实 API Server 上执行 TokenReview + Pod GET，并拒绝未绑定 API 凭证。该 lane
      不宣称完整 Worker 注册持久化、真实跨 Region 数据复制、生产 Metadata failover 或物理独立故障域；这些是
      可选 geographic DR 集成，不阻塞当前支持边界。证据见
      [`final4`](docs/reports/stage-4-dual-cluster-dr-acceptance-20260726-final4.md)。
- [x] 定义 Tenant/Organization 对 Cluster、Region、Provider 和资源规格的允许策略。
      v1 的服务端权威、append-only revision/head、`any` 与空 allow-list、Tenant/Organization 交集及提交锁顺序已在
      [`Execution Scheduling Policy v1`](docs/contracts/execution-scheduling-policy-v1.md) 冻结；Migration `000074`、
      独立 read/manage 权限、Tenant/Organization GET/PUT API、CAS + 同事务 Audit、routed/fixed hard admission、
      policy/location commit revalidation、Execution/Recovery Bundle version+digest 快照及 PostgreSQL/SQLite 防绕过
      约束均已落地。v1 的资源规格边界是结构化 Capacity Class；任意 scheduling-template JSON 不作为授权规则。
- [x] 在现有 Target/Region、Provider/Pool correctness filtering 上增加 live capacity、配额和 Provider affinity
      的统一排序决策。
      当前 Provider soft affinity、Resume/new-operation 的 Tenant quota admission，以及 Migration `000069`
      target-local fresh-ready Warm Pool 偏好、Migration `000074` Tenant/Organization hard policy 均已进入公共
      launch coordinator。无 acknowledgement 的 Target 保留 `queue-pressure-v1` 兼容软排名；Migration `000084`
      新增 generation-scoped `exact-active-v1` acknowledgement 集合和 `reservation-aware-v1`：严格使用量为
      Pod occupancy 加未确认的 `queued/recovering` reservation，已确认 queued Pod 不再双计。Health 发布、普通/
      failover 新 Execution、恢复重入通过 Target 锁串行；PostgreSQL 两连接已证明第二个 admission 等待并在首个
      reservation 提交后拒绝。每个新 Execution 另有不可变 Capacity Admission 摘要和 bounded metrics，契约见
      [`Execution Capacity Reservation Authority v1`](docs/contracts/execution-capacity-reservation-authority-v1.md)。
      rejected routing/preview/policy/capability candidate 的完整结构化轨迹仍属于下一层解释性证据。
- [x] 建立每个新 Execution 的原子 Scheduling Decision graph：普通 Turn、review/compact 和 failover successor
      共用创建入口；routed launch 同事务冻结有序候选、routing/preview/policy/capability rejection 和 final
      post-lock Target/Member/Health/queue/DR/placement，fixed Target 保持 `selected-only`，两者都写 canonical
      candidate/set SHA-256。Event/Outbox 只携带 Decision ID、算法、完整度和摘要；Migration `000076` 对历史
      Execution 做 deterministic `legacy-selected-only` backfill，PostgreSQL/SQLite 拒绝 scope mismatch 和原地改写，
      Recovery Bundle 也冻结 Decision identity。真实 PostgreSQL candidate trace 证据见
      [`final1`](docs/reports/stage-4-complete-scheduling-candidate-trace-orbstack-pg-20260726-final1.md)，契约见
      [`Execution Scheduling Decision v1`](docs/contracts/execution-scheduling-decision-v1.md)。
- [x] 实现 Worker Pool 容量上报、可调度容量和排队时间指标。
      Reconciler 已上报 per-Pool desired/claimed/ready-idle 与 fresh/expired bounded metrics；新增
      `synara_execution_queue_depth` 和 `synara_execution_queue_oldest_age_seconds`，只按 bounded
      `target_kind/capacity_class` 聚合 durable queued/recovering 状态；Migration `000084` 又补 exact authority、
      acknowledged/unacknowledged reservation 和 strict-used bounded aggregates，均不暴露 Tenant/Target/Execution
      ID。Migration `000087` 又将 `minIdleUnits` 保证下限、bounded warm deficit 和 Target Pod budget 截断显式
      纳入同一 authority；当前源码的真实 OrbStack kubelet + PostgreSQL 验收见
      [`final1`](docs/reports/stage-4-guaranteed-warm-orbstack-pg-20260727-final1.md)。Migration `000091` 又从真实
      ResourceQuota status 发布 Pod/CPU/Memory/Ephemeral/GPU total/allocated/available 向量，并以最紧资源计算跨
      Target schedulable Pod-equivalent units；缺失/过期 status fail closed。Prometheus recording rules 对 bounded
      capacity/queue/autoscaling series 做长期预聚合，保留期由自建集群 operator 配置；PostgreSQL 只保存最新调度
      authority，完整收尾见 [`final1`](docs/reports/stage-4-queue-capacity-network-storage-20260727-final1.md)。
- [x] 冻结每 Execution Pod、常驻 Worker Pool、Warm Pool 的适用场景与 one-shot Warm Pod 安全契约。
- [x] 为交互式 Agent 建立冷启动硬上限；target-local release-aware Warm Pool、`minIdleUnits` 保证下限、
      warm deficit 告警与实测 P50/P95/P99 已落地。Migration `000090` 进一步持久化 per-Pool target delay、min/max、
      scale-up/cooldown/stabilized scale-down authority；leader controller 按 Queue Depth/oldest age 调整 effective desired
      units。交互式 hard deadline 只原子取消 exact overdue + unclaimed Execution，claimed/fresh/batch/其他 Pool 或
      Generation 不受影响，Prometheus 直接告警 violated gate。实现与验证见
      [`final1`](docs/reports/stage-4-queue-capacity-network-storage-20260727-final1.md)。
- [x] 为自动化和批处理任务建立独立 Queue/Priority/Class。Migration `000089` 冻结
      `interactive | automation | batch`、class-specific priority、Automation identity 和 immutable quota units；
      Kubernetes Pod 与 reusable Worker Claim 共用 class/priority/starvation/FIFO 排序。
- [x] 实现 Tenant、Project、Session 和 Automation 级并发与资源配额。Tenant 的 active/queued/resource-unit 上限与
      Project/Session/Automation CAS policy 在同一层级 admission transaction 组合；真实 PostgreSQL 双连接已证明
      并发创建只能有一个胜者，见 [`final1`](docs/reports/stage-4-queue-capacity-network-storage-20260727-final1.md)。
- [x] 实现公平调度，避免单个 Tenant 占满共享 Worker Pool。共享 `fairqueue` equal-share/FIFO 核心、
      Kubernetes batch Pod 选择和通用/暖池 Worker Claim 已接入；Claim 在 Target lock 下先比较 Tenant 的
      `leased/running/waiting-for-approval` service units，再保留 Tenant 内 FIFO，真实 PostgreSQL 已证明 idle
      Tenant 会先于已有 active unit 的 Tenant。Migration `000088` 又为 reusable `general-pool` 增加默认
      `pinned` / 显式 `shared`：前者首个 Execution 或 Workspace-cleanup Claim 原子绑定 Tenant，后续 Claim、
      Heartbeat 与重注册均不可跨 Tenant；后者才保留 equal-share 跨 Tenant 复用。真实 OrbStack 两 Pod +
      PostgreSQL 证据见
      [`final1`](docs/reports/stage-4-worker-pool-tenant-isolation-orbstack-pg-20260727-final1.md)。Migration `000089` 的
      bounded starvation-age promotion 防止低优先级长期饥饿；真实 PostgreSQL fair-share gate 与 OrbStack + Kind
      每集群 8 Execution/峰值 4 Pod 压力、3 轮 UID chaos 见
      [`dual-cluster final7`](docs/reports/stage-4-dual-cluster-pressure-chaos-20260727-final7.md)。
- [x] 支持 Kubernetes non-preempting Priority，并明确拒绝抢占：Pool 写入和 Pod 构建只接受
      `preemptionPolicy=Never`；默认 `synara-worker-nonpreempting-v1` 与自定义 PriorityClass 都必须由 Target
      Credential 读取真实集群对象后才允许 Pod apply，缺类、RBAC 不足、默认类漂移或实际
      `PreemptLowerPriority` 均 fail closed。Pod-spec revision 会淘汰旧的隐式默认策略容量，Kustomize 提供默认类
      和只读 `get priorityclasses` RBAC。真实 OrbStack API/kubelet 已验证 cold/warm Running Pod 的
      priority/value/`Never`，并证明 Pod 无法覆盖可抢占类以及该类会在 apply 前拒绝；证据见
      [`final1`](docs/reports/stage-4-kubernetes-nonpreempting-priority-orbstack-20260727-final1.md)。Kubernetes
      PriorityClass 不承担 Automation/Batch Queue priority 或 Tenant fairness；这些由上面的服务端 Queue/Class 与
      starvation promotion 独立处理。
- [x] 完成自建 K8s Namespace、ServiceAccount、NetworkPolicy、ResourceQuota 和 Pod Security 验收资产与本地承重验证（
      本地
      OrbStack real-kubelet [`final14`](docs/reports/stage-4-orbstack-resilience-acceptance-20260726-final14.md) 已运行
      schema 73 镜像，并通过 RBAC/Lease Guard v2 Leader takeover/Control Plane failover 3/3；disposable Kind
      [`final5`](docs/reports/stage-4-kind-resilience-acceptance-20260726-final5.md) 4 节点 6/6 场景、20 秒 partition、
      实际 601 秒/10 周期 soak 也已通过；OrbStack + disposable Kind 双真实 API 的 DR + Pod-bound TokenReview/
      Pod GET [`final4`](docs/reports/stage-4-dual-cluster-dr-acceptance-20260726-final4.md) 也已通过。非 Kind
      node-partition 另有严格 `systemd-user` controller 和宿主/Linux fake 矩阵，可由自建集群 operator 接入物理网络
      隔离工具。namespace/RBAC 隔离 runner 已在当前 schema 87 dirty-verification 镜像上完成 OrbStack
      Pod/DB/MinIO baseline、3/3 顶层场景和 120 秒 8/8 disruption soak，全部 readiness failure 为 0，且原
      `synara-system` 保持 2/2，最新证据见
      [`schema87-final1`](docs/reports/stage-4-orbstack-isolated-schema87-20260727-final1.md)。正式边界仅承诺自建
      Kubernetes；operator 在目标硬件执行同一 runner 属于部署验收，不再要求托管云多可用区或云负载均衡证据。
- [x] 将 Kubernetes Worker Registration Secret 改为 10 分钟自动轮换、Pod-bound、target-scoped audience 的
      ServiceAccount token；Control Plane 通过 TokenReview + 实时 Pod GET 校验 SA、Pod name/UID 和 ownership，
      同一物理 Pod UID 只能注册一次，terminating Pod 会拒绝注册，durable deletion fence 会阻止已决定删除的
      exact UID 重新接活。Local/SSH/Docker 仍保留独立 shared-token 路径。
- [x] 冻结云厂商集成边界：Stage 4 暂时只正式支持自建 Kubernetes，不支持或宣传 EKS/GKE/AKS 专属
      Workload Identity、AWS/GCP/Azure 原生账单 Export、云 KMS/IAM fallback 或跨云 E4。未来若商业需求成立，再按
      [`Managed-cloud billing acceptance v1`](docs/contracts/managed-cloud-billing-acceptance-v1.md) 重新启用独立项目；
      该 deferred contract 当前不阻塞 Stage 4。
- [x] 建立 Provider API Egress Allowlist 和 Tenant 自定义网络策略。自建 K8s Target 冻结 CIDR + TCP port allowlist、
      private-network subset、credential-free HTTP(S)/SOCKS5 proxy 和 bounded no-proxy；NetworkPolicy 只向
      `kube-system/kube-dns` 开 TCP/UDP 53，业务出网只走显式端口，并从 broad CIDR subtract link-local/metadata。
      选择器、端口、代理投影与 unsafe CIDR/credential URL 负向测试见
      [`final1`](docs/reports/stage-4-queue-capacity-network-storage-20260727-final1.md)。
- [x] 支持私有 Git、企业代理、私有镜像仓库和内部 Package Registry。私有 Git 默认关闭，只允许 egress 覆盖的
      RFC1918/CGNAT/ULA exact policy；镜像继续使用 exact-registry `worker_image_pull` Secret；npm/PyPI
      `package_read` Grant 生成 execution-local 0600 config 并进入 SecretGuard，只经 generation-fenced absolute-path
      alias 到 Provider Host，ambient package/proxy env 不继承。v1 不向 Provider 自动注入 publish Credential。
- [x] 设计 Workspace Storage：Ephemeral Disk、PVC、Snapshot、Object Storage 和 Git 的边界。正式契约见
      [`Workspace Storage Boundary v1`](docs/contracts/workspace-storage-boundary-v1.md)：live Workspace 是 size-bounded
      `emptyDir`，PVC 仅为 rebuildable Git cache，CSI Snapshot 不进入 RecoveryBundle authority；跨 Pod/Target 恢复只
      接受 Ready Git-reference/Patch/Snapshot Checkpoint 与已验证 Object Storage Artifact。未知 live-Workspace PVC
      配置会 fail closed。
- [x] 实现 Worker/Pod Drain、滚动升级、灰度发布和自动回滚：Stage 3 已完成 immutable Release
      canary/promote/rollback 与自动回滚；Kubernetes 使用 exact Pod UID 删除 fence 并在 Execution/Cleanup Lease
      清零后删除；Migration `000086` 为 Managed Docker 增加服务端持久化 exact-incarnation Drain、每 Target
      单一 in-flight 与每轮单 Worker 推进、replacement readiness 门禁及 crash recovery。真实 OrbStack Docker
      与 PostgreSQL 并发验收见
      [`final1`](docs/reports/stage-4-managed-docker-rolling-drain-orbstack-pg-20260727-final1.md)；生产 clean-SHA
      镜像、Registry 签名和自建集群 rollout 仍按发布门禁执行，不回退本机制完成结论。
- [x] 实现 Cluster 不可用时的 Execution 停止、重排和恢复 authority：过期 Target health、tenant-scoped
      Region/Cluster evacuation 与 lease-free failover 会冻结 source、选择新 Target 并创建带 predecessor lineage
      的 successor；本地 OrbStack → Kind 已验证两侧真实 runtime-ready、单 successor 和旧源 Pod UID 消失，
      实际跨集群数据面仍受 DR readiness watermark 门禁。
- [x] 实现 Region 故障时的 Artifact/Metadata 恢复 authority：Recovery Bundle required-set、source DR domain、
      destination replicated-through watermark 和 Artifact/Checkpoint/Memory readiness 已落地；实际跨 Region
      复制、successor 运行时消费和生产 Metadata 控制面切换仍是部署集成/演练门禁。
- [x] 增加 Outbox/Queue 堆积时的自动扩容和限流策略。Migration `000092` 持久化 depth/oldest-age pressure、adaptive
      batch/concurrency 与 hard throttle；同 `message_key` 保序，只限流新的 `execution.queued`，Recovery/terminal/
      cancel/cleanup 永不被该阈值阻断。Queue 侧由 Migration `000090` Pool autoscaler 和 hierarchical queued quota
      处理。PostgreSQL multi-active claim、SQLite pressure/throttle 与 bounded metrics 均通过，见
      [`final1`](docs/reports/stage-4-queue-capacity-network-storage-20260727-final1.md)。
- [x] 建立多集群 Reconciler Leader Election 和幂等 Apply/Delete；PostgreSQL lease 使用数据库时间、单调
      fencing token、controller-cycle advisory lock 和 mutation-transaction fence，SQLite profile 仍限制为单副本。
- [x] 增加 Worker Pod 创建失败、ImagePullBackOff、Pending、Evicted、OOMKilled 分类：Migration `000078`
      将首次 Apply、首次 Pending、首次 Running 与最后观测时间固化到 durable Generation fact；apply failure、
      pending timeout、unschedulable、image pull、container start、evicted、OOM killed 和 generic failed 各自保留
      一条不可删除的低基数事实，后续成功 Pod 不会覆盖历史失败。注册后的 terminal Worker 也会在安全删除前
      立即按 Evicted/OOMKilled 精确关闭 incarnation，而不是等待 Lease 超时。本地 OrbStack API 已产生真实
      Unschedulable/ImagePullBackOff/OOMKilled，并用隔离 Pod status subresource 验证 Evicted 解析；未使用 node drain。
      PostgreSQL/运行时证据见 [`final1`](docs/reports/stage-4-kubernetes-pod-failure-orbstack-pg-20260726-final1.md)。
- [x] 建立 durable Pod/Worker incarnation runtime、active/idle、requested CPU/Memory/Ephemeral Storage seconds、
      warm hit/fallback、恢复终态和端到端冷启动 P50/P95/P99 指标；requested-resource seconds 是成本代理，
      不是货币账单。
- [x] 建立自建环境的 operator-managed versioned 价格表、requested-resource 计量、幂等估算/对账和
      leader-scoped scheduler；本地
      PostgreSQL 17.10 + versioned MinIO final19 已通过精确旧 VersionId、AWS CUR2 native-shaped manifest/
      multi-chunk CSV/GZIP 导入、两段 tariff estimate、对账、scheduler audit、重启 replay 和双连接并发首次导入
      串行化；CPU/Memory split child 只有在同边界唯一 parent 与合计金额都可证明时才替换 parent，缺 parent/chunk、
      重复 parent、net/gross 混合或错误 adjustment 符号整单 fail closed。相同 checksum 返回同一持久化 identity，
      不同 checksum 稳定冲突。当前正式产品路径只使用 operator 配置费率与内部资源事实；provider-shaped parser、
      S3/GCS/Azure Blob adapter 和 normalized invoice import 保留为内部兼容/测试面，不作为受支持的云账单连接器，
      也不再要求真实云 Workload Identity 或原生 Export 门禁。
      provider 与 parser format 现已强绑定并在配置加载期 fail closed，错误云归属不会进入对象读取或导入。
      `estimateAfterImport` 已使用显式 tenant-owned Target scope 的
      durable/idempotent Worker-fact sweeper，invoice replay 可补偿 import 后崩溃窗口。shared global tariff catalog
      的追加 API 还要求显式 platform tariff-operator Tenant，不能由普通租户 `billing.manage` 越权改写；PG/SQLite
      都在 DB 层拒绝重叠区间，PG 额外串行化并发写。新的 authoritative Worker claim ledger 已支持完整 incarnation
      的 per-period request delta；同 request ID 的 Execution/Cleanup 并发 claim 已在真实 PostgreSQL 证明只写一次
      ledger/receipt，OrbStack final5 也已现场确认 Migration 68 的表、业务唯一索引和权威触发器。Migration `000075`
      新增 1:1 append-only Claim Release Ledger，Completion/Failure/Release/Expiry/Cancel/Interaction/Suspend/
      Control/Revoke 和 Workspace Cleanup 全释放路径均同事务双写；真实 OrbStack PostgreSQL 已验证合法/非法 scope、
      immutable、exact replay 及 Completion 删除 Lease + 唯一 ReleaseFact。第一阶段故意不启用 deletion guard；
      migration-era 缺 claim fact 的存量必须先 backfill 并完成 minimum-writer-version gate。Migration `000077` 与
      `closed-claim-interval-v1` 现已对 sealed cutover 后、terminal、完整 Claim/Release 的 shared Worker 将活跃区间
      分给 exact Tenant、空闲区间保留为 platform cost，并以 deterministic Run/Slice、ledger digest、累计舍入和
      PostgreSQL scope advisory lock 保证守恒/并发重放，并拒绝重叠账期；SQLite 单副本也串行化首次写。review 后又
      补齐 priced resource 缺失 fail closed、terminal-only 右边界、隐藏 fallback boundary 合并、Target scope 更新
      保护、semantic Slice 唯一、Tariff/资源快照绑定和 exact-region precedence。OrbStack PostgreSQL
      [`final4`](docs/reports/stage-4-shared-cost-allocation-orbstack-pg-20260726-final4.md) 已通过两 Tenant、两段 tariff、
      终止点=tariff end Request、并发首次写及四条 DB negative gate。platform billing operator 现可通过原子 API
      封存 Coverage，并对显式闭合账期执行 replay-safe shared sweep；单 Worker 失败不回滚其他持久化 Run/Slice，
      返回 `retry-required` 和 bounded failure，重复调用只补偿失败项；OrbStack PostgreSQL
      [`final2`](docs/reports/stage-4-shared-cost-management-orbstack-pg-20260726-final2.md) 已验证并发 seal 只有一条
      Coverage/审计、授权 sweep audit 与同一 1 Run/27 Slice replay。随后 static closed-period runtime mapping、
      settlement delay、独立 `synara:billing-shared-allocation-scheduler` lease 和 transaction fence 已接通；OrbStack
      PostgreSQL [`final3`](docs/reports/stage-4-shared-cost-scheduler-orbstack-pg-20260726-final3.md) 证明 standby 不执行、
      handoff fencing token=2、两个 epoch 各一条 system audit 且仍只有 1 Run/27 Slice。Migration `000080` 又加入
      显式 `monthly-utc` anchor/可选终止边界和 per-period durable due/claim/outcome state；静态精确账期保持兼容，
      同一 Target/provider/currency 禁止混用日历与其他 mapping。重启和 Leader handoff 不再依赖进程内 throttle，
      PostgreSQL 两副本同时抢同一月账期只有一个执行，证据见
      [`final1`](docs/reports/stage-4-billing-calendar-scheduler-orbstack-pg-20260726-final1.md)。历史不完整 incarnation
      仍 fail closed。Migration `000082` 已新增 operator-owned account invoice 到 shared Target 的独立实际分摊图：
      exact provider/currency/period/resource/kind 才能入图，跨 Target 歧义和零 estimate basis fail closed，signed
      micros 以累计整数比例逐 line 守恒，未匹配 line/amount 保持显式。Run 只有完整图才能原子 `building -> sealed`，
      invoice line 与 estimate slice 均只能使用一次，晚到 source/estimate 被数据库 fence；并发首次写只保留一份图
      和一条审计。本地 SQLite + OrbStack PostgreSQL 17.10 证据见
      [`final1`](docs/reports/stage-4-shared-actual-allocation-orbstack-pg-20260726-final1.md)；真实 provider account scope、
      export settlement 和 workload identity 不在当前支持范围；该实际分摊图仅供未来 operator-provided normalized
      invoice 能力复用，不构成 AWS/GCP/Azure 产品声明。
- [x] 增加独立 Execution 排队时间和 Pod provisioning 时间/失败分类；trailing-30-day 指标分别提供
      dispatch-to-first-apply、first-apply-to-first-Running P50/P95/P99，以及每种 failure class 的 Generation 数。
- [x] 为长期保留的 terminal Worker incarnation 增加精确日级滚动预聚合：Migration `000079` 以不可删除
      membership/completion entry 保证每条 terminal fact 最多汇入一次，并在同一 PostgreSQL transaction advisory
      lock 事务内递增 bucket、完成 entry。scrape 在一致快照内合并 rollup、尚未处理的 terminal raw fact 和
      nonterminal raw fact；scheduler 延迟只增加 pending backlog，不会少算或重复计数。本地 OrbStack PostgreSQL
      已验证历史回填、新增触发、回滚、重放、非有限值拒绝、session lock 和 transaction lock 接管，证据见
      [`final1`](docs/reports/stage-4-worker-metric-rollup-orbstack-pg-20260726-final1.md)。
- [x] 为 trailing-30-day Generation 冷启动、排队、Pod provisioning、outcome 和 Pod failure 增加可合并的
      滚动预聚合：Migration `000081` 只在 terminal Generation source 封存后建立 append-only membership，Pod
      failure 则按 immutable first proof 独立入队；固定整数直方图以不超过 2% 相对 bucket width 合并 duration，
      不对 P50/P95/P99 本身求和。scrape 仅合并窗口内完整 UTC 日 bucket，并从 raw fact 精确读取窗口首尾两个
      partial day、nonterminal 与 pending-rollup tail，因此 scheduler 延迟和日边界都不会造成漏计/重计。
      本地 OrbStack PostgreSQL 双连接并发、迁移回放和 direct DB negative gate 证据见
      [`final1`](docs/reports/stage-4-generation-metric-rollup-orbstack-pg-20260726-final1.md)。
- [x] 增加 durable Generation start/Provider-ready outcome、Provider-ready duration、bounded resume decision
      和 validated runtime fallback reason 指标（Migration `000053`）；Migration `000059` 已把冷启动和恢复结果
      转为 trailing 30-day durable Generation facts；Worker incarnation terminal history 已由 Migration `000079`
      转为精确日级 rollup，Generation 的可合并分位数预聚合已由 Migration `000081` 完成。
- [x] 执行多副本 Control Plane、多个 K8s Cluster 的压力和混沌测试。本地单 Cluster 双副本现可在独立
      namespace/RBAC 中重复执行；schema 87 OrbStack 已完成基线 Pod/DB/MinIO 故障、3/3 顶层验收和 120 秒
      8/8 Leader/Pod disruption soak，所有 readiness failure 为 0；schema 83 又完成签名 routing authority 在
      两个 direct Control Plane Pod、删除后 survivor 与 replacement 上的 exact receipt replay。新增 OrbStack +
      disposable Kind、两个 Control Plane service/Reconciler 共享 PostgreSQL 的 190 秒压力/混沌验收：每集群 8
      Execution、峰值 4 Pod、并发 failover 单 successor、3 轮双集群 exact Pod UID 删除/不同 UID 重建、16 个
      terminal boundary、全部 cleanup/secret/context gate 通过，见
      [`final7`](docs/reports/stage-4-dual-cluster-pressure-chaos-20260727-final7.md)。这是自建本地有界证据，不宣称
      geographic RPO/RTO、生产时长或多节点 failure-domain。
- [x] 编写 Cluster 接入、升级、下线、Credential 轮换和事故处理
      [`Runbook`](docs/runbooks/kubernetes-cluster-lifecycle.md)。文档只使用当前公开权威 API，并明确区分
      `routing-offboarded` 与物理 `target-decommissioned`。tenant-owned managed Kubernetes Target 现有终态
      `POST .../kubernetes/disable`：与完整 Reconciler cycle 共用 advisory lock，并在同一 Target transaction 内对
      Member、固定 Session、Execution、Pool、Workspace、Worker/Lease 和 fresh exact-zero managed health 全部
      fail closed，成功后 Reconciler 不再维护该 Target且只写一次 Audit。历史 Target 行保持 disabled 不删除；固定
      Session 迁移和 encrypted configuration 原地轮换仍无 API，分别要求归档/保持 blocked 与蓝绿替换，物理
      Namespace 清理必须在 disable 后使用 UID precondition。真实 OrbStack Kubernetes + PostgreSQL、advisory-lock
      与并发 Member insert 证据见 [`final1`](docs/reports/stage-4-managed-kubernetes-target-disable-orbstack-20260727-final1.md)。

#### 完成条件

- [x] 单个 Cluster 或 Control Plane Pod 故障不会丢失权威 Session 状态：OrbStack 双副本 Control Plane
      删除当前 Pod 后由 survivor 与 replacement 继续服务，故障前后唯一 `agent_sessions` 权威行的完整 JSON
      SHA-256 保持字节级一致，且重复写入/歧义结果只允许按精确 Session ID 只读调和。证据见
      [`final3`](docs/reports/stage-4-orbstack-session-authority-failover-20260726-final3.md)。该证据覆盖当前正式支持的
      自建单站点 Kubernetes Control Plane Pod 故障；整 Cluster 物理失效由上面的 logical cross-Cluster
      successor authority 与 operator 部署演练覆盖，不宣称地理级数据库 RPO/RTO。
- [x] 调度决策可解释、可审计、可重放。Migration `000076` 对 routed launch 冻结 complete candidate trace、
      losing/rejected reason、final post-lock winner 和 canonical digest，fixed/historical 分别显式保留
      `selected-only` / `legacy-selected-only`；Recovery Bundle 冻结 Decision identity。真实 PostgreSQL
      [`final1`](docs/reports/stage-4-complete-scheduling-candidate-trace-orbstack-pg-20260726-final1.md) 已验证完整轨迹、
      replay 和数据库不可变门禁。
- [x] Shared 和 Dedicated Worker Pool 均有隔离与容量验证：Migration `000088` 将 `tenantIsolation` 冻结为
      `pinned | shared`，默认 pinned Worker 在首个 Claim 原子绑定 Tenant，显式 shared Worker 可按 fairqueue
      轮转；Execution 与 Workspace cleanup 共用同一 fence，SQLite/PostgreSQL 拒绝清空、换绑、跨 Target 或
      非 general Worker 绑定。真实 OrbStack Kubernetes 两个物理 Pod 分别完成 pinned 同 Tenant 两次容量与
      shared 跨 Tenant 两次容量，Pod UID/Worker/Lease/Execution 全部对齐，见
      [`final1`](docs/reports/stage-4-worker-pool-tenant-isolation-orbstack-pg-20260727-final1.md)。
- [x] Worker Pool 可以根据 Queue Depth 和目标延迟安全扩缩容。Migration `000090` 的服务端 policy/state、
      leader-only sweep、min/max clamp、immediate scale-up、cooldown/stabilized scale-down、effective desired warm
      capacity 和 hard cold-start gate 均已接通；API、metrics、Deployment env 与 focused tests 见
      [`final1`](docs/reports/stage-4-queue-capacity-network-storage-20260727-final1.md)。
- [x] 滚动升级期间已运行 Execution 不被错误重复执行：Worker Drain 与 Claim 在同一 Worker row lock 下串行化，
      已持有 Lease 的旧 incarnation 可继续 renew/complete/release，新 Claim 被服务端 fence；真实 PostgreSQL
      Claim/Drain 双连接竞态与 OrbStack 两 Worker 逐个替换均已通过，Stage 3 bounded load 另覆盖 release-pinned
      Execution 不静默改绑。
- [x] 本地双 Kubernetes API 的跨 Cluster control-path 恢复有可重复演练记录。
- [x] 冻结当前 DR 支持声明：支持自建 Kubernetes 的 logical cross-Cluster 路由、readiness 门禁和 successor
      authority；不宣称地理级 RPO/RTO。真实跨站点 Metadata、Artifact/Checkpoint/Memory 复制与 successor 消费
      仅在 operator 需要 geographic DR 时作为可选部署项目，不阻塞当前 Stage 4。
- [x] Personal、SSH、Docker 单机部署没有因 K8s 调度模型产生回归。当前源码的 Personal bootstrap/export-import/
      config、SSH provision/revoke/readiness 和 Docker reconcile/drain/claim focused matrix 全绿；当前脏工作树镜像又在
      OrbStack Docker 29.4.0 完成两 Worker 逐个替换并稳定收敛（52.82 秒，`activeDrains=0/currentWorkers=2/
terminatedFacts=2`），完整边界见
      [`final1`](docs/reports/stage-4-single-node-regression-20260727-final1.md)。

### Stage 5：Provider 沙箱与运行时隔离加固

状态：DONE。短板审计日期 2026-07-26；2026-07-28 启动重审与实现切片见
[`Stage 5 独立计划`](docs/plans/stage-5-provider-sandbox-runtime-isolation.md)。
完成边界与逐项证据见
[`Stage 5 完成边界`](docs/reports/stage-5-completion-boundary-20260729.md)。Stage 5 的 Kubernetes
结论严格限定为 Stage 4 已冻结的正式支持面——自建 Kubernetes；EKS/GKE/AKS 专属 IAM/CNI/Region
集成仍是 deferred 产品项目，不反向成为本阶段完成前提。每个新自建 Target 上线时仍必须在其完整
Ready/非 cordon Worker 集合重跑同一逐节点 Runner，单次验收不会自动授权其他集群。

审计更新（2026-07-27）：[protected cgroup supervisor v3](docs/contracts/agentd-protected-cgroup-supervisor-v3.md)
已从纯**围栏（fencing）与可靠终止（termination）**扩展到有限的**资源约束（resource confinement）**：
Provider 子树强制 `pids.max`、`memory.max`、`cpu.max`，并以 `DelegateSubgroup`、空委派父节点、controller/limit
精确 readback 和 v3/probe 2 attestation fail closed。它仍不是完整沙箱：文件系统、设备、syscall 和网络隔离仍由
外层容器/VM 承担；protected 模式仍是 Linux SSH/local 专属，Docker、Kubernetes、macOS 不继承这层保证。

Provider CLI 自带沙箱是主动关闭的（`apps/provider-host/src/codexAppServerRuntime.ts` 的
`sandbox: "danger-full-access"`；Claude 仍允许普通工具，但保持 host permission callback 以拦截敏感动作），
代码注释已说明理由是"容器才是隔离边界，且标准容器内 bubblewrap 无法创建 user namespace"。
该选择本身成立，但它把"容器边界必须足够强"变成硬前提。
本阶段负责让这个前提在每种 Target 上真正成立，或显式声明该 Target 不是多租户面。

本阶段覆盖**两类不可信输入**，它们的威胁模型不同但缺一不可：

- **不可信代码**——Provider 在 workspace 中执行任意程序。对策是进程、资源与网络隔离（下方第一组）。
- **不可信文本**——Provider 读取的 README、代码注释、依赖描述、Issue/PR 正文与评论、工具输出、
  外部 MCP 返回值，全部是攻击者可控内容，可诱导 agent 执行非预期动作（prompt injection）。
  2026-07-27 启动核查时全仓库不存在相关防护；当前已冻结威胁模型并接入部分来源、凭证/能力边界、
  敏感动作 classifier 与 audit alert，剩余 adapter 缺口列在下方第二组。

第二类是 agent 产品特有的、当前业界最主要的实际攻击面，且**Stage 9 会显著放大它**——事件触发的
自动化意味着攻击者只要开一个 Issue 就能把文本直接投喂给无人值守的 agent。因此第二组必须在
Stage 9 上线前完成，不能延后。

本阶段排在企业 GA 之前：下列缺口正是 Stage 6"跨 Tenant 越权、SSRF、容器逃逸测试"必然命中的
问题，在 GA 前修复远比在 GA 后修复便宜。

#### 目标

- 单个 Execution 不能因资源耗尽影响同宿主的其他 Execution 或 Worker 本身。
- 共享 Worker 上的跨租户机密性不依赖路径不可猜测性。
- 每种 Execution Target 的隔离强度有明确声明，弱隔离 Target 不被当作多租户面使用。
- 出网边界不因 operator 配置疏漏而静默失效。
- 被注入的 agent 即使完全听从攻击者指令，也无法造成不可逆或不可见的损害。

#### 交付顺序

风险与成本差异很大，建议按此顺序而非并行铺开：

1. **资源上限**（`pids.max` 优先）——唯一无需恶意租户即可触发的可靠性风险，改动最小，先做。
2. **云元数据阻断与 DNS 收窄**——一次配置疏漏即等于云账号凭证失窃，改动同样很小。
3. **跨租户残留（租户绑定）**——最重的机密性问题，但涉及调度语义变更，需要设计。
4. **敏感动作闸门与不可信内容标注**——Stage 9 的前置条件。
5. **Docker 定位决策、隔离矩阵契约、microVM 论证**——方向性决策，可与上述并行推进。

#### TODO — 进程、资源与网络隔离

- [x] 为 Provider cgroup 写入实际资源上限：`pids.max`（优先级最高，防 fork 炸弹）、`memory.max`、
      `cpu.max`。v3 使用 systemd `DelegateSubgroup=synara-agentd` 满足 no-internal-process 规则，在 held parent 与
      每个 bundle 的 `cgroup.subtree_control` 启用并回读 `cpu memory pids`，再于 Provider 启动前写入/回读三项
      有限值；配置四元组必须完整，内核拒绝或接口缺失不会回退无上限。Linux 单测、SSH gate 与 disposable
      OrbStack systemd 255/cgroup-v2 实机证据见
      [`final3`](docs/reports/stage-4-protected-cgroup-v3-live-acceptance-20260727-final3.md)。
- [x] 将 Kubernetes Pod 的 CPU/Memory/EphemeralStorage limits 与节点级 PID 上限从可选/隐式改为必填：
      `kubernetes_pod_spec.go` 中每个字段均为 `if value != ""` 可选，`kubernetes_reconciler.go`
      现在在配置归一化时要求 `cpuLimit`、`memoryLimit`、`ephemeralStorageLimit` 与 `pidsLimit` 全部
      存在。Reconciler 在任何 Kubernetes API mutation 前读取 Target `nodeSelector` 下全部节点的 kubelet
      `podPidsLimit`，Worker 注册再复验实际 `spec.nodeName`；`-1`、`0`、超出 Target 上限、无匹配节点或
      无法读取均 fail closed。execution/warm Pod 共用该入口，受影响 Go package tests 已通过。
- [x] 完成 general_pool Worker 跨租户残留的纵深清理。Migration `000088` 已完成核心机密性 fence：
      `tenantIsolation: pinned | shared` 不可变，多租户默认 pinned，首个 Execution/Workspace-cleanup Claim
      在 Worker row lock 下原子绑定 Tenant，候选过滤、Heartbeat、重注册及 PostgreSQL/SQLite 触发器均
      禁止跨租户、清空或换绑；显式 shared 才允许跨 Tenant，真实 OrbStack + PostgreSQL 证据见
      [`final1`](docs/reports/stage-4-worker-pool-tenant-isolation-orbstack-pg-20260727-final1.md)。
      仓库内 scrub-on-release 已由 migration `000094`、控制面 generation/receipt fence、agentd 幂等
      workspace/git-cache/private-`/tmp` 物理清理、反向 ownership 修复及失败 drain 落地；scrub receipt
      未确认前 Execution/Workspace-cleanup Claim 均 fail closed。该实现是纵深防御，不能取代一次写对的
      pinned 不变式。OrbStack 同一物理 Worker 已完成真实控制面 A → receipt → B fence；补充组合实测
      由真实 agentd 依次运行两个 Provider Host Protocol v2 fixture 进程，A 在 workspace v2/v3、legacy、
      git cache、quarantine 与 Worker-private `/tmp` 写入 6 处 marker，agentd scrub/receipt 后 B Provider
      扫描 27 个路径且残留/可读 marker 均为 0。证据见
      [`stage-5-agentd-provider-tenant-isolation-local-acceptance-20260728.md`](docs/reports/stage-5-agentd-provider-tenant-isolation-local-acceptance-20260728.md)。
- [x] 为生成的 Kubernetes NetworkPolicy 增加云元数据端点阻断：Target 配置现在拒绝直接声明
      link-local/metadata CIDR；对 `0.0.0.0/0` 等宽范围生成 `ipBlock.except`，至少扣除
      `169.254.0.0/16`、`100.100.100.200/32`、`fe80::/10` 和 `fd00:ec2::254/128`。单测覆盖宽范围扣除与
      直接 metadata CIDR 的 fail-closed。这里完成的是配置/清单门禁；从真实 Provider 进程发起请求的
      负向实测 Runner 已接入真实 Control Plane → agentd → Provider Host → Codex/Claude 路径，并可用
      `--kubernetes-node-name` 精确钉住/回读 Worker；托管 Provider × Node 矩阵仍保留在下方 Stage 5
      完成条件，不以代码或 YAML 检查冒充运行期隔离证明。
- [x] 收窄 NetworkPolicy 的 DNS 规则：生成规则现在同时使用 `kube-system` Namespace selector 与
      `k8s-app=kube-dns` Pod selector，仅开放 UDP/TCP 53；单测冻结 selector、协议和端口。operator 仍需在
      目标发行版采用不同 DNS 标签时显式适配并复跑验收，不能回退为任意解析器。
- [x] 决定 Docker Target 的产品定位并执行。当前无 `CapDrop`、无 `SecurityOpt`
      （no-new-privileges/seccomp 均未请求）、无 `ReadonlyRootfs`、无 `PidsLimit`，内存/CPU 可选，
      user 可被 operator 改为 root，bridge 网络全互联网出网无 allowlist，且同一 Target 的每个
      容器槽位共用同一命名卷；protected cgroup 模式在 Docker 上是契约明确非目标，容器内仅有
      `Setpgid` + `Pdeathsig`，`setsid()` 后代可逃逸。二选一：补齐到 Kubernetes 平价，或在文档与
      产品面显式声明为个人/开发用途、非多租户面（推荐后者，把加固预算集中在 Kubernetes 路径）。
      已执行推荐路径：Docker 返回 `single-tenant-trusted-v1`，契约明确其缺口；平台共享 Docker 不会
      进入 Target 列表、会话选择、路由或 Worker 注册。
- [x] 声明并收敛 SSH/local 非 protected 回退路径的隔离等级：`CgroupV2Root` 为空时仅
      `Setpgid` + `Pdeathsig`，`setsid()` 后代可逃逸；配置了 cgroup root 但未配 provider 身份时
      Provider 与 agentd 同 UID 且无资源上限；macOS 仅 `Setpgid`，无任何凭证隔离与 `Pdeathsig`。
      这些路径统一声明为 `single-tenant-trusted-v1`，契约分别列出 protected、fallback、macOS 能力；
      非个人部署的内置 `platform-local` 会被 bootstrap 禁用，平台共享 local/SSH 同样从执行入口排除。
- [x] 按隔离与延迟双轴重新论证 microVM（`snapshot-restore`）层：
      [fast-provision 提案](docs/plans/fast-provision-runtime-proposal-v0.md)目前只按延迟立项，
      但它同时是"托管路径缺乏真实内核边界"的答案；两条论证合并后 ROI 与单看延迟不同，其开放
      问题"microVM 落在何处"已冻结为 Kubernetes 控制面 + 专用 Linux/KVM node pool 上由
      `sandbox-operator` 管理的自管 Firecracker；agentd/credential broker 位于 guest 外，Provider/tool
      位于 guest 内，fenced vsock 通信。Kata 仅保留为兼容性 spike，不作为 snapshot tier 权威。
- [x] 将每种 Execution Target 的隔离能力矩阵**并入既有
      [`docs/contracts/execution-target-v1.md`](docs/contracts/execution-target-v1.md)，不新建独立
      文档**（2026-07-27 决定）。该契约已按 Kind 组织（`Kinds`、`Managed SSH lifecycle`、
      `Managed Docker Worker Pool`、`Managed Kubernetes execution`），隔离等级本就是 Target Kind
      的属性；拆成两份会让"新增 Target 必须声明隔离等级"这条 Roadmap 规则更容易被漏掉，也会
      造成新增 Kind 时需同步两处。API 现返回派生的 `isolationProfile`、`platformSharedEligible` 与
      `productBoundary`，不能由 Target `capabilities` 自行升级。
- [x] 为 Provider 自带沙箱关闭（Codex `danger-full-access`；Claude 保留 host permission callback）的前提条件加自动化
      测试：agentd 丢弃 ambient profile 后注入自身派生值，Provider Host 在启动 Provider 前拒绝缺失
      或未知值；`kubernetes-restricted-v1` 还要求控制面读取 live Pod，并校验实际 security context、
      host namespace、容器/卷、projected token、private `/tmp`、三类 limits 与 PID 声明，削弱即
      `kubernetes_workload_identity_outer_sandbox_invalid`，不允许静默降级。
- [x] 评估 spawn 期进程加固的可行增量：当前 Go 侧 spawn 路径不存在 seccomp、`no_new_privs`、
      capability drop、`Setrlimit`、namespace 或 landlock。Kubernetes 已由 Pod securityContext
      覆盖大部分（`drop: ALL`、`RuntimeDefault` seccomp、`allowPrivilegeEscalation: false`、
      `readOnlyRootFilesystem`）。结论是不为 SSH/local 增加容易被误读为完整沙箱的零散 flag：protected
      cgroup 继续只承诺资源/终止围栏，SSH/local 保持 `single-tenant-trusted-v1` 并退出平台共享面；需要
      文件系统/syscall/device 隔离的托管路径使用 Kubernetes，后续更强边界使用 microVM。

#### TODO — 不可信输入与 Agent 行为边界（Stage 9 前置）

- [x] 建立 prompt injection 威胁模型文档并冻结防护基线：明确列举攻击者可控的输入通道（repo
      文件内容、依赖元数据、Issue/PR 正文与评论、工具 stdout、外部 MCP 返回、被 fetch 的网页），
      以及每条通道的缓解手段。冻结契约见
      [`Untrusted Content and Sensitive Actions v1`](docs/contracts/untrusted-content-sensitive-actions-v1.md)，
      并明确区分已接线来源与仍保留的 adapter 缺口。
- [x] 实现不可信内容的来源标注：外部来源文本在进入 Provider 上下文时携带显式来源标记，与用户
      指令在结构上可区分，使"README 里写的话"不与"用户下的指令"同权。这是纵深防御的第一层，
      不假设模型一定能抵抗，但显著抬高成功率门槛。External MCP、Synara MCP 与 automation 已使用
      服务端不可伪造的 message source，并在 Provider 输入前进入 escaped JSON-string provenance
      boundary。新增恶意 Issue 形态回归曾抓出 Reactor bootstrap 重新使用原始文本的真实绕过，现已改为
      始终沿用 `provenanceWrappedMessageText`；三类不可信 source 即使内部请求 `full-access` 也由 decider
      降为 `approval-required`。所有本地 Provider 现由 exhaustive registry 声明 content-trust policy 的交付
      位置；Antigravity 因无 system/MCP transport 改为每个 CLI Turn 都携带 identity-only host block。该策略
      明确把 repo/tool stdout/web fetch/第三方 MCP result 视为不可信。Claude managed Host 与本地 adapter
      已用 SDK `PostToolUse.updatedToolOutput` 为成功结果加宿主控制的结构化 envelope；失败结果用不复制正文的
      相邻 host context 标记，`AskUserQuestion` 实时用户答案保留可信作者身份。Codex 0.145 的 `PostToolUse`
      只在 handler success 后运行，不能覆盖 MCP `isError`、patch/Approval 拒绝或 handler failure；managed 与
      local 因此都通过隔离 `CODEX_HOME` 注入同一 Host-owned 命令的单个 session-flags `PreToolUse` hook：
      managed 复用只读 Worker 镜像内 Provider Host 入口，local 把固定有界程序嵌入启动参数并只调用 Synara
      当前绝对进程路径，不依赖
      workspace 可变 helper。两者在执行前追加不复制 input/output/error 正文的 provenance，并在开 Thread 前用
      `hooks/list` 拒绝任一缺失或额外启用的 non-managed hook；start/resume/fork 同时强制 request-level
      hook-trust bypass，
      Pre hook 会在不能产生 fresh Approval 的模式下于执行前拒绝敏感调用，并从 `apply_patch` header 提取
      依赖、CI、Credential 与 egress-policy 路径。approval-required 的 pathless native file-change Approval
      必须按 `itemId` 命中前置 `item/started` assessment，缺失关联时 local/managed Host 都直接拒绝。
      `request_user_input` 实时答案不降级。0.145 的 `write_stdin` 明确跳过 Pre hook，因此隔离参数关闭
      `features.unified_exec`，只暴露每条命令重新分类的一次性 `shell_command`。默认绕过 ToolRegistry 的 hosted
      cached web search 已固定为 `disabled`，code mode、browser use 与 computer use 也关闭；local/managed
      启动在开 Thread 前通过 `config/read` 复验有效 search mode、全部受限 feature 与精确 MCP 配置，任一
      覆盖即失败。local `synara` 必须额外匹配同一 scoped lease 的 numeric-loopback `/mcp` URL、完整字段集与
      `bearer_token_env_var`，且有效 shell policy 必须排除 gateway token 与所有保留的 model-provider
      `env_key` / `env_http_headers` credential mapping，禁止经 `set` / `include_only` / 非默认继承重引入；无
      lease 的 discovery MCP 固定为空集。真实 0.145.0 Responses 请求捕获没有 `web_search`、code-mode `exec` 或 `wait`；当前
      `tool_search` 仅返回 Host-owned Synara MCP registry metadata。真实成功/patch-decline 双 probe
      证明唯一 Pre hook 各写入一条 raw rollout developer context，模型在两类结果后都能复述精确
      policyVersion/toolName；成功路径无 `exec_command` / `write_stdin`，拒绝补丁无副作用。Pi direct SDK
      现禁止 project extension trust，并由最后一个 hidden in-memory Host `tool_result` extension 为成功/失败结果
      追加相邻 provenance，同时保持原始 text/image block。ACP 只在 Agent 已消费结果后向 Client 发 `session/update`；
      OpenCode/Kilo 的模型前 hook 只属于同进程 external plugin chain，故不冒充 Host 边界；Synara 已强制关闭
      二者的 repository project config，并为所有 server/discovery/辅助 CLI 命令同时固定 `--pure` 与对应
      `*_PURE=1`，使 user/global external plugin 也不能加载；workspace 打开前还要求 OpenCode >= 1.15.11、
      Kilo >= 7.4.16，旧 binary 不支持 pure interface、版本过低或版本不可解析时启动失败。已配置的
      外部 OpenCode/Kilo server 无法认证相同 profile。ACP、OpenCode/Kilo 与 Antigravity 仍如实标为
      `policy-only`；Antigravity 2.0 的 `PostToolUse` 官方输出契约也只能是 `{}`，同 UID external plugin 不能算
      Host 边界。result provenance registry 现已成为三类 server-authored untrusted source 的第三项硬准入，
      与 fresh Approval、repository startup isolation 缺一不可；Agent/Synara MCP schema、External MCP capability、
      Automation create/update/run、durable decider 与 Reactor backstop 当前只允许 Codex/Claude，普通人工 Turn
      不受影响。本项按可交付产品面完成；未来 Provider/Codex tool surface 只有在 registry 升级并复验后才能扩大
      准入。官方 release source/hash/隔离 XDG probe 证据见
      [`stage-5-opencode-kilo-pure-mode-local-acceptance-20260729.md`](docs/reports/stage-5-opencode-kilo-pure-mode-local-acceptance-20260729.md)；
      Codex 有效 tool surface 与真实请求捕获见
      [`stage-5-codex-attested-tool-surface-local-acceptance-20260729.md`](docs/reports/stage-5-codex-attested-tool-surface-local-acceptance-20260729.md)，
      scoped MCP transport/token exclusion 的精确自检见
      [`stage-5-codex-mcp-transport-attestation-local-acceptance-20260729.md`](docs/reports/stage-5-codex-mcp-transport-attestation-local-acceptance-20260729.md)，
      model-provider 静态 Credential containment 见
      [`stage-5-codex-model-provider-credential-containment-local-acceptance-20260729.md`](docs/reports/stage-5-codex-model-provider-credential-containment-local-acceptance-20260729.md)。
      Provider 出站代理同样不再把 authenticated URL 当作“可 redaction 的 secret”：Kubernetes Target、agentd
      与 Provider Host 分层拒绝 userinfo/query/fragment/路径、非法 scheme/host/port、无端口 SOCKS5 及
      wildcard/超界 `NO_PROXY`，模型和任意工具只收到 credential-free authority；本地证据见
      [`stage-5-provider-proxy-credential-containment-local-acceptance-20260729.md`](docs/reports/stage-5-provider-proxy-credential-containment-local-acceptance-20260729.md)。
      Result provenance 准入证据见
      [`stage-5-untrusted-result-provenance-admission-local-acceptance-20260729.md`](docs/reports/stage-5-untrusted-result-provenance-admission-local-acceptance-20260729.md)。
- [x] 冻结敏感动作闸门清单，强制走既有 Approval 机制而非自动执行：向默认分支/受保护分支
      push、创建或修改 CI 配置与工作流文件、修改依赖清单与锁文件、新增出网目标、写入
      Credential 相关路径。清单必须是服务端权威，不可由 Provider 侧自行放行。共享 classifier 与
      Claude/Codex、Cursor/Grok/Droid ACP、OpenCode/Kilo request 路径已落地，敏感动作把
      `acceptForSession` 降级为单次 `accept`。不可信任务准入现同时要求宿主可观测 fresh Approval 与
      repository 可执行启动配置隔离：local Codex 使用独立最小 `CODEX_HOME`，只链接 `auth.json`，session
      store 独立并按明确 resume/fork ID copy-on-write 导入；0600 config 只保留非可执行 model-provider
      transport 子集，明文 provider token 改为工具子进程不可见的环境变量；保留的 base URL 必须是无
      userinfo/query/fragment 的 HTTP(S)，静态 query 只接受 dedicated table 中日期形态 `api-version`，任意
      query Credential fail closed。并丢弃 command-backed/AWS auth、
      user/project MCP、hooks、plugins、rules、skills、profiles 与 project trust。Codex 最低版本为 0.145.0，
      `--strict-config` + Host CLI flags 关闭 executable extensions、external memory import、shell snapshot、
      hosted web、code/browser/computer use 与 unified exec，MCP 只允许空集或唯一 Host-owned Synara gateway；
      `config/read` 在开 Thread 前复验同一有效 tool surface、精确 transport 与全部 Provider credential env
      的 shell exclusion。local/managed Claude 使用
      `settingSources: []` + `strictMcpConfig`，OpenCode/Kilo 强制 project-config kill switch + pure mode，并拒绝
      untrusted dispatch 复用不可认证的外部 server，managed Codex
      使用隔离 HOME 与 hooks attestation。Cursor 会自动读取 project MCP；Droid 虽已用每会话 0600 runtime
      settings 关闭所有 hooks、继承 autonomy、IDE auto-connect 与 cloud sync，仍可能加载 project MCP/plugin；
      Grok 也没有已证明的完整 kill switch。因此 Cursor/Grok/Droid 与没有宿主 permission callback 的
      Antigravity/Pi 均在任务创建、Automation create/update/run 与最终编排入口 fail closed，不能承接
      External/Synara MCP 或 automation；Cloud Control Plane 也会严格校验 assessment 并将其从
      `request.opened` 延续到 `request.resolved`。真实 Claude metadata case 会验证 full-access fresh
      Approval；credential-scope case 则要求 Claude `full-access` 与 Codex `approval-required` 都持久化精确
      `credential-access` assessment 并只允许一次授权。`malicious-issue-denial` exact-node case 也已就绪：
      真实 Claude/Codex 必须暴露 `credential-access` + `protected-branch-publish` assessment，Runner 显式
      `decline` 后不允许出现 command item、Terminal、command output 或 Artifact 生命周期；命令由前置
      `false &&` 安全熔断。metadata-egress 的 Codex 路径也已改为 `approval-required` 并要求同一 fresh
      Approval 后才执行实际网络探针。该 case 已在自建物理 K3s 的 Codex/Claude × 完整 Ready Worker
      集合通过；任意子进程未发出 tool/approval 事件时仍只能依赖外层沙箱/凭证/egress，因此未来新增
      Provider 或 Target 必须重新扩展并运行同一负向矩阵。真实 0.145
      `apply_patch` 探针另已证明 full-access 修改 `package.json` 在执行前 blocked 且无文件/lifecycle，
      approval-required 则恰好产生一次 native file-change Approval；显式 decline 后 item 以 `declined` 完成且
      文件不存在，不能把 file-change 的审计 lifecycle 误判为已写入副作用。
      classifier 已进一步覆盖带 Git/package-manager 全局参数的 fetch/push/install、GitLab/Buildkite 等 CI
      路径、Composer/SwiftPM/.NET/Elixir/Dart/Nix 等依赖文件、常见 Credential 路径/命令，以及
      `Write`/`Edit` 新内容中的 URL 或 Credential 引用；删除用 `old_string` 不算新增 authority。当前 Codex
      attested 写入面只有 `apply_patch`，其 patch command/header 由 3909-character inline guard 扫描并保留
      4096 上限，未来新增 content-key 写工具必须先扩展 guard 与工具面 attestation。本地完整证据见
      [`stage-5-sensitive-action-classifier-local-acceptance-20260729.md`](docs/reports/stage-5-sensitive-action-classifier-local-acceptance-20260729.md)。
- [x] 收敛 agent 可达的凭证范围：复核 git push 凭证、云凭证与 Provider Credential Grant 的最小
      权限边界，确保被注入的 agent 拿不到超出当前任务所需的授权（Grant 已是 generation-scoped，
      本条聚焦 git 与云侧）。External MCP 已禁止新授予 `runtime:local` / `runtime:full-access`，旧 scope
      也不能越过 managed-worktree + approval-required runtime policy。Provider 长期 Key 已由 agentd
      execution-lifetime loopback broker 替换为 task token；git fetch secret 在 Provider 前销毁，publish
      型 Binding 不进入普通 Workload，ambient git/cloud secret 被环境 allowlist 排除。Kubernetes
      Pod-bound registration token 也只在受限 init 中投影并由主容器一次性消费删除。新增真实 Provider
      `credential-scope` exact-node case 只检查 ambient cloud、Git/SSH、package、Docker/Kubernetes 与
      ServiceAccount Credential 是否存在，不读取环境值或 Credential 文件内容；仅以 64 KiB 上限检查非
      symlink `.git/config` 的 HTTPS userinfo，输出固定 sentinel，并按设计排除 execution-lifetime Provider
      broker task token。代码门禁已就绪，托管 Provider × Node 矩阵仍待运行。
- [x] 为外部 MCP 建立信任分级与出网约束：当前 managed Provider Host deny-by-default——Claude 不加载
      user/project/local settings，Codex 使用 agentd-owned clean `CODEX_HOME` 并以 `mcp_servers={}` 启动，故 repo 不能启动 MCP
      server 绕过 Target egress。未来 host-defined MCP 必须同时接入 result provenance 并在 Target 内运行。
- [x] 建立注入检测与事后可审计性：External MCP audit migration `088` 已持久化 source/trust/SHA-256、
      bounded indicator IDs 与 `prompt_injection_suspected` alert kind；integration/request/project/created
      Thread 可关联 Message source、Turn、Provider request/item、sensitive categories 与 Approval 结果，
      且不复制 prompt 明文。本地 Runtime Event 与 Approval activity 现也在 opened/resolved 两端保留同一
      canonical assessment。Stage 6 可消费该 durable alert feed 做外部通知。

#### 完成条件

- [x] 单个 Execution 无法通过 fork 炸弹、内存或 CPU 耗尽影响同宿主的其他 Execution 或 Worker，
      并有真实触发验证而非仅配置检查。一次性 Kind `podPidsLimit=128` 已实测 256 次 fork 中 136 次被
      拒绝、64Mi 内存 OOMKilled、CPU finite、邻居 12/12 响应；OrbStack `podPidsLimit=-1` 被严格测试
      抓出且现在会被门禁拒绝。自建物理 K3s 的完整 Ready Worker 集合已按 exact-node 运行；其他
      自建 Target 仍须在接入时独立复验。
- [x] 共享 Worker 上不存在跨租户可读残留，且该结论不依赖路径不可猜测性。判定方式：租户 A 执行
      结束后，在同一 Worker 上运行租户 B 的 Execution，B 的 Provider 进程无法枚举或读取 A 的
      workspace、git cache 与 `/tmp` 残留——以实测而非配置审查为准。真实 agentd + Provider Host
      组合实测已覆盖 6 类路径并由 B 递归扫描同一存储根，见 Stage 5 本地验收报告。
- [x] Kubernetes Execution Target 在缺少 CPU/Memory/EphemeralStorage/PID limits 或实际节点 PID
      上限不合规时无法通过校验/注册。
- [x] 云元数据端点在所有 Kubernetes Worker 上不可达。判定方式：从运行中的 Provider 进程内实际
      发起到 `169.254.169.254`、`100.100.100.200` 与 `fd00:ec2::254` 的请求必须失败，而不是仅检查
      NetworkPolicy 清单是否包含 `except`。Codex/Claude × Ready/非 cordon Worker 矩阵已在本次自建
      物理 K3s Target 完整执行。聚合 `stage5_provider_isolation_matrix.py` 负责按 Target label selector
      枚举首尾节点集合、逐 cell 运行三个 case，并在节点集合变化/漏跑时 fail closed；该 gate 是每个
      后续自建 Target 的接入门禁。
- [x] 所有准入 Provider 在所有 Kubernetes Worker 上只能看到当前 Execution 所需的 broker task token；
      冻结清单内的 ambient cloud、Git/SSH、package、Docker/Kubernetes 或 ServiceAccount Credential 环境名与
      路径均不存在。真实 Provider `credential-scope` exact-node Runner 已就绪，并只持久化固定
      absent/present/error sentinel；同一聚合编排器要求每个 Codex/Claude × Ready/非 cordon Worker cell
      通过该 case。Codex/Claude 已在本次自建物理 K3s Target 的完整 Ready Worker 集合通过；未来新增
      准入 Provider、Worker 或 Target 时必须重新形成完整笛卡尔积，不能沿用本次结论。
- [x] 每种 Execution Target 的隔离等级在契约中显式声明；弱隔离 Target 不出现在多租户产品面。
- [x] Provider 自带沙箱关闭的前提条件有自动化守卫，削弱即失败。
- [x] 存在 prompt injection 威胁模型文档与冻结的防护基线；敏感动作清单为服务端权威，被注入的
      agent 无法在不经审批的情况下 push 到受保护分支、改 CI 配置或新增出网目标。
- [x] 敏感动作可回溯到触发它的输入片段，注入疑似事件可审计、可告警。External MCP audit migration
      `088` 持久化 source/trust/SHA-256/indicator IDs 与 `prompt_injection_suspected`，并可沿 integration /
      request / project / created Thread → Message source → Turn / request / item / sensitive categories / Approval
      结果关联；不复制 prompt 明文，外发通知属于 Stage 6。
- [x] 上述行为边界在 Stage 9 的事件触发自动化上线前已生效，且有以恶意 Issue 正文为输入的负向
      测试证明无人值守路径不会被诱导执行敏感动作。当前 product-path 回归已覆盖恶意 closing tag、
      `git push` 与 Credential 打印指令进入 automation provenance boundary、full-access 被降级；补充
      ingestion 回归将同一 Issue 形态消息与敏感请求/assessment/pending/显式 decline 串联，并证明没有 tool
      lifecycle。真实 Provider `malicious-issue-denial` Runner 进一步把安全熔断的 Issue 形态命令、双类别
      Approval、显式 decline 与零执行生命周期串联，并已在物理 K3s 的 Codex/Claude exact-node 矩阵通过。
      真实 Issue webhook adapter 尚未实现，故 adapter-specific webhook → identity mapping → idempotent
      automation → Provider → 拒绝的端到端测试继续作为 Stage 9 自身的上线门禁（见 Stage 9 入站事件与
      无人值守权限条目），而不是用不存在的 Stage 9 功能反向阻塞 Stage 5。任何 Stage 9 adapter 在该门禁
      通过前不得上线。

### Stage 6：企业 SaaS GA、运营、安全与商业化

状态：TODO。本节于 2026-07-27 重做：原版写于 Stage 5/7/8 存在之前，是 28 条平铺清单，既未核对
仓库现状，也与新阶段多处重叠。本次补齐现状基线、标注跨阶段归属、按主题分组，并补入同类产品
已成常识而本路线缺失的能力。

现状基线（避免重复实现已有模块）：OIDC、SAML、SCIM、Service Account、Audit、Quota、Retention、
KMS 与基础 Observability 已存在；Stage 4 已落地 requested-resource-seconds 成本代理、versioned
tariff、三云账单只读导入与幂等对账、shared Target 成本分摊。因此本阶段的计费工作是**产品化与
对外收口**，不是从零建计量。

2026-07-27 逐条核查后，下列条目已在正文标注"现状核查"，指出其中哪些子项**已有 API 实现**、
真实缺口是什么——按原措辞执行会重建已有模块。当前确认完全缺失的是：Legal Hold、Domain
Verification、SSO Enforcement、Plan/Entitlement/Feature Flag、离职回收闭环。执行本阶段前应对
其余条目重复同样的核查，而不是照单开工。已核查条目的确认缺失项：Legal Hold、Domain Verification、
SSO Enforcement、Plan/Entitlement/Feature Flag、离职回收闭环、分布式 Tracing、用户自助数据导出与
单用户删除、隐私请求（DSAR）受理流程。

跨阶段归属（本阶段不重复实现，只做集成与验收）：

- API Key / Service Account 的资源级 scope 与生命周期归 **Stage 7**；本阶段只负责其在企业管理
  后台的可视化与治理。
- API 文档与开发者文档站归 **Stage 7**；本阶段只负责用户文档、管理员文档、部署与排障文档。
- 跨 Tenant 越权、SSRF、容器逃逸的**修复**归 **Stage 5**；本阶段只负责第三方渗透验收，不接受
  用本阶段测试替代 Stage 5 的修复。
- 协作内容的保留、导出与 Legal Hold 边界由 **Stage 8** 接入本阶段的数据治理策略，不建旁路。
- 免费层/试用的配额形状已在 Stage 7 的 D8 冻结；本阶段负责其 Plan/Entitlement 落地与自助开通。

#### 目标

- 产品可以面向多家公司正式提供服务，而不依赖人工数据库操作和开发模式配置。
- Tenant 生命周期、用户生命周期、用量、成本、配额、安全、审计和支持流程形成闭环。
- 建立明确的 SLO、发布、备份恢复、安全响应和数据治理制度。
- 用户能自己解释"这次花了多少、为什么"，而不是只有平台方看得到成本。

#### TODO — 租户与身份生命周期

- [ ] 完成 Tenant 注册、试用、启用、暂停、关闭、删除和恢复状态机。
- [ ] 完成企业邀请、OIDC/SAML、SCIM、Group Mapping 和离职回收闭环。**现状核查（2026-07-27）**：
      邀请（`POST /v1/tenants/{id}/invitations`、`POST /v1/invitations/{token}/accept`）、SSO 登录、
      SCIM、Group Mapping 的 API 均已存在；实际缺口是**离职回收闭环**——从 IdP 侧停用到平台侧
      Session/Credential/Grant 全链路回收的端到端保证。不要重建已有部分。
- [ ] 支持多个 Identity Connection、Domain Verification 和 SSO Enforcement。**现状核查**：多
      Identity Connection 的增删禁用 API 已存在；`domainVerification` 与 `ssoEnforcement` 全仓库
      无实现，是本条的真实缺口。
- [ ] 评估是否需要自定义 Role；若不需要，冻结固定 RBAC v1（Stage 7 的 API Key 权限语言依赖
      此决策，应优先给出结论）。
- [ ] 完成 User、Service Account、Credential 和 API Token 的生命周期管理（实体与 scope 由
      Stage 7 定义，本阶段负责管理后台与治理视图）。
- [ ] 实现 Provider BYOK、企业统一 Credential 和用户 Credential 的策略与优先级。
- [ ] 完成 Credential Rotation、KMS Key Rotation、Re-encryption 和应急撤销 Runbook。

#### TODO — 计划、配额与计费

- [ ] 建立 Tenant Plan、Entitlement、Quota 和 Feature Flag 模型。**现状核查**：Quota 已有
      `GET|PUT /v1/tenants/{id}/quota` 与 `internal/quotas`；Plan、Entitlement、Feature Flag 均无
      实现，是本条的真实缺口，且 Stage 7 的自助免费层依赖它们。
- [ ] 记录 Token、Execution Time、CPU、Memory、Storage、Network 和 Provider Cost 用量（Stage 4
      已有 requested-resource-seconds 与云账单导入，本条聚焦缺口：Token 与 Network 维度）。
- [ ] 实现用量聚合、账单周期、超限行为和管理员报表。
- [ ] 如需要对外收费，接入 Billing Provider；内部平台则接入成本中心/部门分摊。
- [ ] 【新增】实现面向最终用户的用量与成本可见性：per-Session / per-Turn 的 token、执行时长与
      估算成本，可回答"这次为什么贵"。Stage 4 建的是平台侧成本权威，用户侧可见性是另一件事，
      当前完全缺失；同类 agent 产品已把它当作基础能力，缺失会直接导致用量投诉与信任流失。
- [ ] 【新增】实现软性配额预警：接近上限时先告警与降级提示，而非直接硬停。硬停无预警是自助
      产品最常见的差评来源，且与 Stage 7 免费层的低门槛注册叠加后影响面更大。

#### TODO — 运营与支持

- [ ] 建立 Tenant/Organization 管理后台和 Platform Admin 后台。
- [ ] 实现 Worker、Execution、Queue、Artifact、Credential 和 Identity Connection 运维视图。
- [ ] 【新增】实现受控的支持态 impersonation：支持工程师在排障时以租户视角只读访问，必须填写
      理由、有时限、全程审计、对租户管理员可见，且租户可整体关闭该能力。没有它，支持只能靠
      直连数据库（更危险）；有它但不受控则是最典型的内部越权事故来源，两者都不可接受。
- [ ] 【新增】建立公开 Status Page 与事故对外沟通流程（事件分级、发布时机、事后复盘公开范围）。
      当前已规划 On-call 与告警，但缺少对外一侧；企业客户在采购阶段就会检查它是否存在。

#### TODO — 数据治理与合规

- [ ] 建立 Audit Search、Export、Legal Hold 和保留策略（需覆盖 Stage 8 的协作内容）。**现状
      核查**：Audit Search（`GET .../audit-logs`）、Export（`GET .../audit-logs/export`）与 Retention
      Policy（`GET|PUT .../retention-policy`）的 API 均已存在；`legalHold` 全仓库无实现，是本条的
      真实缺口。本阶段应聚焦 Legal Hold，以及把 Stage 8 协作内容纳入既有保留策略。
- [ ] 完成数据导出、Tenant 删除、用户删除和隐私请求流程。**现状核查**：Tenant 删除已有
      `deleting` 状态与 `internal/retention` 的清除链路（Stage 4 的 Credential 下发也已按
      `deleting` 停止）；用户自助数据导出、单用户删除与隐私请求（DSAR）受理流程均无实现，
      是本条的真实缺口。
- [ ] 明确 Provider 许可、账号共享、数据使用、隐私和企业合规要求。
- [ ] 【新增】确定合规认证路径（SOC 2 Type II / ISO 27001 择一或并行），并把证据采集自动化接到
      既有 Audit 与发布流程上。"满足合规要求"与"拿到可出示的认证"是两件事，后者是企业采购的
      硬门槛，且准备周期以季度计，必须尽早启动而不是 GA 前补。
- [ ] 【新增】将数据驻留从调度能力提升为产品承诺：租户可选择 Region 并获得书面边界说明。
      Stage 4 已具备跨 Region/Cluster 路由与 evacuation 能力，但没有对租户的可承诺语义；
      欧盟与金融客户会把它作为准入条件。

#### TODO — 可靠性、发布与安全验收

- [ ] 定义 PostgreSQL、S3、KMS、Queue 的备份、恢复和 RPO/RTO。
- [ ] 定期执行数据库恢复、对象恢复和区域灾备演练。
- [ ] 定义 Availability、API Latency、Execution Start Delay、Event Delay 等 SLO（交互式冷启动
      SLO 由 Stage 4 与 fast-provision 提案定义，本阶段负责对外承诺口径）。
- [ ] 建立错误预算、告警分级、On-call 和事故响应流程。
- [ ] 完成结构化日志、Tracing、Metrics 与 Tenant 安全边界审计。**现状核查（2026-07-27）**：
      结构化日志（slog）与 Metrics（Stage 4 已建大量 bounded 指标与滚动预聚合）已具备；
      分布式 Tracing 实质缺失——仅 `httpapi/server.go` 发出 `Traceparent`/`X-Trace-ID` 响应头，
      控制面内部包无 OpenTelemetry 接入，跨 Control Plane → Worker → Provider 的链路无法串联。
      本条的真实缺口是 Tracing 落地与 Tenant 边界审计。
- [ ] 执行跨 Tenant 越权、SSRF、命令注入、路径穿越、供应链和容器逃逸测试（**验收 Stage 5 的
      修复成果**；Stage 5 未完成前本条不得开始，否则只会重复记录已知缺口）。
- [ ] 对 Worker Image、Provider CLI、依赖和 SBOM 建立签名与漏洞扫描。
- [ ] 完成 Secret 管理、生产配置、证书、域名和密钥轮换流程。
- [ ] 建立数据库 Migration、协议版本、Worker Image 和前端的**跨组件兼容发布矩阵**。注意去重：
      Worker Release 的 canary/promote/rollback **机制**已由 Stage 3 完成并落在
      `internal/workerreleases`（含 `auto_rollback.go`、`scheduling.go` 与对应 API 路由），
      Worker/Pod Drain 与滚动升级也已由 Stage 4 完成。本阶段**不重复实现这些机制**，只负责它们
      之间的兼容矩阵。
- [ ] 建立发布评审、审批与对外变更通告流程：谁批准、按哪份 checklist 门禁、如何对外公告，与
      产品内更新日志及 Stage 7 的 `Deprecation`/`Sunset` 政策共用同一发布节奏。
- [ ] 完成用户文档、管理员文档、部署文档和故障排查文档（API 与开发者文档归 Stage 7）。
- [ ] 【新增】建立产品内更新日志与破坏性变更通告通道，与 Stage 7 的 `Deprecation`/`Sunset`
      政策共用同一发布节奏，避免对外承诺与产品内公告不一致。
- [ ] 完成容量测试、长时间稳定性测试、渗透测试和上线评审。
- [ ] 【新增】编写 `docs/release-checklists/stage-6-enterprise-ga.md`，沿用既有约定（每次发布复制
      一份，记录 Commit、镜像 Digest、Migration、执行人、时间与证据链接；未满足项保持未勾选，不
      接受 fixture 或静态检查替代真实发布证据）。当前"GA Release Checklist 全部通过"被列为完成
      条件，但该清单从未被定义，而 Stage 2/3 都已有对应文件——这是一个悬空的验收门禁。

#### 完成条件

- [ ] 新 Tenant 可以不经人工数据库操作完成注册或企业开通。
- [ ] 用户入职、调岗、离职和 Credential 回收有完整审计链路。
- [ ] 用量、配额、成本和超限策略可解释且并发安全；用户可自助查到单次 Session/Turn 的成本构成。
      并发安全以真实 PostgreSQL 双连接实测为准（沿用 Stage 4 既有的并发验证方式），不接受
      仅代码审查。
- [ ] 支持人员无需直连生产数据库即可完成排障，且每次 impersonation 可审计、租户可见。判定方式：
      列出支持团队的日常操作清单，逐项验证可在管理后台完成，不依赖 CLI 或数据库客户端。
- [ ] 生产环境有明确 SLO、告警、值班、对外 Status Page 和事故处理流程。
- [ ] 备份恢复与区域灾备经过真实演练。判定方式：从生产备份实际恢复到可服务状态，测得 RTO/RPO
      并与承诺值比对——执行备份脚本成功不算通过，必须完成恢复侧验证。
- [ ] 第三方渗透测试没有未接受的高危问题，且 Stage 5 的沙箱修复已先行完成。
- [ ] 跨组件发布（Migration × 协议版本 × Worker Image × 前端）有明确兼容矩阵与审批流程。Worker
      侧灰度与回滚的机制正确性由 Stage 3/4 保证，本阶段只验收其在完整发布流程中被正确使用。
- [ ] 企业管理员和平台运维人员不依赖开发工具完成日常操作。
- [ ] 合规认证路径已确定并在执行中，证据采集自动化而非人工整理。
- [ ] `docs/release-checklists/stage-6-enterprise-ga.md` 已存在且全部通过，证据链接完整。

### Stage 7：对外 SDK 与开发者平台

状态：TODO。设计已冻结（2026-07-26），完整内容见独立设计文档
[`docs/plans/external-sdk-developer-platform.md`](docs/plans/external-sdk-developer-platform.md)，
本节不重复其 TODO 与完成条件。

摘要：把只服务第一方（Web 经 Node 代理、Worker、SCIM）的 `/v1` 开放成外部开发者可编程的产品。
已冻结的关键决策：公开面三级 allowlist + 防暴露守卫；OpenAPI 3.1 单一契约源 + CI 一致性门禁；
机器鉴权扩展既有 Service Account（与 SCIM 同一实体分 scope），不新造凭证体系、不开 CORS；
SDK 采用自建 codegen 管线，TypeScript 首发、Python 紧随；品牌定名 **Polaris**
（npm `@polaris-agents/sdk`、PyPI `polaris-agents`）；不做独立 execution 事件流；免费层为强制
BYOK + 平台计算配额。GA 有一条硬门槛：Stage 5 的沙箱隔离加固必须先完成——公开 SDK 意味着任意
第三方提交任意代码。

### Stage 8：组织内协作与 Agent/人统一提及

状态：TODO。设想评估日期 2026-07-26，定位于 2026-07-27 由操作人明确。

**产品定位（不可动摇的前提）：本阶段是组织既有 IM 的补充，不是替代。** 组织继续在
飞书/Slack/Teams 里进行日常沟通；Synara 只承接一类它们承接不了的对话——**锚定在 Session、
Execution、Artifact、diff 上，且回复需要直接生效的对话**。

要解决的问题是真实的：信息丢失发生在"上下文离开 Synara"的边界上——把 Session 片段截图或复制
到第三方 IM，丢掉的是完整历史、Artifact、diff、执行状态，以及最关键的"回复能直接生效"的能力。
同事在 IM 里回一句"可以，但改成 batch"，这句话无法自己变成 Session 里的一个动作。

但解法方向必须是反的：不是把对话搬进来，而是**让上下文无损地走出去、让回复可执行地走回来**。
自建通用 IM 会同时踩三个坑——组织不会用编码 agent 平台取代既有 IM，于是出现第二个信息沉淀地
使丢失加剧；presence/推送/移动端/搜索/留存/合规取证会吞掉整条路线图；聊天本身也不构成差异化。

补充定位有三条由此推出的硬约束，它们是"补充"不退化成"第二个 IM"的全部保障：

1. **锚定规则**：平台内的每一条协作内容都必须锚定到具体的 Session / Execution / Artifact /
   diff 位置。不提供游离的自由对话入口；没有锚点的讨论一律属于 IM。这条规则让两个渠道的领域
   互斥而非竞争，是避免"信息分裂成两半"的根本手段。
2. **单向溢出**：平台内的协作可以溢出到 IM（通知、deep link、可执行动作），但 IM 中的普通对话
   不向平台同步。**不做双向镜像**——双向镜像是滑向"变成 IM"的起点。
3. **不以活跃度衡量**：本阶段的成功指标是"跨人协作是否无损完成"，不是消息数、DAU 或停留时长。
   一旦按互动量优化，产品必然朝替代 IM 漂移。

已有地基比预期多：

- `packages/contracts/src/agentMentions.ts` 已实现 `@alias(task)` 语法，语义是**委派给 agent**。
- 控制面已有 Approval / Structured User Input 的完整链路（interaction 拉取、resolve 端点、
  `snapshotSequence` 对账），本质就是"执行中向人提问并把回答变成动作"。
- 已有 session reader 的 `session.event.redacted` 投影，即"他人查看本 Session"的权限模型骨架。
- 控制面尚无任何 notification / mention / comment 实现，人侧协作是 greenfield。

因此本阶段的差异化命题是**统一提及命名空间**：`@` 既可指向 agent（已实现），也可指向组织成员
（本阶段新增），共用同一语法、同一 durable interaction 语义、同一事件流——人和 agent 都是"可被
委派的响应者"。这是自建 IM 换不来的东西，也正是"补充"的价值所在：它补的不是聊天能力，而是
IM 天然做不到的"对话与执行同处一个权威状态机"。

#### 目标

- Session 成为可协作对象；锚定在证据上的跨人协作不需要把上下文搬出平台。
- `@` 对人与 agent 使用同一语法和同一 durable 语义，人↔agent 四个方向共用一个 interaction
  类型，不产生两套并行机制。
- 触达复用组织既有 IM 与邮件，不自建通讯基础设施；组织的 IM 使用习惯不因本阶段改变。
- 提及是受控、可审计、可撤销的访问授予，绝不成为权限旁路。

#### TODO

- [ ] 冻结统一提及契约：`@member` 与既有 `@agent-alias` 共用解析、渲染与 durable interaction
      语义；明确两者在能力上的差异（agent 可执行、人可决策/回答），但不在语法与事件模型上分叉。
- [ ] 在控制面实现 Mention 领域模型：durable、幂等（复用 `Idempotency-Key`）、进入 Session
      Event Stream、可 resolve/过期，并复用既有 interaction 的 pending 快照与 reconcile 语义。
- [ ] 冻结"提及即访问授予"的安全语义：被提及人按**其自身角色**应用既有 redaction 投影（不继承
      提及者的可见性）、写入 Audit、可撤销、可设过期；提及不得越过 Organization 边界。
- [ ] 实现 Session 转交（handoff）、协作者列表与显式共享，复用 Organization 角色而非新建权限体系。
- [ ] 实现对具体 Event / Artifact / diff 位置的定位评论，使讨论锚定在证据上而非游离的聊天流。
- [ ] 将锚定规则实现为强制约束而非惯例：所有协作内容必须携带 Session / Execution / Artifact /
      diff 锚点，不提供任何创建无锚点会话的入口；缺锚点的写入在 API 层直接拒绝。这是"补充"不
      退化为"第二个 IM"的结构性保障，需要一条守卫测试防止后续新增入口绕过它。
- [ ] 触达层集成而非自建：Slack / 飞书 / Teams / 邮件；通知携带 deep link 与安全摘要（不外发敏感
      内容，复用 Stage 7 webhook 的 thin-payload 原则）；简单动作（approve / reject / 短回答）
      可在 IM 内完成并回写 Session，实现复用 Stage 7 的 Webhook + SDK。
- [ ] 实现通知偏好、免打扰与聚合，避免提及噪音；未读/待办以"待我响应的 interaction"为单位，
      不引入独立的消息未读体系。
- [ ] 将协作内容纳入既有 Audit 与 Retention 机制，明确保留期、导出与 Legal Hold 边界（成为沟通
      通道即继承合规义务，不能游离在 Stage 6 的数据治理之外）。
- [ ] 将非目标写入契约：通用频道、与 Session 无关的 DM、presence、输入中状态、移动端应用、
      语音/视频，以及**与组织 IM 的双向消息镜像**。这些一律不做——溢出只允许平台 → IM 单向，
      IM 中的普通对话不回流，避免退化为第二个 IM。
- [ ] 【已决策 2026-07-27：统一】实现单一 Request/Response interaction 类型，覆盖全部四个方向：
      人 → agent（既有 `@alias(task)` 委派）、人 → 人（新增）、agent → 人（**既有 Approval /
      Structured User Input 即是此方向**）、agent → agent（子任务委派）。发起者与响应者各自可为
      人或 agent；锚点、durable 语义、事件流、幂等与 pending 队列全部共用，不为方向差异建第二套
      模型。人的"待我响应"收件箱因此天然同时覆盖"agent 请求审批"与"同事提问"。
- [ ] 将既有 Approval / Structured User Input 重述为统一类型的子类型，且必须是**契约层加法**：
      现有 wire 类型、resolve 端点、一次性 receipt、Generation fencing 和 fail-closed 语义保持
      有效（Stage 3/4 已冻结，不得回归），统一模型只做泛化不做替换；迁移期两种表述必须指向同一
      持久化事实，不允许双写。
- [ ] 冻结响应者能力门禁，堵住统一**新引入**的权限提升面（两套机制分开时不存在）：响应者为
      agent 时不得解析需要人类授权的请求——禁止 `@` 另一个 agent 来批准本应由人批准的事项；
      被提及 agent 的执行权限取"发起者有效权限 ∩ 该 agent 自身权限"，永不放大；跨 Organization
      的提及一律拒绝。门禁按响应者类型而非请求类型判定，并需要负向测试覆盖。
- [ ] 【已决策 2026-07-27：并入统一模型】将 Stage 9 的 VCS 评论提及纳入同一 interaction 类型：
      PR/Issue 评论中 `@` agent 是一次外部来源的提及，agent 的 review comment 回写是该提及的
      响应溢出到外部渠道——正是本阶段"单向溢出"原则的实例。因此锚点模型需从 Session/Execution/
      Artifact/diff 扩展到 PR/commit/行位置，但**不新建第二套提及机制**。
- [ ] 冻结外部身份的信任边界（上一条引入的新风险）：来自 VCS 的提及必须先映射到已知 Organization
      成员或显式配置的服务身份才被受理；未映射身份、外部协作者与公开仓库的匿名评论一律拒绝并
      记审计。否则"任何能在仓库下评论的人都能触发你的 agent"，等于把 Stage 5 的敏感动作闸门
      从外部绕开。映射关系变更（成员离职、权限降级）必须即时生效。

#### 完成条件

- [ ] 一次典型跨人协作（提问 → 同事回答 → 回答在 Session 内直接生效）全程不需要离开平台，
      也不需要人工复制任何上下文。判定方式：以一次真实跨人任务端到端演练为准，记录是否发生过
      截图、复制粘贴或在 IM 中转述上下文——发生任意一次即未通过。
- [ ] 被提及人看到的内容严格按其自身角色投影；提及不产生越权可见性，且每次授予与撤销均可审计。
- [ ] 四个提及方向（人↔agent 全组合）共用同一 interaction 类型与同一持久化事实，既有 Approval /
      Structured User Input 的冻结语义无回归；agent 无法解析需要人类授权的请求，且被提及 agent
      的权限不超过发起者，均有负向测试证明。
- [ ] 组织既有 IM 能收到可点击、可执行的通知；至少 approve / reject / 短回答可在 IM 内完成并
      正确回写 Session。
- [ ] 平台没有引入独立通用聊天面（无频道、无 presence、无移动端、无双向镜像），非目标在契约中
      成文；锚定规则有守卫测试，无锚点内容无法写入。
- [ ] 协作内容的保留、导出与审计与 Stage 6 的数据治理策略一致，无独立旁路。
- [ ] 补充定位可验证：组织的 IM 使用习惯与工具选择未因本阶段发生改变，本阶段的度量口径是
      "跨人协作无损完成率"，未引入消息数 / DAU / 停留时长等互动量指标。

### Stage 9：开发者工作流集成与自动化

状态：TODO。立项日期 2026-07-27，来源是一次现状核查而非新设想。

立项依据（这是**回归修复**，不只是新功能）：本地 TypeScript 产品已实现完整的 Automation
能力——`packages/contracts/src/automation.ts` 有 cron 表达式、时区、`AutomationSchedule`、
`AutomationWorktreeMode`、`approval-required` 运行模式与 standalone/heartbeat/dedicated 模式，
`pullRequests.ts` 也有完整的 PR 状态与合并动作模型。但 **Go 控制面没有任何 automation /
trigger / webhook 实现**（`schedulingdecision` 与 `schedulingpolicy` 是执行放置决策，不是用户侧
自动化）。Stage 3 已把 Session 权威迁到控制面，而这两块能力留在本地，等于 SaaS 版本缺少个人版
已有的功能。这直接触碰 Roadmap-wide rule "Personal、Single-node、Enterprise 保持同一领域模型，
不维护独立产品分支"——放任下去会分叉成两个产品，是本阶段最优先要止损的问题。

同时补齐一个从未被规划的产品面：**与 VCS 的应用级集成**。Stage 3 已完成 Git
Clone/Fetch/Branch/Worktree/Push/PR 的 workspace 生命周期，但那是"用凭证操作 git"，不是"作为
GitHub/GitLab App 参与开发流"——没有安装式授权、没有入站事件、没有 Checks 回写。同类产品
（后台 agent、PR review bot、issue 自动修复）的入口几乎全部在这一层，缺失它意味着用户必须手工
把工作搬进平台。

排期说明：本阶段的两组任务依赖不同，不应作为一个整体排期。**"自动化能力回归"组不依赖 Stage 8，
建议提前启动**——它修的是既有能力缺口而非新增功能，拖得越久本地与云端分叉越深；**"VCS 应用级
集成"组依赖 Stage 8 的统一 interaction 类型**（评论提及必须复用它，否则会长出第二套提及机制），
且其事件触发上线前必须先完成 Stage 5 的不可信输入组。全局视图见文首"阶段间排期与并行"。

#### 目标

- 个人版与 SaaS 版共用同一 Automation 领域模型，不存在"本地有、云端没有"的能力。
- 用户可以从既有开发流（PR、Issue、提交）直接触发 agent，不必手工搬运上下文。
- 无人值守执行的安全边界明确：自动化不得成为绕过审批与沙箱的通道。

#### TODO — 自动化能力回归（建议优先）

- [ ] 将本地 `automation.ts` 的领域模型迁移为控制面权威：Schedule（cron/时区）、
      `AutomationWorktreeMode`、`approval-required` 运行模式、standalone/heartbeat/dedicated
      模式与 Proposal 状态机，复用既有 Session/Execution 权威而非新建并行执行路径。
- [ ] 实现定时触发的 Execution 创建，接入 Stage 4 的 batch lane、fairqueue 与 Tenant 配额；
      定时任务不得占用交互式 lane 的保证容量。
- [ ] 冻结无人值守执行的授权语义：`approval-required` 在云端的含义、审批超时行为、以及自动化
      持有的 Provider Credential Grant 范围；自动化不得静默获得比创建者更高的权限。
- [ ] 将 PR 管理（`pullRequests.ts` 的状态与合并动作）纳入控制面权威，或明确声明其为本地专属
      并在产品面标注——两者取其一，不允许长期悬空。

#### TODO — VCS 应用级集成

- [ ] 实现 GitHub App / GitLab App 安装式授权，替代长期个人 token；仓库授权范围由安装决定，
      并接入既有 Provider Credential 与 KMS 体系。
- [ ] 实现入站事件订阅：PR opened/updated、Issue labeled、评论中 `@` 提及；事件到 Execution 的
      映射必须幂等（复用 `Idempotency-Key` 语义），重复投递不产生重复执行。评论提及走 **Stage 8
      的统一 interaction 类型**，不新建第二套提及机制；外部身份必须先映射到已知 Organization
      成员或服务身份才受理（信任边界见 Stage 8 对应条目）。
- [ ] 实现出站结果回写：以 Check Run / Commit Status 呈现 agent 结论，以 review comment 形式
      给出定位到行的建议；回写内容遵循 Stage 8 的 thin-payload 与脱敏原则。回写在模型上是"提及
      的响应溢出到外部渠道"，与 Stage 8 的单向溢出原则一致——不因此引入 VCS 评论向平台的反向
      镜像。
- [ ] 支持自托管 GitHub Enterprise / GitLab 与企业代理网络（与 Stage 4 的私有 Git、企业代理
      开放项合并推进，不重复实现）。
- [ ] 明确 VCS 集成的出网边界：入站 webhook 与出站 API 调用都要落在 Stage 5 冻结的 egress
      allowlist 内，不得为集成开特例。

#### TODO — 接入与首次体验

- [ ] 实现仓库连接引导与项目模板，使新用户从注册到第一次成功 Turn 不需要阅读文档。
- [ ] 建立 agent 效果可观测性：成功率、重试率、人工接管率与按 Provider/模型的对比，用于回答
      "换模型是否更好"。当前 Stage 4 建的全是执行侧指标（冷启动、排队、Pod 失败分类），**没有任何
      效果侧指标**——即系统能精确回答"跑得快不快"，完全无法回答"跑得好不好"。
- [ ] 优先接入采纳率这一免费的高信号指标：VCS 集成天然产出 ground truth——agent 提交的 PR 是否
      被合并、review 建议是否被采纳、产出是否被人工大幅重写。这比任何自建评分都更接近真实质量，
      且随 VCS 集成一并获得，不需要额外的标注或 eval 基础设施。
- [ ] 【留待评估】完整的评估体系（eval 数据集、prompt/模型变更的回归检测、成本-质量权衡曲线）
      内容量足以独立成阶段，但**当前不宜提前建设**：eval 基础设施的价值依赖真实用量规模，样本
      不足时建出来的是维护负担而非决策依据。触发条件：采纳率指标上线并积累足够样本、或出现
      一次由模型/prompt 变更导致而未被发现的质量回归。届时再从本阶段提取。

#### 完成条件

- [ ] 个人版与 SaaS 版的 Automation 使用同一领域模型与同一契约，无独立分支。
- [ ] 定时与事件触发的 Execution 具备幂等性。判定方式：对同一 VCS 事件重复投递（含 VCS 平台的
      自动重试与人工重放）不产生第二次 Execution，也不产生重复的 PR 评论或 Check Run。
- [ ] 自动化执行的权限不超过其创建者，且无人值守路径不能绕过审批与沙箱边界。
- [ ] 从 PR 或 Issue 触发的 agent 结论能以 Check Run 与 review comment 回到开发流，且该链路复用
      Stage 8 的统一 interaction 类型，未产生第二套提及机制。
- [ ] 未映射到已知成员的外部 VCS 身份无法触发任何 Execution，且尝试被记入审计。
- [ ] 存在效果侧指标（至少采纳率），可回答"换模型/换 prompt 后质量是否变化"。
- [ ] 新用户可在不阅读文档的情况下完成仓库连接并跑通第一次 Turn。

### Roadmap-wide rules

- [ ] 每个 Stage 开始前重新审计当前代码和计划状态。
- [ ] 已有功能以真实测试和验收证据为准，不以文件名或计划勾选状态为准。
- [ ] Personal、Single-node、Enterprise 保持同一领域模型，不维护独立产品分支。**该规则曾被违反**：
      Automation 与 PR 管理只实现在本地 TypeScript 侧（`packages/contracts/src/automation.ts`、
      `pullRequests.ts`），控制面完全没有，Stage 9 正在止损。因此加强为：功能可以先在本地落地，
      但必须同时登记其云端归属阶段；不允许无归属地长期停留在本地。
- [ ] Local、SSH、Docker、Kubernetes 保持同一 Worker Protocol。
- [ ] 进入 Provider 上下文的一切外部内容都是不可信输入（repo 文件、依赖元数据、Issue/PR 正文与
      评论、工具输出、外部 MCP 返回、被 fetch 的网页）。**新增任何输入通道时，必须同时声明其信任
      等级与缓解手段**，不得默认按可信处理。此规则由 Stage 5 冻结，适用于所有后续阶段——每新增
      一条通道（未来的邮件、IM、上传文件等）都要重新走一次这个声明，而不是沿用已有通道的结论。
- [ ] 提及与人机交互只有一套模型：`@` 无论指向人还是 agent、无论来自 Web、IM 还是 VCS 评论，都
      复用 Stage 8 冻结的统一 interaction 类型与同一持久化事实。新增触达渠道只能作为该模型的
      入站来源或出站溢出，不得新建第二套提及/审批/收件箱机制。
- [ ] 新增 Execution Target 或运行时供给层级（如 microVM/snapshot-restore）时，必须显式声明其
      隔离等级，并说明是否可用于多租户面。未声明隔离等级的运行时不得进入多租户产品面；弱隔离
      Target 必须在文档与产品面标注用途边界（当前 Docker Target 即属待决策项，见 Stage 5）。
- [ ] 前端不直接连接 Worker，Worker 不直接访问 Control Plane 数据库。
- [ ] PostgreSQL 保存事务状态，S3/MinIO 保存大对象，Worker 本地状态可随时丢弃。
- [ ] 所有跨进程命令和事件都必须幂等、版本化并可审计。
- [ ] 数据库 Migration 序号是全局单调、跨分支唯一的资源：并行分支/worktree 在合并前必须重新核对
      `services/control-plane/migrations` 的版本号唯一性。重复版本号会让 `readMigrations` 启动时 fail closed；
      已在任何环境应用过的 Migration 不能改号或改内容（checksum lineage 会拒绝），冲突只能由未部署的一侧改号。
- [ ] `docs/reports/` 下的验收证据（含 JSON/JSONL 与其中记录的 SHA-256 冻结表）一经生成即视为不可变：
      格式化工具、批量重排和后续编辑都不得触碰，否则报告内的校验和与文件本体不再匹配，证据链失效。
      `.oxfmtrc.json` 已将 `docs/reports` 加入 ignore；新增证据目录时必须同步维护该 ignore 列表。
- [ ] 后台恢复/对账类 sweep 必须有唯一的不节流权威（leader-elected reconciler）；散布在请求热路径上的
      opportunistic sweep 只能作为延迟优化，必须节流且不得成为任何正确性前提。
- [ ] 根 `package.json` 的 `overrides` 优先级高于任何 workspace 包的依赖范围：合并分支时必须把 overrides
      与各包的直接依赖一起核对。过期的 override 会静默压过 `^x.y.z`，症状是装出低版本、改 lockfile 条目后
      被重新生成回旧版、`bun update` 也"无效"，极易被误判为镜像元数据陈旧或缓存问题。判定方法是先查
      `overrides`，而不是先查 registry。同理适用于 `resolutions` 与 workspace `catalog`。
- [ ] 真实 PostgreSQL 门禁测试当前无法整套通过：以 stage-2 checklist 的调用方式（全新数据库 +
      `go test -p 1 -count=1 ./...`）实测 **93 个失败 / 4 个包**，其中仅 2 个属于在途未提交工作，
      其余 91 个为既有。失败集中在 `executions_integration_test.go`(25)、
      `provider_cursor_postgres_integration_test.go`(11)、`workspace_cleanup_integration_test.go`(9)。
      已抽样确认三类根因：(1) 多个测试共用同一个 `SYNARA_TEST_DATABASE_URL` 且不自清理，彼此撞唯一
      约束——单测独占全新库可通过，整包同库即失败；(2) 迁移测试的 seed helper 未随后续迁移更新，缺少
      后来被设为 NOT NULL 的列（如 `000063` 的 `requested_execution_target_id`）；(3) 迁移测试把当前
      `bootstrap` 生产代码跑在历史 schema 上，引用尚不存在的列（如 `ssh_operation_generation`）。
      迁移本身经核对是正确的（`000063` 用"加可空列 → 回填 → 设 NOT NULL"的安全模式），问题在测试侧。
      已修一处：`cleanupFixture` 漏了 Migration `000078`/`000081` 新增的两个
      `execution_generation_facts` 子表，补齐后 `internal/executions` 在全新库上从 54 个失败降到 27 个、
      无新增失败。修复该类问题的方法是逐层跑单测看 FK/触发器报错，并用
      `pg_constraint`/`pg_trigger` 反查真实引用关系，不要照名字猜；也不要照兄弟表的写法猜列名——
      `worker_incarnation_metric_rollup_entries` 按 `worker_id` 组织、没有 `tenant_id` 列，照搬
      `execution_generation_metric_rollup_entries` 的 `WHERE tenant_id = ?` 会让该包失败数从 27 涨到 80。
      每次改动都要用失败测试名集合（而非计数）对比前后：清理提前中断会掩盖后续失败，清理走得更远又会
      暴露新失败，两种效应会让计数互相抵消而失去意义。
      CI 不运行这些门禁测试，所以长期无人察觉；修复前不应把 checklist 的
      "真实 PostgreSQL Integration Test 通过"勾成通过。
