# Stage 6 release evidence collector

The collector pins a clean Git commit and `bun.lock`, five self-hosted service artifact digests, all four native Desktop artifact
digests, every SQL migration checksum, the exact environment identity and HTTPS origins, deployment Regions and explicit
control-evidence files into one JSON manifest plus a SHA-256 sidecar. It never marks a control passed; a human or external
auditor must assess the collected evidence.

Example:

```bash
python3 scripts/stage6-evidence/collect_release_evidence.py \
  --release stage6-rc1 \
  --control-plane-image registry.example/synara-control-plane@sha256:... \
  --worker-image registry.example/synara-worker@sha256:... \
  --provider-host-image registry.example/synara-provider-host@sha256:... \
  --web-artifact sha256:... \
  --admin-artifact sha256:... \
  --desktop-artifact macos-arm64=sha256:... \
  --desktop-artifact macos-x64=sha256:... \
  --desktop-artifact windows-x64=sha256:... \
  --desktop-artifact linux-x64=sha256:... \
  --environment-class production \
  --environment-id production/stage6-rc1 \
  --control-plane-base-url https://control.example.com/v1 \
  --web-base-url https://app.example.com \
  --admin-base-url https://admin.example.com \
  --region cn-east-1 \
  --evidence cc6.1=docs/reports/approved-access-review.md \
  --evidence cc7.2=docs/reports/restore-drill.md \
  --output docs/reports/stage6-rc1-evidence.json
```

Run it only from an exact clean release commit. Evidence paths must be regular, non-symlink files inside the repository;
do not pass logs or reports containing credentials, Provider payloads, prompts or customer content.
The collector rechecks HEAD and the clean worktree after hashing and before publication, so a concurrent source change
cannot silently retain `source.clean=true`. The manifest and SHA-256 sidecar are published as exclusive `0600` files;
existing paths are never overwritten, and a sidecar publication failure rolls back the new manifest. Use a new candidate
directory for every attempt.

After the external control receipts are available, use this v2 release manifest as the canonical identity input to
`bun run stage6:candidate:prepare -- ...`. The preparer computes the v3 bundle references and validates all ten receipts;
do not manually copy its commit, environment, origins or digests into a second authority.
