# Stage 3 SaaS Web compatible Worker restart acceptance (`0eeabbc1` baseline)

Date: 2026-07-18

## Conclusion

The patched worktree based on clean commit `0eeabbc1` passes the deterministic compatible-Worker Web projection
and between-Turn restart slice. A real isolated Web/Server used the Go Control Plane, PostgreSQL, MinIO and the
embedded Local agentd. The same authoritative Session completed a first Turn with streamed assistant text, a Tool
row, usage, one Ready generated file and a Ready Workspace Checkpoint. The Control Plane and embedded Worker were
then restarted, the Worker re-registered from incarnation `1` to `2`, and a second Turn restored the first
Checkpoint and verified the generated file before completing on the same Session route.

The run found and fixed two Web gaps:

1. Execution-scoped Runtime Events without a duplicate payload `turnId` were not associated with the Turn created
   for their `executionId`, so Tool, usage and Artifact activities could be filtered out of the transcript.
2. A temporary `unobserved` Provider capability projection could remain cached after a compatible Worker returned,
   leaving a stale Worker-wait banner until page reload.

The Web now keeps one immutable execution-to-Turn correlation in the Session projection and polls capability
projections every two seconds only while a Worker manifest is unobserved. Polling stops as soon as the projection is
observed. Focused tests cover both behaviors.

This closes only deterministic compatible-Worker live projection and restart between completed Turns. It does not
prove a real Codex/Claude adapter, active mid-Turn Worker replacement, Approval/Input recovery, multi-browser
concurrency, a remote Target, production load SLA or production KMS policy.

## Environment

- Source baseline: clean `0eeabbc1` on `codex/saas-tenancy-user`; the two Web fixes above were tested in the final
  worktree that publishes this report.
- Browser surface: Codex in-app browser against isolated Vite Web `http://localhost:55833`.
- Synara Server: isolated development Server on `58290` with browser auth token inheritance disabled.
- Control Plane: repository `single-node` profile on `127.0.0.1:59900`.
- Metadata and queue: PostgreSQL on `127.0.0.1:59432` plus `postgres-outbox`.
- Artifact store: private MinIO on `127.0.0.1:59100`.
- Worker: embedded Local agentd using the deterministic Provider Host fixture.
- Identity and Secrets: synthetic Dev Bootstrap identity and independent random run-owned values. No real Provider,
  SSH, Kubernetes or production KMS credential was used or recorded.

## Browser flow and checks

```text
local SaaS login
  -> create Project
  -> first Turn: [text] [tool] [usage] [artifact]
  -> observe streamed text, Tool, usage, generated file and Checkpoint
  -> restart Control Plane and embedded Worker
  -> keep the same Session route and reconnect the Session Event stream
  -> second Turn: [text] [tool] [usage] [workspace-verify]
  -> restore the first Checkpoint and verify the generated file
  -> open a fresh stable Browser tab and rehydrate both Turns
```

| Check                        | Result | Evidence                                                                                          |
| ---------------------------- | ------ | ------------------------------------------------------------------------------------------------- |
| Session identity             | pass   | same route `97f76b5f-3efa-4048-8280-6f7a72216767` before and after restart                        |
| Assistant projection         | pass   | two visible `deterministic fixture text` messages                                                 |
| Tool projection              | pass   | two visible `Deterministic fixture tool` rows after expanding both Turn details                   |
| Usage projection             | pass   | two visible `Token usage updated` rows                                                            |
| Artifact preservation        | pass   | one Ready `artifact.txt`, `Generated file · 42 B`, remains after restart and the second Turn      |
| Workspace recovery           | pass   | second Turn shows `Workspace restored`; the first generated file was verified from restored state |
| Capability reconciliation    | pass   | stable page has no visible compatible-Worker wait banner after the Worker returns                 |
| Session Event reconciliation | pass   | stable page has no visible reconnect banner after catch-up                                        |
| Console health               | pass   | fresh stable Browser tab reports zero page `error`/`warn` entries                                 |

The final screenshot is saved outside the repository at
`/tmp/synara-stage3-compatible-worker-final-0eeabbc1.png`. It shows the preserved Artifact and the expanded resumed
Turn with Worker assignment, Workspace restore and deterministic Tool output. Browser automation telemetry warnings
are not page Console entries and were not emitted by Synara.

## Worker restart and Workspace evidence

Before restart, the compatible Local Worker was online at incarnation `1`. After stopping the Control Plane and
starting the same binary with the same isolated environment, the persisted Worker row was:

```text
worker_id=0e2063cd-0554-4768-8ec9-443ff9be8946
status=online
compatibility_status=compatible
incarnation=2
instance_uid=c101d5c0-8ca0-48a1-8728-0cf2c13de57f
```

The same Workspace and materialization were retained. The active materialization was rebound to Worker incarnation
`2`, while the second `workspace.ready` Event recorded the first Turn's Checkpoint as
`restoredCheckpointId=f6a22591-826d-4732-87f3-2ff1413bf566`.

The second terminal output included only non-secret acceptance metadata:

```json
{
  "text": "fixture codex turn 1 complete",
  "workspaceEvidence": {
    "artifactRelativePath": ".synara-stage3-acceptance/artifact.txt",
    "artifactContentVerified": true
  }
}
```

The fixture's internal Turn counter restarted with the Provider Host process. Continuity is therefore proved by the
authoritative Session, Workspace, Checkpoint and verified file, not by a fixture Turn number.

## PostgreSQL evidence

The authoritative Session Event chain is continuous with no duplicate Sequence:

```text
session=97f76b5f-3efa-4048-8280-6f7a72216767
min_sequence=1
max_sequence=28
row_count=28
distinct_sequence_count=28
contiguous=true
```

The first Turn occupies Sequence `2` through `15`:

```text
2  turn.created
3  execution.leased
4  workspace.ready
5  execution.started
6  content.delta
7  item.started
8  item.completed
9  thread.token-usage.updated
10 artifact.ready
11 workspace.dirty
12 checkpoint.created
13 artifact.ready
14 checkpoint.ready
15 execution.completed
```

The resumed Turn occupies Sequence `16` through `28`:

```text
16 turn.created
17 execution.leased
18 workspace.ready
19 execution.started
20 item.started
21 item.completed
22 thread.token-usage.updated
23 content.delta
24 workspace.dirty
25 checkpoint.created
26 artifact.ready
27 checkpoint.ready
28 execution.completed
```

PostgreSQL ended with:

- one Ready `generated_file` named `artifact.txt`, `42` bytes;
- two internal Ready `workspace_snapshot` Artifacts;
- two Ready snapshot Checkpoints, each with `file_count=1` and `total_bytes=42`;
- one active Session with `last_event_sequence=28` and a usable Provider Cursor state;
- two completed Executions and no failure code.

## Verification

```text
apps/web focused Vitest -> 2 files, 44 tests passed
bun fmt                    -> passed, 1,924 files checked
bun lint                   -> passed with repository baseline warnings and zero errors
bun typecheck              -> passed, 9/9 packages
```

## DDL boundary

No migration or DDL changed. PostgreSQL and `/ready` both reported:

```text
control_plane_schema_migrations count|max(version) = 41|41
expectedVersion=41
appliedVersion=41
```

The Stage 3 migration boundary remains
`services/control-plane/migrations/000041_diff_artifact_kind.sql`.

## Security and cleanup

- The implementation and documentation contain no Credential value, password, token, private key, presigned URL or
  production signer identity.
- The externally supplied SSH credential was not used, echoed, persisted or copied into evidence; it must be rotated
  because it was disclosed in chat.
- Secret scanning covers the Git diff and isolated logs without printing the compared values.
- The isolated Browser tabs were finalized. The Web/Server and Control Plane processes, PostgreSQL and MinIO
  containers and volumes, and the run-owned temporary runtime directory were removed. Ports `55833`, `58290`,
  `59432`, `59100` and `59900` were no longer listening.

## Remaining release gates

1. Real Codex and Claude compatible-Worker output and native resume across Local/SSH/Docker/Kubernetes.
2. Active mid-Turn Worker loss, replacement, Generation fencing and Interaction recovery in the same Browser Session.
3. Multi-browser concurrency and cross-browser model/Interaction/Artifact reconciliation.
4. Passing real SSH aggregate and passing real Codex/Claude Docker/Kubernetes product profiles.
5. Production-duration load/soak with approved resource profile, P95/P99/error/recovery thresholds.
6. Approved production KMS reference, signer identity, transparency-log and admission policy evidence.
