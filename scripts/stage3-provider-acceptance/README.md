# Stage 3 Provider acceptance harness

`acceptance_runner.py` is the target-agnostic entry point for Stage 3 acceptance. The Local driver starts an
isolated Personal Control Plane and exercises the production `LocalSupervisor` and `agentd` paths through user
APIs. Its default Provider Host is the deterministic fixture in this directory, so a passing report proves the
Worker-to-Host protocol and recovery path; it does not prove a real Codex or Claude adapter release.

The Local driver requires a POSIX host because it owns and terminates the isolated Control Plane process group.

Run the Local fixture suite:

```sh
python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --target local \
  --provider codex \
  --timeout 240
```

JSON, Markdown, and redacted logs are written under `.tmp/stage3-provider-acceptance-results/` by default. Use
`--output-dir` for an explicit destination. `--keep` additionally preserves the isolated SQLite, Artifact,
Workspace, Git cache, and built Control Plane state beneath that destination.

The report status vocabulary is `pass | unsupported | skipped | fail`. Docker, SSH, and Kubernetes currently
return `fail / runner.target_driver_missing`; a missing product driver is not reported as an infrastructure skip.
The fixture executes Codex and Claude Agent. Cursor, Gemini, Grok, Kilo, OpenCode, and Pi produce an explicit
`unsupported` report instead of being rejected before a report can be written.

## Provider Host Protocol fixture

This directory contains a deterministic Provider Host Protocol 2.1 fixture for the Stage 3 protocol and fault
acceptance suite. It is not a substitute for Target acceptance using the built
`apps/provider-host/dist/index.mjs` and real Codex/Claude adapter paths.

Run it as a JSONL process:

```sh
SYNARA_PROVIDER_HOST_EXPERIMENTAL_PROVIDERS=codex,claudeAgent \
  bun run scripts/stage3-provider-acceptance/provider-host-fixture.ts --protocol-v2
```

`--enable-providers=codex,claudeAgent` can be used instead of the environment variable. The fixture describes
the repository's ordered 8 Provider by 28 Capability catalog. Codex and Claude Agent remain Experimental and
must be explicitly enabled; the other six Providers remain Local-only.

SendTurn scenarios are selected with composable `inputText` directives:

| Directive | Deterministic behavior |
| --- | --- |
| `[text]` | Canonical `content.delta` |
| `[tool]` | Canonical tool item start/completion |
| `[usage]` | Canonical token usage event |
| `[approval]` | Approval InteractionRequest, completed by ResolveApproval |
| `[user-input]` | User-input InteractionRequest, completed by ResolveUserInput |
| `[artifact]` | Creates and emits a Workspace-local artifact candidate |
| `[credential]` | Reads the anonymous FD once and returns boolean/key-only verification evidence |
| `[provider-error]` | Stable `provider_rate_limited` terminal Error |
| `[steer]` | Pending turn completed by SteerTurn |

Input without a directive defaults to `[text]`. Only one blocking directive (`approval`, `user-input`, or
`steer`) may be active in a SendTurn.

Protocol fault hooks are opt-in and fire once on the selected command:

```sh
bun run scripts/stage3-provider-acceptance/provider-host-fixture.ts \
  --fault=malformed --fault-on=Describe
bun run scripts/stage3-provider-acceptance/provider-host-fixture.ts \
  --fault=oversized --fault-on=SendTurn
```

The fixture reads the anonymous Credential FD only when `[credential]` is requested. It requires this exact test
shape and Sentinel:

```json
{"payload":{"acceptanceToken":"stage3-provider-acceptance-credential-v1"}}
```

It closes the FD, clears byte buffers, and returns only `credentialVerified` plus sorted payload key names under
`Result.payload.output.credentialEvidence`. Keeping the evidence inside `output` preserves it through agentd's
execution-compatible `RunnerResult` projection. The fixture never reflects Credential values or command payloads.
Secret-shaped command fields such as `credential`, `workerToken`, or `leaseToken` fail closed with
`protocol_violation`.

Focused verification:

```sh
bun run --cwd scripts test stage3-provider-acceptance/provider-host-fixture.test.ts
```
