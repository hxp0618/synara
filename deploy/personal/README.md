# Personal profile Compose example

This example runs one control plane with a persisted pure-Go SQLite metadata database, a declared
local artifact directory, an in-process queue, and the existing TypeScript Provider Runtime.

```bash
cp .env.example .env
mkdir -p workspace
# Fill the auth token, cursor/KMS keys, and Worker registration token in `.env`.
docker compose up --build
```

To use the dedicated software KMS instead of the static local Credential KEK, add
`self-hosted-kms.override.yml` and follow
[`docs/runbooks/self-hosted-kms-worker.md`](../../docs/runbooks/self-hosted-kms-worker.md). The overlay keeps the KMS in a
separate container/user/volume and requires mTLS plus separate lifecycle roles; it is not enabled by default.

The first control-plane startup persists an installation ID when `SYNARA_INSTALLATION_ID` is empty,
then creates the deterministic Personal Tenant, root/personal Organization, local owner memberships,
and tenant-owned `local-default` execution target.

The Personal profile now provides the same Artifact lifecycle without S3: tenant-scoped metadata,
short-lived local upload/download grants, SHA-256/size/Content-Type verification, lifecycle deletion,
and an explicit Local-to-MinIO/S3 migration path.

Managed Docker targets are opt-in through `docker-workers.override.yml`; the same Docker socket and
group-permission warning as the single-node profile applies.
