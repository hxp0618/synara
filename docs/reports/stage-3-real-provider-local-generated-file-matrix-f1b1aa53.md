# Stage 3 Real Provider Local Generated File Matrix

- Evidence date: `2026-07-16` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Implementation commit: `f1b1aa53e11af96b3be8aa058f365cb749c93aa0`
- Result: **PASS FOR THE IMPLEMENTED LOCAL MATRIX; RELEASE GATE REMAINS OPEN**

## 1. Scope

The shared `real-provider-smoke` Runner executed every implemented real Provider case through:

```text
user API -> Control Plane -> LocalSupervisor -> agentd -> Provider Host -> real Provider
```

Both final runs used a clean worktree and the canonical case order:

```text
approval -> user-input -> steer -> interrupt
         -> generated-file-checkpoint -> terminal-large
         -> restart -> continuity -> review -> compact -> rollback -> fork
```

The new `generated-file-checkpoint` case writes one deterministic Workspace file, waits for the production
Checkpoint lifecycle, obtains a user Artifact download grant, downloads the Ready `workspace_snapshot`, and
validates the archive without extracting it to disk.

## 2. Clean-commit results

| Provider     | Runtime                                  | Case result              | Duration | Result |
| ------------ | ---------------------------------------- | ------------------------ | -------: | ------ |
| Codex        | Codex App Server `0.144.4`               | `21 pass, 1 unsupported` | 342.594s | pass   |
| Claude Agent | `@anthropic-ai/claude-agent-sdk 0.3.207` | `20 pass, 2 unsupported` | 128.927s | pass   |

Both final reports record:

- source Git SHA `f1b1aa53e11af96b3be8aa058f365cb749c93aa0`;
- `worktreeDirty=false`;
- Provider Capability Catalog SHA-256
  `5f912b33629e9da83969a96a14dc468bca4c3425848421527e8987d373221d9e`;
- exact runner-owned Local resource cleanup;
- an empty output Secret scan.

## 3. Generated File Checkpoint evidence

The generated file is identical for both Providers:

| Field      | Value                                                                    |
| ---------- | ------------------------------------------------------------------------ |
| Path       | `.synara-stage3-acceptance/generated-file.txt`                           |
| Size       | `1,048,833 B` (`1 MiB + 257 B`)                                          |
| SHA-256    | `c839026d9e03ffa989c8a7202a130cc085ff18829285714d67c5cba0fac27d4a`       |
| Checkpoint | `workspace_snapshot`, `application/x-tar`, Ready before Execution finish |

Provider-specific Snapshot evidence:

| Provider     | Archive bytes | Archive SHA-256                                                    | Members | Lifecycle Sequence |
| ------------ | ------------: | ------------------------------------------------------------------ | ------: | ------------------ |
| Codex        |     1,052,672 | `f8fa04e3a3ef39e40c6ee70793a6082523af56b0b759f7c90fe93697bce6716e` |       3 | `130 -> 134`       |
| Claude Agent |     1,051,648 | `b6286b656ba8984e0f5f0342b1161391b39e48b8f33d484cabcf9282e303805e` |       2 | `105 -> 109`       |

Both cases require the strict order:

```text
workspace.dirty -> checkpoint.created -> artifact.ready
                -> checkpoint.ready -> execution.completed
```

The Runner rejects absolute, traversal, Windows-style, link, special, duplicate, or unexpected Tar members. It
also verifies the exact generated payload, any known Approval/Steer sentinel that is present, Artifact metadata,
downloaded byte count and hash, one Ready Artifact boundary, and the absence of physical Workspace or Artifact
paths in Session Events. Neither final report contains a standalone `generated_file` Artifact for this Execution.

## 4. Explicit unsupported boundaries

These are capability boundaries, not hidden passes:

- Codex `terminal-large`: Unified Exec retains only a 1 MiB head/tail view for output beyond 1 MiB. The Runner
  does not disable Unified Exec because doing so would change durable Approval and native Cursor semantics.
- Claude ambient OAuth `terminal-large`: lossless retained SDK output requires a controlled Provider Credential
  so `CLAUDE_CONFIG_DIR` can be bound to the agentd-owned Runtime Output Root.
- Claude Compact: the Agent SDK returns stable `capability_unsupported`; the Session Sequence remains unchanged.

## 5. Retry provenance

The first clean Codex full-matrix attempt did not produce an Approval request. Its baseline real Turn passed and
the Approval Execution completed without opening an Approval interaction. The
Runner therefore timed out at the overall 900-second deadline. This attempt still recorded
`worktreeDirty=false`, exact cleanup, and an empty Secret scan.

A clean-SHA Approval-only reproduction immediately passed in 54.061 seconds, including Control Plane restart and
native-Cursor continuity. The subsequent complete Codex rerun passed all implemented cases. No source change was
made between the failed attempt, the narrow reproduction, and the final pass. Only the final complete rerun is
used for the matrix result; the earlier reports remain retained as diagnostic evidence rather than being erased.

## 6. Raw reports

| Purpose                    | Report directory                                                     | JSON SHA-256                                                       | Markdown SHA-256                                                   |
| -------------------------- | -------------------------------------------------------------------- | ------------------------------------------------------------------ | ------------------------------------------------------------------ |
| Final Codex matrix         | `.tmp/stage3-provider-acceptance-results/20260716T055142Z-081b0b38/` | `9378ac2ab86caf019687e78b033a8799d5704a22a5b0a59c06c3269a4285b119` | `a8821c426aefb9c11a41485448c990f73ea692d2ed284e97561101b0d7ea9154` |
| Final Claude matrix        | `.tmp/stage3-provider-acceptance-results/20260716T055819Z-c824e2f1/` | `d269703c2a53411e4d35599c2bc2371730ec9c62d769eca1bbd40ed38855125a` | `765da5ed320571e420935fb15f9c19a933af1d6ed6834c4268b5482aa440dd1f` |
| Initial Codex diagnostic   | `.tmp/stage3-provider-acceptance-results/20260716T052958Z-d634595a/` | `0d6b4b249a9a9f5872804952f148023b73b37ed12940c35301dfff38d00f7185` | `4247a05645ef6178de8f266b88874cf61ab28e5629a7ccb86ee73ba19fcc30ac` |
| Approval-only reproduction | `.tmp/stage3-provider-acceptance-results/20260716T054638Z-a2db4b9d/` | `017c9c1c2fe7ae137da94c21f577aaf46c0c9a6f190a0bfd2d83a38d19dc8014` | `9ef8affd0fc71966cd1fdc81cbde921a632adf030f4011c3ad3a6d26907d05b8` |

The raw directories are intentionally ignored local artifacts. This checked-in report records their immutable
source SHA and report digests without committing logs, SQLite state, Workspaces, Artifact payloads, or credentials.

## 7. Verification and DDL boundary

Before the clean-commit matrix:

- `bun fmt` passed;
- `bun lint` passed with zero errors and 235 repository-existing warnings;
- `bun typecheck` passed for all 9 packages;
- Acceptance Runner tests passed `67/67`;
- Python `py_compile` and `git diff --check` passed;
- the Provider Host clean build completed.

This increment adds no DDL and changes no migration. The checked-in Stage 3 migration boundary remains
`000032`-`000040`.

## 8. Remaining release gates

This evidence closes the implemented **Local Workspace Generated File Checkpoint capture path**, not Stage 3
production release:

1. Standalone Provider `generated_file` ArtifactCandidate and large Diff gates remain.
2. Real lossless large Terminal remains open for Codex and for Claude without a controlled Credential.
3. Real Provider auth, rate-limit, crash, and retry classification matrices remain.
4. Real Codex/Claude SSH, Docker, and Kubernetes matrices remain.
5. Registry-pushed immutable multi-arch image, signature, SBOM, reproducibility, promote/rollback, multi-node
   Kubernetes, Retention concurrency, long Session, and soak gates remain.
