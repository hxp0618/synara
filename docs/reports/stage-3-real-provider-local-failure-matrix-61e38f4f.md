# Stage 3 Real Provider Local Failure Matrix

- Evidence date: `2026-07-16` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Implementation commit: `61e38f4f8eafcdb2f16a0952e8d7eea11c84b0de`
- Provider Host runtime: Node.js `24.13.1`
- Result: **PASS FOR THE IMPLEMENTED LOCAL FAILURE MATRIX; RELEASE GATE REMAINS OPEN**

## 1. Scope

The shared `real-provider-smoke` Runner executed the independent failure/recovery matrix through:

```text
user API -> Control Plane -> LocalSupervisor -> agentd -> Provider Host -> real Provider
```

The canonical matrix is intentionally separate from the eleven-case product/capability matrix:

```text
baseline Turn -> real HTTP 401 -> new-Execution recovery
              -> real HTTP 429 -> new-Execution recovery
              -> scoped Provider Host SIGKILL -> new-Execution recovery
              -> Cursor expiry -> Control Plane restart -> authoritative-history continuity
```

The HTTP fault endpoints listen only on `127.0.0.1`. They retain bounded method, path, credential-header name and
request-count metadata, but never retain request bodies or Credential values. The crash case waits for real
`item.started`, finds the unique `--protocol-v2` process only inside the isolated Control Plane process tree, and
sends `SIGKILL` without a broad process-name match. Cursor expiry uses `SYNARA_PROVIDER_CURSOR_MAX_AGE=1s`; the
Runner waits across the real expiry boundary and does not mutate Cursor bytes or database rows.

## 2. Clean-commit results

| Provider     | Runtime                                  | Case result | Duration | Result |
| ------------ | ---------------------------------------- | ----------- | -------: | ------ |
| Codex        | Codex App Server `0.144.5`               | `16 pass`   |  87.688s | pass   |
| Claude Agent | `@anthropic-ai/claude-agent-sdk 0.3.207` | `16 pass`   |  67.591s | pass   |

Both reports record:

- source Git SHA `61e38f4f8eafcdb2f16a0952e8d7eea11c84b0de` and `worktreeDirty=false`;
- Provider Capability Catalog SHA-256
  `8d47c4a08cdce16f0420c911737f92bf6b28ba49c6310e5601b7bd434f671f70`;
- exact runner-owned Local resource cleanup;
- an empty output Secret scan.

## 3. Failure and recovery evidence

| Case                 | Codex                                                                  | Claude Agent                                                           |
| -------------------- | ---------------------------------------------------------------------- | ---------------------------------------------------------------------- |
| Authentication       | HTTP 401 on `/v1/responses`, `Authorization`, 6 requests               | HTTP 401 on `/v1/messages`, `X-Api-Key`, 7 requests                    |
| Stable error         | `authentication_required`                                              | `authentication_required`                                              |
| Rate limit           | HTTP 429 on `/v1/responses`, `Authorization`, 1 request                | HTTP 429 on `/v1/messages`, `X-Api-Key`, 7 requests                    |
| Stable error         | `provider_rate_limited`                                                | `provider_rate_limited`                                                |
| Host crash           | scoped `SIGKILL`, no broad match, `provider_unavailable`               | scoped `SIGKILL`, no broad match, `provider_unavailable`               |
| Failure recovery     | every 401/429/crash recovery used a new successful Execution           | every 401/429/crash recovery used a new successful Execution           |
| Cursor expiry        | `native-cursor -> authoritative-history / cursor_expired`              | `native-cursor -> authoritative-history / cursor_expired`              |
| Continuity assertion | immediately previous exact marker restored after Control Plane restart | immediately previous exact marker restored after Control Plane restart |
| Duplicate terminal   | none                                                                   | none                                                                   |
| Credential leak      | none                                                                   | none                                                                   |

The Codex controlled-Credential path no longer reads ambient OAuth state. Agentd supplies the API key only through
the existing anonymous FD-to-environment path, while Provider Host creates an execution-local `CODEX_HOME` with an
official `model_providers` entry containing only the validated Base URL and environment variable name. The config
never stores the API key. Non-HTTP(S), userinfo-bearing, fragment-bearing or control-character Base URLs fail
closed.

Claude controlled-Credential runs retain execution-local `CLAUDE_CONFIG_DIR` isolation. The real matrix exposed
that the SDK can emit a stable 401/429 `api_retry` and otherwise retry internally for minutes before returning a
contradictory `subtype=success`, `is_error=true` result. Provider Host now ends only those stable authentication or
rate-limit retries immediately, preserves `api_error_status` as a terminal compatibility fallback, and leaves
other transient SDK retry classes to the SDK.

## 4. Classification corrections

The matrix closed three product-path defects without weakening acceptance assertions:

1. Generic `includes("auth")` classification treated words such as `authoritative` as authentication failures.
   Classification now uses explicit 401/authentication and 429/rate-limit markers.
2. Credential-backed Codex inherited the user's ambient `CODEX_HOME`, so a local OAuth session could bypass the
   controlled fault endpoint. Controlled runs now use the isolated execution-local config described above.
3. Claude hid stable 401/429 failures behind SDK retry and a contradictory terminal result. The Host now preserves
   the actual authentication/rate-limit category and returns control to Synara's new-Execution recovery policy.

## 5. Raw reports

| Provider     | Report directory                                                                                        | JSON SHA-256                                                       | Markdown SHA-256                                                   |
| ------------ | ------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------ | ------------------------------------------------------------------ |
| Codex        | `.tmp/stage3-provider-acceptance-results/20260716-61e38f4f-node24-codex-real-provider-failure-matrix/`  | `6747c2e7b52efca2db181d2f4467fb0f0946d588b381dc5eb4810f2ffcba9c56` | `b993ac0805023feee90054a606b6b0d9da798878ada6c878b3b570b03d3244de` |
| Claude Agent | `.tmp/stage3-provider-acceptance-results/20260716-61e38f4f-node24-claude-real-provider-failure-matrix/` | `b5f914c3d378cd279fc8a5a78bbaa9cd34c353f2706f35ca6987a632123f6463` | `1217e15afacd248f28fdac8c211baae3fd083d70e08de9a625aae591f894ae1c` |

The raw directories are intentionally ignored local artifacts. This checked-in report records their immutable
source SHA and report digests without committing logs, SQLite state, Workspaces, request payloads or credentials.

## 6. Verification and DDL boundary

- Provider Host passed `112/112` tests, package typecheck and build.
- Acceptance Runner passed `76/76` tests and Python bytecode compilation.
- Both clean-commit matrices used Node.js `24.13.1`, completed exact cleanup and reported zero Secret findings.
- `git diff --check` passed before the implementation commit.

This increment adds no DDL and changes no migration. Historical migrations remain unchanged, and the checked-in
Stage 3 migration boundary remains `000041_diff_artifact_kind.sql`.

## 7. Remaining release gates

This evidence closes the implemented **real Codex/Claude Local failure classification and recovery matrix**, not
Stage 3 production release:

1. The complete real Local capability, Artifact and failure suites still need one consolidated final-release run.
2. Real Codex/Claude SSH, Docker and Kubernetes matrices remain.
3. Cross-Target Artifact/Checkpoint/Retention concurrency and long-Session/multi-Provider soak remain.
4. Real lossless large Terminal remains unsupported for Codex `0.144.x` and Claude ambient authentication.
5. Registry-pushed immutable multi-arch image, signature, SBOM, reproducibility, promote/rollback and multi-node
   Kubernetes release evidence remain.
