# Stage 3 Real Provider Consolidated Local Release Gate

- Evidence date: `2026-07-16` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Implementation commit: `253052aaba8c731d1089b5f169038a087e1193ea`
- Provider Host runtime: Node.js `24.13.1`
- Provider Host bundle SHA-256: `43b23061c105d8b6234f87bc90f024fc8b8724dd52f81baf1a0162d91133e8b6`
- Provider Capability Catalog SHA-256: `8d47c4a08cdce16f0420c911737f92bf6b28ba49c6310e5601b7bd434f671f70`
- Result: **PASS FOR THE CONSOLIDATED REAL LOCAL SLICE; FOUR-TARGET RELEASE GATE REMAINS OPEN**

## 1. Scope

`local_release_gate.py` rebuilt Provider Host from the clean checkout and executed four independent child reports
through the production Local path:

```text
user API -> Control Plane -> LocalSupervisor -> agentd -> Provider Host -> real Provider

Codex product matrix   + Codex failure matrix
Claude product matrix  + Claude failure matrix
```

The product reports covered every canonical Local case:

```text
approval -> user-input -> steer -> interrupt
         -> generated-file-checkpoint -> large-diff -> terminal-large
         -> restart -> continuity -> review -> compact -> rollback -> fork
```

The failure reports remained separate and covered real HTTP 401, real HTTP 429, a scoped Provider Host crash and
Cursor expiry followed by Control Plane restart and authoritative-history recovery.

The aggregate could pass only when all four reports used the same clean Git SHA and Capability Catalog hash,
contained every canonical case, had no failed or skipped cases, preserved only the frozen explicit-unsupported
boundaries, completed exact runner-owned cleanup and passed their output Secret scans. Raw child process output and
credentials were not persisted by the aggregate.

## 2. Clean-commit results

| Provider     | Matrix  | Case result              | Duration | Result |
| ------------ | ------- | ------------------------ | -------: | ------ |
| Codex        | product | `22 pass, 1 unsupported` | 332.548s | pass   |
| Codex        | failure | `16 pass`                |  89.120s | pass   |
| Claude Agent | product | `21 pass, 2 unsupported` | 192.430s | pass   |
| Claude Agent | failure | `16 pass`                |  70.806s | pass   |
| Aggregate    | all     | four required runs       | 687.787s | pass   |

All four reports record:

- source Git SHA `253052aaba8c731d1089b5f169038a087e1193ea` and `worktreeDirty=false`;
- the same Provider Capability Catalog SHA-256;
- zero failed and zero skipped cases;
- exact Local resource cleanup;
- an empty output Secret scan.

## 3. Frozen unsupported boundaries

The release gate accepted only the already documented Local boundaries:

| Provider     | Case                               | Boundary                                                                               |
| ------------ | ---------------------------------- | -------------------------------------------------------------------------------------- |
| Codex        | `real-provider.terminal-large-log` | Codex Unified Exec retains a bounded head/tail rather than a lossless stream.          |
| Claude Agent | `real-provider.compact-boundary`   | Claude Compact remains explicit `capability_unsupported`.                              |
| Claude Agent | `real-provider.terminal-large-log` | Ambient OAuth cannot safely bind retained output to the execution Runtime Output Root. |

No new unsupported result was accepted, and no unsupported case was relabeled as a pass.

## 4. Acceptance reliability correction

The first consolidated attempt on parent commit `d591cd89` exposed a real Provider behavior failure: the Codex
Approval Turn emitted only reasoning and the requested final marker, completed successfully from the Provider's
perspective, and never invoked the shell tool or created an Approval Interaction. The original Runner continued
polling the interaction endpoint even though the terminal Event proved that the request could no longer appear.

Commit `253052aa` made interaction waits terminal-aware. A Turn that completes, fails, cancels or interrupts before
the required Approval or User Input now fails immediately with
`runner.interaction_missing_after_terminal`. The same rule protects replacement-interaction recovery. It does not
retry the Provider, weaken the interaction assertion or convert the failure into an unsupported result.

The correction passed a focused real Codex Approval run, including resolution delivery/acknowledgement, restart,
native-Cursor continuity, cleanup and Secret scan. The final four-matrix release gate then ran once from the clean
`253052aa` checkout and passed without retry.

## 5. Raw report identity

| Report               | JSON SHA-256                                                       | Markdown SHA-256                                                   |
| -------------------- | ------------------------------------------------------------------ | ------------------------------------------------------------------ |
| Aggregate            | `c7275ef44ccdf27284b18039b1da78ca224c85e35853f44b86f61709df1181a0` | `cdfca5b06547eb79225f7c58efe88ef9b2b6f6308821065b713c75e4ee860134` |
| Codex product        | `d4cd2875c4c6cbee34a9854e8d5e04b899dfb698e35d5fc5c94e933e13ce15f6` | `67755a31c3367e09436d18a97dd67e501e07da68a6010fa99f4a1c35419d39b5` |
| Codex failure        | `4d1ca2043e74b156afe7f38e69be08798910f612e69c73371a99212345586204` | `9315c8de9c22325d39628fda1a9abcef1405c65ff113e4a32c4865c8906f0958` |
| Claude Agent product | `27b295ce6190c3381cdb9c315fce669c9be636e5ad8ece7a1ca7cd2f81527e8b` | `f078aa68079d7b295453ba60a8e7541f3331b16d958d15027ab799905e1fa2b3` |
| Claude Agent failure | `b6837f7eb267b824e66394d6d26547adeb2222269e7f2ed3e61b8ccb86bb75a5` | `c5405079501ea2ed6432374200e3115546f1ceca572f95ff0a395f109a09d155` |

The raw directory is
`.tmp/stage3-provider-acceptance-results/20260716-253052aa-node24-local-release-gate/`. It is intentionally ignored;
this checked-in report records immutable source and report digests without committing logs, SQLite state,
Workspaces, Artifact payloads or credentials.

## 6. Verification and DDL boundary

- Acceptance Runner passed `77/77` tests.
- Consolidated gate passed `14/14` tests.
- Acceptance and gate Python modules passed bytecode compilation.
- The focused real Codex Approval run passed before the final aggregate.
- Provider Host was rebuilt by the gate from the clean commit with Node.js `24.13.1`.
- All four child reports and the aggregate passed.

This increment adds no DDL and changes no migration. Historical migrations remain unchanged, and the checked-in
Stage 3 migration boundary remains `000041_diff_artifact_kind.sql`.

## 7. Remaining release gates

This evidence closes the implemented **real Codex/Claude consolidated Local release slice**, not Stage 3
production release:

1. Real Codex/Claude SSH, Docker and Kubernetes matrices remain.
2. Registry-pushed immutable multi-arch image, signature, SBOM, reproducibility and rollout evidence remain.
3. Cross-Target Artifact/Checkpoint/Retention concurrency and long-Session/multi-Provider soak remain.
4. Real lossless large Terminal remains unsupported for Codex Unified Exec and Claude ambient authentication.
5. Production multi-node Drain/PDB/Eviction, canary, promote and rollback-under-load evidence remain.
