# Stage 3 Drift Audit

Baseline: `codex/saas-tenancy-user` after Stage 2 repository-controlled acceptance and the
post-`702cb0d0` browser route verification on 2026-07-13.

This audit treats executable code, migrations and repeatable tests as evidence. A local Provider Adapter is not
evidence that the Provider is supported by a remote Worker.

## Release boundary

| Provider | Existing local runtime | Existing remote host | Stage 3 remote release boundary |
| --- | --- | --- | --- |
| Codex | App Server adapter with Send, Steer, Review, Interrupt, Approval/Input, Compact, Rollback and Fork paths | One-shot `codex exec --json`, native Cursor and bounded history reconstruction | Tier 1 target |
| Claude | Agent SDK adapter with multi-turn, Interrupt, Approval/Input, history and discovery paths | One-shot `claude --print --output-format stream-json`, native Session ID and bounded history reconstruction | Tier 1 target |
| Cursor | ACP adapter with Send, Interrupt, Approval/Input, history and Rollback | None | Local-only until the shared remote suite passes |
| Gemini | ACP adapter with Send, Interrupt, Approval/Input, history and Rollback | None | Local-only until the shared remote suite passes |
| Grok | ACP adapter with Send, Interrupt, history, Rollback and Compact | None | Local-only until the shared remote suite passes |
| Kilo | OpenCode-compatible adapter with Send, Interrupt, Approval/Input, history, Compact and Fork | None | Local-only until the shared remote suite passes |
| OpenCode | SDK adapter with Send, Interrupt, Approval/Input, history, Compact and Fork | None | Local-only until the shared remote suite passes |
| Pi | SDK adapter with Send, Steer, Interrupt, User Input, history, Rollback and Compact | None | Local-only until the shared remote suite passes |

`Local-only` is an explicit support conclusion, not a silent fallback. Remote scheduling must reject it with a
stable capability error before an Execution is claimed. Promotion to Tier 2 or Tier 1 requires the same Provider
Acceptance Fixture used by Codex and Claude.

## Workflow audit

| Workflow | Status | Current evidence and required delta |
| --- | --- | --- |
| A. Capability matrix and support levels | partial | The shared Contract freezes Capability IDs and Host descriptors for all eight Providers. Agentd probes all eight; Control Plane stores immutable Provider manifests and rejects incompatible assigned Claims. Web capability projection and per-capability Acceptance evidence remain. |
| B. Provider Host Protocol v2 | partial | Shared/Host v2 contracts, Describe, Command/Message envelopes, stable errors and terminal replay exist. Agentd defaults to v2, publishes startup descriptors, persists/gates the actual Host/Provider manifest and multiplexes concurrent Send/Interaction/Interrupt command terminals. Compact/Rollback/Fork/Review commands and broader Acceptance evidence remain. |
| C. Unified Session/Turn semantics | partial | Start/Resume/Send use correlated v2 Command IDs with in-process terminal replay and persisted Runtime Binding identity. Codex App Server and Claude Agent SDK pass native Cursor resume, authoritative-history fallback, native Turn interrupt and native Steer. Migration `000022` provides the reusable Generation-fenced Control Command channel; Web Steer/Interrupt intent, Worker delivery and acknowledgement projections are implemented. PostgreSQL continuity now proves Interrupt Cursor persistence, a distinct later Turn on a replacement Worker, authoritative user/assistant history reconstruction and uninterrupted Session Sequence allocation. Compact, Rollback, Fork, Review and broader real-target restart evidence remain. |
| D. Approval and Structured User Input | partial | Migration `000019`, Worker APIs and agentd implement Generation-fenced pull/delivered/acknowledged transitions and obsolete-Generation superseding. Migration `000021` persists immutable Turn runtime/interaction modes through Web, Session Event and Worker Workload. Real Codex and Claude Approval plus Plan Mode User Input pass Host round trips; refresh/restart Web acceptance remains. Claude uses a host-owned PreToolUse hook so local permission allow rules cannot bypass the durable decision. |
| E. Runtime Event compatibility | partial | `packages/contracts` has a rich canonical `ProviderRuntimeEventV2`; remote Host emits a separate small `runtime.*` vocabulary and Control Plane accepts only Event Version 1. Add an audited compatibility bridge and unknown-event policy instead of creating a third vocabulary. |
| F. Authoritative history and Worker migration | partial | Migration `000018` and Session/Execution code create versioned Runtime Bindings. Native Cursor reuse is gated by the persisted Capability Descriptor Hash and otherwise falls back to authoritative history. Tool/Artifact/Plan/Interaction/Checkpoint reconstruction and explicit fallback outcome Events remain. |
| G. Credential isolation | partial | Codex/Claude Provider credentials use FD 3, strict allowlists and redaction; Worker/Lease variables are removed. Migration `000023` adds purpose-isolated Git Credentials, legacy Provider AAD compatibility, immutable scope/type, Project-only binding and a separate Generation-fenced Worker resolver. Agentd keeps Git username/token in an ephemeral Unix-socket AskPass channel used only for Clone/Fetch, clears it before Provider start and rejects unsafe local Git configuration. Web create/rotate and Project bind/unbind paths filter Provider versus Git purpose and Organization scope. Remaining Provider allowlists, SSH/registry/package delivery and the full leakage suite remain. |
| H. Remote Workspace/Git lifecycle | partial | Migration `000020` creates one logical Workspace per Session and binds it to Target/Execution without persisting Worker paths or Credentials. Agentd uses the logical Workspace ID for a stable Session checkout, renews the Lease during preparation, validates HTTPS Repository URLs and branches, rejects embedded Credentials and local/private/link-local targets, DNS-pins Clone/Fetch with redirects disabled, creates an isolated Session branch, and reports `workspace.ready/failed` through Generation-fenced idempotent Worker APIs before Provider start. Public and Project-bound private HTTPS Clone/Fetch are implemented with exact-host Credential enforcement. After Provider execution, agentd revalidates Git config, reports `workspace.dirty`, then creates a Git-reference or bounded regular-file Snapshot Checkpoint. A replacement Worker downloads and verifies the frozen ready Snapshot before Provider start. Checkpoint/Artifact retention protects current and active restore references, fails abandoned/expired uploads and preserves the last ready recovery point. SSH delivery, shared cache plus `git worktree`, Patch Checkpoints and physical Workspace cleanup dispatch/acknowledgement remain. |
| I. Terminal/log/generated file/checkpoint | partial | Artifact path containment, server-side size/hash verification and retry-safe Checkpoint Artifact identity are implemented. Migrations `000020`, `000024` and `000025` enforce Checkpoint scope, ready-only recovery pointers, immutable terminal metadata, reverse Artifact binding and deletion protection. Agentd automatically creates Git-reference/Snapshot Checkpoints; ambiguous Create/PUT/Complete responses reuse one Artifact, and retention fails expired uploads without losing the previous recovery point. Terminal lifecycle, rolling logs, general generated-file retry identity and Patch payloads remain. |
| J. Worker drain/upgrade/version isolation | partial | Migration `000017` stores immutable Worker/Image/Provider manifests; registration deduplicates by Hash, records all eight Provider conclusions, and Claim binds/rejects by compatibility. Agentd advertises a tested Worker Runtime manifest and validates the Git SHA with the persisted lowercase-hex constraint. SIGTERM now sends an immediate Draining Heartbeat, stops Claim, keeps Heartbeat/Lease renewal through a bounded deadline, completes safe work or cancels and releases the Lease; stale Draining Workers become Offline. Managed SSH/Docker/Kubernetes carry a 20-second deadline inside their 30-second stop window, and embedded Local drain finishes before HTTP shutdown. Complete image digest/SBOM inputs, canary/rollback and incompatible/revoked operations remain. |
| K. Web authority switch | implemented Stage 2 baseline | SaaS Project/Session/Turn/Event is Control Plane authoritative and local mode remains isolated. Stage 3 must add Capability/Interaction/Artifact/Recovering projection without restoring dual authority. |
| L. Unified acceptance suite | missing | Stage 2 has target-specific smoke/failure scripts, but no `Provider × Capability × Target` fixture, machine-readable report or long-session/failure matrix. |

## Frozen version boundary

- Worker Protocol remains independently versioned; the current deployed version is `1`.
- Provider Host Protocol starts at `{ major: 2, minor: 0 }`.
- Canonical local Runtime Event remains `ProviderRuntimeEventV2`; Control Plane keeps legacy Event Version 1 during
  migration and gains an explicit canonical version rather than reinterpreting version 1 payloads.
- Major Provider Host mismatch is non-schedulable. Unknown optional Minor fields are ignored. Unknown commands are
  rejected. Missing capabilities are `unsupported`, never guessed.
- The v1 one-shot runner remains only as a bounded compatibility path while every managed Target is upgraded. It
  cannot advertise v2-only capabilities.

## Frozen stable errors

The v2 contract must use these codes and attach retry/user-action/recovery metadata:

```text
provider_not_installed
provider_version_incompatible
capability_unsupported
credential_missing
credential_invalid
authentication_required
session_resume_invalid
session_resume_expired
provider_rate_limited
provider_unavailable
workspace_invalid
protocol_violation
cancelled
interrupted
internal_error
```

## Database and DDL status

The checked-in forward-only migrations now continue through:

1. `000017_worker_provider_manifests.sql`: immutable Worker/Image/Provider manifests and compatibility binding.
2. `000018_provider_runtime_bindings.sql`: Session Runtime Binding and Cursor compatibility metadata.
3. `000019_interaction_delivery.sql`: Interaction expiry, resolution command, delivery and acknowledgement state.
4. `000020_remote_workspaces_checkpoints.sql`: logical Workspace lifecycle and Artifact-backed Checkpoints.
5. `000021_turn_runtime_modes.sql`: immutable runtime and interaction modes carried from Web through Workload.
6. `000022_execution_control_commands.sql`: durable Generation-fenced Provider Control Commands and terminal Interrupt acknowledgement.
7. `000023_git_credentials.sql`: purpose-isolated Git Credentials, Project binding and Worker resolution enforcement.
8. `000024_checkpoint_lifecycle.sql`: Generation-fenced Checkpoint lifecycle, ready recovery pointers and immutable payload constraints.
9. `000025_checkpoint_artifact_binding.sql`: reverse-bound deterministic Checkpoint Artifacts and retention-safe deletion constraints.
10. `000026_checkpoint_retention.sql`: failed/ready evidence-preserving expiry and cleanup access paths.

An integration test applies `000001`–`000016`, seeds legacy Cursor/Execution/Interaction/Repository state, then
upgrades through the current migration set and verifies every backfill. Runtime `AutoMigrate` and hand-applied database mutation
remain non-authoritative.

## Reuse decisions

- Reuse `packages/contracts/src/providerRuntime.ts` as the canonical event vocabulary.
- Reuse the eight local Provider Adapters as behavioral references and extraction sources; do not copy their
  implementation into eight separate remote runners.
- Reuse Stage 2 Worker Lease/Generation, Artifact, Credential, SSE and SaaS Projection boundaries.
- Extend `synara-agentd` for protocol negotiation, interaction delivery, workspace/checkpoint and graceful Drain.
- Preserve the current Web Control Plane authority adapter; add capability-driven behavior above it.

## Implementation order

1. Completed: add shared Capability and Provider Host v2 contracts plus contract fixtures.
2. Completed: implement Host Describe/Handshake, persisted compatibility gating and the bounded v1 path.
3. In progress: Codex App Server and Claude Agent SDK multi-Turn, native Interrupt/Steer, Approval, Plan Mode Input and history fallback are implemented. PostgreSQL cross-Worker Turn/Cursor/history/sequence continuity is covered; close real Target restart acceptance next.
4. In progress: Workspace/Git/Checkpoint DDL, public/private HTTPS Clone/Fetch, Git Credential, state reporting, Git-reference/Snapshot capture/restore and Checkpoint/Artifact retention are implemented; add SSH delivery, worktree/cache, Patch capture/restore and physical Workspace cleanup lifecycle.
5. In progress: Worker Manifest and graceful Drain are implemented; add reproducible image evidence, canary/rollback and upgrade isolation.
6. Run the shared suite across Local, SSH, Docker and Kubernetes; only then promote any Local-only Provider.
