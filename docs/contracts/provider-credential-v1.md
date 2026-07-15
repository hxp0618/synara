# Credential v1 contract

> Scope resolution, opt-in automatic selection, Platform policy, and scope-aware AAD v3 are defined by
> `provider-credential-v2.md`. This document remains authoritative for legacy v1/v2 ciphertext and Git
> delivery compatibility.

## Scope

Provider and Git Credentials are Tenant-owned encrypted resources in one Vault. The persisted
`purpose` is either `provider` or `git`; the two purposes have separate binding and Worker-resolution
paths. A Credential may be available to every Organization in its Tenant or restricted to one
Organization. Browser APIs expose metadata only; plaintext is available exclusively to a Worker that
owns the current Execution Lease and Generation.

Provider Credentials bind to Agent Sessions through `provider_credential_id`. Git Credentials bind to
Projects through `git_credential_id`; the two foreign keys and database triggers reject cross-purpose
binding.

## Envelope format

Each create or rotation generates an independent random 32-byte data key. The payload is serialized
as a non-empty JSON object no larger than 64 KiB and encrypted with AES-256-GCM. The data key is then
wrapped by the configured KMS provider.

Existing Provider ciphertext retains the NUL-delimited v1 authenticated additional data:

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

Git Credentials use purpose-aware v2 authenticated additional data:

```text
synara-credential-v2
tenant_id
credential_id
git
git
https_token
version
```

This preserves decryption of legacy Provider rows while preventing a Git ciphertext from being
reinterpreted under another purpose. Credential Tenant, Organization, purpose, Provider, and type are
immutable after creation.

## KMS providers

- `local`: a base64-encoded 32-byte KEK supplied through `SYNARA_CREDENTIAL_MASTER_KEY`; intended for
  Personal and controlled single-node deployments.
- `aws-kms`: an AWS KMS key ID or ARN. The wrapper passes a SHA-256 digest of the resource AAD as the
  KMS encryption context and relies on the standard AWS credential chain.

The KMS provider and key ID stored in an envelope must match the active wrapper. Key replacement is
not implicit: operators must rotate/re-encrypt Credentials before retiring a KEK or KMS key.

## User API

Owner and Security Admin roles may list, create, rotate, and revoke Credentials:

```text
GET  /v1/tenants/{tenantID}/credentials
POST /v1/tenants/{tenantID}/credentials
POST /v1/tenants/{tenantID}/credentials/{credentialID}/rotate
POST /v1/tenants/{tenantID}/credentials/{credentialID}/revoke
```

Create requests include `purpose`; omitted legacy values normalize to `provider`. Provider payloads
remain Provider-specific JSON. Git v1 supports only this strict shape:

```json
{
  "purpose": "git",
  "provider": "git",
  "credentialType": "https_token",
  "payload": {
    "host": "github.com",
    "username": "x-access-token",
    "token": "..."
  }
}
```

Extra Git payload fields, control characters, an invalid hostname, or another Provider/type are
rejected. Rotation cannot change the Git hostname.

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

Git resolution uses a separate endpoint:

```text
POST /v1/workers/executions/{executionID}/git-credentials/{credentialID}/resolve
```

In addition to the Lease and Generation checks above, the Control Plane requires the current Project
binding, Git purpose/type, Organization scope, availability, an HTTPS repository on port 443, and an
exact Credential-host/Repository-host match. The normal Provider resolver never returns Git payloads.

Agentd resolves the Git Credential only while materializing a Workspace. Username and token stay in an
in-memory Unix-socket AskPass server; the Git child receives only the socket path and helper executable.
The socket directory is mode `0700`, the socket is `0600`, and both are removed immediately after Clone
or Fetch. Local branch, status, merge-base, and worktree operations do not receive AskPass access.

## Non-disclosure rules

- Plaintext must not be written to logs, Audit metadata, Session Events, Artifact metadata, object
  storage metadata, command-line arguments, or database columns.
- Browser query caches contain Credential metadata only. Secret form state is cleared after a
  successful create or rotation.
- Worker/Lease tokens are never passed to provider runner processes. Agentd/provider adapters may
  resolve a configured Credential and deliver it to the provider process through an in-memory pipe;
  they must not persist or echo it.
- Git username/token never enter the Repository URL, argv, normal process environment, Workspace,
  Event, Artifact, Outbox, Audit metadata, or Provider Runner Input. Network Git clears ambient
  Credential helpers, proxy/header overrides, hooks, URL rewrites, includes, filters, and unsafe remote
  commands before use.
- Audit records cover create, rotate, and revoke operations using metadata only. Plaintext resolution
  is deliberately not evented or logged.
