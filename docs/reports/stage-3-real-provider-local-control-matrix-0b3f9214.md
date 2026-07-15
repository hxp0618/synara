# Stage 3 Real Provider Local Control Matrix

- Evidence date: `2026-07-16` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Implementation commit: `0b3f9214f925678220bf1fac15cd735f1a5b372e`
- Result: **PASS FOR THE IMPLEMENTED LOCAL MATRIX; RELEASE GATE REMAINS OPEN**

## 1. Scope

The shared `real-provider-smoke` Runner executed every implemented real Provider case through:

```text
user API -> Control Plane -> LocalSupervisor -> agentd -> Provider Host -> real Provider
```

Both runs used a clean worktree and selected the canonical case order:

```text
approval -> user-input -> steer -> interrupt -> restart -> continuity
         -> review -> compact boundary -> rollback -> fork continuity
```

The matrix is Provider-aware without silently changing product semantics:

- Codex uses native App Server collaboration mode, Steer, Interrupt, Review and Compact.
- Claude uses Agent SDK streaming input for Steer, native Interrupt, fixed read-only emulated Review and an
  explicit unsupported Compact boundary.
- Rollback and Fork remain Control Plane emulations for both Providers and do not invent Host commands.

## 2. Clean-commit results

| Provider     | Runtime                                  | Case result              | Duration | Result |
| ------------ | ---------------------------------------- | ------------------------ | -------: | ------ |
| Codex        | Codex App Server                         | `20 pass`                | 260.963s | pass   |
| Claude Agent | `@anthropic-ai/claude-agent-sdk 0.3.207` | `19 pass, 1 unsupported` |  95.653s | pass   |

Both reports record:

- source Git SHA `0b3f9214f925678220bf1fac15cd735f1a5b372e`;
- `worktreeDirty=false`;
- the same Provider Capability Catalog SHA-256
  `5f912b33629e9da83969a96a14dc468bca4c3425848421527e8987d373221d9e`;
- exact runner-owned cleanup;
- an empty output Secret scan.

## 3. Capability evidence

| Capability            | Codex                                                                                                                          | Claude Agent                                                                                                                                 |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------- |
| Approval              | Durable command Approval resolves through the user API and resumes the same Execution.                                         | Same; Generation-scoped request identity and resolution delivery pass.                                                                       |
| Structured User Input | Plan Mode question resolves through the user API and returns the exact marker.                                                 | Same through `AskUserQuestion`, with canonical `user-input.resolved`.                                                                        |
| Steer                 | Native `turn/steer` changes the final marker while Approval is pending.                                                        | Streaming priority input changes the marker while a bounded command is active; the Host waits past the SDK's superseded intermediate result. |
| Interrupt             | Native interrupt clears the stale Interaction, produces `execution.interrupted` and permits an immediate recovery Turn.        | Same product semantics through Agent SDK interrupt and stale Interaction cleanup.                                                            |
| Review                | Native Review persists `review_entered`, `review_exited`, canonical assistant text and `supportMode=native`.                   | Fixed read-only Tool Policy persists the same boundaries with `supportMode=emulated`.                                                        |
| Compact               | Native context compaction persists one completed boundary and `thread.state.changed(state=compacted)`.                         | API returns `409 capability_unsupported`; the case is recorded as explicit `unsupported`, and Session Sequence is unchanged.                 |
| Rollback              | Worker-free `session.history.rolled-back`, `supportMode=emulated`, workspace unchanged and external side effects not reverted. | Same; no Execution, Worker or Generation is attached to the logical event.                                                                   |
| Fork                  | Worker-free logical Fork, then a real Turn reconstructs authoritative history because no Provider Cursor is copied.            | Same, with exact source-marker continuity on the forked Session.                                                                             |

Codex Rollback removed three logical Turns after Review and Compact. Claude Rollback removed two because Compact
was rejected before mutation. Both Fork continuations selected
`authoritative-history / cursor_absent` and matched their deterministic source marker.

## 4. Runtime defects closed

The real matrix exposed and closed product-path defects rather than acceptance-only exceptions:

1. Codex resumed native Threads could remain in Plan Mode because the Host omitted the default collaboration
   preset. Interactive Turns now send the effective `default` or `plan` preset with a safe model fallback.
2. Claude can cancel an outstanding SDK permission request when Steer input arrives. A late durable resolution is
   ignored only for a request that this exact Runtime previously knew and cancelled; unknown or wrong-Generation
   request IDs still fail.
3. Claude streaming input emits an intermediate result for the superseded response before the steered response.
   The Host now waits for the subsequent result instead of closing the Query early.
4. Claude can return a contradictory `subtype=success`, `is_error=true` Review result with review text and no
   explicit errors. Only this fixed read-only Review shape is accepted with a warning; ordinary error results still
   fail.
5. Real case selection is composable: Rollback-only accepts its single removed anchor Turn, and Fork creates its
   own deterministic source marker so `review + fork` does not inherit arbitrary Review text.

## 5. Raw reports

| Provider     | JSON report                                                                                | JSON SHA-256                                                       | Markdown SHA-256                                                   |
| ------------ | ------------------------------------------------------------------------------------------ | ------------------------------------------------------------------ | ------------------------------------------------------------------ |
| Codex        | `.tmp/stage3-provider-acceptance-results/20260715T232911Z-6bbb5c61/acceptance-report.json` | `00c64a94e04553f33032b644d5e0c344afd63a58750737e5dc2e3a0f28aa9a0a` | `c0725484ed0e0d3d80830957da8d87ad02bacb841bf7375d00115eb33748ea8b` |
| Claude Agent | `.tmp/stage3-provider-acceptance-results/20260715T233358Z-d77e3391/acceptance-report.json` | `d5772fbb9d3d4f5fe84e4e11b7de77849c7163bb3fd74e83d1cfee5f3fc7ad62` | `9ba8f5fe9d1620ae03d76824960f36e0648cc5cb3fdbab6273f0b634f58c2bf5` |

The raw directories are intentionally ignored local artifacts. The checked-in report records the immutable source
SHA and report digests without committing runtime logs, SQLite state, Workspaces or credentials.

## 6. Verification and DDL boundary

Before the clean-commit runs:

- `bun fmt` passed;
- `bun lint` passed with zero errors and repository-existing warnings;
- `bun typecheck` passed for all 9 packages;
- Provider Host tests passed `94/94`;
- Acceptance Runner tests passed `53/53`;
- Provider Host build completed.

This increment adds no DDL and changes no migration. The checked-in Stage 3 migration boundary remains
`000032`-`000040`.

## 7. Remaining release gates

This result closes the implemented **Local control/capability matrix**, not Stage 3 production release:

1. Real Provider generated-file, large Diff/Terminal, Artifact/Checkpoint and auth/rate-limit/crash matrices remain.
2. Real Codex/Claude SSH, Docker and Kubernetes matrices remain.
3. Registry-pushed immutable multi-arch image, signature, SBOM, reproducibility and promote/rollback remain.
4. Multi-node Kubernetes, PDB/CNI enforcement, real Drain/Eviction and upgrade stress remain.
5. Long Session, repeated Compact/Checkpoint/Resume, concurrency, Retention/Cleanup and soak remain.
