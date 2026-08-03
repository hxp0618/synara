# Credential envelope-KMS rotation v1

## Scope and invariants

This contract covers every Control Plane envelope that uses `SYNARA_CREDENTIAL_KMS_*`:

- `provider_credentials` (Provider, Git, registry and package credentials);
- `identity_connections` (OIDC client secrets and SAML signing material); and
- `identity_login_attempts` (short-lived OIDC/SAML state).

The KMS key is a key-encryption key (KEK). Rotation unwraps and rewraps only the random data-encryption key. It must not
decrypt and re-encrypt the business payload, advance a Provider Credential version, change AAD, revoke an Execution grant,
or alter resource timestamps. New writes always use the configured primary key. Old keys are decrypt-only.

Every key identity is the exact `(provider, keyId)` stored in the envelope. AWS deployments must use an immutable key ARN
or otherwise immutable ID; moving an alias while retaining the same stored ID is not a supported rotation.

## Runtime keyring

The primary is configured by `SYNARA_CREDENTIAL_KMS_PROVIDER`, `SYNARA_CREDENTIAL_KMS_KEY_ID`, and either the local key or
AWS Region. Up to eight decrypt-only keys are declared by `SYNARA_CREDENTIAL_KMS_DECRYPT_KEYS_JSON`:

```json
[
  {
    "provider": "local",
    "keyId": "local-v1",
    "localKeyEnvironment": "SYNARA_CREDENTIAL_MASTER_KEY_OLD_V1"
  },
  {
    "provider": "aws-kms",
    "keyId": "arn:aws:kms:eu-west-1:123456789012:key/old-key-uuid",
    "region": "eu-west-1"
  }
]
```

Local key bytes are read only from the named environment variable; key bytes in JSON are rejected by the exact JSON
schema. Duplicate identities, a duplicate primary, reused local key bytes, missing key material, and unknown providers
fail startup. `local` and `aws-kms` are supported; Vault may deliver a local key but Vault Transit is not a KMS provider.

## Zero-downtime state machine

1. **Bridge:** deploy the old key as primary and the future new key as decrypt-only to every replica. Prove every replica
   can read both canary envelopes. Old replicas can now read envelopes produced later by new replicas.
2. **Switch writer:** roll every replica to the new primary plus the old decrypt-only key. New writes use only the new key;
   mixed-version replicas can read both. Stop if any replica lacks either identity.
3. **Inventory:** run `control-plane-metadata rewrap-kms` without `--execute`. Any `unconfigured > 0` is a hard stop.
4. **Rewrap:** execute with a non-secret change/incident reference. Resume an interrupted run with its UUID. The executor
   authenticates the payload under its existing AAD, inserts exact authorization evidence, compare-and-swaps the envelope,
   and writes a Tenant audit event in one transaction per resource.
5. **Converge:** a completion receipt is issued only when inventory finds zero non-primary envelopes. Repeat dry-run from a
   separate current image and retain the receipt/digest.
6. **Retire:** remove the old decrypt key from every replica, prove representative reads and SSO login, then schedule old
   key disablement and destruction under the deployment's retention policy.

Skipping the bridge step is unsafe during a rolling deployment: an old replica cannot read a new replica's writes.

## Database authority and evidence

`kms_rewrap_runs`, `kms_rewrap_entries`, and `kms_rewrap_receipts` are immutable. A missing receipt identifies an
interrupted run. `--resume-run-id` continues that run only when its primary identity and operator reference still match.

Database triggers reject a same-version Provider Credential envelope change unless an exact entry already exists for the
Tenant, resource, old/new KMS identities, and old/new wrapped data keys. Identity envelopes apply the same rule when their
business ciphertext is unchanged; normal SSO secret rotation with new business ciphertext remains valid. Entry creation,
compare-and-swap update, and `security.credential_kms_rewrapped` audit insertion share one transaction.

The receipt records per-resource counts and a SHA-256 digest over ordered immutable entries. Neither entries, audits, CLI
JSON nor the digest contain plaintext data keys or decrypted payloads. A dry-run writes no run, entry, receipt, or audit.

## Failure behavior

- An envelope whose exact key identity is not in the keyring fails closed before a run is created.
- Authentication failure stops the run and preserves the row. It must be investigated as wrong key/AAD or corruption.
- A concurrent business rotation wins its compare-and-swap; the rewrap retries from current authoritative state.
- Process/database interruption preserves completed entry transactions. Resume the recorded run; do not edit evidence.
- KMS unavailability never falls back to plaintext, local files, another arbitrary key, or a database rewrite.

Source compatibility and local/PostgreSQL tests are implementation evidence only. A production rotation is complete only
with the deployment-specific approvals and evidence required by the Stage 6 release checklist.
