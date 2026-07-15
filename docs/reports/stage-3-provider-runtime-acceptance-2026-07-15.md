# Stage 3 Provider Runtime / Remote Worker 当前验收报告

> 本文保留 2026-07-15 至 2026-07-16 的 dirty-worktree 与历史 fixture 证据。真实 Codex/Claude 的
> clean-commit Local 产品路径后续证据见
> `docs/reports/stage-3-provider-runtime-acceptance-fb9e25ec.md`。

- Evidence window: `2026-07-15`–`2026-07-16` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Starting clean checkpoint: `93da9b29`
- Current evidence source: uncommitted Stage 3 worktree
- Result: **PARTIAL — IMPLEMENTATION EVIDENCE PASS, RELEASE GATE OPEN**

本报告汇总当前工作树已经实际运行的证据。由于证据包含未提交改动，它不能替代最终 Commit 的发布报告；
真实 Codex/Claude 四 Target Release Gate、registry-pushed multi-arch Worker Image、多节点生产 Kubernetes
和长时间 soak 仍未完成。

## 1. 证据边界

| 证据                                          | 经过真实 Provider | 经过 Control Plane/agentd | Target lifecycle       | 结论                                    |
| --------------------------------------------- | ----------------- | ------------------------- | ---------------------- | --------------------------------------- |
| Codex/Claude Local Provider Host direct smoke | 是                | 否                        | Local process only     | Adapter Describe/Start/Send 可用        |
| deterministic Local core/failure suite        | 否，fixture       | 是                        | Local Supervisor       | 产品通路与故障分类实现期通过            |
| deterministic Docker core/network suite       | 否，fixture       | 是                        | managed Docker         | replacement/network/recovery 实现期通过 |
| deterministic Kubernetes failure-only matrix  | 否，fixture       | 是                        | owned disposable Kind  | failure/canary 编排实现期通过           |
| SSH fixture 13/13                             | 否，fixture       | 是                        | disposable OrbStack VM | 2026-07-14 历史证据                     |
| Kubernetes fixture 13/13                      | 否，fixture       | 是                        | owned disposable Kind  | clean commit `2763ebd3` 历史证据        |

禁止从这些结果推出以下结论：

- “真实 Codex/Claude 已通过四 Target”。
- “Kubernetes image-canary 已证明 immutable Revision promote/rollback”。
- “单节点 Kind 已证明生产多节点 Drain、PDB、CNI 或云厂商 Eviction”。
- “本地构建镜像已证明 Registry multi-arch reproducibility”。

## 2. 真实 Local Provider Host smoke

Provider Host 从当前源码重新构建：

```bash
bun run --cwd apps/provider-host build
```

使用项目要求的 Node.js 24 运行真实 Host，而不是 deterministic fixture：

```text
/opt/homebrew/opt/node@24/bin/node apps/provider-host/dist/index.mjs --protocol-v2
```

两个 Provider 分别在独立进程和临时 Workspace 中发送 Provider Host Protocol `2.1` 命令：

```text
Describe -> StartSession -> SendTurn -> StopSession
```

### 2.1 Codex

| 项目             | 结果                          |
| ---------------- | ----------------------------- |
| Host build       | `0.2.0`                       |
| Adapter          | `codex-app-server-v2`         |
| Runtime          | Codex CLI `0.144.4`           |
| Compatible range | `>=0.144.1 <0.145.0`          |
| Describe         | `Result`                      |
| StartSession     | `Result`                      |
| SendTurn         | `11 Event + Result`           |
| StopSession      | `Result`                      |
| Output           | exact `SYNARA_CODEX_SMOKE_OK` |
| Resume Cursor    | present                       |
| Process          | exit `0`, stderr empty        |

SendTurn Event：

- `content.delta` × 8。
- `thread.token-usage.updated` × 1。
- `runtime.warning` × 2。

两条 warning 已定向复跑并读取安全内容：一条来自本机 Codex `chronicle` under-development feature
提示，一条来自 Skills description 2% context budget 提示；二者都不是认证、Protocol、Runtime 兼容或
Execution 终态失败。

### 2.2 Claude Agent SDK

| 项目             | 结果                                     |
| ---------------- | ---------------------------------------- |
| Host build       | `0.2.0`                                  |
| Adapter          | `claude-agent-sdk-v2`                    |
| Runtime          | `@anthropic-ai/claude-agent-sdk 0.3.207` |
| Compatible range | `>=0.3.207 <0.4.0`                       |
| Describe         | `Result`                                 |
| StartSession     | `Result`                                 |
| SendTurn         | `11 Event + Result`                      |
| StopSession      | `Result`                                 |
| Output           | exact `SYNARA_CLAUDE_SMOKE_OK`           |
| Resume Cursor    | present                                  |
| Process          | exit `0`, stderr empty                   |

SendTurn Event：

- `content.delta` × 10。
- `thread.token-usage.updated` × 1。

Codex 使用现有 ChatGPT 登录，Claude 使用现有 OAuth 登录；本次没有外部认证阻塞。临时 Workspace
和 Host 进程均已清理。该 smoke 没有经过 Local Supervisor、Control Plane、agentd、Lease 或 Artifact，
所以只证明真实 Adapter 的最小 Host Contract 闭环。

## 3. Deterministic 产品通路证据

默认 Runner 使用 Provider Host Protocol `2.1` deterministic fixture，但 Target driver 使用真实
Control Plane、agentd、Worker Protocol 和产品生命周期。

### 3.1 Core suites

- Local deterministic Codex fixture：`12/12 pass`。
- Docker deterministic Codex fixture：`14/14 pass`。
- Docker 覆盖 managed Worker replacement、Workspace continuity、Control Plane restart、第二 Turn、
  32 KiB Terminal Preview、三个 `terminal_log` Artifact 和精确资源清理。

这些运行来自未提交工作区，必须在最终 Commit 后重新生成 immutable release report。

### 3.2 Local Provider fault matrix

Workspace-local report：
`.tmp/stage3-provider-acceptance-results/local-failure-matrix-cleanup-evidence-current-source/acceptance-report.md`

- Result：`pass`。
- `provider-malformed` -> `protocol_violation`，后续 Turn 成功。
- `provider-oversized` -> `protocol_violation`，后续 Turn 成功。
- `provider-crash` -> `provider_unavailable`，后续 Turn 成功。
- Baseline、post-failure continuity、cleanup 和 output Secret scan 全部通过。

### 3.3 Docker network interruption

Workspace-local report：
`.tmp/stage3-provider-acceptance-results/docker-network-cleanup-evidence-current-source/acceptance-report.md`

- Result：`pass`。
- 只断开 Runner-owned Worker container 的网络。
- Outage 跨过 acceptance Lease TTL 后，同一 Execution 从 Generation 1 恢复到 2。
- 旧 Approval Interaction/Request 被 fenced，新 Generation Request 可 Resolve 并完成。
- Post-failure continuity、cleanup 和 output Secret scan 通过。

### 3.4 Kubernetes failure/canary matrix

Workspace-local report：
`.tmp/stage3-provider-acceptance-results/kubernetes-failure-canary-cleanup-evidence-current-source/acceptance-report.md`

- Result：`pass`。
- Worker-only network interruption。
- 精确 Node cordon/drain/uncordon。
- `policy/v1` Eviction，包含 Pod UID precondition。
- 独立 Target/Namespace/Session 的 image-canary 与 baseline continuity。
- Cleanup 和 output Secret scan 通过，owned Kind 和精确自动构建镜像无残留。

Image-canary 使用同内容的 Runner-owned alias，只证明 Target isolation、image selection、Worker Manifest
discovery 和 baseline continuity；它不是 `000037` Release Revision 的正式 registry rollout。

## 4. 历史 clean-commit fixture 证据

### 4.1 SSH

2026-07-14 的 deterministic Codex fixture 在 disposable OrbStack Ubuntu 24.04 VM 上 `13/13 pass`，覆盖：

- one-time SSH Credential 和 pinned Host Key mismatch 负例。
- install/upgrade/revoke。
- sshd restart 和 systemd Worker replacement。
- Workspace continuity、Control Plane restart 和第二 Turn。
- 精确 VM 清理与报告/日志 Private Key scan。

该证据不是当前 Commit，也不是真实 Codex/Claude Adapter Release Acceptance。

### 4.2 Kubernetes

Clean commit `2763ebd3` 的 deterministic Codex fixture 在 owned disposable Kind 上 `13/13 pass`，见
`docs/reports/stage-3-kubernetes-provider-fixture-acceptance-2763ebd3.md`。它覆盖 Pending Approval Pod
delete、Generation 1→2、Interaction replacement、Artifact/User Input/Provider Error、Control Plane
restart、第二 Turn 和 Event Sequence `1..57`。

该证据早于当前 `000032`–`000040` 与 rollout/revocation/credential changes，不能当作当前发布镜像验证。

## 5. DDL 与数据库证据

当前 checked-in migration boundary：`000040`。

| Migration | 已验证范围                                                                           |
| --------- | ------------------------------------------------------------------------------------ |
| `000032`  | Advanced Turn graph/shape、Primary Command immutable rules、PostgreSQL/SQLite safety |
| `000033`  | Credential Scope backfill、selector/auto-select shape、PostgreSQL/SQLite safety      |
| `000034`  | Worker administrative revocation、tombstone、Lease/Claim/re-registration fencing     |
| `000035`  | Credential Binding ownership、stage、immutable generation Grant                      |
| `000036`  | legacy Project Git authority backfill、clear 和 write rejection                      |
| `000037`  | Revision/Policy/Transition、Worker/Execution release shape、多 Revision Target       |
| `000038`  | 四个 Credential Binding FK lookup indexes，PostgreSQL/SQLite                         |
| `000039`  | 每 Target 唯一 active `worker_image_pull` Binding、歧义 fail-closed 与可重试修复     |
| `000040`  | 当前 Worker Release Policy 与最新 immutable Transition 一致性 fencing                |

当前工作树实际通过：

```bash
cd services/control-plane
go test ./...
go vet ./...
go test -race ./internal/secretguard ./internal/agentd
```

真实 PostgreSQL 17：

```bash
SYNARA_TEST_DATABASE_URL='postgres://.../synara_test?sslmode=disable' \
SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL='postgres://.../synara_test?sslmode=disable' \
  go test ./...
```

全量通过。全量 PostgreSQL 首次运行暴露 Eventstream fixture 使用随机 configured installation ID 与共享
数据库 installation 冲突；修复为复用 persisted installation 后，定向测试和全量重新运行均通过。
临时 PostgreSQL 容器已删除。

## 6. SecretGuard 与 Credential evidence

- Provider Host 和 Provider 子进程从显式 allowlist 环境构造，不继承 Worker/Lease/Control Plane、
  Database、Object Store、Cloud 或 ambient proxy Secret。
- Provider Credential 通过 anonymous FD 3，Codex/Claude Payload 使用严格 allowlist。
- `000033`、`000035`、`000036`、`000038`、`000039` 建立 Scope、Binding、Grant、legacy authority
  retirement 和唯一 Target image-pull authority。
- agentd 只解析当前 Generation、当前 stage 的 Credential；Git HTTPS 使用 AskPass，SSH 使用 pinned
  Host Key 和临时 Agent。
- `worker_image_pull` 只在 Control Plane Target provisioner 中解析，不进入 Worker Workload。
- deterministic Acceptance 的 output Secret scan 全部通过，findings 为空。
- Git ignore 审计移除 45 MB `services/control-plane/api` 构建产物，并为 `agentd`、`api`、`metadata`
  三个 cmd 输出增加精确 ignore 规则。

仍缺真实 Provider 在四 Target 的 stdout/stderr、Crash dump、Artifact/Terminal、集中日志和长期运行
Secret canary；当前证据不能证明生产环境零泄漏。

## 7. 当前未关闭项

1. 最终 Commit 后尚未重跑本报告所引用的全部 current-worktree acceptance。
2. 真实 Codex/Claude 尚未经过 Local Supervisor/agentd 的完整 Local Release Suite。
3. 真实 Codex/Claude 尚未经过 SSH、Docker、Kubernetes Target Release Gate。
4. Worker Release reconciler/rollout 的最终代码评审和同 Commit 运行门禁尚未在本报告中记录。
5. registry-pushed immutable multi-arch Image、签名、SBOM 和 reproducibility 尚未证明。
6. 多节点生产 Kubernetes、PDB、CNI enforcement、云厂商 Eviction 和真实升级压力尚未证明。
7. 多 Turn 长 Session、多 Provider 并发、重复 Compact/Checkpoint/Resume 和 Retention soak 尚未执行。
8. 真实 Provider large terminal/generated file/diff 和完整 auth/rate-limit failure matrix 尚未关闭。

## 8. 发布决定

当前决定：**不批准将 Stage 3 标记为 Release Gate complete**。

可以确认的范围：

- 当前真实 Codex/Claude Provider Host 最小 Local Contract 可运行。
- deterministic Control Plane/agentd/Worker/Target 产品通路和选定故障矩阵通过。
- DDL `000032`–`000040` 可追踪，并有 PostgreSQL/SQLite 覆盖。
- SecretGuard、Credential Binding/Grant、Worker revocation 和 Release Policy 已形成实现期基线。

发布前必须完成
`docs/release-checklists/stage-3-provider-runtime-remote-worker.md`，并按
`docs/runbooks/worker-release-rollout.md` 生成最终 Commit 的真实 rollout/rollback 证据。
