# Stage 7 Developer Platform Release Checklist

> 每个 public beta 或 GA candidate 都复制本文件。源码测试、fixture、localhost、一次性 VM 和构件
> verifier 只能满足对应工程行，不能替代 Registry ownership、外部部署、真实 Provider、生产网络、
> 第三方评审或人工发布批准。没有直接证据的项目保持未勾选。

## Release identity

- [ ] Release channel (`public-beta` / `public-ga`) and version:
- [ ] Git commit and protected CI run:
- [ ] OpenAPI SHA-256 and public operation count:
- [ ] Control Plane image digest and migration tail/checksums:
- [ ] Developer Docs artifact digest and deployed origin:
- [ ] npm package/version, tarball digest, provenance and attestation:
- [ ] PyPI project/version, wheel/sdist digests and attestations:
- [ ] TypeScript/Python minimum supported Control Plane version:
- [ ] Environment, Regions, started/completed UTC:
- [ ] Engineering, Security, Product and Release approvers:

## M1 — API product surface

- [ ] Every registered `/v1` route is classified `internal`, `public-beta` or `public-ga`; no route bypasses the classified mux.
- [ ] Published OpenAPI 3.1 contains only approved `codegen-ready` operations and matches the deployed handlers.
- [ ] Internal Worker, Provider Host, dev-login, SCIM and Artifact-content routes are absent from the public specification.
- [ ] Service Account bearer enforces Tenant/Organization scope, fixed RBAC, immediate revocation and private-resource denial.
- [ ] Machine Event, Audit and Idempotency attribution records the Service Account rather than its creating User.
- [ ] Bounded pagination, opaque cursor, Event `afterSequence`, stable error envelope and idempotent replay pass conformance.
- [ ] Per-key request/SSE admission, rate-limit headers and Tenant/Organization/key usage attribution reconcile in PostgreSQL.
- [ ] From API Key issuance, the curl quickstart completes Session → Turn → SSE → Approval → terminal Event within five minutes.

## M2 — TypeScript SDK and developer experience

- [ ] `@polaris-agents/sdk` generated types match the release OpenAPI and the hand-written domain layer passes conformance.
- [ ] Mutation retry, stable idempotency, 429/5xx behavior, typed errors, SSE reconnect/poll fallback and sequence-gap denial pass.
- [ ] Artifact upload/download and Interaction resolution do not expose grant tokens, payload text or internal delivery fields.
- [ ] CI-fix, PR-review and batch-migration examples typecheck against the exact candidate package.
- [ ] Developer Docs quickstart, auth, idempotency, streaming, approvals, Webhooks, errors and lifecycle pages match the candidate.
- [ ] Deployed API Reference is generated from the filtered public document and is reachable from the approved external origin.
- [ ] npm organization/package ownership, OIDC Trusted Publisher, protected environment and provenance are independently verified.

## M3 — Python, Webhook, Console and BYO compute

- [ ] `polaris-agents` generated types match the same release OpenAPI and Python 3.11+ conformance passes.
- [ ] Wheel and sdist contain only the allowlisted package files and pass verifier plus `twine check` in the release environment.
- [ ] PyPI project ownership, OIDC Trusted Publisher and protected environment are independently verified.
- [ ] Console creates, scopes, rate-limits, rotates and revokes API Keys; plaintext is displayed once and usage/last-used are visible.
- [ ] Webhook secret lifecycle, thin payload, HMAC signature, timestamp, stable delivery ID, retry, dead-letter and replay pass.
- [ ] Deployed Webhook egress reaches an approved external HTTPS receiver and still denies redirect, proxy, private, loopback,
      link-local, reserved and DNS-rebinding destinations without persisting response bodies.
- [ ] Service Account registers a secret-free SSH/Docker/Kubernetes Target projection; local Target creation remains denied.
- [ ] Durable SSH provisioning replays one operation, survives claim takeover, fences the Target and reaches exact Worker readiness.
- [ ] A real managed Target run covers replacement, post-replacement Workspace continuity, Control Plane restart and cleanup.

## M4 — GA dependencies

- [ ] `/v1` additive-only, `/v2` breaking-change policy, changelog, `Deprecation`/`Sunset` headers and ≥12-month window are published.
- [ ] Every SDK release declares and tests its minimum compatible Control Plane version.
- [ ] Self-serve Tenant onboarding, mandatory BYOK, free-tier quota, retention and batch/shared-pool policy pass without database work.
- [ ] Stage 6 production SLO, error budget, incident response, recovery, data-residency and release-governance gates are approved.
- [ ] Stage 5 sandbox acceptance and third-party penetration review have no unaccepted High/Critical finding.
- [ ] Production secrets, certificates, DNS, KMS/registry keys and rotation runbooks are exercised against the exact candidate.
- [ ] Real Codex and Claude Provider release gates pass on the advertised Execution Targets without Credential or prompt leakage.
- [ ] External docs, npm, PyPI and API availability are monitored; rollback and customer communication paths are exercised.

## Final decision

- [ ] All required evidence above binds the exact candidate bytes and remains current.
- [ ] Engineering approval:
- [ ] Security approval:
- [ ] Product approval:
- [ ] Release approval:
- [ ] GA approval, if applicable:

Unchecked external-authority rows cannot be converted to pass by a source-only receipt. A public-beta source commit may be
released only after its M1–M3 release-channel rows are satisfied; `public-ga` additionally requires every applicable M4 row.
