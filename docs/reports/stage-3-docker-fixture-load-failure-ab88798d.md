# Stage 3 Deterministic Managed Docker Failure-Under-Load Gate

- Evidence date: `2026-07-17` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Clean gate commit: `ab88798de3a9a6000d842ad33c23a3fdea772d62`
- Gate run: `stage3-provider-acceptance-04d8ccb2-71cf-4a94-a20f-64d2c27579f9`
- Result: **PASS FOR DETERMINISTIC TARGETED MANAGED DOCKER NETWORK FAILURE UNDER LOAD; REAL PROVIDER,
  MULTI-HOST, MULTI-NODE AND PRODUCTION FAILURE/LOAD REMAIN OPEN**

## 1. Scope and evidence boundary

The shared `acceptance_runner.py` adds `fixture-load-failure` without introducing another Target Driver, Provider
fixture, Worker lifecycle, resource owner or cleanup path. It reuses the existing managed Docker two-Worker setup,
four Codex/Claude Sessions, Tenant quota, deterministic Provider Host Protocol 2.1 fixtures, Credential delivery,
Session APIs, report format, Secret scan and post-recovery `fixture-load` waves.

The failure path resolves the selected active Execution through the isolated metadata chain
`agent_executions.worker_id -> worker_instances.pod_name`, then requires that `pod_name` to match exactly one running
managed container with the expected Target and `synara.io/worker-index` labels. Only that container is disconnected
from the runner-owned Docker network. The other active Session must retain identical Events, pending Interaction,
Worker and Generation while the selected Execution emits `execution.recovering`, replaces its Generation-owned
Request and Interaction, advances exactly one Generation and reaches one terminal path.

After both failure-barrier Turns complete, the same four Sessions immediately run the canonical 25 bounded load
waves. This proves deterministic single-host network-failure targeting, peer isolation, Generation fencing and
post-recovery quota/admission, Artifact and Checkpoint mechanics. It does not prove real Provider behavior,
multi-host or Kubernetes multi-node failure isolation, process/container loss, production latency/throughput SLA,
sustained production load or production-duration soak.

## 2. Canonical configuration

The canonical run used:

- Target: one real managed `docker` Execution Target.
- Workers: `2` managed agentd containers.
- Sessions: `4`, split `2` Codex and `2` Claude Agent.
- Tenant `maxConcurrentExecutions`: `2`.
- Injected fault: `worker-network` against one exact busy Execution Worker.
- Network outage: `8s`, crossing the acceptance Lease TTL.
- Pre-fault active Executions: `2` on distinct Workers.
- Pre-fault quota rejections: `2`, both side-effect free.
- Post-recovery waves: `25`.
- Planned post-recovery completed Executions: `100`.
- Planned post-recovery quota rejections and recovery admissions: `50` each.
- Planned post-recovery overlap observations: `75`.
- Overall timeout: `900s`.
- Source worktree: clean.
- Capability Catalog SHA-256: `8d47c4a08cdce16f0420c911737f92bf6b28ba49c6310e5601b7bd434f671f70`.

The full report completed in `444,116 ms`, including the clean-revision Worker image build. The targeted failure case
completed in `12,401 ms`; post-recovery load completed in `152,292 ms`. Its observed rate was `0.657` completed
Executions/second, which is diagnostic evidence rather than a threshold or SLA. Wave duration averaged `6,091.68 ms`
(`4,848..7,543 ms`), and admission after a released slot averaged `1,167.54 ms` (`1,044..1,357 ms`). All `10/10`
report cases passed.

## 3. Exact failure target and Generation fencing

Before injection, both Approval interactions were pending on distinct Workers:

| Role       | Provider      | Session                                | Execution                              | Worker                                 | Generation |
| ---------- | ------------- | -------------------------------------- | -------------------------------------- | -------------------------------------- | ---------: |
| Affected   | `codex`       | `4f79d0d7-baf1-476d-bd1f-e1c39844994a` | `9263a66e-dbfa-46a1-b3fc-8f1c2c4a314c` | `2c9ac0cd-c061-4f27-867e-4df053a9ed45` |          1 |
| Unaffected | `claudeAgent` | `e2f152f9-bf22-45fa-9690-0bafa9df600b` | `7a78bf8a-7d50-427b-b708-eed15d6037bc` | `90fbecd3-b2ed-4570-b743-2a05455ed2bd` |          1 |

The exact affected Worker mapping resolved to:

- Container: `synara-agentd-30964f04-f294-4a6d-bbcb-41ff419832aa-0`.
- Container ID prefix: `65289224a293`.
- Worker index: `0`.
- Worker incarnation: `1`.
- Execution/Worker/container match: `true`.
- Network: runner-owned `synara-stage3-868169e2c36d`.
- Restore after interruption: `true`.

The affected Execution preserved its identity and fenced the obsolete Generation:

| Evidence       | Stale Generation                       | Replacement Generation                 |
| -------------- | -------------------------------------- | -------------------------------------- |
| Generation     | `1`                                    | `2`                                    |
| Worker ID      | `2c9ac0cd-c061-4f27-867e-4df053a9ed45` | `2c9ac0cd-c061-4f27-867e-4df053a9ed45` |
| Request ID     | `fixture-approval-generation-1-1`      | `fixture-approval-generation-2-1`      |
| Interaction ID | `613a297d-df1e-446a-9cb9-57bb338a4527` | `89ea30c7-0bbb-4e2c-81a8-27d0b4d503c4` |

The `execution.recovering` Event retained the stale Worker ID and Generation 1. The replacement `request.opened`
Event retained the same Execution ID and advanced exactly to Generation 2. Only the replacement Request was
resolved. The affected Turn then emitted one `execution.completed` terminal on Generation 2; the peer Turn emitted
one `execution.completed` terminal on its original Worker and Generation 1.

The peer Session's Events and pending Interaction were byte-for-byte unchanged after target recovery and again after
the affected terminal. The runner recorded `peerSessionEventsUnchanged=true`,
`peerInteractionIdentityUnchanged=true`, `peerWorkerAndGenerationUnchanged=true`, `terminalCount=2`,
`duplicateTerminal=false` and `pendingInteractionCount=0`.

## 4. Quota rejection and post-recovery slot reuse

Before failure injection, the other two Sessions each received `execution_quota_exceeded`; neither rejection changed
Session Events or pending interactions. After recovery, the same four Sessions were reused for the canonical load
waves without creating another Credential, Project, Target or resource owner.

Across `25` post-recovery waves, the runner observed:

- `100/100` unique completed Executions.
- `50/50` quota rejections with exact reason `execution_quota_exceeded`.
- `50/50` successful admissions after a preceding terminal released one slot.
- Zero Session Event or pending-interaction mutations from rejected admissions.
- `75/75` two-Execution overlap observations on distinct Workers.
- Codex and Claude Agent each completed `50` Executions.
- Each Session completed exactly `25` Executions.
- Zero pending interactions after the final wave.
- `doubleExecution=false` and `duplicateTerminal=false`.

## 5. Durable event, Artifact and Checkpoint evidence

The two failure-barrier Turns each completed one Ready Workspace Checkpoint and one Ready Checkpoint Artifact after
their approved terminal path. The affected Turn had exactly two `execution.leased`, `workspace.ready`,
`execution.started` and `request.opened` Events around one `execution.recovering`; the peer retained exactly one of
each and no recovery Event.

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

Cleanup used exact runner ownership `1573c46c971740fe8bbf` and removed both managed Worker containers, the named
Workspace volume, isolated network, Worker image and isolated Control Plane state. It recorded
`broadCleanupUsed=false`. Post-run Docker container, volume and network queries found no remaining acceptance
resources.

The output Secret scan covered `10` JSON, Markdown and redacted log files totaling `2,687,660` bytes. It checked five
known-secret canaries and private-key, AWS access-key, GitHub-token and OpenAI-style-key patterns; findings were empty.

The raw output directory was `.tmp/stage3-docker-fixture-load-failure-ab88798d/`. Its report hashes were:

| Report   | SHA-256                                                            |
| -------- | ------------------------------------------------------------------ |
| JSON     | `0228df0c4888443b7e69b66026ae378dd7b99a9e9b0263f9cd215da761ac7fd2` |
| Markdown | `61207bbf41e56c0a0d03dd39a26134552df7b52caee2d3266e966d9113411a90` |

## 7. Automated validation and DDL boundary

Before the clean-SHA canonical run, the same implementation passed:

- Acceptance Runner unit tests: `133/133`.
- All Stage 3 Python tests: `245/245`.
- Focused Go tests for `internal/quotas`, `internal/sessions`, `internal/executions` and `internal/agentd`.
- Dirty-worktree managed Docker `2`-wave failure-under-load gate: `10/10`.
- `bun fmt`.
- `bun lint`: `0` errors and `238` existing warnings.
- `bun typecheck`: `9/9` workspace tasks.
- Python compilation and `git diff --check`.

This slice changes no database DDL. The checked-in forward migration boundary remains
`000041_diff_artifact_kind.sql`.

## 8. Remaining completion boundary

This report closes only deterministic single-host managed Docker network failure targeting under two-Worker load,
peer Session isolation, one-step Generation fencing and post-recovery bounded admission/Artifact/Checkpoint
mechanics. Workflow L remains `partial`. Stage 3 still requires:

1. real Codex and Claude concurrent/load/failure evidence with controlled Credentials;
2. real SSH, Docker and Kubernetes consolidated Provider gates;
3. multi-host and Kubernetes multi-node scheduling, failure and load evidence;
4. process crash, container loss, Worker replacement and rollout failure injection under load;
5. production latency/throughput objectives and sustained production-duration load/soak evidence.
