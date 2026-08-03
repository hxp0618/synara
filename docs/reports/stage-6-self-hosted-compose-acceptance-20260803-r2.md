# Stage 6 self-hosted Compose acceptance — 2026-08-03 (r2)

## Scope and assessment

This report records two isolated local Compose runs of the internal self-hosted product profile plus one full Web/Admin
smoke. They are engineering evidence for usage/cost visibility, dependency recovery, multi-replica Control Plane
behavior and container readiness. They are not a clean release-candidate receipt, production evidence, external
security review or GA approval.

Assessment: `local-self-hosted-compose-usage-cost-recovery-and-ha-validated`.

## Subject

- Branch: `codex/saas-tenancy-user`
- Observed Git HEAD: `a7ea05fd0f98ae4d15ac5a14a67b02914875ce9f`
- Worktree: dirty (`675` porcelain entries); no clean-commit or candidate-artifact claim
- Latest migration exercised by the multi-replica run: `000162`

## Single-replica failure acceptance

Command:

```bash
SYNARA_FAILURE_RUN_BASE_ACCEPTANCE=1 SYNARA_FAILURE_KEEP_RESOURCES=0 \
  bash deploy/saas/failure-acceptance.sh
```

The run used Compose project `synara-stage2-failure-88326` and Control Plane image
`sha256:01463b7e4a3571f28fe752cd765d81855456a9f61d2b2ca002469f6cee524356`.

Passed checks:

- Standard internal Tenant, Organization, Local Target and Worker lifecycle without direct database work.
- Token/Network/Provider-cost usage projection and soft-quota semantics.
- Project cost-center/department CAS assignment, internal cost report and CSV export.
- Payment/Checkout/Stripe/Billing capability absence in the Platform profile and CSV.
- Cross-Tenant authorization, object hash verification and sensitive-log audit.
- Worker-offline recovery, MinIO outage/recovery and PostgreSQL outage/recovery.

## Multi-replica acceptance

Command:

```bash
bash deploy/saas/multi-replica-acceptance.sh
```

The run used Compose project `synara-stage2-multi-89712`, two Control Plane replicas and image
`sha256:6a1c034cdbde237f910c4e0d86593c72f6b0b52dda94e44d9791e1961628b24d`.

Passed checks:

- Cross-replica SSE delivery and Last-Event-ID catch-up.
- Unique Worker Claim behavior across replicas.
- Legal sequential Turn creation and replica failover.
- Migration readiness through `162`.

Both scripts cleaned their temporary containers, volumes and networks after completion; no matching resources remained
when checked after the runs.

## Full Web/Admin Compose smoke

An isolated full-profile smoke was also run with generated throwaway secrets and ports. The run used project
`synara-stage6-ui-21765` and completed with:

```text
ui_compose_acceptance_passed project=synara-stage6-ui-21765 web=200 web_ready=200 admin=200 admin_ready=200
web_branding_present=true
admin_branding_present=true
```

This exercised the built Web and Platform Admin containers against PostgreSQL, MinIO and the Control Plane. The
local HTTP exception was explicit (`SYNARA_ALLOW_INSECURE_REMOTE=true`); production remains fail-closed unless an
HTTPS `SYNARA_PUBLIC_URL` is supplied. The temporary project was removed after the smoke and no matching resources
remained.

The independent Admin Chromium suite also passed 15/15 source-browser checks, including the credential-free Internal
status link, responsive layout, keyboard order, entitlement wording and absence of payment/Billing surfaces. These are
source-browser checks, not the required multi-role production-like operations exercise.

## Deliberate limits

- Development login and local Compose infrastructure were used; no enterprise IdP/MFA, production IAM/KMS, real
  Status Board/on-call delivery or deployed 49-row browser exercise was run.
- The worktree and images were not signed, attested, pushed or bound to a release candidate.
- No production backup restore, 30-day SLO window, long-duration capacity run, independent penetration test, external
  compliance audit or formal GA review was performed.

Passing these runs closes the local internal self-hosted deployment/usage/cost/recovery/HA engineering check only;
external and organizational Stage 6 gates remain open.
