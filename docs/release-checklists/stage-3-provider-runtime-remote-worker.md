# Stage 3 Provider Runtime / Remote Worker 发布检查单

每次发布复制本检查单，并记录 Commit、不可变镜像 Digest、数据库 Migration、执行人、时间和证据链接。
未满足项必须保持未勾选，不能用 deterministic fixture、单一 Target 或静态代码检查替代真实发布证据。

当前最新的 consolidated Local 证据见
`docs/reports/stage-3-real-provider-local-release-gate-253052aa.md`。它关闭同一 clean SHA 上真实
Codex/Claude 的 Local product 与 failure slice，但仍明确保持四 Target、Registry rollout、并发和 soak
`RELEASE GATE OPEN`，不能直接作为 SSH、Docker、Kubernetes 或生产环境发布批准。
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
- [ ] 第三方 Codex/Claude API Key 只通过受控 Credential `apiKey`/`authToken` 与可选 `baseUrl` 注入；值和
      operator 环境变量名未进入聊天、命令参数、Target 配置、日志或报告。
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
- [x] Checked-in signing policy 可显式选择 `ephemeral-key`、`keyless` 或 `kms-key`；生产模式拒绝非 TLS
      Registry、强制 tlog，并隔离/清理 OIDC token 或仅按允许的环境变量名传递 KMS Credential。
- [x] `linux/amd64`、`linux/arm64` 均为 `HIGH=0`、`CRITICAL=0`、Secret=0、非 EOSL，Trivy DB 满足 24 小时
      freshness；`GO-2026-5932` 保留为未豁免的不可达 `UNKNOWN` review finding。
- [ ] Worker Manifest 中的 Git SHA、OS/Arch、Image Digest、Protocol、Provider Runtime 和 Capability Hash 可追溯。
- [ ] 生产发布已归档 SBOM/扫描报告，并使用获批的 KMS/keyless 身份、transparency log 与 admission policy
      完成镜像签名/来源验证；不得以 disposable ephemeral-key 证据替代。
- [ ] 当前生产选择 `kms-key`；具体 KMS reference（自建 Vault 可使用 `hashivault://...`）、最小 credential
      环境变量名、signer identity、tlog 和 admission policy 已审批并留存非 Secret 证据。
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

| 证据                                                   | 当前结论                                                                          | 发布边界                                                                                                                                                                                   |
| ------------------------------------------------------ | --------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| 真实 Codex/Claude Local two-Turn product-path smoke    | clean commit `fb9e25ec` 各 12/12                                                  | 经过 Control Plane/LocalSupervisor/agentd，但不是完整 Local Gate                                                                                                                           |
| 真实 Codex/Claude Generated File + Checkpoint          | clean commit `be919393` matrix pass                                               | standalone Ready Artifact 与 Snapshot 已验；Diff 由下一行独立跟踪                                                                                                                          |
| 真实 Codex/Claude Local Large Diff                     | clean commit `90fae52c` matrix pass                                               | Ready `diff`/下载/顺序/restart/cleanup/Secret scan 已验                                                                                                                                    |
| 真实 Codex/Claude Local failure matrix                 | clean commit `61e38f4f` 各 `16/16`                                                | 401/429、scoped Host crash、Cursor expiry/restart 与新 Execution 已验                                                                                                                      |
| 真实 Codex/Claude consolidated Local release gate      | clean commit `253052aa` aggregate pass                                            | 四份 product/failure 报告同 SHA/hash，无 fail/skipped，cleanup/Secret scan 已验                                                                                                            |
| deterministic Local long-Session fixture soak          | clean commit `6e866a30` 100/100 Turns                                             | 9 次额外 restart、Event `1..1371`、分页与 repeated Checkpoint 已验；不是真实 Provider/production soak                                                                                      |
| deterministic Docker Provider concurrency fixture      | clean commit `eeb7a2f1` 9/9                                                       | 两 Worker、Codex/Claude 两 Session/Execution、同时 pending Approval 与隔离终态已验；不是真实 Provider/Retention/production concurrency                                                     |
| deterministic Local Retention/Cleanup fixture          | clean commit `c27914da` 9/9                                                       | active Execution fencing、无引用 Artifact 删除、Checkpoint 保护与终态后单次 physical cleanup 已验；不是真实 Provider/remote/production Retention                                           |
| deterministic Docker bounded load/admission fixture    | clean commit `e944b449` 100/100 Executions                                        | 四 Session、50 次 quota rejection/retry、75 次双 Worker overlap、Artifact/Checkpoint 唯一终态已验；不是真实 Provider/production load                                                       |
| deterministic Docker network/container/Host crash load | clean commit `cfecba63` 12/12                                                     | exact network/container/Host-process fault、same-Worker replacement、Generation 或 new-Execution recovery 与 100 Execution load 已验；不是真实 Provider/multi-node fault                   |
| deterministic Docker release rollout failure/load      | clean commit `41683366` 15/15                                                     | canary container-loss Generation `1->2`、peer 隔离、25 波/100 Execution release pins、分页 Audit 与 topic-filtered Outbox 已验；不是真实 Provider/production rollout                       |
| deterministic Kubernetes Registry release rollout      | clean commit `d1f3b68a` 15/15                                                     | three-node Kind、两个真实 Registry digest、promote/canary/promote/rollback、Pod/Worker/Manifest release pins 与 exact cleanup 已验；overlap Pod 同 Node，不是 production distribution/load |
| 真实 Codex `0.144.x` `terminal-large`                  | Explicit Unsupported                                                              | Unified Exec 仅保留 1 MiB Head/Tail；不得牺牲 durable Approval                                                                                                                             |
| Claude ambient OAuth `terminal-large`                  | Explicit Unsupported                                                              | 需 controlled Credential 绑定 Runtime Output Root                                                                                                                                          |
| deterministic Local/Docker core suite                  | 已通过                                                                            | 证明共享 Control Plane/agentd/Host orchestration，不证明真实 Adapter                                                                                                                       |
| deterministic Provider fault matrix                    | malformed/oversized/crash 已通过                                                  | 不是真实 Provider failure 分类                                                                                                                                                             |
| deterministic Docker/Kubernetes failure matrix         | 已通过实现期运行                                                                  | 不等于生产网络、真实 CNI 或正式 rollout                                                                                                                                                    |
| SSH real Provider runtime provisioning                 | disposable VM preflight 已通过                                                    | Host SHA + Codex 0.144.1 + Claude 2.1.197 已验；尚无真实 Credential 报告                                                                                                                   |
| SSH real Provider fault-injection transport            | Runner 99/99 + SSH fixture 16/16                                                  | token-scoped reverse relay 与 systemd MainPID crash 已实现；尚无真实 Credential 报告                                                                                                       |
| Docker real Provider fault-injection transport         | 实现期容器探针与 Docker 16/16 已通过                                              | 401/429/精确 Host crash 已实现；尚无真实 Provider Credential 报告                                                                                                                          |
| Kubernetes real Provider fault-injection transport     | Runner 99/99 + Linux 容器探针通过                                                 | host-gateway 401/429 与精确 Pod crash 已实现；尚无真实 Provider Credential 报告                                                                                                            |
| SSH consolidated release gate                          | 独立引擎与 10 项 SSH gate tests 已通过                                            | 四个 disposable VM child 尚待真实 Credential 执行                                                                                                                                          |
| Docker consolidated release gate                       | Local+Docker 32 项 gate tests 已通过                                              | 单次 Gate-owned Image + 四份同 SHA/Catalog/Image 报告尚待真实 Credential 执行                                                                                                              |
| Kubernetes consolidated release gate                   | clean `6b71703f` 四 child 已真实执行；1 pass/3 fail                               | Codex failure `16/16` 通过；Codex product 缺 approval interaction，Claude profile HTTP `502`；必须更换/修复第三方 profile 后重跑，不得降级放行                                             |
| Worker Registry signing-policy gate                    | clean commit `7659dd5f` gate/report 已通过                                        | keyless/KMS 实现路径与 ephemeral mechanics 已验；真实生产 identity/tlog/admission、Registry Credential/retention 与 rollout 尚待记录                                                       |
| SSH fixture                                            | 2026-07-14 disposable VM 13/13                                                    | 不是当前 Commit 的真实 Provider gate                                                                                                                                                       |
| Kubernetes fixture                                     | `aa1d0225` three-node owned Kind 24/24；`6b71703f` OrbStack 22 pass/1 unsupported | PDB-blocked Drain、跨 Worker replacement、普通 Drain、Eviction、Network、Canary、restart 与 exact cleanup 已验；不证明真实 Provider 或 production multi-node pass gate                     |

真实 Provider × Target gate：

- [ ] Codex × Local：实现证据 `253052aa` 已覆盖 Discovery、Start、Send、第二 Turn、Restart、
      Interaction、Artifact 和错误分类；本次发布 Commit 仍须重跑。
- [ ] Claude × Local：实现证据 `253052aa` 已覆盖同一 Local release slice；本次发布 Commit 仍须重跑。
- [ ] Codex × SSH：install/upgrade/revoke、Host Key、systemd restart、Workspace continuity。
- [ ] Claude × SSH：同上。
- [ ] Codex × Docker：replace、volume/checkpoint、network interruption、resource limits。
- [ ] Claude × Docker：同上。
- [ ] Codex × Kubernetes：Pod replacement、Drain、Eviction、Network、Image rollout。
- [ ] Claude × Kubernetes：同上。
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
- [ ] 第三方 Credential 的 Kubernetes 四 child gate 已执行但未通过：Codex failure `16/16` 通过，Codex
      product 缺 approval interaction，Claude product/failure 为 HTTP `502` `provider_unavailable`。需使用满足
      tool/approval 与 Anthropic streaming 的 profile 重跑；证据为
      `docs/reports/stage-3-real-provider-kubernetes-third-party-gate-6b71703f.md`。
- [ ] 已授权外部 SSH target 尚需 repository-external identity、pinned Host Key 和 clean-SHA external-host
      运行。SSH 认证信息不得写入仓库或 evidence；当前第三方 Provider profile 也不能作为该 gate 的通过输入。
- [ ] 所有运行均来自本次发布 Commit 和 registry-pushed immutable image。
- [ ] 多 Turn 长 Session、多 Provider 并发、长日志、Checkpoint/Resume、Retention 与 load/soak 完成。（`6e866a30`
      仅关闭 deterministic Local 100-Turn/restart/pagination/repeated-Checkpoint mechanics；`eeb7a2f1` 仅关闭
      deterministic managed Docker 双 Worker、双 Provider、双 Session overlap mechanics；`c27914da` 仅关闭
      deterministic Local active-Execution Retention fencing 与 post-terminal physical cleanup mechanics；`e944b449`
      仅关闭 deterministic managed Docker 四 Session、100 Execution 的 bounded quota/admission、slot reuse 与
      Artifact/Checkpoint terminal mechanics；`cfecba63` 仅关闭 deterministic single-host exact Docker
      network/container-loss/fixture Provider Host process fault、same logical Worker replacement、Peer Session 隔离、
      incarnation/Generation fencing、distinct new-Execution recovery 与 post-failure load mechanics；`41683366` 仅关闭
      deterministic single-host immutable release-rollout container loss、25 波 release-pinned load、load-safe
      Audit/Outbox retrieval 与 rollback mechanics。）
- [ ] Load 报告记录 Tenant quota、Worker/slot 数、CPU/内存 requests/limits、达到的有效并发和
      admission/retry；生产并发不以脱离资源档位的单一硬编码数字验收。
- [ ] 生产持续时间、P95/P99、错误率和恢复时间满足审批 SLA；数值未批准前此项保持未勾选。
- [ ] 故障运行没有重复终态、双 Worker 写入、Generation 回退或 Credential 泄漏。

## 7. Web 与前后端联通

- [ ] SaaS Web 的 Project、Session、Turn、Compact、Review、Rollback、Fork 只调用 Control Plane API。
- [ ] SaaS handler 没有回退到 `readNativeApi()` 或本地 Provider discovery。
- [ ] Credential Scope、Auto-select、Project/Target Binding 和 Disable 操作可以从设置页完成。
- [ ] Worker 列表、operator revoke、Release Revision、Canary、Promote、Rollback 使用服务端权威状态。
- [ ] CAS conflict 会重新读取 `policyVersion`，不会覆盖并发运维操作。
- [ ] SSE 断开、刷新和 Server restart 后 Event Sequence、Interaction、Artifact 和 Worker 状态一致。
- [ ] 未配置 Control Plane 的本地模式没有回归，也不会写入 SaaS authority。
- [ ] 浏览器 Console 无相关 Error/Warning 或框架 Overlay。

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
