# Fast-Provision Runtime v0（PROPOSAL——未实现，重构输入）

状态：提案草案。本文不是实现契约；在被采纳并实现前，不得作为任何验收或行为的依据。它把
[方向评估](cloud-agent-direction-assessment-20260726.md) 的建议具体化为可评审的契约形状。

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

## 7. 开放问题

- microVM 层落在何处：自管 Firecracker on bare metal / Kata on K8s / 托管（e2b 自托管、Fly
  Machines）——隔离、成本、运维三角需要 spike 数据。
- 内存快照的合规边界：快照文件的加密、驻留时长、租户隔离证明需要单独安全评审。
- `guaranteed-warm` 的空闲成本模型与 min-idle 自动伸缩策略（可复用 queue-depth 指标驱动）。
