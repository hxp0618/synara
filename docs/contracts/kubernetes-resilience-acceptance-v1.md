# Kubernetes resilience acceptance v1

This document defines the additive Kubernetes resilience acceptance assets under
`deploy/kubernetes`. They extend the existing disposable Stage 2 Kind gate; they
do not replace it and they do not claim real cloud or production validation.

## Scope

Entry points:

- `deploy/kubernetes/kind-resilience-acceptance.sh`
- `deploy/kubernetes/resilience-acceptance.sh`
- `deploy/kubernetes/validate-resilience-assets.sh`

The resilience lane is production-oriented in structure, but safe by default:

- it refuses non-Kind contexts unless `SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE=1`;
- baseline bootstrap accepts only a fresh `synara-*` `SYNARA_K8S_NAMESPACE`; a non-default namespace derives an
  isolated ClusterRole/Binding name unless `SYNARA_K8S_ACCEPTANCE_RBAC_NAME` is supplied, and all four RBAC references
  (role, binding, roleRef, ServiceAccount subject) change together; the namespace and both RBAC identities carry an
  exact run-owner label, existing identities are never overwritten, and cleanup deletes only an exact owner match;
- it reuses the existing Stage 2 bootstrap unless explicitly disabled;
- every disruption is bounded by explicit timeouts;
- node drain is simulated narrowly by cordoning a selected node and replacing
  only the targeted Synara control-plane Pod;
- node partition uses disposable Kind node containers by default, and on an
  explicitly allowed non-Kind context it may call operator-provided
  `SYNARA_K8S_NODE_PARTITION_START_HOOK` /
  `SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK` /
  `SYNARA_K8S_NODE_PARTITION_STOP_HOOK` commands as an external adapter point;
  `SYNARA_K8S_NODE_PARTITION_START_HOOK_TIMEOUT_SECONDS` and
  `SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK_TIMEOUT_SECONDS` and
  `SYNARA_K8S_NODE_PARTITION_STOP_HOOK_TIMEOUT_SECONDS` independently bound
  disruption, independent observation, and heal execution, while
  `SYNARA_K8S_NODE_PARTITION_HOOK_TIMEOUT_SECONDS` remains their backward-compatible
  shared fallback; and
- the runner refuses broader destructive actions when the selected node also
  hosts the single PostgreSQL or MinIO dependency Pod.

The controller evidence records the timeout applied to each phase. Every phase
gets a distinct, initially absent artifact path through
`SYNARA_MANAGED_HOOK_ARTIFACT`. The controller opens the resulting regular file
with `O_NOFOLLOW`, bounds one stable snapshot, rejects duplicate/non-standard
JSON, and validates the exact operation, scope, phase, transition, freshness,
and passed evidence-digest checks. Persistent reports retain only controller
status and digests; operation ID, challenge, scope text, check names, command,
stdout, stderr, and process identifiers stay redacted. Missing, malformed,
stale, replayed, pending, or mismatched evidence fails closed and runs the stop
phase. A failed or timed-out start also invokes stop with its independent heal
budget; the EXIT trap uses that same stop budget. Setting only the legacy shared
timeout preserves the timeout fallback, but does not remove the independent
verify phase.

The checked-in multi-node Kind topology reserves one labeled Worker for both
acceptance dependencies before their PVCs bind. This guarantees that at least
one of the two host-spread Control Plane Pods is on a dependency-free node and
that node drain/partition are exercised rather than skipped because of random
scheduler placement.

## Required checks

The default case set is:

1. `rbac`
2. `topology`
3. `leader-takeover`
4. `control-plane-failover`
5. `node-drain`
6. `node-partition`

`rbac` must verify the current least-privilege surface required by the shipped
implementation:

- `authentication.k8s.io` `tokenreviews`: `create`
- `namespaces`: `get`, `create`, `patch`
- `pods`: `get`, `list`, `create`, `patch`, `delete`
- `serviceaccounts`, `secrets`, `resourcequotas`: `get`, `create`, `patch`
- `networkpolicies.networking.k8s.io`: `get`, `create`, `patch`

It must also assert that destructive cluster-wide permissions such as Namespace
deletion are not granted.

`topology` must verify the multi-node Kind shape, control-plane host spread, and
the presence of the shipped PodDisruptionBudget.

`leader-takeover` must read the active
`synara:kubernetes-execution-reconciler` holder and fencing token from
PostgreSQL while holding the reconciler's matching advisory-lock key. The guard
must select exactly one non-terminating Running and Ready PostgreSQL Pod and bind
one exact-Pod `kubectl exec -i` connection and one `psql` session to its immutable
Pod UID. That session must acquire the lock, emit exactly one nonce-bound lease
record followed by exactly one Ready record, and then wait for an explicit
nonce-bound unlock command. Elapsed time, local process liveness, buffered
output, or a separate PostgreSQL polling session is not proof that the lock
remains held.

The holder Pod deletion must use `DeleteOptions.preconditions.uid` with the
immutable UID resolved for the active lease holder. Every retry must reuse that
UID and is permitted only after bounded read-after-write reconciliation proves
that the same UID remains active while the guard is still in its pre-unlock
state. An ambiguous write is accepted only when the original UID is absent,
terminating, or replaced by a different UID. The harness waits for disappearance
of the original UID, never merely for disappearance of a reusable Pod name.

The unlock command may be sent only after the UID-preconditioned deletion was
successfully submitted or its ambiguous result was accepted by read-after-write
reconciliation. Successful guard evidence requires the same `psql` session to
emit one nonce-bound `UNLOCKED|true` record and exit zero. EOF, process exit,
writer failure, deadline expiry, malformed, duplicate, unknown, or out-of-order
guard output, or PostgreSQL Pod name, UID, phase, termination, or Ready drift
fails the case. Cleanup and the independent watchdog only terminate the writer
controller and exact exec stream; advisory-lock release caused by connection
cleanup is not a successful handshake. The case must finally prove that a
different Ready Pod owns exactly the next epoch with
`fencing_token = previous + 1`. A continuous readiness probe starts before the
guard is acquired and stops only after that successor is active and Ready.
Deleting an arbitrary Pod is not sufficient evidence because it may only remove
a follower.

The harness binds every local guard child to its creation identity and rejects
zombie/dead state or identity drift. It repeats the complete stream, controller,
and exact PostgreSQL Pod check immediately before every UID-preconditioned
Delete request, including ambiguous-write retries. This is a narrow liveness
boundary, not an atomic check-and-delete claim; the immutable Kubernetes UID
precondition remains the authority at the API write.
Cleanup examines every owned child even after one identity failure: live exact
children are terminated, terminal direct children are only reaped, absent
children are cleared, and identity drift is reported without signalling the
observed PID.

`control-plane-failover` must support optional operator-provided hook commands
before and after the Pod replacement so external leader or reconciliation checks
can be layered without changing the script.

`node-drain` and `node-partition` are bounded smoke scenarios. They are useful
for disposable pre-production rehearsal, not as evidence of a cloud provider's
node-controller timing, storage behavior, or managed load-balancer semantics.
The managed node-partition hooks are a trusted external adapter point for cloud-
or platform-specific network isolation drills. The runner generates one
operation ID and challenge for the case, and each start, verify, and stop phase
must create a fresh `synara.managed-hook-transition.v1` artifact bound to that
operation, phase, exact context/namespace, immutable Node/Pod UIDs, and
challenge supplied as `SYNARA_MANAGED_HOOK_CHALLENGE`. The required transitions
are respectively `terminal-applied`, `applied`, and `terminal-healed`; exit zero
without the exact fresh artifact is
not acceptance evidence. The repository cannot independently establish whether
a dishonest adapter fabricated its provider observation.
This document does not claim that those drills have been executed on a real
cloud, and it does not claim real cloud or production validation.
Every configured top-level case is required by default: a safety skip makes the
overall report fail. An explicitly narrower environment may list intentional
skips in `SYNARA_K8S_RESILIENCE_ALLOW_SKIPPED_CASES`; the report records that
allowlist and still records the case as `skipped`.

## Execution Pod failure evidence

Repository and environment acceptance for managed Execution Pods must distinguish the following bounded classes:

- API apply failure before a Pod UID exists;
- Pending timeout for the current Pod UID, measured from Kubernetes `metadata.creationTimestamp` rather than browser
  presence or an older replacement Pod;
- `PodScheduled=False/Unschedulable`;
- `ErrImagePull`, `ImagePullBackOff`, invalid image, and registry-unavailable states;
- bounded container-start waiting reasons;
- Pod `reason=Evicted`;
- current or last container termination `reason=OOMKilled`; and
- a generic failed phase without copying the raw status message into a metric label.

The durable authority is Migration `000078`: one Generation provisioning timeline plus one immutable first proof per
Generation/failure class whose last-observed timestamp may only advance. Reconcile polling must not manufacture event
counts, and a later healthy Pod must not overwrite earlier failure classes. A registered Worker with a terminal Failed
Pod must be terminalized before exact-UID deletion so recovery does not wait only for Lease expiry.

A local API status-subresource patch is acceptable E3 parser/persistence evidence for `Evicted`; it is not evidence of
real node-pressure eviction. Closing the production gate requires an actual kubelet/node-controller eviction and OOM
under the target cluster's runtime, in addition to API apply, scheduling, image-pull, and Pending scenarios.

## Evidence schema

The runner keeps the existing final report shape and adds two sidecars beside the
configured evidence path:

- `<evidence>.journal.jsonl`: append-only progress records for baseline
  completion, each top-level case completion, each soak-cycle completion, and
  final report completion. Journal entries are intentionally narrow: status,
  timestamps, case or cycle identity, and a bounded details summary plus digest.
  They never record hook environment values.
- `<evidence>.partial.json`: the current running snapshot. It is rewritten
  atomically in the same directory by writing a temporary file and renaming it
  into place. Interrupt cleanup must not delete it.

The final `<evidence>` JSON document remains the authoritative acceptance
report, with:

- `schemaVersion`: always `synara.kubernetes.resilience.acceptance.v1`
- `status`: `passed`, `failed`, or `dry-run`
- `startedAt`, `finishedAt`, `durationSeconds`
- `context`, `namespace`, `rbacName`
- `evidenceFile`
- `safety`: static guardrail metadata
- `baseline`: whether Stage 2 bootstrap ran and its status/duration
- `plannedCases`: the configured top-level case order
- `caseCounts`: passed/failed/skipped counts for recorded cases
- `permissions`: machine-readable `kubectl auth can-i` results
- `topology`: node counts, control-plane placement, and PodDisruptionBudget data
- `scenarios`: per-case records with `name`, `status`, `startedAt`,
  `finishedAt`, and case-specific `details`
- `soak`: soak configuration and per-cycle results when enabled

## Soak behavior

`SYNARA_K8S_RESILIENCE_SOAK_SECONDS` enables a bounded soak loop. The default is
`0`, which disables it. When enabled, the operator must set
`SYNARA_K8S_RESILIENCE_EVIDENCE_FILE` explicitly; soak evidence may not default
to an anonymous temporary path. The runner executes the configured
`SYNARA_K8S_RESILIENCE_SOAK_CASES` in order, records each cycle, and probes
readiness between cycles. Evidence records both the configured minimum window
and actual wall-clock duration; a disruption already started near the deadline
is allowed to finish, so actual duration can be longer than requested. The last
idle probe is capped to the remaining window. Long-running use is explicit and
finite; unbounded or cluster-wide destructive soak is out of scope.

Progress sidecars are updated at least when baseline bootstrap finishes, each
top-level case finishes, each soak cycle finishes, and the final pass/fail
report is emitted.

Interrupt cleanup kills any active probe, reconnects a partitioned Kind node,
uncordons a drain target, and runs the managed node-partition stop hook before
deleting resources. The harness marks cleanup active before start can execute
and launches `managed-hook-controller.py` as its own background child, recording
that child's creation identity. A failed, interrupted, or malformed controller
run first invokes recovery for the deterministic operation/phase unit, then
runs stop/heal, and the case remains failed even when recovery and heal succeed.
Stop-hook or recovery failure remains visible and fails the final report.
The direct child is a same-PID launcher: it publishes its own creation identity
to a private ready file and cannot exec the controller until the parent validates
that PID/identity and releases it. Identity failure expires without a numeric-PID
signal and therefore cannot execute the hook.

Local namespace/RBAC isolation, schema-81 two-replica bootstrap, exact Leader takeover, Control Plane failover, and a
120-second six-cycle bounded soak are recorded in
[`stage-4-orbstack-isolated-resilience-20260726-final2.md`](../reports/stage-4-orbstack-isolated-resilience-20260726-final2.md).
This is E3 single-node evidence and does not replace managed multi-AZ or production-duration E4 acceptance.

Optional failover hooks remain supported, but recorded hook evidence is
redacted-by-default: command text, stdout, and stderr are replaced with
redaction markers while digests and byte counts are retained for correlation.
Managed node-partition hooks return only the controller's sanitized JSON result;
hook command text, stdout, stderr, scope text, challenge, process identifiers,
and check names are not persisted. Real managed contexts require the
`systemd-user` backend with `securityBoundary=true`. It must reject unsupported
Linux/cgroup-v2/systemd-user environments with exit 125 before hook execution,
run the hook in an exact transient unit with `ExitType=cgroup`,
`KillMode=control-group`, and collection enabled, bind the unit InvocationID and
an opened cgroup device/inode, and require recursive `cgroup.events populated=0`
before success. Recovery stops, resets, and waits for collection of that exact
deterministic unit so the same operation/phase can safely retry.
The harness validates an exact result-key set and recomputes the operation,
scope, unit-name, phase, backend, boundary, transition, and digest bindings
before projecting a bounded summary. Every nonzero, interrupted, malformed, or
replayed execute result requires exact-unit recovery before heal, even when the
result claims termination was confirmed; the case remains failed.
The same managed-detail sanitizer runs before top-level and soak-cycle details
reach final evidence, partial snapshots, or journal payloads. Nonzero controller
results may not use successful status/reason semantics.
The runner selects that sanitizer from the trusted case name and non-Kind
context, never from hook-controlled detail fields, so pre-hook target failures
receive the same redaction, including `rawTarget`.

`managed-validation` may use `process-group-test` only with
`securityBoundary=false` and `testOnly=true`. That backend exists for repository
fake tests and explicitly is not production containment evidence; a new-session
child can escape it. Neither backend accepts an HMAC, hook-writable PID/PGID
authority, or inherited runner file descriptor. Operators should keep stop/heal
idempotent and safe to retry.

## Non-claims

This acceptance lane does not prove:

- managed cloud control-plane failover;
- multi-zone storage recovery;
- real production ingress, DNS, or load-balancer behavior;
- node-controller eviction timing outside the bounded Kind smoke window; or
- any production result on a shared cluster where the safety override was used.
