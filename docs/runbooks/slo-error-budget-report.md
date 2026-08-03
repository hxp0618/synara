# SLO and error-budget reporting runbook

Use this runbook with [`enterprise-service-level-objectives-v1.md`](../contracts/enterprise-service-level-objectives-v1.md)
for a Stage 6 candidate. The report is a rolling production measurement, not a synthetic load-test result.

## 1. Freeze identity and window

Record the full release commit, environment identifier, public origin and Prometheus query/rule revision. Production and
production-like release evidence requires one uninterrupted 30-day UTC window. Planned maintenance remains in the internal
measurement. If a customer contract has a separate exclusion, report it separately; do not remove its samples from this
manifest.

Confirm that the public `/ready` origin is monitored from at least three operationally independent Regions at exactly
30-second cadence. The probe service must not depend on Synara login or the primary Synara deployment.

## 2. Export authoritative observations

For the same timestamps, export:

- external probe raw/range data, expected and observed sample counts, and Region inventory;
- the four 30-day Prometheus good ratios, sample counts and error-budget remaining recording rules;
- fast-burn, budget-low and missing-probe alert history;
- queue age, terminal-before-ready, Outbox age, SSE lease and catch-up companion signals;
- incident, maintenance, reliability exception and corrective-action references; and
- release/deployment identity plus the operator summary.

Every route status stays in the API Latency denominator except `/health`, `/ready` and `/metrics`. Availability comes only
from the external probe. Execution Start Delay uses initial-claim Bundle-to-Provider-ready observations. Event Delay is
durable successful Worker append latency, not end-user transcript delivery.

Keep six evidence files distinct. Store them below one access-controlled evidence root and refer to them by relative POSIX
paths from a `synara.slo-window-evidence-draft.v1` draft; the preparer derives their SHA-256 values. Store raw telemetry and
incident detail in an access-controlled evidence location. Do not include credentials, prompts, Provider payloads, customer
content or high-cardinality Tenant/User/Session identifiers in the receipt.

## 3. Review gaps and budget response

No-data intervals, query failure, excluded samples or insufficient volume are `not-assessable`, never passing. Reconcile
every material burn with an incident or maintenance reference, review all incidents, include all maintenance, document every
reliability-freeze exception and assign every open corrective action.

Apply the contract thresholds directly: at 50% remaining add a reliability risk note; at 25% pause nonessential risky
rollout; at zero enter reliability freeze. An exception changes delivery authorization, never the measured ratio.

## 4. Validate and retain

```bash
bun run stage6:slo:prepare -- \
  --evidence-root /secure/slo-window \
  --draft slo-window-draft.json \
  --manifest-output slo-window-manifest.json \
  --receipt-output slo-window-receipt.json
```

A nonzero exit means schema or evidence integrity failed. A zero exit may still contain a missed/not-assessable objective or
`eligibleForHumanGateReview=false`; retain that failed result and keep the GA item open. Verify the receipt sidecar and attach
immutable references to the copied release checklist.

The preparer reads every source as a stable bounded regular file, rejects duplicate JSON fields, Secret-like material,
symlinks and traversal, and publishes the manifest/receipt plus sidecars as exclusive `0600` files. A receipt publication
failure rolls back the manifest pair. Use `stage6:slo:validate` only to independently recheck an already materialized
immutable manifest.

Import the exact receipt JSON and sidecar digest through Platform Admin → SLO windows, selecting the Release candidate whose
commit and environment match the receipt. The candidate creator performs the import. Engineering, Operations, Security and
Product authority holders then review the exact retained projection; each person records at most one immutable functional
decision and the importer cannot self-review. Each decision must bind the exact external approval evidence bytes with its
own non-zero lowercase `sha256:<64 hex>` digest. Reject failed, incomplete or disputed windows rather than deleting them.

Migration `000142` preserves legacy URL-only decisions as superseded history and reopens any SLO Window they previously
approved back to `recorded`. Engineering, Operations, Security and Product must submit replacement byte-bound decisions
before the Window or any forward Release transition can be approved again. Superseded rows remain immutable and do not
occupy the active role/operator uniqueness slots.

Operations verifies probe coverage, alerts, incidents and budget response. Engineering verifies query/deployment identity
and companion signals. Product reviews the customer promise. Security reviews exceptions where relevant. Only the copied
GA checklist and release governance can mark the production SLO gate complete; the receipt always remains
`evidence-validated-not-slo-passed`.
