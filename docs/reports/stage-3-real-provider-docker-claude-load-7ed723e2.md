# Stage 3 Real Claude Docker Load Blocker Closure

- Result: **PASS FOR THE PREVIOUSLY FAILED CLAUDE DOCKER LOAD CHILD**
- Source commit: `7ed723e20eb72605263c6f79d9b4e01612a27ded`
- Source worktree: clean
- Target: managed Docker
- Provider: Claude Agent SDK through the real Control Plane, agentd, Worker Protocol and Provider Host
- Report JSON SHA-256: `5af9eece29a0e35b4bd8707d9cac417a4fa36bf11d5c5c8be870615d0f3813e5`
- Report Markdown SHA-256: `9a7af2599e663d71816931ea51b5153a24d64e4d4219335c1756116f15453050`

## Result

The targeted `real-provider.load.multi-session-admission-wave` child passed all `8/8` acceptance cases after the
preceding consolidated Docker run had passed the other five children and failed only this child. The passing run
used the checked-in operator-approved `1800s` load SLA, two `1 CPU / 2 GiB` Workers with one active slot each, a
Tenant concurrency limit of two and four Sessions.

The run completed:

- `17` controlled admission waves and `68/68` successful real-Provider Executions;
- `34/34` exact `execution_quota_exceeded` rejections followed by successful slot-reuse admissions;
- `51` pending-interaction overlap observations across two distinct Workers;
- one Control Plane restart after wave `10`, followed by native-Cursor, Session sequence and terminal-path
  continuity checks;
- zero unexpected failures, zero duplicate terminal outcomes and zero double execution.

The enforced Synara-controlled SLA observations were:

| Metric                      |    Observed |    Required | Result |
| --------------------------- | ----------: | ----------: | ------ |
| Minimum duration            | `1866.483s` |  `>= 1800s` | pass   |
| Control Plane admission P95 |       `7ms` | `<= 1000ms` | pass   |
| Control Plane admission P99 |      `18ms` | `<= 2000ms` | pass   |
| Slot-reuse admission P95    |       `7ms` | `<= 2000ms` | pass   |
| Slot-reuse admission P99    |      `18ms` | `<= 3000ms` | pass   |
| Unexpected error rate       |         `0` |         `0` | pass   |

The Provider-dependent interaction-ready and Turn-completion distributions remain capacity-planning evidence and
are not presented as Synara-controlled admission SLIs.

## Security and cleanup

The Runner removed its two managed Worker containers, owned image, network, Workspace volume and local state without
broad cleanup. The final output scan covered `11` JSON/Markdown/log/text files and `13,320,174` bytes against seven
known secret sentinels plus private-key, cloud-key, GitHub-token and OpenAI-style-key patterns, with zero findings.
Credential, endpoint and operator environment-variable values are not reproduced in this report.

During the follow-up repository audit on 2026-07-23, four old owner-labelled Stage 3 acceptance images and seven old
owner-labelled, unused Stage 3 networks from earlier failed runs were removed individually. A post-cleanup inventory
reported zero Stage 3 acceptance containers, images, volumes and networks.

## Evidence boundary

This report closes the concrete Claude Docker load child that failed in
`.tmp/stage3-real-provider-docker-release-47214f38-rerun1`. It does **not** rewrite that failed aggregate, import a
standalone report into a resumable checkpoint after the fact, or claim that all six Docker children ran under one
current release SHA and one shared immutable image.

The later merge commit `cc546d3a9c212e663e23bb92d52b08fa8f714c07` changes no file or tree under
`apps/provider-host`, `services/control-plane`, `scripts/stage3-provider-acceptance`, `deploy/worker`, the Provider
Runtime contract/catalog, the Worker Dockerfile, `bun.lock` or the root package manifest. That proves the tested
runtime/gate inputs did not drift across the merge, but it does not replace the release checklist's stricter
same-commit aggregate requirement.

Still open are the current-release six-child Docker aggregate, the two Kubernetes load children, a same-release
four-Target run, registry-pushed immutable multi-arch evidence, production Vault/Rekor/Kyverno evidence and the
production-duration retention/soak boundary.
