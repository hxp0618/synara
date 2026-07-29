# Stage 5 local runtime-isolation acceptance — 2026-07-28

## Evidence status

This report records local development evidence for Stage 5. It is **not** managed-cloud, production-cluster, or final
Stage 5 acceptance. The source tree was dirty throughout the run; no commit, push, or release artifact was created.

- Git branch: `codex/saas-tenancy-user`
- Git HEAD: `0d400eaa6e46f1fc16dcae5010fca79770323cae`
- Final local Worker image: `synara-worker:stage5-local-20260728`
- Final local image ID: `sha256:772e33a4e43053d2fb2fa5d76583d61b6a849fb19839f2a10cfa84fdb7655d00`
- Build identity embedded in the image: version `0.0.0-stage5-local`, Git SHA above
- Evidence class: local dirty-tree build and disposable-cluster observation

## Runtime resource and metadata probes

### Negative discovery on the existing OrbStack cluster

Environment:

- Kubernetes `v1.34.8+orb1`, Linux/arm64
- node `orbstack`, kubelet `v1.34.8+orb1`
- kernel `7.0.11-orbstack-00360-gc9bc4d96ac70`
- runtime `docker://29.4.0`

The initial pressure probe verified finite `cpu.max`, a 64 MiB OOM kill, no HTTP response from the AWS, Alibaba Cloud,
or IPv6 metadata endpoints, and 12/12 successful peer probes. Tightening the PID assertion then exposed that
`/sys/fs/cgroup/pids.max` was `max`. The authoritative kubelet `configz` value was:

```json
{"podPidsLimit":-1}
```

This is a real Stage 5 failure, not a skipped local difference. Kubernetes does not provide a PodSpec PID resource;
the kubelet limit must be finite. The implementation now requires Target `pidsLimit`, checks every node eligible under
the Target `nodeSelector` before any Kubernetes mutation, and checks the actual Pod node again during Pod-bound Worker
registration. Missing access, no nodes, `-1`, `0`, or a value above the Target maximum fails closed with
`kubernetes_pids_limit_unverified` or `kubernetes_workload_identity_pids_limit_invalid`.

### Positive disposable Kind proof

A disposable cluster named `synara-stage5-pids-20260728` used:

- Kubernetes/kubelet `v1.33.1`
- containerd `2.1.1`
- the same OrbStack Linux kernel shown above
- kubelet `podPidsLimit=128`
- final Worker image ID `sha256:772e33a4e43053d2fb2fa5d76583d61b6a849fb19839f2a10cfa84fdb7655d00`

Command:

```text
SYNARA_KUBERNETES_STAGE5_RUNTIME_ISOLATION_TEST=1 \
SYNARA_TEST_KUBERNETES_NAMESPACE=synara-stage5-runtime-20260728 \
SYNARA_TEST_KUBERNETES_WORKER_IMAGE=synara-worker:stage5-local-20260728 \
SYNARA_TEST_KUBERNETES_CONTEXT=kind-synara-stage5-pids-20260728 \
go test ./internal/executiontargets -run TestKubernetesStage5RuntimeIsolation -count=1 -v
```

Result:

```text
Stage 5 Kubernetes runtime isolation PASS
peerChecks=12
forksStarted=120
forksRejected=136
metadataBlocked=true
cpuMaxFinite=true
memory=OOMKilled
```

The fork probe attempted 256 concurrent child processes and observed the kernel reject 136 after the per-Pod budget was
reached. A separate peer Pod remained responsive for 12/12 checks while fork and CPU pressure ran, and the memory probe
terminated with `OOMKilled` at 64 MiB.

`metadataBlocked=true` means the process obtained no HTTP response from any tested metadata endpoint; any HTTP status,
including a non-2xx response, is treated as reachable and fails the assertion. Kind endpoint reachability is useful
local evidence, but Kind's local network is not evidence that a managed-cloud CNI enforces the generated NetworkPolicy.
The probe is a representative process in the production Worker image, not a real Codex/Claude Provider CLI process.

## Pod-bound registration-token handoff

The first live run found that kubelet projected volumes use an AtomicWriter symlink layout, which the original regular-
file-only source reader rejected. After separating the read-only kubelet projection reader from the no-symlink staged
file reader, the second live run found that a non-root init cannot chmod an EmptyDir mount root owned by kubelet. The
stager now creates its own mode-0700 `one-shot/` child and writes the token mode 0600 there.

Final command used the disposable Kind cluster and final Worker image:

```text
go test ./internal/executiontargets -run TestKubernetesStage5RegistrationTokenHandoff -count=1 -v
```

Result:

```text
Stage 5 Kubernetes registration-token handoff PASS
projectedMounted=false
stagedInitially=true
stagedConsumed=true
```

The restricted init was the only container that mounted the projected Pod-bound token. The main container saw only the
one-shot EmptyDir, and the real `agentd.LoadConfig` consumed and durably removed the staged token before Provider start.

## Shared Worker Tenant A to Tenant B probe

The OrbStack/PostgreSQL acceptance command passed on namespace `synara-stage5-isolation-20260728`:

```text
go test ./internal/executions -run TestOrbStackKubernetesGeneralWorkerTenantIsolation -count=1 -v
```

That earlier probe used dirty-tree Worker image ID
`sha256:2de056fd64c1f0fb17b6fe880225c165c769e604de9bb3c3bd1697757411b49e`; later runtime/token evidence used the
final image identified at the top of this report.

Observed identities:

```text
pinnedWorker=d8240444-6d2d-4a30-82c3-36eec0df9829
pinnedUID=12da6d58-13af-4c51-8963-fe32ea43c9f3
sharedWorker=41cdb95a-e024-431d-b2f6-4a6253aa6fd8
sharedUID=232d3c59-1733-4c52-a542-4c2e1dd5298b
pinnedOther=queued
```

On the same shared physical Pod/Worker, Tenant A wrote markers into workspace v2/v3, legacy workspace, Target-scoped Git
cache, quarantine, and private `/tmp`. A claim before the scrub receipt was blocked. After physical removal and the
generation-fenced receipt, Tenant B recursively enumerated the same roots and observed zero residual paths and no
readable Tenant A secret. The pinned Worker did not claim the other Tenant.

Limitation: the live acceptance Worker script performed the physical removal while exercising the real control-plane
claim/receipt state machine. The production `agentd` scrub implementation has focused Go tests for the same roots,
symlink rejection, ownership repair, durable absence, restart recovery, and failure drain, but this run was not a full
real-agentd + Provider Host A-to-B combination. The Stage 5 completion checkbox therefore remains open.

## Automated verification

All of the following passed after the final edits:

- 11 affected Go packages: `agentd`, `executions`, `executiontargets`, `platform`, `bootstrap`, `placement`, `routing`,
  `sessions`, `httpapi`, `database`, and `credentials`.
- Provider Host: 3 files, 82 tests.
- Server: 9 files, 291 passed and 2 skipped.
- Shared sensitive-action policy: 3 tests.
- Web External MCP setup: 10 tests.
- Kubernetes Worker rollout gate: 16 Python unit tests.
- `git diff --check`.

The first parallel TypeScript/Go pass produced two timing failures and one stale test-fixture expectation. Each timing
case passed alone and the full serial suites passed. The stale fixture was corrected to accept only either the direct
test credential or a `synara_task_*` credential paired with a loopback broker URL; the affected agentd tests and full
package then passed.

Per repository instructions, `bun fmt`, `bun lint`, and `bun typecheck` were not run because the user did not explicitly
authorize them in this conversation. Stage 5 cannot be marked complete until those required final gates pass.

## Cleanup and remaining acceptance boundary

After evidence capture:

- PostgreSQL port-forward on local TCP 55432 was stopped.
- OrbStack namespace `synara-stage5-isolation-20260728` was deleted and confirmed absent.
- Kind cluster `synara-stage5-pids-20260728` was deleted and confirmed absent.
- No test Pod remained before deletion.
- The local Worker image was intentionally retained; no existing namespace or Stage 4 report was changed.

Still required for final Stage 5 acceptance:

1. real Provider CLI fork/memory/CPU and metadata probes on every supported managed cluster/CNI/node class;
2. a full real-agentd + Provider Host Tenant A-to-B residual test;
3. structural provenance for repository/tool/web/third-party-MCP results where Provider adapters expose no host-owned
   result boundary today;
4. negative Provider-adapter proof that sensitive direct subprocess/syscall paths cannot bypass the frozen approval
   policy, plus the Stage 9 malicious-Issue test before that channel ships; and
5. the required workspace-wide format, lint, and typecheck pass after explicit authorization.
