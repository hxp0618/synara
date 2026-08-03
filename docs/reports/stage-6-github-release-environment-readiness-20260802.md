# Stage 6 GitHub release-environment readiness — 2026-08-02

## Scope and assessment

This report records a read-only inspection of the GitHub repository that is configured as this checkout's `origin`. It
checks whether that repository can currently execute the protected, signed Stage 6 Enterprise GA release workflow. It does
not inspect secret values, change repository settings, dispatch a workflow or approve a release.

Assessment: `github-release-environment-not-ready`.

Observed at: `2026-08-01T22:40:22Z`.

## Repository identity

- Explicit query target: `hxp0618/synara`
- Repository URL: `https://github.com/hxp0618/synara`
- Repository shape: public fork of `Emanuele-web04/synara`
- Default branch: `main`
- Local branch: `codex/saas-tenancy-user`
- Local HEAD at inspection: `a7ea05fd0f98ae4d15ac5a14a67b02914875ce9f`
- Remote feature-branch HEAD: `3ed09053e81a42b43e716a5c3cdf943e8a8319b0`
- Local branch relation: 15 commits ahead of `origin/codex/saas-tenancy-user`, with additional uncommitted files

The GitHub queries named `hxp0618/synara` explicitly. This avoids accidentally treating the upstream repository selected
by generic `gh repo view` discovery as the writable release target.

## Read-only observations

| Requirement | Observed state | Release consequence |
| --- | --- | --- |
| Repository Actions secrets | Empty name list | No macOS Developer ID/notary or Windows signing inputs are available to the workflow |
| Repository Actions variables | Empty name list | Required Stage 6 publication/finalization controls are not configured |
| GitHub Environments | None | The protected `stage6-enterprise-ga` approval boundary does not exist |
| `main` branch protection | GitHub API returned `404 Branch not protected` | Protected-source requirements cannot pass |
| `release.yml` workflow runs | None | No remote signed candidate or release-run evidence exists in this fork |
| Current Stage 6 source | Ahead and dirty locally | The remote workflow does not contain or build the current working-tree implementation |

Secret values were neither requested nor available. `gh secret list` exposes names and timestamps only; the observed list
was empty.

## Minimum path to a real candidate

1. Decide that `hxp0618/synara` is the authoritative release repository, or explicitly select a different repository and
   update every candidate/repository identity accordingly.
2. Reconcile, review, commit and push the intended Stage 6 source boundary; do not publish from the current dirty tree.
3. Protect the release source branch with administrator enforcement, stale-review dismissal, required review and strict
   status checks, while disabling force-push and deletion.
4. Configure the required GitHub Environment with no administrator bypass, prevent self-review and the separated reviewer
   set expected by the protected release workflow.
5. Configure the macOS Developer ID/notary and Windows signing secrets plus the exact Stage 6 release variables. Use
   `bun run stage6:desktop:mac:preflight` in the macOS lane; never copy secret values into evidence.
6. Produce one immutable candidate receipt and apply its exact protected-environment configuration through the repository's
   fail-closed Stage 6 environment tooling.
7. Dispatch the first-attempt release workflow for that exact candidate, then collect signed artifact provenance,
   attestations, approval history and the final asset set before any publication decision.

Until those external repository controls exist, local source gates may be reported as passed but the release environment
must remain not ready and no Stage 6 GA claim is valid.

The observation is now reproducible with:

```bash
bun run stage6:github:readiness -- --repository hxp0618/synara --branch main
```

The command emits `synara.stage6-github-release-readiness.v1`, requests configured secret names without secret values,
lists variable names, and verifies only the non-secret `SYNARA_FINALIZE_RELEASE=1` control when it is present. The state
also rejects any configured `SYNARA_ALLOW_UNSIGNED_WINDOWS_RELEASE` version exception for Enterprise GA. The state
recorded above has neither variable, so no variable value was requested and the final assessment remains
`github-release-inputs-incomplete`.
