# Polaris Developer API contract

`openapi.yaml` is the OpenAPI 3.1 source of truth for the external Polaris API.

The top-level `x-synara-route-surfaces` inventory classifies every control-plane route as
`internal`, `public-beta`, or `public-ga`. Every non-internal route must also have exactly one
standard OpenAPI operation under `paths` with the same exposure. The control-plane conformance
tests compare this file with the actual route registrations in both directions.

Operations carrying `x-synara-contract-status: route-only` are not eligible for SDK generation or
external publication. That marker means route identity, operation ID, authentication scheme, and
error-envelope linkage are frozen, while request parameters and success-response schemas still
need to be completed against the live handler contract. Remove the marker only when those schemas
and their spec-driven runtime conformance cases are complete.

Operations carrying `x-synara-contract-status: codegen-ready` have explicit parameter, request,
success-response, stable-error, and response-header schemas. The first vertical slice is
`createSession` → `createTurn` → `listSessionEvents` / `streamSessionEvents` →
`resolveExecutionApproval`; generation
must select only codegen-ready operations until the remaining public-beta surface is frozen.
The developer documentation build runs `scripts/generate-stage7-public-openapi.ts` and publishes a
filtered API Reference containing only those operations. The full source remains the route-exposure
inventory used by Control Plane conformance tests; it must never be served directly as public reference.

Never add an internal Worker, platform-governance, dev-login, SCIM, or Artifact content-channel
route to `paths`. Artifact content remains authorized through short-lived grant tokens rather than
the Service Account bearer scheme.

Run the focused contract gate from `services/control-plane`:

```sh
go test ./internal/httpapi -run 'TestOpenAPI|TestEveryPublicRoute|TestEveryServerRouteDeclaresItsAPISurface'
```

Run the OpenAPI 3.1 structural and reference validator from the repository root:

```sh
bun run stage7:api:lint
```
