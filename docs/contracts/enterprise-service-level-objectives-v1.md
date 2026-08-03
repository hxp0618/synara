# Enterprise service-level objectives v1

## Status and promise boundary

This contract freezes the Stage 6 SLI definitions, provisional objectives, error-budget math, and evidence required for
an Enterprise internal self-hosted deployment. It does not assert that any production environment has met the objectives. A release may claim
an SLO only after the copied Stage 6 release checklist links a complete rolling-window report from the exact production
deployment.

The default compliance window is a rolling 30 days. Planned maintenance is not silently removed from the calculation.
Any narrower customer contract must name its exclusions explicitly and cannot weaken the internal measurements below.

## SLI catalog

| SLO                   | Good event                                                                                                            | Objective | Required volume and authority                                                                                                                                                             |
| --------------------- | --------------------------------------------------------------------------------------------------------------------- | --------: | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Availability          | An external probe to the public `GET /ready` origin succeeds.                                                         |     99.9% | At least three independent probe regions, 30-second cadence, and at least 95% of expected samples. `up` and in-process request counters are diagnostic only.                              |
| API Latency           | An eligible HTTP request completes in at most 2.5 seconds.                                                            |     99.0% | At least 10,000 eligible requests. The bounded `synara_http_request_duration_seconds` histogram is authoritative.                                                                         |
| Execution Start Delay | An initial Generation reaches Provider `session.started` within 60 seconds of its immutable Recovery Bundle creation. |     99.0% | At least 100 initial-claim Generations. `synara_execution_generation_provider_ready_duration_seconds` is authoritative. Pending and terminal-before-ready outcomes must also be reviewed. |
| Event Delay           | A successful authenticated Worker Runtime Event append commits within 1 second.                                       |     99.9% | At least 1,000 successful appends. `synara_session_event_append_duration_seconds` is authoritative.                                                                                       |

Eligible API traffic is every registered Control Plane route except `/health`, `/ready`, and `/metrics`. All response
statuses remain in the latency denominator. Availability is not inferred from this traffic: a dead endpoint cannot count
its own failed requests, so only an independently operated external probe can close that SLO.

Execution Start Delay measures Bundle creation to Provider readiness. It therefore includes Worker claim, Workspace
materialization, and Provider startup, but it does not include time before the Generation's authoritative Bundle exists.
Queue age and terminal-before-ready outcomes are mandatory companion signals. The interactive Warm Pool objective remains
the stricter Stage 4 dispatch-to-ready P95 of 2.5 seconds for warm hits; it is a product performance objective, not a
substitute for the fleet-wide Stage 6 SLO.

Event Delay currently measures durable server ingestion. End-user transcript delivery has no client acknowledgement
authority, so it must not be described as an end-to-end delivery SLO. SSE catch-up P95, expired connection leases, and
Outbox age remain companion diagnostics until a versioned client-delivery receipt exists.

## Error budgets

For a target `T` and observed good-event ratio `G`:

```text
allowed bad ratio       = 1 - T
consumed budget ratio   = (1 - G) / (1 - T)
remaining budget ratio  = clamp(1 - consumed budget ratio, 0, 1)
burn rate               = observed bad ratio / allowed bad ratio
```

No-data and insufficient-volume windows are `not-assessable`, never passing. Missing external probe data pages the
operator separately and blocks an Availability claim.

The checked-in Prometheus rules record the 30-day good ratio, sample count, and remaining budget for each SLO. They also
page on the standard fast-burn shape for Availability and API Latency: both the five-minute and one-hour burn rates must
exceed `14.4`. The page is a rapid-response symptom, not a complete 30-day compliance decision.

Budget policy:

- `remaining > 50%`: normal delivery, while every SLO alert is still investigated.
- `25% < remaining <= 50%`: require an explicit reliability-risk note in release approval.
- `0% < remaining <= 25%`: stop nonessential risky rollout and assign a recovery owner.
- `remaining = 0%`: reliability freeze for the affected service surface until the incident commander and service owner
  approve recovery work or a documented security/legal emergency exception.

Security fixes may proceed during a freeze with a rollback owner and explicit risk record. An exception changes the
delivery decision, not the measured SLI or consumed budget.

## Labels, privacy, and Tenant boundaries

SLO series aggregate only bounded route, status, recovery-reason, result, and Target-kind labels already permitted by the
observability contract. Tenant, User, Session, Execution, Request, Trace, Artifact, Credential, or Provider payload values
must never become metric labels. Per-Tenant contractual reporting must be built from access-controlled audit/usage data,
not by adding Tenant IDs to Prometheus.

## Release evidence

An approver must retain:

1. the exact Prometheus query or dashboard revision and query timestamps;
2. raw 30-day SLI ratios, sample counts, budget remaining, and any no-data intervals;
3. external probe locations and the public origin tested;
4. incident and maintenance references for every material budget burn;
5. the release commit and deployment artifact digests; and
6. Security, Operations, Product, and Engineering approval or a recorded rejection.

The report must distinguish `objective-defined`, `measurement-configured`, and `production-window-passed`. Checked-in
rules establish only the first state until a deployment supplies real telemetry.

## Evidence validator

`scripts/stage6-slo/prepare_slo_evidence.py` materializes a path-only draft and atomically publishes the immutable manifest
and receipt; `validate_slo_evidence.py` independently rechecks a materialized manifest. Together they fix one 30-day
production/production-like window, the public origin,
three-or-more 30-second external probe Regions, query revision, all four ratios/sample counts/error budgets, alert history,
mandatory companion-signal review and six distinct evidence hashes. It recomputes the error-budget formula, rejects an
understated probe denominator and treats missing data, excluded samples or insufficient volume as `not-assessable`.

An objective miss remains a valid failed-window receipt rather than a discarded run. The receipt always says
`evidence-validated-not-slo-passed`; `eligibleForHumanGateReview=true` is necessary evidence readiness, not a production
SLO claim or release approval. Collection and review steps are in `docs/runbooks/slo-error-budget-report.md`.

The preparer derives hashes from stable bounded reads and rejects duplicate JSON fields, reused files, symlinks, traversal
and common credential material. These integrity controls do not prove the telemetry source, probe independence or operator
identity; authorized reviewers still verify those external authorities.

## Product governance record

Migration `000118_stage6_slo_window_governance.sql` and Platform Admin → SLO windows accept the exact bounded validator
receipt only when its SHA-256, Release candidate commit/environment, uninterrupted window, public probe authority, four
objective projections and eligibility recompute consistently. Failed and not-assessable receipts remain retained; they
cannot be approved. The importer cannot review their own record, one operator cannot decide multiple roles, and an eligible
window reaches internal `approved` only after distinct Engineering, Operations, Security and Product functional authorities
record immutable approvals. These product and database controls make review auditable; they do not turn the validator's
permanent non-passing assessment into a customer or production SLO claim.
