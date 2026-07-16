# Stage 3 Real Provider Local Large Diff Matrix

- Evidence date: `2026-07-16` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Implementation commit: `90fae52c457654f2844e735515179462aa0229bf`
- Result: **PASS FOR THE IMPLEMENTED LOCAL MATRIX; RELEASE GATE REMAINS OPEN**

## 1. Scope

The shared `real-provider-smoke` Runner executed every implemented real Provider case through:

```text
user API -> Control Plane -> LocalSupervisor -> agentd -> Provider Host -> real Provider
```

Both final runs used a clean worktree and the canonical eleven-case matrix around the restart boundary:

```text
approval -> user-input -> steer -> interrupt
         -> generated-file-checkpoint -> large-diff -> terminal-large
         -> restart -> continuity -> review -> compact -> rollback -> fork
```

The new `large-diff` case creates a deterministic `315,000 B`, 5,000-line Workspace file, mutates it through the
Provider's native file-change path, and requires one standalone Ready `diff` Artifact. The Runner downloads the
Artifact through the user API and verifies exact UTF-8 content, Size/SHA-256, file/addition/deletion summary,
sequence order, absence of an inline large payload, and absence of Runtime Output physical paths.

## 2. Clean-commit results

| Provider     | Runtime                                  | Case result              | Duration | Result |
| ------------ | ---------------------------------------- | ------------------------ | -------: | ------ |
| Codex        | Codex App Server `0.144.x`               | `22 pass, 1 unsupported` | 324.054s | pass   |
| Claude Agent | `@anthropic-ai/claude-agent-sdk 0.3.207` | `21 pass, 2 unsupported` | 199.903s | pass   |

Both reports record:

- source Git SHA `90fae52c457654f2844e735515179462aa0229bf`;
- `worktreeDirty=false`;
- Provider Capability Catalog SHA-256
  `8d47c4a08cdce16f0420c911737f92bf6b28ba49c6310e5601b7bd434f671f70`;
- exact runner-owned Local resource cleanup;
- an empty output Secret scan.

All previously implemented real Local cases remain green. Codex keeps the explicit lossless `terminal-large`
unsupported boundary. Claude keeps the explicit ambient-auth `terminal-large` and Compact unsupported boundaries;
neither is relabeled as a pass.

## 3. Large Diff evidence

| Field                 | Codex                                                              | Claude Agent                                                       |
| --------------------- | ------------------------------------------------------------------ | ------------------------------------------------------------------ |
| Native mutation       | app-server `turn/diff/updated`, delete 5,000 lines                 | SDK native Read + Write, complete-file fallback                    |
| Artifact kind/name    | `diff` / `turn.diff`                                               | `diff` / `turn.diff`                                               |
| Content-Type          | `text/x-diff; charset=utf-8`                                       | `text/x-diff; charset=utf-8`                                       |
| Size                  | `320,258 B`                                                        | `320,201 B`                                                        |
| SHA-256               | `60110a81d34f4cc22e31afc778adb0af32d681f834f2561f153db55d22bec15b` | `a1f849cded461e78ffc00015674ee9b8ce345e9332948789c9b90b3ab40e1b24` |
| Summary               | 1 file, 0 additions, 5,000 deletions                               | 1 file, 1 addition, 5,000 deletions                                |
| Ready/reference/done  | `176 -> 177 -> 183`                                                | `139 -> 140 -> 146`                                                |
| Inline payload stored | no                                                                 | no                                                                 |
| Physical path leaked  | no                                                                 | no                                                                 |

Provider Host keeps Diffs at or below 48 KiB inline. A larger UTF-8 Diff up to 16 MiB is staged beneath the
agentd-owned Runtime Output Root under a content-addressed relative path. Agentd performs the anchored no-symlink
open, exact reported-size check, text Secret Guard, idempotent upload and Ready verification against the guarded
bytes before appending the Artifact-backed `turn.diff.updated` Event.

Claude's real SDK response used a canonical Workspace realpath while the configured Workspace path was an alias.
The shared path resolver accepted that in-Workspace identity without accepting outside or symlink escapes. When the
SDK omitted `gitDiff.patch` and usable structured hunks, Provider Host reconstructed the complete unified Diff from
`originalFile` plus the native Write content; it did not scan the Workspace or parse shell text.

## 4. Acceptance reliability correction

The first clean run on `2d5e6ce6` passed Turn 1, Approval, Structured User Input and Steer, but the Interrupt prompt
said to wait for approval "before doing anything else". Codex followed that phrase literally and waited without
invoking Bash, so no Approval Interaction existed and the remaining cases were dependency-skipped.

Commit `90fae52c` changed the instruction to invoke the tool immediately and let the runtime pause the invocation
at the Approval boundary. The corrected prompt first passed an isolated real Codex Interrupt run, including stale
Interaction removal, immediate recovery, Control Plane restart, second-Turn Cursor continuity, cleanup and Secret
scan. Both final full matrices then passed. No runtime protocol behavior was weakened to accommodate the prompt.

## 5. Raw reports

| Provider     | Report directory                                                                         | JSON SHA-256                                                       | Markdown SHA-256                                                   |
| ------------ | ---------------------------------------------------------------------------------------- | ------------------------------------------------------------------ | ------------------------------------------------------------------ |
| Codex        | `.tmp/stage3-provider-acceptance-results/20260716-90fae52c-codex-real-provider-matrix/`  | `ea52f1246b55ca458a65ce2bd21b7a198c75b8ba4a073fe8cb180215f2529a90` | `ca0e082e09f523f0929018871d5b8a265d6fe907df4df474aa16cddf8417cb6d` |
| Claude Agent | `.tmp/stage3-provider-acceptance-results/20260716-90fae52c-claude-real-provider-matrix/` | `94346ee6452c7c46a66abc8a2b50bdd728ec06e06e5bfa64ce7afcb5d0bc91b8` | `15461808954c0e77623d4efcb7b0caf8ca4a41acb22bac6d17b404d27f933723` |

The raw directories are intentionally ignored local artifacts. This checked-in report records their immutable
source SHA and report digests without committing logs, SQLite state, Workspaces, Artifact payloads or credentials.

## 6. Verification and DDL boundary

Before the clean-commit matrices:

- `bun fmt`, `bun lint`, and `bun typecheck` passed; lint reported zero errors and 237 repository warnings;
- Provider Host passed `103/103` tests, typecheck and build;
- Contracts passed `137/137` tests, typecheck and build;
- Acceptance Runner passed `71/71` tests and Python bytecode compilation;
- agentd, executions, artifacts, database and Provider Catalog Go packages passed their full relevant tests;
- targeted Go vet, `git diff --check`, JSON schema parsing and sensitive-information audit passed;
- PostgreSQL 17 applied `000001` through `000040`, then forward migration `000041`, preserving every existing
  Artifact kind while adding only `diff`.

Migration `000041_diff_artifact_kind.sql` is the only new DDL. Historical migrations remain unchanged, and the
checked-in Stage 3 migration boundary is now `000041`.

## 7. Remaining release gates

This evidence closes the implemented **real Codex/Claude Local Large Diff Artifact path**, not Stage 3 production
release:

1. Real Provider auth, rate-limit, crash, retry and Cursor-expiry failure matrices remain.
2. Real lossless large Terminal remains unsupported for Codex `0.144.x` and for Claude ambient authentication.
3. Real Codex/Claude SSH, Docker and Kubernetes matrices remain.
4. Cross-Target Artifact/Checkpoint/Retention concurrency and long-Session soak remain.
5. Registry-pushed immutable multi-arch image, signature, SBOM, reproducibility, promote/rollback and multi-node
   Kubernetes release evidence remain.
