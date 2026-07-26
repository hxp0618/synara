# Kubernetes enterprise control plane

This Kustomize base runs two stateless control-plane replicas and grants only the
cluster permissions required by the managed Kubernetes Execution Target reconciler.
PostgreSQL, S3, ingress/TLS, and AWS workload identity remain operator-managed.
Set `trusted-proxy-cidrs` to only the ingress or load-balancer network ranges that append
`X-Forwarded-For`; leaving it empty records the direct peer address and ignores forwarded client IPs.

Create the runtime configuration and Secret without committing either generated file:

```bash
cp deploy/kubernetes/config.example.yaml /tmp/synara-config.yaml
cp deploy/kubernetes/secret.example.yaml /tmp/synara-secret.yaml
# Edit both files, then apply them from /tmp or a secret manager.
kubectl apply -f deploy/kubernetes/namespace.yaml
kubectl apply -f /tmp/synara-config.yaml -f /tmp/synara-secret.yaml
kubectl apply -k deploy/kubernetes
```

Before applying, replace `synara-control-plane:local` in `deployment.yaml` with an
immutable image digest available to the cluster. Configure AWS IRSA, EKS Pod Identity,
or the equivalent workload identity on the `synara-control-plane` ServiceAccount so
the process can access S3 and the configured KMS key without static AWS credentials.

The ClusterRole is cluster-scoped because a Target may request a dedicated Namespace.
If every Target uses an operator-created Namespace with `manageNamespace=false`, create
equivalent namespaced Roles for those Namespaces and remove Namespace management from
the target configuration before narrowing the supplied ClusterRole.

`deploy/kubernetes/rbac.yaml` is intentionally least-privilege for the current
implementation surface: namespace apply, managed Pod lifecycle, namespaced
foundation objects, and `authentication.k8s.io` `TokenReview` for workload
identity verification.

Managed Worker Pods get a separate ServiceAccount with token automount disabled. The
control-plane ServiceAccount token is used only by the reconciler and is never copied
into Worker Pods or execution events.

`GET /metrics` is available on the existing `http` Service port. Prometheus Operator
users can apply the optional `deploy/kubernetes/monitoring` ServiceMonitor and alert
rules after installing the required CRDs; they are not included in the base so a
plain Kubernetes cluster can still apply the Kustomization.

## Disposable Kind acceptance

Run the Stage 2 two-replica and failure acceptance in a disposable Kind cluster:

```bash
KIND_BIN=/path/to/kind deploy/kubernetes/kind-acceptance.sh
```

The wrapper builds and loads the current Control Plane plus pinned PostgreSQL/MinIO images, creates PVC-backed
test dependencies, applies this Kustomize base, verifies two Ready replicas and migration uniqueness, deletes one
Control Plane Pod, interrupts PostgreSQL and MinIO, verifies Worker-token continuity, checks RBAC and scans Pod
logs for random secret Sentinels. The cluster and test resources are deleted on exit.

## OrbStack + disposable Kind dual-cluster DR lane

Run the real-API dual-cluster disaster-recovery integration test with OrbStack
as the primary cluster and a run-unique disposable Kind cluster as the
secondary:

```bash
SYNARA_DUAL_CLUSTER_EVIDENCE_FILE=/tmp/synara-dual-cluster.json \
  deploy/kubernetes/kind-dual-cluster-acceptance.sh
```

The evidence path is required and must not already exist. The wrapper never
changes the user's current Kubernetes context: every primary command names the
`orbstack` context explicitly, while Kind receives an isolated temporary
kubeconfig. A different primary is rejected unless the operator explicitly
sets both `SYNARA_DUAL_CLUSTER_PRIMARY_CONTEXT` and
`SYNARA_DUAL_CLUSTER_ALLOW_NON_ORBSTACK_PRIMARY=1`.

Before creating Kind, the wrapper builds a run-labelled, local-only Worker from
the root `Dockerfile`'s `worker-acceptance` target with
`deploy/worker/build.sh --allow-dirty --load`. It verifies the exact local image
config ID, the BuildKit `containerimage.digest` manifest digest, and the
source-revision/run-owner labels, then loads that image into Kind. Both proven
image identities are supplied to the runtime test because container runtimes
may report either form. The OrbStack primary uses the same local image tag.
Cleanup requires the run-owner label, and after identity verification also
requires the tag's current config ID to match. The cleanup trap still inspects
a tag left by an interrupted build and removes it only when its run-owner label
matches. An existing tag is never reused or overwritten.

The Worker API is reached through `host.docker.internal` from both OrbStack and
Kind by default. Override these DNS hostnames with
`SYNARA_DUAL_CLUSTER_PRIMARY_HOST_ALIAS` and
`SYNARA_DUAL_CLUSTER_SECONDARY_HOST_ALIAS` when the local runtime uses
different container-to-host aliases.

Each cluster receives a random, run-labelled authentication Namespace,
ServiceAccount, namespaced workload ClusterRole/RoleBinding, exact-name
Namespace ClusterRole/ClusterRoleBinding, and a distinct pre-created target
Namespace. The test uses those exact target Namespace names; the wrapper owns
their cleanup. Kubernetes creates return each UID in the same response. Cleanup
first verifies that UID and run label, then submits a raw `DeleteOptions` with
an API-server-enforced UID precondition and waits for that original UID to
disappear; it never performs an ordinary name-only delete. The test receives
only short-lived TokenRequest credentials through its process
environment; tokens and CA material are never written to the final evidence or
the test-detail file. Cleanup verifies both UID and run label before deleting
any Kubernetes object, and verifies the Kind node ownership label before
deleting the secondary cluster. It never creates, changes, or deletes anything
in `synara-system`. Set `SYNARA_DUAL_CLUSTER_KEEP_KIND_CLUSTER=1` only when the
disposable secondary must be retained for diagnosis; run-owned Kubernetes auth
objects are still removed.

The bounded final JSON records only context labels, server versions, node
counts, test and cleanup status, source SHA/dirty state, local Worker image
tag/config ID/manifest digest, allowlisted boolean test assertions, and the
SHA-256 digest of the temporary test-detail JSON. Registration is still served
by a bounded stub, but the stub calls the production Kubernetes registration
verifier against each real API server before issuing its non-persistent test
Worker token. The proof therefore covers the Target-audience projected token,
TokenReview, ServiceAccount/Pod name/UID claims, live Pod GET, Target ownership,
and rejection of the unbound Control Plane Kubernetes credential. It does not
claim the production `/v1/workers/register` handler or Worker-row persistence.
A failure or interrupt writes the same evidence shape when the output path was
successfully reserved.

## Disposable Kind resilience lane

Run the additive multi-node resilience acceptance in a disposable Kind cluster:

```bash
KIND_BIN=/path/to/kind deploy/kubernetes/kind-resilience-acceptance.sh
```

The resilience lane reuses the existing Stage 2 bootstrap, then records JSON
evidence for:

- least-privilege RBAC, including `TokenReview`, Namespace apply, and Pod operations;
- multi-node Kind topology and control-plane spread;
- PostgreSQL-backed reconciler Leader Election by guarding the active lease in
  one exact-PostgreSQL-Pod `psql` session, deleting the exact current holder with
  an immutable Pod-UID precondition, and requiring a different Ready Pod at the
  next fencing-token epoch;
- control-plane failover with optional pre/post verification hooks;
- bounded node-drain simulation by cordoning a node and replacing only the
  targeted control-plane Pod;
- bounded Kind-only node partition smoke by disconnecting a selected node
  container from its Docker network and reconnecting it by default;
- an explicit non-Kind node-partition adapter point driven by
  `SYNARA_K8S_NODE_PARTITION_START_HOOK`,
  `SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK`, and
  `SYNARA_K8S_NODE_PARTITION_STOP_HOOK` when
  `SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE=1`;
  `SYNARA_K8S_NODE_PARTITION_START_HOOK_TIMEOUT_SECONDS` and
  `SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK_TIMEOUT_SECONDS` and
  `SYNARA_K8S_NODE_PARTITION_STOP_HOOK_TIMEOUT_SECONDS` independently bound
  disruption, external observation, and heal execution, while
  `SYNARA_K8S_NODE_PARTITION_HOOK_TIMEOUT_SECONDS` remains the shared fallback
  for operators using the original hook contract;
- an optional bounded soak loop driven by `SYNARA_K8S_RESILIENCE_SOAK_SECONDS`.

The leader-takeover guard uses a nonce-bound V2 handshake rather than a timed
sleep. A private writer controller is the only process that owns the SQL FIFO;
the main shell and its later `kubectl get/delete` children inherit no SQL writer.
The session emits lease then Ready evidence while holding the advisory lock and
waits until the UID-preconditioned DELETE succeeds or an ambiguous result is
reconciled as the original UID being absent, terminating, or replaced. Only then
does the runner send the explicit unlock command. Retries are allowed only while
that same original UID remains active. A pass requires the same session's
`UNLOCKED|true` record and zero exit, followed by proof that the original holder
UID disappeared. EOF, malformed or reordered evidence, writer/controller
failure, PostgreSQL Pod identity/Ready drift, and watchdog expiry fail closed;
cleanup only breaks the stream and never manufactures a successful unlock.

The multi-node wrapper pins PostgreSQL and MinIO to the dedicated
`synara.io/resilience-dependency=true` Worker before their PVCs bind, leaving a
safe Control Plane node for both node-level scenarios. Required cases fail the
run when they cannot execute; only cases explicitly named in
`SYNARA_K8S_RESILIENCE_ALLOW_SKIPPED_CASES` may be skipped without failing the
overall evidence.

The final evidence report remains the existing JSON document written to
`SYNARA_K8S_RESILIENCE_EVIDENCE_FILE` (or a temporary path for non-soak runs).
The harness also keeps two additive sidecars beside that path:

- `<evidence>.journal.jsonl`: append-only progress events for baseline
  completion, top-level case completion, soak-cycle completion, and final
  completion. It records timestamps, status, case or cycle identity, and only
  bounded detail summaries or digests.
- `<evidence>.partial.json`: the latest running snapshot, updated atomically in
  place and left behind on failures or interrupts for inspection.

When `SYNARA_K8S_RESILIENCE_SOAK_SECONDS` is greater than `0`, you must set
`SYNARA_K8S_RESILIENCE_EVIDENCE_FILE=/path/report.json` explicitly; soak runs
may not fall back to an anonymous temporary file.

Optional failover hook commands are still supported, but recorded hook evidence
redacts command/stdout/stderr content and retains digests plus byte counts
instead, so stored evidence does not leak hook secrets.
Managed node-partition hooks use the same redaction policy. The runner creates one
unpredictable operation ID and challenge, then launches
`managed-hook-controller.py` as a direct background child for start, verify, and
stop. Each phase receives a fresh artifact path and must write
`synara.managed-hook-transition.v1` bound to the exact operation, phase,
context/namespace, immutable Node/Pod UIDs, and
`SYNARA_MANAGED_HOOK_CHALLENGE`. Required transitions are `terminal-applied`,
`applied`, and `terminal-healed`. Exit zero without
that exact fresh artifact fails closed.

On real managed contexts the controller accepts only `systemd-user` with
`securityBoundary=true`. It rejects a non-Linux, non-cgroup-v2, old-systemd, or
unavailable user manager with exit 125 before starting the hook. The hook runs
in a deterministic transient unit using `ExitType=cgroup`,
`KillMode=control-group`, and collection. The controller binds the unit
InvocationID and an opened cgroup device/inode and requires recursive
`cgroup.events populated=0` before returning success. A controller failure or
interrupt triggers recovery of the exact operation/phase unit before stop/heal;
the case remains failed even when recovery succeeds.
The harness accepts only exact-key results whose operation, scope, unit, phase,
backend, transition, boundary, and SHA-256 fields recompute correctly. Any
nonzero or rejected execute result runs exact-unit recovery even when it claims
termination was confirmed. Stored managed-hook and managed-case evidence keeps
only this validated projection and target digests, never raw target scope.
This sanitization is shared by top-level and soak-cycle final, partial, and
journal detail paths. Nonzero results cannot claim `passed` /
`transition-proved` semantics.
Managed redaction is selected from the runner's node-partition case and non-Kind
context, not from adapter-controlled JSON, and covers pre-hook failures and
`rawTarget` fields. The controller starts behind a private same-PID launcher
handshake: it publishes its own creation identity, waits for parent release,
then `exec`s the controller. Failed identity binding expires without signalling
the numeric PID and never reaches controller execution.

The `managed-validation` context uses `process-group-test` with
`securityBoundary=false` and reports `testOnly=true`. It is repository fake
coverage, not production containment evidence; a new-session child can escape
it. The outer shell records only its own controller child identity and accepts
only sanitized controller JSON. Hook-controlled PID/PGID files, HMAC authority,
and inherited runner descriptors are not part of the protocol. Operators should
keep stop/heal idempotent and safe to retry.

For non-Kind clusters, node partition remains an external adapter point rather
than a shipped cloud fault injector. The harness proves that Synara called all
three hooks and received structurally valid, target-bound transition artifacts;
the external adapter remains trusted and must supply provider-side evidence. It
does not claim real cloud or AZ-level fault validation from this repository alone.

Use `deploy/kubernetes/resilience-acceptance.sh --dry-run` to emit the planned
cases without touching a cluster, and `deploy/kubernetes/validate-resilience-assets.sh`
for static validation of the manifests and scripts.

`deploy/kubernetes/acceptance.sh` defaults to Kind contexts and refuses other clusters. Running it against an
explicitly disposable non-Kind cluster requires `SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE=1`; the script deletes
the `synara-system` Namespace and supplied ClusterRole resources during cleanup, so never use that override on a
shared or production cluster.
