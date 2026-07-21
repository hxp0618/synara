# Execution Target v1 contract

Execution Target identifies where an Agent execution is eligible to run. It is orthogonal to the
control plane's Deployment Profile and to workspace mode.

## Kinds

| Kind         | v1 contract                                                     | Current-phase boundary                                           |
| ------------ | --------------------------------------------------------------- | ---------------------------------------------------------------- |
| `local`      | Worker runs on the control-plane/local host                     | `synara-agentd` runner and automatic supervision are implemented |
| `ssh`        | `synara-agentd` registers from a remote host                    | managed install/upgrade/revoke is implemented                    |
| `docker`     | registered workers execute in a container pool                  | managed pool reconciler is implemented                           |
| `kubernetes` | one execution-pinned Worker Pod per queued/recovering execution | scheduler/reconciler and security foundation are implemented     |

All kinds use `RegisterWorker`, `Heartbeat`, `ClaimExecution`, leases, generation fencing, runtime
events, idempotent receipts, and provider resume cursors. Drivers must not write Session state directly.

Stage 3 adds managed Worker Release and target-scoped image-pull Credential contracts for Docker and
Kubernetes. The persistence/API/reconciler baseline exists, but the real Codex/Claude four-Target release gate,
registry-pushed multi-arch reproduction, production multi-node Kubernetes and soak remain open. An implemented
reconciler is not by itself production release evidence.

## Persistence and ownership

`execution_targets` stores:

```text
id
tenant_id nullable
organization_id nullable
kind
name
status
configuration_encrypted
capabilities
created_at
updated_at
```

Both ownership fields null means a platform target. Otherwise `tenant_id` is required, and a non-null
organization must belong to that tenant. A target's ownership and kind are immutable.

Sessions persist a non-null `execution_target_id`. Executions copy both `execution_target_id` and
`target_kind` when a Turn is queued. `local`/`worktree` workspace mode is not encoded in target kind.

## HTTP API and safe fields

```text
GET  /v1/tenants/{tenantId}/execution-targets
POST /v1/tenants/{tenantId}/execution-targets
GET  /v1/tenants/{tenantId}/execution-targets/{executionTargetId}
GET  /v1/tenants/{tenantId}/workers
POST /v1/tenants/{tenantId}/workers/{workerId}/revoke
GET  /v1/tenants/{tenantId}/execution-targets/{executionTargetId}/worker-releases
POST /v1/tenants/{tenantId}/execution-targets/{executionTargetId}/worker-releases
POST /v1/tenants/{tenantId}/execution-targets/{executionTargetId}/worker-releases/{revisionId}/canary
POST /v1/tenants/{tenantId}/execution-targets/{executionTargetId}/worker-releases/{revisionId}/promote
POST /v1/tenants/{tenantId}/execution-targets/{executionTargetId}/worker-releases/{revisionId}/rollback
GET  /v1/tenants/{tenantId}/credential-bindings?executionTargetId={executionTargetId}
POST /v1/tenants/{tenantId}/credential-bindings
POST /v1/tenants/{tenantId}/credential-bindings/{bindingId}/disable
```

Responses contain only `id`, ownership, `kind`, `name`, `status`, safe `capabilities`, and timestamps.
`configuration_encrypted` is never serialized. Create requests accept a `configuration` object, which
is encrypted before persistence; plaintext must not enter logs, runtime events, audit metadata, or
outbox payloads.

Tenant `worker.read` authorizes list/read and `worker.manage` authorizes create. Organization ownership
is separately validated against the target tenant.

Worker Release mutations and operator revocation also require `worker.manage` and an `Idempotency-Key`.
Credential Binding management requires `credentials.manage`. Responses expose release/Worker identifiers,
versions, channels, status and safe reasons; they never expose encrypted Target configuration, registry auth,
Worker/Lease tokens or Credential plaintext.

## Managed Worker Release Revision and Policy

Migration `000037_worker_release_rollout.sql` adds three target-owned resources:

| Resource                     | Contract                                                                         |
| ---------------------------- | -------------------------------------------------------------------------------- |
| `worker_release_revisions`   | immutable monotonic Revision binding one Target to one immutable Worker Manifest |
| `worker_release_policies`    | one CAS row per Target with one promoted and optional canary Revision            |
| `worker_release_transitions` | immutable policy history keyed by Target and Policy Version                      |

Managed Docker/Kubernetes Revision creation and every Policy transition require the referenced Worker Manifest to
contain an immutable image Digest. A mutable tag or Target configuration image alone is not sufficient once a Policy
exists. The reconciler joins the Revision to the Manifest and uses that Digest as the desired image.

Policy rules:

- Initial `promote` uses `expectedPolicyVersion = 0`.
- A canary Revision must be newer than promoted, different from promoted and use `canaryPercent` in `1..100`.
- Only the current active canary can be promoted.
- Rollback to an older Revision changes promoted and removes canary.
- Calling the rollback endpoint with the current promoted Revision while a canary exists is the explicit
  abort-canary operation.
- Policy Version advances exactly once. Stale CAS must fail; clients reload and re-evaluate intent rather than
  replacing only the Version and replaying an obsolete operation.

The persisted Transition row action remains `rollback` for abort-canary because `000037` freezes the database action
vocabulary as `promote | canary | rollback`. `GET .../worker-releases` projects that exact rollback shape as
`abort-canary`; Audit uses `worker_release.canary_aborted`, and Outbox uses
`worker.release.canary-aborted`. Consumers must not infer abort solely from the stored action string.

Execution selection is deterministic. When a Policy exists, a queued/recovering Execution receives promoted or
canary Revision/Channel before Lease acquisition. Claim requires an active Worker with the same Target, Revision and
Channel. A Policy transition may reassign only unleased Executions; it must not silently mutate a leased/running
Execution's release identity.

Workers whose Manifest is no longer selected become release-inactive and cannot claim new work. Policy transitions
that would retire a Revision/Channel are rejected while it owns `leased`, `running` or `waiting-for-approval`
Executions. Reconciliation may prepare replacements, but an active Lease remains a drain boundary: Docker retains
busy stale containers until the Lease clears, and Kubernetes preserves the execution-pinned Pod until its Execution
reaches a replaceable/terminal state. Operators must not delete a busy Worker merely to make the desired release count
look converged.

## Target-scoped Worker image-pull Credential

Migration `000035_workspace_credential_bindings.sql` adds `worker_image_pull` as an Execution Target-scoped
Credential Binding. Migration `000038_credential_binding_fk_indexes.sql` keeps disabled history usable for foreign-key
enforcement without making it active again. Migration `000039_worker_image_pull_binding_uniqueness.sql` enforces at
most one active image-pull Binding per Target. Migration `000040_worker_release_transition_policy_fencing.sql` keeps
the latest immutable Transition exactly aligned with the current Release Policy.

Provisioning requires exactly one active `worker_image_pull` Binding per Target. Multiple active candidates are
ambiguous and must fail closed rather than relying on database row order. The Binding must reference a Tenant- or
matching Organization-scoped `purpose=registry`, `provider=oci` Credential. The server derives an immutable selector
from the encrypted payload and requires it to match the image Registry authority exactly, with Docker Hub aliases
normalized. Binding plaintext, registry username/password/token and the resolved auth header are never returned by
the API.

Resolution rules:

- Target ownership, Binding state, Credential scope/version/expiry/revocation and selector are checked under database
  locks before KMS I/O.
- KMS decryption occurs outside the transaction, followed by a second snapshot check so Rotation/Disable/Revoke cannot
  race a stale pull credential into use.
- Docker supports OCI basic or registry token auth and sends it only as the Engine pull request's `X-Registry-Auth`.
- Kubernetes currently accepts OCI basic auth for `kubernetes.io/dockerconfigjson`; bearer-only auth is Explicit
  Unsupported for this Target.
- Registry authorities preserve custom ports during image parsing, but the current Registry Credential payload accepts
  hostname-only selectors. Custom-port authenticated pulls therefore fail closed until that contract is extended.
- The resolved registry Credential is a short-lived Control Plane provisioner projection. It never enters an agentd
  Workload, Worker/Provider environment, command argument, Session Event, Audit/Outbox metadata or log.

The resolver distinguishes authoritative absence/disable/revoke/invalid scope from a transient database/KMS failure.
Kubernetes may clear the managed Target registry Secret only for an authoritative result; a transient lookup/KMS
failure marks the Target offline but must not erase the last known Secret. An authoritative clear applies an empty
auth map. Existing node image cache is a separate platform concern and is not erased by Credential revocation.

## Worker binding and claim rules

Worker registration and claim requests use `executionTargetId` and `targetKind`. A worker is permanently
bound to that target for its registration lifetime. Claims match both fields; pool names such as
`shared_pool` and `dedicated_pool` are not v1 domain concepts.

Remote workers must advertise:

```json
{
  "leaseSupported": true,
  "fencingSupported": true
}
```

Registration or claiming is rejected when a remote worker lacks either capability or when target ID
and kind do not match persisted state.

## Managed SSH lifecycle

Owner/Admin operators can invoke:

```text
POST /v1/tenants/{tenantId}/execution-targets/{targetId}/ssh/install
POST /v1/tenants/{tenantId}/execution-targets/{targetId}/ssh/upgrade
POST /v1/tenants/{tenantId}/execution-targets/{targetId}/ssh/revoke
```

The encrypted SSH configuration requires `host`, `user`, `privateKey`, pinned OpenSSH `hostKey`,
`controlPlaneUrl` (or the server-wide public URL), and `runnerCommand`. Optional fields include
`port`, `privateKeyPassphrase`, `workspaceRoot`, `gitCacheRoot`, `installRoot`, `serviceUser`, and `useSudo`.
Plain HTTP control-plane URLs are rejected unless `allowInsecureControlPlane` is explicitly true.

Provisioning uploads `synara-agentd`, a root-readable EnvironmentFile, and a target-specific systemd
unit through the verified SSH connection. It never places SSH keys or Worker registration tokens in
remote command arguments, browser responses, logs, or Audit metadata. Install/upgrade temporarily mark
the target offline and activate it only after systemd restart succeeds. Revoke stops and disables the
unit, removes binary/configuration files, preserves the workspace, and marks the target disabled.
The default roots are `/var/lib/synara/targets/<target>/workspaces` and
`/var/lib/synara/targets/<target>/git-cache`. Provisioning creates and assigns both roots to the service user;
they must be separate absolute paths.

## Managed Docker Worker Pool

The control plane reconciles every non-disabled Docker target through the Docker Engine Unix socket.
Encrypted configuration supports `image`, `runnerCommand`, `desiredWorkers`, `pullPolicy`,
`controlPlaneUrl`, `workspaceVolume`, `workspaceMount`, `workspaceRoot`, `gitCacheRoot`, `networkMode`, `user`,
`memoryBytes`, and `nanoCpus`. HTTP control-plane URLs require explicit
`allowInsecureControlPlane=true`.

Managed containers use deterministic names and labels for Target, Tenant, Organization, Worker index,
configuration digest, Worker Release Revision and release Channel. They override the image entrypoint with
`/usr/local/bin/synara-agentd`, mount one persistent target workspace volume, use `unless-stopped`, and
receive CPU/memory limits from the encrypted target configuration. Registration secrets exist only in
the container environment and never in labels, responses, logs, or Audit metadata.
`workspaceRoot` defaults to `/data/workspaces` and `gitCacheRoot` defaults to `/data/git-cache`; both live on the
same target-scoped named volume but remain separate trees. Multiple Workers for one Docker Target therefore share
the cache and coordinate it with filesystem locks while retaining private Workspace repositories.

Reconciliation is idempotent: stable pools produce no writes or Audit rows. Configuration changes use
the digest to replace stale containers. Scale-down and replacement skip Workers with an unexpired
Lease; those containers are removed on a later pass after the Lease clears. A running deferred container remains
part of the desired capacity and is first matched to an unoccupied slot with the same Release Revision/Channel, so
a Busy promoted Worker does not consume a canary slot or make a healthy Target appear offline. A fully running
desired pool, including safely deferred Busy containers, marks the target active; partial or failed reconciliation
marks it offline.

Without a Release Policy the pool remains unmanaged and uses the encrypted Target `image`. With a Policy, each slot
uses the promoted or canary Manifest Digest. Docker canary requires `desiredWorkers >= 2`; percentage rounding must
leave at least one promoted and one canary slot. Registry auth is used for image pull only and is not written into
container Environment or labels.

## Managed Kubernetes execution

The control plane reconciles every non-disabled Kubernetes target under a PostgreSQL advisory lock.
Encrypted configuration contains the API server authentication material, Namespace policy, Worker
image and runner command, control-plane URL, resource requests/limits, quota ceilings, explicit egress
CIDRs, and optional scheduling constraints. External cluster credentials must be stored as encrypted
inline values; file references are restricted to the standard in-cluster ServiceAccount paths.

For each target the reconciler server-side-applies the Namespace when requested, a tokenless Worker
ServiceAccount, registration Secret, target-scoped registry Secret, ResourceQuota, and default-deny NetworkPolicy.
The registry Secret is referenced only through Pod `imagePullSecrets` and is not mounted or exposed to agentd. It
creates one
execution-pinned Pod for each queued or recovering Execution up to `maxActivePods`. Pod names and
labels encode the expected next Generation plus selected Release Revision/Channel. A release-pinned Execution uses
the exact immutable Manifest Digest instead of the mutable Target image. Terminal, stale-generation, and no-longer-owned Pods are
deleted, while PostgreSQL remains the authority for Session, Event, Lease, and recovery state.

Worker Pods run as UID/GID 10001 with a read-only root filesystem, RuntimeDefault seccomp, no Linux
capabilities, no privilege escalation, no ServiceAccount token, bounded EmptyDir workspace/tmp/home,
and explicit CPU/memory/ephemeral-storage limits. By default `/data/workspaces` and `/data/git-cache` are
separate trees on the Pod-local workspace EmptyDir. Optional `gitCachePersistentVolumeClaim` mounts a dedicated
cache at `/git-cache`; cross-Pod sharing is valid only when the claim supplies RWX-equivalent access and reliable
POSIX file locking. Optional `requireNodeSpread=true` adds one Pod `topologySpreadConstraints` rule with
`maxSkew=1`, `topologyKey=kubernetes.io/hostname`, `whenUnsatisfiable=DoNotSchedule`, and
`labelSelector.matchLabels["synara.io/execution-target-id"]=<target UUID>`. The reconciler deliberately does not
emit required `podAntiAffinity`, so once each eligible hostname already has one Pod, additional concurrency may
still schedule on occupied nodes while keeping per-host skew within 1. The balancing guarantee applies only when
the scheduler sees at least two eligible `kubernetes.io/hostname` domains after `nodeSelector`, `tolerations`,
and cluster policy are applied; the default `requireNodeSpread=false` emits no spread constraint. The registration token is referenced from a Secret;
it is not embedded in Pod labels, Audit metadata, responses, or runner arguments.

The in-cluster control-plane RBAC and Kustomize base live in `deploy/kubernetes`. The reconciler role is
cluster-scoped only because managed targets may create dedicated Namespaces; operators that disable
Namespace management may replace it with equivalent per-Namespace Roles.

## Release and acceptance boundary

The operator workflow is documented in `docs/runbooks/worker-release-rollout.md`; the required release evidence is in
`docs/release-checklists/stage-3-provider-runtime-remote-worker.md`. Current implementation evidence is summarized in
`docs/reports/stage-3-provider-runtime-acceptance-2026-07-15.md`; the latest deterministic managed Docker immutable
rollout and Busy Worker fencing evidence is in `docs/reports/stage-3-worker-release-rollout-d3af9380.md`.

Current deterministic Local/Docker/Kubernetes and historical SSH/Kubernetes fixture evidence proves selected shared
orchestration and failure paths, not a real Provider release. Stage 3 remains `partial` until the same committed,
registry-pushed immutable image passes real Codex and Claude acceptance across Local, SSH, Docker and Kubernetes,
including canary, rollback, active-execution drain, credential revocation and long-session/production soak.
