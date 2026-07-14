# Provider Host Protocol v2.1

Provider Host Protocol v2.1 is the versioned JSONL boundary between `synara-agentd` and a Provider Host. It is
independent from Worker Protocol and Runtime Event versions.

The schema source of truth is `packages/contracts/src/providerHost.ts`.

## Version

```json
{ "major": 2, "minor": 1 }
```

- Major mismatch is incompatible and makes the Host/Provider combination non-schedulable.
- A newer Minor may add optional fields. Unknown optional fields are ignored.
- Missing required fields, unknown commands and undeclared capabilities are rejected.

## Command envelope

Every stdin command is one JSON line with:

```text
requestId, protocolVersion, executionId, generation,
commandType, commandId, occurredAt, payload
```

Supported command IDs are defined by `PROVIDER_HOST_COMMAND_TYPES`. A retry with the same `commandId` must not
repeat Provider Turns, tool side effects or Fork creation.

## Message envelope

Every stdout line is one of:

```text
Event, InteractionRequest, ArtifactCandidate, Checkpoint,
Result, Error, Progress
```

Each command produces exactly one terminal `Result` or `Error`. Output after a terminal message, a duplicate
terminal, malformed JSONL or an oversized line is a `protocol_violation`.

Provider diagnostics use stderr, are bounded and redacted, and are never parsed by Control Plane. Large logs,
files, Diffs and snapshots use Artifact references.

Agentd owns the Host process and its managed process scope. Windows starts the Host suspended, assigns it to a
kill-on-close Job Object, then resumes it. Unix starts an isolated process group and terminates descendants that
remain in that group on normal exit, protocol failure, cancellation or Drain. Deliberately detached Unix
descendants are not yet a supported containment boundary; closing that escape requires the remaining Stage 3
process-sandbox gate and must not be represented as complete process-tree isolation.

## Bidirectional command execution

The Host keeps reading stdin while `SendTurn` is active. Agentd multiplexes stdout by `commandId`, so a live
Turn can emit Runtime Events or an `InteractionRequest` while the same Host process accepts
`ResolveApproval`, `ResolveUserInput`, or `InterruptTurn`. A terminal message is correlated to the command that
created it; it does not terminate unrelated commands.

Interaction delivery is durable across the Control Plane boundary:

1. The Host emits an `InteractionRequest` correlated to the active `SendTurn`.
2. Agentd persists the normalized requested Event through Worker Protocol.
3. The authorized user resolution receives a stable resolution `commandId` and Worker/Generation target.
4. Agentd pulls the resolution, writes the command to the active Host, then marks it `delivered`.
5. After a correlated Host terminal message, agentd marks the resolution `acknowledged`.

Provider-native request identifiers are namespaced by the current Execution Generation before they cross the Host
boundary. Retries in the same Generation keep the same external `requestId`; a replacement Generation must emit a
different external ID even when native resume reproduces the same Provider request. This prevents a recovered
Approval or Structured Input from resolving the expired Interaction owned by the obsolete Worker.

Delivered-but-unacknowledged commands remain pullable and use the stable command ID for replay. A stale
Worker/Generation cannot pull, deliver, or acknowledge them. Lease recovery expires unresolved requests and
supersedes unacknowledged deliveries from the obsolete Generation.

`InterruptTurn` is also delivered as a durable Control Command. Its Result includes the current
`providerResumeCursor` when the Provider runtime has established one. Agentd forwards that Result during
acknowledgement so Control Plane can atomically persist the Cursor and terminal `execution.interrupted` state.
The Provider Host Send Turn Error that follows is confirmation of the same interrupt, not a second failure.

`SteerTurn` uses the same durable channel without terminating the active Execution. Control Plane persists the
user intent before delivery, agentd correlates it to the active `SendTurn`, and the Host returns a terminal
acknowledgement for the Steer command while the original Turn remains active. The Web SaaS projection renders
the persisted Steer intent as a marked user message and clears the composer only after Control Plane accepts it.
Queue delivery during an active remote Turn remains explicitly unsupported rather than being converted into
Steer or a new Turn.

Every durable Control Command is mapped to a Provider Capability before Claim. A Worker without an immutable
Provider Manifest is skipped for ordinary queue Claims and receives `worker_manifest_required` for an explicit
Execution Claim. A present but incompatible Manifest receives `capability_unsupported`. This check happens
before Generation or Lease mutation, so an incompatible Worker cannot consume or rebind pending control intent.

Codex uses `codex app-server` as its production v2 runtime. It initializes a bidirectional JSON-RPC connection,
starts or resumes the native Thread, streams Turn/Item/usage events, and routes native `turn/interrupt`, command
or file Approval, and Plan Mode Structured User Input through durable Worker Interaction delivery. The immutable
Turn `runtimeMode` selects approval-required versus full-access permissions, while `interactionMode=plan`
activates Codex Plan Mode. Native Cursor resume is attempted first; when it is unavailable, a new Thread is
rebuilt from bounded authoritative history instead of depending on the old Worker state. Fallback is selected
only for an explicitly invalid or expired native Session before Turn activity begins; authentication, rate-limit,
transport, and ambiguous Provider failures remain terminal instead of silently replaying a Turn.

Claude uses `@anthropic-ai/claude-agent-sdk` as its production v2 runtime. Each Send Turn owns one streaming
SDK Query, tries the native Session Cursor before authoritative-history reconstruction, and uses native
`query.interrupt()`. A host-owned `PreToolUse` hook prevents permissive local Claude settings from bypassing
Synara policy: approval-required Turns force mutating/network/command tools through the durable Interaction
channel, while full-access Turns auto-allow them. `AskUserQuestion` is projected as Structured User Input and
Plan Mode uses the native SDK permission mode. Provider output, tool lifecycle and usage are normalized without
forwarding raw SDK payloads.

Selecting authoritative-history fallback emits internal `runtime.provider.warning`, normalized to canonical
`runtime.warning`. Its fixed message states that fallback was selected before Turn activity; it does not claim the
replacement Turn succeeded. Canonical `detail` allows only `kind=session_resume`,
`attemptedStrategy=native-cursor`, `selectedStrategy=authoritative-history`,
`outcome=fallback_selected`, `reasonCode=session_resume_invalid|session_resume_expired`,
`fallbackSafety=before_turn_activity`, `authoritativeHistorySequence`, and `provider`.
Provider errors, Cursor values, credentials, and other native payload fields are never copied into this outcome.
This warning is only for a Provider-native rejection after Control Plane already selected native resume; a
Claim-time TTL/quarantine decision does not emit a Host fallback warning. Managed canonical detail is an exact
shape, `provider` is limited to `codex|claudeAgent`, and Control Plane rejects missing or additional fields.
Agentd derives the warning Event ID from Execution, Generation, Send command ID and the fixed resume-fallback
semantic slot, so replay does not append duplicate audit evidence. The warning proves only
`fallback_selected`, never that the replacement Turn succeeded.

## Describe

`Describe` returns the Host build, complete Capability Descriptor, command/message limits, Runtime Event version
range, credential delivery modes and Resume strategies. Protocol 2.1 keeps the normalized Runtime descriptor and
Release Policy inside `capabilityDescriptor`: Runtime identifies the CLI, SDK package or local build, its observed
version source and compatible range; Release Policy states whether explicit enablement is required and whether the
Host actually enabled the Provider. Managed v2.1 Hosts currently advertise Runtime Event
`{ minimum: 2, maximum: 2 }`; every Event payload carries that negotiated version and a canonical event type from
[Runtime Event v2](./runtime-event-v2.md). Static capability claims must be verified by the shared Provider
Acceptance Suite.

## agentd negotiation and v1 boundary

Managed Local, SSH, Docker and Kubernetes Workers use v2.1. Agentd appends `--protocol-v2`, performs
side-effect-free `Describe` probes before Worker registration, and publishes the returned Codex and Claude
plus explicit Local-only Provider descriptors under the registered `providerHost` capability. It performs
another Describe in the actual Host process before Start/Resume and rejects incompatible Major versions,
Protocol minors below 1, unavailable or version-incompatible Provider runtimes, Local-only or missing `send-turn`
capability, incompatible Runtime Event ranges, unsupported Credential delivery and invalid Resume strategies
before sending the Provider Turn.

The Execution Target Provider Policy is authoritative. Experimental Providers are disabled unless the Target
explicitly enables them. Agentd injects that allowlist into the Host process and Control Plane rejects registration
when the Host's observed Release Policy does not match the persisted Target policy. Runtime and Release Policy
evidence is copied into the immutable Provider Runtime Binding used by an Execution.

`SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL=v1` is the explicit, bounded compatibility switch for an older one-shot
runner. The default is `v2`; there is intentionally no `auto` mode and no fallback after a v2 command. Replaying
the same Workload through v1 after a partial v2 execution could duplicate a Provider Turn, tool call or external
side effect.

## Stable errors

Errors use the codes in `PROVIDER_HOST_ERROR_CODES` and include:

```text
retryable
requiresNewExecution
requiresUserAction
canReconstructFromHistory
canMoveWorker
```

The message is safe for users and logs. It must not contain Credential values, Worker/Lease Tokens, complete
Provider stderr or Presigned URLs.

Release-policy rejection uses `capability_unsupported`; Runtime or Protocol version rejection uses
`provider_version_incompatible`. Manifest projection must not invent a second error-code vocabulary for the same
failure classes.

## v1 boundary

The existing unversioned one-shot runner is retained only through the explicit switch above during managed
Worker upgrades. It advertises a legacy Protocol summary and cannot advertise v2-only capabilities.
