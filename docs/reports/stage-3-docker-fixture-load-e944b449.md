# Stage 3 Deterministic Managed Docker Load/Admission Gate

- Evidence date: `2026-07-17` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Clean gate commit: `e944b449461f26d578c1219d430651451de98510`
- Gate run: `stage3-provider-acceptance-37d11c9c-8a00-4066-b5a6-4732fd00f116`
- Result: **PASS FOR DETERMINISTIC BOUNDED MANAGED DOCKER LOAD/ADMISSION MECHANICS; REAL PROVIDER,
  MULTI-NODE AND PRODUCTION LOAD REMAIN OPEN**

## 1. Scope and evidence boundary

The shared `acceptance_runner.py` added one `fixture-load` suite by generalizing the existing managed Docker
concurrency path. It reuses the same Target Driver, two agentd Workers, Codex/Claude Provider Host Protocol 2.1
fixtures, Credential delivery, Project/Session APIs, report format, Secret scanner and exact cleanup. It does not
create a second load orchestrator, Provider fixture, Worker lifecycle or resource ownership model.

The suite creates four bound Sessions split evenly across Codex and Claude Agent and sets the Tenant concurrent
Execution quota to two. Every wave runs four Approval Turns containing deterministic Text, Tool, Usage, generated
Artifact and Credential evidence. At three points per wave, exactly two pending Executions must occupy distinct
Workers. A third admission must fail with `execution_quota_exceeded` without changing Session Events or
interactions; after one terminal, the rejected Session must be admitted immediately while the other Approval stays
pending. Every admitted Turn must complete with exactly one terminal plus Ready Artifact and Checkpoint evidence.

This proves bounded deterministic admission control, slot release/reuse, two-Worker overlap, repeated Session reuse
and durable completion mechanics. It does not prove real Codex/Claude performance, multi-host or Kubernetes
multi-node scheduling, production latency/throughput SLA, failure behavior under load, sustained production load or
production-duration soak.

## 2. Canonical configuration

The canonical run used:

- Target: one real managed `docker` Execution Target.
- Workers: `2` managed agentd containers.
- Sessions: `4`, split `2` Codex and `2` Claude Agent.
- Tenant `maxConcurrentExecutions`: `2`.
- Waves: `25`.
- Planned completed Executions: `100`.
- Planned quota rejections and recovery admissions: `50` each.
- Planned simultaneous-overlap observations: `75`.
- Overall timeout: `900s`.
- Source worktree: clean.
- Capability Catalog SHA-256: `8d47c4a08cdce16f0420c911737f92bf6b28ba49c6310e5601b7bd434f671f70`.

The full report completed in `291,455 ms`, including a clean-revision Worker image build. The bounded load case
completed in `137,937 ms`. Its observed rate was `0.725` completed Executions/second; this is diagnostic evidence,
not a performance threshold or SLA. Wave duration averaged `5,517.16 ms` (`4,810..6,468 ms`), and recovery from a
released quota slot averaged `1,199.04 ms` (`1,038..1,337 ms`). All `9/9` report cases passed.

## 3. Session and Worker identities

| Provider      | Session                                | Executions |
| ------------- | -------------------------------------- | ---------: |
| `codex`       | `eb4f6ad8-4ea5-4571-90f9-f43b4cb27924` |         25 |
| `claudeAgent` | `98eb9e22-7d2f-4d91-9041-ccbe58c45f56` |         25 |
| `codex`       | `16ba02f5-2450-4a99-ba19-8ee8398ca7fe` |         25 |
| `claudeAgent` | `df5ac8fa-7b3c-46a2-b1c3-d3ee5b2d847c` |         25 |

The two active Worker identities were:

- `182663bf-8aea-4344-8d4e-de50d8e45b54`
- `65fabf1e-71bc-4989-a562-e6891704e507`

Every one of the `75` overlap observations contained two distinct Session IDs, Execution IDs and Worker IDs with
both Approval interactions still pending.

## 4. Quota rejection and slot reuse

Across `25` waves, the runner observed:

- `100/100` unique completed Executions.
- `50/50` quota rejections with exact reason `execution_quota_exceeded`.
- `50/50` successful admissions after a preceding terminal released one slot.
- Zero Session Event or pending-interaction mutations from rejected admissions.
- `75/75` two-Execution overlap observations on distinct Workers.
- Codex and Claude Agent each completed `50` Executions.
- Each Session completed exactly `25` Executions.
- Zero pending interactions after the final wave.
- `doubleExecution=false` and `duplicateTerminal=false`.

The Session order rotates every wave, so the same Session or Provider is not permanently assigned to the first,
rejected or recovery-admission position.

## 5. Durable event, Artifact and Checkpoint evidence

Every completed Execution had exactly one of each required lifecycle event and at least two Ready Artifact events:

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

The terminal Worker ID and Generation had to match the corresponding active Approval evidence. Each terminal also
retained only key-name/boolean Credential evidence: `credentialPayloadKeys=["acceptanceToken"]` and
`credentialVerified=true`.

## 6. Cleanup and Secret scan

Cleanup used exact runner ownership `03c5e5ea763e46429306` and removed both managed Worker containers, the named
Workspace volume, isolated network, Worker image and isolated Control Plane state. It recorded
`broadCleanupUsed=false`. Post-run Docker label queries found no remaining acceptance containers, volumes or
networks.

The output Secret scan covered `10` JSON, Markdown and redacted log files totaling `2,513,498` bytes. It checked
five known-secret canaries and private-key, AWS access-key, GitHub-token and OpenAI-style-key patterns; findings were
empty.

The raw output directory was `/tmp/synara-stage3-fixture-load-e944b449461f/`. Its report hashes were:

| Report   | SHA-256                                                            |
| -------- | ------------------------------------------------------------------ |
| JSON     | `42e80c28ff153349d6e9105ff4976a5e0674c534fe160d2444f5e7b775797ec4` |
| Markdown | `65c4b6208c783254f6e31537dd9b55b00d3a65160dbd3477992a5ebd28f6f2d3` |

## 7. Automated validation and DDL boundary

Before the clean-SHA canonical run, the same implementation passed:

- Acceptance Runner unit tests: `126/126`.
- All Stage 3 Python tests: `238/238`.
- Focused Go tests for `internal/quotas`, `internal/sessions`, `internal/executions` and `internal/agentd`.
- Dirty-worktree load gates with Codex and Claude Agent as the primary Provider.
- A full dirty-worktree `25`-wave / `100`-Execution load gate.
- The original managed Docker concurrency fixture regression: `9/9`.
- `bun fmt`.
- `bun lint`: `0` errors and `238` existing warnings.
- `bun typecheck`: `9/9` workspace tasks.
- Python compilation and `git diff --check`.

This slice changes no database DDL. The checked-in forward migration boundary remains
`000041_diff_artifact_kind.sql`.

## 8. Remaining completion boundary

This report closes only deterministic bounded managed Docker quota/admission, slot reuse, two-Worker overlap and
durable Artifact/Checkpoint terminal mechanics. Workflow L remains `partial`. Stage 3 still requires:

1. real Codex and Claude concurrent/load evidence with controlled Credentials;
2. real SSH, Docker and Kubernetes consolidated Provider gates;
3. multi-host and Kubernetes multi-node scheduling/load evidence;
4. crash, network, Worker replacement and rollout failure injection under load;
5. production latency/throughput objectives and sustained production-duration load/soak evidence.
