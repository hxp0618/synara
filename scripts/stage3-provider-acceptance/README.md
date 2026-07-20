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

## Deterministic long-session soak

`--suite fixture-soak` reuses the same Control Plane, Target driver, agentd, Provider Host fixture, cleanup and
Secret-scan paths as the core suite. After the normal restart/second-Turn baseline, it runs `100` additional Turns by
default and restarts the Control Plane after every `10` completed Turns. The final case requires one distinct
Execution and exactly one terminal per Turn, a contiguous Session Event Sequence, and more than `500` Events so the
user API pagination path is exercised. Every soak Turn also requires text, Tool activity, Usage, Workspace dirty,
Artifact and ready Checkpoint evidence instead of counting an empty no-op Turn as progress.

```sh
python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --suite fixture-soak \
  --target local \
  --provider codex \
  --soak-turns 100 \
  --soak-restart-every 10 \
  --timeout 900
```

Use `--soak-restart-every 0` to keep the required baseline restart but disable additional restarts. The accepted
Turn range is `10..1000`; the restart interval must be smaller than the Turn count. This gate proves deterministic
long-Session, repeated Control Plane/Worker reconnect, event pagination and terminal integrity mechanics only. It is
also deterministic repeated Tool/Usage/Checkpoint evidence, but not real Provider, multi-node,
Retention-concurrency, load or production-duration soak evidence.

## Deterministic multi-Provider concurrency

`--suite fixture-concurrency` provisions one managed Docker Target with two agentd Workers and enables the Codex and
Claude fixtures on both. It creates two bound Sessions, caps the Tenant at two concurrent Executions, then holds one
Approval open in each Session at the same observation point. The case requires two distinct Executions on two
distinct Workers, both interactions pending before either is resolved, and verifies that resolving the secondary
Provider does not disturb the primary barrier. This is a deterministic Control Plane/Worker scheduling and Session
isolation gate; it is not real Codex/Claude, load, remote Target, or production concurrency evidence.

```sh
python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --suite fixture-concurrency \
  --target docker \
  --provider codex \
  --timeout 900
```

The primary Provider may be `codex` or `claudeAgent`; the suite automatically creates the other Provider as the
secondary Session. Other Targets and fixture failure/canary options are rejected so a passing report always carries
the same two-Worker overlap meaning.

## Deterministic bounded load and admission

`--suite fixture-load` reuses the managed Docker Target, two agentd Workers, Codex/Claude fixtures, bound Credential
delivery, Session APIs, report format, Secret scan and exact cleanup from the concurrency suite. It creates four
Sessions split evenly across both Providers and sets the Tenant concurrent Execution quota to two. Every wave runs
four Approval Turns with Text, Tool, Usage, generated Artifact and ready Checkpoint evidence. At three observation
points per wave, exactly two pending Executions must occupy distinct Workers. Attempts to admit a third Execution
must fail with `execution_quota_exceeded` without mutating Session Events or interactions; after one terminal, the
same rejected Session must be admitted immediately while the other Approval remains pending.

The canonical `25` waves complete `100` unique Executions, record `50` quota rejections followed by successful slot
reuse, and make `75` simultaneous-overlap observations. Use a smaller bounded value during development; the accepted
range is `2..100` waves.

For sustained resource-profiled measurement, add `--load-min-duration-seconds`. The load case continues with whole
waves until both the requested minimum wave count and minimum measured duration are satisfied. `--load-max-waves`
is the fail-closed safety bound (default `100`, maximum `10000`); if the duration is not reached before that bound,
the case fails instead of silently shortening the soak. The report records the exact Docker CPU/memory profile,
effective concurrency, execution success and unexpected-error rates, observed throughput, plus P50/P95/P99 wave and
quota-slot recovery latency. These values are diagnostic until an operator-approved SLA supplies thresholds.

```sh
python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --suite fixture-load \
  --target docker \
  --provider codex \
  --load-waves 25 \
  --load-min-duration-seconds 300 \
  --load-max-waves 100 \
  --timeout 900
```

This gate proves deterministic bounded quota/admission, two-Worker overlap, repeated Session reuse, durable
Artifact/Checkpoint completion, unique terminal mechanics and the requested resource-profiled measurement window. It
is not a real Codex/Claude performance result, multi-node evidence or an operator-approved production latency/error
SLA. A release claim must still use the approved production duration and thresholds rather than treating the example
values above as policy.

## Deterministic targeted failure under load

`--suite fixture-load-failure` reuses the same two managed Docker Workers and four Codex/Claude Sessions. With both
Execution slots occupied and the other two Sessions rejected without side effects, it resolves the selected
Execution through `agent_executions.worker_id -> worker_instances.pod_name` to one exact managed container. The first
barrier disconnects only that container from the runner-owned Docker network. The second barrier removes the exact
other busy container and requires the Docker reconciler to recreate the same logical Worker name with a changed
container ID and instance UID, an advanced Worker incarnation and preserved named-volume sentinel. The third barrier
kills exactly one Provider Host descendant inside the selected busy Worker container; that Execution must fail once
as `provider_unavailable`, while a new Execution on the freed logical Worker proves automatic Host recovery.

For all faults, the peer Session must retain byte-for-byte identical Events and pending Interaction identity while
the selected Execution emits `execution.recovering`, advances exactly one Generation, replaces its Request and
Interaction, and reaches one terminal path for Worker transport/loss faults. Provider Host process crash instead
retains the failed Generation 1 terminal and recovers through a distinct new Execution. The peer then completes on
its original Worker/Generation. The same four Sessions immediately continue through the bounded load/admission waves
above, including quota rejection, slot reuse, generated Artifacts and ready Checkpoints.

```sh
python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --suite fixture-load-failure \
  --target docker \
  --provider codex \
  --load-waves 25 \
  --timeout 900
```

This gate proves deterministic single-host network/container-loss/Provider-process targeting, managed replacement,
peer isolation and post-recovery load mechanics. It does not replace real Provider, multi-host, Kubernetes
multi-node, rollout failure, production SLA or production-duration soak evidence.

## Deterministic Retention/Cleanup concurrency

`--suite fixture-retention-concurrency` runs only on the isolated Local Target. It first creates a terminal generated
Artifact and a current ready Workspace Checkpoint, then holds a second fixture Turn at a pending Approval. The
runner applies the real Tenant retention policy and ages only the runner-owned SQLite rows so the real background
sweeper runs immediately instead of waiting days. While the Execution is active, the suite requires the Session,
Lease, Approval, current Checkpoint and physical Workspace generation to remain available, requires zero Workspace
cleanup commands, and allows only the unreferenced prior Artifact to be deleted. After resolving Approval, it
requires Session archival and one agentd-acknowledged physical Workspace cleanup while the current Checkpoint
Artifact remains ready. This is a deterministic Local state-machine gate, not real Provider, remote Target, load or
production-duration retention evidence.

```sh
python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --suite fixture-retention-concurrency \
  --target local \
  --provider codex \
  --timeout 180
```

The suite uses a `250ms` retention sweep interval and records that production time was not changed. Other Targets
and fixture failure/canary options are rejected so the report always has the same isolated Local cleanup meaning.

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
variable, pass only its variable name, and use the Provider Host installed in the Worker image. Entries in
`~/.synara-acceptance-env` must be exported to child processes: either declare each assignment with `export`, or add
one `export SYNARA_ACCEPTANCE_CODEX_KEY ...` declaration covering every configured Key, Base URL, and Model name.
Plain unexported shell assignments are intentionally invisible to the acceptance runner and fail closed:

```sh
source ~/.synara-acceptance-env

# Keep the Credential value out of the command line; pass only its variable name.
python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --suite real-provider-smoke \
  --target docker \
  --provider codex \
  --runner-command-json '["/usr/local/bin/provider-host"]' \
  --real-provider-credential-env SYNARA_ACCEPTANCE_CODEX_KEY \
  --real-provider-base-url-env SYNARA_ACCEPTANCE_CODEX_BASE_URL \
  --real-provider-model-env SYNARA_ACCEPTANCE_CODEX_MODEL \
  --real-provider-matrix \
  --timeout 1800
```

For SSH, use the owned disposable OrbStack target and the real Host command installed by the Runner:

```sh
source ~/.synara-acceptance-env

python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --suite real-provider-smoke \
  --target ssh \
  --provider codex \
  --runner-command-json '["/usr/local/bin/provider-host"]' \
  --real-provider-credential-env SYNARA_ACCEPTANCE_CODEX_KEY \
  --real-provider-base-url-env SYNARA_ACCEPTANCE_CODEX_BASE_URL \
  --real-provider-model-env SYNARA_ACCEPTANCE_CODEX_MODEL \
  --real-provider-matrix \
  --timeout 3600
```

The SSH real-provider path cross-builds `synara-agentd` and the Provider Host bundle from the current checkout,
installs the exact Codex and Claude Code versions from `deploy/worker/provider-tools/package-lock.json`, verifies the
remote CLI versions and Host SHA, and then provisions through the product SSH install API. The deterministic SSH
suite continues to upload only `provider-host-fixture.mjs`; fixture and real runtime artifacts are never confused.

An operator-owned host requires a separate, explicit boundary. The identity and pinned Host Key files must be
absolute, outside the repository, and the private key must be readable only by its owner. The command never accepts
password authentication and never reads user SSH configuration:

```sh
source ~/.synara-acceptance-env

python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --suite real-provider-smoke \
  --target ssh \
  --provider codex \
  --runner-command-json '["/usr/local/bin/provider-host"]' \
  --real-provider-credential-env SYNARA_ACCEPTANCE_CODEX_KEY \
  --real-provider-base-url-env SYNARA_ACCEPTANCE_CODEX_BASE_URL \
  --real-provider-model-env SYNARA_ACCEPTANCE_CODEX_MODEL \
  --real-provider-matrix \
  --ssh-external-host ssh.example.internal \
  --ssh-external-port 22 \
  --ssh-external-user synara-admin \
  --ssh-external-identity-file /secure/synara/id_ed25519 \
  --ssh-external-host-key-file /secure/synara/ssh-host-key.pub \
  --ssh-external-service-user synara \
  --ssh-external-use-sudo \
  --ssh-allow-external-host \
  --ssh-machine-arch amd64 \
  --timeout 3600
```

External-host preflight requires systemd, an active SSH service, the selected service user, non-interactive sudo when
requested, and the small host-tool set needed to unpack a pinned Node runtime. It refuses an existing ownership root
or any target-scoped service/binary/storage path before upload. Provider tools, HOME/XDG state, Workspaces and cache
stay under a unique ownership-marked root. Worker replacement upgrades only the Synara service; it never restarts
sshd or the host. Cleanup uses product `ssh/revoke`, verifies the target unit/binary is absent, removes only the exact
ownership-marked runtime, preserves the host and operator identity source, and keeps host/source paths out of reports.

The Runner reads the value only when creating the isolated Control Plane Credential, registers it with the output
redactor before the API call, binds the Credential ID to the real Provider Session, and never persists the variable
name or secret in reports. Agentd delivers the resolved Credential only through the existing anonymous FD 3 path;
it is not placed in Docker Target configuration, image environment, labels, or command arguments. The isolated
Control Plane state, Worker container, volume, network and auto-built image are removed during normal cleanup.

Use `--real-provider-credential-field authToken` only for a Claude token that intentionally uses that payload field.
`apiKey` is the default for Codex and Claude. An optional controlled endpoint can be supplied with
`--real-provider-base-url-env`; its value is also redacted. This is the supported third-party API-key/Base URL path;
the key and endpoint remain runtime Credential data rather than image or Target configuration. Read the endpoint's
non-secret model identifier from `--real-provider-model-env`, or pin it literally with `--real-provider-model`;
the report records only the resolved model identifier so an SDK default-model mismatch cannot be mistaken for
Provider failure. Remote real-Provider runs fail during CLI validation when the Credential source is omitted, so an
unauthenticated container failure cannot be mistaken for release evidence.

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

## Real Provider bounded load admission

`--suite real-provider-load` is the narrow remote load slice used by the consolidated Docker and Kubernetes real
Provider release gates. It runs only on `--target docker` or `--target kubernetes`, requires an explicit real Host
command plus a controlled Provider Credential environment-variable name, and keeps the same real Control Plane,
selected Target, agentd, Worker Protocol, and Provider Host product path as the smoke suite.

It opens four Sessions, keeps two Approval Turns active at once, requires exact `execution_quota_exceeded`
rejection for the third and fourth admissions, and then requires immediate slot reuse after the first two Turns
resolve. The consolidated Docker and Kubernetes release gates additionally require
`deploy/worker/production-load-sla.json`: each Provider load child runs for at least 1800 seconds, enforces the
checked-in P95/P99 Synara control-plane admission and post-saturation slot-reuse admission bounds with zero
unexpected errors, and restarts the Control Plane every 10 completed waves before verifying continued native-Cursor
execution. The enforced clocks stop when `POST /turns` returns `201`; third-party Provider inference is deliberately
excluded. `interactionReadyLatencyMs` and `turnCompletionLatencyMs` remain mandatory P50/P95/P99 evidence for
capacity planning, but a generic platform gate does not claim those model-dependent measurements are controlled by
Synara. This load slice still does not replace the separate failure-injection matrix, production-duration Retention,
or soak evidence.

Like remote smoke, pass only environment-variable names on the command line. The runner reads the values only when
creating the isolated Provider Credential, redacts them before any report write, and persists only the resolved
non-secret model identifier:

```sh
source ~/.synara-acceptance-env

python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --suite real-provider-load \
  --target docker \
  --provider codex \
  --runner-command-json '["/usr/local/bin/provider-host"]' \
  --real-provider-credential-env SYNARA_ACCEPTANCE_CODEX_KEY \
  --real-provider-base-url-env SYNARA_ACCEPTANCE_CODEX_BASE_URL \
  --real-provider-model-env SYNARA_ACCEPTANCE_CODEX_MODEL \
  --timeout 900
```

```sh
source ~/.synara-acceptance-env

python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --suite real-provider-load \
  --target kubernetes \
  --provider claudeAgent \
  --runner-command-json '["/usr/local/bin/provider-host"]' \
  --real-provider-credential-env SYNARA_ACCEPTANCE_CLAUDE_KEY \
  --real-provider-credential-field apiKey \
  --real-provider-base-url-env SYNARA_ACCEPTANCE_CLAUDE_BASE_URL \
  --real-provider-model-env SYNARA_ACCEPTANCE_CLAUDE_MODEL \
  --timeout 1800
```

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
source ~/.synara-acceptance-env

python3 scripts/stage3-provider-acceptance/local_release_gate.py \
  --runner-command-json '["/absolute/path/to/node-24.13.1","/absolute/path/to/apps/provider-host/dist/index.mjs"]' \
  --codex-credential-env SYNARA_ACCEPTANCE_CODEX_KEY \
  --codex-base-url-env SYNARA_ACCEPTANCE_CODEX_BASE_URL \
  --codex-model-env SYNARA_ACCEPTANCE_CODEX_MODEL \
  --claude-credential-env SYNARA_ACCEPTANCE_CLAUDE_KEY \
  --claude-credential-field apiKey \
  --claude-base-url-env SYNARA_ACCEPTANCE_CLAUDE_BASE_URL \
  --claude-model-env SYNARA_ACCEPTANCE_CLAUDE_MODEL \
  --product-timeout 1800 \
  --failure-timeout 420
```

The Credential, optional Base URL, and optional custom-model rules are the same as the remote gates below. Pass
only environment-variable names; never place values in the command or report. Use literal `--codex-model` or
`--claude-model` instead of the corresponding `--*-model-env` only when the model identifier is intentionally
pinned on the command line.

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
Credential, Worker image and cleanup requirements explicit. The current gate executes six child reports per run:

```text
Codex product matrix   + Codex failure matrix   + Codex load matrix
Claude product matrix  + Claude failure matrix  + Claude load matrix
```

The product and failure children use `real-provider-smoke`; the load children use `real-provider-load` with the
checked-in `deploy/worker/production-load-sla.json`. Each load child completes whole four-Session waves until both
its minimum wave count and `minimumDurationSeconds` are satisfied; the current production boundary is at least
`1800` seconds, subject to the runner's `400`-wave safety cap. Set both product Credentials out of band and pass
only their environment-variable names. When a third-party endpoint also needs controlled Base URL and model
settings, source the operator-owned acceptance env file first:

```sh
source ~/.synara-acceptance-env

python3 scripts/stage3-provider-acceptance/docker_release_gate.py \
  --codex-credential-env SYNARA_ACCEPTANCE_CODEX_KEY \
  --codex-base-url-env SYNARA_ACCEPTANCE_CODEX_BASE_URL \
  --codex-model-env SYNARA_ACCEPTANCE_CODEX_MODEL \
  --claude-credential-env SYNARA_ACCEPTANCE_CLAUDE_KEY \
  --claude-credential-field apiKey \
  --claude-base-url-env SYNARA_ACCEPTANCE_CLAUDE_BASE_URL \
  --claude-model-env SYNARA_ACCEPTANCE_CLAUDE_MODEL \
  --real-provider-load-sla-file deploy/worker/production-load-sla.json \
  --product-timeout 2400 \
  --failure-timeout 900
```

Use `--claude-credential-field authToken` only when the controlled Claude secret intentionally maps to
`ANTHROPIC_AUTH_TOKEN`. Optional Codex/Claude Base URLs are supplied through `--codex-base-url-env` and
`--claude-base-url-env`; Key, Base URL, and model may all come from the same third-party endpoint profile. Credential
and Base URL values are redacted. Models are non-sensitive: the child reports persist the resolved model identifiers
and the aggregate validates those exact values, but it does not record the model environment-variable names. Literal
`--codex-model` and `--claude-model` remain available when you want to pin the identifiers directly on the command
line instead of reading them from `--codex-model-env` / `--claude-model-env`. `--product-timeout` applies to both
the product and load children; `--failure-timeout` applies only to the failure children.

The gate fails before any build when either source is missing or invalid, and fails on a dirty/untracked worktree.
Each child receives only the tool environment allowlist plus that child Provider's Credential/Base URL; Codex and
Claude secrets are never co-inherited. After clean-SHA preflight, the gate builds one uniquely labeled official
`worker-acceptance` image and passes the same tag to all six children with `--docker-skip-worker-build`. Each child
must remove its exact container/volume/network/state resources, prove `ownedImageRemoved=false`, and leave the shared
image to the gate. A pass requires all six reports to reference the gate-built image ID, share one Capability
Catalog hash and clean Git SHA, preserve canonical product/failure/load coverage, use controlled rather than ambient
authentication, keep the load children on the non-fault Credential boundary, and leave both child and aggregate
Secret scans empty. In `finally`, including child failure paths, the gate verifies the image ownership labels and ID
before removing it without broad cleanup. The aggregate records that cleanup evidence in `docker-release-gate.json`
and `docker-release-gate.md`. Until a clean run with real controlled Credentials exists, this command is an
implemented gate rather than Docker release evidence.

## Consolidated real Provider Kubernetes release gate

`kubernetes_release_gate.py` uses the same controlled-remote gate engine, shared clean-SHA Worker image and the same
six-child product/failure/load structure as the Docker gate. Each child creates and removes its own disposable Kind
cluster, loads the shared image without rebuilding it, and runs one Codex/Claude product, failure, or load matrix:

```sh
source ~/.synara-acceptance-env

python3 scripts/stage3-provider-acceptance/kubernetes_release_gate.py \
  --codex-credential-env SYNARA_ACCEPTANCE_CODEX_KEY \
  --codex-base-url-env SYNARA_ACCEPTANCE_CODEX_BASE_URL \
  --codex-model-env SYNARA_ACCEPTANCE_CODEX_MODEL \
  --claude-credential-env SYNARA_ACCEPTANCE_CLAUDE_KEY \
  --claude-credential-field apiKey \
  --claude-base-url-env SYNARA_ACCEPTANCE_CLAUDE_BASE_URL \
  --claude-model-env SYNARA_ACCEPTANCE_CLAUDE_MODEL \
  --real-provider-load-sla-file deploy/worker/production-load-sla.json \
  --kind-bin /absolute/path/to/kind \
  --product-timeout 3600 \
  --failure-timeout 1200
```

The Credential, Base URL, and model environment rules are identical to the Docker gate. Preflight also requires a
working Docker Engine, Kind executable and kubectl client before the shared image is built. A child pass must prove
the owned cluster and isolated state were removed while `ownedWorkerImageRemoved=false`; the aggregate then verifies
all six nested `kubernetes.containerEngine` image IDs, Secret scans, Catalog hashes, clean Git SHA, and canonical
product/failure/load coverage before ownership-checking and removing the shared host image itself. Load children use
the same controlled product Credential boundary, repeat whole four-Session waves until the checked-in `1800`-second
SLA duration and minimum wave count are both met, and use no failure-only Cursor policy. It emits
`kubernetes-release-gate.json` and `kubernetes-release-gate.md`. The
implementation and preflight negative tests are not real Kubernetes Provider release evidence until dedicated
Credentials and a usable Kind binary are supplied and the clean-SHA command passes.

## Consolidated real Provider SSH release gate

`ssh_release_gate.py` runs the same Codex/Claude product and failure matrices in four independent child processes.
By default each child builds the current runtime, creates a unique owned OrbStack machine and one-time SSH key,
provisions through the product SSH API, and removes its machine, key and isolated state during cleanup:

```sh
source ~/.synara-acceptance-env

python3 scripts/stage3-provider-acceptance/ssh_release_gate.py \
  --codex-credential-env SYNARA_ACCEPTANCE_CODEX_KEY \
  --codex-base-url-env SYNARA_ACCEPTANCE_CODEX_BASE_URL \
  --codex-model-env SYNARA_ACCEPTANCE_CODEX_MODEL \
  --claude-credential-env SYNARA_ACCEPTANCE_CLAUDE_KEY \
  --claude-credential-field apiKey \
  --claude-base-url-env SYNARA_ACCEPTANCE_CLAUDE_BASE_URL \
  --claude-model-env SYNARA_ACCEPTANCE_CLAUDE_MODEL \
  --ssh-orbctl-bin /usr/local/bin/orbctl \
  --product-timeout 3600 \
  --failure-timeout 2400
```

The same aggregate can target one explicitly authorized, non-disposable host. Children run serially with distinct
installation/ownership IDs while sharing only the operator-provided identity and pinned Host Key source:

```sh
source ~/.synara-acceptance-env

python3 scripts/stage3-provider-acceptance/ssh_release_gate.py \
  --codex-credential-env SYNARA_ACCEPTANCE_CODEX_KEY \
  --codex-base-url-env SYNARA_ACCEPTANCE_CODEX_BASE_URL \
  --codex-model-env SYNARA_ACCEPTANCE_CODEX_MODEL \
  --claude-credential-env SYNARA_ACCEPTANCE_CLAUDE_KEY \
  --claude-credential-field apiKey \
  --claude-base-url-env SYNARA_ACCEPTANCE_CLAUDE_BASE_URL \
  --claude-model-env SYNARA_ACCEPTANCE_CLAUDE_MODEL \
  --ssh-external-host ssh.example.internal \
  --ssh-external-user synara-admin \
  --ssh-external-identity-file /secure/synara/id_ed25519 \
  --ssh-external-host-key-file /secure/synara/ssh-host-key.pub \
  --ssh-external-service-user synara \
  --ssh-external-use-sudo \
  --ssh-allow-external-host \
  --ssh-machine-arch amd64 \
  --product-timeout 3600 \
  --failure-timeout 2400
```

Preflight requires a clean SHA plus Go, Bun, OpenSSH and `ssh-keygen`; disposable mode additionally requires OrbStack.
The aggregate rejects reused runtime identities, fixture runtimes, unlocked/mismatched Provider versions, different
agentd/Host digests, a changed pinned Host Key in external-host mode, reused Host Keys across disposable machines,
incomplete product revoke/owned cleanup, non-canonical cases, persisted Credential/source metadata or any Secret
finding.
It emits `ssh-release-gate.json` and `ssh-release-gate.md`. Unlike Docker/Kubernetes, there is no shared image: each
child intentionally rebuilds and verifies the same runtime from the clean checkout. The implementation and
unit/runtime preflight evidence are not a real SSH Provider release pass until dedicated Credentials and a usable
repository-external identity are configured and all four clean-SHA children complete.

## Production Registry and Vault KMS admission gates

The production supply-chain boundary is separate from every runner-owned loopback Registry. Export the live
admission ConfigMaps and the approved Registry configuration/retention contract to repository-external files, then
run the production Registry gate. Pass only Credential environment-variable names:

```sh
kubectl --context production -n synara-system get configmap synara-worker-cosign-public-key -o yaml \
  > /secure/synara/synara-worker-cosign-public-key.yaml
kubectl --context production -n synara-system get configmap synara-worker-signing-settings -o yaml \
  > /secure/synara/synara-worker-signing-settings.yaml

python3 scripts/stage3-provider-acceptance/registry_release_gate.py \
  --image-repository registry.example.com/synara/worker \
  --builder synara-worker-release \
  --signing-policy-profile production \
  --registry-auth-username-env REGISTRY_USERNAME \
  --registry-auth-password-env REGISTRY_PASSWORD \
  --registry-ca-cert-env REGISTRY_CA_CERT \
  --production-public-key-configmap /secure/synara/synara-worker-cosign-public-key.yaml \
  --production-repository-configmap /secure/synara/synara-worker-signing-settings.yaml \
  --production-registry-config /secure/synara/registry-config.yml \
  --production-registry-retention-policy /secure/synara/registry-retention-policy.json \
  --production-registry-container synara-production-registry \
  --production-registry-runtime-config-path /etc/distribution/config.yml \
  --output-dir /tmp/synara-worker-registry-release
```

Production mode inspects the named running Registry container, reads its runtime configuration at the pinned path,
and binds the live container identity, TLS certificate, auth mode, repository, and deletion/retention settings to
the exported configuration and the checked-in retention contract. A disposable HTTP Registry, a static config
file without its live container, or a passing immutable-rollout gate is not production Registry evidence.

The retention contract pins the live Registry to
`registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373`. Production
evidence binds the requested container reference, actual runtime Image ID, and matching RepoDigest; mutable tags,
alias repositories, wrong digests, and Image-ID drift fail closed.

The self-hosted production signer is Vault Transit KMS with these immutable public controls:

- Helm chart `hashicorp/vault` `0.34.0`, release `synara-vault`, namespace `synara-kms`;
- image `hashicorp/vault:2.0.3@sha256:a296a888b118615dc01d5f1a6846e6d4a7277946caaed5b447008fff5fe06b54`;
- KMS reference `hashivault://synara-worker-release`, AppRole signer identity
  `auth/approle/role/synara-worker-release-signer`, and audited request path
  `transit/sign/synara-worker-release`;
- non-exportable, non-derived, non-deletable ECDSA P-256 Transit key with
  plaintext backup disabled; rotation is a staged admission-key-overlap change,
  not an automatic uncoordinated key-version switch;
- Credential environment names `VAULT_ADDR`, `VAULT_TOKEN`, and `VAULT_CACERT`; the token must be short-lived and
  policy-scoped, and no Credential value is persisted;
- Vault `lookup-self` must identify an AppRole `batch` orphan token with only the
  `synara-worker-release-signer` policy; evidence retains safe identity fields and the policy-list SHA256 only;
- required public Rekor `https://rekor.sigstore.dev` upload and online verification, including inclusion proof and
  signed entry timestamp;
- Kyverno `failurePolicy: Fail`, `validationFailureAction: Enforce`, tag-to-digest mutation, and digest signature
  verification using the live `synara-system` ConfigMaps.

After the Registry report passes, verify the live signer and admission boundary:

```sh
python3 scripts/stage3-provider-acceptance/vault_kms_admission_gate.py \
  --kube-context production \
  --vault-namespace synara-kms \
  --security-namespace synara-system \
  --admission-test-namespace synara-admission \
  --expected-approle-policy synara-worker-release-signer \
  --registry-release-gate-report /tmp/synara-worker-registry-release/worker-registry-release-gate.json \
  --unsigned-image-ref registry.example.com/synara/worker@sha256:<unsigned-digest> \
  --wrong-key-image-ref registry.example.com/synara/worker@sha256:<wrong-key-digest> \
  --tag-drift-image-ref registry.example.com/synara/worker:latest \
  --output-dir /tmp/synara-worker-vault-kms-admission
```

This gate requires the live Vault image/policy/key/public key, Registry report, Rekor result, ConfigMaps,
`ClusterPolicy`, positive signed-image admission, and negative unsigned/wrong-key/tag-drift probes to agree on the
same clean source boundary. It also requires exact cleanup and an empty output Secret scan.

The production operational KMS boundary also includes the checked-in
`deploy/kubernetes/security/vault/operations-policy.json` and
`docs/runbooks/vault-kms-operations.md` documents. Use the isolated snapshot restore drill to prove the approved
snapshot-operator AppRole, Shamir custody boundary, and exact `--network none` Docker restore path:

```sh
python3 scripts/stage3-provider-acceptance/vault_snapshot_restore_drill.py \
  --output-dir /tmp/synara-worker-vault-snapshot-restore
```

This drill consumes only the approved environment names `VAULT_ADDR`, `VAULT_CACERT`,
`VAULT_SNAPSHOT_OPERATOR_ROLE_ID`, `VAULT_SNAPSHOT_OPERATOR_SECRET_ID`,
`VAULT_SNAPSHOT_RESTORE_KEY_1`, `VAULT_SNAPSHOT_RESTORE_KEY_2`, and
`VAULT_SNAPSHOT_RESTORE_KEY_3`. It captures one source-cluster snapshot-operator token, restores the snapshot into
one isolated Docker Vault using `hashicorp/vault:2.0.3@sha256:a296a888b118615dc01d5f1a6846e6d4a7277946caaed5b447008fff5fe06b54`,
waits for the asynchronous snapshot application to finish, then verifies `vault status`, the single-node Raft
leader, both restored audit devices on a hardened UID-100 tmpfs, the transit key, and the
signer/auditor/snapshot-operator AppRoles,
then removes the container and temporary state. It does not replace the still-required external SIEM retained sink
evidence.

The SIEM evidence uses a separate S3-compatible Object Lock bucket with
versioning and `COMPLIANCE` retention. Configure only the environment names
`VAULT_AUDIT_WORM_MC_ALIAS`, `VAULT_AUDIT_WORM_MC_CONFIG_DIR`,
`VAULT_AUDIT_WORM_MC_HOST`, and `VAULT_AUDIT_WORM_MC_RESOLVE`; never put the
scoped archive credential value in a command or report. Each archive entry
contains the complete structured Vault audit payload; the gate recomputes its
canonical payload, entry, and batch hashes out of band. It writes a gate-owned
probe with the writer identity and requires writer deletion to fail at IAM,
then uses the independent verifier identity to prove the exact retained versions
cannot be deleted or shortened specifically because of COMPLIANCE Object Lock.
The gate additionally requires the short-lived
`VAULT_AUDIT_WORM_MC_VERIFIER_HOST` identity for its negative probes; this keeps
a writer-policy denial distinct from storage-enforced WORM denial.

The collector must be started with `--object-lock-required`; otherwise its
retention API explicitly reports only a tamper-evident local hash chain and the
formal gate fails. Run the formal evidence command from a clean SHA:

```sh
python3 scripts/stage3-provider-acceptance/vault_audit_siem_delivery_gate.py \
  --vault-command-json '["/secure/bin/vault"]' \
  --vault-auditor-token-env VAULT_OPERATOR_TOKEN \
  --kube-context kind-synara-stage3-prod \
  --vault-namespace synara-kms \
  --vault-statefulset synara-vault \
  --output-dir /tmp/synara-stage3-vault-audit-siem
```

## Kubernetes real Provider immutable rollout gate

Run the real rollout once for Codex and once for Claude from the same clean release boundary. The examples support
third-party API endpoints, keys, and custom models through environment-variable names:

```sh
source ~/.synara-acceptance-env

python3 scripts/stage3-provider-acceptance/kubernetes_real_provider_release_rollout_gate.py \
  --provider codex \
  --real-provider-credential-env SYNARA_ACCEPTANCE_CODEX_KEY \
  --real-provider-credential-field apiKey \
  --real-provider-base-url-env SYNARA_ACCEPTANCE_CODEX_BASE_URL \
  --real-provider-model-env SYNARA_ACCEPTANCE_CODEX_MODEL \
  --real-provider-load-sla-file deploy/worker/production-load-sla.json \
  --kind-worker-nodes 2 \
  --load-waves 6 \
  --timeout 5400 \
  --output-dir /tmp/synara-kubernetes-real-provider-codex-rollout

python3 scripts/stage3-provider-acceptance/kubernetes_real_provider_release_rollout_gate.py \
  --provider claudeAgent \
  --real-provider-credential-env SYNARA_ACCEPTANCE_CLAUDE_KEY \
  --real-provider-credential-field apiKey \
  --real-provider-base-url-env SYNARA_ACCEPTANCE_CLAUDE_BASE_URL \
  --real-provider-model-env SYNARA_ACCEPTANCE_CLAUDE_MODEL \
  --real-provider-load-sla-file deploy/worker/production-load-sla.json \
  --kind-worker-nodes 2 \
  --load-waves 6 \
  --timeout 5400 \
  --output-dir /tmp/synara-kubernetes-real-provider-claude-rollout
```

Use `--real-provider-credential-field authToken` only for a Claude profile that intentionally maps to
`ANTHROPIC_AUTH_TOKEN`. The default six nominal waves are split across candidate promotion and baseline rollback;
with six, each phase has a three-wave minimum. Each phase independently continues whole waves until it also reaches
the checked-in `minimumDurationSeconds: 1800`, so the two load phases require at least about 60 minutes before
build, rollout, fault, and cleanup overhead. The gate proves controlled real Provider Turns, two distinct immutable
Registry digests, canary/promote/rollback and Pod-loss recovery, one execution-pinned Pod/Worker identity per
Execution, two simultaneous Pods on distinct schedulable non-control-plane Nodes, same-Session native Cursor
continuity across revisions, Audit/Outbox consistency, exact cleanup, and output scanning.

Its Registry is runner-owned loopback HTTP without TLS or authentication. That Registry proves immutable digest
rollout only; production Registry TLS/auth/retention evidence still comes from the production
`registry_release_gate.py`, and live signer/tlog/admission evidence still comes from
`vault_kms_admission_gate.py`.

## Kubernetes immutable Worker Release rollout gate

`kubernetes_worker_release_rollout_gate.py` exercises the product Worker Release API and execution-pinned
Kubernetes reconciler without requiring a real Provider API key. Run it only from a clean committed checkout:

```sh
python3 scripts/stage3-provider-acceptance/kubernetes_worker_release_rollout_gate.py \
  --go-proxy https://goproxy.cn,direct \
  --kind-worker-nodes 2 \
  --output-dir /tmp/synara-kubernetes-worker-release-rollout \
  --timeout 3600
```

The gate starts a runner-owned loopback Registry, pushes baseline and candidate images from the same source SHA to
one repository, and creates a one-control-plane/two-Worker Kind cluster whose containerd mirror resolves only that
run's `localhost:<port>` authority through the owned Registry container. The main Target permits two simultaneous
execution-pinned Pods; a separate observer Target materializes the candidate Manifest before it is registered on
the main Target. It verifies:

- Registry-returned distinct immutable digests and real `imagePullPolicy=Always` pulls inside Kind;
- exact Pod image, runtime image ID, Worker Manifest, Release Revision, Channel, digest, Generation, and Node;
- `1 promote -> 2 canary -> 3 promote -> 4 rollback` strict-CAS history;
- one busy baseline Pod remains unchanged while a `100%` candidate canary Pod overlaps it;
- exact candidate Pod deletion preserves the immutable Release and advances only Generation `1 -> 2`;
- stale CAS rejection plus active baseline/candidate Execution fencing for promote and rollback;
- six bounded load waves across candidate promotion and baseline rollback, with four Sessions, two Providers, a
  maximum of two active Pods, exact quota rejection/retry, and one Worker identity per execution-pinned Pod;
- four release-pinned Approval Executions, six distinct seed/release Executions, one terminal per Execution,
  contiguous Session Event sequences, exact Audit/Outbox history, and an empty output Secret scan;
- deletion of the owned Kind cluster, Registry container/storage, both Worker images, and isolated state without
  prune or broad cleanup.

The Registry is disposable loopback HTTP without authentication. This gate is deterministic Kubernetes rollout
mechanics evidence, not production Registry TLS/auth/retention, real Provider, cloud CNI/Eviction, production KMS,
admission-policy, SLA, or production-duration soak evidence. The gate uses an explicit `12s` Lease TTL and `24s`
heartbeat timeout so local Kind-to-host jitter is not confused with the separate short-Lease network failure matrix.
Clean commit `39b9b328` passed `15/15` with `24/24` load Executions, `12/12` quota rejection/retry, and `18/18`
overlap observations; the formal boundary and hashes are in
`docs/reports/stage-3-kubernetes-kind-rollout-recovery-load-39b9b328.md`. The earlier no-load registry rollout
checkpoint remains in `docs/reports/stage-3-kubernetes-kind-registry-rollout-d1f3b68a.md`.

## Docker immutable Worker Release rollout gate

`docker_worker_release_rollout_gate.py` is the product Release Revision gate for the managed Docker pool. It does
not require a real Provider API key: the deterministic Provider Host fixture is used only to hold and complete an
Approval Execution while the gate exercises the real Control Plane, agentd, Docker reconciler, Worker Manifest,
Revision, Policy, Audit, Outbox, and Session Event paths.

Run it only from a clean committed checkout:

```sh
python3 scripts/stage3-provider-acceptance/docker_worker_release_rollout_gate.py \
  --go-proxy https://goproxy.cn,direct \
  --load-waves 25 \
  --output-dir /tmp/synara-docker-worker-release-rollout \
  --timeout 3600
```

The gate starts a dedicated loopback-only Registry and builds two single-platform `worker-acceptance` images from
the same Git SHA with distinct controlled versions and Registry-returned digests. A two-Worker main Target observes
the baseline image; a separate one-Worker observer Target makes the candidate Manifest available without first
running it on the main Target. The gate then verifies:

- two immutable Target-scoped Revisions and duplicate-Manifest rejection;
- initial promote, a `100%` Execution-selection canary that still preserves one promoted and one canary Worker,
  stale CAS rejection, and matching container/Manifest/Execution Revision plus Channel evidence;
- simultaneous baseline/promoted and candidate/canary Approval Executions occupy two Workers while two further
  Sessions receive side-effect-free quota rejection;
- exact candidate container loss advances one Execution Generation while preserving its logical Worker, Revision,
  Channel, Manifest, digest and named-volume content, without changing the baseline peer;
- recovered pending work blocks both candidate promote and rollback with `worker_release_active_executions`;
- `--load-waves` bounded waves are split across candidate/promoted and rollback baseline/promoted, with every active
  and terminal Execution checked for its exact release pin and Worker binding;
- exactly four immutable Transitions, load-safe paginated Audit history, topic-filtered six-message Outbox history,
  contiguous per-Session Sequence, no double Execution, no duplicate terminal, an empty output Secret scan, and
  exact resource cleanup.

The Registry uses loopback HTTP and no Registry Credential, so this gate is deterministic rollout/load evidence
rather than production Registry/TLS/auth or real Provider evidence. It does not close Kubernetes multi-node
rollout, production SLA, or production-duration soak. Cleanup stops the isolated Control Plane before removing only
the two Target pools, Registry container, three named volumes, network, and two owner-labeled Worker images; it
never runs prune.

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
  --kind-worker-nodes 2 \
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
| `kubernetes-pdb-drain`    | Kubernetes        | exact PDB blocks Eviction before graceful drain reschedules onto another still-schedulable Node          |
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

The PDB drain case additionally requires at least two Ready schedulable Nodes. For an owned cluster, request two
Kind Worker Nodes explicitly. The case first installs an owner-labeled, exact Target+Execution
`policy/v1` PodDisruptionBudget and proves a normal Eviction-backed drain is blocked with the original Pod still
running. It then deletes only that PDB, performs the existing graceful Pod DELETE drain, waits for the replacement
Pod to run on another Node while the source remains cordoned, and finally uncordons the source Node:

```sh
python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --target kubernetes \
  --provider codex \
  --kind-worker-nodes 2 \
  --failure-only \
  --failure-case kubernetes-pdb-drain \
  --timeout 1800
```

Reusable local contexts that share the host Docker image store, such as OrbStack, require a second explicit image
authorization. The runner then builds its uniquely tagged Worker image, configures `imagePullPolicy=Never`, records
the shared-container-engine transport, and removes only that exact image during cleanup:

```sh
python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --target kubernetes \
  --provider codex \
  --kubernetes-context orbstack \
  --kubernetes-allow-nondisposable \
  --kubernetes-shared-local-image-store \
  --timeout 1800
```

If the reusable Context's generated hostname route is unhealthy while its loopback listener remains reachable,
override only the API origin and preserve certificate verification with the Context's TLS server name. The Runner
applies these as per-process `kubectl` flags and does not modify the operator kubeconfig:

```sh
python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --target kubernetes \
  --provider codex \
  --kubernetes-context orbstack \
  --kubernetes-api-server https://127.0.0.1:26443 \
  --kubernetes-tls-server-name k8s.orb.local \
  --kubernetes-allow-nondisposable \
  --kubernetes-shared-local-image-store \
  --failure-matrix \
  --timeout 1800
```

Do not use `--kubernetes-shared-local-image-store` for a remote cluster or a Context whose nodes do not consume the
same local Docker images. The flag cannot be combined with `--kubernetes-worker-image` or
`--kubernetes-skip-worker-build`; remote/repository-backed images remain operator-provided and are never deleted by
the Runner.

The image canary creates a runner-owned local image alias, loads it into Kind or exposes it through the explicitly
authorized shared local image store, then creates a second Target via the real user API. It proves Target isolation,
image selection, Worker Manifest discovery, Approval round-trip, and baseline continuity. Because the alias points
to the same deterministic fixture image, it does **not** prove an immutable-digest promotion/rollback implementation
or a real Codex/Claude upgrade. Other non-Kind clusters are reported as explicit unsupported until a product release
revision API and caller-published immutable image exist.

All Docker networks/volumes and Kubernetes bootstrap/Target Namespaces receive a unique acceptance owner label.
Cleanup verifies that label before deleting reusable-cluster or Docker resources, uses exact Target labels for
Worker containers/Pods, and never runs prune or broad label-only deletion. On failure the report retains only
redacted Control Plane log paths plus a bounded container/Pod status summary; it does not dump SQLite, Credential
payloads, Workspace contents, or Kubernetes Secrets.

Kubernetes cleanup retries only idempotent ownership `get` and exact `delete --ignore-not-found` operations, at most
three attempts with bounded backoff, for transport failures such as `unexpected EOF` and API request timeouts.
Authorization failures, ownership mismatches, malformed responses, and other non-transient errors fail immediately.

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
Agent. Cursor, Antigravity, Grok, Kilo, OpenCode, and Pi produce an explicit `unsupported` report instead of being
rejected before a report can be written. Droid remains intentionally outside the remote Provider Host catalog.

Clean commit `1e826324` also passed `19/19` on the operator-approved reusable `orbstack` context using the explicit
shared local image store, `imagePullPolicy=Never`, Worker-only network interruption, exact `policy/v1` Pod Eviction,
an isolated local-image Canary, Control Plane restart, Session Sequence `1..119`, bounded idempotent cleanup retry,
exact Namespace/RBAC/image cleanup and zero Secret findings. See
`docs/reports/stage-3-kubernetes-orbstack-fixture-1e826324.md`. This remains deterministic fixture evidence, not a
real Codex/Claude, multi-node or immutable registry rollout gate.

## SaaS Web Structured User Input multi-browser acceptance

`apps/web/scripts/stage3-user-input-multibrowser.mjs` is a state-writing live acceptance harness for an isolated
SaaS Web, Control Plane, PostgreSQL, and MinIO stack. It creates its own Tenant-scoped Project, Session, Turn,
Execution, and fixture Worker, then verifies competing two-page resolution, stale auto-submit cancellation,
replacement-request draft isolation, acknowledged delivery, and one authoritative terminal Event.

Set the Worker registration token out of band. Pass only these environment variable names to the process; never
copy a token, Cookie, or Provider Credential into chat, shell history, screenshots, or reports:

```sh
export SYNARA_STAGE3_WEB_ORIGIN=http://localhost:57733
export SYNARA_STAGE3_CONTROL_PLANE_URL=http://127.0.0.1:60331
# Set SYNARA_WORKER_REGISTRATION_TOKEN out of band.
bun run --cwd apps/web stage3:user-input:multibrowser
```

Optional inputs are `SYNARA_STAGE3_EVIDENCE_DIR`, `SYNARA_STAGE3_USER_EMAIL`,
`SYNARA_STAGE3_MANUAL=1`, and `SYNARA_STAGE3_MANUAL_TIMEOUT_MS`. Automatic mode stores two terminal screenshots in
the evidence directory. Manual mode prints only the generated Session URL and non-secret dev-login identity, then
waits for the operator to resolve `manual-choice=Continue` in the browser.

Do not point this harness at shared or production state. Product requests go through the Web origin, Worker requests
go directly to the isolated Control Plane, Cookies remain in process memory, and failure/console diagnostics retain
only bounded problem codes or hashes rather than raw response bodies or console text.

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

The fixture reads the anonymous Credential FD only when `[credential]` is requested. The deterministic Credential
uses the same supported `api_key` / `apiKey` shape as product Provider Credentials and requires this exact Sentinel:

```json
{ "payload": { "apiKey": "stage3-provider-acceptance-credential-v1" } }
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
