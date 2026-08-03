# Stage 6 Compliance and Evidence Plan

Status: engineering decision; external audit engagement pending
Decision date: 2026-07-30
Owners: Security, Engineering, Operations, Legal/Privacy

## Decision

Use SOC 2 Type II as the first externally attested enterprise control program. Run an ISO 27001 gap analysis and reuse the
same control/evidence system, but do not open a second certification audit until the SOC 2 scope, owners and observation
period are stable. This prioritizes the procurement artifact most immediately requested by SaaS customers while avoiding
two divergent control systems.

The certification claim remains false until an independent auditor is contracted, scope and criteria are signed, the
observation period has run, exceptions are remediated or accepted, and the final report is issued. Repository tests and
evidence manifests are readiness inputs only.

## Scope baseline

In-scope services: SaaS Control Plane, Web application, remote Worker/Provider Host release path, PostgreSQL, object storage,
KMS, Queue/Outbox, CI/CD and production operations. In-scope people and processes: Engineering, Operations, Security,
Support Access, joiner/mover/leaver, incident response, vendor management and release approval. Local Personal deployments
are product dependencies where their code is shared, but are not a production service boundary.

Initial Trust Services Criteria: Security (required), Availability and Confidentiality. Privacy is mapped through the DPA,
DSAR, retention, Legal Hold and subprocessor controls; add the formal Privacy category only after counsel/auditor confirms
scope and evidence readiness.

## Control-to-evidence system

| Control family   | Synara authority/evidence                                                                                  | Remaining external/process evidence                                          |
| ---------------- | ---------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------- |
| Logical access   | RBAC matrix, SSO/SCIM, Service Accounts, joiner/mover/leaver Audit, controlled Support Access              | Quarterly access review and HR identity source sign-off                      |
| Change/release   | Protected Git review, migration lineage, image SBOM/signature/scan, release checklist and artifact digests | Branch protection export, reviewer approval and production deployment record |
| Operations       | Structured logs, Metrics, queue/worker/credential/identity views, incident runbooks                        | On-call schedule, alert acknowledgements, internal Status Board exercise     |
| Data governance  | Audit Export, Retention, Legal Hold, DSAR/erasure, immutable Tenant export receipt                         | DPA/subprocessor register, Legal approvals and customer notices              |
| Resilience       | Backup/restore assets, DR routing/readiness, recovery bundles                                              | Production PostgreSQL/S3/KMS/Queue restore reports and measured RPO/RTO      |
| Vendor/Provider  | `provider-commercial-use-v1`, credential isolation and local-only/experimental gates                       | Executed terms, DPA/order forms, periodic vendor risk review                 |
| Security testing | Stage 5 isolation matrix, secret sentinel checks, supply-chain gates                                       | Independent penetration test and accepted-risk register                      |

## Automated collection

`scripts/stage6-evidence/collect_release_evidence.py` fails closed on a dirty source tree, mutable/non-file evidence path,
invalid digest or missing migration history. It writes a versioned JSON manifest and SHA-256 sidecar containing:

- exact 40-character Git commit and `bun.lock` SHA-256;
- Control Plane, Worker, Provider Host, Web and Platform Admin artifact digests;
- macOS arm64/x64, Windows x64 and Linux x64 Desktop artifact digests;
- environment class, deployment-unique environment ID, Control Plane/Web/Admin HTTPS origins and Region list;
- every SQL migration checksum and the migration tail;
- explicit control ID → evidence file path/hash mappings.

The manifest assessment is always `evidence-collected-not-control-passed`. Store the manifest, sidecar, CI provenance and
human approvals in access-controlled immutable storage. Never collect raw secrets, Provider payloads, customer prompts or
customer content.

Platform Admin → Compliance and Migration `000112` provide the product-authoritative metadata register around that storage:
immutable program scope, named control owners across all seven families, append-only evidence reference/digest/retention,
independent evidence review and four distinct start-gate decisions. Its strongest assessment is
`record-complete-not-audit-active`; it cannot validate external auditor authority or the repository behind an HTTPS reference.
Operations follow `docs/runbooks/compliance-control-evidence-governance.md`.

After individual external validators produce receipts, `scripts/stage6-candidate/validate_candidate_evidence_bundle.py`
binds Billing, Desktop, Operations, Incident, Recovery, Residency, SLO, Capacity and Penetration evidence to that same release
manifest. Its `evidence-consistent-not-ga-approved` receipt prevents cross-candidate evidence splicing; it is not an audit
opinion or a final release decision.

## Execution phases

1. Scope/readiness: name control owners, contract auditor, approve systems/criteria/subprocessors and close all P0 gaps.
2. Design/Type I readiness: document policies, sample each control once, remediate missing ownership and obtain auditor
   readiness feedback. A Type I report is optional; it must not delay a properly instrumented Type II observation period.
3. Type II observation: operate controls for the auditor-agreed period, collect release/access/incident/restore/vendor
   samples continuously and review exceptions monthly.
4. Audit/report: supply hashed evidence manifests and immutable source artifacts, answer samples, remediate findings and
   issue the report through a controlled trust portal.
5. ISO 27001: convert the stable control inventory into ISMS scope, risk register, Statement of Applicability, internal
   audit and management review; then schedule certification without duplicating operational controls.

## Start gate

The path becomes “active” only when all of the following exist: named executive sponsor and control owners, approved scope,
contracted independent auditor, signed observation start/end dates, evidence repository retention/access policy, vendor
register, risk register and first successful automated evidence manifest from an exact release candidate. Until then the
Stage 6 GA checklist and TODO item remain unchecked.
