# sandbox-operator target assets

These assets bootstrap the externally managed `SandboxTemplate` and
`SandboxWarmPool` required by a Synara Kubernetes target whose
`allocationBackend` is `sandbox-operator-standard` or
`sandbox-operator-cocoon`.

Copy `standard.example.yaml`, replace every `REPLACE_*` value, pin the worker
image by digest, and apply it to the same cluster and namespace as the target.
The target configuration then references `synara-worker` and
`synara-worker-interactive`, and must include the selected Target tenant UUID in
`sandboxAllowedTenantIds`. Copying the configuration to another tenant fails
closed until that tenant is deliberately selected.

For Cocoon, start from `cocoon.example.yaml` and pin the first `agent`
container to the immutable guest image. The container name is part of
vk-cocoon's exec/logs contract. Keep
`cocoonset.cocoonstack.io/snapshot-policy: never` on these ephemeral pool
members: Cocoon's empty-policy default snapshots on delete, and a real
two-host loss run blocked cleanup while uploading an approximately 1 GiB
snapshot. Durable workload checkpointing needs a separate template and
lifecycle contract. Target acceptance fails closed when this cleanup policy is
missing. Keep `vm.cocoonstack.io/shared-memory: "true"` on the template because
the host-supervised virtiofs workspace must be fixed before the guest boots.
The pinned, reproducible vk-cocoon patch set that implements this annotation
and fences intentional VM deletion is documented under
`deploy/kubernetes/vk-cocoon/`. It also requires the template to
select only virtual nodes carrying the complete KVM/supervisor/vsock/isolation
labels and to tolerate only the exact Cocoon virtual-kubelet taint. The two
physical-host acceptance uses an explicit node-loss hook and fenced Generation
recovery; template `NoExecute` tolerations are not accepted as recovery proof.
In the two-host runtime checkpoint, a 20-second toleration marked the backing
Pod for deletion but left the Sandbox terminating and did not replenish it on
the other host.

Keep the pool update strategy at `Recreate`. Synara reapplies that strategy and
will not create new Claims while any pool-owned idle Sandbox still carries the
previous template image or while replacement capacity is not Ready.

The worker starts immediately but waits for the projected
`assigned-execution-id` downward-API file before registering. This is required
for pre-warmed Pods: environment-variable field references are evaluated only
at Pod startup and cannot carry a later Claim assignment.
`SYNARA_AGENTD_SANDBOX_ALLOCATION_BIND_TIMEOUT` bounds the subsequent retry of
only `kubernetes_sandbox_allocation_not_bound`. Keep it at least as large as the
Target's `sandboxClaimReadyTimeoutSeconds`; the example/default pair is 30
seconds, while HA acceptance deliberately uses 180 seconds. Other registration
errors remain terminal.

The control plane owns pool replicas after the backend is enabled. Do not attach
an HPA or another field manager to `spec.replicas`. To roll back, drain/cancel
Sandbox executions while the Sandbox backend is still configured, wait for all
allocations to become `deleted`, then change the target to `native-pod`.

Worker Release selection is also fail-closed. The template's `agentd` image and
the eventual backing Pod image must exactly match the immutable image selected
for the Execution. Update the external template; the `Recreate` pool rollout
drains/replenishes idle Sandboxes and Synara waits for it before allocating the
promoted release. The current single-pool configuration
does not support simultaneous promoted/canary images; canary-selected
Executions are rejected until separate per-release pools are configured.

The sandbox-operator controller must mount its `sandbox-config` ConfigMap with
`allowed-label-domains: synara.io`. Without this allowlist, Claims are rejected
as `InvalidMetadata` before assignment. The Stage B runner creates this config
and restarts the disposable controller before testing.

This example is an integration input, not production authorization. Run the
Stage B isolation, restart, deletion, quota and Provider-ready acceptance suite
before canary use.

Synara also contains the gated integration test
`TestSandboxOperatorRealControlPlaneRegistrationAndGenerationFencing`. It
requires explicit Kubernetes API/token/CA/image/context environment values and
uses a disposable namespace. The test proves TokenReview, allocation binding,
Worker persistence, a formal Execution claim with a persisted immutable
Recovery Bundle, validation through the same claim guard used by agentd,
the real agentd/Protocol-v2 fixture lifecycle through completion,
stale-Generation rejection, and exact lineage cleanup. It also preserves the
startup race regression where agentd may briefly observe an allocation that is
not yet persisted as bound. The live test additionally checks projected-token
mount isolation, NetworkPolicy data-plane denial from an untrusted Pod, and
ResourceQuota admission at the configured Pod limit. It also exercises the
generation-bound Provider Credential Grant, lease/tenant-fenced resolution,
anonymous-FD delivery to the Protocol-v2 fixture, cross-tenant rejection, and
secret non-reflection in runtime/completion payloads. It is skipped during
ordinary unit-test runs.

The same run blocks Generation 1 before Provider start, deletes its backing
Pod, waits for the real Worker lease to stop renewing, and invokes the normal
`RecoverExpired` authority. That path must persist a clean `execution-recovery`
Generation 2 and complete it through Provider Host on a replacement Claim/Pod
with new UIDs. A Generation-3 dispatch probe then requires the still-live
Generation-2 Pod registration to be rejected as stale. Cleanup compares the
persisted UIDs, so a Kubernetes object that
later reuses an old name with a different UID is not mistaken for the previous
allocation lineage.

The same gated run also stops the sandbox-operator controller, persists two
concurrent Synara Generation Claims while it is absent, then restores it at two
replicas. After recovery it observes the leader-election Lease, deletes the
active leader, creates another Execution during failover, and requires the
standby leader to complete all three token-routed Workers with unique Claim/Pod
UIDs and exact cleanup. This covers bounded restart and multi-replica failover;
production-duration chaos and sustained-load evidence remain separate gates.

For KVM Stage C, set
`SYNARA_TEST_KUBERNETES_ALLOCATION_BACKEND=sandbox-operator-cocoon`; the live
test then requires a `vk-cocoon` template and the existing positive virtual-node
and KVM acceptance observations. An explicitly configured absolute executable
`SYNARA_TEST_KUBERNETES_NODE_LOSS_HOOK` changes the loss mode from `pod-delete`
to `node-hook`: it must make the selected node NotReady and later restore it.
The replacement Pod must bind on a different Ready node. The ordinary Stage B
default never executes an external hook.

## Repeated control-plane soak

`soak-acceptance.sh` repeats the complete gated control-plane test until both a
minimum wall-clock duration and minimum run count are satisfied. Every iteration
uses real agentd/Provider completion, controller restart, active-leader deletion,
three concurrent Executions, backing-Pod loss and replacement-Generation
recovery, credential non-reflection checks, and exact cleanup. It fails instead
of silently shortening the run when `MAX_RUNS` is
reached first. The runner also enforces Provider-ready p95 (2.5 seconds by
default) and maximum leader failover (30 seconds by default).

Supply the same five non-optional Kubernetes test environment values described
above, then run:

```bash
SYNARA_SANDBOX_OPERATOR_SOAK_MIN_SECONDS=300 \
SYNARA_SANDBOX_OPERATOR_SOAK_MIN_RUNS=3 \
SYNARA_SANDBOX_OPERATOR_SOAK_MAX_RUNS=100 \
./deploy/kubernetes/sandbox-operator/soak-acceptance.sh
```

Optional threshold variables are
`SYNARA_SANDBOX_OPERATOR_SOAK_MAX_PROVIDER_READY_P95_MS` and
`SYNARA_SANDBOX_OPERATOR_SOAK_MAX_FAILOVER_SECONDS`. Controller logs are
followed during each fault window so deleting the leader does not discard its
conflict evidence; `SYNARA_SANDBOX_OPERATOR_SOAK_MAX_CONTROLLER_CONFLICT_LINES`
defaults to 20. The test also samples the Synara-projected pool during recovery,
requires the final deficit to return to zero, and bounds the transient deficit
with `SYNARA_SANDBOX_OPERATOR_SOAK_MAX_WARM_POOL_DEFICIT` (default 1). Evidence is written under
`.tmp/sandbox-operator-soak/<run-id>/` as per-iteration logs, `journal.jsonl`,
and `summary.json`. Each iteration also scans the test/operator logs for the
known test bearer and Provider credential plus JWT and credential key/value
shapes; any match fails the run without printing the matched value. A short
local smoke proves the runner, not a production
duration; release evidence must use the approved duration, load, cluster and
alerting window.
`SYNARA_SANDBOX_OPERATOR_SOAK_MIN_WARM_HIT_RATE_PERCENT` defaults to 95; each
iteration prewarms Synara-authoritative idle capacity and proves the first
measured allocation was actually labelled `warm` before contributing latency.
`SYNARA_SANDBOX_OPERATOR_SOAK_REQUIRED_NODE_LOSS_MODE` defaults to
`pod-delete`; Stage C sets it to `node-hook`, which additionally requires the
old and replacement node names to differ in every iteration.

The current disposable-Kind baseline ran three complete iterations over
377.461 seconds: 12 Executions and 12 Provider Generations completed across 18
observed Generation transitions, warm-hit rate was 100%, Provider-ready p95 was
1,842.156 ms, lease-expiry backing-Pod-loss recovery max was 3.200 seconds,
leader failover max was 25.050 seconds, maximum conflict lines per iteration
was 12 (limit 20), maximum warm deficit was 1 and recovered to zero, and the
secret scan found zero matches. This is local multi-run evidence, not the
approved production-duration canary.
