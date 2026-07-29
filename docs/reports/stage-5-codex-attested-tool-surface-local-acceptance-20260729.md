# Stage 5 Codex attested tool-surface local acceptance — 2026-07-29

## Scope

This report proves the currently audited Codex 0.145 tool-surface slice:

- every Synara-started local or managed app-server disables hosted web search, code mode, browser use, and computer
  use in addition to the existing executable-config isolation;
- startup reads the effective app-server configuration and fails before opening a thread unless every restricted
  feature, hosted search mode, and MCP server name matches the Host-owned profile; and
- a real Codex 0.145.0 Responses request under that profile exposes no hosted web-search or code-mode entrypoint.

It does not prove behavior of a future Codex tool path that uses a new configuration key, managed Kubernetes
isolation, or native result rewriting. Codex still provides adjacent Host context rather than a structural result
envelope.

## Repository state

- Branch: `codex/saas-tenancy-user`
- HEAD: `0d400eaa6e46f1fc16dcae5010fca79770323cae`
- Worktree: dirty before and during this slice (185 porcelain entries at capture time); no staging, commit, or push was
  performed by this acceptance run.

## Audited upstream source and binary

- Upstream tag: `rust-v0.145.0`
- Source commit: `25af12f7e61572b0bc18ddb1008be543b91519b0`
- Probe host: Darwin 25.5.0 arm64, macOS 26.5.2
- Official archive:
  `https://github.com/openai/codex/releases/download/rust-v0.145.0/codex-aarch64-apple-darwin.tar.gz`
- Official archive SHA-256:
  `072a30a65f05666735889ef0f60b56db186adbdde9d5c5cc1a64be0b598530fe`
- Extracted binary SHA-256:
  `1da3f4e0e96028b8a771814293c3033dafd1971f943f6c7e79b0897fe705f590`

The downloaded archive matched the Homebrew cask SHA-256. The extracted binary was byte-identical to the installed
`/opt/homebrew/Caskroom/codex/0.145.0/codex-aarch64-apple-darwin` used by the probe.

The pinned source establishes the relevant boundary:

- absent explicit configuration, Codex resolves web search to cached mode;
- hosted web search is added directly to the Responses tool specification rather than the local ToolRegistry;
- ToolRegistry invokes `PreToolUse` only when a handler supplies a pre-hook payload; and
- code mode has a custom outer `exec` call and a hook-skipping runtime-control `wait` path even though nested tools
  dispatch through the normal registry.

## Implemented fail-closed profile

The shared startup profile now pins top-level `web_search="disabled"` and disables the audited code-mode,
browser-use, computer-use, hosted-search, executable-extension, shell-snapshot, and unified-exec feature set.
`features.hooks` is then re-enabled only for the one exact Host-owned `PreToolUse` command.

After `initialize` and before any thread start, resume, fork, discovery thread, or managed Provider turn, Synara calls:

1. `hooks/list`, requiring the one exact enabled non-managed Host command and no diagnostic degradation; then
2. `config/read`, requiring `web_search=disabled`, every restricted feature at the expected effective boolean, and
   exactly either no MCP server or the sole Host-owned `synara` server.

An ignored CLI flag, project/user override, extra MCP server, re-enabled code mode, or re-enabled hosted search now
fails before the workspace is exposed to a model turn.

## Runtime observations

An isolated `CODEX_HOME` app-server `config/read` returned:

```text
web_search = disabled
features.code_mode = false
features.code_mode_host = false
features.browser_use = false
features.browser_use_external = false
features.browser_use_full_cdp_access = false
features.computer_use = false
features.standalone_web_search = false
features.unified_exec = false
features.hooks = true
mcp_servers = {}
```

A second isolated probe used a loopback-only custom Responses provider with a dummy key. It captured the exact first
request body and then stopped; no external Provider credential or model result was used. The request exposed:

```text
shell_command
update_plan
request_user_input
view_image
multi_agent_v1 namespace
get_goal
create_goal
update_goal
```

It exposed no hosted `web_search`, code-mode `exec`, or code-mode `wait`. The function and namespace tools above are
dispatched through ToolRegistry; `request_user_input` is the explicit live-user-result exception. When `tool_search`
is present for the exact Synara MCP map, its output is Host-owned registry metadata rather than third-party runtime
content. Third-party MCP remains disallowed by the same startup attestation.

## Repository verification

- Shared package: 45 files, 453 passed, 1 skipped, 0 failed.
- Provider Host package: 11 files, 155/155 passed.
- Server package: 790 assertion suites, 3,107 passed, 7 skipped, 0 failed.
- Server build: PASS.
- Provider Host build: PASS.
- `git diff --check`: PASS.

The workspace-level `bun fmt`, `bun lint`, and `bun typecheck` checks were not run because this conversation has not
authorized them. No result here substitutes for the managed Codex/Claude × Ready Worker Stage 5 matrix.

## Remaining boundary

- A future Codex release may add a new hosted/specialized tool or change Hook routing. It must be source-audited and
  added to the effective configuration attestation before that path is claimed.
- Unsupported or malformed calls that fail before ToolRegistry are non-executing Host diagnostics, not structurally
  rewritten native results.
- ACP providers, OpenCode/Kilo, and Antigravity remain `policy-only` for native result provenance.
- Managed cloud IAM/CNI, per-Worker metadata egress, and ambient-Credential absence remain production acceptance gates.
