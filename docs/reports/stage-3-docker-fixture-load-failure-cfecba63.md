# Stage 3 Deterministic Managed Docker Network/Container/Provider-Process Failure-Under-Load Gate

- Evidence date: `2026-07-17` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Clean gate commit: `cfecba6327405e4b2856a9a3b9471166766f2908`
- Gate run: `stage3-provider-acceptance-4b77a513-5dd2-415b-b679-2460aeaa165b`
- Result: **PASS FOR DETERMINISTIC SINGLE-HOST MANAGED DOCKER NETWORK, CONTAINER-LOSS AND PROVIDER-HOST
  PROCESS-CRASH FAILURE UNDER LOAD; REAL PROVIDER, MULTI-HOST, MULTI-NODE, ROLLOUT AND PRODUCTION FAILURE/LOAD
  REMAIN OPEN**

## 1. Scope and evidence boundary

The shared `fixture-load-failure` suite reuses one managed Docker Target, two agentd Workers, four Codex/Claude
Sessions, Tenant quota, deterministic Provider Host Protocol 2.1 fixtures, Credential delivery, report format,
Secret scan and exact cleanup. It does not create another Target Driver, Provider fixture, Worker lifecycle, resource
owner or load orchestrator.

For each failure barrier, the runner resolves the selected active Execution through
`agent_executions.worker_id -> worker_instances.pod_name`, then requires the pod name to match exactly one running
managed container with the expected Target and `synara.io/worker-index` labels. The barriers then:

1. disconnect Worker index 0 from the runner-owned network;
2. remove Worker index 1's exact busy container and wait for the same logical Worker to be recreated; and
3. kill the single Protocol v2 Provider Host descendant inside the exact busy Worker container.

Network and container loss recover the same Execution through one fenced Generation advance. Provider Host process
crash instead fails the current Generation once as `provider_unavailable`, expires its pending Interaction, and uses
a distinct new Execution on the same logical Worker to prove automatic Host restart. For every barrier, the peer
Session must retain identical Events, pending Interaction, Worker and Generation.

This proves deterministic single-host targeting, managed replacement, Provider-process failure classification, peer
isolation, Generation fencing or new-Execution recovery, and post-failure quota/admission, Artifact and Checkpoint
mechanics. It does not prove real Provider behavior, multi-host or Kubernetes multi-node failure isolation, release
rollout failure, production latency/throughput SLA, sustained production load or production-duration soak.

## 2. Canonical configuration and result

The canonical run used:

- Target: one real managed `docker` Execution Target.
- Workers: `2` managed agentd containers.
- Sessions: `4`, split `2` Codex and `2` Claude Agent.
- Tenant `maxConcurrentExecutions`: `2`.
- Fault 1: `worker-network` against exact Worker index 0 for `8s`.
- Fault 2: `worker-container-loss` against exact Worker index 1.
- Fault 3: `provider-host-process-crash` against the exact busy Worker index 1 Provider Host descendant.
- Each fault: `2` active Sessions plus `2` side-effect-free quota rejections.
- Post-failure waves: `25`.
- Planned post-failure completed Executions: `100`.
- Planned post-failure quota rejections and recovery admissions: `50` each.
- Planned post-failure overlap observations: `75`.
- Overall timeout: `900s`.
- Source worktree: clean.
- Capability Catalog SHA-256: `8d47c4a08cdce16f0420c911737f92bf6b28ba49c6310e5601b7bd434f671f70`.

The full report completed in `386,197 ms`, including the clean-revision Worker image build. The network case completed
in `12,016 ms`, exact container loss/replacement in `8,618 ms`, Provider Host process crash and recovery admission in
`3,646 ms`, and post-failure load in `144,389 ms`. The observed post-failure rate was `0.693` completed
Executions/second, which is diagnostic evidence rather than a threshold or SLA. Wave duration averaged `5,775.52 ms`
(`4,885..6,941 ms`), and admission after a released slot averaged `1,201.7 ms` (`1,035..1,346 ms`). All `12/12`
report cases passed.

## 3. Exact network failure target and fencing

The network barrier held these two Approval interactions pending on distinct Workers:

| Role       | Provider      | Session                                | Execution                              | Worker                                 | Generation |
| ---------- | ------------- | -------------------------------------- | -------------------------------------- | -------------------------------------- | ---------: |
| Affected   | `codex`       | `cba44dbf-c6fb-46b0-8784-e787fe7cecfd` | `7fb6495f-3941-42ed-97a5-292de57c8637` | `20f3bb6a-192c-4ffd-80a3-de9b46c1f751` |          1 |
| Unaffected | `claudeAgent` | `44383f61-a42d-48eb-8776-b1d591852fb5` | `46eb896b-f559-49f9-adb3-92945c800803` | `3486a695-df5d-4218-898f-2cea1eea73cb` |          1 |

The affected Worker mapped exactly to container
`synara-agentd-305ac0af-128f-424e-945a-815af9619360-0` (`f2ea46209e50`), Worker index `0`, on runner-owned network
`synara-stage3-3eea624af9d5`. The network was restored after `8,195 ms`.

The Execution and Worker IDs remained stable while the Request changed from `fixture-approval-generation-1-1` to
`fixture-approval-generation-2-1`, the Interaction changed from `dafe8ddd-5852-42a4-a213-31fcabb7276a` to
`923fbc7f-0ed8-4d89-a77d-f2db76df478f`, and Generation advanced exactly from 1 to 2. The affected Turn completed once
on Generation 2; the peer completed once on its original Worker and Generation 1.

## 4. Exact container loss and managed replacement

The second barrier rotated the Session order and held these two Approval interactions pending:

| Role       | Provider      | Session                                | Execution                              | Worker                                 | Generation |
| ---------- | ------------- | -------------------------------------- | -------------------------------------- | -------------------------------------- | ---------: |
| Affected   | `claudeAgent` | `d3f58dda-c67e-48ac-8582-e3b99385787d` | `c551f433-703d-44c4-b034-6f4ec40cbab8` | `3486a695-df5d-4218-898f-2cea1eea73cb` |          1 |
| Unaffected | `codex`       | `d83bcea5-085f-4431-9dbf-c99486cdb8d2` | `22a19ddf-d557-471c-98dd-e87997504555` | `20f3bb6a-192c-4ffd-80a3-de9b46c1f751` |          1 |

The affected Worker mapped exactly to container
`synara-agentd-305ac0af-128f-424e-945a-815af9619360-1`, Worker index `1`. The runner wrote a sentinel into the
runner-owned named Workspace volume, removed only container `129b155a46db`, and observed replacement container
`af4eafd66969` under the same logical Worker name.

Managed replacement evidence was:

- `containerIdChanged=true`.
- Logical Worker ID remained `3486a695-df5d-4218-898f-2cea1eea73cb`.
- Worker incarnation advanced `1 -> 2`.
- Worker instance UID changed.
- Replacement Worker reached `online` before recovery continued.
- Named-volume sentinel `/data/.synara-stage3-provider-acceptance-volume` remained readable.
- Replacement completed in `1,132 ms`.

The affected Execution retained its ID and Worker ID while its Request changed from
`fixture-approval-generation-1-1` to `fixture-approval-generation-2-1`, its Interaction changed from
`9311e175-54ee-43ce-9f09-5d64f7482e3f` to `378d7680-2bdd-41d9-874e-51943bcd64b0`, and Generation advanced exactly
from 1 to 2. The affected Turn completed once on Generation 2; the peer completed once on its original Worker and
Generation 1.

## 5. Exact Provider Host process crash and terminal semantics

The third barrier retained a Codex peer pending while targeting this Claude Agent Execution:

| Role       | Provider      | Session                                | Execution                              | Worker                                 | Generation |
| ---------- | ------------- | -------------------------------------- | -------------------------------------- | -------------------------------------- | ---------: |
| Affected   | `claudeAgent` | `44383f61-a42d-48eb-8776-b1d591852fb5` | `841b7c15-141b-4e52-b474-a5d685752753` | `3486a695-df5d-4218-898f-2cea1eea73cb` |          1 |
| Unaffected | `codex`       | `d83bcea5-085f-4431-9dbf-c99486cdb8d2` | `12d3be1b-269b-4957-9c54-8ce2c477eacf` | `20f3bb6a-192c-4ffd-80a3-de9b46c1f751` |          1 |

The affected Execution mapped exactly to replacement container `af4eafd66969`,
`synara-agentd-305ac0af-128f-424e-945a-815af9619360-1`, Worker index `1`, incarnation `2`. A `/proc` descendant scan
rooted at agentd PID 1 found exactly one `--protocol-v2` Provider Host, PID `94`, and sent only that process
`SIGKILL`. Evidence recorded `scopedToManagedContainer=true`, `scopedToAgentdDescendants=true`,
`broadProcessMatchUsed=false` and `exactExecutionWorkerMatch=true`.

The original defect was that agentd correctly classified the killed Host but Control Plane rejected its failure
report with `execution_interaction_pending` while the Execution was waiting for Approval. The fixed failure path now
atomically expires/supersedes that Generation's pending Interaction, releases the lease and permits
`waiting-for-approval -> failed`.

The affected Execution emitted exactly one `execution.failed` at sequence `18`, on its original Worker and Generation
1, with `failureCode=provider_unavailable`. It emitted no `execution.recovering`. The failed Session had no remaining
pending Interaction. A new Execution `3fd79a89-8226-4d7d-8d9f-36117c8aa739` was admitted on the same logical Worker
after `2,335 ms`, proving Provider Host restart without reusing or resurrecting the failed Execution. The retry and
peer each completed once, while the peer retained its original Event and Interaction identity, Worker and Generation.

Across all three barriers, the runner recorded `peerSessionEventsUnchanged=true`,
`peerInteractionIdentityUnchanged=true`, `peerWorkerAndGenerationUnchanged=true`, `duplicateTerminal=false` and
`pendingInteractionCount=0`.

## 6. Post-failure quota, Artifact and Checkpoint evidence

Before each fault, the other two Sessions received `execution_quota_exceeded` with no Session Event or Interaction
mutation. After all three failures, the same four Sessions completed:

- `100/100` unique Executions.
- `50/50` quota rejections with exact reason `execution_quota_exceeded`.
- `50/50` successful admissions after one slot was released.
- `75/75` two-Execution overlap observations on distinct Workers.
- Codex and Claude Agent each completed `50` Executions.
- Each Session completed exactly `25` Executions.
- Zero pending interactions after the final wave.
- `doubleExecution=false` and `duplicateTerminal=false`.

Every post-failure load Execution had exactly one required lifecycle event and at least two Ready Artifact events:

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

## 7. Cleanup, Secret scan and report identity

Cleanup used exact runner ownership `d207ab5dfea84bf8b0b7` and removed both current managed Worker containers, the
named Workspace volume, isolated network, Worker image and isolated Control Plane state. It recorded
`broadCleanupUsed=false`. Post-run Docker container, volume and network queries found no remaining acceptance
resources.

The output Secret scan covered `10` JSON, Markdown and redacted log files totaling `2,810,588` bytes. It checked five
known-secret canaries and private-key, AWS access-key, GitHub-token and OpenAI-style-key patterns; findings were empty.

The raw output directory was `.tmp/stage3-docker-fixture-load-failure-cfecba63/`. Its report hashes were:

| Report   | SHA-256                                                            |
| -------- | ------------------------------------------------------------------ |
| JSON     | `64115d891f17f1bd398b97e4c7a5c3d1479b227d5aafed37e96f696418e074da` |
| Markdown | `4c484db83b44d6ef727956320ddc90365d7d69984c55a8b07839443c40ec8d05` |

## 8. Automated validation and DDL boundary

Before the clean-SHA canonical run, the same implementation passed:

- Acceptance Runner unit tests: `135/135`.
- All Stage 3 Python tests: `247/247`.
- Focused Go tests for `internal/quotas`, `internal/sessions`, `internal/executions`, `internal/agentd` and
  `internal/executiontargets`.
- Dirty-worktree managed Docker three-fault `2`-wave gate: `12/12`.
- `bun fmt`.
- `bun lint`: `0` errors and `238` existing warnings.
- `bun typecheck`: `9/9` workspace tasks.
- Python compilation and `git diff --check`.

This slice changes no database DDL. The checked-in forward migration boundary remains
`000041_diff_artifact_kind.sql`.

## 9. Remaining completion boundary

This report closes only deterministic single-host managed Docker exact network, container-loss and fixture Provider
Host process-crash targeting under two-Worker load, managed same-Worker replacement, peer Session isolation,
Generation fencing or new-Execution recovery, and post-failure bounded load mechanics. Workflow L remains `partial`.
Stage 3 still requires:

1. real Codex and Claude concurrent/load/failure evidence with controlled Credentials, including real Provider
   process failure under load;
2. real SSH, Docker and Kubernetes consolidated Provider gates;
3. multi-host and Kubernetes multi-node scheduling, failure and load evidence;
4. release rollout failure injection under load; and
5. production latency/throughput objectives and sustained production-duration load/soak evidence.
