# Cloud Agent 方向评估：SaaS 秒级远端 Agent（2026-07-26）

操作人的产品北极星：**E2B 式体验——几秒内部署/恢复一个可用的远端 agent**。本文档以该标准
评估当前 Stage 4 轨迹，指出偏差、保留项与重构建议。它是
[`cloud-agent-docs-audit-20260726`](../reports/cloud-agent-docs-audit-20260726.md) 的姊妹篇：审计
回答"文档与实现是否一致"，本文回答"实现的方向是否对准产品目标"。

## 一、偏差判定：部分偏离，地基有效

### 偏差证据（全部取自现行文档）

1. **冷启动是观察指标，不是产品门禁。** stage-4 计划开放项："为交互式 Agent 建立冷启动硬上限；
   target-local release-aware Warm Pool 与实测 P50/P95/P99 已落地，生产 SLO 门禁仍未完成。"
   系统能精确测量慢，但没有机制强制快。
2. **热路径叠满正确性机器。** 一次冷启动 = 路由决策（Target lock + 全量复核）→ Pod 创建 → 镜像
   拉取 → agentd 注册（TokenReview + Pod GET）→ Claim（Recovery Bundle + Credential Grant +
   Scheduling Decision 同事务）→ Workspace 物化 → Provider Host 启动 → Describe/兼容门禁 →
   SendTurn。每段各自正确，串起来是秒到分钟级。
3. **Workspace 每 Turn 强制网络 fetch。** `workspace-v1`："performs a credential-authorized network
   Fetch on every Turn, so a warm cache cannot bypass a revoked Credential." 把凭证吊销检查绑在
   网络往返上，是正确性优先、延迟敌对的选择。
4. **Warm Pool 是软偏好不是保证。** `worker-pool-placement-v1`：warm 观察是 TTL 软提示，
   "never decremented or reserved by placement"，miss 静默回退冷启动。E2B 语义是"永远有热的"，
   当前语义是"有热的就用"。
5. **没有快照恢复层。** E2B 的核心是 Firecracker microVM + 内存快照（百毫秒级启动/fork）。当前
   suspend/resume 是 checkpoint 到对象存储 → 删 Pod → 新 Pod → 重物化——为成本回收设计，
   不为瞬时恢复设计。全部契约中不存在 microVM/CRIU/内存快照概念。
6. **近期投入重心。** `000077`–`000083` 全部是计费守恒、账期调度、签名 publisher——企业正确性；
   同期没有任何迁移攻冷启动路径。

### 有效的地基（不要推翻）

- 单一 Worker Protocol 跨 Local/SSH/Docker/Kubernetes；控制面权威 + Generation fencing；
  Artifact/Checkpoint/Memory 权威模型；Grant 化凭证；这些是 E2B 类产品缺而企业必需的。
- Target → Pool → Worker 分层和 capacity class 概念，天然容纳"快速路径"的插入——新增运行时
  层级不需要动领域模型。
- suspend/resume 的一次性 receipt + 精确终态证明语义是对的；换恢复介质不换语义。

## 二、重构建议（按杠杆排序）

1. **冷启动 SLO 升级为完成条件。** 交互式 Session 的 dispatch→provider-ready P95 ≤ 2–3s 写进
   Stage 4 完成条件与 release checklist；现有 trailing-30d 指标即为度量来源。
2. **Warm Pool 升级为保证池（interactive lane）。** min-idle ≥ N 的预注册 Worker；进一步预启动
   Provider Host 至 Describe 完成（凭证仍在 Claim 后经 Grant 注入，不破坏现有安全模型）。把
   链路中最贵的 Pod 调度 + Host 启动移出热路径。
3. **新增 `snapshot-restore` 运行时层级。** 作为新的 Pool mode/capacity class：Firecracker /
   Cloud Hypervisor 内存快照（优先）或 CRIU（退路）。目标：resume ≤ 500ms、模板 fork ≤ 1s。
   与现有 Kubernetes Pod 层并存——Pod 层继续服务批处理与强隔离场景。
4. **Workspace cache-first + 异步新鲜度。** 以"Grant 解析成功"作为凭证有效性放行（Grant 本就
   每次 Claim 必经且可即时吊销），网络 fetch 改为后台刷新 + 显式 stale 标注；git-cache/私有
   repo 分离结构适配 overlayfs/卷快照。
5. **正确性机器移出热路径。** attestation/manifest 验证移到入池时；Claim 事务保留 fencing，
   其余预计算。签名 publisher/健康 TTL 等控制面授权保持不变——它们本来就不在每 Turn 路径上。
6. **双 lane 产品化。** interactive（热、秒级、贵）/ batch（冷、便宜）；挂载点即 TODO 开放项
   "为自动化和批处理任务建立独立 Queue/Priority/Class"。计费侧已有 requested-resource-seconds
   与 warm hit/fallback 事实，可直接为两 lane 定价。

> 上述建议已具体化为可评审的契约形状草案：
> [`fast-provision-runtime-proposal-v0`](fast-provision-runtime-proposal-v0.md)（含三层供给层级、
> `microvm-snapshot-v1` completion mode、cache-first 放行条件、双 lane 与落地顺序）。

## 三、排序建议

Stage 4 剩余开放项中，建议把"冷启动硬上限 + 保证池"提到"多集群混沌/生产 soak"之前：后者验证
的是已建成机器的韧性，前者决定产品是否成立。剩余企业验收项（E4 云身份、跨域复制演练）不受
此排序影响，可并行。

## 四、非目标声明

本文不主张放弃企业正确性路线，也不主张重写既有契约。它主张的是：在既有分层上**新增**快速
路径层级，并把"秒级可用"从观察指标提升为验收门禁——这与全部现行契约兼容。
