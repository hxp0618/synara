# Stage 3 real Provider SSH release gate — `14f7dd2d`

## Result

- Status: **pass**
- Evidence date: `2026-07-19` (`Asia/Shanghai`)
- Clean Git SHA: `14f7dd2d569a6baac8b3d38e0cab49d1090c3785`
- Source worktree dirty: `false`
- Run ID: `stage3-provider-ssh-release-6ba20ce7-5877-4224-a0b9-cd9a5f240396`
- Gate schema: `synara.provider-ssh-release-gate.v1`
- Capability Catalog SHA-256: `742a7eef08fde2394438fb0a9ee008cf1d062576d3b884709c291ffc17e9bdeb`
- Provider Host SHA-256: `ad191bfb674eafc8039252a06ba1a2ee79f2176db2f7fa5d7a16210a4db92820`
- Agentd SHA-256: `5c0e6046ee6c82ecba5cd9601660b39f6f6662c2fc9185fb54401c71109b9352`
- Host Key fingerprint: `SHA256:MZqvwzkwDsysBjZ1HF/1NDgSqa7BdFGOiqUEuG3lQLw`
- Gate duration: `781696 ms`

The gate used one explicitly authorized, operator-owned external SSH host. Its address, identity source, Host Key
source, Credential environment-variable names, and secret values were not persisted. Both Provider profiles used a
controlled `apiKey`, controlled Base URL, and environment-selected custom model.

## Four-child matrix

| Provider      | Matrix  | Status | Cases                                     | Explicit unsupported               | Runtime identity                                                  | JSON SHA-256                                                       |
| ------------- | ------- | ------ | ----------------------------------------- | ---------------------------------- | ----------------------------------------------------------------- | ------------------------------------------------------------------ |
| `codex`       | product | pass   | pass=22, unsupported=1, skipped=0, fail=0 | `real-provider.terminal-large-log` | `stage3-provider-acceptance-c505b326-e469-4c26-82f9-510397c8590a` | `0a1ab242ccfae1d43da10cf7bdfc43d22398fe8db74dd766a374f9cf51170ab4` |
| `codex`       | failure | pass   | pass=16, unsupported=0, skipped=0, fail=0 | none                               | `stage3-provider-acceptance-6e8acd4f-bd78-4a12-b4c4-2b0902aa4e62` | `9a6248119be8b88bbc5196603098d497e1830caf560685d882d27fbda52342c8` |
| `claudeAgent` | product | pass   | pass=22, unsupported=1, skipped=0, fail=0 | `real-provider.compact-boundary`   | `stage3-provider-acceptance-61b24543-4569-45a8-be84-bd97d3ec50ad` | `6ddf0c0fac78eb22bfcb4d2624d6329a9278ebc8d1839e70c16e75452dac69cd` |
| `claudeAgent` | failure | pass   | pass=16, unsupported=0, skipped=0, fail=0 | none                               | `stage3-provider-acceptance-ac0fb572-bc40-4cb9-bcb6-33b006c95e35` | `3b74c9bfde80bd7917dcbc9547f96dc251c0e6f8bb7eec24a298716eca528db5` |

No failed or skipped case was accepted. The only unsupported results are the frozen Codex unified-exec large-Terminal
boundary and Claude Compact boundary.

## Controlled Provider profiles

- Codex model: `gpt-5.6-sol`
- Claude model: `claude-fable-5-dd-los-6.5-tpg`
- Controlled Base URL configured for both Providers: `true`
- Credential field for both Providers: `apiKey`
- Credential environment-variable names persisted: `false`
- External host address persisted: `false`
- Identity source persisted: `false`
- Host Key source persisted: `false`
- Ambient Provider authentication used: `false`

## Runtime isolation and cleanup

- Runtime: `authorized-external-host`
- Distinct runtime identities: `4/4`
- Worker lease TTL: `60s`
- Worker heartbeat timeout: `180s`
- Remote architecture: `arm64`
- Remote Node: `24.13.1`
- Codex CLI: `0.144.1`
- Claude CLI: `2.1.197`
- All four children shared the same clean SHA, Capability Catalog, Provider Host SHA, agentd SHA, locked Provider
  versions, and pinned Host Key fingerprint.
- Every child requested product `ssh/revoke`, removed only its ownership-marked runtime, preserved the external host
  and operator identity source, did not restart the host, and reported `broadCleanupUsed=false`.
- Post-gate inspection found no owned runtime marker, target-scoped systemd unit, temporary stage directory, or
  process owned by the temporary service user.

## Closed blockers

1. The external SSH host-crash case now reuses the canonical Approval-producing prompt and command shape, so Codex
   reaches a durable pending Approval before the scoped Provider Host crash.
2. External SSH Provider CLI verification and generated-file, Diff, and Terminal commands now use the target's pinned
   Node runtime instead of assuming a system Node installation.
3. External Node downloads now force HTTP/1.1 over IPv4 and use bounded all-error retries. This removes the observed
   HTTP/2 stream failure and OrbStack IPv6 tail-stall without weakening checksum verification or cleanup fencing.

## Security and DDL

- Aggregate output scan: `40` files, `3177374` bytes, `0` findings.
- Every child required its own passing Secret scan and exact Target cleanup.
- Raw child output persisted by the aggregate: `false`.
- Credential environment-variable names persisted: `false`.
- No database DDL or migration changed in this slice. The migration boundary remains
  `000041_diff_artifact_kind.sql`.

## Artifact digests

| Artifact                | SHA-256                                                            |
| ----------------------- | ------------------------------------------------------------------ |
| Aggregate JSON          | `7d702a0f189062cbd8f8718609e887b9ea0c12f02426316bde0bba44ca16d6da` |
| Aggregate Markdown      | `70d495ae7a30cd41b21027e6fd8550cedb751ef53d592b6d5f5213b6f67abab1` |
| Codex product JSON      | `0a1ab242ccfae1d43da10cf7bdfc43d22398fe8db74dd766a374f9cf51170ab4` |
| Codex product Markdown  | `230faed2548c9b4c2679b247cc2b14fcc96bf171d26d8b5c105fcef66c95a8ec` |
| Codex failure JSON      | `9a6248119be8b88bbc5196603098d497e1830caf560685d882d27fbda52342c8` |
| Codex failure Markdown  | `9fc9025a86a39d1539947d6e6e260384500ada212adcfd91cbc93300a495c453` |
| Claude product JSON     | `6ddf0c0fac78eb22bfcb4d2624d6329a9278ebc8d1839e70c16e75452dac69cd` |
| Claude product Markdown | `d25dd0e28eb8fb800f98ffac77fcf9b4c7e32cf6de011963de2f42f5625b614c` |
| Claude failure JSON     | `3b74c9bfde80bd7917dcbc9547f96dc251c0e6f8bb7eec24a298716eca528db5` |
| Claude failure Markdown | `24825ac80078c3b1d8337fc66d60f94c2b4b6e9af1b4ed708c334211253d51cd` |

## Verification before documentation

- External SSH full-download probe over HTTP/1.1 and IPv4: pass (`29964164` bytes).
- Acceptance Runner unit suite: `197/197` pass.
- SSH release-gate unit suite: `18/18` pass.
- Focused Claude failure matrix on clean `14f7dd2d`: `16/16` pass with exact cleanup and zero Secret findings.
- Formal clean-SHA SSH four-child aggregate: pass.
- `bun fmt`: pass.
- `bun lint`: pass with `288` existing warnings and `0` errors.
- `bun typecheck`: `9/9` packages pass.

## Evidence boundary

This pass closes the implemented real Codex/Claude authorized external-SSH product and controlled-failure slice. It
does not close a same-release-SHA four-Target rerun, production multi-node Kubernetes rollout, production
Registry/KMS identity/tlog/admission, approved production SLA/soak, or real-Provider concurrency, Retention, load,
and rollout evidence.
