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
```

Responses contain only `id`, ownership, `kind`, `name`, `status`, safe `capabilities`, and timestamps.
`configuration_encrypted` is never serialized. Create requests accept a `configuration` object, which
is encrypted before persistence; plaintext must not enter logs, runtime events, audit metadata, or
outbox payloads.

Tenant `worker.read` authorizes list/read and `worker.manage` authorizes create. Organization ownership
is separately validated against the target tenant.

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
and a configuration digest. They override the image entrypoint with
`/usr/local/bin/synara-agentd`, mount one persistent target workspace volume, use `unless-stopped`, and
receive CPU/memory limits from the encrypted target configuration. Registration secrets exist only in
the container environment and never in labels, responses, logs, or Audit metadata.
`workspaceRoot` defaults to `/data/workspaces` and `gitCacheRoot` defaults to `/data/git-cache`; both live on the
same target-scoped named volume but remain separate trees. Multiple Workers for one Docker Target therefore share
the cache and coordinate it with filesystem locks while retaining private Workspace repositories.

Reconciliation is idempotent: stable pools produce no writes or Audit rows. Configuration changes use
the digest to replace stale containers. Scale-down and replacement skip Workers with an unexpired
Lease; those containers are removed on a later pass after the Lease clears. A fully running desired
pool marks the target active; partial or failed reconciliation marks it offline.

## Managed Kubernetes execution

The control plane reconciles every non-disabled Kubernetes target under a PostgreSQL advisory lock.
Encrypted configuration contains the API server authentication material, Namespace policy, Worker
image and runner command, control-plane URL, resource requests/limits, quota ceilings, explicit egress
CIDRs, and optional scheduling constraints. External cluster credentials must be stored as encrypted
inline values; file references are restricted to the standard in-cluster ServiceAccount paths.

For each target the reconciler server-side-applies the Namespace when requested, a tokenless Worker
ServiceAccount, registration Secret, ResourceQuota, and default-deny NetworkPolicy. It creates one
execution-pinned Pod for each queued or recovering Execution up to `maxActivePods`. Pod names and
labels encode the expected next Generation. Terminal, stale-generation, and no-longer-owned Pods are
deleted, while PostgreSQL remains the authority for Session, Event, Lease, and recovery state.

Worker Pods run as UID/GID 10001 with a read-only root filesystem, RuntimeDefault seccomp, no Linux
capabilities, no privilege escalation, no ServiceAccount token, bounded EmptyDir workspace/tmp/home,
and explicit CPU/memory/ephemeral-storage limits. By default `/data/workspaces` and `/data/git-cache` are
separate trees on the Pod-local workspace EmptyDir. Optional `gitCachePersistentVolumeClaim` mounts a dedicated
cache at `/git-cache`; cross-Pod sharing is valid only when the claim supplies RWX-equivalent access and reliable
POSIX file locking. The registration token is referenced from a Secret;
it is not embedded in Pod labels, Audit metadata, responses, or runner arguments.

The in-cluster control-plane RBAC and Kustomize base live in `deploy/kubernetes`. The reconciler role is
cluster-scoped only because managed targets may create dedicated Namespaces; operators that disable
Namespace management may replace it with equivalent per-Namespace Roles.
