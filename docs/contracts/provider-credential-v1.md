# Provider Credential v1 contract

## Scope

Provider Credentials are Tenant-owned encrypted resources. A Credential may be available to every
Organization in its Tenant or restricted to one Organization. Browser APIs expose metadata only;
plaintext is available exclusively to a Worker that owns the current Execution Lease and Generation.

## Envelope format

Each create or rotation generates an independent random 32-byte data key. The payload is serialized
as a non-empty JSON object no larger than 64 KiB and encrypted with AES-256-GCM. The data key is then
wrapped by the configured KMS provider.

The authenticated additional data is the NUL-delimited sequence:

```text
synara-credential-v1
tenant_id
credential_id
provider
credential_type
version
```

Changing resource ownership, provider/type metadata, version, encrypted payload, or wrapped data key
causes authenticated decryption to fail. Stored rows contain only ciphertext, the wrapped data key,
KMS provider/key metadata, version, expiry, and revocation metadata.

## KMS providers

- `local`: a base64-encoded 32-byte KEK supplied through `SYNARA_CREDENTIAL_MASTER_KEY`; intended for
  Personal and controlled single-node deployments.
- `aws-kms`: an AWS KMS key ID or ARN. The wrapper passes a SHA-256 digest of the resource AAD as the
  KMS encryption context and relies on the standard AWS credential chain.

The KMS provider and key ID stored in an envelope must match the active wrapper. Key replacement is
not implicit: operators must rotate/re-encrypt Credentials before retiring a KEK or KMS key.

## User API

Owner and Security Admin roles may list, create, rotate, and revoke Provider Credentials:

```text
GET  /v1/tenants/{tenantID}/credentials
POST /v1/tenants/{tenantID}/credentials
POST /v1/tenants/{tenantID}/credentials/{credentialID}/rotate
POST /v1/tenants/{tenantID}/credentials/{credentialID}/revoke
```

Create and rotate requests accept the plaintext `payload`. Responses never include `payload`,
`encryptedPayload`, or `encryptedDataKey`. Rotation requires `expectedVersion` and increments the
version atomically. Revoke is idempotent.

## Worker retrieval

```text
POST /v1/workers/executions/{executionID}/credentials/{credentialID}/resolve
Authorization: Bearer <worker-token>

{
  "tenantId": "...",
  "generation": 2,
  "leaseToken": "..."
}
```

The control plane validates Worker identity, Tenant, current Lease Token, unexpired Lease,
Generation, Execution ownership, Session Organization, Credential expiry, and revocation. A
Credential scoped to another Organization is returned as not found. Successful responses set
`Cache-Control: no-store` and contain `{ "payload": { ... } }`.

## Non-disclosure rules

- Plaintext must not be written to logs, Audit metadata, Session Events, Artifact metadata, object
  storage metadata, command-line arguments, or database columns.
- Browser query caches contain Credential metadata only. Secret form state is cleared after a
  successful create or rotation.
- Worker/Lease tokens are never passed to provider runner processes. Agentd/provider adapters may
  resolve a configured Credential and deliver it to the provider process through an in-memory pipe;
  they must not persist or echo it.
- Audit records cover create, rotate, and revoke operations using metadata only. Plaintext resolution
  is deliberately not evented or logged.
