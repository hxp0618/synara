# Registry production blueprint

This directory is a minimal baseline for a private OCI registry that backs the
production Worker signing flow.

- TLS only
- Basic auth via `htpasswd`
- delete disabled
- PVC-backed storage

Files:

- `distribution-config.example.yml`: example `distribution` configuration
- `retention-policy.json`: checked-in exact Registry runtime image, production immutability, archive retention, and
  GC boundary

Notes:

- `distribution` does not provide first-class immutable tags. Keep delete disabled
  and promote release artifacts by digest; enforce tag immutability in CI and
  through your release process.
- Run the Registry container only from the exact tag-plus-digest `runtimeImage` in `retention-policy.json`. The
  production gate verifies the requested container image reference, runtime image ID, and matching RepoDigest;
  mutable tags and alias repositories fail closed.
- `retention-policy.json` is the production evidence contract: keep release evidence
  for the declared horizon, archive digests/signatures/attestations before GC, and
  run garbage collection only after that archive step is complete.
- Mount the TLS keypair at `/certs`, the `htpasswd` file at `/auth/htpasswd`, and a
  persistent volume at `/var/lib/registry`.
