# Provider Commercial Use and Privacy Boundary v1

Status: engineering baseline; Legal/Privacy approval required before hosted GA
Owner: Product Security + Legal
Last reviewed: 2026-07-30

This contract defines which Provider credentials Synara may accept in a multi-user hosted Tenant. It is an engineering
control baseline, not legal advice and not a substitute for an executed Provider agreement, DPA, order form, or counsel
approval. Provider terms change independently of Synara; Legal must review the linked current terms and record an approval
artifact before a Provider can move to `tier-1` or `tier-2` hosted support.

## 1. Non-negotiable boundary

- Synara never shares a consumer login, browser session, cookie, OAuth refresh token, or individual seat across users.
- A customer may supply only a credential they are authorized to use for the selected scope. User BYOK remains user scoped;
  Organization/Tenant credentials require an enterprise/API agreement that permits an application to serve authorized end
  users.
- Synara does not buy, sell, transfer, sublicense, rent, or expose Provider API keys. Secrets stay envelope encrypted and are
  resolved only for an authorized Execution; UI, Audit, logs, exports and Support Access expose metadata only.
- Hosted use requires business/API terms. Consumer/free-product credentials are local-only even when a runtime adapter can
  technically authenticate them.
- Customer Input may be sent to the selected Provider. The Tenant administrator is responsible for a lawful basis and user
  notice; Synara must disclose the Provider/subprocessor, applicable region/retention behavior and feature-specific data use.
- Training/data-sharing opt-ins are prohibited for platform-managed credentials. Customer BYOK may use them only after an
  explicit Tenant policy and Privacy approval; the default is no training/data sharing.
- Provider safety, sanctions, supported-region and prohibited-use policies remain enforceable. Synara must not bypass rate
  limits, safety controls or human-confirmation requirements.

## 2. Internal self-hosted remote-Target enablement matrix

| Catalog Provider               | Current support tier | Organization-owned credential allowed on remote Targets                                                                                                                                            | GA decision                                                                                                              |
| ------------------------------ | -------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------ |
| `codex`                        | `experimental`       | Deploying organization's OpenAI API/Business/Enterprise credential whose agreement permits the application and its internal users. Never a shared ChatGPT consumer login.                          | Explicit Target enablement plus Legal approval; not GA by default.                                                       |
| `claudeAgent`                  | `experimental`       | Deploying organization's Anthropic API/commercial credential or separately approved enterprise agreement. Never shared Claude.ai/Pro consumer authentication.                                      | Explicit Target enablement plus Legal approval; not GA by default.                                                       |
| `cursor`                       | `local-only`         | None on shared or managed remote Targets.                                                                                                                                                          | Remains local-only until Anysphere gives written approval for the exact automated internal deployment and account model. |
| `antigravity` (`gemini` alias) | `local-only`         | None on shared or managed remote Targets. If later enabled, use only an approved paid Gemini API/Vertex business project; unpaid AI Studio/Gemini quota is prohibited for internal Tenant content. | Remains local-only pending adapter productization, regional review, DPA and Legal approval.                              |
| `grok`                         | `local-only`         | None on shared or managed remote Targets. If later enabled, use an approved xAI enterprise/API credential, never Grok consumer credentials.                                                        | Remains local-only pending adapter productization, enterprise terms/DPA and Legal approval.                              |
| `kilo`, `opencode`, `pi`       | `local-only`         | None on shared or managed remote Targets. Their downstream model Provider must be reviewed independently.                                                                                          | Remains local-only; an aggregator/CLI approval never replaces the underlying model Provider review.                      |

The catalog support tier and Worker release policy are enforcement controls, not evidence of commercial permission.
`local-only` Providers must remain unavailable on remote Targets. `experimental` Providers require explicit Target
enablement and a compatible Worker manifest, but that technical gate does not waive the Legal approval gate.

## 3. Official term baselines reviewed

- OpenAI Services Agreement and Service Terms: <https://openai.com/policies/services-agreement/> and
  <https://openai.com/policies/service-terms/>. The business agreement permits API integration for customer applications
  and end users, while prohibiting shared individual logins and buying, selling or transferring API keys.
- Anthropic Commercial Terms and Consumer Terms: <https://www.anthropic.com/legal/commercial-terms> and
  <https://www.anthropic.com/legal/consumer-terms>. Commercial terms cover API-powered customer products and state that
  commercial Customer Content is not used to train models; consumer credentials may not be shared and automated access is
  not the hosted integration path.
- Cursor Terms of Service: <https://cursor.com/en-US/terms-of-service>. The public terms restrict renting, leasing, lending
  or selling the Service; hosted multi-user automation therefore remains unapproved without a specific enterprise grant.
- Gemini API Additional Terms: <https://ai.google.dev/gemini-api/terms>. Paid and unpaid data-use rules differ materially;
  unpaid service content may be used for product/model improvement and must not receive sensitive customer content.
- xAI Enterprise Terms and DPA: <https://x.ai/legal/terms-of-service-enterprise> and
  <https://x.ai/legal/data-processing-addendum>. Enterprise API terms contemplate approved bundled services/end users;
  consumer Grok account credentials remain outside the hosted boundary.

Legal must capture the effective terms/version, contracting entity, approved products/features, regions, data retention,
training setting, DPA/SCC/BAA requirements, subprocessor notice, end-user rights, prohibited use, termination/export path and
renewal owner. A link alone is not approval evidence: the authorization must bind `sha256:<64 lowercase hex>` for the exact
terms, executed agreement/Order Form, DPA and termination Runbook bytes reviewed by the four approvers. Each Legal,
Privacy, Security and Product decision must separately bind its evidence reference to the exact reviewed bytes with the
same digest format; the primary document hashes do not substitute for decision-specific evidence.

## 4. Release evidence

Before hosted enablement, the release record must contain:

1. Legal approval ID and reviewer; Privacy/DPA approval ID where customer content or personal data is processed.
2. Provider product, account type, contracting entity, credential scope and allowed Tenant/Organization population.
3. Data-use/training setting, retention/ZDR eligibility, permitted regions and feature exclusions.
4. A negative test proving consumer/session credentials cannot be configured or resolved for remote Targets.
5. A secret-leak test covering logs, Audit, Support Access, Tenant export and incident bundles.
6. Revocation, offboarding and Provider-suspension runbooks with an accountable owner.
7. Review expiry no later than 180 days or earlier when Provider terms change.

Until every artifact exists, UI and sales material must label the Provider experimental/local-only and must not promise
hosted enterprise support.

## 5. Product enforcement

Migration `000113`, `internal/providercommercial` and Platform Admin → Provider use implement the metadata and runtime gate
for this contract. The immutable record requires Legal, Privacy, Security and Product decisions by four distinct operators;
the creator cannot approve, rejected decisions terminate the record, and review expiry is capped at 180 days.

Enterprise remote Worker claim rechecks the active record against the exact Provider, placement/home Region, snapshotted
Provider Credential mode and scope before creating the Worker lease or credential grant. Expiry or revocation blocks new
claims immediately. Personal/local execution remains outside the hosted gate, while catalog `local-only` enforcement remains
independent. Operations follow `docs/runbooks/provider-commercial-authorization.md`.

Migration `000114` additionally requires each decider to hold the exact active `provider_commercial.*` functional
assignment managed by a distinct Operator Tenant Owner. The request-body role is not authority; expiry, revocation or
offboarding rejects new decisions at both service and database boundaries. Operations follow
`docs/runbooks/stage-6-governance-authority.md`.

Migration `000140` requires each active functional-authority grant to bind the exact corporate-delegation evidence bytes
with a non-zero SHA-256. Legacy URL-only grants are atomically revoked during upgrade and cannot authorize a new Provider
decision, even when the user still holds an eligible Operator Tenant role.

Migration `000136` adds immutable SHA-256 bindings for the terms, agreement/Order Form, DPA and termination Runbook.
Pre-migration URL-only records are retained for history but atomically revoked during upgrade; they cannot authorize a new
hosted claim. New records reject missing, malformed and all-zero digests at service, SQLite and PostgreSQL boundaries. A
reference or digest change requires a new authorization and four new decisions.

Migration `000137` adds the same immutable SHA-256 binding to every individual approval evidence reference. Authorizations
with a legacy URL-only approval are retained but atomically revoked during upgrade. Activation and the runtime hosted-claim
gate require exactly four approved, byte-bound decisions; approval evidence cannot be edited after insertion.

Migration `000138` closes the release-to-runtime authority gap. A Stage 6 Release candidate with `provider_commercial`
impact must bind one or more exact active authorization IDs and versions while still in `draft`. The binding is immutable;
missing bindings, expiry, revocation or version drift block review, approval, deployment, observation and release in the
service plus SQLite/PostgreSQL. Release Privacy/Legal approval therefore cannot substitute for the exact four-role Provider
authorization, and the Provider authorization cannot substitute for Release approval.

The product record does not validate external signatures, contracting authority or legal interpretation. Those remain
required external evidence.
