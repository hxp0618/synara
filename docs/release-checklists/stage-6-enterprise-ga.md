# Stage 6 Enterprise Self-hosted GA Release Checklist

> 模板。每次 GA 候选发布复制本文件；没有真实证据的项目保持未勾选。fixture、静态检查和计划文档不能替代
> 真实发布、恢复、渗透、容量或长稳证据。

## Release identity

- [ ] Release / candidate:
- [ ] Git commit:
- [ ] Control Plane image digest:
- [ ] Worker image digest:
- [ ] Provider Host image digest:
- [ ] Web artifact digest:
- [ ] Platform Admin artifact digest:
- [ ] Desktop macOS arm64 artifact digest:
- [ ] Desktop macOS x64 artifact digest:
- [ ] Desktop Windows x64 artifact digest:
- [ ] Desktop Linux x64 artifact digest:
- [ ] Migration tail and checksums:
- [ ] Environment / Regions:
- [ ] `synara-stage6-release-evidence-v2` manifest / SHA-256:
- [ ] Candidate evidence bundle consistency receipt / SHA-256:
- [ ] Final GA review archive receipt / SHA-256:
- [ ] Protected Desktop build run ID / raw artifact-set SHA-256 / final asset-set SHA-256 / environment approval record:
- [ ] Started / completed at (UTC):
- [ ] Operators and approvers:

## Tenant and identity lifecycle

- [ ] New Tenant completed registration or enterprise activation without direct database work.
- [ ] Evaluation, activation, suspension, closure, deletion and recovery transitions passed with audit evidence.
- [ ] Joiner, mover and leaver paths revoked Sessions, Memberships, Grants and Credentials as specified.
- [ ] Domain Verification and SSO Enforcement passed positive, bypass and lockout-recovery cases.
- [ ] RBAC v1 decision and Service Account / API Token governance are documented and enforced.

## Plan, quota, usage and internal cost

- [ ] Internal entitlement profile, Feature Flag and usage-policy admission matched the approved installation policy.
- [ ] Token, Execution, CPU, Memory, Storage, Network and Provider Cost dimensions reconciled.
- [ ] Soft quota warning and hard-limit behavior passed concurrent PostgreSQL tests.
- [ ] A user explained one Session and one Turn cost from product-visible data.
- [ ] Internal cost-center allocation reconciled estimated and actual platform allocations without duplicates.

This release line uses the `internal-self-hosted` product mode. Payment, Checkout, tax, dunning and external
subscription settlement are out of scope and must remain disabled. Evidence must reconcile Provider cost, shared Target
estimated allocation and any available actual cloud-invoice allocation to the organization's cost-center export without
double counting; it must not use Stripe fixtures as a substitute.
For the Session/Turn explanation, confirm an execution with missing Provider pricing is displayed as unavailable and excluded
from known subtotals, while a separately captured explicit zero-cost report is displayed as zero. Migration `000150` makes
that distinction authoritative and monotonic in PostgreSQL and SQLite.
Import those exact bytes through Platform Admin → Internal cost reviews and collect distinct Operations and Owner
decisions before Release Governance approval. That product/database gate verifies receipt consistency and role separation;
it does not independently authenticate the external cost sources or prove reporting-period completeness.
Create the receipt with `bun run stage6:cost:prepare -- ...` from a path-only draft under the protected evidence root;
do not hand-enter file hashes or use the lower-level validator to overwrite an existing receipt.

## Operations and support

- [ ] Tenant/Organization and Platform Admin daily-operation matrix passed without CLI or database access.
- [ ] Worker, Execution, Queue, Artifact, Credential and Identity Connection views matched authority data.
- [ ] Support impersonation was read-only, reasoned, time-bounded, audited, Tenant-visible and disableable.
- [ ] Internal Status Board and employee incident communication exercise completed.

## Data governance and compliance

- [ ] Audit Search/Export, Retention and Legal Hold passed immutable-boundary tests.
- [ ] Tenant export/deletion, user export/deletion and DSAR workflows completed with evidence.
- [ ] Data residency placement and written Region boundary matched during failover/evacuation.
      Generate the immutable manifest/receipt pair from the eight finalized attachments with
      `stage6:residency:prepare`; preserve a failed failover/evacuation as an ineligible receipt. The permanent
      `evidence-validated-not-residency-approved` assessment does not verify Annex/approval signatures, deployed browser,
      inventory authority or live exercise provenance, which must be checked separately before completing this row.
- [ ] Provider licensing, account sharing, data use and privacy requirements were approved.
      Confirm the active Provider commercial authorization binds the exact terms, executed agreement/Order Form, DPA and
      termination Runbook bytes with four non-zero SHA-256 values, and that each Legal/Privacy/Security/Product decision
      binds its own evidence bytes with a non-zero SHA-256 value. A URL-only legacy record or decision, matching URL with
      changed bytes, or product approval without external Legal/Privacy authority cannot satisfy this row. The exact active
      authorization ID/version must also appear in the `provider_commercial` Release candidate binding and remain valid
      through the released transition.
- [ ] SOC 2 Type II / ISO 27001 path is active and release evidence was collected automatically.

Record the exact scope, owners, auditor engagement, observation window, repository/access/retention policy, seven control
families, independently reviewed release manifest and four separated start-gate decisions through Platform Admin →
Compliance. `record-complete-not-audit-active` is metadata readiness only; verify auditor authority, repository immutability
and the actual observation start externally before checking this item. Each accepted release-manifest review and each of
the four role decisions must bind the exact externally retained evidence bytes with a non-zero `sha256:` value; superseded
URL-only legacy records do not count.

## Reliability, security and release

- [ ] Signed packaged Desktop Enrollment link-open passed on macOS arm64/x64, Windows x64 and supported Linux targets.
- [ ] Desktop OS credential protection, allowlist denial, initial-hydration rollback, rotation/replay and disconnect/revocation passed without secret leakage.
- [ ] Real PostgreSQL concurrent Desktop Enrollment redemption and session rotation/replay passed against the candidate migration chain.
- [ ] PostgreSQL, object storage, KMS and Queue were restored from production backup; measured RPO/RTO met policy.
      Build the evidence with `stage6:recovery:prepare` in `subject` then `final` mode; retain both immutable
      manifest/result pairs and their SHA-256 sidecars. Do not accept a final approval file created before the exact subject
      was frozen.
      Confirm the candidate-bound Recovery Drill has five active Database/KMS/Operations/Security/Storage decisions, each
      bound to its exact retained evidence bytes with a non-zero `sha256:`. Superseded URL-only decisions and a Drill
      reopened by Migration `000143` cannot satisfy this row.
- [ ] Availability, API Latency, Execution Start Delay and Event Delay SLOs met the error budget.
      Generate the immutable manifest/receipt pair with `stage6:slo:prepare`; retain both SHA-256 sidecars and preserve
      missed or not-assessable windows rather than replacing them with a passing sample.
      Confirm the candidate-bound SLO Window has four active Engineering/Operations/Security/Product decisions and each
      decision binds its exact externally retained evidence bytes with a non-zero `sha256:`. URL-only superseded decisions
      and a legacy Window reopened by Migration `000142` cannot satisfy this row.
- [ ] Control Plane → Worker → Provider traces and Tenant-boundary audit passed.
- [ ] Stage 5 acceptance is complete; third-party penetration test has no unaccepted high-severity finding.
      Generate the immutable manifest/receipt pair with `stage6:penetration:prepare`; preserve incomplete runs and open
      High/Critical findings as ineligible receipts instead of replacing the assessor-bound attempt.
- [ ] Worker Image, Provider CLI and dependencies passed SBOM, signature and vulnerability policy.
      Attach the same-candidate `synara.stage6-worker-supply-chain-evidence.v1` receipt and its three immutable inputs. The
      validator must stable-read the exact bounded input bytes and reject symlinks, duplicate JSON fields and credential
      material. The receipt assessment must remain `evidence-validated-not-worker-supply-chain-approved`; separately verify
      the Registry, KMS/Rekor, cluster and operator authorities before checking this item.
- [ ] Migration × Protocol × Worker Image × Web compatibility matrix and rollback exercise passed.
- [ ] Production secrets, certificates, domains and key-rotation runbooks were exercised.
      Validate the same-candidate exercise under
      [`production-rotation-acceptance-v2.md`](../contracts/production-rotation-acceptance-v2.md). Attach the immutable
      `synara.stage6-production-rotation-evidence-receipt.v2` receipt and sidecar; its assessment must remain
      `evidence-validated-not-production-rotation-passed`. `eligibleForHumanGateReview=true` does not authenticate the
      provider evidence, operators, approvers or signatures, so those authorities must still be checked before marking
      this row complete.
- [ ] Capacity and long-duration stability runs met the recorded resource profile and SLO.
- [ ] User, administrator, deployment and troubleshooting documentation matches the release.
- [ ] Release approval, changelog and breaking-change notice were published through the agreed channels.
      Confirm every Engineering/Operations/Security/Product and conditional Privacy/Legal Release decision binds its
      credential-free evidence reference to the exact reviewed bytes with a non-zero SHA-256. A legacy URL-only decision
      cannot authorize the candidate. Verify each approver's exact `release.*` Governance Authority grant also binds the
      corporate-delegation evidence bytes with a non-zero SHA-256 and remains active at decision time.

Local implementation evidence is recorded in
`docs/reports/stage-6-desktop-macos-arm64-local-acceptance-20260731.md`,
`docs/reports/stage-6-desktop-macos-x64-rosetta-local-acceptance-20260731.md`, and
`docs/reports/stage-6-desktop-macos-arm64-local-install-fix-20260731.md`. It covers an arm64 installed
connection lifecycle, x64 under Rosetta, target-native dependency packaging, and disconnected arm64
startup without premature Keychain access. The apps used Apple Development signing, were rejected by
Gatekeeper, had no stapled ticket, and used local/loopback infrastructure; Rosetta is not native Intel
evidence. Both Desktop rows above therefore remain unchecked.

Desktop candidate evidence follows `docs/contracts/desktop-installed-acceptance-v1.md` and
`docs/runbooks/desktop-native-release-acceptance.md`. Prepare the four-host protected bundle from path-only evidence with
`stage6:desktop:native:prepare`; it derives all hashes from stable secret-scanned bytes and validates before immutable
publication. `stage6:desktop:native:validate` remains the read-only path for an existing manifest. Their permanent
`evidence-validated-not-desktop-ga-passed` assessment and `eligibleForHumanGateReview` flag do not replace distinct
Engineering, Security and Release approval; Rosetta, Apple Development signing and build-only artifacts remain ineligible.

Evidence for the first two items must use `docs/contracts/backup-recovery-rpo-rto-v1.md` and
`docs/contracts/enterprise-service-level-objectives-v1.md`. A recovery-validator receipt or Prometheus rule installation
alone is implementation evidence, not a checked production result.

SLO-window collection follows `docs/runbooks/slo-error-budget-report.md`. The validator receipt always remains
`evidence-validated-not-slo-passed`; only a complete production/production-like 30-day window with incident and budget
review can check the SLO item.

Paging and internal communication exercises follow `docs/runbooks/enterprise-incident-response.md`. The validator receipt
always remains `evidence-validated-not-operations-ready`; only a live production/production-like exercise with the private
rota annex, independent Status Board, employee notification delivery and Operations/Communications approval can check the item.
Create its immutable manifest/receipt with `bun run stage6:incident:prepare -- ...` from a path-only protected draft; do
not hand-enter the seven evidence hashes.

Daily administrator/support operations follow `docs/contracts/enterprise-operations-ui-v1.md` and
`docs/runbooks/enterprise-admin-daily-operations.md`. The source matrix receipt remains
`source-ui-routes-validated-all-surfaces-reachable-not-operations-passed`; source-level pending surfaces are now zero, but
check the item only after all 49 rows pass in the deployed browser with role separation, denied-role cases and Audit
evidence. The Support lifecycle evidence must include a Grant-correlated `support.access_revoked` for explicit or
Tenant-policy revocation and a system-actor `support.access_expired` for automatic timeout; a terminal Grant without the
matching Tenant-visible event fails the row. Only the Operations v2 validation receipt can enter current governance;
Migration 162 retains v1 as immutable audit history and supersedes its active decisions. Validate the protected bundle with
`scripts/stage6-operations/validate_operations_exercise_evidence.py`; its
`evidence-validated-not-operations-passed` receipt and `eligibleForHumanGateReview` flag still require distinct Operations
and Security review before the checklist item can be checked.

The compatibility item starts from `docs/release-matrices/stage-6-compatibility-v1.json` and
`docs/contracts/release-compatibility-v1.md`. `stage6:compatibility:check` stable-reads and bounds the exact matrix,
package/protocol sources and complete Migration lineage, rejects symlink/duplicate-JSON/credential input, and derives the
tail digest from the captured bytes. A source-compatible validator receipt does not replace prior-build
forward-schema, Worker canary/promote/rollback, and internal-user-path verification.

Release approval and communication follow `docs/runbooks/enterprise-release-governance.md`; copy
`docs/release-checklists/stage-6-change-notice.md` for the candidate and retain its publication proof. The in-product channel
uses the same entry for the post-upgrade notice and Settings → Release history; structured administrator/breaking/security
notices must pass the 30/90-day or approved-expedited timing contract before the candidate build.

Secret/certificate/domain exercises follow `docs/runbooks/production-secret-certificate-domain-rotation.md`. The checked-in
KMS implementation, dry-run or local/PostgreSQL tests are not production exercise evidence; attach the immutable rewrap
receipt, old-path denial and external TLS/DNS/client-path results before checking that item.

The documentation source set starts at `docs/enterprise/README.md` and is fixed by
`docs/release-matrices/stage-6-documentation-v1.json`. Run `stage6:documentation:validate` against the exact source tree;
it stable-reads and bounds the matrix and five documents, rejects symlinks, duplicate JSON fields and prohibited secret
material, and hashes the same bytes used for semantic checks. Its validator receipt remains
`source-documentation-validated-not-release-verified`; check the item only after the deployed candidate's UI labels,
role access, immutable configuration and approved internal wording have been reviewed.

Capacity and long-duration evidence follows `docs/contracts/capacity-long-duration-acceptance-v1.md` and
`docs/runbooks/capacity-long-duration-test.md`. Create the bundle with the path-only capacity preparer so the seven evidence
hashes and immutable manifest/receipt pairs are derived from stable bounded reads rather than hand-authored. The validator
receipt always remains
`evidence-validated-not-capacity-passed`; only a production/production-like run with signed review can check the item.
Both Engineering and Operations approvals must bind the exact reviewed evidence bytes with a nonzero lowercase SHA-256;
Migration `000145` supersedes URL-only history and reopens the run until both replacement decisions are active.

Third-party penetration evidence follows `docs/contracts/third-party-penetration-acceptance-v1.md` and
`docs/runbooks/third-party-penetration-test.md`. The validator receipt always remains
`evidence-validated-not-penetration-passed`; only the signed assessor report, closed/accepted findings and Security release
review can check the item. Each Engineering/Product/Security approval must bind the exact reviewed evidence bytes with a
nonzero lowercase SHA-256; Migration `000144` supersedes URL-only history and reopens the engagement until all three
replacement decisions are active.

The checked-in Candidate v5 preparer requires the exact source-current compatibility matrix and an exact-candidate internal usage/cost receipt covering Token totals,
Provider cost coverage, actual-over-estimated platform allocation and currency-safe known-cost totals. It rejects payment
semantics and cannot consume the historical Stripe Billing receipt. Candidate/Release ingestion and both database paths
require the approved two-role internal cost review; this structural gate does not substitute for real source authority or
the reporting-period evidence.

Incident exercise Operations/Communications decisions must each bind the exact reviewed paging/internal-communication
evidence bytes with a nonzero lowercase SHA-256. Migration `000146` supersedes URL-only history and reopens the Exercise
until both replacement decisions are active; this still does not prove live Status Board or employee notification delivery.

Operations exercise Operations/Security decisions must each bind the exact reviewed deployed-browser/Audit evidence bytes
with a nonzero lowercase SHA-256. Migration `000147` supersedes URL-only history and reopens the Exercise until both
replacement decisions are active; this still does not prove deployed hosts, browser sessions, Audit authority or execution.

Migration `000131`/`000132`/`000148` Billing exercise decisions are historical compatibility records only. They neither
satisfy nor belong to the current internal self-hosted release gate.

Security/Privacy incident resolution decisions must bind the exact reviewed containment and recovery evidence bytes with a
nonzero lowercase SHA-256. Migration `000149` supersedes URL-only history; already terminal incidents remain immutable, while
an incident still in `monitoring` requires a replacement active byte-bound decision before it can become `resolved`.

## Final decision

- [ ] All evidence links are immutable and access-controlled.
- [ ] Security, Operations, Product and Engineering approved GA.
- [ ] Release decision and residual accepted risks were recorded in Audit.

Record the exact candidate through Platform Admin → Release governance. The candidate creator must be distinct from all
four required role approvers; a successful source/API receipt is not evidence that the deployment/organizational gates or human authority
were valid. The product/database transition additionally requires internally approved exact-candidate SLO, Recovery,
Penetration, Capacity, Incident exercise and Operations exercise records, plus the exact-candidate internal usage/cost review;
those internal states still do not prove the deployed production SLO, restore execution,
backup authority, third-party assessor identity, signed report, capacity environment/telemetry authority, long-duration
execution, live Status Board/paging/employee notification delivery, deployed browser/Audit evidence authority, cost-source
completeness, organizational authority or signatures. Preserve the resulting Platform
Audit request IDs with this copied checklist.

Before any role decision, Platform Admin → Governance roles must show an active, unexpired exact-function assignment from
a distinct Operator Tenant Owner. Follow `docs/runbooks/stage-6-governance-authority.md`; a selected role string without
that server/database authority is rejected and cannot count as approval.

After every row above is decided, validate the frozen archive with
`docs/contracts/stage-6-final-ga-review-v1.md` and the fail-closed `bun run stage6:final:prepare -- ...` path. Supply only
relative evidence paths in the draft; the preparer computes hashes, validates the complete materialized manifest and then
publishes the immutable manifest/receipt pairs. The archive binds this completed copy, the completed change notice,
Candidate v5 receipt, protected release approval, canonical final asset set, Platform Audit request IDs, all 32 functional
control decisions, final role decisions and residual risks. Its receipt must remain
`final-review-consistent-not-ga-authority-verified`; external evidence and approver authority still require the real GA
authority's acceptance.

- [ ] Upload that exact immutable Final Review v1 validation receipt through Platform Admin → Release governance while
      the candidate is `observing`; confirm the displayed receipt digest/control count, then use the identical decision
      summary and residual-risk inventory for `released`. Eligibility is still not external GA approval.
