# `@synara/cloud-agent-distribution`

Pinned Cloud Agent distribution shared by Synara and T3 Code. Its JavaScript
factory explicitly registers the allowlisted Providers; it also exports the
stdio client, deeply immutable source manifest, and the Protocol v2 envelope
schema from `@synara/cloud-agent-distribution/schemas`. It does not modify
either host during installation.

The `cloud-agent-runtime` bin is emitted as a single bundled executable so a
host-side SHA-256 check covers the actual Runtime implementation rather than an
external-import shim. Its Provider allowlist is declared by `manifest.json` and
must match the explicitly composed Runtime registry before a candidate is
accepted.

Hosts start the NDJSON protocol with `cloud-agent-runtime --protocol-v2`. The
bin accepts only the Codex and Claude plugins pinned by the manifest; an
unknown or disabled Provider returns `provider_not_installed`.

Release validation is tarball-first:

```sh
node scripts/cloud-agent-release-smoke.ts --output-dir /new/candidate/directory
```

The check builds and packs all seven public packages, rejects local dependency
protocols and private Synara dependencies, installs the tarballs into a fresh
Node 24 project, exercises ESM, CommonJS, schemas, and the real bin, and then
verifies that every tarball SHA-256 is unchanged. `--allow-dirty` exists only
for local source validation; a release candidate requires a clean tree.
