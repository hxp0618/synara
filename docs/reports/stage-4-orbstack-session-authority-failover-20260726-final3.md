# Stage 4 OrbStack Session authority failover — final3

Date: 2026-07-26
Repository branch: `codex/saas-tenancy-user`
HEAD observed after runtime acceptance: `e7514b802ac9ed007dc8466defb3b166791fe255`
Evidence class: **E3 local real-Kubernetes/PostgreSQL**
Kubernetes context: explicit `--context orbstack` (`v1.34.8+orb1`)
Owned namespace: `synara-stage4-session-authority-3`

The shared branch advanced concurrently during the run. This agent did not stage, commit, merge, or push. This report
covers only the additive Session-authority check in the Kubernetes resilience runner and its isolated acceptance run.

## Result

The schema-85 Control Plane image passed the existing Stage 2 baseline and a top-level two-replica Control Plane Pod
failover while retaining one byte-identical authoritative Session row:

```text
baseline: passed | duration=122s | migrations=85
control-plane-failover: passed | duration=11s | readiness failures=0
Session authority: stage2-postgres | verified=true | rowCount=1
beforeDigest == afterDigest == 15497b7480cbf1f2d8536c3457c56b3b8e2357111da9fb8d3ab9040f794cbb13
```

The runner deleted `synara-control-plane-fdd755f75-lt4jz`; the second replica remained Ready, and Kubernetes created
replacement `synara-control-plane-fdd755f75-4gz7k`. The report retains only a SHA-256 of the sentinel Session ID, not the
raw Tenant, Organization, User, Project, Session, or Target identity.

## Authority gate

With baseline bootstrap enabled, `SYNARA_K8S_RESILIENCE_SESSION_AUTHORITY_MODE` now defaults to `stage2-postgres`.
Immediately before disruption the runner uses one constrained PostgreSQL statement to create an isolated acceptance
User, Tenant, memberships, root Organization, Project, and fixed-Target Session. It hashes the complete
`to_jsonb(agent_sessions row)` value with SHA-256. After the replacement Control Plane is Ready, the runner requires:

- the exact Session ID to resolve to one row;
- a fresh digest over every persisted Session column; and
- exact equality with the pre-failover digest.

The write path is safe under an unknown `kubectl exec` result: the creation statement is submitted only once. If its
transport result is ambiguous, the runner performs a read-only reconciliation by the exact Session ID and accepts only
one row with a recomputable digest; it never blindly replays the User/Tenant/Session write graph.

Runs with baseline bootstrap disabled default this local adapter to `disabled`. Managed/external databases must supply
their own production authority check; this local PostgreSQL proof cannot be reclassified as managed-cloud evidence.

## Negative iterations retained

The failed reports are intentionally retained as bounded negative evidence:

- `final1`: the first sentinel assumed enterprise bootstrap already contained a Tenant domain. Enterprise bootstrap owns
  only the platform Target, so the producer returned no Session row and failed closed.
- `final2`: the self-contained domain write exposed an ambiguous `kubectl exec` result. Blind generic retry replayed the
  same User identity and PostgreSQL rejected the duplicate primary key.
- `final3`: single-submit plus read-only reconciliation passed.

Neither failed run was counted as acceptance success. Each used a fresh owned namespace and was cleaned before the next
run.

## Baseline evidence

The final run also reconfirmed:

- two Ready Control Plane replicas on migration 85;
- baseline Control Plane Pod deletion with zero readiness failures;
- Worker token validity after Pod replacement;
- PostgreSQL outage and readiness recovery;
- MinIO outage and readiness recovery;
- sensitive-log audit; and
- least-privilege RBAC allow/deny checks.

The image was built from the current control-plane worktree:

```text
synara-control-plane:stage4-session-authority-20260726
sha256:3db485ea86d0701580ed6f3a3ca599cd62b05844258301fc582095449ab13f95
```

`proxy.golang.org` timed out twice during dependency download. The successful, otherwise identical build used
`GOPROXY=https://goproxy.cn,direct`.

## Command

```bash
SYNARA_K8S_CONTEXT=orbstack \
SYNARA_K8S_NAMESPACE=synara-stage4-session-authority-3 \
SYNARA_K8S_ACCEPTANCE_RBAC_NAME=synara-control-plane-reconciler-synara-stage4-session-authority-3 \
SYNARA_K8S_ACCEPTANCE_OWNER=stage4-session-authority-final3 \
SYNARA_K8S_ACCEPTANCE_IMAGE=synara-control-plane:stage4-session-authority-20260726 \
SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE=1 \
SYNARA_K8S_RESILIENCE_SESSION_AUTHORITY_MODE=stage2-postgres \
SYNARA_K8S_RESILIENCE_CASES=control-plane-failover \
SYNARA_K8S_RESILIENCE_DISRUPTION_WINDOW_SECONDS=10 \
SYNARA_K8S_RESILIENCE_PROBE_INTERVAL_SECONDS=1 \
SYNARA_K8S_RESILIENCE_EVIDENCE_FILE=docs/reports/stage-4-orbstack-session-authority-failover-20260726-final3.json \
bash deploy/kubernetes/resilience-acceptance.sh
```

Final local asset verification passed:

```text
bash -n deploy/kubernetes/acceptance.sh deploy/kubernetes/resilience-acceptance.sh
bash deploy/kubernetes/validate-resilience-assets.sh
Ran 24 tests — OK
Python resilience validation passed
Kubernetes resilience assets validation passed
```

The Leader watchdog fixture retains its internal four-second case deadline. Its outer Python subprocess allowance was
raised from 10 to 20 seconds only to leave cleanup/report-emission headroom on a loaded workstation; two consecutive
10-second outer timeouts motivated the change, and the final full negative matrix passed.

## Evidence hashes

| Artifact | SHA-256 |
| --- | --- |
| `deploy/kubernetes/resilience-acceptance.sh` | `8f8003f3a2c11d0c6ee3e2cbf43709f53eb409e9c189db1d38818e26cb6d6e37` |
| `deploy/kubernetes/validate-resilience-assets.py` | `d0c6d225a3831ec3721dcd3ad0a9aea6bf9d3b5b1e81200b07760a47d4ee9339` |
| `stage-4-orbstack-session-authority-failover-20260726-final1.json` | `526d61453555c562b9073546140a2865a3a4eb9f274466a70a2efa73e11fe059` |
| `stage-4-orbstack-session-authority-failover-20260726-final2.json` | `8a7a9cf37c2250328cc784b91acd6570dccc0c527d5cb3f053e6c9db0a19760b` |
| `stage-4-orbstack-session-authority-failover-20260726-final3.json` | `670dcbca44be01d6b063728473e71a404ecbbb6057358c3797162d2172da56c0` |
| `stage-4-orbstack-session-authority-failover-20260726-final3.json.journal.jsonl` | `5da7c2d006850d03dad17a2939caccab269bc57625e3722b8d19d8a5a35b13c2` |
| `stage-4-orbstack-session-authority-failover-20260726-final3.json.partial.json` | `9196473363fed6ef96522ac2a1dce12713d238d52c077639e646e7a56bba87a1` |

## Cleanup and boundary

The final namespace, isolated ClusterRole, and ClusterRoleBinding were absent after cleanup. The pre-existing
`synara-system/synara-control-plane` deployment remained `2/2` Ready and available.

This closes the local E3 Control Plane Pod-failure Session-authority proof. OrbStack is still one physical node and one
local failure domain. The run does not prove whole-Cluster loss, external PostgreSQL failover, multi-AZ storage/load
balancing, real Node partition, cross-Region recovery, or production-duration soak; those remain Stage 4 E4 gates.

`bun fmt`, `bun lint`, and `bun typecheck` were not run because the user did not request the heavyweight Bun checks.
