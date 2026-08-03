# Production secret, certificate, domain and key rotation acceptance v2

This is the active internal-self-hosted successor to v1. It retains the candidate identity, five immutable evidence files
per control, old/new authority separation, new-path and old-path probes, rollback, zero-risk requirement, Secret scan and
three-role approval semantics defined in
[production rotation acceptance v1](production-rotation-acceptance-v1.md).

The manifest schema is `synara.stage6-production-rotation-evidence.v2`; the path-only draft and validation receipt are
`synara.stage6-production-rotation-evidence-draft.v2` and
`synara.stage6-production-rotation-evidence-receipt.v2`.

The exact active control set is:

1. `credential-envelope-kek`;
2. `runtime-secret-key`;
3. `worker-registration-token`;
4. `postgresql-credential`;
5. `artifact-storage-credential`;
6. `internal-incident-publisher-hmac-key`;
7. `telemetry-relay-identity`;
8. `public-tls-certificate`; and
9. `public-domain-dns`.

The incident Publisher control binds distinct old/new Key IDs, relay overlap, a new-key signed notification, rollback to
the old version, return to the new version and final old-signature denial. The Key ID is sent as `X-Synara-Key-Id`; key
bytes must never be included in evidence.

V1 Billing-provider receipts remain historical evidence only. They cannot satisfy the active internal-self-hosted
rotation gate because this product rejects Billing Provider configuration.
