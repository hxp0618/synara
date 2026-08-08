# Stage 7 SDK Release Artifact Local Acceptance — 2026-08-05

## Scope and verdict

This report binds a non-publishing local rehearsal of the Polaris SDK release artifact path to source commit
`15aa63c973fe2cbe80f4fec84796b14547a10cfc`.

- npm staging, package build and archive allowlist verification: **PASS**
- Python wheel/sdist build and archive allowlist verification: **PASS**
- `twine check` for the exact wheel and sdist: **PASS**
- registry publication, OIDC attestation and protected-environment approval: **NOT PERFORMED**

The run used an isolated temporary directory and virtual environment. It did not authenticate to npm or PyPI,
create a release tag, upload an artifact, create a GitHub environment or publish a package.

## Runtime and commands

The host was `Darwin arm64` with Python `3.14.6`, Node `26.5.1`, npm `11.17.0` and Bun `1.3.14`.
The Python build environment pinned the same release tools as the workflow: `build==1.3.0`, `twine==6.2.0`
and `hatchling==1.27.0`.

The rehearsal executed the release workflow's material steps:

1. build `packages/polaris-sdk`;
2. create bounded npm staging with `scripts/prepare-polaris-sdk-release.ts`;
3. run `npm pack` against that staging directory;
4. build the Python wheel and sdist through `python -m build`;
5. run `python -m twine check` on both Python archives; and
6. run `scripts/verify_polaris_sdk_artifacts.py` against all three archives.

## Verified artifacts

| Kind | Version / file | Bytes | SHA-256 |
| --- | --- | ---: | --- |
| npm | `polaris-agents-sdk-0.1.0-beta.1.tgz` | 23,274 | `2a791fa9bafce528433b840f318d4e6139830b031e7068315ac55561bd1599bf` |
| Python wheel | `polaris_agents-0.1.0b1-py3-none-any.whl` | 13,687 | `8e1b3be8774a56e31e6cebd469f7745b63ba742c8fe1dfeb73b91d7556d85f4c` |
| Python sdist | `polaris_agents-0.1.0b1.tar.gz` | 19,453 | `8e5c30cc8c7667609a32ecda86e59e262082d9687a7208e8794c55f6bb4914b1` |

The npm verifier found exactly six allowlisted files. The Python verifier rejected generated interpreter caches,
checked the wheel metadata and required modules, and checked the sdist root and required source files.

## External boundary

At the time of this run, the public npm package endpoint for `@polaris-agents/sdk` and the PyPI JSON endpoint for
`polaris-agents` both returned HTTP `404`; that proves only that neither package was publicly readable at that
instant. It does not prove organization ownership or reserve either name.

The repository did not expose the `polaris-npm` or `polaris-pypi` protected environments, the
`POLARIS_NPM_TRUSTED_PUBLISHING_READY` / `POLARIS_PYPI_TRUSTED_PUBLISHING_READY` variables, or registry
credentials. GitHub also refused to dispatch `polaris-sdk-release.yml` from this branch because that new workflow
does not yet exist on the repository default branch. Consequently this local PASS cannot satisfy the protected
OIDC, provenance, registry ownership or actual-publication rows in the Stage 7 release checklist.
