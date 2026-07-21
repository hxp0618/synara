# Vault KMS operations

This runbook defines the production operational boundary for the Synara Vault-backed
worker signing KMS. It covers the non-secret control set only: signing identity,
transparency/admission policy references, Shamir custody, the isolated snapshot
restore drill, and audit-log retention/export expectations.

## Production references

- KMS reference: `hashivault://synara-worker-release`
- Signer identity: `auth/approle/role/synara-worker-release-signer`
- Transit audit request path: `transit/sign/synara-worker-release`
- Transparency log: online upload and verification against public Rekor at
  `https://rekor.sigstore.dev`, with both the inclusion proof and signed entry
  timestamp (SET) required; offline or ignored-tlog verification is not an
  approved release path.
- Admission policy: Kyverno `Enforce` + webhook `Fail`, `mutateDigest=true`,
  `verifyDigest=true`, and `ignoreTlog=false`, checked in at
  `deploy/kubernetes/security/cluster/verify-synara-worker-images.yaml`.
- Exact-digest admission uses the live `synara-system/synara-worker-cosign-public-key`
  public-key bundle and `synara-system/synara-worker-signing-settings` repository/tlog
  boundary. A tag-only match or an unlogged signature is insufficient.

## Transit key lifecycle

- Key type: ECDSA P-256 (`ecdsa-p256`).
- The key must remain non-exportable, non-derived, non-deletable, and ineligible
  for plaintext backup. The release gate reads these properties from live
  Vault; relying on Vault defaults alone is not sufficient evidence.
- Automatic Transit rotation is disabled because admission uses an explicit
  public-key trust bundle. Rotate at least annually and immediately after a
  suspected signer or cryptographic-boundary compromise.
- Rotation is a staged operation: add the new public key alongside the old key,
  require Rekor-backed positive and old/new-key negative admission probes,
  switch signing to the new Transit version, roll all immutable Worker images,
  and remove the old public key only after no runnable release references it.
- The signer AppRole cannot create, configure, rotate, export, back up, or
  delete the Transit key. Those operations remain under the separately audited
  custody/change-control path.

## Credential environment names

Signing runtime:

- `VAULT_ADDR`
- `VAULT_TOKEN`
- `VAULT_CACERT`

Snapshot restore drill:

- `VAULT_ADDR`
- `VAULT_CACERT`
- `VAULT_SNAPSHOT_OPERATOR_ROLE_ID`
- `VAULT_SNAPSHOT_OPERATOR_SECRET_ID`
- `VAULT_SNAPSHOT_RESTORE_KEY_1`
- `VAULT_SNAPSHOT_RESTORE_KEY_2`
- `VAULT_SNAPSHOT_RESTORE_KEY_3`

Audit SIEM shipping:

- `VAULT_AUDIT_SIEM_ENDPOINT`
- `VAULT_AUDIT_SIEM_RESOLVE`
- `VAULT_AUDIT_SIEM_CLIENT_CERT`
- `VAULT_AUDIT_SIEM_CLIENT_KEY`
- `VAULT_AUDIT_SIEM_CA_CERT`

External WORM archive verification:

- `VAULT_AUDIT_WORM_MC_ALIAS`
- `VAULT_AUDIT_WORM_MC_CONFIG_DIR`
- `VAULT_AUDIT_WORM_MC_HOST`
- `VAULT_AUDIT_WORM_MC_VERIFIER_HOST`
- `VAULT_AUDIT_WORM_MC_RESOLVE`

Do not commit values for any of these environments. Reports may retain the
environment variable names and hashes only.

## Short-lived operator credential session

For the repository-owned Stage 3 production-like overlay observed on July 21,
2026, the current live non-secret runtime boundary is:

- Kubernetes context: `kind-synara-stage3-prod`
- Vault namespace: `synara-kms`
- active Vault Service: `synara-vault-active`
- Registry host: `192.168.139.3:5443`
- Registry repository: `192.168.139.3:5443/synara/worker`
- Registry container: `synara-stage3-prod-registry`

Long-running rollout, load, or soak gates must finish before minting Vault or
Registry material. Start the short-lived credential shell only immediately
before the Registry/KMS/admission/snapshot/SIEM chain.

Required launcher inputs:

- `SYNARA_STAGE3_KMS_RUNTIME`
- `SYNARA_VAULT_INIT_JSON`
- `VAULT_ADDR`
- `VAULT_CACERT`
- optional `SYNARA_STAGE3_REGISTRY_HOST`

The launcher emits a clean shell that exports:

- `VAULT_TOKEN`
- `VAULT_OPERATOR_TOKEN`
- `VAULT_SNAPSHOT_OPERATOR_ROLE_ID`
- `VAULT_SNAPSHOT_OPERATOR_SECRET_ID`
- `VAULT_SNAPSHOT_RESTORE_KEY_1`
- `VAULT_SNAPSHOT_RESTORE_KEY_2`
- `VAULT_SNAPSHOT_RESTORE_KEY_3`
- `REGISTRY_USERNAME`
- `REGISTRY_PASSWORD`
- `REGISTRY_CA_CERT`
- `SYNARA_STAGE3_CREDENTIAL_SESSION=ready`

The helper itself is operator-owned and not checked into this repository.
Treat `SYNARA_STAGE3_KMS_RUNTIME` as the owner-only runtime directory that
contains the live boundary files plus:

- `bin/start-short-lived-credential-session.py`
- `bin/vault-kubectl-active`

Start it with environment-variable names and owner-only paths only:

```sh
export SYNARA_STAGE3_KMS_RUNTIME=/secure/synara-stage3-kms-runtime
export SYNARA_VAULT_INIT_JSON=/secure/synara-vault/init.json
export VAULT_ADDR=<approved-live-vault-address>
export VAULT_CACERT="$SYNARA_STAGE3_KMS_RUNTIME/ca.crt"

"$SYNARA_STAGE3_KMS_RUNTIME/bin/start-short-lived-credential-session.py"
```

Inside that shell, prefer the checked wrapper
`"$SYNARA_STAGE3_KMS_RUNTIME/bin/vault-kubectl-active"` for live Vault CLI
commands so every gate resolves the current active Vault pod on
`kind-synara-stage3-prod` without separate port-forwarding.

## Shamir custody policy

- Scheme: `5` total shares, `3` required to restore or generate a new root.
- Minimum participants per drill or incident: `3` distinct custodians.
- Custodian classes must span platform/SRE, security, and operations.
- Each share must live in a separate managed secret store or hardware-backed
  password manager under distinct operator control.
- Rotate the custody set after custodian departure, suspected share exposure,
  major cluster rebuild, or any cryptographic boundary change.

## Generate-root break-glass

- Checked-in steady state is explicit fail-closed:
  `enable_unauthenticated_access = []` in
  `deploy/kubernetes/security/vault/values.production.yaml`.
- The only approved temporary unauthenticated opening is
  `enable_unauthenticated_access = ["generate-root"]`.
  No other endpoint family may be added.
- The window is limited to `15` minutes maximum and requires `3` of `5`
  Shamir custodians spanning platform/SRE, security, and operations.
- Open the window only by applying a temporary top-level HCL edit to the live
  Vault server config, then sending `SIGHUP` to the exact `/bin/vault server`
  PID on every pod. Do not signal a shell wrapper or leave the temporary file
  in place after the window closes.
- Before opening, record the approved change ticket and verify an
  unauthenticated `sys/generate-root/attempt` probe returns `403`.
- During the window, verify the same unauthenticated probe returns `200`,
  complete the `3/5` share submissions, and mint only the minimum privileged
  replacement material required to restore signer operations.
- Immediately after the root is generated, restore the checked-in config,
  `SIGHUP` every Vault pod again, and verify unauthenticated
  `sys/generate-root/attempt` returns `403`.
- Revoke the generated root token immediately after the replacement signer
  token or Secret ID is minted. Reports retain only operator identities,
  timestamps, status codes, and hashes; never token or share material.
- Required audit evidence for every window:
  `approved-change-record`, `pre-open-403-check`, `window-open-200-check`,
  `three-custodian-share-submissions`, `post-close-403-check`,
  `generated-root-revoked`.

## Snapshot operator identity

- Policy file:
  `deploy/kubernetes/security/vault/synara-vault-snapshot-operator.hcl`
- AppRole name: `synara-vault-snapshot-operator`
- Required role constraints:
  - no default policy
  - batch token
  - `1800s` TTL
  - `3600s` max TTL
  - `1` Secret ID use
  - `600s` Secret ID TTL

The snapshot operator is read-only. Its `sys/audit` access is limited to
`read, sudo`, which Vault requires to inspect audit-device metadata during the
restore drill; it has no audit create, update, or delete capability. It must not
be reused as the signing identity.

## Isolated snapshot restore drill

The drill must never restore into the source release or source Kubernetes
namespace. The approved restore target is an isolated Docker Vault with:

- fresh local Raft state
- `--network none`
- a non-executable, nodev, nosuid `/vault/audit` tmpfs owned by Vault UID 100
- no `retry_join`
- no source-cluster DNS or Service discovery
- exact cleanup of the named container and temporary state after the run

Run the gate with an empty output directory:

```bash
python3 scripts/stage3-provider-acceptance/vault_snapshot_restore_drill.py \
  --vault-bin "$SYNARA_STAGE3_KMS_RUNTIME/bin/vault-kubectl-active" \
  --output-dir ./.stage3-vault-restore-drill
```

The gate:

1. logs in with the snapshot-operator AppRole from environment variables
2. captures a source Raft snapshot without modifying the source cluster
3. initializes the isolated Vault with the same 5-share / 3-share-threshold
   Shamir boundary, then restores the snapshot
4. waits for Vault's asynchronous snapshot application to finish and return to
   the sealed state before submitting any source share
5. unseals the restored Vault with the three supplied source Shamir shares
6. verifies the single-node Raft leader, the two restored audit file devices and
   their writable 0600 files, the transit key, and the signer, auditor, and
   snapshot-operator AppRoles
7. writes secret-safe JSON and Markdown evidence
8. removes the container, local snapshot, tmpfs, and temporary state

The formal release run rejects a dirty worktree before reading any Vault
credential. Its report must include `source.gitSha` and
`source.worktreeDirty=false`; a diagnostic run from a dirty checkout is not a
release artifact.

Required report artifacts:

- `vault-snapshot-restore-drill.json`
- `vault-snapshot-restore-drill.md`

## Audit retention and external SIEM

The production baseline is exactly two local PVC-backed file audit devices:

- `file -> /vault/audit/audit-primary.log`
- `file-secondary -> /vault/audit/audit-secondary.log`

Operational requirements:

- rotate outside the Vault process
- rotate at `100 MiB` or less per file
- keep at least `7` compressed files per stream
- export to an external SIEM or collector over mTLS or private transport
- retain the exported copy immutably outside the Vault cluster
- keep export lag within `5` minutes

The retained copy uses an S3-compatible bucket with versioning enabled and a
`365` day default Object Lock in `COMPLIANCE` mode. `GOVERNANCE` mode, an
application-only append API, and a hash chain without storage enforcement do
not satisfy this boundary. The collector archives each canonical ledger batch
under `entries/`, including the complete structured Vault audit payload. The
release gate independently reads the exact retained version, recomputes the
batch, payload, and ledger-entry hashes from canonical JSON, and proves that
both version deletion and retention shortening are denied by the storage layer.

The archive credential is a dedicated `synara-vault-audit-archive` identity
bound to
`deploy/kubernetes/security/vault/audit-object-lock-writer-policy.json`. Its
exact allow-list is bucket introspection/listing plus object Put/Get/retention
read; it has no DeleteObjectVersion or PutObjectRetention capability. It is not
a Vault, Registry, or cluster-admin credential. Acceptance passes the endpoint
and scoped credential only through `VAULT_AUDIT_WORM_MC_HOST`; reports retain
the environment name, bucket, object key/version, retention timestamp, and
hashes, never its value. The gate writes and reads a writer-owned version, then
requires the writer's delete attempt to fail specifically at IAM. The formal
gate uses the separate, short-lived
`VAULT_AUDIT_WORM_MC_VERIFIER_HOST` identity for destructive negative probes,
and requires its delete and retention-shortening attempts to fail specifically
at COMPLIANCE Object Lock, so an archive writer's IAM denial cannot be mistaken
for storage enforcement. That verifier is not injected into Vault or the
long-running collector.

The concrete Kind Service IP, API server address, OrbStack host address, and
Registry host in this repository are the `kind-synara-stage3-prod` overlay.
Each real production cluster must render and review its own addresses before
deployment.

Local file rotation without an external retained sink is not sufficient for
Stage 3 go-live.

Start the repository-provided acceptance collector only with Object Lock
enabled. Its state directory and all TLS/mc material must be outside the
repository:

```sh
python3 scripts/stage3-provider-acceptance/vault_audit_acceptance_sink.py \
  --bind-host 0.0.0.0 \
  --port 18443 \
  --state-dir /secure/synara-vault-audit-state \
  --server-cert /secure/synara-vault-audit-tls/server.crt \
  --server-key /secure/synara-vault-audit-tls/server.key \
  --client-ca-cert /secure/synara-vault-audit-tls/ca.crt \
  --retention-days 365 \
  --object-lock-required
```

Then run the formal gate from a clean source SHA. Credential values remain in
the named environments:

```sh
python3 scripts/stage3-provider-acceptance/vault_audit_siem_delivery_gate.py \
  --vault-command-json "[\"$SYNARA_STAGE3_KMS_RUNTIME/bin/vault-kubectl-active\"]" \
  --vault-auditor-token-env VAULT_OPERATOR_TOKEN \
  --kube-context kind-synara-stage3-prod \
  --vault-namespace synara-kms \
  --vault-statefulset synara-vault \
  --output-dir /tmp/synara-stage3-vault-audit-siem
```
