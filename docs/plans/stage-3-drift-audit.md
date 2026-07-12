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
| A. Capability matrix and support levels | partial | Eight local adapters exist, but current booleans cover only composer features. Add one schema-backed Capability ID set, behavior state (`native`, `emulated`, `unsupported`) and release tier shared by Host, Worker, Control Plane and Web. |
| B. Provider Host Protocol v2 | missing | `provider-host` reads one unversioned JSON document and emits only `event`/`result`. Add v2 Describe/Handshake, Command/Message envelopes, size limits, terminal guarantees, stable errors and a controlled v1 boundary. |
| C. Unified Session/Turn semantics | partial | Local adapters expose most operations. Remote Host only runs one Start/Resume-like Turn and has no Command ID or duplicate-side-effect protection. Freeze cross-provider command semantics and implement Tier 1 paths. |
| D. Approval and Structured User Input | partial | Migration `000014` and Control Plane Resolve APIs persist interactions and fence old Generations. agentd cannot deliver a persisted Resolution back to a running/recovered Host; states are only `pending/resolved/expired`. |
| E. Runtime Event compatibility | partial | `packages/contracts` has a rich canonical `ProviderRuntimeEventV2`; remote Host emits a separate small `runtime.*` vocabulary and Control Plane accepts only Event Version 1. Add an audited compatibility bridge and unknown-event policy instead of creating a third vocabulary. |
| F. Authoritative history and Worker migration | partial | PostgreSQL history reconstruction and encrypted native Cursor recovery work for text Turns. Snapshot policy lacks Tool/Artifact/Plan/Interaction/Checkpoint context, Cursor compatibility binding and explicit fallback results. |
| G. Credential isolation | partial | Codex/Claude credentials use FD 3, strict allowlists and redaction; Worker/Lease variables are removed. Remaining Providers and Git/registry/package credentials have no remote delivery contract or leakage suite. |
| H. Remote Workspace/Git lifecycle | missing | agentd creates an Execution directory and validates uploaded regular files. There is no persisted Workspace identity/state, secure Clone/Fetch/Worktree policy, Repository URL SSRF guard, Checkpoint or cleanup/retention lifecycle. |
| I. Terminal/log/generated file/checkpoint | partial | Artifact upload and regular-file/symlink containment exist. There is no Terminal lifecycle, rolling long-log Artifact, Checkpoint model, last-known-good recovery point or reference-aware retention. |
| J. Worker drain/upgrade/version isolation | partial | Control Plane supports `draining`, protocol v1 and Generation fencing. agentd does not initiate Drain on shutdown, expose a complete Manifest, checkpoint before deadline, or distinguish incompatible/revoked lifecycle states. Worker image pins Codex/Claude CLI versions but not base digests and produces no Manifest/SBOM. |
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

## Database and DDL gaps

Existing migrations stop at `000016_sse_connection_leases.sql`. Stage 3 needs checked-in, forward-only DDL for:

1. Worker/Provider Host/Image Manifest and compatibility status.
2. Expanded Interaction lifecycle, expiry and delivery/acknowledgement state.
3. Provider Runtime Binding/Cursor compatibility metadata.
4. Remote Workspace identity/state/manifest and retention fields.
5. Checkpoint metadata and last-known-good references.

The exact split is determined while implementing the owning workflow. No runtime `AutoMigrate` or hand-applied
database mutation substitutes for these migrations.

## Reuse decisions

- Reuse `packages/contracts/src/providerRuntime.ts` as the canonical event vocabulary.
- Reuse the eight local Provider Adapters as behavioral references and extraction sources; do not copy their
  implementation into eight separate remote runners.
- Reuse Stage 2 Worker Lease/Generation, Artifact, Credential, SSE and SaaS Projection boundaries.
- Extend `synara-agentd` for protocol negotiation, interaction delivery, workspace/checkpoint and graceful Drain.
- Preserve the current Web Control Plane authority adapter; add capability-driven behavior above it.

## Implementation order

1. Add shared Capability and Provider Host v2 contracts plus contract fixtures.
2. Implement Host Describe/Handshake and agentd compatibility gating while retaining the bounded v1 path.
3. Close Codex/Claude Tier 1 command, interaction and recovery semantics.
4. Add Workspace/Git/Checkpoint DDL and runtime lifecycle.
5. Add Worker Manifest, graceful Drain, reproducible image evidence and upgrade isolation.
6. Run the shared suite across Local, SSH, Docker and Kubernetes; only then promote any Local-only Provider.
