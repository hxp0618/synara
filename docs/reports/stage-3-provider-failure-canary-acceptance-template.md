# Stage 3 Provider Failure and Canary Acceptance

> Template only. Do not mark a release accepted until the generated JSON report, exact Git SHA, Target evidence,
> cleanup checks, and Secret scan are attached. Deterministic fixture evidence is never a substitute for the real
> Codex App Server and Claude Agent SDK release gate.

## Run identity

- Git SHA: `<full-sha>`
- Worktree dirty: `<true|false>`
- Provider capability catalog SHA-256: `<sha256>`
- Runner schema: `synara.provider-acceptance.v1`
- Target: `<local|ssh|docker|kubernetes>`
- Provider: `<codex|claudeAgent>`
- Started/finished UTC: `<timestamps>`
- JSON report: `<absolute-or-artifact-path>/acceptance-report.json`
- Markdown report: `<absolute-or-artifact-path>/acceptance-report.md`

## Exact command

```sh
python3 scripts/stage3-provider-acceptance/acceptance_runner.py \
  --target <target> \
  --provider <provider> \
  --failure-matrix \
  --output-dir <unique-output-directory>
```

List every non-default safety authorization, reused image, Docker network, Kubernetes context, kubeconfig, and
timeout. Never redact an image digest or Git SHA; always redact bearer tokens, cookies, private keys, and
Credential payloads.

For an owned Kind Kubernetes matrix, add `--kind-worker-nodes 2` so the PDB drain case can prove replacement onto
another Ready schedulable Node while the source remains cordoned. A reused context must already expose at least two
such Nodes and still requires the explicit non-disposable/Node-drain authorizations.

## Machine-readable matrix result

Copy the generated `configuration.failureMatrix` object and the case summary from `acceptance-report.json`.

| Case                        | Status     | Stable reason/failure code               | Generation/identity evidence                         |
| --------------------------- | ---------- | ---------------------------------------- | ---------------------------------------------------- |
| Provider malformed          | `<status>` | `protocol_violation`                     | `<fault and recovery execution IDs>`                 |
| Provider oversized          | `<status>` | `protocol_violation`                     | `<fault and recovery execution IDs>`                 |
| Provider crash              | `<status>` | `provider_unavailable`                   | `<fault and recovery execution IDs>`                 |
| Worker network interruption | `<status>` | `<reason>`                               | `<stale/replacement request and Generation>`         |
| Kubernetes Node drain       | `<status>` | `<reason>`                               | `<node, selector, Pod UID, replacement UID>`         |
| Kubernetes PDB Node drain   | `<status>` | `<reason>`                               | `<PDB budget, source/replacement Nodes, Generation>` |
| Kubernetes Pod eviction     | `<status>` | `<reason>`                               | `<Eviction API, UID precondition, replacement UID>`  |
| Kubernetes image canary     | `<status>` | `<reason>`                               | `<source/canary image, Target, Namespace, Manifest>` |
| Output Secret scan          | `<status>` | `runner.output_secret_detected` or empty | `<files/bytes/pattern names>`                        |

`unsupported` and `skipped` require the exact documented infrastructure reason. They must not be rewritten as
passes. Any `fail` requires the bounded diagnostic references from the generated report.

## Ownership and cleanup

- Acceptance owner label: `<synara.io/stage3-provider-acceptance-owner value>`
- Docker container/network/volume absent after cleanup: `<evidence>`
- Runner-owned image aliases absent after cleanup: `<evidence>`
- Owned Kind cluster absent, or exact reused-cluster Namespaces/RBAC absent: `<evidence>`
- Drained Node confirmed uncordoned: `<evidence>`
- No prune, broad Namespace deletion, or operator-owned image deletion was used: `<confirmed>`

## Security scan

- Case: `security.output-secret-scan`
- Scanned files/bytes: `<counts>`
- Known runtime Secret count: `<count>`
- High-confidence patterns: `<names>`
- Findings: `[]`

The scan covers generated JSON, Markdown, text metadata, and logs. Binary SQLite, Artifact, Checkpoint, and
Workspace payloads require their separate SecretGuard/storage acceptance and must not be claimed by this row.

## Evidence boundary and remaining release gates

State each boundary explicitly:

- Deterministic Provider Host fixture: `<pass|not-run>`
- Real Codex App Server on this Target: `<pass|not-run|blocked reason>`
- Real Claude Agent SDK on this Target: `<pass|not-run|blocked reason>`
- Immutable Worker release revision promotion/rollback: `<pass|not-implemented|blocked reason>`
- CNI-enforced NetworkPolicy interruption: `<pass|not-run|cluster implementation>`
- Long Session, repeated reconnect, and multi-Provider soak: `<pass|not-run|blocked reason>`

Release acceptance remains incomplete while any required real-Provider or immutable release-revision row is not
`pass`, even when every deterministic fixture case passes.
