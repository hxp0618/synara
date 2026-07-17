# Stage 3 Deterministic Local Retention/Cleanup Concurrency Gate

- Evidence date: `2026-07-17` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Clean gate commit: `c27914da24fd568e5cb0e3347139112d98316813`
- Gate run: `stage3-provider-acceptance-3f3b5b2e-21f5-43fa-9922-7182c0bd3318`
- Result: **PASS FOR DETERMINISTIC LOCAL ACTIVE-EXECUTION RETENTION FENCING AND POST-TERMINAL PHYSICAL
  CLEANUP; REAL PROVIDER, REMOTE TARGET AND PRODUCTION RETENTION REMAIN OPEN**

## 1. Scope and evidence boundary

The shared `acceptance_runner.py` added one `fixture-retention-concurrency` suite without creating another Target
Driver, Provider fixture framework, report format, Secret scanner, Control Plane lifecycle or cleanup path. The
suite runs only against an isolated Local Target backed by the real Control Plane, Local agentd and background
retention sweeper.

The run first creates a terminal `generated_file` Artifact and a current Ready Workspace Checkpoint. A second Turn
is then held at pending Approval while the runner enables the real Tenant retention policy and ages only its own
isolated SQLite rows. This makes the production sweeper exercise active-Execution fencing without changing the
production clock. The gate proves that eligible unreferenced Artifact cleanup can continue while Session archival
and physical Workspace cleanup remain fenced, then complete exactly once after the active Execution terminates.

It does not prove real Codex/Claude Adapter behavior, SSH/Docker/Kubernetes or multi-node behavior, sustained load,
production-duration retention, or a production data-volume policy.

## 2. Canonical configuration

The canonical run used:

- Target: isolated `local`.
- Provider: deterministic `codex` Provider Host Protocol 2.1 fixture.
- Barrier: `pending-approval-active-execution`.
- Tenant retention policy: archive Session, delete eligible Artifact and clean Workspace after `1` day.
- Runner-owned metadata age: `2` days.
- Retention sweep interval: `250ms`.
- Production clock changed: `false`.
- Overall timeout: `180s`.
- Source worktree: clean.
- Capability Catalog SHA-256: `8d47c4a08cdce16f0420c911737f92bf6b28ba49c6310e5601b7bd434f671f70`.

The full report completed in `8,547 ms`. The active sweep was observed in `1,531 ms`, and post-terminal physical
cleanup was observed in `891 ms`. All `9/9` report cases passed.

## 3. Durable identities

| Resource                         | ID                                     |
| -------------------------------- | -------------------------------------- |
| Session                          | `316bb70e-9fd1-4e77-a312-12948952d737` |
| Active Approval Execution        | `430cf265-c168-415e-ac87-ca7a0114de34` |
| Worker                           | `97182b61-e2fe-4ad4-adc2-d4dd4c5810a8` |
| Seed Ready Checkpoint            | `b4f16f7c-03ad-46a4-b7d9-74bd7d3bd7b3` |
| Post-terminal current Checkpoint | `28b245f4-9a01-4128-a953-f2fc4e7af07b` |
| Cleanup command                  | `0d046822-fb4d-499e-b0af-e06b6f43bc79` |

The active Approval Execution emitted exactly one `turn.created`, `execution.leased`, `workspace.ready`,
`execution.started` and `request.opened` before the retention observation point, with no terminal Event.

## 4. Active-Execution fencing

During the real retention sweep, the report required and observed:

- Session `status=active` and `archived=false`.
- Execution `status=waiting-for-approval` with `leaseCount=1`.
- Interaction `status=pending`.
- Workspace `state=ready`, `cleaned=false` and its physical generation still present.
- Materialization `state=active`, `activeExecutionCount=1` and `cleaned=false`.
- Zero Workspace cleanup commands.
- Seed/current Checkpoint and its Artifact remained `ready`, present and not deleted.
- The prior unreferenced generated Artifact was deleted and its payload removed.

Deleting the unreferenced Artifact while preserving the active Session, Workspace generation and Checkpoint lineage
proves that the sweeper did not achieve safety by globally pausing retention.

## 5. Post-terminal physical cleanup

After the Approval was resolved, the same Execution reached exactly one terminal `completed` state. The next
retention observation required and recorded:

- Session `status=archived` and `archived=true`.
- A new current Checkpoint remained `ready`, with its Artifact payload present.
- The seed Checkpoint and seed Artifact remained `ready` and were not accidentally deleted.
- Exactly one cleanup command was dispatched for generation `1` and acknowledged by agentd on delivery attempt `1`.
- Materialization transitioned to `state=cleaned` with `activeExecutionCount=0`.
- Workspace transitioned to `state=cleaned`, and the physical Workspace generation no longer existed.
- No physical Worker path was persisted in report evidence.

The report recorded `singleTerminal=true`, `sessionArchiveFencedWhileActive=true`,
`workspaceCleanupFencedWhileActive=true`, `seedCheckpointRetained=true` and
`postTerminalCurrentCheckpointReady=true`.

## 6. Cleanup and Secret scan

The Local driver stopped the isolated Control Plane and removed its runner-owned state directory. State preservation
was not requested. The output Secret scan covered `4` JSON, Markdown and redacted log files totaling `71,518`
bytes. It checked four known-secret canaries and private-key, AWS access-key, GitHub-token and OpenAI-style-key
patterns; findings were empty.

The raw output directory was `/tmp/synara-stage3-fixture-retention-concurrency-c27914da24fd/`. Its report hashes
were:

| Report   | SHA-256                                                            |
| -------- | ------------------------------------------------------------------ |
| JSON     | `82d29197d983fda73c4fcded57e24e969277b1eebf6c790532fa0f5b13161bc2` |
| Markdown | `e9e63231a6ca6fef7ab18aedf8011842272fa589222b8f77c77c5b65bc22ab34` |

## 7. Automated validation and DDL boundary

Before the clean-SHA canonical run, the same implementation passed:

- Acceptance Runner unit tests: `118/118`.
- All Stage 3 Python tests: `230/230`.
- Focused Go tests for `internal/retention`, `internal/executions`, `internal/artifacts` and `internal/agentd`.
- Dirty-worktree retention gates with the Codex and Claude Agent fixtures.
- The original Local fixture regression.
- `bun fmt`.
- `bun lint`: `0` errors and `238` existing warnings.
- `bun typecheck`: `9/9` workspace tasks.
- Python compilation and `git diff --check`.

This slice changes no database DDL. The checked-in forward migration boundary remains
`000041_diff_artifact_kind.sql`.

## 8. Remaining completion boundary

This report closes only deterministic Local active-Execution/Retention fencing, concurrent unreferenced Artifact
deletion, protected Checkpoint lineage and post-terminal physical Workspace cleanup mechanics. Workflows H and L
remain `partial`. Stage 3 still requires:

1. real Codex and Claude retention/concurrency evidence with controlled Credentials;
2. real SSH, Docker and Kubernetes Provider retention/cleanup gates;
3. multi-Worker and multi-node cleanup/failure-injection evidence;
4. sustained load and production data-volume behavior;
5. production-duration retention and soak evidence.
