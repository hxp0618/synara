# Stage 3 Provider acceptance harness

`acceptance_runner.py` is the target-agnostic entry point for Stage 3 acceptance. The Local driver starts an
isolated Personal Control Plane and exercises the production `LocalSupervisor` and `agentd` paths through user
APIs. The Docker driver creates a real managed Docker Execution Target through the same user API, then lets the
production `DockerPoolReconciler` create and replace the Worker container. Neither driver registers, heartbeats,
or claims a Worker on behalf of `agentd`.

The default Provider Host is the deterministic fixture in this directory, so a passing report proves the
Control Plane-to-Worker-to-Host protocol, container isolation, and recovery path; it does not prove a real Codex
or Claude adapter release.

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

Run the Docker fixture suite against the current checkout:

```sh
python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --target docker \
  --provider codex \
  --timeout 900
```

The Docker driver requires an accessible Unix Engine socket (default `/var/run/docker.sock`) and a container
route to its temporary host-side Worker-only proxy (default `host.docker.internal`). The full Control Plane stays
bound to `127.0.0.1`; the proxy exposes only `/v1/workers/*` and `/v1/artifact-content/*`. Override the socket and
advertised proxy host independently when needed:

```sh
python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --target docker \
  --docker-socket-path /absolute/path/to/docker.sock \
  --docker-control-plane-host host.docker.internal
```

By default it builds the root Dockerfile's `worker-acceptance` target under a unique local tag. To reuse an
already-built acceptance image without overwriting an operator-owned tag:

```sh
python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --target docker \
  --docker-worker-image synara-stage3-provider-acceptance:local \
  --docker-skip-worker-build
```

The Docker suite verifies the configured non-root user, named Workspace volume, network, memory/CPU limits,
Provider Policy, compatible Worker Manifest, container replacement, stable Worker slot, incremented Worker
incarnation, changed `instanceUid`, named-volume and Workspace-content continuity, an immediate Turn on the
replacement Worker, Control Plane restart, and contiguous Session Event sequence. Replacement is triggered
through the Provider Policy API so the real reconciler's config-hash path is exercised. Volume continuity is not
reported as Workspace Checkpoint restore.

Cleanup is narrowly scoped to the exact Execution Target label plus the run's unique container, volume, network,
and auto-built image. It never runs a Docker prune command. `--keep` still stops/removes the Worker container but
preserves the isolated Control Plane state, volume, network, and image for diagnostics.

The report status vocabulary is `pass | unsupported | skipped | fail`. Local, Docker, SSH, and Kubernetes have
product Target drivers; a missing driver is reported as `fail / runner.target_driver_missing`, never as an
infrastructure skip. On 2026-07-14 the deterministic Codex fixture passed all 13 SSH cases on a disposable isolated
OrbStack Ubuntu 24.04 VM, including pinned-Host-Key rejection, product install/upgrade/revoke, sshd restart, systemd
Worker replacement, Workspace continuity, Control Plane restart, second-Turn continuity, and exact machine cleanup;
a post-run report/log scan found no private-key patterns. This is live SSH Target evidence, not real Codex App
Server or Claude Agent SDK release acceptance. The fixture executes Codex and Claude Agent. Cursor, Gemini, Grok,
Kilo, OpenCode, and Pi produce an explicit `unsupported` report instead of being rejected before a report can be
written.

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
| `[workspace-verify]` | Reads and verifies the exact artifact sentinel already stored in the Workspace |
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
