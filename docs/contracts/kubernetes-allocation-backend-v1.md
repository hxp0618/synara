# Kubernetes Allocation Backend v1

Kubernetes Execution Targets accept an optional `allocationBackend`:

- `native-pod` (default): the existing Synara Pod reconciler;
- `sandbox-operator-standard`: sandbox-operator with the standard kubelet runtime;
- `sandbox-operator-cocoon`: sandbox-operator with the vk-cocoon KVM runtime.

Sandbox backends additionally require `sandboxTemplateName`,
`sandboxWarmPoolName`, and an optional `sandboxClaimReadyTimeoutSeconds`
(default 30, range 1–300). They also require `sandboxAllowedTenantIds` to
explicitly contain the Target's tenant UUID. The native backend rejects this
Sandbox-only field, so rollback removes it together with the template/pool
configuration.

Configuration selects an adapter and its acceptance contract. It does not
authorize an in-place backend change for an active Generation. The immutable
scheduling decision must pin the selected Target and backend before external
resource creation.

The tenant allowlist is enforced twice. Target configuration fails closed
before a Kubernetes client is opened when its owning tenant is absent. After
execution state is loaded, every candidate Execution tenant is checked again
before backend transition or Claim materialization. A wrong-tenant database row
therefore cannot borrow a canary Target merely by referencing its ID.

`native-pod` acceptance requires only the existing Kubernetes Target gates.
`sandbox-operator-standard` additionally requires the Sandbox, SandboxClaim,
and SandboxWarmPool APIs, a Ready operator, the configured template and pool,
and a positively observed standard runtime. `sandbox-operator-cocoon` replaces
the final requirement with a vk-cocoon virtual node, a positively observed KVM
runtime, and the attested host-supervisor boundary described below. KVM alone
is substrate evidence and never authorizes Synara materialization.

Sandbox API acceptance reads all four CRDs from the exact target, requires
`v1beta1` to be an established storage version, verifies that the pool points
to the configured template, and treats reconciled pool status as the controller
health signal. Because `SandboxWarmPool.spec.replicas` means unclaimed idle
Sandboxes, Synara projects `min(effectiveDesiredIdle, maxActivePods-activeClaims)`.
It waits for the operator to observe that projection before creating another
Claim. Operator status remains observed capacity and never becomes scheduling
authority.

Synara applies the pool with `updateStrategy.type: Recreate` and observes every
pool-owned idle Sandbox before materializing a Claim. New Claims remain fenced
with `kubernetes_sandbox_warm_pool_rollout_pending` until the owned count and
pool status match desired capacity and every idle Sandbox carries the current
template agentd image. This prevents `OnReplenish` from lending an old idle
Worker after a promoted image update.

The adapter acceptance interface and materializer are fail-closed. Every
allocation is persisted before a `SandboxClaim` is applied. Once the Claim is
Ready, Synara binds the Claim UID, Sandbox name and UID, and backing Pod name
and UID to the immutable execution Generation. Cleanup pre-reads the Claim and
uses its exact UID as a Kubernetes delete precondition; a name reused with a
different UID is fenced instead of deleted.

The allocation configuration digest includes the observed SandboxTemplate UID,
resourceVersion, and the exact agentd image selected for the Execution.
Updating a template in place therefore cannot silently change an
already-authorized Generation; a new Generation must accept the new template
identity and image.

Worker Release selection remains Synara authority. Before applying a Claim, the
adapter requires `SandboxTemplate.spec.podTemplate`'s `agentd` image to equal
the Execution's resolved release image. After assignment it checks the backing
Pod image again before binding the allocation. A mismatched Pod is fenced and
its exact Claim is deleted instead of registering the wrong release. A single
configured SandboxWarmPool cannot safely serve promoted and canary revisions at
the same time, so canary-selected Executions fail closed until per-release
template/pool routing is implemented. Promoted rollout updates the external
template; the enforced `Recreate` pool strategy drains/replenishes idle members,
and the rollout fence keeps new Executions queued until the current image and
capacity are fully observed.

The configured `SandboxTemplate` must expose the
`synara.io/assigned-execution-id` Pod label through an updating downward-API
volume and set `SYNARA_AGENTD_ASSIGNED_EXECUTION_ID_FILE` to the mounted item.
The materializer supplies that label through `additionalPodMetadata`; agentd
waits on the file before registration, so a pre-warmed Sandbox is pinned to its
assigned execution when the Claim is fulfilled. A downward-API environment
field is not accepted because it is evaluated only when the Pod starts. The template annotation
`sandbox.cocoonstack.io/runtime` must be `standard` or `vk-cocoon` to match the
selected backend.

## Cocoon host-supervisor gate

The frozen `microvm-isolated-v1` boundary keeps Synara agentd, the Control Plane
Worker credential, and the Provider Credential broker outside the guest. Only
the Provider Host, Provider CLI, and tool processes run inside the microVM, over
an Execution/Generation-fenced vsock transport. A Ready virtual node must carry
all of these labels on the same node before `sandbox-operator-cocoon` can pass
target acceptance:

- `sandbox.cocoonstack.io/kvm-ready=true`;
- `synara.io/host-supervisor=v1`;
- `synara.io/provider-transport=vsock-v2`;
- `synara.io/isolation-profile=microvm-isolated-v1`.

These are attestation outputs, not operator configuration shortcuts. Manually
adding them does not constitute acceptance evidence. The attesting component
must prove the host supervisor, guest identity fence, vsock peer binding,
credential non-entry into the guest, and the negative isolation suite. Current
vk-cocoon v0.3.5 provides the VM/exec/logs substrate but does not implement this
Synara host-supervisor contract, so it remains fail closed even on a KVM-ready
node.

After assignment, agentd retries only the explicit
`kubernetes_sandbox_allocation_not_bound` registration result for
`SYNARA_AGENTD_SANDBOX_ALLOCATION_BIND_TIMEOUT` (default 30 seconds, range 1
second–5 minutes). This value must be at least the configured Claim readiness
timeout in HA environments. A verified Kubernetes Pod-bound Worker may register
while its Target is transiently `offline`; disabled Targets and registrations
without Pod-bound trust remain rejected. Sandbox-owned backing Pods are
identified by their `Sandbox` controller ownerReference and are not mistaken
for native Pods by the backend-transition drain fence.

The operator metadata-domain allowlist must include `synara.io`; terminal Claim
rejections such as `InvalidMetadata` are fenced and cleaned immediately instead
of being misreported as a readiness timeout.

Passing target acceptance authorizes materialization in that environment; it
does not by itself prove production readiness. Each environment still needs
latency, burst, recovery, isolation, and exact-cleanup acceptance evidence.

Changing backends is a fenced drain operation. Moving from Sandbox allocation
to `native-pod` first deletes every persisted Claim by exact UID and waits for
absence. Moving from native Pods to a Sandbox backend requires all target Pods
to be drained. The reconciler never materializes both backends concurrently.

Cleanup lineage is UID-authoritative. If Kubernetes reuses a recorded Sandbox
or Pod name with a different UID after the old object disappears, the new
object is not treated as the old allocation and is never deleted on its behalf.
Backing-Pod loss recovery is driven by the existing Worker lease-expiry
authority, not by the materializer mutating Execution status. `RecoverExpired`
persists the next Generation dispatch fact; the materializer then requires a
distinct Claim and Pod UID. A later Generation dispatch makes the previous
Pod-bound registration stale and rejected.
