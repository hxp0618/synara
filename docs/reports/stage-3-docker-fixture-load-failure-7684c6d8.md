# Stage 3 Deterministic Managed Docker Network/Container-Loss Failure-Under-Load Gate

- Evidence date: `2026-07-17` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Clean gate commit: `7684c6d8b456671631ed6511fdd62c38b3d62d32`
- Gate run: `stage3-provider-acceptance-dec0c47d-ba11-496b-9241-a295c098402d`
- Result: **PASS FOR DETERMINISTIC TARGETED MANAGED DOCKER NETWORK AND CONTAINER-LOSS FAILURE UNDER LOAD;
  REAL PROVIDER, MULTI-HOST, MULTI-NODE, ROLLOUT AND PRODUCTION FAILURE/LOAD REMAIN OPEN**

## 1. Scope and evidence boundary

The shared `fixture-load-failure` suite reuses one managed Docker Target, two agentd Workers, four Codex/Claude
Sessions, Tenant quota, deterministic Provider Host Protocol 2.1 fixtures, Credential delivery, report format,
Secret scan and exact cleanup. It does not create another Target Driver, Provider fixture, Worker lifecycle, resource
owner or load orchestrator.

For each failure barrier, the runner resolves the selected active Execution through
`agent_executions.worker_id -> worker_instances.pod_name`, then requires the pod name to match exactly one running
managed container with the expected Target and `synara.io/worker-index` labels. The first barrier disconnects Worker
index 0 from the runner-owned network. The second removes Worker index 1's exact busy container and requires the
Docker reconciler to recreate the same logical Worker name with a new container ID, an advanced Worker incarnation,
a new instance UID and preserved named-volume sentinel.

For both faults, the peer Session must retain identical Events, pending Interaction, Worker and Generation while the
selected Execution emits `execution.recovering`, replaces its Generation-owned Request and Interaction, advances
exactly one Generation and reaches one terminal path. After the four failure-barrier Turns complete, the same four
Sessions immediately run the canonical 25 bounded load waves.

This proves deterministic single-host exact network/container-loss targeting, managed replacement, peer isolation,
Generation fencing and post-recovery quota/admission, Artifact and Checkpoint mechanics. It does not prove real
Provider behavior, multi-host or Kubernetes multi-node failure isolation, release rollout failure, production
latency/throughput SLA, sustained production load or production-duration soak.

## 2. Canonical configuration

The canonical run used:

- Target: one real managed `docker` Execution Target.
- Workers: `2` managed agentd containers.
- Sessions: `4`, split `2` Codex and `2` Claude Agent.
- Tenant `maxConcurrentExecutions`: `2`.
- Fault 1: `worker-network` against exact Worker index 0 for `8s`.
- Fault 2: `worker-container-loss` against exact Worker index 1.
- Each fault: `2` active Sessions plus `2` side-effect-free quota rejections.
- Post-recovery waves: `25`.
- Planned post-recovery completed Executions: `100`.
- Planned post-recovery quota rejections and recovery admissions: `50` each.
- Planned post-recovery overlap observations: `75`.
- Overall timeout: `900s`.
- Source worktree: clean.
- Capability Catalog SHA-256: `8d47c4a08cdce16f0420c911737f92bf6b28ba49c6310e5601b7bd434f671f70`.

The full report completed in `515,958 ms`, including the clean-revision Worker image build. The network case completed
in `11,953 ms`, exact container loss/replacement in `9,436 ms`, and post-recovery load in `145,464 ms`. The observed
post-recovery rate was `0.687` completed Executions/second, which is diagnostic evidence rather than a threshold or
SLA. Wave duration averaged `5,818.32 ms` (`4,301..6,845 ms`), and admission after a released slot averaged
`1,199.26 ms` (`1,054..1,374 ms`). All `11/11` report cases passed.

## 3. Exact network failure target and fencing

The network barrier held these two Approval interactions pending on distinct Workers:

| Role       | Provider      | Session                                | Execution                              | Worker                                 | Generation |
| ---------- | ------------- | -------------------------------------- | -------------------------------------- | -------------------------------------- | ---------: |
| Affected   | `codex`       | `19c15215-9771-4de7-8cc0-32ccd1b14904` | `a6e46c02-e205-459f-add7-be5568f196ec` | `255d27a5-ab4d-4855-88c0-7a3c44ee73de` |          1 |
| Unaffected | `claudeAgent` | `92f398ec-20ad-4b4c-a804-e68a9452e8cd` | `39de4483-2a13-4f94-b614-000adaa9d7f4` | `9710f760-52b5-47ab-8351-76346dd95141` |          1 |

The affected Worker mapped exactly to container
`synara-agentd-11e8f48d-f4a1-460f-a09e-7de36a77fb6d-0` (`0d8f3f688a50`), Worker index `0`, on the runner-owned
network `synara-stage3-7ce00bdcf00d`. The network was restored after `8,181 ms`.

The Execution ID and Worker ID remained stable while the Request changed from
`fixture-approval-generation-1-1` to `fixture-approval-generation-2-1`, the Interaction changed from
`2c59da9b-62fe-4984-b5e0-599d58450000` to `8ffb0b7c-5f43-4941-bc9f-bb9885ddad92`, and Generation advanced exactly
from 1 to 2. The affected Turn completed once on Generation 2; the peer completed once on its original Worker and
Generation 1.

## 4. Exact container loss and managed replacement

The second barrier rotated the Session order and held these two Approval interactions pending:

| Role       | Provider      | Session                                | Execution                              | Worker                                 | Generation |
| ---------- | ------------- | -------------------------------------- | -------------------------------------- | -------------------------------------- | ---------: |
| Affected   | `claudeAgent` | `0b0374f5-3d1e-4cb6-b808-51a0fab32845` | `04b24693-f713-497d-bc32-b383ea2b2bb8` | `9710f760-52b5-47ab-8351-76346dd95141` |          1 |
| Unaffected | `codex`       | `5352ab6f-dd22-4893-97d7-d71226c203bc` | `265fa85b-b541-474f-8309-212e49d295c0` | `255d27a5-ab4d-4855-88c0-7a3c44ee73de` |          1 |

The affected Worker mapped exactly to container
`synara-agentd-11e8f48d-f4a1-460f-a09e-7de36a77fb6d-1`, Worker index `1`. The runner wrote a sentinel into the
runner-owned named Workspace volume, removed only container `e7283328fd60`, and observed replacement container
`fbd0598deeca` under the same logical Worker name.

Managed replacement evidence was:

- `containerIdChanged=true`.
- Logical Worker ID remained `9710f760-52b5-47ab-8351-76346dd95141`.
- Worker incarnation advanced `1 -> 2`.
- Worker instance UID changed.
- Replacement Worker reached `online` before recovery continued.
- Named-volume sentinel `/data/.synara-stage3-provider-acceptance-volume` remained readable.
- Replacement completed in `1,463 ms`.

The affected Execution retained its ID and Worker ID while its Request changed from
`fixture-approval-generation-1-1` to `fixture-approval-generation-2-1`, its Interaction changed from
`bd6a1634-9619-4219-b7cd-958253cfebb0` to `d700a9bc-df51-4f98-8d54-3f28e7df84b1`, and Generation advanced exactly
from 1 to 2. The affected Turn completed once on Generation 2; the peer completed once on its original Worker and
Generation 1.

For both fault barriers, the runner recorded `peerSessionEventsUnchanged=true`,
`peerInteractionIdentityUnchanged=true`, `peerWorkerAndGenerationUnchanged=true`,
`targetedGenerationFenced=true`, `terminalCount=2`, `duplicateTerminal=false` and `pendingInteractionCount=0`.

## 5. Post-recovery quota, Artifact and Checkpoint evidence

Before each fault, the other two Sessions received `execution_quota_exceeded` with no Session Event or Interaction
mutation. After both failures, the same four Sessions completed:

- `100/100` unique Executions.
- `50/50` quota rejections with exact reason `execution_quota_exceeded`.
- `50/50` successful admissions after one slot was released.
- `75/75` two-Execution overlap observations on distinct Workers.
- Codex and Claude Agent each completed `50` Executions.
- Each Session completed exactly `25` Executions.
- Zero pending interactions after the final wave.
- `doubleExecution=false` and `duplicateTerminal=false`.

Every post-recovery load Execution had exactly one required lifecycle event and at least two Ready Artifact events:

| Event type                   | Count |
| ---------------------------- | ----: |
| `turn.created`               |   100 |
| `execution.leased`           |   100 |
| `workspace.ready`            |   100 |
| `execution.started`          |   100 |
| `content.delta`              |   100 |
| `item.started`               |   100 |
| `item.completed`             |   100 |
| `thread.token-usage.updated` |   100 |
| `request.opened`             |   100 |
| `request.resolved`           |   100 |
| `workspace.dirty`            |   100 |
| `checkpoint.created`         |   100 |
| `artifact.ready`             |   200 |
| `checkpoint.ready`           |   100 |
| `execution.completed`        |   100 |

## 6. Cleanup and Secret scan

Cleanup used exact runner ownership `f5be412e10054379ba76` and removed both current managed Worker containers, the
named Workspace volume, isolated network, Worker image and isolated Control Plane state. It recorded
`broadCleanupUsed=false`. Post-run Docker container, volume and network queries found no remaining acceptance
resources.

The output Secret scan covered `10` JSON, Markdown and redacted log files totaling `2,733,690` bytes. It checked five
known-secret canaries and private-key, AWS access-key, GitHub-token and OpenAI-style-key patterns; findings were empty.

The raw output directory was `.tmp/stage3-docker-fixture-load-failure-7684c6d8/`. Its report hashes were:

| Report   | SHA-256                                                            |
| -------- | ------------------------------------------------------------------ |
| JSON     | `d6f53a30d75f20bf0bd5cff83aa553fe24666828d01731004d1b3d6c445f8e41` |
| Markdown | `f7644c94597ff31cf1542cdad9b9a47b5e775cd08cea8355f87514c2f25022e5` |

## 7. Automated validation and DDL boundary

Before the clean-SHA canonical run, the same implementation passed:

- Acceptance Runner unit tests: `134/134`.
- All Stage 3 Python tests: `246/246`.
- Focused Go tests for `internal/quotas`, `internal/sessions`, `internal/executions`, `internal/agentd` and
  `internal/executiontargets`.
- Dirty-worktree managed Docker two-fault `2`-wave gate: `11/11`.
- `bun fmt`.
- `bun lint`: `0` errors and `238` existing warnings.
- `bun typecheck`: `9/9` workspace tasks.
- Python compilation and `git diff --check`.

This slice changes no database DDL. The checked-in forward migration boundary remains
`000041_diff_artifact_kind.sql`.

## 8. Remaining completion boundary

This report closes only deterministic single-host managed Docker exact network and container-loss failure targeting
under two-Worker load, managed same-Worker replacement, peer Session isolation, one-step Generation fencing and
post-recovery bounded load mechanics. Workflow L remains `partial`. Stage 3 still requires:

1. real Codex and Claude concurrent/load/failure evidence with controlled Credentials;
2. real SSH, Docker and Kubernetes consolidated Provider gates;
3. multi-host and Kubernetes multi-node scheduling, failure and load evidence;
4. Provider process crash and release rollout failure injection under load;
5. production latency/throughput objectives and sustained production-duration load/soak evidence.
