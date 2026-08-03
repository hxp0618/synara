# Capacity and long-duration acceptance v1

## Promise boundary

This contract defines the Stage 6 capacity/soak gate. It does not claim a production environment has passed. A source test,
short fixture soak, locally generated receipt, Kubernetes restart loop, or green SLO dashboard alone cannot close the GA
checklist. The release candidate must attach a production or production-like run reviewed by Operations and Engineering.

## Forecast and load

The approved forecast records peak concurrent Sessions, Execution starts/minute, Runtime Event appends/second and SSE
connections. The test load must cover every dimension at the forecast plus at least 20% headroom; lowering one dimension to
make another pass is invalid. Provider/Target mix, Tenant distribution, payload size and interaction mix belong in the
workload-generator evidence and must match the offer being approved.

Continuous minimum duration is 72 hours in production or 24 hours in a production-like environment. Staging requires eight
hours for engineering feedback but cannot close the release gate. The run includes steady peak, burst, Tenant hotspot,
Control Plane rolling disruption/Worker churn, dependency-pressure and cooldown phases without resetting telemetry between
phases.

## Required measurements

The four Stage 6 SLO definitions and minimum volumes come directly from
[`enterprise-service-level-objectives-v1.md`](enterprise-service-level-objectives-v1.md). External Availability probes use
at least three independent Regions, at most 60-second cadence and at least 95% sample coverage.

Capacity headroom additionally requires maximum Control Plane CPU, memory and PostgreSQL connection utilization at or below
80%; Outbox oldest age at or below 300 seconds; queue oldest age at or below 60 seconds; no persistent warm deficit, dead
letter, OOM kill, unexpected restart or failed workload assertion; and worst-Tenant success ratio at least 80% of the best
Tenant success ratio. Planned rolling restarts are exercise evidence, not unexpected restarts.

The evidence bundle contains workload-generator results, Prometheus range output/query revision, external canary data,
database pool/query evidence, Kubernetes resource/restart evidence, redacted application logs and an operator summary. Each
is a distinct regular file with a fixed SHA-256. Secrets and high-cardinality Tenant/User/Session identifiers are forbidden.

## Validator semantics

`scripts/stage6-capacity/prepare_capacity_evidence.py` accepts only path references beneath one evidence root, stable-reads
bounded regular files, scans drafts/evidence for common credential material, derives hashes and validates the fully
materialized manifest before atomically publishing immutable manifest/receipt pairs. The validator rejects schema drift,
duplicate JSON fields, traversal/symlinks, duplicate/tampered or oversized evidence, missing phases/exercises, insufficient
headroom, short duration or missing probe coverage. A measured SLO/saturation miss is preserved as a valid failed run rather
than discarded. Its receipt always says
`evidence-validated-not-capacity-passed`; final acceptance remains a signed release decision.

The receipt retains the exact run start/completion, environment, duration, sample cadence, external probe Regions and
coverage, forecast/load vectors, five uninterrupted phases, required disruption exercises, measurement projections,
manifest/evidence hashes and a derived `eligibleForHumanGateReview` flag. Failed or staging/fixture receipts remain useful
engineering evidence but are ineligible for the release gate.

## Product governance boundary

Migration `000125` stores the exact receipt bytes and SHA-256 against the candidate-bundle `capacity` reference. The
Control Plane independently recomputes the 24/72-hour window, headroom, phase/exercise coverage and measurement objectives;
the candidate creator imports the receipt, and distinct active `capacity.engineering` and `capacity.operations` authorities
must append immutable decisions. Migration `000126` blocks Release Governance approval until the same candidate and
Operator Tenant has an eligible internally approved Capacity run.

This internal state deliberately records `cryptographicSignaturesVerified=false` and
`externalAuthorityVerificationRequired=true`. It does not authenticate the production environment, telemetry authority,
evidence signatures, real execution, Operations/Engineering identities or their external authority. Those remain copied
GA-checklist evidence.
