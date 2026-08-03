# Self-hosted KMS Worker Operations

Status: repository implementation and operator workflow; deployment acceptance remains environment-specific

This runbook operates the dedicated `kms-worker` defined by
[`self-hosted-kms-worker-v1.md`](../contracts/self-hosted-kms-worker-v1.md). It protects the Control Plane credential
envelope KEK. It does not replace the runtime-secret keyring or the Vault Worker-release signer.

## Security boundary

- Run `kms-worker` separately from the Control Plane and every Execution Worker.
- Give only the Control Plane client identity `control-plane-cryptor`. It must have no lifecycle role.
- Use separate identities for key management, disable/delete requests, deletion approval, and audit.
- Keep the sealing key outside the KMS database. A database backup is recoverable only with separately controlled sealing
  key custody.
- Do not expose port `3790` without mTLS. The Compose overlays bind it to `127.0.0.1` by default for operator access.
- Do not put a key, certificate, private key, wrapped key, AAD, or plaintext in a report or command-line argument.

## Required files

Generate the 32-byte sealing key into an owner-only file:

```bash
install -d -m 700 /secure/synara-kms
openssl rand -base64 32 > /secure/synara-kms/seal.key
chmod 600 /secure/synara-kms/seal.key
```

Use the deployment's certificate authority to issue:

| Identity          | Required certificate identity                       | Files                                                                    |
| ----------------- | --------------------------------------------------- | ------------------------------------------------------------------------ |
| KMS server        | DNS SANs for `kms-worker` and the operator hostname | `server/tls.crt`, `server/tls.key`, `server/client-ca.crt`               |
| Control Plane     | `spiffe://synara/control-plane`                     | `control-plane/ca.crt`, `control-plane/tls.crt`, `control-plane/tls.key` |
| Key manager       | `spiffe://synara/kms-manager`                       | operator-owned client certificate and key                                |
| Key disabler      | `spiffe://synara/kms-disabler`                      | operator-owned client certificate and key                                |
| Deletion approver | `spiffe://synara/kms-deletion-approver`             | independently controlled client certificate and key                      |
| Auditor           | `spiffe://synara/kms-auditor`                       | read-only client certificate and key                                     |

The server requires TLS 1.3 and a certificate chaining to `server/client-ca.crt`. A URI SAN is authoritative when present;
otherwise the certificate Common Name is used. Certificates with multiple URI identities are rejected.

Configure exact roles without giving the Control Plane or deletion requester an approval role:

```bash
export SYNARA_KMS_IDENTITY_ROLES_JSON='{
  "spiffe://synara/control-plane":["control-plane-cryptor"],
  "spiffe://synara/kms-manager":["kms-key-manager"],
  "spiffe://synara/kms-disabler":["kms-key-disabler"],
  "spiffe://synara/kms-deletion-approver":["kms-deletion-approver"],
  "spiffe://synara/kms-auditor":["kms-auditor"]
}'
```

## Start and bootstrap

The optional Compose overlays are:

- `deploy/personal/self-hosted-kms.override.yml`
- `deploy/saas/self-hosted-kms.override.yml`

Set the three host paths before Compose interpolation:

```bash
export SYNARA_KMS_SEAL_KEY_FILE=/secure/synara-kms/seal.key
export SYNARA_KMS_SERVER_TLS_DIRECTORY=/secure/synara-kms/server
export SYNARA_KMS_CONTROL_PLANE_TLS_DIRECTORY=/secure/synara-kms/control-plane
```

Start only the KMS first. This lets an operator create the initial immutable key version before configuring the Control
Plane:

```bash
docker compose \
  -f deploy/saas/docker-compose.yml \
  -f deploy/saas/self-hosted-kms.override.yml \
  up -d kms-worker
```

Create a logical key with the manager certificate. Reuse the same `Idempotency-Key` after any ambiguous response:

```bash
curl --fail-with-body \
  --cacert /secure/synara-kms/operator/ca.crt \
  --cert /secure/synara-kms/operator/manager.crt \
  --key /secure/synara-kms/operator/manager.key \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: initial-credential-kek' \
  -d '{"name":"Synara credential envelope KEK","policyReference":"self-hosted-kms-v1"}' \
  https://127.0.0.1:3790/v1/keys
```

Retain the returned logical `keyId` and configure the exact first version, never a movable alias:

```bash
export SYNARA_CREDENTIAL_KMS_KEY_ID='<logical-key-uuid>/versions/1'
```

Then start the deployment with both Compose files. The Control Plane performs an mTLS `Describe` at startup and fails if
the exact primary version is not encryptable.

The overlay is intentionally opt-in. The ordinary Compose files keep their existing `local` provider and do not require
KMS certificate paths.

## API behavior

Management requests require `Idempotency-Key`. Errors return a stable code and never echo request bodies or cryptographic
material. Relevant endpoints are:

```text
POST  /v1/keys
GET   /v1/keys/{keyId}
PATCH /v1/keys/{keyId}
POST  /v1/keys/{keyId}:rotate
POST  /v1/keys/{keyId}/versions/{version}:disable
POST  /v1/keys/{keyId}/versions/{version}:scheduleDeletion
POST  /v1/keys/{keyId}/versions/{version}:cancelDeletion
POST  /v1/keys/{keyId}/versions/{version}:destroy
GET   /v1/audit-events?limit=100
GET   /v1/reseal-receipts?limit=100
GET   /v1/destruction-receipts?limit=100
GET   /metrics
```

`Wrap` and `Unwrap` are Control Plane data-plane endpoints and are not operator workflows. The caller-supplied AAD and the
exact immutable version identity are authenticated by AES-256-GCM.

## Metadata and expiry update

`PATCH` requires `expectedRevision`. It can change name, description, labels, policy reference, and the active version's
two lifecycle timestamps. It cannot change key material, algorithm, identity, or creation time. A timestamp set to JSON
`null` is cleared.

```json
{
  "expectedRevision": 3,
  "encryptNotAfter": "2027-08-03T00:00:00Z",
  "decryptNotAfter": "2027-11-03T00:00:00Z"
}
```

`encryptNotAfter` is checked synchronously on every Wrap. `decryptNotAfter` is checked independently on every Unwrap; a
stopped reconciler cannot extend either boundary.

## Managed-key rotation

Rotate with a non-secret change reference:

```bash
curl --fail-with-body \
  --cacert "$KMS_CA" --cert "$KMS_MANAGER_CERT" --key "$KMS_MANAGER_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: rotation-change-123' \
  -d '{"operatorReference":"change-123"}' \
  -X POST "https://127.0.0.1:3790/v1/keys/$KMS_KEY_ID:rotate"
```

Rotation atomically creates version `N+1`, makes it active, and changes version `N` to decrypt-only. It does not migrate
existing Synara envelopes. Complete the existing bridge/switch/rewrap/converge flow:

1. Add both exact `synara-kms` versions to every replica's readable keyring.
2. Change `SYNARA_CREDENTIAL_KMS_KEY_ID` to version `N+1` and roll every replica.
3. Run `control-plane-metadata rewrap-kms` without `--execute`; stop if `unconfigured` is non-zero.
4. Execute with `--execute --operator change-123` and retain the immutable receipt.
5. Repeat the dry-run from a current image and require zero non-primary envelopes.
6. Disable the old version only after representative Credential and SSO reads.

The decrypt-key JSON for an old self-hosted version contains paths, not PEM or key contents:

```json
[
  {
    "provider": "synara-kms",
    "keyId": "<logical-key-uuid>/versions/1",
    "endpoint": "https://kms-worker:3790",
    "caFile": "/run/secrets/synara-kms/client/ca.crt",
    "clientCertFile": "/run/secrets/synara-kms/client/tls.crt",
    "clientKeyFile": "/run/secrets/synara-kms/client/tls.key",
    "timeout": "10s"
  }
]
```

## Disable and delayed destruction

Disabling rejects Wrap and Unwrap immediately. Destruction is a separate delayed workflow and cannot target an active or
decrypt-only version.

Generate current Control Plane inventory evidence with the metadata command. The command counts Provider Credential,
enterprise identity Connection, and login-attempt envelopes for the exact version and binds the result to the supplied
deployment and database identities:

```bash
control-plane-metadata rewrap-kms \
  --deletion-evidence-provider synara-kms \
  --deletion-evidence-key-id "$OLD_VERSIONED_KEY_ID" \
  --deployment-identity "$SYNARA_DEPLOYMENT_IDENTITY" \
  --database-identity "$SYNARA_DATABASE_IDENTITY"
```

`resourceCount` must be zero. Submit the returned `inventory` object with `scheduleDeletion`, a destruction time at least
seven days in the future, and a non-secret change reference. Before physical destruction, run the inventory command again.
The second evidence must be fresh, postdate the deletion request, and have a different `evidenceId`. Submit it through
`:destroy` using the independently controlled deletion-approver certificate.

Cancellation before destruction returns the version to `disabled`, never `active`. Destruction clears sealed material but
retains the immutable key/version tombstone, approvals, evidence digests, and audit events. A destroyed identity cannot be
reused.

## Root sealing-key rotation

Root rotation changes only the encrypted representation of managed key material. It does not change managed key versions
or require Control Plane envelope rewrap.

1. Create a new owner-only 32-byte sealing-key file.
2. Mount the new file as `SYNARA_KMS_SEAL_KEY_FILE` and the old file through
   `SYNARA_KMS_FALLBACK_SEAL_KEY_FILES_JSON` using an additional read-only volume.
3. Stop the serving replica so no lifecycle or data-plane request races the offline maintenance command.
4. Run `kms-worker reseal` with `SYNARA_KMS_RESEAL_ACTOR_IDENTITY` and
   `SYNARA_KMS_RESEAL_OPERATOR_REFERENCE`.
5. Verify the receipt through `GET /v1/reseal-receipts`, restart on the new key, and perform representative Wrap/Unwrap.
6. Remove the fallback only after a verified database backup/restore drill under the new custody set.

The reseal is one database transaction and is resumable by rerunning it: versions already sealed by the new key are
unchanged. Losing all usable sealing keys makes the self-hosted managed keys irrecoverable.

## Provider replacement

`local`, `synara-kms`, and `aws-kms` use the same Control Plane envelope interface. Provider replacement uses the same
reader bridge and online rewrap procedure as version rotation. Keep the old provider configured as decrypt-only until the
inventory converges, then retire it under its own lifecycle policy. KMS unavailability never authorizes plaintext, an
arbitrary fallback key, or database ciphertext rewriting.

## Verification boundary

Repository tests cover SQLite lifecycle behavior, mTLS authorization, mixed-provider rewrap, expiry, deletion evidence,
idempotency, immutable audit/receipt guards, and sealing-key rewrap. A production deployment still needs current evidence
for external certificate issuance, workload identity, PostgreSQL/backup/restore, monitoring, custody separation, incident
response, availability, and human change approval. A software self-hosted KMS is not an HSM.
