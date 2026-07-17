# Stage 3 Managed Docker Worker Release Rollout Gate

- Evidence date: `2026-07-17` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Busy Worker coverage: `17c56fae`
- Lease timing fix: `be776f1a`
- Docker Target capacity fix and clean gate commit: `d3af9380672cc9a3869a085be92af2d1352f392c`
- Gate run: `stage3-worker-release-rollout-594b1022-0d11-4e34-88e5-8262e028eb30`
- Result: **PASS FOR THE DETERMINISTIC MANAGED DOCKER BUSY-WORKER ROLLOUT SLICE; PRODUCTION AND FOUR-TARGET RELEASE GATES REMAIN OPEN**

## 1. Scope and evidence boundary

`docker_worker_release_rollout_gate.py` built and pushed two different Worker versions from the same clean Git SHA
to a loopback-only disposable Registry. It then used the product Control Plane API and managed Docker Target path to
prove immutable Revision registration, initial promotion, Busy baseline Worker preservation, canary placement,
strict-CAS conflicts, active-Execution fencing, candidate promotion, baseline rollback, release-pinned scheduling,
immutable history and exact cleanup.

The deterministic Provider fixture created bounded Approval Executions so the gate could hold both promoted and
canary Workers across rollout transitions. This proves deterministic completion and fencing mechanics; it does not
replace production Registry TLS/authentication/retention, real keyless or KMS identity and transparency-log evidence,
real Codex/Claude remote Provider credentials, Kubernetes multi-node rollout, load or soak evidence.

## 2. Clean-SHA images and immutable Revisions

Both images embed the same clean Git SHA but have different versions, Registry digests, manifests and immutable
Release Revisions.

| Slot      | Worker version            | Registry digest                                                           | Release Revision ID                    |
| --------- | ------------------------- | ------------------------------------------------------------------------- | -------------------------------------- |
| baseline  | `0.5.4+rollout.baseline`  | `sha256:1ed58f4b9c27389d8cbe87155ebc2ffa41ed14224a7811951325a5da99f859eb` | `cfe5cc3c-b5ab-4243-a1a1-a7a0bd728cfd` |
| candidate | `0.5.4+rollout.candidate` | `sha256:012d931cecaef7bc39d084e61c280ae16b54fbf654730f41b32218e434ad573f` | `d69a4770-bb1a-4d5b-a557-43658185715b` |

Registering the baseline Manifest a second time failed closed with HTTP `409`
`worker_release_manifest_already_registered`; the original immutable Revision identity was returned.

## 3. Busy baseline Worker preservation

The baseline Approval Execution `719eee7b-bc13-4702-9516-f1ff3297c35e` acquired Generation `1` on promoted
baseline container `95b188cab700`. Starting the canary produced one candidate/canary slot while preserving that
exact busy container ID; the protected baseline Worker did not consume the canary slot and the Target remained
available.

While the baseline Execution was active, candidate promotion failed closed with HTTP `409`
`worker_release_active_executions`. Its details identified the baseline Revision and `promoted` channel. The
Execution remained on Generation `1`, its Approval resolved without superseding the Interaction, and it reached
exactly one `execution.completed` terminal at Sequence `12`. Only after that terminal did reconciliation replace
container `95b188cab700` with `39e80c66459b` and finish the requested pool transition.

Two defects were found and fixed before the clean-SHA pass:

- managed Local, SSH, Docker and Kubernetes agentd now derive renewal cadence from the authoritative Worker Lease
  TTL, approximately one third of the TTL, instead of allowing a fixed default to exceed a short Control Plane TTL;
- a running stale Docker container deferred by an active Lease counts toward the desired Target capacity, preventing
  a healthy Busy Worker from making the Target appear `offline` while replacement waits for the safe boundary.

## 4. Policy transitions and concurrency fences

The Target's immutable Transition history and final Policy formed one contiguous strict-CAS sequence:

| Policy version | Action   | Promoted Revision | Canary Revision | Canary percent |
| -------------- | -------- | ----------------- | --------------- | -------------- |
| `1`            | promote  | baseline          | none            | `0`            |
| `2`            | canary   | baseline          | candidate       | `100`          |
| `3`            | promote  | candidate         | none            | `0`            |
| `4`            | rollback | baseline          | none            | `0`            |

- A write using `expectedPolicyVersion=1` after version `2` was active returned HTTP `409`
  `worker_release_policy_version_conflict`, with the current and expected versions preserved.
- While the baseline promoted Approval Execution was active, candidate promotion returned HTTP `409`
  `worker_release_active_executions` for the baseline / `promoted` release pin.
- While a second Approval Execution was active on candidate / `canary`, both promote and rollback returned HTTP
  `409 worker_release_active_executions` without rewriting the active Execution pin.
- After each protected Execution reached its terminal event, the requested transition succeeded. Rollback selected
  the older immutable baseline Revision and did not reuse a mutable image tag.

## 5. Runtime release-pin evidence

The managed two-Worker pool converged through these exact states:

1. two baseline containers on `promoted`;
2. one preserved busy baseline `promoted` container plus one candidate `canary` container;
3. two candidate containers on `promoted`;
4. two baseline containers on `promoted` after rollback.

For every phase, the running container Image Digest, Worker Manifest, Worker Revision/Channel and newly scheduled
Execution Revision/Channel agreed. Four Executions were created:

| Purpose             | Execution ID                           | Release pin            | Generation | Event sequence | Terminal count |
| ------------------- | -------------------------------------- | ---------------------- | ---------- | -------------- | -------------- |
| busy baseline fence | `719eee7b-bc13-4702-9516-f1ff3297c35e` | baseline / `promoted`  | `1`        | `2..12`        | `1`            |
| active canary fence | `227afa62-7000-4106-9a39-bc359bfb9e06` | candidate / `canary`   | `1`        | `13..19`       | `1`            |
| candidate promoted  | `69d2a217-9eaf-4dff-898f-f79426cdd632` | candidate / `promoted` | `1`        | `20..26`       | `1`            |
| baseline rollback   | `1b450dca-8bc0-4fdd-b86f-ca11eecb6b8d` | baseline / `promoted`  | `1`        | `27..33`       | `1`            |

No retired Worker claimed a new Execution. The complete Session Event stream was contiguous from Sequence `1` to
`33`, with `doubleExecution=false`, `duplicateTerminal=false` and exactly one terminal per Execution.

## 6. Durable history, Audit and Outbox

- Release Revision audit entries: `2`.
- Policy audit entries: `4`, in order: promote, canary, promote, rollback.
- Outbox messages: `6`, all with status `published`.
- Outbox topics cover the two Revision creations and the four Policy transitions.
- The final Policy version `4`, immutable Transition version `4` and promoted baseline Revision agree.

## 7. Cleanup and Secret scan

Cleanup used only exact runner-owned identities:

- removed all `3` managed Worker containers;
- removed the disposable Registry container;
- removed the main, observer and Registry volumes;
- removed the owned Docker network;
- removed both Worker image slots and the isolated state directory;
- `broadCleanupUsed=false`.

The output Secret scan covered `17` JSON, Markdown, text, YAML and redacted log files totaling `396,520` bytes. It
exercised five known-secret canaries and found zero private-key, AWS access-key, GitHub-token or OpenAI-style key
patterns.

The raw output directory was `/tmp/synara-stage3-worker-release-rollout-d3af9380/`. Its report hashes were:

| Report   | SHA-256                                                            |
| -------- | ------------------------------------------------------------------ |
| JSON     | `6b296dcf00dfe4affe5e3114e65069c151bfb5c7f7067d71a50dc106b5c905d3` |
| Markdown | `4dc3ebea8879cb9407216571c21a2ab3d8e0c0232781a1eb008f538e85b5dd34` |

## 8. Automated validation and DDL boundary

- Managed Docker rollout gate: `15/15`.
- Rollout gate unit tests: `18/18`.
- All release-gate tests: `108/108`.
- Stage 3 Python tests: `211/211`.
- `services/control-plane`: `go test ./...` passed after the lease-timing fix.
- Workspace `bun fmt`: pass.
- Workspace `bun lint`: zero errors and `238` pre-existing warnings.
- Workspace `bun typecheck`: `9/9`.

The full Bun workspace pass completed before the final Go/Python-only Busy Worker hardening. The subsequent changes
were covered by the final Go and Python runs; the documentation slice is checked separately with the repository
formatter.

This slice changes no database DDL. The checked-in forward migration boundary remains
`000041_diff_artifact_kind.sql`.

## 9. Remaining release boundary

Workflow J and L remain `partial`. This report closes deterministic Busy Worker completion, lease-renewal and
rollout-fencing mechanics only. Closing the workflows still requires:

1. a production TLS Registry with authenticated pull, immutable retention and recorded Credential rotation;
2. real keyless or KMS signer identity, transparency-log and admission-policy evidence;
3. real Codex/Claude Docker, SSH and Kubernetes product/failure gates with controlled Provider Credentials;
4. Kubernetes multi-node canary, Drain/PDB/Eviction and rollback evidence;
5. Busy Worker load/failure injection, retention concurrency and long-session production soak.
