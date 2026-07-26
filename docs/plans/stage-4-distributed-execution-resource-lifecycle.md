# Stage 4：分布式执行与资源生命周期产品化

状态：IN PROGRESS

Stage 3 冻结了 Provider/Worker Contract、Control Plane 权威 Session、Generation fencing、Workspace
Checkpoint 和跨 Pod 恢复。Stage 4 不重新定义这些契约，而是在其上实现可解释的资源生命周期、
scale-to-zero、容量调度、多集群恢复和成本治理。

## 1. 首个里程碑：Session 长存、Pod 可丢弃

首个里程碑按以下顺序交付：

1. 原子 Recovery Bundle 和服务端活动时间轴。
2. `waiting-for-approval` 的 Checkpoint、Suspend 和新 Generation Resume。
3. Tenant/Project/Session 生命周期策略与 Web 配置、状态展示。
4. Warm Pool、冷启动 SLO、Pod 成本和恢复指标。

截至 2026-07-25，Recovery Bundle、活动时间轴、策略/Web 和 waiting Suspend 的 Control Plane 主链路已经
落到当前工作树；Provider Credential 已改为 Generation-scoped opaque Grant，Worker Lease 也冻结了物理
incarnation/instance UID，避免重注册 Worker 继承旧 Pod 权限。Control Plane 现在只接受 Execution Target
`signed-v1` Ed25519 policy 验证通过、且绑定 Target、Pod UID、Worker image/build 和 probe identity 的
containment attestation；Migration `000052` 会把升级前的 self-attested 严格 Manifest 显式降级为
`legacy-untrusted`，策略 key 轮换也会立即使旧 Manifest fail closed。bundled Linux agentd 具备 fd-relative、
启动前绑定和 `cgroup.kill -> populated=0` 的 cgroup-v2 清理原语；managed Kubernetes foundation 也已给
Namespace 增加 restricted Pod Security 标签，并显式关闭 Pod 的 host namespaces / service links。Provider
与 supervisor 仍是同一安全身份，因此不会签发上述 in-Pod attestation。Kubernetes waiting Suspend 现在改用
独立的外部信任路径：target-scoped Pod-bound ServiceAccount TokenReview、冻结的 Worker incarnation/Pod UID、
durable checkpoint-ready handoff，以及 kubelet 对同一 Pod UID 发布的 `Succeeded`/agentd exit 0 终态共同构成
完成证明；`DELETE 202`、Pod `NotFound`、`Failed` 或浏览器/Worker 心跳都不能提交 Suspend。Provider Host 2.2
active-turn Suspend 也已使用一次性 native cursor receipt 和相同的精确 Pod 终态证明落地。Warm Pool 的
[Worker Pool/Placement v1 契约](../contracts/worker-pool-placement-v1.md) 已冻结并在当前工作树落地 target-local
Kubernetes 预热容量、release-aware one-shot Worker、cold fallback 和 pool-aware scheduling template。Generation
与物理 Worker/Pod incarnation 事实也已持久化，用于重启后仍稳定的 30 天冷启动分位、warm hit/fallback、恢复终态、
运行/空闲时长及 requested-resource-seconds 成本代理。当前工作树又增加了跨 Target/Region/Cluster 的分组路由、
lease-free 灾难恢复 successor、多副本 Reconciler Leader Election、不同 UID/GID 的受保护 cgroup-v2 supervisor，
以及 versioned tariff、实际云账单导入/对账。tenant-owned managed Kubernetes 的 fresh-reconcile health publisher
已落地；platform-shared/external Target 的生产健康发布器、跨故障域 Artifact/Checkpoint 复制、受保护
supervisor 的生产宿主机特权验收和托管云/生产时长 Kubernetes soak 仍需要部署环境证据，因此 Stage 4 继续保持
`IN PROGRESS`。当前指标已补到 Provider `session.started` ready 延迟、Generation start/ready outcome、
claim-time resume decision、经过严格验证的 runtime fallback reason，以及短期 Provider Credential access Lease
状态。Migration `000055` 已把 Session semantic activity sequence 接到 Generation/Grant/Worker-Lease fenced 的
短期访问授权；agentd 在授权撤销、轮换、过期、元数据回退或续租流中断时会停止 Provider。Generation 冷启动指标
使用 30 天窗口；Worker incarnation 资源时长仍按保留事实 scrape-time 聚合，尚不是无限历史下的最终预聚合形态。
跨域 DR 已按当前 Session 水位与冻结 Bundle 水位合并 Artifact authority，并覆盖 fork origin、rollback、畸形引用和
运行期新增 Artifact。Resume Snapshot 的 byte/token budget 现在不再删除 Result Artifact 引用本体，只会先裁剪
narrative / tool / interaction 元数据以及可选 Artifact metadata；若精确引用集合本身仍超预算则 fail closed。
因此 DR required-set 与最终 Recovery Bundle / Resume Snapshot 中的 Result Artifact 集合保持同一恢复契约。
普通 Create Session、Turn、review/compact 的公共 launch coordinator 现在也会在 target-local placement 之前执行
global routing commit revalidation；任何 Target/Group/Member/Location Outage/Health/DR readiness 漂移都会以 stale
失败原子回滚，且进入 commit 锁阶段后不再跨 Target 重试。Tenant/Organization 的下一层硬准入语义已冻结在
[`Execution Scheduling Policy v1`](../contracts/execution-scheduling-policy-v1.md)；Migration `000074` 已实现 append-only
revision/head、CAS + 原子 Audit、精确 Organization 交集、fixed/routed policy 和 location 提交复核、Execution 与
Recovery Bundle version/digest 快照，以及 PostgreSQL/SQLite insert/immutability 防绕过。`queue-pressure-v1` 已将
durable `queued/recovering` Execution 作为保守软压力进入跨 Target 排名，并在 Target commit lock 下重算以阻止
过期决策提交；它不会在缺少 publisher acknowledgement watermark 时把 queue 与 Pod occupancy 合并为硬容量。
共享 `fairqueue` 已进入 Kubernetes batch Pod 选择和通用/暖池 Worker Claim；Claim 在 Target lock 下按 Tenant
active service units equal-share，并有真实 PostgreSQL idle-Tenant 优先证明。Migration `000076` 又为普通 Turn、
review/compact 和 failover successor 建立原子 `selected-only` Scheduling Decision/Candidate，冻结 final post-lock
Health、queue pressure、DR readiness、placement 及 canonical SHA-256，并把 Decision identity 接入 Recovery Bundle；
历史数据明确标为 `legacy-selected-only`。严格 reservation、完整 rejected-candidate 重放和生产多租户 load/soak
证据仍未完成，因此本计划继续保持 `IN PROGRESS`。

2026-07-26 的[最终 disposable Kind 证明](../reports/stage-4-kind-resilience-acceptance-20260726-final5.md)已通过：
1 个 Kubernetes control-plane node、3 个 Worker node、
2 个分散的 Synara Control Plane Pod，`rbac`、`topology`、`leader-takeover`、`control-plane-failover`、
`node-drain`、`node-partition` 共 6/6 场景通过且无 skip；600 秒配置 soak 实际运行 601 秒，10/10 disruption
cycle 通过，idle 和每轮 readiness probe failure 均为 0。精确 Reconciler holder Pod 被删除后 holder 改变且
fencing token 只从 3 增至 4；20 秒 Node partition 后节点重连和双副本 readiness 均恢复。该结果只证明
checked-in Kind lane 的预生产行为，不替代托管
Kubernetes 的 Node controller、存储、负载均衡、跨可用区网络和长周期生产 soak。

本地 real-kubelet OrbStack 证据已收口到
[final14 报告](../reports/stage-4-orbstack-resilience-acceptance-20260726-final14.md)。不可变镜像
`synara-control-plane:stage4-orbstack-final12-20260726`
（`sha256:e0c9b0079432052203e044c4becad86eb251746cd220f0951ac1b4557905de39`）在双副本下完成
RBAC、Lease Guard v2 精确 Leader takeover 和独立 Control Plane failover 3/3；fencing token 从 20 单调推进到 21，两次
扰动的 readiness probe failure 都为 0，`/ready` 在扰动后报告 schema 73/73。Migration `000071`–`000073`
的 row/checksum、Target operation/expected-instance 列和 Worker bootstrap-generation 列已在保留的 PostgreSQL
中现场核对；对应 disposable OrbStack SSH final4 还通过 16/16 生命周期与撤权验收。

Migration `000069` 的 target-local Warm capacity authority 也延续到 final14：独立
`synara-warm-final6b` Pod 在后续 rollout 和控制面故障后仍保持原 UID、Ready、零重启；final10 后 4 个
追加 Reconciler 样本将 authority version 从 4676 推进到 4689，并持续报告
desired/claimed/ready-idle=`1/0/1`。Placement 仍只把 fresh-ready 作为性能软偏好，缺失、过期、unsupported
或 zero 时保留冷启动候选。

Migration `000070` 为 Kubernetes Pod 删除增加 exact
`(target, namespace, podName, podUID)` durable fence：Register 与 Delete 共用 logical-identity lock，删除事务
先锁 Worker、检查所有 Execution Lease 和 active Workspace cleanup delivery、持久化不可变 fence、进入
draining，再在提交后发出带 UID precondition 的 Kubernetes DELETE。被 fence 的 UID 不能注册、鉴权、心跳、
Claim 或重新激活；同名新 UID 不受旧 fence 影响。真实 PostgreSQL 双事务与 SQLite trigger 测试已覆盖该竞态。
精确的 SQLSTATE/message matcher 会把该锁竞争转换为可重试 503，同时保留其他 40001 的原错误语义。
OrbStack 仍是单节点，因此 final14 与多节点 Kind final5 互补，不等价于托管云多可用区和生产时长验收。

2026-07-26 又新增了[OrbStack + disposable Kind 双集群 DR final4](../reports/stage-4-dual-cluster-dr-acceptance-20260726-final4.md)：
两套不同的真实 Kubernetes API 分别启动 source 与 successor Worker Pod，并要求 agentd 对精确
Target/Execution/Pod UID 完成 register、claim 和 heartbeat 后才算 runtime-ready。演练验证了缺失 DR readiness
时 mutation-free fail closed、精确 source-domain watermark 放行、唯一 successor、source placement 不变、
predecessor lineage、旧 source Pod UID 消失以及 run-owned 资源的 UID-precondition 清理。failover 现在还会在
变更前验证源 Recovery Bundle 的持久化 SHA-256/envelope；DR successor 首次 Claim 会沿 predecessor ancestry
保留源当前 Turn 的 Context 和 Result Artifact。final4 还让两侧 Target-audience projected token 通过生产
Kubernetes verifier 在真实 API Server 上完成 TokenReview、ServiceAccount/Pod claim 和 Pod GET/ownership 校验，
并拒绝未绑定的控制凭证。该 lane 是本地可重复 control-path 证据，不代表完整 Worker 注册持久化、实际
backing-store 复制、successor 对恢复包的完整 Provider 运行时消费、生产 PostgreSQL failover、云 Workload
Identity 或云上独立 Region/AZ，后者仍保持部署验收待办。

同日的[账单 runtime final19](../reports/stage-4-billing-postgres-minio-acceptance-20260726-final19.md)在隔离 Docker
网络内使用真实 PostgreSQL 17.10 和 versioned MinIO，验证了精确旧 VersionId、AWS CUR2 native-shaped manifest、
多 chunk CSV/GZIP、split-child parent replacement、两段 tariff estimate、对账、scheduler audit、重启 replay 与
并发首次导入串行化。证据绑定 wrapper、生产 billing service、三份测试源码、parser/source 实现和实际 Linux
test binary 的 SHA-256，并强制执行 child-only、net/gross orphan、跨 chunk 缺 parent 三个负例；清理后无
run-owned container/network/volume。该结果仍明确报告
`cloudWorkloadIdentityVerified=false`，不替代真实 AWS/GCP/Azure export 与 workload identity 门禁。

这里的 `Session.status = suspended` 继续表示用户或管理员控制的操作状态。资源挂起使用独立
`resourceState = suspended`，两者不得混用。

## 2. 冻结的状态模型

```text
idle
  -> provisioning
  -> active
  -> waiting
  -> checkpointing
  -> suspended
  -> restoring
  -> active
  -> terminating
  -> idle
```

- `meaningfulActivityAt / meaningfulActivitySequence`：Control Plane 实际接受到的语义用户写操作或 Worker
  Runtime Event 的时间与单调 Event 水位；纯系统维护事件不续期。
- Worker 侧只允许显式语义白名单续期：对话 Delta、Turn/Item/Tool 进度、交互请求/响应和真实 Runtime
  warning/error 可以推进时间轴；Session/Thread 状态、Token usage、Auth/Account/MCP 状态及未知扩展事件不能续期。
- `resourceIdleSince`：当前资源空闲区间起点；持有或正在申请执行资源时为 `NULL`。
- `absoluteExpiresAt`：活动续期不可突破的硬边界；无策略时为 `NULL`，不得在迁移时猜测。
- Browser/SSE/WebSocket presence、读取 API 和传输心跳不构成 meaningful activity。
- Worker heartbeat 只续短期 Execution Lease，不直接延长 Session、Workspace、Credential 或 KMS 生命周期。
- semantic activity 只提供短期 refresh authority。不可变 Provider Credential Grant 固定 Generation 的
  Credential ID/version，Worker Lease 上的短期 access authorization 才能随有效活动窗口续期；它不能跨越
  `absoluteExpiresAt`、Credential expiry/revocation/rotation，也不能把任何长期明文密钥随 Session 无限续期。
- 当前 Broker 管理的是“该 Generation 还能使用已冻结 Provider secret 多久”，不是通用 OAuth refresh-token
  adapter；KMS/云 STS 继续使用各自 workload identity/SDK 的短期凭证生命周期，Session 活动不会延长 KMS key。

当前 `absoluteExpiresAt` 已作为不可滑动的权威时间轴持久化并展示，并且服务端 background controller 已开始
对到期 Session 执行硬门禁：新 Claim（含 receipt replay）、Lease Renew/Start、interaction resolution、Runtime
Event 和恢复入队会被拒绝或转成权威取消；Worker 的晚到 Complete/Fail、Suspend Abort 和资源指令轮询也会在
同一事务收敛为取消，其中资源轮询返回 `terminate` directive 立即停止本地 Provider；Kubernetes Reconciler
不会为到期 Session 新建 Pod，并会删除仍存在的旧 Pod；已有 active/suspended Execution 也会被 controller
权威取消并 fenced。`suspendAfterIdleSeconds` 已覆盖 compatible Kubernetes `waiting-for-approval` 和
Provider Host 2.2 native active-turn checkpoint 两条路径；不具备精确 checkpoint/终态证明的其他 Provider 或
资源类型仍会 fail closed，不能把“字段已经存在”解释为所有资源类型都已执行 idle reclaim。

## 3. Recovery Bundle v1

每次成功 Claim 一个新 Generation，都必须在同一数据库事务内写入一个不可变、内容哈希的
`execution_recovery_bundles` 行。Bundle 冻结以下内容：

- Execution、Session、Turn 和 Generation 身份。
- Target、Worker Manifest、Worker Release、Provider Runtime Binding。
- Provider Credential ID/Version；不包含明文 Credential。
- Turn input、Primary Operation 和 immutable Credential Grant descriptors。
- 有界 Resume Snapshot、权威 Event Sequence 和 Pending Interaction。
- Remote Workspace、Materialization、Restore Checkpoint 和 Git/Artifact 引用。
- 显式 Memory reference 集合。

Bundle 不保存 Provider Resume Cursor 明文/密文、Lease Token、Worker Token、对象存储 Credential 或 KMS
Token。Provider Cursor 继续通过既有加密字段独立传输；Bundle 只冻结恢复策略和安全引用。

规则：

- `(tenant_id, execution_id, generation)` 唯一。
- Generation 2 及以后必须引用同一 Execution 的较早 Bundle；升级时已有活跃 Generation 使用一次
  `legacy-adoption`，禁止伪造历史 predecessor。
- Claim replay 从 Bundle 恢复冻结 Workload，不重新投影可能已经变化的 Session 历史。
- Recovery Generation 的有界 Snapshot 覆盖当前 Turn 已经权威提交的输出、Tool 和 Artifact 进度；原生 Provider
  cursor 恢复成功时只补充这些恢复元数据，不重放已有 transcript，并明确禁止重复已完成副作用。
- agentd 在 Workspace、Credential 或 Provider 启动前校验 Bundle envelope 和 SHA-256。
- PostgreSQL 和 SQLite 都拒绝 Bundle 更新或删除；正式租户物理删除/合规擦除需要单独的受控清理流程。

## 4. Memory authority v1

Migration `000045` 已补齐独立的 Agent Memory authority：

- `agent_memory_heads` 为 User、Project、Session 三个 scope 维护可变 head。
- `agent_memory_revisions` 绑定 ready、content-hashed 的 `memory` Artifact，并强制 Revision 不可变。
- Claim 在同一事务里解析有效 Memory 集合，Recovery Bundle 冻结确切的 Revision ID、Artifact ID、
  SHA-256、媒体类型及精确字节数，不会在运行时追踪“当前最新”指针。
- agentd 和 Artifact 授权路径只接受当前 Generation Recovery Bundle 中已经冻结的 Memory Revision；
  mutable head 不能绕过 Bundle 直接读写运行期 Memory。

当前边界：

- Scope 固定为 User、Project 或 Session。
- lower scope 可以覆盖同 `memoryKey` 的 broader scope，但必须在 Claim 事务里重新验证 Artifact 和 SHA-256。
- Memory payload 进入对象存储 Artifact 边界并受 Tenant retention、加密和授权约束。
- pinned Memory Artifact 的内容类型、大小和 object key 不可改写；对象存储迁移后的实际下载仍必须通过
  SHA-256、精确大小和 UTF-8 复核。
- Memory 缺失、身份不符或越权安全失败，不能静默启动一个“失忆”Pod。

## 5. Suspend-and-resume v1

当前工作树已经实现 `waiting-for-approval` 与 capability-gated active Turn 的服务端权威 suspend/resume 路径：

1. Worker 通过 `PullResourceDirective` 轮询资源指令。只有 Kubernetes Target、`waiting-for-approval`
   Execution、超过冻结的 `waitingKeepAliveSeconds`、没有未确认副作用/Pending Checkpoint，并且满足以下一种
   completion mode 时才会触发：
   - `worker-attested-v1`：当前不可变 Worker Manifest 持有与 Target policy key 一致的 `signed-v1`
     strict-containment attestation；
   - `kubernetes-pod-terminal-v1`：Worker 使用 target-scoped、Pod-bound ServiceAccount token 注册，Control Plane
     已通过 TokenReview 和 Pod GET 校验 audience、ServiceAccount、Pod name/UID 与 Synara ownership labels。
   普通 Unix process group 无法约束 `setsid`/daemonized 子进程，不能据此走 `worker-attested-v1`。
2. Control Plane 创建 generation-fenced `execution_suspend_attempts(status=checkpointing, reason=waiting-keepalive)`，
   并写入 `execution.suspend-checkpointing` Event。
3. Worker 收到指令后先停止 Control delivery、取消 Provider，并等待本地 Provider runner 退出，再调用
   generation-fenced `MarkResourceSuspendQuiesced`。Control
   Plane 在同一事务中把 `provider_quiesced_at` 从 `NULL` 写成一次性时间戳并追加
   `execution.suspend-quiesced`。在 `worker-attested-v1` 中它由严格 containment 支撑；在
   `kubernetes-pod-terminal-v1` 中它只是 checkpoint ordering marker，不能单独授权 Suspend，Lease expiry 也不会
   把它误判成安全完成。
4. durable quiesce proof 成功后，Worker 才封存终端状态和 ready/unchanged Workspace Checkpoint，避免
   checkpoint 之后仍有文件或副作用继续变化。
5. `worker-attested-v1` 继续由 `CompleteResourceSuspend` 再次校验 Worker incarnation、strict-containment proof、
   quiesce proof 与 Workspace 可恢复性，然后原子完成。`kubernetes-pod-terminal-v1` 则只能由 Worker 写入
   `checkpoint_status/checkpoint_ready_at`，不能删除 Lease；专用 agentd 随后退出 PID 1，且不会从旧 Pod 重领下一
   Generation。Kubernetes Reconciler 必须观察到配置哈希、Generation、Pod name/UID 全部匹配，Pod phase 为
   `Succeeded`，且唯一 `agentd` container 已 exit 0，才由 Control Plane 原子写入 terminal proof、完成 attempt、
   删除 Lease、将 Execution 变为 lease-free `suspended`，并设置 `next_recovery_reason = suspend-resume`。
   如果 quiesce 后
   Checkpoint 或提交被明确拒绝，Worker 会 Abort attempt 并 Release 到新 Generation recovery，绝不重启旧进程。
   若用户恰好在该窗口提交 resolution，尚未写给旧 Provider 的答案会先原子转换为 `resume-recorded`，再由
   下一份 Recovery Bundle 一次性消费并转为 `resume-bound`；未回答的旧请求则 supersede，避免保留旧
   Worker 绑定。已经写给 Provider 但缺少 ACK 的结果不能安全重放，会终止为 `outcome-unknown`。
6. Session 资源时间轴同步进入 `checkpointing -> suspended`；`execution.suspended` 提交时写入
   `resourceState = suspended` 和新的 `resourceIdleSince`。
7. 用户在 suspended 期间 Resolve Interaction 时，resolution 仍然 durable，但
   `execution_interactions.delivery_status` 会写成 `resume-recorded`，不再投递给已经 fenced 的旧
   Provider Generation。
8. 当最后一个 pending Interaction 被 resolve 时，Execution 才会从 `suspended -> recovering`，
   Turn 返回 `queued`，Remote Workspace 标记为 `recovering`，并排队新的 Recovery Bundle / Generation。
   Claim 会在创建该不可变 Bundle 的同一事务内把其中的 resolution 转为 `resume-bound`，记录确切 Bundle ID
   与 Generation；同一答案不能进入后续第二份 Bundle。若这个已绑定 Generation 在 Provider ACK 前丢失，
   Execution fail closed，Interaction 进入可审计且不可重放的 `outcome-unknown`。
9. 如果 suspended 期间所有 pending Interaction 都过期，Execution/Turn 会失败，而不是自动拉起替代 Pod。
10. Kubernetes Reconciler 只在消费完精确 `Succeeded` terminal proof 后删除 waiting Suspend 的旧 Pod；普通
    completed/failed/obsolete Pod 仍走精确 UID precondition 删除。新的 Pod 只会在 `queued/recovering` 时创建。
    删除请求被接受、对象缺失或 `Failed/Unknown` 都不是 terminal proof；无法观察证明时保留 Execution fence，
    让 Lease recovery 收敛，而不是乐观标记 suspended。
11. Quiesce proof 和 Suspend completion 都使用确定性幂等 request ID。提交请求发出前本地 Provider 已停止；若服务端已经提交
    但 HTTP acknowledgement 丢失，agentd 会用新 Context 重放确认。结果仍不确定时会停止 Lease renewal，
    由 idempotency receipt 或 Lease recovery 收敛，旧进程不会继续运行。
12. `PullResourceDirective` 也是 absolute lifetime 的主动 fencing 通道：若 Session 已到硬期限，Control Plane
    会先原子取消 Execution/Lease，再返回 `terminate`。agentd 取得本地 runner ownership 后立即停止 Provider；
    containment 无法证明清空时整台 Worker fail-stop，不能把服务端 fenced 冒充成本地进程已安全退出。
13. `running` Execution 只有在 Provider Host 2.2、`suspend-active-turn=native`、native cursor 和当前不可变
    Manifest 全部匹配时，才会因 `suspendAfterIdleSeconds` 创建 `reason=active-idle-timeout` attempt。attempt 与
    durable `SuspendTurn` Control Command 同事务创建，并冻结 `boundary_meaningful_activity_sequence`。
14. `SuspendTurn` 必须等待原 `SendTurn` 到达 interrupted terminal，返回 active command ID、协议版本和非空
    Provider Cursor；Control Plane 把 history/turn/activity sequence、runtime binding、cursor source/digest 与
    Worker incarnation 原子写入一次性 receipt。自然完成继续走正常完成；命令已投递却没有 receipt、receipt 漂移或
    terminal proof 未完成时直接 outcome-unknown，禁止 authoritative-history 自动重放。
15. receipt 落地后的用户 Steer/Interrupt 不会重新打开旧 Provider，而是解绑旧 Generation delivery；旧 Pod 完成
    精确终止证明后原子进入 `suspended -> recovering`，新 Generation 消费这些操作。没有跨界活动时保持
    lease-free `suspended`，由显式 Resume API 触发恢复。
16. Claim 只能用 receipt 冻结的 exact native cursor，并把 receipt 一次绑定到一份 `suspend-resume` Recovery Bundle。
    cursor 过期/隔离会在 Claim 前终止 Execution；已经绑定的恢复 Generation 丢失会写
    `resume_outcome_unknown_at` 并 fail closed，第二份 Bundle 不能重复消费同一边界。

## 6. 策略层级（当前实现）

```text
Operator hard bounds
  -> Deployment profile defaults
    -> Tenant policy override
      -> Project override
        -> Session snapshot
```

当前可变策略层是三层：Deployment defaults、Tenant override、Project override。创建 Session 时可以提交
一次性 Session override，Control Plane 会把解析后的 effective policy 冻结进 `agent_sessions`；后续上层策略
调整不会回写历史 Session，因此这里没有独立的可变 Session policy 表或事后更新 API。

下级只能在上级允许范围内选择。Web 是配置和状态入口，不是计时或销毁权威。当前 Web 已提供：

- Tenant 级 lifecycle policy 查看与编辑。
- Project 级 lifecycle policy 查看与编辑。
- 创建 Session 时可展开一次性 override；空字段继承 Project effective policy，并在提交前显示 bounds 和结果预览。
- Session 级 `resourceState`、`meaningfulActivityAt`、`resourceIdleSince`、`absoluteExpiresAt` 和
  effective policy 只读展示。

首批字段：

- `waitingKeepAliveSeconds`
- `suspendAfterIdleSeconds`
- `absoluteSessionLifetimeSeconds`
- `workspaceRetentionDays`
- `warmPoolMode`：`disabled | balanced | low-latency`

## 7. 验收条件

- 删除 waiting Pod 后，用户 Resolve 能创建 Generation `N+1` 并继续同一 Session/Turn。
- Recovery Bundle ID、SHA、predecessor、Event Sequence、Memory/Workspace references 可审计。
- 旧 Pod、旧 Lease 和旧 Interaction delivery 不能写入或产生第二终态。
- Cursor 失效、Credential rotation 和 KMS/STS Token 更新不导致重复副作用。
- 浏览器保持打开、SSE reconnect 或读取 API 不续资源空闲期。
- 双 Control Plane 并发 Sweep/Resolve 只有一个状态转换成功。
- 前端显示 `provisioning / active / waiting / checkpointing / suspended / restoring`，并显示冷启动提示。
- 指标覆盖 Suspend/Resume 成功率、恢复耗时、冷启动 P50/P95/P99、Pod 运行/空闲成本和 fallback 原因。

## 8. 当前实现进度

- [x] Migration `000043`：不可变、按 Generation 的 Recovery Bundle、predecessor lineage 和索引。
- [x] Claim 首次/重放接入 Bundle；agentd 在执行前校验 envelope 和内容 Hash。
- [x] Migration `000044`：独立 `resourceState`、`meaningfulActivityAt`、`resourceIdleSince`、
      `absoluteExpiresAt`。
- [x] Session Event 写入驱动资源状态与 meaningful activity；读取流量不续期。
- [x] Migration `000045`：User/Project/Session scoped Agent Memory heads、immutable Memory Revision、
      ready-memory Artifact pinning，以及 Recovery Bundle 冻结 Memory references。
- [x] Migration `000046`：Deployment defaults + Tenant/Project override + frozen Session snapshot 的
      lifecycle policy schema、边界约束与测试。
- [x] Migration `000047`：`execution_suspend_attempts.provider_quiesced_at` 一次性证明、lease-free `suspended` Execution 形状、
      `resume-recorded -> resume-bound -> outcome-unknown` Interaction delivery state，以及 PostgreSQL/SQLite
      的精确 Bundle/Generation 绑定、不可重放约束。
- [x] SQLite 安全触发器、Recovery/replay/legacy adoption、Memory pinning 和活动时间轴测试。
- [x] `waiting-for-approval` suspend attempt、Checkpoint、`execution.suspended`、Resolve 后
      `resume-recorded` / `recovering` 与 Reconciler 删除 suspended Pod 的 Control Plane 状态链路已落地；
      `worker-attested-v1` 需要签名 strict containment，Kubernetes 生产路径可使用 Pod-bound identity + exact
      kubelet terminal proof。
- [x] Migration `000054`：冻结 suspend attempt 的 Execution Target、Worker incarnation、Pod name/UID，增加
      `checkpoint-ready` 与 `Succeeded` terminal proof 的一次写入/不可变约束；Kubernetes Worker 注册改用
      target-scoped projected ServiceAccount token、TokenReview 和实时 Pod ownership 校验，不再向 Worker Pod
      挂载全局注册 Secret。
- [x] Migration `000055`：增加 `meaningful_activity_sequence` 与 Worker-Lease-scoped Provider Credential
      access authorization；只有显式 Grant resolve 可首次签发，后续 Lease renew 仅依据服务端 semantic activity
      续短授权，Grant/version/absolute expiry/hard cap 不可滑动。agentd watchdog 对撤销、轮换、过期、序列回退、
      缺失元数据和续租流关闭 fail closed。
- [x] Web 已提供 Tenant/Project lifecycle policy 配置，以及 Session 资源状态/时间轴/effective policy 展示。
- [x] Web 创建 Session 流程支持一次性 lifecycle override，并保持空值继承与 Operator bounds。
- [x] 指标已覆盖 durable suspend attempt 状态、resource state / warm-pool-mode 配置库存、Recovery Bundle
      reason、Provider `session.started` ready latency、bounded claim-time resume decision、validated runtime
      fallback reason，以及 `synara_provider_credential_access_leases{state}`。Migration `000059` 把每个
      Generation 的 dispatch/lease/start/ready/terminal、requested warm mode 和实际 hit/fallback 决策固化为
      durable fact；冷启动 P50/P95/P99 与 recovery outcome 使用 trailing 30-day gauge，不再依赖重复 Event join。
- [x] PostgreSQL 已覆盖并发 suspend completion 单赢家和 `suspend-resume` 新 Generation lineage。
- [x] Migration `000057`-`000060` + Worker Pool/Placement v1：显式 `execution-pinned | warm-pool |
      general-pool` 模式、target-local Pool/Capacity Class/Placement Policy、队列时不可变选择、exact Pool/version/
      release Claim fence，以及 Kubernetes release-aware one-shot Warm Pod。已注册、online/active、无 Lease 且
      exact release/pool 匹配的 Worker 才算 ready warm capacity；Claim 与 scale-down 通过 Worker row lock + Lease
      recheck 串行化，Claim 后会在 `maxActiveUnits` 内回补新的 idle slot。Pool/Placement 仍只负责选中 Target 内的
      capacity；跨 Target/Region/Cluster 选择由后续的 Target Group routing authority 在它之前完成。
- [x] Migration `000069` + live warm-capacity authority：Reconciler 仅在成功结论后发布 active Warm Pool 的
      release-aware desired/claimed/ready-idle，失败时旧结论自然 TTL 过期；Placement 保留原候选为 cold fallback，
      只用 fresh-ready 信号作性能偏好。Prometheus 只暴露 bounded class/freshness/support/kind；PostgreSQL 已验证
      scope/CAS/不可删除约束。所有 suspended Resume 路径会在统一 Tenant admission lock 下重新获取 execution
      quota，并有真实 PostgreSQL 双 Session 并发单赢家证明。
- [x] Migration `000070` + durable exact Pod UID deletion fence：Worker Register/Delete 共享 logical-identity
      transaction lock；Delete 在外部 Kubernetes 调用前原子检查 Execution/Cleanup lease、写不可变 fence 并
      drain exact incarnation。fenced UID 的 Register/Auth/Heartbeat/Claim/reactivation 全部 fail closed，
      agentd 将 fence 响应视为 terminal；同名 replacement 新 UID 仍可正常注册和 Claim，UID-precondition 409
      作为旧观察处理且保留旧 fence。
- [x] Migration `000061` + Pod observer：物理 Worker incarnation 固化注册身份、Pool snapshot、requested CPU/
      Memory/Ephemeral Storage 与 active/idle/draining/terminal 时间线。Kubernetes `DELETE` 只进入 draining，只有
      kubelet terminal phase 或成功 List 后确认 exact Pod UID missing 才关闭 `terminated_at`。指标提供 trailing
      30-day 端到端 dispatch-to-Provider-ready P50/P95/P99、warm hit/fallback、recovery outcome、Pod runtime/
      idle/active seconds 和 requested-resource-seconds；后者是成本代理，不是货币账单。
- [x] `absoluteExpiresAt` server-authoritative controller：新的 execution-bearing 操作在硬到期后被拒绝；已在飞或
      suspended 的 Execution 会被权威取消并 fenced。
- [x] Migration `000056` + Provider Host 2.2 实现 `suspendAfterIdleSeconds` active-turn Suspend/显式 Resume：
      `SuspendTurn` interrupted terminal、activity/history/runtime/cursor lineage receipt、Kubernetes exact Pod terminal
      proof、跨界活动自动恢复、exact native cursor 一次性 Bundle 绑定，以及缺 receipt/cursor/恢复 ACK 时的
      outcome-unknown fail-closed 均已有 PostgreSQL/SQLite、Control Plane、agentd 和 Host 测试。
- [x] Worker Manifest 已冻结 process-containment mode、supervisor version、probe digest 和不同安全身份；
      Execution Target 通过 `signed-v1` Ed25519 public-key policy 验签，签名覆盖物理 Pod identity、Worker
      image/build 与完整 probe statement；Suspend eligibility 会重新核对当前 Target key ID/SHA。Migration
      `000052` 把旧严格 Manifest 标记为 `legacy-untrusted`，不再信 transient Worker capability 或历史
      self-attestation。尚无受保护 supervisor 的真实 agentd Manifest 仍为 `mode = none`。
- [x] Migration `000051` 将 Worker Lease 绑定到不可变 worker incarnation / instance UID；Worker 重注册后即使
      逻辑 `worker_id` 相同，也不能续租、完成 quiesce 后的 Suspend 或继承旧 Pod 的 Generation 权限。
- [x] Linux/非 Kubernetes 受保护 cgroup-v2 supervisor 已支持独立 Provider UID/GID、supervisor-owned parent、
      启动前 `UseCgroupFD` 绑定、`Pdeathsig=SIGKILL`、单调 incarnation/generation fence、`cgroup.kill`、
      `populated=0` 等待与清理；Provider workspace/runtime output 会显式移交给低权限身份，Git cache、注册材料和
      Ed25519 private key 仍由 supervisor/root 保护。只有 live probe 真正证明 credential drop、fd-relative attach、
      `setsid` descendant 清理并由 root-only key 签名时，agentd 才投影可被 Target policy 验证的 capability；legacy
      同身份模式仍不能宣告严格 containment。独立 SSH gate 使用固定 host key、仅读取非秘密 env allowlist，并要求
      Control Plane 最终投影 `trustState = verified`。SSH provisioner 还会把 Target 保持 offline，等待精确
      instance + operation generation 的 post-registration fresh heartbeat、Protocol v2/lease/fence compatibility、
      当前 build Manifest 和 signed containment policy；activation 在锁定 Target/Worker 的同一事务中二次检查后
      才提交，offline bootstrap 只接受当前 install/upgrade expected UID + generation，不能 Claim Execution/
      Workspace cleanup。active 重启只恢复同一逻辑 Worker/UID/generation；revoke 则在单个本地事务先提交 Target
      fence 和 Worker/token/lease/recovery/cleanup 撤权，再访问 KMS/远端，失败可复用同 generation 重试。当前仓库已有
      [特权 Linux 容器证明](../reports/stage-4-protected-cgroup-linux-acceptance-20260725.md)，但生产 SSH 宿主机仍需执行
      该 gate；OrbStack VM live containment final1 的真实 systemd/cgroup 场景通过 5/5，但 runner 因 OrbStack
      opaque-ID delete panic 在 cleanup 失败，不能计作全绿 gate；本地 disposable OrbStack SSH final4 已完成 16/16
      产品路径验收；Kubernetes waiting/active-turn
      Suspend 继续使用 exact kubelet Pod terminal proof。
- [x] Claim 已为每个 Generation 创建不可变 opaque Provider Credential Grant；Recovery Bundle/Workload 冻结
      Grant ID，agentd 通过 Grant + 当前 Lease/Generation 解析 Credential，已存在 Grant 的 Generation 无法回退
      到 Credential-ID resolve 路径；legacy Bundle/receipt 缺 Grant 时重放 fail closed，tenant deleting 或 absolute
      lifetime 到期后所有 Credential Grant 解析都会被拒绝。
- [x] semantic activity 已接入 Generation-fenced Provider Credential 短期 access broker，并覆盖首次解析前不签发、
      rotation/revocation/scope loss、refresh-window closed/reopen、absolute hard expiry 和 agentd 本地到期终止。
      当前静态 `apiKey`/`authToken` Provider 不支持 Generation 内热换 secret；KMS/STS workload identity 的生产云验证
      仍属于后续多集群接入项。
- [x] Migration `000062` + [Reconciler Leader Election v1](../contracts/reconciler-leader-election-v1.md)：使用数据库时间、单调 fencing token 和
      acquire/renew/assert/release，把 Docker/Kubernetes、跨 Target failover、Session resource lifecycle、Worker
      release rollback 与 retention sweep 置于跨副本租约下。带 epoch 的 GORM create/update/delete 会在同一
      mutation transaction 内锁定并复核 holder/token/expiry；租约 takeover 还必须取得与 in-flight controller
      cycle 相同 key 的 PostgreSQL transaction advisory lock，因此不能越过仍在执行外部副作用的旧 cycle。
      跨 Target failover 的精确 lease holder/token 同样会在提交 successor 的事务内复核。Outbox 保持数据库
      claim-safe 的 multi-active 模式。
- [x] Migration `000063` + [Global Target Routing and Disaster Recovery v1](../contracts/global-target-routing-dr-v1.md)：
      tenant-scoped Target Group、Region/Cluster member、TTL health/capacity authority、priority/balanced/latency
      选择、Session/Execution frozen routing snapshot，以及 lease-free source 到新 Execution attempt 的原子灾难恢复。
      Recovery Bundle 支持 exact predecessor Execution/Bundle 的 `disaster-recovery` lineage，未绑定 Interaction 和
      Workspace checkpoint 会安全迁移；已投递/结果不确定的副作用 fail closed。failover 在 mutation 前复用共享
      canonical validator 核对源 Bundle 的 persisted hash/envelope，DR successor 首次 Claim 沿 predecessor ancestry
      恢复源当前 Turn Context/Artifact；篡改 Bundle 的失败路径与 Claim/Release/Re-Claim/DR replay 均有测试覆盖。
- [x] Migration `000065` 将 destination DR readiness 提升为 `(execution_target_id, source_dr_domain)` 精确 authority，
      冻结 publisher、TTL、单调版本和 `replicatedThroughAt` watermark，并按实际需要分别门禁 Artifact、Checkpoint、
      Memory backing store。普通新 Turn 的重新路由与显式 failover 使用相同的 frozen source Region/Cluster 规则；
      缺 source snapshot、过期/错误 source authority 或落后 watermark 都 fail closed。
- [x] Migration `000064` + Cloud Cost Accounting v1：requested-resource proxy 与货币金额明确分表；versioned
      tariff 生成 micros estimate，AWS CUR、GCP Cloud Billing Export、Azure Cost Export 通过只读 Blob adapter
      严格解析实际账单，并按 provider external ID/checksum 幂等导入和对账。runtime 已接入 S3、GCS、Azure Blob
      默认 workload identity/credential chain、不可变 object version/generation、leader-scoped scheduler 和
      mutation-only audit；custom S3/Azure HTTP endpoint 必须显式开启且经过严格 URL 校验。本地 BlobSource 使用
      root-relative 打开并拒绝中间 symlink。`estimateAfterImport` 已接通内建 durable sweeper：配置必须显式绑定
      tenant-owned `executionTargetIds`，invoice replay 会幂等补扫同账期 Worker facts；一个 Worker 失败不阻止其他
      Worker，但 job 仍失败并可重试。本地 [final19](../reports/stage-4-billing-postgres-minio-acceptance-20260726-final19.md)
      已在 disposable PostgreSQL 17.10 + versioned MinIO 中通过
      `RuntimeConfig -> S3 adapter -> native CUR2 manifest/chunks -> scheduler -> estimate -> reconcile -> restart replay`，
      并在同 key 最新版本被 poison payload 覆盖后读取精确旧 VersionId；CPU/Memory split child 只有在唯一 parent、
      同 cost family 和金额一致都可证明时才替换，orphan/跨 chunk 缺失/错误 adjustment 整单 fail closed；scheduled
      import/reconcile 还共享一个满足 PostgreSQL 审计约束的 bounded correlation request ID。相同 invoice identity
      的双连接首次导入还通过 PostgreSQL transaction
      advisory lock 实测串行化：同 checksum 返回同一 import/line identity，不同 checksum 稳定冲突。
      生产环境仍需分别用真实 AWS/GCP/Azure workload identity 和账单导出对象
      执行验收；Migration `000068` 已用 append-only Worker claim ledger 补齐 tenant-owned Worker 的 per-period、
      per-tariff request delta；Execution/Cleanup 的同 request ID 并发 claim 已在真实 PostgreSQL 证明线性化为一次
      ledger/receipt 写入与一次 replay，OrbStack final5 也已确认 Migration 68 在当前镜像应用。Migration `000075`
      又以 1:1 immutable ReleaseFact 覆盖 Execution 与 Workspace Cleanup 的全部生产释放路径，分别冻结业务
      `releasedAt` 和入账 `recordedAt`；OrbStack 临时 PostgreSQL 17 已验证 scope/immutable/exact replay，以及
      Completion 的 Lease 删除与唯一 ReleaseFact 原子提交。第一阶段不启用 deletion guard，需在存量 backfill 和
      minimum-writer-version gate 后再收紧。Migration `000077` 已新增显式 shared Target ledger coverage cutover、
      immutable Run/Slice；`closed-claim-interval-v1` 只处理 cutover 后 terminal 且 Claim/Release 完整的 incarnation，
      将 active interval 分给 exact Tenant、idle gap 保留给 platform，并通过累计整数舍入守恒；scope advisory
      lock 和数据库触发器拒绝重叠账期。review 后还补齐 SQLite 并发 replay、priced-resource 完整性、terminal-only
      右边界、隐藏 fallback boundary 合并、Target scope 更新保护、semantic Slice 唯一及 Tariff/资源快照绑定。
      OrbStack PostgreSQL [final4](../reports/stage-4-shared-cost-allocation-orbstack-pg-20260726-final4.md) 已通过两
      Tenant、两段 tariff、终止点=tariff end request、并发首次写/replay 及 direct DB negative gates。platform
      billing operator 已有原子 Coverage seal/get API 与显式闭合账期的 replay-safe shared sweep；每个 Worker 独立
      提交，partial failure 返回 `retry-required`，重启重放可补偿；OrbStack PostgreSQL
      [final2](../reports/stage-4-shared-cost-management-orbstack-pg-20260726-final2.md) 已验证并发 seal 唯一
      Coverage/审计和授权 sweep 的 1 Run/27 Slice replay。static closed-period runtime mapping、settlement delay、
      独立 scheduler lease 和 transaction write fence 也已接通；OrbStack PostgreSQL
      [final3](../reports/stage-4-shared-cost-scheduler-orbstack-pg-20260726-final3.md) 证明 standby 不执行、handoff
      fencing token=2、两个 epoch 各一条 system audit 且分摊图不重复。历史不完整数据、动态账期生成、
      account-level actual invoice 分摊及长期滚动预聚合继续 fail closed/保持待办。
- [ ] 多副本 Control Plane 的广域压力/混沌、真实 Kubernetes 长时运行和生产 soak 验收。
      Pod-bound identity 还要求 target Kubernetes credential 具备 `tokenreviews.create` 与精确 Pod GET 权限；部署
      验收必须覆盖 TokenReview 不可用、旧 UID replacement、Node partition、Failed/Unknown Pod 和 proof 重放。
      本地 disposable Kind final5 lane 已完成 6/6 必跑场景、Lease Guard v2 精确 Leader takeover 和实际 601 秒/
      10 周期 soak；OrbStack final14 又在 schema 73 镜像上通过 RBAC、Leader takeover、Control Plane failover
      3/3，并保持 Warm Pod 连续；OrbStack + disposable Kind final4 已补齐双真实 API 的跨 Cluster runtime-ready
      DR control path，并在两侧
      真实执行生产 verifier 的 TokenReview + Pod GET，但这些本地证据仍不能关闭托管云多可用区、实际跨域数据
      复制和生产时长验收项。
