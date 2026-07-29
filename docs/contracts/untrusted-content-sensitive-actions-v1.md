# Untrusted Content and Sensitive Actions v1

Status: FROZEN BASELINE, PARTIALLY IMPLEMENTED (2026-07-29).

This contract defines the minimum behavior when a Provider reads text or tool results that were not authored by the
current human user. Model compliance is useful defense in depth, but it is never the authority for credentials,
irreversible actions, or security-policy changes.

## Threat model and input inventory

The following inputs are attacker-controlled unless a stronger source-specific contract proves otherwise:

| Channel                                                         | Required source/trust declaration                              | Primary containment                                                                 |
| --------------------------------------------------------------- | -------------------------------------------------------------- | ----------------------------------------------------------------------------------- |
| Repository files, code comments, README and dependency metadata | `repository` / `untrusted-external`                            | Outer runtime sandbox; sensitive-action policy; task-scoped credentials             |
| Issue/PR body, title, review and comments                       | `automation` / `untrusted-external`                            | Approval-required worktree; source boundary; event-trigger allowlist                |
| External MCP client task prompt                                 | `external-mcp` / `untrusted-external`                          | Managed worktree and approval-required mode are immutable; project/capability scope |
| Agent-created Synara task prompt                                | `synara-mcp` / `untrusted-agent`                               | No privilege escalation relative to the caller; source boundary                     |
| Tool stdout/stderr and fetched web content                      | `tool-output` or `web-fetch` / `untrusted-external` (reserved) | Bounded structured result; egress policy; sensitive-action policy                   |
| Third-party MCP server result                                   | `external-mcp-result` / `untrusted-external` (reserved)        | Target egress allowlist; result provenance; no credential delegation by default     |
| Fork/handoff history                                            | `fork-import` or `handoff-import` / imported history           | Existing history boundary; never treated as a new current-user instruction          |

`reserved` means the source vocabulary and policy are frozen here but the corresponding path is not yet wired across
every Provider. Claude's successful native results are structurally wired as described below; Pi adds adjacent Host
provenance for both outcomes, and Codex adds adjacent Host provenance before supported successful or failed tool
outcomes in both local and managed runtimes. The remaining paths stay explicit `policy-only` and cannot receive the
server-authored `automation`, `external-mcp`, or `synara-mcp` task sources. A new external-content channel must choose
one of these declarations or extend this contract before shipping.

## Source and Provider-context invariants

- `OrchestrationMessage.source` is server-authored. Browser `thread.turn.start` decoding omits the field, so a caller
  cannot relabel external data as `native`.
- External MCP and Synara MCP creation write `external-mcp` and `synara-mcp` respectively. Automation dispatch derives
  `automation` from the server-only dispatch origin.
- Before Provider dispatch, these sources are rendered inside a mandatory `synara_untrusted_content` boundary with an
  explicit trust level. The content is carried as a JSON string with raw markup characters escaped so it cannot close
  its own boundary. If boundary overhead would exceed the Provider input limit, dispatch fails; it does not silently
  remove provenance.
- The Reactor must carry `provenanceWrappedMessageText` through ordinary, sidechat, context-bootstrap and stale-resume
  retry assembly; rebuilding any of those prompts from raw `input.messageText` is a security regression. The final
  decider independently forces `automation`, `external-mcp`, and `synara-mcp` messages to `approval-required`, even if
  an internal caller requests `full-access`.
- Heuristic prompt-injection indicators are alerting signals only. Audit stores a SHA-256 digest and bounded indicator
  IDs, never a second plaintext copy of the prompt.
- Every local Provider has an exhaustive, code-checked content-trust delivery declaration. Codex uses session
  instructions, Claude uses its system prompt, Cursor/Grok/Droid/OpenCode/Kilo/Pi use the first user content, and
  Antigravity carries an identity-only host block on every CLI Turn because it has neither a system-prompt transport
  nor a safely scoped Synara MCP connection. This policy explicitly classifies repository files, tool/terminal
  output, web content, and MCP results as untrusted. It is defense in depth, not a substitute for structural
  source/result provenance.

### Untrusted-dispatch Provider admission

Admission is the conjunction of four separately audited capabilities:

1. the adapter exposes a host-observed permission request that Synara can reduce to a fresh one-action approval; and
2. repository-owned executable startup configuration cannot run before that approval boundary is active; and
3. successful and failed native tool results both receive attested Host provenance before model consumption; and
4. the selected runtime is Host-started under that isolation profile, or independently attested to the same profile.

The local adapter boundary currently admits Codex and Claude. Local Codex app-server starts from a
distinct Synara-owned `CODEX_HOME`: it links only `auth.json`, keeps its own session store, and copy-on-write imports
only an explicitly resumed/forked rollout. The generated 0600 config retains a non-executable model-provider transport
subset, converts a literal provider bearer token into a tool-excluded process environment variable, and drops
command-backed provider auth, AWS auth, user/project MCP, hooks, plugins, rules, skills, profiles, and project trust.
Retained Provider base URLs must be absolute HTTP(S) URLs without userinfo, query, or fragment. Static
`query_params` are otherwise rejected; the sole audited compatibility shape is a dedicated-table `api-version` with a
date or date-preview value. Arbitrary static query credentials cannot be copied into the same-identity-readable config
and must instead use an enumerated, tool-excluded environment-backed credential transport.
Codex 0.145.0 or newer is required; `--strict-config` plus Host-owned CLI overrides disable executable extension,
external-memory-import, shell-snapshot, and unified-exec features and define either no MCP server or the one Synara
gateway. Unified exec is disabled because 0.145 intentionally skips `PreToolUse` for `write_stdin`; the remaining
one-shot `shell_command` path classifies every model-authored command before execution. Hosted web search is pinned
to `disabled`; code mode, browser use, and computer use are also disabled because they do not share the attested
ToolRegistry boundary. Before any thread opens, `config/read` must confirm every effective feature value, hosted
search mode, and the exact empty-or-`synara` MCP configuration requested by the Host. A local `synara` entry is
accepted only when its complete field set, numeric-loopback `/mcp` URL, enabled state, local environment identity and
`bearer_token_env_var` match the scoped session lease. The effective shell policy must also exclude that token variable
and every retained model-provider `env_key` / `env_http_headers` credential mapping, without reintroducing environment
state through `set`, `include_only`, non-default inheritance, or an experimental profile. Credential environment names
must be portable; `env_http_headers` uses a dedicated table so every mapping is enumerable and attestable. Session
start/fork derive URL and token from the same lease; discovery has no lease and attests `mcp_servers={}`. Local Claude uses Agent SDK
isolation mode (`settingSources: []`) plus `strictMcpConfig: true`. Every Synara-started OpenCode/Kilo command forces both the
provider-specific `*_DISABLE_PROJECT_CONFIG=1` flag and pure mode through `--pure` plus `OPENCODE_PURE=1` or
`KILO_PURE=1`. The environment setting covers bootstrap/child paths; the mandatory CLI flag makes a binary without
the audited pure-mode interface fail closed. Before opening the workspace, the Host also requires a parseable version
at or above the audited floors (OpenCode 1.15.11, Kilo 7.4.16). Pure mode retains built-in plugins but prevents
user/global external plugins from entering the same process. That closes executable startup configuration but not
native result provenance: their only public model-preconsumption `tool.execute.after` hook belongs to the external
same-process plugin chain that pure mode deliberately removes. OpenCode/Kilo therefore remain `policy-only` and are not
admitted for `external-mcp`, `synara-mcp`, or `automation`, whether Host-started or already running. Synara rejects them
in tool schema/capability projection, explicit command admission, automation create/update/run, and the durable
decider before Provider start or subagent steer. Ordinary human-authored turns may still use either runtime path. The
managed Provider Host separately admits Codex and Claude:
its Codex path has the isolated/attested hook boundary described below, and its Claude path also uses empty setting
sources and strict MCP configuration.

Cursor, Grok, and Droid do expose ACP permission requests, but that is not sufficient startup isolation. Cursor
automatically discovers project `.cursor/mcp.json`; no host-controlled disable switch is currently attested. Droid
now receives a private per-session runtime settings file that disables every hook, inherited startup autonomy, IDE
auto-connect, and cloud session sync, but project MCP/plugin configuration can still load. No equivalent exhaustive
Grok project-config kill switch has been proven. These three adapters therefore fail closed for `external-mcp`,
`synara-mcp`, and `automation`, alongside Antigravity/Pi, which lack the first capability, and OpenCode/Kilo, which
lack the third. Creation, automation create/update/run, and the final orchestration admission check all use the same
capability registries, with runtime configuration re-evaluated at the last responsible moment. Ordinary
human-authored local turns remain available and do not constitute evidence for unattended admission.

### Native tool-result provenance

- The host-owned successful-result schema is `synara.provider-untrusted-content.v1`. Its outer
  `__synaraUntrustedContent` object contains the frozen source (`repository`, `tool-output`, `web-fetch`,
  `external-mcp-result`, or `agent-output`), trust (`untrusted-external` or `untrusted-agent`), policy version, and
  Provider-native tool name. The original SDK value remains nested under `content`; lookalike keys in attacker data
  cannot overwrite the outer metadata.
- Claude Agent SDK exposes `PostToolUse.updatedToolOutput`. Both the managed Provider Host and local Claude adapter use
  it for successful native results. `AskUserQuestion` is the one explicit exception because its result is the live
  current-user response; downgrading it would erase trusted authorship.
- Claude `PostToolUseFailure` cannot replace the error value. The host therefore adds adjacent structured provenance
  through `additionalContext` without copying the failure body. This is weaker than a structural envelope and remains
  recorded as such.
- Codex 0.145 `PostToolUse` cannot rewrite `updatedMCPToolOutput` and runs only after a successful handler result, so it
  cannot cover MCP `isError`, rejected patches, declined approvals, or handler failures. Both managed and local Codex
  therefore install one exact Host-owned command as the sole session-flags `PreToolUse` hook. The managed command
  re-enters the immutable Provider Host bundle on the read-only Worker rootfs. The local command
  embeds the fixed bounded hook program in the attested flag, invokes only Synara's absolute process executable, and
  does not depend on a mutable workspace helper. Both paths start from an isolated `CODEX_HOME` without user hook
  trust state, then reject either missing hook or any additional enabled non-managed hook through startup `hooks/list`
  attestation. Codex 0.145 app-server does not propagate its CLI hook-trust bypass into the Thread Hook Engine, so
  every Host thread start/resume/fork also carries the request-level `config.bypass_hook_trust=true` override. The
  hook records bounded Host-authored `additionalContext` before execution for every supported non-user-answer tool;
  Codex stores it as a developer message before the model later consumes either the successful or failed native
  result. It never copies tool input, output, or error text, and `request_user_input` remains the explicit live-user
  answer exception. Codex does not project these developer-only entries through `thread/read`, so `hook/completed`
  plus the raw rollout are the audit surfaces. The same hook parses `apply_patch` file headers through the frozen path
  rules used by the Host classifier. In `approval-required`, Codex emits a path-bearing `item/started` before its
  pathless native file-change Approval; both Host paths cache that assessment by `itemId`, merge it into the Approval,
  and decline an Approval that lacks the preceding classified item. Sensitive actions in a permission mode that cannot
  ask are denied before execution and therefore produce no native result to label. Unified `write_stdin`, hosted web
  search, code mode, browser use, and computer use are not exposed. A real 0.145.0 request capture under the attested
  configuration contained only ToolRegistry-backed function/namespace tools plus the live-user exception; no
  `web_search`, code-mode `exec`, or code-mode `wait` tool was present. `tool_search`, when present for the sole
  Host-owned Synara MCP registry, returns Host metadata rather than third-party runtime content. Unsupported or
  malformed non-executing diagnostics are not counted as rewritten results, and a future Codex tool path is not
  covered until its effective configuration or Hook behavior is audited.
- Pi's direct SDK exposes a model-preconsumption `tool_result` replacement event. Synara forces project trust to
  `false`, then appends one hidden in-memory Host extension after the remaining user extensions. The extension
  prepends bounded success/failure provenance without copying attacker text and retains each original text/image
  block in its original order. This is recorded as `adjacent-host-context`, not as a structural envelope.
- ACP is Agent-owned at this boundary: the Agent executes tools, reports their results to the Client through
  `session/update`, and feeds the results back to its model internally. Rewriting Synara's received notification
  would alter only the UI/audit projection after model consumption, so Cursor/Grok/Droid remain `policy-only`.
- OpenCode/Kilo expose a model-preconsumption `tool.execute.after` hook, but only through external modules running in
  the Provider's same-process plugin chain. Synara forces project-config isolation and pure mode for every managed
  child/CLI command, so neither repository nor user/global external plugins load. That deliberately removes the only
  public result-rewrite hook instead of treating an un-attested module as Host authority. Without an inline built-in
  Host hook or an exact loaded-hook attestation interface, both Providers remain `policy-only`.
- Antigravity 2.0 exposes a Host-installed `PostToolUse` event, but its documented stdout contract is exactly `{}` and
  cannot rewrite or append to the native result. `PreInvocation` can inject a transient message, but the current local
  adapter's external plugin and script are writable by the same OS identity as Provider tools, so they are not an
  attested Host boundary. See [Google Antigravity Hooks](https://www.antigravity.google/docs/hooks).
- The exhaustive Provider capability registry records Claude as `host-structural-envelope` / `adjacent-host-context`
  for all runtimes, Pi as adjacent Host context for both outcomes in all runtimes, and Codex supported successful and
  failed results as adjacent Host context for all runtimes. ACP providers, OpenCode/Kilo, and Antigravity remain
  `policy-only` and are excluded from server-authored untrusted dispatch. Codex still has no host result-rewrite point.
  Sensitive-action approval, minimum credentials, egress, and the outer sandbox remain authoritative; policy text or
  adjacent context is never counted as a structural guarantee.

## Sensitive-action authority

The server-authoritative v1 categories are:

1. `protected-branch-publish`: any `git push`, force push, release creation, workflow dispatch, or merge action. v1 is
   intentionally broader than a default-branch-only parser because remotes and default branches can change during a
   turn.
2. `ci-workflow-change`: GitHub Actions, GitLab CI/includes, CircleCI, Buildkite, Drone, Woodpecker, Bitbucket,
   Jenkins, or Azure Pipelines configuration.
3. `dependency-change`: package/dependency commands, manifests, and lockfiles for JavaScript, Go, Rust, Java,
   Python, Ruby, PHP/Composer, SwiftPM, .NET, Elixir, Dart/Flutter, and Nix.
4. `credential-access`: credential stores, `.env`, SSH, cloud/cluster/package-manager configuration, secret-manager
   commands, credential helpers, environment enumeration, or bounded credential-like environment names.
5. `network-egress`: WebFetch/WebSearch, network commands, explicit hosts/URLs, or egress/proxy policy files.
6. `external-mcp-action`: calls to third-party MCP tools.

The classifier walks bounded nested tool input and evaluates tool names, command/cmd/script fields, file/path fields,
explicit host/URL fields, and content fields belonging to known mutating file tools. Git global options and package
manager global options cannot move a protected publish, fetch, or dependency mutation outside the classified command
window. For `Write`/`Edit`-style tools, newly supplied content containing a URL or credential path/environment
reference is sensitive; removal-only `old_string` content is not treated as newly introduced authority. Codex's
currently attested mutation surface exposes `apply_patch`, whose bounded patch command and file headers are scanned
conservatively by the inline Host guard. A future Codex content-key write tool is unsupported until that inline guard
and the tool-surface attestation are both extended and reaccepted.

Every classified action requires a fresh human approval. `acceptForSession` is downgraded to a one-action `accept`,
the session-wide allow state does not consume later sensitive requests, and the assessment is attached to request and
resolution events without copying command/file contents into the assessment. Claude's local and Provider Host paths
keep the permission callback active even in full-access mode; ordinary tools remain auto-allowed there, while
sensitive tools are routed to approval. A non-interactive Provider Host Claude run denies a sensitive PreToolUse hook
instead of waiting for an unavailable user. Codex request handling applies the same fresh-approval rule whenever Codex
surfaces a native approval request. Its exact Host `PreToolUse` hook independently classifies every supported local
tool call: if the active permission mode cannot surface a fresh human Approval, a sensitive call is denied before
execution and must be retried in `approval-required`; ordinary calls continue. For native file-change requests, the
Host correlates the preceding `fileChange.changes[].path` rather than trusting the pathless Approval payload, so a
dependency, CI, credential, or egress-policy edit retains its exact assessment and cannot consume a session-wide
grant. Cursor/Grok/Droid ACP permission
requests and OpenCode/Kilo permission requests now use the same server classifier: provider-side blanket allow is
disabled, sensitive Full Access requests remain visible,
and a requested session grant is normalized to a one-action grant in both the provider reply and resolution audit. If
an ACP Provider offers only a persistent allow option, the request is cancelled rather than widening a one-shot grant.
The cloud Control Plane accepts only one-action `accept` or `decline`, validates the bounded assessment vocabulary and
fresh/session booleans on canonical Runtime Events, and copies the same sanitized assessment from `request.opened` to
`request.resolved` so the remote product path does not lose the security decision during durable resolution.
The local Runtime Event contract now exposes the same canonical top-level assessment. Claude, Codex, ACP, and
OpenCode/Kilo adapters retain it on both request Events; orchestration Approval projection preserves it and derives
`sessionApprovalAvailable=false` whenever a fresh assessment is present. This keeps local UI/audit consumers from
having to recover security authority from Provider-specific nested arguments.

Antigravity and Pi do not expose a host-observed permission callback that can satisfy this contract. Cursor, Grok,
and Droid expose a callback but have not proved that repository MCP/plugin startup is isolated before it becomes
active. Server-authored `external-mcp`, `synara-mcp`, and `automation` content therefore cannot select any of these
five local adapters: creation validates before worktree allocation, automation create/update/run all share the same
gate, and orchestration admission is the final backstop. Their local, human-authored use is not evidence of the full
unattended boundary.

Current limitation: no user-space callback can mediate a syscall that a Provider performs without emitting a tool or
approval event. The Codex guard covers the attested ToolRegistry surface; unified exec and other audited hook-skipping
surfaces are disabled. A repository executable can still hide a later syscall behind an otherwise benign invocation,
so Codex and arbitrary executables require the outer sandbox, minimum credentials, and egress boundary. This contract
is not fully accepted until protected
push/credential/egress tests run
against every admitted Provider adapter and prove there is no unobserved bypass. Antigravity/Pi remain outside until
they add an observable permission protocol; Cursor/Grok/Droid remain outside until repository executable startup
configuration is disabled or exactly attested by the Host.
The explicit Kubernetes `metadata-egress` real Provider case now exercises this boundary without reading response
bodies or headers: Claude retains a full-access fresh Approval, while Codex runs in `approval-required`; both must
retain the pending/opened/resolved one-action `network-egress` assessment before the approved process runs the
value-free agentd isolation verifier. Success is the exact command with exit 0 and zero output/preview/segments.
Code coverage is not managed-cluster evidence; the case must still pass
for Codex and Claude on every admitted production Worker Node/CNI.
The complementary `credential-scope` case requires a fresh `credential-access` Approval from both admitted
Providers and checks only presence of the frozen ambient environment/path inventory. It emits no discovered value or
file content and deliberately excludes the intended execution-lifetime Provider broker token. Like metadata egress,
code coverage is not managed-cluster evidence; the exact-node Provider matrix remains mandatory.
The `malicious-issue-denial` case complements those positive probes with a negative real-Provider decision path. A
native user Turn replays attacker-authored Issue-shaped text and asks for a command containing both a protected push
and credential environment read. The command begins with `false &&`, so neither sensitive subcommand can run even if
mediation regresses. Claude `full-access` and Codex `approval-required` must both expose the canonical
`credential-access` + `protected-branch-publish` assessment; the Runner sends `decline` and accepts only if durable
opened/resolved Events retain it. A Provider may retain one fenced bounded-declined command/Terminal pair, but it must
have no exit/signal, command output, or Artifact and must prove `commandExecuted=false`. This
does not claim webhook provenance: the actual Stage 9 Issue webhook and unattended automation path remains a separate
completion gate. The Stage 5 aggregate matrix coordinator treats all three real-Provider cases as mandatory for every
Codex/Claude × Ready non-cordoned Worker cell; it does not allow an `unsupported` result to stand in for mediation.

## External MCP and credential scope

- External MCP credentials use the dedicated `synara.external-mcp` audience, expire, revoke immediately, are stored
  outside MCP configuration, and authorize only the integration's selected/all-project set plus explicit read/create
  capabilities.
- Stage 5 retires `runtime:local` and `runtime:full-access` as grantable authority. New integrations containing either
  scope are rejected. Existing rows may still decode for migration/UI compatibility, but runtime policy ignores the
  legacy grant and rejects local or full-access creation.
- Externally created work always uses a generated managed worktree and `approval-required`. The browser no longer
  offers switches that the server will reject.
- Provider Credential Grants remain execution-generation scoped. Agentd now replaces the long-lived Codex/Claude
  key with an Execution-lifetime loopback broker token before Provider Host starts; the broker pins the upstream
  origin and dies with Execution/Grant cancellation. Git fetch credentials are removed before Provider start,
  publish-type Git/Registry/Package bindings do not enter ordinary Provider Workloads, and ambient cloud/Git
  credentials remain outside the Provider environment. A future publish operation must introduce a separately
  approved brokered stage rather than resolving the currently excluded descriptors.

## MCP and egress boundary

MCP is not a trust upgrade. Third-party MCP return values use the same untrusted-content boundary as web/tool output.
MCP processes and network calls run inside the Execution Target's egress policy; they may not be launched as a host-side
proxy that bypasses Kubernetes NetworkPolicy or the future microVM boundary. Any new hostname/CIDR/proxy must be an
operator policy change and a fresh `network-egress` approval, not a model-authored local exception.

The current managed Provider Host does not admit third-party MCP servers: Claude uses `settingSources: []` and
`strictMcpConfig: true`, so repo, local and user settings cannot install MCP servers or hooks; Codex starts with the highest-precedence
`mcp_servers={}` override and an agentd-owned `CODEX_HOME`. A future host-defined MCP integration must add an explicit
result source boundary and run its process/network traffic inside the Execution Target before this deny-by-default
rule can be relaxed.

The current Synara External MCP bridge is loopback-only and is an inbound, scoped client integration rather than a
third-party outbound MCP server. Its task prompt is still untrusted because the paired client may have consumed an
attacker-controlled Issue, repository, or web page before calling Synara.

## Audit and alert chain

For untrusted external task creation the durable chain is:

```text
External MCP integration + requestId
  -> audit metadata (source, trust, prompt digest, indicator IDs)
  -> deterministic gateway operation
  -> Thread creationSource + user-message source
  -> Provider turn/request events
  -> sensitiveAction categories + fresh approval decision
  -> Provider/tool result
```

External MCP audit migration `088` persists source, trust, SHA-256, bounded indicator IDs and
`security_alert_kind=prompt_injection_suspected` without prompt plaintext. Its integration/request/project and created
Thread IDs join to the server-authored Message source; Provider runtime request events then supply Turn/request/item
IDs and sensitive-action categories. This is the durable alert feed and forensic chain. Notification delivery and the
Stage 9 malicious-Issue end-to-end test remain open; logging plaintext attacker content or credentials is prohibited.
