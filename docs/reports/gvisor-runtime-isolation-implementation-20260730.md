# gVisor runtime isolation implementation report — 2026-07-30

状态：**独立 worktree 的实现与本地验证已完成；生产 release gate 未放行。**

本报告对应
[`gVisor Runtime Isolation Tier v0`](../plans/gvisor-runtime-isolation-tier-v0.md)，记录分支
`codex/gvisor-runtime-isolation` 当前实现事实。它不把本地 Kind、单节点、fixture 或单元测试外推为
生产多节点、真实 Codex/Claude 兼容矩阵或 GA 证据。

## 七项已确认决策的实现结果

| 决策 | 当前实现证据 |
| --- | --- |
| Kubernetes + RuntimeClass 首发 | `synara-gvisor` 固定 handler `runsc`；Pod、canary、live registration 均复验 exact RuntimeClass、Node 与 profile。 |
| profile 为 `gvisor-sandboxed-v1` | platform、agentd、Provider Host、API/UI、持久化约束和验收 schema 使用同一名称。 |
| 现有无配置 Target 保持 legacy；新 Target 固化 auto | 加载旧加密配置时规范化为 `legacy-native`；Create API 在加密前写入并严格校验显式的 `auto` 默认。 |
| 保留 `kubernetes-restricted-v1` 过渡基线 | 新 Kubernetes Target 默认 `gvisor → runc`、minimum restricted、`allow-lower`；未批准全局提升。 |
| fallback 不得低于 minimum | 有序 profile 比较在候选过滤前执行；`fail-closed` 和 minimum 不满足均在外部 materialization 前拒绝。 |
| 高风险企业 lane 默认 microVM | Cocoon backend 只接受 Firecracker / `microvm-isolated-v1`；gVisor 必须由上层策略改选独立 Target，不能在同一 backend 原地切换。 |
| Docker + runsc 仍为 trusted | Docker Engine 只接受 exact runtime 名 `runsc`，下发 `HostConfig.Runtime=runsc` 并加固 rootfs/caps/PID/CPU/memory/tmpfs；effective profile 固定 `single-tenant-trusted-v1`。 |

## 关键安全与一致性事实

- Runtime policy 更新要求 tenant `WorkerManage`、tenant-owned Target、drained/terminal Execution，并与
  Kubernetes 或 Docker reconciler 共用周期锁。更新会使 Target offline、删除旧观察并失效 Worker
  Manifest；审计不包含加密策略正文。
- Target 与 active promoted/canary Worker Release 都必须用 immutable
  `gvisorCompatibleProviders` 接受全部启用 Provider。显式 gVisor 缺失接受时在 Pod/Container 创建前返回
  `gvisor_provider_compatibility_unaccepted`；auto 只有在 minimum 和 fallback 同时允许时才能选 runc。
- Kubernetes Generation decision 在 Pod/SandboxClaim 前 append-only 持久化；Docker Worker 先于
  Execution 存在，因此在 claim 事务中、`execution.leased` 事件前，从 fresh Target observation 冻结
  `docker-engine` Generation decision。Docker 决策不得携带 RuntimeClass 或伪造 attestation。
- Node attestor 只挂载 exact read-only `/usr/local/bin/runsc` 与
  `/etc/containerd/config.toml`，不挂 runtime socket 或 writable host path。它要求 exact CRI plugin section
  和 `io.containerd.runsc.v1`，并发布 runsc version、runtime-binary SHA-256、config SHA-256、instance UUID
  与短 TTL heartbeat。binary/config/instance/eligible-node set 变化都会改变控制面 attestation digest。
- disposable canary 使用当前 agentd verifier，要求 gVisor `/proc/version`、文件/fsync、子进程与 loopback
  TCP 全部成功。正式 Worker 注册重新读取 live Pod、RuntimeClass、Node、restricted security context、资源、
  卷、PID gate 与 immutable Generation digest；任何漂移均拒绝注册。
- 安全 API 不返回 attestation digest、runtime socket、宿主路径或原始 probe；Target UI 分开展示
  configured/requested、available/detected、running/effective。legacy 编辑器预填等价显式策略，不会提交
  无效的空对象。
- Stage 5 aggregate 在指定 exact RuntimeClass 时自动追加真实 Provider 触发的
  `gvisor-compatibility` case，覆盖 Git、Node/Bun/npm/pnpm、Go/Rust/Java/Python、PTY、signal、文件
  metadata/watch 和 loopback TCP，并对每个 Provider × Node 聚合 duration/max-RSS P50/P95/P99。任何工具、
  probe 或样本缺失都 fail closed。

## 当前验证

- `go test ./...`：Control Plane 全量通过。
- Web：299 个 test files、3549 tests 通过；`tsc --noEmit` 通过。
- Provider Host：11 个 test files、172 tests 通过；`tsc --noEmit` 通过。
- Stage 3/5 Python：286 tests 通过；相关脚本 `py_compile` 通过。
- SQLite：append-only、scope、NULL-safe shape、lower-hex attestation digest 与 trusted Docker decision 通过。
- PostgreSQL 16 disposable 实例：迁移、immutable trigger、Provider 集合去重、Kubernetes gVisor、trusted
  Docker gVisor 与 observation 约束通过；实例在验证后已删除。
- Docker build：专用 `gvisor-node-attestor` target 通过；attestor 未复制进普通 Worker 镜像。
- Kubernetes：`kubectl kustomize deploy/kubernetes/gvisor` 通过。
- 无 KVM Kind/systrap：当前 attestor binary/config 双 digest、exact 单文件挂载、当前 agentd canary 与
  `Linux 4.19.0-gvisor` 诊断通过；详见单独的本地验收报告。集群、verify 镜像与容器已删除。
- 外部 x86_64 Linux host：固定 gVisor `release-20260727.0` 的 systrap 与 KVM 严格 canary、各 30 次
  顺序样本和各 8 路并发样本全部通过；native-host 负向检测、K3s 非影响核对和 exact cleanup 通过。
  该结果没有变更现有单控制面 K3s；详见
  [`external host standalone acceptance`](gvisor-runtime-external-host-acceptance-20260730.md)。
- 根级 workspace checks：`bun fmt`、`bun lint`、`bun typecheck` 全部通过；typecheck 8/8 packages
  successful。lint 保留仓库现有非阻断 warnings，没有 error。
- `git diff --check` 通过。

## 仍然阻断生产放行的外部证据

1. 受控凭据下的 Codex × Claude × 每个生产 Ready gVisor Node 完整矩阵。
2. 真实生产 CNI/metadata/credential/恶意 Issue 负向结果及 exact cleanup。
3. Provider restart/resume、Node loss、attestation expiry、RuntimeClass drift 与故障注入报告。
4. dispatch/ready、工具链、依赖安装、编译、网络和内存的 operator-approved P50/P95/P99 预算。
5. Docker 独立宿主 attestation、每 Execution volume、强制 egress allowlist 与跨租户验收；完成前 Docker
   始终不是平台共享多租户档位。

以上证据缺失不会让已实现代码回落到较弱 runtime；它只会保持 release gate 关闭。
