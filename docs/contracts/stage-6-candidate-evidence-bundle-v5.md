# Stage 6 candidate evidence bundle v5

Candidate v5 is the active internal-self-hosted release evidence authority. It retains the ten v4 control-receipt
families and adds an exact `{path, sha256}` reference to the copied
`synara.release-compatibility-matrix.v1` bytes.

The preparer emits `synara.stage6-candidate-evidence-bundle.v5`; the validation receipt is
`synara.stage6-candidate-evidence-bundle-validation.v5`. The validator reruns the source compatibility checker against
the captured matrix, records the bounded source file/byte inventory, and requires the matrix Migration tail to equal the
exact Release Evidence and Candidate projection. Matrix, manifest, Release Evidence and all ten receipts must use unique
paths.

The protected TypeScript release verifier and Control Plane independently require the matrix schema, version, permanent
`source-compatible-not-release-approved` assessment, non-zero digest, positive source inventory and exact Candidate
Migration tail. Migration 000163 enforces the same shape in PostgreSQL and SQLite. Active v4 Candidates must be replaced
or closed; terminal v2-v4 receipt bytes remain immutable audit history and cannot regain release authority.

This binding proves that the exact candidate used a source-current compatibility decision. It does not prove that the
previous build ran successfully on the target schema, that Worker canary/promote/rollback occurred, or that the complete
release was observed. Those execution results remain required in Final Review and the copied GA checklist.
