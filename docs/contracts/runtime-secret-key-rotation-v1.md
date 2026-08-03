# Runtime Secret key rotation v1

## Scope

This contract covers the AES-GCM key configured by `SYNARA_PROVIDER_CURSOR_KEY`. Despite its historical name, it protects:

- authenticated native Provider resume cursors in `agent_sessions`; and
- encrypted SSH, Docker and Kubernetes Execution Target configuration in `execution_targets`.

It is separate from the envelope KMS used for Provider Credentials and enterprise identity. The two keyrings, commands and
receipts must never share key material.

## Compatibility and keyring

The original ciphertext formats contain no Key ID. `SYNARA_PROVIDER_CURSOR_KEY_ID` explicitly enables the new writer; new
Target configuration uses a keyed runtime-secret envelope and new Provider Cursors use keyed cursor envelope v3. The Key ID
is authenticated as AES-GCM additional data. Readers continue to support legacy Target ciphertext and cursor v2.

Up to eight decrypt-only keys are configured without embedding key bytes:

```text
SYNARA_PROVIDER_CURSOR_KEY_ID=runtime-v2
SYNARA_PROVIDER_CURSOR_KEY=<new key from secret manager>
SYNARA_PROVIDER_CURSOR_KEY_OLD_V1=<old key from secret manager>
SYNARA_PROVIDER_CURSOR_DECRYPT_KEYS_JSON=[{"keyId":"runtime-v1","keyEnvironment":"SYNARA_PROVIDER_CURSOR_KEY_OLD_V1"}]
```

Duplicate IDs, duplicate key material, unknown/missing key environments, malformed JSON and keys other than 32 bytes fail
startup. Because older binaries cannot read keyed envelopes, binary compatibility must be rolled out with the Key ID unset
before any replica enables the named keyring.

## Inventory and execution

`control-plane-metadata rekey-runtime-secrets` is read-only by default. It decrypts each non-empty Target configuration and
each `usable` Provider Cursor to identify the real key used; it never trusts an unverified stored Key ID. The report separates
converged primary envelopes, pending legacy/fallback envelopes, unreadable envelopes and non-usable cursors.

`--execute --operator REF` creates an immutable run and re-encrypts plaintext under the named primary in bounded UUID-ordered
batches. Each resource transaction writes ciphertext SHA-256 evidence, compare-and-swaps the exact old ciphertext/key label,
updates only ciphertext plus Key ID, and appends `security.runtime_secret_reencrypted` for Tenant-owned resources. A
platform-owned Target has no customer Tenant audit stream, so its immutable global entry is the authority.

An interrupted run is resumed with `--resume-run-id` and the same operator reference. A receipt is accepted only when entry
counts match and every non-empty Target configuration plus every usable Provider Cursor names the run primary. Runs, entries
and receipts reject update/delete; a completed run rejects new entries. The receipt digest covers ordered entry identities,
old/new key labels and old/new ciphertext hashes, never plaintext or ciphertext.

## Safety and retirement

- Missing fallback keys, authentication failures, Cursor binding failures and stored/envelope Key ID mismatches fail closed
  before creating a run.
- Concurrent normal Cursor writes win the compare-and-swap and are re-inspected on the next convergence pass.
- Provider Cursor source Execution, Generation, history sequence and business payload are preserved byte-for-byte.
- Execution Target configuration JSON is preserved byte-for-byte after decrypt/re-encrypt; resource timestamps are unchanged.
- Non-usable Cursor ciphertext is counted but excluded from retirement convergence because product paths cannot consume it.
- The old key is removed only after a second dry-run, representative real client paths and all-replica startup/read checks.

Local/PostgreSQL tests and a source receipt prove implementation behavior, not that a production rotation was exercised.
