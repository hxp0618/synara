# TODO

## Small things

- [x] Submitting new messages should scroll to bottom
- [x] Only show last 10 threads for a given project
- [ ] Thread archiving
- [x] New projects should go on top
- [ ] Projects should be sorted by latest thread update

## Bigger things

- [x] Queueing messages

## Enterprise self-hosted product roadmap

> 这里的 Stage 是产品路线阶段，不等同于
> `docs/plans/saas-tenancy-organization-user-plan.md` 中的技术 Phase 0-6。
>
> 当前工作区已经包含大量 Control Plane、Provider Host、agentd、Docker、Kubernetes、
> Artifact、Credential 和企业身份基础实现。下面的 TODO 表示生产化与产品收口目标；执行每个
> Stage 前必须先做差距审计，禁止按旧计划重复实现已有模块。

| Stage   | 目标                                                     | 状态                | 依赖             |
| ------- | -------------------------------------------------------- | ------------------- | ---------------- |
| Stage 1 | 定义 Control Plane 边界、Tenant/Organization/User 和协议 | 基线完成            | —                |
| Stage 2 | Go Control Plane 收口与生产化                            | 仓库内完成 / 已验收 | Stage 1          |
| Stage 3 | Provider Runtime 与远程 Worker 产品化                    | 已完成 / 已验收     | Stage 2          |
| Stage 4 | 分布式执行平台和 K8s 多集群生产化                        | COMPLETE            | Stage 2、Stage 3 |
| Stage 5 | Provider 沙箱与运行时隔离加固                            | IN PROGRESS         | Stage 3、Stage 4 |
| Stage 6 | 企业 Self-hosted GA、运营、安全与成本治理                | TODO                | Stage 2-5        |
| Stage 7 | 对外 SDK 与开发者平台                                    | TODO                | Stage 2、5、6    |
| Stage 8 | 组织内协作与 Agent/人统一提及                            | TODO                | Stage 6、Stage 7 |
| Stage 9 | 开发者工作流集成与自动化                                 | TODO                | Stage 4、5、8    |
| Stage 10 | 计算供给弹性与成本可编程化                              | TODO                | Stage 4、6、7、8 |

Stage 2 的独立执行计划：
[`docs/plans/stage-2-go-control-plane-productionization.md`](docs/plans/stage-2-go-control-plane-productionization.md)

Stage 3 的独立执行计划：
[`docs/plans/stage-3-provider-runtime-remote-worker-productization.md`](docs/plans/stage-3-provider-runtime-remote-worker-productization.md)

Stage 7 的独立设计文档（文件名不带阶段编号，后续顺延不需要重命名）：
[`docs/plans/external-sdk-developer-platform.md`](docs/plans/external-sdk-developer-platform.md)

> 独立计划文档的抽取时机（2026-07-27 决定）：**阶段启动时抽取，启动前留在本文件**。理由是
> Roadmap-wide rules 要求"每个 Stage 开始前重新审计当前代码和计划状态"——启动前写的详细设计
> 文档会在真正动工时已经过时，反而制造"文档说的和代码不一样"的负担。Stage 7 已有文档是因为
> 其关键决策（鉴权模型、契约源、命名、免费层）已冻结且不依赖后续代码状态。Stage 5、6、8、9、10
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
| Stage 10 FOCUS 命名决策 → Stage 7 usage/cost 契约冻结 | 对外契约冻结后再改列名与语义是 breaking change |

**关键路径（面向 GA）**：Stage 5 → Stage 6 → GA。Stage 7/8/9/10 是产品扩展，
除上表门禁外不阻塞 GA。

**建议尽早启动、不必等待前序阶段的工作**：

- Stage 5 的资源上限与云元数据阻断——无任何依赖，改动最小，且资源上限是唯一无需恶意租户即可
  触发的可靠性风险，应当立即开始。
- Stage 9 的"自动化能力回归"组——修的是本地已有而云端缺失的能力，属于回归而非新功能，拖得越久
  本地与云端分叉越深；该组不依赖 Stage 8。
- Stage 7 的 M1–M2（API 产品化与 TypeScript SDK）——只需要 Stage 2 级别的 API 稳定性。
- Stage 6 的合规认证路径——准备周期以季度计，必须尽早启动而不是 GA 前补。
- Stage 10 的 FOCUS 列名对齐决策——纯契约形状决策，改动极小，但必须赶在 Stage 7 usage/cost
  契约冻结之前完成，晚了就是对外 breaking change（见上表硬门禁）。

**排期与优先级的区别**：Stage 编号表达依赖与主题归属，不表达优先级。若资源有限，优先级排序是
Stage 5（安全与可靠性下限）> Stage 9 自动化回归（止损产品分叉）> Stage 6（可正式售卖）>
Stage 7 > Stage 8 > Stage 9 其余 > Stage 10 其余（成本结构优化，价值随真实用量增长；唯 FOCUS
命名决策例外，须提前）。

### Stage 3：Provider Runtime 与远程 Worker 产品化

状态：COMPLETE。Runtime 发布源码固定为 `8415efa15cebc48a23723dbdb147d3bafd7071bf`；本地验证环境、临时输出和
最终生成报告已按操作人要求清理。

验收边界：远程 Agent 的正式路径是第三方 API Key、可选 Base URL 和自定义模型。每个 Provider Adapter
保留契约、产品路径和可控故障验证；耗额度的长时间 load/soak、多节点与 immutable rollout 只要求一个
代表性 API-key Provider 通过。订阅/OAuth 登录属于低优先级兼容项，延期到 Stage 3 之后，不阻断本阶段发布。

#### 目标

- Control Plane 只依赖稳定的 Worker/Provider Host Contract，不依赖 Provider SDK 细节。
- 所有正式支持的 Provider 在 Local、SSH、Docker、Kubernetes Target 中具有一致的核心行为。
- TypeScript 本地 Orchestration 不再与 Go Agent Session 同时充当 Control Plane 权威状态。
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
- [x] 建立应用级 Control Plane Context 和 Session Projection Adapter。
- [x] 将主聊天创建 Project/Session/Turn 的权威写入切换到 Go Control Plane。
- [x] 保留未配置 Control Plane 时的本地个人模式，避免维护两套领域模型。
- [x] 为 Local、SSH、Docker、Kubernetes 分别建立相同的 Provider Acceptance Suite。
- [x] 验证 Worker 崩溃、网络中断、Provider 崩溃和控制面滚动升级后的 Session 连续性。

#### 完成条件

- [x] 所有正式支持 Provider 的核心能力矩阵有自动化验证。
- [x] Web 主流程只存在一个 Control Plane Session 权威来源。
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
      的追加 API 还要求显式 platform tariff-operator Tenant，不能由普通租户 `cost.manage` 越权改写；PG/SQLite
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
      终止点=tariff end Request、并发首次写及四条 DB negative gate。platform cost-accounting operator 现可通过原子 API
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

### Stage 6：企业 Self-hosted GA、运营、安全与成本治理

状态：IN PROGRESS。本节于 2026-07-27 重做：原版写于 Stage 5/7/8 存在之前，是 28 条平铺清单，既未核对
仓库现状，也与新阶段多处重叠。本次补齐现状基线、标注跨阶段归属、按主题分组，并补入同类产品
已成常识而本路线缺失的能力。

现状基线（避免重复实现已有模块）：OIDC、SAML、SCIM、Service Account、Audit、Quota、Retention、
KMS 与基础 Observability 已存在；Stage 4 已落地 requested-resource-seconds 成本代理、versioned
tariff、三云成本账单只读导入与幂等对账、shared Target 成本分摊。因此本阶段的成本工作是**内部用量与
成本治理收口**，不是从零建计量，也不建立支付或对外结算产品面。

2026-07-27 初始逐条核查后，下列条目已在正文标注"现状核查"，指出其中哪些子项**已有 API 实现**、
真实缺口是什么——按原措辞执行会重建已有模块。初始确认的缺失项包括 Legal Hold、Domain Verification、
SSO Enforcement、Plan/Entitlement/Feature Flag、离职回收闭环、分布式 Tracing、用户自助数据导出与
单用户删除、隐私请求（DSAR）受理流程；截至 2026-07-30，这些工程缺口已在下文对应条目中实现并记录
技术验证。组织级 Legal/Privacy、审计认证、真实生产 SLO/恢复、内部 Status Board、渗透/长稳与 GA 审批仍是开放门禁。

**产品边界更新（2026-08-02）**：当前定位为企业内部平台，不向外部客户提供付费服务。Stage 6 暂不做
支付、Checkout、税务、退款、dunning 或外部订阅结算；“商业化”按内部成本中心/部门分摊、Provider 使用授权
与成本治理验收。运行配置固定为 `internal-self-hosted`，拒绝启用 Stripe；支付实现不得进入 UI 或成为当前 GA
候选的必需证据。

跨阶段归属（本阶段不重复实现，只做集成与验收）：

- API Key / Service Account 的资源级 scope 与生命周期归 **Stage 7**；本阶段只负责其在企业管理
  后台的可视化与治理。
- API 文档与开发者文档站归 **Stage 7**；本阶段只负责用户文档、管理员文档、部署与排障文档。
- 跨 Tenant 越权、SSRF、容器逃逸的**修复**归 **Stage 5**；本阶段只负责第三方渗透验收，不接受
  用本阶段测试替代 Stage 5 的修复。
- 协作内容的保留、导出与 Legal Hold 边界由 **Stage 8** 接入本阶段的数据治理策略，不建旁路。
- Standard/Enterprise entitlement profile 的配额形状由本阶段落地；evaluation 仅是内部启用前评估状态，
  不构成免费层、试用或对外订阅产品。

#### 目标

- 产品可在企业内部 self-host 环境正式运行，不依赖人工数据库操作、开发模式配置或支付系统。
- Tenant 生命周期、用户生命周期、用量、成本、配额、安全、审计和支持流程形成闭环。
- 建立明确的 SLO、发布、备份恢复、安全响应和数据治理制度。
- 用户能自己解释"这次花了多少、为什么"，而不是只有平台方看得到成本。

#### TODO — 租户与身份生命周期

- [x] 完成 Tenant 注册、试用、启用、暂停、关闭、删除和恢复状态机。Migration `000095` 冻结
      数据库兼容状态 `trialing | active | suspended | closed | deleting`、生命周期版本与时间戳形状；公开 API/UI
      使用 `evaluation | active | suspended | closed | deleting`；转换、版本冲突、
      活跃 Execution/Workspace cleanup 门禁、删除恢复与审计均由服务端权威执行。试用到期在 Login、
      SSO、Turn/高级操作与 Worker Claim 路径 fail closed。SQLite 聚焦矩阵与一次性 PostgreSQL 16
      双连接竞争（恰好一个成功、一个 version conflict）已通过。Web Settings 现暴露 owner-only 的版本化
      activate/suspend/close 与关闭后删除请求；独立 `GET /v1/tenants/deletion-requests` 只列出当前 Owner 的
      soft-deleted Tenant，使刷新页面或失去 active Tenant 后仍可经 UI 恢复为 closed，不再依赖数据库或记忆 ID。
      注册路径也已分权：普通已登录用户只能经 Web/`POST /v1/tenants` 创建 active Standard Tenant，服务端拒绝
      client-supplied Enterprise/evaluation；Platform Operator Tenant 的 Owner/Admin 可经独立 Platform UI/API 为已有
      active internal account 开通 Standard/Enterprise 或 evaluation Tenant，且 operator 不获得 Tenant Membership。两条
      路径均原子创建 root Organization、内部 entitlement assignment、Owner Membership 与 Audit。
- [x] 完成企业邀请、OIDC/SAML、SCIM、Group Mapping 和离职回收闭环。**现状核查（2026-07-27）**：
      邀请（`POST /v1/tenants/{id}/invitations`、`POST /v1/invitations/{token}/accept`）、SSO 登录、
      SCIM、Group Mapping 的 API 均已存在；实际缺口是**离职回收闭环**——从 IdP 侧停用到平台侧
      Session/Credential/Grant 全链路回收的端到端保证。Migration `000096` 后，SCIM 停用会在同一
      事务暂停 Tenant/Organization Membership、撤销该 Tenant Login Session、撤销并取消自动选择
      用户 BYOK；不可变 Execution Grant 的每次 resolve/refresh 仍重查 Membership 与 Credential，
      因而不能复活。人工成员暂停/移除现与 SCIM 共用 `internal/tenantuseraccess` offboarding 原语，避免管理后台
      成为较弱旁路；重新启用只恢复 Tenant Membership，不静默恢复 Organization 或 Credential。SSO 登录也
      不再自动恢复 suspended Membership，只能由 IdP/SCIM active 更新恢复。
- [x] 支持多个 Identity Connection、Domain Verification 和 SSO Enforcement。**现状核查**：多
      Identity Connection 的增删禁用 API 已存在；`domainVerification` 与 `ssoEnforcement` 全仓库
      无实现，是本条的真实缺口。Migration `000096` 与 Identity API 已增加只存哈希的 DNS TXT challenge、
      全局唯一活动 Domain claim、验证/撤销审计，以及 versioned `optional | required` SSO policy。
      `required` 必须同时具有 verified Domain、active Connection 与 recovery Owner；启用时撤销非 SSO
      Session，且不能撤销最后 Domain 或禁用最后 Connection。SQLite 产品路径与一次性 PostgreSQL 16
      数据库约束均已通过。Tenant Identity 设置页已提供 Domain challenge/verify/revoke 与 versioned
      `optional | required` 策略，读者角色只读，Owner/Admin 写操作仍由 typed client 与服务端 RBAC 双重约束。
- [x] 评估是否需要自定义 Role；当前不引入，冻结固定 RBAC v1。Tenant Role 保持
      `owner | admin | security_admin | cost_admin | auditor | member`，Organization Role 保持
      `owner | admin | agent_operator | member | viewer`；Stage 7 API Key 使用独立资源 scope，不允许
      自定义 Role 绕过这套服务端权限矩阵。若未来引入自定义 Role，必须作为 versioned policy language
      单独设计和迁移，不能把自由字符串塞入现有 role 字段。
- [x] 完成 User、Service Account、Credential 的生命周期管理后台与治理视图；API Token 的资源级实体/scope
      仍按边界归 Stage 7。成员页现支持固定 RBAC 调岗、暂停/重启、Tenant Session 主动撤销与受约束移除；人工
      暂停和 SCIM 共用事务化 offboarding，撤销用户 Credential 与自动选择。Service Account 支持一次显示 token、
      rotate/revoke；Credential 支持 user/organization/tenant/platform scope、rotate/revoke/expiry 与 selector。
      自操作被 UI 拒绝，Owner 管理保持 owner-only，数据库最后 Owner/依赖约束继续 fail closed。
- [x] 实现 Provider BYOK、企业统一 Credential 和用户 Credential 的策略与优先级。显式 Session binding 永不
      fallback；自动选择固定为 `user > organization > tenant > platform`，同一优先级多匹配会拒绝并要求显式
      选择。User scope 每次重查 active Membership，Tenant 可按 Organization/Model selector；Platform fallback
      同时要求 enterprise deployment、enterprise Plan、Tenant enable 与独立 auto-select opt-in。Settings 已提供
      scope、selector、显式/自动策略及 Platform policy 管理，resolver/Grant 测试覆盖歧义、停用与 entitlement 丢失。
- [x] 完成 Credential Rotation、KMS Key Rotation、Re-encryption 和应急撤销 Runbook。Credential 本身已有
      versioned rotate/revoke；Migration `000105`、`internal/kmsrotation` 与 `control-plane-metadata rewrap-kms`
      实现 Provider Credential、OIDC/SAML Secret 和 Login Attempt 的在线 envelope rewrap，只更换 wrapped
      data key，不改业务密文或版本。Migration `000106`、`internal/runtimesecretrotation` 与
      `rekey-runtime-secrets` 覆盖 Provider Cursor/Execution Target runtime secret 的具名 Key ID、兼容读取、CAS
      重加密、逐 Tenant Audit、不可变 entry/receipt、断点续跑与最终零旧 Key inventory。两阶段 keyring、
      应急 revoke、证书/Domain 联动与回滚停止条件统一记录在
      `docs/runbooks/production-secret-certificate-domain-rotation.md`；SQLite 与 PostgreSQL 16 产品路径已验证。
      每个真实生产环境的轮换演练仍由后文独立 GA Gate 验收，不反向取消本项的工程闭环。

#### TODO — Entitlement、配额与内部成本治理

- [x] 建立 Tenant Entitlement Profile、Quota 和 Feature Flag 模型。**现状核查**：Quota 已有
      `GET|PUT /v1/tenants/{id}/quota` 与 `internal/quotas`；Entitlement Profile、Feature Flag 均无
      实现，是本条的真实缺口。Migration `000097` 已冻结历史 profile code、typed Entitlement assignment、
      Feature Flag 与带过期时间的 Tenant Override；`internal/entitlements` 提供租户读模型和 Platform
      Authority 的版本化 profile/Flag 变更，且 PostgreSQL
      16 双连接竞争验证为恰好一个成功、一个 version conflict。现有 Quota 继续作为执行准入权威，
      没有复制第二套并发配额机制。Platform Admin UI/API 现以 entitlement profile version CAS 执行
      Standard/Enterprise active/evaluation 变更并写 Audit；数据库保留的历史 Subscription 表只承载兼容状态，
      暂停/关闭继续走 Tenant lifecycle，避免第二套状态冒充执行门禁。
- [x] 记录 Token、Execution Time、CPU、Memory、Storage、Network 和 Provider Cost 用量（Stage 4
      已有 requested-resource-seconds、Artifact bytes 与云账单导入，本条聚焦缺口：Token 与 Network 维度）。
      Migration `000098` 将 input/cached/output/reasoning/total token、duration、ingress/egress bytes、Provider
      cost/currency 按 Tenant/Session/Turn/Execution/Generation 持久化；Runtime Event 投影使用单调 max 防重放，
      agentd usage report 以 report sequence/CAS 拒绝计数回退。Stage 4 的 Worker/Pod requested CPU/Memory/
      ephemeral-storage seconds、Artifact ready bytes 与云实际/估算成本保持各自权威，不伪装成同一来源。
- [x] 实现用量聚合、reporting period、超限行为和管理员报表。`internal/usage` 以 versioned entitlement assignment 的
      reporting period 聚合 Token、Network、Execution seconds 与按 currency Provider cost，关联 Stage 4
      shared charge slice；Migration `000099` 将 80%/100% 阈值做成 period/metric 唯一的持久告警，明确
      `enforcement=soft`、`hardStop=false`，已有硬并发 Quota 仍是 admission 权威。Tenant Settings 提供当前
      周期管理员报表，Session/Turn 页面提供用户级解释，不再依赖数据库查询。当前周期聚合不再把历史 Usage
      Generation 与可变的 Execution 当前 Generation 等值连接；恢复推进 Generation 后，历史 Token、Network、
      Provider cost 与内部成本归属仍保留。SQLite 回归和真实 PostgreSQL Compose 的 generation 恢复链路均已通过，
      证据见 [`stage-6-self-hosted-usage-failure-acceptance-20260802.md`](docs/reports/stage-6-self-hosted-usage-failure-acceptance-20260802.md)。
- [x] 完成内部成本中心/部门分摊闭环；当前不接 Billing Provider 或支付。Provider cost、共享 Target 估算分摊与
      已导入云账单的实际分摊必须按 Tenant/周期可解释，实际分摊存在时替代对应估算而不能重复相加，并能导出给
      企业既有财务流程。Session/Tenant 投影已实现实际分摊优先于对应估算、来源标记与已知多币种小计，并在
      SQLite/PostgreSQL 17 验证不重复计数。运行配置固定 `internal-self-hosted`、强制 payment provider disabled，
      Platform Profile 不暴露支付 capability，Tenant UI 不挂载支付组件，标准 Compose/Kubernetes 也不注入 Stripe
      配置或 Secret。历史 Migration `000109`/`000131`/`000132` 仅为只读迁移兼容保留；Migration `000160`
      自动撤销历史 active Billing-exercise authority、保留 revoked 审计记录并禁止新建。Migration `000159`、
      `internal/usage/cost_allocation.go` 与 `/cost-accounting/report|export.csv|projects/{projectID}` 已补齐 Project →
      成本中心/部门的版本化 CAS 归属、未分摊显式标记、Token/Network/Provider/平台成本周期报表、按 Project/币种
      分行且不重复用量的 CSV，以及归属/导出 Audit。Tenant Usage UI 支持 `cost_admin` 分配与下载；SQLite 和
      PostgreSQL 17 共用端到端用例验证 actual 替代 estimate、已知成本 24,000 micros、不完整 Provider coverage、
      stale version 拒绝和 CSV 24 列形状。Candidate/Release 已由 Migration `000152`/`000153` 改为内部成本门禁。
      `stage6:cost:prepare` 进一步从只含相对路径的 draft 稳定读取四类内部证据，自动计算摘要并拒绝 symlink、
      重复 JSON、明显凭据、payment 语义、coverage/多币种算术漂移；完整验证后才以 `0600` 原子发布
      manifest/receipt 四文件，第二对失败会回滚第一对。该闭环证明产品工程能力，不宣称企业外部财务系统已实际
      导入 CSV，也不替代 Operations 与成本 Owner 对真实来源和报告周期完整性的审批。
- [x] 【新增】实现面向最终用户的用量与成本可见性：per-Session / per-Turn 的 token、执行时长与
      估算成本，可回答"这次为什么贵"。Stage 4 建的是平台侧成本权威，用户侧可见性是另一件事，
      当前完全缺失；同类 agent 产品已把它当作基础能力，缺失会直接导致用量投诉与信任流失。
      Migration `000098` 与 Runtime Event/Worker report 投影记录每个 Execution generation 的 token、
      时长、Provider cost 与 Network counters；`GET /v1/sessions/{id}/usage` 将其与 Stage 4 的 shared
      cost slice 关联。Web Environment 面板按 Turn 汇总重试/generation，展示 token、时长、Provider
      与已分摊平台成本；当前解释投影进一步分列 Provider/平台 charge、多币种总额、charge kind、Network
      ingress/egress 和非 final 状态，Session 行按相同口径汇总且不跨币种相加；Tenant 设置页同时展示当前 reporting period 总量。
      Migration `000150` 进一步以单调 `provider_cost_reported` 区分 Provider 明确报告的零成本与价格缺失；历史
      正成本回填为 reported，历史零值保持 unavailable，Session/Turn/Tenant 聚合排除未知金额并显示缺失数量。
- [x] 【新增】实现软性配额预警：接近上限时先告警与降级提示，而非直接硬停。硬停无预警是自助
      产品最常见的差评来源，且与 Stage 7 免费层的低门槛注册叠加后影响面更大。Migration `000099`
      把 Standard profile 的 20 execution-hours/月与 80% warning 冻结为 Entitlement，并持久化 80%/100%
      语义唯一告警；读模型明确返回 `enforcement=soft`、`hardStop=false` 与降级建议，不进入 Execution
      admission。SQLite 阈值矩阵及 PostgreSQL 16 双连接并发投影（最终恰好两条阈值告警）已通过。

#### TODO — 运营与支持

- [ ] 按 ADR 0004 建立共享 Tenant 管理面与独立 `apps/admin` Platform 后台。原先把客户 Tenant 设置和
      Platform Tenant overview/四眼 Support Access 放在同一聚合页的实现不再算完成证据。Phase A 已建立
      `apps/web/src/features/enterprise` 静态注册边界，把客户入口拆为 Organization overview、Members & roles、
      Identity & access、Credentials、Usage & limits、Data & compliance、Support 七个普通 Settings 目的地；
      目的地在 Control Plane 返回认证上下文后按 capability 过滤，本地模式不注册 Cloud Panel 页面，旧
      `?section=tenancy` 兼容跳到 overview。客户组合已不再挂载 Platform 操作组件。Phase B 也已将 typed
      models、HTTP/cookie transport、稳定错误、Artifact helper 与可续传 SSE 迁到
      `packages/control-plane-client`；Web 消费方只引用 `@synara/control-plane-client` 公开 API，Desktop 的
      私有 URL bridge 通过 host resolver 注入，HTTP(S) 浏览器仍强制同源。Phase C 也已将完整 Tenant
      destination renderer、登录/上下文呈现、capability 解析及身份、凭证、生命周期、用量与合规组件迁入
      `@synara/enterprise-ui`；Web 只通过显式 Control Plane runtime、设计系统与两个 Overview extension slots
      接入，不存在 Web route、Electron 或应用 singleton 私有依赖。企业 UI 包 22 文件/59 测试与 ESM/CJS/d.ts
      构建、Web 301 文件/3502 测试、Chromium narrow/density/keyboard/reduced-motion 4 条用例及隔离七目的地
      浏览器验收通过；真实 Session 还验证并修复 lifecycle version/时间戳投影，Owner 写控件不再误禁用。
      Phase D 已新增独立 `apps/admin`：独立 SSO/Operator Tenant 门禁、常驻 Platform authority/Tenant context、
      Owner/Admin/Security 权限面、Tenant triage/provisioning/versioned entitlement/四眼 Support Access 已挂载；
      Compose/Kustomize 与兼容矩阵已纳入 Admin，旧 Web 停放组件已删除。隔离真实 Owner、第二 Admin、Security、
      support*readonly 会话验证了成功、四眼、403 写拒绝与 Audit request ID。Phase E 的单次短时、
      device-bound Desktop Enrollment 已完成 Migration `000108`、服务端原子生命周期、Admin 自助签发/撤销、
      Electron allowlist Deep Link、OS 凭据适配、完整 hydration 后提交及 rollback/disconnect；SQLite 与真实
      PostgreSQL 双连接兑换/rotation 竞争通过。macOS arm64 最终本地包又完成 LaunchServices 协议唤起、allowlist
      拒绝、Keychain-backed 密文、共享 Enterprise UI hydration、重启 rotation、UI disconnect、可信 CORS 与双向
      自动重载，且保留本地 SQLite；x64/Rosetta 本地安装态也跑通同一连接闭环。后续修复了无连接启动提前打开
      `safeStorage`、x64/Mach-O 架构名映射和跨架构 staging 沿用构建机原生依赖的问题；原生 arm64 build 10 已
      安装到 `~/Applications/Synara.app`，隔离 HOME 与实际安装启动均不再出现钥匙串或 Intel 兼容提示。边界报告为
      `docs/reports/stage-6-desktop-macos-arm64-local-acceptance-20260731.md`、
      `docs/reports/stage-6-desktop-macos-x64-rosetta-local-acceptance-20260731.md` 与
      `docs/reports/stage-6-desktop-macos-arm64-local-install-fix-20260731.md`。但这些 app 仅用 Apple Development
      签名，Gatekeeper 拒绝且无 stapled ticket；Rosetta 也不等于原生 Intel 主机。Developer ID 签名/公证的
      macOS arm64/x64 以及 Windows/Linux 协议唤起、真实 OS Credential Store 与候选部署端到端验收仍未完成，
      因此整条 ADR 项继续保持未勾选，不能把本地开发签名证据冒充四目标候选发布验收。机器合同与运行手册
      已新增 `desktop-installed-acceptance-v1.md` / `desktop-native-release-acceptance.md`；部署态校验器直接读取四份
      workflow provenance，要求原生 host、Developer ID+公证/Authenticode/CI attestation、系统凭据、完整
      Enrollment 与 Secret scan、Engineering/Security/Release 三方审批。macOS signed provenance 现还会重新展开
      update ZIP、只读挂载最终 DMG，要求两侧唯一真实 `Synara.app` 的全部 Mach-O 都满足目标架构、deep signing
      与同一 Team ID。当前没有可提交的合格四平台 manifest。
      Desktop 运行模式也已从“无连接即默认本地”改为显式持久化状态机：全新 profile 在启动 backend 前选择
      local/Cloud Panel；不存在 mode 记录时，即使残留受保护 Cloud 连接、legacy local SQLite 或显式 Enrollment
      Link 也不得替用户推断模式。非首次只按持久化 mode 恢复；local 收到 Enrollment Link 时必须再次确认切换。
      Cloud→local 停止远程注入但不删除凭据，local→Cloud 重新 hydration/rotation，只有 disconnect 才撤销设备。
      四平台 validator 已要求首次选择、双向切换、两种模式重启恢复、local 零 Cloud 流量与凭据保留；其
      Enrollment evidence 不再只是任意哈希文件，而必须使用 `synara.stage6-desktop-enrollment-runtime-evidence.v1`
      绑定候选/环境/URL/runner/原生目标与时间闭包，并由六段有序转换、请求计数和凭据摘要连续性派生模式结果，
      外层 manifest 与结构化收据漂移会直接失败。本地 arm64 Electron 首启/Local 重启恢复的运行态边界报告见
      `docs/reports/stage-6-desktop-connection-mode-local-runtime-20260801.md`，仍不替代四平台签名候选验收。
      当前安装到 `/Applications` 的 Apple Development `.12` arm64 包也已用空 `SYNARA_HOME` 实测：选择前 backend
      不启动，Local 选择写入 `0600` 状态且不创建 Cloud 连接文件，重启直接恢复 Local；DMG 内 ASAR 与安装态逐字
      一致、26 个 Mach-O 均为 arm64 且 Team ID 一致。证据见
      `docs/reports/stage-6-desktop-connection-mode-installed-local-20260802.md`。该包仍被 Gatekeeper 拒绝且无 stapled
      ticket，因此只增强本地安装态证据，不改变 Developer ID/公证及四平台候选门禁。
      Signed macOS 构建现又在 staging 前执行无泄露分发预检：要求 Developer ID credential source、密码、Apple
      API key path/ID/issuer 与 Team ID，拒绝缺失/格式错误、symlink、非当前用户所有或 group/other 可读的公证
      私钥；release workflow 从创建 `.p8` 起即使用 `0600`，预检 JSON 只输出结构布尔值。该预检不解析
      `CSC_LINK` 内证书，也不替代构建后的 Developer ID/Gatekeeper/公证/架构/Team ID 验证。
      Candidate v5 现在还会独立重验上述四平台投影并派生 `desktopEnrollmentEvidenceSetSha256`；受保护发布侧要求
      该集合摘要非零，因此旧形状或只伪造顶层 eligible 的 Desktop receipt 不能进入 Environment 审批。
      `stage6:desktop:enrollment:prepare` 已把 native harness capture 收口为经同一语义校验、URL 归一化、`0600`
      且不可覆盖的 evidence/sidecar，失败不留下半发布文件；真实四平台 harness 仍需在签名候选上执行。
      `stage6:desktop:native:prepare` 现从 path-only draft 稳定读取并 secret-scan 四平台 35 份附件，自动派生全部
      引用摘要，在同一份字节上完成 provenance/attestation/Enrollment 语义校验，并以不可覆盖的 `0600`
      manifest/receipt+sidecar 发布；重复 JSON、symlink、超限输入、摘要后换包及半发布均 fail-closed。
      该工具只消除了人工拼 manifest 的证据完整性缺口，仍没有生成 Developer ID+公证、原生 Intel、Windows
      Authenticode 或 Linux CI attestation 候选，故四平台 Desktop GA 项保持未完成。
      当前产品定位也已从用户可见 `SaaS` 术语统一为 Local / Cloud Panel / Control Plane：独立 Admin 与共享
      Tenant UI 的真实 Chromium 验收确认不存在 Payments/Billing exercises 入口，内部成本审查明确不含 payment、
      invoice 或 Checkout state，390×844 无横向溢出且控制台无 error/warn。部署配置由
      `SYNARA*BILLING\**`迁移为`SYNARA*COST\*ACCOUNTING\**`，旧名启动即返回明确替代项；Compose、Kubernetes
 ConfigMap、运行合同与校验器使用同一名称。当前 `/cost-accounting/\_`Problem Code 使用
`cost*accounting*\_`，新审计 action/resource 使用 `cost*accounting.*`/`cost*accounting\*_`；历史表名、Go 包名
 与既有 `billing._`审计行仅作为不可改写的兼容证据保留。Kubernetes base 继续要求`synara-control-plane`Workload
 Identity 并显式禁止 Artifact static access/secret key。上述是本地 production-like 源码与浏览器证据，
      不替代正式候选、真实 SSO、多角色部署态 49 项演练或外部签署。
      路由安全扫描又把 payment-path 禁止项从固定字符串扩大为 `/billing`、`/commercial-billing`、`/checkout`、
      `/payment(s)`、`/stripe` 路径模式；`/cost-accounting/_/actual-invoices/_` 明确保留为内部成本事实导入，
      不属于支付收款能力。
      self-hosted boundary validator 现在还扫描所有非测试 Control Plane Go 源文件：历史支付类型/表名只能出现在
      SQLite 锁定、治理权限清理和持久化兼容 seam，新的活动服务引用会 fail closed；这保留升级历史可读性但不让
      Checkout/Provider event/Billing exercise 重新成为运行时能力。
      Desktop 断开审计理由与设置搜索入口也已统一为 Cloud Panel / Usage & internal cost，避免用户可见语义回到 SaaS
      或 billing；内部 IPC 兼容标识保持不变。
      租户用量页也已从`Plan usage`收口为`Usage & internal cost`，软配额建议只允许请求内部管理员调整
 entitlement profile；Platform Admin 的筛选、provisioning、生命周期和 entitlement 表单把保留的
 数据库保留 `free | enterprise | trialing` 兼容码，公开 API/UI 只使用 Standard/Enterprise profile 与
      evaluation，并不再出现升级 Plan 或 Subscription 定位。真实 Chromium 已完成内部 Tenant 创建 → Entitlements
      交互，页面/控制台通过；该本地
      bootstrap 演练仍不替代真实身份源或完整候选运维演练。
- [x] 收紧 Artifact 对象存储的源码与单节点部署边界。Enterprise endpoint 只接受无凭据 HTTPS origin，远程
      presign 最长 15 分钟，静态 key 必须是带 session token 的临时凭据；Kubernetes base 不携带 Artifact key，
      生产 overlay 必须把 `synara-control-plane` ServiceAccount 绑定到 `tenants/*` bucket scope 的 workload identity。
      Compose 用 pinned MinIO client bootstrap 独立非 root 用户、private bucket、精确 CORS 与最小 policy；本地运行
      已通过 presigned lifecycle，并拒绝前缀外写入、建桶和 Admin API。静态收据仍明确为
      `artifact-storage-deployment-wiring-validated-not-cloud-iam-accepted`，候选云 IAM/KMS/Region/Network 仍是外部门禁。
- [x] 实现 Worker、Execution、Queue、Artifact、Credential 和 Identity Connection 运维视图。
      `GET /v1/platform/tenants` 每 30 秒生成租户级运维快照：Target/Worker 与离线数、Execution 活跃/
      排队/最老等待/24 小时失败、Artifact 数量/待上传/容量、Credential active/unavailable、Identity
      Connection active/disabled；平台页直接展示这些信号，需查看租户内详情时必须走只读 Support Access。
      Web capability 已拆分 `canReadCredentials` 与 `canManageCredentials`：`support_readonly` 可查看脱敏 Credential
      scope/version/expiry/状态与 Platform policy，但不会渲染 create/rotate/revoke/auto-select 写控件。
      Delivery Outbox 也已接入 Settings：只返回 topic/key/status/attempt/时间戳的脱敏 API，隐藏 payload/header/
      原始错误；Support 只读，Owner/Admin 才能执行逐条受审计 dead-letter replay。
      `docs/release-matrices/stage-6-operations-ui-v1.json` v2 与校验器固定 49 项日常操作的 UI → typed client →
      authenticated route 链路，并额外验证产品 surface/host seam：20 项 Tenant Web 与 29 项 Platform Admin
      操作均已 `reachable`，旧 Web 死代码组件已删除。隔离 Control Plane 的 Admin 实测完成 internal Tenant
      provisioning、entitlement profile version 2 写入、Owner 请求/另一 Admin 批准、Security 写入口缺失与直接 403、
      support_readonly 写阻断，并关联 Audit request ID；源码收据仍固定为 not-operations-passed，不替代候选部署
      的 49 项全矩阵与签署证据。部署态 `validate_operations_exercise_evidence.py` 已把候选 digest/origin、
      十一个分离角色、逐行正向/固定负向浏览器证据、全局唯一 request ID、Support 四眼/只读/撤销/Audit 与
      Operations/Security 双审批冻结为 fail-closed manifest；失败或使用 CLI/数据库/开发者工具的行只会得到
      ineligible receipt。`stage6:operations:prepare` 现将运营 draft 中 99 份相对证据路径自动收口为稳定、限大小、
      拒绝 symlink/复用/明显凭据的 SHA-256 引用；完整验证后才以 `0600` 原子发布 manifest/receipt 四文件，第二对
      发布失败会回滚第一对。该工具不推断 SSO/MFA、浏览器动作、request ID 或审批 authority，真实
      production-like 执行仍未发生。
      SQLite 聚合夹具覆盖全部资源维度，共享数据库时间解析同时保持 PostgreSQL/SQLite 返回形态一致。
- [x] 【新增】实现受控的支持态 impersonation：支持工程师在排障时以租户视角只读访问，必须填写
      理由、有时限、全程审计、对租户管理员可见，且租户可整体关闭该能力。没有它，支持只能靠
      直连数据库（更危险）；有它但不受控则是最典型的内部越权事故来源，两者都不可接受。
      Migration `000100` 与 `internal/supportaccess` 将其实现为独立于目标 Tenant Membership 的
      `support_readonly` 授权：Tenant 默认关闭并可立即撤销，Support Engineer 提交 5 分钟至 4 小时
      请求，必须由另一名 Platform Admin 四眼批准。HTTP 中间件在每次请求前写入目标 Tenant Audit，
      并额外阻断除退出支持态/登出之外的所有非 GET 操作；Tenant 管理员可查看历史和撤销，支持人员
      的 Session 在撤销/过期后自动清除活跃 Tenant。Migration `000110` 又把每份新 Grant 绑定到不可变
      Platform Operator Tenant，并在每次目标 Tenant 授权、Tenant 列表和 Session 认证时重验 requester 仍是 active
      `owner|admin|security_admin`、Operator Tenant 仍 active；支持人员离职/降权会立即失去目标 Tenant 读取并清除旧
      Session context，历史未绑定 Grant fail closed。SQLite 全状态矩阵、HTTP 写阻断测试和 PostgreSQL 17
      双连接并发批准、authority 不可变及离职撤权均已通过；实现不伪造客户 User 身份。
- [ ] 【定位修订】建立内部 Status Board 与员工事故沟通流程（事件分级、发布时机、事后复盘范围）。
      `docs/runbooks/enterprise-incident-response.md` 已冻结 SEV-0..3、角色、15/30 分钟发布时限、更新节奏、
      复盘范围与内部组件契约；`scripts/stage6-incident/validate_incident_exercise_evidence.py` 会固定独立 Status
      Board、角色分权、primary/secondary paging、ack/首报/更新节奏、六类内部组件、员工通知投递、恢复观察与七份
      证据 hash，收据固定为 `evidence-validated-not-operations-ready`。Control Plane 现已支持严格校验的
      `SYNARA_INTERNAL_STATUS_BOARD_URL`，应用 Profile 在未配置时明确返回 `configured: false`，配置后 Web/Admin
      才显示 Internal status；该 URL 必须是不带凭据/query/fragment、且与 Control Plane/Admin 故障域分离的 HTTPS。
      Migration `000161` 将活动收据升级为 v2 内部通信词汇，同时保留历史数据库列名。
      `stage6:incident:prepare` 现从 path-only draft 稳定读取七份证据，自动计算摘要并拒绝 symlink、重复 JSON、
      超限输入和明显凭据；完整验证后才原子发布 `0600` manifest/receipt 四文件，失败演练保持为 ineligible
      receipt，第二对发布失败会回滚第一对。该工具不验证外部分页/Status Board/员工通知或人员 authority。
      当前仍没有真实部署的 Internal Status Board URL，尚未配置实际 On-call 人员/通道，也没有完成员工收到通知的
      端到端演练，因此本项保持未完成。源码投递边界现已补齐：`incident.internal-update` 不再被 database-only
      Publisher 伪标记为 published；配置独立 HTTPS relay 与专用 32-byte HMAC key 后，Control Plane 以 Outbox Message
      ID 幂等、精确字节签名和禁止 redirect 的 webhook 投递，非 2xx 进入既有 retry/dead-letter，未配置则 fail closed。
      relay 的 2xx 仍只代表耐久接收，不代表员工实际收到通知，真实演练边界不变。

#### TODO — 数据治理与合规

- [x] 建立 Audit Search、Export、Legal Hold 和保留策略（需覆盖 Stage 8 的协作内容）。**现状
      核查**：Audit Search（`GET .../audit-logs`）、Export（`GET .../audit-logs/export`）与 Retention
      Policy（`GET|PUT .../retention-policy`）的 API 均已存在；`legalHold` 全仓库无实现，是本条的
      真实缺口。Migration `000101` 与 `internal/legalholds` 已增加不可变、versioned、单向 release 的
      Tenant/User/Organization/Project/Session scope；active Hold 会在同一权威查询边界挡住 Session 归档、
      Workspace/Checkpoint 清理、Artifact 物理删除和 Tenant 删除，并写入创建/释放 Audit。SQLite
      端到端验证 Hold 前后破坏路径；PostgreSQL 16 验证 scope trigger、不可变证据与双连接并发 release
      恰好一个成功、一个 version conflict。Stage 8 新协作内容必须绑定既有 Project/Session/Artifact
      scope，不得另建旁路 retention 或 hold 表。
- [x] 完成数据导出、Tenant 删除、用户删除和隐私请求流程。Tenant 删除沿用 `deleting` 状态与
      `internal/retention` 清除链路，并受 active Legal Hold 阻断。Migration `000102` 与
      `internal/privacy` 增加 30 天到期、versioned 不可变历史的 DSAR 状态机：Subject 可自助请求并下载
      Tenant-scoped 个人数据，隐私管理员核验/批准后执行单用户擦除；active Hold、active Execution 与
      最后一个 Owner 均 fail closed。擦除删除用户 Artifact payload、撤销 Login/BYOK、移除成员关系并
      脱敏协作内容，同时显式保留不可变 Audit/Memory/Checkpoint 证据。Migration `000103` 再增加管理员
      Tenant JSON 快照导出与不可变 SHA-256/字节数/行数收据，明确排除 Credential、SSO、KMS Secret 与
      Artifact 下载 URL。SQLite 端到端覆盖导出、Hold 阻断、擦除和收据不可变；PostgreSQL 16 覆盖状态
      并发单胜者、repeatable-read Tenant 快照及收据 UPDATE/DELETE 拒绝。
- [ ] 明确 Provider 许可、账号共享、数据使用、隐私和企业合规要求。工程基线已冻结在
      `docs/contracts/provider-commercial-use-v1.md`：Hosted 禁止共享消费者登录/Session/API Key，
      `local-only` Provider 不得进入远程 Target，`experimental` 仍需显式 Target enablement；逐 Provider
      记录商业/API 凭据、训练/数据使用、DPA/Region 与下游模型边界。当前仍缺 Legal/Privacy 对签约实体、
      Order Form/DPA 和具体 Hosted 用例的审批证据，因此本项保持未完成，不能用技术 enablement 代替授权。
      Migration `000113`、`internal/providercommercial` 与 Platform Admin → Provider use 现将精确 Provider product、
      account type、contracting entity、customer BYOK/platform-managed credential scope、Region、training/data-use、
      retention、terms/agreement/DPA、prohibited use、termination runbook 与最长 180 天 review 固定为不可变授权。
      创建者不能审批，Legal/Privacy/Security/Product 四角色必须由不同 active operator append-only 决策；拒绝原子
      终止，撤销/到期立即阻断新的 Enterprise remote claim。claim 在 Worker lease/Provider Credential Grant 前重查
      exact Provider、placement/home Region、Credential Tenant/version/mode/scope；Personal/local 不启用 hosted gate，
      catalog `local-only` 仍独立 fail closed。运行手册见 `docs/runbooks/provider-commercial-authorization.md`。
      真实签约实体、协议签名和 Legal/Privacy authority 仍需外部证据，因此本项继续保持未勾选。
      Migration `000114` 又新增 Owner 管理、最长 366 天的精确职能授权；Provider commercial 的 Legal/Privacy/
      Security/Product 角色现在由服务与数据库在写决策时实时重验，不能再靠请求体自报。相同权威同时覆盖
      Release 与 Compliance 决策，运行手册见 `docs/runbooks/stage-6-governance-authority.md`。
      Migration `000140` 进一步要求所有治理职能授权绑定 exact corporate-delegation evidence 的非零
      `sha256:`；升级会原子撤销旧 active URL-only grant 并保留版本化历史，新授权缺失/伪造摘要或摘要篡改在
      服务、SQLite/PostgreSQL、typed client 与 Platform Admin 同步 fail closed。内部记录仍不验证真实雇佣、
      公司委任或外部仓库 authority。
      Migration `000136` 进一步修复“不可变记录仍指向可漂移 URL”的缺口：terms、执行协议/Order Form、DPA 与
      termination Runbook 均必须绑定非零 `sha256:`；服务、SQLite/PostgreSQL trigger、typed client 与 Platform
      Admin 同步要求并展示四份摘要，运行时 hosted claim 只接受摘要完整的 active record。升级会把旧 URL-only
      非终态记录原子转为 `revoked` 历史，不能静默沿用；document URL 或 bytes 变化都必须新建记录并重新四方审批。
      Migration `000137` 又要求 Legal/Privacy/Security/Product 每一条 approval evidence 都绑定独立非零
      `sha256:`；服务、SQLite/PostgreSQL trigger、typed client、Platform Admin、activation 与运行时 claim
      同步 fail closed，升级会撤销含旧 URL-only decision 的非终态授权，审批证据 bytes 变化也必须重建授权并
      重新四方审批。
      Migration `000138` 继续关闭 Release 与运行时授权脱节：带 `provider_commercial` impact 的候选在 draft
      创建时必须选择一个或多个 exact active Authorization ID/version；绑定不可变，缺失、到期、撤销或版本漂移
      会在服务、SQLite/PostgreSQL、readiness 与 Platform Admin 同步阻断 review 及后续正向转换，Release 的
      Privacy/Legal 决策不能再用任意商业材料替代真正授权。
      这仍只证明审批对象字节未漂移，不验证签约实体、外部仓库或 Legal/Privacy authority，因此本项继续未勾选。
- [ ] 【新增】确定合规认证路径（SOC 2 Type II / ISO 27001 择一或并行），并把证据采集自动化接到
      既有 Audit 与发布流程上。"满足合规要求"与"拿到可出示的认证"是两件事，后者是企业采购的
      硬门槛，且准备周期以季度计，必须尽早启动而不是 GA 前补。工程决策已记录在
      `docs/plans/stage-6-compliance-evidence-plan.md`：先做 SOC 2 Type II（Security + Availability +
      Confidentiality），同步 ISO 27001 control mapping；`scripts/stage6-evidence/collect_release_evidence.py`
      已能从与当前 HEAD 精确一致的 clean commit 固定 `bun.lock`、五类 self-hosted service Artifact digest、四个平台/架构
      Desktop digest、环境 ID/HTTPS origins、Region、全部 Migration checksum 与逐控制证据 hash，并拒绝任意历史
      SHA 或 symlink 证据绕过；`bun run stage6:candidate:prepare -- ...` 再将 Internal Cost、Desktop、Operations、Incident、
      Recovery、Residency、SLO、Capacity、Penetration 与 Worker supply-chain 十份收据及 source-current 兼容矩阵绑定到同一候选，机器计算
      Release Evidence、兼容矩阵与十份收据共十二份输入摘要并在发布前完成 v5 语义校验，拒绝覆盖、路径逃逸、重复输入以及 commit、环境、origin、Region、
      Migration 或 Artifact 混用。Release Evidence collector 还会在哈希后复核 HEAD/clean 状态，以不可覆盖
      `0600` manifest+sidecar 发布并在 sidecar 失败时回滚，候选根身份不能被覆盖或半发布。两层收据都明确不自动判定 passed。当前仍缺外部审计机构合同、批准 scope/观察期、控制 Owner
      与正式证据库，故认证路径尚未达到 active gate，本项保持未完成。
      Migration `000112`、`internal/compliancegovernance` 与 Platform Admin → Compliance 现已把 program scope、
      executive sponsor、auditor engagement、observation window、evidence repository/access/retention policy、七类
      control owner、append-only evidence digest/reference、独立 evidence review 及 Security/Operations/Legal-Privacy/
      Executive 四人 start-gate decision 纳入 Platform 权威和 Audit。创建者不能审批，同一人员不能占两个角色，
      evidence submitter 不能自审；`record_complete` 必须拥有七类 control、四项批准和 accepted release manifest，
      且永久返回 `record-complete-not-audit-active`。Migration `000141` 又要求 accepted manifest review 与四类
      start-gate decision 分别绑定 exact external evidence bytes 的非零 `sha256:`；旧 URL-only 行保留为 superseded
      history，但不再计入 readiness，且可由新的 byte-bound active review/decision 替换。运行手册见
      `docs/runbooks/compliance-control-evidence-governance.md`。真实 auditor authority、WORM repository enforcement、
      正式观察期和报告仍需外部完成，因此本项继续保持未勾选。
- [ ] 【新增】将数据驻留从调度能力提升为产品承诺：租户可选择 Region 并获得书面边界说明。
      Stage 4 已具备跨 Region/Cluster 路由与 evacuation 能力，但没有对租户的可承诺语义；
      欧盟与金融客户会把它作为准入条件。当前 Settings 已接入 versioned Tenant Execution Scheduling Policy
      的 Region allow-list，并可从服务端 `GET /v1/tenants/{tenantID}/data-residency-statement` 下载带 policy
      version/digest、SHA-256/字节数头和 Tenant Audit 收据的 `synara-data-residency-statement-v1`；candidate
      selection 与 placement commit 都 fail closed，home Region 更新也不能逃逸已生效边界。边界合同见
      `docs/contracts/data-residency-v1.md`。`scripts/stage6-residency/validate_data_residency_evidence.py` 已用完整
      processing-plane inventory、同 policy head 的 failover/evacuation、越界目标拒绝、三方分离审批与逐文件 hash
      生成永久非批准收据；八份附件现必须是共享 Tenant/candidate/policy subject 的严格 JSON，Annex/runtime inventory
      逐 plane 一致，browser statement、双演练及三方审批逐层绑定，并以 `evidenceSetSha256` 进入 v2 candidate bundle。
      `stage6:residency:prepare` 进一步从同一 evidence root 的 path-only draft 稳定有界读取八份最终附件，拒绝
      Secret、重复 JSON、symlink/traversal 与输出碰撞，自动生成摘要并以 `0600` 原子发布 manifest/receipt 四文件；
      receipt 发布失败会回滚 manifest，失败演练仍保留为不可评审收据。收据明确不声称已验证密码学签名或部署
      authority。但 PostgreSQL、Artifact、KMS、备份、日志、Support 与 Provider
      处理 Region 仍需逐部署签署 annex 并用真实 failover/evacuation 报告核对，所以目前只能承诺 execution
      placement，不能宣称全数据驻留，本项保持未完成。

#### TODO — 可靠性、发布与安全验收

- [x] 定义 PostgreSQL、S3、KMS、Queue 的备份、恢复和 RPO/RTO。合同
      `docs/contracts/backup-recovery-rpo-rto-v1.md` 冻结 PostgreSQL PITR（5m/60m）、对象版本副本
      （15m/120m）、cloud multi-Region KMS（0/60m）或 Vault Raft snapshot（24h/120m），以及以 PostgreSQL
      Outbox 为权威的 Queue replay（5m/60m）。它明确不把逻辑 dump、备份成功或 broker 快照当恢复证据，
      也不把不同数据类别混成一个更好看的平台 RPO。
- [ ] 定期执行数据库恢复、对象恢复和区域灾备演练。Recovery v2 收据现已绑定完整 candidate、lockfile、
      Regions、origins、全部 Artifact、Migration tail 与恢复后的 release identity，并以单一 subject 约束 Database、
      KMS、Operations、Security、Storage 五个分离角色审批及完整时间窗口；它仍明确不验证真实备份 authority、
      审批人 authority 或密码学签名，不能替代生产恢复演练。
      `stage6:recovery:prepare` 现强制采用 path-only `subject` → `final` 两阶段：先以空 approvals 冻结四组件证据
      subject，再由五份独立审批绑定该摘要，最后原子发布 `0600` manifest/结果及 sidecar；稳定有界读取、重复
      JSON、symlink/traversal、常见凭据扫描及第二对文件发布失败回滚均有回归测试，但这仍只是证据采集门禁。
      Migration `000121`、`internal/recoverygovernance` 与 Platform Admin → Recovery drills 已把 exact receipt、四组件
      重算投影、失败收据保留及五个 `recovery.*` Platform 职能审批固化为服务和 PostgreSQL/SQLite 权威；Migration
      `000143` 又要求五项审批分别绑定 exact external evidence bytes 的非零 `sha256:`，将旧 URL-only decision
      supersede、重开其曾批准的 Drill，并在 Recovery 与 Release 两层 forward gate 重验五项摘要；内部
      approved 仍不把尚未执行的真实生产恢复升级为通过，因此本项继续保持未完成。
- [x] 定义 Availability、API Latency、Execution Start Delay、Event Delay 等 SLO（交互式冷启动
      SLO 由 Stage 4 与 fast-provision 提案定义，本阶段负责对外承诺口径）。
      `docs/contracts/enterprise-service-level-objectives-v1.md` 冻结 30 天窗口、99.9% 外部 Availability、
      99%/2.5s API、99%/60s Provider-ready 与 99.9%/1s Event append；Prometheus 已增加 good ratio、样本量、
      error budget 与 5m/1h fast-burn 规则。`scripts/stage6-slo/validate_slo_evidence.py` 进一步重算预算、校验
      三 Region/30 秒探针覆盖、四类最小样本、告警/companion review 与六份证据 hash；无外部 probe/样本不足
      只能是 not-assessable，收据固定为 `evidence-validated-not-slo-passed`，不能报通过。
      `stage6:slo:prepare` 现从 path-only draft 对六份来源执行稳定有界读取、Secret/重复 JSON/路径安全检查，
      自动派生 hash，并原子发布 `0600` manifest/receipt 与 sidecar；失败窗口仍被保留，第二对发布失败会回滚
      manifest。这补齐了真实 30 天窗口的安全采集入口，但不等于已有真实生产窗口。
- [ ] 建立错误预算、告警分级、On-call 和事故响应流程。错误预算公式、25%/0% 发布限制、SEV 分级、角色、
      响应/内部通告时限与演练频率已落在 SLO 合同、`docs/runbooks/slo-error-budget-report.md` 和
      `docs/runbooks/enterprise-incident-response.md`。Migration `000118`、`internal/slogovernance` 与 Platform Admin →
      SLO windows 已将 exact receipt、四项预算投影、失败窗口、Engineering/Operations/Security/Product 分权审批及
      Audit 产品化，并在临时 PostgreSQL 17 验证并发收敛与不可变历史；Migration `000142` 又要求四项审批各自
      绑定 exact external evidence bytes 的非零 `sha256:`，将旧 URL-only 决策标记为 superseded，并把它们曾
      授权的 Window 重新打开为 `recorded`；SLO 与 Release 两层 forward gate 都重验四项 byte-bound 决策。
      当前仍缺每个生产部署的实名 primary/secondary rota、
      Paging/Status Board 配置、真实 30 天生产窗口及演练证据。SLO 与事故演练校验器只能固定这些外部证据，
      不能替代其执行，故本项保持未完成。
- [ ] 完成结构化日志、Tracing、Metrics 与 Tenant 安全边界审计。**现状核查（2026-07-27）**：
      结构化日志（slog）与 Metrics（Stage 4 已建大量 bounded 指标与滚动预聚合）已具备；
      Migration `000104` 与 `internal/tracing` 已接通 OTLP/W3C：HTTP server span 在 Execution 创建时冻结
      不可变 traceparent，Worker claim 交给 agentd 延续 `worker.execution`/`provider.run`，Provider Host 收到
      只读 `SYNARA_TRACEPARENT`；跨 Target successor 继承源 Execution parent。无 exporter 时保持 correlation，
      有 endpoint 时按 parent-based ratio export；PostgreSQL 16 与 SQLite 均验证格式/不可变边界。Trace ID
      明确不参与授权、幂等、fencing 或调度，也不得附带 Prompt/Secret/Provider payload。企业 Control Plane
      exporter 现会在启动前强制绝对 HTTPS OTLP/HTTP endpoint、完整 mTLS client identity、bounded collector Region
      与 1–90 天 retention 声明；非 Local agentd 使用独立的无凭据策略，只允许 credential-free HTTPS 或带显式端口的
      IP-loopback relay，拒绝 Header、client certificate/key 和 insecure override。URL credential/query/fragment、
      相对 certificate path、协议漂移和缺项均 fail closed。Region/retention 会成为 bounded resource metadata，但不
      冒充 collector 的实际 IAM、Network、落盘 Region 或删除执行证据。Kubernetes 主生产清单现也以默认关闭的
      ConfigMap endpoint/protocol/Region/retention 和只读 Secret mTLS volume 装配 Control Plane 策略；Stage 6 静态门禁会拒绝
      默认启用 endpoint、可写 identity、缺少数据策略或暴露 insecure/header-auth override。动态 native/Warm Worker
      Pod 与 sandbox-operator standard 模板只从 Target Namespace 按完整 Target UUID 命名、可选的 operator-owned ConfigMap 读取
      无凭据 exporter 配置，避免共享 Namespace 的 Target 共用 policy；Pod identity 会拒绝资源改名、必需引用、Header/client identity/insecure 注入；Collector identity 由 agentd 文件系统之外的 relay/service mesh 持有。
      Docker 与 SSH agentd 现从 Control Plane operator-only 根目录派生 Target UUID 子目录：Docker 只读 bind、SSH
      只传派生远端路径，agentd 在 tracing 前以白名单解析并拒绝 symlink、可写 authority、Header/insecure、冲突值和
      server-CA 路径逃逸与 client identity；Target JSON 不能选择宿主机路径。Cocoon guest 仍由其 Target-local 部署 authority 配置。当前剩余缺口是完整 Tenant 安全边界审计
      与生产 collector 的真实访问、Retention/Region 验收，因此本项保持未完成。
      `scripts/stage6-security/validate_route_auth_boundaries.py` 现会枚举 304 个 ServeMux route，确保 122 个显式
      Tenant route 全部处于 Login Session 外层，并对 Platform signature、Worker、SCIM 与 Artifact content token
      入口 fail closed；但它明确不能证明对象 ownership/SQL predicate，剩余审计矩阵见
      `docs/plans/stage-6-tenant-security-boundary-audit.md`。聚焦机器矩阵现固定 112 条高风险证据、620 个操作组、
      47 个 Go package 与 79 个测试源文件；95 条无环境依赖的本地可执行证据通过，17 条 PostgreSQL 环境门槛
      在常规收据中继续明确为 source-only。2026-08-01 新增显式 `--run-postgres-tests` 路径：要求单一
      PostgreSQL URL authority，将两套历史测试环境变量统一到同一值，逐测试使用独立且自动清理的 schema，
      并把 skip/missing/fail 全部视为失败；2026-08-02 临时 PostgreSQL 17 已统一执行 17/17 条通过且无 schema 残留。
      收据仍把数据库标记为 operator-supplied/not-attested，因此该结果不冒充生产 Tenant 安全审计。
      本轮环境、命令、矩阵摘要和证据边界记录在
      [`stage-6-tenant-isolation-local-postgres-20260802.md`](docs/reports/stage-6-tenant-isolation-local-postgres-20260802.md)。
      Prometheus 的唯一 label serializer 现也在运行时拒绝 Tenant/User/Session/Execution/Credential/Trace 等
      标识型 key、任意 `*_id|*_uuid|*_digest|*_token|*_secret` 以及 UUID、Commit/Digest、URL、Email、常见
      Credential 形状和超长值；histogram/quantile 的追加标签走同一门禁，拒绝 panic 不回显被拒值。
      身份矩阵又逐条枚举 122 条显式 Tenant route：有效 Support Grant 的 GET 可达且每次写 Audit，所有写路由
      在 handler 前返回 `support_access_read_only`；仍属于 Platform Operator Tenant、但没有 Grant 的账号在所有
      非恢复路由被清除客户上下文并返回 `tenant_not_found`，唯一 middleware 恢复例外也由真实 Owner handler 拒绝。
      Worker request receipt 现经 Migration `000133` 持久绑定 Tenant、Execution、Generation 与 Target；所有
      Execution lease 相关幂等重放会先锁定并复验这组权威字段以及 Session 当前 Target，旧 incarnation、successor
      Generation 或 Session Target 迁移后的请求不再返回历史成功响应，Workspace cleanup 等独立 fenced queue
      仍使用自己的 dispatch authority。临时 PostgreSQL 17 已执行 Migration `000133` 并分别通过 successor
      Generation 与真实 Session Target move 两条 replay 门禁；每个测试 schema 均自动清理且容器已删除。
      适用于成员离职的身份矩阵也已闭合：人工/SCIM offboarding 在同一事务撤销 Web/Desktop Session、用户
      Credential 与待兑换 Desktop Enrollment，分别写入 Audit 计数；成员重新启用不会恢复旧 Enrollment、
      Session、Organization Membership 或 Credential。跨 Tenant 的 user-global Device 注册不授予独立访问，
      只允许 Platform Admin 或用户认证后的 Disconnect 撤销，客户 Tenant offboarding 不越权修改。SQLite 产品
      路径及 PostgreSQL 17 连接态/待兑换链接用例均通过；Service Account/Worker 继续使用独立 Tenant-owned
      生命周期，Artifact content token/对象存储 presign 作为有界 capability 留在资源与对象存储审计门禁。
      2026-07-31 已另用临时 PostgreSQL 17 执行三条精确门禁并通过；该次运行同时修正 interaction 测试的合法
      时间线、确保已 sweep 的请求稳定返回 `interaction_expired`，并验证 Support authority 不可变和离职撤权。
      2026-08-01 又在精确命名并自动清理的临时 PostgreSQL 17 上执行 Migration `000112`，验证七类 Control、
      accepted release manifest、四角色并发 start-gate decision、`record_complete` 以及 Program/Evidence 防篡改。
      同日 Migration `000113` 也在独立临时 PostgreSQL 17 上验证四角色并发 Provider 商业审批、active Hosted
      fence 以及 Authorization/Approval 防篡改；常规无环境收据仍保留 source-only 标记。
      Migration `000114` 随后在四个隔离 PostgreSQL 17 数据库验证职能授权并发单胜者、撤销即时生效、Release/
      Compliance/Provider 决策数据库门禁与历史防篡改；常规矩阵把该环境测试作为第 7 条 source-only 证据。
      Migration `000115` 又在独立 PostgreSQL 17 数据库验证影响域不可变、商业影响候选五角色并发审批及
      Privacy/Legal 缺失时的数据库阻断；Migration `000116` 再要求候选保存 exact-byte v2 evidence receipt，
      服务端重算摘要并核对九类 review-ready 证据及 candidate/commit/lockfile/environment/Desktop artifact-set，
      PostgreSQL/SQLite 拒绝跨候选复用和直接篡改。该结果仍是本地数据库门禁证据，不替代真实公司审批。
      Migration `000117` 同日在临时 PostgreSQL 17 上验证事件通告证据并发串行化、固定 Status Board origin 和
      事件/内部时间线不可变；常规矩阵仍将该 PostgreSQL 行保留为 source-only。
      Migration `000118` 随后验证 exact SLO window receipt、四职能并发审批收敛与历史不可变；常规矩阵同样
      将该 PostgreSQL 行保留为 source-only。
      Migration `000119` 又在全新 PostgreSQL 17 验证完整九类投影候选可进入分权审批、空 projected control
      receipt 不能绕过服务经数据库直写；SQLite trigger 也执行同一空投影阻断。该结果仍不验证外部签名或控制执行。
      Migration `000120` 进一步要求 Release candidate 在进入 approved 前已经拥有同候选、同 Operator Tenant、
      eligible 且四职能内部批准的不可变 SLO window；服务、SQLite 与 PostgreSQL 均阻断缺失或跨候选 SLO 门禁。
      这只组合产品内审批权威，仍不替代真实 30 天生产窗口或外部 SLO 证明。
      Migration `000121` 又在全新 PostgreSQL 17 验证 exact Recovery v2 receipt 导入、五职能并发审批收敛以及
      receipt/组件/审批历史不可变；SQLite 执行相同投影与职能 authority 门禁。该结果仍不替代真实生产备份恢复。
      Migration `000122` 进一步要求 Release candidate 在进入 approved 前已经拥有同候选、同 Operator Tenant、
      eligible、四组件目标与 canary 通过、五份来源决策通过且外部权威边界未弱化的内部 approved Recovery drill；
      服务、SQLite 与全新 PostgreSQL 17 均阻断缺失、跨候选及数据库直写绕过。该门禁仍不替代真实生产恢复证明。
      Migration `000123` 又将 candidate bundle 引用的 exact Penetration receipt、Commit、环境、四类 Artifact、
      Stage 5 依赖、独立性声明、六类攻击面、方法论及 High/Critical projection 固定为服务和数据库权威；
      Engineering/Product/Security 三个不同 `penetration.*` 职能审批才能形成内部 approved。Migration `000124`
      再要求 Release candidate 消费同候选、同 Operator Tenant 的上述批准记录；服务、SQLite 与全新 PostgreSQL
      17 已验证三职能并发收敛、不可变历史，以及 Release 缺失门禁和数据库直写绕过阻断。该门禁仍明确不验证
      第三方身份、签署报告、密码学签名或真实执行。Migration `000144` 进一步要求三份内部审批各自保存所审
      外部证据精确字节的非零小写 SHA-256；历史 URL-only 决策会被 supersede，原 approved Engagement 重开，
      Release 的 approved/deploying/observing/released 状态都按三份 active byte-bound 决策 fail-closed 重验。
      Migration `000125` 继续将 exact Capacity/soak receipt、24/72 小时连续窗口、forecast+headroom、五阶段、
      五类扰动、三 Region 探针以及 SLO/饱和度/Tenant fairness 投影提升为 Platform Capacity Governance；
      Engineering/Operations 两个不同 `capacity.*` 职能审批才能形成内部 approved。Migration `000126` 要求
      Release candidate 消费同候选、同 Operator Tenant 的批准记录；服务、SQLite 与临时 PostgreSQL 17 已验证
      两职能并发收敛、不可变历史、缺失门禁和数据库直写绕过阻断。该门禁不验证真实环境、遥测、签名、
      外部审批权威或长稳执行。
      Migration `000127` 进一步把 exact Incident exercise receipt、独立 HTTPS Status Board origin、六角色分权、
      SEV paging/ack/escalation、六个内部组件、内部时间线节奏、员工通知投递、恢复观察、复盘标志以及 manifest/
      七份 evidence 引用提升为 Platform Incident Exercise Governance；Operations/Communications 两个不同的
      `incident_exercise.*` 职能审批才能形成内部 approved。Migration `000128` 要求 Release candidate 消费同候选、
      同 Operator Tenant 的上述记录；服务、SQLite 与临时 PostgreSQL 17 已验证两职能并发收敛、不可变历史、
      缺失/跨候选门禁和数据库直写绕过阻断。该门禁不验证真实 Status Board、paging、员工通知投递、签名、执行或
      外部审批权威。Migration `000146` 进一步要求 Operations/Communications 两份审批各自保存所审外部证据
      精确字节的非零小写 SHA-256；历史 URL-only 决策会被 supersede，原 approved Exercise 重开，Release 的
      approved/deploying/observing/released 状态都按两份 active byte-bound 决策 fail-closed 重验。
      Migration `000129` 又把 exact Operations browser exercise receipt、Control Plane/Web/Admin Artifact、双 HTTPS
      origin、11 个分离账号、48 项正向/固定负向角色、全局唯一 request ID、零开发工具回退、Support 四眼闭环及
      原始 Operations/Security 签署提升为 Platform Operations Exercise Governance；两个不同的
      `operations_exercise.*` 职能审批才能形成内部 approved。Migration `000130` 要求 Release candidate 消费同候选、
      同 Operator Tenant 的上述记录；服务、SQLite 与临时 PostgreSQL 17 已验证两职能并发收敛、不可变历史、
      缺失/跨候选门禁和数据库直写绕过阻断。Operations 收据自身的导入/审批属于 Release 元治理，不加入其治理的 48 项演练矩阵，
      避免收据自引用死锁；内部状态仍不验证真实部署、浏览器 Session、Audit/evidence authority、签名或执行。
      Migration `000147` 进一步要求 Operations/Security 两份审批各自保存所审外部证据精确字节的非零小写
      SHA-256；历史 URL-only 决策会被 supersede，原 approved Exercise 重开，Release 的
      approved/deploying/observing/released 状态都按两份 active byte-bound 决策 fail-closed 重验。
      历史 Migration `000131` 随后把 exact Billing exercise receipt、同候选/Migration/Artifact/origin 身份、Stripe live
      mode/API version、固定 10 场景与 24 份唯一证据、金额/税额/settlement cardinality、数据边界和来源三签署
      提升为 Platform Billing Exercise Governance；Finance/Security/Release 三个不同的 `billing_exercise.*`
      职能审批才能形成内部 approved。Migration `000132` 要求 Release candidate 消费同候选记录；服务、SQLite
      与临时 PostgreSQL 17 已验证三职能并发收敛、不可变历史、缺失/伪造/跨候选门禁和数据库直写绕过阻断。
      该门禁现仅为迁移兼容记录，不属于 `internal-self-hosted` 产品或当前 GA 证据。Candidate v5 已由
      Migration `000152`/`000153` 切换为内部用量/成本收据；Stripe 记录只允许历史 v2/v3 离线审计读取。
      Migration `000148` 进一步要求 Finance/Security/Release 三份审批各自保存所审外部证据精确字节的非零
      小写 SHA-256；历史 URL-only 决策会被 supersede，原 approved Exercise 重开，Release 的
      approved/deploying/observing/released 状态都按三份 active byte-bound 决策 fail-closed 重验。
      全部 26 个直接接受 `identity.Principal` 的生产服务包现至少有一条分类证据，未来新增未分类包会让校验器
      失败。方法级清单另枚举 175 个导出 `Service` 入口：174 个
      已映射到直接调用它们的聚焦测试，1 个已授权且持锁的内部事务 hook 以源码契约单列，未分类数为 0；该方法
      清单闭合仍不等于分支、身份、并发与部署边界的操作级穷尽。登录中间件
      在 handler/body 之前统一拒绝 121 条非恢复显式 Tenant 路由的活动上下文替换。其收据仍明确为
      not-audit-passed，尚未覆盖每个资源族/身份/部署面。
- [ ] 执行跨 Tenant 越权、SSRF、命令注入、路径穿越、供应链和容器逃逸测试（**验收 Stage 5 的
      修复成果**；Stage 5 未完成前本条不得开始，否则只会重复记录已知缺口）。验收合同与运行手册已落在
      `docs/contracts/third-party-penetration-acceptance-v1.md`、`docs/runbooks/third-party-penetration-test.md`；
      `scripts/stage6-penetration/validate_penetration_evidence.py` 会固定 Stage 5 精确部署面、独立评估方、Web/API/
      Worker/Provider Host 候选 Artifact、六类必测攻击面、发现项状态、Critical 必须复测关闭、High 最长 30 天
      双人风险接受及九份独立证据 hash。开放高危和范围缺口会被保留为失败收据，收据固定为
      `evidence-validated-not-penetration-passed`。Migration `000123`、`internal/penetrationgovernance` 与 Platform Admin →
      Penetration reviews 已把 exact receipt、候选 Artifact 投影及 Engineering/Product/Security 分权审批产品化；
      `stage6:penetration:prepare` 现从 path-only draft 对九份来源执行稳定有界读取、Secret/重复 JSON/路径安全
      检查，自动派生 hash，并原子发布 `0600` manifest/receipt 与 sidecar；开放 High 仍产生不可评审收据，第二对
      文件发布失败会回滚 manifest。这只补齐证据采集完整性，不证明第三方实际执行或签名真实性。
      Migration `000124` 同时把同候选内部 approved 状态接入 Release gate。当前仍缺真实第三方授权、执行、
      签署报告与外部 Security 评审，故本项保持未完成。
- [x] 对 Worker Image、Provider CLI、依赖和 SBOM 建立签名与漏洞扫描。复用 Stage 3 已完成的正式供应链，
      不复制第二套：Provider CLI/Claude SDK、APK、Base Image、BuildKit frontend/SBOM generator 均由 lock 或
      digest 固定；Worker manifest 产出规范化 SPDX SBOM；production profile 使用 Vault Transit KMS Cosign、
      Rekor inclusion proof 与 Kyverno fail-closed admission；Trivy 同时扫描 vuln/secret，阻断未豁免
      HIGH/CRITICAL、Secret、EOSL 和超过 24h 的 DB，例外必须有 owner/reason/expiry 且 unused/expired 也失败。
      当前聚焦回归为 registry supply-chain `49/49`、Worker image manifest `2/2` 通过。此勾选只表示机制已
      建立；每个 GA candidate 的真实双架构 digest、SBOM、扫描、签名、tlog 和 admission 证据仍必须在
      release checklist 单独通过。Stage 6 已增加 `validate_worker_supply_chain_evidence.py`，用 exact-byte Registry
      report hash、clean commit 与 Worker digest 把 release manifest、Registry gate 和 Vault/KMS admission 报告绑定，
      三份输入均以稳定有界 regular-file 读取，同一字节用于 Secret/重复 JSON 检查、语义验证和 hash，拒绝 symlink
      与读取期漂移；收据以 `evidence-validated-not-worker-supply-chain-approved` 明确保留外部 authority/人工审批边界。
      candidate bundle
      已显式升级为 v3，第十份收据同时接入受保护发布、Release Governance 与 PostgreSQL/SQLite Migration `000134`；
      v2 仅保留历史读取，不能授权新的或活动中的发布候选。Migration `000135` 又把 Final Review v1 精确收据
      作为 `observing -> released` 的强制门禁：服务与 PostgreSQL/SQLite 均要求 append-only 绑定，并逐字匹配最终
      决定摘要和规范化残余风险。数据库触发器还独立重验 32 个唯一 Control/Owner、非空 JSON array、分离角色/
      approver、时间闭包、Audit request UUID 与 evidence/risk 结构；重新计算摘要的重复 Control 直写在 SQLite 与
      临时 PostgreSQL 17 均被拒绝。SQLite 现通过连接级 deterministic `synara_sha256(blob)` 重算精确收据摘要，
      不再只检查 32 字节长度；合法绑定/发布/不可变历史链路通过。Platform Admin 的 released 转换直接只读复用
      收据绑定的决定摘要和风险清单，不再让操作者二次抄写；readiness API 在既有九项 approved 门禁之外单独返回
      `finalReviewGate` 和 `releaseTransitionEligible`，页面明确区分内部审批完成与可进入 released。内部 eligibility
      仍不冒充外部 GA authority。
- [ ] 完成 Secret 管理、生产配置、证书、域名和密钥轮换流程。工程基线已新增 Migration `000105`、
      `internal/kmsrotation` 与 `control-plane-metadata rewrap-kms`：Provider Credential、OIDC/SAML Secret 和登录
      Attempt 共享 primary+decrypt-only keyring，rewrap 只换 wrapped data key、不改业务密文/版本，并以精确
      数据库授权、逐 Tenant Audit、不可变 run/entry/receipt、断点续跑与最终零旧 Key inventory fail closed；
      Local/AWS KMS 的两阶段 rolling bridge、Secret/Worker/存储/证书/域名步骤见
      `docs/contracts/credential-kms-rotation-v1.md` 与
      `docs/runbooks/production-secret-certificate-domain-rotation.md`。Migration `000106`、具名 runtime keyring 与
      `control-plane-metadata rekey-runtime-secrets` 进一步为 Cursor v2/Target legacy 密文提供兼容读取，为新密文
      写入经认证 Key ID，并在线 CAS 重加密 usable Provider Cursor 与 Target Configuration；逐 Tenant Audit、
      platform Target 全局不可变 entry、可恢复 run、收敛 receipt 和旧 Key 缺失 fail-closed 均已覆盖。SQLite
      端到端和 PostgreSQL 16 迁移/收据封闭性实测通过。现又新增
      `production-rotation-acceptance-v2.md` 与 `validate_production_rotation_evidence.py`，把同候选/Migration/Region
      身份、Credential KEK、runtime key、Worker registration、PostgreSQL、Artifact、Incident Publisher HMAC、OTLP relay、TLS、DNS
      九类轮换，逐项 owner/approver 分离、五份唯一 evidence、新路径、旧 authority 最终状态、rollback、零风险、
      Secret scan 与 Security/Operations/Release 三方决定冻结为不可覆盖 `0600` 收据；明显 private key、live
      provider key、Bearer 和带凭据 URL 会在不回显值的前提下失败。收据固定为
      `evidence-validated-not-production-rotation-passed`，不验证外部 provider/人员/签名 authority；真实生产演练
      证据仍缺，故本项保持未完成。`stage6:rotation:prepare` 又将运营输入降为只含相对路径的 draft，自动计算
      49 份 evidence 摘要、完整校验后才原子发布 manifest/receipt 四文件；第二对发布失败会回滚第一对，避免
      人工抄 hash 或留下半套 GA 证据。历史 `billing-provider-credential` 控制已从活动 self-hosted 轮换矩阵删除，
      由 `internal-incident-publisher-hmac-key` 取代；Control Plane 现在要求 key bytes 与独立 bounded Key ID 同时
      配置，并通过 `X-Synara-Key-Id` 让 relay 在 old+new overlap 期间精确选择验证 key。Runbook 已冻结 relay
      双读、Control Plane 单写切换、新路径、rollback、旧签名拒绝与旧 Secret 退役顺序；真实 relay/生产轮换仍需执行。
- [x] 建立数据库 Migration、协议版本、Worker Image 和前端的**跨组件兼容发布矩阵**。注意去重：
      Worker Release 的 canary/promote/rollback **机制**已由 Stage 3 完成并落在
      `internal/workerreleases`（含 `auto_rollback.go`、`scheduling.go` 与对应 API 路由），
      Worker/Pod Drain 与滚动升级也已由 Stage 4 完成。本阶段**不重复实现这些机制**，只负责它们
      之间的兼容矩阵。机器基线 `docs/release-matrices/stage-6-compatibility-v1.json` 冻结 Migration `000163`
      checksum、API v1、Web/Admin/Desktop/Server/Contracts `0.6.3`、Worker Protocol 2、Runtime Event read 1..2/write 2、
      Provider Host accepted 2.1+/produced 2.2 与 Worker Manifest schema 3；
      `scripts/stage6-compatibility/validate_compatibility_matrix.py` 会直接读取源码常量、包版本和 Migration
      尾部 fail closed，并拒绝同一数字版本对应不同文件；现进一步对 matrix、8 个 package manifest、协议/API
      源码与完整 Migration lineage 做稳定有界非 symlink 读取，拒绝重复 JSON/明显凭据，并用同一捕获字节解释
      源码和计算尾部摘要，收据记录精确源文件/字节数量。并行 runtime-isolation 分支占用已进入 HEAD 的
      `000107` 后，未部署的 Desktop Enrollment 已改号为 `000108`；全新 PostgreSQL 17 数据库实际记录了
      `107=execution_runtime_isolation_decisions` 与 `108=desktop_enrollment`，Schema readiness 和 Desktop
      并发兑换/rotation 均通过；`000109=commercial_subscription_billing` 现仅作为历史迁移保留，当前
      `internal-self-hosted` runtime 不包含支付 provider、Checkout/Portal API、客户端方法、UI、监控或部署配置。
      `000154` 进一步清空 Subscription 的历史 provider 字段并将 Migration `000109` 的支付表冻结为只读；
      `000155` 将已有 `billing_admin` 归一为 `cost_admin`，`000156` 验证新角色约束。当前权限固定为
      `cost.manage`，公开内部成本核算路由固定为 `/v1/tenants/{tenantID}/cost-accounting/*`。
      `000157` 再将新的 Operations exercise 固定到 48 项 `internal-self-hosted-v4` 矩阵，把内部成本收据导入与
      分权审批纳入浏览器运营证据；历史 46 项 v3 和支付时代 v2 记录仅保留审计读取。
      `000162` 再将当前 Operations 收据升级为 v2，要求逐 Grant 撤销与系统到期两类 Audit 证据；历史收据字节
      保持不可变，但 v1 决定失去正向 Candidate/Release authority。临时 PostgreSQL 17 已验证 161→162 重开、
      supersede、v1 再审批拒绝及新 v2 双审批闭环。
      Migration `000163` 与 Candidate v5 进一步关闭“合同要求复制兼容矩阵、候选却未绑定其字节”的发布旁路：
      preparer 接受独立 compatibility matrix 输入，Python 会对捕获字节重新执行 source checker，并要求其 Migration
      tail 与 Release Evidence 一致；受保护 TypeScript、Go 服务、PostgreSQL 和 SQLite 均重验 schema/version、非零
      摘要、source inventory、唯一路径与 exact Migration tail。活动 v4 Candidate 必须关闭或重建，终态 v2-v4
      仅保留审计历史。该开发闭环仍不替代在真实完整发布中执行 previous-build forward-schema、Worker
      canary/promote/rollback 与观察窗口，因此完成条件继续保持未勾选。
      `000158` 将历史 Billing exercise 及审批在 PostgreSQL/SQLite 全部冻结为只读，并删除未注册但仍可调用的
      Go 服务包与 Stripe 收据夹具；当前源码仅保留迁移兼容读取和候选 v2/v3 离线审计解析。
      `000110=support_access_operator_authority` 再把 Support Grant 固定到 Platform Operator Tenant，旧的未绑定
      pending/active Grant 不会被授权，并已在临时 PostgreSQL 17 验证双连接批准、authority 不可变与离职撤权。
      `000111=stage6_release_governance` 固定精确发布候选与分离审批，`000112=stage6_compliance_governance`
      固定控制 Owner、证据摘要/复核和合规 start-gate；两者均已在临时 PostgreSQL 17 验证并发及不可变约束。
      `000113=provider_commercial_authorization` 再把 Hosted Provider 商业授权、四角色审批与 Enterprise remote
      claim gate 固定到同一兼容尾部。
      `000114=stage6_governance_authority` 将 Release、Compliance 与 Provider commercial 的职能角色从自报字符串
      提升为 Owner 授予、限时、可撤销且受 offboarding 实时约束的 Platform 权威。
      `000115=stage6_release_impact_scope` 再冻结发布影响域，并由服务与数据库推导条件式 Privacy/Legal 第五审批；
      商业、隐私、Residency、受监管客户或安全事件候选缺少该审批时不能进入 approved。
      `000116=stage6_release_evidence_receipt_binding` 将候选 receipt 从手工摘要升级为 exact-byte 上传与不可变数据库
      证据；摘要、schema、eligibility、九类 receipt、候选身份和 Desktop artifact-set 任一不符都不能进入 review。
      `000117=stage6_incident_governance` 再将事件 identity、角色分离、独立 Status Board origin、内部更新顺序/节奏与
      Security/Privacy 关闭审批固定为数据库权威；临时 PostgreSQL 17 已验证 Migration、并发证据串行化与历史不可变。
      `000149=stage6_incident_resolution_approval_evidence_digest` 进一步要求关闭审批绑定所审 containment/recovery
      证据精确字节的非零小写 SHA-256；URL-only 历史决策会被 supersede，已结束事件不改写，仍在 monitoring
      的事件必须补录 active byte-bound decision 才能进入 resolved。
      `000161=internal_incident_communications` 将活动 API、候选收据和发布投影切换为内部 Status Board、员工通知和
      internal-user-path 词汇；历史列名保持兼容。它只保存内部通告证据，不会代替真实部署、值班或候选环境演练。
      `000118=stage6_slo_window_governance` 将 exact 30 天 SLO receipt、四项 budget projection、失败窗口和四个分离
      职能审批固化为产品/数据库权威；它不把内部 approved 状态冒充生产 SLO claim。
      `000119=stage6_release_receipt_projection_guard` 再使九类 projected receipt、候选 Artifact/Region/origin/Migration、
      manifest/release-evidence reference、Desktop artifact-set 与 Recovery/Residency 外部权威边界由服务和数据库
      双重校验；空对象、schema 漂移、路径复用或弱化验证边界不能进入发布人工审批。该门禁不验证外部签名或真实执行。
      `000120=stage6_release_slo_approval_gate` 要求候选绑定的 eligible SLO window 已经通过四个分离职能的内部审批，
      才允许 Release Governance 进入 approved；服务和两种数据库都拒绝缺失或跨候选的 SLO 门禁，且不将其表述为生产 SLO。
      `000121=stage6_recovery_governance` 将完整 Recovery v2 receipt、恢复 identity、四组件投影和五个来源决策
      接入 Platform Recovery Governance，并要求 Database/KMS/Operations/Security/Storage 五个不同职能 authority；
      服务与两种数据库保留不可变历史，但不验证外部 backup authority、审批身份或签名。
      `000122=stage6_release_recovery_approval_gate` 要求候选绑定的 eligible Recovery drill 已经通过五个分离职能的
      内部审批，且外部权威边界保持显式，才允许 Release Governance 进入 approved；服务和两种数据库都拒绝
      缺失、跨候选或弱化边界的 Recovery 门禁，且不将其表述为真实生产恢复证明。
      `000123=stage6_penetration_governance` 将 candidate bundle 精确引用的渗透 receipt、候选 Commit/环境/四类
      Artifact、Stage 5 依赖、独立性、六类范围、方法论和发现项投影接入 Platform 权威，并要求不同的
      Engineering/Product/Security 职能审批；`000124=stage6_release_penetration_approval_gate` 再要求 Release
      approved 消费同候选的内部批准记录。两者都保留外部 assessor/report/signature/execution 边界。
      `000125=stage6_capacity_governance` 将 exact Capacity receipt 与 24/72 小时、headroom、阶段、扰动、探针、
      SLO/饱和度/公平性投影接入 Platform 权威，并要求不同的 Engineering/Operations 职能审批；
      `000126=stage6_release_capacity_approval_gate` 再要求 Release approved 消费同候选的内部批准记录。
      两者都保留真实环境、遥测、签名、执行和外部审批权威边界。Migration `000145` 进一步要求 Engineering/
      Operations 两份审批各自保存所审外部证据精确字节的非零小写 SHA-256；历史 URL-only 决策会被 supersede，
      原 approved Capacity Run 重开，Release 的 approved/deploying/observing/released 状态都按两份 active
      byte-bound 决策 fail-closed 重验。
      收据只标记 source-compatible，真实 rollout/rollback 仍由每次 GA checklist 验收。
- [x] 建立发布评审、审批与对外变更通告流程：谁批准、按哪份 checklist 门禁、如何对外公告，与
      产品内更新日志及 Stage 7 的 `Deprecation`/`Sunset` 政策共用同一发布节奏。
      `docs/runbooks/enterprise-release-governance.md` 已冻结 Release Manager、Engineering、Operations、Security、
      Product、Comms 与条件式 Privacy/Legal 的职责/分权，定义 draft→review→approved→deploying→observing→
      released/rollback 状态，复用 Stage 6 checklist/evidence、兼容矩阵与 Stage 3/4 Worker rollout。
      `stage-6-candidate-evidence-bundle-v3` 现强制十份最终外部控制收据使用同一 commit、环境 ID、origin、Region、
      Migration、lockfile 与 Artifact 集合，包含 Worker supply-chain，避免不同 RC 的成功证据被拼成一次发布结论；
      准备器先完整校验再以不可覆盖文件发布 manifest/receipt 及 sidecar，一致性收据仍不替代角色审批。
      十类外部 validator 的 receipt/sidecar 现全部为不可覆盖 `0600` 双文件；除 Billing 的等价加固实现外，其余
      九类共用同一经覆盖拒绝和 sidecar 回滚测试的 immutable I/O 模块，候选准备器不会消费半发布收据。
      Desktop Enterprise GA 模式进一步在同一 `release.yml` run 内先生成四平台签名包、等待受保护 environment
      验收审批，再发布同一批字节并创建 tag；受保护 job 会读取实际 v3 receipt、重算 hash 并校验 eligibility/
      candidate/commit/lockfile/artifact-set，孤立的合法格式 hash 不再能授权。它还读取当前 environment 与 run approval
      history，要求禁用管理员 bypass、阻止 self-review、实际 reviewer 与 workflow actor 分离、精确结构化评论且仅允许首轮 run。
      受保护侧 `stage6-candidate-release-binding.v2` 还独立重验十份 projected receipt 的 schema、固定 non-pass
      assessment、安全唯一路径、非零摘要、ready 与时间闭包，以及完整 candidate/Release Evidence/Recovery/
      Residency 结构；此前前九份投影可为空对象而只依赖顶层 eligible 的缺口已由负向及 Python→TypeScript
      跨语言契约测试关闭。
      `stage6:environment:prepare` 再把 verified receipt 与独立 candidate/commit/run ID 收口为不可覆盖的 `0600`
      配置及 sidecar，机器派生五个 Environment 变量、receipt base64 secret 和结构化审批 comment，避免人工重算/
      转录；`stage6:environment:apply` 进一步要求 GitHub Environment 已由管理员在 UI 禁用 bypass，未满足时零写入，
      并向 GitHub 重验 exact first-attempt active release.yml run、仓库、commit 与 source branch protection；分支必须
      enforce admin、dismiss stale review、至少一票审批、strict status checks 且禁止 force-push/delete。随后才以两到
      六个唯一 User/Team ID 配置 prevent-self-review 与 protected-branch-only，通过 stdin 写 secret，并回读 Environment、
      五个变量与 secret presence 后生成私有、不可覆盖的 application receipt。GitHub 不允许回读 secret 值，因此该
      收据明确只证明配置应用，不冒充环境审批或 secret 内容的远端回读证明。
      受保护 release job 不信任应用器收据，还会独立下载 exact run 与 branch protection 响应；v2 Environment approval
      与 v4 Release approval 固定 source commit/branch，并拒绝 workflow ref、仓库或分支保护漂移。
      候选 ID、commit、run ID 与 receipt 任一不符都会失败，
      四份 provenance 的稳定 artifact-set SHA 还会在受保护 environment 放行后、授权记录写入前与发布前分别
      重算，避免 build-only 验收后重建或
      同一 run 重跑得到不同签名/公证字节却沿用旧收据。
      Updater manifest 的 merge/copy/delete 也已前移到独立 `finalize_release_assets` job；canonical
      `release-final-asset-set.json` 覆盖最终公开 installer、blockmap、YAML、provenance 与 attestation，受保护审批与
      发布前均重验。Updater verifier 还把 default/channel alias、版本、架构、basename、size、SHA-512 与 Windows
      blockmap 对到真实 payload；branded raw→final verifier 要求除预定义 YAML 变换外的每个公开字节都与获批 raw
      candidate 一致，发布 job 不再在验签/审批后改写公开字节。
      Release workflow 默认 token 也已收敛为 `contents: read`：构建仅获得 attestation/OIDC 写权，只有 GitHub
      Release job 获得 `contents: write`，版本提交使用独立 Release App token；全部远程 Action 固定 full commit SHA，
      除显式 App push 外的 checkout 均不持久化凭据。
      2026-08-02 对当前 `origin` fork `hxp0618/synara` 的只读远端核查显示：repository secret 名称、变量、
      Environment 和 `release.yml` run 均为空，`main` 返回 `Branch not protected`；本地 Stage 6 分支还领先远端
      15 个 commit 且工作树未收口。因此远端当时不能生成受保护签名候选，详见
      `docs/reports/stage-6-github-release-environment-readiness-20260802.md`。该状态必须在真实 candidate 前通过明确
      release repository、clean source、branch protection、protected Environment、分离 reviewer 与签名/公证配置
      关闭，不能以本地工程门禁替代。
      2026-08-04 结构性控制已按下述工具关闭：operator 确认 `hxp0618/synara` 为发布仓库；`main` 分支保护经
      `stage6:branch-protection` 应用并回读（5 个无条件 CI required check、enforce-admins、last-push approval、
      strict checks、线性历史、禁 force-push/删除）；`stage6-enterprise-ga` Environment 经
      `stage6:environment:baseline` 达到 `environment-baseline-ready-not-release-approved`（`hxp0618` + `ameliaWiza2`
      双分离 reviewer、prevent self-review、protected-branch-only、管理员 bypass 已在 UI 关闭并回读）；
      `SYNARA_FINALIZE_RELEASE=1` 已设置。期间修复了两处工具缺陷：个人仓库发送组织专属
      `bypass_pull_request_allowances` 导致 422，以及被 GitHub 静默削减 reviewer 的漂移 Environment 无法进入
      准确的 fail-closed 拒绝路径。readiness 六项控制全部为 `true`，但 16 个签名/公证/Release App secret 与
      per-candidate Environment 值仍缺失，评估保持 `github-release-inputs-incomplete`，详见
      `docs/reports/stage-6-github-release-environment-readiness-20260804.md`。
      `stage6:github:readiness` 已把这次人工核查固化为只读机器投影：从本地 `release.yml` 提取固定 secret/variable
      名称，Secret 永远只读名称；唯一读取的值是非敏感 repository variable `SYNARA_FINALIZE_RELEASE`，且必须精确
      为 `1`，错误只报告变量名、不回显值。Windows 未签名例外按 candidate version 命中，readiness 在未知候选版本
      时不猜测遗留值是否安全，而要求 `SYNARA_ALLOW_UNSIGNED_WINDOWS_RELEASE` 完全不存在；CLI 发布开关仍可选。
      投影区分 repository 与 candidate Environment 输入，并检查管理员 bypass、
      self-review、2–6 个有效 `User|Team` reviewer、protected-branch-only 及完整 source branch protection；branch 名
      会 URL 编码，404 记为缺失而 403 保持权限错误。Organization owner 因未核对组织级 secret visibility 而
      fail closed；即使全绿也只返回 `github-release-inputs-ready-not-ga-approved`。
      `stage6:branch-protection` 又补齐该门禁此前缺少的安全配置入口：默认只生成绑定 repository、branch、精确
      HEAD 与真实 CI job 名称的 canonical plan；只有重复相同输入、显式 `--apply` 且确认 plan SHA-256 才能写入。
      写前重验 Admin 权限、HEAD 与既有策略，写后重验管理员强制、过期 review 驳回、last-push approval、strict
      checks、会话解决、线性历史及禁止 force-push/delete；404 可配置而 403 不会伪装成缺失，已合规时幂等不写。
      该工具只关闭 GitHub 分支保护配置缺口，不会自动修改当前 fork，也不等于候选发布获批。
      `stage6:environment:baseline` 继续补齐 Environment 不存在时的引导：默认计划绑定 repository、protected branch、
      HEAD 与 2–6 个 numeric User/Team reviewer，摘要确认后才可创建/加固 `stage6-enterprise-ga`，并回读禁止自审、
      protected-branch-only 与 reviewer 精确集合；既有 wait timer 或不同 reviewer 不会被静默覆盖。GitHub 官方 REST
      与当前 GraphQL schema 都不提供 administrator bypass 写入，因此收据在该设置仍开启时固定返回
      `environment-baseline-applied-manual-admin-bypass-required`；只有授权管理员在 Settings 关闭后再次验证，才返回
      `environment-baseline-ready-not-release-approved`。该人工边界不能被本地工具伪造成完成。
      `release.yml` 现又把 `publish_cli` 限定为非 Enterprise GA candidate：npm Trusted Publishing 虽使用 OIDC，
      但 CLI package 不在 Desktop artifact-set、candidate v3 十份收据或 Final Review archive 中，不能借同一
      `stage6_enterprise_approval` 被顺带发布。普通发布仍可用 `SYNARA_PUBLISH_CLI=1`；Stage 6 CLI 发布须另行建立
      自己的证据与审批边界。
      Migration `000111` 与 `internal/releasegovernance` 现把同一 candidate/tag、commit、lockfile、candidate receipt、
      final asset set 与 environment ID 固定为不可变 Platform 权威；创建者不能自批，同一人员不能占用两个审批角色，
      Engineering/Operations/Security/Product 四类 append-only 决策缺一不可，任何拒绝都原子终止候选。Admin 的
      Release governance 页面以 version CAS 推进 draft→review→approved→deploying→observing→released/rollback，
      released 必须显式记录最终决定以及 `none|accepted` 残余风险处置；accepted 风险必须带 owner、未来 due date、
      理由和 HTTPS 证据。每一步写入 Operator Tenant Audit，但它不会把缺失的外部证据或人工权限伪造成 GA 通过。
      Migration `000139` 进一步要求上述五类 Release decision 的每条 evidence URL 都绑定非零 `sha256:`；服务、
      SQLite/PostgreSQL trigger、typed client、Platform Admin、Audit 与 readiness 同步要求，旧 URL-only 决策保留历史
      但不再计入任何正向转换，append-only role 已占用时必须重建候选而不能覆盖旧证据。
      Migration `000140` 又把所有 `release.*` 及共用的 Compliance/Provider/Recovery/Penetration/Capacity/Incident/
      Operations/Internal Cost 职能授权根证据升级为 SHA-256 字节绑定；旧 active URL-only grant 原子撤销，新决策必须
      使用重新签发且 byte-bound 的当前 grant。
      页面另通过只读 exact-candidate readiness API 一次投影候选 evidence、Provider commercial authorization、
      影响域审批、SLO、Recovery、Penetration、Capacity、Incident、Operations 与 Internal Cost 十项门禁；该 API 与进入 approved 的 transition 复用同一服务判定，
      避免页面状态和写门禁漂移。每一项都固定展示仍需外部验证的执行、签名、证据和组织权威边界，内部全绿只表示
      product transition eligible，不产生 GA passed 声明。
      `docs/release-checklists/stage-6-change-notice.md` 是 release note、管理员通知、Support brief、员工通知与
      产品内 changelog 的单一内容源。每次真实发布的审批/投递证据仍必须在复制的 checklist 中完成。
- [x] 完成用户文档、管理员文档、部署文档和故障排查文档（API 与开发者文档归 Stage 7）。
      `docs/enterprise/` 已按最终用户、Tenant 管理员、部署运维与支持排障拆分入口，并链接权威合同、部署说明与
      深度 Runbook。`stage-6-documentation-v1.json` 和 validator 固定五份必需页面、章节 marker、本地链接与
      内容 hash；`stage6:documentation:validate` 现对 matrix/Markdown 执行稳定有界非 symlink 读取、重复 JSON 与
      明显凭据拒绝、总字节上限，并用同一份捕获字节做 marker/link 语义检查和摘要，避免摘要后换包。收据始终为
      `source-documentation-validated-not-release-verified`。每个真实候选仍须用部署后的
      角色、UI label、镜像配置与签署承诺复核后，才能勾选 GA checklist 的 release-specific 文档项。文档源码、
      Operations UI 源码、Candidate bundle 和 downstream Final GA Review 四类辅助验证收据现与十类外部收据一致
      使用不可覆盖 `0600` 双文件发布；源码回归门禁会拒绝恢复直接写最终输出的实现。Final Review v1 进一步把
      完成后的清单/通告、protected approval、final asset set、Platform Audit request ID、32 项职能控制、四方及
      条件式 Privacy/Legal 决策与残余风险固定到同一候选；fail-closed preparer 从 path-only draft 稳定读取并计算
      全部摘要，完整语义校验后才发布 manifest/receipt 四文件，冲突或失败不留半成品，但永久保留外部证据和审批
      权威未由 Synara 验证的边界。Platform Admin 已提供 observing 状态的 Final Review 上传绑定，Migration
      `000136` 与 SQLite safety trigger 又要求 Provider commercial 四类文档摘要并撤销旧 URL-only 授权；
      `000137` 继续要求四个角色的 approval evidence 摘要并撤销含 URL-only decision 的旧授权；`000138`
      再把 exact active Provider Authorization ID/version 绑定到商业影响 Release candidate 并在全正向状态转换
      重验；`000139` 继续要求 Release 五类角色决策 evidence 摘要并让旧 URL-only 决策失去正向授权，兼容矩阵以
      `000161` 为当前尾部：`000140` 要求所有治理职能 grant 的委任证据摘要并撤销旧 URL-only active grant，
      `000141` 让 Compliance accepted manifest review 与四方 start-gate decision 的 URL-only 历史记录失去
      `record_complete` 授权并允许提交 byte-bound replacement，`000142` 同样收紧四方 SLO decision、重开旧
      approved Window，并在所有 Release 正向状态重验其摘要；`000143`–`000160` 继续收紧 Recovery、
      Penetration、Capacity、Incident、Operations、内部成本及历史支付兼容边界，`000161` 将活动事故通告切换为
      Internal Status Board 与员工通知契约。`000135` 继续阻断未绑定或决定/风险漂移的
      `released` 转换。
- [x] 【新增】建立产品内更新日志与破坏性变更通告通道，与 Stage 7 的 `Deprecation`/`Sunset`
      政策共用同一发布节奏，避免对外承诺与产品内公告不一致。复用 Web 现有的一次性升级提示与
      Settings → Release history，两者读取同一 `whatsNew/entries.ts`。typed `notices` 把管理员动作、
      破坏性变更和紧急安全例外置于普通功能卡片之前，并强制 audience/action/effective/first-published；
      breaking change 必须有 migration guide，单测验证 30/90 天窗口及紧急例外 expiry。真实候选版本的
      发布可见性、受众和内容 hash 仍由 change-notice/GA checklist 留证，源码存在不算投递完成。
- [ ] 完成容量测试、长时间稳定性测试、渗透测试和上线评审。容量/长稳合同与运行手册已落在
      `docs/contracts/capacity-long-duration-acceptance-v1.md`、`docs/runbooks/capacity-long-duration-test.md`；
      `scripts/stage6-capacity/validate_capacity_evidence.py` 对 forecast+至少 20% headroom、production 72h /
      production-like 24h、五个必需阶段、三 Region 外部探针 95% coverage、四类 SLO 样本/比例、CPU/Memory/DB
      80% headroom、Queue/Outbox、warm deficit、OOM/dead-letter/restart、Tenant fairness 与七份独立证据 hash
      fail closed；path-only preparer 又以稳定受限读取、重复 JSON/秘密/路径检查、自动 hash、`0600` 不可覆盖
      双文件发布和 receipt 失败回滚消除手工摘要与半发布风险，且收据固定为
      `evidence-validated-not-capacity-passed`。第三方渗透的独立合同、运行手册与校验器
      同样已建立但不冒充真实执行。Migration `000125`、`internal/capacitygovernance` 与 Platform Admin → Capacity
      reviews 已将补全后的 exact receipt、候选绑定、机器重算和 Engineering/Operations 分权审批固化；Migration
      `000126` 又把同候选内部 approved 状态接入 Release gate，但不会把内部批准冒充真实长稳通过。当前仍缺真实
      production-like 长稳、第三方渗透与正式上线评审，故本项保持
      未完成。
- [x] 【新增】编写 `docs/release-checklists/stage-6-enterprise-ga.md`，沿用既有约定（每次发布复制
      一份，记录 Commit、镜像 Digest、Migration、执行人、时间与证据链接；未满足项保持未勾选，不
      接受 fixture 或静态检查替代真实发布证据）。模板、证据索引及各外部验证器的非通过收据语义均已定义；
      真实候选版本仍必须复制模板并逐项附证，不能把仓库模板本身视为 GA 通过。

#### 完成条件

- [x] 新 Tenant 可以不经人工数据库操作完成注册或企业开通。Standard-profile 自助路径与 Platform Admin 企业开通路径
      均有 Web → typed client → authenticated route，服务端按 authority 分离 entitlement profile/状态能力；企业初始 Owner 必须
      已有 active account，找不到时返回稳定错误而不是要求直接写用户或 Membership 表。
- [x] 用户入职、调岗、离职和 Credential 回收有完整审计链路。邀请/接受、角色调岗、暂停与移除写入稳定
      Audit action；暂停与移除在同一事务撤销 Tenant Login Session、Organization Membership 与 user-scoped
      Credential，并记录撤销数量。Tenant `admin | owner` 自动获得的 root Organization 同级角色会在降级时
      原子撤销，但独立调整过的 Organization role 不会被误删；聚焦链路测试覆盖完整 actor/request/resource/
      role delta 与 Credential/Session 最终状态。
- [x] 用量、配额、成本和超限策略可解释且并发安全；用户可自助查到单次 Session/Turn 的成本构成。
      `ExecutionUsageSummary` 以 Execution/Generation 单调投影 token、时长、Network 与 Provider cost，Tenant
      周期汇总关联 entitlement profile assignment、软配额告警与 Stage 4 分摊成本；Web Session/Turn Environment 面板和 Tenant
      Usage 设置页均使用 typed API 展示来源与 coverage。SQLite 阈值/解释测试与真实 PostgreSQL 双连接并发
      投影已验证 80%/100% 告警各一条、重复执行不增生；隔离 Compose 又验证无支付产品档案、180 Token、
      Provider cost、Network 与恢复后的历史 Generation 汇总，见
      [`stage-6-self-hosted-usage-failure-acceptance-20260802.md`](docs/reports/stage-6-self-hosted-usage-failure-acceptance-20260802.md)。
      硬并发 Quota 继续作为独立 admission 权威。
- [ ] 支持人员无需直连生产数据库即可完成排障，且每次 impersonation 可审计、租户可见。判定方式：
      列出支持团队的日常操作清单，逐项验证可在管理后台完成，不依赖 CLI 或数据库客户端。
      49 项源码操作矩阵已完成，但部署环境中的 Authenticated User/Owner/Admin/Security/Cost/Auditor/Support
      角色浏览器演练、拒绝用例与对应 Audit 证据尚未执行，因此完成条件保持未勾选。隔离 Personal
      Control Plane 的 Owner 浏览器验证和 Owner/`security_admin`/Support View 聚焦渲染测试只能证明
      本地 UI/角色逻辑，不能把该项升级为生产运营通过。部署态证据校验器已经就绪，但只有真实
      production/production-like manifest 的 49 项正向、49 项固定角色拒绝、Support 闭环和双审批全部通过后，
      才能把其 `eligibleForHumanGateReview` 收据交给 GA 审批。Support Grant 的显式撤销、Tenant 策略关闭触发的
      逐 Grant 撤销和自动到期现在分别写入 Grant-correlated Tenant Audit；状态与审计处于同一事务，审计失败会
      回滚终态转换。Migration `000162` 将部署证据升级为 Operations v2，分别绑定
      `support.access_revoked`/`support.access_expired` 及其 Tenant 可见结果；v1 审批被 supersede、Exercise 重开，
      且不能再授权 Candidate 或 Release。新增 `GET /v1/tenants/{tenantID}/support-diagnostic.json`，把脱敏的
      Tenant 运维聚合、Token/Network、Provider 成本覆盖与内部平台分摊以 exact-byte、可审计 JSON 快照提供给
      Tenant Support；客户端会重算 SHA-256，且不包含 Credential payload、Artifact 内容或 Execution prompt。
      该源码闭环仍不能代替部署态浏览器与真实 Audit authority 验收。
- [ ] 生产环境有明确 SLO、告警、值班、内部 Status Board 和事故处理流程。Migration `000117`、
      `internal/incidentgovernance` 与 Platform Admin → Incidents 已实现事件 identity、Commander/Communications/
      Security-Privacy 分权、独立 Status Board origin、内部更新顺序/节奏、关闭审批和 Audit；SQLite、HTTP 与临时
      PostgreSQL 17 的并发/不可变门禁已通过。但源码不会代替内部 Status Board 部署，当前仍缺真实 rota、paging、
      employee notification delivery、internal-user-path recovery observation 与 production-like live exercise。Migration `000118`
      又提供 exact SLO window 与四职能内部审批；path-only SLO preparer 已能安全固化窗口证据，但当前没有真实
      30 天生产窗口，故本项保持未完成。
- [ ] 备份恢复与区域灾备经过真实演练。判定方式：从生产备份实际恢复到可服务状态，测得 RTO/RPO
      并与承诺值比对——执行备份脚本成功不算通过，必须完成恢复侧验证。恢复 Runbook 与四组件证据
      manifest 已定义；`scripts/stage6-recovery/validate_recovery_evidence.py` 会从时间戳计算 RPO/RTO、校验
      独立 failure domain、真实客户端 canary 声明及证据文件 hash；v2 收据还把完整候选、恢复后的 release
      identity、单一 recovery subject、五个分离角色审批及 drill/component/approval/validation 时间闭包固定下来，
      path-only 两阶段 preparer 进一步阻止在 subject 冻结前生成最终审批，并对来源文件执行稳定有界读取、
      Secret/重复字段/路径安全检查和不可变原子发布；
      同时明确真实备份 authority、审批 authority 与密码学签名仍需外部复核。收据固定标记
      `evidence-validated-not-control-passed`，不能替代尚未执行的生产恢复。
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
第三方提交任意代码。另一条来自 Stage 10 的硬门禁：usage/cost 相关端点的契约冻结前，必须先完成
FOCUS 列名对齐决策（见文首硬门禁表与 Stage 10），否则冻结后再改是对外 breaking change。

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
- Session 任一时刻只有一个驱动者；跨人协作以 interaction 为单位完成，不引入多人并发驱动。

#### TODO

- [ ] 冻结统一提及契约：`@member` 与既有 `@agent-alias` 共用解析、渲染与 durable interaction
      语义；明确两者在能力上的差异（agent 可执行、人可决策/回答），但不在语法与事件模型上分叉。
- [ ] 在控制面实现 Mention 领域模型：durable、幂等（复用 `Idempotency-Key`）、进入 Session
      Event Stream、可 resolve/过期，并复用既有 interaction 的 pending 快照与 reconcile 语义。
- [ ] 冻结"提及即访问授予"的安全语义：被提及人按**其自身角色**应用既有 redaction 投影（不继承
      提及者的可见性）、写入 Audit、可撤销、可设过期；提及不得越过 Organization 边界。
- [ ] 【已决策 2026-08-04：单驱动者】冻结会话驱动权模型：Session 增加权威字段 `driverUserId`
      （初始为创建者），任一时刻只有 driver 能发起 Turn；在该 Session 内发起 人 → agent 委派
      等价于发起 Turn，同样归 driver 独占，否则协作者可经 `@agent(task)` 绕过驱动权。interaction
      的 resolve 权按其**响应者指向**判定而非按 driver 判定：Approval / Structured User Input
      默认指向 driver，driver 可经提及把具体问题定向给同事，被指向者即可 resolve——一次性
      receipt 记录 resolver 身份，"谁批准了这次危险操作"始终有唯一答案。其他协作者是响应者：
      按自身角色的 redacted 读投影，可写锚定评论、发起锚定的 人 → 人 interaction、resolve 指向
      自己的 interaction。协作的单位是 interaction 而非并发键入，**不做多人共驾**：Provider
      Runtime 是单流状态机，真实的并行诉求由 fork 满足。
- [ ] 实现 Session 转交（handoff）、协作者列表与显式共享，复用 Organization 角色而非新建权限
      体系。handoff 是控制面权威、版本化、写 Audit 的显式动作（沿用既有 authoritative session
      lifecycle actions 的模式）：正常路径需接收方 accept；管理路径允许具备相应 Organization
      角色的成员强制接管（覆盖驱动者离职/掉线）。驱动权变更即递增 driver generation 并进入
      Session Event Stream（客户端经 `snapshotSequence` 对账），携带旧 driver generation 的
      Turn 提交与 interaction resolve 一律 fail closed；该 fencing 是对既有 Generation fencing
      语义的同构加法，不改动 Stage 3/4 已冻结的 wire 语义。并发 resolve 复用 `Idempotency-Key`
      与一次性 receipt 的单胜者语义。handoff 时仍 pending 且指向旧 driver 的 interaction 随
      驱动权重新指向新 driver（携带新 generation 进入其待响应队列，不留下永远无法 resolve 的
      悬空项）；显式定向给第三人的 interaction 不受影响。新 driver 的可见性与审批能力按其自身
      角色生效，不继承旧 driver——与"提及即访问授予"同一条不放大原则。
- [ ] 非 driver 的 UI 呈现为显式的"由 X 驱动"状态，composer 降级为评论/回答模式并提供"请求
      接管"入口（走 handoff 流程）；不做 Google Docs 式多人光标与输入中状态，避免制造"共驾被
      支持"的错误预期。
- [ ] 提供并行协作的正式出口：fork Session（继承上下文快照，独立 workspace/worktree，执行面
      天然隔离），结论经锚定评论/提及回流原 Session；不在同一 Session 内提供任何形式的并发驱动。
      fork 不得成为绕过 redaction 的通道：fork 者只能带走按其自身角色可见的内容（快照先过其
      redaction 投影再落入新 Session），fork 写 Audit，并需负向测试证明受限角色 fork 不出被
      脱敏的事件。
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
      语音/视频、**多人并发驱动同一 Session（共驾/多人光标）**，以及**与组织 IM 的双向消息
      镜像**。这些一律不做——溢出只允许平台 → IM 单向，IM 中的普通对话不回流，避免退化为
      第二个 IM。
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
- [ ] 任一时刻 Session 只有一个 driver：被 fence 的旧 driver 提交 Turn / resolve 被拒绝、双人
      并发 resolve 同一 interaction 恰好一个成功、非 driver 无法发起 Turn 或 resolve 非指向自己
      的 interaction（尝试记审计），均有负向测试证明；handoff 全程可审计，且新 driver 权限严格
      按其自身角色、无继承无放大。
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
自动化）。Stage 3 已把 Session 权威迁到控制面，而这两块能力留在本地，等于 Control Plane 模式缺少个人版
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

- 个人版与 Control Plane 模式共用同一 Automation 领域模型，不存在“本地有、远程没有”的能力。
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

- [ ] 个人版与 Control Plane 模式的 Automation 使用同一领域模型与同一契约，无独立分支。
- [ ] 定时与事件触发的 Execution 具备幂等性。判定方式：对同一 VCS 事件重复投递（含 VCS 平台的
      自动重试与人工重放）不产生第二次 Execution，也不产生重复的 PR 评论或 Check Run。
- [ ] 自动化执行的权限不超过其创建者，且无人值守路径不能绕过审批与沙箱边界。
- [ ] 从 PR 或 Issue 触发的 agent 结论能以 Check Run 与 review comment 回到开发流，且该链路复用
      Stage 8 的统一 interaction 类型，未产生第二套提及机制。
- [ ] 未映射到已知成员的外部 VCS 身份无法触发任何 Execution，且尝试被记入审计。
- [ ] 存在效果侧指标（至少采纳率），可回答"换模型/换 prompt 后质量是否变化"。
- [ ] 新用户可在不阅读文档的情况下完成仓库连接并跑通第一次 Turn。

### Stage 10：计算供给弹性与成本可编程化

状态：TODO。立项日期 2026-08-04，来源是一次外部对标（Cloudflare Agents Week 2026-08 系列
发布，含 `@cloudflare/computer` 与 Billable Usage API）而非内部缺陷核查。对标结论不改变
Stage 5-9 的优先级与 GA 关键路径，只把路线图已隐含依赖、但从未显式立项的两个能力收口成
阶段：会话生命周期与计算生命周期解耦，以及成本的程序可消费化。

立项依据：

- Stage 8 的 pending interaction（会话停在"等同事回答/等审批"上数小时）与 Stage 9 的定时/
  事件自动化（大量短执行）在放大同一个成本结构问题：会话存活时长不能等于计算持有时长。
  **差距审计（2026-08-04）**：机制层已在 Stage 4 落地并验收——`waiting-for-approval`
  suspend、`suspendAfterIdleSeconds` 的 Checkpoint/Suspend/Resume、lease-free `suspended`、
  Warm Pool 与冷启动 P50/P95/P99 指标、空闲区间计为 platform cost 均已存在。因此本阶段
  **不新建挂起机制**，真实缺口有三：挂起触发面未覆盖 Stage 8 新增的 pending interaction
  类型；"空闲不持有计算"缺产品级验收姿态（没有指标回答"多少空闲会话仍在持有计算"、哪些
  Target 应默认开启挂起）；周边轻量工作没有低成本执行路径。
- Stage 6 的目标原话是"用户能自己解释'这次花了多少、为什么'"，其用户侧 per-Session/Turn
  成本可见性已完成——但载体是管理面板与 CSV，缺程序可消费的 API 形状，而消费者应包括
  无人值守的 agent 本身（Stage 9）。行业成本数据形状已在 FinOps FOCUS 规范上收敛
  （AWS/Azure/GCP 与主流成本工具均已支持），列名与语义要趁 Stage 7 契约冻结前定，之后
  再改就是对外 breaking change。

**边界（不可动摇）**：本阶段借的是"计算按需供给 + 空闲挂起"的经济学，不是技术选型——
不自建 isolate/microVM 技术栈，不引入第二套 Worker Protocol，不因成本优化降低任何 Stage 5
冻结的隔离、审批与 egress 边界。

跨阶段归属（本阶段不重复实现，只做扩展与验收）：

- Checkpoint/Suspend/Resume、Warm Pool、冷启动指标与空闲成本归属机制归 **Stage 4**（已验收）；
  本阶段只扩展触发面并补产品级验收，禁止重建。
- 用户侧 per-Session/Turn 成本可见性与 billing 权威归 **Stage 6**（已完成）；本阶段只做导出
  形状（FOCUS）与程序化端点，数字必须同源。
- API 鉴权、三级 allowlist、防暴露守卫与 Service Account scope 归 **Stage 7**；本阶段不新增
  鉴权机制。
- 运行时层级的隔离等级声明规则归 Roadmap-wide rules 与 **Stage 5**；轻量执行路径只是该规则
  的一次新实例，不豁免。

#### 目标

- 会话生命周期与计算生命周期解耦：计算按 Turn 租借，空闲即挂起/释放，恢复对用户透明。
- 成本与用量是程序可消费的一等 API：形状对齐 FOCUS，agent 可自查配额与当期花费，数字与
  Stage 6 billing 权威同源。
- 对外 API 以"无人值守 agent 能否只靠响应内容自我纠错"为可用性检验标准。

#### TODO — Turn-scoped 计算租借与空闲挂起

- [ ] 将挂起触发面扩展到 Stage 8 的统一 interaction：Session 进入任何 pending interaction
      （审批、同事提问、agent 间委派）超过阈值即走 Stage 4 既有的 Checkpoint/Suspend/Resume
      链路，与既有 `waiting-for-approval`、`suspendAfterIdleSeconds` 同一状态机，不新建；
      恢复必须复用已冻结的 session 恢复、Generation fencing 与 approval 一次性 receipt
      语义，无回归。
- [ ] 把"空闲不持有计算"从能力升级为验收姿态：建立"空闲会话计算持有率"指标；明确各
      Execution Target 的默认挂起策略与开启条件——p95 恢复延迟预算不达标的 Target 不得
      默认开启；恢复对用户透明，UI 不得出现"会话丢失"或需要手工重连的状态。
- [ ] 为周边工作定义轻量执行路径（git 元数据操作、artifact/diff 投影、Stage 9 入站 webhook
      的 pre/post 处理），不为其消耗完整 provider 沙箱。按 roadmap-wide rule 显式声明该层级
      的隔离等级；执行层选择只是调度与成本提示，**绝不构成安全边界的降级通道**——不可信
      输入（Stage 5 定义）不得被调度到弱隔离层，需负向测试覆盖。
- [ ] 为本阶段新增的每条挂起/租借路径与护栏给出实测性能预算：默认关闭路径零开销、开启路径
      的吞吐与尾延迟开销有实测数字、校验失败一律 fail closed（中止而非降级放行）。

#### TODO — 成本可编程化

- [ ] 将 usage/cost 导出的列名与语义对齐 FOCUS 规范（`ServiceName`、`ChargePeriodStart/End`、
      `ConsumedQuantity`/`ConsumedUnit`、`ContractedCost`、`BillingCurrency` 等），数据复用
      Stage 6 billing 权威，不建第二套统计旁路；暂未覆盖的 FOCUS 必填列显式登记差距，不硬造
      数据。该命名决策必须在 Stage 7 usage/cost 契约冻结前完成（硬门禁见文首）。
- [ ] 在 Polaris API 提供 agent 可自查的配额余量与当期成本端点，归因粒度到 Session/
      Execution；走 Stage 7 的三级 allowlist 与防暴露守卫、Service Account scope，不新增
      鉴权机制。
- [ ] 将"无人值守 agent 只靠 API 响应即可自我纠错"纳入 Stage 7 错误契约的验收：错误响应
      包含机器可读原因码、重试语义与配额余量。判定方式：一次 agent 不读文档跑通 SDK 典型
      流程、并在故障注入（限流/配额耗尽/无效参数）下自行退避恢复的演练。

#### 完成条件

- [ ] 一个停在 pending interaction 的 Session 超过阈值后不再持有 provider runtime 与沙箱
      计算资源；用户下一次交互透明恢复，p95 恢复延迟在预算内。判定以真实挂起/恢复演练、
      资源计量与"空闲会话计算持有率"指标为准，且 Generation fencing、approval receipt 与
      session 对账在挂起恢复路径上有回归测试。
- [ ] 轻量执行路径（如落地）具备显式隔离等级声明，且有负向测试证明不可信输入无法被调度到
      弱隔离层。
- [ ] usage/cost API 列名与 FOCUS 对齐并有契约一致性测试；API 数字与 Stage 6 billing 权威
      同源，无旁路统计。
- [ ] agent 能通过 Polaris 自查配额与当期成本，并在配额受限时依据机器可读错误自行退避，有
      演练证据。
- [ ] 成本优化未降低任何安全边界：Stage 5 的隔离等级、审批与 egress 语义在本阶段结束后全部
      无回归。

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
