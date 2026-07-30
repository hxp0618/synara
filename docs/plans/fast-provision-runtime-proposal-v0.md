# Fast-Provision Runtime v0（ARCHITECTURE DECIDED——未实现）

状态：2026-07-28 已冻结 microVM 位置与可信边界；snapshot-restore 仍未实现，本文不得作为运行期
验收证据。它把
[方向评估](cloud-agent-direction-assessment-20260726.md) 的建议具体化为可评审的契约形状。
不具备 KVM 的普通 Linux VM 所对应的 gVisor 中间隔离档位另见
[`gVisor Runtime Isolation Tier v0`](gvisor-runtime-isolation-tier-v0.md)；其实现与本地验证状态以
链接文档为准，在真实 Provider/性能 release gate 放行前仍不能据此升级
平台默认隔离声明。

## 0. 目标与 SLO

- 交互式 Session：dispatch → Provider `session.started` ready，P95 ≤ 2.5s、P99 ≤ 5s（warm 命中）。
- suspended → 可继续输入：P95 ≤ 1s（snapshot-restore 层）/ ≤ 15s（现有 checkpoint 层）。
- 度量沿用现有 trailing-30d Generation facts；新增 `provision_tier` bounded label
  （`snapshot-restore | guaranteed-warm | cold`）。SLO 写入 Stage 4 完成条件与 release checklist。

## 1. 三层供给层级（Pool mode 扩展，不动领域模型）

| tier               | 语义                                                          | 目标延迟 | 隔离/成本 |
| ------------------ | ------------------------------------------------------------- | -------- | --------- |
| `snapshot-restore` | microVM 内存快照 fork/restore（Firecracker/Cloud Hypervisor） | 100ms–1s | 强/高     |
| `guaranteed-warm`  | min-idle ≥ N 的预注册 Worker + 预启动 Provider Host           | 1–3s     | 中/中     |
| `cold`             | 现有 execution-pinned / general pool 路径                     | 现状     | 强/低     |

- `guaranteed-warm` 与现有 `warm` 的差异：min-idle 是 Reconciler 的硬补齐目标（缺口即告警），
  Placement 对 interactive lane 可等待短暂补齐而非静默降级；现有"软偏好"语义保留给 batch。
- 预启动 Provider Host 到 `Describe` 完成即停：凭证仍在 Claim 后经既有 Grant resolve 注入
  （FD 3 路径不变），Manifest/attestation 验证移到入池时。Claim 事务只保留 fencing + Grant 创建。

## 2. snapshot-restore 层的契约要点

- 新 Execution Target capability：`snapshotRestore`，与 `worker-attested-v1` /
  `kubernetes-pod-terminal-v1` 并列为第三种 suspend completion mode `microvm-snapshot-v1`。
- 快照身份 = (Worker Manifest digest, Provider 版本, base image digest, snapshot digest) 四元组，
  进入 Worker Manifest 与 Recovery Bundle；模板快照按 Release Revision 构建，随 canary/promote/
  rollback 生命周期走现有 Release Policy。
- restore 后的实例是**新的物理 incarnation**：沿用 instanceUid 轮换、Lease 冻结、Generation
  fencing 全套现有语义；内存快照绝不包含 Credential 明文（Grant 在 restore 后重新 resolve），
  也不包含 Workspace 私有 repo 之外的租户数据。
- suspend 侧：quiesce receipt 语义不变，"介质"从对象存储 checkpoint 换为本地/池化内存快照 +
  异步 checkpoint 兜底（快照丢失时回落现有 recovery 路径，fail closed 不重放副作用）。

## 3. Workspace cache-first

- Claim 放行条件从"网络 fetch 成功"改为"Grant resolve 成功"（Grant 已是每 Claim 必经、可即时
  吊销的权威）；网络 fetch 转为后台刷新，Session Event 显式携带 `workspaceFreshness:
fresh | stale(<age>)`。
- 凭证吊销后的在途 Turn：下一次 Grant 续期失败即停止 Provider（现有 access broker 语义已覆盖）。
- 物化介质：git-cache/私有 repo 分离结构保持；Kubernetes 增加 volume-snapshot/overlayfs 选项作为
  materialization 加速，不改变 Checkpoint 恢复权威。

## 4. 双 lane

- Session 创建时冻结 `lane: interactive | batch`（默认 interactive；Automation 默认 batch）。
- interactive → `snapshot-restore`/`guaranteed-warm`；batch → `cold` + 现有 fairqueue。
- 计费：`provision_tier` 进入 requested-resource-seconds 事实与 tariff（新增
  `snapshot_restore_hour_rate_micros` 等费率列时沿用 append-only tariff 版本化）。

## 5. 不变式（与现行契约的兼容承诺）

- Worker Protocol、Runtime Event、Grant、fencing、Checkpoint/Artifact 权威、suspend receipt
  语义全部不变；本提案只新增 Pool mode、completion mode、capability 与 bounded label。
- fail closed 原则不变：快照缺失/身份不符回落 cold 路径或失败，绝不静默降级安全边界。

## 6. 落地顺序建议

1. SLO 门禁 + `provision_tier` label（纯度量/文档，1 个迁移）。
2. `guaranteed-warm`（Reconciler min-idle 硬补齐 + Host 预启动，无新基础设施）。
3. Workspace cache-first（Grant 放行 + freshness 标注）。
4. `snapshot-restore`（新运行时层，最大件；先单 Target 原型，后接 Release Policy）。

## 7. 2026-07-28 microVM 决策：Kubernetes 控制、自管 Firecracker 数据面

隔离与延迟合并评估后的选择是：**由 `sandbox-operator` 在专用 Linux/KVM bare-metal node pool
上管理自管 Firecracker microVM**。Kubernetes 保留 Placement、SandboxClaim、Release、配额和节点
生命周期控制面；Execution 的 Provider 数据面不作为普通 Pod 内进程运行。Kata RuntimeClass 可作为
兼容性 spike，但不作为 `snapshot-restore` 层的权威实现；第三方托管 runtime 也不能成为默认信任根。

可信边界冻结如下：

- agentd supervisor、Control Plane Worker Credential、Provider Credential broker、云/Kubernetes 身份和
  snapshot key 位于 guest 外；guest 只运行 Provider Host、Provider CLI 与其工具子进程。
- host agentd 与 guest Provider Host 复用 Provider Host Protocol v2 语义，但 transport 改为带
  Execution/Generation fence 的 vsock；guest 不获得 Control Plane bearer token。
- guest 只挂载本 Execution 的 Workspace/Runtime Output 介质，不挂载宿主目录、容器 runtime socket、
  Kube ServiceAccount、git cache 根或其他 Execution 的块设备。网络由 host tap/CNI egress policy 强制，
  guest 内配置不能扩大 allowlist。
- 模板快照只能在 Provider Credential、租户 Workspace 和私有内存进入 guest **之前**创建；快照按
  Release Revision 签名、加密、短期驻留。restore 后产生新 VM ID、Worker incarnation 和 task broker
  token，不能复用原实例身份。
- 新隔离声明预留为 `microvm-isolated-v1`，只有 KVM、jailer、vsock peer、rootfs/设备、egress、snapshot
  identity 与资源上限全部 attested 且通过负向逃逸测试后才能发布。未声明/未验真的 runtime 继续从
  多租户产品面排除，不能静默降级为普通容器。

选择理由：直接 Firecracker 同时提供独立 guest kernel 与可控 snapshot/restore；Kata 优先解决 Pod
兼容而非稳定的应用级内存快照接口，托管 runtime 则把身份、驻留与取证边界交给第三方。专用节点和
自管运维成本更高，但这条路径复用现有 SandboxClaim/Release/Allocation 权威，并且不会为低延迟再造
一套绕过 Stage 4 fencing 的调度系统。

仍需用 spike 数据关闭的不是“落在哪里”，而是实现验收：

- template build、restore、vsock ready、Workspace attach 各阶段 P50/P95/P99 与失败率；
- KVM/Firecracker/Jailer 版本矩阵、节点密度、内存超配与 noisy-neighbor 上限；
- 快照文件的 KMS envelope、驻留时长、删除证明与跨租户负向恢复；
- 失去 snapshot/节点时回落既有 cold recovery 的延迟与副作用不重放证明。

## 8. 其余开放问题

- 内存快照的合规边界：快照文件的加密、驻留时长、租户隔离证明需要单独安全评审。
- `guaranteed-warm` 的空闲成本模型与 min-idle 自动伸缩策略（可复用 queue-depth 指标驱动）。
