# Stage 3 OrbStack Kubernetes Fixture and Failure Matrix at `6b71703f`

- Evidence date: `2026-07-18` Asia/Shanghai
- Branch: `codex/saas-tenancy-user`
- Clean implementation commit: `6b71703f5770faa892c3bc5594f5d75ee3638de2`
- Gate run: `stage3-provider-acceptance-a083553a-0574-4ea9-b08c-96d2f96d516f`
- Result: **PASS FOR THE DETERMINISTIC ORBSTACK KUBERNETES SLICE; REAL-PROVIDER AND PRODUCTION GATES REMAIN OPEN**

## Evidence boundary

The shared Stage 3 Acceptance Runner executed the deterministic Provider Host Protocol 2.1 fixture through the real
user API, Control Plane, reusable `orbstack` Kubernetes Target, execution-pinned Pod, agentd and Worker Protocol. The
run does not use a real Codex App Server or Claude Agent SDK and does not prove multi-node scheduling, production
SLA, registry rollout or production KMS admission.

## Stable reusable-context route

The generated `k8s.orb.local` route was intermittently timing out while the loopback listener remained healthy. The
Runner now accepts a credential-free HTTPS API origin override plus a TLS server name override and applies both as
per-process `kubectl` flags. The operator kubeconfig and active Context were not modified. The Control Plane Target
used the same explicit API origin with the Context CA; the report records the route but no Kubernetes Credential.

This clean run used:

- Context `orbstack`;
- API origin `https://127.0.0.1:26443`;
- TLS server name `k8s.orb.local`;
- shared local image transport with `imagePullPolicy=Never`;
- Capability Catalog SHA-256 `742a7eef08fde2394438fb0a9ee008cf1d062576d3b884709c291ffc17e9bdeb`.

## Result

The report contains `22` passing cases, `0` failed/skipped cases and one explicit unsupported case. The unsupported
case is reusable single-node Node Drain because separate node-mutation authorization was not granted. Required
coverage passed for:

- target preparation, Control Plane startup, dev login and Target provisioning;
- Worker discovery and Pending Approval Pod-loss Generation recovery;
- approval, text/tool/usage, generated Artifact, segmented Terminal and structured input paths;
- malformed, oversized and crashed Provider Host recovery;
- eight-second Worker-only network interruption;
- exact `policy/v1` Pod Eviction;
- isolated local-image Canary and baseline continuity;
- Control Plane restart, second Turn continuity, exact cleanup and output Secret scan.

Total duration was `860,719 ms`.

## Cleanup and security

The Runner removed the exact three owned Namespaces, ClusterRole, ClusterRoleBinding, main Worker image, Canary
image, Control Plane process and isolated state. Independent post-run queries found no resource with owner
`b87873d248544552ae91` and no owned image. `broadCleanupUsed=false`.

The output scan covered `10` JSON, Markdown, log and text files totaling `891,285` bytes, registered six known
Secret canaries and found zero private-key, cloud-key, GitHub-token or OpenAI-style key patterns.

## Report integrity and DDL

The ignored raw output directory is `.tmp/stage3-kubernetes-orbstack-fixture-6b71703f-formal/`.

| Report   | SHA-256                                                            |
| -------- | ------------------------------------------------------------------ |
| JSON     | `9f25172a764309b194ee0ca6bd66a83c463c16b3bc8722c32c916a5ee768ca92` |
| Markdown | `260afb75ee4391727108e08a8a422a17183a861804cbde3ec21543922f7dc327` |

No database DDL changed. The checked-in forward migration boundary remains
`000041_diff_artifact_kind.sql`.
