# Runtime Event v2

Runtime Event v2 is the canonical Provider Runtime vocabulary carried from Provider Host through `synara-agentd`
to the Control Plane Session Event log. It reuses `packages/contracts/src/providerRuntime.ts`; it does not create a
second remote-only event namespace.

The machine-readable persisted Session Event envelope is
[runtime-event-v2.schema.json](./runtime-event-v2.schema.json).

## Version boundary

- Runtime Event v1 remains a read/write compatibility boundary only for the explicit Provider Host v1 runner.
- Managed Provider Host Protocol v2 advertises and emits Runtime Event v2 only.
- Agentd negotiates version 2 through `Describe`, rejects Event frames outside that range, and forwards the exact
  negotiated version to Control Plane.
- Control Plane never reinterprets a v1 payload as v2. Existing v1 rows remain replayable.

Provider Host Protocol, Worker Protocol, and Runtime Event versions are independent. A Worker Protocol v2 client
does not make a Provider Host or Runtime Event version compatible by implication.

## Provider Host Event frame

Provider Host Protocol v2 carries:

```json
{
  "messageType": "Event",
  "payload": {
    "eventVersion": 2,
    "eventType": "content.delta",
    "payload": {
      "streamKind": "assistant_text",
      "delta": "hello"
    }
  }
}
```

Agentd supplies the Worker-authenticated execution, lease, generation, event ID, and occurrence time in the Worker
upload envelope. Control Plane assigns the authoritative Session Sequence. Provider-native sequence numbers are
never used as Session Sequence.

The SaaS Session Event envelope predates the richer local Provider Runtime base fields. Stable correlation values
such as `requestId` therefore remain a documented payload transport extension for Interaction events until that
outer envelope evolves; their meaning and validation still follow the canonical Runtime Event contract.

## Canonical mapping

The bounded v1 runner keeps its historical types. Provider Host Protocol v2 maps its internal runner messages as
follows:

| Internal/legacy source | Runtime Event v2 |
| --- | --- |
| `runtime.output.delta` | `content.delta` with `streamKind=assistant_text` |
| `runtime.provider.activity` | `item.started`, `item.updated`, or `item.completed` |
| `runtime.usage` | `thread.token-usage.updated` |
| `runtime.provider.warning` | `runtime.warning` |
| Approval `InteractionRequest` | `request.opened` |
| Structured input `InteractionRequest` | `user-input.requested` |
| Approval resolution | `request.resolved` |
| Structured input resolution | `user-input.resolved` |

Provider-specific tool names are retained only as bounded title/data references. The lifecycle `itemType` uses the
canonical values from `ProviderRuntimeEventV2`. Raw Provider payloads, credentials, token values, complete stderr,
and presigned URLs do not cross this boundary.

## Validation and unknown events

- Serialized payload is limited to 65,536 bytes. Larger logs, Diffs, files, and snapshots use Artifact references.
- Control Plane validates the v2 event type and the canonical payload shape before persistence.
- Unknown Provider-native messages are degraded by Provider Host to a bounded `runtime.warning`; their raw payload
  is not persisted.
- An unknown v2 wire event type is rejected as a protocol violation because the Host declared support for the same
  frozen version. A future vocabulary must advertise a compatible negotiated version instead of silently changing
  v2 semantics.
- Unknown optional fields on a known v2 payload are preserved and ignored by older projections.
- Web projects both legacy `runtime.output.delta` and canonical `content.delta`, but a v2 Host emits only the latter.
  Session Sequence deduplication prevents retry or SSE reconnect from appending the same Delta twice.

## Interaction compatibility

Migration `000028_interaction_runtime_event_version.sql` records the request event version on each durable
Interaction. Old rows default to v1. New Provider Host v2 requests persist v2, and their resolved event inherits
that version so the Control Plane never guesses from an ambiguous event name.
