# Stage 5 Codex MCP Transport Attestation — Local Acceptance (2026-07-29)

## Scope and evidence boundary

This report covers one local fail-closed increment: a Codex app-server that exposes the Host-owned `synara` MCP
server must prove the exact scoped transport and the shell exclusion for every retained Provider credential environment
variable before opening a Thread. It does not prove a managed Kubernetes Provider × Worker matrix, CNI behavior, cloud IAM, or the
Stage 9 webhook path.

- Branch: `codex/saas-tenancy-user`
- HEAD: `0d400eaa6e46f1fc16dcae5010fca79770323cae`
- Evidence tree: dirty, with 186 porcelain entries when captured; this report describes the uncommitted working tree.
- Host: macOS 26.5.2 (`25F84`), Darwin 25.5.0 arm64.
- Codex: `codex-cli 0.145.0`; installed binary SHA-256
  `1da3f4e0e96028b8a771814293c3033dafd1971f943f6c7e79b0897fe705f590`.

The installed-binary provenance and the wider hosted-tool surface audit are recorded in
[`stage-5-codex-attested-tool-surface-local-acceptance-20260729.md`](stage-5-codex-attested-tool-surface-local-acceptance-20260729.md).

## Gap found

The existing startup check compared only the effective MCP server names returned by `config/read`. A configuration
could therefore retain the expected name `synara` while changing its URL, bearer-token environment variable, headers,
enabled state, or another transport field. The same check also did not prove that the MCP bearer token remained in
`shell_environment_policy.exclude` and was not reintroduced through `set` or `include_only`.

The isolated model-provider sanitizer already converted a literal bearer token to an environment variable, but a
preconfigured `env_key` or `env_http_headers` credential mapping was not added to the same explicit exclusion set. With
Codex's default environment-filter behavior, relying on a credential-looking variable name was weaker than proving the
effective policy.

The local manager also assembled the MCP URL from a global endpoint callback while taking the token from a scoped
session lease. That split authority could pair credentials with a different endpoint. Discovery startup used the same
builder without a lease and could consequently carry an unauthenticated gateway configuration it did not need.

## Implemented invariant

`config/read` now passes only when the effective state is exactly the state requested by the Host:

- the actual MCP name set equals the expected set;
- every expected endpoint is numeric loopback HTTP, has an explicit port and the exact `/mcp` path, and has no URL
  credentials, query, or fragment;
- the `synara` entry contains exactly `url`, `bearer_token_env_var`, `environment_id`, `enabled`, and
  `tool_timeout_sec`;
- URL and bearer-token variable equal the scoped launch values, `environment_id=local`, `enabled=true`, and
  `tool_timeout_sec=null`;
- no command transport, literal headers, environment-header mapping, approval override, or other same-name field can
  be merged into the accepted entry;
- `shell_environment_policy.exclude` contains the gateway bearer-token variable and every retained model-provider
  `env_key` / `env_http_headers` credential mapping, while `set`, `include_only`, non-default inheritance, and an
  experimental profile cannot reintroduce or reinterpret process environment state;
- credential mappings use portable environment variable names; `env_http_headers` must use a dedicated TOML table so
  every value is enumerable before launch, while inline or malformed mappings fail closed.

Session start and fork now pass the URL and token from one `AgentGatewaySessionLease.connection` object into process
configuration and attestation. Discovery has no lease, starts with `mcp_servers={}`, and attests an empty set.

Managed Provider Host Codex remains deny-by-default with an exact empty MCP set; the shared stronger checker therefore
also protects that path without adding a managed MCP exception.

## Real Codex probe

A temporary isolated home launched the installed Codex 0.145.0 through the production local launch builder. The probe
used dummy gateway/model-provider credential values and non-listening loopback URLs, called only `initialize`,
`hooks/list`, and `config/read`, and never opened a Thread or contacted a Provider model.

Observed result:

```json
{
  "hookAttested": true,
  "configAttested": true,
  "mcpServer": {
    "url": "http://127.0.0.1:48123/mcp",
    "bearer_token_env_var": "SYNARA_AGENT_GATEWAY_TOKEN",
    "environment_id": "local",
    "enabled": true,
    "tool_timeout_sec": null
  },
  "expectedShellExcludedEnvVarNames": [
    "CUSTOM_PROVIDER_HEADER",
    "CUSTOM_PROVIDER_KEY",
    "SYNARA_AGENT_GATEWAY_TOKEN"
  ],
  "actualShellExclusions": [
    "CUSTOM_PROVIDER_KEY",
    "CUSTOM_PROVIDER_HEADER",
    "SYNARA_AGENT_GATEWAY_TOKEN"
  ],
  "discoveryMcpServers": [],
  "discoveryExpectedShellExclusions": [
    "CUSTOM_PROVIDER_HEADER",
    "CUSTOM_PROVIDER_KEY"
  ]
}
```

The temporary home and process were removed after the probe. No real gateway credential was read or persisted.

## Verification

- Server focused cross-module regression (`codexAppServerManager`, `CodexAdapter`, MCP injection, Codex process env):
  168 passed / 2 skipped / 0 failed.
- Shared full package: 453 passed / 1 skipped / 0 failed across 45 files.
- Provider Host full package: 155 passed / 0 failed across 11 files.
- Server full package: 3109 passed / 7 skipped / 0 failed across 286 files.
- Server build: PASS.
- Provider Host build: PASS.
- `git diff --check`: PASS.

Per repository instructions, workspace-level `bun fmt`, `bun lint`, and `bun typecheck` were not run because the
current conversation has not explicitly authorized them.

## Remaining Stage 5 gates

- Run the real Codex/Claude × Ready Worker matrix on every selected managed Kubernetes node and retain its IAM/CNI,
  resource-pressure, credential-absence, malicious-input denial, cleanup, and secret-scan evidence.
- Close or explicitly exclude the remaining Provider-native result-provenance gaps documented in the Stage 5 plan.
- Implement the actual Stage 9 Issue webhook → automation provenance → unattended tool request → decline path before
  accepting that future event source.
- Run the final workspace format, lint, and typecheck pass after explicit authorization.

Stage 5 therefore remains **IN PROGRESS**.
