# Enterprise deployment guide

This guide routes a production deployment through the supported Synara artifacts. It does not replace cloud-specific
architecture review, a signed data-residency annex, the candidate GA checklist or a real recovery/capacity/security exercise.

## Choose the deployment profile

| Profile       | Metadata and artifact baseline                                                 | Intended use                                        |
| ------------- | ------------------------------------------------------------------------------ | --------------------------------------------------- |
| `personal`    | SQLite and local artifact storage                                              | One local owner; not an enterprise multi-user claim |
| `single-node` | PostgreSQL and S3-compatible object storage                                    | Operator-managed single-site service                |
| `enterprise`  | PostgreSQL, object storage, multiple Control Plane replicas and remote Workers | Production enterprise control plane                 |

Execution Target kind (`local`, `ssh`, `docker`, or `kubernetes`) is independent of the Control Plane profile. The current
product boundary is self-hosted/operator-managed; EKS/GKE/AKS identity, native cloud billing and managed-cloud availability
are not implied by an `enterprise` profile.

Start with the [single-node Compose guide](../../deploy/saas/README.md) or the
[Kubernetes guide](../../deploy/kubernetes/README.md). Read the
[deployment-profile contract](../contracts/deployment-profile-v1.md) before mixing profiles or Targets.

## Production prerequisites

Provide independently managed PostgreSQL, artifact storage, KMS or Vault, Queue/outbox dependencies, DNS/TLS, an OTLP
collector, monitoring, paging and an internal Status Board appropriate to the selected profile. Use dedicated workload
identities and least privilege. Never commit `.env`, private keys, cookies, tokens, KMS credentials or customer hostnames.

An enterprise OTLP exporter is optional, but enabling it is fail closed. The Control Plane requires an absolute HTTPS
OTLP/HTTP endpoint, mounted mTLS client certificate/key files, the collector's processing Region and a 1–90 day
trace-retention declaration. Every non-Local agentd instead requires a credential-free HTTPS endpoint or explicit IP-loopback
HTTP relay and rejects Header/client identity injection; exporter credentials are never sent
through Worker registration or claims. The declarations become bounded resource metadata, not proof of collector access,
storage Region or deletion. Attach the collector IAM/network policy and observed retention/Region evidence to the deployment
residency and security annex. The Kubernetes base keeps its endpoint empty by default, reads the policy from its ConfigMap,
and mounts the dedicated mTLS identity read-only from the Secret. Generated native/Warm Workers use separate fixed optional,
credentialless ConfigMaps in each Target Namespace; an external relay/service mesh owns Collector identity. Recycle Workers
after endpoint or server-CA configuration changes. Managed Docker and SSH instead derive a Target-UUID child from their
operator-configured host root and let agentd parse only the bounded exporter allowlist; Target JSON cannot select host paths.
The static validator proves wiring only.

For enterprise Support Access, set a dedicated internal Platform Operator Tenant. Do not reuse a workload Tenant. Configure
the browser-visible Control Plane origin before exporting OIDC/SAML metadata and allow artifact CORS only from the exact Web
origin. Every non-loopback production endpoint uses HTTPS and Secure cookies.

After the independent internal Status Board and employee notification relay exist, set
`SYNARA_INTERNAL_STATUS_BOARD_URL` and `SYNARA_INTERNAL_INCIDENT_PUBLISHER_URL` to credential-free HTTPS URLs, and
provide the relay's dedicated base64 32-byte `SYNARA_INTERNAL_INCIDENT_PUBLISHER_HMAC_KEY` through the Secret manager.
The publisher must use a different origin from the Control Plane and Admin. An empty Status Board value is reported
explicitly as unconfigured; a populated link and webhook acceptance do not replace the paging and communication
exercise or employee delivery evidence.

The current product boundary is `internal-self-hosted`. External recurring billing is unsupported; do not configure Stripe
API keys, Webhook secrets, Price IDs, Checkout return URLs or Customer Portal. Provider and platform costs are reconciled to
the organization's existing cost-center process; Synara does not collect payment details or claim tax/settlement authority.

## Install and migrate

1. Pin the Git commit, Web/Control Plane/Worker image digests, Provider CLI versions, SBOMs and signatures for one candidate.
2. Take and verify a restore point. A successful backup command is not recovery evidence.
3. Run the compatibility validator and review the
   [cross-component compatibility contract](../contracts/release-compatibility-v1.md).
4. Apply the forward-only migration chain embedded in the Control Plane image. `/ready` reports the authoritative expected
   schema version; never rewrite an applied migration or manually insert its checksum row.
5. Roll out compatible Control Plane and Web builds, then canary and promote the Worker release. Stop on schema checksum,
   protocol, authorization, isolation, signature, SLO-burn or evidence-authority failure.
6. Observe real user and administrator paths before declaring the release complete.

Rollback uses the newest compatible immutable artifact. Do not down-migrate or directly edit Execution, Lease, event,
outbox or release state. Follow the [release-governance runbook](../runbooks/enterprise-release-governance.md).

## Secrets, domains and rotation

Provider Credentials and enterprise identity secrets use envelope encryption. Provider Cursor and Execution Target runtime
secrets use a separate named keyring. Roll compatible readers first, add the new primary while keeping the old key decrypt-
only, perform audited online rewrap/rekey, prove zero remaining old-key inventory, then remove the old key. Certificate and
Domain changes require overlap and real client-path verification. Use the
[production rotation runbook](../runbooks/production-secret-certificate-domain-rotation.md); replacing a key in place is not
a supported rotation.

## Readiness, monitoring and acceptance

`/health` proves process liveness. `/ready` proves dependencies, schema and serving readiness; only ready replicas receive
traffic. Preserve SSE streaming headers and idle timeouts through every proxy. Install the bounded metrics and alerts from
the deployment manifests, but do not treat rule installation as a successful SLO window.

Before GA, copy [the Stage 6 checklist](../release-checklists/stage-6-enterprise-ga.md) and attach candidate-specific evidence
for migration, compatibility, supply chain, role-based browser operations, 30-day SLO, capacity/soak, recovery, incident
communication, key rotation and third-party penetration testing. Repository validators intentionally distinguish source or
evidence-shape validation from a passed production control. Before final review, use the
[candidate evidence bundle contract](../contracts/stage-6-candidate-evidence-bundle-v4.md) to prove every external receipt
belongs to the same clean commit, environment ID, origin, Migration tail, lockfile and Artifact set.
