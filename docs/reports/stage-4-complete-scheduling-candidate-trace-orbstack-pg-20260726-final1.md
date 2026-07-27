# Stage 4 complete scheduling candidate trace — OrbStack PostgreSQL final1

Date: 2026-07-26
Repository branch: `codex/saas-tenancy-user`
HEAD observed at final verification: `3ad15523bd74bd1b21f73a43184e2f4fef99039a`
Evidence level: **E3 local real-Kubernetes/PostgreSQL evidence**
Kubernetes context: explicit `--context orbstack`
Kubernetes node: single Ready control-plane node, `v1.34.8+orb1`

The shared branch advanced concurrently during this work. This agent did not stage, commit, merge, or push. The report
covers only the complete scheduling-candidate trace increment and its isolated acceptance resources.

## Scope

Successful Target Group scheduling now persists one `complete` immutable Decision containing every active Member from
the stable first-pass candidate set:

- route-eligible losers retain Health, queue pressure, Member weights, Region/Health rank, and total route rank;
- request, scope, Target kind/status, Scheduling Policy, Health/capacity, Region, outage, and DR filters retain stable
  rejection codes;
- repeated placement/capability retries merge into the same Member-ID-ordered trace, preserving the coordinator rejection
  instead of overwriting it with the next pass's synthetic `request-excluded` result;
- a successful preview retained before coordinator rejection freezes its Pool ID/version, Capacity Class, and placement
  policy version;
- the final winner is refreshed from post-lock routing authority and final Pool selection;
- fixed-Target launches remain honest one-row `selected-only`, and legacy backfill remains `legacy-selected-only`;
- more than 4096 active Group Members fail closed before launch; and
- completely failed or stale-commit transactions remain mutation-free.

Every Candidate and the ordered candidate set retain the existing canonical SHA-256 coverage. No encrypted Target
configuration, capability map, scheduling template, policy body, free-form Provider error, or publisher reason enters the
evidence graph.

## Source identity

| Source | SHA-256 |
| --- | --- |
| `internal/routing/service.go` | `d5ee1237abfc35594f95a10859cc15f20d38b3e442240f5ee934f3db32114c8d` |
| `internal/sessions/execution_launch_target.go` | `27e2aa2f5f89a5bf3eb5db66b6665b382551b905086ed98610cd4dc6bc822b21` |
| `internal/sessions/scheduled_execution.go` | `6c128254c676c8c7cf6f257d9ca4f59616a394173de882b2b448f15a3c2e99f6` |
| `internal/schedulingdecision/schedulingdecision.go` | `9ff9b5c701821a26ec0af9f479f38684ecd76b4dd457dc3ee60489216c3edb5e` |
| `internal/schedulingdecision/schedulingdecision_postgres_integration_test.go` | `11af972ee85d214ed30567988d710bf202817d3cf763c704560b58d9b9a09f5a` |

## Package and SQLite result

The focused package run passed:

```text
go test ./internal/routing ./internal/schedulingdecision ./internal/sessions -count=1
```

It covers deterministic Member-ID order; eligible-loser ranks; policy, missing-Health, request-excluded, capability, and
DR-failover rejection paths; exact selected authority; selected/rejected Worker Pool evidence; fixed-Target compatibility;
and canonical candidate/set digest reload verification.

## OrbStack PostgreSQL result

Owned namespace: `synara-scheduling-trace-20260726` (deleted after acceptance).
Workload: PostgreSQL 16 Pod and ClusterIP Service.
Local port-forward: `55486` (terminated after acceptance).

Command under test:

```text
SYNARA_TEST_DATABASE_URL=<ephemeral OrbStack PostgreSQL URL> \
  go test ./internal/schedulingdecision \
  -run '^TestPostgresPersistsCompleteSchedulingCandidateTrace$' \
  -count=1 -v
```

Final result: PASS. Pre-acceptance fixture runs exposed required PostgreSQL `NOT NULL` and routed-snapshot constraints;
the fixture was corrected and only the final clean run is counted.

The real PostgreSQL database reported:

```text
schema: 85 | agent_executions_claim_indexes
decision: complete | queue-pressure-v1 | candidateCount=2 | selectedOrdinal=1 | setDigestLength=64
candidate 0: rejected | health-missing | selected=false | digestLength=64
candidate 1: eligible | rejection=- | selected=true | digestLength=64
```

A direct update of candidate 0 from `health-missing` to `health-expired` was rejected by the PostgreSQL immutable evidence
trigger. The successful graph therefore proved deferred Execution/Decision ownership, multi-candidate count/ordinal and
single-selection validation, canonical per-candidate/set digests, and mutation fencing on the real dialect.

## Cleanup and evidence boundary

The owned namespace, PostgreSQL Pod/Service/database, and port-forward were removed. Namespace absence and absence of a
listener on port `55486` were verified.

This is E3. It proves the local real-Kubernetes-hosted PostgreSQL migration and trigger path, not a managed-cloud database,
independent Region/AZ failure domains, multi-cluster scheduling, long-duration soak, or production IAM. Those remain
separate Stage 4 E4 gates.

`bun fmt`, `bun lint`, and `bun typecheck` were not run because the user did not request the heavyweight Bun checks.
