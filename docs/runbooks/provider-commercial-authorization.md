# Hosted Provider commercial authorization

Status: product enforcement implemented; external Legal/Privacy authority remains required

## Boundary

Platform Admin → Provider use is the authority for whether an experimental Provider may receive a new Enterprise remote
Execution claim. The record binds one exact Provider product, account type, contracting entity, credential mode/scope,
Region set, data-use policy, retention statement, terms, agreement, DPA, prohibited-use statement, termination runbook and
review expiry.

For the terms, executed agreement/Order Form, DPA and termination Runbook, record both the credential-free HTTPS reference
and `sha256:<64 lowercase hex>` of the exact reviewed bytes. Compute the digest from a private stable file, not from a
browser-rendered page or mutable redirect. Never upload contract contents, signatures or credentials to Platform Admin.

An active record proves only that Synara recorded the required metadata and separated decisions. It does not prove that a
contract is genuine, current, signed by the correct entity or interpreted correctly by counsel. Keep the executed agreement
and review artifacts in the controlled evidence repository.

## Create the authorization

Only an active Platform Operator Tenant Owner or Admin may create or advance a record. Current hosted authorization is
limited to the `codex`/OpenAI and `claudeAgent`/Anthropic experimental adapters. A `local-only` Provider cannot be converted
to hosted support through this workflow; its catalog tier and adapter must first pass a separate product/security release.

References must be credential-free HTTPS URLs without query strings or fragments. Review expiry must be in the future and
no more than 180 days after creation. Scope, Provider, contract and Region identity are immutable; a terms or use-case change
requires a new authorization.

Missing, malformed or all-zero document digests reject creation. URL-only records created before Migration `000136` are
retained as `revoked` history during upgrade; recreate them with exact byte bindings before hosted use.

## Review and activate

Move the record from `draft` to `ready_for_review`. Four different active operators then record append-only decisions for:

- Legal;
- Privacy;
- Security;
- Product.

The creator cannot approve and one operator cannot occupy two roles. Any rejection atomically terminates the record. An
Owner/Admin can activate only after all four approvals and only while the review remains unexpired. Every operation is
written to Platform Audit.

For each decision, enter a credential-free HTTPS evidence reference and the non-zero `sha256:<64 lowercase hex>` of the
exact approval artifact reviewed for that role. Missing or malformed hashes are rejected. Migration `000137` retains a
legacy URL-only decision for history but revokes its parent authorization; recreate the authorization and all four
byte-bound decisions before hosted use.

Each decision also requires the matching active `provider_commercial.*` assignment from Platform Admin → Governance roles.
Follow [`stage-6-governance-authority.md`](stage-6-governance-authority.md); selecting a role in the approval form does not
grant that role.

## Runtime enforcement

Enterprise deployments enable the commercial gate in the Control Plane process. Immediately before a remote Worker receives
a lease or Provider Credential Grant, the claim transaction rechecks:

1. an active, unexpired authorization with four approved, byte-bound role decisions exists for the exact Provider;
2. the Execution placement Region, or fixed-Target Tenant home Region fallback, is allowed;
3. the exact snapshotted Provider Credential is active and belongs to the Execution Tenant;
4. its scope is allowed;
5. a platform-scoped credential uses a `platform_managed` authorization and all other scopes use `customer_byok`.

Missing, mismatched, expired or revoked authorization returns `provider_commercial_authorization_required` before a Worker
lease or credential grant is created. Revocation therefore blocks new claims immediately. Existing in-flight leases must be
handled through the Provider suspension/credential revocation incident procedure; authorization revocation is not a hidden
remote kill command.

Personal/local deployments do not enable this hosted gate. `local-only` catalog enforcement remains independent and still
blocks those Providers on remote Targets.

## Renewal and termination

Begin review before expiry. Terms, DPA, Region, retention, training policy, product, account model or contracting-entity
changes require a new immutable record and four new decisions. Revoke the old record after the replacement is active.
The same applies when repository bytes no longer match any recorded document digest, even if the URL is unchanged.

For termination or emergency suspension, revoke the authorization, revoke affected Provider Credentials, prevent new
Target enablement, assess active leases, notify affected Tenants, and retain Audit/request IDs and Provider confirmation in
the compliance evidence repository.
