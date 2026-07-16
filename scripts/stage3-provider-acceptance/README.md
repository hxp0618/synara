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

## Real Provider two-Turn smoke

`--suite real-provider-smoke` replaces the fixture flow with a narrow real Codex or Claude Agent check through
the same Control Plane, Target, agentd, Worker Protocol, and Provider Host product path. It starts one Turn with
an exact generated marker, discovers the compatible Worker Manifest, validates canonical Runtime Event v2
assistant output, restarts the Control Plane, then requires the second Turn to repeat the marker through a
`native-cursor / cursor_usable` resume decision. The suite requires an explicit built Provider Host command and
does not accept fixture failure/canary flags.

Build the Host with the repository's required Node.js 24 runtime, then run Codex or Claude Agent:

```sh
bun run --cwd apps/provider-host build

python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --suite real-provider-smoke \
  --target local \
  --provider codex \
  --runner-command-json '["/absolute/path/to/node","/absolute/path/to/apps/provider-host/dist/index.mjs"]' \
  --timeout 600
```

Without controlled Credential options, the Local real smoke creates no Provider Credential and uses the local
user's existing Codex or Claude login. Credential-backed Claude runs keep the execution-local `CLAUDE_CONFIG_DIR`
isolation; ambient OAuth runs preserve the user's normal Claude configuration lookup so the Host does not silently
discard a valid login.

Remote Targets require an explicit controlled Provider Credential. Put the secret in an operator-owned environment
variable, pass only its variable name, and use the Provider Host installed in the Worker image:

```sh
# Set SYNARA_ACCEPTANCE_CODEX_KEY out of band; do not put its value in this command.
python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --suite real-provider-smoke \
  --target docker \
  --provider codex \
  --runner-command-json '["/usr/local/bin/provider-host"]' \
  --real-provider-credential-env SYNARA_ACCEPTANCE_CODEX_KEY \
  --real-provider-matrix \
  --timeout 1800
```

For SSH, use the owned disposable OrbStack target and the real Host command installed by the Runner:

```sh
python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --suite real-provider-smoke \
  --target ssh \
  --provider codex \
  --runner-command-json '["/usr/local/bin/provider-host"]' \
  --real-provider-credential-env SYNARA_ACCEPTANCE_CODEX_KEY \
  --real-provider-matrix \
  --timeout 3600
```

The SSH real-provider path cross-builds `synara-agentd` and the Provider Host bundle from the current checkout,
installs the exact Codex and Claude Code versions from `deploy/worker/provider-tools/package-lock.json`, verifies the
remote CLI versions and Host SHA, and then provisions through the product SSH install API. The deterministic SSH
suite continues to upload only `provider-host-fixture.mjs`; fixture and real runtime artifacts are never confused.

The Runner reads the value only when creating the isolated Control Plane Credential, registers it with the output
redactor before the API call, binds the Credential ID to the real Provider Session, and never persists the variable
name or secret in reports. Agentd delivers the resolved Credential only through the existing anonymous FD 3 path;
it is not placed in Docker Target configuration, image environment, labels, or command arguments. The isolated
Control Plane state, Worker container, volume, network and auto-built image are removed during normal cleanup.

Use `--real-provider-credential-field authToken` only for a Claude token that intentionally uses that payload field.
`apiKey` is the default for Codex and Claude. An optional controlled endpoint can be supplied with
`--real-provider-base-url-env`; its value is also redacted. Remote real-Provider runs fail during CLI validation when
the Credential source is omitted, so an unauthenticated container failure cannot be mistaken for release evidence.

`--real-provider-case generated-file-checkpoint` first requires a Provider-native file mutation (`apply_patch` for
Codex, `Write` for Claude) to create a 43-byte standalone file, then uses one exact shell command to write a
deterministic `1 MiB + 257 B` Workspace file. The first path must produce one downloadable Ready `generated_file`
Artifact before `workspace.dirty`; the second must produce
`workspace.dirty -> checkpoint.created -> workspace_snapshot artifact.ready -> checkpoint.ready` before Execution
completion. The Runner downloads both authenticated Artifacts, verifies exact Size/SHA-256 and metadata, rejects
unsafe or duplicate Snapshot members, and confirms that Session Events expose no physical paths. Shell output paths
are not inferred by parsing commands or scanning the Workspace; without a Provider-native exact path they remain
durable only through the Checkpoint. The large Diff gate remains separate.

The latest clean-worktree Codex and Claude Local matrices for this boundary are recorded in
`docs/reports/stage-3-real-provider-local-standalone-generated-file-matrix-be919393.md`. That evidence closes the
implemented Local standalone `generated_file` plus Workspace Checkpoint path only; Large Diff is independently
covered by the case below.

`--real-provider-case large-diff` creates a deterministic 5,000-line seed file, then requires Codex or Claude to
mutate it through a native file-change path. Provider Host keeps a bounded Diff inline and stages a larger UTF-8
Diff beneath the agentd-owned Runtime Output Root. The Runner requires exactly one Ready `diff` Artifact and one
Artifact-backed `turn.diff.updated`, downloads the payload through the user API, and verifies Size/SHA-256,
file/addition/deletion counts, Ready/reference/completion order, no inline large payload, and no physical path leak.
Claude reads only one bounded line before Write so the SDK satisfies its read-before-write rule without loading the
5,000-line file into context. The latest clean-worktree matrix is recorded in
`docs/reports/stage-3-real-provider-local-large-diff-matrix-90fae52c.md`.

`--real-provider-case terminal-large` adds the large-Terminal capability boundary before Control Plane restart.
The deterministic fixture still requires the exact `2 MiB + 257 B` stream, a 32 KiB preview, and
`1 MiB / 1 MiB / 257 B` Ready Artifacts. Real Codex `0.144.x` is explicit `unsupported`: Unified Exec retains only
a 1 MiB head/tail transcript, and the Runner does not disable it because that changes native durable Approval
semantics. Claude ambient OAuth is also explicit `unsupported`: lossless SDK retained output requires a controlled
Provider Credential so `CLAUDE_CONFIG_DIR` can be bound to the agentd-owned Runtime Output Root. The strict real
Provider assertion remains available for that controlled Claude path. The Runner never accepts a retained path
outside the root or reads the user's ambient credential files to manufacture a pass.

A base smoke pass without selected cases proves only two real Provider Turns, Control Plane restart, native Cursor
continuity, exact cleanup, and the report Secret scan for the selected Target. It does not replace Approval/User
Input, Artifact/large Terminal, failure matrix, immutable Worker image, four-Target, or soak Release Gates.

Every run ends with `security.output-secret-scan`. It scans generated JSON, Markdown, text metadata, and logs for
all runtime Secrets known to the redactor plus high-confidence private-key, AWS, GitHub, and OpenAI-style key
patterns. The report records only file, pattern name, and byte offset; it never echoes matched material. Binary
SQLite and Artifact payloads are deliberately excluded from this output scan and remain covered by their own
storage/SecretGuard acceptance.

## Real Provider failure and recovery matrix

Real Provider failures use a separate canonical run so the stable product/capability matrix is not polluted by
controlled 401/429 credentials, a deliberate Host kill, or a one-second Cursor policy:

```sh
python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --suite real-provider-smoke \
  --target local \
  --provider codex \
  --runner-command-json '["/absolute/path/to/node-24.13.1","/absolute/path/to/apps/provider-host/dist/index.mjs"]' \
  --real-provider-failure-matrix \
  --timeout 420
```

Docker uses the same cases with a controlled product Credential for the baseline and recovery Turns. Set the
secret out of band and pass only its environment-variable name:

```sh
python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --suite real-provider-smoke \
  --target docker \
  --provider codex \
  --runner-command-json '["/usr/local/bin/provider-host"]' \
  --real-provider-credential-env SYNARA_ACCEPTANCE_CODEX_KEY \
  --real-provider-failure-matrix \
  --timeout 1800
```

Use repeated `--real-provider-failure-case` flags for focused iteration. Product-path `--real-provider-case`
options and failure options cannot be combined in one run; each report therefore has one unambiguous evidence
boundary. The canonical failure cases are:

| Case                        | Product-path assertion                                                                             |
| --------------------------- | -------------------------------------------------------------------------------------------------- |
| `authentication`            | controlled Provider HTTP 401, stable `authentication_required`, no Credential leak, recovery works |
| `rate-limit-retry`          | controlled Provider HTTP 429, stable `provider_rate_limited`, new Execution recovery               |
| `provider-host-crash-retry` | kill only the active Target-scoped `--protocol-v2` descendant after `item.started`, then recover   |
| `cursor-expiry`             | expire the authenticated Cursor through policy, restart, and select `authoritative-history`        |

The fault server never retains request bodies or Credential values. Every run uses an unguessable route prefix
that is registered with output redaction and omitted from report paths. Local binds only loopback. Docker binds an
ephemeral host port, advertises the configured `--docker-control-plane-host`, and probes the endpoint from the exact
managed Worker container before creating the fault Session. Kubernetes uses the same configured host-gateway as its
Worker-only Control Plane proxy; the actual controlled Provider request from the execution-pinned Pod proves
reachability, so the unguessable endpoint is never persisted in a probe Pod specification. SSH reuses its already
established pinned-Host-Key Worker-only reverse relay: the local proxy temporarily registers only the exact random
fault route, forwards Provider Credential/version headers and 429 response metadata only for that route, and removes
the mapping when the fault server stops. It does not retain the one-time SSH private key or open a second tunnel
after provisioning. Docker Host crash injection executes inside the exact managed container. Kubernetes first
requires exactly one Running Pod for the Target, then executes inside its `agentd` container. SSH uses the managed
systemd service MainPID as its agentd root inside the owned disposable machine. All three walk only scoped agentd
descendants, require exactly one `--protocol-v2` process, and fail closed instead of using a host-wide, machine-wide,
or Namespace-wide process match. Codex controlled credentials use an execution-local `CODEX_HOME`; Claude controlled
credentials use the existing execution-local `CLAUDE_CONFIG_DIR`. Cursor expiry does not edit SQLite or Cursor
bytes. `--keep` can preserve isolated state for diagnosis, but that binary state is local-only evidence and must not
be committed.

The latest clean-worktree Node.js 24.13.1 Codex and Claude Local results are recorded in
`docs/reports/stage-3-real-provider-local-failure-matrix-61e38f4f.md`. Both pass all 16 cases, exact cleanup and the
output Secret scan. Docker 401/429 reachability and scoped Host crash have implementation-time unit, real container
and deterministic Target regression coverage. SSH now has token-scoped reverse-relay routing, actual-request
reachability accounting and systemd-MainPID-scoped Host crash unit coverage; a current dirty-worktree disposable
OrbStack fixture passes all 16 deterministic cases with exact machine/key cleanup and no Secret findings. A separate
no-Provider-call runtime preflight builds the real Host bundle from the checkout, installs the checked-in locked
Codex `0.144.1` and Claude Code `2.1.197` packages in a disposable Ubuntu 24.04 machine, verifies CLI versions and
the remote Host SHA, then removes the exact machine and local key material. Kubernetes has host-gateway 401/429
transport, actual-request reachability accounting, unique execution-Pod selection and shared Linux `/proc`
Host-crash coverage. No real SSH, Docker or Kubernetes Codex/Claude failure report exists without an
operator-provided controlled product Credential. The clean evidence therefore still closes only the Local failure
slice; SSH/Docker/Kubernetes release, concurrency and soak gates remain open.

## Consolidated real Provider Local release gate

`local_release_gate.py` keeps the product/capability and controlled-failure evidence in four independent child
reports, then validates them as one release unit:

```text
Codex product matrix   + Codex failure matrix
Claude product matrix  + Claude failure matrix
```

The gate requires a completely clean worktree, including no untracked files. It probes direct Node
`>=24.13.1 <25.0.0`, rebuilds `apps/provider-host/dist/index.mjs` from the current checkout, executes every child
against LocalSupervisor and emits `local-release-gate.json` plus `local-release-gate.md`:

```sh
python3 scripts/stage3-provider-acceptance/local_release_gate.py \
  --runner-command-json '["/absolute/path/to/node-24.13.1","/absolute/path/to/apps/provider-host/dist/index.mjs"]' \
  --product-timeout 1800 \
  --failure-timeout 420
```

A consolidated pass requires all four reports to share the same clean Git SHA and Capability Catalog hash, all
canonical cases to be present, no failed/skipped cases, only the frozen Local explicit-unsupported boundaries,
exact state cleanup, and an empty child output Secret scan. An explicitly unsupported case may become `pass` in a
new Provider version, but no new unsupported case is accepted silently. The aggregate stores only child report
paths, hashes, counts and bounded metadata; it does not retain child process output or credentials.

Interaction waits are terminal-aware. If a Provider completes, fails, cancels, or interrupts a Turn without
emitting the required Approval or User Input interaction, the child report fails immediately with
`runner.interaction_missing_after_terminal`. The consolidated gate does not retry that Provider behavior or turn
the missing interaction into an unsupported result.

## Consolidated real Provider Docker release gate

`docker_release_gate.py` uses the same shared clean-SHA child-report validator while keeping Docker-specific
Credential, Worker image and cleanup requirements explicit. Set both product Credentials out of band and pass only
their environment-variable names:

```sh
python3 scripts/stage3-provider-acceptance/docker_release_gate.py \
  --codex-credential-env SYNARA_ACCEPTANCE_CODEX_KEY \
  --claude-credential-env SYNARA_ACCEPTANCE_CLAUDE_KEY \
  --claude-credential-field apiKey \
  --product-timeout 2400 \
  --failure-timeout 900
```

Use `--claude-credential-field authToken` only when the controlled Claude secret intentionally maps to
`ANTHROPIC_AUTH_TOKEN`. Optional Codex/Claude Base URLs are supplied through `--codex-base-url-env` and
`--claude-base-url-env`; their values and all Credential values are registered with the aggregate redactor.

The gate fails before any build when either source is missing or invalid, and fails on a dirty/untracked worktree.
Each child receives only the tool environment allowlist plus that child Provider's Credential/Base URL; Codex and
Claude secrets are never co-inherited. After clean-SHA preflight, the gate builds one uniquely labeled official
`worker-acceptance` image and passes the same tag to all four children with `--docker-skip-worker-build`. Each child
must remove its exact container/volume/network/state resources, prove `ownedImageRemoved=false`, and leave the shared
image to the gate. A pass requires all four reports to reference the gate-built image ID and one Capability Catalog
hash, canonical product and failure coverage, controlled rather than ambient authentication, empty child and
aggregate Secret scans, and no persisted operator environment-variable names. In `finally`, including child failure
paths, the gate verifies the image ownership labels and ID before removing it without broad cleanup. The aggregate
records that cleanup evidence in `docker-release-gate.json` and `docker-release-gate.md`. Until a clean run with real
controlled Credentials exists, this command is an implemented gate rather than Docker release evidence.

## Consolidated real Provider Kubernetes release gate

`kubernetes_release_gate.py` uses the same controlled-remote gate engine, shared clean-SHA Worker image and four
isolated child boundaries as the Docker gate. Each child creates and removes its own disposable Kind cluster, loads
the shared image without rebuilding it, and runs one Codex/Claude product or failure matrix:

```sh
python3 scripts/stage3-provider-acceptance/kubernetes_release_gate.py \
  --codex-credential-env SYNARA_ACCEPTANCE_CODEX_KEY \
  --claude-credential-env SYNARA_ACCEPTANCE_CLAUDE_KEY \
  --claude-credential-field apiKey \
  --kind-bin /absolute/path/to/kind \
  --product-timeout 3600 \
  --failure-timeout 1200
```

The Credential and optional Base URL environment rules are identical to the Docker gate. Preflight also requires a
working Docker Engine, Kind executable and kubectl client before the shared image is built. A child pass must prove
the owned cluster and isolated state were removed while `ownedWorkerImageRemoved=false`; the aggregate then verifies
all four nested `kubernetes.containerEngine` image IDs, Secret scans and Catalog hashes before ownership-checking and
removing the shared host image itself. It emits `kubernetes-release-gate.json` and
`kubernetes-release-gate.md`. The implementation and preflight negative tests are not real Kubernetes Provider
release evidence until dedicated Credentials and a usable Kind binary are supplied and the clean-SHA command passes.

## Deterministic failure and canary matrix

The fault matrix is opt-in so the default core suite remains stable and fast:

```sh
python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --target local \
  --provider codex \
  --failure-matrix

python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --target docker \
  --provider codex \
  --failure-matrix

python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --target kubernetes \
  --provider codex \
  --failure-matrix \
  --timeout 1800
```

Use repeated `--failure-case` flags for a minimal targeted run. The canonical case order is stable regardless of
CLI argument order:

```sh
python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --target local \
  --failure-only \
  --failure-case provider-malformed \
  --failure-case provider-crash
```

`--failure-only` runs only isolated setup, one baseline smoke, the selected fault/canary cases, one final
continuity smoke, cleanup, and the output Secret scan. It is intended for focused iteration; a release report
still requires the default core suite plus the required failure matrix.

The available cases are:

| Case                      | Targets           | Product-path assertion                                                                                   |
| ------------------------- | ----------------- | -------------------------------------------------------------------------------------------------------- |
| `provider-malformed`      | all               | `protocol_violation`, one failed terminal, then a successful next Turn                                   |
| `provider-oversized`      | all               | oversized JSONL is `protocol_violation`, then Host recovery                                              |
| `provider-crash`          | all               | mid-Turn Host exit is `provider_unavailable`, then Host recovery                                         |
| `worker-network`          | Docker/Kubernetes | outage crosses the 6-second acceptance Lease TTL, then Generation-fenced Approval recovery               |
| `kubernetes-drain`        | Kubernetes        | exact Node cordon/drain/uncordon with a target+execution Pod selector and graceful Pod DELETE            |
| `kubernetes-eviction`     | Kubernetes        | `policy/v1` Eviction with exact Namespace, Pod name, and UID precondition                                |
| `kubernetes-image-canary` | Kind Kubernetes   | independent canary Target/Namespace/Session through the user API, followed by baseline Target continuity |

Docker network interruption disconnects only the exact managed Worker container. It runs automatically only on
the runner-created network; mutating a supplied network requires `--docker-allow-network-interruption`.
Kubernetes Worker network interruption closes connections at the runner-owned Worker-only proxy while the user API
remains reachable. This is deterministic transport-loss evidence, not proof that a particular CNI enforces
NetworkPolicy. The default outage is eight seconds and cannot be configured below seven seconds.

Kubernetes Node drain runs automatically only on an owned disposable Kind cluster. Reused contexts require both
the existing `--kubernetes-allow-nondisposable` gate and the separate
`--kubernetes-allow-node-drain` authorization. The runner always attempts `uncordon` in `finally`. Eviction stays
scoped to the unique acceptance Namespace and Pod UID and does not require Node mutation.

The image canary creates a runner-owned local image alias and loads it into Kind, then creates a second Target via
the real user API. It proves Target isolation, image selection, Worker Manifest discovery, Approval round-trip,
and baseline continuity. Because the alias points to the same deterministic fixture image, it does **not** prove
an immutable-digest promotion/rollback implementation or a real Codex/Claude upgrade. Non-Kind clusters are
reported as explicit unsupported until a product release revision API and caller-published immutable image exist.

All Docker networks/volumes and Kubernetes bootstrap/Target Namespaces receive a unique acceptance owner label.
Cleanup verifies that label before deleting reusable-cluster or Docker resources, uses exact Target labels for
Worker containers/Pods, and never runs prune or broad label-only deletion. On failure the report retains only
redacted Control Plane log paths plus a bounded container/Pod status summary; it does not dump SQLite, Credential
payloads, Workspace contents, or Kubernetes Secrets.

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
Server or Claude Agent SDK release acceptance. Clean commit `2763ebd3` also passed all 13 Kubernetes cases on an
owned disposable Kind cluster, including Pending Approval Pod deletion, Generation 1→2 Interaction replacement,
Artifact/User Input/Provider Error, Control Plane restart and Session Event Sequence 1→57. Post-run checks confirmed
the owned cluster and exact auto-built image were absent; see
`docs/reports/stage-3-kubernetes-provider-fixture-acceptance-2763ebd3.md`. The fixture executes Codex and Claude
Agent. Cursor, Gemini, Grok, Kilo, OpenCode, and Pi produce an explicit `unsupported` report instead of being
rejected before a report can be written.

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

| Directive              | Deterministic behavior                                                         |
| ---------------------- | ------------------------------------------------------------------------------ |
| `[text]`               | Canonical `content.delta`                                                      |
| `[tool]`               | Canonical tool item start/completion                                           |
| `[usage]`              | Canonical token usage event                                                    |
| `[approval]`           | Approval InteractionRequest, completed by ResolveApproval                      |
| `[user-input]`         | User-input InteractionRequest, completed by ResolveUserInput                   |
| `[artifact]`           | Creates and emits a Workspace-local artifact candidate                         |
| `[terminal-large]`     | Emits `2 MiB + 257 B` of deterministic safe Terminal output in 63 KiB chunks   |
| `[workspace-verify]`   | Reads and verifies the exact artifact sentinel already stored in the Workspace |
| `[credential]`         | Reads the anonymous FD once and returns boolean/key-only verification evidence |
| `[provider-error]`     | Stable `provider_rate_limited` terminal Error                                  |
| `[provider-malformed]` | Emits one malformed JSONL line for agentd protocol classification              |
| `[provider-oversized]` | Emits one over-limit JSONL line for agentd protocol classification             |
| `[provider-crash]`     | Exits the fixture process with status 73 during SendTurn                       |
| `[steer]`              | Pending turn completed by SteerTurn                                            |

Input without a directive defaults to `[text]`. Only one blocking directive (`approval`, `user-input`, or
`steer`) may be active in a SendTurn.

Every Target suite runs `[terminal-large]` through the real agentd collector. Acceptance requires an exact 32 KiB
Session Event preview, three `terminal_log` Artifact references at offsets `0 / 1 MiB / 2 MiB`, segment lengths
`1 MiB / 1 MiB / 257 B`, matching Ready Artifact size/SHA-256 metadata, correct completion totals, and no Runtime
Output physical path in Session Events. The escape-free 63 KiB fixture chunks leave room beneath the 64 KiB Runtime
Event payload limit; Artifact segmentation remains fixed at 1 MiB.

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
{ "payload": { "acceptanceToken": "stage3-provider-acceptance-credential-v1" } }
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
