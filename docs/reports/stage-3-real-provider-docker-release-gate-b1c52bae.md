# Stage 3 real Provider Docker release gate — `b1c52bae`

## Result

- Status: **pass**
- Clean Git SHA: `b1c52bae6215505be4cc29ba81788ba1eccf3d27`
- Capability Catalog SHA-256: `742a7eef08fde2394438fb0a9ee008cf1d062576d3b884709c291ffc17e9bdeb`
- Gate schema: `synara.provider-docker-release-gate.v1`
- Gate duration: `965499 ms`
- Controlled Provider profiles used environment-backed Key, Base URL, and custom model inputs. No
  environment-variable name or secret value was persisted.
- A validated public, credential-free Go proxy override was enabled for the gate-owned Worker image build
  after the default module proxy twice failed before any Provider child run could start.

## Four-child matrix

| Provider      | Matrix  | Status | Cases                                     | Explicit unsupported               |
| ------------- | ------- | ------ | ----------------------------------------- | ---------------------------------- |
| `codex`       | product | pass   | pass=22, unsupported=1, skipped=0, fail=0 | `real-provider.terminal-large-log` |
| `codex`       | failure | pass   | pass=16, unsupported=0, skipped=0, fail=0 | none                               |
| `claudeAgent` | product | pass   | pass=22, unsupported=1, skipped=0, fail=0 | `real-provider.compact-boundary`   |
| `claudeAgent` | failure | pass   | pass=16, unsupported=0, skipped=0, fail=0 | none                               |

All four children used the same gate-owned Worker image:

- Image ID: `sha256:a7de83ca752c26c147c35d3c22eea39159bfdfd7ce7e87692204c64922b55b7d`
- Build metadata SHA-256: `e8ef2d9dd41c990910a43a1849f905c1387a24bcaacba8c921234069a9d04a69`
- Child builds skipped: `true`
- Ownership verified before cleanup: `true`
- Exact gate image removed: `true`
- Broad cleanup used: `false`

## Closed blockers

1. Codex real-Provider Fork now explicitly rebinds the source Session Credential through the user API. The
   Control Plane therefore re-runs `CredentialsUse` and scope validation while retaining the controlled
   third-party Key/Base URL profile. The formal product matrix passed Fork authoritative-history continuity.
2. Claude foreground Bash completion now preserves an explicit SDK exit code when available and reports
   `exitCode=0` for a successful synchronous completion when the SDK omits the numeric code. The formal product
   matrix passed the lossless `2 MiB + 257 B` Terminal case, including preview, three Ready Artifact segments,
   completion totals, exit code, restart continuity, Review, Rollback, and Fork.

## Security and cleanup

- Aggregate output scan: `40` files, `2637168` bytes, `0` findings.
- Each child required its own passing Secret scan and exact Target cleanup.
- Credential environment names persisted: `false`.
- Raw child output persisted in the aggregate: `false`.
- Gate-owned Worker image cleanup required and completed: `true`.
- No database DDL or migration changed in this blocker slice.

## Verification before the formal gate

- Focused Python Fork Credential regression: pass.
- Claude Provider Host test file: `27/27` pass.
- Focused Docker Codex Fork run: pass, including cleanup and Secret scan.
- Focused Docker Claude large-Terminal run: pass, including cleanup and Secret scan.
- `bun fmt --threads=1`: pass.
- `bun lint --threads=1`: pass with repository warnings and `0` errors.
- `bun typecheck`: `9/9` packages pass.
- Docker release-gate tests after adding the validated Go proxy input: `23/23` pass.

## Evidence boundary

This pass closes the implemented real Codex/Claude Docker product and controlled-failure release slice. It does
not close the SSH aggregate, Kubernetes release gate, registry-pushed production rollout, production KMS/signing,
approved production SLA/soak, or real-Provider remote concurrency/retention/load evidence.
