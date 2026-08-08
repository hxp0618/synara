# `@polaris-agents/sdk`

TypeScript SDK for the Polaris Developer API. The source workspace remains intentionally private.
The release workflow creates a bounded public manifest and verified tarball only after the Stage 7
contract and conformance gates pass; npm ownership, trusted-publisher registration, and release
approval remain external gates.

The generated OpenAPI types live in `src/generated/openapi.ts`; do not edit that file manually. Run
`bun run generate` from this package after changing a `codegen-ready` operation in
`docs/api/openapi.yaml`. The public entrypoint exposes only operations whose contract status is
`codegen-ready`.

The complete forty-one-operation public-beta surface includes bounded Project and Session reads with Usage, sanitized Provider capability, Execution-cancel, Turn interrupt/steer, checkpoint-resume and queued primary-operation projections, idempotent Project update/archive, Session model/history/lifecycle operations, response-loss-safe Artifact upload/download/delete, safe Interaction pages and user-input resolution, plus durable SSH Execution Target provisioning. Use
`targets.provision(...)` with one stable idempotency key and `targets.waitForProvisioning(...)`;
registration alone is not compute readiness.

## Beta quickstart

```ts
import { Polaris } from "@polaris-agents/sdk";

const polaris = new Polaris({
  apiKey: process.env.POLARIS_API_KEY!,
  baseUrl: process.env.POLARIS_BASE_URL!,
});

const project = await polaris.projects.create("tenant-id", "organization-id", {
  name: "CI automation",
});

const session = await polaris.sessions.create({
  projectId: project.data.id,
  title: "Fix CI",
  provider: "codex",
});

await session.sendTurn({
  inputText: "Find and fix the failing tests.",
  runtimeMode: "full-access",
  interactionMode: "default",
});

for await (const event of session.events()) {
  console.log(event.sequence, event.eventType, event.payload);
}
```

`baseUrl` remains explicit for the beta because Synara supports tenant-controlled and self-hosted control-plane
origins. The SDK is server-side only: it sends a Service Account bearer credential and does not enable browser
CORS. Mutations generate an `Idempotency-Key`, retry retryable network/5xx/429 failures, and expose replay and
rate-limit metadata. Event streaming uses the durable sequence cursor, releases its HTTP connection when the
consumer exits early, falls back to bounded event polling when an SSE connection pool is full, and fails closed
on a sequence gap in either transport.

The developer guides live in `apps/developer-docs`; complete CI repair, pull-request review, and batch
migration integrations live in `examples/polaris/typescript`. Run `bun run stage7:developer:check` from the
repository root to verify the OpenAPI contract, generated SDK types, package build, examples, documentation
content, and generated API Reference together.

Maintainers can run `bun run stage7:sdk-release:check` to verify release staging and archive-safety
guards. `.github/workflows/polaris-sdk-release.yml` defaults to build-only; publishing additionally
requires an exact `polaris-sdk-v<version>` tag, registry-ready repository variables, protected
environments, and npm/PyPI OIDC trusted publishers. No registry token belongs in the repository.
