# Stage 3 real Provider Kubernetes release gate — `3c523417`

## Result

- Status: **pass**
- Evidence date: `2026-07-19` Asia/Shanghai
- Clean Git SHA: `3c523417a64a9e98572b9464f65d8815d91a0010`
- Run ID: `stage3-provider-kubernetes-release-a9536edf-aef2-4609-9852-e695299c8e76`
- Gate schema: `synara.provider-kubernetes-release-gate.v1`
- Capability Catalog SHA-256: `742a7eef08fde2394438fb0a9ee008cf1d062576d3b884709c291ffc17e9bdeb`
- Gate duration: `1541980 ms`
- Source worktree dirty: `false`

The gate used controlled third-party Provider profiles from an operator-owned, mode-`0600` environment file.
Codex and Claude each received their configured API key, Base URL, and custom model through the supported
Credential and Session bindings. Secret values and operator environment-variable names were not persisted.

## Four-child matrix

| Provider      | Matrix  | Status | Cases                                     | Explicit unsupported               |    Duration |
| ------------- | ------- | ------ | ----------------------------------------- | ---------------------------------- | ----------: |
| `codex`       | product | pass   | pass=22, unsupported=1, skipped=0, fail=0 | `real-provider.terminal-large-log` | `477349 ms` |
| `codex`       | failure | pass   | pass=16, unsupported=0, skipped=0, fail=0 | none                               | `235688 ms` |
| `claudeAgent` | product | pass   | pass=22, unsupported=1, skipped=0, fail=0 | `real-provider.compact-boundary`   | `567074 ms` |
| `claudeAgent` | failure | pass   | pass=16, unsupported=0, skipped=0, fail=0 | none                               | `247782 ms` |

The product matrices passed the real baseline and continuity Turns, Approval, Structured User Input, Steer,
Interrupt, generated-file Checkpoint, downloadable Large Diff, Review, Compact boundary, Rollback, and Fork.
The failure matrices passed controlled authentication, rate-limit retry, scoped Provider Host crash/retry, and
Cursor-expiry recovery. No failed or skipped case was accepted.

The only unsupported results are the frozen Provider boundaries:

- Codex Unified Exec retains a bounded head/tail instead of a lossless large Terminal stream.
- Claude Compact remains explicit `capability_unsupported` without mutating the Session.

## Controlled Provider profiles

- Codex model: `gpt-5.6-sol`
- Claude model: `claude-fable-5-dd-los-6.5-tpg`
- Controlled Base URL configured for both Providers: `true`
- Credential field for both Providers: `apiKey`
- Ambient authentication used: `false`
- Credential environment-variable names persisted: `false`

The model identifiers are non-secret evidence. API keys and Base URLs remain redacted and are not reproduced in
this report.

## Shared Worker image and exact cleanup

All four children used the same gate-owned Worker image built once from the clean SHA:

- Image ID: `sha256:1c8c840e5043c8a56c96a178437505da728a6afe93f3a2d57a91818b421972c8`
- Build metadata SHA-256: `6630b9e0f87de985d2af3ca7b6212a9ce48e17b61e0b2b861d3320883ab468ef`
- Child image builds skipped: `true`
- Gate image ownership verified before cleanup: `true`
- Exact gate-owned image removed: `true`
- Broad cleanup used: `false`

Each child created its own disposable Kind cluster and isolated state, then proved:

- `ownedClusterRemoved=true`
- `stateRemoved=true`
- `ownedWorkerImageRemoved=false`, leaving the shared image to the aggregate owner
- `broadCleanupUsed=false`

Post-run verification on the host reports no Kind clusters and confirms the exact shared image ID is absent.

## Security

- Aggregate output scan: `40` files, `4231228` bytes, `0` findings.
- Every child passed its own output Secret scan.
- Aggregate raw child output persisted: `false`.
- Credential environment-variable names persisted: `false`.
- The gate removed only resources carrying its exact ownership and identity evidence.
- No database DDL or migration changed in this blocker slice.

## Raw artifact identity

The raw directory is `.tmp/stage3-real-provider-kubernetes-release-3c523417/`. It remains intentionally ignored;
this checked-in report records immutable source and report digests without committing logs, state, Workspaces,
Artifacts, or credentials.

| Report               | JSON SHA-256                                                       | Markdown SHA-256                                                   |
| -------------------- | ------------------------------------------------------------------ | ------------------------------------------------------------------ |
| Aggregate            | `88cae46a6de3da2ccb386c10c0f0461b7eccb2a491e8c50c44e328e929786dd3` | `abcf822b7e16fba0f5b8b0c10c2e9fc1d5dfd2a5bdf595570db3ab0d414703aa` |
| Codex product        | `2eea0a6db00d2945b88510a184de5ea1b6ada776fd05519e0965dbaed78d0a1e` | `216abf6aaff404ef6d427ccee727c402bfda38b6610e711e046defa0f7f0a00b` |
| Codex failure        | `c962c7ea5ce0a8239d0c13ca1fd33935f6d3f2ef909934832099af796227c3aa` | `3bdd7a2ea6d6dda2436fb5399c4216630baba9d440d33059204c2de5907d025e` |
| Claude Agent product | `4c3825a59705d41f0505ca9af660c408f7a40655397c6b8de7f007499a0872f8` | `87d250856248bf73a608bd9b14e09d82a035c9caf0fd372d3e2a984d700baff6` |
| Claude Agent failure | `56dfcafeef5b766e60828aba771b4800e67cb8468ce365233710f092264dbefa` | `19d65b97935992da0f673cb7cd487912dbc0259b95b4e7ffc69cfe9c0de5116a` |

## Closed blockers

This clean run supersedes the `6b71703f` Kubernetes third-party gate failure for the configured controlled
Provider profiles:

1. Codex Approval now produces and durably resolves the required canonical interaction, including bounded
   sequential approvals and request/interaction identity checks.
2. Codex Steer and Interrupt verify their operation-specific canonical read-only commands instead of sharing the
   Approval command assertion.
3. Session capability observation survives the execution-pinned Pod lifecycle for capabilities already confirmed
   by the highest-revision active Runtime Binding, while unsupported capabilities remain unobserved and continue
   polling.
4. The configured Claude endpoint completes both product and failure matrices; its Compact boundary remains the
   previously frozen explicit unsupported result.

## Evidence boundary

This pass closes the implemented real Codex/Claude **disposable-Kind Kubernetes product and controlled-failure
release slice**. Together with the existing `b1c52bae` Docker aggregate, the previously executed Docker and
Kubernetes profile blockers are closed.

It does not close the real SSH aggregate, production multi-node Kubernetes rollout, production Registry/KMS
identity and admission evidence, approved production SLA/soak, or real-Provider remote
concurrency/Retention/load/rollout evidence. Those remain separate open gates and were not executed or claimed by
this run.
