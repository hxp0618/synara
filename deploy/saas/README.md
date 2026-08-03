# Single-node PostgreSQL + MinIO profile

This Compose profile runs PostgreSQL metadata, a private MinIO bucket, the Go control plane, the
Tenant Web/provider runtime, and the independently built Platform Admin surface.

```bash
cp .env.example .env
mkdir -p workspace
# Fill every required secret in .env.
# Local-only acceptance (keep the published ports loopback-bound):
SYNARA_PUBLIC_URL= SYNARA_ALLOW_INSECURE_REMOTE=true docker compose --env-file .env up --build
```

For a deployed internal environment, keep `SYNARA_ALLOW_INSECURE_REMOTE=false` and set
`SYNARA_PUBLIC_URL=https://<tenant-web-origin>` to the HTTPS origin terminated by the reverse proxy
before running the same Compose command. The local exception is deliberately explicit and must not be
copied into a reachable production deployment.

`SYNARA_ARTIFACT_ENDPOINT` uses the internal Compose address `http://minio:9000` for control-plane
operations. `SYNARA_ARTIFACT_PUBLIC_ENDPOINT` is built from `MINIO_PUBLIC_HOST` and
`MINIO_HOST_PORT`; it must be reachable from browsers because it is embedded in presigned upload and
download URLs.

MinIO root credentials are bootstrap-only. Set a different `MINIO_ARTIFACT_USER` and
`MINIO_ARTIFACT_PASSWORD`; the one-shot pinned `minio-bootstrap` service creates the private bucket and attaches only
bucket-location/list plus object read/write/delete/multipart operations under `tenants/*`. The Control Plane receives this
dedicated identity and waits for bootstrap success. Reusing either root value fails closed. Set
`SYNARA_ARTIFACT_CORS_ALLOW_ORIGIN` to one exact browser origin; no wildcard or fallback is accepted. This Compose policy is
single-node acceptance evidence, not proof of a cloud IAM attachment.

Provider Credentials use envelope encryption. The example defaults to a separate local 32-byte KEK
in `SYNARA_CREDENTIAL_MASTER_KEY`; production deployments can set
`SYNARA_CREDENTIAL_KMS_PROVIDER=aws-kms` with a KMS key ID and region instead. Rotate this KEK with
the two-reader/one-writer keyring and audited `control-plane-metadata rewrap-kms` procedure in
[`docs/runbooks/production-secret-certificate-domain-rotation.md`](../../docs/runbooks/production-secret-certificate-domain-rotation.md);
never replace a key in place.

Deployments that need lifecycle-managed keys without a cloud KMS can add
`self-hosted-kms.override.yml`. It runs the dedicated mTLS-only `kms-worker` with a separate data volume and sealing-key
mount, then selects the exact `synara-kms` key version in the Control Plane. Bootstrap, rotation, provider replacement,
sealing-key rotation, and delayed deletion are documented in
[`docs/runbooks/self-hosted-kms-worker.md`](../../docs/runbooks/self-hosted-kms-worker.md). The overlay is opt-in and does
not change this Compose file's `local` default.

The separately named Provider Cursor key also protects Execution Target configuration. Roll the compatible binary first
with no Key ID, then use the named primary/decrypt-only keyring and
`control-plane-metadata rekey-runtime-secrets` procedure in the same production rotation runbook. This separation prevents
old binaries from encountering the new keyed envelope during a rolling upgrade.

Enterprise deployments must set `SYNARA_PLATFORM_OPERATOR_TENANT_ID` to a dedicated internal Tenant.
Its `owner|admin` members approve time-bounded Support Access, while `security_admin` members may
request it. Do not reuse a workload Tenant. Tenant opt-in, four-eyes approval, read-only request
enforcement, and revocation drill are documented in
[`docs/runbooks/control-plane-operations.md`](../../docs/runbooks/control-plane-operations.md#11-受控-support-access).

Platform Admin is exposed separately on `SYNARA_ADMIN_HOST_PORT` (3774 by default) and proxies only
its same-origin `/v1` requests to the Control Plane. `SYNARA_PUBLIC_WEB_URL` is injected at runtime as
the handoff target after an approved Support Engineer enters `support_readonly`; set it to the Tenant
Web HTTPS origin in production. Keep `SYNARA_ADMIN_ENABLE_DEV_LOGIN=false` outside an
isolated acceptance environment. The Admin host rechecks the active Operator Tenant and Platform role
on every load; sharing a login cookie does not make Tenant Settings a Platform surface.

Set `SYNARA_PUBLIC_ADMIN_URL` to the separate Admin HTTPS origin. The Control Plane recognizes only a
fixed Platform Admin SSO return marker and maps it to this configured URL; arbitrary absolute return
URLs remain rejected. When Control Plane, Tenant Web, and Admin use different subdomains, configure
one narrow shared `SYNARA_LOGIN_COOKIE_DOMAIN`, keep `SYNARA_LOGIN_COOKIE_SECURE=true`, and restrict
ingress so only the intended hosts proxy `/v1`.

The Web container binds `0.0.0.0` internally. Production must set `SYNARA_PUBLIC_URL` to the HTTPS
root origin served by the TLS-terminating reverse proxy; an empty value is treated as unset and the
remote-access policy remains fail-closed. For an isolated trusted-LAN acceptance run only, set
`SYNARA_ALLOW_INSECURE_REMOTE=true` explicitly and keep the published port loopback-bound.

Set `SYNARA_INTERNAL_STATUS_BOARD_URL`, `SYNARA_INTERNAL_INCIDENT_PUBLISHER_URL`, an opaque
`SYNARA_INTERNAL_INCIDENT_PUBLISHER_HMAC_KEY_ID`, and a dedicated base64 32-byte
`SYNARA_INTERNAL_INCIDENT_PUBLISHER_HMAC_KEY` together only after an internal Status Board and employee notification
relay are live. The relay verifies the exact `v1` HMAC body, deduplicates by the Outbox Message ID, and returns 2xx only
after durable acceptance; redirects and non-2xx responses fail closed into the existing retry/dead-letter path.
The value must be a credential-free HTTPS URL without a query or fragment, and its origin must differ
from both the Control Plane and Platform Admin origins. When configured, the sanitized public Platform
profile exposes it and Web/Admin show an **Internal status** link. Leaving it empty reports
`configured: false`; source configuration alone does not close the internal communications exercise gate.

Payment providers are not part of this internal self-hosted product. Do not add legacy
`SYNARA_COMMERCIAL_BILLING_*` or `SYNARA_STRIPE_*` settings: startup rejects them. The Usage UI and internal cost review
cover Token, Provider cost and platform allocation only; they do not create invoices or collect payment data.

## Distributed tracing

Set `OTEL_EXPORTER_OTLP_ENDPOINT` to the trusted OTLP/HTTP collector endpoint and choose a bounded
`SYNARA_OTEL_TRACE_SAMPLE_RATIO` between `0` and `1` (the example uses `0.1`). With no endpoint, export is disabled but
W3C `traceparent`, `X-Trace-ID` response correlation and immutable Execution trace handoff remain active. Control Plane
HTTP spans freeze a diagnostic parent on new Executions; Worker claims pass it to agentd, which creates
`worker.execution` and `provider.run` child spans and provides `SYNARA_TRACEPARENT` to Provider Host processes.

The single-node profile may use a credential-free local HTTP collector. Authentication headers are rejected over HTTP.
The enterprise profile and every non-Local agentd require an HTTPS OTLP/HTTP endpoint, a complete client certificate/key
pair from `OTEL_EXPORTER_OTLP[_TRACES]_CLIENT_CERTIFICATE|CLIENT_KEY`, a bounded
`SYNARA_OTEL_COLLECTOR_REGION`, and `SYNARA_OTEL_TRACE_RETENTION_DAYS` between 1 and 90. Certificate paths must be absolute
regular files supplied by the deployment Secret authority. Embedded URL credentials, query/fragment data, incomplete mTLS
identity and non-`http/protobuf` protocols fail startup before any span is exported.

The Compose variables configure the Control Plane process only. Generated Kubernetes native/Warm Workers use a fixed optional
credentialless ConfigMap in each Target Namespace; sandbox-operator standard uses the same contract. Managed Docker, SSH,
Cocoon guest and any other separately deployed agentd receive only their own OTLP endpoint and sampling policy from that
Target's configuration authority; an external relay/service mesh owns collector credentials. Synara does not send collector credentials or exporter
configuration through Worker registration or Execution claims.

Managed Docker and SSH can use `deploy/worker/observability.env.example`. For Docker, set
`SYNARA_DOCKER_WORKER_OBSERVABILITY_ROOT` to an absolute directory on the Docker Engine host; Synara mounts only its
`<target-uuid>` child read-only at `/etc/synara/targets/<target-uuid>`. For SSH, set
`SYNARA_SSH_WORKER_OBSERVABILITY_ROOT` to the absolute directory already prepared on every remote host. In both cases the
generated agentd path must end in `<target-uuid>/observability.env`; the parent directory and file must not be group/world
writable, symlinks are rejected, and an optional server certificate path is confined to `ca.crt` beside that file. The parser
accepts only endpoint/protocol/server-CA/sample ratio/Region/retention; Header credentials, client certificate/key paths and
insecure overrides are forbidden. Empty roots keep export disabled. Prepare readable permissions for the agentd service UID,
then recycle Workers through the normal release flow after configuration changes.

Trace and span IDs are diagnostics only. Never use them for authorization, fencing, idempotency or Tenant scope. Restrict
collector access by Tenant-support policy, do not attach prompts, credentials, Provider payloads or Presigned URLs, and
apply trace retention/Region rules in the deployment residency annex. OTLP auth headers belong in the deployment secret
manager rather than `.env.example` or checked-in manifests.

To enable managed Docker Worker Pools, build an image that contains `/usr/local/bin/synara-agentd`
and the configured provider runner, then add the socket override:

```bash
docker compose --env-file .env \
  -f docker-compose.yml -f docker-workers.override.yml up --build
```

The Docker socket grants host-level container control. Keep it opt-in, set `SYNARA_DOCKER_GID` to the
socket-owning group on Linux, and never expose the Docker API over an unauthenticated TCP endpoint.

For browser uploads, set `SYNARA_ARTIFACT_CORS_ALLOW_ORIGIN` to the exact Synara web origin before Compose interpolation.

## Acceptance and failure drills

The normal single-node flow is covered by `acceptance.sh`. It verifies the internal-self-hosted Platform profile has no
payment surface (including payment-provider, Checkout, Stripe, Billing and public Status Page capability fields), creates
and selects a Standard internal Tenant without database intervention, uses an explicit Tenant-owned Local Execution Target,
and checks per-Session and per-Tenant Token, Network, Provider-cost and soft-quota projections across Execution generation
recovery, then assigns a project cost center and verifies the internal-cost CSV export. Multi-replica PostgreSQL/SSE/Claim behavior is covered by
`multi-replica-acceptance.sh`; it supplies disposable Compose-only artifact/CORS and Operator Tenant defaults, while
`SYNARA_MULTI_REPLICA_WEB_PORT`, `SYNARA_MULTI_REPLICA_MINIO_PORT`, and the authority variables remain overridable.

Run the isolated dependency and Worker failure drill with:

```bash
deploy/saas/failure-acceptance.sh
```

It exposes only the temporary Control Plane test port through
`control-plane-acceptance.override.yml`, generates all required PostgreSQL, MinIO artifact, Worker and Control Plane
authorities, runs the normal internal-product and usage acceptance first, verifies Worker offline recovery and Generation
Fencing, stops and restores MinIO and PostgreSQL, and scans Control Plane logs for Credential, Token, Lease, Prompt and
Presigned URL leakage. Containers, networks and volumes are removed on exit.

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
