# Stage 3 Provider Runtime / Remote Worker 发布检查单

每次发布复制本检查单，并记录 Commit、不可变镜像 Digest、数据库 Migration、执行人、时间和证据链接。
未满足项必须保持未勾选，不能用 deterministic fixture、单一 Target 或静态代码检查替代真实发布证据。

当前最新的 consolidated Local 证据见
`docs/reports/stage-3-real-provider-local-release-gate-253052aa.md`。它关闭同一 clean SHA 上真实
Codex/Claude 的 Local product 与 failure slice，但仍明确保持四 Target、Registry rollout、并发和 soak
`RELEASE GATE OPEN`，不能直接作为 SSH、Docker、Kubernetes 或生产环境发布批准。
真实 SSH、Docker 与 disposable-Kind Kubernetes product/failure aggregates 已分别由 clean `14f7dd2d`、
`b1c52bae` 与 `3c523417` 通过；证据见
`docs/reports/stage-3-real-provider-ssh-release-gate-14f7dd2d.md`、
`docs/reports/stage-3-real-provider-docker-release-gate-b1c52bae.md` 与
`docs/reports/stage-3-real-provider-kubernetes-release-gate-3c523417.md`。它们关闭各自已执行的受控 Provider
profile slice，不关闭同一 release commit 的四 Target 重跑、生产多节点/Registry/KMS、并发/Retention/load 或 soak。
Deterministic Local long-Session/restart/pagination mechanics 的最新证据见
`docs/reports/stage-3-local-fixture-soak-6e866a30.md`，同样不能替代真实 Provider 或 production soak。
Deterministic managed Docker multi-Provider/multi-Session overlap mechanics 的最新证据见
`docs/reports/stage-3-docker-fixture-concurrency-eeb7a2f1.md`；它不替代真实 Codex/Claude、remote Target、
load 或 production concurrency。Deterministic Local active-Execution Retention fencing 与 post-terminal physical
cleanup mechanics 的最新证据见 `docs/reports/stage-3-local-fixture-retention-concurrency-c27914da.md`；它不替代
真实 Provider、remote Target、multi-node、load、生产时长或生产 Retention。Deterministic managed Docker
bounded load/admission mechanics 的最新证据见 `docs/reports/stage-3-docker-fixture-load-e944b449.md`；它不替代
真实 Provider、multi-host/Kubernetes multi-node、failure injection under load、生产 SLA 或生产时长负载。
Deterministic managed Docker exact network failure targeting、Peer Session 隔离、Generation fencing 与
post-recovery load mechanics 的最新证据见
`docs/reports/stage-3-docker-fixture-load-failure-cfecba63.md`；它覆盖 single-host deterministic exact network、
busy-container loss、same logical Worker replacement、incarnation/Generation fencing、named-volume continuity，
以及 exact busy Provider Host descendant process crash、`provider_unavailable` terminalization 与同 logical Worker
上的 distinct new-Execution recovery。前序 container-loss checkpoint 保留在
`docs/reports/stage-3-docker-fixture-load-failure-7684c6d8.md`，早期 network-only checkpoint 保留在
`docs/reports/stage-3-docker-fixture-load-failure-ab88798d.md`。这些证据不替代真实 Provider、
multi-host/Kubernetes multi-node、real Provider-process/release-rollout failure under load、生产 SLA 或生产时长负载。

## 1. 发布身份与证据边界

- [ ] 发布分支、Commit SHA 和 Git 工作区状态已记录。
- [ ] Control Plane、Worker 和 Provider Host 使用同一已提交源码构建。
- [ ] Worker 镜像使用 Registry 返回的不可变 Digest，不使用 tag 作为唯一发布身份。
- [ ] Worker Manifest ID、Release Revision ID、Execution Target ID 和目标环境已记录。
- [ ] Provider Host Protocol 固定为 `2.1`，Worker Protocol 固定为 `2`，Runtime Event 固定为 `2`。
- [ ] 报告明确区分真实 Provider、deterministic fixture、Target 类型和是否经过 Control Plane/agentd。
- [ ] 当前已知限制、外部依赖和未执行项已由发布负责人接受。
- [ ] 第三方 Codex/Claude API Key 只通过受控 Credential `apiKey` 与可选 `baseUrl` 注入；新建/轮换拒绝
      Codex `organization` 与 Claude `authToken`，历史记录只保留解析兼容；Secret 值未进入聊天、命令参数、
      Target 配置、日志或报告；operator 环境变量名只作为受控 gate CLI 输入，未进入 Target 配置、持久化
      command evidence、日志或报告。
- [ ] 没有把 Local Provider Host smoke 描述成 Local Supervisor、SSH、Docker 或 Kubernetes Release Gate。

## 2. 数据库与 DDL

当前工作树的 checked-in forward migration boundary 是 `000041`：

| Migration | 发布不变量                                                                              |
| --------- | --------------------------------------------------------------------------------------- |
| `000032`  | Compact、Review、Rollback、Fork Turn 形状、逻辑历史祖先和 Primary Control Command       |
| `000033`  | Provider Credential User/Organization/Tenant/Platform Scope、选择器和自动选择策略       |
| `000034`  | Worker operator revocation、logical identity tombstone 和 Token/Lease/Claim fencing     |
| `000035`  | Project/Target Credential Binding 与 immutable per-Generation Execution Grant           |
| `000036`  | `projects.git_credential_id` 兼容回填后退役为不可写 authority                           |
| `000037`  | immutable Worker Release Revision、CAS Policy、Transition History 和 release pinning    |
| `000038`  | disabled Binding 历史也可用的复合外键 lookup indexes                                    |
| `000039`  | 每个 Execution Target 最多一个 active `worker_image_pull` Binding，歧义升级 fail closed |
| `000040`  | Worker Release Transition 必须与当前 Policy 的版本和 promoted/canary 状态完全一致       |
| `000041`  | `artifacts_kind_check` forward 扩展 `diff`，历史 Artifact kind 与 migration 保持不变    |

- [ ] PostgreSQL 备份完成，并在隔离环境验证可恢复。
- [ ] `/ready.checks.schema.expectedVersion` 与当前镜像内 migration boundary 一致。
- [ ] `control_plane_schema_migrations` 中版本连续、Checksum 匹配，没有手工补写记录。
- [ ] PostgreSQL 真实 forward migration integration 全部通过。
- [ ] SQLite safety mirror tests 全部通过。
- [ ] `000037` 的 Revision、Policy、Transition、Worker/Execution release shape 和多 Revision Target 已验证。
- [ ] `000038` 的四个 Credential Binding 外键索引在 PostgreSQL 和 SQLite 均存在。
- [ ] `000039` 在重复 active Target Binding 上拒绝升级，修复歧义后可重试且新唯一索引生效。
- [ ] `000040` 在 Policy/最新 Transition 不一致时拒绝升级，并阻止写入不匹配的 Transition。
- [ ] `000041` 升级前拒绝 `diff`、升级后保留全部既有 kind 并接受 `diff`，未知 kind 继续被拒绝。
- [ ] PostgreSQL 不依赖 Runtime `AutoMigrate`；历史 migration 文件没有被修改。
- [ ] 回滚方案确认旧镜像可以读取已应用的新 schema，或已有经过评审的 forward fix；不得仅回滚 Deployment。

只读核对：

```sql
SELECT version, name, checksum, applied_at
FROM control_plane_schema_migrations
ORDER BY version;
```

## 3. Worker 构建与供应链

Clean-SHA Registry 验证入口（输出目录必须为空或不存在，Registry Credential 由 Docker/Buildx 外部安全
配置，禁止写入参数）：

```bash
python3 scripts/stage3-provider-acceptance/registry_release_gate.py \
  --image-repository registry.example.com/synara/worker \
  --builder synara-worker-release \
  --output-dir /tmp/synara-worker-registry-release
```

当前 checked-in production profile 的 Registry/Vault KMS/Rekor/Kyverno 门禁命令如下。只传环境变量名，
不要把值写进命令、仓库、报告或 shell history；`--production-*configmap` 应指向当前 live 集群已应用
ConfigMap 的导出 YAML，而不是仍含 placeholder 的模板文件。长时间的 rollout/load/soak 门禁先完成，
然后才启动短期 Credential shell。当前仓库自建 Stage 3 production-like overlay 在 2026-07-21 的
non-secret runtime truth 是：`kind-synara-stage3-prod`、`synara-kms`、`192.168.139.3:5443/synara/worker`
和 Registry container `synara-stage3-prod-registry`。

```bash
export SYNARA_STAGE3_KMS_RUNTIME=/secure/synara-stage3-kms-runtime
export SYNARA_VAULT_INIT_JSON=/secure/synara-vault/init.json
export VAULT_ADDR=<approved-live-vault-address>
export VAULT_CACERT="$SYNARA_STAGE3_KMS_RUNTIME/ca.crt"

"$SYNARA_STAGE3_KMS_RUNTIME/bin/start-short-lived-credential-session.py"

kubectl --context kind-synara-stage3-prod -n synara-system get configmap synara-worker-cosign-public-key -o yaml \
  > "$SYNARA_STAGE3_KMS_RUNTIME/synara-worker-cosign-public-key.live.yaml"
kubectl --context kind-synara-stage3-prod -n synara-system get configmap synara-worker-signing-settings -o yaml \
  > "$SYNARA_STAGE3_KMS_RUNTIME/synara-worker-signing-settings.live.yaml"

python3 scripts/stage3-provider-acceptance/registry_release_gate.py \
  --image-repository 192.168.139.3:5443/synara/worker \
  --builder synara-worker-release \
  --signing-policy-profile production \
  --registry-auth-username-env REGISTRY_USERNAME \
  --registry-auth-password-env REGISTRY_PASSWORD \
  --registry-ca-cert-env REGISTRY_CA_CERT \
  --production-public-key-configmap "$SYNARA_STAGE3_KMS_RUNTIME/synara-worker-cosign-public-key.live.yaml" \
  --production-repository-configmap "$SYNARA_STAGE3_KMS_RUNTIME/synara-worker-signing-settings.live.yaml" \
  --production-registry-config "$SYNARA_STAGE3_KMS_RUNTIME/registry-production.yml" \
  --production-registry-retention-policy "$SYNARA_STAGE3_KMS_RUNTIME/registry-retention-policy.json" \
  --production-registry-container synara-stage3-prod-registry \
  --production-registry-runtime-config-path /etc/distribution/config.yml \
  --output-dir /tmp/synara-worker-registry-release

python3 scripts/stage3-provider-acceptance/vault_kms_admission_gate.py \
  --kube-context kind-synara-stage3-prod \
  --vault-namespace synara-kms \
  --security-namespace synara-system \
  --admission-test-namespace synara-admission \
  --vault-bin "$SYNARA_STAGE3_KMS_RUNTIME/bin/vault-kubectl-active" \
  --expected-approle-policy synara-worker-release-signer \
  --registry-release-gate-report /tmp/synara-worker-registry-release/worker-registry-release-gate.json \
  --unsigned-image-ref 192.168.139.3:5443/synara/worker@sha256:<unsigned-digest> \
  --wrong-key-image-ref 192.168.139.3:5443/synara/worker@sha256:<wrong-key-digest> \
  --tag-drift-image-ref 192.168.139.3:5443/synara/worker:synara-stage3-tag-drift-<unique-run-id> \
  --output-dir /tmp/synara-worker-vault-kms-admission

# `synara-stage3-tag-drift-<unique-run-id>` must resolve to a non-baseline Digest, be owned by this run,
# and be removed exactly after the gate. Never read, replace, or reuse `latest`.
python3 scripts/stage3-provider-acceptance/vault_snapshot_restore_drill.py \
  --vault-bin "$SYNARA_STAGE3_KMS_RUNTIME/bin/vault-kubectl-active" \
  --output-dir /tmp/synara-worker-vault-snapshot-restore

# Formal audit SIEM/WORM gate inputs. Keep values outside the repository and shell history;
# only the environment variable names may appear in reports. The three TLS variables below
# carry PEM contents, not file paths. The endpoint must be a bare `https://host:port`
# authority with no path or embedded credentials. The two mc host variables are
# credentialed HTTPS authorities for distinct writer and verifier identities.
export VAULT_AUDIT_SIEM_ENDPOINT="$(< /secure/synara-vault-audit-siem/endpoint.txt)"
export VAULT_AUDIT_SIEM_RESOLVE="$(< /secure/synara-vault-audit-siem/resolve.txt)"
export VAULT_AUDIT_SIEM_CLIENT_CERT="$(< /secure/synara-vault-audit-tls/client.crt)"
export VAULT_AUDIT_SIEM_CLIENT_KEY="$(< /secure/synara-vault-audit-tls/client.key)"
export VAULT_AUDIT_SIEM_CA_CERT="$(< /secure/synara-vault-audit-tls/ca.crt)"
export VAULT_AUDIT_WORM_MC_ALIAS=synara-vault-audit
export VAULT_AUDIT_WORM_MC_CONFIG_DIR=/secure/synara-vault-audit-mc
export VAULT_AUDIT_WORM_MC_HOST="$(< /secure/synara-vault-audit-mc/writer-host.txt)"
export VAULT_AUDIT_WORM_MC_VERIFIER_HOST="$(< /secure/synara-vault-audit-mc/verifier-host.txt)"
export VAULT_AUDIT_WORM_MC_RESOLVE="$(< /secure/synara-vault-audit-mc/resolve.txt)"

# Start the acceptance sink in a separate operator-owned terminal before the formal SIEM gate.
python3 scripts/stage3-provider-acceptance/vault_audit_acceptance_sink.py \
  --bind-host 0.0.0.0 \
  --port 18443 \
  --state-dir /secure/synara-vault-audit-state \
  --server-cert /secure/synara-vault-audit-tls/server.crt \
  --server-key /secure/synara-vault-audit-tls/server.key \
  --client-ca-cert /secure/synara-vault-audit-tls/ca.crt \
  --retention-days 365 \
  --object-lock-required \
  --object-lock-mc-alias-env VAULT_AUDIT_WORM_MC_ALIAS \
  --object-lock-mc-config-dir-env VAULT_AUDIT_WORM_MC_CONFIG_DIR \
  --object-lock-mc-host-env VAULT_AUDIT_WORM_MC_HOST \
  --object-lock-mc-resolve-env VAULT_AUDIT_WORM_MC_RESOLVE \
  --object-lock-bucket synara-vault-audit \
  --object-lock-prefix entries

python3 scripts/stage3-provider-acceptance/vault_audit_siem_delivery_gate.py \
  --operations-policy deploy/kubernetes/security/vault/operations-policy.json \
  --vault-command-json "[\"$SYNARA_STAGE3_KMS_RUNTIME/bin/vault-kubectl-active\"]" \
  --vault-auditor-token-env VAULT_OPERATOR_TOKEN \
  --kube-context kind-synara-stage3-prod \
  --vault-namespace synara-kms \
  --vault-statefulset synara-vault \
  --shipper-container vault-audit-shipper \
  --timeout-seconds 60 \
  --poll-interval-seconds 2 \
  --mc-bin mc \
  --output-dir /tmp/synara-stage3-vault-audit-siem

test -f /tmp/synara-stage3-vault-audit-siem/vault-audit-siem-delivery-gate.json
test -f /tmp/synara-stage3-vault-audit-siem/vault-audit-siem-delivery-gate.md
```

The audit/SIEM gate consumes only the named inputs `VAULT_ADDR`, `VAULT_CACERT`,
`VAULT_OPERATOR_TOKEN`, `VAULT_AUDIT_SIEM_ENDPOINT`,
`VAULT_AUDIT_SIEM_RESOLVE`, `VAULT_AUDIT_SIEM_CLIENT_CERT`,
`VAULT_AUDIT_SIEM_CLIENT_KEY`, `VAULT_AUDIT_SIEM_CA_CERT`,
`VAULT_AUDIT_WORM_MC_ALIAS`, `VAULT_AUDIT_WORM_MC_CONFIG_DIR`,
`VAULT_AUDIT_WORM_MC_HOST`, `VAULT_AUDIT_WORM_MC_VERIFIER_HOST`, and
`VAULT_AUDIT_WORM_MC_RESOLVE`. Its required retained artifacts are
`vault-audit-siem-delivery-gate.json` and
`vault-audit-siem-delivery-gate.md`; neither report may contain a value from
those environments.

生产 KMS pin 固定为自建 HashiCorp Vault Transit on Kubernetes：Helm chart `hashicorp/vault` `0.34.0`、
release `synara-vault`、namespace `synara-kms` 和 image
`hashicorp/vault:2.0.3@sha256:a296a888b118615dc01d5f1a6846e6d4a7277946caaed5b447008fff5fe06b54`。
KMS reference 为 `hashivault://synara-worker-release`；signer identity 为
`auth/approle/role/synara-worker-release-signer`，仅允许审计路径 `transit/sign/synara-worker-release`。
signing Credential 环境变量名固定为 `VAULT_ADDR`、`VAULT_TOKEN`、`VAULT_CACERT`，token 必须短期且
policy-scoped。helper shell 额外只导出 `VAULT_OPERATOR_TOKEN`、`VAULT_SNAPSHOT_OPERATOR_ROLE_ID`、
`VAULT_SNAPSHOT_OPERATOR_SECRET_ID`、`VAULT_SNAPSHOT_RESTORE_KEY_1..3`、`REGISTRY_USERNAME`、
`REGISTRY_PASSWORD` 和 `REGISTRY_CA_CERT`。
tlog 强制上传并在线验证 public Rekor `https://rekor.sigstore.dev` 的 inclusion proof 与 signed entry
timestamp；Kyverno 固定 `failurePolicy: Fail`、`validationFailureAction: Enforce`、tag-to-digest mutation 和
exact-digest signature verification。

当前 production 路径固定绑定 clean SHA 与 checked-in source hash：`deploy/worker/production-signing-policy.json`、
`deploy/worker/production-signing-profile.json`、`deploy/kubernetes/security/cluster/verify-synara-worker-images.yaml`、
`deploy/kubernetes/security/namespaced/synara-worker-cosign-public-key-configmap.yaml`、
`deploy/kubernetes/security/namespaced/synara-worker-signing-settings-configmap.yaml`、
`deploy/kubernetes/security/production/kustomization.yaml`。`registry_release_gate.py`
先验证这些 source，再读取命名的运行中 Registry 容器及其 runtime config，把 container/image identity、TLS
证书、auth、repository、delete/retention 设置与导出配置和 checked-in retention contract 比对，并校验当前
导出的 runtime ConfigMap YAML 与 Vault 导出的公钥/仓库模式一致；
`vault_kms_admission_gate.py` 再通过 passing `registry_release_gate.py` 报告复核同一 clean-SHA/source-hash
boundary，并要求 isolated state、materialized Vault/Registry CA、Registry auth config 与 owner-scoped 临时
admission 资源全部被精确清理。以下命令是当前实现的门禁，不代表它们已经在 clean commit 上执行通过。

生产 Vault 运维边界额外固定在 `deploy/kubernetes/security/vault/operations-policy.json` 与
`docs/runbooks/vault-kms-operations.md`。Shamir custody 固定为 `5` shares / `3` threshold；隔离 snapshot restore
drill 固定使用 snapshot-operator AppRole、三把 unseal key、`--network none` 的 isolated Docker Vault 和
UID-100 hardened audit tmpfs，并由 `vault_snapshot_restore_drill.py` 报告当前 clean Git SHA、异步 snapshot
application、单节点 leader 与 source/restore hash；audit 仍要求恰好两个 PVC-backed file device，并且
必须保留独立的 rotation/外部 SIEM retained sink 边界。

Registry runtime 必须精确使用
`registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373`；门禁同时
绑定容器 `Config.Image`、实际 Image ID 与匹配 RepoDigest，mutable tag、alias repository 或 digest drift
一律 fail closed。Vault `lookup-self` 还必须证明 signer token 来自
`synara-worker-release-signer` AppRole、类型为 `batch`、`orphan=true` 且只含该 signer policy；报告仅保留
这些安全字段和 policy-list SHA256，不保留 token 或 Credential 值。
Signer AppRole 还必须与 `operations-policy.json` 的精确合同一致：bound
Secret ID、`batch` orphan token、仅 `synara-worker-release-signer` policy、
`token_no_default_policy=true`、token TTL/max TTL `7200s/14400s`、Secret ID
TTL/uses `600s/1`。Auditor 和 snapshot-operator 同样为单 policy、bound
single-use Secret ID、`batch` orphan、no-default-policy，token TTL/max TTL
固定 `1800s/3600s`。任一放宽都阻断发布。

最新 clean-SHA signing-policy/disposable Registry slice 已在 commit `7659dd5f` 通过，证据见
`docs/reports/stage-3-worker-registry-signing-policy-7659dd5f.md`；较早 supply-chain 与仅覆盖 reproducibility
的报告分别保留在 `docs/reports/stage-3-worker-registry-supply-chain-71ef4b5e.md` 和
`docs/reports/stage-3-worker-registry-release-gate-dc43a4d6.md`。以下已勾选项仅表示该技术断言已有
clean-commit 证据；生产 Registry、生产签名身份、Credential、retention 与 rollout 仍按未勾选项验收。

- [ ] Worker Image 已推送到目标 Registry，并记录 registry-returned Digest。
- [x] 至少生成目标平台所需的 `linux/amd64`、`linux/arm64` manifest list；若只发布单架构，已记录审批。
- [x] Base Image、Node.js、Codex CLI、Claude Agent SDK 和系统包均由锁文件或 Digest 固定。
- [x] BuildKit SBOM generator 与 Dockerfile frontend 均使用 checked-in immutable Digest，不解析 mutable tag。
- [x] Registry export 使用 `SOURCE_DATE_EPOCH` layer rewrite，且 transient APK log/raw SBOM 未进入最终 layer。
- [x] Worker build-revision cache identity 等于发布 Git SHA，跨 stage runtime artifacts 的 mtime 已归一。
- [x] Disposable gate 使用 digest-pinned Cosign/Trivy，精确验证两个 OCI index 的 Git SHA、Version、Run ID、
      Slot 和 Digest annotations，并删除临时私钥与隔离 state。
- [x] Checked-in signing policy schema 可区分 `ephemeral-key`、`keyless` 与 `kms-key`；当前 checked-in
      `--signing-policy-profile production` 固定为 Vault Transit `kms-key`，并强制 TLS Registry、Rekor 与
      Kyverno admission。
- [x] `linux/amd64`、`linux/arm64` 均为 `HIGH=0`、`CRITICAL=0`、Secret=0、非 EOSL，Trivy DB 满足 24 小时
      freshness；`GO-2026-5932` 保留为未豁免的不可达 `UNKNOWN` review finding。`e29e2757` 两个已验证
      build digest 的前序 production-profile gate 仅因 Trivy DB 下载 `EOF` 失败；`d0b379c8` 增加不持久化
      值的工具容器代理通道后，只重跑该失败扫描即通过，未重复 build/sign/Rekor write，见
      `docs/reports/stage-3-registry-vulnerability-proxy-d0b379c8.md`。
- [ ] Worker Manifest 中的 Git SHA、OS/Arch、Image Digest、Protocol、Provider Runtime 和 Capability Hash 可追溯。
- [ ] 生产发布已归档 SBOM/扫描报告，并使用当前 checked-in Vault Transit `kms-key` signer、Rekor
      transparency log 与 Kyverno admission policy 完成镜像签名/来源验证；不得以 disposable
      ephemeral-key 证据替代。
- [ ] 当前 production Registry Credential 仅通过 `--registry-auth-username-env`、
      `--registry-auth-password-env` 与 `--registry-ca-cert-env` 传入；当前 runtime ConfigMap YAML 仅通过
      `--production-public-key-configmap` 与 `--production-repository-configmap` 指向导出文件；Registry
      config/retention/container/runtime path 四个 production 参数齐全，live 容器身份、TLS 证书、auth 与
      retention 和导出/checked-in contract 一致；Registry exact tag+digest、runtime Image ID 与 RepoDigest
      一致；没有把值写进命令、仓库或报告。
- [ ] `vault_kms_admission_gate.py` 已用 passing `registry_release_gate.py` 报告、当前 clean SHA/source hash、
      Vault AppRole policy、正向 signed image 和负向 unsigned/wrong-key/tag-drift probes 完成 admission
      验证，并留存非 Secret 证据。
- [ ] `vault_snapshot_restore_drill.py` 已在当前 clean SHA 上使用 checked-in
      `operations-policy.json`、snapshot-operator AppRole 和三把 Shamir key shares 完成 isolated Docker restore，
      验证 `source.gitSha`/clean worktree、`vault status`、单节点 Raft leader、两个 audit device 及 0600 sink、
      Transit key、signer/auditor/snapshot-operator AppRole，并精确清理 container、tmpfs 与临时 state。
- [ ] `deploy/kubernetes/security/vault/operations-policy.json` 与
      `docs/runbooks/vault-kms-operations.md` 已记录当前生产 KMS reference、Credential 环境变量名、signer
      identity、Rekor/tlog、Kyverno admission、Shamir custody、snapshot drill 和 audit/SIEM boundary，且与
      checked-in bootstrap/policy 文件一致。
- [ ] 生产 Vault live 只保留恰好两个 PVC-backed file audit device：
      `file -> /vault/audit/audit-primary.log` 与
      `file-secondary -> /vault/audit/audit-secondary.log`；没有额外 active audit sink。
- [ ] audit PVC rotation 与外部 SIEM retained sink 已按
      `operations-policy.json` / `vault-kms-operations.md` 落地；正式 gate 必须直接验证外部 bucket
      versioning、365 天 `COMPLIANCE` Object Lock、exact audit batch/version/content hash，并证明 delete 与
      retention-shortening 均被存储层拒绝；报告同时固定 writer/verifier 两份 IAM policy 的 repository path
      和 SHA-256。仅有本地 hash-chain 或 DELETE API 405 不得勾选此项。
- [ ] Worker 使用非 Root 用户，Workspace、Git Cache 和 Runtime Output Root 权限正确。
- [ ] 没有在镜像、Layer、Build Arg、Environment 或 Manifest 中写入 Credential。
- [ ] 私有 Registry 使用 Tenant/Organization-scoped Registry Credential 和 Target-scoped
      `worker_image_pull` Binding；Binding selector 与镜像 Registry Host 精确匹配。
- [ ] Kubernetes Registry auth 仅使用 OCI Basic；Bearer-only 和带自定义端口的 Registry auth 在当前
      Credential contract 下安全失败，未通过手工 Secret 或数据库改写绕过。

## 4. Credential、SecretGuard 与安全撤销

- [ ] Provider Credential 只通过匿名 FD 3 和 Provider-specific allowlist 传递。
- [ ] Worker Registration Token、Lease Token、数据库/对象存储凭据没有进入 Provider Host 输入或子进程环境。
- [ ] Git、Registry、Package Credential 只在对应 operation stage 解析，且使用 immutable Generation Grant。
- [ ] HTTPS AskPass、SSH Agent/Host Key、Registry pull auth 和 Package auth 的临时状态在阶段结束后清理。
- [ ] SecretGuard 覆盖 Provider stdout/stderr、Runtime Event、Terminal Preview/Artifact、错误和安全 Metadata。
- [ ] 输出报告、Control Plane/agentd/Provider Host 日志和集中日志平台均通过 Secret scan。
- [ ] Rotation 后旧 Credential version 不能被新 Execution 解析；Revoke/Expiry/Binding Disable 立即阻止新解析。
- [ ] Worker operator revocation 使用当前 `expectedIncarnation` 和唯一 `Idempotency-Key`，并记录原因。
- [ ] 已确认 Worker revocation 不可逆：logical identity tombstone 禁止同身份重新注册，不能用于普通滚动升级。
- [ ] `outcomeUnknownExecutions` 或 `checkpointUnconfirmedExecutions` 非零时已升级为人工恢复事件。

## 5. 自动化质量门禁

Go：

```bash
cd services/control-plane
go test ./...
go vet ./...
go test -race ./internal/secretguard ./internal/agentd

SYNARA_TEST_DATABASE_URL='postgres://.../synara_test?sslmode=disable' \
SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL='postgres://.../synara_test?sslmode=disable' \
  go test -count=1 ./...
```

- [ ] 默认 Go 全量测试通过。
- [ ] `go vet ./...` 通过。
- [ ] SecretGuard/agentd Race Test 通过。
- [ ] 真实 PostgreSQL 全量测试通过，且测试数据库在运行后清理。
- [ ] `git diff --check` 通过。

TypeScript/Web：只能使用 `bun run test`，禁止 `bun test`。

```bash
bun run --cwd packages/contracts test src/providerHost.test.ts src/providerRuntime.test.ts
bun run --cwd apps/provider-host test \
  src/protocol.test.ts \
  src/runtimeEventV2.test.ts \
  src/turnDiffs.test.ts \
  src/codexAppServerRuntime.test.ts \
  src/claudeAgentSdkRuntime.test.ts
bun run --cwd apps/web test \
  src/lib/controlPlaneClient.test.ts \
  src/lib/controlPlaneProjection.test.ts \
  src/session-logic.test.ts \
  src/components/ChatView.logic.test.ts
```

- [ ] Contracts、Provider Host、Web/Projection 和设置页 focused tests 通过。
- [ ] Web Production Build 通过。
- [ ] 操作人已明确授权，并且最终一次 `bun fmt`、`bun lint`、`bun typecheck` 全部通过。
- [ ] Final gate 后没有再修改受格式化、Lint 或 TypeScript 检查覆盖的文件。

## 6. Acceptance 证据等级

当前仓库已有的实现期证据不能替代下列发布勾选项：

真实 Provider immutable Kubernetes rollout 必须从同一 clean release boundary 分别运行 Codex 与 Claude；
只传第三方 Credential、Base URL 和自定义模型的环境变量名：

```bash
source ~/.synara-acceptance-env

python3 scripts/stage3-provider-acceptance/kubernetes_real_provider_release_rollout_gate.py \
  --provider codex \
  --real-provider-credential-env SYNARA_ACCEPTANCE_CODEX_KEY \
  --real-provider-credential-field apiKey \
  --real-provider-base-url-env SYNARA_ACCEPTANCE_CODEX_BASE_URL \
  --real-provider-model-env SYNARA_ACCEPTANCE_CODEX_MODEL \
  --real-provider-load-sla-file deploy/worker/production-load-sla.json \
  --kind-worker-nodes 2 \
  --load-waves 6 \
  --timeout 5400 \
  --output-dir /tmp/synara-kubernetes-real-provider-codex-rollout

python3 scripts/stage3-provider-acceptance/kubernetes_real_provider_release_rollout_gate.py \
  --provider claudeAgent \
  --real-provider-credential-env SYNARA_ACCEPTANCE_CLAUDE_KEY \
  --real-provider-credential-field apiKey \
  --real-provider-base-url-env SYNARA_ACCEPTANCE_CLAUDE_BASE_URL \
  --real-provider-model-env SYNARA_ACCEPTANCE_CLAUDE_MODEL \
  --real-provider-load-sla-file deploy/worker/production-load-sla.json \
  --kind-worker-nodes 2 \
  --load-waves 6 \
  --timeout 5400 \
  --output-dir /tmp/synara-kubernetes-real-provider-claude-rollout
```

For Stage 3, these two commands are also the explicit production-duration soak
gate. Concurrency is resource-governed, not an unbounded request count: two
worker nodes, one active execution slot per Worker, Tenant concurrency `2`, and
four Sessions continuously exercise quota rejection and slot reuse. Each
Worker is pinned to `requests cpu=100m/memory=128Mi/ephemeral=128Mi`,
`limits cpu=1/memory=1Gi/ephemeral=2Gi`, and a `1Gi` Workspace; the namespace
quota is `requests cpu=1/memory=1Gi`, `limits cpu=2/memory=2Gi`, and `4Gi`
ephemeral storage. Both candidate and rollback phases must independently meet
the checked-in `1800s` minimum plus P95/P99 admission limits and zero unexpected
errors from `deploy/worker/production-load-sla.json`. A pass therefore requires
at least `3600s` load per Provider and produces
`kubernetes-real-provider-worker-release-rollout-gate.json` plus the matching
Markdown report. A shorter smoke, more client concurrency without more
resources, or a report missing either phase cannot close production soak.

默认 6 个 nominal waves 在 candidate promotion 与 baseline rollback 间拆为 `3 + 3` 的最低波数；两个 phase
各自还必须达到 `minimumDurationSeconds: 1800`，不足时继续完整 wave，因此 load 部分最低约 60 分钟，另加
build/rollout/fault/cleanup 时间。门禁必须证明两个 immutable digest、每个 Execution 独立 Pod/Worker、两个
overlap Pod 分布到两个可调度 non-control-plane Node、同一 Sessions 跨 Revision 的 native Cursor continuity、
Audit/Outbox、精确 cleanup 和输出扫描。该门禁自带的 disposable loopback HTTP Registry 无 TLS/auth，只证明
immutable rollout；production Registry live evidence 仍由带四个 runtime 参数的 production
`registry_release_gate.py` 提供，Vault/Kyverno/Rekor live evidence 由 `vault_kms_admission_gate.py` 提供。

| 证据                                                   | 当前结论                                                                          | 发布边界                                                                                                                                                                                        |
| ------------------------------------------------------ | --------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 真实 Codex/Claude Local two-Turn product-path smoke    | clean commit `fb9e25ec` 各 12/12                                                  | 经过 Control Plane/LocalSupervisor/agentd，但不是完整 Local Gate                                                                                                                                |
| 真实 Codex/Claude Generated File + Checkpoint          | clean commit `be919393` matrix pass                                               | standalone Ready Artifact 与 Snapshot 已验；Diff 由下一行独立跟踪                                                                                                                               |
| 真实 Codex/Claude Local Large Diff                     | clean commit `90fae52c` matrix pass                                               | Ready `diff`/下载/顺序/restart/cleanup/Secret scan 已验                                                                                                                                         |
| 真实 Codex/Claude Local failure matrix                 | clean commit `61e38f4f` 各 `16/16`                                                | 401/429、scoped Host crash、Cursor expiry/restart 与新 Execution 已验                                                                                                                           |
| 真实 Codex/Claude consolidated Local release gate      | clean commit `253052aa` aggregate pass                                            | 四份 product/failure 报告同 SHA/hash，无 fail/skipped，cleanup/Secret scan 已验                                                                                                                 |
| deterministic Local long-Session fixture soak          | clean commit `6e866a30` 100/100 Turns                                             | 9 次额外 restart、Event `1..1371`、分页与 repeated Checkpoint 已验；不是真实 Provider/production soak                                                                                           |
| deterministic Docker Provider concurrency fixture      | clean commit `eeb7a2f1` 9/9                                                       | 两 Worker、Codex/Claude 两 Session/Execution、同时 pending Approval 与隔离终态已验；不是真实 Provider/Retention/production concurrency                                                          |
| deterministic Local Retention/Cleanup fixture          | clean commit `c27914da` 9/9                                                       | active Execution fencing、无引用 Artifact 删除、Checkpoint 保护与终态后单次 physical cleanup 已验；不是真实 Provider/remote/production Retention                                                |
| deterministic Docker bounded load/admission fixture    | clean commit `e944b449` 100/100 Executions                                        | 四 Session、50 次 quota rejection/retry、75 次双 Worker overlap、Artifact/Checkpoint 唯一终态已验；不是真实 Provider/production load                                                            |
| deterministic Docker network/container/Host crash load | clean commit `cfecba63` 12/12                                                     | exact network/container/Host-process fault、same-Worker replacement、Generation 或 new-Execution recovery 与 100 Execution load 已验；不是真实 Provider/multi-node fault                        |
| deterministic Docker release rollout failure/load      | clean commit `41683366` 15/15                                                     | canary container-loss Generation `1->2`、peer 隔离、25 波/100 Execution release pins、分页 Audit 与 topic-filtered Outbox 已验；不是真实 Provider/production rollout                            |
| deterministic Kubernetes Registry release rollout      | clean commit `d1f3b68a` 15/15                                                     | three-node Kind、两个真实 Registry digest、promote/canary/promote/rollback、Pod/Worker/Manifest release pins 与 exact cleanup 已验；overlap Pod 同 Node，不是 production distribution/load      |
| 真实 Codex `0.144.x` `terminal-large`                  | Explicit Unsupported                                                              | Unified Exec 仅保留 1 MiB Head/Tail；不得牺牲 durable Approval                                                                                                                                  |
| Claude ambient OAuth `terminal-large`                  | Explicit Unsupported                                                              | 需 controlled Credential 绑定 Runtime Output Root                                                                                                                                               |
| deterministic Local/Docker core suite                  | 已通过                                                                            | 证明共享 Control Plane/agentd/Host orchestration，不证明真实 Adapter                                                                                                                            |
| deterministic Provider fault matrix                    | malformed/oversized/crash 已通过                                                  | 不是真实 Provider failure 分类                                                                                                                                                                  |
| deterministic Docker/Kubernetes failure matrix         | 已通过实现期运行                                                                  | 不等于生产网络、真实 CNI 或正式 rollout                                                                                                                                                         |
| SSH real Provider runtime provisioning                 | clean `14f7dd2d` Codex/Claude product 各 `22 pass + 1 unsupported`                | 同一 external host/pinned Host Key/Provider Host SHA、受控 `apiKey` + `baseUrl` + 自定义模型、四个不同 runtime identity 与 exact owned-runtime cleanup 已验                                     |
| SSH real Provider fault-injection transport            | clean `14f7dd2d` Codex/Claude failure 各 `16/16`                                  | token-scoped reverse relay、401/429、systemd MainPID scoped crash/retry、Cursor expiry/restart、零 Secret finding 已验                                                                          |
| Docker real Provider fault-injection transport         | clean `b1c52bae` Codex/Claude failure 各 `16/16`                                  | 真实 401/429、精确 Host crash、Cursor expiry/restart、exact cleanup 与 Secret scan 已验                                                                                                         |
| Kubernetes real Provider fault-injection transport     | clean `3c523417` Codex/Claude failure 各 `16/16`                                  | host-gateway 401/429、精确 Pod crash、Cursor expiry/restart、四集群 exact cleanup 与 Secret scan 已验                                                                                           |
| SSH consolidated release gate                          | clean `14f7dd2d` 四 child aggregate pass                                          | Codex/Claude product 各 `22 pass + 1 unsupported`、failure 各 `16 pass`；同一 pinned Host Key/Provider Host SHA、4 个不同 runtime identity、exact cleanup、Secret scan 与环境变量名非持久化已验 |
| Docker consolidated release gate                       | historical clean `b1c52bae` 四 child pass；当前六 child gate open                 | 历史 product/failure 已验；当前实现新增 Codex/Claude 两个 1800s SLA/load child，必须在本次 clean SHA 重跑                                                                                       |
| Kubernetes consolidated release gate                   | historical clean `3c523417` 四 child pass；当前六 child gate open                 | 历史 product/failure 已验；当前实现新增两个 SLA/load child，必须在本次 clean SHA 的六个 disposable Kind 上重跑                                                                                  |
| Worker Registry signing-policy gate                    | clean commit `7659dd5f` gate/report 已通过                                        | keyless/KMS 实现路径与 ephemeral mechanics 已验；真实生产 identity/tlog/admission、Registry Credential/retention 与 rollout 尚待记录                                                            |
| SSH fixture                                            | 2026-07-14 disposable VM 13/13                                                    | 不是当前 Commit 的真实 Provider gate                                                                                                                                                            |
| Kubernetes fixture                                     | `aa1d0225` three-node owned Kind 24/24；`6b71703f` OrbStack 22 pass/1 unsupported | PDB-blocked Drain、跨 Worker replacement、普通 Drain、Eviction、Network、Canary、restart 与 exact cleanup 已验；不证明真实 Provider 或 production multi-node pass gate                          |

真实 Provider × Target gate：

- [ ] Codex × Local：实现证据 `253052aa` 已覆盖 Discovery、Start、Send、第二 Turn、Restart、
      Interaction、Artifact 和错误分类；本次发布 Commit 仍须重跑。
- [ ] Claude × Local：实现证据 `253052aa` 已覆盖同一 Local release slice；本次发布 Commit 仍须重跑。
- [x] Codex × SSH：clean `14f7dd2d` 已覆盖 real external-host product `22 pass + 1 unsupported` 与 failure
      `16 pass`；证据为 `docs/reports/stage-3-real-provider-ssh-release-gate-14f7dd2d.md`。
- [x] Claude × SSH：clean `14f7dd2d` 已覆盖 real external-host product `22 pass + 1 unsupported` 与 failure
      `16 pass`；唯一 frozen unsupported 为 `real-provider.compact-boundary`；证据同上。
- [ ] Codex × Docker：replace、volume/checkpoint、network interruption、resource limits。
- [ ] Claude × Docker：同上。
- [ ] Codex × Kubernetes：`kubernetes_real_provider_release_rollout_gate.py` 在本次 clean release boundary
      完成 immutable canary/promote/rollback、Pod replacement、跨 Node overlap、跨 Revision Session resume、
      两 phase production-duration SLA 和 exact cleanup。
- [ ] Claude × Kubernetes：同一真实 rollout/SLA 门禁通过；使用受控 `apiKey` 或明确批准的 `authToken`、
      第三方 Base URL 与自定义模型环境变量名，且不持久化这些名称或值。
- [x] 本地 `orbstack` context 已完成 deterministic clean-SHA required matrix、Context/TLS pinning、共享本地镜像
      `Never` 策略与精确 cleanup；证据为
      `docs/reports/stage-3-kubernetes-orbstack-fixture-6b71703f.md`。
- [x] owned disposable Kind 已在 clean SHA `aa1d0225` 完成 three-node deterministic `24/24`：进入矩阵前
      `3/3` Node Ready、两个 Worker 可调度；exact PDB 先阻止 drain，删除 PDB 后 replacement Pod 在源 Node
      仍 cordon 时跨 Worker 调度，普通 Drain、Generation `1 -> 2` fencing、独立 `policy/v1` Eviction、Canary、
      restart 与精确 cluster/image cleanup 也通过。证据为
      `docs/reports/stage-3-kubernetes-kind-pdb-multinode-aa1d0225.md`；前序 single-node 证据保留在
      `docs/reports/stage-3-kubernetes-kind-drain-fixture-fc9b2bf6.md`。Production multi-node 仍未关闭。
- [x] owned disposable Kind 已在 clean SHA `d1f3b68a` 完成 registry-pushed immutable rollout `15/15`：同一
      repository baseline/candidate 两个不同 digest 通过 containerd mirror 与 `Always` 策略真实拉取，正式 API
      完成 `promote -> 100% canary -> promote -> rollback`，并验证 active Execution fencing、Pod/Worker/Manifest/
      Revision/Channel/digest、Audit/Outbox、Event Sequence、Secret scan 与 exact cleanup。两个 overlap Pod 本次
      调度到同一 Worker，因此不关闭 production scheduler distribution 或 rollout under load。证据为
      `docs/reports/stage-3-kubernetes-kind-registry-rollout-d1f3b68a.md`。
- [ ] 第三方 Key/Base URL/自定义模型的 Kubernetes 当前六 child gate 在本次 clean SHA 通过：历史 clean
      `3c523417` 仅覆盖 Codex/Claude product/failure 四 child；新增的两个 1800s SLA/load child、每 10 waves
      Control Plane restart 已分别在 clean `cc546d3a`/`46f99518` 通过：Codex `11` 波、`44/44` Execution、
      Claude `12` 波、`48/48` Execution，quota rejection/retry、重启连续性、SLA、精确 cleanup 与零 Secret
      finding 均通过，见 `docs/reports/stage-3-real-provider-kubernetes-load-2026-07-23.md`。由于四个历史
      product/failure child 与两个新 load child 不在同一当前 SHA/共享镜像 aggregate，本项继续保持未勾选；
      不得重复执行已通过的 load 单项来冒充聚合闭环。历史四 child 证据保留在
      `docs/reports/stage-3-real-provider-kubernetes-release-gate-3c523417.md`。
- [ ] 第三方 Key/Base URL/自定义模型的 Docker 当前六 child gate 在本次 clean SHA 通过：历史 clean
      `b1c52bae` 仅覆盖 Codex/Claude product/failure 四 child；新增两个 SLA/load child 及当前 source boundary
      尚须重跑。`47214f38` 的后续聚合已通过 Codex product/failure/load 与 Claude product/failure，仅 Claude
      load 失败；该唯一失败 child 已在 clean `7ed723e2` 以 `1800s` operator-approved SLA、`17` 波、`68/68`
      Execution、`34/34` quota rejection/retry、一次 Control Plane restart 和零 Secret finding 单独通过，见
      `docs/reports/stage-3-real-provider-docker-claude-load-7ed723e2.md`。由于它不是同一 resumable aggregate
      内的共享镜像/同 SHA 六 child 证据，本项继续保持未勾选；不得重复执行该已通过单项来冒充聚合闭环。
      历史四 child 证据保留在 `docs/reports/stage-3-real-provider-docker-release-gate-b1c52bae.md`。
- [x] 已授权 external SSH target 已在 clean `14f7dd2d` 通过 repository-external identity、单一 pinned Host Key
      与 clean-SHA external-host 四 child gate；aggregate Secret scan `40` files / `3,177,374` bytes 零 finding，且未
      持久化 Host、identity/Host Key source path 或 Credential 环境变量名。证据为
      `docs/reports/stage-3-real-provider-ssh-release-gate-14f7dd2d.md`。
- [ ] 所有运行均来自本次发布 Commit 和 registry-pushed immutable image。
- [ ] 多 Turn 长 Session、多 Provider 并发、长日志、Checkpoint/Resume、Retention 与 load/soak 完成。（`6e866a30`
      仅关闭 deterministic Local 100-Turn/restart/pagination/repeated-Checkpoint mechanics；`eeb7a2f1` 仅关闭
      deterministic managed Docker 双 Worker、双 Provider、双 Session overlap mechanics；`c27914da` 仅关闭
      deterministic Local active-Execution Retention fencing 与 post-terminal physical cleanup mechanics；`e944b449`
      仅关闭 deterministic managed Docker 四 Session、100 Execution 的 bounded quota/admission、slot reuse 与
      Artifact/Checkpoint terminal mechanics；`e2d70fb6` 仅关闭同一 load 路径的资源档位、最短持续时间、
      P50/P95/P99 与意外错误率测量机制；`cfecba63` 仅关闭 deterministic single-host exact Docker
      network/container-loss/fixture Provider Host process fault、same logical Worker replacement、Peer Session 隔离、
      incarnation/Generation fencing、distinct new-Execution recovery 与 post-failure load mechanics；`41683366` 仅关闭
      deterministic single-host immutable release-rollout container loss、25 波 release-pinned load、load-safe
      Audit/Outbox retrieval 与 rollback mechanics。）
- [x] Load 报告记录 Tenant quota、Worker/slot 数、CPU/内存 requests/limits、达到的有效并发和
      admission/retry；clean `e2d70fb6` 进一步记录最短持续时间、最大波次、吞吐量、成功/意外错误率以及
      wave/recovery P50/P95/P99。证据为 `docs/reports/stage-3-docker-resource-profiled-load-e2d70fb6.md`；生产
      并发不以脱离资源档位的单一硬编码数字验收。
- [ ] 当前 clean SHA 的生产持续时间、Control Plane admission P95/P99、slot-reuse admission P95/P99 和
      意外错误率满足 `deploy/worker/production-load-sla.json`；第三方 API/自定义模型的
      `interactionReadyLatencyMs` / `turnCompletionLatencyMs` 继续记录但不冒充 Synara 可控 SLI。该行业
      基线已批准，正式 gate 未通过前此项保持未勾选。
- [ ] 故障运行没有重复终态、双 Worker 写入、Generation 回退或 Credential 泄漏。

## 7. Web 与前后端联通

实现期证据：clean commit `3a6d347d` 已在隔离 PostgreSQL/MinIO/Go Control Plane + Web 上完成真实
Project/Session/Turn 创建、PostgreSQL 恢复、连续刷新、Browser reconnect、Server restart、Console 健康与
SSE lease 精确回收；报告为
`docs/reports/stage-3-saas-web-control-plane-authority-sse-3a6d347d.md`。该证据只关闭基础 authority/reconnect
slice。Clean commit `0b4d8e4e` 又完成未配置 Control Plane 的真实 Codex 本地主聊天、刷新、完整 Server/dev
restart、本地 SQLite 恢复和第二轮 native resume；报告为
`docs/reports/stage-3-web-local-mode-restart-resume-0b4d8e4e.md`。Clean commit `88f922ed` 又完成真实 SaaS
Artifact Ready/list/download、精确 Payload hash、刷新/reconnect/完整 Server restart 恢复和 SSE lease cleanup；
报告为 `docs/reports/stage-3-saas-web-artifact-download-88f922ed.md`。`0eeabbc1` baseline 的 patched worktree
又完成 deterministic compatible Worker 的两轮 text/Tool/usage、Ready generated file、Checkpoint、Control Plane +
embedded Worker restart、incarnation `1 -> 2`、同 Session Workspace restore 与连续 Event `1..28`；报告为
`docs/reports/stage-3-saas-web-compatible-worker-restart-0eeabbc1.md`。`82adfc3f` baseline 的 patched
worktree 又完成 deterministic active mid-Turn standalone Worker loss/replacement、Generation-fenced Approval、
Workspace/Checkpoint 连续性、双页面 pending Interaction 收敛和严格模型 CAS 冲突收敛；报告为
`docs/reports/stage-3-saas-web-active-worker-replacement-multibrowser-82adfc3f.md`。该证据不替代真实 Provider、
Structured User Input、远程 Target 或完整高级操作。Clean commit `b07e5bd9` 又以 owned Kind `17/17` 关闭
deterministic Pending Structured User Input Pod-loss/Generation fencing/旧 request supersede/替换问题校验/单终态，
并以 `4/4` Browser component tests 覆盖单选/多选/replacement timer/resolving 禁用；报告为
`docs/reports/stage-3-kubernetes-structured-user-input-recovery-b07e5bd9.md`。Clean commit `807ffa8c` 再关闭真实
SaaS 双页面 Structured User Input 收敛/并发 Resolve、旧 timer 零请求、replacement 草稿隔离、resolution
acknowledged、单终态和 replacement-SSE false reconnect；报告为
`docs/reports/stage-3-saas-web-structured-user-input-multibrowser-807ffa8c.md`。这些证据仍不替代真实 Provider 或
生产 Target。

- [ ] SaaS Web 的 Project、Session、Turn、Compact、Review、Rollback、Fork 只调用 Control Plane API。
- [ ] SaaS handler 没有回退到 `readNativeApi()` 或本地 Provider discovery。
- [ ] Credential Scope、Auto-select、Project/Target Binding 和 Disable 操作可以从设置页完成。
- [ ] Worker 列表、operator revoke、Release Revision、Canary、Promote、Rollback 使用服务端权威状态。
- [ ] CAS conflict 会重新读取 `policyVersion`，不会覆盖并发运维操作。
- [ ] SSE 断开、刷新和 Server restart 后 Event Sequence、Interaction、Artifact 和 Worker 状态一致。
- [x] SaaS Web 可从权威列表展示 Ready 的用户 Artifact，使用新鲜 download grant 下载精确 Payload，并在
      页面刷新、SSE reconnect 和完整 Server restart 后恢复同一 Artifact；该项不替代 Worker/Interaction
      或多浏览器发布证据。
- [x] Deterministic compatible Worker 可在 SaaS Web 同一 Session 投影 text、Tool、usage、Ready generated
      file 与 Checkpoint；Control Plane + embedded Worker between-Turn restart 后 incarnation `1 -> 2`，第二轮
      恢复首轮 Workspace 并验证文件。该项不替代真实 Provider、active mid-Turn replacement、Approval/Input
      或多浏览器发布证据。
- [x] Deterministic active mid-Turn standalone Worker loss 保持同一 Session/Turn/Execution/Workspace，Worker
      incarnation `1 -> 2`、Execution Generation `1 -> 2`；旧 Approval `expired/superseded`，替换 Approval
      只向 Generation `2` 交付并由原 Browser 解决，Event 连续且只有一个终态，Workspace 从首轮 Ready
      Checkpoint 恢复并生成新的 Ready Checkpoint。
- [x] Deterministic execution-pinned Kubernetes Structured User Input 在 Pod 丢失后保持同一 Turn/Execution，
      Generation `1 -> 2`，旧 request `expired/superseded`，替换问题结构完全一致且只有 Generation `2`
      request 可 Resolve 为单终态；UI component 同时覆盖单选自动提交、多选延迟、replacement timer 取消和
      resolving 禁用。
- [x] 两个真实 SaaS Browser 页面同时投影同一 pending Structured User Input；任一页面 Resolve 后两页无刷新
      收敛，竞争 Resolve 只有一个权威终态，replacement request 不会被旧页面 timer 或草稿误提交。
- [x] 两个 Browser 页面可同时投影同一 pending Approval；第二页解决后两页无刷新移除 Interaction 并显示
      唯一终态。普通模型切换传播到被动页；并发模型切换精确产生一个成功和一个
      `409 session_model_conflict`，冲突页重读服务端 Session 后收敛且不覆盖胜出模型。
- [x] 未配置 Control Plane 的本地主聊天、真实 Codex、刷新、Server restart 与 native resume 没有回归，
      本地 SQLite 也没有 Control Plane 命名表或 SaaS authority 写入；本地 Project 文件操作不在本项证据边界。
- [x] 上述隔离 Web acceptance 的稳定页面无相关 Browser Error/Warning 或框架 Overlay；开发期 HMR 过渡日志
      不作为发布页面证据。

## 8. Canary、Promote 与 Rollback

Clean-SHA managed Docker mechanics gate（使用 loopback disposable Registry，不代替生产 Registry/TLS/auth、
真实 Provider 或 Kubernetes 多节点证据）：

```bash
python3 scripts/stage3-provider-acceptance/docker_worker_release_rollout_gate.py \
  --go-proxy https://goproxy.cn,direct \
  --load-waves 25 \
  --output-dir /tmp/synara-docker-worker-release-rollout \
  --timeout 3600
```

- [ ] 已按 `docs/runbooks/worker-release-rollout.md` 完成预检。
- [ ] 初始 promoted Revision 绑定已验证的 Worker Manifest 与不可变 Image Digest。
- [ ] Canary 使用更新的 Revision、`expectedPolicyVersion` 和非空原因，比例在 `1..100`。
- [ ] Canary Worker/Execution 的 Revision 与 Channel 可从 API 和运行证据追溯。
- [ ] Canary 观察窗口覆盖 Claim、Session continuation、Artifact、Interaction、Drain 和错误率。
- [ ] Promote 只针对当前 active canary Revision，并使用最新 Policy Version。
- [ ] Abort-canary 使用 rollback API 指向当前 promoted Revision；真正 rollback 只选择更旧 Revision。
- [ ] 任何 `409 worker_release_policy_version_conflict` 都先重新读取 Policy，不重放旧版本写入。
- [ ] Busy Worker 不被手工原地替换；失败时先停止 rollout，再按安全边界 Drain/Release/Recover。

## 9. 发布完成条件

- [ ] 所有 Required 项已勾选，未执行项有审批、负责人和截止时间。
- [ ] 验收报告来自最终 Commit，且报告、日志和资源清理均通过 Secret scan。
- [ ] Registry、Docker、Kubernetes、SSH 临时资源已按精确 owner/Target 标识清理。
- [ ] Release/PR 附带 DDL boundary、Runbook、Acceptance Report 和已知限制。
- [ ] 真实四 Target Provider gate、registry-pushed multi-arch 和生产 soak 未完成时，发布状态保持阻断。
