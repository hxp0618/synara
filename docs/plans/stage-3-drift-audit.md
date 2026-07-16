# Stage 3 Drift Audit

Baseline: `codex/saas-tenancy-user` after clean commit `253052aa`, including the consolidated real Codex/Claude
Local product and controlled-failure release gate, terminal-aware Interaction waits, the Provider Cursor expiry
policy, audited Resume selection, standalone Provider-native generated-file capture and Artifact-backed Large Diff
projection. The immutable Kubernetes deterministic Provider fixture report remains tied to `2763ebd3` and was
recorded on 2026-07-14. The latest clean-commit real Local release evidence is summarized in
`docs/reports/stage-3-real-provider-local-release-gate-253052aa.md`; the standalone generated-file and Large Diff
predecessors remain in `docs/reports/stage-3-real-provider-local-standalone-generated-file-matrix-be919393.md` and
`docs/reports/stage-3-real-provider-local-large-diff-matrix-90fae52c.md`. The earlier two-Turn smoke,
dirty-worktree and deterministic fixture evidence remains in
`docs/reports/stage-3-provider-runtime-acceptance-fb9e25ec.md` and
`docs/reports/stage-3-provider-runtime-acceptance-2026-07-15.md`; none closes the four-Target release gate.

This audit treats executable code, migrations and repeatable tests as evidence. A local Provider Adapter is not
evidence that the Provider is supported by a remote Worker.

## Release boundary

| Provider | Existing local runtime                                                                                   | Existing remote host                                                                                                                                                 | Stage 3 remote release boundary                 |
| -------- | -------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------- |
| Codex    | App Server adapter with Send, Steer, Review, Interrupt, Approval/Input, Compact, Rollback and Fork paths | Bidirectional Provider Host Protocol 2.1 runtime backed by Codex App Server, with native Cursor, Interrupt, Steer, Approval/Input and bounded history reconstruction | Tier 1 target                                   |
| Claude   | Agent SDK adapter with multi-turn, Interrupt, Approval/Input, history and discovery paths                | Streaming Provider Host Protocol 2.1 runtime backed by Claude Agent SDK, with native Session ID, Interrupt, Steer, Approval/Input and bounded history reconstruction | Tier 1 target                                   |
| Cursor   | ACP adapter with Send, Interrupt, Approval/Input, history and Rollback                                   | None                                                                                                                                                                 | Local-only until the shared remote suite passes |
| Gemini   | ACP adapter with Send, Interrupt, Approval/Input, history and Rollback                                   | None                                                                                                                                                                 | Local-only until the shared remote suite passes |
| Grok     | ACP adapter with Send, Interrupt, history, Rollback and Compact                                          | None                                                                                                                                                                 | Local-only until the shared remote suite passes |
| Kilo     | OpenCode-compatible adapter with Send, Interrupt, Approval/Input, history, Compact and Fork              | None                                                                                                                                                                 | Local-only until the shared remote suite passes |
| OpenCode | SDK adapter with Send, Interrupt, Approval/Input, history, Compact and Fork                              | None                                                                                                                                                                 | Local-only until the shared remote suite passes |
| Pi       | SDK adapter with Send, Steer, Interrupt, User Input, history, Rollback and Compact                       | None                                                                                                                                                                 | Local-only until the shared remote suite passes |

`Local-only` is an explicit support conclusion, not a silent fallback. Remote scheduling must reject it with a
stable capability error before an Execution is claimed. Promotion to Tier 2 or Tier 1 requires the same Provider
Acceptance Fixture used by Codex and Claude.

## Workflow audit

| Workflow                                      | Status               | Current evidence and required delta                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| --------------------------------------------- | -------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| A. Capability matrix and support levels       | partial              | `providerCapabilityCatalog.json` is now the single machine-readable TS source and generates a hash-checked Go Catalog used by agentd, Manifest validation and Target policy. Control Plane exposes sanitized Project/Session `target` or execution-pinned projections with `supported / unsupported / unobserved`, stable reasons and `native / emulated` support modes; Session/Turn persistence rechecks Start/Send/Plan requirements inside the idempotent transaction. Web Provider selection, Plan, Steer, Interrupt and unsupported advanced commands consume the same projection. Provider × Target Acceptance for every declared native/emulated capability, including real remote Compact/Rollback/Fork/Review evidence, remains.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| B. Provider Host Protocol v2.1                | partial              | Shared/Host Protocol 2.1 contracts, Describe, normalized Runtime/Release descriptors, Command/Message envelopes, stable errors and terminal replay exist. Agentd requires Minor 1 for managed compatibility, publishes and gates the actual Host/Provider manifest, and multiplexes concurrent Send/Interaction/Interrupt plus primary Compact/Review terminals. Rollback/Fork are explicit Control Plane emulations rather than silently invented Host commands. Broader real-adapter and four-Target acceptance evidence remains.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| C. Unified Session/Turn semantics             | partial              | Start/Resume/Send use correlated v2 Command IDs with persisted Runtime Binding identity, native Cursor resume and authoritative-history fallback. Migration `000031` plus the Session service enforce one active Execution across the five guarded statuses. Migration `000032` makes Compact, Review, Rollback and Fork explicit immutable Turn kinds, preserves logical history ancestry and requires one matching primary Control Command for Worker-executed advanced Turns. Control Plane, agentd, Provider Host and SaaS Web now share that single path; real Codex/Claude replacement-Worker and four-Target restart evidence remains.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| D. Approval and Structured User Input         | partial              | Migration `000019`, Worker APIs and agentd implement Generation-fenced pull/delivered/acknowledged transitions and obsolete-Generation superseding. Migration `000021` persists immutable Turn runtime/interaction modes through Web, Session Event and Worker Workload. Migration `000028` records the request Runtime Event version so canonical resolutions never guess from an ambiguous name. Web now uses the durable Interaction list/reconcile and authoritative resolve path in SaaS mode. Real Codex and Claude Approval plus Plan Mode User Input pass Host round trips, and `2763ebd3` passed the Kubernetes deterministic fixture's Pending Approval Pod-loss Generation 1→2 recovery. Drain, real Eviction, unsupported-resume behavior and real-adapter cross-Target failure evidence remain. Claude uses a host-owned PreToolUse hook so local permission allow rules cannot bypass the durable decision.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| E. Runtime Event compatibility                | implemented baseline | `ProviderRuntimeEventV2` is now the only v2 wire vocabulary. Provider Host maps its bounded internal v1 messages to canonical v2 frames; agentd negotiates and enforces version 2; Control Plane keeps explicit legacy v1 while validating canonical v2 type/payload/size; Web projects both legacy and canonical Delta without duplicate Sequence application. Unknown Provider-native messages degrade to bounded `runtime.warning`; an unknown v2 wire type is rejected rather than silently reinterpreted. Provider-native Resume fallback uses an exact-shape canonical warning with no raw error/Cursor/Secret fields; agentd derives a stable Event ID only for that semantic slot from Execution, Generation and Send command identity. Eight-Provider golden fixtures and full Target acceptance remain under workflow L.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| F. Authoritative history and Worker migration | partial              | Migration `000018` and Session/Execution code create versioned Runtime Bindings; `000030` freezes the Credential version, Resume strategy and Provider binding digest per Execution generation. Cursor Envelope v2 authenticates that binding. Migration `000031` adds `absent / usable / quarantined`, source Execution/Generation/History Sequence lineage, legacy-ciphertext quarantine and the single-active-Execution index. Wrong-key, missing-Cipher, unknown/legacy-envelope, non-native Runtime, expiry at `now >= issuedAt + TTL`, and timestamps more than five minutes in the future preserve but quarantine ciphertext and select authoritative history; explicit Binding/Credential drift may clear it to `absent`. The default maximum age is 720 hours through `SYNARA_PROVIDER_CURSOR_MAX_AGE`, capped at 8760 hours. Extending TTL or restoring a key cannot revive quarantined ciphertext; only a fresh Cursor from the current Execution restores `usable`. Each Generation commits one safe Resume decision inside the existing `execution.leased` Event, and Claim receipt replay reuses that decision without reapplying age. If a previously selected native Cursor is no longer exactly available, replay returns `409 claim_replay_resume_cursor_unavailable` instead of silently changing strategy. SQLite and real PostgreSQL tests cover exact TTL/future-skew boundaries, quarantine stickiness, fresh recovery and two-pool retry/concurrency. Clean commit `61e38f4f` passes real Codex/Claude Local policy expiry, restart and `authoritative-history / cursor_expired` continuity; replacement-Worker and SSH/Docker/Kubernetes evidence remain.                                                                                                                                                                                                                                |
| G. Credential isolation                       | partial              | Codex/Claude Provider credentials use anonymous FD 3 with strict allowlists and SecretGuard redaction; Worker/Lease tokens are removed before Provider start. Migration `000033` adds explicit Tenant/Organization/User/Platform scopes and selection policy. Migrations `000035`/`000036` replace legacy Project Git authority with active Binding plus immutable per-Generation Grant IDs, `000038` preserves efficient FK enforcement for disabled history, and `000039` enforces one active image-pull Binding per Target. Agentd resolves only the exact operation stage, keeps HTTPS AskPass or one pinned SSH key/host-key agent in memory, clears it before Provider start and never forwards Workspace credentials to Provider Host. The shared Runner now requires an operator-selected environment source for real SSH/Docker/Kubernetes runs, registers the secret and optional Base URL with redaction before build/start, creates an encrypted isolated-Control-Plane Credential, and binds only its ID; command, Target, Image and report evidence retain neither the variable name nor value. Docker 401/429 injection adds an unguessable redacted route token and persists only normalized paths/header names. Registry/package execution stages, Windows Provider transport and the full real-target leakage suite remain.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| H. Remote Workspace/Git lifecycle             | partial              | Migrations `000020`/`000027` provide logical Workspace, Checkpoint, materialization and fenced physical-cleanup state without persisting Worker paths or plaintext credentials. Migrations `000035`/`000036` bind Project Git access through immutable generation Grants. Agentd holds the logical Workspace lock across materialization, Provider execution, inspection, Checkpoint and terminal reporting; HTTPS Clone/Fetch uses exact-host AskPass, while SSH uses an `ssh://` repository, public-address DNS pinning, exact stored host key and one temporary key agent. Cache and mutable Workspace generations remain isolated and recover interrupted installs fail closed. Git-reference/Patch/Snapshot capture and restore preserve the authoritative file tree, branch and tracked/untracked classification. Real multi-Worker/Target acceptance, long-lived SSH rotation and failure-injection evidence remain.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| I. Terminal/log/generated file/checkpoint     | partial              | Artifact path containment, server-side size/hash verification and retry-safe Checkpoint Artifact identity are implemented. Ordinary `generated_file`, `terminal_log` and `diff` uploads negotiate a header-based v1 idempotency feature, derive a content-bound deterministic Execution/Generation Artifact ID, reuse stable Create/Complete request IDs, refresh pending grants and recover a Ready Artifact after ambiguous responses. Before Provider start, agentd binds the Workspace and Runtime Output roots to anchored descriptors, rejects traversal/symlinks/non-regular files, and retains the opened descriptor through Secret Guard, hashing, upload and Ready verification. Migrations `000020`, `000024` and `000025` enforce Checkpoint scope and binding; forward migration `000041` adds only the `diff` Artifact kind. Agentd automatically creates Git-reference/Patch/Snapshot Checkpoints, including an empty Snapshot after the last non-Git file is deleted. Clean commit `be919393` proves the generated-file boundary, and clean commit `90fae52c` proves Ready downloadable Large Diff Artifacts for Codex and Claude, Artifact-backed `turn.diff.updated`, canonical-path alias handling, restart/Cursor continuity, cleanup and zero Secret findings. `workspace-checkpoint-unconfirmed` remains an explicit error Activity. Lossless real large-log acceptance, cross-Target and Retention concurrency remain.                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| J. Worker drain/upgrade/version isolation     | partial              | Migration `000017` stores immutable Worker/Image/Provider manifests and Claim compatibility. Migration `000034` separates terminal operator revocation from compatibility, fences Token/Heartbeat/Claim/Lease and records immutable logical-identity tombstones. Migration `000037` adds immutable target-scoped Release Revisions, a strict-CAS promoted/canary Policy, transition history and release-pinned Worker plus unleased Execution selection. Agentd Drain retains the Workspace lock, stops new Claim, renews through the bounded deadline, flushes terminal/Checkpoint state and reports conservative data-loss risk before Release. Worker images remain digest/lock/SBOM/version validated. A clean registry-pushed multi-arch reproduction, deliberate process-group escape, Windows FD3 transport and real multi-node canary/rollback/eviction evidence remain.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| K. Web authority switch                       | partial              | SaaS Project/Session/Turn/Event is Control Plane authoritative and local mode remains isolated. Tenant switching clears the old Tenant query/subscription/draft scope, and SaaS Provider/advanced-operation handlers fail closed through Control Plane capability and Session projections without calling local Native API paths. Strict-CAS model-switch still reuses `000030`/`000031` state and adds no DDL for that local operation. Credential scope/binding administration uses `000033`, `000035`, `000036`, `000038` and `000039`; Worker management and release rollout use `000034`, `000037` and `000040`. Artifact Ready plus explicit refresh/reconnect/Server-restart and no-Control-Plane local-mode evidence remains.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| L. Unified acceptance suite                   | partial              | A shared Runner emits machine-readable JSON, Markdown and redacted logs, drives Local, Docker, SSH and Kubernetes through user APIs and real Control Plane/agentd product paths, and models both standing and execution-pinned Worker allocation plus capability-declared managed replacement. On 2026-07-14 the deterministic Codex fixture passed all 13 SSH cases on an isolated disposable OrbStack Ubuntu 24.04 VM. Clean commit `2763ebd3` then passed all 13 Kubernetes cases on an owned disposable Kind cluster. Current dirty-worktree failure-only runs pass Local malformed/oversized/crash, Docker network interruption and Kubernetes worker-network/drain/eviction/image-canary. Clean commit `253052aa` rebuilds Provider Host with Node.js 24.13.1 and passes the consolidated real Local release unit: Codex product `22 pass + 1 unsupported`, Codex failure `16/16`, Claude product `21 pass + 2 unsupported`, Claude failure `16/16`, all on one clean SHA and Capability Catalog hash with exact cleanup and zero Secret findings. Terminal-aware Interaction waits fail immediately when a Provider terminates without the required request. The current implementation adds pre-build controlled Credentials, Docker-reachable 401/429, exact-container Host-crash injection, and a target-aware shared consolidated validator. `docker_release_gate.py` now builds one uniquely owned clean-SHA Worker image, requires all four controlled-environment children to skip their own build and reference its exact ID plus one Catalog hash, verifies exact per-child cleanup and empty Secret scans, then ownership-checks and removes the shared image in `finally`; 32 release-gate tests pass, but no real Docker aggregate report exists yet. Registry-pushed immutable rollout, long-session, real SSH/Docker/Kubernetes Providers, and SSH/Kubernetes remote fault injection remain. |

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
3. In progress: Codex App Server and Claude Agent SDK multi-Turn, native Interrupt/Steer, Approval, Plan Mode Input and history fallback are implemented. Runtime Event v2 is canonical end to end. Cursor Envelope v2, per-Execution Provider snapshots, Cursor quarantine/lineage, the bounded expiry policy, audited Claim selection, safe Provider-native invalid/expired fallback, one active Execution per Session and pre-Claim Interrupt cancellation are implemented. Clean commit `253052aa` passes the consolidated real Local product/failure release gate with the frozen Compact/lossless-Terminal boundaries, standalone `generated_file`, Workspace Checkpoint, Artifact-backed Large Diff, real 401/429, scoped Host crash and Cursor-expiry recovery. Continue with SSH, Docker and Kubernetes acceptance.
4. In progress: Workspace/Git/Checkpoint DDL, public/private HTTPS Clone/Fetch, Git Credential, state reporting, cross-process locked cache plus private relative worktree generations, Git-reference/Patch/Snapshot capture/restore, interrupted staging/backup reconciliation, physical cleanup and Checkpoint/Artifact retention are implemented; add SSH Credential delivery and real multi-Worker/Target acceptance.
5. In progress: Worker Manifest and graceful Drain are implemented; add reproducible image evidence, canary/rollback and upgrade isolation.
6. In progress: the deterministic shared Runner covers Local, Docker, SSH and Kubernetes and emits JSON/Markdown
   evidence. The SSH Driver's deterministic Codex fixture passed the 13-case live suite on 2026-07-14; clean commit
   `2763ebd3` passed the 13-case Kubernetes core suite. Current dirty-worktree failure-only runs also pass Local
   Provider faults, Docker network interruption and Kubernetes Network/Drain/Eviction/Image Canary. Re-run the
   implemented real Codex/Claude Local control/capability matrix passes on clean commit `0b3f9214`; clean commit
   `be919393` also passes the ten-case matrix, standalone generated-file Artifact and Workspace Checkpoint capture.
   Clean commit `253052aa` completes the consolidated real Local release suite across both adapters. Run both
   adapters across SSH, Docker and Kubernetes, then complete long-session and registry-pushed rollout before
   promoting any Local-only Provider or claiming the four-Target release gate.
