# Stage 6 incident-communication exercise validator

The validator checks a live paging and internal Status Board exercise against the Stage 6 incident-response contract. It
records role separation, acknowledgement/internal-update targets, component coverage, employee notification delivery, recovery
observation and seven evidence hashes while preserving missed exercises as failed receipts.

For a real exercise, put a path-only draft and all seven evidence files below one protected root. The draft uses
`synara.incident-communication-exercise-evidence-draft.v2`; every `evidence` value is a relative path string. Materialize,
validate and atomically publish with:

```bash
bun run stage6:incident:prepare -- \
  --evidence-root /secure/incident-exercise \
  --draft incident-draft.json \
  --manifest-output incident-manifest.json \
  --receipt-output incident-receipt.json

python3 -m unittest discover -s scripts/stage6-incident -p 'test_*.py'
```

The preparer uses stable bounded non-symlink reads, rejects duplicate JSON fields and obvious credential material, computes
all seven SHA-256 references, and publishes only after the full incident validator succeeds. The manifest/receipt and both
sidecars are exclusive `0600` files; receipt publication failure rolls back the manifest pair. A failed or tabletop
exercise is preserved as an ineligible receipt rather than upgraded or discarded.

The lower-level `stage6:incident:validate` command remains available for an already materialized manifest.

Keep rota contacts, provider configuration and detailed incident material in the protected evidence store. The receipt
always states `evidence-validated-not-operations-ready`; deployed board/paging configuration and human approval remain required.
The receipt and SHA-256 sidecar are exclusive `0600` files produced by the shared Stage 6 immutable publisher. Use a new
attempt path for every exercise; a collision or sidecar failure cannot replace or half-publish a prior result.
