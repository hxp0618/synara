# Stage 3 Real Provider Docker Third-Party Credential Gate at `f958c1b2`

- Evidence date: `2026-07-18` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Clean gate commit: `f958c1b2c48fddd7181382e59a404b1a1e4ea575`
- Aggregate run: `stage3-provider-docker-release-d77a2cb5-a349-460c-8c8d-57335860ba4d`
- Result: **FAIL CLOSED; THE CONFIGURED THIRD-PARTY PROVIDER PROFILES DO NOT QUALIFY FOR THE FULL DOCKER PRODUCT GATE**

## Evidence boundary

The consolidated gate loaded controlled Codex and Claude API keys plus optional Base URLs from an operator-owned
mode-`0600` environment file. Neither values nor operator environment-variable names were persisted. The gate built
one Worker image from the clean Git SHA, then ran four isolated child reports through the production Docker path:

```text
user API -> Control Plane -> Docker reconciler -> agentd -> Provider Host -> real Provider

Codex product   + Codex controlled failure
Claude product  + Claude controlled failure
```

Each child used one CPU, `2 GiB` memory, an isolated Docker network and its own Workspace volume. A failed Provider
capability remains failed; the gate does not retry it, relabel it as unsupported or convert it into release evidence.

## Aggregate result

The aggregate completed all four required runs in `1,326,557 ms`. Every report used clean Git SHA
`f958c1b2c48fddd7181382e59a404b1a1e4ea575`, Capability Catalog SHA-256
`742a7eef08fde2394438fb0a9ee008cf1d062576d3b884709c291ffc17e9bdeb` and Worker image ID
`sha256:b574d8e3731cb4b86e2514c482bdfd6c541f39096e1f7709485b33272ce726c8`.

| Provider | Matrix  | Result | Passed | Failed | Duration | Finding                                                                                              |
| -------- | ------- | ------ | -----: | -----: | -------: | ---------------------------------------------------------------------------------------------------- |
| Codex    | product | fail   |     10 |     13 |  24.494s | Baseline real Turn passed, but the approval-required Turn completed without an approval interaction. |
| Codex    | failure | pass   |     16 |      0 |  46.329s | Authentication, rate limit, scoped Host crash and Cursor expiry/restart all passed.                  |
| Claude   | product | fail   |      9 |     14 | 617.705s | Initial real Turn ended as `provider_unavailable` after the third-party endpoint returned HTTP 502.  |
| Claude   | failure | fail   |      9 |      7 | 622.468s | The independent matrix also failed its required baseline real Turn with the same HTTP 502 result.    |

### Codex product boundary

The first real text Turn completed and discovered a compatible Docker Worker. The next Turn explicitly requested one
shell command under `approval-required` mode. The Provider emitted reasoning/final-answer items and completed without
`command_execution`, `request.opened` or a pending Approval Interaction. The Runner correctly stopped immediately
with `runner.interaction_missing_after_terminal`; all later product cases remained prerequisite failures.

This matches the controlled Kubernetes and Local diagnosis for the same third-party profile. It proves text and the
complete controlled failure matrix, but not the tool/approval capability required by the frozen Tier 1 boundary.

### Claude product and failure boundary

Both Claude children reached the real Provider through the isolated Docker Worker, then failed their required first
Turn with stable code `provider_unavailable`. The retained redacted message is:

```text
Claude Agent SDK API request failed with HTTP 502.
```

The same profile already reproduced this result outside Kubernetes. The consistent Docker result confirms that the
configured third-party Claude endpoint, rather than Docker scheduling, Worker startup or Credential delivery, is the
current dependency failure.

## Credential, cleanup and Secret evidence

- The aggregate records only that both profiles used controlled API-key fields and controlled Base URLs. It records
  no operator environment-variable name or Secret value.
- Each child passed `environment.cleanup`: the exact managed Worker container, isolated network, Workspace volume and
  state directory were removed; no broad cleanup was used and no child deleted the shared image.
- The gate verified image ownership and exact image ID before deleting the shared image in its `finally` path.
- Post-run inspection found no matching managed Worker container or gate image.
- Each child passed its own output Secret scan. The aggregate scan covered `37` files and `4,971,618` bytes with zero
  findings.

## Report integrity

The ignored raw directory is
`.tmp/stage3-provider-acceptance-results/20260717T220122Z-65a00380-docker-release/`.

| Report               | JSON SHA-256                                                       | Markdown SHA-256                                                   |
| -------------------- | ------------------------------------------------------------------ | ------------------------------------------------------------------ |
| Docker aggregate     | `fa05ddcf853c0c6ebf36d11c6328f5109721b8beabb01f85a3cbaea51b0fbba2` | `3f6fcb4433c8de67c5c88b81f1bb654db2c3a5ef84aafdf8cbfd7e9231353a03` |
| Codex product        | `e30f22778abe54060f0680ebae8890074fab385027c8ac6b4b69bc8db91130a4` | `bb81a5a420dce5846225b54adbacd2cd31e505cf2d290c0af6a893585cbd30fa` |
| Codex failure        | `a8bf5b11d9b1bc6a551768e12c65f9b2d704af3b057a5ce7215982327b0b89fd` | `a725a77326389fd561e6e73b3b6b10629a299397a84f8a94883936b0836ddd74` |
| Claude Agent product | `466866ed3998768d777ed4ca41b6713d7139498e0b96abf0a80c93b34b572cc7` | `2b6dd7b23868f1544ea7b2da2296c1618f12320145b67907d5491c71cb62abf2` |
| Claude Agent failure | `80c1cd47d44a14217402e1bdd74376b2158c0d719e27ce7b87095907f8f48ec0` | `ffbce594e99bb106b040cb3b9f9fad8710bac3eae3826b2a28b9e4108b072c31` |

## Release status and required operator inputs

The controlled third-party API-key/Base-URL path is implemented and evidenced, but the Docker real Provider release
gate remains open. It can be rerun without a production-code change after both conditions are met:

1. provide a Codex API profile/model that supports Responses API command/tool calls and produces a real approval
   request under `approval-required` mode;
2. provide a Claude Anthropic-compatible streaming profile that completes the baseline request without HTTP 502.

If either profile is intentionally text-only or reduced-capability, it must remain a lower support tier and cannot be
used as Tier 1 Codex/Claude release evidence. This evidence increment changes no database DDL; the forward migration
boundary remains `000041_diff_artifact_kind.sql`.
