# Stage 3 Deterministic Local Long-Session Soak Gate

- Evidence date: `2026-07-17` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Clean gate commit: `6e866a30487f20c0c699521cd6c136bfd27c9278`
- Gate run: `stage3-provider-acceptance-68e85643-a34e-48e2-abfe-7fcb16b77bf8`
- Result: **PASS FOR THE DETERMINISTIC LOCAL LONG-SESSION/RESTART/PAGINATION SLICE; REAL PROVIDER AND PRODUCTION SOAK REMAIN OPEN**

## 1. Scope and evidence boundary

The shared `acceptance_runner.py` added one `fixture-soak` suite without creating a second Target or Provider
framework. It reused the production Personal Control Plane, LocalSupervisor, agentd, Worker Protocol v2, Provider
Host Protocol 2.1 fixture, Workspace/Checkpoint/Artifact path, report writer, exact cleanup and output Secret scan.

The clean-SHA gate proves deterministic long-Session mechanics, repeated Control Plane and Local Worker reconnect,
Session Event pagination, unique Execution ownership, one terminal per Execution, and repeated text/Tool/Usage/
Workspace/Checkpoint behavior. It does not prove real Codex/Claude duration, multi-Provider concurrency,
Retention/Cleanup concurrency, load, remote Targets, multi-node behavior or production soak.

## 2. Canonical configuration

The canonical run used:

- Target: `local`.
- Provider descriptor: `codex` backed by the deterministic fixture.
- Additional soak Turns: `100`.
- Additional restart interval: every `10` completed Turns while more Turns remained.
- Overall timeout: `900s`.
- Source worktree: clean.
- Capability Catalog SHA-256: `8d47c4a08cdce16f0420c911737f92bf6b28ba49c6310e5601b7bd434f671f70`.

The ordinary core suite ran first, including Approval, User Input, large Terminal segmentation, Provider failure,
one baseline Control Plane restart and second-Turn continuity. The soak then added `100` Turns and `9` more
Control Plane restarts, ending at process generation `11`.

## 3. Long Session and Execution integrity

The soak case completed in `130,700 ms`; the full report completed in `146,669 ms`.

- Soak Turns requested/completed: `100/100`.
- Distinct soak Executions: `100`.
- Distinct Local Worker IDs: `1`, preserving the stable Local registration slot across Control Plane restarts.
- Execution Generation: `1` for each independent Execution.
- Double Execution: `false`.
- Duplicate terminal: `false`.
- Identity SHA-256 across the ordered Turn/Execution/Worker/Generation evidence:
  `e7bb7d352a41fb88db9a204d28547154211b00284a627bfe5ca4975b2ea7ee51`.

Every soak Turn was required to contain all of:

```text
turn.created
execution.leased
workspace.ready
execution.started
content.delta
item.started
item.completed
thread.token-usage.updated
workspace.dirty
checkpoint.created
artifact.ready
checkpoint.ready
execution.completed
```

An empty or partial no-op Turn therefore cannot satisfy the gate.

## 4. Restart continuity

The core suite performed one persisted-state restart before the second Turn. The soak performed nine additional
restarts after Turns `10`, `20`, `30`, `40`, `50`, `60`, `70`, `80` and `90`. Their pre-restart Session Sequences
were respectively:

```text
201, 331, 461, 591, 721, 851, 981, 1111, 1241
```

Each following Turn completed on the same authoritative Session. Cookies, SQLite metadata, Runtime Binding,
Workspace and Checkpoint state remained usable without reconstructing state from a unique old Worker process.

## 5. Event pagination and repeated Checkpoints

- Sequence before the soak: `71`.
- Events added by the soak: `1,300`.
- Final Session Event Sequence: contiguous `1..1371`.
- User API page size: `500`.
- Multi-page Event read: required and exercised.
- Full-run `checkpoint.created`: `105`.
- Full-run `checkpoint.ready`: `105`.
- Full-run `artifact.ready`: `109`.
- Full-run `workspace.dirty`: `105`.
- Full-run Tool `item.started` / `item.completed`: `102/102`.
- Full-run Usage events: `102`.

The final full-history read traversed more than two `500`-Event pages and revalidated exact Sequence continuity,
all `100` soak `turn.created` identities and exactly one terminal for each soak Execution.

## 6. Core suite and failure boundary

All `15/15` report cases passed. Before the soak, the same run covered:

- compatible Worker Manifest discovery;
- Credential/Project/Session binding;
- text, Tool, Usage, Artifact and Workspace Checkpoint flow;
- durable Approval and Structured User Input resolution;
- `2 MiB + 257 B` Terminal segmentation with bounded preview and Ready Artifacts;
- deterministic Provider rate-limit failure classification;
- Control Plane restart and second-Turn continuity.

The one expected `execution.failed` in the final event inventory belongs to the deterministic Provider-error case;
the soak itself required all `100` Turns to complete successfully.

## 7. Cleanup and Secret scan

Cleanup stopped the isolated Control Plane and removed the runner-owned temporary state directory. No SQLite,
Artifact payload, Workspace, Git cache or Credential state was retained.

The output Secret scan covered `14` JSON, Markdown and redacted log files totaling `1,542,440` bytes. It exercised
four known-secret canaries and found zero private-key, AWS access-key, GitHub-token or OpenAI-style key patterns.

The raw output directory was `/tmp/synara-stage3-fixture-soak-6e866a30/`. Its report hashes were:

| Report   | SHA-256                                                            |
| -------- | ------------------------------------------------------------------ |
| JSON     | `49e654a119522067cc188f7eb75ea0a8a0a91df0fed8d548abc68ad7c08bcd31` |
| Markdown | `d43cd84140d80726bfa259f9cfda874a245cea81a189e3bcda01b075a3273586` |

## 8. Automated validation and DDL boundary

- Acceptance Runner unit tests: `106/106`.
- All Stage 3 Python tests: `218/218`.
- `go test ./...`: pass.
- `go vet ./...`: pass.
- `go test -race ./internal/secretguard ./internal/agentd`: pass.
- Targeted repository formatter and `git diff --check`: pass.

This slice changes no database DDL. The checked-in forward migration boundary remains
`000041_diff_artifact_kind.sql`.

## 9. Remaining completion boundary

This report closes only the deterministic Local long-Session, repeated restart, Event pagination and repeated
Checkpoint mechanics slice. Workflow L remains `partial`. Stage 3 still requires:

1. real Codex and Claude long-duration Session evidence;
2. multi-Provider and multi-Session concurrency on one Control Plane;
3. Artifact Retention/Cleanup concurrency while Executions are active;
4. real SSH, Docker and Kubernetes consolidated Provider gates with controlled Credentials;
5. load, production-duration soak and production Registry/Kubernetes evidence.
