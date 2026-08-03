# Enterprise incident response and internal communication

## Readiness gate

This runbook defines the Stage 6 operating model. A deployment is not on-call ready until its private operations annex
names the primary and secondary responders, paging service, Security/Privacy contacts, internal Status Board URL, employee
notification channel, and executive escalation path. Do not commit personal phone numbers, private channel URLs, access
tokens, or paging credentials to this repository.

## Severity and response targets

| Severity | Typical condition                                                                                                         |           Acknowledge |                                             Internal update |
| -------- | ------------------------------------------------------------------------------------------------------------------------- | --------------------: | ----------------------------------------------------------: |
| SEV-0    | Confirmed broad compromise, destructive cross-Tenant access, or unrecoverable multi-region data loss.                     |                 5 min | 15 min after confirmation, coordinated with Security/Legal. |
| SEV-1    | Internal service unavailable, sustained fast SLO burn, widespread execution failure, or credible Tenant isolation breach. |                 5 min |                                  15 min after confirmation. |
| SEV-2    | Material degradation, limited-region failure, delayed jobs, or an SLO budget at or below 25% without broad outage.        |                15 min |                30 min when internal user impact is visible. |
| SEV-3    | Low-impact defect, warning threshold, or operational issue with a safe workaround.                                        | Next staffed rotation |                          Normally no Status Board incident. |

The incident commander may raise severity immediately. Lowering severity requires the previous severity's stabilization
evidence and an incident-log entry; it never rewrites the historical timeline.

## Roles

- Incident commander owns severity, priorities, role assignment, and the final recovery decision.
- Operations lead diagnoses and mitigates using audited operator paths and the applicable component runbook.
- Communications lead publishes employee-safe updates and keeps the operational/restricted timelines consistent.
- Scribe records UTC timestamps, decisions, evidence links, commands at a safe abstraction level, and owners.
- Security/Privacy lead is mandatory for suspected isolation, credential, personal-data, or supply-chain incidents.

The governed record always separates the incident commander and communications lead. A declared Security/Privacy impact
also requires a Security/Privacy lead separated from the commander. Other operational roles may be combined during a small
incident, but the incident commander must not be the sole approver for destructive recovery, Support Access, key rotation,
or a security-impacting broad internal statement.

## Response flow

1. Acknowledge the page, open the Platform Admin governed incident record, assign severity and separated roles, and record
   the first observed internal user impact. Broad-impact incidents fail closed unless a failure-independent HTTPS internal
   Status Board origin is configured.
2. Preserve evidence before mutation. Capture dashboards, trace IDs, bounded logs, Audit references, deployment digests,
   migration tail, and current routing/backup state. Never paste Secret values, prompts, presigned URLs, or raw personal
   data into the incident channel.
3. Contain with the narrowest reversible authority: stop rollout, drain/disable a Target, revoke a Credential, suspend a
   Tenant, or activate the documented failover path. Direct database mutation is not an incident API.
4. Create the internal Status Board incident, bind its opaque reference once in Incident Governance, publish the first internal
   update by the table above, then have the assigned communications lead record the same-origin update URL. State observed
   impact, affected components/regions, start time, mitigation status, and the next update time. Do not speculate about root
   cause or expose Tenant identity.
5. Update at least every 30 minutes for SEV-0/1 and every 60 minutes for broad-impact SEV-2 incidents, even when there is no
   material change.
6. Validate recovery through internal probes and internal-user-visible behavior in addition to service, database, queue, object,
   KMS, Worker, and Audit evidence. Watch at least one normal alert window before resolving.
7. Resolve the Status Board incident with impact and duration and record its immutable internal evidence URL. The commander may
   transition the governed record to resolved only after that evidence exists; a Security/Privacy incident additionally requires
   the assigned independent lead's immutable approved decision bound to the exact reviewed containment and recovery evidence
   bytes with a nonzero lowercase SHA-256. Keep root-cause language provisional until review.

## Internal Status Board contract

The internal board must expose Control Plane/API, authentication/SSO, execution scheduling, Worker runtime, Artifact service,
and web application as separate components, with Region detail where the installation makes a Region promise. It must retain
an employee-visible incident history and provide an internal notification channel. The Status Board must be
failure-independent from the primary Synara deployment; access remains governed by the enterprise operating environment.

Internal posts use UTC timestamps and one incident identifier. A Tenant-specific incident stays Tenant-restricted unless it
indicates systemic risk, but its restricted timeline follows the same update cadence. Security incidents may delay
technical detail on Legal/Security advice; they do not justify hiding confirmed service impact.

The Control Plane accepts an independent `SYNARA_INTERNAL_STATUS_BOARD_URL`, projects only the sanitized URL and configured
state through `GET /v1/platform/profile`, and exposes the link in Web/Admin. No live Status Board URL or completed exercise
evidence is checked into this repository. The GA checklist remains open until a deployed internal board exists and a communication
exercise proves that paging, role assignment, posting, employee notification delivery, and resolution work end to end.

Migration `000117_stage6_incident_governance.sql` retains legacy column names but the active API exposes only employee-safe
incident identity, internal component/Region scope, role assignments, cadence targets, opaque board reference, same-origin
evidence URLs and the independent
resolution decision. The origin is immutable from incident creation, internal updates and decisions are append-only, and direct
database mutation cannot bypass role, ordering, cadence or resolution gates. This record is an internal evidence authority;
it does not call the Status Board provider and does not prove that a post or employee notification delivery occurred.

After a valid employee-safe internal update commits, the same transaction appends an `incident.internal-update` intent to
the PostgreSQL Outbox. The intent contains only sanitized Incident identity, summary, component/Region scope and
credential-free Status Board references. Outbox persistence means the notification can be claimed, retried and audited;
it is not delivery evidence. The configured Publisher remains responsible for the failure-independent Status Board or
employee channel and must use the Outbox Message ID as its idempotency key. Configure
`SYNARA_INTERNAL_INCIDENT_PUBLISHER_URL`, `SYNARA_INTERNAL_INCIDENT_PUBLISHER_HMAC_KEY`, and the Status Board URL
together. The endpoint must verify the `v1` HMAC over the exact request body before parsing it, deduplicate by
`Idempotency-Key`, and return 2xx only after it has durably accepted responsibility for publication. The Control Plane
does not follow redirects and no longer acknowledges this Topic through the database-only publisher.

Migration `000149_stage6_incident_resolution_approval_evidence_digest.sql` supersedes historical URL-only Security/Privacy
decisions without rewriting already terminal incident history. A still-monitoring incident must receive a replacement active
byte-bound approval before it can become `resolved`; PostgreSQL and SQLite reject direct lifecycle bypass, zero digests and
mutating an approval digest after insert.

## Error-budget response

Use the thresholds in `docs/contracts/enterprise-service-level-objectives-v1.md`. At 50% remaining, add reliability risk
to release review. At 25%, pause nonessential risky rollout. At exhaustion, enter a reliability freeze for the affected
surface. Every exception records approver, reason, rollback owner, expiry, and affected SLO; an exception never edits the
measured budget.

## Review and exercises

- Hold a quarterly paging and internal-communication exercise and a semiannual restore/region-failover exercise.
- Produce an internal review within five business days for every SEV-0/1 and any repeated SEV-2.
- Publish an employee-safe review when impact was broad or internal policy requires it.
- Track corrective actions with owner, due date, verification evidence, and accepted residual risk.
- Review the runbook, contact annex, Status Board components, and templates after every exercise or material incident.

An exercise report proves only the exercised path and environment. A tabletop does not replace a real restore or employee
notification-delivery test.

## Exercise evidence validator

`scripts/stage6-incident/validate_incident_exercise_evidence.py` fixes one candidate and environment to an independent
Status Board, named role references, primary/secondary paging, SEV acknowledgement/internal-update cadence, all six internal
service components, employee notification delivery, internal-probe/user-path recovery observation and seven distinct evidence hashes. A
production or production-like `live-internal-communication` exercise is release-eligible; tabletop, staging and fixture runs
remain engineering evidence.

Missed acknowledgement, publication cadence, employee notification delivery, recovery observation or approval is preserved in a
failed receipt. The receipt always says `evidence-validated-not-operations-ready`; `eligibleForHumanGateReview=true` does
not replace the private rota/contact annex, deployed board configuration or Operations/Communications release approval.

## Platform Incident exercise governance

After the validator produces the final receipt, import its exact bytes through Platform Admin → Incident exercises. The
Control Plane accepts at most 512 KiB, verifies the SHA-256 recorded by the exact candidate, rejects extra top-level fields,
and recomputes candidate/Commit/environment binding, independent HTTPS Status Board origins, role separation, severity and
paging targets, all six internal service components, internal timeline order/cadence, employee notification delivery, recovery observation, review
flags, the manifest and seven distinct evidence references. Receipt replacement and cross-candidate reuse are forbidden.

The candidate creator cannot import the exercise. Two different users holding active
`incident_exercise.operations` and `incident_exercise.communications` authority must append immutable approvals before the
record becomes internally `approved`. Each approval must include the nonzero lowercase `sha256:<64-hex>` digest of the exact
externally retained evidence bytes reviewed; a protected URL without that byte binding is not active. Migration
`000127_stage6_incident_exercise_governance.sql` enforces that projection
and approval boundary in PostgreSQL; SQLite applies the same guards. Migration
`000128_stage6_release_incident_exercise_approval_gate.sql` prevents the exact Release candidate from entering `approved`
without that exact internally approved record and blocks direct database bypass.

Migration `000146_stage6_incident_exercise_approval_evidence_digests.sql` supersedes historical URL-only decisions and
reopens their previously approved Exercise for replacement review. Operations and Communications must submit new
byte-bound decisions; every forward Release state is revalidated fail-closed against both active digests.

This is an internal evidence/decision authority only. It does not call or authenticate the Status Board or paging provider,
prove employee notification delivery, verify receipt signatures, prove the exercise ran in the declared environment, or validate the
approvers' organizational authority. Keep the checklist open until those deployment facts are independently reviewed.

After the exercise, create a `synara.incident-communication-exercise-evidence-draft.v2` draft containing relative paths
for the seven evidence fields. Do not hand-enter hashes. Publish the protected evidence bundle with:

```bash
bun run stage6:incident:prepare -- \
  --evidence-root /secure/incident-exercise \
  --draft incident-draft.json \
  --manifest-output incident-manifest.json \
  --receipt-output incident-receipt.json
```

The preparer rejects unstable, oversized, symlinked, duplicate or obvious credential-bearing input and publishes nothing
until the full semantic validator succeeds. It does not authenticate the paging provider, Status Board, notification
delivery, human roles or organizational authority.
