# Enterprise documentation

This is the supported entry point for Stage 6 enterprise documentation. API and external-developer documentation remain
Stage 7 scope. Every release candidate must review these pages against the deployed UI, configuration, compatibility
matrix and approved internal-user wording; checked-in documentation alone is not publication evidence.

## Choose your path

| Audience                      | Start here                                        | Use it for                                                                      |
| ----------------------------- | ------------------------------------------------- | ------------------------------------------------------------------------------- |
| End user                      | [User guide](user-guide.md)                       | Sign-in, Tenant context, agent work, usage visibility, release notices and help |
| Tenant owner or administrator | [Administrator guide](administrator-guide.md)     | Identity, members, Credentials, usage, governance, support and lifecycle        |
| Deployment operator           | [Deployment guide](deployment-guide.md)           | Profile selection, production prerequisites, rollout, readiness and rollback    |
| Support or on-call operator   | [Troubleshooting guide](troubleshooting-guide.md) | UI-first diagnosis, safe evidence collection, escalation and recovery routing   |

Deep operational procedures remain in the canonical runbooks:

- [Enterprise administrator daily operations](../runbooks/enterprise-admin-daily-operations.md)
- [Control Plane production operations](../runbooks/control-plane-operations.md)
- [Incident response](../runbooks/enterprise-incident-response.md)
- [Backup and recovery drill](../runbooks/backup-recovery-drill.md)
- [Secret, certificate, domain and key rotation](../runbooks/production-secret-certificate-domain-rotation.md)
- [Worker release rollout](../runbooks/worker-release-rollout.md)

## Documentation release gate

Before checking the documentation item in a copied Stage 6 GA checklist:

1. Run the source documentation validator and retain its receipt. It proves required pages, markers and local links exist;
   its assessment is deliberately `source-documentation-validated-not-release-verified`.
2. Test every documented Settings label and Tenant/user path in the deployed candidate with the named role. Record denied-role
   cases too; an Owner-only control visible to a Member is a release blocker.
3. Compare configuration names and defaults with the immutable image, `/ready`, deployment manifests and the compatibility
   matrix. Do not copy secrets, private hostnames or Tenant/user identifiers into documentation evidence.
4. Verify SLO, Region, Provider-processing, privacy, cost-accounting and support wording against the signed deployment annex.
   The repository defines engineering boundaries; it cannot sign a commercial or compliance promise.
5. Publish the approved release entry through the in-product channel and retain its visible version, timestamp, audience and
   content hash alongside the copied change-notice checklist.

Run the source check from the repository root:

```bash
python3 scripts/stage6-documentation/validate_documentation_set.py \
  --repository-root . \
  --matrix docs/release-matrices/stage-6-documentation-v1.json
```
