# Stage 6 self-hosted usage, cost and failure acceptance — 2026-08-03

## Scope and assessment

This report records one isolated local Compose execution of the internal self-hosted product profile, including
Token/Network/Provider-cost projections, project cost-center allocation and dependency/Worker recovery. It is
engineering evidence from a dirty development worktree, not a clean release-candidate receipt, production exercise,
external security review or GA approval.

Assessment: `local-self-hosted-usage-cost-and-recovery-validated`.

## Subject

- Branch: `codex/saas-tenancy-user`
- Observed Git HEAD: `a7ea05fd0f98ae4d15ac5a14a67b02914875ce9f`
- Worktree: dirty; no clean-commit or candidate-artifact claim
- Compose project: `synara-stage2-failure-56282`
- Locally built Control Plane image: `sha256:ae4c174132e94e46285b12c435ff059c1e865768008a4ff36edefd3905e7e720`

## Execution

The isolated run used generated PostgreSQL, MinIO artifact, Worker-registration, Provider-cursor, credential-master
and development-auth authorities. Only the temporary Control Plane test port was published. The executed command was:

```bash
SYNARA_FAILURE_RUN_BASE_ACCEPTANCE=1 bash deploy/saas/failure-acceptance.sh
```

The base acceptance created a Standard internal Tenant without database intervention, selected it explicitly, created
an Organization-owned Local Execution Target and exercised generation recovery. It also queried the internal cost
report, assigned the project with version `0` using a CAS update, re-read the report and downloaded the CSV export.

## Product and usage result

- Platform profile reported `commercializationMode=internal-self-hosted` and the acceptance rejected payment-provider,
  Checkout, Stripe, Billing and public Status Page capability fields.
- Session/Tenant usage retained the recovered historical generation:
  - input/cached/output/reasoning/total tokens: `120 / 40 / 60 / 20 / 180`
  - ingress/egress: `4096 / 2048 bytes`
  - Provider cost: `12345 micros USD`, explicitly reported
  - soft quota: `enforcement=soft`, `hardStop=false`
- Cost accounting initially exposed the project as unallocated, then accepted the versioned assignment
  `eng-platform` / `internal-ai` at version `0 -> 1`.
- The post-assignment report showed zero unallocated projects while preserving the same Token and Provider-cost
  totals. The CSV contained `cost_center_code` and `department_code` and no `checkout`, `stripe` or `payment` terms.

## Failure and cleanup result

- Worker-offline recovery and generation fencing passed.
- MinIO outage and service recovery passed.
- PostgreSQL outage and service recovery passed.
- Sensitive-log audit passed.
- The final markers were `Internal self-hosted product, usage and cost acceptance passed` and
  `Stage 2 failure acceptance passed`.
- A post-run Docker query returned no container, volume or network matching `synara-stage2-failure-56282`.

## Deliberate limits

- Development login and a local single-node Compose stack were used; no enterprise IdP, production IAM/KMS,
  real Status Board/on-call delivery or browser role matrix was run.
- The source worktree and image were dirty and were not signed, attested, pushed or bound to a release candidate.
- No production backup restore, production SLO window, long-duration capacity test, independent penetration test,
  external compliance audit or formal GA review was performed.
- Passing this run closes the local internal self-hosted usage/cost/recovery engineering check only; external and
  organizational Stage 6 gates remain open.
