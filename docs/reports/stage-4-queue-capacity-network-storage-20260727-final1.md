# Stage 4 queue, capacity, network, and storage closure — 2026-07-27 final1

Status: **PASS (current dirty worktree mechanics)**
Supported product boundary: Personal/SSH/Docker plus operator-managed self-hosted Kubernetes; no EKS/GKE/AKS,
native cloud billing export, cloud KMS/IAM, or managed-cloud availability claim

## Queue admission, quotas, and fairness

Migration `000089_execution_queue_classes_and_scoped_quotas.sql` adds immutable `interactive`, `automation`, and
`batch` classes, explicit priority and quota units, and an Automation identity rule. Tenant admission now composes
Tenant, Project, Session, and Automation policies in one locked hierarchy. Kubernetes Pod selection and reusable
Worker Claim both use the shared fair queue: starvation-age promotion, class/priority order, Tenant active service
units, then Tenant-local FIFO. The queue metrics include bounded `queue_class` labels.

A disposable PostgreSQL 17 run passed:

- `TestCreateTurnPostgresConcurrentQuotaAllowsOnlyOneActiveExecution`;
- `TestClaimFairShareOrderPostgresPrefersTenantWithFewerActiveServiceUnits`; and
- `TestPostgresConcurrentClaimAndExpiredRecovery` for multi-active Outbox claiming.

## Capacity, cold-start, and autoscaling

Migration `000090_worker_pool_autoscaling.sql` persists per-Pool policy and controller state. The leader-elected
controller scales against Queue Depth and oldest queued age, raises capacity immediately, and applies cooldown plus
stabilized scale-down. Effective desired units feed the existing release-aware/min-idle warm authority. The
interactive hard deadline atomically cancels only the exact overdue, still-unclaimed Execution; claimed, fresh,
batch, other-Pool, and other-generation work is left untouched.

Migration `000091_execution_target_capacity_vectors.sql` persists the latest versioned Target capacity authority.
The Kubernetes publisher derives total/allocated/available Pods, CPU millicores, memory bytes, ephemeral bytes, and
optional GPU units from live ResourceQuota status, then reports the minimum resource-constrained schedulable
Pod-equivalent units. Incomplete or stale authority fails closed. Prometheus recording rules retain bounded historical
capacity/queue/autoscaling series according to the operator's configured retention; PostgreSQL intentionally keeps
only current scheduling authority rather than duplicating a time-series database.

## Outbox and Queue pressure

Migration `000092_outbox_pressure_autoscaling.sql` stores one durable pressure decision. The Dispatcher adapts batch
size and concurrency, partitions by `message_key` so same-key ordering is retained, and scales back after pressure
falls. At the hard depth threshold only new `execution.queued` admission is throttled; cancellation, recovery,
terminal, and cleanup messages remain admissible so the system can drain. Queue pressure is independently bounded by
the hierarchical queued-Execution quota and drives Worker Pool desired capacity.

## Self-hosted K8s network and private dependencies

Managed Target configuration now freezes explicit egress CIDRs and TCP ports, private-network CIDRs, credential-free
HTTP(S)/SOCKS5 proxy authorities, bounded no-proxy entries, and optional GPU resources. Generated NetworkPolicy:

- selects only the Target's Pods;
- permits DNS only to `kube-system` / `k8s-app=kube-dns` on TCP/UDP 53;
- permits declared CIDRs only on declared TCP ports;
- automatically includes the Control Plane and proxy ports; and
- subtracts link-local and known metadata endpoints from broad CIDRs.

Private Git resolution is disabled by default and only accepts explicitly declared RFC1918, CGNAT, or IPv6 ULA
subnets covered by egress policy; loopback, metadata, link-local, and public widening remain rejected. Private Worker
images keep the existing exact-registry `worker_image_pull` binding. Npm/PyPI `package_read` grants now materialize
execution-local mode-0600 config files, enter SecretGuard, and reach Provider Host only through generation-fenced
absolute-path aliases; ambient package-manager config is never inherited. Provider proxy variables use the same
controlled alias pattern.

## Workspace Storage

[`Workspace Storage Boundary v1`](../contracts/workspace-storage-boundary-v1.md) freezes the roles of ephemeral disk,
Git-cache PVC, CSI snapshot, Object Storage, and Git. The live K8s Workspace remains a size-bounded `emptyDir`; the
only supported PVC field is the rebuildable Git cache. A shared live Workspace PVC is rejected as an unknown Target
field. Durable recovery requires a Ready Git-reference/Patch/Snapshot Checkpoint, with Object Storage bytes verified
by size/SHA and bound through the RecoveryBundle. CSI snapshots cannot substitute for that authority.

## Verification

The final focused Go pass covered these packages and all passed:

```text
executionqueue fairqueue quotas poolautoscaling warmcapacity targetcapacity
outbox observability executiontargets executions sessions httpapi config database cmd/api
```

Additional results:

- agentd + executiontargets focused network/package tests: PASS;
- Provider Host `src/providerHost.test.ts`: 23/23 PASS;
- Kubernetes network selector/port/metadata/proxy/private-CIDR negative tests: PASS;
- Workspace `emptyDir`/Git-cache PVC and unknown live-PVC rejection tests: PASS;
- resilience asset validator: 24+9 unit groups plus the full fake disruption matrix PASS;
- Kustomize render, YAML parse, shell syntax, and `git diff --check`: PASS.

The real dual-cluster pressure result is recorded separately in
[`dual-cluster final7`](stage-4-dual-cluster-pressure-chaos-20260727-final7.md), and the single-machine regression in
[`Personal/SSH/Docker final1`](stage-4-single-node-regression-20260727-final1.md).
