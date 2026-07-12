# Single-node PostgreSQL + MinIO profile

This Compose profile runs PostgreSQL metadata, a private MinIO bucket, the Go control plane, and the
existing Synara web/provider runtime.

```bash
cp .env.example .env
mkdir -p workspace
# Fill every required secret in .env.
docker compose up --build
```

`SYNARA_ARTIFACT_ENDPOINT` uses the internal Compose address `http://minio:9000` for control-plane
operations. `SYNARA_ARTIFACT_PUBLIC_ENDPOINT` is built from `MINIO_PUBLIC_HOST` and
`MINIO_HOST_PORT`; it must be reachable from browsers because it is embedded in presigned upload and
download URLs.

Provider Credentials use envelope encryption. The example defaults to a separate local 32-byte KEK
in `SYNARA_CREDENTIAL_MASTER_KEY`; production deployments can set
`SYNARA_CREDENTIAL_KMS_PROVIDER=aws-kms` with a KMS key ID and region instead. Rotating this KEK
requires an explicit Credential re-encryption procedure; do not replace it in place.

To enable managed Docker Worker Pools, build an image that contains `/usr/local/bin/synara-agentd`
and the configured provider runner, then add the socket override:

```bash
docker compose --env-file .env \
  -f docker-compose.yml -f docker-workers.override.yml up --build
```

The Docker socket grants host-level container control. Keep it opt-in, set `SYNARA_DOCKER_GID` to the
socket-owning group on Linux, and never expose the Docker API over an unauthenticated TCP endpoint.

For browser uploads, set `SYNARA_ARTIFACT_CORS_ALLOW_ORIGIN` to the exact Synara web origin in
production. The example defaults can be used for local acceptance only.

Personal migration:

```bash
control-plane-metadata export --output manifest.json

control-plane-metadata import \
  --input manifest.json \
  --source-artifact-dir /path/to/personal/artifacts
```

The import is reentrant. It verifies source and destination SHA-256 values and records one migration
receipt per Artifact and destination object key.

## Enterprise SAML

Create a Tenant SAML connection from the Enterprise identity settings with the IdP metadata URL.
Synara generates a unique SP entity ID and KMS-encrypted signing key for every connection. Register
the entity ID, HTTP-POST callback, and signing certificate from this endpoint in the IdP:

```text
${SYNARA_PUBLIC_CONTROL_PLANE_URL}/v1/auth/sso/{connectionId}/metadata
```

The IdP must sign the SAML response or assertion and should validate Synara's signed RSA-SHA256
AuthnRequest. Configure email, display-name, and Group attribute names to match the connection, then
add explicit Group mappings in Synara. Production metadata URLs must use HTTPS; loopback HTTP is
accepted only for local acceptance. `SYNARA_PUBLIC_CONTROL_PLANE_URL` must be the browser-reachable
origin before exporting metadata, otherwise the advertised assertion consumer URL will be wrong.
