# Workspace Storage Boundary v1

This contract defines the supported storage roles for a Synara Workspace. It applies to Personal, SSH, Docker,
and operator-managed Kubernetes targets. It deliberately does not depend on EKS, GKE, AKS, a cloud CSI driver,
or a cloud-native snapshot service.

## Authority rule

A Worker filesystem is disposable. A local path, container layer, Kubernetes Pod, `emptyDir`, PVC, node disk,
or CSI `VolumeSnapshot` is never a cross-Pod recovery authority. The durable authority is the PostgreSQL
Workspace/Checkpoint metadata plus, when required, a verified `ready` Artifact in the configured Object Store.

An Execution or Recovery Bundle carries only logical IDs and content identities. It must never carry a Worker
path, Pod UID as storage identity, PVC name as live Workspace identity, or infrastructure snapshot handle.

## Supported roles

| Medium               | Supported role                                                                                                   | Authoritative for recovery                                             | Failure boundary                                                                                                                          |
| -------------------- | ---------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------- |
| Ephemeral disk       | Active, mutable Workspace for one Worker generation. Kubernetes uses a size-bounded `emptyDir`.                  | No                                                                     | Pod, container, VM, or host loss may remove it.                                                                                           |
| PVC                  | Optional target-local, rebuildable Git object cache only (`gitCachePersistentVolumeClaim`).                      | No                                                                     | May outlive Pods, but corruption or deletion must be handled as a cache miss. It is not a shared writable Workspace.                      |
| CSI `VolumeSnapshot` | Optional operator backup/acceleration outside the Synara v1 protocol.                                            | No                                                                     | Cluster/driver scoped. Synara cannot select it for recovery until its content is imported and verified as a Ready Artifact.               |
| Object Storage       | Payload bytes for Patch and Workspace Snapshot Checkpoints, with size and SHA-256 verified by the Control Plane. | Yes, after Artifact and Checkpoint are both `ready`                    | Survives Worker/Pod/Target loss according to the operator's Object Store durability and replication policy.                               |
| Git                  | Base commit and branch/reference for reproducible tracked content.                                               | Yes, only for the content actually represented by the Ready Checkpoint | Does not preserve uncommitted tracked changes, untracked files, deleted files, ignored non-cache content, or unavailable private remotes. |

The PVC role is intentionally narrow. A future design that introduces a live Workspace PVC or makes a CSI
snapshot part of RecoveryBundle authority requires a new contract version, tenant isolation model, generation
fence, content verification, retention semantics, and multi-cluster portability proof. Unknown Kubernetes target
fields such as `workspacePersistentVolumeClaim` are rejected rather than silently enabling that mode.

## Checkpoint selection

Agentd chooses the smallest complete durable representation:

1. A clean Git Workspace may use a `git-reference` Checkpoint. It has no Artifact payload and freezes the exact
   repository/base/branch/HEAD metadata.
2. A dirty Git Workspace uses a `patch` Checkpoint. Its Ready Artifact contains the tracked patch, authoritative
   tracked upserts, untracked files, mode metadata, and the rebuildable-cache exclusion policy.
3. A non-Git Workspace uses a `snapshot` Checkpoint backed by a Ready `workspace_snapshot` Artifact.

A Checkpoint is not recoverable while it is `pending` or `uploading`. A failed Checkpoint cannot replace the last
Ready recovery point. The Control Plane binds the Ready Checkpoint to the logical Workspace and freezes its ID on
the next Execution generation. Agentd downloads through a lease- and generation-fenced grant, verifies Artifact
size and SHA-256, validates the strategy-specific manifest, and rejects traversal, symlinks, unexpected members,
or content mismatch before materialization.

## Suspend, resume, and disaster recovery

Before a Pod can be destroyed for resource suspension, the Provider must be quiesced and the Workspace must have
either a newly Ready Checkpoint or a proved `unchanged` relationship to the already Ready Checkpoint. Pod exit or
Lease expiry alone is not storage proof.

Resume and cross-Target recovery consume the frozen Recovery Bundle atomically. Workspace restoration uses the
exact Ready Checkpoint reference; Provider cursor, Memory, context/history, pending Interaction, and Credential
Grant remain separate members of the same bundle and cannot be reconstructed from a PVC or Git checkout. If any
required member is missing or not ready at the destination watermark, recovery fails closed instead of starting
from an empty Workspace.

## Retention and cleanup

- Physical Workspace cleanup is generation-fenced, idempotent, and root-relative.
- Active Leases, pending Artifact uploads, and pending Checkpoints block cleanup.
- Ready Artifacts referenced by a current Workspace, an Execution restore point, a fork, or a Recovery Bundle are
  retained.
- Terminal Execution cleanup may remove ephemeral bytes without deleting the logical Checkpoint lineage.
- A Git cache PVC may be reclaimed independently because it is rebuildable and carries no recovery authority or
  package/Provider credentials.

## Deployment mapping

- Personal uses a local disposable Workspace and the configured Local Artifact Store.
- Single-node and self-hosted Kubernetes use disposable Worker storage plus MinIO or another S3-compatible Object
  Store for durable Checkpoint payloads.
- Multi-cluster self-hosted Kubernetes requires destination Artifact/Checkpoint readiness and replication
  watermark authority before failover. A PVC from the source cluster never satisfies that gate.

The implementation is guarded by the Kubernetes Pod-spec tests (Workspace remains `emptyDir` even when the Git
cache uses a PVC), strict target-configuration decoding, Checkpoint lifecycle tests, Artifact Ready verification,
restore-manifest validation, and Workspace cleanup/retention tests.
