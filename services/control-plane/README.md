# Synara SaaS control plane

The Go control plane owns SaaS identity, tenants, organizations, memberships, RBAC, audit, sessions,
executions, leases, worker registration, and the revised Deployment Profile/Execution Target
foundations. The existing TypeScript server remains the Provider Runtime during the gradual migration.

## Persistence

- Metadata-store adapter: PostgreSQL for `single-node`/`enterprise`, pure-Go SQLite for `personal`
- PostgreSQL DDL: ordered, checksum-verified files under `migrations/`
  - `000001_identity_tenancy.sql`: identity, tenant, organization, membership, audit, outbox
  - `000002_projects_sessions.sql`: project, agent session, turn, ordered event, automation ownership
  - `000003_execution_worker_leases.sql`: execution queue, worker identity, leases, fencing, idempotent receipts
  - `000004_deployment_profiles_execution_targets.sql`: installation/profile metadata, execution targets,
    target-bound sessions/executions/workers, and metadata-import receipts
  - `000005_artifacts.sql`: tenant-scoped Artifact metadata, lifecycle constraints, local access tokens,
    and reentrant payload-migration receipts
  - `000006_tenant_quotas.sql`: tenant execution and ready-Artifact storage limits without redundant
    usage counters
  - `000007_provider_credentials.sql`: tenant-scoped encrypted Provider Credential envelopes,
    KMS-wrapped data keys, rotation versions, expiry, and revocation metadata
  - `000008_session_provider_credentials.sql`: explicit Session-to-Provider-Credential binding
  - `000009_tenant_retention_policies.sql`: Tenant Session/Artifact retention policy
  - `000010_enterprise_identity.sql`: OIDC/SAML connection boundary, Service Accounts,
    SCIM Groups and mappings, and protected login attempts
- ORM: GORM with PostgreSQL and CGO-free SQLite drivers
- Shared model/repository/transaction utilities: `internal/persistence`
- Tenant-scoped queries always include `tenant_id`
- Runtime CRUD uses GORM; raw SQL is confined to versioned DDL and the migration lock/runner
- Migrations are embedded, checksum-verified, serialized with a PostgreSQL advisory lock, and run
  automatically before the HTTP listener starts
- Personal SQLite uses `AutoMigrate` for all current models. It is intentionally single-control-plane
  and does not emulate distributed `SKIP LOCKED` semantics.

## Deployment profiles

The v1 profiles are validated before database startup:

```text
personal    = SQLite + local artifacts + in-process queue + one replica
single-node = PostgreSQL + MinIO/S3 + PostgreSQL outbox + one replica
enterprise  = PostgreSQL + S3 + outbox/external queue + multiple replicas
```

Execution targets (`local`, `ssh`, `docker`, `kubernetes`) are independent from profile. See
`docs/contracts/deployment-profile-v1.md` and `docs/contracts/execution-target-v1.md`.
Tenant audit search and streaming JSONL/CSV export are defined in `docs/contracts/audit-log-v1.md`.
Provider Credential envelope encryption and Worker retrieval are defined in
`docs/contracts/provider-credential-v1.md`.
Enterprise identity, retention, Provider Host, observability, and Worker image boundaries
are documented under `docs/contracts` and `docs/worker-image.md`.

## Local run

```bash
SYNARA_DEPLOYMENT_PROFILE=single-node \
SYNARA_DATABASE_URL='postgres://synara:password@127.0.0.1:5432/synara?sslmode=disable' \
SYNARA_CONTROL_PLANE_DEV_BOOTSTRAP=true \
SYNARA_LOGIN_COOKIE_SECURE=false \
SYNARA_WORKER_REGISTRATION_TOKEN='replace-with-a-random-secret' \
SYNARA_PUBLIC_CONTROL_PLANE_URL='https://synara.example.com' \
SYNARA_PROVIDER_CURSOR_KEY="$(openssl rand -base64 32)" \
SYNARA_CREDENTIAL_KMS_PROVIDER=local \
SYNARA_CREDENTIAL_KMS_KEY_ID=single-node-local-v1 \
SYNARA_CREDENTIAL_MASTER_KEY="$(openssl rand -base64 32)" \
SYNARA_ARTIFACT_ENDPOINT='http://127.0.0.1:9000' \
SYNARA_ARTIFACT_PUBLIC_ENDPOINT='http://127.0.0.1:9000' \
SYNARA_ARTIFACT_ACCESS_KEY_ID='synara' \
SYNARA_ARTIFACT_SECRET_ACCESS_KEY='replace-with-a-random-secret' \
go run ./cmd/api
```

Personal SQLite example:

```bash
SYNARA_DEPLOYMENT_PROFILE=personal \
SYNARA_SQLITE_PATH='./data/metadata.sqlite' \
SYNARA_CONTROL_PLANE_DEV_BOOTSTRAP=true \
SYNARA_PROVIDER_CURSOR_KEY="$(openssl rand -base64 32)" \
SYNARA_CREDENTIAL_KMS_PROVIDER=local \
SYNARA_CREDENTIAL_KMS_KEY_ID=personal-local-v1 \
SYNARA_CREDENTIAL_MASTER_KEY="$(openssl rand -base64 32)" \
SYNARA_LOCAL_AGENTD_RUNNER_COMMAND_JSON='["provider-host","run","--jsonl"]' \
go run ./cmd/api
```

On first startup, Personal creates one persisted installation ID plus deterministic Personal Tenant,
root/personal Organization, local owner User, both owner memberships, and the tenant-owned
`local-default` execution target. Dev login reuses that domain. When
`SYNARA_LOCAL_AGENTD_RUNNER_COMMAND_JSON` is configured, the control plane also supervises an
embedded Local agentd loop, generates an internal registration credential when necessary, and
restarts the Worker after unexpected exits. Its workspace defaults to `./data/workspaces` and can be
changed with `SYNARA_LOCAL_AGENTD_WORKSPACE_ROOT`.

## Personal metadata export/import

Stop or quiesce workers first; export rejects active leases/executions.

```bash
SYNARA_DEPLOYMENT_PROFILE=personal \
SYNARA_SQLITE_PATH='./data/metadata.sqlite' \
go run ./cmd/metadata export --output personal-metadata.json

SYNARA_DEPLOYMENT_PROFILE=single-node \
SYNARA_DATABASE_URL='postgres://synara:password@127.0.0.1:5432/synara?sslmode=disable' \
go run ./cmd/metadata import \
  --input personal-metadata.json \
  --source-artifact-dir ./data/artifacts
```

The manifest is versioned, import is transactional and idempotent, domain IDs are preserved, encrypted
provider resume cursors remain ciphertext, and ready Local Artifact payloads are copied and verified in
MinIO/S3. Treat the manifest as sensitive metadata, keep file permissions restrictive, and configure
the destination with the same encryption key when existing resume cursors must remain decryptable.

Set `SYNARA_CONTROL_PLANE_URL=http://127.0.0.1:3780` on the TypeScript Synara server. The web app
then uses the same-origin `/v1` proxy.

## Verification

```bash
go test ./...
docker build -t synara-control-plane:test .
../../deploy/saas/acceptance.sh http://127.0.0.1:3780
```

Development bootstrap is deliberately disabled by default outside the Personal example. Production
deployments should keep it disabled and connect an enterprise identity provider in the later security
phase.

Worker registration uses a separate bearer secret. A worker registers one `executionTargetId` and
`targetKind`, advertises lease/fencing support, receives a one-time worker token, and can claim only
compatible executions. Claimed executions receive a one-time lease token; metadata stores retain only
SHA-256 hashes. Provider resume cursors and non-empty target configuration use AES-256-GCM encryption.
Provider Credential payloads use a fresh AES-256-GCM data key per Credential version; only the
KMS-wrapped data key and authenticated ciphertext are stored.
Managed SSH targets pin the remote host key and install a target-specific systemd service. Configure
`SYNARA_PUBLIC_CONTROL_PLANE_URL` to an origin reachable by the remote Worker and keep
`SYNARA_AGENTD_BINARY_PATH` pointed at the built `synara-agentd` binary.
