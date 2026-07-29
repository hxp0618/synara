# Stage 5 Codex Model-provider Credential Containment — Local Acceptance (2026-07-29)

## Scope and evidence boundary

This report covers the static custom model-provider configuration copied into Synara's isolated local `CODEX_HOME`.
The Provider and its tool subprocesses run under the same local OS identity; file mode `0600` protects against other
users, not against a model-authored command in the same runtime. A bearer token, authenticated URL, or secret query
parameter retained in `config.toml` would therefore violate the task-scoped credential boundary even if repository
configuration and MCP startup were otherwise isolated.

- Branch: `codex/saas-tenancy-user`
- HEAD: `0d400eaa6e46f1fc16dcae5010fca79770323cae`
- Evidence tree: dirty, with 187 porcelain entries when captured; this report describes the uncommitted working tree.
- Host: macOS 26.5.2 (`25F84`), Darwin 25.5.0 arm64.
- Codex: `codex-cli 0.145.0`; installed binary SHA-256
  `1da3f4e0e96028b8a771814293c3033dafd1971f943f6c7e79b0897fe705f590`.

This is local runtime/config evidence. It is not managed Kubernetes IAM/CNI evidence and does not replace the
Provider × Ready Worker production matrix.

## Gap found

The isolated-home sanitizer retained `base_url` and arbitrary static `query_params` as model transport fields.
Codex appends `query_params` directly to Provider requests. A configured value such as `api_key=...`, or credentials
embedded in URL userinfo/query/fragment, would remain plaintext in the isolated config and be readable by a
same-identity tool command.

The earlier MCP transport increment already moved literal provider bearer tokens to environment variables, enumerated
`env_key` / `env_http_headers`, excluded those variables from tool environments, and attested the effective shell
policy. Static URL/query credentials were the remaining path around that design.

## Implemented fail-closed profile

- Retained top-level and model-provider base URLs must be absolute HTTP(S) URLs with a hostname.
- URL userinfo, query strings, and fragments are rejected; they are not copied and silently relied upon.
- Inline `query_params` are rejected because their complete shape cannot be bounded by the line-oriented sanitizer.
- A dedicated `[model_providers.<name>.query_params]` table may contain only `api-version` with a
  `YYYY-MM-DD` or `YYYY-MM-DD-preview` value. This preserves the audited Azure-style non-secret compatibility case
  observed in the pinned Codex source while rejecting arbitrary static query credentials.
- Literal `experimental_bearer_token` continues to become a generated environment variable. Existing `env_key` and
  table-form `env_http_headers` mappings must use portable environment names, are explicitly excluded from tools, and
  are covered by startup `config/read` attestation.
- Other query-parameter requirements are deliberately unsupported in isolated mode until they receive an explicit
  non-secret schema or an environment-backed transport. Startup fails with an actionable error instead of weakening
  the credential boundary.

## Real Codex 0.145 probe

A temporary source home used a non-listening loopback custom Provider with an environment-backed key/header and the
allowed `api-version` parameter. Synara built the production isolated launch, then the probe called only `initialize`,
`hooks/list`, and `config/read`. It did not open a Thread, contact a model, or use a real credential.

Observed result:

```json
{
  "hookAttested": true,
  "configAttested": true,
  "modelProvider": {
    "base_url": "http://127.0.0.1:48124/v1",
    "env_key": "CUSTOM_PROVIDER_KEY",
    "env_http_headers": {
      "X-Provider": "CUSTOM_PROVIDER_HEADER"
    },
    "query_params": {
      "api-version": "2026-01-01-preview"
    }
  },
  "shellExclusions": [
    "CUSTOM_PROVIDER_KEY",
    "CUSTOM_PROVIDER_HEADER",
    "SYNARA_AGENT_GATEWAY_TOKEN"
  ]
}
```

The temporary home and app-server process were removed after the probe.

## Verification

- Codex manager focused file, rerun after the final empty query/fragment delimiter tightening: 123 passed / 2 skipped /
  0 failed.
- Shared full package: 453 passed / 1 skipped / 0 failed across 45 files.
- Provider Host full package: 155 passed / 0 failed across 11 files.
- Server full package: 3110 passed / 7 skipped / 0 failed across 286 files.
- Server build: PASS.
- Provider Host build: PASS.
- `git diff --check`: PASS.

The full Server run covered the completed URL/query sanitizer; the only subsequent code change made the existing
query/fragment rejection also catch empty `?` / `#` delimiters and received the focused rerun above, following the
repository's small-follow-up verification rule. Per repository instructions, workspace-level `bun fmt`, `bun lint`,
and `bun typecheck` were not run because the current conversation has not explicitly authorized them.

## Remaining Stage 5 gates

- Execute and retain the complete managed Codex/Claude × Ready Worker matrix.
- Close or explicitly exclude the remaining Provider-native result-provenance gaps.
- Implement the actual Stage 9 Issue webhook → automation provenance → unattended request → decline path.
- Run the final workspace format, lint, and typecheck pass after explicit authorization.

Stage 5 remains **IN PROGRESS**.
