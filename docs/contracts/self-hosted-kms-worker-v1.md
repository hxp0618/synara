# Self-hosted KMS Worker Contract v1

Status: implemented repository contract; not production-approved

This contract defines a small Synara-managed key-management service for deployments that do not use a cloud KMS. It
also preserves the existing provider boundary so the same Control Plane deployment can use the self-hosted service,
AWS KMS, or a future cloud/Vault adapter without changing business ciphertext formats.

The service is called `kms-worker` in this contract. It is a dedicated internal service role, not a normal Synara
Execution Worker and not part of `agentd`.

## Goals

Version 1 provides:

- immutable symmetric key versions for envelope encryption;
- create, metadata update, rotate, expire, disable, delayed delete, and describe operations;
- authenticated `Wrap` and `Unwrap` operations without exporting managed key material;
- an append-only audit trail and idempotent management operations;
- a provider adapter that fits the existing `(provider, keyId)` credential envelope identity;
- migration between `local`, `synara-kms`, and supported cloud KMS providers through the existing online rewrap flow.

Version 1 does not provide:

- an HSM, a certified cryptographic boundary, or equivalence to a cloud KMS/HSM service;
- tenant-managed BYOK/HYOK, public KMS APIs, asymmetric signing, certificate authority, or general secret storage;
- automatic failover from an unavailable provider to another key or plaintext;
- key material import/export through the API;
- use by Provider subprocesses, arbitrary Execution workloads, or tenant code.

## Trust boundary and topology

```text
                         management identity
                                 |
                                 v
Control Plane  -- mTLS -->  dedicated kms-worker  --> encrypted key store
     |                               |
     |                               +--> append-only audit records
     |
     +--> credential business ciphertext + wrapped data keys

Execution Worker / agentd / Provider Host
     +--> no KMS credentials, no seal key, and no general Unwrap authority
```

The Control Plane remains authoritative for Tenant access, Credential scope, Execution Lease, Generation, expiry,
revocation, and audit ownership. It completes those checks before requesting an `Unwrap`. Moving those authorization
decisions into `kms-worker`, or giving a normal Execution Worker unrestricted `Unwrap`, is outside this contract.

`kms-worker` must run as a separate process, container, operating-system user, and Kubernetes ServiceAccount from the
Control Plane and every Execution Worker. It must not mount Workspaces, Provider credentials, Execution artifacts,
Worker registration tokens, or Provider configuration.

A personal single-node deployment may place the Control Plane and `kms-worker` on the same host. That is a deployment
convenience, not process isolation, host-compromise resistance, HSM evidence, or production approval.

## Provider boundary

The Control Plane depends on two conceptual interfaces. They may be represented by Go interfaces internally, but the
contract is behavior rather than an exact source signature.

```go
type KeyCryptor interface {
	Provider() string
	KeyID() string
	Describe(ctx context.Context) (KeyDescription, error)
	WrapKey(ctx context.Context, dataKey, aad []byte) ([]byte, error)
	UnwrapKey(ctx context.Context, wrappedDataKey, aad []byte) ([]byte, error)
}

type KeyLifecycle interface {
	CreateKey(ctx context.Context, input CreateKeyInput) (KeyDescription, error)
	UpdateKey(ctx context.Context, keyID string, input UpdateKeyInput) (KeyDescription, error)
	RotateKey(ctx context.Context, keyID string, input RotateKeyInput) (KeyDescription, error)
	DisableKey(ctx context.Context, keyID string, input DisableKeyInput) (KeyDescription, error)
	ScheduleKeyDeletion(ctx context.Context, keyID string, input DeleteKeyInput) (KeyDescription, error)
}
```

The data-plane interface is mandatory. Lifecycle management is capability-based:

- `synara-kms` implements both data plane and lifecycle management.
- `local` remains a deployment-provided static key and implements only the data plane.
- `aws-kms` continues to use an existing immutable AWS KMS key identity and implements only the data plane by default.
- future `vault-transit`, `gcp-kms`, and `azure-keyvault` adapters may expose lifecycle capabilities only when the
  deployment explicitly grants them.

Cloud keys are operator-provisioned by default. Synara must not infer permission to create, change, disable, or delete
a cloud-provider key merely because it has `Encrypt`/`Decrypt` access.

Every credential envelope continues to store the exact immutable `(provider, keyId)` used to wrap its data key. A
provider alias that can move between underlying keys is not an immutable Key ID and must not be persisted as one.

## Self-hosted key identity and versions

A self-hosted logical key has a stable opaque `keyId`, such as a UUID. It contains one or more immutable versions. A
version has a monotonically increasing positive integer, immutable algorithm, creation time, and encrypted key material.

The provider identity stored in a Synara credential envelope is:

```text
provider = synara-kms
keyId    = <logical-key-id>/versions/<version>
```

Persisting the exact version makes historical ciphertext deterministic to route and prevents an alias or primary-pointer
change from silently changing decryption authority. `Wrap` against a logical-key endpoint resolves the current primary
inside `kms-worker` and returns the immutable versioned Key ID with the wrapped result.

Version 1 supports only random 256-bit symmetric keys generated inside `kms-worker`. A managed key version's plaintext
material is never returned by an API, written to logs, stored unsealed, or accepted from a caller.

## Lifecycle state machine

Logical keys and immutable versions use the following effective states:

```text
pending-active --> active --> decrypt-only --> disabled --> pending-deletion --> destroyed
                         \-----------> disabled
```

Only one version of a logical key may be `active`. The operations have these exact meanings:

| Operation | Required behavior                                                                                                                                                                      |
| --------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Create    | Creates a logical key and version `1` using fresh random key material. Version `1` becomes active atomically.                                                                          |
| Update    | Changes only mutable metadata, policy references, and lifecycle timestamps using optimistic concurrency. It never replaces key material, algorithm, Key ID, version, or creation time. |
| Rotate    | Creates version `N+1`, makes it active, and changes version `N` to decrypt-only in one transaction. It never overwrites version `N`.                                                   |
| Expire    | At `encryptNotAfter`, the version stops accepting new Wrap operations. Unwrap remains allowed until its separately configured retirement boundary.                                     |
| Disable   | Rejects both Wrap and Unwrap. Re-enabling is not included in v1; recovery requires an explicit future contract.                                                                        |
| Delete    | First enters pending-deletion. Physical destruction occurs only after the safety checks, approval reference, and minimum delay defined below.                                          |

`pending-active` is an internal transactional state and must never be advertised as usable. A failed create or rotation
must leave the previous active version unchanged.

### Expiry is not deletion

Every version may have two distinct timestamps:

- `encryptNotAfter`: after this instant, `Wrap` fails with `key_not_encryptable`.
- `decryptNotAfter`: after this instant, `Unwrap` fails with `key_not_decryptable`.

`decryptNotAfter` must be later than `encryptNotAfter`. Omitting `decryptNotAfter` keeps the version decryptable until
an explicit disable or deletion. Expiry never silently destroys material and never changes an envelope to another key.

The service evaluates lifecycle timestamps on every request, rather than relying only on a background reconciler. A
reconciler may materialize state changes and metrics, but its delay cannot extend cryptographic authority.

### Deletion safety

Immediate key destruction is forbidden. `ScheduleKeyDeletion` requires:

- the exact immutable version identity;
- a non-secret operator/change reference;
- an idempotency key;
- a configured delay of at least seven days;
- evidence from the current Control Plane inventory that no authoritative envelope references the version;
- a second independent inventory after the delay and before destruction.

An inventory result is bound to the Control Plane deployment identity, database identity, key version, completion time,
resource counts, and digest. `kms-worker` rejects stale, mismatched, missing, or non-zero inventory evidence. The normal
Control Plane runtime identity cannot create or approve its own destruction evidence.

Cancellation during the delay is allowed and audited. It returns the version to `disabled`, never directly to `active`
or `decrypt-only`. After destruction, the key row, version identity, timestamps, approval reference, and audit digest
remain as a tombstone; only cryptographic material is removed. A destroyed identity can never be reused.

Cloud-provider deletion remains an operator-owned external action unless a future provider-specific contract explicitly
defines equivalent evidence, delay, and separation of duties.

## Cryptography and storage

The self-hosted data plane wraps only random envelope data-encryption keys. Business payload encryption stays in the
Control Plane under the existing AES-256-GCM envelope format and resource-specific AAD.

For each `Wrap` request, `kms-worker`:

1. resolves an active immutable key version;
2. validates the caller, key policy, request size, lifecycle state, and timestamp;
3. encrypts the 32-byte data key with AES-256-GCM under that managed key version;
4. authenticates the caller-supplied AAD, versioned Key ID, format version, and algorithm;
5. returns the versioned Key ID and versioned wrapped-key blob.

For `Unwrap`, it routes only by the exact versioned Key ID and authenticates the same context. Authentication failure,
unknown format, truncated ciphertext, wrong AAD, disabled state, or unknown Key ID fails closed.

The encrypted key store contains metadata and key versions sealed under a deployment-provided root sealing key. The root
sealing key:

- is independent from every Provider Credential, runtime-secret key, Worker signing key, database credential, and TLS key;
- is provided through an owner-controlled secret mount, workload-backed secret delivery, operating-system key store, TPM,
  or HSM integration;
- is never stored in the `kms-worker` database, configuration JSON, audit record, backup, command line, or API response;
- must be available before the service reports ready.

An environment variable may supply the seal key for personal or development deployments. Production deployments should
prefer a file/agent/workload delivery mechanism that avoids broad process-environment inheritance.

Rotating the root sealing key re-seals the unchanged managed key versions in an atomic, resumable, audited maintenance
operation. It does not change their versioned Key IDs or require rewrapping Synara credential envelopes. Losing every
usable copy of the sealing key makes the self-hosted managed keys irrecoverable. Compromise of the seal key and encrypted
key store compromises the self-hosted KMS boundary; this is the principal security difference from an HSM-backed service.

## Persistence authority

The self-hosted store must durably represent at least:

- logical keys and mutable metadata versions;
- immutable cryptographic key versions and their lifecycle timestamps;
- the single active-version pointer per logical key;
- idempotency records for every mutating request;
- append-only audit events;
- deletion requests, inventory evidence, approvals, cancellation, and destruction receipts;
- seal-key re-sealing runs and completion receipts.

Key creation, rotation, primary selection, lifecycle transition, idempotency result, and audit insertion must commit in
one transaction for each operation. Concurrent mutation uses compare-and-swap against a metadata revision. An ambiguous
client retry with the same idempotency key and identical request returns the original result; reuse with different input
is a conflict.

SQLite with strict owner-only filesystem permissions, WAL durability, and verified backups is supported for a personal
single-node deployment. A multi-replica service requires one authoritative transactional database and leader-safe
lifecycle reconciliation. Copying one SQLite file into multiple writable replicas is unsupported.

Backups contain sealed key material and are security-sensitive even though they contain no plaintext key. Restore
acceptance must prove the database and matching sealing-key custody in an isolated environment without exposing either
artifact in reports.

## Internal API contract

The exact transport may be HTTP or gRPC. The initial HTTP resource model is:

```text
POST   /v1/keys
GET    /v1/keys/{keyId}
PATCH  /v1/keys/{keyId}
POST   /v1/keys/{keyId}:rotate
POST   /v1/keys/{keyId}/versions/{version}:wrap
POST   /v1/keys/{keyId}/versions/{version}:unwrap
POST   /v1/keys/{keyId}/versions/{version}:disable
POST   /v1/keys/{keyId}/versions/{version}:scheduleDeletion
POST   /v1/keys/{keyId}/versions/{version}:cancelDeletion
POST   /v1/keys/{keyId}/versions/{version}:destroy
GET    /v1/audit-events
GET    /v1/reseal-receipts
GET    /v1/destruction-receipts
GET    /metrics
GET    /health/live
GET    /health/ready
```

The logical-key Wrap form may be used to select the current primary, but its response must always return the exact
versioned Key ID. Unwrap never accepts a logical alias.

Every mutating management request carries `Idempotency-Key`, expected metadata revision, actor identity, and a non-secret
operator reference where required. Data-plane bodies are strictly size-bounded. Responses, errors, traces, and access
logs must not include plaintext data keys, unredacted wrapped blobs, sealing material, or caller AAD.

Stable error categories include:

```text
key_not_found
key_version_not_found
key_not_encryptable
key_not_decryptable
key_policy_denied
key_version_conflict
key_deletion_inventory_required
key_deletion_inventory_nonzero
key_authentication_failed
kms_unavailable
```

Authentication, authorization, and lifecycle failures must not be collapsed into a successful response using another
key.

## Authentication and authorization

All non-health endpoints require mutually authenticated TLS or an equivalent workload-identity channel. Network location
alone is not authentication. The service must reject plaintext transport outside an explicitly marked local development
mode.

At minimum, v1 separates these machine/operator roles:

- `control-plane-cryptor`: Describe, Wrap, and Unwrap only for configured logical keys;
- `kms-key-manager`: create, metadata update, and rotate;
- `kms-key-disabler`: disable and schedule/cancel deletion;
- `kms-deletion-approver`: submit independent zero-reference evidence and authorize destruction;
- `kms-auditor`: read non-secret metadata, audit events, and receipts without Wrap, Unwrap, or mutation.

The runtime Control Plane identity must not create keys, rotate keys, alter expiry, disable keys, schedule deletion, approve
destruction, or read audit storage directly. No identity may broaden its own policy.

## Control Plane configuration

The existing provider configuration remains authoritative. A self-hosted deployment selects the new adapter with values
equivalent to:

```text
SYNARA_CREDENTIAL_KMS_PROVIDER=synara-kms
SYNARA_CREDENTIAL_KMS_KEY_ID=<immutable-logical-key-or-version-reference>
SYNARA_CREDENTIAL_KMS_ENDPOINT=https://synara-kms.internal
SYNARA_CREDENTIAL_KMS_CA_FILE=/var/run/secrets/synara-kms/ca.crt
SYNARA_CREDENTIAL_KMS_CLIENT_CERT_FILE=/var/run/secrets/synara-kms/tls.crt
SYNARA_CREDENTIAL_KMS_CLIENT_KEY_FILE=/var/run/secrets/synara-kms/tls.key
```

Names are part of this proposed contract and are not evidence that configuration parsing exists. Secret values and PEM
contents must not appear in a JSON keyring, checked-in manifest, report, or command-line argument.

Decrypt-only entries may mix providers, for example an old `local` key, the current `synara-kms` key, and a future
`aws-kms` key. Startup rejects duplicate identities, aliases without immutable resolution, unavailable mandatory
decryptors, and a configured primary that cannot pass an authenticated canary Wrap/Unwrap.

## Rotation and provider replacement

Managed-key rotation and provider replacement use the existing credential envelope rotation contract:

1. **Bridge readers:** deploy the old identity and future identity to every Control Plane replica as readable keys.
2. **Switch writer:** make the new immutable identity primary while retaining the old decryptor.
3. **Inventory:** run the existing read-only `rewrap-kms` inventory and reject unknown identities.
4. **Rewrap:** unwrap each existing data key with its exact old provider/version and wrap the same data key with the new
   primary. Business payload ciphertext, AAD, Credential version, and resource timestamps remain unchanged.
5. **Converge:** require zero non-primary envelopes and retain the existing immutable completion receipt.
6. **Retire:** stop old Wrap operations, remove the old decryptor only after representative reads, then disable and
   schedule deletion under the provider's policy.

This flow supports `local -> synara-kms`, `synara-kms -> aws-kms`, `aws-kms -> synara-kms`, and future provider pairs. An
outage during migration never falls back to plaintext or silently selects a different provider.

The self-hosted lifecycle rotation operation creates a new KMS version, but it does not by itself migrate existing Synara
envelopes. The Control Plane rewrap run remains the authority for migration and retirement readiness.

## Availability and failure behavior

- `kms-worker` unavailability makes new credential writes and authorized credential resolution unavailable; it does not
  reveal plaintext or authorize a fallback key.
- The Control Plane must not hold a database transaction or row lock across remote KMS I/O.
- Wrap and Unwrap calls have bounded deadlines, bounded retries for provably retryable transport failures, and no automatic
  retry after an ambiguous mutating lifecycle response unless the same idempotency key is reused.
- Readiness requires the authoritative database, audit sink, sealing key, active key metadata, and an in-memory
  authenticated canary. Liveness must not claim that cryptographic operations are ready.
- Clock uncertainty that could extend expiry or deletion authority fails closed and makes the affected lifecycle operation
  unavailable.
- Audit failure rejects management mutations. Data-plane audit delivery may use a bounded durable outbox, but exhaustion
  rejects new operations rather than dropping records silently.

Required non-secret metrics include request count/latency by operation and status, key-state counts, time until primary
expiry, rewrap inventory counts, audit-outbox depth, seal-key generation identity hash, and deletion-delay status. Metrics
must not label plaintext, ciphertext, AAD, key bytes, certificate content, or unbounded Tenant/resource identifiers.

## Minimum implementation slice

The first implementation is intentionally bounded to:

1. a dedicated Go `kms-worker` binary;
2. one self-hosted AES-256-GCM backend with a deployment-supplied 32-byte sealing key;
3. SQLite for personal/single-node mode and PostgreSQL for multi-replica mode;
4. mTLS identities for Control Plane cryptor and separate operator management;
5. create, describe, metadata update, rotate, expiry enforcement, disable, delayed delete/cancel, Wrap, and Unwrap;
6. immutable audit/idempotency records and deletion receipts;
7. a `synara-kms` implementation of the existing Control Plane key-wrapper boundary;
8. mixed-provider rewrap tests covering `local <-> synara-kms <-> aws-kms` with AWS represented by a controlled test
   double unless a real cloud acceptance environment is explicitly in scope.

The first implementation does not add a tenant-facing UI. Lifecycle operations begin as operator-only CLI/internal API
workflows so authorization, evidence, and failure semantics stabilize before a UI exposes them.

## Acceptance boundary

Source completion requires focused tests that prove:

- key material is random, sealed at rest, absent from responses/logs/audits, and zeroed from temporary buffers where
  practical;
- wrong AAD, wrong identity, corrupted ciphertext, expired/disabled/destroyed keys, and missing seal material fail closed;
- concurrent create/rotate/update/delete requests preserve one active version and deterministic idempotency results;
- rotation never overwrites key material and old versions remain decrypt-only until explicitly disabled;
- expiry is enforced synchronously even when the reconciler is stopped;
- deletion cannot proceed with missing, stale, mismatched, or non-zero inventory evidence;
- process/database interruption preserves committed operations and supports deterministic recovery;
- mixed-version Control Plane replicas can bridge, switch, rewrap, converge, and retire without changing business
  ciphertext;
- Execution Workers and Provider processes receive no KMS client identity, sealing key, or general Unwrap authority.

Repository tests, local PostgreSQL/SQLite tests, and a source receipt are implementation evidence only. Production approval
still requires deployment-specific identity, TLS, backup/restore, custody, availability, monitoring, incident, rotation,
destruction, and human change-control evidence. A self-hosted software KMS must be labeled as such and must not be described
as HSM-backed unless independently true and verified.

## Relationship to existing contracts

- [`credential-kms-rotation-v1.md`](./credential-kms-rotation-v1.md) remains authoritative for Synara credential-envelope
  rewrap, evidence, and zero-downtime retirement.
- [`runtime-secret-key-rotation-v1.md`](./runtime-secret-key-rotation-v1.md) remains a separate keyring and migration path;
  its key material must not be moved into or reused by this service without a future explicit contract.
- [`worker-protocol-v2.md`](./worker-protocol-v2.md) remains authoritative for generation-fenced Credential resolution and
  the rule that Provider processes receive task-scoped authority rather than long-lived Provider secrets.
- [`vault-kms-operations.md`](../runbooks/vault-kms-operations.md) covers Worker release signing and Vault operational
  custody. Signing keys are outside this envelope-encryption KMS v1.
