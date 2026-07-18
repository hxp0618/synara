# Stage 3 Controlled Provider Model-Environment Targeted Acceptance at `98d0d2cc`

- Evidence date: `2026-07-18` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Clean acceptance commit: `98d0d2cc17078dc3a0a891b39cd4cfd137f13e86`
- Result: **PASS for the previously failed controlled Codex and Claude blockers**

## Evidence boundary

The two targeted Local real-Provider runs loaded the controlled API Key, Base URL, and Model from the
operator-owned mode-`0600` `~/.synara-acceptance-env` file. The file exports the configured variables so child
processes can resolve them. Neither Credential/Base URL values nor Model environment-variable names were persisted
in reports. Resolved Model identifiers are non-secret and were recorded for exact-provider validation.

This is a targeted blocker rerun, not a replacement for the full Docker, Kubernetes, or SSH product/failure
matrices. No database schema or DDL changed.

## Codex controlled profile

- Run: `stage3-provider-acceptance-06a45a2b-5e0b-4686-8fe0-935cbdbec303`
- Duration: `52,953 ms`
- Model: `gpt-5.6-sol`
- Authentication: controlled `apiKey`
- Controlled Base URL: configured
- Result: `14` passed, `0` failed

The required baseline and the previously blocked product interactions passed:

| Case                                | Result |
| ----------------------------------- | ------ |
| `real-provider.turn-1`              | pass   |
| `real-provider.approval-resolution` | pass   |
| `real-provider.steer-active-turn`   | pass   |
| `real-provider.turn-2-continuity`   | pass   |
| `security.output-secret-scan`       | pass   |

The run proves that the resolved custom Model reaches the controlled third-party Codex endpoint and that the shared
collaboration-mode developer instructions preserve approval and steer behavior.

## Claude controlled profile

The original `claude-sonnet-4-6` profile reached the configured endpoint but returned HTTP `400` with the redacted
provider diagnostic `antigravity auth missing project_id`. The endpoint Model catalog contained the identifier, so
the failure was profile-route compatibility rather than missing Synara Credential/Base URL/Model propagation.
`claude-opus-4-6-thinking` was available but its individual quota was exhausted. A minimal Anthropic Messages probe
for `claude-fable-5-dd-los-6.5-tpg` returned HTTP `200`, so that verified custom Model was selected for the acceptance
rerun.

- Run: `stage3-provider-acceptance-daffacab-1b86-4e5c-ab0c-1a307a56b3f9`
- Duration: `23,555 ms`
- Model: `claude-fable-5-dd-los-6.5-tpg`
- Authentication: controlled `apiKey`
- Controlled Base URL: configured
- Result: `12` passed, `0` failed

| Case                              | Result |
| --------------------------------- | ------ |
| `real-provider.turn-1`            | pass   |
| `recovery.control-plane-restart`  | pass   |
| `real-provider.turn-2-continuity` | pass   |
| `security.output-secret-scan`     | pass   |

This closes the prior controlled Claude baseline `provider_unavailable` blocker with an endpoint-supported custom
Model while retaining the existing Provider Credential and Base URL boundary.

## Report integrity

The ignored raw reports remain under `.tmp/stage3-provider-acceptance-results/`.

| Report          | SHA-256                                                            |
| --------------- | ------------------------------------------------------------------ |
| Codex JSON      | `e099d6ae0d91098bf150f11c8aa7b6add2459edccc7e290c7227675691b95f2f` |
| Codex Markdown  | `8ce855fe41de95242a347456cc1c3bb0a79bdd2c2460251e01e6c29458597ffc` |
| Claude JSON     | `d4f4edc3ee5319cbc342adb5bcdd33efd2fb24f2469cbc16c110dfe65e5255ad` |
| Claude Markdown | `b92a5b0e17d69d4a53fec9af0e176b8bd7d7aaecf806acca2df3ba9525c22f74` |

Both report directories were scanned for the respective Model environment-variable names and contained none. Each
runner also passed its built-in output Secret scan.
