# Stage 3 Deterministic Managed Docker Provider Concurrency Gate

- Evidence date: `2026-07-17` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Clean gate commit: `eeb7a2f1d249e46c2e259afc557eca96a0067542`
- Gate run: `stage3-provider-acceptance-c7c6c592-dc06-4605-9b1e-fe3803b081ac`
- Result: **PASS FOR DETERMINISTIC MANAGED DOCKER MULTI-PROVIDER/MULTI-SESSION OVERLAP MECHANICS;
  REAL PROVIDER AND PRODUCTION CONCURRENCY REMAIN OPEN**

## 1. Scope and evidence boundary

The shared `acceptance_runner.py` added one `fixture-concurrency` suite without creating another Target Driver,
Provider fixture framework, report format, Secret scanner or cleanup path. The suite provisions one real managed
Docker Execution Target with two agentd Workers, enables the deterministic Codex and Claude Provider Host Protocol
2.1 fixtures, creates one bound Session for each Provider and sets the Tenant concurrent Execution quota to two.

The clean-SHA gate proves that two Sessions can hold simultaneous pending Approval Executions on two distinct
Workers, that resolving one Provider does not mutate the other Session's pending interaction, and that each
Execution reaches exactly one terminal state without double execution. It does not prove real Codex/Claude Adapter
concurrency, SSH/Kubernetes or multi-node behavior, load, Artifact Retention/Cleanup concurrency, or
production-duration concurrency.

## 2. Canonical configuration

The canonical run used:

- Target: managed `docker` with `desiredWorkers=2`.
- Primary Provider descriptor: `codex`; the suite automatically added `claudeAgent` as the secondary Provider.
- Tenant `maxConcurrentExecutions`: `2`.
- Barrier: `simultaneous-pending-approval`.
- Control Plane restart: disabled for this bounded overlap gate.
- Overall timeout: `900s`.
- Source worktree: clean.
- Capability Catalog SHA-256: `8d47c4a08cdce16f0420c911737f92bf6b28ba49c6310e5601b7bd434f671f70`.
- Worker Image ID: `sha256:80da97f34b766048df64259ed86a31a74f1fa147a4b91d93d4917d4a9dc3c42d`.

The full report completed in `146,393 ms`; the concurrency case completed in `3,107 ms`. All `9/9` report cases
passed.

## 3. Target and Worker discovery

The run created Target `5c3c3f34-6425-419c-855b-1357d63dee0f` and discovered Manifest
`b5f3a234-87cf-4d4d-8408-ffcc2c33196e` with:

- Worker status counts: `online=2`, `draining=0`, `offline=0`.
- Worker Protocol: exact version `2`.
- Runtime Event: exact version `2`.
- Codex fixture runtime: compatible and explicitly enabled.
- Claude Agent fixture runtime: compatible and explicitly enabled.

Both containers ran as `10001:10001`, with `2 GiB` memory, `1` CPU, the isolated
`synara-stage3-2e7a917acd01` network and the runner-owned `/data` Workspace volume. The gate requires exactly two
running managed containers before creating Session resources.

## 4. Concurrent Session and Execution identities

At the shared observation point, both Approval interactions were pending and neither Execution had emitted a
terminal event:

| Provider      | Session                                | Execution                              | Worker                                 |
| ------------- | -------------------------------------- | -------------------------------------- | -------------------------------------- |
| `codex`       | `c0cd758e-6dda-4848-94e1-f54381dbd61c` | `d65a3b71-d049-41df-84eb-89a6094d73b0` | `f595f646-e1fd-43c0-bbfd-35a39bdcb4db` |
| `claudeAgent` | `71b11e5d-529c-4514-b552-06a94bf72f15` | `6efcbdfd-bb12-473d-ae7a-4c34f1d55b6e` | `ed68867f-a840-4320-813f-02e31164a891` |

Each active Execution had exactly one of every required event:

```text
turn.created
execution.leased
workspace.ready
execution.started
request.opened
```

The evidence therefore recorded `distinctSessionCount=2`, `distinctExecutionCount=2`,
`distinctWorkerCount=2`, `simultaneousPendingApprovals=true`, `doubleExecution=false` and
`duplicateTerminal=false`.

## 5. Isolation and terminal integrity

The suite resolved the secondary Claude interaction first, then reread the primary Codex Session interaction
snapshot. Codex remained pending, proving that the secondary resolution did not remove or resolve the primary
barrier. The suite then resolved Codex.

For both Sessions, the runner required `request.resolved` followed by exactly one `execution.completed` for the
same Execution identity. More than one terminal Event is a hard failure. Both final per-Execution Sequence ranges
were `2..12`, and the report recorded `primaryRemainedPendingAfterSecondaryResolution=true`.

## 6. Cleanup and Secret scan

Cleanup used exact runner ownership `6c048e55abc34af38d1f` and removed both managed Worker containers, the named
Workspace volume, isolated network, Worker Image and isolated Control Plane state. It recorded
`broadCleanupUsed=false`. A post-run Docker label query found zero remaining containers, volumes, networks or
images for that owner.

The output Secret scan covered `10` JSON, Markdown and redacted log files totaling `88,040` bytes. It checked five
known-secret canaries and private-key, AWS access-key, GitHub-token and OpenAI-style-key patterns; findings were
empty.

The raw output directory was `/tmp/synara-stage3-fixture-concurrency-eeb7a2f1d249/`. Its report hashes were:

| Report   | SHA-256                                                            |
| -------- | ------------------------------------------------------------------ |
| JSON     | `7dabe737965e66024393fc4a4cdb35fd53c1bdeff3c6eb156f6cc626bae4a658` |
| Markdown | `d6e1c7f470eb329ac9c5163914898e133a4e4ac659be1fa1af41c1e2c337b53b` |

## 7. Automated validation and DDL boundary

Before the clean-SHA canonical run, the same implementation passed:

- Acceptance Runner unit tests: `113/113`.
- All Stage 3 Python tests: `225/225`.
- Dirty-worktree canonical concurrency runs with Codex and Claude Agent as the primary Provider.
- The original managed Docker single-Worker fixture regression: `16/16`.
- `bun fmt`.
- `bun lint`: `0` errors and `238` existing warnings.
- `bun typecheck`: `9/9` workspace tasks.
- Python compilation and `git diff --check`.

This slice changes no database DDL. The checked-in forward migration boundary remains
`000041_diff_artifact_kind.sql`.

## 8. Remaining completion boundary

This report closes only deterministic managed Docker multi-Provider/multi-Session overlap scheduling and Session
isolation mechanics. Workflow L remains `partial`. Stage 3 still requires:

1. real Codex and Claude concurrent Executions with controlled Credentials;
2. real SSH, Docker and Kubernetes consolidated Provider gates;
3. Artifact Retention/Cleanup concurrency while Executions are active;
4. multi-node and sustained load behavior;
5. production-duration concurrency and soak evidence.
