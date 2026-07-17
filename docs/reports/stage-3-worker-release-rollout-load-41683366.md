# Stage 3 Managed Docker Worker Release Rollout Failure Under Load Gate

- Evidence date: `2026-07-17` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Clean implementation commit: `41683366145a3a9b67ab57ac2099b344f81aef89`
- Gate run: `stage3-worker-release-rollout-5291349a-e9fc-425c-950d-bc6eb3c5bb1d`
- Result: **PASS FOR THE DETERMINISTIC MANAGED DOCKER RELEASE-ROLLOUT FAILURE/LOAD SLICE; PRODUCTION AND REAL-PROVIDER GATES REMAIN OPEN**

## 1. Scope and evidence boundary

`docker_worker_release_rollout_gate.py` now combines the existing immutable Revision canary/promote/rollback path
with an exact candidate Worker container-loss recovery while a baseline/promoted Approval and candidate/canary
Approval overlap on two Workers. It then runs the same four reusable Sessions through `25` bounded load waves split
across candidate promotion and baseline rollback.

The gate uses the deterministic Provider Host fixture so it can create repeatable Approval barriers, quota
rejections, Text/Tool/Usage/Credential/Artifact/Checkpoint terminals and container loss without external Provider
Credentials. It exercises the real Control Plane, agentd, Docker reconciler, Worker Manifest, Release Revision,
Policy, Audit, Outbox and Session Event paths. It does not prove real Codex/Claude remote behavior, production
Registry auth/TLS/retention, multi-host or Kubernetes multi-node rollout, production KMS identity/tlog/admission,
production SLA, or production-duration load/soak.

## 2. Clean-SHA images and immutable Revisions

Both registry-pushed images were built from the same clean commit with different controlled versions and immutable
digests.

| Slot      | Worker version            | Registry digest                                                           | Release Revision                       | Worker Manifest                        |
| --------- | ------------------------- | ------------------------------------------------------------------------- | -------------------------------------- | -------------------------------------- |
| baseline  | `0.5.4+rollout.baseline`  | `sha256:ef66e1d26f69ac589f3eb15b5eeead0f75037a2b6c39bc3b14d13c0fdae5d72f` | `bb2fc1fa-465e-4372-a504-17774481f94d` | `e1a82929-2ab5-4fd4-abf5-3256319ec12b` |
| candidate | `0.5.4+rollout.candidate` | `sha256:62b7ba058230121f05adf25266738413844fd7245358ea8e31027ee81f86ef14` | `e6dace28-b393-4c0b-8473-5d6912297760` | `f0a20d52-21b3-49b8-8804-35528a3c7e75` |

Registering the baseline Manifest twice failed closed with HTTP `409`
`worker_release_manifest_already_registered`. The policy history remained exactly
`1 promote -> 2 canary -> 3 promote -> 4 rollback`; no synthetic policy transition was introduced for failure
injection or load.

## 3. Canary container loss under two-Worker overlap

The Tenant quota was fixed at `2`. Four Sessions were created, while the original primary Session retained the busy
baseline Approval. A second Session started candidate/canary work, producing two simultaneous pending Executions on
two distinct Workers. The other two Sessions each received `execution_quota_exceeded` without Event or Interaction
side effects.

The gate mapped the candidate Execution through `agent_executions.worker_id -> worker_instances.pod_name` to its
exact managed container and removed only that container. Recovery retained the same Execution, logical Worker,
candidate Revision, `canary` Channel, Manifest and Registry digest while replacing obsolete runtime identity:

| Evidence             | Before                                                                    | After          |
| -------------------- | ------------------------------------------------------------------------- | -------------- |
| Execution Generation | `1`                                                                       | `2`            |
| Container ID         | `9a5b84072fdc`                                                            | `a858262a7566` |
| Interaction/Request  | obsolete identities                                                       | new identities |
| Release Revision     | `e6dace28-b393-4c0b-8473-5d6912297760`                                    | unchanged      |
| Release Channel      | `canary`                                                                  | unchanged      |
| Registry digest      | `sha256:62b7ba058230121f05adf25266738413844fd7245358ea8e31027ee81f86ef14` | unchanged      |

The Worker ID stayed stable, Worker incarnation advanced, instance UID changed, and the named-volume sentinel was
preserved. The baseline peer retained the same Session Events, pending Interaction, Execution, Worker, Generation
and container. During the recovered pending window, both promote and rollback failed closed with
`worker_release_active_executions`. Candidate and baseline then each completed exactly once, leaving zero pending
Interactions.

## 4. Bounded load across promotion and rollback

`25` waves produced `100/100` unique load Executions on the same four Sessions. Each Execution was checked at both
active and terminal time for exact Revision, Channel, Manifest, Worker and Generation identity.

| Phase                          | Waves | Executions | Quota rejection/retry | Overlap observations | Active pins | Terminal pins | Worker bindings |
| ------------------------------ | ----: | ---------: | --------------------: | -------------------: | ----------: | ------------: | --------------: |
| candidate / `promoted`         |    13 |         52 |               26 / 26 |                   39 |          52 |            52 |              52 |
| baseline rollback / `promoted` |    12 |         48 |               24 / 24 |                   36 |          48 |            48 |              48 |
| Total                          |    25 |        100 |               50 / 50 |                   75 |         100 |           100 |             100 |

Codex and Claude Agent fixtures each completed `50` load Executions. Every Session completed `25`, exactly two
logical Workers were used, retired release pools claimed no new Execution, all `50` rejected admissions were
side-effect free, and every freed slot admitted the intended retry.

Including the baseline/candidate failure barriers, the four Sessions retained `102` distinct Executions and `102`
single terminal events. Their Event sequences were individually contiguous: `1..401`, `1..401`, `1..412` and
`1..417`. There was no double Execution, duplicate terminal, pending Interaction, or Generation regression.

## 5. Durable release history under load

The first formal load run exposed that a fixed `audit-logs?limit=200` view could hide early release records. The gate
now follows the product audit cursor with duplicate Event/cursor detection and a bounded page count. The canonical
run read `229` Tenant audit entries over `2` pages, then found exactly:

- `2` immutable `worker_release.revision_created` entries;
- `4` ordered policy audit entries matching promote/canary/promote/rollback;
- `4` immutable Transition rows matching final Policy version `4`.

The same load exposed that the Outbox admin API's recent-`200` view could hide release messages. The API now accepts
a validated `topicPrefix` filter, and the gate requests only `worker.release.` messages. It found exactly `6`
published messages: two Revision creations and four policy transitions. This uses the authorized product API rather
than raising the limit or reading the private metadata database.

## 6. Cleanup, Secret scan and report integrity

Cleanup stopped the isolated Control Plane and removed exactly the two main-pool containers, one observer container,
Registry container, three named volumes, network, both owner-labeled Worker image slots and temporary state.
`broadCleanupUsed=false`; post-run owner-label queries returned zero containers, volumes, networks and images.

The output Secret scan covered `17` JSON, Markdown, text, YAML and redacted log files totaling `3,206,778` bytes. It
used five known-secret canaries and found zero private-key, AWS access-key, GitHub-token or OpenAI-style key
patterns.

The raw output directory is `.tmp/stage3-worker-release-rollout-load-41683366-formal/`. Report hashes are:

| Report   | SHA-256                                                            |
| -------- | ------------------------------------------------------------------ |
| JSON     | `f9a8331f3f7e33b59e3451d503d6ee07916ca002456a98641cb90ecc0a463eae` |
| Markdown | `7465dfc7193760362309bab91008bbea55fe70ebd66f260885839d17779e6223` |

## 7. Automated validation and DDL boundary

- Formal managed Docker rollout/load gate: `15/15`.
- Rollout gate unit tests: `23/23`.
- Acceptance Runner unit tests: `136/136`.
- All Stage 3 Python tests: `253/253`.
- Focused Go tests passed for `internal/quotas`, `internal/sessions`, `internal/executions`, `internal/agentd`,
  `internal/executiontargets`, `internal/workerreleases`, `internal/outbox` and `internal/httpapi`.
- `bun fmt` passed and changed no files.
- `bun lint` passed with `0` errors and `238` existing warnings.
- `bun typecheck` passed all `9/9` workspace tasks.
- Python compilation, `gofmt` and `git diff --check` passed.

The final Outbox/API hardening occurred after the full Bun pass and changed only Go/Python gate code; it was covered
by the final focused Go and `253/253` Python runs. This slice changes no database DDL. The checked-in forward
migration boundary remains `000041_diff_artifact_kind.sql`.

## 8. Remaining completion boundary

This report closes deterministic single-host managed Docker immutable release-rollout container-loss recovery under
bounded load, load-safe Audit/Outbox history retrieval, and release-pinned load before and after rollback. Workflows
J and L remain `partial`. Remaining release gates include:

1. real Codex/Claude product and controlled-failure reports on SSH, Docker and Kubernetes using operator-provided
   environment-variable Credential sources, including third-party API Base URLs where configured;
2. external SSH host and approved Kubernetes context execution, including pinned host identity and explicit
   disposable/non-disposable safety boundaries;
3. multi-host/Kubernetes multi-node canary, Drain/PDB/Eviction and rollback evidence;
4. production Registry auth/TLS/retention plus approved Vault or other KMS identity, transparency-log and admission
   evidence;
5. resource-shaped production concurrency with explicit latency/error-rate acceptance thresholds and
   production-duration load/soak.
