# Stage 4 Capacity reservation authority — OrbStack E3 final1

Date: 2026-07-26  
Repository branch: `codex/saas-tenancy-user`  
HEAD observed at final verification: `a175e24c8978`  
Evidence level: **E3 local real-Kubernetes/PostgreSQL evidence**  
Kubernetes context: explicit `--context orbstack`  
Kubernetes server: `v1.34.8+orb1`

The shared branch advanced concurrently during the run, including commits made by another actor. This agent did not
stage, commit, merge, or push. The working tree remained dirty. This report covers only the strict Execution
capacity-reservation increment and does not reclassify unrelated Stage 4 evidence.

## Scope

The increment adds:

- generation-scoped `(executionId, generation)` acknowledgement identity;
- `exact-active-v1` Health authority with exact next-version rows, bounded count, and canonical set SHA-256;
- strict usage as allocated occupancy plus active unacknowledged `queued | recovering` reservations;
- `reservation-aware-v1` routing rank and hard eligibility without queued-Pod double count;
- Target-lock serialization across Health publication, new Execution admission, and active-state recovery;
- managed Kubernetes publication for exact existing/applied Pods and distinct ready-idle Warm Pool slots;
- separately scoped signed Platform publication through `publishReservations`;
- one immutable `execution_capacity_admissions` snapshot and SHA-256 per new Execution;
- Event/Outbox admission mode and digest projection; and
- bounded admission/acknowledged/unacknowledged/strict-used metrics without Tenant, Target, Execution, or Pod labels.

Fixed Targets retain legacy explicit-target behavior until exact authority is enabled. After enablement, omission is a
downgrade error and fixed launches also fail closed on stale, unhealthy, saturated, or fully reserved capacity.

## Source identity

| Source | SHA-256 |
| --- | --- |
| `000084_execution_capacity_reservation_authority.sql` | `7073977b4c4273c2a0d7614bf886d6db694604bba7dfaad43ad6382f866ad0f9` |
| `internal/routing/capacity_reservations.go` | `f3142982ce864feb55714b55fa00a8d3293deb3defefa778fd8786d4f73893af` |
| `internal/routing/service.go` | `6e72e9ed59c12e604284eb1550e3e9dce33dc9cd5a9bd773a5e812147ec33378` |
| `internal/executiontargets/kubernetes_reconciler.go` | `01db065231db5250978879228ea48e6939942aa07fe56bd4c0dd3d10fbbb0da5` |
| `internal/sessions/scheduled_execution.go` | `69863209d4be12a96e51881490ec3b7868c981077674a706278f558239c1e702` |
| `internal/database/capacity_reservation_sqlite.go` | `c2b3853867b545926ed84f65df3bc73f84dc40edb9690f996cd67802b4e904cf` |
| `internal/persistence/capacity_admission_models.go` | `cbf02ee28e6d6200d3a11e9693fd3be78b96c42adbe6b52ad178430d7c6ebe57` |
| `internal/executiontargets/kubernetes_reservation_authority_orbstack_integration_test.go` | `702afe9b2087c27f95f8111a2069b38518b878b1a83b9745e50b7ad79d7e2eec` |
| `internal/routing/commit_validation_postgres_integration_test.go` | `ef6a7515d7311408bdafae517fccae95d3b42d5ed2ab30fe228c392b923f1705` |
| `internal/observability/distributed_routing_billing_metrics.go` | `123f9f37b2940725aff5ddeadb794684cad2e02c3377be308d1873f74fe2beb4` |

## Package and SQLite results

Focused tests proved:

- an acknowledged queued Pod contributes only its published allocated unit, not a second queue unit;
- an unacknowledged new or recovering Generation contributes one strict reservation unit;
- a Generation-0 acknowledgement does not match the same Execution after it re-enters recovery as Generation 1;
- exact authority cannot be omitted after enablement;
- stale claimed/terminal identities are filtered before the next Health version commits;
- acknowledgement count cannot exceed same-observation allocated occupancy;
- SQLite accepts only next-Health-version staging, rejects in-place admission mutation, and cascades evidence with its
  parent Execution;
- signed Platform reservation publication requires its independent exact scope and preserves canonical ordering; and
- the reference signer includes reservation authority in the signed canonical Health object.

The ordinary fixed-Target idempotency test also proved exactly one immutable admission row, byte-stable SHA-256 after
reload, and identical admission mode/digest in the authoritative Event and Outbox message. A routed capability-gate test
proved `reservation-aware-v1` plus exact admission evidence after final Target/Pool commit validation.

## OrbStack PostgreSQL two-connection run

Owned namespace: `synara-capacity-reservation-20260726` (deleted after the run).  
Database: PostgreSQL 16 in an OrbStack Pod.  
Migration row: `84 | execution_capacity_reservation_authority | 7073977b4c42...`.

Command under test:

```text
SYNARA_TEST_DATABASE_URL=<ephemeral PostgreSQL URL> \
  go test ./internal/routing \
  -run '^TestCapacityAdmissionPostgresSerializesConcurrentReservations$' \
  -count=1 -v
```

Result: PASS. The first connection locked the Target, observed one free strict slot, and paused before commit. A second
connection attempted admission and remained blocked across the test's 150 ms negative window. The first connection then
inserted its queued Execution plus immutable admission evidence and committed. The second connection resumed, recounted
the first Execution as unacknowledged, and returned `execution_target_capacity_reserved`.

PostgreSQL additionally rejected:

- direct `execution_capacity_admissions` mutation; and
- an acknowledgement inserted into the current rather than next Health version.

The stored admission digest exactly matched a fresh Go canonical recomputation.

## OrbStack real Kubernetes Reconciler run

Owned namespace: `synara-capacity-worker-20260726` (deleted after the run). The acceptance test used the real OrbStack API,
an encrypted tenant-owned Kubernetes Target configuration, PostgreSQL metadata, two durable queued Executions, a
namespace-scoped ServiceAccount, real foundation apply, and real Pod apply.

The first attempt intentionally exposed an RBAC boundary: Kubernetes returned 403 because the built-in namespaced
`admin` ClusterRole cannot patch ResourceQuota. That attempt was not counted as success. The successful retry added a
namespace-scoped wildcard Role and RoleBinding; it did not grant a ClusterRoleBinding or cluster-scoped privilege.

Two successful reconcile passes produced:

```text
Health: exact-active-v1 | acknowledged=1 | allocated=1 | ceiling=1 | version=2
DB active reservations: 2
DB unacknowledged reservations: 1
current-version acknowledgement rows: 1
actual Target-labeled Pod objects: 1
```

The Reconciler also created the expected Target ResourceQuota, NetworkPolicy, ServiceAccount, Role, and RoleBinding. The
acceptance image was deliberately `busybox`, so the generated `/usr/local/bin/synara-agentd` command later terminated and
the final observed Pod phase was `Failed`. This run proves real API/RBAC/foundation/Pod-occupancy acknowledgement
mechanics, not Worker runtime readiness. A production image and long-running Worker success remain separate gates.

## Regression checks

- `go test ./... -count=1` in `services/control-plane`: PASS.
- Focused routing, scheduling decision, sessions, executiontargets, database, config, observability, and signer tests:
  PASS.
- OrbStack PostgreSQL migration and two-connection test: PASS.
- OrbStack real Kubernetes reservation-authority integration: PASS after the recorded RBAC negative gate.
- `bun fmt`, `bun lint`, and `bun typecheck`: not run; the user did not request the heavyweight Bun checks.
- No commit, merge, or push was performed by this agent; concurrent branch movement is recorded separately above.

## Cleanup and evidence boundary

The PostgreSQL and Worker namespaces, all namespace-scoped test RBAC/foundation/Pod resources, the short-lived
ServiceAccount token, and the local port-forward were removed. Namespace absence and port `55484` listener absence were
verified.

This is E3. It proves local real-Kubernetes API/RBAC/Pod behavior, PostgreSQL schema/trigger behavior, multi-connection
serialization, generation fencing, immutable evidence, and exact occupancy acknowledgement. OrbStack is a single-node
local cluster. It does not prove independent Region/AZ failure domains, managed-cloud IAM, a production external
publisher, heterogeneous CPU/memory/GPU reservation vectors, production Worker images, multi-Tenant load, or a
production-duration soak. Those remain E4 or later Stage 4 gates.
