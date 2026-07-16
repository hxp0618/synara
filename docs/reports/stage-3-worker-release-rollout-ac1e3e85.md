# Stage 3 Managed Docker Worker Release Rollout Gate

- Evidence date: `2026-07-17` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Implementation commit: `a4d9e4ef11cdad6753d715dee60fed430985e362`
- Observation-race fixes: `53f253b3`, `ac1e3e85`
- Clean gate commit: `ac1e3e85cd58563b3a429ced08254b49fb6cadeb`
- Gate run: `stage3-worker-release-rollout-1576046a-24fe-4e4b-9e8c-645a50272728`
- Result: **PASS FOR THE DETERMINISTIC MANAGED DOCKER IMMUTABLE ROLLOUT SLICE; PRODUCTION AND FOUR-TARGET RELEASE GATES REMAIN OPEN**

## 1. Scope and evidence boundary

`docker_worker_release_rollout_gate.py` built and pushed two different Worker versions from the same clean Git SHA
to a loopback-only disposable Registry. It then used the product Control Plane API and managed Docker Target path to
prove immutable Revision registration, initial promotion, canary, strict-CAS conflicts, active-Execution fencing,
candidate promotion, baseline rollback, release-pinned scheduling, immutable history and exact cleanup.

The run used the deterministic Provider fixture only to create bounded Executions and an Approval barrier. It does
not replace production Registry TLS/authentication/retention, real keyless or KMS identity and transparency-log
evidence, real Codex/Claude remote Provider credentials, Kubernetes multi-node rollout, load or soak evidence.

## 2. Clean-SHA images and immutable Revisions

Both images embed the same clean Git SHA but have different versions, Registry digests, manifests and immutable
Release Revisions.

| Slot      | Worker version            | Registry digest                                                           | Release Revision ID                    |
| --------- | ------------------------- | ------------------------------------------------------------------------- | -------------------------------------- |
| baseline  | `0.5.4+rollout.baseline`  | `sha256:27c09dcbba4e8b82dc190696b4106def05706fab8805016246ce1b1d98f3ca5f` | `5fcfc384-7150-42ac-bf81-6ae30f29a0c9` |
| candidate | `0.5.4+rollout.candidate` | `sha256:1711f96ca40b99cb940dbf87c2ff8dc5e84b9d321947743210c438e75290cf2e` | `dedfcf59-e3ae-47c8-b26c-622fd46677a4` |

Registering the baseline Manifest a second time failed closed with HTTP `409`
`worker_release_manifest_already_registered`; the original immutable Revision identity was returned.

## 3. Policy transitions and concurrency fences

The Target's immutable Transition history and final Policy formed one contiguous strict-CAS sequence:

| Policy version | Action   | Promoted Revision | Canary Revision | Canary percent |
| -------------- | -------- | ----------------- | --------------- | -------------- |
| `1`            | promote  | baseline          | none            | `0`            |
| `2`            | canary   | baseline          | candidate       | `100`          |
| `3`            | promote  | candidate         | none            | `0`            |
| `4`            | rollback | baseline          | none            | `0`            |

- A write using `expectedPolicyVersion=1` after version `2` was active returned HTTP `409`
  `worker_release_policy_version_conflict`, with the current and expected versions preserved.
- While an Approval Execution was active on the candidate canary, both promote and rollback returned HTTP `409`
  `worker_release_active_executions`; neither operation rewrote the active Execution's release pin.
- After the Approval was resolved and the Execution reached its terminal event, candidate promotion succeeded.
- The subsequent rollback selected the older baseline Revision and did not reuse a mutable image tag.

## 4. Runtime release-pin evidence

The managed two-Worker pool converged through these exact states:

1. two baseline containers on `promoted`;
2. one baseline `promoted` container plus one candidate `canary` container;
3. two candidate containers on `promoted`;
4. two baseline containers on `promoted` after rollback.

For every phase, the running container Image Digest, Worker Manifest, Worker Revision/Channel and newly scheduled
Execution Revision/Channel agreed. Three Executions were created:

| Purpose             | Execution ID                           | Release pin            | Event sequence | Terminal count |
| ------------------- | -------------------------------------- | ---------------------- | -------------- | -------------- |
| active canary fence | `271bb034-6ba1-41b4-a417-648eae1a3ce9` | candidate / `canary`   | `2..12`        | `1`            |
| candidate promoted  | `a25548f3-2ae8-4ca4-b486-5a6dfb0f4ef8` | candidate / `promoted` | `13..19`       | `1`            |
| baseline rollback   | `74f4b17d-634a-42ca-9cc9-3f3f435a0feb` | baseline / `promoted`  | `20..26`       | `1`            |

No retired Worker claimed a new Execution. The complete Session Event stream was contiguous from Sequence `1` to
`26`, with `doubleExecution=false`, `duplicateTerminal=false` and exactly one terminal per Execution.

## 5. Durable history, Audit and Outbox

- Release Revision audit entries: `2`.
- Policy audit entries: `4`, in order: promote, canary, promote, rollback.
- Outbox messages: `6`, all with status `published`.
- Outbox topics cover the two Revision creations and the four Policy transitions.
- The final Policy version `4`, immutable Transition version `4` and promoted baseline Revision agree.

## 6. Cleanup and Secret scan

Cleanup used only exact runner-owned identities:

- removed all `3` managed Worker containers;
- removed the disposable Registry container;
- removed the main, observer and Registry volumes;
- removed the owned Docker network;
- removed both Worker image slots and the isolated state directory;
- `broadCleanupUsed=false`.

The output Secret scan covered `17` JSON, Markdown, text, YAML and redacted log files totaling `323,825` bytes. It
exercised five known-secret canaries and found zero private-key, AWS access-key, GitHub-token or OpenAI-style key
patterns.

The raw output directory was `/tmp/synara-stage3-worker-release-rollout-ac1e3e85/`. Its report hashes were:

| Report   | SHA-256                                                            |
| -------- | ------------------------------------------------------------------ |
| JSON     | `9b2216e6e9775e82fd20fdd836b39fab342bac52bd810b8f623b084482e12915` |
| Markdown | `16806d203ec0fa0c154ae62b3444a429494f94a891a3cd4d83a34f0202206543` |

## 7. Automated validation and DDL boundary

- Managed Docker rollout gate: `14/14`.
- All release-gate tests: `105/105`.
- Stage 3 Python tests: `208/208`.
- Go focused tests: `internal/workerreleases`, `internal/executiontargets` and `internal/httpapi` passed.
- Worker Release Web unit tests: `44/44`; browser tests: `3/3`.
- Workspace `bun fmt`: pass.
- Workspace `bun lint`: zero errors and `238` pre-existing warnings.
- Workspace `bun typecheck`: `9/9`.

The full Bun workspace pass was completed before the final two Python-only observation-race fixes. Those follow-up
diffs are confined to the rollout gate and its tests and are covered by the final `208/208` Stage 3 Python run.

This slice changes no database DDL. The checked-in forward migration boundary remains
`000041_diff_artifact_kind.sql`.

## 8. Remaining release boundary

Workflow J and L remain `partial`. Closing them still requires:

1. a production TLS Registry with authenticated pull, immutable retention and recorded Credential rotation;
2. real keyless or KMS signer identity, transparency-log and admission-policy evidence;
3. real Codex/Claude Docker, SSH and Kubernetes product/failure gates with controlled Provider Credentials;
4. Kubernetes multi-node canary, Drain/PDB/Eviction and rollback evidence;
5. Busy Worker long-running Executions, load, retention concurrency and long-session soak.
