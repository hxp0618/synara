# Stage 5 OpenCode/Kilo pure-mode local acceptance — 2026-07-29

## Scope

This report proves the local executable-config slice only:

- every Synara-started OpenCode/Kilo CLI path requests pure mode through both the CLI and environment;
- an old or unparseable CLI version fails before the Provider server opens the workspace;
- an already-running external server is not accepted for server-authored untrusted content because Synara cannot
  attest its startup profile.

It does not prove managed Kubernetes isolation, native tool-result provenance, or safety of a third-party binary that
impersonates the official CLI.

## Repository state

- Branch: `codex/saas-tenancy-user`
- HEAD: `0d400eaa6e46f1fc16dcae5010fca79770323cae`
- Worktree: dirty before and during this slice (184 porcelain entries at capture time); no staging, commit, or push was
  performed by this acceptance run.

## Audited upstream source

| Provider | Release | Source commit | Audited behavior |
| --- | --- | --- | --- |
| OpenCode | `v1.15.11` | `d2bd7eaad54bf39de04bf6e279d5953bd1666574` | `--pure` sets `OPENCODE_PURE=1`; the server plugin loader selects an empty external-plugin list in pure mode |
| Kilo | `v7.4.16` | `f80ebff83b32550333da7c50c91c4755e4524d0d` | `--pure` sets `KILO_PURE=1`; the server plugin loader selects an empty external-plugin list in pure mode |

Both source trees also condition project config discovery on their provider-specific
`*_DISABLE_PROJECT_CONFIG` flag. Pure mode retains built-in Provider plugins; the claim here is only that external
repository/user/global plugins are excluded.

## Official binary integrity

The Darwin arm64 release archives were downloaded from the two official GitHub releases and matched their published
SHA-256 values:

```text
f82f0bdb285836971c63677dd18d7005dbaf46bfd04e22383905bf8453f4db8c  opencode-darwin-arm64-v1.15.11.zip
09617440c98987418401c719f2884b4f5b35cac0518c8fbaf88f027ee7f6a704  kilo-darwin-arm64-v7.4.16.zip
```

Probe host: Darwin 25.5.0, arm64. XDG config/data/state and provider config directories were isolated under a fresh
`/tmp/synara-provider-pure-probe.*` root. Project config, update checks, and remote model catalog fetches were disabled;
no Provider credential was supplied or read.

## Runtime observations

The exact version probes used by the Synara runtime succeeded:

```text
$ opencode --pure --version
1.15.11

$ kilo --pure --version
7.4.16
```

With the corresponding `OPENCODE_PURE=1` / `KILO_PURE=1` and project-config kill switches, both official CLIs reported
the same runtime state:

```text
plugins:
external plugins disabled (--pure)
```

The first isolated run performed only each CLI's one-time empty-database migration inside the temporary XDG data
directory.

## Repository verification

- `opencodeRuntime.test.ts`: 26/26 PASS, including version-floor rejection before server startup.
- `ProviderHealth.test.ts`: 92/92 PASS, including the `--pure --version` health gate.
- `security/untrustedContent.test.ts`, `decider.projectScripts.test.ts`, and
  `ProviderCommandReactor.test.ts`: external runtime admission is rejected before Provider start or subagent steer.
- `AutomationService.test.ts`: external runtime admission is rejected at create, update, and persisted run backstop.
- Combined directly affected test run: 478/478 PASS.
- Server build: PASS.
- `git diff --check`: PASS.

The full Server suite initially exposed one stale Stage 5 assertion in `mcpInjection.test.ts`: it expected a missing
`shell_environment_policy` to remain unchanged even though the current credential-isolation implementation correctly
creates the token exclusion immediately. The assertion was updated to the implemented fail-closed contract and its
12/12 focused test now passes. The final machine-readable full Server rerun completed with 3,106 passed, 7 skipped,
0 failed (790 assertion suites), and the Server build passed.

## Remaining boundary

- OpenCode/Kilo remain `policy-only` for native tool-result provenance. Their public model-preconsumption hook is an
  external plugin hook, and pure mode deliberately removes external plugins rather than promoting one to Host authority.
- Human-authored local turns may still use a configured external server; server-authored `external-mcp`, `synara-mcp`,
  and `automation` turns may not.
- No local result in this report substitutes for the required managed Provider × Ready Worker matrix.
