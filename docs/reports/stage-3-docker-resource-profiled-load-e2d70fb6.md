# Stage 3 Deterministic Docker Resource-Profiled Load Measurement Gate

- Evidence date: `2026-07-18` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Clean gate commit: `e2d70fb61f04a646a28be1a801d2e3d9af70b663`
- Gate run: `stage3-provider-acceptance-0024d075-4c94-4af8-a811-42774a9351a9`
- Result: **PASS FOR RESOURCE-PROFILED DETERMINISTIC LOAD MEASUREMENT; PRODUCTION SLA AND REAL PROVIDER SOAK REMAIN OPEN**

## Scope and evidence boundary

The shared `fixture-load` path now accepts a minimum measured load duration in addition to its minimum wave count.
It continues adding complete four-Session waves until both conditions are true, while an explicit maximum wave count
provides a fail-closed safety bound. If the duration cannot be reached before that bound, the case fails rather than
silently shortening the requested window.

The implementation reuses the existing Control Plane, Docker Target driver, two agentd Workers, Codex/Claude
deterministic Provider Host fixtures, quota/admission path, Artifact/Checkpoint flow, report writer, Secret scan and
exact cleanup. It does not add a second load orchestrator or bypass the product APIs.

The report now records:

- Tenant quota, Worker count, active slots per Worker, Docker CPU and memory limits;
- effective concurrency, admission attempts, expected quota rejection rate and successful retry count;
- completed Execution success rate and unexpected failure/error rate;
- observed throughput;
- nearest-rank P50/P95/P99 for complete wave latency and released-slot admission recovery latency.

These are measurements, not an approved production policy. The gate deliberately records that no operator-approved
SLA thresholds were enforced.

## Requested resource profile and duration

| Dimension                         | Value                           |
| --------------------------------- | ------------------------------- |
| Target                            | managed Docker                  |
| Worker containers                 | `2`                             |
| CPU per Worker                    | `1,000,000,000` NanoCPUs (`1`)  |
| Memory per Worker                 | `2,147,483,648` bytes (`2 GiB`) |
| Active Execution slots per Worker | `1`                             |
| Tenant concurrent Execution quota | `2`                             |
| Sessions                          | `4`                             |
| Providers                         | Codex `2`, Claude Agent `2`     |
| Minimum waves                     | `25`                            |
| Minimum measured load duration    | `300s`                          |
| Maximum wave safety bound         | `100`                           |
| Overall gate timeout              | `1200s`                         |

The source worktree was clean, and the Capability Catalog SHA-256 was
`742a7eef08fde2394438fb0a9ee008cf1d062576d3b884709c291ffc17e9bdeb`.

## Sustained load result

The load case continued beyond its minimum `25` waves until the duration target was met:

| Measurement                         |     Result |
| ----------------------------------- | ---------: |
| Measured load duration              | `304.201s` |
| Waves completed                     |       `56` |
| Executions completed                |      `224` |
| Codex / Claude Agent Executions     |  `112/112` |
| Executions per Session              |       `56` |
| Admission attempts                  |      `336` |
| Expected quota rejections           |      `112` |
| Successful released-slot admissions |      `112` |
| Two-Worker overlap observations     |      `168` |
| Effective concurrency               |        `2` |
| Execution success rate              |   `1.0000` |
| Unexpected failure count            |        `0` |
| Unexpected error rate               |   `0.0000` |
| Observed completed Executions/s     |    `0.736` |

All quota rejections used exact reason `execution_quota_exceeded`, mutated no Session Event or pending Interaction,
and were followed by successful admission after one slot was released. Every overlap observation contained two
distinct Execution IDs on two distinct Worker IDs. The final state had no pending Interaction, double Execution or
duplicate terminal.

## Latency observations

### Complete wave latency

| Samples |  Minimum |  Average |      P50 |      P95 |      P99 |  Maximum |
| ------: | -------: | -------: | -------: | -------: | -------: | -------: |
|    `56` | `4.730s` | `5.432s` | `5.304s` | `6.493s` | `6.826s` | `6.826s` |

### Released-slot admission recovery latency

| Samples |  Minimum |  Average |      P50 |      P95 |      P99 |  Maximum |
| ------: | -------: | -------: | -------: | -------: | -------: | -------: |
|   `112` | `1.029s` | `1.168s` | `1.095s` | `1.372s` | `1.386s` | `1.393s` |

These values prove stable collection and percentile semantics for this exact deterministic resource profile. They do
not establish production pass/fail thresholds.

## Durable completion evidence

Every admitted Execution completed with the required Text, Tool, Usage, Workspace and Checkpoint lifecycle. The load
case recorded:

- `224` each of `turn.created`, `execution.leased`, `workspace.ready`, `execution.started` and
  `execution.completed`;
- `224` each of Tool `item.started` / `item.completed` and `thread.token-usage.updated`;
- `224` each of `workspace.dirty`, `checkpoint.created` and `checkpoint.ready`;
- `448` `artifact.ready` Events;
- exactly one `request.opened` and `request.resolved` per Execution.

## Cleanup and Secret scan

All `9/9` report cases passed. Cleanup removed only the runner-owned two Worker containers, isolated network,
Workspace volume, Worker image and state directory. `broadCleanupUsed=false`, and post-run inspection found no
container or image for clean SHA `e2d70fb6`.

The output Secret scan covered `10` JSON, Markdown and redacted log files totaling `5,874,277` bytes. Five known
Secret canaries were registered, and the scan found zero private-key, AWS access-key, GitHub-token or OpenAI-style
key patterns.

## Report integrity and automated verification

The ignored raw directory is
`.tmp/stage3-provider-acceptance-results/20260718-e2d70fb6-resource-profiled-load/`.

| Report   | SHA-256                                                            |
| -------- | ------------------------------------------------------------------ |
| JSON     | `46b739cebc3a6dc9014099340c6ee88cf00e956dbf946e50c03b514886e6eab0` |
| Markdown | `ac51c343e6b67fadcccd493349ca8ad522715388abf90cb63dd8cb6e8f2e642f` |

Before the clean gate:

- Acceptance Runner unit tests passed `164/164`;
- all Stage 3 Python tests passed `298/298`;
- a dirty-worktree real Docker duration smoke requested two waves and `12s`, then correctly extended to three waves;
- `bun fmt`, `bun lint` and `bun typecheck` passed; lint reported no errors and only existing warnings;
- `git diff --check` and Python bytecode compilation passed.

This increment changes no database DDL. The forward migration boundary remains
`000041_diff_artifact_kind.sql`.

## Remaining production boundary

This report closes the reusable minimum-duration and resource-profiled measurement mechanics. Stage 3 still requires:

1. operator-approved production duration plus P95/P99, error-rate and recovery-time thresholds;
2. the same approved policy on the production resource profiles and production-duration environment;
3. real Codex/Claude load and soak using qualifying Provider profiles;
4. production Kubernetes multi-node scheduling, rollout and cloud failure behavior;
5. real external SSH, production Registry/KMS identity/tlog/admission and Retention evidence.
