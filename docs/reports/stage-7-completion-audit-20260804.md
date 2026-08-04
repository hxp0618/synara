# Stage 7 Completion Audit — 2026-08-04

This audit applies the milestone exits and decisions in
[`external-sdk-developer-platform.md`](../plans/external-sdk-developer-platform.md) to current repository and
runtime evidence. “Achieved” means direct evidence covers the stated scope; source/fixture evidence is not used
to satisfy an external deployment, Registry, real Provider or GA requirement.

## Verdict

| Scope | Verdict | Direct evidence |
| --- | --- | --- |
| M1 API productization | **Achieved for source beta and controlled runtime** | Classified mux/OpenAPI guards, 41 codegen-ready operations, Service Account/rate-limit/usage tests, SDK HTTP/SQLite conformance, and the 11.143-second curl acceptance |
| M2 TypeScript SDK/docs/examples | **Achieved for source beta** | 17 SDK tests, generated-type drift gate, three TypeScript examples, 12-page Astro/Redocly build, release artifact verifier |
| M3 Python/Webhook/Console/BYO Target | **Achieved for source beta and controlled runtime** | 11 Python tests plus cross-process conformance, real localhost Webhook HTTP delivery/retry/dead-letter tests, Console component tests, real SSH durable provisioning acceptance |
| External public beta activation | **Not achieved** | Remote-host Docs rendering passed but public ingress was blocked; no deployed origin, external HTTPS Webhook egress receipt, npm/PyPI ownership, Trusted Publisher registration or actual publication |
| M4 public GA | **Not achieved** | Depends on unfinished Stage 6 production/authority gates, real Provider release gates, production DNS/cert/rotation evidence and human approvals |

## Requirement-by-requirement evidence

### M1

- **Achieved:** every registered route is classified and the OpenAPI route inventory is bidirectionally checked by
  `internal/httpapi/api_routes_test.go` and `openapi_contract_test.go`.
- **Achieved:** all 41 public-beta operations are `codegen-ready`; the generated public document has no
  `route-only` operation and excludes the internal route inventory.
- **Achieved:** Service Account bearer, fixed RBAC, tenant mismatch, private-resource denial, immediate revocation,
  rate-limit headers, usage attribution, machine Audit/Event/Idempotency attribution and pagination are covered by
  focused Go tests and real HTTP/SQLite conformance.
- **Achieved:** [`stage-7-curl-quickstart-acceptance-20260804.md`](stage-7-curl-quickstart-acceptance-20260804.md)
  directly proves API Key → Session → Turn → SSE → Approval → terminal Event in 11143 ms.

### M2

- **Achieved:** `@polaris-agents/sdk` combines OpenAPI-generated types with the hand-written Session/Execution/
  Interaction/Artifact domain layer; retry, idempotency, typed errors, SSE reconnect, polling fallback, replay
  suppression, sequence-gap denial and execution filtering are tested.
- **Achieved:** CI-fix, PR-review and batch-migration examples typecheck against the workspace SDK.
- **Achieved:** Developer Docs and the filtered API Reference build from the same OpenAPI source.
- **Partially evidenced externally:** the exact static artifact rendered on an authorized remote host, but the public
  high port was blocked and no shared firewall/ingress was changed; see
  [`stage-7-developer-docs-remote-host-acceptance-20260804.md`](stage-7-developer-docs-remote-host-acceptance-20260804.md).
- **Achieved:** npm staging removes workspace-only fields and the exact tarball allowlist is verified before attestation.
- **Not achieved externally:** `@polaris-agents` organization/package ownership and npm OIDC Trusted Publisher are
  not registered or independently verified; no npm package has been published.

### M3

- **Achieved:** `polaris-agents` Python 3.11+ package shares the generated operation inventory and real Control
  Plane conformance behavior with TypeScript.
- **Achieved locally:** wheel/sdist build and archive verifier pass. `twine check` is wired into protected CI, but the
  earlier local attempt could not download `nh3`; only the protected release environment can satisfy that exact row.
- **Achieved:** Console API Key management exposes create/scope/rate-limit/rotate/revoke/last-used/usage without
  retaining plaintext after the one-time display.
- **Achieved locally:** Webhook projection sends a thin signed payload to a real local HTTP receiver, retries
  independently, dead-letters and omits response bodies from persistence while SSRF validation fails closed.
- **Achieved in controlled runtime:** Service Account creates a secret-free Target projection and durable SSH
  provisioning reaches exact Worker readiness, replacement and restart continuity.
- **Not achieved externally:** no approved external HTTPS receiver/network-policy exercise and no PyPI ownership,
  Trusted Publisher or publication evidence exist.

### M4 and GA

- **Achieved in source:** additive `/v1`, `/v2` breaking-change direction, public-beta lifecycle, 12-month
  deprecation policy and SDK minimum Control Plane version metadata are documented.
- **Not proven for GA:** deployed self-serve onboarding/free-tier policy, Stage 6 production SLO/recovery/incident/
  compliance gates, real Codex/Claude release acceptance, external availability monitoring, production rotation and
  Engineering/Security/Product/Release approvals are missing or explicitly incomplete in the Stage 6 checklist.

## Authoritative release gate

Every candidate must use
[`stage-7-developer-platform.md`](../release-checklists/stage-7-developer-platform.md). The checklist deliberately
keeps external-authority rows unchecked; a green source workflow cannot convert those rows into pass. The SDK publish
workflow now fails closed when `publish=true` but the exact tag or either Trusted Publisher readiness variable is absent.
