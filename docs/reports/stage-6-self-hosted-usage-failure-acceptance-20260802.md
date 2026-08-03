# Stage 6 self-hosted usage and failure acceptance — 2026-08-02

## Scope and assessment

This report records one isolated local Compose execution of the internal self-hosted product profile, its usage and
internal-cost projections, and its dependency/Worker recovery drill. It also records the reporting defect found by the
first run and the successful rerun after the fix. This is engineering evidence from a dirty development worktree, not a
clean release-candidate receipt, production exercise, external security review or GA approval.

Assessment: `local-self-hosted-usage-and-recovery-validated`.

## Subject

- Branch: `codex/saas-tenancy-user`
- Observed Git HEAD: `a7ea05fd0f98ae4d15ac5a14a67b02914875ce9f`
- Worktree: dirty; no clean-commit or candidate-artifact claim
- Completion observed at: `2026-08-02T21:15:00+08:00`
- Compose project: `synara-stage2-failure-50910`
- Locally built Control Plane image ID: `sha256:a03857b5e2775c9749bdba4f5e68fa35757fd0339554f0c490db749970e2e521`
- Build network override: `GOPROXY=https://goproxy.cn,direct`; an earlier attempt using the default external Go proxy
  stopped on a TLS timeout and is not counted as an application failure or successful acceptance

## Execution

The isolated run used generated PostgreSQL, MinIO artifact, Worker-registration, Provider-cursor, credential-master and
development-auth authorities. Only the temporary Control Plane test port was published. The executed command was:

```bash
GOPROXY='https://goproxy.cn,direct' deploy/saas/failure-acceptance.sh
```

The base acceptance created a Standard internal Tenant without database intervention, switched to it explicitly, created
an Organization-owned Local Execution Target and ran an Execution through generation recovery. It then exercised the
failure script's separate Tenant and Worker flow.

## Product and usage result

- Platform profile reported `commercializationMode=internal-self-hosted`.
- No `commercialBilling` or legacy external `statusPage` capability was exposed.
- No Checkout, invoice or payment-provider flow was exercised or required.
- Session Usage retained generation 1 after the mutable Execution advanced during recovery:
  - input/cached/output/reasoning/total tokens: `120 / 40 / 60 / 20 / 180`
  - duration: `2500 ms`
  - ingress/egress: `4096 / 2048 bytes`
  - Provider cost: `12345 micros USD`, explicitly reported
- Tenant Usage returned the same historical totals, one reported Provider-cost row, zero missing Provider-cost rows,
  known cost of at least `12345 micros USD`, and `enforcement=soft`, `hardStop=false`.

The first end-to-end run exposed that Tenant Usage joined historical `execution_usage_summaries.generation` to the mutable
current `agent_executions.generation`. Recovery from generation 1 to generation 2 therefore hid valid historical tokens,
network and Provider cost from Tenant and cost-center totals while Session Usage remained correct. Period aggregation and
internal cost allocation now join the immutable summaries to the Execution identity and Tenant, not to its current
generation. A SQLite regression advances the Execution only after valid generation-1 lease/claim history exists, and the
successful PostgreSQL Compose rerun exercises the same recovery scenario.

## Failure and cleanup result

- Worker-offline recovery and generation fencing passed.
- MinIO outage and service recovery passed.
- PostgreSQL outage and service recovery passed.
- Control Plane log scanning found none of the generated database, object-store, Worker, Provider-cursor, credential,
  auth, lease, prompt or Presigned URL secrets.
- The final markers were `Internal self-hosted product, usage and cost acceptance passed`, `Sensitive-log audit passed`
  and `Stage 2 failure acceptance passed`.
- The exit trap removed all project containers, networks and volumes. A post-run Compose-label query returned no container
  or volume for `synara-stage2-failure-50910`; the local test image was intentionally retained in the Docker image cache.

## Deliberate limits

- Development login and a local single-node Compose stack were used; no enterprise IdP or browser role matrix was run.
- The source worktree and image were dirty and were not signed, attested, pushed or bound to a release candidate.
- No production backup restore, cloud IAM/KMS, real internal Status Board delivery, production SLO window, long-duration
  capacity test, independent penetration test or formal GA review was performed.
- Passing this run closes the local self-hosted usage/recovery engineering check only; external and organizational Stage 6
  gates remain open.
