# Compliance control and evidence governance

Status: product workflow implemented; external audit activation pending

## Boundary

Platform Admin → Compliance is the product authority for the Stage 6 compliance readiness record. It stores control and
evidence metadata, not raw customer or production evidence. Raw evidence remains in the access-controlled immutable
repository referenced by the program. Never upload secrets, prompts, Provider payloads or customer content to this record.

`record_complete` means the required metadata and separated decisions exist. It does not mean an auditor has accepted the
scope, the observation period is active, evidence storage is actually immutable, a control operated effectively, or a SOC 2
or ISO 27001 report has been issued. The API and UI expose the permanent assessment
`record-complete-not-audit-active` to preserve that boundary.

## Create the program scope

Only an active Platform Operator Tenant Owner or Admin can create a program. Record the exact framework, scope version,
executive sponsor, contracted auditor reference, observation start/end, evidence repository and access policy, retention,
vendor register and risk register. All references must be credential-free HTTPS URLs without query strings or fragments. Scope is immutable after creation;
corrections require a new program key.

The executive sponsor must be an active Platform Owner or Admin. PostgreSQL and SQLite both reject creation by a weaker or
inactive authority.

## Register controls

Create at least one append-only control in each family:

- logical access;
- change and release;
- operations;
- data governance;
- resilience;
- vendor and Provider governance;
- security testing.

Each control has a stable ID, active Owner/Admin/Security Admin owner, cadence and bounded evidence requirement. Controls are
append-only. A material redesign or ownership correction is represented by a new program rather than rewriting an active
observation record.

## Submit and review evidence

Submit the stable evidence ID, control, type, period, HTTPS repository reference, SHA-256, media type, classification,
collection time and retention deadline. The retention deadline cannot be shorter than the program policy. The database
rejects subject mismatch and all UPDATE/DELETE attempts.

Another active Platform operator records exactly one active immutable `accepted` or `rejected` review. The submitter cannot
review their own evidence. The review reference must bind the exact reviewed external bytes with a non-zero lowercase
`sha256:<64 hex>` digest. `accepted` means the reviewer accepted that bounded byte set and the submitted evidence metadata;
it does not prove object-store authority or any later contents served from the reference.

At least one accepted `release_manifest` is required before record completion. Generate it from the exact candidate using
`scripts/stage6-evidence/collect_release_evidence.py`; retain the manifest, sidecar and CI provenance in the referenced
repository.

## Start-gate decisions

After all controls and initial evidence exist, an Owner/Admin moves the program from `draft` to `ready_for_review`. Four
different operators then record append-only decisions for Security, Operations, Legal/Privacy and Executive. The program
creator cannot decide and one person cannot occupy two roles. Every decision separately binds its exact external decision
evidence bytes with a non-zero lowercase `sha256:<64 hex>` digest.

Migration `000141` preserves URL-only legacy reviews and decisions as superseded history. They are excluded from readiness
and `record_complete`, while partial active uniqueness permits a replacement byte-bound review or role decision. Legacy
rows cannot be edited, revived or used as new authorization evidence.

Evidence review and start-gate decisions additionally require the exact active `compliance.evidence.*` or `compliance.*`
assignment from Platform Admin → Governance roles. Follow
[`stage-6-governance-authority.md`](stage-6-governance-authority.md); request-body role labels are never authority.

An Owner/Admin can move the record to `record_complete` only when:

1. all seven control families exist;
2. all four roles approved with byte-bound decision evidence;
3. an independently accepted release manifest review exists with byte-bound review evidence.

Every create, submission, review, decision and transition writes Platform Audit with the exact request ID. Rejected evidence
or a rejected start-gate decision remains visible and blocks completion; create a new program record after remediation.

## External activation

Before describing the audit as active, independently verify the auditor contract and authority, signed scope and observation
dates, repository immutability/retention/access enforcement, vendor and risk registers, and the first exact-candidate
manifest. External activation, control-operation sampling, exception management and final report issuance remain governed by
the auditor and `docs/plans/stage-6-compliance-evidence-plan.md`.
