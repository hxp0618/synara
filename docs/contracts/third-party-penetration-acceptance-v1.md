# Third-party penetration acceptance v1

## Promise boundary

This contract defines the Stage 6 third-party penetration-test evidence gate. It does not authorize testing, attest that an
assessor is independent, or declare a release secure. A local scanner, route inventory, unit test, internal red-team run,
fixture manifest, or validator receipt cannot close the GA checklist. The copied release checklist must retain the signed
assessor report, Security review and any residual-risk approval.

The engagement starts only after the exact Stage 5 supported deployment surface is accepted and revalidated on the target.
The current self-managed Kubernetes acceptance does not imply managed-cloud IAM, Region, admission or network controls.
The manifest records the Stage 5 accepted commit and matching deployment profile; release approvers must verify ancestry
and the target-specific evidence rather than infer it from a Boolean.

## Release and assessor identity

The test binds one full release commit to the Web, Control Plane API, Worker runtime and Provider Host artifact digests.
Production or production-like is release-eligible; staging and fixture runs are retained as engineering evidence only. A
release engagement requires a contracted third party that declares independence and no conflict of interest. The manifest
uses bounded references rather than assessor personal data; the signed scope and independence statement remain protected
evidence files.

Testing requires written authorization, named emergency contacts, source address/time windows, stop conditions and a data
handling agreement. The assessor must not place credentials, exploit payloads, prompts, customer content or detailed
vulnerability narratives in the public receipt.

## Minimum scope

Every engagement explicitly records whether it tested all of these areas:

- cross-Tenant authorization and nested resource substitution;
- SSRF and metadata/internal-network reachability;
- command and argument injection across Git, SSH, Provider and automation inputs;
- Workspace, archive, object-key and download path traversal;
- release dependency, image, SBOM, signature and admission supply-chain boundaries;
- Worker/Provider container escape, credential containment and host isolation.

The minimum methodology combines manual business-logic testing, OWASP Web/API testing and cloud/runtime testing. Each of
Web, Control Plane API, Worker runtime and Provider Host must bind to and test the candidate artifact. A declared `not-tested`
area or asset is preserved in the receipt but makes the bundle ineligible for release review.

## Findings policy

Every finding has an opaque ID, severity, affected release assets, discovery/review timestamps and one of `open`,
`remediation-in-progress`, `remediated-verified` or `risk-accepted`. Detailed exploit material stays in the assessor report.
`remediated-verified` requires an assessor retest. Critical findings must be remediated and verified; risk acceptance does
not make a Critical finding eligible. A High finding may be remediated and verified or accepted for at most 30 days with
distinct Security and business approvers plus named compensating controls. Open or in-progress High/Critical findings make
the bundle ineligible, but the validator still emits a failed-run receipt so the result cannot be discarded.

Medium and lower findings remain subject to the release risk decision, backlog ownership and normal expiry policy. The
validator's High/Critical Boolean is not an instruction to ignore them.

## Evidence and validator semantics

The path-only draft contains nine distinct regular-file paths: Stage 5 completion, target revalidation, signed scope,
assessor independence, execution log/export, final report, finding register, remediation/retest evidence, and risk-acceptance
record. `prepare_penetration_evidence.py` derives their SHA-256 values from stable bounded reads and atomically publishes the
materialized manifest and receipt. Use an access-controlled evidence store; do not commit sensitive reports to the
repository. Duplicate JSON fields, credential material, symlinks, traversal, duplicate files and hash drift fail closed.

`scripts/stage6-penetration/validate_penetration_evidence.py` independently validates the exact materialized schema,
timestamps, artifact/scope coverage,
finding transitions, dual approval and evidence hashes. Its receipt always says
`evidence-validated-not-penetration-passed`. `eligibleForHumanGateReview=true` means only that the declared bundle meets the
machine-checkable prerequisites; Security and release approvers still own the control decision.
