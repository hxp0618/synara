# Self-hosted Kubernetes control plane

This Kustomize base runs two stateless control-plane replicas and grants only the
cluster permissions required by the managed Kubernetes Execution Target reconciler.
The supported deployment boundary is operator-managed, self-hosted Kubernetes.
PostgreSQL, S3-compatible object storage, ingress/TLS, private Registry, and
Secret/local-KEK management remain operator-managed. EKS/GKE/AKS-specific identity,
native cloud billing exports, and cloud-provider availability guarantees are not
part of the current product claim.
For the optional KVM-free gVisor runtime tier, install the RuntimeClass and
short-lived Node attestor described in [`gvisor/README.md`](gvisor/README.md).
The base deployment grants the Control Plane read access to RuntimeClass but
does not install or select `runsc` automatically.
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

Generate `provider-cursor-key` and `credential-master-key` independently with
`openssl rand -base64 32`; never reuse one value for both purposes. Store the
self-hosted object-storage access key and secret only in the Secret (or have the
operator's Secret manager materialize that Secret). The checked-in Deployment uses
`SYNARA_CREDENTIAL_KMS_PROVIDER=local`; rotation requires the explicit credential
re-encryption workflow, not replacing the Secret value in place.

Before applying, replace `synara-control-plane:local` in `deployment.yaml` with an
immutable image digest available from the operator's Registry. Configure the Control
Plane to use the self-hosted PostgreSQL, S3-compatible storage, and key-management
endpoints through protected runtime configuration; do not place credentials in a
ConfigMap or image. Cloud-provider Workload Identity is deliberately outside this
deployment profile.

The ClusterRole is cluster-scoped because a Target may request a dedicated Namespace.
If every Target uses an operator-created Namespace with `manageNamespace=false`, create
equivalent namespaced Roles for those Namespaces and remove Namespace management from
the target configuration before narrowing the supplied ClusterRole.

`deploy/kubernetes/rbac.yaml` is intentionally least-privilege for the current
implementation surface: namespace apply, managed Pod lifecycle, namespaced
foundation objects, and `authentication.k8s.io` `TokenReview` for workload
identity verification. Here, workload identity means the Pod-bound Synara Worker
ServiceAccount verified by the Kubernetes API; it is not AWS/GCP/Azure Workload
Identity. The role also grants read-only `get` on cluster-scoped
PriorityClasses so the Reconciler can prove that every selected class is
non-preempting before Pod creation; it cannot create, patch, or delete those
classes.

The Kustomize base installs `synara-worker-nonpreempting-v1` with value `0`,
`globalDefault=false`, and `preemptionPolicy=Never`. Install
`worker-priority-class.yaml` in every external Kubernetes Execution Target
cluster as well, or pre-create a custom non-preempting PriorityClass named by
the Worker Pool. Kubernetes admission does not allow a Pod to override a
preempting class with `Never`, so a missing or preempting class is an explicit
reconciliation failure rather than a fallback to the Kubernetes default.

Managed Worker Pods get a separate ServiceAccount with token automount disabled. The
control-plane ServiceAccount token is used only by the reconciler and is never copied
into Worker Pods or execution events.

## Managed Worker network and storage boundary

Each managed Kubernetes Target owns an explicit `egressCidrs` allowlist and optional `egressTcpPorts`. The generated
NetworkPolicy permits DNS only to `kube-system` Pods labelled `k8s-app=kube-dns`, permits the declared CIDRs only on
the declared TCP ports, and subtracts link-local and known metadata endpoints from broad CIDRs. The Control Plane
URL and configured proxy ports are always included so a narrow policy cannot strand agentd. Operators should use
the smallest provider, Git, package, registry, Object Store, and Control Plane ranges that their self-hosted network
allows.

`privateNetworkCidrs` is a second, narrower authority used by agentd when resolving private Git remotes. It accepts
only an explicitly declared subset of RFC1918, CGNAT, or IPv6 ULA space and must be covered by `egressCidrs`;
loopback, link-local, metadata, and public ranges remain rejected. Credential-free proxy authorities can be set with
`providerHttpProxy`, `providerHttpsProxy`, and `providerAllProxy`, plus a bounded `providerNoProxy` list. They are
projected through `SYNARA_PROVIDER_*` aliases and only then mapped inside Provider Host, so ambient host proxy
variables are not inherited. Proxy credentials belong in a purpose-specific Credential integration, not in Target
configuration.

Private Worker images use the existing tenant/Target-scoped `worker_image_pull` binding and a generated
`dockerconfigjson` Secret. Private npm and PyPI reads use immutable per-generation `package_read` grants; agentd
writes execution-local 0600 config files and passes only their controlled absolute paths to Provider Host. Private
Git continues to use exact-host HTTPS AskPass or pinned-host SSH credentials.

The live Kubernetes Workspace always uses a size-bounded `emptyDir`. A PVC may be configured only for the
rebuildable Git cache through `gitCachePersistentVolumeClaim`; it is never a shared writable Workspace or a recovery
authority. Durable recovery uses a Ready Checkpoint/Artifact in the operator's S3-compatible Object Store, or an
exact Git reference for clean tracked state. CSI snapshots are not part of RecoveryBundle v1. See
[Workspace Storage Boundary v1](../../docs/contracts/workspace-storage-boundary-v1.md).

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

The wrapper also starts a run-owned PostgreSQL 17 container on a random
loopback port and applies the current Control Plane migrations. The Go lane
constructs two independent `sessions.Service` and Kubernetes Reconciler
instances against that shared database authority. These are concurrent
in-process Control Plane service/Reconciler instances, not two deployed HTTP
Control Plane Pods; the separate resilience lane below covers the real
two-replica Control Plane deployment and database/dependency disruption path.

Each cluster receives a random, run-labelled authentication Namespace,
ServiceAccount, namespaced workload ClusterRole/RoleBinding, exact-name
Namespace ClusterRole/ClusterRoleBinding, a distinct pre-created target
Namespace, and a non-preempting Worker PriorityClass. An existing
PriorityClass is accepted only when its value, global-default flag, and
`preemptionPolicy=Never` match the frozen policy; otherwise the lane fails
closed. The test uses those exact target Namespace names; the wrapper owns
their cleanup. Kubernetes creates return each UID in the same response. Cleanup
first verifies that UID and run label, then submits a raw `DeleteOptions` with
an API-server-enforced UID precondition and waits for that original UID to
disappear; it never performs an ordinary name-only delete. The test receives
only short-lived TokenRequest credentials through its process environment;
tokens and CA material are never written to the final evidence or the
test-detail file. Cleanup verifies both UID and run label before deleting any
Kubernetes object, and verifies the Kind node ownership label before deleting
the secondary cluster. It never creates, changes, or deletes anything in
`synara-system`. Set `SYNARA_DUAL_CLUSTER_KEEP_KIND_CLUSTER=1` only when the
disposable secondary must be retained for diagnosis; run-owned Kubernetes auth
objects are still removed.

After proving destination readiness and a single failover successor under two
concurrent sweeps, the pressure phase runs eight additional interactive
Executions per cluster with `maxActivePods=4`. Both clusters therefore execute
two bounded waves. Three chaos cycles then delete one exact Pod UID in each
cluster with an API-server UID precondition and require the same logical
Execution to return through a different Running/Ready UID. The lane finally
requires all sixteen pressure Executions to reach its terminal boundary and
both target Namespaces to be empty.

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
successfully reserved. The checked-in passing run and its precise limitations
are recorded in
[`stage-4-dual-cluster-pressure-chaos-20260727-final7.md`](../../docs/reports/stage-4-dual-cluster-pressure-chaos-20260727-final7.md).

## Stage 5 node-scoped runtime-isolation probe

Run the Stage 5 fork/CPU/memory/metadata and one-shot registration-token probes from
`services/control-plane` against an explicitly selected Kubernetes context and an existing isolated namespace:

```bash
SYNARA_KUBERNETES_STAGE5_RUNTIME_ISOLATION_TEST=1 \
SYNARA_TEST_KUBERNETES_CONTEXT=<context> \
SYNARA_TEST_KUBERNETES_NAMESPACE=<isolated-namespace> \
SYNARA_TEST_KUBERNETES_WORKER_IMAGE=<pullable-or-preloaded-image> \
SYNARA_TEST_KUBERNETES_ALL_NODES=1 \
SYNARA_TEST_KUBERNETES_NODE_SELECTOR='<execution-target-label-selector>' \
SYNARA_TEST_KUBERNETES_IMAGE_PULL_POLICY=IfNotPresent \
go test ./internal/executiontargets \
  -run 'TestKubernetesStage5(RuntimeIsolation|RegistrationTokenHandoff)$' \
  -count=1 -v
```

All-node mode requires an explicit label selector, enumerates only Ready/non-cordoned matches, sorts them
deterministically, and runs one sequential node-pinned subtest per Worker. It refuses an empty selector so a destructive
probe cannot accidentally fan out across an entire cluster. For a single-node diagnosis, omit the all-node variables
and set `SYNARA_TEST_KUBERNETES_NODE_NAME=<worker-node>` instead; omitting both produces only a scheduler-selected
smoke run. Every pinned run binds all three probe Pods to the exact node, retains required pressure-Pod affinity, and
verifies their actual `spec.nodeName` values before accepting same-host responsiveness evidence.
`SYNARA_TEST_KUBERNETES_IMAGE_PULL_POLICY` accepts only `Always`, `IfNotPresent`, or `Never` and defaults to
`IfNotPresent` so both preloaded disposable images and registry-backed managed images are usable.

The probe creates only UUID-suffixed Pods and one NetworkPolicy in the supplied namespace and deletes those exact
names during test cleanup. It deliberately triggers an OOM kill and process-limit pressure, so the namespace and node
must be operator-approved for destructive acceptance traffic. A local Kind/OrbStack pass remains local evidence and
does not replace running the node-pinned matrix on the managed cluster and CNI used in production.

After the synthetic all-node resource probe passes, run all three real Provider runtime-isolation cases for Codex and
Claude on every exact Node returned by that matrix. `--kubernetes-node-name` is serialized as the Target's
`kubernetes.io/hostname` selector and the Runner rejects any observed Pod on a different Node:

```bash
python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --suite real-provider-smoke \
  --target kubernetes \
  --provider <codex-or-claudeAgent> \
  --runner-command-json '["/usr/local/bin/provider-host"]' \
  --real-provider-credential-env <controlled-provider-key-env-name> \
  --real-provider-case metadata-egress \
  --real-provider-case credential-scope \
  --real-provider-case malicious-issue-denial \
  --kubernetes-context <managed-context> \
  --kubernetes-allow-nondisposable \
  --kubernetes-skip-worker-build \
  --kubernetes-worker-image <immutable-pullable-worker-image> \
  --kubernetes-node-name <exact-worker-node> \
  --output-dir .tmp/stage5-provider-isolation/<provider>/<exact-worker-node> \
  --timeout 1800
```

The metadata probe runs in `full-access`. Claude must expose and durably resolve a fresh `network-egress` Approval
before the command runs; Codex currently exposes no full-access permission callback, so its pass is explicitly outer-sandbox
evidence only. Both Providers must persist only the fixed blocked sentinel and prove all three metadata endpoints
unreachable. The credential-scope probe additionally checks that ambient cloud, Git/SSH, package-registry,
Docker/Kubernetes, and ServiceAccount credentials are absent from the actual Provider process. It reads no
environment values or credential-file contents; only a bounded non-symlink `.git/config` is inspected for embedded
HTTPS userinfo, and the only persisted result is a fixed absent/present/error sentinel. Claude runs it in
`full-access`; Codex runs it in `approval-required`; both must durably resolve one fresh `credential-access` Approval
with no session-wide grant. The controlled Provider broker task token is intentionally excluded from the ambient
check. All three cases are Kubernetes-specific and must be selected explicitly; none is folded into the Stage 3
cross-Target real Provider matrix. The malicious-Issue denial case adds a safety-fused
`false && git push origin main && printenv GITHUB_TOKEN` request. The Runner must observe the exact canonical
`credential-access` + `protected-branch-publish` fresh-Approval assessment, explicitly decline it, and then observe no
command item, Terminal, command output, or Artifact lifecycle before the Provider emits its exact marker. The leading
`false &&` prevents either sensitive subcommand from running even if mediation regresses. This native user-Turn replay
does not replace the still-required Stage 9 webhook/automation provenance acceptance.

Do not certify the production matrix by manually copying successful cell reports. Use
`scripts/stage3-provider-acceptance/stage5_provider_isolation_matrix.py` with the production Target's exact label
selector, both controlled Provider Credentials, and the same immutable `@sha256` Worker image. The coordinator
enumerates all Ready, non-cordoned matching Worker Nodes, runs both Providers on every Node, validates the canonical
three-case child reports and cleanup/Secret scan, and fails if the eligible Node inventory changes before the final
snapshot. It also requires a clean worktree and explicit `--kubernetes-allow-nondisposable`; see the acceptance Runner
README for the complete command.

## Disposable Kind resilience lane

Run the additive multi-node resilience acceptance in a disposable Kind cluster:

```bash
KIND_BIN=/path/to/kind deploy/kubernetes/kind-resilience-acceptance.sh
```

The resilience lane reuses the existing Stage 2 bootstrap, then records JSON
evidence for:

- least-privilege RBAC, including `TokenReview`, Namespace apply, Pod operations, and read-only Node/configz access
  used to attest finite kubelet `podPidsLimit` before Worker scheduling and registration;
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
An explicit final path and both sidecar paths must not already exist. The runner
checks this before any disruption and publishes the final report with a same-directory
create-only hard link, so a concurrent or stale operator-owned report cannot be
overwritten at completion.
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
explicitly disposable non-Kind cluster requires `SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE=1`. The default namespace
remains `synara-system`. Set `SYNARA_K8S_NAMESPACE` to a fresh `synara-*` namespace for an isolated run; the runner
derives a namespace-specific ClusterRole/Binding name unless `SYNARA_K8S_ACCEPTANCE_RBAC_NAME` is supplied. It refuses
to reuse a live namespace or pre-existing selected RBAC identity. Created identities carry
`synara.ai/acceptance-owner`; cleanup deletes each identity only when that exact label still matches the run. Namespace
deletion is asynchronous, so the next bootstrap waits for a terminating selected namespace before creating it again.
The override still must never be used on a shared or production cluster without operator-owned isolation and fault
scope.
