# Stage 4 OrbStack + Kind 双集群 DR 与 Pod-bound 身份验收（final3）

## 结论

2026-07-26，本地双 Kubernetes API 灾难恢复通道在 final2 的 runtime-ready/恢复正确性基础上，进一步通过
真实 Pod-bound Worker 身份验收。OrbStack 作为主集群，一次性 Kind 作为副集群；两侧 agentd 的注册均由生产
`VerifyKubernetesWorkerRegistration` 实现调用各自真实 API Server 的 TokenReview 和 Pod GET 后才被验收 stub
接受。未绑定到 Pod/Target audience 的普通 Kubernetes API 凭证在两侧都被拒绝。

本报告对应本地脏工作树，尚未提交、推送或部署到远端：

- 分支：`codex/saas-tenancy-user`
- 源提交：`59d1d7b8c28bccd305f6e0b23c191facfba5f14f`
- 工作树：`dirty = true`
- 机器可读证据：[stage-4-dual-cluster-dr-acceptance-20260726-final3.json](stage-4-dual-cluster-dr-acceptance-20260726-final3.json)
- 结果：`passed`，退出码 `0`
- Run ID：`20260725180933-52617-31640`
- UTC 时间：`2026-07-25T18:09:33Z` 至 `2026-07-25T18:11:54Z`

## 执行命令

```bash
SYNARA_DUAL_CLUSTER_EVIDENCE_FILE=docs/reports/stage-4-dual-cluster-dr-acceptance-20260726-final3.json \
  deploy/kubernetes/kind-dual-cluster-acceptance.sh
```

脚本对所有 OrbStack 操作显式指定 `--context orbstack`，Kind 使用隔离临时 kubeconfig；执行前后的用户
current-context 保持不变。

## 环境与镜像

| 角色 | Kubernetes context | Server version | Node 数 |
| --- | --- | --- | ---: |
| 主集群 | `orbstack` | `v1.34.8+orb1` | 1 |
| 副集群 | `kind-synara-dr-20260725180933-52617-31640` | `v1.33.1` | 1 |

- Worker tag：`synara-worker:dual-cluster-20260725180933-52617-31640`
- Docker config ID：`sha256:839b848c7dd6feb36f728ef2ca888d6eb449a01b177cfd1f964321b05cdcb7ba`
- BuildKit manifest digest：`sha256:839b848c7dd6feb36f728ef2ca888d6eb449a01b177cfd1f964321b05cdcb7ba`

## Pod-bound 身份证明

每个 Execution Pod 挂载 Kubernetes 颁发、10 分钟、Target-specific audience 的 projected ServiceAccount token，
并通过 Downward API 绑定自己的 Pod name/UID。验收注册端调用生产验证器并在真实 API Server 上验证：

1. TokenReview `authenticated=true` 且 audience 精确匹配 Execution Target。
2. ServiceAccount username、Pod name 和 Pod UID token extra claims 精确匹配。
3. Pod GET 返回同一 UID，Pod 未 terminating，Target ownership 标签和 ServiceAccount 匹配。
4. `execution-pinned` mode 与 assigned Execution ID 来自受信 Pod 标签，而不是 Worker 自报。
5. requested CPU/Memory/Ephemeral Storage 来自受信 Pod spec，而不是 Worker JSON。
6. wrapper 自身的 Kubernetes API TokenRequest 凭证不能冒充 Pod-bound Worker token。

Wrapper 只给控制凭证增加 `tokenreviews.create`，Pod GET 仍通过目标 Namespace 内 RoleBinding 授权；最终证据不
保存 projected Pod token、控制凭证、CA 或 API server 地址。

## 通过的断言

本次十个 allowlisted assertions 全部为 `true`：

| 断言 | 证明内容 |
| --- | --- |
| `podBoundWorkloadIdentityVerified` | 两侧生产身份验证器均完成真实 TokenReview + Pod GET，未绑定 API 凭证被拒绝 |
| `missingReadinessFailedClosed` | 未发布 destination readiness 时 failover mutation-free |
| `exactReadinessAccepted` | 精确 source domain、watermark 与 required backing-store readiness 放行 |
| `sourcePlacementImmutable` | 源 Execution 的 Target/Region/Cluster 快照未改写 |
| `singleSuccessor` | 首轮只提交一个 successor，重复 sweep 幂等 |
| `successorLineagePersisted` | predecessor Execution/Bundle、routing snapshot 与 leader fence 已持久化 |
| `obsoletePrimaryPodAbsent` | 原 OrbStack Pod 的精确 UID 已消失 |
| `sourceRuntimeReady` | 源 Pod/agentd Running/Ready、零重启并完成 register/heartbeat/claim |
| `successorRuntimeReady` | successor Pod/agentd Running/Ready、零重启并完成 register/heartbeat/claim |
| `recoveryBundleIntegrityVerified` | failover 前验证 persisted Recovery Bundle SHA-256 与 envelope |

临时详细证据为 2,246 bytes，SHA-256 为
`233707b3a95ea6209384b82be4dc1f5037da8f67e058c451289bc705f3558cab`。

## 安全与清理

- 所有临时 Kubernetes 对象均绑定 run label 与创建响应 UID。
- 清理使用 API Server 强制的 UID precondition，并等待原 UID 消失；没有 name-only delete。
- 一次性 Kind、run-owned OrbStack Namespace/RBAC 和临时 Worker image tag 均已删除。
- 未创建、修改或删除 `synara-system` 对象；验收后其两个 Control Plane Pod、PostgreSQL、MinIO 和既有 Warm Pod
  均保持 Ready，Warm Pod 零重启。

## 证据边界

- Worker API 仍是 `bounded-stub-with-production-kubernetes-identity-verifier`。它真实运行生产 Kubernetes
  身份验证器，但签发的是非持久验收 Worker token；`fullWorkerRegistrationPersistenceVerified = false`，没有声称
  覆盖生产 `/v1/workers/register` handler、Worker row 或 `kubernetes-pod-bound-v1` trust-mode 持久化。
- `replicationVerified = false`；`replicatedThroughAt` 是 readiness publisher 的授权断言，不是实测 RPO。
- 没有验证 AWS/GCP/Azure 云 Workload Identity、实际 Artifact/Checkpoint/Memory 复制与 successor Provider 消费。
- Metadata 使用隔离并执行迁移的 SQLite；未覆盖多副本 PostgreSQL failover leader。
- OrbStack 与 Kind 均为本机单节点，不代表云上独立 Region/AZ、存储、负载均衡或网络故障域。

## 验证

- `go test ./internal/sessions ./internal/executiontargets -count=1`：通过。
- 真实 OrbStack + disposable Kind final3：通过，测试本体耗时约 39 秒。
- `deploy/kubernetes/validate-resilience-assets.sh`：通过。
- `bash -n deploy/kubernetes/kind-dual-cluster-acceptance.sh`：通过。
- `git diff --check`：通过。
- 未运行 Bun workspace 检查；本次没有前端变更，且仓库约束要求只有用户明确要求时才运行。

final3 supersedes [final2](stage-4-dual-cluster-dr-acceptance-20260726-final2.md) 作为当前双集群验收结果；final2
仍保留为首次 runtime-ready/Recovery Bundle 收口证据。
