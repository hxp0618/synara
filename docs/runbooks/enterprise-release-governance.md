# Enterprise self-hosted release governance

## Scope

This runbook governs Stage 6 internal self-hosted candidates. Desktop packaging in `.github/workflows/release.yml` and Target-
scoped Worker canary/promote/rollback remain their own execution mechanisms; neither constitutes Enterprise self-hosted approval.
The release manager assembles their evidence under one copied `stage-6-enterprise-ga.md` checklist.

Every product decision role also requires a current Platform Admin → Governance roles assignment under
`docs/runbooks/stage-6-governance-authority.md`. The request body's role is descriptive input, not authority.

## Roles and separation

- Release manager owns candidate identity, checklist state, schedule, decision record, and communication coordination.
- Engineering approver verifies source, Migration/protocol compatibility, tests, artifacts, and rollback safety.
- Operations approver verifies deployment, backup/restore point, capacity, SLO/error budget, on-call, and observation.
- Security approver verifies Stage 5 dependency, supply chain, secrets, Tenant-boundary and penetration evidence.
- Product approver verifies internal Plan/Entitlement behavior, user/admin documentation, Tenant impact, and residual risk.
- Communications owner prepares internal-user, administrator, Status Board, and support messaging.
- Privacy/Legal approval is required for Provider commercial terms, personal data, retention/legal hold, data
  residency, regulated-Tenant, or security-incident impact. Platform Admin records at least one immutable impact domain;
  the service and database derive this fifth approval requirement instead of trusting an operator checkbox.

The change author cannot be the sole Engineering approver. The deploy operator cannot be the sole Operations approver.
Support Access, destructive recovery, security exceptions, and acceptance of a high-severity risk keep their existing
four-eyes or executive boundary.

## Candidate states

```text
draft -> ready-for-review -> approved -> deploying -> observing -> released
                          \-> rejected              \-> rolled-back
```

Only the release manager advances state. `approved` fixes commit, five self-hosted service artifact digests, four native Desktop
artifact digests, Migration tail, compatibility matrix hash, evidence manifest hash, Regions, window, operators, approvers,
environment ID, Tenant Web/Platform Admin/Control Plane origins, candidate-bundle consistency receipt and planned notice.
Any change to those values
creates a new candidate or returns the candidate to review.

`released` requires the complete observation window, Tenant/user-path checks, SLO/Audit review, approved notice publication,
and recorded residual risks. A successful deploy is only `observing`.

## Platform Release Governance record

Use Platform Admin → Release governance after the immutable candidate receipt and final asset-set digest exist. An Owner or
Admin uploads the exact bounded v3 receipt and creates the record with the exact candidate ID, source commit, `bun.lock`, candidate receipt, final asset set,
environment ID and impact domains; those fields cannot be edited. The creator cannot approve the record, one operator cannot occupy two roles,
and Engineering, Operations, Security and Product must each append one immutable decision before `approved`; applicable
impact domains additionally require Privacy/Legal. A rejected
role decision atomically terminates that candidate. Security Admin may review but cannot create or advance candidates.
Every decision requires both a credential-free HTTPS reference and `sha256:<64 lowercase hex>` for the exact reviewed
evidence bytes. A URL alone is not approval evidence.

When `provider_commercial` is selected, choose every exact active Provider commercial authorization consumed by the
candidate. The Control Plane freezes each authorization ID, key, Provider and version during candidate creation. A missing
binding, URL-only document or decision, expiry, revocation, version drift, or attempt to edit/delete the binding blocks
review and every forward release transition. Removing the impact domain does not bypass the database gate; create a new
candidate if the commercial scope changes. A pre-Migration-138 nonterminal candidate with Provider commercial impact is
retained for history but cannot be backfilled after review began; recreate it from the exact candidate receipt and active
authorization set.

Advance state with the current version only. After `observing`, use **Bind immutable Final Review** to upload the exact
eligible v1 validation receipt. The Control Plane re-hashes and parses the receipt and binds its exact candidate identity,
32 controls, final approvals, decision summary and residual risks. `released` remains disabled and is rejected by the
service plus PostgreSQL/SQLite direct-write gates until that immutable binding exists. The released transition requires
the same bounded decision summary and explicit residual-risk disposition recorded in the receipt. `accepted` risks each
require a stable ID, owner, future due date, acceptance reason and HTTPS evidence reference; `none` requires an empty
inventory. Platform Admin displays these immutable receipt values read-only and sends them unchanged during the released
transition; do not copy, edit or reconstruct them in a separate form. Every create, approval, Final Review binding,
rejection and transition is written to the Platform
Operator Tenant Audit log. Do not place tokens, signed URLs or private evidence contents in references or reasons.

Migration 139 adds immutable SHA-256 evidence binding to every Release approval. Existing URL-only decisions remain
historical but no longer count in readiness or any forward transition; because decisions and roles are append-only, create
a new candidate instead of trying to replace one. Migration 138 adds the immutable exact Provider authorization binding and forward-transition gate. Migration 135 adds the
append-only Final Review table and exact decision/risk release gate. Migration 134 retains the 32 KiB exact-byte boundary, recomputes the receipt SHA-256, parses the exact v3 schema, requires
all ten receipt families plus the source-current compatibility matrix projection to be review-ready, and matches candidate ID, commit, lockfile, environment and Desktop
artifact-set digest.
PostgreSQL and SQLite repeat the structural/identity boundary and reject receipt replacement or cross-candidate reuse.

Before attempting `approved`, inspect **Exact-candidate readiness** on the same page. Its read-only server projection lists
candidate evidence, exact Provider commercial authorization bindings, impact-derived Release decisions, SLO, Recovery, Penetration, Capacity, Incident exercise, Operations
exercise and internal cost review together. Refresh after importing or approving a governed receipt. The transition and this
view call the same predicates, so a displayed missing gate is the actual product-state blocker rather than a UI estimate.
After the observation window, the same view shows a separate **Final Review release binding** and an explicit released
transition eligibility result; this does not alter the ten gates used for the earlier `approved` transition.
Every row also names its external verification boundary. `internal gates satisfied` means only that the candidate is eligible
for the internal state transition; `externalGaStatus=required_not_verified_by_synara` remains fixed and must not be relabeled.

This product record binds who decided what and when; it does not validate reviewer corporate authority, protected GitHub
environment authority, external assessor, production observation or cryptographic signatures. Release `approved` additionally
requires exact-candidate internally approved SLO, Recovery, Penetration, Capacity, Incident exercise, Operations exercise and internal cost review records; these compose internal
decisions only and do not authenticate the external evidence. Keep using
the candidate-bundle, protected workflow and role-owned external gates below. A missing external gate cannot be repaired by
advancing this state machine.

For the four Desktop installers, Stage 6 uses the protected same-run path in `.github/workflows/release.yml`: dispatch from
the clean candidate branch with publication and `enterprise_ga_candidate` enabled before the planned tag exists. After the
four native jobs upload their exact bytes, the workflow waits at the `stage6-enterprise-ga` environment. Reviewers download
those artifacts plus the pre-gate `finalized-release-assets` artifact, complete native/external evidence, configure the candidate ID/source commit/build run ID/bundle receipt
SHA-256 plus the candidate receipt's Desktop artifact-set SHA-256 environment variable and the actual receipt as a
single-line base64 protected environment secret, then approve. Before recording approval the job parses the receipt,
verifies every raw provenance-listed payload and the canonical final public asset set from the same run, proves the final
non-YAML payload/trust closure is byte-identical to the accepted raw candidate, and binds all three digests. The publication
job repeats all checks and only uploads the finalized artifact; it cannot rewrite updater metadata after verification. The
job publishes those accepted bytes and creates the tag; rebuilding after acceptance is a new candidate.

Before dispatch, configure that environment with two to six independent required reviewers, self-review prevention, and
administrator-bypass disabled. GitHub requires only one of the configured reviewers to release the waiting job, so that
technical approval is not a substitute for the role-specific checklist signatures above. The preflight summary prints the
exact required review comment `SYNARA_STAGE6_APPROVE <tag> <run-id>`. The workflow then verifies the current environment
configuration, disabled bypass, actual review history, distinct reviewer/workflow actors and first run attempt before writing
the immutable record. Never rerun an approved candidate; dispatch a new run and regenerate its evidence. Finish review within
GitHub's 30-day waiting limit.

## Required evidence before approval

1. Exact clean commit and immutable Control Plane, Worker, Provider Host, Web, Platform Admin, macOS arm64/x64, Windows x64
   and Linux x64 Desktop digests collected by the Stage 6 evidence collector.
   The collector must finish with its second HEAD/clean-worktree check and exclusive `0600` manifest/sidecar publication;
   never overwrite or edit a prior candidate's release-evidence files.
2. Compatibility validator receipt plus prior-build-on-target-schema and rollback exercise evidence.
3. Migration checksums, additive/contract review, backup/restore-point ID, and the recovery policy selected.
4. Worker SBOM, vulnerability/secret scan, signature, transparency-log, admission, canary, and rollback evidence.
5. Tenant lifecycle/identity/entitlement/support/data-governance acceptance for the candidate.
6. SLO window, current budget, capacity/soak, alert/on-call, Status Board, incident, and deployment runbook readiness.
7. Stage 5 dependency and independent security/penetration disposition.
8. Provider/Privacy/Residency/Compliance approvals applicable to the exact offer and deployment.
9. Customer-safe release notes, administrator actions, known limitations, rollback impact, and breaking-change notice.
10. A `stage-6-candidate-evidence-bundle-v5` receipt proving all ten external-control receipts, the exact compatibility matrix, including Worker
    supply-chain and the deployment Residency Annex receipt, refer to this exact candidate commit, environment ID, origins,
    Regions, Migration tail, lockfile and Artifact digests.

Platform Admin → Release governance cannot enter `approved` until the exact candidate also has internally approved SLO,
Recovery, Penetration, Capacity, Incident exercise, Operations exercise and internal cost review records. The Recovery gate requires all four component objectives and restore canaries,
all five source decisions, five separated functional approvals and the preserved external-authority boundary. The Capacity
gate requires an eligible 24/72-hour receipt projection, headroom, phases/exercises, probes, measurements and distinct
Engineering/Operations decisions while retaining the external environment/telemetry/signature/execution boundary.
The Incident exercise gate requires the exact candidate receipt, independent Status Board origins, separated roles, paging
targets, all six internal service components, timeline cadence, employee notification delivery, recovery observation, review flags and distinct
Operations/Communications decisions while retaining the external provider/delivery/signature/execution boundary.
The Operations exercise gate requires the exact 49-row candidate receipt, three Artifact digests, distinct Web/Admin
origins, eleven production-authenticated accounts, every positive/negative authorization result, unique request IDs, zero
developer fallback, Support lifecycle and distinct Operations/Security decisions while retaining the external deployment,
browser-session, Audit/evidence, signature, execution and approver-authority boundary. Its own import/decision meta-actions
remain outside the 49 governed rows to avoid a self-referential receipt deadlock.
The internal cost gate requires the exact v4 candidate receipt, usage period, Token totals, complete Provider-cost coverage,
actual-over-estimated platform allocation, currency-safe known-cost arithmetic, four unique evidence files and distinct
Operations/Owner decisions. It rejects payment semantics and preserves the external source, signature, execution and
approver-authority boundary. Its import/decision actions are two of the 49 independently exercised Platform operations;
only the Operations-exercise receipt's own later import/approval actions stay outside the matrix to avoid self-reference.
These product/database gates prevent missing or cross-candidate evidence from being approved; they do not authenticate the
external evidence or replace the production SLO and restore checks above.

For the compliance portion, link the Platform Admin → Compliance program and its Audit request IDs. The program must show
all seven control families, an accepted exact-candidate release manifest and four separated start-gate approvals. Its
`record-complete-not-audit-active` assessment is deliberately insufficient to claim an active audit; independently verify
the auditor engagement, repository authority and observation period under
`docs/runbooks/compliance-control-evidence-governance.md`.

An unchecked mandatory line rejects the candidate unless the owning approver records a time-bounded, named, non-
prohibited risk acceptance. Real production restore, unaccepted high-severity penetration findings, missing Provider
authorization, missing internal Status Board/on-call, or an exhausted error budget cannot be waived by an engineer alone.

## Deployment and observation

Follow the order in `docs/contracts/release-compatibility-v1.md`: artifacts, restore point, additive Migration, Control
Plane/Web, Worker canary/promote, then observation. Record UTC start/end and every policy/version transition. Stop on
checksum mismatch, incompatibility, auth/isolation regression, missing evidence authority, SLO fast burn, or unknown
customer-impacting error.

Rollback uses the newest safe layer. Never down-migrate or directly edit release/Worker/Execution rows. A security or data
integrity incident enters the incident-response runbook even if rollback succeeds.

## Changelog and external notice

Use `docs/release-checklists/stage-6-change-notice.md` as the single source for product release notes, administrator notice,
support brief, and internal stakeholder communication. Derive channel-specific text from one approved change inventory; do not maintain
different promises per channel.

Each entry states:

- internal-user-visible outcome and affected plan/Region/role;
- administrator or user action and deadline;
- compatibility/Migration/Worker implications and rollback behavior;
- security/privacy/data-residency impact without exploit or Tenant-sensitive detail;
- new limitations, known issues, and support route; and
- availability date, staged rollout window, and evidence-backed completion status.

Routine additive changes may publish at release. An administrator action should be announced at least 30 days before it
becomes mandatory. A internal-user-visible breaking change requires an approved migration path and at least 90 days notice
unless an urgent security/legal action requires less; the exception names approvers, impact, mitigation, and expiry. Once
Stage 7 exposes a public API, its longer versioned `Deprecation`/`Sunset` policy takes precedence and cannot be shortened by
this runbook.

Status Board incidents describe current impairment, not planned release marketing. A planned high-risk maintenance window
may be posted there, but the release note and incident record remain distinct linked artifacts.

## Channels and evidence

The deployment annex must name the public release-notes URL, administrator notification channel, support brief channel,
in-product changelog channel, and customer contact policy. Preserve publication IDs/URLs, timestamps, audience, and final
content hashes. Never include mailing lists, private channel webhooks, or credentials in the repository.

The Web app implements the in-product channel through its one-time post-upgrade notice and Settings → Release history,
both sourced from `apps/web/src/whatsNew/entries.ts`. Mandatory administrator action, breaking changes, and urgent security
exceptions use the typed `notices` records and render ahead of ordinary feature highlights. The release author must copy
the approved, customer-safe content from the candidate change notice into that source before building the candidate;
`apps/web/src/whatsNew/entries.test.ts` verifies identifier uniqueness and the 30/90-day timing rules. A passing source test
does not prove publication: the candidate checklist still records the deployed version, visible notice, timestamp, audience,
URL/publication ID, and final content hash.

## Emergency changes

For an active security/legal incident, the incident commander and Security/Legal may authorize an expedited candidate.
The exact commit/digests, rollback owner, compatibility check, supply-chain check, backup/restore point, on-call, and
customer-impact record are never skipped. Deferred documentation, SLO review, and full approval receive named owners and
deadlines; the measured incident and error budget are not edited away.

## Closeout

After observation, freeze the checklist, release decision, change notice, evidence/compatibility/recovery receipts,
candidate-bundle consistency receipt, dashboard queries, Audit reference, and residual risks in the access-controlled evidence repository. Create follow-up
owners for every accepted risk and update this runbook after material rollback, exercise, or incident.

Before configuring or dispatching a protected candidate, run:

```bash
bun run stage6:github:readiness -- \
  --repository owner/repository \
  --branch protected-release-branch
```

This command is read-only. It extracts the secret and variable **names** referenced by the local `release.yml`, requests
configured names from GitHub, checks the Stage 6 Environment and source-branch protection, and separates static repository
inputs from candidate-scoped Environment inputs. It never requests secret values. It reads only the non-secret repository
variable `SYNARA_FINALIZE_RELEASE` and requires its exact value to be `1`; an invalid value is reported by variable name
without echoing the value. Because `SYNARA_ALLOW_UNSIGNED_WINDOWS_RELEASE` activates when it equals the eventual candidate
version, Enterprise GA readiness requires that exception variable to be absent entirely; a stale or future version value
is not accepted as safe. `SYNARA_PUBLISH_CLI` remains an optional publication-scope control. The checker treats 403 as an authorization failure rather than missing configuration. For
organization-owned repositories it remains fail-closed because organization-secret visibility is not assessed. A ready result is only
`github-release-inputs-ready-not-ga-approved`; it does not prove a credential value, signature, workflow run or GA
decision.

The release workflow ignores `SYNARA_PUBLISH_CLI=1` whenever `enterprise_ga_candidate=true`. npm CLI publication remains
available for ordinary releases, but Stage 6 approval cannot authorize a package that is absent from the Desktop artifact
set, candidate evidence bundle and Final Review archive. Publish a CLI package separately under its own release evidence.

Interpret the two readiness fields separately. `readyForCandidateEnvironmentApply` means the protected-environment
apply tool has a safe starting point: the workflow contract is exact, static repository input names exist, the source
branch is protected, the Environment exists, and administrator bypass is already disabled. The apply operation is what
writes and then verifies the candidate-specific reviewers, self-review policy, branch policy and Environment inputs.
`readyForProtectedCandidateDispatch` is stricter and becomes true only after those protections and candidate input names
are present. Neither field authorizes a repository mutation or a release.

If the authoritative source branch is not protected, first produce a read-only plan bound to its exact current HEAD and
the actual blocking CI job names:

```bash
bun run stage6:branch-protection -- \
  --repository owner/repository \
  --branch protected-release-branch \
  --expected-head 40-character-lowercase-commit \
  --required-check "CI / Format, Lint, Typecheck, Test, Browser Test, Build" \
  --required-check "CI / Windows Process Regression" \
  --required-check "CI / Migration Lineage" \
  --required-check "CI / Release Smoke" \
  --required-check "CI / Go Control Plane"
```

Review the complete plan and its `planSha256`. Applying it requires repeating the exact inputs, adding `--apply`, and
passing that digest through `--confirm-sha256`. The write path rechecks repository administrator authority, exact branch
HEAD and previous protection before writing; it then verifies the complete policy and unchanged HEAD. It treats 404 as
unconfigured, keeps 403 as an authorization failure, and is idempotent when the exact policy is already installed. The
write preflight refuses to remove stronger existing controls such as additional checks, multiple approvals, Code Owner
review, commit signatures, branch locks or push restrictions; reconcile those controls into an explicitly reviewed policy
instead of weakening them implicitly. Its receipt is `configuration-applied-not-release-approved`; applying protection
does not approve a candidate. Do not use a generic `CI` placeholder: required status-check names must match the real
workflow jobs.

After branch protection is active, bootstrap the API-supported Environment boundary with another read-only plan:

```bash
bun run stage6:environment:baseline -- \
  --repository owner/repository \
  --branch protected-release-branch \
  --expected-head 40-character-lowercase-commit \
  --reviewer User:numeric-id \
  --reviewer Team:numeric-id
```

Review its `planSha256`; applying requires the same inputs plus `--apply --confirm-sha256 sha256:...`. The command rechecks
repository administrator authority, exact branch HEAD and complete source-branch protection before creating or updating
`stage6-enterprise-ga`. It applies zero wait, two-to-six exact reviewers, self-review prevention and protected-branch-only
deployment, then reads everything back and rechecks the branch HEAD. It refuses to replace an existing wait timer or a
different reviewer set. GitHub's documented REST and GraphQL APIs do not expose an administrator-bypass write, so a new
Environment normally returns `environment-baseline-applied-manual-admin-bypass-required`. An authorized administrator
must deselect **Allow administrators to bypass configured protection rules** in GitHub Settings; rerun the command until
the idempotent receipt is `environment-baseline-ready-not-release-approved`. Do not proceed to candidate-scoped secret and
variable application while the manual-action flag remains true.

Before presenting the archive to the external GA authority, run
`scripts/stage6-final/prepare_final_ga_review.py` under
`docs/contracts/stage-6-final-ga-review-v1.md`. It rejects cross-candidate protected approvals/final assets, unfinished
checklists or notices, missing Audit request IDs, incomplete control inventories, approval-role reuse and invalid residual
risk records. The preparer computes every digest from a path-only draft, validates before publication and rolls back the
first output pair if the second cannot be published. Preserve its immutable manifest, receipt and sidecars with the archive.
Its eligibility flag is a consistency gate only; it does not replace the authority accepting the release.
