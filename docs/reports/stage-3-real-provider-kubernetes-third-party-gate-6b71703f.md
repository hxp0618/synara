# Stage 3 Real Provider Kubernetes Third-Party Credential Gate at `6b71703f`

- Evidence date: `2026-07-18` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Clean gate commit: `6b71703f5770faa892c3bc5594f5d75ee3638de2`
- Diagnostic-report commit: `8883e037`
- Aggregate run: `stage3-provider-kubernetes-release-dbc890a0-7aa7-4edb-8bd8-811f26192584`
- Result: **FAIL CLOSED; THE CONFIGURED THIRD-PARTY PROVIDER PROFILES DO NOT QUALIFY FOR THE FULL PRODUCT GATE**

## Evidence boundary

The consolidated gate loaded two controlled API keys and two optional Base URLs from one operator-owned mode-`0600`
environment file. Neither values nor operator environment-variable names were persisted. Four child runs shared one
clean-SHA Worker image and each used an owned disposable Kind cluster:

```text
Codex product   + Codex controlled failure
Claude product  + Claude controlled failure
```

The gate proves Credential/Base URL injection, shared-image consensus, real Provider startup where reported, exact
cluster cleanup and Secret scanning. A failed product capability is not converted to `unsupported` and does not
close the Kubernetes release gate.

## Aggregate result

All reports shared Git SHA `6b71703f5770faa892c3bc5594f5d75ee3638de2`, Capability Catalog SHA-256
`742a7eef08fde2394438fb0a9ee008cf1d062576d3b884709c291ffc17e9bdeb` and Worker image ID
`sha256:1a1452f69ea610ff8819a18d9a8bc7fe94ac212364c7bc7edb5b945f817880f0`.

| Provider | Matrix  | Result | Passed | Failed | Finding                                                                                              |
| -------- | ------- | ------ | -----: | -----: | ---------------------------------------------------------------------------------------------------- |
| Codex    | product | fail   |     10 |     13 | Baseline real Turn passed, but the approval-required Turn completed without an approval interaction. |
| Codex    | failure | pass   |     16 |      0 | Authentication, rate limit, scoped Host crash and Cursor expiry/restart all passed.                  |
| Claude   | product | fail   |      9 |     14 | Initial real Turn ended as `provider_unavailable`.                                                   |
| Claude   | failure | fail   |      9 |      7 | Initial real Turn failed before the independent failure cases.                                       |

The aggregate child-output scan covered `37` files and `5,511,658` bytes with zero findings.

## Controlled diagnostic reruns

Two minimal Local runs on clean commit `8883e037` removed Kubernetes from the diagnosis while preserving the same
controlled Credential profiles:

### Claude baseline

- Run `stage3-provider-acceptance-dacaafb1-5019-4ed8-9209-18445b44c5ef`.
- Result: fail after `619,353 ms`.
- Stable failure code: `provider_unavailable`.
- Redacted failure message: `Claude Agent SDK API request failed with HTTP 502.`
- Cleanup and Secret scan: pass, zero findings.

This reproduces the Kubernetes behavior locally and makes the configured third-party Claude endpoint, not the
Kubernetes Target, the current blocking dependency.

### Codex approval

- Run `stage3-provider-acceptance-4f66cc38-112e-495c-83df-629d593b73fe`.
- Result: fail after `32,244 ms`.
- Baseline text Turn completed successfully.
- The approval-required Turn emitted only `reasoning` items and completed without `command_execution` or a durable
  approval request.
- Cleanup and Secret scan: pass, zero findings.

This proves the configured third-party Codex endpoint can serve text and the complete controlled failure matrix but
does not currently demonstrate the tool/approval capability required by the frozen Tier 1 product boundary.

## Cleanup

All four disposable Kind clusters were deleted. The shared gate image was ownership- and ID-verified before exact
deletion. `kind get clusters` returned no clusters and the image lookup returned no matching image after the run.
No broad cleanup command was used.

## Report integrity

The ignored raw aggregate directory is
`.tmp/stage3-real-provider-kubernetes-release-6b71703f-formal/`.

| Report                        | SHA-256                                                            |
| ----------------------------- | ------------------------------------------------------------------ |
| Kubernetes aggregate JSON     | `94104ce2ad4dc9d891037e4be318c280824edfc10a5acdc5b813bf2f13de4981` |
| Kubernetes aggregate Markdown | `45a6eb960a9d88cef862f8b9285f636a11daaa191e41083cf611df6567b4202f` |
| Claude Local JSON             | `a826d05cafa13d65e5e9fb407ce804554e965ea4e20940715a5012b5d9b4c8f1` |
| Claude Local Markdown         | `fee84f6384aa1e4c16d1da3f8357641958313644d6b328e7a3b678659b0389cd` |
| Codex approval Local JSON     | `bd68beec97a65c2b38c5447ec5d63311e773140308c15afa2ce7a70e02c09da6` |
| Codex approval Local Markdown | `2c903d5cc894f9ac20f6296b955be080f2189d0acfc0628516b86c4e1795c299` |

## Required operator decisions

The gate can be rerun without code changes after both conditions are met:

1. provide a Claude API profile whose Anthropic-compatible streaming request completes without HTTP 502;
2. provide a Codex API profile/model that supports Responses API command/tool calls and produces an approval request
   under `approval-required` mode.

If either third-party profile is intentionally text-only or reduced-capability, it must remain an explicitly lower
support profile and cannot be used as evidence for the frozen Codex/Claude Tier 1 release boundary. No database DDL
changed; the forward migration boundary remains `000041_diff_artifact_kind.sql`.
