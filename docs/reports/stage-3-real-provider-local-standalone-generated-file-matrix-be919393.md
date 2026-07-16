# Stage 3 Real Provider Local Standalone Generated File Matrix

- Evidence date: `2026-07-16` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Implementation commit: `be91939344e2ca878a94d01ea977c42252f14e87`
- Result: **PASS FOR THE IMPLEMENTED LOCAL MATRIX; RELEASE GATE REMAINS OPEN**

## 1. Scope

The shared `real-provider-smoke` Runner executed every implemented real Provider case through:

```text
user API -> Control Plane -> LocalSupervisor -> agentd -> Provider Host -> real Provider
```

Both final runs used a clean worktree and the canonical ten-case order:

```text
approval -> user-input -> steer -> interrupt
         -> generated-file-checkpoint -> terminal-large
         -> restart -> continuity -> review -> compact -> rollback -> fork
```

The `generated-file-checkpoint` case now proves two independent durability boundaries in one Execution:

1. a Provider-native file mutation creates one standalone `generated_file` Artifact;
2. one exact shell command creates a larger file that is preserved through the Workspace Checkpoint Snapshot.

The Provider Host does not parse shell commands or scan the Workspace to guess standalone Artifact paths.

## 2. Clean-commit results

| Provider     | Runtime                                  | Case result              | Duration | Result |
| ------------ | ---------------------------------------- | ------------------------ | -------: | ------ |
| Codex        | Codex App Server `0.144.4`               | `21 pass, 1 unsupported` | 307.344s | pass   |
| Claude Agent | `@anthropic-ai/claude-agent-sdk 0.3.207` | `20 pass, 2 unsupported` | 148.674s | pass   |

Both reports record:

- source Git SHA `be91939344e2ca878a94d01ea977c42252f14e87`;
- `worktreeDirty=false`;
- Provider Capability Catalog SHA-256
  `5f912b33629e9da83969a96a14dc468bca4c3425848421527e8987d373221d9e`;
- exact runner-owned Local resource cleanup;
- an empty output Secret scan.

## 3. Standalone generated-file evidence

The standalone file is identical for both Providers:

| Field        | Value                                                              |
| ------------ | ------------------------------------------------------------------ |
| Path         | `.synara-stage3-standalone-generated-file.txt`                     |
| Size         | `43 B`                                                             |
| Content-Type | `application/octet-stream`                                         |
| SHA-256      | `9161fafda333d0ccd5925c25805f553f01aa0b62659cdfe272531b72cf78b4dd` |
| Artifact     | one downloadable Ready `generated_file` before `workspace.dirty`   |

| Provider     | Authoritative native source                 | Lifecycle sequence |
| ------------ | ------------------------------------------- | ------------------ |
| Codex        | completed successful `fileChange` item      | `138 -> 140`       |
| Claude Agent | successful `PostToolUse` for native `Write` | `108 -> 110`       |

The Runner lists the Session Artifacts, requires exactly one Execution-scoped Ready `generated_file`, obtains a
user download grant, downloads the payload, and re-verifies exact bytes, Size and SHA-256. It rejects duplicate
Ready Events, mismatched metadata, download corruption and physical path leakage.

Provider Host candidates are bounded and deduplicated. Paths longer than 4 KiB, out-of-Workspace paths, VCS
metadata, missing paths, symlinks, directories and non-Regular Files are not emitted. At most 256 candidates are
tracked and 64 are emitted per Turn; overflow produces a safe warning while the complete Workspace remains
durable through its Checkpoint. Agentd remains authoritative for anchored open, Secret Guard, Size/SHA-256,
upload and Ready confirmation.

## 4. Workspace Checkpoint evidence

The larger shell-created file remains identical for both Providers:

| Field   | Value                                                              |
| ------- | ------------------------------------------------------------------ |
| Path    | `.synara-stage3-acceptance/generated-file.txt`                     |
| Size    | `1,048,833 B` (`1 MiB + 257 B`)                                    |
| SHA-256 | `c839026d9e03ffa989c8a7202a130cc085ff18829285714d67c5cba0fac27d4a` |

| Provider     | Snapshot bytes | Snapshot SHA-256                                                   | Members | Full lifecycle sequence                  |
| ------------ | -------------: | ------------------------------------------------------------------ | ------: | ---------------------------------------- |
| Codex        |      1,053,696 | `dd2bac8429125ec91bad354ea9680ab580b5f0fb7de5be92419984d379fee977` |       4 | `138 -> 140 -> 141 -> 142 -> 143 -> 144` |
| Claude Agent |      1,052,672 | `a070a79932aae3f58c1f6bf6c33c80440508b6b9322da90cb7fb9fa0ab146d0d` |       3 | `108 -> 110 -> 111 -> 112 -> 113 -> 114` |

The required order is:

```text
generated_file artifact.ready -> workspace.dirty -> checkpoint.created
                              -> workspace_snapshot artifact.ready
                              -> checkpoint.ready -> execution.completed
```

The Snapshot verifier rejects absolute, traversal, Windows-style, link, special, duplicate or unexpected Tar
members. It verifies the exact large file and known Runner sentinels, one Ready boundary, authenticated download,
and the absence of physical Workspace or Artifact paths in Session Events.

## 5. Explicit unsupported boundaries

These remain capability boundaries rather than hidden passes:

- Codex `terminal-large`: Unified Exec retains only a 1 MiB head/tail view for output beyond 1 MiB. The Runner
  does not disable it because doing so would change durable Approval and native Cursor semantics.
- Claude ambient OAuth `terminal-large`: lossless retained SDK output requires a controlled Provider Credential
  so `CLAUDE_CONFIG_DIR` can be bound to the agentd-owned Runtime Output Root.
- Claude Compact: the Agent SDK returns stable `capability_unsupported`; Session Sequence remains unchanged.

## 6. Raw reports

| Provider     | Report directory                                                                         | JSON SHA-256                                                       | Markdown SHA-256                                                   |
| ------------ | ---------------------------------------------------------------------------------------- | ------------------------------------------------------------------ | ------------------------------------------------------------------ |
| Codex        | `.tmp/stage3-provider-acceptance-results/20260716-be919393-codex-real-provider-matrix/`  | `1549f4003d4dc0dcd281fdb1c50df880313402a7571f00ff7d8b17c7127f843f` | `127ddc50c49a8bf4066061ab6df6d7adb8218696ba03a912ac340a543c0e7d32` |
| Claude Agent | `.tmp/stage3-provider-acceptance-results/20260716-be919393-claude-real-provider-matrix/` | `39f12b29a22396a82c11a1034ee847010e1818349a94c550d070b9649e65b46f` | `7f0cbb5e8c26cdcc0096631f019044243b1c9c5ee40bae94d3d44eeacc2986ee` |

The raw directories are intentionally ignored local artifacts. This checked-in report records their immutable
source SHA and report digests without committing logs, SQLite state, Workspaces, Artifact payloads or credentials.

## 7. Verification and DDL boundary

Before the clean-commit matrices:

- `bun fmt` passed;
- `bun lint` passed with zero errors and 237 repository warnings;
- `bun typecheck` passed for all 9 packages;
- Provider Host tests passed `98/98`;
- Acceptance Runner tests passed `69/69`;
- `go test ./internal/agentd` passed;
- Provider Host build, Python `py_compile`, `git diff --check` and the isolated Local deterministic fixture passed.

This increment adds no DDL and changes no migration. The checked-in Stage 3 migration boundary remains
`000032`-`000040`.

## 8. Remaining release gates

This evidence closes the implemented **Local standalone Provider generated-file Artifact plus Workspace
Checkpoint path**, not Stage 3 production release:

1. Large Diff/Patch Artifact projection remains open.
2. Real lossless large Terminal remains open for Codex and for Claude without a controlled Credential.
3. Real Provider auth, rate-limit, crash and retry classification matrices remain.
4. Real Codex/Claude SSH, Docker and Kubernetes matrices remain.
5. Registry-pushed immutable multi-arch image, signature, SBOM, reproducibility, promote/rollback, multi-node
   Kubernetes, Retention concurrency, long Session and soak gates remain.
