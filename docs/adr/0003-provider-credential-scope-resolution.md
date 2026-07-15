# ADR 0003: Provider Credential scope resolution

- Status: Accepted
- Date: 2026-07-15
- Scope: Stage 3 Workflow G1

## Context

Credential v1 stores encrypted Provider and Git Credentials as Tenant-owned rows. Provider Credentials
may be restricted to one Organization and may be bound explicitly to an Agent Session, but v1 does not
define User or Platform scope, automatic selection, same-level ambiguity, or model-specific Tenant
policy. Treating an unbound Session as permission to choose any Tenant Credential would silently change
existing behavior and could disclose a secret after an unrelated Credential is created.

A Platform Credential must not become a cross-Tenant global secret. Synara's Tenant remains the data,
encryption, audit, and revocation boundary even when an enterprise installation provides the Credential.

## Decision

1. Every ciphertext remains owned by exactly one Tenant. `scope = platform` is a Tenant-owned enterprise
   fallback, not a shared row and not a platform-wide KMS identity.
2. Provider Credential resolution order is frozen as:

   ```text
   explicit Session binding
   user
   organization
   tenant
   platform
   ```

3. `agent_sessions.provider_credential_id` is the authoritative persisted Session binding. An explicitly
   requested Credential wins even when its `auto_select_enabled` flag is false. If no ID is requested,
   automatic selection runs inside Session creation and persists the winner into the same field. An invalid
   requested or persisted binding fails closed and never falls through to another Credential.
4. Automatic selection is opt-in per Credential. Migration `000033` backfills every existing row with
   `auto_select_enabled = false`; new rows also default to false. Git Credentials can never participate in
   Provider automatic selection.
5. User scope names one active Tenant member. The User must own the Session (`agent_sessions.created_by`),
   and active membership is rechecked under a shared database lock both when selecting the Credential and
   when a Worker resolves plaintext. Suspending the member therefore fences an old explicit binding.
6. Organization scope requires an exact Session Organization match.
7. Tenant scope may add an exact Organization and/or Model selector. The persisted Credential `provider`
   is already the Provider selector; no duplicate selector column is introduced.
8. Platform scope requires all of the following:
   - the installation deployment profile is `enterprise`;
   - the Tenant plan entitlement is `enterprise`;
   - `provider_credential_scope_policies.platform_credentials_enabled` is explicitly true.

   Automatic Platform selection additionally requires both the Credential's `auto_select_enabled` and the
   Tenant policy's `platform_credential_auto_select`. Both policy flags default to false.

9. If more than one eligible Credential exists at the first matching automatic scope, resolution returns
   stable error `credential_scope_ambiguous`; it does not choose by creation time, name, UUID, or database
   query order.
10. Scope ownership and Tenant selectors are immutable. `auto_select_enabled` is a mutable security policy:
    authorized operators may disable it atomically without KMS access, ciphertext rotation, or Execution
    Credential-version fencing. Changes are audited without secret material.
11. New Credentials and every payload rotation use scope-aware AAD v3. Existing Provider v1 and Git v2
    ciphertext remains readable. Rotation upgrades v1/v2 to v3 only while advancing exactly one Credential
    version and replacing both ciphertext and wrapped data key.
12. Session creation and Fork run the resolver inside their idempotent transaction. Claim revalidates the
    persisted binding and freezes `provider_credential_id_snapshot` plus
    `provider_credential_version_snapshot`. Worker resolution uses the frozen ID/version plus current
    Lease/Generation and rechecks mutable authorization state.

## Rejected alternatives

- **Automatically enable existing Tenant or Organization Credentials:** rejected because an unbound Session
  would acquire a secret without an explicit operator decision.
- **Choose the newest or lexicographically first same-level Credential:** rejected because ordering is not an
  authorization rule and changes as rows are added or restored.
- **Store one global Platform Credential:** rejected because it breaks Tenant ownership, audit, KMS context,
  revocation, and incident-containment boundaries.
- **Duplicate `selector_provider`:** rejected because `provider_credentials.provider` is immutable and already
  provides the exact Provider restriction.
- **Authenticate `auto_select_enabled` in AAD:** rejected because emergency disablement must not require
  decryption/re-encryption or change the in-flight Credential version.

## Consequences

- Session, Fork, Claim, and Worker-resolution paths use `internal/credentialscope.Resolve` rather than
  implement local fallback rules.
- Metadata APIs may expose scope, selectors, and `autoSelectEnabled`, but never the internal AAD version or
  envelope fields.
- PostgreSQL enforces ownership, selector, Platform entitlement, immutable identity, and safe envelope
  rotation. SQLite mirrors the critical checks and indexes used by Personal development and tests.
- Disabling automatic selection affects future selection immediately. Existing Execution generations remain
  fenced to their captured Credential ID/version; suspension, expiry, revocation, and Platform policy are
  still rechecked before plaintext release.
