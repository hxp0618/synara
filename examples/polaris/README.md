# Polaris TypeScript examples

These server-side examples exercise the public `@polaris-agents/sdk` surface. They intentionally do not place
API Keys in browser code and do not call internal Worker or Provider Host protocols.

Set the shared environment first:

```sh
export POLARIS_API_KEY="syna_sa_..."
export POLARIS_BASE_URL="https://control.example.com"
export POLARIS_PROJECT_ID="..."
```

Then run one workflow from the repository root:

```sh
bun run examples:ci-fix

PULL_REQUEST_NUMBER=42 PULL_REQUEST_HEAD_SHA=abc123 \
  bun run examples:pr-review

bun run examples:batch-migration -- examples/polaris/typescript/repositories.example.json
```

The CI and pull-request examples derive stable idempotency keys from the external job identity. The batch
migration example persists a local sequence checkpoint after every event and can safely skip completed jobs on
restart. A production integration should store checkpoints in the same durable transaction as its own side
effects.
