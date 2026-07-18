# Stage 3 Drift Audit

Current baseline: `codex/saas-tenancy-user` after clean commit `39b9b328`. The owned disposable
one-control-plane/two-Worker Kind Worker Release gate now combines registry-pushed immutable rollout, exact candidate
Pod-loss recovery, transition fencing, and bounded load in one clean-SHA report: `15/15` cases, Generation `1 -> 2`
only for the deleted candidate Pod, `24/24` execution-pinned load Executions, `12/12` quota rejection/retry,
`18/18` overlap, `24/24` release-pin/Worker-binding/resource-profile checks, paginated `2097`-entry Audit history,
six published Outbox messages, exact cleanup, and zero Secret findings. The two concurrent Pods were observed across
the two schedulable Worker Nodes. See
`docs/reports/stage-3-kubernetes-kind-rollout-recovery-load-39b9b328.md`.

Earlier evidence chain: `codex/saas-tenancy-user` after clean commit `1e826324`, adding the operator-approved reusable `orbstack`
Kubernetes deterministic fixture/failure gate `19/19`, shared local image transport, exact Pod Eviction and cleanup
on top of clean commit `41683366`'s deterministic managed Docker immutable Worker Release candidate container-loss
recovery under baseline/canary overlap, `25` release-pinned load waves
across promote and rollback, load-safe paginated Audit and topic-filtered Outbox history, the earlier exact
network/container/fixture Provider Host failure-under-load gates, managed same-Worker replacement and their
post-recovery four-Session bounded load/admission waves, the deterministic Local active-Execution
Retention/Cleanup concurrency gate, the deterministic managed Docker two-Worker/two-Session Provider concurrency
gate, and the deterministic Local 100-Turn fixture soak. The
consolidated real Codex/Claude Local product and controlled-failure
release gate remains tied to `253052aa`; terminal-aware Interaction waits, the Provider Cursor expiry policy,
audited Resume selection, standalone Provider-native generated-file capture and Artifact-backed Large Diff
projection remain part of that earlier clean evidence chain. The owned Kind Kubernetes deterministic Provider
fixture remains tied to `2763ebd3`; the latest approved reusable-context evidence is summarized in
`docs/reports/stage-3-kubernetes-orbstack-fixture-1e826324.md`. The latest Docker rollout/load evidence is
summarized in `docs/reports/stage-3-worker-release-rollout-load-41683366.md`; its earlier Busy Worker predecessor is
retained in `docs/reports/stage-3-worker-release-rollout-d3af9380.md`; the real Local release evidence remains in
`docs/reports/stage-3-real-provider-local-release-gate-253052aa.md`; deterministic Local long-Session evidence is in
`docs/reports/stage-3-local-fixture-soak-6e866a30.md`; deterministic Docker Provider concurrency evidence is in
`docs/reports/stage-3-docker-fixture-concurrency-eeb7a2f1.md`; deterministic Local Retention/Cleanup concurrency
evidence is in `docs/reports/stage-3-local-fixture-retention-concurrency-c27914da.md`; deterministic Docker bounded
load/admission evidence is in `docs/reports/stage-3-docker-fixture-load-e944b449.md`; deterministic Docker targeted
network and container-loss failure under load evidence is in
`docs/reports/stage-3-docker-fixture-load-failure-7684c6d8.md`, with the earlier network-only checkpoint retained in
`docs/reports/stage-3-docker-fixture-load-failure-ab88798d.md`. The
standalone generated-file and Large Diff
predecessors remain in `docs/reports/stage-3-real-provider-local-standalone-generated-file-matrix-be919393.md` and
`docs/reports/stage-3-real-provider-local-large-diff-matrix-90fae52c.md`. The earlier two-Turn smoke,
dirty-worktree and deterministic fixture evidence remains in
`docs/reports/stage-3-provider-runtime-acceptance-fb9e25ec.md` and
`docs/reports/stage-3-provider-runtime-acceptance-2026-07-15.md`. Clean `88f922ed` now closes the isolated SaaS Web
Artifact Ready/list/download/refresh/reconnect/Server-restart slice with PostgreSQL/MinIO and exact payload hash
evidence; see `docs/reports/stage-3-saas-web-artifact-download-88f922ed.md`. None closes the four-Target release gate.

This audit treats executable code, migrations and repeatable tests as evidence. A local Provider Adapter is not
evidence that the Provider is supported by a remote Worker.

Operator inputs recorded on 2026-07-17 and executed on 2026-07-18 narrow the next gates without closing them:
controlled Codex/Claude Credentials must support third-party `apiKey` plus optional `baseUrl`; an external SSH target
is authorized but its authentication remains outside the repository; the selected local Kubernetes context
`orbstack` now has a new
clean-SHA `6b71703f` deterministic report with `22` pass, one explicitly unauthorized Node Drain and exact cleanup.
The clean-SHA real Kubernetes four-child gate also ran with controlled third-party profiles: Codex failure passed
`16/16`, while Codex product lacked an approval interaction and Claude product/failure received a stable HTTP `502`
`provider_unavailable`; all clusters/image/state were removed and the aggregate Secret scan was empty. Clean SHA
`f958c1b2` then ran the same four-child gate through Docker: Codex failure again passed `16/16`, Codex product again
completed without an Approval Interaction, and both Claude children failed their baseline Turn with the same HTTP
`502`; all child container/network/volume/state resources and the shared Image were removed, and the aggregate scan
covered `37` files/`4,971,618` bytes with zero findings. The real-Provider gate therefore remains open rather than
silently degrading the profiles. Production concurrency is
governed by quota, Worker slots and CPU/memory resource profiles rather than one hard-coded number. Clean SHA
`e2d70fb6` now enforces a minimum measured load duration plus a maximum-wave safety bound and records the exact
resource profile, effective concurrency, success/unexpected-error rates, throughput and nearest-rank P50/P95/P99.
Its deterministic Docker gate completed `304.201s`, `56` waves and `224/224` Executions with zero unexpected errors;
production signing will use `kms-key`, potentially through a self-hosted Vault `hashivault://...`
reference. External-host Runner/Gate integration is implemented with repository-external identity, pinned Host Key,
install conflict refusal and non-destructive cleanup; its clean-SHA real-Provider report, the remaining remote reports,
numeric latency/error/duration SLA, and concrete KMS identity/tlog/admission policy remain required evidence.

## Release boundary

| Provider    | Existing local runtime                                                                                   | Existing remote host                                                                                                                                                 | Stage 3 remote release boundary                 |
| ----------- | -------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------- |
| Codex       | App Server adapter with Send, Steer, Review, Interrupt, Approval/Input, Compact, Rollback and Fork paths | Bidirectional Provider Host Protocol 2.1 runtime backed by Codex App Server, with native Cursor, Interrupt, Steer, Approval/Input and bounded history reconstruction | Tier 1 target                                   |
| Claude      | Agent SDK adapter with multi-turn, Interrupt, Approval/Input, history and discovery paths                | Streaming Provider Host Protocol 2.1 runtime backed by Claude Agent SDK, with native Session ID, Interrupt, Steer, Approval/Input and bounded history reconstruction | Tier 1 target                                   |
| Cursor      | ACP adapter with Send, Interrupt, Approval/Input, history and Rollback                                   | None                                                                                                                                                                 | Local-only until the shared remote suite passes |
| Antigravity | CLI adapter with Send, Interrupt, Approval/Input, history and model discovery                            | None                                                                                                                                                                 | Local-only until the shared remote suite passes |
| Grok        | ACP adapter with Send, Interrupt, history, Rollback and Compact                                          | None                                                                                                                                                                 | Local-only until the shared remote suite passes |
| Kilo        | OpenCode-compatible adapter with Send, Interrupt, Approval/Input, history, Compact and Fork              | None                                                                                                                                                                 | Local-only until the shared remote suite passes |
| OpenCode    | SDK adapter with Send, Interrupt, Approval/Input, history, Compact and Fork                              | None                                                                                                                                                                 | Local-only until the shared remote suite passes |
| Pi          | SDK adapter with Send, Steer, Interrupt, User Input, history, Rollback and Compact                       | None                                                                                                                                                                 | Local-only until the shared remote suite passes |

Droid remains intentionally outside the ordered Provider Host catalog. Its local adapter does not imply a remote
capability descriptor or scheduling eligibility.

`Local-only` is an explicit support conclusion, not a silent fallback. Remote scheduling must reject it with a
stable capability error before an Execution is claimed. Promotion to Tier 2 or Tier 1 requires the same Provider
Acceptance Fixture used by Codex and Claude.

## Workflow audit

| Workflow                                      | Status               | Current evidence and required delta                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| --------------------------------------------- | -------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| A. Capability matrix and support levels       | partial              | `providerCapabilityCatalog.json` is now the single machine-readable TS source and generates a hash-checked Go Catalog used by agentd, Manifest validation and Target policy. Control Plane exposes sanitized Project/Session `target` or execution-pinned projections with `supported / unsupported / unobserved`, stable reasons and `native / emulated` support modes; Session/Turn persistence rechecks Start/Send/Plan requirements inside the idempotent transaction. Web Provider selection, Plan, Steer, Interrupt and unsupported advanced commands consume the same projection. Provider × Target Acceptance for every declared native/emulated capability, including real remote Compact/Rollback/Fork/Review evidence, remains.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| B. Provider Host Protocol v2.1                | partial              | Shared/Host Protocol 2.1 contracts, Describe, normalized Runtime/Release descriptors, Command/Message envelopes, stable errors and terminal replay exist. Agentd requires Minor 1 for managed compatibility, publishes and gates the actual Host/Provider manifest, and multiplexes concurrent Send/Interaction/Interrupt plus primary Compact/Review terminals. Rollback/Fork are explicit Control Plane emulations rather than silently invented Host commands. Broader real-adapter and four-Target acceptance evidence remains.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| C. Unified Session/Turn semantics             | partial              | Start/Resume/Send use correlated v2 Command IDs with persisted Runtime Binding identity, native Cursor resume and authoritative-history fallback. Migration `000031` plus the Session service enforce one active Execution across the five guarded statuses. Migration `000032` makes Compact, Review, Rollback and Fork explicit immutable Turn kinds, preserves logical history ancestry and requires one matching primary Control Command for Worker-executed advanced Turns. Control Plane, agentd, Provider Host and SaaS Web now share that single path; real Codex/Claude replacement-Worker and four-Target restart evidence remains.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| D. Approval and Structured User Input         | partial              | Migrations `000019`, `000021` and `000028`, Worker APIs and agentd implement durable, versioned, Generation-fenced Interaction delivery and obsolete-Generation superseding. SaaS Web uses the authoritative Interaction list/reconcile/resolve path; Claude's host-owned PreToolUse hook prevents local allow rules from bypassing the durable decision. Real Codex/Claude Local Approval and Plan Mode User Input pass Host round trips. Clean `b07e5bd9` passes owned-Kind `17/17`: Pending Approval remains compatible, while Pending Structured User Input Pod loss preserves the Turn/Execution, advances Generation `1 -> 2`, expires/supersedes the stale request, verifies the exact replacement question and resolves only the replacement into one terminal path. SaaS two-page User Input convergence/concurrent resolve, unsupported-resume behavior and real-adapter cross-Target release evidence remain.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| E. Runtime Event compatibility                | implemented baseline | `ProviderRuntimeEventV2` is now the only v2 wire vocabulary. Provider Host maps its bounded internal v1 messages to canonical v2 frames; agentd negotiates and enforces version 2; Control Plane keeps explicit legacy v1 while validating canonical v2 type/payload/size; Web projects both legacy and canonical Delta without duplicate Sequence application. Unknown Provider-native messages degrade to bounded `runtime.warning`; an unknown v2 wire type is rejected rather than silently reinterpreted. Provider-native Resume fallback uses an exact-shape canonical warning with no raw error/Cursor/Secret fields; agentd derives a stable Event ID only for that semantic slot from Execution, Generation and Send command identity. Eight-Provider golden fixtures and full Target acceptance remain under workflow L.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| F. Authoritative history and Worker migration | partial              | Migration `000018` and Session/Execution code create versioned Runtime Bindings; `000030` freezes the Credential version, Resume strategy and Provider binding digest per Execution generation. Cursor Envelope v2 authenticates that binding. Migration `000031` adds `absent / usable / quarantined`, source Execution/Generation/History Sequence lineage, legacy-ciphertext quarantine and the single-active-Execution index. Wrong-key, missing-Cipher, unknown/legacy-envelope, non-native Runtime, expiry at `now >= issuedAt + TTL`, and timestamps more than five minutes in the future preserve but quarantine ciphertext and select authoritative history; explicit Binding/Credential drift may clear it to `absent`. The default maximum age is 720 hours through `SYNARA_PROVIDER_CURSOR_MAX_AGE`, capped at 8760 hours. Extending TTL or restoring a key cannot revive quarantined ciphertext; only a fresh Cursor from the current Execution restores `usable`. Each Generation commits one safe Resume decision inside the existing `execution.leased` Event, and Claim receipt replay reuses that decision without reapplying age. If a previously selected native Cursor is no longer exactly available, replay returns `409 claim_replay_resume_cursor_unavailable` instead of silently changing strategy. SQLite and real PostgreSQL tests cover exact TTL/future-skew boundaries, quarantine stickiness, fresh recovery and two-pool retry/concurrency. Clean commit `61e38f4f` passes real Codex/Claude Local policy expiry, restart and `authoritative-history / cursor_expired` continuity; replacement-Worker and SSH/Docker/Kubernetes evidence remain.                                                                                         |
| G. Credential isolation                       | partial              | Codex/Claude Provider credentials use anonymous FD 3 with strict allowlists and SecretGuard redaction; Worker/Lease tokens are removed before Provider start. Managed Codex/Claude create and rotate now accept only `apiKey` plus an optional validated `baseUrl`; provider-specific Web forms avoid the advanced JSON surface, while historical Codex `organization` and Claude `authToken` remain resolve-only compatibility fields. Migration `000033` adds explicit Tenant/Organization/User/Platform scopes and selection policy. Migrations `000035`/`000036` replace legacy Project Git authority with active Binding plus immutable per-Generation Grant IDs, `000038` preserves efficient FK enforcement for disabled history, and `000039` enforces one active image-pull Binding per Target. Agentd resolves only the exact operation stage, keeps HTTPS AskPass or one pinned SSH key/host-key agent in memory, clears it before Provider start and never forwards Workspace credentials to Provider Host. The shared Runner now requires an operator-selected environment source for real SSH/Docker/Kubernetes runs, registers the secret and optional Base URL with redaction before build/start, creates an encrypted isolated-Control-Plane Credential, and binds only its ID; command, Target, Image and report evidence retain neither the variable name nor value. Docker/Kubernetes 401/429 injection uses an unguessable redacted route token and persists only normalized paths/header names; Kubernetes proves host-gateway reachability through the actual execution-pinned Provider request rather than a token-bearing probe Pod. Registry/package execution stages, Windows Provider transport and the full real-target leakage suite remain. |
| H. Remote Workspace/Git lifecycle             | partial              | Migrations `000020`/`000027` provide logical Workspace, Checkpoint, materialization and fenced physical-cleanup state without persisting Worker paths or plaintext credentials. Migrations `000035`/`000036` bind Project Git access through immutable generation Grants. Agentd holds the logical Workspace lock across materialization, Provider execution, inspection, Checkpoint and terminal reporting; HTTPS Clone/Fetch uses exact-host AskPass, while SSH uses an `ssh://` repository, public-address DNS pinning, exact stored host key and one temporary key agent. Cache and mutable Workspace generations remain isolated and recover interrupted installs fail closed. Git-reference/Patch/Snapshot capture and restore preserve the authoritative file tree, branch and tracked/untracked classification. Clean commit `c27914da` proves deterministic Local active-Execution retention fencing, protected seed/current Ready Checkpoint lineages and exactly one agentd-acknowledged post-terminal physical cleanup. Real Provider multi-Worker/Target retention, long-lived SSH rotation, multi-node and failure-injection evidence remain.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| I. Terminal/log/generated file/checkpoint     | partial              | Artifact path containment, server-side size/hash verification and retry-safe Checkpoint Artifact identity are implemented. Ordinary `generated_file`, `terminal_log` and `diff` uploads negotiate a header-based v1 idempotency feature, derive a content-bound deterministic Execution/Generation Artifact ID, reuse stable Create/Complete request IDs, refresh pending grants and recover a Ready Artifact after ambiguous responses. Before Provider start, agentd binds the Workspace and Runtime Output roots to anchored descriptors, rejects traversal/symlinks/non-regular files, and retains the opened descriptor through Secret Guard, hashing, upload and Ready verification. Migrations `000020`, `000024` and `000025` enforce Checkpoint scope and binding; forward migration `000041` adds only the `diff` Artifact kind. Agentd automatically creates Git-reference/Patch/Snapshot Checkpoints, including an empty Snapshot after the last non-Git file is deleted. Clean commit `be919393` proves the generated-file boundary, clean commit `90fae52c` proves Ready downloadable Large Diff Artifacts for Codex and Claude, and clean commit `c27914da` proves concurrent deletion of one unreferenced generated Artifact without deleting protected seed/current Checkpoint Artifacts during an active Execution. `workspace-checkpoint-unconfirmed` remains an explicit error Activity. Lossless real large-log, cross-Target and production Retention acceptance remain.                                                                                                                                                                                                                                                                             |
| J. Worker drain/upgrade/version isolation     | partial              | Migrations `000017`, `000034`, `000037` and `000040` provide immutable Worker manifests, operator-revocation fencing, target-scoped Release Revisions, strict-CAS promoted/canary policy, transition history and release-pinned scheduling. Agentd Drain preserves the Workspace lock and reports conservative data-loss risk. Clean commit `7659dd5f` proves cached/no-cache multi-arch Registry reproducibility, SPDX/SLSA, embedded identity, default ephemeral exact-digest signing, `HIGH/CRITICAL=0`, Secret=0, EOSL/DB freshness and exact cleanup. Clean commit `d3af9380` proves deterministic managed Docker immutable Revision canary/promote/rollback and Busy baseline fencing. Clean commit `41683366` adds exact candidate container-loss recovery while promoted/canary Executions overlap, Generation `1->2`, unchanged baseline peer, promote/rollback fail-closed behavior, `25` release-pinned load waves across promotion and rollback, load-safe Audit/Outbox history and exact cleanup. Clean commit `aa1d0225` proves a deterministic three-node owned-Kind PDB-blocked drain followed by cross-Worker replacement while the source Node remains cordoned, plus separate graceful Drain and direct Eviction recovery. Checked-in keyless/KMS production signing paths are implemented; real production signer identity/tlog/admission, Registry Credential/retention, real Provider remote rollout, production Kubernetes multi-node canary/rollback/cloud eviction/CNI and production-duration SLA/soak remain.                                                                                                                                                                                                                                   |
| K. Web authority switch                       | partial              | SaaS Project/Session/Turn/Event is Control Plane authoritative, local mode remains isolated, and Provider/advanced-operation handlers fail closed through Control Plane projections without local Native API fallback. Clean `3a6d347d`, `0b4d8e4e` and `88f922ed` prove SaaS authority/reconnect, local-mode restart/resume and durable Artifact download boundaries. The `0eeabbc1` and `82adfc3f` Browser slices add compatible-Worker restart, active pending-Approval Worker replacement, two-page Approval convergence, strict-CAS model convergence and transcript/SSE fixes. Clean `b07e5bd9` adds `4/4` Structured User Input component tests for single-select auto-submit, multi-select deferral, stale timer cancellation on replacement remount and resolving-state disabling, plus remote deterministic Kind replacement evidence. Real Provider live output/replacement, SaaS two-page Structured User Input convergence, remaining advanced operations and remote-Target multi-browser evidence remain.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| L. Unified acceptance suite                   | partial              | The shared Runner and Local/SSH/Docker/Kubernetes/Registry gates emit source, cleanup and Secret-scanned reports. Clean `253052aa` passes the real Codex/Claude Local product/failure aggregate; `7659dd5f`, `d3af9380`, `41683366`, `aa1d0225`, `e2d70fb6` and the focused load/Retention/soak commits cover deterministic supply chain, immutable rollout, multi-node recovery, resource-profiled load and cleanup mechanics. Clean `b07e5bd9` updates the owned-Kind core suite to `17/17` with Pending Structured User Input Pod-loss recovery and product-shaped fixture Credentials. Acceptance Runner is `170/170` and Stage 3 Python is `304/304`. Clean `6b71703f` and `f958c1b2` execute the real Kubernetes/Docker four-child gates but the configured Codex product and Claude profiles still fail closed. A real SSH aggregate, passing Docker/Kubernetes Provider profiles, approved production SLA thresholds, concrete production KMS identity/tlog/admission, production Kubernetes multi-node rollout and real-Provider concurrency/Retention/load/failure remain open.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |

> 2026-07-17 status correction: the earlier J/L summary-table wording that still lists clean Registry
> reproducibility, image-signing mechanics, vulnerability-policy evidence, or the production signing implementation
> path as wholly missing is superseded by the `7659dd5f` signing-policy update below. Real production signer
> identity/tlog/admission, Registry Credential/retention, real Provider four-Target rollout, Kubernetes multi-node
> canary/rollback, and production soak remain open. The deterministic managed Docker immutable rollout slice is
> closed by `d3af9380`; the deterministic Local long-Session/restart/pagination slice is closed by `6e866a30`;
> deterministic managed Docker multi-Provider/multi-Session overlap mechanics are closed by `eeb7a2f1`; deterministic
> Local active-Execution Retention fencing and post-terminal physical cleanup mechanics are closed by `c27914da`;
> deterministic bounded managed Docker quota/admission and slot-reuse mechanics are closed by `e944b449`;
> deterministic single-host managed Docker exact network-failure targeting, peer isolation, Generation fencing and
> post-recovery load mechanics are closed by `ab88798d`; exact busy-container loss, same logical Worker replacement,
> incarnation fencing and named-volume continuity are closed by `7684c6d8`; exact busy Provider Host descendant
> process crash, `provider_unavailable` terminalization and distinct new-Execution recovery are closed by `cfecba63`;
> deterministic immutable release-rollout container-loss recovery, `25` release-pinned waves, paginated Audit and
> topic-filtered Outbox history are closed by `41683366`.

> 2026-07-17 OrbStack correction: clean commit `1e826324` supersedes the D/L table wording that still lists
> deterministic Kubernetes Eviction or approved-context clean-SHA fixture evidence as missing. The `19/19` report
> and Stage 3 Python `259/259` close only fixture Pod-loss/network/Eviction/Canary/restart/cleanup mechanics; real
> Codex/Claude, Node Drain/PDB, multi-node and immutable registry rollout remain open.

> 2026-07-18 Kubernetes/third-party correction: clean commit `6b71703f` adds a reusable-Context API origin/TLS
> server-name override without mutating kubeconfig and passes the deterministic OrbStack matrix with exact cleanup.
> The real four-child gate was executed rather than left as a preflight claim. Codex controlled failures pass, but
> the configured Codex profile does not produce command/approval interactions and the configured Claude profile
> returns HTTP `502` in both Kubernetes and Local reproduction. Report diagnostics remain redacted and no new
> unsupported capability was accepted. See
> `docs/reports/stage-3-real-provider-kubernetes-third-party-gate-6b71703f.md`.

> 2026-07-18 owned-Kind Drain correction: clean commit `fc9b2bf6` passes the complete deterministic Kubernetes
> `23/23` matrix on a runner-owned disposable Kind cluster. Exact Node cordon/drain/uncordon, graceful Pod DELETE,
> Generation `1 -> 2` fencing, `policy/v1` Eviction, Canary, restart, exact cluster/image/state cleanup and Secret
> scan are proven. This closes deterministic single-node Drain only; PDB, multi-node, immutable registry rollout and
> real Provider gates remain open. See
> `docs/reports/stage-3-kubernetes-kind-drain-fixture-fc9b2bf6.md`.

> 2026-07-18 owned-Kind PDB/multi-node correction: clean commit `aa1d0225` supersedes the wording that still lists
> deterministic PDB or multi-node Drain mechanics as missing. The complete `24/24` matrix waits for one
> control-plane and two Worker Nodes to reach `3/3` Ready with two schedulable Workers; an exact PDB first blocks
> Eviction-backed drain with `disruptionsAllowed=0`, then exact PDB removal allows graceful Drain and replacement on
> the other Worker while the source remains cordoned. Separate direct Eviction, Canary, restart, cleanup and Secret
> scan also pass. This does not close real Provider, production multi-node/cloud CNI, immutable registry rollout or
> production load/soak. See `docs/reports/stage-3-kubernetes-kind-pdb-multinode-aa1d0225.md`.

> 2026-07-18 Kubernetes rollout recovery/load correction: clean commit `39b9b328` supersedes J/L wording that still
> lists deterministic multi-node immutable rollout under bounded load as missing. The `15/15` owned-Kind gate pulls
> two distinct Registry digests, performs strict `promote -> 100% canary -> promote -> rollback`, deletes only the
> candidate Pod, preserves the immutable Release while advancing Generation exactly `1 -> 2`, and blocks unsafe
> promote/rollback throughout recovery. Six waves complete `24/24` load Executions with `12/12` quota
> rejection/retry, `18/18` overlap, `24/24` pin/binding/resource checks, no double execution or duplicate terminal,
> Audit `2097` entries/`11` pages, six published Outbox messages, exact cleanup, and zero Secret findings. Stage 3
> Python is `296/296`; focused `agentd` and `workerreleases` Go tests pass. This remains deterministic fixture
> evidence: real SSH/Docker/Kubernetes Provider aggregates, numeric production SLA/soak, production Registry, and
> concrete KMS signer identity/tlog/admission remain open. See
> `docs/reports/stage-3-kubernetes-kind-rollout-recovery-load-39b9b328.md`.

### 2026-07-15 Advanced Session operation evidence update

- Workflow A/B/C/K 的 Compact、Review、Rollback、Fork 主阻断已解除。Migration `000032`、Control Plane
  queued Primary Operation、agentd terminal-before-ack、Provider Host v2.1、Capability Catalog 与 SaaS Web
  路由已形成单一路径；Rollback/Fork 为 Control Plane `emulated`，Codex Compact/Review 为 native，Claude
  Review 为只读 `emulated`，Claude Compact 为 Explicit Unsupported。
- PostgreSQL 17 真实 Migration Integration 验证 Fork cycle、NULL/shape、逻辑祖先 Turn、Primary Command
  UPDATE/DELETE 和父 Execution cascade；SQLite 镜像关键安全约束。Service/HTTP/daemon tests 覆盖
  private/CAS/quota/capability/idempotency/concurrency、Replay Header/非法 JSON/缺 Key，以及 Primary
  terminal-before-ack。Logical History/Resume tests 覆盖 Fork Prefix、循环/深链、501 条尾部和 Rollback Chain。
- Contracts 24/24、Provider Host focused 89/89、Web focused 382/382 通过；SaaS Compact/Review/Fork/Rollback
  测试确认 `readNativeApi()` 调用数为零。仍缺真实 Codex/Claude Remote Worker 替换后的高级操作 Release
  Acceptance，因此相关 Workflow 保持 `partial`，不把 deterministic fixture 当作最终发布证据。

### 2026-07-15 Credential, Worker revocation and release DDL evidence update

- Workflow G/H/K 的 Workspace Credential 已迁移到单一 Binding/Grant authority。Migration `000033`
  增加 Tenant/Organization/User/Platform Credential Scope 与自动选择策略；`000035` 增加 Project/Target
  Binding 和 immutable per-Generation Grant；`000036` 完成 rolling backfill 后清空并禁止继续写
  `projects.git_credential_id`；`000038` 为历史 disabled Binding 的复合外键补齐非 partial lookup index。
  Workflow K 中“model-switch 不新增 DDL、复用 `000031`”仅描述 model-switch 这一局部操作，不是当前
  Stage 3 的全局 Migration boundary。
- Workflow J/L 的 Worker operator fencing 与 Release rollout 已持久化。Migration `000034` 将兼容性状态
  与不可逆的 operator revocation 分离，以 logical identity tombstone 阻止 Token、Heartbeat、Claim 和
  同身份重新注册；Migration `000037` 增加 immutable Release Revision、CAS Policy、Transition History，
  并把 promoted/canary selection 冻结到 Worker 与未租用 Execution。真实多节点 canary/rollback Release
  Gate 仍未关闭，因此 Workflow 状态保持 `partial`。
- `000032`–`000036` 均有 PostgreSQL migration integration 与 SQLite safety 测试；`000037` 覆盖
  Revision/Policy/Transition 不变量、Worker/Execution release shape 与多 Revision Target；`000038` 在
  PostgreSQL 和 SQLite 两侧断言四个 Credential Binding 外键索引。Runtime `AutoMigrate` 只服务 SQLite
  metadata store，PostgreSQL 仍以 checked-in forward migration 为唯一权威。

### 2026-07-15 Terminal/Acceptance evidence update

- Workflow I 的 shared deterministic fixture 现以 escape-free 63 KiB Chunk 产生 `2 MiB + 257 B` Terminal
  Stream。当前工作区的 Local 12/12 与 Docker 14/14 产品路径运行均验证：Session Event 只保留准确的
  32 KiB Preview；Artifact Reference 为 `0 / 1 MiB / 2 MiB`，长度为 `1 MiB / 1 MiB / 257 B`；三个
  Ready `terminal_log` 的 Size/SHA-256、Completion Total/Exit Code 均匹配，且 Event 不含 Runtime Output
  物理路径。Fixture 时间改为进程启动时钟，避免固定日期在 24 小时后让 Interaction 立即过期。
- Workflow L 的 Docker Worker smoke gate 改用 non-login shell，保留 Image `PATH` 中的 Codex/Claude CLI。
  Docker 运行还通过 Managed Replacement、Workspace 连续性、Control Plane Restart 和后续 Turn，并精确
  清理本次 Container、Volume、Network 与自动构建 Image。该证据来自未提交工作区；最终 Commit 后需
  重新生成报告，更新后的 Suite 也尚未在 SSH/Kubernetes 重跑。
- Workflow I/L 仍为 `partial`：standalone Generated File 与 Large Diff 均已有 clean Local Codex/Claude
  完整矩阵证据；clean commit `253052aa` 也已将 product/capability 与 failure 四份报告聚合为真实
  consolidated Local release gate。真实 Codex/Claude lossless 大日志、长 Session 与 SSH/Docker/Kubernetes
  Release Gate 尚未关闭。

### 2026-07-16 Release documentation and real Local adapter smoke update

- Clean commit `fb9e25ec` 上，真实 Codex App Server 与 Claude Agent SDK 使用共享
  `real-provider-smoke` 分别通过 Local 12/12。两条路径都经过用户 API、Control Plane、LocalSupervisor、
  agentd、Worker Protocol 和真实 Provider Host；第一 Turn 使用 `authoritative-history / cursor_absent`，
  Control Plane restart 后第二 Turn 使用 `native-cursor / cursor_usable` 并精确重复上一轮 marker。Codex
  Session Sequence 为 `1..42`，Claude 为 `1..41`；报告均记录 `worktreeDirty=false`、精确 cleanup 和零
  Secret finding。该结果是 narrow Local two-Turn smoke，不是完整 Local 或四 Target Release Gate。
- Clean commit `0b3f9214` 将共享 Runner 扩展为 8 个可组合真实能力 case。Codex 为 20 pass；Claude 为
  19 pass + 1 explicit unsupported。Approval、Plan Mode User Input、Steer、Interrupt、Restart/Continuity、
  Review、Compact boundary、Rollback 和 Fork 均经过真实产品路径；Codex Review/Compact 为 native，Claude
  Review 为只读 emulated、Compact 为不改变 Session Sequence 的稳定 `capability_unsupported`，Rollback/Fork
  为 Worker-free Control Plane emulation。两份报告均记录 `worktreeDirty=false`、精确 cleanup 和零 Secret
  finding。详见 `docs/reports/stage-3-real-provider-local-control-matrix-0b3f9214.md`；该结果关闭已实现能力的
  Local control/capability matrix，但不关闭真实 Provider 大输出、故障、四 Target 或 soak Release Gate。
- Clean commit `f1b1aa53` 将 canonical matrix 扩展到第 9 个 `terminal-large` case。Deterministic Fixture
  继续严格校验精确 `2 MiB + 257 B`、32 KiB Preview、三个 Ready Artifact、Size/SHA-256 和物理路径隔离。
  真实 Codex `0.144.x` 明确记录为 Explicit Unsupported：默认 `unified_exec` 只保留 1 MiB Head/Tail，且
  不能为单个 Turn 禁用它而破坏 durable Approval 与跨 Turn Cursor 语义。Claude ambient OAuth 同一 case
  也为 Explicit Unsupported，继续要求 controlled Credential 才能安全绑定 Runtime Output Root；不读取或
  复制 ambient Credential，不接受 root 外路径。两个边界均不会被伪装为 lossless pass。
- 第 10 个 canonical case `generated-file-checkpoint` 的真实 Codex/Claude Local 完整 matrix 均通过：精确
  `1 MiB + 257 B` 文件在 Execution 完成前形成 Dirty/Created/Artifact Ready/Checkpoint Ready 顺序；Runner
  通过用户下载授权重新读取 `workspace_snapshot`，验证安全 Tar、目标相对文件、已知 Runner 哨兵、
  Size/SHA-256、无重复 Ready 与无物理路径泄漏。Codex 为 `21 pass + 1 unsupported`，Claude 为
  `20 pass + 2 unsupported`（Compact 与 lossless Terminal）；两份 cleanup 和 Secret scan 均通过。该证据
  只关闭 Local Workspace Checkpoint 捕获，standalone `generated_file` ArtifactCandidate、大 Diff、
  Retention 并发和跨 Target Gate 仍保持开放。详见
  `docs/reports/stage-3-real-provider-local-generated-file-matrix-f1b1aa53.md`。
- Clean commit `be919393` 继续使用同一个第 10 case，但把 standalone Artifact 与 Checkpoint 分成独立
  边界。Codex 仅接受成功完成的原生 `fileChange`，Claude 仅接受成功 native file-tool `PostToolUse`；Host
  不解析 shell、不扫描 Workspace。两份 clean-worktree report 均在 `workspace.dirty` 前得到唯一 Ready
  `generated_file`，通过用户授权下载后验证 `43 B` 固定 payload、SHA-256、Metadata 和无路径泄漏；随后
  `workspace_snapshot` 仍按严格顺序 Ready。Codex 为 `21 pass + 1 unsupported`，Claude 为
  `20 pass + 2 unsupported`，cleanup 与 Secret scan 均通过。该证据关闭 Local standalone Generated File，
  不关闭大 Diff、真实 failure、跨 Target、Retention 或 soak。详见
  `docs/reports/stage-3-real-provider-local-standalone-generated-file-matrix-be919393.md`。
- Clean commit `90fae52c` 新增并通过第 11 个 `large-diff` case。Codex 完整矩阵为
  `22 pass + 1 unsupported`，下载验证 `320,258 B` Ready `diff` 与 5,000 deletions；Claude 为
  `21 pass + 2 unsupported`，通过 canonical realpath alias 和原生 Write fallback 下载验证 `320,201 B`、
  1 addition、5,000 deletions。两者均满足
  `artifact.ready -> turn.diff.updated -> execution.completed`、无 inline 大 Payload、无物理路径、restart、
  cleanup 与零 Secret finding。详见
  `docs/reports/stage-3-real-provider-local-large-diff-matrix-90fae52c.md`。
- Clean commit `61e38f4f` 新增独立 real Provider failure matrix，避免受控故障污染既有 11-case 能力矩阵。
  Codex 与 Claude Agent 均以 Node.js `24.13.1` 通过 `16/16`：loopback 401/429 保留
  `authentication_required` / `provider_rate_limited`，scoped Host crash 只杀隔离 Control Plane 子树内唯一
  `--protocol-v2` 进程，Cursor 通过 `1s` policy 自然过期并在 restart 后选择
  `authoritative-history / cursor_expired`。每个 401/429/crash 后的新 Execution 均恢复，cleanup 与 output
  Secret scan 均通过。Codex controlled Credential 使用 execution-local `CODEX_HOME`；Claude 对稳定 SDK
  `api_retry` 结束隐藏的 401/429 重试。详见
  `docs/reports/stage-3-real-provider-local-failure-matrix-61e38f4f.md`。
- Clean commit `253052aa` 新增并通过 consolidated Local release gate。聚合器要求完全 clean worktree，使用
  Node.js `24.13.1` 从当前 checkout 重建 Provider Host，并分别运行 Codex/Claude product 与 failure 四份
  报告；最终为 Codex `22 pass + 1 unsupported`、`16/16`，Claude `21 pass + 2 unsupported`、`16/16`。
  四份报告共享同一 Git SHA 与 Capability Catalog hash，无 fail/skipped，只有冻结的 Explicit Unsupported，
  cleanup 和 output Secret scan 全部通过。首次聚合尝试发现 Codex Approval Turn 未调用工具却终止；
  Runner 现对 terminal-without-interaction 立即返回 `runner.interaction_missing_after_terminal`，不自动重试或
  放宽断言。详见 `docs/reports/stage-3-real-provider-local-release-gate-253052aa.md`。
- 首次真实 Claude 产品路径运行发现 Execution-local `CLAUDE_CONFIG_DIR` 会让已登录的 ambient OAuth
  不可见。Provider Host 已区分 controlled Credential 与 ambient authentication：前者保留 Runtime Output
  Root 隔离，后者保留用户 Claude 配置查找路径；Provider Host 全量测试与真实 clean-commit smoke 均通过。
- 当前 dirty worktree 的 deterministic failure-only reports 通过 Local malformed/oversized/crash、Docker
  Worker network interruption，以及 owned Kind 上的 Worker network、Node drain、Pod eviction 和 image
  canary；所有运行的 cleanup 与 output Secret scan 均通过。Kind image canary 使用同内容 alias，不是
  `000037` immutable Release Revision rollout，也不替代真实 Provider gate。
- 2026-07-14 的 SSH 13/13 和 clean commit `2763ebd3` Kubernetes 13/13 仍只作为历史 fixture 证据；真实
  Codex/Claude consolidated Local 已在 `253052aa` 关闭，SSH/Docker/Kubernetes Release Acceptance 仍待完成。
- 新增 Stage 3 发布检查单、Worker Release rollout Runbook 和当前验收报告：
  `docs/release-checklists/stage-3-provider-runtime-remote-worker.md`、
  `docs/runbooks/worker-release-rollout.md`、
  `docs/reports/stage-3-provider-runtime-acceptance-fb9e25ec.md`。早期 dirty-worktree/fixture 汇总仍保留在
  `docs/reports/stage-3-provider-runtime-acceptance-2026-07-15.md`；所有文档继续保持 Release Gate open。

### 2026-07-17 Registry release-gate implementation update

- 新增 `scripts/stage3-provider-acceptance/registry_release_gate.py`，在 clean SHA 上执行 cached 与 no-cache
  两次双架构 Registry push。Gate 要求 `docker-container` Buildx builder，精确验证 Registry-returned OCI
  index、`linux/amd64`/`linux/arm64` manifest、每平台 SPDX/SLSA attestation、non-root config，以及 Image 内
  Manifest、normalized SBOM、npm/Bun/APK lockfile、Provider Host 与 agentd evidence。
- `deploy/worker/build.sh` 新增可选 credential-free `--go-proxy`，只接受 HTTPS、`direct` 或 `off`，拒绝
  userinfo、query、fragment、空白和控制字符。该值只用于 Go build stage；Credential 不进入参数、报告或
  Image runtime environment。
- `deploy/worker/buildkit-sbom-generator.lock` 将 BuildKit Syft scanner 固定到 provenance 已记录的 immutable
  digest，避免 release build 重新解析 mutable `stable-1` tag；Gate 报告同时记录该 scanner reference。
- Registry exporter 使用 `rewrite-timestamp=true` 将生成 layer 统一到 `SOURCE_DATE_EPOCH`；APK install 在
  同层删除含运行时间的 `/var/log/apk.log`，raw npm SBOM 通过只读 BuildKit mount 输入 normalization，不再
  以 transient COPY layer 留在最终 Image history。Worker rootfs 以 clean Git SHA build-revision marker
  隔离跨 Commit cache，agentd、Provider Host 与 Provider tools 的跨 stage mtime 也固定到同一 epoch。锁定
  npm `12.0.1` 替换基础镜像旧 npm 后，同层删除 npm 产生的 `/tmp/node-compile-cache`，避免 cached/no-cache
  rootfs 漂移。
- 新增 digest-pinned Cosign `v3.1.1`、Trivy `0.72.0`、checked-in signing policy 与 vulnerability policy。
  Signing policy 支持 `ephemeral-key`、OIDC `keyless` 和 AWS/GCP/Azure/Vault `kms-key`。生产模式拒绝
  `--insecure-registry`、强制 transparency log、要求 certificate identity/issuer 或 KMS reference，并把
  OIDC token 放入 gate-owned `0600` 文件后删除；KMS Credential 仅按 allowlisted 环境变量名传入，值不进入
  Docker 参数或报告。Gate 同时阻断 `HIGH,CRITICAL`、Secret、EOSL 与超过 24 小时的 Trivy DB。
- 首次 gate 在完成双平台 reproducibility 与签名后因 Trivy DB OCI 下载 `unexpected EOF` fail closed；后续
  仅对明确的 DB 下载瞬时网络错误增加一次有界重试，finding、过期 DB、无效报告、策略失败和第二次下载
  失败均不重试。Registry release/supply-chain tests `40/40`、全部 release-gate tests `90/90` 与 Stage 3
  Python `193/193` 已通过。
- clean commit `7659dd5f` 的正式 gate 已通过。cached/no-cache OCI index digest 分别为
  `sha256:912223cb...`、`sha256:630bff03...`；两次构建的 `linux/amd64` manifest 均为
  `sha256:2d0b9d8a...`，`linux/arm64` manifest 均为 `sha256:7fd11ce0...`。两平台 SPDX/SLSA、non-root
  config、嵌入 Manifest/SBOM/lockfile/runtime、ephemeral exact-digest signature、`HIGH/CRITICAL=0`、Secret=0、
  非 EOSL、DB freshness、精确 cleanup 与输出 Secret scan 均通过。`GO-2026-5932` 作为未豁免的不可达
  `UNKNOWN` finding 保留；agentd 不导入 `x/crypto/openpgp`，`govulncheck v1.6.0` 报告受影响漏洞为 `0`。
  完整证据见 `docs/reports/stage-3-worker-registry-signing-policy-7659dd5f.md`。
- 该证据关闭 production signing implementation path，以及 disposable Registry 上的 clean-SHA multi-arch
  reproducibility、ephemeral signing mechanics 与 vulnerability-policy slice；真实生产 KMS/keyless identity、
  transparency log/admission policy、Registry
  retention/Credential、四 Target Provider rollout、canary/rollback 与 soak 尚未完成，Workflow J/L 继续
  保持 `partial`。本切片没有 DDL 变更，migration boundary 仍为 `000041`。

### 2026-07-17 Managed Docker Worker Release rollout evidence update

- 新增正式 `docker_worker_release_rollout_gate.py`。Clean commit `d3af9380` 从同一源码构建并推送
  `0.5.4+rollout.baseline` 与 `0.5.4+rollout.candidate` 两个不同 Registry Digest，创建 immutable Revision
  `cfe5cc3c-...` 与 `d69a4770-...`，并经正式 API 完成 Policy `1 promote -> 2 canary -> 3 promote -> 4 rollback`。
- baseline Approval Execution 持有 active Lease 时，原容器 `95b188cab700` 与 Generation `1` 保持不变，Busy
  baseline 不占 canary slot，Target 不被误标 `offline`；candidate promote 精确返回
  `409 worker_release_active_executions` 并指向 baseline / `promoted`。Execution 终态后旧容器才替换为
  `39e80c66459b`。candidate canary 上的第二个 active Approval Execution 同样阻断 promote/rollback。
- 每个阶段的 Container、Worker Manifest、Execution Revision/Channel 与 Registry Digest 完全一致；四个
  Execution 的 Session Event 形成连续 Sequence `1..33`，无双执行、无重复终态。Audit 保留 `2` 条 Revision
  与 `4` 条 Policy entry，Outbox `6` 条全部 published，Transition 与最终 Policy 一致。
- 正式 gate `15/15`、rollout gate unit `18/18`、全部 release-gate tests `108/108`、Stage 3 Python `211/211`；
  `services/control-plane` 全量 Go tests 通过。Cleanup 精确删除三个 Worker container、Registry、三个 volume、
  network、两个 Worker image slot 与 state，`broadCleanupUsed=false`；Secret scan 覆盖 17 files / 396,520 bytes，
  finding 为零。完整证据见 `docs/reports/stage-3-worker-release-rollout-d3af9380.md`。
- 该结果关闭 deterministic managed Docker immutable rollout 与 Busy Worker completion/fencing mechanics，不关闭生产 Registry TLS/auth/retention、
  真实 keyless/KMS identity/tlog/admission、真实 Codex/Claude Docker/SSH/Kubernetes Credential gate、Kubernetes
  多节点 rollout、Busy Worker load/failure injection 或 soak。Workflow J/L 因此继续保持 `partial`。本切片没有 DDL
  变更，migration boundary 仍为 `000041_diff_artifact_kind.sql`。

### 2026-07-17 Deterministic Local long-Session soak evidence update

- `acceptance_runner.py` 新增复用现有 Target Driver、Provider fixture、报告、cleanup 与 Secret scan 的
  `fixture-soak`，默认在 core/restart/second-Turn 后追加 `100` Turn，并每 `10` Turn 在仍有后续 Turn 时重启
  Control Plane；没有创建第二套 soak Target/Provider 框架。
- Clean commit `6e866a30` 的 Local run 完成 `100/100` 唯一 Execution 与 `9` 次额外 restart；soak 新增
  `1,300` 条 Event，完整 Session Sequence 连续 `1..1371`，500-event pagination 实际触发。每个 soak Turn
  都包含 Text、Tool、Usage、Workspace dirty、Artifact、Checkpoint Ready 与一个 `execution.completed`；
  `doubleExecution=false`、`duplicateTerminal=false`。
- 全量 run 产生 `105` 个 ready Checkpoint、`109` 个 Ready Artifact；15/15 report cases、Runner unit
  `106/106` 与 Stage 3 Python `218/218` 通过。Cleanup 删除 isolated state；Secret scan 覆盖 14 files /
  1,542,440 bytes，finding 为零。完整证据见 `docs/reports/stage-3-local-fixture-soak-6e866a30.md`。
- 该证据关闭 deterministic Local long-Session、重复 Control Plane/Worker reconnect、Event pagination 与
  repeated Checkpoint mechanics；真实 Provider 长 Session、multi-Provider/multi-Session concurrency、Retention
  concurrency、remote Target、load 与 production-duration soak 仍 open，Workflow L 保持 `partial`。本切片
  没有 DDL 变更，migration boundary 仍为 `000041_diff_artifact_kind.sql`。

### 2026-07-17 Deterministic managed Docker Provider concurrency evidence update

- `acceptance_runner.py` 新增 `fixture-concurrency`，只接受 managed Docker，固定一个 Target、两个 agentd
  Worker、Codex/Claude 两个 fixture Provider 和 Tenant 并发配额 `2`。Credential、Project/Session、Target
  discovery、报告、cleanup 与 Secret scan 均复用现有实现；Docker Driver 只扩展为观察多个 managed
  container，没有引入第二套编排或冗余 container-name 状态。
- Clean commit `eeb7a2f1` 的 canonical run 以两个同时 pending Approval 为屏障，记录两个唯一 Session、两个
  唯一 Execution 和两个不同 Worker。每个活跃 Execution 的 `turn.created`、`execution.leased`、
  `workspace.ready`、`execution.started` 与 `request.opened` 都恰好一条且无终态；先 Resolve Claude 后 Codex
  仍 pending，最后两边各只有一个 `execution.completed`，`doubleExecution=false`、
  `duplicateTerminal=false`。
- 正式 report `9/9` 通过；Runner unit `113/113`、Stage 3 Python `225/225`、原 Docker fixture `16/16`、
  `bun fmt`、`bun lint`（0 errors / 238 existing warnings）与 `bun typecheck`（9/9）均通过。Cleanup 精确删除
  两个 Worker container、volume、network、Image 与 state，owner 资源零残留；Secret scan 覆盖 10 files /
  88,040 bytes，finding 为零。完整证据见
  `docs/reports/stage-3-docker-fixture-concurrency-eeb7a2f1.md`。
- 该证据只关闭 deterministic managed Docker multi-Provider/multi-Session overlap mechanics；真实 Codex/Claude
  concurrency、真实 SSH/Docker/Kubernetes gate、Retention/Cleanup concurrency、load、multi-node 与
  production concurrency 仍 open，Workflow L 保持 `partial`。本切片没有 DDL 变更，migration boundary
  仍为 `000041_diff_artifact_kind.sql`。

### 2026-07-17 Deterministic Local Retention/Cleanup concurrency evidence update

- `acceptance_runner.py` 新增只允许 isolated Local Target 的 `fixture-retention-concurrency`，复用现有 Runner、
  Provider fixture、报告、Secret scan、Control Plane、Local agentd 与 cleanup。Fixture 创建 terminal generated
  Artifact 与 Ready Workspace Checkpoint，再让第二个 Turn 停在 pending Approval，并启用真实 Tenant retention
  policy；只老化 runner-owned SQLite 行，不修改生产时钟。
- Clean commit `c27914da` 的 active sweep 中，Session 保持 active，Execution 为 `waiting-for-approval` 且 Lease
  为 1，Interaction 保持 pending，Workspace generation 与 seed/current Ready Checkpoint 保留，cleanup command
  为 0；同时无引用旧 Artifact 被安全删除，证明不是全局暂停 Retention。终态后 Approval Execution 单终态完成，
  Session archived，新的 current Checkpoint 与 seed Checkpoint 均 Ready；唯一 cleanup command 以 generation 1、
  attempt 1 被 agentd acknowledged，物理 Workspace generation 删除。
- 正式 report `9/9` 通过；Runner unit `118/118`、Stage 3 Python `230/230`、focused Go、原 Local fixture、
  `bun fmt`、`bun lint`（0 errors / 238 existing warnings）与 `bun typecheck`（9/9）均通过。Cleanup 删除 isolated
  state；Secret scan 覆盖 4 files / 71,518 bytes，finding 为零。完整证据见
  `docs/reports/stage-3-local-fixture-retention-concurrency-c27914da.md`。
- 该证据只关闭 deterministic Local active-Execution/Retention/physical-cleanup mechanics；真实 Provider、remote
  Target、multi-node、load、生产时长与生产 Retention 仍 open，Workflow H/L 保持 `partial`。本切片没有 DDL
  变更，migration boundary 仍为 `000041_diff_artifact_kind.sql`。

### 2026-07-17 Deterministic managed Docker bounded load/admission evidence update

- `acceptance_runner.py` 新增 `fixture-load`，泛化现有 managed Docker concurrency 路径并复用两个 agentd
  Worker、Codex/Claude fixture、Credential delivery、Project/Session API、报告、Secret scan 与精确 cleanup；没有
  创建第二套 load Target Driver、Provider fixture 或资源 owner。
- Clean commit `e944b449` 固定四个 Session、Tenant 并发配额 `2` 与 `25` 波次。每波四个 Approval Turn 都包含
  Text、Tool、Usage、Credential、generated Artifact 与 Ready Checkpoint；三个观察点各要求两个 pending
  Execution 位于不同 Worker。正式结果为 `100/100` 唯一 Execution、`50/50` quota rejection 精确返回
  `execution_quota_exceeded` 且不改变 Session Event/Interaction、释放槽位后 `50/50` 重试成功、`75/75` overlap；
  Codex/Claude 各 50、四 Session 各 25，Artifact Ready `200`、Checkpoint Ready `100`，无双执行/重复终态。
- 正式 report `9/9` 通过；Runner unit `126/126`、Stage 3 Python `238/238`、原 Docker concurrency fixture
  `9/9`、quotas/sessions/executions/agentd focused Go tests、`bun fmt`、`bun lint`（0 errors / 238 existing warnings）
  与 `bun typecheck`（9/9）均通过。Cleanup 精确删除两个 Worker container、volume、network、Image 与 state，
  owner 资源零残留；Secret scan 覆盖 10 files / 2,513,498 bytes，finding 为零。完整证据见
  `docs/reports/stage-3-docker-fixture-load-e944b449.md`。
- 该证据只关闭 deterministic bounded managed Docker load/admission、slot reuse、双 Worker overlap 与 durable
  Artifact/Checkpoint terminal mechanics；真实 Provider、multi-host/Kubernetes multi-node、failure injection under
  load、生产 SLA 与 production-duration load/soak 仍 open，Workflow L 保持 `partial`。本切片没有 DDL 变更，
  migration boundary 仍为 `000041_diff_artifact_kind.sql`。

### 2026-07-17 Deterministic managed Docker targeted failure-under-load evidence update

- `acceptance_runner.py` 新增 `fixture-load-failure`，复用同一 managed Docker 双 Worker、四 Session、quota/load、
  Provider fixture、报告、Secret scan 与 cleanup 路径；没有创建第二套 Target Driver、Provider fixture 或资源
  owner。Runner 从 isolated metadata 通过 `agent_executions.worker_id -> worker_instances.pod_name` 定位 exact
  busy Worker，再要求该 pod name 只匹配一个带正确 Target 与 `synara.io/worker-index` 标签的 managed container。
- Clean commit `ab88798d` 先保持 Codex/Claude 两个 Session 同时 pending，另两个 Session 精确收到无副作用
  `execution_quota_exceeded`；只断开目标容器的 runner-owned network。Peer Session 的 Event、pending Interaction、
  Worker 与 Generation 在目标恢复和目标终态后均保持不变。受影响 Execution 保持同一 ID，
  `execution.recovering` 绑定 stale Worker/Generation 1，replacement Request/Interaction 前进到 Generation 2；
  两边各只有一个 `execution.completed`，无 pending Interaction。
- 恢复后同四 Session 完成 25 波次、`100/100` 唯一 Execution、`50/50` quota rejection/slot reuse、`75/75`
  overlap、Artifact Ready `200`、Checkpoint Ready `100`，无双执行或重复终态。正式 report `10/10`、Runner unit
  `133/133`、Stage 3 Python `245/245`、focused Go、`bun fmt`、`bun lint`（0 errors / 238 existing warnings）和
  `bun typecheck`（9/9）均通过。Cleanup 精确删除两个 Worker container、volume、network、Image 与 state，owner
  资源零残留；Secret scan 覆盖 10 files / 2,687,660 bytes，finding 为零。完整证据见
  `docs/reports/stage-3-docker-fixture-load-failure-ab88798d.md`。
- Clean commit `7684c6d8` 在同一 suite/Target/resource owner 中增加第二个 failure barrier：轮换 Session 顺序后
  精确删除另一个 busy Worker container。Docker reconciler 以同 logical Worker name/ID 创建 replacement，
  container ID 与 instance UID 变化、Worker incarnation `1 -> 2`、named-volume sentinel 保留；Peer Session 的
  Event/Interaction/Worker/Generation 仍不变，受影响 Execution 仍只前进 Generation 1 -> 2 且只有一个 terminal。
  随后的同四 Session 25 波次 load 仍为 `100/100` Execution、`50/50` quota rejection/slot reuse、`75/75`
  overlap、Artifact Ready `200` 与 Checkpoint Ready `100`。正式 report `11/11`、Runner unit `134/134`、Stage 3
  Python `246/246`、focused Go（含 `executiontargets`）、Bun 三项均通过；Secret scan 覆盖 10 files /
  2,733,690 bytes，finding 为零。完整证据见
  `docs/reports/stage-3-docker-fixture-load-failure-7684c6d8.md`。
- Clean commit `cfecba63` 增加第三个 failure barrier：从 affected Execution 精确定位 busy Worker container，
  再仅向 agentd descendant 中唯一 `--protocol-v2` Provider Host 发送 `SIGKILL`。Control Plane 的 Worker failure
  path 同时修复 `waiting-for-approval` 终态边界：原 Generation pending Interaction 在同一事务中
  expired/superseded，Execution 只产生一次 `execution.failed(provider_unavailable)`，不进入
  `execution.recovering`；随后新的 Execution 在同一 logical Worker 上完成，Peer Session 的
  Event/Interaction/Worker/Generation 全程不变。
- Clean report `12/12` 通过；Provider process case 记录 `failedTerminalCount=1`、new Execution ID、same Worker ID、
  `recoveryAdmissionMs=2335` 与零 pending Interaction。随后 25 波次仍为 `100/100` Execution、`50/50` quota
  rejection/slot reuse、`75/75` overlap、Artifact Ready `200` 与 Checkpoint Ready `100`。Runner unit
  `135/135`、Stage 3 Python `247/247`、focused Go（含 `executiontargets`）、`bun fmt`、`bun lint`（0 errors /
  238 existing warnings）与 `bun typecheck`（9/9）均通过；Secret scan 覆盖 10 files / 2,810,588 bytes，finding
  为零。完整证据见 `docs/reports/stage-3-docker-fixture-load-failure-cfecba63.md`。
- 该证据只关闭 deterministic single-host managed Docker exact network/container-loss/fixture Provider-process
  failure targeting under load、same logical Worker replacement、Peer 隔离、Generation/incarnation fencing、
  new-Execution recovery 与 post-failure bounded load mechanics；真实 Provider、multi-host/Kubernetes multi-node、
  real Provider-process/release-rollout failure、生产 SLA 与 production-duration load/soak 仍 open，Workflow L
  保持 `partial`。本切片没有 DDL 变更，migration boundary 仍为 `000041_diff_artifact_kind.sql`。

### 2026-07-17 Deterministic managed Docker release-rollout failure/load evidence update

- `docker_worker_release_rollout_gate.py` 复用同一四 Session load helper，不创建第二套 Driver/fixture。原 busy
  baseline Approval 保持 pending 后启动 candidate/canary Approval，Tenant quota `2` 形成两个 Worker/两个
  Revision 的真实 overlap，另两个 Session 精确收到无副作用 `execution_quota_exceeded`。
- Clean commit `41683366` 精确删除 candidate Execution 容器 `9a5b84072fdc`；replacement
  `a858262a7566` 保持同 Execution、logical Worker、candidate Revision、`canary` Channel、Manifest、Registry
  digest 与 named-volume content，Generation `1 -> 2`，Worker incarnation 前进且 instance UID/Request/Interaction
  更换。Baseline peer 的 Event、pending Interaction、Worker、Generation 和容器不变；恢复 pending 窗口内
  promote/rollback 均以 `worker_release_active_executions` fail closed。
- Candidate promote 后完成 `13` 波/`52` Execution，baseline rollback 后完成 `12` 波/`48` Execution；合计
  `25` 波、`100/100` load Execution、`50/50` quota rejection/retry、`75/75` overlap、`100/100` active release-pin、
  `100/100` terminal release-pin 与 `100/100` Worker binding。四 Session 总计 `102` 个唯一 Execution/单终态，
  Sequence 分别连续为 `1..401`、`1..401`、`1..412`、`1..417`。
- Load 暴露并修复 release history 的可见性边界：Audit gate 现在有界 cursor 分页并拒绝重复 Event/cursor，正式
  run 读取 `229` 条/`2` 页后定位 `2` Revision + `4` Policy audit；Outbox 管理 API 新增校验后的
  `topicPrefix=worker.release.` 服务端过滤，正式 run 精确返回 `6` 条 published release message。没有提高
  `limit` 或读取私有 metadata database。
- 正式 run `stage3-worker-release-rollout-5291349a-e9fc-425c-950d-bc6eb3c5bb1d` 为 `15/15`；rollout unit
  `23/23`、Runner unit `136/136`、Stage 3 Python `253/253`、focused Go（含 outbox/httpapi/workerreleases）、
  `bun fmt`、`bun lint`（0 errors / 238 existing warnings）与 `bun typecheck`（9/9）通过。Secret scan 覆盖
  17 files / 3,206,778 bytes，finding 为零；cleanup 精确删除三个 Worker container、Registry、三个 volume、
  network、两个 image slot 与 state，owner 资源零残留。完整证据见
  `docs/reports/stage-3-worker-release-rollout-load-41683366.md`。
- 该证据关闭 deterministic single-host immutable release-rollout container-loss recovery under bounded load、
  promote/rollback release pins 与 load-safe Audit/Outbox mechanics；真实 Provider、外部 SSH、Kubernetes
  real-Provider/multi-node、生产 Registry/KMS identity/tlog/admission、生产 SLA 与 production-duration load/soak
  仍 open。获批外部 SSH target 已有 fail-closed/non-destructive Runner 与 Gate 实现，但尚无安全本机 identity 下的
  clean-SHA 四矩阵报告；本地 `orbstack` context 已有 clean commit `1e826324` 的 deterministic `19/19` 报告，
  不再列为“缺少获批 context 证据”，但它不替代真实 Provider Kubernetes Gate。生产并发由 quota/Worker
  slot/CPU/内存资源档位控制，数值 latency/error/duration SLA 仍待定义。Workflow J/L 保持 `partial`；本切片
  没有 DDL 变更，migration boundary 仍为 `000041_diff_artifact_kind.sql`。当前 Acceptance Runner `159/159`、
  SSH Gate `15/15`、Stage 3 Python `281/281` 与 Control Plane `go test ./...` 均通过。

### 2026-07-18 Real Docker third-party Provider gate evidence update

- Clean SHA `f958c1b2` used the controlled third-party Codex/Claude API-key and Base-URL path to run the complete
  Docker release aggregate. All four children used the same Capability Catalog and one gate-owned Worker Image built
  once from the clean source; child builds were disabled, and every child used an isolated network, Workspace volume,
  state directory, one CPU and `2 GiB` memory.
- Codex controlled failure passed `16/16`. Codex product passed the baseline real Turn, then the required
  approval-mode Turn completed without `command_execution`, `request.opened` or an Approval Interaction and failed
  immediately as `runner.interaction_missing_after_terminal`; later product cases remained prerequisite failures.
- Claude product and failure each reached the Docker Worker and failed their required baseline Turn with stable
  `provider_unavailable`: `Claude Agent SDK API request failed with HTTP 502.` This matches the earlier Kubernetes and
  Local diagnosis for the same profile and does not identify a Docker scheduling, startup or Credential-delivery
  regression.
- All four child cleanup cases passed; the gate ownership- and ID-verified the shared Image before exact deletion.
  Post-run inspection found no managed Worker container or gate Image. The aggregate Secret scan covered `37` files
  and `4,971,618` bytes with zero findings, and neither Credential values nor operator environment-variable names were
  persisted. The gate remains open; full evidence is
  `docs/reports/stage-3-real-provider-docker-third-party-gate-f958c1b2.md`.
- This evidence increment changes no DDL. The migration boundary remains `000041_diff_artifact_kind.sql`; a
  tool-capable Codex profile and a Claude streaming endpoint without HTTP `502` are required before rerunning the
  Docker gate as release evidence.

### 2026-07-18 Resource-profiled minimum-duration load measurement update

- The existing `fixture-load` orchestration now accepts a minimum measured load duration and a maximum-wave safety
  bound. It runs complete waves until both minimum waves and duration are satisfied; exhausting the bound returns
  `runner.load_duration_not_reached`. Existing rollout helpers pass explicit segments and retain exact wave-count
  semantics.
- Reports now include Tenant quota, Worker/slot count, Docker CPU/memory limits, effective concurrency, admission
  attempts, expected quota rejection rate, successful retries, Execution success rate, unexpected failure/error rate,
  throughput and nearest-rank P50/P95/P99 for full-wave and released-slot admission recovery latency. The report also
  states whether an operator-approved SLA threshold was enforced; the current value remains false.
- Clean SHA `e2d70fb6` required at least `25` waves and `300s` under two `1 CPU/2 GiB` Workers, quota `2` and four
  Sessions. It completed `56` waves, `224/224` Executions, `112/112` quota rejection/retry and `168/168` overlap in
  `304.201s`; success rate was `1.0`, unexpected error rate `0.0` and throughput `0.736` Executions/s. Wave P95/P99
  were `6.493s/6.826s`; admission recovery P95/P99 were `1.372s/1.386s`.
- All `9/9` cases, exact container/network/volume/image/state cleanup and the `10` files / `5,874,277` bytes Secret
  scan passed. Runner unit tests are `164/164`, Stage 3 Python is `298/298`, and `bun fmt`/`bun lint`/`bun typecheck`
  passed. Full evidence is `docs/reports/stage-3-docker-resource-profiled-load-e2d70fb6.md`.
- This closes deterministic resource-profiled duration/percentile/error measurement mechanics only. Production
  duration and P95/P99/error/recovery thresholds remain unapproved; real Provider, production Kubernetes multi-node,
  external SSH and production KMS/Registry evidence remain open. No DDL changed; the migration boundary remains
  `000041_diff_artifact_kind.sql`.

### 2026-07-18 Structured User Input recovery and fixture Credential update

- Clean SHA `b07e5bd9` generalizes the execution-pinned pending-interaction recovery path without adding a second
  Target implementation. Approval retains its prior API boundary; Structured User Input uses its canonical
  `user-input.requested` Event, requires an exact replacement question payload, and polls until the stale interaction
  is `expired` with `deliveryStatus=superseded`.
- The deterministic Provider Credential now uses the supported `api_key` / `apiKey` product shape. The fixture reads
  the anonymous Credential FD, returns only key-name/boolean evidence, and rejects an incorrect Structured User Input
  answer. No new Credential type or compatibility bypass was introduced.
- The clean owned-Kind core run passed `17/17`. Pending Structured User Input Pod deletion preserved the same
  Turn/Execution, advanced Generation `1 -> 2`, replaced Request/Interaction/Worker/Pod identities, resolved only the
  Generation 2 request, and produced one terminal path. Existing Pending Approval recovery, Artifact, large Terminal,
  Provider Error, Control Plane restart, second-Turn continuity, exact cleanup, and zero Secret findings also passed.
  Full evidence is `docs/reports/stage-3-kubernetes-structured-user-input-recovery-b07e5bd9.md`.
- Web coverage adds `116/116` focused authority tests plus `4/4` browser tests for single-select auto-submit,
  multi-select deferral, stale timer cancellation on replacement request remount, and resolving-state disabling.
  Acceptance Runner is `170/170`, Stage 3 Python is `304/304`, Provider Host fixture is `17/17`, and
  `bun fmt`/`bun lint`/`bun typecheck` pass.
- This closes deterministic Kubernetes Structured User Input runtime replacement mechanics only. SaaS two-page User
  Input convergence/concurrent resolve, real Provider remote interactions, real SSH/Docker/Kubernetes aggregates,
  approved production SLA thresholds, and concrete production KMS identity/tlog/admission remain open. No DDL changed;
  the migration boundary remains `000041_diff_artifact_kind.sql`.

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
16. `000032_session_advanced_operations.sql`: immutable Compact/Review/Rollback/Fork Turn shapes, logical history ancestry and one primary Control Command per Worker-executed advanced Turn.
17. `000033_provider_credential_scopes.sql`: Tenant/Organization/User/Platform Credential scopes, selector policy, auto-selection controls and AAD migration state.
18. `000034_worker_revocation_fencing.sql`: terminal operator revocation, logical Worker identity tombstones and Lease/Claim/re-registration fencing.
19. `000035_workspace_credential_bindings.sql`: Project/Execution Target Workspace Credential Bindings and immutable per-Generation Execution Grants.
20. `000036_project_git_binding_authority.sql`: rolling compatibility backfill followed by retirement of `projects.git_credential_id` as a writable authority.
21. `000037_worker_release_rollout.sql`: immutable Worker Release Revisions, CAS promoted/canary Policy, transition history and release-pinned Worker/Execution scheduling state.
22. `000038_credential_binding_fk_indexes.sql`: non-partial lookup indexes required by Credential Binding foreign-key enforcement, including disabled history.
23. `000039_worker_image_pull_binding_uniqueness.sql`: one active image-pull Binding per Target with fail-closed upgrade validation.
24. `000040_worker_release_transition_policy_fencing.sql`: current Policy and latest immutable Transition consistency fencing.
25. `000041_diff_artifact_kind.sql`: forward-only extension of `artifacts_kind_check` with the standalone `diff` kind.

An integration test applies `000001`–`000016`, seeds legacy Cursor/Execution/Interaction/Repository state, then
upgrades through the current migration set and verifies every backfill. Dedicated migration coverage also verifies that
`000031` fails closed when duplicate active Session Executions already exist and does not record a failed migration as
applied. Dedicated PostgreSQL and SQLite coverage verifies `000032` advanced-operation graph/command invariants,
`000033` Credential scope backfill and shape, `000034` revocation/tombstone fencing, `000035` Binding/Grant ownership
and generation fencing, `000036` legacy Git authority retirement, `000037` Release Revision/Policy/Transition and
Worker/Execution selection shape, the four `000038` foreign-key indexes, `000039` image-pull Binding uniqueness and
`000040` Policy/Transition fencing. `000041` has a dedicated PostgreSQL 17 integration test that proves the old
boundary rejects `diff` and the upgraded constraint preserves every existing kind while adding only `diff`.
Runtime `AutoMigrate` and hand-applied PostgreSQL database mutation remain non-authoritative. The current checked-in
migration boundary is `000041`.

Individual increments can still reuse earlier schema without adding DDL. In particular, Provider capability projection
and Session model-switch reuse existing Target/Manifest/Session/Execution plus `000030`/`000031` Cursor state; those
local no-new-DDL statements do not redefine the repository-wide migration boundary.

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
3. In progress: Codex App Server and Claude Agent SDK multi-Turn, native Interrupt/Steer, Approval, Plan Mode Input and history fallback are implemented. Runtime Event v2 is canonical end to end. Cursor Envelope v2, per-Execution Provider snapshots, Cursor quarantine/lineage, the bounded expiry policy, audited Claim selection, safe Provider-native invalid/expired fallback, one active Execution per Session and pre-Claim Interrupt cancellation are implemented. Clean commit `253052aa` passes the consolidated real Local product/failure release gate with the frozen Compact/lossless-Terminal boundaries, standalone `generated_file`, Workspace Checkpoint, Artifact-backed Large Diff, real 401/429, scoped Host crash and Cursor-expiry recovery. Clean `6b71703f`/`f958c1b2` execute the Kubernetes/Docker gates but fail closed on the configured Codex approval and Claude HTTP 502 boundaries; continue with qualifying profiles and SSH acceptance.
4. In progress: Workspace/Git/Checkpoint DDL, public/private HTTPS Clone/Fetch, Git Credential, state reporting, cross-process locked cache plus private relative worktree generations, Git-reference/Patch/Snapshot capture/restore, interrupted staging/backup reconciliation, physical cleanup and Checkpoint/Artifact retention are implemented. Clean commit `c27914da` closes deterministic Local active-Execution Retention fencing and post-terminal physical cleanup mechanics; add SSH Credential delivery plus real Provider multi-Worker/Target, multi-node, load and production-duration Retention acceptance.
5. In progress: Worker Manifest, graceful Drain, disposable Registry reproducibility/supply-chain evidence and the
   deterministic managed Docker immutable canary/promote/rollback gate are implemented. Clean commit `41683366`
   additionally closes deterministic candidate container-loss recovery under promoted/canary overlap and `25`
   release-pinned waves across promote/rollback; add production signer/Registry evidence, real Provider remote
   rollout, multi-node upgrade isolation and production-duration load/soak evidence.
6. In progress: the deterministic shared Runner covers Local, Docker, SSH and Kubernetes and emits JSON/Markdown
   evidence. The SSH Driver's deterministic Codex fixture passed the 13-case live suite on 2026-07-14; clean commit
   `2763ebd3` passed the 13-case Kubernetes core suite. Current dirty-worktree failure-only runs also pass Local
   Provider faults, Docker network interruption and Kubernetes Network/Drain/Eviction/Image Canary. Re-run the
   implemented real Codex/Claude Local control/capability matrix passes on clean commit `0b3f9214`; clean commit
   `be919393` also passes the ten-case matrix, standalone generated-file Artifact and Workspace Checkpoint capture.
   Clean commit `253052aa` completes the consolidated real Local release suite across both adapters; clean commit
   `6e866a30` closes deterministic Local long-Session/restart/pagination mechanics, clean commit `eeb7a2f1` closes
   deterministic managed Docker two-Worker/two-Session overlap mechanics, clean commit `c27914da` closes deterministic
   Local active-Execution Retention fencing and post-terminal physical cleanup mechanics, and clean commit `e944b449`
   closes deterministic bounded Docker quota/admission and slot-reuse mechanics. Clean commit `ab88798d` closes
   deterministic single-host exact network-failure targeting under two-Worker load, Peer Session isolation,
   Generation 1 -> 2 fencing and full post-recovery load mechanics. Clean commit `7684c6d8` adds exact busy-container
   loss, same logical Worker replacement, incarnation fencing and named-volume continuity under the same load gate.
   Clean commit `e2d70fb6` adds resource-profiled minimum-duration and P50/P95/P99/error measurement on the shared
   load path. Clean commit `41683366` adds deterministic immutable release-rollout failure under load, load-safe Audit
   pagination and Outbox topic filtering. Clean commit `b07e5bd9` updates the owned Kind core suite to `17/17` with
   pending Structured User Input Pod-loss recovery, strict replacement-question validation and product-shaped fixture
   Credentials.
   Run both real adapters on SSH and rerun Docker/Kubernetes with qualifying profiles, then complete real Provider/remote Target
   Retention/load/failure, multi-host/Kubernetes multi-node, real Provider process/rollout evidence and
   production-duration soak before promoting any Local-only Provider or claiming the four-Target release gate.
