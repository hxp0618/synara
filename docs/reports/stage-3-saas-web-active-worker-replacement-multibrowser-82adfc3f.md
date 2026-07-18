# Stage 3 SaaS Web active Worker replacement and multi-browser acceptance (`82adfc3f` baseline)

Date: 2026-07-18

## Conclusion

The patched worktree based on clean commit `82adfc3f` passes the deterministic SaaS Web active mid-Turn Worker
replacement, durable Approval fencing and multi-browser convergence slice.

An isolated real Web/Server used the Go Control Plane, PostgreSQL, MinIO and one standalone Local agentd running the
deterministic Provider Host fixture. The same authoritative Session first produced a generated file and Ready
Checkpoint. A second Turn then entered `waiting-for-approval` on Generation `1`; the agentd process was terminated
with `SIGKILL` and restarted with the same run-owned Worker identity, Target, Workspace root and Git cache. The
Control Plane preserved the same Session, Turn, Execution and logical Workspace, expired and superseded the stale
Generation `1` Approval, claimed the Execution on Generation `2`, and exposed exactly one replacement Approval to
the already-open Browser. Resolving that replacement from the Browser produced one terminal Event and a new Ready
Checkpoint after verifying the file restored from the baseline Checkpoint.

Two simultaneous Browser pages then watched a third Approval Turn. Both rendered the same pending Interaction; an
approval submitted from the second page disappeared from both pages without reload. A model-switch convergence
probe also passed: a normal switch propagated to the passive page, and a simultaneous two-page switch produced one
`200` plus one `409 session_model_conflict`. The losing page refreshed authoritative Session state, both pages
converged on the winner, and a final switch restored `gpt-5.5` on both pages.

The run and adjacent code audit found two Web integration issues that the final worktree fixes with focused tests:

1. `MessagesTimeline` gave both LegendList bootstrap and the existing imperative transcript path ownership of the
   initial bottom stick. Dynamic footer/row measurement could exceed LegendList's development convergence guard and
   emit `LegendList bootstrap initial scroll aborted after exceeding convergence bounds.` The transcript now uses
   only the existing imperative bottom-stick path.
2. A successful or failed REST catch-up could leave or force `reconnecting` even while an SSE subscription remained
   active. The projection runtime now distinguishes an active stream from a reconnect placeholder and does not
   replace or downgrade a healthy stream during manual catch-up. The Browser's disclosure DOM retains the banner
   subtree while closed, so acceptance checks use the collapsed ancestor/visual state rather than raw text count.

This closes deterministic fixture evidence for active mid-Turn replacement, Approval Generation fencing,
cross-page pending Interaction convergence and Session model-switch CAS convergence. It does not prove a real
Codex/Claude adapter, Structured User Input, a remote Target, production load SLA, production KMS identity/tlog/
admission, or the remaining advanced SaaS operations.

## Environment

- Source baseline: clean `82adfc3f` on `codex/saas-tenancy-user`; the Web fixes above were exercised in the final
  worktree that publishes this report.
- Browser surface: Codex in-app browser against isolated Vite Web `http://localhost:55933`.
- Synara Server: isolated development Server on `58390` with inherited browser auth disabled.
- Control Plane: repository `single-node` profile on `127.0.0.1:59920`.
- PostgreSQL: run-owned container on `127.0.0.1:59452`, schema `41/41`.
- Artifact store: run-owned MinIO on `127.0.0.1:59120`.
- Worker: standalone Local agentd using the deterministic Provider Host fixture and one execution slot.
- Identity: synthetic Dev Bootstrap identity `stage3-active-loss@example.invalid`.
- Secrets: independent random run-owned values remained in process environment only. No real Provider, SSH,
  Kubernetes or production KMS credential was used or recorded.

## Browser flow

```text
local SaaS login
  -> create Project `stage3-active-loss`
  -> baseline Turn `[artifact]`
  -> observe generated file + Ready Checkpoint
  -> second Turn `[text] [tool] [usage] [workspace-verify] [approval]`
  -> observe Generation 1 Approval
  -> SIGKILL standalone agentd
  -> restart same Worker identity / Target / Workspace roots
  -> observe one replacement Approval on Generation 2 without Browser reload
  -> approve replacement from the original Browser
  -> observe one terminal result and new Ready Checkpoint
  -> open a second page on the same Session
  -> third Turn `[approval]`
  -> both pages render pending Approval
  -> approve from the second page
  -> both pages remove pending Approval and show the terminal Turn
  -> switch model on one page and observe passive-page convergence
  -> issue simultaneous model switches from both pages
  -> one succeeds, one fails strict CAS, both converge to the winner
  -> restore `gpt-5.5` and verify both pages
```

## Authoritative identities

```text
tenant_id=b137af63-1443-4c17-8098-4d28da9a2d67
project_id=e3396241-9df9-4bb9-921e-211d48c71bf2
session_id=054363bf-dc47-4ffe-9700-a725bcde7338
execution_target_id=87ff7f9b-1d77-5c5f-ab55-6b8a309ca16c
worker_id=3d181736-dbb4-42dc-b60a-1d31ed563995
remote_workspace_id=4fbbd1f9-d9cb-43fd-8d47-95c07a04ee7a
workspace_materialization_id=bbaa6fe3-ef8d-4885-bcc5-18b9c2b27a11
```

The restarted Worker re-registered as the same logical Worker with incarnation `2`. The active recovery Execution
remained bound to that Worker and advanced only from Generation `1` to Generation `2`.

## Active Worker loss and Interaction fencing

Baseline Execution:

```text
execution_id=5f1361a5-43a5-4186-9a97-8a6ec92736cb
turn_id=9ac6a730-d126-458e-a05d-b9bb2299f89b
status=completed
generation=1
checkpoint_id=79d4740d-03db-471a-846c-7f9cb0890c8c
```

Recovered active Execution:

```text
execution_id=430ace34-bcca-4fb7-bf88-6e3500db806b
turn_id=7430b36f-2226-4fff-b4f1-110efef2ddd8
status=completed
generation=2
checkpoint_id=dd91ff6b-6b5a-499f-9c52-6d56c4c228fd
```

Interaction rows ended as:

| Request ID                        | Generation | Status     | Resolution | Delivery       |
| --------------------------------- | ---------- | ---------- | ---------- | -------------- |
| `fixture-approval-generation-1-1` | `1`        | `expired`  | none       | `superseded`   |
| `fixture-approval-generation-2-1` | `2`        | `resolved` | `approved` | `acknowledged` |

The stale row retained the normalized error `The Worker lease expired before the execution lifecycle completed.`
It was never delivered to Generation `2`. The replacement row was delivered and acknowledged only by Worker
Generation `2`.

The recovery Event interval is contiguous:

```text
12 turn.created
13 execution.leased           generation=1
14 workspace.ready            generation=1
15 execution.started          generation=1
16 item.started               generation=1
17 item.completed             generation=1
18 thread.token-usage.updated generation=1
19 request.opened             generation=1 request=fixture-approval-generation-1-1
20 execution.recovering       generation=1 reason=lease_expired
21 execution.leased           generation=2
22 workspace.ready            generation=2 restoredCheckpointId=79d4740d-...
23 execution.started          generation=2
24 item.started               generation=2
25 item.completed             generation=2
26 thread.token-usage.updated generation=2
27 request.opened             generation=2 request=fixture-approval-generation-2-1
28 request.resolved           generation=2 request=fixture-approval-generation-2-1
29 content.delta              generation=2
30 workspace.dirty            generation=2
31 checkpoint.created         generation=2
32 artifact.ready             generation=2
33 checkpoint.ready           generation=2
34 execution.completed        generation=2
```

There is exactly one terminal Event for this Execution:

```text
execution.completed=1
execution.failed=0
execution.cancelled=0
execution.interrupted=0
```

The terminal output proves Workspace continuity without exposing a Worker path:

```json
{
  "text": "fixture codex turn 1 complete",
  "workspaceEvidence": {
    "artifactRelativePath": ".synara-stage3-acceptance/artifact.txt",
    "artifactContentVerified": true
  }
}
```

## Workspace and Artifact continuity

The logical Workspace ID and materialization ID stayed unchanged across both Generations. Generation `2`
`workspace.ready` referenced the baseline Ready Checkpoint as `restoredCheckpointId`.

PostgreSQL ended with:

- one Ready user generated file `artifact.txt`, `42` bytes, from the baseline Execution;
- one Ready baseline snapshot Artifact for Generation `1`;
- one Ready replacement snapshot Artifact for Generation `2`;
- two Ready snapshot Checkpoints on the same logical Workspace;
- `remote_workspaces.last_generation=2` and `current_checkpoint_id=dd91ff6b-...`.

## Multi-browser Interaction convergence

Two independent pages watched `session_id=054363bf-dc47-4ffe-9700-a725bcde7338`. A third Turn created:

```text
execution_id=de23a610-bd37-42cd-8d7b-f74fcda99cc8
turn_id=a2dbfe8a-9537-4049-bb24-95f65f0044e2
generation=1
request_id=fixture-approval-generation-1-1
```

Both pages showed exactly one `Approve once` action. The second page resolved the request; within the same live run
both pages reported zero Approval actions and rendered `Approval resolved`. The Interaction ended
`resolved/approved/acknowledged` on Generation `1`, and the Execution produced exactly one `execution.completed`.

## Multi-browser model CAS convergence

1. A normal `gpt-5.5 -> gpt-5.4` switch updated both active and passive pages.
2. Both pages then submitted different switches from the same expected `gpt-5.4` state.
3. Control Plane logs recorded one `POST /v1/sessions/{sessionID}/model-switch` with `200` and one with
   `409 session_model_conflict`.
4. The losing page showed `Failed to switch model`, re-read the authoritative Session and converged with the winning
   page on `gpt-5.2`; it did not overwrite the winner.
5. One final authoritative switch restored `gpt-5.5`, and both pages displayed `GPT-5.5 / Medium`.

## Browser and SSE health

- A fresh stable page after the transcript fix reported zero page Console `warning`/`error` entries.
- The initial LegendList convergence warning is covered by a real browser regression with non-empty initial rows and
  a changing bottom-content inset.
- SSE remained a long-lived `200 text/event-stream` response through the same-origin Web/Server proxy, with active
  per-page leases and exact event delivery.
- The projection runtime fix prevents manual REST catch-up from downgrading or replacing an already active stream;
  the final stable page no longer retains a false reconnect banner.

Browser-plugin telemetry attempts to `ab.chatgpt.com` are outside the Synara page Console and are not product
findings.

## Focused verification

```text
MessagesTimeline bootstrap browser regression                 -> 1/1 passed
ControlPlaneProjectionRuntime and Provider Credential Web tests -> 18/18 passed
Control Plane Credential Go package                            -> passed
```

The required single repository-wide verification pass completed:

```text
bun fmt       -> passed
bun lint      -> passed with the existing repository warning baseline and no errors
bun typecheck -> 9/9 packages passed
```

## DDL boundary

No migration or DDL changed during this slice. PostgreSQL reported `41/41`; the Stage 3 migration boundary remains
`services/control-plane/migrations/000041_diff_artifact_kind.sql`.

## Security and cleanup

- No Provider Credential value, password, Worker token, private key, presigned URL or production KMS identity is in
  the implementation, report or Browser evidence.
- No repository-external SSH authentication value was used, echoed, copied or saved.
- The Browser was finalized after evidence collection.
- The Web/Server, Control Plane, standalone agentd, PostgreSQL, MinIO and the run-owned temporary directory were
  removed after verification.

## Remaining release gates

1. Real Codex and Claude active mid-Turn replacement plus Approval/Input across Local/SSH/Docker/Kubernetes.
2. Structured User Input multi-browser convergence and the remaining advanced SaaS operations.
3. Passing real SSH aggregate and passing real Codex/Claude Docker/Kubernetes product profiles.
4. Production-duration load/soak with approved P95/P99/error/recovery thresholds.
5. Approved production KMS reference, signer identity, transparency-log and admission-policy evidence.
