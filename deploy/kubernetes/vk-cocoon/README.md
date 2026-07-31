# Synara vk-cocoon patch set

Synara's `microvm-isolated-v1` integration requires two vk-cocoon behaviors
that are not present in upstream v0.3.6:

- intentional VM deletion must not race the `VMGone` recovery handler; and
- Cloud Hypervisor guests must opt into shared memory before boot so the
  supervisor can attach the post-boot virtiofs workspace.

The mailbox patches under `patches/` preserve the two reviewed commits from the
external vk-cocoon integration checkout. They are based on the immutable
upstream v0.3.6 commit `de972e73a711b6e147a2042b8f1a92e58356b3bc`
and produce tree `2872e3b14f3cf516940546219e9dffb388d9fd0a`.

Prepare and verify a source tree with:

```bash
./scripts/prepare-vk-cocoon-source.sh /absolute/output/path
```

The output directory must not already exist. The script clones the pinned
upstream commit, applies the mailbox patches with `git am`, verifies the
resulting tree hash, and runs `go test ./...`. Image publication remains an
explicit release operation; deployments must pin the resulting vk-cocoon image
by digest.
