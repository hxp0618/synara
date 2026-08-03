# Production secret, certificate, domain and key rotation

## Purpose and release boundary

Use this runbook for scheduled rotation and compromise response. It defines safe ordering and evidence; it does not claim
that a deployment has exercised the procedure. A Stage 6 release candidate remains blocked until the production owner has
attached real receipts, client-path results, timestamps and approvals to the release evidence bundle.

Never put secret values, private keys, bearer URLs, database DSNs, presigned URLs or decrypted configuration in tickets,
terminal transcripts, audit metadata or evidence. Record secret-manager versions and irreversible hashes/IDs only.

## Common change record

Before every rotation, record:

- change/incident reference, owner, approver, environment, Region and maintenance/rollback window;
- affected secret/certificate/domain IDs, current and target versions, consumers and maximum cache/lease/session lifetime;
- clean release commit and image digests, database restore point, current SLO/error-budget state and rollback authority;
- synthetic Tenant/User/Worker/Target resources used for probes, never a customer credential; and
- expected old-credential denial time and old-key disable/destruction date.

Stop on a red error budget, unavailable restore point, unknown consumer, replica configuration drift, or missing rollback
authority. After the change, collect config rollout status, readiness, error-rate and audit links, new-path success, old-path
denial, expiry/disable confirmation and operator/approver sign-off.

## Credential/identity envelope KEK

Follow [`credential-kms-rotation-v1.md`](../contracts/credential-kms-rotation-v1.md). The required two-reader/one-writer
sequence is old-primary+new-fallback on all replicas, then new-primary+old-fallback on all replicas, audited rewrap, zero-old
inventory, and only then fallback removal/old-key retirement.

Local example (the values live in the secret manager, not the command history):

```text
SYNARA_CREDENTIAL_KMS_PROVIDER=local
SYNARA_CREDENTIAL_KMS_KEY_ID=local-v2
SYNARA_CREDENTIAL_MASTER_KEY=<new key from secret manager>
SYNARA_CREDENTIAL_MASTER_KEY_OLD_V1=<old key from secret manager>
SYNARA_CREDENTIAL_KMS_DECRYPT_KEYS_JSON=[{"provider":"local","keyId":"local-v1","localKeyEnvironment":"SYNARA_CREDENTIAL_MASTER_KEY_OLD_V1"}]
```

Compose dry-run and execution:

```bash
docker compose --env-file .env run --rm \
  --entrypoint /usr/local/bin/control-plane-metadata \
  control-plane rewrap-kms

docker compose --env-file .env run --rm \
  --entrypoint /usr/local/bin/control-plane-metadata \
  control-plane rewrap-kms --execute --operator CHANGE-1234 --batch-size 200
```

For an interrupted execution, use the persisted run UUID and the same operator reference:

```bash
control-plane-metadata rewrap-kms \
  --execute --operator CHANGE-1234 --resume-run-id 00000000-0000-0000-0000-000000000000
```

Retain both dry-run JSON documents, the immutable receipt, matching Tenant audit counts, representative Credential resolve,
OIDC/SAML login, and a final startup/read test after removing the old fallback. Do not disable the old KMS key before that
last test passes on every replica.

## Provider cursor and Execution Target configuration key

`SYNARA_PROVIDER_CURSOR_KEY` protects both bound Provider resume cursors and encrypted Execution Target configuration. It
uses a separate named keyring and re-encryption receipt defined in
[`runtime-secret-key-rotation-v1.md`](../contracts/runtime-secret-key-rotation-v1.md). Do not overwrite it during an ordinary
rolling update.

The compatibility-code rollout and key switch are separate changes:

1. roll the new binary with `SYNARA_PROVIDER_CURSOR_KEY_ID` and decrypt-key JSON still unset; it continues writing the old
   envelope format, so pre-upgrade replicas remain compatible;
2. after every replica runs the new binary, deploy the old key as named primary `runtime-v1` and the future new key as a
   decrypt-only entry; prove all replicas read both canaries;
3. roll to new primary `runtime-v2` plus old decrypt-only `runtime-v1`; all new writes now carry an authenticated Key ID;
4. dry-run and execute the online re-encryption command, retaining the immutable receipt:

```bash
control-plane-metadata rekey-runtime-secrets

control-plane-metadata rekey-runtime-secrets \
  --execute --operator CHANGE-1234 --batch-size 200
```

5. verify zero pending/unconfigured rows, Target reconciliation for SSH/Docker/Kubernetes, a new native Provider checkpoint
   and resume, matching Tenant audit events, and the global entries for platform-owned Targets; and
6. remove the old fallback from every replica, repeat the probes, then retire the old key under policy.

Only `usable` Provider Cursors are re-encrypted. Non-usable ciphertext is counted explicitly and remains unusable; it cannot
be promoted back into a native resume decision without a fresh Provider Cursor. A missing old key, inconsistent stored Key
ID, binding failure or malformed envelope stops execution before a run is created.

## Worker registration and worker identity

The shared registration token is used only to obtain a per-Worker token; existing registered Workers continue with their
own fenced identity. For scheduled rotation, update the Control Plane registration token first, then Worker templates and
one-shot Kubernetes token staging, and finally replace idle Workers. During the interval, an old unregistered Worker cannot
register, so pause autoscaling and keep warm/capacity headroom. Prove new registration, heartbeat/claim, old registration
denial, and no unexpected loss of existing Worker heartbeats. Revoke individual Worker identities through the authoritative
revocation path; do not rotate the shared token as a substitute for Worker compromise containment.

## Database, object storage, billing and telemetry credentials

Prefer provider-issued overlapping credentials or workload identity:

1. create the new credential with least privilege and the same Region/resource boundary;
2. deploy it to one canary consumer, prove read/write/delete or read-only behavior as appropriate, then roll all consumers;
3. verify PostgreSQL pool replacement, Artifact upload/download, billing import and OTLP export with synthetic data;
4. revoke the old credential and prove its access is denied from a controlled client path; and
5. retain provider audit IDs and secret-manager version metadata.

If the provider cannot overlap credentials, use a maintenance window and stop writers. Never change database/object
authority and credential in the same rollback unit. Presigned URLs and cached sessions remain valid until their own bounded
expiry unless the storage provider supports explicit revocation.

For Worker OTLP identity, rotate the client credential at the external relay or service-mesh boundary; never mount it into
agentd or the Provider-visible filesystem. Prove a canary Worker still exports through the new relay identity, roll the
relay boundary, revoke the old identity, and prove old-identity denial. Recycle Workers only when their credentialless
endpoint or server-CA policy changes. Never place OTLP Header tokens, client certificate/key paths or certificate bytes in
Execution Target JSON, Worker registration, Claims or the generated SSH environment file.

## Public TLS certificate

Stage the complete new certificate chain and private key in the ingress/load-balancer secret while the old certificate is
still valid. Validate hostname/SAN, issuer, chain, OCSP/CRL policy, key algorithm, expiry and private-key match offline.
Roll one edge, probe SNI/ALPN and `/ready` from outside every advertised Region, then roll the remainder. Verify Web login,
SSE, Worker API and Artifact public endpoint with real TLS clients. Roll back the edge secret if any client path fails.
Revoke the old certificate after all edges serve the new fingerprint; emergency compromise rotation skips the overlap but
still requires external client-path proof.

## Public domain and DNS

Domain migration is a two-endpoint coexistence change, not a DNS-only edit:

1. lower TTL at least one previous-TTL interval in advance; verify ownership and issue the new certificate;
2. deploy routes, CORS, cookie Domain/SameSite/Secure settings, SSO redirect/ACS/entity metadata, Artifact public endpoint,
   Worker Control Plane URL and trusted proxy boundaries for the new origin;
3. keep the old origin serving redirects or compatible APIs for at least the maximum DNS/client/session lifetime;
4. shift synthetic traffic, then weighted/authoritative DNS; check public resolvers, IPv4/IPv6, SNI, Web login, SSO, SSE,
   Worker registration/heartbeat and Artifact upload/download from each supported Region; and
5. remove the old origin only after logs show no supported clients, IdP/Worker configurations are updated, and rollback DNS
   is no longer required. Remove old certificate/private key and DNS validation records under separate approval.

Cookie-domain changes can invalidate sessions and SSO metadata changes require IdP administrator coordination. Record that
user-visible impact explicitly; never silently widen a cookie domain.

## Exercise result

Each rehearsal or production change ends as `passed`, `failed`, or `not-assessable`. Source tests, a dry-run, a Secret update,
certificate issuance, or a DNS response alone cannot be marked `passed`. Attach measured timings, old-path denial, rollback
result and unresolved risks to `docs/release-checklists/stage-6-enterprise-ga.md` evidence; keep its checkbox open until a
production-equivalent exercise is approved.

Materialize the nine-control manifest defined by
[`production-rotation-acceptance-v1.md`](../contracts/production-rotation-acceptance-v1.md), keeping all evidence below one
private non-symlink directory. Use only opaque authority IDs and evidence hashes; never copy the credential itself into the
manifest or attachments. Then run:

```bash
bun run stage6:rotation:prepare -- \
  --evidence-root /secure/stage6-rc1 \
  --draft production-rotation-draft.json \
  --repository-root . \
  --manifest-output production-rotation-manifest.json \
  --receipt-output production-rotation-receipt.json
```

The path-only draft avoids manually transcribing 49 evidence digests. The preparer computes them, requires candidate and
Migration identity, nine exact controls, five unique evidence files per control, a zero-finding Secret scan and distinct
Security/Operations/Release decisions. It also rejects obvious private keys, live provider credentials, bearer tokens and
credential-bearing URLs without echoing the matched value. Preserve failed and not-assessable receipts. Even an eligible
receipt remains `evidence-validated-not-production-rotation-passed`; independently verify the external evidence and
approver authority before checking the GA item.
