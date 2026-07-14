# Stage 3 Drift Audit

Baseline: `codex/saas-tenancy-user` after clean commit `36ae47d6`, including the Provider Cursor expiry policy,
audited Resume selection, safe Provider-native fallback and replay-stable fallback Event identity. The immutable
Kubernetes deterministic Provider fixture report remains tied to `2763ebd3` and was recorded on 2026-07-14.

This audit treats executable code, migrations and repeatable tests as evidence. A local Provider Adapter is not
evidence that the Provider is supported by a remote Worker.

## Release boundary

| Provider | Existing local runtime | Existing remote host | Stage 3 remote release boundary |
| --- | --- | --- | --- |
| Codex | App Server adapter with Send, Steer, Review, Interrupt, Approval/Input, Compact, Rollback and Fork paths | Bidirectional Provider Host Protocol 2.1 runtime backed by Codex App Server, with native Cursor, Interrupt, Steer, Approval/Input and bounded history reconstruction | Tier 1 target |
| Claude | Agent SDK adapter with multi-turn, Interrupt, Approval/Input, history and discovery paths | Streaming Provider Host Protocol 2.1 runtime backed by Claude Agent SDK, with native Session ID, Interrupt, Steer, Approval/Input and bounded history reconstruction | Tier 1 target |
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
| B. Provider Host Protocol v2.1 | partial | Shared/Host Protocol 2.1 contracts, Describe, normalized Runtime and Release Policy descriptors, Command/Message envelopes, stable errors and terminal replay exist. Agentd defaults to v2, requires Minor 1 for managed compatibility, publishes startup descriptors, persists/gates the actual Host/Provider manifest and multiplexes concurrent Send/Interaction/Interrupt command terminals. Compact/Rollback/Fork/Review commands and broader real-adapter Acceptance evidence remain. |
| C. Unified Session/Turn semantics | partial | Start/Resume/Send use correlated v2 Command IDs with in-process terminal replay and persisted Runtime Binding identity. Codex App Server and Claude Agent SDK pass native Cursor resume, authoritative-history fallback, native Turn interrupt and native Steer. Migration `000022` provides the reusable Generation-fenced Control Command channel; Web Steer/Interrupt intent, Worker delivery and acknowledgement projections are implemented. Migration `000031` and the Session service enforce one active Execution per Session across `queued`, `leased`, `running`, `waiting-for-approval` and `recovering`. Interrupting `queued` or `recovering` work acknowledges the command and cancels synchronously without waiting for a Worker. Compact, Rollback, Fork, Review and broader real-target restart evidence remain. |
| D. Approval and Structured User Input | partial | Migration `000019`, Worker APIs and agentd implement Generation-fenced pull/delivered/acknowledged transitions and obsolete-Generation superseding. Migration `000021` persists immutable Turn runtime/interaction modes through Web, Session Event and Worker Workload. Migration `000028` records the request Runtime Event version so canonical resolutions never guess from an ambiguous name. Web now uses the durable Interaction list/reconcile and authoritative resolve path in SaaS mode. Real Codex and Claude Approval plus Plan Mode User Input pass Host round trips, and `2763ebd3` passed the Kubernetes deterministic fixture's Pending Approval Pod-loss Generation 1→2 recovery. Drain, real Eviction, unsupported-resume behavior and real-adapter cross-Target failure evidence remain. Claude uses a host-owned PreToolUse hook so local permission allow rules cannot bypass the durable decision. |
| E. Runtime Event compatibility | implemented baseline | `ProviderRuntimeEventV2` is now the only v2 wire vocabulary. Provider Host maps its bounded internal v1 messages to canonical v2 frames; agentd negotiates and enforces version 2; Control Plane keeps explicit legacy v1 while validating canonical v2 type/payload/size; Web projects both legacy and canonical Delta without duplicate Sequence application. Unknown Provider-native messages degrade to bounded `runtime.warning`; an unknown v2 wire type is rejected rather than silently reinterpreted. Provider-native Resume fallback uses an exact-shape canonical warning with no raw error/Cursor/Secret fields; agentd derives a stable Event ID only for that semantic slot from Execution, Generation and Send command identity. Eight-Provider golden fixtures and full Target acceptance remain under workflow L. |
| F. Authoritative history and Worker migration | partial | Migration `000018` and Session/Execution code create versioned Runtime Bindings; `000030` freezes the Credential version, Resume strategy and Provider binding digest per Execution generation. Cursor Envelope v2 authenticates that binding. Migration `000031` adds `absent / usable / quarantined`, source Execution/Generation/History Sequence lineage, legacy-ciphertext quarantine and the single-active-Execution index. Wrong-key, missing-Cipher, unknown/legacy-envelope, non-native Runtime, expiry at `now >= issuedAt + TTL`, and timestamps more than five minutes in the future preserve but quarantine ciphertext and select authoritative history; explicit Binding/Credential drift may clear it to `absent`. The default maximum age is 720 hours through `SYNARA_PROVIDER_CURSOR_MAX_AGE`, capped at 8760 hours. Extending TTL or restoring a key cannot revive quarantined ciphertext; only a fresh Cursor from the current Execution restores `usable`. Each Generation commits one safe Resume decision inside the existing `execution.leased` Event, and Claim receipt replay reuses that decision without reapplying age. If a previously selected native Cursor is no longer exactly available, replay returns `409 claim_replay_resume_cursor_unavailable` instead of silently changing strategy. SQLite and real PostgreSQL tests cover exact TTL/future-skew boundaries, quarantine stickiness, fresh recovery and two-pool retry/concurrency. Real Codex/Claude migration and four-Target restart evidence remain. |
| G. Credential isolation | partial | Codex/Claude Provider credentials use anonymous FD 3 on managed Unix Worker images, with strict allowlists and redaction; Worker/Lease variables are removed. Migration `000023` adds purpose-isolated Git Credentials, legacy Provider AAD compatibility, immutable scope/type, Project-only binding and a separate Generation-fenced Worker resolver. Agentd keeps Git username/token in an ephemeral Unix-socket AskPass channel used only for Clone/Fetch, clears it before Provider start and rejects unsafe local Git configuration. Web create/rotate and Project bind/unbind paths filter Provider versus Git purpose and Organization scope. Windows Provider Credential transport, remaining Provider allowlists, SSH/registry/package delivery and the full leakage suite remain. |
| H. Remote Workspace/Git lifecycle | partial | Migration `000020` creates one logical Workspace per Session and binds it to Target/Execution without persisting Worker paths or Credentials. Agentd uses the logical Workspace ID for a stable Session checkout, renews the Lease during preparation, validates HTTPS Repository URLs and branches, rejects embedded Credentials and local/private/link-local targets, DNS-pins Clone/Fetch with redirects disabled, creates an isolated Session branch, and reports `workspace.ready/failed` through Generation-fenced idempotent Worker APIs before Provider start. Public and Project-bound private HTTPS Clone/Fetch are implemented with exact-host Credential enforcement. A Target/Tenant/Project/repository-fingerprint bare cache is cross-process locked and network-Fetched every Turn; the mutable Workspace owns a private bare repository and relative linked worktree with no hardlinks, alternates, shared clone or cache common-dir dependency. Workspace locks span Provider exit, inspection, Checkpoint and terminal reporting. Docker Workers share the target volume cache, SSH provisions separate persistent roots, and Kubernetes uses a Pod-local cache unless an explicit RWX-equivalent PVC is configured. After Provider execution, agentd revalidates Git config, reports `workspace.dirty`, then creates a Git-reference, deterministic Patch or bounded regular-file Snapshot Checkpoint. Patch capture covers `baseCommit` to the final Worktree, authoritative raw tracked upserts and included regular untracked/ignored files; ignored trees are excluded only by a versioned rebuildable dependency/tool-cache segment policy. Sibling generation staging overlays the raw tracked payload and verifies Patch, index/worktree and Manifest identity before a locked two-rename replacement with rollback. A replacement Worker needs only the available Base Commit; the contract preserves the file tree and branch, not an unpushed local Commit graph. Checkpoint/Artifact retention protects current and active restore references, fails abandoned/expired uploads and preserves the last ready recovery point. Migration `000027` and Worker Protocol v2 now add Generation/incarnation/layout-fenced physical cleanup dispatch, acknowledgement and fair agentd execution. SSH Credential delivery, crash recovery/reconciliation for leftover generation backup/staging directories and real multi-Worker/Target acceptance remain. |
| I. Terminal/log/generated file/checkpoint | partial | Artifact path containment, server-side size/hash verification and retry-safe Checkpoint Artifact identity are implemented. Migrations `000020`, `000024` and `000025` enforce Checkpoint scope, ready-only recovery pointers, immutable terminal metadata, reverse Artifact binding and deletion protection. Agentd automatically creates Git-reference/Patch/Snapshot Checkpoints; Patch tar and manifest validation reject unsafe paths/types and partial restore, ambiguous Create/PUT/Complete responses reuse one Artifact, and retention fails expired uploads without losing the previous recovery point. Terminal lifecycle, rolling logs and general generated-file retry identity remain. |
| J. Worker drain/upgrade/version isolation | partial | Migration `000017` stores immutable Worker/Image/Provider manifests; registration deduplicates by Hash, records all eight Provider conclusions, and Claim binds/rejects by compatibility. Agentd advertises a tested Worker Runtime manifest and validates the Git SHA with the persisted lowercase-hex constraint. SIGTERM now sends an immediate Draining Heartbeat, stops Claim, keeps Heartbeat/Lease renewal through a bounded deadline, completes safe work or cancels and releases the Lease; stale Draining Workers become Offline. Legacy Runner cancellation and Provider Host abort terminate same-group Unix descendants and a Windows kill-on-close Job Object before the Workspace lock is released; command writes and durable-control terminal waits now honor cancellation. Deliberate Unix process-group escape, Windows FD3 Credential delivery, complete image digest/SBOM inputs, canary/rollback and incompatible/revoked operations remain. Managed SSH/Docker/Kubernetes carry a 20-second deadline inside their 30-second stop window, and embedded Local drain finishes before HTTP shutdown. |
| K. Web authority switch | partial | SaaS Project/Session/Turn/Event is Control Plane authoritative and local mode remains isolated. Tenant switching now cancels/removes the old Tenant query scope, closes old Event subscriptions, clears cross-Tenant drafts and serializes rapid switches. Canonical Runtime Events and durable pending Interaction list/resolve project through the same Control Plane-backed Thread model. Artifact Ready, Capability Unsupported and a current explicit no-Control-Plane local-mode acceptance still need first-class evidence without restoring dual authority. |
| L. Unified acceptance suite | partial | A shared Runner emits machine-readable JSON, Markdown and redacted logs, drives Local, Docker, SSH and Kubernetes through user APIs and real Control Plane/agentd product paths, and models both standing and execution-pinned Worker allocation plus capability-declared managed replacement. On 2026-07-14 the deterministic Codex fixture passed all 13 SSH cases on an isolated disposable OrbStack Ubuntu 24.04 VM. Clean commit `2763ebd3` then passed all 13 Kubernetes cases on an owned disposable Kind cluster, including Pending Approval Pod deletion, Generation 1→2 Interaction replacement, Artifact/User Input/Provider Error, Control Plane restart and Session Event Sequence 1→57; post-run checks confirmed the owned cluster and exact auto-built image were absent. See `docs/reports/stage-3-kubernetes-provider-fixture-acceptance-2763ebd3.md`. These fixture runs prove shared protocol and orchestration, not a real Codex/Claude release. Kubernetes Drain/Eviction/Network/Image Rollout, real Codex/Claude Target runs, long-session and the full failure matrix remain. |

## Frozen version boundary

- Worker Protocol remains independently versioned; the current managed version is `2`. Version 1 registration is
  rejected and retained only as historical documentation.
- Managed Provider Host Protocol is `{ major: 2, minor: 1 }`; Minor 0 is below the current managed compatibility
  floor and cannot supply the normalized Runtime/Release Policy evidence required for scheduling.
- Canonical local and remote Runtime Event is `ProviderRuntimeEventV2`. Control Plane keeps explicit Event Version 1
  compatibility for the bounded Provider Host v1 runner and validates Event Version 2 independently; it never
  reinterprets version 1 payloads.
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
5. `000021_agent_turn_modes.sql`: immutable runtime and interaction modes carried from Web through Workload.
6. `000022_execution_control_commands.sql`: durable Generation-fenced Provider Control Commands and terminal Interrupt acknowledgement.
7. `000023_git_credentials.sql`: purpose-isolated Git Credentials, Project binding and Worker resolution enforcement.
8. `000024_checkpoint_lifecycle.sql`: Generation-fenced Checkpoint lifecycle, ready recovery pointers and immutable payload constraints.
9. `000025_checkpoint_artifact_binding.sql`: reverse-bound deterministic Checkpoint Artifacts and retention-safe deletion constraints.
10. `000026_checkpoint_retention.sql`: failed/ready evidence-preserving expiry and cleanup access paths.
11. `000027_workspace_cleanup_dispatch.sql`: physical Workspace materialization identity, cleanup command fencing and Worker-incarnation ownership.
12. `000028_interaction_runtime_event_version.sql`: persisted Interaction Runtime Event version for legacy/canonical resolution continuity.
13. `000029_provider_runtime_release_policy.sql`: normalized Provider Runtime evidence, explicit Experimental release policy and immutable Runtime Binding snapshots.
14. `000030_execution_provider_cursor_snapshots.sql`: Generation-fenced Execution Credential/Resume/Provider binding snapshots and immutable content-addressed manifests.
15. `000031_session_execution_cursor_lineage.sql`: Cursor state/lineage constraints, legacy ciphertext quarantine and one active Execution per Session across the five guarded statuses, including `queued`.

An integration test applies `000001`–`000016`, seeds legacy Cursor/Execution/Interaction/Repository state, then
upgrades through the current migration set and verifies every backfill. Dedicated migration coverage also verifies that
`000031` fails closed when duplicate active Session Executions already exist and does not record a failed migration as
applied. Runtime `AutoMigrate` and hand-applied database mutation remain non-authoritative. The Cursor
age/fallback policy uses authenticated Envelope `IssuedAt` plus existing `000030`/`000031` state and lineage; it
does not add or mutate a migration.

## Reuse decisions

- Reuse `packages/contracts/src/providerRuntime.ts` as the canonical event vocabulary.
- Reuse the eight local Provider Adapters as behavioral references and extraction sources; do not copy their
  implementation into eight separate remote runners.
- Reuse Stage 2 Worker Lease/Generation, Artifact, Credential, SSE and SaaS Projection boundaries.
- Extend `synara-agentd` for protocol negotiation, interaction delivery, workspace/checkpoint and graceful Drain.
- Preserve the current Web Control Plane authority adapter; add capability-driven behavior above it.

## Implementation order

1. Completed: add shared Capability and Provider Host Protocol 2.1 contracts plus contract fixtures.
2. Completed: implement Host Describe/Handshake, persisted compatibility gating and the bounded v1 path.
3. In progress: Codex App Server and Claude Agent SDK multi-Turn, native Interrupt/Steer, Approval, Plan Mode Input and history fallback are implemented. Runtime Event v2 is canonical end to end. Cursor Envelope v2, per-Execution Provider snapshots, Cursor quarantine/lineage, the bounded expiry policy, audited Claim selection, safe Provider-native invalid/expired fallback, one active Execution per Session and pre-Claim Interrupt cancellation are implemented. Run real Codex/Claude migration and restart acceptance across Local, SSH, Docker and Kubernetes next.
4. In progress: Workspace/Git/Checkpoint DDL, public/private HTTPS Clone/Fetch, Git Credential, state reporting, cross-process locked cache plus private relative worktree generations, Git-reference/Patch/Snapshot capture/restore, physical cleanup and Checkpoint/Artifact retention are implemented; add SSH Credential delivery, leftover staging/backup reconciliation and real multi-Worker/Target acceptance.
5. In progress: Worker Manifest and graceful Drain are implemented; add reproducible image evidence, canary/rollback and upgrade isolation.
6. In progress: the deterministic shared Runner covers Local, Docker and Kubernetes and emits JSON/Markdown
   evidence. The SSH Driver owns a disposable OrbStack VM, one-time credential, host-key pinning and product
   install/upgrade/revoke lifecycle; its deterministic Codex fixture passed the 13-case live SSH suite on
   2026-07-14. Clean commit `2763ebd3` also passed the 13-case Kubernetes suite including Pending Approval Pod loss.
   Finish Kubernetes Drain/Eviction/Network/Image Rollout coverage and run real Codex/Claude adapters across the
   required Targets plus the long-session/failure matrix before promoting any Local-only Provider or claiming the
   four-Target release gate.
