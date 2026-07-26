# Stage 4 OrbStack + Kind 双集群 DR 验收（final2）

## 结论

2026-07-26，本地双 Kubernetes API 灾难恢复通道通过。OrbStack 作为主集群，一次性 Kind 作为副集群；
源 Execution 的真实 Pod 和 agentd 在 OrbStack 达到运行就绪，Control Plane 在声明精确 DR readiness 后只提交
一个 successor，successor 的真实 Pod 和 agentd 在 Kind 达到运行就绪，原 OrbStack Pod 的精确 UID 随后消失。

本报告对应本地脏工作树，尚未提交、推送或部署到远端：

- 分支：`codex/saas-tenancy-user`
- 源提交：`59d1d7b8c28bccd305f6e0b23c191facfba5f14f`
- 工作树：`dirty = true`
- 机器可读证据：[stage-4-dual-cluster-dr-acceptance-20260726-final2.json](stage-4-dual-cluster-dr-acceptance-20260726-final2.json)
- 结果：`passed`，退出码 `0`
- Run ID：`20260725175633-43317-6984`
- UTC 时间：`2026-07-25T17:56:33Z` 至 `2026-07-25T17:59:01Z`

## 执行命令

```bash
SYNARA_DUAL_CLUSTER_EVIDENCE_FILE=docs/reports/stage-4-dual-cluster-dr-acceptance-20260726-final2.json \
  deploy/kubernetes/kind-dual-cluster-acceptance.sh
```

脚本对所有 OrbStack 操作显式指定 `--context orbstack`，Kind 使用隔离的临时 kubeconfig；执行前后的用户
current-context 保持不变。

## 环境和镜像身份

| 角色   | Kubernetes context                         | Server version | Node 数 |
| ------ | ------------------------------------------ | -------------- | ------: |
| 主集群 | `orbstack`                                 | `v1.34.8+orb1` |       1 |
| 副集群 | `kind-synara-dr-20260725175633-43317-6984` | `v1.33.1`      |       1 |

测试使用根 `Dockerfile` 的 `worker-acceptance` target 构建 run-owned 本地镜像，并在两套运行时核对精确镜像身份：

- Tag：`synara-worker:dual-cluster-20260725175633-43317-6984`
- Docker config ID：`sha256:eba049f7156d75b50f1b45bc62e1d84060afee7989dbaf213fa0da8a22779d97`
- BuildKit manifest digest：`sha256:eba049f7156d75b50f1b45bc62e1d84060afee7989dbaf213fa0da8a22779d97`

## 通过的控制路径断言

机器可读证据只保留以下九个 allowlisted boolean；本次全部为 `true`：

| 断言                              | 证明内容                                                                                       |
| --------------------------------- | ---------------------------------------------------------------------------------------------- |
| `missingReadinessFailedClosed`    | 未发布 destination readiness 时，failover 不产生持久化副作用                                   |
| `exactReadinessAccepted`          | 精确 source DR domain、watermark 和 backing-store readiness 可授权切换                         |
| `sourcePlacementImmutable`        | 源 Execution 的 Target/Region/Cluster 快照保持历史事实，不被改写                               |
| `singleSuccessor`                 | 首轮只提交一个 successor，重复 sweep 幂等且不再创建 successor                                  |
| `successorLineagePersisted`       | successor 与源 Execution、源 Recovery Bundle 和 leader fence 的 lineage 已持久化               |
| `obsoletePrimaryPodAbsent`        | 原 OrbStack Pod 的精确 UID 已消失，不以同名 Pod 混淆                                           |
| `sourceRuntimeReady`              | 源 Pod 为 Running/Ready，agentd 启动且零重启，并完成精确身份的 register/claim/heartbeat        |
| `successorRuntimeReady`           | successor Pod 为 Running/Ready，agentd 启动且零重启，并完成精确身份的 register/claim/heartbeat |
| `recoveryBundleIntegrityVerified` | failover 前验证 Recovery Bundle 的持久化 SHA-256 和 envelope 一致性                            |

临时详细证据为 2,108 bytes，SHA-256 为
`bb976ab820f6bfd65a8a814dd0486776640c0f49bbfb7738ab6b631d0005cbad`；最终 JSON 不保存 Token、CA、
API server 地址、内部 ID 或其他连接材料。

## Recovery Bundle 恢复正确性

本轮同时收口了两处恢复边界：

1. failover 在读取源 Recovery Bundle 后，先通过共享 canonical encoder 验证持久化 hash 和 envelope；篡改
   hash 或 envelope 会以明确冲突失败，并且不产生 successor、Event 或 Outbox 副作用。
2. `disaster-recovery` successor 的首次 Claim 会沿 predecessor ancestry 找到原始 `turn.created`，因此源
   Generation 当前 Turn 的上下文和 Result Artifact 会进入 successor Resume Snapshot，不会被误当成初始空历史。

SQLite 端到端测试覆盖 Source Claim、当前 Turn Context/Artifact、Release/Re-Claim replay、DR successor
Claim/replay、predecessor Bundle lineage 以及最终 Resume Snapshot 完整性。

## 安全与清理

- 每个集群只创建 run-labelled 的临时认证 Namespace、ServiceAccount、最小权限 RBAC 和目标 Namespace。
- TokenRequest 凭证只经进程环境传入，没有落入最终证据。
- 删除前同时核对 run label 和创建响应中的 UID，再使用 Kubernetes API 的 UID precondition 删除并等待原 UID
  消失；没有执行 name-only delete。
- 一次性 Kind 集群、run-owned OrbStack 临时资源和临时 Worker image tag 均已删除。
- 未创建、修改或删除 `synara-system` 中的对象；验收后其两个 Control Plane Pod 仍为 Ready，PostgreSQL 和
  MinIO 仍健康，既有 Warm Pod 仍为 Ready 且零重启。

## 证据边界

本次结果是可重复的本地双集群 control-path 验收，不等价于生产跨 Region 灾备：

- Worker API 是受限验收 stub，只验证 agentd register、heartbeat 和 claim；未验证完整 Provider 会话消费恢复包。
- `podBoundWorkloadIdentityVerified = false`，没有声称完成 AWS/GCP/Azure 或生产 Kubernetes Workload Identity。
- `replicationVerified = false`；测试声明并门禁精确 `replicatedThroughAt`，但没有实际复制 Object Storage、
  Checkpoint、Memory 或 Metadata backing store。
- Metadata 使用隔离并执行迁移的 SQLite；没有覆盖多副本 PostgreSQL leader contention。
- OrbStack 与 Kind 都是本机单节点，逻辑 Region/Cluster 不代表云上多可用区网络、存储、负载均衡或故障域。
- 生产多可用区、真实数据复制、云 Workload Identity、长周期 soak 和广域压力/混沌仍是 Stage 4 部署验收项。

## 验证

- `go test ./... -count=1`：通过。
- `go test ./internal/sessions -count=1`：修正 fixture 后通过。
- `deploy/kubernetes/validate-resilience-assets.sh`：通过。
- `bash -n deploy/kubernetes/kind-dual-cluster-acceptance.sh`：通过。
- `git diff --check`：通过。
- 未运行 Bun workspace 检查；本次没有前端变更，且仓库约束要求只有用户明确要求时才运行。

首次运行留下的失败证据
[`stage-4-dual-cluster-dr-acceptance-20260726.json`](stage-4-dual-cluster-dr-acceptance-20260726.json)
证明数据库确实拒绝在 `queued` 状态伪造 claimed Generation 的 Recovery Bundle。fixture 改为先进入合法
`leased` 边界后，final2 才通过；失败证据保留，未被覆盖。
