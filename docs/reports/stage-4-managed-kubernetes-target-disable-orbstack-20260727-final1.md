# Stage 4 managed Kubernetes Target disable — OrbStack E3 final1

Date: 2026-07-27 (Asia/Shanghai)

Result: **PASS**. A tenant-owned managed Kubernetes Target reached a real OrbStack Kubernetes API, published one fresh
`healthy/available` `exact-active-v1` observation with zero allocated and acknowledged capacity, committed the new
terminal disable operation once, and was then skipped by the Reconciler without a Health-version advance or Pod
recreation. Independent PostgreSQL tests proved that the HTTP mutation shares the Reconciler advisory lock and that a
concurrent Target Group Member insert cannot cross the Target lifecycle lock.

This is local E3 evidence. It does not replace EKS/GKE/AKS identity, independent failure-domain, or production soak
evidence.

## Source and environment

- source HEAD: `82a64f8012ff696433684910d00448475e12d427`
- branch: `codex/saas-tenancy-user`
- worktree: intentionally dirty; no staging, commit, push, or unrelated-file cleanup was performed
- Kubernetes context: explicit `orbstack` from `/Users/huang/.kube/config`; no default context was relied on
- Kubernetes server: `v1.34.8+orb1`, `linux/arm64`
- isolated namespace: `synara-stage4-target-disable-final1`
- captured namespace UID: `a07fe5a2-4571-4ab0-b223-d32fe7d77bb8`
- PostgreSQL: `16.14 (Debian 16.14-1.pgdg13+1)`
- PostgreSQL image: `postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20`
- applied schema: `85`
- Control Plane oracle namespace: existing `synara-system`, two replicas Ready before/after acceptance

The isolated ServiceAccount was granted namespaced Pod, ServiceAccount, Secret, ResourceQuota, and NetworkPolicy
operations. Explicit RBAC probes returned `yes` for namespaced Pod create and NetworkPolicy patch, and `no` for Namespace
delete.

## Product behavior proved

The new endpoint is:

```text
POST /v1/tenants/{tenantID}/execution-targets/{executionTargetID}/kubernetes/disable
```

It requires `worker.manage`, is bodyless and terminal, returns the safe Target projection, emits
`Idempotency-Replayed: true` on a terminal retry, and writes exactly one
`execution_target.kubernetes_disabled` Audit row on the first mutation.

Before the status update, the implementation holds the same
`synara:kubernetes-execution-reconciler` cycle lock as real reconciliation plus the Target row lock. It rejects disable
while any routing Member, fixed active/suspended Session, nonterminal Execution, non-disabled Worker Pool, unclean
Workspace materialization/cleanup command, nonterminated Worker incarnation, Worker Lease, or non-authoritative/nonzero
managed Health remains. Target Group Member creation now locks Target before Group, matching routed launch lock order and
preventing an active Member insert behind terminal disable.

The terminal mutation keeps the historical Target row and encrypted configuration. It stops future placement,
registration, and Reconciler maintenance; it does not delete a Namespace or other Kubernetes object.

## Automated verification

Focused SQLite/HTTP tests passed:

```text
go test ./internal/executiontargets ./internal/routing ./internal/httpapi -count=1
```

The lifecycle suite covered shared local lock behavior, no-write replay, one Audit, wrong kind/shared ownership, missing
or stale Health, nonzero capacity, and each durable blocker category. The HTTP test covered permission denial, safe
response, first mutation, and replay header.

Full Control Plane Go regression passed:

```text
go test ./... -count=1
```

No `bun fmt`, `bun lint`, or `bun typecheck` command was run, per repository instructions for this conversation.

## Real OrbStack Kubernetes + PostgreSQL proof

The final Kubernetes case ran alone against a fresh database to prevent cross-package fixture pollution:

```text
TestDisableManagedKubernetesTargetOrbStackIntegration
PASS (5.04s test duration)
```

Post-run database evidence for that isolated database:

```json
{"kubernetesTargets":1,"disabledTargets":1}
{"healthRows":1,"healthy":1,"available":1,"zeroAllocated":1,"zeroAcknowledged":1,"exactAuthority":1,"managedSource":1,"minVersion":1,"maxVersion":1}
{"disableAudits":1}
{"members":0,"sessions":0,"executions":0,"pools":0,"materializations":0,"cleanupCommands":0,"workers":0,"leases":0}
```

The exact Target label selected zero Pods before disable and still selected zero after a second real Reconciler call.
Health stayed at version 1 with the same `observedAt`, proving the disabled Target was not reconciled again.

Two additional tests ran sequentially against a separate fresh PostgreSQL database:

```text
TestDisableManagedKubernetesTargetPostgresSharesReconcilerAdvisoryLock  PASS (3.95s)
TestAddMemberPostgresSerializesWithTargetDisable                       PASS (0.66s)
```

The first held the real PostgreSQL advisory lock, observed `kubernetes_reconciler_busy`, released it, and then committed
one disable plus one Audit. The second held an uncommitted Target disable row lock for more than 150 ms; `AddMember`
remained blocked, resumed after commit, returned `target_group_member_target_not_found`, and left member count zero.

## Negative evidence and harness corrections

Three failures were retained as harness/safety evidence rather than hidden:

1. A test publisher identity without the production `managed-kubernetes-routing-publisher:` prefix produced otherwise
   healthy zero-capacity state, but disable rejected it as non-authoritative. The final test uses the production identity
   convention; product safety logic was not weakened.
2. Reusing an already initialized database with a new bootstrap installation ID failed the installation fence. The live
   test now creates a unique tenant scope directly and does not mutate installation authority.
3. Running the Kubernetes and routing packages concurrently against one database let the routing fixture insert unrelated
   configuration-less Kubernetes Targets while the real Reconciler scanned all Targets. The final Kubernetes case and
   pure PostgreSQL races use separate fresh databases and run sequentially.

## Cleanup proof

Before cleanup, the isolated namespace contained zero Target-labelled Pods. Foundation objects from the successful and
negative attempts were confined to that namespace. Cleanup used the captured Namespace UID in Kubernetes
`DeleteOptions.preconditions.uid`; the API accepted that exact UID and the namespace reached absent state. The dedicated
PostgreSQL port-forward and Kubernetes API proxy were then stopped, and neither local listener remained.

After cleanup:

- `synara-stage4-target-disable-final1` was absent;
- the original `synara-system` Control Plane deployment remained `2/2` Ready;
- both existing Control Plane Pods still reported zero restarts.

The deleted namespace and its disposable PostgreSQL databases are not recoverable. Historical debug namespaces not
owned by this acceptance were left untouched.

## Source hashes

```text
579d68d6a2cc50dc270bcd18128bb79eb933ae647fc82b7c59edfb6aae13edfa  executiontargets/service.go
41886cf5c63c033f5141e04b90511241dd97b5767231adea20f8168e9430f411  kubernetes_target_lifecycle.go
c2102cad5e866664ca0daed1a558325a4b6d9dc9f29c5b99a578cfdbccf2c024  kubernetes_target_lifecycle_test.go
244ba97da6f8f19711835f9738e74e010e8bb248b50cc7ebd7556bbf5480eeae  kubernetes_target_lifecycle_postgres_integration_test.go
6611b2707003cad0ad1007e6e3efb3539b73764c37039d6e337be226c2543e16  kubernetes_target_disable_orbstack_integration_test.go
73f06c0bf37e94950c8d5ad904f42c6e2a586a06dcefb42a7b963467eabcc8b5  kubernetes_reconciler.go
e1538e903e1d19400401c19a3a1caa33657010b61d96acb5f778e3671901bc04  routing/service.go
c57b20e36a2c0b626492ec116db21c0c17c9827e84f01b14a94b21e28ab3719d  member_target_lifecycle_postgres_integration_test.go
eae32299aedb352c55ba7000c7bddf7e1223ec02d8e148de001d5605d5dc33c7  execution_targets_api.go
5e958d48d88dc3387c7c6af7fc0396566fce07a1967190e551a96a8da8394c65  execution_targets_api_test.go
05580fc3a4552c65d0cea6c64a5724a8b54e4fa07402d09787a41f1518ae73e4  httpapi/server.go
9c8435704369387aeea9c0e69aa707398bd1207a893ffa1107aef3a97318c0c5  execution-target-v1.md
2be8bd4f428ef0ed695275a348fc682b8ac8307d0c57c8b1527972d06a4949e7  global-target-routing-dr-v1.md
2d9c06efb8b8c37a7782d1a6795b927422537ce244c52235243409173ab52f92  kubernetes-cluster-lifecycle.md
d5816c097a302d15a8f05a327930cb6babbe931fa3621a7398412e23332e4ba3  control-plane-operations.md
1204aa4debfd05321769b2c6e0f13773e880627376f51ad32b72e51dcf5205a8  TODO.md
ac8b9b60b1ed41a615d39294d7ef0a6896ec9b89f68d946cdc5cb370224c95dc  stage-4-distributed-execution-resource-lifecycle.md
```

These hashes bind the evidence to the dirty working-tree source used for the run, not merely to HEAD.
