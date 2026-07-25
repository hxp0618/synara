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
- it reuses the existing Stage 2 bootstrap unless explicitly disabled;
- every disruption is bounded by explicit timeouts;
- node drain is simulated narrowly by cordoning a selected node and replacing
  only the targeted Synara control-plane Pod;
- node partition uses disposable Kind node containers by default, and on an
  explicitly allowed non-Kind context it may call operator-provided
  `SYNARA_K8S_NODE_PARTITION_START_HOOK` /
  `SYNARA_K8S_NODE_PARTITION_STOP_HOOK` commands as an external adapter point;
  `SYNARA_K8S_NODE_PARTITION_START_HOOK_TIMEOUT_SECONDS` and
  `SYNARA_K8S_NODE_PARTITION_STOP_HOOK_TIMEOUT_SECONDS` independently bound
  disruption and heal execution, while
  `SYNARA_K8S_NODE_PARTITION_HOOK_TIMEOUT_SECONDS` remains their backward-compatible
  shared fallback; and
- the runner refuses broader destructive actions when the selected node also
  hosts the single PostgreSQL or MinIO dependency Pod.

The hook evidence records the actual timeout applied to each phase. A failed or
timed-out start always invokes the stop hook with its independent heal budget;
the EXIT trap uses that same stop budget. Setting only the legacy shared timeout
preserves the original behavior.

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
PostgreSQL while holding the reconciler's matching advisory-lock key, submit
deletion of that exact holder Pod before releasing the guard, and prove that a
different Ready Pod owns the next active epoch with
`fencing_token = previous + 1`. A continuous readiness probe starts before the
guard is acquired and stops only after the successor epoch is active and its
holder Pod is Ready. Deleting an arbitrary Pod is not sufficient evidence for
this case because it may only remove a follower.

`control-plane-failover` must support optional operator-provided hook commands
before and after the Pod replacement so external leader or reconciliation checks
can be layered without changing the script.

`node-drain` and `node-partition` are bounded smoke scenarios. They are useful
for disposable pre-production rehearsal, not as evidence of a cloud provider's
node-controller timing, storage behavior, or managed load-balancer semantics.
The managed node-partition hooks are an external adapter point for cloud- or
platform-specific network isolation drills; this document does not claim that
those drills have been executed on a real cloud, and it does not claim real cloud or production validation.
Every configured top-level case is required by default: a safety skip makes the
overall report fail. An explicitly narrower environment may list intentional
skips in `SYNARA_K8S_RESILIENCE_ALLOW_SKIPPED_CASES`; the report records that
allowlist and still records the case as `skipped`.

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
- `context`, `namespace`
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
uncordons a drain target, and best-effort runs the managed node-partition stop
hook before deleting resources. Start-hook execution registers cleanup
authority before the command runs, so a start failure or interrupt still
attempts a stop/heal. Stop-hook failure must remain visible in case evidence
and fail the final report.

Optional failover hooks remain supported, but recorded hook evidence is
redacted-by-default: command text, stdout, and stderr are replaced with
redaction markers while digests and byte counts are retained for correlation.
Managed node-partition hooks use the same redaction rules. Their timeout
enforcement terminates the hook process group best-effort; operators should
still make the stop/heal command idempotent and safe to retry.

## Non-claims

This acceptance lane does not prove:

- managed cloud control-plane failover;
- multi-zone storage recovery;
- real production ingress, DNS, or load-balancer behavior;
- node-controller eviction timing outside the bounded Kind smoke window; or
- any production result on a shared cluster where the safety override was used.
